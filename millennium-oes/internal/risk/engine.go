package risk

import (
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/millennium-oes/internal/order"
)

// Config holds risk limits
type Config struct {
	MaxOrderSize      int64   // max shares per order
	MaxPositionValue  float64 // max $ value of any single position
	MaxDailyLoss      float64 // max $ loss before kill switch
	PriceDeviationPct float64 // max % deviation from market price for limit orders
}

// Engine performs pre-trade risk checks.
// All checks run synchronously on the hot path — must be fast.
type Engine struct {
	cfg Config

	mu         sync.RWMutex
	positions  map[string]float64 // symbol → net position
	dailyPnL   float64
	orderCount map[string]int     // symbol → order count today (duplicate detection)
	lastReset  time.Time
	killSwitch bool
}

func NewEngine(cfg Config) *Engine {
	return &Engine{
		cfg:        cfg,
		positions:  make(map[string]float64),
		orderCount: make(map[string]int),
		lastReset:  time.Now(),
	}
}

// Check runs all pre-trade risk checks. Returns nil if order is approved.
func (e *Engine) Check(req order.SubmitRequest, marketPrice float64) error {
	e.resetDailyIfNeeded()

	e.mu.RLock()
	defer e.mu.RUnlock()

	if e.killSwitch {
		return fmt.Errorf("KILL SWITCH ACTIVE: all trading halted")
	}

	checks := []func(order.SubmitRequest, float64) error{
		e.checkOrderSize,
		e.checkPositionLimit,
		e.checkPriceReasonability,
		e.checkDailyLoss,
		e.checkDuplicateOrder,
		e.checkShortSellRestriction,
		e.checkNotionalLimit,
	}

	for _, check := range checks {
		if err := check(req, marketPrice); err != nil {
			return fmt.Errorf("risk check failed: %w", err)
		}
	}

	return nil
}

// -----------------------------------------------------------------------
// Individual checks
// -----------------------------------------------------------------------

func (e *Engine) checkOrderSize(req order.SubmitRequest, _ float64) error {
	if req.Qty > float64(e.cfg.MaxOrderSize) {
		return fmt.Errorf("order size %.0f exceeds max %d shares", req.Qty, e.cfg.MaxOrderSize)
	}
	return nil
}

func (e *Engine) checkPositionLimit(req order.SubmitRequest, marketPrice float64) error {
	if marketPrice <= 0 {
		return nil // can't check without price
	}

	currentPos := e.positions[req.Symbol]
	newPos := currentPos
	if req.Side == order.SideBuy || req.Side == order.SideBuyToCover {
		newPos += req.Qty
	} else {
		newPos -= req.Qty
	}

	posValue := math.Abs(newPos) * marketPrice
	if posValue > e.cfg.MaxPositionValue {
		return fmt.Errorf("position value $%.2f would exceed limit $%.2f", posValue, e.cfg.MaxPositionValue)
	}
	return nil
}

func (e *Engine) checkPriceReasonability(req order.SubmitRequest, marketPrice float64) error {
	if marketPrice <= 0 || req.LimitPrice == nil {
		return nil
	}

	deviation := math.Abs(*req.LimitPrice-marketPrice) / marketPrice * 100
	if deviation > e.cfg.PriceDeviationPct {
		return fmt.Errorf("limit price $%.2f deviates %.1f%% from market $%.2f (max %.1f%%)",
			*req.LimitPrice, deviation, marketPrice, e.cfg.PriceDeviationPct)
	}
	return nil
}

func (e *Engine) checkDailyLoss(req order.SubmitRequest, _ float64) error {
	if e.dailyPnL < -e.cfg.MaxDailyLoss {
		return fmt.Errorf("daily loss $%.2f exceeds limit $%.2f", -e.dailyPnL, e.cfg.MaxDailyLoss)
	}
	return nil
}

func (e *Engine) checkDuplicateOrder(req order.SubmitRequest, _ float64) error {
	key := fmt.Sprintf("%s-%s-%.0f", req.Symbol, req.Side, req.Qty)
	count := e.orderCount[key]
	if count >= 3 {
		return fmt.Errorf("possible duplicate: %d similar orders for %s in last minute", count, req.Symbol)
	}
	return nil
}

func (e *Engine) checkShortSellRestriction(req order.SubmitRequest, _ float64) error {
	if req.Side != order.SideSellShort {
		return nil
	}
	// In a real system, check the locate list and SSR (Short Sale Restriction) list
	// For now, just ensure we're not shorting more than we can cover
	pos := e.positions[req.Symbol]
	if pos < 0 && math.Abs(pos)+req.Qty > float64(e.cfg.MaxOrderSize) {
		return fmt.Errorf("short position would exceed limits")
	}
	return nil
}

func (e *Engine) checkNotionalLimit(req order.SubmitRequest, marketPrice float64) error {
	if req.Notional > 0 && req.Notional > e.cfg.MaxPositionValue {
		return fmt.Errorf("notional $%.2f exceeds max position value $%.2f",
			req.Notional, e.cfg.MaxPositionValue)
	}
	return nil
}

// -----------------------------------------------------------------------
// State updates (called after fills)
// -----------------------------------------------------------------------

func (e *Engine) RecordFill(symbol string, side order.Side, qty, price float64) {
	e.mu.Lock()
	defer e.mu.Unlock()

	if side == order.SideBuy || side == order.SideBuyToCover {
		e.positions[symbol] += qty
	} else {
		e.positions[symbol] -= qty
	}
}

func (e *Engine) RecordPnL(delta float64) {
	e.mu.Lock()
	e.dailyPnL += delta
	e.mu.Unlock()
}

func (e *Engine) RecordOrder(symbol string, side order.Side, qty float64) {
	e.mu.Lock()
	key := fmt.Sprintf("%s-%s-%.0f", symbol, side, qty)
	e.orderCount[key]++
	e.mu.Unlock()

	// Decay after 1 minute
	go func() {
		time.Sleep(time.Minute)
		e.mu.Lock()
		if e.orderCount[key] > 0 {
			e.orderCount[key]--
		}
		e.mu.Unlock()
	}()
}

// ActivateKillSwitch halts all trading immediately
func (e *Engine) ActivateKillSwitch() {
	e.mu.Lock()
	e.killSwitch = true
	e.mu.Unlock()
}

// DeactivateKillSwitch re-enables trading (requires explicit action)
func (e *Engine) DeactivateKillSwitch() {
	e.mu.Lock()
	e.killSwitch = false
	e.mu.Unlock()
}

func (e *Engine) GetPositions() map[string]float64 {
	e.mu.RLock()
	defer e.mu.RUnlock()
	result := make(map[string]float64, len(e.positions))
	for k, v := range e.positions {
		result[k] = v
	}
	return result
}

func (e *Engine) GetDailyPnL() float64 {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.dailyPnL
}

func (e *Engine) IsKillSwitchActive() bool {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.killSwitch
}

func (e *Engine) resetDailyIfNeeded() {
	now := time.Now()
	if now.Day() != e.lastReset.Day() {
		e.mu.Lock()
		e.dailyPnL = 0
		e.orderCount = make(map[string]int)
		e.lastReset = now
		e.mu.Unlock()
	}
}
