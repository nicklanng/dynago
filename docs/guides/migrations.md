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

6. **Later, clean up.** Once you're sure you won't roll back, remove the generation from `retain`.
   Terraform will then delete the old table. Deletion protection is on, so disable it first,
   deliberately.

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
old table's traffic. Every pass reads the whole old table, and for each entity also reads its copy
in the new table.

### Conflicts

Some items can't be copied until someone fixes the data in the old table:
- two old items claiming a value that is now unique;
- a count past a new constant `limit`;
- a key value the new schema can't take;
- an empty required field;
- two old items converting to the same new key;
- an error your conversion returns.

The job copies everything else, lists these, and **exits with an error**, so a rollout gated on it
waits. Fix the data through the old version, then run the job again: fixed items are copied, and
conflicts for items deleted meanwhile are dropped.

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

## Limits

- **Every pass reads the whole old table.** Cost and time grow with the table. For very large, old
  tables a table copy per structural change may stop being practical.
- **Only entities of the previous generation are copied.** Attributes outside its schema, and
  entities it dropped, stay behind in the old table.
- **Expired items** (past their `ttl`) aren't copied: they're gone, even if DynamoDB hasn't deleted
  them yet.
- **The final pass needs writes stopped.** Nothing stops them for you: `Recreate` (above), a
  maintenance mode or scaling the old version down are all fine.
