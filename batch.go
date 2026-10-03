package dynago

import (
	"context"
	"fmt"
	"sort"

	"github.com/guregu/dynamo/v2"
)

// BatchError reports the items of a batch write that were not written. Every other item was.
// errors.Is and errors.As see each item's error.
type BatchError struct {
	// Failed lists the items not written, in the order their keys were given.
	Failed []BatchFailure
}

// BatchFailure is one item a batch write did not write.
type BatchFailure struct {
	// Index is the item's place in the keys the call was given.
	Index int
	Err   error
}

func (e *BatchError) Error() string {
	if len(e.Failed) == 1 {
		return fmt.Sprintf("dynago: 1 item of the batch was not written: item %d: %v", e.Failed[0].Index, e.Failed[0].Err)
	}
	return fmt.Sprintf("dynago: %d items of the batch were not written; the first, item %d: %v", len(e.Failed), e.Failed[0].Index, e.Failed[0].Err)
}

// Unwrap returns each failed item's error.
func (e *BatchError) Unwrap() []error {
	errs := make([]error, len(e.Failed))
	for i, f := range e.Failed {
		errs[i] = f.Err
	}
	return errs
}

// BatchWrite applies one write to several items, size of them to a transaction, so that what
// they share is written once per transaction: the changes all of a transaction's items make to
// one counter item are merged into a single update of it.
//
// Each transaction reads its items with one consistent BatchGetItem and calls build for each one
// found, which returns the item's own write, guarded by the revision read, and its derived items
// before and after. An item that is absent fails with notFound, and one build returns an error
// for fails with that error; the rest of the transaction goes ahead without them. If an item
// changed between the read and the write, the transaction is built again from a fresh read, under
// the retry policy. A transaction that fails for any other reason (a claim taken, a limit reached,
// too many items) is split in two and each half tried on its own, down to single items, so one
// item's failure doesn't keep the others from being written.
//
// The batch as a whole is not atomic: each transaction is. BatchWrite returns nil if every item
// was written, and otherwise a *BatchError naming those that were not. A key given twice is
// written once.
func BatchWrite(ctx context.Context, db *dynamo.DB, t dynamo.Table, keys []Key, size int, notFound error,
	build func(key Key, raw dynamo.Item) (Op, Change, error)) error {
	seen := make(map[Key]bool, len(keys))
	var order []int
	for i, k := range keys {
		if err := k.Valid(); err != nil {
			return err
		}
		if !seen[k] {
			seen[k] = true
			order = append(order, i)
		}
	}
	var failed []BatchFailure
	var apply func(idx []int)
	apply = func(idx []int) {
		var skipped map[int]error
		err := Retry(ctx, func() error {
			skipped = map[int]error{}
			chunk := make([]Key, len(idx))
			for j, i := range idx {
				chunk[j] = keys[i]
			}
			raws, err := GetBatch(ctx, t, chunk, true)
			if err != nil {
				return err
			}
			found := make(map[Key]dynamo.Item, len(raws))
			for _, raw := range raws {
				found[ItemKey(raw)] = raw
			}
			var ops []Op
			var changes []Change
			for _, i := range idx {
				raw, ok := found[keys[i]]
				if !ok {
					skipped[i] = notFound
					continue
				}
				op, change, err := build(keys[i], raw)
				if err != nil {
					skipped[i] = err
					continue
				}
				ops = append(ops, op)
				changes = append(changes, change)
			}
			if len(ops) == 0 {
				return nil
			}
			derived, err := DiffAll(t, changes)
			if err != nil {
				return err
			}
			return Run(ctx, db, append(ops, derived...))
		})
		switch {
		case err == nil:
			for i, e := range skipped {
				failed = append(failed, BatchFailure{Index: i, Err: e})
			}
		case len(idx) > 1 && ctx.Err() == nil:
			// One item may be why the others weren't written: try each half on its own.
			apply(idx[:len(idx)/2])
			apply(idx[len(idx)/2:])
		default:
			for _, i := range idx {
				failed = append(failed, BatchFailure{Index: i, Err: err})
			}
		}
	}
	for start := 0; start < len(order); start += max(size, 1) {
		apply(order[start:min(start+max(size, 1), len(order))])
	}
	if len(failed) == 0 {
		return nil
	}
	sort.Slice(failed, func(i, j int) bool { return failed[i].Index < failed[j].Index })
	return &BatchError{Failed: failed}
}
