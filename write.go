package dynago

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"strings"
	"sync/atomic"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/guregu/dynamo/v2"
)

// Op is one item write, with the error to report if its condition is what failed.
type Op struct {
	key    Key
	put    *dynamo.Put
	update *dynamo.Update
	del    *dynamo.Delete
	check  *dynamo.ConditionCheck
	err    error
	// selfRev is the revision a create wrote. If the create's condition fails because the item
	// exists with exactly this revision, an SDK retry re-sent a create that had already succeeded.
	selfRev int64
}

// Each constructor takes the key of the item the op touches: a transaction may touch an item only
// once, and Run checks that before sending.

// PutOp wraps a put of the item at key. condErr is returned when the put's condition fails.
func PutOp(key Key, p *dynamo.Put, condErr error) Op { return Op{key: key, put: p, err: condErr} }

// CreateOp wraps the put that creates the item at key with revision rev. condErr is returned when
// the item already exists, unless it is this very write repeated by an SDK retry.
func CreateOp(key Key, p *dynamo.Put, condErr error, rev int64) Op {
	return Op{key: key, put: p.IncludeItemInCondCheckFail(true), err: condErr, selfRev: rev}
}

// UpdateOp wraps an update of the item at key. condErr is returned when its condition fails.
func UpdateOp(key Key, u *dynamo.Update, condErr error) Op {
	return Op{key: key, update: u, err: condErr}
}

// DeleteOp wraps a delete of the item at key. condErr is returned when its condition fails.
func DeleteOp(key Key, d *dynamo.Delete, condErr error) Op {
	return Op{key: key, del: d, err: condErr}
}

// CheckOp wraps a condition check on the item at key. condErr is returned when the check fails.
func CheckOp(key Key, c *dynamo.ConditionCheck, condErr error) Op {
	return Op{key: key, check: c, err: condErr}
}

func (o Op) run(ctx context.Context) error {
	switch {
	case o.put != nil:
		return o.put.Run(ctx)
	case o.update != nil:
		return o.update.Run(ctx)
	case o.del != nil:
		return o.del.Run(ctx)
	}
	return errors.New("dynago: a condition check cannot run outside a transaction")
}

// Run applies ops atomically: a single op runs as a plain write (half the cost of a
// transaction), several run as one TransactWriteItems. A failed condition is reported as the
// error registered for the op that failed.
func Run(ctx context.Context, db *dynamo.DB, ops []Op) error {
	for _, op := range ops {
		if err := op.key.Valid(); err != nil {
			return err
		}
	}
	switch {
	case len(ops) == 0:
		return nil
	case len(ops) > 100:
		return fmt.Errorf("%w (%d items)", ErrTooManyItems, len(ops))
	case len(ops) == 1 && ops[0].check == nil:
		op := ops[0]
		err := op.run(ctx)
		if err == nil || op.err == nil || !dynamo.IsCondCheckFailed(err) {
			return contention(err)
		}
		if op.selfRev != 0 {
			var existing struct {
				Rev int64 `dynamo:"_rev"`
			}
			if ok, _ := dynamo.UnmarshalItemFromCondCheckFailed(err, &existing); ok && existing.Rev == op.selfRev {
				return nil // our own create, repeated by an SDK retry after it had succeeded
			}
		}
		return &OpError{Key: op.key, Err: op.err}
	}
	seen := make(map[Key]bool, len(ops))
	for _, o := range ops {
		if seen[o.key] {
			// DynamoDB rejects a transaction touching an item twice (DynamoDB Local does not
			// always), so this is a schema shape dynago failed to refuse: report it plainly.
			return fmt.Errorf("%w: %s / %s", ErrSameItemTwice, o.key.PK, o.key.SK)
		}
		seen[o.key] = true
	}
	// The idempotency token makes the SDK's own retries (after a timeout or 5xx) safe: if the
	// first attempt committed, the retry reports success instead of failing its conditions.
	tx := db.WriteTx().Idempotent(true)
	for _, o := range ops {
		switch {
		case o.put != nil:
			tx.Put(o.put)
		case o.update != nil:
			tx.Update(o.update)
		case o.del != nil:
			tx.Delete(o.del)
		case o.check != nil:
			tx.Check(o.check)
		}
	}
	return mapTxError(tx.Run(ctx), ops)
}

// mapTxError turns a cancelled transaction into the error registered for the item that failed.
// DynamoDB returns cancellation reasons positionally aligned with the transaction's items.
func mapTxError(err error, ops []Op) error {
	var tce *types.TransactionCanceledException
	if err == nil || !errors.As(err, &tce) {
		return err
	}
	contended := false
	for i, reason := range tce.CancellationReasons {
		if reason.Code == nil {
			continue
		}
		switch *reason.Code {
		case "ConditionalCheckFailed":
			if i < len(ops) && ops[i].err != nil {
				return &OpError{Key: ops[i].key, Err: ops[i].err}
			}
			return err
		case "TransactionConflict", "ThrottlingError", "ProvisionedThroughputExceeded":
			contended = true
		}
	}
	if contended {
		return fmt.Errorf("%w: %w", ErrContention, err)
	}
	return err
}

// OpError is a write's failed condition, carrying the key of the item whose condition failed.
// It unwraps to the error registered for that item, so errors.Is works as with the bare error;
// errors.As gives the key, which the migration job uses to find who holds a claim.
type OpError struct {
	Key Key
	Err error
}

func (e *OpError) Error() string { return e.Err.Error() }
func (e *OpError) Unwrap() error { return e.Err }

// contention marks an error from a single-item write that collided with a transaction in progress
// on the item, so Retry retries it like a cancelled transaction. The SDK does not retry it.
func contention(err error) error {
	var tce *types.TransactionConflictException
	if errors.As(err, &tce) {
		return fmt.Errorf("%w: %w", ErrContention, err)
	}
	return err
}

// RetryPolicy controls how generated writes retry when the item changed under them (ErrStale) or
// another transaction held one of their items (ErrContention). Delays grow exponentially from
// BaseDelay up to MaxDelay, with jitter: each wait is between half and one and a half times the
// delay.
type RetryPolicy struct {
	Attempts  int
	BaseDelay time.Duration
	// MaxDelay caps each delay; zero means DefaultRetries.MaxDelay.
	MaxDelay time.Duration
}

// DefaultRetries is the policy generated writes use unless SetRetries changes it: 8 attempts,
// waiting 5 ms, 10 ms, ... up to 250 ms between them, about half a second in total. That rides out
// bursts of contention on a busy counter.
var DefaultRetries = RetryPolicy{Attempts: 8, BaseDelay: 5 * time.Millisecond, MaxDelay: 250 * time.Millisecond}

var retries atomic.Pointer[RetryPolicy]

// SetRetries sets the policy generated writes use. It is safe to call while writes run.
func SetRetries(p RetryPolicy) { retries.Store(&p) }

func currentRetries() RetryPolicy {
	if p := retries.Load(); p != nil {
		return *p
	}
	return DefaultRetries
}

// Retry runs a write, retrying on ErrStale and ErrContention under the retry policy (see
// SetRetries). After the last attempt it returns ErrConflict.
func Retry(ctx context.Context, fn func() error) error {
	policy := currentRetries()
	attempts := max(policy.Attempts, 1)
	maxDelay := policy.MaxDelay
	if maxDelay <= 0 {
		maxDelay = DefaultRetries.MaxDelay
	}
	for i := 0; ; i++ {
		err := fn()
		if err == nil || (!errors.Is(err, ErrStale) && !errors.Is(err, ErrContention)) {
			return err
		}
		if i == attempts-1 {
			return fmt.Errorf("%w after %d attempts: %w", ErrConflict, attempts, err)
		}
		backoff := maxDelay
		if i < 30 && policy.BaseDelay<<i < maxDelay { // i < 30 keeps the shift from overflowing
			backoff = policy.BaseDelay << i
		}
		if backoff <= 0 {
			backoff = time.Millisecond
		}
		jitter := time.Duration(rand.Int64N(int64(backoff)))
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff/2 + jitter):
		}
	}
}

// Path quotes an attribute name for guregu's update builders unless it is a plain identifier.
// Names such as "_rev" are not valid bare identifiers in DynamoDB expressions; quoting makes guregu
// substitute a placeholder for them.
func Path(attr string) string {
	for i, r := range attr {
		letter := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z'
		if !letter && (i == 0 || r < '0' || r > '9') {
			return "'" + attr + "'"
		}
	}
	return attr
}

// CheckKeyPart rejects a value that contains a character the key template uses to separate its
// parts: "a#b" + "c" and "a" + "b#c" would otherwise render the same key. The value is checked as
// given and as {field|lower} renders it, since lower-casing (and Unicode normalisation) can
// produce a separator: "AX" before a literal "x", or the Kelvin sign becoming "k".
func CheckKeyPart(field, value, separators string) error {
	if strings.ContainsAny(value, separators) || strings.ContainsAny(Lower(value), separators) {
		return fmt.Errorf("%w: %s may not contain any of %q, which separate key parts", ErrInvalidKey, field, separators)
	}
	return nil
}

// Set is one attribute change in an update. Zero values are removed rather than stored, which
// matches how items are written (every non-key attribute is omitted when empty).
type Set struct {
	Attr   string
	Value  any
	Remove bool
	// StringSet stores Value as a DynamoDB string set rather than a list.
	StringSet bool
	// Add adds the number Value to the attribute (an absent one counts as 0) instead of replacing
	// it: DynamoDB applies it atomically, so concurrent additions all count.
	Add bool
	// AddElems and RemoveElems change a string set by element: Value is a []string of elements
	// to add to it, or to remove from it, whatever else it holds. Both are atomic and idempotent,
	// and an empty Value changes nothing.
	AddElems, RemoveElems bool
}

// Cond is a precondition on one attribute: that it equals Value; with Not, that it differs from
// Value; with In, that it equals one of several values. Zero says the value compared with (or one
// of In) is the field's zero value, which is stored as an absent attribute.
type Cond struct {
	Attr  string
	Value any
	Zero  bool
	// Not requires the attribute to differ from Value.
	Not bool
	// In, if set, lists the values the attribute may have; Value is unused.
	In []any
}

// expr renders the condition with guregu placeholders ($ for names, ? for values), in
// parentheses when it has more than one term.
func (c Cond) expr() (string, []any) {
	switch {
	case len(c.In) > 0:
		in := "$ IN (?" + strings.Repeat(", ?", len(c.In)-1) + ")"
		args := append([]any{c.Attr}, c.In...)
		if c.Zero {
			return "(attribute_not_exists($) OR " + in + ")", append([]any{c.Attr}, args...)
		}
		return in, args
	case c.Not && c.Zero:
		// Anything but the zero value: the attribute is there, and isn't a stored zero.
		return "(attribute_exists($) AND $ <> ?)", []any{c.Attr, c.Attr, c.Value}
	case c.Not:
		return "(attribute_not_exists($) OR $ <> ?)", []any{c.Attr, c.Attr, c.Value}
	case c.Zero:
		return "(attribute_not_exists($) OR $ = ?)", []any{c.Attr, c.Attr, c.Value}
	}
	return "$ = ?", []any{c.Attr, c.Value}
}

// Guard is the conditions every write to an existing item carries.
type Guard struct {
	// ExpectRev, if non-zero, is the revision the caller read (a document version).
	ExpectRev int64
	// TTLAttr, if set, makes an expired item count as absent.
	TTLAttr string
	// NotFound is returned if the item is absent (or expired).
	NotFound error
}

// UpdateFields changes attributes of an existing item in one UpdateItem, bumping its revision,
// and returns the new revision. It is used for updates that feed no counters, claims, copies or
// index keys, so no read is needed. Besides the Guard's errors it returns precondition if a Cond
// fails.
func UpdateFields(ctx context.Context, t dynamo.Table, key Key, sets []Set, when []Cond, g Guard, precondition error) (int64, error) {
	if err := key.Valid(); err != nil {
		return 0, err
	}
	u := t.Update(AttrPK, key.PK).Range(AttrSK, key.SK)
	SetFields(u, sets)
	now := Now()
	GuardUpdate(u, g, now)
	for _, c := range when {
		CondUpdate(u, c)
	}
	var out struct {
		Rev int64 `dynamo:"_rev"`
	}
	err := u.OnlyUpdatedValue(ctx, &out)
	if err == nil || !dynamo.IsCondCheckFailed(err) {
		return out.Rev, contention(err)
	}
	return 0, whyFailed(ctx, t, key, g, now, precondition)
}

// SetFields applies attribute changes to an update, bumps the item's revision and stamps it as
// updated now.
func SetFields(u *dynamo.Update, sets []Set) {
	for _, s := range sets {
		switch {
		case s.Remove:
			u.Remove(Path(s.Attr))
		case s.AddElems || s.RemoveElems:
			elems, _ := s.Value.([]string)
			switch {
			case len(elems) == 0: // DynamoDB refuses an empty set
			case s.AddElems:
				u.AddStringsToSet(Path(s.Attr), elems...)
			default:
				u.DeleteStringsFromSet(Path(s.Attr), elems...)
			}
		case s.Add:
			u.Add(Path(s.Attr), s.Value)
		case s.StringSet:
			u.SetSet(Path(s.Attr), s.Value)
		default:
			u.Set(Path(s.Attr), s.Value)
		}
	}
	u.Add(Path(AttrRev), 1)
	u.Set(Path(AttrUpdated), NewStamp())
}

// KeepUnknown returns item, marshalled, with every attribute of raw (the stored item it replaces)
// that known doesn't name. Code at an older compatible version rewriting an item that newer code
// wrote must not drop the fields it doesn't know: within a table generation, versions only add
// or remove plain fields, so keeping what it doesn't know is always safe.
func KeepUnknown(raw dynamo.Item, known map[string]bool, item any) (dynamo.Item, error) {
	out, err := dynamo.MarshalItem(item)
	if err != nil {
		return nil, err
	}
	for k, v := range raw {
		if _, set := out[k]; !set && !known[k] {
			out[k] = v
		}
	}
	return out, nil
}

// Requirement is the state a write requires of another item, checked in the write's transaction.
type Requirement struct {
	Key Key
	// When lists values the item must have.
	When []Cond
	// TTLAttr, if set, makes an expired item count as absent.
	TTLAttr string
	// Optional lets an absent (or expired) item pass: When applies only to an item that is there.
	Optional bool
}

// condition renders the requirement as a condition expression with guregu placeholders ($ for
// names, ? for values). It is "" when anything passes.
func (r Requirement) condition(now int64) (string, []any) {
	var match []string
	var args []any
	for _, c := range r.When {
		expr, cargs := c.expr()
		match = append(match, expr)
		args = append(args, cargs...)
	}
	if r.Optional {
		if len(match) == 0 {
			return "", nil
		}
		absent, aargs := "attribute_not_exists($)", []any{AttrPK}
		if r.TTLAttr != "" {
			absent += " OR $ <= ?"
			aargs = append(aargs, r.TTLAttr, now)
		}
		return absent + " OR (" + strings.Join(match, " AND ") + ")", append(aargs, args...)
	}
	expr, pargs := "attribute_exists($)", []any{AttrPK}
	if r.TTLAttr != "" {
		expr += " AND (attribute_not_exists($) OR $ > ?)"
		pargs = append(pargs, r.TTLAttr, r.TTLAttr, now)
	}
	for _, m := range match {
		expr += " AND " + m
	}
	return expr, append(pargs, args...)
}

// CheckRequirement builds a condition check that another item meets a requirement.
func CheckRequirement(t dynamo.Table, r Requirement) *dynamo.ConditionCheck {
	c := t.Check(AttrPK, r.Key.PK).Range(AttrSK, r.Key.SK)
	if expr, args := r.condition(Now()); expr != "" {
		c.If(expr, args...)
	}
	return c
}

// ConsumeRequirement builds the deletion of another item that meets a requirement.
func ConsumeRequirement(t dynamo.Table, r Requirement) *dynamo.Delete {
	d := t.Delete(AttrPK, r.Key.PK).Range(AttrSK, r.Key.SK)
	if expr, args := r.condition(Now()); expr != "" {
		d.If(expr, args...)
	}
	return d
}

// CheckAbsent builds a condition check that an item is absent or expired. A write that read an
// optional item and found none uses it to fail if one appeared before the write commits.
func CheckAbsent(t dynamo.Table, key Key, ttlAttr string) *dynamo.ConditionCheck {
	c := t.Check(AttrPK, key.PK).Range(AttrSK, key.SK)
	if ttlAttr != "" {
		return c.If("attribute_not_exists($) OR $ <= ?", AttrPK, ttlAttr, Now())
	}
	return c.If("attribute_not_exists($)", AttrPK)
}

// CheckCounter builds a condition check on a counter item's values. A missing value (or item)
// counts as zero.
func CheckCounter(t dynamo.Table, key Key, values []Cond) *dynamo.ConditionCheck {
	c := t.Check(AttrPK, key.PK).Range(AttrSK, key.SK)
	for _, v := range values {
		expr, args := v.expr()
		c.If(expr, args...)
	}
	return c
}

// ConsumeCounter builds the deletion of a counter item whose values meet the conditions: a counter
// that has returned to zero, removed with the thing it counted for. A missing item passes.
func ConsumeCounter(t dynamo.Table, key Key, values []Cond) *dynamo.Delete {
	d := t.Delete(AttrPK, key.PK).Range(AttrSK, key.SK)
	for _, v := range values {
		expr, args := v.expr()
		d.If(expr, args...)
	}
	return d
}

// ReadIfNeeded runs a write that also changes other items. It first builds the changes assuming
// those items are in the state the schema requires, without reading them; if that assumption
// fails (ErrNeedsRead), it runs again reading them, which reports precisely what was wrong.
func ReadIfNeeded(fn func(read bool) error) error {
	if err := fn(false); !NeedsRead(err) {
		return err
	}
	return fn(true)
}

// NeedsRead reports whether a write attempted without reading the item found that its
// assumptions did not hold, so it must be retried by reading first.
func NeedsRead(err error) bool { return errors.Is(err, ErrNeedsRead) }

// GuardUpdate adds a Guard's conditions to an update.
func GuardUpdate(u *dynamo.Update, g Guard, now int64) {
	u.If("attribute_exists($)", AttrPK)
	if g.TTLAttr != "" {
		u.If("attribute_not_exists($) OR $ > ?", g.TTLAttr, g.TTLAttr, now)
	}
	if g.ExpectRev != 0 {
		u.If("$ = ?", AttrRev, g.ExpectRev)
	}
}

// CondUpdate adds a precondition to an update.
func CondUpdate(u *dynamo.Update, c Cond) {
	expr, args := c.expr()
	u.If(expr, args...)
}

// DeleteIfExists deletes an item that has no derived items, returning the Guard's errors if it is
// absent, expired or at another revision.
func DeleteIfExists(ctx context.Context, t dynamo.Table, key Key, g Guard) error {
	if err := key.Valid(); err != nil {
		return err
	}
	now := Now()
	d := t.Delete(AttrPK, key.PK).Range(AttrSK, key.SK).If("attribute_exists($)", AttrPK)
	if g.TTLAttr != "" {
		d.If("attribute_not_exists($) OR $ > ?", g.TTLAttr, g.TTLAttr, now)
	}
	if g.ExpectRev != 0 {
		d.If("$ = ?", AttrRev, g.ExpectRev)
	}
	err := d.Run(ctx)
	if dynamo.IsCondCheckFailed(err) {
		return whyFailed(ctx, t, key, g, now, nil)
	}
	return contention(err)
}

// whyFailed tells apart the conditions of a failed single-item write by reading the item once.
func whyFailed(ctx context.Context, t dynamo.Table, key Key, g Guard, now int64, precondition error) error {
	var raw dynamo.Item
	err := t.Get(AttrPK, key.PK).Range(AttrSK, dynamo.Equal, key.SK).Consistent(true).One(ctx, &raw)
	switch {
	case errors.Is(err, dynamo.ErrNotFound):
		return g.NotFound
	case err != nil:
		return err
	}
	var probe struct {
		Rev int64 `dynamo:"_rev"`
	}
	if err := dynamo.UnmarshalItem(raw, &probe); err != nil {
		return err
	}
	if g.TTLAttr != "" {
		if v, ok := raw[g.TTLAttr]; ok {
			var ttl int64
			if dynamo.Unmarshal(v, &ttl) == nil && Expired(ttl, now) {
				return g.NotFound
			}
		}
	}
	switch {
	case g.ExpectRev != 0 && probe.Rev != g.ExpectRev:
		return ErrVersionMismatch
	case precondition != nil:
		return precondition
	}
	// The item changed between the write and this read in a way that now satisfies every
	// condition: report it as a lost race.
	return ErrStale
}

// Expired reports whether a TTL attribute value (epoch seconds, 0 for none) has passed.
// DynamoDB deletes expired items lazily, often hours later, so reads must check.
func Expired(ttl, now int64) bool {
	return ttl != 0 && ttl <= now
}

// Now returns the current time in epoch seconds, for TTL checks.
func Now() int64 { return time.Now().Unix() }
