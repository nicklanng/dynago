package dynago

import (
	"errors"
	"fmt"
	"hash/fnv"
	"reflect"
	"sort"
	"strconv"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/guregu/dynamo/v2"
)

// DerivedKind is the kind of item maintained alongside an entity item.
type DerivedKind int

// Kinds of derived items.
const (
	// KindCounter is one entity's contribution to one attribute of a counter item.
	KindCounter DerivedKind = iota + 1
	// KindClaim is a uniqueness claim owned by the entity item.
	KindClaim
	// KindCopy is a copy item holding a projection of the entity, used as a transactional index.
	KindCopy
)

// Derived describes an item that exists because of an entity's state. Generated code computes the
// derived items of an entity before and after a write; Diff turns the difference into writes.
type Derived struct {
	Kind DerivedKind
	Key  Key
	// Type is the _t value stored on the derived item.
	Type string

	// Counter contribution.
	Attr     string
	Amount   int64
	Limit    Limit
	LimitErr error
	// LimitRequired marks a value whose limit the caller must supply: growing it without one fails.
	LimitRequired bool
	// Min is a lower bound on the counter value; MinErr is returned when a write would cross it.
	Min    Limit
	MinErr error

	// Claim.
	TakenErr error

	// Copy: the full item to store, including its PK, SK, _t and _v attributes. Created is the
	// owner's creation time (TimeLayout), stamped on the copy; "" for a new owner, stamped now.
	Item    any
	Created string
}

// ShardPK appends the shard an owner's contributions land on to a counter partition key.
func ShardPK(pk string, owner Key, shards int) string {
	if shards <= 1 {
		return pk
	}
	h := fnv.New32a()
	h.Write([]byte(owner.PK))
	h.Write([]byte{0})
	h.Write([]byte(owner.SK))
	return pk + "#S" + strconv.Itoa(int(h.Sum32()%uint32(shards)))
}

// ShardPKs returns every shard's partition key, for reading a sharded counter.
func ShardPKs(pk string, shards int) []string {
	if shards <= 1 {
		return []string{pk}
	}
	out := make([]string, shards)
	for i := range out {
		out[i] = pk + "#S" + strconv.Itoa(i)
	}
	return out
}

type claimItem struct {
	PK      string `dynamo:"PK"`
	SK      string `dynamo:"SK"`
	Type    string `dynamo:"_t"`
	OwnerPK string `dynamo:"ownerPK"`
	OwnerSK string `dynamo:"ownerSK"`
	Created string `dynamo:"_created,omitempty"`
	Updated string `dynamo:"_updated,omitempty"`
}

// Claim is the stored form of a uniqueness claim.
type Claim = claimItem

// Change is one item's derived items before and after a write. A write that also changes other
// entities (through requires) has one Change per entity it changes.
type Change struct {
	Owner         Key
	Before, After []Derived
}

// Diff returns the writes that move the derived items of owner from before to after. See DiffAll.
func Diff(t dynamo.Table, owner Key, before, after []Derived) ([]Op, error) {
	return DiffAll(t, []Change{{Owner: owner, Before: before, After: after}})
}

// DiffAll returns the writes that apply every change of one transaction:
//   - counters get one ADD per changed item, conditioned on their limits when they grow. Changes
//     to the same counter item from different owners are merged, since a transaction may touch
//     an item only once (two accounts of one tenant moving between states in one write);
//   - claims are created (conditioned on being free or already the owner's) or released;
//   - copies are put when new or changed and deleted when gone.
func DiffAll(t dynamo.Table, changes []Change) ([]Op, error) {
	var ops []Op

	type counterAttr struct {
		key  Key
		attr string
	}
	type counterState struct {
		typ      string
		delta    int64
		limit    Limit
		limitErr error
		required bool
		name     string
		min      Limit
		minErr   error
	}
	counters := map[counterAttr]*counterState{}
	var counterOrder []counterAttr
	touch := func(d Derived) *counterState {
		ca := counterAttr{d.Key, d.Attr}
		cs := counters[ca]
		if cs == nil {
			cs = &counterState{typ: d.Type}
			counters[ca] = cs
			counterOrder = append(counterOrder, ca)
		}
		return cs
	}
	bounds := func(cs *counterState, d Derived) {
		if d.LimitRequired {
			cs.required, cs.name = true, d.Type+"."+d.Attr
		}
		if d.Limit.Set && !cs.limit.Set {
			cs.limit, cs.limitErr = d.Limit, d.LimitErr
		}
		if d.Min.Set && !cs.min.Set {
			cs.min, cs.minErr = d.Min, d.MinErr
		}
	}
	for _, ch := range changes {
		for _, d := range ch.Before {
			if d.Kind == KindCounter {
				cs := touch(d)
				cs.delta -= d.Amount
				if d.Min.Set && !cs.min.Set {
					cs.min, cs.minErr = d.Min, d.MinErr
				}
			}
		}
		for _, d := range ch.After {
			if d.Kind == KindCounter {
				cs := touch(d)
				cs.delta += d.Amount
				bounds(cs, d)
			}
		}
	}

	// Counters: group the changed attributes of each counter item into one update.
	byKey := map[Key][]counterAttr{}
	var keyOrder []Key
	for _, ca := range counterOrder {
		if counters[ca].delta == 0 {
			continue
		}
		if _, ok := byKey[ca.key]; !ok {
			keyOrder = append(keyOrder, ca.key)
		}
		byKey[ca.key] = append(byKey[ca.key], ca)
	}
	sortKeys(keyOrder)
	now := NewStamp()
	for _, k := range keyOrder {
		u := t.Update(AttrPK, k.PK).Range(AttrSK, k.SK).Set(Path(AttrType), counters[byKey[k][0]].typ).
			Set(Path(AttrUpdated), now).SetIfNotExists(Path(AttrCreated), now)
		var limitErrs []error
		for _, ca := range byKey[k] {
			cs := counters[ca]
			u.Add(Path(ca.attr), cs.delta)
			if cs.delta > 0 && cs.required && !cs.limit.Set {
				return nil, fmt.Errorf("%w: %s", ErrLimitRequired, cs.name)
			}
			if cs.delta > 0 && cs.limit.Set && !cs.limit.Unbounded {
				if cs.delta > cs.limit.Max {
					return nil, cs.limitErr
				}
				u.If("attribute_not_exists($) OR $ <= ?", ca.attr, ca.attr, cs.limit.Max-cs.delta)
				limitErrs = append(limitErrs, cs.limitErr)
			}
			if cs.delta < 0 && cs.min.Set {
				// value + delta >= min. A missing attribute is 0, which fails the comparison,
				// correctly: 0 + a negative delta is below any min >= 0.
				u.If("$ >= ?", ca.attr, cs.min.Max-cs.delta)
				limitErrs = append(limitErrs, cs.minErr)
			}
		}
		// With several limited attributes on one item DynamoDB cannot say which condition failed,
		// so the error matches each of them.
		ops = append(ops, UpdateOp(k, u, errors.Join(limitErrs...)))
	}
	for _, ch := range changes {
		cc, err := claimsAndCopies(t, ch, now)
		if err != nil {
			return nil, err
		}
		ops = append(ops, cc...)
	}
	return ops, nil
}

// claimsAndCopies returns the claim and copy writes of one owner's change, stamped now.
func claimsAndCopies(t dynamo.Table, ch Change, now string) ([]Op, error) {
	var ops []Op
	owner := ch.Owner
	claimsBefore, claimsAfter := map[Key]Derived{}, map[Key]Derived{}
	copiesBefore, copiesAfter := map[Key]Derived{}, map[Key]Derived{}
	var claimOrder, copyOrder []Key
	for _, d := range ch.Before {
		switch d.Kind {
		case KindClaim:
			claimsBefore[d.Key] = d
			claimOrder = append(claimOrder, d.Key)
		case KindCopy:
			copiesBefore[d.Key] = d
			copyOrder = append(copyOrder, d.Key)
		}
	}
	for _, d := range ch.After {
		switch d.Kind {
		case KindClaim:
			claimsAfter[d.Key] = d
			if _, ok := claimsBefore[d.Key]; !ok {
				claimOrder = append(claimOrder, d.Key)
			}
		case KindCopy:
			copiesAfter[d.Key] = d
			if _, ok := copiesBefore[d.Key]; !ok {
				copyOrder = append(copyOrder, d.Key)
			}
		}
	}

	sortKeys(claimOrder)
	for _, k := range dedupe(claimOrder) {
		_, had := claimsBefore[k]
		d, has := claimsAfter[k]
		switch {
		case has && !had:
			p := t.Put(claimItem{PK: k.PK, SK: k.SK, Type: d.Type, OwnerPK: owner.PK, OwnerSK: owner.SK, Created: now, Updated: now}).
				If("attribute_not_exists($) OR ($ = ? AND $ = ?)", AttrPK, "ownerPK", owner.PK, "ownerSK", owner.SK)
			ops = append(ops, PutOp(k, p, d.TakenErr))
		case had && !has:
			del := t.Delete(AttrPK, k.PK).Range(AttrSK, k.SK).
				If("attribute_not_exists($) OR ($ = ? AND $ = ?)", AttrPK, "ownerPK", owner.PK, "ownerSK", owner.SK)
			ops = append(ops, DeleteOp(k, del, ErrStale))
		}
	}

	sortKeys(copyOrder)
	for _, k := range dedupe(copyOrder) {
		b, had := copiesBefore[k]
		a, has := copiesAfter[k]
		switch {
		case has && (!had || !sameItem(a.Item, b.Item)):
			item, err := dynamo.MarshalItem(a.Item)
			if err != nil {
				return nil, err
			}
			created := a.Created
			if created == "" {
				created = now
			}
			item[AttrCreated] = &types.AttributeValueMemberS{Value: created}
			item[AttrUpdated] = &types.AttributeValueMemberS{Value: now}
			ops = append(ops, PutOp(k, t.Put(item), nil))
		case had && !has:
			ops = append(ops, DeleteOp(k, t.Delete(AttrPK, k.PK).Range(AttrSK, k.SK), nil))
		}
	}
	return ops, nil
}

func sameItem(a, b any) bool {
	ma, errA := dynamo.MarshalItem(a)
	mb, errB := dynamo.MarshalItem(b)
	return errA == nil && errB == nil && reflect.DeepEqual(ma, mb)
}

func sortKeys(keys []Key) {
	sort.SliceStable(keys, func(i, j int) bool {
		if keys[i].PK != keys[j].PK {
			return keys[i].PK < keys[j].PK
		}
		return keys[i].SK < keys[j].SK
	})
}

func dedupe(keys []Key) []Key {
	out := keys[:0:0]
	seen := map[Key]bool{}
	for _, k := range keys {
		if !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	return out
}

// Unbounded returns derived items without their limits and lower bounds. The migration job uses
// it: it copies what the old table holds, whose writes already enforced its rules, so a count
// briefly out of range while items are copied in any order is not an error.
func Unbounded(ds []Derived) []Derived {
	out := make([]Derived, len(ds))
	for i, d := range ds {
		d.Limit, d.LimitErr, d.LimitRequired = Limit{}, nil, false
		d.Min, d.MinErr = Limit{}, nil
		out[i] = d
	}
	return out
}
