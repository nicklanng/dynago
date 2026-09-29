package testdb

import (
	"context"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
)

// counting counts requests by kind, like dynagotest.CountingDB, for tests in AWS.
type counting struct {
	*dynamodb.Client
	n *Requests
}

func (c counting) GetItem(ctx context.Context, in *dynamodb.GetItemInput, opts ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error) {
	c.n.GetItem.Add(1)
	return c.Client.GetItem(ctx, in, opts...)
}

func (c counting) BatchGetItem(ctx context.Context, in *dynamodb.BatchGetItemInput, opts ...func(*dynamodb.Options)) (*dynamodb.BatchGetItemOutput, error) {
	c.n.BatchGetItem.Add(1)
	return c.Client.BatchGetItem(ctx, in, opts...)
}

func (c counting) Query(ctx context.Context, in *dynamodb.QueryInput, opts ...func(*dynamodb.Options)) (*dynamodb.QueryOutput, error) {
	c.n.Query.Add(1)
	return c.Client.Query(ctx, in, opts...)
}

func (c counting) TransactGetItems(ctx context.Context, in *dynamodb.TransactGetItemsInput, opts ...func(*dynamodb.Options)) (*dynamodb.TransactGetItemsOutput, error) {
	c.n.TransactGetItems.Add(1)
	return c.Client.TransactGetItems(ctx, in, opts...)
}

func (c counting) PutItem(ctx context.Context, in *dynamodb.PutItemInput, opts ...func(*dynamodb.Options)) (*dynamodb.PutItemOutput, error) {
	c.n.PutItem.Add(1)
	return c.Client.PutItem(ctx, in, opts...)
}

func (c counting) UpdateItem(ctx context.Context, in *dynamodb.UpdateItemInput, opts ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error) {
	c.n.UpdateItem.Add(1)
	return c.Client.UpdateItem(ctx, in, opts...)
}

func (c counting) DeleteItem(ctx context.Context, in *dynamodb.DeleteItemInput, opts ...func(*dynamodb.Options)) (*dynamodb.DeleteItemOutput, error) {
	c.n.DeleteItem.Add(1)
	return c.Client.DeleteItem(ctx, in, opts...)
}

func (c counting) TransactWriteItems(ctx context.Context, in *dynamodb.TransactWriteItemsInput, opts ...func(*dynamodb.Options)) (*dynamodb.TransactWriteItemsOutput, error) {
	c.n.TransactWriteItems.Add(1)
	return c.Client.TransactWriteItems(ctx, in, opts...)
}
