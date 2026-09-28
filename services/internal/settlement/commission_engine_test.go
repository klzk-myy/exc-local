// Unit tests for Task 3.3.13 (dual fee model, tiers, monthly volume) and
// Task 3.3.17 (negative maker rebate GL). All seams are faked — no DB.
package settlement

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"exchange/internal/ledger"
)

// ---------------------------------------------------------------------------
// Fakes
// ---------------------------------------------------------------------------

type fakeCommissionStore struct {
	tiers    []CommissionTier
	volumes  map[string]decimal.Decimal // "acct|YYYY-MM-01" → USD
	recorded []struct {
		acct  int64
		month time.Time
		delta decimal.Decimal
	}
	volErr error
}

func monthKey(acct int64, m time.Time) string {
	return fmt.Sprintf("%d|%s", acct, m.UTC().Format("2006-01"))
}

func (f *fakeCommissionStore) LoadCommissionTiers(context.Context) ([]CommissionTier, error) {
	return f.tiers, nil
}

func (f *fakeCommissionStore) MonthlyVolumeUSD(_ context.Context, accountID int64, month time.Time) (decimal.Decimal, error) {
	if f.volErr != nil {
		return decimal.Zero, f.volErr
	}
	if f.volumes == nil {
		return decimal.Zero, nil
	}
	return f.volumes[monthKey(accountID, month)], nil
}

func (f *fakeCommissionStore) RecordFillVolumeUSD(_ context.Context, accountID int64, month time.Time, deltaUSD decimal.Decimal) error {
	f.recorded = append(f.recorded, struct {
		acct  int64
		month time.Time
		delta decimal.Decimal
	}{accountID, month, deltaUSD})
	return nil
}

func testTiers() []CommissionTier {
	return []CommissionTier{
		{TierID: 1, TierName: "STANDARD", MinMonthlyVolume: decimal.Zero,
			RatePerLot: decimal.NewFromInt(7), RatePerMillion: decimal.NewFromInt(70)},
		{TierID: 2, TierName: "ACTIVE", MinMonthlyVolume: decimal.NewFromInt(1_000_000),
			RatePerLot: decimal.NewFromInt(6), RatePerMillion: decimal.NewFromInt(60)},
		{TierID: 3, TierName: "PRIME", MinMonthlyVolume: decimal.NewFromInt(500_000_000),
			RatePerLot: decimal.NewFromInt(3), RatePerMillion: decimal.NewFromInt(30)},
	}
}

func baseFill() CommissionFill {
	return CommissionFill{
		AccountID: 42, TradeID: 9001, InstrumentID: 1, Symbol: "EUR/USD",
		Role: FillRoleTaker, Quantity: decimal.NewFromInt(100_000),
		Price: decimal.RequireFromString("1.20"), LotSize: decimal.NewFromInt(100_000),
		QuoteCurrency: "USD", MidPrice: decimal.RequireFromString("1.1999"),
		PostedBy: "test",
	}
}

func newEngine(t *testing.T, model FeeModel, store *fakeCommissionStore) *CommissionEngine {
	t.Helper()
	e, err := NewCommissionEngine(
		StaticFeeModelSource{Model: model}, store,
		StaticUsdConverter{Rates: map[string]decimal.Decimal{}}, nil)
	if err != nil {
		t.Fatalf("engine: %v", err)
	}
	return e
}

// requireCode (ledger_service_test.go) and d (position_service_test.go)
// are shared package test helpers — reused here.

// ---------------------------------------------------------------------------
// Task 3.3.13 — dual fee model
// ---------------------------------------------------------------------------

// SPREAD_MARKUP: fee implicit in the spread — zero commission, no journal,
// effective_spread still disclosed.
func TestAssessSpreadMarkup(t *testing.T) {
	store := &fakeCommissionStore{tiers: testTiers()}
	e := newEngine(t, FeeModelSpreadMarkup, store)

	a, err := e.Assess(context.Background(), baseFill())
	if err != nil {
		t.Fatalf("assess: %v", err)
	}
	if !a.Commission.IsZero() {
		t.Fatalf("markup model charged commission %s", a.Commission)
	}
	if len(a.Journals) != 0 {
		t.Fatalf("markup model emitted %d journals", len(a.Journals))
	}
	if len(store.recorded) != 0 {
		t.Fatalf("markup model recorded monthly volume")
	}
	// effective_spread = 2 × |1.20 − 1.1999| = 0.0002
	want := decimal.RequireFromString("0.0002")
	if !a.EffectiveSpread.Equal(want) {
		t.Fatalf("effective_spread %s != %s", a.EffectiveSpread, want)
	}
	view := a.ExecutionReportView()
	if !view.Commission.IsZero() || !view.EffectiveSpread.Equal(want) {
		t.Fatalf("report view wrong: %+v", view)
	}
}

// RAW_SPREAD_COMMISSION: explicit commission = per-lot + per-million legs,
// journal debits client liability and credits COMMISSION_REVENUE (distinct
// from 4010 spread revenue).
func TestAssessRawSpreadCommission(t *testing.T) {
	store := &fakeCommissionStore{tiers: testTiers()}
	e := newEngine(t, FeeModelRawSpreadCommission, store)

	f := baseFill()
	a, err := e.Assess(context.Background(), f)
	if err != nil {
		t.Fatalf("assess: %v", err)
	}
	// qty=100k (1 lot) × $7/lot + notional $120k → 0.12M × $70 = $8.40 → total 15.40
	want := decimal.RequireFromString("15.4")
	if !a.Commission.Equal(want) {
		t.Fatalf("commission %s != %s", a.Commission, want)
	}
	if !a.TierResolved || a.Tier.TierName != "STANDARD" {
		t.Fatalf("tier resolution wrong: %+v", a.Tier)
	}
	if len(a.Journals) != 1 {
		t.Fatalf("want 1 journal, got %d", len(a.Journals))
	}
	j := a.Journals[0]
	if err := j.Validate(); err != nil {
		t.Fatalf("journal invalid: %v", err)
	}
	if err := j.ValidateAccounts(ledger.DefaultChart()); err != nil {
		t.Fatalf("journal accounts unresolved: %v", err)
	}
	if j.Lines[0].AccountCode != ledger.CustomerLiability("USD") || !j.Lines[0].Debit.Equal(want) {
		t.Fatalf("debit leg wrong: %+v", j.Lines[0])
	}
	if j.Lines[1].AccountCode != ledger.CommissionRevenue("USD") || !j.Lines[1].Credit.Equal(want) {
		t.Fatalf("credit leg wrong: %+v", j.Lines[1])
	}
	if j.Lines[1].AccountCode == ledger.TradingFeeRevenue("USD") {
		t.Fatalf("commission must be distinct from spread revenue")
	}
	// Wallet effect debits the charge.
	if len(j.Effects) != 1 || !j.Effects[0].AvailableDelta.Equal(want.Neg()) {
		t.Fatalf("effect wrong: %+v", j.Effects)
	}
	// Monthly volume recorded AFTER tier resolution (USD notional 120k).
	if len(store.recorded) != 1 || !store.recorded[0].delta.Equal(decimal.NewFromInt(120_000)) {
		t.Fatalf("volume record wrong: %+v", store.recorded)
	}
	// Signed fee = commission (no rebate on a taker fill).
	if v := a.ExecutionReportView(); !v.SignedFee.Equal(want) {
		t.Fatalf("signed fee %s != %s", v.SignedFee, want)
	}
}

// Tier boundary: volume exactly at a threshold qualifies for the higher
// tier; just below stays on the lower tier.
func TestCommissionTierBoundary(t *testing.T) {
	store := &fakeCommissionStore{
		tiers: testTiers(),
		volumes: map[string]decimal.Decimal{
			monthKey(42, time.Now()): decimal.NewFromInt(1_000_000),
		},
	}
	e := newEngine(t, FeeModelRawSpreadCommission, store)
	a, err := e.Assess(context.Background(), baseFill())
	if err != nil {
		t.Fatalf("assess: %v", err)
	}
	if a.Tier.TierName != "ACTIVE" {
		t.Fatalf("volume at bound must qualify ACTIVE tier, got %s", a.Tier.TierName)
	}
	// 1 lot × $6 + 0.12M × $60 = 6 + 7.20 = 13.20
	if want := decimal.RequireFromString("13.2"); !a.Commission.Equal(want) {
		t.Fatalf("commission %s != %s", a.Commission, want)
	}

	// Just below the boundary stays STANDARD.
	store.volumes[monthKey(42, time.Now())] = decimal.NewFromInt(999_999)
	a, err = e.Assess(context.Background(), baseFill())
	if err != nil {
		t.Fatalf("assess: %v", err)
	}
	if a.Tier.TierName != "STANDARD" {
		t.Fatalf("below bound must stay STANDARD, got %s", a.Tier.TierName)
	}
}

// Mid-month model switch: an account assessed under RAW charges commission;
// the same account shape under markup charges none (the profile read, not
// the fill, decides).
func TestAccountTypeSwitchMidMonth(t *testing.T) {
	store := &fakeCommissionStore{tiers: testTiers()}
	raw := newEngine(t, FeeModelRawSpreadCommission, store)
	a, err := raw.Assess(context.Background(), baseFill())
	if err != nil {
		t.Fatalf("raw assess: %v", err)
	}
	if !a.Commission.IsPositive() {
		t.Fatalf("raw model must charge")
	}
	markup := newEngine(t, FeeModelSpreadMarkup, store)
	a, err = markup.Assess(context.Background(), baseFill())
	if err != nil {
		t.Fatalf("markup assess: %v", err)
	}
	if !a.Commission.IsZero() || len(a.Journals) != 0 {
		t.Fatalf("markup model after switch must charge nothing")
	}
}

// Unknown fee model fails closed.
func TestAssessUnknownModel(t *testing.T) {
	store := &fakeCommissionStore{tiers: testTiers()}
	e := newEngine(t, "BOGUS", store)
	_, err := e.Assess(context.Background(), baseFill())
	requireCode(t, err, CodeCommissionConfigInvalid)
}

// ---------------------------------------------------------------------------
// Task 3.3.17 — negative maker fee (rebate) GL
// ---------------------------------------------------------------------------

// VIP 4+ maker fill with negative maker_bps credits cash: DR
// 5100_LIQUIDITY_REBATE_EXPENSE, CR 2100_CLIENT_COLLATERAL, wallet +rebate.
func TestMakerRebateJournal(t *testing.T) {
	store := &fakeCommissionStore{tiers: testTiers()}
	e := newEngine(t, FeeModelSpreadMarkup, store)

	f := baseFill()
	f.Role, f.VipTier = FillRoleMaker, 5
	f.MakerBps = decimal.RequireFromString("-0.1") // −0.1 bps of notional
	a, err := e.Assess(context.Background(), f)
	if err != nil {
		t.Fatalf("assess: %v", err)
	}
	// rebate = 0.1/10⁴ × 120,000 = 1.20 USD
	want := decimal.RequireFromString("1.2")
	if !a.MakerRebate.Equal(want) {
		t.Fatalf("rebate %s != %s", a.MakerRebate, want)
	}
	if len(a.Journals) != 1 {
		t.Fatalf("want 1 rebate journal, got %d", len(a.Journals))
	}
	j := a.Journals[0]
	if err := j.Validate(); err != nil {
		t.Fatalf("journal invalid: %v", err)
	}
	if err := j.ValidateAccounts(ledger.DefaultChart()); err != nil {
		t.Fatalf("accounts unresolved: %v", err)
	}
	if j.Lines[0].AccountCode != ledger.LiquidityRebateExpense("USD") || !j.Lines[0].Debit.Equal(want) {
		t.Fatalf("expense debit leg wrong: %+v", j.Lines[0])
	}
	if j.Lines[1].AccountCode != ledger.ClientCollateral("USD") || !j.Lines[1].Credit.Equal(want) {
		t.Fatalf("client credit leg wrong: %+v", j.Lines[1])
	}
	if !j.Effects[0].AvailableDelta.Equal(want) {
		t.Fatalf("wallet credit wrong: %+v", j.Effects[0])
	}
	// Report surfaces a negative net fee.
	if v := a.ExecutionReportView(); !v.SignedFee.Equal(want.Neg()) {
		t.Fatalf("signed fee %s != %s (rebate must surface negative)", v.SignedFee, want.Neg())
	}
}

// Raw model + negative maker rate: commission AND rebate journals post
// separately; signed fee nets them.
func TestMakerRebateAlongsideCommission(t *testing.T) {
	store := &fakeCommissionStore{tiers: testTiers()}
	e := newEngine(t, FeeModelRawSpreadCommission, store)
	f := baseFill()
	f.Role, f.VipTier = FillRoleMaker, 9
	f.MakerBps = decimal.RequireFromString("-0.5")
	a, err := e.Assess(context.Background(), f)
	if err != nil {
		t.Fatalf("assess: %v", err)
	}
	if len(a.Journals) != 2 {
		t.Fatalf("want commission+rebate journals, got %d", len(a.Journals))
	}
	// rebate = 0.5bps × 120k = 6.00; commission 15.40 → signed fee 9.40
	if want := decimal.RequireFromString("9.4"); !a.ExecutionReportView().SignedFee.Equal(want) {
		t.Fatalf("signed fee %s != %s", a.ExecutionReportView().SignedFee, want)
	}
}

// Negative maker rate below VIP 4 is configuration corruption — fail closed.
func TestNegativeMakerBelowVip4Rejected(t *testing.T) {
	store := &fakeCommissionStore{tiers: testTiers()}
	e := newEngine(t, FeeModelSpreadMarkup, store)
	f := baseFill()
	f.Role, f.VipTier = FillRoleMaker, 3
	f.MakerBps = decimal.RequireFromString("-0.1")
	_, err := e.Assess(context.Background(), f)
	requireCode(t, err, CodeCommissionConfigInvalid)
}

// Zero-commission tier still records volume and posts nothing.
func TestZeroCommissionTier(t *testing.T) {
	store := &fakeCommissionStore{tiers: []CommissionTier{
		{TierID: 1, TierName: "FREE", MinMonthlyVolume: decimal.Zero},
	}}
	e := newEngine(t, FeeModelRawSpreadCommission, store)
	a, err := e.Assess(context.Background(), baseFill())
	if err != nil {
		t.Fatalf("assess: %v", err)
	}
	if !a.Commission.IsZero() || len(a.Journals) != 0 {
		t.Fatalf("zero-rate tier must post no journal")
	}
	if len(store.recorded) != 1 {
		t.Fatalf("volume must still be tracked for tier progression")
	}
}

// Pure helpers.
func TestCommissionTierChargeAndEffectiveSpread(t *testing.T) {
	tier := CommissionTier{RatePerLot: decimal.NewFromInt(5), RatePerMillion: decimal.NewFromInt(50)}
	// 2 lots × 5 + 0.5M × 50 = 10 + 25 = 35
	got := CommissionTierCharge(tier, decimal.NewFromInt(200_000), decimal.NewFromInt(100_000), decimal.NewFromInt(500_000))
	if want := decimal.NewFromInt(35); !got.Equal(want) {
		t.Fatalf("charge %s != %s", got, want)
	}
	if got := EffectiveSpread(decimal.RequireFromString("1.10"), decimal.RequireFromString("1.09")); !got.Equal(decimal.RequireFromString("0.02")) {
		t.Fatalf("effective spread %s", got)
	}
	if _, ok := TierForVolume(testTiers(), decimal.NewFromInt(-1)); ok {
		t.Fatalf("negative volume must match no tier")
	}
}
