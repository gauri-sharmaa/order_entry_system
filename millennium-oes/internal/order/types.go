package order

import "time"

// -----------------------------------------------------------------------
// Order Types — every type supported by the engine
// -----------------------------------------------------------------------

type Type string

const (
	// Basic types
	TypeMarket      Type = "MARKET"
	TypeLimit       Type = "LIMIT"
	TypeStop        Type = "STOP"
	TypeStopLimit   Type = "STOP_LIMIT"
	TypeMOO         Type = "MARKET_ON_OPEN"   // Market-on-Open
	TypeMOC         Type = "MARKET_ON_CLOSE"  // Market-on-Close
	TypeLOO         Type = "LIMIT_ON_OPEN"    // Limit-on-Open
	TypeLOC         Type = "LIMIT_ON_CLOSE"   // Limit-on-Close
	TypeTrailingStop Type = "TRAILING_STOP"

	// Conditional / linked types (managed by engine, not sent directly to broker)
	TypeBracket Type = "BRACKET" // Entry + take-profit + stop-loss
	TypeOCO     Type = "OCO"     // One-Cancels-Other
	TypeOTO     Type = "OTO"     // One-Triggers-Other
	TypeOTOCO   Type = "OTOCO"   // One-Triggers-OCO

	// Algorithmic types
	TypeTWAP    Type = "TWAP"    // Time-Weighted Average Price
	TypeVWAP    Type = "VWAP"    // Volume-Weighted Average Price
	TypeIceberg Type = "ICEBERG" // Show small qty, hide rest

	// Pegged types
	TypePeggedMid  Type = "PEGGED_MID"  // Peg to midpoint
	TypePeggedBid  Type = "PEGGED_BID"  // Peg to bid
	TypePeggedAsk  Type = "PEGGED_ASK"  // Peg to ask

	// Touch-triggered
	TypeMIT Type = "MARKET_IF_TOUCHED" // Market order when price touches level
	TypeLIT Type = "LIMIT_IF_TOUCHED"  // Limit order when price touches level

	// Funari: day order that converts to MOC if unfilled at close
	TypeFunari Type = "FUNARI"
)

// -----------------------------------------------------------------------
// Time-in-Force
// -----------------------------------------------------------------------

type TimeInForce string

const (
	TIFDay  TimeInForce = "day"
	TIFGTC  TimeInForce = "gtc"  // Good Till Cancelled
	TIFGTD  TimeInForce = "gtd"  // Good Till Date
	TIFIOC  TimeInForce = "ioc"  // Immediate or Cancel
	TIFFOK  TimeInForce = "fok"  // Fill or Kill
	TIFATO  TimeInForce = "ato"  // At the Open
	TIFATC  TimeInForce = "atc"  // At the Close
	TIFGFA  TimeInForce = "gfa"  // Good for Auction
)

// -----------------------------------------------------------------------
// Side
// -----------------------------------------------------------------------

type Side string

const (
	SideBuy       Side = "buy"
	SideSell      Side = "sell"
	SideSellShort Side = "sell_short"
	SideBuyToCover Side = "buy_to_cover"
)

// -----------------------------------------------------------------------
// Status — FIX-aligned order lifecycle states
// -----------------------------------------------------------------------

type Status string

const (
	StatusNew             Status = "new"
	StatusPendingNew      Status = "pending_new"
	StatusAcknowledged    Status = "acknowledged"
	StatusPartiallyFilled Status = "partially_filled"
	StatusFilled          Status = "filled"
	StatusCancelled       Status = "cancelled"
	StatusPendingCancel   Status = "pending_cancel"
	StatusReplaced        Status = "replaced"
	StatusPendingReplace  Status = "pending_replace"
	StatusRejected        Status = "rejected"
	StatusExpired         Status = "expired"
	StatusHeld            Status = "held"   // Waiting for trigger condition
	StatusTriggered       Status = "triggered"
)

// -----------------------------------------------------------------------
// TrailingType — for trailing stop orders
// -----------------------------------------------------------------------

type TrailingType string

const (
	TrailingTypePrice   TrailingType = "price"   // Trail by fixed $ amount
	TrailingTypePercent TrailingType = "percent"  // Trail by percentage
)

// -----------------------------------------------------------------------
// Order — the core struct
// -----------------------------------------------------------------------

type Order struct {
	// Identity
	ID          string    `json:"id"`
	ClientOrderID string  `json:"client_order_id"`
	BrokerOrderID string  `json:"broker_order_id,omitempty"`

	// Instrument
	Symbol      string    `json:"symbol"`
	AssetClass  string    `json:"asset_class"` // us_equity, crypto, option

	// Order definition
	Side        Side        `json:"side"`
	Type        Type        `json:"type"`
	Qty         float64     `json:"qty"`
	Notional    float64     `json:"notional,omitempty"` // Dollar amount instead of qty

	// Pricing
	LimitPrice  *float64    `json:"limit_price,omitempty"`
	StopPrice   *float64    `json:"stop_price,omitempty"`

	// Trailing stop
	TrailType   TrailingType `json:"trail_type,omitempty"`
	TrailValue  *float64     `json:"trail_value,omitempty"`  // $ or %
	HighWaterMark *float64   `json:"high_water_mark,omitempty"` // tracked internally

	// Time
	TimeInForce TimeInForce `json:"time_in_force"`
	ExpireAt    *time.Time  `json:"expire_at,omitempty"` // for GTD

	// Iceberg
	VisibleQty  *float64    `json:"visible_qty,omitempty"`  // shown qty
	ReserveQty  float64     `json:"reserve_qty,omitempty"`  // hidden qty

	// Algo params
	AlgoParams  *AlgoParams `json:"algo_params,omitempty"`

	// Linked orders (OCO, OTO, Bracket)
	ParentID    string      `json:"parent_id,omitempty"`
	LinkedIDs   []string    `json:"linked_ids,omitempty"`
	LinkType    string      `json:"link_type,omitempty"` // "oco", "oto", "bracket_tp", "bracket_sl"

	// MIT/LIT trigger
	TouchPrice  *float64    `json:"touch_price,omitempty"`

	// Status & fills
	Status      Status      `json:"status"`
	FilledQty   float64     `json:"filled_qty"`
	FilledAvgPx float64     `json:"filled_avg_px,omitempty"`
	Fills       []Fill      `json:"fills,omitempty"`

	// Metadata
	CreatedAt   time.Time   `json:"created_at"`
	UpdatedAt   time.Time   `json:"updated_at"`
	SubmittedAt *time.Time  `json:"submitted_at,omitempty"`
	FilledAt    *time.Time  `json:"filled_at,omitempty"`

	// Extended hours
	ExtendedHours bool      `json:"extended_hours,omitempty"`
}

// Fill represents a single execution event
type Fill struct {
	ID        string    `json:"id"`
	OrderID   string    `json:"order_id"`
	Qty       float64   `json:"qty"`
	Price     float64   `json:"price"`
	Side      Side      `json:"side"`
	Timestamp time.Time `json:"timestamp"`
}

// AlgoParams holds parameters for TWAP/VWAP execution
type AlgoParams struct {
	// TWAP
	StartTime   *time.Time `json:"start_time,omitempty"`
	EndTime     *time.Time `json:"end_time,omitempty"`
	SliceCount  int        `json:"slice_count,omitempty"`  // number of child orders

	// VWAP
	ParticipationRate float64 `json:"participation_rate,omitempty"` // % of volume, e.g. 0.10 = 10%
	MaxPctVolume      float64 `json:"max_pct_volume,omitempty"`

	// Iceberg refill
	RefillThreshold float64 `json:"refill_threshold,omitempty"` // refill when visible qty drops below this
}

// -----------------------------------------------------------------------
// Request/Response DTOs
// -----------------------------------------------------------------------

// SubmitRequest is what the client sends to place an order
type SubmitRequest struct {
	Symbol        string       `json:"symbol"`
	Side          Side         `json:"side"`
	Type          Type         `json:"type"`
	Qty           float64      `json:"qty,omitempty"`
	Notional      float64      `json:"notional,omitempty"`
	LimitPrice    *float64     `json:"limit_price,omitempty"`
	StopPrice     *float64     `json:"stop_price,omitempty"`
	TimeInForce   TimeInForce  `json:"time_in_force"`
	ExpireAt      *time.Time   `json:"expire_at,omitempty"`
	TrailType     TrailingType `json:"trail_type,omitempty"`
	TrailValue    *float64     `json:"trail_value,omitempty"`
	VisibleQty    *float64     `json:"visible_qty,omitempty"`
	AlgoParams    *AlgoParams  `json:"algo_params,omitempty"`
	TouchPrice    *float64     `json:"touch_price,omitempty"`
	ExtendedHours bool         `json:"extended_hours,omitempty"`
	ClientOrderID string       `json:"client_order_id,omitempty"`

	// For bracket orders
	TakeProfitPrice *float64 `json:"take_profit_price,omitempty"`
	StopLossPrice   *float64 `json:"stop_loss_price,omitempty"`
	StopLossLimit   *float64 `json:"stop_loss_limit,omitempty"`

	// For OCO
	OCOPairRequest *SubmitRequest `json:"oco_pair,omitempty"`

	// For OTO
	OTOSecondary *SubmitRequest `json:"oto_secondary,omitempty"`
}

// ReplaceRequest modifies an existing order
type ReplaceRequest struct {
	Qty        *float64 `json:"qty,omitempty"`
	LimitPrice *float64 `json:"limit_price,omitempty"`
	StopPrice  *float64 `json:"stop_price,omitempty"`
	TrailValue *float64 `json:"trail_value,omitempty"`
	TimeInForce *TimeInForce `json:"time_in_force,omitempty"`
}
