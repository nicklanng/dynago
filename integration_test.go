package dynago_test

import (
	"context"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/guregu/dynamo/v2"
	"github.com/nicklanng/dynago"
	"github.com/nicklanng/dynago/internal/testdb"
)

type item struct {
	PK  string `dynamo:"PK"`
	SK  string `dynamo:"SK"`
	Rev int64  `dynamo:"_rev"`
}

// If the SDK retries a create whose first attempt succeeded, the retry must report success, not
// "already exists": the random revision tells our own item apart from someone else's.
func TestCreateSurvivesRetryOfItself(t *testing.T) {
	db := testdb.DB(t)
	tbl := db.Table(testdb.Table(t, db, dynago.TableSpec{}))
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

// changes counts the requests EnsureTable makes that change a table.
type changes struct {
	*dynamodb.Client
	creates, ttls int
}

func (c *changes) CreateTable(ctx context.Context, in *dynamodb.CreateTableInput, opts ...func(*dynamodb.Options)) (*dynamodb.CreateTableOutput, error) {
	c.creates++
	return c.Client.CreateTable(ctx, in, opts...)
}

func (c *changes) UpdateTimeToLive(ctx context.Context, in *dynamodb.UpdateTimeToLiveInput, opts ...func(*dynamodb.Options)) (*dynamodb.UpdateTimeToLiveOutput, error) {
	c.ttls++
	return c.Client.UpdateTimeToLive(ctx, in, opts...)
}

// EnsureTable at every start must not make requests that fail once the table exists: against
// DynamoDB Local each failed one makes the SDK log a warning.
func TestEnsureTableLeavesAnExistingTableAlone(t *testing.T) {
	spec := dynago.TableSpec{TTLAttr: "ttl", GSIs: []dynago.GSISpec{{Name: "Other", PKAttr: "GPK", Projection: "ALL"}}}
	name := testdb.Table(t, testdb.DB(t), spec)
	c := &changes{Client: testdb.Client(t)}
	for range 2 {
		if err := dynago.EnsureTable(ctx, dynamo.NewFromIface(c), name, spec); err != nil {
			t.Fatal(err)
		}
	}
	if c.creates != 0 || c.ttls != 0 {
		t.Errorf("an existing table with TTL on: %d CreateTable and %d UpdateTimeToLive requests, want none", c.creates, c.ttls)
	}
}

// Conditions compare with what is stored, and a field's zero value is stored as no attribute at
// all: "not x" must hold for an absent attribute, "not the zero value" must fail for one, and a
// list that includes the zero value must admit one.
func TestConditionsNotAndIn(t *testing.T) {
	db := testdb.DB(t)
	tbl := db.Table(testdb.Table(t, db, dynago.TableSpec{}))
	type row struct {
		PK    string `dynamo:"PK"`
		SK    string `dynamo:"SK"`
		Rev   int64  `dynamo:"_rev"`
		State string `dynamo:"state,omitempty"`
	}
	set, unset := dynago.Key{PK: "A", SK: "set"}, dynago.Key{PK: "A", SK: "unset"}
	if err := tbl.Put(row{PK: set.PK, SK: set.SK, Rev: 1, State: "open"}).Run(ctx); err != nil {
		t.Fatal(err)
	}
	if err := tbl.Put(row{PK: unset.PK, SK: unset.SK, Rev: 1}).Run(ctx); err != nil {
		t.Fatal(err)
	}
	refused := errors.New("refused")
	holds := func(k dynago.Key, c dynago.Cond) bool {
		t.Helper()
		_, err := dynago.UpdateFields(ctx, tbl, k, []dynago.Set{{Attr: "touched", Value: true}}, []dynago.Cond{c}, dynago.Guard{}, refused)
		if err != nil && !errors.Is(err, refused) {
			t.Fatal(err)
		}
		return err == nil
	}
	for _, c := range []struct {
		name            string
		cond            dynago.Cond
		onSet, onAbsent bool
	}{
		{"not another value", dynago.Cond{Attr: "state", Value: "closed", Not: true}, true, true},
		{"not its value", dynago.Cond{Attr: "state", Value: "open", Not: true}, false, true},
		{"not the zero value", dynago.Cond{Attr: "state", Value: "", Zero: true, Not: true}, true, false},
		{"in a list holding its value", dynago.Cond{Attr: "state", In: []any{"open", "held"}}, true, false},
		{"in a list without it", dynago.Cond{Attr: "state", In: []any{"closed", "held"}}, false, false},
		{"in a list with the zero value", dynago.Cond{Attr: "state", In: []any{"", "closed"}, Zero: true}, false, true},
	} {
		if got := holds(set, c.cond); got != c.onSet {
			t.Errorf("%s, on an item whose state is open: %v, want %v", c.name, got, c.onSet)
		}
		if got := holds(unset, c.cond); got != c.onAbsent {
			t.Errorf("%s, on an item with no state: %v, want %v", c.name, got, c.onAbsent)
		}
	}
	// A requirement renders the same conditions, alongside its own.
	check := func(c dynago.Cond) error {
		req := dynago.Requirement{Key: set, When: []dynago.Cond{c, {Attr: "state", Value: "open"}}}
		return dynago.Run(ctx, db, []dynago.Op{
			dynago.CheckOp(set, dynago.CheckRequirement(tbl, req), refused),
			dynago.PutOp(dynago.Key{PK: "A", SK: "other"}, tbl.Put(row{PK: "A", SK: "other", Rev: 1}), nil),
		})
	}
	if err := check(dynago.Cond{Attr: "state", In: []any{"open", "held"}}); err != nil {
		t.Errorf("a requirement with a list that holds: %v", err)
	}
	if err := check(dynago.Cond{Attr: "state", Value: "open", Not: true}); !errors.Is(err, refused) {
		t.Errorf("a requirement with a not that fails: %v", err)
	}
}
