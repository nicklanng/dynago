# dynago

**A schema file for DynamoDB, and the review surface for its design.** Declare your entities,
exactly how they are read and written, what each read needs, and roughly how much data and
traffic to expect. dynago then generates:

- **typed Go access code** on [guregu/dynamo](https://github.com/guregu/dynamo): one method per
  declared access pattern and nothing else;
- **a data model document** (`*.model.md`, with Mermaid diagrams) that someone who has never used
  DynamoDB can review: the domain and its relationships, the rules the code guarantees, each
  status field's lifecycle, every read and write, and which items share partitions;
- **an analysis of the design**: every partition's size, growth and busiest key at peak, the cost
  of each call, and findings with rule ids (hot keys, keys with too few values, indexes that
  silently drop items, transaction and size limits), which a schema can accept with a reason and
  a policy file can enforce;
- **architecture diffs** for pull requests (`dynago diff`), and a check that no code reaches
  DynamoDB around the schema (`dynago vet`);
- **infrastructure**: Terraform JSON and a `create-table` definition;
- **a lock file** recording each entity's storage shape per version, so schema changes are explicit
  and migrations can be planned.

It keeps what DynamoDB is good at (predictable single-request reads, horizontal scale) and addresses
what makes it hard to work with: the data model is invisible and scattered through code, key
formats live in string concatenation, partitions grow and heat up unnoticed, denormalised copies
drift out of sync, counts are expensive or wrong, and rules spanning items race.

```
schema (YAML) ──► Go store + errors          (<table>_dynago.go)
              ├─► model document             (<table>.model.md)
              ├─► Terraform + table JSON     (<table>.tf.json, <table>.table.json)
              ├─► lock / version history     (<table>.dynago.lock)
              ├─► analysis: costs, partitions, findings   (dynago check)
              └─► architecture diff          (dynago diff)
```

The example, [`examples/toollibrary`](examples/toollibrary), is a fictional neighbourhood tool
library that uses almost every feature. Read its [schema](examples/toollibrary/toollibrary.dynago.yaml),
the [model document](examples/toollibrary/toollibrary.model.md) generated from it, the
[generated Go](examples/toollibrary/toollibrary_dynago.go). dynago's
[end-to-end tests](internal/e2e/toollibrary_test.go) run that generated store against DynamoDB
Local.

## A taste

```yaml
Loan:
  doc: A member borrowing a tool.
  fields:
    libraryId: string
    toolId: string
    loanId: string
    memberId: string
    status: { type: enum, values: [active, returned] }
    dueAt: { type: time, required: true }
    returnedAt: time
  key:
    pk: "LIB#{libraryId}#TOOL#{toolId}"   # a tool's loans live together…
    sk: "LOAN#{loanId}"
  volume: { typical: 20, max: 500 }       # …20 per tool, 500 for the busiest
  indexes:
    Overdue:                      # a GSI of active loans by due date…
      pk: "LIB#{libraryId}#DUE"
      sk: "{dueAt}#{loanId}"
      where: { status: active }   # …returned loans drop out
      project: [memberId]
  counters:
    MemberLoans:                  # kept exact by every write, in the same transaction
      pk: "LIB#{libraryId}#MEMBER#{memberId}"
      sk: "LOANS"
      values:
        active: { count: true, where: { status: active }, limit: arg }
  access:
    Overdue: { query: Overdue, range: dueAt, freshness: eventual }   # a GSI will do
    ActiveLoans: { counter: MemberLoans }
  writes:
    Borrow:
      create: true
      set: { status: active }     # whatever the caller passes
      requires:                   # all in the same transaction
        Member: { key: { libraryId: libraryId, memberId: memberId }, when: { status: active } }
        Tool:
          key: { libraryId: libraryId, toolId: toolId }
          when: { status: available }
          set: { status: onLoan }  # borrowing marks the tool on loan, atomically
    Return:
      update: [returnedAt]
      set: { status: returned }
      when: { status: active }
      requires:
        Tool: { key: { libraryId: libraryId, toolId: toolId }, when: { status: onLoan }, set: { status: available } }
```

```go
st := toollibrary.New(db, toollibrary.TableName("prod-toollibrary"))   // prod-toollibrary-g2

err := st.Loans.Borrow(ctx, loan, toollibrary.LoanBorrowLimits{MemberLoansActive: dynago.Max(member.MaxLoans)})
switch {
case errors.Is(err, toollibrary.ErrMemberLoansActiveLimit): // return something first
case errors.Is(err, toollibrary.ErrLoanBorrowRequiresTool): // the tool is out
}

overdue, next, err := st.Loans.Overdue(ctx, toollibrary.LoanOverdueQuery{LibraryID: lib, To: &now}, dynago.Page{})
```

`Borrow` writes the loan, marks the tool on loan (and updates the library's tool counts), adds to
the member's active count (conditioned on their limit) and checks that the member is active,
**all in one transaction**, without reading anything first. There is no moment at which a loan
exists for an available tool, and no crash that can leave one. `Return` reads the loan first (it
leaves the member's list of current loans), then reverses the tool and the count in one
transaction. The tests race ten members for one tool and assert that exactly one gets it,
and check that a refused borrow leaves the table byte-for-byte unchanged.

## Install and use

```sh
go get -tool github.com/nicklanng/dynago/cmd/dynago   # pins the generator in go.mod (Go 1.25+)

go tool dynago check    things.dynago.yaml            # validate + analysis: costs, partitions, findings
go tool dynago generate things.dynago.yaml            # write the Go, docs, Terraform, table JSON and lock
go tool dynago generate -check things.dynago.yaml     # CI: fail if any generated file is stale
go tool dynago diff -base origin/main things.dynago.yaml   # a pull request's architectural changes
go tool dynago vet ./...                              # CI: no DynamoDB calls around the schema
```

Commit the schema **and** everything generated, including the lock file. The model document is the
review surface for the design, and `dynago diff` for each change to it
([reviewing designs and changes](docs/guides/review.md)). The
[getting started guide](docs/getting-started.md) walks through it end to end.

## What it gives you

- **Only declared reads and writes exist.** Each read is one request. A query nobody declared has no
  method, so every new way of reading data shows up in review as a schema change. `dynago vet`
  finds code that calls DynamoDB around the store; a deliberate exception is marked with its
  reason, and a lasting one is declared in the schema (a `scan` with a `reason`).
- **The design is analysed.** From a few numbers per entity (a total, or typical and max per
  parent), dynago estimates each partition's item count, size, growth and busiest key at peak,
  and reports the risks: hot partitions and counters, keys with too few values, indexes that drop
  items without a word, lookups that assume uniqueness nothing enforces, copies whose fan-out one
  transaction can't hold. Findings have rule ids: accept one on the object it's about with a
  reason, or set a `dynago.policy.yaml` for the repository.
- **Requirements, not just mechanisms.** A read states the `freshness` it needs, and dynago makes
  its index a GSI or a transactional copy accordingly, showing what the other would cost. A copied
  field says whether it's a `snapshot_of` its source or a `copy_of` that must stay equal.
- **Changes are reviewed as architecture.** `dynago diff` turns a schema change into what it means:
  a new index and the writes it now adds to, a read that became strongly consistent, a partition
  that grows, a new risk, and whether existing items need a new table generation.
- **Derived items stay consistent.** Copies (a transactional index, readable immediately),
  uniqueness claims (one per value, or per element of a set), and counters with upper (`limit`)
  and lower (`min`) bounds change atomically
  with the item, and every write path computes them the same way. GSIs are maintained by DynamoDB,
  asynchronously: usually well under a second behind.
- **Rules and changes across entities** (`requires`) happen in the write's transaction: check
  another item, change it (`set`), delete it (`consume`), or check a counter ("no active loans").
  Nothing can change in between, and nothing is left half done.
- **Writes cost what they must.** An update that feeds nothing derived is one UpdateItem. One whose
  counter change is known from its arguments (`set: { status: retired }, when: { status: available }`)
  runs without reading the item, and so do changes to other entities under the same rule. Only the
  rest read first.
- **Optional updates** (`patch:`) change only the fields given, and read first only when a given
  field feeds something derived.
- **Required fields** (`required: true`) can't be written empty.
- **Every row is timestamped:** `_created` and `_updated` on every item dynago writes, set on
  every write path, and read as `e.Timestamps()`.
- **Lost updates prevented** with document versions: `Version()`, `dynago.From`,
  `dynago.IfVersion`, and `versioned: required` writes.
- **Safe to run in production:**
  - read-modify-writes are guarded by a revision that starts at a random value;
  - a transaction or create that the SDK retries after it succeeded is not applied twice or
    reported as failed (a single-item update or delete retried that way may report a failed
    condition for a change that did go through);
  - during a rolling deploy or after a rollback, older code keeps the fields a newer version
    stored when it rewrites an item;
  - TTL-expired items are treated as gone before DynamoDB deletes them;
  - key values can't collide through separators;
  - cursors can be signed.
- **Schema changes are explicit, and migrations are generated.** Entities have versions, and a lock
  file records every shape. A change existing items don't fit (a new index, claim or counter, a new
  key) moves the table to a new generation. A generated job copies the old table into the new one
  while the old version serves, catches up, and makes a short final pass with writes stopped. The
  old table stays for rollback. See [Migrations](docs/guides/migrations.md).

## Documentation

- [Getting started](docs/getting-started.md): from an empty module to a tested store.
- [Schema reference](docs/schema.md): every key, default and rule. The
  [JSON Schema](schema/dynago.schema.json) gives editors autocomplete; add
  `# yaml-language-server: $schema=https://raw.githubusercontent.com/nicklanng/dynago/main/schema/dynago.schema.json`
  to the top of a schema file.
- [Generated code](docs/generated-code.md): every generated type, method and error.
- [Command line](docs/cli.md).
- Guides: [modelling](docs/guides/modelling.md), [analysis](docs/guides/analysis.md),
  [reviewing designs and changes](docs/guides/review.md), [counters](docs/guides/counters.md),
  [concurrency](docs/guides/concurrency.md), [schema changes](docs/guides/schema-changes.md),
  [migrations](docs/guides/migrations.md), [costs](docs/guides/costs.md).
- [Contributing](CONTRIBUTING.md) and [changelog](CHANGELOG.md).

## Testing your store

`dynagotest` connects to DynamoDB Local and gives each test its own table:

```go
db := dynagotest.DB(t)   // skips unless DYNAGO_TEST_ENDPOINT is set
st := toollibrary.New(db, dynagotest.Table(t, db, toollibrary.TableSpec))
```

`dynagotest.CountingDB` also counts the reads a call makes. In this repository, `make test` starts
DynamoDB Local in Docker and runs everything (with `DYNAGO_REQUIRE_DB=1`, so nothing skips
silently); `make test-unit` needs no Docker.

## Status and roadmap

**Working now:** everything above, exercised by end-to-end tests of the example against DynamoDB
Local.

**Next:**
1. **LSIs** (`strategy: lsi`). Now that a new table generation can add or drop them, they're no
   longer a decision you can't undo. The partition analysis already estimates the item collection
   sizes their 10 GB limit needs.
2. **Importing from another table**, such as a legacy single table, through the same job engine.
3. **Runtime checks.** Compare consumed capacity per access pattern with the estimates, so the
   declared volumes and rates stay true.

**Known limits:**
- **TTL expiry bypasses the generated code.** An expired item does not decrement its counters or
  release its claims and copies. Reads and `requires` treat it as gone, and `dynago check` warns
  about each case. Design expiring items to own nothing else, as the example's holds do.
- **Background writes change an item's version too**, so a form left open on an item that jobs
  update often will see conflicts.
- **Migrations read the whole table on every pass.** For very large, old tables, a table copy
  per structural change may stop being practical.
- **Not supported yet:** LSIs (see the roadmap), custom Go field types, batch writes,
  multi-valued index keys.

## License

MIT. See [LICENSE](LICENSE).
