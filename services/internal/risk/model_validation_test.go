// model_validation_test.go — unit + PG-gated coverage for Tasks
// 19.3.13 + 19.3.21: the ParamChangeGate, run/param-change stores,
// through-the-cycle clamps, add-ons, insurance calibration, custody
// verification and the intraday client-money guard.
//
//	EXC_PG_TEST=1 go test ./internal/risk/ -run 'TestPgModelRun|TestPgParamChange' -v
package risk

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"exchange/internal/ledger"
	"exchange/pkg/decimal"
	excerrors "exchange/pkg/errors"
)

// ---------------------------------------------------------------------------
// Fakes
// ---------------------------------------------------------------------------

type fakeRunStore struct {
	runs map[int64]*ModelRun
	next int64
}

func newFakeRunStore() *fakeRunStore { return &fakeRunStore{runs: map[int64]*ModelRun{}, next: 1} }

func (f *fakeRunStore) InsertRun(_ context.Context, r ModelRun) (int64, error) {
	id := f.next
	f.next++
	cp := r
	cp.RunID = id
	f.runs[id] = &cp
	return id, nil
}

func (f *fakeRunStore) RunByID(_ context.Context, id int64) (*ModelRun, error) {
	r := f.runs[id]
	if r == nil {
		return nil, nil
	}
	cp := *r
	return &cp, nil
}

func (f *fakeRunStore) LatestRun(_ context.Context, kind string) (*ModelRun, error) {
	var best *ModelRun
	for _, r := range f.runs {
		if r.Kind != kind {
			continue
		}
		if best == nil || r.CreatedAt.After(best.CreatedAt) ||
			(r.CreatedAt.Equal(best.CreatedAt) && r.RunID > best.RunID) {
			best = r
		}
	}
	if best == nil {
		return nil, nil
	}
	cp := *best
	return &cp, nil
}

func (f *fakeRunStore) SumBreaches(_ context.Context, kind string, since time.Time) (int, error) {
	n := 0
	for _, r := range f.runs {
		if r.Kind == kind && !r.CreatedAt.Before(since) {
			n += r.BreachCount
		}
	}
	return n, nil
}

func (f *fakeRunStore) RecordReview(_ context.Context, runID, reviewerID int64) error {
	r := f.runs[runID]
	if r == nil || r.ReviewedBy != nil {
		return excerrors.New(codeDualControlRequired, "already reviewed or missing")
	}
	r.ReviewedBy = &reviewerID
	now := time.Now().UTC()
	r.ReviewedAt = &now
	return nil
}

func (f *fakeRunStore) ListRuns(_ context.Context, kind string, since time.Time) ([]ModelRun, error) {
	var out []ModelRun
	for _, r := range f.runs {
		if r.Kind == kind && !r.CreatedAt.Before(since) {
			out = append(out, *r)
		}
	}
	return out, nil
}

type fakeChangeStore struct {
	rows map[int64]ParamChange
	stat map[int64]string
	next int64
}

func newFakeChangeStore() *fakeChangeStore {
	return &fakeChangeStore{rows: map[int64]ParamChange{}, stat: map[int64]string{}, next: 1}
}

func (f *fakeChangeStore) InsertChange(_ context.Context, c ParamChange) (int64, error) {
	id := f.next
	f.next++
	c.ChangeID = id
	f.rows[id] = c
	f.stat[id] = ChangeStatusPending
	return id, nil
}

func (f *fakeChangeStore) DecideChange(_ context.Context, id int64, status, _ string) error {
	f.stat[id] = status
	return nil
}

type captureAlerter struct{ alerts []OpsAlert }

func (c *captureAlerter) Raise(_ context.Context, a OpsAlert) error {
	c.alerts = append(c.alerts, a)
	return nil
}

type captureSink struct{ reports []ClientMoneyReport }

func (c *captureSink) RecordReport(_ context.Context, r ClientMoneyReport) error {
	c.reports = append(c.reports, r)
	return nil
}

var testNow = time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)

// ---------------------------------------------------------------------------
// ParamChangeGate
// ---------------------------------------------------------------------------

func gateWith(t *testing.T, store ModelRunStore) *ParamChangeGate {
	t.Helper()
	g, err := NewParamChangeGate(store, newFakeChangeStore(), 0,
		func() time.Time { return testNow })
	if err != nil {
		t.Fatalf("gate: %v", err)
	}
	return g
}

func reviewedPassRun(store *fakeRunStore, reviewer int64, age time.Duration, param string) int64 {
	var p *string
	if param != "" {
		p = &param
	}
	id, _ := store.InsertRun(context.Background(), ModelRun{
		Kind: RunKindStress, Scenario: "RATE_SHOCK_-3SIGMA", Status: RunStatusPass,
		BreachCount: 0, ParamChange: p, CreatedAt: testNow.Add(-age),
	})
	_ = store.RecordReview(context.Background(), id, reviewer)
	return id
}

func TestParamChangeGateRejectsUnvalidated(t *testing.T) {
	ctx := context.Background()
	store := newFakeRunStore()
	g := gateWith(t, store)
	base := ParamChange{Parameter: "auction_floor_decay", OwnerID: 11, ApprovedBy: 22}

	// No linked run.
	if err := g.ValidateParameterChange(ctx, base); excerrors.CodeOf(err) != CodeMarginModelUnvalidated {
		t.Fatalf("no run: want %s, got %v", CodeMarginModelUnvalidated, err)
	}
	// Missing run id.
	bad := base
	bad.RunID = 999
	if err := g.ValidateParameterChange(ctx, bad); excerrors.CodeOf(err) != CodeMarginModelUnvalidated {
		t.Fatalf("missing run: want %s, got %v", CodeMarginModelUnvalidated, err)
	}
	// FAIL run.
	failID, _ := store.InsertRun(ctx, ModelRun{Kind: RunKindStress, Scenario: "X",
		Status: RunStatusFail, CreatedAt: testNow})
	bad = base
	bad.RunID = failID
	if err := g.ValidateParameterChange(ctx, bad); excerrors.CodeOf(err) != CodeMarginModelUnvalidated {
		t.Fatalf("fail run: want %s, got %v", CodeMarginModelUnvalidated, err)
	}
	// Stale run.
	staleID := reviewedPassRun(store, 33, DefaultMaxRunAge+time.Hour, "")
	bad.RunID = staleID
	if err := g.ValidateParameterChange(ctx, bad); excerrors.CodeOf(err) != CodeMarginModelUnvalidated {
		t.Fatalf("stale run: want %s, got %v", CodeMarginModelUnvalidated, err)
	}
	// Unreviewed run — independence unprovable.
	unrevID, _ := store.InsertRun(ctx, ModelRun{Kind: RunKindStress, Scenario: "Y",
		Status: RunStatusPass, CreatedAt: testNow})
	bad.RunID = unrevID
	if err := g.ValidateParameterChange(ctx, bad); excerrors.CodeOf(err) != CodeMarginModelUnvalidated {
		t.Fatalf("unreviewed run: want %s, got %v", CodeMarginModelUnvalidated, err)
	}
	// Validator == owner — independence violated.
	selfID := reviewedPassRun(store, 11, time.Hour, "")
	bad.RunID = selfID
	if err := g.ValidateParameterChange(ctx, bad); excerrors.CodeOf(err) != CodeMarginModelUnvalidated {
		t.Fatalf("self-validated run: want %s, got %v", CodeMarginModelUnvalidated, err)
	}
	// Parameter mismatch — the run validated a different parameter.
	mismatchID := reviewedPassRun(store, 33, time.Hour, "leverage_tier:MAJOR")
	bad.RunID = mismatchID
	if err := g.ValidateParameterChange(ctx, bad); excerrors.CodeOf(err) != CodeMarginModelUnvalidated {
		t.Fatalf("param mismatch: want %s, got %v", CodeMarginModelUnvalidated, err)
	}
}

func TestParamChangeGateDualControl(t *testing.T) {
	ctx := context.Background()
	store := newFakeRunStore()
	g := gateWith(t, store)
	runID := reviewedPassRun(store, 33, time.Hour, "")

	// No approver.
	if err := g.ValidateParameterChange(ctx, ParamChange{
		Parameter: "x", OwnerID: 11, RunID: runID,
	}); excerrors.CodeOf(err) != "DUAL_CONTROL_REQUIRED" {
		t.Fatalf("no approver: want DUAL_CONTROL_REQUIRED, got %v", err)
	}
	// Approver == owner.
	if err := g.ValidateParameterChange(ctx, ParamChange{
		Parameter: "x", OwnerID: 11, ApprovedBy: 11, RunID: runID,
	}); excerrors.CodeOf(err) != "DUAL_CONTROL_REQUIRED" {
		t.Fatalf("self-approve: want DUAL_CONTROL_REQUIRED, got %v", err)
	}
	// Shape errors.
	if err := g.ValidateParameterChange(ctx, ParamChange{OwnerID: 1, ApprovedBy: 2, RunID: runID}); excerrors.CodeOf(err) != CodeInvalidRequest {
		t.Fatalf("missing parameter: %v", err)
	}
}

func TestParamChangeGateHappyPathAndAudit(t *testing.T) {
	ctx := context.Background()
	store := newFakeRunStore()
	changes := newFakeChangeStore()
	g, err := NewParamChangeGate(store, changes, 0, func() time.Time { return testNow })
	if err != nil {
		t.Fatalf("gate: %v", err)
	}
	runID := reviewedPassRun(store, 33, time.Hour, "auction_floor_decay")

	id, err := g.GateAndRecord(ctx, ParamChange{
		Parameter: "auction_floor_decay", OwnerID: 11, ApprovedBy: 22, RunID: runID,
	})
	if err != nil {
		t.Fatalf("validated change rejected: %v", err)
	}
	if changes.stat[id] != ChangeStatusValidated {
		t.Fatalf("audit status: %s", changes.stat[id])
	}
	// A rejected change leaves a REJECTED audit row.
	id2, err := g.GateAndRecord(ctx, ParamChange{
		Parameter: "auction_floor_decay", OwnerID: 11, ApprovedBy: 22, RunID: 4242,
	})
	if excerrors.CodeOf(err) != CodeMarginModelUnvalidated {
		t.Fatalf("phantom run must reject: %v", err)
	}
	if changes.stat[id2] != ChangeStatusRejected {
		t.Fatalf("reject audit status: %s", changes.stat[id2])
	}
}

// ---------------------------------------------------------------------------
// Through-the-cycle clamp + add-ons
// ---------------------------------------------------------------------------

func TestThroughTheCycleClamp(t *testing.T) {
	// Below the 0.5% floor → clamped up.
	c, fl, cap := ThroughTheCycleClamp(decimal.RequireFromString("0.001"),
		decimal.RequireFromString("0.01"))
	if !fl || cap || c.String() != "0.005" {
		t.Fatalf("floor clamp: %s fl=%v cap=%v", c, fl, cap)
	}
	// Above baseline×1.25 → clamped down (0.02 vs 0.01×1.25=0.0125).
	c, fl, cap = ThroughTheCycleClamp(decimal.RequireFromString("0.02"),
		decimal.RequireFromString("0.01"))
	if !cap || fl || c.String() != "0.0125" {
		t.Fatalf("cap clamp: %s fl=%v cap=%v", c, fl, cap)
	}
	// Inside → untouched.
	c, fl, cap = ThroughTheCycleClamp(decimal.RequireFromString("0.01"),
		decimal.RequireFromString("0.01"))
	if fl || cap || c.String() != "0.01" {
		t.Fatalf("pass-through: %s fl=%v cap=%v", c, fl, cap)
	}
}

func TestConcentrationAddon(t *testing.T) {
	base := decimal.NewFromInt(1000)
	// 20% of OI — under the 25% threshold → no add-on.
	if got := ConcentrationAddon(decimal.NewFromInt(200), decimal.NewFromInt(1000), base); !got.IsZero() {
		t.Fatalf("under threshold: %s", got)
	}
	// 50% of OI — excess 0.25 × 0.5 × 1000 = 125.
	got := ConcentrationAddon(decimal.NewFromInt(500), decimal.NewFromInt(1000), base)
	if got.String() != "125" {
		t.Fatalf("50%% share: want 125, got %s", got)
	}
	// Unknown OI → fully concentrated: excess 0.75 × 0.5 × 1000 = 375.
	got = ConcentrationAddon(decimal.NewFromInt(500), decimal.Zero, base)
	if got.String() != "375" {
		t.Fatalf("zero OI: want 375, got %s", got)
	}
}

func TestLiquidityAddon(t *testing.T) {
	base := decimal.NewFromInt(1000)
	// ≤1 day ADV → none.
	if got := LiquidityAddon(decimal.NewFromInt(900), decimal.NewFromInt(1000), base); !got.IsZero() {
		t.Fatalf("under a day: %s", got)
	}
	// 3 days → (3−1)×0.10 = 0.20 → 200.
	got := LiquidityAddon(decimal.NewFromInt(3000), decimal.NewFromInt(1000), base)
	if got.String() != "200" {
		t.Fatalf("3 days: want 200, got %s", got)
	}
	// Unknown ADV → cap → 500.
	got = LiquidityAddon(decimal.NewFromInt(3000), decimal.Zero, base)
	if got.String() != "500" {
		t.Fatalf("zero ADV: want 500, got %s", got)
	}
}

// ---------------------------------------------------------------------------
// Insurance calibration + custody
// ---------------------------------------------------------------------------

func TestCalibratedFundTarget(t *testing.T) {
	// 0.5% of $1B = $5M < $10M floor → $10M.
	got := CalibratedFundTarget(decimal.NewFromInt(1_000_000_000), decimal.Zero)
	if got.String() != "10000000" {
		t.Fatalf("floor: %s", got)
	}
	// Worst-1% shortfall above both → the stress metric wins.
	got = CalibratedFundTarget(decimal.NewFromInt(1_000_000_000), decimal.NewFromInt(20_000_000))
	if got.String() != "20000000" {
		t.Fatalf("stress leg: %s", got)
	}
	// 0.5% of $4B = $20M > $10M > $5M shortfall → equity leg.
	got = CalibratedFundTarget(decimal.NewFromInt(4_000_000_000), decimal.NewFromInt(5_000_000))
	if got.String() != "20000000" {
		t.Fatalf("equity leg: %s", got)
	}
}

func TestVerifySegregatedCustody(t *testing.T) {
	good := SegregatedCustody{Currency: "USD",
		GLAccount:          ledger.InsuranceFundNostro("USD"),
		ExternalAccountRef: "NOSTRO-USD-001", Segregated: true}
	if err := VerifySegregatedCustody(good); err != nil {
		t.Fatalf("good custody: %v", err)
	}
	// House operating nostro is not fund custody.
	bad := good
	bad.GLAccount = ledger.Nostro("USD")
	if err := VerifySegregatedCustody(bad); err == nil {
		t.Fatal("operating nostro must fail custody check")
	}
	// Unsegregated flag fails regardless of GL.
	bad = good
	bad.Segregated = false
	if err := VerifySegregatedCustody(bad); err == nil {
		t.Fatal("unsegregated flag must fail")
	}
}

// ---------------------------------------------------------------------------
// Client-money guard
// ---------------------------------------------------------------------------

func TestClientMoneyGuard(t *testing.T) {
	ctx := context.Background()
	alerter := &captureAlerter{}
	sink := &captureSink{}
	g := NewClientMoneyGuard(alerter, sink, func() time.Time { return testNow }, nil)

	// Held 110 vs required 100 → 110% ≥ 105% → clean.
	r, err := g.Evaluate(ctx, ClientMoneySnapshot{
		Currency: "USD", SegregatedHeld: decimal.NewFromInt(110),
		RequiredSegregation: decimal.NewFromInt(100),
	})
	if err != nil || r.BufferBreach || r.HardBreach {
		t.Fatalf("clean pass: %+v %v", r, err)
	}
	if r.Shortfall != "0" || r.RequiredWithBuffer != "105" {
		t.Fatalf("metrics: %+v", r)
	}
	// Held 104 → buffer breach (104 < 105) but not hard.
	r, err = g.Evaluate(ctx, ClientMoneySnapshot{
		Currency: "USD", SegregatedHeld: decimal.NewFromInt(104),
		RequiredSegregation: decimal.NewFromInt(100),
	})
	if err != nil || !r.BufferBreach || r.HardBreach || r.Shortfall != "1" {
		t.Fatalf("buffer breach: %+v %v", r, err)
	}
	// Held 90 → hard breach too.
	r, err = g.Evaluate(ctx, ClientMoneySnapshot{
		Currency: "USD", SegregatedHeld: decimal.NewFromInt(90),
		RequiredSegregation: decimal.NewFromInt(100),
	})
	if err != nil || !r.HardBreach || !r.BufferBreach {
		t.Fatalf("hard breach: %+v %v", r, err)
	}
	// 2 breach alerts (buffer + hard) so far.
	// Late transfer: pending for 2h → flagged + alerted.
	r, err = g.Evaluate(ctx, ClientMoneySnapshot{
		Currency: "EUR", SegregatedHeld: decimal.NewFromInt(200),
		RequiredSegregation: decimal.NewFromInt(100),
		PendingMarginTransfers: []PendingMarginTransfer{
			{ID: 7, Amount: decimal.NewFromInt(50), RequestedAt: testNow.Add(-2 * time.Hour)},
			{ID: 8, Amount: decimal.NewFromInt(10), RequestedAt: testNow.Add(-30 * time.Minute)},
		},
	})
	if err != nil || len(r.LateTransfers) != 1 || r.LateTransfers[0] != 7 {
		t.Fatalf("late transfers: %+v %v", r.LateTransfers, err)
	}
	if len(sink.reports) != 4 {
		t.Fatalf("sink rows: %d", len(sink.reports))
	}
	var shortfallAlerts, lateAlerts int
	for _, a := range alerter.alerts {
		switch a.Code {
		case codeClientMoneyShortfall:
			shortfallAlerts++
		case codeMarginTransferLate:
			lateAlerts++
		}
	}
	if shortfallAlerts != 2 || lateAlerts != 1 {
		t.Fatalf("alerts: shortfall=%d late=%d", shortfallAlerts, lateAlerts)
	}
}

func TestIntradayBufferMonitor(t *testing.T) {
	ctx := context.Background()
	src := &stubMoneySource{snaps: []ClientMoneySnapshot{
		{Currency: "USD", SegregatedHeld: decimal.NewFromInt(110), RequiredSegregation: decimal.NewFromInt(100)},
		{Currency: "EUR", SegregatedHeld: decimal.NewFromInt(50), RequiredSegregation: decimal.NewFromInt(100)},
	}}
	mon, err := NewIntradayBufferMonitor(NewClientMoneyGuard(nil, nil, nil, nil), src)
	if err != nil {
		t.Fatalf("monitor: %v", err)
	}
	reports, err := mon.TickOnce(ctx)
	if err != nil || len(reports) != 2 {
		t.Fatalf("tick: %v %d", err, len(reports))
	}
	if reports[0].BufferBreach || !reports[1].HardBreach {
		t.Fatalf("reports: %+v", reports)
	}
	if _, err := NewIntradayBufferMonitor(nil, src); err == nil {
		t.Fatal("nil guard must fail construction")
	}
}

type stubMoneySource struct{ snaps []ClientMoneySnapshot }

func (s *stubMoneySource) Snapshot(context.Context) ([]ClientMoneySnapshot, error) {
	return s.snaps, nil
}

func TestDiscloseNegativeInterest(t *testing.T) {
	// $1M at −50bps for 30 days ACT/360 → 1e6 × 0.005 × 30/360 = 416.66666667.
	d := DiscloseNegativeInterest("EUR", decimal.NewFromInt(1_000_000),
		decimal.NewFromInt(50), 30)
	if d.AmountDebited != "416.66666667" || d.DayBasis != 360 || d.PeriodDays != 30 {
		t.Fatalf("disclosure: %+v", d)
	}
	// Zero rate → disclosed zero line, not an absent line.
	d = DiscloseNegativeInterest("CHF", decimal.NewFromInt(1_000), decimal.Zero, 30)
	if d.AmountDebited != "0" {
		t.Fatalf("zero rate: %+v", d)
	}
}

// ---------------------------------------------------------------------------
// Revalidation report
// ---------------------------------------------------------------------------

func TestGenerateRevalidationReport(t *testing.T) {
	ctx := context.Background()
	store := newFakeRunStore()
	start := testNow.Add(-90 * 24 * time.Hour)
	end := testNow
	mk := func(kind, status string, breaches int, metrics string, at time.Time) {
		var raw []byte
		if metrics != "" {
			raw = []byte(metrics)
		}
		_, err := store.InsertRun(ctx, ModelRun{Kind: kind, Scenario: "S", Status: status,
			BreachCount: breaches, ResultMetrics: raw, CreatedAt: at})
		if err != nil {
			t.Fatal(err)
		}
	}
	mk(RunKindStress, RunStatusPass, 0, `{"fund_drawdown_usd":"100"}`, testNow.Add(-24*time.Hour))
	mk(RunKindStress, RunStatusFail, 3, `{"fund_drawdown_usd":"9000"}`, testNow.Add(-48*time.Hour))
	mk(RunKindBacktest, RunStatusPass, 0, "", testNow.Add(-24*time.Hour))
	mk(RunKindBacktest, RunStatusFail, 2, "", testNow.Add(-12*time.Hour))
	mk(RunKindStress, RunStatusPass, 0, "", testNow.Add(-200*24*time.Hour)) // outside window

	rep, err := GenerateRevalidationReport(ctx, store, start, end, func() time.Time { return testNow })
	if err != nil {
		t.Fatal(err)
	}
	if rep.StressRuns != 2 || rep.BacktestRuns != 2 || rep.FailedRuns != 2 || rep.TotalBreaches != 5 {
		t.Fatalf("report: %+v", rep)
	}
	if rep.WorstDrawdown != "9000" || len(rep.RunIDs) != 4 {
		t.Fatalf("report detail: %+v", rep)
	}
}

// ---------------------------------------------------------------------------
// PG-gated store tests — real migration 064 in a scratch schema
// ---------------------------------------------------------------------------

const modelRunTestDSN = "postgres://exchange:exchange_dev@127.0.0.1:5433/exchange?sslmode=disable"

func modelRunDSN() string {
	if d := os.Getenv("EXC_PG_DSN"); d != "" {
		return d
	}
	return modelRunTestDSN
}

func modelRunFixture(t *testing.T) (*PgModelRunStore, *PgParamChangeStore, *pgxpool.Pool) {
	t.Helper()
	if os.Getenv("EXC_PG_TEST") != "1" {
		t.Skip("EXC_PG_TEST not set")
	}
	dsn := modelRunDSN()
	schema := fmt.Sprintf("modelrun_itest_%d", time.Now().UnixNano())
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	boot, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Skipf("postgres unreachable (%v)", err)
	}
	if err := boot.Ping(ctx); err != nil {
		boot.Close(ctx)
		t.Skipf("postgres unreachable (%v)", err)
	}
	if _, err := boot.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		boot.Close(ctx)
		t.Fatalf("create schema: %v", err)
	}
	boot.Close(ctx)
	t.Cleanup(func() {
		c2, cancel2 := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel2()
		conn, err := pgx.Connect(c2, dsn)
		if err == nil {
			_, _ = conn.Exec(c2, "DROP SCHEMA IF EXISTS "+schema+" CASCADE")
			conn.Close(c2)
		}
	})

	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	cfg.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol
	cfg.RuntimeParams["search_path"] = schema + ",public"
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	sql, err := os.ReadFile("../db/migrations/064_margin_model_runs.up.sql")
	if err != nil {
		t.Fatalf("read migration: %v", err)
	}
	if _, err := conn.Exec(ctx, string(sql)); err != nil {
		conn.Close(ctx)
		t.Fatalf("apply 064: %v", err)
	}
	conn.Close(ctx)

	pcfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("pool cfg: %v", err)
	}
	pcfg.ConnConfig.RuntimeParams["search_path"] = schema + ",public"
	pool, err := pgxpool.NewWithConfig(ctx, pcfg)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)

	rs, err := NewPgModelRunStore(pool)
	if err != nil {
		t.Fatalf("run store: %v", err)
	}
	cs, err := NewPgParamChangeStore(pool)
	if err != nil {
		t.Fatalf("change store: %v", err)
	}
	return rs, cs, pool
}

func TestPgModelRunStoreRoundTrip(t *testing.T) {
	rs, _, pool := modelRunFixture(t)
	ctx := context.Background()

	var reviewer int64 = 42
	param := "auction_floor_decay"
	id, err := rs.InsertRun(ctx, ModelRun{
		Kind: RunKindStress, Scenario: "RATE_SHOCK_-3SIGMA", Status: RunStatusFail,
		ResultMetrics: []byte(`{"fund_drawdown_usd":"123.5"}`),
		BreachCount:   7, ParamChange: &param, ReviewedBy: &reviewer,
	})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	if id <= 0 {
		t.Fatalf("run id %d", id)
	}
	r, err := rs.RunByID(ctx, id)
	if err != nil || r == nil {
		t.Fatalf("by id: %v %+v", err, r)
	}
	if r.Kind != RunKindStress || r.Status != RunStatusFail || r.BreachCount != 7 ||
		r.ParamChange == nil || *r.ParamChange != param ||
		r.ReviewedBy == nil || *r.ReviewedBy != 42 {
		t.Fatalf("round-trip: %+v", r)
	}
	var metrics string
	if err := pool.QueryRow(ctx,
		`SELECT result_metrics::text FROM margin_model_runs WHERE run_id=$1`, id).
		Scan(&metrics); err != nil || !strings.Contains(metrics, "123.5") {
		t.Fatalf("metrics jsonb: %q %v", metrics, err)
	}

	// LatestRun of each kind.
	back, err := rs.InsertRun(ctx, ModelRun{Kind: RunKindBacktest, Scenario: "DAILY_SLIPPAGE",
		Status: RunStatusPass, BreachCount: 1})
	if err != nil {
		t.Fatal(err)
	}
	latest, err := rs.LatestRun(ctx, RunKindStress)
	if err != nil || latest == nil || latest.RunID != id {
		t.Fatalf("latest stress: %v %+v", err, latest)
	}
	latest, err = rs.LatestRun(ctx, RunKindBacktest)
	if err != nil || latest == nil || latest.RunID != back {
		t.Fatalf("latest backtest: %v %+v", err, latest)
	}

	// SumBreaches over the window — bound behind wall clock so rows
	// stamped by the DB's now() always fall inside.
	since := time.Now().UTC().Add(-24 * time.Hour)
	n, err := rs.SumBreaches(ctx, RunKindStress, since)
	if err != nil || n != 7 {
		t.Fatalf("breach sum: %d %v", n, err)
	}
	n, err = rs.SumBreaches(ctx, RunKindBacktest, since)
	if err != nil || n != 1 {
		t.Fatalf("backtest sum: %d %v", n, err)
	}

	// RecordReview pins one validator; a second reviewer is refused.
	fresh, err := rs.InsertRun(ctx, ModelRun{Kind: RunKindStress, Scenario: "S2",
		Status: RunStatusPass})
	if err != nil {
		t.Fatal(err)
	}
	if err := rs.RecordReview(ctx, fresh, 77); err != nil {
		t.Fatalf("first review: %v", err)
	}
	if err := rs.RecordReview(ctx, fresh, 88); err == nil {
		t.Fatal("second reviewer must be refused")
	}
	// Enum/CHECK failures surface as errors (fail closed).
	if _, err := rs.InsertRun(ctx, ModelRun{Kind: "BOGUS", Scenario: "s", Status: RunStatusPass}); err == nil {
		t.Fatal("invalid kind must error")
	}
}

func TestPgParamChangeStoreRoundTrip(t *testing.T) {
	_, cs, pool := modelRunFixture(t)
	ctx := context.Background()

	id, err := cs.InsertChange(ctx, ParamChange{
		Parameter: "leverage_tier:MAJOR:ESMA:0", OwnerID: 5, ApprovedBy: 9,
		Proposed: []byte(`{"max_leverage":25}`),
	})
	if err != nil {
		t.Fatalf("insert change: %v", err)
	}
	var status string
	if err := pool.QueryRow(ctx,
		`SELECT status FROM margin_model_param_changes WHERE id=$1`, id).
		Scan(&status); err != nil || status != ChangeStatusPending {
		t.Fatalf("pending row: %q %v", status, err)
	}
	if err := cs.DecideChange(ctx, id, ChangeStatusValidated, ""); err != nil {
		t.Fatalf("decide: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`SELECT status FROM margin_model_param_changes WHERE id=$1`, id).
		Scan(&status); err != nil || status != ChangeStatusValidated {
		t.Fatalf("validated row: %q %v", status, err)
	}
	// The dual-control CHECK rejects owner == approver at the DB layer too.
	if _, err := cs.InsertChange(ctx, ParamChange{
		Parameter: "x", OwnerID: 5, ApprovedBy: 5}); err == nil {
		t.Fatal("self-approving change must hit the CHECK")
	}
}
