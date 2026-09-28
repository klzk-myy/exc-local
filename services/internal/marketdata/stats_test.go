package marketdata

import (
	"context"
	"testing"
	"time"

	"exchange/pkg/decimal"
)

// TestStats_AllMarketFrames — Task 6.3.20 / §24 #291: stats@all and
// miniTicker@all emit per tick with per-symbol rolling stats.
func TestStats_AllMarketFrames(t *testing.T) {
	em := &emitter{}
	p := NewStatsProducer(StatsProducerConfig{Tick: 25 * time.Millisecond},
		nil, em.fn())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.Run(ctx)

	p.Push(TradeEvent{TradeID: 1, Symbol: testSym,
		Price: mustDec(t, "1.10"), Quantity: decimal.NewFromInt(10),
		Ts: time.Now()})
	p.Push(TradeEvent{TradeID: 2, Symbol: testSym,
		Price: mustDec(t, "1.20"), Quantity: decimal.NewFromInt(5),
		Ts: time.Now()})

	if em.waitFor(4, 2*time.Second) < 4 {
		t.Fatal("stats frames missing")
	}
	stats := forChannel(em.all(), "stats@all")
	mini := forChannel(em.all(), "miniTicker@all")
	if len(stats) == 0 || len(mini) == 0 {
		t.Fatalf("stats=%d mini=%d", len(stats), len(mini))
	}

	m := payload(t, stats[0])
	if m["event"] != "marketStats" {
		t.Fatalf("event=%v", m["event"])
	}
	syms, ok := m["symbols"].([]any)
	if !ok || len(syms) == 0 {
		t.Fatalf("symbols empty: %v", m)
	}
	s0 := syms[0].(map[string]any)
	if s0["symbol"] != testSym {
		t.Fatalf("symbol=%v", s0)
	}
	if s0["last_price"] != "1.2" {
		t.Fatalf("last_price=%v", s0["last_price"])
	}
	wins := s0["windows"].(map[string]any)
	d1, ok := wins["1d"].(map[string]any)
	if !ok {
		t.Fatalf("no 1d window: %v", wins)
	}
	if d1["volume"] != "15" || d1["trade_count"] != float64(2) {
		t.Fatalf("1d window wrong: %v", d1)
	}
	if d1["window_ms"] != float64(86400000) {
		t.Fatalf("window_ms=%v", d1["window_ms"])
	}

	mm := payload(t, mini[0])
	if mm["event"] != "miniTicker" {
		t.Fatalf("event=%v", mm["event"])
	}
	ms0 := mm["symbols"].([]any)[0].(map[string]any)
	if ms0["symbol"] != testSym || ms0["volume"] != "15" {
		t.Fatalf("miniTicker entry wrong: %v", ms0)
	}
	if ms0["high"] != "1.2" || ms0["low"] != "1.1" || ms0["close"] != "1.2" {
		t.Fatalf("miniTicker ohlc wrong: %v", ms0)
	}
}

// TestBlockTape_DelayedAndAnonymous — block prints publish only after
// the deferral delay, anonymously (no side/taker fields), with the
// delay_ms transparency field.
func TestBlockTape_DelayedAndAnonymous(t *testing.T) {
	em := &emitter{}
	p := NewBlockTapeProducer(BlockTapeProducerConfig{
		Delay:        60 * time.Millisecond,
		ThresholdUSD: decimal.NewFromInt(1_000_000),
	}, nil, em.fn())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.Run(ctx)

	p.Push(TradeEvent{TradeID: 9001, Symbol: "EUR/USD",
		Price: mustDec(t, "1.10"), Quantity: mustDec(t, "1000000"),
		Ts: time.Now()}) // notional = $1.1m ≥ threshold

	// Invisible before the delay.
	time.Sleep(20 * time.Millisecond)
	if n := len(em.all()); n != 0 {
		t.Fatalf("block print leaked early: %d frames", n)
	}
	if em.waitFor(1, 2*time.Second) != 1 {
		t.Fatal("block print never published")
	}
	e := em.all()[0]
	if e.Channel != "blockTrades@EUR/USD" {
		t.Fatalf("channel=%q", e.Channel)
	}
	m := payload(t, e)
	if m["event"] != "blockTrade" {
		t.Fatalf("event=%v", m)
	}
	if m["notional_usd"] != "1100000" {
		t.Fatalf("notional=%v", m["notional_usd"])
	}
	if m["delay_ms"] != float64(60) {
		t.Fatalf("delay_ms=%v", m["delay_ms"])
	}
	for _, banned := range []string{"side", "is_buyer_maker", "taker_order_id",
		"maker_order_id", "trade_id"} {
		if _, ok := m[banned]; ok {
			t.Fatalf("anonymity violated: %q present", banned)
		}
	}
	flags := m["venue_flags"].([]any)
	if len(flags) == 0 || flags[0] != "DEFERRED_PUBLICATION" {
		t.Fatalf("venue_flags=%v", flags)
	}
}

// TestBlockTape_BelowThresholdIgnored — a $999k EUR/USD print is not a
// block (threshold is USD notional via the direct resolver).
func TestBlockTape_BelowThresholdIgnored(t *testing.T) {
	em := &emitter{}
	p := NewBlockTapeProducer(BlockTapeProducerConfig{
		Delay:        30 * time.Millisecond,
		ThresholdUSD: decimal.NewFromInt(1_000_000),
	}, nil, em.fn())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.Run(ctx)

	p.Push(TradeEvent{TradeID: 1, Symbol: "EUR/USD",
		Price: mustDec(t, "1.10"), Quantity: mustDec(t, "900000"),
		Ts: time.Now()}) // $990k — under the bar
	time.Sleep(150 * time.Millisecond)
	if n := len(em.all()); n != 0 {
		t.Fatalf("sub-threshold trade published: %d frames", n)
	}
}

// TestBlockTape_NonUSDUnresolved — a pair with no USD leg returns
// ok=false from DirectUSDNotional: it must NOT be published through a
// guessed conversion.
func TestBlockTape_NonUSDUnresolved(t *testing.T) {
	em := &emitter{}
	p := NewBlockTapeProducer(BlockTapeProducerConfig{
		Delay: 30 * time.Millisecond,
	}, nil, em.fn())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.Run(ctx)

	p.Push(TradeEvent{TradeID: 1, Symbol: "EUR/GBP",
		Price: mustDec(t, "0.85"), Quantity: mustDec(t, "9000000"),
		Ts: time.Now()})
	time.Sleep(150 * time.Millisecond)
	if n := len(em.all()); n != 0 {
		t.Fatalf("non-USD block published via fabricated rate")
	}
}

// TestBlockTape_CorrectionPrePublication — a bust of a still-queued
// block cancels it silently: nothing ever reaches the public tape.
func TestBlockTape_CorrectionPrePublication(t *testing.T) {
	em := &emitter{}
	p := NewBlockTapeProducer(BlockTapeProducerConfig{
		Delay: 100 * time.Millisecond,
	}, nil, em.fn())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.Run(ctx)

	p.Push(TradeEvent{TradeID: 55, Symbol: "USD/JPY",
		Price: mustDec(t, "150"), Quantity: mustDec(t, "2000000"),
		Ts: time.Now()}) // USD-base → notional = qty = $2m
	p.PushCorrection(55, "BUST", "", "", time.Now())

	time.Sleep(200 * time.Millisecond)
	if n := len(em.all()); n != 0 {
		t.Fatalf("busted block published: %d frames", n)
	}
}

// TestBlockTape_CorrectionPostPublication — busting a published print
// emits a linked correction on the ORIGINAL channel.
func TestBlockTape_CorrectionPostPublication(t *testing.T) {
	em := &emitter{}
	p := NewBlockTapeProducer(BlockTapeProducerConfig{
		Delay: 30 * time.Millisecond,
	}, nil, em.fn())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.Run(ctx)

	p.Push(TradeEvent{TradeID: 77, Symbol: "USD/JPY",
		Price: mustDec(t, "150"), Quantity: mustDec(t, "2000000"),
		Ts: time.Now()})
	if em.waitFor(1, 2*time.Second) != 1 {
		t.Fatal("block print missing")
	}
	blockID := payload(t, em.all()[0])["block_trade_id"]

	p.PushCorrection(77, "CORRECTION", "149.9", "", time.Now())
	if em.waitFor(2, 2*time.Second) != 2 {
		t.Fatal("correction missing")
	}
	m := payload(t, em.all()[1])
	if m["event"] != "blockTradeCorrection" || m["kind"] != "CORRECTION" {
		t.Fatalf("correction wrong: %v", m)
	}
	if m["block_trade_id"] != blockID ||
		m["original_trade_id"] != float64(77) {
		t.Fatalf("correction linkage wrong: %v", m)
	}
	if m["corrected_price"] != "149.9" {
		t.Fatalf("corrected_price=%v", m["corrected_price"])
	}
}
