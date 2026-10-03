// Phase-17 Task 17.3.2 / 17.3.4 — L3 order-level data.
//
// Two surfaces share one hub:
//
//	L3Server  — dedicated authenticated WS endpoint /ws/v1/l3/{symbol}
//	            (premium tier, max 5 L3 subscriptions per account,
//	            last_seq resume from a per-symbol 100,000-event ring,
//	            no conflation: every ORDER_ADD/MODIFY/CANCEL/EXECUTE is
//	            forwarded).
//	L3Hub     — per-symbol distribution state: 100k replay ring,
//	            in-memory order-level book mirror (resync snapshot
//	            source), ordered non-blocking per-subscriber outboxes,
//	            feed-gap detection (L3_SEQUENCE_GAP_DETECTED) and
//	            slow-consumer eviction (L3_CONSUMER_OVERRUN).
//
// Spec anchors: §11 L3 stream, §11.1 resilience/slow-consumer policy,
// §10.5/§10.7 frame grammar + resume contract, §24 #197 (hidden orders
// are never exposed pre-execution), §24 #244/#318.
//
// Transport: the C++ engine publishes L3OrderEvent (wire union member 9)
// on the outbound Aeron/_out stream; the bridge republishes it on the
// JetStream "l3" stream as l3.{shard}.{symbol} (symbol lives in the
// subject token — the event itself carries no instrument_id). Until the
// generated wire type lands, ipc.DecodeL3OrderEvent is the single decode
// seam pinned to the published field contract.
package marketdata

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"math"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"

	"exchange/internal/auth"
	"exchange/internal/ipc"
	"exchange/internal/ipc/wire"
	"exchange/internal/middleware"
	excnats "exchange/internal/nats"
	"exchange/internal/ratelimit"
	"exchange/internal/tracing"
	"exchange/internal/ws"
	"exchange/pkg/decimal"
)

// Spec-pinned L3 constants (spec §11.1 — values are contractual, not
// tunables).
const (
	// L3ReplayBufferMsgs is the per-symbol replay ring retention:
	// the last 100,000 L3 events are replayable on reconnect.
	L3ReplayBufferMsgs = 100_000
	// L3MaxOutboxLag — a subscriber more than 5,000 messages behind is
	// disconnected with L3_CONSUMER_OVERRUN. The outbox holds exactly
	// this many frames; a failed (full) push means the next event puts
	// the client over the bound.
	L3MaxOutboxLag = 5_000
	// L3OutboundBuffer is the per-subscriber outbox depth.
	L3OutboundBuffer = L3MaxOutboxLag
	// L3SaturationTimeout — socket writes stalled longer than 1.5s are
	// L3_CONSUMER_OVERRUN evictions.
	L3SaturationTimeout = 1500 * time.Millisecond
	// L3MaxSubsPerAccount — maximum five simultaneous L3 subscriptions
	// per session/account (spec §24 #84 / Phase-17 task text).
	L3MaxSubsPerAccount = 5
	// L3AuthTimeout bounds the in-band authenticate window.
	L3AuthTimeout = 10 * time.Second
	// l3SnapshotMaxOrders mirrors the REST ceiling so an over-limit
	// book refuses snapshots consistently (L3_SNAPSHOT_TOO_LARGE is a
	// REST surface; the WS mirror degrades to resync directives).
	l3SnapshotMaxOrders = 100_000
)

// L3Kind is the order-lifecycle discriminator (mirrors the sibling's
// L3OrderKind encoding pinned in internal/ipc/l3.go).
type L3Kind uint8

const (
	L3Add     L3Kind = L3Kind(ipc.L3KindAdd)
	L3Modify  L3Kind = L3Kind(ipc.L3KindModify)
	L3Cancel  L3Kind = L3Kind(ipc.L3KindCancel)
	L3Execute L3Kind = L3Kind(ipc.L3KindExecute)
)

func (k L3Kind) String() string {
	switch k {
	case L3Add:
		return "ORDER_ADD"
	case L3Modify:
		return "ORDER_MODIFY"
	case L3Cancel:
		return "ORDER_CANCEL"
	case L3Execute:
		return "ORDER_EXECUTE"
	}
	return "UNKNOWN"
}

// L3Event is the transport-neutral projection of one L3OrderEvent —
// every order event, never conflated. Seq is the engine's per-symbol
// monotonic l3_seq; WalSeq preserves the exact WAL correspondence
// (spec §11.1: reconnect uses l3_seq; recovery compares wal_seq).
type L3Event struct {
	Symbol       string          // canonical "EUR/USD" (subject/instrument resolution)
	InstrumentID uint32          `json:"instrument_id,omitempty"`
	OrderID      uint64          `json:"order_id"`
	AccountHash  uint64          `json:"account_hash"`
	Kind         L3Kind          `json:"-"`
	Side         Side            `json:"side"` // "BUY"|"SELL"
	Price        decimal.Decimal `json:"price"`
	Quantity     decimal.Decimal `json:"qty"`       // order remaining AFTER the event (wire qty)
	QtyDelta     decimal.Decimal `json:"qty_delta"` // signed change applied by the event
	Ts           time.Time       `json:"-"`
	Seq          uint64          `json:"l3_seq"`
	WalSeq       uint64          `json:"wal_seq"`
	Hidden       bool            `json:"hidden"`
}

// l3EventPayload is the client-facing `data` projection of one event.
type l3EventPayload struct {
	OrderID     uint64 `json:"order_id"`
	AccountHash uint64 `json:"account_hash"`
	Event       string `json:"event"` // ORDER_ADD|ORDER_MODIFY|ORDER_CANCEL|ORDER_EXECUTE
	Side        string `json:"side"`
	Price       string `json:"price"`
	Qty         string `json:"qty"`       // remaining after the event
	QtyDelta    string `json:"qty_delta"` // signed change applied
	TsNs        uint64 `json:"ts"`
	WalSeq      uint64 `json:"wal_seq"`
	Hidden      bool   `json:"hidden"`
}

// payload projects the event for the wire frame. Hidden orders emit a
// REDACTED marker (§24 #197): the l3_seq chain stays complete so
// consumers never see a phantom gap, but side/price/qty/account_hash of
// a hidden resting order are never exposed before execution. EXECUTE on
// a hidden order publishes the full event — the print is public data.
func (e L3Event) payload() l3EventPayload {
	p := l3EventPayload{
		OrderID: e.OrderID, Event: e.Kind.String(),
		TsNs: uint64(e.Ts.UnixNano()), WalSeq: e.WalSeq, Hidden: e.Hidden,
	}
	if e.Hidden && e.Kind != L3Execute {
		p.Event = "ORDER_HIDDEN"
		return p
	}
	p.AccountHash = e.AccountHash
	p.Side = string(e.Side)
	p.Price = e.Price.String()
	p.Qty = e.Quantity.String()
	p.QtyDelta = e.QtyDelta.String()
	return p
}

// L3Source yields L3 events until ctx is cancelled; the channel closes
// on termination. Same contract family as TradeSource/DeltaSource.
type L3Source interface {
	Events(ctx context.Context) (<-chan L3Event, error)
}

// L3SourceFunc adapts a function to L3Source.
type L3SourceFunc func(ctx context.Context) (<-chan L3Event, error)

// Events implements L3Source.
func (f L3SourceFunc) Events(ctx context.Context) (<-chan L3Event, error) {
	return f(ctx)
}

// ---------------------------------------------------------------------------
// WireL3Source — full-ring decoder (IPC _out ring / republished payloads).
// ---------------------------------------------------------------------------

// WireL3Source decodes L3OrderEvent rows from a raw wire.Event byte
// stream. L3 rows carry instrument_id, so the instrument resolver is
// authoritative for the symbol; the order index fed by OrderNew echoes
// on the SAME stream remains as a fallback for rows whose instrument_id
// is absent/unknown (identical mechanics to WireTradeSource).
type WireL3Source struct {
	src ByteSource
	res InstrumentResolver
	idx *excnats.OrderIndex[orderAdmission]
	log *slog.Logger

	// OnDrop observes skipped payloads ("malformed" | "unresolved").
	OnDrop func(reason string)
}

// NewWireL3Source binds a byte source + instrument resolver. USAGE
// CONSTRAINT: the IPC _out ring is SPSC — attach only when no other
// consumer has claimed it (see cmd/marketdata source-mode wiring).
func NewWireL3Source(src ByteSource, res InstrumentResolver, log *slog.Logger) *WireL3Source {
	if log == nil {
		log = slog.Default()
	}
	return &WireL3Source{src: src, res: res, log: log,
		idx: excnats.NewOrderIndex[orderAdmission](DefaultOrderIndexCap)}
}

// OrderIndex exposes the admission index so a second consumer can share
// order_id → symbol resolution.
func (s *WireL3Source) OrderIndex() *excnats.OrderIndex[orderAdmission] { return s.idx }

// Events implements L3Source.
func (s *WireL3Source) Events(ctx context.Context) (<-chan L3Event, error) {
	raw, err := s.src(ctx)
	if err != nil {
		return nil, err
	}
	out := make(chan L3Event, 4096)
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

// decode converts one wire.Event: OrderNew feeds the symbol index
// (never emitted); L3OrderEvent resolves to an L3Event. Everything else
// is skipped silently.
func (s *WireL3Source) decode(buf []byte) (L3Event, bool) {
	defer func() {
		if r := recover(); r != nil {
			s.log.Error("marketdata: malformed L3 wire payload", "panic", r)
			if s.OnDrop != nil {
				s.OnDrop("malformed")
			}
		}
	}()
	if len(buf) < 8 {
		if s.OnDrop != nil {
			s.OnDrop("malformed")
		}
		return L3Event{}, false
	}
	body, _, _ := tracing.StripAeronTrace(buf)
	ev := ipc.DecodeEvent(body)
	switch ev.TypeType() {
	case wireTypeOrderNew:
		on := ipc.EventOrderNew(ev)
		if on == nil {
			return L3Event{}, false
		}
		sym, ok := s.res.Symbol(on.InstrumentId())
		if !ok {
			sym = "" // unresolved ids stay out of the index (never guess)
			if s.OnDrop != nil {
				s.OnDrop("unresolved")
			}
			return L3Event{}, false
		}
		s.idx.Put(on.OrderId(), orderAdmission{symbol: sym, admitSeq: ev.Seq()})
		return L3Event{}, false
	case ipc.EventTypeL3OrderEvent:
		f, ok := ipc.DecodeL3OrderEvent(ev)
		if !ok {
			if s.OnDrop != nil {
				s.OnDrop("malformed")
			}
			return L3Event{}, false
		}
		e := L3Event{
			InstrumentID: f.InstrumentID,
			OrderID:      f.OrderID,
			AccountHash:  f.AccountHash,
			Kind:         L3Kind(f.Kind),
			Price:        decimal.NewFromScaled(f.PriceTicks),
			Quantity:     decimal.NewFromScaled(f.QtyUnits),
			QtyDelta:     decimal.NewFromScaled(f.QtyDelta),
			Seq:          f.L3Seq,
			WalSeq:       f.WalSeq,
			Hidden:       f.Hidden,
			Ts:           time.Unix(0, int64(ev.Ts())),
		}
		if f.Side == byte(sideBuyWire) {
			e.Side = SideBuy
		} else {
			e.Side = SideSell
		}
		// instrument_id is the authoritative symbol route (the row
		// carries it); the OrderNew-fed index backstops rows with an
		// absent/unknown instrument id.
		if f.InstrumentID != 0 {
			if sym, ok := s.res.Symbol(f.InstrumentID); ok {
				e.Symbol = sym
			}
		}
		if e.Symbol == "" {
			if a, ok := s.idx.Get(f.OrderID); ok {
				e.Symbol = a.symbol
			}
		}
		if e.Symbol == "" {
			if s.OnDrop != nil {
				s.OnDrop("unresolved")
			}
			return L3Event{}, false // cannot route without symbol
		}
		return e, true
	default:
		return L3Event{}, false
	}
}

// ---------------------------------------------------------------------------
// JetStreamL3Source — republished "l3" stream consumer (gateway path).
// ---------------------------------------------------------------------------

// JetStreamL3Source consumes the bridge's republished "l3" stream
// (l3.{shard}.{symbol} subjects — raw wire.Event payloads). The symbol
// comes from the SUBJECT token (bridge routing is authoritative); the
// row's instrument_id is carried through for consumers.
type JetStreamL3Source struct {
	src     MsgSource
	symbols SubjectSymbol
	log     *slog.Logger

	// OnDrop observes skipped payloads ("malformed" | "unresolved").
	OnDrop func(reason string)
}

// NewJetStreamL3Source binds a message source on the "l3" stream.
func NewJetStreamL3Source(src MsgSource, symbols SubjectSymbol, log *slog.Logger) *JetStreamL3Source {
	if log == nil {
		log = slog.Default()
	}
	return &JetStreamL3Source{src: src, symbols: symbols, log: log}
}

// Events implements L3Source.
func (s *JetStreamL3Source) Events(ctx context.Context) (<-chan L3Event, error) {
	raw, err := s.src(ctx)
	if err != nil {
		return nil, err
	}
	out := make(chan L3Event, 4096)
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

func (s *JetStreamL3Source) decode(m RawMsg) (L3Event, bool) {
	defer func() {
		if r := recover(); r != nil {
			s.log.Error("marketdata: malformed l3 payload", "panic", r)
			if s.OnDrop != nil {
				s.OnDrop("malformed")
			}
		}
	}()
	if len(m.Data) < 8 {
		if s.OnDrop != nil {
			s.OnDrop("malformed")
		}
		return L3Event{}, false
	}
	body, _, _ := tracing.StripAeronTrace(m.Data)
	ev := ipc.DecodeEvent(body)
	f, ok := ipc.DecodeL3OrderEvent(ev)
	if !ok {
		if s.OnDrop != nil {
			s.OnDrop("malformed")
		}
		return L3Event{}, false
	}
	sym, ok := s.symbols.symbol(m.Subject)
	if !ok {
		s.log.Warn("marketdata: l3 event on unrouted subject",
			"subject", m.Subject, "order_id", f.OrderID)
		if s.OnDrop != nil {
			s.OnDrop("unresolved")
		}
		return L3Event{}, false
	}
	e := L3Event{
		Symbol:       sym,
		InstrumentID: f.InstrumentID,
		OrderID:      f.OrderID,
		AccountHash:  f.AccountHash,
		Kind:         L3Kind(f.Kind),
		Price:        decimal.NewFromScaled(f.PriceTicks),
		Quantity:     decimal.NewFromScaled(f.QtyUnits),
		QtyDelta:     decimal.NewFromScaled(f.QtyDelta),
		Seq:          f.L3Seq,
		WalSeq:       f.WalSeq,
		Hidden:       f.Hidden,
		Ts:           time.Unix(0, int64(ev.Ts())),
	}
	if f.Side == byte(sideBuyWire) {
		e.Side = SideBuy
	} else {
		e.Side = SideSell
	}
	return e, true
}

// wire-side enum aliases keep the switch/select sites readable — the
// discriminators themselves are the generated schema's.
const (
	wireTypeOrderNew = wire.EventTypeOrderNew
	sideBuyWire      = wire.SideBuy
)

// ---------------------------------------------------------------------------
// L3 order-level book mirror — resync snapshot state for the WS path.
// ---------------------------------------------------------------------------

// l3OrderState is one resting order in the mirror.
type l3OrderState struct {
	OrderID     uint64
	AccountHash uint64
	Side        Side
	Price       decimal.Decimal
	Quantity    decimal.Decimal // remaining
	Hidden      bool
}

// L3BookMirror maintains order-level book state from the event stream.
// It is a READ MODEL: events apply in l3_seq order; a feed gap marks the
// mirror suspect (snapshots then refuse — the WAL REST reader is the
// durable fallback).
type L3BookMirror struct {
	mu      sync.Mutex
	orders  map[uint64]*l3OrderState
	suspect bool // feed gap observed — snapshot refuses (§2.7 fail-closed)
}

// NewL3BookMirror builds an empty mirror.
func NewL3BookMirror() *L3BookMirror {
	return &L3BookMirror{orders: map[uint64]*l3OrderState{}}
}

// apply folds one event into the mirror. Hidden orders are tracked as
// markers (their lifecycle still mutates mirror membership) but never
// exposed by Snapshot.
func (m *L3BookMirror) apply(e L3Event) {
	m.mu.Lock()
	defer m.mu.Unlock()
	switch e.Kind {
	case L3Add:
		m.orders[e.OrderID] = &l3OrderState{
			OrderID: e.OrderID, AccountHash: e.AccountHash,
			Side: e.Side, Price: e.Price, Quantity: e.Quantity,
			Hidden: e.Hidden,
		}
	case L3Modify:
		if o, ok := m.orders[e.OrderID]; ok {
			o.Price = e.Price
			o.Quantity = e.Quantity
			o.Hidden = e.Hidden
		} else {
			// MODIFY for an unseen order = mirror has an implicit hole.
			m.suspect = true
		}
	case L3Cancel:
		delete(m.orders, e.OrderID)
	case L3Execute:
		if o, ok := m.orders[e.OrderID]; ok {
			// Wire qty = order remaining AFTER the fill (schema §11):
			// zero → the order is fully filled and leaves the book.
			if e.Quantity.IsPositive() {
				o.Quantity = e.Quantity
			} else {
				delete(m.orders, e.OrderID)
			}
		}
		// An EXECUTE for an unseen order means it rested before the
		// mirror attached — normal on attach, not a suspect marker.
	}
}

// markSuspect flags the mirror unreliable after a feed gap.
func (m *L3BookMirror) markSuspect() {
	m.mu.Lock()
	m.suspect = true
	m.mu.Unlock()
}

// L3RestingOrder is one order row in an L3 snapshot (WS mirror and the
// WAL REST reader share the shape).
type L3RestingOrder struct {
	OrderID     uint64 `json:"order_id"`
	AccountHash uint64 `json:"account_hash"`
	Side        string `json:"side"`
	Price       string `json:"price"`
	Qty         string `json:"qty"`
	Hidden      bool   `json:"hidden"`
}

// L3Snapshot is a complete point-in-time order-level book state.
type L3Snapshot struct {
	Symbol    string           `json:"symbol"`
	Seq       uint64           `json:"l3_seq"`  // l3_seq the state is consistent at (0 for WAL-only cuts)
	WalSeq    uint64           `json:"wal_seq"` // WAL position marker
	AsOfMs    int64            `json:"asof_ms"`
	Orders    []L3RestingOrder `json:"orders"`
	Count     int              `json:"count"`
	Fresh     bool             `json:"fresh"` // false when state is suspect/stale
	Truncated bool             `json:"truncated,omitempty"`
}

// Snapshot returns the mirror's resting orders (hidden orders excluded —
// §24 #197). Returns ok=false when the mirror is suspect after a feed
// gap (fail-closed: never serve a state known to have holes).
func (m *L3BookMirror) Snapshot() (orders []L3RestingOrder, ok bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.suspect {
		return nil, false
	}
	out := make([]L3RestingOrder, 0, len(m.orders))
	for _, o := range m.orders {
		if o.Hidden {
			continue
		}
		out = append(out, L3RestingOrder{
			OrderID: o.OrderID, AccountHash: o.AccountHash,
			Side: string(o.Side), Price: o.Price.String(),
			Qty: o.Quantity.String(),
		})
	}
	return out, true
}

// Size reports resting-order count (metrics/health).
func (m *L3BookMirror) Size() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.orders)
}

// ---------------------------------------------------------------------------
// L3Hub — per-symbol rings, mirrors and ordered subscriber fanout.
// ---------------------------------------------------------------------------

// l3SymbolState is the hub's per-symbol bookkeeping.
type l3SymbolState struct {
	ring    *ringBuffer // 100k L3 event frames (count-bound)
	mirror  *L3BookMirror
	subs    map[*l3Conn]struct{}
	lastSeq uint64
	feedGap bool // l3_seq discontinuity observed upstream
}

// L3HubConfig tunes the hub; zero values pick spec defaults.
type L3HubConfig struct {
	Logger  *slog.Logger
	Journal GapJournal
	Metrics *Metrics
	Now     func() time.Time

	ReplayBufferMsgs int           // default 100_000 (spec §11.1)
	OutboundBuffer   int           // default 5_000
	SaturationWindow time.Duration // default 1.5s
}

func (c *L3HubConfig) defaults() {
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	if c.ReplayBufferMsgs <= 0 {
		c.ReplayBufferMsgs = L3ReplayBufferMsgs
	}
	if c.OutboundBuffer <= 0 {
		c.OutboundBuffer = L3OutboundBuffer
	}
	if c.SaturationWindow <= 0 {
		c.SaturationWindow = L3SaturationTimeout
	}
	if c.Metrics == nil {
		c.Metrics = NewMetrics()
	}
}

// L3Hub owns per-symbol L3 distribution state: the 100k replay ring,
// the order mirror, feed-gap bookkeeping and the subscriber set. Every
// published event reaches every subscriber — no conflation (spec §11).
type L3Hub struct {
	cfg     L3HubConfig
	mu      sync.Mutex
	symbols map[string]*l3SymbolState

	overruns atomic.Int64 // L3_CONSUMER_OVERRUN evictions (observability)
	feedGaps atomic.Int64 // upstream l3_seq discontinuities
}

// NewL3Hub builds the distribution hub.
func NewL3Hub(cfg L3HubConfig) *L3Hub {
	cfg.defaults()
	return &L3Hub{cfg: cfg, symbols: map[string]*l3SymbolState{}}
}

// state returns (creating) the symbol state. Caller holds h.mu.
func (h *L3Hub) stateLocked(symbol string) *l3SymbolState {
	st := h.symbols[symbol]
	if st == nil {
		st = &l3SymbolState{
			// L3 retention is COUNT-bound only: the §11.1 contract is
			// "the last 100,000 events", no age horizon (unlike the L2
			// 10k/60s ring — L2 defaults are unchanged).
			ring:   newRingBuffer(h.cfg.ReplayBufferMsgs, math.MaxInt64, h.cfg.Now),
			mirror: NewL3BookMirror(),
			subs:   map[*l3Conn]struct{}{},
		}
		h.symbols[symbol] = st
	}
	return st
}

// Publish applies one L3 event: seq-continuity check, mirror apply,
// ring append, then ordered non-blocking fanout to the symbol's
// subscribers. Publish never blocks on a slow socket — a subscriber
// over the lag/saturation bound is evicted with L3_CONSUMER_OVERRUN
// (the upstream feed and matching loop are never backpressured).
func (h *L3Hub) Publish(ev L3Event) {
	if ev.Symbol == "" || ev.Seq == 0 {
		return // unroutable/unsequenced — dropped upstream is the rule
	}
	h.mu.Lock()
	st := h.stateLocked(ev.Symbol)
	// Feed-gap detection: a jump in l3_seq means the mirror (and any
	// resuming client's replay chain) is missing events. Mark the
	// mirror suspect and push a resync directive — the durable recovery
	// path is the WAL snapshot REST endpoint.
	if st.lastSeq != 0 && ev.Seq > st.lastSeq+1 {
		st.feedGap = true
		st.mirror.markSuspect()
		h.feedGaps.Add(1)
		noteGap(h.cfg.Journal, h.cfg.Logger, "l3@"+ev.Symbol, SeqGap{
			From: st.lastSeq + 1, To: ev.Seq - 1, Reason: GapResumeHorizon,
			AtMs: h.cfg.Now().UnixMilli(),
		})
		h.cfg.Logger.Warn("marketdata: l3 feed gap",
			"symbol", ev.Symbol, "from", st.lastSeq+1, "to", ev.Seq-1)
	}
	if ev.Seq > st.lastSeq {
		st.lastSeq = ev.Seq
	}
	st.mirror.apply(ev)

	b, err := marshalFrame(eventFrame{
		Type: "event", Channel: "l3@" + ev.Symbol, Seq: ev.Seq,
		Data: ev.payload(), TsMs: h.cfg.Now().UnixMilli(),
	})
	if err != nil {
		h.mu.Unlock()
		h.cfg.Logger.Error("marketdata: l3 marshal failed", "err", err)
		return
	}
	st.ring.append(ev.Seq, b)
	subs := make([]*l3Conn, 0, len(st.subs))
	for c := range st.subs {
		subs = append(subs, c)
	}
	var gapSubs []*l3Conn
	if st.feedGap {
		for _, c := range subs {
			if !c.gapNotified.Load() {
				gapSubs = append(gapSubs, c)
				c.gapNotified.Store(true)
			}
		}
	}
	h.mu.Unlock()

	for _, c := range gapSubs {
		c.notifyFeedGap(ev.Symbol)
	}
	for _, c := range subs {
		c.enqueue(b)
	}
	h.cfg.Metrics.FramesPublished.Add(1)
}

// ErrL3Suspect is returned by snapshot when the mirror is unreliable.
var ErrL3Suspect = errors.New("marketdata: l3 mirror suspect after feed gap")

// snapshot materializes the point-in-time mirror cut for one symbol.
func (h *L3Hub) snapshot(symbol string) (*L3Snapshot, error) {
	h.mu.Lock()
	st := h.symbols[symbol]
	if st == nil {
		h.mu.Unlock()
		return &L3Snapshot{Symbol: symbol, Fresh: true,
			AsOfMs: h.cfg.Now().UnixMilli()}, nil
	}
	seq := st.lastSeq
	mirror := st.mirror
	h.mu.Unlock()
	orders, ok := mirror.Snapshot()
	if !ok {
		return nil, ErrL3Suspect
	}
	if len(orders) > l3SnapshotMaxOrders {
		return &L3Snapshot{Symbol: symbol, Seq: seq, Fresh: true,
			Truncated: true, Count: len(orders),
			AsOfMs: h.cfg.Now().UnixMilli()}, nil
	}
	sortOrders(orders)
	return &L3Snapshot{Symbol: symbol, Seq: seq, Orders: orders,
		Count: len(orders), Fresh: true, AsOfMs: h.cfg.Now().UnixMilli()}, nil
}

func sortOrders(o []L3RestingOrder) {
	// order_id ascending — the stable cursor domain shared with the
	// WAL REST reader's pagination contract.
	for i := 1; i < len(o); i++ {
		for j := i; j > 0 && o[j].OrderID < o[j-1].OrderID; j-- {
			o[j], o[j-1] = o[j-1], o[j]
		}
	}
}

// attach binds a conn to the symbol's subscriber set and returns the
// replay verdict for its cursor — computed under the hub lock so the
// verdict is a consistent cut vs concurrent Publish.
func (h *L3Hub) attach(c *l3Conn, symbol string, lastSeq uint64) ReplayResult {
	h.mu.Lock()
	defer h.mu.Unlock()
	st := h.stateLocked(symbol)
	st.subs[c] = struct{}{}
	return st.ring.replay(lastSeq)
}

// detach removes the conn from its symbol set (empty symbol states are
// GC'd only when their ring is also empty).
func (h *L3Hub) detach(c *l3Conn, symbol string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if st := h.symbols[symbol]; st != nil {
		delete(st.subs, c)
		if len(st.subs) == 0 && st.ring.len() == 0 {
			delete(h.symbols, symbol)
		}
	}
}

// tailSeq returns the symbol's newest buffered l3_seq (0 = empty).
func (h *L3Hub) tailSeq(symbol string) uint64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	if st := h.symbols[symbol]; st != nil {
		return st.ring.tailSeq()
	}
	return 0
}

// Stats is the /health observability cut.
func (h *L3Hub) Stats() (symbols, subs int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, st := range h.symbols {
		symbols++
		subs += len(st.subs)
	}
	return symbols, subs
}

// Overruns reports cumulative L3_CONSUMER_OVERRUN evictions.
func (h *L3Hub) Overruns() int64 { return h.overruns.Load() }

// FeedGaps reports cumulative upstream l3_seq discontinuities.
func (h *L3Hub) FeedGaps() int64 { return h.feedGaps.Load() }

// Run pumps one L3Source into the hub until ctx is cancelled or the
// source closes. Feed sources call this; it never blocks a subscriber.
func (h *L3Hub) Run(ctx context.Context, src L3Source) error {
	ch, err := src.Events(ctx)
	if err != nil {
		return err
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case ev, ok := <-ch:
			if !ok {
				return nil
			}
			h.Publish(ev)
		}
	}
}

// ---------------------------------------------------------------------------
// L3Server — dedicated /ws/v1/l3/{symbol} endpoint.
// ---------------------------------------------------------------------------

// L3ServerConfig tunes the endpoint; nil verifier/issuer fail closed.
type L3ServerConfig struct {
	Hub *L3Hub // shared distribution state (required)

	// Auth seams — identical contract to marketdata.Server:
	//   {"action":"authenticate","token":"<jwt|ak_*>","signature":..,
	//    "timestamp":..,"protocol_version":1}
	Issuer   *auth.Issuer
	Verifier *auth.SignatureVerifier
	// TierResolver resolves the session tier (same seam as ws.Config).
	// Nil ⇒ session-carried tier label.
	TierResolver func(ctx context.Context, sess *ws.Session) ratelimit.Tier

	Logger     *slog.Logger
	TrustProxy bool
	Now        func() time.Time

	MaxConnsPerIP     int           // default 256
	MaxSubsPerAccount int           // default 5 (spec-pinned)
	AuthTimeout       time.Duration // default 10s
	PingInterval      time.Duration // default 30s
	PongWait          time.Duration // default 60s
	MaxFrameBytes     int64         // default 64KiB
	OutboundBuffer    int           // default 5_000
	SaturationWindow  time.Duration // default 1.5s
}

func (c *L3ServerConfig) defaults() {
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	if c.MaxConnsPerIP <= 0 {
		c.MaxConnsPerIP = 256
	}
	if c.MaxSubsPerAccount <= 0 {
		c.MaxSubsPerAccount = L3MaxSubsPerAccount
	}
	if c.AuthTimeout <= 0 {
		c.AuthTimeout = L3AuthTimeout
	}
	if c.PingInterval <= 0 {
		c.PingInterval = 30 * time.Second
	}
	if c.PongWait <= 0 {
		c.PongWait = 60 * time.Second
	}
	if c.MaxFrameBytes <= 0 {
		c.MaxFrameBytes = 64 << 10
	}
	if c.OutboundBuffer <= 0 {
		c.OutboundBuffer = L3OutboundBuffer
	}
	if c.SaturationWindow <= 0 {
		c.SaturationWindow = L3SaturationTimeout
	}
}

// L3Server is the dedicated authenticated L3 endpoint. Unlike the
// generic marketdata server, the subscription is IMPLIED BY THE PATH —
// the conn binds l3@{symbol} after authenticate; last_seq (query param
// or resume frame) drives the replay verdict.
type L3Server struct {
	cfg L3ServerConfig
	up  websocket.Upgrader

	mu        sync.Mutex
	conns     map[*l3Conn]struct{}
	byIP      map[string]int
	byAccount map[int64]int

	draining atomic.Bool
}

// NewL3Server builds the endpoint around a shared hub. A nil Hub fails
// safe: the endpoint mounts but streams nothing (an empty hub still
// answers replay/snapshot control frames honestly).
func NewL3Server(cfg L3ServerConfig) *L3Server {
	cfg.defaults()
	if cfg.Hub == nil {
		cfg.Hub = NewL3Hub(L3HubConfig{Logger: cfg.Logger, Now: cfg.Now})
	}
	return &L3Server{
		cfg: cfg,
		up: websocket.Upgrader{
			ReadBufferSize:  16 << 10,
			WriteBufferSize: 16 << 10,
			CheckOrigin:     func(*http.Request) bool { return true },
		},
		conns:     map[*l3Conn]struct{}{},
		byIP:      map[string]int{},
		byAccount: map[int64]int{},
	}
}

// Draining reports drain state (readiness surface).
func (s *L3Server) Draining() bool { return s.draining.Load() }

// Drain latches the drain flag; live conns close 1001.
func (s *L3Server) Drain() {
	s.draining.Store(true)
	s.mu.Lock()
	conns := make([]*l3Conn, 0, len(s.conns))
	for c := range s.conns {
		conns = append(conns, c)
	}
	s.mu.Unlock()
	for _, c := range conns {
		c.closeConn(websocket.CloseGoingAway, "server draining")
	}
}

// ServeHTTP upgrades GET /ws/v1/l3/{symbol} to a WebSocket.
func (s *L3Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if s.draining.Load() {
		writeL3Problem(w, http.StatusServiceUnavailable,
			"MAINTENANCE_MODE", "service draining for shutdown")
		return
	}
	symbol := r.PathValue("symbol")
	if symbol == "" {
		// PathValue empty when mounted outside the route pattern —
		// accept the last path segment as a fallback token.
		symbol = strings.TrimPrefix(r.URL.Path, "/ws/v1/l3/")
	}
	if symbol == "" || strings.Contains(symbol, "/") || len(symbol) > 32 {
		writeL3Problem(w, http.StatusBadRequest,
			"INVALID_REQUEST", "symbol path parameter required")
		return
	}
	ip := middleware.ClientIP(r, s.cfg.TrustProxy)

	s.mu.Lock()
	if s.byIP[ip] >= s.cfg.MaxConnsPerIP {
		s.mu.Unlock()
		writeL3Problem(w, http.StatusServiceUnavailable,
			"CAPACITY_EXCEEDED", "per-IP WebSocket connection cap reached")
		return
	}
	s.mu.Unlock()

	sock, err := s.up.Upgrade(w, r, nil)
	if err != nil {
		return // upgrader already answered
	}
	c := &l3Conn{
		srv: s, ws: sock, remoteIP: ip, symbol: symbol,
		out:      make(chan []byte, s.cfg.OutboundBuffer),
		done:     make(chan struct{}),
		pumpDone: make(chan struct{}),
	}
	s.mu.Lock()
	if s.byIP[ip] >= s.cfg.MaxConnsPerIP {
		s.mu.Unlock()
		_ = sock.WriteMessage(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseTryAgainLater,
				"per-IP connection cap"))
		_ = sock.Close()
		return
	}
	s.conns[c] = struct{}{}
	s.byIP[ip]++
	s.mu.Unlock()

	go c.writePump()
	c.readLoop(r)
	s.unregister(c)
}

func writeL3Problem(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"type": "error", "error": code, "message": msg, "status": status,
	})
}

// unregister detaches the conn from the hub and indexes.
func (s *L3Server) unregister(c *l3Conn) {
	c.closeConn(websocket.CloseNormalClosure, "bye")
	select {
	case <-c.pumpDone:
	case <-time.After(15 * time.Second):
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.conns, c)
	if s.byIP[c.remoteIP]--; s.byIP[c.remoteIP] <= 0 {
		delete(s.byIP, c.remoteIP)
	}
	if sess := c.session(); sess.Authenticated && sess.AccountID != 0 {
		if s.byAccount[sess.AccountID]--; s.byAccount[sess.AccountID] <= 0 {
			delete(s.byAccount, sess.AccountID)
		}
	}
	if c.attached {
		s.cfg.Hub.detach(c, c.symbol)
		c.attached = false
	}
}

// accountCapAdmit reserves one of the five per-account L3 slots.
func (s *L3Server) accountCapAdmit(c *l3Conn, sess *ws.Session) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.byAccount[sess.AccountID] >= s.cfg.MaxSubsPerAccount {
		return false
	}
	s.byAccount[sess.AccountID]++
	return true
}

// ---------------------------------------------------------------------------
// l3Conn — one upgraded L3 socket.
// ---------------------------------------------------------------------------

// l3Conn mirrors Conn's discipline (single writer pump, non-blocking
// enqueue, saturation bookkeeping) with the L3-specific thresholds:
// >5,000 pending ⇒ L3_CONSUMER_OVERRUN; a socket write blocked longer
// than the 1.5s saturation window ⇒ L3_CONSUMER_OVERRUN.
type l3Conn struct {
	srv      *L3Server
	ws       *websocket.Conn
	remoteIP string
	symbol   string

	out       chan []byte
	done      chan struct{}
	pumpDone  chan struct{}
	closeOne  sync.Once
	closeCode atomic.Int32
	closeWhy  atomic.Value // string

	sessMu sync.Mutex
	sess   ws.Session

	attached    bool        // bound to hub subscriber set
	gapNotified atomic.Bool // feed-gap resync already pushed this conn
	satSince    atomic.Int64

	// Replay ordering gate: while replaying is set, fanout enqueues
	// divert to `pending` so live frames can never overtake the
	// replayed range (a 100k replay is far longer than the generic
	// path's, so the subscribe→replay race is closed structurally,
	// not by luck). endReplay flushes pending in publish order.
	replayMu  sync.Mutex
	replaying bool
	pending   [][]byte
}

func (c *l3Conn) session() ws.Session {
	c.sessMu.Lock()
	defer c.sessMu.Unlock()
	return c.sess
}

func (c *l3Conn) setSession(s ws.Session) {
	c.sessMu.Lock()
	c.sess = s
	c.sessMu.Unlock()
}

// enqueue offers a frame to the write pump — non-blocking. During a
// replay window the frame defers to `pending` (ordering gate above).
func (c *l3Conn) enqueue(b []byte) {
	c.replayMu.Lock()
	if c.replaying {
		if len(c.pending) >= c.srv.cfg.OutboundBuffer {
			// Pending overflow is the same overrun bound — >5,000
			// undelivered frames ⇒ L3_CONSUMER_OVERRUN.
			c.replayMu.Unlock()
			c.overrun("replay_pending_lag")
			return
		}
		c.pending = append(c.pending, b)
		c.replayMu.Unlock()
		return
	}
	c.replayMu.Unlock()
	c.push(b)
}

// beginReplay diverts fanout enqueues into `pending` until endReplay.
func (c *l3Conn) beginReplay() {
	c.replayMu.Lock()
	c.replaying = true
	c.replayMu.Unlock()
}

// endReplay flushes deferred fanout frames (in publish order) after the
// replayed range — the client sees an unbroken seq chain.
func (c *l3Conn) endReplay() {
	c.replayMu.Lock()
	pend := c.pending
	c.pending = nil
	c.replaying = false
	c.replayMu.Unlock()
	for _, b := range pend {
		c.push(b)
	}
}

// push is the raw non-blocking offer to the outbox. With the outbox at
// exactly L3MaxOutboxLag frames, a failed push means the subscriber is
// now >5,000 messages behind ⇒ L3_CONSUMER_OVERRUN (spec §11.1 — never
// backpressure the feed).
func (c *l3Conn) push(b []byte) {
	select {
	case c.out <- b:
		c.satSince.Store(0)
		return
	case <-c.done:
		return
	default:
	}
	// Buffer full ⇒ ≥5,000 already queued; the dropped frame is #5001.
	c.overrun("outbox_lag")
}

// overrun emits the terminal L3_CONSUMER_OVERRUN error (best-effort —
// a saturated client may only see the close-frame reason) and closes.
func (c *l3Conn) overrun(why string) {
	c.srv.cfg.Hub.overruns.Add(1)
	c.srv.cfg.Logger.Warn("marketdata: l3 consumer overrun",
		"symbol", c.symbol, "account", c.session().AccountID,
		"queued", c.srv.cfg.OutboundBuffer, "cause", why)
	c.sendErrorNow("", "", "L3_CONSUMER_OVERRUN",
		"L3 subscriber evicted: outbound buffer saturation (>5000 messages behind or socket stalled >1.5s)", 0)
	c.closeConn(CloseSlowConsumer, "L3_CONSUMER_OVERRUN")
}

// sendErrorNow appends a terminal error frame directly to the outbox —
// used on the overrun path where enqueue would itself evict. Best-effort:
// a saturated client may not see it before the close frame.
func (c *l3Conn) sendErrorNow(requestID, action, code, message string, retryAfterMs int64) {
	b, err := marshalFrame(errorFrame{
		Type: "error", RequestID: requestID, Action: action,
		Error: code, Message: message,
		TsMs: c.srv.cfg.Now().UnixMilli(), RetryAfterMs: retryAfterMs,
	})
	if err != nil {
		return
	}
	select {
	case c.out <- b:
	default: // saturated — the close frame still reports the reason
	}
}

// sendError emits a canonical WS error frame through the normal path.
func (c *l3Conn) sendError(requestID, action, code, message string, retryAfterMs int64) {
	b, err := marshalFrame(errorFrame{
		Type: "error", RequestID: requestID, Action: action,
		Error: code, Message: message,
		TsMs: c.srv.cfg.Now().UnixMilli(), RetryAfterMs: retryAfterMs,
	})
	if err != nil {
		return
	}
	c.enqueue(b)
}

// sendResponse emits the ACK/NACK response frame (§10.5 item 6).
func (c *l3Conn) sendResponse(requestID, action, status string, data any) {
	if status == "" {
		status = "ACK"
	}
	b, err := marshalFrame(responseFrame{
		Type: "response", RequestID: requestID, Action: action,
		Status: status, Data: data, TsMs: c.srv.cfg.Now().UnixMilli(),
	})
	if err != nil {
		c.sendError(requestID, action, "INTERNAL_ERROR", "response marshal failure", 0)
		return
	}
	c.enqueue(b)
}

// notifyFeedGap pushes the gap-detection pair to a live subscriber when
// the upstream feed lost events: an L3_SEQUENCE_GAP_DETECTED error frame
// (spec §23 registered code) followed by a resync directive — the client
// refetches state via the WAL snapshot REST endpoint.
func (c *l3Conn) notifyFeedGap(symbol string) {
	now := c.srv.cfg.Now().UnixMilli()
	b, _ := marshalFrame(errorFrame{
		Type: "error", Error: "L3_SEQUENCE_GAP_DETECTED",
		Message: "L3 stream sequence gap detected upstream; resync required",
		TsMs:    now,
	})
	c.enqueue(b)
	b, _ = marshalFrame(resyncFrame{
		Type: "resync", Channel: "l3@" + symbol, Reason: "feed_gap",
		TsMs: now,
	})
	c.enqueue(b)
}

// closeConn initiates teardown; first close code wins.
func (c *l3Conn) closeConn(code int, reason string) {
	c.closeOne.Do(func() {
		c.closeCode.Store(int32(code))
		c.closeWhy.Store(reason)
		close(c.done)
	})
}

// writePump is the sole writer. Per-write deadline = saturation window
// (1.5s): a socket that cannot take a frame inside the window is an
// L3_CONSUMER_OVERRUN eviction — the write side of the spec's second
// eviction clause.
func (c *l3Conn) writePump() {
	ping := time.NewTicker(c.srv.cfg.PingInterval)
	defer ping.Stop()
	defer close(c.pumpDone)
	defer c.ws.Close()
	for {
		select {
		case b := <-c.out:
			_ = c.ws.SetWriteDeadline(
				c.srv.cfg.Now().Add(c.srv.cfg.SaturationWindow))
			if err := c.ws.WriteMessage(websocket.TextMessage, b); err != nil {
				c.writeStall(err)
				return
			}
		case <-ping.C:
			_ = c.ws.SetWriteDeadline(
				c.srv.cfg.Now().Add(c.srv.cfg.SaturationWindow))
			if err := c.ws.WriteMessage(websocket.PingMessage, nil); err != nil {
				c.writeStall(err)
				return
			}
		case <-c.done:
			c.flushOut()
			code := int(c.closeCode.Load())
			why, _ := c.closeWhy.Load().(string)
			_ = c.ws.SetWriteDeadline(c.srv.cfg.Now().Add(time.Second))
			_ = c.ws.WriteMessage(websocket.CloseMessage,
				websocket.FormatCloseMessage(code, why))
			return
		}
	}
}

// writeStall accounts the socket-saturation half of L3_CONSUMER_OVERRUN:
// a write that could not complete inside the 1.5s window is the spec's
// "socket saturation" eviction (§11.1). Only a DEADLINE error counts —
// a plain TCP reset is a normal disconnect, not a consumer overrun.
func (c *l3Conn) writeStall(err error) {
	var nerr net.Error
	if errors.As(err, &nerr) && nerr.Timeout() {
		c.srv.cfg.Hub.overruns.Add(1)
		c.srv.cfg.Logger.Warn("marketdata: l3 consumer overrun",
			"symbol", c.symbol, "account", c.session().AccountID,
			"cause", "socket_saturation", "err", err)
		c.closeConn(CloseSlowConsumer, "L3_CONSUMER_OVERRUN")
	}
}

// flushOut drains queued frames best-effort on teardown.
func (c *l3Conn) flushOut() {
	for {
		select {
		case b := <-c.out:
			_ = c.ws.SetWriteDeadline(c.srv.cfg.Now().Add(time.Second))
			if err := c.ws.WriteMessage(websocket.TextMessage, b); err != nil {
				return
			}
		default:
			return
		}
	}
}

// readLoop pumps inbound frames: authenticate first (10s window),
// then the small L3 control set — ping/resume — everything else is a
// spec-grammar error. The conn binds l3@{symbol} on auth success.
func (c *l3Conn) readLoop(r *http.Request) {
	c.ws.SetReadLimit(c.srv.cfg.MaxFrameBytes)
	_ = c.ws.SetReadDeadline(c.srv.cfg.Now().Add(c.srv.cfg.AuthTimeout))
	c.ws.SetPongHandler(func(string) error {
		return c.ws.SetReadDeadline(c.srv.cfg.Now().Add(c.srv.cfg.PongWait))
	})
	// last_seq arrives on the connect URL: ?last_seq=N is the reconnect
	// contract — replay strictly after the cursor once authenticated.
	var wantSeq uint64
	var haveSeq bool
	if raw := r.URL.Query().Get("last_seq"); raw != "" {
		if n, err := strconv.ParseUint(raw, 10, 64); err == nil {
			wantSeq, haveSeq = n, true
		}
	}

	for {
		mt, msg, err := c.ws.ReadMessage()
		if err != nil {
			return
		}
		if mt != websocket.TextMessage && mt != websocket.BinaryMessage {
			continue
		}
		f, perr := parseFrame(msg)
		if perr != nil {
			c.sendError("", "", "INVALID_REQUEST", "frame is not valid JSON", 0)
			continue
		}
		sess := c.session()
		if !sess.Authenticated {
			if f.Action != "authenticate" {
				c.sendError(f.rid(), f.Action, "UNAUTHORIZED",
					"authenticate first — the L3 surface is premium-tier only", 0)
				continue
			}
			if !c.handleAuthenticate(f) {
				return // rejected or socket-level close already queued
			}
			// Bound: arm the pong deadline now that auth passed.
			_ = c.ws.SetReadDeadline(c.srv.cfg.Now().Add(c.srv.cfg.PongWait))
			c.bindAndResume(wantSeq, haveSeq)
			continue
		}
		switch f.Action {
		case "ping":
			b, _ := marshalFrame(pongFrame{Type: "pong",
				TsMs: c.srv.cfg.Now().UnixMilli()})
			c.enqueue(b)
		case "resume":
			c.bindAndResume(f.LastSeq, true)
		case "refresh_token":
			c.sendError(f.rid(), f.Action, "INVALID_REQUEST",
				"reconnect to re-authenticate on the L3 endpoint", 0)
		default:
			c.sendError(f.rid(), f.Action, "INVALID_REQUEST",
				"unsupported action on the L3 endpoint (allowed: ping, resume)", 0)
		}
	}
}

// l3PremiumTier is the L3 entitlement gate: professional, institutional
// and admin sessions qualify; basic/free/public tiers are rejected with
// ENTITLEMENT_REQUIRED (spec §24 premium-surface rule).
func l3PremiumTier(t ratelimit.Tier) bool {
	switch t {
	case ratelimit.TierProfessional, ratelimit.TierInstitutional,
		ratelimit.TierAdmin:
		return true
	}
	return false
}

// handleAuthenticate elevates the conn — same credential contract as the
// §10.5 surfaces (JWT or ak_+HMAC), plus the L3 gates: authenticated,
// premium tier (≥ professional), and the 5-subscription account budget.
func (c *l3Conn) handleAuthenticate(f clientFrame) bool {
	if f.ProtocolVersion == nil {
		c.sendError(f.rid(), "authenticate", "INVALID_REQUEST",
			"protocol_version is required", 0)
		return true
	}
	if err := middleware.CheckWSProtocolVersion(*f.ProtocolVersion); err != nil {
		c.sendError(f.rid(), "authenticate", "UNSUPPORTED_PROTOCOL_VERSION",
			"protocol_version not supported", 0)
		return false
	}
	if f.Token == "" {
		c.sendError(f.rid(), "authenticate", "INVALID_REQUEST",
			"token is required", 0)
		return true
	}

	var sess ws.Session
	sess.ProtocolVer = *f.ProtocolVersion
	if strings.HasPrefix(f.Token, "ak_") {
		if c.srv.cfg.Verifier == nil {
			c.sendError(f.rid(), "authenticate", "UNAUTHORIZED",
				"api-key authentication not configured", 0)
			return true
		}
		if f.Signature == "" || len(f.Timestamp) == 0 {
			c.sendError(f.rid(), "authenticate", "INVALID_REQUEST",
				"api-key auth requires signature and timestamp", 0)
			return true
		}
		key, err := c.srv.cfg.Verifier.Verify(context.Background(), auth.SignedRequest{
			KeyID:     f.Token,
			Timestamp: f.tsString(),
			Signature: f.Signature,
			Method:    "WS",
			Path:      "/ws/v1/l3/" + c.symbol,
			Body:      []byte(f.Token),
			RemoteIP:  c.remoteIP,
		})
		if err != nil {
			c.sendError(f.rid(), "authenticate", codeOf(err),
				"api-key authentication failed", 0)
			return true
		}
		sess.Authenticated = true
		sess.ViaAPIKey = true
		sess.Subject = "apikey:" + key.KeyID
		sess.AccountID = key.AccountID
		sess.Scopes = key.Scopes
		sess.Tier = key.RateLimitTier
		sess.KeyID = key.KeyID
	} else {
		if c.srv.cfg.Issuer == nil {
			c.sendError(f.rid(), "authenticate", "UNAUTHORIZED",
				"jwt authentication not configured", 0)
			return true
		}
		claims, err := c.srv.cfg.Issuer.Parse(f.Token)
		if err != nil {
			c.sendError(f.rid(), "authenticate", "UNAUTHORIZED",
				"token validation failed", 0)
			return true
		}
		sess.Authenticated = true
		sess.Subject = claims.Subject
		sess.AccountID = claims.AccountID
		sess.Scopes = claims.Scopes
		sess.KeyID = claims.KeyID
		sess.SessionID = claims.SessionID
		sess.ExpiresAt = claims.ExpiresAt
	}

	// Premium-tier gate: the L3 surface is restricted to professional
	// and above (route registry: TierProfessional). Below-premium
	// sessions get the registered entitlement code — never a silent
	// upgrade or a misleading success.
	tier := ratelimit.ParseTier(sess.Tier)
	if c.srv.cfg.TierResolver != nil {
		tier = c.srv.cfg.TierResolver(context.Background(), &sess)
	}
	if !l3PremiumTier(tier) {
		c.sendError(f.rid(), "authenticate", "ENTITLEMENT_REQUIRED",
			"L3 order-level data requires a premium-tier subscription", 0)
		return false
	}

	// Five simultaneous L3 subscriptions per session/account.
	if !c.srv.accountCapAdmit(c, &sess) {
		c.sendError(f.rid(), "authenticate", "WS_MAX_SUBSCRIPTIONS_EXCEEDED",
			"L3 subscription budget (5 channels) exceeded", 0)
		return false
	}

	sess.RemoteIP = c.remoteIP
	c.setSession(sess)
	c.sendResponse(f.rid(), "authenticate", "ACK", map[string]any{
		"subject":          sess.Subject,
		"account_id":       sess.AccountID,
		"scopes":           sess.Scopes,
		"protocol_version": sess.ProtocolVer,
		"channel":          "l3@" + c.symbol,
	})
	return true
}

// bindAndResume attaches the conn to the hub's subscriber set for
// l3@{symbol} and resolves the replay cursor: inside the 100k ring →
// verbatim replay + resumed; outside → L3_SEQUENCE_GAP_DETECTED +
// complete point-in-time snapshot + live incrementals (spec §11.1).
//
// Ordering gate: the conn subscribes under beginReplay so fanout frames
// landing mid-replay divert to `pending`; replayed frames and control
// frames push directly; endReplay then flushes the deferred live range —
// the client always sees [replay][control][live].
func (c *l3Conn) bindAndResume(lastSeq uint64, haveSeq bool) {
	c.gapNotified.Store(false)
	c.beginReplay()
	res := c.srv.cfg.Hub.attach(c, c.symbol, lastSeq)
	c.attached = true
	defer c.endReplay()
	now := c.srv.cfg.Now().UnixMilli()
	channel := "l3@" + c.symbol

	switch res.Verdict {
	case ReplayOK, ReplayUpToDate:
		for _, m := range res.Msgs {
			c.push(m)
		}
		b, _ := marshalFrame(resumedFrame{
			Type: "resumed", Channel: channel,
			FromSeq: lastSeq + 1, ToSeq: res.Tail,
			Count: len(res.Msgs), TsMs: now,
		})
		c.push(b)
	default:
		// ReplayGapTooLarge / ReplayInvalidSeq / ReplayEmpty-with-cursor:
		// the client's cursor is beyond the replay horizon — the
		// registered gap code precedes the full-state snapshot.
		if !haveSeq {
			// Cold start (no last_seq): emit the point-in-time snapshot
			// then live incrementals — no gap error, the client never
			// had a chain to break.
			c.emitL3Snapshot(channel, "cold_start")
			return
		}
		b, _ := marshalFrame(errorFrame{
			Type: "error", Error: "L3_SEQUENCE_GAP_DETECTED",
			Message: "last_seq is outside the 100,000-event replay horizon; " +
				"serving a complete snapshot",
			TsMs: now,
		})
		c.push(b)
		c.emitL3Snapshot(channel, "gap_too_large")
	}
}

// emitL3Snapshot pushes the mirror's point-in-time book state (when the
// mirror is trustworthy) plus a resync directive naming the seq the
// client binds as its new last_seq. A suspect mirror (post feed-gap)
// degrades to the resync directive alone — the WAL snapshot REST
// endpoint is the durable fallback (§2.7: never serve known-holed state).
func (c *l3Conn) emitL3Snapshot(channel, reason string) {
	now := c.srv.cfg.Now().UnixMilli()
	snap, err := c.srv.cfg.Hub.snapshot(c.symbol)
	if err != nil || snap == nil || snap.Truncated {
		b, _ := marshalFrame(resyncFrame{
			Type: "resync", Channel: channel, Reason: "no_snapshot_available",
			TsMs: now,
		})
		c.push(b)
		c.srv.cfg.Hub.cfg.Metrics.ResyncDirectives.Add(1)
		return
	}
	b, _ := marshalFrame(snapshotFrame{
		Type: "snapshot", Channel: channel, Reason: reason,
		Seq: snap.Seq, Data: snap, TsMs: now,
	})
	c.push(b)
	c.srv.cfg.Hub.cfg.Metrics.ResyncDirectives.Add(1)
}
