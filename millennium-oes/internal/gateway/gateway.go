// Package gateway provides the HTTP server serving both:
//   - Execution Desk (order entry, blotter, fills)
//   - Investor Dashboard (portfolio, risk metrics, P&L, signals)

package gateway

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/millennium-oes/internal/broker/alpaca"
	"github.com/millennium-oes/internal/engine"
	"github.com/millennium-oes/internal/risk"
	"github.com/millennium-oes/internal/signal"
)

// Config for the gateway
type Config struct {
	Engine      *engine.Engine
	RiskTracker *risk.Tracker
	SignalModel *signal.Model
	Alpaca      *alpaca.Client // nil if not using Alpaca
	Port        int
}

// Gateway is the HTTP server
type Gateway struct {
	eng    *engine.Engine
	risk   *risk.Tracker
	signal *signal.Model
	alpaca *alpaca.Client
	port   int
}

// New creates a gateway
func New(cfg Config) *Gateway {
	return &Gateway{
		eng:    cfg.Engine,
		risk:   cfg.RiskTracker,
		signal: cfg.SignalModel,
		alpaca: cfg.Alpaca,
		port:   cfg.Port,
	}
}

// Start begins serving HTTP
func (g *Gateway) Start() {
	mux := http.NewServeMux()

	// -----------------------------------------------------------------------
	// Execution Desk API (order management)
	// -----------------------------------------------------------------------
	mux.HandleFunc("POST /api/orders", g.submitOrder)
	mux.HandleFunc("GET /api/orders", g.listOrders)
	mux.HandleFunc("GET /api/orders/{id}", g.getOrder)
	mux.HandleFunc("DELETE /api/orders/{id}", g.cancelOrder)

	// -----------------------------------------------------------------------
	// Investor Dashboard API (portfolio, risk, signals)
	// -----------------------------------------------------------------------
	mux.HandleFunc("GET /api/portfolio", g.getPortfolio)
	mux.HandleFunc("GET /api/risk", g.getRiskMetrics)
	mux.HandleFunc("GET /api/signal/{symbol}", g.getSignal)
	mux.HandleFunc("GET /api/quote/{symbol}", g.getQuote)
	mux.HandleFunc("GET /api/account", g.getAccount)

	// -----------------------------------------------------------------------
	// System
	// -----------------------------------------------------------------------
	mux.HandleFunc("GET /api/stats", g.getStats)
	mux.HandleFunc("POST /api/risk/killswitch", g.toggleKillSwitch)
	mux.HandleFunc("GET /api/stream", g.stream)

	// -----------------------------------------------------------------------
	// Static files (serves both views)
	// -----------------------------------------------------------------------
	mux.Handle("/", http.FileServer(http.Dir("./web")))

	addr := fmt.Sprintf(":%d", g.port)
	srv := &http.Server{
		Addr:         addr,
		Handler:      cors(mux),
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 30 * time.Second,
	}

	log.Printf("[HTTP] Listening on %s", addr)
	if err := srv.ListenAndServe(); err != nil {
		log.Fatalf("[HTTP] %v", err)
	}
}

// -----------------------------------------------------------------------
// Execution Desk handlers
// -----------------------------------------------------------------------

func (g *Gateway) submitOrder(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Symbol    string  `json:"symbol"`
		Side      string  `json:"side"`
		Type      string  `json:"type"`
		Qty       int     `json:"qty"`
		Price     float64 `json:"price,omitempty"`
		StopPrice float64 `json:"stop_price,omitempty"`
		TIF       string  `json:"time_in_force"`
		TrailType string  `json:"trail_type,omitempty"`
		TrailVal  float64 `json:"trail_value,omitempty"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, "invalid request: "+err.Error())
		return
	}
	if req.Symbol == "" || req.Qty <= 0 {
		writeErr(w, 400, "symbol and qty required")
		return
	}

	ev := engine.Event{
		Symbol:    engine.SymbolFromString(req.Symbol),
		Side:      parseSide(req.Side),
		OrdType:   parseOrdType(req.Type),
		TIF:       parseTIF(req.TIF),
		Qty:       int32(req.Qty),
		Price:     engine.PriceToMicros(req.Price),
		StopPrice: engine.PriceToMicros(req.StopPrice),
		TrailType: parseTrailType(req.TrailType),
		TrailVal:  engine.PriceToMicros(req.TrailVal),
	}

	idx, err := g.eng.Submit(ev)
	if err != nil {
		writeErr(w, 403, err.Error())
		return
	}

	writeJSON(w, 201, map[string]interface{}{"id": idx, "status": "submitted"})
}

func (g *Gateway) getOrder(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseUint(r.PathValue("id"), 10, 32)
	o, ok := g.eng.GetOrder(uint32(id))
	if !ok {
		writeErr(w, 404, "not found")
		return
	}
	writeJSON(w, 200, orderToJSON(o))
}

func (g *Gateway) listOrders(w http.ResponseWriter, r *http.Request) {
	indices := g.eng.ListActive()
	orders := make([]map[string]interface{}, 0, len(indices))
	for _, idx := range indices {
		if o, ok := g.eng.GetOrder(idx); ok {
			orders = append(orders, orderToJSON(o))
		}
	}
	writeJSON(w, 200, orders)
}

func (g *Gateway) cancelOrder(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseUint(r.PathValue("id"), 10, 32)
	if err := g.eng.Cancel(uint32(id)); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, map[string]string{"status": "cancel_submitted"})
}

// -----------------------------------------------------------------------
// Investor Dashboard handlers
// -----------------------------------------------------------------------

func (g *Gateway) getPortfolio(w http.ResponseWriter, r *http.Request) {
	if g.alpaca != nil {
		positions, err := g.alpaca.GetPositions()
		if err == nil {
			writeJSON(w, 200, positions)
			return
		}
	}
	// Fallback: return positions from risk tracker
	snap := g.risk.GetSnapshot()
	writeJSON(w, 200, snap.Positions)
}

func (g *Gateway) getRiskMetrics(w http.ResponseWriter, r *http.Request) {
	snap := g.risk.GetSnapshot()
	snap.PnLHistory = nil // don't send full history on this endpoint (use /api/risk/pnl)
	result := map[string]interface{}{
		"sharpe_ratio":     snap.SharpeRatio,
		"max_drawdown_pct": snap.MaxDrawdown,
		"current_dd_pct":   snap.CurrentDD,
		"daily_var_95":     snap.DailyVaR95,
		"concentration_pct": snap.Concentration,
		"top_position":     snap.TopPosition,
		"win_rate_pct":     snap.WinRate,
		"total_trades":     snap.TotalTrades,
		"daily_pnl":        snap.DailyPnL,
		"total_pnl":        snap.TotalPnL,
		"equity":           snap.Equity,
		"kill_switch":      g.eng.IsKillSwitchActive(),
	}
	writeJSON(w, 200, result)
}

func (g *Gateway) getSignal(w http.ResponseWriter, r *http.Request) {
	symbol := r.PathValue("symbol")

	// In a real system, you'd have a price buffer per symbol.
	// For demo, generate a synthetic signal or fetch from Alpaca.
	if g.alpaca == nil {
		writeJSON(w, 200, map[string]interface{}{
			"symbol":     symbol,
			"direction":  0,
			"confidence": 0,
			"message":    "no market data available (simulation mode)",
		})
		return
	}

	// Fetch recent price (simplified — real system would buffer ticks)
	price, err := g.alpaca.GetQuote(symbol)
	if err != nil || price == 0 {
		writeJSON(w, 200, map[string]interface{}{
			"symbol":    symbol,
			"direction": 0,
			"message":   "could not fetch quote",
		})
		return
	}

	// Generate synthetic price history around current price for demo
	// In production, you'd maintain a rolling buffer of real prices
	prices := syntheticPrices(price, 25)
	volumes := syntheticVolumes(25)

	sig := g.signal.Predict(prices, volumes)

	dirStr := "neutral"
	if sig.Direction > 0 {
		dirStr = "bullish"
	} else if sig.Direction < 0 {
		dirStr = "bearish"
	}

	writeJSON(w, 200, map[string]interface{}{
		"symbol":      symbol,
		"price":       price,
		"direction":   dirStr,
		"confidence":  sig.Confidence,
		"pred_return": sig.PredReturn,
	})
}

func (g *Gateway) getQuote(w http.ResponseWriter, r *http.Request) {
	symbol := r.PathValue("symbol")
	if g.alpaca == nil {
		writeErr(w, 503, "no broker connected")
		return
	}
	price, err := g.alpaca.GetQuote(symbol)
	if err != nil {
		writeErr(w, 502, err.Error())
		return
	}
	writeJSON(w, 200, map[string]interface{}{"symbol": symbol, "price": price})
}

func (g *Gateway) getAccount(w http.ResponseWriter, r *http.Request) {
	if g.alpaca == nil {
		writeJSON(w, 200, map[string]interface{}{
			"equity": g.risk.GetSnapshot().Equity,
			"status": "simulation",
		})
		return
	}
	acct, err := g.alpaca.GetAccount()
	if err != nil {
		writeErr(w, 502, err.Error())
		return
	}
	writeJSON(w, 200, acct)
}

// -----------------------------------------------------------------------
// System handlers
// -----------------------------------------------------------------------

func (g *Gateway) getStats(w http.ResponseWriter, r *http.Request) {
	processed, avgLatency := g.eng.Stats()
	writeJSON(w, 200, map[string]interface{}{
		"orders_processed": processed,
		"avg_latency_ns":   avgLatency,
		"avg_latency_us":   float64(avgLatency) / 1000.0,
	})
}

func (g *Gateway) toggleKillSwitch(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Active bool `json:"active"`
	}
	json.NewDecoder(r.Body).Decode(&body)
	g.eng.SetKillSwitch(body.Active)
	writeJSON(w, 200, map[string]bool{"active": body.Active})
}

func (g *Gateway) stream(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, 500, "streaming not supported")
		return
	}

	fmt.Fprintf(w, "event: connected\ndata: {}\n\n")
	flusher.Flush()

	subID, updates := g.eng.Subscribe()
	defer g.eng.Unsubscribe(subID)

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case update, ok := <-updates:
			if !ok {
				return
			}
			o, exists := g.eng.GetOrder(update.Idx)
			if !exists {
				continue
			}
			data, _ := json.Marshal(orderToJSON(o))
			fmt.Fprintf(w, "event: order\ndata: %s\n\n", data)
			flusher.Flush()
		}
	}
}

// -----------------------------------------------------------------------
// Helpers
// -----------------------------------------------------------------------

func orderToJSON(o engine.Order) map[string]interface{} {
	return map[string]interface{}{
		"id":            o.ID,
		"symbol":        engine.SymbolToString(o.Symbol),
		"side":          sideStr(o.Side),
		"type":          ordTypeStr(o.Type),
		"tif":           tifStr(o.TIF),
		"qty":           o.Qty,
		"filled_qty":    o.FilledQty,
		"price":         engine.MicrosToPrice(o.Price),
		"stop_price":    engine.MicrosToPrice(o.StopPrice),
		"filled_avg_px": engine.MicrosToPrice(o.FilledAvgPx),
		"status":        statusStr(o.Status),
		"created_at":    time.Unix(0, o.CreatedAt).Format(time.RFC3339Nano),
	}
}

func syntheticPrices(current float64, n int) []float64 {
	prices := make([]float64, n)
	for i := range prices {
		// Slight random walk backward from current price
		offset := float64(n-i) * 0.001 * current
		prices[i] = current - offset
	}
	prices[n-1] = current
	return prices
}

func syntheticVolumes(n int) []float64 {
	vols := make([]float64, n)
	for i := range vols {
		vols[i] = 1000000 + float64(i)*10000
	}
	return vols
}

func parseSide(s string) engine.Side {
	switch s {
	case "sell":
		return engine.SideSell
	case "sell_short", "short":
		return engine.SideShort
	case "buy_to_cover", "cover":
		return engine.SideCover
	default:
		return engine.SideBuy
	}
}

func parseOrdType(s string) engine.OrderType {
	switch s {
	case "LIMIT":
		return engine.OrdLimit
	case "STOP":
		return engine.OrdStop
	case "STOP_LIMIT":
		return engine.OrdStopLimit
	case "TRAILING_STOP":
		return engine.OrdTrailingStop
	case "MOO", "MARKET_ON_OPEN":
		return engine.OrdMOO
	case "MOC", "MARKET_ON_CLOSE":
		return engine.OrdMOC
	case "BRACKET":
		return engine.OrdBracket
	case "OCO":
		return engine.OrdOCO
	case "TWAP":
		return engine.OrdTWAP
	case "VWAP":
		return engine.OrdVWAP
	case "ICEBERG":
		return engine.OrdIceberg
	default:
		return engine.OrdMarket
	}
}

func parseTIF(s string) engine.TimeInForce {
	switch s {
	case "gtc":
		return engine.TIFGTC
	case "ioc":
		return engine.TIFIOC
	case "fok":
		return engine.TIFFOK
	default:
		return engine.TIFDay
	}
}

func parseTrailType(s string) engine.TrailType {
	switch s {
	case "percent":
		return engine.TrailPercent
	case "price", "amount":
		return engine.TrailAmount
	default:
		return engine.TrailNone
	}
}

func sideStr(s engine.Side) string {
	switch s {
	case engine.SideSell:
		return "sell"
	case engine.SideShort:
		return "sell_short"
	case engine.SideCover:
		return "buy_to_cover"
	default:
		return "buy"
	}
}

func ordTypeStr(t engine.OrderType) string {
	types := map[engine.OrderType]string{
		engine.OrdMarket: "MARKET", engine.OrdLimit: "LIMIT", engine.OrdStop: "STOP",
		engine.OrdStopLimit: "STOP_LIMIT", engine.OrdTrailingStop: "TRAILING_STOP",
		engine.OrdMOO: "MOO", engine.OrdMOC: "MOC", engine.OrdBracket: "BRACKET",
		engine.OrdOCO: "OCO", engine.OrdTWAP: "TWAP", engine.OrdVWAP: "VWAP",
		engine.OrdIceberg: "ICEBERG",
	}
	if s, ok := types[t]; ok {
		return s
	}
	return "MARKET"
}

func tifStr(t engine.TimeInForce) string {
	switch t {
	case engine.TIFGTC:
		return "gtc"
	case engine.TIFIOC:
		return "ioc"
	case engine.TIFFOK:
		return "fok"
	default:
		return "day"
	}
}

func statusStr(s engine.Status) string {
	names := map[engine.Status]string{
		engine.StatusNew: "new", engine.StatusPendingNew: "pending_new",
		engine.StatusAcknowledged: "acknowledged", engine.StatusPartialFill: "partially_filled",
		engine.StatusFilled: "filled", engine.StatusCancelled: "cancelled",
		engine.StatusRejected: "rejected", engine.StatusHeld: "held",
	}
	if n, ok := names[s]; ok {
		return n
	}
	return "unknown"
}

func cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, PATCH, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		if r.Method == "OPTIONS" {
			w.WriteHeader(204)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
