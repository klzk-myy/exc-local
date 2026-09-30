// Handler tests for Task 20.3.12 — GET /api/v1/account/income.
// Source is an in-memory fake; the live CH path is covered by
// internal/analytics' EXC_CH_TEST-gated tests.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"exchange/internal/analytics"
	"exchange/internal/auth"
	"exchange/pkg/decimal"
)

type fakeIncomeSource struct {
	rows   []analytics.IncomeRow
	total  int64
	err    error
	any    bool
	anyErr error
	got    analytics.IncomeQuery
	probes int
}

func (f *fakeIncomeSource) Query(_ context.Context, q analytics.IncomeQuery) ([]analytics.IncomeRow, int64, error) {
	f.got = q
	return f.rows, f.total, f.err
}
func (f *fakeIncomeSource) HasAny(context.Context) (bool, error) {
	f.probes++
	return f.any, f.anyErr
}

var _ IncomeHistorySource = (*fakeIncomeSource)(nil)

func incomeReq(t *testing.T, url string, accountID int64) (*httptest.ResponseRecorder, *http.Request) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, url, nil)
	if accountID != 0 {
		req = req.WithContext(auth.WithClaims(req.Context(),
			auth.Claims{Subject: "1", AccountID: accountID, Scopes: []string{"read"}}))
	}
	return httptest.NewRecorder(), req
}

func TestAccountIncomeRows(t *testing.T) {
	src := &fakeIncomeSource{any: true, total: 2, rows: []analytics.IncomeRow{
		{PostedAt: time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC),
			AccountID: 42, Currency: "USD", IncomeType: "COMMISSION",
			EntryType: "FEE", Symbol: "EUR/USD",
			Amount:        decimal.RequireFromString("-2.5"),
			LedgerEntryID: 1001, JournalEntryID: 2002, ReferenceID: 77,
			Description: "COMMISSION trade=77"},
		{PostedAt: time.Date(2026, 9, 27, 11, 0, 0, 0, time.UTC),
			AccountID: 42, Currency: "USD", IncomeType: "REBATE",
			EntryType: "FEE", Amount: decimal.RequireFromString("0.4"),
			LedgerEntryID: 999, JournalEntryID: 2000},
	}}
	rec, req := incomeReq(t,
		"/api/v1/account/income?type=commission&symbol=EURUSD&limit=2", 42)
	AccountIncome(src).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d body %s", rec.Code, rec.Body)
	}
	// Filters normalized: lowercase type → canonical token, flat symbol
	// → slash form.
	if src.got.Type != "COMMISSION" || src.got.Symbol != "EUR/USD" {
		t.Fatalf("query = %+v", src.got)
	}
	if src.got.AccountID != 42 {
		t.Fatalf("account scoping: %d", src.got.AccountID)
	}
	var env struct {
		Data []struct {
			Type           string `json:"type"`
			EntryType      string `json:"entry_type"`
			Symbol         string `json:"symbol"`
			Amount         string `json:"amount"`
			LedgerEntryID  uint64 `json:"ledger_entry_id"`
			JournalEntryID uint64 `json:"journal_entry_id"`
		} `json:"data"`
		NextCursor string `json:"next_cursor"`
		Total      int64  `json:"total"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if env.Total != 2 || len(env.Data) != 2 {
		t.Fatalf("envelope = %+v", env)
	}
	if env.Data[0].Amount != "-2.50000000" || env.Data[0].JournalEntryID != 2002 {
		t.Fatalf("row = %+v", env.Data[0])
	}
	if env.NextCursor == "" {
		t.Fatal("full page must emit next_cursor")
	}
}

func TestAccountIncomeFailClosedCHDown(t *testing.T) {
	src := &fakeIncomeSource{err: errors.New("clickhouse: conn refused")}
	rec, req := incomeReq(t, "/api/v1/account/income", 42)
	AccountIncome(src).ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status %d, want 503 — CH outage must fail closed", rec.Code)
	}
	var env struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if env.Error != "SERVICE_DEGRADED" {
		t.Fatalf("code = %s, want SERVICE_DEGRADED", env.Error)
	}
}

func TestAccountIncomeEmptyProjection503(t *testing.T) {
	// Zero rows + unpopulated projection → 503 (an empty answer would
	// lie about a never-synced stream).
	src := &fakeIncomeSource{rows: nil, any: false}
	rec, req := incomeReq(t, "/api/v1/account/income", 42)
	AccountIncome(src).ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status %d, want 503", rec.Code)
	}
	if src.probes != 1 {
		t.Fatalf("emptiness probes = %d", src.probes)
	}
	// Populated projection + no matching rows → 200 empty page.
	src2 := &fakeIncomeSource{rows: nil, any: true}
	rec2, req2 := incomeReq(t, "/api/v1/account/income", 42)
	AccountIncome(src2).ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("status %d, want 200 empty page", rec2.Code)
	}
	var env struct {
		Data []any `json:"data"`
	}
	if err := json.Unmarshal(rec2.Body.Bytes(), &env); err != nil || len(env.Data) != 0 {
		t.Fatalf("empty page decode: %v %v", env.Data, err)
	}
}

func TestAccountIncomeValidation(t *testing.T) {
	src := &fakeIncomeSource{any: true}
	cases := []struct {
		url  string
		want int
	}{
		{"/api/v1/account/income?type=BAD%20TYPE%21", http.StatusBadRequest},
		{"/api/v1/account/income?type=%27%3Bdrop", http.StatusBadRequest},
		{"/api/v1/account/income?symbol=XX", http.StatusBadRequest},
		{"/api/v1/account/income?from=nope", http.StatusBadRequest},
		{"/api/v1/account/income?limit=99999", http.StatusBadRequest},
		{"/api/v1/account/income?cursor=garbage", http.StatusBadRequest},
		{"/api/v1/account/income?account_id=43", http.StatusForbidden},
	}
	for _, tc := range cases {
		rec, req := incomeReq(t, tc.url, 42)
		AccountIncome(src).ServeHTTP(rec, req)
		if rec.Code != tc.want {
			t.Fatalf("%s → %d, want %d (body %s)", tc.url, rec.Code, tc.want, rec.Body)
		}
	}
	// Unauthenticated → 401.
	rec, req := incomeReq(t, "/api/v1/account/income", 0)
	AccountIncome(src).ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauth → %d, want 401", rec.Code)
	}
	// Nil store → 503.
	rec2, req2 := incomeReq(t, "/api/v1/account/income", 42)
	AccountIncome(nil).ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusServiceUnavailable {
		t.Fatalf("nil store → %d, want 503", rec2.Code)
	}
}

func TestAccountIncomeWindowAndCursor(t *testing.T) {
	src := &fakeIncomeSource{any: true}
	rec, req := incomeReq(t,
		"/api/v1/account/income?from=2026-09-01T00:00:00Z&to=2026-09-28T00:00:00Z", 42)
	AccountIncome(src).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d body %s", rec.Code, rec.Body)
	}
	if src.got.From.IsZero() || src.got.To.IsZero() {
		t.Fatalf("window not passed: %+v", src.got)
	}
	if src.got.From.Format("2006-01-02") != "2026-09-01" {
		t.Fatalf("from = %v", src.got.From)
	}
}
