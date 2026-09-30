// handlers_client_money_test.go — Phase-24 Tasks 24.3.17/24.3.18 handler
// tests: fail-closed nil seams (SERVICE_DEGRADED 503), admin identity,
// request validation and happy-path wire shapes.
package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"exchange/internal/admin"
	"exchange/internal/auth"
	"exchange/internal/backoffice"
	"exchange/pkg/decimal"
)

func cmReq(method, path, body string, withClaims bool) (*httptest.ResponseRecorder, *http.Request) {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if withClaims {
		req = req.WithContext(auth.WithClaims(req.Context(), *adminClaims()))
	}
	return httptest.NewRecorder(), req
}

// -- fakes ----------------------------------------------------------------

type fakeTreasuryAPI struct {
	funds       []backoffice.OwnFunds
	commitments []backoffice.Commitment
	ctrl        *backoffice.TreasuryControls
	err         error
	gotActor    admin.AdminActor
	gotCommit   backoffice.Commitment
}

func (f *fakeTreasuryAPI) ListOwnFunds(_ context.Context, a admin.AdminActor) ([]backoffice.OwnFunds, error) {
	f.gotActor = a
	return f.funds, f.err
}
func (f *fakeTreasuryAPI) ListCommitments(_ context.Context, a admin.AdminActor) ([]backoffice.Commitment, error) {
	f.gotActor = a
	return f.commitments, f.err
}
func (f *fakeTreasuryAPI) RecordCommitment(_ context.Context, a admin.AdminActor, c backoffice.Commitment) (*backoffice.Commitment, error) {
	f.gotActor = a
	f.gotCommit = c
	c.ID = 7
	return &c, f.err
}
func (f *fakeTreasuryAPI) Controls(_ context.Context, a admin.AdminActor) (*backoffice.TreasuryControls, error) {
	f.gotActor = a
	if f.ctrl == nil {
		return &backoffice.TreasuryControls{}, f.err
	}
	return f.ctrl, f.err
}

type fakeAssuranceAPI struct {
	audits   []backoffice.ClientMoneyAudit
	certs    []backoffice.SegregationCertification
	pack     *backoffice.EvidencePack
	err      error
	gotActor admin.AdminActor
	gotPack  int64
}

func (f *fakeAssuranceAPI) ListAudits(_ context.Context, a admin.AdminActor, _ string, _ int) ([]backoffice.ClientMoneyAudit, error) {
	f.gotActor = a
	return f.audits, f.err
}
func (f *fakeAssuranceAPI) CreateAudit(_ context.Context, a admin.AdminActor, in backoffice.ClientMoneyAudit) (*backoffice.ClientMoneyAudit, error) {
	f.gotActor = a
	in.ID = 3
	return &in, f.err
}
func (f *fakeAssuranceAPI) AssembleEvidencePack(_ context.Context, a admin.AdminActor, auditID int64) (*backoffice.EvidencePack, error) {
	f.gotActor = a
	f.gotPack = auditID
	if f.pack != nil {
		return f.pack, f.err
	}
	return &backoffice.EvidencePack{ID: 1, AuditID: auditID}, f.err
}
func (f *fakeAssuranceAPI) IssueCertification(_ context.Context, a admin.AdminActor, auditID, packID int64,
	statement string, signatories json.RawMessage, until time.Time) (*backoffice.SegregationCertification, error) {
	f.gotActor = a
	return &backoffice.SegregationCertification{
		ID: 9, AuditID: auditID, EvidencePackID: &packID,
		Statement: statement, Signatories: signatories,
		PackSHA256: "abc", Status: backoffice.CertIssued, PublishedUntil: until,
	}, f.err
}
func (f *fakeAssuranceAPI) ListCertifications(_ context.Context, a admin.AdminActor) ([]backoffice.SegregationCertification, error) {
	f.gotActor = a
	return f.certs, f.err
}

// -- nil seam → 503 ---------------------------------------------------------

func TestClientMoneyHandlers_NilServiceDegraded(t *testing.T) {
	cases := []struct {
		name string
		h    http.HandlerFunc
		m    string
		path string
		body string
	}{
		{"own funds", AdminTreasuryOwnFunds(nil), "GET", "/api/v1/admin/treasury/own-funds", ""},
		{"cc list", AdminContingentCapitalList(nil), "GET", "/api/v1/admin/treasury/contingent-capital", ""},
		{"cc create", AdminContingentCapitalCreate(nil), "POST", "/api/v1/admin/treasury/contingent-capital", `{}`},
		{"audits list", AdminClientMoneyAudits(nil), "GET", "/api/v1/admin/client-money/audits", ""},
		{"audits create", AdminClientMoneyAuditCreate(nil), "POST", "/api/v1/admin/client-money/audits", `{}`},
		{"cert list", AdminClientMoneyCertificationList(nil), "GET", "/api/v1/admin/client-money/certifications", ""},
		{"cert create", AdminClientMoneyCertificationCreate(nil), "POST", "/api/v1/admin/client-money/certifications", `{}`},
	}
	for _, c := range cases {
		rec, req := cmReq(c.m, c.path, c.body, true)
		c.h(rec, req)
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s: status=%d want 503", c.name, rec.Code)
		}
		var env struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &env)
		if env.Error != "SERVICE_DEGRADED" {
			t.Fatalf("%s: code=%q want SERVICE_DEGRADED", c.name, env.Error)
		}
	}
	// Path-parameter handler separately.
	rec, req := cmReq("POST", "/api/v1/admin/client-money/audits/5/evidence-pack", `{}`, true)
	req.SetPathValue("id", "5")
	AdminClientMoneyEvidencePack(nil)(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("evidence-pack: status=%d want 503", rec.Code)
	}
}

// -- unauthenticated → 401/UNAUTHORIZED -------------------------------------

func TestClientMoneyHandlers_RequireAdmin(t *testing.T) {
	ts := &fakeTreasuryAPI{}
	as := &fakeAssuranceAPI{}
	cases := []http.HandlerFunc{
		AdminTreasuryOwnFunds(ts), AdminContingentCapitalList(ts),
		AdminContingentCapitalCreate(ts), AdminClientMoneyAudits(as),
		AdminClientMoneyAuditCreate(as), AdminClientMoneyEvidencePack(as),
		AdminClientMoneyCertificationList(as), AdminClientMoneyCertificationCreate(as),
	}
	for i, h := range cases {
		rec, req := cmReq("GET", "/", "", false)
		h(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("handler %d: status=%d want 401", i, rec.Code)
		}
	}
}

// -- happy paths -------------------------------------------------------------

func TestTreasuryOwnFunds_HappyPath(t *testing.T) {
	ts := &fakeTreasuryAPI{funds: []backoffice.OwnFunds{
		{ID: 1, LineKind: "HOUSE_EQUITY", Currency: "USD",
			Balance: decimal.RequireFromString("1000"), ReconciliationStatus: "RECONCILED"},
	}}
	rec, req := cmReq("GET", "/api/v1/admin/treasury/own-funds", "", true)
	AdminTreasuryOwnFunds(ts)(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var env struct {
		OwnFunds []backoffice.OwnFunds `json:"own_funds"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil || len(env.OwnFunds) != 1 {
		t.Fatalf("decode: %v", err)
	}
}

func TestContingentCapitalCreate_DecimalWire(t *testing.T) {
	ts := &fakeTreasuryAPI{}
	rec, req := cmReq("POST", "/api/v1/admin/treasury/contingent-capital", `{
		"provider_name":"SponsorCo","commitment_kind":"SPONSOR","priority_seq":2,
		"committed_amount":"500000.25","currency":"USD",
		"activation_trigger":"FUND_DEPLETED","draw_window_days":5,
		"agreement_ref":"SPA-2026-01"}`, true)
	AdminContingentCapitalCreate(ts)(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !ts.gotCommit.CommittedAmount.Equal(decimal.RequireFromString("500000.25")) {
		t.Fatalf("committed_amount parsed=%s", ts.gotCommit.CommittedAmount)
	}
	// Bad decimal rejected.
	rec, req = cmReq("POST", "/api/v1/admin/treasury/contingent-capital",
		`{"committed_amount":"not-a-number"}`, true)
	AdminContingentCapitalCreate(ts)(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad amount status=%d want 400", rec.Code)
	}
}

func TestClientMoneyAuditCreate_HappyPath(t *testing.T) {
	as := &fakeAssuranceAPI{}
	rec, req := cmReq("POST", "/api/v1/admin/client-money/audits", `{
		"engagement_year":2026,"auditor_firm":"Assurance LLP",
		"scope":"CASS client-money","period_start":"2026-01-01","period_end":"2026-12-31"}`, true)
	AdminClientMoneyAuditCreate(as)(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	// Bad dates rejected.
	rec, req = cmReq("POST", "/api/v1/admin/client-money/audits",
		`{"period_start":"garbage","period_end":"2026-01-01"}`, true)
	AdminClientMoneyAuditCreate(as)(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad dates status=%d want 400", rec.Code)
	}
}

func TestEvidencePack_PathID(t *testing.T) {
	as := &fakeAssuranceAPI{}
	rec, req := cmReq("POST", "/api/v1/admin/client-money/audits/42/evidence-pack", `{}`, true)
	req.SetPathValue("id", "42")
	AdminClientMoneyEvidencePack(as)(rec, req)
	if rec.Code != http.StatusCreated || as.gotPack != 42 {
		t.Fatalf("status=%d gotPack=%d", rec.Code, as.gotPack)
	}
	// Non-integer id → 400.
	rec, req = cmReq("POST", "/api/v1/admin/client-money/audits/x/evidence-pack", `{}`, true)
	req.SetPathValue("id", "x")
	AdminClientMoneyEvidencePack(as)(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad id status=%d want 400", rec.Code)
	}
}

func TestCertificationCreate_ApproverPropagated(t *testing.T) {
	as := &fakeAssuranceAPI{}
	rec, req := cmReq("POST", "/api/v1/admin/client-money/certifications", `{
		"audit_id":3,"evidence_pack_id":1,"statement":"segregation effective",
		"signatories":[{"name":"A"},{"name":"B"}],
		"published_until":"2027-10-01","approver_id":77}`, true)
	AdminClientMoneyCertificationCreate(as)(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if as.gotActor.ApproverID != 77 || as.gotActor.UserID != 9001 {
		t.Fatalf("actor=%+v — approver_id not propagated", as.gotActor)
	}
	// Missing published_until → 400.
	rec, req = cmReq("POST", "/api/v1/admin/client-money/certifications",
		`{"audit_id":3,"signatories":[{"n":"A"}]}`, true)
	AdminClientMoneyCertificationCreate(as)(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("missing published_until status=%d want 400", rec.Code)
	}
}
