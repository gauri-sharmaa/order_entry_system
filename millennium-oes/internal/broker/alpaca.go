package broker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/millennium-oes/internal/order"
)

// Config holds Alpaca API credentials and endpoints
type Config struct {
	APIKey    string
	APISecret string
	BaseURL   string // https://paper-api.alpaca.markets
	StreamURL string // wss://paper-api.alpaca.markets/stream
}

// AlpacaClient implements order.BrokerClient
type AlpacaClient struct {
	cfg        Config
	httpClient *http.Client

	// WebSocket for real-time updates
	wsMu   sync.Mutex
	wsConn *websocket.Conn

	fills chan order.FillEvent
}

func NewAlpacaClient(cfg Config) *AlpacaClient {
	return &AlpacaClient{
		cfg: cfg,
		httpClient: &http.Client{
			Timeout: 5 * time.Second,
			Transport: &http.Transport{
				MaxIdleConns:        100,
				MaxIdleConnsPerHost: 100,
				IdleConnTimeout:     90 * time.Second,
				// Keep-alive for persistent connections — critical for latency
				DisableKeepAlives: false,
			},
		},
		fills: make(chan order.FillEvent, 1024),
	}
}

// Connect establishes the WebSocket connection for real-time order updates
func (a *AlpacaClient) Connect(ctx context.Context) error {
	dialer := websocket.Dialer{
		HandshakeTimeout: 5 * time.Second,
	}

	conn, _, err := dialer.DialContext(ctx, a.cfg.StreamURL, http.Header{
		"APCA-API-KEY-ID":     []string{a.cfg.APIKey},
		"APCA-API-SECRET-KEY": []string{a.cfg.APISecret},
	})
	if err != nil {
		return fmt.Errorf("websocket dial: %w", err)
	}

	a.wsMu.Lock()
	a.wsConn = conn
	a.wsMu.Unlock()

	// Authenticate
	authMsg := map[string]interface{}{
		"action": "authenticate",
		"data": map[string]string{
			"key_id":     a.cfg.APIKey,
			"secret_key": a.cfg.APISecret,
		},
	}
	if err := conn.WriteJSON(authMsg); err != nil {
		return fmt.Errorf("ws auth: %w", err)
	}

	// Subscribe to trade updates
	listenMsg := map[string]interface{}{
		"action": "listen",
		"data": map[string][]string{
			"streams": {"trade_updates"},
		},
	}
	if err := conn.WriteJSON(listenMsg); err != nil {
		return fmt.Errorf("ws subscribe: %w", err)
	}

	// Start reading in background
	go a.readStream(ctx)

	log.Println("[ALPACA] WebSocket connected and subscribed to trade_updates")
	return nil
}

// readStream processes incoming WebSocket messages
func (a *AlpacaClient) readStream(ctx context.Context) {
	defer func() {
		a.wsMu.Lock()
		if a.wsConn != nil {
			a.wsConn.Close()
		}
		a.wsMu.Unlock()
	}()

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		a.wsMu.Lock()
		conn := a.wsConn
		a.wsMu.Unlock()

		_, msg, err := conn.ReadMessage()
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Printf("[ALPACA] WebSocket read error: %v — reconnecting in 1s", err)
			time.Sleep(time.Second)
			a.reconnect(ctx)
			continue
		}

		a.handleStreamMessage(msg)
	}
}

func (a *AlpacaClient) handleStreamMessage(raw []byte) {
	var envelope struct {
		Stream string          `json:"stream"`
		Data   json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return
	}

	if envelope.Stream != "trade_updates" {
		return
	}

	var update alpacaTradeUpdate
	if err := json.Unmarshal(envelope.Data, &update); err != nil {
		log.Printf("[ALPACA] Failed to parse trade update: %v", err)
		return
	}

	// Only emit fill events
	if update.Event != "fill" && update.Event != "partial_fill" {
		return
	}

	filledQty := update.Order.FilledQty
	filledPrice := update.Order.FilledAvgPrice

	a.fills <- order.FillEvent{
		BrokerOrderID: update.Order.ID,
		FilledQty:     filledQty,
		FilledPrice:   filledPrice,
		Timestamp:     time.Now(),
	}
}

func (a *AlpacaClient) reconnect(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		if err := a.Connect(ctx); err != nil {
			log.Printf("[ALPACA] Reconnect failed: %v — retrying in 2s", err)
			time.Sleep(2 * time.Second)
			continue
		}
		return
	}
}

// -----------------------------------------------------------------------
// BrokerClient interface implementation
// -----------------------------------------------------------------------

func (a *AlpacaClient) SubmitOrder(ctx context.Context, o *order.Order) (string, error) {
	payload := a.buildAlpacaPayload(o)

	body, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}

	resp, err := a.post(ctx, "/v2/orders", body)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("alpaca submit error %d: %s", resp.StatusCode, string(b))
	}

	var result struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", err
	}

	return result.ID, nil
}

func (a *AlpacaClient) CancelOrder(ctx context.Context, brokerOrderID string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete,
		a.cfg.BaseURL+"/v2/orders/"+brokerOrderID, nil)
	if err != nil {
		return err
	}
	a.setHeaders(req)

	resp, err := a.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("alpaca cancel error %d: %s", resp.StatusCode, string(b))
	}
	return nil
}

func (a *AlpacaClient) ReplaceOrder(ctx context.Context, brokerOrderID string, req order.ReplaceRequest) (string, error) {
	payload := map[string]interface{}{}
	if req.Qty != nil {
		payload["qty"] = fmt.Sprintf("%.0f", *req.Qty)
	}
	if req.LimitPrice != nil {
		payload["limit_price"] = fmt.Sprintf("%.2f", *req.LimitPrice)
	}
	if req.StopPrice != nil {
		payload["stop_price"] = fmt.Sprintf("%.2f", *req.StopPrice)
	}
	if req.TrailValue != nil {
		payload["trail"] = fmt.Sprintf("%.2f", *req.TrailValue)
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPatch,
		a.cfg.BaseURL+"/v2/orders/"+brokerOrderID, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	a.setHeaders(httpReq)
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := a.httpClient.Do(httpReq)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("alpaca replace error %d: %s", resp.StatusCode, string(b))
	}

	var result struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", err
	}
	return result.ID, nil
}

func (a *AlpacaClient) FillEvents() <-chan order.FillEvent {
	return a.fills
}

// GetQuote fetches the latest quote for a symbol
func (a *AlpacaClient) GetQuote(ctx context.Context, symbol string) (float64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"https://data.alpaca.markets/v2/stocks/"+symbol+"/quotes/latest", nil)
	if err != nil {
		return 0, err
	}
	a.setHeaders(req)

	resp, err := a.httpClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	var result struct {
		Quote struct {
			AskPrice float64 `json:"ap"`
			BidPrice float64 `json:"bp"`
		} `json:"quote"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return 0, err
	}

	// Return midpoint
	return (result.Quote.AskPrice + result.Quote.BidPrice) / 2, nil
}

// GetPositions fetches current positions from Alpaca
func (a *AlpacaClient) GetPositions(ctx context.Context) ([]*Position, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		a.cfg.BaseURL+"/v2/positions", nil)
	if err != nil {
		return nil, err
	}
	a.setHeaders(req)

	resp, err := a.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var raw []struct {
		Symbol        string `json:"symbol"`
		Qty           string `json:"qty"`
		AvgEntryPrice string `json:"avg_entry_price"`
		MarketValue   string `json:"market_value"`
		UnrealizedPnL string `json:"unrealized_pl"`
		CurrentPrice  string `json:"current_price"`
		Side          string `json:"side"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, err
	}

	positions := make([]*Position, 0, len(raw))
	for _, r := range raw {
		p := &Position{
			Symbol: r.Symbol,
			Side:   r.Side,
		}
		fmt.Sscanf(r.Qty, "%f", &p.Qty)
		fmt.Sscanf(r.AvgEntryPrice, "%f", &p.AvgEntryPrice)
		fmt.Sscanf(r.MarketValue, "%f", &p.MarketValue)
		fmt.Sscanf(r.UnrealizedPnL, "%f", &p.UnrealizedPnL)
		fmt.Sscanf(r.CurrentPrice, "%f", &p.CurrentPrice)
		positions = append(positions, p)
	}
	return positions, nil
}

// GetAccount fetches account info
func (a *AlpacaClient) GetAccount(ctx context.Context) (*Account, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		a.cfg.BaseURL+"/v2/account", nil)
	if err != nil {
		return nil, err
	}
	a.setHeaders(req)

	resp, err := a.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var raw struct {
		ID          string `json:"id"`
		Equity      string `json:"equity"`
		Cash        string `json:"cash"`
		BuyingPower string `json:"buying_power"`
		Status      string `json:"status"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, err
	}

	acct := &Account{ID: raw.ID, Status: raw.Status}
	fmt.Sscanf(raw.Equity, "%f", &acct.Equity)
	fmt.Sscanf(raw.Cash, "%f", &acct.Cash)
	fmt.Sscanf(raw.BuyingPower, "%f", &acct.BuyingPower)
	return acct, nil
}

// -----------------------------------------------------------------------
// Helpers
// -----------------------------------------------------------------------

func (a *AlpacaClient) buildAlpacaPayload(o *order.Order) map[string]interface{} {
	p := map[string]interface{}{
		"symbol":        o.Symbol,
		"side":          string(o.Side),
		"time_in_force": string(o.TimeInForce),
		"client_order_id": o.ClientOrderID,
		"extended_hours": o.ExtendedHours,
	}

	if o.Qty > 0 {
		p["qty"] = fmt.Sprintf("%.0f", o.Qty)
	} else if o.Notional > 0 {
		p["notional"] = fmt.Sprintf("%.2f", o.Notional)
	}

	// Map our order types to Alpaca's types
	switch o.Type {
	case order.TypeMarket:
		p["type"] = "market"
	case order.TypeLimit:
		p["type"] = "limit"
		p["limit_price"] = fmt.Sprintf("%.2f", *o.LimitPrice)
	case order.TypeStop:
		p["type"] = "stop"
		p["stop_price"] = fmt.Sprintf("%.2f", *o.StopPrice)
	case order.TypeStopLimit:
		p["type"] = "stop_limit"
		p["stop_price"] = fmt.Sprintf("%.2f", *o.StopPrice)
		p["limit_price"] = fmt.Sprintf("%.2f", *o.LimitPrice)
	case order.TypeTrailingStop:
		p["type"] = "trailing_stop"
		if o.TrailType == order.TrailingTypePercent {
			p["trail_percent"] = fmt.Sprintf("%.2f", *o.TrailValue)
		} else {
			p["trail_price"] = fmt.Sprintf("%.2f", *o.TrailValue)
		}
	case order.TypeMOO:
		p["type"] = "market"
		p["time_in_force"] = "opg"
	case order.TypeMOC:
		p["type"] = "market"
		p["time_in_force"] = "cls"
	case order.TypeLOO:
		p["type"] = "limit"
		p["time_in_force"] = "opg"
		p["limit_price"] = fmt.Sprintf("%.2f", *o.LimitPrice)
	case order.TypeLOC:
		p["type"] = "limit"
		p["time_in_force"] = "cls"
		p["limit_price"] = fmt.Sprintf("%.2f", *o.LimitPrice)
	default:
		p["type"] = "market"
	}

	return p
}

func (a *AlpacaClient) post(ctx context.Context, path string, body []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		a.cfg.BaseURL+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	a.setHeaders(req)
	req.Header.Set("Content-Type", "application/json")
	return a.httpClient.Do(req)
}

func (a *AlpacaClient) setHeaders(req *http.Request) {
	req.Header.Set("APCA-API-KEY-ID", a.cfg.APIKey)
	req.Header.Set("APCA-API-SECRET-KEY", a.cfg.APISecret)
}

// -----------------------------------------------------------------------
// Alpaca internal response types
// -----------------------------------------------------------------------

type alpacaTradeUpdate struct {
	Event string `json:"event"`
	Order struct {
		ID             string  `json:"id"`
		FilledQty      float64 `json:"filled_qty,string"`
		FilledAvgPrice float64 `json:"filled_avg_price,string"`
	} `json:"order"`
}
