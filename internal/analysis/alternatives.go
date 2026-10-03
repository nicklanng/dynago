package analysis

import (
	"fmt"
	"strings"

	"github.com/nicklanng/dynago/internal/cost"
	"github.com/nicklanng/dynago/internal/keytmpl"
	"github.com/nicklanng/dynago/internal/schema"
)

// Alternative compares an index with the other strategy: a GSI as a copy, or a copy as a GSI.
type Alternative struct {
	Index    *schema.Index
	Strategy schema.Strategy
	// Feasible is false when the index's keys can't work the other way; Why says why.
	Feasible bool
	Why      string
	// Writes lists the writes whose cost or shape would change.
	Writes []AltWrite
	// Reads lists the reads through the index, now and then.
	Reads []AltRead
	// Breaks lists reads needing immediate freshness, which a GSI can't give.
	Breaks []string
}

// AltWrite is one write's cost now and with the alternative.
type AltWrite struct {
	Write     *schema.Write
	Now, Then cost.WriteCost
}

// AltRead is one read's cost now and with the alternative.
type AltRead struct {
	Access    *schema.Access
	Now, Then cost.ReadCost
}

// WriteSummary describes how writes would change: "Borrow 23 → 19 WRU, no longer a transaction".
func (alt *Alternative) WriteSummary() string {
	var parts []string
	for _, w := range alt.Writes {
		parts = append(parts, w.Describe())
	}
	if len(parts) == 0 {
		return "No write's cost would change."
	}
	return "As a " + strategyName(alt.Strategy) + ": " + strings.Join(parts, "; ") + "."
}

// Describe says how one write would change.
func (w AltWrite) Describe() string {
	s := fmt.Sprintf("%s %s → %s WRU", writeName(w.Write), trim(w.Now.WRU.P50), trim(w.Then.WRU.P50))
	switch {
	case w.Now.Transactional && !w.Then.Transactional:
		s += ", no longer a transaction"
	case !w.Now.Transactional && w.Then.Transactional:
		s += ", now a transaction"
	case w.Now.MaxTxItems != w.Then.MaxTxItems:
		s += fmt.Sprintf(" (%d → %d items)", w.Now.MaxTxItems, w.Then.MaxTxItems)
	}
	switch {
	case w.Now.ReadFirst && !w.Then.ReadFirst:
		s += ", without reading first"
	case !w.Now.ReadFirst && w.Then.ReadFirst:
		s += ", reading first"
	}
	return s
}

func writeName(w *schema.Write) string { return w.Entity.Name + "." + w.Name }

func strategyName(s schema.Strategy) string {
	if s == schema.StrategyCopy {
		return "copy"
	}
	return "GSI"
}

func trim(f float64) string {
	return strings.TrimSuffix(strings.TrimRight(fmt.Sprintf("%.1f", f), "0"), ".")
}

// alternatives costs every index with the other strategy, by switching it in the model, costing
// the writes it would change, and switching it back.
func (a *analyzer) alternatives() {
	for _, e := range a.m.Entities {
		for _, ix := range e.Indexes {
			a.r.Alternatives[ix] = a.alternative(e, ix)
		}
	}
}

func (a *analyzer) alternative(e *schema.Entity, ix *schema.Index) *Alternative {
	alt := &Alternative{Index: ix, Strategy: schema.StrategyCopy, Feasible: true}
	if ix.Strategy == schema.StrategyCopy {
		alt.Strategy = schema.StrategyGSI
		for _, ac := range e.Access {
			if ac.Index == ix && ac.Freshness == schema.FreshnessImmediate {
				alt.Breaks = append(alt.Breaks, ac.Name)
			}
		}
	} else if why := copyInfeasible(a.m, e, ix); why != "" {
		alt.Feasible, alt.Why = false, why
		return alt
	}
	saved := *ix
	defer func() { *ix = saved }()
	ix.Strategy = alt.Strategy
	if alt.Strategy == schema.StrategyCopy {
		ix.PKAttr, ix.SKAttr = schema.AttrPK, schema.AttrSK
	} else {
		ix.PKAttr, ix.SKAttr = ix.Name+"PK", ix.Name+"SK"
	}
	for _, oe := range a.m.Entities {
		oer := a.r.Cost.Entity(oe)
		for i, w := range oe.Writes {
			affected := oe == e
			for _, rq := range w.Requires {
				affected = affected || (rq.Target == e && rq.Writes())
			}
			if !affected {
				continue
			}
			readFirst := w.ReadFirst && !w.Transition
			if oe == e {
				rf, tr := schema.PlanReads(e, w)
				readFirst = rf && !tr
			}
			now := oer.Writes[i]
			then := cost.WriteCostOf(a.m, oe, w, readFirst, cost.EntitySizes(a.m, oe))
			if now.WRU.P50 != then.WRU.P50 || now.MaxTxItems != then.MaxTxItems || now.ReadFirst != then.ReadFirst {
				alt.Writes = append(alt.Writes, AltWrite{Write: w, Now: now, Then: then})
			}
		}
	}
	er := a.r.Cost.Entity(e)
	sizes := cost.EntitySizes(a.m, e)
	for i, ac := range e.Access {
		if ac.Index != ix {
			continue
		}
		variant := *ac
		variant.Consistent = alt.Strategy == schema.StrategyCopy && ac.Freshness != schema.FreshnessEventual
		alt.Reads = append(alt.Reads, AltRead{Access: ac, Now: er.Reads[i], Then: cost.ReadCostOf(a.m, &variant, sizes)})
	}
	return alt
}

// copyInfeasible says why an index's keys can't be a copy's, or "" if they can.
func copyInfeasible(m *schema.Model, e *schema.Entity, ix *schema.Index) string {
	if !ix.HasSK {
		return "a copy needs a sort key, and " + ix.Name + " has none"
	}
	if ix.SK.LiteralPrefix() == "" {
		return "a copy's sort key must start with literal text, and " + ix.Name + "'s starts with a field"
	}
	used := map[string]bool{}
	for _, t := range []schema.Template{ix.PK, ix.SK} {
		for _, sg := range t.Segments {
			if sg.IsField() && sg.Transform == "" {
				used[sg.Field] = true
			}
		}
	}
	for _, f := range e.KeyFields() {
		if !used[f.Name] {
			return fmt.Sprintf("a copy's keys must include every key field of %s, and %s's lack %s", e.Name, ix.Name, f.Name)
		}
	}
	// In the base table, its items must be told apart from every other family's.
	for _, o := range m.Entities {
		families := []struct {
			what   string
			pk, sk schema.Template
		}{{o.Name + " items", o.PK, o.SK}}
		for _, oix := range o.Indexes {
			if oix.Strategy == schema.StrategyCopy && oix != ix {
				families = append(families, struct {
					what   string
					pk, sk schema.Template
				}{o.Name + " " + oix.Name + " copies", oix.PK, oix.SK})
			}
		}
		for _, u := range o.Uniques {
			families = append(families, struct {
				what   string
				pk, sk schema.Template
			}{o.Name + " " + u.Name + " claims", u.PK, u.SK})
		}
		for _, c := range o.Counters {
			families = append(families, struct {
				what   string
				pk, sk schema.Template
			}{"counter " + c.Name, c.PK, c.SK})
		}
		for _, fam := range families {
			if keytmpl.MayOverlap(ix.PK.Template, fam.pk.Template) && keytmpl.PrefixCollides(ix.SK.Template, fam.sk.Template) {
				return fmt.Sprintf("as copies in the base table, %s's items could be confused with %s (sort keys %q and %q share a prefix)", ix.Name, fam.what, ix.SK.Raw, fam.sk.Raw)
			}
		}
	}
	return ""
}
