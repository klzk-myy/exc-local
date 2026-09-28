package ledger

import (
	"strings"
	"testing"

	"github.com/shopspring/decimal"
)

func accrualInput() SwapAccrualInput {
	return SwapAccrualInput{
		AccountID:       7,
		PositionID:      70,
		InstrumentID:    1,
		Symbol:          "EUR/USD",
		Side:            SwapLong,
		InterbankAmount: d("10"),
		Notional:        d("1000000"), // 1m USD
		MarkupBps:       d("25"),
		AccrualCurrency: "USD",
		Days:            1,
		ReferenceID:     900,
		PostedBy:        "rollover-service",
	}
}

// Markup accrual: notional × bps/10⁴ × days/basis, house-side always.
func TestSwapAccrualMarkupMath(t *testing.T) {
	res, err := ComputeSwapAccrual(accrualInput())
	if err != nil {
		t.Fatalf("compute: %v", err)
	}
	// 1,000,000 × 25/10⁴ × 1/360 = 6.94444444
	wantMarkup := d("6.94444444")
	if !res.Record.MarkupAmount.Equal(wantMarkup) {
		t.Fatalf("markup %s want %s", res.Record.MarkupAmount, wantMarkup)
	}
	// client delta = interbank 10 − markup 6.94444444
	if !res.Record.ClientDelta.Equal(d("3.05555556")) {
		t.Fatalf("client delta %s", res.Record.ClientDelta)
	}
	if res.Journal == nil {
		t.Fatal("expected a journal")
	}
	if err := res.Journal.Validate(); err != nil {
		t.Fatalf("journal invalid: %v", err)
	}
	if err := res.Journal.ValidateAccounts(DefaultChart()); err != nil {
		t.Fatalf("journal accounts unresolved: %v", err)
	}
	// Interbank and markup legs as SEPARATE line pairs (§5.21a).
	if len(res.Journal.Lines) != 4 {
		t.Fatalf("expected 4 lines (interbank + markup legs), got %d", len(res.Journal.Lines))
	}
	// interbank credit to client: revenue debit, client liability credit
	l0, l1 := res.Journal.Lines[0], res.Journal.Lines[1]
	if l0.AccountCode != SwapRolloverRevenue("USD") || !l0.Debit.Equal(d("10")) {
		t.Fatalf("interbank leg debit wrong: %+v", l0)
	}
	if l1.AccountCode != CustomerLiability("USD") || !l1.Credit.Equal(d("10")) {
		t.Fatalf("interbank leg credit wrong: %+v", l1)
	}
	// markup leg: client debited, markup revenue credited
	l2, l3 := res.Journal.Lines[2], res.Journal.Lines[3]
	if l2.AccountCode != CustomerLiability("USD") || !l2.Debit.Equal(wantMarkup) {
		t.Fatalf("markup leg debit wrong: %+v", l2)
	}
	if l3.AccountCode != SwapMarkupRevenue("USD") || !l3.Credit.Equal(wantMarkup) {
		t.Fatalf("markup leg credit wrong: %+v", l3)
	}
	// Wallet effect: single signed delta on available.
	if len(res.Journal.Effects) != 1 ||
		!res.Journal.Effects[0].AvailableDelta.Equal(d("3.05555556")) {
		t.Fatalf("effect wrong: %+v", res.Journal.Effects)
	}
	if res.Journal.EntryType != EntryEODRollover {
		t.Fatalf("entry type %s", res.Journal.EntryType)
	}
}

// ACT/365 currency changes the basis materially.
func TestSwapAccrualDayCount(t *testing.T) {
	in := accrualInput()
	in.AccrualCurrency = "GBP" // ACT/365
	res, err := ComputeSwapAccrual(in)
	if err != nil {
		t.Fatalf("compute: %v", err)
	}
	// 1,000,000 × 25/10⁴ × 1/365 = 6.84931507
	if !res.Record.MarkupAmount.Equal(d("6.84931507")) {
		t.Fatalf("GBP markup %s", res.Record.MarkupAmount)
	}
	if res.Record.DayCount != DayCountACT365 {
		t.Fatalf("day count %s", res.Record.DayCount)
	}
}

// Wednesday triple roll: markup scales with days.
func TestSwapAccrualTripleDay(t *testing.T) {
	in := accrualInput()
	in.Days = 3
	in.InterbankAmount = d("30") // engine supplied ×3
	res, err := ComputeSwapAccrual(in)
	if err != nil {
		t.Fatalf("compute: %v", err)
	}
	// 1,000,000 × 25/10⁴ × 3/360 = 20.83333333
	if !res.Record.MarkupAmount.Equal(d("20.83333333")) {
		t.Fatalf("markup %s", res.Record.MarkupAmount)
	}
}

// Negative policy rates post symmetrically; sign preserved in narrative.
func TestSwapAccrualNegativeRate(t *testing.T) {
	in := accrualInput()
	in.InterbankAmount = d("-8")
	in.Side = SwapShort
	res, err := ComputeSwapAccrual(in)
	if err != nil {
		t.Fatalf("compute: %v", err)
	}
	if err := res.Journal.Validate(); err != nil {
		t.Fatalf("journal invalid: %v", err)
	}
	// Interbank leg reversed: client liability debited, revenue credited.
	l0, l1 := res.Journal.Lines[0], res.Journal.Lines[1]
	if l0.AccountCode != CustomerLiability("USD") || !l0.Debit.Equal(d("8")) {
		t.Fatalf("negative-rate leg wrong: %+v", l0)
	}
	if l1.AccountCode != SwapRolloverRevenue("USD") || !l1.Credit.Equal(d("8")) {
		t.Fatalf("negative-rate leg wrong: %+v", l1)
	}
	if !strings.Contains(res.Record.Narrative, "interbank=-8") {
		t.Fatalf("sign not preserved in narrative: %q", res.Record.Narrative)
	}
	if !res.Record.ClientDelta.Equal(d("-14.94444444")) {
		t.Fatalf("client delta %s", res.Record.ClientDelta)
	}
}

// Swap-free (Islamic VERIFIED): zero accrual, foregone amount reported —
// never silently forgiven.
func TestSwapAccrualSwapFree(t *testing.T) {
	in := accrualInput()
	in.SwapFree = true
	res, err := ComputeSwapAccrual(in)
	if err != nil {
		t.Fatalf("compute: %v", err)
	}
	if res.Journal != nil {
		t.Fatal("swap-free accrual must post no journal")
	}
	if !res.Record.ClientDelta.IsZero() {
		t.Fatalf("swap-free client delta must be zero, got %s", res.Record.ClientDelta)
	}
	if !res.Record.ForegoneAmount.Equal(d("3.05555556")) {
		t.Fatalf("foregone %s", res.Record.ForegoneAmount)
	}
	if !strings.Contains(res.Record.Narrative, "swapfree=1") {
		t.Fatalf("narrative missing swap-free marker: %q", res.Record.Narrative)
	}
}

// Zero interbank + zero markup → no journal, but the audit record stands.
func TestSwapAccrualZero(t *testing.T) {
	in := accrualInput()
	in.InterbankAmount = decimal.Zero
	in.MarkupBps = decimal.Zero
	res, err := ComputeSwapAccrual(in)
	if err != nil {
		t.Fatalf("compute: %v", err)
	}
	if res.Journal != nil {
		t.Fatal("zero accrual must not emit a journal")
	}
}

func TestDayCountUnknownFailsClosed(t *testing.T) {
	in := accrualInput()
	in.AccrualCurrency = "XXX"
	if _, err := ComputeSwapAccrual(in); err == nil {
		t.Fatal("unknown currency day-count must fail closed")
	}
}

// Dual control: self-approval and double-approval are rejected.
func TestMarkupPolicyDualControl(t *testing.T) {
	p := &SwapMarkupPolicy{ID: 1, Status: MarkupPending, ProposedBy: "alice"}
	if err := p.Approve("alice"); err == nil {
		t.Fatal("self-approval accepted")
	}
	if err := p.Approve("bob"); err != nil {
		t.Fatalf("dual approval rejected: %v", err)
	}
	if p.Status != MarkupActive {
		t.Fatalf("status %s", p.Status)
	}
	if err := p.Approve("carol"); err == nil {
		t.Fatal("re-approval of ACTIVE policy accepted")
	}
}

// Per-instrument policy beats the global default; inactive policies skip.
func TestMarkupForResolution(t *testing.T) {
	policies := []SwapMarkupPolicy{
		{ID: 1, InstrumentID: 0, LongMarkupBps: d("10"), ShortMarkupBps: d("12"),
			Status: MarkupActive, ProposedBy: "a", ApprovedBy: "b"},
		{ID: 2, InstrumentID: 5, LongMarkupBps: d("30"), ShortMarkupBps: d("35"),
			Status: MarkupActive, ProposedBy: "a", ApprovedBy: "b"},
		{ID: 3, InstrumentID: 6, LongMarkupBps: d("99"), ShortMarkupBps: d("99"),
			Status: MarkupPending, ProposedBy: "a"},
	}
	if v, ok := MarkupFor(policies, 5, SwapLong); !ok || !v.Equal(d("30")) {
		t.Fatalf("instrument markup %v %v", v, ok)
	}
	if v, ok := MarkupFor(policies, 5, SwapShort); !ok || !v.Equal(d("35")) {
		t.Fatalf("short markup %v %v", v, ok)
	}
	if v, ok := MarkupFor(policies, 6, SwapLong); !ok || !v.Equal(d("10")) {
		t.Fatalf("pending policy must not apply; got %v %v", v, ok)
	}
	if v, ok := MarkupFor(policies, 7, SwapLong); !ok || !v.Equal(d("10")) {
		t.Fatalf("global fallback %v %v", v, ok)
	}
}
