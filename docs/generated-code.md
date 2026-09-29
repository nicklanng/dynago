# Generated code

`dynago generate` writes one Go file per schema (`<table>_dynago.go`). This page lists everything
in it, how it is named, and how each generated method behaves. The generated code depends only on
[guregu/dynamo](https://github.com/guregu/dynamo) and the small runtime package
`github.com/nicklanng/dynago`, whose exported names you also use: errors, `Page`, `Limit`, `Max`,
`Unlimited`, `Ptr`, `From`, `IfVersion`, `ReturnVersion`, `Timestamps`, `SignCursors` and
`SetRetries`.

Names below come from [the example](../examples/toollibrary): entities `Loan`, `Member`, `Tool`,
counter `MemberLoans`, index `Overdue`, and so on.

## Package level

| Name | What it is |
|---|---|
| `Generation`, `TableName(base) string` | The table generation this code uses, and its table's name for a base name: `TableName("prod-toollibrary")` is `"prod-toollibrary-g3"`. See [Schema changes](guides/schema-changes.md). |
| `TableSpec` | The table's physical shape (GSIs, TTL attribute), a `dynago.TableSpec`. |
| `Store` | One field per entity, named as the plural of the entity: `Store.Loans`, `Store.Libraries`. |
| `New(db *dynamo.DB, tableName string) *Store` | Returns a store for the named table. The table name is a parameter, so environments can differ. |
| `EnsureTable(ctx, db, tableName) error` | Creates the table (and enables TTL) if it does not exist. For local development and tests; production tables belong in the generated Terraform. |
| `RunMigration(ctx, db, args, out) error` | The migration job's command line: `copy`, `finish` or `status`. Generation 1 has nothing to migrate. See [Migrations](guides/migrations.md). |
| `NewMigration(db, base) *dynago.Migration` | The migration job, for running it from your own code. Generation 2 and later. |
| `LibraryG2`, `MigrateLibrary`, `AutoMigrateLibrary` | Per entity, generation 2 and later: how the previous generation stored it; the conversion you set when fields can't carry over by themselves; the field-by-field conversion. |

```go
db := dynamo.New(cfg)                                   // guregu/dynamo
st := toollibrary.New(db, toollibrary.TableName("prod-toollibrary"))
tool, err := st.Tools.Get(ctx, toollibrary.ToolKey{LibraryID: lib, ToolID: id})
```

## Per entity

For an entity `Loan`:

| Name | Declared by | What it is |
|---|---|---|
| `Loan` | the entity | A struct with one exported field per schema field, tagged for guregu. Has `Key()`, `Version()` and `Timestamps()`. |
| `LoanKey` | the key templates | The primary key fields, in template order. |
| `(*Loan).Key() LoanKey` | | The entity's key. |
| `(*Loan).Version() string` | | The opaque version the entity was read or created at, or `""` if the store never saw it. See [Concurrency](guides/concurrency.md). |
| `(*Loan).Timestamps() dynago.Timestamps` | | When the stored item was first written and last changed. See [Timestamps](#timestamps). |
| `LoanStatus` + `LoanStatusActive`, … | an `enum` field `status` | A string type and one constant per value. |
| `LoanStore` | | The methods below. Get it from `Store.Loans`. |
| `LoanOverdue` | an index `Overdue` without `project: all` | What a query through the index returns: key fields plus projected fields. |
| `LoanHistoryItem` | a `query: key` access `History` with `project` | What the projected query returns. |
| `MemberLoans`, `MemberLoansKey` | a counter `MemberLoans` | The counter's values (`int64` fields) and the fields that address it. |
| `Loan<Access>Query` | a `query` access pattern | Its partition key fields, plus `From, To *T` when it has a `range`. |
| `Loan<Write>` | an update with `update:` or `patch:` fields | The values the caller supplies; `patch` fields are pointers. |
| `Loan<Write>Limits` | a write that can grow a `limit: arg` counter value | One `dynago.Limit` per value, named `<Counter><Value>`. |

### Errors

Each error wraps a `dynago` sentinel, so match either the specific or the general one:

```go
switch {
case errors.Is(err, toollibrary.ErrMemberLoansActiveLimit): // this member's cap
case errors.Is(err, dynago.ErrLimit):                       // any limit
}
```

| Name | Wraps | When |
|---|---|---|
| `Err<Entity>NotFound` | `dynago.ErrNotFound` | A get, update or delete found no item (or only an expired one); a unique lookup found no holder. |
| `Err<Entity>Exists` | `dynago.ErrExists` | A create found an item at that key that hasn't expired. |
| `Err<Entity><Write>Precondition` | `dynago.ErrPrecondition` | An update's `when` did not hold. |
| `Err<Entity><Write>Requires<Target>` | `dynago.ErrPrecondition` | A `requires` check failed: the other item is missing or expired (unless `optional`), not in the required state, or a counter doesn't have the required value. |
| `Err<Entity><Unique>Taken` | `dynago.ErrTaken` | The unique value belongs to another item. |
| `Err<Counter><Value>Limit` | `dynago.ErrLimit` | The write would take the value above its `limit`. |
| `Err<Counter><Value>Min` | `dynago.ErrLimit` | The write would take the value below its `min`. |

And, from the runtime directly:

| Error | When | Retried by the generated code? |
|---|---|---|
| `dynago.ErrInvalidKey` | A key field is empty, contains a separator character of its template, or the entity passed with `From` is a different item. | no |
| `dynago.ErrFieldRequired` | A write left a `required: true` field at its zero value. | no |
| `dynago.ErrLimitRequired` | A write that takes a `limit: arg` value was not given one. Pass `dynago.Max(n)`, or `dynago.Unlimited()` deliberately. | no |
| `dynago.ErrInvalidCursor` | A page cursor is malformed, came from another query, partition or range, from before a change to how the query is keyed, or fails its signature. | no |
| `dynago.ErrVersionMismatch` | A version was supplied and the item has changed since. | no: the caller decides |
| `dynago.ErrVersionRequired` | The write is `versioned: required` and no version was supplied. | no |
| `dynago.ErrConflict` | The item kept changing, or other transactions kept holding its items (a single-item write can be blocked by a transaction too), until retries ran out (about half a second by default; `dynago.SetRetries` changes the policy). | it is the result of retries |
| `dynago.ErrMigrationConflict` | The migration job couldn't copy some items (see [Migrations](guides/migrations.md#conflicts)). | no: fix the data and run it again |
| `dynago.ErrSameItemTwice` | A write would touch one item twice in a transaction, which DynamoDB rejects. dynago refuses the schema shapes that cause it, so this means a bug: please report it. | no |
| `dynago.ErrTooManyItems` | The write would exceed DynamoDB's 100-item transaction limit. (Transactions are also limited to 4 MB in total, which DynamoDB itself reports.) | no |

When several bounded values of one counter item are checked in a single write, DynamoDB reports that
the item's condition failed, not which clause, so the error matches each of their sentinels.

## Reads

Every read is one request, except a lookup by unique value (two). None of them page internally,
and none scan, except a read declared as a `scan`. Items whose `ttl` time has passed are treated as absent everywhere, even though
DynamoDB may not delete them for days.

### `get: key`

```go
func (s *ToolStore) Get(ctx context.Context, k ToolKey) (*Tool, error)
```

One GetItem (strongly consistent with `consistent: true`). Returns `ErrToolNotFound` if absent.
The entity carries its version.

### `get: { unique: Name }`

```go
func (s *MemberStore) GetByEmail(ctx context.Context, libraryID string, email string) (*Member, error)
```

One parameter per unique field; for a `string_set` field, one element (`barcodesElem string`). A
strongly consistent read of the claim, then of the item it points to; returns
`Err<Entity>NotFound` if no item holds the value (or the set no longer contains the element).

### `query`

```go
func (s *LoanStore) Overdue(ctx context.Context, q LoanOverdueQuery, page dynago.Page) ([]LoanOverdue, string, error)
```

Exactly one Query request per call. `page.Size` defaults to the pattern's `page` and is clamped to
`max_page`; `page.Cursor` is the cursor the previous call returned. The result is at most
`page.Size` items and the next cursor, `""` at the end. A page can hold fewer items than asked for
(expired items are left out) and still have a next cursor; keep going until the cursor is `""`.

```go
page := dynago.Page{Size: 25}
for {
    tools, next, err := st.Tools.Catalogue(ctx, q, page)
    if err != nil { return err }
    // use tools
    if next == "" { break }
    page.Cursor = next
}
```

Cursors are opaque, URL-safe strings, tied to the access pattern, the partition and the range
bounds: reused anywhere else they fail with `dynago.ErrInvalidCursor`. They are also tied to the
query's shape (the keys and index it reads, and its order), but not to the entity's schema
version: a new version that leaves the query alone keeps its cursors working, so a client paging
through a rolling deploy isn't interrupted. A change to how the query is keyed refuses older
cursors, rather than resuming from a key that means something else now.
Call `dynago.SignCursors(secret)` at startup to make them tamper-proof too; they still show the
key of the last item, so sign them if keys are sensitive. To rotate the secret, deploy
`dynago.SignCursors(next, current)` (signing with `next`, still accepting `current`), then drop
`current` once old cursors have gone out of use.

Queries on the entity's own partition or through `project: all` indexes return entities. Through
a GSI they carry the version they were indexed at, which may be a moment old: a write passed one
with `dynago.From` fails with `ErrVersionMismatch` if so. Copy items don't store the revision, so
entities read through a copy index have no version (`Version()` is `""`, and `dynago.From` refuses
them): get the item when you need one. Other indexes return the `<Entity><Index>` view type, and
projected base queries the `<Entity><Access>Item` type.

### `scan`

```go
func (s *LoanStore) Export(ctx context.Context, page dynago.Page) ([]Loan, string, error)
```

A declared exception: exactly one Scan request per call, over **the whole table**, filtered to the
entity's items (and, for an entity with a `ttl`, to unexpired ones). `page.Size` counts items
evaluated, of every kind, so a page can hold few or none of the entity's items and still have a
next cursor: keep going until the cursor is `""`. Cursors work as for queries. The method's doc
comment carries the schema's `reason`.

### `counter`

```go
func (s *LibraryStore) MemberStats(ctx context.Context, k MemberCountsKey) (MemberCounts, error)
```

One GetItem, or a BatchGetItem over all shards of a sharded counter, summed (DynamoDB may return
some keys unprocessed, which are fetched again). A counter that
nothing has written reads as zero. The method can live on any entity's store (here the library
reads the member counts).

## Writes

Every write is atomic: either all of its items change or none do. The request shape of each write is
decided from the schema, and the model document lists it.

### create

```go
func (s *LoanStore) Borrow(ctx context.Context, e *Loan, limits LoanBorrowLimits) error
```

The `limits` parameter appears only if the write can grow a `limit: arg` counter value. Every
limit must be given (`dynago.Max(n)` or `dynago.Unlimited()`).

Sets the write's `set` constants on `e` (overriding what the caller passed) and checks `required`
fields. Then writes the item on the condition that no unexpired item exists at the key, plus every
claim, copy and counter contribution the entity has, and everything its `requires` declare. With
none of those it is a single PutItem; otherwise a transaction. Afterwards `e.Version()` is the new
item's version.

### update

```go
func (s *LoanStore) Return(ctx context.Context, k LoanKey, v LoanReturn, opts ...dynago.WriteOption) error
func (s *MemberStore) UpdateProfile(ctx context.Context, k MemberKey, v MemberUpdateProfile, opts ...dynago.WriteOption) error
```

`v` is present if the update has `update:` or `patch:` fields; `patch` fields are pointers, and
`nil` leaves a field unchanged (`dynago.Ptr` makes pointers). An update whose patch fields are all
`nil` and that has no other changes writes nothing. `limits` is present if the update can grow a
`limit: arg` value. `opts` takes `dynago.From(entity)`, `dynago.IfVersion(version)` and
`dynago.ReturnVersion(&s)`.

The generator picks one of three shapes:

| Shape | When | Requests |
|---|---|---|
| **Single update** | The changed fields feed no index key, `where`, copy, claim or counter, and the write has no `requires`. | One conditional UpdateItem. No read. (Only if the condition fails is the item read once, to report which condition failed.) |
| **Read-free** | It changes no field used in an index's keys or `where`, or projected into a copy; the counters and claims it changes depend only on key fields and fields its `when` pins, e.g. `Retire: { set: { status: retired }, when: { status: available } }`, and its `requires` use only fields known from the call. | One transaction: the conditional update plus the counter changes and `requires`, with no read. If the item isn't in the assumed state, it falls back to the read-first shape, which reports exactly why. `From` and `ReturnVersion` use read-first directly: they need the stored state. |
| **Read-first** | Anything else. | A consistent GetItem (skipped with `dynago.From`), then a transaction (or a single PutItem if there are no derived items) conditioned on the revision read. |

A `patch` update is decided per call as well: if every patch field that feeds something derived is
`nil`, the call runs as a single update. `UpdateProfile` with only a new phone number doesn't read;
with a new name, which moves the directory entry, it does.

The read-first shape computes every derived item before and after the change and writes only the
differences: counter deltas, claims to take and release, copies to put and delete. If the item
changed between the read and the write, it re-reads and tries again, unless a version was
supplied, in which case it returns `dynago.ErrVersionMismatch`.

`when` preconditions fail with `Err<Entity><Write>Precondition`, and a `required` field set to
its zero value with `dynago.ErrFieldRequired`.

### requires

A `requires` entry adds items to the write's transaction:

| Declared | In the transaction |
|---|---|
| `when` only | A ConditionCheck: the item exists, hasn't expired and matches (with `optional`, an absent or expired item passes). |
| a counter | A ConditionCheck on the counter item's values; a missing value counts as 0. |
| `set` | An update of the other entity, with its revision bumped and its counters, claims, copies and index keys maintained. |
| `consume` | A delete of the other entity, releasing what it contributed. |

A change to another entity is built by an unexported method on the writing store
(`requireBorrowTool`). When its derived changes are known from `when` and `set`, it is first
written without reading the other item, conditioned on the required state. If that condition
fails, the transaction is cancelled and the write runs again reading the other item
(`dynago.ReadIfNeeded`), which either reports the precondition error or writes the change guarded
by that item's revision. Otherwise the other item is always read first. Either way, the item
changes only in the same transaction as the write.

### delete

```go
func (s *MemberStore) Leave(ctx context.Context, k MemberKey, opts ...dynago.WriteOption) error
```

An entity with no claims, copies, counters or `requires` is deleted with one conditional
DeleteItem. Otherwise the item is read (or taken from `dynago.From`) so its contributions can be
released in the same transaction.

## Timestamps

Every row dynago writes records when it was first written (`_created`) and last changed
(`_updated`): entity items, copies, uniqueness claims and counter items. The generated code sets
them on every write path (creates, each update shape, changes made through `requires`, counter
updates), so they can't be forgotten, and callers can't set them.

```go
tool, _ := st.Tools.Get(ctx, key)
ts := tool.Timestamps()          // dynago.Timestamps{Created, Updated time.Time}
```

- A create stamps both, and afterwards `e.Timestamps()` returns them.
- Every change stamps `_updated`; `_created` never changes.
- A copy carries its entity's creation time, and is stamped updated when it is written. A counter
  item records when it was first and last counted; a claim, when the value was taken.
- The migration job copies an item with its timestamps: moving to a new table generation doesn't
  change it.
- Items written before dynago kept timestamps have none until they are rewritten, and then only
  `_updated`: their creation time stays unknown (zero), rather than wrong.
- They are stored in the same fixed-width UTC format as times in keys, so they sort and read
  naturally in the console.
- The time comes from the clock of the server that made the write. Across servers, timestamps are
  ordered only as well as those clocks agree: fine for "when did this change", not for deciding
  which of two concurrent writes came first (the revision does that).
- They are on the item, not in index projections: entities read through a GSI or copy with
  `project: all` carry them (a copy's `_updated` is when the copy was last written); narrower
  views don't.

## What every write guarantees

- **Atomic derived items.** Claims, copies, counters and everything `requires` declares (checks,
  and changes to other entities) are in the same transaction as the item, so they never disagree
  with it. GSI entries are maintained by DynamoDB,
  eventually consistently.
- **No lost read-modify-writes.** Read-first writes are conditioned on the revision they read.
  Revisions start at a random value, so an item deleted and re-created in between is detected.
- **Retries are safe.** Transactions carry an idempotency token. A single-item create that the SDK
  retries after it had already succeeded recognises its own item by its random revision and
  reports success.
- **Rolling deploys and rollbacks.** Code at two compatible versions shares a table. A rewrite
  keeps the attributes this code doesn't know, so an older version never drops a field a newer one
  stored. Changes to derived items need a new table generation, so two versions sharing a table
  always agree on them.
- **Expired items are absent.** Updates and deletes treat an item past its `ttl` as not found, and
  a create may replace one.
- **No key collisions.** A string or enum value containing the character that follows it in a key
  identifying an item (the item's own key, copies, claims, counters, required items, GSI partition
  keys) is rejected with `dynago.ErrInvalidKey`, since it could render another item's key. A field
  at the end of its template needs no check, nor do GSI sort keys, which needn't be unique: a tool
  can be called "Drill #2". Updates check only the fields they change. Fields linked by a
  `requires` key share their checks: a loan's `toolId` appears mid-key in its MyLoans copy, so
  `Tools.Add` refuses a `toolId` containing `#`, rather than every later borrow of that tool
  failing.
- **Exact counters across schema changes.** A new counter, claim or index starts in a new table
  generation, built from every entity by the migration job. See
  [Schema changes](guides/schema-changes.md).
- **Limits are never silently off.** A missing caller-supplied limit is an error.

## What the generated code does not do

- It does not expose raw DynamoDB access, or filters (the only filters are the one that drops
  expired items, on entities with a `ttl`, and a declared scan's filter to its entity's items). A
  read that is not declared does not
  exist. Add an access pattern to the schema instead. `dynago vet` finds code that reaches
  DynamoDB around the store.
- It does not change another entity in arbitrary ways: `requires` can set constants (or the
  writer's field values) on another entity and delete it, not compute new values from it.
- It does not decrement counters or release claims and copies when DynamoDB deletes an expired item.
