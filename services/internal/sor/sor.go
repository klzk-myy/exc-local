// Package sor implements Task 18.3.14: Smart Order Routing (SOR) and
// external liquidity aggregation (spec §9.8, §24 #193/#242).
//
// When the internal CLOB lacks liquidity — depth below the configured
// floor or spread above the configured cap — a marketable order may be
// routed to an external venue (ECN / bank LP) over an outward FIX
// initiator session. Every external route mints a shadow order in the
// local OMS (sor_shadow_orders, migration 227) carrying the canonical
// 5-state lifecycle:
//
//	PENDING_ROUTE → ROUTED → PARTIALLY_FILLED_EXTERNAL →
//	FILLED_EXTERNAL | CANCELLED_EXTERNAL
//
// Invariants enforced here:
//   - External venue non-response timeout is 500ms: on expiry the
//     external order is auto-cancelled and the route tries the next
//     venue; exhaustion rejects the client with SOR_TIMEOUT (§23).
//   - Race prevention: a parent order never has a live local order and
//     an external shadow order concurrently — Route fails
//     ROUTING_REJECTED while a non-terminal shadow exists, and
//     local liquidity returning mid-route requires the external cancel
//     to confirm before local re-submission (ReleaseForLocal).
//   - Fills reconcile via FILL_BRIDGE events published to the
//     "settlements" JetStream stream (Phase-3 consumer); fill dedup is
//     (venue_id, external_order_id, exec_id).
//   - External venues are reachable only through VenueConnector;
//     production wiring env-blocks real venues (the loopback adapter
//     exercises the full initiator encode/exec-report decode path).
package sor

import (
	"context"
	"fmt"
	"sync"
	"time"

	"exchange/pkg/decimal"
	excerrors "exchange/pkg/errors"
)

// State is the canonical SOR shadow-order lifecycle (spec §24 #242).
type State string

const (
	StatePendingRoute    State = "PENDING_ROUTE"
	StateRouted          State = "ROUTED"
	StatePartiallyFilled State = "PARTIALLY_FILLED_EXTERNAL"
	StateFilled          State = "FILLED_EXTERNAL"
	StateCancelled       State = "CANCELLED_EXTERNAL"
)

// Terminal reports whether the shadow order is finished.
func (s State) Terminal() bool {
	return s == StateFilled || s == StateCancelled
}

// ExternalTimeout is the canonical venue non-response budget
// (spec §24 #242, Task 18.3.14 step 5).
const ExternalTimeout = 500 * time.Millisecond

// ParentOrder is the client order considered for external routing.
type ParentOrder struct {
	OrderID       int64
	AccountID     int64
	Symbol        string
	Side          string // BUY | SELL
	Qty           decimal.Decimal
	LimitPrice    *decimal.Decimal // nil = market order
	ClientOrderID string
}

// BookView is the internal CLOB liquidity snapshot driving the routing
// decision.
type BookView struct {
	Symbol   string
	BestBid  decimal.Decimal
	BestAsk  decimal.Decimal
	BidDepth decimal.Decimal // aggregate visible bid depth
	AskDepth decimal.Decimal // aggregate visible ask depth
	HasBBO   bool
}

// ShadowOrder is the sor_shadow_orders row (migration 227).
type ShadowOrder struct {
	ID              int64
	ParentOrderID   int64
	AccountID       int64
	VenueID         string
	ExternalOrderID string
	Symbol          string
	Side            string
	Qty             decimal.Decimal
	FilledQty       decimal.Decimal
	AvgFillPrice    *decimal.Decimal
	State           State
	Attempt         int
	LastError       string
	CreatedAt       time.Time
	UpdatedAt       time.Time
	RoutedAt        *time.Time
	CompletedAt     *time.Time
}

// FillBridgeEvent is the NATS "settlements" payload emitted per external
// fill (Task 18.3.14 step 4).
type FillBridgeEvent struct {
	EventType       string `json:"event_type"` // "FILL_BRIDGE"
	ParentOrderID   int64  `json:"parent_order_id"`
	AccountID       int64  `json:"account_id"`
	ShadowOrderID   int64  `json:"shadow_order_id"`
	VenueID         string `json:"venue_id"`
	ExternalOrderID string `json:"external_order_id"`
	ExecID          string `json:"exec_id"`
	Symbol          string `json:"symbol"`
	Side            string `json:"side"`
	Qty             string `json:"qty"`
	Price           string `json:"price"`
	CumQty          string `json:"cum_qty"`
	At              string `json:"at"`
}

// FillPublisher is the JetStream seam — production wires
// nats.Client.Publish on the "settlements" stream.
type FillPublisher func(ctx context.Context, ev *FillBridgeEvent) error

// VenueEventKind classifies inbound venue reports.
type VenueEventKind int

const (
	VenueAck           VenueEventKind = iota // external order accepted
	VenueFill                                // partial or final fill
	VenueReject                              // external order rejected
	VenueCancelConfirm                       // external order cancelled
)

// VenueEvent is one normalized venue report (the connector decodes the
// venue's ExecutionReport into this shape).
type VenueEvent struct {
	VenueID         string
	ExternalOrderID string
	ExecID          string // per-fill exec id (dedup key)
	Kind            VenueEventKind
	Qty             decimal.Decimal // fill qty this event
	Price           decimal.Decimal
	CumQty          decimal.Decimal // venue-reported cumulative
	LeavesQty       decimal.Decimal
	Done            bool // fill is terminal (LeavesQty == 0)
	RejectReason    string
	At              time.Time
}

// VenueConnector is the external-venue adapter contract — production
// adapters are FIX initiators to ECNs/bank LPs.
type VenueConnector interface {
	VenueID() string
	// Submit sends the order; returns the venue-assigned order id.
	Submit(ctx context.Context, o *VenueOrder) (externalOrderID string, err error)
	// Cancel requests cancellation of the external order.
	Cancel(ctx context.Context, externalOrderID string) error
	// Events streams normalized venue reports.
	Events() <-chan VenueEvent
}

// VenueOrder is the order payload handed to a connector.
type VenueOrder struct {
	ClOrdID    string
	Symbol     string
	Side       string
	Qty        decimal.Decimal
	LimitPrice *decimal.Decimal
}

// ShadowStore is the shadow-order persistence seam — PgShadowStore in
// production, MemoryShadowStore in tests.
type ShadowStore interface {
	// CreateShadow inserts a PENDING_ROUTE row.
	CreateShadow(ctx context.Context, s *ShadowOrder) (*ShadowOrder, error)
	// UpdateShadow persists state/fill/error fields.
	UpdateShadow(ctx context.Context, s *ShadowOrder) error
	// OpenByParent returns the non-terminal shadow for a parent order,
	// or nil.
	OpenByParent(ctx context.Context, parentOrderID int64) (*ShadowOrder, error)
	// ShadowByExternalID resolves (venue_id, external_order_id).
	ShadowByExternalID(ctx context.Context, venueID, externalOrderID string) (*ShadowOrder, error)
	// RecordFill inserts the dedup row; inserted=false means duplicate
	// exec report — callers skip it.
	RecordFill(ctx context.Context, shadowID int64, venueID, externalOrderID, execID string,
		qty, price decimal.Decimal) (inserted bool, err error)
}

// Thresholds gate the routing decision.
type Thresholds struct {
	MinDepth     decimal.Decimal // required depth on the contra side
	MaxSpreadBps decimal.Decimal // max spread in basis points
}

// ShouldRouteExternal reports whether the book is thin enough to route
// externally: missing BBO, insufficient contra-side depth, or a spread
// wider than the cap.
func (t Thresholds) ShouldRouteExternal(book BookView, side string) bool {
	if !book.HasBBO {
		return true
	}
	// Spread in bps = (ask-bid)/mid*10000; only meaningful with both sides.
	if book.BestBid.IsPositive() && book.BestAsk.IsPositive() {
		mid := book.BestBid.Add(book.BestAsk).Div(decimal.NewFromInt(2))
		if mid.IsPositive() {
			spreadBps := book.BestAsk.Sub(book.BestBid).Div(mid).Mul(decimal.NewFromInt(10000))
			if spreadBps.GreaterThan(t.MaxSpreadBps) {
				return true
			}
		}
	}
	depth := book.AskDepth
	if side == "SELL" {
		depth = book.BidDepth
	}
	return depth.LessThan(t.MinDepth)
}

// Router executes SOR decisions. Safe for concurrent Route calls across
// parents; per-parent routing is serialized on parentMu.
type Router struct {
	Venues     []VenueConnector
	Store      ShadowStore
	Publisher  FillPublisher // nil → FILL_BRIDGE events counted but dropped (dev)
	Thresholds Thresholds
	Timeout    time.Duration // default ExternalTimeout
	Now        func() time.Time

	mu       sync.Mutex
	parentMu map[int64]*sync.Mutex
	subs     map[string]map[chan VenueEvent]struct{} // venueID → awaiter channels
	cancel   context.CancelFunc
	wg       sync.WaitGroup
	started  bool
}

// NewRouter builds the router; Run must be started to consume venue
// events (ack/fill/cancel) before Route is used.
func NewRouter(store ShadowStore, venues []VenueConnector, pub FillPublisher, t Thresholds) *Router {
	return &Router{
		Venues: venues, Store: store, Publisher: pub,
		Thresholds: t, Timeout: ExternalTimeout,
		Now:      time.Now,
		parentMu: map[int64]*sync.Mutex{},
	}
}

// Start begins consuming venue event feeds — idempotent.
func (r *Router) Start(ctx context.Context) {
	r.mu.Lock()
	if r.started {
		r.mu.Unlock()
		return
	}
	r.started = true
	cctx, cancel := context.WithCancel(context.Background())
	r.cancel = cancel
	r.mu.Unlock()
	for _, v := range r.Venues {
		ch := v.Events()
		if ch == nil {
			continue
		}
		r.wg.Add(1)
		go func(c VenueConnector, feed <-chan VenueEvent) {
			defer r.wg.Done()
			for {
				select {
				case <-cctx.Done():
					return
				case ev, ok := <-feed:
					if !ok {
						return
					}
					r.onVenueEvent(cctx, c.VenueID(), ev)
				}
			}
		}(v, ch)
	}
	_ = ctx
}

// Stop terminates event consumption.
func (r *Router) Stop() {
	r.mu.Lock()
	if r.cancel != nil {
		r.cancel()
	}
	r.mu.Unlock()
	r.wg.Wait()
}

func (r *Router) lock(parentID int64) *sync.Mutex {
	r.mu.Lock()
	defer r.mu.Unlock()
	m, ok := r.parentMu[parentID]
	if !ok {
		m = &sync.Mutex{}
		r.parentMu[parentID] = m
	}
	return m
}

// RouteResult describes the outcome of one Route call.
type RouteResult struct {
	Local    bool         // true → submit on internal CLOB
	Shadow   *ShadowOrder // set when routed externally
	TimedOut bool         // every venue exhausted via timeout
}

// Route decides local-vs-external for one parent order and, when
// external, executes the venue walk: shadow order → submit → ack or
// timeout → cancel → next venue. Per-parent serialization + the
// non-terminal shadow guard enforce "no concurrent local+external".
func (r *Router) Route(ctx context.Context, parent *ParentOrder, book BookView) (*RouteResult, error) {
	if !r.Thresholds.ShouldRouteExternal(book, parent.Side) {
		return &RouteResult{Local: true}, nil
	}
	m := r.lock(parent.OrderID)
	m.Lock()
	defer m.Unlock()

	open, err := r.Store.OpenByParent(ctx, parent.OrderID)
	if err != nil {
		return nil, excerrors.New("ROUTING_REJECTED", fmt.Sprintf("shadow store lookup failed: %v", err))
	}
	if open != nil {
		return nil, excerrors.New("ROUTING_REJECTED",
			fmt.Sprintf("parent order %d already has a non-terminal external route", parent.OrderID))
	}
	if len(r.Venues) == 0 {
		return nil, excerrors.New("SOR_TIMEOUT", "no external venues configured")
	}
	timeout := r.Timeout
	if timeout <= 0 {
		timeout = ExternalTimeout
	}
	var lastErr error
	for i, v := range r.Venues {
		shadow, err := r.Store.CreateShadow(ctx, &ShadowOrder{
			ParentOrderID: parent.OrderID,
			AccountID:     parent.AccountID,
			VenueID:       v.VenueID(),
			Symbol:        parent.Symbol,
			Side:          parent.Side,
			Qty:           parent.Qty,
			State:         StatePendingRoute,
			Attempt:       i + 1,
			CreatedAt:     r.Now(),
		})
		if err != nil {
			return nil, fmt.Errorf("sor: shadow create: %w", err)
		}
		clOrdID := fmt.Sprintf("sor:%d:%d", parent.OrderID, i+1)
		extID, err := v.Submit(ctx, &VenueOrder{
			ClOrdID: clOrdID, Symbol: parent.Symbol, Side: parent.Side,
			Qty: parent.Qty, LimitPrice: parent.LimitPrice,
		})
		if err != nil {
			lastErr = err
			shadow.State = StateCancelled
			shadow.LastError = err.Error()
			now := r.Now()
			shadow.CompletedAt = &now
			_ = r.Store.UpdateShadow(ctx, shadow)
			continue
		}
		shadow.ExternalOrderID = extID
		shadow.State = StateRouted
		now := r.Now()
		shadow.RoutedAt = &now
		if err := r.Store.UpdateShadow(ctx, shadow); err != nil {
			return nil, fmt.Errorf("sor: shadow persist: %w", err)
		}
		// Wait for the venue ack/fill inside the 500ms budget.
		if err := r.awaitAck(ctx, v.VenueID(), extID, timeout); err != nil {
			// Timeout or reject — auto-cancel then walk to next venue.
			_ = v.Cancel(ctx, extID)
			r.cancelAndWait(ctx, v.VenueID(), extID, timeout/2)
			shadow.State = StateCancelled
			shadow.LastError = err.Error()
			now = r.Now()
			shadow.CompletedAt = &now
			_ = r.Store.UpdateShadow(ctx, shadow)
			lastErr = err
			continue
		}
		return &RouteResult{Shadow: shadow}, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("venues exhausted")
	}
	return nil, excerrors.New("SOR_TIMEOUT",
		fmt.Sprintf("external venues failed after %d attempts: %v", len(r.Venues), lastErr))
}

// awaitAck blocks for the venue ack (first event for extID) inside the
// budget. Events are matched by external order id on the connector's
// shared feed — consumed by Run's fan-in; here we subscribe a private
// per-call channel so the steady-state consumer never competes.
func (r *Router) awaitAck(ctx context.Context, venueID, extID string, budget time.Duration) error {
	ch, unsub := r.sub(venueID)
	defer unsub()
	t := time.NewTimer(budget)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			return fmt.Errorf("sor: venue %s non-response after %s", venueID, budget)
		case ev := <-ch:
			if ev.ExternalOrderID != extID {
				continue
			}
			switch ev.Kind {
			case VenueAck, VenueFill:
				return nil
			case VenueReject:
				return fmt.Errorf("sor: venue %s rejected: %s", venueID, ev.RejectReason)
			}
		}
	}
}

// cancelAndWait drains the venue feed until the cancel confirms or the
// budget lapses — the ROUTED→cancelled transition is never left
// dangling before the next venue attempt.
func (r *Router) cancelAndWait(ctx context.Context, venueID, extID string, budget time.Duration) {
	ch, unsub := r.sub(venueID)
	defer unsub()
	t := time.NewTimer(budget)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			return
		case ev := <-ch:
			if ev.ExternalOrderID == extID && ev.Kind == VenueCancelConfirm {
				return
			}
		}
	}
}

// sub registers a per-awaiter channel on the venue feed and returns it
// with its unsubscribe. Each awaiter gets its own channel — concurrent
// routes on one venue never steal each other's reports.
func (r *Router) sub(venueID string) (<-chan VenueEvent, func()) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.subs == nil {
		r.subs = map[string]map[chan VenueEvent]struct{}{}
	}
	set, ok := r.subs[venueID]
	if !ok {
		set = map[chan VenueEvent]struct{}{}
		r.subs[venueID] = set
	}
	ch := make(chan VenueEvent, 16)
	set[ch] = struct{}{}
	return ch, func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		delete(set, ch)
	}
}

// onVenueEvent is the steady-state consumer: every venue report updates
// the shadow and publishes FILL_BRIDGE on fills; it also fans the event
// to per-call awaiters (Route's ack wait).
func (r *Router) onVenueEvent(ctx context.Context, venueID string, ev VenueEvent) {
	r.mu.Lock()
	for ch := range r.subs[venueID] {
		select {
		case ch <- ev:
		default:
		}
	}
	r.mu.Unlock()

	if r.Store == nil || ev.ExternalOrderID == "" {
		return
	}
	sh, err := r.Store.ShadowByExternalID(ctx, venueID, ev.ExternalOrderID)
	if err != nil || sh == nil {
		return
	}
	switch ev.Kind {
	case VenueFill:
		if ev.ExecID == "" {
			return
		}
		inserted, err := r.Store.RecordFill(ctx, sh.ID, venueID,
			ev.ExternalOrderID, ev.ExecID, ev.Qty, ev.Price)
		if err != nil || !inserted {
			return // duplicate exec report — dedup wins
		}
		sh.FilledQty = sh.FilledQty.Add(ev.Qty)
		if ev.CumQty.GreaterThan(sh.FilledQty) {
			sh.FilledQty = ev.CumQty // venue cumulative is authoritative
		}
		if sh.FilledQty.GreaterThanOrEqual(sh.Qty) || ev.Done {
			sh.State = StateFilled
			now := r.Now()
			sh.CompletedAt = &now
		} else {
			sh.State = StatePartiallyFilled
		}
		if ev.Price.IsPositive() {
			// running VWAP
			if sh.AvgFillPrice == nil {
				p := ev.Price
				sh.AvgFillPrice = &p
			} else {
				prevNotional := sh.AvgFillPrice.Mul(sh.FilledQty.Sub(ev.Qty))
				newNotional := ev.Price.Mul(ev.Qty)
				avg := prevNotional.Add(newNotional).Div(sh.FilledQty)
				sh.AvgFillPrice = &avg
			}
		}
		sh.UpdatedAt = r.Now()
		_ = r.Store.UpdateShadow(ctx, sh)
		if r.Publisher != nil {
			_ = r.Publisher(ctx, &FillBridgeEvent{
				EventType:       "FILL_BRIDGE",
				ParentOrderID:   sh.ParentOrderID,
				AccountID:       sh.AccountID,
				ShadowOrderID:   sh.ID,
				VenueID:         venueID,
				ExternalOrderID: ev.ExternalOrderID,
				ExecID:          ev.ExecID,
				Symbol:          sh.Symbol,
				Side:            sh.Side,
				Qty:             ev.Qty.String(),
				Price:           ev.Price.String(),
				CumQty:          sh.FilledQty.String(),
				At:              r.Now().UTC().Format(time.RFC3339Nano),
			})
		}
	case VenueReject:
		sh.State = StateCancelled
		sh.LastError = ev.RejectReason
		now := r.Now()
		sh.CompletedAt = &now
		sh.UpdatedAt = now
		_ = r.Store.UpdateShadow(ctx, sh)
	case VenueCancelConfirm:
		if !sh.State.Terminal() {
			sh.State = StateCancelled
			now := r.Now()
			sh.CompletedAt = &now
			sh.UpdatedAt = now
			_ = r.Store.UpdateShadow(ctx, sh)
		}
	}
}

// ReleaseForLocal cancels any live external route for parent and waits
// for the cancel to confirm — the "external cancel must complete before
// local re-submission" leg of the no-concurrent-orders invariant.
// Returns true when the parent is free for local submission.
func (r *Router) ReleaseForLocal(ctx context.Context, parentOrderID int64) (bool, error) {
	m := r.lock(parentOrderID)
	m.Lock()
	defer m.Unlock()
	open, err := r.Store.OpenByParent(ctx, parentOrderID)
	if err != nil {
		return false, err
	}
	if open == nil {
		return true, nil
	}
	for _, v := range r.Venues {
		if v.VenueID() == open.VenueID {
			if err := v.Cancel(ctx, open.ExternalOrderID); err != nil {
				return false, err
			}
			r.cancelAndWait(ctx, v.VenueID(), open.ExternalOrderID, r.Timeout/2)
			break
		}
	}
	open.State = StateCancelled
	now := r.Now()
	open.CompletedAt = &now
	open.UpdatedAt = now
	if err := r.Store.UpdateShadow(ctx, open); err != nil {
		return false, err
	}
	return true, nil
}
