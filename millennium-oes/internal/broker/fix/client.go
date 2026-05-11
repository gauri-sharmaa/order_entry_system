// Package fix implements a FIX 4.2 broker client using QuickFIX/Go,
// targeting Interactive Brokers' FIX gateway.
//
// FIX Protocol primer:
//   - Every message has a MsgType (tag 35). Key types:
//     D  = NewOrderSingle      (we send to place an order)
//     F  = OrderCancelRequest  (we send to cancel)
//     G  = OrderCancelReplaceRequest (we send to modify)
//     8  = ExecutionReport     (broker sends back fills/acks/rejects)
//     9  = OrderCancelReject   (broker sends if cancel fails)
//   - Messages are tag=value pairs delimited by SOH (ASCII 0x01)
//   - Every session has a SenderCompID and TargetCompID
//   - Sequence numbers must be monotonically increasing per session
//
// IBKR FIX Gateway:
//   - Runs locally on the same machine as TWS or IB Gateway
//   - Default port: 4001 (live), 4002 (paper)
//   - Accepts FIX 4.2 sessions
//   - Docs: https://interactivebrokers.github.io/tws-api/fix_protocol.html

package fix

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/millennium-oes/internal/broker"
	"github.com/millennium-oes/internal/order"
	"github.com/quickfixgo/quickfix"
)

// -----------------------------------------------------------------------
// FIX tag constants — FIX 4.2 standard tags used in this implementation
// -----------------------------------------------------------------------
const (
	// Header tags
	tagMsgType    = quickfix.Tag(35)
	tagSenderComp = quickfix.Tag(49)
	tagTargetComp = quickfix.Tag(56)

	// Order identification
	tagClOrdID    = quickfix.Tag(11)  // our client order ID
	tagOrigClOrdID = quickfix.Tag(41) // original clOrdID for cancel/replace
	tagOrderID    = quickfix.Tag(37)  // broker-assigned order ID

	// Instrument
	tagSymbol   = quickfix.Tag(55)
	tagSecExch  = quickfix.Tag(207) // SecurityExchange
	tagCurrency = quickfix.Tag(15)

	// Order definition
	tagSide        = quickfix.Tag(54)  // 1=Buy 2=Sell 5=SellShort
	tagOrdType     = quickfix.Tag(40)  // 1=Market 2=Limit 3=Stop 4=StopLimit
	tagOrderQty    = quickfix.Tag(38)
	tagPrice       = quickfix.Tag(44)  // limit price
	tagStopPx      = quickfix.Tag(99)  // stop price
	tagTimeInForce = quickfix.Tag(59)  // 0=Day 1=GTC 3=IOC 4=FOK 6=GTD
	tagExpireDate  = quickfix.Tag(432) // for GTD
	tagAccount     = quickfix.Tag(1)
	tagHandlInst   = quickfix.Tag(21)  // 1=AutoPrivate
	tagTransactTime = quickfix.Tag(60)

	// Execution report fields
	tagExecID    = quickfix.Tag(17)
	tagExecType  = quickfix.Tag(150) // 0=New 1=PartialFill 2=Fill 4=Cancelled 8=Rejected
	tagOrdStatus = quickfix.Tag(39)
	tagLastQty   = quickfix.Tag(32)  // qty filled in this execution
	tagLastPx    = quickfix.Tag(31)  // price of this execution
	tagCumQty    = quickfix.Tag(14)  // total filled qty
	tagAvgPx     = quickfix.Tag(6)   // average fill price
	tagLeavesQty = quickfix.Tag(151) // remaining qty
	tagText      = quickfix.Tag(58)  // rejection reason / free text

	// IBKR custom tags for trailing stop
	tagIBKRTrailAmt  = quickfix.Tag(6140) // trail amount in $
	tagIBKRTrailPct  = quickfix.Tag(6141) // trail amount in %
)

// -----------------------------------------------------------------------
// Config
// -----------------------------------------------------------------------

// Config holds FIX session configuration
type Config struct {
	// FIX session identity
	SenderCompID string // your firm's ID, e.g. "MILLENNIUM"
	TargetCompID string // broker's ID, e.g. "IBFX" for IBKR

	// IBKR Gateway connection
	Host string // "127.0.0.1" for local gateway
	Port int    // 4001 (live) or 4002 (paper)

	// Session settings
	HeartbeatInterval int    // seconds, typically 30
	ResetOnLogon      bool   // reset sequence numbers on each logon
	FileStorePath     string // path to store FIX message log

	// Account
	Account string // IBKR account number
}

// -----------------------------------------------------------------------
// Client
// -----------------------------------------------------------------------

// Client is a FIX 4.2 broker client that implements broker.Client.
// It implements the quickfix.Application interface so QuickFIX calls
// our methods directly when messages arrive.
type Client struct {
	cfg     Config
	fills   chan order.FillEvent

	// QuickFIX initiator — manages the TCP connection and session
	initiator *quickfix.Initiator
	sessionID quickfix.SessionID

	// Pending order tracking: clOrdID → channel waiting for broker ACK
	mu      sync.RWMutex
	pending map[string]chan execReport

	// Quote cache: symbol → midpoint price
	quoteMu sync.RWMutex
	quotes  map[string]float64

	// Position and account caches
	posMu     sync.RWMutex
	positions map[string]*broker.Position

	accountMu sync.RWMutex
	account   *broker.Account

	connected bool
}

// execReport is the parsed content of a FIX ExecutionReport (35=8)
type execReport struct {
	ClOrdID   string
	OrderID   string
	ExecType  string // 0=New 1=PartialFill 2=Fill 4=Cancelled 8=Rejected
	OrdStatus string
	LastQty   float64
	LastPx    float64
	CumQty    float64
	AvgPx     float64
	Text      string
}

// NewClient creates a FIX client. Call Connect() to start the session.
func NewClient(cfg Config) *Client {
	return &Client{
		cfg:       cfg,
		fills:     make(chan order.FillEvent, 4096),
		pending:   make(map[string]chan execReport),
		quotes:    make(map[string]float64),
		positions: make(map[string]*broker.Position),
		account:   &broker.Account{Status: "connecting"},
	}
}

// -----------------------------------------------------------------------
// quickfix.Application interface — callbacks from the FIX engine
// -----------------------------------------------------------------------

func (c *Client) OnCreate(sessionID quickfix.SessionID) {
	c.sessionID = sessionID
	log.Printf("[FIX] Session created: %s→%s", sessionID.SenderCompID, sessionID.TargetCompID)
}

func (c *Client) OnLogon(sessionID quickfix.SessionID) {
	c.connected = true
	log.Printf("[FIX] ✓ Logged on to %s", sessionID.TargetCompID)
}

func (c *Client) OnLogout(sessionID quickfix.SessionID) {
	c.connected = false
	log.Printf("[FIX] Logged out from %s", sessionID.TargetCompID)
}

// ToAdmin is called before admin messages (Logon, Heartbeat) are sent.
// IBKR paper trading doesn't require credentials in FIX Logon.
// For live trading, inject tag 553 (Username) and 554 (Password) here.
func (c *Client) ToAdmin(msg *quickfix.Message, sessionID quickfix.SessionID) {}

func (c *Client) FromAdmin(msg *quickfix.Message, sessionID quickfix.SessionID) quickfix.MessageRejectError {
	return nil
}

// ToApp is called before every outbound application message — log it
func (c *Client) ToApp(msg *quickfix.Message, sessionID quickfix.SessionID) error {
	log.Printf("[FIX] → %s", fixMsgSummary(msg))
	return nil
}

// FromApp is called for every inbound application message.
// This is the hot path — execution reports arrive here.
func (c *Client) FromApp(msg *quickfix.Message, sessionID quickfix.SessionID) quickfix.MessageRejectError {
	msgType, err := msg.Header.GetString(tagMsgType)
	if err != nil {
		return err
	}

	log.Printf("[FIX] ← %s", fixMsgSummary(msg))

	switch msgType {
	case "8": // ExecutionReport
		c.handleExecutionReport(msg)
	case "9": // OrderCancelReject
		c.handleCancelReject(msg)
	case "j": // BusinessMessageReject
		text, _ := msg.Body.GetString(tagText)
		log.Printf("[FIX] Business reject: %s", text)
	case "W": // MarketDataSnapshotFullRefresh
		c.handleMarketDataSnapshot(msg)
	case "X": // MarketDataIncrementalRefresh
		c.handleMarketDataSnapshot(msg) // simplified
	}

	return nil
}

// -----------------------------------------------------------------------
// ExecutionReport handler (35=8) — the most important message in FIX
// -----------------------------------------------------------------------

func (c *Client) handleExecutionReport(msg *quickfix.Message) {
	var rep execReport

	rep.ClOrdID, _ = msg.Body.GetString(tagClOrdID)
	rep.OrderID, _ = msg.Body.GetString(tagOrderID)
	rep.ExecType, _ = msg.Body.GetString(tagExecType)
	rep.OrdStatus, _ = msg.Body.GetString(tagOrdStatus)
	rep.Text, _ = msg.Body.GetString(tagText)

	if s, err := msg.Body.GetString(tagLastQty); err == nil {
		rep.LastQty, _ = strconv.ParseFloat(s, 64)
	}
	if s, err := msg.Body.GetString(tagLastPx); err == nil {
		rep.LastPx, _ = strconv.ParseFloat(s, 64)
	}
	if s, err := msg.Body.GetString(tagCumQty); err == nil {
		rep.CumQty, _ = strconv.ParseFloat(s, 64)
	}
	if s, err := msg.Body.GetString(tagAvgPx); err == nil {
		rep.AvgPx, _ = strconv.ParseFloat(s, 64)
	}

	// Notify any goroutine waiting for this order's ACK
	c.mu.RLock()
	ch, waiting := c.pending[rep.ClOrdID]
	c.mu.RUnlock()
	if waiting {
		select {
		case ch <- rep:
		default:
		}
	}

	// ExecType 1=PartialFill, 2=Fill — emit fill events to the order engine
	if (rep.ExecType == "1" || rep.ExecType == "2") && rep.LastQty > 0 {
		c.fills <- order.FillEvent{
			BrokerOrderID: rep.OrderID,
			FilledQty:     rep.LastQty,
			FilledPrice:   rep.LastPx,
			Timestamp:     time.Now(),
		}
	}
}

func (c *Client) handleCancelReject(msg *quickfix.Message) {
	clOrdID, _ := msg.Body.GetString(tagClOrdID)
	reason, _ := msg.Body.GetString(tagText)
	log.Printf("[FIX] Cancel rejected for %s: %s", clOrdID, reason)

	// Notify waiting goroutine
	c.mu.RLock()
	ch, waiting := c.pending[clOrdID]
	c.mu.RUnlock()
	if waiting {
		select {
		case ch <- execReport{ClOrdID: clOrdID, ExecType: "9", Text: reason}:
		default:
		}
	}
}

// -----------------------------------------------------------------------
// Connect — start the QuickFIX initiator
// -----------------------------------------------------------------------

func (c *Client) Connect(ctx context.Context) error {
	settings, err := c.buildSettings()
	if err != nil {
		return fmt.Errorf("build FIX settings: %w", err)
	}

	storeFactory := quickfix.NewFileStoreFactory(settings)

	logFactory, err := quickfix.NewFileLogFactory(settings)
	if err != nil {
		return fmt.Errorf("create log factory: %w", err)
	}

	initiator, err := quickfix.NewInitiator(c, storeFactory, settings, logFactory)
	if err != nil {
		return fmt.Errorf("create initiator: %w", err)
	}

	c.initiator = initiator

	if err := initiator.Start(); err != nil {
		return fmt.Errorf("start initiator: %w", err)
	}

	// Wait for logon (up to 10 seconds)
	deadline := time.Now().Add(10 * time.Second)
	for !c.connected && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}
	if !c.connected {
		return fmt.Errorf("FIX session did not establish within 10s — is IBKR Gateway running on %s:%d?", c.cfg.Host, c.cfg.Port)
	}

	go func() {
		<-ctx.Done()
		initiator.Stop()
	}()

	log.Printf("[FIX] Connected to IBKR gateway at %s:%d", c.cfg.Host, c.cfg.Port)
	return nil
}

// -----------------------------------------------------------------------
// broker.Client interface — order operations
// -----------------------------------------------------------------------

// SubmitOrder sends a FIX NewOrderSingle (35=D) and waits for broker ACK
func (c *Client) SubmitOrder(ctx context.Context, o *order.Order) (string, error) {
	msg := c.buildNewOrderSingle(o)

	ackCh := make(chan execReport, 1)
	c.mu.Lock()
	c.pending[o.ClientOrderID] = ackCh
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.pending, o.ClientOrderID)
		c.mu.Unlock()
	}()

	if err := quickfix.SendToTarget(*msg, c.sessionID); err != nil {
		return "", fmt.Errorf("FIX send NewOrderSingle: %w", err)
	}

	select {
	case rep := <-ackCh:
		if rep.ExecType == "8" {
			return "", fmt.Errorf("order rejected by IBKR: %s", rep.Text)
		}
		return rep.OrderID, nil
	case <-time.After(5 * time.Second):
		return "", fmt.Errorf("timeout waiting for broker ACK on %s", o.ClientOrderID)
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// CancelOrder sends a FIX OrderCancelRequest (35=F)
func (c *Client) CancelOrder(ctx context.Context, brokerOrderID string) error {
	clOrdID := "cxl-" + shortID()

	msg := quickfix.NewMessage()
	msg.Header.SetString(tagMsgType, "F")
	msg.Body.SetString(tagOrigClOrdID, brokerOrderID)
	msg.Body.SetString(tagClOrdID, clOrdID)
	msg.Body.SetString(tagOrderID, brokerOrderID)
	msg.Body.SetString(tagSymbol, "")       // IBKR requires symbol; caller should pass it
	msg.Body.SetString(tagSide, "1")        // placeholder; IBKR requires side
	msg.Body.SetString(tagTransactTime, utcNow())
	msg.Body.SetString(tagAccount, c.cfg.Account)

	ackCh := make(chan execReport, 1)
	c.mu.Lock()
	c.pending[clOrdID] = ackCh
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.pending, clOrdID)
		c.mu.Unlock()
	}()

	if err := quickfix.SendToTarget(msg, c.sessionID); err != nil {
		return fmt.Errorf("FIX send OrderCancelRequest: %w", err)
	}

	select {
	case rep := <-ackCh:
		if rep.OrdStatus == "4" || rep.ExecType == "4" {
			return nil
		}
		if rep.ExecType == "9" {
			return fmt.Errorf("cancel rejected: %s", rep.Text)
		}
		return nil
	case <-time.After(5 * time.Second):
		return fmt.Errorf("timeout waiting for cancel ACK")
	case <-ctx.Done():
		return ctx.Err()
	}
}

// ReplaceOrder sends a FIX OrderCancelReplaceRequest (35=G)
func (c *Client) ReplaceOrder(ctx context.Context, brokerOrderID string, req order.ReplaceRequest) (string, error) {
	clOrdID := "rep-" + shortID()

	msg := quickfix.NewMessage()
	msg.Header.SetString(tagMsgType, "G")
	msg.Body.SetString(tagOrigClOrdID, brokerOrderID)
	msg.Body.SetString(tagClOrdID, clOrdID)
	msg.Body.SetString(tagOrderID, brokerOrderID)
	msg.Body.SetString(tagHandlInst, "1")
	msg.Body.SetString(tagSymbol, "")
	msg.Body.SetString(tagSide, "1")
	msg.Body.SetString(tagTransactTime, utcNow())
	msg.Body.SetString(tagOrdType, "2") // limit
	msg.Body.SetString(tagAccount, c.cfg.Account)

	if req.Qty != nil {
		msg.Body.SetString(tagOrderQty, fmt.Sprintf("%.0f", *req.Qty))
	}
	if req.LimitPrice != nil {
		msg.Body.SetString(tagPrice, fmt.Sprintf("%.2f", *req.LimitPrice))
	}
	if req.StopPrice != nil {
		msg.Body.SetString(tagStopPx, fmt.Sprintf("%.2f", *req.StopPrice))
	}

	ackCh := make(chan execReport, 1)
	c.mu.Lock()
	c.pending[clOrdID] = ackCh
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.pending, clOrdID)
		c.mu.Unlock()
	}()

	if err := quickfix.SendToTarget(msg, c.sessionID); err != nil {
		return "", fmt.Errorf("FIX send OrderCancelReplaceRequest: %w", err)
	}

	select {
	case rep := <-ackCh:
		if rep.ExecType == "8" {
			return "", fmt.Errorf("replace rejected: %s", rep.Text)
		}
		return rep.OrderID, nil
	case <-time.After(5 * time.Second):
		return "", fmt.Errorf("timeout waiting for replace ACK")
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

func (c *Client) FillEvents() <-chan order.FillEvent {
	return c.fills
}

// -----------------------------------------------------------------------
// broker.Client interface — market data & account
// -----------------------------------------------------------------------

func (c *Client) GetQuote(ctx context.Context, symbol string) (float64, error) {
	c.quoteMu.RLock()
	price, ok := c.quotes[symbol]
	c.quoteMu.RUnlock()
	if !ok {
		// Auto-subscribe on first request
		c.SubscribeQuote(symbol)
		return 0, fmt.Errorf("no quote yet for %s — subscribed, retry shortly", symbol)
	}
	return price, nil
}

func (c *Client) SetQuote(symbol string, price float64) {
	c.quoteMu.Lock()
	c.quotes[symbol] = price
	c.quoteMu.Unlock()
}

func (c *Client) GetPositions(ctx context.Context) ([]*broker.Position, error) {
	c.posMu.RLock()
	defer c.posMu.RUnlock()
	result := make([]*broker.Position, 0, len(c.positions))
	for _, p := range c.positions {
		result = append(result, p)
	}
	return result, nil
}

func (c *Client) GetAccount(ctx context.Context) (*broker.Account, error) {
	c.accountMu.RLock()
	defer c.accountMu.RUnlock()
	if c.account == nil {
		return nil, fmt.Errorf("account data not yet received from IBKR")
	}
	return c.account, nil
}

func (c *Client) IsConnected() bool { return c.connected }

// -----------------------------------------------------------------------
// NewOrderSingle builder — maps our Order to FIX 4.2 tags
// -----------------------------------------------------------------------

func (c *Client) buildNewOrderSingle(o *order.Order) *quickfix.Message {
	msg := quickfix.NewMessage()
	msg.Header.SetString(tagMsgType, "D") // NewOrderSingle

	// Core fields
	msg.Body.SetString(tagClOrdID, o.ClientOrderID)
	msg.Body.SetString(tagHandlInst, "1") // AutoPrivate
	msg.Body.SetString(tagSymbol, o.Symbol)
	msg.Body.SetString(tagSide, mapSide(o.Side))
	msg.Body.SetString(tagTransactTime, utcNow())
	msg.Body.SetString(tagOrdType, mapOrdType(o.Type))
	msg.Body.SetString(tagTimeInForce, mapTIF(o.TimeInForce))
	msg.Body.SetString(tagAccount, c.cfg.Account)
	msg.Body.SetString(tagSecExch, "SMART") // IBKR smart routing
	msg.Body.SetString(tagCurrency, "USD")

	// Quantity
	if o.Qty > 0 {
		msg.Body.SetString(tagOrderQty, fmt.Sprintf("%.0f", o.Qty))
	}

	// Prices
	if o.LimitPrice != nil {
		msg.Body.SetString(tagPrice, fmt.Sprintf("%.4f", *o.LimitPrice))
	}
	if o.StopPrice != nil {
		msg.Body.SetString(tagStopPx, fmt.Sprintf("%.4f", *o.StopPrice))
	}

	// Trailing stop — IBKR custom tags
	if o.Type == order.TypeTrailingStop && o.TrailValue != nil {
		if o.TrailType == order.TrailingTypePercent {
			msg.Body.SetString(tagIBKRTrailPct, fmt.Sprintf("%.2f", *o.TrailValue))
		} else {
			msg.Body.SetString(tagIBKRTrailAmt, fmt.Sprintf("%.2f", *o.TrailValue))
		}
	}

	// GTD expiry
	if o.TimeInForce == order.TIFGTD && o.ExpireAt != nil {
		msg.Body.SetString(tagExpireDate, o.ExpireAt.Format("20060102"))
	}

	return &msg
}

// -----------------------------------------------------------------------
// FIX value mappers
// -----------------------------------------------------------------------

func mapSide(s order.Side) string {
	switch s {
	case order.SideSell:
		return "2"
	case order.SideSellShort:
		return "5"
	case order.SideBuyToCover:
		return "1" // IBKR treats buy-to-cover as buy
	default:
		return "1" // Buy
	}
}

func mapOrdType(t order.Type) string {
	switch t {
	case order.TypeLimit, order.TypeLOO, order.TypeLOC:
		return "2"
	case order.TypeStop:
		return "3"
	case order.TypeStopLimit:
		return "4"
	default:
		return "1" // Market
	}
}

func mapTIF(tif order.TimeInForce) string {
	switch tif {
	case order.TIFGTC:
		return "1"
	case order.TIFIOC:
		return "3"
	case order.TIFFOK:
		return "4"
	case order.TIFGTD:
		return "6"
	case order.TIFATO:
		return "2" // At the Opening
	case order.TIFATC:
		return "7" // At the Close (FIX 4.4; IBKR accepts it)
	default:
		return "0" // Day
	}
}

// -----------------------------------------------------------------------
// Market data subscription (35=V)
// -----------------------------------------------------------------------

// SubscribeQuote sends a FIX MarketDataRequest (35=V) for a symbol
func (c *Client) SubscribeQuote(symbol string) error {
	if !c.connected {
		return fmt.Errorf("FIX session not connected")
	}

	reqID := fmt.Sprintf("MDR-%s-%d", symbol, time.Now().UnixNano())

	msg := quickfix.NewMessage()
	msg.Header.SetString(tagMsgType, "V") // MarketDataRequest

	msg.Body.SetString(quickfix.Tag(262), reqID) // MDReqID
	msg.Body.SetString(quickfix.Tag(263), "1")   // SubscriptionRequestType: Snapshot+Updates
	msg.Body.SetString(quickfix.Tag(264), "1")   // MarketDepth: top of book

	// NoMDEntryTypes (267) — Bid and Ask
	msg.Body.SetString(quickfix.Tag(267), "2")
	msg.Body.SetString(quickfix.Tag(269), "0") // Bid
	msg.Body.SetString(quickfix.Tag(269), "1") // Ask

	// NoRelatedSym (146)
	msg.Body.SetString(quickfix.Tag(146), "1")
	msg.Body.SetString(tagSymbol, symbol)

	if err := quickfix.SendToTarget(msg, c.sessionID); err != nil {
		return fmt.Errorf("send market data request: %w", err)
	}

	log.Printf("[FIX] Subscribed to market data for %s", symbol)
	return nil
}

// handleMarketDataSnapshot processes 35=W (MarketDataSnapshotFullRefresh)
func (c *Client) handleMarketDataSnapshot(msg *quickfix.Message) {
	symbol, err := msg.Body.GetString(tagSymbol)
	if err != nil || symbol == "" {
		return
	}

	// Parse bid/ask from the repeating group
	// Tags 269=MDEntryType, 270=MDEntryPx
	var bid, ask float64

	// QuickFIX raw message iteration — walk all body fields
	rawMsg := msg.String()
	fields := strings.Split(rawMsg, "\x01")

	var lastEntryType string
	for _, f := range fields {
		parts := strings.SplitN(f, "=", 2)
		if len(parts) != 2 {
			continue
		}
		switch parts[0] {
		case "269":
			lastEntryType = parts[1]
		case "270":
			px, _ := strconv.ParseFloat(parts[1], 64)
			switch lastEntryType {
			case "0":
				bid = px
			case "1":
				ask = px
			}
		}
	}

	if bid > 0 && ask > 0 {
		mid := (bid + ask) / 2
		c.SetQuote(symbol, mid)
		log.Printf("[FIX] Quote %s bid=%.4f ask=%.4f mid=%.4f", symbol, bid, ask, mid)
	}
}

// -----------------------------------------------------------------------
// QuickFIX settings builder
// -----------------------------------------------------------------------

func (c *Client) buildSettings() (*quickfix.Settings, error) {
	resetStr := "N"
	if c.cfg.ResetOnLogon {
		resetStr = "Y"
	}

	cfg := fmt.Sprintf(`
[DEFAULT]
ConnectionType=initiator
ReconnectInterval=5
HeartBtInt=%d
StartTime=00:00:00
EndTime=00:00:00
UseDataDictionary=N
FileStorePath=%s
FileLogPath=%s/log
ResetOnLogon=%s
SocketConnectHost=%s
SocketConnectPort=%d

[SESSION]
BeginString=FIX.4.2
SenderCompID=%s
TargetCompID=%s
`,
		c.cfg.HeartbeatInterval,
		c.cfg.FileStorePath,
		c.cfg.FileStorePath,
		resetStr,
		c.cfg.Host,
		c.cfg.Port,
		c.cfg.SenderCompID,
		c.cfg.TargetCompID,
	)

	return quickfix.ParseSettings(strings.NewReader(cfg))
}

// -----------------------------------------------------------------------
// Helpers
// -----------------------------------------------------------------------

func utcNow() string {
	return time.Now().UTC().Format("20060102-15:04:05.000")
}

func shortID() string {
	return strconv.FormatInt(time.Now().UnixNano(), 36)
}

func fixMsgSummary(msg *quickfix.Message) string {
	msgType, _ := msg.Header.GetString(tagMsgType)
	clOrdID, _ := msg.Body.GetString(tagClOrdID)
	symbol, _ := msg.Body.GetString(tagSymbol)
	return fmt.Sprintf("35=%s clOrdID=%s sym=%s", msgType, clOrdID, symbol)
}

// Ensure broker package is used
var _ broker.Client = (*Client)(nil)
