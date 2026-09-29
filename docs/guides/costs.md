# Costs

`dynago check` prints, and the model document includes, an estimate of every access pattern's and
write's cost and the table's storage. It exists so a design can be reviewed on numbers before it
ships, and so the numbers are argued about as assumptions, not opinions. The same numbers feed
the [partition analysis and findings](analysis.md).

```
Loan (v1)  item 464 B / 1.9 KB  2,400,000 items  storage 3.22 GB ($0.80/month)
  read   Overdue      Query                    2.5/5 RRU  $0.01/month
  read   Totals       BatchGetItem (4 shards)  2 RRU
  write  Borrow       tx 8 items               23/61 WRU  $74.52/month
  write  Return       tx 6 items + read        19/57 WRU  $62.21/month

warning [large-field] entity Tool: manual (p99 19.5 KB) makes every write cost up to 21 WRU (42
in a transaction), including writes that never change it: Relabel, Retire, Loan.Borrow,
Loan.Return. Consider moving it to an entity of its own, written only when it changes.
```

The warning is worth reading closely: marking the tool on loan rewrites the whole tool item, so a
borrow pays for the tool's 20 KB manual as well as the loan. It names the loan's writes because
they change the tool through `requires`.

## Inputs you provide

| Input | Where | Used for |
|---|---|---|
| Field sizes | `size: p50/p99` on fields (defaults per type) | Item and index entry sizes |
| Volumes | `volume` on entities: a total, or typical and max per parent ([Volume](../schema.md#volume)) | Storage, and the partition analysis |
| Call rates | `rate:` on access patterns and writes (average per second) | Monthly throughput cost, and each partition's busiest key |
| Peak | `workload.peak`, and `hot_key_rate:` on writes | Hot partition and hot counter findings |
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

## Findings

The risks the estimates reveal (items over the size limits, transactions over 100 items, hot
counters and partitions, large fields rewritten by unrelated writes) are findings with rule ids,
listed in [Analysis](analysis.md#findings). `dynago generate` refuses to write output while the
design has errors (or whatever a policy fails on).

## How far to trust it

The estimate is exact about DynamoDB's arithmetic and only as good as your inputs. Use it to compare
designs (GSI vs copy, projection choices, what a transaction costs) and to catch order-of-magnitude
mistakes. Checking it against real consumed capacity is on the roadmap.
