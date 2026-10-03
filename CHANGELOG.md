# Changelog

All notable changes to dynago are recorded here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the project uses
[semantic versioning](https://semver.org). Before 1.0, a minor version may change the schema
format, the generated code and the runtime API; the changelog says how to move.

## [Unreleased]

### Added

- The generated Terraform has an output `<table>_table_arns`: every declared table's ARN (the current
  generation and the retained ones) under a name that doesn't change with the generation, so an IAM
  policy written against it follows the table through a migration.

### Changed

- Go names keep the capitals of an initialism in the plural: a field `labelIds` generates
  `LabelIDs`, where it generated `LabelIds`, and likewise `imageUrls` → `ImageURLs`. After
  regenerating, rename the uses of the affected names in your code; the compiler finds them.
  Stored attribute names don't change.

- A read of several entities that share a partition, in one Query:
  `Open: { query: partition, of: [Thread, Message, Attachment, Draft] }` returns a page of the
  partition with each kind in its own field (`ThreadOpen{Thread, Messages, Attachments, Drafts}`).
  The entities must have the same partition key template. `dynago.QuerySpec` gained `Types`.
- Reads of several items at once. `GetSeveral: { get: key, batch: 40 }` takes a slice of keys and
  returns the items that exist, in the order asked, with one BatchGetItem per 100 keys.
  `AllCounts: { counter: LabelCounts, all: true }` returns every item of a counter in one
  partition (each label's counts for a user) with one Query, a page at a time, each with the key
  its sort key holds. The runtime gained `dynago.GetBatch` and `dynago.SplitKey` for them.
- A requirement can do more to its item than assign: `add: { messageCount: 1 }` adds to an int
  field (atomically, so concurrent writes all count), and `patch: { hasAttachments:
  "{hasAttachments}" }` sets a field from the writing entity's only when that has a value,
  leaving it alone otherwise. Both maintain the item's counters, copies and index keys as `set`
  does. The runtime's `dynago.Set` gained `Add`.
- `matches` on an index with a `where`: the share of the entity's items that satisfy it. The
  analysis sizes the index's partitions, storage and traffic with it, and the writes that maintain
  it pay for the entry in that share of their calls.
- Conditions can exclude a value or list several: `where: { mailbox: { not: trash } }`,
  `when: { mailbox: { in: [inbox, archived] } }`. They work wherever a condition does: an index's
  and a counter value's `where`, a write's `when`, and a require's `when` (where `not` may also
  name a field of the writing entity). A `when` using one doesn't pin the state the write starts
  from, so a write that moves a counter or an index entry on that field reads the item first.
  The runtime's `dynago.Cond` gained `Not` and `In` for them.
- An `accept` on an entity also covers the rule's findings about what the entity declares: its
  fields, indexes, constraints, counters, reads and writes. An entity whose fields are all copies,
  for one reason, states the reason once, and the model document lists it once. An acceptance on
  the object itself takes precedence; one that matches nothing under the entity is still an error.
- When a shape changes under the version the lock recorded, and the lock holds only a first design
  (generation 1, every entity at version 1), the error also says what to do if no table exists
  yet: delete the lock file and generate again. The schema-changes guide and getting-started say
  when that is right.

### Fixed

- A sparse index (one with a `where`) was sized, and its writes priced, as if every item of the
  entity were in it, and reported as fact: it could be the model document's largest partition
  while holding a sliver of the items. Now a write's `when` and `set` decide what it does to the
  index where they can: a write that takes the item out of the index removes one entry (it was
  costed as a delete and a put), and one that can't match doesn't touch it. Where the write
  doesn't say, the index's `matches` share is used, and without one the numbers are marked as
  upper bounds: "at most" in the partition table, worded so in the `low-cardinality-key` finding,
  and kept out of the summary's largest and busiest partition. Estimated costs and transaction
  sizes of existing schemas can go down as a result.
- A `volume.by` naming an entity further up the parent chain (a draft's spread `by: User`, where
  drafts are counted per thread and threads per user) was ignored: the partitions keyed by that
  entity were sized from the chain, whose largest value multiplies the most skewed step by the
  typical others. The declared spread is now used. A `by` that names the volume's own `per` entity
  is an error, since the volume's `typical` and `max` already say it.
- A key longer than DynamoDB allows (2,048 bytes for a partition key, 1,024 for a sort key) is
  refused with `dynago.ErrInvalidKey` by every read and write, before any request is made. It used
  to come back as DynamoDB's `ValidationException`, which callers matching `ErrInvalidKey` for bad
  input didn't catch. Index key attributes are not checked yet.
- `EnsureTable` looks before it changes anything (`DescribeTable`, `DescribeTimeToLive`), so calling
  it at every start makes no failing request once the table exists. It used to send `CreateTable`
  and `UpdateTimeToLive` every time and ignore their errors, and against DynamoDB Local each of
  those errors made the AWS SDK log a warning.

## [0.1.0] - 2026-09-30

First release.

### Analysis and review

- `volume` on entities (a total, or `typical` and `max` per parent, with `by` for the spread over
  other entities) and `workload` (`peak`, `horizon`). Parents are inferred from partition key
  nesting; `ref` links a field to another entity's key, and `via` says which link a volume counts
  by when an entity holds another's key more than one way.
- Partition analysis: every partition family in the base table and each GSI, with items per key,
  size (typical and largest), growth, the busiest key's capacity at peak, and a risk level.
- Findings with rule ids, severities and subjects: DynamoDB's limits (item size, partitions,
  transactions, indexes), hot partitions and counters, contention, low-cardinality keys, sparse
  indexes, unenforced uniqueness, unused and unneeded indexes, copies that drift (with their
  fan-out), scans, and what a policy requires. A counter item's load is summed over every write
  that changes it, and reported on the counter; a partition's busiest member is weighed by reads
  as well as writes; a key that moves loads each of its two keys once. `accept: { rule: reason }` on the table, an entity,
  field, index, constraint, counter, read or write records one as deliberate; errors can't be
  accepted, and stale acceptances are errors.
- `dynago.policy.yaml`: `fail_on`, rule severities, limits (item and partition size, transaction
  items, GSIs, indexes per entity) and required declarations (volumes, rates, freshness).
- `freshness: immediate | eventual` on reads. An index without `strategy` becomes a copy if a read
  through it needs immediate freshness, a GSI otherwise; the model document shows what the other
  strategy would cost.
- `snapshot_of`: a copied value deliberately kept as it was when written.
- `scan: true` with a `reason`: a declared, paginated scan of the entity's items, for work off the
  request path.
- The model document leads with a summary, the domain (relationships, guarantees, lifecycles),
  catalogues of every read and write, the indexes and a partition map, then risks and costs, and
  ends with per-entity reference detail.
- `dynago check`: the analysis (costs, partitions, findings), without writing anything; `-json`
  prints the analysed design as JSON. Its output explains its figures, and says a cost is not
  estimated when nothing declares a volume or rate.
- `dynago diff`: the architectural changes since a git ref or another file, as Markdown, including
  whether existing items need a new table generation and what migrating them costs. Storage
  changes are judged against the base's lock file, as `dynago generate` will judge them, and
  changes to the workload are shown.
- `dynago vet`: DynamoDB calls outside generated code, with `//dynago:raw <reason>` for exceptions.
  Methods passed as values (`retry(c.PutItem)`) are reported too, and a mark may follow a
  multi-line call's closing `})` or precede further comment lines above the call.

### Schema and generator

- Schema files declaring entities, fields, key templates, indexes, uniqueness, counters, access
  patterns and writes; validation with actionable errors, and a JSON Schema for editors.
- Generated Go stores on guregu/dynamo v2: one method per declared read and write, typed keys, enum
  types, views for projected indexes and queries, entity-specific errors wrapping `dynago` sentinels.
- Indexes as GSIs (maintained by DynamoDB) or copies (written in the same transaction, and read
  consistently by default), sparse by empty key fields and `where`.
- Uniqueness claims, including composite and optional values, moved atomically on change, and
  one claim per element of a `string_set`. Claims point back to their owner.
- Counters maintained atomically from every write: counts and sums, conditional (`where`), upper
  bounds (`limit`, constant or caller-supplied), lower bounds (`min`), sharding. Counters can be read
  from any entity of the table.
- `requires`: rules and changes across items, all in the write's transaction. Check another entity
  (`when`, which may compare with the writer's own fields as `"{field}"`), accept its absence
  (`optional`), change it (`set`, maintaining its counters, claims, copies and index keys), delete
  it (`consume`), or check a counter's values (a missing value counts as 0). Changes to another
  entity are written without reading it when its derived changes are known, falling back to a read
  otherwise.
- A requirement whose key field is empty fails with `dynago.ErrFieldRequired` instead of being
  skipped; only `optional` requirements are skipped.
- `set` on creates: fields fixed to constants whatever the caller passes.
- `required: true` fields: writes refuse their zero value with `dynago.ErrFieldRequired`.
- `patch`: optional update fields; `nil` leaves a field unchanged. A call that leaves every field
  feeding a derived item `nil` runs as a single UpdateItem.
- `copy_of`: declares a field copied from another entity that must stay equal to it, shown in the
  model document and warned about by `dynago check`.
- Three update shapes chosen from the schema: a single conditional UpdateItem when nothing derived
  changes; a read-free transaction when the derived changes are known from the call; a read, diff
  and guarded transaction otherwise.
- `project` on queries of an entity's own partition.
- `{field|lower}` key template transform, for case-insensitive ordering and uniqueness.
- Document versions for optimistic concurrency: `Version()`, `dynago.From`, `dynago.IfVersion`,
  `dynago.ReturnVersion`, and `versioned: required` writes. Creates set the entity's version.
- Schema versions stored on every item, a lock file of storage shapes, and required version bumps.
- Table generations: a change existing items don't fit (a new, changed or dropped index, claim or
  counter; a new key; a field's type, a reused attribute, or a field made required) needs a new
  generation, a new table named `<name>-g<n>`, with older ones retained
  for rollback (`table.retain`, declared in the Terraform). The change that bumps the generation
  must retain the previous one, which the migration job copies from. An unversioned change that
  also needs a new generation says both at once. Within a generation, rewrites keep
  attributes the code doesn't know, so compatible versions can share a table during rolling
  deploys and rollbacks.
- A generated migration job (`RunMigration`, and a `main` package with `output.migrate_cmd`):
  `copy` makes a bulk pass and catch-up passes while the old generation serves, and `finish` a
  last pass with writes stopped. It is resumable, rate-limited and safe to run from several pods:
  a lease, renewed as it runs, fences every write (on a fence item per scan segment, so workers don't
  contend), and a job that lost it writes nothing more. Copies follow key changes made while the
  old generation serves; conflicts are retried after each pass's removals, and a unique value held
  by a stale copy is freed, so swapped or rotated values copy cleanly. A resumed pass keeps its scan
  segments whatever `-workers` is, and expiry of old items follows the old table's TTL attribute.
  It rebuilds derived items from the entities, and reports items it can't copy as conflicts. It
  converts entities field by field, or with a `Migrate<Entity>` function where fields don't carry
  over. Which items of a page are already copied, and which sources still exist, it checks with
  one BatchGetItem per page.
- Model document with Mermaid diagrams, Terraform JSON (with point-in-time recovery, deletion
  protection and TTL) and CreateTable JSON.
- Read-free writes only when the key of every counter they change is known; `versioned: required`
  enforced on them too.
- Refusals of schemas that would generate uncompilable Go or unusable writes: colliding Go names,
  reserved field names, `when` on key fields, requires touching an item twice, copy keys that
  transform the entity key, more than 100 projected attributes across GSIs, fields named like GSI
  key attributes.
- Lock file: refuses a missing history (`-new-history` to start one), records the TTL attribute and
  each generation's table, ignores declaration order, and upgrades older lock files in place.
- CLI flags may follow the schema files.
- Cost estimates: sizes, capacity per call (including condition checks), monthly cost from
  declared rates, and storage.

### Runtime

- Every row dynago writes carries `_created` and `_updated` (entity items, copies, claims,
  counters), set on every write path and kept by the migration job; entities expose them as
  `Timestamps()`.
- Caller-supplied limits are required: `dynago.Max(n)` or `dynago.Unlimited()`; a missing one is
  `dynago.ErrLimitRequired`.
- Retry-safe writes: idempotent transactions, and single-item creates that recognise their own item
  after an SDK retry.
- Random starting revisions; configurable, capped retries (`dynago.SetRetries`).
- TTL-aware reads and writes: expired items are absent before DynamoDB deletes them, and a create
  may replace one.
- Key values containing the separator that follows them in a key identifying an item are rejected;
  fields at the end of a key and GSI sort keys (names) are free to contain it. Updates check only
  the fields they change. Fields linked by a `requires` key share their checks, so a value is
  refused where it is first written.
- `{field|lower}` normalises Unicode to NFC before lower-casing.
- `DYNAGO_REQUIRE_DB` makes `dynagotest` fail instead of skip without DynamoDB Local; `make test`
  sets it.
- Cursors tied to query, partition, range bounds and the query's key shape (not the schema
  version, so they survive a rolling deploy of a compatible version), optionally signed
  (`dynago.SignCursors`, with previous keys accepted for rotation).
- Transactions refuse to touch an item twice (`dynago.ErrSameItemTwice`) before sending, and
  changes to one counter item from several entities in a write are merged.
- Single-item writes blocked by a transaction on the item are retried as contention.
- `dynagotest`: DynamoDB Local connection, a table per test, and counts of every request kind.
- dynago's own tests can also run against real tables in a test AWS account (`make test-aws`, for
  maintainers).

### Example

- `toollibrary`, a fictional neighbourhood tool library using most features: borrowing and
  returning change the tool atomically, and holds reserve a tool until they lapse. It is at table
  generation 3 (barcodes, then a list of every library, the service's tenants), with its migration
  command and the conversion the last move needs. End-to-end tests
  (`internal/e2e`) show a refused borrow leaves the table unchanged and ten racing borrowers get
  one tool.

[Unreleased]: https://github.com/nicklanng/dynago/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/nicklanng/dynago/releases/tag/v0.1.0
