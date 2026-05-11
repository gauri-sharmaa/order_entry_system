package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/millennium-oes/internal/order"
)

const (
	orderKeyPrefix      = "order:"
	brokerIndexPrefix   = "broker:"
	activeOrdersKey     = "orders:active"
	orderTTL            = 7 * 24 * time.Hour // 7 days
)

// RedisStore handles all Redis operations for the order engine.
// Redis is the hot path — all active order state lives here.
type RedisStore struct {
	client *redis.Client
}

func NewRedisStore(addr string) (*RedisStore, error) {
	client := redis.NewClient(&redis.Options{
		Addr:         addr,
		PoolSize:     50,              // connection pool
		MinIdleConns: 10,
		DialTimeout:  2 * time.Second,
		ReadTimeout:  500 * time.Millisecond,
		WriteTimeout: 500 * time.Millisecond,
		// Pipeline for batch operations
		MaxRetries: 3,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	if err := client.Ping(ctx).Err(); err != nil {
		return nil, fmt.Errorf("redis ping: %w", err)
	}

	return &RedisStore{client: client}, nil
}

func (r *RedisStore) Close() error {
	return r.client.Close()
}

// SetOrder stores an order in Redis with TTL
func (r *RedisStore) SetOrder(ctx context.Context, o *order.Order) error {
	data, err := json.Marshal(o)
	if err != nil {
		return err
	}

	pipe := r.client.Pipeline()

	// Store the order
	pipe.Set(ctx, orderKeyPrefix+o.ID, data, orderTTL)

	// Index by broker order ID for fast fill lookup
	if o.BrokerOrderID != "" {
		pipe.Set(ctx, brokerIndexPrefix+o.BrokerOrderID, o.ID, orderTTL)
	}

	// Track in active set
	if o.Status.IsCancellable() {
		pipe.SAdd(ctx, activeOrdersKey, o.ID)
	} else {
		pipe.SRem(ctx, activeOrdersKey, o.ID)
	}

	_, err = pipe.Exec(ctx)
	return err
}

// GetOrder retrieves an order by ID
func (r *RedisStore) GetOrder(ctx context.Context, orderID string) (*order.Order, error) {
	data, err := r.client.Get(ctx, orderKeyPrefix+orderID).Bytes()
	if err == redis.Nil {
		return nil, fmt.Errorf("order %s not found", orderID)
	}
	if err != nil {
		return nil, err
	}

	var o order.Order
	if err := json.Unmarshal(data, &o); err != nil {
		return nil, err
	}
	return &o, nil
}

// GetOrderByBrokerID looks up an order by broker-assigned ID
func (r *RedisStore) GetOrderByBrokerID(ctx context.Context, brokerID string) (*order.Order, error) {
	orderID, err := r.client.Get(ctx, brokerIndexPrefix+brokerID).Result()
	if err == redis.Nil {
		return nil, fmt.Errorf("no order for broker ID %s", brokerID)
	}
	if err != nil {
		return nil, err
	}
	return r.GetOrder(ctx, orderID)
}

// ListOrders returns orders matching the filter
func (r *RedisStore) ListOrders(ctx context.Context, filter order.ListFilter) ([]*order.Order, error) {
	// Get all active order IDs
	ids, err := r.client.SMembers(ctx, activeOrdersKey).Result()
	if err != nil {
		return nil, err
	}

	if len(ids) == 0 {
		return []*order.Order{}, nil
	}

	// Batch fetch using pipeline
	pipe := r.client.Pipeline()
	cmds := make([]*redis.StringCmd, len(ids))
	for i, id := range ids {
		cmds[i] = pipe.Get(ctx, orderKeyPrefix+id)
	}
	pipe.Exec(ctx)

	var orders []*order.Order
	for _, cmd := range cmds {
		data, err := cmd.Bytes()
		if err != nil {
			continue
		}
		var o order.Order
		if err := json.Unmarshal(data, &o); err != nil {
			continue
		}

		// Apply filters
		if filter.Symbol != "" && o.Symbol != filter.Symbol {
			continue
		}
		if filter.Status != "" && o.Status != filter.Status {
			continue
		}
		if filter.Side != "" && o.Side != filter.Side {
			continue
		}

		orders = append(orders, &o)

		if filter.Limit > 0 && len(orders) >= filter.Limit {
			break
		}
	}

	return orders, nil
}

// DeleteOrder removes an order from Redis
func (r *RedisStore) DeleteOrder(ctx context.Context, orderID string) error {
	pipe := r.client.Pipeline()
	pipe.Del(ctx, orderKeyPrefix+orderID)
	pipe.SRem(ctx, activeOrdersKey, orderID)
	_, err := pipe.Exec(ctx)
	return err
}
