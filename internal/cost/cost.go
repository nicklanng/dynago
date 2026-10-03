// Package cost estimates item sizes, capacity units and monthly cost for a schema. Every number is
// derived from declared assumptions (field sizes, volumes, request rates), which the report lists
// alongside the results. The analysis package turns these numbers into findings.
package cost

import (
	"fmt"
	"math"

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

// DynamoDB's limits and accounting constants.
const (
	// IndexOverhead is the per-item overhead DynamoDB adds to index and item storage.
	IndexOverhead = 100
	// PartitionWCU and PartitionRCU are the throughput of one partition per second.
	PartitionWCU = 1000
	PartitionRCU = 3000
	// MaxItemSize is DynamoDB's item size limit.
	MaxItemSize = 400 * 1024
	// MaxTxItems and MaxTxBytes are DynamoDB's transaction limits.
	MaxTxItems = 100
	MaxTxBytes = 4 * 1024 * 1024

	secondsPerMonth = 30 * 24 * 3600
)

// Size is an estimated size in bytes at the median and 99th percentile.
type Size struct{ P50, P99 int }

func (s Size) add(o Size) Size { return Size{s.P50 + o.P50, s.P99 + o.P99} }

// Units is a capacity estimate at the median and 99th percentile item sizes.
type Units struct{ P50, P99 float64 }

func (u Units) add(o Units) Units { return Units{u.P50 + o.P50, u.P99 + o.P99} }

// Count is how many items of one kind a write touches, typically (P50) and at worst (P99).
type Count struct {
	P50, P99 int
	// Spread says the items are on different partition keys (a key moved, or one claim per
	// element), so each key takes one of them rather than all.
	Spread bool
	// Chance is the share of calls that touch the items at all, when it is less than every call:
	// entries of a sparse index exist only for the items that match its where. 0 means every call.
	Chance float64
}

// chance is the share of calls that touch the items.
func (c Count) chance() float64 {
	if c.Chance > 0 {
		return c.Chance
	}
	return 1
}

// maybe returns the count for a share of the calls.
func (c Count) maybe(chance float64) Count {
	if chance < 1 {
		c.Chance = chance
	}
	return c
}

var (
	once  = Count{P50: 1, P99: 1}
	twice = Count{P50: 2, P99: 2}
	// apart is two items on two partition keys: an entry or counter whose partition key moved.
	apart = Count{P50: 2, P99: 2, Spread: true}
)

// TargetKind is a kind of stored item.
type TargetKind string

// Target kinds.
const (
	TargetItem    TargetKind = "item"
	TargetGSI     TargetKind = "gsi"
	TargetCopy    TargetKind = "copy"
	TargetClaim   TargetKind = "claim"
	TargetCounter TargetKind = "counter"
)

// Target names a family of stored items: an entity's items, its entries in an index, its copies,
// its claims, or a counter's items.
type Target struct {
	Kind    TargetKind
	Entity  *schema.Entity
	Index   *schema.Index
	Unique  *schema.Unique
	Counter *schema.Counter
}

// Touch is one family of items a write puts, updates, deletes or checks.
type Touch struct {
	Target Target
	Label  string
	// Check marks a condition check: billed as a write of the item, but nothing changes.
	Check bool
	// Async marks GSI entries, which DynamoDB writes after the write, outside any transaction.
	Async bool
	Size  Size
	Count Count
	// Units is the WRU the touch costs, before the transaction doubling.
	Units Units
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
	// Reads are the item families the call reads.
	Reads []Target
	// FullPassRRU is, for a scan, the RRU of reading the whole table once.
	FullPassRRU float64
}

// WriteCost is the estimated cost of one write call.
type WriteCost struct {
	Write         *schema.Write
	Items         []string
	Touches       []Touch
	ReadFirst     bool
	Transactional bool
	MaxTxItems    int
	// TxBytes is the p99 size of everything the write's own request writes.
	TxBytes int
	WRU     Units
	RRU     float64
	Monthly float64
}

// EntityReport is the estimate for one entity.
type EntityReport struct {
	Entity  *schema.Entity
	Item    Size
	Indexes map[string]Size
	Items   []ItemCost
	Reads   []ReadCost
	Writes  []WriteCost
	// StorageBytes counts the entity's items, index entries and claims at the declared volume.
	StorageBytes float64
	// BaseBytes and BaseItems count what the entity puts in the base table (items, copies and
	// claims, without GSI entries or storage overhead): what a Scan reads.
	BaseBytes, BaseItems float64
	StorageGB            float64
	StorageUSD           float64
}

// Report is the estimate for a whole schema.
type Report struct {
	Prices      Prices
	Entities    []*EntityReport
	Assumptions []string
	// StorageBytes is the whole table, from the entities with a known volume.
	StorageBytes float64
	// BaseBytes and BaseItems are the base table's items (without GSIs): what a Scan reads.
	BaseBytes, BaseItems float64
	MonthlyUSD           float64
}

// Entity returns the report of an entity.
func (r *Report) Entity(e *schema.Entity) *EntityReport {
	for _, er := range r.Entities {
		if er.Entity == e {
			return er
		}
	}
	return &EntityReport{Entity: e, Indexes: map[string]Size{}}
}

// Analyze estimates sizes, capacity per call and monthly costs.
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
		"Storage counts each entity's items, index entries and claims at its declared volume, plus 100 bytes of overhead per item. A scan reads the base table's items, copies and claims, without GSI entries or overhead; counter items aren't counted.",
	)
	for _, e := range m.Entities {
		er := analyzeEntity(r, m, e)
		r.Entities = append(r.Entities, er)
		r.StorageBytes += er.StorageBytes
		r.BaseBytes += er.BaseBytes
		r.BaseItems += er.BaseItems
	}
	// A scan reads the whole table, which is known once every entity is: its full pass, and a
	// page, which reads page items of every entity (the filter to one entity applies after).
	for _, er := range r.Entities {
		for i := range er.Reads {
			rc := &er.Reads[i]
			if rc.Access.Kind == schema.AccessScan {
				rc.FullPassRRU = math.Ceil(r.BaseBytes/4096) * readFactor(rc.Access)
				if r.BaseItems > 0 {
					page := rru(int(math.Ceil(r.BaseBytes/r.BaseItems))*rc.Access.Page, readFactor(rc.Access))
					rc.RRU = Units{page, math.Max(page, rc.RRU.P99)}
				}
			}
		}
	}
	for _, er := range r.Entities {
		r.MonthlyUSD += er.StorageUSD
		for _, rc := range er.Reads {
			r.MonthlyUSD += rc.Monthly
		}
		for _, wc := range er.Writes {
			r.MonthlyUSD += wc.Monthly
		}
	}
	return r
}

// EntitySizes estimates the sizes of an entity's item, index entries, claims and counters.
func EntitySizes(m *schema.Model, e *schema.Entity) *EntityReport {
	er := &EntityReport{Entity: e, Indexes: map[string]Size{}}
	item := ItemSize(m, e)
	er.Item = item
	er.Items = append(er.Items, ItemCost{Name: e.Name, Kind: "entity", Size: item})
	keys := attrSize(schema.AttrPK, templateSize(e.PK)).add(attrSize(schema.AttrSK, templateSize(e.SK)))
	meta := metaSize(e)

	for _, ix := range e.Indexes {
		var s Size
		switch {
		case ix.Strategy == schema.StrategyGSI && ix.Projection == schema.ProjectAll:
			s = item.add(indexKeysSize(ix))
		case ix.Strategy == schema.StrategyGSI:
			s = keys.add(indexKeysSize(ix)).add(fieldsSize(ix.ProjectedFields())).add(meta)
		case ix.Projection == schema.ProjectAll: // a copy of the whole item
			s = attrSize(schema.AttrPK, templateSize(ix.PK)).add(attrSize(schema.AttrSK, templateSize(ix.SK))).add(fieldsSize(e.Fields)).add(meta).add(stampsSize)
		default: // copy
			s = attrSize(schema.AttrPK, templateSize(ix.PK)).add(attrSize(schema.AttrSK, templateSize(ix.SK))).add(fieldsSize(ix.ProjectedFields())).add(meta).add(stampsSize)
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
		s := CounterSize(c)
		er.Items = append(er.Items, ItemCost{Name: c.Name, Kind: "counter", Size: s})
		if c.Shards > 1 {
			er.Items[len(er.Items)-1].Name += fmt.Sprintf(" (×%d shards)", c.Shards)
		}
	}
	return er
}

// CounterSize estimates the size of one counter item.
func CounterSize(c *schema.Counter) Size {
	s := attrSize(schema.AttrPK, templateSize(c.PK)).add(attrSize(schema.AttrSK, templateSize(c.SK))).
		add(attrSize("_t", Size{len(c.Name), len(c.Name)})).add(stampsSize)
	for _, v := range c.Values {
		s = s.add(attrSize(v.Attr, Size{8, 11}))
	}
	return s
}

func analyzeEntity(r *Report, m *schema.Model, e *schema.Entity) *EntityReport {
	er := EntitySizes(m, e)
	for _, a := range e.Access {
		rc := ReadCostOf(a, er)
		if a.Rate > 0 {
			rc.Monthly = (rc.RRU.P50 * a.Rate * secondsPerMonth / 1e6) * r.Prices.RRUPerMillion
		}
		er.Reads = append(er.Reads, rc)
	}
	for _, w := range e.Writes {
		wc := WriteCostOf(m, e, w, w.ReadFirst && !w.Transition, er)
		if w.Rate > 0 {
			wc.Monthly = (wc.WRU.P50*r.Prices.WRUPerMillion + wc.RRU*r.Prices.RRUPerMillion) * w.Rate * secondsPerMonth / 1e6
		}
		er.Writes = append(er.Writes, wc)
	}
	if n := e.Count; n > 0 {
		bytes := n * float64(er.Item.P50+IndexOverhead)
		er.BaseBytes, er.BaseItems = n*float64(er.Item.P50), n
		for _, ix := range e.Indexes {
			// A sparse index holds the share of the items its where matches, when declared.
			in := n * ix.Share()
			bytes += in * float64(er.Indexes[ix.Name].P50+IndexOverhead)
			if ix.Strategy == schema.StrategyCopy {
				er.BaseBytes += in * float64(er.Indexes[ix.Name].P50)
				er.BaseItems += in
			}
		}
		for _, u := range e.Uniques {
			claims := 1.0
			if u.Set != nil {
				claims = float64(setElements(u.Set).P50)
			}
			bytes += n * claims * float64(claimSize.P50+IndexOverhead)
			er.BaseBytes += n * claims * float64(claimSize.P50)
			er.BaseItems += n * claims
		}
		er.StorageBytes = bytes
		er.StorageGB = bytes / (1 << 30)
		er.StorageUSD = er.StorageGB * r.Prices.GBMonth
	}
	return er
}

func readFactor(a *schema.Access) float64 {
	if a.Consistent {
		return 1
	}
	return 0.5
}

// ReadCostOf estimates one call of an access pattern.
func ReadCostOf(a *schema.Access, er *EntityReport) ReadCost {
	rc := ReadCost{Access: a, RoundTrips: 1}
	factor := readFactor(a)
	e := a.Entity
	switch a.Kind {
	case schema.AccessGet:
		rc.Requests = "GetItem"
		rc.RRU = Units{rru(er.Item.P50, factor), rru(er.Item.P99, factor)}
		if a.Batch > 0 {
			// Each item of a batch is charged as a GetItem of it would be.
			rc.Requests = fmt.Sprintf("BatchGetItem (%d keys)", a.Batch)
			rc.RoundTrips = (a.Batch + 99) / 100
			rc.RRU = Units{rc.RRU.P50 * float64(a.Batch), rc.RRU.P99 * float64(a.Batch)}
		}
		rc.Reads = []Target{{Kind: TargetItem, Entity: e}}
	case schema.AccessGetUnique:
		rc.Requests = "GetItem (claim) → GetItem"
		rc.RoundTrips = 2
		rc.RRU = Units{1 + rru(er.Item.P50, 1), 1 + rru(er.Item.P99, 1)}
		rc.Reads = []Target{{Kind: TargetClaim, Entity: e, Unique: a.Unique}, {Kind: TargetItem, Entity: e}}
	case schema.AccessQuery:
		entry := er.Item
		t := Target{Kind: TargetItem, Entity: e}
		if a.Index != nil {
			entry = er.Indexes[a.Index.Name]
			t = Target{Kind: TargetGSI, Entity: e, Index: a.Index}
			if a.Index.Strategy == schema.StrategyCopy {
				t.Kind = TargetCopy
			}
		}
		rc.Requests = "Query"
		rc.RRU = Units{rru(entry.P50*a.Page, factor), rru(entry.P99*a.Page, factor)}
		rc.Reads = []Target{t}
	case schema.AccessScan:
		rc.Requests = "Scan (one page)"
		rc.RRU = Units{rru(er.Item.P50*a.Page, factor), rru(er.Item.P99*a.Page, factor)}
		rc.Reads = []Target{{Kind: TargetItem, Entity: e}}
	case schema.AccessCounter:
		if a.All {
			size := CounterSize(a.Counter)
			rc.Requests = "Query"
			rc.RRU = Units{rru(size.P50*a.Page, factor), rru(size.P99*a.Page, factor)}
			rc.Reads = []Target{{Kind: TargetCounter, Entity: a.Counter.Entity, Counter: a.Counter}}
			break
		}
		if a.Counter.Shards > 1 {
			rc.Requests = fmt.Sprintf("BatchGetItem (%d shards)", a.Counter.Shards)
		} else {
			rc.Requests = "GetItem"
		}
		n := float64(a.Counter.Shards) * factor
		rc.RRU = Units{n, n}
		rc.Reads = []Target{{Kind: TargetCounter, Entity: a.Counter.Entity, Counter: a.Counter}}
	}
	return rc
}

// WriteCostOf estimates one call of a write of e (which may be a variant of w.Entity with other
// indexes, for comparing designs). readFirst says whether the write reads the item first.
func WriteCostOf(m *schema.Model, e *schema.Entity, w *schema.Write, readFirst bool, er *EntityReport) WriteCost {
	wc := WriteCost{Write: w, ReadFirst: readFirst}
	// gsi holds index entries DynamoDB writes asynchronously, outside any transaction. own holds
	// the items the write itself puts, updates, deletes or checks.
	var gsi, own Units
	ops := 0
	write := func(t Target, check, async bool, name string, s Size, n Count) {
		dst := &own
		if async {
			dst = &gsi
		} else {
			ops += n.P99
			if !check {
				wc.TxBytes += n.P99 * s.P99
			}
		}
		// Typically, only the calls that touch the items pay for them; at worst, every one does.
		u := Units{float64(n.P50) * wru(s.P50) * n.chance(), float64(n.P99) * wru(s.P99)}
		*dst = dst.add(u)
		wc.Items = append(wc.Items, name)
		wc.Touches = append(wc.Touches, Touch{Target: t, Label: name, Check: check, Async: async, Size: s, Count: n, Units: u})
	}
	write(Target{Kind: TargetItem, Entity: e}, false, false, e.Name, er.Item, once)
	derivedWrites(e, w, er, "", write)
	if wc.ReadFirst {
		wc.RRU = rru(er.Item.P50, 1)
	}
	for _, rq := range w.Requires {
		switch {
		case rq.Counter != nil:
			// A condition check on another item is billed as a transactional write of that item.
			write(Target{Kind: TargetCounter, Entity: rq.Counter.Entity, Counter: rq.Counter}, true, false, "check counter "+rq.Counter.Name, CounterSize(rq.Counter), once)
		case rq.Writes():
			ter := EntitySizes(m, rq.Target)
			tw := rq.TargetWrite()
			label := rq.Name + " (" + rq.Effect(e.Name) + ")"
			if rq.Consume {
				tw = &schema.Write{Name: "requires", Entity: rq.Target, Kind: schema.WriteDelete}
			}
			write(Target{Kind: TargetItem, Entity: rq.Target}, false, false, label, ter.Item, once)
			derivedWrites(rq.Target, tw, ter, rq.Name+"'s ", write)
			if !rq.Fast {
				wc.RRU += rru(ter.Item.P50, 1)
			}
		default:
			write(Target{Kind: TargetItem, Entity: rq.Target}, true, false, "check "+rq.Name, ItemSize(m, rq.Target), once)
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

// writeFunc records one family of items a write touches: check marks condition checks, async
// marks GSI entries, which DynamoDB writes outside the transaction.
type writeFunc func(t Target, check, async bool, name string, s Size, n Count)

// setElementBytes is the assumed size of one element of a string set, for estimating how many
// claims a set makes from the set's declared size.
const setElementBytes = 20

// setElements estimates the number of elements of a string set field.
func setElements(f *schema.Field) Count {
	n := func(size int) int { return max(1, (size+setElementBytes-1)/setElementBytes) }
	return Count{P50: n(f.SizeP50), P99: n(f.SizeP99), Spread: true}
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
		keys := append(append([]*schema.Field{}, ix.PK.Fields...), ix.SK.Fields...)
		projected := ix.Projection == schema.ProjectAll || touches(ix.ProjectedFields())
		size := er.Indexes[ix.Name]
		t, entry, moved, async := Target{Kind: TargetCopy, Entity: e, Index: ix}, owner+"copy "+ix.Name, " (moved: put + delete)", false
		if ix.Strategy == schema.StrategyGSI {
			t, entry, moved, async = Target{Kind: TargetGSI, Entity: e, Index: ix}, owner+"GSI "+ix.Name+" entry", " (moved: delete + put)", true
		}
		// A sparse index has an entry only while the item matches its where: the write's `when`
		// and `set` may say whether it does, and otherwise the declared share of items does.
		before, after := ix.Members(w)
		chance := func(m schema.Membership) float64 {
			if m == schema.Maybe {
				return ix.Share()
			}
			return 1
		}
		switch {
		case before == schema.No && after == schema.No:
			// Never in the index: nothing to write.
		case before == schema.No:
			if all {
				write(t, false, async, entry, size, once.maybe(chance(after)))
			} else {
				write(t, false, async, entry+" (added)", size, once.maybe(chance(after)))
			}
		case after == schema.No:
			if all {
				write(t, false, async, entry, size, once.maybe(chance(before)))
			} else {
				write(t, false, async, entry+" (removed)", size, once.maybe(chance(before)))
			}
		case touches(keys) || (touches(predFields(ix.Where)) && (before == schema.Maybe || after == schema.Maybe)):
			// Its keys change, or it may enter or leave the index: the old entry goes and the
			// new one is written, each only if the item matches at that moment.
			n := twice
			if touches(ix.PK.Fields) {
				n = apart
			}
			write(t, false, async, entry+moved, size, n.maybe((chance(before)+chance(after))/2))
		case projected:
			write(t, false, async, entry, size, once.maybe(chance(before)))
		}
	}
	for _, c := range e.Counters {
		t := Target{Kind: TargetCounter, Entity: e, Counter: c}
		in := [][]*schema.Field{c.KeyFields()}
		for _, v := range c.Values {
			in = append(in, predFields(v.Where))
			if v.Sum != nil {
				in = append(in, []*schema.Field{v.Sum})
			}
		}
		size := CounterSize(c)
		switch {
		case all:
			write(t, false, false, owner+"counter "+c.Name, size, once)
		case touches(c.KeyFields()):
			movedN := twice
			if touches(c.PK.Fields) {
				movedN = apart
			}
			write(t, false, false, owner+"counter "+c.Name+" (moved: two counter items)", size, movedN)
		case touches(in...):
			write(t, false, false, owner+"counter "+c.Name, size, once)
		}
	}
	for _, u := range e.Uniques {
		t := Target{Kind: TargetClaim, Entity: e, Unique: u}
		if u.Set != nil {
			// One claim per element: all of them on a create or delete. An update claims the
			// elements it adds and releases those it drops: typically one of each, at worst all.
			n := setElements(u.Set)
			switch {
			case all:
				write(t, false, false, fmt.Sprintf("%sclaims %s (one per %s element)", owner, u.Name, u.Set.Name), claimSize, n)
			case touches(u.Fields):
				write(t, false, false, fmt.Sprintf("%sclaims %s (added and dropped %s elements)", owner, u.Name, u.Set.Name), claimSize, Count{P50: 2, P99: 2 * n.P99, Spread: true})
			}
			continue
		}
		switch {
		case all:
			write(t, false, false, owner+"claim "+u.Name, claimSize, once)
		case touches(u.Fields) && clears(w, u.Fields):
			write(t, false, false, owner+"claim "+u.Name+" (released)", claimSize, once)
		case touches(u.Fields):
			write(t, false, false, owner+"claim "+u.Name+" (moved: put + delete)", claimSize, apart)
		}
	}
}

// ClaimSize is the size of one uniqueness claim item: its keys, type, owner's key and timestamps.
var ClaimSize = Size{230, 230}

var claimSize = ClaimSize

// stampsSize is the two timestamps dynago stores on every item it writes.
var stampsSize = attrSize(schema.AttrCreated, Size{30, 30}).add(attrSize(schema.AttrUpdated, Size{30, 30}))

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
	s = s.add(metaSize(e)).add(attrSize(schema.AttrRev, Size{3, 5})).add(stampsSize)
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

// WRU is the write units of writing an item of the given size.
func WRU(bytes int) float64 { return wru(bytes) }

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

// HumanBytes formats a large byte count: 3.1 GB, 540 MB.
func HumanBytes(b float64) string {
	switch {
	case b >= 1<<40:
		return fmt.Sprintf("%.1f TB", b/(1<<40))
	case b >= 1<<30:
		return fmt.Sprintf("%.1f GB", b/(1<<30))
	case b >= 1<<20:
		return fmt.Sprintf("%.0f MB", b/(1<<20))
	case b >= 1<<10:
		return fmt.Sprintf("%.0f KB", b/(1<<10))
	}
	return fmt.Sprintf("%.0f B", b)
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
