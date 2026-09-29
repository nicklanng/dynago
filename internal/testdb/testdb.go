// Package testdb gives dynago's own tests a DynamoDB: DynamoDB Local by default (through
// dynagotest), or real tables in an AWS account for the maintainer's release checks.
//
// Set DYNAGO_TEST_AWS to the id of the AWS account to use. The tests then use the default AWS
// credential chain (AWS_PROFILE, AWS_REGION, …), refuse to run if the credentials belong to another
// account, and create on-demand tables named dynagotest-…, tagged with their creation time, which
// each test deletes when it ends. `go run ./internal/testdb/sweep` deletes any left behind.
//
// This is not part of dynago's API: users test their stores with dynagotest and DynamoDB Local.
package testdb

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/guregu/dynamo/v2"

	"github.com/nicklanng/dynago"
	"github.com/nicklanng/dynago/dynagotest"
)

// EnvAWS names the environment variable holding the AWS account id to test in.
const EnvAWS = "DYNAGO_TEST_AWS"

// Prefix starts every table the tests create in AWS; the sweeper only deletes tables with it and
// the created tag.
const Prefix = "dynagotest-"

// TagCreated is the tag holding a test table's creation time (RFC 3339).
const TagCreated = "dynagotest-created"

// AWS reports whether the tests run against real AWS.
func AWS() bool { return os.Getenv(EnvAWS) != "" }

// Requests counts requests by kind; see dynagotest.Requests.
type Requests = dynagotest.Requests

var (
	awsOnce   sync.Once
	awsClient *dynamodb.Client
	awsErr    error
)

// Client returns a DynamoDB client: for the configured AWS account, or DynamoDB Local.
func Client(t testing.TB) *dynamodb.Client {
	t.Helper()
	if !AWS() {
		return dynagotest.Client(t)
	}
	awsOnce.Do(func() { awsClient, awsErr = connect(context.Background(), os.Getenv(EnvAWS)) })
	if awsErr != nil {
		t.Fatal(awsErr)
	}
	return awsClient
}

// connect loads the default AWS configuration and checks it is for the expected account.
func connect(ctx context.Context, account string) (*dynamodb.Client, error) {
	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("%s: loading AWS configuration: %w", EnvAWS, err)
	}
	if cfg.Region == "" {
		return nil, fmt.Errorf("%s: no AWS region configured (set AWS_REGION)", EnvAWS)
	}
	id, err := sts.NewFromConfig(cfg).GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if err != nil {
		return nil, fmt.Errorf("%s: checking the AWS account: %w", EnvAWS, err)
	}
	if got := aws.ToString(id.Account); got != account {
		return nil, fmt.Errorf("%s is %s, but the AWS credentials are for account %s: refusing to create tables there", EnvAWS, account, got)
	}
	return dynamodb.NewFromConfig(cfg), nil
}

// DB returns a client for the test's DynamoDB.
func DB(t testing.TB) *dynamo.DB {
	t.Helper()
	if !AWS() {
		return dynagotest.DB(t)
	}
	return dynamo.NewFromIface(Client(t))
}

// CountingDB is DB, plus counts of the requests made through it.
func CountingDB(t testing.TB) (*dynamo.DB, *Requests) {
	t.Helper()
	if !AWS() {
		return dynagotest.CountingDB(t)
	}
	n := &Requests{}
	return dynamo.NewFromIface(counting{Client: Client(t), n: n}), n
}

// UniqueName returns prefix followed by a random suffix. In AWS, names start with Prefix.
func UniqueName(t testing.TB, prefix string) string {
	t.Helper()
	if !AWS() {
		return dynagotest.UniqueName(t, prefix)
	}
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return Prefix + prefix + "-" + hex.EncodeToString(b)
}

// Table creates a uniquely named table with spec for the duration of the test.
func Table(t testing.TB, db *dynamo.DB, spec dynago.TableSpec) string {
	t.Helper()
	name := UniqueName(t, "t")
	TableNamed(t, db, name, spec)
	return name
}

// TableNamed creates a table called name with spec for the duration of the test. In AWS the name
// must start with Prefix.
func TableNamed(t testing.TB, db *dynamo.DB, name string, spec dynago.TableSpec) {
	t.Helper()
	if !AWS() {
		dynagotest.TableNamed(t, db, name, spec)
		return
	}
	if len(name) < len(Prefix) || name[:len(Prefix)] != Prefix {
		t.Fatalf("test table %q doesn't start with %q, so the sweeper couldn't clean it up", name, Prefix)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	in := spec.CreateTableInput(name)
	in.Tags = []types.Tag{{Key: aws.String(TagCreated), Value: aws.String(time.Now().UTC().Format(time.RFC3339))}}
	if _, err := db.Client().CreateTable(ctx, in); err != nil {
		t.Fatalf("create table %s: %v", name, err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if _, err := db.Client().DeleteTable(ctx, &dynamodb.DeleteTableInput{TableName: aws.String(name)}); err != nil {
			t.Errorf("delete table %s (the sweeper will): %v", name, err)
		}
	})
	// EnsureTable waits for the table, and its indexes, and enables TTL.
	if err := dynago.EnsureTable(ctx, db, name, spec); err != nil {
		t.Fatal(err)
	}
}

// RawItem reads an item as stored, or nil if it is absent.
func RawItem(t testing.TB, table dynamo.Table, pk, sk string) map[string]any {
	t.Helper()
	return dynagotest.RawItem(t, table, pk, sk)
}

// Eventually retries check until it passes or a deadline passes: for reads through a GSI, which
// DynamoDB updates a moment after each write. With DynamoDB Local, whose GSIs are up to date at
// once, the first try passes.
func Eventually(t testing.TB, what string, check func() error) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		err := check()
		if err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: still failing after 15s: %v", what, err)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// errNot is a convenience for Eventually checks.
var errNot = errors.New("not yet")

// Check turns a condition into an Eventually check.
func Check(ok bool, format string, args ...any) error {
	if ok {
		return nil
	}
	return fmt.Errorf("%w: "+format, append([]any{errNot}, args...)...)
}
