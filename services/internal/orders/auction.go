// Phase-16 Task 16.3.25 — MOO/MOC session-uncross orders (spec §6.2b,
// §24 #399) plus the composite lifecycle dispatcher that connects the
// engine's out-ring events to bracket (16.3.14) and order-list
// (16.3.20) state machines.
//
// MOO/MOC submit as RESERVED rows — they never participate in
// continuous matching. The §7.1 auction scheduler builds the
// `instrument:auction:{symbol}:queue` payload from resting MOO/MOC
// rows and the armed CALL key freezes amends/cancels; the Injector
// below replays queued orders onto the engine as MARKET OrderNew
// events once the CALL arms, so the call-auction book uncrosses them
// at the single max-volume price. Unfilled remainder cancels through
// the scheduler's CancelOrderType sweep (reason AUCTION_CANCELLED).
package orders

import (
	"context"
	"strconv"
	"strings"
	"time"

	flatbuffers "github.com/google/flatbuffers/go"

	"exchange/internal/ipc"
	"exchange/internal/ipc/wire"
	"exchange/pkg/decimal"
)

// Private-channel vocabulary (§6.2b item 6).
const (
	ChanOrderQueued      = "order.queued"
	ChanOrderAuctionFill = "order.auction_fill"
	ChanOrderCancelled   = "order.cancelled"
)

// AUCTION_CANCELLED is the §6.2b rejection/cancellation reason surfaced
// on remainder cancels — also the wire detail on cancelled notifications.
const ReasonAuctionCancelled = "AUCTION_CANCELLED"

// notify emits a private WS frame via the bound seam — nil-safe (tests
// and unwired dev builds skip silently, mirroring the OTR convention).
func (s *Service) notify(accountID int64, channel string, data any) {
	if s.notifyFn == nil {
		return
	}
	defer func() { _ = recover() }() // never let a WS hiccup kill a pipeline path
	s.notifyFn(accountID, channel, data)
}

// ---------------------------------------------------------------------------
// Submit / cancel / amend integration (Task 16.3.25 items 1, 5)
// ---------------------------------------------------------------------------

// queueAuctionOrder completes the MOO/MOC submit path after the row is
// inserted: RESERVED status + the §6.2b `order.queued` private event.
// No wire dispatch — the engine only sees the order when the armed
// CALL lets the injector replay it.
func (s *Service) queueAuctionOrder(ctx context.Context, o *Order) (*Ack, error) {
	if err := s.store.MarkReserved(ctx, o.ID); err != nil {
		return nil, errInternal("auction queue", err)
	}
	s.notify(o.AccountID, ChanOrderQueued, map[string]any{
		"order_id":        o.ID,
		"client_order_id": o.ClientOrderID,
		"instrument_id":   o.InstrumentID,
		"side":            o.Side,
		"type":            o.OrderType,
		"quantity":        o.Quantity.String(),
		"status":          "RESERVED",
		"queued_at":       s.now().UTC().Format(time.RFC3339Nano),
	})
	return &Ack{OrderID: o.ID, ClientOrderID: o.ClientOrderID,
		Status: "RESERVED", OrderSeq: o.OrderSeq,
		TransactTime: s.now().UTC().Format(time.RFC3339Nano)}, nil
}

// assertAuctionMutable gates a client-initiated cancel/amend on a
// queued session order through the §6.2b T-30s freeze (item 5): frozen
// ⇒ AMEND_IN_AUCTION_REJECTED. A gate error fails closed.
func (s *Service) assertAuctionMutable(ctx context.Context, o *Order,
	inst *Instrument) error {
	if s.auction == nil {
		return codeErr("AMEND_IN_AUCTION_REJECTED",
			"auction freeze gate unavailable — mutation rejected (fail closed)")
	}
	frozen, err := s.auction.Frozen(ctx, inst, o.OrderType, s.now())
	if err != nil {
		return codeErr("AMEND_IN_AUCTION_REJECTED",
			"auction freeze state unreadable — mutation rejected: %v", err)
	}
	if frozen {
		return codeErr("AMEND_IN_AUCTION_REJECTED",
			"order %d is frozen for the %s call auction (T-30s)", o.ID, inst.Symbol)
	}
	return nil
}

// cancelQueued cancels a never-dispatched auction order locally — the
// row is all that exists, so ApplyCancel + the notification replace the
// wire-confirm path. Reason distinguishes a client cancel from the
// scheduler's AUCTION_UNFILLED_REMAINDER sweep (surfaced as
// AUCTION_CANCELLED per spec).
func (s *Service) cancelQueued(ctx context.Context, o *Order, reason string) error {
	if err := s.store.ApplyCancel(ctx, o.ID); err != nil {
		return errInternal("order cancel", err)
	}
	out := ReasonAuctionCancelled
	if reason != "AUCTION_UNFILLED_REMAINDER" && reason != ReasonAuctionCancelled {
		out = reason
	}
	s.notify(o.AccountID, ChanOrderCancelled, map[string]any{
		"order_id":        o.ID,
		"client_order_id": o.ClientOrderID,
		"instrument_id":   o.InstrumentID,
		"type":            o.OrderType,
		"status":          "CANCELLED",
		"reason":          out,
	})
	return nil
}

// auctionInBook reports whether an auction order has already been
// injected into the engine's call-auction book (ACTIVE /
// PARTIALLY_FILLED) — RESERVED rows are queue-local only.
func auctionInBook(o *Order) bool {
	return o.Status == "ACTIVE" || o.Status == "PARTIALLY_FILLED"
}

// ---------------------------------------------------------------------------
// Injector — replays queued MOO/MOC onto the engine when CALL arms
// ---------------------------------------------------------------------------

// ArmedFeed reads the `instrument:auction:{symbol}` control key —
// "" means no CALL is armed (admin.RedisStatusFeed implementation is
// bound in cmd/gateway). AuctionQueue returns the scheduler-published
// `instrument:auction:{symbol}:queue` order-id list (empty when no
// scheduler-built queue exists — e.g. a reopening CALL).
type ArmedFeed interface {
	AuctionArmed(ctx context.Context, symbol string) (string, error)
	AuctionQueue(ctx context.Context, symbol string) ([]int64, error)
}

// Injector watches for armed call auctions and replays every queued
// MOO/MOC order of the instrument as a MARKET OrderNew — the engine's
// call-auction machinery parks them until the uncross (the armed key
// also makes the book non-continuous, so nothing matches early).
type Injector struct {
	svc  *Service
	feed ArmedFeed
	poll time.Duration
	logf func(string, ...any)
}

// NewInjector wires the replay loop. poll ≤ 0 defaults to 2s — well
// inside the 5-minute CALL window.
func NewInjector(svc *Service, feed ArmedFeed, poll time.Duration,
	logf func(string, ...any)) *Injector {
	if poll <= 0 {
		poll = 2 * time.Second
	}
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &Injector{svc: svc, feed: feed, poll: poll, logf: logf}
}

// Run polls until ctx is cancelled.
func (in *Injector) Run(ctx context.Context) {
	t := time.NewTicker(in.poll)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			in.sweep(ctx)
		}
	}
}

// sweep dispatches queued auction orders on instruments with an armed
// CALL. One bad symbol never blocks the rest (fail-open on a single
// instrument, fail-closed on persistence — an undispatchable order
// stays RESERVED and retries next tick).
//
// Queue discipline (§6.2b): when the scheduler published a :queue id
// list only those ids inject — a close auction uncrosses exactly the
// pre-close accumulation. When no queue list exists (a reopening CALL
// after HALT/SUSPEND→ACTIVE) only MOO orders inject — §6.2b pins MOO to
// "first auction after HALT/SUSPEND→ACTIVE" while MOC remains bound to
// the scheduled close.
func (in *Injector) sweep(ctx context.Context) {
	for _, typ := range []string{TypeMOO, TypeMOC} {
		resting, err := in.svc.store.OpenOrders(ctx, MassCancelScope{
			OrderType: typ,
		})
		if err != nil {
			in.logf("auction injector: open %s scan: %v", typ, err)
			continue
		}
		for i := range resting {
			o := &resting[i]
			if o.Status != "RESERVED" && o.Status != "PENDING" {
				continue // already injected (ACTIVE/PARTIAL) — engine-owned
			}
			inst, err := in.svc.store.InstrumentByID(ctx, o.InstrumentID)
			if err != nil || inst == nil {
				continue
			}
			armed, err := in.feed.AuctionArmed(ctx, inst.Symbol)
			if err != nil || armed == "" {
				continue // not armed (or unreadable — retry next tick)
			}
			queue, err := in.feed.AuctionQueue(ctx, inst.Symbol)
			if err != nil {
				continue // queue unreadable — retry next tick
			}
			if len(queue) > 0 {
				if !queueHas(queue, o.ID) {
					continue
				}
			} else if o.OrderType != TypeMOO {
				continue // reopening CALL — MOO only
			}
			in.dispatch(ctx, o, inst, armed)
		}
	}
}

func queueHas(ids []int64, id int64) bool {
	for _, v := range ids {
		if v == id {
			return true
		}
	}
	return false
}

// dispatch sends one queued order as a MARKET OrderNew on its shard
// and flips it ACTIVE — the engine's call-auction book now owns it;
// fills arrive as ordinary TradeFill events (emitted to the client as
// order.auction_fill by OnFill). The armed-key deadline stamps the
// wire GTD expiry — §6.2b "implicitly GTD with expiry at the auction":
// an uninjected/unfilled order dies at the uncross deadline (engine
// emits the CancelReasonExpired echo) instead of resting forever.
func (in *Injector) dispatch(ctx context.Context, o *Order, inst *Instrument,
	armed string) {
	if in.svc.sub == nil {
		return
	}
	acct, err := in.svc.store.AccountByID(ctx, o.AccountID)
	if err != nil || acct == nil {
		return
	}
	shard := uint16(0)
	if o.ShardID != nil {
		shard = uint16(*o.ShardID)
	}
	msg := orderNewMsg(o, acct, nil)
	// "CALL:{ns}" / "EXTEND:{ns}" — the deadline doubles as the
	// implicit auction GTD expiry.
	if i := strings.IndexByte(armed, ':'); i >= 0 {
		if ns, perr := strconv.ParseInt(armed[i+1:], 10, 64); perr == nil {
			msg.TIF = wire.TimeInForceGTD
			msg.GtdExpiryNs = ns
		}
	}
	seq := in.svc.seq.Next()
	b := flatbuffers.NewBuilder(256)
	payload := ipc.EncodeOrderNewEvent(b, seq,
		uint64(in.svc.now().UnixNano()), msg)
	if err := in.svc.sub.Send(ctx, shard, payload); err != nil {
		in.logf("auction injector: dispatch %s order %d: %v",
			o.OrderType, o.ID, err)
		return // stays RESERVED — retried while the CALL lasts
	}
	_ = in.svc.store.MarkActive(ctx, o.ID)
}

// ---------------------------------------------------------------------------
// Composite + auction lifecycle dispatch — fill/cancel fan-in
// ---------------------------------------------------------------------------

// OnFill is invoked by the consumer's fill hook after ApplyFill lands
// (Task 16.3.14 children spawn; Task 16.3.20 activation; Task 16.3.25
// `order.auction_fill` emission). Errors are contained — the hook must
// never break the read-model drain.
func (s *Service) OnFill(ctx context.Context, orderID int64,
	price, qty decimal.Decimal) {
	o, err := s.store.GetOrder(ctx, orderID)
	if err != nil || o == nil {
		return
	}
	if IsAuctionType(o.OrderType) {
		s.notify(o.AccountID, ChanOrderAuctionFill, map[string]any{
			"order_id":      o.ID,
			"instrument_id": o.InstrumentID,
			"side":          o.Side,
			"type":          o.OrderType,
			"price":         price.String(),
			"quantity":      qty.String(),
			"filled_qty":    o.FilledQty.String(),
			"status":        o.Status,
		})
	}
	if s.composite == nil {
		return
	}
	if b, err := s.composite.BracketByParent(ctx, orderID); err == nil && b != nil {
		s.onBracketFill(ctx, b)
		return
	}
	if l, legs, err := s.composite.OrderListByWorking(ctx, orderID); err == nil && l != nil {
		s.onListWorkingFill(ctx, l, legs, o)
	}
}

// OnCancel is invoked by the consumer's cancel hook after ApplyCancel —
// bracket parent cancels cascade to placed children; list leg cancels
// advance the list state machine; engine-cancelled auction orders emit
// the §6.2b `order.cancelled` notice (reason AUCTION_CANCELLED — covers
// both the unfilled-remainder sweep and a rejected/withdrawn CALL).
func (s *Service) OnCancel(ctx context.Context, orderID int64) {
	if o, err := s.store.GetOrder(ctx, orderID); err == nil && o != nil &&
		IsAuctionType(o.OrderType) {
		s.notify(o.AccountID, ChanOrderCancelled, map[string]any{
			"order_id":        o.ID,
			"client_order_id": o.ClientOrderID,
			"instrument_id":   o.InstrumentID,
			"type":            o.OrderType,
			"status":          "CANCELLED",
			"reason":          ReasonAuctionCancelled,
		})
	}
	if s.composite == nil {
		return
	}
	if b, err := s.composite.BracketByParent(ctx, orderID); err == nil && b != nil {
		s.onBracketCancel(ctx, b)
		return
	}
	if l, legs, err := s.composite.OrderListByLegOrder(ctx, orderID); err == nil && l != nil {
		s.onLegCancel(ctx, l, legs, orderID)
	}
}

// RecoverComposites re-drives every non-terminal composite at boot —
// brackets whose parent filled while the gateway was down get their
// uncovered children placed; EXECUTING lists whose working leg
// completed activate pending legs; lists whose working leg died
// close EXPIRED/CANCELLED (§24 #287 restart-safety: composite state is
// reconstructable from orders + fills alone).
func (s *Service) RecoverComposites(ctx context.Context, logf func(string, ...any)) {
	if s.composite == nil {
		return
	}
	if logf == nil {
		logf = func(string, ...any) {}
	}
	brackets, err := s.composite.OpenBrackets(ctx)
	if err != nil {
		logf("composite recovery: open brackets: %v", err)
	}
	for i := range brackets {
		b := &brackets[i]
		parent, err := s.store.GetOrder(ctx, b.ParentOrderID)
		if err != nil || parent == nil {
			continue
		}
		switch parent.Status {
		case "FILLED", "PARTIALLY_FILLED":
			s.onBracketFill(ctx, b)
		case "CANCELLED", "REJECTED", "EXPIRED":
			if parent.Status == "REJECTED" {
				_, _ = s.composite.SetBracketState(ctx, b.ID,
					[]string{BracketWorking}, BracketFailed)
			} else {
				s.onBracketCancel(ctx, b)
			}
		}
	}
	lists, err := s.composite.OpenOrderLists(ctx)
	if err != nil {
		logf("composite recovery: open lists: %v", err)
	}
	for i := range lists {
		l := &lists[i]
		_, legs, err := s.composite.OrderListGet(ctx, l.ID)
		if err != nil {
			continue
		}
		wo, err := s.store.GetOrder(ctx, l.WorkingOrderID)
		if err != nil || wo == nil {
			continue
		}
		switch wo.Status {
		case "FILLED":
			s.onListWorkingFill(ctx, l, legs, wo)
		case "CANCELLED", "REJECTED", "EXPIRED":
			to := ListStateCancelled
			if wo.Status == "EXPIRED" {
				to = ListStateExpired
			}
			_, _ = s.composite.SetOrderListState(ctx, l.ID,
				[]string{ListStateExecuting}, to, "OPO_PARENT_FAILED")
		}
	}
}
