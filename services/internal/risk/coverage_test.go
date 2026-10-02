// coverage_test.go — Kupiec POF / Christoffersen independence coverage
// tests (Task 19.3.13 §4, spec Basel backtesting convention).
package risk

import (
	"context"
	"encoding/json"
	"math"
	"math/rand"
	"testing"
	"time"

	"exchange/pkg/decimal"
)

// Reference values hand-computed from Kupiec (1995): n=250, p=0.01 →
// x=4 LR=0.769 (p=0.38, accept), x=8 LR=7.734 (p=0.005, reject),
// x=10 LR=12.955 (p=0.0003).
func TestKupiecPOFReferenceValues(t *testing.T) {
	lr, p := KupiecPOF(250, 4, 0.01)
	if math.Abs(lr-0.769) > 0.05 || p < 0.3 {
		t.Fatalf("green-zone LR=%.3f p=%.4f want ~0.77/>0.3", lr, p)
	}
	lr, p = KupiecPOF(250, 8, 0.01)
	if math.Abs(lr-7.734) > 0.1 || p > 0.01 {
		t.Fatalf("red-zone LR=%.3f p=%.4f want ~7.7/<0.01", lr, p)
	}
	lr, p = KupiecPOF(250, 10, 0.01)
	if lr < 12 || p > 0.001 {
		t.Fatalf("red-zone LR=%.3f p=%.4f want ~13/<0.001", lr, p)
	}
}

func TestKupiecPOFEdgeCases(t *testing.T) {
	// No observations → untestable, not a rejection.
	if lr, p := KupiecPOF(0, 0, 0.01); lr != 0 || p != 1 {
		t.Fatalf("empty: lr=%v p=%v", lr, p)
	}
	// All-breach sequence must not NaN (phat=1 → (n-x)·ln(1-phat) edge).
	lr, p := KupiecPOF(10, 10, 0.01)
	if math.IsNaN(lr) || math.IsNaN(p) || p >= 0.001 {
		t.Fatalf("all-breach: lr=%v p=%v", lr, p)
	}
	// Green-zone counts accept.
	if lr, p := KupiecPOF(250, 2, 0.01); math.IsNaN(lr) || p < 0.5 {
		t.Fatalf("clean: lr=%v p=%v", lr, p)
	}
	// Zero breaches over the full Basel window is itself significant —
	// the model is over-conservative (two-sided LR, p=0.025 < 0.05).
	if lr, p := KupiecPOF(250, 0, 0.01); math.Abs(lr-5.025) > 0.01 || p >= 0.05 {
		t.Fatalf("zero-breach over-coverage must flag: lr=%v p=%v", lr, p)
	}
}

func TestChristoffersenIndependence(t *testing.T) {
	// A fixed-seed shuffled ~25% breach sequence has transition
	// proportions matching the marginals → LR ≈ 0, accept.
	flags := make([]bool, 200)
	for _, i := range rand.New(rand.NewSource(42)).Perm(200)[:50] {
		flags[i] = true
	}
	lr, p := ChristoffersenIndependence(flags)
	if math.IsNaN(lr) || lr > 3.84 || p < 0.05 {
		t.Fatalf("independent seq: lr=%.3f p=%.4f", lr, p)
	}
	// Breaches strictly clustered (long clean run, then long breach run)
	// → strong dependence → reject.
	clustered := make([]bool, 100)
	for i := 90; i < 100; i++ {
		clustered[i] = true
	}
	lr, p = ChristoffersenIndependence(clustered)
	if lr < 3.84 || p > 0.05 {
		t.Fatalf("clustered seq: lr=%.3f p=%.4f want χ²₁>3.84", lr, p)
	}
	// Single-state sequence → untestable.
	if lr, p := ChristoffersenIndependence([]bool{false, false}); lr != 0 || p != 1 {
		t.Fatalf("single-state: lr=%v p=%v", lr, p)
	}
}

func TestEvaluateCoverage(t *testing.T) {
	// Healthy register: 250 days, 4 breach-days over ~500 events.
	flags := make([]bool, 250)
	for _, i := range []int{10, 60, 130, 200} {
		flags[i] = true
	}
	r := EvaluateCoverage(500, 4, flags, BaselExpectedBreachProb)
	if r.Rejected95 {
		t.Fatalf("healthy model rejected: %+v", r)
	}
	if r.Observations != 500 || r.Breaches != 4 || r.Days != 250 || r.BreachDays != 4 {
		t.Fatalf("counts: %+v", r)
	}
	// Pathological: 40 breaches over 500 events → POF rejects at 95%.
	bad := make([]bool, 250)
	for _, i := range []int{5, 90, 150, 220} {
		bad[i] = true
	}
	r = EvaluateCoverage(500, 40, bad, BaselExpectedBreachProb)
	if !r.Rejected95 || r.KupiecPValue >= 0.05 {
		t.Fatalf("pathological model must reject: %+v", r)
	}
}

// The Backtester stamps the coverage verdict into result_metrics and
// raises the model-adequacy alert when the window rejects.
func TestBacktestCoverageStamped(t *testing.T) {
	ctx := context.Background()
	store := newFakeRunStore()
	alerter := &captureAlerter{}
	b, _ := NewBacktester(&stubBacktestSource{}, store, alerter,
		func() time.Time { return testNow }, nil)

	// Seed a window of clean days with one event each.
	for i := 1; i <= 60; i++ {
		m := `{"events_evaluated":1}`
		if _, err := store.InsertRun(ctx, ModelRun{Kind: RunKindBacktest,
			Scenario: backtestScenarioName, Status: RunStatusPass,
			ResultMetrics: []byte(m),
			CreatedAt:     testNow.Add(-time.Duration(i) * 24 * time.Hour)}); err != nil {
			t.Fatal(err)
		}
	}
	res, err := b.RunDailyBacktest(ctx, testNow.Add(-24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if res.Coverage == nil {
		t.Fatal("coverage verdict must be stamped on the result")
	}
	if res.Coverage.Days != 61 || res.Coverage.Rejected95 {
		t.Fatalf("coverage: %+v", res.Coverage)
	}
	// The persisted row carries the verdict too.
	var decoded BacktestResult
	row := store.runs[res.RunID]
	if err := json.Unmarshal(row.ResultMetrics, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Coverage == nil || decoded.Coverage.Days != 61 {
		t.Fatalf("persisted coverage: %+v", decoded.Coverage)
	}
}

func TestBacktestCoverageRejectsPathologicalModel(t *testing.T) {
	ctx := context.Background()
	store := newFakeRunStore()
	alerter := &captureAlerter{}
	b, _ := NewBacktester(&stubBacktestSource{}, store, alerter,
		func() time.Time { return testNow }, nil)

	// 10 historical breach days, 5 events each, 2 breaches/day.
	for i := 1; i <= 10; i++ {
		m := `{"events_evaluated":5}`
		if _, err := store.InsertRun(ctx, ModelRun{Kind: RunKindBacktest,
			Scenario: backtestScenarioName, Status: RunStatusFail,
			BreachCount:   2,
			ResultMetrics: []byte(m),
			CreatedAt:     testNow.Add(-time.Duration(i) * 24 * time.Hour)}); err != nil {
			t.Fatal(err)
		}
	}
	// Today breaches too — pushes the window's breach-day frequency
	// and event ratio deep into the reject region.
	src := &stubBacktestSource{events: []RealizedLiquidation{
		{EventID: 1, Kind: "DIRECT_CLOSE", Side: "LONG",
			MarkPrice: decimal.One, Price: decimal.RequireFromString("0.9")},
	}}
	b, _ = NewBacktester(src, store, alerter,
		func() time.Time { return testNow }, nil)
	res, err := b.RunDailyBacktest(ctx, testNow.Add(-24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if res.Coverage == nil || !res.Coverage.Rejected95 {
		t.Fatalf("pathological window must reject at 95%%: %+v", res.Coverage)
	}
	var adequacy int
	for _, a := range alerter.alerts {
		if a.Code == codeMarginModelAdequacy {
			adequacy++
		}
	}
	if adequacy == 0 {
		t.Fatalf("model-adequacy alert expected: %+v", alerter.alerts)
	}
}
