package docs

import (
	"fmt"
	"strings"

	"github.com/nicklanng/dynago/internal/analysis"
	"github.com/nicklanng/dynago/internal/cost"
	"github.com/nicklanng/dynago/internal/schema"
)

// storage renders the physical design: the table, its indexes and how items group into
// partitions.
func (d *doc) storage() {
	m := d.m
	d.p("## Storage and partitions")
	d.p("")
	d.p("### Table")
	d.p("")
	d.p("| Setting | Value |")
	d.p("|---|---|")
	d.p("| Table | `%s` (generation %d) |", m.Table.GenerationTable(m.Table.Generation), m.Table.Generation)
	d.p("| Base key | `PK` (partition, string) + `SK` (sort, string) |")
	d.p("| Billing | On-demand |")
	if ttl := ttlAttr(m); ttl != "" {
		d.p("| TTL attribute | `%s` (epoch seconds) |", ttl)
	}
	d.p("")
	d.indexes()
	d.partitionMap()
	d.partitionTable()
}

func (d *doc) indexes() {
	var all []*schema.Index
	for _, e := range d.m.Entities {
		all = append(all, e.Indexes...)
	}
	if len(all) == 0 {
		d.p("The table has no indexes.")
		d.p("")
		return
	}
	d.p("### Indexes")
	d.p("")
	d.p("A GSI is maintained by DynamoDB a moment after each write: writes stay cheap, and reads are eventually consistent. A copy is written by the generated code in the write's transaction: writes that change it cost a transaction, and reads see them immediately.")
	d.p("")
	d.p("| Index | Kind | Keys | Projection | Read by | Why this kind | The other kind |")
	d.p("|---|---|---|---|---|---|---|")
	for _, ix := range all {
		e := ix.Entity
		kind := "copy"
		if ix.Strategy == schema.StrategyGSI {
			kind = "GSI `" + ix.GSI.Name + "`"
		}
		keys := "`" + ix.PK.Raw + "`"
		if ix.HasSK {
			keys += " / `" + ix.SK.Raw + "`"
		}
		if len(ix.Where) > 0 {
			keys += "; only when " + predText(ix.Where)
		}
		var readers []string
		for _, a := range e.Access {
			if a.Index == ix {
				readers = append(readers, a.Name)
			}
		}
		read := strings.Join(readers, ", ")
		if read == "" {
			read = "nothing"
		}
		why := "declared"
		if ix.StrategyInferred {
			why = "chosen: " + ix.StrategyReason
		}
		d.p("| `%s.%s` | %s | %s | %s | %s | %s | %s |", e.Name, ix.Name, kind, escape(keys), projText(ix), read, escape(why), escape(d.otherKind(ix)))
	}
	d.p("")
}

// otherKind says what the index would cost as the other strategy.
func (d *doc) otherKind(ix *schema.Index) string {
	alt := d.a.Alternatives[ix]
	if alt == nil {
		return ""
	}
	if !alt.Feasible {
		return "Can't be a copy: " + alt.Why + "."
	}
	var parts []string
	for _, w := range alt.Writes {
		parts = append(parts, w.Describe())
	}
	s := "As a " + map[schema.Strategy]string{schema.StrategyCopy: "copy", schema.StrategyGSI: "GSI"}[alt.Strategy] + ": "
	if len(parts) == 0 {
		s += "no write's cost would change"
	} else {
		s += strings.Join(parts, "; ")
	}
	switch {
	case alt.Strategy == schema.StrategyCopy:
		s += "; reads could see writes immediately"
	case len(alt.Breaks) > 0:
		s += "; " + strings.Join(alt.Breaks, " and ") + " would lose immediate freshness"
	default:
		s += "; reads would be eventually consistent"
	}
	return s + "."
}

// partitionMap draws each partition family with the items it holds and the reads that reach it.
func (d *doc) partitionMap() {
	ps := d.a.Partitions
	if len(ps) == 0 {
		return
	}
	d.p("### Partition map")
	d.p("")
	d.p("Which items share a partition, in the base table and in each GSI, and which reads reach them.")
	d.p("")
	d.p("```mermaid")
	d.p("flowchart LR")
	id := map[*analysis.Partition]string{}
	for i, p := range ps {
		id[p] = fmt.Sprintf("p%d", i)
		title := p.PK
		if p.Index != "" {
			title = "GSI " + p.Index + ": " + p.PK
		}
		d.p("  subgraph p%d[\"%s\"]", i, mm(title))
		for j, mb := range p.Members {
			d.p("    p%dm%d[\"%s%s\"]", i, j, mm(mb.Label), mm(countSuffix(mb.Count)))
		}
		d.p("  end")
	}
	for _, er := range d.r.Entities {
		for _, rc := range er.Reads {
			a := rc.Access
			if a.Kind == schema.AccessScan {
				continue // reads every partition: drawn below, not as an edge to one
			}
			var targets []string
			for _, t := range rc.Reads {
				if mb := d.a.MemberOf(t); mb != nil && !containsStr(targets, id[mb.Partition]) {
					targets = append(targets, id[mb.Partition])
				}
			}
			if len(targets) == 0 {
				continue
			}
			rid := "r_" + a.Entity.Name + "_" + a.Name
			d.p("  %s{{\"%s.%s\"}} --> %s", rid, a.Entity.Name, a.Name, strings.Join(targets, " & "))
		}
	}
	d.p("```")
	d.p("")
	for _, e := range d.m.Entities {
		for _, a := range e.Access {
			if a.Kind == schema.AccessScan {
				d.p("`%s.%s` scans every partition of the base table.", e.Name, a.Name)
				d.p("")
			}
		}
	}
}

func countSuffix(c analysis.Estimate) string {
	switch {
	case !c.Known:
		return ""
	case c.MaxKnown && c.Max <= 1 && c.Typical >= 1:
		return ""
	case c.MaxKnown && c.Max <= 1:
		return " ×0–1"
	case c.MaxKnown && c.Max != c.Typical:
		return fmt.Sprintf(" ×%s (≤%s)", schema.Number(roundCount(c.Typical)), schema.Number(roundCount(c.Max)))
	}
	return " ×" + schema.Number(roundCount(c.Typical))
}

func containsStr(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

// partitionTable lists each partition family's contents, size, growth and busiest key.
func (d *doc) partitionTable() {
	ps := d.a.Partitions
	if len(ps) == 0 {
		return
	}
	d.p("### Partitions")
	d.p("")
	d.p("Each row is every partition key value one key pattern renders: *Keys* is how many values there are. Counts are per value: typical, then the largest. Traffic is the busiest value's at peak.")
	d.p("")
	d.p("| Partition key | In | Keys | Holds | Size, typical / largest | Grows | Busiest key at peak | Risk |")
	d.p("|---|---|---|---|---|---|---|---|")
	for _, p := range ps {
		var holds []string
		for _, mb := range p.Members {
			holds = append(holds, mb.Label+": "+countText(mb.Count))
		}
		grows := "no"
		if p.Grows {
			var who []string
			for _, mb := range p.Members {
				if mb.Grows {
					who = append(who, mb.Target.Entity.Name)
				}
			}
			grows = "yes: " + strings.Join(dedupe(who), ", ") + " never removed"
		}
		busy := "no rates declared"
		risk := "—"
		if p.Rated {
			busy = fmt.Sprintf("%s WRU/s, %s RRU/s", load(p.PeakWRU), load(p.PeakRRU))
			risk = fmt.Sprintf("%s (%s%%)", p.Risk, load(p.Headroom*100))
		}
		size := bytesText(p.Size.Typical, p.Size.Known) + " / " + bytesText(p.Size.Max, p.Size.MaxKnown)
		keys := "unknown"
		if p.Count.Known {
			keys = schema.Number(roundCount(p.Count.Typical))
		}
		d.p("| `%s` | %s | %s | %s | %s | %s | %s | %s |", escape(p.PK), p.Space(), keys, escape(strings.Join(holds, "<br>")), size, grows, busy, risk)
	}
	d.p("")
	var undeclared []string
	for _, p := range ps {
		for _, f := range p.Undeclared {
			undeclared = append(undeclared, fmt.Sprintf("`%s` in `%s`", f, escape(p.PK)))
		}
	}
	if len(undeclared) > 0 {
		d.p("Unknown counts: nothing declares how many items share a value of %s. For a field that identifies another entity, name it (`ref:`, or a matching field name) and give the spread (`volume.by`).", strings.Join(undeclared, ", "))
		d.p("")
	}
}

func countText(c analysis.Estimate) string {
	switch {
	case !c.Known:
		return "unknown"
	case c.MaxKnown && c.Max <= 1 && c.Typical >= 1:
		return "1"
	case c.MaxKnown && c.Max <= 1:
		return "0 or 1"
	case c.MaxKnown && c.Max != c.Typical:
		return schema.Number(roundCount(c.Typical)) + " / " + schema.Number(roundCount(c.Max))
	case c.MaxKnown:
		return schema.Number(roundCount(c.Typical))
	}
	return schema.Number(roundCount(c.Typical)) + " / unknown"
}

func bytesText(b float64, known bool) string {
	if !known {
		return "unknown"
	}
	return cost.HumanBytes(b)
}

// load formats a small rate: 0.004, 1.3, 540.
func load(f float64) string {
	switch {
	case f == 0:
		return "0"
	case f < 0.01:
		return "<0.01"
	case f < 10:
		return strings.TrimSuffix(strings.TrimRight(fmt.Sprintf("%.2f", f), "0"), ".")
	}
	return fmt.Sprintf("%.0f", f)
}

func dedupe(ss []string) []string {
	var out []string
	for _, s := range ss {
		if !containsStr(out, s) {
			out = append(out, s)
		}
	}
	return out
}
