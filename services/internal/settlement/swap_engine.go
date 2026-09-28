// Overnight Swap Rate Engine — Phase-03 Task 3.3.11
// (spec §17.4, §24 #221; feeds §17.15 swap-rate history).
//
// At each daily 17:00 ET rollover cutoff the engine computes the Tom-Next
// financing accrual for every open position:
//
//	swap_charge = position_qty(lots) × swap_points × lot_size   (per day)
//
// day-multiplied by the calendar span the roll covers — 1 on a normal day,
// 3 on Wednesday (the interbank T+2 value date moves Fri→Mon), 4–5 when a
// currency holiday lengthens the span (Task 3.3.8 calendar does the
// business-day math). Positive swap points credit the position holder;
// negative points debit — negative policy rates therefore post
// symmetrically (spec §5.21a.2). The admin markup leg (swap_markup_policies,
// migration 088) is priced by ledger.ComputeSwapAccrual, which also emits
// the balanced GL journal; every accrual — including swap-free zero rows —
// lands in swap_accrual_records.
//
// Division of labour: THIS file owns the rate resolution, day-count and
// charge math plus the posting/notify pipeline per position. The EOD cron
// and settlement_intent filtering live in rollover_service.go (Task 3.3.7,
// parallel-owned) — it supplies the position set and calls
// ProcessRollover; nothing here builds a second scheduler.
//
// Timezone: the cutoff is 17:00 in America/New_York — 21:00 UTC under EDT
// (summer), 22:00 UTC under EST (winter). The zone is resolved through
// time.LoadLocation and the UTC instant is DERIVED, never hardcoded
// (remediation #35 corrected the inverted pairing).
//
// Fail-closed (spec §2.7): missing/stale rates, unknown calendars and bad
// inputs are coded per-position failures collected in the RolloverReport —
// a position we cannot price is skipped loudly, never charged a guess.
package settlement

import (
	"context"
	"encoding/json"
	stderrors "errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"

	"exchange/internal/ledger"
	excerrors "exchange/pkg/errors"
)

// Codes emitted by the engine. SWAP_RATE_STALE and
// ROLLOVER_CALCULATION_ERROR are spec §23-registered (503 L1 / 500 L1);
// Phase-05 Task 5.3.21 binds HTTP statuses centrally.
const (
	CodeRolloverCalcError = "ROLLOVER_CALCULATION_ERROR" // spec §23: 500, L1
	// CodeSwapRateStale is declared in swap_rates.go (spec §23: 503, L1).
)

// SwapValueDateLag is the business-day lag the financing day-count is
// measured on. Spec §17.4 item 4 anchors the rollover book to the standard
// interbank T+2 value-date convention — the Wednesday roll advances the
// value date Friday→Monday and so accrues 3 calendar days ("triple-swap
// Wednesday") uniformly across spot pairs. An instrument's
// settlement_cycle governs DELIVERY settlement (Task 3.3.8
// SettlementDate), not the financing day-count; pairs needing their own
// convention use RolloverDaysForLag.
const SwapValueDateLag = 2

// DefaultSwapRateStalenessDays bounds how old a fallback rate may be when
// the roll date's own row is absent (vendor non-publish days); beyond it
// the position fails SWAP_RATE_STALE rather than accrue on a guess.
const DefaultSwapRateStalenessDays = 3

// ---------------------------------------------------------------------------
// RolloverClock — the DST-aware 17:00 America/New_York cutoff
// ---------------------------------------------------------------------------

// RolloverClock converts civil dates in the rollover zone into exact UTC
// cutoff instants. The zone+time are configurable per session (spec
// §7.4/§15.3 session overrides); production uses 17:00 America/New_York.
type RolloverClock struct {
	loc    *time.Location
	hour   int
	minute int
}

// DefaultRolloverClock is the canonical 17:00 ET rollover boundary.
func DefaultRolloverClock() (*RolloverClock, error) {
	return NewRolloverClock("America/New_York", 17, 0)
}

// NewRolloverClock builds a cutoff clock for locName (an IANA zone like
// "America/New_York") at hour:minute local. A bad zone or time fails at
// construction — a misconfigured rollover clock must never run silently.
func NewRolloverClock(locName string, hour, minute int) (*RolloverClock, error) {
	loc, err := time.LoadLocation(locName)
	if err != nil {
		return nil, fmt.Errorf("rollover clock: load location %q: %w", locName, err)
	}
	if hour < 0 || hour > 23 || minute < 0 || minute > 59 {
		return nil, fmt.Errorf("rollover clock: invalid cutoff %02d:%02d", hour, minute)
	}
	return &RolloverClock{loc: loc, hour: hour, minute: minute}, nil
}

// Location exposes the configured zone (America/New_York).
func (c *RolloverClock) Location() *time.Location { return c.loc }

// CutoffOn returns the instant of the cutoff on the civil date that day
// falls on in the clock's zone. Under America/New_York this resolves to
// 21:00 UTC in EDT and 22:00 UTC in EST — the UTC mapping is derived from
// the zone rules, so DST transitions are handled structurally.
func (c *RolloverClock) CutoffOn(day time.Time) time.Time {
	d := day.In(c.loc)
	return time.Date(d.Year(), d.Month(), d.Day(), c.hour, c.minute, 0, 0, c.loc)
}

// NextCutoff returns the first cutoff instant strictly after now.
func (c *RolloverClock) NextCutoff(now time.Time) time.Time {
	c0 := c.CutoffOn(now)
	if now.Before(c0) {
		return c0
	}
	return c.CutoffOn(now.In(c.loc).AddDate(0, 0, 1))
}

// LastCutoffOnOrBefore returns the most recent cutoff instant at or
// before now.
func (c *RolloverClock) LastCutoffOnOrBefore(now time.Time) time.Time {
	c0 := c.CutoffOn(now)
	if !now.Before(c0) {
		return c0
	}
	return c.CutoffOn(now.In(c.loc).AddDate(0, 0, -1))
}

// RollDate returns the rollover's civil date in the clock's zone,
// normalized to UTC midnight — the swap_rates.effective_date and the
// RolloverDays input.
func (c *RolloverClock) RollDate(instant time.Time) time.Time {
	d := instant.In(c.loc)
	return time.Date(d.Year(), d.Month(), d.Day(), 0, 0, 0, 0, time.UTC)
}

// ---------------------------------------------------------------------------
// Rollover day-count (triple-swap Wednesday, holiday-aware 4x/5x)
// ---------------------------------------------------------------------------

// RolloverDays returns the number of calendar days of financing a roll on
// rollDate accrues, under the canonical T+2 interbank value-date
// convention (SwapValueDateLag). The roll advances the position's value
// date by one mutual business day; the accrual multiplier is the calendar
// span crossed — 1 normally, 3 on Wednesday (Fri→Mon), 4–5 when a currency
// holiday lengthens the crossing. Which weekday carries the multiple is
// DERIVED from the calendar, so split-holiday pairs naturally move their
// triple day (spec §17.4 note).
func RolloverDays(cal *HolidayCalendar, base, quote string, rollDate time.Time) (int, error) {
	return RolloverDaysForLag(cal, base, quote, rollDate, SwapValueDateLag)
}

// RolloverDaysForLag is RolloverDays with an explicit business-day lag —
// for instruments whose financing convention differs from T+2.
func RolloverDaysForLag(cal *HolidayCalendar, base, quote string, rollDate time.Time, lag int) (int, error) {
	if cal == nil {
		return 0, excerrors.New(CodeRolloverCalcError, "rollover days: nil holiday calendar")
	}
	if lag < 1 || lag > 5 {
		return 0, excerrors.New(CodeRolloverCalcError,
			fmt.Sprintf("rollover days: lag %d out of range [1,5]", lag))
	}
	ccys := SettlementCurrencies(base, quote)
	if err := cal.requireCurrencies(ccys); err != nil {
		return 0, excerrors.Wrap(CodeRolloverCalcError, "rollover days", err)
	}
	v := normalizeDay(rollDate)
	for i := 0; i < lag; i++ {
		v = cal.NextMutualBusinessDay(v, ccys...)
	}
	vTo := cal.NextMutualBusinessDay(v, ccys...)
	return DaysBetween(v, vTo), nil
}

// ---------------------------------------------------------------------------
// Charge math
// ---------------------------------------------------------------------------

// InterbankSwapCharge implements the Task 3.3.11 formula
//
//	swap_charge = position_qty(lots) × swap_points × lot_size
//
// day-multiplied. positionQty is carried in base-currency UNITS (the
// positions-table convention: min_order_qty == lot_size), so
// lots = qty / lotSize and the product reduces to qty × points × days —
// written out literally so the audit trail mirrors the task formula.
// The result is signed like swapPoints and rounded to the 8dp quantum.
func InterbankSwapCharge(positionQty, lotSize, swapPoints decimal.Decimal, days int) (decimal.Decimal, error) {
	if days < 1 {
		return decimal.Zero, excerrors.New(CodeRolloverCalcError,
			fmt.Sprintf("swap charge days=%d must be >= 1", days))
	}
	if !positionQty.IsPositive() {
		return decimal.Zero, excerrors.New(CodeRolloverCalcError,
			"swap charge requires positive position quantity")
	}
	if !lotSize.IsPositive() {
		return decimal.Zero, excerrors.New(CodeRolloverCalcError,
			"swap charge requires positive lot_size")
	}
	lots := positionQty.Div(lotSize)
	return lots.Mul(swapPoints).Mul(lotSize).
		Mul(decimal.NewFromInt(int64(days))).Round(8), nil
}

// ---------------------------------------------------------------------------
// Engine
// ---------------------------------------------------------------------------

// SwapPosition is the engine's per-position input. The rollover service
// (Task 3.3.7) supplies positions held open at the cutoff; OpenedAt —
// when known — excludes positions opened at/after the cutoff same day
// (a delayed run must not charge a position that was not held through it).
type SwapPosition struct {
	PositionID   int64
	AccountID    int64
	InstrumentID int64
	Symbol       string // canonical "EUR/USD"
	Base         string
	Quote        string
	Side         string          // LONG | SHORT
	Quantity     decimal.Decimal // base-currency units (positions.quantity)
	LotSize      decimal.Decimal // units per lot (instruments.lot_size)
	// Notional is the position notional in AccrualCurrency for the markup
	// leg (qty × mark for quote-ccy accrual). Zero → Quantity is used.
	Notional   decimal.Decimal
	SwapFree   bool      // accounts.swapfree_status == 'VERIFIED' (migration 095)
	OpenedAt   time.Time // zero = unknown → treated as held at cutoff
	AccrualCcy string    // accrual currency; empty → Quote
}

// accrualCurrency resolves the currency the charge settles in.
func (p SwapPosition) accrualCurrency() string {
	if p.AccrualCcy != "" {
		return strings.ToUpper(p.AccrualCcy)
	}
	return strings.ToUpper(p.Quote)
}

// notional resolves the markup-leg notional.
func (p SwapPosition) notional() decimal.Decimal {
	if p.Notional.IsPositive() {
		return p.Notional
	}
	return p.Quantity
}

// JournalPoster is declared in fee_service.go — *LedgerService satisfies
// it (single package-level declaration shared by all GL posters here).
// SwapEngine prices and posts overnight swap accruals. The store supplies
// rates, markup policies and the accrual audit sink; cal supplies the
// holiday arithmetic; poster is the GL (Task 3.3.6); pub is the optional
// client-notification seam (NATS → WS private channel, Task 3.3.11.8 —
// nil disables notification, flagged per result).
type SwapEngine struct {
	store    SwapRateStore
	ids      InstrumentIDResolver
	cal      *HolidayCalendar
	poster   JournalPoster
	pub      Publisher // may be nil
	clock    func() time.Time
	rc       *RolloverClock
	lagDays  int
	maxStale int
}

// NewSwapEngine wires the engine. store, cal and poster are required —
// an engine that cannot resolve rates or post journals must not run.
// ids may be nil only when SwapRatesHandler is never served. rc nil →
// DefaultRolloverClock (17:00 America/New_York); clock nil → real time.
func NewSwapEngine(store SwapRateStore, ids InstrumentIDResolver, cal *HolidayCalendar,
	poster JournalPoster, pub Publisher, rc *RolloverClock, clock func() time.Time) (*SwapEngine, error) {
	if store == nil || cal == nil || poster == nil {
		return nil, fmt.Errorf("swap engine: store, calendar and poster are required")
	}
	if rc == nil {
		var err error
		if rc, err = DefaultRolloverClock(); err != nil {
			return nil, err
		}
	}
	if clock == nil {
		clock = time.Now
	}
	return &SwapEngine{
		store: store, ids: ids, cal: cal, poster: poster, pub: pub,
		clock: clock, rc: rc, lagDays: SwapValueDateLag,
		maxStale: DefaultSwapRateStalenessDays,
	}, nil
}

// WithLagDays overrides the financing value-date lag (default T+2).
func (e *SwapEngine) WithLagDays(lag int) *SwapEngine {
	if lag >= 1 && lag <= 5 {
		e.lagDays = lag
	}
	return e
}

// WithMaxRateStaleness overrides the fallback-rate staleness bound.
func (e *SwapEngine) WithMaxRateStaleness(days int) *SwapEngine {
	if days > 0 {
		e.maxStale = days
	}
	return e
}

// RolloverClock exposes the configured cutoff clock for schedulers.
func (e *SwapEngine) RolloverClock() *RolloverClock { return e.rc }

// ---------------------------------------------------------------------------
// Rollover run
// ---------------------------------------------------------------------------

// SwapPositionResult is one position's accrual outcome. Err is set on
// failure (the position is uncharged, loudly); Skipped with SkipReason
// covers legitimate non-accrual (opened after cutoff).
type SwapPositionResult struct {
	PositionID int64                    `json:"position_id"`
	AccountID  int64                    `json:"account_id"`
	Symbol     string                   `json:"symbol"`
	Days       int                      `json:"days"`
	Rate       SwapRate                 `json:"rate,omitempty"`
	RateStale  bool                     `json:"rate_stale"` // fallback rate used (older than roll date)
	Accrual    ledger.SwapAccrualRecord `json:"accrual"`
	JournalID  int64                    `json:"journal_id"`
	Replayed   bool                     `json:"replayed"` // idempotent replay — journal already committed
	Skipped    bool                     `json:"skipped"`
	SkipReason string                   `json:"skip_reason,omitempty"`
	Notified   bool                     `json:"notified"`
	NotifyErr  string                   `json:"notify_err,omitempty"` // post-commit; funds are final
	Err        string                   `json:"err,omitempty"`
}

// RolloverReport summarizes one rollover run.
type RolloverReport struct {
	RollDate  time.Time            `json:"roll_date"` // NY civil date (UTC-normalized)
	Cutoff    time.Time            `json:"cutoff"`    // UTC instant of the cutoff
	Positions int                  `json:"positions"`
	Accrued   int                  `json:"accrued"`
	Skipped   int                  `json:"skipped"`
	Failed    int                  `json:"failed"`
	Results   []SwapPositionResult `json:"results"`
}

// ProcessRollover accrues swap for every supplied position at the rollover
// that rollInstant belongs to. Per-position failures are collected (the
// rest of the book still accrues); store/policy load failures abort the
// run — a wrong markup schedule must never price the whole book.
func (e *SwapEngine) ProcessRollover(ctx context.Context, positions []SwapPosition, rollInstant time.Time) (*RolloverReport, error) {
	rollDate := e.rc.RollDate(rollInstant)
	cutoff := e.rc.CutoffOn(rollInstant)
	rep := &RolloverReport{RollDate: rollDate, Cutoff: cutoff, Positions: len(positions)}

	policies, err := e.store.ActiveSwapMarkupPolicies(ctx)
	if err != nil {
		return nil, excerrors.Wrap(CodeRolloverCalcError,
			"rollover: load markup policies", err)
	}

	for _, pos := range positions {
		res := e.accruePosition(ctx, pos, rollDate, cutoff, policies)
		rep.Results = append(rep.Results, res)
		switch {
		case res.Err != "":
			rep.Failed++
		case res.Skipped:
			rep.Skipped++
		default:
			rep.Accrued++
		}
		if ctx.Err() != nil {
			return rep, ctx.Err()
		}
	}
	return rep, nil
}

// accruePosition runs one position through the rate → days → markup →
// journal → audit → notify pipeline.
func (e *SwapEngine) accruePosition(ctx context.Context, pos SwapPosition, rollDate, cutoff time.Time, policies []ledger.SwapMarkupPolicy) (res SwapPositionResult) {
	res.PositionID, res.AccountID, res.Symbol = pos.PositionID, pos.AccountID, pos.Symbol

	fail := func(err error) SwapPositionResult {
		res.Err = err.Error()
		return res
	}

	// Position opened at/after the cutoff did not carry through it —
	// no accrual (SDD edge case).
	if !pos.OpenedAt.IsZero() && !pos.OpenedAt.Before(cutoff) {
		res.Skipped = true
		res.SkipReason = "OPENED_AFTER_CUTOFF"
		return res
	}

	accrual, rate, days, stale, err := e.priceAccrual(ctx, pos, rollDate, policies)
	if err != nil {
		return fail(err)
	}
	res.Rate, res.RateStale, res.Days = rate, stale, days
	res.Accrual = accrual.Record

	// GL posting — swap-free and zero-delta accruals carry no journal but
	// are still written to the audit trail below.
	if accrual.Journal != nil {
		j := *accrual.Journal
		j.IdempotencyKey = fmt.Sprintf("swap:%d:%s", pos.PositionID,
			rollDate.Format("20060102"))
		posted, err := e.poster.Post(ctx, j)
		if err != nil {
			return fail(excerrors.Wrap(CodeRolloverCalcError,
				fmt.Sprintf("position %d swap posting", pos.PositionID), err))
		}
		res.JournalID = posted.JournalID
		res.Replayed = posted.Replayed
		res.Accrual.JournalEntryID = posted.JournalID
	}

	if _, err := e.store.InsertSwapAccrual(ctx, res.Accrual); err != nil {
		// The journal (if any) is already committed — surface the audit-gap
		// loudly rather than lose it silently.
		return fail(excerrors.Wrap(CodeRolloverCalcError,
			fmt.Sprintf("position %d accrual record (journal %d committed)", pos.PositionID, res.JournalID), err))
	}

	if e.pub != nil {
		if err := e.notifyCharged(ctx, res, rollDate); err != nil {
			res.NotifyErr = err.Error() // post-commit; non-fatal
		} else {
			res.Notified = true
		}
	}
	return res
}

// priceAccrual is the pure pricing half of the pipeline — side/rate/days/
// markup resolution and ComputeSwapAccrual — shared with the carry-trade
// settlement (Task 3.3.15), which nets leg deltas before posting a single
// distribution journal instead of per-position journals.
func (e *SwapEngine) priceAccrual(ctx context.Context, pos SwapPosition, rollDate time.Time, policies []ledger.SwapMarkupPolicy) (ledger.SwapAccrual, SwapRate, int, bool, error) {
	side := ledger.SwapSide(strings.ToUpper(pos.Side))
	if side != ledger.SwapLong && side != ledger.SwapShort {
		return ledger.SwapAccrual{}, SwapRate{}, 0, false,
			excerrors.New(CodeRolloverCalcError,
				fmt.Sprintf("position %d side %q not in {LONG,SHORT}", pos.PositionID, pos.Side))
	}

	rate, stale, err := e.resolveRate(ctx, pos.InstrumentID, rollDate)
	if err != nil {
		return ledger.SwapAccrual{}, SwapRate{}, 0, false, err
	}

	days, err := RolloverDaysForLag(e.cal, pos.Base, pos.Quote, rollDate, e.lagDays)
	if err != nil {
		return ledger.SwapAccrual{}, SwapRate{}, 0, false, err
	}

	points := rate.LongPoints
	if side == ledger.SwapShort {
		points = rate.ShortPoints
	}

	interbank, err := InterbankSwapCharge(pos.Quantity, pos.LotSize, points, days)
	if err != nil {
		return ledger.SwapAccrual{}, SwapRate{}, 0, false, err
	}

	markupBps, _ := ledger.MarkupFor(policies, pos.InstrumentID, side)
	accrual, err := ledger.ComputeSwapAccrual(ledger.SwapAccrualInput{
		AccountID:       pos.AccountID,
		PositionID:      pos.PositionID,
		InstrumentID:    pos.InstrumentID,
		Symbol:          pos.Symbol,
		Side:            side,
		InterbankAmount: interbank,
		Notional:        pos.notional(),
		MarkupBps:       markupBps,
		AccrualCurrency: pos.accrualCurrency(),
		Days:            days,
		SwapFree:        pos.SwapFree,
		ReferenceID:     pos.PositionID,
		PostedBy:        "swap-engine",
	})
	if err != nil {
		return ledger.SwapAccrual{}, SwapRate{}, 0, false, err
	}
	return accrual, rate, days, stale, nil
}

// resolveRate returns the rate for the roll date: the exact effective row,
// else the latest prior row inside the staleness bound (stale=true). No
// rate at all, or one older than the bound, fails SWAP_RATE_STALE.
func (e *SwapEngine) resolveRate(ctx context.Context, instrumentID int64, rollDate time.Time) (SwapRate, bool, error) {
	rate, ok, err := e.store.SwapRateFor(ctx, instrumentID, rollDate)
	if err != nil {
		return SwapRate{}, false, excerrors.Wrap(CodeRolloverCalcError,
			"swap rate lookup", err)
	}
	if ok {
		return rate, false, nil
	}
	rate, ok, err = e.store.LatestSwapRate(ctx, instrumentID, rollDate)
	if err != nil {
		return SwapRate{}, false, excerrors.Wrap(CodeRolloverCalcError,
			"swap rate fallback lookup", err)
	}
	if !ok {
		return SwapRate{}, false, excerrors.New(CodeSwapRateStale,
			fmt.Sprintf("no swap rate for instrument %d on/before %s",
				instrumentID, rollDate.Format("2006-01-02")))
	}
	if DaysBetween(rate.EffectiveDate, rollDate) > e.maxStale {
		return SwapRate{}, false, excerrors.New(CodeSwapRateStale,
			fmt.Sprintf("swap rate for instrument %d stale: effective %s, roll %s (>%dd)",
				instrumentID, rate.EffectiveDate.Format("2006-01-02"),
				rollDate.Format("2006-01-02"), e.maxStale))
	}
	return rate, true, nil
}

// ---------------------------------------------------------------------------
// Client notification seam (Task 3.3.11.8 — NATS → WS private channel)
// ---------------------------------------------------------------------------

// SwapChargedSubject is the per-account notification topic.
func SwapChargedSubject(accountID int64) string {
	return fmt.Sprintf("account.swap.charged.%d", accountID)
}

// swapChargedEvent is the published payload (decimal strings, never
// float64).
type swapChargedEvent struct {
	EventType       string `json:"event_type"` // "SWAP_CHARGED"
	AccountID       int64  `json:"account_id"`
	PositionID      int64  `json:"position_id"`
	Symbol          string `json:"symbol"`
	Side            string `json:"side"`
	Days            int    `json:"days"`
	Currency        string `json:"currency"`
	InterbankAmount string `json:"interbank_amount"`
	MarkupAmount    string `json:"markup_amount"`
	ClientDelta     string `json:"client_delta"`
	JournalID       int64  `json:"journal_id"`
	RollDate        string `json:"roll_date"`
}

func (e *SwapEngine) notifyCharged(ctx context.Context, res SwapPositionResult, rollDate time.Time) error {
	ev := swapChargedEvent{
		EventType:       "SWAP_CHARGED",
		AccountID:       res.Accrual.AccountID,
		PositionID:      res.PositionID,
		Symbol:          res.Symbol,
		Side:            string(res.Accrual.Side),
		Days:            res.Days,
		Currency:        res.Accrual.Currency,
		InterbankAmount: res.Accrual.InterbankAmount.String(),
		MarkupAmount:    res.Accrual.MarkupAmount.String(),
		ClientDelta:     res.Accrual.ClientDelta.String(),
		JournalID:       res.JournalID,
		RollDate:        rollDate.Format("2006-01-02"),
	}
	payload, err := json.Marshal(ev)
	if err != nil {
		return fmt.Errorf("swap notify encode: %w", err)
	}
	return e.pub.Publish(ctx, SwapChargedSubject(res.AccountID), payload)
}

// ---------------------------------------------------------------------------
// Open-position loader (rollover daemon seam — Task 3.3.7 consumes)
// ---------------------------------------------------------------------------

// SwapPositionLoader supplies the positions to accrue.
type SwapPositionLoader interface {
	OpenPositions(ctx context.Context, asOf time.Time) ([]SwapPosition, error)
}

// PgSwapPositionLoader loads non-flat positions joined to instruments.
// accounts.swapfree_status (migration 095) and settlement_intent
// (migration 104) land later — until then SwapFree reports false and the
// daemon's intent filter supplies the exclusions.
type PgSwapPositionLoader struct {
	Pool *pgxpool.Pool
}

// NewPgSwapPositionLoader wraps a pool.
func NewPgSwapPositionLoader(pool *pgxpool.Pool) *PgSwapPositionLoader {
	return &PgSwapPositionLoader{Pool: pool}
}

// OpenPositions implements SwapPositionLoader. Notional is
// qty × mark (entry price when unmarked) in the quote currency.
func (l *PgSwapPositionLoader) OpenPositions(ctx context.Context, _ time.Time) ([]SwapPosition, error) {
	rows, err := l.Pool.Query(ctx, `
		SELECT p.id, p.account_id, p.instrument_id, i.symbol,
		       i.base_currency, i.quote_currency, p.side::text,
		       p.quantity::text, i.lot_size::text,
		       (p.quantity * COALESCE(p.mark_price, p.entry_price))::text
		FROM positions p
		JOIN instruments i ON i.id = p.instrument_id
		WHERE p.quantity <> 0
		ORDER BY p.id`)
	if err != nil {
		return nil, fmt.Errorf("open positions: %w", err)
	}
	defer rows.Close()
	var out []SwapPosition
	for rows.Next() {
		var (
			p                  SwapPosition
			qty, lot, notional string
		)
		if err := rows.Scan(&p.PositionID, &p.AccountID, &p.InstrumentID,
			&p.Symbol, &p.Base, &p.Quote, &p.Side, &qty, &lot, &notional); err != nil {
			return nil, fmt.Errorf("open positions scan: %w", err)
		}
		var err error
		if p.Quantity, err = decimal.NewFromString(qty); err != nil {
			return nil, fmt.Errorf("position qty parse %q: %w", qty, err)
		}
		if p.LotSize, err = decimal.NewFromString(lot); err != nil {
			return nil, fmt.Errorf("lot size parse %q: %w", lot, err)
		}
		if p.Notional, err = decimal.NewFromString(notional); err != nil {
			return nil, fmt.Errorf("notional parse %q: %w", notional, err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// REST handler — handler function only; Phase-05 Task 5.3.7 registers
// "GET /api/v1/instruments/{symbol}/swap-rates" on the v1 router.
// ---------------------------------------------------------------------------

// swapRateJSON is the wire view of one effective-dated rate.
type swapRateJSON struct {
	Symbol          string `json:"symbol"`
	EffectiveDate   string `json:"effective_date"`
	LongSwapPoints  string `json:"long_swap_points"`
	ShortSwapPoints string `json:"short_swap_points"`
	Source          string `json:"source"`
	Stale           bool   `json:"stale,omitempty"` // effective_date < requested date
}

func rateJSON(symbol string, r SwapRate, stale bool) swapRateJSON {
	return swapRateJSON{
		Symbol:          symbol,
		EffectiveDate:   r.EffectiveDate.Format("2006-01-02"),
		LongSwapPoints:  r.LongPoints.String(),
		ShortSwapPoints: r.ShortPoints.String(),
		Source:          r.Source,
		Stale:           stale,
	}
}

// SwapRatesHandler serves GET /api/v1/instruments/{symbol}/swap-rates.
// Query params: date=YYYY-MM-DD (exact date, 404 when absent) or
// from=YYYY-MM-DD&to=YYYY-MM-DD&limit=N for history; neither → latest rate
// on or before today.
func (e *SwapEngine) SwapRatesHandler(w http.ResponseWriter, r *http.Request) {
	symbol := NormalizeSymbol(r.PathValue("symbol"))
	if e.ids == nil {
		writeSwapError(w, excerrors.New("INTERNAL_ERROR", "instrument resolver not wired"))
		return
	}
	id, err := e.ids.InstrumentID(r.Context(), symbol)
	if err != nil {
		writeSwapError(w, err)
		return
	}

	q := r.URL.Query()
	dateStr := q.Get("date")
	fromStr, toStr := q.Get("from"), q.Get("to")

	switch {
	case dateStr != "":
		date, perr := time.Parse("2006-01-02", dateStr)
		if perr != nil {
			writeSwapError(w, excerrors.New(codeInvalidRequest, "invalid date "+dateStr))
			return
		}
		rate, ok, err := e.store.SwapRateFor(r.Context(), id, date)
		if err != nil {
			writeSwapError(w, err)
			return
		}
		if !ok {
			writeSwapError(w, excerrors.New(codeNotFound,
				fmt.Sprintf("no swap rate for %s on %s", symbol, dateStr)))
			return
		}
		writeSwapJSON(w, rateJSON(symbol, rate, false))

	case fromStr != "" || toStr != "":
		from, err1 := time.Parse("2006-01-02", fromStr)
		to, err2 := time.Parse("2006-01-02", toStr)
		if err1 != nil || err2 != nil || to.Before(from) {
			writeSwapError(w, excerrors.New(codeInvalidRequest,
				"from/to must be YYYY-MM-DD with from <= to"))
			return
		}
		limit := 0
		if ls := q.Get("limit"); ls != "" {
			if _, err := fmt.Sscanf(ls, "%d", &limit); err != nil {
				writeSwapError(w, excerrors.New(codeInvalidRequest, "invalid limit "+ls))
				return
			}
		}
		rates, err := e.store.SwapRateHistory(r.Context(), id, from, to, limit)
		if err != nil {
			writeSwapError(w, err)
			return
		}
		out := make([]swapRateJSON, 0, len(rates))
		for _, rt := range rates {
			out = append(out, rateJSON(symbol, rt, false))
		}
		writeSwapJSON(w, out)

	default:
		today := normalizeDay(e.clock())
		rate, ok, err := e.store.LatestSwapRate(r.Context(), id, today)
		if err != nil {
			writeSwapError(w, err)
			return
		}
		if !ok {
			writeSwapError(w, excerrors.New(codeNotFound,
				"no swap rates for "+symbol))
			return
		}
		writeSwapJSON(w, rateJSON(symbol, rate, rate.EffectiveDate.Before(today)))
	}
}

func writeSwapJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(v)
}

// swapHTTPStatus maps emitted codes to HTTP status — unknown codes are
// 500 (fail-closed, spec §2.7.1).
func swapHTTPStatus(code string) int {
	switch code {
	case codeInvalidRequest:
		return http.StatusBadRequest
	case codeNotFound:
		return http.StatusNotFound
	case CodeSwapRateStale, codeOracleUnavailable:
		return http.StatusServiceUnavailable
	default:
		return http.StatusInternalServerError
	}
}

// writeSwapError emits the RFC 7807 envelope for err.
func writeSwapError(w http.ResponseWriter, err error) {
	var ee *excerrors.Error
	if stderrors.As(err, &ee) {
		excerrors.NewProblem(swapHTTPStatus(ee.Code), ee.Code, ee.Code, ee.Message).WriteTo(w)
		return
	}
	excerrors.NewProblem(http.StatusInternalServerError,
		"INTERNAL_ERROR", "internal error", "").WriteTo(w)
}
