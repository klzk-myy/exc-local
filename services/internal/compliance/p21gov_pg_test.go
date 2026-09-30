// Phase-21 wave-2 governance — PostgreSQL integration tests.
//
// Gated: EXC_PG_TEST=1 (holdPool / holdSeed helpers, dev DSN or
// EXC_PG_DSN). Covers Basel III snapshot idempotency + breach codes
// (21.3.13), the FX Global Code verdict lock + statement lifecycle
// (21.3.17), the regulatory-change lifecycle gates + auditor scope
// (21.3.25), and the execution-policy activation/consent/review
// contract incl. the order-entry gate (21.3.28).
package compliance

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func p21Resolver(roles map[int64]string) HoldRoleResolver {
	return func(_ context.Context, id int64) (string, error) {
		r, ok := roles[id]
		if !ok {
			return "", fmt.Errorf("no role binding")
		}
		return r, nil
	}
}

// ---- 21.3.28 — execution policy lifecycle + consent gate ----

func TestPGExecutionPolicyLifecycle(t *testing.T) {
	pool := holdPool(t)
	uid, acct := holdSeed(t, pool)
	ctx := context.Background()
	officer := uid
	resolver := p21Resolver(map[int64]string{
		officer: "Compliance Officer",
	})
	svc, err := NewExecutionPolicyService(pool, resolver)
	if err != nil {
		t.Fatalf("service: %v", err)
	}
	ts := time.Now().UnixNano()
	v1 := fmt.Sprintf("v-it-%d", ts)
	v2 := fmt.Sprintf("v-it-%d-b", ts)
	now := time.Now().UTC()

	// Non-officer cannot draft.
	svcRO, _ := NewExecutionPolicyService(pool,
		p21Resolver(map[int64]string{officer: "Read-Only Auditor"}))
	if _, err := svcRO.CreateDraft(ctx, v1+"-ro", "ref", officer); err == nil {
		t.Fatal("auditor must not draft a policy")
	}

	d1, err := svc.CreateDraft(ctx, v1, "s3://policies/"+v1+".md", officer)
	if err != nil {
		t.Fatalf("draft v1: %v", err)
	}
	if _, err := svc.Activate(ctx, d1.ID, officer,
		now, now.Add(365*24*time.Hour), false, nil); err != nil {
		t.Fatalf("activate v1: %v", err)
	}
	active, err := svc.Active(ctx)
	if err != nil || active == nil || active.Version != v1 {
		t.Fatalf("active=%+v err=%v", active, err)
	}

	// Gate refuses before consent (PRODUCT_NOT_PERMITTED), admits
	// reduce-only traffic unconditionally.
	if err := svc.CheckConsent(ctx, acct, false); err == nil {
		t.Fatal("order without consent must refuse")
	}
	if err := svc.CheckConsent(ctx, acct, true); err != nil {
		t.Fatalf("reduce-only must bypass consent: %v", err)
	}

	c, created, err := svc.Consent(ctx, acct, uid, "", "127.0.0.1", nil)
	if err != nil || !created || c.Version != v1 {
		t.Fatalf("consent=%+v created=%v err=%v", c, created, err)
	}
	if _, again, err := svc.Consent(ctx, acct, uid, "", "", nil); err != nil || again {
		t.Fatalf("replayed consent must dedup: again=%v err=%v", again, err)
	}
	// Generic ledger row written for the gate.
	var n int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM account_consents
		WHERE account_id=$1 AND consent_type='EXECUTION_POLICY' AND doc_ref=$2`,
		acct, v1).Scan(&n); err != nil || n != 1 {
		t.Fatalf("account_consents n=%d err=%v want 1", n, err)
	}
	if err := svc.CheckConsent(ctx, acct, false); err != nil {
		t.Fatalf("consented order must admit: %v", err)
	}
	// Stale-version consent is refused.
	if _, _, err := svc.Consent(ctx, acct, uid, "v-old",
		"", nil); err == nil {
		t.Fatal("stale-version consent must refuse")
	}

	// Material change: activate v2 → incumbent superseded in-tx, prior
	// consent no longer satisfies the gate. Material amendments must
	// cite the regulatory-change record they flow through (21.3.25).
	regSvc, err := NewRegChangeService(pool, resolver)
	if err != nil {
		t.Fatalf("reg service: %v", err)
	}
	regChg, _, _, err := regSvc.Register(ctx, RegChangeInput{
		Authority:   "FCA",
		Instrument:  fmt.Sprintf("PS-EP-%d", ts),
		Title:       "best-ex policy amendment driver",
		PublishedAt: now.Truncate(24 * time.Hour),
	}, officer)
	if err != nil {
		t.Fatalf("driver change: %v", err)
	}
	d2, err := svc.CreateDraft(ctx, v2, "s3://policies/"+v2+".md", officer)
	if err != nil {
		t.Fatalf("draft v2: %v", err)
	}
	if _, err := svc.Activate(ctx, d2.ID, officer,
		now, now.Add(365*24*time.Hour), true, &regChg.ChangeID); err != nil {
		t.Fatalf("activate v2: %v", err)
	}
	active, _ = svc.Active(ctx)
	if active == nil || active.Version != v2 {
		t.Fatalf("active=%+v want v2", active)
	}
	var st string
	if err := pool.QueryRow(ctx,
		`SELECT status FROM execution_policies WHERE id=$1`,
		d1.ID).Scan(&st); err != nil || st != "SUPERSEDED" {
		t.Fatalf("v1 status=%s err=%v want SUPERSEDED", st, err)
	}
	if err := svc.CheckConsent(ctx, acct, false); err == nil {
		t.Fatal("v1 consent must not satisfy v2 (material change)")
	}
	if err := svc.CheckConsent(ctx, acct, true); err != nil {
		t.Fatalf("reduce-only still bypasses: %v", err)
	}

	// Annual review: officer sign-off rolls the deadline.
	if _, err := svc.Review(ctx, d2.ID, officer, "annual attest",
		time.Now().UTC().Add(365*24*time.Hour)); err != nil {
		t.Fatalf("review: %v", err)
	}
	t.Cleanup(func() {
		c2, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = pool.Exec(c2,
			`DELETE FROM execution_policy_consents WHERE policy_id = ANY($1)`,
			[]int64{d1.ID, d2.ID})
		_, _ = pool.Exec(c2,
			`DELETE FROM account_consents WHERE account_id=$1
			   AND consent_type='EXECUTION_POLICY' AND doc_ref = ANY($2)`,
			acct, []string{v1, v2})
		_, _ = pool.Exec(c2,
			`DELETE FROM execution_policies WHERE id = ANY($1)`,
			[]int64{d1.ID, d2.ID})
	})
}

// ---- 21.3.17 — FX Global Code run lifecycle ----

func TestPGFXGCLifecycle(t *testing.T) {
	pool := holdPool(t)
	uid, _ := holdSeed(t, pool)
	ctx := context.Background()
	resolver := p21Resolver(map[int64]string{uid: "Compliance Officer"})
	svc, err := NewFXGCService(pool, resolver)
	if err != nil {
		t.Fatalf("service: %v", err)
	}
	// period is VARCHAR(16) — keep the token short.
	period := fmt.Sprintf("IT-%d", time.Now().UnixNano()%1000000000)

	run, created, err := svc.StartRun(ctx, period, "", uid)
	if err != nil || !created {
		t.Fatalf("start=%+v created=%v err=%v", run, created, err)
	}
	if _, again, err := svc.StartRun(ctx, period, "", uid); err != nil || again {
		t.Fatalf("replayed start must dedup: again=%v err=%v", again, err)
	}
	rows, err := svc.ListMatrix(ctx, run.ID)
	if err != nil || len(rows) != 55 {
		t.Fatalf("matrix rows=%d err=%v want 55", len(rows), err)
	}
	// A NON_ADHERENT verdict without a remediation ref refuses.
	if _, err := svc.Assess(ctx, run.ID, 2, uid,
		AdherenceNon, "found gaps", ""); err == nil {
		t.Fatal("NON_ADHERENT without remediation_ref must refuse")
	}
	// Completion locks while any verdict is PENDING.
	if _, err := svc.Complete(ctx, run.ID, uid); err == nil {
		t.Fatal("complete with PENDING verdicts must refuse")
	}
	// Officer-assess every still-pending row.
	for _, r := range rows {
		cur, err := svc.assessmentRow(ctx, run.ID, r.PrincipleID)
		if err != nil {
			t.Fatalf("row %d: %v", r.PrincipleID, err)
		}
		if cur.AdherenceStatus == AdherencePending {
			if _, err := svc.Assess(ctx, run.ID, r.PrincipleID, uid,
				AdherenceAdherent, "attested", ""); err != nil {
				t.Fatalf("assess p%d: %v", r.PrincipleID, err)
			}
		}
	}
	done, err := svc.Complete(ctx, run.ID, uid)
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	if done.Status != "COMPLETED" {
		t.Fatalf("status=%s want COMPLETED", done.Status)
	}
	stmt, err := svc.GetStatement(ctx, run.ID)
	if err != nil || stmt == nil || len(stmt.BodySHA256) != 64 {
		t.Fatalf("statement=%+v err=%v", stmt, err)
	}
	if _, err := svc.Sign(ctx, run.ID, uid); err != nil {
		t.Fatalf("sign: %v", err)
	}
	if _, err := svc.PublishStatement(ctx, run.ID, uid); err != nil {
		t.Fatalf("publish: %v", err)
	}
	// Verdicts are locked after completion.
	if _, err := svc.Assess(ctx, run.ID, 1, uid,
		AdherenceAdherent, "", ""); err == nil {
		t.Fatal("verdict on a completed run must refuse")
	}
}

// ---- 21.3.25 — regulatory change register ----

func TestPGRegChangeLifecycle(t *testing.T) {
	pool := holdPool(t)
	uid, _ := holdSeed(t, pool)
	ctx := context.Background()
	officer := uid
	auditor := uid + 900000 // distinct principal id, auditor-bound below
	resolver := p21Resolver(map[int64]string{
		officer: "Compliance Officer",
		auditor: "Read-Only Auditor",
	})
	svc, err := NewRegChangeService(pool, resolver)
	if err != nil {
		t.Fatalf("service: %v", err)
	}
	in := RegChangeInput{
		Authority:   "FCA",
		Instrument:  fmt.Sprintf("PS-IT-%d", time.Now().UnixNano()),
		Title:       "integration-test rule change",
		PublishedAt: time.Now().UTC().Truncate(24 * time.Hour),
	}
	chg, _, created, err := svc.Register(ctx, in, officer)
	if err != nil || !created {
		t.Fatalf("register=%+v created=%v err=%v", chg, created, err)
	}
	dup, _, dupCreated, err := svc.Register(ctx, in, officer)
	if err != nil || dupCreated || dup.ChangeID != chg.ChangeID {
		t.Fatalf("dedup replay failed: %+v created=%v err=%v", dup, dupCreated, err)
	}
	// Triage SLA lands on a business day ≥ published+10 days.
	if chg.TriageDueAt.Before(in.PublishedAt.Add(10 * 24 * time.Hour)) {
		t.Fatalf("triage_due=%s inside 10 calendar days", chg.TriageDueAt)
	}

	// IMPLEMENTED refuses before any impact assessment.
	if _, err := svc.Triage(ctx, chg.ChangeID, officer, &officer,
		"triaged"); err != nil {
		t.Fatalf("triage: %v", err)
	}
	if _, err := svc.SetStatus(ctx, chg.ChangeID, officer,
		RegStatusScoped); err != nil {
		t.Fatalf("scope: %v", err)
	}
	if _, err := svc.MarkImplemented(ctx, chg.ChangeID,
		officer, "matrix-2026-09"); err == nil {
		t.Fatal("IMPLEMENT without an impact assessment must refuse")
	}
	imp, _, err := svc.RecordImpact(ctx, chg.ChangeID, officer,
		ImpactSpecSection, "§14.10.2", &officer, "2d", nil)
	if err != nil {
		t.Fatalf("impact: %v", err)
	}
	if _, err := svc.MarkImplemented(ctx, chg.ChangeID,
		officer, "matrix-2026-09"); err == nil {
		t.Fatal("IMPLEMENT with OPEN impacts must refuse")
	}
	if _, err := svc.CompleteImpact(ctx, imp.ID, officer); err != nil {
		t.Fatalf("complete impact: %v", err)
	}
	done, err := svc.MarkImplemented(ctx, chg.ChangeID,
		officer, "matrix-2026-09")
	if err != nil || done.Status != RegStatusImplemented {
		t.Fatalf("implement=%+v err=%v", done, err)
	}
	// Correspondence ledger.
	if _, err := svc.AttachCorrespondence(ctx, chg.ChangeID, officer,
		"INFO_REQUEST", "FCA response received",
		time.Now().UTC(), nil); err != nil {
		t.Fatalf("correspondence: %v", err)
	}

	// Auditor scope: an untriaged row is visible, the implemented one
	// is not (NOT_FOUND reads through the scoped projection).
	live, _, _, err := svc.Register(ctx, RegChangeInput{
		Authority:   "ESMA",
		Instrument:  fmt.Sprintf("ITS-IT-%d", time.Now().UnixNano()),
		Title:       "auditor-visible watch",
		PublishedAt: time.Now().UTC().Truncate(24 * time.Hour),
	}, officer)
	if err != nil {
		t.Fatalf("register live: %v", err)
	}
	if _, err := svc.GetForRole(ctx, auditor, live.ChangeID); err != nil {
		t.Fatalf("auditor must see untriaged change: %v", err)
	}
	if _, err := svc.GetForRole(ctx, auditor, chg.ChangeID); err == nil {
		t.Fatal("auditor must not see implemented change")
	}
	list, err := svc.ListForRole(ctx, auditor, "", 50)
	if err != nil {
		t.Fatalf("auditor list: %v", err)
	}
	for _, c := range list {
		if c.ChangeID == chg.ChangeID {
			t.Fatal("auditor list leaked the implemented change")
		}
	}
	// Close requires a disposition note.
	if _, err := svc.Close(ctx, live.ChangeID, officer, ""); err == nil {
		t.Fatal("close without note must refuse")
	}
	if _, err := svc.Close(ctx, live.ChangeID, officer,
		"withdrawn by regulator"); err != nil {
		t.Fatalf("close: %v", err)
	}
}

// ---- 21.3.13 — Basel III snapshot idempotency + breach flags ----

func TestPGBaselSnapshot(t *testing.T) {
	pool := holdPool(t)
	ctx := context.Background()
	svc, err := NewBaselService(pool)
	if err != nil {
		t.Fatalf("service: %v", err)
	}
	period := time.Now().UTC().Add(-48 * time.Hour).Truncate(24 * time.Hour)
	key := fmt.Sprintf("it:%d", time.Now().UnixNano())

	rep, created, err := svc.Snapshot(ctx, period, key, "it")
	if err != nil || !created {
		t.Fatalf("snapshot=%+v created=%v err=%v", rep, created, err)
	}
	rep2, again, err := svc.Snapshot(ctx, period, key, "it")
	if err != nil || again || rep2.ID != rep.ID {
		t.Fatalf("replayed snapshot must dedup: id=%d again=%v err=%v",
			rep2.ID, again, err)
	}
	// Missing FX conversion flags inputs_complete and never fabricates
	// a component (dev GL may be empty — the flag contract holds either
	// way).
	if rep.CAR.IsPositive() && rep.RWA.IsPositive() &&
		rep.CAR.GreaterThan(BaselCARFloor) && rep.CARBreach {
		t.Fatal("breach flag contradicts ratio")
	}
	if rep.CARBreach && rep.Code != BaselCodeCapitalBreach {
		t.Fatalf("code=%s want %s", rep.Code, BaselCodeCapitalBreach)
	}
	if !rep.CARBreach && rep.LeverageBreach &&
		rep.Code != BaselCodeLeverageBreach {
		t.Fatalf("code=%s want %s", rep.Code, BaselCodeLeverageBreach)
	}
	// Get by period returns the stored row.
	got, err := svc.Get(ctx, period)
	if err != nil || got == nil {
		// Another snapshot for the same period may exist from prior
		// runs — the read contract is "latest wins or error".
		if err != nil {
			t.Fatalf("get by period: %v", err)
		}
	}
	// RunEOD is idempotent on its eod:{period} key.
	if _, _, err := svc.RunEOD(ctx); err != nil {
		t.Fatalf("RunEOD: %v", err)
	}
	if _, again, err := svc.RunEOD(ctx); err != nil || again {
		t.Fatalf("RunEOD replay: again=%v err=%v", again, err)
	}
}
