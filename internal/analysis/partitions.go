package analysis

import (
	"fmt"
	"math"
	"slices"
	"strings"

	"github.com/nicklanng/dynago/internal/cost"
	"github.com/nicklanng/dynago/internal/schema"
)

// Estimate is an approximate number: typical, and at most Max when MaxKnown.
type Estimate struct {
	Typical  float64
	Max      float64
	Known    bool
	MaxKnown bool
}

func exactly(n float64) Estimate { return Estimate{n, n, true, true} }

// Risk is how close a partition comes to DynamoDB's per-partition throughput at peak.
type Risk string

// Risks.
const (
	RiskUnknown Risk = ""
	RiskLow     Risk = "low"
	RiskMedium  Risk = "medium"
	RiskHigh    Risk = "high"
)

// Member is one family of items in a partition: an entity's items, index entries, copies, claims
// or a counter's items.
type Member struct {
	Target cost.Target
	Label  string
	// Count is how many of these items one partition holds.
	Count Estimate
	// Weight is how many of the entity's items feed one partition's items: the count for items
	// and index entries, the items counted for a counter. It decides a partition's share of traffic.
	Weight Estimate
	Size   cost.Size
	// Grows is true when nothing ever removes these items: the partition grows as long as the
	// table lives.
	Grows bool
	// Owner is the schema object that declares the items, for findings.
	Owner     schema.Subject
	Partition *Partition
}

// Partition is a family of partitions: every partition key value one key template renders, in
// the base table or a GSI.
type Partition struct {
	// Index is the GSI the partitions live in, or "" for the base table.
	Index string
	// PK is the partition key template ("LIB#{libraryId}"); sharded counters add "#S{0..n}".
	PK      string
	Fields  []string
	Shards  int
	Members []*Member
	// Count is the number of partition key values.
	Count Estimate
	// Size is one partition's size in bytes: typical, and the largest.
	Size  Estimate
	Grows bool
	// PeakWRU and PeakRRU are the busiest partition key's capacity use per second at peak.
	PeakWRU, PeakRRU float64
	// Rated is true when some declared rate reaches the partition.
	Rated    bool
	Headroom float64
	Risk     Risk
	// SingleItem is true when every partition holds one item, which DynamoDB can't split.
	SingleItem bool
	// Undeclared lists partition key fields whose cardinality nothing declares, which leave the
	// partition's counts unknown.
	Undeclared []string
	// busiest is the member taking the most write capacity at the busiest key.
	busiestWRU map[*Member]float64
}

// Space names where the partition lives: "base table" or "GSI Name".
func (p *Partition) Space() string {
	if p.Index == "" {
		return "base table"
	}
	return "GSI " + p.Index
}

// ReadStats describes one read against the declared volumes.
type ReadStats struct {
	// Items is how many items the read's partition holds (what reading everything returns).
	Items Estimate
	// Pages is how many calls reading everything takes at the default page size.
	Pages Estimate
	// Evaluated is, for a scan, every item in the base table, which its pages read through.
	Evaluated Estimate
	// Filtered is true when the index has a where, so fewer items qualify than counted.
	Filtered  bool
	Partition *Partition
}

type targetKey struct {
	kind    cost.TargetKind
	entity  *schema.Entity
	index   *schema.Index
	unique  *schema.Unique
	counter *schema.Counter
}

func keyOf(t cost.Target) targetKey {
	return targetKey{t.Kind, t.Entity, t.Index, t.Unique, t.Counter}
}

// MemberOf returns the partition member for a cost target, or nil.
func (r *Result) MemberOf(t cost.Target) *Member { return r.targets[keyOf(t)] }

// partitions groups every item family by partition key template, and estimates the partitions'
// counts and sizes from the volumes.
func (a *analyzer) partitions() {
	byKey := map[string]*Partition{}
	add := func(index, pk string, fields []*schema.Field, shards int, mb *Member) {
		k := index + "\x00" + pk
		p := byKey[k]
		if p == nil {
			p = &Partition{Index: index, PK: pk, Shards: shards}
			for _, f := range fields {
				p.Fields = append(p.Fields, f.Name)
			}
			byKey[k] = p
			a.r.Partitions = append(a.r.Partitions, p)
		}
		mb.Partition = p
		p.Members = append(p.Members, mb)
		a.r.targets[keyOf(mb.Target)] = mb
	}
	for _, e := range a.m.Entities {
		er := a.r.Cost.Entity(e)
		cnt := a.perKey(e, e.PK.Fields)
		add("", e.PK.Raw, e.PK.Fields, 1, &Member{
			Target: cost.Target{Kind: cost.TargetItem, Entity: e}, Label: e.Name,
			Count: cnt, Weight: cnt, Size: er.Item, Grows: a.grows(e, nil, e.PK.Fields), Owner: entitySubject(e),
		})
		for _, ix := range e.Indexes {
			cnt := a.perKey(e, ix.PK.Fields)
			mb := &Member{Label: e.Name + " " + ix.Name + " entry", Count: cnt, Weight: cnt, Size: er.Indexes[ix.Name],
				Grows: a.grows(e, ix, ix.PK.Fields), Owner: indexSubject(ix)}
			if ix.Strategy == schema.StrategyGSI {
				mb.Target = cost.Target{Kind: cost.TargetGSI, Entity: e, Index: ix}
				add(ix.GSI.Name, ix.PK.Raw, ix.PK.Fields, 1, mb)
				continue
			}
			mb.Target = cost.Target{Kind: cost.TargetCopy, Entity: e, Index: ix}
			mb.Label = e.Name + " " + ix.Name + " copy"
			add("", ix.PK.Raw, ix.PK.Fields, 1, mb)
		}
		for _, u := range e.Uniques {
			one := exactly(1)
			add("", u.PK.Raw, u.PK.Fields, 1, &Member{
				Target: cost.Target{Kind: cost.TargetClaim, Entity: e, Unique: u}, Label: e.Name + " " + u.Name + " claim",
				Count: one, Weight: one, Size: cost.ClaimSize, Owner: uniqueSubject(u),
			})
		}
		for _, c := range e.Counters {
			pk := c.PK.Raw
			if c.Shards > 1 {
				pk += fmt.Sprintf("#S{0..%d}", c.Shards-1)
			}
			count := exactly(1)
			if extra := minus(c.SK.Fields, c.PK.Fields); len(extra) > 0 {
				count = Estimate{} // one counter item per value of the sort key's own fields
			}
			w := a.perKey(e, c.KeyFields())
			add("", pk, c.PK.Fields, c.Shards, &Member{
				Target: cost.Target{Kind: cost.TargetCounter, Entity: e, Counter: c}, Label: "counter " + c.Name,
				Count: count, Weight: w, Size: cost.CounterSize(c), Owner: counterSubject(c),
			})
		}
	}
	for _, p := range a.r.Partitions {
		a.summarise(p)
	}
}

// summarise totals a partition family's members.
func (a *analyzer) summarise(p *Partition) {
	p.SingleItem = true
	sizeKnown, maxKnown := true, true
	for _, mb := range p.Members {
		e := mb.Target.Entity
		if mb.Count.Known && mb.Count.Typical > 0 && e.Count > 0 {
			var n float64
			if mb.Target.Kind == cost.TargetCounter {
				n = e.Count / mb.Weight.Typical
			} else {
				n = e.Count / mb.Count.Typical
			}
			if mb.Target.Kind == cost.TargetCounter && !mb.Weight.Known {
				n = 0
			}
			if n > p.Count.Typical {
				p.Count = Estimate{Typical: n, Max: n, Known: true, MaxKnown: true}
			}
		}
		if !mb.Count.MaxKnown || mb.Count.Max > 1 {
			p.SingleItem = false
		}
		p.Grows = p.Grows || mb.Grows
		if !mb.Count.Known {
			sizeKnown = false
		}
		if !mb.Count.MaxKnown {
			maxKnown = false
		}
		p.Size.Typical += mb.Count.Typical * float64(mb.Size.P50)
		p.Size.Max += mb.Count.Max * float64(mb.Size.P50)
	}
	if len(p.Members) > 1 {
		p.SingleItem = false
	}
	p.Size.Known, p.Size.MaxKnown = sizeKnown, sizeKnown && maxKnown
	for _, f := range p.Fields {
		for _, mb := range p.Members {
			if mb.Target.Kind == cost.TargetClaim || (mb.Count.Known && mb.Weight.Known) {
				continue // a claim is one item per value by definition
			}
			if fd := mb.Target.Entity.Field(f); fd != nil && a.undeclared(mb.Target.Entity, fd) && !slices.Contains(p.Undeclared, f) {
				p.Undeclared = append(p.Undeclared, f)
			}
		}
	}
}

// undeclared reports whether a partition key field is neither an enum nor linked to another
// entity whose count is declared: nothing says how many items share one of its values.
func (a *analyzer) undeclared(e *schema.Entity, f *schema.Field) bool {
	if f.Type == schema.TypeEnum || f.Type == schema.TypeBool {
		return false
	}
	for _, anc := range a.ancestors(e) {
		for _, k := range anc.key {
			if k == f {
				return false
			}
		}
	}
	for _, b := range e.Volume.By {
		for _, k := range b.Relation.Key {
			if k.Source == f {
				return false
			}
		}
	}
	for _, rel := range a.m.Relations {
		if rel.From != e {
			continue
		}
		for _, k := range rel.Key {
			if k.Source == f {
				return false
			}
		}
	}
	return true
}

// ancestor is an entity up e's parent chain, with its key in e's fields and e's items per one of it.
type ancestor struct {
	entity *schema.Entity
	key    []*schema.Field // e's fields holding the ancestor's key
	per    Estimate
}

// ancestors walks up e's parents. Per-ancestor counts multiply the typical volumes; the largest
// count takes the step with the biggest skew (max over typical) at its max, and the others typical.
func (a *analyzer) ancestors(e *schema.Entity) []ancestor {
	var out []ancestor
	mapping := map[*schema.Field]*schema.Field{} // field of the current ancestor → field of e
	for _, f := range e.Fields {
		mapping[f] = f
	}
	cur := e
	var steps []Estimate
	seen := map[*schema.Entity]bool{e: true}
	for cur.Parent != nil && !seen[cur.Parent.To] {
		rel := cur.Parent
		seen[rel.To] = true
		next := map[*schema.Field]*schema.Field{}
		var key []*schema.Field
		ok := true
		for _, k := range rel.Key {
			src := mapping[k.Source]
			if src == nil {
				src = e.Field(k.Source.Name)
			}
			if src == nil {
				ok = false
				break
			}
			next[k.Target] = src
			key = append(key, src)
		}
		if !ok {
			break
		}
		for _, f := range rel.To.Fields {
			if _, done := next[f]; !done {
				if ef := e.Field(f.Name); ef != nil {
					next[f] = ef
				}
			}
		}
		steps = append(steps, stepVolume(cur, rel))
		out = append(out, ancestor{entity: rel.To, key: key, per: chain(steps)})
		mapping, cur = next, rel.To
	}
	return out
}

// stepVolume is how many items of e each item of rel.To has.
func stepVolume(e *schema.Entity, rel *schema.Relation) Estimate {
	v := e.Volume
	if v.Declared && v.Per == rel {
		return Estimate{Typical: v.Typical, Max: v.Max, Known: true, MaxKnown: v.Max > 0}
	}
	if e.Count > 0 && rel.To.Count > 0 {
		est := Estimate{Typical: e.Count / rel.To.Count, Known: true}
		if rel.OneToOne() {
			est.Max, est.MaxKnown = 1, true
		}
		return est
	}
	if rel.OneToOne() {
		return Estimate{Max: 1, MaxKnown: true}
	}
	return Estimate{}
}

// chain combines per-step counts: typicals multiply, and the max takes the most skewed step.
func chain(steps []Estimate) Estimate {
	out := Estimate{Typical: 1, Known: true, MaxKnown: true}
	best := 0.0
	for _, s := range steps {
		out.Typical *= s.Typical
		out.Known = out.Known && s.Known
		out.MaxKnown = out.MaxKnown && s.MaxKnown
	}
	if !out.MaxKnown {
		return out
	}
	for i, s := range steps {
		m := s.Max
		for j, o := range steps {
			if j != i {
				m *= o.Typical
			}
		}
		best = math.Max(best, m)
	}
	out.Max = math.Max(best, out.Typical)
	return out
}

// perKey estimates how many items of e share one value of the given fields: the count of e per
// partition when they are a partition key's fields.
func (a *analyzer) perKey(e *schema.Entity, fields []*schema.Field) Estimate {
	in := map[*schema.Field]bool{}
	for _, f := range fields {
		in[f] = true
	}
	if coversAll(in, e.KeyFields()) {
		// At most one per value; fewer when e is an optional companion of its parent (a tool's hold).
		if rel := e.Parent; rel != nil && rel.OneToOne() && coversAll(in, sources(rel.Key)) {
			if st := stepVolume(e, rel); st.Known && st.Typical < 1 {
				return Estimate{Typical: st.Typical, Max: 1, Known: true, MaxKnown: true}
			}
		}
		return exactly(1)
	}
	// The anchor is the related entity whose key the fields cover most closely.
	var base Estimate
	var covered []*schema.Field
	found := false
	for _, anc := range a.ancestors(e) {
		if coversAll(in, anc.key) {
			base, covered, found = anc.per, anc.key, true
			break
		}
	}
	for _, b := range e.Volume.By {
		src := sources(b.Relation.Key)
		if coversAll(in, src) && (!found || len(src) > len(covered)) {
			base = Estimate{Typical: b.Typical, Max: b.Max, Known: true, MaxKnown: b.Max > 0}
			covered, found = src, true
		}
	}
	for _, rel := range a.m.Relations {
		src := sources(rel.Key)
		if rel.From != e || rel.To == e || !coversAll(in, src) || (found && len(src) <= len(covered)) {
			continue
		}
		base, covered, found = Estimate{}, src, true
		if e.Count > 0 && rel.To.Count > 0 {
			base = Estimate{Typical: e.Count / rel.To.Count, Known: true}
		}
		if rel.OneToOne() {
			base.Max, base.MaxKnown = 1, true
		}
	}
	if !found {
		base = Estimate{Typical: e.Count, Max: e.Count, Known: e.Count > 0, MaxKnown: e.Count > 0}
	}
	for _, f := range fields {
		if slices.Contains(covered, f) {
			continue
		}
		switch f.Type {
		case schema.TypeEnum:
			base.Typical /= float64(len(f.Enum))
		case schema.TypeBool:
			base.Typical /= 2
		default:
			// Nothing declares how many items share a value of this field.
			return Estimate{}
		}
	}
	return base
}

func coversAll(in map[*schema.Field]bool, fs []*schema.Field) bool {
	for _, f := range fs {
		if !in[f] {
			return false
		}
	}
	return true
}

func sources(key []schema.RequireKey) []*schema.Field {
	out := make([]*schema.Field, len(key))
	for i, k := range key {
		out[i] = k.Source
	}
	return out
}

func minus(a, b []*schema.Field) []*schema.Field {
	var out []*schema.Field
	for _, f := range a {
		if !slices.Contains(b, f) {
			out = append(out, f)
		}
	}
	return out
}

// grows reports whether a family of e's items (its own, or an index's entries) only ever grows
// within a partition: e is created but never deleted, doesn't expire, isn't consumed by another
// write, and (for an index) no write moves an item out through the index's where.
func (a *analyzer) grows(e *schema.Entity, ix *schema.Index, pk []*schema.Field) bool {
	in := map[*schema.Field]bool{}
	for _, f := range pk {
		in[f] = true
	}
	if coversAll(in, e.KeyFields()) || e.TTL != nil {
		return false
	}
	created := false
	for _, w := range e.Writes {
		switch w.Kind {
		case schema.WriteCreate:
			created = true
		case schema.WriteDelete:
			return false
		}
	}
	for _, oe := range a.m.Entities {
		for _, w := range oe.Writes {
			for _, rq := range w.Requires {
				if rq.Target == e && rq.Consume {
					return false
				}
			}
		}
	}
	if ix != nil && len(ix.Where) > 0 {
		for _, s := range a.setsOn(e) {
			for _, p := range ix.Where {
				if s.Field == p.Field && (s.Source != nil || fmt.Sprint(s.Value) != fmt.Sprint(p.Value)) {
					return false
				}
			}
		}
	}
	return created
}

// setsOn returns every constant or reference any write sets on e: its own updates, and other
// entities' requires.
func (a *analyzer) setsOn(e *schema.Entity) []schema.SetConst {
	var out []schema.SetConst
	for _, w := range e.Writes {
		if w.Kind == schema.WriteUpdate {
			out = append(out, w.Sets...)
			for _, f := range w.Args {
				out = append(out, schema.SetConst{Field: f, Source: f})
			}
			for _, f := range w.Patch {
				out = append(out, schema.SetConst{Field: f, Source: f})
			}
		}
	}
	for _, oe := range a.m.Entities {
		for _, w := range oe.Writes {
			for _, rq := range w.Requires {
				if rq.Target == e {
					out = append(out, rq.Sets...)
				}
			}
		}
	}
	return out
}

// share is the fraction of an entity's traffic the busiest partition of a member takes: its
// largest weight over the entity's items.
func share(mb *Member) (float64, bool) {
	e := mb.Target.Entity
	if e.Count <= 0 || !mb.Weight.Known {
		return 0, false
	}
	w := mb.Weight.Typical
	if mb.Weight.MaxKnown {
		w = mb.Weight.Max
	}
	return math.Min(1, math.Max(w, 1)/e.Count), true
}

// traffic estimates each partition's busiest key at peak from the declared rates.
func (a *analyzer) traffic() {
	peak := a.m.Workload.Peak
	for _, er := range a.r.Cost.Entities {
		for _, wc := range er.Writes {
			w := wc.Write
			if w.Rate <= 0 && w.HotKeyRate <= 0 {
				continue
			}
			tx := 1.0
			if wc.Transactional {
				tx = 2
			}
			for _, t := range wc.Touches {
				mb := a.r.MemberOf(t.Target)
				if mb == nil {
					continue
				}
				calls := w.HotKeyRate
				if calls <= 0 {
					s, known := share(mb)
					if !known {
						continue
					}
					calls = w.Rate * peak * s
				}
				units := t.Units.P50
				if !t.Async {
					units *= tx
				}
				if mb.Target.Kind == cost.TargetCounter && mb.Target.Counter.Shards > 1 {
					units /= float64(mb.Target.Counter.Shards)
				}
				p := mb.Partition
				p.Rated = true
				p.PeakWRU += calls * units
				if p.busiestWRU == nil {
					p.busiestWRU = map[*Member]float64{}
				}
				p.busiestWRU[mb] += calls * units
			}
		}
		for _, rc := range er.Reads {
			ac := rc.Access
			if ac.Rate <= 0 {
				continue
			}
			for i, t := range rc.Reads {
				mb := a.r.MemberOf(t)
				if mb == nil {
					continue
				}
				s, known := share(mb)
				if !known {
					continue
				}
				// Each partition read takes its own part of the call: a unique lookup's claim read
				// is 1 RRU and the item read the rest; a sharded counter read is one item per shard.
				units := rc.RRU.P50
				switch {
				case ac.Kind == schema.AccessGetUnique && i == 0:
					units = 1
				case ac.Kind == schema.AccessGetUnique:
					units = math.Max(0, units-1)
				case t.Kind == cost.TargetCounter && t.Counter.Shards > 1:
					units /= float64(t.Counter.Shards)
				}
				p := mb.Partition
				p.Rated = true
				p.PeakRRU += ac.Rate * peak * s * units
			}
		}
	}
	for _, p := range a.r.Partitions {
		if !p.Rated {
			continue
		}
		p.Headroom = math.Max(p.PeakWRU/cost.PartitionWCU, p.PeakRRU/cost.PartitionRCU)
		switch {
		case p.Headroom >= 0.5:
			p.Risk = RiskHigh
		case p.Headroom >= 0.1:
			p.Risk = RiskMedium
		default:
			p.Risk = RiskLow
		}
	}
}

// Busiest returns the member taking most of the partition's write capacity, or its first member.
func (p *Partition) Busiest() *Member {
	var best *Member
	for _, mb := range p.Members {
		if best == nil || p.busiestWRU[mb] > p.busiestWRU[best] {
			best = mb
		}
	}
	return best
}

// readStats estimates how many items and pages reading all of a query's partition takes.
func (a *analyzer) readStats() {
	for _, e := range a.m.Entities {
		for _, ac := range e.Access {
			if ac.Kind != schema.AccessQuery && ac.Kind != schema.AccessScan {
				continue
			}
			st := &ReadStats{}
			var t cost.Target
			switch {
			case ac.Kind == schema.AccessScan:
				st.Items = Estimate{Typical: e.Count, Max: e.Count, Known: e.Count > 0, MaxKnown: e.Count > 0}
				// A page evaluates items of every kind: reading all of them pages through the table.
				st.Evaluated = Estimate{Typical: a.r.Cost.BaseItems, Max: a.r.Cost.BaseItems, Known: a.r.Cost.BaseItems > 0, MaxKnown: a.r.Cost.BaseItems > 0}
			case ac.Index == nil:
				t = cost.Target{Kind: cost.TargetItem, Entity: e}
			case ac.Index.Strategy == schema.StrategyGSI:
				t = cost.Target{Kind: cost.TargetGSI, Entity: e, Index: ac.Index}
			default:
				t = cost.Target{Kind: cost.TargetCopy, Entity: e, Index: ac.Index}
			}
			if ac.Kind == schema.AccessQuery {
				if mb := a.r.MemberOf(t); mb != nil {
					st.Items, st.Partition = mb.Count, mb.Partition
				}
				st.Filtered = ac.Index != nil && len(ac.Index.Where) > 0
			}
			pages := func(n float64) float64 { return math.Max(1, math.Ceil(n/float64(ac.Page))) }
			read := st.Items
			if ac.Kind == schema.AccessScan {
				read = st.Evaluated
			}
			st.Pages = Estimate{Known: read.Known, MaxKnown: read.MaxKnown}
			if read.Known {
				st.Pages.Typical = pages(read.Typical)
			}
			if read.MaxKnown {
				st.Pages.Max = pages(read.Max)
			}
			a.r.Reads[ac] = st
		}
	}
}

// FieldList renders field names for people: "libraryId, category".
func FieldList(fs []string) string { return strings.Join(fs, ", ") }
