package dynago

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/guregu/dynamo/v2"
)

// TableSpec is the physical shape of a table: its GSIs and TTL attribute. The base key is always
// PK (hash) and SK (range), both strings. Billing is on-demand.
type TableSpec struct {
	TTLAttr string    `json:"ttl_attribute,omitempty"`
	GSIs    []GSISpec `json:"gsis,omitempty"`
}

// GSISpec is one global secondary index.
type GSISpec struct {
	Name   string `json:"name"`
	PKAttr string `json:"pk"`
	// SKAttr is "" for an index without a sort key.
	SKAttr string `json:"sk,omitempty"`
	// Projection is "ALL" or "INCLUDE".
	Projection  string   `json:"projection"`
	NonKeyAttrs []string `json:"non_key_attributes,omitempty"`
}

// CreateTableInput builds the CreateTable request for a table named name.
func (s TableSpec) CreateTableInput(name string) *dynamodb.CreateTableInput {
	attrs := []types.AttributeDefinition{
		{AttributeName: aws.String(AttrPK), AttributeType: types.ScalarAttributeTypeS},
		{AttributeName: aws.String(AttrSK), AttributeType: types.ScalarAttributeTypeS},
	}
	in := &dynamodb.CreateTableInput{
		TableName:   aws.String(name),
		BillingMode: types.BillingModePayPerRequest,
		KeySchema: []types.KeySchemaElement{
			{AttributeName: aws.String(AttrPK), KeyType: types.KeyTypeHash},
			{AttributeName: aws.String(AttrSK), KeyType: types.KeyTypeRange},
		},
	}
	for _, g := range s.GSIs {
		attrs = append(attrs, types.AttributeDefinition{AttributeName: aws.String(g.PKAttr), AttributeType: types.ScalarAttributeTypeS})
		ks := []types.KeySchemaElement{{AttributeName: aws.String(g.PKAttr), KeyType: types.KeyTypeHash}}
		if g.SKAttr != "" {
			attrs = append(attrs, types.AttributeDefinition{AttributeName: aws.String(g.SKAttr), AttributeType: types.ScalarAttributeTypeS})
			ks = append(ks, types.KeySchemaElement{AttributeName: aws.String(g.SKAttr), KeyType: types.KeyTypeRange})
		}
		proj := &types.Projection{ProjectionType: types.ProjectionType(g.Projection)}
		if g.Projection == string(types.ProjectionTypeInclude) {
			proj.NonKeyAttributes = g.NonKeyAttrs
		}
		in.GlobalSecondaryIndexes = append(in.GlobalSecondaryIndexes, types.GlobalSecondaryIndex{
			IndexName: aws.String(g.Name), KeySchema: ks, Projection: proj,
		})
	}
	in.AttributeDefinitions = attrs
	return in
}

// EnsureTable creates the table if it does not exist, waits for it to become active and enables
// TTL. It is meant for local development and tests; production tables belong in infrastructure
// code (dynago generates Terraform for them).
func EnsureTable(ctx context.Context, db *dynamo.DB, name string, spec TableSpec) error {
	client := db.Client()
	_, err := client.CreateTable(ctx, spec.CreateTableInput(name))
	var inUse *types.ResourceInUseException
	if err != nil && !errors.As(err, &inUse) {
		return fmt.Errorf("dynago: create table %s: %w", name, err)
	}
	if err := db.Table(name).Wait(ctx); err != nil {
		return fmt.Errorf("dynago: wait for table %s: %w", name, err)
	}
	if spec.TTLAttr == "" {
		return nil
	}
	_, err = client.UpdateTimeToLive(ctx, &dynamodb.UpdateTimeToLiveInput{
		TableName: aws.String(name),
		TimeToLiveSpecification: &types.TimeToLiveSpecification{
			AttributeName: aws.String(spec.TTLAttr), Enabled: aws.Bool(true),
		},
	})
	if err != nil && !isTTLAlreadyEnabled(err) {
		return fmt.Errorf("dynago: enable ttl on %s: %w", name, err)
	}
	return nil
}

func isTTLAlreadyEnabled(err error) bool {
	var ve interface{ ErrorMessage() string }
	return errors.As(err, &ve) && strings.Contains(ve.ErrorMessage(), "already enabled")
}
