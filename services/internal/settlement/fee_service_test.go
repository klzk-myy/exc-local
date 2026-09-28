package settlement

import (
	"context"
	stderrors "errors"
	"fmt"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"exchange/internal/ledger"
	excerrors "exchange/pkg/errors"
)

// ---------------------------------------------------------------------------
// Fakes
// ---------------------------------------------------------------------------

type fakeFeeStore struct {
	tiers map[int64]FeeTier // account_id → tier
}

func (s *fakeFeeStore) FeeTierFor(_ context.Context, accountID int64) (FeeTier, bool, error) {
	t, ok := s.tiers[accountID]
	return t, ok, nil
}

type fakePoster struct {
	journals []ledger.Journal
	err      error
}

func (p *fakePoster) Post(_ context.Context, j ledger.Journal) (ledger.PostResult, error) {
	if p.err != nil {
		return ledger.PostResult{}, p.err
	}
	p.journals = append(p.journals, j)
	return ledger.PostResult{JournalID: int64(len(p.journals)), Committed: true}, nil
}

func newTestFeeService(t *testing.T, now time.Time) (*FeeService, *fakeFeeStore, *fakePoster) {
	t.Helper()
	store := &fakeFeeStore{tiers: map[int64]FeeTier{}}
	poster := &fakePoster{}
	svc, err := NewFeeService(store, poster, "test", func() time.Time { return now })
	if err != nil {
		t.Fatalf("NewFeeService: %v", err)
	}
	return svc, store, poster
}

// ---------------------------------------------------------------------------
// Pure math — fee = qty × price × bps / 10_000
// ---------------------------------------------------------------------------

func TestComputeFeeFormula(t *testing.T) {
	// 100,000 EUR/USD @ 1.0850 → 108,500 USD notional; 1.5 bps → 16.275.
	got := ComputeFee(decimal.NewFromInt(100_000), decimal.RequireFromString("1.0850"),
		decimal.RequireFromString("1.5"))
	if !got.Equal(decimal.RequireFromString("16.275")) {
		t.Fatalf("fee = %s, want 16.275", got)
	}
	// Zero rate → zero fee.
	if !ComputeFee(decimal.NewFromInt(100), decimal.NewFromInt(1), decimal.Zero).IsZero() {
		t.Fatal("zero rate must yield zero fee")
	}
	// Negative rate → negative fee (rebate tier).
	neg := ComputeFee(decimal.NewFromInt(100_000), decimal.RequireFromString("1.0850"),
		decimal.RequireFromString("-0.25"))
	if !neg.Equal(decimal.RequireFromString("-2.7125")) {
		t.Fatalf("rebate = %s, want -2.7125", neg)
	}
	// Rounding to the 8dp quantum: 1 unit @ 1.00000001 × 3.3333 bps.
	r := ComputeFee(decimal.NewFromInt(1), decimal.RequireFromString("1.00000001"),
		decimal.RequireFromString("3.3333"))
	if !r.Round(8).Equal(r) {
		t.Fatalf("fee %s exceeds 8dp quantum", r)
	}
}

// ---------------------------------------------------------------------------
// Tier resolution — maker/taker, promo windows
// ---------------------------------------------------------------------------

func TestRateBpsMakerTaker(t *testing.T) {
	tier := FeeTier{ID: 1, TierName: "standard",
		MakerBps: decimal.RequireFromString("0.5"),
		TakerBps: decimal.RequireFromString("1.5")}
	mk, promo, err := tier.RateBps(RoleMaker, time.Now())
	if err != nil || promo || !mk.Equal(decimal.RequireFromString("0.5")) {
		t.Fatalf("maker rate = %s promo=%v err=%v", mk, promo, err)
	}
	tk, promo, err := tier.RateBps(RoleTaker, time.Now())
	if err != nil || promo || !tk.Equal(decimal.RequireFromString("1.5")) {
		t.Fatalf("taker rate = %s promo=%v err=%v", tk, promo, err)
	}
	if _, _, err := tier.RateBps("SIDEWAYS", time.Now()); err == nil {
		t.Fatal("invalid role must error")
	}
}

func TestRateBpsPromoWindow(t *testing.T) {
	now := time.Date(2025, 11, 24, 12, 0, 0, 0, time.UTC)
	future := now.Add(time.Hour)
	past := now.Add(-time.Hour)
	promoTaker := decimal.RequireFromString("0.5")
	promoMaker := decimal.RequireFromString("0.1")

	tier := FeeTier{ID: 1, TierName: "standard",
		MakerBps: decimal.RequireFromString("0.5"), TakerBps: decimal.RequireFromString("1.5"),
		PromoUntil: &future, PromoMakerBps: &promoMaker, PromoTakerBps: &promoTaker}

	// Active promo overrides both sides.
	r, promo, err := tier.RateBps(RoleTaker, now)
	if err != nil || !promo || !r.Equal(promoTaker) {
		t.Fatalf("active promo taker = %s promo=%v err=%v", r, promo, err)
	}
	r, promo, _ = tier.RateBps(RoleMaker, now)
	if !promo || !r.Equal(promoMaker) {
		t.Fatalf("active promo maker = %s promo=%v", r, promo)
	}

	// Boundary: promo_until == now is NOT active (spec §8.5: promo_until > now()).
	tier.PromoUntil = &now
	r, promo, _ = tier.RateBps(RoleTaker, now)
	if promo || !r.Equal(decimal.RequireFromString("1.5")) {
		t.Fatalf("boundary promo = rate %s promo=%v, want tier 1.5", r, promo)
	}

	// Expired promo falls back to the tier rate.
	tier.PromoUntil = &past
	r, promo, _ = tier.RateBps(RoleTaker, now)
	if promo || !r.Equal(decimal.RequireFromString("1.5")) {
		t.Fatalf("expired promo = rate %s promo=%v, want tier 1.5", r, promo)
	}
}

// §8.5: promo-derived rates never below zero — a negative promo clamps.
func TestRateBpsNegativePromoClamps(t *testing.T) {
	now := time.Now()
	future := now.Add(time.Hour)
	neg := decimal.RequireFromString("-0.5")
	tier := FeeTier{MakerBps: decimal.RequireFromString("0.5"),
		TakerBps:   decimal.RequireFromString("1.5"),
		PromoUntil: &future, PromoTakerBps: &neg}
	r, promo, err := tier.RateBps(RoleTaker, now)
	if err != nil || !promo || !r.IsZero() {
		t.Fatalf("negative promo = rate %s promo=%v err=%v, want 0/true/nil", r, promo, err)
	}
}

// Partial promo (only taker covered) — maker falls back to tier rate.
func TestRateBpsPartialPromo(t *testing.T) {
	now := time.Now()
	future := now.Add(time.Hour)
	promoTaker := decimal.RequireFromString("0.5")
	tier := FeeTier{MakerBps: decimal.RequireFromString("0.5"),
		TakerBps:   decimal.RequireFromString("1.5"),
		PromoUntil: &future, PromoTakerBps: &promoTaker} // PromoMakerBps nil
	r, promo, _ := tier.RateBps(RoleMaker, now)
	if promo || !r.Equal(decimal.RequireFromString("0.5")) {
		t.Fatalf("uncovered maker side = rate %s promo=%v, want tier 0.5", r, promo)
	}
}

// ---------------------------------------------------------------------------
// Quote + ApplyFee
// ---------------------------------------------------------------------------

func stdTier() FeeTier {
	return FeeTier{ID: 7, TierName: "standard",
		MakerBps: decimal.RequireFromString("0.5"), TakerBps: decimal.RequireFromString("1.5")}
}

func TestApplyFeeHappyPath(t *testing.T) {
	now := time.Date(2025, 11, 24, 12, 0, 0, 0, time.UTC)
	svc, store, poster := newTestFeeService(t, now)
	store.tiers[11] = stdTier()

	q, res, applied, err := svc.ApplyFee(context.Background(), FeeParams{
		TradeID: 42, AccountID: 11, Role: RoleTaker,
		Quantity: decimal.NewFromInt(100_000), Price: decimal.RequireFromString("1.0850"),
		FeeCurrency: "USD",
	})
	if err != nil || !applied {
		t.Fatalf("ApplyFee err=%v applied=%v", err, applied)
	}
	if !q.Amount.Equal(decimal.RequireFromString("16.275")) || q.PromoApplied {
		t.Fatalf("quote = %+v", q)
	}
	if res.JournalID != 1 || len(poster.journals) != 1 {
		t.Fatalf("poster got %d journals", len(poster.journals))
	}
	j := poster.journals[0]
	if j.EntryType != ledger.EntryFee || j.ReferenceID != 42 {
		t.Fatalf("journal type=%s ref=%d", j.EntryType, j.ReferenceID)
	}
	if j.IdempotencyKey != "fee:42:11:TAKER" {
		t.Fatalf("idempotency key %q", j.IdempotencyKey)
	}
	// Journal passes structural + chart validation (spec §5.21).
	if err := j.Validate(); err != nil {
		t.Fatalf("journal Validate: %v", err)
	}
	if err := j.ValidateAccounts(ledger.DefaultChart()); err != nil {
		t.Fatalf("journal ValidateAccounts: %v", err)
	}
	// Fee debit: customer liability debited, trading-fee revenue credited.
	debit, credit := j.Lines[0], j.Lines[1]
	if debit.AccountCode != "2010_CUSTOMER_LIABILITY_USD" ||
		!debit.Debit.Equal(q.Amount) || !debit.Credit.IsZero() {
		t.Fatalf("debit line = %+v", debit)
	}
	if credit.AccountCode != "4010_TRADING_FEE_REVENUE_USD" ||
		!credit.Credit.Equal(q.Amount) || !credit.Debit.IsZero() {
		t.Fatalf("credit line = %+v", credit)
	}
	if len(j.Effects) != 1 || j.Effects[0].AccountID != 11 ||
		!j.Effects[0].AvailableDelta.Equal(q.Amount.Neg()) {
		t.Fatalf("effects = %+v", j.Effects)
	}
}

// Maker rate applied to the maker side.
func TestApplyFeeMakerRate(t *testing.T) {
	svc, store, poster := newTestFeeService(t, time.Now())
	store.tiers[11] = stdTier()
	q, _, applied, err := svc.ApplyFee(context.Background(), FeeParams{
		TradeID: 43, AccountID: 11, Role: RoleMaker,
		Quantity: decimal.NewFromInt(100_000), Price: decimal.RequireFromString("1.0850"),
		FeeCurrency: "USD",
	})
	if err != nil || !applied {
		t.Fatalf("ApplyFee: %v", err)
	}
	// 108,500 × 0.5 bps / 10_000 = 5.425
	if !q.Amount.Equal(decimal.RequireFromString("5.425")) {
		t.Fatalf("maker fee = %s, want 5.425", q.Amount)
	}
	if poster.journals[0].IdempotencyKey != "fee:43:11:MAKER" {
		t.Fatalf("key = %q", poster.journals[0].IdempotencyKey)
	}
}

// Active promo rate flows through Quote → ApplyFee.
func TestApplyFeePromoRate(t *testing.T) {
	now := time.Now()
	future := now.Add(time.Hour)
	promoTaker := decimal.RequireFromString("0.5")
	svc, store, _ := newTestFeeService(t, now)
	tier := stdTier()
	tier.PromoUntil = &future
	tier.PromoTakerBps = &promoTaker
	store.tiers[11] = tier
	q, _, applied, err := svc.ApplyFee(context.Background(), FeeParams{
		TradeID: 44, AccountID: 11, Role: RoleTaker,
		Quantity: decimal.NewFromInt(100_000), Price: decimal.RequireFromString("1.0850"),
		FeeCurrency: "USD",
	})
	if err != nil || !applied || !q.PromoApplied {
		t.Fatalf("ApplyFee err=%v applied=%v promo=%v", err, applied, q.PromoApplied)
	}
	if !q.Amount.Equal(decimal.RequireFromString("5.425")) {
		t.Fatalf("promo fee = %s, want 5.425", q.Amount)
	}
}

// Zero-fee tier: no journal posted, applied=false.
func TestApplyFeeZeroTierNoPosting(t *testing.T) {
	svc, store, poster := newTestFeeService(t, time.Now())
	store.tiers[11] = FeeTier{ID: 9, TierName: "free",
		MakerBps: decimal.Zero, TakerBps: decimal.Zero}
	_, _, applied, err := svc.ApplyFee(context.Background(), FeeParams{
		TradeID: 45, AccountID: 11, Role: RoleTaker,
		Quantity: decimal.NewFromInt(1000), Price: decimal.NewFromInt(1), FeeCurrency: "USD",
	})
	if err != nil {
		t.Fatalf("ApplyFee: %v", err)
	}
	if applied || len(poster.journals) != 0 {
		t.Fatalf("zero fee posted %d journals (applied=%v)", len(poster.journals), applied)
	}
}

// Negative maker rate → rebate: revenue debited, account credited.
func TestApplyFeeRebate(t *testing.T) {
	svc, store, poster := newTestFeeService(t, time.Now())
	store.tiers[11] = FeeTier{ID: 10, TierName: "vip-rebate",
		MakerBps: decimal.RequireFromString("-0.25"), TakerBps: decimal.RequireFromString("1.5")}
	q, _, applied, err := svc.ApplyFee(context.Background(), FeeParams{
		TradeID: 46, AccountID: 11, Role: RoleMaker,
		Quantity: decimal.NewFromInt(100_000), Price: decimal.RequireFromString("1.0850"),
		FeeCurrency: "USD",
	})
	if err != nil || !applied {
		t.Fatalf("ApplyFee: %v applied=%v", err, applied)
	}
	if !q.IsRebate() || !q.Amount.Equal(decimal.RequireFromString("-2.7125")) {
		t.Fatalf("rebate quote = %+v", q)
	}
	j := poster.journals[0]
	if err := j.Validate(); err != nil {
		t.Fatalf("rebate journal Validate: %v", err)
	}
	if j.Lines[0].AccountCode != "4010_TRADING_FEE_REVENUE_USD" || !j.Lines[0].Debit.IsPositive() {
		t.Fatalf("rebate debit line = %+v", j.Lines[0])
	}
	if j.Lines[1].AccountCode != "2010_CUSTOMER_LIABILITY_USD" || !j.Lines[1].Credit.IsPositive() {
		t.Fatalf("rebate credit line = %+v", j.Lines[1])
	}
	if !j.Effects[0].AvailableDelta.IsPositive() {
		t.Fatalf("rebate effect = %+v, want positive delta", j.Effects[0])
	}
}

// Missing tier → FEE_TIER_NOT_FOUND (fail-closed, never fee-free).
func TestApplyFeeNoTierFailsClosed(t *testing.T) {
	svc, _, poster := newTestFeeService(t, time.Now())
	_, _, _, err := svc.ApplyFee(context.Background(), FeeParams{
		TradeID: 47, AccountID: 99, Role: RoleTaker,
		Quantity: decimal.NewFromInt(1000), Price: decimal.NewFromInt(1), FeeCurrency: "USD",
	})
	var ce *excerrors.Error
	if !stderrors.As(err, &ce) || ce.Code != CodeFeeTierNotFound {
		t.Fatalf("err = %v, want %s", err, CodeFeeTierNotFound)
	}
	if len(poster.journals) != 0 {
		t.Fatal("journal posted despite missing tier")
	}
}

// Minimum fee floors positive fees only.
func TestQuoteMinFee(t *testing.T) {
	svc, store, _ := newTestFeeService(t, time.Now())
	store.tiers[11] = stdTier()
	q, err := svc.Quote(context.Background(), FeeParams{
		AccountID: 11, Role: RoleTaker,
		Quantity: decimal.NewFromInt(10), Price: decimal.RequireFromString("1.0"),
		FeeCurrency: "USD", MinFee: decimal.RequireFromString("0.50"),
	})
	if err != nil {
		t.Fatalf("Quote: %v", err)
	}
	if !q.Amount.Equal(decimal.RequireFromString("0.50")) {
		t.Fatalf("min-fee quote = %s, want 0.50", q.Amount)
	}
}

// Poster error propagates (caller decides retry — idempotency key makes
// retry safe).
func TestApplyFeePosterError(t *testing.T) {
	svc, store, poster := newTestFeeService(t, time.Now())
	store.tiers[11] = stdTier()
	poster.err = fmt.Errorf("boom")
	_, _, applied, err := svc.ApplyFee(context.Background(), FeeParams{
		TradeID: 48, AccountID: 11, Role: RoleTaker,
		Quantity: decimal.NewFromInt(1000), Price: decimal.NewFromInt(1), FeeCurrency: "USD",
	})
	if err == nil || applied {
		t.Fatalf("expected post error, got applied=%v err=%v", applied, err)
	}
}
