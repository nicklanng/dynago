// Command sweep deletes the tables dynago's AWS test runs left behind: tables named dynagotest-…
// and tagged with a creation time more than an hour ago (-age changes it). It checks the
// credentials are for the account in DYNAGO_TEST_AWS first, and with -n only lists what it would
// delete.
//
//	DYNAGO_TEST_AWS=123456789012 go run ./internal/testdb/sweep [-n] [-age 1h]
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/sts"

	"github.com/nicklanng/dynago/internal/testdb"
)

func main() {
	dry := flag.Bool("n", false, "list the tables it would delete, without deleting them")
	age := flag.Duration("age", time.Hour, "delete test tables created longer ago than this")
	flag.Parse()
	if err := sweep(context.Background(), *dry, *age); err != nil {
		fmt.Fprintln(os.Stderr, "sweep:", err)
		os.Exit(1)
	}
}

func sweep(ctx context.Context, dry bool, age time.Duration) error {
	account := os.Getenv(testdb.EnvAWS)
	if account == "" {
		return fmt.Errorf("set %s to the test account's id", testdb.EnvAWS)
	}
	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return err
	}
	id, err := sts.NewFromConfig(cfg).GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if err != nil {
		return err
	}
	if got := aws.ToString(id.Account); got != account {
		return fmt.Errorf("%s is %s, but the credentials are for account %s", testdb.EnvAWS, account, got)
	}
	client := dynamodb.NewFromConfig(cfg)
	cutoff := time.Now().Add(-age)
	pages := dynamodb.NewListTablesPaginator(client, &dynamodb.ListTablesInput{ExclusiveStartTableName: aws.String(testdb.Prefix)})
	for pages.HasMorePages() {
		page, err := pages.NextPage(ctx)
		if err != nil {
			return err
		}
		for _, name := range page.TableNames {
			if !strings.HasPrefix(name, testdb.Prefix) {
				return nil // tables are listed in name order: past the prefix, done
			}
			created, err := createdAt(ctx, client, name)
			if err != nil {
				return err
			}
			if created.IsZero() || created.After(cutoff) {
				continue // not ours, or still in use
			}
			fmt.Printf("%s (created %s)\n", name, created.Format(time.RFC3339))
			if dry {
				continue
			}
			if _, err := client.DeleteTable(ctx, &dynamodb.DeleteTableInput{TableName: aws.String(name)}); err != nil {
				return fmt.Errorf("delete %s: %w", name, err)
			}
		}
	}
	return nil
}

// createdAt reads a test table's creation tag, or returns zero if it has none.
func createdAt(ctx context.Context, client *dynamodb.Client, name string) (time.Time, error) {
	desc, err := client.DescribeTable(ctx, &dynamodb.DescribeTableInput{TableName: aws.String(name)})
	if err != nil {
		return time.Time{}, err
	}
	tags, err := client.ListTagsOfResource(ctx, &dynamodb.ListTagsOfResourceInput{ResourceArn: desc.Table.TableArn})
	if err != nil {
		return time.Time{}, err
	}
	for _, tag := range tags.Tags {
		if aws.ToString(tag.Key) == testdb.TagCreated {
			return time.Parse(time.RFC3339, aws.ToString(tag.Value))
		}
	}
	return time.Time{}, nil
}
