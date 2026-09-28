package position

import (
	"context"
	"errors"
	"testing"

	"exchange/pkg/decimal"
	excerrors "exchange/pkg/errors"
)

// mapRates is a stub FXRateProvider over a static pair→mid map.
type mapRates map[Pair]decimal.Decimal

func (m mapRates) MidRate(_ context.Context, p Pair) (decimal.Decimal, error) {
	if v, ok := m[p]; ok {
		return v, nil
	}
	return decimal.Zero, ErrPairNotFound
}

type errRates struct{ err error }

func (e errRates) MidRate(context.Context, Pair) (decimal.Decimal, error) {
	return decimal.Zero, e.err
}

func d(s string) decimal.Decimal { return decimal.MustFromString(s) }

func TestConvertIdentity(t *testing.T) {
	c := NewConverter(mapRates{}, "")
	cv, err := c.Convert(context.Background(), d("100"), "USD", "USD")
	if err != nil {
		t.Fatal(err)
	}
	if !cv.ToAmount.Equal(d("100")) || !cv.Rate.Equal(decimal.One) {
		t.Fatalf("identity conversion wrong: %+v", cv)
	}
}

func TestConvertDirect(t *testing.T) {
	// GBP P&L → USD at GBP/USD = 1.25
	c := NewConverter(mapRates{{"GBP", "USD"}: d("1.25")}, "")
	cv, err := c.Convert(context.Background(), d("80"), "GBP", "USD")
	if err != nil {
		t.Fatal(err)
	}
	if !cv.ToAmount.Equal(d("100")) {
		t.Fatalf("to=%s want 100", cv.ToAmount)
	}
	if cv.Path != "GBP/USD" {
		t.Fatalf("path=%s", cv.Path)
	}
}

func TestConvertInverse(t *testing.T) {
	// JPY P&L → USD via USD/JPY = 150 → rate = 1/150
	c := NewConverter(mapRates{{"USD", "JPY"}: d("150")}, "")
	cv, err := c.Convert(context.Background(), d("15000"), "JPY", "USD")
	if err != nil {
		t.Fatal(err)
	}
	// 1/150 is non-terminating — compare at the 8dp ledger precision.
	if !cv.ToAmount.Round(8).Equal(d("100")) {
		t.Fatalf("to=%s want 100", cv.ToAmount)
	}
}

func TestConvertCrossViaUSD(t *testing.T) {
	// AUD/NZD yields NZD P&L; account base EUR.
	// NZD→USD: NZD/USD=0.60 ; USD→EUR: EUR/USD=1.10 → USD per EUR = 1.10 → rate NZD→EUR = 0.60/1.10
	c := NewConverter(mapRates{
		{"NZD", "USD"}: d("0.60"),
		{"EUR", "USD"}: d("1.10"),
	}, "")
	cv, err := c.Convert(context.Background(), d("110"), "NZD", "EUR")
	if err != nil {
		t.Fatal(err)
	}
	// 110 NZD * (0.60/1.10) = 60 EUR (non-terminating rate → 8dp compare)
	if !cv.ToAmount.Round(8).Equal(d("60")) {
		t.Fatalf("to=%s want 60", cv.ToAmount)
	}
}

func TestConvertZeroRateFails(t *testing.T) {
	c := NewConverter(mapRates{{"GBP", "USD"}: decimal.Zero}, "")
	_, err := c.Convert(context.Background(), d("10"), "GBP", "USD")
	var e *excerrors.Error
	if !errors.As(err, &e) || e.Code != CodeMarkPriceOutOfBounds {
		t.Fatalf("want MARK_PRICE_OUT_OF_BOUNDS, got %v", err)
	}
}

func TestConvertNoPathFails(t *testing.T) {
	c := NewConverter(mapRates{{"EUR", "USD"}: d("1.1")}, "")
	_, err := c.Convert(context.Background(), d("10"), "CHF", "JPY")
	var e *excerrors.Error
	if !errors.As(err, &e) || e.Code != CodeOracleUnavailable {
		t.Fatalf("want PRICE_ORACLE_UNAVAILABLE, got %v", err)
	}
}

func TestConvertOracleErrorPropagates(t *testing.T) {
	c := NewConverter(errRates{err: errors.New("oracle timeout")}, "")
	_, err := c.Convert(context.Background(), d("10"), "GBP", "USD")
	if err == nil || err.Error() == "" {
		t.Fatal("oracle error must propagate, not fall back")
	}
}

func TestConvertNegativeAmount(t *testing.T) {
	// A loss converts with its sign preserved.
	c := NewConverter(mapRates{{"GBP", "USD"}: d("1.25")}, "")
	cv, err := c.Convert(context.Background(), d("-80"), "GBP", "USD")
	if err != nil {
		t.Fatal(err)
	}
	if !cv.ToAmount.Equal(d("-100")) {
		t.Fatalf("to=%s want -100", cv.ToAmount)
	}
}

func TestConvertAllBatch(t *testing.T) {
	// USD/JPY=150 only → JPY→USD resolves via the inverse path.
	c := NewConverter(mapRates{{"USD", "JPY"}: d("150")}, "")
	out, err := c.ConvertAll(context.Background(),
		[]decimal.Decimal{d("15000"), d("-1500"), d("0")}, "JPY", "USD")
	if err != nil {
		t.Fatal(err)
	}
	if !out[0].ToAmount.Round(8).Equal(d("100")) || !out[1].ToAmount.Round(8).Equal(d("-10")) || !out[2].ToAmount.IsZero() {
		t.Fatalf("batch results wrong: %+v", out)
	}
}
