package order

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/google/uuid"
)

// OrderStore is the interface for the hot-path order state store (Redis)
type OrderStore interface {
	SetOrder(ctx context.Context, o *Order) error
	GetOrder(ctx context.Context, orderID string) (*Order, error)
	GetOrderByBrokerID(ctx context.Context, brokerID string) (*Order, error)
	ListOrders(ctx context.Context, filter ListFilter) ([]*Order, error)
}

// DurableStore is the interface for durable persistence (DynamoDB)
type DurableStore interface {
	PutOrder(ctx context.Context, o *Order) error
	GetOrder(ctx context.Context, orderID string) (*Order, error)
}

// Engine is the core order management engine.
// It handles order creation, lifecycle management, linked order logic,
// and processes fill events from the broker.
type Engine struct {
	redis   OrderStore
	dynamo  DurableStore

	// In-memory index of active orders for ultra-fast lookup
	// (Redis is source of truth; this is a hot cache)
	mu      sync.RWMutex
	active  map[string]*Order // orderID → Order

	// Subscribers for order update events (SSE / WebSocket)
	subsMu  sync.RWMutex
	subs    map[string]chan *Order // subscriberID → channel
}

func NewEngine(redis OrderStore, dynamo DurableStore) *Engine {
	return &Engine{
		redis:  redis,
		dynamo: dynamo,
		active: make(map[string]*Order),
		subs:   make(map[string]chan *Order),
	}
}

// -----------------------------------------------------------------------
// Submit — validate, build, and route an order
// -----------------------------------------------------------------------

func (e *Engine) Submit(ctx context.Context, req SubmitRequest, broker BrokerClient) (*Order, error) {
	start := time.Now()

	// Build the order struct
	o, err := e.buildOrder(req)
	if err != nil {
		return nil, fmt.Errorf("build order: %w", err)
	}

	// Persist to Redis (hot path) and DynamoDB (durable)
	if err := e.persist(ctx, o); err != nil {
		return nil, fmt.Errorf("persist order: %w", err)
	}

	// Handle complex order types that require special routing
	switch o.Type {
	case TypeBracket:
		err = e.submitBracket(ctx, o, req, broker)
	case TypeOCO:
		err = e.submitOCO(ctx, o, req, broker)
	case TypeOTO:
		err = e.submitOTO(ctx, o, req, broker)
	case TypeOTOCO:
		err = e.submitOTOCO(ctx, o, req, broker)
	case TypeTWAP:
		err = e.submitTWAP(ctx, o, broker)
	case TypeVWAP:
		err = e.submitVWAP(ctx, o, broker)
	case TypeIceberg:
		err = e.submitIceberg(ctx, o, broker)
	case TypeTrailingStop:
		err = e.submitTrailingStop(ctx, o, broker)
	case TypeMIT, TypeLIT:
		err = e.submitTouchTriggered(ctx, o, broker)
	case TypeFunari:
		err = e.submitFunari(ctx, o, broker)
	default:
		// Simple order types go directly to broker
		err = e.sendToBroker(ctx, o, broker)
	}

	if err != nil {
		o.Status = StatusRejected
		o.UpdatedAt = time.Now()
		e.persist(ctx, o)
		return o, fmt.Errorf("route order: %w", err)
	}

	log.Printf("[ENGINE] Order %s submitted in %v", o.ID, time.Since(start))
	return o, nil
}

// -----------------------------------------------------------------------
// Cancel
// -----------------------------------------------------------------------

func (e *Engine) Cancel(ctx context.Context, orderID string, broker BrokerClient) (*Order, error) {
	o, err := e.Get(ctx, orderID)
	if err != nil {
		return nil, err
	}

	if !o.Status.IsCancellable() {
		return nil, fmt.Errorf("order %s in status %s cannot be cancelled", orderID, o.Status)
	}

	o.Status = StatusPendingCancel
	o.UpdatedAt = time.Now()
	e.persist(ctx, o)

	// Cancel linked orders (OCO, bracket legs)
	for _, linkedID := range o.LinkedIDs {
		linked, err := e.Get(ctx, linkedID)
		if err == nil && linked.Status.IsCancellable() {
			e.Cancel(ctx, linkedID, broker)
		}
	}

	if o.BrokerOrderID != "" {
		if err := broker.CancelOrder(ctx, o.BrokerOrderID); err != nil {
			return nil, fmt.Errorf("broker cancel: %w", err)
		}
	}

	o.Status = StatusCancelled
	o.UpdatedAt = time.Now()
	e.persist(ctx, o)
	e.notify(o)

	return o, nil
}

// -----------------------------------------------------------------------
// Replace (modify)
// -----------------------------------------------------------------------

func (e *Engine) Replace(ctx context.Context, orderID string, req ReplaceRequest, broker BrokerClient) (*Order, error) {
	o, err := e.Get(ctx, orderID)
	if err != nil {
		return nil, err
	}

	if !o.Status.IsReplaceable() {
		return nil, fmt.Errorf("order %s in status %s cannot be replaced", orderID, o.Status)
	}

	o.Status = StatusPendingReplace
	o.UpdatedAt = time.Now()
	e.persist(ctx, o)

	// Apply changes
	if req.Qty != nil {
		o.Qty = *req.Qty
	}
	if req.LimitPrice != nil {
		o.LimitPrice = req.LimitPrice
	}
	if req.StopPrice != nil {
		o.StopPrice = req.StopPrice
	}
	if req.TrailValue != nil {
		o.TrailValue = req.TrailValue
	}
	if req.TimeInForce != nil {
		o.TimeInForce = *req.TimeInForce
	}

	newBrokerID, err := broker.ReplaceOrder(ctx, o.BrokerOrderID, req)
	if err != nil {
		o.Status = StatusAcknowledged // rollback
		e.persist(ctx, o)
		return nil, fmt.Errorf("broker replace: %w", err)
	}

	o.BrokerOrderID = newBrokerID
	o.Status = StatusReplaced
	o.UpdatedAt = time.Now()
	e.persist(ctx, o)
	e.notify(o)

	return o, nil
}

// -----------------------------------------------------------------------
// Get / List
// -----------------------------------------------------------------------

func (e *Engine) Get(ctx context.Context, orderID string) (*Order, error) {
	// Check hot cache first
	e.mu.RLock()
	if o, ok := e.active[orderID]; ok {
		e.mu.RUnlock()
		return o, nil
	}
	e.mu.RUnlock()

	// Fall back to Redis
	return e.redis.GetOrder(ctx, orderID)
}

func (e *Engine) List(ctx context.Context, filter ListFilter) ([]*Order, error) {
	return e.redis.ListOrders(ctx, filter)
}

// -----------------------------------------------------------------------
// Fill event processing
// -----------------------------------------------------------------------

// ListenForFills consumes fill events from the broker and updates order state.
// This runs in a dedicated goroutine.
func (e *Engine) ListenForFills(ctx context.Context, fills <-chan FillEvent) {
	for {
		select {
		case <-ctx.Done():
			return
		case event, ok := <-fills:
			if !ok {
				return
			}
			e.processFill(ctx, event)
		}
	}
}

func (e *Engine) processFill(ctx context.Context, event FillEvent) {
	start := time.Now()

	o, err := e.getByBrokerID(ctx, event.BrokerOrderID)
	if err != nil {
		log.Printf("[ENGINE] Fill for unknown broker order %s: %v", event.BrokerOrderID, err)
		return
	}

	fill := Fill{
		ID:        uuid.New().String(),
		OrderID:   o.ID,
		Qty:       event.FilledQty,
		Price:     event.FilledPrice,
		Side:      o.Side,
		Timestamp: event.Timestamp,
	}

	o.Fills = append(o.Fills, fill)
	o.FilledQty += event.FilledQty

	// Recalculate average fill price
	totalValue := o.FilledAvgPx * (o.FilledQty - event.FilledQty)
	totalValue += event.FilledPrice * event.FilledQty
	o.FilledAvgPx = totalValue / o.FilledQty

	if o.FilledQty >= o.Qty {
		now := time.Now()
		o.Status = StatusFilled
		o.FilledAt = &now
		e.handlePostFill(ctx, o)
	} else {
		o.Status = StatusPartiallyFilled
	}

	o.UpdatedAt = time.Now()
	e.persist(ctx, o)
	e.notify(o)

	log.Printf("[ENGINE] Fill processed for %s in %v", o.ID, time.Since(start))
}

// handlePostFill triggers linked orders after a fill
func (e *Engine) handlePostFill(ctx context.Context, o *Order) {
	switch o.LinkType {
	case "oco":
		// Cancel the other leg
		for _, linkedID := range o.LinkedIDs {
			if linkedID != o.ID {
				linked, err := e.Get(ctx, linkedID)
				if err == nil && linked.Status.IsCancellable() {
					log.Printf("[ENGINE] OCO: cancelling linked order %s", linkedID)
					// We don't have broker here, so mark for cancellation
					linked.Status = StatusPendingCancel
					linked.UpdatedAt = time.Now()
					e.persist(ctx, linked)
				}
			}
		}
	case "oto":
		// Trigger the secondary order
		for _, linkedID := range o.LinkedIDs {
			linked, err := e.Get(ctx, linkedID)
			if err == nil && linked.Status == StatusHeld {
				log.Printf("[ENGINE] OTO: activating secondary order %s", linkedID)
				linked.Status = StatusPendingNew
				linked.UpdatedAt = time.Now()
				e.persist(ctx, linked)
			}
		}
	}
}

// -----------------------------------------------------------------------
// Broker routing helpers
// -----------------------------------------------------------------------

func (e *Engine) sendToBroker(ctx context.Context, o *Order, broker BrokerClient) error {
	now := time.Now()
	o.Status = StatusPendingNew
	o.SubmittedAt = &now
	o.UpdatedAt = now
	e.persist(ctx, o)

	brokerID, err := broker.SubmitOrder(ctx, o)
	if err != nil {
		return err
	}

	o.BrokerOrderID = brokerID
	o.Status = StatusAcknowledged
	o.UpdatedAt = time.Now()
	e.persist(ctx, o)
	e.notify(o)
	return nil
}

func (e *Engine) submitBracket(ctx context.Context, parent *Order, req SubmitRequest, broker BrokerClient) error {
	// Send the entry order
	if err := e.sendToBroker(ctx, parent, broker); err != nil {
		return err
	}

	// Build take-profit leg
	if req.TakeProfitPrice != nil {
		tp := e.buildLeg(parent, TypeLimit, req.TakeProfitPrice, nil, "bracket_tp")
		e.persist(ctx, tp)
		parent.LinkedIDs = append(parent.LinkedIDs, tp.ID)
	}

	// Build stop-loss leg
	if req.StopLossPrice != nil {
		var slType Type
		if req.StopLossLimit != nil {
			slType = TypeStopLimit
		} else {
			slType = TypeStop
		}
		sl := e.buildLeg(parent, slType, req.StopLossLimit, req.StopLossPrice, "bracket_sl")
		e.persist(ctx, sl)
		parent.LinkedIDs = append(parent.LinkedIDs, sl.ID)
	}

	e.persist(ctx, parent)
	return nil
}

func (e *Engine) submitOCO(ctx context.Context, parent *Order, req SubmitRequest, broker BrokerClient) error {
	if req.OCOPairRequest == nil {
		return fmt.Errorf("OCO order requires oco_pair")
	}

	pair, err := e.buildOrder(*req.OCOPairRequest)
	if err != nil {
		return err
	}

	parent.LinkedIDs = []string{pair.ID}
	parent.LinkType = "oco"
	pair.LinkedIDs = []string{parent.ID}
	pair.LinkType = "oco"

	e.persist(ctx, pair)
	e.persist(ctx, parent)

	if err := e.sendToBroker(ctx, parent, broker); err != nil {
		return err
	}
	return e.sendToBroker(ctx, pair, broker)
}

func (e *Engine) submitOTO(ctx context.Context, primary *Order, req SubmitRequest, broker BrokerClient) error {
	if req.OTOSecondary == nil {
		return fmt.Errorf("OTO order requires oto_secondary")
	}

	secondary, err := e.buildOrder(*req.OTOSecondary)
	if err != nil {
		return err
	}

	secondary.Status = StatusHeld // wait for primary fill
	secondary.LinkType = "oto"
	primary.LinkedIDs = []string{secondary.ID}
	primary.LinkType = "oto"

	e.persist(ctx, secondary)
	e.persist(ctx, primary)

	return e.sendToBroker(ctx, primary, broker)
}

func (e *Engine) submitOTOCO(ctx context.Context, primary *Order, req SubmitRequest, broker BrokerClient) error {
	// OTOCO = OTO where the secondary is an OCO pair
	if req.OTOSecondary == nil || req.OTOSecondary.OCOPairRequest == nil {
		return fmt.Errorf("OTOCO requires oto_secondary with oco_pair")
	}

	secondary, err := e.buildOrder(*req.OTOSecondary)
	if err != nil {
		return err
	}
	secondary.Status = StatusHeld

	ocoPair, err := e.buildOrder(*req.OTOSecondary.OCOPairRequest)
	if err != nil {
		return err
	}
	ocoPair.Status = StatusHeld

	secondary.LinkedIDs = []string{ocoPair.ID}
	secondary.LinkType = "oco"
	ocoPair.LinkedIDs = []string{secondary.ID}
	ocoPair.LinkType = "oco"
	primary.LinkedIDs = []string{secondary.ID, ocoPair.ID}
	primary.LinkType = "oto"

	e.persist(ctx, secondary)
	e.persist(ctx, ocoPair)
	e.persist(ctx, primary)

	return e.sendToBroker(ctx, primary, broker)
}

func (e *Engine) submitTWAP(ctx context.Context, o *Order, broker BrokerClient) error {
	if o.AlgoParams == nil || o.AlgoParams.EndTime == nil {
		return fmt.Errorf("TWAP requires algo_params with end_time")
	}

	params := o.AlgoParams
	sliceCount := params.SliceCount
	if sliceCount == 0 {
		sliceCount = 10
	}

	sliceQty := o.Qty / float64(sliceCount)
	duration := time.Until(*params.EndTime)
	interval := duration / time.Duration(sliceCount)

	o.Status = StatusAcknowledged
	e.persist(ctx, o)

	go func() {
		for i := 0; i < sliceCount; i++ {
			time.Sleep(interval)

			child := e.buildChildOrder(o, sliceQty, TypeMarket)
			e.persist(context.Background(), child)
			o.LinkedIDs = append(o.LinkedIDs, child.ID)

			if err := e.sendToBroker(context.Background(), child, broker); err != nil {
				log.Printf("[TWAP] Slice %d failed: %v", i, err)
			}
		}
		e.persist(context.Background(), o)
	}()

	return nil
}

func (e *Engine) submitVWAP(ctx context.Context, o *Order, broker BrokerClient) error {
	// VWAP slices based on volume profile — simplified: uniform slices over trading hours
	if o.AlgoParams == nil {
		return fmt.Errorf("VWAP requires algo_params")
	}

	sliceCount := 12 // one per 30 min in a 6.5hr trading day
	sliceQty := o.Qty / float64(sliceCount)
	interval := 30 * time.Minute

	o.Status = StatusAcknowledged
	e.persist(ctx, o)

	go func() {
		for i := 0; i < sliceCount; i++ {
			time.Sleep(interval)
			child := e.buildChildOrder(o, sliceQty, TypeMarket)
			e.persist(context.Background(), child)
			o.LinkedIDs = append(o.LinkedIDs, child.ID)
			e.sendToBroker(context.Background(), child, broker)
		}
		e.persist(context.Background(), o)
	}()

	return nil
}

func (e *Engine) submitIceberg(ctx context.Context, o *Order, broker BrokerClient) error {
	if o.VisibleQty == nil {
		return fmt.Errorf("iceberg order requires visible_qty")
	}

	// Send first visible slice
	visible := e.buildChildOrder(o, *o.VisibleQty, TypeLimit)
	visible.LimitPrice = o.LimitPrice
	o.ReserveQty = o.Qty - *o.VisibleQty
	o.Status = StatusAcknowledged

	e.persist(ctx, visible)
	e.persist(ctx, o)

	return e.sendToBroker(ctx, visible, broker)
}

func (e *Engine) submitTrailingStop(ctx context.Context, o *Order, broker BrokerClient) error {
	if o.TrailValue == nil {
		return fmt.Errorf("trailing stop requires trail_value")
	}
	// Alpaca natively supports trailing stops — pass through
	return e.sendToBroker(ctx, o, broker)
}

func (e *Engine) submitTouchTriggered(ctx context.Context, o *Order, broker BrokerClient) error {
	if o.TouchPrice == nil {
		return fmt.Errorf("MIT/LIT order requires touch_price")
	}
	// Hold the order; a market data monitor will trigger it
	o.Status = StatusHeld
	e.persist(ctx, o)
	e.notify(o)
	return nil
}

func (e *Engine) submitFunari(ctx context.Context, o *Order, broker BrokerClient) error {
	// Submit as a limit day order; a scheduler converts to MOC if unfilled near close
	o.TimeInForce = TIFDay
	if err := e.sendToBroker(ctx, o, broker); err != nil {
		return err
	}

	// Schedule conversion to MOC at 3:55 PM ET
	go func() {
		now := time.Now()
		// Calculate time until 3:55 PM ET today
		loc, _ := time.LoadLocation("America/New_York")
		closeTime := time.Date(now.Year(), now.Month(), now.Day(), 15, 55, 0, 0, loc)
		if time.Now().After(closeTime) {
			return
		}
		time.Sleep(time.Until(closeTime))

		current, err := e.Get(context.Background(), o.ID)
		if err != nil || current.Status == StatusFilled || current.Status == StatusCancelled {
			return
		}

		log.Printf("[FUNARI] Converting order %s to MOC", o.ID)
		e.Cancel(context.Background(), o.ID, broker)

		moc := e.buildChildOrder(o, o.Qty-o.FilledQty, TypeMOC)
		e.persist(context.Background(), moc)
		e.sendToBroker(context.Background(), moc, broker)
	}()

	return nil
}

// -----------------------------------------------------------------------
// Helpers
// -----------------------------------------------------------------------

func (e *Engine) buildOrder(req SubmitRequest) (*Order, error) {
	if req.Symbol == "" {
		return nil, fmt.Errorf("symbol is required")
	}
	if req.Qty == 0 && req.Notional == 0 {
		return nil, fmt.Errorf("qty or notional is required")
	}
	if req.TimeInForce == "" {
		req.TimeInForce = TIFDay
	}

	id := uuid.New().String()
	clientID := req.ClientOrderID
	if clientID == "" {
		clientID = "c-" + id[:8]
	}

	now := time.Now()
	return &Order{
		ID:            id,
		ClientOrderID: clientID,
		Symbol:        req.Symbol,
		AssetClass:    "us_equity",
		Side:          req.Side,
		Type:          req.Type,
		Qty:           req.Qty,
		Notional:      req.Notional,
		LimitPrice:    req.LimitPrice,
		StopPrice:     req.StopPrice,
		TrailType:     req.TrailType,
		TrailValue:    req.TrailValue,
		TimeInForce:   req.TimeInForce,
		ExpireAt:      req.ExpireAt,
		VisibleQty:    req.VisibleQty,
		AlgoParams:    req.AlgoParams,
		TouchPrice:    req.TouchPrice,
		ExtendedHours: req.ExtendedHours,
		Status:        StatusNew,
		CreatedAt:     now,
		UpdatedAt:     now,
	}, nil
}

func (e *Engine) buildLeg(parent *Order, orderType Type, limitPrice, stopPrice *float64, linkType string) *Order {
	// Flip side for closing legs
	side := SideSell
	if parent.Side == SideSell || parent.Side == SideSellShort {
		side = SideBuy
	}

	now := time.Now()
	return &Order{
		ID:          uuid.New().String(),
		Symbol:      parent.Symbol,
		AssetClass:  parent.AssetClass,
		Side:        side,
		Type:        orderType,
		Qty:         parent.Qty,
		LimitPrice:  limitPrice,
		StopPrice:   stopPrice,
		TimeInForce: TIFGTC,
		ParentID:    parent.ID,
		LinkType:    linkType,
		Status:      StatusHeld,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
}

func (e *Engine) buildChildOrder(parent *Order, qty float64, orderType Type) *Order {
	now := time.Now()
	return &Order{
		ID:          uuid.New().String(),
		Symbol:      parent.Symbol,
		AssetClass:  parent.AssetClass,
		Side:        parent.Side,
		Type:        orderType,
		Qty:         qty,
		TimeInForce: TIFDay,
		ParentID:    parent.ID,
		Status:      StatusNew,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
}

func (e *Engine) persist(ctx context.Context, o *Order) error {
	e.mu.Lock()
	if o.Status != StatusFilled && o.Status != StatusCancelled &&
		o.Status != StatusRejected && o.Status != StatusExpired {
		e.active[o.ID] = o
	} else {
		delete(e.active, o.ID)
	}
	e.mu.Unlock()

	if err := e.redis.SetOrder(ctx, o); err != nil {
		log.Printf("[ENGINE] Redis persist error for %s: %v", o.ID, err)
	}

	// DynamoDB write is async — don't block hot path
	go func() {
		if err := e.dynamo.PutOrder(context.Background(), o); err != nil {
			log.Printf("[ENGINE] DynamoDB persist error for %s: %v", o.ID, err)
		}
	}()

	return nil
}

func (e *Engine) getByBrokerID(ctx context.Context, brokerID string) (*Order, error) {
	e.mu.RLock()
	for _, o := range e.active {
		if o.BrokerOrderID == brokerID {
			e.mu.RUnlock()
			return o, nil
		}
	}
	e.mu.RUnlock()
	return e.redis.GetOrderByBrokerID(ctx, brokerID)
}

// notify pushes order updates to all SSE subscribers
func (e *Engine) notify(o *Order) {
	e.subsMu.RLock()
	defer e.subsMu.RUnlock()
	for _, ch := range e.subs {
		select {
		case ch <- o:
		default: // don't block if subscriber is slow
		}
	}
}

// Subscribe returns a channel that receives order updates
func (e *Engine) Subscribe(id string) <-chan *Order {
	ch := make(chan *Order, 64)
	e.subsMu.Lock()
	e.subs[id] = ch
	e.subsMu.Unlock()
	return ch
}

// Unsubscribe removes a subscriber
func (e *Engine) Unsubscribe(id string) {
	e.subsMu.Lock()
	if ch, ok := e.subs[id]; ok {
		close(ch)
		delete(e.subs, id)
	}
	e.subsMu.Unlock()
}

// -----------------------------------------------------------------------
// Status helpers
// -----------------------------------------------------------------------

func (s Status) IsCancellable() bool {
	switch s {
	case StatusNew, StatusPendingNew, StatusAcknowledged, StatusPartiallyFilled, StatusHeld:
		return true
	}
	return false
}

func (s Status) IsReplaceable() bool {
	switch s {
	case StatusAcknowledged, StatusPartiallyFilled:
		return true
	}
	return false
}

// -----------------------------------------------------------------------
// Types used by engine
// -----------------------------------------------------------------------

// BrokerClient is the interface the engine uses to talk to any broker
type BrokerClient interface {
	SubmitOrder(ctx context.Context, o *Order) (brokerOrderID string, err error)
	CancelOrder(ctx context.Context, brokerOrderID string) error
	ReplaceOrder(ctx context.Context, brokerOrderID string, req ReplaceRequest) (newBrokerOrderID string, err error)
	FillEvents() <-chan FillEvent
}

// FillEvent is emitted by the broker when an order is (partially) filled
type FillEvent struct {
	BrokerOrderID string
	FilledQty     float64
	FilledPrice   float64
	Timestamp     time.Time
}

// ListFilter for querying orders
type ListFilter struct {
	Symbol string
	Status Status
	Side   Side
	Limit  int
}
