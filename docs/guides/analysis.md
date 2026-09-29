# Analysis: partitions, findings and policy

`dynago check` and the model document analyse the design a schema describes, not only its syntax.
From a few declared assumptions they estimate how items group into partitions, how big and busy
each partition gets, what each read and write costs, and which choices are risky. Each finding
has a rule id, so a schema can accept one with a reason, and a policy can tune or enforce them
across a repository.

The goal is approximate architectural reasoning, not capacity planning. The inputs are orders of
magnitude and skew; the output says which parts of the design are fine by a wide margin, which
are close to a limit, and why.

## Inputs

| Input | Where | Drives |
|---|---|---|
| Volumes | `volume` on entities: a total, or typical and max per parent | Item counts, partition sizes, storage, pages to read a partition, each partition's share of traffic |
| Spread over other entities | `volume.by` | Partitions keyed by another entity's key (a member's loans) |
| Horizon | `workload.horizon` | When the volumes are expected; shown to readers |
| Peak | `workload.peak` | Peak traffic as a multiple of the average rates |
| Rates | `rate` on reads and writes (average per second) | Monthly cost, and each partition's busiest key at peak |
| Hot keys | `hot_key_rate` on writes | Replaces the estimated per-key rate for every partition the write touches |
| Freshness | `freshness` on reads | Chooses between GSI and copy, and flags copies nobody needs |
| Field sizes | `size` on fields | Item, entry and partition sizes, capacity per call |

Nothing is required. What isn't declared isn't estimated, and the document says "unknown" rather
than guessing. `require:` in a [policy](#policy) can insist on volumes, rates or freshness.

A typical declaration is a handful of numbers per entity:

```yaml
workload: { horizon: 3 years, peak: 5 }
entities:
  Library: { ..., volume: 2000 }
  Member:  { ..., volume: { typical: 20, max: 2000 } }      # per Library
  Tool:    { ..., volume: { typical: 60, max: 5000 } }      # per Library
  Loan:
    ...
    volume: { typical: 20, max: 500, by: { Member: { typical: 60, max: 400 } } }   # per Tool
```

`max` is the most important number: the biggest tenant, the busiest user, the tool that's always
out. Most DynamoDB trouble comes from the largest partition, not the average one.

## Partitions

Every family of items (an entity's items, its index entries or copies, its claims, a counter's
items) has a partition key template. Families whose templates are the same share partitions:
in the example, `LIB#{libraryId}` holds a library, its members and two counters. For each such
**partition family**, in the base table and in each GSI, the analysis estimates:

- **Items per partition key value**, typical and at most, for each family in it. Counts follow the
  volumes up the parent chain (loans per library = tools per library × loans per tool). The
  largest takes the most skewed step at its max and the others at their typical value: the
  biggest library with typical tools, or a typical library with the busiest tool, whichever is
  larger, not both at once.
- **Size** of a partition key value's items, typical and largest.
- **Growth.** A family grows without bound when nothing removes its items: the entity is created,
  never deleted, doesn't expire, and no index `where` moves it out. A tool's loan history grows
  for as long as the tool exists. That's fine on its own (without a local secondary index a
  partition key value has no size limit), but it means the volumes must describe the horizon, not
  today, and that "read all of it" stops being cheap.
- **The busiest key at peak**, in WRU and RRU per second: each write's and read's peak rate times
  the busiest partition's share of the entity's items (its largest count over the total), times
  the capacity each call uses there.
- **Risk**: the busiest key's share of one partition's throughput (1,000 WRU and 3,000 RRU per
  second). Low under 10%, medium under 50%, high above. High is a `hot-partition` warning.

How the analysis treats partition key fields:

| Field in the partition key | Counted as |
|---|---|
| A key field of an ancestor (`libraryId` in `LIB#{libraryId}#DUE`) | The ancestor's share: items per library |
| The key of an entity this one refers to (`memberId`, by name, `ref` or `requires`) | `volume.by` if declared; otherwise spread evenly, maximum unknown |
| An enum | Divides the typical count by its values; the maximum stays (they could all share one) |
| Anything else (`codeHash`, a free-text name, a time) | Unknown: nothing says how many items share a value |

### What a hot partition means

DynamoDB serves each partition key value from one partition, up to 1,000 WRU and 3,000 RRU per
second. What happens above that depends on the shape:

- **One item** (a counter, a singleton) can never be split: over the limit, writes throttle.
  `hot-counter` and `hot-partition` report this as an error.
- **Many items under one partition key** can be split by sort key range when load is spread over
  many sort keys. It doesn't help when writes go to the end of the range (a sort key starting with
  a time), and a table with a local secondary index can't split an item collection at all. The
  analysis reports these as warnings, and says when the sort key starts with a time.
- **A GSI partition** has the same limits, and a throttled GSI throttles the base table's writes
  that feed it.

### Local secondary indexes

dynago doesn't support LSIs yet. When it does, partition size matters in a new way: a table with
an LSI caps each partition key value's items and index entries at 10 GB. The growth and size
estimates are what that check will use.

## Reads and writes

The model document lists every read with its freshness, what it returns and, for queries, how
many items its partition holds and how many pages reading them all takes (`1–25 pages of 20`: a
tool's loan history is usually one page and at worst 25). A scan's pages go through the whole base
table, whatever it holds, so its page count follows the table's items, not the entity's. Every
write lists what it checks, every item it changes, whether it reads first, whether it's a
transaction, and its cost.

## GSI or copy

An index is either a GSI, maintained by DynamoDB a moment after each write, or a copy, written in
the write's own transaction. The choice trades write cost for read freshness:

| | GSI | Copy |
|---|---|---|
| Writes | cheaper: the item, plus DynamoDB's entry write | the item and the copy in one transaction (twice the units) |
| Reads | eventually consistent (usually well under a second behind) | strongly consistent: read-your-writes |
| Keys | anything | must include the entity's key; the sort key starts with literal text |

Declare what the read needs, not the mechanism:

```yaml
access:
  MyLoans: { query: ByMember, freshness: immediate }   # a member sees a loan they just took
  Overdue: { query: Overdue, freshness: eventual }      # the reminder run can lag
```

An index without a `strategy` becomes a copy if any read through it needs `immediate`, and a GSI
otherwise. Either way, the model document shows what the other kind would cost ("As a GSI:
Loan.Borrow 23 → 22 WRU; Loan.Extend 8 → 5 WRU, no longer a transaction; MyLoans would lose
immediate freshness"). A declared GSI with an immediate read is an error; a copy that no read
needs is a `copy-not-needed` note.

## Guarantees and lifecycles

The document states what the generated code enforces, in words: uniqueness, counter bounds,
required fields, and what each write requires of other items. It also draws each enum field's
lifecycle from the writes: which write moves a tool from `available` to `onLoan`, which values
no write leaves, and which writes let the caller choose a value.

## Findings

Every finding has a severity, a rule id, and the schema object it's about.

| Rule | Default | What it catches |
|---|---|---|
| `item-too-large` | error, limit | An item's p99 size is over DynamoDB's 400 KB limit. |
| `transaction-too-many-items` | error, limit | A write can touch more than 100 items, DynamoDB's transaction limit. |
| `transaction-too-large` | error, limit | A write's transaction can exceed DynamoDB's 4 MB limit. |
| `accept-invalid` | error, limit | An acceptance names an unknown rule, an error, or a finding the object doesn't have. |
| `hot-partition` | warning; error on a single item over capacity | A partition key's busiest value takes over half its throughput at peak. |
| `hot-counter` | error | A counter item takes more write units per second than a partition can serve. Suggests a shard count. |
| `counter-contention` | warning, or note | Transactions update one counter item often enough (over ~20 a second) to conflict and retry. |
| `low-cardinality-key` | warning | A partition key holds nothing but constants and enums (`STATUS#{status}`), so all of an entity's items share a few partitions. |
| `sparse-index` | warning | An index is keyed by an optional field, so items without it silently drop out of the index and every read through it. |
| `unenforced-unique` | warning | A read takes one entry of an index (`page: 1`, no sort key) as if its key were unique, but no unique constraint makes it so. |
| `unused-index` | warning | No declared read uses an index, yet every write pays for it. |
| `copy-drift` | warning | A `copy_of` field must be rewritten by your code when its source changes; says how many items one change fans out to, and whether one transaction could hold them. |
| `item-large` | warning | An item's p99 size is over 100 KB: every read and write of it is expensive. |
| `large-field` | warning | A large field makes writes that never change it pay for it. |
| `ttl-counter` | warning | An expiring entity feeds counters, which TTL deletion never decrements. |
| `ttl-claim` | warning | An expiring entity holds unique claims, which TTL deletion never releases. |
| `ttl-copy` | warning | An expiring entity has copies, which outlive it. |
| `copy-not-needed` | note | A copy index costs transactions, but every read through it accepts eventual freshness. |
| `scan` | note | A declared scan reads the whole table; says what a full pass costs. |
| `project-all` | note | A GSI projects every attribute, storing and writing each item twice. |
| `shared-gsi` | note | Several entities share one GSI. |
| `volume-missing` | policy | An entity declares no volume (`require.volumes`). |
| `rate-missing` | policy | A read or write declares no rate (`require.rates`). |
| `freshness-unstated` | policy | A read doesn't state its freshness (`require.freshness`). |
| `item-size-limit` | policy | An item's p99 size exceeds `limits.item_size`. |
| `partition-size-limit` | policy | A partition's largest estimated size exceeds `limits.partition_size`. |
| `transaction-items-limit` | policy | A write touches more items than `limits.transaction_items`. |
| `gsi-limit` | policy | The table has more GSIs than `limits.gsis`. |
| `index-limit` | policy | An entity has more indexes than `limits.indexes_per_entity`. |

The rules are deliberately few: each catches a mistake that fails in production or silently
returns wrong results, not a matter of style.

## Accepting a finding

A warning or note that is deliberate is accepted on the object it's about, with the reason:

```yaml
indexes:
  ByName:
    pk: "LIB#{libraryId}#MEMBERS"
    sk: "{name|lower}#{memberId}"
    project: [role, status]
    accept:
      sparse-index: "Members invited by email join without a name, and stay out of the directory until they give one."
```

The model document lists accepted findings with their reasons, so the decision is reviewed with
the design. An acceptance covers every finding of its rule about its object: a write touching two
hot counters has two `counter-contention` findings, and one acceptance covers both. Errors can't be accepted. An acceptance that no longer matches a finding is an error,
so reasons don't outlive what they explained.

## Policy

A `dynago.policy.yaml` holds rules for every schema in a repository: `dynago` uses the nearest one
in the schema's directory or above it (or `-policy <file>`).

```yaml
# Fail the build on warnings, not just errors.
fail_on: warning

# Change a rule's severity (error, warning, note), or turn it off. DynamoDB's limits can't be changed.
rules:
  scan: error               # no scans in this repository
  shared-gsi: off

# Limits stricter than DynamoDB's own.
limits:
  item_size: 64KB           # p99
  partition_size: 1GB       # the largest partition key value's items
  transaction_items: 25
  gsis: 10                  # per table
  indexes_per_entity: 4

# What every schema must declare.
require:
  volumes: true
  rates: true
  freshness: true
```

| Key | Default | Meaning |
|---|---|---|
| `fail_on` | `error` | The least severe open finding that fails `dynago check` and `dynago generate`. |
| `rules` | | Rule id → `error`, `warning`, `note` or `off`. |
| `limits.item_size` | none | Largest p99 item size (`item-size-limit`). Sizes take `B`, `KB`, `MB`, `GB` (1 KB = 1,024 bytes). |
| `limits.partition_size` | none | Largest estimated partition size (`partition-size-limit`). |
| `limits.transaction_items` | none | Most items one write may touch (`transaction-items-limit`). |
| `limits.gsis` | none | Most GSIs per table (`gsi-limit`). |
| `limits.indexes_per_entity` | none | Most indexes per entity (`index-limit`). |
| `require.volumes` | `false` | Every entity declares `volume` (`volume-missing`). |
| `require.rates` | `false` | Every read and write declares `rate` (`rate-missing`). |
| `require.freshness` | `false` | Every read declares `freshness` (`freshness-unstated`). |

The schema holds the design and its accepted exceptions. The policy holds what applies to every
design. A rule the policy makes an error can't be accepted, so a policy can forbid exceptions too.

## How far to trust it

The arithmetic follows DynamoDB's rules exactly; the inputs are estimates. Use the analysis to
compare designs, to find the partition that grows or heats first, and to catch mistakes an order
of magnitude away from a limit. When real traffic exists, compare it with the estimates and adjust
the volumes and rates, so the document stays true.
