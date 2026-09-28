package marketdata

import (
	"context"
	"testing"
	"time"

	"exchange/pkg/decimal"
)

// TestAggTrades_GroupsLineage — Task 6.3.12 / §24 #262: contiguous fills
// by the same taker at the same price fold into one aggregate carrying
// first_trade_id, last_trade_id and a constituent count.
func TestAggTrades_GroupsLineage(t *testing.T) {
	em := &emitter{}
	src := tradeChanSource{evts: []TradeEvent{
		{TradeID: 10, Symbol: testSym, Price: mustDec(t, "1.10"),
			Quantity: decimal.NewFromInt(1), TakerSide: SideSell,
			TakerOrderID: 500, Seq: 1, Ts: time.UnixMilli(100)},
		{TradeID: 11, Symbol: testSym, Price: mustDec(t, "1.10"),
			Quantity: decimal.NewFromInt(2), TakerSide: SideSell,
			TakerOrderID: 500, Seq: 2, Ts: time.UnixMilli(101)},
		{TradeID: 12, Symbol: testSym, Price: mustDec(t, "1.10"),
			Quantity: decimal.NewFromInt(3), TakerSide: SideSell,
			TakerOrderID: 500, Seq: 3, Ts: time.UnixMilli(102)},
		// Key change — different taker: flushes the 3-fill aggregate.
		{TradeID: 13, Symbol: testSym, Price: mustDec(t, "1.10"),
			Quantity: decimal.NewFromInt(1), TakerSide: SideBuy,
			TakerOrderID: 501, Seq: 4, Ts: time.UnixMilli(103)},
	}}
	p := NewAggTradesProducer(
		AggTradesProducerConfig{FlushEvery: 30 * time.Millisecond},
		src, em.fn())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.Run(ctx)

	if em.waitFor(2, 2*time.Second) != 2 {
		t.Fatalf("emitted %d, want 2", em.waitFor(0, 0))
	}
	m := payload(t, em.all()[0])
	if m["event"] != "aggTrade" || m["symbol"] != testSym {
		t.Fatalf("envelope wrong: %v", m)
	}
	if m["first_trade_id"] != float64(10) || m["last_trade_id"] != float64(12) {
		t.Fatalf("lineage wrong: %v", m)
	}
	if m["trade_count"] != float64(3) {
		t.Fatalf("count=%v", m["trade_count"])
	}
	if m["quantity"] != "6" {
		t.Fatalf("total qty=%v, want 6", m["quantity"])
	}
	if m["price"] != "1.1" {
		t.Fatalf("price=%v", m["price"])
	}
	if m["is_buyer_maker"] != true {
		t.Fatalf("SELL taker → buyer is maker: %v", m)
	}
	if m["agg_trade_id"] != float64(1) {
		t.Fatalf("agg_trade_id=%v", m["agg_trade_id"])
	}
	// Second frame: the new taker's flush (timer or ctx-cancel flush).
}

// TestAggTrades_PriceBreakSplits — same taker, different price → two
// aggregates even though the taker id is identical.
func TestAggTrades_PriceBreakSplits(t *testing.T) {
	em := &emitter{}
	p := NewAggTradesProducer(
		AggTradesProducerConfig{FlushEvery: 20 * time.Millisecond},
		nil, em.fn())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.Run(ctx)

	p.Push(TradeEvent{TradeID: 1, Symbol: testSym, Price: mustDec(t, "1.10"),
		Quantity: decimal.NewFromInt(1), TakerSide: SideBuy, TakerOrderID: 9,
		Seq: 1, Ts: time.Now()})
	p.Push(TradeEvent{TradeID: 2, Symbol: testSym, Price: mustDec(t, "1.11"),
		Quantity: decimal.NewFromInt(1), TakerSide: SideBuy, TakerOrderID: 9,
		Seq: 2, Ts: time.Now()})

	if em.waitFor(2, 2*time.Second) != 2 {
		t.Fatalf("emitted %d, want 2", em.waitFor(0, 0))
	}
	first, second := payload(t, em.all()[0]), payload(t, em.all()[1])
	if first["price"] != "1.1" || second["price"] != "1.11" {
		t.Fatalf("split wrong: %v / %v", first["price"], second["price"])
	}
	if first["last_trade_id"] != float64(1) || second["first_trade_id"] != float64(2) {
		t.Fatalf("lineage wrong")
	}
}

// TestAggTrades_UnknownTakerSingletons — an unresolvable aggressor must
// never group two distinct fills together: each emits alone.
func TestAggTrades_UnknownTakerSingletons(t *testing.T) {
	em := &emitter{}
	p := NewAggTradesProducer(
		AggTradesProducerConfig{FlushEvery: 20 * time.Millisecond},
		nil, em.fn())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.Run(ctx)

	for i := uint64(1); i <= 3; i++ {
		p.Push(TradeEvent{TradeID: i, Symbol: testSym,
			Price: mustDec(t, "1.10"), Quantity: decimal.NewFromInt(1),
			Seq: i, Ts: time.Now()}) // TakerOrderID=0 → unique key
	}
	if em.waitFor(3, 2*time.Second) != 3 {
		t.Fatalf("emitted %d, want 3 singletons", em.waitFor(0, 0))
	}
	for i, e := range em.all() {
		m := payload(t, e)
		if m["trade_count"] != float64(1) {
			t.Fatalf("frame %d grouped %v fills", i, m["trade_count"])
		}
		if _, ok := m["is_buyer_maker"]; ok {
			t.Fatal("unknown side must not claim maker flag")
		}
	}
}

// TestAggTrades_IDMonotonicPerSymbol — agg_trade_id is per-symbol
// monotonic across flushes.
func TestAggTrades_IDMonotonicPerSymbol(t *testing.T) {
	em := &emitter{}
	p := NewAggTradesProducer(
		AggTradesProducerConfig{FlushEvery: 10 * time.Millisecond},
		nil, em.fn())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.Run(ctx)

	// Two separate runs (key change between) for one symbol.
	for _, id := range []uint64{1, 2} {
		p.Push(TradeEvent{TradeID: id, Symbol: testSym,
			Price: mustDec(t, "1.10"), Quantity: decimal.NewFromInt(1),
			TakerSide: SideBuy, TakerOrderID: 100 + id,
			Seq: id, Ts: time.Now()})
	}
	if em.waitFor(2, 2*time.Second) != 2 {
		t.Fatal("frames missing")
	}
	if payload(t, em.all()[0])["agg_trade_id"] != float64(1) ||
		payload(t, em.all()[1])["agg_trade_id"] != float64(2) {
		t.Fatalf("agg ids not monotonic: %v", em.all())
	}
}
