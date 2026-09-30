package app

import (
	"context"

	"github.com/aws/aws-sdk-go-v2/aws"
	ddb "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/guregu/dynamo/v2"
)

// retry stands in for a helper that takes the request to make as a function value.
func retry(fns ...any) error { return nil }

func use(...any) {}

// wrapped embeds the client, so its requests are its methods.
type wrapped struct{ *ddb.Client }

// Values hands requests to a helper instead of calling them: each still reaches DynamoDB.
func Values(ctx context.Context, c *ddb.Client, w wrapped) {
	_ = retry(c.PutItem)           // a method value, through an aliased import
	_ = retry(w.GetItem)           // through an embedded client
	_ = retry((*ddb.Client).Query) // a method expression
	_ = retry(ddb.NewQueryPaginator)
	_ = retry(ddb.NewFromConfig, c.Options) // creating a client and reading its options make no request
}

// MarkedValues marks function values like calls.
func MarkedValues(ctx context.Context, c *ddb.Client) {
	//dynago:raw retried by the helper
	_ = retry(c.DeleteItem)
	_ = retry(ctx,
		c.TransactWriteItems,
	) //dynago:raw the statement's last line
}

// Layouts marks calls spread over lines.
func Layouts(ctx context.Context, c *ddb.Client, db *dynamo.DB) {
	_, _ = c.UpdateItem(ctx, &ddb.UpdateItemInput{
		TableName: aws.String("x"),
	}) //dynago:raw after the closing brace

	//dynago:raw a one-off repair
	// The explanation continues on the next line.
	_, _ = c.BatchGetItem(ctx, &ddb.BatchGetItemInput{})

	use(db.Table("x").
		Get("PK", "a").
		Iter(), //dynago:raw the admin console pages a partition
		0)

	if _, err := c.DescribeTable(ctx, &ddb.DescribeTableInput{}); err != nil {
		return
	} //dynago:raw a mark on a block's closing brace is not about the condition
}
