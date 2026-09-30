// Phase-21 wave-2 surveillance/compliance — postgres-gated
// integration tests for market-abuse enforcement (21.3.8), RTS 6
// algo/DEA controls (21.3.12), surveillance case management
// (21.3.21), employee dealing (21.3.24) and tuning/audit-trail
// (21.3.27).
//
// Gated: EXC_PG_TEST=1 (holdPool/holdSeed helpers, dev DSN or
// EXC_PG_DSN). Requires migrations 079 + 241 + 242 + 247 + 248.
package compliance

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"exchange/internal/marketdata"
)

// seedSignal inserts an OPEN surveillance signal carrying the L3
// account pseudonym — the same row shape the Phase-17 engine writes.
func seedSignal(t *testing.T, pool *pgxpool.Pool, aid int64,
	signalType, evidence string) int64 {
	t.Helper()
	var id int64
	err := pool.QueryRow(context.Background(), `
		INSERT INTO surveillance_signals
		    (signal_type, symbol, account_hash, first_l3_seq, last_l3_seq,
		     window_start, window_end, evidence, dedup_key)
		VALUES ($1,'EURUSD',$2,1,10,now(),now(),$3::jsonb,$4)
		RETURNING id`,
		signalType, int64(marketdata.L3AccountHash(uint64(aid))),
		evidence, fmt.Sprintf("it-w2-%d-%d", aid, time.Now().UnixNano())).
		Scan(&id)
	if err != nil {
		t.Fatalf("seed signal: %v", err)
	}
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = pool.Exec(c,
			`DELETE FROM enforcement_actions WHERE signal_id=$1`, id)
		_, _ = pool.Exec(c,
			`DELETE FROM surveillance_case_evidence WHERE case_id IN
			    (SELECT id FROM surveillance_cases WHERE signal_id=$1)`, id)
		_, _ = pool.Exec(c,
			`DELETE FROM surveillance_cases WHERE signal_id=$1`, id)
		_, _ = pool.Exec(c,
			`DELETE FROM surveillance_signals WHERE id=$1`, id)
	})
	return id
}

// ---- Task 21.3.8 — enforcement ----

// WARN resolves account_hash → accounts.id through the L3 resolver,
// persists the ledger row + audit, and marks the signal CASED. The
// UNIQUE(signal_id, action) index makes the duplicate POST a
// CONFLICT rather than a second freeze.
func TestPGEnforcementWarnDismiss(t *testing.T) {
	pool := holdPool(t)
	uid, aid := holdSeed(t, pool)
	amlCleanup(t, pool, aid)
	ctx := context.Background()

	sigID := seedSignal(t, pool, aid, "MOMENTUM_IGNITION",
		`{"z_score":3.4}`)
	svc := NewEnforcementService(pool, nil, nil, nil,
		ScanAccountsResolver(pool), HoldRoleResolver(officerRole), nil)

	act, err := svc.Enforce(ctx, EnforcementRequest{
		SignalID: sigID, Action: EnfActionWarn, Source: "MANUAL",
		ActorID: uid, Note: "it: officer warning",
	})
	if err != nil {
		t.Fatalf("warn: %v", err)
	}
	if act.AccountID == nil || *act.AccountID != aid {
		t.Fatalf("account hash did not resolve: %+v", act.AccountID)
	}
	if act.Action != EnfActionWarn || act.Status != "ACTIVE" {
		t.Fatalf("action row wrong: %+v", act)
	}
	var sigStatus string
	if err := pool.QueryRow(ctx,
		`SELECT status FROM surveillance_signals WHERE id=$1`,
		sigID).Scan(&sigStatus); err != nil {
		t.Fatalf("signal read: %v", err)
	}
	if sigStatus != "CASED" {
		t.Fatalf("signal status %q want CASED", sigStatus)
	}

	// Dedup — same (signal, action) again → CONFLICT.
	if _, err := svc.Enforce(ctx, EnforcementRequest{
		SignalID: sigID, Action: EnfActionWarn, Source: "MANUAL",
		ActorID: uid}); err == nil {
		t.Fatal("duplicate WARN must conflict")
	}

	// DISMISS on a fresh signal closes it without an account gate.
	sig2 := seedSignal(t, pool, aid, "LAYERING", `{}`)
	act2, err := svc.Enforce(ctx, EnforcementRequest{
		SignalID: sig2, Action: EnfActionDismiss, Source: "MANUAL",
		ActorID: uid, Note: "benign pattern",
	})
	if err != nil {
		t.Fatalf("dismiss: %v", err)
	}
	if act2.Action != EnfActionDismiss {
		t.Fatalf("action %q want DISMISS", act2.Action)
	}
	if err := pool.QueryRow(ctx,
		`SELECT status FROM surveillance_signals WHERE id=$1`,
		sig2).Scan(&sigStatus); err != nil {
		t.Fatalf("signal2 read: %v", err)
	}
	if sigStatus != "DISMISSED" {
		t.Fatalf("signal2 status %q want DISMISSED", sigStatus)
	}

	// The ledger read exposes both rows.
	rows, err := svc.List(ctx, aid, 50)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) < 2 {
		t.Fatalf("list rows=%d want ≥2", len(rows))
	}
}

// Role gate: auditors cannot take enforcement actions; RESTRICT fails
// closed when the kill-switch seam is unwired (record persists, the
// caller sees SERVICE_DEGRADED).
func TestPGEnforcementGuards(t *testing.T) {
	pool := holdPool(t)
	uid, aid := holdSeed(t, pool)
	amlCleanup(t, pool, aid)
	ctx := context.Background()

	sigID := seedSignal(t, pool, aid, "SPOOFING", `{}`)
	svcAuditor := NewEnforcementService(pool, nil, nil, nil,
		ScanAccountsResolver(pool), HoldRoleResolver(auditorRole), nil)
	if _, err := svcAuditor.Enforce(ctx, EnforcementRequest{
		SignalID: sigID, Action: EnfActionWarn, Source: "MANUAL",
		ActorID: uid}); err == nil {
		t.Fatal("auditor must not enforce")
	}

	svcNoSeams := NewEnforcementService(pool, nil, nil, nil,
		ScanAccountsResolver(pool), HoldRoleResolver(officerRole), nil)
	if _, err := svcNoSeams.Enforce(ctx, EnforcementRequest{
		SignalID: sigID, Action: EnfActionRestrict, Source: "MANUAL",
		ActorID: uid}); err == nil {
		t.Fatal("unwired suspender must fail closed")
	}
	// The action row still landed (persist-before-side-effect) so the
	// officer dashboard shows the degraded attempt.
	rows, err := svcNoSeams.List(ctx, aid, 50)
	if err != nil || len(rows) == 0 {
		t.Fatalf("degraded restrict must still ledger: %v rows=%d",
			err, len(rows))
	}
}

// ---- Task 21.3.21 — case management ----

// Signal → case (URGENT on z≥3σ), dedup on source_ref, assign,
// evidence attachment flips INVESTIGATING, FALSE_POSITIVE disposition
// is terminal.
func TestPGCaseLifecycle(t *testing.T) {
	pool := holdPool(t)
	uid, aid := holdSeed(t, pool)
	amlCleanup(t, pool, aid)
	ctx := context.Background()

	sigID := seedSignal(t, pool, aid, "SPOOFING", `{"z_score":4.2}`)
	svc := NewCaseService(pool, HoldRoleResolver(officerRole),
		ScanAccountsResolver(pool), nil, nil, nil)

	c, created, err := svc.OpenFromSignal(ctx, sigID)
	if err != nil || !created {
		t.Fatalf("open: created=%v err=%v", created, err)
	}
	if c.Severity != CaseSeverityUrgent {
		t.Fatalf("severity %q want URGENT (z=4.2)", c.Severity)
	}
	if c.AccountID == nil || *c.AccountID != aid {
		t.Fatalf("account not resolved on case: %+v", c.AccountID)
	}
	if _, created, err := svc.OpenFromSignal(ctx, sigID); err != nil ||
		created {
		t.Fatal("source_ref dedup must return the same case")
	}

	c, err = svc.Assign(ctx, uid, c.ID, uid)
	if err != nil {
		t.Fatalf("assign: %v", err)
	}
	if c.Status != CaseStatusAssigned || c.AssignedTo == nil ||
		*c.AssignedTo != uid {
		t.Fatalf("assign state: %+v", c)
	}

	ev, err := svc.AttachEvidence(ctx, uid, c.ID, CaseEvNote,
		"reviewed L3 replay — matched-quote ratio 0.91", "", "")
	if err != nil {
		t.Fatalf("evidence: %v", err)
	}
	if ev.ID <= 0 {
		t.Fatalf("evidence id=%d", ev.ID)
	}
	ws, err := svc.Workspace(ctx, c.ID)
	if err != nil {
		t.Fatalf("workspace: %v", err)
	}
	if ws.Case.Status != CaseStatusInvestigating ||
		len(ws.Evidence) != 1 {
		t.Fatalf("workspace: status=%s evidence=%d",
			ws.Case.Status, len(ws.Evidence))
	}

	closed, err := svc.Disposition(ctx, uid, c.ID, DispositionRequest{
		Disposition: "FALSE_POSITIVE",
		Reason:      "pattern traced to benchmark rebalance flow",
	})
	if err != nil {
		t.Fatalf("disposition: %v", err)
	}
	if closed.Status != CaseStatusClosedFP || closed.ClosedAt == nil {
		t.Fatalf("closed state: %+v", closed)
	}
	// Terminal — a second disposition refuses.
	if _, err := svc.Disposition(ctx, uid, c.ID, DispositionRequest{
		Disposition: "FALSE_POSITIVE", Reason: "again"}); err == nil {
		t.Fatal("closed case must refuse re-disposition")
	}

	m, err := svc.MonthlySummary(ctx, time.Now().UTC().Format("2006-01"))
	if err != nil {
		t.Fatalf("summary: %v", err)
	}
	if m.Opened < 1 || m.Closed < 1 || m.FalsePositives < 1 {
		t.Fatalf("summary counters: %+v", m)
	}
}

// The monitoring.CaseSink seam: EvaluateFlow findings open REVIEW
// cases deduped per rule/account/day.
func TestPGCaseMonitoringSink(t *testing.T) {
	pool := holdPool(t)
	_, aid := holdSeed(t, pool)
	amlCleanup(t, pool, aid)
	ctx := context.Background()

	svc := NewCaseService(pool, HoldRoleResolver(officerRole), nil,
		nil, nil, nil)
	f := MonitoringFinding{
		AccountID: aid, Rule: "structuring_24h", Severity: "P2",
		Summary:    "it: sub-threshold clustering",
		Evidence:   map[string]any{"count": 4},
		DetectedAt: time.Now().UTC(),
	}
	id1, err := svc.OpenCase(ctx, f)
	if err != nil {
		t.Fatalf("opencase: %v", err)
	}
	defer func() {
		c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = pool.Exec(c,
			`DELETE FROM surveillance_cases WHERE id=$1`, id1)
	}()
	id2, err := svc.OpenCase(ctx, f)
	if err != nil || id2 != id1 {
		t.Fatalf("monitoring dedup: id1=%d id2=%d err=%v", id1, id2, err)
	}
	var sev string
	if err := pool.QueryRow(ctx,
		`SELECT severity FROM surveillance_cases WHERE id=$1`,
		id1).Scan(&sev); err != nil {
		t.Fatalf("case read: %v", err)
	}
	if sev != CaseSeverityReview {
		t.Fatalf("monitoring case severity %q want REVIEW", sev)
	}
}

// ---- Task 21.3.12 — RTS 6 ----

// Certify lands CERTIFIED when Art. 9 evidence is complete;
// AssertCertified is the order-admission gate — unknown algo or
// expired/suspended cert ⇒ ALGO_NOT_CERTIFIED.
func TestPGRTS6CertGate(t *testing.T) {
	pool := holdPool(t)
	uid, aid := holdSeed(t, pool)
	amlCleanup(t, pool, aid)
	ctx := context.Background()

	svc := NewRTS6Service(pool, HoldRoleResolver(officerRole), nil)
	algoID := fmt.Sprintf("algo-it-%d", time.Now().UnixNano())
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = pool.Exec(c,
			`DELETE FROM algo_certifications WHERE algo_id=$1`, algoID)
		_, _ = pool.Exec(c,
			`DELETE FROM dea_session_controls WHERE account_id=$1`, aid)
		_, _ = pool.Exec(c,
			`DELETE FROM rts6_self_assessments WHERE period_year >= 2090`)
	})

	// No kill-button test → cannot certify (Art. 9).
	if _, err := svc.Certify(ctx, CertifyRequest{
		AlgoID: algoID, AccountID: aid,
		TestEvidenceRef:   "fix-cert-run-1",
		CapacityAssessRef: "cap-2026-q1",
		ActorID:           uid,
	}); err == nil {
		t.Fatal("certify without kill-button test must fail")
	}

	cert, err := svc.Certify(ctx, CertifyRequest{
		AlgoID: algoID, AccountID: aid,
		TestEvidenceRef:   "fix-cert-run-1",
		KillButtonTested:  true,
		CapacityAssessRef: "cap-2026-q1",
		ActorID:           uid,
	})
	if err != nil {
		t.Fatalf("certify: %v", err)
	}
	if cert.Status != "CERTIFIED" || cert.ExpiresAt == nil {
		t.Fatalf("cert state: %+v", cert)
	}
	if err := svc.AssertCertified(ctx, aid, algoID); err != nil {
		t.Fatalf("certified algo must admit: %v", err)
	}
	if err := svc.AssertCertified(ctx, aid, "unknown-algo"); err == nil {
		t.Fatal("unknown algo must fail closed")
	}

	// SUSPEND blocks admission; CERTIFIED transition restores it.
	if _, err := svc.Transition(ctx, uid, cert.ID, "SUSPENDED",
		"investigation"); err != nil {
		t.Fatalf("suspend: %v", err)
	}
	if err := svc.AssertCertified(ctx, aid, algoID); err == nil {
		t.Fatal("suspended algo must not admit")
	}
	if _, err := svc.Transition(ctx, uid, cert.ID, "CERTIFIED",
		"cleared"); err != nil {
		t.Fatalf("reinstate: %v", err)
	}
	if err := svc.AssertCertified(ctx, aid, algoID); err != nil {
		t.Fatalf("reinstated algo must admit: %v", err)
	}
}

// DEA controls upsert per session and suspend flips the session out
// of admission.
func TestPGRTS6DEA(t *testing.T) {
	pool := holdPool(t)
	uid, aid := holdSeed(t, pool)
	amlCleanup(t, pool, aid)
	ctx := context.Background()

	svc := NewRTS6Service(pool, HoldRoleResolver(officerRole), nil)
	sess := fmt.Sprintf("dea-it-%d", time.Now().UnixNano())
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = pool.Exec(c,
			`DELETE FROM dea_session_controls WHERE session_id=$1`, sess)
	})

	d, err := svc.SetDEALimits(ctx, DEAControls{
		SessionID: sess, AccountID: aid, MaxOrderQty: "50000",
		MaxMsgsPerSec: 20, SponsoringDesk: "DESK-LD4",
		DropCopyFeed: "drop-copy-1", CreatedBy: uid,
	})
	if err != nil {
		t.Fatalf("set dea: %v", err)
	}
	if d.Status != "ACTIVE" || d.MaxMsgsPerSec != 20 {
		t.Fatalf("dea row: %+v", d)
	}
	got, ok, err := svc.DEALimitsFor(ctx, sess)
	if err != nil || !ok || got.AccountID != aid {
		t.Fatalf("dea read: %+v ok=%v err=%v", got, ok, err)
	}
	if err := svc.SuspendDEA(ctx, uid, sess, "fat-finger spike"); err != nil {
		t.Fatalf("dea suspend: %v", err)
	}
	got, ok, _ = svc.DEALimitsFor(ctx, sess)
	if !ok || got.Status != "SUSPENDED" {
		t.Fatalf("dea status after suspend: %+v", got)
	}
}

// ---- Task 21.3.24 — employee dealing ----

// A sensitive-role employee is gated until an APPROVED pre-clearance
// covers the instrument; the decider must differ from the requester
// (separation of duties).
func TestPGEmployeeDealingGate(t *testing.T) {
	pool := holdPool(t)
	uid, aid := holdSeed(t, pool)
	amlCleanup(t, pool, aid)
	ctx := context.Background()

	var officerID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO users (email, status)
		 VALUES ($1,'ACTIVE') RETURNING id`,
		fmt.Sprintf("dealing_it_%d@example.com",
			time.Now().UnixNano())).Scan(&officerID); err != nil {
		t.Fatalf("seed officer: %v", err)
	}
	defer func() {
		c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = pool.Exec(c, `DELETE FROM users WHERE id=$1`, officerID)
	}()

	// Flag the seeded account as staff.
	if _, err := pool.Exec(ctx,
		`UPDATE accounts SET employee_account=true,
		 employee_role='COMPLIANCE' WHERE id=$1`, aid); err != nil {
		t.Fatalf("flag employee: %v", err)
	}

	svc := NewEmployeeDealingService(pool,
		HoldRoleResolver(officerRole), nil)
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = pool.Exec(c,
			`DELETE FROM pre_clearance_requests WHERE account_id=$1`, aid)
		_, _ = pool.Exec(c,
			`DELETE FROM employee_trade_reviews WHERE account_id=$1`, aid)
	})

	// Sensitive role → clearance required on ANY instrument.
	if err := svc.AssertOrderEntry(ctx, aid, "EURUSD"); err == nil {
		t.Fatal("sensitive-role staff must be gated")
	}

	// Self-decide refused (dual control).
	p, err := svc.RequestClearance(ctx, aid, uid, "EURUSD", "BUY",
		"it: personal portfolio rebalancing", nil,
		time.Now().UTC().Add(2*time.Hour))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if _, err := svc.DecideClearance(ctx, uid, p.ID, true,
		"self-decide attempt"); err == nil {
		t.Fatal("requester must not decide their own clearance")
	}

	// Independent officer approves → admission opens.
	dec, err := svc.DecideClearance(ctx, officerID, p.ID, true,
		"no MNPI exposure confirmed")
	if err != nil {
		t.Fatalf("decide: %v", err)
	}
	if dec.Outcome != "APPROVED" {
		t.Fatalf("outcome %q want APPROVED", dec.Outcome)
	}
	if err := svc.AssertOrderEntry(ctx, aid, "EURUSD"); err != nil {
		t.Fatalf("cleared employee must admit: %v", err)
	}
	// Uncovered instrument still gated (clearance scoped EURUSD).
	if err := svc.AssertOrderEntry(ctx, aid, "USDJPY"); err == nil {
		t.Fatal("uncovered instrument must stay gated")
	}

	// A non-employee account is never gated.
	uid2, aid2 := holdSeed(t, pool)
	amlCleanup(t, pool, aid2)
	if err := svc.AssertOrderEntry(ctx, aid2, "EURUSD"); err != nil {
		t.Fatalf("non-employee must admit: %v", err)
	}
	_ = uid2
}

// Restricted windows gate even non-sensitive employees.
func TestPGRestrictedWindowGate(t *testing.T) {
	pool := holdPool(t)
	_, aid := holdSeed(t, pool)
	amlCleanup(t, pool, aid)
	ctx := context.Background()

	// Employee, non-sensitive role → only restricted windows gate.
	if _, err := pool.Exec(ctx,
		`UPDATE accounts SET employee_account=true,
		 employee_role='FINANCE' WHERE id=$1`, aid); err != nil {
		t.Fatalf("flag employee: %v", err)
	}
	now := time.Now().UTC()
	var listID int64
	if err := pool.QueryRow(ctx, `
		INSERT INTO restricted_lists
		    (event_id, event_type, instruments, window_start, window_end,
		     widen_minutes, scope, reason, created_by)
		VALUES ($1,'RATE_FIX','{EURUSD}',$2,$3,0,'ALL_EMPLOYEES','it',1)
		RETURNING id`,
		fmt.Sprintf("it-%d", now.UnixNano()), now.Add(-time.Hour),
		now.Add(time.Hour)).Scan(&listID); err != nil {
		t.Fatalf("seed restricted list: %v", err)
	}
	defer func() {
		c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = pool.Exec(c, `DELETE FROM restricted_lists WHERE id=$1`,
			listID)
		_, _ = pool.Exec(c,
			`DELETE FROM pre_clearance_requests WHERE account_id=$1`, aid)
	}()

	svc := NewEmployeeDealingService(pool,
		HoldRoleResolver(officerRole), nil)
	if err := svc.AssertOrderEntry(ctx, aid, "EURUSD"); err == nil {
		t.Fatal("blackout window must gate employee orders")
	}
	if err := svc.AssertOrderEntry(ctx, aid, "USDJPY"); err != nil {
		t.Fatalf("non-covered instrument must admit: %v", err)
	}
}

// ---- Task 21.3.27 — tuning, reporting values, audit trail ----

// Propose → Activate flips versions atomically; ActiveTuning always
// resolves the live row.
func TestPGTuningLifecycle(t *testing.T) {
	pool := holdPool(t)
	uid, _ := holdSeed(t, pool)
	ctx := context.Background()

	svc := NewTuningService(pool, HoldRoleResolver(officerRole))
	sig := fmt.Sprintf("IT_SIG_%d", time.Now().UnixNano())
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = pool.Exec(c,
			`DELETE FROM surveillance_signal_tuning WHERE signal_type=$1`,
			sig)
	})

	v1, err := svc.Propose(ctx, uid, sig,
		json.RawMessage(`{"z_threshold":3.5}`), 0)
	if err != nil {
		t.Fatalf("propose v1: %v", err)
	}
	if v1.Version != 1 || v1.Status != "DRAFT" {
		t.Fatalf("v1: %+v", v1)
	}
	v2, err := svc.Propose(ctx, uid, sig,
		json.RawMessage(`{"z_threshold":4.0}`), 12.5)
	if err != nil {
		t.Fatalf("propose v2: %v", err)
	}
	if v2.Version != 2 {
		t.Fatalf("v2 version=%d", v2.Version)
	}
	if _, err := svc.Activate(ctx, uid, sig, 1); err != nil {
		t.Fatalf("activate v1: %v", err)
	}
	if _, err := svc.Activate(ctx, uid, sig, 2); err != nil {
		t.Fatalf("activate v2: %v", err)
	}
	active, ok, err := svc.ActiveTuning(ctx, sig)
	if err != nil || !ok || active.Version != 2 {
		t.Fatalf("active tuning: %+v ok=%v err=%v", active, ok, err)
	}
	list, err := svc.List(ctx, sig, 10)
	if err != nil || len(list) != 2 {
		t.Fatalf("list: %d err=%v", len(list), err)
	}
	var states []string
	for _, r := range list {
		states = append(states, r.Status)
	}
	if states[0] != "ACTIVE" || states[1] != "SUPERSEDED" {
		t.Fatalf("version states: %v", states)
	}

	// Backtest over the seeded signal window computes an FP rate.
	bt, err := svc.Backtest(ctx, sig,
		time.Now().UTC().Add(-24*time.Hour), time.Now().UTC())
	if err != nil {
		t.Fatalf("backtest: %v", err)
	}
	if bt.SignalType != sig {
		t.Fatalf("backtest signal %q", bt.SignalType)
	}
}

// ReportingValues: upsert + Get round-trip; VENUE_LEI values are
// checksum-validated before landing.
func TestPGReportingValues(t *testing.T) {
	pool := holdPool(t)
	uid, _ := holdSeed(t, pool)
	ctx := context.Background()

	svc := NewReportingValues(pool, HoldRoleResolver(officerRole))
	key := fmt.Sprintf("it.%d", time.Now().UnixNano())
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = pool.Exec(c,
			`DELETE FROM reporting_values WHERE key=$1`, key)
	})

	if _, err := svc.Set(ctx, uid, "REPORT_ENDPOINT", key,
		json.RawMessage(`{"url":"https://arm.example/ingest"}`),
		"it"); err != nil {
		t.Fatalf("set: %v", err)
	}
	got, ok, err := svc.Get(ctx, "REPORT_ENDPOINT", key)
	if err != nil || !ok {
		t.Fatalf("get: ok=%v err=%v", ok, err)
	}
	var v map[string]string
	if err := json.Unmarshal(got, &v); err != nil ||
		v["url"] == "" {
		t.Fatalf("value round-trip: %s", got)
	}

	// LEI scope enforces ISO 17442.
	if _, err := svc.Set(ctx, uid, "VENUE_LEI", key,
		json.RawMessage(`"NOT-A-LEI-0000000000"`), "it"); err == nil {
		t.Fatal("invalid LEI must reject")
	}
}

// AuditTrailQuery joins admin_audit_log to the hash chain — the WARN
// enforcement above wrote both sides in one tx.
func TestPGAuditTrailJoin(t *testing.T) {
	pool := holdPool(t)
	uid, aid := holdSeed(t, pool)
	amlCleanup(t, pool, aid)
	ctx := context.Background()

	sigID := seedSignal(t, pool, aid, "LAYERING", `{}`)
	svc := NewEnforcementService(pool, nil, nil, nil,
		ScanAccountsResolver(pool), HoldRoleResolver(officerRole), nil)
	if _, err := svc.Enforce(ctx, EnforcementRequest{
		SignalID: sigID, Action: EnfActionWarn, Source: "MANUAL",
		ActorID: uid, Note: "it: audit trail probe",
	}); err != nil {
		t.Fatalf("warn: %v", err)
	}

	q := NewAuditTrailQuery(pool)
	rows, err := q.Search(ctx, AuditTrailFilter{
		Action: "enforcement.WARN", Limit: 20,
	})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	found := false
	for _, r := range rows {
		if r.TargetID != nil && *r.TargetID == sigID {
			found = true
			if r.ChainSeq == nil {
				t.Fatal("joined row must carry chain_seq anchor")
			}
		}
	}
	if !found {
		t.Fatalf("audit trail missing the enforcement row (rows=%d)",
			len(rows))
	}

	chain, err := q.ChainSlice(ctx, "admin_audit_log", "", time.Time{},
		time.Time{}, 0, 10)
	if err != nil {
		t.Fatalf("chain slice: %v", err)
	}
	if len(chain) == 0 {
		t.Fatal("audit_hash_chain must carry admin rows")
	}

	// Masked export scrubs PII leaves.
	masked, err := q.Search(ctx, AuditTrailFilter{
		Action: "enforcement.WARN", Limit: 5, Mask: true,
	})
	if err != nil {
		t.Fatalf("masked search: %v", err)
	}
	for _, r := range masked {
		if len(r.AfterState) > 0 &&
			string(r.AfterState) != "{}" &&
			containsPIIKey(r.AfterState) {
			t.Fatalf("mask left PII: %s", r.AfterState)
		}
	}
}

func containsPIIKey(raw json.RawMessage) bool {
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return false
	}
	return hasPII(v)
}

func hasPII(v any) bool {
	switch t := v.(type) {
	case map[string]any:
		for k, val := range t {
			if piiKey.MatchString(k) {
				if s, ok := val.(string); !ok || s != "***" {
					return true
				}
			}
			if hasPII(val) {
				return true
			}
		}
	case []any:
		for _, e := range t {
			if hasPII(e) {
				return true
			}
		}
	}
	return false
}
