package dynago

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/guregu/dynamo/v2"
)

// This file is the migration engine: it copies a table generation into the next one while the old
// generation keeps serving, then catches up with what changed, then (with writes stopped) makes a
// last pass. Generated code supplies how to copy and remove one item; the engine does the
// scanning, checkpoints, rate limit, lease and reporting.
//
// Its state lives in the new table, under a partition of its own, so a restarted job resumes and
// a second job waits instead of racing the first. Every write the job makes, to entities or to its
// own state, is fenced by the lease: a job that lost it (paused, partitioned, or too slow to renew)
// can write nothing more.

// ErrUnchanged is returned by a migration's Copy when the new table already holds the item at the
// source's revision, or when the item isn't copied at all (it has expired).
var ErrUnchanged = errors.New("dynago: already copied at this revision")

// ErrMigrationConflict is wrapped by errors a migration reports as conflicts: items it can't copy
// until a person fixes the data in the old table (two old items converting to one new key, a
// value a conversion rejects). Claims taken, invalid keys and empty required fields are conflicts
// too. Other errors stop the job.
var ErrMigrationConflict = errors.New("dynago: item can't be copied")

// ErrLeaseLost means another migration job took over: this one stops without writing more.
var ErrLeaseLost = errors.New("dynago: another migration job holds the lease")

// Migration copies one table generation into the next.
type Migration struct {
	DB       *dynamo.DB
	From, To string
	// Types lists the entity types (their _t values) to copy. Other items of the old table
	// (claims, copies, counters, and entities this generation dropped) are not copied: the new
	// table's derived items are rebuilt from the entities.
	Types map[string]bool
	// Copy writes the entity converted from one item of the old table into the new one, with its
	// derived items and fence (an op the write must include in its transaction). It returns
	// ErrUnchanged if the new table already has that revision, or the item isn't copied.
	Copy func(ctx context.Context, raw dynamo.Item, fence Op) error
	// KeyOf returns the key an item of the old table converts to in the new one, or ok false if
	// it isn't copied (expired, or not an entity of this generation).
	KeyOf func(raw dynamo.Item) (key Key, ok bool, err error)
	// Remove deletes an entity of the new table, with its derived items and fence, because the
	// item it was copied from is gone or now converts to another key.
	Remove func(ctx context.Context, raw dynamo.Item, fence Op) error
	// Check reports why the migration can't run at all (a conversion function not provided), or nil.
	Check func() error
	// TTLAttr is the TTL attribute: expired items of the old table are not copied.
	TTLAttr string

	Workers   int     // parallel scan segments; default 8
	Rate      float64 // items per second across workers; 0 for no limit
	MaxPasses int     // catch-up passes after the first, for copy; default 3
	Out       io.Writer
	// LeaseFor is how long a job holds the lease without renewing it; default 2 minutes. The
	// lease is renewed every third of it.
	LeaseFor time.Duration
}

// Attributes the migration adds to the items it copies, recording where each came from.
const (
	attrMigSrcPK = "_msrcPK"
	attrMigSrcSK = "_msrcSK"
	attrMigRev   = "_mrev"
	migType      = "dynago.Migration"
)

// MigrationItem returns item, marshalled, with the source item's key and revision recorded on it.
func MigrationItem(item any, src Key, srcRev int64) (dynamo.Item, error) {
	out, err := dynamo.MarshalItem(item)
	if err != nil {
		return nil, err
	}
	out[attrMigSrcPK] = &types.AttributeValueMemberS{Value: src.PK}
	out[attrMigSrcSK] = &types.AttributeValueMemberS{Value: src.SK}
	out[attrMigRev] = &types.AttributeValueMemberN{Value: fmt.Sprint(srcRev)}
	return out, nil
}

// MigratedFrom inspects an item of the new table: same reports whether it was copied from src at
// srcRev; other reports whether it was copied from a different source item (two old items
// converting to one new key).
func MigratedFrom(raw dynamo.Item, src Key, srcRev int64) (same, other bool) {
	m, ok := migratedFrom(raw)
	if !ok {
		return false, false
	}
	if m.src != src {
		return false, true
	}
	return m.rev == srcRev, false
}

type migSource struct {
	src Key
	rev int64
}

func migratedFrom(raw dynamo.Item) (migSource, bool) {
	var m struct {
		PK  string `dynamo:"_msrcPK"`
		SK  string `dynamo:"_msrcSK"`
		Rev int64  `dynamo:"_mrev"`
	}
	if dynamo.UnmarshalItem(raw, &m) != nil || m.PK == "" {
		return migSource{}, false
	}
	return migSource{Key{m.PK, m.SK}, m.Rev}, true
}

// SourceOf returns the key and revision of an item of the old table.
func SourceOf(raw dynamo.Item) (Key, int64, error) {
	var s struct {
		PK  string `dynamo:"PK"`
		SK  string `dynamo:"SK"`
		Rev int64  `dynamo:"_rev"`
	}
	err := dynamo.UnmarshalItem(raw, &s)
	return Key{s.PK, s.SK}, s.Rev, err
}

// ItemType returns the entity or derived-item type (_t) of a stored item.
func ItemType(raw dynamo.Item) string {
	if s, ok := raw[AttrType].(*types.AttributeValueMemberS); ok {
		return s.Value
	}
	return ""
}

// ExpiredItem reports whether a stored item's TTL attribute has passed.
func ExpiredItem(raw dynamo.Item, ttlAttr string) bool {
	if n, ok := raw[ttlAttr].(*types.AttributeValueMemberN); ok && ttlAttr != "" {
		var ttl int64
		if _, err := fmt.Sscan(n.Value, &ttl); err == nil {
			return Expired(ttl, Now())
		}
	}
	return false
}

// migState is the migration's record in the new table.
type migState struct {
	PK         string `dynamo:"PK"`
	SK         string `dynamo:"SK"`
	T          string `dynamo:"_t"`
	Phase      string `dynamo:"phase"` // "", "copied" or "finished"
	Pass       int    `dynamo:"pass"`  // the pass in progress or last done
	PassDone   bool   `dynamo:"passDone"`
	Finishing  bool   `dynamo:"finishing"` // the pass in progress is the final one
	LeaseOwner string `dynamo:"leaseOwner"`
	LeaseUntil int64  `dynamo:"leaseUntil"`
	Fences     int    `dynamo:"fences"` // how many fence items jobs have used
}

type migFence struct {
	PK    string `dynamo:"PK"`
	SK    string `dynamo:"SK"`
	T     string `dynamo:"_t"`
	Owner string `dynamo:"owner"`
}

type migSegment struct {
	PK      string            `dynamo:"PK"`
	SK      string            `dynamo:"SK"`
	T       string            `dynamo:"_t"`
	Next    map[string]string `dynamo:"next"` // where to resume the scan
	Done    bool              `dynamo:"done"`
	Copied  int64             `dynamo:"copied"`
	Same    int64             `dynamo:"same"`
	Removed int64             `dynamo:"removed"`
	Skipped int64             `dynamo:"skipped"`
}

type migConflict struct {
	PK      string `dynamo:"PK"`
	SK      string `dynamo:"SK"`
	T       string `dynamo:"_t"`
	Item    string `dynamo:"item"`
	Problem string `dynamo:"problem"`
}

// Run runs a migration command: "copy" (bulk copy and catch-up passes, while the old generation
// serves), "finish" (a final pass, with writes to the old generation stopped) or "status".
func (m *Migration) Run(ctx context.Context, command string) error {
	if m.Out == nil {
		m.Out = os.Stdout
	}
	if m.Workers <= 0 {
		m.Workers = 8
	}
	if m.MaxPasses <= 0 {
		m.MaxPasses = 3
	}
	if m.LeaseFor <= 0 {
		m.LeaseFor = 2 * time.Minute
	}
	if m.Check != nil {
		if err := m.Check(); err != nil {
			return err
		}
	}
	switch command {
	case "status":
		return m.status(ctx)
	case "copy", "finish":
	default:
		return fmt.Errorf("dynago migrate: unknown command %q (want copy, finish or status)", command)
	}
	owner, err := m.lease(ctx, command == "finish")
	if err != nil {
		return err
	}
	if owner == "" {
		m.logf("the migration from %s to %s is finished", m.From, m.To)
		return nil
	}
	j := &job{Migration: m, owner: owner}
	// Renew the lease on a timer; if that fails, stop everything this job is doing.
	jctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	go j.keepLease(jctx, cancel)
	defer j.release(context.WithoutCancel(ctx))
	if err := j.installFences(jctx); err != nil {
		return err
	}
	err = j.run(jctx, command)
	if cause := context.Cause(jctx); errors.Is(cause, ErrLeaseLost) {
		return cause
	}
	return err
}

// job is one run of a migration command, holding the lease.
type job struct {
	*Migration
	owner string

	mu sync.Mutex
	// conflicts is the conflict records known to exist, so clearing one costs a write only when
	// there is something to clear.
	conflicts map[Key]bool
}

func (j *job) run(ctx context.Context, command string) error {
	st, err := j.state(ctx)
	if err != nil {
		return err
	}
	if st.Phase == "finished" {
		j.logf("the migration from %s to %s is finished", j.From, j.To)
		return nil
	}
	if command == "copy" {
		if err := j.copyPasses(ctx, st); err != nil {
			return err
		}
	} else if err := j.finishPass(ctx, st); err != nil {
		return err
	}
	return j.report(ctx, command)
}

// copyPasses runs (or resumes) the bulk pass, then catch-up passes until one changes nothing or
// MaxPasses is reached.
func (j *job) copyPasses(ctx context.Context, st migState) error {
	if st.Finishing && !st.PassDone {
		return fmt.Errorf("dynago migrate: a finish pass is in progress; run finish to complete it")
	}
	pass := st.Pass + 1
	if st.Pass > 0 && !st.PassDone {
		pass = st.Pass // resume the interrupted pass
	}
	for runs := 1; ; runs, pass = runs+1, pass+1 {
		changed, err := j.pass(ctx, pass, false)
		if err != nil {
			return err
		}
		if changed == 0 || runs > j.MaxPasses {
			j.logf("pass %d changed %d items; ready for finish", pass, changed)
			return j.setState(ctx, "phase", "copied")
		}
		j.logf("pass %d changed %d items; catching up", pass, changed)
	}
}

// finishPass runs one full pass that started after writes stopped (resuming it if it was
// interrupted), then marks the migration finished.
func (j *job) finishPass(ctx context.Context, st migState) error {
	pass := st.Pass + 1
	if st.Finishing && !st.PassDone {
		pass = st.Pass // resume the interrupted final pass
	}
	if _, err := j.pass(ctx, pass, true); err != nil {
		return err
	}
	conflicts, err := j.liveConflicts(ctx)
	if err != nil {
		return err
	}
	if len(conflicts) > 0 {
		return nil // report returns the error
	}
	if err := j.setState(ctx, "phase", "finished"); err != nil {
		return err
	}
	return j.tidy(ctx)
}

// pass copies every entity of the old table, removes copies whose source is gone or now converts
// to another key, then retries the items that conflicted. It returns how many items it wrote or
// removed.
func (j *job) pass(ctx context.Context, pass int, finishing bool) (int64, error) {
	err := j.stateUpdate().Set("pass", pass).Set("passDone", false).Set("finishing", finishing).If("$ = ?", "leaseOwner", j.owner).Run(ctx)
	if err := leaseErr(err); err != nil {
		return 0, err
	}
	if err := j.loadConflicts(ctx); err != nil {
		return 0, err
	}
	limit := newLimiter(j.Rate)
	var changed int64
	var mu sync.Mutex
	for _, phase := range []string{"copy", "remove"} {
		var wg sync.WaitGroup
		errs := make([]error, j.Workers)
		for seg := 0; seg < j.Workers; seg++ {
			wg.Add(1)
			go func(seg int) {
				defer wg.Done()
				n, err := j.segment(ctx, pass, phase, seg, limit)
				mu.Lock()
				changed += n
				mu.Unlock()
				errs[seg] = err
			}(seg)
		}
		wg.Wait()
		if err := errors.Join(errs...); err != nil {
			return changed, err
		}
	}
	// A conflict can be an artefact of order: a member who joined with the email of one who left
	// is copied before the leaver's copy is removed. Now that removals are done, try again.
	retried, err := j.retryConflicts(ctx, limit)
	if err != nil {
		return changed, err
	}
	changed += retried
	return changed, leaseErr(j.stateUpdate().Set("passDone", true).If("$ = ?", "leaseOwner", j.owner).Run(ctx))
}

// segment scans one segment of the old table (phase "copy") or the new one ("remove"), resuming
// from its checkpoint.
func (j *job) segment(ctx context.Context, pass int, phase string, seg int, limit *limiter) (int64, error) {
	table := j.DB.Table(j.From)
	if phase == "remove" {
		table = j.DB.Table(j.To)
	}
	sk := fmt.Sprintf("PASS#%06d#%s#%03d", pass, phase, seg)
	var cp migSegment
	found, err := GetOne(ctx, j.DB.Table(j.To), Key{j.statePK(), sk}, true, &cp)
	if err != nil {
		return 0, err
	}
	if found && cp.Done {
		return cp.Copied + cp.Removed, nil
	}
	cp = migSegment{PK: j.statePK(), SK: sk, T: migType, Next: cp.Next, Copied: cp.Copied, Same: cp.Same, Removed: cp.Removed, Skipped: cp.Skipped}
	for {
		scan := table.Scan().Segment(seg, j.Workers).Consistent(true).SearchLimit(100)
		if len(cp.Next) > 0 {
			scan = scan.StartFrom(pagingKey(cp.Next))
		}
		var items []dynamo.Item
		next, err := scan.AllWithLastEvaluatedKey(ctx, &items)
		if err != nil {
			return 0, err
		}
		if phase == "copy" {
			err = j.copyItems(ctx, items, &cp, limit, j.fence(seg))
		} else {
			err = j.removeItems(ctx, items, &cp, limit, j.fence(seg))
		}
		if err != nil {
			return 0, err
		}
		cp.Next = stringKey(next)
		cp.Done = len(next) == 0
		// The checkpoint is fenced too, so a job that lost the lease can't move it.
		ops := []Op{PutOp(Key{cp.PK, cp.SK}, j.DB.Table(j.To).Put(cp), nil), j.fence(seg)}
		if err := leaseErr(Run(ctx, j.DB, ops)); err != nil {
			return 0, err
		}
		if cp.Done {
			return cp.Copied + cp.Removed, nil
		}
	}
}

func (j *job) copyItems(ctx context.Context, items []dynamo.Item, cp *migSegment, limit *limiter, fence Op) error {
	for _, raw := range items {
		if !j.Types[ItemType(raw)] {
			cp.Skipped++
			continue
		}
		if err := limit.wait(ctx); err != nil {
			return err
		}
		switch copied, err := j.copyOne(ctx, raw, fence, 0); {
		case err != nil:
			return err
		case copied:
			cp.Copied++
		default:
			cp.Same++
		}
	}
	return nil
}

// copyOne copies an item of the old table, recording a conflict if it can't be copied. It reports
// whether it wrote anything.
//
// A claim taken by a stale copy (whose source has changed since, as when two libraries swap
// slugs) is not a conflict: the stale copy is removed, this item copied, and the removed item's
// source copied again straight away, which may in turn displace another stale copy (depth).
func (j *job) copyOne(ctx context.Context, raw dynamo.Item, fence Op, depth int) (bool, error) {
	src, _, err := SourceOf(raw)
	if err != nil {
		return false, err
	}
	copyIt := func() error { return j.patient(ctx, func() error { return j.Copy(ctx, raw, fence) }) }
	err = copyIt()
	var displaced dynamo.Item
	if errors.Is(err, ErrTaken) && depth < 10 {
		holder, stale, ferr := j.staleClaimHolder(ctx, err)
		if ferr != nil {
			return false, ferr
		}
		if stale {
			if rerr := j.patient(ctx, func() error { return j.Remove(ctx, holder, fence) }); rerr != nil {
				return false, rerr
			}
			displaced = holder
			err = copyIt()
		}
	}
	var copied bool
	switch {
	case errors.Is(err, ErrLeaseLost):
		return false, err
	case errors.Is(err, ErrUnchanged):
		err = j.clearConflict(ctx, src, fence)
	case isConflict(err):
		err = j.recordConflict(ctx, src, err, fence)
	case err != nil:
		return false, fmt.Errorf("copying %s / %s: %w", src.PK, src.SK, err)
	default:
		copied = true
		err = j.clearConflict(ctx, src, fence)
	}
	if err != nil || displaced == nil {
		return copied, err
	}
	// Copy the displaced item again, from its source as it is now.
	ms, _ := migratedFrom(displaced)
	var source dynamo.Item
	found, err := GetOne(ctx, j.DB.Table(j.From), ms.src, true, &source)
	if err != nil || !found || !j.Types[ItemType(source)] {
		return copied, err
	}
	_, err = j.copyOne(ctx, source, fence, depth+1)
	return copied, err
}

// staleClaimHolder finds the copy holding the claim a failed copy needed, and whether that copy
// is stale: its source is gone, has changed since it was copied, or converts to another key.
func (j *job) staleClaimHolder(ctx context.Context, taken error) (dynamo.Item, bool, error) {
	var oe *OpError
	if !errors.As(taken, &oe) {
		return nil, false, nil
	}
	var claim Claim
	found, err := GetOne(ctx, j.DB.Table(j.To), oe.Key, true, &claim)
	if err != nil || !found {
		return nil, false, err
	}
	var holder dynamo.Item
	found, err = GetOne(ctx, j.DB.Table(j.To), Key{claim.OwnerPK, claim.OwnerSK}, true, &holder)
	if err != nil || !found {
		return nil, false, err
	}
	ms, ok := migratedFrom(holder)
	if !ok {
		return nil, false, nil
	}
	var source dynamo.Item
	found, err = GetOne(ctx, j.DB.Table(j.From), ms.src, true, &source)
	if err != nil {
		return nil, false, err
	}
	if !found {
		return holder, true, nil
	}
	if _, rev, err := SourceOf(source); err != nil || rev != ms.rev {
		return holder, true, err
	}
	key, keep, err := j.KeyOf(source)
	if err != nil || !keep || key != ItemKey(holder) {
		return holder, true, nil
	}
	return nil, false, nil
}

func isConflict(err error) bool {
	return errors.Is(err, ErrMigrationConflict) || errors.Is(err, ErrTaken) || errors.Is(err, ErrInvalidKey) ||
		errors.Is(err, ErrFieldRequired) || errors.Is(err, ErrLimit) || errors.Is(err, ErrTooManyItems)
}

// patient runs one item's write, retrying past the usual retry policy when other transactions
// keep holding its items: many copies at once can queue on one busy counter item.
func (j *job) patient(ctx context.Context, fn func() error) error {
	for attempt := 0; ; attempt++ {
		err := fn()
		if !errors.Is(err, ErrConflict) || attempt == 5 {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond << attempt):
		}
	}
}

// removeItems deletes the new table's copies whose source is gone from the old table, or now
// converts to another key (its key fields changed while the old generation served).
func (j *job) removeItems(ctx context.Context, items []dynamo.Item, cp *migSegment, limit *limiter, fence Op) error {
	type copied struct {
		raw dynamo.Item
		key Key
		src Key
	}
	var copies []copied
	var keys []dynamo.Keyed
	seen := map[Key]bool{}
	for _, raw := range items {
		ms, ok := migratedFrom(raw)
		if !ok || ItemType(raw) == migType {
			continue
		}
		own, _, err := SourceOf(raw) // its own key in the new table
		if err != nil {
			return err
		}
		copies = append(copies, copied{raw, own, ms.src})
		if !seen[ms.src] {
			seen[ms.src] = true
			keys = append(keys, dynamo.Keys{ms.src.PK, ms.src.SK})
		}
	}
	if len(keys) == 0 {
		return nil
	}
	var sources []dynamo.Item
	err := j.DB.Table(j.From).Batch(AttrPK, AttrSK).Get(keys...).Consistent(true).All(ctx, &sources)
	if err != nil && !errors.Is(err, dynamo.ErrNotFound) {
		return err
	}
	bySource := map[Key]dynamo.Item{}
	for _, s := range sources {
		k, _, err := SourceOf(s)
		if err != nil {
			return err
		}
		bySource[k] = s
	}
	for _, c := range copies {
		if s, ok := bySource[c.src]; ok {
			key, keep, err := j.KeyOf(s)
			if err == nil && keep && key == c.key {
				continue
			}
			// Otherwise the source expired, or converts elsewhere (or not at all, which the
			// copy phase reports as a conflict): this copy is stale.
		}
		if err := limit.wait(ctx); err != nil {
			return err
		}
		if err := j.patient(ctx, func() error { return j.Remove(ctx, c.raw, fence) }); err != nil {
			if errors.Is(err, ErrLeaseLost) {
				return err
			}
			return fmt.Errorf("removing %s / %s, copied from %s / %s: %w", c.key.PK, c.key.SK, c.src.PK, c.src.SK, err)
		}
		cp.Removed++
	}
	return nil
}

// retryConflicts tries the items with conflicts again. It returns how many it copied.
func (j *job) retryConflicts(ctx context.Context, limit *limiter) (int64, error) {
	j.mu.Lock()
	var srcs []Key
	for k := range j.conflicts {
		srcs = append(srcs, k)
	}
	j.mu.Unlock()
	var n int64
	for _, src := range srcs {
		var raw dynamo.Item
		found, err := GetOne(ctx, j.DB.Table(j.From), src, true, &raw)
		if err != nil {
			return n, err
		}
		if !found {
			if err := j.clearConflict(ctx, src, j.fence(0)); err != nil {
				return n, err
			}
			continue
		}
		if err := limit.wait(ctx); err != nil {
			return n, err
		}
		copied, err := j.copyOne(ctx, raw, j.fence(0), 0)
		if err != nil {
			return n, err
		}
		if copied {
			n++
		}
	}
	return n, nil
}

// fence is the op every write of worker w includes: a check that this job still holds the lease,
// made on the worker's own fence item. A single item checked by every worker's transactions would
// make them conflict with one another; with one per worker, they never do. Taking the lease
// rewrites every fence item, so a job that lost it fails its next write whichever worker makes it.
func (j *job) fence(w int) Op {
	k := Key{j.statePK(), fmt.Sprintf("FENCE#%03d", w)}
	return CheckOp(k, j.DB.Table(j.To).Check(AttrPK, k.PK).Range(AttrSK, k.SK).If("$ = ?", "owner", j.owner), ErrLeaseLost)
}

// installFences points every fence item, including any a previous job with more workers used, at
// this job. Until it has, it writes nothing else.
func (j *job) installFences(ctx context.Context) error {
	st, err := j.state(ctx)
	if err != nil {
		return err
	}
	n := max(st.Fences, j.Workers)
	for w := 0; w < n; w++ {
		k := Key{j.statePK(), fmt.Sprintf("FENCE#%03d", w)}
		put := j.DB.Table(j.To).Put(migFence{PK: k.PK, SK: k.SK, T: migType, Owner: j.owner})
		state := Key{j.statePK(), "STATE"}
		check := CheckOp(state, j.DB.Table(j.To).Check(AttrPK, state.PK).Range(AttrSK, state.SK).If("$ = ?", "leaseOwner", j.owner), ErrLeaseLost)
		if err := leaseErr(Run(ctx, j.DB, []Op{PutOp(k, put, nil), check})); err != nil {
			return err
		}
	}
	return j.setState(ctx, "fences", n)
}

// leaseErr turns the failure of a lease-conditioned write into ErrLeaseLost.
func leaseErr(err error) error {
	if dynamo.IsCondCheckFailed(err) {
		return ErrLeaseLost
	}
	return err
}

func (j *job) keepLease(ctx context.Context, cancel context.CancelCauseFunc) {
	every := j.LeaseFor / 3
	timer := time.NewTimer(every)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			err := leaseErr(j.stateUpdate().Set("leaseUntil", time.Now().Add(j.LeaseFor).Unix()).If("$ = ?", "leaseOwner", j.owner).Run(ctx))
			switch {
			case errors.Is(err, ErrLeaseLost):
				cancel(ErrLeaseLost)
				return
			case err != nil:
				timer.Reset(time.Second) // a transient failure: try again soon, well before expiry
			default:
				timer.Reset(every)
			}
		}
	}
}

func (j *job) release(ctx context.Context) {
	_ = j.DB.Table(j.To).Update(AttrPK, j.statePK()).Range(AttrSK, "STATE").
		Remove("leaseOwner", "leaseUntil").If("$ = ?", "leaseOwner", j.owner).Run(ctx)
}

func (j *job) setState(ctx context.Context, attr string, value any) error {
	return leaseErr(j.stateUpdate().Set(attr, value).If("$ = ?", "leaseOwner", j.owner).Run(ctx))
}

func (j *job) loadConflicts(ctx context.Context) error {
	cs, err := j.allConflicts(ctx)
	if err != nil {
		return err
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	j.conflicts = map[Key]bool{}
	for _, c := range cs {
		j.conflicts[conflictSource(c)] = true
	}
	return nil
}

func conflictSource(c migConflict) Key {
	pk, sk, _ := strings.Cut(strings.TrimPrefix(c.SK, "CONFLICT#"), "|")
	return Key{pk, sk}
}

func (j *job) recordConflict(ctx context.Context, src Key, problem error, fence Op) error {
	k := Key{j.statePK(), "CONFLICT#" + src.PK + "|" + src.SK}
	put := j.DB.Table(j.To).Put(migConflict{PK: k.PK, SK: k.SK, T: migType, Item: src.PK + " / " + src.SK, Problem: problem.Error()})
	if err := leaseErr(Run(ctx, j.DB, []Op{PutOp(k, put, nil), fence})); err != nil {
		return err
	}
	j.mu.Lock()
	j.conflicts[src] = true
	j.mu.Unlock()
	return nil
}

func (j *job) clearConflict(ctx context.Context, src Key, fence Op) error {
	j.mu.Lock()
	known := j.conflicts[src]
	j.mu.Unlock()
	if !known {
		return nil
	}
	k := Key{j.statePK(), "CONFLICT#" + src.PK + "|" + src.SK}
	del := j.DB.Table(j.To).Delete(AttrPK, k.PK).Range(AttrSK, k.SK)
	if err := leaseErr(Run(ctx, j.DB, []Op{DeleteOp(k, del, nil), fence})); err != nil {
		return err
	}
	j.mu.Lock()
	delete(j.conflicts, src)
	j.mu.Unlock()
	return nil
}

// liveConflicts returns the conflicts whose source item still exists (and hasn't expired),
// clearing the others: an item gone from the old table no longer needs copying.
func (j *job) liveConflicts(ctx context.Context) ([]migConflict, error) {
	cs, err := j.allConflicts(ctx)
	if err != nil {
		return nil, err
	}
	var live []migConflict
	for _, c := range cs {
		src := conflictSource(c)
		var raw dynamo.Item
		found, err := GetOne(ctx, j.DB.Table(j.From), src, true, &raw)
		if err != nil {
			return nil, err
		}
		if !found || ExpiredItem(raw, j.TTLAttr) {
			j.mu.Lock()
			if j.conflicts == nil {
				j.conflicts = map[Key]bool{}
			}
			j.conflicts[src] = true
			j.mu.Unlock()
			if err := j.clearConflict(ctx, src, j.fence(0)); err != nil {
				return nil, err
			}
			continue
		}
		live = append(live, c)
	}
	return live, nil
}

// tidy deletes the finished migration's checkpoints, leaving its state record.
func (j *job) tidy(ctx context.Context) error {
	var segs []migSegment
	err := j.DB.Table(j.To).Get(AttrPK, j.statePK()).Range(AttrSK, dynamo.BeginsWith, "PASS#").Consistent(true).All(ctx, &segs)
	if err != nil && !errors.Is(err, dynamo.ErrNotFound) {
		return err
	}
	for _, s := range segs {
		del := j.DB.Table(j.To).Delete(AttrPK, s.PK).Range(AttrSK, s.SK)
		if err := leaseErr(Run(ctx, j.DB, []Op{DeleteOp(Key{s.PK, s.SK}, del, nil), j.fence(0)})); err != nil {
			return err
		}
	}
	return nil
}

// report prints the conflicts, if any, and returns an error for them: a rollout must not go ahead
// with items missing from the new table.
func (j *job) report(ctx context.Context, command string) error {
	cs, err := j.liveConflicts(ctx)
	if err != nil {
		return err
	}
	if len(cs) == 0 {
		j.logf("%s: done", command)
		return nil
	}
	sort.Slice(cs, func(a, b int) bool { return cs[a].Item < cs[b].Item })
	for _, c := range cs {
		j.logf("conflict: %s: %s", c.Item, c.Problem)
	}
	return fmt.Errorf("%w: %d items of %s were not copied; fix them in the old table and run %s again", ErrMigrationConflict, len(cs), j.From, command)
}

func (m *Migration) statePK() string { return "_DYNAGO#MIGRATION#" + m.From }

func (m *Migration) state(ctx context.Context) (migState, error) {
	var st migState
	_, err := GetOne(ctx, m.DB.Table(m.To), Key{m.statePK(), "STATE"}, true, &st)
	return st, err
}

// stateUpdate starts an update of the migration's state item.
func (m *Migration) stateUpdate() *dynamo.Update {
	return m.DB.Table(m.To).Update(AttrPK, m.statePK()).Range(AttrSK, "STATE").Set(Path(AttrType), migType)
}

// lease takes the migration's lease. finish waits for a lease another job holds; copy fails
// instead. It returns "" if the migration is already finished.
func (m *Migration) lease(ctx context.Context, wait bool) (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	host, _ := os.Hostname()
	owner := host + "/" + hex.EncodeToString(b)
	for {
		now := time.Now()
		err := m.stateUpdate().Set("leaseOwner", owner).Set("leaseUntil", now.Add(m.LeaseFor).Unix()).
			If("(attribute_not_exists($) OR $ < ?) AND (attribute_not_exists($) OR $ <> ?)", "leaseUntil", "leaseUntil", now.Unix(), "phase", "phase", "finished").
			Run(ctx)
		if err == nil {
			return owner, nil
		}
		if !dynamo.IsCondCheckFailed(err) {
			return "", err
		}
		st, err := m.state(ctx)
		if err != nil {
			return "", err
		}
		if st.Phase == "finished" {
			return "", nil
		}
		if !wait {
			return "", fmt.Errorf("dynago migrate: another job (%s) holds the migration until %s", st.LeaseOwner, time.Unix(st.LeaseUntil, 0).Format(time.RFC3339))
		}
		m.logf("waiting for %s, which holds the migration", st.LeaseOwner)
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

func (m *Migration) allConflicts(ctx context.Context) ([]migConflict, error) {
	var out []migConflict
	err := m.DB.Table(m.To).Get(AttrPK, m.statePK()).Range(AttrSK, dynamo.BeginsWith, "CONFLICT#").Consistent(true).All(ctx, &out)
	if errors.Is(err, dynamo.ErrNotFound) {
		err = nil
	}
	return out, err
}

func (m *Migration) status(ctx context.Context) error {
	st, err := m.state(ctx)
	if err != nil {
		return err
	}
	phase := st.Phase
	if phase == "" {
		phase = "not started"
		if st.Pass > 0 {
			phase = "copying"
		}
	}
	m.logf("migration %s → %s: %s, pass %d (done: %t, final: %t)", m.From, m.To, phase, st.Pass, st.PassDone, st.Finishing)
	var segs []migSegment
	err = m.DB.Table(m.To).Get(AttrPK, m.statePK()).Range(AttrSK, dynamo.BeginsWith, fmt.Sprintf("PASS#%06d#", st.Pass)).Consistent(true).All(ctx, &segs)
	if err != nil && !errors.Is(err, dynamo.ErrNotFound) {
		return err
	}
	var copied, same, removed, skipped, done int64
	for _, s := range segs {
		copied, same, removed, skipped = copied+s.Copied, same+s.Same, removed+s.Removed, skipped+s.Skipped
		if s.Done {
			done++
		}
	}
	m.logf("pass %d: %d segments done, %d copied, %d unchanged, %d removed, %d not copied (other types)", st.Pass, done, copied, same, removed, skipped)
	cs, err := m.allConflicts(ctx)
	if err != nil {
		return err
	}
	for _, c := range cs {
		m.logf("conflict: %s: %s", c.Item, c.Problem)
	}
	return nil
}

func (m *Migration) logf(format string, args ...any) {
	fmt.Fprintf(m.Out, format+"\n", args...)
}

func pagingKey(m map[string]string) dynamo.PagingKey {
	out := make(dynamo.PagingKey, len(m))
	for k, v := range m {
		out[k] = &types.AttributeValueMemberS{Value: v}
	}
	return out
}

func stringKey(k dynamo.PagingKey) map[string]string {
	if len(k) == 0 {
		return nil
	}
	out := make(map[string]string, len(k))
	for name, v := range k {
		if s, ok := v.(*types.AttributeValueMemberS); ok {
			out[name] = s.Value
		}
	}
	return out
}

// limiter spaces work out to a rate shared by all workers.
type limiter struct {
	mu    sync.Mutex
	every time.Duration
	next  time.Time
}

func newLimiter(perSecond float64) *limiter {
	if perSecond <= 0 {
		return &limiter{}
	}
	return &limiter{every: time.Duration(float64(time.Second) / perSecond)}
}

func (l *limiter) wait(ctx context.Context) error {
	if l.every == 0 {
		return ctx.Err()
	}
	l.mu.Lock()
	now := time.Now()
	if l.next.Before(now) {
		l.next = now
	}
	at := l.next
	l.next = l.next.Add(l.every)
	l.mu.Unlock()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(time.Until(at)):
		return nil
	}
}

// MigrateUsage describes the migration command's arguments, for generated commands.
const MigrateUsage = `usage: migrate [-table base] [-workers n] [-rate items/s] [-passes n] copy|finish|status

  copy    copy the previous table generation into the current one, then catch up with changes,
          while the previous one keeps serving. Run it before rolling out the new version.
  finish  make a last pass with writes to the previous generation stopped. Safe to run from
          every new pod: one does the work, the others wait for it.
  status  print progress and conflicts.`

// ParseMigrate reads the migration command's arguments.
func ParseMigrate(args []string, base string) (command, table string, workers, passes int, rate float64, err error) {
	table = base
	var rest []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "-") {
			rest = append(rest, a)
			continue
		}
		name, val, has := strings.Cut(strings.TrimLeft(a, "-"), "=")
		if !has {
			if i+1 >= len(args) {
				return "", "", 0, 0, 0, fmt.Errorf("flag -%s needs a value\n\n%s", name, MigrateUsage)
			}
			i++
			val = args[i]
		}
		switch name {
		case "table":
			table = val
		case "workers":
			_, err = fmt.Sscan(val, &workers)
		case "passes":
			_, err = fmt.Sscan(val, &passes)
		case "rate":
			_, err = fmt.Sscan(val, &rate)
		default:
			err = fmt.Errorf("unknown flag -%s", name)
		}
		if err != nil {
			return "", "", 0, 0, 0, fmt.Errorf("%w\n\n%s", err, MigrateUsage)
		}
	}
	if len(rest) != 1 {
		return "", "", 0, 0, 0, fmt.Errorf("want one command\n\n%s", MigrateUsage)
	}
	return rest[0], table, workers, passes, rate, nil
}

// ItemKey returns a stored item's key.
func ItemKey(raw dynamo.Item) Key {
	k, _, _ := SourceOf(raw)
	return k
}
