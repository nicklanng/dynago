// Package cost estimates item sizes, capacity units and monthly cost for a schema, and flags
// design risks. Every number is derived from declared assumptions (field sizes, item counts,
// request rates), which the report lists alongside the results.
package cost

import (
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/nicklanng/dynago/internal/schema"
)

// Prices are on-demand prices in dollars. Defaults are us-east-1 list prices; pass your own.
type Prices struct {
	WRUPerMillion float64
	RRUPerMillion float64
	GBMonth       float64
}

// DefaultPrices are DynamoDB Standard on-demand list prices in us-east-1.
var DefaultPrices = Prices{WRUPerMillion: 0.625, RRUPerMillion: 0.125, GBMonth: 0.25}

const (
	// indexOverhead is the per-item overhead DynamoDB adds to index and item storage.
	indexOverhead = 100
	// partitionWCU is the write throughput of one partition.
	partitionWCU    = 1000
	secondsPerMonth = 30 * 24 * 3600
	// conflictRate is the rate of transactions per item above which conflicts become routine.
	conflictRate = 20
	maxItemSize  = 400 * 1024
)

// Size is an estimated size in bytes at the median and 99th percentile.
type Size struct{ P50, P99 int }

func (s Size) add(o Size) Size { return Size{s.P50 + o.P50, s.P99 + o.P99} }

// Units is a capacity estimate at the median and 99th percentile item sizes.
type Units struct{ P50, P99 float64 }

func (u Units) add(o Units) Units { return Units{u.P50 + o.P50, u.P99 + o.P99} }

// Severity of a finding.
type Severity string

// Severities.
const (
	Error Severity = "error"
	Warn  Severity = "warning"
	Info  Severity = "note"
)

// Finding is a risk or design note.
type Finding struct {
	Severity Severity
	Subject  string
	Message  string
}

// ItemCost describes one family of stored items.
type ItemCost struct {
	Name string
	Kind string // entity, gsi, copy, claim, counter
	Size Size
}

// ReadCost is the estimated cost of one access pattern call.
type ReadCost struct {
	Access     *schema.Access
	Requests   string
	RoundTrips int
	RRU        Units
	Monthly    float64
}

// WriteCost is the estimated cost of one write call.
type WriteCost struct {
	Write         *schema.Write
	Items         []string
	ReadFirst     bool
	Transactional bool
	MaxTxItems    int
	WRU           Units
	RRU           float64
	Monthly       float64
}

// EntityReport is the estimate for one entity.
type EntityReport struct {
	Entity     *schema.Entity
	Item       Size
	Indexes    map[string]Size
	Items      []ItemCost
	Reads      []ReadCost
	Writes     []WriteCost
	StorageGB  float64
	StorageUSD float64
}

// Report is the estimate for a whole schema.
type Report struct {
	Prices      Prices
	Entities    []*EntityReport
	Findings    []Finding
	Assumptions []string
	MonthlyUSD  float64
}

// Analyze estimates costs and collects findings.
func Analyze(m *schema.Model, p Prices) *Report {
	r := &Report{Prices: p}
	r.Assumptions = append(r.Assumptions,
		"Sizes use each field's declared size (p50/p99); undeclared sizes use type defaults (string 20/64 B, time 30/35 B, int 8/11 B).",
		"Every declared field is assumed present; empty fields are not stored, so real items are usually smaller.",
		fmt.Sprintf("A unique string set makes one claim per element; the number of elements is estimated from the set's declared size at %d B per element.", setElementBytes),
		"Capacity follows DynamoDB rules: 1 WRU per started 1 KB written, 1 RRU per started 4 KB read strongly (half for eventually consistent); transactions cost double, and a condition check on another item is billed as a transactional write of that item.",
		"GSI and copy writes are counted as one index write per entry; an index key change is a delete plus a put.",
		fmt.Sprintf("Prices: $%.3f per million WRU, $%.3f per million RRU, $%.2f per GB-month (on-demand).", p.WRUPerMillion, p.RRUPerMillion, p.GBMonth),
		"Monthly figures use each access pattern's and write's declared average rate (rate:, per second); patterns without a rate are not costed.",
	)
	for _, e := range m.Entities {
		er := analyzeEntity(r, m, e)
		r.Entities = append(r.Entities, er)
		r.MonthlyUSD += er.StorageUSD
		for _, rc := range er.Reads {
			r.MonthlyUSD += rc.Monthly
		}
		for _, wc := range er.Writes {
			r.MonthlyUSD += wc.Monthly
		}
	}
	for _, g := range m.GSIs {
		if len(g.Users) > 1 {
			var names []string
			for _, ix := range g.Users {
				names = append(names, ix.Entity.Name)
			}
			r.Findings = append(r.Findings, Finding{Info, "GSI " + g.Name,
				fmt.Sprintf("shared by %s; its projection is the union of what each needs, so every entity pays for the others' projected attributes.", strings.Join(names, ", "))})
		}
	}
	sort.SliceStable(r.Findings, func(i, j int) bool { return rank(r.Findings[i].Severity) < rank(r.Findings[j].Severity) })
	return r
}

func rank(s Severity) int {
	switch s {
	case Error:
		return 0
	case Warn:
		return 1
	}
	return 2
}

// HasErrors reports whether any finding is an error.
func (r *Report) HasErrors() bool {
	for _, f := range r.Findings {
		if f.Severity == Error {
			return true
		}
	}
	return false
}

// sizes estimates the sizes of an entity's item, index entries, claims and counters.
func sizes(m *schema.Model, e *schema.Entity) *EntityReport {
	er := &EntityReport{Entity: e, Indexes: map[string]Size{}}
	item := ItemSize(m, e)
	er.Item = item
	er.Items = append(er.Items, ItemCost{Name: e.Name, Kind: "entity", Size: item})
	keys := attrSize(schema.AttrPK, templateSize(e.PK)).add(attrSize(schema.AttrSK, templateSize(e.SK)))
	meta := metaSize(e)

	for _, ix := range e.Indexes {
		var s Size
		switch {
		case ix.Strategy == schema.StrategyGSI && ix.GSI.Projection == schema.ProjectAll:
			s = item.add(indexKeysSize(ix))
		case ix.Strategy == schema.StrategyGSI:
			s = keys.add(indexKeysSize(ix)).add(fieldsSize(ix.ProjectedFields())).add(meta)
		default: // copy
			s = attrSize(schema.AttrPK, templateSize(ix.PK)).add(attrSize(schema.AttrSK, templateSize(ix.SK))).add(fieldsSize(ix.ProjectedFields())).add(meta)
		}
		er.Indexes[ix.Name] = s
		kind := "gsi"
		if ix.Strategy == schema.StrategyCopy {
			kind = "copy"
		}
		er.Items = append(er.Items, ItemCost{Name: e.Name + "." + ix.Name, Kind: kind, Size: s})
	}
	for _, u := range e.Uniques {
		s := attrSize(schema.AttrPK, templateSize(u.PK)).add(attrSize(schema.AttrSK, templateSize(u.SK))).
			add(attrSize("_t", Size{len(e.Name + u.Name), len(e.Name + u.Name)})).add(keys)
		er.Items = append(er.Items, ItemCost{Name: e.Name + "." + u.Name, Kind: "claim", Size: s})
	}
	for _, c := range e.Counters {
		s := attrSize(schema.AttrPK, templateSize(c.PK)).add(attrSize(schema.AttrSK, templateSize(c.SK))).
			add(attrSize("_t", Size{len(c.Name), len(c.Name)}))
		for _, v := range c.Values {
			s = s.add(attrSize(v.Attr, Size{8, 11}))
		}
		er.Items = append(er.Items, ItemCost{Name: c.Name, Kind: "counter", Size: s})
		if c.Shards > 1 {
			er.Items[len(er.Items)-1].Name += fmt.Sprintf(" (×%d shards)", c.Shards)
		}
	}
	return er
}

func analyzeEntity(r *Report, m *schema.Model, e *schema.Entity) *EntityReport {
	er := sizes(m, e)
	item := er.Item

	// Findings about the item itself.
	switch {
	case item.P99 > maxItemSize:
		r.Findings = append(r.Findings, Finding{Error, e.Name, fmt.Sprintf("p99 item size is %s, over DynamoDB's 400 KB limit; move large fields to S3 or split the item.", human(item.P99))})
	case item.P99 > 100*1024:
		r.Findings = append(r.Findings, Finding{Warn, e.Name, fmt.Sprintf("p99 item size is %s; every write and every read of it costs %0.f+ units. Consider splitting rarely-read fields into a separate item.", human(item.P99), math.Ceil(float64(item.P99)/1024))})
	}
	if e.TTL != nil {
		// TTL deletes bypass the generated code, so nothing releases what the item contributed.
		if len(e.Counters) > 0 {
			r.Findings = append(r.Findings, Finding{Warn, e.Name, fmt.Sprintf("expires by TTL (%s) and feeds counters: an expired item is never subtracted, so the counters only ever count items created, not items that currently exist.", e.TTL.Name)})
		}
		if len(e.Uniques) > 0 {
			r.Findings = append(r.Findings, Finding{Warn, e.Name, fmt.Sprintf("expires by TTL (%s) but holds unique claims: an expired item's claims stay behind and keep the value taken.", e.TTL.Name)})
		}
		for _, ix := range e.Indexes {
			if ix.Strategy == schema.StrategyCopy {
				r.Findings = append(r.Findings, Finding{Warn, e.Name + "." + ix.Name, fmt.Sprintf("copies of a TTL-expiring %s outlive it; give the copies their own expiry or use a GSI.", e.Name)})
			}
		}
	}
	for _, ix := range e.Indexes {
		if ix.Strategy == schema.StrategyGSI && ix.GSI.Projection == schema.ProjectAll {
			r.Findings = append(r.Findings, Finding{Info, e.Name + "." + ix.Name,
				fmt.Sprintf("projects ALL: every write of a %s stores and writes the whole item (%s) a second time. List fields only (project: [...]) usually suffice.", e.Name, human(item.P50))})
		}
	}

	for _, a := range e.Access {
		rc := readCost(a, er)
		if a.Rate > 0 {
			rc.Monthly = (rc.RRU.P50 * a.Rate * secondsPerMonth / 1e6) * r.Prices.RRUPerMillion
		}
		er.Reads = append(er.Reads, rc)
	}
	for _, w := range e.Writes {
		wc := writeCost(m, e, w, er)
		if w.Rate > 0 {
			wc.Monthly = (wc.WRU.P50*r.Prices.WRUPerMillion + wc.RRU*r.Prices.RRUPerMillion) * w.Rate * secondsPerMonth / 1e6
		}
		er.Writes = append(er.Writes, wc)
		writeFindings(r, er, w, wc)
	}

	largeFieldFindings(r, m, e, er)
	for _, f := range e.Fields {
		if f.CopyOf != nil {
			r.Findings = append(r.Findings, Finding{Warn, e.Name + "." + f.Name, fmt.Sprintf(
				"copies %s.%s. dynago keeps copies within one entity in sync, but not this one: when %s.%s changes, your code must rewrite every %s that copied it.",
				f.CopyOfEntity.Name, f.CopyOf.Name, f.CopyOfEntity.Name, f.CopyOf.Name, e.Name)})
		}
	}
	if e.Items > 0 {
		bytes := float64(e.Items) * float64(item.P50+indexOverhead)
		for _, ix := range e.Indexes {
			bytes += float64(e.Items) * float64(er.Indexes[ix.Name].P50+indexOverhead)
		}
		bytes += float64(e.Items) * float64(len(e.Uniques)) * float64(claimSize.P50+indexOverhead)
		er.StorageGB = bytes / (1 << 30)
		er.StorageUSD = er.StorageGB * r.Prices.GBMonth
	}
	return er
}

func readCost(a *schema.Access, er *EntityReport) ReadCost {
	rc := ReadCost{Access: a, RoundTrips: 1}
	factor := 0.5
	if a.Consistent {
		factor = 1
	}
	switch a.Kind {
	case schema.AccessGet:
		rc.Requests = "GetItem"
		rc.RRU = Units{rru(er.Item.P50, factor), rru(er.Item.P99, factor)}
	case schema.AccessGetUnique:
		rc.Requests = "GetItem (claim) → GetItem"
		rc.RoundTrips = 2
		rc.RRU = Units{1 + rru(er.Item.P50, 1), 1 + rru(er.Item.P99, 1)}
	case schema.AccessQuery:
		entry := er.Item
		if a.Index != nil {
			entry = er.Indexes[a.Index.Name]
		}
		rc.Requests = "Query"
		rc.RRU = Units{rru(entry.P50*a.Page, factor), rru(entry.P99*a.Page, factor)}
	case schema.AccessCounter:
		if a.Counter.Shards > 1 {
			rc.Requests = fmt.Sprintf("BatchGetItem (%d shards)", a.Counter.Shards)
		} else {
			rc.Requests = "GetItem"
		}
		n := float64(a.Counter.Shards) * factor
		rc.RRU = Units{n, n}
	}
	return rc
}

func writeCost(m *schema.Model, e *schema.Entity, w *schema.Write, er *EntityReport) WriteCost {
	wc := WriteCost{Write: w, ReadFirst: w.ReadFirst && !w.Transition}
	// gsi holds index entries DynamoDB writes asynchronously, outside any transaction. own holds
	// the items the write itself puts, updates, deletes or checks.
	var gsi, own Units
	ops := 0
	write := func(async bool, name string, s Size, n Count) {
		dst := &own
		if async {
			dst = &gsi
		} else {
			ops += n.P99
		}
		*dst = dst.add(Units{float64(n.P50) * wru(s.P50), float64(n.P99) * wru(s.P99)})
		wc.Items = append(wc.Items, name)
	}
	write(false, e.Name, er.Item, once)
	derivedWrites(e, w, er, "", write)
	if wc.ReadFirst {
		wc.RRU = rru(er.Item.P50, 1)
	}
	for _, rq := range w.Requires {
		switch {
		case rq.Counter != nil:
			// A condition check on another item is billed as a transactional write of that item.
			write(false, "check counter "+rq.Counter.Name, counterSize, once)
		case rq.Writes():
			ter := sizes(m, rq.Target)
			tw := rq.TargetWrite()
			label := rq.Name + " (" + rq.Effect(e.Name) + ")"
			if rq.Consume {
				tw = &schema.Write{Name: "requires", Entity: rq.Target, Kind: schema.WriteDelete}
			}
			write(false, label, ter.Item, once)
			derivedWrites(rq.Target, tw, ter, rq.Name+"'s ", write)
			if !rq.Fast {
				wc.RRU += rru(ter.Item.P50, 1)
			}
		default:
			write(false, "check "+rq.Name, ItemSize(m, rq.Target), once)
		}
	}
	wc.MaxTxItems = ops
	wc.Transactional = ops > 1
	if wc.Transactional {
		own = own.add(own) // transactions charge double for every item they write
	}
	wc.WRU = own.add(gsi)
	return wc
}

// writeFunc records one item a write touches: async marks GSI entries, which DynamoDB writes
// outside the transaction.
type writeFunc func(async bool, name string, s Size, n Count)

// Count is how many items of one kind a write touches, typically (P50) and at worst (P99).
type Count struct{ P50, P99 int }

var (
	once  = Count{1, 1}
	twice = Count{2, 2}
)

// setElementBytes is the assumed size of one element of a string set, for estimating how many
// claims a set makes from the set's declared size.
const setElementBytes = 20

// setElements estimates the number of elements of a string set field.
func setElements(f *schema.Field) Count {
	n := func(size int) int { return max(1, (size+setElementBytes-1)/setElementBytes) }
	return Count{n(f.SizeP50), n(f.SizeP99)}
}

// derivedWrites records the index entries, copies, counters and claims a write of e changes. A
// create or delete touches everything the entity has; an update only what its fields feed.
func derivedWrites(e *schema.Entity, w *schema.Write, er *EntityReport, owner string, write writeFunc) {
	changed := map[*schema.Field]bool{}
	for _, f := range w.Changed() {
		changed[f] = true
	}
	touches := func(fs ...[]*schema.Field) bool {
		for _, l := range fs {
			for _, f := range l {
				if changed[f] {
					return true
				}
			}
		}
		return false
	}
	all := w.Kind != schema.WriteUpdate
	for _, ix := range e.Indexes {
		keyFields := append(append([]*schema.Field{}, ix.PK.Fields...), ix.SK.Fields...)
		keyFields = append(keyFields, predFields(ix.Where)...)
		moved, projected := touches(keyFields), ix.Projection == schema.ProjectAll || touches(ix.ProjectedFields())
		size := er.Indexes[ix.Name]
		if ix.Strategy == schema.StrategyGSI {
			switch {
			case all:
				write(true, owner+"GSI "+ix.Name+" entry", size, once)
			case moved:
				write(true, owner+"GSI "+ix.Name+" entry (moved: delete + put)", size, twice)
			case projected:
				write(true, owner+"GSI "+ix.Name+" entry", size, once)
			}
			continue
		}
		switch {
		case all:
			write(false, owner+"copy "+ix.Name, size, once)
		case moved:
			write(false, owner+"copy "+ix.Name+" (moved: put + delete)", size, twice)
		case touches(ix.ProjectedFields()):
			write(false, owner+"copy "+ix.Name, size, once)
		}
	}
	for _, c := range e.Counters {
		in := [][]*schema.Field{c.KeyFields()}
		for _, v := range c.Values {
			in = append(in, predFields(v.Where))
			if v.Sum != nil {
				in = append(in, []*schema.Field{v.Sum})
			}
		}
		switch {
		case all:
			write(false, owner+"counter "+c.Name, counterSize, once)
		case touches(c.KeyFields()):
			write(false, owner+"counter "+c.Name+" (moved: two counter items)", counterSize, twice)
		case touches(in...):
			write(false, owner+"counter "+c.Name, counterSize, once)
		}
	}
	for _, u := range e.Uniques {
		if u.Set != nil {
			// One claim per element: all of them on a create or delete. An update claims the
			// elements it adds and releases those it drops: typically one of each, at worst all.
			n := setElements(u.Set)
			switch {
			case all:
				write(false, fmt.Sprintf("%sclaims %s (one per %s element)", owner, u.Name, u.Set.Name), claimSize, n)
			case touches(u.Fields):
				write(false, fmt.Sprintf("%sclaims %s (added and dropped %s elements)", owner, u.Name, u.Set.Name), claimSize, Count{2, 2 * n.P99})
			}
			continue
		}
		switch {
		case all:
			write(false, owner+"claim "+u.Name, claimSize, once)
		case touches(u.Fields) && clears(w, u.Fields):
			write(false, owner+"claim "+u.Name+" (released)", claimSize, once)
		case touches(u.Fields):
			write(false, owner+"claim "+u.Name+" (moved: put + delete)", claimSize, twice)
		}
	}
}

var (
	counterSize = Size{100, 100}
	claimSize   = Size{150, 150}
)

func writeFindings(r *Report, er *EntityReport, w *schema.Write, wc WriteCost) {
	e := er.Entity
	subject := e.Name + "." + w.Name
	if wc.MaxTxItems > 100 {
		r.Findings = append(r.Findings, Finding{Error, subject, fmt.Sprintf("can touch %d items, over the 100-item transaction limit.", wc.MaxTxItems)})
	}
	if w.HotKeyRate <= 0 {
		return
	}
	tx := 1.0
	if wc.Transactional {
		tx = 2
	}
	for _, c := range e.Counters {
		touched := false
		for _, it := range wc.Items {
			touched = touched || strings.HasPrefix(it, "counter "+c.Name)
		}
		if !touched {
			continue
		}
		perItem := w.HotKeyRate / float64(c.Shards)
		wruPerItem := perItem * tx
		suggest := int(math.Ceil(w.HotKeyRate / 10))
		switch {
		case wruPerItem > partitionWCU:
			r.Findings = append(r.Findings, Finding{Error, subject, fmt.Sprintf(
				"at %.0f writes/s to one %s key, each counter item takes %.0f WRU/s, over a partition's %d: it will throttle. Set shards: %d or more.",
				w.HotKeyRate, c.Name, wruPerItem, partitionWCU, suggest)})
		case wc.Transactional && perItem > conflictRate:
			r.Findings = append(r.Findings, Finding{Warn, subject, fmt.Sprintf(
				"at %.0f writes/s to one %s key, each counter item is written by ~%.0f transactions/s. Transactions touching the same item at once conflict and retry (about half a second by default; see dynago.SetRetries), so expect latency and ErrConflict under bursts. Set shards: %d (about 10 transactions/s per item)%s.",
				w.HotKeyRate, c.Name, perItem, suggest, boundedNote(c))})
		case perItem >= 5:
			r.Findings = append(r.Findings, Finding{Info, subject, fmt.Sprintf(
				"at %.0f writes/s to one %s key, each counter item takes ~%.0f transactions/s: occasional conflicts, retried.",
				w.HotKeyRate, c.Name, perItem)})
		}
	}
	perKey := w.HotKeyRate * tx * wru(er.Item.P50)
	if perKey > partitionWCU/2 {
		r.Findings = append(r.Findings, Finding{Warn, subject, fmt.Sprintf("at %.0f writes/s to one partition key the partition takes about %.0f WRU/s of its %d.", w.HotKeyRate, perKey, partitionWCU)})
	}
}

func predFields(ps []*schema.Pred) []*schema.Field {
	var out []*schema.Field
	for _, p := range ps {
		out = append(out, p.Field)
	}
	return out
}

// ItemSize estimates the stored size of an entity item: attribute names plus values, keys and
// the bookkeeping attributes.
func ItemSize(m *schema.Model, e *schema.Entity) Size {
	s := attrSize(schema.AttrPK, templateSize(e.PK)).add(attrSize(schema.AttrSK, templateSize(e.SK)))
	s = s.add(metaSize(e)).add(attrSize(schema.AttrRev, Size{3, 5}))
	s = s.add(fieldsSize(e.Fields))
	for _, ix := range e.Indexes {
		if ix.Strategy == schema.StrategyGSI {
			s = s.add(indexKeysSize(ix))
		}
	}
	if e.TTL != nil {
		s = s.add(attrSize(m.Table.TTLAttr, Size{6, 6}))
	}
	return s
}

func metaSize(e *schema.Entity) Size {
	return attrSize(schema.AttrType, Size{len(e.Name), len(e.Name)}).add(attrSize(schema.AttrVer, Size{2, 2}))
}

func indexKeysSize(ix *schema.Index) Size {
	s := attrSize(ix.PKAttr, templateSize(ix.PK))
	if ix.HasSK {
		s = s.add(attrSize(ix.SKAttr, templateSize(ix.SK)))
	}
	return s
}

func fieldsSize(fs []*schema.Field) Size {
	var s Size
	for _, f := range fs {
		s = s.add(attrSize(f.Attr, Size{f.SizeP50, f.SizeP99}))
	}
	return s
}

func attrSize(name string, v Size) Size {
	return Size{len(name) + v.P50, len(name) + v.P99}
}

// templateSize estimates a rendered key's length.
func templateSize(t schema.Template) Size {
	var s Size
	fields := map[string]*schema.Field{}
	for _, f := range t.Fields {
		fields[f.Name] = f
	}
	for _, seg := range t.Segments {
		if !seg.IsField() {
			s = s.add(Size{len(seg.Literal), len(seg.Literal)})
			continue
		}
		f := fields[seg.Field]
		switch f.Type {
		case schema.TypeTime:
			s = s.add(Size{30, 30})
		case schema.TypeInt:
			s = s.add(Size{19, 20})
		case schema.TypeBool:
			s = s.add(Size{5, 5})
		default:
			s = s.add(Size{f.SizeP50, f.SizeP99})
		}
	}
	return s
}

func wru(bytes int) float64 { return math.Max(1, math.Ceil(float64(bytes)/1024)) }

func rru(bytes int, factor float64) float64 {
	return math.Max(1, math.Ceil(float64(bytes)/4096)) * factor
}

func human(b int) string {
	switch {
	case b >= 1024*1024:
		return fmt.Sprintf("%.1f MB", float64(b)/(1024*1024))
	case b >= 1024:
		return fmt.Sprintf("%.1f KB", float64(b)/1024)
	}
	return fmt.Sprintf("%d B", b)
}

// Human formats a byte count.
func Human(b int) string { return human(b) }

func boundedNote(c *schema.Counter) string {
	for _, v := range c.Values {
		if v.Limited() || v.HasMin {
			return ", or move bounded values (which cannot be sharded) to a counter of their own"
		}
	}
	return ""
}

// largeFieldFindings flags entities whose big fields make every write expensive, including writes
// that never change them: DynamoDB bills a write by the size of the whole item.
func largeFieldFindings(r *Report, m *schema.Model, e *schema.Entity, er *EntityReport) {
	var big []*schema.Field
	for _, f := range e.Fields {
		if f.SizeP99 >= 8*1024 {
			big = append(big, f)
		}
	}
	if len(big) == 0 {
		return
	}
	var others []string
	for _, w := range e.Writes {
		if w.Kind != schema.WriteUpdate {
			continue
		}
		touches := false
		for _, f := range w.Changed() {
			for _, b := range big {
				touches = touches || f == b
			}
		}
		if !touches {
			others = append(others, w.Name)
		}
	}
	// Other entities' writes that change this one through requires rewrite it too.
	for _, oe := range m.Entities {
		for _, w := range oe.Writes {
			for _, rq := range w.Requires {
				if rq.Target != e || len(rq.Sets) == 0 {
					continue
				}
				touches := false
				for _, st := range rq.Sets {
					for _, b := range big {
						touches = touches || st.Field == b
					}
				}
				if !touches {
					others = append(others, oe.Name+"."+w.Name)
				}
			}
		}
	}
	if len(others) == 0 {
		return
	}
	var names []string
	for _, f := range big {
		names = append(names, fmt.Sprintf("%s (p99 %s)", f.Name, human(f.SizeP99)))
	}
	verb, them := "makes", "it"
	if len(big) > 1 {
		verb, them = "make", "them"
	}
	r.Findings = append(r.Findings, Finding{Warn, e.Name, fmt.Sprintf(
		"%s %s every write cost up to %.0f WRU, including writes that never change %s: %s. Consider moving %s to an entity of its own, written only when %s changes.",
		strings.Join(names, ", "), verb, wru(er.Item.P99), them, strings.Join(others, ", "), them, them)})
}

// clears reports whether the write sets one of fields to a zero constant, which releases a claim
// rather than moving it.
func clears(w *schema.Write, fields []*schema.Field) bool {
	for _, st := range w.Sets {
		for _, f := range fields {
			if st.Field == f && fmt.Sprint(st.Value) == "" {
				return true
			}
		}
	}
	return false
}
