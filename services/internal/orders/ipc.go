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
	"exchange/pkg/decimal"
)

// ---------------------------------------------------------------------------
// Encoding — exc.wire Event envelopes
// ---------------------------------------------------------------------------

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

// orderNewMsg maps a validated submit onto wire.OrderNewMsg.
func orderNewMsg(o *Order, acct *Account) ipc.OrderNewMsg {
	m := ipc.OrderNewMsg{
		OrderID:       uint64(o.ID),
		AccountID:     uint64(o.AccountID),
		InstrumentID:  uint32(o.InstrumentID),
		ClientOrderID: o.ClientOrderID,
	}
	if o.Side == SideSell {
		m.Side = wire.SideSell
	} else {
		m.Side = wire.SideBuy
	}
	switch o.OrderType {
	case TypeMarket:
		m.Type = wire.OrderTypeMarket
	case TypeLimit:
		m.Type = wire.OrderTypeLimit
	case TypeStop:
		m.Type = wire.OrderTypeStopMarket
	case TypeStopLimit:
		m.Type = wire.OrderTypeStopLimit
	case TypeIceberg:
		m.Type = wire.OrderTypeIceberg
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

func (s *ShmSubmitter) Send(_ context.Context, shard uint16, payload []byte) error {
	c, err := s.Channel(shard)
	if err != nil {
		return err
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
	// pollInterval bounds the drain loop cadence; ~50µs production-tight,
	// larger in tests is fine.
	pollInterval time.Duration
}

func NewConsumer(sub Submitter, store Store, pending *pendingConfirms) *Consumer {
	return &Consumer{
		sub: sub, store: store, pending: pending,
		bufSize: 64 << 10, pollInterval: 20 * time.Microsecond,
	}
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

func (c *Consumer) handle(payload []byte) {
	ev := ipc.DecodeEvent(payload)
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
		c.pending.resolve(oc.OrderId())
	case wire.EventTypeTradeFill:
		tf := ipc.EventTradeFill(ev)
		if tf == nil {
			return
		}
		qty := decimal.NewFromScaled(tf.Qty())
		px := decimal.NewFromScaled(tf.Price())
		_ = c.store.ApplyFill(context.Background(), int64(tf.BuyOrderId()), px, qty)
		_ = c.store.ApplyFill(context.Background(), int64(tf.SellOrderId()), px, qty)
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
