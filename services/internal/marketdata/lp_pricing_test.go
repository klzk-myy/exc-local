package marketdata

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"exchange/pkg/decimal"
)

func lpDec(t *testing.T, s string) decimal.Decimal {
	t.Helper()
	d, err := decimal.NewFromString(s)
	if err != nil {
		t.Fatalf("decimal %q: %v", s, err)
	}
	return d
}

func lpCfg(lpID, instrID int64, sym string) LPPricingConfig {
	return LPPricingConfig{
		LPID: lpID, Status: "ACTIVE", LPStalenessMS: 5000,
		InstrumentID: instrID, Symbol: sym, Enabled: true,
		SpreadMarkupBidBps: decimal.Zero, SpreadMarkupAskBps: decimal.Zero,
		SkewBps: decimal.Zero, StalenessTimeoutMS: 5000,
	}
}

func lpQuote(now time.Time) LPQuote {
	return LPQuote{
		LPID: 7, InstrumentID: 3, Symbol: "EUR/USD",
		Bids: []LPQuoteLevel{{Price: lpDecForTest("1.08000"), Qty: lpDecForTest("1000000")}},
		Asks: []LPQuoteLevel{{Price: lpDecForTest("1.08200"), Qty: lpDecForTest("2000000")}},
		Seq:  42, Ts: now,
	}
}

func lpDecForTest(s string) decimal.Decimal {
	return decimal.RequireFromString(s)
}

func TestLPPriceFilterMarkupSkew(t *testing.T) {
	now := time.Now()
	f := NewLPPriceFilter(nil, nil)
	cfg := lpCfg(7, 3, "EUR/USD")
	cfg.SpreadMarkupBidBps = lpDecForTest("5")  // +5 bps on the bid
	cfg.SpreadMarkupAskBps = lpDecForTest("10") // −10 bps on the ask
	cfg.SkewBps = lpDecForTest("2")             // +2 bps mid shift
	f.SetConfigs([]LPPricingConfig{cfg})

	adj, got, reason := f.Apply(lpQuote(now), now)
	if reason != "" {
		t.Fatalf("apply dropped: %s", reason)
	}
	if got.LPID != 7 || got.InstrumentID != 3 || got.Symbol != "EUR/USD" {
		t.Fatalf("config identity: %+v", got)
	}
	// bid′ = 1.08000 × (1 + (2+5)/1e4) = 1.08000 × 1.0007 = 1.080756
	if want := "1.080756"; adj.Bids[0].Price.String() != want {
		t.Fatalf("bid = %s, want %s", adj.Bids[0].Price, want)
	}
	// ask′ = 1.08200 × (1 + (2−10)/1e4) = 1.08200 × 0.9992 = 1.0811344
	if want := "1.0811344"; adj.Asks[0].Price.String() != want {
		t.Fatalf("ask = %s, want %s", adj.Asks[0].Price, want)
	}
	// Quantities pass through untouched.
	if adj.Bids[0].Qty.String() != "1000000" || adj.Asks[0].Qty.String() != "2000000" {
		t.Fatalf("qty mutated: %+v", adj)
	}
	// Input must not be mutated.
	in := lpQuote(now)
	_, _, _ = f.Apply(in, now)
	if in.Bids[0].Price.String() != "1.08" && in.Bids[0].Price.String() != "1.08000" {
		t.Fatalf("input mutated: %s", in.Bids[0].Price)
	}
}

func TestLPPriceFilterNegativeSkew(t *testing.T) {
	now := time.Now()
	f := NewLPPriceFilter(nil, nil)
	cfg := lpCfg(7, 3, "EUR/USD")
	cfg.SkewBps = lpDecForTest("-3") // −3 bps mid shift, no markup
	f.SetConfigs([]LPPricingConfig{cfg})

	adj, _, reason := f.Apply(lpQuote(now), now)
	if reason != "" {
		t.Fatalf("apply dropped: %s", reason)
	}
	// bid′ = 1.08000 × 0.9997 = 1.079676
	if want := "1.079676"; adj.Bids[0].Price.String() != want {
		t.Fatalf("bid = %s, want %s", adj.Bids[0].Price, want)
	}
	// ask′ = 1.08200 × 0.9997 = 1.0816754
	if want := "1.0816754"; adj.Asks[0].Price.String() != want {
		t.Fatalf("ask = %s, want %s", adj.Asks[0].Price, want)
	}
}

func TestLPPriceFilterGates(t *testing.T) {
	now := time.Now()
	f := NewLPPriceFilter(nil, nil)
	f.SetConfigs([]LPPricingConfig{
		lpCfg(7, 3, "EUR/USD"),
		func() LPPricingConfig {
			c := lpCfg(8, 3, "EUR/USD")
			c.Status = "SUSPENDED"
			return c
		}(),
		func() LPPricingConfig {
			c := lpCfg(9, 3, "EUR/USD")
			c.Enabled = false
			return c
		}(),
		func() LPPricingConfig {
			c := lpCfg(10, 3, "EUR/USD")
			c.SpreadMarkupAskBps = lpDecForTest("60") // narrows 1.08200 → 1.075508 < bid
			return c
		}(),
	})

	q := lpQuote(now)
	q.LPID = 77 // no config row at all
	if _, _, r := f.Apply(q, now); r != LPDropUnconfigured {
		t.Fatalf("unknown lp = %q, want unconfigured", r)
	}
	q.LPID = 7
	q.InstrumentID = 99 // unconfigured instrument for a known LP
	if _, _, r := f.Apply(q, now); r != LPDropUnconfigured {
		t.Fatalf("unknown instrument = %q, want unconfigured", r)
	}
	q = lpQuote(now)
	q.LPID = 8
	if _, _, r := f.Apply(q, now); r != LPDropLPInactive {
		t.Fatalf("suspended = %q, want lp_inactive", r)
	}
	q.LPID = 9
	if _, _, r := f.Apply(q, now); r != LPDropDisabled {
		t.Fatalf("disabled = %q, want disabled", r)
	}
	q.LPID = 10
	if _, _, r := f.Apply(q, now); r != LPDropCrossed {
		t.Fatalf("crossed = %q, want crossed", r)
	}
	q = lpQuote(now)
	q.Ts = now.Add(-10 * time.Second) // past the 5s gate
	if _, _, r := f.Apply(q, now); r != LPDropStale {
		t.Fatalf("stale = %q, want stale", r)
	}
	q.Ts = time.Time{} // zero ts is never fresh
	if _, _, r := f.Apply(q, now); r != LPDropStale {
		t.Fatalf("zero ts = %q, want stale", r)
	}
}

func TestLPPriceFilterStaleTimeoutPrecedence(t *testing.T) {
	now := time.Now()
	f := NewLPPriceFilter(nil, nil)

	instr := lpCfg(7, 3, "EUR/USD")
	instr.StalenessTimeoutMS = 200 // per-instrument tightens
	instr.LPStalenessMS = 5000
	f.SetConfigs([]LPPricingConfig{instr})

	q := lpQuote(now)
	q.Ts = now.Add(-300 * time.Millisecond)
	if _, _, r := f.Apply(q, now); r != LPDropStale {
		t.Fatalf("300ms vs 200ms instrument gate = %q, want stale", r)
	}
	q.Ts = now.Add(-100 * time.Millisecond)
	if _, _, r := f.Apply(q, now); r != "" {
		t.Fatalf("100ms vs 200ms gate = %q, want pass", r)
	}

	// Instrument 0 → LP-level default.
	lpLevel := lpCfg(7, 4, "USD/JPY")
	lpLevel.StalenessTimeoutMS = 0
	lpLevel.LPStalenessMS = 50
	f.SetConfigs([]LPPricingConfig{lpLevel})
	q = lpQuote(now)
	q.InstrumentID, q.Symbol = 4, "USD/JPY"
	q.Ts = now.Add(-100 * time.Millisecond)
	if _, _, r := f.Apply(q, now); r != LPDropStale {
		t.Fatalf("100ms vs 50ms LP gate = %q, want stale", r)
	}

	// Both 0 → the 5s venue default.
	def := lpCfg(7, 5, "GBP/USD")
	def.StalenessTimeoutMS, def.LPStalenessMS = 0, 0
	f.SetConfigs([]LPPricingConfig{def})
	q.InstrumentID, q.Symbol = 5, "GBP/USD"
	q.Ts = now.Add(-4 * time.Second)
	if _, _, r := f.Apply(q, now); r != "" {
		t.Fatalf("4s vs 5s default = %q, want pass", r)
	}
	q.Ts = now.Add(-6 * time.Second)
	if _, _, r := f.Apply(q, now); r != LPDropStale {
		t.Fatalf("6s vs 5s default = %q, want stale", r)
	}
}

func TestLPPriceFilterSymbolLookupAndUnresolved(t *testing.T) {
	now := time.Now()
	f := NewLPPriceFilter(nil, nil)
	f.SetConfigs([]LPPricingConfig{lpCfg(7, 3, "EUR/USD")})

	// Symbol-keyed quote (no instrument_id) resolves via bySym.
	q := lpQuote(now)
	q.InstrumentID = 0
	adj, cfg, r := f.Apply(q, now)
	if r != "" {
		t.Fatalf("symbol lookup = %q, want pass", r)
	}
	if adj.InstrumentID != 3 || cfg.InstrumentID != 3 {
		t.Fatalf("resolved instrument = %d, want 3", adj.InstrumentID)
	}

	// Config row with no resolved symbol + symbol-less quote → unresolved.
	f.SetConfigs([]LPPricingConfig{lpCfg(7, 3, "")})
	q = lpQuote(now)
	q.Symbol = ""
	if _, _, r := f.Apply(q, now); r != LPDropUnresolved {
		t.Fatalf("symbol-less = %q, want unresolved", r)
	}
}

func TestLPPriceFilterNonPositive(t *testing.T) {
	now := time.Now()
	f := NewLPPriceFilter(nil, nil)
	cfg := lpCfg(7, 3, "EUR/USD")
	cfg.SkewBps = lpDecForTest("-10000") // −100% shift → price 0
	f.SetConfigs([]LPPricingConfig{cfg})
	if _, _, r := f.Apply(lpQuote(now), now); r != LPDropNonPositive {
		t.Fatalf("−100%% skew = %q, want nonpositive", r)
	}
}

func TestLPPriceFilterEmptyBookWithdrawalPasses(t *testing.T) {
	now := time.Now()
	f := NewLPPriceFilter(nil, nil)
	f.SetConfigs([]LPPricingConfig{lpCfg(7, 3, "EUR/USD")})
	q := lpQuote(now)
	q.Bids, q.Asks = nil, nil // LP pulled both sides — a real update
	adj, _, r := f.Apply(q, now)
	if r != "" || len(adj.Bids) != 0 || len(adj.Asks) != 0 {
		t.Fatalf("empty book = %q %+v, want pass-through", r, adj)
	}
}

func TestLPPriceFilterReloadKeepsLastGood(t *testing.T) {
	f := NewLPPriceFilter(nil, nil)
	f.SetConfigs([]LPPricingConfig{lpCfg(7, 3, "EUR/USD")})
	if !f.Loaded() {
		t.Fatal("loaded flag not set")
	}
	// Swap in a failing source — reload errors, snapshot survives.
	f.src = LPConfigSourceFunc(func(context.Context) ([]LPPricingConfig, error) {
		return nil, errors.New("pg down")
	})
	if err := f.Reload(context.Background()); err == nil {
		t.Fatal("reload error swallowed")
	}
	if _, ok := f.ConfigFor(7, 3); !ok {
		t.Fatal("last-good snapshot lost on reload failure")
	}
	// Recovery restores the new snapshot.
	f.src = LPConfigSourceFunc(func(context.Context) ([]LPPricingConfig, error) {
		return []LPPricingConfig{lpCfg(8, 4, "USD/JPY")}, nil
	})
	if err := f.Reload(context.Background()); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if _, ok := f.ConfigFor(8, 4); !ok {
		t.Fatal("reloaded snapshot missing")
	}
	if _, ok := f.ConfigFor(7, 3); ok {
		t.Fatal("stale snapshot retained")
	}
}

// ---------------------------------------------------------------------------
// LPBookProducer
// ---------------------------------------------------------------------------

type lpEmitSink struct {
	channels []string
	frames   []lpBookFrame
}

func (s *lpEmitSink) emit(ch string, _ uint64, data any) {
	s.channels = append(s.channels, ch)
	if f, ok := data.(lpBookFrame); ok {
		s.frames = append(s.frames, f)
	}
}

func newLPBookTestProducer(now func() time.Time) (*LPBookProducer, *LPPriceFilter, *lpEmitSink) {
	f := NewLPPriceFilter(nil, nil)
	sink := &lpEmitSink{}
	p := NewLPBookProducer(LPBookProducerConfig{
		Now: now, RefreshInterval: -1, StaleSweep: -1,
	}, nil, f, sink.emit)
	return p, f, sink
}

func TestLPBookProducerEmitsAdjusted(t *testing.T) {
	now := time.Now()
	p, f, sink := newLPBookTestProducer(func() time.Time { return now })
	cfg := lpCfg(7, 3, "EUR/USD")
	cfg.SpreadMarkupBidBps = lpDecForTest("5")
	f.SetConfigs([]LPPricingConfig{cfg})

	p.Push(lpQuote(now))
	q := <-p.in // drain ordering not needed — process synchronously below
	p.process(q, now)

	if len(sink.channels) != 1 || sink.channels[0] != "lpBook@7/EUR/USD" {
		t.Fatalf("channels = %v", sink.channels)
	}
	fr := sink.frames[0]
	if fr.Event != "lpBook" || fr.LPID != 7 || fr.InstrumentID != 3 ||
		fr.Symbol != "EUR/USD" || fr.Stale {
		t.Fatalf("frame = %+v", fr)
	}
	if fr.Bids[0][0] != "1.08054" { // 1.08000 × 1.0005
		t.Fatalf("adjusted bid = %s", fr.Bids[0][0])
	}
	if fr.Asks[0][0] != "1.082" { // no ask markup
		t.Fatalf("adjusted ask = %s", fr.Asks[0][0])
	}
	if fr.Seq != 1 || fr.QuoteSeq != 42 {
		t.Fatalf("seqs = %d/%d", fr.Seq, fr.QuoteSeq)
	}
}

func TestLPBookProducerStaleQuoteWithdraws(t *testing.T) {
	now := time.Now()
	clk := now
	p, f, sink := newLPBookTestProducer(func() time.Time { return clk })
	f.SetConfigs([]LPPricingConfig{lpCfg(7, 3, "EUR/USD")})

	p.process(lpQuote(now), now)
	if len(sink.frames) != 1 || sink.frames[0].Stale {
		t.Fatalf("live emit missing: %+v", sink.frames)
	}
	// A quote already past its timeout withdraws the book — once.
	p.process(LPQuote{LPID: 7, InstrumentID: 3, Ts: now.Add(-10 * time.Second)}, now)
	if len(sink.frames) != 2 {
		t.Fatalf("frames = %d, want 2", len(sink.frames))
	}
	w := sink.frames[1]
	if !w.Stale || w.Reason != LPDropStale || len(w.Bids) != 0 || len(w.Asks) != 0 {
		t.Fatalf("withdrawal frame = %+v", w)
	}
	if w.Seq <= sink.frames[0].Seq {
		t.Fatalf("seq regression %d → %d", sink.frames[0].Seq, w.Seq)
	}
	// Second stale quote — already withdrawn, no spam.
	p.process(LPQuote{LPID: 7, InstrumentID: 3, Ts: now.Add(-9 * time.Second)}, now)
	if len(sink.frames) != 2 {
		t.Fatalf("duplicate withdrawal emitted: %d frames", len(sink.frames))
	}
}

func TestLPBookProducerRefreshWithdraws(t *testing.T) {
	now := time.Now()
	p, f, sink := newLPBookTestProducer(func() time.Time { return now })
	f.SetConfigs([]LPPricingConfig{lpCfg(7, 3, "EUR/USD")})
	p.process(lpQuote(now), now)

	// Admin flips enabled=false → refresh withdraws the live book.
	f.src = LPConfigSourceFunc(func(context.Context) ([]LPPricingConfig, error) {
		c := lpCfg(7, 3, "EUR/USD")
		c.Enabled = false
		return []LPPricingConfig{c}, nil
	})
	if err := p.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if len(sink.frames) != 2 {
		t.Fatalf("frames = %d, want 2", len(sink.frames))
	}
	w := sink.frames[1]
	if !w.Stale || w.Reason != LPDropDisabled {
		t.Fatalf("withdrawal = %+v, want disabled", w)
	}
	// New quotes for the disabled row are gated and stay silent.
	p.process(lpQuote(now), now)
	if len(sink.frames) != 2 {
		t.Fatalf("disabled quote emitted: %d frames", len(sink.frames))
	}
}

func TestLPBookProducerSweepStale(t *testing.T) {
	now := time.Now()
	clk := now
	p, f, sink := newLPBookTestProducer(func() time.Time { return clk })
	cfg := lpCfg(7, 3, "EUR/USD")
	cfg.StalenessTimeoutMS = 1000
	f.SetConfigs([]LPPricingConfig{cfg})

	p.process(lpQuote(now), now)
	clk = now.Add(2 * time.Second) // feed went silent past the timeout
	p.sweepStale(clk)
	if len(sink.frames) != 2 || !sink.frames[1].Stale ||
		sink.frames[1].Reason != LPDropStale {
		t.Fatalf("sweep = %+v", sink.frames)
	}
	// Sweep is idempotent on an already-stale book.
	p.sweepStale(clk.Add(time.Minute))
	if len(sink.frames) != 2 {
		t.Fatalf("sweep re-emitted: %d", len(sink.frames))
	}
}

func TestLPBookProducerSnapshot(t *testing.T) {
	now := time.Now()
	p, f, sink := newLPBookTestProducer(func() time.Time { return now })
	f.SetConfigs([]LPPricingConfig{lpCfg(7, 3, "EUR/USD")})
	p.process(lpQuote(now), now)
	_ = sink

	seq, data, err := p.Snapshot(context.Background(), "lpBook@7/EUR/USD")
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if seq != 1 {
		t.Fatalf("snapshot seq = %d", seq)
	}
	fr, ok := data.(lpBookFrame)
	if !ok || fr.Symbol != "EUR/USD" || fr.Stale {
		t.Fatalf("snapshot frame = %+v", data)
	}
	if _, _, err := p.Snapshot(context.Background(), "lpBook@99/EUR/USD"); err == nil {
		t.Fatal("snapshot for never-live channel must fail")
	}
	if _, _, err := p.Snapshot(context.Background(), "lpBook@abc/EUR/USD"); err == nil {
		t.Fatal("snapshot for malformed channel must fail")
	}
	if _, _, err := p.Snapshot(context.Background(), "book@EUR/USD"); err == nil {
		t.Fatal("snapshot must reject non-lpBook channels")
	}
}

func TestLPBookProducerOnDrop(t *testing.T) {
	now := time.Now()
	p, f, _ := newLPBookTestProducer(func() time.Time { return now })
	f.SetConfigs([]LPPricingConfig{lpCfg(7, 3, "EUR/USD")})
	var drops []string
	p.OnDrop = func(r string) { drops = append(drops, r) }
	p.process(LPQuote{LPID: 8, InstrumentID: 3, Symbol: "EUR/USD", Ts: now}, now)
	if len(drops) != 1 || drops[0] != LPDropUnconfigured {
		t.Fatalf("drops = %v", drops)
	}
}

// ---------------------------------------------------------------------------
// Channel grammar + JSON ingress
// ---------------------------------------------------------------------------

func TestParseChannelLPBook(t *testing.T) {
	ch, err := ParseChannel("lpBook@7/EUR/USD")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if ch.Type != "lpBook" || ch.LPID != 7 || ch.Target != "EUR/USD" ||
		ch.Class != ClassL2 {
		t.Fatalf("channel = %+v", ch)
	}
	for _, bad := range []string{
		"lpBook@EUR/USD",   // no lp id — first segment isn't numeric
		"lpBook@7",         // no symbol
		"lpBook@0/EUR/USD", // lp_id must be positive
		"lpBook@-3/EUR/USD",
		"lpBook@7/EUR/USD:1", // params tail not allowed on lpBook
	} {
		if _, err := ParseChannel(bad); err == nil {
			t.Fatalf("parse %q must fail", bad)
		}
	}
}

func TestDecodeLPQuoteJSON(t *testing.T) {
	body := []byte(`{"lp_id":7,"instrument_id":3,"symbol":"EUR/USD",` +
		`"bids":[["1.08000","1000000"],["1.07990","500000"]],` +
		`"asks":[["1.08020","2000000"]],"seq":42,` +
		`"ts":"2026-01-02T03:04:05.123456789Z"}`)
	q, err := DecodeLPQuoteJSON(body, "", nil)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if q.LPID != 7 || q.InstrumentID != 3 || q.Symbol != "EUR/USD" ||
		q.Seq != 42 || len(q.Bids) != 2 || len(q.Asks) != 1 {
		t.Fatalf("quote = %+v", q)
	}
	if q.Bids[1].Price.String() != "1.0799" || q.Asks[0].Qty.String() != "2000000" {
		t.Fatalf("levels = %+v / %+v", q.Bids, q.Asks)
	}
	if q.Ts.Unix() != 1767323045 {
		t.Fatalf("ts = %v", q.Ts)
	}

	// Subject fallback fills lp_id + symbol: lp_quotes.7.EUR-USD.
	body = []byte(`{"instrument_id":3,"bids":[["1.1","1"]],"asks":[],"ts_ms":1767323045123}`)
	q, err = DecodeLPQuoteJSON(body, "lp_quotes.7.EUR-USD",
		SubjectSymbols(MapResolver{3: "EUR/USD"}))
	if err != nil {
		t.Fatalf("decode subject: %v", err)
	}
	if q.LPID != 7 || q.Symbol != "EUR/USD" || q.Ts.UnixMilli() != 1767323045123 {
		t.Fatalf("subject-resolved quote = %+v", q)
	}

	if _, err := DecodeLPQuoteJSON([]byte(`{bad`), "", nil); err == nil {
		t.Fatal("malformed json must fail")
	}
	if _, err := DecodeLPQuoteJSON(
		[]byte(`{"bids":[["nope","1"]]}`), "", nil); err == nil {
		t.Fatal("bad price must fail")
	}
	if _, err := DecodeLPQuoteJSON(
		[]byte(`{"ts":"not-a-time"}`), "", nil); err == nil {
		t.Fatal("bad ts must fail")
	}
}

func TestLPBookChannelToken(t *testing.T) {
	if got := LPBookChannel(12, "USD/JPY"); got != "lpBook@12/USD/JPY" {
		t.Fatalf("channel = %q", got)
	}
	if _, err := ParseChannel(LPBookChannel(12, "USD/JPY")); err != nil {
		t.Fatalf("rendered channel must parse: %v", err)
	}
}

func TestLPBookProducerSymbolKeyedQuote(t *testing.T) {
	now := time.Now()
	p, f, sink := newLPBookTestProducer(func() time.Time { return now })
	f.SetConfigs([]LPPricingConfig{lpCfg(7, 3, "EUR/USD")})
	// Symbol-only quote (instrument_id 0) still resolves via bySym.
	p.process(LPQuote{
		LPID: 7, Symbol: "EUR/USD", Ts: now,
		Bids: []LPQuoteLevel{{Price: lpDecForTest("1.08"), Qty: lpDecForTest("1")}},
		Asks: []LPQuoteLevel{{Price: lpDecForTest("1.09"), Qty: lpDecForTest("1")}},
	}, now)
	if len(sink.channels) != 1 || sink.channels[0] != "lpBook@7/EUR/USD" {
		t.Fatalf("channels = %v", sink.channels)
	}
	if sink.frames[0].InstrumentID != 3 {
		t.Fatalf("instrument = %d", sink.frames[0].InstrumentID)
	}
}

func ExampleLPBookProducer() {
	f := NewLPPriceFilter(nil, nil)
	f.SetConfigs([]LPPricingConfig{{
		LPID: 7, Status: "ACTIVE", InstrumentID: 3, Symbol: "EUR/USD",
		Enabled: true, SpreadMarkupBidBps: decimal.RequireFromString("5"),
		StalenessTimeoutMS: 5000,
	}})
	p := NewLPBookProducer(LPBookProducerConfig{RefreshInterval: -1, StaleSweep: -1},
		nil, f, func(ch string, seq uint64, data any) {
			fmt.Println(ch, data.(lpBookFrame).Bids[0][0])
		})
	p.process(LPQuote{
		LPID: 7, InstrumentID: 3, Ts: time.Now(),
		Bids: []LPQuoteLevel{{Price: decimal.RequireFromString("1.08000"),
			Qty: decimal.RequireFromString("1")}},
	}, time.Now())
	// Output: lpBook@7/EUR/USD 1.08054
}
