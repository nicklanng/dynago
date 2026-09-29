# dynago

**A schema file for DynamoDB.** Declare your entities and exactly how they are read and written.
dynago then generates:

- **typed Go access code** on [guregu/dynamo](https://github.com/guregu/dynamo): one method per
  declared access pattern and nothing else;
- **a data model document** (`*.model.md`, with Mermaid diagrams) that someone who has never used
  DynamoDB can review;
- **infrastructure**: Terraform JSON and a `create-table` definition;
- **a cost and risk report** that shows its assumptions: item sizes, capacity per call, monthly
  cost, hot keys and transaction limits;
- **a lock file** recording each entity's storage shape per version, so schema changes are explicit
  and migrations can be planned.

It keeps what DynamoDB is good at (predictable single-request reads, horizontal scale) and addresses
what makes it hard to work with: the data model is invisible, key formats live in string
concatenation, denormalised copies drift out of sync, counts are expensive or wrong, and rules
spanning items race.

```
schema (YAML) ──► Go store + errors        (<table>_dynago.go)
              ├─► model document           (<table>.model.md)
              ├─► Terraform + table JSON   (<table>.tf.json, <table>.table.json)
              ├─► lock / version history   (<table>.dynago.lock)
              └─► cost & risk report       (dynago check)
```

The example, [`examples/toollibrary`](examples/toollibrary), is a fictional neighbourhood tool
library that uses every feature. Read its [schema](examples/toollibrary/toollibrary.dynago.yaml),
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
    pk: "LIB#{libraryId}#TOOL#{toolId}"
    sk: "LOAN#{loanId}"
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
    Overdue: { query: Overdue, range: dueAt }
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
st := toollibrary.New(db, "toollibrary-prod")

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

go tool dynago check    things.dynago.yaml            # validate + cost/risk report
go tool dynago generate things.dynago.yaml            # write the Go, docs, Terraform, table JSON and lock
go tool dynago generate -check things.dynago.yaml     # CI: fail if any generated file is stale
```

Commit the schema **and** everything generated, including the lock file. The model document's diff
is the review surface for data model changes. The [getting started guide](docs/getting-started.md)
walks through it end to end.

## What it gives you

- **Only declared reads and writes exist.** Each read is one request. A query nobody declared has no
  method, so every new way of reading data shows up in review as a schema change.
- **Derived items stay consistent.** Copies (a transactional index, readable immediately),
  uniqueness claims, and counters with upper (`limit`) and lower (`min`) bounds change atomically
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
- **Lost updates prevented** with document versions: `Version()`, `dynago.From`,
  `dynago.IfVersion`, and `versioned: required` writes.
- **Safe to run in production:**
  - read-modify-writes are guarded by a revision that starts at a random value;
  - a transaction or create that the SDK retries after it succeeded is not applied twice or
    reported as failed (a single-item update or delete retried that way may report a failed
    condition for a change that did go through);
  - old code refuses to overwrite items written by a newer schema during a rolling deploy;
  - TTL-expired items are treated as gone before DynamoDB deletes them;
  - key values can't collide through separators;
  - cursors can be signed.
- **Schema changes are explicit:** versions, a lock file, and refusal of changes that would strand
  existing items.

## Documentation

- [Getting started](docs/getting-started.md): from an empty module to a tested store.
- [Schema reference](docs/schema.md): every key, default and rule. The
  [JSON Schema](schema/dynago.schema.json) gives editors autocomplete; add
  `# yaml-language-server: $schema=https://raw.githubusercontent.com/nicklanng/dynago/main/schema/dynago.schema.json`
  to the top of a schema file.
- [Generated code](docs/generated-code.md): every generated type, method and error.
- [Command line](docs/cli.md).
- Guides: [modelling](docs/guides/modelling.md), [counters](docs/guides/counters.md),
  [concurrency](docs/guides/concurrency.md), [schema changes](docs/guides/schema-changes.md),
  [costs](docs/guides/costs.md).
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
1. **Migrations.**
   - `dynago migrate plan`: classify changes and print the ordered deploy, backfill and enable steps.
   - A backfill runner that rewrites items below the current version through the generated write
     path. It is idempotent and resumable, because writes are conditioned on `_v`.
   - Gating reads of a new counter or index until the backfill is done.
2. **Runtime checks.** Compare consumed capacity per access pattern with the estimates.
3. **Lint.** Fail when DynamoDB is called anywhere except generated code.

**Known limits:**
- **TTL expiry bypasses the generated code.** An expired item does not decrement its counters or
  release its claims and copies. Reads and `requires` treat it as gone, and `dynago check` warns
  about each case. Design expiring items to own nothing else, as the example's holds do.
- **Background writes change an item's version too**, so a form left open on an item that jobs
  update often will see conflicts.
- **Not supported:** LSIs ([deliberately](docs/guides/modelling.md#why-no-lsis)), custom Go field types, batch writes, multi-valued index keys.

## License

MIT. See [LICENSE](LICENSE).
