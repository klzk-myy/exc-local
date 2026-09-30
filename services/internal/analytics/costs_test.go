// Task 20.3.14 unit tests — deterministic fakes (no PG/Redis).
package analytics

import (
	"context"
	"testing"
	"time"

	"exchange/internal/funding"
	"exchange/internal/oracle"
	"exchange/internal/settlement"
	"exchange/pkg/decimal"
)

func costD(s string) decimal.Decimal { return decimal.RequireFromString(s) }

// --- fakes ----------------------------------------------------------------

type fakeCostMarks struct{ v oracle.MarkView }

func (f fakeCostMarks) Mark(context.Context, string) (oracle.MarkView, error) {
	return f.v, nil
}

type fakeCostInstruments struct{ i *CostInstrument }

func (f fakeCostInstruments) InstrumentBySymbol(context.Context, string) (*CostInstrument, error) {
	return f.i, nil
}

type fakeFeeModels struct{ m settlement.FeeModel }

func (f fakeFeeModels) FeeModel(context.Context, int64) (settlement.FeeModel, error) {
	return f.m, nil
}

type fakeCommStore struct {
	tiers []settlement.CommissionTier
	vol   decimal.Decimal
}

func (f fakeCommStore) LoadCommissionTiers(context.Context) ([]settlement.CommissionTier, error) {
	return f.tiers, nil
}
func (f fakeCommStore) MonthlyVolumeUSD(context.Context, int64, time.Time) (decimal.Decimal, error) {
	return f.vol, nil
}
func (f fakeCommStore) RecordFillVolumeUSD(context.Context, int64, time.Time, decimal.Decimal) error {
	return nil
}

type fakeSwapSrc struct {
	rate settlement.SwapRate
	ok   bool
}

func (f fakeSwapSrc) LatestSwapRate(context.Context, int64, time.Time) (settlement.SwapRate, bool, error) {
	return f.rate, f.ok, nil
}

type fakeSpread struct {
	bps   decimal.Decimal
	basis string
}

func (f fakeSpread) SpreadBps(context.Context, int64) (decimal.Decimal, string, error) {
	return f.bps, f.basis, nil
}

type fakeAcctMeta struct{ m *funding.AccountMeta }

func (f fakeAcctMeta) AccountMeta(context.Context, int64) (*funding.AccountMeta, error) {
	return f.m, nil
}

type fakeConv struct{ m funding.MidRate }

func (f fakeConv) MidRate(context.Context, string, string) (funding.MidRate, error) {
	return f.m, nil
}

type fakeActivity struct{ rows []CostActivityRow }

func (f fakeActivity) ActivityTotals(context.Context, int64, time.Time, time.Time) ([]CostActivityRow, error) {
	return f.rows, nil
}

type fakeFills struct{ fills []CostTradeFill }

func (f fakeFills) TradeFills(context.Context, int64, time.Time, time.Time) ([]CostTradeFill, error) {
	return f.fills, nil
}

var costTestInst = &CostInstrument{
	ID: 9, Symbol: "EUR/USD", BaseCurrency: "EUR", QuoteCurrency: "USD",
	LotSize: costD("100000"), TickSize: costD("0.00001"),
}

func costSvc(t *testing.T, d CostsDeps) *CostsDisclosureService {
	t.Helper()
	if d.Marks == nil {
		d.Marks = fakeCostMarks{oracle.MarkView{Price: costD("1.1000"), Found: true, ValidAt: time.Now()}}
	}
	if d.Instruments == nil {
		d.Instruments = fakeCostInstruments{costTestInst}
	}
	if d.FeeModels == nil {
		d.FeeModels = fakeFeeModels{settlement.FeeModelSpreadMarkup}
	}
	if d.Commissions == nil {
		d.Commissions = fakeCommStore{}
	}
	if d.Accounts == nil {
		d.Accounts = fakeAcctMeta{&funding.AccountMeta{ID: 42, BaseCurrency: "USD"}}
	}
	svc, err := NewCostsDisclosureService(d)
	if err != nil {
		t.Fatal(err)
	}
	return svc
}

// SPREAD_MARKUP account: commission is the markup (0 explicit); the
// spread line carries notional × bps.
func TestCostPreview_SpreadMarkupModel(t *testing.T) {
	svc := costSvc(t, CostsDeps{
		Spread: fakeSpread{costD("2"), "lp_markup_max"},
		Now:    func() time.Time { return time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC) },
	})
	p, err := svc.Preview(context.Background(), 42, "EURUSD", "BUY", costD("100000"))
	if err != nil {
		t.Fatal(err)
	}
	// notional = 100000 × 1.1 = 110000 USD; spread = 110000 × 2/10⁴ = 22
	if !p.SpreadCost.Equal(costD("22")) {
		t.Fatalf("spread=%s want 22", p.SpreadCost)
	}
	if !p.Commission.IsZero() || p.FeeModel != "SPREAD_MARKUP" {
		t.Fatalf("commission=%s model=%s", p.Commission, p.FeeModel)
	}
	if !p.Conversion.IsZero() || !p.TotalQuote.Equal(costD("22")) {
		t.Fatalf("conversion=%s total=%s", p.Conversion, p.TotalQuote)
	}
	if p.Inducement != NoInducementStatement {
		t.Fatal("no-inducement statement missing")
	}
	if p.ValidForS != CostPreviewValidForS || p.QuotedAt.IsZero() {
		t.Fatalf("honesty fields missing: %+v", p)
	}
	if p.Estimators["spread"] != "lp_markup_max" {
		t.Fatalf("estimator note=%v", p.Estimators)
	}
}

// RAW_SPREAD_COMMISSION: the tier charge lands on the commission leg.
func TestCostPreview_RawSpreadCommission(t *testing.T) {
	svc := costSvc(t, CostsDeps{
		FeeModels: fakeFeeModels{settlement.FeeModelRawSpreadCommission},
		Commissions: fakeCommStore{
			vol: costD("0"),
			tiers: []settlement.CommissionTier{
				{TierName: "base", RatePerMillion: costD("20")},
			},
		},
		Spread: fakeSpread{costD("1"), "lp_markup_max"},
		Now:    func() time.Time { return time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC) },
	})
	p, err := svc.Preview(context.Background(), 42, "EUR/USD", "SELL", costD("100000"))
	if err != nil {
		t.Fatal(err)
	}
	// $20/M on $110k notional = 2.20; spread 11.
	if !p.Commission.Equal(costD("2.2")) {
		t.Fatalf("commission=%s want 2.2", p.Commission)
	}
	if !p.SpreadCost.Equal(costD("11")) {
		t.Fatalf("spread=%s want 11", p.SpreadCost)
	}
	if !p.TotalQuote.Equal(costD("13.2")) {
		t.Fatalf("total=%s want 13.2", p.TotalQuote)
	}
}

// Financing leg: a swap rate produces the one-night indicative charge.
func TestCostPreview_FinancingLeg(t *testing.T) {
	svc := costSvc(t, CostsDeps{
		Spread: fakeSpread{costD("0"), "none"},
		Swap: fakeSwapSrc{ok: true, rate: settlement.SwapRate{
			InstrumentID: 9, LongPoints: costD("0.5"),
			EffectiveDate: time.Date(2026, 9, 29, 0, 0, 0, 0, time.UTC),
		}},
		Now: func() time.Time { return time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC) },
	})
	p, err := svc.Preview(context.Background(), 42, "EUR/USD", "BUY", costD("100000"))
	if err != nil {
		t.Fatal(err)
	}
	if p.Financing.IsZero() {
		t.Fatalf("financing leg empty: %+v notes=%v", p, p.Estimators)
	}
	if p.Estimators["financing"] == "no_swap_rate" {
		t.Fatal("swap rate not applied")
	}
}

// Cross-currency account: conversion cost + converted total populated.
func TestCostPreview_CrossCurrencyAccount(t *testing.T) {
	svc := costSvc(t, CostsDeps{
		Spread:   fakeSpread{costD("2"), "lp_markup_max"},
		Accounts: fakeAcctMeta{&funding.AccountMeta{ID: 42, BaseCurrency: "EUR"}},
		Conv:     fakeConv{funding.MidRate{Rate: costD("0.91"), Source: "fx_cross_usd"}},
		Now:      func() time.Time { return time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC) },
	})
	p, err := svc.Preview(context.Background(), 42, "EUR/USD", "BUY", costD("100000"))
	if err != nil {
		t.Fatal(err)
	}
	if p.RateUsed == nil || !p.RateUsed.Equal(costD("0.91")) {
		t.Fatalf("rate=%v", p.RateUsed)
	}
	if p.TotalAccount == nil {
		t.Fatal("total_account missing for cross-currency account")
	}
	// totalQuote = 22 + conversion(22×50bps=0.11) = 22.11 → ×0.91 ≈ 20.1201
	if !p.Conversion.Equal(costD("0.11")) {
		t.Fatalf("conversion=%s want 0.11", p.Conversion)
	}
	if !p.TotalAccount.Equal(costD("20.1201")) {
		t.Fatalf("total_account=%s want 20.1201", p.TotalAccount)
	}
}

// Fail-closed: no mark → PRICE_ORACLE_UNAVAILABLE; stale →
// MARK_PRICE_STALE; bad side/qty → INVALID_REQUEST.
func TestCostPreview_FailClosed(t *testing.T) {
	svc := costSvc(t, CostsDeps{
		Marks: fakeCostMarks{oracle.MarkView{Found: false}},
	})
	if _, err := svc.Preview(context.Background(), 42, "EUR/USD", "BUY", costD("1")); err == nil {
		t.Fatal("missing mark accepted")
	}
	svc2 := costSvc(t, CostsDeps{
		Marks: fakeCostMarks{oracle.MarkView{Price: costD("1.1"), Found: true, Stale: true}},
	})
	if _, err := svc2.Preview(context.Background(), 42, "EUR/USD", "BUY", costD("1")); err == nil {
		t.Fatal("stale mark accepted")
	}
	svc3 := costSvc(t, CostsDeps{})
	if _, err := svc3.Preview(context.Background(), 42, "EUR/USD", "HOLD", costD("1")); err == nil {
		t.Fatal("bad side accepted")
	}
	if _, err := svc3.Preview(context.Background(), 42, "EUR/USD", "BUY", costD("-1")); err == nil {
		t.Fatal("non-positive qty accepted")
	}
}

// Annual ex-post: ledger rows bucket correctly; spread is modeled;
// net-cost totals reconcile.
func TestCostAnnual_ReconcilesLedgerRows(t *testing.T) {
	svc := costSvc(t, CostsDeps{
		Spread: fakeSpread{costD("1"), "lp_markup_max"},
		Activity: fakeActivity{[]CostActivityRow{
			{Currency: "USD", EntryType: "FEE", Direction: "CREDIT", Class: "COMMISSION", Total: costD("10")},
			{Currency: "USD", EntryType: "FEE", Direction: "CREDIT", Class: "CONVERSION", Total: costD("3")},
			{Currency: "USD", EntryType: "FEE", Direction: "DEBIT", Class: "REBATE", Total: costD("2")},
			{Currency: "USD", EntryType: "ROLLOVER", Direction: "CREDIT", Class: "SWAP", Total: costD("5")},
			{Currency: "USD", EntryType: "ROLLOVER", Direction: "DEBIT", Class: "SWAP", Total: costD("1.5")},
			{Currency: "USD", EntryType: "TRADE_FILL", Direction: "DEBIT", Class: "", Total: costD("999")},
		}},
		Trades: fakeFills{[]CostTradeFill{
			{InstrumentID: 9, Symbol: "EUR/USD", Quantity: costD("100000"),
				Price: costD("1.10"), QuoteCcy: "USD"},
		}},
		Now: func() time.Time { return time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC) },
	})
	d, err := svc.Annual(context.Background(), 42, 2026)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Buckets) != 1 {
		t.Fatalf("buckets=%+v", d.Buckets)
	}
	b := d.Buckets[0]
	if !b.Commissions.Equal(costD("10")) || !b.ConversionCost.Equal(costD("3")) ||
		!b.Rebates.Equal(costD("2")) || !b.SwapPaid.Equal(costD("5")) ||
		!b.SwapReceived.Equal(costD("1.5")) {
		t.Fatalf("bucket=%+v", b)
	}
	// spread: 110000 × 1bps = 11
	if !b.SpreadCost.Equal(costD("11")) || !b.GrossNotional.Equal(costD("110000")) {
		t.Fatalf("spread=%s notional=%s", b.SpreadCost, b.GrossNotional)
	}
	// total = 11 + 10 + 5 + 3 − 2 = 27
	if !b.Total.Equal(costD("27")) {
		t.Fatalf("total=%s want 27", b.Total)
	}
	if d.Inducement != NoInducementStatement || len(d.Notes) == 0 {
		t.Fatal("disclosure metadata missing")
	}
}

// Annual rejects garbage input and requires the activity/trades seams.
func TestCostAnnual_Validation(t *testing.T) {
	svc := costSvc(t, CostsDeps{
		Activity: fakeActivity{}, Trades: fakeFills{},
		Now: func() time.Time { return time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC) },
	})
	if _, err := svc.Annual(context.Background(), 0, 2026); err == nil {
		t.Fatal("account 0 accepted")
	}
	if _, err := svc.Annual(context.Background(), 42, 1900); err == nil {
		t.Fatal("year 1900 accepted")
	}
	if _, err := svc.Annual(context.Background(), 42, 2027); err == nil {
		t.Fatal("future year accepted")
	}
	bare := costSvc(t, CostsDeps{})
	if _, err := bare.Annual(context.Background(), 42, 2026); err == nil {
		t.Fatal("missing activity/trades seams accepted")
	}
}
