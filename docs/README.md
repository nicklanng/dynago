# dynago documentation

**Start here**

- [Getting started](getting-started.md): from an empty module to a tested store in ten minutes.
- [The example](../examples/toollibrary): a fictional tool library using every feature, with its
  generated code and model document.

**Reference**

- [Schema](schema.md): every key of a schema file, its defaults and rules. Editors can use the
  [JSON Schema](../schema/dynago.schema.json) for autocomplete.
- [Generated code](generated-code.md): every generated type, method and error, and how each call behaves.
- [Command line](cli.md): `dynago generate`, `check`, `diff` and `vet`.
- Runtime package: `go doc github.com/nicklanng/dynago`, or pkg.go.dev.

**Guides**

- [Modelling](guides/modelling.md): starting from access patterns; choosing keys, GSIs vs copies,
  claims and counters.
- [Counters](guides/counters.md): exact counts, limits, lower bounds, sharding.
- [Concurrency](guides/concurrency.md): what dynago guarantees, and preventing lost updates with
  document versions.
- [Schema changes](guides/schema-changes.md): versions, table generations, the lock file, and which
  changes need a new table.
- [Migrations](guides/migrations.md): the generated job that fills a new table generation, and
  running it on Kubernetes.
- [Analysis](guides/analysis.md): volumes, partitions, every finding's rule, accepting findings,
  and the policy file.
- [Reviewing designs and changes](guides/review.md): the model document, architecture diffs for
  pull requests, and keeping DynamoDB access inside the schema with `dynago vet`.
- [Costs](guides/costs.md): how capacity and cost are estimated.

**Project**

- [Contributing](../CONTRIBUTING.md)
- [Changelog](../CHANGELOG.md)
