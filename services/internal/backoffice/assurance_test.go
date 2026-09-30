// assurance_test.go — DoD coverage for Phase-24 Task 24.3.18: the audit
// engagement register, system-assembled evidence packs, hashed segregation
// certifications gating the production release, the time-bounded dual-
// controlled EXTERNAL_AUDITOR grant and the independence record.
package backoffice

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"exchange/internal/ledger"
	"exchange/pkg/decimal"
	excerrors "exchange/pkg/errors"
)

func newAssuranceSvc(t *testing.T, st *cmStore) (*AssuranceService, *cmClock) {
	t.Helper()
	clk := &cmClock{t: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)}
	svc, err := NewAssuranceService(AssuranceDeps{
		Store: st, Resolver: cmRoles, Now: clk.now,
	})
	if err != nil {
		t.Fatalf("NewAssuranceService: %v", err)
	}
	return svc, clk
}

func cmSeedAudit(t *testing.T, svc *AssuranceService, st *cmStore) *ClientMoneyAudit {
	t.Helper()
	a, err := svc.CreateAudit(context.Background(), cmActor(uidCO), ClientMoneyAudit{
		EngagementYear: 2026, AuditorFirm: "Assurance LLP", Scope: "CASS client-money",
		PeriodStart: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		PeriodEnd:   time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("create audit: %v", err)
	}
	return a
}

func TestAudit_RegisterLifecycleAndLogs(t *testing.T) {
	st := newCMStore()
	svc, _ := newAssuranceSvc(t, st)
	ctx := context.Background()

	a := cmSeedAudit(t, svc, st)
	if a.Status != AuditScheduled {
		t.Fatalf("status=%s want SCHEDULED", a.Status)
	}
	// Invalid transition (skip) refused.
	if _, err := svc.TransitionAudit(ctx, cmActor(uidCO), a.ID, AuditIssued); err == nil {
		t.Fatal("SCHEDULED→ISSUED must be refused")
	}
	for _, to := range []string{AuditFieldwork, AuditDraft} {
		got, err := svc.TransitionAudit(ctx, cmActor(uidCO), a.ID, to)
		if err != nil || got.Status != to {
			t.Fatalf("transition %s: %v", to, err)
		}
	}
	// Evidence-request log + findings/remediation tickets.
	a2, err := svc.LogEvidenceRequest(ctx, cmActor(uidCO), a.ID, "segregation calcs for Q3")
	if err != nil {
		t.Fatal(err)
	}
	var reqs []map[string]any
	if err := json.Unmarshal(a2.EvidenceRequests, &reqs); err != nil || len(reqs) != 1 {
		t.Fatalf("evidence requests: %v", err)
	}
	a3, err := svc.RecordFinding(ctx, cmActor(uidFO), a.ID, "one late top-up", "JIRA-77")
	if err != nil {
		t.Fatal(err)
	}
	var findings, tickets []map[string]any
	_ = json.Unmarshal(a3.Findings, &findings)
	_ = json.Unmarshal(a3.RemediationTickets, &tickets)
	if len(findings) != 1 || len(tickets) != 1 {
		t.Fatalf("findings=%d tickets=%d", len(findings), len(tickets))
	}
	// Independence record (annual review stamp).
	a4, err := svc.RecordIndependence(ctx, cmActor(uidCO), a.ID,
		"no contract with the venue's ledger operator; reviewed 2026")
	if err != nil || !a4.IndependenceConfirmed || a4.IndependenceReviewedAt == nil {
		t.Fatalf("independence: %v %+v", err, a4)
	}
}

func TestEvidencePack_SystemAssembledAndHashed(t *testing.T) {
	st := newCMStore()
	svc, _ := newAssuranceSvc(t, st)
	ctx := context.Background()
	a := cmSeedAudit(t, svc, st)

	// Not during SCHEDULED.
	if _, err := svc.AssembleEvidencePack(ctx, cmActor(uidCO), a.ID); err == nil {
		t.Fatal("pack assembly during SCHEDULED must fail")
	}
	if _, err := svc.TransitionAudit(ctx, cmActor(uidCO), a.ID, AuditFieldwork); err != nil {
		t.Fatal(err)
	}
	// Seed system-of-record sources the pack must carry.
	st.recons[999] = &Reconciliation{ID: 999,
		ReconDate: time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC), Currency: "USD",
		Requirement: decimal.RequireFromString("10"), Resource: decimal.RequireFromString("10"),
		Status: ReconBalanced, ExternalStatus: ExtMatched}
	st.roots = []PoRRoot{{Date: time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC), MerkleRoot: "abc"}}
	st.glLines = []GLLine{{JournalEntryID: 5, AccountCode: ledger.ClientMoneySegregated("USD"),
		Debit: decimal.RequireFromString("10"), Currency: "USD",
		PostedAt: time.Date(2026, 3, 1, 1, 0, 0, 0, time.UTC)}}
	st.rems[7] = &Remediation{ID: 7, BreakID: 1, Tier: TierHouse, Action: ActHouseTopup,
		Currency: "USD", Amount: decimal.RequireFromString("4"), Status: RemExecuted,
		CreatedAt: time.Date(2026, 3, 2, 0, 0, 0, 0, time.UTC)}

	pack, err := svc.AssembleEvidencePack(ctx, cmActor(uidCO), a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if pack.PackSHA256 == "" || len(pack.Pack) == 0 {
		t.Fatal("pack must carry content + sha256")
	}
	var body map[string]any
	if err := json.Unmarshal(pack.Pack, &body); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"reconciliations", "topup_log", "gl_lines", "por_roots", "breaks", "bank_reconciliations", "stress_runs"} {
		if _, ok := body[k]; !ok {
			t.Fatalf("evidence pack missing %s", k)
		}
	}
	// Same source state → deterministic hash.
	pack2, err := svc.AssembleEvidencePack(ctx, cmActor(uidCO), a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if pack.PackSHA256 != pack2.PackSHA256 {
		t.Fatal("pack hash not deterministic over identical sources")
	}
}

func TestCertification_DualControlAndReleaseGate(t *testing.T) {
	st := newCMStore()
	svc, clk := newAssuranceSvc(t, st)
	ctx := context.Background()
	day := time.Date(2026, 6, 15, 0, 0, 0, 0, time.UTC)

	// Release gate fails closed with no certification at all.
	if err := svc.AssertReleaseGate(ctx, day); err == nil {
		t.Fatal("release gate must be blocked without certification")
	}

	a := cmSeedAudit(t, svc, st)
	if _, err := svc.TransitionAudit(ctx, cmActor(uidCO), a.ID, AuditFieldwork); err != nil {
		t.Fatal(err)
	}
	pack, err := svc.AssembleEvidencePack(ctx, cmActor(uidCO), a.ID)
	if err != nil {
		t.Fatal(err)
	}
	until := clk.t.Add(365 * 24 * time.Hour)

	// Dual control: missing approver / same principal refused.
	if _, err := svc.IssueCertification(ctx, cmActor(uidCO), a.ID, pack.ID,
		"client money segregated per CASS", json.RawMessage(`["A","B"]`), until); err == nil {
		t.Fatal("issuance without approver must fail")
	}
	if _, err := svc.IssueCertification(ctx, cmAct2(uidCO, uidCO), a.ID, pack.ID,
		"x", json.RawMessage(`["A"]`), until); err == nil {
		t.Fatal("same-principal issuance must fail")
	}
	cert, err := svc.IssueCertification(ctx, cmAct2(uidCO, uidFO), a.ID, pack.ID,
		"segregation effective for the period", json.RawMessage(`["A","B"]`), until)
	if err != nil {
		t.Fatal(err)
	}
	if cert.PackSHA256 != pack.PackSHA256 {
		t.Fatal("certification must bind to the evidence-pack hash")
	}
	if cert.Status != CertIssued {
		t.Fatalf("cert status=%s", cert.Status)
	}
	// Engagement flipped to ISSUED.
	got, _ := st.AuditByID(ctx, a.ID)
	if got.Status != AuditIssued {
		t.Fatalf("audit status=%s want ISSUED", got.Status)
	}
	// Gate opens within published_until.
	if err := svc.AssertReleaseGate(ctx, day); err != nil {
		t.Fatalf("gate blocked despite valid certification: %v", err)
	}
	// Expired certification re-blocks the gate.
	clk.t = until.Add(time.Hour)
	if err := svc.AssertReleaseGate(ctx, day); err == nil {
		t.Fatal("expired certification must re-block the release gate")
	}
}

func TestExternalAuditor_TimeBoundedDualControlledAudited(t *testing.T) {
	st := newCMStore()
	svc, clk := newAssuranceSvc(t, st)
	ctx := context.Background()
	a := cmSeedAudit(t, svc, st)

	until := clk.t.Add(48 * time.Hour)
	// Dual control on the grant.
	if _, err := svc.GrantAuditorAccess(ctx, cmActor(uidCO), a.ID, "auditor@firm.example", until); err == nil {
		t.Fatal("grant without approver must fail")
	}
	if _, err := svc.GrantAuditorAccess(ctx, cmAct2(uidCO, uidCO), a.ID, "auditor@firm.example", until); err == nil {
		t.Fatal("same-principal grant must fail")
	}
	g, err := svc.GrantAuditorAccess(ctx, cmAct2(uidCO, uidFO), a.ID, "auditor@firm.example", until)
	if err != nil || g.Status != GrantActive {
		t.Fatalf("grant: %v %+v", err, g)
	}
	// Every read appends to the access log (separate audit trail).
	g2, err := svc.AssertAuditorAccess(ctx, g.ID, "auditor@firm.example", "evidence_pack/1")
	if err != nil {
		t.Fatal(err)
	}
	var log []map[string]any
	if err := json.Unmarshal(g2.AccessLog, &log); err != nil || len(log) != 1 {
		t.Fatalf("access log: %v", err)
	}
	// Wrong auditor ref on the grant → refused.
	if _, err := svc.AssertAuditorAccess(ctx, g.ID, "other@firm.example", "x"); err == nil {
		t.Fatal("wrong auditor ref must be refused")
	}
	// Time-bounded: past valid_until the grant flips EXPIRED and refuses.
	clk.t = until.Add(time.Hour)
	if _, err := svc.AssertAuditorAccess(ctx, g.ID, "auditor@firm.example", "x"); err == nil {
		t.Fatal("expired grant must refuse")
	}
	g3, _ := st.AuditorGrantByID(ctx, g.ID)
	if g3.Status != GrantExpired {
		t.Fatalf("grant status=%s want EXPIRED", g3.Status)
	}
	// Revocation blocks further access.
	clk.t = clk.t.Add(-2 * time.Hour) // back inside the window
	g4, err := svc.GrantAuditorAccess(ctx, cmAct2(uidCO, uidFO), a.ID, "auditor@firm.example", until)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.RevokeAuditorAccess(ctx, cmActor(uidFO), g4.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.AssertAuditorAccess(ctx, g4.ID, "auditor@firm.example", "x"); err == nil {
		t.Fatal("revoked grant must refuse")
	}
	_ = clk
}

func TestAssurance_NilDepsFailClosed(t *testing.T) {
	if _, err := NewAssuranceService(AssuranceDeps{}); err == nil {
		t.Fatal("nil store must fail")
	}
	if _, err := NewAssuranceService(AssuranceDeps{Store: newCMStore()}); err == nil {
		t.Fatal("nil resolver must fail")
	}
	// Role enforcement on the write surface.
	st := newCMStore()
	svc, _ := newAssuranceSvc(t, st)
	if _, err := svc.CreateAudit(context.Background(), cmActor(uidSupport), ClientMoneyAudit{
		EngagementYear: 2026, AuditorFirm: "X", Scope: "y",
		PeriodStart: time.Now(), PeriodEnd: time.Now().Add(time.Hour),
	}); err == nil || excerrors.CodeOf(err) != "UNAUTHORIZED_ROLE" {
		t.Fatalf("support role must be refused: %v", err)
	}
}
