// types.go — Core data types for the order engine.
//
// Design principles:
//   - Fixed-size structs (no pointers → no GC scanning)
//   - Prices stored as int64 microdollars (avoids floating point)
//   - Symbols stored as fixed [8]byte arrays (no string allocation)
//   - Status transitions are a simple uint8 state machine

package engine

import "time"

// -----------------------------------------------------------------------
// Price representation
// Prices are stored as int64 in microdollars (1 dollar = 1,000,000).
// This avoids floating point entirely on the hot path.
//   $175.50 → 175_500_000
//   $0.01   → 10_000
// -----------------------------------------------------------------------

const MicroDollar = 1_000_000

func PriceToMicros(dollars float64) int64 {
	return int64(dollars * MicroDollar)
}

func MicrosToPrice(micros int64) float64 {
	return float64(micros) / MicroDollar
}

// -----------------------------------------------------------------------
// Order — fixed-size struct, lives in pre-allocated array
// -----------------------------------------------------------------------

type Order struct {
	// Identity (32 bytes)
	ID            uint32   // internal sequential ID
	ClientID      [16]byte // client-assigned ID
	BrokerID      [16]byte // broker-assigned ID (from FIX tag 37)

	// Instrument (16 bytes)
	Symbol        [8]byte
	Exchange      [8]byte  // "SMART", "NYSE", "NASDAQ"

	// Order definition (32 bytes)
	Side          Side
	Type          OrderType
	TIF           TimeInForce
	Status        Status
	Qty           int32
	FilledQty     int32
	Price         int64    // limit price (microdollars)
	StopPrice     int64    // stop trigger price

	// Trailing stop (16 bytes)
	TrailType     TrailType
	_pad1         [3]byte
	TrailValue    int32    // in microdollars or basis points
	HighWaterMark int64    // tracked peak price

	// Fill info (16 bytes)
	FilledAvgPx   int64    // average fill price (microdollars)
	LastFillPx    int64    // most recent fill price

	// Linked orders (16 bytes)
	ParentIdx     uint32   // index of parent order (0 = none)
	LinkedIdx1    uint32   // bracket TP / OCO pair
	LinkedIdx2    uint32   // bracket SL
	LinkType      LinkType

	// Timestamps (24 bytes)
	CreatedAt     int64    // unix nanos
	SubmittedAt   int64
	FilledAt      int64

	// Algo params (16 bytes)
	AlgoSlices    int32
	AlgoInterval  int32    // nanoseconds between slices
	AlgoEndTime   int64    // unix nanos

	// Iceberg (8 bytes)
	VisibleQty    int32
	ReserveQty    int32
}

// Total: ~176 bytes per order. 1M orders = ~176MB. Fits in L3 cache on modern CPUs.

// -----------------------------------------------------------------------
// Enums — all uint8 for compact storage
// -----------------------------------------------------------------------

type Side uint8

const (
	SideBuy      Side = 1
	SideSell     Side = 2
	SideShort    Side = 3
	SideCover    Side = 4
)

type OrderType uint8

const (
	OrdMarket       OrderType = 1
	OrdLimit        OrderType = 2
	OrdStop         OrderType = 3
	OrdStopLimit    OrderType = 4
	OrdTrailingStop OrderType = 5
	OrdMOO          OrderType = 6  // Market-on-Open
	OrdMOC          OrderType = 7  // Market-on-Close
	OrdLOO          OrderType = 8  // Limit-on-Open
	OrdLOC          OrderType = 9  // Limit-on-Close
	OrdMIT          OrderType = 10 // Market-if-Touched
	OrdLIT          OrderType = 11 // Limit-if-Touched
	OrdBracket      OrderType = 12
	OrdOCO          OrderType = 13
	OrdOTO          OrderType = 14
	OrdTWAP         OrderType = 15
	OrdVWAP         OrderType = 16
	OrdIceberg      OrderType = 17
	OrdFunari       OrderType = 18
)

type TimeInForce uint8

const (
	TIFDay TimeInForce = 0
	TIFGTC TimeInForce = 1
	TIFIOC TimeInForce = 2
	TIFFOK TimeInForce = 3
	TIFGTD TimeInForce = 4
	TIFATO TimeInForce = 5 // At the Open
	TIFATC TimeInForce = 6 // At the Close
)

type Status uint8

const (
	StatusEmpty          Status = 0 // slot is free
	StatusNew            Status = 1
	StatusPendingNew     Status = 2
	StatusAcknowledged   Status = 3
	StatusPartialFill    Status = 4
	StatusFilled         Status = 5
	StatusPendingCancel  Status = 6
	StatusCancelled      Status = 7
	StatusRejected       Status = 8
	StatusHeld           Status = 9  // waiting for trigger
	StatusPendingReplace Status = 10
	StatusExpired        Status = 11
)

type TrailType uint8

const (
	TrailNone    TrailType = 0
	TrailAmount  TrailType = 1 // fixed $ trail
	TrailPercent TrailType = 2 // percentage trail
)

type LinkType uint8

const (
	LinkNone      LinkType = 0
	LinkBracketTP LinkType = 1 // take-profit leg
	LinkBracketSL LinkType = 2 // stop-loss leg
	LinkOCO       LinkType = 3
	LinkOTO       LinkType = 4
)

// -----------------------------------------------------------------------
// Status helpers
// -----------------------------------------------------------------------

func (s Status) IsActive() bool {
	return s == StatusNew || s == StatusPendingNew || s == StatusAcknowledged ||
		s == StatusPartialFill || s == StatusHeld
}

func (s Status) IsCancellable() bool {
	return s == StatusNew || s == StatusPendingNew || s == StatusAcknowledged ||
		s == StatusPartialFill || s == StatusHeld
}

func (s Status) IsTerminal() bool {
	return s == StatusFilled || s == StatusCancelled || s == StatusRejected || s == StatusExpired
}

// -----------------------------------------------------------------------
// Symbol helpers
// -----------------------------------------------------------------------

func SymbolFromString(s string) [8]byte {
	var sym [8]byte
	copy(sym[:], s)
	return sym
}

func SymbolToString(sym [8]byte) string {
	for i, b := range sym {
		if b == 0 {
			return string(sym[:i])
		}
	}
	return string(sym[:])
}

// -----------------------------------------------------------------------
// Timestamp helpers
// -----------------------------------------------------------------------

func NowNanos() int64 {
	return time.Now().UnixNano()
}
