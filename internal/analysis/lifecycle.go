package analysis

import (
	"fmt"
	"slices"
	"strings"

	"github.com/nicklanng/dynago/internal/schema"
)

// Any stands for "any value" in a transition: a write that doesn't check the current value, or
// that sets a value the caller chooses.
const Any = "*"

// Lifecycle is the state machine of an enum field: the values writes move it between.
type Lifecycle struct {
	Entity      *schema.Entity
	Field       *schema.Field
	Transitions []Transition
	// Final lists values no write moves out of.
	Final []string
	// Deleted lists the writes that delete the item, from any value.
	Deleted []string
}

// Transition is one way a write changes (or creates) the field's value.
type Transition struct {
	// From is the value required before, Any, or "" for a create.
	From string
	// To is the value after, or Any when the caller chooses it.
	To string
	// By names the write: "Retire", or "Loan.Borrow" for another entity's write.
	By string
}

// lifecycles derives the state machines of enum fields that updates change: from creates (which
// set a value or let the caller choose), updates (`set` with `when`, or a value the caller
// supplies) and other entities' requires that set the field.
func (a *analyzer) lifecycles() {
	for _, e := range a.m.Entities {
		for _, f := range e.Fields {
			if f.Type != schema.TypeEnum {
				continue
			}
			lc := &Lifecycle{Entity: e, Field: f}
			changes := false
			for _, w := range e.Writes {
				switch w.Kind {
				case schema.WriteCreate:
					to := Any
					if v, ok := setValue(w.Sets, f); ok {
						to = v
					}
					lc.Transitions = append(lc.Transitions, Transition{To: to, By: w.Name})
				case schema.WriteDelete:
					lc.Deleted = append(lc.Deleted, w.Name)
				case schema.WriteUpdate:
					if ts := transitions(w.Sets, w.When, f, w.Name); len(ts) > 0 {
						lc.Transitions = append(lc.Transitions, ts...)
						changes = true
					}
					if slices.Contains(w.Args, f) || slices.Contains(w.Patch, f) {
						for _, from := range whenValues(w.When, f) {
							lc.Transitions = append(lc.Transitions, Transition{From: from, To: Any, By: w.Name})
						}
						changes = true
					}
				}
			}
			for _, oe := range a.m.Entities {
				for _, w := range oe.Writes {
					for _, rq := range w.Requires {
						if rq.Target != e {
							continue
						}
						if rq.Consume {
							lc.Deleted = append(lc.Deleted, oe.Name+"."+w.Name)
							continue
						}
						if rq.Ensure {
							// It may create the item: with the value it gives, or the caller's.
							to := Any
							if v, ok := setValue(rq.TargetCreate().Sets, f); ok {
								to = v
							}
							lc.Transitions = append(lc.Transitions, Transition{To: to, By: oe.Name + "." + w.Name})
						}
						if ts := transitions(rq.Sets, rq.When, f, oe.Name+"."+w.Name); len(ts) > 0 {
							lc.Transitions = append(lc.Transitions, ts...)
							changes = true
						}
					}
				}
			}
			if !changes {
				continue
			}
			for _, v := range f.Enum {
				leaves, reached := false, false
				for _, t := range lc.Transitions {
					leaves = leaves || (t.From != "" && (t.From == v || t.From == Any) && t.To != v)
					reached = reached || t.To == v || t.To == Any
				}
				if reached && !leaves {
					lc.Final = append(lc.Final, v)
				}
			}
			a.r.Lifecycles = append(a.r.Lifecycles, lc)
		}
	}
}

// transitions returns the moves a write's set makes to the field: one from each value its when
// allows.
func transitions(sets []schema.SetConst, when []*schema.Pred, f *schema.Field, by string) []Transition {
	for _, s := range sets {
		if s.Field != f {
			continue
		}
		to := Any
		if s.Source == nil {
			to = fmt.Sprint(s.Value)
		}
		var out []Transition
		for _, from := range whenValues(when, f) {
			out = append(out, Transition{From: from, To: to, By: by})
		}
		return out
	}
	return nil
}

func setValue(sets []schema.SetConst, f *schema.Field) (string, bool) {
	for _, s := range sets {
		if s.Field == f && s.Source == nil {
			return fmt.Sprint(s.Value), true
		}
	}
	return "", false
}

// whenValues returns the values of the field a write's when allows it to start from: the one it
// pins, those `in` lists or `not` leaves, or Any when it doesn't constrain the field.
func whenValues(when []*schema.Pred, f *schema.Field) []string {
	for _, p := range when {
		if p.Field != f {
			continue
		}
		if vs, ok := p.Allowed(); ok && len(vs) > 0 {
			return vs
		}
	}
	return []string{Any}
}

// Guarantee is a rule the generated code enforces, in words.
type Guarantee struct {
	Entity *schema.Entity
	// Kind: unique, bound, required, requires.
	Kind string
	Text string
	// By names the mechanism: "claim Email", "counter MemberLoans", "Loan.Borrow".
	By string
}

// guarantees states what the schema's claims, counter bounds, required fields and requires
// enforce, entity by entity.
func (a *analyzer) guarantees() {
	for _, e := range a.m.Entities {
		for _, u := range e.Uniques {
			a.r.Guarantees = append(a.r.Guarantees, Guarantee{e, "unique", uniqueText(u), "claim " + u.Name})
		}
		for _, c := range e.Counters {
			for _, v := range c.Values {
				what := counted(e, c, v)
				switch {
				case v.LimitArg:
					a.r.Guarantees = append(a.r.Guarantees, Guarantee{e, "bound", capital(what) + " never exceeds the limit its writes are given" + limitWrites(e, v) + ".", "counter " + c.Name})
				case v.Limit > 0:
					a.r.Guarantees = append(a.r.Guarantees, Guarantee{e, "bound", fmt.Sprintf("%s never exceeds %d.", capital(what), v.Limit), "counter " + c.Name})
				}
				if v.HasMin {
					a.r.Guarantees = append(a.r.Guarantees, Guarantee{e, "bound", fmt.Sprintf("%s never goes below %d.", capital(what), v.Min), "counter " + c.Name})
				}
			}
		}
		var req []string
		for _, f := range e.Fields {
			if f.Required && !f.Key {
				req = append(req, f.Name)
			}
		}
		if len(req) > 0 {
			a.r.Guarantees = append(a.r.Guarantees, Guarantee{e, "required", fmt.Sprintf("Every %s has %s.", e.Name, join(req)), "required fields"})
		}
		for _, w := range e.Writes {
			var parts []string
			for _, rq := range w.Requires {
				// One that checks nothing says only what it does.
				parts = append(parts, rq.Sentence(e.Name, ", and "))
			}
			if len(parts) > 0 {
				a.r.Guarantees = append(a.r.Guarantees, Guarantee{e, "requires", fmt.Sprintf("%s %s, in one transaction.", w.Name, strings.Join(parts, "; ")), e.Name + "." + w.Name})
			}
		}
	}
}

// uniqueText says what a claim keeps unique: "No two Members have the same libraryId and email
// (ignoring case)."
func uniqueText(u *schema.Unique) string {
	e := u.Entity
	lowered := map[string]bool{}
	for _, t := range []schema.Template{u.PK, u.SK} {
		for _, sg := range t.Segments {
			if sg.Transform == "lower" {
				lowered[sg.Field] = true
			}
		}
	}
	var fs []string
	for _, f := range u.Fields {
		if f == u.Set {
			continue
		}
		n := f.Name
		if lowered[f.Name] {
			n += " (ignoring case)"
		}
		fs = append(fs, n)
	}
	if u.Set != nil {
		scope := ""
		if len(fs) > 0 {
			scope = " with the same " + join(fs)
		}
		return fmt.Sprintf("No element of %s appears in two %s%s.", u.Set.Name, schema.Plural(e.Name), scope)
	}
	return fmt.Sprintf("No two %s have the same %s.", schema.Plural(e.Name), join(fs))
}

// counted describes a counter value: "the count of Loans with status active per libraryId and
// memberId".
func counted(e *schema.Entity, c *schema.Counter, v *schema.CounterValue) string {
	what := fmt.Sprintf("the number of %s", schema.Plural(e.Name))
	if v.Sum != nil {
		what = fmt.Sprintf("the sum of %s over %s", v.Sum.Name, schema.Plural(e.Name))
	}
	if len(v.Where) > 0 {
		what += " with " + schema.PredText(v.Where, e.Name)
	}
	var keys []string
	for _, f := range c.KeyFields() {
		keys = append(keys, f.Name)
	}
	if len(keys) > 0 {
		what += " per " + join(keys)
	}
	return what
}

func limitWrites(e *schema.Entity, v *schema.CounterValue) string {
	var ws []string
	for _, w := range e.Writes {
		if slices.Contains(w.Limits, v) {
			ws = append(ws, w.Name)
		}
	}
	if len(ws) == 0 {
		return ""
	}
	return " (" + join(ws) + ")"
}

func capital(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
