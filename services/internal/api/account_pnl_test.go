// Tests for the Phase-13 Task 13.3.4 GET /api/v1/account/pnl handler —
// claims identity, foreign-account rejection, fail-closed service
// errors. The service math itself lives in internal/risk/pnl_test.go.
package api

import (
	"context"
	stderrors "errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"exchange/internal/risk"
	"exchange/pkg/decimal"
	excerrors "exchange/pkg/errors"
)

// pnlFakeStore satisfies risk.PnlStore; marks come from fakePnlMarks.
type pnlFakeStore struct {
	base string
	rows []risk.PnlRow
	err  error
}

func (f *pnlFakeStore) PnlRows(context.Context, int64) ([]risk.PnlRow, error) {
	return f.rows, f.err
}
func (f *pnlFakeStore) AccountBaseCurrency(context.Context, int64) (string, error) {
	return f.base, f.err
}
func (f *pnlFakeStore) AccountsHoldingInstrument(context.Context, int64) ([]int64, error) {
	return nil, f.err
}

type pnlFakeMarks struct{}

func (pnlFakeMarks) ReferencePrice(context.Context, int64) (*decimal.Decimal, error) {
	return nil, nil
}

func newPnlHandler(t *testing.T, store *pnlFakeStore) http.HandlerFunc {
	t.Helper()
	svc, err := risk.NewPnlService(risk.PnlOptions{Store: store, Marks: pnlFakeMarks{}})
	if err != nil {
		t.Fatal(err)
	}
	return AccountPnL(svc)
}

func TestAccountPnLRequiresClaims(t *testing.T) {
	h := newPnlHandler(t, &pnlFakeStore{base: "USD"})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/account/pnl", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if env := decodeErr(t, rec); env.Error != "UNAUTHORIZED" {
		t.Fatalf("code = %s", env.Error)
	}
}

func TestAccountPnLRejectsForeignAccount(t *testing.T) {
	h := newPnlHandler(t, &pnlFakeStore{base: "USD"})
	rec := httptest.NewRecorder()
	req := userCtx(httptest.NewRequest(http.MethodGet,
		"/api/v1/account/pnl?account_id=99", nil), 7)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
}

func TestAccountPnLNilServiceDegraded(t *testing.T) {
	h := AccountPnL(nil)
	rec := httptest.NewRecorder()
	req := userCtx(httptest.NewRequest(http.MethodGet, "/api/v1/account/pnl", nil), 7)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	if env := decodeErr(t, rec); env.Error != "SERVICE_DEGRADED" {
		t.Fatalf("code = %s", env.Error)
	}
}

func TestAccountPnLHappyPath(t *testing.T) {
	h := newPnlHandler(t, &pnlFakeStore{base: "USD", rows: []risk.PnlRow{
		{InstrumentID: 1, Symbol: "EUR/USD", QuoteCurrency: "USD",
			Side: "LONG", Quantity: decimal.RequireFromString("10"),
			EntryPrice: decimal.RequireFromString("1.10"),
			StoredMark: func() *decimal.Decimal {
				d := decimal.RequireFromString("1.20")
				return &d
			}(),
			RealizedPnL: decimal.RequireFromString("0.50")},
	}})
	rec := httptest.NewRecorder()
	req := userCtx(httptest.NewRequest(http.MethodGet, "/api/v1/account/pnl", nil), 7)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{
		`"account_id":7`, `"base_currency":"USD"`, `"event":"pnl"`,
		`"unrealized_pnl":"1"`, `"realized_pnl":"0.5"`,
	} {
		if !containsStr(body, want) {
			t.Fatalf("body missing %s: %s", want, body)
		}
	}
}

func TestAccountPnLStoreErrorFailsClosed(t *testing.T) {
	h := newPnlHandler(t, &pnlFakeStore{
		base: "USD",
		err:  excerrors.New("SERVICE_DEGRADED", "pg down")})
	rec := httptest.NewRecorder()
	req := userCtx(httptest.NewRequest(http.MethodGet, "/api/v1/account/pnl", nil), 7)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	// Uncoded errors map to INTERNAL_ERROR, never a fabricated 200.
	h2 := newPnlHandler(t, &pnlFakeStore{base: "USD",
		err: stderrors.New("boom")})
	rec2 := httptest.NewRecorder()
	h2.ServeHTTP(rec2, userCtx(httptest.NewRequest(
		http.MethodGet, "/api/v1/account/pnl", nil), 7))
	if rec2.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec2.Code)
	}
}

func containsStr(hay, needle string) bool {
	return len(hay) >= len(needle) && (func() bool {
		for i := 0; i+len(needle) <= len(hay); i++ {
			if hay[i:i+len(needle)] == needle {
				return true
			}
		}
		return false
	})()
}
