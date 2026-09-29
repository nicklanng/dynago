# Changelog

All notable changes to dynago are recorded here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the project will use
[semantic versioning](https://semver.org) once released.

## [Unreleased]

First version.

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
- `copy_of`: declares a field copied from another entity, shown in the model document and warned
  about by `dynago check`.
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
  for rollback (`table.retain`, declared in the Terraform). Within a generation, rewrites keep
  attributes the code doesn't know, so compatible versions can share a table during rolling
  deploys and rollbacks.
- A generated migration job (`RunMigration`, and a `main` package with `output.migrate_cmd`):
  `copy` makes a bulk pass and catch-up passes while the old generation serves, and `finish` a
  last pass with writes stopped. It is resumable, rate-limited and safe to run from several pods:
  a lease, renewed as it runs, fences every write (on a fence item per worker, so workers don't
  contend), and a job that lost it writes nothing more. Copies follow key changes made while the
  old generation serves; conflicts are retried after each pass's removals, and a unique value held
  by a stale copy is freed, so swapped or rotated values copy cleanly. A resumed pass keeps its scan
  segments whatever `-workers` is, and expiry of old items follows the old table's TTL attribute.
  It rebuilds derived items from the entities, and reports items it can't copy as conflicts. It
  converts entities field by field, or with a `Migrate<Entity>` function where fields don't carry
  over.
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
- Cost and risk report: sizes, capacity per call (including condition checks), monthly cost from
  declared rates, storage, and findings (item size, large fields, transaction size, hot and
  conflict-prone counters, cross-entity copies, projections, TTL interactions).

### Runtime

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
- Cursors tied to query, partition, range bounds and schema version, optionally signed
  (`dynago.SignCursors`, with previous keys accepted for rotation).
- Transactions refuse to touch an item twice (`dynago.ErrSameItemTwice`) before sending, and
  changes to one counter item from several entities in a write are merged.
- Single-item writes blocked by a transaction on the item are retried as contention.
- `dynagotest`: DynamoDB Local connection, a table per test, and counts of every request kind.

### Example

- `toollibrary`, a fictional neighbourhood tool library using every feature: borrowing and
  returning change the tool atomically, and holds reserve a tool until they lapse. It is at table
  generation 2 (barcodes), with its migration command. End-to-end tests
  (`internal/e2e`) show a refused borrow leaves the table unchanged and ten racing borrowers get
  one tool.
