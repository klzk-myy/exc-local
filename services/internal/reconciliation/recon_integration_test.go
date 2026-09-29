package reconciliation

// recon_integration_test.go — Task 13.3.2 full-cycle gate: seed a real
// divergence, execute the sweep against PostgreSQL, and prove finding →
// P1 alert → halt emission end to end.
//
// Gated: EXC_PG_TEST=1, DSN via EXC_PG_DSN / EXC_TEST_DSN (defaults to
// the repo's dev database which carries migration 207).

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"exchange/internal/settlement"
	"exchange/pkg/decimal"
)

func reconDSN() string {
	for _, k := range []string{"EXC_PG_DSN", "EXC_TEST_DSN"} {
		if d := os.Getenv(k); d != "" {
			return d
		}
	}
	return "postgres://exchange:exchange_dev@localhost:5433/exchange?sslmode=disable"
}

type capturingFlags struct{ sets []fakeHaltCall }

func (f *capturingFlags) SetHaltScope(_ context.Context, scope, target, reason string) error {
	f.sets = append(f.sets, fakeHaltCall{scope, target, reason})
	return nil
}
func (f *capturingFlags) ClearHaltScope(context.Context, string, string) error {
	return nil
}

// TestReconciliationFullCycle proves the task's dispatch contract:
//
//	seeded positions divergence → RunOnce → MISMATCH run persisted →
//	finding row with expected/actual/delta → P1 alert (durable row +
//	paged) → scoped ACCOUNT suspension + halt flag.
func TestReconciliationFullCycle(t *testing.T) {
	if os.Getenv("EXC_PG_TEST") != "1" {
		t.Skip("set EXC_PG_TEST=1 to run PostgreSQL integration tests")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, reconDSN())
	if err != nil {
		t.Skipf("postgres unreachable: %v", err)
	}
	defer pool.Close()
	if err := pool.Ping(ctx); err != nil {
		t.Skipf("postgres unreachable: %v", err)
	}

	// --- seed: a position row with no fill-ledger backing ---------------
	// positions carries no FKs (migration 014) — synthetic ids are safe.
	const acct, instr = 99999001, 99999001
	var posID int64
	if err := pool.QueryRow(ctx, `
		INSERT INTO positions (account_id, instrument_id, side, quantity,
			entry_price, realized_pnl, unrealized_pnl)
		VALUES ($1, $2, 'LONG', 5.00000000, 1.00000000, 0, 0)
		RETURNING id`, acct, instr).Scan(&posID); err != nil {
		t.Fatalf("seed position: %v", err)
	}
	defer func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM positions WHERE id = $1`, posID)
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM trading_suspensions
			 WHERE scope='ACCOUNT' AND target_id=$1`, "99999001")
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM funding_ops_alerts WHERE code=$1`, AlertMismatchCode)
	}()

	// --- engine over the production seams (flags captured in-process) ---
	flags := &capturingFlags{}
	capture := &fakeAlerter{}
	store := NewPgStore(pool)
	engine, err := NewEngine(Deps{
		Pool:    pool,
		Store:   store,
		Alerter: NewDurableOpsAlerter(pool, capture),
		Halter:  NewPgHalter(pool, flags),
		Logf:    func(string, ...any) {},
	}, DefaultCheckers(NewPgLegs(pool), nil, nil))
	if err != nil {
		t.Fatalf("engine: %v", err)
	}

	run, err := engine.RunOnce(ctx)
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if run.Status != RunMismatch {
		t.Fatalf("run status %s (want MISMATCH); findings=%d", run.Status, run.FindingsCount)
	}

	// --- finding persisted ----------------------------------------------
	findings, err := store.RunFindings(ctx, run.ID)
	if err != nil {
		t.Fatalf("findings: %v", err)
	}
	var posFinding *FindingRow
	for i := range findings {
		if findings[i].Category == CatPositions &&
			findings[i].Subject == "position:99999001:99999001" {
			posFinding = &findings[i]
		}
	}
	if posFinding == nil {
		t.Fatalf("seeded position finding not persisted: %+v", findings)
	}
	if posFinding.Severity != SevMismatch {
		t.Fatalf("finding severity %s", posFinding.Severity)
	}
	if posFinding.Expected == nil || posFinding.Actual == nil || posFinding.Delta == nil {
		t.Fatalf("finding must carry the 8dp triple: %+v", posFinding)
	}
	five := decimal.NewFromInt(5)
	if !posFinding.Expected.IsZero() || !posFinding.Actual.Equal(five) ||
		!posFinding.Delta.Equal(five) {
		t.Fatalf("triple %s/%s/%s, want 0/5/5",
			posFinding.Expected, posFinding.Actual, posFinding.Delta)
	}
	if posFinding.HaltScope != "ACCOUNT" || posFinding.HaltTarget != "99999001" {
		t.Fatalf("finding halt scope %+v", posFinding)
	}

	// --- alert raised: durable row + page --------------------------------
	var alertCount int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM funding_ops_alerts
		 WHERE code = $1 AND severity = 'P1' AND status = 'OPEN'`,
		AlertMismatchCode).Scan(&alertCount); err != nil {
		t.Fatalf("alert count: %v", err)
	}
	if alertCount == 0 {
		t.Fatal("no durable P1 funding_ops_alerts row")
	}
	if len(capture.alerts) == 0 || capture.alerts[0].Code != AlertMismatchCode ||
		capture.alerts[0].Severity != "P1" {
		t.Fatalf("paged alert %+v", capture.alerts)
	}

	// --- halt emitted: durable suspension + enforcement flag -------------
	// The synthetic seed targets ACCOUNT/99999001, but the engine runs
	// over the whole dev dataset: when >32 distinct scopes diverge the
	// documented escalation collapses per-scope halts into one GLOBAL
	// suspension (rule R4 — a systemic divergence has no surgical
	// boundary). Accept either: the scoped row (isolated data) or a
	// GLOBAL row only when the run genuinely found >32 scopes.
	var suspID int64
	var suspScope, suspTarget string
	err = pool.QueryRow(ctx, `
		SELECT suspension_id, scope::text, target_id FROM trading_suspensions
		 WHERE scope='ACCOUNT' AND target_id='99999001' AND state='ACTIVE'`).
		Scan(&suspID, &suspScope, &suspTarget)
	escalated := false
	if err != nil {
		// Escalation is only legitimate when the mismatch findings
		// themselves force it: >32 distinct scopes or a scopeless
		// divergence (rule R4 in Engine.haltPlan). Recompute the plan
		// inputs from the persisted findings — a GLOBAL row without
		// these conditions would be a regression, not an escalation.
		type key struct{ scope, target string }
		set := map[key]struct{}{}
		scopeless := false
		for i := range findings {
			if findings[i].Severity != SevMismatch {
				continue
			}
			if findings[i].HaltScope == "" {
				scopeless = true
			}
			set[key{findings[i].HaltScope, findings[i].HaltTarget}] = struct{}{}
		}
		if len(set) <= 32 && !scopeless {
			t.Fatalf("suspension row: %v — and escalation unjustified (%d scopes, scopeless=%v)", err, len(set), scopeless)
		}
		escalated = true
		if qerr := pool.QueryRow(ctx, `
			SELECT suspension_id, scope::text, target_id FROM trading_suspensions
			 WHERE scope='GLOBAL' AND state='ACTIVE'
			 ORDER BY suspension_id DESC LIMIT 1`).
			Scan(&suspID, &suspScope, &suspTarget); qerr != nil {
			t.Fatalf("suspension row: ACCOUNT read: %v; GLOBAL fallback: %v", err, qerr)
		}
	}
	var initiatedBy int64
	if err := pool.QueryRow(ctx,
		`SELECT initiated_by FROM trading_suspensions WHERE suspension_id=$1`,
		suspID).Scan(&initiatedBy); err != nil || initiatedBy != SystemActorID {
		t.Fatalf("machine halt must carry initiated_by=0: %d %v", initiatedBy, err)
	}
	if len(flags.sets) == 0 {
		t.Fatal("no halt:* flag raised")
	}
	var found bool
	for _, c := range flags.sets {
		if escalated {
			if c.scope == "GLOBAL" {
				found = true
			}
		} else if c.scope == "ACCOUNT" && c.target == "99999001" {
			found = true
		}
	}
	if !found {
		t.Fatalf("halt flag not raised (escalated=%v): %+v", escalated, flags.sets)
	}

	// --- run row finalized -----------------------------------------------
	var status string
	var halts []byte
	if err := pool.QueryRow(ctx,
		`SELECT status, COALESCE(halts_emitted,'null') FROM reconciliation_runs WHERE id=$1`,
		run.ID).Scan(&status, &halts); err != nil {
		t.Fatalf("run row: %v", err)
	}
	if status != string(RunMismatch) {
		t.Fatalf("persisted status %s", status)
	}
	if string(halts) == "null" {
		t.Fatal("halts_emitted not recorded on run")
	}

	_ = settlement.OpsAlert{} // import pin — alert shape is the shared seam
}
