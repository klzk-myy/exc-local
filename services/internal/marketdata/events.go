// Wave-2 shared event plumbing — engine trade events, source adapters,
// fan-out tee, and per-channel sequence allocation (Tasks 6.3.3/6.3.4/
// 6.3.11/6.3.12/6.3.13/6.3.20/6.3.23).
//
// The engine's outbound stream is decoded ONCE per transport — the
// shared-memory _out ring is SPSC (internal/ipc: exactly one consumer may
// attach), so producers can never attach a second reader. Two honest
// consumption paths exist:
//
//   - WireTradeSource (IPC ring / Aeron): decodes the full outbound
//     wire.Event stream. OrderNew echoes populate an order_id →
//     {symbol, admit_seq} index (same mechanism as internal/bridge's
//     orderIndex); a TradeFill resolves its instrument through either
//     order leg and resolves the TAKER as the leg with the LATER
//     admission seq — a resting maker is always admitted before the
//     taker that sweeps it, so admission order is the deterministic
//     aggressor signal the wire schema omits. Only usable when no other
//     consumer has claimed the ring (e.g. delta source mode "ipc"
//     already claimed it — see cmd/marketdata wiring notes).
//   - JetStreamTradeSource: consumes the Bridge's republished "trades"
//     stream (trades.{shard}.{symbol} — raw wire.Event payloads; the
//     symbol lives in the SUBJECT token, "EUR-USD" → "EUR/USD"). The
//     trades stream carries TradeFill only, so the taker/aggressor is
//     derivable only when an auxiliary order-index feed is attached
//     (WithOrderFeed over the compliance/analytics stream); otherwise
//     TakerSide stays "" — honest absence, never guessed.
//
// §27 candidates surfaced by this file: wire.TradeFill has no
// instrument_id and no taker/aggressor marker — both are engine-side
// additive FlatBuffers fields for a later wave (see package report).
package marketdata

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"exchange/internal/ipc"
	"exchange/internal/ipc/wire"
	excnats "exchange/internal/nats"
	"exchange/internal/tracing"
	"exchange/pkg/decimal"
)

// Side is the taker (aggressor) side convention shared with the ohlcv
// package ("BUY" | "SELL"); "" means the aggressor is not derivable from
// the wire event — never guessed (spec §2.7: no fabricated data).
type Side string

const (
	SideBuy  Side = "BUY"
	SideSell Side = "SELL"
)

// SideUnknown is the wire value emitted on trade frames when the
// aggressor cannot be resolved (TradeFill carries no taker marker and no
// order index is available on the transport).
const SideUnknown = "UNKNOWN"

// TradeEvent is the transport-neutral projection of wire.TradeFill for
// public tape consumers — field semantics mirror ohlcv.TradeEvent
// (Task 6.3.8) with the order lineage kept for aggTrades grouping.
//
// Seq is the engine book sequence (wire.TradeFill.seq == book_seq at
// publish): strictly monotonic per instrument, shared with the
// BookSnapshot sequence domain. Ts is the engine event timestamp.
type TradeEvent struct {
	TradeID      uint64
	Symbol       string          // canonical "EUR/USD"; "" = unresolved
	Price        decimal.Decimal // 1e-8 wire scale decoded
	Quantity     decimal.Decimal // base currency
	TakerSide    Side            // "" when not derivable
	TakerOrderID uint64          // aggressor order id; 0 when unknown
	MakerOrderID uint64          // resting order id; 0 when unknown
	BuyOrderID   uint64
	SellOrderID  uint64
	Seq          uint64 // engine per-instrument book seq
	Ts           time.Time
}

// TradeSource yields trade events until ctx is cancelled; the channel
// closes on termination. Mirrors DeltaSource semantics — a blocking
// producer applies natural backpressure and implementations own their
// drop policy (a drop is a client-visible seq gap downstream).
type TradeSource interface {
	Trades(ctx context.Context) (<-chan TradeEvent, error)
}

// TradeSourceFunc adapts a function to TradeSource (e.g. a FanOut tap).
type TradeSourceFunc func(ctx context.Context) (<-chan TradeEvent, error)

// Trades implements TradeSource.
func (f TradeSourceFunc) Trades(ctx context.Context) (<-chan TradeEvent, error) {
	return f(ctx)
}

// DeltaSourceFunc adapts a function to DeltaSource — e.g. wrapping a
// FanOut/TeeDeltaSource tap channel for producers typed on DeltaSource.
type DeltaSourceFunc func(ctx context.Context) (<-chan BookDelta, error)

// Deltas implements DeltaSource.
func (f DeltaSourceFunc) Deltas(ctx context.Context) (<-chan BookDelta, error) {
	return f(ctx)
}

// ---------------------------------------------------------------------------
// Order index — order_id → {symbol, admission seq}
// ---------------------------------------------------------------------------

// orderAdmission records one admitted order: its resolved symbol and the
// engine event seq of its OrderNew echo. Admission seq — not order id —
// decides the aggressor: PG BIGSERIAL order ids are allocation-ordered,
// not admission-ordered (a late-arriving lower id can still be the taker
// when the gateway serialized order creation ahead of IPC dispatch).
type orderAdmission struct {
	symbol   string
	admitSeq uint64
}

// orderIndex is the shared bounded order→admission index
// (internal/nats.OrderIndex); resolveFill adds the aggressor decision on
// top of it.
//
// resolveFill resolves a TradeFill's symbol and aggressor against the
// admission index. The taker is the leg with the LATER admission seq —
// a resting maker is always admitted before the taker that sweeps it.
// When exactly one leg is indexed it is the maker (it rested); when
// neither is indexed (producer attached mid-flight, index evicted) the
// fill is unresolvable — returned ok=false, never guessed.
func resolveFill(o *excnats.OrderIndex[orderAdmission], buyID, sellID uint64) (symbol string,
	takerID, makerID uint64, taker Side, ok bool) {
	b, bOK := o.Get(buyID)
	s, sOK := o.Get(sellID)
	switch {
	case bOK && sOK:
		symbol = b.symbol
		if b.admitSeq >= s.admitSeq {
			return symbol, buyID, sellID, SideBuy, true
		}
		return symbol, sellID, buyID, SideSell, true
	case bOK:
		// Buy leg rested (indexed earlier) → seller is the aggressor.
		return b.symbol, sellID, buyID, SideSell, true
	case sOK:
		return s.symbol, buyID, sellID, SideBuy, true
	}
	return "", 0, 0, "", false
}

// ---------------------------------------------------------------------------
// WireTradeSource — full-ring decoder (IPC / Aeron path)
// ---------------------------------------------------------------------------

// DefaultOrderIndexCap bounds the order_id → admission index.
const DefaultOrderIndexCap = 1 << 20

// WireTradeSource decodes TradeFill events from a raw wire.Event byte
// stream. OrderNew events on the SAME stream populate the admission
// index — this is why the source must attach before/with order flow
// (the engine echoes admitted orders on the outbound ring).
type WireTradeSource struct {
	src ByteSource
	res InstrumentResolver
	log *slog.Logger
	idx *excnats.OrderIndex[orderAdmission]

	// OnDrop observes skipped payloads ("malformed" | "unresolved").
	OnDrop func(reason string)
}

// NewWireTradeSource binds a byte source + instrument resolver. Only
// usable on a transport whose byte stream carries BOTH OrderNew echoes
// and TradeFill events — the IPC _out ring, or a JetStream stream that
// republishes order lifecycle events.
func NewWireTradeSource(src ByteSource, res InstrumentResolver, log *slog.Logger) *WireTradeSource {
	if log == nil {
		log = slog.Default()
	}
	return &WireTradeSource{src: src, res: res, log: log,
		idx: excnats.NewOrderIndex[orderAdmission](DefaultOrderIndexCap)}
}

// OrderIndex exposes the admission index so a JetStream trade consumer
// can share it with an auxiliary order-feed consumer.
func (s *WireTradeSource) OrderIndex() *excnats.OrderIndex[orderAdmission] { return s.idx }

// Trades implements TradeSource.
func (s *WireTradeSource) Trades(ctx context.Context) (<-chan TradeEvent, error) {
	raw, err := s.src(ctx)
	if err != nil {
		return nil, err
	}
	out := make(chan TradeEvent, 1024)
	go func() {
		defer close(out)
		for {
			select {
			case <-ctx.Done():
				return
			case b, ok := <-raw:
				if !ok {
					return
				}
				ev, keep := s.decode(b)
				if !keep {
					continue
				}
				select {
				case out <- ev:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return out, nil
}

// decode converts one wire.Event: OrderNew feeds the admission index
// (never emitted); TradeFill resolves into a TradeEvent. All other
// types return ok=false silently.
func (s *WireTradeSource) decode(buf []byte) (TradeEvent, bool) {
	defer func() {
		if r := recover(); r != nil {
			s.log.Error("marketdata: malformed wire payload", "panic", r)
			if s.OnDrop != nil {
				s.OnDrop("malformed")
			}
		}
	}()
	if len(buf) < 8 {
		if s.OnDrop != nil {
			s.OnDrop("malformed")
		}
		return TradeEvent{}, false
	}
	// Task 9.3.11 — engine echoes EXCTRACE on frames answering traced
	// commands; strip before decode (the block never reaches FlatBuffers).
	body, _, _ := tracing.StripAeronTrace(buf)
	ev := ipc.DecodeEvent(body)
	switch ev.TypeType() {
	case wire.EventTypeOrderNew:
		on := ipc.EventOrderNew(ev)
		if on == nil {
			return TradeEvent{}, false
		}
		sym, ok := s.res.Symbol(on.InstrumentId())
		if !ok {
			s.log.Warn("marketdata: unresolvable instrument",
				"instrument_id", on.InstrumentId())
			if s.OnDrop != nil {
				s.OnDrop("unresolved")
			}
			return TradeEvent{}, false
		}
		s.idx.Put(on.OrderId(), orderAdmission{symbol: sym, admitSeq: ev.Seq()})
		return TradeEvent{}, false
	case wire.EventTypeTradeFill:
		tf := ipc.EventTradeFill(ev)
		if tf == nil {
			if s.OnDrop != nil {
				s.OnDrop("malformed")
			}
			return TradeEvent{}, false
		}
		e := s.fillToEvent(tf, ev)
		if e.Symbol == "" {
			return TradeEvent{}, false // unresolvable — counted in fillToEvent
		}
		return e, true
	default:
		return TradeEvent{}, false
	}
}

// fillToEvent projects a decoded TradeFill into a TradeEvent, resolving
// symbol + aggressor through the admission index.
func (s *WireTradeSource) fillToEvent(tf *wire.TradeFill, ev *wire.Event) TradeEvent {
	e := TradeEvent{
		TradeID:     tf.TradeId(),
		BuyOrderID:  tf.BuyOrderId(),
		SellOrderID: tf.SellOrderId(),
		Price:       decimal.NewFromScaled(tf.Price()),
		Quantity:    decimal.NewFromScaled(tf.Qty()),
		Seq:         tf.Seq(),
		Ts:          time.Unix(0, int64(ev.Ts())),
	}
	sym, takerID, makerID, taker, ok := resolveFill(s.idx, tf.BuyOrderId(), tf.SellOrderId())
	if !ok {
		// Unresolvable — the producer drops it below (symbol-less events
		// cannot be routed to trades@{symbol}). Counted, never fatal.
		s.log.Warn("marketdata: trade fill with unresolvable orders",
			"trade_id", tf.TradeId(),
			"buy_order_id", tf.BuyOrderId(), "sell_order_id", tf.SellOrderId())
		if s.OnDrop != nil {
			s.OnDrop("unresolved")
		}
		return TradeEvent{Symbol: ""}
	}
	e.Symbol, e.TakerOrderID, e.MakerOrderID, e.TakerSide = sym, takerID, makerID, taker
	return e
}

// ---------------------------------------------------------------------------
// RawMsg transport + JetStream trade source
// ---------------------------------------------------------------------------

// RawMsg is a transport-level message: payload plus the routing subject
// when the transport carries one (JetStream "trades.{shard}.{symbol}").
// IPC bytes have no subject — Subject is "".
type RawMsg struct {
	Subject string
	Data    []byte
}

// MsgSource yields RawMsg until ctx is cancelled; closed on termination.
type MsgSource func(ctx context.Context) (<-chan RawMsg, error)

// JetStreamMsgSource adapts a durable pull consumer to MsgSource with
// the same ack discipline as JetStreamDeltaSource: messages are Ack'ed
// only after handoff; a saturated channel leaves them un-acked for
// AckWait redelivery.
func JetStreamMsgSource(nc *excnats.Client, stream, durable, filter string,
	log *slog.Logger) MsgSource {
	return func(ctx context.Context) (<-chan RawMsg, error) {
		opts := []excnats.ConsumerOption{}
		if filter != "" {
			opts = append(opts, excnats.WithFilterSubject(filter))
		}
		cons, err := nc.EnsureConsumer(ctx, stream, durable, opts...)
		if err != nil {
			return nil, fmt.Errorf("marketdata: jetstream consumer: %w", err)
		}
		out := make(chan RawMsg, 4096)
		go func() {
			defer close(out)
			for ctx.Err() == nil {
				msgs, err := nc.Fetch(ctx, cons, 256, time.Second)
				if err != nil {
					if ctx.Err() != nil {
						return
					}
					log.Warn("marketdata: jetstream fetch failed — retrying",
						"stream", stream, "err", err)
					select {
					case <-ctx.Done():
						return
					case <-time.After(500 * time.Millisecond):
					}
					continue
				}
				for _, m := range msgs {
					data := make([]byte, len(m.Data()))
					copy(data, m.Data())
					rm := RawMsg{Subject: m.Subject(), Data: data}
					select {
					case out <- rm:
						_ = m.Ack()
					case <-ctx.Done():
						_ = m.Nak()
						return
					default:
						_ = m.Nak()
					}
				}
			}
		}()
		return out, nil
	}
}

// SubjectSymbol maps a subject token back to the canonical display
// symbol ("EUR-USD" → "EUR/USD"). The token map is built from the same
// instrument map used for wire decode — a token absent from it returns
// ok=false (never guess a symbol for an unrouted event).
type SubjectSymbol map[string]string

// SubjectSymbols builds the token→canonical map from a resolver's known
// instruments. res may be a MapResolver (id→"EUR/USD"); each symbol's
// NATS token form is '/' → '-'.
func SubjectSymbols(res InstrumentResolver) SubjectSymbol {
	out := SubjectSymbol{}
	if mr, ok := res.(MapResolver); ok {
		for _, sym := range mr {
			out[excnats.SymbolToken(sym)] = sym
		}
	}
	return out
}

// subjectSymbol extracts the last subject token and maps it.
func (s SubjectSymbol) symbol(subject string) (string, bool) {
	i := strings.LastIndexByte(subject, '.')
	tok := subject
	if i >= 0 {
		tok = subject[i+1:]
	}
	if sym, ok := s[tok]; ok {
		return sym, true
	}
	// Degenerate fallback: exactly one '-' and no configured map entry —
	// "EUR-USD" is unambiguous even without the map.
	if strings.Count(tok, "-") == 1 {
		return strings.Replace(tok, "-", "/", 1), true
	}
	return "", false
}

// JetStreamTradeSource consumes the bridge's republished "trades"
// stream. The symbol is taken from the SUBJECT (payload has no
// instrument_id); the aggressor is resolved through an order index fed
// by an optional auxiliary order feed (compliance/analytics stream via
// WithOrderFeed). Without an order feed, TakerSide/TakerOrderID stay
// empty — never fabricated.
//
// Cross-stream caveat: the auxiliary order feed and the trades stream
// are independent JetStream orderings; a fill can be processed before
// its taker's OrderNew echo lands on the order feed, leaving that fill's
// aggressor unresolved. The IPC path (full-ring WireTradeSource) is the
// low-latency/high-fidelity attach point when colocated with the engine.
type JetStreamTradeSource struct {
	src     MsgSource
	symbols SubjectSymbol
	idx     *excnats.OrderIndex[orderAdmission]
	res     InstrumentResolver
	log     *slog.Logger

	orderFeed MsgSource // optional auxiliary OrderNew feed

	OnDrop func(reason string)
}

// NewJetStreamTradeSource binds the republished trades stream.
func NewJetStreamTradeSource(src MsgSource, symbols SubjectSymbol,
	res InstrumentResolver, log *slog.Logger) *JetStreamTradeSource {
	if log == nil {
		log = slog.Default()
	}
	return &JetStreamTradeSource{
		src: src, symbols: symbols, res: res, log: log,
		idx: excnats.NewOrderIndex[orderAdmission](DefaultOrderIndexCap),
	}
}

// WithOrderFeed attaches an auxiliary byte stream carrying OrderNew
// echoes (e.g. "analytics.{shard}.*") to populate the admission index —
// enabling taker resolution and acting as a redundant symbol source.
// May be nil (taker side then stays empty).
func (s *JetStreamTradeSource) WithOrderFeed(src MsgSource) {
	s.orderFeed = src
}

// Trades implements TradeSource.
func (s *JetStreamTradeSource) Trades(ctx context.Context) (<-chan TradeEvent, error) {
	raw, err := s.src(ctx)
	if err != nil {
		return nil, err
	}
	if s.orderFeed != nil {
		if err := s.pumpOrderFeed(ctx); err != nil {
			return nil, err
		}
	}
	out := make(chan TradeEvent, 1024)
	go func() {
		defer close(out)
		for {
			select {
			case <-ctx.Done():
				return
			case m, ok := <-raw:
				if !ok {
					return
				}
				ev, keep := s.decode(m)
				if !keep {
					continue
				}
				select {
				case out <- ev:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return out, nil
}

// pumpOrderFeed drains the auxiliary order stream into the admission
// index (OrderNew events only; other types skipped).
func (s *JetStreamTradeSource) pumpOrderFeed(ctx context.Context) error {
	raw, err := s.orderFeed(ctx)
	if err != nil {
		return fmt.Errorf("marketdata: order feed: %w", err)
	}
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case m, ok := <-raw:
				if !ok {
					return
				}
				s.indexOrder(m.Data)
			}
		}
	}()
	return nil
}

// indexOrder records one OrderNew from the auxiliary feed. The
// instrument_id → symbol resolution prefers the configured resolver;
// the symbol in the index is the canonical display form.
func (s *JetStreamTradeSource) indexOrder(buf []byte) {
	defer func() { _ = recover() }() // hostile input must not kill the pump
	if len(buf) < 8 {
		return
	}
	// Task 9.3.11 — strip the echoed EXCTRACE block before decode.
	body, _, _ := tracing.StripAeronTrace(buf)
	ev := ipc.DecodeEvent(body)
	if ev.TypeType() != wire.EventTypeOrderNew {
		return
	}
	on := ipc.EventOrderNew(ev)
	if on == nil {
		return
	}
	sym, ok := s.res.Symbol(on.InstrumentId())
	if !ok {
		sym = fmt.Sprintf("instr-%d", on.InstrumentId())
	}
	s.idx.Put(on.OrderId(), orderAdmission{symbol: sym, admitSeq: ev.Seq()})
}

// decode converts one republished wire.Event into a TradeEvent. Symbol
// comes from the subject; aggressor comes from the admission index.
func (s *JetStreamTradeSource) decode(m RawMsg) (TradeEvent, bool) {
	defer func() {
		if r := recover(); r != nil {
			s.log.Error("marketdata: malformed trades payload", "panic", r)
			if s.OnDrop != nil {
				s.OnDrop("malformed")
			}
		}
	}()
	if len(m.Data) < 8 {
		if s.OnDrop != nil {
			s.OnDrop("malformed")
		}
		return TradeEvent{}, false
	}
	// Task 9.3.11 — strip the echoed EXCTRACE block before decode, same
	// as indexOrder: a TradeFill answering a traced OrderNew carries the
	// 64B block and raw decode panics on the shifted frame.
	body, _, _ := tracing.StripAeronTrace(m.Data)
	ev := ipc.DecodeEvent(body)
	if ev.TypeType() != wire.EventTypeTradeFill {
		return TradeEvent{}, false
	}
	tf := ipc.EventTradeFill(ev)
	if tf == nil {
		if s.OnDrop != nil {
			s.OnDrop("malformed")
		}
		return TradeEvent{}, false
	}
	e := TradeEvent{
		TradeID:     tf.TradeId(),
		BuyOrderID:  tf.BuyOrderId(),
		SellOrderID: tf.SellOrderId(),
		Price:       decimal.NewFromScaled(tf.Price()),
		Quantity:    decimal.NewFromScaled(tf.Qty()),
		Seq:         tf.Seq(),
		Ts:          time.Unix(0, int64(ev.Ts())),
	}
	sym, ok := s.symbols.symbol(m.Subject)
	if !ok {
		// Subject routing failed — fall back to the admission index
		// before giving up (the index resolves via instrument map).
		if t, takerID, makerID, side, ok2 := resolveFill(s.idx, 
			tf.BuyOrderId(), tf.SellOrderId()); ok2 {
			e.Symbol, e.TakerOrderID, e.MakerOrderID, e.TakerSide =
				t, takerID, makerID, side
			return e, true
		}
		s.log.Warn("marketdata: trade on unrouted subject",
			"subject", m.Subject, "trade_id", tf.TradeId())
		if s.OnDrop != nil {
			s.OnDrop("unresolved")
		}
		return TradeEvent{}, false
	}
	e.Symbol = sym
	// Aggressor via admission index when available.
	if _, takerID, makerID, side, ok2 := resolveFill(s.idx, 
		tf.BuyOrderId(), tf.SellOrderId()); ok2 {
		e.TakerOrderID, e.MakerOrderID, e.TakerSide = takerID, makerID, side
	}
	return e, true
}

// ---------------------------------------------------------------------------
// IPC ring byte source (TradeFill path when the ring is ours)
// ---------------------------------------------------------------------------

// FanInTradeSource multiplexes several TradeSources into one stream —
// the multi-shard attach path. Per-symbol ordering holds because each
// instrument lives on exactly one shard (§5.2 router).
func FanInTradeSource(sources ...TradeSource) TradeSource {
	return TradeSourceFunc(func(ctx context.Context) (<-chan TradeEvent, error) {
		out := make(chan TradeEvent, 2048)
		var wg sync.WaitGroup
		for _, s := range sources {
			ch, err := s.Trades(ctx)
			if err != nil {
				return nil, err
			}
			wg.Add(1)
			go func(ch <-chan TradeEvent) {
				defer wg.Done()
				for {
					select {
					case <-ctx.Done():
						return
					case e, ok := <-ch:
						if !ok {
							return
						}
						select {
						case out <- e:
						case <-ctx.Done():
							return
						}
					}
				}
			}(ch)
		}
		go func() { wg.Wait(); close(out) }()
		return out, nil
	})
}

// IPCRingBytes adapts one shard's {base}_{shard}_out ring to a
// ByteSource — the same poll discipline as IPCDeltaSource, exported as
// raw bytes so callers can decode event types other than BookSnapshot.
// USAGE CONSTRAINT: the ring is SPSC — exactly one consumer may attach
// per process. Callers must ensure the ring is not already claimed
// (e.g. by IPCDeltaSource); cmd/marketdata enforces this by source mode.
func IPCRingBytes(ch *ipc.Channel) ByteSource {
	return func(ctx context.Context) (<-chan []byte, error) {
		out := make(chan []byte, 4096)
		go func() {
			defer close(out)
			buf := make([]byte, 64<<10)
			for {
				if ctx.Err() != nil {
					return
				}
				n := ch.Poll(buf)
				switch {
				case n > 0:
					cp := make([]byte, n)
					copy(cp, buf[:n])
					select {
					case out <- cp:
					case <-ctx.Done():
						return
					}
				case n < 0:
					buf = make([]byte, len(buf)*2)
				default:
					select {
					case <-ctx.Done():
						return
					case <-time.After(200 * time.Microsecond):
					}
				}
			}
		}()
		return out, nil
	}
}

// ---------------------------------------------------------------------------
// FanOut — one stream, many producers
// ---------------------------------------------------------------------------

// FanOut broadcasts one event stream to N subscribers. Every subscriber
// gets every event in order — the tee BLOCKS on a saturated subscriber
// rather than dropping (a dropped trade event silently corrupts
// aggTrades lineage and rolling stats; backpressure is the honest
// failure mode — ByteSource implementations own their own drop policy
// upstream). Subscribe before Run.
type FanOut[T any] struct {
	in   <-chan T
	mu   sync.Mutex
	subs []chan T
	done chan struct{}
	once sync.Once

	// OnStall observes a subscriber buffer that stayed full across a
	// grace period — a monitoring hook for consumer lag alerts.
	OnStall func(subscriber int)
}

// NewFanOut wraps an input channel. in is typically the channel returned
// by a Source; the FanOut itself owns the read.
func NewFanOut[T any](in <-chan T) *FanOut[T] {
	return &FanOut[T]{in: in, done: make(chan struct{})}
}

// Subscribe registers a buffered consumer channel. Must be called before
// Run; late subscribers only see events from their attach point.
func (f *FanOut[T]) Subscribe(buf int) <-chan T {
	if buf < 1 {
		buf = 1
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	ch := make(chan T, buf)
	f.subs = append(f.subs, ch)
	return ch
}

// Run distributes events until the input closes or ctx is cancelled.
// Subscriber channels are closed on termination.
func (f *FanOut[T]) Run(ctx context.Context) error {
	defer func() {
		f.mu.Lock()
		for _, ch := range f.subs {
			close(ch)
		}
		f.mu.Unlock()
	}()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case v, ok := <-f.in:
			if !ok {
				return nil
			}
			f.mu.Lock()
			subs := append([]chan T(nil), f.subs...)
			f.mu.Unlock()
			for i, ch := range subs {
				// Blocking send with ctx escape — a stalled subscriber
				// backpressures upstream, never a silent drop.
				for {
					select {
					case ch <- v:
						goto next
					case <-ctx.Done():
						return ctx.Err()
					case <-time.After(5 * time.Second):
						if f.OnStall != nil {
							f.OnStall(i)
						}
					}
				}
			next:
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Delta tee — share one BookDelta stream with a second consumer
// ---------------------------------------------------------------------------

// TeeDeltaSource wraps upstream so every emitted BookDelta is mirrored
// to each tap channel (BBO uses this to ride the same engine feed as
// the L2 conflator — the _out ring is SPSC so a second attach is
// impossible). A saturated tap BLOCKS the upstream pump — zero-drop is
// the §24 #261 zero-conflation contract; size taps generously.
func TeeDeltaSource(up DeltaSource, taps ...chan<- BookDelta) DeltaSource {
	return deltaTee{up: up, taps: taps}
}

type deltaTee struct {
	up   DeltaSource
	taps []chan<- BookDelta
}

func (t deltaTee) Deltas(ctx context.Context) (<-chan BookDelta, error) {
	in, err := t.up.Deltas(ctx)
	if err != nil {
		return nil, err
	}
	out := make(chan BookDelta, 1024)
	go func() {
		defer close(out)
		for {
			select {
			case <-ctx.Done():
				return
			case d, ok := <-in:
				if !ok {
					return
				}
				select {
				case out <- d:
				case <-ctx.Done():
					return
				}
				for _, tap := range t.taps {
					select {
					case tap <- d:
					case <-ctx.Done():
						return
					}
				}
			}
		}
	}()
	return out, nil
}

// ---------------------------------------------------------------------------
// Per-channel sequence allocation
// ---------------------------------------------------------------------------

// seqAllocator hands out strictly monotonic per-channel sequences for
// streams whose seq domain is producer-owned (ticker@/stats@/
// miniTicker@/liquidations@/blockTrades@/openInterest@ — unlike
// trades@/aggTrades@/bbo@, which carry the engine book seq through).
// In-memory: restart continuity for non-L2 channels rides the §10.7
// ring/snapshot contract (a fresh cursor is a resync, not silent data
// corruption).
type seqAllocator struct {
	mu sync.Mutex
	m  map[string]uint64
}

func newSeqAllocator() *seqAllocator { return &seqAllocator{m: map[string]uint64{}} }

// next returns the next sequence for a channel (starts at 1).
func (a *seqAllocator) next(channel string) uint64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.m[channel]++
	return a.m[channel]
}
