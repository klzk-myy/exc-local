package ohlcv

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// Default tuning.
const (
	// DefaultGracePeriod is how long past bucket end a just-elapsed candle
	// stays open for late/out-of-order trades. Sized for JetStream
	// redelivery jitter, not for warehouse backfill.
	DefaultGracePeriod = 10 * time.Second
	// DefaultEmitThrottle bounds in-progress frame rate to 2/sec per
	// channel (Task 6.3.8 step 5 AC).
	DefaultEmitThrottle = 500 * time.Millisecond
	// DefaultAdvanceTick is the Run loop's wall-clock finalize cadence.
	DefaultAdvanceTick = time.Second
	// DefaultMaxGap caps carry-forward emission per finalize pass; a
	// corrupt far-future timestamp finalizes the buckets but cannot make
	// the engine emit an unbounded run of synthesized bars.
	DefaultMaxGap = 500
)

// Config tunes the engine. Zero fields take the defaults above.
type Config struct {
	// Intervals is the candle set; nil → CanonicalIntervals (all 13).
	Intervals []Interval
	// GracePeriod bounds late-trade acceptance past bucket close.
	GracePeriod time.Duration
	// EmitThrottle is the min spacing between in-progress emissions per
	// channel (and the open-bar persist cadence). Closed bars bypass it.
	EmitThrottle time.Duration
	// AdvanceTick is the Run loop's wall-clock finalize tick.
	AdvanceTick time.Duration
	// MaxGap caps synthesized gap-candle emission per finalize pass.
	MaxGap int
	// DisableOpenPersist opts out of writing in-progress bars to
	// CandleStore at the throttle cadence. Default keeps them on — the
	// migration 173 read model exposes the live bar (closed=false).
	DisableOpenPersist bool
	// Now is the clock (tests inject); nil → time.Now.
	Now func() time.Time
	// OnReconcile receives every late trade routed to reconciliation.
	OnReconcile func(LateTrade)
	// Logger; nil → slog.Default().
	Logger *slog.Logger
}

// Deps wires the engine's side-effect boundaries. All are optional except
// Source — which Run alone requires.
type Deps struct {
	Source  Source       // trade event stream (Run only)
	Store   CandleStore  // fx_klines writer; nil → no PG persistence
	Archive ArchiveSink  // §16.2 cold tier; nil → no archive writes
	Emitter Emitter      // kline@ frame sink; nil → no emission
	Seq     SeqAllocator // per-channel frame sequence; nil → in-memory
}

// Metrics is the engine's counter snapshot.
type Metrics struct {
	TradesReceived   uint64
	TradesAggregated uint64 // trades that landed in an open/new candle
	OutOfOrderTrades uint64 // seq regressions observed (still processed)
	LateTrades       uint64 // reconciliation counter — targeted closed buckets
	CandlesClosed    uint64 // finalized real bars
	GapCandles       uint64 // synthesized carry-forward bars emitted
	GapOverflow      uint64 // buckets finalized past the MaxGap emission cap
	FramesEmitted    uint64
	PersistErrors    uint64
	ArchiveErrors    uint64
	EmitErrors       uint64
}

// Engine drives aggregation: trade events in, persisted bars and emitted
// frames out. All state mutation flows through mu — HandleTrade/Advance
// are safe to call concurrently with Run's ticker.
type Engine struct {
	cfg  Config
	deps Deps
	agg  *aggregator
	log  *slog.Logger

	mu       sync.Mutex
	now      func() time.Time
	seq      map[string]uint64    // in-memory per-channel cursor (default SeqAllocator)
	lastSync map[string]time.Time // last open-bar sync per channel
	pending  map[string]Candle    // suppressed open-bar update per channel
	lastSeq  map[string]uint64    // per-symbol trade-seq cursor
	hasSeq   map[string]bool
	metrics  Metrics
}

// NewEngine builds the engine over cfg + deps.
func NewEngine(cfg Config, deps Deps) *Engine {
	intervals := cfg.Intervals
	if len(intervals) == 0 {
		intervals = CanonicalIntervals
	}
	grace := cfg.GracePeriod
	if grace <= 0 {
		grace = DefaultGracePeriod
	}
	throttle := cfg.EmitThrottle
	if throttle <= 0 {
		throttle = DefaultEmitThrottle
	}
	maxGap := cfg.MaxGap
	if maxGap <= 0 {
		maxGap = DefaultMaxGap
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	cfg.Intervals, cfg.GracePeriod, cfg.EmitThrottle, cfg.MaxGap, cfg.Now, cfg.Logger =
		intervals, grace, throttle, maxGap, now, log
	return &Engine{
		cfg:      cfg,
		deps:     deps,
		agg:      newAggregator(intervals, grace, maxGap),
		log:      log,
		now:      now,
		seq:      make(map[string]uint64),
		lastSync: make(map[string]time.Time),
		pending:  make(map[string]Candle),
		lastSeq:  make(map[string]uint64),
		hasSeq:   make(map[string]bool),
	}
}

// HandleTrade folds one trade into all configured intervals: updates or
// opens the bucket's candle, finalizes whatever the event-time move
// closes, emits the in-progress frame (throttled), and persists per the
// interval's tier. Safe for concurrent use.
func (e *Engine) HandleTrade(ctx context.Context, ev TradeEvent) {
	e.mu.Lock()
	defer e.mu.Unlock()

	e.metrics.TradesReceived++
	if ev.Ts.IsZero() {
		ev.Ts = e.now()
	}
	ev.Ts = ev.Ts.UTC()
	if ev.Symbol == "" || !ev.Price.IsPositive() || ev.Quantity.IsNegative() {
		e.log.Warn("dropping malformed trade event",
			"symbol", ev.Symbol, "trade_id", ev.TradeID, "seq", ev.Seq)
		return
	}
	if ev.Seq != 0 {
		if e.hasSeq[ev.Symbol] && ev.Seq <= e.lastSeq[ev.Symbol] {
			e.metrics.OutOfOrderTrades++
		} else {
			e.lastSeq[ev.Symbol] = ev.Seq
			e.hasSeq[ev.Symbol] = true
		}
	}

	res := e.agg.handle(ev)
	for i := range res.closed {
		e.onClosed(ctx, &res.closed[i])
	}
	if res.gapOverflow > 0 {
		e.metrics.GapOverflow += uint64(res.gapOverflow)
		e.log.Warn("gap-candle emission capped", "buckets_suppressed", res.gapOverflow)
	}
	aggregated := false
	for _, u := range res.updates {
		switch {
		case u.Late:
			// Counted per (trade, interval): a trade can be late for the
			// 1m bucket while still landing in its 1D bar.
			e.metrics.LateTrades++
			lt := LateTrade{Event: ev, Interval: u.Interval,
				Bucket: u.LateBucket, Reason: "bucket_closed"}
			e.log.Warn("late trade reconciled", "late", lt.String())
			if e.cfg.OnReconcile != nil {
				e.cfg.OnReconcile(lt)
			}
		case u.Candle != nil:
			aggregated = true
			e.emitOpen(ctx, u.Candle)
		}
	}
	if aggregated {
		e.metrics.TradesAggregated++
	}
}

// Advance moves every series' watermark to now, finalizing elapsed
// buckets, and flushes suppressed open-bar emissions whose throttle
// window has expired. Call periodically (Run does it on AdvanceTick) and
// from tests for deterministic closure.
func (e *Engine) Advance(ctx context.Context, now time.Time) {
	e.mu.Lock()
	defer e.mu.Unlock()

	res := e.agg.advance(now.UTC())
	for i := range res.closed {
		e.onClosed(ctx, &res.closed[i])
	}
	if res.gapOverflow > 0 {
		e.metrics.GapOverflow += uint64(res.gapOverflow)
		e.log.Warn("gap-candle emission capped", "buckets_suppressed", res.gapOverflow)
	}
	e.flushPending(ctx, now.UTC())
}

// Run consumes the source and finalizes on the wall clock until ctx
// ends. It returns ctx.Err() on shutdown.
func (e *Engine) Run(ctx context.Context, src Source) error {
	ch, err := src.Events(ctx)
	if err != nil {
		return err
	}
	tick := e.cfg.AdvanceTick
	if tick <= 0 {
		tick = DefaultAdvanceTick
	}
	t := time.NewTicker(tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case ev, ok := <-ch:
			if !ok {
				return nil
			}
			e.HandleTrade(ctx, ev)
		case now := <-t.C:
			e.Advance(ctx, now)
		}
	}
}

// Metrics returns a counter snapshot.
func (e *Engine) Metrics() Metrics {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.metrics
}

// OpenCandle returns a copy of the still-open candle for the bucket
// containing ts, or nil — used by WS snapshot/resume paths.
func (e *Engine) OpenCandle(symbol string, iv Interval, ts time.Time) *Candle {
	e.mu.Lock()
	defer e.mu.Unlock()
	c := e.agg.openCandle(symbol, iv, iv.Floor(ts))
	if c == nil {
		return nil
	}
	cp := *c
	return &cp
}

// Snapshot1s returns the retained closed 1s bars for symbol (oldest
// first, ≤60 — the Task 6.3.14 memory-only ring buffer).
func (e *Engine) Snapshot1s(symbol string) []Candle {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.agg.ring1s(symbol)
}

// ---------------------------------------------------------------------------
// internals (all called under e.mu)
// ---------------------------------------------------------------------------

// onClosed handles a finalized bar: persist (persisted intervals only),
// archive, emit the closing frame — always, bypassing the throttle.
func (e *Engine) onClosed(ctx context.Context, c *Candle) {
	if c.CarryForward {
		e.metrics.GapCandles++
	} else {
		e.metrics.CandlesClosed++
	}
	if c.Interval.Persisted() {
		if e.deps.Store != nil {
			if err := e.deps.Store.Save(ctx, *c); err != nil {
				e.metrics.PersistErrors++
				e.log.Error("kline persist failed", "err", err,
					"symbol", c.Symbol, "timeframe", c.Interval.String())
			}
		}
		if e.deps.Archive != nil {
			if err := e.deps.Archive.Archive(ctx, *c); err != nil {
				e.metrics.ArchiveErrors++
				e.log.Error("kline archive failed", "err", err,
					"symbol", c.Symbol, "timeframe", c.Interval.String())
			}
		}
	}
	ch := ChannelName(c.Symbol, c.Interval)
	delete(e.pending, ch)
	e.emitNow(ctx, ch, c)
}

// emitOpen syncs an in-progress update subject to the per-channel
// throttle; suppressed updates keep only the latest state in pending.
// The cadence governs BOTH the WS frame and the open-bar persist
// (§10.3 read-model freshness bounded to ≤2 writes/sec/channel) — it is
// tracked in lastSync, not inside emitNow, so a nil Emitter cannot turn
// persistence into a per-trade write.
func (e *Engine) emitOpen(ctx context.Context, c *Candle) {
	ch := ChannelName(c.Symbol, c.Interval)
	now := e.now()
	if last, ok := e.lastSync[ch]; ok && now.Sub(last) < e.cfg.EmitThrottle {
		e.pending[ch] = *c
		return
	}
	e.syncOpen(ctx, ch, c, now)
}

// flushPending emits the newest suppressed update for channels whose
// throttle window has expired.
func (e *Engine) flushPending(ctx context.Context, now time.Time) {
	for ch, c := range e.pending {
		if now.Sub(e.lastSync[ch]) < e.cfg.EmitThrottle {
			continue
		}
		cp := c
		e.syncOpen(ctx, ch, &cp, now)
	}
}

// syncOpen performs one open-bar sync: optional persist + emit + cadence
// bookkeeping.
func (e *Engine) syncOpen(ctx context.Context, ch string, c *Candle, now time.Time) {
	if !e.cfg.DisableOpenPersist && c.Interval.Persisted() && e.deps.Store != nil {
		if err := e.deps.Store.Save(ctx, *c); err != nil {
			e.metrics.PersistErrors++
			e.log.Error("kline open persist failed", "err", err,
				"symbol", c.Symbol, "timeframe", c.Interval.String())
		}
	}
	e.lastSync[ch] = now
	delete(e.pending, ch)
	e.emitNow(ctx, ch, c)
}

// emitNow stamps seq and ships one frame; emit failures are metrics, not
// halts (a stalled subscriber must never stop aggregation — §10.6).
func (e *Engine) emitNow(ctx context.Context, ch string, c *Candle) {
	if e.deps.Emitter == nil {
		return
	}
	var seq uint64
	if e.deps.Seq != nil {
		seq = e.deps.Seq(ch)
	} else {
		e.seq[ch]++
		seq = e.seq[ch]
	}
	frame := NewKlineFrame(ch, seq, c, e.now())
	if err := e.deps.Emitter.Emit(ctx, frame); err != nil {
		e.metrics.EmitErrors++
		e.log.Error("kline emit failed", "err", err, "channel", ch)
		return
	}
	e.metrics.FramesEmitted++
}
