// Phase-24 backoffice handler tests — fail-closed nil seams
// (SERVICE_DEGRADED 503), admin identity, request validation, and the
// happy-path wire shapes.
package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"exchange/internal/auth"
	"exchange/internal/backoffice"
	"exchange/pkg/decimal"
	excerrors "exchange/pkg/errors"
)

func boReq(method, path, body string, withClaims bool) (*httptest.ResponseRecorder, *http.Request) {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if withClaims {
		req = req.WithContext(auth.WithClaims(req.Context(), *adminClaims()))
	}
	return httptest.NewRecorder(), req
}

// ---------------------------------------------------------------------------
// Fail-closed: nil service → 503 SERVICE_DEGRADED, never empty success.
// ---------------------------------------------------------------------------

func TestBackofficeHandlers_NilServiceDegraded(t *testing.T) {
	type tc struct {
		name string
		h    http.HandlerFunc
		m    string
		path string
		body string
	}
	cases := []tc{
		{"nostro create", AdminNostroAccountCreate(nil), "POST",
			"/api/v1/admin/nostro-accounts", `{"currency":"USD"}`},
		{"nostro list", AdminNostroAccountList(nil), "GET",
			"/api/v1/admin/nostro-accounts", ""},
		{"recon report", AdminNostroReconciliation(nil), "GET",
			"/api/v1/admin/nostro-reconciliation", ""},
		{"recon run", AdminNostroReconRun(nil), "POST",
			"/api/v1/admin/nostro-reconciliation/run", `{}`},
		{"swift list", AdminSwiftMessages(nil), "GET",
			"/api/v1/admin/swift-messages", ""},
		{"settlement confirm", AdminSettlementConfirmation(nil), "POST",
			"/api/v1/admin/settlement-confirmations", `{}`},
		{"compliance report", AdminComplianceReport(nil), "GET",
			"/api/v1/admin/compliance-report?type=EMIR", ""},
	}
	for _, c := range cases {
		rec, req := boReq(c.m, c.path, c.body, true)
		c.h(rec, req)
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s: status=%d want 503 (fail closed)", c.name, rec.Code)
		}
		var env struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &env)
		if env.Error != "SERVICE_DEGRADED" {
			t.Fatalf("%s: code=%q want SERVICE_DEGRADED", c.name, env.Error)
		}
	}
}

// Break resolve needs the id path value — test separately.
func TestBackofficeBreakResolve_NilServiceDegraded(t *testing.T) {
	rec, req := boReq("POST",
		"/api/v1/admin/nostro-reconciliation/breaks/1/resolve",
		`{"action":"RESOLVE","notes":"ok"}`, true)
	req.SetPathValue("id", "1")
	AdminNostroBreakResolve(nil)(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d want 503", rec.Code)
	}
}

// ---------------------------------------------------------------------------
// Identity gating — anonymous callers never reach the service.
// ---------------------------------------------------------------------------

func TestBackofficeHandlers_RequireIdentity(t *testing.T) {
	rec, req := boReq("GET", "/api/v1/admin/nostro-accounts", "", false)
	AdminNostroAccountList(stubAccounts{})(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("anon status=%d want 401", rec.Code)
	}
}

// ---------------------------------------------------------------------------
// Fakes.
// ---------------------------------------------------------------------------

type stubAccounts struct {
	created *backoffice.NostroAccount
}

func (s stubAccounts) CreateAccount(_ context.Context, in backoffice.CreateAccountInput) (*backoffice.NostroAccount, error) {
	return &backoffice.NostroAccount{
		ID: 1, Currency: in.Currency, BankName: in.BankName,
		Role: in.Role, Status: "ACTIVE",
		Balance: decimal.Zero}, nil
}

func (s stubAccounts) ListAccounts(_ context.Context, f backoffice.AccountFilter) ([]backoffice.NostroAccount, error) {
	return []backoffice.NostroAccount{}, nil
}

type stubRecon struct{}

func (stubRecon) Report(_ context.Context, d time.Time) (*backoffice.NostroReconReport, error) {
	return &backoffice.NostroReconReport{Date: d}, nil
}
func (stubRecon) RunDaily(_ context.Context, d time.Time, a *int64) (*backoffice.NostroReconReport, error) {
	return &backoffice.NostroReconReport{Date: d}, nil
}
func (stubRecon) AssignBreak(_ context.Context, id, actor int64) (*backoffice.NostroReconBreak, error) {
	return &backoffice.NostroReconBreak{ID: id, Status: backoffice.NostroBreakInvestigating}, nil
}
func (stubRecon) ResolveBreak(_ context.Context, id int64, notes string, actor int64) (*backoffice.NostroReconBreak, error) {
	return &backoffice.NostroReconBreak{ID: id, Status: backoffice.NostroBreakResolved}, nil
}

type stubSwift struct{}

func (stubSwift) Query(_ context.Context, f backoffice.SwiftFilter) ([]backoffice.SwiftMessage, error) {
	return []backoffice.SwiftMessage{}, nil
}

type stubConfirmer struct{}

func (stubConfirmer) ProcessConfirmation(_ context.Context, c backoffice.SwiftConfirmation) (*backoffice.ConfirmationResult, error) {
	return &backoffice.ConfirmationResult{InstructionID: 9, Status: "SETTLED"}, nil
}

type stubExporter struct {
	err error
}

func (s stubExporter) Export(_ context.Context, req backoffice.ReportRequest) (*backoffice.ComplianceExport, error) {
	if s.err != nil {
		return nil, s.err
	}
	return &backoffice.ComplianceExport{Type: req.Type,
		GeneratedAt: time.Now().UTC()}, nil
}

// ---------------------------------------------------------------------------
// Per-handler behavior.
// ---------------------------------------------------------------------------

func TestAdminNostroAccountCreate(t *testing.T) {
	// Happy path → 201 with the account.
	rec, req := boReq("POST", "/api/v1/admin/nostro-accounts",
		`{"currency":"usd","bank_name":"Deutsche Bank","iban":"DE89370400440532013000","role":"VOSTRO"}`, true)
	AdminNostroAccountCreate(stubAccounts{})(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body)
	}
	var env struct {
		Account struct {
			Role    string `json:"role"`
			Balance string `json:"balance"`
		} `json:"account"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	if env.Account.Role != "VOSTRO" || env.Account.Balance != "0" {
		t.Fatalf("payload: %+v", env.Account)
	}

	// Malformed JSON → 400.
	rec, req = boReq("POST", "/api/v1/admin/nostro-accounts", `{bad`, true)
	AdminNostroAccountCreate(stubAccounts{})(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("malformed: status=%d", rec.Code)
	}

	// Service-coded errors surface via writeServiceErr.
	rec, req = boReq("POST", "/api/v1/admin/nostro-accounts",
		`{"currency":"US","bank_name":"B","iban":"X"}`, true)
	AdminNostroAccountCreate(stubAccountsErr{})(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid ccy: status=%d body=%s", rec.Code, rec.Body)
	}
}

type stubAccountsErr struct{ stubAccounts }

func (stubAccountsErr) CreateAccount(_ context.Context, in backoffice.CreateAccountInput) (*backoffice.NostroAccount, error) {
	return nil, excerrors.New("INVALID_REQUEST", "currency must be 3 letters")
}

func TestAdminNostroAccountList_Filters(t *testing.T) {
	rec, req := boReq("GET",
		"/api/v1/admin/nostro-accounts?currency=USD&role=nostro", "", true)
	AdminNostroAccountList(stubAccounts{})(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d", rec.Code)
	}
	rec, req = boReq("GET",
		"/api/v1/admin/nostro-accounts?role=BANANA", "", true)
	AdminNostroAccountList(stubAccounts{})(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad role: status=%d", rec.Code)
	}
}

func TestAdminNostroReconciliation_DateParsing(t *testing.T) {
	rec, req := boReq("GET",
		"/api/v1/admin/nostro-reconciliation?date=2026-09-28", "", true)
	AdminNostroReconciliation(stubRecon{})(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body)
	}
	var rep backoffice.NostroReconReport
	if err := json.Unmarshal(rec.Body.Bytes(), &rep); err != nil {
		t.Fatal(err)
	}
	if rep.Date.Format("2006-01-02") != "2026-09-28" {
		t.Fatalf("date=%v", rep.Date)
	}
	rec, req = boReq("GET",
		"/api/v1/admin/nostro-reconciliation?date=28.09.2026", "", true)
	AdminNostroReconciliation(stubRecon{})(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad date: status=%d", rec.Code)
	}
}

func TestAdminNostroBreakResolve(t *testing.T) {
	// RESOLVE happy path.
	rec, req := boReq("POST",
		"/api/v1/admin/nostro-reconciliation/breaks/7/resolve",
		`{"action":"RESOLVE","notes":"bank fee confirmed"}`, true)
	req.SetPathValue("id", "7")
	AdminNostroBreakResolve(stubRecon{})(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("resolve: status=%d body=%s", rec.Code, rec.Body)
	}
	// Bad id.
	rec, req = boReq("POST",
		"/api/v1/admin/nostro-reconciliation/breaks/x/resolve",
		`{"action":"RESOLVE"}`, true)
	req.SetPathValue("id", "x")
	AdminNostroBreakResolve(stubRecon{})(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad id: status=%d", rec.Code)
	}
	// Bad action.
	rec, req = boReq("POST",
		"/api/v1/admin/nostro-reconciliation/breaks/7/resolve",
		`{"action":"DELETE"}`, true)
	req.SetPathValue("id", "7")
	AdminNostroBreakResolve(stubRecon{})(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad action: status=%d", rec.Code)
	}
}

func TestAdminSwiftMessages(t *testing.T) {
	// Happy path with all filters.
	rec, req := boReq("GET",
		"/api/v1/admin/swift-messages?from=2026-09-01&to=2026-09-30&type=MT900&direction=IN&limit=50",
		"", true)
	AdminSwiftMessages(stubSwift{})(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body)
	}
	// Malformed cursor → 400.
	rec, req = boReq("GET",
		"/api/v1/admin/swift-messages?cursor=!!bogus!!", "", true)
	AdminSwiftMessages(stubSwift{})(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad cursor: status=%d", rec.Code)
	}
	// Bad limit → 400.
	rec, req = boReq("GET",
		"/api/v1/admin/swift-messages?limit=99999", "", true)
	AdminSwiftMessages(stubSwift{})(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad limit: status=%d", rec.Code)
	}
	// Bad date → 400.
	rec, req = boReq("GET",
		"/api/v1/admin/swift-messages?from=yesterday", "", true)
	AdminSwiftMessages(stubSwift{})(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad from: status=%d", rec.Code)
	}
}

func TestAdminSettlementConfirmation(t *testing.T) {
	rec, req := boReq("POST", "/api/v1/admin/settlement-confirmations",
		`{"message_type":"MT910","reference":"BANK-1","related_reference":"LEG-9","currency":"USD","amount":"150.25","value_date":"2026-09-28"}`, true)
	AdminSettlementConfirmation(stubConfirmer{})(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body)
	}
	// Non-positive / non-decimal amounts → 400.
	for _, amt := range []string{`"0"`, `"-5"`, `"abc"`} {
		rec, req = boReq("POST", "/api/v1/admin/settlement-confirmations",
			`{"message_type":"MT910","reference":"R","currency":"USD","amount":`+amt+`}`, true)
		AdminSettlementConfirmation(stubConfirmer{})(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("amount %s: status=%d", amt, rec.Code)
		}
	}
}

func TestAdminComplianceReport(t *testing.T) {
	// Missing type → 400.
	rec, req := boReq("GET", "/api/v1/admin/compliance-report", "", true)
	AdminComplianceReport(stubExporter{})(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("no type: status=%d", rec.Code)
	}
	// Happy path.
	rec, req = boReq("GET",
		"/api/v1/admin/compliance-report?type=EMIR&from=2026-09-01&to=2026-09-30",
		"", true)
	AdminComplianceReport(stubExporter{})(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body)
	}
	var out struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Type != "EMIR" {
		t.Fatalf("type=%q", out.Type)
	}
	// Service-degraded seam code propagates (nil store inside svc).
	rec, req = boReq("GET",
		"/api/v1/admin/compliance-report?type=BASEL3", "", true)
	AdminComplianceReport(stubExporter{
		err: excerrors.New("SERVICE_DEGRADED", "basel store nil")})(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("degraded: status=%d", rec.Code)
	}
}
