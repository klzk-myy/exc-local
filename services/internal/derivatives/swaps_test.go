// swaps_test.go — Task 22.3.2: FX swap pricing (points = far − near),
// leg construction, settlement on respective dates, zero net spot
// exposure, and validation failures.
package derivatives

import (
	"context"
	"testing"
	"time"

	"exchange/pkg/decimal"
)

func swapSvcFor(t *testing.T, store *memStore) *SwapService {
	return NewSwapService(calendarFor(t), testPricer(), store)
}

func TestSwapPricingPoints(t *testing.T) {
	svc := swapSvcFor(t, newMemStore())
	ctx := context.Background()
	pair := mustPair(t, "EUR", "USD")
	spot := decimal.RequireFromString("1.10")
	q, err := svc.PriceSwap(ctx, SwapOrderRequest{
		Pair:            pair,
		Side:            SideBuy,
		Notional:        decimal.RequireFromString("1000000"),
		FarValueDate:    day(2026, 4, 13), // ~3M out
		SettlementCycle: 1,
		TradeDay:        day(2026, 1, 8),
	}, spot)
	if err != nil {
		t.Fatalf("price: %v", err)
	}
	// Spot Fri 2026-01-09; near defaults to spot date.
	if !q.NearValueDate.Equal(day(2026, 1, 9)) {
		t.Fatalf("near %s want 2026-01-09 (spot)", q.NearValueDate)
	}
	if !q.FarValueDate.Equal(day(2026, 4, 13)) {
		t.Fatalf("far %s want 2026-04-13", q.FarValueDate)
	}
	if !q.NearRate.Equal(spot) {
		t.Fatalf("near rate %s want spot %s", q.NearRate, spot)
	}
	// points = far − near, positive here (EUR base rate < USD quote rate
	// lifts the far outright above spot).
	if !q.SwapPoints.Equal(q.FarRate.Sub(q.NearRate)) || !q.SwapPoints.IsPositive() {
		t.Fatalf("points wrong: %s", q.SwapPoints)
	}
}

func TestSwapBookingLegs(t *testing.T) {
	store := newMemStore()
	svc := swapSvcFor(t, store)
	ctx := context.Background()
	far := day(2026, 2, 10)
	c, q, err := svc.BookSwap(ctx, SwapBookRequest{
		SwapOrderRequest: SwapOrderRequest{
			Pair:            mustPair(t, "EUR", "USD"),
			Side:            SideBuy,
			Notional:        decimal.RequireFromString("1000000"),
			FarValueDate:    far,
			SettlementCycle: 1,
			TradeDay:        day(2026, 1, 8),
		},
		TradeID: 201, AccountID: 7, InstrumentID: 43,
		SpotRate:       decimal.RequireFromString("1.10"),
		IdempotencyKey: "swp:201:7",
	})
	if err != nil {
		t.Fatalf("book: %v", err)
	}
	if c.Kind != KindSwap || c.NearLegValueDate == nil {
		t.Fatalf("contract wrong: %+v", c)
	}
	if !q.SwapPoints.Equal(c.SwapPoints) {
		t.Fatalf("quote/contract divergence")
	}
	var legs []PersistedLeg
	_ = store.InTx(ctx, func(ctx context.Context, tx ContractTx) error {
		var err error
		legs, err = tx.ContractLegs(ctx, c.ID)
		return err
	})
	if len(legs) != 4 {
		t.Fatalf("legs=%d want 4: %+v", len(legs), legs)
	}
	// Near legs settle at the spot date (Fri 2026-01-09), far legs at the
	// far date — "both legs settled on respective dates".
	nearDate := day(2026, 1, 9)
	var nearEUR, nearUSD, farEUR, farUSD *PersistedLeg
	for i := range legs {
		l := &legs[i]
		if l.ValueDate.Equal(nearDate) {
			switch l.Currency {
			case "EUR":
				nearEUR = l
			case "USD":
				nearUSD = l
			}
		} else if l.ValueDate.Equal(far) {
			switch l.Currency {
			case "EUR":
				farEUR = l
			case "USD":
				farUSD = l
			}
		}
	}
	if nearEUR == nil || nearUSD == nil || farEUR == nil || farUSD == nil {
		t.Fatalf("leg set wrong: %+v", legs)
	}
	// BUY near → RECEIVE EUR / PAY USD; far → PAY EUR / RECEIVE USD.
	if nearEUR.Direction != LegReceive || nearUSD.Direction != LegPay ||
		farEUR.Direction != LegPay || farUSD.Direction != LegReceive {
		t.Fatalf("directions wrong: %+v", legs)
	}
	// Zero net spot exposure: EUR amounts offset exactly.
	if !nearEUR.Amount.Equal(farEUR.Amount) {
		t.Fatalf("base legs not equal: %s vs %s", nearEUR.Amount, farEUR.Amount)
	}
	// Near quote at the spot rate; far quote at the far rate (larger USD
	// amount since far > spot for this rate differential).
	nearWant := decimal.RequireFromString("1000000").Mul(c.SpotRate).Round(8)
	farWant := decimal.RequireFromString("1000000").Mul(c.ForwardRate).Round(8)
	if !nearUSD.Amount.Equal(nearWant) || !farUSD.Amount.Equal(farWant) {
		t.Fatalf("quote amounts: near %s want %s, far %s want %s",
			nearUSD.Amount, nearWant, farUSD.Amount, farWant)
	}
}

func TestSwapValidationFailures(t *testing.T) {
	svc := swapSvcFor(t, newMemStore())
	base := SwapOrderRequest{
		Pair: mustPair(t, "EUR", "USD"), Side: SideBuy,
		Notional:     decimal.RequireFromString("1000"),
		FarValueDate: day(2026, 2, 10), SettlementCycle: 1,
		TradeDay: day(2026, 1, 8),
	}
	if _, err := svc.ValidateSwapOrder(base); err != nil {
		t.Fatalf("valid: %v", err)
	}
	// Far on/before near (spot 2026-01-09).
	r := base
	r.FarValueDate = day(2026, 1, 9)
	if _, err := svc.ValidateSwapOrder(r); codeOf(t, err) != CodeInvalidRequest {
		t.Fatalf("far==spot: %v", err)
	}
	// Far on a holiday.
	r = base
	r.FarValueDate = day(2026, 1, 19)
	if _, err := svc.ValidateSwapOrder(r); codeOf(t, err) != CodeValueDateOnHoliday {
		t.Fatalf("far holiday: %v", err)
	}
	// Unequal far notional — residual spot exposure, rejected.
	r = base
	farN := decimal.RequireFromString("900")
	r.FarNotional = &farN
	if _, err := svc.ValidateSwapOrder(r); codeOf(t, err) != CodeInvalidRequest {
		t.Fatalf("uneven notional: %v", err)
	}
}

// assertZeroSpotExposure is the hard invariant check.
func TestAssertZeroSpotExposure(t *testing.T) {
	p := mustPair(t, "EUR", "USD")
	n := decimal.RequireFromString("100")
	good := []SettlementLeg{
		{Currency: "EUR", Amount: n, Direction: LegReceive},
		{Currency: "EUR", Amount: n, Direction: LegPay},
		{Currency: "USD", Amount: n, Direction: LegPay},
	}
	if err := assertZeroSpotExposure(good, p); err != nil {
		t.Fatalf("zero net: %v", err)
	}
	bad := []SettlementLeg{
		{Currency: "EUR", Amount: n, Direction: LegReceive},
		{Currency: "EUR", Amount: decimal.RequireFromString("99"), Direction: LegPay},
	}
	if err := assertZeroSpotExposure(bad, p); codeOf(t, err) != CodeInvalidRequest {
		t.Fatalf("net exposure must reject: %v", err)
	}
}

// Swap rollup: near leg settled alone → PARTIALLY_SETTLED; all → SETTLED.
func TestSwapPartialRollup(t *testing.T) {
	store := newMemStore()
	svc := swapSvcFor(t, store)
	ctx := context.Background()
	c, _, err := svc.BookSwap(ctx, SwapBookRequest{
		SwapOrderRequest: SwapOrderRequest{
			Pair: mustPair(t, "EUR", "USD"), Side: SideBuy,
			Notional:     decimal.RequireFromString("1000"),
			FarValueDate: day(2026, 2, 10), SettlementCycle: 1,
			TradeDay: day(2026, 1, 8),
		},
		TradeID: 202, AccountID: 8, InstrumentID: 43,
		SpotRate: decimal.RequireFromString("1.10"),
	})
	if err != nil {
		t.Fatalf("book: %v", err)
	}
	var legs []PersistedLeg
	_ = store.InTx(ctx, func(ctx context.Context, tx ContractTx) error {
		var err error
		legs, err = tx.ContractLegs(ctx, c.ID)
		return err
	})
	// Settle both near legs.
	for _, l := range legs {
		if l.ValueDate.Equal(day(2026, 1, 9)) {
			store.setLegStatus(c.ID, l.ID, "SETTLED")
		}
	}
	st, err := RollupSettlement(ctx, store, c.ID, time.Now())
	if err != nil || st != StatusPartiallySettled {
		t.Fatalf("partial rollup: %v %s", err, st)
	}
	for _, l := range legs {
		store.setLegStatus(c.ID, l.ID, "SETTLED")
	}
	st, err = RollupSettlement(ctx, store, c.ID, time.Now())
	if err != nil || st != StatusSettled {
		t.Fatalf("full rollup: %v %s", err, st)
	}
}
