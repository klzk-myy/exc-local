package ledger

import (
	"strings"
	"testing"
	"time"
)

var feeNow = time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)

func activeFee(kind FeeKind, minDays, maxDays int, amount string) FeeLine {
	return FeeLine{
		ID: 1, Kind: kind, DaysDormantMin: minDays, DaysDormantMax: maxDays,
		Amount: d(amount), Unit: FeeUnitFlat, Currency: "USD",
		VatApplicable: true, Status: FeeActive,
		EffectiveFrom: feeNow.Add(-24 * time.Hour),
		ProposedBy:    "maker", ApprovedBy: "checker",
	}
}

// Dormancy tiers: days-dormant picks the deepest matching tier; a day
// below all tiers charges nothing.
func TestSelectDormancyFeeTiers(t *testing.T) {
	lines := []FeeLine{
		{ID: 1, Kind: FeeKindDormancy, DaysDormantMin: 30, DaysDormantMax: 89,
			Amount: d("10"), Unit: FeeUnitFlat, Currency: "USD",
			Status: FeeActive, EffectiveFrom: feeNow.Add(-time.Hour)},
		{ID: 2, Kind: FeeKindDormancy, DaysDormantMin: 90, DaysDormantMax: 0,
			Amount: d("25"), Unit: FeeUnitFlat, Currency: "USD",
			Status: FeeActive, EffectiveFrom: feeNow.Add(-time.Hour)},
	}
	if _, ok := SelectDormancyFee(lines, FeeKindDormancy, 10, "USD", feeNow); ok {
		t.Fatal("10 days dormant must not match any tier")
	}
	l, ok := SelectDormancyFee(lines, FeeKindDormancy, 45, "USD", feeNow)
	if !ok || l.ID != 1 {
		t.Fatalf("45 days → %+v", l)
	}
	l, ok = SelectDormancyFee(lines, FeeKindDormancy, 400, "USD", feeNow)
	if !ok || l.ID != 2 {
		t.Fatalf("400 days → %+v", l)
	}
}

// Pending-approval and not-yet-effective lines are invisible.
func TestSelectDormancyFeeHonoursStatus(t *testing.T) {
	pending := activeFee(FeeKindInactivity, 30, 0, "10")
	pending.Status = FeePendingApproval
	if _, ok := SelectDormancyFee([]FeeLine{pending}, FeeKindInactivity, 60, "USD", feeNow); ok {
		t.Fatal("PENDING_APPROVAL fee line must not charge")
	}
	future := activeFee(FeeKindInactivity, 30, 0, "10")
	future.EffectiveFrom = feeNow.Add(24 * time.Hour)
	if _, ok := SelectDormancyFee([]FeeLine{future}, FeeKindInactivity, 60, "USD", feeNow); ok {
		t.Fatal("not-yet-effective fee line must not charge")
	}
}

func TestFeeChargeUnits(t *testing.T) {
	flat := activeFee(FeeKindInactivity, 30, 0, "15")
	if got, _ := flat.Charge(d("0"), 0); !got.Equal(d("15")) {
		t.Fatalf("flat charge %s", got)
	}
	bps := activeFee(FeeKindConversionSpread, 0, 0, "30")
	bps.Unit = FeeUnitBps
	if got, _ := bps.Charge(d("100000"), 0); !got.Equal(d("300")) {
		t.Fatalf("bps charge %s", got) // 100k × 30/10⁴
	}
	pa := activeFee(FeeKindFinancingSpread, 0, 0, "3.65")
	pa.Unit = FeeUnitPctAnnum
	pa.Currency = "USD" // ACT/360
	if got, _ := pa.Charge(d("1000000"), 2); !got.Equal(d("202.77777778")) {
		t.Fatalf("pct/annum charge %s", got) // 1m × 3.65% × 2/360
	}
}

// The fee journal is balanced, debits the client, credits the kind's
// revenue account, and carries the VAT flag for Phase-20 invoicing.
func TestFeeJournal(t *testing.T) {
	f := activeFee(FeeKindInactivity, 30, 0, "15")
	j, err := f.FeeJournal(7, d("15"), 55, "fee-service")
	if err != nil {
		t.Fatalf("journal: %v", err)
	}
	if err := j.Validate(); err != nil {
		t.Fatalf("fee journal invalid: %v", err)
	}
	if err := j.ValidateAccounts(DefaultChart()); err != nil {
		t.Fatalf("fee journal accounts unresolved: %v", err)
	}
	if j.Lines[0].AccountCode != CustomerLiability("USD") ||
		j.Lines[1].AccountCode != InactivityFeeRevenue("USD") {
		t.Fatalf("fee legs wrong: %+v", j.Lines)
	}
	if !strings.Contains(j.Description, "vat=yes") {
		t.Fatalf("VAT flag missing from narrative: %q", j.Description)
	}
	if !j.Effects[0].AvailableDelta.Equal(d("-15")) {
		t.Fatalf("effect %s", j.Effects[0].AvailableDelta)
	}
}

// Per-instrument conversion spread wins over the global default; absent
// lines return not-found (never an implied zero).
func TestConversionSpreadDisclosure(t *testing.T) {
	lines := []FeeLine{
		{ID: 1, Kind: FeeKindConversionSpread, InstrumentID: 0, Amount: d("50"),
			Status: FeeActive, EffectiveFrom: feeNow.Add(-time.Hour), Currency: "USD"},
		{ID: 2, Kind: FeeKindConversionSpread, InstrumentID: 8, Amount: d("75"),
			Status: FeeActive, EffectiveFrom: feeNow.Add(-time.Hour), Currency: "USD"},
	}
	if v, ok := ConversionSpreadBps(lines, 8, feeNow); !ok || !v.Equal(d("75")) {
		t.Fatalf("instrument spread %v %v", v, ok)
	}
	if v, ok := ConversionSpreadBps(lines, 3, feeNow); !ok || !v.Equal(d("50")) {
		t.Fatalf("global spread %v %v", v, ok)
	}
	if _, ok := ConversionSpreadBps(nil, 1, feeNow); ok {
		t.Fatal("absent spread must report not-found")
	}
}

// Dual control on fee rows.
func TestFeeLineDualControl(t *testing.T) {
	f := FeeLine{ID: 9, Status: FeePendingApproval, ProposedBy: "maker"}
	if err := f.Approve("maker"); err == nil {
		t.Fatal("self-approval accepted")
	}
	if err := f.Approve("checker"); err != nil {
		t.Fatalf("dual approval rejected: %v", err)
	}
	if f.Status != FeeActive {
		t.Fatalf("status %s", f.Status)
	}
}
