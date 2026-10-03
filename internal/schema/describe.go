package schema

import (
	"fmt"
	"strconv"
	"strings"
)

// PredText renders predicates for people: `status = "active" and memberId = Loan.memberId`,
// `mailbox != "trash"`, `mailbox in ["inbox", "archived"]`. source names the entity whose fields
// references point at.
func PredText(ps []*Pred, source string) string {
	var parts []string
	for _, p := range ps {
		parts = append(parts, p.Text(source))
	}
	return strings.Join(parts, " and ")
}

// Text renders one predicate for people.
func (p *Pred) Text(source string) string {
	if p.In != nil {
		vs := make([]string, len(p.In))
		for i, v := range p.In {
			vs[i] = ValueText(v)
		}
		return p.Field.Name + " in [" + strings.Join(vs, ", ") + "]"
	}
	v := ValueText(p.Value)
	if p.Source != nil {
		v = source + "." + p.Source.Name
	}
	if p.Not {
		return p.Field.Name + " != " + v
	}
	return p.Field.Name + " = " + v
}

// SetText renders field assignments for people: `status to "onLoan"`.
func SetText(sets []SetConst, source string) string {
	var parts []string
	for _, s := range sets {
		if !s.Add {
			parts = append(parts, s.Field.Name+" to "+s.valueText(source))
		}
	}
	return strings.Join(parts, " and ")
}

func (s SetConst) valueText(source string) string {
	if s.Source == nil {
		return ValueText(s.Value)
	}
	v := source + "." + s.Source.Name
	if s.IfSet {
		v += " (if that has a value)"
	}
	return v
}

// ChangeText renders everything a requirement changes on its item, completing "it ...":
// `sets its status to "onLoan", and adds 1 to its loans`.
func ChangeText(sets []SetConst, source string) string {
	var parts []string
	if t := SetText(sets, source); t != "" {
		parts = append(parts, "sets its "+t)
	}
	for _, s := range sets {
		if s.Add {
			parts = append(parts, "adds "+s.valueText(source)+" to its "+s.Field.Name)
		}
	}
	return strings.Join(parts, ", and ")
}

// ValueText renders a constant as it would appear in the schema.
func ValueText(v any) string {
	if s, ok := v.(string); ok {
		return strconv.Quote(s)
	}
	return fmt.Sprint(v)
}

// Condition says what the requirement needs of its item, completing "requires ...".
func (rq *Require) Condition(source string) string {
	switch {
	case rq.Counter != nil:
		var parts []string
		for _, p := range rq.CounterWhen {
			parts = append(parts, fmt.Sprintf("%s = %d", p.Value.Name, p.Equals))
		}
		return fmt.Sprintf("counter %s to have %s (a missing value counts as 0)", rq.Counter.Name, strings.Join(parts, " and "))
	case rq.Optional && len(rq.When) > 0:
		return fmt.Sprintf("any %s to have %s (an absent or expired one passes)", rq.Name, PredText(rq.When, source))
	case rq.Optional:
		return fmt.Sprintf("nothing of any %s", rq.Name)
	case len(rq.When) > 0:
		return fmt.Sprintf("the %s to exist with %s", rq.Name, PredText(rq.When, source))
	}
	return fmt.Sprintf("the %s to exist", rq.Name)
}

// Effect says what the write does to the required item, or "" if it only checks it.
func (rq *Require) Effect(source string) string {
	switch {
	case len(rq.Sets) > 0:
		return ChangeText(rq.Sets, source)
	case rq.Consume && rq.Optional:
		return "deletes the " + rq.Name + " if there is one"
	case rq.Consume:
		return "deletes the " + rq.Name
	}
	return ""
}

// CanFail reports whether the requirement can reject a write (and so has an error).
func (rq *Require) CanFail() bool {
	return rq.Counter != nil || !rq.Optional || len(rq.When) > 0
}
