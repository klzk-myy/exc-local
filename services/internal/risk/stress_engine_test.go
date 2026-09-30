// stress_engine_test.go — unit coverage for the Task-19.3.13 stress
// suite math, adequacy metric and daily backtester (spec §13.10).
package risk

import (
	"context"
	"testing"
	"time"

	"exchange/pkg/decimal"
)

// ---------------------------------------------------------------------------
// Fakes
// ---------------------------------------------------------------------------

type stubPortfolio struct {
	accounts []StressAccount
	oi       map[int64]decimal.Decimal
}

func (s *stubPortfolio) Snapshot(context.Context) ([]StressAccount, error) {
	return s.accounts, nil
}
func (s *stubPortfolio) InstrumentOI(context.Context) (map[int64]decimal.Decimal, error) {
	return s.oi, nil
}

type stubFund struct{ bal decimal.Decimal }

func (s stubFund) FundBalanceUSD(context.Context) (decimal.Decimal, error) {
	return s.bal, nil
}

type stubBacktestSource struct{ events []RealizedLiquidation }

func (s *stubBacktestSource) LiquidationsForDay(context.Context, time.Time) ([]RealizedLiquidation, error) {
	return s.events, nil
}

func stressEngineFor(t *testing.T, src StressPortfolioSource,
	fund decimal.Decimal) (*StressEngine, *fakeRunStore, *captureAlerter) {
	t.Helper()
	store := newFakeRunStore()
	alerter := &captureAlerter{}
	e, err := NewStressEngine(src, store, stubFund{bal: fund}, nil, alerter,
		StressEngineConfig{}, func() time.Time { return testNow }, nil)
	if err != nil {
		t.Fatalf("engine: %v", err)
	}
	return e, store, alerter
}

func mkAcct(id int64, equity, used string, positions ...StressPosition) StressAccount {
	return StressAccount{AccountID: id,
		EquityUSD:     decimal.RequireFromString(equity),
		UsedMarginUSD: decimal.RequireFromString(used),
		Positions:     positions}
}

func mkPos(id, acct, instr int64, sym, group, side, qty, mark string) StressPosition {
	return StressPosition{PositionID: id, AccountID: acct, InstrumentID: instr,
		Symbol: sym, Group: group, Side: side,
		Quantity: decimal.RequireFromString(qty), MarkPrice: decimal.RequireFromString(mark)}
}

// ---------------------------------------------------------------------------
// Scenario math
// ---------------------------------------------------------------------------

func TestEvalScenarioLiquidationAndShortfall(t *testing.T) {
	// Account 1: equity 10, used 100, LONG 100@1.0, shift −5%.
	// stressedEquity = 10 − 5 = 5 < 100×0.5 → liquidated.
	// stressed notional 95 → direct slip 2% → exit cost 1.9.
	// final equity 3.1 → no shortfall.
	// Account 2: equity 2 → stressed equity −3 → liquidated;
	// shortfall = 1.9 − (−3) = 4.9.
	pos1 := mkPos(1, 1, 10, "EUR/USD", GroupMajor, "LONG", "100", "1.0")
	pos2 := mkPos(2, 2, 10, "EUR/USD", GroupMajor, "LONG", "100", "1.0")
	src := &stubPortfolio{
		accounts: []StressAccount{
			mkAcct(1, "10", "100", pos1),
			mkAcct(2, "2", "100", pos2),
		},
		oi: map[int64]decimal.Decimal{10: decimal.NewFromInt(1_000_000)},
	}
	e, _, _ := stressEngineFor(t, src, decimal.NewFromInt(1_000_000))
	sc := StressScenario{Name: "T", Kind: ScenarioKindWeekendGap,
		UniformShift: decimal.RequireFromString("-0.05")}
	res := e.evalScenario(src.accounts, map[int64]decimal.Decimal{10: decimal.NewFromInt(1_000_000)},
		sc, decimal.NewFromInt(1_000_000))
	if res.AccountsEvaluated != 2 || res.AccountsLiquidated != 2 || res.ShortfallAccounts != 1 {
		t.Fatalf("counts: %+v", res)
	}
	if res.FundDrawdownUSD != "4.9" {
		t.Fatalf("drawdown: %s", res.FundDrawdownUSD)
	}
	if !res.Adequate {
		t.Fatal("fund 1e6 must cover 4.9")
	}
}

func TestEvalScenarioAuctionSlippageAndInadequacy(t *testing.T) {
	// Position notional 100 — OI 1,000 → 1% of OI = 10 → notional 95 >
	// 10 → auction leg at 5% slippage.
	pos := mkPos(1, 1, 10, "USD/TRY", GroupExotic, "LONG", "100", "1.0")
	src := &stubPortfolio{
		accounts: []StressAccount{mkAcct(1, "2", "50", pos)},
		oi:       map[int64]decimal.Decimal{10: decimal.NewFromInt(1000)},
	}
	e, _, _ := stressEngineFor(t, src, decimal.Zero)
	sc := StressScenario{Name: "T", Kind: ScenarioKindWeekendGap,
		UniformShift: decimal.RequireFromString("-0.05")}
	res := e.evalScenario(src.accounts, src.oi, sc, decimal.NewFromInt(3))
	// stressed equity 2−5 = −3 → liquidated; notional 95 → auction slip
	// 5% → exit 4.75; shortfall = 4.75 − (−3) = 7.75.
	if res.AuctionPositions != 1 || res.AuctionFailures != 1 {
		t.Fatalf("auction: %+v", res)
	}
	if res.FundDrawdownUSD != "7.75" {
		t.Fatalf("drawdown: %s", res.FundDrawdownUSD)
	}
	if res.Adequate {
		t.Fatal("fund 3 < 7.75 → inadequate")
	}
	// SHORT leg: qty −100, shift +5% → equity 2−5 = −3 → liquidated;
	// notional 105 (adverse move grows the buy-back) → auction slip 5%
	// → exit 5.25; shortfall = 5.25 − (−3) = 8.25.
	src.accounts[0].Positions[0].Side = "SHORT"
	src.accounts[0].Positions[0].Quantity = decimal.NewFromInt(-100)
	sc.UniformShift = decimal.RequireFromString("0.05")
	res = e.evalScenario(src.accounts, src.oi, sc, decimal.NewFromInt(3))
	if res.FundDrawdownUSD != "8.25" {
		t.Fatalf("short leg drawdown: %s", res.FundDrawdownUSD)
	}
}

func TestEvalScenarioVolMultiplierAndFundDrain(t *testing.T) {
	// Vol spike: used margin doubles → 30×2=60; equity 40 →
	// 40 < 60×0.5=30? No — 40 ≥ 30 → not liquidated without the
	// multiplier it would pass even earlier. Use equity 25:
	// 25 < 30 → liquidated by the doubled requirement.
	pos := mkPos(1, 1, 10, "EUR/USD", GroupMajor, "LONG", "10", "1.0")
	src := &stubPortfolio{
		accounts: []StressAccount{mkAcct(1, "25", "30", pos)},
		oi:       map[int64]decimal.Decimal{10: decimal.NewFromInt(1_000_000)},
	}
	e, _, _ := stressEngineFor(t, src, decimal.NewFromInt(100))
	sc := StressScenario{Name: "V", Kind: ScenarioKindVolatilitySpike,
		SigmaMultiple: decimal.Zero, VolMultiplier: decimal.NewFromInt(2)}
	res := e.evalScenario(src.accounts, src.oi, sc, decimal.NewFromInt(100))
	if res.AccountsLiquidated != 1 {
		t.Fatalf("vol-mult liquidation: %+v", res)
	}
	// Fund drain: drain 90% of 100 → fundAfter 10.
	sc2 := StressScenario{Name: "D", Kind: ScenarioKindFundDepletion,
		FundDrainFraction: decimal.RequireFromString("0.9")}
	res = e.evalScenario(src.accounts, src.oi, sc2, decimal.NewFromInt(100))
	if res.FundAfterDrainUSD != "10" {
		t.Fatalf("fund drain: %+v", res)
	}
}

func TestShiftForPrecedence(t *testing.T) {
	sc := StressScenario{
		SigmaMultiple: decimal.NewFromInt(2),
		UniformShift:  decimal.RequireFromString("0.03"),
		SymbolShifts:  map[string]decimal.Decimal{"USD/TRY": decimal.RequireFromString("-0.09")},
	}
	if got := sc.ShiftFor("USD/TRY", GroupExotic); got.String() != "-0.09" {
		t.Fatalf("symbol shift: %s", got)
	}
	if got := sc.ShiftFor("EUR/USD", GroupMajor); got.String() != "0.03" {
		t.Fatalf("uniform shift: %s", got)
	}
	sc.UniformShift = decimal.Zero
	// major σ 0.006 × 2 = 0.012.
	if got := sc.ShiftFor("EUR/USD", GroupMajor); got.String() != "0.012" {
		t.Fatalf("sigma shift: %s", got)
	}
	// Unknown group → exotic σ 0.018 × 2 = 0.036 (widest, §2.7).
	if got := sc.ShiftFor("X", "??"); got.String() != "0.036" {
		t.Fatalf("unknown group σ: %s", got)
	}
}

func TestWorstPctileShortfall(t *testing.T) {
	rs := []ScenarioResult{
		{FundDrawdownUSD: "5"}, {FundDrawdownUSD: "9"}, {FundDrawdownUSD: "1"},
	}
	if got := WorstPctileShortfall(rs, 1); got.String() != "9" {
		t.Fatalf("worst-1%%: %s", got)
	}
	if got := WorstPctileShortfall(nil, 1); !got.IsZero() {
		t.Fatalf("empty: %s", got)
	}
}

func TestScenarioLibraryCoverage(t *testing.T) {
	lib := ScenarioLibrary()
	want := map[string]bool{
		"RATE_SHOCK_-1SIGMA": false, "RATE_SHOCK_+1SIGMA": false,
		"RATE_SHOCK_-2SIGMA": false, "RATE_SHOCK_+2SIGMA": false,
		"RATE_SHOCK_-3SIGMA": false, "RATE_SHOCK_+3SIGMA": false,
		"VOL_SPIKE_DOWN_2X": false, "WEEKEND_GAP_DOWN_2PCT": false,
		"WEEKEND_GAP_UP_2PCT": false, "FUND_DEPLETION_CASCADE": false,
	}
	for _, sc := range lib {
		if _, ok := want[sc.Name]; !ok {
			t.Fatalf("unexpected scenario %q", sc.Name)
		}
		want[sc.Name] = true
	}
	for name, seen := range want {
		if !seen {
			t.Fatalf("library missing %s", name)
		}
	}
}

// ---------------------------------------------------------------------------
// RunStressSuite — persistence + adequacy alert
// ---------------------------------------------------------------------------

func TestRunStressSuitePersistsAndAlerts(t *testing.T) {
	pos := mkPos(1, 1, 10, "EUR/USD", GroupMajor, "LONG", "100", "1.0")
	src := &stubPortfolio{
		accounts: []StressAccount{mkAcct(1, "2", "50", pos)},
		oi:       map[int64]decimal.Decimal{10: decimal.NewFromInt(1_000_000)},
	}
	// Fund 0 → every drawdown-positive scenario is inadequate → alert.
	e, store, alerter := stressEngineFor(t, src, decimal.Zero)
	res, err := e.RunStressSuite(context.Background())
	if err != nil {
		t.Fatalf("suite: %v", err)
	}
	if res.Adequate {
		t.Fatal("zero fund cannot be adequate")
	}
	if len(res.Results) != len(ScenarioLibrary()) {
		t.Fatalf("results %d vs library %d", len(res.Results), len(ScenarioLibrary()))
	}
	for _, r := range res.Results {
		if r.RunID == 0 {
			t.Fatalf("scenario %s missing run row", r.Scenario)
		}
		stored := store.runs[r.RunID]
		if stored == nil || stored.Kind != RunKindStress {
			t.Fatalf("run row %d missing", r.RunID)
		}
	}
	var adequacyAlerts int
	for _, a := range alerter.alerts {
		if a.Code == codeMarginModelAdequacy {
			adequacyAlerts++
		}
	}
	if adequacyAlerts != 1 {
		t.Fatalf("adequacy alerts: %d", adequacyAlerts)
	}
	// Worst-1% provider reads the persisted suite metric.
	worst, err := e.WorstOnePctShortfall(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if worst.String() != res.WorstOnePctShortfallUSD {
		t.Fatalf("provider %s vs suite %s", worst, res.WorstOnePctShortfallUSD)
	}
}

func TestStressSchedulerRunDue(t *testing.T) {
	src := &stubPortfolio{accounts: nil, oi: map[int64]decimal.Decimal{}}
	e, store, _ := stressEngineFor(t, src, decimal.NewFromInt(10))
	s := &StressScheduler{Engine: e}
	// First run — no prior STRESS row → runs.
	ran, err := s.RunDue(context.Background())
	if err != nil || !ran {
		t.Fatalf("first run due: %v %v", ran, err)
	}
	// Just ran → not due.
	ran, err = s.RunDue(context.Background())
	if err != nil || ran {
		t.Fatalf("fresh run must not re-run: %v %v", ran, err)
	}
	// Backdate the latest run past the interval → due again.
	for _, r := range store.runs {
		r.CreatedAt = testNow.Add(-8 * 24 * time.Hour)
	}
	ran, err = s.RunDue(context.Background())
	if err != nil || !ran {
		t.Fatalf("stale run due: %v %v", ran, err)
	}
}

// ---------------------------------------------------------------------------
// Backtester — predicted floors vs realized slippage
// ---------------------------------------------------------------------------

func TestPredictedBoundAndBreached(t *testing.T) {
	// AUCTION_FILL LONG → floor mark×0.98.
	bound, floor, ok := PredictedBound(RealizedLiquidation{
		Kind: "AUCTION_FILL", Side: "LONG", MarkPrice: decimal.One})
	if !ok || !floor || bound.String() != "0.98" {
		t.Fatalf("auction long bound: %s floor=%v ok=%v", bound, floor, ok)
	}
	if !Breached(RealizedLiquidation{Kind: "AUCTION_FILL", Side: "LONG",
		MarkPrice: decimal.One, Price: decimal.RequireFromString("0.97")}) {
		t.Fatal("fill 0.97 < floor 0.98 must breach")
	}
	if Breached(RealizedLiquidation{Kind: "AUCTION_FILL", Side: "LONG",
		MarkPrice: decimal.One, Price: decimal.RequireFromString("0.985")}) {
		t.Fatal("fill 0.985 ≥ floor 0.98 must not breach")
	}
	// SHORT cap ×1.02.
	if !Breached(RealizedLiquidation{Kind: "AUCTION_FILL", Side: "SHORT",
		MarkPrice: decimal.One, Price: decimal.RequireFromString("1.03")}) {
		t.Fatal("buy 1.03 > cap 1.02 must breach")
	}
	// FORCE_CASH LONG → mark×0.95.
	bound, floor, ok = PredictedBound(RealizedLiquidation{
		Kind: "FORCE_CASH", Side: "LONG", MarkPrice: decimal.One})
	if !ok || bound.String() != "0.95" {
		t.Fatalf("force-cash bound: %s", bound)
	}
	// DIRECT_CLOSE → mark exactly.
	if !Breached(RealizedLiquidation{Kind: "DIRECT_CLOSE", Side: "LONG",
		MarkPrice: decimal.One, Price: decimal.RequireFromString("0.999")}) {
		t.Fatal("direct close below mark must breach")
	}
	// Unmarked event → breach by construction.
	if !Breached(RealizedLiquidation{Kind: "ADL", Side: "LONG",
		Price: decimal.One}) {
		t.Fatal("unmarked event must breach")
	}
}

func TestRunDailyBacktest(t *testing.T) {
	ctx := context.Background()
	day := testNow.Add(-24 * time.Hour)
	src := &stubBacktestSource{events: []RealizedLiquidation{
		{EventID: 1, Kind: "AUCTION_FILL", Side: "LONG",
			MarkPrice: decimal.One, Price: decimal.RequireFromString("0.97")},
		{EventID: 2, Kind: "AUCTION_FILL", Side: "SHORT",
			MarkPrice: decimal.One, Price: decimal.RequireFromString("1.01")},
		{EventID: 3, Kind: "FORCE_CASH", Side: "LONG",
			MarkPrice: decimal.One, Price: decimal.RequireFromString("0.96")},
	}}
	store := newFakeRunStore()
	alerter := &captureAlerter{}
	b, err := NewBacktester(src, store, alerter, func() time.Time { return testNow }, nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := b.RunDailyBacktest(ctx, day)
	if err != nil {
		t.Fatal(err)
	}
	// Event 1 breaches (0.97 < 0.98); events 2 (1.01 < 1.02) and 3
	// (0.96 ≥ 0.95) do not.
	if res.Breaches != 1 || res.EventsEvaluated != 3 {
		t.Fatalf("breaches: %+v", res)
	}
	stored := store.runs[res.RunID]
	if stored == nil || stored.Kind != RunKindBacktest || stored.Status != RunStatusFail ||
		stored.BreachCount != 1 {
		t.Fatalf("run row: %+v", stored)
	}
	if res.Rolling250dCount != 1 {
		t.Fatalf("rolling: %d", res.Rolling250dCount)
	}
	var breachAlerts int
	for _, a := range alerter.alerts {
		if a.Code == codeBacktestBreach {
			breachAlerts++
		}
	}
	if breachAlerts != 1 {
		t.Fatalf("breach alerts: %d", breachAlerts)
	}
}

func TestRunDailyBacktestEmptyDayAndRollingCeiling(t *testing.T) {
	ctx := context.Background()
	day := testNow.Add(-24 * time.Hour)
	src := &stubBacktestSource{events: nil}
	store := newFakeRunStore()
	alerter := &captureAlerter{}
	b, _ := NewBacktester(src, store, alerter, func() time.Time { return testNow }, nil)

	// Empty day → PASS row, zero breaches, no alert.
	res, err := b.RunDailyBacktest(ctx, day)
	if err != nil || res.Breaches != 0 || res.EventsEvaluated != 0 {
		t.Fatalf("empty day: %+v %v", res, err)
	}
	if store.runs[res.RunID].Status != RunStatusPass {
		t.Fatalf("empty day must PASS: %+v", store.runs[res.RunID])
	}
	if len(alerter.alerts) != 0 {
		t.Fatalf("clean day must not alert: %+v", alerter.alerts)
	}

	// Seed 5 historical breach days inside the window → rolling > 4
	// raises the model-review alert.
	for i := 1; i <= 5; i++ {
		_, err := store.InsertRun(ctx, ModelRun{Kind: RunKindBacktest,
			Scenario: backtestScenarioName, Status: RunStatusFail, BreachCount: 1,
			CreatedAt: testNow.Add(-time.Duration(i) * 48 * time.Hour)})
		if err != nil {
			t.Fatal(err)
		}
	}
	src.events = []RealizedLiquidation{{EventID: 9, Kind: "DIRECT_CLOSE", Side: "LONG",
		MarkPrice: decimal.One, Price: decimal.RequireFromString("0.9")}}
	_, err = b.RunDailyBacktest(ctx, day)
	if err != nil {
		t.Fatal(err)
	}
	var modelAlerts int
	for _, a := range alerter.alerts {
		if a.Code == codeMarginModelAdequacy {
			modelAlerts++
		}
	}
	if modelAlerts != 1 {
		t.Fatalf("rolling-ceiling alerts: %d (%+v)", modelAlerts, alerter.alerts)
	}
}

func TestBacktestRunDue(t *testing.T) {
	ctx := context.Background()
	src := &stubBacktestSource{events: nil}
	store := newFakeRunStore()
	b, _ := NewBacktester(src, store, nil, func() time.Time { return testNow }, nil)
	// Fresh register → yesterday evaluated once.
	n, err := b.RunDue(ctx)
	if err != nil || n != 1 {
		t.Fatalf("first due: %d %v", n, err)
	}
	// Second call — covered through yesterday → no-op.
	n, err = b.RunDue(ctx)
	if err != nil || n != 0 {
		t.Fatalf("repeat due: %d %v", n, err)
	}
	// Backdate runs 3 days → catch up 2 uncovered days.
	for _, r := range store.runs {
		r.CreatedAt = testNow.Add(-4 * 24 * time.Hour)
	}
	n, err = b.RunDue(ctx)
	if err != nil || n != 3 {
		t.Fatalf("catch-up: %d %v", n, err)
	}
}

func TestStressEngineConstructionFailClosed(t *testing.T) {
	if _, err := NewStressEngine(nil, newFakeRunStore(), stubFund{}, nil, nil,
		StressEngineConfig{}, nil, nil); err == nil {
		t.Fatal("nil source must fail")
	}
	if _, err := NewStressEngine(&stubPortfolio{}, nil, stubFund{}, nil, nil,
		StressEngineConfig{}, nil, nil); err == nil {
		t.Fatal("nil store must fail")
	}
	if _, err := NewStressEngine(&stubPortfolio{}, newFakeRunStore(), nil, nil, nil,
		StressEngineConfig{}, nil, nil); err == nil {
		t.Fatal("nil fund source must fail")
	}
	if _, err := NewBacktester(nil, newFakeRunStore(), nil, nil, nil); err == nil {
		t.Fatal("nil backtest source must fail")
	}
	if _, err := NewPgModelRunStore(nil); err == nil {
		t.Fatal("nil pool must fail")
	}
	if _, err := NewParamChangeGate(nil, nil, 0, nil); err == nil {
		t.Fatal("nil run store must fail")
	}
	if _, err := NewPgStressPortfolioSource(nil); err == nil {
		t.Fatal("nil pool must fail")
	}
	if _, err := NewPgBacktestSource(nil); err == nil {
		t.Fatal("nil pool must fail")
	}
	if _, err := NewPgParamChangeStore(nil); err == nil {
		t.Fatal("nil pool must fail")
	}
}
