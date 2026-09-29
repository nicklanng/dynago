package dynago_test

import (
	"errors"
	"testing"

	"github.com/nicklanng/dynago"
	"github.com/nicklanng/dynago/dynagotest"
)

type item struct {
	PK  string `dynamo:"PK"`
	SK  string `dynamo:"SK"`
	Rev int64  `dynamo:"_rev"`
}

// If the SDK retries a create whose first attempt succeeded, the retry must report success, not
// "already exists": the random revision tells our own item apart from someone else's.
func TestCreateSurvivesRetryOfItself(t *testing.T) {
	db := dynagotest.DB(t)
	tbl := db.Table(dynagotest.Table(t, db, dynago.TableSpec{}))
	exists := errors.New("exists")
	create := func(rev int64) error {
		put := tbl.Put(item{PK: "A", SK: "A", Rev: rev}).If("attribute_not_exists($)", "PK")
		return dynago.Run(ctx, db, []dynago.Op{dynago.CreateOp(dynago.Key{PK: "A", SK: "A"}, put, exists, rev)})
	}
	rev := dynago.NewRev()
	if err := create(rev); err != nil {
		t.Fatal(err)
	}
	if err := create(rev); err != nil {
		t.Fatalf("repeat of our own create: %v", err)
	}
	if err := create(dynago.NewRev()); !errors.Is(err, exists) {
		t.Fatalf("someone else's create: %v", err)
	}
}
