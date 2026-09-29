// Package app calls DynamoDB in every way dynago vet must see.
package app

import (
	"context"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/guregu/dynamo/v2"

	"github.com/nicklanng/dynago"
)

// Connect builds the client the generated store takes: allowed.
func Connect(cfg aws.Config) *dynamo.DB { return dynamo.New(cfg) }

// Raw writes behind the schema's back.
func Raw(ctx context.Context, db *dynamo.DB) error {
	return db.Table("x").Put(map[string]string{"PK": "a"}).Run(ctx)
}

// SDK reads through the AWS client.
func SDK(ctx context.Context, c *dynamodb.Client) {
	_, _ = c.Scan(ctx, &dynamodb.ScanInput{})
	_ = dynamodb.NewScanPaginator(c, &dynamodb.ScanInput{})
	_ = c.Options() // not a request
}

// Export is allowed as a whole.
//
//dynago:raw the nightly export reads everything
func Export(ctx context.Context, db *dynamo.DB) error {
	var out []map[string]any
	return db.Table("x").Scan().All(ctx, &out)
}

// Marked marks one statement.
func Marked(ctx context.Context, db *dynamo.DB) error {
	//dynago:raw seeding a fixture
	return db.Table("x").Delete("PK", "a").Run(ctx)
}

// NoReason marks a call without saying why.
func NoReason(db *dynamo.DB) {
	_ = db.Table("x") //dynago:raw
}

// Runtime calls dynago's runtime directly.
func Runtime(ctx context.Context, t dynamo.Table) {
	_, _ = dynago.GetOne(ctx, t, dynago.Key{}, true, nil)
	_ = dynago.Max(3) // not a request
}

// Chained marks a call chain that spans lines, from the line above its start.
func Chained(ctx context.Context, db *dynamo.DB) error {
	var out []map[string]any
	//dynago:raw the admin console lists a partition
	return db.Table("x").
		Get("PK", "a").
		All(ctx, &out)
}

// Trailing marks one call at the end of its line; the next line's call isn't marked.
func Trailing(ctx context.Context, c *dynamodb.Client) {
	_, _ = c.GetItem(ctx, &dynamodb.GetItemInput{}) //dynago:raw a health check
	_, _ = c.PutItem(ctx, &dynamodb.PutItemInput{})
}

// Items is an interface over the client, as code often wraps it for tests.
type Items interface {
	DeleteItem(ctx context.Context, in *dynamodb.DeleteItemInput, opts ...func(*dynamodb.Options)) (*dynamodb.DeleteItemOutput, error)
}

// Wrapped calls DynamoDB through the interface.
func Wrapped(ctx context.Context, items Items) {
	_, _ = items.DeleteItem(ctx, &dynamodb.DeleteItemInput{})
}
