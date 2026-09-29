# Costs and risks

`dynago check` prints, and the model document includes, an estimate of every access pattern's and
write's cost, the table's storage, and a list of design risks. It exists so a design can be
reviewed on numbers before it ships, and so the numbers are argued about as assumptions, not
opinions.

```
Loan (v1)  item 464 B / 1.9 KB  storage 2.68 GB ($0.67/month)
  read   Overdue      Query                    2.5/5 RRU
  read   Totals       BatchGetItem (4 shards)  2 RRU
  write  Borrow       tx 8 items               23/61 WRU  $74.52/month
  write  Return       tx 6 items + read        19/57 WRU

warning: Tool: manual (p99 19.5 KB) makes every write cost up to 21 WRU (42 in a
transaction), including writes that never change it: Relabel, Retire, Loan.Borrow, Loan.Return.
Consider moving it to an entity of its own, written only when it changes.
```

The warning is worth reading closely: marking the tool on loan rewrites the whole tool item, so a
borrow pays for the tool's 20 KB manual as well as the loan. It names the loan's writes because
they change the tool through `requires`.

## Inputs you provide

| Input | Where | Used for |
|---|---|---|
| Field sizes | `size: p50/p99` on fields (defaults per type) | Item and index entry sizes |
| Item counts | `estimate: { items: N }` on entities | Storage |
| Call rates | `rate:` on access patterns and writes (average per second) | Monthly throughput cost |
| Peak per key | `hot_key_rate:` on writes | Hot partition and hot counter warnings |
| Prices | `-prices wru,rru,gb` (default: us-east-1 on-demand, Standard table class, as published in September 2026: $0.625/M WRU, $0.125/M RRU, $0.25/GB-month) | Dollars. The defaults are fixed figures, so check them against the current price list and your region. The free tier (25 GB of storage a month) and the Standard-IA table class aren't modelled. |

Anything not declared is not costed; the report lists its assumptions.

## How it is computed

DynamoDB's own rules:

- **Writes**: 1 WRU per started KB of the item written (an update costs the larger of the item
  before and after). **Transactions cost double** for every item in them, and a `requires` condition
  check on another item is billed as a transactional write of that item, even if it fails.
- **Reads**: 1 RRU per started 4 KB, half for eventually consistent reads. A Query page costs the
  total size of the items it reads, not their number.
- **GSI entries** cost a write on the index each time an entry is written, and two (a delete and a
  put) when its keys change. They are written by DynamoDB, outside any transaction.
- **Storage** is item size plus 100 bytes of overhead per item, for items and index entries.

dynago applies these per write from the schema: which derived items a write touches, whether it
needs a transaction, whether an index entry moves, whether it reads first (read-free writes don't). Sizes are the sum of
attribute names and declared value sizes, assuming every field is present. Empty fields aren't
stored, so real items are usually smaller.

## Risks it flags

| Severity | Finding |
|---|---|
| error | An item's p99 size exceeds DynamoDB's 400 KB limit |
| error | A write can touch more than 100 items (the transaction limit; transactions are also limited to 4 MB in total, which isn't estimated) |
| error | A counter item would take more than a partition's ~1,000 WRU/s |
| warning | A counter item is written by more than ~20 transactions a second: conflicts become routine (suggests a shard count) |
| warning | A partition key takes more than half its ~1,000 WRU/s |
| warning | An item's p99 size exceeds 100 KB (every read and write of it is expensive) |
| warning | A large field (≥ 8 KB at p99) on an entity with writes that never change it: each of those writes still pays for it |
| warning | A `copy_of` field: another entity's value that your code must keep in sync |
| warning | TTL on an entity with counters, claims or copies (expiry doesn't release them) |
| note | A counter item written by several transactions a second (occasional retried conflicts) |
| note | A GSI projecting ALL (every write stores the item twice) |
| note | A GSI shared by several entities |

`dynago generate` refuses to write output while there are errors.

## How far to trust it

The estimate is exact about DynamoDB's arithmetic and only as good as your inputs. Use it to compare
designs (GSI vs copy, projection choices, what a transaction costs) and to catch order-of-magnitude
mistakes. Checking it against real consumed capacity is on the roadmap.
