package marketdata

import (
	"context"
	"testing"
	"time"

	"exchange/pkg/decimal"
)

// TestTrades_NoConflation — Task 6.3.3: EVERY trade publishes an
// individual frame; bursts are never conflated (spec §10.1/§24 #262's
// sibling requirement for the raw tape).
func TestTrades_NoConflation(t *testing.T) {
	em := &emitter{}
	src := tradeChanSource{evts: []TradeEvent{
		{TradeID: 1, Symbol: testSym, Price: mustDec(t, "1.10"),
			Quantity: decimal.NewFromInt(1), TakerSide: SideBuy,
			Seq: 11, Ts: time.UnixMilli(1000)},
		{TradeID: 2, Symbol: testSym, Price: mustDec(t, "1.10"),
			Quantity: decimal.NewFromInt(2), TakerSide: SideBuy,
			Seq: 12, Ts: time.UnixMilli(1001)},
		{TradeID: 3, Symbol: testSym, Price: mustDec(t, "1.11"),
			Quantity: decimal.NewFromInt(1), TakerSide: SideBuy,
			Seq: 13, Ts: time.UnixMilli(1002)},
	}}
	p := NewTradesProducer(TradesProducerConfig{}, src, em.fn())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.Run(ctx)

	if got := em.waitFor(3, 2*time.Second); got != 3 {
		t.Fatalf("emitted %d frames, want 3", got)
	}
	for i, e := range em.all() {
		if e.Channel != "trades@"+testSym {
			t.Fatalf("frame %d channel %q", i, e.Channel)
		}
		m := payload(t, e)
		if m["trade_id"] != float64(i+1) {
			t.Fatalf("frame %d trade_id=%v", i, m["trade_id"])
		}
		if m["event"] != "trade" {
			t.Fatalf("event=%v", m["event"])
		}
	}
}

// TestTrades_PayloadShape — fields per spec §10.1: trade_id, price,
// quantity, side, timestamp, seq (seq is the frame field, engine_seq
// the diagnostic).
func TestTrades_PayloadShape(t *testing.T) {
	em := &emitter{}
	p := NewTradesProducer(TradesProducerConfig{}, nil, em.fn())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.Run(ctx)

	p.Push(TradeEvent{TradeID: 42, Symbol: testSym,
		Price: mustDec(t, "1.23456789"), Quantity: mustDec(t, "1250000.5"),
		TakerSide: SideSell, Seq: 777, Ts: time.UnixMilli(1_700_000_000_000)})

	if em.waitFor(1, 2*time.Second) != 1 {
		t.Fatal("no frame")
	}
	e := em.all()[0]
	if e.Seq != 777 {
		t.Fatalf("seq = %d, want engine seq 777", e.Seq)
	}
	m := payload(t, e)
	for _, k := range []string{"event", "symbol", "trade_id", "price",
		"quantity", "side", "ts_ms", "engine_seq"} {
		if _, ok := m[k]; !ok {
			t.Fatalf("missing field %q in %v", k, m)
		}
	}
	if m["price"] != "1.23456789" || m["quantity"] != "1250000.5" {
		t.Fatalf("price/qty strings wrong: %v %v", m["price"], m["quantity"])
	}
	if m["side"] != "SELL" {
		t.Fatalf("side=%v", m["side"])
	}
	if m["is_buyer_maker"] != true {
		t.Fatalf("is_buyer_maker=%v (SELL taker → buyer IS maker)", m)
	}
	if m["ts_ms"] != float64(1_700_000_000_000) {
		t.Fatalf("ts_ms=%v", m["ts_ms"])
	}
}

// TestTrades_UnknownSideNeverGuessed — an unresolvable aggressor emits
// "UNKNOWN" and omits is_buyer_maker rather than fabricating it.
func TestTrades_UnknownSideNeverGuessed(t *testing.T) {
	em := &emitter{}
	p := NewTradesProducer(TradesProducerConfig{}, nil, em.fn())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.Run(ctx)

	p.Push(TradeEvent{TradeID: 1, Symbol: testSym,
		Price: decimal.NewFromInt(1), Quantity: decimal.NewFromInt(1),
		Seq: 1, Ts: time.Now()})

	if em.waitFor(1, 2*time.Second) != 1 {
		t.Fatal("no frame")
	}
	m := payload(t, em.all()[0])
	if m["side"] != "UNKNOWN" {
		t.Fatalf("side=%v, want UNKNOWN", m["side"])
	}
	if _, present := m["is_buyer_maker"]; present {
		t.Fatal("is_buyer_maker must be omitted when side unknown")
	}
}

// TestTrades_SeqMonotonicFallback — trades without an engine seq fall
// back to the channel-scoped allocator, still strictly monotonic.
func TestTrades_SeqMonotonicFallback(t *testing.T) {
	em := &emitter{}
	p := NewTradesProducer(TradesProducerConfig{}, nil, em.fn())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.Run(ctx)

	for i := uint64(1); i <= 3; i++ {
		p.Push(TradeEvent{TradeID: i, Symbol: testSym,
			Price: decimal.NewFromInt(1), Quantity: decimal.NewFromInt(1),
			Ts: time.Now()}) // Seq=0 → allocator
	}
	if em.waitFor(3, 2*time.Second) != 3 {
		t.Fatal("frames missing")
	}
	for i, e := range em.all() {
		if e.Seq != uint64(i+1) {
			t.Fatalf("seq[%d]=%d, want %d", i, e.Seq, i+1)
		}
	}
}
