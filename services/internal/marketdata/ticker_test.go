package marketdata

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"exchange/pkg/decimal"
)

// tickerClock is a manually-advanced clock for the 24h-window math.
type tickerClock struct{ now atomic.Int64 } // unix nano

func (c *tickerClock) Now() time.Time          { return time.Unix(0, c.now.Load()) }
func (c *tickerClock) Advance(d time.Duration) { c.now.Add(int64(d)) }

// TestTicker_24hRollingStats — Task 6.3.4 / spec §10.1: the ticker emits
// rolling 24h OHLCV + price-change fields on its cadence.
func TestTicker_24hRollingStats(t *testing.T) {
	clock := &tickerClock{}
	clock.now.Store(time.Now().UnixNano())

	em := &emitter{}
	p := NewTickerProducer(TickerProducerConfig{
		Now: clock.Now, Tick: 25 * time.Millisecond,
	}, nil, em.fn())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.Run(ctx)

	base := clock.Now()
	// Three trades spanning the window: open 1.10, high 1.20, low 1.05,
	// close 1.15, volume 6.
	for i, px := range []string{"1.10", "1.20", "1.05", "1.15"} {
		p.Push(TradeEvent{TradeID: uint64(i + 1), Symbol: testSym,
			Price: mustDec(t, px), Quantity: mustDec(t, "1.5"),
			Ts: base.Add(time.Duration(i) * time.Second)})
	}

	if em.waitFor(1, 2*time.Second) != 1 {
		t.Fatal("no ticker frame")
	}
	m := payload(t, em.all()[0])
	if m["event"] != "ticker24h" || m["symbol"] != testSym {
		t.Fatalf("envelope: %v", m)
	}
	if m["open"] != "1.1" || m["high"] != "1.2" ||
		m["low"] != "1.05" || m["close"] != "1.15" {
		t.Fatalf("ohlc wrong: %v", m)
	}
	if m["volume"] != "6" {
		t.Fatalf("volume=%v, want 6 (4×1.5)", m["volume"])
	}
	if m["trade_count"] != float64(4) {
		t.Fatalf("count=%v", m["trade_count"])
	}
	if m["first_trade_id"] != float64(1) || m["last_trade_id"] != float64(4) {
		t.Fatalf("lineage: %v %v", m["first_trade_id"], m["last_trade_id"])
	}
	// price change = close − open = 0.05; pct = 0.05/1.10.
	chg := decField(t, m, "price_change")
	if !chg.Equal(mustDec(t, "0.05")) {
		t.Fatalf("price_change=%v", chg)
	}
	// pct = change/open × 100, rendered at 4dp ("4.5455").
	wantPct := mustDec(t, "0.05").Div(mustDec(t, "1.1")).
		Mul(decimal.NewFromInt(100))
	if m["price_change_pct"] != wantPct.StringFixed(4) {
		t.Fatalf("pct=%v want %v", m["price_change_pct"], wantPct)
	}
	if m["close_time_ms"].(float64) != float64(clock.Now().UnixMilli()) {
		t.Fatalf("close_time_ms=%v", m["close_time_ms"])
	}
}

// TestTicker_ExpiryEvictsOldTrades — a trade older than 24h must leave
// the window; the final frame after full expiry reports zero volume
// with the last close carried (tombstone frame).
func TestTicker_ExpiryEvictsOldTrades(t *testing.T) {
	clock := &tickerClock{}
	base := time.Now()
	clock.now.Store(base.UnixNano())

	em := &emitter{}
	p := NewTickerProducer(TickerProducerConfig{
		Now: clock.Now, Tick: 20 * time.Millisecond,
	}, nil, em.fn())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.Run(ctx)

	p.Push(TradeEvent{TradeID: 1, Symbol: testSym,
		Price: mustDec(t, "1.50"), Quantity: decimal.NewFromInt(4),
		Ts: base})

	if em.waitFor(1, 2*time.Second) != 1 {
		t.Fatal("no frame")
	}
	if m := payload(t, em.all()[0]); m["volume"] != "4" {
		t.Fatalf("pre-expiry volume=%v", m["volume"])
	}

	// Advance beyond the window: the trade expires; a tombstone frame
	// (zero volume, last close) must be emitted exactly once.
	clock.Advance(25 * time.Hour)
	before := len(em.all())
	deadline := time.Now().Add(2 * time.Second)
	for len(em.all()) == before && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	last := em.all()[len(em.all())-1]
	m := payload(t, last)
	if m["volume"] != "0" || m["trade_count"] != float64(0) {
		t.Fatalf("post-expiry frame not zeroed: %v", m)
	}
	if m["close"] != "1.5" {
		t.Fatalf("tombstone close=%v, want last close 1.5", m["close"])
	}
}

// TestTicker_CadenceOneSecond — default tick is 1s (spec §10.1) and
// emits repeat each tick while the window is non-empty.
func TestTicker_CadenceEmitsEachTick(t *testing.T) {
	em := &emitter{}
	p := NewTickerProducer(TickerProducerConfig{Tick: 30 * time.Millisecond},
		nil, em.fn())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.Run(ctx)

	p.Push(TradeEvent{TradeID: 1, Symbol: testSym,
		Price: decimal.NewFromInt(1), Quantity: decimal.NewFromInt(1),
		Ts: time.Now()})
	if got := em.waitFor(3, 2*time.Second); got < 3 {
		t.Fatalf("only %d tick frames", got)
	}
}
