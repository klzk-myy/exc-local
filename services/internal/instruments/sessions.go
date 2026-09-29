// Session calendar, DST-aware weekly boundaries and the §7.4 tenor grid
// — Phase-15 Task 15.3.11 (spec §6.3, §6.7, §7.4, §17.4/§17.5, §24 #343).
//
// The venue week anchors on the 17:00 America/New_York daily cutoff —
// "New York close". Every UTC instant is resolved from the wall-clock
// mark in that zone, which is what makes the calendar DST-aware:
//
//	daily rollover cutoff      17:00 ET  → 21:00 UTC (EDT) / 22:00 UTC (EST)
//	weekly open                Sun 17:00 ET → 21:00 UTC (EDT) / 22:00 UTC (EST)
//	weekly close               Fri 17:00 ET → 21:00 UTC (EDT) / 22:00 UTC (EST)
//
// spec §6.7 pins "Friday 22:00 UTC close / Sunday 21:00 UTC open" — the
// EST-close and EDT-open canonical figures this rule produces. The same
// §7.4 "21:00 vs 22:00 UTC Sunday open" DST shift falls out of the
// 17:00 ET anchor: winter Sundays open at 22:00 UTC, summer at 21:00.
//
// Holiday obligations ride the Phase-03 Task 3.3.8 currency_holidays
// calendar (settlement.HolidayCalendar) — the engine fails closed when a
// required center has no loaded calendar.
package instruments

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"exchange/internal/settlement"

	excerrors "exchange/pkg/errors"
)

// ---------------------------------------------------------------------------
// Venue session constants (spec §6.7/§17.4 canonical marks)
// ---------------------------------------------------------------------------

// VenueTZ is the FX venue anchor zone — every session mark is a
// wall-clock time in New York.
const VenueTZ = "America/New_York"

// NYCloseLocal is the daily/weekly boundary wall-clock mark (17:00 ET).
const NYCloseLocal = "17:00"

// PreOpen is the §6.7 15-minute order-accumulation window before the
// weekly open auction.
const PreOpen = 15 * time.Minute

// SessionState is the venue session state at an instant.
type SessionState string

const (
	StateOpen       SessionState = "OPEN"        // continuous trading
	StatePreOpen    SessionState = "PRE_OPEN"    // Sun open minus 15m — accumulate, no match
	StateWeekend    SessionState = "WEEKEND"     // Fri close → Sun open
	StateClosedDay  SessionState = "CLOSED"      // full-day holiday override (Christmas/New Year)
	StateEarlyClose SessionState = "EARLY_CLOSE" // after the holiday early-close mark
)

// HolidayOverride is one venue-level session override on a New-York
// calendar date (YYYY-MM-DD). Either Closed (no trading that NY date)
// or an EarlyClose wall-clock mark (HH:MM ET) after which the venue is
// dark — the Christmas/New Year early-close machinery of §7.4 item 2.
type HolidayOverride struct {
	Date            string `json:"date"`                  // YYYY-MM-DD in VenueTZ
	Closed          bool   `json:"closed"`                // whole NY date closed
	EarlyCloseLocal string `json:"early_close,omitempty"` // "HH:MM" in VenueTZ
	Reason          string `json:"reason,omitempty"`
}

// DefaultHolidayOverrides returns the seeded venue overrides —
// Christmas and New Year per spec §7.4 item 2. Dates are evaluated on
// the New York calendar (the venue anchor). New Year’s Eve keeps its
// normal close (17:00 ET) — it is listed only to pin the documented
// default and can be tightened via WithOverrides.
func DefaultHolidayOverrides() []HolidayOverride {
	return []HolidayOverride{
		{Date: "-12-24", EarlyCloseLocal: "14:00", Reason: "Christmas Eve early close"},
		{Date: "-12-25", Closed: true, Reason: "Christmas Day"},
		{Date: "-01-01", Closed: true, Reason: "New Year's Day"},
	}
}

// SessionCalendar resolves venue session instants. Immutable after
// construction; copy-with-options via the With* methods.
type SessionCalendar struct {
	ny        *time.Location
	openDay   time.Weekday // Sunday
	closeDay  time.Weekday // Friday
	mark      hm           // 17:00 local NY
	preOpen   time.Duration
	overrides map[string]HolidayOverride // keyed by annual suffix "-MM-DD" or exact "YYYY-MM-DD"
}

// hm is a wall-clock time-of-day.
type hm struct {
	h, m int
}

// parseHM parses "HH:MM".
func parseHM(s string) (hm, error) {
	var h hm
	if n, err := fmt.Sscanf(strings.TrimSpace(s), "%d:%d", &h.h, &h.m); err != nil || n != 2 {
		return hm{}, fmt.Errorf("session: bad wall-clock %q", s)
	}
	if h.h < 0 || h.h > 23 || h.m < 0 || h.m > 59 {
		return hm{}, fmt.Errorf("session: bad wall-clock %q", s)
	}
	return h, nil
}

// at resolves the wall-clock mark on the given NY calendar date to a
// UTC instant — the DST-aware core of every schedule in this file.
func (c *SessionCalendar) at(nyDate time.Time, t hm) time.Time {
	return time.Date(nyDate.Year(), nyDate.Month(), nyDate.Day(),
		t.h, t.m, 0, 0, c.ny)
}

// nyDate truncates t to its New York calendar day (keeps the location).
func (c *SessionCalendar) nyDate(t time.Time) time.Time {
	l := t.In(c.ny)
	return time.Date(l.Year(), l.Month(), l.Day(), 0, 0, 0, 0, c.ny)
}

// NewSessionCalendar builds the venue calendar. opts may supply extra or
// replacement overrides via WithOverrides.
func NewSessionCalendar() (*SessionCalendar, error) {
	loc, err := time.LoadLocation(VenueTZ)
	if err != nil {
		return nil, fmt.Errorf("session: load %s: %w", VenueTZ, err)
	}
	mark, err := parseHM(NYCloseLocal)
	if err != nil {
		return nil, err
	}
	c := &SessionCalendar{
		ny:        loc,
		openDay:   time.Sunday,
		closeDay:  time.Friday,
		mark:      mark,
		preOpen:   PreOpen,
		overrides: map[string]HolidayOverride{},
	}
	for _, o := range DefaultHolidayOverrides() {
		c.overrides[o.Date] = o
	}
	return c, nil
}

// WithOverrides returns a calendar with extra overrides merged in —
// admin override edits (Task 15.3.4 market_schedule_overrides is the
// runtime surface; this slice is the instrument-side default set).
func (c *SessionCalendar) WithOverrides(extra []HolidayOverride) *SessionCalendar {
	cp := *c
	cp.overrides = map[string]HolidayOverride{}
	for k, v := range c.overrides {
		cp.overrides[k] = v
	}
	for _, o := range extra {
		cp.overrides[o.Date] = o
	}
	return &cp
}

// overrideFor resolves the override applying on a NY calendar date:
// exact "YYYY-MM-DD" wins over the annual "-MM-DD" form.
func (c *SessionCalendar) overrideFor(nyDate time.Time) (HolidayOverride, bool) {
	exact := nyDate.Format("2006-01-02")
	if o, ok := c.overrides[exact]; ok {
		return o, true
	}
	annual := "-" + nyDate.Format("01-02")
	o, ok := c.overrides[annual]
	return o, ok
}

// DailyCutoff returns the 17:00 ET rollover instant on the NY date
// containing t — the boundary separating value-date days (spec §17.4).
// A trade at/after the cutoff belongs to the next trading day.
func (c *SessionCalendar) DailyCutoff(t time.Time) time.Time {
	return c.at(c.nyDate(t), c.mark)
}

// TradeDay returns the trading-day attribution for an instant: before
// the day's 17:00 ET cutoff the NY date itself, at/after it the next NY
// date (FX convention — the post-17:00 session belongs to tomorrow).
func (c *SessionCalendar) TradeDay(t time.Time) time.Time {
	d := c.nyDate(t)
	if !t.Before(c.at(d, c.mark)) {
		d = d.AddDate(0, 0, 1)
	}
	return d
}

// WeeklyOpenUTC returns the weekly-open instant (Sunday 17:00 ET) that
// begins the trading week containing t — the most recent Sunday mark at
// or before t. 21:00 UTC under EDT, 22:00 UTC under EST.
func (c *SessionCalendar) WeeklyOpenUTC(t time.Time) time.Time {
	d := c.nyDate(t)
	for d.Weekday() != c.openDay {
		d = d.AddDate(0, 0, -1)
	}
	open := c.at(d, c.mark)
	if open.After(t) {
		open = open.AddDate(0, 0, -7)
	}
	return open
}

// WeeklyCloseUTC returns the weekly-close instant (Friday 17:00 ET) that
// ends the trading week containing t.
func (c *SessionCalendar) WeeklyCloseUTC(t time.Time) time.Time {
	d := c.nyDate(c.WeeklyOpenUTC(t))
	for d.Weekday() != c.closeDay {
		d = d.AddDate(0, 0, 1)
	}
	return c.at(d, c.mark)
}

// NextWeeklyOpen returns the first weekly-open instant strictly after t.
func (c *SessionCalendar) NextWeeklyOpen(t time.Time) time.Time {
	d := c.nyDate(t)
	for {
		for d.Weekday() != c.openDay {
			d = d.AddDate(0, 0, 1)
		}
		open := c.at(d, c.mark)
		if open.After(t) {
			return open
		}
		d = d.AddDate(0, 0, 7)
	}
}

// State resolves the session state at t: the weekly window first (the
// trading week runs from the Sunday 17:00 ET open through the Friday
// 17:00 ET close), then venue holiday overrides inside it, then the
// 15-minute pre-open accumulation ramp ahead of the next open.
func (c *SessionCalendar) State(t time.Time) SessionState {
	open := c.WeeklyOpenUTC(t)
	close_ := c.WeeklyCloseUTC(t)
	if !t.Before(open) && t.Before(close_) {
		// Inside the trading week — check venue holiday overrides on the
		// New York calendar date.
		if o, ok := c.overrideFor(c.nyDate(t)); ok {
			if o.Closed {
				return StateClosedDay
			}
			if o.EarlyCloseLocal != "" {
				if m, err := parseHM(o.EarlyCloseLocal); err == nil &&
					!t.Before(c.at(c.nyDate(t), m)) {
					return StateEarlyClose
				}
			}
		}
		return StateOpen
	}
	// Outside the weekly window: pre-open ramp ahead of the next open,
	// else weekend.
	if next := c.NextWeeklyOpen(t); !t.Before(next.Add(-c.preOpen)) {
		return StatePreOpen
	}
	return StateWeekend
}

// IsOpen reports whether continuous trading is live at t (OPEN only —
// pre-open accumulates without matching per §6.7).
func (c *SessionCalendar) IsOpen(t time.Time) bool {
	return c.State(t) == StateOpen
}

// ---------------------------------------------------------------------------
// Holiday-bound value dates (Phase-03 calendar consumption)
// ---------------------------------------------------------------------------

// RequireCalendars fails closed when any settlement center for the pair
// lacks a loaded holiday calendar (spec §2.7 pessimism).
func RequireCalendars(cal *settlement.HolidayCalendar, base, quote string) error {
	if cal == nil {
		return excerrors.New("SERVICE_DEGRADED", "holiday calendar unavailable")
	}
	for _, ccy := range settlement.SettlementCurrencies(base, quote) {
		if !cal.HasCurrency(ccy) {
			return excerrors.New("SERVICE_DEGRADED",
				fmt.Sprintf("holiday calendar: no data for %s", ccy))
		}
	}
	return nil
}

// ValidateValueDate rejects a value date landing on a pair-center
// holiday/weekend with VALUE_DATE_ON_HOLIDAY (HTTP 422, spec §7.4 item
// 2). The gateway/order path calls this before admission; the C++
// PreTradeChecker enforces the same rule on the hot path.
func ValidateValueDate(cal *settlement.HolidayCalendar, base, quote string, valueDate time.Time) error {
	if err := RequireCalendars(cal, base, quote); err != nil {
		return err
	}
	if !cal.IsMutualBusinessDay(valueDate,
		settlement.SettlementCurrencies(base, quote)...) {
		return excerrors.New("VALUE_DATE_ON_HOLIDAY",
			fmt.Sprintf("value date %s is not a mutual business day for %s/%s",
				valueDate.UTC().Format("2006-01-02"),
				strings.ToUpper(base), strings.ToUpper(quote)))
	}
	return nil
}

// RollNdfFixing shifts an NDF fixing date to the next good business day
// when it lands on a center holiday (spec §7.4 item 2).
func RollNdfFixing(cal *settlement.HolidayCalendar, base, quote string, fixingDate time.Time) (time.Time, error) {
	if err := RequireCalendars(cal, base, quote); err != nil {
		return time.Time{}, err
	}
	return cal.MutualBusinessDayOnOrAfter(fixingDate,
		settlement.SettlementCurrencies(base, quote)...), nil
}

// ---------------------------------------------------------------------------
// Tenor grid (spec §7.4 item 3)
// ---------------------------------------------------------------------------

// TenorGrid is the canonical §7.4 tenor ladder, ordered near→far.
var TenorGrid = []string{
	"ON", "TN", "SN", "1W", "2W", "1M", "2M", "3M", "6M", "9M", "1Y", "2Y",
}

// tenorSpec decomposes a tenor code into its day/week/month/year part.
type tenorSpec struct {
	days   int // calendar days for ON/TN/SN semantics handled separately
	weeks  int
	months int
	years  int
	cash   string // "ON"|"TN"|"SN" when set — business-day cash tenors
}

// parseTenor decomposes a grid code.
func parseTenor(tenor string) (tenorSpec, error) {
	t := strings.ToUpper(strings.TrimSpace(tenor))
	switch t {
	case "ON", "TN", "SN":
		return tenorSpec{cash: t}, nil
	}
	if len(t) < 2 {
		return tenorSpec{}, excerrors.New("INVALID_REQUEST", "bad tenor "+tenor)
	}
	var n int
	if _, err := fmt.Sscanf(t[:len(t)-1], "%d", &n); err != nil || n <= 0 {
		return tenorSpec{}, excerrors.New("INVALID_REQUEST", "bad tenor "+tenor)
	}
	switch t[len(t)-1] {
	case 'D':
		return tenorSpec{days: n}, nil
	case 'W':
		return tenorSpec{weeks: n}, nil
	case 'M':
		return tenorSpec{months: n}, nil
	case 'Y':
		return tenorSpec{years: n}, nil
	}
	return tenorSpec{}, excerrors.New("INVALID_REQUEST", "bad tenor "+tenor)
}

// SpotDate resolves the spot value date for the pair: tradeDay →
// settlement-cycle business days, split-holiday aware (Phase-03 engine).
func SpotDate(cal *settlement.HolidayCalendar, base, quote string,
	tradeDay time.Time, cycleDays int) (time.Time, error) {
	return cal.SettlementDate(base, quote, tradeDay, cycleDays)
}

// TenorValueDate resolves the far-leg value date for a tenor off the
// spot date. Cash tenors count mutual business days from the trade date;
// week/month/year tenors land on spot + calendar, Modified-Following
// adjusted with the end-of-month rule (spec §7.4/§17.5 ISDA convention).
func TenorValueDate(cal *settlement.HolidayCalendar, base, quote string,
	tradeDay time.Time, cycleDays int, tenor string) (time.Time, error) {

	spec, err := parseTenor(tenor)
	if err != nil {
		return time.Time{}, err
	}
	if err := RequireCalendars(cal, base, quote); err != nil {
		return time.Time{}, err
	}
	ccys := settlement.SettlementCurrencies(base, quote)
	spot, err := cal.SettlementDate(base, quote, tradeDay, cycleDays)
	if err != nil {
		return time.Time{}, err
	}
	trade := normalizeUTC(tradeDay)

	switch spec.cash {
	case "ON":
		// Overnight: trade date → next mutual business day.
		return cal.NextMutualBusinessDay(trade, ccys...), nil
	case "TN":
		// Tom-Next: T+1 → T+2 — the far leg lands one mutual business
		// day after the overnight date.
		on := cal.NextMutualBusinessDay(trade, ccys...)
		return cal.NextMutualBusinessDay(on, ccys...), nil
	case "SN":
		// Spot-Next: spot → next mutual business day.
		return cal.NextMutualBusinessDay(spot, ccys...), nil
	}

	var nominal time.Time
	switch {
	case spec.days > 0:
		nominal = spot.AddDate(0, 0, spec.days)
	case spec.weeks > 0:
		nominal = spot.AddDate(0, 0, 7*spec.weeks)
	case spec.months > 0:
		nominal = spot.AddDate(0, spec.months, 0)
	case spec.years > 0:
		nominal = spot.AddDate(spec.years, 0, 0)
	default:
		return time.Time{}, excerrors.New("INVALID_REQUEST", "bad tenor "+tenor)
	}

	// End-of-month rule: when spot is the last mutual business day of
	// its month, the forward lands on the last mutual business day of
	// the target month.
	if isLastMutualBusinessDay(cal, spot, ccys) {
		eom := time.Date(nominal.Year(), nominal.Month()+1, 0,
			0, 0, 0, 0, time.UTC)
		return cal.MutualBusinessDayOnOrBefore(eom, ccys...), nil
	}
	return cal.AdjustModifiedFollowing(nominal, ccys...), nil
}

// isLastMutualBusinessDay reports whether d is the last mutual business
// day of its calendar month.
func isLastMutualBusinessDay(cal *settlement.HolidayCalendar, d time.Time, ccys []string) bool {
	eom := time.Date(d.Year(), d.Month()+1, 0, 0, 0, 0, 0, time.UTC)
	return normalizeUTC(cal.MutualBusinessDayOnOrBefore(eom, ccys...)).Equal(normalizeUTC(d))
}

func normalizeUTC(t time.Time) time.Time {
	u := t.UTC()
	return time.Date(u.Year(), u.Month(), u.Day(), 0, 0, 0, 0, time.UTC)
}

// TenorBracket locates a broken value date between grid tenors and
// returns the bracketing codes plus the linear-interpolation weight w in
// [0,1] such that points ≈ lower + w·(upper−lower) by calendar-day
// position — the §7.4 broken-date interpolation rule.
func TenorBracket(cal *settlement.HolidayCalendar, base, quote string,
	tradeDay time.Time, cycleDays int,
	valueDate time.Time) (lower, upper string, w float64, err error) {

	vd := normalizeUTC(valueDate)
	dates := make([]time.Time, 0, len(TenorGrid))
	for _, t := range TenorGrid {
		d, err := TenorValueDate(cal, base, quote, tradeDay, cycleDays, t)
		if err != nil {
			return "", "", 0, err
		}
		dates = append(dates, normalizeUTC(d))
	}
	if vd.Before(dates[0]) {
		return "", "", 0, excerrors.New("INVALID_REQUEST",
			"broken date "+vd.Format("2006-01-02")+" precedes the ON tenor date")
	}
	for i := 0; i < len(TenorGrid); i++ {
		if vd.Equal(dates[i]) {
			return TenorGrid[i], TenorGrid[i], 1.0, nil
		}
		if i+1 < len(TenorGrid) && vd.Before(dates[i+1]) {
			span := dates[i+1].Sub(dates[i])
			w = float64(vd.Sub(dates[i])) / float64(span)
			return TenorGrid[i], TenorGrid[i+1], w, nil
		}
	}
	return "", "", 0, excerrors.New("INVALID_REQUEST",
		"broken date "+vd.Format("2006-01-02")+" is beyond the 2Y grid edge")
}

// ---------------------------------------------------------------------------
// IMM stub calendar + option-expiry cuts
// ---------------------------------------------------------------------------

// IMMMonths are the quarterly IMM cycle months (Mar/Jun/Sep/Dec).
var IMMMonths = []time.Month{time.March, time.June, time.September, time.December}

// IMMDate returns the third Wednesday of (year, month) — the IMM stub
// convention.
func IMMDate(year int, month time.Month) time.Time {
	d := time.Date(year, month, 1, 0, 0, 0, 0, time.UTC)
	for d.Weekday() != time.Wednesday {
		d = d.AddDate(0, 0, 1)
	}
	return d.AddDate(0, 0, 14) // third Wednesday
}

// NextIMMDate returns the first IMM date strictly after d.
func NextIMMDate(d time.Time) time.Time {
	d = normalizeUTC(d)
	years := []int{d.Year(), d.Year() + 1}
	for _, y := range years {
		for _, m := range IMMMonths {
			imm := IMMDate(y, m)
			if imm.After(d) {
				return imm
			}
		}
	}
	return IMMDate(d.Year()+2, IMMMonths[0]) // unreachable in practice
}

// OptionCut is the option-expiry cut convention.
type OptionCut string

const (
	// CutNdfFixing is the 10:00 New York NDF fixing cut (spec §7.4/§6.4).
	CutNdfFixing OptionCut = "NDF_FIXING_10NY"
	// CutVenue is the 15:00 UTC venue option-expiry cut (Task 22.3.10).
	CutVenue OptionCut = "VENUE_15UTC"
)

// OptionExpiryInstant resolves the expiry instant for a value date under
// the given cut. The date is expected to be holiday-rolled already —
// RollOptionExpiry does that composition.
func (c *SessionCalendar) OptionExpiryInstant(valueDate time.Time, cut OptionCut) (time.Time, error) {
	d := normalizeUTC(valueDate)
	switch cut {
	case CutNdfFixing:
		return time.Date(d.Year(), d.Month(), d.Day(), 10, 0, 0, 0, c.ny), nil
	case CutVenue:
		return time.Date(d.Year(), d.Month(), d.Day(), 15, 0, 0, 0, time.UTC), nil
	}
	return time.Time{}, excerrors.New("INVALID_REQUEST", "unknown option cut "+string(cut))
}

// RollOptionExpiry rolls a holiday-landing expiry forward to the next
// mutual business day, then resolves the cut instant.
func (c *SessionCalendar) RollOptionExpiry(cal *settlement.HolidayCalendar,
	base, quote string, expiryDate time.Time, cut OptionCut) (time.Time, error) {
	rolled, err := RollNdfFixing(cal, base, quote, expiryDate)
	if err != nil {
		return time.Time{}, err
	}
	return c.OptionExpiryInstant(rolled, cut)
}

// ---------------------------------------------------------------------------
// Convenience: value-date validation entry point for the order path
// ---------------------------------------------------------------------------

// SessionService bundles the calendar + holiday engine behind one
// injectable seam so the order-admission path takes a single dependency.
type SessionService struct {
	cal *SessionCalendar
	hol *settlement.HolidayCalendar
}

// NewSessionService wires the seam. hol may be nil — every holiday-bound
// method then fails closed SERVICE_DEGRADED rather than guessing.
func NewSessionService(cal *SessionCalendar, hol *settlement.HolidayCalendar) *SessionService {
	return &SessionService{cal: cal, hol: hol}
}

// Calendar exposes the venue session calendar.
func (s *SessionService) Calendar() *SessionCalendar { return s.cal }

// CheckValueDate is the order-path gate (VALUE_DATE_ON_HOLIDAY, 422).
func (s *SessionService) CheckValueDate(_ context.Context, base, quote string, valueDate time.Time) error {
	return ValidateValueDate(s.hol, base, quote, valueDate)
}

// SortedInstrumentHolidayCurrencies lists every currency whose holiday
// calendar must exist for the given symbols to be tradable for value —
// the reload-settlement health probe's input.
func SortedInstrumentHolidayCurrencies(symbols []struct{ Base, Quote string }) []string {
	set := map[string]bool{}
	for _, s := range symbols {
		for _, c := range settlement.SettlementCurrencies(s.Base, s.Quote) {
			set[c] = true
		}
	}
	out := make([]string, 0, len(set))
	for c := range set {
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}
