// Package arch captures a schema's analysed design as a plain snapshot (what `dynago check
// -json` prints), and compares two snapshots as an architecture diff for review.
package arch

import (
	"fmt"
	"strings"

	"github.com/nicklanng/dynago/internal/analysis"
	"github.com/nicklanng/dynago/internal/cost"
	"github.com/nicklanng/dynago/internal/lock"
	"github.com/nicklanng/dynago/internal/schema"
)

// Snapshot is a schema's design as reviewers see it: entities, reads, writes, guarantees,
// partitions, findings and costs.
type Snapshot struct {
	Table        string      `json:"table"`
	Generation   int         `json:"generation"`
	Workload     Workload    `json:"workload"`
	Entities     []Entity    `json:"entities"`
	Guarantees   []string    `json:"guarantees"`
	Lifecycles   []string    `json:"lifecycles"`
	Partitions   []Partition `json:"partitions"`
	Findings     []Finding   `json:"findings"`
	StorageBytes float64     `json:"storageBytes"`
	// BaseBytes is the base table without GSIs: what a scan, or a migration pass, reads.
	BaseBytes  float64 `json:"baseBytes"`
	MonthlyUSD float64 `json:"monthlyUSD"`
}

// Workload is the table-wide workload assumptions.
type Workload struct {
	Peak    float64 `json:"peak"`
	Horizon string  `json:"horizon,omitempty"`
}

// Entity is one entity's design.
type Entity struct {
	Name     string     `json:"name"`
	Version  int        `json:"version"`
	PK       string     `json:"pk"`
	SK       string     `json:"sk"`
	Volume   string     `json:"volume"`
	Count    float64    `json:"count,omitempty"`
	Fields   []Field    `json:"fields"`
	Indexes  []Index    `json:"indexes,omitempty"`
	Uniques  []Unique   `json:"uniques,omitempty"`
	Counters []Counter  `json:"counters,omitempty"`
	Reads    []Read     `json:"reads,omitempty"`
	Writes   []Write    `json:"writes,omitempty"`
	Shape    lock.Shape `json:"shape"`
	// ItemBytes is the p50 item size.
	ItemBytes    int     `json:"itemBytes"`
	StorageBytes float64 `json:"storageBytes"`
	// MigrateWRU is what the migration job's copy of one item costs: the item with its copies,
	// claims and counters in one transaction with the job's fence check, plus its GSI entries.
	MigrateWRU float64 `json:"migrateWRU"`
}

// Field is one field.
type Field struct {
	Name     string `json:"name"`
	Type     string `json:"type"`
	Required bool   `json:"required,omitempty"`
	// Copy is "copy_of E.f" or "snapshot_of E.f", or "".
	Copy string `json:"copy,omitempty"`
	Ref  string `json:"ref,omitempty"`
}

// Index is one index.
type Index struct {
	Name       string `json:"name"`
	Strategy   string `json:"strategy"`
	Why        string `json:"why,omitempty"`
	PK         string `json:"pk"`
	SK         string `json:"sk,omitempty"`
	Projection string `json:"projection"`
	Where      string `json:"where,omitempty"`
	// Matches is the declared share of items its where matches (0: not declared).
	Matches float64  `json:"matches,omitempty"`
	ReadBy  []string `json:"readBy,omitempty"`
}

// Unique is one uniqueness constraint.
type Unique struct {
	Name   string   `json:"name"`
	Fields []string `json:"fields"`
}

// Counter is one counter.
type Counter struct {
	Name   string   `json:"name"`
	PK     string   `json:"pk"`
	SK     string   `json:"sk"`
	Shards int      `json:"shards"`
	Values []string `json:"values"`
}

// Read is one declared read.
type Read struct {
	Name      string  `json:"name"`
	ServedBy  string  `json:"servedBy"`
	Freshness string  `json:"freshness"`
	RRU       float64 `json:"rru"`
	RRUP99    float64 `json:"rruP99"`
	Rate      float64 `json:"rate,omitempty"`
}

// Write is one declared write.
type Write struct {
	Name string `json:"name"`
	Kind string `json:"kind"`
	// Does says what the write changes and checks, in the schema's terms.
	Does      string   `json:"does"`
	Items     []string `json:"items"`
	TxItems   int      `json:"txItems"`
	ReadFirst bool     `json:"readFirst"`
	WRU       float64  `json:"wru"`
	WRUP99    float64  `json:"wruP99"`
	Rate      float64  `json:"rate,omitempty"`
}

// Partition is one partition family.
type Partition struct {
	Space    string   `json:"space"`
	PK       string   `json:"pk"`
	Holds    []string `json:"holds"`
	Size     float64  `json:"size,omitempty"`
	MaxSize  float64  `json:"maxSize,omitempty"`
	Grows    bool     `json:"grows,omitempty"`
	Risk     string   `json:"risk,omitempty"`
	Headroom float64  `json:"headroom,omitempty"`
}

// Finding is one finding.
type Finding struct {
	Rule     string `json:"rule"`
	Severity string `json:"severity"`
	Subject  string `json:"subject"`
	Message  string `json:"message"`
	Accepted string `json:"accepted,omitempty"`
	// Nth numbers findings of the same rule about the same subject (a write touching two hot
	// counters has two), so each is compared with its counterpart.
	Nth int `json:"nth,omitempty"`
}

// Key identifies a finding across snapshots.
func (f Finding) Key() string { return fmt.Sprintf("%s\x00%s\x00%d", f.Rule, f.Subject, f.Nth) }

// Build snapshots an analysed schema.
func Build(r *analysis.Result) *Snapshot {
	m := r.Model
	s := &Snapshot{Table: m.Table.Name, Generation: m.Table.Generation, Workload: Workload{Peak: m.Workload.Peak, Horizon: m.Workload.Horizon},
		StorageBytes: r.Cost.StorageBytes, BaseBytes: r.Cost.BaseBytes, MonthlyUSD: r.Cost.MonthlyUSD}
	for _, e := range m.Entities {
		er := r.Cost.Entity(e)
		se := Entity{Name: e.Name, Version: e.Version, PK: e.PK.Raw, SK: e.SK.Raw, Volume: e.Volume.String(), Count: e.Count,
			Shape: lock.ShapeOf(e, m.Table.TTLAttr), ItemBytes: er.Item.P50, StorageBytes: er.StorageBytes,
			MigrateWRU: migrateWRU(m, e, er)}
		for _, f := range e.Fields {
			sf := Field{Name: f.Name, Type: string(f.Type), Required: f.Required}
			if f.Type == schema.TypeEnum {
				sf.Type += "(" + strings.Join(f.Enum, "|") + ")"
			}
			switch {
			case f.CopyOf != nil:
				sf.Copy = "copy_of " + f.CopyOfEntity.Name + "." + f.CopyOf.Name
			case f.SnapshotOf != nil:
				sf.Copy = "snapshot_of " + f.SnapshotOfEntity.Name + "." + f.SnapshotOf.Name
			}
			if f.Ref != nil {
				sf.Ref = f.Ref.Name
			}
			se.Fields = append(se.Fields, sf)
		}
		for _, ix := range e.Indexes {
			si := Index{Name: ix.Name, Strategy: string(ix.Strategy), PK: ix.PK.Raw, Projection: projection(ix)}
			if ix.StrategyInferred {
				si.Why = ix.StrategyReason
			}
			if ix.HasSK {
				si.SK = ix.SK.Raw
			}
			if len(ix.Where) > 0 {
				si.Where = schema.PredText(ix.Where, e.Name)
				si.Matches = ix.Matches
			}
			for _, a := range e.Access {
				if a.Index == ix {
					si.ReadBy = append(si.ReadBy, a.Name)
				}
			}
			se.Indexes = append(se.Indexes, si)
		}
		for _, u := range e.Uniques {
			var fs []string
			for _, f := range u.Fields {
				fs = append(fs, f.Name)
			}
			se.Uniques = append(se.Uniques, Unique{Name: u.Name, Fields: fs})
		}
		for _, c := range e.Counters {
			sc := Counter{Name: c.Name, PK: c.PK.Raw, SK: c.SK.Raw, Shards: c.Shards}
			for _, v := range c.Values {
				sc.Values = append(sc.Values, counterValue(v))
			}
			se.Counters = append(se.Counters, sc)
		}
		for _, rc := range er.Reads {
			a := rc.Access
			se.Reads = append(se.Reads, Read{Name: a.Name, ServedBy: servedBy(a), Freshness: freshness(a), RRU: rc.RRU.P50, RRUP99: rc.RRU.P99, Rate: a.Rate})
		}
		for _, wc := range er.Writes {
			w := wc.Write
			tx := 0
			if wc.Transactional {
				tx = wc.MaxTxItems
			}
			se.Writes = append(se.Writes, Write{Name: w.Name, Kind: string(w.Kind), Does: does(w), Items: wc.Items, TxItems: tx, ReadFirst: wc.ReadFirst, WRU: wc.WRU.P50, WRUP99: wc.WRU.P99, Rate: w.Rate})
		}
		s.Entities = append(s.Entities, se)
	}
	for _, g := range r.Guarantees {
		s.Guarantees = append(s.Guarantees, g.Entity.Name+": "+g.Text)
	}
	for _, lc := range r.Lifecycles {
		for _, t := range lc.Transitions {
			from := t.From
			if from == "" {
				from = "(new)"
			}
			s.Lifecycles = append(s.Lifecycles, fmt.Sprintf("%s.%s: %s → %s by %s", lc.Entity.Name, lc.Field.Name, from, t.To, t.By))
		}
	}
	for _, p := range r.Partitions {
		sp := Partition{Space: p.Space(), PK: p.PK, Grows: p.Grows, Risk: string(p.Risk), Headroom: p.Headroom}
		if p.Size.Known {
			sp.Size = p.Size.Typical
		}
		if p.Size.MaxKnown {
			sp.MaxSize = p.Size.Max
		}
		for _, mb := range p.Members {
			sp.Holds = append(sp.Holds, mb.Label)
		}
		s.Partitions = append(s.Partitions, sp)
	}
	nth := map[string]int{}
	for _, f := range r.Findings {
		sf := Finding{Rule: f.Rule, Severity: string(f.Severity), Subject: analysis.SubjectText(f.Subject), Message: f.Message}
		k := sf.Rule + "\x00" + sf.Subject
		sf.Nth = nth[k]
		nth[k]++
		if f.Accepted != nil {
			sf.Accepted = f.Accepted.Reason
		}
		s.Findings = append(s.Findings, sf)
	}
	return s
}

// migrateWRU costs the migration job's copy of one item: a create of it (with its claims, copies and
// counters) in one transaction with the fence check, plus its GSI entries.
func migrateWRU(m *schema.Model, e *schema.Entity, er *cost.EntityReport) float64 {
	wc := cost.WriteCostOf(m, e, &schema.Write{Name: "migrate", Entity: e, Kind: schema.WriteCreate}, false, er)
	var own, async float64
	for _, t := range wc.Touches {
		if t.Async {
			async += t.Units.P50
		} else {
			own += t.Units.P50
		}
	}
	return 2*(own+1) + async
}

func projection(ix *schema.Index) string {
	switch ix.Projection {
	case schema.ProjectAll:
		return "all"
	case schema.ProjectKeys:
		return "keys"
	}
	var fs []string
	for _, f := range ix.Project {
		fs = append(fs, f.Name)
	}
	return strings.Join(fs, ", ")
}

func counterValue(v *schema.CounterValue) string {
	s := v.Name + ": count"
	if v.Sum != nil {
		s = v.Name + ": sum of " + v.Sum.Name
	}
	if len(v.Where) > 0 {
		s += " where " + schema.PredText(v.Where, "")
	}
	switch {
	case v.LimitArg:
		s += ", limit from caller"
	case v.Limit > 0:
		s += fmt.Sprintf(", limit %d", v.Limit)
	}
	if v.HasMin {
		s += fmt.Sprintf(", min %d", v.Min)
	}
	return s
}

func servedBy(a *schema.Access) string {
	switch {
	case a.Of != nil:
		return "Query of the whole partition"
	case a.Batch > 0:
		return "BatchGetItem"
	case a.All:
		return "Query of counter " + a.Counter.Name + "'s items"
	}
	switch a.Kind {
	case schema.AccessGet:
		return "GetItem"
	case schema.AccessGetUnique:
		return "claim " + a.Unique.Name + ", then GetItem"
	case schema.AccessCounter:
		return "counter " + a.Counter.Name
	case schema.AccessScan:
		return "Scan"
	}
	switch {
	case a.Index == nil:
		return "Query of the entity's partition"
	case a.Index.Strategy == schema.StrategyCopy:
		return "Query of copy " + a.Index.Name
	}
	return "Query of GSI " + a.Index.Name
}

func freshness(a *schema.Access) string {
	if a.Freshness != schema.FreshnessUnstated {
		return string(a.Freshness)
	}
	if a.Consistent || a.Kind == schema.AccessGetUnique {
		return "strongly consistent (freshness not stated)"
	}
	return "eventually consistent (freshness not stated)"
}

// does describes a write in the schema's terms: "create; sets status = \"available\"; requires …".
func does(w *schema.Write) string {
	parts := []string{string(w.Kind)}
	var sets []string
	for _, f := range w.Args {
		sets = append(sets, f.Name)
	}
	for _, f := range w.Patch {
		sets = append(sets, f.Name+" if given")
	}
	for _, st := range w.Sets {
		sets = append(sets, st.Field.Name+" = "+schema.ValueText(st.Value))
	}
	if len(sets) > 0 {
		parts = append(parts, "sets "+strings.Join(sets, ", "))
	}
	if len(w.When) > 0 {
		parts = append(parts, "when "+schema.PredText(w.When, w.Entity.Name))
	}
	for _, rq := range w.Requires {
		t := "requires " + rq.Condition(w.Entity.Name)
		if eff := rq.Effect(w.Entity.Name); eff != "" {
			t += " and " + eff
		}
		parts = append(parts, t)
	}
	if w.VersionRequired {
		parts = append(parts, "version required")
	}
	return strings.Join(parts, "; ")
}
