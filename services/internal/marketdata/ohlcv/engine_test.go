package ohlcv

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"exchange/pkg/decimal"
)

// ---------------------------------------------------------------------------
// test doubles for the engine's boundary interfaces (real implementations
// of Source/Emitter/CandleStore/ArchiveSink recording what they received)
// ---------------------------------------------------------------------------

type chanSource struct{ ch chan TradeEvent }

func (s chanSource) Events(context.Context) (<-chan TradeEvent, error) { return s.ch, nil }

type recEmitter struct{ frames []KlineFrame }

func (r *recEmitter) Emit(_ context.Context, f KlineFrame) error {
	r.frames = append(r.frames, f)
	return nil
}

type recStore struct{ saved []Candle }

func (s *recStore) Save(_ context.Context, c Candle) error {
	s.saved = append(s.saved, c)
	return nil
}

func (s *recStore) closed() []Candle {
	var out []Candle
	for _, c := range s.saved {
		if c.Closed {
			out = append(out, c)
		}
	}
	return out
}

type recArchive struct{ got []Candle }

func (a *recArchive) Archive(_ context.Context, c Candle) error {
	a.got = append(a.got, c)
	return nil
}

// clock is a manually advanced time source.
type clock struct{ t time.Time }

func (c *clock) now() time.Time      { return c.t }
func (c *clock) set(t time.Time)     { c.t = t }
func (c *clock) add(d time.Duration) { c.t = c.t.Add(d) }

func dec(s string) decimal.Decimal { return decimal.MustFromString(s) }

func trade(symbol, price, qty string, side Side, seq uint64, ts time.Time) TradeEvent {
	return TradeEvent{
		TradeID:   seq*10 + 1,
		Symbol:    symbol,
		Price:     dec(price),
		Quantity:  dec(qty),
		TakerSide: side,
		Seq:       seq,
		Ts:        ts,
	}
}

func newTestEngine(clk *clock, store *recStore, em *recEmitter, ar *recArchive,
	intervals []Interval) (*Engine, *[]LateTrade) {
	var lates []LateTrade
	cfg := Config{
		Intervals:    intervals,
		GracePeriod:  10 * time.Second,
		EmitThrottle: 500 * time.Millisecond,
		Now:          clk.now,
		OnReconcile:  func(l LateTrade) { lates = append(lates, l) },
	}
	deps := Deps{}
	if store != nil {
		deps.Store = store
	}
	if em != nil {
		deps.Emitter = em
	}
	if ar != nil {
		deps.Archive = ar
	}
	return NewEngine(cfg, deps), &lates
}

var t0 = time.Date(2026, 1, 5, 10, 0, 0, 0, time.UTC) // Monday

// ---------------------------------------------------------------------------
// aggregation math
// ---------------------------------------------------------------------------

func TestAggregationMathAcrossIntervals(t *testing.T) {
	clk := &clock{t: t0}
	store, em, ar := &recStore{}, &recEmitter{}, &recArchive{}
	e, _ := newTestEngine(clk, store, em, ar, CanonicalIntervals)
	ctx := context.Background()

	// Three 1m-bucket trades at 10:00; also lands in every wider bucket.
	e.HandleTrade(ctx, trade("EUR/USD", "1.10000", "100", SideBuy, 1, t0.Add(1*time.Second)))
	e.HandleTrade(ctx, trade("EUR/USD", "1.12000", "50", SideSell, 2, t0.Add(30*time.Second)))
	e.HandleTrade(ctx, trade("EUR/USD", "1.11000", "25", SideBuy, 3, t0.Add(59*time.Second)))

	// Open candle state before close.
	oc := e.OpenCandle("EUR/USD", I1m, t0.Add(30*time.Second))
	if oc == nil || oc.TradeCount != 3 {
		t.Fatalf("open candle=%+v", oc)
	}
	if !oc.Open.Equal(dec("1.1")) || !oc.High.Equal(dec("1.12")) ||
		!oc.Low.Equal(dec("1.1")) || !oc.Close.Equal(dec("1.11")) {
		t.Fatalf("ohlc=%s %s %s %s", oc.Open, oc.High, oc.Low, oc.Close)
	}
	if !oc.Volume.Equal(dec("175")) {
		t.Fatalf("volume=%s", oc.Volume)
	}
	// quote volume = 1.10*100 + 1.12*50 + 1.11*25 = 110 + 56 + 27.75
	if !oc.QuoteVolume.Equal(dec("193.75")) {
		t.Fatalf("quote_volume=%s", oc.QuoteVolume)
	}
	if !oc.TakerBuyVolume.Equal(dec("125")) {
		t.Fatalf("taker_buy=%s", oc.TakerBuyVolume)
	}
	if oc.FirstSeq != 1 || oc.LastSeq != 3 {
		t.Fatalf("seq lineage %d..%d", oc.FirstSeq, oc.LastSeq)
	}

	// Close everything: advance past bucket end + grace.
	e.Advance(ctx, t0.Add(2*time.Hour))

	var m1 *Candle
	for i := range store.saved {
		c := &store.saved[i]
		if c.Interval == I1m && c.OpenTime.Equal(t0) {
			m1 = c
		}
	}
	if m1 == nil || !m1.Closed {
		t.Fatalf("closed 1m candle missing: %+v", store.saved)
	}
	if !m1.Open.Equal(dec("1.1")) || !m1.Close.Equal(dec("1.11")) ||
		!m1.Volume.Equal(dec("175")) || m1.TradeCount != 3 {
		t.Fatalf("closed candle=%+v", m1)
	}
	// Wider intervals got the same trades.
	var h1 *Candle
	for i := range store.saved {
		if store.saved[i].Interval == I1h {
			h1 = &store.saved[i]
		}
	}
	if h1 == nil || !h1.Close.Equal(dec("1.11")) || h1.TradeCount != 3 {
		t.Fatalf("1h candle=%+v", h1)
	}
	if e.Metrics().TradesAggregated == 0 || e.Metrics().CandlesClosed == 0 {
		t.Fatalf("metrics=%+v", e.Metrics())
	}
	// Archive saw only persisted intervals, all closed.
	if len(ar.got) != 0 {
		for _, c := range ar.got {
			if !c.Closed || c.Interval == I1s {
				t.Fatalf("archived non-closed/1s candle: %+v", c)
			}
		}
	}
}

func TestCanonicalSetCompletenessEndToEnd(t *testing.T) {
	clk := &clock{t: t0}
	store, em, ar := &recStore{}, &recEmitter{}, &recArchive{}
	e, _ := newTestEngine(clk, store, em, ar, nil) // nil → all 13
	ctx := context.Background()

	e.HandleTrade(ctx, trade("EUR/USD", "1.10000", "100", SideBuy, 1, t0.Add(time.Second)))
	for _, iv := range CanonicalIntervals {
		// Query inside the bucket that holds the trade (10:00:01).
		if e.OpenCandle("EUR/USD", iv, t0.Add(time.Second)) == nil {
			t.Fatalf("no open candle for %s", iv)
		}
	}
	// Advance far enough to close every interval (1M ends Feb 1).
	e.Advance(ctx, time.Date(2026, 2, 2, 0, 0, 0, 0, time.UTC))
	persisted := map[string]bool{}
	for _, c := range store.saved {
		persisted[c.Interval.String()] = true
	}
	for _, iv := range PersistedIntervals {
		if !persisted[iv.String()] {
			t.Fatalf("interval %s never persisted", iv)
		}
	}
	// 1s persisted nowhere: store rows are all non-1s.
	for _, c := range store.saved {
		if c.Interval == I1s {
			t.Fatal("1s candle persisted — must be memory-only")
		}
	}
	if got := len(e.Snapshot1s("EUR/USD")); got != 1 {
		t.Fatalf("1s ring=%d, want 1 real bar", got)
	}
	// 13 channels emitted (one frame per interval minimum).
	seen := map[string]bool{}
	for _, f := range em.frames {
		seen[f.Channel] = true
	}
	for _, iv := range CanonicalIntervals {
		if !seen[ChannelName("EUR/USD", iv)] {
			t.Fatalf("no frame on channel %s", ChannelName("EUR/USD", iv))
		}
	}
}

// ---------------------------------------------------------------------------
// boundaries, lateness, immutability
// ---------------------------------------------------------------------------

func TestBoundaryTradeOpensNewBucket(t *testing.T) {
	clk := &clock{t: t0}
	store, em := &recStore{}, &recEmitter{}
	e, _ := newTestEngine(clk, store, em, nil, []Interval{I1m})
	ctx := context.Background()

	e.HandleTrade(ctx, trade("EUR/USD", "1.10000", "100", SideBuy, 1, t0.Add(59*time.Second)))
	// Exactly on the edge → belongs to the 10:01 bucket, never the 10:00 bar.
	e.HandleTrade(ctx, trade("EUR/USD", "1.20000", "10", SideBuy, 2, t0.Add(time.Minute)))
	e.Advance(ctx, t0.Add(2*time.Minute).Add(11*time.Second))

	closed := store.closed()
	if len(closed) != 2 {
		t.Fatalf("closed=%d, want 2: %+v", len(closed), closed)
	}
	first, second := closed[0], closed[1]
	if !first.OpenTime.Equal(t0) || !first.Close.Equal(dec("1.1")) || first.TradeCount != 1 {
		t.Fatalf("first bar=%+v", first)
	}
	if !second.OpenTime.Equal(t0.Add(time.Minute)) ||
		!second.Open.Equal(dec("1.2")) || !second.Close.Equal(dec("1.2")) {
		t.Fatalf("second bar=%+v", second)
	}
}

func TestLateTradeGraceThenClosedImmutability(t *testing.T) {
	clk := &clock{t: t0}
	store, em := &recStore{}, &recEmitter{}
	var lates []LateTrade
	e, lp := newTestEngine(clk, store, em, nil, []Interval{I1m})
	lates = *lp
	_ = lates
	ctx := context.Background()

	// Bucket A = [10:00,10:01). A trade in bucket B moves the watermark
	// forward but A stays open inside the 10s grace (A closes at 10:01:10).
	e.HandleTrade(ctx, trade("EUR/USD", "1.10000", "100", SideBuy, 1, t0.Add(20*time.Second)))
	e.HandleTrade(ctx, trade("EUR/USD", "1.30000", "10", SideBuy, 2, t0.Add(65*time.Second))) // B bucket, wm=10:01:05

	// Late trade stamped inside A (arrives at wm 10:01:05; A end+grace =
	// 10:01:10 → still open) → must update A's candle.
	e.HandleTrade(ctx, trade("EUR/USD", "1.25000", "5", SideSell, 3, t0.Add(45*time.Second)))
	oc := e.OpenCandle("EUR/USD", I1m, t0.Add(45*time.Second))
	if oc == nil || oc.TradeCount != 2 || !oc.High.Equal(dec("1.25")) {
		t.Fatalf("in-grace late trade not applied: %+v", oc)
	}
	if m := e.Metrics(); m.LateTrades != 0 {
		t.Fatalf("premature reconcile: %+v", m)
	}

	// Advance past A's grace → A finalizes.
	e.Advance(ctx, t0.Add(71*time.Second)) // 10:01:11
	closed := store.closed()
	if len(closed) != 1 || !closed[0].OpenTime.Equal(t0) || closed[0].TradeCount != 2 {
		t.Fatalf("closed=%+v", closed)
	}

	// Same-bucket trade after close → reconciliation, bar untouched.
	e.HandleTrade(ctx, trade("EUR/USD", "9.99000", "1", SideBuy, 4, t0.Add(50*time.Second)))
	if m := e.Metrics(); m.LateTrades != 1 {
		t.Fatalf("LateTrades=%d, want 1", m.LateTrades)
	}
	if len(*lp) != 1 || (*lp)[0].Reason != "bucket_closed" ||
		(*lp)[0].Interval != I1m || !(*lp)[0].Bucket.Equal(t0) {
		t.Fatalf("reconcile hook=%+v", *lp)
	}
	// Closed candle immutable: no new save, values unchanged.
	if got := store.closed(); len(got) != 1 || !got[0].Close.Equal(dec("1.25")) {
		t.Fatalf("closed bar mutated: %+v", got)
	}
}

func TestOutOfOrderSeqCountedNotDropped(t *testing.T) {
	clk := &clock{t: t0}
	store := &recStore{}
	e, _ := newTestEngine(clk, store, nil, nil, []Interval{I1m})
	ctx := context.Background()

	e.HandleTrade(ctx, trade("EUR/USD", "1.10000", "1", SideBuy, 5, t0.Add(10*time.Second)))
	e.HandleTrade(ctx, trade("EUR/USD", "1.20000", "1", SideBuy, 3, t0.Add(20*time.Second))) // seq regression
	if m := e.Metrics(); m.OutOfOrderTrades != 1 {
		t.Fatalf("OutOfOrder=%d", m.OutOfOrderTrades)
	}
	oc := e.OpenCandle("EUR/USD", I1m, t0.Add(30*time.Second))
	if oc == nil || oc.TradeCount != 2 {
		t.Fatalf("out-of-order trade dropped: %+v", oc)
	}
}

// ---------------------------------------------------------------------------
// gap candles
// ---------------------------------------------------------------------------

func TestGapCandlesCarryForward(t *testing.T) {
	clk := &clock{t: t0}
	store := &recStore{}
	e, _ := newTestEngine(clk, store, nil, nil, []Interval{I1m})
	ctx := context.Background()

	e.HandleTrade(ctx, trade("EUR/USD", "1.11000", "10", SideBuy, 1, t0.Add(10*time.Second)))
	// Next trade at 10:05:30 → buckets 10:01..10:04 had no trades.
	e.HandleTrade(ctx, trade("EUR/USD", "1.50000", "5", SideBuy, 2, t0.Add(5*time.Minute+30*time.Second)))

	closed := store.closed()
	if len(closed) != 5 {
		t.Fatalf("closed=%d, want 5 (1 real + 4 gap): %+v", len(closed), closed)
	}
	real, gaps := 0, 0
	for _, c := range closed {
		if c.CarryForward {
			gaps++
			if !c.Open.Equal(dec("1.11")) || !c.High.Equal(dec("1.11")) ||
				!c.Low.Equal(dec("1.11")) || !c.Close.Equal(dec("1.11")) ||
				!c.Volume.IsZero() || c.TradeCount != 0 {
				t.Fatalf("bad gap candle: %+v", c)
			}
		} else {
			real++
		}
	}
	if real != 1 || gaps != 4 {
		t.Fatalf("real=%d gaps=%d", real, gaps)
	}
	if m := e.Metrics(); m.GapCandles != 4 {
		t.Fatalf("GapCandles=%d", m.GapCandles)
	}
}

func TestGapBeforeFirstCloseEmitsNothing(t *testing.T) {
	// First candle ever: no previous close to carry — empty buckets before
	// the first trade finalize silently rather than fabricating prices.
	clk := &clock{t: t0}
	store := &recStore{}
	e, _ := newTestEngine(clk, store, nil, nil, []Interval{I1m})
	ctx := context.Background()

	// Watermark exists only via Advance before any trade.
	e.Advance(ctx, t0.Add(10*time.Minute))
	e.HandleTrade(ctx, trade("EUR/USD", "1.10000", "1", SideBuy, 1, t0.Add(10*time.Minute+5*time.Second)))
	e.Advance(ctx, t0.Add(12*time.Minute))

	if got := len(store.closed()); got != 1 {
		t.Fatalf("closed=%d, want exactly the real bar (no fabricated history)", got)
	}
}

// ---------------------------------------------------------------------------
// emission contract
// ---------------------------------------------------------------------------

func TestEmissionPayloadShapeAndSeq(t *testing.T) {
	clk := &clock{t: t0}
	em := &recEmitter{}
	e, _ := newTestEngine(clk, nil, em, nil, []Interval{I1m})
	ctx := context.Background()

	e.HandleTrade(ctx, trade("EUR/USD", "1.10000", "100", SideBuy, 1, t0.Add(5*time.Second)))
	if len(em.frames) != 1 {
		t.Fatalf("frames=%d", len(em.frames))
	}
	raw, err := json.Marshal(em.frames[0])
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	// Envelope follows the ws eventFrame convention (§10.5).
	if m["type"] != "event" || m["channel"] != "kline@EUR/USD_1m" ||
		m["seq"] != float64(1) {
		t.Fatalf("envelope=%v", m)
	}
	if _, ok := m["ts_ms"]; !ok {
		t.Fatal("missing ts_ms")
	}
	d := m["data"].(map[string]any)
	for _, k := range []string{"symbol", "timeframe", "open_time_ms",
		"close_time_ms", "open", "high", "low", "close", "volume",
		"quote_volume", "taker_buy_volume", "trade_count", "closed"} {
		if _, ok := d[k]; !ok {
			t.Fatalf("data missing %q: %v", k, d)
		}
	}
	if d["open"] != "1.10000000" || d["closed"] != false ||
		d["timeframe"] != "1m" {
		t.Fatalf("data=%v", d)
	}
	if d["open_time_ms"] != float64(t0.UnixMilli()) ||
		d["close_time_ms"] != float64(t0.Add(time.Minute).UnixMilli()) {
		t.Fatalf("times=%v", d)
	}

	// Close emits immediately with seq=2 on the same channel.
	e.Advance(ctx, t0.Add(2*time.Minute))
	if len(em.frames) != 2 {
		t.Fatalf("frames=%d after close", len(em.frames))
	}
	f2 := em.frames[1]
	if f2.Seq != 2 || f2.Channel != "kline@EUR/USD_1m" ||
		!f2.Data.Closed || f2.Data.Close != "1.10000000" {
		t.Fatalf("closing frame=%+v", f2)
	}
}

func TestEmitThrottleCoalescesOpenUpdates(t *testing.T) {
	clk := &clock{t: t0}
	em := &recEmitter{}
	e, _ := newTestEngine(clk, nil, em, nil, []Interval{I1m})
	ctx := context.Background()

	// Three trades inside one 500ms window → single open frame.
	e.HandleTrade(ctx, trade("EUR/USD", "1.10", "1", SideBuy, 1, t0.Add(time.Second)))
	e.HandleTrade(ctx, trade("EUR/USD", "1.20", "1", SideBuy, 2, t0.Add(2*time.Second)))
	e.HandleTrade(ctx, trade("EUR/USD", "1.30", "1", SideBuy, 3, t0.Add(3*time.Second)))
	if len(em.frames) != 1 {
		t.Fatalf("throttle: frames=%d, want 1", len(em.frames))
	}
	// Once the window expires, the NEXT Advance flushes the latest state.
	clk.add(600 * time.Millisecond)
	e.Advance(ctx, clk.now())
	if len(em.frames) != 2 {
		t.Fatalf("pending flush: frames=%d", len(em.frames))
	}
	if em.frames[1].Data.Close != "1.30000000" {
		t.Fatalf("flushed frame should carry latest close: %+v", em.frames[1].Data)
	}
	if em.frames[1].Seq != 2 {
		t.Fatalf("seq=%d", em.frames[1].Seq)
	}
	// Closed bars bypass the throttle entirely.
	e.HandleTrade(ctx, trade("EUR/USD", "1.40", "1", SideBuy, 4, t0.Add(61*time.Second)))
	e.Advance(ctx, t0.Add(2*time.Minute))
	var last KlineFrame
	for _, f := range em.frames {
		if f.Data.Closed {
			last = f
		}
	}
	if !last.Data.Closed {
		t.Fatal("no closing frame emitted")
	}
}

// ---------------------------------------------------------------------------
// 1s ring buffer (memory-only interval)
// ---------------------------------------------------------------------------

func TestRing1sMemoryOnly(t *testing.T) {
	clk := &clock{t: t0}
	store, ar := &recStore{}, &recArchive{}
	e, _ := newTestEngine(clk, store, nil, ar, []Interval{I1s})
	ctx := context.Background()

	for i := 0; i < 70; i++ {
		e.HandleTrade(ctx, trade("EUR/USD", "1.10", "1", SideBuy,
			uint64(i+1), t0.Add(time.Duration(i)*time.Second)))
	}
	e.Advance(ctx, t0.Add(5*time.Minute)) // finalize all 70 buckets
	if len(store.saved) != 0 || len(ar.got) != 0 {
		t.Fatalf("1s must never persist: saved=%d archived=%d",
			len(store.saved), len(ar.got))
	}
	ring := e.Snapshot1s("EUR/USD")
	if len(ring) != 60 {
		t.Fatalf("ring=%d, want cap 60", len(ring))
	}
	// Oldest retained bar is bucket 10 (70 closed − 60 kept).
	if !ring[0].OpenTime.Equal(t0.Add(10 * time.Second)) {
		t.Fatalf("ring head=%s", ring[0].OpenTime)
	}
}

// ---------------------------------------------------------------------------
// Run loop smoke test over a real Source
// ---------------------------------------------------------------------------

func TestRunConsumesSourceAndStops(t *testing.T) {
	clk := &clock{t: t0}
	store := &recStore{}
	e, _ := newTestEngine(clk, store, nil, nil, []Interval{I1m})
	src := chanSource{ch: make(chan TradeEvent, 2)}
	src.ch <- trade("EUR/USD", "1.10", "1", SideBuy, 1, t0.Add(time.Second))
	src.ch <- trade("EUR/USD", "1.20", "1", SideSell, 2, t0.Add(90*time.Second))
	close(src.ch)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := e.Run(ctx, src); err != nil {
		t.Fatalf("Run: %v", err)
	}
	oc := e.OpenCandle("EUR/USD", I1m, t0.Add(90*time.Second))
	if oc == nil || oc.TradeCount != 1 {
		t.Fatalf("run did not aggregate: %+v", oc)
	}
}
