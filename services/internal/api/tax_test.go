// Task 20.3.10 — tax-report handler deltas: from/to window, koinly
// export, 5/day limiter.
package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"exchange/internal/tax"
	"exchange/pkg/decimal"
)

type taxFakeFills struct{ fills []tax.Fill }

func (f taxFakeFills) Fills(context.Context, int64, time.Time) ([]tax.Fill, error) {
	return f.fills, nil
}

func taxTestSvc(t *testing.T) *tax.Service {
	t.Helper()
	svc, err := tax.NewService(taxFakeFills{[]tax.Fill{
		{TradeID: 1, InstrumentID: 7, Symbol: "EUR/USD", QuoteCurrency: "USD",
			Side: "BUY", Quantity: decimal.RequireFromString("100"),
			Price: decimal.RequireFromString("1.10"), At: time.Date(2026, 1, 5, 12, 0, 0, 0, time.UTC)},
		{TradeID: 2, InstrumentID: 7, Symbol: "EUR/USD", QuoteCurrency: "USD",
			Side: "SELL", Quantity: decimal.RequireFromString("100"),
			Price: decimal.RequireFromString("1.30"), At: time.Date(2026, 2, 1, 12, 0, 0, 0, time.UTC)},
	}})
	if err != nil {
		t.Fatal(err)
	}
	return svc
}

type taxMemLimiter struct {
	max, n int
}

func (m *taxMemLimiter) Allow(context.Context, int64) (int, error) {
	m.n++
	if m.n > m.max {
		return 0, tax.ErrDailyReportLimit
	}
	return m.max - m.n, nil
}

func TestTaxReportYearPathStillWorks(t *testing.T) {
	h := TaxReport(taxTestSvc(t))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, userCtx(httptest.NewRequest(http.MethodGet,
		"/api/v1/account/tax-report?year=2026", nil), 42))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), `"year":2026`) {
		t.Fatalf("body=%s", rec.Body)
	}
}

func TestTaxReportFromToWindow(t *testing.T) {
	h := TaxReport(taxTestSvc(t))
	// A window covering only January must report zero disposals.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, userCtx(httptest.NewRequest(http.MethodGet,
		"/api/v1/account/tax-report?from=2026-01-01&to=2026-01-20", nil), 42))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), `"disposals":[]`) &&
		!strings.Contains(rec.Body.String(), `"disposals":null`) {
		t.Fatalf("windowed body=%s", rec.Body)
	}
	// February window catches the close.
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, userCtx(httptest.NewRequest(http.MethodGet,
		"/api/v1/account/tax-report?from=2026-02-01&to=2026-03-01&format=csv", nil), 42))
	if !strings.Contains(rec2.Body.String(), "EUR/USD") {
		t.Fatalf("csv=%s", rec2.Body)
	}
	// Malformed window → 400.
	rec3 := httptest.NewRecorder()
	h.ServeHTTP(rec3, userCtx(httptest.NewRequest(http.MethodGet,
		"/api/v1/account/tax-report?from=nope&to=2026-03-01", nil), 42))
	if rec3.Code != http.StatusBadRequest {
		t.Fatalf("bad from status=%d", rec3.Code)
	}
}

func TestTaxReportKoinlyFormat(t *testing.T) {
	h := TaxReport(taxTestSvc(t))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, userCtx(httptest.NewRequest(http.MethodGet,
		"/api/v1/account/tax-report?year=2026&format=koinly", nil), 42))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	if !strings.HasPrefix(body, "Date,Sent Amount,Sent Currency,") {
		t.Fatalf("koinly header missing:\n%s", body)
	}
	if !strings.Contains(body, "100.00000000,EUR,130.00000000,USD") {
		t.Fatalf("koinly disposal row missing:\n%s", body)
	}
}

func TestTaxReportDailyLimit(t *testing.T) {
	lim := &taxMemLimiter{max: tax.TaxReportsPerDay}
	h := TaxReport(taxTestSvc(t), lim)
	for i := 0; i < tax.TaxReportsPerDay; i++ {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, userCtx(httptest.NewRequest(http.MethodGet,
			"/api/v1/account/tax-report?year=2026", nil), 42))
		if rec.Code != http.StatusOK {
			t.Fatalf("request %d status=%d", i+1, rec.Code)
		}
		if rec.Header().Get("X-RateLimit-Remaining") == "" {
			t.Fatal("remaining header missing")
		}
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, userCtx(httptest.NewRequest(http.MethodGet,
		"/api/v1/account/tax-report?year=2026", nil), 42))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("6th report status=%d want 429", rec.Code)
	}
	if env := decodeErr(t, rec); env.Error != "RATE_LIMIT_TIER_EXCEEDED" {
		t.Fatalf("code=%s", env.Error)
	}
}
