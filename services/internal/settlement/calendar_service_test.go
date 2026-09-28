package settlement

import (
	"testing"
	"time"
)

// testCalendar seeds a compact holiday set covering the cases exercised:
// real central-bank dates plus one synthetic USD holiday (2025-01-31) used
// to force a Modified Following month-end pull-back.
func testCalendar(t *testing.T) *HolidayCalendar {
	t.Helper()
	mk := func(ccy, d, name string) Holiday {
		parsed, err := time.Parse("2006-01-02", d)
		if err != nil {
			t.Fatalf("bad fixture date %s", d)
		}
		return Holiday{Currency: ccy, Date: parsed, Name: name, Source: "TEST"}
	}
	cal, err := NewHolidayCalendar([]Holiday{
		// USD (Federal Reserve)
		mk("USD", "2025-01-01", "New Year's Day"),
		mk("USD", "2025-01-31", "Synthetic month-end holiday"),
		mk("USD", "2025-07-04", "Independence Day"),
		mk("USD", "2025-09-01", "Labor Day"),
		mk("USD", "2025-11-27", "Thanksgiving"),
		mk("USD", "2025-12-25", "Christmas Day"),
		// EUR (TARGET2)
		mk("EUR", "2025-01-01", "New Year's Day"),
		mk("EUR", "2025-04-18", "Good Friday"),
		mk("EUR", "2025-12-25", "Christmas Day"),
		mk("EUR", "2025-12-26", "St Stephen's Day"),
		// JPY (Bank of Japan)
		mk("JPY", "2025-01-01", "New Year's Day"),
		mk("JPY", "2025-05-05", "Children's Day"),
		mk("JPY", "2025-05-06", "Constitution Day (substitute)"),
		mk("JPY", "2025-08-11", "Mountain Day"),
		mk("JPY", "2025-11-03", "Culture Day"),
		// CAD (Bank of Canada)
		mk("CAD", "2025-01-01", "New Year's Day"),
		mk("CAD", "2025-07-01", "Canada Day"),
		mk("CAD", "2025-12-25", "Christmas Day"),
		// MXN (Banxico)
		mk("MXN", "2025-01-01", "New Year's Day"),
		mk("MXN", "2025-05-01", "Labour Day"),
		mk("MXN", "2025-12-25", "Christmas Day"),
		// GBP (Bank of England)
		mk("GBP", "2025-01-01", "New Year's Day"),
		mk("GBP", "2025-12-25", "Christmas Day"),
	})
	if err != nil {
		t.Fatalf("NewHolidayCalendar: %v", err)
	}
	return cal
}

func day(t *testing.T, s string) time.Time {
	t.Helper()
	d, err := time.Parse("2006-01-02", s)
	if err != nil {
		t.Fatalf("bad date %s", s)
	}
	return d
}

func assertSettles(t *testing.T, cal *HolidayCalendar, base, quote, trade string, cycle int, want string) {
	t.Helper()
	got, err := cal.SettlementDate(base, quote, day(t, trade), cycle)
	if err != nil {
		t.Fatalf("SettlementDate(%s/%s, %s, T+%d): %v", base, quote, trade, cycle, err)
	}
	if got.Format("2006-01-02") != want {
		t.Fatalf("SettlementDate(%s/%s, %s, T+%d) = %s, want %s",
			base, quote, trade, cycle, got.Format("2006-01-02"), want)
	}
}

func TestSettlementT1PlainWeekday(t *testing.T) {
	cal := testCalendar(t)
	// Monday 2025-07-07 EUR/USD T+1 -> Tuesday 2025-07-08.
	assertSettles(t, cal, "EUR", "USD", "2025-07-07", 1, "2025-07-08")
}

func TestSettlementT1WeekendShift(t *testing.T) {
	cal := testCalendar(t)
	// Friday 2025-07-11 T+1 -> Monday (Sat/Sun not business days).
	assertSettles(t, cal, "EUR", "USD", "2025-07-11", 1, "2025-07-14")
}

func TestSettlementHolidayShift(t *testing.T) {
	cal := testCalendar(t)
	// Thursday 2025-07-03 EUR/USD T+1 -> Friday Jul 4 is a USD holiday and
	// Sat/Sun follow -> next mutual business day Monday 2025-07-07.
	assertSettles(t, cal, "EUR", "USD", "2025-07-03", 1, "2025-07-07")
}

func TestSettlementT2InterimHoliday(t *testing.T) {
	cal := testCalendar(t)
	// USD/JPY T+2 traded Wednesday 2025-07-02: day1 Thu Jul 3 good in both,
	// day2 Fri Jul 4 USD holiday -> Mon Jul 7.
	assertSettles(t, cal, "USD", "JPY", "2025-07-02", 2, "2025-07-07")
}

func TestSettlementSplitHoliday(t *testing.T) {
	cal := testCalendar(t)
	// USD/JPY T+1 traded Friday 2025-05-02: Monday May 5 is a JPY holiday
	// (USD open) -> split holiday rolls to Tuesday May 6. May 6 is also a
	// JPY holiday in the fixture -> Wednesday May 7.
	assertSettles(t, cal, "USD", "JPY", "2025-05-02", 1, "2025-05-07")
	// Same shape reversed: EUR/USD T+1 traded Wed 2025-12-24 -> Thu Dec 25
	// holidays in BOTH centers, Fri Dec 26 EUR-only holiday -> Mon Dec 29.
	assertSettles(t, cal, "EUR", "USD", "2025-12-24", 1, "2025-12-29")
}

func TestSettlementSameDayUSDCAD(t *testing.T) {
	cal := testCalendar(t)
	// USD/CAD carries settlement_cycle 0 (same-day) per spec §6.3.
	assertSettles(t, cal, "USD", "CAD", "2025-07-07", 0, "2025-07-07")
	// Same-day on a CAD holiday (Canada Day) shifts to next mutual day.
	assertSettles(t, cal, "USD", "CAD", "2025-07-01", 0, "2025-07-02")
	// USD/MXN same-day likewise.
	assertSettles(t, cal, "USD", "MXN", "2025-07-07", 0, "2025-07-07")
	assertSettles(t, cal, "USD", "MXN", "2025-05-01", 0, "2025-05-02") // trade date IS the MXN holiday
}

func TestSettlementCrossPairIncludesUSD(t *testing.T) {
	cal := testCalendar(t)
	// EUR/GBP is a cross pair: USD calendar joins the gate per spec §17.5.
	// Friday 2025-07-04 is a USD holiday even though both legs are non-USD.
	assertSettles(t, cal, "EUR", "GBP", "2025-07-03", 1, "2025-07-07")
}

func TestSettlementModifiedFollowingMonthEnd(t *testing.T) {
	cal := testCalendar(t)
	// EUR/USD T+2 traded Wed 2025-01-29: business-day walk counts Jan 30
	// then hits the synthetic USD holiday on Fri Jan 31 and lands Feb 3 —
	// a different month than the nominal Jan 31 target. Modified Following
	// pulls the value date back to Thu Jan 30, the last mutual business
	// day of January (FX month-end shortens settlement, never extends it
	// into the next month).
	assertSettles(t, cal, "EUR", "USD", "2025-01-29", 2, "2025-01-30")
}

func TestSettlementNeverBeforeTradeDate(t *testing.T) {
	cal := testCalendar(t)
	// T+1 on the last business day before a month-end holiday: nominal
	// Jan 31 is a USD holiday; the only remaining January mutual business
	// day is the trade date itself, so Modified Following pull-back would
	// be degenerate and the forward-rolled date must stand.
	assertSettles(t, cal, "EUR", "USD", "2025-01-30", 1, "2025-02-03")
}

func TestSettlementFailClosedUnknownCurrency(t *testing.T) {
	cal := testCalendar(t)
	if _, err := cal.SettlementDate("EUR", "ZAR", day(t, "2025-07-07"), 1); err == nil {
		t.Fatal("expected error for currency with no loaded calendar")
	}
	if _, err := cal.SettlementDate("EUR", "EUR", day(t, "2025-07-07"), 1); err == nil {
		t.Fatal("expected error for degenerate pair")
	}
	if _, err := cal.SettlementDate("EUR", "USD", day(t, "2025-07-07"), 3); err == nil {
		t.Fatal("expected error for out-of-range cycle")
	}
}

func TestIsHolidayAndBusinessDay(t *testing.T) {
	cal := testCalendar(t)
	if !cal.IsHoliday("USD", day(t, "2025-07-04")) {
		t.Fatal("2025-07-04 must be a USD holiday")
	}
	if cal.IsHoliday("USD", day(t, "2025-07-07")) {
		t.Fatal("2025-07-07 must not be a USD holiday")
	}
	if cal.IsBusinessDay("EUR", day(t, "2025-07-05")) { // Saturday
		t.Fatal("Saturday is never a business day")
	}
	if !cal.IsBusinessDay("JPY", day(t, "2025-07-07")) {
		t.Fatal("2025-07-07 must be a JPY business day")
	}
	if !cal.IsMutualBusinessDay(day(t, "2025-07-07"), "USD", "JPY") {
		t.Fatal("2025-07-07 must be mutual for USD/JPY")
	}
}

func TestRollValueDateAndDaysBetween(t *testing.T) {
	cal := testCalendar(t)
	// Friday -> Monday roll spans 3 calendar days (triple-swap style);
	// Friday -> over holiday Monday lands Tuesday, a 4-day span.
	next, err := cal.RollValueDate("USD", "JPY", day(t, "2025-05-02"))
	if err != nil {
		t.Fatalf("RollValueDate: %v", err)
	}
	if next.Format("2006-01-02") != "2025-05-07" {
		t.Fatalf("roll = %s, want 2025-05-07", next.Format("2006-01-02"))
	}
	if got := DaysBetween(day(t, "2025-05-02"), next); got != 5 {
		t.Fatalf("DaysBetween = %d, want 5 (holiday weekend 5x)", got)
	}
	plain, err := cal.RollValueDate("EUR", "USD", day(t, "2025-07-11"))
	if err != nil {
		t.Fatalf("RollValueDate: %v", err)
	}
	if plain.Format("2006-01-02") != "2025-07-14" || DaysBetween(day(t, "2025-07-11"), plain) != 3 {
		t.Fatalf("weekend roll = %s, want 2025-07-14 spanning 3 days", plain)
	}
}
