package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/millennium-oes/internal/order"
)

// DynamoStore handles durable persistence of orders.
// DynamoDB writes happen asynchronously — not on the hot path.
type DynamoStore struct {
	client    *dynamodb.Client
	tableName string
}

func NewDynamoStore(region, tableName string) (*DynamoStore, error) {
	cfg, err := config.LoadDefaultConfig(context.Background(),
		config.WithRegion(region),
	)
	if err != nil {
		return nil, fmt.Errorf("load aws config: %w", err)
	}

	client := dynamodb.NewFromConfig(cfg)

	return &DynamoStore{
		client:    client,
		tableName: tableName,
	}, nil
}

// DynamoOrder is the DynamoDB item schema
type DynamoOrder struct {
	PK          string `dynamodbav:"PK"`          // "ORDER#<id>"
	SK          string `dynamodbav:"SK"`          // "ORDER#<id>"
	OrderID     string `dynamodbav:"order_id"`
	Symbol      string `dynamodbav:"symbol"`
	Status      string `dynamodbav:"status"`
	Side        string `dynamodbav:"side"`
	Type        string `dynamodbav:"type"`
	CreatedAt   string `dynamodbav:"created_at"`
	UpdatedAt   string `dynamodbav:"updated_at"`
	Payload     string `dynamodbav:"payload"`     // full JSON blob
	TTL         int64  `dynamodbav:"ttl"`         // Unix timestamp for DynamoDB TTL
}

// PutOrder writes an order to DynamoDB
func (d *DynamoStore) PutOrder(ctx context.Context, o *order.Order) error {
	payload, err := json.Marshal(o)
	if err != nil {
		return err
	}

	item := DynamoOrder{
		PK:        "ORDER#" + o.ID,
		SK:        "ORDER#" + o.ID,
		OrderID:   o.ID,
		Symbol:    o.Symbol,
		Status:    string(o.Status),
		Side:      string(o.Side),
		Type:      string(o.Type),
		CreatedAt: o.CreatedAt.Format(time.RFC3339Nano),
		UpdatedAt: o.UpdatedAt.Format(time.RFC3339Nano),
		Payload:   string(payload),
		TTL:       time.Now().Add(90 * 24 * time.Hour).Unix(), // 90 day TTL
	}

	av, err := attributevalue.MarshalMap(item)
	if err != nil {
		return err
	}

	_, err = d.client.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: aws.String(d.tableName),
		Item:      av,
	})
	return err
}

// GetOrder retrieves an order from DynamoDB by ID
func (d *DynamoStore) GetOrder(ctx context.Context, orderID string) (*order.Order, error) {
	result, err := d.client.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(d.tableName),
		Key: map[string]types.AttributeValue{
			"PK": &types.AttributeValueMemberS{Value: "ORDER#" + orderID},
			"SK": &types.AttributeValueMemberS{Value: "ORDER#" + orderID},
		},
	})
	if err != nil {
		return nil, err
	}
	if result.Item == nil {
		return nil, fmt.Errorf("order %s not found in DynamoDB", orderID)
	}

	var item DynamoOrder
	if err := attributevalue.UnmarshalMap(result.Item, &item); err != nil {
		return nil, err
	}

	var o order.Order
	if err := json.Unmarshal([]byte(item.Payload), &o); err != nil {
		return nil, err
	}
	return &o, nil
}

// QueryBySymbol returns all orders for a symbol (uses GSI)
func (d *DynamoStore) QueryBySymbol(ctx context.Context, symbol string, limit int32) ([]*order.Order, error) {
	result, err := d.client.Query(ctx, &dynamodb.QueryInput{
		TableName:              aws.String(d.tableName),
		IndexName:              aws.String("symbol-created-index"),
		KeyConditionExpression: aws.String("symbol = :sym"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":sym": &types.AttributeValueMemberS{Value: symbol},
		},
		ScanIndexForward: aws.Bool(false), // newest first
		Limit:            aws.Int32(limit),
	})
	if err != nil {
		return nil, err
	}

	orders := make([]*order.Order, 0, len(result.Items))
	for _, item := range result.Items {
		var dynItem DynamoOrder
		if err := attributevalue.UnmarshalMap(item, &dynItem); err != nil {
			continue
		}
		var o order.Order
		if err := json.Unmarshal([]byte(dynItem.Payload), &o); err != nil {
			continue
		}
		orders = append(orders, &o)
	}
	return orders, nil
}
