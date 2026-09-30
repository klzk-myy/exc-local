// adl_test.go — unit + gated integration coverage for the ADL cluster
// (Phase-19 Task 19.3.19 + the ADL execution leg of 19.3.3/19.3.20;
// spec §13.6, §13.11 item 2, §24 #94/#97/#269).
//
// Ungated unit legs cover the scorer (formula, quintile bucketing,
// mark/leverage legs, fail-closed errors), the pure write-plan shape,
// and the engine trigger/reconcile policy over fakes.
//
// Gated legs (same conventions as liquidation_store_test.go):
//
//	EXC_PG_TEST=1    go test ./internal/risk/ -run 'TestPgADL' -v
//	EXC_REDIS_TEST=1 go test ./internal/risk/ -run 'TestRedisADL' -v
//
// The PG legs reuse liqStoreFixture + the liqSeed* helpers from
// liquidation_store_test.go (same package) — the scratch schema already
// carries migration 230 (adl_directives + the 'ADL' kind CHECK).
package risk

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strconv"
	"testing"
	"time"

	"exchange/internal/accounts"
	excredis "exchange/internal/redis"
	"exchange/pkg/decimal"
)

// ---------------------------------------------------------------------------
// Fakes (name-spaced to avoid collisions with sibling test files)
// ---------------------------------------------------------------------------

type fakeADLMarks struct {
	m   map[string]decimal.Decimal
	err error
}

func (f fakeADLMarks) BatchMarks(_ context.Context, _ []string) (map[string]decimal.Decimal, error) {
	return f.m, f.err
}

type fakeADLLeverage struct {
	m   map[[2]int64]int
	err error
}

func (f fakeADLLeverage) Effective(_ context.Context, acct, instr int64) (int, error) {
	if f.err != nil {
		return 0, f.err
	}
	if v, ok := f.m[[2]int64{acct, instr}]; ok {
		return v, nil
	}
	return 1, fmt.Errorf("leverage pair %d/%d not seeded", acct, instr)
}

type fakeADLUniverse struct {
	positions []LiqPosition
	err       error
}

func (f *fakeADLUniverse) Universe(_ context.Context) ([]LiqPosition, error) {
	return f.positions, f.err
}

type fakeADLStore struct {
	fakeADLUniverse
	nextSeq       int64
	inserted      []ADLDirective
	dispatchedSeq []int64
	failedSeq     []int64
	insertErr     error
	reconcileOut  *ADLReconcileResult
	reconcileErr  error
	reconcileIn   []ADLFillReport
}

func (f *fakeADLStore) InsertADLDirective(_ context.Context, d ADLDirective) (int64, error) {
	if f.insertErr != nil {
		return 0, f.insertErr
	}
	f.nextSeq++
	d.Seq = f.nextSeq
	f.inserted = append(f.inserted, d)
	return d.Seq, nil
}

func (f *fakeADLStore) MarkADLDirectiveDispatched(_ context.Context, seq int64, _ time.Time) error {
	f.dispatchedSeq = append(f.dispatchedSeq, seq)
	return nil
}

func (f *fakeADLStore) MarkADLDirectiveFailed(_ context.Context, seq int64) error {
	f.failedSeq = append(f.failedSeq, seq)
	return nil
}

func (f *fakeADLStore) CompleteADLFill(_ context.Context, r ADLFillReport) (*ADLReconcileResult, error) {
	f.reconcileIn = append(f.reconcileIn, r)
	return f.reconcileOut, f.reconcileErr
}

var _ ADLEngineStore = (*fakeADLStore)(nil)

type fakeADLFund struct {
	depleted bool
	bal      decimal.Decimal
	err      error
}

func (f fakeADLFund) Depleted(_ context.Context, _ string) (bool, decimal.Decimal, error) {
	return f.depleted, f.bal, f.err
}

type fakeADLDispatch struct {
	err    error
	accept bool
	calls  []accounts.CloseOrderRequest
}

func (f *fakeADLDispatch) MassCancel(_ context.Context, _ accounts.MassCancelScope) (*accounts.MassCancelResult, error) {
	return &accounts.MassCancelResult{}, nil
}

func (f *fakeADLDispatch) SubmitClose(_ context.Context, req accounts.CloseOrderRequest) (*accounts.OrderAck, error) {
	f.calls = append(f.calls, req)
	if f.err != nil {
		return &accounts.OrderAck{Accepted: false, Detail: f.err.Error()}, f.err
	}
	return &accounts.OrderAck{Accepted: f.accept, OrderID: 42}, nil
}

var _ accounts.OrderDispatcher = (*fakeADLDispatch)(nil)

// flakyADLDispatch rejects the first close, accepts the rest.
type flakyADLDispatch struct {
	calls     []accounts.CloseOrderRequest
	failFirst bool
}

func (f *flakyADLDispatch) MassCancel(_ context.Context, _ accounts.MassCancelScope) (*accounts.MassCancelResult, error) {
	return &accounts.MassCancelResult{}, nil
}

func (f *flakyADLDispatch) SubmitClose(_ context.Context, req accounts.CloseOrderRequest) (*accounts.OrderAck, error) {
	f.calls = append(f.calls, req)
	if f.failFirst && len(f.calls) == 1 {
		return &accounts.OrderAck{Accepted: false, Detail: "engine rejected"}, fmt.Errorf("engine rejected")
	}
	return &accounts.OrderAck{Accepted: true, OrderID: 77}, nil
}

var _ accounts.OrderDispatcher = (*flakyADLDispatch)(nil)

// ---------------------------------------------------------------------------
// Builders + wiring helpers
// ---------------------------------------------------------------------------

func adlPos(id, acct, instr int64, symbol, side, qty, entry, mark, mode string) LiqPosition {
	return LiqPosition{
		ID: id, AccountID: acct, InstrumentID: instr, Symbol: symbol,
		Side: side, Quantity: decimal.RequireFromString(qty),
		EntryPrice: decimal.RequireFromString(entry),
		MarkPrice:  decimal.RequireFromString(mark),
		MarginMode: mode,
	}
}

func adlTestScorer(marks map[string]decimal.Decimal, lev map[[2]int64]int) *ADLScorer {
	s, err := NewADLScorer(fakeADLMarks{m: marks}, fakeADLLeverage{m: lev})
	if err != nil {
		panic(err)
	}
	return s
}

func adlTestEngine(t *testing.T, store *fakeADLStore, fund fakeADLFund,
	disp accounts.OrderDispatcher, alerter *opsAlertSpy, scorer *ADLScorer) *ADLEngine {
	t.Helper()
	e, err := NewADLEngine(ADLEngineDeps{
		Store: store, Scorer: scorer, Fund: fund,
		Dispatch: disp, Alerter: alerter,
	})
	if err != nil {
		t.Fatalf("engine: %v", err)
	}
	return e
}

// ---------------------------------------------------------------------------
// ADLScorer
// ---------------------------------------------------------------------------

func TestADLScorerNilLeverage(t *testing.T) {
	if _, err := NewADLScorer(nil, nil); err == nil {
		t.Fatal("nil resolver must fail construction")
	}
}

func TestADLScorerFormula(t *testing.T) {
	// LONG: qty 100, entry 1.0 → mark 1.1: profitPct = 0.1; lev 20 → score 2.
	// SHORT: qty 100, entry 1.0 → mark 0.9: profitPct = 0.1; lev 20 → score 2.
	s := adlTestScorer(
		map[string]decimal.Decimal{
			"EUR/USD": decimal.RequireFromString("1.1"),
			"GBP/USD": decimal.RequireFromString("0.9"),
		},
		map[[2]int64]int{{1, 10}: 20, {1, 11}: 20})
	scored, err := s.ScoreUniverse(context.Background(), []LiqPosition{
		adlPos(1, 1, 10, "EUR/USD", "LONG", "100", "1.0", "1.0", "CROSS"),
		adlPos(2, 1, 11, "GBP/USD", "SHORT", "100", "1.0", "1.0", "PORTFOLIO"),
	})
	if err != nil {
		t.Fatalf("score: %v", err)
	}
	if len(scored) != 2 {
		t.Fatalf("want 2 scored, got %d", len(scored))
	}
	// Tied scores: position-id asc is the deterministic tie-break, so pos 1
	// takes rank 0 (level 5) and pos 2 sits at rank fraction 0.5 → band 2 →
	// level 3 (n=2 spreads across the quintile bands).
	wantQ := map[int64]int{1: 5, 2: 3}
	for _, sc := range scored {
		if !sc.ProfitPct.Equal(decimal.RequireFromString("0.1")) {
			t.Fatalf("pos %d profitPct %s, want 0.1", sc.PositionID, sc.ProfitPct)
		}
		if !sc.Score.Equal(decimal.NewFromInt(2)) {
			t.Fatalf("pos %d score %s, want 2", sc.PositionID, sc.Score)
		}
		if sc.Quintile != wantQ[sc.PositionID] {
			t.Fatalf("pos %d quintile %d, want %d", sc.PositionID, sc.Quintile, wantQ[sc.PositionID])
		}
	}
}

func TestADLScorerQuintileBucketing(t *testing.T) {
	// 10 profitable positions with distinct scores: top 20% → 5, next
	// → 4, → 3, and the bottom two bands clamp to 2 (level 1 is reserved
	// for non-profitable positions).
	var positions []LiqPosition
	marks := map[string]decimal.Decimal{"EUR/USD": decimal.RequireFromString("110")}
	lev := map[[2]int64]int{}
	for i := int64(1); i <= 10; i++ {
		// entry 101..109 gives a strictly descending profit% against
		// mark 110; i=10 would sit exactly at mark (profitPct 0) so the
		// profitable set is i=1..9 with lev i (score separates them).
		entry := decimal.NewFromInt(100).Add(decimal.NewFromInt(i)).String()
		positions = append(positions,
			adlPos(i, i, 10, "EUR/USD", "LONG", "100", entry, entry, "CROSS"))
		lev[[2]int64{i, 10}] = int(i)
	}
	// Scores: profitPct_i × lev_i — check the intended rank order holds.
	s := adlTestScorer(marks, lev)
	scored, err := s.ScoreUniverse(context.Background(), positions)
	if err != nil {
		t.Fatalf("score: %v", err)
	}
	sort.Slice(scored, func(a, b int) bool { return ADLScoreLess(scored[a], scored[b]) })
	profitable := []ADLScore{}
	for _, sc := range scored {
		if sc.Score.IsPositive() {
			profitable = append(profitable, sc)
		}
	}
	// Whatever the exact score order, the quintile assignment must be a
	// monotonic non-increasing sequence over the ranked order with
	// values in 2..5, top band(s) at 5, and strictly ≥2 throughout.
	prev := 6
	for i, sc := range profitable {
		if sc.Quintile < 2 || sc.Quintile > 5 {
			t.Fatalf("rank %d pos %d quintile %d out of 2..5", i, sc.PositionID, sc.Quintile)
		}
		if sc.Quintile > prev {
			t.Fatalf("rank %d pos %d quintile %d exceeds predecessor %d", i, sc.PositionID, sc.Quintile, prev)
		}
		prev = sc.Quintile
	}
	if profitable[0].Quintile != 5 {
		t.Fatalf("top-ranked position must be quintile 5, got %d", profitable[0].Quintile)
	}
	// Position 10 (entry == mark) scores 0 → level 1.
	for _, sc := range scored {
		if sc.PositionID == 10 && sc.Quintile != 1 {
			t.Fatalf("break-even position must be level 1, got %d", sc.Quintile)
		}
	}
}

func TestADLScorerExactBuckets(t *testing.T) {
	// Controlled scores 10..1 (desc): exact level sequence for n=10 is
	// 5,5,4,4,3,3,2,2,2,2 (top 20% → 5; the fifth band clamps to 2).
	var positions []LiqPosition
	marks := map[string]decimal.Decimal{}
	lev := map[[2]int64]int{}
	for i := int64(0); i < 10; i++ {
		// Identical profit% (entry 100 → mark 110 ⇒ +10%); leverage 10..1
		// makes the score strictly descending with position ID.
		positions = append(positions,
			adlPos(i+1, i+1, 10, "EUR/USD", "LONG", "100", "100", "100", "CROSS"))
		lev[[2]int64{i + 1, 10}] = int(10 - i)
	}
	marks["EUR/USD"] = decimal.RequireFromString("110")
	s := adlTestScorer(marks, lev)
	scored, err := s.ScoreUniverse(context.Background(), positions)
	if err != nil {
		t.Fatalf("score: %v", err)
	}
	got := map[int64]int{}
	for _, sc := range scored {
		got[sc.PositionID] = sc.Quintile
	}
	want := map[int64]int{1: 5, 2: 5, 3: 4, 4: 4, 5: 3, 6: 3, 7: 2, 8: 2, 9: 2, 10: 2}
	for id, w := range want {
		if got[id] != w {
			t.Fatalf("pos %d quintile %d, want %d (map %v)", id, got[id], w, got)
		}
	}
}

func TestADLScorerLeverageFailureFailsClosed(t *testing.T) {
	// A profitable position whose leverage cannot resolve must fail the
	// whole pass — guessing it would mis-rank the deleveraging order.
	s := adlTestScorer(
		map[string]decimal.Decimal{"EUR/USD": decimal.RequireFromString("2")},
		map[[2]int64]int{}) // no pair seeded → resolver errors
	_, err := s.ScoreUniverse(context.Background(), []LiqPosition{
		adlPos(1, 1, 10, "EUR/USD", "LONG", "100", "1", "1", "CROSS"),
	})
	if err == nil {
		t.Fatal("unresolvable leverage on a profitable position must error")
	}
}

func TestADLScorerMarkFallbackChain(t *testing.T) {
	// No live Redis mark → stored mark_price → entry. At entry mark the
	// position has zero PnL → level 1, score 0.
	s := adlTestScorer(map[string]decimal.Decimal{}, map[[2]int64]int{{1, 10}: 30})
	scored, err := s.ScoreUniverse(context.Background(), []LiqPosition{
		adlPos(1, 1, 10, "EUR/USD", "LONG", "100", "1.5", "1.5", "CROSS"),
		adlPos(2, 1, 10, "EUR/USD", "LONG", "100", "1.5", "0", "ISOLATED"), // excluded mode
	})
	if err != nil {
		t.Fatalf("score: %v", err)
	}
	if len(scored) != 1 || scored[0].Quintile != 1 || scored[0].Score.IsPositive() {
		t.Fatalf("fallback scoring wrong: %+v", scored)
	}
}

// ---------------------------------------------------------------------------
// planADLWrites — hash/ZSET shape (matches RedisADLIndicatorReader decoding)
// ---------------------------------------------------------------------------

func TestPlanADLWritesShape(t *testing.T) {
	scored := []ADLScore{
		{AccountID: 7, PositionID: 100, InstrumentID: 10, Symbol: "EUR/USD",
			Side: "LONG", Score: decimal.RequireFromString("3"), Quintile: 5},
		{AccountID: 7, PositionID: 101, InstrumentID: 10, Symbol: "EUR/USD",
			Side: "LONG", Score: decimal.RequireFromString("9"), Quintile: 5},
		{AccountID: 8, PositionID: 102, InstrumentID: 10, Symbol: "EUR/USD",
			Side: "LONG", Score: decimal.Zero, Quintile: 1},
	}
	plan := planADLWrites(scored)
	if plan.indicator[7][100] != 5 || plan.indicator[7][101] != 5 || plan.indicator[8][102] != 1 {
		t.Fatalf("indicator fields: %+v", plan.indicator)
	}
	z := plan.priority[AdlPriorityKey("EUR/USD", "LONG")]
	if len(z) != 1 || z[7] != 9 { // one member per account — the MAX leg score
		t.Fatalf("priority members: %+v", z)
	}
}

// ---------------------------------------------------------------------------
// ADLEngine — trigger policy (fakes)
// ---------------------------------------------------------------------------

func adlTriggerReq() ADLTriggerRequest {
	return ADLTriggerRequest{
		LiquidatedAccountID: 1, LiquidatedPositionID: 900,
		InstrumentID: 10, Symbol: "EUR/USD", LiquidatedSide: "LONG",
		DeficitQty:      decimal.NewFromInt(150),
		BankruptcyPrice: decimal.RequireFromString("1.0"),
		Currency:        "USD",
	}
}

func TestADLEngineNotDepletedIsNoop(t *testing.T) {
	store := &fakeADLStore{}
	disp := &fakeADLDispatch{accept: true}
	e := adlTestEngine(t, store, fakeADLFund{depleted: false, bal: decimal.NewFromInt(1000)},
		disp, &opsAlertSpy{}, adlTestScorer(nil, nil))
	res, err := e.TriggerADL(context.Background(), adlTriggerReq())
	if err != nil {
		t.Fatalf("trigger: %v", err)
	}
	if res.Triggered || len(store.inserted) != 0 || len(disp.calls) != 0 {
		t.Fatalf("healthy fund must no-op: %+v", res)
	}
}

func TestADLEngineValidatesRequest(t *testing.T) {
	e := adlTestEngine(t, &fakeADLStore{}, fakeADLFund{depleted: true},
		&fakeADLDispatch{}, &opsAlertSpy{}, adlTestScorer(nil, nil))
	for _, mut := range []func(*ADLTriggerRequest){
		func(r *ADLTriggerRequest) { r.DeficitQty = decimal.Zero },
		func(r *ADLTriggerRequest) { r.BankruptcyPrice = decimal.Zero },
		func(r *ADLTriggerRequest) { r.LiquidatedSide = "SIDEWAYS" },
		func(r *ADLTriggerRequest) { r.Symbol = "" },
	} {
		req := adlTriggerReq()
		mut(&req)
		if _, err := e.TriggerADL(context.Background(), req); err == nil {
			t.Fatalf("bad request accepted: %+v", req)
		}
	}
}

func TestADLEngineExecutesTopCounterparties(t *testing.T) {
	// Liquidated LONG deficit 150 @ 1.0. Profitable SHORT counterparties:
	// acct 2 (qty 100, best score) then acct 3 (qty 100) — partial take.
	// The liquidated account's own opposing SHORT must never be picked.
	store := &fakeADLStore{fakeADLUniverse: fakeADLUniverse{positions: []LiqPosition{
		adlPos(900, 1, 10, "EUR/USD", "LONG", "300", "1.5", "1.0", "CROSS"),
		adlPos(901, 1, 10, "EUR/USD", "SHORT", "500", "2.0", "2.0", "CROSS"), // own side — excluded
		adlPos(902, 2, 10, "EUR/USD", "SHORT", "100", "2.0", "2.0", "CROSS"), // pct=(2-1)/2=.5
		adlPos(903, 3, 10, "EUR/USD", "SHORT", "100", "1.1", "1.1", "CROSS"), // pct=(1.1-1)/1.1≈.09
		adlPos(904, 4, 10, "EUR/USD", "SHORT", "400", "1.0", "1.0", "CROSS"), // break-even — out
	}}}
	scorer := adlTestScorer(
		map[string]decimal.Decimal{"EUR/USD": decimal.NewFromInt(1)},
		map[[2]int64]int{{1, 10}: 10, {2, 10}: 10, {3, 10}: 10, {4, 10}: 10})
	alerter := &opsAlertSpy{}
	disp := &fakeADLDispatch{accept: true}
	e, err := NewADLEngine(ADLEngineDeps{
		Store: store, Scorer: scorer,
		Fund:     fakeADLFund{depleted: true, bal: decimal.NewFromInt(-150000)},
		Dispatch: disp, Alerter: alerter})
	if err != nil {
		t.Fatalf("engine: %v", err)
	}
	res, err := e.TriggerADL(context.Background(), adlTriggerReq())
	if err != nil {
		t.Fatalf("trigger: %v", err)
	}
	if !res.Triggered || res.Counterparties != 2 || res.Dispatched != 2 {
		t.Fatalf("result: %+v", res)
	}
	if !res.CoveredQty.Equal(decimal.NewFromInt(150)) || res.ShortfallQty.IsPositive() {
		t.Fatalf("coverage: covered=%s shortfall=%s", res.CoveredQty, res.ShortfallQty)
	}
	// Directive order: acct 2 full 100 first, acct 3 partial 50.
	if len(store.inserted) != 2 || store.inserted[0].AccountID != 2 ||
		!store.inserted[0].Qty.Equal(decimal.NewFromInt(100)) ||
		store.inserted[1].AccountID != 3 ||
		!store.inserted[1].Qty.Equal(decimal.NewFromInt(50)) {
		t.Fatalf("directives: %+v", store.inserted)
	}
	// Counterparty SHORT closes by BUY at the bankruptcy cap, reduce-only.
	if len(disp.calls) != 2 || disp.calls[0].Side != accounts.SideBuy ||
		!disp.calls[0].LimitPrice.Equal(decimal.RequireFromString("1.0")) ||
		!disp.calls[0].ReduceOnly || disp.calls[0].ClientOrderID != "adl-1" {
		t.Fatalf("dispatch calls: %+v", disp.calls)
	}
	if len(store.dispatchedSeq) != 2 || len(store.failedSeq) != 0 {
		t.Fatalf("status marks: dispatched=%v failed=%v", store.dispatchedSeq, store.failedSeq)
	}
	// Directive rows carry the rank economics.
	if !store.inserted[0].Score.IsPositive() || store.inserted[0].Quintile < 2 ||
		store.inserted[0].Status != ADLStatusQueued || store.inserted[0].Side != "SHORT" {
		t.Fatalf("directive fields: %+v", store.inserted[0])
	}
	// Success pages the ADL_TRIGGERED event.
	found := false
	for _, a := range alerter.alerts {
		if a.Code == CodeADLTriggered {
			found = true
		}
	}
	if !found {
		t.Fatalf("ADL_TRIGGERED alert missing: %+v", alerter.alerts)
	}
}

func TestADLEngineNoCounterpartyFailsLoud(t *testing.T) {
	store := &fakeADLStore{fakeADLUniverse: fakeADLUniverse{positions: []LiqPosition{
		adlPos(900, 1, 10, "EUR/USD", "LONG", "300", "1.5", "1.0", "CROSS"),
	}}}
	scorer := adlTestScorer(map[string]decimal.Decimal{"EUR/USD": decimal.NewFromInt(1)}, map[[2]int64]int{})
	alerter := &opsAlertSpy{}
	e, _ := NewADLEngine(ADLEngineDeps{
		Store: store, Scorer: scorer,
		Fund:     fakeADLFund{depleted: true, bal: decimal.NewFromInt(-1)},
		Dispatch: &fakeADLDispatch{accept: true}, Alerter: alerter})
	res, err := e.TriggerADL(context.Background(), adlTriggerReq())
	if err == nil {
		t.Fatal("no counterparties must error")
	}
	if !res.Triggered || res.Counterparties != 0 {
		t.Fatalf("result: %+v", res)
	}
	found := false
	for _, a := range alerter.alerts {
		if a.Code == CodeADLNoCounterparty {
			found = true
		}
	}
	if !found {
		t.Fatal("ADL_NO_COUNTERPARTY alert missing")
	}
}

func TestADLEngineShortfallAndDispatchFailure(t *testing.T) {
	// acct 2's dispatch fails (→ FAILED), acct 3 covers 60 of 150 →
	// shortfall 90 must page ADL_SHORTFALL and error out.
	store := &fakeADLStore{fakeADLUniverse: fakeADLUniverse{positions: []LiqPosition{
		adlPos(902, 2, 10, "EUR/USD", "SHORT", "100", "2.0", "2.0", "CROSS"),
		adlPos(903, 3, 10, "EUR/USD", "SHORT", "60", "1.1", "1.1", "CROSS"),
	}}}
	scorer := adlTestScorer(
		map[string]decimal.Decimal{"EUR/USD": decimal.NewFromInt(1)},
		map[[2]int64]int{{2, 10}: 10, {3, 10}: 10})
	alerter := &opsAlertSpy{}
	disp := &flakyADLDispatch{failFirst: true}
	e, _ := NewADLEngine(ADLEngineDeps{
		Store: store, Scorer: scorer,
		Fund:     fakeADLFund{depleted: true, bal: decimal.NewFromInt(-1)},
		Dispatch: disp, Alerter: alerter})
	res, err := e.TriggerADL(context.Background(), adlTriggerReq())
	if err == nil {
		t.Fatal("shortfall must error")
	}
	if res.Failed != 1 || res.Dispatched != 1 ||
		!res.CoveredQty.Equal(decimal.NewFromInt(60)) ||
		!res.ShortfallQty.Equal(decimal.NewFromInt(90)) {
		t.Fatalf("result: %+v", res)
	}
	if len(store.failedSeq) != 1 || len(store.dispatchedSeq) != 1 {
		t.Fatalf("marks: failed=%v dispatched=%v", store.failedSeq, store.dispatchedSeq)
	}
	found := false
	for _, a := range alerter.alerts {
		if a.Code == CodeADLShortfall {
			found = true
		}
	}
	if !found {
		t.Fatal("ADL_SHORTFALL alert missing")
	}
}

func TestADLEngineInsertFailureFailsClosed(t *testing.T) {
	store := &fakeADLStore{fakeADLUniverse: fakeADLUniverse{positions: []LiqPosition{
		adlPos(902, 2, 10, "EUR/USD", "SHORT", "100", "2.0", "2.0", "CROSS"),
	}}, insertErr: fmt.Errorf("pg down")}
	scorer := adlTestScorer(
		map[string]decimal.Decimal{"EUR/USD": decimal.NewFromInt(1)},
		map[[2]int64]int{{2, 10}: 10})
	disp := &fakeADLDispatch{accept: true}
	e, _ := NewADLEngine(ADLEngineDeps{
		Store: store, Scorer: scorer,
		Fund:     fakeADLFund{depleted: true, bal: decimal.NewFromInt(-1)},
		Dispatch: disp, Alerter: &opsAlertSpy{}})
	if _, err := e.TriggerADL(context.Background(), adlTriggerReq()); err == nil {
		t.Fatal("outbox insert failure must error")
	}
	if len(disp.calls) != 0 {
		t.Fatal("no dispatch may follow a failed directive insert")
	}
}

func TestADLReconcileValidation(t *testing.T) {
	e := adlTestEngine(t, &fakeADLStore{}, fakeADLFund{}, &fakeADLDispatch{},
		&opsAlertSpy{}, adlTestScorer(nil, nil))
	for _, r := range []ADLFillReport{
		{AdlSeq: 0, FilledQty: decimal.One, FillPrice: decimal.One},
		{AdlSeq: 1, FilledQty: decimal.Zero, FillPrice: decimal.One},
		{AdlSeq: 1, FilledQty: decimal.One, FillPrice: decimal.Zero},
		{AdlSeq: 1, FilledQty: decimal.One, FillPrice: decimal.One,
			LiquidatedAccountID: 5, LiquidatedPositionID: 0, LiquidatedSide: ""},
	} {
		if _, err := e.ReconcileADLFill(context.Background(), r); err == nil {
			t.Fatalf("bad fill report accepted: %+v", r)
		}
	}
}

func TestADLReconcileStoreErrorAlerts(t *testing.T) {
	store := &fakeADLStore{reconcileErr: fmt.Errorf("not found")}
	alerter := &opsAlertSpy{}
	e := adlTestEngine(t, store, fakeADLFund{}, &fakeADLDispatch{}, alerter, adlTestScorer(nil, nil))
	_, err := e.ReconcileADLFill(context.Background(), ADLFillReport{
		AdlSeq: 9, FilledQty: decimal.One, FillPrice: decimal.One})
	if err == nil {
		t.Fatal("store error must propagate")
	}
	found := false
	for _, a := range alerter.alerts {
		if a.Code == CodeADLFillReconcileError {
			found = true
		}
	}
	if !found {
		t.Fatal("ADL_FILL_RECONCILE_ERROR alert missing")
	}
}

// ---------------------------------------------------------------------------
// PG-gated integration — PgADLStore (EXC_PG_TEST=1)
// ---------------------------------------------------------------------------

func TestPgADLStoreNilPool(t *testing.T) {
	if _, err := NewPgADLStore(nil); err == nil {
		t.Fatal("nil pool must fail construction")
	}
}

func TestPgADLStoreUniverse(t *testing.T) {
	_, pool := liqStoreFixture(t)
	ctx := context.Background()
	store, err := NewPgADLStore(pool)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	inst := liqSeedInstrument(t, pool)
	cross := liqSeedAccount(t, pool, "RETAIL")
	portfolio := liqSeedAccount(t, pool, "PROFESSIONAL")
	isolated := liqSeedAccount(t, pool, "RETAIL")
	noRow := liqSeedAccount(t, pool, "RETAIL") // no margin_accounts row → default CROSS
	liqSeedMarginAccount(t, pool, cross, "CROSS", "")
	liqSeedMarginAccount(t, pool, portfolio, "PORTFOLIO", "")
	liqSeedMarginAccount(t, pool, isolated, "ISOLATED", "")

	pCross := liqSeedPosition(t, pool, cross, inst, "LONG", "100", "1.10",
		liqStr("1.20"), nil, "10", "4")
	pPf := liqSeedPosition(t, pool, portfolio, inst, "SHORT", "-50", "1.30",
		liqStr("1.20"), nil, "5", "3")
	pNoRow := liqSeedPosition(t, pool, noRow, inst, "LONG", "10", "1.10",
		liqStr("1.10"), nil, "0", "1")
	// Excluded: ISOLATED-mode row and a flat row on the CROSS account.
	liqSeedPosition(t, pool, isolated, inst, "LONG", "100", "1.10",
		liqStr("0.90"), liqStr("1.00"), "-20", "4")
	liqSeedPosition(t, pool, cross, inst, "LONG", "0", "1.10",
		liqStr("1.10"), nil, "0", "0")

	got, err := store.Universe(ctx)
	if err != nil {
		t.Fatalf("universe: %v", err)
	}
	want := map[int64]string{pCross: "CROSS", pPf: "PORTFOLIO", pNoRow: "CROSS"}
	if len(got) != len(want) {
		t.Fatalf("universe size %d, want %d: %+v", len(got), len(want), got)
	}
	for _, p := range got {
		if w, ok := want[p.ID]; !ok {
			t.Fatalf("unexpected position %d in universe", p.ID)
		} else if p.MarginMode != w {
			t.Fatalf("position %d mode %s, want %s", p.ID, p.MarginMode, w)
		}
	}
}

func TestPgADLStoreDirectiveLifecycle(t *testing.T) {
	_, pool := liqStoreFixture(t)
	ctx := context.Background()
	store, err := NewPgADLStore(pool)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	cp := liqSeedAccount(t, pool, "RETAIL")
	liq := liqSeedAccount(t, pool, "RETAIL")
	inst := liqSeedInstrument(t, pool)
	cpPos := liqSeedPosition(t, pool, cp, inst, "SHORT", "-100", "2.00",
		liqStr("1.00"), nil, "100", "7")
	liqPosID := liqSeedPosition(t, pool, liq, inst, "LONG", "300", "1.50",
		liqStr("1.00"), liqStr("1.00"), "-150", "10")

	seq, err := store.InsertADLDirective(ctx, ADLDirective{
		AccountID: cp, InstrumentID: inst, Symbol: "EUR/USD", Side: "SHORT",
		Qty:             decimal.NewFromInt(100),
		BankruptcyPrice: decimal.RequireFromString("1.00"),
		Score:           decimal.RequireFromString("5"),
		Quintile:        5,
	})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	if seq <= 0 {
		t.Fatalf("bad seq %d", seq)
	}
	d, err := store.ADLDirectiveBySeq(ctx, seq)
	if err != nil || d == nil || d.Status != ADLStatusQueued {
		t.Fatalf("queued read-back: %v %+v", err, d)
	}
	if err := store.MarkADLDirectiveDispatched(ctx, seq, time.Now().UTC()); err != nil {
		t.Fatalf("dispatch mark: %v", err)
	}
	d, _ = store.ADLDirectiveBySeq(ctx, seq)
	if d.Status != ADLStatusDispatched {
		t.Fatalf("want DISPATCHED, got %s", d.Status)
	}

	res, err := store.CompleteADLFill(ctx, ADLFillReport{
		AdlSeq: seq, FilledQty: decimal.NewFromInt(100),
		FillPrice:           decimal.RequireFromString("1.00"),
		MarkPrice:           decimal.RequireFromString("1.00"),
		LiquidatedAccountID: liq, LiquidatedPositionID: liqPosID,
		LiquidatedSide: "LONG",
	})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if res.AlreadyReconciled || res.CounterpartyEventID <= 0 || res.LiquidatedEventID == nil {
		t.Fatalf("reconcile result: %+v", res)
	}
	d, _ = store.ADLDirectiveBySeq(ctx, seq)
	if d.Status != ADLStatusFilled {
		t.Fatalf("want FILLED, got %s", d.Status)
	}
	// Both liquidation_events rows landed with kind='ADL' + quintile.
	var kind string
	var side string
	var quint *int
	var acct int64
	var pos int64
	if err := pool.QueryRow(ctx, `
		SELECT account_id, position_id, kind, side::text, adl_quintile
		FROM liquidation_events WHERE id=$1`, res.CounterpartyEventID).
		Scan(&acct, &pos, &kind, &side, &quint); err != nil {
		t.Fatalf("cp event read: %v", err)
	}
	if acct != cp || pos != cpPos || kind != "ADL" || side != "SHORT" || quint == nil || *quint != 5 {
		t.Fatalf("cp event row: acct=%d pos=%d kind=%s side=%s q=%v", acct, pos, kind, side, quint)
	}
	if err := pool.QueryRow(ctx, `
		SELECT account_id, position_id, kind, side::text
		FROM liquidation_events WHERE id=$1`, *res.LiquidatedEventID).
		Scan(&acct, &pos, &kind, &side); err != nil {
		t.Fatalf("liq event read: %v", err)
	}
	if acct != liq || pos != liqPosID || kind != "ADL" || side != "LONG" {
		t.Fatalf("liq event row: acct=%d pos=%d kind=%s side=%s", acct, pos, kind, side)
	}
	// The directive links back to the counterparty event.
	var linked *int64
	if err := pool.QueryRow(ctx,
		`SELECT liquidation_event_id FROM adl_directives WHERE adl_seq=$1`, seq).
		Scan(&linked); err != nil || linked == nil || *linked != res.CounterpartyEventID {
		t.Fatalf("event link: %v %v", linked, err)
	}
	// Replay dedups — second fill for the same seq is a no-op.
	res2, err := store.CompleteADLFill(ctx, ADLFillReport{
		AdlSeq: seq, FilledQty: decimal.NewFromInt(100),
		FillPrice: decimal.RequireFromString("1.00")})
	if err != nil || !res2.AlreadyReconciled {
		t.Fatalf("replay must dedupe: %v %+v", err, res2)
	}
	var evCount int
	if err := pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM liquidation_events le
		JOIN adl_directives d ON d.liquidation_event_id = le.id
		WHERE d.adl_seq=$1`, seq).Scan(&evCount); err != nil || evCount != 1 {
		t.Fatalf("event count after replay: %d %v", evCount, err)
	}
}

func TestPgADLStoreCompleteFillGuards(t *testing.T) {
	_, pool := liqStoreFixture(t)
	ctx := context.Background()
	store, err := NewPgADLStore(pool)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	cp := liqSeedAccount(t, pool, "RETAIL")
	inst := liqSeedInstrument(t, pool)
	liqSeedPosition(t, pool, cp, inst, "SHORT", "-100", "2.00",
		liqStr("1.00"), nil, "100", "7")
	seq, err := store.InsertADLDirective(ctx, ADLDirective{
		AccountID: cp, InstrumentID: inst, Symbol: "EUR/USD", Side: "SHORT",
		Qty:             decimal.NewFromInt(100),
		BankruptcyPrice: decimal.One, Score: decimal.NewFromInt(3), Quintile: 3,
	})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	// Over-fill must fail closed.
	if _, err := store.CompleteADLFill(ctx, ADLFillReport{
		AdlSeq: seq, FilledQty: decimal.NewFromInt(101), FillPrice: decimal.One}); err == nil {
		t.Fatal("over-fill must error")
	}
	// Unknown seq → error.
	if _, err := store.CompleteADLFill(ctx, ADLFillReport{
		AdlSeq: 424242, FilledQty: decimal.One, FillPrice: decimal.One}); err == nil {
		t.Fatal("unknown directive must error")
	}
}

func TestPgADLStoreCompleteFillNoPositionFails(t *testing.T) {
	_, pool := liqStoreFixture(t)
	ctx := context.Background()
	store, err := NewPgADLStore(pool)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	orphan := liqSeedAccount(t, pool, "RETAIL") // account exists, position row does not
	inst := liqSeedInstrument(t, pool)
	seq, err := store.InsertADLDirective(ctx, ADLDirective{
		AccountID: orphan, InstrumentID: inst, Symbol: "EUR/USD", Side: "SHORT",
		Qty:             decimal.NewFromInt(10),
		BankruptcyPrice: decimal.One, Score: decimal.NewFromInt(2), Quintile: 2,
	})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	if _, err := store.CompleteADLFill(ctx, ADLFillReport{
		AdlSeq: seq, FilledQty: decimal.NewFromInt(10), FillPrice: decimal.One}); err == nil {
		t.Fatal("fill without a counterparty position row must fail closed")
	}
	// Directive stayed DISPATCHED-eligible (not FILLED).
	d, _ := store.ADLDirectiveBySeq(ctx, seq)
	if d.Status == ADLStatusFilled {
		t.Fatal("failed reconcile must not mark FILLED")
	}
}

// ---------------------------------------------------------------------------
// Redis-gated integration — ADLIndicatorPublisher (EXC_REDIS_TEST=1, db 14)
// ---------------------------------------------------------------------------

func adlTestRedis(t *testing.T) *excredis.Client {
	t.Helper()
	if os.Getenv("EXC_REDIS_TEST") != "1" {
		t.Skip("EXC_REDIS_TEST not set")
	}
	addr := os.Getenv("EXC_REDIS_TEST_ADDR")
	if addr == "" {
		addr = "127.0.0.1:16379"
	}
	rdb := excredis.New(addr, os.Getenv("EXC_REDIS_TEST_PASSWORD"), 14)
	if err := rdb.Ping(context.Background()); err != nil {
		t.Skipf("redis unreachable: %v", err)
	}
	return rdb
}

func TestRedisADLPublisherTick(t *testing.T) {
	rdb := adlTestRedis(t)
	ctx := context.Background()
	// Clean slate for the keys this test touches.
	t.Cleanup(func() {
		for _, k := range []string{
			AdlIndicatorKey(71), AdlIndicatorKey(72),
			AdlPriorityKey("EUR/USD", "LONG"), AdlPriorityKey("GBP/USD", "SHORT"),
		} {
			_ = rdb.Del(ctx, k).Err()
		}
	})

	universe := &fakeADLUniverse{positions: []LiqPosition{
		adlPos(501, 71, 10, "EUR/USD", "LONG", "100", "1.0", "1.0", "CROSS"),
		adlPos(502, 72, 11, "GBP/USD", "SHORT", "100", "1.0", "1.0", "CROSS"),
	}}
	scorer := adlTestScorer(
		map[string]decimal.Decimal{
			"EUR/USD": decimal.RequireFromString("2.0"), // +100% → profitable
			"GBP/USD": decimal.RequireFromString("2.0"), // SHORT at 2× entry → losing
		},
		map[[2]int64]int{{71, 10}: 30, {72, 11}: 30})
	pub, err := NewADLIndicatorPublisher(ADLPublisherDeps{
		Redis: rdb, Universe: universe, Scorer: scorer})
	if err != nil {
		t.Fatalf("publisher: %v", err)
	}

	stats, err := pub.TickOnce(ctx)
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if stats.Positions != 2 || stats.Ranked != 1 || stats.Accounts != 2 || stats.PriorityKeys != 1 {
		t.Fatalf("stats: %+v", stats)
	}

	// Indicator hash matches the RedisADLIndicatorReader decoding:
	// field = position_id, value = "1".."5".
	m, err := rdb.HGetAll(ctx, AdlIndicatorKey(71)).Result()
	if err != nil {
		t.Fatalf("hgetall: %v", err)
	}
	if len(m) != 1 || m["501"] != "5" {
		t.Fatalf("acct 71 hash: %v", m)
	}
	m, err = rdb.HGetAll(ctx, AdlIndicatorKey(72)).Result()
	if err != nil {
		t.Fatalf("hgetall: %v", err)
	}
	if len(m) != 1 || m["502"] != "1" {
		t.Fatalf("acct 72 hash: %v", m)
	}
	for field, raw := range m {
		if _, err := strconv.ParseInt(field, 10, 64); err != nil {
			t.Fatalf("field %q not a position id", field)
		}
		if q, err := strconv.Atoi(raw); err != nil || q < 1 || q > 5 {
			t.Fatalf("field %q value %q not a quintile", field, raw)
		}
	}

	// Priority ZSET carries the profitable member only.
	zs, err := rdb.ZRangeWithScores(ctx, AdlPriorityKey("EUR/USD", "LONG"), 0, -1).Result()
	if err != nil {
		t.Fatalf("zrange: %v", err)
	}
	if len(zs) != 1 || fmt.Sprint(zs[0].Member) != "71" || zs[0].Score <= 0 {
		t.Fatalf("priority zset: %+v", zs)
	}
	exists, err := rdb.Exists(ctx, AdlPriorityKey("GBP/USD", "SHORT")).Result()
	if err != nil || exists != 0 {
		t.Fatalf("losing side must not have a priority zset (exists=%d)", exists)
	}

	// Second tick after acct 72's position closes → its hash is deleted
	// in the same MULTI (stale-key cleanup).
	universe.positions = universe.positions[:1]
	stats, err = pub.TickOnce(ctx)
	if err != nil {
		t.Fatalf("tick 2: %v", err)
	}
	if stats.KeysRemoved != 1 {
		t.Fatalf("stale cleanup: %+v", stats)
	}
	exists, err = rdb.Exists(ctx, AdlIndicatorKey(72)).Result()
	if err != nil || exists != 0 {
		t.Fatalf("closed account hash must be deleted (exists=%d)", exists)
	}
}
