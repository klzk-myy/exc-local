package settlement

import (
	"context"
	"encoding/json"
	stderrors "errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	excerrors "exchange/pkg/errors"
)

// ---------------------------------------------------------------------------
// Fixtures
// ---------------------------------------------------------------------------

var testInstruments = StaticInstruments{
	"EUR/USD": {Symbol: "EUR/USD", Base: "EUR", Quote: "USD", DecimalPlaces: 5},
	"USD/JPY": {Symbol: "USD/JPY", Base: "USD", Quote: "JPY", DecimalPlaces: 3},
	"EUR/GBP": {Symbol: "EUR/GBP", Base: "EUR", Quote: "GBP", DecimalPlaces: 5},
	"GBP/JPY": {Symbol: "GBP/JPY", Base: "GBP", Quote: "JPY", DecimalPlaces: 3},
	"EUR/JPY": {Symbol: "EUR/JPY", Base: "EUR", Quote: "JPY", DecimalPlaces: 3},
	"USD/CHF": {Symbol: "USD/CHF", Base: "USD", Quote: "CHF", DecimalPlaces: 5},
}

func seedPrices(t *testing.T, p *LastTradePriceProvider, at time.Time) {
	t.Helper()
	for sym, px := range map[string]string{
		"EUR/USD": "1.10",
		"USD/JPY": "150.00",
		"EUR/GBP": "0.85",
		"GBP/JPY": "190.00",
		"GBP/USD": "1.25",
		"USD/CHF": "0.90",
	} {
		if err := p.SetLastTrade(sym, decimal.RequireFromString(px), at); err != nil {
			t.Fatalf("seed %s: %v", sym, err)
		}
	}
}

func newCalc(t *testing.T, prices PriceProvider, cache PipCache) *PipCalculator {
	t.Helper()
	c, err := NewPipCalculator(testInstruments, prices, cache)
	if err != nil {
		t.Fatalf("NewPipCalculator: %v", err)
	}
	return c
}

func assertPip(t *testing.T, res PipValueResult, want string) {
	t.Helper()
	w := decimal.RequireFromString(want)
	if !res.ValueAccountCcy.Equal(w) {
		t.Fatalf("pip value = %s, want %s (res %+v)", res.ValueAccountCcy, w, res)
	}
}

// ---------------------------------------------------------------------------
// Direct / indirect / cross pairs
// ---------------------------------------------------------------------------

func TestPipValueDirectPair(t *testing.T) {
	now := time.Now()
	prices := NewLastTradePriceProvider()
	seedPrices(t, prices, now)
	calc := newCalc(t, prices, nil)

	// EUR/USD, USD account, 1 standard lot: 100000 × 0.0001 = $10/pip.
	res, err := calc.PipValue(context.Background(), "EUR/USD", decimal.NewFromInt(1), "USD")
	if err != nil {
		t.Fatalf("PipValue: %v", err)
	}
	assertPip(t, res, "10")
	if res.PipSize.String() != "0.0001" || res.ConversionPair != "" {
		t.Fatalf("direct pair metadata wrong: %+v", res)
	}
}

func TestPipValueIndirectPairAccountIsBase(t *testing.T) {
	now := time.Now()
	prices := NewLastTradePriceProvider()
	seedPrices(t, prices, now)
	calc := newCalc(t, prices, nil)

	// USD/JPY, USD account: pip = 100000 × 0.01 = ¥1000 → /150 = $6.6667.
	res, err := calc.PipValue(context.Background(), "USD/JPY", decimal.NewFromInt(1), "USD")
	if err != nil {
		t.Fatalf("PipValue: %v", err)
	}
	want := decimal.NewFromInt(1000).Div(decimal.NewFromInt(150))
	if !res.ValueAccountCcy.Equal(want) {
		t.Fatalf("pip value = %s, want %s", res.ValueAccountCcy, want)
	}
	if !res.ConversionDivide || res.ConversionPair != "USD/JPY" {
		t.Fatalf("expected divide-by-pair conversion, got %+v", res)
	}
}

func TestPipValueCrossPair(t *testing.T) {
	now := time.Now()
	prices := NewLastTradePriceProvider()
	seedPrices(t, prices, now)
	calc := newCalc(t, prices, nil)

	// EUR/GBP, USD account: pip = £10 → ×GBP/USD 1.25 = $12.50.
	res, err := calc.PipValue(context.Background(), "EUR/GBP", decimal.NewFromInt(1), "USD")
	if err != nil {
		t.Fatalf("PipValue: %v", err)
	}
	assertPip(t, res, "12.5")
	if res.ConversionPair != "GBP/USD" || res.ConversionDivide {
		t.Fatalf("expected multiply via GBP/USD, got %+v", res)
	}
}

func TestPipValueCrossPairInverseLeg(t *testing.T) {
	now := time.Now()
	prices := NewLastTradePriceProvider()
	seedPrices(t, prices, now)
	calc := newCalc(t, prices, nil)

	// EUR/JPY, USD account: pip = ¥1000; JPY/USD leg absent, USD/JPY
	// exists → divide: 1000/150 = $6.6667.
	res, err := calc.PipValue(context.Background(), "EUR/JPY", decimal.NewFromInt(1), "USD")
	if err != nil {
		t.Fatalf("PipValue: %v", err)
	}
	want := decimal.NewFromInt(1000).Div(decimal.NewFromInt(150))
	if !res.ValueAccountCcy.Equal(want) {
		t.Fatalf("pip value = %s, want %s", res.ValueAccountCcy, want)
	}
	if res.ConversionPair != "USD/JPY" || !res.ConversionDivide {
		t.Fatalf("expected divide via USD/JPY, got %+v", res)
	}
}

func TestPipValueJPYPipSize(t *testing.T) {
	now := time.Now()
	prices := NewLastTradePriceProvider()
	seedPrices(t, prices, now)
	calc := newCalc(t, prices, nil)

	// decimal_places=3 → pip_size 0.01, NOT 0.0001.
	res, err := calc.PipValue(context.Background(), "GBP/JPY", decimal.NewFromInt(1), "GBP")
	if err != nil {
		t.Fatalf("PipValue: %v", err)
	}
	if !res.PipSize.Equal(decimal.New(1, -2)) {
		t.Fatalf("pip size = %s, want 0.01", res.PipSize)
	}
	// GBP account on GBP/JPY: account=base → ¥1000 / 190 = £5.263...
	want := decimal.NewFromInt(1000).Div(decimal.NewFromInt(190))
	if !res.ValueAccountCcy.Equal(want) {
		t.Fatalf("pip value = %s, want %s", res.ValueAccountCcy, want)
	}
}

func TestPipValueLotsScalingAndNormalization(t *testing.T) {
	now := time.Now()
	prices := NewLastTradePriceProvider()
	seedPrices(t, prices, now)
	calc := newCalc(t, prices, nil)

	// Symbol without slash, lowercase, fractional lots.
	res, err := calc.PipValue(context.Background(), "eurusd",
		decimal.RequireFromString("2.5"), "usd")
	if err != nil {
		t.Fatalf("PipValue: %v", err)
	}
	assertPip(t, res, "25")
	if res.Symbol != "EUR/USD" || res.AccountCurrency != "USD" {
		t.Fatalf("normalization failed: %+v", res)
	}
}

// ---------------------------------------------------------------------------
// Fail-closed behaviour
// ---------------------------------------------------------------------------

func TestPipValueStalePriceFailsClosed(t *testing.T) {
	clock := time.Now()
	cur := clock
	prices := NewLastTradePriceProvider().WithClock(func() time.Time { return cur })
	seedPrices(t, prices, cur)
	calc := newCalc(t, prices, nil)

	cur = cur.Add(6 * time.Second) // beyond the 5s oracle staleness gate
	_, err := calc.PipValue(context.Background(), "EUR/GBP", decimal.NewFromInt(1), "USD")
	var ee *excerrors.Error
	if !stderrors.As(err, &ee) || ee.Code != codeMarkPriceStale {
		t.Fatalf("expected MARK_PRICE_STALE, got %v", err)
	}
}

func TestPipValueNoConversionPath(t *testing.T) {
	now := time.Now()
	prices := NewLastTradePriceProvider()
	seedPrices(t, prices, now)
	calc := newCalc(t, prices, nil)

	// CHF account on EUR/GBP: neither GBP/CHF nor CHF/GBP exists → error.
	_, err := calc.PipValue(context.Background(), "EUR/GBP", decimal.NewFromInt(1), "CHF")
	var ee *excerrors.Error
	if !stderrors.As(err, &ee) || ee.Code != codeOracleUnavailable {
		t.Fatalf("expected PRICE_ORACLE_UNAVAILABLE, got %v", err)
	}
}

func TestPipValueUnknownInstrument(t *testing.T) {
	prices := NewLastTradePriceProvider()
	calc := newCalc(t, prices, nil)
	_, err := calc.PipValue(context.Background(), "ZZZ/QQQ", decimal.NewFromInt(1), "USD")
	var ee *excerrors.Error
	if !stderrors.As(err, &ee) || ee.Code != codeNotFound {
		t.Fatalf("expected NOT_FOUND, got %v", err)
	}
}

func TestPipValueRejectsBadInputs(t *testing.T) {
	prices := NewLastTradePriceProvider()
	calc := newCalc(t, prices, nil)
	for _, tc := range []struct {
		lots, account string
	}{
		{"0", "USD"}, {"-1", "USD"}, {"2000000", "USD"}, {"1", "US"}, {"1", ""},
	} {
		lots, _ := decimal.NewFromString(tc.lots)
		if _, err := calc.PipValue(context.Background(), "EUR/USD", lots, tc.account); err == nil {
			t.Fatalf("expected error for lots=%s account=%s", tc.lots, tc.account)
		}
	}
}

// ---------------------------------------------------------------------------
// Cache semantics: 1s TTL + >0.1% invalidation
// ---------------------------------------------------------------------------

func TestPipValueCacheHitAndInvalidation(t *testing.T) {
	var calls atomic.Int64
	base := time.Now()
	cur := base

	// Counting provider wraps the stub to observe recomputes.
	stub := NewLastTradePriceProvider().WithClock(func() time.Time { return cur })
	seedPrices(t, stub, cur)
	prices := &countingPrices{inner: stub, calls: &calls}
	cache := NewMemoryPipCache(func() time.Time { return cur })
	calc := newCalc(t, prices, cache)

	ctx := context.Background()
	one := decimal.NewFromInt(1)

	res1, err := calc.PipValue(ctx, "EUR/GBP", one, "USD")
	if err != nil {
		t.Fatalf("PipValue: %v", err)
	}
	assertPip(t, res1, "12.5")
	callsAfterFirst := calls.Load()

	// Second call within TTL, price unmoved → cache hit (no new price call
	// beyond the invalidation check... the check itself fetches the mid).
	res2, err := calc.PipValue(ctx, "EUR/GBP", one, "USD")
	if err != nil {
		t.Fatalf("PipValue cached: %v", err)
	}
	if !res2.Cached {
		t.Fatal("expected cached result")
	}
	assertPip(t, res2, "12.5")

	// 0.05% move (below the 0.1% gate) → still cached.
	if err := stub.SetLastTrade("GBP/USD",
		decimal.RequireFromString("1.2506"), cur); err != nil {
		t.Fatal(err)
	}
	res3, err := calc.PipValue(ctx, "EUR/GBP", one, "USD")
	if err != nil {
		t.Fatalf("PipValue after small move: %v", err)
	}
	if !res3.Cached {
		t.Fatal("0.05% move must not invalidate")
	}

	// 0.2% move (above the gate) → recompute; £10 × 1.253 = $12.53.
	if err := stub.SetLastTrade("GBP/USD",
		decimal.RequireFromString("1.253"), cur); err != nil {
		t.Fatal(err)
	}
	res4, err := calc.PipValue(ctx, "EUR/GBP", one, "USD")
	if err != nil {
		t.Fatalf("PipValue after big move: %v", err)
	}
	if res4.Cached {
		t.Fatal("0.2% move must invalidate the cached value")
	}
	assertPip(t, res4, "12.53")

	// TTL expiry: advance the clock 1.1s → recompute even unmoved.
	cur = cur.Add(1100 * time.Millisecond)
	res5, err := calc.PipValue(ctx, "EUR/GBP", one, "USD")
	if err != nil {
		t.Fatalf("PipValue after TTL: %v", err)
	}
	if res5.Cached {
		t.Fatal("1s TTL expiry must force recompute")
	}
	_ = callsAfterFirst
}

// countingPrices counts MidPrice invocations for the cache tests.
type countingPrices struct {
	inner *LastTradePriceProvider
	calls *atomic.Int64
}

func (c *countingPrices) MidPrice(ctx context.Context, symbol string) (decimal.Decimal, error) {
	c.calls.Add(1)
	return c.inner.MidPrice(ctx, symbol)
}

// ---------------------------------------------------------------------------
// REST handler
// ---------------------------------------------------------------------------

func TestPipValueHandler(t *testing.T) {
	now := time.Now()
	prices := NewLastTradePriceProvider()
	seedPrices(t, prices, now)
	calc := newCalc(t, prices, nil)

	req := httptest.NewRequest(http.MethodGet,
		"/api/v1/instruments/EUR%2FUSD/pip-value?lots=1&account_currency=USD", nil)
	req.SetPathValue("symbol", "EUR/USD")
	rec := httptest.NewRecorder()
	calc.PipValueHandler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	var res PipValueResult
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode: %v", err)
	}
	assertPip(t, res, "10")
}

func TestPipValueHandlerErrors(t *testing.T) {
	now := time.Now()
	prices := NewLastTradePriceProvider()
	seedPrices(t, prices, now)
	calc := newCalc(t, prices, nil)

	for _, tc := range []struct {
		name, target, symbol string
		wantStatus           int
		wantCode             string
	}{
		{"missing account", "/api/v1/instruments/EUR%2FUSD/pip-value?lots=1", "EUR/USD", 400, codeInvalidRequest},
		{"bad lots", "/api/v1/instruments/EUR%2FUSD/pip-value?lots=abc&account_currency=USD", "EUR/USD", 400, codeInvalidRequest},
		{"unknown instrument", "/api/v1/instruments/ZZZ%2FQQQ/pip-value?account_currency=USD", "ZZZ/QQQ", 404, codeNotFound},
		{"no conversion", "/api/v1/instruments/EUR%2FGBP/pip-value?account_currency=CHF", "EUR/GBP", 503, codeOracleUnavailable},
	} {
		req := httptest.NewRequest(http.MethodGet, tc.target, nil)
		req.SetPathValue("symbol", tc.symbol)
		rec := httptest.NewRecorder()
		calc.PipValueHandler(rec, req)
		if rec.Code != tc.wantStatus {
			t.Fatalf("%s: status = %d, want %d (body %s)", tc.name, rec.Code, tc.wantStatus, rec.Body.String())
		}
		var p excerrors.Problem
		if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
			t.Fatalf("%s: decode: %v", tc.name, err)
		}
		if p.Code != tc.wantCode {
			t.Fatalf("%s: code = %s, want %s", tc.name, p.Code, tc.wantCode)
		}
	}
}
