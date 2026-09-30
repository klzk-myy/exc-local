// Task 20.3.14 — cost-preview + annual-disclosure handler coverage.
package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"exchange/internal/analytics"
	"exchange/internal/funding"
	"exchange/internal/oracle"
	"exchange/internal/settlement"
	"exchange/pkg/decimal"
)

// --- analytics seam fakes (local; the analytics package fakes are
// unexported test helpers) -------------------------------------------------

type apiCostMarks struct{ v oracle.MarkView }

func (f apiCostMarks) Mark(context.Context, string) (oracle.MarkView, error) {
	return f.v, nil
}

type apiCostInstruments struct{ i *analytics.CostInstrument }

func (f apiCostInstruments) InstrumentBySymbol(context.Context, string) (*analytics.CostInstrument, error) {
	return f.i, nil
}

type apiCostFeeModels struct{ m settlement.FeeModel }

func (f apiCostFeeModels) FeeModel(context.Context, int64) (settlement.FeeModel, error) {
	return f.m, nil
}

type apiCostCommStore struct{ tiers []settlement.CommissionTier }

func (f apiCostCommStore) LoadCommissionTiers(context.Context) ([]settlement.CommissionTier, error) {
	return f.tiers, nil
}

func (f apiCostCommStore) MonthlyVolumeUSD(context.Context, int64, time.Time) (decimal.Decimal, error) {
	return decimal.Zero, nil
}

func (f apiCostCommStore) RecordFillVolumeUSD(context.Context, int64, time.Time, decimal.Decimal) error {
	return nil
}

type apiCostSwap struct{}

func (apiCostSwap) LatestSwapRate(context.Context, int64, time.Time) (settlement.SwapRate, bool, error) {
	return settlement.SwapRate{}, false, nil
}

type apiCostAccounts struct{ m *funding.AccountMeta }

func (f apiCostAccounts) AccountMeta(context.Context, int64) (*funding.AccountMeta, error) {
	return f.m, nil
}

type apiCostConv struct{}

func (apiCostConv) MidRate(_ context.Context, from, to string) (funding.MidRate, error) {
	return funding.MidRate{Rate: decimal.RequireFromString("1")}, nil
}

type apiCostActivity struct{ rows []analytics.CostActivityRow }

func (f apiCostActivity) ActivityTotals(context.Context, int64, time.Time, time.Time) ([]analytics.CostActivityRow, error) {
	return f.rows, nil
}

type apiCostFills struct{ fills []analytics.CostTradeFill }

func (f apiCostFills) TradeFills(context.Context, int64, time.Time, time.Time) ([]analytics.CostTradeFill, error) {
	return f.fills, nil
}

func costPreviewSvc(t *testing.T) *analytics.CostsDisclosureService {
	t.Helper()
	svc, err := analytics.NewCostsDisclosureService(analytics.CostsDeps{
		Marks: apiCostMarks{oracle.MarkView{
			Price: decimal.RequireFromString("1.1000"), Found: true, ValidAt: time.Now()}},
		Instruments: apiCostInstruments{&analytics.CostInstrument{
			ID: 7, Symbol: "EUR/USD", BaseCurrency: "EUR", QuoteCurrency: "USD",
			LotSize:  decimal.RequireFromString("100000"),
			TickSize: decimal.RequireFromString("0.0001")}},
		FeeModels:   apiCostFeeModels{settlement.FeeModelSpreadMarkup},
		Commissions: apiCostCommStore{},
		Swap:        apiCostSwap{},
		Accounts:    apiCostAccounts{&funding.AccountMeta{ID: 42, BaseCurrency: "USD"}},
		Conv:        apiCostConv{},
		Activity: apiCostActivity{[]analytics.CostActivityRow{
			{EntryType: "FEE", Direction: "CREDIT", Class: "COMMISSION",
				Currency: "USD", Total: decimal.RequireFromString("12.50"), Count: 3},
			{EntryType: "ROLLOVER", Direction: "CREDIT", Class: "SWAP",
				Currency: "USD", Total: decimal.RequireFromString("4.20"), Count: 2},
		}},
		Trades: apiCostFills{},
	})
	if err != nil {
		t.Fatal(err)
	}
	return svc
}

func TestCostPreviewExAnte(t *testing.T) {
	h := AccountCostPreview(costPreviewSvc(t))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, userCtx(httptest.NewRequest(http.MethodGet,
		"/api/v1/account/cost-preview?symbol=EUR/USD&side=BUY&quantity=100000", nil), 42))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	for _, want := range []string{`"fee_model"`, `"spread_cost"`,
		`"inducement_statement"`, `"valid_for_s"`, `"estimators"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("preview missing %s:\n%s", want, body)
		}
	}
}

func TestCostPreviewAnnualMode(t *testing.T) {
	h := AccountCostPreview(costPreviewSvc(t))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, userCtx(httptest.NewRequest(http.MethodGet,
		"/api/v1/account/cost-preview?annual=2026", nil), 42))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	for _, want := range []string{`"year":2026`, `"inducement_statement"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("annual missing %s:\n%s", want, body)
		}
	}
}

func TestCostPreviewValidation(t *testing.T) {
	h := AccountCostPreview(costPreviewSvc(t))
	// Missing params → 400.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, userCtx(httptest.NewRequest(http.MethodGet,
		"/api/v1/account/cost-preview", nil), 42))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("missing params status=%d", rec.Code)
	}
	// Bad quantity → 400.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, userCtx(httptest.NewRequest(http.MethodGet,
		"/api/v1/account/cost-preview?symbol=EUR/USD&side=BUY&quantity=-5", nil), 42))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad qty status=%d", rec.Code)
	}
	// Unauthenticated → 401.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet,
		"/api/v1/account/cost-preview?symbol=EUR/USD&side=BUY&quantity=1", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauth status=%d", rec.Code)
	}
}
