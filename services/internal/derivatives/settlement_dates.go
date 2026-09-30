// settlement_dates.go — Phase-22 dated-instrument value-date math.
//
// This file is a thin derivatives-facing façade over the canonical
// engines — it owns NO date logic of its own:
//   - settlement.HolidayCalendar (Phase-03 Task 3.3.8): mutual-business-
//     day arithmetic under ISDA Modified Following across base, quote and
//     USD-for-cross settlement centers;
//   - instruments.SpotDate / TenorValueDate / ValidateValueDate /
//     RollNdfFixing (Phase-15 Task 15.3.11): the §7.4 spot-date +
//     tenor-grid machinery and the order-path holiday gate.
//
// Conventions (spec §6.3, §7.4): T+1 default, T+2 exotics, same-day
// USD/CAD and USD/MXN; an explicit forward/NDF value date that lands on
// a holiday rejects VALUE_DATE_ON_HOLIDAY; an NDF fixing date on a
// holiday rolls forward to the next good business day.
package derivatives

import (
	"fmt"
	"time"

	"exchange/internal/instruments"
	"exchange/internal/settlement"
	excerrors "exchange/pkg/errors"
)

// Dates wraps the holiday calendar for the derivatives package.
type Dates struct {
	cal *settlement.HolidayCalendar
}

// NewDates wires the façade; a nil calendar fails closed on every
// holiday-bound call (spec §2.7).
func NewDates(cal *settlement.HolidayCalendar) *Dates { return &Dates{cal: cal} }

// Calendar exposes the underlying holiday engine.
func (d *Dates) Calendar() *settlement.HolidayCalendar { return d.cal }

// DefaultSpotCycleDays is the spec §6.3 fallback settlement cycle when no
// instrument row is in hand: same-day for USD/CAD and USD/MXN, T+1
// otherwise. T+2 exotics are assigned per-instrument via
// instruments.settlement_cycle — the row is always authoritative; this
// helper exists for pure pricing paths that pre-date an instrument.
func DefaultSpotCycleDays(base, quote string) int {
	p, err := NewPair(base, quote)
	if err != nil {
		return 1
	}
	if (p.Base == "USD" && p.Quote == "CAD") ||
		(p.Base == "USD" && p.Quote == "MXN") {
		return 0
	}
	return 1
}

// SpotDate resolves the pair's spot value date: tradeDay + cycleDays
// mutual business days, split-holiday aware (T+0/T+1/T+2).
func (d *Dates) SpotDate(pair Pair, tradeDay time.Time, cycleDays int) (time.Time, error) {
	if err := instruments.RequireCalendars(d.cal, pair.Base, pair.Quote); err != nil {
		return time.Time{}, err
	}
	return instruments.SpotDate(d.cal, pair.Base, pair.Quote, tradeDay, cycleDays)
}

// TenorDate resolves a §7.4-grid tenor's forward value date off the
// holiday-adjusted spot date (ON/TN/SN cash tenors; W/M/Y tenors Modified
// Following with the end-of-month rule).
func (d *Dates) TenorDate(pair Pair, tradeDay time.Time, cycleDays int, tenor string) (time.Time, error) {
	return instruments.TenorValueDate(d.cal, pair.Base, pair.Quote, tradeDay, cycleDays, tenor)
}

// CheckValueDate is the explicit-date admission gate: the forward/NDF
// value date must be a mutual business day across the pair's settlement
// centers (VALUE_DATE_ON_HOLIDAY, 422) and strictly after the spot date —
// a "forward" settling on/before spot is a spot trade, not a forward.
func (d *Dates) CheckValueDate(pair Pair, spotDate, valueDate time.Time) error {
	if err := instruments.ValidateValueDate(d.cal, pair.Base, pair.Quote, valueDate); err != nil {
		return err
	}
	if !normDay(valueDate).After(normDay(spotDate)) {
		return excerrors.New(CodeInvalidRequest, fmt.Sprintf(
			"forward value date %s must be after spot value date %s for %s",
			valueDate.Format("2006-01-02"), spotDate.Format("2006-01-02"), pair.Symbol()))
	}
	return nil
}

// CheckSwapDates validates the near/far leg pair: both must be mutual
// business days and the far leg strictly after the near leg.
func (d *Dates) CheckSwapDates(pair Pair, nearDate, farDate time.Time) error {
	if err := instruments.ValidateValueDate(d.cal, pair.Base, pair.Quote, nearDate); err != nil {
		return err
	}
	if err := instruments.ValidateValueDate(d.cal, pair.Base, pair.Quote, farDate); err != nil {
		return err
	}
	if !normDay(farDate).After(normDay(nearDate)) {
		return excerrors.New(CodeInvalidRequest, fmt.Sprintf(
			"swap far-leg %s must be after near-leg %s for %s",
			farDate.Format("2006-01-02"), nearDate.Format("2006-01-02"), pair.Symbol()))
	}
	return nil
}

// RollNdfFixing shifts an NDF fixing date to the next good business day
// when it lands on a settlement-center holiday (spec §7.4 item 2).
func (d *Dates) RollNdfFixing(pair Pair, fixingDate time.Time) (time.Time, error) {
	return instruments.RollNdfFixing(d.cal, pair.Base, pair.Quote, fixingDate)
}

// IsMutualBusinessDay reports the settle-ready condition for a value
// date across the pair's centers.
func (d *Dates) IsMutualBusinessDay(pair Pair, day time.Time) bool {
	if d.cal == nil {
		return false
	}
	return d.cal.IsMutualBusinessDay(day,
		settlement.SettlementCurrencies(pair.Base, pair.Quote)...)
}

// normDay truncates t to its UTC calendar day.
func normDay(t time.Time) time.Time {
	u := t.UTC()
	return time.Date(u.Year(), u.Month(), u.Day(), 0, 0, 0, 0, time.UTC)
}
