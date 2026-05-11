package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/millennium-oes/internal/order"
)

// Handler holds the API handler methods
type Handler struct {
	deps Deps
}

// -----------------------------------------------------------------------
// Order handlers
// -----------------------------------------------------------------------

func (h *Handler) SubmitOrder(w http.ResponseWriter, r *http.Request) {
	var req order.SubmitRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}

	// Fetch market price for risk checks
	marketPrice, err := h.deps.Broker.GetQuote(r.Context(), req.Symbol)
	if err != nil {
		log.Printf("[API] Could not fetch quote for %s: %v", req.Symbol, err)
		marketPrice = 0 // risk engine handles zero gracefully
	}

	// Pre-trade risk check
	if err := h.deps.RiskEngine.Check(req, marketPrice); err != nil {
		writeError(w, http.StatusForbidden, err.Error())
		return
	}

	// Record order attempt for duplicate detection
	h.deps.RiskEngine.RecordOrder(req.Symbol, req.Side, req.Qty)

	// Submit to engine
	o, err := h.deps.Engine.Submit(r.Context(), req, h.deps.Broker)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusCreated, o)
}

func (h *Handler) GetOrder(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	o, err := h.deps.Engine.Get(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, o)
}

func (h *Handler) ListOrders(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	filter := order.ListFilter{
		Symbol: q.Get("symbol"),
		Status: order.Status(q.Get("status")),
		Side:   order.Side(q.Get("side")),
		Limit:  50,
	}

	orders, err := h.deps.Engine.List(r.Context(), filter)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	if orders == nil {
		orders = []*order.Order{}
	}
	writeJSON(w, http.StatusOK, orders)
}

func (h *Handler) CancelOrder(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	o, err := h.deps.Engine.Cancel(r.Context(), id, h.deps.Broker)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, o)
}

func (h *Handler) ReplaceOrder(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	var req order.ReplaceRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}

	o, err := h.deps.Engine.Replace(r.Context(), id, req, h.deps.Broker)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, o)
}

func (h *Handler) CancelAllOrders(w http.ResponseWriter, r *http.Request) {
	orders, err := h.deps.Engine.List(r.Context(), order.ListFilter{Limit: 500})
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	cancelled := 0
	for _, o := range orders {
		if o.Status.IsCancellable() {
			if _, err := h.deps.Engine.Cancel(r.Context(), o.ID, h.deps.Broker); err == nil {
				cancelled++
			}
		}
	}

	writeJSON(w, http.StatusOK, map[string]int{"cancelled": cancelled})
}

// -----------------------------------------------------------------------
// Market data handlers
// -----------------------------------------------------------------------

func (h *Handler) GetQuote(w http.ResponseWriter, r *http.Request) {
	symbol := r.PathValue("symbol")
	price, err := h.deps.Broker.GetQuote(r.Context(), symbol)
	if err != nil {
		writeError(w, http.StatusBadGateway, "failed to fetch quote: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"symbol": symbol,
		"price":  price,
		"time":   time.Now().Format(time.RFC3339),
	})
}

// -----------------------------------------------------------------------
// Portfolio handlers
// -----------------------------------------------------------------------

func (h *Handler) GetPositions(w http.ResponseWriter, r *http.Request) {
	positions, err := h.deps.Broker.GetPositions(r.Context())
	if err != nil {
		writeError(w, http.StatusBadGateway, "failed to fetch positions: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, positions)
}

func (h *Handler) GetAccount(w http.ResponseWriter, r *http.Request) {
	account, err := h.deps.Broker.GetAccount(r.Context())
	if err != nil {
		writeError(w, http.StatusBadGateway, "failed to fetch account: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, account)
}
// -----------------------------------------------------------------------
// Risk handlers
// -----------------------------------------------------------------------

func (h *Handler) GetRiskStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"kill_switch_active": h.deps.RiskEngine.IsKillSwitchActive(),
		"daily_pnl":          h.deps.RiskEngine.GetDailyPnL(),
		"positions":          h.deps.RiskEngine.GetPositions(),
	})
}

func (h *Handler) ToggleKillSwitch(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Active bool `json:"active"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid body")
		return
	}

	if body.Active {
		h.deps.RiskEngine.ActivateKillSwitch()
		log.Println("[RISK] KILL SWITCH ACTIVATED")
	} else {
		h.deps.RiskEngine.DeactivateKillSwitch()
		log.Println("[RISK] Kill switch deactivated")
	}

	writeJSON(w, http.StatusOK, map[string]bool{"active": body.Active})
}

// -----------------------------------------------------------------------
// Server-Sent Events — real-time order updates to the UI
// -----------------------------------------------------------------------

func (h *Handler) StreamOrders(w http.ResponseWriter, r *http.Request) {
	// SSE headers
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // disable nginx buffering

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming not supported")
		return
	}

	subID := uuid.New().String()
	updates := h.deps.Engine.Subscribe(subID)
	defer h.deps.Engine.Unsubscribe(subID)

	// Send initial ping
	fmt.Fprintf(w, "event: connected\ndata: {\"id\":\"%s\"}\n\n", subID)
	flusher.Flush()

	ctx := r.Context()
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-heartbeat.C:
			fmt.Fprintf(w, ": heartbeat\n\n")
			flusher.Flush()
		case o, ok := <-updates:
			if !ok {
				return
			}
			data, err := json.Marshal(o)
			if err != nil {
				continue
			}
			fmt.Fprintf(w, "event: order_update\ndata: %s\n\n", data)
			flusher.Flush()
		}
	}
}

// -----------------------------------------------------------------------
// Helpers
// -----------------------------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// Ensure context is used (suppress unused import)
var _ = context.Background
