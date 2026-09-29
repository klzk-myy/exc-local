// Phase-15 Task 15.3.13 — the benchmark-fixing scheduler (spec §6.4/
// §7.1, §24 #401; migration 087 auction_calendar BENCHMARK_FIXING rows +
// migration 222 benchmark_fixings).
//
// At each benchmark trigger — WM/R London 4PM (16:00 Europe/London),
// ECB reference (14:15 Europe/Berlin), Tokyo (09:55 Asia/Tokyo) — the
// scheduler resolves the holiday-rolled fixing instant, asks the
// PriceSource seam (the Phase-19.5 price oracle) for the rate, persists
// a benchmark_fixings row (rate + execution timestamp — the spec's
// recorded evidence), publishes the FIXING control key
//
//	instrument:fixing:{symbol} = "FIXING:{benchmark}:{rate}:{unix_ns}"
//
// and queues the instrument's resting FIXING orders (whose
// orders_derivative_params.fixing_benchmark matches) for execution at
// rate ± the order's agreed spread — the execution leg rides the
// engine-side FIXING order machinery (Task 16.3.9); this package owns
// the schedule, the rate publication and the durable record.
//
// Fail-closed: an unwired/failing PriceSource or a holiday ambiguity
// records SKIPPED with skip_reason — never a fabricated rate (§2.7).
// The (instrument_id, benchmark, scheduled_at) UNIQUE key makes refires
// idempotent.
package instruments

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"

	"exchange/internal/admin"
	"exchange/internal/settlement"

	excerrors "exchange/pkg/errors"
)

// ---------------------------------------------------------------------------
// Price oracle + fixing order seams
// ---------------------------------------------------------------------------

// FixingRate is the published benchmark rate.
type FixingRate struct {
	Rate   decimal.Decimal
	Source string // oracle feed id that priced it
}

// PriceSource is the Phase-19.5 price-oracle seam. nil on the scheduler
// → every firing records SKIPPED (never a synthetic rate).
type PriceSource interface {
	// FixingRate returns the benchmark rate for the pair at the
	// scheduled instant, or an error when no feed can price it.
	FixingRate(ctx context.Context, symbol, benchmark string,
		at time.Time) (*FixingRate, error)
}

// FixingFeed publishes the engine-side fixing key — the same
// instrument:fixing:{symbol} contract documented in reference.go.
type FixingFeed interface {
	SetFixing(ctx context.Context, symbol, payload string) error
}

// FixingOrderLister returns the resting FIXING orders for an instrument
// whose derivative params name this benchmark — the queue the engine
// fills at rate±spread.
type FixingOrderLister interface {
	FixingOrderIDs(ctx context.Context, instrumentID int64,
		benchmark string) ([]int64, error)
}

// PgFixingOrders reads orders.fixing_benchmark (migration 038 — the
// canonical spec §6.4 order vocabulary WM_R_4PM/ECB_1415/TOKYO_0955)
// with the scheduler vocabulary translated at the seam. The legacy
// orders_derivative_params.fixing_benchmark (migration 039) path is
// UNIONed for rows written before 038 landed — deduped, never double-
// counted.
type PgFixingOrders struct{ pool *pgxpool.Pool }

// NewPgFixingOrders wires the lister.
func NewPgFixingOrders(pool *pgxpool.Pool) *PgFixingOrders {
	return &PgFixingOrders{pool: pool}
}

// orderBenchmarkFor maps the scheduler/calendar vocabulary onto the
// canonical order-level benchmark the orders table stores (migration
// 038 CHECK constraint). The mirror map lives in algo/fixing.go
// (SchedulerBenchmark/OrderBenchmark) — kept bidirectionally identical;
// a fourth benchmark added to either side must land on both.
func orderBenchmarkFor(schedulerBenchmark string) (string, bool) {
	switch schedulerBenchmark {
	case BenchWMLondon:
		return "WM_R_4PM", true
	case BenchECB:
		return "ECB_1415", true
	case BenchTokyo:
		return "TOKYO_0955", true
	}
	return "", false
}

// FixingOrderIDs returns open FIXING order ids for the instrument+benchmark.
func (l *PgFixingOrders) FixingOrderIDs(ctx context.Context, instrumentID int64,
	benchmark string) ([]int64, error) {
	canonical, ok := orderBenchmarkFor(benchmark)
	if !ok {
		canonical = benchmark // unknown vocabulary — still match literally
	}
	rows, err := l.pool.Query(ctx, `
		SELECT id FROM (
		    SELECT o.id FROM orders o
		     WHERE o.instrument_id=$1 AND o.order_type='FIXING'
		       AND o.fixing_benchmark=$2
		       AND o.status IN ('PENDING','RESERVED','ACTIVE','PARTIALLY_FILLED')
		    UNION
		    SELECT o.id FROM orders o
		      JOIN orders_derivative_params d ON d.order_id = o.id
		     WHERE o.instrument_id=$1 AND o.order_type='FIXING'
		       AND d.fixing_benchmark=$3
		       AND o.status IN ('PENDING','RESERVED','ACTIVE','PARTIALLY_FILLED')
		) q ORDER BY id`, instrumentID, canonical, benchmark)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []int64{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// Scheduler
// ---------------------------------------------------------------------------

// FixingDeps wires the scheduler. Pool and Store are required; Holidays
// gates the holiday roll (nil → the scheduler skips the roll but still
// fires at the calendar instant — documented degradation; the NDF
// fixing-holiday obligation binds the order-path checks, not the clock).
type FixingDeps struct {
	Pool     *pgxpool.Pool
	Store    *CalendarStore
	Holidays *settlement.HolidayCalendar
	Prices   PriceSource
	Feed     FixingFeed
	Orders   FixingOrderLister
	WS       admin.InstrumentWSPublisher
	Now      func() time.Time
	Logf     func(format string, args ...any)
}

// FixingScheduler fires benchmark fixings per the calendar.
type FixingScheduler struct {
	pool     *pgxpool.Pool
	store    *CalendarStore
	holidays *settlement.HolidayCalendar
	prices   PriceSource
	feed     FixingFeed
	orders   FixingOrderLister
	ws       admin.InstrumentWSPublisher
	now      func() time.Time
	logf     func(format string, args ...any)
}

// NewFixingScheduler wires the scheduler.
func NewFixingScheduler(d FixingDeps) (*FixingScheduler, error) {
	if d.Pool == nil {
		return nil, fmt.Errorf("fixing scheduler: pgx pool is nil")
	}
	if d.Store == nil {
		return nil, fmt.Errorf("fixing scheduler: calendar store is nil")
	}
	s := &FixingScheduler{
		pool: d.Pool, store: d.Store, holidays: d.Holidays,
		prices: d.Prices, feed: d.Feed, orders: d.Orders, ws: d.WS,
		now: d.Now, logf: d.Logf,
	}
	if s.now == nil {
		s.now = time.Now
	}
	if s.logf == nil {
		s.logf = func(string, ...any) {}
	}
	return s, nil
}

// ScheduledFor resolves the holiday-rolled fixing instant for an
// occurrence date: the nominal trigger on the date in the row's
// timezone, then rolled forward to the next mutual business day when
// the pair's settlement centers are closed (spec §7.4 item 2 — the NDF
// fixing-holiday rule; applied to all benchmarks). Returns the UTC
// instant.
func (s *FixingScheduler) ScheduledFor(ctx context.Context,
	e CalendarEntry, base, quote string, onOrAfter time.Time) (time.Time, error) {

	// Nominal instant: first occurrence on/after onOrAfter.
	nominal, err := e.NextOccurrence(onOrAfter)
	if err != nil {
		return time.Time{}, err
	}
	if s.holidays == nil {
		return nominal, nil
	}
	ccys := settlement.SettlementCurrencies(base, quote)
	ok := true
	for _, c := range ccys {
		if !s.holidays.HasCurrency(c) {
			ok = false
			break
		}
	}
	if !ok {
		// No holiday data — fire at the nominal instant; the rate seam's
		// SKIPPED record carries the ambiguity (fail closed downstream,
		// not silently shifted).
		return nominal, nil
	}
	// Roll the fixing date (the local calendar date of the nominal
	// instant) forward while the centers are closed, preserving the
	// local wall-clock trigger on the rolled date.
	loc, err := time.LoadLocation(e.Timezone)
	if err != nil {
		return time.Time{}, err
	}
	mark, err := parseHM(e.TriggerTime)
	if err != nil {
		return time.Time{}, err
	}
	for i := 0; i < 30; i++ {
		local := nominal.In(loc)
		day := time.Date(local.Year(), local.Month(), local.Day(),
			0, 0, 0, 0, time.UTC)
		if s.holidays.IsMutualBusinessDay(day, ccys...) {
			return nominal, nil
		}
		rolled := s.holidays.NextMutualBusinessDay(day, ccys...)
		nominal = time.Date(rolled.Year(), rolled.Month(), rolled.Day(),
			mark.h, mark.m, 0, 0, loc)
	}
	return time.Time{}, fmt.Errorf("fixing: no business day within 30 days")
}

// Tick fires every due BENCHMARK_FIXING row.
func (s *FixingScheduler) Tick(ctx context.Context) {
	now := s.now()
	rows, err := s.pool.Query(ctx, `
		SELECT c.id, c.instrument_id, c.symbol, c.auction_type,
		       COALESCE(c.benchmark,''), to_char(c.trigger_time,'HH24:MI'),
		       c.timezone, c.recurrence, c.enabled, c.last_fired_at,
		       i.base_currency, i.quote_currency
		  FROM auction_calendar c
		  JOIN instruments i ON i.id = c.instrument_id
		 WHERE c.enabled AND c.auction_type='BENCHMARK_FIXING'
		   AND i.status = 'ACTIVE'
		 ORDER BY c.id`)
	if err != nil {
		s.logf("fixing scheduler: scan: %v", err)
		return
	}
	type dueRow struct {
		e           CalendarEntry
		base, quote string
	}
	var due []dueRow
	for rows.Next() {
		var d dueRow
		if err := rows.Scan(&d.e.ID, &d.e.InstrumentID, &d.e.Symbol,
			&d.e.AuctionType, &d.e.Benchmark, &d.e.TriggerTime, &d.e.Timezone,
			&d.e.Recurrence, &d.e.Enabled, &d.e.LastFiredAt,
			&d.base, &d.quote); err != nil {
			rows.Close()
			s.logf("fixing scheduler: scan row: %v", err)
			return
		}
		due = append(due, d)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		s.logf("fixing scheduler: rows: %v", err)
		return
	}
	for _, d := range due {
		// Bounded catch-up: the firing window is the latest occurrence
		// at or before now — a never-fired row looks back at most 7 days
		// (a fixing older than that cannot be priced faithfully; the gap
		// is visible as absent benchmark_fixings rows on the ops board).
		anchor := now.Add(-7 * 24 * time.Hour)
		if d.e.LastFiredAt != nil {
			anchor = *d.e.LastFiredAt
		}
		var sched time.Time
		for i := 0; i < 40; i++ {
			next, err := s.ScheduledFor(ctx, d.e, d.base, d.quote, anchor)
			if err != nil {
				s.logf("fixing scheduler: schedule %s %s: %v",
					d.e.Symbol, d.e.Benchmark, err)
				sched = time.Time{}
				break
			}
			if next.After(now) {
				break
			}
			sched, anchor = next, next
		}
		if sched.IsZero() {
			continue // nothing due
		}
		if err := s.fire(ctx, d.e, sched, now); err != nil {
			s.logf("fixing scheduler: fire %s %s: %v",
				d.e.Symbol, d.e.Benchmark, err)
		}
	}
}

// Run is the tick loop (interval ≤ 0 → 10s; fixings are minute-scale
// and the durable record dedupes refires).
func (s *FixingScheduler) Run(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = 10 * time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.Tick(ctx)
		}
	}
}

// fire executes one due fixing: rate lookup → FIXING-order queue →
// benchmark_fixings record → control-key publication → last_fired_at.
// The record lands even on failure (status SKIPPED/FAILED with reason)
// so the ops board sees the miss rather than a silent gap.
func (s *FixingScheduler) fire(ctx context.Context, e CalendarEntry,
	scheduledAt, now time.Time) error {

	// FIXING order queue — orders fixing against this benchmark.
	var orderIDs []int64
	if s.orders != nil {
		ids, err := s.orders.FixingOrderIDs(ctx, e.InstrumentID, e.Benchmark)
		if err != nil {
			s.logf("fixing scheduler: order queue %s: %v", e.Symbol, err)
		} else {
			orderIDs = ids
		}
	}

	status := "RECORDED"
	var rate decimal.Decimal
	var source, skipReason string
	if s.prices == nil {
		status, skipReason = "SKIPPED", "price source unwired (Phase-19.5 oracle seam absent)"
	} else {
		fr, err := s.prices.FixingRate(ctx, e.Symbol, e.Benchmark, scheduledAt)
		switch {
		case err != nil:
			status, skipReason = "SKIPPED", "price source: "+err.Error()
		case fr == nil || !fr.Rate.IsPositive():
			status, skipReason = "SKIPPED", "price source returned no rate"
		default:
			rate, source = fr.Rate, fr.Source
		}
	}

	// Durable record — UNIQUE(instrument_id, benchmark, scheduled_at)
	// makes a refire idempotent: a conflicting row means this occurrence
	// already committed, so skip the rest.
	var rateVal any
	if rate.IsPositive() {
		rateVal = rate.String()
	}
	ordersJSON, _ := json.Marshal(orderIDs)
	var recID int64
	err := s.pool.QueryRow(ctx, `
		INSERT INTO benchmark_fixings
		    (instrument_id, symbol, benchmark, scheduled_at, fired_at,
		     rate, rate_source, status, skip_reason, order_ids)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
		ON CONFLICT (instrument_id, benchmark, scheduled_at) DO NOTHING
		RETURNING id`,
		e.InstrumentID, e.Symbol, e.Benchmark, scheduledAt, now,
		rateVal, source, status, skipReason, ordersJSON).Scan(&recID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil // already recorded — idempotent refire
	}
	if err != nil {
		return excerrors.Wrap("INTERNAL_ERROR", "fixing record", err)
	}

	// Publish the rate key + mark fired only for a real rate — a SKIPPED
	// fixing must not publish a fabricated trigger.
	if status == "RECORDED" && s.feed != nil {
		payload := fmt.Sprintf("FIXING:%s:%s:%d",
			e.Benchmark, rate.String(), scheduledAt.UnixNano())
		if err := s.feed.SetFixing(ctx, e.Symbol, payload); err != nil {
			s.logf("fixing scheduler: publish %s: %v", e.Symbol, err)
		}
		if s.ws != nil {
			s.ws.Publish(admin.InstrumentStatusChannel, admin.InstrumentStatusEvent{
				Event: "BENCHMARK_FIXING", Symbol: e.Symbol,
				Reason: e.Benchmark + "=" + rate.String() + " (" + source + ")",
				Source: "fixing-scheduler", TsMs: now.UnixMilli(),
			})
		}
	}
	if err := s.store.MarkFired(ctx, e.ID, now); err != nil {
		s.logf("fixing scheduler: mark fired %d: %v", e.ID, err)
	}
	return nil
}

// RecentFixings returns the fixing rows for a symbol, newest first — the
// ops-board/admin read surface.
func (s *FixingScheduler) RecentFixings(ctx context.Context, symbol string,
	limit int) ([]map[string]any, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id, symbol, benchmark, scheduled_at, fired_at,
		       COALESCE(rate::text,''), rate_source, status, skip_reason,
		       order_ids
		  FROM benchmark_fixings WHERE symbol=$1
		 ORDER BY scheduled_at DESC LIMIT $2`,
		strings.ToUpper(strings.TrimSpace(symbol)), limit)
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "fixings read", err)
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var (
			id                       int64
			sym, bench, rate, source string
			status, skip             string
			sched, firedAt           time.Time
			orderIDs                 json.RawMessage
		)
		if err := rows.Scan(&id, &sym, &bench, &sched, &firedAt, &rate,
			&source, &status, &skip, &orderIDs); err != nil {
			return nil, excerrors.Wrap("INTERNAL_ERROR", "scan fixing", err)
		}
		out = append(out, map[string]any{
			"id": id, "symbol": sym, "benchmark": bench,
			"scheduled_at": sched, "fired_at": firedAt,
			"rate": rate, "rate_source": source,
			"status": status, "skip_reason": skip,
			"order_ids": json.RawMessage(orderIDs),
		})
	}
	return out, rows.Err()
}
