package docs

import (
	"fmt"
	"strings"

	"github.com/nicklanng/dynago/internal/schema"
)

// reads renders every declared read as one table.
func (d *doc) reads() {
	d.p("## Reads")
	d.p("")
	d.p("Every read the code can make. A read that isn't listed has no method, so a new way of reading the data can only arrive as a change to the schema, and to this document.")
	d.p("")
	d.p("| Read | Answers | Served by | Freshness | Returns | RRU per call p50/p99 | Rate |")
	d.p("|---|---|---|---|---|---|---|")
	for _, er := range d.r.Entities {
		for _, rc := range er.Reads {
			a := rc.Access
			d.p("| `%s.%s` | %s | %s | %s | %s | %s | %s |", a.Entity.Name, a.Name, escape(answers(a)), escape(servedBy(a)), freshnessText(a),
				escape(d.returns(a)), unitsText(rc.RRU), rateText(a.Rate))
		}
	}
	d.p("")
	var scans []*schema.Access
	for _, e := range d.m.Entities {
		for _, a := range e.Access {
			if a.Kind == schema.AccessScan {
				scans = append(scans, a)
			}
		}
	}
	for _, a := range scans {
		d.p("- **`%s.%s` scans the whole table.** Reason: %s", a.Entity.Name, a.Name, escape(a.Reason))
	}
	if len(scans) > 0 {
		d.p("")
	}
}

// answers says what a read answers: its doc, or a description from its shape.
func answers(a *schema.Access) string {
	if a.Doc != "" {
		return firstSentence(a.Doc)
	}
	e := a.Entity
	switch {
	case a.Of != nil:
		return fmt.Sprintf("%s under one %s, each kind on its own.", strings.Join(ofNames(a), ", "), join(names(e.PK.Fields)))
	case a.Batch > 0:
		return fmt.Sprintf("Several %s, each by %s.", schema.Plural(e.Name), join(names(e.KeyFields())))
	case a.All:
		return fmt.Sprintf("Every %s count of a %s: one for each %s.", a.Counter.Name, join(names(a.Counter.PK.Fields)), join(names(a.Counter.ItemFields())))
	}
	switch a.Kind {
	case schema.AccessGet:
		return fmt.Sprintf("One %s, by %s.", e.Name, join(names(e.KeyFields())))
	case schema.AccessGetUnique:
		what := "a " + join(names(a.Unique.Fields))
		if a.Unique.Set != nil {
			what = "an element of " + a.Unique.Set.Name
		}
		return fmt.Sprintf("The %s holding %s.", e.Name, what)
	case schema.AccessCounter:
		return fmt.Sprintf("The %s counts for a %s.", a.Counter.Name, join(names(a.Counter.KeyFields())))
	case schema.AccessScan:
		return fmt.Sprintf("Every %s in the table.", e.Name)
	}
	pk := a.QueryPK()
	sk, hasSK := a.QuerySK()
	if !hasSK {
		return fmt.Sprintf("%s with a given %s.", schema.Plural(e.Name), join(names(pk.Fields)))
	}
	order := "ascending"
	if a.Desc {
		order = "descending"
	}
	by := strings.Trim(sk.Raw, "#")
	if f := sk.FirstField(); f != "" {
		by = f
	}
	return fmt.Sprintf("%s with a given %s, by %s (%s).", schema.Plural(e.Name), join(names(pk.Fields)), by, order)
}

// ofNames names the kinds a partition read returns: "the Thread", "its Messages".
func ofNames(a *schema.Access) []string {
	var out []string
	for i, oe := range a.Of {
		n := schema.Plural(oe.Name)
		if oe.Singleton() {
			n = "the " + oe.Name
		}
		if i == 0 {
			n = strings.ToUpper(n[:1]) + n[1:]
		}
		out = append(out, n)
	}
	return out
}

func servedBy(a *schema.Access) string {
	switch {
	case a.Of != nil:
		return "Query of the whole partition, keeping these kinds"
	case a.Batch > 0:
		return "BatchGetItem (one per 100 keys)"
	case a.All:
		return fmt.Sprintf("Query of counter %s's items", a.Counter.Name)
	}
	switch a.Kind {
	case schema.AccessGet:
		return "GetItem"
	case schema.AccessGetUnique:
		return fmt.Sprintf("claim %s, then the item (2 GetItems)", a.Unique.Name)
	case schema.AccessCounter:
		if a.Counter.Shards > 1 {
			return fmt.Sprintf("counter %s (%d shards, one BatchGetItem)", a.Counter.Name, a.Counter.Shards)
		}
		return fmt.Sprintf("counter %s (GetItem)", a.Counter.Name)
	case schema.AccessScan:
		return "Scan of the whole table, one page per call"
	}
	switch {
	case a.Index == nil:
		return fmt.Sprintf("Query of the %s's partition", a.Entity.Name)
	case a.Index.Strategy == schema.StrategyCopy:
		return fmt.Sprintf("Query of copy %s", a.Index.Name)
	}
	return fmt.Sprintf("Query of GSI %s", a.Index.Name)
}

func freshnessText(a *schema.Access) string {
	strong := a.Consistent || a.Kind == schema.AccessGetUnique
	switch a.Freshness {
	case schema.FreshnessImmediate:
		return "immediate"
	case schema.FreshnessEventual:
		if strong {
			return "eventual; read strongly anyway"
		}
		return "eventual"
	}
	if strong {
		return "strong (not stated)"
	}
	return "eventual (not stated)"
}

// returns says how many items a read returns; for a query, how many are in its partition and how
// many pages reading them all takes.
func (d *doc) returns(a *schema.Access) string {
	switch {
	case a.Batch > 0:
		return fmt.Sprintf("the ones that exist: %d keys a call typically", a.Batch)
	case a.All:
		return fmt.Sprintf("pages of %d counter items, a page at a time", a.Page)
	}
	switch a.Kind {
	case schema.AccessGet, schema.AccessGetUnique:
		return "one"
	case schema.AccessCounter:
		return "the counts"
	}
	st := d.a.Reads[a]
	unit := fmt.Sprintf("pages of %d", a.Page)
	if st == nil || !st.Items.Known {
		return fmt.Sprintf("%s, a page at a time (volume not declared)", unit)
	}
	s := fmt.Sprintf("%s typically", itemsText(st.Items.Typical))
	if st.Items.MaxKnown && st.Items.Max != st.Items.Typical {
		s += fmt.Sprintf(", %s at most", schema.Number(roundCount(st.Items.Max)))
	}
	if st.Filtered {
		s += " (fewer: only those matching the index's where)"
	}
	pages := schema.Number(st.Pages.Typical)
	if st.Pages.MaxKnown && st.Pages.Max != st.Pages.Typical {
		pages += "–" + schema.Number(st.Pages.Max)
	}
	if pages == "1" {
		unit = fmt.Sprintf("page of %d", a.Page)
	}
	if a.Kind == schema.AccessScan {
		if !st.Pages.Known {
			return fmt.Sprintf("%s, over pages of %d of the whole table", s, a.Page)
		}
		return fmt.Sprintf("%s, over %s %s of the whole table", s, pages, unit)
	}
	return fmt.Sprintf("%s: %s %s", s, pages, unit)
}

func itemsText(n float64) string {
	if n == 1 {
		return "1 item"
	}
	return schema.Number(roundCount(n)) + " items"
}

func rateText(r float64) string {
	if r <= 0 {
		return "—"
	}
	return schema.Number(r) + "/s"
}

// writes renders every declared write: what it does, what it checks, and every item it changes.
func (d *doc) writes() {
	d.p("## Writes")
	d.p("")
	d.p("Every write the code can make, and everything each changes. Each is atomic: all of it happens, or none of it does.")
	d.p("")
	d.p("| Write | Does | Checks | Changes | Reads first | Atomic | WRU per call p50/p99 | Rate |")
	d.p("|---|---|---|---|---|---|---|---|")
	for _, er := range d.r.Entities {
		for i, w := range er.Entity.Writes {
			wc := er.Writes[i]
			reads := "no"
			switch {
			case w.Transition:
				reads = "no (read-free)"
			case wc.ReadFirst:
				reads = "yes"
			}
			atomic := "single item"
			if wc.Transactional {
				atomic = fmt.Sprintf("transaction, %d items", wc.MaxTxItems)
			}
			what, units := does(w), unitsText(wc.WRU)
			if w.Batch > 0 {
				what = fmt.Sprintf("For each of several %s (%d a call): %s", schema.Plural(w.Entity.Name), w.Batch, strings.ToLower(what[:1])+what[1:])
				reads = "yes, together"
				atomic = fmt.Sprintf("each transaction, not the batch: up to %d %s to one", wc.BatchSize, schema.Plural(w.Entity.Name))
			}
			d.p("| `%s.%s` | %s | %s | %s | %s | %s | %s | %s |", w.Entity.Name, w.Name, escape(what), escape(strings.Join(checks(w), "; ")),
				strings.Join(wc.Items, "<br>"), reads, atomic, units, rateText(w.Rate))
		}
	}
	d.p("")
}

// does says what a write changes on its own item.
func does(w *schema.Write) string {
	var parts []string
	switch w.Kind {
	case schema.WriteCreate:
		parts = append(parts, "creates a "+w.Entity.Name)
	case schema.WriteDelete:
		parts = append(parts, "deletes a "+w.Entity.Name)
	}
	for _, f := range w.Args {
		parts = append(parts, "sets `"+f.Name+"`")
	}
	for _, f := range w.Patch {
		parts = append(parts, "sets `"+f.Name+"` if given")
	}
	for _, s := range w.Sets {
		parts = append(parts, fmt.Sprintf("sets `%s` = %s", s.Field.Name, value(s.Value)))
	}
	for _, el := range w.Elems {
		t := elemsText(el)
		parts = append(parts, strings.Replace(strings.Replace(t, "add ", "adds ", 1), "remove ", "removes ", 1))
	}
	for _, rq := range w.Requires {
		if eff := rq.Effect(w.Entity.Name); eff != "" {
			parts = append(parts, strings.Replace(eff, "sets its ", "sets the "+rq.Name+"'s ", 1))
		}
	}
	return capital(strings.Join(parts, ", "))
}

// checks lists the conditions a write enforces: its preconditions, requirements, uniqueness and
// counter bounds.
func checks(w *schema.Write) []string {
	e := w.Entity
	var out []string
	switch w.Kind {
	case schema.WriteCreate:
		out = append(out, "no "+e.Name+" at the key")
	default:
		out = append(out, "the "+e.Name+" exists")
	}
	if len(w.When) > 0 {
		out = append(out, schema.PredText(w.When, e.Name))
	}
	for _, rq := range w.Requires {
		if !rq.CanFail() {
			continue
		}
		out = append(out, requirement(rq, e.Name))
	}
	changed := map[*schema.Field]bool{}
	for _, f := range w.Changed() {
		changed[f] = true
	}
	for _, u := range e.Uniques {
		touched := w.Kind == schema.WriteCreate
		for _, f := range u.Fields {
			touched = touched || changed[f]
		}
		if touched && w.Kind != schema.WriteDelete {
			out = append(out, fmt.Sprintf("%s unique (claim %s)", join(names(u.Fields)), u.Name))
		}
	}
	for _, c := range e.Counters {
		for _, v := range c.Values {
			switch {
			case v.Limit > 0 && w.Kind == schema.WriteCreate:
				out = append(out, fmt.Sprintf("%s.%s stays at most %d", c.Name, v.Name, v.Limit))
			case v.LimitArg && contains(w.Limits, v):
				out = append(out, fmt.Sprintf("%s.%s stays within the caller's limit", c.Name, v.Name))
			}
			if v.HasMin && w.Kind != schema.WriteCreate && (w.Kind == schema.WriteDelete || feeds(w, c, v)) {
				out = append(out, fmt.Sprintf("%s.%s stays at least %d", c.Name, v.Name, v.Min))
			}
		}
	}
	for _, f := range e.Fields {
		if f.Required && !f.Key && (w.Kind == schema.WriteCreate || changed[f]) {
			out = append(out, "required fields are set")
			break
		}
	}
	if w.VersionRequired {
		out = append(out, "the caller's version is current")
	}
	return out
}

// requirement states what a requires entry checks: "the Tool exists with status = \"available\"".
func requirement(rq *schema.Require, source string) string {
	switch {
	case rq.Counter != nil:
		var parts []string
		for _, p := range rq.CounterWhen {
			parts = append(parts, fmt.Sprintf("%s = %d", p.Value.Name, p.Equals))
		}
		return fmt.Sprintf("counter %s has %s", rq.Counter.Name, strings.Join(parts, " and "))
	case rq.Ensure:
		return fmt.Sprintf("any %s there is has %s (none is created)", rq.Name, schema.PredText(rq.When, source))
	case rq.Optional:
		return fmt.Sprintf("any %s has %s (none is fine)", rq.Name, schema.PredText(rq.When, source))
	case len(rq.When) > 0:
		return fmt.Sprintf("the %s exists with %s", rq.Name, schema.PredText(rq.When, source))
	}
	return "the " + rq.Name + " exists"
}

func contains(vs []*schema.CounterValue, v *schema.CounterValue) bool {
	for _, x := range vs {
		if x == v {
			return true
		}
	}
	return false
}

func names(fs []*schema.Field) []string {
	out := make([]string, len(fs))
	for i, f := range fs {
		out[i] = f.Name
		if f.Type == schema.TypeStringSet {
			// In a key, a set stands for one of its elements.
			out[i] = "element of " + f.Name
		}
	}
	return out
}

// join lists names for people: "a", "a and b", "a, b and c".
func join(ss []string) string {
	switch len(ss) {
	case 0:
		return ""
	case 1:
		return ss[0]
	}
	return strings.Join(ss[:len(ss)-1], ", ") + " and " + ss[len(ss)-1]
}
