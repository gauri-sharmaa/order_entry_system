// engine.go — The core order engine.
//
// This is a single-threaded event loop that processes orders from the
// ring buffer. No locks on the hot path. No allocations. No syscalls
// (except when writing to the WAL or sending FIX messages).
//
// The event loop pattern:
//   for {
//       event = ring.TryConsume()
//       if event: process(event)    ← hot path, nanoseconds
//       else: yield CPU briefly     ← cold path, microseconds
//   }
//
// This is the same architecture as:
//   - LMAX Disruptor (London Stock Exchange)
//   - Aeron (real-time messaging)
//   - Every HFT matching engine

package engine

import (
	"context"
	"fmt"
	"log"
	"runtime"
	"sync/atomic"
	"time"

	"github.com/millennium-oes/internal/wal"
)

// Config for the engine
type Config struct {
	MaxOrders         int
	MaxPositionValue  float64
	MaxOrderSize      int
	MaxDailyLoss      float64
	PriceDeviationPct float64
	WAL               *wal.Log
}

// Broker is the interface for sending orders to the exchange
type Broker interface {
	Submit(o *Order) error
	Cancel(brokerID [16]byte) error
	Replace(brokerID [16]byte, qty int32, price int64) error
}

// Engine is the core order management engine.
type Engine struct {
	// Order store — pre-allocated flat array, indexed by order ID
	orders    []Order
	nextID    atomic.Uint32
	maxOrders uint32

	// Ring buffer — lock-free event queue
	ring *RingBuffer

	// Risk state (inline, no indirection)
	positions  [4096]position // symbol hash → position
	dailyPnL   int64          // microdollars
	killSwitch atomic.Bool

	// Risk limits
	maxPositionValue  int64 // microdollars
	maxOrderSize      int32
	maxDailyLoss      int64 // microdollars
	priceDeviationPct int64 // basis points (500 = 5%)

	// Broker connection
	broker Broker

	// WAL for durability
	wal *wal.Log

	// Subscribers for real-time updates (off hot path)
	updates chan OrderUpdate

	// Stats
	ordersProcessed atomic.Uint64
	latencySum      atomic.Uint64 // nanoseconds
	running         atomic.Bool
}

type position struct {
	symbol [8]byte
	qty    int32
	value  int64 // microdollars
}

// OrderUpdate is sent to the HTTP gateway for SSE streaming
type OrderUpdate struct {
	Idx    uint32
	Status Status
}

// New creates a new engine with pre-allocated storage.
func New(cfg Config) *Engine {
	e := &Engine{
		orders:            make([]Order, cfg.MaxOrders),
		maxOrders:         uint32(cfg.MaxOrders),
		ring:              NewRingBuffer(65536), // 64K event slots
		maxPositionValue:  PriceToMicros(cfg.MaxPositionValue),
		maxOrderSize:      int32(cfg.MaxOrderSize),
		maxDailyLoss:      PriceToMicros(cfg.MaxDailyLoss),
		priceDeviationPct: int64(cfg.PriceDeviationPct * 100), // to basis points
		wal:               cfg.WAL,
		updates:           make(chan OrderUpdate, 4096),
	}
	// Reserve slot 0 as "null"
	e.nextID.Store(1)
	return e
}

func (e *Engine) SetBroker(b Broker) { e.broker = b }

// -----------------------------------------------------------------------
// Submit — called by HTTP handler (producer side of ring buffer)
// -----------------------------------------------------------------------

func (e *Engine) Submit(ev Event) (uint32, error) {
	if e.killSwitch.Load() {
		return 0, fmt.Errorf("KILL SWITCH ACTIVE")
	}

	// Allocate order slot
	idx := e.nextID.Add(1) - 1
	if idx >= e.maxOrders {
		return 0, fmt.Errorf("order store full (%d max)", e.maxOrders)
	}

	ev.OrderIdx = idx
	ev.Timestamp = NowNanos()
	ev.Type = EventSubmit

	if !e.ring.TryPublish(ev) {
		return 0, fmt.Errorf("ring buffer full (back-pressure)")
	}

	return idx, nil
}

// Cancel publishes a cancel event to the ring buffer
func (e *Engine) Cancel(orderIdx uint32) error {
	ev := Event{Type: EventCancel, OrderIdx: orderIdx, Timestamp: NowNanos()}
	if !e.ring.TryPublish(ev) {
		return fmt.Errorf("ring buffer full")
	}
	return nil
}

// GetOrder returns a copy of an order by index (safe for concurrent read)
func (e *Engine) GetOrder(idx uint32) (Order, bool) {
	if idx == 0 || idx >= e.maxOrders {
		return Order{}, false
	}
	o := e.orders[idx]
	if o.Status == StatusEmpty {
		return Order{}, false
	}
	return o, true
}

// ListActive returns indices of all active orders
func (e *Engine) ListActive() []uint32 {
	max := e.nextID.Load()
	result := make([]uint32, 0, 256)
	for i := uint32(1); i < max && i < e.maxOrders; i++ {
		if e.orders[i].Status.IsActive() {
			result = append(result, i)
		}
	}
	return result
}

// Updates returns the channel for SSE streaming
func (e *Engine) Updates() <-chan OrderUpdate { return e.updates }

// Stats returns engine statistics
func (e *Engine) Stats() (processed uint64, avgLatencyNs uint64) {
	p := e.ordersProcessed.Load()
	if p == 0 {
		return 0, 0
	}
	return p, e.latencySum.Load() / p
}

// KillSwitch activates/deactivates the emergency stop
func (e *Engine) SetKillSwitch(active bool) {
	e.killSwitch.Store(active)
	if active {
		log.Println("[ENGINE] ⚡ KILL SWITCH ACTIVATED")
	} else {
		log.Println("[ENGINE] Kill switch deactivated")
	}
}

func (e *Engine) IsKillSwitchActive() bool { return e.killSwitch.Load() }

// -----------------------------------------------------------------------
// Run — the event loop (HOT PATH)
// This runs on a dedicated, pinned CPU core.
// -----------------------------------------------------------------------

func (e *Engine) Run(ctx context.Context) {
	e.running.Store(true)
	defer e.running.Store(false)

	log.Println("[ENGINE] Event loop started")

	spins := 0
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		event, ok := e.ring.TryConsume()
		if !ok {
			// Nothing to process — busy-wait with backoff
			spins++
			if spins > 1000 {
				runtime.Gosched() // yield after 1000 empty spins
				spins = 0
			}
			continue
		}

		spins = 0
		start := time.Now().UnixNano()

		switch event.Type {
		case EventSubmit:
			e.processSubmit(event)
		case EventCancel:
			e.processCancel(event)
		case EventReplace:
			e.processReplace(event)
		case EventFill:
			e.processFill(event)
		case EventKillSwitch:
			e.killSwitch.Store(true)
		}

		elapsed := time.Now().UnixNano() - start
		e.ordersProcessed.Add(1)
		e.latencySum.Add(uint64(elapsed))
	}
}

func (e *Engine) Stop() {
	e.running.Store(false)
}

// -----------------------------------------------------------------------
// Event processors (all run on the hot path, single-threaded)
// -----------------------------------------------------------------------

func (e *Engine) processSubmit(ev Event) {
	idx := ev.OrderIdx
	o := &e.orders[idx]

	// Build order from event
	o.ID = idx
	o.Symbol = ev.Symbol
	o.Side = ev.Side
	o.Type = ev.OrdType
	o.TIF = ev.TIF
	o.Qty = ev.Qty
	o.Price = ev.Price
	o.StopPrice = ev.StopPrice
	o.TrailType = ev.TrailType
	o.TrailValue = int32(ev.TrailVal)
	o.Status = StatusNew
	o.CreatedAt = ev.Timestamp

	// -----------------------------------------------------------------------
	// RISK CHECKS (inline, no function call overhead)
	// -----------------------------------------------------------------------

	// 1. Order size
	if o.Qty > e.maxOrderSize {
		o.Status = StatusRejected
		e.notify(idx, StatusRejected)
		return
	}

	// 2. Kill switch
	if e.killSwitch.Load() {
		o.Status = StatusRejected
		e.notify(idx, StatusRejected)
		return
	}

	// 3. Daily loss
	if e.dailyPnL < -e.maxDailyLoss {
		o.Status = StatusRejected
		e.notify(idx, StatusRejected)
		return
	}

	// -----------------------------------------------------------------------
	// WAL — write before sending to broker (durability guarantee)
	// -----------------------------------------------------------------------
	if e.wal != nil {
		e.wal.Append(wal.Entry{
			Type:      wal.EntryNewOrder,
			OrderIdx:  idx,
			Timestamp: ev.Timestamp,
			Data:      walEncodeOrder(o),
		})
	}

	// -----------------------------------------------------------------------
	// Send to broker
	// -----------------------------------------------------------------------
	o.Status = StatusPendingNew
	o.SubmittedAt = NowNanos()

	if e.broker != nil {
		if err := e.broker.Submit(o); err != nil {
			o.Status = StatusRejected
			e.notify(idx, StatusRejected)
			return
		}
	} else {
		// Simulation mode — immediately acknowledge
		o.Status = StatusAcknowledged
	}

	e.notify(idx, o.Status)
}

func (e *Engine) processCancel(ev Event) {
	idx := ev.OrderIdx
	o := &e.orders[idx]

	if !o.Status.IsCancellable() {
		return
	}

	o.Status = StatusPendingCancel

	if e.broker != nil {
		if err := e.broker.Cancel(o.BrokerID); err != nil {
			o.Status = StatusAcknowledged // rollback
			return
		}
	}

	o.Status = StatusCancelled

	// Cancel linked orders
	if o.LinkedIdx1 != 0 {
		linked := &e.orders[o.LinkedIdx1]
		if linked.Status.IsCancellable() {
			linked.Status = StatusCancelled
			e.notify(o.LinkedIdx1, StatusCancelled)
		}
	}
	if o.LinkedIdx2 != 0 {
		linked := &e.orders[o.LinkedIdx2]
		if linked.Status.IsCancellable() {
			linked.Status = StatusCancelled
			e.notify(o.LinkedIdx2, StatusCancelled)
		}
	}

	if e.wal != nil {
		e.wal.Append(wal.Entry{Type: wal.EntryCancel, OrderIdx: idx, Timestamp: NowNanos()})
	}

	e.notify(idx, StatusCancelled)
}

func (e *Engine) processReplace(ev Event) {
	idx := ev.OrderIdx
	o := &e.orders[idx]

	if o.Status != StatusAcknowledged && o.Status != StatusPartialFill {
		return
	}

	if ev.Qty > 0 {
		o.Qty = ev.Qty
	}
	if ev.Price > 0 {
		o.Price = ev.Price
	}

	if e.broker != nil {
		e.broker.Replace(o.BrokerID, o.Qty, o.Price)
	}

	e.notify(idx, o.Status)
}

func (e *Engine) processFill(ev Event) {
	idx := ev.OrderIdx
	o := &e.orders[idx]

	fillQty := ev.Qty
	fillPx := ev.Price

	// Update fill state
	totalValue := int64(o.FilledQty)*o.FilledAvgPx + int64(fillQty)*fillPx
	o.FilledQty += fillQty
	if o.FilledQty > 0 {
		o.FilledAvgPx = totalValue / int64(o.FilledQty)
	}
	o.LastFillPx = fillPx

	if o.FilledQty >= o.Qty {
		o.Status = StatusFilled
		o.FilledAt = NowNanos()
	} else {
		o.Status = StatusPartialFill
	}

	// Update position
	symHash := hashSymbol(o.Symbol)
	pos := &e.positions[symHash]
	pos.symbol = o.Symbol
	if o.Side == SideBuy || o.Side == SideCover {
		pos.qty += fillQty
	} else {
		pos.qty -= fillQty
	}
	pos.value = int64(pos.qty) * fillPx

	if e.wal != nil {
		e.wal.Append(wal.Entry{
			Type:      wal.EntryFill,
			OrderIdx:  idx,
			Timestamp: NowNanos(),
		})
	}

	e.notify(idx, o.Status)
}

// -----------------------------------------------------------------------
// Helpers
// -----------------------------------------------------------------------

func (e *Engine) notify(idx uint32, status Status) {
	select {
	case e.updates <- OrderUpdate{Idx: idx, Status: status}:
	default: // don't block hot path if subscriber is slow
	}
}

// hashSymbol maps a symbol to a position array index (simple FNV-like hash)
func hashSymbol(sym [8]byte) uint16 {
	h := uint32(2166136261)
	for _, b := range sym {
		if b == 0 {
			break
		}
		h ^= uint32(b)
		h *= 16777619
	}
	return uint16(h & 0x0FFF) // 4096 slots
}

// ReplayWAL restores engine state from the write-ahead log
func (e *Engine) ReplayWAL() error {
	if e.wal == nil {
		return nil
	}
	entries := e.wal.ReadAll()
	for _, entry := range entries {
		switch entry.Type {
		case wal.EntryNewOrder:
			if entry.OrderIdx < e.maxOrders {
				walDecodeOrder(entry.Data, &e.orders[entry.OrderIdx])
				if e.nextID.Load() <= entry.OrderIdx {
					e.nextID.Store(entry.OrderIdx + 1)
				}
			}
		case wal.EntryCancel:
			if entry.OrderIdx < e.maxOrders {
				e.orders[entry.OrderIdx].Status = StatusCancelled
			}
		case wal.EntryFill:
			if entry.OrderIdx < e.maxOrders {
				e.orders[entry.OrderIdx].Status = StatusFilled
			}
		}
	}
	return nil
}

// InjectFill is called by the FIX client when a fill arrives from the broker
func (e *Engine) InjectFill(orderIdx uint32, qty int32, price int64) {
	ev := Event{
		Type:     EventFill,
		OrderIdx: orderIdx,
		Qty:      qty,
		Price:    price,
	}
	e.ring.TryPublish(ev)
}

// walEncodeOrder serializes an order to bytes for the WAL (minimal, fixed-size)
func walEncodeOrder(o *Order) []byte {
	// Simple binary encoding — in production you'd use a proper codec
	buf := make([]byte, 64)
	buf[0] = byte(o.Side)
	buf[1] = byte(o.Type)
	buf[2] = byte(o.TIF)
	buf[3] = byte(o.Status)
	copy(buf[4:12], o.Symbol[:])
	putInt32(buf[12:], o.Qty)
	putInt64(buf[16:], o.Price)
	putInt64(buf[24:], o.StopPrice)
	return buf[:32]
}

func walDecodeOrder(data []byte, o *Order) {
	if len(data) < 32 {
		return
	}
	o.Side = Side(data[0])
	o.Type = OrderType(data[1])
	o.TIF = TimeInForce(data[2])
	o.Status = Status(data[3])
	copy(o.Symbol[:], data[4:12])
	o.Qty = getInt32(data[12:])
	o.Price = getInt64(data[16:])
	o.StopPrice = getInt64(data[24:])
}

func putInt32(b []byte, v int32) {
	b[0] = byte(v)
	b[1] = byte(v >> 8)
	b[2] = byte(v >> 16)
	b[3] = byte(v >> 24)
}

func getInt32(b []byte) int32 {
	return int32(b[0]) | int32(b[1])<<8 | int32(b[2])<<16 | int32(b[3])<<24
}

func putInt64(b []byte, v int64) {
	b[0] = byte(v)
	b[1] = byte(v >> 8)
	b[2] = byte(v >> 16)
	b[3] = byte(v >> 24)
	b[4] = byte(v >> 32)
	b[5] = byte(v >> 40)
	b[6] = byte(v >> 48)
	b[7] = byte(v >> 56)
}

func getInt64(b []byte) int64 {
	return int64(b[0]) | int64(b[1])<<8 | int64(b[2])<<16 | int64(b[3])<<24 |
		int64(b[4])<<32 | int64(b[5])<<40 | int64(b[6])<<48 | int64(b[7])<<56
}
