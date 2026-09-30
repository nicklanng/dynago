# Getting started

This walks from an empty Go module to a tested DynamoDB store in about ten minutes. You need Go
1.25 or later, and Docker for DynamoDB Local.

We'll build a to-do list: tasks in projects, with a live count of open and done tasks.

## 1. Add dynago to your module

```sh
go get -tool github.com/nicklanng/dynago/cmd/dynago
```

This pins the generator in `go.mod` so everyone runs the same version with `go tool dynago`.
(Or install it globally: `go install github.com/nicklanng/dynago/cmd/dynago@latest`.)

## 2. Write a schema

Create `tasks/tasks.dynago.yaml`:

```yaml
# yaml-language-server: $schema=https://raw.githubusercontent.com/nicklanng/dynago/main/schema/dynago.schema.json
dynago: 1
package: tasks
table: { name: tasks }

entities:
  Task:
    doc: A to-do item in a project.
    fields:
      projectId: { type: string, example: p_1 }
      taskId: { type: string, example: t_1 }
      title: string
      done: bool
      doneAt: time
    key:
      pk: "PROJECT#{projectId}"          # a project's tasks live together…
      sk: "TASK#{taskId}"                # …sorted by task id
    counters:
      ProjectCounts:                     # kept exact by every task write
        pk: "PROJECT#{projectId}"
        sk: "COUNTS"
        values:
          open: { count: true, where: { done: false } }
          done: { count: true, where: { done: true } }
    access:                              # the only reads the store will have
      Get: get
      ListForProject: { query: key }
      Counts: { counter: ProjectCounts }
    writes:                              # the only writes
      Add: create
      Rename: { update: [title] }
      Complete: { update: [doneAt], set: { done: true }, when: { done: false } }
      Remove: delete
```

The first line gives your editor autocomplete and inline errors from the
[JSON Schema](../schema/dynago.schema.json). Every key is described in the
[schema reference](schema.md).

## 3. Check and generate

```sh
cd tasks
go tool dynago check tasks.dynago.yaml      # validate, and print costs, partitions and findings
go tool dynago generate tasks.dynago.yaml
```

```
wrote tasks_dynago.go        # the typed store
wrote tasks.model.md         # the data model document: open it
wrote tasks.tf.json          # Terraform for the table
wrote tasks.table.json       # the same, for `aws dynamodb create-table`
wrote tasks.dynago.lock      # the schema's version history
```

The generated store imports dynago's runtime, guregu/dynamo and the AWS SDK. Add them to your
module:

```sh
go mod tidy
```

`check` prints each read and write with its cost per call, and each partition with how many items
it holds. Its numbers are typical / p99 (or typical / largest), and `?` means unknown: this schema
declares no `volume` or `rate`, so nothing is estimated yet. [Analysis](guides/analysis.md) explains
how to declare them.

Open `tasks.model.md`. It shows every item the schema creates, the key of each, which writes touch
the counter, and what each call costs. This file is what a reviewer reads.

To regenerate with `go generate ./...`, add a file next to the schema:

```go
package tasks

//go:generate go tool dynago generate tasks.dynago.yaml
```

## 4. Use the store

```go
import (
    "context"
    "errors"
    "fmt"
    "time"

    "github.com/aws/aws-sdk-go-v2/config"
    "github.com/guregu/dynamo/v2"
    "github.com/nicklanng/dynago"
    "example.com/todo/tasks"
)

awsConfig, err := config.LoadDefaultConfig(ctx)   // credentials and region from the environment
st := tasks.New(dynamo.New(awsConfig), tasks.TableName("tasks"))   // the table "tasks-g1"

err = st.Tasks.Add(ctx, &tasks.Task{ProjectID: "p1", TaskID: "t1", Title: "Write docs"})

// Completing is conditional on the task not being done, and moves it from open to done
// in the same transaction.
err = st.Tasks.Complete(ctx, tasks.TaskKey{ProjectID: "p1", TaskID: "t1"}, tasks.TaskComplete{DoneAt: time.Now()})
if errors.Is(err, tasks.ErrTaskCompletePrecondition) {
    // already done
}

counts, err := st.Tasks.Counts(ctx, tasks.ProjectCountsKey{ProjectID: "p1"})
fmt.Println(counts.Open, counts.Done)

page, next, err := st.Tasks.ListForProject(ctx, tasks.TaskListForProjectQuery{ProjectID: "p1"}, dynago.Page{})
```

Every method is exactly what the schema declares. There is no "scan the table" method, because the
schema doesn't declare one.

## 5. Run DynamoDB Local and test

```sh
docker run -d --name dynamodb-local -p 8000:8000 amazon/dynamodb-local -jar DynamoDBLocal.jar -inMemory -sharedDb
```

`dynagotest` connects to it and gives each test its own table:

```go
package tasks_test

import (
    "context"
    "testing"
    "time"

    "github.com/nicklanng/dynago/dynagotest"
    "example.com/todo/tasks"
)

func TestTasks(t *testing.T) {
    ctx := context.Background()
    db := dynagotest.DB(t)                                     // skips unless DYNAGO_TEST_ENDPOINT is set
    st := tasks.New(db, dynagotest.Table(t, db, tasks.TableSpec))

    for _, id := range []string{"t1", "t2"} {
        if err := st.Tasks.Add(ctx, &tasks.Task{ProjectID: "p1", TaskID: id, Title: "Write docs"}); err != nil {
            t.Fatal(err)
        }
    }
    key := tasks.TaskKey{ProjectID: "p1", TaskID: "t1"}
    if err := st.Tasks.Complete(ctx, key, tasks.TaskComplete{DoneAt: time.Now()}); err != nil {
        t.Fatal(err)
    }
    counts, _ := st.Tasks.Counts(ctx, tasks.ProjectCountsKey{ProjectID: "p1"})
    if counts.Open != 1 || counts.Done != 1 {
        t.Fatalf("counts = %+v", counts)
    }
}
```

```sh
DYNAGO_TEST_ENDPOINT=http://localhost:8000 go test ./...
```

Without the endpoint these tests skip, so a CI job that forgot it would pass without testing
anything. Set `DYNAGO_REQUIRE_DB=1` in CI to make them fail instead.

To create the table by hand instead (for running your app locally):

```sh
aws dynamodb create-table --cli-input-json file://tasks.table.json --endpoint-url http://localhost:8000
# CreateTable can't enable TTL; if an entity has `ttl:`, turn it on separately:
aws dynamodb update-time-to-live --table-name tasks-g1 --time-to-live-specification Enabled=true,AttributeName=ttl --endpoint-url http://localhost:8000
```

or call `tasks.EnsureTable(ctx, db, tasks.TableName("tasks"))` at startup in development.

## 6. Commit everything, and check it in CI

Commit the schema **and** all generated files, including the lock file. In CI:

```sh
go tool dynago generate -check tasks/tasks.dynago.yaml
```

This fails if anything generated is out of date, so the model document in the repository always
matches the code. `dynago check` fails on the design's errors (or whatever a `dynago.policy.yaml`
says), `dynago diff -base origin/main tasks/tasks.dynago.yaml` prints a pull request's
architectural changes, and `dynago vet ./...` finds DynamoDB calls that bypass the schema. See
[Reviewing designs and changes](guides/review.md).

## 7. Change the schema

Add a field `dueAt: time` and run `generate`:

```
tasks.dynago.yaml: entity Task: its storage shape changed but its version is still 1. Set `version: 2` so stored
items record which shape wrote them. Changes: field dueAt added
```

Set `version: 2` on the entity (under `Task:`, beside `doc:`) and generate again. The lock file
records both versions.

Adding an optional field is a change existing items fit. A change they don't fit, such as a new
index, claim or counter, or a changed key, also needs a new table generation:
`table: { name: tasks, generation: 2, retain: [1] }`, with `output.migrate_cmd` set so dynago
generates the job that copies the old table into the new one. `generate` says which a change needs.
Read [Schema changes](guides/schema-changes.md) and [Migrations](guides/migrations.md) before
changing keys, indexes, claims or counters on live data.

## Next

- [Modelling](guides/modelling.md): choosing keys, GSIs vs copies, claims and counters.
- [Analysis](guides/analysis.md): declaring volumes, reading the partition estimates and findings.
- [Counters](guides/counters.md): limits, lower bounds, sharding.
- [Concurrency](guides/concurrency.md): preventing lost updates with `dynago.From` and `dynago.IfVersion`.
- [Generated code](generated-code.md): every generated name, method and error.
- [The example](../examples/toollibrary): a fictional tool library that uses most features, with its
  generated output and tests.
