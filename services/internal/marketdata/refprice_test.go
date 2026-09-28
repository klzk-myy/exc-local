// Task 6.3.17 — referencePrice@{symbol} stream tests: fresh emits,
// 5s staleness gate, fail-closed stale marking, snapshot source.
// Task 6.3.22 — gap journal + restart continuity.
package marketdata

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"
)

// countingRefSource is a stub ReferencePriceSource returning a fixed
// observation (mutable between calls).
type countingRefSource struct {
	mu    sync.Mutex
	ref   OracleRef
	err   error
	calls int
}

func (s *countingRefSource) Reference(_ context.Context, symbol string) (OracleRef, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	if s.err != nil {
		return OracleRef{}, s.err
	}
	r := s.ref
	if r.Symbol == "" {
		r.Symbol = symbol
	}
	return r, nil
}

func (s *countingRefSource) setRef(r OracleRef) { s.mu.Lock(); s.ref = r; s.mu.Unlock() }
func (s *countingRefSource) setErr(e error)     { s.mu.Lock(); s.err = e; s.mu.Unlock() }
func (s *countingRefSource) n() int             { s.mu.Lock(); defer s.mu.Unlock(); return s.calls }

// runRefStream starts a RefPriceStream against symbols/emit stubs.
func runRefStream(t *testing.T, src ReferencePriceSource,
	rules ExecutionRuleSource, symbols func() []string,
	seq SeqStore) (*RefPriceStream, *emitSink) {
	t.Helper()
	sink := newEmitSink()
	rs := NewRefPriceStream(RefPriceConfig{
		PollInterval:  10 * time.Millisecond,
		StalenessGate: 5 * time.Second,
	}, src, rules, symbols, sink.emit, seq, nil, nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- rs.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("refprice run: %v", err)
		}
	})
	return rs, sink
}

func decodeRef(t *testing.T, row emitRow) map[string]any {
	t.Helper()
	b, err := marshalFrame(row.data)
	if err != nil {
		t.Fatalf("marshal ref emit: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal ref emit: %v", err)
	}
	return m
}

// TestRefPriceFreshEmit — a fresh oracle observation emits price,
// provenance, expiry metadata and derived collars.
func TestRefPriceFreshEmit(t *testing.T) {
	src := &countingRefSource{ref: OracleRef{
		Price: 108_000_000, Source: "composite", ValidAt: time.Now(),
	}}
	rules := ExecutionRuleFunc(func(context.Context, string) (ExecutionRule, error) {
		return ExecutionRule{BuyLimitBps: 50, SellLimitBps: 50, ExpiryMs: 30_000}, nil
	})
	_, sink := runRefStream(t, src, rules,
		func() []string { return []string{"EUR/USD"} }, NewMemSeqStore())

	sink.waitFor(t, 1)
	row := sink.get(0)
	if row.channel != "referencePrice@EUR/USD" {
		t.Fatalf("channel = %q", row.channel)
	}
	m := decodeRef(t, row)
	if m["stale"] != false {
		t.Fatalf("fresh frame marked stale: %v", m)
	}
	if m["price"] != "1.08" {
		t.Fatalf("price = %v, want 1.08 (1e-8 scale)", m["price"])
	}
	if m["buy_limit"] != "1.0854" || m["sell_limit"] != "1.0746" {
		t.Fatalf("collars = %v/%v (50bps around 1.08)", m["buy_limit"], m["sell_limit"])
	}
	if m["source"] != "composite" || m["valid_at_ms"] == nil || m["expires_at_ms"] == nil {
		t.Fatalf("provenance/expiry fields missing: %v", m)
	}
	if m["rule_expiry_ms"].(float64) != 30_000 {
		t.Fatalf("rule expiry = %v", m["rule_expiry_ms"])
	}
}

// TestRefPriceStaleFailClosed — an observation older than the 5s gate
// emits NO usable price: stale:true + provenance + staleness metadata
// only (spec fail-closed — CONDITIONAL_TRIGGER_ORACLE_STALE lineage).
func TestRefPriceStaleFailClosed(t *testing.T) {
	src := &countingRefSource{ref: OracleRef{
		Price:   108_000_000,
		Source:  "refinitiv",
		ValidAt: time.Now().Add(-10 * time.Second), // 5s gate breached
	}}
	rs, sink := runRefStream(t, src, nil,
		func() []string { return []string{"EUR/USD"} }, NewMemSeqStore())

	sink.waitFor(t, 1)
	m := decodeRef(t, sink.get(0))
	if m["stale"] != true || m["reason"] != "reference_stale" {
		t.Fatalf("stale frame = %v", m)
	}
	if _, ok := m["price"]; ok {
		t.Fatalf("stale frame carried a usable price: %v", m["price"])
	}
	if _, ok := m["buy_limit"]; ok {
		t.Fatal("stale frame carried collars — fail-closed violated")
	}
	if m["source"] != "refinitiv" || m["valid_at_ms"] == nil {
		t.Fatalf("stale frame missing provenance: %v", m)
	}
	if m["stale_ms"].(float64) < 5_000 {
		t.Fatalf("stale_ms = %v", m["stale_ms"])
	}
	if rs.m.RefPriceStaleEmits.Load() < 1 {
		t.Fatal("RefPriceStaleEmits counter did not move")
	}
}

// TestRefPriceOracleUnavailable — a source error emits the fail-closed
// marker with reason oracle_unavailable (never a fabricated price).
func TestRefPriceOracleUnavailable(t *testing.T) {
	src := &countingRefSource{err: errors.New("oracle down")}
	_, sink := runRefStream(t, src, nil,
		func() []string { return []string{"EUR/USD"} }, NewMemSeqStore())
	sink.waitFor(t, 1)
	m := decodeRef(t, sink.get(0))
	if m["stale"] != true || m["reason"] != "oracle_unavailable" {
		t.Fatalf("unavailable frame = %v", m)
	}
	if _, ok := m["price"]; ok {
		t.Fatal("unavailable frame carried a price")
	}
}

// TestRefPriceNoSubscribersNoPoll — the stream polls only symbols with
// live subscriptions (no consumer, no oracle traffic).
func TestRefPriceNoSubscribersNoPoll(t *testing.T) {
	src := &countingRefSource{ref: OracleRef{
		Price: 1, Source: "ecb", ValidAt: time.Now(),
	}}
	_, sink := runRefStream(t, src, nil,
		func() []string { return nil }, NewMemSeqStore())
	time.Sleep(60 * time.Millisecond) // several poll ticks
	if src.n() != 0 {
		t.Fatalf("oracle polled %d times with no subscribers", src.n())
	}
	_ = sink
}

// TestRefPriceSnapshot — the stream is a SnapshotSource: a resynced
// client sees the latest frame (stale marker included — the snapshot
// must not pretend freshness).
func TestRefPriceSnapshot(t *testing.T) {
	src := &countingRefSource{ref: OracleRef{
		Price: 108_000_000, Source: "ecb", ValidAt: time.Now(),
	}}
	rs, sink := runRefStream(t, src, nil,
		func() []string { return []string{"EUR/USD"} }, NewMemSeqStore())
	sink.waitFor(t, 1)

	seq, data, err := rs.Snapshot(context.Background(), "referencePrice@EUR/USD")
	if err != nil || seq == 0 {
		t.Fatalf("snapshot: seq=%d err=%v", seq, err)
	}
	u, ok := data.(refPriceUpdate)
	if !ok || u.Price != "1.08" || u.Stale {
		t.Fatalf("snapshot payload %+v", data)
	}
	if _, _, err := rs.Snapshot(context.Background(), "referencePrice@USD/JPY"); err == nil {
		t.Fatal("snapshot for unobserved symbol accepted")
	}
	if _, _, err := rs.Snapshot(context.Background(), "book@EUR/USD"); err == nil {
		t.Fatal("snapshot on wrong channel type accepted")
	}
}

// TestRefPriceSeqRestartContinuity — the md:seq:referencePrice cursor
// survives a stream restart (Task 6.3.22).
func TestRefPriceSeqRestartContinuity(t *testing.T) {
	store := NewMemSeqStore()
	src := &countingRefSource{ref: OracleRef{
		Price: 108_000_000, Source: "ecb", ValidAt: time.Now(),
	}}
	syms := func() []string { return []string{"EUR/USD"} }
	rs1, sink1 := runRefStream(t, src, nil, syms, store)
	sink1.waitFor(t, 2)
	waitStore(t, store, "referencePrice:EUR/USD", 2)

	// Second stream instance = restart: cursor must continue, not reset.
	rs2, sink2 := runRefStream(t, src, nil, syms, store)
	sink2.waitFor(t, 1)
	m := decodeRef(t, sink2.get(0))
	if int(m["seq"].(float64)) < 3 {
		t.Fatalf("post-restart seq = %v — cursor reset", m["seq"])
	}
	_ = rs1
	_ = rs2
}

// TestGapJournalBounded — the journal retains a bounded newest-first
// window per key (Task 6.3.22: bounded gap log, 1024 cap).
func TestGapJournalBounded(t *testing.T) {
	j := NewMemGapJournal()
	ctx := context.Background()
	for i := 0; i < gapJournalCap+50; i++ {
		if err := j.Record(ctx, "EUR/USD", SeqGap{
			From: uint64(i), To: uint64(i), Reason: GapInputSaturation,
		}); err != nil {
			t.Fatalf("record: %v", err)
		}
	}
	l, err := j.Recent(ctx, "EUR/USD", 0)
	if err != nil {
		t.Fatalf("recent: %v", err)
	}
	if len(l) != gapJournalCap {
		t.Fatalf("journal len = %d, want cap %d", len(l), gapJournalCap)
	}
	// Newest first.
	if l[0].From != uint64(gapJournalCap+49) {
		t.Fatalf("newest entry = %d, want %d", l[0].From, gapJournalCap+49)
	}
	if n, _ := j.Recent(ctx, "EUR/USD", 5); len(n) != 5 {
		t.Fatalf("limit ignored: %d", len(n))
	}
}

// TestSeqRestartBoundaryJournaled — a conflator restart on a pre-seeded
// store journals the restart boundary (audit evidence for §10.7).
func TestSeqRestartBoundaryJournaled(t *testing.T) {
	store := NewMemSeqStore()
	if err := store.Store(context.Background(), "EUR/USD", 42); err != nil {
		t.Fatalf("seed: %v", err)
	}
	j := NewMemGapJournal()
	c, sink := runConflator(t, ConflatorConfig{
		Window: 20 * time.Millisecond, MaxEvents: 1, Journal: j,
	}, store)
	c.Push(mkDelta("EUR/USD", 108_000_000))
	sink.waitFor(t, 2)

	u := decodeUpdate(t, sink.get(0))
	if u.LastSeq != 43 || u.PrevLastSeq != 42 {
		t.Fatalf("post-restart envelope = %+v — cursor must continue at 43", u)
	}
	gaps, _ := j.Recent(context.Background(), "EUR/USD", 0)
	found := false
	for _, g := range gaps {
		if g.Reason == GapRestartBoundary && g.From == 42 {
			found = true
		}
	}
	if !found {
		t.Fatalf("restart boundary not journaled: %+v", gaps)
	}
}
