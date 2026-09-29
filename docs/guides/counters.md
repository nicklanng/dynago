# Counters

A counter is an item holding counts or sums, updated with atomic `ADD`s **in the same transaction**
as every entity write that changes them. It never drifts from the items it counts, and reading it
is one GetItem, however many items there are.

```yaml
Member:
  counters:
    MemberCounts:
      pk: "LIB#{libraryId}"
      sk: "COUNTS#MEMBERS"
      values:
        active: { count: true, where: { status: active } }
        suspended: { count: true, where: { status: suspended } }
        stewards: { count: true, where: { role: steward, status: active }, min: 1 } # never below one
Loan:
  counters:
    MemberLoans:
      pk: "LIB#{libraryId}#MEMBER#{memberId}"
      sk: "LOANS"
      values:
        active: { count: true, where: { status: active }, limit: arg } # capped by the caller
```

```go
counts, err := st.Libraries.MemberStats(ctx, toollibrary.MemberCountsKey{LibraryID: lib})
fmt.Println(counts.Active, counts.Stewards)
```

A counter can be read from any entity's store: here the library page reads the member counts.

## How the counts stay exact

For every write, the generated code computes each counter value's contribution **before** and
**after** the write, from the entity's fields, and adds the difference. So:

- a member joining adds `active +1` (and `stewards +1` if they're a steward);
- suspending moves them from `active` to `suspended` (and takes `stewards -1` if they're a steward);
- stepping down as an active steward adds `stewards -1` and nothing else;
- leaving subtracts everything the member contributed.

Because this is computed in one place from the schema, every write path agrees. There is no
hand-written "remember to decrement X in Y".

An update changing what a counter depends on normally needs the "before" state. There are two ways to
avoid reading it:

- **Read-free writes.** If every input of the changed counters is a key field or pinned by `when`,
  the change is known from the call itself. `Retire: { set: { status: retired }, when: { status:
  available } }` always moves exactly one tool from `available` to `retired`, so it runs as one
  transaction of conditional writes with no read. If the tool isn't available after all, the
  conditions fail and the write falls back to reading, which reports the precondition error.

  `Suspend: { set: { status: suspended }, when: { status: active } }` is not read-free: whether it
  lowers `stewards` depends on the member's role, which it doesn't pin. It reads the member first.
  Adding a field to a value's `where` can change which writes read, and the model document says
  which do.
- **`dynago.From(entity)`.** Pass the entity you already loaded; it becomes the before state. See
  [Concurrency](concurrency.md).

Updates that change nothing a counter depends on don't touch it.

## Conditional counts: `where`

`where` counts only entities matching all its conditions:
`{ count: true, where: { status: active, role: steward } }`. An entity moving in or out of the
condition adds or removes its contribution.

## Upper bounds: `limit`

`limit` makes the counter enforce a cap. The ADD is conditioned on the value staying within the
limit, so there is no race: ten concurrent borrows against a member's three-loan cap admit exactly
three (the example's tests check this).

- `limit: 50`: a constant ("at most 50 tools out per library").
- `limit: arg`: the caller passes the limit on each write that can grow the value, as a
  `dynago.Limit` in a generated `<Entity><Write>Limits` struct. Use it when the cap lives
  elsewhere, such as a member's `maxLoans`:

```go
err := st.Loans.Borrow(ctx, loan, toollibrary.LoanBorrowLimits{MemberLoansActive: dynago.Max(member.MaxLoans)})
if errors.Is(err, toollibrary.ErrMemberLoansActiveLimit) {
    // return something first
}
```

**A limit must always be given.** `dynago.Max(n)` caps the value; `dynago.Unlimited()` switches the
cap off on purpose. The zero `dynago.Limit` is an error (`dynago.ErrLimitRequired`), so a forgotten
limit can never silently disable enforcement.

Only writes that can grow the value take a limit. `Return: { set: { status: returned } }` can only
lower `active`, so it has no limits parameter. Decreases are never checked against the limit.

## Lower bounds: `min`

`min` stops a value dropping below a floor: "a library always keeps at least one active steward".

```yaml
stewards: { count: true, where: { role: steward, status: active }, min: 1 }
```

A write that would take `stewards` from 1 to 0 (the last active steward stepping down, being
suspended or leaving) fails
with `ErrMemberCountsStewardsMin`, and nothing is written. As with limits, it is a condition on the
counter update, so two stewards stepping down at once cannot both succeed. Only decreases are
checked: a new library with no stewards yet can gain its first.

## Sums: `sum`

`{ sum: loanDays }` adds up an `int` field instead of counting. Changing the field adds the
difference; a `limit` or `min` on a sum works the same way as on a count.

## Sparse counters

A counter is only touched while every string, enum and time field in its key templates is set. A
counter keyed by an optional field (say, an invite code a member joined with) is simply not written
for members without one.

## Hot counters and sharding

Every write to a counter serialises on one item. A single item takes at most ~1,000 WRU/s, and
transactions touching it at the same moment conflict. The generated code retries with backoff
(about half a second in total by default; see `dynago.SetRetries`), then returns `dynago.ErrConflict`. Declare
`hot_key_rate` on writes and `dynago check` estimates the load on each counter item. It warns when
transactions on one item are frequent enough for conflicts to be routine (about 20 a second), and
suggests a shard count.

For counters written very often (every loan across a library, say), set `shards: N`. Writes spread
over N items (each entity always lands on the same shard), and reads sum them with one
BatchGetItem. Bounded values (`limit`, `min`) cannot be sharded, because the bound needs one item
to check: put them in a counter of their own.

## Adding a counter to existing data

Existing items were never counted, so a new counter, a new value, or a changed definition needs a
new table generation. The migration job copies every entity into the new table, and each copy
adds its contribution, so the counter starts exact. See [Migrations](migrations.md). Removing a
counter or value is compatible: its counts stay on the counter item, unused.

## Caveats

- **TTL**: an item deleted by TTL is not subtracted, so a counter over an expiring entity counts
  items created, not items that exist. `dynago check` warns.
- **Several bounded values on one counter item**: if a write breaks one, DynamoDB cannot say which,
  so the error matches all of their sentinels.
- Counters count one entity's items. Counting across entities means one counter per entity type.
