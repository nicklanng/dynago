package analysis

import (
	"fmt"
	"math"
	"slices"
	"strings"

	"github.com/nicklanng/dynago/internal/cost"
	"github.com/nicklanng/dynago/internal/schema"
)

// conflictRate is the rate of transactions per counter item above which conflicts become routine.
const conflictRate = 20

func (a *analyzer) rules() {
	m := a.m
	for _, e := range m.Entities {
		er := a.r.Cost.Entity(e)
		a.itemRules(e, er)
		a.ttlRules(e)
		a.indexRules(e)
		a.fieldRules(e)
		for i, w := range e.Writes {
			a.writeRules(w, er.Writes[i])
		}
		for i, ac := range e.Access {
			a.accessRules(ac, er.Reads[i])
		}
		a.largeFields(e, er)
		a.policyEntityRules(e)
	}
	for _, g := range m.GSIs {
		if len(g.Users) > 1 {
			var names []string
			for _, ix := range g.Users {
				names = append(names, ix.Entity.Name)
			}
			a.add("shared-gsi", Note, indexSubject(g.Users[0]),
				"GSI %s is shared by %s; its projection is the union of what each needs, so every entity pays for the others' projected attributes.", g.Name, strings.Join(names, ", "))
		}
	}
	if n := a.r.Policy.Limits.GSIs; n > 0 && len(m.GSIs) > n {
		a.add("gsi-limit", Warning, schema.Subject{Kind: schema.SubjectTable}, "the table has %d GSIs; the policy allows %d.", len(m.GSIs), n)
	}
	a.counterLoadRules()
	a.partitionRules()
}

func (a *analyzer) itemRules(e *schema.Entity, er *cost.EntityReport) {
	item := er.Item
	switch {
	case item.P99 > cost.MaxItemSize:
		a.add("item-too-large", Error, entitySubject(e), "p99 item size is %s, over DynamoDB's 400 KB limit; move large fields to S3 or split the item.", cost.Human(item.P99))
	case item.P99 > 100*1024:
		a.add("item-large", Warning, entitySubject(e), "p99 item size is %s; every write of it costs %.0f+ WRU, and every strongly consistent read %.0f RRU. Consider splitting rarely-read fields into a separate item.", cost.Human(item.P99), math.Ceil(float64(item.P99)/1024), math.Ceil(float64(item.P99)/4096))
	}
	if n := a.r.Policy.Limits.ItemSize; n > 0 && item.P99 > n {
		a.add("item-size-limit", Warning, entitySubject(e), "p99 item size is %s; the policy allows %s.", cost.Human(item.P99), cost.Human(n))
	}
}

func (a *analyzer) ttlRules(e *schema.Entity) {
	if e.TTL == nil {
		return
	}
	// TTL deletes bypass the generated code, so nothing releases what the item contributed.
	if len(e.Counters) > 0 {
		a.add("ttl-counter", Warning, entitySubject(e), "expires by TTL (%s) and feeds counters: an expired item is never subtracted, so the counters only ever count items created, not items that currently exist.", e.TTL.Name)
	}
	if len(e.Uniques) > 0 {
		a.add("ttl-claim", Warning, entitySubject(e), "expires by TTL (%s) but holds unique claims: an expired item's claims stay behind and keep the value taken.", e.TTL.Name)
	}
	for _, ix := range e.Indexes {
		if ix.Strategy == schema.StrategyCopy {
			a.add("ttl-copy", Warning, indexSubject(ix), "copies of a TTL-expiring %s outlive it; give the copies their own expiry or use a GSI.", e.Name)
		}
	}
}

func (a *analyzer) indexRules(e *schema.Entity) {
	er := a.r.Cost.Entity(e)
	for _, ix := range e.Indexes {
		s := indexSubject(ix)
		var readers []*schema.Access
		for _, ac := range e.Access {
			if ac.Index == ix {
				readers = append(readers, ac)
			}
		}
		if len(readers) == 0 {
			what := "every write that creates, deletes or moves an entry pays for it"
			if ix.Strategy == schema.StrategyCopy {
				what = "every write that touches it is a transaction because of it"
			}
			a.add("unused-index", Warning, s, "no declared read uses %s, yet %s. Declare the read that needs it, or remove it.", ix.Name, what)
		}
		if ix.Strategy == schema.StrategyGSI && ix.Projection == schema.ProjectAll {
			a.add("project-all", Note, s, "projects ALL: every write of a %s stores and writes the whole item (%s) a second time. List fields only (project: [...]) usually suffice.", e.Name, cost.Human(er.Item.P50))
		}
		// An optional field in the key drops items from the index without anything saying so.
		var optional []string
		for _, f := range templateFields(ix) {
			if !f.Key && !f.Required && f.Sparse() && !slices.Contains(optional, f.Name) && !pinned(ix.Where, f) {
				optional = append(optional, f.Name)
			}
		}
		if len(optional) > 0 {
			var reads []string
			for _, ac := range readers {
				reads = append(reads, ac.Name)
			}
			also := ""
			if len(reads) > 0 {
				also = " and from " + join(reads)
				if len(reads) == 1 && reads[0] == ix.Name {
					also = " and from the read " + reads[0]
				}
			}
			a.add("sparse-index", Warning, s, "is keyed by %s, which %s optional: a %s without %s is missing from %s%s, with nothing to say so. Make the field required, or accept this if it's deliberate.",
				join(optional), isAre(len(optional)), e.Name, pronoun(len(optional)), ix.Name, also)
		}
		// A copy index exists for read-your-writes; if no read needs it, a GSI is cheaper.
		if ix.Strategy == schema.StrategyCopy && len(readers) > 0 {
			eventual := true
			for _, ac := range readers {
				eventual = eventual && ac.Freshness == schema.FreshnessEventual
			}
			if eventual {
				saving := ""
				if alt := a.r.Alternatives[ix]; alt != nil && alt.Feasible {
					saving = " " + alt.WriteSummary()
				}
				a.add("copy-not-needed", Note, s, "is a copy, which makes writes that touch it transactions, but every read through it accepts eventual freshness. A GSI would do.%s", saving)
			}
		}
	}
	if n := a.r.Policy.Limits.IndexesPerEntity; n > 0 && len(e.Indexes) > n {
		a.add("index-limit", Warning, entitySubject(e), "has %d indexes; the policy allows %d per entity.", len(e.Indexes), n)
	}
	// A partition key with nothing but constants and enums puts all of an entity's items in a few
	// partitions.
	check := func(s schema.Subject, what string, pk schema.Template, many bool) {
		if !many {
			return
		}
		for _, f := range pk.Fields {
			if f.Type != schema.TypeEnum && f.Type != schema.TypeBool {
				return
			}
		}
		n := 1
		for _, f := range pk.Fields {
			if f.Type == schema.TypeEnum {
				n *= len(f.Enum)
			} else {
				n *= 2
			}
		}
		where := "one partition"
		if n > 1 {
			where = fmt.Sprintf("at most %d partitions", n)
		}
		// With volumes and rates declared, the partition estimate says whether it matters: a list
		// of every tenant is one small, quiet partition, and the right design.
		if p := a.partitionOf(s); p != nil && p.Risk == RiskLow && p.Size.MaxKnown {
			grows := ""
			if p.Grows {
				grows = " (and grows as long as the table lives)"
			}
			a.add("low-cardinality-key", Note, s, "%s partition key %q has no field that varies per item beyond enums: every %s lands in %s. At the declared volumes and rates that's fine: the largest holds %s%s, and its busiest key takes %s of a partition's capacity at peak. Revisit it if the volumes or rates grow by orders of magnitude.",
				what, pk.Raw, e.Name, where, cost.HumanBytes(p.Size.Max), grows, headroomText(p.Headroom))
			return
		}
		unknown := ""
		if p := a.partitionOf(s); p == nil || p.Risk == RiskUnknown || !p.Size.MaxKnown {
			unknown = " Declare the volume and rates to see whether that matters here."
		}
		a.add("low-cardinality-key", Warning, s, "%s partition key %q has no field that varies per item beyond enums: every %s lands in %s, however many there are. Add an id (a tenant, a parent) to the key.%s", what, pk.Raw, e.Name, where, unknown)
	}
	check(entitySubject(e), e.Name+"'s", e.PK, len(minus(e.KeyFields(), e.PK.Fields)) > 0)
	for _, ix := range e.Indexes {
		check(indexSubject(ix), ix.Name+"'s", ix.PK, true)
	}
}

// templateFields returns an index's key fields, partition key first.
func templateFields(ix *schema.Index) []*schema.Field {
	out := append([]*schema.Field{}, ix.PK.Fields...)
	if ix.HasSK {
		out = append(out, ix.SK.Fields...)
	}
	return out
}

// pinned reports whether a where predicate fixes the field to a value, which an empty field
// doesn't have: the index is sparse on purpose.
func pinned(ws []*schema.Pred, f *schema.Field) bool {
	for _, p := range ws {
		if p.Field == f {
			return true
		}
	}
	return false
}

func (a *analyzer) fieldRules(e *schema.Entity) {
	for _, f := range e.Fields {
		if f.CopyOf == nil {
			continue
		}
		src := f.CopyOfEntity
		fan := ""
		// How many items hold one source item's value decides whether one write can update them.
		if key, ok := schema.HoldsKey(e, src); ok {
			n := a.perKey(e, key)
			switch {
			case n.MaxKnown && n.Max <= 1:
				fan = fmt.Sprintf(" Each %s is copied into at most one %s, so the change could be made in the same transaction.", src.Name, e.Name)
			case n.MaxKnown && n.Max < cost.MaxTxItems:
				fan = fmt.Sprintf(" Each %s is copied into up to %s %s: one transaction could update them all (up to %s items).", src.Name, schema.Number(n.Max), schema.Plural(e.Name), schema.Number(n.Max+1))
			case n.MaxKnown:
				fan = fmt.Sprintf(" Each %s is copied into up to %s %s: more than one transaction holds, so the update can't be atomic, and a background job must do it.", src.Name, schema.Number(n.Max), schema.Plural(e.Name))
			case n.Known:
				fan = fmt.Sprintf(" Each %s is copied into about %s %s (the most isn't declared).", src.Name, schema.Number(n.Typical), schema.Plural(e.Name))
			}
		}
		a.add("copy-drift", Warning, fieldSubject(e, f),
			"copies %s.%s and must stay equal to it: when %s.%s changes, your code must rewrite every %s that copied it.%s If the value should stay as it was when the %s was written, declare snapshot_of instead.",
			src.Name, f.CopyOf.Name, src.Name, f.CopyOf.Name, e.Name, fan, e.Name)
	}
}

func (a *analyzer) writeRules(w *schema.Write, wc cost.WriteCost) {
	s := writeSubject(w)
	if wc.MaxTxItems > cost.MaxTxItems {
		a.add("transaction-too-many-items", Error, s, "can touch %d items, over the 100-item transaction limit.", wc.MaxTxItems)
	}
	if wc.TxBytes > cost.MaxTxBytes {
		a.add("transaction-too-large", Error, s, "can write %s in one transaction (p99 sizes), over DynamoDB's 4 MB limit.", cost.Human(wc.TxBytes))
	}
	if n := a.r.Policy.Limits.TransactionItems; n > 0 && wc.MaxTxItems > n {
		a.add("transaction-items-limit", Warning, s, "can touch %d items in one transaction; the policy allows %d.", wc.MaxTxItems, n)
	}
	if a.r.Policy.Require.Rates && w.Rate <= 0 {
		a.add("rate-missing", Warning, s, "declares no rate, so neither its cost nor its load on partitions is estimated.")
	}
	a.counterRules(w, wc)
}

// counterLoad is what every write puts on one counter's busiest item at peak.
type counterLoad struct {
	c       *schema.Counter
	rate    float64 // writes/s to the busiest key, over every write
	wru     float64 // WRU/s on one item
	tx      float64 // transactions/s on one item
	writers []string
	anyTx   bool
}

// counterRules records how hard a write hits each counter item: at the declared hot_key_rate, or
// at the busiest counter key's share of its peak rate. counterLoadRules judges the sums, since
// every write to a counter item contends with the others.
func (a *analyzer) counterRules(w *schema.Write, wc cost.WriteCost) {
	tx := 1.0
	if wc.Transactional {
		tx = 2
	}
	seen := map[*schema.Counter]bool{}
	for _, t := range wc.Touches {
		c := t.Target.Counter
		if t.Target.Kind != cost.TargetCounter || t.Check || seen[c] {
			continue
		}
		seen[c] = true
		rate, basis := w.HotKeyRate, "declared hot_key_rate"
		if rate <= 0 {
			mb := a.r.MemberOf(t.Target)
			sh, ok := share(mb)
			if !ok || w.Rate <= 0 {
				continue
			}
			rate, basis = w.Rate*a.m.Workload.Peak*sh, "the busiest key's share of the peak rate"
		}
		l := a.counterLoads[c]
		if l == nil {
			l = &counterLoad{c: c}
			a.counterLoads[c] = l
			a.counterOrder = append(a.counterOrder, c)
		}
		perItem := rate / float64(c.Shards)
		l.rate += rate
		l.wru += perItem * tx * t.Units.P50 / float64(t.Count.P50)
		if wc.Transactional {
			l.tx += perItem
			l.anyTx = true
		}
		l.writers = append(l.writers, fmt.Sprintf("%s.%s %s/s, %s", w.Entity.Name, w.Name, schema.Number(round(rate)), basis))
	}
}

func (a *analyzer) counterLoadRules() {
	for _, c := range a.counterOrder {
		l := a.counterLoads[c]
		s := counterSubject(c)
		from := strings.Join(l.writers, "; ")
		suggest := int(math.Ceil(l.rate / 10))
		switch {
		case l.wru > cost.PartitionWCU:
			a.add("hot-counter", Error, s,
				"at %s writes/s to one %s key (%s), each counter item takes %.0f WRU/s, over a partition's %d: it will throttle. Set shards: %d or more.",
				schema.Number(round(l.rate)), c.Name, from, l.wru, cost.PartitionWCU, suggest)
		case l.anyTx && l.tx > conflictRate:
			a.add("counter-contention", Warning, s,
				"at %s writes/s to one %s key (%s), each counter item is written by ~%.0f transactions/s. Transactions touching the same item at once conflict and retry (about half a second by default; see dynago.SetRetries), so expect latency and ErrConflict under bursts. Set shards: %d (about 10 transactions/s per item)%s.",
				schema.Number(round(l.rate)), c.Name, from, l.tx, suggest, boundedNote(c))
		case l.anyTx && l.tx >= 5:
			a.add("counter-contention", Note, s,
				"at %s writes/s to one %s key (%s), each counter item takes ~%.0f transactions/s: occasional conflicts, retried.",
				schema.Number(round(l.rate)), c.Name, from, l.tx)
		}
	}
}

func round(f float64) float64 {
	if f >= 10 {
		return math.Round(f)
	}
	return math.Round(f*100) / 100
}

func boundedNote(c *schema.Counter) string {
	for _, v := range c.Values {
		if v.Limited() || v.HasMin {
			return ", or move bounded values (which cannot be sharded) to a counter of their own"
		}
	}
	return ""
}

func (a *analyzer) accessRules(ac *schema.Access, rc cost.ReadCost) {
	s := accessSubject(ac)
	e := ac.Entity
	if ac.Kind == schema.AccessScan {
		pass := ""
		if rc.FullPassRRU > 0 {
			pass = fmt.Sprintf(" At the declared volumes one full pass reads about %s, costing about %s RRU.",
				cost.HumanBytes(a.r.Cost.BaseBytes), schema.Number(math.Round(rc.FullPassRRU)))
		}
		a.add("scan", Note, s, "scans the whole table, every entity's items, for its %s, one page per call: %s.%s", schema.Plural(e.Name), strings.TrimSuffix(ac.Reason, "."), pass)
	}
	// A lookup of one entry through an index nothing keeps unique may find two.
	if ac.Kind == schema.AccessQuery && ac.Index != nil && ac.MaxPage == 1 && !ac.Index.HasSK {
		unique := false
		for _, u := range e.Uniques {
			in := map[*schema.Field]bool{}
			for _, f := range ac.Index.PK.Fields {
				in[f] = true
			}
			unique = unique || coversAll(in, u.Fields)
		}
		if !unique {
			a.add("unenforced-unique", Warning, indexSubject(ac.Index), "%s reads one entry of %s, keyed by %s, as if the key were unique, but no unique constraint makes it so: two %s with the same %s would both be there, and the read returns either. Declare unique: on the fields, or accept this if duplicates can't happen.",
				ac.Name, ac.Index.Name, join(names(ac.Index.PK.Fields)), schema.Plural(e.Name), join(names(ac.Index.PK.Fields)))
		}
	}
	p := a.r.Policy
	if p.Require.Freshness && ac.Freshness == schema.FreshnessUnstated {
		a.add("freshness-unstated", Warning, s, "doesn't state the freshness it needs (freshness: immediate or eventual).")
	}
	if p.Require.Rates && ac.Rate <= 0 {
		a.add("rate-missing", Warning, s, "declares no rate, so neither its cost nor its load on partitions is estimated.")
	}
}

func (a *analyzer) policyEntityRules(e *schema.Entity) {
	if a.r.Policy.Require.Volumes && !e.Volume.Declared {
		a.add("volume-missing", Warning, entitySubject(e), "declares no volume, so its storage, partition sizes and traffic aren't estimated.")
	}
}

// largeFields flags entities whose big fields make every write expensive, including writes that
// never change them: DynamoDB bills a write by the size of the whole item.
func (a *analyzer) largeFields(e *schema.Entity, er *cost.EntityReport) {
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
			touches = touches || slices.Contains(big, f)
		}
		if !touches {
			others = append(others, w.Name)
		}
	}
	// Other entities' writes that change this one through requires rewrite it too.
	for _, oe := range a.m.Entities {
		for _, w := range oe.Writes {
			for _, rq := range w.Requires {
				if rq.Target != e || len(rq.Sets) == 0 {
					continue
				}
				touches := false
				for _, st := range rq.Sets {
					touches = touches || slices.Contains(big, st.Field)
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
	var ns []string
	for _, f := range big {
		ns = append(ns, fmt.Sprintf("%s (p99 %s)", f.Name, cost.Human(f.SizeP99)))
	}
	verb, them := "makes", "it"
	if len(big) > 1 {
		verb, them = "make", "them"
	}
	a.add("large-field", Warning, entitySubject(e),
		"%s %s every write cost up to %.0f WRU (%.0f in a transaction), including writes that never change %s: %s. Consider moving %s to an entity of its own, written only when %s changes.",
		strings.Join(ns, ", "), verb, cost.WRU(er.Item.P99), 2*cost.WRU(er.Item.P99), them, strings.Join(others, ", "), them, them)
}

// partitionRules flags partitions whose busiest key comes close to DynamoDB's per-partition
// throughput, and partitions over the policy's size limit.
func (a *analyzer) partitionRules() {
	for _, p := range a.r.Partitions {
		if limit := a.r.Policy.Limits.PartitionSize; limit > 0 && p.Size.MaxKnown && p.Size.Max > limit {
			mb := largest(p)
			a.add("partition-size-limit", Warning, mb.Owner, "the largest %s partition (%s, %s) holds about %s; the policy allows %s.",
				p.Space(), p.PK, describeMembers(p), cost.HumanBytes(p.Size.Max), cost.HumanBytes(limit))
		}
		if !p.Rated || p.Risk != RiskHigh {
			continue
		}
		mb := p.Busiest()
		load := fmt.Sprintf("%.0f WRU/s and %.0f RRU/s", p.PeakWRU, p.PeakRRU)
		pct := math.Round(p.Headroom * 100)
		switch {
		case p.Headroom > 1 && p.SingleItem:
			a.add("hot-partition", Error, mb.Owner, "at peak the busiest %s key (%s) takes about %s, %.0f%% of one partition's capacity, on a single item, which DynamoDB can't split: it will throttle. Spread the writes over more keys, or lower the assumption if it's wrong.",
				p.Space(), p.PK, load, pct)
		default:
			split := "DynamoDB may split a busy key's items across partitions by sort key, but only for traffic spread over many sort keys."
			if monotonic(p) {
				split = "Its sort key starts with a time, so new items land together and splitting by sort key won't help."
			}
			a.add("hot-partition", Warning, mb.Owner, "at peak the busiest %s key (%s) takes about %s, %.0f%% of one partition's capacity (%d WRU, %d RRU per second). %s",
				p.Space(), p.PK, load, pct, cost.PartitionWCU, cost.PartitionRCU, split)
		}
	}
}

func largest(p *Partition) *Member {
	best := p.Members[0]
	for _, mb := range p.Members {
		if mb.Count.Max*float64(mb.Size.P50) > best.Count.Max*float64(best.Size.P50) {
			best = mb
		}
	}
	return best
}

func describeMembers(p *Partition) string {
	var out []string
	for _, mb := range p.Members {
		out = append(out, mb.Label)
	}
	return join(out)
}

// monotonic reports whether a partition's items are sorted by a time first, so writes of new
// items concentrate at the end of the key range.
func monotonic(p *Partition) bool {
	for _, mb := range p.Members {
		var sk schema.Template
		switch mb.Target.Kind {
		case cost.TargetItem:
			sk = mb.Target.Entity.SK
		case cost.TargetGSI, cost.TargetCopy:
			sk = mb.Target.Index.SK
		default:
			continue
		}
		if f := sk.FirstField(); f != "" && mb.Target.Entity.Field(f) != nil && mb.Target.Entity.Field(f).Type == schema.TypeTime {
			return true
		}
	}
	return false
}

func names(fs []*schema.Field) []string {
	out := make([]string, len(fs))
	for i, f := range fs {
		out[i] = f.Name
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

func isAre(n int) string {
	if n == 1 {
		return "is"
	}
	return "are"
}

func pronoun(n int) string {
	if n == 1 {
		return "one"
	}
	return "them"
}

// partitionOf returns the partition family holding the items the subject declares (an entity's
// items, an index's entries), or nil.
func (a *analyzer) partitionOf(s schema.Subject) *Partition {
	for _, p := range a.r.Partitions {
		for _, mb := range p.Members {
			if mb.Owner == s {
				return p
			}
		}
	}
	return nil
}

// headroomText formats a share of a partition's capacity: "<1%", "4%".
func headroomText(h float64) string {
	if h < 0.01 {
		return "<1%"
	}
	return fmt.Sprintf("%.0f%%", h*100)
}
