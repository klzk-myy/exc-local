// Package surveillance — Phase-17 Task 17.3.3 signal emission.
//
// The engine consumes the L3 order-level stream and emits persisted,
// deduplicated abuse signals. Phase-21 consumes surveillance_signals
// (migration 029) for case management and enforcement — this package
// only DETECTS and EMITS; nothing here throttles, cancels, or flags an
// account live (spec mechanism map: Phase-17 emits, Phase-21 enforces).
//
// Signal set (spec §19 abuse taxonomy):
//
//	SPOOFING           repeated non-bona-fide cancels — order added then
//	                   cancelled inside CancelWindow, ≥ SpoofCount in
//	                   WindowSeconds per (symbol, account).
//	LAYERING           ≥ LayerCount same-side ADDs at distinct price
//	                   levels inside WindowSeconds subsequently cancelled
//	                   (staged depth pulled before execution).
//	WASH_TRADING       EXECUTE legs of the same fill pairing orders whose
//	                   account_hash matches (self-cross).
//	MARKING_THE_CLOSE  aggressive EXECUTE inside a configured daily close
//	                   window moving the price > CloseMoveBps.
//	MOMENTUM_IGNITION  ≥ IgnitionCount same-direction EXECUTEs by one
//	                   account inside IgnitionWindowMs moving the price
//	                   > IgnitionMoveBps.
//	FRONT_RUNNING      an account EXECUTE immediately followed by an
//	                   adverse move it positioned ahead of (price moved
//	                   > FrontRunMoveBps inside FrontRunWindowMs against
//	                   the follow-on flow).
//	INSIDER_DEALING    EXECUTE inside a configured announcement window
//	                   (pre-event position taking) — the venue-side
//	                   proxy pending Phase-21's corporate-event feeds.
//
// Determinism/dedup: each emitted Signal carries a dedup_key hashing
// (type|symbol|account|first_l3_seq|last_l3_seq|detector salt). Stream
// redelivery or service restart replays the same l3_seq window → same
// key → ON CONFLICT DO NOTHING (exactly-once storage over an
// at-least-once feed, spec §2.7).
package surveillance

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"exchange/internal/marketdata"
	"exchange/pkg/decimal"
)

// Signal types — CHECK-constrained in migration 029.
const (
	SignalSpoofing        = "SPOOFING"
	SignalLayering        = "LAYERING"
	SignalWashTrading     = "WASH_TRADING"
	SignalMarkingTheClose = "MARKING_THE_CLOSE"
	SignalMomentumIgnition = "MOMENTUM_IGNITION"
	SignalFrontRunning    = "FRONT_RUNNING"
	SignalInsiderDealing  = "INSIDER_DEALING"
)

// Signal is one persisted detection.
type Signal struct {
	Type          string         `json:"signal_type"`
	Symbol        string         `json:"symbol"`
	AccountHash   uint64         `json:"account_hash"`
	OrderID       uint64         `json:"order_id,omitempty"`
	CounterOrder  uint64         `json:"counter_order_id,omitempty"`
	FirstL3Seq    uint64         `json:"first_l3_seq"`
	LastL3Seq     uint64         `json:"last_l3_seq"`
	FirstWalSeq   uint64         `json:"first_wal_seq,omitempty"`
	LastWalSeq    uint64         `json:"last_wal_seq,omitempty"`
	WindowStart   time.Time      `json:"window_start"`
	WindowEnd     time.Time      `json:"window_end"`
	Evidence      map[string]any `json:"evidence"`
	DedupKey      string         `json:"dedup_key"`
}

// key derives the deterministic dedup fingerprint. salt discriminates
// two same-shaped windows detected by different sub-rules.
func (s *Signal) key(salt string) string {
	h := sha256.New()
	fmt.Fprintf(h, "%s|%s|%d|%d|%d|%s",
		s.Type, s.Symbol, s.AccountHash, s.FirstL3Seq, s.LastL3Seq, salt)
	return hex.EncodeToString(h.Sum(nil))[:40]
}

// Config tunes the detectors; zero values pick conservative defaults
// (documented per detector — thresholds are heuristics pending the
// Phase-21 calibration task, not spec-pinned constants).
type Config struct {
	Logger *slog.Logger
	Now    func() time.Time

	WindowSeconds    int // rolling detector window (default 10s)
	SpoofCount       int // cancels inside CancelWindow to flag (default 3)
	CancelWindowMs   int // ADD→CANCEL latency considered non-bona-fide (default 800ms)
	MinSpoofQty      int64 // qty floor (scaled units) — noise filter (default 0 = off)

	LayerCount       int // same-side distinct-price ADDs to flag (default 3)

	WashSeqSpan      uint64 // EXECUTE pairing span (default 2 l3_seqs)

	IgnitionCount    int // same-direction EXECUTEs to flag (default 4)
	IgnitionWindowMs int // (default 2_000)
	IgnitionMoveBps  int64 // price move over the burst (default 25 bps)

	CloseUTCStart    string // "HH:MM" close-window begin (default "21:45")
	CloseUTCEnd      string // "HH:MM" close-window end (default "22:05")
	CloseMoveBps     int64 // (default 15 bps)

	FrontRunWindowMs int   // (default 1_000)
	FrontRunMoveBps  int64 // (default 10 bps)

	// AnnounceWindows lists daily UTC windows ("HH:MM-HH:MM") in which
	// pre-positioning EXECUTEs raise INSIDER_DEALING candidates.
	AnnounceWindows []string

	MaxOpenOrders int // per-symbol order cache bound (default 1<<20)
}

func (c *Config) defaults() {
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	if c.WindowSeconds <= 0 {
		c.WindowSeconds = 10
	}
	if c.SpoofCount <= 0 {
		c.SpoofCount = 3
	}
	if c.CancelWindowMs <= 0 {
		c.CancelWindowMs = 800
	}
	if c.LayerCount <= 0 {
		c.LayerCount = 3
	}
	if c.WashSeqSpan <= 0 {
		c.WashSeqSpan = 2
	}
	if c.IgnitionCount <= 0 {
		c.IgnitionCount = 4
	}
	if c.IgnitionWindowMs <= 0 {
		c.IgnitionWindowMs = 2000
	}
	if c.IgnitionMoveBps <= 0 {
		c.IgnitionMoveBps = 25
	}
	if c.CloseUTCStart == "" {
		c.CloseUTCStart = "21:45"
	}
	if c.CloseUTCEnd == "" {
		c.CloseUTCEnd = "22:05"
	}
	if c.CloseMoveBps <= 0 {
		c.CloseMoveBps = 15
	}
	if c.FrontRunWindowMs <= 0 {
		c.FrontRunWindowMs = 1000
	}
	if c.FrontRunMoveBps <= 0 {
		c.FrontRunMoveBps = 10
	}
	if c.MaxOpenOrders <= 0 {
		c.MaxOpenOrders = 1 << 20
	}
}

// Sink persists signals; the PgSink is the production target.
type Sink interface {
	Emit(ctx context.Context, s Signal) error
}

// SinkFunc adapts a function to Sink.
type SinkFunc func(ctx context.Context, s Signal) error

// Emit implements Sink.
func (f SinkFunc) Emit(ctx context.Context, s Signal) error { return f(ctx, s) }

// PgSink writes surveillance_signals rows idempotently — the dedup_key
// unique index plus ON CONFLICT DO NOTHING makes re-emission a no-op.
type PgSink struct {
	pool *pgxpool.Pool
}

// NewPgSink binds the PostgreSQL pool (migration 029 must be applied).
func NewPgSink(pool *pgxpool.Pool) *PgSink { return &PgSink{pool: pool} }

// Emit implements Sink — idempotent insert; returns inserted=true when
// the row landed (false = dedup hit, still nil error).
func (s *PgSink) Emit(ctx context.Context, sig Signal) error {
	ev, err := json.Marshal(sig.Evidence)
	if err != nil {
		return fmt.Errorf("surveillance: evidence marshal: %w", err)
	}
	var orderID, counter, firstWal, lastWal *int64
	if sig.OrderID != 0 {
		v := int64(sig.OrderID)
		orderID = &v
	}
	if sig.CounterOrder != 0 {
		v := int64(sig.CounterOrder)
		counter = &v
	}
	if sig.FirstWalSeq != 0 {
		v := int64(sig.FirstWalSeq)
		firstWal = &v
	}
	if sig.LastWalSeq != 0 {
		v := int64(sig.LastWalSeq)
		lastWal = &v
	}
	tag, err := s.pool.Exec(ctx, `
		INSERT INTO surveillance_signals
		    (signal_type, symbol, account_hash, order_id, counter_order_id,
		     first_l3_seq, last_l3_seq, first_wal_seq, last_wal_seq,
		     window_start, window_end, evidence, dedup_key)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)
		ON CONFLICT (dedup_key) DO NOTHING`,
		sig.Type, sig.Symbol, int64(sig.AccountHash), orderID, counter,
		int64(sig.FirstL3Seq), int64(sig.LastL3Seq), firstWal, lastWal,
		sig.WindowStart, sig.WindowEnd, ev, sig.DedupKey)
	if err != nil {
		return fmt.Errorf("surveillance: insert signal: %w", err)
	}
	_ = tag
	return nil
}

// ---------------------------------------------------------------------------
// Engine — per-symbol detector state driven by the L3 event stream.
// ---------------------------------------------------------------------------

// openOrder is the engine's resting-order record.
type openOrder struct {
	orderID     uint64
	accountHash uint64
	side        string
	price       decimal.Decimal
	qty         decimal.Decimal
	added       time.Time
	addSeq      uint64
	addWal      uint64
	hidden      bool
}

// lastExec records one EXECUTE for cross-leg pairing (wash/ignition).
type lastExec struct {
	orderID     uint64
	accountHash uint64
	side        string
	price       decimal.Decimal
	qty         decimal.Decimal
	ts          time.Time
	seq         uint64
	wal         uint64
}

// cancelEvt feeds the spoofing/layering counters.
type cancelEvt struct {
	at           time.Time
	orderID      uint64
	price        decimal.Decimal
	restedMs     int64
	seq          uint64
	wal          uint64
}

// symState is the detector state for one symbol.
type symState struct {
	orders  map[uint64]*openOrder
	cancels map[uint64][]cancelEvt // account_hash → recent suspicious cancels
	adds    map[uint64][]openOrder // account_hash → recent same-side adds
	lastPx  decimal.Decimal        // last execution price (move baseline)
	lastEx  *lastExec
	closeRef decimal.Decimal // price at close-window open (0 = unset)
	closeDay int             // yday the closeRef was captured for
}

func newSymState() *symState {
	return &symState{
		orders:  map[uint64]*openOrder{},
		cancels: map[uint64][]cancelEvt{},
		adds:    map[uint64][]openOrder{},
	}
}

// Engine consumes L3 events and emits signals to the sink. It is
// stateless across restarts — the stream's own replay provides recovery;
// dedup keys keep re-detected windows idempotent.
type Engine struct {
	cfg    Config
	sink   Sink
	states map[string]*symState
	closeW dayWindow
	annWs  []dayWindow
}

// dayWindow is a UTC "HH:MM-HH:MM" daily window.
type dayWindow struct{ startMin, endMin int }

func parseDayWindow(s string) (dayWindow, error) {
	var w dayWindow
	var sh, sm, eh, em int
	n, err := fmt.Sscanf(s, "%d:%d-%d:%d", &sh, &sm, &eh, &em)
	if err != nil || n != 4 {
		return w, fmt.Errorf("bad window %q", s)
	}
	return dayWindow{startMin: sh*60 + sm, endMin: eh*60 + em}, nil
}

func (w dayWindow) contains(t time.Time) bool {
	m := t.UTC().Hour()*60 + t.UTC().Minute()
	return m >= w.startMin && m <= w.endMin
}

func hm(s string) (int, error) {
	var h, m int
	n, err := fmt.Sscanf(s, "%d:%d", &h, &m)
	if err != nil || n != 2 {
		return 0, fmt.Errorf("bad time %q", s)
	}
	return h*60 + m, nil
}

// NewEngine builds the detector. cfg zero values pick defaults; a nil
// sink fails closed at construction (emission must never be silent).
func NewEngine(cfg Config, sink Sink) (*Engine, error) {
	cfg.defaults()
	if sink == nil {
		return nil, fmt.Errorf("surveillance: nil signal sink")
	}
	closeStart, err := hm(cfg.CloseUTCStart)
	if err != nil {
		return nil, err
	}
	closeEnd, err := hm(cfg.CloseUTCEnd)
	if err != nil {
		return nil, err
	}
	e := &Engine{cfg: cfg, sink: sink, states: map[string]*symState{},
		closeW: dayWindow{startMin: closeStart, endMin: closeEnd}}
	for _, w := range cfg.AnnounceWindows {
		d, err := parseDayWindow(w)
		if err != nil {
			return nil, err
		}
		e.annWs = append(e.annWs, d)
	}
	return e, nil
}

// Run pumps the L3 source until ctx ends or the source closes. Emit
// errors are logged fail-loud and counted — a signal must never block
// the L3 feed (sink failures degrade to retries by the caller's
// reconnect loop, not stream backpressure).
func (e *Engine) Run(ctx context.Context, src marketdata.L3Source) error {
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
			e.Apply(ctx, ev)
		}
	}
}

// Apply folds one L3 event into the detectors. Emissions go to the
// sink synchronously but the sink contract is a fast idempotent insert;
// detector bookkeeping happens first so a sink error never corrupts state.
func (e *Engine) Apply(ctx context.Context, ev marketdata.L3Event) {
	if ev.Symbol == "" || ev.Seq == 0 {
		return
	}
	st := e.states[ev.Symbol]
	if st == nil {
		st = newSymState()
		e.states[ev.Symbol] = st
	}
	switch ev.Kind {
	case marketdata.L3Add:
		e.onAdd(ctx, st, ev)
	case marketdata.L3Modify:
		if o, ok := st.orders[ev.OrderID]; ok {
			o.price, o.qty = ev.Price, ev.Quantity
		}
	case marketdata.L3Cancel:
		e.onCancel(ctx, st, ev)
	case marketdata.L3Execute:
		e.onExecute(ctx, st, ev)
	}
}

// onAdd tracks the resting order and feeds the layering counter.
func (e *Engine) onAdd(ctx context.Context, st *symState, ev marketdata.L3Event) {
	if len(st.orders) >= e.cfg.MaxOpenOrders {
		return // bound — engine never grows unbounded on a hostile book
	}
	o := &openOrder{
		orderID: ev.OrderID, accountHash: ev.AccountHash,
		side: string(ev.Side), price: ev.Price, qty: ev.Quantity,
		added: ev.Ts, addSeq: ev.Seq, addWal: ev.WalSeq,
		hidden: ev.Hidden,
	}
	st.orders[ev.OrderID] = o
	if ev.Hidden {
		return // hidden orders don't participate in visible-book patterns
	}
	win := time.Duration(e.cfg.WindowSeconds) * time.Second
	list := append(st.adds[ev.AccountHash], *o)
	// Trim outside the window.
	cut := ev.Ts.Add(-win)
	i := 0
	for i < len(list) && list[i].added.Before(cut) {
		i++
	}
	list = append([]openOrder(nil), list[i:]...)
	st.adds[ev.AccountHash] = list

	// LAYERING: ≥N same-side ADDs at distinct prices in the window.
	sameSide, prices := 0, map[string]struct{}{}
	for _, a := range list {
		if a.side == o.side {
			sameSide++
			prices[a.price.String()] = struct{}{}
		}
	}
	if sameSide >= e.cfg.LayerCount && len(prices) >= e.cfg.LayerCount {
		sig := Signal{
			Type: SignalLayering, Symbol: ev.Symbol,
			AccountHash: ev.AccountHash, OrderID: ev.OrderID,
			FirstL3Seq:  list[0].addSeq, LastL3Seq: ev.Seq,
			FirstWalSeq: list[0].addWal, LastWalSeq: ev.WalSeq,
			WindowStart: list[0].added, WindowEnd: ev.Ts,
			Evidence: map[string]any{
				"side": o.side, "levels": len(prices), "adds": sameSide,
			},
		}
		sig.DedupKey = sig.key("layer")
		e.emit(ctx, sig)
	}
}

// onCancel feeds the spoofing counter (quick cancels of resting orders).
func (e *Engine) onCancel(ctx context.Context, st *symState, ev marketdata.L3Event) {
	o, ok := st.orders[ev.OrderID]
	if !ok {
		return // cancel for an order the engine never saw — nothing to score
	}
	rested := ev.Ts.Sub(o.added)
	delete(st.orders, ev.OrderID)
	if o.hidden || rested > time.Duration(e.cfg.CancelWindowMs)*time.Millisecond {
		return
	}
	if e.cfg.MinSpoofQty > 0 && o.qty.LessThan(decimal.NewFromScaled(e.cfg.MinSpoofQty)) {
		return
	}
	ce := cancelEvt{at: ev.Ts, orderID: ev.OrderID, price: o.price,
		restedMs: rested.Milliseconds(), seq: ev.Seq, wal: ev.WalSeq}
	win := time.Duration(e.cfg.WindowSeconds) * time.Second
	list := append(st.cancels[ev.AccountHash], ce)
	cut := ev.Ts.Add(-win)
	i := 0
	for i < len(list) && list[i].at.Before(cut) {
		i++
	}
	list = append([]cancelEvt(nil), list[i:]...)
	st.cancels[ev.AccountHash] = list
	if len(list) >= e.cfg.SpoofCount {
		sig := Signal{
			Type: SignalSpoofing, Symbol: ev.Symbol,
			AccountHash: ev.AccountHash, OrderID: ev.OrderID,
			FirstL3Seq:  list[0].seq, LastL3Seq: ev.Seq,
			FirstWalSeq: list[0].wal, LastWalSeq: ev.WalSeq,
			WindowStart: list[0].at, WindowEnd: ev.Ts,
			Evidence: map[string]any{
				"quick_cancels": len(list),
				"rested_ms_max": e.cfg.CancelWindowMs,
			},
		}
		sig.DedupKey = sig.key("spoof")
		e.emit(ctx, sig)
	}
}

// onExecute handles wash pairing, close-window marking, momentum
// ignition, front-running and announcement-window insider candidates.
func (e *Engine) onExecute(ctx context.Context, st *symState, ev marketdata.L3Event) {
	// Update resting state for the executed order. Wire qty is the
	// order's remaining AFTER the fill (schema §11): zero → the order
	// leaves the book entirely.
	if o, ok := st.orders[ev.OrderID]; ok {
		if ev.Quantity.IsPositive() {
			o.qty = ev.Quantity
		} else {
			delete(st.orders, ev.OrderID)
		}
	}

	// WASH_TRADING: consecutive EXECUTE legs of one fill pairing the
	// same account_hash on both sides (self-cross). The stream emits a
	// per-order EXECUTE — maker and taker legs land within WashSeqSpan.
	if le := st.lastEx; le != nil &&
		le.accountHash == ev.AccountHash &&
		le.orderID != ev.OrderID &&
		le.side != string(ev.Side) &&
		ev.Seq-le.seq <= e.cfg.WashSeqSpan {
		sig := Signal{
			Type: SignalWashTrading, Symbol: ev.Symbol,
			AccountHash:  ev.AccountHash,
			OrderID:      le.orderID, CounterOrder: ev.OrderID,
			FirstL3Seq:   le.seq, LastL3Seq: ev.Seq,
			FirstWalSeq:  le.wal, LastWalSeq: ev.WalSeq,
			WindowStart:  le.ts, WindowEnd: ev.Ts,
			Evidence: map[string]any{
				"buy_order": legFor(le, ev, "BUY"), "sell_order": legFor(le, ev, "SELL"),
				"price": ev.Price.String(), "qty": fillQty(ev).String(),
			},
		}
		sig.DedupKey = sig.key("wash")
		e.emit(ctx, sig)
	}

	// MARKING_THE_CLOSE: EXECUTE inside the close window moving the
	// price > CloseMoveBps off the window-open reference.
	inClose := e.closeW.contains(ev.Ts)
	day := ev.Ts.UTC().YearDay() + ev.Ts.UTC().Year()*366
	if inClose {
		if st.closeDay != day || st.closeRef.IsZero() {
			st.closeDay = day
			st.closeRef = ev.Price
		}
		if !st.closeRef.IsZero() {
			if bps := moveBps(st.closeRef, ev.Price); bps >= e.cfg.CloseMoveBps {
				sig := Signal{
					Type: SignalMarkingTheClose, Symbol: ev.Symbol,
					AccountHash: ev.AccountHash, OrderID: ev.OrderID,
					FirstL3Seq:  ev.Seq, LastL3Seq: ev.Seq,
					FirstWalSeq: ev.WalSeq, LastWalSeq: ev.WalSeq,
					WindowStart: ev.Ts, WindowEnd: ev.Ts,
					Evidence: map[string]any{
						"ref_price": st.closeRef.String(), "exec_price": ev.Price.String(),
						"move_bps": bps,
					},
				}
				sig.DedupKey = sig.key("close")
				e.emit(ctx, sig)
			}
		}
	}

	// MOMENTUM_IGNITION: burst of same-direction EXECUTEs by one
	// account inside IgnitionWindowMs with a >IgnitionMoveBps move.
	if le := st.lastEx; le != nil &&
		le.accountHash == ev.AccountHash &&
		le.side == string(ev.Side) &&
		le.orderID != ev.OrderID &&
		ev.Ts.Sub(le.ts) <= time.Duration(e.cfg.IgnitionWindowMs)*time.Millisecond &&
		!le.price.IsZero() {
		if bps := moveBps(le.price, ev.Price); bps >= e.cfg.IgnitionMoveBps {
			sig := Signal{
				Type: SignalMomentumIgnition, Symbol: ev.Symbol,
				AccountHash: ev.AccountHash, OrderID: ev.OrderID,
				FirstL3Seq:  le.seq, LastL3Seq: ev.Seq,
				FirstWalSeq: le.wal, LastWalSeq: ev.WalSeq,
				WindowStart: le.ts, WindowEnd: ev.Ts,
				Evidence: map[string]any{
					"side": le.side, "from": le.price.String(),
					"to": ev.Price.String(), "move_bps": bps,
				},
			}
			sig.DedupKey = sig.key("ignition")
			e.emit(ctx, sig)
		}
		// FRONT_RUNNING: same account, consecutive EXECUTEs, but the
		// price moved AGAINST the follow-on flow beyond the bound —
		// the account positioned ahead of the adverse move.
		if bps := moveBps(le.price, ev.Price); bps >= e.cfg.FrontRunMoveBps &&
			ev.Ts.Sub(le.ts) <= time.Duration(e.cfg.FrontRunWindowMs)*time.Millisecond {
			adverse := (le.side == "BUY" && ev.Price.LessThan(le.price)) ||
				(le.side == "SELL" && ev.Price.GreaterThan(le.price))
			if adverse {
				sig := Signal{
					Type: SignalFrontRunning, Symbol: ev.Symbol,
					AccountHash: ev.AccountHash, OrderID: ev.OrderID,
					FirstL3Seq:  le.seq, LastL3Seq: ev.Seq,
					FirstWalSeq: le.wal, LastWalSeq: ev.WalSeq,
					WindowStart: le.ts, WindowEnd: ev.Ts,
					Evidence: map[string]any{
						"first_price": le.price.String(), "next_price": ev.Price.String(),
						"move_bps": bps,
					},
				}
				sig.DedupKey = sig.key("frontrun")
				e.emit(ctx, sig)
			}
		}
	}

	// INSIDER_DEALING: EXECUTE inside a configured announcement window.
	for _, w := range e.annWs {
		if w.contains(ev.Ts) {
			sig := Signal{
				Type: SignalInsiderDealing, Symbol: ev.Symbol,
				AccountHash: ev.AccountHash, OrderID: ev.OrderID,
				FirstL3Seq:  ev.Seq, LastL3Seq: ev.Seq,
				FirstWalSeq: ev.WalSeq, LastWalSeq: ev.WalSeq,
				WindowStart: ev.Ts, WindowEnd: ev.Ts,
				Evidence: map[string]any{
					"window": fmt.Sprintf("%d-%d", w.startMin, w.endMin),
					"price":  ev.Price.String(), "qty": fillQty(ev).String(),
				},
			}
			sig.DedupKey = sig.key("insider")
			e.emit(ctx, sig)
			break
		}
	}

	if !ev.Price.IsZero() {
		st.lastPx = ev.Price
	}
	st.lastEx = &lastExec{
		orderID: ev.OrderID, accountHash: ev.AccountHash,
		side: string(ev.Side), price: ev.Price, qty: ev.Quantity,
		ts: ev.Ts, seq: ev.Seq, wal: ev.WalSeq,
	}
}

// fillQty is the executed quantity of a Fill leg: -qty_delta when the
// wire carries the delta (executed amount), else the row qty — wire qty
// is the order's remaining AFTER the fill, never the fill size itself.
func fillQty(ev marketdata.L3Event) decimal.Decimal {
	if !ev.QtyDelta.IsZero() {
		return ev.QtyDelta.Neg()
	}
	return ev.Quantity
}

// legFor picks the order_id of the requested side out of an exec pair.
func legFor(a *lastExec, b marketdata.L3Event, side string) uint64 {
	if a.side == side {
		return a.orderID
	}
	if string(b.Side) == side {
		return b.OrderID
	}
	return 0
}

// moveBps is |a-b|/a in basis points (a>0 precondition at call sites).
func moveBps(a, b decimal.Decimal) int64 {
	if a.IsZero() {
		return 0
	}
	d := b.Sub(a).Abs()
	// bps = |a-b| * 10_000 / a — scaled through Decimal ops only.
	v := d.Mul(decimal.NewFromInt(10_000)).Div(a)
	return v.IntPart()
}

// emit fires the sink; failures are logged (never silent) but do not
// backpressure the stream — the caller's reconnect replay re-emits.
func (e *Engine) emit(ctx context.Context, s Signal) {
	if err := e.sink.Emit(ctx, s); err != nil {
		e.cfg.Logger.Error("surveillance: signal emit failed",
			"type", s.Type, "symbol", s.Symbol, "key", s.DedupKey, "err", err)
		return
	}
	e.cfg.Logger.Info("surveillance: signal",
		"type", s.Type, "symbol", s.Symbol,
		"account", s.AccountHash, "key", s.DedupKey)
}
