// ndfs_test.go — Task 22.3.3: NDF admission validation, benchmark fixing
// (§15.7 source vocabulary, BENCHMARK_UNAVAILABLE fail-closed), cash-
// settlement arithmetic and the single-leg no-physical-delivery guarantee
// (spec §15.1, §6.3, §24 #58).
package derivatives

import (
	"context"
	"testing"

	"exchange/pkg/decimal"
)

func ndfSvcFor(t *testing.T, store *memStore) *NdfService {
	return NewNdfService(calendarFor(t), testPricer(), store)
}

const ndfSource = "CENTRAL_BANK:PTAX" // Banco Central do Brasil fixing page

func bookNdf(t *testing.T, svc *NdfService, side ContractSide, notional, agreed string) *Contract {
	t.Helper()
	agreedD := decimal.RequireFromString(agreed)
	c, err := svc.BookNdf(context.Background(), NdfBookRequest{
		NdfOrderRequest: NdfOrderRequest{
			Pair:            mustPair(t, "USD", "BRL"),
			Side:            side,
			Notional:        decimal.RequireFromString(notional),
			ValueDate:       day(2026, 1, 15), // Thursday, business day
			SettlementCycle: 1,
			TradeDay:        day(2026, 1, 8),
			FixingSource:    ndfSource,
		},
		TradeID: 301, AccountID: 7, InstrumentID: 44,
		SpotRate:       decimal.RequireFromString("5.00"),
		AgreedRate:     &agreedD,
		IdempotencyKey: "ndf:301:7:" + string(side) + notional,
	})
	if err != nil {
		t.Fatalf("book: %v", err)
	}
	return c
}

func TestNdfBookingNoPhysicalLegs(t *testing.T) {
	store := newMemStore()
	svc := ndfSvcFor(t, store)
	ctx := context.Background()
	c := bookNdf(t, svc, SideBuy, "1000000", "5.10")
	if c.Kind != KindNDF || c.NdfFixingSource != ndfSource {
		t.Fatalf("contract wrong: %+v", c)
	}
	if c.NdfFixingDate == nil || !c.NdfFixingDate.Equal(day(2026, 1, 15)) {
		t.Fatalf("fixing date %+v", c.NdfFixingDate)
	}
	if c.SettlementCurrency != "USD" {
		t.Fatalf("settle ccy %q want USD (deliverable side)", c.SettlementCurrency)
	}
	// No legs yet — the cash amount only exists once the fixing lands.
	var n int
	_ = store.InTx(ctx, func(ctx context.Context, tx ContractTx) error {
		l, err := tx.ContractLegs(ctx, c.ID)
		n = len(l)
		return err
	})
	if n != 0 {
		t.Fatalf("NDF must not create physical delivery legs at booking: %d", n)
	}
}

func TestNdfCashSettlementBuySide(t *testing.T) {
	store := newMemStore()
	svc := ndfSvcFor(t, store)
	ctx := context.Background()
	c := bookNdf(t, svc, SideBuy, "1000000", "5.10")

	err := svc.ApplyFixing(ctx, c.ID, decimal.RequireFromString("5.25"),
		ndfSource, day(2026, 1, 15))
	if err != nil {
		t.Fatalf("fixing: %v", err)
	}
	out, err := svc.SettleNdf(ctx, c.ID)
	if err != nil {
		t.Fatalf("settle: %v", err)
	}
	if out.Replayed {
		t.Fatal("first settle must not replay")
	}
	// BUY base (USD): fixing 5.25 > contract 5.10 → holder gains
	// N·(F−K)/F = 1e6·0.15/5.25 = 28571.42857143 USD (base-settled).
	want := decimal.RequireFromString("1000000").
		Mul(decimal.RequireFromString("0.15")).
		Div(decimal.RequireFromString("5.25")).Round(8)
	if !out.Amount.Equal(want) || out.Direction != LegReceive {
		t.Fatalf("settle %s %s want %s RECEIVE", out.Amount, out.Direction, want)
	}
	if out.SettlementCurrency != "USD" {
		t.Fatalf("ccy %s", out.SettlementCurrency)
	}
	if !out.FixingRate.Equal(decimal.RequireFromString("5.25")) ||
		!out.ContractRate.Equal(decimal.RequireFromString("5.10")) {
		t.Fatalf("rates: %+v", out)
	}

	// Exactly one net cash leg — physical delivery never exists for NDFs.
	var legs []PersistedLeg
	_ = store.InTx(ctx, func(ctx context.Context, tx ContractTx) error {
		var err error
		legs, err = tx.ContractLegs(ctx, c.ID)
		return err
	})
	if len(legs) != 1 {
		t.Fatalf("legs=%d want exactly one cash leg (no physical delivery)", len(legs))
	}
	l := legs[0]
	if l.Currency != "USD" || l.Direction != LegReceive || !l.Amount.Equal(want) {
		t.Fatalf("cash leg wrong: %+v", l)
	}
	if !l.ValueDate.Equal(day(2026, 1, 15)) {
		t.Fatalf("cash leg value date %s want fixing date", l.ValueDate)
	}

	// Idempotent replay returns the stored outcome, writes nothing new.
	out2, err := svc.SettleNdf(ctx, c.ID)
	if err != nil || !out2.Replayed || !out2.Amount.Equal(want) {
		t.Fatalf("replay: %v %+v", err, out2)
	}
}

func TestNdfCashSettlementSellSidePays(t *testing.T) {
	store := newMemStore()
	svc := ndfSvcFor(t, store)
	ctx := context.Background()
	c := bookNdf(t, svc, SideSell, "1000000", "5.10")

	if err := svc.ApplyFixing(ctx, c.ID, decimal.RequireFromString("5.25"),
		ndfSource, day(2026, 1, 15)); err != nil {
		t.Fatalf("fixing: %v", err)
	}
	out, err := svc.SettleNdf(ctx, c.ID)
	if err != nil {
		t.Fatalf("settle: %v", err)
	}
	// SELL base: fixing above contract → holder owes → PAY leg.
	if out.Direction != LegPay || !out.Amount.IsNegative() {
		t.Fatalf("sell-side outcome wrong: %+v", out)
	}
	var legs []PersistedLeg
	_ = store.InTx(ctx, func(ctx context.Context, tx ContractTx) error {
		var err error
		legs, err = tx.ContractLegs(ctx, c.ID)
		return err
	})
	if len(legs) != 1 || legs[0].Direction != LegPay || legs[0].Currency != "USD" {
		t.Fatalf("sell-side cash leg wrong: %+v", legs)
	}
}

func TestNdfBenchmarkUnavailable(t *testing.T) {
	store := newMemStore()
	svc := ndfSvcFor(t, store)
	ctx := context.Background()
	c := bookNdf(t, svc, SideBuy, "1000000", "5.10")

	// No fixing observed yet — settling without the benchmark must halt.
	_, err := svc.SettleNdf(ctx, c.ID)
	if codeOf(t, err) != CodeBenchmarkUnavailable {
		t.Fatalf("missing fixing must be BENCHMARK_UNAVAILABLE: %v", err)
	}
	// Contract stays OPEN, nothing written.
	_ = store.InTx(ctx, func(ctx context.Context, tx ContractTx) error {
		cc, err := tx.ContractForUpdate(ctx, c.ID)
		if err == nil && cc.Status != StatusOpen {
			t.Fatalf("status mutated on failed settle: %s", cc.Status)
		}
		l, lerr := tx.ContractLegs(ctx, c.ID)
		if lerr == nil && len(l) != 0 {
			t.Fatalf("legs written on failed settle: %+v", l)
		}
		return err
	})
}

func TestNdfFixingAfterSettleRejected(t *testing.T) {
	store := newMemStore()
	svc := ndfSvcFor(t, store)
	ctx := context.Background()
	c := bookNdf(t, svc, SideBuy, "1000000", "5.10")
	if err := svc.ApplyFixing(ctx, c.ID, decimal.RequireFromString("5.25"),
		ndfSource, day(2026, 1, 15)); err != nil {
		t.Fatalf("fixing: %v", err)
	}
	if _, err := svc.SettleNdf(ctx, c.ID); err != nil {
		t.Fatalf("settle: %v", err)
	}
	// A second fixing on a SETTLED contract is a state conflict —
	// settlement is irreversible (fail-closed, spec §2.7).
	err := svc.ApplyFixing(ctx, c.ID, decimal.RequireFromString("5.30"),
		ndfSource, day(2026, 1, 15))
	if codeOf(t, err) != CodeDerivativeStateConflict {
		t.Fatalf("re-fix on settled must conflict: %v", err)
	}
}

func TestNdfValidation(t *testing.T) {
	svc := ndfSvcFor(t, newMemStore())
	base := NdfOrderRequest{
		Pair: mustPair(t, "USD", "BRL"), Side: SideBuy,
		Notional:  decimal.RequireFromString("1000"),
		ValueDate: day(2026, 1, 15), SettlementCycle: 1,
		TradeDay: day(2026, 1, 8), FixingSource: ndfSource,
	}
	if _, err := svc.ValidateNdfOrder(base); err != nil {
		t.Fatalf("valid: %v", err)
	}
	// Fixing date on a settlement-center holiday rejects at admission —
	// the client submits the rolled date (RollNdfFixing covers the
	// settlement-time path; spec §7.4 item 2).
	r := base
	r.ValueDate = day(2026, 1, 19) // USD MLK holiday
	if _, err := svc.ValidateNdfOrder(r); codeOf(t, err) != CodeValueDateOnHoliday {
		t.Fatalf("holiday fixing: %v", err)
	}
	// Fixing date on/before the spot date → not a forward.
	r.ValueDate = day(2026, 1, 9) // == spot date (Fri)
	if _, err := svc.ValidateNdfOrder(r); codeOf(t, err) != CodeInvalidRequest {
		t.Fatalf("fixing<=spot: %v", err)
	}
	// Pair with no restricted currency is not NDF-able.
	r = base
	r.Pair = mustPair(t, "EUR", "USD")
	if _, err := svc.ValidateNdfOrder(r); codeOf(t, err) != CodeInvalidRequest {
		t.Fatalf("deliverable pair: %v", err)
	}
	// Settlement currency must be the deliverable side.
	r = base
	r.SettlementCurrency = "BRL" // restricted leg — non-deliverable
	if _, err := svc.ValidateNdfOrder(r); codeOf(t, err) != CodeInvalidRequest {
		t.Fatalf("restricted settle ccy: %v", err)
	}
	// Unknown fixing source.
	r = base
	r.FixingSource = "WIRE"
	if _, err := svc.ValidateNdfOrder(r); codeOf(t, err) != CodeInvalidRequest {
		t.Fatalf("bad source: %v", err)
	}
	// Source vocabulary check.
	for _, s := range []string{
		NdfSourceCentralBank, "REUTERS:BRLFIX", NdfSourceBloomberg + ":BFIX", NdfSourcePriorDay,
	} {
		if err := ValidateNdfSource(s); err != nil {
			t.Fatalf("source %q should be valid: %v", s, err)
		}
	}
}

// SettlementAmount covers both settlement-currency conventions directly.
func TestNdfSettlementAmountMath(t *testing.T) {
	p := mustPair(t, "USD", "BRL")
	n := decimal.RequireFromString("1000000")
	k := decimal.RequireFromString("5.10")
	f := decimal.RequireFromString("5.25")

	// Base-settled (USD): N·(F−K)/F.
	got, err := SettlementAmount(SideBuy, n, k, f, "USD", p)
	if err != nil {
		t.Fatalf("amount: %v", err)
	}
	decEq(t, got, "28571.42857143")
	// Quote-settled (hypothetical pair orientation): N·(F−K).
	got, err = SettlementAmount(SideBuy, n, k, f, "BRL", p)
	if err != nil {
		t.Fatalf("amount: %v", err)
	}
	decEq(t, got, "150000.00000000")
	// Sell side mirrors.
	got, err = SettlementAmount(SideSell, n, k, f, "USD", p)
	if err != nil {
		t.Fatalf("amount: %v", err)
	}
	decEq(t, got, "-28571.42857143")
	// Non-positive inputs are coded rejections.
	if _, err := SettlementAmount(SideBuy, n, decimal.Zero, f, "USD", p); codeOf(t, err) != CodeInvalidRequest {
		t.Fatalf("zero rate: %v", err)
	}
}
