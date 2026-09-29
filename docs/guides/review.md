# Reviewing designs and changes

A dynago schema is the one place a table's design lives: its entities, every read and write,
what each write guarantees, and the assumptions about volume and traffic. dynago makes that
reviewable three ways: a model document for the whole design, an architecture diff for each
change, and a check that no code reaches DynamoDB around the schema.

## The model document

`dynago generate` writes `<table>.model.md` next to the schema. It is generated, committed and
checked in CI (`dynago generate -check`), so it always describes the code that runs. It reads top
down, from what a reviewer needs to what an implementer needs:

| Section | Answers |
|---|---|
| Summary | How big is this design? The largest and busiest partitions, the cost, the open findings. |
| Domain | The entities, how they relate (and which live in each other's partitions), the guarantees the code enforces on every write, and each status field's lifecycle. |
| Reads | Every read the code can make: what it answers, what serves it, how fresh it is, how many items it returns and how many pages reading them all takes, and what it costs. |
| Writes | Every write: what it changes, what it checks, every item it touches, whether it reads first, whether it's a transaction, and what it costs. |
| Storage and partitions | The table, each index (why it's a GSI or a copy, and what the other kind would cost), a map of which items share partitions and which reads reach them, and each partition's size, growth and busiest key. |
| Risks and costs | Open findings, accepted findings with their reasons, costs, and every assumption behind the numbers. |
| Reference | Each entity in full: fields, every stored item with example keys, key conditions, errors. |

Diagrams are Mermaid, which GitHub, GitLab and most editors render in Markdown: the relationships
between entities, each lifecycle, and the partition map.

## Architecture diffs

The model document's own diff shows every change, but also every re-rendered table row.
`dynago diff` shows what changed in the design:

```sh
dynago diff -base origin/main things.dynago.yaml
```

It compares the schema with the version at a git ref (default `HEAD`), or with another file
(`-from old.dynago.yaml`), and prints Markdown for a pull request:

- **Migration first.** Whether existing items still fit, and if the change needs a new table
  generation, what the migration job copies: items, gigabytes, and the capacity each pass takes.
- **Entities, fields, indexes, claims and counters** added, removed or changed: a GSI becoming a
  copy, a projection widening, a field becoming required.
- **Reads and writes**: new and removed methods, changed freshness, a write that now reads first
  or joins a transaction, items it newly touches, capacity per call before and after.
- **Guarantees** added or lost.
- **Partitions**: new partition families, size and risk changes.
- **Findings**: new risks, resolved ones, and newly accepted ones with their reasons.
- **Costs**: storage and monthly cost at the declared volumes and rates.

Documentation-only changes produce "no architectural changes".

### In a pull request

A GitHub Actions job that comments the diff on each pull request that touches a schema:

```yaml
name: dynago
on:
  pull_request:
    paths: ["**/*.dynago.yaml"]
jobs:
  diff:
    runs-on: ubuntu-latest
    permissions: { pull-requests: write }
    steps:
      - uses: actions/checkout@v4
        with: { fetch-depth: 0 }
      - uses: actions/setup-go@v5
        with: { go-version-file: go.mod }
      - run: |
          # Changed and added schemas; a deleted one has nothing to compare.
          for f in $(git diff --name-only --diff-filter=d origin/${{ github.base_ref }} -- '*.dynago.yaml'); do
            go tool dynago diff -base origin/${{ github.base_ref }} "$f"
          done > diff.md
      - run: gh pr comment ${{ github.event.number }} --body-file diff.md --edit-last || gh pr comment ${{ github.event.number }} --body-file diff.md
        env: { GH_TOKEN: "${{ github.token }}" }
```

## Only declared access

The generated store has one method per declared read and write, and nothing else. A read nobody
declared has no method, so the list of reads in the model document is the list of reads the code
can make, as long as nothing reaches DynamoDB another way. `dynago vet` checks that:

```sh
dynago vet ./...
```

It type-checks the packages and reports every call outside generated code that makes a DynamoDB
request:

- methods of guregu/dynamo's `DB`, `Table`, queries, scans, puts, updates, deletes and batches;
- methods of the AWS SDK's DynamoDB client (v1 and v2), and its paginators and waiters;
- methods of any interface over the client: methods taking the SDK's DynamoDB request inputs
  (`*dynamodb.GetItemInput` and so on), such as SDK v1's `dynamodbiface.DynamoDBAPI` or a wrapper
  of your own;
- dynago's runtime functions that generated code uses (`dynago.Query`, `dynago.Run`, …).

Creating a client (`dynamo.New`, `dynamodb.NewFromConfig`) is allowed: the generated `New` takes
one. Test files are skipped unless `-tests` is given.

A call that must stay (a one-off backfill, an admin tool) is marked with the reason: at the end of
its line, alone on the line above the statement (which covers a call chain spread over several
lines), or in its function's doc comment:

```go
// Backfill copies legacy rows into the new table.
//
//dynago:raw one-off import from the legacy table, deleted after the migration
func Backfill(ctx context.Context, db *dynamo.DB) error { ... }
```

A mark without a reason still fails. `dynago vet -list` prints every marked call and its reason,
so the exceptions are reviewable too. `vet` sees calls whose types it can resolve: a call through a
function value or `any` goes unseen. When the need is lasting, declare it in the schema instead:
a `scan` with a `reason` is costed, documented and diffed like every other read.

## In CI

```sh
go tool dynago generate -check ./path/to/*.dynago.yaml   # generated code and docs are current
go tool dynago check ./path/to/*.dynago.yaml             # findings the policy fails on
go tool dynago vet ./...                                 # no DynamoDB access around the schema
```

A `dynago.policy.yaml` at the repository root decides which findings fail (errors by default),
changes rule severities, and sets limits and required declarations for every schema. See
[Analysis](analysis.md#policy).
