// Phase-23 Task 23.3.9 — daily Tom-Next swap-rate history read model
// (spec §17.15, §24 #358; Phase-23 §AC row 20):
//
//	GET /api/v1/history/swap-rates?symbol=&from=&to=
//
// DATA SOURCE — PostgreSQL, queried directly (the task allows "the Task
// 3.3.11 accrual journal projected to ClickHouse OR queried directly";
// no JetStream→CH projection of the journal exists — see
// analytics/consumer.go — so the PG source of truth IS the projection).
// Two tables reconcile day-for-day:
//
//   - swap_rates (migration 114): the published interbank point sheet —
//     one row per (instrument_id, effective_date), long/short points,
//     feed source, ingested_at. The rollover engine priced accruals
//     from exactly these rows.
//   - swap_accrual_records (migration 088): the per-position journal —
//     side, days (rollover day-count actually applied: 1 normal, 3
//     Wednesday, 4–5 holiday-stretched), markup_bps, amounts. The
//     journal's NY-civil-date (accrued_at in America/New_York — the
//     17:00 ET cutoff posts inside date D local) is the day a row
//     reconciles against.
//
// The published row for day D carries the sheet's interbank points
// plus the markup split the journal actually applied (per-side bps) —
// the §17.15 "long/short, interbank + markup split". Points and bps
// are different units and are never summed on the wire.
//
// Tiering (same contract as tick history, task item 2): free →
// last-30-days window AND ≥15-minute publication delay measured on
// ingested_at (a daily series has no intra-day events, so the delay
// horizon binds the sheet's publication instant, not the effective
// date); premium/staff → full history, real-time. The Task-23.3.8
// guard (10s timeout, 60s closed-interval cache) applies in the
// handler.
//
// Fail-closed (§2.7): a store outage surfaces SERVICE_DEGRADED, never
// an empty page that looks like "no swaps accrued".
package marketdata

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"exchange/pkg/decimal"
)

// SwapRolloverZone is the rollover civil-time zone — the Task 3.3.11
// engine's canonical 17:00 America/New_York cutoff (swap_engine.go
// DefaultRolloverClock). The journal day a record reconciles to is
// accrued_at rendered in this zone.
const SwapRolloverZone = "America/New_York"

// SwapRateRow is one row of the published interbank sheet
// (swap_rates + instruments join).
type SwapRateRow struct {
	InstrumentID  int64
	Symbol        string
	EffectiveDate time.Time // UTC midnight — the NY roll civil date
	LongPoints    decimal.Decimal
	ShortPoints   decimal.Decimal
	Source        string    // REFINITIV|BLOOMBERG|FILE|HTTP|MANUAL
	IngestedAt    time.Time // publication instant — the delay-tier horizon
}

// SwapJournalRow is the journal slice the day reconciles to —
// swap_accrual_records (instrument, NY-date) collapsed per side.
// Rows is the raw journal count inside the group so AccrualCount can
// sum true rows rather than side groups.
type SwapJournalRow struct {
	Side      string          // "LONG" | "SHORT"
	Days      int             // rollover day-count actually applied
	MarkupBps decimal.Decimal // admin markup actually charged
	Rows      int64           // raw journal rows in this side group
}

// SwapRateDay is the API read model: the sheet row plus the journal's
// applied parameters. Reconciliation contract (§24 #358): for a day
// with journal rows, DaysApplied/LongMarkupBps/ShortMarkupBps equal the
// journal's values EXACTLY — the projection copies, never recomputes.
type SwapRateDay struct {
	InstrumentID   int64
	Symbol         string
	EffectiveDate  time.Time
	LongPoints     decimal.Decimal // interbank points/day, LONG side
	ShortPoints    decimal.Decimal // interbank points/day, SHORT side
	LongMarkupBps  decimal.Decimal // markup bps applied to LONG accruals
	ShortMarkupBps decimal.Decimal // markup bps applied to SHORT accruals
	DaysApplied    int             // journal rollover day-count; 0 = no accruals that day
	Triple         bool            // 3-day financing day (Wednesday roll convention)
	AccrualCount   int64           // journal rows reconciled into this day
	Source         string
	IngestedAt     time.Time
}

// ProjectSwapRateDay folds one day's journal rows onto its published
// rate row — THE reconciliation point the task's DoD names ("history
// matches accrued journals day-for-day including triple-Wednesdays").
//
// Per-side markup and the day-count are uniform across a day's
// accruals by construction (the engine resolves one markup and one
// day-count per roll); MAX is used purely for determinism if a partial
// retry ever split a roll. Triple is a CALENDAR fact: days==3 when the
// journal proves the roll, else the Wednesday T+2 convention flag
// (§17.4 item 4) marks the sheet even on a no-accrual day.
func ProjectSwapRateDay(rate SwapRateRow, journal []SwapJournalRow) SwapRateDay {
	d := SwapRateDay{
		InstrumentID:  rate.InstrumentID,
		Symbol:        rate.Symbol,
		EffectiveDate: rate.EffectiveDate,
		LongPoints:    rate.LongPoints,
		ShortPoints:   rate.ShortPoints,
		Source:        rate.Source,
		IngestedAt:    rate.IngestedAt,
	}
	for _, j := range journal {
		d.AccrualCount += j.Rows
		if j.Days > d.DaysApplied {
			d.DaysApplied = j.Days
		}
		switch strings.ToUpper(j.Side) {
		case "LONG":
			if j.MarkupBps.GreaterThan(d.LongMarkupBps) {
				d.LongMarkupBps = j.MarkupBps
			}
		case "SHORT":
			if j.MarkupBps.GreaterThan(d.ShortMarkupBps) {
				d.ShortMarkupBps = j.MarkupBps
			}
		}
	}
	switch {
	case d.DaysApplied == 3:
		d.Triple = true
	case d.AccrualCount == 0 && rate.EffectiveDate.UTC().Weekday() == time.Wednesday:
		// No accruals landed that day (empty book) — the Wednesday
		// roll convention still applies to the sheet (§17.4 item 4).
		d.Triple = true
	}
	return d
}

// SwapRateHistoryCursor is the (effective_date, instrument_id) keyset
// position for the newest-first stream.
type SwapRateHistoryCursor struct {
	Date         time.Time // effective_date (UTC midnight)
	InstrumentID int64
}

// SwapRateHistoryQuery is one bounded read of the daily history.
// From/To bound effective_date [from, to); PublishedBefore bounds
// ingested_at — the free tier's delay horizon. Symbol "" reads all
// instruments. A zero bound is unbounded.
type SwapRateHistoryQuery struct {
	Symbol          string
	From            time.Time // inclusive lower bound on effective_date
	To              time.Time // exclusive upper bound on effective_date
	PublishedBefore time.Time // upper bound on ingested_at (zero = none)
	After           *SwapRateHistoryCursor
	Limit           int // 0 → store default (100)
}

// SwapRateHistorySource is the history-read seam the api handler
// consumes — *PgSwapRateHistory satisfies it; tests inject fakes (the
// DoD reconciliation fixture).
type SwapRateHistorySource interface {
	Query(ctx context.Context, q SwapRateHistoryQuery) ([]SwapRateDay, error)
}

// ---------------------------------------------------------------------------
// PgSwapRateHistory — production source over pgx (PostgreSQL 16)
// ---------------------------------------------------------------------------

// PgSwapRateHistory reads swap_rates joined to instruments plus the
// journal aggregate from swap_accrual_records. Numerics cross the wire
// as text (::text read) to avoid the pgtype-decimal shim — the same
// convention settlement.PgSwapRateStore uses.
type PgSwapRateHistory struct {
	Pool *pgxpool.Pool
	// Zone is the rollover civil zone the journal day reconciles on;
	// empty → SwapRolloverZone (America/New_York).
	Zone string
}

// NewPgSwapRateHistory binds the pool. pool nil is tolerated only by
// tests that never call Query.
func NewPgSwapRateHistory(pool *pgxpool.Pool) *PgSwapRateHistory {
	return &PgSwapRateHistory{Pool: pool}
}

func (s *PgSwapRateHistory) zone() string {
	if s.Zone != "" {
		return s.Zone
	}
	return SwapRolloverZone
}

// Query reads one keyset page of rate rows, then folds the journal
// aggregate for exactly the (instrument_id, effective_date) pairs on
// the page — the accrual join must not widen the page (a day with N
// accruals would otherwise consume N LIMIT slots).
func (s *PgSwapRateHistory) Query(ctx context.Context, q SwapRateHistoryQuery) ([]SwapRateDay, error) {
	limit := q.Limit
	if limit <= 0 {
		limit = 100
	}
	var b strings.Builder
	b.WriteString(`SELECT sr.instrument_id, i.symbol, sr.effective_date,
	       sr.long_swap_points::text, sr.short_swap_points::text,
	       sr.source, sr.ingested_at
	FROM swap_rates sr JOIN instruments i ON i.id = sr.instrument_id
	WHERE true`)
	args := []any{}
	arg := func(v any) string {
		args = append(args, v)
		return fmt.Sprintf("$%d", len(args))
	}
	if q.Symbol != "" {
		b.WriteString(" AND i.symbol = " + arg(q.Symbol))
	}
	if !q.From.IsZero() {
		b.WriteString(" AND sr.effective_date >= " + arg(q.From.UTC()))
	}
	if !q.To.IsZero() {
		b.WriteString(" AND sr.effective_date < " + arg(q.To.UTC()))
	}
	if !q.PublishedBefore.IsZero() {
		b.WriteString(" AND sr.ingested_at <= " + arg(q.PublishedBefore.UTC()))
	}
	if q.After != nil {
		b.WriteString(" AND (sr.effective_date, sr.instrument_id) < (" +
			arg(q.After.Date.UTC()) + ", " + arg(q.After.InstrumentID) + ")")
	}
	b.WriteString(" ORDER BY sr.effective_date DESC, sr.instrument_id DESC LIMIT " +
		arg(int64(limit)))

	rows, err := s.Pool.Query(ctx, b.String(), args...)
	if err != nil {
		return nil, fmt.Errorf("swap history rates %q: %w", q.Symbol, err)
	}
	defer rows.Close()

	var rates []SwapRateRow
	for rows.Next() {
		var (
			r             SwapRateRow
			longS, shortS string
		)
		if err := rows.Scan(&r.InstrumentID, &r.Symbol, &r.EffectiveDate,
			&longS, &shortS, &r.Source, &r.IngestedAt); err != nil {
			return nil, fmt.Errorf("swap history rate scan: %w", err)
		}
		if r.LongPoints, err = decimal.NewFromString(longS); err != nil {
			return nil, fmt.Errorf("swap history long %q: %w", longS, err)
		}
		if r.ShortPoints, err = decimal.NewFromString(shortS); err != nil {
			return nil, fmt.Errorf("swap history short %q: %w", shortS, err)
		}
		rates = append(rates, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("swap history rates: %w", err)
	}
	if len(rates) == 0 {
		return nil, nil
	}

	journal, err := s.journalFor(ctx, rates)
	if err != nil {
		return nil, err
	}
	out := make([]SwapRateDay, 0, len(rates))
	for _, r := range rates {
		key := swapDayKey{r.InstrumentID, r.EffectiveDate}
		out = append(out, ProjectSwapRateDay(r, journal[key]))
	}
	return out, nil
}

// swapDayKey keys the journal fold by (instrument, NY-civil date).
type swapDayKey struct {
	InstrumentID int64
	Date         time.Time
}

// journalFor loads the per-side journal aggregate for the page's exact
// (instrument, date) pairs — a bounded second read so multi-row
// accrual days never distort the rates page.
func (s *PgSwapRateHistory) journalFor(ctx context.Context, rates []SwapRateRow) (map[swapDayKey][]SwapJournalRow, error) {
	ids := make([]int64, 0, len(rates))
	minD, maxD := rates[0].EffectiveDate, rates[0].EffectiveDate
	seen := map[int64]bool{}
	for _, r := range rates {
		if !seen[r.InstrumentID] {
			seen[r.InstrumentID] = true
			ids = append(ids, r.InstrumentID)
		}
		if r.EffectiveDate.Before(minD) {
			minD = r.EffectiveDate
		}
		if r.EffectiveDate.After(maxD) {
			maxD = r.EffectiveDate
		}
	}
	// The IN-list is crossed with the page's date span; pairs that don't
	// belong to a page row are dropped by the key lookup in Query —
	// correct and simple, and the span on a page is ≤ `limit` days.
	rows, err := s.Pool.Query(ctx, `
		SELECT instrument_id,
		       (accrued_at AT TIME ZONE $1)::date AS roll_date,
		       side::text, MAX(days)::int, MAX(markup_bps)::text, COUNT(*)
		FROM swap_accrual_records
		WHERE instrument_id = ANY($2::bigint[])
		  AND (accrued_at AT TIME ZONE $1)::date BETWEEN $3::date AND $4::date
		GROUP BY instrument_id, roll_date, side`,
		s.zone(), ids, minD, maxD)
	if err != nil {
		return nil, fmt.Errorf("swap history journal: %w", err)
	}
	defer rows.Close()

	out := map[swapDayKey][]SwapJournalRow{}
	for rows.Next() {
		var (
			instID int64
			day    time.Time
			row    SwapJournalRow
			markup string
		)
		if err := rows.Scan(&instID, &day, &row.Side, &row.Days, &markup,
			&row.Rows); err != nil {
			return nil, fmt.Errorf("swap history journal scan: %w", err)
		}
		if row.MarkupBps, err = decimal.NewFromString(markup); err != nil {
			return nil, fmt.Errorf("swap history markup %q: %w", markup, err)
		}
		k := swapDayKey{instID, day.UTC()}
		out[k] = append(out[k], row)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// CSV renderer (Accept: text/csv — same negotiation as tick history)
// ---------------------------------------------------------------------------

// WriteSwapRateDaysCSV streams one RFC-4180-style row per effective
// date: interbank points plus the journal's applied markup/day-count —
// the reconciliation surface §24 #358 names.
func WriteSwapRateDaysCSV(w io.Writer, days []SwapRateDay) error {
	if _, err := io.WriteString(w,
		"symbol,instrument_id,effective_date,long_points,short_points,"+
			"long_markup_bps,short_markup_bps,days_applied,triple,"+
			"accrual_count,source,ingested_at\n"); err != nil {
		return err
	}
	for _, d := range days {
		if _, err := fmt.Fprintf(w, "%s,%d,%s,%s,%s,%s,%s,%d,%t,%d,%s,%s\n",
			d.Symbol, d.InstrumentID,
			d.EffectiveDate.UTC().Format("2006-01-02"),
			d.LongPoints.StringFixed(8), d.ShortPoints.StringFixed(8),
			d.LongMarkupBps.String(), d.ShortMarkupBps.String(),
			d.DaysApplied, d.Triple, d.AccrualCount, d.Source,
			d.IngestedAt.UTC().Format("2006-01-02T15:04:05.000Z")); err != nil {
			return err
		}
	}
	return nil
}
