package e2e_test

import (
	"bytes"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/guregu/dynamo/v2"

	"github.com/nicklanng/dynago"
	"github.com/nicklanng/dynago/dynagotest"
	"github.com/nicklanng/dynago/examples/toollibrary"
	"github.com/nicklanng/dynago/internal/e2e/rekey"
)

// Generation 2 keys people by email. A person whose email changes in generation 1 during the
// migration moves in generation 2: the copy under the old email is removed, and the count holds.
func TestMigrationFollowsKeyChanges(t *testing.T) {
	db := dynagotest.DB(t)
	base := dynagotest.UniqueName(t, "rekey")
	dynagotest.TableNamed(t, db, base+"-g1", rekey.TableSpec)
	dynagotest.TableNamed(t, db, rekey.TableName(base), rekey.TableSpec)
	old := db.Table(base + "-g1")
	person := func(id, email string, rev int) {
		must(t, old.Put(map[string]any{"PK": "P#" + id, "SK": "PERSON", "_t": "Person", "_v": 1, "_rev": rev,
			"personId": id, "email": email, "name": id}).Run(ctx))
	}
	person("p1", "ann@example.org", 10)
	person("p2", "bea@example.org", 20)
	run := func() {
		t.Helper()
		m := rekey.NewMigration(db, base)
		m.Workers, m.Out = 2, &bytes.Buffer{}
		must(t, m.Run(ctx, "copy"))
	}
	st := rekey.New(db, rekey.TableName(base))
	has := func(email string) bool {
		_, err := st.Persons.Get(ctx, rekey.PersonKey{Email: email})
		return err == nil
	}
	run()
	if !has("ann@example.org") || !has("bea@example.org") {
		t.Fatal("people not copied")
	}
	person("p1", "ann@new.example.org", 11) // ann changes her email in generation 1
	run()
	if has("ann@example.org") || !has("ann@new.example.org") {
		t.Fatalf("after the change: old email copied %t, new email copied %t", has("ann@example.org"), has("ann@new.example.org"))
	}
	if c, _ := st.Persons.Count(ctx, rekey.PeopleKey{}); c.All != 2 {
		t.Fatalf("people counted %d times, want 2", c.All)
	}
}

// A job that loses its lease (here, stolen outright; in life, a paused or partitioned pod) stops,
// and writes nothing more: every write it makes is fenced by the lease.
func TestLostLeaseStopsTheJob(t *testing.T) {
	g := setupGenerations(t)
	for i := 0; i < 40; i++ {
		must(t, g.old.Tools.Add(ctx, &toollibrary.Tool{LibraryID: "lib1", ToolID: fmt.Sprintf("t%02d", i), Name: "Tool", Category: toollibrary.ToolCategoryHand,
			Status: toollibrary.ToolStatusAvailable}))
	}
	m := toollibrary.NewMigration(g.db, g.base)
	m.Workers, m.Rate, m.Out = 1, 10, &bytes.Buffer{} // slow: about 4 seconds for 40 tools
	done := make(chan error, 1)
	go func() { done <- m.Run(ctx, "copy") }()
	time.Sleep(time.Second)
	newTable := g.db.Table(toollibrary.TableName(g.base))
	must(t, newTable.Update("PK", "_DYNAGO#MIGRATION#"+g.base+"-g1").Range("SK", "STATE").Set("leaseOwner", "another job").Run(ctx))
	stolenAt := count(t, newTable)
	err := <-done
	if !errors.Is(err, dynago.ErrLeaseLost) {
		t.Fatalf("got %v, want ErrLeaseLost", err)
	}
	if n := count(t, newTable); n != stolenAt {
		t.Fatalf("%d items written after the lease was lost", n-stolenAt)
	}
	if st := dynagotest.RawItem(t, newTable, "_DYNAGO#MIGRATION#"+g.base+"-g1", "STATE"); st["leaseOwner"] != "another job" {
		t.Fatalf("the stale job changed the lease: %v", st)
	}
}

// count returns how many items a table holds.
func count(t *testing.T, table dynamo.Table) int {
	t.Helper()
	n, err := table.Scan().Consistent(true).Count(ctx)
	must(t, err)
	return n
}
