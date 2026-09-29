# Schema changes: versions, generations and the lock file

Items in DynamoDB don't change when the schema does. An item written last year has last year's
shape until something rewrites it. dynago makes that explicit, so changing a live schema is safe
and reviewable. There are two kinds of change:

- **Compatible changes** leave existing items valid. They happen in place, in the same table.
- **Changes existing items don't fit** start a new **table generation**: a new table, filled from
  the old one by a generated migration job, with the old table kept for rollback. See
  [Migrations](migrations.md) for running the job.

## Versions

Every entity has a `version` (default 1), and every item stores the version it was written at
(`_v`). An entity's **storage shape** is everything that decides what its items look like:

- fields: their attribute names, types, enum values, and whether they're required;
- the key templates and TTL field;
- indexes (keys, projection, `where`, strategy);
- uniqueness claims;
- counters, their values and shard counts.

Docs, sizes, examples, access patterns, writes, rates and limits are not part of the shape.
Changing them never needs a version bump.

**Changing the shape requires a higher version.** `dynago generate` refuses otherwise, and names the
change:

```
entity Member: its storage shape changed but its version is still 1. Set `version: 2` so stored
items record which shape wrote them. Changes: field pronouns added
```

## Compatible changes and generations

| Change | Existing items | Where it happens |
|---|---|---|
| Add an optional field | Read back with the zero value until written | Same table |
| Add enum values | Hold the old values, which are still valid | Same table |
| Remove a field | Keep the attribute (unused) | Same table |
| Re-add a removed field with its old type | Hold values of that type, which are still valid | Same table |
| Remove an index, claim, counter or counter value | Keep them, and an older version still running (a rolling deploy, a rollback) keeps maintaining them while the new one doesn't | New generation |
| Add or change an index (GSI or copy) | Have no entry, or the old one | New generation |
| Add or change a unique claim | Aren't claimed; duplicates possible | New generation |
| Add or change a counter or counter value | Aren't counted | New generation |
| Add a required field, or make a field required | Don't have it | New generation |
| Store another type in an attribute used earlier in the generation (a removed field re-added with another type, or its attribute reused) | Hold the old type | New generation |
| Change a field's type or attribute | Hold the old one | New generation |
| Remove enum values | May hold them | New generation |
| Change the primary key or TTL field | Are under the old key, or expire by the old field | New generation |

A change that needs a new generation, made without one, is refused, and the error says what to do:

```
entity Tool: existing items don't fit version 2: unique claim Barcode added (existing items don't
have it). Start a new table generation (`table.generation: 2`): the generated migration job copies
every item into the new table, and the old one stays for rollback
```

### Within a generation

Code at two compatible versions can share a table, which happens during a rolling deploy, or after
a rollback. When a write rewrites an item, it **keeps attributes it doesn't know**, so an older
version never drops a field a newer one stored. A field removed from the schema stays on existing
items until the next generation.

### A new generation

```yaml
table:
  name: toollibrary
  generation: 2      # was 1
  retain: [1]        # keep toollibrary-g1 for rollback
```

Each generation's table is named `<name>-g<generation>`: generated code has `Generation` and
`TableName(base)`, and the Terraform declares the current table and every retained one. A
generation may change anything. The migration job copies each entity from the previous generation,
converting it, and rebuilds every index entry, claim, copy and count in the new table from the
entities, so they are exact whatever the old table held. Generations go up one at a time.

## The lock file

`<table>.dynago.lock` records every version of every entity's shape, which generation each
belongs to, and each generation's physical table. Commit it, and review its diff like a migration.
`dynago generate -check` in CI fails if it (or any generated file) is out of date.

The generator uses it to decide whether a change is compatible, to describe the previous
generation's items for the migration job, and to keep declaring retained tables.

**Don't lose it.** Without it, dynago can't tell whether a change is compatible, so a new
generation needed for existing items could be missed. `dynago generate` refuses a schema above
version or generation 1 with no history. Restore the file from version control. For a new table
whose entities start above version 1, `-new-history` starts the history there.

The shape records what is stored, not how the schema is written: reordering fields, indexes or
counters is not a change. A lock file written by an older dynago is upgraded in place.

## Deploying

- **A compatible change:** generate, review the model document and lock diffs, and deploy as
  usual, with a rolling update.
- **A new generation:** generate, review, then run the migration job before the new version serves.
  See [Migrations](migrations.md).
