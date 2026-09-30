// Phase-23 Task 23.3.11 tests — public venue performance statistics
// (spec §16.10, §24 #380): insufficient-data markers, the
// reconcile→alert→hold-last-good state machine, and privacy of the
// serialized document.
package marketdata

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"exchange/internal/observability"
	"exchange/pkg/decimal"
)

// ---------------------------------------------------------------------------
// Fakes
// ---------------------------------------------------------------------------

type fakeDailyStats struct {
	rows []PairDayStats
	err  error
}

func (f *fakeDailyStats) DailyStats(_ context.Context, _, _ time.Time) ([]PairDayStats, error) {
	return f.rows, f.err
}

type fakeFillRates struct {
	rows []VenueFillCounter
	err  error
}

func (f *fakeFillRates) FillRates(_ context.Context, _, _ time.Time) ([]VenueFillCounter, error) {
	return f.rows, f.err
}

type fakeUptime struct {
	value float64
	found bool
	err   error
}

func (f *fakeUptime) Uptime(_ context.Context, _ string, _ time.Duration) (float64, bool, error) {
	return f.value, f.found, f.err
}

type fakeRTS27 struct {
	figs []RTS27Figure
	err  error
}

func (f *fakeRTS27) PublishedFigures(context.Context) ([]RTS27Figure, error) {
	return f.figs, f.err
}

// fakeReference feeds per-metric reconciliation values.
type fakeReference struct {
	vals  map[string]float64
	found map[string]bool
}

func (f *fakeReference) Reference(_ context.Context, metric string,
	_, _ time.Time) (float64, bool, error) {
	if !f.found[metric] {
		return 0, false, nil
	}
	return f.vals[metric], true, nil
}

// alertFake captures observability.Alert dispatches.
type alertFake struct {
	mu     sync.Mutex
	alerts []observability.Alert
}

func (f *alertFake) Raise(_ context.Context, a observability.Alert) error {
	f.mu.Lock()
	f.alerts = append(f.alerts, a)
	f.mu.Unlock()
	return nil
}

func (f *alertFake) all() []observability.Alert {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]observability.Alert, len(f.alerts))
	copy(out, f.alerts)
	return out
}

func perfDay(sym string, day time.Time, spread, lat, fr string,
	sub, fil, fills int64) PairDayStats {
	s := decimal.MustFromString(spread)
	l := decimal.MustFromString(lat)
	r := decimal.MustFromString(fr)
	return PairDayStats{
		Symbol: sym, Day: day,
		AvgSpreadBps:        &s,
		MedianExecLatencyMs: &l,
		FillRate:            &r,
		OrdersSubmitted:     &sub,
		OrdersFilled:        &fil,
		Fills:               fills,
		VolumeQuote:         decimal.NewFromInt(1_000_000),
	}
}

var perfNow = time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)

func testPerfService(daily *fakeDailyStats, fills *fakeFillRates,
	up *fakeUptime, ref *fakeReference, alerts observability.Sink) *VenuePerformanceService {
	deps := VenuePerformanceDeps{
		Daily: daily,
		RTS27: &fakeRTS27{figs: []RTS27Figure{{
			QuarterStart:    time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC),
			InstrumentClass: "CURRENCY_DERIVATIVES",
			Version:         1, DaysCovered: 60,
			Metrics: map[string]any{"fill_rate": "0.97"},
		}}},
		Alerts: alerts,
	}
	// Assign only when non-nil — a typed nil would poison the
	// `dep != nil` unwired checks in the service.
	if fills != nil {
		deps.Fills = fills
	}
	if up != nil {
		deps.Uptime = up
	}
	if ref != nil {
		deps.Reference = ref
	}
	return NewVenuePerformanceService(VenuePerformanceConfig{
		Now:     func() time.Time { return perfNow },
		Symbols: []string{"EUR/USD", "GBP/USD", "USD/JPY"},
	}, deps)
}

func healthyPerfDaily() *fakeDailyStats {
	// Oct 3 (Sat) + Oct 4 (Sun) 2026 share ISO week 40 — keeps the
	// weekly-rollup test on one bucket.
	d1 := perfNow.AddDate(0, 0, -3)
	d2 := perfNow.AddDate(0, 0, -2)
	return &fakeDailyStats{rows: []PairDayStats{
		perfDay("EUR/USD", d1, "0.40", "12.0", "0.98", 1000, 980, 5000),
		perfDay("EUR/USD", d2, "0.50", "10.0", "0.96", 1000, 960, 8000),
		perfDay("GBP/USD", d1, "0.60", "15.0", "0.95", 500, 475, 2000),
		perfDay("GBP/USD", d2, "0.55", "14.0", "0.97", 500, 485, 3000),
	}}
}

func healthyPerfFills() *fakeFillRates {
	return &fakeFillRates{rows: []VenueFillCounter{
		{Symbol: "EUR/USD", OrdersSubmitted: 1000, OrdersFilled: 960},
		{Symbol: "GBP/USD", OrdersSubmitted: 500, OrdersFilled: 480},
	}}
}

// ---------------------------------------------------------------------------
// Happy path
// ---------------------------------------------------------------------------

func TestVenuePerformanceReport(t *testing.T) {
	svc := testPerfService(healthyPerfDaily(), healthyPerfFills(),
		&fakeUptime{value: 0.9998, found: true},
		&fakeReference{vals: map[string]float64{
			"fill_rate_24h": 0.9827, // ≈ (960+480)/(1000+500)
		}, found: map[string]bool{"fill_rate_24h": true}},
		&alertFake{})
	doc, err := svc.Report(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if doc.Status != "ok" {
		t.Fatalf("status=%s divergent=%v", doc.Status, doc.Divergent)
	}
	if doc.DelayMs != int64(900000) {
		t.Fatalf("delay_ms=%d want 900000 (15m)", doc.DelayMs)
	}
	if len(doc.Pairs) != 3 {
		t.Fatalf("pairs=%d want 3 (universe ∪ observed)", len(doc.Pairs))
	}
	bySym := map[string]PairPerformance{}
	for _, p := range doc.Pairs {
		bySym[p.Symbol] = p
	}
	eur := bySym["EUR/USD"]
	if len(eur.Daily) != 2 || eur.InsufficientData {
		t.Fatalf("EUR/USD daily=%+v", eur)
	}
	if *eur.Daily[0].AvgSpreadBps == "" {
		t.Fatal("spread missing")
	}
	// USD/JPY is configured but has no rows — insufficient_data marker,
	// never a zero-filled row.
	if !bySym["USD/JPY"].InsufficientData || len(bySym["USD/JPY"].Daily) != 0 {
		t.Fatalf("new pair must mark insufficient_data: %+v", bySym["USD/JPY"])
	}
	if doc.Venue == nil || doc.Venue.FillRate24h == nil {
		t.Fatal("venue rollup missing")
	}
	if *doc.Venue.FillRate24h == "" {
		t.Fatal("fill_rate_24h missing")
	}
	if doc.Venue.Uptime24h == nil || *doc.Venue.Uptime24h != 0.9998 {
		t.Fatalf("uptime24h=%v", doc.Venue.Uptime24h)
	}
	if doc.Venue.CurrentSession == nil ||
		doc.Venue.CurrentSession.DelayMs != 900000 {
		t.Fatalf("current_session=%+v", doc.Venue.CurrentSession)
	}
	if len(doc.RTS27) != 1 || doc.RTS27[0].InstrumentClass != "CURRENCY_DERIVATIVES" {
		t.Fatalf("rts27=%+v", doc.RTS27)
	}
}

func TestVenuePerformanceWeeklyRollup(t *testing.T) {
	svc := testPerfService(healthyPerfDaily(), nil, nil, nil, &alertFake{})
	doc, err := svc.Report(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var eur PairPerformance
	for _, p := range doc.Pairs {
		if p.Symbol == "EUR/USD" {
			eur = p
		}
	}
	if len(eur.Weekly) != 1 {
		t.Fatalf("weekly=%d want 1 (same ISO week)", len(eur.Weekly))
	}
	w := eur.Weekly[0]
	if w.Fills != 13000 || w.Days != 2 {
		t.Fatalf("week rollup=%+v", w)
	}
	// fills-weighted spread: (0.40*5000 + 0.50*8000)/13000 ≈ 0.4615
	if w.AvgSpreadBps == nil ||
		*w.AvgSpreadBps < "0.46" || *w.AvgSpreadBps > "0.47" {
		t.Fatalf("weighted spread=%v", w.AvgSpreadBps)
	}
	// weekly fill rate from summed counters: 1940/2000 = 0.97
	if w.FillRate == nil || *w.FillRate != "0.970000" {
		t.Fatalf("weekly fill_rate=%v", w.FillRate)
	}
}

// ---------------------------------------------------------------------------
// Reconciliation → alert → hold-last-good (Task 23.3.11 edge case)
// ---------------------------------------------------------------------------

func TestVenuePerformanceDivergenceHoldsLastGood(t *testing.T) {
	ref := &fakeReference{
		vals:  map[string]float64{"fill_rate_24h": 0.9827},
		found: map[string]bool{"fill_rate_24h": true},
	}
	alerts := &alertFake{}
	svc := testPerfService(healthyPerfDaily(), healthyPerfFills(),
		nil, ref, alerts)

	good, err := svc.Report(context.Background())
	if err != nil || good.Status != "ok" {
		t.Fatalf("first report status=%s err=%v", good.Status, err)
	}
	goodRate := *good.Venue.FillRate24h

	// Reference now disagrees wildly — the fresh compute is 0.9827, the
	// reference reads 0.40 → >5% divergence → hold last-good + alert.
	ref.vals["fill_rate_24h"] = 0.40
	held, err := svc.Report(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if held.Status != "held" {
		t.Fatalf("status=%s want held", held.Status)
	}
	if held.HeldAgeMs == nil {
		t.Fatal("held report must carry held_age_ms")
	}
	if *held.Venue.FillRate24h != goodRate {
		t.Fatalf("held payload diverged from last-good: %s vs %s",
			*held.Venue.FillRate24h, goodRate)
	}
	if len(held.Divergent) != 1 || held.Divergent[0] != "fill_rate_24h" {
		t.Fatalf("divergent=%v", held.Divergent)
	}
	if got := len(alerts.all()); got != 1 {
		t.Fatalf("alerts=%d want exactly 1 (edge-triggered)", got)
	}
	if alerts.all()[0].Status != "firing" ||
		alerts.all()[0].Details["metric"] != "fill_rate_24h" {
		t.Fatalf("alert=%+v", alerts.all()[0])
	}

	// Sustained divergence stays held without refiring the alert.
	if _, err := svc.Report(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := len(alerts.all()); got != 1 {
		t.Fatalf("alert refired: %d", got)
	}

	// Recovery publishes fresh data again and resolves the alert.
	ref.vals["fill_rate_24h"] = 0.9827
	ok, err := svc.Report(context.Background())
	if err != nil || ok.Status != "ok" {
		t.Fatalf("recovery status=%s err=%v", ok.Status, err)
	}
	if alerts.all()[1].Status != "resolved" {
		t.Fatalf("resolve alert missing: %+v", alerts.all())
	}
}

func TestVenuePerformanceDivergenceWithoutLastGood(t *testing.T) {
	ref := &fakeReference{
		vals:  map[string]float64{"fill_rate_24h": 0.10},
		found: map[string]bool{"fill_rate_24h": true},
	}
	svc := testPerfService(healthyPerfDaily(), healthyPerfFills(),
		nil, ref, &alertFake{})
	doc, err := svc.Report(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if doc.Status != "insufficient_data" {
		t.Fatalf("status=%s — first-ever report is divergent with no "+
			"last-good: must suppress, not publish bad data", doc.Status)
	}
	if doc.Pairs != nil || doc.Venue != nil {
		t.Fatal("suppressed report must carry no figures")
	}
}

// ---------------------------------------------------------------------------
// Privacy — the serialized document must carry no account/order fields
// ---------------------------------------------------------------------------

func TestVenuePerformancePrivacy(t *testing.T) {
	svc := testPerfService(healthyPerfDaily(), healthyPerfFills(),
		&fakeUptime{value: 0.999, found: true}, nil, &alertFake{})
	doc, err := svc.Report(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(doc)
	for _, bad := range []string{
		"account_id", "order_id", "position_id", "client_id",
		"generated_by", "published_by", "email",
	} {
		if strings.Contains(strings.ToLower(string(raw)), bad) {
			t.Fatalf("performance payload leaks %q", bad)
		}
	}
}

// ---------------------------------------------------------------------------
// Absent-source honesty
// ---------------------------------------------------------------------------

func TestVenuePerformanceUptimeUnknown(t *testing.T) {
	svc := testPerfService(healthyPerfDaily(), nil,
		&fakeUptime{found: false}, nil, &alertFake{})
	doc, err := svc.Report(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if doc.Venue.Uptime24h != nil || doc.Venue.Uptime30d != nil {
		t.Fatal("no-history uptime must be null, never 1.0")
	}
}

func TestVenuePerformanceNoDailySource(t *testing.T) {
	svc := NewVenuePerformanceService(VenuePerformanceConfig{},
		VenuePerformanceDeps{})
	if _, err := svc.Report(context.Background()); err == nil {
		t.Fatal("nil daily source must fail closed")
	}
}
