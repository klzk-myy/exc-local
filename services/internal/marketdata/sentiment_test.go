// Phase-23 Task 23.3.6 tests — sentiment@ channel, delay/cohort
// invariants, privacy serialization, series aggregation (§10.8, §24
// #276). Sources are fakes; the PG path is covered by EXC_PG_TEST-gated
// suites.
package marketdata

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"exchange/pkg/decimal"
)

// ---------------------------------------------------------------------------
// Fakes
// ---------------------------------------------------------------------------

type fakeCohortSource struct {
	c     PositionCohort
	err   error
	calls int
	got   string
}

func (f *fakeCohortSource) Cohort(_ context.Context, sym string) (PositionCohort, error) {
	f.calls++
	f.got = sym
	return f.c, f.err
}

type fakeTakerFlowSource struct {
	f              TakerFlow
	err            error
	calls          int
	gotFrom, gotTo time.Time
	gotSym         string
}

func (f *fakeTakerFlowSource) TakerFlow(_ context.Context, sym string,
	from, to time.Time) (TakerFlow, error) {
	f.calls++
	f.gotSym, f.gotFrom, f.gotTo = sym, from, to
	return f.f, f.err
}

func testClock(t0 time.Time) (func() time.Time, *time.Time) {
	cur := t0
	return func() time.Time { return cur }, &cur
}

func healthyCohort(sym string, asOf time.Time) PositionCohort {
	return PositionCohort{
		Symbol: sym, AsOf: asOf,
		Accounts: 150, LongAccounts: 90, ShortAccounts: 60,
		LongNotional:  decimal.NewFromInt(9_000_000),
		ShortNotional: decimal.NewFromInt(6_000_000),
		GrossNotional: decimal.NewFromInt(15_000_000),
	}
}

func newTestProducer(now func() time.Time, pos PositionCohortSource,
	flow TakerFlowSource) (*SentimentProducer, *emitter) {
	em := &emitter{}
	p := NewSentimentProducer(SentimentProducerConfig{
		Now: now, Symbols: []string{"EUR/USD"},
	}, pos, flow, em.fn())
	return p, em
}

// ---------------------------------------------------------------------------
// Channel grammar
// ---------------------------------------------------------------------------

func TestSentimentChannelParses(t *testing.T) {
	ch, err := ParseChannel("sentiment@EUR/USD")
	if err != nil {
		t.Fatalf("ParseChannel: %v", err)
	}
	if ch.Type != "sentiment" || ch.Target != "EUR/USD" {
		t.Fatalf("channel=%+v", ch)
	}
	if ch.Private {
		t.Fatal("sentiment must be a public channel")
	}
}

func TestSentimentChannelRegistrationIdempotent(t *testing.T) {
	RegisterSentimentChannels()
	RegisterSentimentChannels()
	if _, err := ParseChannel("sentiment@GBP/USD"); err != nil {
		t.Fatalf("re-registration broke the grammar: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Delay invariant (§10.8 item 2 — "time-delayed by 5 minutes")
// ---------------------------------------------------------------------------

func TestSentimentDelayEnforced(t *testing.T) {
	t0 := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	now, cur := testClock(t0)
	p, em := newTestProducer(now, &fakeCohortSource{}, nil)

	// A cohort sampled at t0 must not publish while t0 is inside the
	// 5-minute horizon.
	p.Push(healthyCohort("EUR/USD", t0))
	p.tick(t0)
	evts := em.all()
	if len(evts) != 1 {
		t.Fatalf("expected 1 frame, got %d", len(evts))
	}
	ls := payload(t, evts[0])["long_short"].(map[string]any)
	if ls["suppressed"] != true || ls["reason"] != "delay_horizon" {
		t.Fatalf("fresh sample must not publish: %v", ls)
	}
	if _, leaked := ls["long_ratio"]; leaked {
		t.Fatal("delay_horizon frame leaked ratios")
	}

	// Advance the wall clock past the horizon; the SAME t0 sample now
	// publishes — the public surface is anchored at now−5m.
	*cur = t0.Add(6 * time.Minute)
	p.tick(*cur)
	ls = payload(t, em.all()[1])["long_short"].(map[string]any)
	if ls["suppressed"] == true {
		t.Fatalf("aged sample suppressed: %v", ls)
	}
	if ls["long_ratio"] != "0.600000" || ls["short_ratio"] != "0.400000" {
		t.Fatalf("ratios wrong: %v", ls)
	}
	m := payload(t, em.all()[1])
	if m["as_of_ms"].(float64) != float64(t0.UnixMilli()) {
		t.Fatalf("as_of_ms=%v want the sample time, not tick time", m["as_of_ms"])
	}
	if m["delay_ms"].(float64) != float64(300000) {
		t.Fatalf("delay_ms=%v", m["delay_ms"])
	}
}

func TestSentimentTakerFlowWindowBounded(t *testing.T) {
	t0 := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	now, _ := testClock(t0)
	fs := &fakeTakerFlowSource{f: TakerFlow{
		BuyNotional:  decimal.NewFromInt(4000),
		SellNotional: decimal.NewFromInt(2000),
		Trades:       500, Accounts: 250,
	}}
	p, em := newTestProducer(now, nil, fs)
	p.Push(healthyCohort("EUR/USD", t0.Add(-10*time.Minute)))
	p.tick(t0)

	if fs.calls != 1 {
		t.Fatalf("flow calls=%d", fs.calls)
	}
	// Window = [horizon−5m, horizon] where horizon = now−5m.
	wantTo := t0.Add(-SentimentPublicationDelay)
	if !fs.gotTo.Equal(wantTo) {
		t.Fatalf("flow window end=%v want %v (delay horizon)", fs.gotTo, wantTo)
	}
	if !fs.gotFrom.Equal(wantTo.Add(-5 * time.Minute)) {
		t.Fatalf("flow window start=%v", fs.gotFrom)
	}
	tf := payload(t, em.all()[0])["taker_flow"].(map[string]any)
	if tf["buy_sell_ratio"] != "2.000000" {
		t.Fatalf("buy_sell_ratio=%v", tf)
	}
}

// ---------------------------------------------------------------------------
// Cohort floor (§24 #276 — suppress below 100 accounts)
// ---------------------------------------------------------------------------

func TestSentimentCohortSuppressed(t *testing.T) {
	t0 := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	now, _ := testClock(t0)
	p, em := newTestProducer(now, nil, nil)
	small := healthyCohort("EUR/USD", t0.Add(-10*time.Minute))
	small.Accounts = 42
	p.Push(small)
	p.tick(t0)

	ls := payload(t, em.all()[0])["long_short"].(map[string]any)
	if ls["suppressed"] != true || ls["reason"] != "insufficient_cohort" {
		t.Fatalf("below-floor cohort must suppress: %v", ls)
	}
	for k := range ls {
		if k != "suppressed" && k != "reason" {
			t.Fatalf("suppressed view leaked %q=%v", k, ls[k])
		}
	}
}

func TestSentimentFlowCohortSuppressed(t *testing.T) {
	t0 := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	now, _ := testClock(t0)
	fs := &fakeTakerFlowSource{f: TakerFlow{
		BuyNotional: decimal.NewFromInt(1), Accounts: 10,
	}}
	p, em := newTestProducer(now, nil, fs)
	p.Push(healthyCohort("EUR/USD", t0.Add(-10*time.Minute)))
	p.tick(t0)
	tf := payload(t, em.all()[0])["taker_flow"].(map[string]any)
	if tf["suppressed"] != true || tf["reason"] != "insufficient_cohort" {
		t.Fatalf("flow cohort below floor must suppress: %v", tf)
	}
	if _, leaked := tf["buy_notional"]; leaked {
		t.Fatal("suppressed flow leaked notional")
	}
}

// ---------------------------------------------------------------------------
// Privacy — serialized wire must carry no per-account attribution
// ---------------------------------------------------------------------------

func TestSentimentFramePrivacy(t *testing.T) {
	t0 := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	now, _ := testClock(t0)
	fs := &fakeTakerFlowSource{f: TakerFlow{
		BuyNotional:  decimal.NewFromInt(4000),
		SellNotional: decimal.NewFromInt(2000),
		Trades:       500, Accounts: 250,
	}}
	p, em := newTestProducer(now, nil, fs)
	p.Push(healthyCohort("EUR/USD", t0.Add(-10*time.Minute)))
	p.tick(t0)

	raw, _ := json.Marshal(em.all()[0].Data)
	for _, bad := range []string{
		"account_id", "maker", "taker_account", "position_id",
		"order_id", "client", "email",
	} {
		if strings.Contains(strings.ToLower(string(raw)), bad) {
			t.Fatalf("wire payload leaks %q: %s", bad, raw)
		}
	}
	// "accounts" (a count) is legitimate; only identifiers are banned.
}

// ---------------------------------------------------------------------------
// Series aggregation
// ---------------------------------------------------------------------------

func TestLongShortSeries(t *testing.T) {
	t0 := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	now, _ := testClock(t0)
	em := &emitter{}
	p := NewSentimentProducer(SentimentProducerConfig{Now: now},
		nil, nil, em.fn())

	// Two 5m buckets: [t0-15m,t0-10m) suppressed, [t0-10m,t0-5m)
	// publishable. The horizon lands mid-second-bucket so the tail reads
	// open=true (as a premium real-time caller would see it).
	small := healthyCohort("EUR/USD", t0.Add(-14*time.Minute))
	small.Accounts = 10
	p.rings["EUR/USD"] = []cohortSample{
		{at: small.AsOf, c: small},
		{at: t0.Add(-9 * time.Minute), c: healthyCohort("EUR/USD", t0.Add(-9*time.Minute))},
		{at: t0.Add(-7 * time.Minute), c: healthyCohort("EUR/USD", t0.Add(-7*time.Minute))},
	}
	horizon := t0.Add(-6 * time.Minute) // mid [t0-10m, t0-5m)
	pts, err := p.LongShortSeries("EUR/USD", 300, horizon, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(pts) != 2 {
		t.Fatalf("points=%d want 2: %+v", len(pts), pts)
	}
	if !pts[0].Suppressed {
		t.Fatalf("first bucket should be suppressed: %+v", pts[0])
	}
	if pts[1].Suppressed || pts[1].LongRatio != "0.600000" {
		t.Fatalf("second bucket wrong: %+v", pts[1])
	}
	if pts[1].LongShortRatio != "1.500000" {
		t.Fatalf("l/s ratio=%q", pts[1].LongShortRatio)
	}
	if !pts[1].Open {
		t.Fatal("last bucket straddles the horizon — must flag open")
	}
}

func TestLongShortSeriesRejectsBadWidth(t *testing.T) {
	p := NewSentimentProducer(SentimentProducerConfig{}, nil, nil,
		func(string, uint64, any) {})
	if _, err := p.LongShortSeries("EUR/USD", 123, time.Now(), 10); err == nil {
		t.Fatal("non-period bucket width must fail")
	}
}

func TestLatestCohortHorizon(t *testing.T) {
	t0 := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	p := NewSentimentProducer(SentimentProducerConfig{}, nil, nil,
		func(string, uint64, any) {})
	older := healthyCohort("EUR/USD", t0.Add(-10*time.Minute))
	younger := healthyCohort("EUR/USD", t0.Add(-2*time.Minute))
	younger.LongAccounts = 30
	p.rings["EUR/USD"] = []cohortSample{
		{at: older.AsOf, c: older},
		{at: younger.AsOf, c: younger},
	}
	c, ok := p.LatestCohort("EUR/USD", t0.Add(-5*time.Minute))
	if !ok || c.LongAccounts != 90 {
		t.Fatalf("horizon must bound the read: %+v ok=%v", c, ok)
	}
	if _, ok := p.LatestCohort("EUR/USD", t0.Add(-20*time.Minute)); ok {
		t.Fatal("no sample aged past horizon — found must be false")
	}
}

// ---------------------------------------------------------------------------
// Snapshot seam (WS resume/cold-start)
// ---------------------------------------------------------------------------

func TestSentimentSnapshot(t *testing.T) {
	t0 := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	now, _ := testClock(t0)
	p, _ := newTestProducer(now, nil, nil)
	p.Push(healthyCohort("EUR/USD", t0.Add(-10*time.Minute)))
	p.tick(t0)

	seq, data, err := p.Snapshot(context.Background(), "sentiment@EUR/USD")
	if err != nil || seq == 0 {
		t.Fatalf("snapshot seq=%d err=%v", seq, err)
	}
	d := data.(sentimentData)
	if d.Symbol != "EUR/USD" || d.LongShort == nil || d.LongShort.Suppressed {
		t.Fatalf("snapshot data=%+v", d)
	}
	if _, _, err := p.Snapshot(context.Background(), "sentiment@XXX"); err == nil {
		t.Fatal("unknown symbol must error")
	}
	if _, _, err := p.Snapshot(context.Background(), "trades@EUR/USD"); err == nil {
		t.Fatal("wrong channel type must error")
	}
}

// ---------------------------------------------------------------------------
// aggregateCohort — hedging counts + concentration bands
// ---------------------------------------------------------------------------

func TestAggregateCohortConcentration(t *testing.T) {
	var rows []cohortRow
	// One whale with 50% of gross + 99 minnows; one account hedged both
	// sides counts once in Accounts and in EACH side count.
	for i := int64(1); i <= 99; i++ {
		rows = append(rows, cohortRow{accountID: i, side: "LONG",
			notional: decimal.NewFromInt(100)})
	}
	rows = append(rows, cohortRow{accountID: 100, side: "SHORT",
		notional: decimal.NewFromInt(9900)})
	rows = append(rows, cohortRow{accountID: 99, side: "SHORT",
		notional: decimal.NewFromInt(50)}) // acct 99 hedges

	c := aggregateCohort("EUR/USD", rows,
		time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC))
	if c.Accounts != 100 {
		t.Fatalf("accounts=%d", c.Accounts)
	}
	if c.LongAccounts != 99 || c.ShortAccounts != 2 {
		t.Fatalf("side counts long=%d short=%d", c.LongAccounts, c.ShortAccounts)
	}
	// Gross = 99*100 + 9900 + 50 = 19900; top-5% = ceil(5) largest.
	if c.Top5Share == nil {
		t.Fatal("top5 share missing")
	}
	// Whale(9900) + acct99(150) + three 100s = 10350 / 19900 ≈ 0.5201
	if c.Top5Share.LessThan(decimal.MustFromString("0.51")) ||
		c.Top5Share.GreaterThan(decimal.MustFromString("0.53")) {
		t.Fatalf("top5 share=%s", *c.Top5Share)
	}
}

func TestAggregateCohortEmpty(t *testing.T) {
	c := aggregateCohort("EUR/USD", nil, time.Now())
	if !c.Suppressed(SentimentMinCohortAccounts) {
		t.Fatal("empty cohort must read as suppressed")
	}
	if c.LongRatio() != nil || c.Top5Share != nil {
		t.Fatal("empty cohort must carry nil ratios, never zeros")
	}
}

// ---------------------------------------------------------------------------
// OI read seam (endpoint a of Task 23.3.6)
// ---------------------------------------------------------------------------

func TestOIProducerLatest(t *testing.T) {
	p := NewOIProducer(OIProducerConfig{}, nil, func(string, uint64, any) {})
	if _, ok := p.Latest("EUR/USD"); ok {
		t.Fatal("never-observed symbol must report !ok")
	}
	p.last["EUR/USD"] = OISample{
		Symbol: "EUR/USD", OpenInterest: decimal.NewFromInt(1000),
		AsOf: time.Now(),
	}
	s, ok := p.Latest("EUR/USD")
	if !ok || !s.OpenInterest.Equal(decimal.NewFromInt(1000)) {
		t.Fatalf("latest=%+v ok=%v", s, ok)
	}
}
