// TCA engine — Phase-20 Task 20.3.9: per-fill execution-quality metrics
// against the arrival mid, the session VWAP and the ECB/WM reference
// fix, persisted to ClickHouse `tca_results` (MiFID II Art. 27
// best-execution evidence + institutional TCA reporting).
//
// Benchmark contracts:
//   - Arrival price: the symbol mid at order receipt. ArrivalSource is
//     the seam; OracleArrivalSource reads the published oracle mark
//     (oracle:mark:{sym} keyspace — an approximation documented in
//     ArrivalSource's contract: the production orchestrator may bind a
//     receipt-time book-mid source when Phase-02 order events expose
//     one). Absent mark → the column records NULL, never a fabrication.
//   - Session VWAP: sum(price×qty)/sum(qty) over the fill's session
//     window read from the ClickHouse tick store (CHSessionVWAP).
//   - ECB fix: the published fix for the symbol (RedisFixSource reads
//     `oracle:fix:{symbol}` written by the ECB feed adapter's fix
//     publication — spec §19.5 oracle keyspace extension). A fill with
//     no published fix records fix_absent, not an interpolated value.
//   - Price improvement: trade-through-prevention events arrive on
//     `compliance.trade_through.{symbol}` (PriceImprovementEvent; the
//     Phase-02 guard is the producer — no publisher existed before this
//     task, the subject is bound here). delta = limit − exec verbatim;
//     zero/negative is adverse selection and is recorded faithfully.
package analytics

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	goredis "github.com/redis/go-redis/v9"

	"exchange/internal/ipc"
	"exchange/internal/ipc/wire"
	"exchange/internal/oracle"
	"exchange/internal/settlement"
	"exchange/pkg/decimal"
)

// TCATable is the ClickHouse ReplacingMergeTree the sibling Task-20
// migration owns — columns:
//
//	account_id, instrument_id, symbol, fill_id, order_id, exec_price,
//	arrival_price, session_vwap, ecb_fix, slip_arrival_bps,
//	slip_vwap_bps, slip_fix_bps, price_improvement_delta, period,
//	period_start, ts, ver
const TCATable = "tca_results"

// The VWAP source reads the sibling's `ticks` store (ticks.go — columns
// ts, symbol, price, quantity, side, trade_id, event_seq, shard_id, ver;
// TickTable is the table-name const).

// TCAFill is one account-side fill leg — the engine emits one TCARecord
// per leg per fill (matching the confirmation pipeline's per-leg
// semantics: the maker and taker each get their own execution-quality
// view).
type TCAFill struct {
	FillID       int64 // engine trade_id
	OrderID      int64 // this leg's order
	AccountID    int64
	InstrumentID int64
	Symbol       string
	Side         string // BUY | SELL
	ExecPrice    decimal.Decimal
	Qty          decimal.Decimal
	LimitPrice   *decimal.Decimal // nil for market orders
	ReceivedAt   time.Time        // order receipt (arrival reference)
	ExecutedAt   time.Time        // fill timestamp
}

// TCARecord is one tca_results row (decimal columns serialized as
// strings — the CH Decimal columns parse them natively).
type TCARecord struct {
	AccountID    int64
	InstrumentID int64
	Symbol       string
	FillID       int64
	OrderID      int64
	ExecPrice    decimal.Decimal
	// Benchmarks: nil → CH NULL (absent data condition, recorded not
	// fabricated — spec §2.7).
	ArrivalPrice *decimal.Decimal
	SessionVWAP  *decimal.Decimal
	ECBFix       *decimal.Decimal
	// Side-signed slippage in basis points: positive = worse than the
	// benchmark (BUY: exec−bench; SELL: bench−exec). nil when the
	// benchmark was absent.
	SlipArrivalBps *decimal.Decimal
	SlipVWAPBps    *decimal.Decimal
	SlipFixBps     *decimal.Decimal
	// PriceImprovement is limit_price − exec_price for maker-improving
	// fills (trade-through prevention). Zero/negative = adverse
	// selection — recorded verbatim. nil when no prevention event
	// matched the order.
	PriceImprovement *decimal.Decimal
	// FixAbsent marks rows computed without an ECB fix (benchmark-not-
	//-published is a data condition, distinct from a zero slip).
	FixAbsent   bool
	Period      string // "fill" | "daily" | "monthly" | "quarterly"
	PeriodStart time.Time
	Ts          time.Time
	Ver         uint64
}

// ---------------------------------------------------------------------------
// Benchmark sources
// ---------------------------------------------------------------------------

// ArrivalSource resolves the mid price at (symbol, at). found=false is
// an absent-mark data condition — the record carries NULL.
type ArrivalSource interface {
	Arrival(ctx context.Context, symbol string, at time.Time) (decimal.Decimal, bool, error)
}

// OracleArrivalSource approximates arrival from the oracle's published
// mark (Provider.Mark). This is the mark nearest the fill, not a
// receipt-time snapshot — the honest default while the order-event
// book-mid seam doesn't exist; the orchestrator binds a receipt-time
// source over the Phase-02 order event feed when one lands.
type OracleArrivalSource struct {
	P oracle.MarkReader
}

// Arrival implements ArrivalSource — stale/absent marks report
// found=false (a stale benchmark is an absent benchmark).
func (s OracleArrivalSource) Arrival(ctx context.Context, symbol string, _ time.Time) (decimal.Decimal, bool, error) {
	v, err := s.P.Mark(ctx, symbol)
	if err != nil {
		return decimal.Zero, false, fmt.Errorf("tca: arrival mark %s: %w", symbol, err)
	}
	if !v.Found || v.Stale || !v.Price.IsPositive() {
		return decimal.Zero, false, nil
	}
	return v.Price, true, nil
}

// MemArrivalSource is the test/static source — Set(symbol, price).
type MemArrivalSource struct {
	mu sync.RWMutex
	m  map[string]decimal.Decimal
}

// NewMemArrivalSource builds the fake.
func NewMemArrivalSource() *MemArrivalSource {
	return &MemArrivalSource{m: map[string]decimal.Decimal{}}
}

// Set installs (or removes with a non-positive price) a mark.
func (s *MemArrivalSource) Set(symbol string, p decimal.Decimal) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !p.IsPositive() {
		delete(s.m, symbol)
		return
	}
	s.m[symbol] = p
}

// Arrival implements ArrivalSource.
func (s *MemArrivalSource) Arrival(_ context.Context, symbol string, _ time.Time) (decimal.Decimal, bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	p, ok := s.m[symbol]
	return p, ok, nil
}

// ---------------------------------------------------------------------------

// SessionVWAPSource resolves the volume-weighted average price over
// [start, end] for symbol — the session window is the fill's FX trading
// day boundary, computed by the caller.
type SessionVWAPSource interface {
	SessionVWAP(ctx context.Context, symbol string, start, end time.Time) (decimal.Decimal, bool, error)
}

// CHSessionVWAP computes VWAP from the ClickHouse tick store — the
// documented (symbol, price, qty, ts) shape; an absent/empty window is
// found=false.
type CHSessionVWAP struct {
	CH    Conn
	Table string // override; default "ticks"
}

// SessionVWAP implements SessionVWAPSource.
func (s CHSessionVWAP) SessionVWAP(ctx context.Context, symbol string, start, end time.Time) (decimal.Decimal, bool, error) {
	table := s.Table
	if table == "" {
		table = TickTable
	}
	rows, err := s.CH.Query(ctx, fmt.Sprintf(`
		SELECT sum(price * quantity) / nullIf(sum(quantity), 0)
		FROM %s
		WHERE symbol = ? AND ts >= ? AND ts <= ?`, table),
		symbol, start.UTC(), end.UTC())
	if err != nil {
		return decimal.Zero, false, fmt.Errorf("tca: vwap %s: %w", symbol, err)
	}
	defer rows.Close()
	if !rows.Next() {
		return decimal.Zero, false, nil
	}
	var raw *float64 // CH Nullable(Decimal) surfaces as *float64 via the driver
	if err := rows.Scan(&raw); err != nil {
		return decimal.Zero, false, fmt.Errorf("tca: vwap scan %s: %w", symbol, err)
	}
	if raw == nil || *raw <= 0 {
		return decimal.Zero, false, nil
	}
	return decimal.NewFromFloat(*raw), true, nil
}

// MemSessionVWAP is the scripted test source.
type MemSessionVWAP struct {
	mu sync.RWMutex
	m  map[string]decimal.Decimal
}

// NewMemSessionVWAP builds the fake.
func NewMemSessionVWAP() *MemSessionVWAP {
	return &MemSessionVWAP{m: map[string]decimal.Decimal{}}
}

// Set installs a VWAP for symbol (non-positive removes → absent).
func (s *MemSessionVWAP) Set(symbol string, v decimal.Decimal) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !v.IsPositive() {
		delete(s.m, symbol)
		return
	}
	s.m[symbol] = v
}

// SessionVWAP implements SessionVWAPSource.
func (s *MemSessionVWAP) SessionVWAP(_ context.Context, symbol string, _, _ time.Time) (decimal.Decimal, bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	v, ok := s.m[symbol]
	return v, ok, nil
}

// ---------------------------------------------------------------------------

// FixSource resolves the latest published reference fix for a symbol —
// the ECB euro reference rate (published ~14:15 CET) and/or the WM 16:00
// London fix, whichever the producer wrote. found=false = no fix
// published (recorded as fix_absent, never interpolated).
type FixSource interface {
	Fix(ctx context.Context, symbol string) (decimal.Decimal, bool, error)
}

// RedisFixSource reads `oracle:fix:{symbol}` — the fix publication key
// (value is a bare decimal string, optionally "price|unix_ns"). The
// producer binding is the Phase-19.5 ECB feed adapter / fixing
// scheduler (instrument:fixing:{symbol} Redis key is the instrument-lifecycle
// contract; this keyspace is the TCA read seam documented in this file).
func RedisFixKey(symbol string) string {
	return "oracle:fix:" + symbol
}

// RedisFixSource implements FixSource over Redis.
type RedisFixSource struct {
	Rdb goredis.Cmdable
}

// Fix implements FixSource.
func (s RedisFixSource) Fix(ctx context.Context, symbol string) (decimal.Decimal, bool, error) {
	raw, err := s.Rdb.Get(ctx, RedisFixKey(symbol)).Result()
	if err == goredis.Nil {
		return decimal.Zero, false, nil
	}
	if err != nil {
		return decimal.Zero, false, fmt.Errorf("tca: fix read %s: %w", symbol, err)
	}
	raw, _, _ = strings.Cut(raw, "|") // tolerate "price|ts" provenance
	d, err := decimal.NewFromString(strings.TrimSpace(raw))
	if err != nil || !d.IsPositive() {
		return decimal.Zero, false, fmt.Errorf("tca: malformed fix %s %q", symbol, raw)
	}
	return d, true, nil
}

// MemFixSource is the scripted test source.
type MemFixSource struct {
	mu sync.RWMutex
	m  map[string]decimal.Decimal
}

// NewMemFixSource builds the fake.
func NewMemFixSource() *MemFixSource { return &MemFixSource{m: map[string]decimal.Decimal{}} }

// Set installs (non-positive removes) a fix.
func (s *MemFixSource) Set(symbol string, d decimal.Decimal) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !d.IsPositive() {
		delete(s.m, symbol)
		return
	}
	s.m[symbol] = d
}

// Fix implements FixSource.
func (s *MemFixSource) Fix(_ context.Context, symbol string) (decimal.Decimal, bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	d, ok := s.m[symbol]
	return d, ok, nil
}

// ---------------------------------------------------------------------------
// Price improvement (trade-through prevention) events
// ---------------------------------------------------------------------------

// PriceImprovementSubject is the JetStream subject family the producer
// publishes prevention observations on:
//
//	compliance.trade_through.{symbol-token}
//
// Producer binding (documented — no publisher existed before this task):
// the Phase-02 trade-through guard / book-protection layer publishes one
// JSON PriceImprovementEvent per prevention. The compliance stream
// accepts `compliance.>` subjects per the canonical stream table
// (internal/nats/streams.go).
const PriceImprovementSubject = "compliance.trade_through"

// PriceImprovementEvent is the prevention observation: the order's limit
// and the price it actually achieved. Delta = LimitPrice − ExecPrice is
// computed verbatim (negative/zero = adverse selection, recorded).
type PriceImprovementEvent struct {
	OrderID    int64           `json:"order_id"`
	TradeID    int64           `json:"trade_id,omitempty"`
	Symbol     string          `json:"symbol"`
	LimitPrice decimal.Decimal `json:"limit_price"`
	ExecPrice  decimal.Decimal `json:"exec_price"`
	Ts         time.Time       `json:"ts"`
}

// PriceImprovementSource is the pull seam — implementations subscribe to
// the prevention subject. The JetStream adapter lives below
// (PriceImprovementConsumer); the engine consumes via OnPriceImprovement
// so non-NATS producers (tests, direct wiring) work too.
type PriceImprovementSource interface {
	Next(ctx context.Context) (PriceImprovementEvent, bool, error)
}

// pendingImprovementTTL bounds how long a prevention observation waits
// for its fill — a stale match would corrupt a later order's record.
const pendingImprovementTTL = 10 * time.Minute

// ---------------------------------------------------------------------------
// TCASink — the ClickHouse write seam
// ---------------------------------------------------------------------------

// TCASink persists TCARecords (CH impl below; MemTCASink in tests and
// here for dev).
type TCASink interface {
	InsertTCA(ctx context.Context, r TCARecord) error
}

// CHTCASink writes rows through the shared analytics Conn. Insert uses
// parameterized Exec — per-fill rates are bounded by engine throughput
// and the §16.6 batch path belongs to the tick ingest, not this row
// stream.
type CHTCASink struct {
	CH    Conn
	Table string // override; default TCATable
}

// NewCHTCASink wires the sink.
func NewCHTCASink(ch Conn) *CHTCASink { return &CHTCASink{CH: ch, Table: TCATable} }

func decOrNull(d *decimal.Decimal) any {
	if d == nil {
		return nil
	}
	return d.String()
}

// InsertTCA implements TCASink.
func (s *CHTCASink) InsertTCA(ctx context.Context, r TCARecord) error {
	table := s.Table
	if table == "" {
		table = TCATable
	}
	return s.CH.Exec(ctx, fmt.Sprintf(`
		INSERT INTO %s
		    (account_id, instrument_id, symbol, fill_id, order_id,
		     exec_price, arrival_price, session_vwap, ecb_fix,
		     slip_arrival_bps, slip_vwap_bps, slip_fix_bps,
		     price_improvement_delta, period, period_start, ts, ver)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, table),
		r.AccountID, r.InstrumentID, r.Symbol, r.FillID, r.OrderID,
		r.ExecPrice.String(), decOrNull(r.ArrivalPrice), decOrNull(r.SessionVWAP),
		decOrNull(r.ECBFix), decOrNull(r.SlipArrivalBps), decOrNull(r.SlipVWAPBps),
		decOrNull(r.SlipFixBps), decOrNull(r.PriceImprovement),
		r.Period, r.PeriodStart.UTC(), r.Ts.UTC(), r.Ver)
}

// MemTCASink captures rows for tests/dev.
type MemTCASink struct {
	mu   sync.Mutex
	Rows []TCARecord
	Err  error
}

// InsertTCA implements TCASink.
func (s *MemTCASink) InsertTCA(_ context.Context, r TCARecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.Err != nil {
		return s.Err
	}
	s.Rows = append(s.Rows, r)
	return nil
}

// ---------------------------------------------------------------------------
// Engine
// ---------------------------------------------------------------------------

// TCAEngine computes and persists per-fill records. All sources are
// optional individually — a missing source yields NULL columns, not a
// dropped record (the fill itself is never lost).
type TCAEngine struct {
	Arrival ArrivalSource
	VWAP    SessionVWAPSource
	Fix     FixSource
	Sink    TCASink
	Now     func() time.Time
	Logf    func(string, ...any)

	mu      sync.Mutex
	pending map[int64]pendingImprovement // order_id → prevention obs
}

type pendingImprovement struct {
	delta  decimal.Decimal
	expiry time.Time
}

// NewTCAEngine wires the engine; Sink is required (a TCA engine without
// persistence is a no-op that must never start).
func NewTCAEngine(sink TCASink, arrival ArrivalSource, vwap SessionVWAPSource, fix FixSource) (*TCAEngine, error) {
	if sink == nil {
		return nil, fmt.Errorf("tca: sink required")
	}
	return &TCAEngine{
		Arrival: arrival, VWAP: vwap, Fix: fix, Sink: sink,
		Now: time.Now, pending: map[int64]pendingImprovement{},
	}, nil
}

func (e *TCAEngine) log(format string, args ...any) {
	if e.Logf != nil {
		e.Logf(format, args...)
	}
}

// SessionWindow returns the FX session window containing t: the venue
// trades 24/5 with the session boundary at 22:00 UTC (New York close) —
// a fill belongs to the session that opened at the most recent 22:00.
// Used by the VWAP source and the report bucketing.
func SessionWindow(t time.Time) (start, end time.Time) {
	u := t.UTC()
	day := u.Truncate(24 * time.Hour)
	end = u
	start = day.Add(-2 * time.Hour) // 22:00 of the previous UTC day
	if u.Hour() >= 22 {
		start = day.Add(22 * time.Hour) // today's 22:00 open already passed
	}
	return start, end
}

func signedSlipBps(exec, bench decimal.Decimal, side string) decimal.Decimal {
	diff := exec.Sub(bench)
	if strings.EqualFold(side, "SELL") {
		diff = diff.Neg()
	}
	return diff.Div(bench).Mul(decimal.NewFromInt(10000))
}

// OnPriceImprovement records a trade-through-prevention observation. The
// delta is attached to the next fill for the same order seen within
// pendingImprovementTTL; an event for an order that never fills expires
// silently (its own lifecycle already reported it upstream).
func (e *TCAEngine) OnPriceImprovement(ev PriceImprovementEvent) {
	if ev.OrderID <= 0 {
		return
	}
	now := e.Now()
	e.mu.Lock()
	defer e.mu.Unlock()
	// Evict expired entries opportunistically (map stays bounded).
	for k, p := range e.pending {
		if now.After(p.expiry) {
			delete(e.pending, k)
		}
	}
	e.pending[ev.OrderID] = pendingImprovement{
		delta:  ev.LimitPrice.Sub(ev.ExecPrice),
		expiry: now.Add(pendingImprovementTTL),
	}
}

// consumeImprovement pops a pending delta for the order, if any.
func (e *TCAEngine) consumeImprovement(orderID int64) *decimal.Decimal {
	e.mu.Lock()
	defer e.mu.Unlock()
	p, ok := e.pending[orderID]
	if !ok || e.Now().After(p.expiry) {
		if ok {
			delete(e.pending, orderID)
		}
		return nil
	}
	delete(e.pending, orderID)
	d := p.delta
	return &d
}

// OnFill computes and persists the per-leg TCA record. Errors from a
// benchmark source degrade the record (NULL) — only the sink write
// failing propagates (the consumer NAKs and redelivers; the ReplacingMergeTree
// `ver` column absorbs replays).
func (e *TCAEngine) OnFill(ctx context.Context, f TCAFill) (*TCARecord, error) {
	rec := TCARecord{
		AccountID: f.AccountID, InstrumentID: f.InstrumentID,
		Symbol: f.Symbol, FillID: f.FillID, OrderID: f.OrderID,
		ExecPrice: f.ExecPrice,
		Period:    "fill", PeriodStart: f.ExecutedAt.UTC(),
		Ts:  f.ExecutedAt.UTC(),
		Ver: uint64(e.Now().UnixNano()),
	}
	if e.Arrival != nil {
		p, ok, err := e.Arrival.Arrival(ctx, f.Symbol, f.ReceivedAt)
		if err != nil {
			e.log("tca: arrival %s order %d: %v", f.Symbol, f.OrderID, err)
		} else if ok {
			p := p
			rec.ArrivalPrice = &p
			s := signedSlipBps(f.ExecPrice, p, f.Side)
			rec.SlipArrivalBps = &s
		}
	}
	if e.VWAP != nil {
		start, end := SessionWindow(f.ExecutedAt)
		v, ok, err := e.VWAP.SessionVWAP(ctx, f.Symbol, start, end)
		if err != nil {
			e.log("tca: vwap %s order %d: %v", f.Symbol, f.OrderID, err)
		} else if ok {
			v := v
			rec.SessionVWAP = &v
			s := signedSlipBps(f.ExecPrice, v, f.Side)
			rec.SlipVWAPBps = &s
		}
	}
	if e.Fix != nil {
		fix, ok, err := e.Fix.Fix(ctx, f.Symbol)
		if err != nil {
			e.log("tca: fix %s order %d: %v", f.Symbol, f.OrderID, err)
			rec.FixAbsent = true
		} else if !ok {
			rec.FixAbsent = true
		} else {
			fix := fix
			rec.ECBFix = &fix
			s := signedSlipBps(f.ExecPrice, fix, f.Side)
			rec.SlipFixBps = &s
		}
	}
	if d := e.consumeImprovement(f.OrderID); d != nil {
		rec.PriceImprovement = d
	} else if f.LimitPrice != nil {
		// No prevention event — the maker-improving delta is still
		// computable when the fill beat its own limit (limit − exec,
		// verbatim; negative = adverse selection, recorded).
		d := f.LimitPrice.Sub(f.ExecPrice)
		if !d.IsZero() {
			rec.PriceImprovement = &d
		}
	}
	if err := e.Sink.InsertTCA(ctx, rec); err != nil {
		return nil, fmt.Errorf("tca: persist fill %d order %d: %w", f.FillID, f.OrderID, err)
	}
	return &rec, nil
}

// ---------------------------------------------------------------------------
// JetStream adapters
// ---------------------------------------------------------------------------

// PriceImprovementConsumer adapts a JetStream message channel to
// OnPriceImprovement — the orchestrator binds it on the compliance
// stream with filter `compliance.trade_through.>`.
type PriceImprovementConsumer struct {
	Engine *TCAEngine
}

// Msg is one decoded NATS message (jetstream.Msg-compatible shape).
type Msg interface {
	Data() []byte
	Ack() error
	Nak() error
}

// Handle decodes one JSON event; malformed frames ack away (poison
// frames must never wedge the durable).
func (c *PriceImprovementConsumer) Handle(m Msg) error {
	var ev PriceImprovementEvent
	if err := json.Unmarshal(m.Data(), &ev); err != nil {
		_ = m.Ack()
		return nil
	}
	c.Engine.OnPriceImprovement(ev)
	return m.Ack()
}

// decodeTCAFillPayload parses the trades-stream wire frame into the
// engine fill + event timestamp. Shared by the TCA trades consumer.
func decodeTCAFillPayload(data []byte) (fillID, buyOrderID, sellOrderID uint64,
	price, qty decimal.Decimal, ts time.Time, ok bool) {

	defer func() {
		if recover() != nil {
			ok = false
		}
	}()
	if len(data) < 8 {
		return 0, 0, 0, decimal.Zero, decimal.Zero, time.Time{}, false
	}
	ev := ipc.DecodeEvent(data)
	if ev == nil || ev.TypeType() != wire.EventTypeTradeFill {
		return 0, 0, 0, decimal.Zero, decimal.Zero, time.Time{}, false
	}
	tf := ipc.EventTradeFill(ev)
	if tf == nil || tf.TradeId() > math.MaxInt64 {
		return 0, 0, 0, decimal.Zero, decimal.Zero, time.Time{}, false
	}
	return tf.TradeId(), tf.BuyOrderId(), tf.SellOrderId(),
		decimal.NewFromScaled(tf.Price()), decimal.NewFromScaled(tf.Qty()),
		time.Unix(0, int64(ev.Ts())).UTC(), true
}

// ---------------------------------------------------------------------------
// trades-stream consumer (optional binding — orchestrator may call
// OnFill directly instead)
// ---------------------------------------------------------------------------

// TCAOrderSource resolves the leg's own-order context (receipt stamp +
// limit price). orders.created_at is the gateway receipt stamp used as
// the arrival reference.
type TCAOrderSource interface {
	OrderLeg(ctx context.Context, orderID int64) (receivedAt time.Time,
		limit *decimal.Decimal, err error)
}

// TCASymbolSource resolves instrument_id → venue symbol.
type TCASymbolSource interface {
	Symbol(ctx context.Context, instrumentID int64) (string, error)
}

// TCAFillConsumer decodes `trades.{shard}.{symbol}` frames, resolves
// counterparties through settlement.TradeResolver, and calls OnFill per
// leg — the same at-least-once contract as reporting's
// ConfirmationConsumer (NAK → redeliver; ReplacingMergeTree `ver` makes
// replays converge).
type TCAFillConsumer struct {
	Engine   *TCAEngine
	Resolver settlement.TradeResolver
	Orders   TCAOrderSource
	Symbols  TCASymbolSource
}

// NewTCAFillConsumer wires the consumer — all seams required.
func NewTCAFillConsumer(e *TCAEngine, res settlement.TradeResolver,
	orders TCAOrderSource, syms TCASymbolSource) (*TCAFillConsumer, error) {
	if e == nil || res == nil || orders == nil || syms == nil {
		return nil, fmt.Errorf("tca: fill consumer requires engine+resolver+orders+symbols")
	}
	return &TCAFillConsumer{Engine: e, Resolver: res, Orders: orders, Symbols: syms}, nil
}

// HandleMsg processes one trades message; nil → ACK, error → NAK.
func (c *TCAFillConsumer) HandleMsg(ctx context.Context, m Msg) error {
	tradeID, buyID, sellID, price, qty, ts, ok := decodeTCAFillPayload(m.Data())
	if !ok {
		return m.Ack() // not a fill / poison frame — ack away
	}
	fill := settlement.EngineFill{
		TradeID: tradeID, BuyOrderID: buyID, SellOrderID: sellID,
		Price: price, Qty: qty,
	}
	rt, err := c.Resolver.Resolve(ctx, fill)
	if err != nil {
		_ = m.Nak()
		return fmt.Errorf("tca consumer: resolve trade %d: %w", tradeID, err)
	}
	symbol, err := c.Symbols.Symbol(ctx, rt.InstrumentID)
	if err != nil {
		_ = m.Nak()
		return fmt.Errorf("tca consumer: symbol instrument %d: %w", rt.InstrumentID, err)
	}
	legs := []struct {
		orderID   uint64
		accountID int64
		side      string
	}{
		{buyID, rt.BuyerAccountID, "BUY"},
		{sellID, rt.SellerAccountID, "SELL"},
	}
	for _, leg := range legs {
		if leg.orderID > math.MaxInt64 || leg.accountID <= 0 {
			continue
		}
		recAt, limit, err := c.Orders.OrderLeg(ctx, int64(leg.orderID))
		if err != nil {
			_ = m.Nak()
			return fmt.Errorf("tca consumer: order leg %d: %w", leg.orderID, err)
		}
		if _, err := c.Engine.OnFill(ctx, TCAFill{
			FillID: int64(tradeID), OrderID: int64(leg.orderID),
			AccountID: leg.accountID, InstrumentID: rt.InstrumentID,
			Symbol: symbol, Side: leg.side, ExecPrice: price, Qty: qty,
			LimitPrice: limit, ReceivedAt: recAt, ExecutedAt: ts,
		}); err != nil {
			_ = m.Nak()
			return err
		}
	}
	return m.Ack()
}

// ---------------------------------------------------------------------------
// PG implementations
// ---------------------------------------------------------------------------

// PgTCAOrderSource reads orders.
type PgTCAOrderSource struct {
	Pool *pgxpool.Pool
}

// NewPgTCAOrderSource wraps a pool.
func NewPgTCAOrderSource(pool *pgxpool.Pool) *PgTCAOrderSource {
	return &PgTCAOrderSource{Pool: pool}
}

// OrderLeg implements TCAOrderSource — absent order returns zero time /
// nil limit (engine-internal fills lack gateway rows).
func (s *PgTCAOrderSource) OrderLeg(ctx context.Context, orderID int64) (time.Time, *decimal.Decimal, error) {
	var createdAt time.Time
	var price *decimal.Decimal
	err := s.Pool.QueryRow(ctx,
		`SELECT created_at, price::text FROM orders WHERE id = $1`,
		orderID).Scan(&createdAt, &price)
	if errors.Is(err, pgx.ErrNoRows) {
		return time.Time{}, nil, nil
	}
	if err != nil {
		return time.Time{}, nil, fmt.Errorf("tca: order leg %d: %w", orderID, err)
	}
	return createdAt, price, nil
}

// PgTCASymbolSource reads instruments.
type PgTCASymbolSource struct {
	Pool *pgxpool.Pool
}

// NewPgTCASymbolSource wraps a pool.
func NewPgTCASymbolSource(pool *pgxpool.Pool) *PgTCASymbolSource {
	return &PgTCASymbolSource{Pool: pool}
}

// Symbol implements TCASymbolSource — absent instruments resolve to a
// stable synthetic label (the fill happened; the record must not drop).
func (s *PgTCASymbolSource) Symbol(ctx context.Context, instrumentID int64) (string, error) {
	var sym string
	err := s.Pool.QueryRow(ctx,
		`SELECT symbol FROM instruments WHERE id = $1`, instrumentID).Scan(&sym)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Sprintf("INSTR-%d", instrumentID), nil
	}
	if err != nil {
		return "", fmt.Errorf("tca: instrument %d: %w", instrumentID, err)
	}
	return sym, nil
}
