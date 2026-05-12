// Package risk implements portfolio-level risk tracking.
//
// Metrics computed:
//   - Sharpe Ratio (annualized return / volatility)
//   - Max Drawdown (largest peak-to-trough decline)
//   - Concentration (largest position as % of portfolio)
//   - Daily VaR (Value at Risk — 95th percentile expected loss)
//   - Beta (portfolio sensitivity to market)
//   - Win Rate (% of profitable trades)
//
// All computations are O(1) amortized using rolling windows.
// No external libraries — pure arithmetic.

package risk

import (
	"math"
	"sync"
	"time"
)

// Tracker maintains real-time risk metrics
type Tracker struct {
	mu sync.RWMutex

	// Daily returns for Sharpe/VaR calculation (rolling 60-day window)
	dailyReturns [60]float64
	returnIdx    int
	returnCount  int

	// Equity curve for drawdown
	peakEquity    float64
	currentEquity float64
	maxDrawdown   float64

	// Trade tracking for win rate
	totalTrades int
	winTrades   int

	// Position tracking for concentration
	positions map[string]float64 // symbol → market value

	// P&L history (last 30 days, for the chart)
	pnlHistory []PnLPoint

	// Daily tracking
	dayStartEquity float64
	lastResetDay   int
}

// PnLPoint is a single data point on the P&L curve
type PnLPoint struct {
	Timestamp int64   `json:"t"` // unix seconds
	Equity    float64 `json:"e"`
	PnL       float64 `json:"p"` // cumulative P&L
}

// Snapshot is the current risk state (returned to the UI)
type Snapshot struct {
	SharpeRatio    float64            `json:"sharpe_ratio"`
	MaxDrawdown    float64            `json:"max_drawdown_pct"`
	CurrentDD      float64            `json:"current_drawdown_pct"`
	DailyVaR95     float64            `json:"daily_var_95"`
	Concentration  float64            `json:"concentration_pct"`
	TopPosition    string             `json:"top_position"`
	WinRate        float64            `json:"win_rate_pct"`
	TotalTrades    int                `json:"total_trades"`
	DailyPnL       float64            `json:"daily_pnl"`
	TotalPnL       float64            `json:"total_pnl"`
	Equity         float64            `json:"equity"`
	PnLHistory     []PnLPoint         `json:"pnl_history"`
	Positions      map[string]float64 `json:"positions"`
}

// NewTracker creates a risk tracker
func NewTracker(initialEquity float64) *Tracker {
	return &Tracker{
		peakEquity:     initialEquity,
		currentEquity:  initialEquity,
		dayStartEquity: initialEquity,
		positions:      make(map[string]float64),
		pnlHistory:     make([]PnLPoint, 0, 1024),
	}
}

// RecordTrade records a completed trade for win rate tracking
func (t *Tracker) RecordTrade(pnl float64) {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.totalTrades++
	if pnl > 0 {
		t.winTrades++
	}
}

// UpdateEquity updates the current portfolio value
func (t *Tracker) UpdateEquity(equity float64) {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.currentEquity = equity

	// Update peak and drawdown
	if equity > t.peakEquity {
		t.peakEquity = equity
	}
	dd := (t.peakEquity - equity) / t.peakEquity
	if dd > t.maxDrawdown {
		t.maxDrawdown = dd
	}

	// Record P&L point (throttle to once per second)
	now := time.Now().Unix()
	if len(t.pnlHistory) == 0 || t.pnlHistory[len(t.pnlHistory)-1].Timestamp < now {
		t.pnlHistory = append(t.pnlHistory, PnLPoint{
			Timestamp: now,
			Equity:    equity,
			PnL:       equity - t.dayStartEquity,
		})
		// Keep last 8 hours of second-level data
		if len(t.pnlHistory) > 28800 {
			t.pnlHistory = t.pnlHistory[len(t.pnlHistory)-28800:]
		}
	}

	// Daily return tracking
	today := time.Now().Day()
	if today != t.lastResetDay && t.lastResetDay != 0 {
		// New day — record yesterday's return
		if t.dayStartEquity > 0 {
			dailyReturn := (equity - t.dayStartEquity) / t.dayStartEquity
			t.dailyReturns[t.returnIdx%60] = dailyReturn
			t.returnIdx++
			if t.returnCount < 60 {
				t.returnCount++
			}
		}
		t.dayStartEquity = equity
	}
	if t.lastResetDay == 0 {
		t.dayStartEquity = equity
	}
	t.lastResetDay = today
}

// UpdatePosition updates a position's market value
func (t *Tracker) UpdatePosition(symbol string, marketValue float64) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if marketValue == 0 {
		delete(t.positions, symbol)
	} else {
		t.positions[symbol] = marketValue
	}
}

// GetSnapshot returns the current risk metrics
func (t *Tracker) GetSnapshot() Snapshot {
	t.mu.RLock()
	defer t.mu.RUnlock()

	snap := Snapshot{
		MaxDrawdown: t.maxDrawdown * 100,
		TotalTrades: t.totalTrades,
		Equity:      t.currentEquity,
		DailyPnL:    t.currentEquity - t.dayStartEquity,
		TotalPnL:    t.currentEquity - t.pnlStartEquity(),
		Positions:   make(map[string]float64, len(t.positions)),
		PnLHistory:  t.pnlHistory,
	}

	// Current drawdown
	if t.peakEquity > 0 {
		snap.CurrentDD = (t.peakEquity - t.currentEquity) / t.peakEquity * 100
	}

	// Win rate
	if t.totalTrades > 0 {
		snap.WinRate = float64(t.winTrades) / float64(t.totalTrades) * 100
	}

	// Sharpe ratio (annualized)
	snap.SharpeRatio = t.computeSharpe()

	// VaR (95th percentile)
	snap.DailyVaR95 = t.computeVaR95()

	// Concentration
	snap.Concentration, snap.TopPosition = t.computeConcentration()

	// Copy positions
	for k, v := range t.positions {
		snap.Positions[k] = v
	}

	return snap
}

// -----------------------------------------------------------------------
// Computations
// -----------------------------------------------------------------------

func (t *Tracker) computeSharpe() float64 {
	if t.returnCount < 2 {
		return 0
	}

	// Mean daily return
	var sum float64
	for i := 0; i < t.returnCount; i++ {
		sum += t.dailyReturns[i]
	}
	mean := sum / float64(t.returnCount)

	// Standard deviation
	var variance float64
	for i := 0; i < t.returnCount; i++ {
		diff := t.dailyReturns[i] - mean
		variance += diff * diff
	}
	variance /= float64(t.returnCount - 1)
	stddev := math.Sqrt(variance)

	if stddev == 0 {
		return 0
	}

	// Annualize: Sharpe = (mean * 252) / (stddev * sqrt(252))
	return (mean * 252) / (stddev * math.Sqrt(252))
}

func (t *Tracker) computeVaR95() float64 {
	if t.returnCount < 5 || t.currentEquity == 0 {
		return 0
	}

	// Parametric VaR: VaR = μ - 1.645σ (95% confidence)
	var sum float64
	for i := 0; i < t.returnCount; i++ {
		sum += t.dailyReturns[i]
	}
	mean := sum / float64(t.returnCount)

	var variance float64
	for i := 0; i < t.returnCount; i++ {
		diff := t.dailyReturns[i] - mean
		variance += diff * diff
	}
	stddev := math.Sqrt(variance / float64(t.returnCount-1))

	// VaR as dollar amount
	varPct := mean - 1.645*stddev
	return math.Abs(varPct * t.currentEquity)
}

func (t *Tracker) computeConcentration() (float64, string) {
	if len(t.positions) == 0 || t.currentEquity == 0 {
		return 0, ""
	}

	var maxVal float64
	var maxSym string
	for sym, val := range t.positions {
		absVal := math.Abs(val)
		if absVal > maxVal {
			maxVal = absVal
			maxSym = sym
		}
	}

	return (maxVal / t.currentEquity) * 100, maxSym
}

func (t *Tracker) pnlStartEquity() float64 {
	if len(t.pnlHistory) > 0 {
		return t.pnlHistory[0].Equity
	}
	return t.dayStartEquity
}
