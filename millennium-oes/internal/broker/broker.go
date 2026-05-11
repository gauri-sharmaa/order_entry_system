// Package broker defines the unified broker interface used by the API layer.
// Both the Alpaca client and the FIX client implement this interface,
// making them interchangeable via config.
package broker

import (
	"context"

	"github.com/millennium-oes/internal/order"
)

// Position is a broker-agnostic position struct
type Position struct {
	Symbol        string  `json:"symbol"`
	Qty           float64 `json:"qty,string"`
	AvgEntryPrice float64 `json:"avg_entry_price,string"`
	MarketValue   float64 `json:"market_value,string"`
	UnrealizedPnL float64 `json:"unrealized_pl,string"`
	CurrentPrice  float64 `json:"current_price,string"`
	Side          string  `json:"side"`
}

// Account is a broker-agnostic account summary
type Account struct {
	ID          string  `json:"id"`
	Equity      float64 `json:"equity"`
	Cash        float64 `json:"cash"`
	BuyingPower float64 `json:"buying_power"`
	Status      string  `json:"status"`
}

// Client is the full broker interface used by the API layer.
// It extends order.BrokerClient with market data and account queries.
type Client interface {
	order.BrokerClient

	// Market data
	GetQuote(ctx context.Context, symbol string) (float64, error)

	// Account & positions
	GetPositions(ctx context.Context) ([]*Position, error)
	GetAccount(ctx context.Context) (*Account, error)
}
