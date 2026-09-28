package marketdata

import (
	"context"
	"testing"
	"time"
)

// TestBBO_EveryTopChange — Task 6.3.11 / §24 #261: every top-of-book
// change publishes immediately; the stream is never conflated.
func TestBBO_EveryTopChange(t *testing.T) {
	em := &emitter{}
	deltas := []BookDelta{
		{Symbol: testSym, EngineSeq: 1, Ts: time.UnixMilli(10),
			Bids: []Level{{Price: 1_10000000, Qty: 1_00000000, Count: 1}},
			Asks: []Level{{Price: 1_20000000, Qty: 1_00000000, Count: 1}}},
		{Symbol: testSym, EngineSeq: 2, Ts: time.UnixMilli(20),
			Bids: []Level{{Price: 1_10000001, Qty: 5_00000000, Count: 2}}, // bid moved
			Asks: []Level{{Price: 1_20000000, Qty: 1_00000000, Count: 1}}},
		{Symbol: testSym, EngineSeq: 3, Ts: time.UnixMilli(30),
			Bids: []Level{{Price: 1_10000001, Qty: 5_00000000, Count: 2}}, // unchanged top
			Asks: []Level{{Price: 1_20000000, Qty: 1_00000000, Count: 1}}},
		{Symbol: testSym, EngineSeq: 4, Ts: time.UnixMilli(40),
			Bids: []Level{{Price: 1_10000001, Qty: 9_00000000, Count: 3}}, // qty changed
			Asks: []Level{{Price: 1_20000000, Qty: 1_00000000, Count: 1}}},
	}
	p := NewBBOProducer(BBOProducerConfig{}, deltaChanSource{deltas}, em.fn())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.Run(ctx)

	// 3 emits expected: seq1 (first seen), seq2 (bid price), seq4 (bid qty).
	// seq3 is suppressed — identical top.
	if got := em.waitFor(3, 2*time.Second); got != 3 {
		t.Fatalf("emitted %d, want 3", got)
	}
	time.Sleep(50 * time.Millisecond)
	if got := len(em.all()); got != 3 {
		t.Fatalf("over-emitted: %d frames", got)
	}
	all := em.all()
	if all[0].Seq != 1 || all[1].Seq != 2 || all[2].Seq != 4 {
		t.Fatalf("engine seqs = %d,%d,%d", all[0].Seq, all[1].Seq, all[2].Seq)
	}
	m := payload(t, all[1])
	if m["bid"] != "1.10000001" || m["bid_qty"] != "5" {
		t.Fatalf("bid tuple wrong: %v", m)
	}
}

// TestBBO_OneSidedBook — an empty side renders JSON null fields, never
// a stale carry from the prior frame.
func TestBBO_OneSidedBook(t *testing.T) {
	em := &emitter{}
	p := NewBBOProducer(BBOProducerConfig{}, deltaChanSource{[]BookDelta{
		{Symbol: testSym, EngineSeq: 1, Ts: time.UnixMilli(10),
			Asks: []Level{{Price: 1_20000000, Qty: 2_00000000, Count: 1}}},
	}}, em.fn())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.Run(ctx)

	if em.waitFor(1, 2*time.Second) != 1 {
		t.Fatal("no frame")
	}
	m := payload(t, em.all()[0])
	if v, present := m["bid"]; !present || v != nil {
		t.Fatalf("bid must be present-and-null on empty side, got %v", m["bid"])
	}
	if m["ask"] != "1.2" || m["ask_qty"] != "2" {
		t.Fatalf("ask tuple wrong: %v", m)
	}
	for _, k := range []string{"event", "symbol", "bid", "bid_qty", "ask",
		"ask_qty", "ts_ms"} {
		if _, ok := m[k]; !ok {
			t.Fatalf("missing field %q", k)
		}
	}
}

// TestBBO_SymbolFanout — independent top trackers per symbol.
func TestBBO_SymbolFanout(t *testing.T) {
	em := &emitter{}
	res2 := []BookDelta{
		{Symbol: "EUR/USD", EngineSeq: 1, Bids: []Level{{Price: 1, Qty: 1}}},
		{Symbol: "GBP/USD", EngineSeq: 5, Bids: []Level{{Price: 2, Qty: 2}}},
		{Symbol: "EUR/USD", EngineSeq: 2, Bids: []Level{{Price: 3, Qty: 1}}},
	}
	p := NewBBOProducer(BBOProducerConfig{}, deltaChanSource{res2}, em.fn())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.Run(ctx)

	if em.waitFor(3, 2*time.Second) != 3 {
		t.Fatal("frames missing")
	}
	eur := forChannel(em.all(), "bbo@EUR/USD")
	gbp := forChannel(em.all(), "bbo@GBP/USD")
	if len(eur) != 2 || len(gbp) != 1 {
		t.Fatalf("eur=%d gbp=%d", len(eur), len(gbp))
	}
}
