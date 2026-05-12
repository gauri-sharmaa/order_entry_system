// Package gateway provides the HTTP server and web UI.
// This runs OFF the hot path in a separate goroutine.
// It's the interface between humans and the engine.

package gateway

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/millennium-oes/internal/engine"
)

// Gateway is the HTTP server
type Gateway struct {
	eng  *engine.Engine
	port int
}

// New creates a gateway
func New(eng *engine.Engine, port int) *Gateway {
	return &Gateway{eng: eng, port: port}
}

// Start begins serving HTTP
func (g *Gateway) Start() {
	mux := http.NewServeMux()

	// Order endpoints
	mux.HandleFunc("POST /api/orders", g.submitOrder)
	mux.HandleFunc("GET /api/orders", g.listOrders)
	mux.HandleFunc("GET /api/orders/{id}", g.getOrder)
	mux.HandleFunc("DELETE /api/orders/{id}", g.cancelOrder)

	// Risk
	mux.HandleFunc("GET /api/risk", g.getRisk)
	mux.HandleFunc("POST /api/risk/killswitch", g.toggleKillSwitch)

	// Stats
	mux.HandleFunc("GET /api/stats", g.getStats)

	// SSE stream
	mux.HandleFunc("GET /api/stream", g.stream)

	// Static web UI
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
		log.Fatalf("[HTTP] Server error: %v", err)
	}
}

// -----------------------------------------------------------------------
// Handlers
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
		writeErr(w, 400, "symbol and qty are required")
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

	writeJSON(w, 201, map[string]interface{}{
		"id":     idx,
		"status": "submitted",
	})
}

func (g *Gateway) getOrder(w http.ResponseWriter, r *http.Request) {
	idStr := r.PathValue("id")
	id, err := strconv.ParseUint(idStr, 10, 32)
	if err != nil {
		writeErr(w, 400, "invalid order ID")
		return
	}

	o, ok := g.eng.GetOrder(uint32(id))
	if !ok {
		writeErr(w, 404, "order not found")
		return
	}

	writeJSON(w, 200, orderToJSON(o))
}

func (g *Gateway) listOrders(w http.ResponseWriter, r *http.Request) {
	indices := g.eng.ListActive()
	orders := make([]map[string]interface{}, 0, len(indices))
	for _, idx := range indices {
		o, ok := g.eng.GetOrder(idx)
		if ok {
			orders = append(orders, orderToJSON(o))
		}
	}
	writeJSON(w, 200, orders)
}

func (g *Gateway) cancelOrder(w http.ResponseWriter, r *http.Request) {
	idStr := r.PathValue("id")
	id, err := strconv.ParseUint(idStr, 10, 32)
	if err != nil {
		writeErr(w, 400, "invalid order ID")
		return
	}

	if err := g.eng.Cancel(uint32(id)); err != nil {
		writeErr(w, 400, err.Error())
		return
	}

	writeJSON(w, 200, map[string]string{"status": "cancel_submitted"})
}

func (g *Gateway) getRisk(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]interface{}{
		"kill_switch": g.eng.IsKillSwitchActive(),
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

func (g *Gateway) getStats(w http.ResponseWriter, r *http.Request) {
	processed, avgLatency := g.eng.Stats()
	writeJSON(w, 200, map[string]interface{}{
		"orders_processed": processed,
		"avg_latency_ns":   avgLatency,
		"avg_latency_us":   float64(avgLatency) / 1000.0,
	})
}

// SSE stream for real-time order updates
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
		"id":           o.ID,
		"symbol":       engine.SymbolToString(o.Symbol),
		"side":         sideStr(o.Side),
		"type":         ordTypeStr(o.Type),
		"tif":          tifStr(o.TIF),
		"qty":          o.Qty,
		"filled_qty":   o.FilledQty,
		"price":        engine.MicrosToPrice(o.Price),
		"stop_price":   engine.MicrosToPrice(o.StopPrice),
		"filled_avg_px": engine.MicrosToPrice(o.FilledAvgPx),
		"status":       statusStr(o.Status),
		"created_at":   time.Unix(0, o.CreatedAt).Format(time.RFC3339Nano),
	}
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
	switch t {
	case engine.OrdLimit:
		return "LIMIT"
	case engine.OrdStop:
		return "STOP"
	case engine.OrdStopLimit:
		return "STOP_LIMIT"
	case engine.OrdTrailingStop:
		return "TRAILING_STOP"
	case engine.OrdMOO:
		return "MOO"
	case engine.OrdMOC:
		return "MOC"
	case engine.OrdBracket:
		return "BRACKET"
	case engine.OrdOCO:
		return "OCO"
	case engine.OrdTWAP:
		return "TWAP"
	case engine.OrdVWAP:
		return "VWAP"
	case engine.OrdIceberg:
		return "ICEBERG"
	default:
		return "MARKET"
	}
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
	switch s {
	case engine.StatusNew:
		return "new"
	case engine.StatusPendingNew:
		return "pending_new"
	case engine.StatusAcknowledged:
		return "acknowledged"
	case engine.StatusPartialFill:
		return "partially_filled"
	case engine.StatusFilled:
		return "filled"
	case engine.StatusCancelled:
		return "cancelled"
	case engine.StatusRejected:
		return "rejected"
	case engine.StatusHeld:
		return "held"
	default:
		return "unknown"
	}
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
