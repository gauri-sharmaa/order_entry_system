// Package alpaca implements the Alpaca Markets broker client.
// Used for live paper trading demos with real market data.
// Satisfies the engine.Broker interface.

package alpaca

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/millennium-oes/internal/engine"
)

// Config for Alpaca
type Config struct {
	APIKey    string
	APISecret string
	BaseURL   string // https://paper-api.alpaca.markets
	DataURL   string // https://data.alpaca.markets
}

// Client implements engine.Broker for Alpaca
type Client struct {
	cfg        Config
	httpClient *http.Client

	// Map our order IDs to Alpaca order IDs for cancel/fill matching
	mu      sync.RWMutex
	idMap   map[uint32]string // our ID → alpaca ID
	revMap  map[string]uint32 // alpaca ID → our ID

	// Track which fills we've already processed (prevent duplicates)
	filled  map[string]bool // alpaca order ID → already injected

	eng *engine.Engine
}

// New creates an Alpaca client
func New(cfg Config) *Client {
	if cfg.BaseURL == "" {
		cfg.BaseURL = "https://paper-api.alpaca.markets"
	}
	if cfg.DataURL == "" {
		cfg.DataURL = "https://data.alpaca.markets"
	}
	return &Client{
		cfg: cfg,
		httpClient: &http.Client{
			Timeout: 5 * time.Second,
			Transport: &http.Transport{
				MaxIdleConns:        50,
				MaxIdleConnsPerHost: 50,
				IdleConnTimeout:     90 * time.Second,
				DisableKeepAlives:   false,
			},
		},
		idMap:  make(map[uint32]string, 1024),
		revMap: make(map[string]uint32, 1024),
		filled: make(map[string]bool, 1024),
	}
}

func (c *Client) SetEngine(eng *engine.Engine) { c.eng = eng }

// Submit sends an order to Alpaca
func (c *Client) Submit(o *engine.Order) error {
	payload := map[string]interface{}{
		"symbol":         engine.SymbolToString(o.Symbol),
		"side":           alpacaSide(o.Side),
		"type":           alpacaOrdType(o.Type),
		"time_in_force":  alpacaTIF(o.TIF),
		"qty":            strconv.Itoa(int(o.Qty)),
		"extended_hours": true,
	}

	if o.Price > 0 {
		payload["limit_price"] = fmt.Sprintf("%.2f", engine.MicrosToPrice(o.Price))
	}
	if o.StopPrice > 0 {
		payload["stop_price"] = fmt.Sprintf("%.2f", engine.MicrosToPrice(o.StopPrice))
	}

	body, _ := json.Marshal(payload)
	req, err := http.NewRequest("POST", c.cfg.BaseURL+"/v2/orders", bytes.NewReader(body))
	if err != nil {
		return err
	}
	c.setHeaders(req)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("alpaca submit: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("alpaca %d: %s", resp.StatusCode, string(b))
	}

	var result struct {
		ID string `json:"id"`
	}
	json.NewDecoder(resp.Body).Decode(&result)

	// Store mapping
	c.mu.Lock()
	c.idMap[o.ID] = result.ID
	c.revMap[result.ID] = o.ID
	c.mu.Unlock()

	// Copy broker ID into order
	copy(o.BrokerID[:], result.ID)

	return nil
}

// Cancel cancels an order on Alpaca
func (c *Client) Cancel(brokerID [16]byte) error {
	alpacaID := trimNull(brokerID[:])
	if alpacaID == "" {
		return fmt.Errorf("no broker ID")
	}

	req, _ := http.NewRequest("DELETE", c.cfg.BaseURL+"/v2/orders/"+alpacaID, nil)
	c.setHeaders(req)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// Replace modifies an order on Alpaca
func (c *Client) Replace(brokerID [16]byte, qty int32, price int64) error {
	alpacaID := trimNull(brokerID[:])
	payload := map[string]interface{}{}
	if qty > 0 {
		payload["qty"] = strconv.Itoa(int(qty))
	}
	if price > 0 {
		payload["limit_price"] = fmt.Sprintf("%.2f", engine.MicrosToPrice(price))
	}

	body, _ := json.Marshal(payload)
	req, _ := http.NewRequest("PATCH", c.cfg.BaseURL+"/v2/orders/"+alpacaID, bytes.NewReader(body))
	c.setHeaders(req)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// GetQuote fetches the latest quote for a symbol
func (c *Client) GetQuote(symbol string) (float64, error) {
	req, _ := http.NewRequest("GET", c.cfg.DataURL+"/v2/stocks/"+symbol+"/quotes/latest", nil)
	c.setHeaders(req)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	var result struct {
		Quote struct {
			Ap float64 `json:"ap"`
			Bp float64 `json:"bp"`
		} `json:"quote"`
	}
	json.NewDecoder(resp.Body).Decode(&result)
	return (result.Quote.Ap + result.Quote.Bp) / 2, nil
}

// GetPositions fetches positions from Alpaca
func (c *Client) GetPositions() ([]Position, error) {
	req, _ := http.NewRequest("GET", c.cfg.BaseURL+"/v2/positions", nil)
	c.setHeaders(req)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var positions []Position
	json.NewDecoder(resp.Body).Decode(&positions)
	return positions, nil
}

// GetAccount fetches account info
func (c *Client) GetAccount() (*Account, error) {
	req, _ := http.NewRequest("GET", c.cfg.BaseURL+"/v2/account", nil)
	c.setHeaders(req)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var acct Account
	json.NewDecoder(resp.Body).Decode(&acct)
	return &acct, nil
}

// PollFills polls for order updates (simpler than WebSocket for a project)
func (c *Client) PollFills(ctx context.Context) {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.checkFills()
		}
	}
}

func (c *Client) checkFills() {
	req, _ := http.NewRequest("GET", c.cfg.BaseURL+"/v2/orders?status=filled&limit=50&direction=desc", nil)
	c.setHeaders(req)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()

	var orders []struct {
		ID          string `json:"id"`
		FilledQty   string `json:"filled_qty"`
		FilledAvgPx string `json:"filled_avg_price"`
	}
	json.NewDecoder(resp.Body).Decode(&orders)

	for _, ao := range orders {
		c.mu.RLock()
		ourID, found := c.revMap[ao.ID]
		alreadyFilled := c.filled[ao.ID]
		c.mu.RUnlock()

		if !found || c.eng == nil || alreadyFilled {
			continue
		}

		qty, _ := strconv.ParseFloat(ao.FilledQty, 64)
		px, _ := strconv.ParseFloat(ao.FilledAvgPx, 64)

		if qty > 0 && px > 0 {
			c.eng.InjectFill(ourID, int32(qty), engine.PriceToMicros(px))
			c.mu.Lock()
			c.filled[ao.ID] = true
			c.mu.Unlock()
		}
	}
}

func (c *Client) setHeaders(req *http.Request) {
	req.Header.Set("APCA-API-KEY-ID", c.cfg.APIKey)
	req.Header.Set("APCA-API-SECRET-KEY", c.cfg.APISecret)
}

// -----------------------------------------------------------------------
// Types
// -----------------------------------------------------------------------

type Position struct {
	Symbol        string `json:"symbol"`
	Qty           string `json:"qty"`
	AvgEntryPrice string `json:"avg_entry_price"`
	MarketValue   string `json:"market_value"`
	UnrealizedPnL string `json:"unrealized_pl"`
	CurrentPrice  string `json:"current_price"`
	Side          string `json:"side"`
}

type Account struct {
	ID          string `json:"id"`
	Equity      string `json:"equity"`
	Cash        string `json:"cash"`
	BuyingPower string `json:"buying_power"`
	Status      string `json:"status"`
}

// -----------------------------------------------------------------------
// Helpers
// -----------------------------------------------------------------------

func alpacaSide(s engine.Side) string {
	switch s {
	case engine.SideSell, engine.SideShort:
		return "sell"
	default:
		return "buy"
	}
}

func alpacaOrdType(t engine.OrderType) string {
	switch t {
	case engine.OrdLimit:
		return "limit"
	case engine.OrdStop:
		return "stop"
	case engine.OrdStopLimit:
		return "stop_limit"
	case engine.OrdTrailingStop:
		return "trailing_stop"
	default:
		return "market"
	}
}

func alpacaTIF(t engine.TimeInForce) string {
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

func trimNull(b []byte) string {
	for i, c := range b {
		if c == 0 {
			return string(b[:i])
		}
	}
	return string(b)
}

// Ensure we log on init
func init() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
}
