# Command line

```
dynago generate [-check] [-prices wru,rru,gb] [-policy file] [-new-history] <schema.dynago.yaml>...
dynago check [-json] [-prices wru,rru,gb] [-policy file] [-new-history] <schema.dynago.yaml>...
dynago diff [-base ref | -from file] [-prices wru,rru,gb] [-policy file] <schema.dynago.yaml>...
dynago vet [-tests] [-list] [packages]
```

Flags may come before or after the schema files.

Install with `go get -tool github.com/nicklanng/dynago/cmd/dynago` (run as `go tool dynago`) or
`go install github.com/nicklanng/dynago/cmd/dynago@latest`.

## `dynago generate`

For each schema, validates it, checks it against its lock file, and writes next to it (paths
configurable with [`output`](schema.md#output)):

| File | Contents |
|---|---|
| `<table>_dynago.go` | The typed store. See [Generated code](generated-code.md). |
| `<table>.model.md` | The data model document: items, keys, indexes, access patterns, writes, costs, risks, diagrams. |
| `<table>.tf.json` | Terraform (JSON syntax): a variable for the base name, an `aws_dynamodb_table` resource (`<base>-g<generation>`) with its GSIs, TTL, point-in-time recovery and deletion protection for the current table generation and each one in `retain`, and their ARNs. |
| `<table>.table.json` | The same keys and indexes as input for `aws dynamodb create-table --cli-input-json`, for local development. CreateTable can't set TTL or point-in-time recovery: enable TTL with `aws dynamodb update-time-to-live` (or `EnsureTable`). Production tables belong in the Terraform, which sets TTL, point-in-time recovery and deletion protection. |
| `<table>.dynago.lock` | Each entity's storage shape per version, and each table generation. See [Schema changes](guides/schema-changes.md). |
| `<migrate_cmd>/main.go` | With `output.migrate_cmd` set: the migration job's main package. See [Migrations](guides/migrations.md). |

Files whose content hasn't changed are not rewritten. Nothing is written if the schema is invalid,
if its storage shape changed without a version bump, if a change existing items don't fit is made
without a new table generation, or if the design has open findings the policy fails on (by
default, errors, such as an item that can exceed 400 KB). See [Analysis](guides/analysis.md).

It also prints a note for each change since the last recorded version, marked as a warning where
it needs the new generation's migration job.

`-check` writes nothing and fails if any file is out of date. Run it in CI so the committed
model document and code always match the schema.

## `dynago check`

Validates each schema and prints its analysis: item sizes and counts, capacity units per call,
monthly cost at the declared rates, storage, every partition family (items per key, size, growth,
busiest key at peak, risk), open findings and accepted ones with their reasons. See
[Analysis](guides/analysis.md) and [Costs](guides/costs.md). Like `generate`, it also checks the
schema against its lock file (a missing `version` bump or `generation` fails it) and prints a
`change:` line for each compatible change the lock file hasn't recorded yet. Nothing is written.

With `-json`, it prints the analysed design as JSON instead: entities, reads, writes, guarantees,
partitions, findings and costs, for tools of your own.

It fails if the design has open findings the policy fails on.

## `dynago diff`

Prints the architectural changes to each schema, as Markdown for a pull request: whether existing
items need a new table generation (and what the migration copies), entities, fields, indexes,
claims, counters, reads and writes added, removed or changed, guarantees, partitions, findings and
costs. See [Reviewing designs and changes](guides/review.md).

It compares the schema with its version at a git ref (`-base`, default `HEAD`), or with another
schema file (`-from`). A schema that didn't exist at the ref is shown as new. Documentation-only
changes print "no architectural changes".

Storage changes are judged against the lock file recorded with the base (at the ref, or beside the
`-from` file), exactly as `dynago generate` will judge them: a change it would refuse, such as a
missing version bump, leads the diff with why. Without a lock file there, the history starts at
the base schema.

## `dynago vet`

Finds DynamoDB calls outside generated code in the given packages (default `./...`): methods of
guregu/dynamo's types and of the AWS SDK's DynamoDB client, its paginators and waiters, methods
of interfaces over the client (taking the SDK's request inputs), and dynago's runtime functions.
Method values and function values of these (`retry(c.PutItem)`) count as calls. Creating a client
is allowed. A call marked `//dynago:raw <reason>` (at the end of its line or of its statement's last
line, alone above its statement, or in its function's doc comment) is allowed; `-list` prints those
with their reasons. Test files are skipped unless `-tests` is given. Fails if any unmarked call is
found. See [Reviewing designs and changes](guides/review.md#only-declared-access).

## Flags

| Flag | Default | Meaning |
|---|---|---|
| `-check` | off | `generate` only: fail instead of writing when outputs are stale. |
| `-policy` | the nearest `dynago.policy.yaml` in the schema's directory or above, up to the repository root | The policy file: which findings fail, rule severities, limits, required declarations. See [Analysis](guides/analysis.md#policy). |
| `-json` | off | `check` only: print the analysed design as JSON. |
| `-base` | `HEAD` | `diff` only: the git ref to compare with. |
| `-from` | | `diff` only: a schema file to compare with, instead of a git ref. |
| `-tests` | off | `vet` only: include test files. |
| `-list` | off | `vet` only: also print the calls marked `//dynago:raw` and their reasons. |
| `-prices` | `0.625,0.125,0.25` | On-demand prices: dollars per million WRU, per million RRU, per GB-month. The default is us-east-1's Standard table class as published in September 2026; prices change, so check yours. |
| `-new-history` | off | Accept a schema whose history the lock file doesn't have (entities above version 1, or a table above generation 1), starting the history there. Only right for a new table; otherwise restore the lock file. See [Schema changes](guides/schema-changes.md#the-lock-file). |

## Exit status

| Status | Meaning |
|---|---|
| 0 | Success. |
| 1 | A schema is invalid, needs a version bump, has open findings the policy fails on, or (with `-check`) has stale outputs; or `vet` found calls that bypass the schema. Errors are printed per schema; all schemas are processed. |
| 2 | Usage error: unknown command, bad flag, or no schema given. |

## With `go generate`

Put a file next to the schema:

```go
package tasks

//go:generate go tool dynago generate tasks.dynago.yaml
```

Then `go generate ./...` regenerates every schema in the module.
