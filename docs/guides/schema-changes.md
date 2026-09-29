# Schema changes, versions and the lock file

Items in DynamoDB don't change when the schema does. An item written last year has last year's
shape until something rewrites it. dynago makes that explicit, so changing a live schema is safe
and reviewable.

## Versions

Every entity has a `version` (default 1), and every item stores the version it was written at
(`_v`). An entity's **storage shape** is everything that decides what its items look like:

- fields, their attribute names and types;
- the key templates and TTL field;
- indexes (keys, projection, `where`, strategy);
- uniqueness claims;
- counters and their values.

Docs, sizes, examples, access patterns, writes, rates and limits are not part of the shape.
Changing them never needs a version bump.

**Changing the shape requires a higher version.** `dynago generate` refuses otherwise, and names the
change:

```
entity Loan: its storage shape changed but its version is still 1. Set `version: 2` so stored
items record which shape wrote them. Changes: counter MemberLoans changed (new values count only
items written from this version: needs a backfill before they are complete)
```

## The lock file

`<table>.dynago.lock` records every version of every entity's shape. Commit it, and review its diff
like a migration. `dynago generate -check` in CI fails if it (or any generated file) is out of date.

The generator uses the history to work out **which version introduced each counter value, claim and
copy**. Generated writes only count an item towards a counter value (or give it a claim or copy) if
the item was written at or after that version. This is what makes adding a counter to existing
data safe:

- an old item that never contributed is never subtracted when it is updated or deleted;
- when any write rewrites an old item, it moves to the current version and starts contributing,
  with limits applied.

**Don't lose the lock file.** Without it, every counter value, claim and copy would look new at
the current version, so deleting an older item would never release what it contributed. `dynago
generate` refuses an entity above version 1 with no history: restore the file from version control.
For a new table whose entities start above version 1, `-new-history` starts the history there.

The shape records what is stored, not how the schema is written: reordering fields, indexes or
counters is not a change. It also records the table's TTL attribute, since renaming it would leave
existing items' expiry in the old attribute (they would never expire). A lock file written by an
older dynago is upgraded in place when nothing has changed.

## What each kind of change means for existing data

| Change | Existing items | What to do |
|---|---|---|
| Add a field | Read back with the zero value until written | Nothing. |
| Remove a field | Keep the attribute until rewritten (unused) | Nothing, or a cleanup. |
| Change a field's attribute or type | Keep the old attribute or type | Needs a rewrite migration; add a new field instead where possible. |
| Change the primary key | Stay under the old key: the new code can't find them | Needs a key rewrite migration. Avoid. |
| Add a GSI index | No entry until rewritten (DynamoDB backfills the index, but old items lack the key attributes) | Backfill if queries must see old items. |
| Change a GSI index's keys or `where` | Old entries keep the old keys until rewritten | Backfill. |
| Change a GSI index's projection | DynamoDB can't change a projection: the index is deleted and re-created (Terraform replaces it), and queries through it fail until DynamoDB has rebuilt it | Plan for the gap; no data backfill. |
| Add a counter or counter value | Not counted until rewritten; the value undercounts | Backfill before relying on it. |
| Add a unique claim | Not claimed until rewritten; duplicates with old items possible | Backfill before relying on it. |
| Add a copy index | No copy until rewritten | Backfill. |
| Remove a claim | Old claims stay, keeping their values taken | Cleanup migration. |
| Remove a copy index | Old copies stay (unused) | Cleanup migration. |

`dynago generate` prints each change with this guidance, marked as a warning where data needs work.

## Changes that are refused

Some changes would strand existing items, because the code only knows the current definition and
can't release what an item contributed under an old one. They are refused even with a version bump:

- changing an existing **copy index** (its keys, projection or `where`);
- changing an existing **unique claim** (its fields or keys);
- changing a **counter's** keys or shards;
- changing what an existing **counter value** counts (`count`/`sum`, `where`);
- adding a `limit: arg` counter value to an entity that another entity's writes change through
  `requires`: old items start counting when that write changes them, but only the entity's own
  writes can be given the limit. Use a constant limit.

Instead, add a new one under a new name, and remove the old one:

```yaml
values:
  active: { count: true, where: { status: active } }                    # old: keep until you remove it
  activeOverdue: { count: true, where: { status: active, overdue: true } } # new name, new definition
```

## Deploying a version bump

1. Generate and review: the model document diff shows what changed, the lock diff the new shape.
2. Deploy. During the rollout, old instances refuse to rewrite items that new instances have
   written (`dynago.ErrNewerSchema`), so they can't undo the new shape. Callers see that error only
   during the rollout.
3. If the change needs a backfill (new counters, claims, copies, index keys), run it before relying
   on the new structure. **dynago does not ship a backfill runner yet**; it is the next item on the
   roadmap. It will rewrite every item below the current version through the generated write path,
   which is exactly what a normal write does.
