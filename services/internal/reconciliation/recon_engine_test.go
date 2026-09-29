package reconciliation

// recon_engine_test.go — engine unit tests: classification, alert/halt
// dispatch, escalation policy, scheduler lifecycle. All seams faked.

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"exchange/internal/settlement"
	"exchange/pkg/decimal"
)

// ---- fakes --------------------------------------------------------------

type fakeChecker struct {
	cat      Category
	findings []Finding
	err      error
}

func (f fakeChecker) Name() Category { return f.cat }
func (f fakeChecker) Run(context.Context, Scope) ([]Finding, error) {
	return f.findings, f.err
}

type reconFakeStore struct {
	runs     int
	findings []Finding
	finished []struct {
		status     RunStatus
		cats       int
		mis, incon int
		halts      []HaltRecord
		err        string
	}
}

func (s *reconFakeStore) InsertRun(context.Context, time.Time) (int64, error) {
	s.runs++
	return int64(s.runs), nil
}
func (s *reconFakeStore) InsertFindings(_ context.Context, _ int64, fs []Finding) error {
	s.findings = append(s.findings, fs...)
	return nil
}
func (s *reconFakeStore) FinishRun(_ context.Context, _ int64, st RunStatus,
	cats, mis, incon int, halts []HaltRecord, runErr string) error {
	s.finished = append(s.finished, struct {
		status     RunStatus
		cats       int
		mis, incon int
		halts      []HaltRecord
		err        string
	}{st, cats, mis, incon, halts, runErr})
	return nil
}

type fakeAlerter struct{ alerts []settlement.OpsAlert }

func (a *fakeAlerter) Raise(_ context.Context, al settlement.OpsAlert) error {
	a.alerts = append(a.alerts, al)
	return nil
}

type fakeHaltCall struct{ scope, target, reason string }
type fakeHalter struct {
	calls []fakeHaltCall
	err   error
}

func (h *fakeHalter) Halt(_ context.Context, scope, target, reason string) (int64, error) {
	h.calls = append(h.calls, fakeHaltCall{scope, target, reason})
	if h.err != nil {
		return 0, h.err
	}
	return int64(len(h.calls)), nil
}

func newTestEngine(t *testing.T, checkers []Checker,
	st Store, al Alerter, h Halter) *Engine {
	t.Helper()
	e, err := NewEngine(Deps{Store: st, Alerter: al, Halter: h}, checkers)
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	return e
}

// ---- tests --------------------------------------------------------------

func TestEngineCleanRun(t *testing.T) {
	st := &reconFakeStore{}
	al := &fakeAlerter{}
	h := &fakeHalter{}
	e := newTestEngine(t, []Checker{
		fakeChecker{cat: CatBalances},
		fakeChecker{cat: CatGeneralLedger},
	}, st, al, h)

	run, err := e.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if run.Status != RunClean {
		t.Fatalf("status = %s, want CLEAN", run.Status)
	}
	if len(al.alerts) != 0 {
		t.Fatalf("alerts on clean run: %+v", al.alerts)
	}
	if len(h.calls) != 0 {
		t.Fatalf("halts on clean run: %+v", h.calls)
	}
	if len(st.finished) != 1 || st.finished[0].status != RunClean {
		t.Fatalf("FinishRun not recorded: %+v", st.finished)
	}
}

func TestEngineMismatchAlertsAndHalts(t *testing.T) {
	st := &reconFakeStore{}
	al := &fakeAlerter{}
	h := &fakeHalter{}
	f := AmountFinding(CatBalances, accountSubject(7, "USD"),
		"ledger_vs_balances", decimal.NewFromInt(100), decimal.NewFromInt(90),
		UnitAmount).WithHalt("ACCOUNT", "7")
	e := newTestEngine(t, []Checker{
		fakeChecker{cat: CatBalances, findings: []Finding{f}},
	}, st, al, h)

	run, err := e.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if run.Status != RunMismatch || run.MismatchCount != 1 {
		t.Fatalf("status %s mismatches %d", run.Status, run.MismatchCount)
	}
	if len(al.alerts) != 1 {
		t.Fatalf("alerts %d, want 1", len(al.alerts))
	}
	got := al.alerts[0]
	if got.Severity != "P1" || got.Code != AlertMismatchCode {
		t.Fatalf("alert %+v", got)
	}
	if len(h.calls) != 1 || h.calls[0].scope != "ACCOUNT" || h.calls[0].target != "7" {
		t.Fatalf("halts %+v", h.calls)
	}
	if len(run.Halts) != 1 || run.Halts[0].SuspensionID == 0 {
		t.Fatalf("run halts %+v", run.Halts)
	}
	// The finding persisted BEFORE the halt dispatch recorded anything.
	if len(st.findings) != 1 || st.findings[0].HaltScope != "ACCOUNT" {
		t.Fatalf("persisted findings %+v", st.findings)
	}
}

func TestEngineInconclusiveNeverHalts(t *testing.T) {
	st := &reconFakeStore{}
	al := &fakeAlerter{}
	h := &fakeHalter{}
	inc := InconclusiveFinding(CatFunding, string(CatFunding),
		"bank_statements", "no statement source")
	e := newTestEngine(t, []Checker{
		fakeChecker{cat: CatFunding, findings: []Finding{inc}},
	}, st, al, h)

	run, err := e.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if run.Status != RunInconclusive {
		t.Fatalf("status %s", run.Status)
	}
	if len(h.calls) != 0 {
		t.Fatalf("halt on inconclusive-only run: %+v", h.calls)
	}
	if len(al.alerts) != 1 || al.alerts[0].Code != AlertInconclusiveCode ||
		al.alerts[0].Severity != "P2" {
		t.Fatalf("alert %+v", al.alerts)
	}
}

func TestEngineCheckerErrorIsInconclusive(t *testing.T) {
	st := &reconFakeStore{}
	e := newTestEngine(t, []Checker{
		fakeChecker{cat: CatOrders, err: errors.New("wal dir unreadable")},
	}, st, nil, nil)
	run, err := e.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if run.Status != RunInconclusive || run.InconclusiveCount != 1 {
		t.Fatalf("status %s inconclusive %d", run.Status, run.InconclusiveCount)
	}
	if st.findings[0].Leg != "checker_error" ||
		st.findings[0].Severity != SevInconclusive {
		t.Fatalf("finding %+v", st.findings[0])
	}
}

func TestEngineHaltEscalation(t *testing.T) {
	// >32 distinct scopes → single GLOBAL halt.
	var fs []Finding
	for i := 0; i < 40; i++ {
		fs = append(fs, AmountFinding(CatBalances,
			accountSubject(int64(1000+i), "USD"), "leg",
			decimal.NewFromInt(1), decimal.NewFromInt(0), UnitAmount).
			WithHalt("ACCOUNT", fmt.Sprint(1000+i)))
	}
	e := newTestEngine(t,
		[]Checker{fakeChecker{cat: CatBalances, findings: fs}},
		&reconFakeStore{}, nil, nil)
	plan := e.haltPlan(fs)
	if len(plan) != 1 || plan[0] != [2]string{"GLOBAL", ""} {
		t.Fatalf("escalated plan %+v", plan)
	}

	// A scope-less finding escalates the plan to include GLOBAL.
	f2 := AmountFinding(CatSettlement, settlementSubject(1), "leg",
		decimal.NewFromInt(1), decimal.Zero, UnitAmount)
	plan = e.haltPlan([]Finding{f2})
	if len(plan) != 1 || plan[0][0] != "GLOBAL" {
		t.Fatalf("scopeless plan %+v", plan)
	}
}

func TestEngineSchedulerRunsAndStops(t *testing.T) {
	st := &reconFakeStore{}
	e := newTestEngine(t, []Checker{fakeChecker{cat: CatBalances}},
		st, nil, nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { e.RunScheduler(ctx, 30*time.Millisecond); close(done) }()
	time.Sleep(150 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("scheduler did not stop on cancel")
	}
	if st.runs < 2 {
		t.Fatalf("runs = %d, want >=2 (boot + ticks)", st.runs)
	}
}

func TestAmountFindingFixedPoint(t *testing.T) {
	f := AmountFinding(CatFees, "trade:1", "leg",
		decimal.RequireFromString("10.1234567891"),
		decimal.RequireFromString("10.1234567801"), UnitAmount)
	if f.Delta == nil || f.Expected == nil || f.Actual == nil {
		t.Fatal("triple must be populated")
	}
	if f.Delta.String() != "-0.00000001" {
		t.Fatalf("delta %s, want -0.00000001 (8dp)", f.Delta.String())
	}
}
