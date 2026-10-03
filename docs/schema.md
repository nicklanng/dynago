# Schema reference

A dynago schema is one YAML file per DynamoDB table. It declares the entities stored in the table,
how each is keyed, the items derived from it (index entries, copies, uniqueness claims, counters),
and **every** way it is read and written. `dynago generate` turns it into Go, documentation,
Terraform and a lock file; `dynago check` validates it and prints its analysis: costs, partitions
and findings.

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
[Volume](#volume) · [Workload](#workload) · [Accepting findings](#accept) · [Names](#names) ·
[Rules across the file](#rules-across-the-file)

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
| `workload` | no | Table-wide workload assumptions for the analysis. See [Workload](#workload). |
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
| `retain` | no | | Older generations whose tables stay in the Terraform, for rollback: `[2]`. It must include the previous generation in the change that bumps `generation`, since the migration job copies from that table. To delete one, turn off its deletion protection outside the Terraform (the generated Terraform always enables it), then remove it here. |
| `accept` | no | | Findings about the table as a whole recorded as deliberate. See [Accepting findings](#accept). |

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
    volume: { typical: 20, max: 500 }   # loans per tool
    accept: { large-field: "..." }
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
| `volume` | no | | How many items to expect: a total, or typical and max per parent. See [Volume](#volume). |
| `accept` | no | | Findings about the entity, or about anything it declares, recorded as deliberate. See [Accepting findings](#accept). |

Every item also stores bookkeeping attributes: `_t` (entity name), `_v` (schema version it was
written at), `_rev` (revision, used for optimistic concurrency), and `_created` and `_updated`
(when dynago first wrote the item and last changed it; see [Generated code](generated-code.md#timestamps)).
Field attributes cannot use these names, `PK`, `SK`, or the table's TTL attribute.

## Fields

```yaml
fields:
  libraryId: string                                  # short form: just the type
  status: { type: enum, values: [active, returned] }
  manual: { type: string, size: 2000/20000, doc: Care and safety notes. }
  dueAt: { type: time, required: true, example: "2026-10-12T17:00:00Z" }
  toolName: { type: string, snapshot_of: Tool.name }  # the name when borrowed
  borrowerId: { type: string, ref: Member }          # holds a Member's memberId
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
| `copy_of` | no | `Entity.field` this field copies from another entity and must **stay equal to**. dynago keeps copies within an entity in sync but not this one: declaring it documents that your code must rewrite the copies when the source changes, and `dynago check` warns (`copy-drift`), saying how many items one change fans out to and whether one transaction could hold them. Types must match. |
| `snapshot_of` | no | `Entity.field` whose value this field takes when the item is written, and **deliberately keeps**: a tool's name at the time of a loan, a price at the time of an order. Documented, not warned about. Exclusive with `copy_of`. |
| `ref` | no | The entity whose key this field holds, when its name doesn't match that entity's key field (`borrowerId` holding a `Member`'s `memberId`, `userId` holding a `User`'s `id`). Fields with matching names link without it. The field supplies the key field no same-named field can, or else the entity's last key field (a `stewardId` beside `memberId` refers to another `Member`); the other key fields come from fields of the same name. When an entity holds another's key more than one way (a `memberId` and a `stewardId` both reaching `Member`), `volume.per` and `volume.by` must say which with `via`. A match by name that is just this entity's own key (`Post.id` against `User.id`) doesn't count when a `ref` to that entity exists. On a `string_set`, each element is that entity's key field (`labelIds: { type: string_set, ref: Label }`): the item refers to several of them, and `volume.by` says how many items each one has. |
| `accept` | no | Findings about the field recorded as deliberate. See [Accepting findings](#accept). |

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

A field may not be called `key`, `version` or `timestamps` (the entity's generated `Key()`,
`Version()` and `Timestamps()` methods), nor `pk`, `sk`, `t`, `v`, `rev`, `ttl`, `dynagoCreated` or
`dynagoUpdated` (the stored item's own attributes). A field called `createdAt` is fine: dynago's
own timestamps don't use that name.

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
  `int`, `time`, `bool`). An index's, a counter's and a unique constraint's templates may also
  name one `string_set`, which renders a key for each of its elements; an entity's own key can't.
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
  ByMember:                                     # a member's current loans, soonest due first
    pk: "LIB#{libraryId}#MEMBER#{memberId}"     # (on Loan); a copy, because a read through it
                                                # declares freshness: immediate
    sk: "MYLOAN#{dueAt}#{toolId}#{loanId}"
    where: { status: active }
    project: [toolName, dueAt]
```

An index is another key for the same entity. Index names are PascalCase.

| Key | Required | Default | Meaning |
|---|---|---|---|
| `strategy` | no | from the reads | `gsi`: a global secondary index, maintained by DynamoDB, read eventually consistently. `copy`: copy items written by the generated code in the same transaction as the entity, readable strongly consistently. Left out, dynago chooses: `copy` if a read through the index declares `freshness: immediate`, `gsi` otherwise, and the model document says why. See [Modelling](guides/modelling.md). |
| `pk` | yes | | Partition key template. |
| `sk` | gsi: no; copy: yes | | Sort key template. A copy's `sk` must start with literal text. |
| `project` | yes | | What the index holds: `all` (every field), `keys` (key fields only), or a list of fields. Key fields are always included. |
| `where` | no | | Only index the entity while these [predicates](#predicates) hold (a sparse index). |
| `matches` | no | | With `where`: the share of the entity's items that satisfy it, from just above 0 to 1 (`0.01` is one in a hundred). The analysis sizes the index, its partitions' traffic and the cost of the writes that maintain it with this share. Without it every item is counted, which is the most the index can hold: the results are marked as upper bounds, and left out of the summary's largest and busiest partition. |
| `doc` | no | | Shown in the model document and on the generated method. |
| `accept` | no | | Findings about the index recorded as deliberate. See [Accepting findings](#accept). |

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
- **Limits.** DynamoDB allows 100 attributes in `INCLUDE` projections per table, summed across its
  secondary indexes; `ALL` projections don't count. dynago's `_t` and `_v`, and the TTL attribute,
  count in each `INCLUDE` GSI. A field's `attr` can't be a GSI's key attribute (`ByCategoryPK`).
- **Results.** A query through an index with `project: all` returns entities; otherwise it returns
  a generated `<Entity><Index>` struct holding the projected fields.
- **Keyed by a set's elements.** A key template may name one `string_set` field: the entity then
  has an entry for **each element** of the set, so a thread with three labels is in three label
  lists:

  ```yaml
  ByLabel:
    pk: "USER#{userId}#LABEL#{labelIds}"      # labelIds is a string_set
    sk: "L#{lastMessageAt}#{threadId}"
    project: [subject, snippet, unread]
  ```

  Such an index is always a copy (a GSI holds an item under one key), written in the write's
  transaction like any copy. A write that changes the set adds the copies of new elements and
  deletes those of dropped ones; a write that changes anything else the copies hold rewrites every
  one, so its cost and its transaction grow with the set: declare the set's `size`, from which
  dynago estimates the number of elements, at 20 bytes each. A query through the index takes one
  element (`LabelIDsElem`) where it would take the field. Elements that render the same key
  (`{labelIds|lower}`) share one copy.

Limits: 20 GSIs per table (a default quota AWS can raise), and 100 attributes in `INCLUDE`
projections across a table's indexes (a fixed limit).

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
| `accept` | no | | Findings about the constraint recorded as deliberate. See [Accepting findings](#accept). |

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
| `shards` | no | `1` | Spread the counter over this many items (1–100) to raise its write throughput; a read sums them with a BatchGetItem. Bounded values cannot be sharded. |
| `values` | yes | | At least one value. |
| `doc` | no | | Shown in the model document and on the Go type. |
| `accept` | no | | Findings about the counter recorded as deliberate. See [Accepting findings](#accept). |

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

A counter's key templates may name one `string_set` field, as an index's may: the entity then
counts towards **one counter item for each element** of the set.

```yaml
LabelCounts:
  pk: "USER#{userId}"
  sk: "COUNTS#LABEL#{labelIds}"                 # one item per label
  values:
    total: count
    unread: { count: true, where: { unread: true } }
```

A thread with three labels adds to three items, in the write's transaction; changing the set moves
its contribution from the labels dropped to the labels added. The counter's key type holds one
element (`LabelCountsKey.LabelIDsElem`), and `{ counter: LabelCounts, all: true }` reads every
label's item in one Query. A `requires` on such a counter names one element in its key, from a
string field of the writing entity (`labelIds: labelId`).

Counter items aren't removed when their values return to zero: a label nothing is filed under
still has its item, reading zero, and `all: true` returns it. To remove one, `consume` it from
the write that removes what it was counting for (see [Requires](#requires)).

## Access

```yaml
access:
  Get: get
  GetConsistent: { get: key, consistent: true }
  GetByEmail: { get: { unique: Email } }
  History: { query: key, order: desc, page: 20, project: [memberId, status, dueAt] }
  Overdue: { query: Overdue, range: dueAt, freshness: eventual, rate: 1 }
  MyLoans: { query: ByMember, freshness: immediate }
  ActiveLoans: { counter: MemberLoans }
  Export: { scan: true, reason: "The nightly warehouse export reads every loan." }
  GetSeveral: { get: key, batch: 40 }             # several loans by key, one BatchGetItem
  Open: { query: partition, of: [Thread, Message, Draft] }   # entities that share a partition, one Query
  LabelCounts: { counter: LabelCounts, all: true } # every item of a counter in a partition, one Query
```

Access patterns are the **only** reads the generated store offers: one method each. Names are
PascalCase and must be unique across `access` and `writes`. Declare exactly one of `get`, `query`,
`counter` and `scan`.

| Key | Applies to | Default | Meaning |
|---|---|---|---|
| `get` | | | `key`: one GetItem by primary key (short form: `Name: get`). `{ unique: Name }`: find the entity holding a unique value — a consistent read of the claim, then of the item. |
| `batch` | `get: key` | | Makes the read take several keys: `GetSeveral: { get: key, batch: 40 }` returns the items that exist, in the order of the keys, with one BatchGetItem per 100 keys. The number is how many keys a call typically passes, which the cost estimate and the partition analysis use; a call may pass any number. Each item is billed as a GetItem of it would be. |
| `query` | | | `key`: query the entity's own partition (items matched by its sort key prefix). An index name: query that index. `partition`: read several entities' items under one partition key, listed in `of`. Exactly one Query request per page. |
| `of` | `query: partition` | | The entities whose items the read returns: `Open: { query: partition, of: [Thread, Message, Draft] }`. Every one must have the same partition key template as this entity, which is what puts their items side by side. The method takes the partition key fields and returns a generated `<Entity><Access>` value with a field per entity: a slice of each kind, or a pointer for an entity with at most one item per partition (a sort key with no field of its own). One Query reads the partition in sort key order and keeps these kinds, so the items the partition holds of other kinds (other entities, counters, copies, claims) are read and billed too, a page can hold few items and still have a next cursor, and one kind's items can span pages. Takes `order`, `page` and `max_page`. |
| `counter` | | | Read a counter: one GetItem, or a BatchGetItem over its shards. The counter may belong to any entity of the table, so a library can offer its member counts. |
| `all` | `counter` | `false` | Read every item of the counter in one partition, with one Query a page: `AllCounts: { counter: LabelCounts, all: true }` returns each label's counts for a user, where the counter's sort key is `COUNTS#LABEL#{labelId}`. The method takes the counter's partition key fields and returns `<Counter>Entry` values: the counts, and the key each item's sort key holds. The sort key's own fields must be `string` or `enum`, untransformed and separated by literal text, so they can be read back; the counter can't be sharded. Takes `page` and `max_page`. |
| `scan` | | | `true`: read every item of the entity, one page of the **whole table** per call (a Scan filtered to the entity's items, so a page can hold few or none and still have a next cursor). A declared exception for exports and backfills, never for a request path: `dynago check` notes its full-pass cost, and a policy can forbid it. Needs `reason`. |
| `reason` | scan | | Why a scan is needed. Shown in the model document and on the method. |
| `freshness` | all | | What the reader needs. `immediate`: it must see writes that just happened (read-your-writes), so the read is strongly consistent, and an index it reads without a declared `strategy` becomes a copy; through a GSI it's an error. `eventual`: a moment's lag is fine, and the read is eventually consistent (half the cost). Left out, the read is as `consistent` says, and the model document marks it "not stated". |
| `order` | query | `asc` | `asc` or `desc`, by sort key. |
| `page` | query, scan, counter with `all` | `50` | Default page size (items evaluated per request). For a partition read, size it for the whole partition if one call should return it all. |
| `max_page` | query, scan, counter with `all` | `max(100, page)` | Largest page a caller may ask for (≤ 1000). |
| `range` | query | | A field that directly follows the sort key's literal prefix. Adds optional inclusive `From` / `To` bounds to the query. |
| `consistent` | get, query, counter, scan | `false`; `true` for a query through a `copy` index | Strongly consistent read (twice the cost). Not allowed on a `gsi` index. A copy index is read consistently unless it says `consistent: false` or `freshness: eventual`: read-your-writes is why it's a copy. Prefer `freshness`, which states the need rather than the mechanism; the two may not contradict each other. |
| `project` | `query: key` | all fields | Read only these fields (plus key fields): `[name, status]`, or `keys`. Returns a generated `<Entity><Access>Item` type. Saves bandwidth and keeps other fields (secrets, large text) from callers; **DynamoDB still bills the whole item**, so for cheaper lists use an index with a narrow projection. |
| `doc` | all | | Shown in the model document and on the method. |
| `rate` | all | | Average calls per second, for the monthly cost estimate and the partition analysis. |
| `accept` | all | | Findings about the read recorded as deliberate. See [Accepting findings](#accept). |

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
| `rate` | all | Average calls per second, for the monthly cost estimate and the partition analysis. |
| `add_to` | update | Adds elements to `string_set` fields, leaving the rest of each set as it is: `AddLabel: { add_to: { labelIds: arg } }`. The value is `arg` (the caller passes the elements, as a `[]string` named after the field) or one constant element. An element the set already holds changes nothing. If nothing is keyed by the set, it is DynamoDB's atomic `ADD` on one UpdateItem, with no read, and concurrent additions all land; if an index or counter is keyed by its elements, the item is read, and only the copies and counts of the elements that are new are written. A write that changes nothing but set elements leaves an item that is already as asked untouched. Goes with `batch`: the same elements to several items. |
| `remove_from` | update | Removes elements from `string_set` fields, the same way: `RemoveLabel: { remove_from: { labelIds: arg } }`. An element the set doesn't hold changes nothing. Not on a `required` field, which it could leave empty. |
| `batch` | update | Makes the write take several keys and apply the same change to each: `Archive: { set: { mailbox: archived }, batch: 50 }` generates `Archive(ctx, keys []ThreadKey) error`. The items are read together and written several to a transaction (as many as fit in DynamoDB's 100 items, counting what each one's change touches), each guarded by its revision, and **a counter item several of them change is updated once per transaction**, so fifty changes to one user's threads touch the user's counter a handful of times, not fifty. Each transaction is atomic; the batch is not: it returns a `*dynago.BatchError` naming the items not written (absent, failing `when`, or refused), and the rest are written. The number is how many items a call typically changes, for the estimates. Not with `requires`, `versioned: required`, or a counter value whose limit callers supply. |
| `hot_key_rate` | all | Peak calls per second against one partition key. Without it, the analysis estimates each partition's share of `rate` from the volumes; with it, this number is used for every partition the write touches. |
| `accept` | all | Findings about the write recorded as deliberate. See [Accepting findings](#accept). |

How an update runs is decided from the schema:

- If it changes nothing that feeds an index key, a `where`, a copy, a claim or a counter, it is
  **one conditional UpdateItem** with no read.
- If it changes no field used in an index's keys or `where`, or projected into a copy, and the
  counters and claims it changes depend only on key fields and fields its `when` pins (for
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
| `set` | Entities only. Changes the target in the same transaction: its revision, counters, claims, copies and index keys are maintained as by an update of it declared with this `set` and `when`. Values are constants or `"{field}"`. Not key fields; not a counter value whose limit the target's callers supply. The same holds for `add` and `patch`, and a field is changed by one of the three. |
| `add` | Entities only. Adds to `int` fields of the target: `add: { messageCount: 1 }`. A whole number (negative to subtract) or `"{field}"`, an int field of this entity. Concurrent writes all count: the addition is DynamoDB's atomic `ADD` when the target isn't read, and guarded by the target's revision (and retried) when it is. |
| `patch` | Entities only. Sets fields of the target from this entity's fields, each only when that field has a value: `patch: { hasAttachments: "{hasAttachments}" }` marks the thread when a message with attachments arrives, and leaves the mark alone when one without arrives. Use `set` to assign whatever the field holds, empty or not. |
| `ensure` | Entities only. Creates the target when it is absent (or expired), in the same transaction: `ensure: { subject: "{subject}" }`, or `ensure: {}`. The new item has its key from `key`, the fields `ensure` gives (constants or `"{field}"`), then `set`, `add` and `patch` applied, and its counters, claims, copies and index keys are written as a create of it writes them. A target that is there is checked against `when` and changed as without `ensure`; `when` says nothing about one the write creates. Every `required` field of the target must be given by `ensure` or `set`. Exclusive with `optional` and `consume`. See [a parent and its first child](#a-parent-and-its-first-child). |
| `optional` | Entities only. The write goes ahead if the target is absent or has expired; `when` applies only to one that is there. |
| `consume` | Deletes the target in the same transaction. For an entity, it releases what the item contributed; exclusive with `set`, `add` and `patch`. For a counter, it deletes the counter item, which must read zero for **every** value (whether or not `when` names it): `Label.Remove` with `requires: { LabelCounts: { key: { userId: userId, labelIds: labelId }, consume: true } }` is refused while a thread carries the label, and takes the label's counter item with it. A counter item that was never written passes. |

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

#### A parent and its first child

A conversation is a thread and its first message; later messages change the thread through
`requires`. With `ensure`, the first message creates it, so the two are never written apart:

```yaml
Message:
  writes:
    Deliver:
      create: true
      requires:
        Thread:
          key: { userId: userId, threadId: threadId }
          ensure: { subject: "{subject}" }          # given only to a thread this write creates
          set: { lastMessageAt: "{date}", snippet: "{snippet}", unread: true, mailbox: inbox }
          add: { messageCount: 1 }
```

If no thread is at the key, the write creates one with the message's subject, the `set` values and
a count of 1. If one is there, it is changed as before, and `ensure`'s fields are left alone. When
two first messages race, one creates the thread and the other finds it: the loser's transaction
fails its "no thread yet" condition and runs again.

The target is read to find out which case it is, unless the change can be written without reading
it: then the write tries the change first, and reads only when no target turns out to be there.

## Predicates

`where`, `when`, `set` and a require's `when` and `set` take a mapping of field to constant (a
require's may also name a field of the writing entity, `"{field}"`), which the field must equal:

```yaml
where: { status: active, role: steward }
```

All conditions must hold (AND). Values must match the field's type: `true`/`false` for `bool`, an
integer for `int`, a number for `float`, a string for `string`, one of the declared values for
`enum`. Fields of other types cannot be compared. A condition on a zero value (`false`, `0`, `""`)
also matches an absent attribute, since zero values are not stored.

A condition (`where`, `when`, and a require's `when`; not `set`) can also exclude a value, or list
several:

```yaml
where: { hasSent: true, mailbox: { not: trash } }      # every mailbox but the trash
when:  { mailbox: { in: [inbox, archived, sent] } }     # one of these
```

| Form | Holds when |
|---|---|
| `field: value` | the field equals the value |
| `field: { not: value }` | the field holds anything else. An empty field counts as holding its zero value, so `{ not: trash }` holds for an item with no mailbox, and `{ not: "" }` holds only for one that has a value. In a require's `when`, the value may be `"{field}"` of the writing entity. |
| `field: { in: [a, b] }` | the field equals one of the values (two or more, and not every value of an enum) |

A `when` that names one value tells dynago the state the write starts from, which is what lets a
write that moves a counter run [read-free](#writes). `not` and `in` leave several states possible,
so a write whose `when` uses one for a field that feeds a counter, an index or a claim reads the
item first. Where nothing derived depends on the field, the condition goes on the single
UpdateItem as any other does.

## Volume

How many items of the entity to expect, at the point in time `workload.horizon` names. Either a
total, for an entity at the top:

```yaml
Library:
  volume: 2000
```

or a number per item of its **parent**, for everything below:

```yaml
Member:
  volume: { typical: 20, max: 2000 }        # per Library: most are small, the biggest has 2,000
Loan:
  volume:
    typical: 20                             # per Tool
    max: 500
    by: { Member: { typical: 60, max: 400 } }
Hold:
  volume: { typical: 0.02 }                 # per Tool: at most one, and few tools have one
```

| Key | Meaning |
|---|---|
| `per` | The parent the numbers count against. Left out, it is the entity whose partition this one's nests in: its partition key is a prefix of this entity's, and this entity holds its key (by field name, or `ref`). If several entities qualify equally, the one declared first. |
| `typical` | Items per parent item, typically. Fractions are fine (0.02 holds per tool). |
| `max` | Items per parent item, at most: the biggest parent. Skew lives here, and so do hot partitions: a partition's largest size and traffic follow from it. Left out, unknown (or 1, when this entity's key is its parent's key). |
| `by` | How the items spread over other entities whose keys they hold: `{ Member: { typical, max } }` is how many loans one member has. Partitions keyed by that entity's key (a copy or GSI keyed by `memberId`, a counter per member) are counted with it. Without it, the items are assumed to spread evenly, with no known maximum. It can name an entity further up the parent chain, such as a draft's `by: { User: ... }` when drafts are counted per thread: without it the most a user has is estimated from the chain, as the busiest thread's count times a typical number of threads. It can't name the `per` entity itself, whose numbers are the volume's own. For an entity a `string_set` field refers to (`ref`), it is how many items carry each element: `by: { Label: { typical: 150, max: 20000 } }` sizes the partitions of an index or counter keyed by the set's elements. |
| `via` | In `volume` (with `per`) or in an entry of `by`: the field linking to that entity, when this entity holds its key more than one way. A loan with a `memberId` (its borrower) and a `stewardId: { ref: Member }` says `by: { Member: { typical: 60, via: memberId } }`. Without it, dynago refuses to guess. |

Totals multiply down: 2,000 libraries × 60 tools × 20 loans is 2.4 million loans. From the volumes
and the key templates, `dynago check` and the model document estimate every partition's item
count, size and busiest key. Counts are approximate by design: declare the order of magnitude and
the skew you expect, not a forecast. See [Analysis](guides/analysis.md).

## Workload

```yaml
workload: { horizon: 3 years, peak: 5 }
```

| Key | Default | Meaning |
|---|---|---|
| `horizon` | | When the volumes are expected ("3 years"). Shown to readers, so everyone argues about the same point in time. |
| `peak` | `1` | Peak traffic as a multiple of the declared average `rate`s. The partition analysis estimates the busiest key at peak; the monthly cost uses the averages. |

## Accept

```yaml
Hold:
  indexes:
    ByCode:
      pk: "HOLDCODE#{codeHash}"
      project: [memberId]
      accept:
        unenforced-unique: "Codes are HMACs under a server-side secret: collisions are negligible."
```

`dynago check` reports findings, each with a rule id and the schema object it is about. `accept`
records one as deliberate, on the object it is about (the table, an entity, a field, an index, a
constraint, a counter, a read or a write), with the reason: rule id → reason. An accepted finding
is listed with its reason in the model document and doesn't fail the build.

- Only warnings and notes can be accepted. An error is a limit or a certain failure: fix the design,
  or the assumption behind the estimate.
- An acceptance must match a finding: naming an unknown rule, or a finding the object doesn't have
  (it was fixed, or moved), is an error, so acceptances don't outlive their reasons.
- An acceptance on an entity also covers the rule's findings about everything the entity declares
  (fields, indexes, constraints, counters, reads, writes), so a reason shared by six copied fields
  is written once. One on the object itself takes precedence.
- A policy can turn a rule off, or make it an error, for every schema at once. See
  [Analysis](guides/analysis.md) for every rule and the policy file.

## Names

| Declared | Case | Becomes |
|---|---|---|
| Entity, index, unique, counter, access, write | PascalCase | Go names as written |
| Field, counter value | camelCase | Go field names with initialisms upper-cased |

Go names are built by splitting on case changes, `_` and `-`, capitalising each word, and
upper-casing common initialisms (`id`, `url`, `uri`, `http`, `html`, `api`, `uid`, `uuid`, `ulid`,
`json`, `ttl`, `sku`, `ip`, `sql`, `css`, `xml`, `sms`, `https`) and their plurals: `libraryId` →
`LibraryID`, `manualUrl` → `ManualURL`, `labelIds` → `LabelIDs`, enum value `on_loan` → `OnLoan`.

Generated type names must not collide (for example an entity `MemberLoans` and a counter
`MemberLoans`); dynago reports collisions. [Generated code](generated-code.md) lists every name.

## Rules across the file

Checked by `dynago` beyond the JSON Schema:

- **Distinguishable items.** Every family of items in the base table — entities, copies, claims,
  counters — whose partition keys could coincide must have sort keys that no `begins_with` on one
  could match on the other. So a query never returns another family's items.
- **References.** Key templates, `project`, `where`, `when`, `set`, `patch`, `sum`, `range`, `ttl`
  and `unique` fields must name fields of the entity; `query` must name an index of the entity,
  `get: { unique }` a constraint, `counter` a counter of the table; `copy_of`, `snapshot_of`,
  `ref`, `requires`, `volume.per` and `volume.by` other entities of the table (and `via` a field of
  the entity).
- **Key values.** At runtime, string and enum values used in keys may not contain the literal
  character that follows them in a key identifying an item (e.g. `#`): `"a#b" + "c"` and
  `"a" + "b#c"` would render the same key. Such writes and lookups fail with
  `dynago.ErrInvalidKey`. The last field of a key, and GSI sort keys, are exempt. Fields linked by a
  `requires` key share their checks, and values are checked as `|lower` renders them too.
- **Go names.** Everything becomes Go identifiers, which must not collide: two fields (or enum
  values, or entities) with the same Go name (`userId` and `userID`, `in-progress` and
  `in_progress`), a field named `key`, `version` or `timestamps` (methods of the entity) or `pk`,
  `sk`, `t`, `v`, `rev`, `ttl`, `dynagoCreated` or `dynagoUpdated` (the stored item's own
  attributes), an entity named like another's generated type
  (`OrderStore`), or a partition key field named `from` or `to` on a range query.
- **Writes.** `when` can't name a primary key field (the key already selects the item). A
  `requires` can't target the write's own item, or check a counter that the write (or a change it
  makes to another entity) also updates: DynamoDB rejects a transaction that touches an item
  twice. Changes to one counter item from several entities are merged into one update.
- **TTL.** Items past their `ttl` time are treated as absent by every read and write, even before
  DynamoDB deletes them (which can take days); a create may replace an expired item.
- **Consistency.** `consistent: true` and `freshness: immediate` are not allowed on a GSI query;
  `freshness` and `consistent` may not contradict each other.
- **Relations and volumes.** `ref`, `volume.per` and `volume.by` must name entities whose key this
  entity holds (fields linked by name, or through a `ref` field), with `via` naming the link when
  there is more than one. A `per`-parent volume needs a parent; volumes must not count per each
  other in a circle.
- **Analysis.** Beyond validity, `dynago check` reports findings (limits, hot partitions, risky
  shapes); errors stop `dynago generate`. See [Analysis](guides/analysis.md).
- **Versions and generations.** Changing an entity's storage shape (fields and their attributes
  and types, keys, TTL, indexes, claims, counters) requires a higher `version`. A change existing
  items don't fit also needs a new `table.generation`, filled by the migration job; see
  [Schema changes](guides/schema-changes.md).
