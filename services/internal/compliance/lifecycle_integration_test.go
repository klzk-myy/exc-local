// PostgreSQL-gated integration tests for the Phase-14 lifecycle +
// categorization store paths (Tasks 14.3.4 / 14.3.7). Gated on
// EXC_PG_TEST=1 like integration_test.go; each test builds a scratch
// schema with the real migration files (009/010 audit chain, 017/203/
// 204 KYC, 042 categorization) plus minimal anchor fixtures.
package compliance

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// applyLifecycleSchema builds the anchors the lifecycle/categorization
// paths touch (accounts with user_id + updated_at, audit tables via the
// real migrations, instruments + positions for the open-exposure read)
// then the owned migrations.
func applyLifecycleSchema(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(ctx, `
		CREATE TABLE accounts (
		    id         BIGSERIAL PRIMARY KEY,
		    user_id    BIGINT NOT NULL,
		    kyc_tier   VARCHAR(4) NOT NULL DEFAULT 'T0',
		    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
		);
		CREATE TABLE risk_limits (
		    id                   BIGSERIAL PRIMARY KEY,
		    account_id           BIGINT,
		    symbol               VARCHAR(32),
		    tier                 VARCHAR(16),
		    max_daily_volume     NUMERIC(28,8),
		    daily_withdraw_limit NUMERIC(28,8)
		);
		CREATE TABLE instruments (
		    id              BIGSERIAL PRIMARY KEY,
		    symbol          VARCHAR(32) NOT NULL,
		    instrument_type VARCHAR(16) NOT NULL
		);
		CREATE TABLE positions (
		    id            BIGSERIAL PRIMARY KEY,
		    account_id    BIGINT NOT NULL,
		    instrument_id BIGINT NOT NULL,
		    quantity      NUMERIC(28,8) NOT NULL
		)`); err != nil {
		t.Fatalf("lifecycle anchors: %v", err)
	}
	for _, m := range []string{
		"009_create_audit_hash_chain.up.sql",
		"010_create_admin_audit_log.up.sql",
		"017_create_kyc_documents.up.sql",
		"203_kyc_submission.up.sql",
		"204_kyc_ops_matrix.up.sql",
		"042_client_categorization.up.sql",
	} {
		execSQLFile(t, ctx, pool, m)
	}
}

func mkLifecycleAccount(t *testing.T, ctx context.Context, pool *pgxpool.Pool,
	userID int64, tier string) int64 {
	t.Helper()
	var id int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO accounts (user_id, kyc_tier) VALUES ($1,$2) RETURNING id`,
		userID, tier).Scan(&id); err != nil {
		t.Fatalf("insert account: %v", err)
	}
	return id
}

func mkSubmission(t *testing.T, ctx context.Context, pool *pgxpool.Pool,
	accountID int64, tier, status string) int64 {
	t.Helper()
	var id int64
	if err := pool.QueryRow(ctx, `
		INSERT INTO kyc_submissions (account_id, requested_tier, status, sla_due_at)
		VALUES ($1,$2,$3, now() + interval '24 hours') RETURNING id`,
		accountID, tier, status).Scan(&id); err != nil {
		t.Fatalf("insert submission: %v", err)
	}
	return id
}

func lifecycleStore(t *testing.T, pool *pgxpool.Pool) *PgStore {
	t.Helper()
	st, err := NewPgStore(pool, testBox(t))
	if err != nil {
		t.Fatalf("NewPgStore: %v", err)
	}
	return st
}

// TestITApprove_AssignsTierAndAudit covers the approve path end-to-end:
// submission → APPROVED, accounts.kyc_tier flips, reverify_due_at lands
// ~12 months out, and the audit row + hash-chain link commit together.
func TestITApprove_AssignsTierAndAudit(t *testing.T) {
	ctx, pool := itPool(t)
	applyLifecycleSchema(t, ctx, pool)
	st := lifecycleStore(t, pool)

	acct := mkLifecycleAccount(t, ctx, pool, 900, "T0")
	sub := mkSubmission(t, ctx, pool, acct, "T2", "PENDING_REVIEW")

	now := time.Now().UTC()
	res, err := st.DecideSubmissionTx(ctx, DecisionTx{
		SubmissionID: sub, Approve: true, ReviewerID: 55,
		ClientIP: "127.0.0.1", Now: now, ReverifyMonths: 12,
	})
	if err != nil {
		t.Fatalf("decide: %v", err)
	}
	if res.Decision != "APPROVED" || res.AssignedTier != "T2" {
		t.Fatalf("decision: %+v", res)
	}
	var tier, status string
	var due *time.Time
	if err := pool.QueryRow(ctx, `
		SELECT a.kyc_tier, s.status, s.reverify_due_at
		  FROM accounts a JOIN kyc_submissions s ON s.account_id = a.id
		 WHERE s.id=$1`, sub).Scan(&tier, &status, &due); err != nil {
		t.Fatalf("readback: %v", err)
	}
	if tier != "T2" || status != "APPROVED" {
		t.Fatalf("tier=%s status=%s", tier, status)
	}
	if due == nil || due.Before(now.AddDate(0, 11, 0)) {
		t.Fatalf("reverify_due_at must be ~12 months out, got %v", due)
	}
	var audits, links int64
	if err := pool.QueryRow(ctx, `
		SELECT (SELECT count(*) FROM admin_audit_log WHERE action='kyc.approve'),
		       (SELECT count(*) FROM audit_hash_chain)`).Scan(&audits, &links); err != nil {
		t.Fatalf("audit readback: %v", err)
	}
	if audits != 1 || links != 1 {
		t.Fatalf("audit row + chain link must commit together: audits=%d links=%d", audits, links)
	}
	// Second approve on a decided submission → transition error.
	if _, err := st.DecideSubmissionTx(ctx, DecisionTx{
		SubmissionID: sub, Approve: true, ReviewerID: 55, Now: now,
	}); codeOf(t, err) != "INVALID_REQUEST" {
		t.Fatalf("re-decide must reject, got %v", err)
	}
}

// TestITReject_PersistsReason — rejection leaves the tier untouched,
// persists reject_reason and writes the audit row.
func TestITReject_PersistsReason(t *testing.T) {
	ctx, pool := itPool(t)
	applyLifecycleSchema(t, ctx, pool)
	st := lifecycleStore(t, pool)

	acct := mkLifecycleAccount(t, ctx, pool, 901, "T0")
	sub := mkSubmission(t, ctx, pool, acct, "T1", "PENDING_REVIEW")

	if _, err := st.DecideSubmissionTx(ctx, DecisionTx{
		SubmissionID: sub, Approve: false, Reason: "document illegible",
		ReviewerID: 55, Now: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("reject: %v", err)
	}
	var tier, status, reason string
	if err := pool.QueryRow(ctx, `
		SELECT a.kyc_tier, s.status, s.reject_reason
		  FROM accounts a JOIN kyc_submissions s ON s.account_id = a.id
		 WHERE s.id=$1`, sub).Scan(&tier, &status, &reason); err != nil {
		t.Fatalf("readback: %v", err)
	}
	if tier != "T0" || status != "REJECTED" || reason != "document illegible" {
		t.Fatalf("tier=%s status=%s reason=%q", tier, status, reason)
	}
	var n int64
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM admin_audit_log WHERE action='kyc.reject'`).Scan(&n); err != nil {
		t.Fatalf("audit readback: %v", err)
	}
	if n != 1 {
		t.Fatalf("reject audit row missing: %d", n)
	}
}

// TestITSweep_DowngradeIdempotent — an overdue APPROVED submission
// downgrades its T2 account to T1 once; the second sweep finds nothing
// (the feed predicates exclude resolved rows).
func TestITSweep_DowngradeIdempotent(t *testing.T) {
	ctx, pool := itPool(t)
	applyLifecycleSchema(t, ctx, pool)
	st := lifecycleStore(t, pool)

	acct := mkLifecycleAccount(t, ctx, pool, 902, "T2")
	sub := mkSubmission(t, ctx, pool, acct, "T2", "APPROVED")
	if _, err := pool.Exec(ctx, `
		UPDATE kyc_submissions
		   SET verified_at = now() - interval '13 months',
		       reverify_due_at = now() - interval '1 month'
		 WHERE id=$1`, sub); err != nil {
		t.Fatalf("backdate reverify: %v", err)
	}

	rows, err := st.OverdueReverifications(ctx, time.Now().UTC(), 100)
	if err != nil || len(rows) != 1 {
		t.Fatalf("overdue feed: n=%d err=%v", len(rows), err)
	}
	applied, err := st.DowngradeReverifyTx(ctx,
		DowngradeTx{OverdueReverify: rows[0], Now: time.Now().UTC()})
	if err != nil || !applied {
		t.Fatalf("downgrade: applied=%v err=%v", applied, err)
	}
	var tier, status string
	if err := pool.QueryRow(ctx, `
		SELECT a.kyc_tier, s.status FROM accounts a
		  JOIN kyc_submissions s ON s.id=$2 WHERE a.id=$1`,
		acct, sub).Scan(&tier, &status); err != nil {
		t.Fatalf("readback: %v", err)
	}
	if tier != "T1" || status != "EXPIRED" {
		t.Fatalf("tier=%s status=%s", tier, status)
	}
	var n int64
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM admin_audit_log WHERE action='kyc.reverify_downgrade'`).Scan(&n); err != nil {
		t.Fatalf("audit readback: %v", err)
	}
	if n != 1 {
		t.Fatalf("downgrade audit row missing: %d", n)
	}
	// Idempotent: the feed no longer surfaces the row and a replayed
	// downgrade applies nothing.
	rows, err = st.OverdueReverifications(ctx, time.Now().UTC(), 100)
	if err != nil || len(rows) != 0 {
		t.Fatalf("idempotent feed must be empty, got n=%d err=%v", len(rows), err)
	}
	applied, err = st.DowngradeReverifyTx(ctx, DowngradeTx{
		OverdueReverify: OverdueReverify{SubmissionID: sub, AccountID: acct,
			UserID: 902, RequestedTier: "T2"},
		Now: time.Now().UTC(),
	})
	if err != nil || applied {
		t.Fatalf("replayed downgrade must no-op, got applied=%v err=%v", applied, err)
	}
}

// TestITCategorization_RoundTrip — assessment insert/read, category
// readback and the audited SetCategoryTx write.
func TestITCategorization_RoundTrip(t *testing.T) {
	ctx, pool := itPool(t)
	applyLifecycleSchema(t, ctx, pool)
	st := lifecycleStore(t, pool)

	acct := mkLifecycleAccount(t, ctx, pool, 903, "T1")
	// Migration 042 defaults: RETAIL + nbp.
	cat, nbp, err := st.ClientCategory(ctx, acct)
	if err != nil || cat != "RETAIL" || !nbp {
		t.Fatalf("defaults: cat=%q nbp=%v err=%v", cat, nbp, err)
	}
	now := time.Now().UTC()
	a := &Assessment{
		AccountID: acct, InstrumentClass: "FORWARD", Outcome: "PASS",
		Score: 80, AssessedAt: now, ExpiresAt: now.AddDate(0, 12, 0),
	}
	if err := st.InsertAssessment(ctx, a); err != nil {
		t.Fatalf("insert assessment: %v", err)
	}
	latest, err := st.LatestAssessment(ctx, acct, "FORWARD")
	if err != nil || latest == nil || latest.Outcome != "PASS" {
		t.Fatalf("latest: %+v err=%v", latest, err)
	}
	// Category assignment: PROFESSIONAL drops nbp; open derivative
	// exposure reports on the change record.
	var fwdID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO instruments (symbol, instrument_type) VALUES ('EURUSD-1M','FORWARD') RETURNING id`).Scan(&fwdID); err != nil {
		t.Fatalf("instrument: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO positions (account_id, instrument_id, quantity) VALUES ($1,$2,1000)`, acct, fwdID); err != nil {
		t.Fatalf("position: %v", err)
	}
	ch, err := st.SetCategoryTx(ctx, SetCategoryTx{
		AccountID: acct, Category: CategoryProfessional, NBP: false,
		ReviewerID: 55, Evidence: "annex ii", Now: now,
		Action: "account.client_category",
	})
	if err != nil {
		t.Fatalf("set category: %v", err)
	}
	if ch.From != CategoryRetail || ch.To != CategoryProfessional || !ch.OpenExposure {
		t.Fatalf("change: %+v", ch)
	}
	cat, nbp, err = st.ClientCategory(ctx, acct)
	if err != nil || cat != "PROFESSIONAL" || nbp {
		t.Fatalf("post-change: cat=%q nbp=%v err=%v", cat, nbp, err)
	}
	var audits int64
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM admin_audit_log WHERE action='account.client_category'`).Scan(&audits); err != nil {
		t.Fatalf("audit readback: %v", err)
	}
	if audits != 1 {
		t.Fatalf("category audit row missing: %d", audits)
	}
}
