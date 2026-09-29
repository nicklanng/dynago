package arch

import (
	"bytes"
	"fmt"
	"math"
	"slices"
	"strings"

	"github.com/nicklanng/dynago/internal/cost"
	"github.com/nicklanng/dynago/internal/lock"
	"github.com/nicklanng/dynago/internal/schema"
)

// Diff renders the architectural differences from old to next as Markdown, for a pull request. A
// nil old means the schema is new. It returns "" when nothing architectural changed.
func Diff(old, next *Snapshot, from, to string) string {
	d := &differ{}
	if old == nil {
		old = &Snapshot{Table: next.Table}
		d.fresh = true
	}
	d.migration(old, next)
	d.entities(old, next)
	d.guarantees(old, next)
	d.partitions(old, next)
	d.findings(old, next)
	d.costs(old, next)
	if len(d.sections) == 0 {
		return ""
	}
	slices.SortStableFunc(d.sections, func(a, b section) int {
		return slices.Index(sectionOrder, a.title) - slices.Index(sectionOrder, b.title)
	})
	var b bytes.Buffer
	fmt.Fprintf(&b, "### `%s`: architecture changes (%s → %s)\n\n", next.Table, from, to)
	if d.lead != "" {
		fmt.Fprintf(&b, "%s\n\n", d.lead)
	}
	for _, s := range d.sections {
		fmt.Fprintf(&b, "#### %s\n\n", s.title)
		for _, l := range s.lines {
			fmt.Fprintf(&b, "- %s\n", l)
		}
		b.WriteString("\n")
	}
	return b.String()
}

// sectionOrder is the order sections appear in: the design top down.
var sectionOrder = []string{"Entities", "Fields", "Indexes", "Uniqueness and counters", "Reads", "Writes", "Guarantees", "Lifecycles", "Partitions", "Findings", "Costs"}

type section struct {
	title string
	lines []string
}

type differ struct {
	fresh    bool
	lead     string
	sections []section
}

func (d *differ) add(title, format string, args ...any) {
	line := fmt.Sprintf(format, args...)
	for i := range d.sections {
		if d.sections[i].title == title {
			d.sections[i].lines = append(d.sections[i].lines, line)
			return
		}
	}
	d.sections = append(d.sections, section{title, []string{line}})
}

const (
	added   = "**+**"
	removed = "**−**"
	changed = "**~**"
)

// migration says whether existing items fit the new design: a new generation copies the table.
func (d *differ) migration(old, next *Snapshot) {
	if d.fresh {
		d.lead = fmt.Sprintf("A new table, `%s-g%d`.", next.Table, next.Generation)
		return
	}
	var misfits, fits []string
	for _, ne := range next.Entities {
		oe := entityByName(old, ne.Name)
		if oe == nil {
			continue
		}
		for _, c := range lock.Changes(oe.Shape, ne.Shape, nil) {
			line := ne.Name + ": " + c.Text
			if c.Compatible {
				fits = append(fits, line)
			} else {
				misfits = append(misfits, line)
			}
		}
	}
	switch {
	case next.Generation != old.Generation:
		// A pass reads the whole old table, the whole new one, and each entity again by key; the
		// first copies every item.
		var items, wru, rereads float64
		for _, e := range old.Entities {
			items += e.Count
			rereads += e.Count * math.Max(1, math.Ceil(float64(e.ItemBytes)/4096))
		}
		for _, e := range next.Entities {
			wru += e.Count * e.MigrateWRU
		}
		rru := math.Ceil(old.BaseBytes/4096) + math.Ceil(next.BaseBytes/4096) + rereads
		lead := fmt.Sprintf("**Needs a new table generation (%d → %d):** the migration job copies `%s-g%d` into `%s-g%d`, which the old version serves from until the copy catches up; `%s-g%d` stays for rollback.",
			old.Generation, next.Generation, next.Table, old.Generation, next.Table, next.Generation, next.Table, old.Generation)
		if items > 0 {
			lead += fmt.Sprintf(" At the declared volumes that is about %s items (%s in the base table). Each pass reads both tables and each item again by key, about %s RRU; the first also writes every item with what it maintains, about %s WRU.",
				schema.Number(math.Round(items)), cost.HumanBytes(old.BaseBytes), schema.Number(rru), schema.Number(math.Round(wru)))
		}
		if len(misfits) > 0 {
			lead += "\n\nChanges existing items don't fit:\n\n- " + strings.Join(misfits, "\n- ")
		}
		d.lead = lead
	case len(misfits) > 0:
		d.lead = "**Existing items don't fit this change, but the table generation is unchanged:** `dynago generate` will refuse it until `table.generation` goes up.\n\n- " + strings.Join(misfits, "\n- ")
	case len(fits) > 0:
		d.lead = "Existing items fit these storage changes, so they happen in place:\n\n- " + strings.Join(fits, "\n- ")
	}
}

func entityByName(s *Snapshot, name string) *Entity {
	for i := range s.Entities {
		if s.Entities[i].Name == name {
			return &s.Entities[i]
		}
	}
	return nil
}

func (d *differ) entities(old, next *Snapshot) {
	for _, oe := range old.Entities {
		if entityByName(next, oe.Name) == nil {
			d.add("Entities", "%s `%s`, with its %d reads and %d writes", removed, oe.Name, len(oe.Reads), len(oe.Writes))
		}
	}
	for _, ne := range next.Entities {
		oe := entityByName(old, ne.Name)
		if oe == nil {
			if !d.fresh {
				d.add("Entities", "%s `%s` (volume: %s), keyed `%s` / `%s`", added, ne.Name, ne.Volume, ne.PK, ne.SK)
			}
			oe = &Entity{Name: ne.Name}
		} else {
			if oe.Volume != ne.Volume {
				d.add("Entities", "%s `%s` volume: %s → %s", changed, ne.Name, oe.Volume, ne.Volume)
			}
			if oe.PK != ne.PK || oe.SK != ne.SK {
				d.add("Entities", "%s `%s` key: `%s` / `%s` → `%s` / `%s`", changed, ne.Name, oe.PK, oe.SK, ne.PK, ne.SK)
			}
		}
		d.fields(oe, &ne)
		d.indexes(oe, &ne)
		d.uniques(oe, &ne)
		d.counters(oe, &ne)
		d.reads(oe, &ne)
		d.writes(oe, &ne)
	}
}

func (d *differ) fields(oe, ne *Entity) {
	if d.fresh {
		return
	}
	for _, of := range oe.Fields {
		if !slices.ContainsFunc(ne.Fields, func(f Field) bool { return f.Name == of.Name }) {
			d.add("Fields", "%s `%s.%s`", removed, ne.Name, of.Name)
		}
	}
	for _, nf := range ne.Fields {
		i := slices.IndexFunc(oe.Fields, func(f Field) bool { return f.Name == nf.Name })
		if i < 0 {
			if len(oe.Fields) > 0 {
				d.add("Fields", "%s `%s.%s` (%s)", added, ne.Name, nf.Name, fieldText(nf))
			}
			continue
		}
		if of := oe.Fields[i]; of != nf {
			d.add("Fields", "%s `%s.%s`: %s → %s", changed, ne.Name, nf.Name, fieldText(of), fieldText(nf))
		}
	}
}

func fieldText(f Field) string {
	parts := []string{f.Type}
	if f.Required {
		parts = append(parts, "required")
	} else {
		parts = append(parts, "optional")
	}
	if f.Copy != "" {
		parts = append(parts, f.Copy)
	}
	if f.Ref != "" {
		parts = append(parts, "ref "+f.Ref)
	}
	return strings.Join(parts, ", ")
}

func (d *differ) indexes(oe, ne *Entity) {
	for _, oi := range oe.Indexes {
		if !slices.ContainsFunc(ne.Indexes, func(x Index) bool { return x.Name == oi.Name }) {
			d.add("Indexes", "%s `%s.%s` (%s)", removed, ne.Name, oi.Name, strategyText(oi.Strategy))
		}
	}
	for _, ni := range ne.Indexes {
		i := slices.IndexFunc(oe.Indexes, func(x Index) bool { return x.Name == ni.Name })
		if i < 0 {
			d.add("Indexes", "%s `%s.%s`: %s", added, ne.Name, ni.Name, indexText(ni))
			continue
		}
		oi := oe.Indexes[i]
		var ch []string
		if oi.Strategy != ni.Strategy {
			why := ""
			if ni.Why != "" {
				why = " (" + ni.Why + ")"
			}
			ch = append(ch, fmt.Sprintf("%s → %s%s", strategyText(oi.Strategy), strategyText(ni.Strategy), why))
		}
		if oi.PK != ni.PK || oi.SK != ni.SK {
			ch = append(ch, fmt.Sprintf("keys `%s` / `%s` → `%s` / `%s`", oi.PK, oi.SK, ni.PK, ni.SK))
		}
		if oi.Projection != ni.Projection {
			ch = append(ch, fmt.Sprintf("projection %s → %s", oi.Projection, ni.Projection))
		}
		if oi.Where != ni.Where {
			ch = append(ch, fmt.Sprintf("where %q → %q", oi.Where, ni.Where))
		}
		if strings.Join(oi.ReadBy, ",") != strings.Join(ni.ReadBy, ",") {
			ch = append(ch, fmt.Sprintf("read by %s → %s", listOrNothing(oi.ReadBy), listOrNothing(ni.ReadBy)))
		}
		if len(ch) > 0 {
			d.add("Indexes", "%s `%s.%s`: %s", changed, ne.Name, ni.Name, strings.Join(ch, "; "))
		}
	}
}

func listOrNothing(ss []string) string {
	if len(ss) == 0 {
		return "nothing"
	}
	return strings.Join(ss, ", ")
}

func strategyText(s string) string {
	if s == string(schema.StrategyCopy) {
		return "copy (written in the write's transaction, read-your-writes)"
	}
	return "GSI (maintained by DynamoDB, eventually consistent)"
}

func indexText(ix Index) string {
	s := strategyText(ix.Strategy) + ", keyed `" + ix.PK + "`"
	if ix.SK != "" {
		s += " / `" + ix.SK + "`"
	}
	s += ", projecting " + ix.Projection
	if ix.Where != "" {
		s += ", only when " + ix.Where
	}
	s += "; read by " + listOrNothing(ix.ReadBy)
	return s
}

func (d *differ) uniques(oe, ne *Entity) {
	for _, ou := range oe.Uniques {
		if !slices.ContainsFunc(ne.Uniques, func(x Unique) bool { return x.Name == ou.Name }) {
			d.add("Uniqueness and counters", "%s claim `%s.%s` (%s no longer unique)", removed, ne.Name, ou.Name, strings.Join(ou.Fields, ", "))
		}
	}
	for _, nu := range ne.Uniques {
		i := slices.IndexFunc(oe.Uniques, func(x Unique) bool { return x.Name == nu.Name })
		switch {
		case i < 0:
			d.add("Uniqueness and counters", "%s claim `%s.%s`: %s unique", added, ne.Name, nu.Name, strings.Join(nu.Fields, ", "))
		case strings.Join(oe.Uniques[i].Fields, ",") != strings.Join(nu.Fields, ","):
			d.add("Uniqueness and counters", "%s claim `%s.%s`: %s → %s", changed, ne.Name, nu.Name, strings.Join(oe.Uniques[i].Fields, ", "), strings.Join(nu.Fields, ", "))
		}
	}
}

func (d *differ) counters(oe, ne *Entity) {
	for _, oc := range oe.Counters {
		if !slices.ContainsFunc(ne.Counters, func(x Counter) bool { return x.Name == oc.Name }) {
			d.add("Uniqueness and counters", "%s counter `%s`", removed, oc.Name)
		}
	}
	for _, nc := range ne.Counters {
		i := slices.IndexFunc(oe.Counters, func(x Counter) bool { return x.Name == nc.Name })
		if i < 0 {
			d.add("Uniqueness and counters", "%s counter `%s` at `%s` / `%s`: %s", added, nc.Name, nc.PK, nc.SK, strings.Join(nc.Values, "; "))
			continue
		}
		oc := oe.Counters[i]
		if oc.PK != nc.PK || oc.SK != nc.SK || oc.Shards != nc.Shards || strings.Join(oc.Values, ";") != strings.Join(nc.Values, ";") {
			d.add("Uniqueness and counters", "%s counter `%s`: %s → %s", changed, nc.Name, counterText(oc), counterText(nc))
		}
	}
}

func counterText(c Counter) string {
	s := fmt.Sprintf("`%s` / `%s`", c.PK, c.SK)
	if c.Shards > 1 {
		s += fmt.Sprintf(" ×%d shards", c.Shards)
	}
	return s + ": " + strings.Join(c.Values, "; ")
}

func (d *differ) reads(oe, ne *Entity) {
	for _, or := range oe.Reads {
		if !slices.ContainsFunc(ne.Reads, func(x Read) bool { return x.Name == or.Name }) {
			d.add("Reads", "%s `%s.%s`", removed, ne.Name, or.Name)
		}
	}
	for _, nr := range ne.Reads {
		i := slices.IndexFunc(oe.Reads, func(x Read) bool { return x.Name == nr.Name })
		if i < 0 {
			d.add("Reads", "%s `%s.%s`: %s, %s, %s RRU per call", added, ne.Name, nr.Name, nr.ServedBy, nr.Freshness, num(nr.RRU))
			continue
		}
		or := oe.Reads[i]
		var ch []string
		if or.ServedBy != nr.ServedBy {
			ch = append(ch, fmt.Sprintf("%s → %s", or.ServedBy, nr.ServedBy))
		}
		if or.Freshness != nr.Freshness {
			ch = append(ch, fmt.Sprintf("%s → %s", or.Freshness, nr.Freshness))
		}
		if or.RRU != nr.RRU {
			ch = append(ch, fmt.Sprintf("%s → %s RRU per call", num(or.RRU), num(nr.RRU)))
		}
		if or.Rate != nr.Rate {
			ch = append(ch, fmt.Sprintf("rate %s → %s", rate(or.Rate), rate(nr.Rate)))
		}
		if len(ch) > 0 {
			d.add("Reads", "%s `%s.%s`: %s", changed, ne.Name, nr.Name, strings.Join(ch, "; "))
		}
	}
}

func (d *differ) writes(oe, ne *Entity) {
	for _, ow := range oe.Writes {
		if !slices.ContainsFunc(ne.Writes, func(x Write) bool { return x.Name == ow.Name }) {
			d.add("Writes", "%s `%s.%s`", removed, ne.Name, ow.Name)
		}
	}
	for _, nw := range ne.Writes {
		i := slices.IndexFunc(oe.Writes, func(x Write) bool { return x.Name == nw.Name })
		if i < 0 {
			d.add("Writes", "%s `%s.%s` (%s): %s; %s WRU per call", added, ne.Name, nw.Name, nw.Kind, writeShape(nw), num(nw.WRU))
			continue
		}
		ow := oe.Writes[i]
		var ch []string
		if ow.Does != nw.Does {
			ch = append(ch, fmt.Sprintf("%s → %s", ow.Does, nw.Does))
		}
		if ow.TxItems != nw.TxItems {
			ch = append(ch, fmt.Sprintf("%s → %s", txText(ow.TxItems), txText(nw.TxItems)))
		}
		if ow.ReadFirst != nw.ReadFirst {
			ch = append(ch, map[bool]string{true: "now reads the item first", false: "no longer reads the item first"}[nw.ReadFirst])
		}
		if ow.WRU != nw.WRU {
			ch = append(ch, fmt.Sprintf("%s → %s WRU per call", num(ow.WRU), num(nw.WRU)))
		}
		for _, it := range nw.Items {
			if !slices.Contains(ow.Items, it) {
				ch = append(ch, "+ "+it)
			}
		}
		for _, it := range ow.Items {
			if !slices.Contains(nw.Items, it) {
				ch = append(ch, "− "+it)
			}
		}
		if ow.Rate != nw.Rate {
			ch = append(ch, fmt.Sprintf("rate %s → %s", rate(ow.Rate), rate(nw.Rate)))
		}
		if len(ch) > 0 {
			d.add("Writes", "%s `%s.%s`: %s", changed, ne.Name, nw.Name, strings.Join(ch, "; "))
		}
	}
}

func writeShape(w Write) string {
	s := txText(w.TxItems)
	if w.ReadFirst {
		s += ", reading first"
	}
	return s + ", writes " + strings.Join(w.Items, ", ")
}

func txText(n int) string {
	if n == 0 {
		return "single item"
	}
	return fmt.Sprintf("transaction of %d items", n)
}

func (d *differ) guarantees(old, next *Snapshot) {
	if d.fresh {
		return
	}
	lists := func(title string, before, after []string) {
		for _, g := range before {
			if !slices.Contains(after, g) {
				d.add(title, "%s %s", removed, g)
			}
		}
		for _, g := range after {
			if !slices.Contains(before, g) {
				d.add(title, "%s %s", added, g)
			}
		}
	}
	lists("Guarantees", old.Guarantees, next.Guarantees)
	lists("Lifecycles", old.Lifecycles, next.Lifecycles)
}

func partitionKey(p Partition) string { return p.Space + "\x00" + p.PK }

func (d *differ) partitions(old, next *Snapshot) {
	for _, op := range old.Partitions {
		if !slices.ContainsFunc(next.Partitions, func(x Partition) bool { return partitionKey(x) == partitionKey(op) }) {
			d.add("Partitions", "%s `%s` (%s)", removed, op.PK, op.Space)
		}
	}
	for _, np := range next.Partitions {
		i := slices.IndexFunc(old.Partitions, func(x Partition) bool { return partitionKey(x) == partitionKey(np) })
		if i < 0 {
			d.add("Partitions", "%s `%s` (%s): holds %s; %s%s", added, np.PK, np.Space, strings.Join(np.Holds, ", "), sizeText(np), riskText(np))
			continue
		}
		op := old.Partitions[i]
		var ch []string
		if strings.Join(op.Holds, ",") != strings.Join(np.Holds, ",") {
			ch = append(ch, fmt.Sprintf("holds %s → %s", strings.Join(op.Holds, ", "), strings.Join(np.Holds, ", ")))
		}
		if significant(op.MaxSize, np.MaxSize) {
			ch = append(ch, fmt.Sprintf("largest %s → %s", bytesOrUnknown(op.MaxSize), bytesOrUnknown(np.MaxSize)))
		}
		if op.Grows != np.Grows {
			ch = append(ch, map[bool]string{true: "now grows without bound", false: "no longer grows without bound"}[np.Grows])
		}
		if op.Risk != np.Risk {
			ch = append(ch, fmt.Sprintf("risk %s → %s", orUnknown(op.Risk), orUnknown(np.Risk)))
		}
		if len(ch) > 0 {
			d.add("Partitions", "%s `%s` (%s): %s", changed, np.PK, np.Space, strings.Join(ch, "; "))
		}
	}
}

// significant reports whether a size changed by more than 10%.
func significant(a, b float64) bool {
	if a == 0 || b == 0 {
		return a != b
	}
	return math.Abs(a-b)/math.Max(a, b) > 0.1
}

func sizeText(p Partition) string {
	if p.MaxSize == 0 {
		return "size unknown"
	}
	return fmt.Sprintf("%s typical, %s at most", bytesOrUnknown(p.Size), bytesOrUnknown(p.MaxSize))
}

func riskText(p Partition) string {
	if p.Risk == "" {
		return ""
	}
	return fmt.Sprintf("; risk %s (%.0f%% of a partition at peak)", p.Risk, p.Headroom*100)
}

func bytesOrUnknown(b float64) string {
	if b == 0 {
		return "unknown"
	}
	return cost.HumanBytes(b)
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}

func (d *differ) findings(old, next *Snapshot) {
	byKey := func(s *Snapshot) map[string]Finding {
		out := map[string]Finding{}
		for _, f := range s.Findings {
			out[f.Key()] = f
		}
		return out
	}
	ok, nk := byKey(old), byKey(next)
	for _, f := range next.Findings {
		o, had := ok[f.Key()]
		switch {
		case !had && f.Accepted == "":
			d.add("Findings", "%s %s `%s`, %s: %s", added, f.Severity, f.Rule, f.Subject, f.Message)
		case !had:
			d.add("Findings", "%s %s `%s`, %s, accepted: %s", added, f.Severity, f.Rule, f.Subject, f.Accepted)
		case o.Accepted == "" && f.Accepted != "":
			d.add("Findings", "%s `%s`, %s, now accepted: %s", changed, f.Rule, f.Subject, f.Accepted)
		case o.Accepted != "" && f.Accepted == "":
			d.add("Findings", "%s `%s`, %s, no longer accepted: %s", changed, f.Rule, f.Subject, f.Message)
		case o.Accepted != f.Accepted:
			d.add("Findings", "%s `%s`, %s, accepted for a new reason: %s", changed, f.Rule, f.Subject, f.Accepted)
		case o.Severity != f.Severity:
			d.add("Findings", "%s `%s`, %s: %s → %s", changed, f.Rule, f.Subject, o.Severity, f.Severity)
		}
	}
	for _, f := range old.Findings {
		if _, has := nk[f.Key()]; !has {
			d.add("Findings", "%s resolved: `%s`, %s", removed, f.Rule, f.Subject)
		}
	}
}

func (d *differ) costs(old, next *Snapshot) {
	if !significant(old.StorageBytes, next.StorageBytes) && math.Abs(old.MonthlyUSD-next.MonthlyUSD) < 0.01 {
		return
	}
	if len(d.sections) == 0 && d.lead == "" {
		return // cost moved only with docs or rates of nothing architectural: not worth a section alone
	}
	d.add("Costs", "storage %s → %s; $%.2f → $%.2f per month at the declared volumes and rates", bytesOrUnknown(old.StorageBytes), bytesOrUnknown(next.StorageBytes), old.MonthlyUSD, next.MonthlyUSD)
}

func rate(r float64) string {
	if r == 0 {
		return "none"
	}
	return num(r) + "/s"
}

func num(f float64) string { return schema.Number(math.Round(f*100) / 100) }
