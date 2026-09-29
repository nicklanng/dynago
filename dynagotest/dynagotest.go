// Package dynagotest connects tests to DynamoDB Local and gives each test its own table.
//
// Set DYNAGO_TEST_ENDPOINT (for example http://127.0.0.1:8000) to run tests that need DynamoDB;
// without it they are skipped, unless DYNAGO_REQUIRE_DB is set, in which case they fail.
package dynagotest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/smithy-go/logging"
	"github.com/guregu/dynamo/v2"

	"github.com/nicklanng/dynago"
)

// EnvEndpoint names the environment variable holding the DynamoDB Local endpoint.
const EnvEndpoint = "DYNAGO_TEST_ENDPOINT"

// EnvRequire names an environment variable that, when set, makes tests needing DynamoDB fail
// instead of skipping without an endpoint. Set it in CI, so a missing endpoint can't pass as green.
const EnvRequire = "DYNAGO_REQUIRE_DB"

// DB returns a client for DynamoDB Local, skipping the test if no endpoint is configured.
func DB(t testing.TB) *dynamo.DB {
	t.Helper()
	return dynamo.NewFromIface(Client(t))
}

// Requests counts the requests a client has made, by kind.
type Requests struct {
	GetItem, BatchGetItem, Query, TransactGetItems      atomic.Int64
	PutItem, UpdateItem, DeleteItem, TransactWriteItems atomic.Int64
}

// Reads returns the number of read requests: GetItem, BatchGetItem, Query and TransactGetItems.
func (r *Requests) Reads() int64 {
	return r.GetItem.Load() + r.BatchGetItem.Load() + r.Query.Load() + r.TransactGetItems.Load()
}

// Writes returns the number of write requests: PutItem, UpdateItem, DeleteItem and
// TransactWriteItems (one per transaction, however many items it holds).
func (r *Requests) Writes() int64 {
	return r.PutItem.Load() + r.UpdateItem.Load() + r.DeleteItem.Load() + r.TransactWriteItems.Load()
}

type countingClient struct {
	*dynamodb.Client
	n *Requests
}

func (c countingClient) GetItem(ctx context.Context, in *dynamodb.GetItemInput, opts ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error) {
	c.n.GetItem.Add(1)
	return c.Client.GetItem(ctx, in, opts...)
}

func (c countingClient) BatchGetItem(ctx context.Context, in *dynamodb.BatchGetItemInput, opts ...func(*dynamodb.Options)) (*dynamodb.BatchGetItemOutput, error) {
	c.n.BatchGetItem.Add(1)
	return c.Client.BatchGetItem(ctx, in, opts...)
}

func (c countingClient) Query(ctx context.Context, in *dynamodb.QueryInput, opts ...func(*dynamodb.Options)) (*dynamodb.QueryOutput, error) {
	c.n.Query.Add(1)
	return c.Client.Query(ctx, in, opts...)
}

func (c countingClient) TransactGetItems(ctx context.Context, in *dynamodb.TransactGetItemsInput, opts ...func(*dynamodb.Options)) (*dynamodb.TransactGetItemsOutput, error) {
	c.n.TransactGetItems.Add(1)
	return c.Client.TransactGetItems(ctx, in, opts...)
}

func (c countingClient) PutItem(ctx context.Context, in *dynamodb.PutItemInput, opts ...func(*dynamodb.Options)) (*dynamodb.PutItemOutput, error) {
	c.n.PutItem.Add(1)
	return c.Client.PutItem(ctx, in, opts...)
}

func (c countingClient) UpdateItem(ctx context.Context, in *dynamodb.UpdateItemInput, opts ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error) {
	c.n.UpdateItem.Add(1)
	return c.Client.UpdateItem(ctx, in, opts...)
}

func (c countingClient) DeleteItem(ctx context.Context, in *dynamodb.DeleteItemInput, opts ...func(*dynamodb.Options)) (*dynamodb.DeleteItemOutput, error) {
	c.n.DeleteItem.Add(1)
	return c.Client.DeleteItem(ctx, in, opts...)
}

func (c countingClient) TransactWriteItems(ctx context.Context, in *dynamodb.TransactWriteItemsInput, opts ...func(*dynamodb.Options)) (*dynamodb.TransactWriteItemsOutput, error) {
	c.n.TransactWriteItems.Add(1)
	return c.Client.TransactWriteItems(ctx, in, opts...)
}

// CountingDB is DB, plus counts of the requests made through it by kind, for tests that assert
// what an operation costs: how many reads, and whether it was one write or a transaction.
func CountingDB(t testing.TB) (*dynamo.DB, *Requests) {
	t.Helper()
	n := &Requests{}
	return dynamo.NewFromIface(countingClient{Client: Client(t), n: n}), n
}

// Client returns a raw DynamoDB client for DynamoDB Local, skipping the test if no endpoint is
// configured.
func Client(t testing.TB) *dynamodb.Client {
	t.Helper()
	endpoint := os.Getenv(EnvEndpoint)
	if endpoint == "" {
		if os.Getenv(EnvRequire) != "" {
			t.Fatalf("%s is set but %s is not: this run must test against DynamoDB Local", EnvRequire, EnvEndpoint)
		}
		t.Skipf("%s is not set; skipping test that needs DynamoDB Local", EnvEndpoint)
	}
	cfg, err := config.LoadDefaultConfig(context.Background(),
		config.WithRegion("us-east-1"),
		config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider("local", "local", "")),
		// DynamoDB Local's error responses make the SDK warn about unread bodies; that is noise here.
		config.WithLogger(logging.Nop{}),
	)
	if err != nil {
		t.Fatal(err)
	}
	return dynamodb.NewFromConfig(cfg, func(o *dynamodb.Options) { o.BaseEndpoint = aws.String(endpoint) })
}

// Table creates a uniquely named table with spec for the duration of the test and returns its name.
func Table(t testing.TB, db *dynamo.DB, spec dynago.TableSpec) string {
	t.Helper()
	name := UniqueName(t, "dynagotest")
	TableNamed(t, db, name, spec)
	return name
}

// UniqueName returns prefix followed by a random suffix, for naming tables in tests: a migration
// test names its tables base-g1 and base-g2 after one base.
func UniqueName(t testing.TB, prefix string) string {
	t.Helper()
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return prefix + "-" + hex.EncodeToString(b)
}

// TableNamed creates a table called name with spec for the duration of the test.
func TableNamed(t testing.TB, db *dynamo.DB, name string, spec dynago.TableSpec) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := dynago.EnsureTable(ctx, db, name, spec); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = db.Table(name).DeleteTable().Run(context.Background())
	})
}

// RawItem reads an item as stored, as plain Go values, or nil if it is absent.
func RawItem(t testing.TB, table dynamo.Table, pk, sk string) map[string]any {
	t.Helper()
	var item map[string]any
	err := table.Get(dynago.AttrPK, pk).Range(dynago.AttrSK, dynamo.Equal, sk).Consistent(true).One(context.Background(), &item)
	if errors.Is(err, dynamo.ErrNotFound) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return item
}
