package gocode

import (
	"fmt"
	"strings"

	"github.com/nicklanng/dynago/internal/schema"
)

// This file generates a write's `requires`: condition checks on other items, and changes to them
// (set, consume) made in the write's transaction.

// hasTargetWrites reports whether a write changes another item, which needs the read fallback.
func hasTargetWrites(w *schema.Write) bool {
	for _, rq := range w.Requires {
		if rq.Writes() {
			return true
		}
	}
	return false
}

// requireFuncName names the store method building a requirement's writes.
func requireFuncName(w *schema.Write, rq *schema.Require) string {
	return "require" + w.GoName + rq.Target.GoName
}

// requireOps emits the ops of a write's requires, reading source fields from recv. read is the Go
// expression telling target writes whether to read their item first. Changes to other entities
// append their derived Change to `changes`, which the caller diffs together with its own.
func (g *gen) requireOps(w *schema.Write, recv, read string) {
	e := w.Entity
	for _, rq := range w.Requires {
		var sources []*schema.Field
		vals := map[string]string{}
		for _, k := range rq.Key {
			sources = append(sources, k.Source)
			vals[k.Target.Name] = recv + "." + k.Source.GoName
		}
		g.p("// requires %s", rq.Name)
		pc := presence(sources, recv)
		if pc != "" && !rq.Optional {
			// A requirement that can't be keyed must not be skipped: that would switch the rule off.
			g.p("if %s {", negate(pc))
			g.p("return fmt.Errorf(\"%%w: %s.%s requires the %s, keyed by %s\", dynago.ErrFieldRequired)", e.Name, w.Name, rq.Name, fieldList(sources))
			g.p("}")
			pc = ""
		}
		if pc != "" {
			// An optional requirement with an empty key field has no item to require.
			g.p("if %s {", pc)
		}
		switch {
		case rq.Counter != nil:
			c := rq.Counter
			var conds []string
			for _, p := range rq.CounterWhen {
				conds = append(conds, fmt.Sprintf("{Attr: %q, Value: %d, Zero: %t}", p.Value.Attr, p.Equals, p.Equals == 0))
			}
			kv := lowerFirst(rq.Name) + "Key"
			g.p("%s := dynago.Key{PK: %s, SK: %s}", kv, tmplExprVals(c.Entity, c.PK, vals), tmplExprVals(c.Entity, c.SK, vals))
			g.p("ops = append(ops, dynago.CheckOp(%s, dynago.CheckCounter(s.t, %s, []dynago.Cond{%s}), %s))", kv, kv, strings.Join(conds, ", "), rq.ErrName)
		case rq.Writes():
			n := lowerFirst(rq.Name)
			g.p("%sOps, %sChange, err := s.%s(ctx, %s, %s)", n, n, requireFuncName(w, rq), addr(recv), read)
			g.p("if err != nil {")
			g.p("return err")
			g.p("}")
			g.p("ops = append(ops, %sOps...)", n)
			g.p("changes = append(changes, %sChange)", n)
		default:
			te := rq.Target
			kv := lowerFirst(rq.Name) + "Key"
			g.p("%s := dynago.Key{PK: %s, SK: %s}", kv, tmplExprVals(te, te.PK, vals), tmplExprVals(te, te.SK, vals))
			g.p("ops = append(ops, dynago.CheckOp(%s, dynago.CheckRequirement(s.t, dynago.Requirement{Key: %s%s}), %s))",
				kv, kv, g.requirementFields(rq, recv), rq.ErrName)
		}
		if pc != "" {
			g.p("}")
		}
	}
}

// addr returns a pointer expression for recv ("e" is already a pointer; "after" is a value).
func addr(recv string) string {
	if recv == "e" || strings.HasPrefix(recv, "&") {
		return recv
	}
	return "&" + recv
}

// requirementFields renders the When, TTLAttr and Optional fields of a dynago.Requirement literal.
func (g *gen) requirementFields(rq *schema.Require, recv string) string {
	var b strings.Builder
	if len(rq.When) > 0 {
		var conds []string
		for _, p := range rq.When {
			conds = append(conds, condLit(rq.Target, p, recv))
		}
		fmt.Fprintf(&b, ", When: []dynago.Cond{%s}", strings.Join(conds, ", "))
	}
	if rq.Target.TTL != nil {
		fmt.Fprintf(&b, ", TTLAttr: %q", g.m.Table.TTLAttr)
	}
	if rq.Optional {
		b.WriteString(", Optional: true")
	}
	return b.String()
}

// condLit renders a dynago.Cond for a predicate on another entity's field, whose value is a
// constant or a field of the writing entity read from recv.
func condLit(te *schema.Entity, p *schema.Pred, recv string) string {
	if p.Source != nil {
		x := recv + "." + p.Source.GoName
		return fmt.Sprintf("{Attr: %q, Value: %s, Zero: %s}", p.Field.Attr, x, zeroCond(p.Source, x))
	}
	return fmt.Sprintf("{Attr: %q, Value: %s, Zero: %t}", p.Field.Attr, literal(te, p.Field, p.Value), isZeroValue(p.Value))
}

// sourceValue renders the value a requirement gives field tf of te: a constant, or the writing
// entity's field converted to tf's Go type.
func sourceValue(e, te *schema.Entity, tf, sf *schema.Field, v any, recv string) string {
	if sf == nil {
		return literal(te, tf, v)
	}
	x := recv + "." + sf.GoName
	if tf.Type == schema.TypeEnum && e != te {
		return te.GoName + tf.GoName + "(" + x + ")"
	}
	return x
}

// requireFuncs emits, for each requirement that changes its item, the store method building
// those changes.
func (g *gen) requireFuncs(e *schema.Entity) {
	for _, w := range e.Writes {
		for _, rq := range w.Requires {
			if rq.Writes() {
				g.requireFunc(w, rq)
			}
		}
	}
}

func (g *gen) requireFunc(w *schema.Write, rq *schema.Require) {
	e, te := w.Entity, rq.Target
	tlo := lowerFirst(te.GoName)
	name := requireFuncName(w, rq)
	effect := rq.Effect(e.Name)
	var doc strings.Builder
	if rq.CanFail() {
		fmt.Fprintf(&doc, "%s returns the writes %s.%s makes to the %s: it requires %s, and %s.", name, e.Name, w.Name, te.Name, rq.Condition(e.Name), effect)
	} else {
		fmt.Fprintf(&doc, "%s returns the writes %s.%s makes to the %s: it %s.", name, e.Name, w.Name, te.Name, effect)
	}
	if rq.Fast {
		fmt.Fprintf(&doc, " Unless read is set, it assumes that state without reading the %s: if the assumption is wrong, the transaction fails with dynago.ErrNeedsRead and the caller runs again with read set.", te.Name)
	} else {
		fmt.Fprintf(&doc, " It reads the %s first, because the change to its derived items depends on more than the state required.", te.Name)
	}
	comment(&g.buf, doc.String())
	g.p("func (s *%sStore) %s(ctx context.Context, e *%s, read bool) ([]dynago.Op, dynago.Change, error) {", e.GoName, name, e.GoName)
	var kv []string
	for _, k := range rq.Key {
		kv = append(kv, fmt.Sprintf("%s: %s", k.Target.GoName, sourceValue(e, te, k.Target, k.Source, nil, "e")))
	}
	g.p("k := %sKey{%s}", te.GoName, strings.Join(kv, ", "))
	g.p("key, err := k.dynamoKey()")
	g.p("if err != nil {")
	g.p("return nil, dynago.Change{}, err")
	g.p("}")
	ttl, ttlGuard := "", ""
	if te.TTL != nil {
		ttl = g.m.Table.TTLAttr
		ttlGuard = fmt.Sprintf(", TTLAttr: %q", ttl)
	}
	limits := ""
	if hasLimitArgs(te) {
		limits = fmt.Sprintf(", %sLimits{}", tlo)
	}
	if !rq.Fast {
		g.p("_ = read // the %s is always read: see above", te.Name)
	} else {
		g.p("if !read {")
		if rq.Consume {
			g.p("req := dynago.Requirement{Key: key%s}", g.requirementFields(rq, "e"))
			g.p("return []dynago.Op{dynago.DeleteOp(key, dynago.ConsumeRequirement(s.t, req, %sVersion), dynago.ErrNeedsRead)}, dynago.Change{}, nil", tlo)
		} else {
			known := []string{}
			for _, f := range te.KeyFields() {
				known = append(known, fmt.Sprintf("%s: k.%s", f.GoName, f.GoName))
			}
			for _, p := range rq.When {
				known = append(known, fmt.Sprintf("%s: %s", p.Field.GoName, sourceValue(e, te, p.Field, p.Source, p.Value, "e")))
			}
			g.p("before := %s{%s}", te.GoName, strings.Join(known, ", "))
			g.p("after := before.clone()")
			g.targetSets(e, rq)
			g.p("u := s.t.Update(\"PK\", key.PK).Range(\"SK\", key.SK)")
			g.p("dynago.SetFields(u, sets)")
			g.p("dynago.GuardUpdate(u, dynago.Guard{Version: %sVersion%s}, dynago.Now())", tlo, ttlGuard)
			if te.HasDerived() {
				g.p("dynago.AtLeastVersion(u, %d)", derivedSince(te))
			}
			for _, p := range rq.When {
				g.p("dynago.CondUpdate(u, dynago.Cond%s)", condLit(te, p, "e"))
			}
			ch := "dynago.Change{}"
			if te.HasDerived() {
				ch = fmt.Sprintf("dynago.Change{Owner: key, Before: %sDerived(&before, key, %sVersion%s), After: %sDerived(&after, key, %sVersion%s)}", tlo, tlo, limits, tlo, tlo, limits)
			}
			g.p("return []dynago.Op{dynago.UpdateOp(key, u, dynago.ErrNeedsRead)}, %s, nil", ch)
		}
		g.p("}")
	}
	g.p("it, err := (&%sStore{db: s.db, t: s.t}).load(ctx, key, true)", te.GoName)
	g.p("if err == Err%sNotFound {", te.GoName)
	if rq.Optional {
		g.p("// Nothing to change, as long as one doesn't appear before the write commits.")
		g.p("return []dynago.Op{dynago.CheckOp(key, dynago.CheckAbsent(s.t, key, %q), dynago.ErrStale)}, dynago.Change{}, nil", ttl)
	} else {
		g.p("return nil, dynago.Change{}, %s", rq.ErrName)
	}
	g.p("}")
	g.p("if err != nil {")
	g.p("return nil, dynago.Change{}, err")
	g.p("}")
	g.p("if it.V > %sVersion {", tlo)
	g.p("return nil, dynago.Change{}, fmt.Errorf(\"%%w: %s at v%%d, this code is v%%d\", dynago.ErrNewerSchema, it.V, %sVersion)", te.Name, tlo)
	g.p("}")
	if len(rq.When) > 0 || te.HasDerived() || !rq.Consume {
		g.p("before := &it.%s", te.GoName)
	}
	if len(rq.When) > 0 {
		var conds []string
		for _, p := range rq.When {
			conds = append(conds, fmt.Sprintf("before.%s == %s", p.Field.GoName, sourceValue(e, te, p.Field, p.Source, p.Value, "e")))
		}
		g.p("if %s {", negate(strings.Join(conds, " && ")))
		g.p("return nil, dynago.Change{}, %s", rq.ErrName)
		g.p("}")
	}
	guard := fmt.Sprintf(`.If("$ = ? AND (attribute_not_exists($) OR $ <= ?)", "_rev", it.Rev, "_v", "_v", %sVersion)`, tlo)
	ch := "dynago.Change{}"
	if rq.Consume {
		if te.HasDerived() {
			ch = fmt.Sprintf("dynago.Change{Owner: key, Before: %sDerived(before, key, it.V%s)}", tlo, limits)
		}
		g.p("return []dynago.Op{dynago.DeleteOp(key, s.t.Delete(\"PK\", key.PK).Range(\"SK\", key.SK)%s, dynago.ErrStale)}, %s, nil", guard, ch)
	} else {
		g.p("after := before.clone()")
		var changed []*schema.Field
		for _, s := range rq.Sets {
			g.p("after.%s = %s", s.Field.GoName, sourceValue(e, te, s.Field, s.Source, s.Value, "e"))
			changed = append(changed, s.Field)
		}
		g.requiredChecks(te, changed, "after", "return nil, dynago.Change{}, err")
		g.keyPartChecks(changed, func(f *schema.Field) string { return "after." + f.GoName }, "return nil, dynago.Change{}, err")
		if te.HasDerived() {
			ch = fmt.Sprintf("dynago.Change{Owner: key, Before: %sDerived(before, key, it.V%s), After: %sDerived(&after, key, %sVersion%s)}", tlo, limits, tlo, tlo, limits)
		}
		g.p("return []dynago.Op{dynago.PutOp(key, s.t.Put(%sToItem(&after, key, it.Rev+1))%s, dynago.ErrStale)}, %s, nil", tlo, guard, ch)
	}
	g.p("}")
	g.p("")
}

// targetSets emits after's changes and the matching sets list for a requirement's set.
func (g *gen) targetSets(e *schema.Entity, rq *schema.Require) {
	te := rq.Target
	for _, s := range rq.Sets {
		g.p("after.%s = %s", s.Field.GoName, sourceValue(e, te, s.Field, s.Source, s.Value, "e"))
	}
	var changed []*schema.Field
	for _, s := range rq.Sets {
		changed = append(changed, s.Field)
	}
	g.requiredChecks(te, changed, "after", "return nil, dynago.Change{}, err")
	g.keyPartChecks(changed, func(f *schema.Field) string { return "after." + f.GoName }, "return nil, dynago.Change{}, err")
	g.p("sets := []dynago.Set{")
	for _, s := range rq.Sets {
		if s.Source == nil {
			g.p("%s,", constSetLit(te, s.Field, s.Value))
		} else {
			g.p("%s,", setLit(s.Field, "after."+s.Field.GoName))
		}
	}
	g.p("}")
	for _, s := range rq.Sets {
		if s.Field == te.TTL {
			x := "after." + s.Field.GoName
			g.p("sets = append(sets, dynago.Set{Attr: %q, Value: dynago.UnixTTL(%s), Remove: %s.IsZero()})", g.m.Table.TTLAttr, x, x)
		}
	}
}

// derivedSince returns the latest version that introduced any of an entity's counters, counter
// values or claims: items older than it may not contribute everything the current code assumes.
func derivedSince(e *schema.Entity) int {
	since := 1
	for _, c := range e.Counters {
		for _, v := range c.Values {
			since = max(since, v.Since)
		}
		since = max(since, c.Since)
	}
	for _, u := range e.Uniques {
		since = max(since, u.Since)
	}
	for _, ix := range e.Indexes {
		if ix.Strategy == schema.StrategyCopy {
			since = max(since, ix.Since)
		}
	}
	return since
}

// requiredChecks emits a check that each of fields declared required is not its zero value.
func (g *gen) requiredChecks(e *schema.Entity, fields []*schema.Field, recv, ret string) {
	for _, f := range fields {
		if !f.Required || f.Key {
			continue
		}
		g.p("if %s {", zeroCond(f, recv+"."+f.GoName))
		g.p("err := fmt.Errorf(\"%%w: %s.%s\", dynago.ErrFieldRequired)", e.Name, f.Name)
		g.p("%s", ret)
		g.p("}")
	}
}

// requiresText describes a write's requires for its doc comment.
func requiresText(w *schema.Write) string {
	var checks, effects []string
	for _, rq := range w.Requires {
		eff := rq.Effect(w.Entity.Name)
		if !rq.CanFail() {
			effects = append(effects, eff)
			continue
		}
		t := rq.Condition(w.Entity.Name) + " (else " + rq.ErrName + ")"
		if eff != "" {
			t += ", and " + eff
		}
		checks = append(checks, t)
	}
	var out string
	if len(checks) > 0 {
		out += " In the same transaction it requires " + strings.Join(checks, "; ") + "."
	}
	if len(effects) > 0 {
		out += " In the same transaction it " + strings.Join(effects, ", and ") + "."
	}
	return out
}
