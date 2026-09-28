// Unit tests for Task 3.3.14 — multi-asset collateral auto-exchange.
// Pricer and poster are faked; the journal is checked for balance legs,
// currency pick, and the 0.1% buffer.
package settlement

import (
	"context"
	"fmt"
	"testing"

	"github.com/shopspring/decimal"

	"exchange/internal/ledger"
)

type fakeIndexPricer struct {
	rates map[string]decimal.Decimal // "BASE→QUOTE"
	errs  map[string]error
}

func (f *fakeIndexPricer) IndexRate(_ context.Context, base, quote string) (decimal.Decimal, error) {
	k := base + "→" + quote
	if err := f.errs[k]; err != nil {
		return decimal.Zero, err
	}
	r, ok := f.rates[k]
	if !ok {
		return decimal.Zero, fmt.Errorf("no rate %s", k)
	}
	return r, nil
}

type fakeAutoExchangePoster struct {
	got []ledger.Journal
	res ledger.PostResult
	err error
}

func (f *fakeAutoExchangePoster) Post(_ context.Context, j ledger.Journal) (ledger.PostResult, error) {
	f.got = append(f.got, j)
	if f.err != nil {
		return ledger.PostResult{}, f.err
	}
	res := f.res
	res.JournalID = 77
	res.Committed = true
	return res, nil
}

func testTrigger() AutoExchangeTrigger {
	return AutoExchangeTrigger{
		AccountID:       7,
		DeficitCurrency: "USD",
		DeficitAmount:   d("100"),
		Collateral: []CollateralSlice{
			{Currency: "EUR", Free: d("1000"), HaircutBps: d("0")},
			{Currency: "JPY", Free: d("100000"), HaircutBps: d("500")},
			{Currency: "USD", Free: d("5"), HaircutBps: d("0")}, // deficit ccy — ignored
		},
		Reason:      "concentration-limit",
		ReferenceID: 555,
		PostedBy:    "collateral-monitor",
	}
}

func newAutoExchange(t *testing.T, p IndexPricer, poster JournalPoster) *AutoExchangeEngine {
	t.Helper()
	e, err := NewAutoExchangeEngine(poster, p)
	if err != nil {
		t.Fatalf("engine: %v", err)
	}
	return e
}

// Highest post-haircut excess wins; conversion covers deficit + 0.1%.
func TestSettlePicksHighestPostHaircutExcess(t *testing.T) {
	pricer := &fakeIndexPricer{rates: map[string]decimal.Decimal{
		"EUR→USD": d("1.2"),
		"JPY→USD": d("0.008"),
	}}
	poster := &fakeAutoExchangePoster{}
	e := newAutoExchange(t, pricer, poster)

	res, err := e.Settle(context.Background(), testTrigger())
	if err != nil {
		t.Fatalf("settle: %v", err)
	}
	// Post-haircut excess in USD: EUR 1000×1.00×1.2 = 1200; JPY 100000×0.95×0.008 = 760 → EUR.
	if res.SourceCurrency != "EUR" {
		t.Fatalf("picked %s, want EUR (highest post-haircut excess)", res.SourceCurrency)
	}
	// Target = 100 × 1.001 = 100.10 USD credited.
	if want := d("100.1"); !res.DeficitCredited.Equal(want) {
		t.Fatalf("credited %s != %s", res.DeficitCredited, want)
	}
	// Source debit = 100.1 / 1.2 = 83.41666666..., rounded UP to 8dp.
	if want := d("83.41666667"); !res.SourceAmount.Equal(want) {
		t.Fatalf("source %s != %s", res.SourceAmount, want)
	}
	if !res.IndexRate.Equal(d("1.2")) || res.JournalID != 77 || res.Replayed {
		t.Fatalf("result fields wrong: %+v", res)
	}

	j := poster.got[0]
	if err := j.Validate(); err != nil {
		t.Fatalf("journal invalid: %v", err)
	}
	if err := j.ValidateAccounts(ledger.DefaultChart()); err != nil {
		t.Fatalf("accounts unresolved: %v", err)
	}
	// Source leg: DR 2100_CLIENT_COLLATERAL_EUR / CR 1200_MULTI_CURRENCY_CLEARING_EUR.
	if j.Lines[0].AccountCode != ledger.ClientCollateral("EUR") || !j.Lines[0].Debit.Equal(res.SourceAmount) {
		t.Fatalf("source debit leg wrong: %+v", j.Lines[0])
	}
	if j.Lines[1].AccountCode != ledger.MultiCcyClearing("EUR") || !j.Lines[1].Credit.Equal(res.SourceAmount) {
		t.Fatalf("source credit leg wrong: %+v", j.Lines[1])
	}
	// Deficit leg: DR 1200_MULTI_CURRENCY_CLEARING_USD / CR 2100_CLIENT_COLLATERAL_USD.
	if j.Lines[2].AccountCode != ledger.MultiCcyClearing("USD") || !j.Lines[2].Debit.Equal(res.DeficitCredited) {
		t.Fatalf("deficit debit leg wrong: %+v", j.Lines[2])
	}
	if j.Lines[3].AccountCode != ledger.ClientCollateral("USD") || !j.Lines[3].Credit.Equal(res.DeficitCredited) {
		t.Fatalf("deficit credit leg wrong: %+v", j.Lines[3])
	}
	// Wallet effects: −EUR source, +USD deficit (AllowNegative — the
	// deficit balance is negative by definition).
	var srcEff, defEff *ledger.AccountEffect
	for i := range j.Effects {
		switch j.Effects[i].Currency {
		case "EUR":
			srcEff = &j.Effects[i]
		case "USD":
			defEff = &j.Effects[i]
		}
	}
	if srcEff == nil || !srcEff.AvailableDelta.Equal(res.SourceAmount.Neg()) || srcEff.AllowNegative {
		t.Fatalf("source effect wrong: %+v", srcEff)
	}
	if defEff == nil || !defEff.AvailableDelta.Equal(res.DeficitCredited) || !defEff.AllowNegative {
		t.Fatalf("deficit effect wrong: %+v", defEff)
	}
	if j.IdempotencyKey != "autoex:7:555" {
		t.Fatalf("idempotency key %q", j.IdempotencyKey)
	}
}

// When the best candidate cannot cover the debit, the next-best covers it.
func TestSettleFallsBackToNextCandidate(t *testing.T) {
	pricer := &fakeIndexPricer{rates: map[string]decimal.Decimal{
		"EUR→USD": d("1.2"),
		"JPY→USD": d("0.008"),
	}}
	poster := &fakeAutoExchangePoster{}
	e := newAutoExchange(t, pricer, poster)
	tr := testTrigger()
	tr.Collateral[0].Free = d("10") // EUR excess 12 USD — cannot cover 100.1
	res, err := e.Settle(context.Background(), tr)
	if err != nil {
		t.Fatalf("settle: %v", err)
	}
	if res.SourceCurrency != "JPY" {
		t.Fatalf("fallback should pick JPY, got %s", res.SourceCurrency)
	}
	// source = 100.1 / 0.008 = 12512.5 JPY
	if want := d("12512.5"); !res.SourceAmount.Equal(want) {
		t.Fatalf("source %s != %s", res.SourceAmount, want)
	}
}

// No candidate can cover → coded insufficient error, nothing posts.
func TestSettleInsufficientCollateral(t *testing.T) {
	pricer := &fakeIndexPricer{rates: map[string]decimal.Decimal{
		"EUR→USD": d("1.2"),
		"JPY→USD": d("0.008"),
	}}
	poster := &fakeAutoExchangePoster{}
	e := newAutoExchange(t, pricer, poster)
	tr := testTrigger()
	tr.Collateral[0].Free = d("10")
	tr.Collateral[1].Free = d("1000") // JPY covers only 7.6 USD
	_, err := e.Settle(context.Background(), tr)
	requireCode(t, err, CodeAutoExchangeInsufficient)
	if len(poster.got) != 0 {
		t.Fatalf("nothing must post on insufficient collateral")
	}
}

// Fail-closed inputs.
func TestSettleFailClosed(t *testing.T) {
	pricer := &fakeIndexPricer{rates: map[string]decimal.Decimal{"EUR→USD": d("1.2")}}
	poster := &fakeAutoExchangePoster{}
	e := newAutoExchange(t, pricer, poster)

	// non-positive deficit
	tr := testTrigger()
	tr.DeficitAmount = decimal.Zero
	if _, err := e.Settle(context.Background(), tr); err == nil {
		t.Fatalf("zero deficit must reject")
	}

	// non-positive index rate
	tr = testTrigger()
	tr.Collateral = []CollateralSlice{{Currency: "EUR", Free: d("1000")}}
	pricer.rates["EUR→USD"] = d("-1")
	_, err := e.Settle(context.Background(), tr)
	requireCode(t, err, CodeIndexPriceUnavailable)

	// unpriceable collateral → insufficient (oracle error is collected)
	pricer.rates = map[string]decimal.Decimal{}
	pricer.errs = map[string]error{"EUR→USD": fmt.Errorf("oracle down")}
	tr = testTrigger()
	tr.Collateral = []CollateralSlice{{Currency: "EUR", Free: d("1000")}}
	_, err = e.Settle(context.Background(), tr)
	requireCode(t, err, CodeIndexPriceUnavailable)

	// haircut out of domain
	pricer.rates = map[string]decimal.Decimal{"EUR→USD": d("1.2")}
	pricer.errs = nil
	tr.Collateral = []CollateralSlice{{Currency: "EUR", Free: d("1000"), HaircutBps: d("10001")}}
	_, err = e.Settle(context.Background(), tr)
	requireCode(t, err, CodeAutoExchangeInvalid)
}
