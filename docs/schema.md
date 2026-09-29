# Schema reference

A dynago schema is one YAML file per DynamoDB table. It declares the entities stored in the table,
how each is keyed, the items derived from it (index entries, copies, uniqueness claims, counters),
and **every** way it is read and written. `dynago generate` turns it into Go, documentation,
Terraform and a lock file; `dynago check` validates it and prints a cost and risk report.

Name the file `<something>.dynago.yaml`. For editor autocomplete and inline errors, put this on its
first line (VS Code with the YAML extension, JetBrains IDEs and others understand it):

```yaml
# yaml-language-server: $schema=https://raw.githubusercontent.com/nicklanng/dynago/main/schema/dynago.schema.json
```

The JSON Schema catches typos and wrong types as you type. `dynago` itself applies further rules
that JSON Schema cannot express (key templates referring to real fields, sort-key collisions,
version bumps); they are listed with each section below.

Contents: [File](#file) · [Table](#table) · [Output](#output) · [Entities](#entities) ·
[Fields](#fields) · [Keys](#keys) · [Indexes](#indexes) · [Uniqueness](#unique) ·
[Counters](#counters) · [Access patterns](#access) · [Writes](#writes) · [Requires](#requires) · [Predicates](#predicates) ·
[Estimates](#estimate) · [Names](#names) · [Rules across the file](#rules-across-the-file)

## File

```yaml
dynago: 1
package: toollibrary
table: { name: toollibrary }
entities:
  Tool: { ... }
```

| Key | Required | Meaning |
|---|---|---|
| `dynago` | yes | Schema format version. Always `1`. |
| `package` | yes | Go package name for the generated code: lower-case letters and digits, starting with a letter. |
| `table` | yes | The physical table. See [Table](#table). |
| `output` | no | Where generated files go. See [Output](#output). |
| `entities` | yes | At least one entity, keyed by PascalCase name. See [Entities](#entities). |

Every table has the same base key: `PK` (partition key) and `SK` (sort key), both strings. Billing
is on-demand. Several entities can share a table; dynago checks they can never be confused.

## Table

| Key | Required | Default | Meaning |
|---|---|---|---|
| `name` | yes | | Base table name. Each generation's table is `<name>-g<generation>`. Also used for output file names. |
| `doc` | no | | Shown at the top of the model document and on the generated `Store`. |
| `ttl_attribute` | no | `ttl` | Attribute DynamoDB's TTL reads (epoch seconds). Only enabled if an entity declares `ttl`. |
| `generation` | no | `1` | The table generation. Changes existing items don't fit need a new generation: a new table, filled by the generated migration job. See [Migrations](guides/migrations.md). |
| `retain` | no | | Older generations whose tables stay in the Terraform, for rollback: `[2]`. Remove a generation to delete its table. |

## Output

Paths of generated files, relative to the schema file. `<table>` is the table name, lower-cased,
with characters other than letters and digits replaced by `_`.

| Key | Default | File |
|---|---|---|
| `go` | `<table>_dynago.go` | The generated Go store. |
| `docs` | `<table>.model.md` | The model document. |
| `terraform` | `<table>.tf.json` | An `aws_dynamodb_table` resource in Terraform JSON syntax. |
| `table_json` | `<table>.table.json` | Input for `aws dynamodb create-table --cli-input-json`. |
| `lock` | `<table>.dynago.lock` | Version history of each entity's storage shape, and of the table generations. Commit it. |
| `migrate_cmd` | none | A directory for the migration job's `main` package, e.g. `cmd/migrate-orders`: build it into your image and run it as a job. Without it, call the generated `RunMigration` from a command of your own. See [Migrations](guides/migrations.md). |

## Entities

```yaml
entities:
  Loan:
    doc: A member borrowing a tool.
    version: 1
    fields: { ... }
    key: { pk: "LIB#{libraryId}#TOOL#{toolId}", sk: "LOAN#{loanId}" }
    ttl: expiresAt
    indexes: { ... }
    unique: { ... }
    counters: { ... }
    access: { ... }
    writes: { ... }
    estimate: { items: 2000000 }
```

| Key | Required | Default | Meaning |
|---|---|---|---|
| `doc` | no | | Documentation for the Go type and the model document. A noun phrase with an article ("A member borrowing a tool.") reads as "Loan is a member borrowing a tool."; anything else becomes its own paragraph after a generated first sentence. |
| `version` | no | `1` | Storage shape version. **Bump it whenever fields, keys, indexes, claims or counters change**; `dynago generate` refuses otherwise. A change existing items don't fit also needs a new table generation. See [Schema changes](guides/schema-changes.md). |
| `fields` | yes | | The entity's attributes. See [Fields](#fields). |
| `key` | yes | | The primary key templates: `pk` and `sk`. See [Keys](#keys). |
| `ttl` | no | | A `time` field. DynamoDB deletes the item after that time. Expired items do **not** update counters or release claims and copies (`dynago check` warns). |
| `indexes` | no | | Other ways to reach the entity. See [Indexes](#indexes). |
| `unique` | no | | Uniqueness constraints. See [Uniqueness](#unique). |
| `counters` | no | | Counters maintained from the entity's writes. See [Counters](#counters). |
| `access` | no | | The reads the store offers. See [Access patterns](#access). |
| `writes` | no | | The writes the store offers. See [Writes](#writes). |
| `estimate` | no | | Volume assumptions for the cost report. See [Estimates](#estimate). |

Every item also stores three bookkeeping attributes: `_t` (entity name), `_v` (schema version it
was written at) and `_rev` (revision, used for optimistic concurrency). Field attributes cannot use
these names, `PK`, `SK`, or the table's TTL attribute.

## Fields

```yaml
fields:
  libraryId: string                                  # short form: just the type
  status: { type: enum, values: [active, returned] }
  manual: { type: string, size: 2000/20000, doc: Care and safety notes. }
  dueAt: { type: time, required: true, example: "2026-10-12T17:00:00Z" }
  toolName: { type: string, copy_of: Tool.name }
  shortName: { type: string, attr: sn }              # stored as "sn"
```

Field names are camelCase. Each becomes an exported Go field (`libraryId` → `LibraryID`, see
[Names](#names)) and, by default, an attribute of the same name.

| Key | Required | Meaning |
|---|---|---|
| `type` | yes | One of the types below. |
| `attr` | no | Stored attribute name, if it should differ from the field name (e.g. shorter). Letters, digits, `_`, `.`, `-`. |
| `doc` | no | Copied onto the Go field and into the model document. |
| `size` | no | Expected size in bytes, `n` or `p50/p99`, for the cost report. |
| `example` | no | Example value, used to render example keys in the model document. Times accept RFC 3339 or `YYYY-MM-DD`. |
| `values` | enum only | The allowed values of an `enum`. Required for enums, not allowed otherwise. |
| `required` | no | `true` makes writes refuse the field's zero value with `dynago.ErrFieldRequired`: creates must set it, updates (`update`, `patch`) may not clear it, and `set` may not set it to zero. Key fields are always required. Use it for fields the model depends on, such as a hold's expiry. |
| `copy_of` | no | `Entity.field` this field copies from another entity (e.g. a tool's name on each loan, so "my loans" can show it without reading the tool). dynago keeps copies **within** an entity in sync but not this one; declaring it documents that your code must rewrite the copies, shows it in the model document, and makes `dynago check` warn. Types must match. |

| Type | Go type | Stored as | In keys | Empty means absent | Default size p50/p99 |
|---|---|---|---|---|---|
| `string` | `string` | S | as is | yes | 20 / 64 B |
| `enum` | named string type with constants | S | as is | yes | longest value |
| `int` | `int64` | N | zero-padded, sorts numerically | no | 8 / 11 B |
| `float` | `float64` | N | not allowed | no | 8 / 11 B |
| `bool` | `bool` | BOOL | `true` / `false` | no | 1 B |
| `time` | `time.Time` | S (RFC 3339) | fixed-width UTC, sorts chronologically | yes | 30 / 35 B |
| `bytes` | `[]byte` | B | not allowed | no | 200 / 2000 B |
| `string_list` | `[]string` | L | not allowed | no | 60 / 400 B |
| `string_set` | `[]string` | SS | not allowed | no | 60 / 400 B |
| `string_map` | `map[string]string` | M | not allowed | no | 120 / 800 B |

Empty (zero) values of non-key fields are not stored: an item with no `returnedAt` has no
`returnedAt` attribute, and reads back as the zero value. Updates that set a field to its zero
value remove the attribute.

An enum `status` on entity `Loan` generates `type LoanStatus string` and constants such as
`LoanStatusActive`.

A field may not be called `key` (it would clash with the generated `Key()` method).

## Keys

```yaml
key:
  pk: "LIB#{libraryId}#TOOL#{toolId}"
  sk: "LOAN#{loanId}"
```

| Key | Required | Meaning |
|---|---|---|
| `pk` | yes | Partition key template. |
| `sk` | yes | Sort key template. **Must start with literal text** (e.g. `LOAN#`), so the entity's items can be told apart from other items in the same partition. |

**Key templates** are literal text with `{field}` placeholders. Rules:

- Every placeholder names a field of the entity, of a type allowed in keys (`string`, `enum`,
  `int`, `time`, `bool`).
- Two placeholders must be separated by literal text (`{a}#{b}`, not `{a}{b}`).
- A `string` or `enum` placeholder can apply a transform: `{name|lower}` puts the lower-cased value
  in the key while the attribute keeps its case. Use it to sort case-insensitively (`"adam"` before
  `"Zoe"`; plain byte order puts every capital first) and for case-insensitive uniqueness
  (`pk: "UNIQUE#Member.Email#{libraryId}#{email|lower}"`). `lower` is the only transform; accented
  letters still sort after `z`.
- Values are rendered as described in [Fields](#fields); times always in UTC with nanoseconds,
  so a sort key such as `{dueAt}#{loanId}` sorts chronologically.
- Values are not escaped. Instead, a `string` or `enum` value may not contain the literal
  characters around it in any template it appears in: `"a#b" + "c"` and `"a" + "b#c"` would render
  the same key. Such writes and lookups fail with `dynago.ErrInvalidKey`.

The fields used by `pk` and `sk` are the entity's **key fields**. They form the generated
`<Entity>Key` struct, cannot be changed by an update, and must be non-empty (`string`, `enum` and
`time` key fields); an empty one fails with `dynago.ErrInvalidKey`.

## Indexes

```yaml
indexes:
  ByCategory:                                   # the catalogue (on Tool)
    pk: "LIB#{libraryId}#CAT#{category}"
    sk: "{name}#{toolId}"
    project: [status, tags]
  Overdue:                                      # active loans by due date (on Loan)
    pk: "LIB#{libraryId}#DUE"
    sk: "{dueAt}#{loanId}"
    where: { status: active }
    project: [memberId]
  ByMember:                                     # a member's current loans, soonest due first,
    strategy: copy                              # read-your-writes (on Loan)
    pk: "LIB#{libraryId}#MEMBER#{memberId}"
    sk: "MYLOAN#{dueAt}#{toolId}#{loanId}"
    where: { status: active }
    project: [toolName, dueAt]
```

An index is another key for the same entity. Index names are PascalCase.

| Key | Required | Default | Meaning |
|---|---|---|---|
| `strategy` | no | `gsi` | `gsi`: a global secondary index, maintained by DynamoDB, read eventually consistently. `copy`: copy items written by the generated code in the same transaction as the entity, readable strongly consistently. See [Choosing a structure](guides/modelling.md). |
| `pk` | yes | | Partition key template. |
| `sk` | gsi: no; copy: yes | | Sort key template. A copy's `sk` must start with literal text. |
| `project` | yes | | What the index holds: `all` (every field), `keys` (key fields only), or a list of fields. Key fields are always included. |
| `where` | no | | Only index the entity while these [predicates](#predicates) hold (a sparse index). |
| `doc` | no | | Shown in the model document and on the generated method. |

Behaviour:

- **Sparse.** An entity has no index entry while any `string`, `enum` or `time` field in the
  index's templates is empty, or while `where` does not hold.
- **GSI attributes.** A `gsi` index named `ByCategory` stores its keys on the entity item as
  `ByCategoryPK` and `ByCategorySK`, and the physical GSI is called `ByCategory`. Its projection is
  `INCLUDE` of the projected fields plus key fields, `_t` and `_v` (or `ALL` if `project: all`).
- **Shared GSIs.** Entities that declare an index with the same name share one physical GSI, with
  the union of their projections. They must all declare an `sk` (or none), and their sort keys
  must be distinguishable by prefix.
- **Copies** are stored in the base table under the index's keys. Their keys must include every
  key field of the entity, without a transform (so each entity has its own copy: `{id|lower}`
  would give "Bob" and "bob" one copy). Entities read through a copy have no version.
- **Limits.** DynamoDB allows 100 projected attributes per table, summed across all GSIs (dynago's
  `_t` and `_v`, and the TTL attribute, count in each), and a field's `attr` can't be a GSI's key
  attribute (`ByCategoryPK`).
- **Results.** A query through an index with `project: all` returns entities; otherwise it returns
  a generated `<Entity><Index>` struct holding the projected fields.

Limits: 20 GSIs per table and 100 projected attributes across a table's GSIs (DynamoDB's defaults).

## Unique

```yaml
unique:
  Slug: { fields: [slug] }                         # global
  Email: { fields: [libraryId, email] }            # unique per library
  Barcode: { fields: [libraryId, barcodes] }       # barcodes is a string_set: each label is unique
```

A uniqueness constraint is enforced by a **claim** item keyed by the value, created in the same
transaction as the entity with a condition that it does not exist (or is already the entity's
own). The claim stores the owner's key (`ownerPK`, `ownerSK`), which lookups follow back to the
entity. Constraints are scoped to their entity: another entity's claims never collide with them.
Names are PascalCase.

| Key | Required | Default | Meaning |
|---|---|---|---|
| `fields` | yes | | The fields whose combination must be unique. Types allowed in keys, plus at most one `string_set`, whose elements are each unique (one claim per element). A `string_list` can hold duplicates, so it can't be unique. |
| `pk` | no | `UNIQUE#<Entity>.<Name>#{field1}#{field2}…` | Claim partition key. Must use exactly the unique fields (with `sk`). |
| `sk` | no | `UNIQUE` | Claim sort key. |
| `doc` | no | | Shown in the model document. |

Behaviour:

- Creating an entity whose value is taken fails with `Err<Entity><Name>Taken` and writes nothing.
- **Sets**: with a `string_set` among the fields, each element is claimed. A write claims the
  elements it adds and releases those it drops, all in the entity's transaction; one taken element
  refuses the whole write. `{field|lower}` and separator checks apply per element. Every claim is
  an item in the transaction, so the set's size counts toward DynamoDB's 100-item limit:
  `dynago check` estimates it from the field's `size` and reports writes that could exceed it.
- An update that changes the value moves the claim (claims the new value, releases the old) in one
  transaction; a delete releases it.
- **Optional values**: no claim is made while any `string`, `enum` or `time` field of the
  constraint is empty, so any number of entities may leave it empty.
- Declaring an access pattern `{ get: { unique: Name } }` generates a lookup by value.

## Counters

```yaml
counters:
  MemberLoans:
    pk: "LIB#{libraryId}#MEMBER#{memberId}"
    sk: "LOANS"
    values:
      active: { count: true, where: { status: active }, limit: arg }
  MemberCounts:
    pk: "LIB#{libraryId}"
    sk: "COUNTS#MEMBERS"
    values:
      total: count
      stewards: { count: true, where: { role: steward, status: active }, min: 1 }
      loanDays: { sum: loanDays }
```

A counter is an item whose attributes are updated with atomic `ADD`s in the same transaction as
every entity write that changes them. Names are PascalCase; value names are camelCase and are also
the stored attribute names.

| Key | Required | Default | Meaning |
|---|---|---|---|
| `pk` | yes | | Counter partition key template, over the entity's fields. |
| `sk` | yes | | Counter sort key template. Must start with literal text. |
| `shards` | no | `1` | Spread the counter over this many items (1–100) to raise its write throughput; a read sums them with one BatchGetItem. Bounded values cannot be sharded. |
| `values` | yes | | At least one value. |
| `doc` | no | | Shown in the model document and on the Go type. |

Each value is `count` (short form) or a mapping:

| Key | Meaning |
|---|---|
| `count` | `true`: each entity contributes 1. |
| `sum` | An `int` field: each entity contributes its value. |
| `where` | Only entities matching these [predicates](#predicates) contribute. |
| `limit` | Upper bound. A number, or `arg`: writes that can grow the value take a caller-supplied `dynago.Limit` (e.g. a member's `maxLoans`). Writes that can grow it must be given one: `dynago.Max(n)`, or `dynago.Unlimited()` deliberately; otherwise they fail with `dynago.ErrLimitRequired`. Exceeding it fails with `Err<Counter><Value>Limit`. |
| `min` | Lower bound (≥ 0). Writes that would take the value below it fail with `Err<Counter><Value>Min` (e.g. "at least one steward"). |
| `doc` | Shown in the model document and on the Go field. |

Use exactly one of `count` and `sum`. The counter item is only touched while every `string`, `enum`
and `time` field in its templates is non-empty (a sparse counter). See [Counters](guides/counters.md).

## Access

```yaml
access:
  Get: get
  GetConsistent: { get: key, consistent: true }
  GetByEmail: { get: { unique: Email } }
  History: { query: key, order: desc, page: 20, project: [memberId, status, dueAt] }
  Overdue: { query: Overdue, range: dueAt, rate: 1 }
  ActiveLoans: { counter: MemberLoans }
```

Access patterns are the **only** reads the generated store offers: one method each. Names are
PascalCase and must be unique across `access` and `writes`. Declare exactly one of `get`, `query`
and `counter`.

| Key | Applies to | Default | Meaning |
|---|---|---|---|
| `get` | | | `key`: one GetItem by primary key (short form: `Name: get`). `{ unique: Name }`: find the entity holding a unique value — a consistent read of the claim, then of the item. |
| `query` | | | `key`: query the entity's own partition (items matched by its sort key prefix). An index name: query that index. Exactly one Query request per page. |
| `counter` | | | Read a counter: one GetItem, or one BatchGetItem over its shards. The counter may belong to any entity of the table, so a library can offer its member counts. |
| `order` | query | `asc` | `asc` or `desc`, by sort key. |
| `page` | query | `50` | Default page size (items evaluated per request). |
| `max_page` | query | `max(100, page)` | Largest page a caller may ask for (≤ 1000). |
| `range` | query | | A field that directly follows the sort key's literal prefix. Adds optional inclusive `From` / `To` bounds to the query. |
| `consistent` | get, query, counter | `false` | Strongly consistent read (twice the cost). Not allowed on a `gsi` index. |
| `project` | `query: key` | all fields | Read only these fields (plus key fields): `[name, status]`, or `keys`. Returns a generated `<Entity><Access>Item` type. Saves bandwidth and keeps other fields (secrets, large text) from callers; **DynamoDB still bills the whole item**, so for cheaper lists use an index with a narrow projection. |
| `doc` | all | | Shown in the model document and on the method. |
| `rate` | all | | Average calls per second, for the monthly cost estimate. |

## Writes

```yaml
writes:
  Borrow: { create: true, set: { status: active } }
  Return: { update: [returnedAt], set: { status: returned }, when: { status: active } }
  AddNote: { patch: [notes] }
  Extend: { update: [dueAt], versioned: required }
  Remove: delete
```

Writes are the **only** writes the generated store offers: one method each. Names are PascalCase.
Declare exactly one kind:

| Kind | Form | Generated behaviour |
|---|---|---|
| create | `Name: create` or `{ create: true }` | Put the entity, failing with `Err<Entity>Exists` if it exists; create its claims, copies and counter contributions in the same transaction. `set` fixes fields to constants, whatever the caller passed. |
| update | `{ update: [...], patch: [...], set: {...} }` | Change the listed fields. See below. |
| delete | `Name: delete` or `{ delete: true }` | Delete the entity, failing with `Err<Entity>NotFound`; release its claims, copies and counter contributions in the same transaction. |

| Key | Applies to | Meaning |
|---|---|---|
| `create` | create | `true`. |
| `delete` | delete | `true`. |
| `update` | update | Fields the caller supplies (a generated `<Entity><Write>` struct). Not key fields. The value given is written: a zero value clears the field. |
| `patch` | update | Optional fields the caller may supply, generated as pointers: `nil` leaves the field unchanged, a pointer to a zero value clears it (`dynago.Ptr(v)` makes pointers). Use for edit forms that send only what changed. A patch with nothing given writes nothing. |
| `set` | create, update | Fields set to constants, as [predicates](#predicates) (`{ status: former }`). On a create, they override what the caller passed, so a caller can't create a loan that is already returned. A field can appear in only one of `update`, `patch` and `set`. Setting `""` or `false` clears the attribute. |
| `when` | update | Preconditions ([predicates](#predicates)). If they do not hold, the write fails with `Err<Entity><Write>Precondition`. |
| `requires` | all | Conditions on other items, checked **in the same transaction**, and changes to them: see [Requires](#requires). |
| `versioned` | update, delete | `optional` (default) or `required`. Required writes refuse to run without a version from `dynago.From` or `dynago.IfVersion`. See [Concurrency](guides/concurrency.md). |
| `doc` | all | Shown in the model document and on the method. |
| `rate` | all | Average calls per second, for the monthly cost estimate. |
| `hot_key_rate` | all | Peak calls per second against one partition key, for hot-partition warnings. |

How an update runs is decided from the schema:

- If it changes nothing that feeds an index key, a `where`, a copy, a claim or a counter, it is
  **one conditional UpdateItem** with no read.
- If the counters and claims it changes depend only on key fields and fields its `when` pins (for
  example `Retire: { set: { status: retired }, when: { status: available } }` moves the tool
  from the `available` count to the `retired` count), it is **read-free**: conditional writes in one transaction,
  with no read. If the item turns out not to be in the assumed state, it falls back to the next
  shape, which reports exactly why.
- Otherwise it **reads the item** (consistently), computes the derived items before and after, and
  writes the item plus every derived change in one transaction guarded by the item's revision,
  retrying if the item changed underneath it. Passing `dynago.From(entity)` skips the read.

A `patch` whose fields only some feed derived items reads only when the caller gives one of those:
a profile edit that changes the phone number but not the name is a single UpdateItem.

Writes that take a `limit: arg` value require the caller to pass it (`dynago.Max(n)`, or
`dynago.Unlimited()` on purpose): an unset limit fails with `dynago.ErrLimitRequired`.

### Requires

```yaml
Borrow:
  create: true
  set: { status: active }
  requires:
    Member: { key: { libraryId: libraryId, memberId: memberId }, when: { status: active } }
    Tool:                                 # checked, and changed
      key: { libraryId: libraryId, toolId: toolId }
      when: { status: available }
      set: { status: onLoan }
    Hold:                                 # if there is one, it must be the borrower's; collected
      key: { libraryId: libraryId, toolId: toolId }
      optional: true
      when: { memberId: "{memberId}" }
      consume: true
Leave:
  delete: true
  requires:
    MemberLoans: { key: { libraryId: libraryId, memberId: memberId }, when: { active: 0 } }
```

Each entry names another item of the table, an entity or a counter, and everything it declares
happens **in the write's transaction**: nothing can change the item between the check and the
write, and no crash can leave the write half done.

| Key | Meaning |
|---|---|
| `key` | Maps every key field of the target (for a counter, every field of its key templates) to the field of this entity holding its value. Types must match. |
| `when` | Values the target must have ([predicates](#predicates)). For an entity, a value `"{field}"` means this entity's field: `memberId: "{memberId}"` requires the hold to be the borrower's. For a counter, integers; a missing value counts as 0, so `active: 0` also passes before anything was counted. |
| `set` | Entities only. Changes the target in the same transaction: its revision, counters, claims, copies and index keys are maintained as by an update of it declared with this `set` and `when`. Values are constants or `"{field}"`. Not key fields; not a counter value whose limit the target's callers supply. |
| `optional` | Entities only. The write goes ahead if the target is absent or has expired; `when` applies only to one that is there. |
| `consume` | Entities only. Deletes the target in the same transaction, releasing what it contributed. Exclusive with `set`. |

The write fails with `Err<Entity><Write>Requires<Target>`, and writes nothing, unless the target
meets the requirement: it exists (unless `optional`), has not expired, and meets `when`.

How the target is changed is decided like an update's shape. If its derived changes follow from
its key and the state `when` pins (the tool's `available`/`onLoan` counts in `ToolCounts`), or it
has none (a hold), the change is written **without reading** the target, conditioned on that
state. If the condition fails, the write runs again reading the target, which reports exactly what
was wrong. Otherwise (and always for `optional` with `set`) the target is read first and the change
is guarded by its revision.

If a source field of the key is empty (a `string`, `enum` or `time`), the write fails with
`dynago.ErrFieldRequired`: an empty field must never switch a rule off. An `optional` requirement
is skipped instead, which is how to declare an optional reference ("if the loan names a steward,
the steward must be active"). A write with `requires` is always a transaction. Updates with `requires` read the item
first, unless they are read-free and every field the requirements use is known from the call.

The model document shows which writes read first and why, and what each changes.

## Predicates

`where`, `when`, `set` and a require's `when` and `set` take a mapping of field to constant (a
require's may also name a field of the writing entity, `"{field}"`):

```yaml
where: { status: active, role: steward }
```

All conditions must hold (AND). Values must match the field's type: `true`/`false` for `bool`, an
integer for `int`, a number for `float`, a string for `string`, one of the declared values for
`enum`. Fields of other types cannot be compared. A condition on a zero value (`false`, `0`, `""`)
also matches an absent attribute, since zero values are not stored.

## Estimate

| Key | Meaning |
|---|---|
| `items` | Expected number of items of the entity, for the storage estimate. |

Together with `size` on fields and `rate` / `hot_key_rate` on access patterns and writes, these are
the assumptions behind the cost report. See [Costs](guides/costs.md).

## Names

| Declared | Case | Becomes |
|---|---|---|
| Entity, index, unique, counter, access, write | PascalCase | Go names as written |
| Field, counter value | camelCase | Go field names with initialisms upper-cased |

Go names are built by splitting on case changes, `_` and `-`, capitalising each word, and
upper-casing common initialisms (`id`, `url`, `uri`, `http`, `html`, `api`, `uid`, `uuid`, `ulid`,
`json`, `ttl`, `sku`, `ip`, `sql`, `css`, `xml`, `sms`): `libraryId` → `LibraryID`,
`manualUrl` → `ManualURL`, enum value `on_loan` → `OnLoan`.

Generated type names must not collide (for example an entity `MemberLoans` and a counter
`MemberLoans`); dynago reports collisions. [Generated code](generated-code.md) lists every name.

## Rules across the file

Checked by `dynago` beyond the JSON Schema:

- **Distinguishable items.** Every family of items in the base table — entities, copies, claims,
  counters — whose partition keys could coincide must have sort keys that no `begins_with` on one
  could match on the other. So a query never returns another family's items.
- **References.** Key templates, `project`, `where`, `when`, `set`, `patch`, `sum`, `range`, `ttl`
  and `unique` fields must name fields of the entity; `query` must name an index of the entity,
  `get: { unique }` a constraint, `counter` a counter of the table, `copy_of` and `requires` other
  entities of the table.
- **Key values.** At runtime, string and enum values used in keys may not contain the literal
  character that follows them in a key identifying an item (e.g. `#`): `"a#b" + "c"` and
  `"a" + "b#c"` would render the same key. Such writes and lookups fail with
  `dynago.ErrInvalidKey`. The last field of a key, and GSI sort keys, are exempt. Fields linked by a
  `requires` key share their checks, and values are checked as `|lower` renders them too.
- **Go names.** Everything becomes Go identifiers, which must not collide: two fields (or enum
  values, or entities) with the same Go name (`userId` and `userID`, `in-progress` and
  `in_progress`), a field named `key` or `version` (methods of the entity) or `pk`, `sk`, `t`, `v`,
  `rev` or `ttl` (the stored item's own attributes), an entity named like another's generated type
  (`OrderStore`), or a partition key field named `from` or `to` on a range query.
- **Writes.** `when` can't name a primary key field (the key already selects the item). A
  `requires` can't target the write's own item, or check a counter that the write (or a change it
  makes to another entity) also updates: DynamoDB rejects a transaction that touches an item
  twice. Changes to one counter item from several entities are merged into one update.
- **TTL.** Items past their `ttl` time are treated as absent by every read and write, even before
  DynamoDB deletes them (which can take days); a create may replace an expired item.
- **Consistency.** `consistent: true` is not allowed on a GSI query.
- **Transactions.** `dynago check` reports writes that could exceed 100 items.
- **Versions and generations.** Changing an entity's storage shape (fields and their attributes
  and types, keys, TTL, indexes, claims, counters) requires a higher `version`. A change existing
  items don't fit also needs a new `table.generation`, filled by the migration job; see
  [Schema changes](guides/schema-changes.md).
