# Modelling: choosing keys, indexes, copies, claims and counters

DynamoDB has no query planner. Every read is either one keyed request or an expensive mistake, so a
DynamoDB model starts from the questions the application asks, not from the entities. This guide
walks through the decisions a dynago schema makes explicit.

## 1. List the access patterns first

Write down every read the application needs, with how it is bounded and how often it runs:

| Question | Bound | Rate |
|---|---|---|
| Get a tool | one | 50/s |
| The catalogue: a category's tools by name | paged | 20/s |
| A member's loans | paged | 10/s |
| Overdue loans, oldest first | paged | 1/min |
| How many tools a member has out | one | 5/s |

Each row becomes an entry under `access:`. If a question cannot be answered by one keyed request,
the model needs another key, index, copy or counter, never a scan or a filter. dynago generates no
filters (except one dropping expired items), and a scan only where the schema declares one with a
reason, for work off the request path such as an export. A missing pattern shows up as a missing
method, not as a slow page in production.

## 2. Choose the primary key

The primary key answers the most important question, and decides what lives together.

- **Partition key**: everything that is read together shares one. `LIB#{libraryId}#TOOL#{toolId}`
  puts a tool, its loans and its pickup hold in one partition, so a tool's history is one Query.
- **Sort key**: orders items within the partition and tells item kinds apart. `LOAN#{loanId}` with
  time-ordered ids (ULIDs) lists a tool's loans by time; `HOLD` marks its one hold.

Keep in mind:

- A partition serves at most ~1,000 writes (WRU) and ~3,000 reads (RRU) per second. Put the tenant
  (here, the library) first so tenants never share a partition, and don't key busy things by a
  constant (`pk: "ALL"`).
- The sort key must start with literal text (`LOAN#`), so several kinds of item can share a
  partition and a query for one kind never returns another.
- Times in keys are rendered fixed-width in UTC, so they sort chronologically.

Then say how many items to expect, and how uneven they are:

```yaml
Library: { ..., volume: 2000 }
Tool:    { ..., volume: { typical: 60, max: 5000 } }    # per library
Loan:    { ..., volume: { typical: 20, max: 500 } }     # per tool: its whole history
```

dynago works out which items share a partition, how big each partition key value gets, whether it
grows forever, and how close its busiest value comes to a partition's throughput. `max` matters
most: trouble comes from the biggest tenant. See [Analysis](analysis.md).

## 3. Other ways in: GSI, copy, or neither

When a question needs a different partition or order ("a member's loans across tools"), add an
**index**. There are two strategies. Rather than choosing one, declare what each read needs,
`freshness: immediate` or `eventual`, and dynago chooses (and the model document shows what the
other would cost); `strategy:` overrides it:

| | `gsi` (default) | `copy` |
|---|---|---|
| Maintained by | DynamoDB, asynchronously | the generated code, in the write's transaction |
| Read consistency | eventual (usually well under a second behind) | strong: read-your-writes |
| Write cost | about +1 WRU per index entry written; no transaction | every write touching copied fields becomes a transaction (2× every item) |
| Half-finished writes | impossible: DynamoDB maintains it | impossible: same transaction |
| One source item can appear | once | once per copy index |
| Chosen when | every read through it is fine with `freshness: eventual` (or doesn't say) | a read through it declares `freshness: immediate`: the user must see their own change in the list |

Either way:

- **Project only what the list shows.** `project: [name, status]` keeps index entries small, so
  list pages are cheap. `project: all` stores and writes every item twice; the cost report says so.
- **Use sparse indexes.** An entity only has an index entry while all the string, enum and time
  fields of its index keys are set and `where` holds. `where: { status: active }` gives an index of
  active loans only, so returned loans drop out of the overdue list with no filtering.
- **Several entities can share a GSI** by using the same index name, when their sort keys are
  distinguishable by prefix. The index projects the union of their fields, but an item only carries
  the attributes it has, so entities don't pay for each other's. Sharing uses less of the table's
  budget of 100 projected attributes than separate GSIs would, since each attribute counts once.
  The risks are two entities meaning different things by one attribute name, and one index serving
  unrelated queries. Separate GSIs are usually clearer (up to 20 per table).

A query that only needs a different **order within the same partition** can often use the sort
key itself: pick the sort key for the most common order and an index for the rest.

### Ordering a list by another field

Add an index whose partition key picks the list and whose sort key is the order: one index per
order. To list a library's members newest first as well as alphabetically:

```yaml
indexes:
  ByName:
    pk: "LIB#{libraryId}#MEMBERS"
    sk: "{name|lower}#{memberId}"      # alphabetical, ignoring case
    project: [role, status]
  ByJoined:
    pk: "LIB#{libraryId}#MEMBERS"
    sk: "{joinedAt}#{memberId}"        # by join date
    project: [name, role, status]
access:
  Directory: { query: ByName }
  Newest: { query: ByJoined, order: desc, range: joinedAt }
```

- **End the sort key with the id**, so ties sort stably and entries are distinct.
- **Sort text with `{field|lower}`.** Keys compare bytes, so without it every capitalised name
  sorts before every lower-case one. `lower` also normalises Unicode (NFC), so an "é" typed two
  ways is one value. Order is still by bytes, not by a language's alphabet: "émile" sorts after
  "zoe". For locale-aware order, store a sort key your application computes (a collation key) and
  sort by that, or use a search engine.
- **A filter goes in the partition key.** The catalogue's `LIB#{libraryId}#CAT#{category}` lists one
  category, sorted by name.
- **Read-your-writes?** Declare `freshness: immediate` on the read: the index becomes a copy.
- **Small bounded lists** (a member's handful of active loans) can be read with the query you
  already have and sorted in Go.
- **An order by a value that lives elsewhere** ("most borrowed tools") isn't a field of the item,
  so no index can sort by it: write the value onto the item (a periodic job, for example), or use a
  search engine.

Adding an order to a table that already has data needs a new table generation: dynago stores
index keys as their own attributes, which older items don't have. The migration job copies every
item into a new table with the new index. See [Migrations](migrations.md).

### LSIs

A local secondary index (LSI) is another sort order on the same partition key, maintained by
DynamoDB in step with the item. dynago doesn't support them yet; they're next on the roadmap.

An LSI's one unique feature is a combination: strongly consistent reads, in another order, within
one partition, without a transaction on each write. Drop any one of those and something without an
LSI's costs covers it: a GSI (if a moment's lag is fine) or a copy index (if it isn't).
The price of a copy instead of an LSI is that affected writes become transactions. For a small item
that's about 4 WRU per write instead of 2, usually a few dollars a month.

Against that, LSIs have table-wide costs:

- **They can only be created with the table**, never added or removed. With dynago that's a new
  table generation and a generated migration (see [Migrations](migrations.md)): workable, but a
  full copy of the table each time.
- **Every partition key value is capped at 10 GB** (its items plus their LSI entries, an "item
  collection") as soon as the table has any LSI, including values whose items never use it. Writes
  that would exceed the cap fail with `ItemCollectionSizeLimitExceededException`. Without LSIs, a
  partition key value's items can grow without limit.
- **At most 5 per table, with projections fixed at creation.** You must choose between `ALL`, which
  stores each item twice, and a narrower projection, before you know what the index is for.

A common workaround is to create all five up front as generic spares, "in case we need them". That
has the worst of both: the 10 GB cap from day one on every partition key value, projections chosen blind,
and index names like `LSI3SK` that tell reviewers nothing. The spares also tend to get used up by
unrelated features.

Declare an LSI when a domain has a concrete, high-volume need for a consistent sort order within
a partition, and no partition key value's items will come near 10 GB. Otherwise a GSI or a copy
index is the lighter choice: neither caps partitions, and neither has to be decided when the table
is created (though adding or dropping one still takes a new table generation).

## 4. Uniqueness: claims

A GSI cannot enforce uniqueness, and a lookup through one is eventually consistent. For "no two
members with this email" and "route /l/{slug} to its library", declare `unique:`. dynago writes a
**claim** item keyed by the value, conditioned on it not existing, in the same transaction as the
entity. Changing the value moves the claim; deleting the entity releases it; `get: { unique }`
looks it up with strongly consistent reads.

- Scope uniqueness by including the scope in `fields`: `[libraryId, email]` is unique per library.
- A claim can encode a rule: the example's `Serial` claim stops the same physical tool being
  catalogued twice.
- Optional values (empty strings) make no claim, so any number of entities can leave them empty.
- A **set** of unique values (several emails per user, several labels per tool) is a `string_set`
  in `fields`: one claim per element, each pointing back to the entity, added and released as the
  set changes. The example's tools do this with their barcodes. Keep such sets small: every
  element is an item in the write's transaction.
- **Don't combine claims with `ttl`**: an expired item leaves its claim behind. For a token on an
  expiring item, use a GSI (its entry expires with the item). The example's `Hold` entity does
  exactly this for its pickup codes. It stores an HMAC of the code under a server-side secret, not
  the code: keys end up in backups, exports and logs, and a plain hash of a short code is reversed
  by hashing every possible code.

## 5. Counts and sums: counters

Counting items by querying them is O(items) and gets slower as data grows. Declare a **counter**:
an item updated with atomic `ADD`s in the same transaction as every write that changes it, so it
can never disagree with the items it counts. See [Counters](counters.md) for limits, lower bounds,
sharding and conditional counts.

## 6. Rules and changes across entities: requires

"A suspended member can't borrow" and "you can only borrow an available tool" are rules about
*another* item. Checking them with a read before the write leaves a race: the member could be
suspended in between. And "borrowing marks the tool on loan" is a change to another item: done as a
second write, a crash between the two leaves a loan for a tool that still shows as available.
Declare both instead:

```yaml
Borrow:
  create: true
  set: { status: active }
  requires:
    Member: { key: { libraryId: libraryId, memberId: memberId }, when: { status: active } }
    Tool:
      key: { libraryId: libraryId, toolId: toolId }
      when: { status: available }
      set: { status: onLoan }
```

Everything a `requires` declares happens in the write's own transaction: the member check, the
tool's change and the library's tool counts are committed together with the loan, or not at all.
A requirement can also:

- **check a counter**: `MemberLoans: { key: {...}, when: { active: 0 } }` stops a member with tools
  out from leaving;
- **accept an absent item**: `optional: true` with `when: { memberId: "{memberId}" }` means "if the
  tool has a hold, it must be the borrower's";
- **delete the item**: `consume: true` collects that hold as the tool is borrowed.

The change to another entity follows the same rules as an update of it: if its derived changes
are known from `when` and `set` (the tool's counts move from `available` to `onLoan`), it is written
without reading the tool. Otherwise the tool is read first.

**Keep expiring items self-contained.** The example's holds reserve a tool by existing, not by
changing the tool: borrowing requires "no one else's live hold", and an expired hold counts as
absent. Had placing a hold set the tool to `held`, a hold lapsing by TTL (which runs no code) would
leave the tool held forever.

## 7. Denormalise deliberately

Copying a field onto another item (a tool's name onto each loan, so "my loans" shows it without
reading the tool) is normal in DynamoDB. First decide what the copy means:

- **A snapshot**: the value when the item was written, deliberately kept. A loan records the name
  the tool had when it was borrowed; an order records the price it was placed at. Nothing needs
  updating. Declare `snapshot_of: Tool.name`, and the model document says so.
- **A copy that must stay equal** to its source. Within one entity, dynago does it: indexes and
  copies are recomputed on every write. **Across entities**, your code must: if a tool is renamed,
  its loans' `toolName` must be rewritten. Declare `copy_of: Tool.name`, and `dynago check` warns
  (`copy-drift`), saying how many items one change fans out to: a few can be updated in one
  transaction, thousands need a background job and can't change atomically.

Most copies turn out to be snapshots. Saying which removes the question for every reviewer.

## 8. Edits: update or patch

`update: [a, b]` writes the values given: a zero value clears the field, so a caller must send
every field of the write. `patch: [a, b]` takes optional values: fields left `nil` are unchanged.
Use `patch` for edit forms and APIs that send only what changed, and keep large fields out of
writes that don't need them.

## 9. One table or several?

dynago generates one store per table and supports several entities per table. Put entities in the
same table when they are read together (same partition key) or belong to the same service. Separate
tables are easier to reason about, to secure (IAM per table), to back up and to monitor. On
on-demand billing there is no capacity reason to merge unrelated domains into one table.

## 10. Check the design

Run `dynago check` and read the model document. For each read, it shows what serves it, its
freshness, how many items it returns and what it costs; for each write, what it checks, the items it
touches, whether it reads first, whether it is a transaction and what it costs. It lists the
guarantees the code enforces, each status field's lifecycle, every partition's size, growth and
busiest key, and findings: oversized items, transactions near their limits, hot partitions and
counters, keys with too few values, indexes that silently drop items or that nothing reads, and
TTL interactions. Accept a finding that's deliberate, with the reason (`accept:`). See
[Analysis](analysis.md) and [Reviewing designs and changes](review.md).
