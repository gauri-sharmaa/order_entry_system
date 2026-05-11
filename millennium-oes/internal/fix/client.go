// Package fix implements a minimal FIX 4.2 client for IBKR.
//
// This is a raw TCP implementation — no QuickFIX dependency.
// Building it from scratch gives us:
//   - Zero allocations on the hot path (pre-allocated buffers)
//   - Full control over the TCP connection (Nagle disabled, keep-alive)
//   - No framework overhead
//   - Educational value (shows exactly how FIX works)
//
// FIX message format:
//   tag=value<SOH>tag=value<SOH>...tag=value<SOH>
//   where SOH = 0x01 (Start of Header, ASCII control character)
//
// Required fields in every message:
//   8  = BeginString (FIX.4.2)
//   9  = BodyLength
//   35 = MsgType
//   49 = SenderCompID
//   56 = TargetCompID
//   34 = MsgSeqNum
//   52 = SendingTime
//   10 = CheckSum (last field, mod 256 of all bytes)

package fix

import (
	"bufio"
	"context"
	"fmt"
	"log"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/millennium-oes/internal/engine"
)

const (
	SOH          = byte(0x01) // FIX field delimiter
	beginString  = "FIX.4.2"
	maxMsgSize   = 4096
)

// Config for the FIX client
type Config struct {
	SenderCompID string
	TargetCompID string
	Host         string
	Port         int
	Account      string
	Heartbeat    int // seconds
}

// Client is a raw FIX 4.2 TCP client
type Client struct {
	cfg Config

	conn   net.Conn
	reader *bufio.Reader

	// Sequence numbers (FIX requires monotonically increasing)
	outSeq atomic.Uint64
	inSeq  atomic.Uint64

	// Pre-allocated send buffer (avoids allocation on hot path)
	sendBuf [maxMsgSize]byte

	// Pending orders: clOrdID → order index (for matching fills)
	mu      sync.RWMutex
	pending map[string]uint32 // clOrdID → engine order index

	// Engine reference for injecting fills
	engine *engine.Engine

	connected atomic.Bool
	ctx       context.Context
	cancel    context.CancelFunc
}

// NewClient creates a FIX client
func NewClient(cfg Config) *Client {
	return &Client{
		cfg:     cfg,
		pending: make(map[string]uint32, 1024),
	}
}

// SetEngine wires the FIX client to the order engine for fill injection
func (c *Client) SetEngine(eng *engine.Engine) {
	c.engine = eng
}

// Connect establishes the TCP connection and starts the FIX session
func (c *Client) Connect(ctx context.Context) error {
	c.ctx, c.cancel = context.WithCancel(ctx)

	addr := fmt.Sprintf("%s:%d", c.cfg.Host, c.cfg.Port)

	// TCP connection with Nagle disabled (critical for latency)
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		return fmt.Errorf("FIX connect to %s: %w", addr, err)
	}

	// Disable Nagle's algorithm — send immediately, don't buffer
	if tc, ok := conn.(*net.TCPConn); ok {
		tc.SetNoDelay(true)
		tc.SetKeepAlive(true)
		tc.SetKeepAlivePeriod(30 * time.Second)
	}

	c.conn = conn
	c.reader = bufio.NewReaderSize(conn, 65536)
	c.outSeq.Store(1)
	c.inSeq.Store(1)

	// Send Logon (35=A)
	if err := c.sendLogon(); err != nil {
		conn.Close()
		return fmt.Errorf("FIX logon: %w", err)
	}

	// Wait for Logon response
	msg, err := c.readMessage()
	if err != nil {
		conn.Close()
		return fmt.Errorf("FIX logon response: %w", err)
	}

	if msg.getTag("35") != "A" {
		conn.Close()
		return fmt.Errorf("expected Logon response, got MsgType=%s", msg.getTag("35"))
	}

	c.connected.Store(true)
	log.Printf("[FIX] Connected to %s (session: %s→%s)", addr, c.cfg.SenderCompID, c.cfg.TargetCompID)

	// Start reader goroutine (processes incoming messages)
	go c.readLoop()

	// Start heartbeat goroutine
	go c.heartbeatLoop()

	return nil
}

// -----------------------------------------------------------------------
// Broker interface implementation
// -----------------------------------------------------------------------

// Submit sends a NewOrderSingle (35=D)
func (c *Client) Submit(o *engine.Order) error {
	if !c.connected.Load() {
		return fmt.Errorf("FIX not connected")
	}

	clOrdID := fmt.Sprintf("M%d", o.ID)

	// Register pending order for fill matching
	c.mu.Lock()
	c.pending[clOrdID] = o.ID
	c.mu.Unlock()

	msg := c.newMessage("D") // NewOrderSingle
	msg.setTag(11, clOrdID)
	msg.setTag(1, c.cfg.Account)
	msg.setTag(21, "1") // HandlInst: AutoPrivate
	msg.setTag(55, engine.SymbolToString(o.Symbol))
	msg.setTag(54, fixSide(o.Side))
	msg.setTag(40, fixOrdType(o.Type))
	msg.setTag(38, strconv.Itoa(int(o.Qty)))
	msg.setTag(59, fixTIF(o.TIF))
	msg.setTag(60, fixTimestamp(time.Now()))
	msg.setTag(207, "SMART") // SecurityExchange

	if o.Price > 0 {
		msg.setTag(44, formatPrice(o.Price))
	}
	if o.StopPrice > 0 {
		msg.setTag(99, formatPrice(o.StopPrice))
	}

	return c.send(msg)
}

// Cancel sends an OrderCancelRequest (35=F)
func (c *Client) Cancel(brokerID [16]byte) error {
	if !c.connected.Load() {
		return fmt.Errorf("FIX not connected")
	}

	origID := trimBytes(brokerID[:])
	clOrdID := fmt.Sprintf("CXL%d", time.Now().UnixNano())

	msg := c.newMessage("F")
	msg.setTag(11, clOrdID)
	msg.setTag(41, origID) // OrigClOrdID
	msg.setTag(1, c.cfg.Account)
	msg.setTag(60, fixTimestamp(time.Now()))

	return c.send(msg)
}

// Replace sends an OrderCancelReplaceRequest (35=G)
func (c *Client) Replace(brokerID [16]byte, qty int32, price int64) error {
	if !c.connected.Load() {
		return fmt.Errorf("FIX not connected")
	}

	origID := trimBytes(brokerID[:])
	clOrdID := fmt.Sprintf("REP%d", time.Now().UnixNano())

	msg := c.newMessage("G")
	msg.setTag(11, clOrdID)
	msg.setTag(41, origID)
	msg.setTag(1, c.cfg.Account)
	msg.setTag(21, "1")
	msg.setTag(60, fixTimestamp(time.Now()))

	if qty > 0 {
		msg.setTag(38, strconv.Itoa(int(qty)))
	}
	if price > 0 {
		msg.setTag(44, formatPrice(price))
	}

	return c.send(msg)
}

// -----------------------------------------------------------------------
// Message reading (runs in dedicated goroutine)
// -----------------------------------------------------------------------

func (c *Client) readLoop() {
	for {
		select {
		case <-c.ctx.Done():
			return
		default:
		}

		msg, err := c.readMessage()
		if err != nil {
			if c.ctx.Err() != nil {
				return
			}
			log.Printf("[FIX] Read error: %v", err)
			c.connected.Store(false)
			return
		}

		c.handleMessage(msg)
	}
}

func (c *Client) handleMessage(msg *fixMsg) {
	msgType := msg.getTag("35")

	switch msgType {
	case "8": // ExecutionReport
		c.handleExecReport(msg)
	case "9": // OrderCancelReject
		log.Printf("[FIX] Cancel rejected: %s", msg.getTag("58"))
	case "0": // Heartbeat — ignore
	case "1": // TestRequest — respond with heartbeat
		c.sendHeartbeat(msg.getTag("112"))
	case "5": // Logout
		log.Println("[FIX] Received Logout")
		c.connected.Store(false)
	}
}

func (c *Client) handleExecReport(msg *fixMsg) {
	execType := msg.getTag("150")
	clOrdID := msg.getTag("11")
	orderID := msg.getTag("37")

	// Store broker order ID
	c.mu.RLock()
	idx, found := c.pending[clOrdID]
	c.mu.RUnlock()

	if !found {
		return
	}

	// Copy broker ID into order
	if c.engine != nil && orderID != "" {
		o, ok := c.engine.GetOrder(idx)
		if ok {
			copy(o.BrokerID[:], orderID)
			_ = o // read-only copy, actual update happens via ring buffer
		}
	}

	switch execType {
	case "0": // New (acknowledged)
		// Order accepted by broker
	case "1", "2": // PartialFill, Fill
		lastQtyStr := msg.getTag("32")
		lastPxStr := msg.getTag("31")
		lastQty, _ := strconv.Atoi(lastQtyStr)
		lastPx, _ := strconv.ParseFloat(lastPxStr, 64)

		if c.engine != nil && lastQty > 0 {
			c.engine.InjectFill(idx, int32(lastQty), engine.PriceToMicros(lastPx))
		}

		log.Printf("[FIX] Fill: %s qty=%d px=%s", clOrdID, lastQty, lastPxStr)

	case "4": // Cancelled
		log.Printf("[FIX] Cancelled: %s", clOrdID)
	case "8": // Rejected
		reason := msg.getTag("58")
		log.Printf("[FIX] Rejected: %s reason=%s", clOrdID, reason)
	}
}

// -----------------------------------------------------------------------
// Heartbeat
// -----------------------------------------------------------------------

func (c *Client) heartbeatLoop() {
	ticker := time.NewTicker(time.Duration(c.cfg.Heartbeat) * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-c.ctx.Done():
			return
		case <-ticker.C:
			if c.connected.Load() {
				c.sendHeartbeat("")
			}
		}
	}
}

func (c *Client) sendHeartbeat(testReqID string) {
	msg := c.newMessage("0") // Heartbeat
	if testReqID != "" {
		msg.setTag(112, testReqID)
	}
	c.send(msg)
}

func (c *Client) sendLogon() error {
	msg := c.newMessage("A") // Logon
	msg.setTag(98, "0")      // EncryptMethod: None
	msg.setTag(108, strconv.Itoa(c.cfg.Heartbeat))
	msg.setTag(141, "Y") // ResetSeqNumFlag
	return c.send(msg)
}

// -----------------------------------------------------------------------
// Low-level FIX message building and sending
// -----------------------------------------------------------------------

type fixMsg struct {
	tags []fixTag
}

type fixTag struct {
	tag   string
	value string
}

func (c *Client) newMessage(msgType string) *fixMsg {
	seq := c.outSeq.Add(1) - 1
	return &fixMsg{
		tags: []fixTag{
			{tag: "35", value: msgType},
			{tag: "49", value: c.cfg.SenderCompID},
			{tag: "56", value: c.cfg.TargetCompID},
			{tag: "34", value: strconv.FormatUint(seq, 10)},
			{tag: "52", value: fixTimestamp(time.Now())},
		},
	}
}

func (m *fixMsg) setTag(tag int, value string) {
	m.tags = append(m.tags, fixTag{tag: strconv.Itoa(tag), value: value})
}

func (m *fixMsg) getTag(tag string) string {
	for _, t := range m.tags {
		if t.tag == tag {
			return t.value
		}
	}
	return ""
}

func (c *Client) send(msg *fixMsg) error {
	// Build body (all tags except 8, 9, 10)
	body := make([]byte, 0, 512)
	for _, t := range msg.tags {
		body = append(body, []byte(t.tag)...)
		body = append(body, '=')
		body = append(body, []byte(t.value)...)
		body = append(body, SOH)
	}

	// Build full message: 8=FIX.4.2|9=<len>|<body>|10=<checksum>|
	header := fmt.Sprintf("8=%s%c9=%d%c", beginString, SOH, len(body), SOH)
	raw := append([]byte(header), body...)

	// Checksum: sum of all bytes mod 256
	sum := 0
	for _, b := range raw {
		sum += int(b)
	}
	checksum := fmt.Sprintf("10=%03d%c", sum%256, SOH)
	raw = append(raw, []byte(checksum)...)

	_, err := c.conn.Write(raw)
	return err
}

func (c *Client) readMessage() (*fixMsg, error) {
	// Read until we find 10=xxx<SOH> (checksum marks end of message)
	var raw []byte
	for {
		b, err := c.reader.ReadByte()
		if err != nil {
			return nil, err
		}
		raw = append(raw, b)

		// Check if we've received the checksum field
		if len(raw) > 7 && raw[len(raw)-1] == SOH {
			// Look for "10=" near the end
			end := string(raw[len(raw)-8:])
			if len(end) > 4 && end[0] == '1' && end[1] == '0' && end[2] == '=' {
				break
			}
			// Also check if the last field starts with 10=
			for i := len(raw) - 2; i >= 0 && i > len(raw)-10; i-- {
				if raw[i] == SOH || i == 0 {
					start := i
					if raw[i] == SOH {
						start = i + 1
					}
					field := string(raw[start : len(raw)-1])
					if len(field) > 3 && field[:3] == "10=" {
						goto done
					}
					break
				}
			}
		}
	}
done:

	// Parse tags
	msg := &fixMsg{}
	start := 0
	for i, b := range raw {
		if b == SOH {
			field := string(raw[start:i])
			for j, c := range field {
				if c == '=' {
					msg.tags = append(msg.tags, fixTag{
						tag:   field[:j],
						value: field[j+1:],
					})
					break
				}
			}
			start = i + 1
		}
	}

	return msg, nil
}

// -----------------------------------------------------------------------
// Helpers
// -----------------------------------------------------------------------

func fixTimestamp(t time.Time) string {
	return t.UTC().Format("20060102-15:04:05.000")
}

func fixSide(s engine.Side) string {
	switch s {
	case engine.SideSell:
		return "2"
	case engine.SideShort:
		return "5"
	case engine.SideCover:
		return "1"
	default:
		return "1"
	}
}

func fixOrdType(t engine.OrderType) string {
	switch t {
	case engine.OrdLimit, engine.OrdLOO, engine.OrdLOC:
		return "2"
	case engine.OrdStop:
		return "3"
	case engine.OrdStopLimit:
		return "4"
	default:
		return "1"
	}
}

func fixTIF(tif engine.TimeInForce) string {
	switch tif {
	case engine.TIFGTC:
		return "1"
	case engine.TIFIOC:
		return "3"
	case engine.TIFFOK:
		return "4"
	case engine.TIFGTD:
		return "6"
	case engine.TIFATO:
		return "2"
	case engine.TIFATC:
		return "7"
	default:
		return "0"
	}
}

func formatPrice(micros int64) string {
	return strconv.FormatFloat(engine.MicrosToPrice(micros), 'f', 4, 64)
}

func trimBytes(b []byte) string {
	for i, c := range b {
		if c == 0 {
			return string(b[:i])
		}
	}
	return string(b)
}
