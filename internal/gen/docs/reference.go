package docs

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/nicklanng/dynago"
	"github.com/nicklanng/dynago/internal/cost"
	"github.com/nicklanng/dynago/internal/schema"
)

// reference renders each entity in full: fields, stored items with example keys, key conditions,
// errors and a diagram. It is for the people implementing against the store.
func (d *doc) reference() {
	d.p("## Reference")
	d.p("")
	d.p("Each entity in full: its fields, every item stored for it, the key condition of each read, and the errors each write returns.")
	d.p("")
	for _, e := range d.m.Entities {
		d.entity(e)
	}
}

func (d *doc) entity(e *schema.Entity) {
	er := d.r.Entity(e)
	d.p("### %s", e.Name)
	d.p("")
	if e.Doc != "" {
		d.p("%s", e.Doc)
		d.p("")
	}
	d.p("Schema version **%d**. Go type `%s`, store `Store.%s`.", e.Version, e.GoName, schema.Plural(e.GoName))
	d.p("")
	d.diagram(e)

	d.p("#### Fields")
	d.p("")
	d.p("| Field | Type | Attribute | Size p50/p99 | Notes |")
	d.p("|---|---|---|---|---|")
	for _, f := range e.Fields {
		var notes []string
		if f.Key {
			notes = append(notes, "key")
		}
		if e.TTL == f {
			notes = append(notes, "TTL: the item expires at this time")
		}
		if f.CopyOf != nil {
			notes = append(notes, fmt.Sprintf("**copy of %s.%s, not kept in sync by dynago**", f.CopyOfEntity.Name, f.CopyOf.Name))
		}
		if f.SnapshotOf != nil {
			notes = append(notes, fmt.Sprintf("snapshot of %s.%s when written, deliberately never updated", f.SnapshotOfEntity.Name, f.SnapshotOf.Name))
		}
		if f.Ref != nil {
			notes = append(notes, "holds a "+f.Ref.Name+"'s key")
		}
		if f.Required {
			notes = append(notes, "required")
		}
		if f.Doc != "" {
			notes = append(notes, f.Doc)
		}
		typ := string(f.Type)
		if f.Type == schema.TypeEnum {
			typ = "enum: " + strings.Join(f.Enum, ", ")
		}
		d.p("| `%s` | %s | `%s` | %d / %d B | %s |", f.Name, typ, f.Attr, f.SizeP50, f.SizeP99, escape(strings.Join(notes, "; ")))
	}
	d.p("")

	d.p("#### Stored items")
	d.p("")
	d.p("Every item that exists because of %s %s, and what keeps it up to date.", article(e.Name), e.Name)
	d.p("")
	d.p("| Item | Partition key | Sort key | Example | Size p50/p99 | Maintained by |")
	d.p("|---|---|---|---|---|---|")
	d.p("| **%s** | `%s` | `%s` | %s | %s | the writes below |", e.Name, e.PK.Raw, e.SK.Raw,
		example(e, e.PK, e.SK, true), sizeText(er.Item))
	for _, ix := range e.Indexes {
		size := er.Indexes[ix.Name]
		where := ""
		if len(ix.Where) > 0 {
			where = " Only when " + predText(ix.Where) + "."
		}
		if ix.Strategy == schema.StrategyGSI {
			sk := "—"
			if ix.HasSK {
				sk = "`" + ix.SK.Raw + "`"
			}
			d.p("| GSI `%s` entry | `%s` | %s | %s | %s | DynamoDB, from the item's `%s`/`%s` attributes (eventually consistent). Sparse: absent when a key field is empty.%s |",
				ix.Name, ix.PK.Raw, sk, example(e, ix.PK, ix.SK, ix.HasSK), sizeText(size), ix.PKAttr, ix.SKAttr, where)
		} else {
			d.p("| Copy `%s` | `%s` | `%s` | %s | %s | the writes below, in the same transaction as the item.%s |",
				ix.Name, ix.PK.Raw, ix.SK.Raw, example(e, ix.PK, ix.SK, true), sizeText(size), where)
		}
	}
	for _, u := range e.Uniques {
		what := fmt.Sprintf("a conditional put makes %s unique", fieldCodes(u.Fields))
		if u.Set != nil {
			what = fmt.Sprintf("one claim per element of `%s`, each taken by a conditional put", u.Set.Name)
		}
		d.p("| Claim `%s` | `%s` | `%s` | %s | ~%s each | the writes below, in the same transaction; %s. |",
			u.Name, u.PK.Raw, u.SK.Raw, example(e, u.PK, u.SK, true), cost.Human(cost.ClaimSize.P50), what)
	}
	for _, c := range e.Counters {
		pk := c.PK.Raw
		if c.Shards > 1 {
			pk += fmt.Sprintf("#S{0..%d}", c.Shards-1)
		}
		d.p("| Counter `%s` | `%s` | `%s` | %s | ~%s | the writes below, with atomic ADDs in the same transaction. |",
			c.Name, pk, c.SK.Raw, example(e, c.PK, c.SK, true), cost.Human(cost.CounterSize(c).P50))
	}
	d.p("")

	if len(e.Indexes) > 0 {
		d.p("#### Indexes")
		d.p("")
		for _, ix := range e.Indexes {
			strategy := "global secondary index, maintained by DynamoDB, eventually consistent"
			if ix.Strategy == schema.StrategyCopy {
				strategy = "copy items written in the same transaction as the entity, readable strongly consistently"
			}
			if ix.StrategyInferred {
				strategy += "; chosen because " + ix.StrategyReason
			}
			d.p("- **%s** (%s). %s Projection: %s.", ix.Name, strategy, strings.TrimSpace(ix.Doc), projText(ix))
		}
		d.p("")
	}
	if len(e.Counters) > 0 {
		d.p("#### Counters")
		d.p("")
		for _, c := range e.Counters {
			extra := ""
			if c.Shards > 1 {
				extra = fmt.Sprintf(" Spread over %d shards; reads sum them with one BatchGetItem.", c.Shards)
			}
			d.p("- **%s**, keyed by %s. %s%s", c.Name, fieldCodes(c.KeyFields()), strings.TrimSpace(c.Doc), extra)
			for _, v := range c.Values {
				d.p("  - `%s`: %s.", v.Attr, counterValueText(v))
			}
		}
		d.p("")
	}
	if len(e.Uniques) > 0 {
		d.p("#### Uniqueness")
		d.p("")
		for _, u := range e.Uniques {
			rule := fmt.Sprintf("no two %s items share %s", e.Name, fieldCodes(u.Fields))
			if u.Set != nil {
				rule = fmt.Sprintf("no element of `%s` appears in two %s items", u.Set.Name, e.Name)
				if len(u.Fields) > 1 {
					rule = fmt.Sprintf("for the same %s, %s", fieldCodes(slices.DeleteFunc(slices.Clone(u.Fields), func(f *schema.Field) bool { return f == u.Set })), rule)
				}
			}
			d.p("- **%s**: %s. %s", u.Name, rule, strings.TrimSpace(u.Doc))
		}
		d.p("")
	}

	d.p("#### Access patterns")
	d.p("")
	if len(e.Access) == 0 {
		d.p("None declared.")
		d.p("")
	} else {
		d.p("| Method | Reads | Key condition | Consistency | Requests | RRU per call p50/p99 |")
		d.p("|---|---|---|---|---|---|")
		for i, a := range e.Access {
			rc := er.Reads[i]
			d.p("| `%s` | %s | %s | %s | %s | %s |", a.Name, readsText(a), escape(keyCondition(a)),
				consistencyText(a), rc.Requests, unitsText(rc.RRU))
		}
		d.p("")
		d.docList(func(yield func(name, doc string)) {
			for _, a := range e.Access {
				yield(a.Name, a.Doc)
			}
		})
	}

	d.p("#### Writes")
	d.p("")
	if len(e.Writes) == 0 {
		d.p("None declared.")
		d.p("")
	} else {
		d.p("| Method | Does | Items written | Reads first | Atomic | Version check | WRU per call p50/p99 | Fails with |")
		d.p("|---|---|---|---|---|---|---|---|")
		for i, w := range e.Writes {
			wc := er.Writes[i]
			reads := "no"
			switch {
			case w.Transition:
				reads = "no: read-free (reads only if the item is not in the assumed state)"
			case wc.ReadFirst:
				reads = "yes (1 consistent read; none with `dynago.From`)"
			}
			version := "optional"
			switch {
			case w.Kind == schema.WriteCreate:
				version = "—"
			case w.VersionRequired:
				version = "**required**"
			}
			atomic := "single item"
			if wc.Transactional {
				atomic = fmt.Sprintf("transaction (%d items)", wc.MaxTxItems)
			}
			d.p("| `%s` | %s | %s | %s | %s | %s | %s | %s |", w.Name, escape(writeText(w)), strings.Join(wc.Items, "<br>"),
				reads, atomic, version, unitsText(wc.WRU), strings.Join(writeErrors(w), "<br>"))
		}
		d.p("")
		d.docList(func(yield func(name, doc string)) {
			for _, w := range e.Writes {
				yield(w.Name, w.Doc)
			}
		})
	}
}

// docList prints the documented methods as a list, followed by a blank line if there were any.
func (d *doc) docList(each func(yield func(name, doc string))) {
	found := false
	each(func(name, doc string) {
		if doc != "" {
			d.p("- `%s`: %s", name, doc)
			found = true
		}
	})
	if found {
		d.p("")
	}
}

func (d *doc) diagram(e *schema.Entity) {
	d.p("```mermaid")
	d.p("flowchart LR")
	id := func(kind, name string) string { return kind + "_" + name }
	d.p("  %s[\"%s item<br/>%s<br/>%s\"]", id("item", e.Name), e.Name, mm(e.PK.Raw), mm(e.SK.Raw))
	for _, ix := range e.Indexes {
		if ix.Strategy == schema.StrategyGSI {
			d.p("  %s[(\"GSI %s<br/>%s\")]", id("ix", ix.Name), ix.Name, mm(ix.PK.Raw))
			d.p("  %s -. DynamoDB maintains .-> %s", id("item", e.Name), id("ix", ix.Name))
		} else {
			d.p("  %s[\"copy %s<br/>%s\"]", id("ix", ix.Name), ix.Name, mm(ix.PK.Raw))
		}
	}
	for _, u := range e.Uniques {
		d.p("  %s[\"claim %s<br/>%s\"]", id("claim", u.Name), u.Name, mm(u.PK.Raw))
	}
	for _, c := range e.Counters {
		d.p("  %s[\"counter %s<br/>%s\"]", id("counter", c.Name), c.Name, mm(c.PK.Raw+" / "+c.SK.Raw))
	}
	for _, w := range e.Writes {
		d.p("  %s([\"%s\"])", id("w", w.Name), w.Name)
		targets := []string{id("item", e.Name)}
		all := w.Kind != schema.WriteUpdate
		changed := map[*schema.Field]bool{}
		for _, f := range w.Changed() {
			changed[f] = true
		}
		touched := func(fs []*schema.Field) bool {
			for _, f := range fs {
				if changed[f] {
					return true
				}
			}
			return false
		}
		for _, ix := range e.Indexes {
			if ix.Strategy == schema.StrategyCopy && (all || touched(ix.ProjectedFields()) || touched(ix.PK.Fields) || touched(ix.SK.Fields)) {
				targets = append(targets, id("ix", ix.Name))
			}
		}
		for _, u := range e.Uniques {
			if all || touched(u.Fields) {
				targets = append(targets, id("claim", u.Name))
			}
		}
		for _, c := range e.Counters {
			fs := c.KeyFields()
			for _, v := range c.Values {
				if v.Sum != nil {
					fs = append(fs, v.Sum)
				}
				for _, p := range v.Where {
					fs = append(fs, p.Field)
				}
			}
			if all || touched(fs) {
				targets = append(targets, id("counter", c.Name))
			}
		}
		d.p("  %s --> %s", id("w", w.Name), strings.Join(targets, " & "))
		for _, rq := range w.Requires {
			verb := "checks"
			switch {
			case len(rq.Sets) > 0:
				verb = "checks and changes"
			case rq.Consume:
				verb = "checks and deletes"
			}
			if rq.Counter != nil && rq.Counter.Entity == e {
				d.p("  %s -. %s .-> %s", id("w", w.Name), verb, id("counter", rq.Counter.Name))
				continue
			}
			label := rq.Name + " item"
			if rq.Counter != nil {
				label = "counter " + rq.Name
			}
			d.p("  %s[\"%s\"]", id("other", rq.Name), label)
			d.p("  %s -. %s .-> %s", id("w", w.Name), verb, id("other", rq.Name))
		}
	}
	for _, a := range e.Access {
		d.p("  %s{{\"%s\"}}", id("r", a.Name), a.Name)
		var target string
		switch {
		case a.Kind == schema.AccessCounter:
			target = id("counter", a.Counter.Name)
		case a.Kind == schema.AccessGetUnique:
			target = id("claim", a.Unique.Name) + " & " + id("item", e.Name)
		case a.Index != nil:
			target = id("ix", a.Index.Name)
		default:
			target = id("item", e.Name)
		}
		d.p("  %s --> %s", target, id("r", a.Name))
	}
	d.p("```")
	d.p("")
	d.p("Writes on the left, reads on the right. Dotted arrows are maintained by DynamoDB; solid arrows are written by the generated code.")
	d.p("")
}

// ---- text helpers ----

func ttlAttr(m *schema.Model) string {
	for _, e := range m.Entities {
		if e.TTL != nil {
			return m.Table.TTLAttr
		}
	}
	return ""
}

func readsText(a *schema.Access) string {
	switch a.Kind {
	case schema.AccessGet:
		return "item by key"
	case schema.AccessGetUnique:
		return "claim `" + a.Unique.Name + "`, then the item"
	case schema.AccessCounter:
		return "counter `" + a.Counter.Name + "`"
	case schema.AccessScan:
		return fmt.Sprintf("the whole table, page %d (max %d)", a.Page, a.MaxPage)
	}
	if a.Index == nil {
		proj := ""
		if a.Project != nil {
			proj = ", only " + fieldCodes(a.ProjectedFields())
		}
		return fmt.Sprintf("entity partition%s, page %d (max %d)", proj, a.Page, a.MaxPage)
	}
	kind := "GSI"
	if a.Index.Strategy == schema.StrategyCopy {
		kind = "copies"
	}
	return fmt.Sprintf("%s `%s`, page %d (max %d)", kind, a.Index.Name, a.Page, a.MaxPage)
}

func keyCondition(a *schema.Access) string {
	e := a.Entity
	switch a.Kind {
	case schema.AccessGet:
		return fmt.Sprintf("`PK = %s`, `SK = %s`", e.PK.Raw, e.SK.Raw)
	case schema.AccessGetUnique:
		return fmt.Sprintf("`PK = %s`", a.Unique.PK.Raw)
	case schema.AccessCounter:
		return fmt.Sprintf("`PK = %s`, `SK = %s`", a.Counter.PK.Raw, a.Counter.SK.Raw)
	case schema.AccessScan:
		return fmt.Sprintf("none: a Scan, filtered to `_t = %s`", e.Name)
	}
	pkAttr, skAttr := "PK", "SK"
	if a.Index != nil && a.Index.Strategy == schema.StrategyGSI {
		pkAttr, skAttr = a.Index.PKAttr, a.Index.SKAttr
	}
	cond := fmt.Sprintf("`%s = %s`", pkAttr, a.QueryPK().Raw)
	sk, ok := a.QuerySK()
	switch {
	case a.Range != nil:
		cond += fmt.Sprintf(", `%s` between optional bounds on `%s`", skAttr, a.Range.Name)
	case ok && sk.LiteralPrefix() != "":
		cond += fmt.Sprintf(", `begins_with(%s, %q)`", skAttr, sk.LiteralPrefix())
	}
	order := "ascending"
	if a.Desc {
		order = "descending"
	}
	return cond + ", " + order
}

func consistencyText(a *schema.Access) string {
	switch {
	case a.Kind == schema.AccessGetUnique, a.Consistent:
		return "strong"
	case a.Index != nil && a.Index.Strategy == schema.StrategyGSI:
		return "eventual (GSI)"
	}
	return "eventual"
}

func writeText(w *schema.Write) string {
	switch w.Kind {
	case schema.WriteCreate:
		text := "create (fails if it exists)"
		for _, s := range w.Sets {
			text += fmt.Sprintf(", set `%s` = %s", s.Field.Name, value(s.Value))
		}
		return text + requiresText(w)
	case schema.WriteDelete:
		return "delete (fails if absent)" + requiresText(w)
	}
	var parts []string
	for _, f := range w.Args {
		parts = append(parts, "set `"+f.Name+"`")
	}
	for _, f := range w.Patch {
		parts = append(parts, "set `"+f.Name+"` if given")
	}
	for _, s := range w.Sets {
		parts = append(parts, fmt.Sprintf("set `%s` = %s", s.Field.Name, value(s.Value)))
	}
	text := strings.Join(parts, ", ")
	if len(w.When) > 0 {
		text += " when " + predText(w.When)
	}
	return text + requiresText(w)
}

func requiresText(w *schema.Write) string {
	var parts []string
	for _, rq := range w.Requires {
		t := "requires " + rq.Condition(w.Entity.Name)
		if eff := rq.Effect(w.Entity.Name); eff != "" {
			t += " and " + eff
		}
		parts = append(parts, t)
	}
	if len(parts) == 0 {
		return ""
	}
	return "; " + strings.Join(parts, "; ")
}

func writeErrors(w *schema.Write) []string {
	e := w.Entity
	var out []string
	switch w.Kind {
	case schema.WriteCreate:
		out = append(out, "`Err"+e.GoName+"Exists`")
	default:
		out = append(out, "`Err"+e.GoName+"NotFound`")
	}
	if len(w.When) > 0 {
		out = append(out, "`Err"+e.GoName+w.GoName+"Precondition`")
	}
	for _, rq := range w.Requires {
		if rq.CanFail() {
			out = append(out, "`"+rq.ErrName+"`")
		}
	}
	for _, f := range e.Fields {
		if f.Required && !f.Key && (w.Kind == schema.WriteCreate || changedField(w, f)) {
			out = append(out, "`dynago.ErrFieldRequired` (a required field is empty)")
			break
		}
	}
	if len(w.Limits) > 0 {
		out = append(out, "`dynago.ErrLimitRequired` (no limit given)")
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
			out = append(out, "`"+u.ErrName+"`")
		}
	}
	for _, c := range e.Counters {
		for _, v := range c.Values {
			if v.Limit > 0 && w.Kind == schema.WriteCreate {
				out = append(out, "`"+v.ErrName+"`")
			}
		}
	}
	for _, v := range w.Limits {
		out = append(out, "`"+v.ErrName+"`")
	}
	if w.Kind != schema.WriteCreate {
		for _, c := range e.Counters {
			for _, v := range c.Values {
				if v.HasMin && (w.Kind == schema.WriteDelete || feeds(w, c, v)) {
					out = append(out, "`"+v.MinErrName+"`")
				}
			}
		}
	}
	if w.Kind != schema.WriteCreate {
		out = append(out, "`dynago.ErrVersionMismatch` (with a version)")
	}
	if w.VersionRequired {
		out = append(out, "`dynago.ErrVersionRequired`")
	}
	if w.ReadFirst {
		out = append(out, "`dynago.ErrConflict` (after retries)")
	}
	return out
}

func counterValueText(v *schema.CounterValue) string {
	var b strings.Builder
	if v.Sum != nil {
		fmt.Fprintf(&b, "sum of `%s`", v.Sum.Name)
	} else {
		b.WriteString("count of items")
	}
	if len(v.Where) > 0 {
		fmt.Fprintf(&b, " where %s", predText(v.Where))
	}
	switch {
	case v.LimitArg:
		b.WriteString("; writes that grow it take a caller-supplied limit")
	case v.Limit > 0:
		fmt.Fprintf(&b, "; never exceeds %d", v.Limit)
	}
	if v.HasMin {
		fmt.Fprintf(&b, "; never goes below %d", v.Min)
	}
	if v.Doc != "" {
		b.WriteString(". " + strings.TrimSuffix(v.Doc, "."))
	}
	return b.String()
}

func projText(ix *schema.Index) string {
	switch ix.Projection {
	case schema.ProjectAll:
		return "all fields"
	case schema.ProjectKeys:
		return "key fields only"
	}
	return fieldCodes(ix.Project) + " plus key fields"
}

func predText(ps []*schema.Pred) string {
	var parts []string
	for _, p := range ps {
		parts = append(parts, "`"+p.Text("")+"`")
	}
	return strings.Join(parts, " and ")
}

func value(v any) string {
	if s, ok := v.(string); ok {
		return strconv.Quote(s)
	}
	return fmt.Sprint(v)
}

func fieldCodes(fs []*schema.Field) string {
	var out []string
	for _, f := range fs {
		out = append(out, "`"+f.Name+"`")
	}
	return strings.Join(out, ", ")
}

// example renders a key with the fields' example values, or the pattern where a field has none.
func example(e *schema.Entity, pk, sk schema.Template, hasSK bool) string {
	vals := map[string]string{}
	for _, f := range e.Fields {
		if f.Example == "" {
			continue
		}
		vals[f.Name] = exampleValue(f)
	}
	out := "`" + pk.Render(vals) + "`"
	if hasSK {
		out += "<br>`" + sk.Render(vals) + "`"
	}
	return out
}

func exampleValue(f *schema.Field) string {
	switch f.Type {
	case schema.TypeTime:
		for _, layout := range []string{time.RFC3339Nano, time.DateOnly} {
			if t, err := time.Parse(layout, f.Example); err == nil {
				return dynago.FmtTime(t)
			}
		}
	case schema.TypeInt:
		if n, err := strconv.ParseInt(f.Example, 10, 64); err == nil {
			return dynago.FmtInt(n)
		}
	}
	return f.Example
}

func sizeText(s cost.Size) string {
	return cost.Human(s.P50) + " / " + cost.Human(s.P99)
}

func unitsText(u cost.Units) string {
	if u.P50 == u.P99 {
		return trim(u.P50)
	}
	return trim(u.P50) + " / " + trim(u.P99)
}

func trim(f float64) string {
	return strconv.FormatFloat(f, 'f', -1, 64)
}

func escape(s string) string {
	return strings.ReplaceAll(s, "|", "\\|")
}

// mm escapes text for a quoted Mermaid label.
func mm(s string) string {
	return strings.NewReplacer("#", "#35;", `"`, "#quot;").Replace(s)
}

// article returns "a" or "an" for a word.
func article(word string) string {
	if word != "" && strings.ContainsRune("AEIOUaeiou", rune(word[0])) {
		return "an"
	}
	return "a"
}

// feeds reports whether an update changes anything a counter value depends on.
func feeds(w *schema.Write, c *schema.Counter, v *schema.CounterValue) bool {
	changed := map[*schema.Field]bool{}
	for _, f := range w.Changed() {
		changed[f] = true
	}
	for _, f := range c.KeyFields() {
		if changed[f] {
			return true
		}
	}
	if v.Sum != nil && changed[v.Sum] {
		return true
	}
	for _, p := range v.Where {
		if changed[p.Field] {
			return true
		}
	}
	return false
}

func changedField(w *schema.Write, f *schema.Field) bool {
	for _, c := range w.Changed() {
		if c == f {
			return true
		}
	}
	return false
}

// ---- more text helpers ----

func capital(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// firstSentence returns the text up to the end of its first sentence.
func firstSentence(s string) string {
	s = strings.TrimSpace(s)
	for i := 0; i < len(s)-1; i++ {
		if (s[i] == '.' || s[i] == ':') && s[i+1] == ' ' {
			if s[i] == ':' {
				return s[:i] + "."
			}
			return s[:i+1]
		}
	}
	return s
}

// anchor is GitHub's heading anchor for a simple heading.
func anchor(s string) string { return strings.ToLower(s) }

// roundCount rounds an estimated count for display: whole numbers above 10, two decimals below.
func roundCount(f float64) float64 {
	if f >= 10 {
		return float64(int64(f + 0.5))
	}
	return float64(int64(f*100+0.5)) / 100
}

// mmState escapes a state label for Mermaid.
func mmState(s string) string { return strings.ReplaceAll(s, `"`, "'") }
