package marketdata

import (
	"context"
	"testing"
	"time"
)

// TestLiquidations_InvisibleBeforeTwoSeconds — the §24 #263 proof: a
// liquidation pushed at T is observable ONLY at/after T+2s, on both
// liquidations@{symbol} and liquidations@all.
func TestLiquidations_InvisibleBeforeTwoSeconds(t *testing.T) {
	em := &emitter{}
	p := NewLiquidationsProducer(LiquidationsProducerConfig{}, nil, em.fn())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.Run(ctx)

	ev := LiquidationEvent{
		Symbol: testSym, Side: "SELL", OrderType: "LIMIT",
		Price: "1.10", Qty: "500000", Ts: time.Now(),
	}
	p.Push(ev)

	// Assert invisibility at ~1.6s — well inside the 2s window.
	time.Sleep(1600 * time.Millisecond)
	if n := len(em.all()); n != 0 {
		t.Fatalf("liquidation leaked early: %d frames before 2s", n)
	}

	// By ~2.6s both frames (symbol + @all) must be published.
	if got := em.waitFor(2, 1500*time.Millisecond); got != 2 {
		t.Fatalf("emitted %d, want 2 (symbol + all)", got)
	}
	sym := forChannel(em.all(), "liquidations@"+testSym)
	all := forChannel(em.all(), "liquidations@all")
	if len(sym) != 1 || len(all) != 1 {
		t.Fatalf("sym=%d all=%d", len(sym), len(all))
	}
	m := payload(t, sym[0])
	if m["event"] != "liquidation" || m["side"] != "SELL" ||
		m["price"] != "1.10" || m["quantity"] != "500000" {
		t.Fatalf("payload wrong: %v", m)
	}
	// Delay transparency: pub_ts_ms must trail ts_ms by ~2000ms.
	pub := int64(m["pub_ts_ms"].(float64))
	ts := int64(m["ts_ms"].(float64))
	if pub-ts < 1900 {
		t.Fatalf("published only %dms after event — delay violated", pub-ts)
	}
}

// TestLiquidations_DelayFloorEnforced — configuring a shorter delay must
// not weaken the anti-front-running gate: the floor clamps to 2s.
func TestLiquidations_DelayFloorEnforced(t *testing.T) {
	p := NewLiquidationsProducer(
		LiquidationsProducerConfig{Delay: 10 * time.Millisecond},
		nil, func(string, uint64, any) {})
	if p.gate.delay != LiquidationDelay {
		t.Fatalf("delay = %v, floor %v must apply", p.gate.delay, LiquidationDelay)
	}
}

// TestLiquidations_Anonymized — the public frame carries no account,
// order or position identifiers.
func TestLiquidations_Anonymized(t *testing.T) {
	em := &emitter{}
	p := NewLiquidationsProducer(
		LiquidationsProducerConfig{Delay: 30 * time.Millisecond},
		nil, em.fn())
	// NOTE: floor clamps to 2s — but Push through the GATE uses the
	// producer's configured delay; a sub-2s config is clamped. To keep
	// this test fast we exercise the gate directly at a lawful 2s via a
	// shortened internal path: Push → gate (clamped) → assert shape on
	// the events emitted after the floor.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.Run(ctx)
	p.Push(LiquidationEvent{Symbol: testSym, Side: "BUY",
		OrderType: "MARKET", Price: "1.2", Qty: "10", IsAuction: true,
		Ts: time.Now().Add(-3 * time.Second)}) // aged: release due soon
	// (aged anchor: ts in the past → release = ts+2s ≈ already due)
	if em.waitFor(2, 2*time.Second) != 2 {
		t.Fatal("no frames")
	}
	m := payload(t, forChannel(em.all(), "liquidations@"+testSym)[0])
	for _, banned := range []string{"account", "account_id", "order_id",
		"position_id", "user_id"} {
		if _, ok := m[banned]; ok {
			t.Fatalf("anonymity violated: field %q present", banned)
		}
	}
	if m["is_auction"] != true || m["order_type"] != "MARKET" {
		t.Fatalf("fields: %v", m)
	}
}

// TestJetStreamLiquidationSource_FiltersTypes — only type=="liquidation"
// messages flow; margin calls on the same stream never leak.
func TestJetStreamLiquidationSource_FiltersTypes(t *testing.T) {
	src := MsgSource(func(ctx context.Context) (<-chan RawMsg, error) {
		ch := make(chan RawMsg, 2)
		ch <- RawMsg{Subject: "margin-events.0.EUR-USD",
			Data: []byte(`{"type":"margin_call","side":"SELL"}`)}
		ch <- RawMsg{Subject: "margin-events.0.EUR-USD",
			Data: []byte(`{"type":"liquidation","side":"SELL","order_type":"LIMIT","price":"1.1","qty":"5","ts_ms":1700000000000}`)}
		close(ch)
		return ch, nil
	})
	ls := JetStreamLiquidationSource(src, SubjectSymbols(testResolver), nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := ls.Liquidations(ctx)
	if err != nil {
		t.Fatalf("Liquidations: %v", err)
	}
	ev, ok := <-ch
	if !ok {
		t.Fatal("expected one liquidation")
	}
	if ev.Symbol != testSym || ev.Side != "SELL" || ev.Price != "1.1" {
		t.Fatalf("event wrong: %+v", ev)
	}
	if _, ok := <-ch; ok {
		t.Fatal("margin_call leaked through the filter")
	}
}
