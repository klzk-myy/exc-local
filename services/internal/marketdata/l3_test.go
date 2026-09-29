// Phase-17 Task 17.3.2/17.3.4 — L3 unit tests: mirror semantics, hub
// replay/gap/overrun contract, ordering gate, hidden-order redaction,
// wire/JetStream decode, subscription cap, premium-tier gate.
package marketdata

import (
	"context"
	"encoding/json"
	"log/slog"
	"testing"
	"time"

	"exchange/internal/ipc"
	"exchange/internal/ipc/wire"
	"exchange/internal/ratelimit"
	"exchange/internal/ws"
	"exchange/pkg/decimal"
)

// l3wire marshals a wire.Event carrying the provisional L3OrderEvent
// union member (the test half of the ipc.MarshalL3Event seam).
func l3wire(envSeq, ts uint64, f *ipc.L3OrderFields) []byte {
	return ipc.MarshalL3Event(envSeq, ts, f)
}

func l3ev(seq uint64, kind L3Kind, orderID uint64) L3Event {
	return L3Event{
		Symbol: testSym, OrderID: orderID, AccountHash: 0xAA,
		Kind: kind, Side: SideBuy, Price: mustDec(&testing.T{}, "1.1"),
		Quantity: mustDec(&testing.T{}, "1.0"), Ts: time.Unix(0, int64(testNs)),
		Seq: seq, WalSeq: seq * 10,
	}
}

// --- Mirror ------------------------------------------------------------------

func TestL3Mirror_ApplyAndSuspect(t *testing.T) {
	m := NewL3BookMirror()
	m.apply(l3ev(1, L3Add, 100))
	m.apply(l3ev(2, L3Add, 101))
	orders, ok := m.Snapshot()
	if !ok || len(orders) != 2 {
		t.Fatalf("snapshot ok=%v n=%d", ok, len(orders))
	}
	m.apply(l3ev(3, L3Cancel, 100))
	orders, _ = m.Snapshot()
	if len(orders) != 1 || orders[0].OrderID != 101 {
		t.Fatalf("after cancel: %+v", orders)
	}
	// MODIFY for an unseen order = implicit hole ⇒ suspect.
	m.markSuspect()
	if _, ok := m.Snapshot(); ok {
		t.Fatal("suspect mirror must refuse snapshots")
	}
}

func TestL3Mirror_HiddenExcluded(t *testing.T) {
	m := NewL3BookMirror()
	e := l3ev(1, L3Add, 100)
	e.Hidden = true
	m.apply(e)
	m.apply(l3ev(2, L3Add, 101))
	orders, ok := m.Snapshot()
	if !ok || len(orders) != 1 || orders[0].OrderID != 101 {
		t.Fatalf("hidden order leaked or missing visible: %+v ok=%v", orders, ok)
	}
}

// --- Payload redaction (§24 #197) ---------------------------------------------

func TestL3EventPayload_HiddenRedacted(t *testing.T) {
	e := l3ev(7, L3Add, 42)
	e.Hidden = true
	p := e.payload()
	if p.Event != "ORDER_HIDDEN" {
		t.Fatalf("hidden event = %q, want ORDER_HIDDEN", p.Event)
	}
	if p.AccountHash != 0 || p.Side != "" || p.Price != "" || p.Qty != "" {
		t.Fatalf("hidden order fields leaked: %+v", p)
	}
	if p.OrderID != 42 || p.WalSeq != 70 {
		t.Fatalf("identity/seq fields must survive: %+v", p)
	}
	// EXECUTE on a hidden order publishes the full print.
	e.Kind = L3Execute
	p = e.payload()
	if p.Event != "ORDER_EXECUTE" || p.Price == "" || p.Side == "" {
		t.Fatalf("execute print must be public: %+v", p)
	}
}

// --- Hub: no conflation, replay, gaps, overrun --------------------------------

func TestL3Hub_NoConflationEveryEventForwarded(t *testing.T) {
	h := NewL3Hub(L3HubConfig{Logger: slog.Default()})
	h.Publish(l3ev(1, L3Add, 100))
	h.Publish(l3ev(2, L3Modify, 100))
	h.Publish(l3ev(3, L3Cancel, 100))

	res := h.attach(&l3Conn{srv: &L3Server{}}, testSym, 0)
	if res.Verdict != ReplayOK || len(res.Msgs) != 3 {
		t.Fatalf("replay: %+v msgs=%d", res.Verdict, len(res.Msgs))
	}
	// Every marshaled frame carries its own l3_seq — verbatim, unmerged.
	for i, raw := range res.Msgs {
		var f struct {
			Type string `json:"type"`
			Seq  uint64 `json:"seq"`
			Data struct {
				Event string `json:"event"`
			} `json:"data"`
		}
		if err := json.Unmarshal(raw, &f); err != nil {
			t.Fatalf("frame %d: %v", i, err)
		}
		if f.Seq != uint64(i+1) {
			t.Fatalf("frame %d seq=%d — conflation/coalescing detected", i, f.Seq)
		}
	}
}

func TestL3Hub_ReplayWithinHorizon(t *testing.T) {
	h := NewL3Hub(L3HubConfig{Logger: slog.Default()})
	for i := uint64(1); i <= 50; i++ {
		h.Publish(l3ev(i, L3Add, 1000+i))
	}
	c := &l3Conn{srv: &L3Server{}}
	res := h.attach(c, testSym, 40)
	if res.Verdict != ReplayOK || len(res.Msgs) != 10 {
		t.Fatalf("want 10 replayed frames seq>40: %+v n=%d", res.Verdict, len(res.Msgs))
	}
	res = h.attach(&l3Conn{srv: &L3Server{}}, testSym, 50)
	if res.Verdict != ReplayUpToDate {
		t.Fatalf("tail cursor verdict = %v", res.Verdict)
	}
}

func TestL3Hub_ReplayGapTooLarge(t *testing.T) {
	h := NewL3Hub(L3HubConfig{Logger: slog.Default(), ReplayBufferMsgs: 10})
	for i := uint64(1); i <= 20; i++ {
		h.Publish(l3ev(i, L3Add, 1000+i))
	}
	res := h.attach(&l3Conn{srv: &L3Server{}}, testSym, 5)
	if res.Verdict != ReplayGapTooLarge {
		t.Fatalf("cursor below horizon verdict = %v", res.Verdict)
	}
	res = h.attach(&l3Conn{srv: &L3Server{}}, testSym, 99)
	if res.Verdict != ReplayInvalidSeq {
		t.Fatalf("cursor ahead of tail verdict = %v", res.Verdict)
	}
}

func TestL3Hub_FeedGapMarksMirrorSuspect(t *testing.T) {
	h := NewL3Hub(L3HubConfig{Logger: slog.Default()})
	h.Publish(l3ev(1, L3Add, 100))
	h.Publish(l3ev(5, L3Add, 101)) // l3_seq jump 1→5 = lost feed events
	if h.FeedGaps() != 1 {
		t.Fatalf("feed gaps = %d, want 1", h.FeedGaps())
	}
	if _, err := h.snapshot(testSym); err != ErrL3Suspect {
		t.Fatalf("suspect mirror snapshot err = %v", err)
	}
}

func TestL3Hub_SubscriberGetsEveryLiveEvent(t *testing.T) {
	h := NewL3Hub(L3HubConfig{Logger: slog.Default()})
	srv := &L3Server{cfg: L3ServerConfig{Hub: h}}
	srv.cfg.OutboundBuffer = 8
	c := &l3Conn{srv: srv, out: make(chan []byte, 8),
		done: make(chan struct{}), pumpDone: make(chan struct{})}
	h.attach(c, testSym, 0)
	h.Publish(l3ev(1, L3Add, 100))
	h.Publish(l3ev(2, L3Cancel, 100))
	for want := uint64(1); want <= 2; want++ {
		select {
		case raw := <-c.out:
			var f struct {
				Seq uint64 `json:"seq"`
			}
			_ = json.Unmarshal(raw, &f)
			if f.Seq != want {
				t.Fatalf("sub saw seq=%d want %d", f.Seq, want)
			}
		case <-time.After(time.Second):
			t.Fatalf("missing frame %d", want)
		}
	}
}

// --- Overrun / ordering gate ---------------------------------------------------

func newTestL3Server(buf int) (*L3Server, *l3Conn) {
	h := NewL3Hub(L3HubConfig{Logger: slog.Default()})
	srv := NewL3Server(L3ServerConfig{Hub: h, OutboundBuffer: buf})
	c := &l3Conn{srv: srv, out: make(chan []byte, buf),
		done: make(chan struct{}), pumpDone: make(chan struct{})}
	return srv, c
}

func TestL3Conn_OutboxLagOverrun(t *testing.T) {
	srv, c := newTestL3Server(4)
	// Fill the outbox without a reader: pushes 5,6.. fail → overrun.
	for i := 0; i < 4; i++ {
		c.push([]byte("x"))
	}
	c.push([]byte("over"))
	if got := srv.cfg.Hub.overruns.Load(); got != 1 {
		t.Fatalf("overruns = %d, want 1", got)
	}
	if code := c.closeCode.Load(); int(code) != CloseSlowConsumer {
		t.Fatalf("close code = %d, want CloseSlowConsumer %d", code, CloseSlowConsumer)
	}
	select {
	case <-c.done:
	case <-time.After(time.Second):
		t.Fatal("conn not closed after overrun")
	}
}

func TestL3Conn_ReplayGateOrdering(t *testing.T) {
	srv, c := newTestL3Server(16)
	h := srv.cfg.Hub
	h.Publish(l3ev(1, L3Add, 100))
	h.Publish(l3ev(2, L3Add, 101))

	c.beginReplay()
	res := h.attach(c, testSym, 0) // replay verdict = frames 1,2
	if res.Verdict != ReplayOK || len(res.Msgs) != 2 {
		t.Fatalf("replay: %+v", res.Verdict)
	}
	for _, m := range res.Msgs {
		c.push(m) // replay frames go direct
	}
	h.Publish(l3ev(3, L3Add, 102)) // live publish mid-replay → pending
	c.endReplay()

	var seqs []uint64
	for i := 0; i < 3; i++ {
		select {
		case raw := <-c.out:
			var f struct {
				Seq uint64 `json:"seq"`
			}
			_ = json.Unmarshal(raw, &f)
			seqs = append(seqs, f.Seq)
		case <-time.After(time.Second):
			t.Fatalf("missing frame %d", i)
		}
	}
	if len(seqs) != 3 || seqs[0] != 1 || seqs[1] != 2 || seqs[2] != 3 {
		t.Fatalf("ordering broken: %v", seqs)
	}
}

func TestL3Conn_PendingOverflowOverruns(t *testing.T) {
	srv, c := newTestL3Server(2)
	c.beginReplay()
	c.enqueue([]byte("a"))
	c.enqueue([]byte("b"))
	c.enqueue([]byte("c")) // pending full → overrun
	if srv.cfg.Hub.overruns.Load() != 1 {
		t.Fatalf("overruns = %d", srv.cfg.Hub.overruns.Load())
	}
}

// --- Account cap + tier gate ----------------------------------------------------

func TestL3Server_FiveSubscriptionCap(t *testing.T) {
	h := NewL3Hub(L3HubConfig{Logger: slog.Default()})
	srv := NewL3Server(L3ServerConfig{Hub: h})
	sess := &ws.Session{AccountID: 42, Authenticated: true}
	for i := 0; i < L3MaxSubsPerAccount; i++ {
		if !srv.accountCapAdmit(&l3Conn{srv: srv}, sess) {
			t.Fatalf("admit %d rejected", i+1)
		}
	}
	if srv.accountCapAdmit(&l3Conn{srv: srv}, sess) {
		t.Fatal("6th L3 subscription admitted — cap broken")
	}
	// A different account is independent.
	other := &ws.Session{AccountID: 7, Authenticated: true}
	if !srv.accountCapAdmit(&l3Conn{srv: srv}, other) {
		t.Fatal("independent account rejected")
	}
}

func TestL3PremiumTierGate(t *testing.T) {
	for _, tier := range []ratelimit.Tier{
		ratelimit.TierProfessional, ratelimit.TierInstitutional, ratelimit.TierAdmin,
	} {
		if !l3PremiumTier(tier) {
			t.Fatalf("premium tier %q rejected", tier)
		}
	}
	for _, tier := range []ratelimit.Tier{
		ratelimit.TierBasic, ratelimit.TierPublic, ratelimit.Tier(""),
	} {
		if l3PremiumTier(tier) {
			t.Fatalf("non-premium tier %q admitted", tier)
		}
	}
}

// --- Wire / JetStream decode -----------------------------------------------------

func TestWireL3Source_DecodesViaInstrumentID(t *testing.T) {
	// instrument_id is the authoritative symbol route — the L3 row
	// resolves without any prior OrderNew on the stream.
	src := byteChanSource{bufs: [][]byte{
		l3wire(2, testNs+10, &ipc.L3OrderFields{
			InstrumentID: testInst,
			OrderID:      100, AccountHash: 0xBEEF,
			Kind: ipc.L3KindAdd, Side: 0,
			PriceTicks: 110_000_000, QtyUnits: 5_000_000,
			QtyDelta: 5_000_000, L3Seq: 41, WalSeq: 9001,
		}),
	}}
	ws := NewWireL3Source(src.Bytes, testResolver, slog.Default())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := ws.Events(ctx)
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	select {
	case ev := <-ch:
		if ev.Symbol != testSym || ev.OrderID != 100 ||
			ev.Seq != 41 || ev.WalSeq != 9001 || ev.Kind != L3Add {
			t.Fatalf("event = %+v", ev)
		}
		if !ev.Price.Equal(decimal.NewFromScaled(110_000_000)) {
			t.Fatalf("price = %v", ev.Price)
		}
		if ev.InstrumentID != testInst ||
			!ev.QtyDelta.Equal(decimal.NewFromScaled(5_000_000)) {
			t.Fatalf("instrument/qty_delta = %+v", ev)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no L3 event emitted")
	}
}

func TestWireL3Source_OrderIndexFallback(t *testing.T) {
	// Rows whose instrument_id is absent (0) still route through the
	// OrderNew-fed admission index.
	src := byteChanSource{bufs: [][]byte{
		encodeOrderNew(1, testNs, 100, 7, testInst, wire.SideBuy,
			1_00000000, 1_10000000),
		l3wire(2, testNs+10, &ipc.L3OrderFields{
			OrderID: 100, Kind: ipc.L3KindAdd, Side: 0,
			PriceTicks: 110_000_000, QtyUnits: 5_000_000,
			L3Seq: 41, WalSeq: 9001,
		}),
	}}
	ws := NewWireL3Source(src.Bytes, testResolver, slog.Default())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := ws.Events(ctx)
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	select {
	case ev := <-ch:
		if ev.Symbol != testSym || ev.OrderID != 100 || ev.Seq != 41 {
			t.Fatalf("index-fallback event = %+v", ev)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no L3 event emitted")
	}
}

func TestWireL3Source_UnresolvedOrderDropped(t *testing.T) {
	src := byteChanSource{bufs: [][]byte{
		l3wire(1, testNs, &ipc.L3OrderFields{
			OrderID: 999, Kind: ipc.L3KindAdd, L3Seq: 1, WalSeq: 1,
		}),
	}}
	var drops []string
	ws := NewWireL3Source(src.Bytes, testResolver, slog.Default())
	ws.OnDrop = func(r string) { drops = append(drops, r) }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := ws.Events(ctx)
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	if ev, ok := <-ch; ok {
		t.Fatalf("unrouted event emitted: %+v", ev)
	}
	if len(drops) != 1 || drops[0] != "unresolved" {
		t.Fatalf("drops = %v", drops)
	}
}

func TestJetStreamL3Source_SubjectRoutes(t *testing.T) {
	payload := l3wire(9, testNs, &ipc.L3OrderFields{
		OrderID: 55, AccountHash: 0xAA, Kind: ipc.L3KindExecute,
		Side: 1, PriceTicks: 100, QtyUnits: 200, L3Seq: 77, WalSeq: 88,
	})
	src := MsgSource(func(ctx context.Context) (<-chan RawMsg, error) {
		ch := make(chan RawMsg, 1)
		ch <- RawMsg{Subject: "l3.0.EUR-USD", Data: payload}
		close(ch)
		return ch, nil
	})
	js := NewJetStreamL3Source(src, SubjectSymbols(testResolver), slog.Default())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := js.Events(ctx)
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	ev := <-ch
	if ev.Symbol != testSym || ev.Kind != L3Execute || ev.Side != SideSell ||
		ev.Seq != 77 || ev.WalSeq != 88 {
		t.Fatalf("event = %+v", ev)
	}
}
