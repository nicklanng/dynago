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
// a second job waits instead of racing the first.

// ErrUnchanged is returned by a migration's Copy when the new table already holds the item at the
// source's revision.
var ErrUnchanged = errors.New("dynago: already copied at this revision")

// ErrMigrationConflict is wrapped by errors a migration reports as conflicts: items it can't copy
// until a person fixes the data in the old table (two old items converting to one new key, a
// value a conversion rejects). Claims taken, limits crossed, invalid keys and empty required
// fields are conflicts too. Other errors stop the job.
var ErrMigrationConflict = errors.New("dynago: item can't be copied")

// Migration copies one table generation into the next.
type Migration struct {
	DB       *dynamo.DB
	From, To string
	// Types lists the entity types (their _t values) to copy. Other items of the old table
	// (claims, copies, counters, and entities this generation dropped) are not copied: the new
	// table's derived items are rebuilt from the entities.
	Types map[string]bool
	// Copy writes the entity converted from one item of the old table into the new one, with its
	// derived items. It returns ErrUnchanged if the new table already has that revision.
	Copy func(ctx context.Context, raw dynamo.Item) error
	// Remove deletes an entity of the new table, with its derived items, because the item it was
	// copied from no longer exists.
	Remove func(ctx context.Context, raw dynamo.Item) error
	// Check reports why the migration can't run at all (a conversion function not provided), or nil.
	Check func() error

	Workers   int     // parallel scan segments; default 8
	Rate      float64 // items per second across workers; 0 for no limit
	MaxPasses int     // catch-up passes after the first, for copy; default 3
	Out       io.Writer
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
	var m struct {
		PK  string `dynamo:"_msrcPK"`
		SK  string `dynamo:"_msrcSK"`
		Rev int64  `dynamo:"_mrev"`
	}
	if dynamo.UnmarshalItem(raw, &m) != nil || m.PK == "" {
		return false, false
	}
	if (Key{m.PK, m.SK}) != src {
		return false, true
	}
	return m.Rev == srcRev, false
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

const leaseFor = 2 * time.Minute

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
	defer m.release(context.WithoutCancel(ctx), owner)
	st, err := m.state(ctx)
	if err != nil {
		return err
	}
	if st.Phase == "finished" {
		m.logf("the migration from %s to %s is finished", m.From, m.To)
		return nil
	}
	if command == "copy" {
		if err := m.copyPasses(ctx, owner, st); err != nil {
			return err
		}
	} else {
		if err := m.finishPass(ctx, owner, st); err != nil {
			return err
		}
	}
	return m.report(ctx, command)
}

// copyPasses runs (or resumes) the bulk pass, then catch-up passes until one changes nothing or
// MaxPasses is reached.
func (m *Migration) copyPasses(ctx context.Context, owner string, st migState) error {
	if st.Finishing && !st.PassDone {
		return fmt.Errorf("dynago migrate: a finish pass is in progress; run finish to complete it")
	}
	pass := st.Pass + 1
	if st.Pass > 0 && !st.PassDone {
		pass = st.Pass // resume the interrupted pass
	}
	for runs := 1; ; runs, pass = runs+1, pass+1 {
		changed, err := m.pass(ctx, owner, pass, false)
		if err != nil {
			return err
		}
		if changed == 0 || runs > m.MaxPasses {
			m.logf("pass %d changed %d items; ready for finish", pass, changed)
			return m.setPhase(ctx, "copied")
		}
		m.logf("pass %d changed %d items; catching up", pass, changed)
	}
}

// finishPass runs one full pass that started after writes stopped (resuming it if it was
// interrupted), then marks the migration finished.
func (m *Migration) finishPass(ctx context.Context, owner string, st migState) error {
	pass := st.Pass + 1
	if st.Finishing && !st.PassDone {
		pass = st.Pass // resume the interrupted final pass
	}
	if _, err := m.pass(ctx, owner, pass, true); err != nil {
		return err
	}
	conflicts, err := m.liveConflicts(ctx)
	if err != nil {
		return err
	}
	if len(conflicts) > 0 {
		return nil // report returns the error
	}
	if err := m.setPhase(ctx, "finished"); err != nil {
		return err
	}
	return m.tidy(ctx)
}

// tidy deletes the finished migration's checkpoints, leaving its state record.
func (m *Migration) tidy(ctx context.Context) error {
	var segs []migSegment
	err := m.DB.Table(m.To).Get(AttrPK, m.statePK()).Range(AttrSK, dynamo.BeginsWith, "PASS#").Consistent(true).All(ctx, &segs)
	if err != nil && !errors.Is(err, dynamo.ErrNotFound) {
		return err
	}
	for _, s := range segs {
		if err := m.DB.Table(m.To).Delete(AttrPK, s.PK).Range(AttrSK, s.SK).Run(ctx); err != nil {
			return err
		}
	}
	return nil
}

// pass copies every entity of the old table and removes copies whose source is gone. It returns
// how many items it wrote or removed.
func (m *Migration) pass(ctx context.Context, owner string, pass int, finishing bool) (int64, error) {
	if err := m.startPass(ctx, pass, finishing); err != nil {
		return 0, err
	}
	limit := newLimiter(m.Rate)
	var changed int64
	var mu sync.Mutex
	for _, phase := range []string{"copy", "remove"} {
		var wg sync.WaitGroup
		errs := make([]error, m.Workers)
		for seg := 0; seg < m.Workers; seg++ {
			wg.Add(1)
			go func(seg int) {
				defer wg.Done()
				n, err := m.segment(ctx, owner, pass, phase, seg, limit)
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
	return changed, m.stateTable().Set("passDone", true).Run(ctx)
}

// segment scans one segment of the old table (phase "copy") or the new one ("remove"), resuming
// from its checkpoint.
func (m *Migration) segment(ctx context.Context, owner string, pass int, phase string, seg int, limit *limiter) (int64, error) {
	table := m.DB.Table(m.From)
	if phase == "remove" {
		table = m.DB.Table(m.To)
	}
	sk := fmt.Sprintf("PASS#%06d#%s#%03d", pass, phase, seg)
	var cp migSegment
	found, err := GetOne(ctx, m.DB.Table(m.To), Key{m.statePK(), sk}, true, &cp)
	if err != nil {
		return 0, err
	}
	if found && cp.Done {
		return cp.Copied + cp.Removed, nil
	}
	cp = migSegment{PK: m.statePK(), SK: sk, T: migType, Next: cp.Next, Copied: cp.Copied, Same: cp.Same, Removed: cp.Removed, Skipped: cp.Skipped}
	for {
		if err := m.renew(ctx, owner); err != nil {
			return 0, err
		}
		scan := table.Scan().Segment(seg, m.Workers).Consistent(true).SearchLimit(100)
		if len(cp.Next) > 0 {
			scan = scan.StartFrom(pagingKey(cp.Next))
		}
		var items []dynamo.Item
		next, err := scan.AllWithLastEvaluatedKey(ctx, &items)
		if err != nil {
			return 0, err
		}
		if phase == "copy" {
			err = m.copyItems(ctx, items, &cp, limit)
		} else {
			err = m.removeItems(ctx, items, &cp, limit)
		}
		if err != nil {
			return 0, err
		}
		cp.Next = stringKey(next)
		cp.Done = len(next) == 0
		if err := m.DB.Table(m.To).Put(cp).Run(ctx); err != nil {
			return 0, err
		}
		if cp.Done {
			return cp.Copied + cp.Removed, nil
		}
	}
}

func (m *Migration) copyItems(ctx context.Context, items []dynamo.Item, cp *migSegment, limit *limiter) error {
	for _, raw := range items {
		if !m.Types[itemType(raw)] {
			cp.Skipped++
			continue
		}
		if err := limit.wait(ctx); err != nil {
			return err
		}
		src, _, err := SourceOf(raw)
		if err != nil {
			return err
		}
		err = m.Copy(ctx, raw)
		switch {
		case errors.Is(err, ErrUnchanged):
			cp.Same++
		case errors.Is(err, ErrMigrationConflict), errors.Is(err, ErrTaken), errors.Is(err, ErrLimit),
			errors.Is(err, ErrInvalidKey), errors.Is(err, ErrFieldRequired):
			if err := m.recordConflict(ctx, src, err); err != nil {
				return err
			}
		case err != nil:
			return fmt.Errorf("copying %s / %s: %w", src.PK, src.SK, err)
		default:
			cp.Copied++
			if err := m.clearConflict(ctx, src); err != nil {
				return err
			}
		}
	}
	return nil
}

// removeItems deletes the new table's copies of items the old table no longer has.
func (m *Migration) removeItems(ctx context.Context, items []dynamo.Item, cp *migSegment, limit *limiter) error {
	bySource := map[Key]dynamo.Item{}
	var keys []dynamo.Keyed
	for _, raw := range items {
		var s struct {
			PK string `dynamo:"_msrcPK"`
			SK string `dynamo:"_msrcSK"`
		}
		if dynamo.UnmarshalItem(raw, &s) != nil || s.PK == "" || itemType(raw) == migType {
			continue
		}
		k := Key{s.PK, s.SK}
		bySource[k] = raw
		keys = append(keys, dynamo.Keys{k.PK, k.SK})
	}
	if len(keys) == 0 {
		return nil
	}
	var present []struct {
		PK string `dynamo:"PK"`
		SK string `dynamo:"SK"`
	}
	err := m.DB.Table(m.From).Batch(AttrPK, AttrSK).Get(keys...).Project(AttrPK, AttrSK).Consistent(true).All(ctx, &present)
	if err != nil && !errors.Is(err, dynamo.ErrNotFound) {
		return err
	}
	for _, p := range present {
		delete(bySource, Key{p.PK, p.SK})
	}
	for src, raw := range bySource {
		if err := limit.wait(ctx); err != nil {
			return err
		}
		if err := m.Remove(ctx, raw); err != nil {
			return fmt.Errorf("removing the copy of %s / %s: %w", src.PK, src.SK, err)
		}
		cp.Removed++
		if err := m.clearConflict(ctx, src); err != nil {
			return err
		}
	}
	return nil
}

// ItemType returns the entity or derived-item type (_t) of a stored item.
func ItemType(raw dynamo.Item) string { return itemType(raw) }

// ExpiredItem reports whether a stored item's TTL attribute has passed.
func ExpiredItem(raw dynamo.Item, ttlAttr string) bool {
	if n, ok := raw[ttlAttr].(*types.AttributeValueMemberN); ok {
		var ttl int64
		if _, err := fmt.Sscan(n.Value, &ttl); err == nil {
			return Expired(ttl, Now())
		}
	}
	return false
}

func itemType(raw dynamo.Item) string {
	if s, ok := raw[AttrType].(*types.AttributeValueMemberS); ok {
		return s.Value
	}
	return ""
}

func (m *Migration) statePK() string { return "_DYNAGO#MIGRATION#" + m.From }

func (m *Migration) state(ctx context.Context) (migState, error) {
	var st migState
	_, err := GetOne(ctx, m.DB.Table(m.To), Key{m.statePK(), "STATE"}, true, &st)
	return st, err
}

// stateTable starts an update of the migration's state item.
func (m *Migration) stateTable() *dynamo.Update {
	return m.DB.Table(m.To).Update(AttrPK, m.statePK()).Range(AttrSK, "STATE").Set(Path(AttrType), migType)
}

func (m *Migration) startPass(ctx context.Context, pass int, finishing bool) error {
	return m.stateTable().Set("pass", pass).Set("passDone", false).Set("finishing", finishing).Run(ctx)
}

func (m *Migration) setPhase(ctx context.Context, phase string) error {
	return m.stateTable().Set("phase", phase).Run(ctx)
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
		err := m.stateTable().Set("leaseOwner", owner).Set("leaseUntil", now.Add(leaseFor).Unix()).
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

func (m *Migration) renew(ctx context.Context, owner string) error {
	err := m.stateTable().Set("leaseUntil", time.Now().Add(leaseFor).Unix()).If("$ = ?", "leaseOwner", owner).Run(ctx)
	if dynamo.IsCondCheckFailed(err) {
		return fmt.Errorf("dynago migrate: lost the migration lease to another job")
	}
	return err
}

func (m *Migration) release(ctx context.Context, owner string) {
	_ = m.DB.Table(m.To).Update(AttrPK, m.statePK()).Range(AttrSK, "STATE").
		Remove("leaseOwner", "leaseUntil").If("$ = ?", "leaseOwner", owner).Run(ctx)
}

func (m *Migration) recordConflict(ctx context.Context, src Key, problem error) error {
	return m.DB.Table(m.To).Put(migConflict{PK: m.statePK(), SK: "CONFLICT#" + src.PK + "|" + src.SK, T: migType,
		Item: src.PK + " / " + src.SK, Problem: problem.Error()}).Run(ctx)
}

func (m *Migration) clearConflict(ctx context.Context, src Key) error {
	return m.DB.Table(m.To).Delete(AttrPK, m.statePK()).Range(AttrSK, "CONFLICT#"+src.PK+"|"+src.SK).Run(ctx)
}

// liveConflicts returns the conflicts whose source item still exists, dropping the others: an item
// deleted from the old table no longer needs copying.
func (m *Migration) liveConflicts(ctx context.Context) ([]migConflict, error) {
	cs, err := m.conflicts(ctx)
	if err != nil {
		return nil, err
	}
	var live []migConflict
	for _, c := range cs {
		pk, sk, _ := strings.Cut(strings.TrimPrefix(c.SK, "CONFLICT#"), "|")
		var probe struct{ PK string }
		found, err := GetOne(ctx, m.DB.Table(m.From), Key{pk, sk}, true, &probe)
		if err != nil {
			return nil, err
		}
		if !found {
			if err := m.clearConflict(ctx, Key{pk, sk}); err != nil {
				return nil, err
			}
			continue
		}
		live = append(live, c)
	}
	return live, nil
}

func (m *Migration) conflicts(ctx context.Context) ([]migConflict, error) {
	var out []migConflict
	err := m.DB.Table(m.To).Get(AttrPK, m.statePK()).Range(AttrSK, dynamo.BeginsWith, "CONFLICT#").Consistent(true).All(ctx, &out)
	if errors.Is(err, dynamo.ErrNotFound) {
		err = nil
	}
	return out, err
}

// report prints the conflicts, if any, and returns an error for them: a rollout must not go ahead
// with items missing from the new table.
func (m *Migration) report(ctx context.Context, command string) error {
	cs, err := m.liveConflicts(ctx)
	if err != nil {
		return err
	}
	if len(cs) == 0 {
		m.logf("%s: done", command)
		return nil
	}
	sort.Slice(cs, func(i, j int) bool { return cs[i].Item < cs[j].Item })
	for _, c := range cs {
		m.logf("conflict: %s: %s", c.Item, c.Problem)
	}
	return fmt.Errorf("%w: %d items of %s were not copied; fix them in the old table and run %s again", ErrMigrationConflict, len(cs), m.From, command)
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
	cs, err := m.conflicts(ctx)
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
