// forwards_test.go — Task 22.3.1: forward order validation, booking with
// physical-delivery legs, idempotent replay, and settlement rollup.
package derivatives

import (
	"context"
	"testing"
	"time"

	"exchange/pkg/decimal"
)

func calendarFor(t *testing.T) *Dates { return NewDates(testCalendar(t)) }

func fwdSvcFor(t *testing.T, store *memStore) *ForwardService {
	return NewForwardService(calendarFor(t), testPricer(), store)
}

func TestForwardBookDeliveryLegs(t *testing.T) {
	store := newMemStore()
	svc := fwdSvcFor(t, store)
	ctx := context.Background()
	tradeDay := day(2026, 1, 8) // Thursday; EUR/USD T+1 spot = Fri 2026-01-09
	vd := day(2026, 2, 10)

	c, err := svc.BookForward(ctx, ForwardBookRequest{
		ForwardOrderRequest: ForwardOrderRequest{
			Pair:            mustPair(t, "EUR", "USD"),
			Side:            SideBuy,
			Notional:        decimal.RequireFromString("1000000"),
			ValueDate:       vd,
			SettlementCycle: 1,
			TradeDay:        tradeDay,
		},
		TradeID: 101, AccountID: 7, InstrumentID: 42,
		SpotRate:       decimal.RequireFromString("1.10"),
		IdempotencyKey: "fwd:101:7",
	})
	if err != nil {
		t.Fatalf("book: %v", err)
	}
	if c.Kind != KindForward || c.Status != StatusOpen {
		t.Fatalf("contract wrong: %+v", c)
	}
	// EUR 3.25% (base) < USD 5.25% (quote) → forward above spot.
	if !c.ForwardRate.GreaterThan(c.SpotRate) {
		t.Fatalf("fwd %s should exceed spot %s", c.ForwardRate, c.SpotRate)
	}
	if !c.SwapPoints.Equal(c.ForwardRate.Sub(c.SpotRate)) {
		t.Fatalf("points wrong: %s", c.SwapPoints)
	}
	if !c.SpotValueDate.Equal(day(2026, 1, 9)) {
		t.Fatalf("spot date %s want 2026-01-09", c.SpotValueDate)
	}

	if err := store.InTx(ctx, func(ctx context.Context, tx ContractTx) error {
		got, err := tx.ContractLegs(ctx, c.ID)
		if err == nil {
			assertForwardLegs(t, got, c)
		}
		return err
	}); err != nil {
		t.Fatalf("legs: %v", err)
	}

	// Replay — same idempotency key returns the same contract, no dup legs.
	c2, err := svc.BookForward(ctx, ForwardBookRequest{
		ForwardOrderRequest: ForwardOrderRequest{
			Pair:            mustPair(t, "EUR", "USD"),
			Side:            SideBuy,
			Notional:        decimal.RequireFromString("1000000"),
			ValueDate:       vd,
			SettlementCycle: 1,
			TradeDay:        tradeDay,
		},
		TradeID: 101, AccountID: 7, InstrumentID: 42,
		SpotRate:       decimal.RequireFromString("1.10"),
		IdempotencyKey: "fwd:101:7",
	})
	if err != nil || c2.ID != c.ID {
		t.Fatalf("replay: %v id %d vs %d", err, c2.ID, c.ID)
	}
	var nLegs int
	_ = store.InTx(ctx, func(ctx context.Context, tx ContractTx) error {
		l, err := tx.ContractLegs(ctx, c.ID)
		nLegs = len(l)
		return err
	})
	if nLegs != 2 {
		t.Fatalf("replay duplicated legs: %d", nLegs)
	}
}

func assertForwardLegs(t *testing.T, legs []PersistedLeg, c *Contract) {
	t.Helper()
	if len(legs) != 2 {
		t.Fatalf("legs=%d want 2", len(legs))
	}
	var base, quote *PersistedLeg
	for i := range legs {
		switch legs[i].Currency {
		case "EUR":
			base = &legs[i]
		case "USD":
			quote = &legs[i]
		}
	}
	if base == nil || quote == nil {
		t.Fatalf("missing legs: %+v", legs)
	}
	if base.Direction != LegReceive || quote.Direction != LegPay {
		t.Fatalf("BUY legs wrong: %+v", legs)
	}
	if !base.Amount.Equal(c.Notional) {
		t.Fatalf("base leg amount %s want %s", base.Amount, c.Notional)
	}
	wantQuote := c.Notional.Mul(c.ForwardRate).Round(8)
	if !quote.Amount.Equal(wantQuote) {
		t.Fatalf("quote leg %s want %s", quote.Amount, wantQuote)
	}
	if !base.ValueDate.Equal(c.ValueDate) || !quote.ValueDate.Equal(c.ValueDate) {
		t.Fatalf("legs not dated at maturity: %+v", legs)
	}
}

func TestForwardValidation(t *testing.T) {
	svc := fwdSvcFor(t, newMemStore())
	req := ForwardOrderRequest{
		Pair: mustPair(t, "EUR", "USD"), Side: SideBuy,
		Notional:  decimal.RequireFromString("1000"),
		ValueDate: day(2026, 2, 10), SettlementCycle: 1,
		TradeDay: day(2026, 1, 8),
	}
	if err := svc.ValidateForwardOrder(req); err != nil {
		t.Fatalf("valid: %v", err)
	}
	// Missing value date.
	r2 := req
	r2.ValueDate = time.Time{}
	if err := svc.ValidateForwardOrder(r2); codeOf(t, err) != CodeInvalidRequest {
		t.Fatalf("no value date: %v", err)
	}
	// Holiday value date.
	r3 := req
	r3.ValueDate = day(2026, 1, 19) // USD MLK
	if err := svc.ValidateForwardOrder(r3); codeOf(t, err) != CodeValueDateOnHoliday {
		t.Fatalf("holiday: %v", err)
	}
	// Value date before spot (spot = Fri 2026-01-09).
	r4 := req
	r4.ValueDate = day(2026, 1, 8)
	if err := svc.ValidateForwardOrder(r4); codeOf(t, err) != CodeValueDateOnHoliday &&
		codeOf(t, err) != CodeInvalidRequest {
		t.Fatalf("past value date: %v", err)
	}
	// Non-positive notional.
	r5 := req
	r5.Notional = decimal.Zero
	if err := svc.ValidateForwardOrder(r5); codeOf(t, err) != CodeInvalidRequest {
		t.Fatalf("zero notional: %v", err)
	}
}

func TestForwardBookAgreedRate(t *testing.T) {
	store := newMemStore()
	svc := fwdSvcFor(t, store)
	agreed := decimal.RequireFromString("1.095")
	c, err := svc.BookForward(context.Background(), ForwardBookRequest{
		ForwardOrderRequest: ForwardOrderRequest{
			Pair: mustPair(t, "EUR", "USD"), Side: SideSell,
			Notional:  decimal.RequireFromString("500000"),
			ValueDate: day(2026, 2, 10), SettlementCycle: 1,
			TradeDay: day(2026, 1, 8),
		},
		TradeID: 102, AccountID: 8, InstrumentID: 42,
		SpotRate:   decimal.RequireFromString("1.10"),
		AgreedRate: &agreed,
	})
	if err != nil {
		t.Fatalf("book: %v", err)
	}
	decEq(t, c.ForwardRate, "1.095")
	var legs []PersistedLeg
	_ = store.InTx(context.Background(), func(ctx context.Context, tx ContractTx) error {
		var err error
		legs, err = tx.ContractLegs(ctx, c.ID)
		return err
	})
	// SELL: PAY base, RECEIVE quote.
	var dirs int
	for _, l := range legs {
		if l.Currency == "EUR" && l.Direction == LegPay {
			dirs++
		}
		if l.Currency == "USD" && l.Direction == LegReceive {
			dirs++
		}
	}
	if dirs != 2 {
		t.Fatalf("SELL legs wrong: %+v", legs)
	}
}

// RollupSettlement: legs flip to SETTLED → contract settles; partial →
// PARTIALLY_SETTLED is a swap concept but a lone-settled forward leg
// stays OPEN until both legs settle.
func TestForwardRollupSettlement(t *testing.T) {
	store := newMemStore()
	svc := fwdSvcFor(t, store)
	ctx := context.Background()
	c, err := svc.BookForward(ctx, ForwardBookRequest{
		ForwardOrderRequest: ForwardOrderRequest{
			Pair: mustPair(t, "EUR", "USD"), Side: SideBuy,
			Notional:  decimal.RequireFromString("1000"),
			ValueDate: day(2026, 2, 10), SettlementCycle: 1,
			TradeDay: day(2026, 1, 8),
		},
		TradeID: 103, AccountID: 9, InstrumentID: 42,
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
	// Settle one leg — contract stays OPEN (forward delivery is atomic
	// in status terms: PARTIALLY_SETTLED only applies to swaps).
	store.setLegStatus(c.ID, legs[0].ID, "SETTLED")
	st, err := RollupSettlement(ctx, store, c.ID, time.Now())
	if err != nil || st != StatusOpen {
		t.Fatalf("rollup1: %v %s", err, st)
	}
	store.setLegStatus(c.ID, legs[1].ID, "SETTLED")
	st, err = RollupSettlement(ctx, store, c.ID, time.Now())
	if err != nil || st != StatusSettled {
		t.Fatalf("rollup2: %v %s", err, st)
	}
	// Terminal — rollup is a no-op on SETTLED.
	st, err = RollupSettlement(ctx, store, c.ID, time.Now())
	if err != nil || st != StatusSettled {
		t.Fatalf("rollup3: %v %s", err, st)
	}
}

func TestDueSettlements(t *testing.T) {
	store := newMemStore()
	svc := fwdSvcFor(t, store)
	ctx := context.Background()
	c, err := svc.BookForward(ctx, ForwardBookRequest{
		ForwardOrderRequest: ForwardOrderRequest{
			Pair: mustPair(t, "EUR", "USD"), Side: SideBuy,
			Notional:  decimal.RequireFromString("1000"),
			ValueDate: day(2026, 2, 10), SettlementCycle: 1,
			TradeDay: day(2026, 1, 8),
		},
		TradeID: 104, AccountID: 9, InstrumentID: 42,
		SpotRate: decimal.RequireFromString("1.10"),
	})
	if err != nil {
		t.Fatalf("book: %v", err)
	}
	due, err := DueSettlements(ctx, store, day(2026, 2, 9), 100)
	if err != nil || len(due) != 0 {
		t.Fatalf("not due yet: %v %+v", err, due)
	}
	due, err = DueSettlements(ctx, store, day(2026, 2, 10), 100)
	if err != nil || len(due) != 1 || due[0].ID != c.ID {
		t.Fatalf("due: %v %+v", err, due)
	}
}
