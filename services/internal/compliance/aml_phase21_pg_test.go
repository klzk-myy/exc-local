// Phase-21 Tasks 21.3.2/21.3.3/21.3.6 — postgres-gated integration
// tests for the travel-rule gate, SAR lifecycle and AML/MSB engine.
//
// Gated: EXC_PG_TEST=1 (holdPool helper, dev DSN or EXC_PG_DSN).
// Requires migrations 032 + 033 applied on the target schema.
package compliance

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"

	"exchange/internal/funding"
)

// amlCleanup deletes every Phase-21 row the test seeded before the
// holdSeed teardown removes the account/user (cleanups run LIFO — this
// registers AFTER holdSeed so it fires first). FILED sar_reports rows
// are trigger-immutable; session_replication_role bypasses the trigger
// where the dev user is privileged — residue is otherwise harmless.
func amlCleanup(t *testing.T, pool *pgxpool.Pool, aid int64) {
	t.Helper()
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = pool.Exec(c,
			`DELETE FROM travel_rule_records WHERE account_id=$1`, aid)
		_, _ = pool.Exec(c,
			`DELETE FROM aml_monitoring_events WHERE account_id=$1`, aid)
		_, _ = pool.Exec(c,
			`DELETE FROM ctr_reports WHERE account_id=$1`, aid)
		_, _ = pool.Exec(c,
			`DELETE FROM aml_account_assessments WHERE account_id=$1`, aid)
		_, _ = pool.Exec(c, `SET session_replication_role = 'replica'`)
		_, _ = pool.Exec(c,
			`DELETE FROM sar_reports WHERE account_id=$1`, aid)
		_, _ = pool.Exec(c, `SET session_replication_role = 'origin'`)
		_, _ = pool.Exec(c,
			`DELETE FROM funding_transactions WHERE account_id=$1`, aid)
	})
}

// seedFundingTx inserts a CONFIRMED funding row for the seeded account.
// createdAt overrides let the AML scan pin a private business day.
func seedFundingTx(t *testing.T, pool *pgxpool.Pool, aid int64,
	typ, ccy, amt string, usd *string, at time.Time) int64 {
	t.Helper()
	var id int64
	err := pool.QueryRow(context.Background(), `
		INSERT INTO funding_transactions
		    (account_id, currency, type, amount, status, usd_amount, created_at)
		VALUES ($1,$2,$3,$4::numeric,'CONFIRMED',$5::numeric,$6)
		RETURNING id`, aid, ccy, typ, amt, usd, at).Scan(&id)
	if err != nil {
		t.Fatalf("seed funding tx: %v", err)
	}
	return id
}

func strP(s string) *string { return &s }

// beginTx starts a tx whose cleanup always rolls back — a Fatalf while
// the conn is checked out would otherwise hang pgxpool.Close (Rollback
// after Commit is a no-op).
func beginTx(t *testing.T, pool *pgxpool.Pool, ctx context.Context) pgx.Tx {
	t.Helper()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("tx: %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	return tx
}

// ---------------------------------------------------------------------------
// Task 21.3.2 — travel rule outbound gate
// ---------------------------------------------------------------------------

// A complete >= $1,000 wire records COMPLETE and enriches the payment
// with the ordering customer (MT103 50K); a missing field holds the
// wire as MISSING_INFO and an officer supply cures it.
func TestIntegrationTravelRuleOutbound(t *testing.T) {
	pool := holdPool(t)
	_, aid := holdSeed(t, pool)
	amlCleanup(t, pool, aid)
	ctx := context.Background()
	usd := "5000"
	wID := seedFundingTx(t, pool, aid, "WITHDRAWAL", "USD", "5000", &usd,
		time.Now().UTC())

	svc, err := NewTravelRuleService(pool)
	if err != nil {
		t.Fatalf("service: %v", err)
	}
	// Resolver supplies the KYC legal name only — no client bank
	// account number and no address.
	svc.WithClientResolver(func(context.Context, pgx.Tx, int64) (*TravelRuleParty, error) {
		return &TravelRuleParty{Name: "Alice Doe"}, nil
	})

	w := &funding.DispatchableWithdrawal{
		ID: wID, AccountID: aid, Currency: "USD",
		Amount:    decimal.RequireFromString("5000"),
		USDAmount: decPtr("5000"), Status: "CONFIRMED"}
	pay := funding.OutboundPayment{
		Rail: funding.RailSWIFT, Currency: "USD",
		Amount:     decimal.RequireFromString("5000"),
		DebtorName: "Exchange Ops Nostro", DebtorAccount: "NOSTRO-1",
		CreditorName: "Bob Receiver", CreditorIBAN: "DE89370400440532013000"}

	// Missing originator.address → MISSING_INFO, no enrichment.
	tx := beginTx(t, pool, ctx)
	ep, missing, err := svc.EnforceOutbound(ctx, tx, w, pay)
	if err != nil {
		t.Fatalf("enforce: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if len(missing) != 1 || missing[0] != "originator.address" {
		t.Fatalf("missing=%v, want [originator.address]", missing)
	}
	if ep.OriginatorName != "" {
		t.Fatalf("incomplete record must not enrich payment: %+v", ep)
	}
	var recID int64
	var status string
	if err := pool.QueryRow(ctx, `
		SELECT id, status FROM travel_rule_records
		WHERE transfer_id=$1 AND direction='OUTBOUND'`, wID).
		Scan(&recID, &status); err != nil {
		t.Fatalf("record row: %v", err)
	}
	if status != TravelRuleMissing {
		t.Fatalf("status=%s, want MISSING_INFO", status)
	}
	var fieldRef string
	if err := pool.QueryRow(ctx,
		`SELECT swift_field_ref FROM travel_rule_records WHERE id=$1`,
		recID).Scan(&fieldRef); err != nil || fieldRef != "MT103:50K/59" {
		t.Fatalf("field ref %q err %v", fieldRef, err)
	}
	// Dispatch sweep retry — same outcome, same row (idempotent).
	tx = beginTx(t, pool, ctx)
	ep, missing, err = svc.EnforceOutbound(ctx, tx, w, pay)
	_ = tx.Commit(ctx)
	if err != nil || len(missing) != 1 {
		t.Fatalf("retry must re-report the same gap: %v %v", missing, err)
	}
	var n int64
	_ = pool.QueryRow(ctx,
		`SELECT count(*) FROM travel_rule_records WHERE transfer_id=$1`,
		wID).Scan(&n)
	if n != 1 {
		t.Fatalf("retry duplicated the record: %d rows", n)
	}

	// Officer supplies the address → COMPLETE → next sweep enriches.
	rec, err := svc.SupplyInfo(ctx, recID, 77, TravelRuleSupply{
		Originator: &TravelRuleParty{Address: "1 Main St, Frankfurt"}})
	if err != nil {
		t.Fatalf("supply: %v", err)
	}
	if rec.Status != TravelRuleOK || len(rec.MissingFields) != 0 {
		t.Fatalf("supply did not cure: %+v", rec)
	}
	tx = beginTx(t, pool, ctx)
	ep, missing, err = svc.EnforceOutbound(ctx, tx, w, pay)
	_ = tx.Commit(ctx)
	if err != nil || len(missing) != 0 {
		t.Fatalf("cured record must pass: %v %v", missing, err)
	}
	if ep.OriginatorName != "Alice Doe" ||
		ep.OriginatorAddress != "1 Main St, Frankfurt" ||
		ep.OriginatorAccount != fmt.Sprintf("acct-%d", aid) {
		t.Fatalf("50K enrichment wrong: %+v", ep)
	}
}

// A sub-$1,000 transfer is out of scope — no record, no hold.
func TestIntegrationTravelRuleBelowThreshold(t *testing.T) {
	pool := holdPool(t)
	_, aid := holdSeed(t, pool)
	amlCleanup(t, pool, aid)
	ctx := context.Background()
	usd := "999.99"
	wID := seedFundingTx(t, pool, aid, "WITHDRAWAL", "USD", "999.99", &usd,
		time.Now().UTC())
	svc, _ := NewTravelRuleService(pool)
	w := &funding.DispatchableWithdrawal{
		ID: wID, AccountID: aid, Currency: "USD",
		Amount:    decimal.RequireFromString("999.99"),
		USDAmount: decPtr("999.99"), Status: "CONFIRMED"}
	tx := beginTx(t, pool, ctx)
	_, missing, err := svc.EnforceOutbound(ctx, tx, w,
		funding.OutboundPayment{Rail: funding.RailSWIFT})
	_ = tx.Commit(ctx)
	if err != nil || missing != nil {
		t.Fatalf("sub-threshold wire must pass untouched: %v %v", missing, err)
	}
	var n int64
	_ = pool.QueryRow(ctx,
		`SELECT count(*) FROM travel_rule_records WHERE transfer_id=$1`,
		wID).Scan(&n)
	if n != 0 {
		t.Fatalf("out-of-scope wire recorded %d rows", n)
	}
}

// Inbound wire: sender identity rides the confirmations; a missing
// originator field leaves the record MISSING_INFO (the deposit parks
// in PENDING_REVIEW upstream via the TRAVEL_RULE_MISSING_INFO flag).
func TestIntegrationTravelRuleInbound(t *testing.T) {
	pool := holdPool(t)
	_, aid := holdSeed(t, pool)
	amlCleanup(t, pool, aid)
	ctx := context.Background()
	usd := "8000"
	dID := seedFundingTx(t, pool, aid, "DEPOSIT", "USD", "8000", &usd,
		time.Now().UTC())
	svc, _ := NewTravelRuleService(pool)
	svc.WithClientResolver(func(context.Context, pgx.Tx, int64) (*TravelRuleParty, error) {
		return &TravelRuleParty{Name: "Alice Doe"}, nil
	})
	bm := "SWIFT"
	row := &funding.FundingTxRow{
		ID: dID, AccountID: aid, Currency: "USD", Type: "DEPOSIT",
		Amount:    decimal.RequireFromString("8000"),
		USDAmount: decPtr("8000"), BankMethod: &bm, Status: "PENDING"}
	confs := []funding.DepositConfirmationRow{{
		FundingTransactionID: dID, Source: "BANK",
		SenderName: strP("Carl Sender"), SenderAccount: strP("DE111111")}}
	tx := beginTx(t, pool, ctx)
	missing, err := svc.CheckInbound(ctx, tx, row, confs)
	_ = tx.Commit(ctx)
	if err != nil {
		t.Fatalf("check inbound: %v", err)
	}
	if len(missing) != 1 || missing[0] != "originator.address" {
		t.Fatalf("missing=%v, want [originator.address]", missing)
	}
	var status string
	if err := pool.QueryRow(ctx,
		`SELECT status FROM travel_rule_records
		 WHERE transfer_id=$1 AND direction='INBOUND'`, dID).
		Scan(&status); err != nil || status != TravelRuleMissing {
		t.Fatalf("inbound record status %q err %v", status, err)
	}
}

// ---------------------------------------------------------------------------
// Task 21.3.3 — SAR lifecycle
// ---------------------------------------------------------------------------

// Full four-eyes chain: draft → review → (self-approvals denied) →
// distinct approve → file → immutable. Deadline = detected+30d.
func TestIntegrationSARLifecycle(t *testing.T) {
	pool := holdPool(t)
	_, aid := holdSeed(t, pool)
	amlCleanup(t, pool, aid)
	ctx := context.Background()
	svc, err := NewSARService(pool, HoldRoleResolver(officerRole))
	if err != nil {
		t.Fatalf("service: %v", err)
	}
	o1, o2, o3 := int64(101), int64(102), int64(103)
	detected := time.Now().UTC().Add(-48 * time.Hour).Truncate(time.Second)

	rep, created, err := svc.Draft(ctx, SARDraftInput{
		TriggerType: SARTriggerManual, AccountID: &aid,
		Description: "structuring at the counter", SourceRef: "manual:it-1",
		DetectedAt: detected, CreatedBy: &o1,
		TransactionIDs: []int64{11, 12}})
	if err != nil || !created {
		t.Fatalf("draft: %v created=%v", err, created)
	}
	if rep.Status != SARStatusDraft {
		t.Fatalf("status=%s", rep.Status)
	}
	wantDeadline := detected.Add(SARFilingWindow)
	if rep.FilingDeadline.Sub(wantDeadline).Abs() > time.Second {
		t.Fatalf("deadline %v != detected+30d %v",
			rep.FilingDeadline, wantDeadline)
	}
	// Dedup: same source_ref replays the stored row.
	rep2, created2, err := svc.Draft(ctx, SARDraftInput{
		TriggerType: SARTriggerManual, AccountID: &aid,
		Description: "dup", SourceRef: "manual:it-1"})
	if err != nil || created2 || rep2.ID != rep.ID {
		t.Fatalf("dedup replay: %v created=%v id=%d", err, created2, rep2.ID)
	}

	// Review.
	rep, err = svc.Review(ctx, rep.ID, o2, "checked evidence")
	if err != nil || rep.Status != SARStatusUnderReview {
		t.Fatalf("review: %v %+v", err, rep)
	}
	// Four-eyes: reviewer cannot approve; drafter cannot approve.
	if _, err := svc.Approve(ctx, rep.ID, o2, "ok"); err == nil ||
		codeOf(t, err) != "SAR_DUAL_CONTROL_REQUIRED" {
		t.Fatalf("reviewer self-approve: got %v", err)
	}
	if _, err := svc.Approve(ctx, rep.ID, o1, "ok"); err == nil ||
		codeOf(t, err) != "SAR_DUAL_CONTROL_REQUIRED" {
		t.Fatalf("drafter self-approve: got %v", err)
	}
	// A DISTINCT third officer approves.
	rep, err = svc.Approve(ctx, rep.ID, o3, "approved for filing")
	if err != nil || rep.Status != SARStatusApproved {
		t.Fatalf("approve: %v %+v", err, rep)
	}
	// Filing needs the BSA tracking reference.
	if _, err := svc.File(ctx, rep.ID, o2, ""); err == nil ||
		codeOf(t, err) != "INVALID_REQUEST" {
		t.Fatalf("empty filing_ref: got %v", err)
	}
	rep, err = svc.File(ctx, rep.ID, o2, "BSA-2026-000123")
	if err != nil || rep.Status != SARStatusFiled ||
		rep.FilingRef != "BSA-2026-000123" || rep.FiledAt == nil {
		t.Fatalf("file: %v %+v", err, rep)
	}
	// Immutable at the DB layer — UPDATE and DELETE both refuse.
	if _, err := pool.Exec(ctx,
		`UPDATE sar_reports SET status='REJECTED' WHERE id=$1`,
		rep.ID); err == nil {
		t.Fatalf("filed SAR must be immutable to UPDATE")
	}
	if _, err := pool.Exec(ctx,
		`DELETE FROM sar_reports WHERE id=$1`, rep.ID); err == nil {
		t.Fatalf("filed SAR must be immutable to DELETE")
	}
	// Service-side guards agree.
	if _, err := svc.Reject(ctx, rep.ID, o3, "nope"); err == nil ||
		codeOf(t, err) != "INVALID_REQUEST" {
		t.Fatalf("filed reject: got %v", err)
	}
}

// The signal backstop drafts one SAR per OPEN surveillance signal —
// dedup on 'signal:{id}' makes re-polls no-ops.
func TestIntegrationSARFromOpenSignals(t *testing.T) {
	pool := holdPool(t)
	_, aid := holdSeed(t, pool)
	amlCleanup(t, pool, aid)
	ctx := context.Background()
	svc, err := NewSARService(pool, HoldRoleResolver(officerRole))
	if err != nil {
		t.Fatalf("service: %v", err)
	}
	var sigID int64
	err = pool.QueryRow(ctx, `
		INSERT INTO surveillance_signals
		    (signal_type, symbol, account_hash, first_l3_seq, last_l3_seq,
		     window_start, window_end, evidence, dedup_key)
		VALUES ('SPOOFING','EURUSD',$1,1,10,now(),now(),'{}',$2)
		RETURNING id`, aid, fmt.Sprintf("it-sar-%d", time.Now().UnixNano())).
		Scan(&sigID)
	if err != nil {
		t.Fatalf("seed signal: %v", err)
	}
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = pool.Exec(c, `SET session_replication_role = 'replica'`)
		_, _ = pool.Exec(c,
			`DELETE FROM sar_reports WHERE source_ref=$1`,
			fmt.Sprintf("signal:%d", sigID))
		_, _ = pool.Exec(c, `SET session_replication_role = 'origin'`)
		_, _ = pool.Exec(c,
			`DELETE FROM surveillance_signals WHERE id=$1`, sigID)
	})
	n, err := svc.DraftFromOpenSignals(ctx, 50)
	if err != nil {
		t.Fatalf("poller: %v", err)
	}
	if n < 1 {
		t.Fatalf("poller drafted %d, want >=1", n)
	}
	var src string
	if err := pool.QueryRow(ctx,
		`SELECT source_ref FROM sar_reports
		 WHERE source_ref=$1`, fmt.Sprintf("signal:%d", sigID)).
		Scan(&src); err != nil {
		t.Fatalf("signal SAR missing: %v", err)
	}
	// Second pass: the open signal already has a report — no dup.
	n, err = svc.DraftFromOpenSignals(ctx, 50)
	if err != nil {
		t.Fatalf("re-poll: %v", err)
	}
	var dupN int64
	_ = pool.QueryRow(ctx,
		`SELECT count(*) FROM sar_reports WHERE source_ref=$1`, src).Scan(&dupN)
	if dupN != 1 {
		t.Fatalf("dedup broken: %d rows", dupN)
	}
}

// The hold-escalation seam opens a DRAFT SAR keyed 'hold:{id}'.
func TestIntegrationHoldEscalateDraftsSAR(t *testing.T) {
	pool := holdPool(t)
	_, aid := holdSeed(t, pool)
	amlCleanup(t, pool, aid)
	ctx := context.Background()
	sarSvc, err := NewSARService(pool, HoldRoleResolver(officerRole))
	if err != nil {
		t.Fatalf("sar: %v", err)
	}
	svc := holdSvc(pool, &fakeRestingCanceller{}, &fakeHoldAlerter{},
		nil, HoldRoleResolver(officerRole)).WithSARDraft(sarSvc)
	h, err := svc.PlaceHold(ctx, PlaceHoldRequest{
		AccountID: aid, Trigger: HoldTriggerManual, Reason: "r"})
	if err != nil {
		t.Fatalf("place: %v", err)
	}
	if _, err := svc.EscalateSAR(ctx, h.HoldID, 9, "confirmed structuring"); err != nil {
		t.Fatalf("escalate: %v", err)
	}
	var trig string
	if err := pool.QueryRow(ctx,
		`SELECT trigger_type FROM sar_reports WHERE source_ref=$1`,
		"hold:"+h.HoldID).Scan(&trig); err != nil ||
		trig != SARTriggerHoldEscalated {
		t.Fatalf("hold SAR missing: trig=%q err=%v", trig, err)
	}
}

// ---------------------------------------------------------------------------
// Task 21.3.6 — CTR aggregation, structuring, scoring, program register
// ---------------------------------------------------------------------------

// Two sub-threshold deposits summing >= $10,000 land a CTR row, fire
// both monitoring rules, draft the STRUCTURING SAR and escalate the
// account to EDD (score 50+20 >= 60). Re-scan is dedup-idempotent.
func TestIntegrationAMLScanCTRAndStructuring(t *testing.T) {
	pool := holdPool(t)
	_, aid := holdSeed(t, pool)
	amlCleanup(t, pool, aid)
	ctx := context.Background()
	sarSvc, err := NewSARService(pool, HoldRoleResolver(officerRole))
	if err != nil {
		t.Fatalf("sar: %v", err)
	}
	aml, err := NewAMLService(pool, sarSvc)
	if err != nil {
		t.Fatalf("aml: %v", err)
	}
	// A private business day far in the past — the dev DB carries no
	// other traffic inside the scan window.
	day := time.Date(2020, 6, 15, 0, 0, 0, 0, time.UTC)
	u6k := "6000"
	seedFundingTx(t, pool, aid, "DEPOSIT", "USD", "6000", &u6k,
		day.Add(2*time.Hour))
	seedFundingTx(t, pool, aid, "DEPOSIT", "USD", "6000", &u6k,
		day.Add(6*time.Hour))

	n, err := aml.ScanBusinessDay(ctx, day)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if n != 1 {
		t.Fatalf("scan processed %d account-days, want 1", n)
	}
	var ctrTotal string
	var ctrN int64
	err = pool.QueryRow(ctx, `
		SELECT count(*), coalesce(max(total_usd)::text,'0')
		FROM ctr_reports WHERE account_id=$1 AND business_date=$2`,
		aid, day).Scan(&ctrN, &ctrTotal)
	if err != nil || ctrN != 1 || ctrTotal != "12000.00000000" {
		t.Fatalf("ctr row: n=%d total=%q err=%v", ctrN, ctrTotal, err)
	}
	// Both rule detections landed.
	var evN int64
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM aml_monitoring_events
		WHERE account_id=$1 AND rule_id IN ('STRUCTURING','CTR_THRESHOLD')`,
		aid).Scan(&evN); err != nil || evN != 2 {
		t.Fatalf("monitoring events=%d err=%v", evN, err)
	}
	// The structuring SAR drafted against the transaction set.
	var sarN int64
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM sar_reports
		WHERE account_id=$1 AND trigger_type='STRUCTURING'`, aid).
		Scan(&sarN); err != nil || sarN != 1 {
		t.Fatalf("structuring SAR rows=%d err=%v", sarN, err)
	}
	// Score 70 >= 60 → EDD assessment + MONITORING_RULE SAR.
	var lvl string
	var score int
	if err := pool.QueryRow(ctx,
		`SELECT cdd_level, score FROM aml_account_assessments
		 WHERE account_id=$1`, aid).Scan(&lvl, &score); err != nil {
		t.Fatalf("assessment: %v", err)
	}
	if lvl != "EDD" || score != 70 {
		t.Fatalf("assessment=%s/%d, want EDD/70", lvl, score)
	}
	var msarN int64
	_ = pool.QueryRow(ctx, `
		SELECT count(*) FROM sar_reports
		WHERE account_id=$1 AND trigger_type='MONITORING_RULE'`, aid).
		Scan(&msarN)
	if msarN != 1 {
		t.Fatalf("score SAR rows=%d, want 1", msarN)
	}
	// Idempotent re-scan: same aggregates, no new events or reports.
	if _, err := aml.ScanBusinessDay(ctx, day); err != nil {
		t.Fatalf("re-scan: %v", err)
	}
	var allSar int64
	_ = pool.QueryRow(ctx,
		`SELECT count(*) FROM sar_reports WHERE account_id=$1`, aid).
		Scan(&allSar)
	if allSar != 2 {
		t.Fatalf("re-scan duplicated SARs: %d rows", allSar)
	}
	_ = pool.QueryRow(ctx,
		`SELECT count(*) FROM aml_monitoring_events WHERE account_id=$1`,
		aid).Scan(&evN)
	if evN != 2 {
		t.Fatalf("re-scan duplicated events: %d rows", evN)
	}
}

// Below the CTR floor the scan writes nothing.
func TestIntegrationAMLScanBelowThreshold(t *testing.T) {
	pool := holdPool(t)
	_, aid := holdSeed(t, pool)
	amlCleanup(t, pool, aid)
	ctx := context.Background()
	aml, err := NewAMLService(pool, nil)
	if err != nil {
		t.Fatalf("aml: %v", err)
	}
	day := time.Date(2020, 7, 20, 0, 0, 0, 0, time.UTC)
	u5k := "5000"
	seedFundingTx(t, pool, aid, "DEPOSIT", "USD", "5000", &u5k,
		day.Add(time.Hour))
	n, err := aml.ScanBusinessDay(ctx, day)
	if err != nil || n != 0 {
		t.Fatalf("scan=%d err=%v, want 0", n, err)
	}
	var ctrN int64
	_ = pool.QueryRow(ctx,
		`SELECT count(*) FROM ctr_reports WHERE account_id=$1`, aid).
		Scan(&ctrN)
	if ctrN != 0 {
		t.Fatalf("sub-threshold CTR row exists")
	}
}

// The program register: breaches list every missing mandatory artifact;
// registering the full set clears to Compliant and same-type rows
// supersede cleanly.
func TestIntegrationAMLProgramStatus(t *testing.T) {
	pool := holdPool(t)
	_, aid := holdSeed(t, pool)
	ctx := context.Background()
	aml, err := NewAMLService(pool, nil)
	if err != nil {
		t.Fatalf("aml: %v", err)
	}
	resolver := HoldRoleResolver(officerRole)
	// Register every mandatory artifact type (cleanup removes them by
	// title prefix — artifact rows carry no account FK).
	tag := fmt.Sprintf("it-%d", time.Now().UnixNano())
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = pool.Exec(c,
			`DELETE FROM aml_program_artifacts WHERE title LIKE $1`, tag+"%")
	})
	fut := time.Now().UTC().Add(300 * 24 * time.Hour)
	for _, kind := range []string{ArtifactMSB, ArtifactRisk, ArtifactPolicy,
		ArtifactOfficer, ArtifactAnnualReview} {
		if _, err := aml.RegisterArtifact(ctx, 55, resolver, ArtifactInput{
			ArtifactType: kind, Title: tag + "-" + kind,
			Reference: "F107-" + kind, ReviewDueAt: &fut}); err != nil {
			t.Fatalf("register %s: %v", kind, err)
		}
	}
	st, err := aml.ProgramStatus(ctx)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if !st.Compliant {
		t.Fatalf("fully-registered program breaches: %v", st.Breaches)
	}
	// Same-type re-registration supersedes, never duplicates CURRENT.
	if _, err := aml.RegisterArtifact(ctx, 55, resolver, ArtifactInput{
		ArtifactType: ArtifactPolicy, Title: tag + "-POLICY-v2",
		Version: "2"}); err != nil {
		t.Fatalf("re-register: %v", err)
	}
	var curN int64
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM aml_program_artifacts
		WHERE artifact_type='POLICY' AND status='CURRENT'`).
		Scan(&curN); err != nil || curN != 1 {
		t.Fatalf("current policies=%d err=%v", curN, err)
	}
	// Role gate: auditors cannot register.
	if _, err := aml.RegisterArtifact(ctx, 55,
		HoldRoleResolver(auditorRole), ArtifactInput{
			ArtifactType: ArtifactPolicy, Title: "x"}); err == nil ||
		codeOf(t, err) != "UNAUTHORIZED_ROLE" {
		t.Fatalf("auditor register: got %v", err)
	}
	_ = aid
}
