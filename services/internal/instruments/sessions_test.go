// Unit tests — Phase-15 Task 15.3.11/15.3.13: session state machine,
// DST anchoring, tenor grid arithmetic, holiday-bound value dates and
// the auction-calendar recurrence (spec §6.7, §7.1, §7.4, §24 #343/#401).
package instruments

import (
	"testing"
	"time"

	"exchange/internal/settlement"
)

// testCal returns a holiday calendar with the centers the test pairs
// need — NewHolidayCalendar registers a currency only when it has ≥1
// row, so each center gets one real holiday.
func testCal(t *testing.T) *settlement.HolidayCalendar {
	t.Helper()
	c, err := settlement.NewHolidayCalendar([]settlement.Holiday{
		{Currency: "USD", Date: time.Date(2025, 7, 4, 0, 0, 0, 0, time.UTC), Name: "Independence Day", Source: "FEDERAL_RESERVE"},
		{Currency: "USD", Date: time.Date(2025, 12, 25, 0, 0, 0, 0, time.UTC), Name: "Christmas", Source: "FEDERAL_RESERVE"},
		{Currency: "USD", Date: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), Name: "New Year", Source: "FEDERAL_RESERVE"},
		{Currency: "EUR", Date: time.Date(2025, 12, 25, 0, 0, 0, 0, time.UTC), Name: "Christmas", Source: "TARGET2"},
		{Currency: "EUR", Date: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), Name: "New Year", Source: "TARGET2"},
		{Currency: "GBP", Date: time.Date(2025, 12, 26, 0, 0, 0, 0, time.UTC), Name: "Boxing Day", Source: "BOE"},
		{Currency: "JPY", Date: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), Name: "New Year", Source: "BOJ"},
		{Currency: "CHF", Date: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), Name: "New Year", Source: "SNB"},
	})
	if err != nil {
		t.Fatalf("calendar: %v", err)
	}
	return c
}

func utc(y int, m time.Month, d, h, mi int) time.Time {
	return time.Date(y, m, d, h, mi, 0, 0, time.UTC)
}

// ---------------------------------------------------------------------------
// DST anchoring — 17:00 ET resolves to 21:00 UTC under EDT, 22:00 under EST
// ---------------------------------------------------------------------------

func TestWeeklyOpenDST(t *testing.T) {
	c, err := NewSessionCalendar()
	if err != nil {
		t.Fatal(err)
	}
	// July = EDT (UTC-4): Sunday open 17:00 ET → 21:00 UTC.
	if got := c.WeeklyOpenUTC(utc(2025, 7, 21, 12, 0)); !got.Equal(utc(2025, 7, 20, 21, 0)) {
		t.Fatalf("summer open: got %v want 2025-07-20 21:00Z", got)
	}
	// January = EST (UTC-5): Sunday open 17:00 ET → 22:00 UTC.
	if got := c.WeeklyOpenUTC(utc(2025, 1, 20, 12, 0)); !got.Equal(utc(2025, 1, 19, 22, 0)) {
		t.Fatalf("winter open: got %v want 2025-01-19 22:00Z", got)
	}
	// US spring-forward 2025-03-09: the March-9 open is already EDT.
	if got := c.WeeklyOpenUTC(utc(2025, 3, 10, 12, 0)); !got.Equal(utc(2025, 3, 9, 21, 0)) {
		t.Fatalf("spring-forward open: got %v want 2025-03-09 21:00Z", got)
	}
	// The week before is still EST → 22:00 UTC.
	if got := c.WeeklyOpenUTC(utc(2025, 3, 3, 12, 0)); !got.Equal(utc(2025, 3, 2, 22, 0)) {
		t.Fatalf("pre-transition open: got %v want 2025-03-02 22:00Z", got)
	}
	// Close mirrors the same anchors (Friday 17:00 ET).
	if got := c.WeeklyCloseUTC(utc(2025, 7, 16, 12, 0)); !got.Equal(utc(2025, 7, 18, 21, 0)) {
		t.Fatalf("summer close: got %v want 2025-07-18 21:00Z", got)
	}
	if got := c.WeeklyCloseUTC(utc(2025, 1, 14, 12, 0)); !got.Equal(utc(2025, 1, 17, 22, 0)) {
		t.Fatalf("winter close: got %v want 2025-01-17 22:00Z", got)
	}
}

func TestSessionStates(t *testing.T) {
	c, err := NewSessionCalendar()
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		at   time.Time
		want SessionState
	}{
		{utc(2025, 7, 16, 15, 0), StateOpen},         // Wed mid-session
		{utc(2025, 7, 18, 20, 59), StateOpen},        // Fri just before close (EDT 21:00)
		{utc(2025, 7, 18, 21, 1), StateWeekend},      // Fri just after close
		{utc(2025, 7, 19, 15, 0), StateWeekend},      // Saturday — the anchoring fix
		{utc(2025, 7, 20, 20, 40), StateWeekend},     // Sun before the 15m ramp
		{utc(2025, 7, 20, 20, 46), StatePreOpen},     // inside 20:45→21:00 accumulation
		{utc(2025, 7, 20, 21, 0), StateOpen},         // open instant
		{utc(2025, 12, 25, 15, 0), StateClosedDay},   // Christmas Day (Thu) — closed override
		{utc(2025, 12, 24, 18, 0), StateOpen},        // Christmas Eve before 14:00 ET early close
		{utc(2025, 12, 24, 19, 30), StateEarlyClose}, // after 14:00 ET (=19:00 UTC EST)
		{utc(2026, 1, 1, 15, 0), StateClosedDay},     // New Year's Day (Thu)
	}
	for _, tc := range cases {
		if got := c.State(tc.at); got != tc.want {
			t.Fatalf("State(%s): got %s want %s", tc.at, got, tc.want)
		}
	}
	if !c.IsOpen(utc(2025, 7, 16, 15, 0)) || c.IsOpen(utc(2025, 7, 20, 20, 46)) {
		t.Fatal("IsOpen must be true only in StateOpen — pre-open does not match")
	}
}

func TestTradeDayAndCutoff(t *testing.T) {
	c, _ := NewSessionCalendar()
	// 21:30 UTC Wednesday is after the 17:00 ET (21:00 UTC EDT) cutoff —
	// the trade belongs to Thursday's trading day.
	td := c.TradeDay(utc(2025, 7, 16, 21, 30))
	if td.Format("2006-01-02") != "2025-07-17" {
		t.Fatalf("TradeDay post-cutoff: got %s", td.Format("2006-01-02"))
	}
	td = c.TradeDay(utc(2025, 7, 16, 20, 30))
	if td.Format("2006-01-02") != "2025-07-16" {
		t.Fatalf("TradeDay pre-cutoff: got %s", td.Format("2006-01-02"))
	}
}

// ---------------------------------------------------------------------------
// Tenor grid (spec §7.4 item 3)
// ---------------------------------------------------------------------------

func TestParseTenor(t *testing.T) {
	for _, g := range TenorGrid {
		if _, err := parseTenor(g); err != nil {
			t.Fatalf("grid tenor %s must parse: %v", g, err)
		}
	}
	for _, bad := range []string{"", "3X", "0M", "-1W", "FOO", "M"} {
		if _, err := parseTenor(bad); err == nil {
			t.Fatalf("bad tenor %q must fail", bad)
		}
	}
	if spec, _ := parseTenor("6M"); spec.months != 6 {
		t.Fatalf("6M parse: %+v", spec)
	}
}

func TestTenorValueDates(t *testing.T) {
	cal := testCal(t)
	// EUR/USD T+2, trade Mon 2025-06-30 → spot Wed 2025-07-02.
	trade := utc(2025, 6, 30, 0, 0)
	cases := map[string]string{
		"ON": "2025-07-01", // next mutual business day
		"TN": "2025-07-02", // ON+1 — lands on spot (the tom-next boundary)
		"SN": "2025-07-03", // spot+1 business day
		"1W": "2025-07-09",
		"2W": "2025-07-16",
		"1M": "2025-08-04", // 8/2 is Saturday → Modified Following → Mon
		"3M": "2025-10-02",
		"1Y": "2026-07-02",
		"2Y": "2027-07-02",
	}
	for tenor, want := range cases {
		got, err := TenorValueDate(cal, "EUR", "USD", trade, 2, tenor)
		if err != nil {
			t.Fatalf("tenor %s: %v", tenor, err)
		}
		if got.Format("2006-01-02") != want {
			t.Fatalf("tenor %s: got %s want %s", tenor, got.Format("2006-01-02"), want)
		}
	}
}

func TestTenorHolidaySkip(t *testing.T) {
	cal := testCal(t)
	// Trade Thu 2025-07-03: T+1 lands on the Friday July-4 USD holiday →
	// next mutual business day Mon 7/7; T+2 → Tue 7/8.
	spot, err := SpotDate(cal, "EUR", "USD", utc(2025, 7, 3, 0, 0), 2)
	if err != nil {
		t.Fatal(err)
	}
	if spot.Format("2006-01-02") != "2025-07-08" {
		t.Fatalf("spot over holiday: got %s want 2025-07-08", spot.Format("2006-01-02"))
	}
}

func TestTenorEndOfMonthRule(t *testing.T) {
	cal := testCal(t)
	// Trade Thu 2025-06-26 → spot Mon 6/30, the last mutual business day
	// of June → the 1M forward must land on the last mutual business day
	// of July (Thu 7/31), not the nominal 7/30.
	spot, err := SpotDate(cal, "EUR", "USD", utc(2025, 6, 26, 0, 0), 2)
	if err != nil || spot.Format("2006-01-02") != "2025-06-30" {
		t.Fatalf("spot: %v %v", spot, err)
	}
	got, err := TenorValueDate(cal, "EUR", "USD", utc(2025, 6, 26, 0, 0), 2, "1M")
	if err != nil {
		t.Fatal(err)
	}
	if got.Format("2006-01-02") != "2025-07-31" {
		t.Fatalf("EOM rule: got %s want 2025-07-31", got.Format("2006-01-02"))
	}
}

func TestTenorBracket(t *testing.T) {
	cal := testCal(t)
	trade := utc(2025, 6, 30, 0, 0) // spot 7/2
	// Exact grid hit.
	l, u, w, err := TenorBracket(cal, "EUR", "USD", trade, 2, utc(2025, 7, 9, 0, 0))
	if err != nil || l != "1W" || u != "1W" || w != 1.0 {
		t.Fatalf("exact: %s %s %f %v", l, u, w, err)
	}
	// Broken date between 1W (7/9) and 2W (7/16): 7/10 → w = 1/7.
	l, u, w, err = TenorBracket(cal, "EUR", "USD", trade, 2, utc(2025, 7, 10, 0, 0))
	if err != nil || l != "1W" || u != "2W" {
		t.Fatalf("broken: %s %s %v", l, u, err)
	}
	if w < 0.14 || w > 0.15 {
		t.Fatalf("broken weight: got %f want ~0.143", w)
	}
	// Before the ON date fails closed.
	if _, _, _, err = TenorBracket(cal, "EUR", "USD", trade, 2, trade); err == nil {
		t.Fatal("pre-ON broken date must fail")
	}
}

func TestIMMStubs(t *testing.T) {
	// Third-Wednesday IMM dates for 2025.
	exp := map[time.Month]string{
		time.March: "2025-03-19", time.June: "2025-06-18",
		time.September: "2025-09-17", time.December: "2025-12-17",
	}
	for m, want := range exp {
		if got := IMMDate(2025, m).Format("2006-01-02"); got != want {
			t.Fatalf("IMM %s 2025: got %s want %s", m, got, want)
		}
	}
	if got := NextIMMDate(utc(2025, 3, 19, 12, 0)); got.Format("2006-01-02") != "2025-06-18" {
		t.Fatalf("NextIMMDate: got %s want 2025-06-18", got.Format("2006-01-02"))
	}
}

// ---------------------------------------------------------------------------
// Holiday-bound value dates + NDF fixing roll (spec §7.4 item 2)
// ---------------------------------------------------------------------------

func TestValidateValueDate(t *testing.T) {
	cal := testCal(t)
	if err := ValidateValueDate(cal, "EUR", "USD", utc(2025, 7, 4, 0, 0)); err == nil {
		t.Fatal("July-4 value date must reject — USD holiday")
	}
	if err := ValidateValueDate(cal, "EUR", "USD", utc(2025, 7, 7, 0, 0)); err != nil {
		t.Fatalf("Monday value date: %v", err)
	}
	// A pair center with no loaded calendar fails closed.
	if err := ValidateValueDate(cal, "EUR", "USD", utc(2025, 7, 7, 0, 0)); err != nil {
		t.Fatal(err)
	}
	onlyUSD, _ := settlement.NewHolidayCalendar([]settlement.Holiday{
		{Currency: "USD", Date: utc(2025, 12, 25, 0, 0), Name: "x"},
	})
	if err := ValidateValueDate(onlyUSD, "EUR", "USD", utc(2025, 7, 7, 0, 0)); err == nil {
		t.Fatal("missing EUR calendar must fail closed")
	}
	if err := ValidateValueDate(nil, "EUR", "USD", utc(2025, 7, 7, 0, 0)); err == nil {
		t.Fatal("nil calendar must fail closed")
	}
}

func TestRollNdfFixing(t *testing.T) {
	cal := testCal(t)
	// Christmas fixing for USD/JPY rolls to Friday 2025-12-26 (JPY is not
	// closed on 12/26; GBP's Boxing Day is not a center here).
	got, err := RollNdfFixing(cal, "USD", "JPY", utc(2025, 12, 25, 0, 0))
	if err != nil {
		t.Fatal(err)
	}
	if got.Format("2006-01-02") != "2025-12-26" {
		t.Fatalf("NDF roll: got %s want 2025-12-26", got.Format("2006-01-02"))
	}
}

func TestOptionExpiryCuts(t *testing.T) {
	c, _ := NewSessionCalendar()
	// NDF 10:00 ET cut: January (EST) → 15:00 UTC.
	got, err := c.OptionExpiryInstant(utc(2025, 1, 15, 0, 0), CutNdfFixing)
	if err != nil || !got.Equal(utc(2025, 1, 15, 15, 0)) {
		t.Fatalf("NDF cut: %v %v", got, err)
	}
	// Venue 15:00 UTC cut is timezone-free.
	got, err = c.OptionExpiryInstant(utc(2025, 1, 15, 0, 0), CutVenue)
	if err != nil || !got.Equal(utc(2025, 1, 15, 15, 0)) {
		t.Fatalf("venue cut: %v %v", got, err)
	}
	// July NDF cut under EDT → 14:00 UTC.
	got, _ = c.OptionExpiryInstant(utc(2025, 7, 15, 0, 0), CutNdfFixing)
	if !got.Equal(utc(2025, 7, 15, 14, 0)) {
		t.Fatalf("NDF cut DST: got %v want 14:00Z", got)
	}
}

// ---------------------------------------------------------------------------
// Auction-calendar recurrence (Task 15.3.13)
// ---------------------------------------------------------------------------

func TestParseRecurrence(t *testing.T) {
	cases := map[string][7]bool{
		"MON-FRI":     {false, true, true, true, true, true, false},
		"FRI":         {false, false, false, false, false, true, false},
		"MON,WED,FRI": {false, true, false, true, false, true, false},
		"DAILY":       {true, true, true, true, true, true, true},
		"":            {true, true, true, true, true, true, true},
		"FRI-MON":     {true, true, false, false, false, true, true}, // wraps the week
	}
	for in, want := range cases {
		got, err := parseRecurrence(in)
		if err != nil {
			t.Fatalf("parse %q: %v", in, err)
		}
		if got != want {
			t.Fatalf("parse %q: got %v want %v", in, got, want)
		}
	}
	for _, bad := range []string{"FOO", "MON-", "MOO-FRI", "1D"} {
		if _, err := parseRecurrence(bad); err == nil {
			t.Fatalf("bad recurrence %q must fail", bad)
		}
	}
}

func TestNextOccurrenceDST(t *testing.T) {
	e := CalendarEntry{
		AuctionType: AuctionDailyClose, TriggerTime: "16:00",
		Timezone: "Europe/London", Recurrence: "MON-FRI", Enabled: true,
	}
	// January — London is GMT: 16:00 local = 16:00 UTC. From Friday
	// 2025-01-10 17:00Z the next occurrence is Monday 1/13.
	got, err := e.NextOccurrence(utc(2025, 1, 10, 17, 0))
	if err != nil || !got.Equal(utc(2025, 1, 13, 16, 0)) {
		t.Fatalf("winter: %v %v", got, err)
	}
	// July — London is BST (UTC+1): 16:00 local = 15:00 UTC.
	got, err = e.NextOccurrence(utc(2025, 7, 4, 17, 0))
	if err != nil || !got.Equal(utc(2025, 7, 7, 15, 0)) {
		t.Fatalf("summer: %v %v — BST must yield 15:00Z", got, err)
	}
	// Same-day trigger strictly after `after` fires today.
	got, err = e.NextOccurrence(utc(2025, 7, 7, 14, 0))
	if err != nil || !got.Equal(utc(2025, 7, 7, 15, 0)) {
		t.Fatalf("same-day: %v %v", got, err)
	}
	// A trigger exactly at `after` is already consumed — next day.
	got, _ = e.NextOccurrence(utc(2025, 7, 7, 15, 0))
	if !got.Equal(utc(2025, 7, 8, 15, 0)) {
		t.Fatalf("strictly-after: got %v", got)
	}
}

func TestNextOccurrenceTokyo(t *testing.T) {
	e := CalendarEntry{
		AuctionType: AuctionFixing, Benchmark: BenchTokyo,
		TriggerTime: "09:55", Timezone: "Asia/Tokyo", Recurrence: "MON-FRI",
		Enabled: true,
	}
	// Tokyo has no DST — 09:55 JST is always 00:55 UTC.
	got, err := e.NextOccurrence(utc(2025, 7, 14, 0, 0))
	if err != nil || !got.Equal(utc(2025, 7, 14, 0, 55)) {
		t.Fatalf("tokyo: %v %v", got, err)
	}
	// Weekend skip: from Friday after the trigger, next is Monday.
	got, err = e.NextOccurrence(utc(2025, 7, 18, 1, 0)) // Fri 10:00 JST
	if err != nil || !got.Equal(utc(2025, 7, 21, 0, 55)) {
		t.Fatalf("tokyo weekend skip: %v %v", got, err)
	}
}

func TestCalendarEntryValidate(t *testing.T) {
	good := CalendarEntry{
		AuctionType: AuctionDailyClose, TriggerTime: "17:00",
		Timezone: VenueTZ, Recurrence: "FRI", Enabled: true,
	}
	if err := good.Validate(); err != nil {
		t.Fatalf("valid row: %v", err)
	}
	bads := []CalendarEntry{
		{AuctionType: "WEEKLY_CLOSE", TriggerTime: "17:00", Timezone: VenueTZ, Recurrence: "FRI"},
		{AuctionType: AuctionFixing, TriggerTime: "16:00", Timezone: "Europe/London", Recurrence: "MON-FRI"}, // missing benchmark
		{AuctionType: AuctionFixing, Benchmark: "LIBOR", TriggerTime: "16:00", Timezone: "Europe/London", Recurrence: "MON-FRI"},
		{AuctionType: AuctionDailyClose, Benchmark: BenchECB, TriggerTime: "16:00", Timezone: "Europe/London", Recurrence: "MON-FRI"}, // benchmark on non-fixing
		{AuctionType: AuctionDailyClose, TriggerTime: "25:00", Timezone: VenueTZ, Recurrence: "FRI"},
		{AuctionType: AuctionDailyClose, TriggerTime: "17:00", Timezone: "No/Such", Recurrence: "FRI"},
		{AuctionType: AuctionDailyClose, TriggerTime: "17:00", Timezone: VenueTZ, Recurrence: "ZZZ"},
	}
	for i, b := range bads {
		if err := b.Validate(); err == nil {
			t.Fatalf("bad row %d must fail validation", i)
		}
	}
}
