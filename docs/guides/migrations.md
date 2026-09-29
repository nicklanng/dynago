# Migrations: moving to a new table generation

When a change doesn't fit the items already stored (a new index, claim or counter, a changed key or
field type: see [Schema changes](schema-changes.md)), the table moves to a new **generation**: a new
table, `<name>-g<generation>`, filled from the previous one by a job dynago generates. The old
table isn't touched, so rolling back means pointing the old version at it again.

## The workflow

1. **Bump the generation**, and keep the old one for rollback:

   ```yaml
   table:
     name: toollibrary
     generation: 2
     retain: [1]
   output:
     migrate_cmd: cmd/migrate-toollibrary   # generate the job's main package
   ```

2. **Generate.** dynago prints what changed and that a migration is needed. It writes:
   - the store for generation 2;
   - a struct per entity for how generation 1 stored it (`ToolG1`);
   - the migration job;
   - Terraform declaring both tables.

3. **Write the conversions dynago can't do alone.** An entity is copied field by field when the new
   shape keeps every old field with the same type and only adds optional ones. Otherwise the job
   refuses to start until you set `Migrate<Entity>`, in a file of your own in the store's package:

   ```go
   package toollibrary

   func init() {
       // Generation 2 adds barcodes: label each tool with its serial number.
       MigrateTool = func(old ToolG1) (Tool, error) {
           t := AutoMigrateTool(old) // copies the fields that carry over
           if old.SerialNumber != "" {
               t.Barcodes = []string{"LBL-" + old.SerialNumber}
           }
           return t, nil
       }
   }
   ```

   Needing a conversion is decided field by field: a new required field; a field whose type
   changed; enum values that went away; or a field that's gone. A field that's gone might have been
   renamed, and only you know which. The generated doc comment on `Migrate<Entity>` lists the
   reasons, and so does the job's error.

4. **Apply the Terraform** to create the new table. The old one stays declared while it's listed
   in `retain`.

5. **Run the job, then roll out.** See below.

6. **Later, clean up.** Once you're sure you won't roll back:
   1. Turn off the old table's deletion protection outside the generated Terraform, for example
      with `aws dynamodb update-table --table-name prod-toollibrary-g1 --no-deletion-protection-enabled`.
      The generated Terraform always enables it, so it can't be turned off there.
   2. Remove the generation from `retain` and apply the Terraform, which then deletes the table.

## The job

```
migrate-toollibrary [-table base] [-workers n] [-rate items/s] [-passes n] copy|finish|status
```

`-table` is the base name (default: the schema's table name), so an environment's tables
`prod-toollibrary-g1` and `prod-toollibrary-g2` take `-table prod-toollibrary`. It uses the AWS SDK's
usual environment: credentials, `AWS_REGION`, and `AWS_ENDPOINT_URL_DYNAMODB` for DynamoDB Local.

- **`copy`**, while the old generation serves:
  1. A **bulk pass** scans the old table in parallel segments. It converts each entity and writes
     it to the new table, with its index entries, claims, copies and counts.
  2. **Catch-up passes** scan again. Each writes only items that changed since they were copied
     (every write changes an item's revision) and removes copies of items that were deleted.
     Passes repeat until one changes nothing, or `-passes` (default 3) is reached.

  Claims, copies and counter items of the old table aren't copied; the new table rebuilds them from
  the entities, so they are exact.
- **`finish`**, once writes to the old generation have stopped, makes one last pass and marks the
  migration finished. Every pod can run it: a lease in the new table lets one do the work while the
  others wait. Once finished, `finish` and `copy` return at once.
- **`status`** prints the current pass, its progress and any conflicts.

Progress is checkpointed per scan segment in the new table (under a partition of its own), so a
restarted job resumes where it stopped. `-rate` caps items per second across workers, to spare the
old table's traffic.

**Cost of a pass.** Every pass reads:
- the whole old table;
- for each entity, its copy in the new table;
- the whole new table, to find copies whose source is gone or now converts to another key, along
  with the sources of those copies.

It writes only what changed. The job holds a lease, renewed as it runs, and every write it makes
checks it. A job that loses the lease (a paused or partitioned pod) stops without writing more.

### Conflicts

Some items can't be copied until someone fixes the data in the old table:
- two old items claiming a value that is now unique;
- a key value the new schema can't take;
- an empty required field;
- two old items converting to the same new key;
- an error your conversion returns.

The job copies everything else, lists these, and **exits with an error**, so a rollout gated on it
waits. Order within a pass doesn't cause conflicts: items that conflicted are retried after the
pass's removals. Say a member left and another joined with the same email; that's fine. Counter
limits and minimums aren't checked while copying: the counts record what the old table holds, and
a new limit applies to writes made after the cutover.

Fix the data in the old table, then run the job again. Fixed items are copied, and conflicts for
items deleted or expired meanwhile are dropped. Fix conflicts during `copy`, while the old version
still serves, because `finish` runs with it stopped.

## On Kubernetes

Run `copy` before the rollout, and `finish` with writes stopped:

```yaml
# A Helm pre-upgrade hook (or an Argo CD PreSync hook): the release waits for it.
apiVersion: batch/v1
kind: Job
metadata:
  name: migrate-toollibrary
  annotations:
    helm.sh/hook: pre-upgrade
    helm.sh/hook-delete-policy: before-hook-creation
spec:
  backoffLimit: 3
  template:
    spec:
      restartPolicy: Never
      containers:
        - name: migrate
          image: registry.example.com/toollibrary:v2   # contains the migrate binary
          command: ["/migrate-toollibrary", "-table", "prod-toollibrary", "copy"]
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: toollibrary
spec:
  # Recreate stops every old pod before a new one starts: writes to the old generation stop.
  strategy:
    type: Recreate
  template:
    spec:
      initContainers:
        # The last pass, while nothing writes. One pod does it; the others wait for it.
        - name: finish-migration
          image: registry.example.com/toollibrary:v2
          command: ["/migrate-toollibrary", "-table", "prod-toollibrary", "finish"]
      containers:
        - name: app
          image: registry.example.com/toollibrary:v2
```

- **Downtime** is the length of the last pass: the old pods stop, `finish` runs, the new pods
  start. The bulk copy and the catch-up happened while the old version served.
- **Rollouts without a new generation** don't need `Recreate`. The job runs, finds no migration to
  do (or one already finished), and exits at once, so it's safe to keep in every release.
- **Rolling back:** deploy the old version, which uses the old table. Writes made to the new table
  after the cutover don't exist in the old one: rollback loses them. To keep them, you'd migrate
  back.

## Permissions

The job and the service need different access. On EKS, give each its own IAM role through IRSA or
EKS Pod Identity: the AWS SDK's default credential chain, which the generated command uses, picks
up either.

| Who | Previous generation's table | Current generation's table |
|---|---|---|
| The service | none | `GetItem`, `BatchGetItem`, `Query`, `PutItem`, `UpdateItem`, `DeleteItem`, `ConditionCheckItem` |
| The migration job | `Scan`, `GetItem`, `BatchGetItem` | the service's, plus `Scan` |

- **Resources:** the table ARNs, plus `<table-arn>/index/*` for GSI queries.
- **Transactions have no IAM action of their own.** Each item in one is authorised by its own
  action, which is why `ConditionCheckItem` is on the list.
- **The service never needs `Scan`,** because dynago never scans.
- **Rollback:** a rolled-back version needs its old access, to the previous table. Keep that in
  the service's policy while the generation is retained.
- **The init container in the example runs as the service's service account.** So `finish` run
  that way needs the job's access in the service's role. The alternative is running `finish` as
  a second Job, with its own role, between scaling the old version down and starting the new one.

## Limits

- **Every pass reads the whole old table.** Cost and time grow with the table. For very large, old
  tables a table copy per structural change may stop being practical.
- **Only entities of the previous generation are copied.** Attributes outside its schema, and
  entities it dropped, stay behind in the old table.
- **Expired items** (past their `ttl`) aren't copied: they're gone, even if DynamoDB hasn't deleted
  them yet.
- **The final pass needs writes stopped.** Nothing stops them for you: `Recreate` (above), a
  maintenance mode or scaling the old version down are all fine.
