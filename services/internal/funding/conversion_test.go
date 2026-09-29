// Unit tests for the Phase-11 Task 11.3.9 currency-conversion engine —
// the OracleRateSource adapter (direct/inverse/stale/non-positive) and
// ConversionService.Quote (same-currency short-circuit, mid±spread math,
// persisted record, fail-closed source). Real-Redis coverage of
// RedisCrossRateSource lives in fee_schedule_integration_test.go.
package funding

import (
	"context"
	"fmt"
	"testing"
	"time"

	"exchange/internal/marketdata"
	"exchange/pkg/decimal"
)

// ---------------------------------------------------------------------------
// OracleRateSource — ReferencePriceSource adapter
// ---------------------------------------------------------------------------

func refFunc(price int64, age time.Duration, err error) marketdata.ReferencePriceFunc {
	return func(context.Context, string) (marketdata.OracleRef, error) {
		if err != nil {
			return marketdata.OracleRef{}, err
		}
		return marketdata.OracleRef{
			Symbol: "EUR/USD", Price: price,
			Source: "composite", ValidAt: time.Now().Add(-age),
		}, nil
	}
}

func TestOracleRateSourceDirect(t *testing.T) {
	// 1.10000000 EUR→USD direct.
	src := &OracleRateSource{Src: refFunc(110_000_000, time.Second, nil)}
	mid, err := src.MidRate(context.Background(), "EUR", "USD")
	if err != nil {
		t.Fatalf("mid: %v", err)
	}
	if mid.Rate.String() != "1.1" || mid.Source != "composite" {
		t.Fatalf("mid=%+v", mid)
	}
}

func TestOracleRateSourceInverse(t *testing.T) {
	// EUR→USD unavailable but USD→EUR exists at 0.9 → rate = 1/0.9.
	src := &OracleRateSource{Src: marketdata.ReferencePriceFunc(
		func(_ context.Context, symbol string) (marketdata.OracleRef, error) {
			if symbol != "USD/EUR" {
				return marketdata.OracleRef{}, fmt.Errorf("no such symbol")
			}
			return marketdata.OracleRef{Symbol: "USD/EUR",
				Price: 90_000_000, Source: "ecb", ValidAt: time.Now()}, nil
		})}
	mid, err := src.MidRate(context.Background(), "EUR", "USD")
	if err != nil {
		t.Fatalf("mid: %v", err)
	}
	if !mid.Rate.Equal(decimal.One.Div(decimal.MustFromString("0.9"))) {
		t.Fatalf("inverse mid=%s", mid.Rate)
	}
	if mid.Source != "ecb:inverted" {
		t.Fatalf("source=%s", mid.Source)
	}
}

func TestOracleRateSourceFailClosed(t *testing.T) {
	// Both directions dead → error.
	src := &OracleRateSource{Src: refFunc(0, 0, fmt.Errorf("oracle down"))}
	if _, err := src.MidRate(context.Background(), "EUR", "USD"); err == nil {
		t.Fatal("expected error")
	}
	// Stale observation (>5s gate) → error.
	src = &OracleRateSource{Src: refFunc(110_000_000, 6*time.Second, nil)}
	if _, err := src.MidRate(context.Background(), "EUR", "USD"); err == nil {
		t.Fatal("stale rate must fail closed")
	}
	// Non-positive price → error.
	src = &OracleRateSource{Src: refFunc(-1, 0, nil)}
	if _, err := src.MidRate(context.Background(), "EUR", "USD"); err == nil {
		t.Fatal("non-positive price must fail closed")
	}
	// Unwired source → error.
	if _, err := (&OracleRateSource{}).MidRate(context.Background(), "EUR", "USD"); err == nil {
		t.Fatal("nil src must fail closed")
	}
}

// ---------------------------------------------------------------------------
// ConversionService
// ---------------------------------------------------------------------------

type fakeRateSource struct {
	mid MidRate
	err error
}

func (f fakeRateSource) MidRate(context.Context, string, string) (MidRate, error) {
	return f.mid, f.err
}

type fakeConvStore struct {
	rows []ConversionRecord
	err  error
}

func (f *fakeConvStore) InsertConversion(_ context.Context, rec ConversionRecord) (*ConversionRecord, error) {
	if f.err != nil {
		return nil, f.err
	}
	rec.ID = int64(len(f.rows) + 1)
	rec.CreatedAt = time.Now()
	f.rows = append(f.rows, rec)
	return &rec, nil
}

func (f *fakeConvStore) ConversionHistory(_ context.Context, _ int64, _ int) ([]ConversionRecord, error) {
	return f.rows, f.err
}

func convSvc(src ConversionRateSource, st *fakeConvStore) *ConversionService {
	svc, err := NewConversionService(src, st, fakeAccounts{metas: map[int64]*AccountMeta{
		1: {ID: 1, UserID: 100, Status: "ACTIVE", KYCTier: "T2", BaseCurrency: "USD"},
	}})
	if err != nil {
		panic(err)
	}
	return svc
}

func TestConversionSameCurrency(t *testing.T) {
	st := &fakeConvStore{}
	svc := convSvc(fakeRateSource{err: fmt.Errorf("no rates")}, st)
	res, err := svc.Quote(context.Background(), ConversionRequest{
		AccountID: 1, FromCurrency: "USD", ToCurrency: "USD",
		Amount: decimal.NewFromInt(50)})
	if err != nil {
		t.Fatalf("same-ccy: %v", err)
	}
	if res.Converted || res.RateApplied.String() != "1" || len(st.rows) != 0 {
		t.Fatalf("same-ccy res=%+v rows=%d", res, len(st.rows))
	}
}

func TestConversionDepositSpread(t *testing.T) {
	st := &fakeConvStore{}
	svc := convSvc(fakeRateSource{mid: MidRate{
		Rate: decimal.MustFromString("1.1"), Source: "composite",
		ValidAt: time.Now()}}, st)
	res, err := svc.Quote(context.Background(), ConversionRequest{
		AccountID: 1, FromCurrency: "EUR", // to defaults to base USD
		Amount: decimal.NewFromInt(1_000)})
	if err != nil {
		t.Fatalf("quote: %v", err)
	}
	// mid 1.1, deposit spread 50bps → applied 1.1×0.995 = 1.0945;
	// 1000 × 1.0945 = 1094.5 USD.
	if res.ToCurrency != "USD" || !res.Converted ||
		res.RateApplied.String() != "1.0945" ||
		res.AmountTo.String() != "1094.5" {
		t.Fatalf("res=%+v", res)
	}
	if len(st.rows) != 1 || st.rows[0].RateSource != "composite" {
		t.Fatalf("record not persisted: %+v", st.rows)
	}
}

func TestConversionWithdrawalSpread(t *testing.T) {
	st := &fakeConvStore{}
	svc := convSvc(fakeRateSource{mid: MidRate{
		Rate: decimal.MustFromString("1.1"), ValidAt: time.Now()}}, st)
	res, err := svc.Quote(context.Background(), ConversionRequest{
		AccountID: 1, FromCurrency: "EUR", ToCurrency: "USD",
		Direction: "WITHDRAWAL", Amount: decimal.NewFromInt(1_000)})
	if err != nil {
		t.Fatalf("quote: %v", err)
	}
	// Withdrawal side pays the spread: applied = 1.1×1.005 = 1.1055.
	if res.RateApplied.String() != "1.1055" || res.AmountTo.String() != "1105.5" {
		t.Fatalf("withdrawal res=%+v", res)
	}
}

func TestConversionFailClosedNoSource(t *testing.T) {
	st := &fakeConvStore{}
	svc := convSvc(nil, st) // unwired source
	_, err := svc.Quote(context.Background(), ConversionRequest{
		AccountID: 1, FromCurrency: "EUR", ToCurrency: "USD",
		Amount: decimal.NewFromInt(10)})
	if err == nil || codeOfT(t, err) != "PRICE_ORACLE_UNAVAILABLE" {
		t.Fatalf("nil src: %v", err)
	}
	if len(st.rows) != 0 {
		t.Fatal("no record may persist without a rate")
	}
	svc = convSvc(fakeRateSource{err: fmt.Errorf("all feeds down")}, st)
	_, err = svc.Quote(context.Background(), ConversionRequest{
		AccountID: 1, FromCurrency: "EUR", ToCurrency: "USD",
		Amount: decimal.NewFromInt(10)})
	if err == nil || codeOfT(t, err) != "PRICE_ORACLE_UNAVAILABLE" {
		t.Fatalf("dead src: %v", err)
	}
}

func TestConversionValidation(t *testing.T) {
	st := &fakeConvStore{}
	svc := convSvc(fakeRateSource{mid: MidRate{
		Rate: decimal.One, ValidAt: time.Now()}}, st)
	for _, tc := range []ConversionRequest{
		{AccountID: 1, FromCurrency: "EU", ToCurrency: "USD",
			Amount: decimal.One}, // bad from currency
		{AccountID: 1, FromCurrency: "EUR", ToCurrency: "US",
			Amount: decimal.One}, // bad to currency
		{AccountID: 1, FromCurrency: "EUR", ToCurrency: "USD",
			Amount: decimal.Zero}, // non-positive amount
		{AccountID: 1, FromCurrency: "EUR", ToCurrency: "USD",
			Direction: "SEND", Amount: decimal.One}, // bad direction
	} {
		if _, err := svc.Quote(context.Background(), tc); err == nil ||
			codeOfT(t, err) != "INVALID_REQUEST" {
			t.Fatalf("req=%+v err=%v", tc, err)
		}
	}
	// No to_currency and the account has no base → INVALID_REQUEST.
	svc2, err := NewConversionService(fakeRateSource{},
		st, fakeAccounts{metas: map[int64]*AccountMeta{
			2: {ID: 2, UserID: 200, Status: "ACTIVE", KYCTier: "T0"},
		}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc2.Quote(context.Background(), ConversionRequest{
		AccountID: 2, FromCurrency: "EUR", Amount: decimal.One}); err == nil ||
		codeOfT(t, err) != "INVALID_REQUEST" {
		t.Fatalf("missing to_currency: %v", err)
	}
}

func TestConversionHistory(t *testing.T) {
	st := &fakeConvStore{}
	svc := convSvc(fakeRateSource{mid: MidRate{
		Rate: decimal.MustFromString("1.1"), ValidAt: time.Now()}}, st)
	if _, err := svc.Quote(context.Background(), ConversionRequest{
		AccountID: 1, FromCurrency: "EUR", ToCurrency: "USD",
		Amount: decimal.NewFromInt(10)}); err != nil {
		t.Fatal(err)
	}
	rows, err := svc.History(context.Background(), 1, 10)
	if err != nil || len(rows) != 1 {
		t.Fatalf("history: %v len=%d", err, len(rows))
	}
}
