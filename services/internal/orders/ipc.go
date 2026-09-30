// Wire-path of the order pipeline: FlatBuffers encoding onto the
// exc.wire schema (core/proto/exchange.fbs), the per-shard submitter over
// the shared-memory/Aeron channel (internal/ipc), the outbound consumer
// that keeps the read model honest, and the pending-confirmation
// registry that makes "cancel returns after core confirmation" real.
package orders

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	flatbuffers "github.com/google/flatbuffers/go"

	"exchange/internal/ipc"
	"exchange/internal/ipc/wire"
	"exchange/internal/tracing"
	"exchange/pkg/decimal"
)

// ---------------------------------------------------------------------------
// Encoding — exc.wire Event envelopes
// ---------------------------------------------------------------------------

// Cancel-reason codes mirror the C++ kWalCancelReason* constants
// (core/include/matching/WalWriter.hpp) — the engine stamps them on the
// outbound OrderCancel so the read model can tell an OCO sibling cancel
// (7) from a user cancel (0).
const (
	CancelReasonUser         = 0
	CancelReasonExpired      = 1
	CancelReasonStp          = 2
	CancelReasonFokUnfilled  = 3
	CancelReasonIocRemainder = 4
	CancelReasonSlippage     = 5
	CancelReasonExecRange    = 6
	// CancelReasonOcoLink — sibling leg reached terminal FILLED first;
	// this leg was cancelled atomically in the same engine dispatch
	// (spec §6.5). The doomed-leg inbound reject shares the reason.
	CancelReasonOcoLink = 7
)

// EncodeCancelEvent serializes Event{seq, ts, OrderCancel{order_id, account_id}}.
func EncodeCancelEvent(b *flatbuffers.Builder, seq, ts uint64, orderID, accountID uint64) []byte {
	wire.OrderCancelStart(b)
	wire.OrderCancelAddOrderId(b, orderID)
	wire.OrderCancelAddAccountId(b, accountID)
	oc := wire.OrderCancelEnd(b)

	wire.EventStart(b)
	wire.EventAddSeq(b, seq)
	wire.EventAddTs(b, ts)
	wire.EventAddTypeType(b, wire.EventTypeOrderCancel)
	wire.EventAddType(b, oc)
	b.Finish(wire.EventEnd(b))
	return b.FinishedBytes()
}

// EncodeAmendEvent serializes Event{seq, ts, OrderAmend{...}}. The wire
// convention is 0=unchanged for price/qty/stop/gtd_expiry_ns; order_seq
// is the engine's §6.9 #1 amend fence (stale → STALE_MODIFY in-core).
func EncodeAmendEvent(b *flatbuffers.Builder, seq, ts uint64,
	orderID, orderSeq uint64, price, qty, stop, gtdExpiryNs int64) []byte {
	wire.OrderAmendStart(b)
	wire.OrderAmendAddOrderId(b, orderID)
	wire.OrderAmendAddOrderSeq(b, orderSeq)
	wire.OrderAmendAddPrice(b, price)
	wire.OrderAmendAddQty(b, qty)
	wire.OrderAmendAddStopPrice(b, stop)
	wire.OrderAmendAddGtdExpiryNs(b, gtdExpiryNs)
	oa := wire.OrderAmendEnd(b)

	wire.EventStart(b)
	wire.EventAddSeq(b, seq)
	wire.EventAddTs(b, ts)
	wire.EventAddTypeType(b, wire.EventTypeOrderAmend)
	wire.EventAddType(b, oa)
	b.Finish(wire.EventEnd(b))
	return b.FinishedBytes()
}

// EncodeOcoLinkEvent serializes Event{seq, ts, OcoLink{link_id, order_id_a,
// order_id_b, account_id, instrument_id}} — the Phase-14 Task 14.3.1
// pair-linkage command (spec §6.2/§6.5). It MUST be sent on the shard
// ring BEFORE either leg's OrderNew: the SPSC ring preserves order, so the
// engine installs the link while both legs are still unplaced.
func EncodeOcoLinkEvent(b *flatbuffers.Builder, seq, ts, linkID,
	orderIDA, orderIDB, accountID uint64, instrumentID uint32) []byte {
	wire.OcoLinkStart(b)
	wire.OcoLinkAddLinkId(b, linkID)
	wire.OcoLinkAddOrderIdA(b, orderIDA)
	wire.OcoLinkAddOrderIdB(b, orderIDB)
	wire.OcoLinkAddAccountId(b, accountID)
	wire.OcoLinkAddInstrumentId(b, instrumentID)
	ol := wire.OcoLinkEnd(b)

	wire.EventStart(b)
	wire.EventAddSeq(b, seq)
	wire.EventAddTs(b, ts)
	wire.EventAddTypeType(b, wire.EventTypeOcoLink)
	wire.EventAddType(b, ol)
	b.Finish(wire.EventEnd(b))
	return b.FinishedBytes()
}

// stpModeByte maps the §5.4 stp_mode string onto the engine's StpMode
// ordinal (core Order.hpp: 0..4); "" → 0xFF unset so the engine resolves
// order → account default → CANCEL_NEWEST (Task 2.3.21).
func stpModeByte(mode string) byte {
	switch mode {
	case "CANCEL_NEWEST":
		return 0
	case "CANCEL_OLDEST":
		return 1
	case "CANCEL_BOTH":
		return 2
	case "DECREMENT":
		return 3
	case "NONE":
		return 4
	default:
		return 0xFF
	}
}

// pegModeByte maps the §5.4 peg_mode vocabulary onto the Phase-16 wire
// ordinals (exchange.fbs OrderNew.peg_mode).
func pegModeByte(mode string) byte {
	switch mode {
	case PegModeMid:
		return 1
	case PegModePrimary:
		return 2
	case PegModeMarket:
		return 3
	default:
		return 0
	}
}

// triggerSourceByte maps migration-066 trigger_source onto the wire
// ordinals; the empty string means LAST_PRICE (column default).
func triggerSourceByte(src string) byte {
	switch src {
	case TriggerSourceMark:
		return 1
	case TriggerSourceIndex:
		return 2
	default:
		return 0 // "" / LAST_PRICE
	}
}

// orderNewMsg maps a validated submit onto wire.OrderNewMsg. req carries
// the request-only fields the orders row doesn't materialize (gtd_expiry);
// the persisted Order supplies the rest.
func orderNewMsg(o *Order, acct *Account, req *SubmitRequest) ipc.OrderNewMsg {
	m := ipc.OrderNewMsg{
		OrderID:       uint64(o.ID),
		AccountID:     uint64(o.AccountID),
		InstrumentID:  uint32(o.InstrumentID),
		ClientOrderID: o.ClientOrderID,
		StpMode:       0xFF,
	}
	if o.Side == SideSell {
		m.Side = wire.SideSell
	} else {
		m.Side = wire.SideBuy
	}
	switch o.OrderType {
	case TypeMarket, TypeMOO, TypeMOC:
		// MOO/MOC reach the wire only through the auction injector —
		// they uncross as plain MARKET orders in the armed CALL book.
		m.Type = wire.OrderTypeMarket
	case TypeLimit:
		m.Type = wire.OrderTypeLimit
	case TypeStop:
		m.Type = wire.OrderTypeStopMarket
	case TypeStopLimit:
		m.Type = wire.OrderTypeStopLimit
	case TypeIceberg:
		m.Type = wire.OrderTypeIceberg
	case TypePeg:
		m.Type = wire.OrderTypePeg
	}
	switch o.TimeInForce {
	case TIFIOC:
		m.TIF = wire.TimeInForceIOC
	case TIFFOK:
		m.TIF = wire.TimeInForceFOK
	case TIFGTD:
		m.TIF = wire.TimeInForceGTD
	case TIFDAY:
		m.TIF = wire.TimeInForceDAY
	default:
		m.TIF = wire.TimeInForceGTC
	}
	m.Qty = decimal.Scaled(o.Quantity)
	if o.Price != nil {
		m.Price = decimal.Scaled(*o.Price)
	}
	if o.StopPrice != nil {
		m.StopPrice = decimal.Scaled(*o.StopPrice)
	}
	if o.DisplayQty != nil {
		m.DisplayQty = decimal.Scaled(*o.DisplayQty)
	}
	m.StpMode = stpModeByte(o.STPMode)
	if o.PostOnly {
		m.Flags |= 1
	}
	if o.ReduceOnly {
		m.Flags |= 2
	}
	// Phase-16 Task 16.3.13/16.3.16 flags ride bits 2/3 (schema comment).
	if o.Hidden {
		m.Flags |= 4
	}
	if o.GSLO {
		m.Flags |= 8
	}
	// Phase-16 Task 16.3.10/.11/.17 aux — the engine sibling decodes;
	// zeros are the wire "unset" convention.
	if o.PegMode != nil {
		m.PegMode = pegModeByte(*o.PegMode)
	}
	if o.PegOffset != nil {
		m.PegOffset = decimal.Scaled(*o.PegOffset)
	}
	if o.PegLimit != nil {
		m.PegLimit = decimal.Scaled(*o.PegLimit)
	}
	m.TriggerSource = triggerSourceByte(o.TriggerSource)
	if acct != nil && acct.TradeGroupID != nil {
		m.TradeGroupID = uint32(*acct.TradeGroupID)
	}
	if req != nil && req.GTDExpiry != nil {
		m.GtdExpiryNs = req.GTDExpiry.UnixNano()
	}
	return m
}

// ---------------------------------------------------------------------------
// Submitter — per-shard outbound channel fan-out
// ---------------------------------------------------------------------------

// Submitter abstracts the engine-bound transport so tests can fake it.
type Submitter interface {
	// Send publishes one encoded event to the shard's in-ring. Returns
	// ENGINE_OVERLOAD on a full ring (§2.7.3 backpressure) and
	// SERVICE_DEGRADED when the engine image is absent/unmappable.
	Send(ctx context.Context, shard uint16, payload []byte) error
	// Channel exposes the underlying channel for the event consumer.
	Channel(shard uint16) (*ipc.Channel, error)
}

// ShmSubmitter opens {base}_{shard}_in rings lazily as the gateway
// producer. The C++ core creates the images; a missing image fails
// closed with SERVICE_DEGRADED.
type ShmSubmitter struct {
	base string
	mu   sync.Mutex
	ch   map[uint16]*ipc.Channel
}

func NewShmSubmitter(base string) *ShmSubmitter {
	if base == "" {
		base = ipc.DefaultShmBase
	}
	return &ShmSubmitter{base: base, ch: make(map[uint16]*ipc.Channel)}
}

func (s *ShmSubmitter) Channel(shard uint16) (*ipc.Channel, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if c, ok := s.ch[shard]; ok {
		return c, nil
	}
	c, err := ipc.OpenChannel(s.base, shard, ipc.EndpointGateway, false,
		ipc.DefaultRingCapacity, ipc.DefaultRingSlotPayload)
	if err != nil {
		return nil, codeErr("SERVICE_DEGRADED",
			"engine IPC channel shard %d unavailable: %v", shard, err)
	}
	s.ch[shard] = c
	return c, nil
}

func (s *ShmSubmitter) Send(ctx context.Context, shard uint16, payload []byte) error {
	c, err := s.Channel(shard)
	if err != nil {
		return err
	}
	// Task 9.3.11 — HTTP -> Aeron -> C++ trace continuity: prepend the
	// 64B EXCTRACE block when the caller carries a span context. The
	// engine decodes the Event at +64 when the magic is present and
	// echoes the block verbatim on emitted frames. Untraced sends keep
	// the legacy layout. Tracing never rejects traffic: a frame that
	// would exceed the slot with the block ships untraced, never
	// dropped.
	if _, ok := tracing.SpanContextFrom(ctx); ok &&
		len(payload)+tracing.AeronTraceHeaderLen <= int(ipc.DefaultRingSlotPayload) {
		frame := make([]byte, tracing.AeronTraceHeaderLen+len(payload))
		tracing.InjectAeronTrace(ctx, frame[:tracing.AeronTraceHeaderLen])
		copy(frame[tracing.AeronTraceHeaderLen:], payload)
		payload = frame
	}
	if !c.Send(payload) {
		return codeErr("ENGINE_OVERLOAD",
			"ingress ring for shard %d is full", shard)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Per-shard sequence allocator + pending-confirmation registry
// ---------------------------------------------------------------------------

// seqAllocator mints strictly-increasing per-shard event sequences.
// Seeded with the wall clock so restarts never reuse a lower seq than a
// live engine has already fenced (§6.9 #1 amend fence).
type seqAllocator struct{ v atomic.Uint64 }

func newSeqAllocator() *seqAllocator {
	a := &seqAllocator{}
	a.v.Store(uint64(time.Now().UnixNano()))
	return a
}

func (a *seqAllocator) Next() uint64 { return a.v.Add(1) }

// pendingCancel tracks an in-flight cancel awaiting the engine's
// outbound OrderCancel echo.
type pendingCancel struct {
	orderID   uint64
	done      chan struct{}
	succeeded atomic.Bool
}

// pendingConfirms is the correlation registry between dispatched
// cancels and the out-ring consumer. Registration MUST precede Send —
// the echo may land faster than the register call otherwise.
type pendingConfirms struct {
	mu sync.Mutex
	m  map[uint64]*pendingCancel
}

func newPendingConfirms() *pendingConfirms {
	return &pendingConfirms{m: make(map[uint64]*pendingCancel)}
}

// register installs the wait channel; caller must deregister on return.
func (p *pendingConfirms) register(orderID uint64) *pendingCancel {
	pc := &pendingCancel{orderID: orderID, done: make(chan struct{})}
	p.mu.Lock()
	p.m[orderID] = pc
	p.mu.Unlock()
	return pc
}

func (p *pendingConfirms) deregister(orderID uint64) {
	p.mu.Lock()
	delete(p.m, orderID)
	p.mu.Unlock()
}

// resolve is called by the consumer when an outbound OrderCancel names
// an order we are waiting on.
func (p *pendingConfirms) resolve(orderID uint64) bool {
	p.mu.Lock()
	pc, ok := p.m[orderID]
	p.mu.Unlock()
	if !ok {
		return false
	}
	pc.succeeded.Store(true)
	close(pc.done)
	return true
}

// ---------------------------------------------------------------------------
// Outbound consumer — keeps the orders read model honest
// ---------------------------------------------------------------------------

// Consumer drains each shard's out-ring: TradeFill events fold into
// filled_qty/avg_fill_price; OrderCancel events mark orders CANCELLED and
// resolve pending cancel confirmations (the engine emits the same event
// for user cancels and reject-driven removals — spec wire contract).
type Consumer struct {
	sub     Submitter
	store   Store
	pending *pendingConfirms
	bufSize int
	// onFill is the optional Phase-12 Task 12.3.5 order_filled
	// notification seam — invoked once per order id after each
	// TradeFill is applied to the read model. It runs inline on the
	// consumer goroutine; implementations must be cheap, non-blocking
	// and panic-safe (the hook swallows its own errors).
	onFill func(orderID int64, price, qty decimal.Decimal)
	// onGSLOFill is the Phase-16 Task 16.3.16 gap-absorption seam —
	// invoked per order id on every TradeFill; *algo.GSLOService.OnFill
	// detects gslo orders filled worse than their guaranteed stop and
	// posts the insurance-fund compensation journal. Same inline/
	// panic-safe contract as onFill.
	onGSLOFill func(orderID int64, price, qty decimal.Decimal)
	// onCancel is the Phase-16 composite-lifecycle seam — invoked once
	// per outbound OrderCancel echo after ApplyCancel lands. The
	// bracket/order-list/auction state machines fan out from it
	// (Service.OnCancel). Same inline/panic-safe contract as onFill.
	onCancel func(orderID int64, reason uint8)
	// pollInterval bounds the drain loop cadence; ~50µs production-tight,
	// larger in tests is fine.
	pollInterval time.Duration
	// malformed counts frames dropped by the decode panic-guard in
	// handle (Phase-13.5 pen-test remediation): a corrupt shm slot must
	// poison one frame, never the consumer goroutine.
	malformed atomic.Int64
	// tracer is the optional Task-9.3.11 continuation seam — when bound,
	// each frame that arrives carrying the C++-echoed EXCTRACE block
	// mints one CONSUMER span remote-parented on it. Nil → the block is
	// still stripped before decode; spans simply aren't emitted.
	tracer *tracing.Tracer
}

func NewConsumer(sub Submitter, store Store, pending *pendingConfirms) *Consumer {
	return &Consumer{
		sub: sub, store: store, pending: pending,
		bufSize: 64 << 10, pollInterval: 20 * time.Microsecond,
	}
}

// WithFillHook wires the optional per-fill observer (notification
// pipeline). Nil hook → zero overhead.
func (c *Consumer) WithFillHook(h func(orderID int64, price, qty decimal.Decimal)) *Consumer {
	c.onFill = h
	return c
}

// WithGSLOHook wires the Phase-16 Task 16.3.16 guaranteed-stop fill
// observer (insurance-fund gap absorption). Nil hook → zero overhead.
func (c *Consumer) WithGSLOHook(h func(orderID int64, price, qty decimal.Decimal)) *Consumer {
	c.onGSLOFill = h
	return c
}

// WithCancelHook wires the Phase-16 composite/auction cancel observer —
// fired once per engine OrderCancel echo (reason = the wire cancel
// code: 0 user, 1 expired, 7 OCO sibling, …). Nil hook → zero overhead.
func (c *Consumer) WithCancelHook(h func(orderID int64, reason uint8)) *Consumer {
	c.onCancel = h
	return c
}

// WithTracer binds the span exporter used for the engine->Go trace hop
// (Task 9.3.11). Nil keeps the no-span path.
func (c *Consumer) WithTracer(t *tracing.Tracer) *Consumer {
	c.tracer = t
	return c
}

// fireFill invokes the hooks under a panic guard — a misbehaving emitter
// must never kill the read-model consumer.
func (c *Consumer) fireFill(orderID int64, price, qty decimal.Decimal) {
	if c.onFill == nil && c.onGSLOFill == nil {
		return
	}
	defer func() { _ = recover() }()
	if c.onFill != nil {
		c.onFill(orderID, price, qty)
	}
	if c.onGSLOFill != nil {
		c.onGSLOFill(orderID, price, qty)
	}
}

// fireCancel invokes the cancel hook under the same panic guard —
// a misbehaving observer must never kill the read-model consumer.
func (c *Consumer) fireCancel(orderID int64, reason uint8) {
	if c.onCancel == nil {
		return
	}
	defer func() { _ = recover() }()
	c.onCancel(orderID, reason)
}

// Run polls the given shards' out-rings until ctx is cancelled.
func (c *Consumer) Run(ctx context.Context, shards []uint16) {
	type bound struct {
		shard uint16
		ch    *ipc.Channel
	}
	var chans []bound
	for _, sh := range shards {
		ch, err := c.sub.Channel(sh)
		if err != nil {
			continue // engine offline — fail closed is the submitter's job
		}
		chans = append(chans, bound{sh, ch})
	}
	buf := make([]byte, c.bufSize)
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		idle := true
		for _, b := range chans {
			for {
				n := b.ch.Poll(buf)
				if n <= 0 {
					break
				}
				idle = false
				c.handle(buf[:n])
			}
		}
		if idle {
			select {
			case <-ctx.Done():
				return
			case <-time.After(c.pollInterval):
			}
		}
	}
}

// Malformed returns the count of frames dropped by the decode
// panic-guard — the observability seam for corrupt-ring diagnostics.
func (c *Consumer) Malformed() int64 { return c.malformed.Load() }

// HandleFragment feeds one outbound-engine frame through the canonical
// decode + read-model apply + pending-resolve + hook pipeline — the
// narrow Phase-18 transport seam: the FIX gateway consumes
// aeron:ipc?alias=orders_out through its own subscription (the shm
// rings stay the REST gateway's binding) and must not fork this codec.
// Same inline/panic-guarded contract as the shm drain loop.
func (c *Consumer) HandleFragment(payload []byte) { c.handle(payload) }

func (c *Consumer) handle(payload []byte) {
	// Fail-closed decode guard (Phase-13.5 Task 13.5.3.9 pen-test):
	// ipc.DecodeEvent roots a FlatBuffers accessor without a verifier —
	// a corrupt or maliciously malformed shm frame panics on slice bounds
	// inside generated accessors. Every sibling decode site
	// (marketdata/events.go, settlement/balance_consumer.go,
	// bridge/bridge.go) already recovers; this consumer must too — one
	// poisoned frame must not kill the read-model drain loop.
	defer func() {
		if r := recover(); r != nil {
			c.malformed.Add(1)
		}
	}()
	// Task 9.3.11 — the engine echoes the 64B EXCTRACE block verbatim on
	// frames answering a traced command; strip it before decode and
	// continue the trace when a tracer is bound.
	body, tsc, traced := tracing.StripAeronTrace(payload)
	if len(body) < 8 { // uoffset + minimal table — not a valid Event
		c.malformed.Add(1)
		return
	}
	var span *tracing.Span
	if traced && c.tracer != nil {
		_, span = c.tracer.Start(
			tracing.ContextWithSpanContext(context.Background(), tsc),
			"orders.consume", tracing.KindConsumer)
		defer func() {
			if span != nil {
				span.Finish()
			}
		}()
	}
	ev := ipc.DecodeEvent(body)
	if ev == nil {
		return
	}
	switch ev.TypeType() {
	case wire.EventTypeOrderCancel:
		var t flatbuffers.Table
		if !ev.Type(&t) {
			return
		}
		oc := &wire.OrderCancel{}
		oc.Init(t.Bytes, t.Pos)
		_ = c.store.ApplyCancel(context.Background(), int64(oc.OrderId()))
		if oc.Reason() == CancelReasonOcoLink {
			// Phase-14 Task 14.3.1 — OCO sibling cancellation: audit the
			// engine-driven reason so the order's history distinguishes it
			// from a user cancel (spec §6.5 terminal notice).
			_ = c.store.WriteAudit(context.Background(), []AuditEntry{{
				OrderID:    int64(oc.OrderId()),
				AccountID:  int64(oc.AccountId()),
				Operation:  "OCO_SIBLING_CANCEL",
				FieldName:  "status",
				NewValue:   "CANCELLED",
				ModifiedBy: "engine",
			}})
		}
		c.pending.resolve(oc.OrderId())
		c.fireCancel(int64(oc.OrderId()), oc.Reason())
	case wire.EventTypeTradeFill:
		tf := ipc.EventTradeFill(ev)
		if tf == nil {
			return
		}
		qty := decimal.NewFromScaled(tf.Qty())
		px := decimal.NewFromScaled(tf.Price())
		_ = c.store.ApplyFill(context.Background(), int64(tf.BuyOrderId()), px, qty)
		_ = c.store.ApplyFill(context.Background(), int64(tf.SellOrderId()), px, qty)
		c.fireFill(int64(tf.BuyOrderId()), px, qty)
		c.fireFill(int64(tf.SellOrderId()), px, qty)
	}
}

// ---------------------------------------------------------------------------
// Batch rate limiter — rl:batch:{accountId}:{second} (Task 5.3.32)
// ---------------------------------------------------------------------------

// BatchRateLimiter enforces the spec §4 key
// `rl:batch:{accountId}:{second}` via INCR+EXPIRE — same primitive shape
// as the tiered limiter but a dedicated, batch-only window.
type BatchRateLimiter interface {
	// AllowBatch consumes one slot for the current second.
	AllowBatch(ctx context.Context, accountID int64) (bool, error)
}
