// Phase-23 Task 23.3.9 — swap-rate history unit tests. The
// reconciliation invariant (§24 #358: API row == journal row,
// day-for-day) is pinned through ProjectSwapRateDay with a seeded fake
// journal including a triple-Wednesday row; the SQL surface of
// PgSwapRateHistory is exercised by EXC_PG_TEST integration harnesses
// (the pool is concrete — no in-repo pgx mock exists).
package marketdata

import (
	"strings"
	"testing"
	"time"

	"exchange/pkg/decimal"
)

func swDec(s string) decimal.Decimal { return decimal.MustFromString(s) }

// TestProjectSwapRateDayTripleWednesday is the DoD fixture: a fake
// journal with a Wednesday days=3 accrual must project triple:true and
// carry the journal's values EXACTLY.
func TestProjectSwapRateDayTripleWednesday(t *testing.T) {
	wed := time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC) // a Wednesday
	if wed.Weekday() != time.Wednesday {
		t.Fatal("fixture must be a Wednesday")
	}
	rate := SwapRateRow{
		InstrumentID: 7, Symbol: "EUR/USD", EffectiveDate: wed,
		LongPoints: swDec("1.254"), ShortPoints: swDec("-2.318"),
		Source: "REFINITIV", IngestedAt: wed.Add(-6 * time.Hour),
	}
	journal := []SwapJournalRow{
		{Side: "LONG", Days: 3, MarkupBps: swDec("50.25"), Rows: 11},
		{Side: "SHORT", Days: 3, MarkupBps: swDec("50.25"), Rows: 9},
	}
	d := ProjectSwapRateDay(rate, journal)
	if !d.Triple || d.DaysApplied != 3 {
		t.Fatalf("triple-Wednesday projection = %+v", d)
	}
	if !d.LongPoints.Equal(swDec("1.254")) || !d.ShortPoints.Equal(swDec("-2.318")) {
		t.Fatalf("interbank points = %s/%s", d.LongPoints, d.ShortPoints)
	}
	// Exact reconciliation: markup per side is the journal's, verbatim.
	if !d.LongMarkupBps.Equal(swDec("50.25")) || !d.ShortMarkupBps.Equal(swDec("50.25")) {
		t.Fatalf("markup split = %s/%s", d.LongMarkupBps, d.ShortMarkupBps)
	}
	if d.AccrualCount != 20 || d.Symbol != "EUR/USD" || d.Source != "REFINITIV" {
		t.Fatalf("day = %+v", d)
	}
}

// TestProjectSwapRateDayHolidaySpan pins the holiday-aware day-count
// (4-day stretches are NOT triples — triple is exactly days==3 or the
// Wednesday calendar convention).
func TestProjectSwapRateDayHolidaySpan(t *testing.T) {
	thu := time.Date(2026, 10, 8, 0, 0, 0, 0, time.UTC) // Thursday
	d := ProjectSwapRateDay(
		SwapRateRow{InstrumentID: 7, Symbol: "EUR/USD", EffectiveDate: thu},
		[]SwapJournalRow{{Side: "LONG", Days: 4, MarkupBps: swDec("12"), Rows: 3}},
	)
	if d.Triple || d.DaysApplied != 4 {
		t.Fatalf("holiday projection = %+v", d)
	}
	if !d.LongMarkupBps.Equal(swDec("12")) || d.ShortMarkupBps.Sign() != 0 {
		t.Fatalf("markups = %s/%s", d.LongMarkupBps, d.ShortMarkupBps)
	}
}

// TestProjectSwapRateDayNoAccruals covers the quiet-book day: the
// sheet still publishes, and Wednesday's calendar convention flags
// triple even when no accrual row landed.
func TestProjectSwapRateDayNoAccruals(t *testing.T) {
	wed := time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC)
	thu := time.Date(2026, 10, 8, 0, 0, 0, 0, time.UTC)
	dw := ProjectSwapRateDay(SwapRateRow{Symbol: "EUR/USD", EffectiveDate: wed}, nil)
	dt := ProjectSwapRateDay(SwapRateRow{Symbol: "EUR/USD", EffectiveDate: thu}, nil)
	if !dw.Triple || dw.DaysApplied != 0 || dw.AccrualCount != 0 {
		t.Fatalf("Wednesday no-accrual = %+v", dw)
	}
	if dt.Triple {
		t.Fatalf("Thursday no-accrual must not be triple: %+v", dt)
	}
}

// TestProjectSwapRateDayPerSideSplit pins asymmetric markups (admin
// markup differs per side) and mixed day-counts — MAX wins the
// journal-day deterministically.
func TestProjectSwapRateDayPerSideSplit(t *testing.T) {
	mon := time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC) // Monday
	d := ProjectSwapRateDay(
		SwapRateRow{Symbol: "USD/JPY", EffectiveDate: mon,
			LongPoints: swDec("0.412"), ShortPoints: swDec("-0.731")},
		[]SwapJournalRow{
			{Side: "LONG", Days: 1, MarkupBps: swDec("35"), Rows: 4},
			{Side: "SHORT", Days: 1, MarkupBps: swDec("42"), Rows: 2},
		})
	if d.Triple || d.DaysApplied != 1 || d.AccrualCount != 6 {
		t.Fatalf("day = %+v", d)
	}
	if !d.LongMarkupBps.Equal(swDec("35")) || !d.ShortMarkupBps.Equal(swDec("42")) {
		t.Fatalf("split = %s/%s", d.LongMarkupBps, d.ShortMarkupBps)
	}
}

func TestWriteSwapRateDaysCSV(t *testing.T) {
	wed := time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC)
	days := []SwapRateDay{{
		InstrumentID: 7, Symbol: "EUR/USD", EffectiveDate: wed,
		LongPoints: swDec("1.254"), ShortPoints: swDec("-2.318"),
		LongMarkupBps: swDec("50.25"), ShortMarkupBps: swDec("50.25"),
		DaysApplied: 3, Triple: true, AccrualCount: 20,
		Source: "REFINITIV", IngestedAt: wed.Add(-6 * time.Hour),
	}}
	var buf strings.Builder
	if err := WriteSwapRateDaysCSV(&buf, days); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("lines = %d", len(lines))
	}
	if !strings.HasPrefix(lines[0], "symbol,instrument_id,effective_date") ||
		!strings.Contains(lines[0], "triple") {
		t.Fatalf("header = %s", lines[0])
	}
	for _, frag := range []string{"EUR/USD", "1.25400000", "-2.31800000",
		"3,true,20", "REFINITIV"} {
		if !strings.Contains(lines[1], frag) {
			t.Fatalf("row missing %q: %s", frag, lines[1])
		}
	}
}
