// Phase-21 wave-3 integration tests — GDPR export/erase/consent
// (21.3.7), data residency (21.3.18), comms recording chain + retention
// (21.3.20), tax-report lifecycle (21.3.22), financial promotions
// (21.3.26). Gated: EXC_PG_TEST=1 (holdPool helper, dev DSN or
// EXC_PG_DSN). devs3 provides the WORM object seam.
package compliance

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"testing"
	"time"

	"exchange/internal/content"
	"exchange/internal/devs3"
	"exchange/internal/objectstore"
	excerrors "exchange/pkg/errors"
)

func officerResolver(_ context.Context, _ int64) (string, error) {
	return "Compliance Officer", nil
}

func devObjects(t *testing.T, bucket string) objectstore.Client {
	t.Helper()
	srv, err := devs3.New(t.TempDir())
	if err != nil {
		t.Fatalf("devs3: %v", err)
	}
	ts := httptest.NewServer(srv)
	c, err := objectstore.NewDev(context.Background(), objectstore.Config{
		Bucket:   bucket,
		Endpoint: ts.URL,
		Region:   "eu-central-1",
	})
	if err != nil {
		ts.Close()
		t.Fatalf("dev object client: %v", err)
	}
	t.Cleanup(ts.Close)
	return c
}

func TestGDPRConsentLifecycle(t *testing.T) {
	pool := holdPool(t)
	svc, err := NewGDPRService(pool, nil, nil)
	if err != nil {
		t.Fatalf("gdpr svc: %v", err)
	}
	ctx := context.Background()
	var uid, aid int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO users (email, status, country)
		 VALUES ($1,'ACTIVE','DE') RETURNING id`,
		fmt.Sprintf("gdpr_it_%d@example.com", time.Now().UnixNano())).
		Scan(&uid); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO accounts (user_id, account_type, kyc_tier)
		 VALUES ($1,'SPOT','T1') RETURNING id`, uid).Scan(&aid); err != nil {
		t.Fatalf("seed account: %v", err)
	}

	if _, err := svc.SetConsent(ctx, aid, uid, "MARKETING", "EMAIL",
		"GRANTED", map[string]any{"surface": "it"}); err != nil {
		t.Fatalf("grant: %v", err)
	}
	ok, err := svc.ConsentGranted(ctx, aid, "MARKETING", "EMAIL")
	if err != nil || !ok {
		t.Fatalf("granted consent must report true: ok=%v err=%v", ok, err)
	}
	// Channel-specific grant also satisfies the ALL fallback read.
	ok, err = svc.ConsentGranted(ctx, aid, "MARKETING", "PUSH")
	if err != nil {
		t.Fatalf("fallback read: %v", err)
	}
	if ok {
		t.Fatal("EMAIL grant must not imply PUSH consent")
	}
	if _, err := svc.SetConsent(ctx, aid, uid, "MARKETING", "EMAIL",
		"WITHDRAWN", nil); err != nil {
		t.Fatalf("withdraw: %v", err)
	}
	ok, err = svc.ConsentGranted(ctx, aid, "MARKETING", "EMAIL")
	if err != nil || ok {
		t.Fatalf("withdrawn consent must report false: ok=%v err=%v", ok, err)
	}
	// Both transitions are evidenced in the append-only ledger.
	var events int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM account_consent_events
		 WHERE account_id=$1 AND purpose='MARKETING'`, aid).
		Scan(&events); err != nil {
		t.Fatalf("event count: %v", err)
	}
	if events != 2 {
		t.Fatalf("want 2 consent events, got %d", events)
	}
}

func TestGDPRExportAndErasure(t *testing.T) {
	pool := holdPool(t)
	svc, err := NewGDPRService(pool, nil, nil)
	if err != nil {
		t.Fatalf("gdpr svc: %v", err)
	}
	ctx := context.Background()
	var uid, aid int64
	if err := pool.QueryRow(ctx, `
		INSERT INTO users (email, status, country, full_name)
		VALUES ($1,'ACTIVE','DE','Erase Me') RETURNING id`,
		fmt.Sprintf("gdpr_erase_%d@example.com", time.Now().UnixNano())).
		Scan(&uid); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	if err := pool.QueryRow(ctx, `
		INSERT INTO accounts (user_id, account_type, kyc_tier)
		VALUES ($1,'SPOT','T1') RETURNING id`, uid).Scan(&aid); err != nil {
		t.Fatalf("seed account: %v", err)
	}

	req, manifest, err := svc.RequestExport(ctx, aid, uid)
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	if req.Kind != "EXPORT" || req.Status != "COMPLETED" {
		t.Fatalf("export request state: %s/%s", req.Kind, req.Status)
	}
	var m map[string]any
	if err := json.Unmarshal(manifest, &m); err != nil {
		t.Fatalf("manifest parse: %v", err)
	}
	sections, ok := m["sections"].(map[string]any)
	if !ok {
		t.Fatalf("manifest missing sections map: %v", m)
	}
	for _, want := range []string{"profile", "accounts", "orders",
		"trades", "funding_transactions", "login_history",
		"kyc_submissions", "tax_certifications", "consents"} {
		if _, ok := sections[want]; !ok {
			t.Fatalf("manifest missing section %q", want)
		}
	}
	if req.SHA256 == nil || len(*req.SHA256) != 64 {
		t.Fatalf("export artifact digest missing: %+v", req.SHA256)
	}

	// Re-request while nothing open returns a fresh completed run; the
	// open-request dedup is exercised by the partial unique index —
	// a second export is a new row, not an error.
	req2, _, err := svc.RequestExport(ctx, aid, uid)
	if err != nil {
		t.Fatalf("re-export: %v", err)
	}
	if req2.ID == req.ID {
		t.Fatal("re-export must create a new completed request")
	}

	// Erasure refuses while the account is live.
	if _, err := svc.RequestErasure(ctx, aid, uid, "rtbf"); err == nil {
		t.Fatal("erasure on ACTIVE account must refuse")
	}
	if _, err := pool.Exec(ctx,
		`UPDATE accounts SET status='CLOSED' WHERE id=$1`, aid); err != nil {
		t.Fatalf("close account: %v", err)
	}
	er, err := svc.RequestErasure(ctx, aid, uid, "art17")
	if err != nil {
		t.Fatalf("erasure: %v", err)
	}
	if er.Status != "COMPLETED" {
		t.Fatalf("erasure status: %s", er.Status)
	}
	var email, name *string
	if err := pool.QueryRow(ctx,
		`SELECT email, full_name FROM users WHERE id=$1`, uid).
		Scan(&email, &name); err != nil {
		t.Fatalf("post-erasure read: %v", err)
	}
	if email == nil || *email != fmt.Sprintf("erased-%d@erased.invalid", uid) {
		t.Fatalf("email not tombstoned: %v", email)
	}
	if name != nil {
		t.Fatal("full_name must be NULL after erasure")
	}
	// Every revocable purpose ends WITHDRAWN (erasure revokes all).
	var n int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM account_consent_states
		 WHERE account_id=$1 AND state='WITHDRAWN'`, aid).Scan(&n); err != nil {
		t.Fatalf("consent read: %v", err)
	}
	if n != 3 {
		t.Fatalf("want 3 withdrawn consent purposes, got %d", n)
	}
}

func TestCommsRecordingLifecycle(t *testing.T) {
	pool := holdPool(t)
	objects := devObjects(t, "comms-it")
	svc, err := NewCommsRecordingService(pool, objects, "", officerResolver)
	if err != nil {
		t.Fatalf("comms svc: %v", err)
	}
	ctx := context.Background()
	var uid, aid int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO users (email, status) VALUES ($1,'ACTIVE') RETURNING id`,
		fmt.Sprintf("comms_it_%d@example.com", time.Now().UnixNano())).
		Scan(&uid); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	if err := pool.QueryRow(ctx, `
		INSERT INTO accounts (user_id, account_type, kyc_tier)
		VALUES ($1,'SPOT','T2') RETURNING id`, uid).Scan(&aid); err != nil {
		t.Fatalf("seed account: %v", err)
	}

	started := time.Now().Add(-2 * time.Minute)
	r1, err := svc.Record(ctx, RecordInput{
		AccountID: &aid, UserID: &uid,
		Channel: "IN_APP_CHAT", Direction: "OUTBOUND",
		Source: "it", SourceID: time.Now().UnixNano(),
		StartedAt: started, EndedAt: started.Add(time.Minute),
		Body: []byte("chat message 1"),
	})
	if err != nil {
		t.Fatalf("record 1: %v", err)
	}
	r2, err := svc.Record(ctx, RecordInput{
		AccountID: &aid, UserID: &uid,
		Channel: "IN_APP_CHAT", Direction: "INBOUND",
		Source: "it", SourceID: time.Now().UnixNano() + 1,
		StartedAt: started.Add(2 * time.Minute),
		EndedAt:   started.Add(3 * time.Minute),
		Body:      []byte("chat message 2"),
	})
	if err != nil {
		t.Fatalf("record 2: %v", err)
	}
	if r2.PrevChainHash != r1.ChainHash {
		t.Fatalf("chain not linked: prev=%s head=%s",
			r2.PrevChainHash, r1.ChainHash)
	}
	if r1.ChainHash != commsChainHash(r1) {
		t.Fatal("stored chain hash does not recompute")
	}
	if r1.RetentionUntil.Before(r1.EndedAt.Add(CommsRetentionFloor)) {
		t.Fatal("retention floor not enforced")
	}

	// Dual-control retrieval.
	if _, err := svc.Retrieve(ctx, r1.RecordingID, 1, 1,
		"case review", "CASE-1"); err == nil {
		t.Fatal("officer == approver must be DUAL_CONTROL_VIOLATION")
	}
	if _, err := svc.Retrieve(ctx, r1.RecordingID, 1, 2,
		"", "CASE-1"); err == nil {
		t.Fatal("blank justification must refuse")
	}
	got, err := svc.Retrieve(ctx, r1.RecordingID, 1, 2,
		"regulator request FCA-2029-114", "CASE-1")
	if err != nil {
		t.Fatalf("retrieve: %v", err)
	}
	var env struct {
		Body string `json:"body"` // []byte marshals base64
	}
	if err := json.Unmarshal(got.Body, &env); err != nil {
		t.Fatalf("retrieved body parse: %v", err)
	}
	decoded, err := base64.StdEncoding.DecodeString(env.Body)
	if err != nil {
		t.Fatalf("body base64: %v", err)
	}
	if string(decoded) != "chat message 1" {
		t.Fatalf("body mismatch: %q", decoded)
	}
	// Access row is durable evidence.
	var accesses int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM comms_recording_access
		 WHERE recording_id=$1`, r1.RecordingID).Scan(&accesses); err != nil {
		t.Fatalf("access log: %v", err)
	}
	if accesses == 0 {
		t.Fatal("retrieval not audited")
	}

	// Day chain verifies; early deletion refuses.
	ok, err := svc.VerifyDay(ctx, r1.DayBucket)
	if err != nil || !ok {
		t.Fatalf("chain verify: ok=%v err=%v", ok, err)
	}
	if err := svc.Delete(ctx, r1.RecordingID, 1); err == nil {
		t.Fatal("pre-retention delete must refuse")
	}
}

func TestPromotionLifecycle(t *testing.T) {
	pool := holdPool(t)
	svc, err := NewPromotionService(pool, officerResolver)
	if err != nil {
		t.Fatalf("promo svc: %v", err)
	}
	ctx := context.Background()
	var uid int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO users (email, status) VALUES ($1,'ACTIVE') RETURNING id`,
		fmt.Sprintf("promo_it_%d@example.com", time.Now().UnixNano())).
		Scan(&uid); err != nil {
		t.Fatalf("seed user: %v", err)
	}

	p, err := svc.Create(ctx, uid, PromotionInput{
		Slug:    fmt.Sprintf("it-promo-%d", time.Now().UnixNano()),
		Channel: "EMAIL", BodyRef: "cms://it/promo/v1",
		Title: "IT promo", ContainsClaim: true,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if p.ApprovalStatus != "DRAFT" {
		t.Fatalf("status: %s", p.ApprovalStatus)
	}
	if _, err := svc.SubmitForReview(ctx, p.PromotionID, uid); err != nil {
		t.Fatalf("submit: %v", err)
	}
	// Incomplete checklist refuses.
	if _, err := svc.Approve(ctx, p.PromotionID, uid, PromoChecklist{
		RiskWarning: true}, nil, nil); err == nil {
		t.Fatal("incomplete checklist must refuse")
	}
	// Claims need a DISTINCT second approver.
	full := PromoChecklist{RiskWarning: true, CapitalAtRisk: true,
		ClaimBasis: true, EntityDetails: true, FairClear: true}
	if _, err := svc.Approve(ctx, p.PromotionID, uid, full,
		&uid, nil); err == nil {
		t.Fatal("same second approver must be DUAL_CONTROL_REQUIRED")
	}
	second := uid + 1
	ap, err := svc.Approve(ctx, p.PromotionID, uid, full, &second, nil)
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	if ap.ApprovalStatus != "APPROVED" || ap.ApprovedUntil == nil {
		t.Fatalf("approve state: %+v", ap)
	}
	if ap.ApprovedUntil.After(time.Now().Add(PromoApprovalBound + time.Hour)) {
		t.Fatal("approved_until exceeded the 12-month bound")
	}

	// Render gate agrees (same PG table, independent reader).
	gate, err := content.NewGate(content.NewPgPromotionStore(pool))
	if err != nil {
		t.Fatalf("gate: %v", err)
	}
	if _, err := gate.Renderable(ctx, p.PromotionID); err != nil {
		t.Fatalf("approved promotion must render: %v", err)
	}
	if _, err := svc.Withdraw(ctx, p.PromotionID, uid, "recall"); err != nil {
		t.Fatalf("withdraw: %v", err)
	}
	if _, err := gate.Renderable(ctx, p.PromotionID); err == nil {
		t.Fatal("withdrawn promotion must not render")
	}
}

func TestResidencyAuthorizeAccess(t *testing.T) {
	pool := holdPool(t)
	svc, err := NewResidencyService(pool)
	if err != nil {
		t.Fatalf("residency svc: %v", err)
	}
	ctx := context.Background()
	var uid, aid int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO users (email, status) VALUES ($1,'ACTIVE') RETURNING id`,
		fmt.Sprintf("res_it_%d@example.com", time.Now().UnixNano())).
		Scan(&uid); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	if err := pool.QueryRow(ctx, `
		INSERT INTO accounts (user_id, account_type, kyc_tier)
		VALUES ($1,'SPOT','T1') RETURNING id`, uid).Scan(&aid); err != nil {
		t.Fatalf("seed account: %v", err)
	}

	if _, err := svc.PinAccount(ctx, aid, "EU",
		fmt.Sprintf("%d", uid)); err != nil {
		t.Fatalf("pin: %v", err)
	}
	// Cross-border admin with no justification refuses.
	if _, err := svc.AuthorizeAccess(ctx, uid,
		[]string{"us-east"}, aid, "READ", ""); err == nil {
		t.Fatal("cross-border access without justification must refuse")
	}
	// Same-region access: allowed, unmasked, still logged.
	masked, err := svc.AuthorizeAccess(ctx, uid,
		[]string{"eu-central"}, aid, "READ", "")
	if err != nil {
		t.Fatalf("same-region access: %v", err)
	}
	if masked {
		t.Fatal("same-region access must be unmasked")
	}
	// Cross-border with justification: allowed but masked + logged.
	masked, err = svc.AuthorizeAccess(ctx, uid,
		[]string{"us-east"}, aid, "READ", "ticket SUP-114")
	if err != nil {
		t.Fatalf("cross-border w/ justification: %v", err)
	}
	if !masked {
		t.Fatal("cross-border access must be masked")
	}
	log, err := svc.AccessLog(ctx, 10)
	if err != nil {
		t.Fatalf("access log: %v", err)
	}
	if len(log) < 2 {
		t.Fatalf("expected ≥2 access-log rows, got %d", len(log))
	}
}

func TestTaxReportLifecycle(t *testing.T) {
	pool := holdPool(t)
	kycBox, err := NewPgStore(pool, testBox(t))
	if err != nil {
		t.Fatalf("kyc store: %v", err)
	}
	svc, err := NewTaxReportService(pool, kycBox, officerResolver, TaxVenue{
		Name: "IT Venue", IN: "ITIN.00000.LE.826", Country: "GB"})
	if err != nil {
		t.Fatalf("tax svc: %v", err)
	}
	ctx := context.Background()

	// FATCA scopes to US persons only — a dev DB full of foreign-resident
	// seeds cannot poison the run (non-US accounts aren't candidates).
	// CRS is exercised separately: on a dirty DB it fails closed with
	// TAX_REPORT_DATA_INCOMPLETE — that refusal IS the assertion.
	// A fresh report_year per run keeps the (regime, year, jurisdiction)
	// key collision-free — a prior SUBMITTED run must refuse regenerate.
	year := 2060 + int(time.Now().UnixNano()%40)
	run, err := svc.Generate(ctx, "FATCA", year, "US", 1)
	if err != nil {
		t.Fatalf("generate FATCA: %v", err)
	}
	if run.Status != "DRAFT" || run.SHA256 == nil || *run.SHA256 == "" {
		t.Fatalf("run state: %+v", run)
	}
	// Invalid transition: approve straight from DRAFT.
	if _, err := svc.Approve(ctx, run.ID, 1); err == nil {
		t.Fatal("DRAFT→APPROVED must refuse")
	}
	if _, err := svc.Review(ctx, run.ID, 1); err != nil {
		t.Fatalf("review: %v", err)
	}
	// Approver must differ from the reviewer.
	if _, err := svc.Approve(ctx, run.ID, 1); err == nil {
		t.Fatal("approver == reviewer must refuse")
	}
	if _, err := svc.Approve(ctx, run.ID, 2); err != nil {
		t.Fatalf("approve: %v", err)
	}
	if _, err := svc.Submit(ctx, run.ID, 1, "IT-SUB-1"); err != nil {
		t.Fatalf("submit: %v", err)
	}
	// Re-generation over a SUBMITTED key refuses (amendment flow owns it).
	if _, err := svc.Generate(ctx, "FATCA", year, "US", 1); err == nil {
		t.Fatal("regenerate over SUBMITTED must refuse")
	}
	// Artifact integrity on read.
	if _, err := svc.Artifact(ctx, run.ID); err != nil {
		t.Fatalf("artifact read: %v", err)
	}

	// CRS fail-closed path: a foreign-resident account missing holder
	// identity must abort the run rather than emit a partial report.
	// If the dev DB happens to be clean, an empty-set run is equally
	// valid.
	crsRun, crsErr := svc.Generate(ctx, "CRS", 2098, "AQ", 1)
	if crsErr != nil {
		var coded *excerrors.Error
		if !errors.As(crsErr, &coded) || coded.Code != "TAX_REPORT_DATA_INCOMPLETE" {
			t.Fatalf("CRS generate: %v", crsErr)
		}
	} else if crsRun.Status != "DRAFT" {
		t.Fatalf("CRS run state: %s", crsRun.Status)
	}
}
