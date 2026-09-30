// Handler tests for Tasks 20.3.8/20.3.9 — account scoping on the
// confirmation portal read and the TCA report endpoint.
package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"exchange/internal/analytics"
	"exchange/internal/auth"
	"exchange/internal/reporting"
)

// ---------------------------------------------------------------------------
// TCA report endpoint
// ---------------------------------------------------------------------------

type fakeTCAReports struct {
	rows []analytics.AggregateRow
	err  error
	got  analytics.ReportFilter
}

func (f *fakeTCAReports) Aggregate(_ context.Context, flt analytics.ReportFilter) ([]analytics.AggregateRow, error) {
	f.got = flt
	return f.rows, f.err
}

type fakeClassResolver struct {
	ids []int64
	err error
}

func (f *fakeClassResolver) InstrumentIDs(_ context.Context, _ string) ([]int64, error) {
	return f.ids, f.err
}

func tcaReq(t *testing.T, accountID int64, path, pathAcct string, scopes []string) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.SetPathValue("account_id", pathAcct)
	return req.WithContext(auth.WithClaims(req.Context(),
		auth.Claims{Subject: "u", AccountID: accountID, Scopes: scopes}))
}

func TestReportsTCA_HappyPath(t *testing.T) {
	reps := &fakeTCAReports{rows: []analytics.AggregateRow{
		{AccountID: 7, Symbol: "EUR/USD", Fills: 12},
	}}
	h := ReportsTCA(TCAReportDeps{Reports: reps, Classes: &fakeClassResolver{ids: []int64{3}}})
	rec := httptest.NewRecorder()
	h(rec, tcaReq(t, 7, "/api/v1/reports/tca/7?period=monthly&instrument_class=FX_MAJOR", "7", []string{"read"}))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if reps.got.AccountID != 7 || reps.got.Period != "monthly" {
		t.Fatalf("filter %+v", reps.got)
	}
	if len(reps.got.InstrumentIDs) != 1 || reps.got.InstrumentIDs[0] != 3 {
		t.Fatalf("class ids %v", reps.got.InstrumentIDs)
	}
}

func TestReportsTCA_ForeignAccountForbidden(t *testing.T) {
	reps := &fakeTCAReports{}
	h := ReportsTCA(TCAReportDeps{Reports: reps})
	rec := httptest.NewRecorder()
	h(rec, tcaReq(t, 7, "/api/v1/reports/tca/99", "99", []string{"read"}))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
}

func TestReportsTCA_AdminReadsForeign(t *testing.T) {
	reps := &fakeTCAReports{}
	h := ReportsTCA(TCAReportDeps{Reports: reps})
	rec := httptest.NewRecorder()
	h(rec, tcaReq(t, 7, "/api/v1/reports/tca/99", "99", []string{"admin"}))
	if rec.Code != http.StatusOK {
		t.Fatalf("admin read: status %d: %s", rec.Code, rec.Body.String())
	}
	if reps.got.AccountID != 99 {
		t.Fatalf("admin filter account %d", reps.got.AccountID)
	}
}

func TestReportsTCA_BadPeriodRejected(t *testing.T) {
	h := ReportsTCA(TCAReportDeps{Reports: &fakeTCAReports{}})
	rec := httptest.NewRecorder()
	h(rec, tcaReq(t, 7, "/api/v1/reports/tca/7?period=hourly", "7", []string{"read"}))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status %d", rec.Code)
	}
}

func TestReportsTCA_NoClaimsUnauthorized(t *testing.T) {
	h := ReportsTCA(TCAReportDeps{Reports: &fakeTCAReports{}})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/reports/tca/7", nil)
	req.SetPathValue("account_id", "7")
	h(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status %d", rec.Code)
	}
}

// ---------------------------------------------------------------------------
// Confirmation portal endpoint
// ---------------------------------------------------------------------------

type fakeConfFiles struct {
	file *analytics.ConfirmationFile
	err  error
	got  struct {
		acct, trade int64
		format      string
	}
}

func (f *fakeConfFiles) GetFile(_ context.Context, accountID, tradeID int64, format string) (*analytics.ConfirmationFile, error) {
	f.got.acct, f.got.trade, f.got.format = accountID, tradeID, format
	return f.file, f.err
}

func TestAccountConfirmation_OwnerGetsPDF(t *testing.T) {
	src := &fakeConfFiles{file: &analytics.ConfirmationFile{
		Body: []byte("%PDF-x"), ContentType: "application/pdf",
		FileRef: "confirmations/7/55/v1",
		Row:     &analytics.Confirmation{Status: "DELIVERED", Version: 1},
	}}
	h := AccountConfirmation(ConfirmationReadDeps{Service: src})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/account/confirmations/55", nil)
	req.SetPathValue("trade_id", "55")
	req = req.WithContext(auth.WithClaims(req.Context(),
		auth.Claims{Subject: "u", AccountID: 7, Scopes: []string{"read"}}))
	h(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Content-Type") != "application/pdf" {
		t.Fatalf("ctype %s", rec.Header().Get("Content-Type"))
	}
	if src.got.acct != 7 || src.got.trade != 55 || src.got.format != "pdf" {
		t.Fatalf("fetch %+v", src.got)
	}
	if rec.Header().Get("X-Confirmation-Status") != "DELIVERED" {
		t.Fatal("status header missing")
	}
}

func TestAccountConfirmation_JSONFormat(t *testing.T) {
	src := &fakeConfFiles{file: &analytics.ConfirmationFile{
		Body: []byte(`{"type":"trade_confirmation"}`), ContentType: "application/json",
		FileRef: "confirmations/7/55/v1",
	}}
	h := AccountConfirmation(ConfirmationReadDeps{Service: src})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/account/confirmations/55?format=json", nil)
	req.SetPathValue("trade_id", "55")
	req = req.WithContext(auth.WithClaims(req.Context(),
		auth.Claims{Subject: "u", AccountID: 7, Scopes: []string{"read"}}))
	h(rec, req)
	if rec.Code != http.StatusOK || src.got.format != "json" {
		t.Fatalf("code=%d fmt=%s", rec.Code, src.got.format)
	}
}

func TestAccountConfirmation_AdminResolvesOwner(t *testing.T) {
	src := &fakeConfFiles{file: &analytics.ConfirmationFile{
		Body: []byte("%PDF-x"), ContentType: "application/pdf", FileRef: "k",
	}}
	tracker := reporting.NewMemConfirmationStore()
	tracker.Seed(reporting.ConfirmationRecord{TradeID: 55, AccountID: 9, Version: 1})
	h := AccountConfirmation(ConfirmationReadDeps{Service: src, AdminLookup: tracker})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/account/confirmations/55", nil)
	req.SetPathValue("trade_id", "55")
	req = req.WithContext(auth.WithClaims(req.Context(),
		auth.Claims{Subject: "ops", AccountID: 1, Scopes: []string{"admin"}}))
	h(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if src.got.acct != 9 {
		t.Fatalf("admin must resolve owning account 9, got %d", src.got.acct)
	}
}

func TestAccountConfirmation_BadID(t *testing.T) {
	h := AccountConfirmation(ConfirmationReadDeps{Service: &fakeConfFiles{}})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/account/confirmations/abc", nil)
	req.SetPathValue("trade_id", "abc")
	req = req.WithContext(auth.WithClaims(req.Context(),
		auth.Claims{Subject: "u", AccountID: 7, Scopes: []string{"read"}}))
	h(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status %d", rec.Code)
	}
}
