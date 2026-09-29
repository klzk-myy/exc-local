package reconciliation

// engine.go — Task 13.3.2 sweep orchestration: per-category checkers,
// the jittered hourly scheduler, the mismatch→P1+halt dispatcher, and
// the durable run/findings persistence (migration 207).

import (
	"context"
	"fmt"
	"math/rand/v2"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"exchange/internal/settlement"
)

// AlertSubject is the JetStream subject for reconciliation ops alerts.
const AlertSubject = "ops.alerts.reconciliation"

// AlertMismatchCode / AlertInconclusiveCode are the OpsAlert codes on
// the durable + paged trail (alert codes, not API error codes — the
// RBAC_LIFECYCLE/COMPLAINT_SLA_BREACH precedent).
const (
	AlertMismatchCode     = "RECONCILIATION_MISMATCH"
	AlertInconclusiveCode = "RECONCILIATION_INCONCLUSIVE"
)

// Alerter pages an OpsAlert (settlement.PublisherAlerter satisfies it;
// the durable funding_ops_alerts twin-write lives in the wired adapter).
type Alerter interface {
	Raise(ctx context.Context, a settlement.OpsAlert) error
}

// Halter emits the protective halt for one scope — the production
// adapter writes the trading_suspensions row (initiated_by=0 system
// sentinel) and raises the halt:* Redis flag (ruling R4). suspensionID
// is 0 when the scope was already suspended.
type Halter interface {
	Halt(ctx context.Context, scope, target, reason string) (suspensionID int64, err error)
}

// Store is the durable report surface (reconciliation_runs +
// reconciliation_findings). *PgStore satisfies it; tests use fakes.
type Store interface {
	InsertRun(ctx context.Context, startedAt time.Time) (int64, error)
	InsertFindings(ctx context.Context, runID int64, fs []Finding) error
	FinishRun(ctx context.Context, runID int64, status RunStatus,
		categories, mismatches, inconclusive int, halts []HaltRecord, runErr string) error
}

// Engine runs the nine-category sweep.
type Engine struct {
	checkers       []Checker
	pool           *pgxpool.Pool
	store          Store
	alerter        Alerter
	halter         Halter
	walDirs        []string
	now            func() time.Time
	maxScopedHalts int           // distinct halt scopes before GLOBAL escalation (0 → 32)
	checkerTimeout time.Duration // per-checker deadline (0 → 5m)
	logf           func(format string, args ...any)
}

// Deps wires the engine. Store is required; Alerter/Halter may be nil
// (slog fallback + no-halt — the run still reports MISMATCH; see logf).
type Deps struct {
	Pool           *pgxpool.Pool
	Store          Store
	Alerter        Alerter
	Halter         Halter
	WalDirs        []string
	Now            func() time.Time
	MaxScopedHalts int
	CheckerTimeout time.Duration
	Logf           func(format string, args ...any)
}

// NewEngine builds the engine over the checker set. checkers nil →
// DefaultCheckers over Deps.Pool/WalDirs (the production wiring).
func NewEngine(d Deps, checkers []Checker) (*Engine, error) {
	if d.Store == nil {
		return nil, fmt.Errorf("reconciliation: store is required")
	}
	e := &Engine{
		checkers: checkers, pool: d.Pool, store: d.Store, alerter: d.Alerter,
		halter: d.Halter, walDirs: d.WalDirs, now: d.Now,
		maxScopedHalts: d.MaxScopedHalts, checkerTimeout: d.CheckerTimeout,
		logf: d.Logf,
	}
	if e.now == nil {
		e.now = time.Now
	}
	if e.maxScopedHalts <= 0 {
		e.maxScopedHalts = 32
	}
	if e.checkerTimeout <= 0 {
		e.checkerTimeout = 5 * time.Minute
	}
	if e.logf == nil {
		e.logf = func(string, ...any) {}
	}
	return e, nil
}

// ---------------------------------------------------------------------------
// Sweep
// ---------------------------------------------------------------------------

// RunOnce executes every checker once, persists findings, and — on
// MISMATCH — raises the P1 alert and emits the scoped halts. Returns the
// completed run. A persistence failure at InsertRun aborts (the sweep
// cannot report); mid-run failures degrade to status ERROR.
func (e *Engine) RunOnce(ctx context.Context) (*Run, error) {
	started := e.now().UTC()
	runID, err := e.store.InsertRun(ctx, started)
	if err != nil {
		return nil, fmt.Errorf("reconciliation: open run: %w", err)
	}
	run := &Run{ID: runID, StartedAt: started, Status: RunRunning}

	scope := Scope{Pool: e.pool, WalDirs: e.walDirs, Now: started}

	var findings []Finding
	for _, c := range e.checkers {
		cctx, cancel := context.WithTimeout(ctx, e.checkerTimeout)
		fs, cerr := c.Run(cctx, scope)
		cancel()
		if cerr != nil {
			findings = append(findings, InconclusiveFinding(
				c.Name(), string(c.Name()), "checker_error", cerr.Error()))
			continue
		}
		findings = append(findings, fs...)
	}

	var mismatches, inconclusive int
	for _, f := range findings {
		switch f.Severity {
		case SevMismatch:
			mismatches++
		case SevInconclusive:
			inconclusive++
		}
	}
	run.CategoriesChecked = len(e.checkers)
	run.FindingsCount = len(findings)
	run.MismatchCount = mismatches
	run.InconclusiveCount = inconclusive
	switch {
	case mismatches > 0:
		run.Status = RunMismatch
	case inconclusive > 0:
		run.Status = RunInconclusive
	default:
		run.Status = RunClean
	}

	if err := e.store.InsertFindings(ctx, runID, findings); err != nil {
		run.Status = RunError
		run.Error = "findings persist: " + err.Error()
		_ = e.store.FinishRun(ctx, runID, run.Status,
			run.CategoriesChecked, mismatches, inconclusive, nil, run.Error)
		return run, fmt.Errorf("reconciliation: %s", run.Error)
	}

	// --- dispatch ---------------------------------------------------------
	if mismatches > 0 {
		e.raiseAlert(ctx, run, findings, true)
		run.Halts = e.emitHalts(ctx, runID, findings)
	} else if inconclusive > 0 {
		e.raiseAlert(ctx, run, findings, false)
	}

	finished := e.now().UTC()
	run.FinishedAt = &finished
	if err := e.store.FinishRun(ctx, runID, run.Status,
		run.CategoriesChecked, mismatches, inconclusive, run.Halts, ""); err != nil {
		return run, fmt.Errorf("reconciliation: finish run: %w", err)
	}
	return run, nil
}

// haltPlan dedups the finding scopes: a finding with no scope means the
// divergence has no surgical boundary → GLOBAL; too many distinct scopes
// means the problem is systemic → GLOBAL (ruling R4).
func (e *Engine) haltPlan(findings []Finding) [][2]string {
	type key struct{ scope, target string }
	set := map[key]struct{}{}
	for _, f := range findings {
		if f.Severity != SevMismatch {
			continue
		}
		set[key{f.HaltScope, f.HaltTarget}] = struct{}{}
	}
	if len(set) == 0 || len(set) > e.maxScopedHalts {
		return [][2]string{{"GLOBAL", ""}}
	}
	var global bool
	out := make([][2]string, 0, len(set))
	for k := range set {
		if k.scope == "" || k.scope == "GLOBAL" {
			global = true
			continue
		}
		out = append(out, [2]string{k.scope, k.target})
	}
	if global {
		out = append(out, [2]string{"GLOBAL", ""})
	}
	return out
}

// emitHalts drives the halt plan; failures are recorded on the run and
// logged — a failed flag write never hides the alert already paged.
func (e *Engine) emitHalts(ctx context.Context, runID int64, findings []Finding) []HaltRecord {
	var halts []HaltRecord
	if e.halter == nil {
		e.logf("reconciliation: MISMATCH run %d but no halter wired — halts NOT emitted", runID)
		return []HaltRecord{{Scope: "GLOBAL", Error: "no halter wired"}}
	}
	for _, st := range e.haltPlan(findings) {
		reason := fmt.Sprintf("reconciliation run %d: %s", runID,
			AlertMismatchCode)
		id, err := e.halter.Halt(ctx, st[0], st[1], reason)
		rec := HaltRecord{Scope: st[0], Target: st[1], Reason: reason, SuspensionID: id}
		if err != nil {
			rec.Error = err.Error()
			e.logf("reconciliation: halt %s:%s failed: %v", st[0], st[1], err)
		}
		halts = append(halts, rec)
	}
	return halts
}

// raiseAlert pages the OpsAlert seam. A raise failure is logged, never
// fatal — the durable findings row is the system of record.
func (e *Engine) raiseAlert(ctx context.Context, run *Run, findings []Finding, mismatch bool) {
	code, sev := AlertInconclusiveCode, "P2"
	if mismatch {
		code, sev = AlertMismatchCode, "P1"
	}
	summary := fmt.Sprintf(
		"reconciliation run %d: %d mismatch / %d inconclusive findings across %d categories",
		run.ID, run.MismatchCount, run.InconclusiveCount, run.CategoriesChecked)
	cats := map[string]int{}
	subjects := make([]string, 0, 16)
	for _, f := range findings {
		cats[string(f.Category)]++
		if len(subjects) < 16 && f.Severity == SevMismatch {
			subjects = append(subjects, f.Subject)
		}
	}
	details := map[string]string{
		"run_id":   fmt.Sprint(run.ID),
		"status":   string(run.Status),
		"subjects": fmt.Sprint(subjects),
	}
	for c, n := range cats {
		details["cat_"+c] = fmt.Sprint(n)
	}
	if e.alerter == nil {
		e.logf("reconciliation: %s (%s) — no alerter wired", summary, code)
		return
	}
	if err := e.alerter.Raise(ctx, settlement.OpsAlert{
		Severity: sev, Code: code, Summary: summary, Details: details,
	}); err != nil {
		e.logf("reconciliation: ops alert raise failed: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Scheduler — hourly, ±10%% jitter, graceful per the repo sweeper contract.
// ---------------------------------------------------------------------------

// RunScheduler executes a sweep at boot and then every interval ±10%%.
// interval is injectable (tests pass ~100ms). Shutdown is ctx-driven —
// a running sweep finishes; the ticker never abandons mid-persist.
func (e *Engine) RunScheduler(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = time.Hour
	}
	e.runLogged(ctx)
	for {
		j := time.Duration(rand.Int64N(int64(interval) / 5)) // 0..20%
		d := interval - interval/10 + j                      // ±10% band
		t := time.NewTimer(d)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
			e.runLogged(ctx)
		}
	}
}

func (e *Engine) runLogged(ctx context.Context) {
	run, err := e.RunOnce(ctx)
	if err != nil {
		e.logf("reconciliation: run failed: %v", err)
		return
	}
	if run.Status != RunClean {
		e.logf("reconciliation: run %d status %s (mismatch=%d inconclusive=%d)",
			run.ID, run.Status, run.MismatchCount, run.InconclusiveCount)
	}
}
