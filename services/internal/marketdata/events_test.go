package marketdata

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"exchange/internal/ipc/wire"
)

// TestWireTradeSource_DecodesTradeFill proves the IPC path resolves
// symbol + aggressor through the admission index: the later-admitted
// order is the taker (spec §10.1 trade side semantics).
func TestWireTradeSource_DecodesTradeFill(t *testing.T) {
	src := byteChanSource{bufs: [][]byte{
		encodeOrderNew(1, testNs, 100, 7, testInst, wire.SideBuy,
			1_00000000, 1_10000000), // resting buy, admitted seq 1
		encodeOrderNew(2, testNs, 200, 9, testInst, wire.SideSell,
			1_00000000, 1_10000000), // incoming sell, admitted seq 2
		encodeTradeFill(3, testNs+50, 9001, 100, 200,
			1_10000000, 50000000, 42), // fill: taker = order 200
	}}

	ws := NewWireTradeSource(src.Bytes, testResolver, slog.Default())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := ws.Trades(ctx)
	if err != nil {
		t.Fatalf("Trades: %v", err)
	}

	var ev TradeEvent
	select {
	case ev = <-ch:
	case <-time.After(2 * time.Second):
		t.Fatal("no trade event")
	}

	if ev.Symbol != testSym {
		t.Fatalf("symbol = %q, want %q", ev.Symbol, testSym)
	}
	if ev.TradeID != 9001 {
		t.Fatalf("trade_id = %d", ev.TradeID)
	}
	if ev.TakerSide != SideSell {
		t.Fatalf("taker side = %q, want SELL", ev.TakerSide)
	}
	if ev.TakerOrderID != 200 || ev.MakerOrderID != 100 {
		t.Fatalf("lineage taker=%d maker=%d, want 200/100",
			ev.TakerOrderID, ev.MakerOrderID)
	}
	if ev.Seq != 42 {
		t.Fatalf("engine seq = %d, want 42", ev.Seq)
	}
	if !ev.Price.Equal(dec(t, 1_10000000)) || !ev.Quantity.Equal(dec(t, 50000000)) {
		t.Fatalf("price/qty = %v/%v", ev.Price, ev.Quantity)
	}
	if ev.Ts.UnixNano() != int64(testNs+50) {
		t.Fatalf("ts = %v", ev.Ts)
	}
}

// TestWireTradeSource_UnresolvedFillDropped — a fill whose orders were
// never admitted cannot be routed or sided; it is dropped and counted,
// never emitted with a fabricated symbol (§2.7).
func TestWireTradeSource_UnresolvedFillDropped(t *testing.T) {
	src := byteChanSource{bufs: [][]byte{
		encodeTradeFill(1, testNs, 1, 999, 998, 1_10000000, 1, 7),
	}}
	var dropped []string
	ws := NewWireTradeSource(src.Bytes, testResolver, slog.Default())
	ws.OnDrop = func(r string) { dropped = append(dropped, r) }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := ws.Trades(ctx)
	if err != nil {
		t.Fatalf("Trades: %v", err)
	}
	if ev, ok := <-ch; ok {
		t.Fatalf("unexpected event %+v", ev)
	}
	if len(dropped) != 1 || dropped[0] != "unresolved" {
		t.Fatalf("drops = %v", dropped)
	}
}

// TestJetStreamTradeSource_SubjectRoutes — the bridge's republished
// stream carries no instrument_id; the subject token resolves the
// symbol. Without an order feed the aggressor stays unknown.
func TestJetStreamTradeSource_SubjectRoutes(t *testing.T) {
	fill := encodeTradeFill(5, testNs, 77, 11, 22,
		1_25000000, 250000000, 9)
	src := MsgSource(func(ctx context.Context) (<-chan RawMsg, error) {
		ch := make(chan RawMsg, 1)
		ch <- RawMsg{Subject: "trades.0.EUR-USD", Data: fill}
		close(ch)
		return ch, nil
	})

	js := NewJetStreamTradeSource(src, SubjectSymbols(testResolver),
		testResolver, slog.Default())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := js.Trades(ctx)
	if err != nil {
		t.Fatalf("Trades: %v", err)
	}
	ev := <-ch
	if ev.Symbol != testSym {
		t.Fatalf("symbol = %q", ev.Symbol)
	}
	if ev.TakerSide != "" || ev.TakerOrderID != 0 {
		t.Fatalf("taker should be unknown, got side=%q id=%d",
			ev.TakerSide, ev.TakerOrderID)
	}
	if ev.TradeID != 77 || ev.Seq != 9 {
		t.Fatalf("trade_id=%d seq=%d", ev.TradeID, ev.Seq)
	}
}

// TestJetStreamTradeSource_UnroutedSubjectDropped — an unmapped subject
// must not emit a guessed symbol.
func TestJetStreamTradeSource_UnroutedSubjectDropped(t *testing.T) {
	fill := encodeTradeFill(5, testNs, 77, 11, 22, 1_25000000, 1, 9)
	src := MsgSource(func(ctx context.Context) (<-chan RawMsg, error) {
		ch := make(chan RawMsg, 1)
		// Token outside the configured map AND not a "X-Y" pair form.
		ch <- RawMsg{Subject: "trades.0.MYSTERY", Data: fill}
		close(ch)
		return ch, nil
	})
	var drops int
	js := NewJetStreamTradeSource(src, SubjectSymbols(testResolver),
		testResolver, slog.Default())
	js.OnDrop = func(string) { drops++ }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := js.Trades(ctx)
	if err != nil {
		t.Fatalf("Trades: %v", err)
	}
	if ev, ok := <-ch; ok {
		t.Fatalf("unexpected event %+v", ev)
	}
	if drops != 1 {
		t.Fatalf("drops = %d", drops)
	}
}

// TestFanOut_Broadcast — every subscriber receives every event; a tap
// added before Run sees the full stream.
func TestFanOut_Broadcast(t *testing.T) {
	up := make(chan TradeEvent, 3)
	f := NewFanOut[TradeEvent](up)
	a := f.Subscribe(4)
	b := f.Subscribe(4)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- f.Run(ctx) }()

	for i := uint64(1); i <= 3; i++ {
		up <- TradeEvent{TradeID: i, Symbol: testSym}
	}
	close(up)
	if err := <-done; err != nil {
		t.Fatalf("fanout: %v", err)
	}
	for name, ch := range map[string]<-chan TradeEvent{"a": a, "b": b} {
		var ids []uint64
		for ev := range ch {
			ids = append(ids, ev.TradeID)
		}
		if len(ids) != 3 || ids[0] != 1 || ids[2] != 3 {
			t.Fatalf("sub %s ids = %v", name, ids)
		}
	}
}

// TestTeeDeltaSource_Mirrors — the BBO tap sees identical deltas to the
// conflator's input, in order.
func TestTeeDeltaSource_Mirrors(t *testing.T) {
	deltas := []BookDelta{
		{Symbol: testSym, EngineSeq: 1,
			Bids: []Level{{Price: 1_10000000, Qty: 1, Count: 1}}},
		{Symbol: testSym, EngineSeq: 2,
			Asks: []Level{{Price: 1_20000000, Qty: 2, Count: 1}}},
	}
	tap := make(chan BookDelta, 4)
	tee := TeeDeltaSource(deltaChanSource{deltas}, tap)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out, err := tee.Deltas(ctx)
	if err != nil {
		t.Fatalf("Deltas: %v", err)
	}
	var got []BookDelta
	for d := range out {
		got = append(got, d)
	}
	// Taps stay open after upstream closes (owned by the caller); drain
	// exactly the mirrored count.
	var tapped []BookDelta
	for range deltas {
		select {
		case d := <-tap:
			tapped = append(tapped, d)
		case <-time.After(2 * time.Second):
			t.Fatal("tap missing delta")
		}
	}
	if len(got) != 2 || len(tapped) != 2 {
		t.Fatalf("got=%d tapped=%d", len(got), len(tapped))
	}
	if tapped[1].EngineSeq != 2 {
		t.Fatalf("tap order broken: %+v", tapped)
	}
}

// TestSeqAllocator_PerChannel — channel-scoped counters start at 1 and
// never collide across channels.
func TestSeqAllocator_PerChannel(t *testing.T) {
	a := newSeqAllocator()
	if a.next("trades@EUR/USD") != 1 || a.next("trades@EUR/USD") != 2 {
		t.Fatal("monotonic violation")
	}
	if a.next("bbo@EUR/USD") != 1 {
		t.Fatal("channels must have independent domains")
	}
}
