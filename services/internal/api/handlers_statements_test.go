// handlers_statements_test.go — Task 20.3.6/20.3.7 REST surface tests
// (in-memory fakes; no PG).
package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"exchange/internal/analytics"
	"exchange/internal/auth"
	excerrors "exchange/pkg/errors"
)

// ---- fakes -------------------------------------------------------------

type fakeStatementStore struct {
	rows     []analytics.StatementRow
	total    int64
	file     *analytics.StatementFile
	listErr  error
	fetchErr error
	gotAfter int64
	gotLimit int
}

func (f *fakeStatementStore) List(_ context.Context, _ int64,
	_ analytics.StatementPeriod, limit int, _ *time.Time, afterID int64) ([]analytics.StatementRow, int64, error) {
	f.gotAfter, f.gotLimit = afterID, limit
	return f.rows, f.total, f.listErr
}

// DocumentPIN satisfies the encrypted-document PIN seam (Task 20.3.8).
func (f *fakeStatementStore) DocumentPIN(int64) string { return "TESTPIN1234" }

func (f *fakeStatementStore) Fetch(_ context.Context, _, statementID int64, format string) (*analytics.StatementFile, error) {
	if f.fetchErr != nil {
		return nil, f.fetchErr
	}
	if f.file != nil {
		return f.file, nil
	}
	return &analytics.StatementFile{
		Body:        []byte("%PDF-1.4 fake"),
		ContentType: "application/pdf",
		FileRef:     fmt.Sprintf("stmt/%d", statementID),
	}, nil
}

type fakeInvoiceStore struct {
	invs []analytics.Invoice
	err  error
}

func (f *fakeInvoiceStore) List(_ context.Context, _ int64, _ *time.Time, _ int) ([]analytics.Invoice, error) {
	return f.invs, f.err
}

func (f *fakeInvoiceStore) FetchFile(_ context.Context, _ int64, _ string) ([]byte, string, error) {
	return []byte("csv"), "text/csv", nil
}

type fakeFinanceService struct {
	tb     *analytics.TrialBalance
	pls    []analytics.PeriodPnL
	checks []analytics.ReconCheck
	csv    []byte
	err    error
}

func (f *fakeFinanceService) Compute(_ context.Context, day time.Time) (*analytics.TrialBalance, error) {
	if f.err != nil {
		return nil, f.err
	}
	if f.tb != nil {
		return f.tb, nil
	}
	return &analytics.TrialBalance{Day: day}, nil
}

func (f *fakeFinanceService) Reconcile(_ context.Context, _ time.Time) ([]analytics.ReconCheck, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.checks, nil
}

func (f *fakeFinanceService) PeriodPnL(_ context.Context, _, _ time.Time) ([]analytics.PeriodPnL, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.pls, nil
}

func (f *fakeFinanceService) ExportCSV(_ context.Context, _ analytics.ExportKind, _ time.Time) ([]byte, *analytics.TrialBalance, []analytics.ReconCheck, error) {
	if f.err != nil {
		return nil, nil, nil, f.err
	}
	return f.csv, f.tb, f.checks, nil
}

// authed wraps a request with claims.
func authed(r *http.Request, accountID int64) *http.Request {
	return r.WithContext(auth.WithClaims(r.Context(),
		auth.Claims{Subject: fmt.Sprint(accountID), AccountID: accountID, Scopes: []string{"read"}}))
}

func decodeErrCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var env struct {
		Error string `json:"error"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&env); err != nil {
		t.Fatalf("decode error envelope: %v", err)
	}
	return env.Error
}

// ---- statements list ----------------------------------------------------

func TestAccountStatementsList(t *testing.T) {
	store := &fakeStatementStore{
		rows: []analytics.StatementRow{{
			StatementID: 7, AccountID: 42, Period: analytics.StmtPeriodDaily,
			PeriodStart: time.Date(2026, 9, 29, 0, 0, 0, 0, time.UTC),
			PeriodEnd:   time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC),
			FileRef:     "statements/42/DAILY/2026-09-29",
			GeneratedAt: time.Date(2026, 9, 30, 1, 0, 0, 0, time.UTC),
		}},
		total: 1,
	}
	req := authed(httptest.NewRequest(http.MethodGet,
		"/api/v1/account/statements?period=DAILY&limit=10", nil), 42)
	rec := httptest.NewRecorder()
	AccountStatements(store).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d body %s", rec.Code, rec.Body)
	}
	var env ListEnvelope
	if err := json.NewDecoder(rec.Body).Decode(&env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if env.Total != 1 || env.Limit != 10 {
		t.Fatalf("envelope: %+v", env)
	}
	if store.gotLimit != 10 {
		t.Fatalf("limit not propagated: %d", store.gotLimit)
	}
}

func TestAccountStatementsValidation(t *testing.T) {
	store := &fakeStatementStore{}
	req := authed(httptest.NewRequest(http.MethodGet,
		"/api/v1/account/statements?period=WEEKLY", nil), 42)
	rec := httptest.NewRecorder()
	AccountStatements(store).ServeHTTP(rec, req)
	if got := decodeErrCode(t, rec); got != "INVALID_REQUEST" {
		t.Fatalf("expected INVALID_REQUEST, got %q", got)
	}
	// unauthenticated → 401-ish error envelope
	req2 := httptest.NewRequest(http.MethodGet, "/api/v1/account/statements", nil)
	rec2 := httptest.NewRecorder()
	AccountStatements(store).ServeHTTP(rec2, req2)
	if got := decodeErrCode(t, rec2); got != "UNAUTHORIZED" {
		t.Fatalf("expected UNAUTHORIZED, got %q", got)
	}
}

func TestAccountStatementsCursorPropagates(t *testing.T) {
	store := &fakeStatementStore{}
	cur := EncodeCursor(Cursor{CreatedAt: time.Date(2026, 9, 30, 1, 0, 0, 0, time.UTC), ID: 55})
	req := authed(httptest.NewRequest(http.MethodGet,
		"/api/v1/account/statements?cursor="+cur, nil), 42)
	rec := httptest.NewRecorder()
	AccountStatements(store).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if store.gotAfter != 55 {
		t.Fatalf("cursor id not propagated: %d", store.gotAfter)
	}
}

// ---- statement download --------------------------------------------------

func TestAccountStatementDownload(t *testing.T) {
	store := &fakeStatementStore{file: &analytics.StatementFile{
		Body: []byte("a,b\n1,2\n"), ContentType: "text/csv", FileRef: "x",
	}}
	req := authed(httptest.NewRequest(http.MethodGet,
		"/api/v1/account/statements/7/download?format=csv", nil), 42)
	req.SetPathValue("id", "7")
	rec := httptest.NewRecorder()
	AccountStatementDownload(store).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/csv" {
		t.Fatalf("content-type %q", ct)
	}
	if !strings.Contains(rec.Header().Get("Content-Disposition"), "statement-7.csv") {
		t.Fatalf("disposition %q", rec.Header().Get("Content-Disposition"))
	}
	if rec.Body.String() != "a,b\n1,2\n" {
		t.Fatalf("body %q", rec.Body.String())
	}
}

func TestAccountStatementDownloadErrors(t *testing.T) {
	store := &fakeStatementStore{fetchErr: excerrors.New("NOT_FOUND", "statement not found")}
	req := authed(httptest.NewRequest(http.MethodGet,
		"/api/v1/account/statements/9/download", nil), 42)
	req.SetPathValue("id", "9")
	rec := httptest.NewRecorder()
	AccountStatementDownload(store).ServeHTTP(rec, req)
	if got := decodeErrCode(t, rec); got != "NOT_FOUND" {
		t.Fatalf("expected NOT_FOUND, got %q (status %d)", got, rec.Code)
	}
	// bad id
	req2 := authed(httptest.NewRequest(http.MethodGet,
		"/api/v1/account/statements/abc/download", nil), 42)
	req2.SetPathValue("id", "abc")
	rec2 := httptest.NewRecorder()
	AccountStatementDownload(store).ServeHTTP(rec2, req2)
	if got := decodeErrCode(t, rec2); got != "INVALID_REQUEST" {
		t.Fatalf("expected INVALID_REQUEST, got %q", got)
	}
}

// ---- invoices -------------------------------------------------------------

func TestAdminInvoicesListAndCSV(t *testing.T) {
	store := &fakeInvoiceStore{invs: []analytics.Invoice{{
		InvoiceID: 3, AccountID: 42,
		Month: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC), Currency: "USD",
		TradingFees: decimal.RequireFromString("10"), Total: decimal.RequireFromString("10"),
		Status:      "ISSUED",
		GeneratedAt: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC),
	}}}
	req := authed(httptest.NewRequest(http.MethodGet,
		"/api/v1/admin/invoices?account=42&month=2026-09", nil), 1)
	rec := httptest.NewRecorder()
	AdminInvoices(store).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	// csv export
	req2 := authed(httptest.NewRequest(http.MethodGet,
		"/api/v1/admin/invoices?format=csv", nil), 1)
	rec2 := httptest.NewRecorder()
	AdminInvoices(store).ServeHTTP(rec2, req2)
	if ct := rec2.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/csv") {
		t.Fatalf("csv content-type %q", ct)
	}
	if !strings.Contains(rec2.Body.String(), "ISSUED") {
		t.Fatalf("csv body: %s", rec2.Body.String())
	}
	// bad month
	req3 := authed(httptest.NewRequest(http.MethodGet,
		"/api/v1/admin/invoices?month=not-a-month", nil), 1)
	rec3 := httptest.NewRecorder()
	AdminInvoices(store).ServeHTTP(rec3, req3)
	if got := decodeErrCode(t, rec3); got != "INVALID_REQUEST" {
		t.Fatalf("expected INVALID_REQUEST, got %q", got)
	}
}

// ---- finance exports -------------------------------------------------------

func TestAdminTrialBalanceJSONAndCSV(t *testing.T) {
	fs := &fakeFinanceService{tb: &analytics.TrialBalance{
		Day: time.Date(2026, 9, 29, 0, 0, 0, 0, time.UTC),
	}}
	req := authed(httptest.NewRequest(http.MethodGet,
		"/api/v1/admin/finance/trial-balance?date=2026-09-29", nil), 1)
	rec := httptest.NewRecorder()
	AdminTrialBalance(fs).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	// csv export path
	fs2 := &fakeFinanceService{csv: []byte("report,trial_balance\n")}
	req2 := authed(httptest.NewRequest(http.MethodGet,
		"/api/v1/admin/finance/trial-balance?format=csv", nil), 1)
	rec2 := httptest.NewRecorder()
	AdminTrialBalance(fs2).ServeHTTP(rec2, req2)
	if !strings.HasPrefix(rec2.Header().Get("Content-Type"), "text/csv") {
		t.Fatalf("csv ct %q", rec2.Header().Get("Content-Type"))
	}
	// recon mismatch → service error envelope
	fs3 := &fakeFinanceService{err: excerrors.New("LEDGER_IMBALANCE_ABORT", "variance")}
	req3 := authed(httptest.NewRequest(http.MethodGet,
		"/api/v1/admin/finance/trial-balance?format=csv", nil), 1)
	rec3 := httptest.NewRecorder()
	AdminTrialBalance(fs3).ServeHTTP(rec3, req3)
	if got := decodeErrCode(t, rec3); got != "LEDGER_IMBALANCE_ABORT" {
		t.Fatalf("expected LEDGER_IMBALANCE_ABORT, got %q", got)
	}
}

func TestAdminFinancePnLValidation(t *testing.T) {
	fs := &fakeFinanceService{}
	req := authed(httptest.NewRequest(http.MethodGet,
		"/api/v1/admin/finance/pnl?from=2026-09-30&to=2026-09-29", nil), 1)
	rec := httptest.NewRecorder()
	AdminFinancePnL(fs).ServeHTTP(rec, req)
	if got := decodeErrCode(t, rec); got != "INVALID_REQUEST" {
		t.Fatalf("expected INVALID_REQUEST, got %q", got)
	}
	// happy path
	fs2 := &fakeFinanceService{pls: []analytics.PeriodPnL{{Currency: "USD"}}}
	req2 := authed(httptest.NewRequest(http.MethodGet,
		"/api/v1/admin/finance/pnl?from=2026-09-28&to=2026-09-29", nil), 1)
	rec2 := httptest.NewRecorder()
	AdminFinancePnL(fs2).ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec2.Code, rec2.Body)
	}
	if !strings.Contains(rec2.Body.String(), "\"period_start\":\"2026-09-28\"") {
		t.Fatalf("body: %s", rec2.Body.String())
	}
}

func TestAdminFinanceBalanceSheet(t *testing.T) {
	fs := &fakeFinanceService{tb: &analytics.TrialBalance{
		Day: time.Date(2026, 9, 29, 0, 0, 0, 0, time.UTC),
		Currencies: []analytics.CurrencyTB{{
			Currency:             "USD",
			Assets:               decimal.RequireFromString("100"),
			Liabilities:          decimal.RequireFromString("80"),
			Equity:               decimal.RequireFromString("20"),
			AccountingEquationOK: true,
		}},
	}}
	req := authed(httptest.NewRequest(http.MethodGet,
		"/api/v1/admin/finance/balance-sheet?date=2026-09-29", nil), 1)
	rec := httptest.NewRecorder()
	AdminFinanceBalanceSheet(fs).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "\"accounting_equation_ok\":true") {
		t.Fatalf("body: %s", rec.Body.String())
	}
	// bad date
	req2 := authed(httptest.NewRequest(http.MethodGet,
		"/api/v1/admin/finance/balance-sheet?date=bad", nil), 1)
	rec2 := httptest.NewRecorder()
	AdminFinanceBalanceSheet(fs).ServeHTTP(rec2, req2)
	if got := decodeErrCode(t, rec2); got != "INVALID_REQUEST" {
		t.Fatalf("expected INVALID_REQUEST, got %q", got)
	}
}
