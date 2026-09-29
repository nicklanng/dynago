# Command line

```
dynago generate [-check] [-prices wru,rru,gb] [-new-history] <schema.dynago.yaml>...
dynago check [-prices wru,rru,gb] [-new-history] <schema.dynago.yaml>...
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
| `<table>.tf.json` | Terraform (JSON syntax): a variable for the table name, the `aws_dynamodb_table` resource with its GSIs, TTL, point-in-time recovery and deletion protection, and the table ARN output. |
| `<table>.table.json` | The same keys and indexes as input for `aws dynamodb create-table --cli-input-json`, for local development. CreateTable can't set TTL or point-in-time recovery: enable TTL with `aws dynamodb update-time-to-live` (or `EnsureTable`). Production tables belong in the Terraform, which sets TTL, point-in-time recovery and deletion protection. |
| `<table>.dynago.lock` | Each entity's storage shape per version. See [Schema changes](guides/schema-changes.md). |

Files whose content hasn't changed are not rewritten. Nothing is written if the schema is invalid,
if its storage shape changed without a version bump, or if the cost report has errors (such as an
item that can exceed 400 KB).

It also prints a note for each change since the last recorded version, marked as a warning where
existing data needs a backfill or cleanup.

`-check` writes nothing and fails if any file is out of date. Run it in CI so the committed
model document and code always match the schema.

## `dynago check`

Validates each schema and prints the cost and risk report: item sizes, capacity units per call,
monthly cost at the declared rates, storage, and findings. See [Costs](guides/costs.md). Nothing is
written.

## Flags

| Flag | Default | Meaning |
|---|---|---|
| `-check` | off | `generate` only: fail instead of writing when outputs are stale. |
| `-prices` | `0.625,0.125,0.25` | On-demand prices: dollars per million WRU, per million RRU, per GB-month. The default is us-east-1. |
| `-new-history` | off | Accept entities above version 1 that the lock file has no history for, starting their history at the current version. Only right when no stored item predates that version (a new table); otherwise restore the lock file. See [Schema changes](guides/schema-changes.md#the-lock-file). |

## Exit status

| Status | Meaning |
|---|---|
| 0 | Success. |
| 1 | A schema is invalid, needs a version bump, has design errors, or (with `-check`) has stale outputs. Errors are printed per schema; all schemas are processed. |
| 2 | Usage error: unknown command, bad flag, or no schema given. |

## With `go generate`

Put a file next to the schema:

```go
package tasks

//go:generate go tool dynago generate tasks.dynago.yaml
```

Then `go generate ./...` regenerates every schema in the module.
