// collateral_test.go — Task 19.3.8 unit + gated integration tests.
//
// Coverage map (task DoD):
//   - haircut-adjusted equity through MarginService.Evaluate (§24 #145)
//   - concentration limit zero-weights excess single-currency collateral
//   - ineligible / absent currencies contribute zero
//   - schedule admin update validated, audit-persisted, cache ≤5s
//   - fail-closed: cold schedule read failure aborts; stale-oracle ccy
//     contributes zero (never inflated)
//   - debit balances valued at face (a liability is not collateral)
package risk

import (
	"context"
	stderrors "errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"exchange/pkg/decimal"
	excerrors "exchange/pkg/errors"
)

// ---------------------------------------------------------------------------
// Shared margin-rig fakes (used by collateral/volatility/correlation tests)
// ---------------------------------------------------------------------------

// marginStoreFake implements MarginStore over in-memory rows.
type marginStoreFake struct {
	account   *MarginAccount
	category  string
	balances  []BalanceAmount
	positions []MarginPosition
	pairs     map[string]FxPair
	getErr    error
	wrote     []MarginSnapshot
}

func (f *marginStoreFake) MarginAccount(context.Context, int64) (*MarginAccount, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	return f.account, nil
}
func (f *marginStoreFake) SetMarginMode(context.Context, int64, MarginMode) error { return nil }
func (f *marginStoreFake) OpenPositionCount(context.Context, int64) (int64, error) {
	return int64(len(f.positions)), nil
}
func (f *marginStoreFake) AccountCategory(context.Context, int64) (string, error) {
	if f.category == "" {
		return CategoryRetail, nil
	}
	return f.category, nil
}
func (f *marginStoreFake) Balances(context.Context, int64) ([]BalanceAmount, error) {
	return f.balances, nil
}
func (f *marginStoreFake) MarginPositions(context.Context, int64) ([]MarginPosition, error) {
	return f.positions, nil
}
func (f *marginStoreFake) FxPairInstruments(_ context.Context, ccys []string) (map[string]FxPair, error) {
	out := map[string]FxPair{}
	for _, c := range ccys {
		if fp, ok := f.pairs[c]; ok {
			out[c] = fp
		}
	}
	return out, nil
}
func (f *marginStoreFake) OpenPositionIndex(context.Context) (map[string][]int64, error) {
	return map[string][]int64{}, nil
}
func (f *marginStoreFake) WriteMarginSnapshot(_ context.Context, s MarginSnapshot) error {
	f.wrote = append(f.wrote, s)
	return nil
}

// markCacheFake implements MarkCache over a static map.
type markCacheFake struct {
	m   map[string]decimal.Decimal
	err error
}

func (f markCacheFake) BatchMarks(context.Context, []string) (map[string]decimal.Decimal, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.m, nil
}

// opsAlertSpy captures OpsAlerter.Raise calls.
type opsAlertSpy struct {
	mu     sync.Mutex
	alerts []OpsAlert
}

func (a *opsAlertSpy) Raise(_ context.Context, al OpsAlert) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.alerts = append(a.alerts, al)
	return nil
}
func (a *opsAlertSpy) count() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.alerts)
}

// collateralStoreFake implements CollateralScheduleStore.
type collateralStoreFake struct {
	mu     sync.Mutex
	rows   []CollateralScheduleEntry
	getErr error
	puts   []collateralPutCall
}
type collateralPutCall struct {
	actor  int64
	rows   []CollateralScheduleEntry
	before []CollateralScheduleEntry
}

func (f *collateralStoreFake) CollateralSchedule(context.Context) ([]CollateralScheduleEntry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.getErr != nil {
		return nil, f.getErr
	}
	return append([]CollateralScheduleEntry(nil), f.rows...), nil
}
func (f *collateralStoreFake) PutCollateralSchedule(_ context.Context, actor int64,
	rows, before []CollateralScheduleEntry) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.puts = append(f.puts, collateralPutCall{actor, rows, before})
	// Upsert semantics for the fake.
	byCcy := map[string]CollateralScheduleEntry{}
	for _, r := range f.rows {
		byCcy[r.Currency] = r
	}
	for _, r := range rows {
		byCcy[r.Currency] = r
	}
	f.rows = f.rows[:0]
	for _, r := range byCcy {
		f.rows = append(f.rows, r)
	}
	return nil
}

// testSchedule mirrors the migration-041 seed subset used by the tests.
func testSchedule() []CollateralScheduleEntry {
	return []CollateralScheduleEntry{
		{Currency: "USD", Eligible: true, HaircutPct: d("0.00"), MaxConcentrationPct: d("100.00")},
		{Currency: "EUR", Eligible: true, HaircutPct: d("0.50"), MaxConcentrationPct: d("40.00")},
		{Currency: "JPY", Eligible: true, HaircutPct: d("1.00"), MaxConcentrationPct: d("30.00")},
		{Currency: "TRY", Eligible: true, HaircutPct: d("10.00"), MaxConcentrationPct: d("10.00")},
		{Currency: "ZZZ", Eligible: false, HaircutPct: d("100.00"), MaxConcentrationPct: d("10.00")},
	}
}

func newCollateralSvc(t *testing.T, store *collateralStoreFake, alerts *opsAlertSpy) *CollateralService {
	t.Helper()
	svc, err := NewCollateralService(CollateralDeps{Store: store, Alerter: alerts})
	if err != nil {
		t.Fatalf("collateral service: %v", err)
	}
	return svc
}

// rateFunc is a static currency→USD rate map for Valuate tests.
func rateFunc(rates map[string]string) func(string) (decimal.Decimal, bool) {
	return func(ccy string) (decimal.Decimal, bool) {
		if ccy == "USD" {
			return decimal.One, true
		}
		if s, ok := rates[ccy]; ok {
			return d(s), true
		}
		return decimal.Zero, false
	}
}

// ---------------------------------------------------------------------------
// Valuation
// ---------------------------------------------------------------------------

func TestCollateralValuateHaircut(t *testing.T) {
	svc := newCollateralSvc(t, &collateralStoreFake{rows: testSchedule()}, nil)
	v, err := svc.Valuate(context.Background(), []BalanceAmount{
		{Currency: "USD", Available: d("20000")},
		{Currency: "EUR", Available: d("5000")},
	}, rateFunc(map[string]string{"EUR": "1.20"}))
	if err != nil {
		t.Fatal(err)
	}
	// EUR adj = 5000 × 1.20 × (1 − 0.005) = 5970; gross = 25970; EUR
	// share 23% < 40% cap → equity 25970.
	if !v.EquityUSD.Equal(d("25970.00000000")) && !v.EquityUSD.Equal(d("25970")) {
		t.Fatalf("equity = %s, want 25970", v.EquityUSD)
	}
	if len(v.Concentrated) != 0 || len(v.Ineligible) != 0 || len(v.Unpriced) != 0 {
		t.Fatalf("unexpected exclusions: %+v", v)
	}
}

func TestCollateralConcentrationCapsExcess(t *testing.T) {
	svc := newCollateralSvc(t, &collateralStoreFake{rows: testSchedule()}, nil)
	v, err := svc.Valuate(context.Background(), []BalanceAmount{
		{Currency: "USD", Available: d("10000")},
		{Currency: "EUR", Available: d("50000")},
	}, rateFunc(map[string]string{"EUR": "1.20"}))
	if err != nil {
		t.Fatal(err)
	}
	// EUR adj = 59700; gross = 69700; cap 40% → allowed 27880; excess
	// 31820 zero-weighted → equity 37880.
	if !v.EquityUSD.Equal(d("37880")) {
		t.Fatalf("equity = %s, want 37880", v.EquityUSD)
	}
	if len(v.Concentrated) != 1 || v.Concentrated[0] != "EUR" {
		t.Fatalf("concentrated = %v, want [EUR]", v.Concentrated)
	}
}

func TestCollateralFullConcentrationSingleCurrency(t *testing.T) {
	svc := newCollateralSvc(t, &collateralStoreFake{rows: testSchedule()}, nil)
	v, err := svc.Valuate(context.Background(), []BalanceAmount{
		{Currency: "JPY", Available: d("100000")},
	}, rateFunc(map[string]string{"JPY": "0.007"}))
	if err != nil {
		t.Fatal(err)
	}
	// 100% JPY: adj = 100000×0.007×0.99 = 693; cap 30% → 207.9.
	if !v.EquityUSD.Equal(d("207.9")) {
		t.Fatalf("equity = %s, want 207.9", v.EquityUSD)
	}
}

func TestCollateralIneligibleAndAbsentContributeZero(t *testing.T) {
	svc := newCollateralSvc(t, &collateralStoreFake{rows: testSchedule()}, nil)
	v, err := svc.Valuate(context.Background(), []BalanceAmount{
		{Currency: "USD", Available: d("1000")},
		{Currency: "ZZZ", Available: d("999")}, // eligible=false
		{Currency: "ABC", Available: d("999")}, // absent from schedule
	}, rateFunc(map[string]string{"ZZZ": "1", "ABC": "1"}))
	if err != nil {
		t.Fatal(err)
	}
	if !v.EquityUSD.Equal(d("1000")) {
		t.Fatalf("equity = %s, want 1000", v.EquityUSD)
	}
	if len(v.Ineligible) != 2 {
		t.Fatalf("ineligible = %v, want [ABC ZZZ]", v.Ineligible)
	}
}

func TestCollateralStaleRateContributesZero(t *testing.T) {
	svc := newCollateralSvc(t, &collateralStoreFake{rows: testSchedule()}, nil)
	v, err := svc.Valuate(context.Background(), []BalanceAmount{
		{Currency: "USD", Available: d("1000")},
		{Currency: "EUR", Available: d("5000")},
	}, rateFunc(map[string]string{})) // no EUR rate — stale oracle case
	if err != nil {
		t.Fatal(err)
	}
	if !v.EquityUSD.Equal(d("1000")) {
		t.Fatalf("equity = %s, want 1000 — stale-rate ccy must not inflate", v.EquityUSD)
	}
	if len(v.Unpriced) != 1 || v.Unpriced[0] != "EUR" {
		t.Fatalf("unpriced = %v, want [EUR]", v.Unpriced)
	}
}

func TestCollateralDebitBalanceFaceValue(t *testing.T) {
	svc := newCollateralSvc(t, &collateralStoreFake{rows: testSchedule()}, nil)
	v, err := svc.Valuate(context.Background(), []BalanceAmount{
		{Currency: "USD", Available: d("1000")},
		{Currency: "TRY", Locked: d("0"), Available: d("-1000")}, // debit
	}, rateFunc(map[string]string{"TRY": "0.05"}))
	if err != nil {
		t.Fatal(err)
	}
	// Debit at FACE: −1000×0.05 = −50 → equity 950 (haircutting a
	// liability would understate the debt).
	if !v.EquityUSD.Equal(d("950")) {
		t.Fatalf("equity = %s, want 950", v.EquityUSD)
	}
}

// ---------------------------------------------------------------------------
// MarginService integration — the haircut leg replaces face value
// ---------------------------------------------------------------------------

func newMarginSvc(t *testing.T, o MarginOptions) *MarginService {
	t.Helper()
	svc, err := NewMarginService(o)
	if err != nil {
		t.Fatalf("margin service: %v", err)
	}
	return svc
}

func marginFixture() (*marginStoreFake, markCacheFake) {
	store := &marginStoreFake{
		category: CategoryRetail,
		balances: []BalanceAmount{
			{Currency: "USD", Available: d("10000")},
			{Currency: "EUR", Available: d("50000")},
			{Currency: "ZZZ", Available: d("999")},
		},
		pairs: map[string]FxPair{
			"EUR": {Symbol: "EUR/USD"},
			"ZZZ": {Symbol: "ZZZ/USD"},
		},
	}
	marks := markCacheFake{m: map[string]decimal.Decimal{
		"EUR/USD": d("1.20"), "ZZZ/USD": d("1"),
	}}
	return store, marks
}

func TestMarginEvaluateCollateralLeg(t *testing.T) {
	store, marks := marginFixture()
	collateral := newCollateralSvc(t, &collateralStoreFake{rows: testSchedule()}, nil)
	svc := newMarginSvc(t, MarginOptions{Store: store, Marks: marks, Collateral: collateral})

	snap, err := svc.Evaluate(context.Background(), 7)
	if err != nil {
		t.Fatal(err)
	}
	// EUR adj=59700, gross=69700, cap40%=27880; USD 10000 → 37880.
	if !snap.Equity.Equal(d("37880")) {
		t.Fatalf("equity = %s, want 37880 (haircut+concentration adjusted)", snap.Equity)
	}
	if len(snap.Concentrated) != 1 || snap.Concentrated[0] != "EUR" {
		t.Fatalf("concentrated = %v, want [EUR]", snap.Concentrated)
	}
	if len(snap.Unvalued) != 1 || snap.Unvalued[0] != "ZZZ" {
		t.Fatalf("unvalued = %v, want [ZZZ]", snap.Unvalued)
	}
}

func TestMarginEvaluateNilCollateralKeepsFaceValue(t *testing.T) {
	store, marks := marginFixture()
	svc := newMarginSvc(t, MarginOptions{Store: store, Marks: marks})
	snap, err := svc.Evaluate(context.Background(), 7)
	if err != nil {
		t.Fatal(err)
	}
	// Face value: 10000 + 50000×1.20 + 999×1 = 70999.
	if !snap.Equity.Equal(d("70999")) {
		t.Fatalf("equity = %s, want 70999 (face value legacy leg)", snap.Equity)
	}
}

func TestMarginEvaluateCollateralErrorFailsClosed(t *testing.T) {
	store, marks := marginFixture()
	collateral := newCollateralSvc(t,
		&collateralStoreFake{getErr: fmt.Errorf("pg down")}, nil)
	svc := newMarginSvc(t, MarginOptions{Store: store, Marks: marks, Collateral: collateral})
	if _, err := svc.Evaluate(context.Background(), 7); err == nil {
		t.Fatal("schedule read failure with cold cache must abort the evaluation")
	}
}

// ---------------------------------------------------------------------------
// Admin update — validation, audit call, ≤5s cache propagation
// ---------------------------------------------------------------------------

func TestCollateralUpdateScheduleAuditAndPropagate(t *testing.T) {
	store := &collateralStoreFake{rows: testSchedule()}
	svc := newCollateralSvc(t, store, nil)
	ctx := context.Background()

	// Warm the cache with the current schedule.
	if _, err := svc.Valuate(ctx, []BalanceAmount{{Currency: "EUR", Available: d("1")}},
		rateFunc(map[string]string{"EUR": "1"})); err != nil {
		t.Fatal(err)
	}
	// Update EUR haircut 0.5% → 2%.
	err := svc.UpdateSchedule(ctx, 9001, []CollateralScheduleEntry{
		{Currency: "EUR", Eligible: true, HaircutPct: d("2.00"), MaxConcentrationPct: d("40.00")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(store.puts) != 1 || store.puts[0].actor != 9001 {
		t.Fatalf("audit-persist call missing: %+v", store.puts)
	}
	if len(store.puts[0].before) == 0 {
		t.Fatal("before image not captured for the audit row")
	}
	// Cache invalidated — the next Valuate must see haircut 2%.
	v, err := svc.Valuate(ctx, []BalanceAmount{{Currency: "EUR", Available: d("100")}},
		rateFunc(map[string]string{"EUR": "1"}))
	if err != nil {
		t.Fatal(err)
	}
	// EUR adj = 98; gross=98; cap 40% → 39.2.
	if !v.EquityUSD.Equal(d("39.2")) {
		t.Fatalf("equity = %s, want 39.2 (updated haircut applied)", v.EquityUSD)
	}
}

func TestCollateralUpdateScheduleValidation(t *testing.T) {
	store := &collateralStoreFake{rows: testSchedule()}
	svc := newCollateralSvc(t, store, nil)
	ctx := context.Background()

	requireCode(t, svc.UpdateSchedule(ctx, 0, []CollateralScheduleEntry{
		{Currency: "EUR", Eligible: true, HaircutPct: d("1"), MaxConcentrationPct: d("40")},
	}), "UNAUTHORIZED_ROLE")

	var e *excerrors.Error
	err := svc.UpdateSchedule(ctx, 1, nil)
	if !stderrors.As(err, &e) || e.Code != CodeInvalidRequest {
		t.Fatalf("empty rows: %v", err)
	}
	err = svc.UpdateSchedule(ctx, 1, []CollateralScheduleEntry{
		{Currency: "eur", Eligible: true, HaircutPct: d("1"), MaxConcentrationPct: d("40")},
	})
	if !stderrors.As(err, &e) || e.Code != CodeInvalidRequest {
		t.Fatalf("lowercase ccy: %v", err)
	}
	err = svc.UpdateSchedule(ctx, 1, []CollateralScheduleEntry{
		{Currency: "EUR", Eligible: true, HaircutPct: d("150"), MaxConcentrationPct: d("40")},
	})
	if !stderrors.As(err, &e) || e.Code != CodeInvalidRequest {
		t.Fatalf("haircut>100: %v", err)
	}
	err = svc.UpdateSchedule(ctx, 1, []CollateralScheduleEntry{
		{Currency: "EUR", Eligible: true, HaircutPct: d("1"), MaxConcentrationPct: d("0")},
	})
	if !stderrors.As(err, &e) || e.Code != CodeInvalidRequest {
		t.Fatalf("concentration=0: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Fail-closed schedule reads — cold failure aborts, warm cache degrades
// ---------------------------------------------------------------------------

func TestCollateralScheduleColdFailureFailsClosed(t *testing.T) {
	svc := newCollateralSvc(t, &collateralStoreFake{getErr: fmt.Errorf("pg down")}, nil)
	_, err := svc.Valuate(context.Background(),
		[]BalanceAmount{{Currency: "USD", Available: d("1")}}, rateFunc(nil))
	if err == nil {
		t.Fatal("cold schedule read failure must fail the valuation")
	}
}

func TestCollateralScheduleStaleCacheFallback(t *testing.T) {
	store := &collateralStoreFake{rows: testSchedule()}
	alerts := &opsAlertSpy{}
	svc := newCollateralSvc(t, store, alerts)
	ctx := context.Background()
	if _, err := svc.Valuate(ctx, []BalanceAmount{{Currency: "USD", Available: d("1")}},
		rateFunc(nil)); err != nil {
		t.Fatal(err)
	}
	// Expire the TTL by rewinding cacheAt, then break the store.
	svc.mu.Lock()
	svc.cacheAt = time.Now().Add(-time.Hour)
	svc.mu.Unlock()
	store.mu.Lock()
	store.getErr = fmt.Errorf("pg down")
	store.mu.Unlock()
	// Stale schedule still applies (USD 0% haircut).
	v, err := svc.Valuate(ctx, []BalanceAmount{{Currency: "USD", Available: d("100")}},
		rateFunc(nil))
	if err != nil {
		t.Fatalf("warm cache must degrade to stale schedule: %v", err)
	}
	if !v.EquityUSD.Equal(d("100")) {
		t.Fatalf("equity = %s, want 100", v.EquityUSD)
	}
	if alerts.count() != 1 {
		t.Fatalf("expected 1 degraded alert, got %d", alerts.count())
	}
}

// ---------------------------------------------------------------------------
// Gated Postgres integration — real collateral_schedule + admin_audit_log
// ---------------------------------------------------------------------------

// TestPgCollateralScheduleStore round-trips the migration-041 table and
// proves the audit row commits with the upsert. Run:
// EXC_PG_TEST=1 go test ./internal/risk/ -run TestPgCollateral -v
func TestPgCollateralScheduleStore(t *testing.T) {
	if os.Getenv("EXC_PG_TEST") != "1" {
		t.Skip("EXC_PG_TEST not set")
	}
	dsn := os.Getenv("EXC_PG_DSN")
	if dsn == "" {
		dsn = "postgres://exchange:exchange_dev@localhost:5433/exchange?sslmode=disable"
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("pg pool: %v", err)
	}
	defer pool.Close()
	ctx := context.Background()
	for _, table := range []string{"collateral_schedule", "admin_audit_log"} {
		var exists bool
		if err := pool.QueryRow(ctx,
			`SELECT to_regclass($1) IS NOT NULL`, table).Scan(&exists); err != nil {
			t.Fatalf("regclass %s: %v", table, err)
		}
		if !exists {
			t.Skipf("%s not present in scratch database", table)
		}
	}
	st, err := NewPgCollateralScheduleStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	before, err := st.CollateralSchedule(ctx)
	if err != nil {
		t.Fatalf("schedule read: %v", err)
	}
	// Upsert a scratch currency row + audit; clean up after.
	row := CollateralScheduleEntry{Currency: "ITX", Eligible: true,
		HaircutPct: d("3.00"), MaxConcentrationPct: d("15.00")}
	if err := st.PutCollateralSchedule(ctx, 4242,
		[]CollateralScheduleEntry{row}, before); err != nil {
		t.Fatalf("put schedule: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM collateral_schedule WHERE currency='ITX'`)
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM admin_audit_log WHERE action='risk.collateral_schedule.update' AND admin_user_id=4242`)
	})
	var eligible bool
	var haircut, conc string
	if err := pool.QueryRow(ctx, `
		SELECT eligible, haircut_pct::text, max_concentration_pct::text
		  FROM collateral_schedule WHERE currency='ITX'`).
		Scan(&eligible, &haircut, &conc); err != nil {
		t.Fatalf("upserted row read: %v", err)
	}
	if !eligible || haircut != "3.00" || conc != "15.00" {
		t.Fatalf("upsert mismatch: eligible=%v haircut=%s conc=%s", eligible, haircut, conc)
	}
	var afterOK bool
	if err := pool.QueryRow(ctx, `
		SELECT after_state @> '[{"currency":"ITX"}]'::jsonb
		  FROM admin_audit_log
		  WHERE action='risk.collateral_schedule.update' AND admin_user_id=4242
		  ORDER BY id DESC LIMIT 1`).Scan(&afterOK); err != nil {
		t.Fatalf("audit read: %v", err)
	}
	if !afterOK {
		t.Fatalf("audit after_state missing ITX row")
	}
}
