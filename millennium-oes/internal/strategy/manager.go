// Package strategy manages multiple concurrent trading strategies.
//
// Each strategy has:
//   - A unique ID (up to 8 chars)
//   - Independent P&L tracking
//   - Independent risk limits
//   - Its own order history
//
// This mirrors how Millennium operates: 300+ independent teams (pods),
// each running their own strategy with their own risk budget.
// The OES tracks them all simultaneously.

package strategy

import (
	"fmt"
	"sync"

	"github.com/millennium-oes/internal/engine"
)

// Strategy represents a single trading strategy/pod
type Strategy struct {
	ID          string  `json:"id"`
	Name        string  `json:"name"`
	Description string  `json:"description,omitempty"`
	Active      bool    `json:"active"`

	// Risk limits (per-strategy)
	MaxPositionValue float64 `json:"max_position_value"`
	MaxOrderSize     int     `json:"max_order_size"`
	MaxDailyLoss     float64 `json:"max_daily_loss"`

	// P&L tracking
	DailyPnL    float64 `json:"daily_pnl"`
	TotalPnL    float64 `json:"total_pnl"`
	OrderCount  int     `json:"order_count"`
	FillCount   int     `json:"fill_count"`
	WinCount    int     `json:"win_count"`

	// Position tracking per symbol
	Positions map[string]float64 `json:"positions"` // symbol → qty
}

// Manager tracks all active strategies
type Manager struct {
	mu         sync.RWMutex
	strategies map[string]*Strategy // strategyID → Strategy
}

// NewManager creates a strategy manager
func NewManager() *Manager {
	return &Manager{
		strategies: make(map[string]*Strategy),
	}
}

// Register adds a new strategy
func (m *Manager) Register(s Strategy) error {
	if s.ID == "" {
		return fmt.Errorf("strategy ID required")
	}
	if len(s.ID) > 8 {
		return fmt.Errorf("strategy ID must be 8 chars or less")
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if _, exists := m.strategies[s.ID]; exists {
		return fmt.Errorf("strategy %s already exists", s.ID)
	}

	if s.Positions == nil {
		s.Positions = make(map[string]float64)
	}
	s.Active = true
	m.strategies[s.ID] = &s
	return nil
}

// Remove deactivates a strategy
func (m *Manager) Remove(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	s, exists := m.strategies[id]
	if !exists {
		return fmt.Errorf("strategy %s not found", id)
	}
	s.Active = false
	return nil
}

// Get returns a strategy by ID
func (m *Manager) Get(id string) (*Strategy, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	s, ok := m.strategies[id]
	return s, ok
}

// List returns all strategies
func (m *Manager) List() []*Strategy {
	m.mu.RLock()
	defer m.mu.RUnlock()
	result := make([]*Strategy, 0, len(m.strategies))
	for _, s := range m.strategies {
		result = append(result, s)
	}
	return result
}

// CheckRisk validates an order against the strategy's risk limits
func (m *Manager) CheckRisk(strategyID string, qty int, price float64) error {
	m.mu.RLock()
	defer m.mu.RUnlock()

	s, exists := m.strategies[strategyID]
	if !exists {
		return nil // no strategy = no extra limits
	}

	if !s.Active {
		return fmt.Errorf("strategy %s is deactivated", strategyID)
	}

	// Per-strategy order size limit
	if s.MaxOrderSize > 0 && qty > s.MaxOrderSize {
		return fmt.Errorf("strategy %s: order size %d exceeds limit %d", s.ID, qty, s.MaxOrderSize)
	}

	// Per-strategy daily loss limit
	if s.MaxDailyLoss > 0 && s.DailyPnL < -s.MaxDailyLoss {
		return fmt.Errorf("strategy %s: daily loss $%.2f exceeds limit $%.2f", s.ID, -s.DailyPnL, s.MaxDailyLoss)
	}

	return nil
}

// RecordOrder increments the order count for a strategy
func (m *Manager) RecordOrder(strategyID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s, ok := m.strategies[strategyID]; ok {
		s.OrderCount++
	}
}

// RecordFill records a fill for a strategy
func (m *Manager) RecordFill(strategyID string, symbol string, side engine.Side, qty int32, pnl float64) {
	m.mu.Lock()
	defer m.mu.Unlock()

	s, ok := m.strategies[strategyID]
	if !ok {
		return
	}

	s.FillCount++
	s.DailyPnL += pnl
	s.TotalPnL += pnl
	if pnl > 0 {
		s.WinCount++
	}

	// Update position
	if side == engine.SideBuy || side == engine.SideCover {
		s.Positions[symbol] += float64(qty)
	} else {
		s.Positions[symbol] -= float64(qty)
	}
	if s.Positions[symbol] == 0 {
		delete(s.Positions, symbol)
	}
}

// ResetDaily resets daily P&L for all strategies (call at market open)
func (m *Manager) ResetDaily() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, s := range m.strategies {
		s.DailyPnL = 0
	}
}
