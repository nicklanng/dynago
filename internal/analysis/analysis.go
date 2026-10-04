// Package analysis reasons about a schema as a design: how its items group into partitions, how
// big and busy each partition gets under the declared workload, what the design guarantees, and
// which choices are risky. Its findings have stable rule ids, so a schema can accept one with a
// reason and a policy can raise, lower or turn one off.
package analysis

import (
	"cmp"
	"fmt"
	"slices"
	"strings"

	"github.com/nicklanng/dynago/internal/cost"
	"github.com/nicklanng/dynago/internal/schema"
)

// Severity of a finding.
type Severity string

// Severities, most severe first.
const (
	Error   Severity = "error"
	Warning Severity = "warning"
	Note    Severity = "note"
)

// Rank orders severities: 0 is the most severe.
func (s Severity) Rank() int {
	switch s {
	case Error:
		return 0
	case Warning:
		return 1
	}
	return 2
}

// AtLeast reports whether s is at least as severe as o.
func (s Severity) AtLeast(o Severity) bool { return s.Rank() <= o.Rank() }

// Finding is a risk or design note about one schema object.
type Finding struct {
	Rule     string
	Severity Severity
	Subject  schema.Subject
	Message  string
	// Accepted is the schema's acceptance of the finding, or nil.
	Accepted *schema.Acceptance
}

// Result is the analysis of one schema.
type Result struct {
	Model  *schema.Model
	Cost   *cost.Report
	Policy *Policy
	// Partitions are the item collections the design creates, in the base table and each GSI.
	Partitions []*Partition
	// Findings are sorted most severe first; accepted ones are included, marked.
	Findings []Finding
	// Lifecycles are the state machines of enum fields that writes move between values.
	Lifecycles []*Lifecycle
	// Guarantees are the rules the generated code enforces, in words.
	Guarantees []Guarantee
	// Alternatives compares each index with the other strategy.
	Alternatives map[*schema.Index]*Alternative
	// Reads holds per-access statistics that need volumes: items and pages to read everything.
	Reads map[*schema.Access]*ReadStats
	// Assumptions lists what the partition and traffic estimates assume.
	Assumptions []string

	targets map[targetKey]*Member
}

// Open returns the findings not accepted by the schema.
func (r *Result) Open() []Finding {
	var out []Finding
	for _, f := range r.Findings {
		if f.Accepted == nil {
			out = append(out, f)
		}
	}
	return out
}

// Failing returns the open findings at or above the policy's failure level.
func (r *Result) Failing() []Finding {
	var out []Finding
	for _, f := range r.Open() {
		if f.Severity.AtLeast(r.Policy.FailOn) {
			out = append(out, f)
		}
	}
	return out
}

// Counts returns the number of open findings of each severity.
func (r *Result) Counts() map[Severity]int {
	out := map[Severity]int{}
	for _, f := range r.Open() {
		out[f.Severity]++
	}
	return out
}

// For returns the findings about a subject.
func (r *Result) For(s schema.Subject) []Finding {
	var out []Finding
	for _, f := range r.Findings {
		if f.Subject == s {
			out = append(out, f)
		}
	}
	return out
}

// Analyze analyses a schema. A nil policy is the default one.
func Analyze(m *schema.Model, prices cost.Prices, p *Policy) *Result {
	if p == nil {
		p = DefaultPolicy()
	}
	a := &analyzer{
		r: &Result{Model: m, Cost: cost.Analyze(m, prices), Policy: p,
			Alternatives: map[*schema.Index]*Alternative{}, Reads: map[*schema.Access]*ReadStats{},
			targets: map[targetKey]*Member{}},
		m: m, counterLoads: map[*schema.Counter]*counterLoad{},
	}
	a.assumptions()
	a.partitions()
	a.limitReads()
	a.traffic()
	a.readStats()
	a.lifecycles()
	a.guarantees()
	a.alternatives()
	a.rules()
	a.applyPolicy()
	a.accept()
	slices.SortStableFunc(a.r.Findings, func(x, y Finding) int {
		return cmp.Compare(x.Severity.Rank(), y.Severity.Rank())
	})
	return a.r
}

type analyzer struct {
	r *Result
	m *schema.Model
	// counterLoads sums the writes on each counter item, in the order counters were first written.
	counterLoads map[*schema.Counter]*counterLoad
	counterOrder []*schema.Counter
}

func (a *analyzer) add(rule string, sev Severity, s schema.Subject, format string, args ...any) {
	a.r.Findings = append(a.r.Findings, Finding{Rule: rule, Severity: sev, Subject: s, Message: fmt.Sprintf(format, args...)})
}

func (a *analyzer) assumptions() {
	m := a.m
	peak := "Peak traffic is assumed equal to the declared average rates (no workload.peak)."
	if m.Workload.PeakDeclared {
		peak = fmt.Sprintf("Peak traffic is %s× the declared average rates (workload.peak).", schema.Number(m.Workload.Peak))
	}
	horizon := "Volumes describe the table at no stated point in time (no workload.horizon)."
	if m.Workload.Horizon != "" {
		horizon = fmt.Sprintf("Volumes describe the table at %s (workload.horizon).", m.Workload.Horizon)
	}
	a.r.Assumptions = append(a.r.Assumptions, horizon, peak,
		"Items per partition follow the volumes: an entity's items per parent (typical and max), multiplied up the parents. A partition's largest count takes the biggest skew along its path, not every one at once.",
		"An enum in a partition key splits the items evenly among its values typically; at worst they all share one. A field that refers to another entity (by name, ref or requires) spreads the items evenly over that entity's items, unless volume.by says otherwise. Any other field leaves the count unknown.",
		"An index with a `where` holds the share of the items its `matches` declares. Without `matches`, every item is counted, so its sizes, traffic and write costs are upper bounds, marked as such. Empty key fields only make an index smaller.",
		"The busiest partition gets traffic in proportion to its share of the items: its largest count over the entity's total. A write's declared hot_key_rate replaces that estimate for every partition it touches.",
		fmt.Sprintf("A partition key value takes at most %d WRU and %d RRU per second. Risk is the larger share of either at peak: low under 10%%, medium under 50%%, high above.", cost.PartitionWCU, cost.PartitionRCU),
	)
}

// Rule describes a kind of finding.
type Rule struct {
	ID string
	// Severity is the default: the most severe level the rule reports at.
	Severity Severity
	// Hard rules report DynamoDB limits: they can't be accepted, lowered or turned off.
	Hard bool
	// Policy rules only report when the policy asks for them.
	Policy  bool
	Summary string
}

// Rules is every rule the analysis reports, in documentation order.
var Rules = []Rule{
	{"item-too-large", Error, true, false, "An item's p99 size is over DynamoDB's 400 KB limit."},
	{"transaction-too-many-items", Error, true, false, "A write can touch more than 100 items, DynamoDB's transaction limit."},
	{"transaction-too-large", Error, true, false, "A write's transaction can exceed DynamoDB's 4 MB limit."},
	{"accept-invalid", Error, true, false, "An acceptance names an unknown rule, an error, or a finding the schema doesn't have."},
	{"hot-partition", Error, false, false, "A partition key takes a large share of its throughput at peak: over 50% is a warning, over 100% of a single item is an error."},
	{"hot-counter", Error, false, false, "A counter item takes more writes than a partition can serve."},
	{"counter-contention", Warning, false, false, "Transactions update one counter item often enough to conflict and retry."},
	{"low-cardinality-key", Warning, false, false, "A partition key holds nothing but constants, enums and bools, so all of an entity's items share a few partitions. A note when the declared volumes and rates keep those partitions small and quiet."},
	{"sparse-index", Warning, false, false, "An index is keyed by an optional field, so items without it silently drop out of the index and its reads."},
	{"unenforced-unique", Warning, false, false, "A lookup reads one entry of an index that nothing keeps unique."},
	{"unused-index", Warning, false, false, "No declared read uses an index, yet every write pays for it."},
	{"copy-drift", Warning, false, false, "A `copy_of` field must be rewritten by your code whenever its source changes."},
	{"item-large", Warning, false, false, "An item's p99 size is over 100 KB, so every read and write of it is expensive."},
	{"large-field", Warning, false, false, "A large field makes writes that never change it pay for it."},
	{"ttl-counter", Warning, false, false, "An expiring entity feeds counters, which TTL deletion never decrements."},
	{"ttl-claim", Warning, false, false, "An expiring entity holds unique claims, which TTL deletion never releases."},
	{"ttl-copy", Warning, false, false, "An expiring entity has copies, which outlive it."},
	{"copy-not-needed", Note, false, false, "A copy index costs transactions on every write, but no read through it needs immediate freshness."},
	{"scan", Note, false, false, "A declared scan reads the whole table."},
	{"project-all", Note, false, false, "A GSI projects every attribute, storing and writing each item twice."},
	{"shared-gsi", Note, false, false, "Several entities share one GSI."},
	{"volume-missing", Warning, false, true, "An entity declares no volume (policy require.volumes)."},
	{"rate-missing", Warning, false, true, "A read or write declares no rate (policy require.rates)."},
	{"freshness-unstated", Warning, false, true, "A read doesn't state the freshness it needs (policy require.freshness)."},
	{"item-size-limit", Warning, false, true, "An item's p99 size exceeds the policy's limits.item_size."},
	{"partition-size-limit", Warning, false, true, "A partition's largest size exceeds the policy's limits.partition_size."},
	{"transaction-items-limit", Warning, false, true, "A write touches more items than the policy's limits.transaction_items."},
	{"gsi-limit", Warning, false, true, "The table has more GSIs than the policy's limits.gsis."},
	{"index-limit", Warning, false, true, "An entity has more indexes than the policy's limits.indexes_per_entity."},
}

// RuleByID returns a rule, or nil.
func RuleByID(id string) *Rule {
	for i := range Rules {
		if Rules[i].ID == id {
			return &Rules[i]
		}
	}
	return nil
}

// applyPolicy adjusts severities and drops findings of rules turned off.
func (a *analyzer) applyPolicy() {
	var out []Finding
	for _, f := range a.r.Findings {
		rule := RuleByID(f.Rule)
		if rule != nil && !rule.Hard {
			switch sev := a.r.Policy.Rules[f.Rule]; sev {
			case "":
			case "off":
				continue
			default:
				f.Severity = Severity(sev)
			}
		}
		out = append(out, f)
	}
	a.r.Findings = out
}

// accept matches the schema's acceptances with findings. An acceptance must name a known rule,
// match a finding of its subject, and not accept an error: errors are fixed, not accepted.
//
// An acceptance on an entity also covers the rule's findings about what the entity declares (its
// fields, indexes, constraints, counters, reads and writes), so one reason that holds for all of
// them is given once. One on the object itself takes precedence.
func (a *analyzer) accept() {
	// Entity-wide acceptances first, so that a closer one replaces them on its own finding.
	accs := slices.Clone(a.m.Accepts)
	slices.SortStableFunc(accs, func(x, y *schema.Acceptance) int {
		return cmp.Compare(b2i(x.Subject.Kind != schema.SubjectEntity), b2i(y.Subject.Kind != schema.SubjectEntity))
	})
	for _, acc := range accs {
		where := fmt.Sprintf("%s accept %s", subjectText(acc.Subject), acc.Rule)
		rule := RuleByID(acc.Rule)
		if rule == nil {
			a.add("accept-invalid", Error, acc.Subject, "%s: there is no rule %q. Rules: %s", where, acc.Rule, ruleList())
			continue
		}
		if a.r.Policy.Rules[acc.Rule] == "off" {
			continue // the policy turned the rule off: nothing to accept
		}
		matched, isError := false, false
		for i := range a.r.Findings {
			f := &a.r.Findings[i]
			if f.Rule != acc.Rule || !covers(acc.Subject, f.Subject) {
				continue
			}
			matched = true
			if f.Severity == Error {
				isError = true
				continue
			}
			f.Accepted = acc
		}
		switch {
		case isError:
			a.add("accept-invalid", Error, acc.Subject, "%s: this finding is an error, which can't be accepted: fix the design, or the assumption behind the estimate", where)
		case !matched && acc.Subject.Kind == schema.SubjectEntity:
			a.add("accept-invalid", Error, acc.Subject, "%s: neither %s nor anything it declares has a %s finding to accept; remove the acceptance", where, subjectText(acc.Subject), acc.Rule)
		case !matched:
			a.add("accept-invalid", Error, acc.Subject, "%s: %s has no %s finding to accept; remove the acceptance", where, subjectText(acc.Subject), acc.Rule)
		}
	}
}

// covers reports whether an acceptance on subject acc applies to a finding about f: the same
// object, or for an entity, anything declared on it.
func covers(acc, f schema.Subject) bool {
	if acc == f {
		return true
	}
	return acc.Kind == schema.SubjectEntity && f.Kind != schema.SubjectTable && f.Entity == acc.Entity
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

func ruleList() string {
	var ids []string
	for _, r := range Rules {
		if !r.Hard {
			ids = append(ids, r.ID)
		}
	}
	return strings.Join(ids, ", ")
}

// subjectText names a subject with its kind: "index Loan.Overdue", "entity Loan", "the table".
func subjectText(s schema.Subject) string {
	if s.Kind == schema.SubjectTable {
		return "the table"
	}
	return string(s.Kind) + " " + s.String()
}

// SubjectText names a subject with its kind: "index Loan.Overdue".
func SubjectText(s schema.Subject) string { return subjectText(s) }

// subject helpers

func entitySubject(e *schema.Entity) schema.Subject {
	return schema.Subject{Kind: schema.SubjectEntity, Entity: e.Name}
}

func fieldSubject(e *schema.Entity, f *schema.Field) schema.Subject {
	return schema.Subject{Kind: schema.SubjectField, Entity: e.Name, Name: f.Name}
}

func indexSubject(ix *schema.Index) schema.Subject {
	return schema.Subject{Kind: schema.SubjectIndex, Entity: ix.Entity.Name, Name: ix.Name}
}

func counterSubject(c *schema.Counter) schema.Subject {
	return schema.Subject{Kind: schema.SubjectCounter, Entity: c.Entity.Name, Name: c.Name}
}

func accessSubject(ac *schema.Access) schema.Subject {
	return schema.Subject{Kind: schema.SubjectAccess, Entity: ac.Entity.Name, Name: ac.Name}
}

func writeSubject(w *schema.Write) schema.Subject {
	return schema.Subject{Kind: schema.SubjectWrite, Entity: w.Entity.Name, Name: w.Name}
}

func uniqueSubject(u *schema.Unique) schema.Subject {
	return schema.Subject{Kind: schema.SubjectUnique, Entity: u.Entity.Name, Name: u.Name}
}
