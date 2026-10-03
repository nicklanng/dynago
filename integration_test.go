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
