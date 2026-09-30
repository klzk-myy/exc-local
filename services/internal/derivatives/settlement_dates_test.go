// settlement_dates_test.go — value-date math against the holiday
// calendar: T+0/T+1 cycles, weekend & holiday rolls, the explicit-date
// VALUE_DATE_ON_HOLIDAY gate and the NDF fixing roll (spec §6.3, §7.4).
package derivatives

import (
	"testing"
	"time"
)

func TestSpotDateT1Weekday(t *testing.T) {
	d := NewDates(testCalendar(t))
	p := mustPair(t, "EUR", "USD")
	// Thursday trade → Friday settle.
	sd, err := d.SpotDate(p, day(2026, 1, 8), 1)
	if err != nil {
		t.Fatalf("spot: %v", err)
	}
	want := day(2026, 1, 9)
	if !sd.Equal(want) {
		t.Fatalf("T+1 got %s want %s", sd, want)
	}
}

func TestSpotDateT1WeekendSkip(t *testing.T) {
	d := NewDates(testCalendar(t))
	p := mustPair(t, "EUR", "USD")
	// Friday trade → Monday settle (weekend is not a business day).
	sd, err := d.SpotDate(p, day(2026, 1, 9), 1)
	if err != nil {
		t.Fatalf("spot: %v", err)
	}
	want := day(2026, 1, 12)
	if !sd.Equal(want) {
		t.Fatalf("T+1 got %s want %s", sd, want)
	}
}

func TestSpotDateHolidaySkip(t *testing.T) {
	d := NewDates(testCalendar(t))
	p := mustPair(t, "EUR", "USD")
	// Friday 2026-01-16 trade; Monday 2026-01-19 is a USD holiday (MLK)
	// → Tuesday.
	sd, err := d.SpotDate(p, day(2026, 1, 16), 1)
	if err != nil {
		t.Fatalf("spot: %v", err)
	}
	want := day(2026, 1, 20)
	if !sd.Equal(want) {
		t.Fatalf("T+1 got %s want %s", sd, want)
	}
}

func TestSpotDateSameDayUSDCAD(t *testing.T) {
	d := NewDates(testCalendar(t))
	p := mustPair(t, "USD", "CAD")
	if DefaultSpotCycleDays("USD", "CAD") != 0 {
		t.Fatal("USD/CAD must default to same-day")
	}
	if DefaultSpotCycleDays("USD", "MXN") != 0 {
		t.Fatal("USD/MXN must default to same-day")
	}
	if DefaultSpotCycleDays("EUR", "USD") != 1 {
		t.Fatal("EUR/USD must default to T+1")
	}
	// Wednesday same-day trade settles Wednesday.
	sd, err := d.SpotDate(p, day(2026, 1, 7), 0)
	if err != nil {
		t.Fatalf("spot: %v", err)
	}
	if !sd.Equal(day(2026, 1, 7)) {
		t.Fatalf("same-day got %s", sd)
	}
	// A same-day trade dated on a weekend rolls forward to Monday.
	sd, err = d.SpotDate(p, day(2026, 1, 10), 0) // Saturday
	if err != nil {
		t.Fatalf("spot: %v", err)
	}
	if !sd.Equal(day(2026, 1, 12)) {
		t.Fatalf("same-day weekend roll got %s", sd)
	}
	// A same-day trade dated on a center holiday rolls forward.
	sd, err = d.SpotDate(p, day(2026, 1, 19), 0) // USD MLK
	if err != nil {
		t.Fatalf("spot: %v", err)
	}
	if !sd.Equal(day(2026, 1, 20)) {
		t.Fatalf("same-day holiday roll got %s", sd)
	}
}

func TestTenorDateOneMonth(t *testing.T) {
	d := NewDates(testCalendar(t))
	p := mustPair(t, "EUR", "USD")
	// Trade Thu 2026-01-08, spot Fri 2026-01-09 → 1M nominal Mon 2026-02-09.
	vd, err := d.TenorDate(p, day(2026, 1, 8), 1, "1M")
	if err != nil {
		t.Fatalf("tenor: %v", err)
	}
	if !vd.Equal(day(2026, 2, 9)) {
		t.Fatalf("1M got %s", vd)
	}
}

func TestCheckValueDateHolidayRejected(t *testing.T) {
	d := NewDates(testCalendar(t))
	p := mustPair(t, "EUR", "USD")
	spot := day(2026, 1, 9)
	// 2026-01-19 is a USD holiday → explicit value date rejects.
	err := d.CheckValueDate(p, spot, day(2026, 1, 19))
	if codeOf(t, err) != CodeValueDateOnHoliday {
		t.Fatalf("holiday value date: %v", err)
	}
	// Weekend also rejects.
	if err := d.CheckValueDate(p, spot, day(2026, 1, 17)); codeOf(t, err) != CodeValueDateOnHoliday {
		t.Fatalf("weekend value date: %v", err)
	}
	// On/before spot date rejects INVALID_REQUEST.
	if err := d.CheckValueDate(p, spot, day(2026, 1, 9)); codeOf(t, err) != CodeInvalidRequest {
		t.Fatalf("value==spot: %v", err)
	}
	// Good business day passes.
	if err := d.CheckValueDate(p, spot, day(2026, 2, 10)); err != nil {
		t.Fatalf("valid date: %v", err)
	}
}

func TestCheckSwapDates(t *testing.T) {
	d := NewDates(testCalendar(t))
	p := mustPair(t, "EUR", "USD")
	near, far := day(2026, 1, 12), day(2026, 2, 12)
	if err := d.CheckSwapDates(p, near, far); err != nil {
		t.Fatalf("valid legs: %v", err)
	}
	// far <= near rejects.
	if err := d.CheckSwapDates(p, near, near); codeOf(t, err) != CodeInvalidRequest {
		t.Fatalf("far<=near: %v", err)
	}
	// holiday near rejects.
	if err := d.CheckSwapDates(p, day(2026, 1, 19), far); codeOf(t, err) != CodeValueDateOnHoliday {
		t.Fatalf("holiday near: %v", err)
	}
}

func TestRollNdfFixing(t *testing.T) {
	d := NewDates(testCalendar(t))
	p := mustPair(t, "USD", "BRL")
	// Friday 2026-01-16 → no roll needed.
	f, err := d.RollNdfFixing(p, day(2026, 1, 16))
	if err != nil {
		t.Fatalf("roll: %v", err)
	}
	if !f.Equal(day(2026, 1, 16)) {
		t.Fatalf("roll got %s", f)
	}
	// Monday 2026-01-19 (USD holiday) → Tuesday 2026-01-20.
	f, err = d.RollNdfFixing(p, day(2026, 1, 19))
	if err != nil {
		t.Fatalf("roll: %v", err)
	}
	if !f.Equal(day(2026, 1, 20)) {
		t.Fatalf("roll got %s want 2026-01-20", f)
	}
}

func TestDatesFailClosedNoCalendar(t *testing.T) {
	d := NewDates(nil)
	p := mustPair(t, "EUR", "USD")
	if _, err := d.SpotDate(p, time.Now(), 1); err == nil {
		t.Fatal("nil calendar must fail closed")
	}
	if d.IsMutualBusinessDay(p, time.Now()) {
		t.Fatal("nil calendar must report non-business-day")
	}
}
