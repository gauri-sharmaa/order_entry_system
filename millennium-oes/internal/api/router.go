package api

import (
	"net/http"

	"github.com/millennium-oes/internal/broker"
	"github.com/millennium-oes/internal/order"
	"github.com/millennium-oes/internal/risk"
)

// Deps holds all dependencies injected into the API layer
type Deps struct {
	Engine     *order.Engine
	RiskEngine *risk.Engine
	Broker     broker.Client // interface — works with Alpaca or FIX
}

// NewRouter wires up all HTTP routes
func NewRouter(deps Deps) http.Handler {
	h := &Handler{deps: deps}

	mux := http.NewServeMux()

	// Order endpoints
	mux.HandleFunc("POST /api/orders", h.SubmitOrder)
	mux.HandleFunc("GET /api/orders", h.ListOrders)
	mux.HandleFunc("GET /api/orders/{id}", h.GetOrder)
	mux.HandleFunc("DELETE /api/orders/{id}", h.CancelOrder)
	mux.HandleFunc("PATCH /api/orders/{id}", h.ReplaceOrder)
	mux.HandleFunc("DELETE /api/orders", h.CancelAllOrders)

	// Market data
	mux.HandleFunc("GET /api/quote/{symbol}", h.GetQuote)

	// Portfolio
	mux.HandleFunc("GET /api/positions", h.GetPositions)
	mux.HandleFunc("GET /api/account", h.GetAccount)

	// Risk
	mux.HandleFunc("GET /api/risk", h.GetRiskStatus)
	mux.HandleFunc("POST /api/risk/killswitch", h.ToggleKillSwitch)

	// Server-Sent Events for real-time order updates
	mux.HandleFunc("GET /api/stream", h.StreamOrders)

	// Static web UI
	mux.Handle("/", http.FileServer(http.Dir("./web")))

	return corsMiddleware(loggingMiddleware(mux))
}
