// Multi-Currency Holiday Calendar Engine — Task 3.3.8 (spec §17.5, §24 #123).
//
// HolidayCalendarService evaluates FX spot value dates under the ISDA
// Modified Following Business Day Convention across per-currency banking
// calendars seeded by migration 150 (Federal Reserve, TARGET2, BoE, BoJ,
// SNB/SIC, Australian/NZ, BoC, Banxico; 2025–2027). It is consumed by:
//
//   - settlement: SettlementDate computes the T+0/T+1/T+2 value date for a
//     currency pair, shifting forward past weekends and bank holidays in
//     ANY settlement center (base, quote, plus USD for cross pairs) to the
//     next mutual business day;
//   - rollover (Task 3.3.7/3.3.11): RollValueDate shifts a position's value
//     date holiday-aware; DaysBetween yields the calendar-day span so
//     holiday weekends correctly produce 4x/5x swap multipliers.
//
// Fail-closed (spec §2.7): a pair leg whose currency has no loaded calendar
// is an error, never a silent "no holidays" assumption.
package settlement

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Querier is the minimal pgx surface the settlement services read through;
// *pgxpool.Pool satisfies it.
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Execer is the minimal pgx write surface (holiday ingestion).
type Execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// DB is the combined read/write surface used by holiday ingestion.
type DB interface {
	Querier
	Execer
}

// usd is the settlement vehicle: per spec §17.5 cross pairs that touch no
// USD leg still require the USD calendar, since USD holidays halt the
// settlement rail both legs ride on.
const usd = "USD"

var currencyRe = regexp.MustCompile(`^[A-Z]{3}$`)

// Holiday is one banking-closure record for a currency center.
type Holiday struct {
	Currency string    // ISO-4217, uppercase
	Date     time.Time // normalized to UTC midnight on load
	Name     string
	Source   string // e.g. FEDERAL_RESERVE, TARGET2, BANK_OF_JAPAN
}

// HolidayCalendar is the in-memory calendar loaded from currency_holidays.
// Safe for concurrent use; Load swaps the whole map atomically.
type HolidayCalendar struct {
	mu       sync.RWMutex
	holidays map[string]map[int]Holiday // currency -> civil date (yyyymmdd) -> holiday
}

// NewHolidayCalendar builds a calendar from a static holiday set
// (tests, embedded fallbacks). Production construction is LoadCalendar.
func NewHolidayCalendar(holidays []Holiday) (*HolidayCalendar, error) {
	c := &HolidayCalendar{holidays: map[string]map[int]Holiday{}}
	for _, h := range holidays {
		if err := c.AddHoliday(h); err != nil {
			return nil, err
		}
	}
	return c, nil
}

// LoadCalendar reads every row of currency_holidays into a fresh calendar.
// A malformed row (bad currency code, NULL name) fails the whole load —
// a partial calendar must never serve value-date math (spec §2.7).
func LoadCalendar(ctx context.Context, q Querier) (*HolidayCalendar, error) {
	c := &HolidayCalendar{}
	if err := c.Reload(ctx, q); err != nil {
		return nil, err
	}
	return c, nil
}

// Reload re-reads currency_holidays and atomically swaps the live set.
// On error the previous calendar stays in force.
func (c *HolidayCalendar) Reload(ctx context.Context, q Querier) error {
	rows, err := q.Query(ctx,
		`SELECT currency, holiday_date, name, source FROM currency_holidays`)
	if err != nil {
		return fmt.Errorf("calendar load query: %w", err)
	}
	defer rows.Close()

	fresh := map[string]map[int]Holiday{}
	for rows.Next() {
		var (
			ccy, name, source string
			d                 time.Time
		)
		if err := rows.Scan(&ccy, &d, &name, &source); err != nil {
			return fmt.Errorf("calendar load scan: %w", err)
		}
		h := Holiday{
			Currency: strings.ToUpper(strings.TrimSpace(ccy)),
			Date:     normalizeDay(d),
			Name:     name,
			Source:   source,
		}
		if err := validateHoliday(h); err != nil {
			return err
		}
		set, ok := fresh[h.Currency]
		if !ok {
			set = map[int]Holiday{}
			fresh[h.Currency] = set
		}
		set[civilOf(h.Date)] = h
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("calendar load rows: %w", err)
	}
	if len(fresh) == 0 {
		return fmt.Errorf("calendar load: currency_holidays is empty")
	}

	c.mu.Lock()
	c.holidays = fresh
	c.mu.Unlock()
	return nil
}

// validateHoliday enforces the row contract before a record is trusted.
func validateHoliday(h Holiday) error {
	if !currencyRe.MatchString(h.Currency) {
		return fmt.Errorf("calendar: invalid currency code %q", h.Currency)
	}
	if strings.TrimSpace(h.Name) == "" {
		return fmt.Errorf("calendar: %s %s has empty holiday name",
			h.Currency, h.Date.Format("2006-01-02"))
	}
	return nil
}

// AddHoliday validates and inserts one holiday (tests / admin ingestion).
func (c *HolidayCalendar) AddHoliday(h Holiday) error {
	h.Currency = strings.ToUpper(strings.TrimSpace(h.Currency))
	h.Date = normalizeDay(h.Date)
	if err := validateHoliday(h); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	set, ok := c.holidays[h.Currency]
	if !ok {
		set = map[int]Holiday{}
		c.holidays[h.Currency] = set
	}
	set[civilOf(h.Date)] = h
	return nil
}

// StoreHolidays upserts holiday rows into currency_holidays — the ingestion
// path for refreshed central-bank calendars. Rows are validated before any
// write; invalid input aborts with nothing persisted in this call.
func StoreHolidays(ctx context.Context, db DB, holidays []Holiday) error {
	for i := range holidays {
		h := &holidays[i]
		h.Currency = strings.ToUpper(strings.TrimSpace(h.Currency))
		h.Date = normalizeDay(h.Date)
		if err := validateHoliday(*h); err != nil {
			return err
		}
	}
	for _, h := range holidays {
		_, err := db.Exec(ctx,
			`INSERT INTO currency_holidays (currency, holiday_date, name, source)
			 VALUES ($1, $2, $3, $4)
			 ON CONFLICT (currency, holiday_date)
			 DO UPDATE SET name = EXCLUDED.name, source = EXCLUDED.source`,
			h.Currency, h.Date, h.Name, h.Source)
		if err != nil {
			return fmt.Errorf("calendar store %s %s: %w",
				h.Currency, h.Date.Format("2006-01-02"), err)
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Calendar queries
// ---------------------------------------------------------------------------

// HasCurrency reports whether a holiday calendar is loaded for ccy.
func (c *HolidayCalendar) HasCurrency(ccy string) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	_, ok := c.holidays[strings.ToUpper(ccy)]
	return ok
}

// Currencies returns the sorted set of loaded currency calendars.
func (c *HolidayCalendar) Currencies() []string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]string, 0, len(c.holidays))
	for ccy := range c.holidays {
		out = append(out, ccy)
	}
	sort.Strings(out)
	return out
}

// Holiday returns the holiday record for (ccy, d) if one exists.
func (c *HolidayCalendar) Holiday(ccy string, d time.Time) (Holiday, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	h, ok := c.holidays[strings.ToUpper(ccy)][civilOf(normalizeDay(d))]
	return h, ok
}

// IsHoliday reports whether d is a banking holiday for ccy. A currency with
// no loaded calendar reports false here, but SettlementDate refuses such
// currencies outright — pair-level callers always take the fail-closed path.
func (c *HolidayCalendar) IsHoliday(ccy string, d time.Time) bool {
	_, ok := c.Holiday(ccy, d)
	return ok
}

// IsBusinessDay reports whether d is a good business day for ccy:
// a weekday (Mon–Fri; the FX week runs Sunday 21:00 UTC to Friday 22:00 UTC)
// that is not a banking holiday in that currency's center.
func (c *HolidayCalendar) IsBusinessDay(ccy string, d time.Time) bool {
	wd := normalizeDay(d).Weekday()
	if wd == time.Saturday || wd == time.Sunday {
		return false
	}
	return !c.IsHoliday(ccy, d)
}

// IsMutualBusinessDay reports whether d is a business day in EVERY listed
// currency center — the settlement-ready condition for a value date.
func (c *HolidayCalendar) IsMutualBusinessDay(d time.Time, ccys ...string) bool {
	for _, ccy := range ccys {
		if !c.IsBusinessDay(ccy, d) {
			return false
		}
	}
	return true
}

// requireCurrencies is the fail-closed gate: every settlement center must
// have a loaded calendar, otherwise we cannot know the holidays.
func (c *HolidayCalendar) requireCurrencies(ccys []string) error {
	for _, ccy := range ccys {
		if !c.HasCurrency(ccy) {
			return fmt.Errorf("calendar: no holiday data for currency %q", ccy)
		}
	}
	return nil
}

// SettlementCurrencies returns the centers whose calendars gate a pair's
// value date: {base, quote}, plus USD for cross pairs (spec §17.5 — USD
// holidays halt settlement even when neither leg is USD).
func SettlementCurrencies(base, quote string) []string {
	base = strings.ToUpper(strings.TrimSpace(base))
	quote = strings.ToUpper(strings.TrimSpace(quote))
	set := map[string]bool{base: true, quote: true}
	if base != usd && quote != usd {
		set[usd] = true
	}
	out := make([]string, 0, len(set))
	for ccy := range set {
		out = append(out, ccy)
	}
	sort.Strings(out)
	return out
}

// ---------------------------------------------------------------------------
// Business-day arithmetic
// ---------------------------------------------------------------------------

// NextMutualBusinessDay returns the first mutual business day strictly
// after d across all listed centers.
func (c *HolidayCalendar) NextMutualBusinessDay(d time.Time, ccys ...string) time.Time {
	d = normalizeDay(d).AddDate(0, 0, 1)
	for !c.IsMutualBusinessDay(d, ccys...) {
		d = d.AddDate(0, 0, 1)
	}
	return d
}

// MutualBusinessDayOnOrAfter returns d itself when it is a mutual business
// day, else the first following one.
func (c *HolidayCalendar) MutualBusinessDayOnOrAfter(d time.Time, ccys ...string) time.Time {
	d = normalizeDay(d)
	for !c.IsMutualBusinessDay(d, ccys...) {
		d = d.AddDate(0, 0, 1)
	}
	return d
}

// MutualBusinessDayOnOrBefore returns d itself when it is a mutual business
// day, else the last preceding one — the "Modified" leg of Modified
// Following used at month boundaries.
func (c *HolidayCalendar) MutualBusinessDayOnOrBefore(d time.Time, ccys ...string) time.Time {
	d = normalizeDay(d)
	for !c.IsMutualBusinessDay(d, ccys...) {
		d = d.AddDate(0, 0, -1)
	}
	return d
}

// AdjustModifiedFollowing applies the ISDA Modified Following Business Day
// convention to a single nominal date: roll forward to the next mutual
// business day, unless that crosses into the next calendar month, in which
// case roll back to the last mutual business day of the nominal month.
func (c *HolidayCalendar) AdjustModifiedFollowing(d time.Time, ccys ...string) time.Time {
	nominal := normalizeDay(d)
	fwd := c.MutualBusinessDayOnOrAfter(nominal, ccys...)
	if fwd.Year() == nominal.Year() && fwd.Month() == nominal.Month() {
		return fwd
	}
	eom := time.Date(nominal.Year(), nominal.Month()+1, 0, 0, 0, 0, 0, time.UTC)
	return c.MutualBusinessDayOnOrBefore(eom, ccys...)
}

// SettlementDate computes the spot value date for a trade:
//
//	cycle 0  — same-day pairs (USD/CAD, USD/MXN per spec §6.3): the value
//	           date is the trade date Modified-Following adjusted.
//	cycle n  — T+n: count n mutual business days forward from the trade
//	           date; a day counts only when every settlement center (base,
//	           quote, plus USD for cross pairs) is open. Split holidays —
//	           a bank holiday in one center but not the other — therefore
//	           shift the value date to the next mutual business day.
//
//	Modified Following month guard: if the walk overshoots the calendar
//	month of the nominal (calendar-day) target, the value date pulls back
//	to the last mutual business day inside that month — standard FX
//	month-end behaviour (a month-end holiday shortens settlement rather
//	than pushing it into the next month). The pull-back can never produce
//	a value date on/before the trade date for T+n≥1; if it would, the
//	forward result stands (defensive — an un-settleable month is a
//	calendar-data defect, surfaced loudly rather than silently accepted).
//
// tradeDate is normalized to its UTC calendar day.
func (c *HolidayCalendar) SettlementDate(base, quote string, tradeDate time.Time, cycleDays int) (time.Time, error) {
	base = strings.ToUpper(strings.TrimSpace(base))
	quote = strings.ToUpper(strings.TrimSpace(quote))
	if !currencyRe.MatchString(base) || !currencyRe.MatchString(quote) {
		return time.Time{}, fmt.Errorf("calendar: invalid pair %s/%s", base, quote)
	}
	if base == quote {
		return time.Time{}, fmt.Errorf("calendar: degenerate pair %s/%s", base, quote)
	}
	if cycleDays < 0 || cycleDays > 2 {
		return time.Time{}, fmt.Errorf(
			"calendar: unsupported spot cycle T+%d (expected 0, 1 or 2; forwards carry explicit value dates)", cycleDays)
	}
	ccys := SettlementCurrencies(base, quote)
	if err := c.requireCurrencies(ccys); err != nil {
		return time.Time{}, err
	}

	trade := normalizeDay(tradeDate)
	if cycleDays == 0 {
		return c.AdjustModifiedFollowing(trade, ccys...), nil
	}

	value := trade
	for i := 0; i < cycleDays; i++ {
		value = c.NextMutualBusinessDay(value, ccys...)
	}

	nominal := trade.AddDate(0, 0, cycleDays)
	if value.Year() != nominal.Year() || value.Month() != nominal.Month() {
		eom := time.Date(nominal.Year(), nominal.Month()+1, 0, 0, 0, 0, 0, time.UTC)
		pulled := c.MutualBusinessDayOnOrBefore(eom, ccys...)
		if pulled.After(trade) {
			value = pulled
		}
		// pulled <= trade: every mutual business day of the nominal month
		// is on/before the trade date; keep the forward-rolled value.
	}
	return value, nil
}

// RollValueDate advances an open position's value date to the next mutual
// business day after from — the Tom-Next roll leg. Pair-aware via the same
// settlement-currency set as SettlementDate (fail-closed on missing data).
func (c *HolidayCalendar) RollValueDate(base, quote string, from time.Time) (time.Time, error) {
	ccys := SettlementCurrencies(base, quote)
	if err := c.requireCurrencies(ccys); err != nil {
		return time.Time{}, err
	}
	return c.NextMutualBusinessDay(from, ccys...), nil
}

// DaysBetween returns the calendar-day span from a to b (b >= a, both
// normalized to UTC days). The rollover engine divides swap days by this
// span, so a roll landing over a holiday weekend yields 4x/5x multipliers.
func DaysBetween(a, b time.Time) int {
	return int(normalizeDay(b).Sub(normalizeDay(a)) / (24 * time.Hour))
}

// ---------------------------------------------------------------------------
// Date normalization helpers
// ---------------------------------------------------------------------------

// normalizeDay truncates t to its UTC calendar day.
func normalizeDay(t time.Time) time.Time {
	u := t.UTC()
	return time.Date(u.Year(), u.Month(), u.Day(), 0, 0, 0, 0, time.UTC)
}

// civilOf encodes a normalized day as a yyyymmdd int map key.
func civilOf(d time.Time) int {
	return d.Year()*10000 + int(d.Month())*100 + d.Day()
}
