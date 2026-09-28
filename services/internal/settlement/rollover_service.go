// rollover_service.go — Automated EOD Spot Rollover daemon (Tom-Next / T/N)
// (Phase-03 Task 3.3.7 + the rollover side of Tasks 3.3.22/3.3.23;
// spec §17.4, §17.4a, §5.45, §12.8a; §24 #122/#406/#407).
//
// The daemon fires at 17:00 ET every New York weekday — 21:00 UTC under
// EDT (summer), 22:00 UTC under EST (winter); the trigger is computed from
// the America/New_York zone rules via RolloverClock (owned by
// swap_engine.go, Task 3.3.11), never a fixed UTC hour (remediation #35
// corrected the inverted mapping).
//
// Responsibility split (remediation #35 boundary — Task 3.3.11 is the
// single owner of swap-point math and swap_rates storage):
//
//	THIS SERVICE (orchestration):
//	- ticker/schedule + graceful shutdown (Run),
//	- Redis execution lock (spec deployment table: one rollover worker),
//	- rollover_runs bookkeeping — a COMPLETED run is never re-executed,
//	  RUNNING/FAILED resumes safely,
//	- the open-position scan restricted to settlement_intent =
//	  'ROLLING_MARGIN' — PHYSICAL_DELIVERY accounts are strictly excluded
//	  from financing (Task 3.3.22; their deliverable amounts ride banking
//	  rails via balance_service.go),
//	- value-date advancement on positions (the roll itself),
//	- the Task 3.3.23 swap-free admin-fee leg (SwapFreeFeeService).
//
//	SwapEngine (Task 3.3.11, consumed via the SwapAccruer seam):
//	- effective-dated rate resolution + SWAP_RATE_STALE bound,
//	- financing day-count (Wednesday triple roll, holiday-aware 4x/5x),
//	- markup + balanced GL journal posting + accrual audit + client
//	  notification; swap-free VERIFIED accounts accrue exactly 0.0 with
//	  foregone_amount reported on the audit row (§5.21a.2).
//
// Fail-closed (spec §2.7): store/policy load failures abort the run;
// per-position failures are collected and mark the run FAILED while the
// healthy book still commits; a weekend or already-completed invocation
// is a no-op.
package settlement

import (
	"context"
	stderrors "errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	goredis "github.com/redis/go-redis/v9"
	"github.com/shopspring/decimal"

	excredis "exchange/internal/redis"
	excerrors "exchange/pkg/errors"
)

const (
	// rolloverLockKey is the execution lock mandated by the spec
	// deployment table ("execution lock in Redis").
	rolloverLockKey = "rollover:tomnext:lock"
	// rolloverLockTTL bounds a crashed worker's lock; a healthy run is far
	// shorter — the lock is a dedup gate, not a lease.
	rolloverLockTTL = 30 * time.Minute
)

// ---------------------------------------------------------------------------
// Seams
// ---------------------------------------------------------------------------

// SwapAccruer is the Task 3.3.11 engine surface the daemon consumes —
// *SwapEngine satisfies it.
type SwapAccruer interface {
	ProcessRollover(ctx context.Context, positions []SwapPosition, rollInstant time.Time) (*RolloverReport, error)
	RolloverClock() *RolloverClock
}

// RolloverPosition is one open position row extended with the fields the
// daemon needs beyond SwapPosition: value date (the roll leg it owns) and
// the swap-free/admin-fee inputs of Task 3.3.23.
type RolloverPosition struct {
	ID           int64
	AccountID    int64
	InstrumentID int64
	Symbol       string
	BaseCcy      string
	QuoteCcy     string
	Side         string // LONG | SHORT
	Quantity     decimal.Decimal
	LotSize      decimal.Decimal // instruments.lot_size
	MarkPrice    decimal.Decimal // mark, else entry price
	OpenedAt     time.Time
	ValueDate    time.Time
	HasValueDate bool
	SwapFree     bool // accounts.swapfree_status == 'VERIFIED'
}

// Lots returns the position size in lots (quantity / lot_size; the
// positions-table convention is base-currency units per migration 001's
// lot_size seed — see swap_engine.go InterbankSwapCharge).
func (p RolloverPosition) Lots() decimal.Decimal {
	if !p.LotSize.IsPositive() {
		return decimal.Zero
	}
	return p.Quantity.Div(p.LotSize)
}

// swapPosition projects the engine's input view.
func (p RolloverPosition) swapPosition() SwapPosition {
	return SwapPosition{
		PositionID:   p.ID,
		AccountID:    p.AccountID,
		InstrumentID: p.InstrumentID,
		Symbol:       p.Symbol,
		Base:         p.BaseCcy,
		Quote:        p.QuoteCcy,
		Side:         p.Side,
		Quantity:     p.Quantity,
		LotSize:      p.LotSize,
		Notional:     p.Quantity.Mul(p.MarkPrice).Round(8), // quote-ccy notional
		SwapFree:     p.SwapFree,
		OpenedAt:     p.OpenedAt,
		AccrualCcy:   p.QuoteCcy,
	}
}

// ---------------------------------------------------------------------------
// Run report
// ---------------------------------------------------------------------------

// RolloverRunReport summarizes a RunOnce execution (persisted to
// rollover_runs).
type RolloverRunReport struct {
	RollDate         time.Time // ET trading date
	RunID            int64
	Skipped          bool   // weekend / already completed / lock held
	SkipReason       string // "WEEKEND" | "ALREADY_COMPLETED" | "LOCK_HELD"
	PositionsScanned int
	PositionsRolled  int
	PositionsSkipped int
	SwapFreeZeroed   int
	FeesAssessed     int
	FeeTotalUSD      decimal.Decimal
	// ForegoneByCcy totals the swap-free foregone amounts per currency —
	// the §5.21a.2 "reported, never silently forgiven" evidence.
	ForegoneByCcy map[string]decimal.Decimal
	Errors        []RolloverError
	Engine        *RolloverReport // the engine's per-position detail
}

// RolloverError is one position-level failure inside a run.
type RolloverError struct {
	PositionID int64
	AccountID  int64
	Err        string
}

// ---------------------------------------------------------------------------
// Service
// ---------------------------------------------------------------------------

// RolloverService is the TomNextRolloverService of spec §17.4 — the
// scheduled daemon that drives the Task 3.3.11 swap engine over the
// rolling-margin book and layers the Task 3.3.23 admin-fee leg on top.
type RolloverService struct {
	pool   *pgxpool.Pool
	engine SwapAccruer
	cal    *HolidayCalendar
	fees   *SwapFreeFeeService // nil disables the Task 3.3.23 fee leg
	rdb    *excredis.Client    // nil allowed for tests — production must pass it
	now    func() time.Time
}

// NewRolloverService wires the daemon. pool, engine and cal are required
// (fail-closed); fees and rdb are optional seams (rdb nil = no execution
// lock — test path only).
func NewRolloverService(pool *pgxpool.Pool, engine SwapAccruer, cal *HolidayCalendar, fees *SwapFreeFeeService, rdb *excredis.Client) (*RolloverService, error) {
	if pool == nil {
		return nil, fmt.Errorf("rollover: nil pgx pool")
	}
	if engine == nil {
		return nil, fmt.Errorf("rollover: nil swap engine")
	}
	if cal == nil {
		return nil, fmt.Errorf("rollover: nil holiday calendar")
	}
	return &RolloverService{
		pool:   pool,
		engine: engine,
		cal:    cal,
		fees:   fees,
		rdb:    rdb,
		now:    time.Now,
	}, nil
}

// nextWeekdayCutoff returns the first cutoff strictly after now that lands
// on a weekday in the clock's zone — Saturday/Sunday cutoffs never fire
// (the FX week is 24/5; Friday's roll already carried the weekend).
func nextWeekdayCutoff(rc *RolloverClock, now time.Time) time.Time {
	next := rc.NextCutoff(now)
	for wd := next.In(rc.Location()).Weekday(); wd == time.Saturday || wd == time.Sunday; {
		next = rc.NextCutoff(next)
		wd = next.In(rc.Location()).Weekday()
	}
	return next
}

// Run is the daemon loop: fire at each weekday 17:00 ET cutoff until ctx
// is cancelled (the caller supplies utils.SignalContext). A failed run is
// logged and the loop continues to the next cutoff — never fatal-loops.
func (s *RolloverService) Run(ctx context.Context) error {
	rc := s.engine.RolloverClock()
	for {
		next := nextWeekdayCutoff(rc, s.now())
		timer := time.NewTimer(time.Until(next))
		select {
		case <-ctx.Done():
			timer.Stop()
			slog.Info("rollover: shutdown before next fire", "next_fire", next)
			return nil
		case <-timer.C:
		}
		report, err := s.RunOnce(ctx, s.now())
		switch {
		case err != nil:
			slog.Error("rollover: run failed", "roll_date", report.RollDate, "err", err)
		default:
			slog.Info("rollover: run complete",
				"roll_date", report.RollDate.Format("2006-01-02"),
				"scanned", report.PositionsScanned,
				"rolled", report.PositionsRolled,
				"skipped", report.PositionsSkipped,
				"errors", len(report.Errors),
				"fees", report.FeesAssessed)
		}
		if ctx.Err() != nil {
			return nil
		}
	}
}

// acquireRunLock takes rolloverLockKey SET NX PX; returns the release func
// (token-checked Lua delete — never evicts another holder) or (nil,false)
// when another worker holds it. rdb nil → no-op lock (test path).
func (s *RolloverService) acquireRunLock(ctx context.Context) (func(), bool, error) {
	if s.rdb == nil {
		return func() {}, true, nil
	}
	token := fmt.Sprintf("rollover-%d", s.now().UnixNano())
	ok, err := s.rdb.SetNX(ctx, rolloverLockKey, token, rolloverLockTTL).Result()
	if err != nil {
		return nil, false, fmt.Errorf("rollover: acquire run lock: %w", err)
	}
	if !ok {
		return nil, false, nil
	}
	release := func() {
		c, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = rolloverUnlockScript.Run(c, s.rdb.Client, []string{rolloverLockKey}, token).Err()
	}
	return release, true, nil
}

// Token-checked release for the run lock (same discipline as the
// compareAndDelScript in internal/redis).
var rolloverUnlockScript = goredis.NewScript(`
if redis.call('GET', KEYS[1]) == ARGV[1] then
  return redis.call('DEL', KEYS[1])
end
return 0
`)

// ---------------------------------------------------------------------------
// RunOnce — one roll for the ET trading date containing `now`.
// ---------------------------------------------------------------------------

// RunOnce executes a single rollover for `now`'s New York trading date.
// Weekday ET only — Saturday/Sunday invocations are skipped (24/5 market).
func (s *RolloverService) RunOnce(ctx context.Context, now time.Time) (RolloverRunReport, error) {
	rc := s.engine.RolloverClock()
	et := now.In(rc.Location())
	rollDate := rc.RollDate(now)
	rep := RolloverRunReport{RollDate: rollDate, ForegoneByCcy: map[string]decimal.Decimal{}}

	if wd := et.Weekday(); wd == time.Saturday || wd == time.Sunday {
		rep.Skipped, rep.SkipReason = true, "WEEKEND"
		return rep, nil
	}

	release, acquired, err := s.acquireRunLock(ctx)
	if err != nil {
		return rep, err
	}
	if !acquired {
		rep.Skipped, rep.SkipReason = true, "LOCK_HELD"
		return rep, nil
	}
	defer release()

	// Run-level dedup / resume bookkeeping.
	var runID int64
	err = s.pool.QueryRow(ctx, `
		INSERT INTO rollover_runs (roll_date, status)
		VALUES ($1, 'RUNNING')
		ON CONFLICT (roll_date) DO NOTHING
		RETURNING id`, rollDate).Scan(&runID)
	if stderrors.Is(err, pgx.ErrNoRows) {
		var status string
		if err2 := s.pool.QueryRow(ctx,
			`SELECT id, status FROM rollover_runs WHERE roll_date=$1`, rollDate).
			Scan(&runID, &status); err2 != nil {
			return rep, fmt.Errorf("rollover: read run row: %w", err2)
		}
		if status == "COMPLETED" {
			rep.Skipped, rep.SkipReason, rep.RunID = true, "ALREADY_COMPLETED", runID
			return rep, nil
		}
		// RUNNING/FAILED → resume; journal idempotency keys + the
		// forward-only value-date guard make replay safe.
	} else if err != nil {
		return rep, fmt.Errorf("rollover: open run row: %w", err)
	}
	rep.RunID = runID

	positions, err := s.loadPositions(ctx)
	if err != nil {
		s.finishRun(ctx, runID, rep, "FAILED", err.Error())
		return rep, err
	}
	rep.PositionsScanned = len(positions)

	// Hand the engine the rolling-margin book — it owns rates, day counts,
	// markup, journals, accrual audit and client notification.
	engineIn := make([]SwapPosition, 0, len(positions))
	byID := make(map[int64]RolloverPosition, len(positions))
	for _, p := range positions {
		engineIn = append(engineIn, p.swapPosition())
		byID[p.ID] = p
	}
	eng, err := s.engine.ProcessRollover(ctx, engineIn, now)
	if err != nil && eng == nil {
		s.finishRun(ctx, runID, rep, "FAILED", err.Error())
		return rep, excerrors.Wrap(CodeRolloverCalcError,
			"rollover: engine run aborted", err)
	}
	rep.Engine = eng

	for _, res := range eng.Results {
		pos, ok := byID[res.PositionID]
		if !ok {
			continue
		}
		switch {
		case res.Err != "":
			rep.Errors = append(rep.Errors, RolloverError{
				PositionID: pos.ID, AccountID: pos.AccountID, Err: res.Err})
			continue
		case res.Skipped:
			rep.PositionsSkipped++
			continue
		}

		// Rolled: advance the position's value date — forward-only, so a
		// resumed run can never double-advance.
		newVD, verr := s.rolledValueDate(pos.BaseCcy, pos.QuoteCcy, rollDate)
		if verr != nil {
			rep.Errors = append(rep.Errors, RolloverError{
				PositionID: pos.ID, AccountID: pos.AccountID, Err: verr.Error()})
			continue
		}
		if _, uerr := s.pool.Exec(ctx, `
			UPDATE positions SET value_date = $2, updated_at = now()
			WHERE id = $1 AND (value_date IS NULL OR value_date < $2)`,
			pos.ID, newVD); uerr != nil {
			rep.Errors = append(rep.Errors, RolloverError{
				PositionID: pos.ID, AccountID: pos.AccountID,
				Err: fmt.Sprintf("advance value date: %v", uerr)})
			continue
		}
		rep.PositionsRolled++

		if pos.SwapFree {
			rep.SwapFreeZeroed++
			if !res.Accrual.ForegoneAmount.IsZero() {
				ccy := res.Accrual.Currency
				rep.ForegoneByCcy[ccy] = rep.ForegoneByCcy[ccy].Add(res.Accrual.ForegoneAmount)
			}
			if s.fees != nil {
				fee, ferr := s.fees.AssessAndPost(ctx, pos, rollDate, runID)
				if ferr != nil {
					rep.Errors = append(rep.Errors, RolloverError{
						PositionID: pos.ID, AccountID: pos.AccountID, Err: ferr.Error()})
				} else if fee.IsPositive() {
					rep.FeesAssessed++
					rep.FeeTotalUSD = rep.FeeTotalUSD.Add(fee)
				}
			}
		}
	}

	status := "COMPLETED"
	errText := ""
	if len(rep.Errors) > 0 {
		status = "FAILED"
		errText = fmt.Sprintf("%d position error(s); first: %s", len(rep.Errors), rep.Errors[0].Err)
	}
	s.finishRun(ctx, runID, rep, status, errText)
	return rep, nil
}

// finishRun persists the terminal status and counters on rollover_runs.
func (s *RolloverService) finishRun(ctx context.Context, runID int64, rep RolloverRunReport, status, errText string) {
	c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := s.pool.Exec(c, `
		UPDATE rollover_runs SET
		    status = $2, completed_at = now(), positions_scanned = $3,
		    positions_rolled = $4, positions_skipped = $5,
		    fees_assessed = $6, error_text = NULLIF($7,'')
		WHERE id = $1`,
		runID, status, rep.PositionsScanned, rep.PositionsRolled,
		rep.PositionsSkipped, rep.FeesAssessed, errText)
	if err != nil {
		slog.Error("rollover: failed to persist run status", "run_id", runID, "err", err)
	}
}

// rolledValueDate returns the value date a position holds after this roll:
// under the canonical T+2 interbank convention (SwapValueDateLag) the roll
// advances (rollDate + lag mutual business days) by one further mutual
// business day — the mirror of the engine's RolloverDaysForLag span.
func (s *RolloverService) rolledValueDate(base, quote string, rollDate time.Time) (time.Time, error) {
	ccys := SettlementCurrencies(base, quote)
	if err := s.cal.requireCurrencies(ccys); err != nil {
		return time.Time{}, excerrors.Wrap(CodeRolloverCalcError,
			"rollover: value date calendar", err)
	}
	v := normalizeDay(rollDate)
	for i := 0; i < SwapValueDateLag; i++ {
		v = s.cal.NextMutualBusinessDay(v, ccys...)
	}
	return s.cal.NextMutualBusinessDay(v, ccys...), nil
}

// loadPositions selects open SPOT positions whose accounts roll on margin.
// The settlement_intent='ROLLING_MARGIN' predicate is the Task 3.3.22
// partition: PHYSICAL_DELIVERY accounts are strictly excluded — no
// financing swap point may ever touch them (§24 #406).
func (s *RolloverService) loadPositions(ctx context.Context) ([]RolloverPosition, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT p.id, p.account_id, p.instrument_id, i.symbol,
		       i.base_currency, i.quote_currency, p.side::text,
		       p.quantity::text, i.lot_size::text,
		       COALESCE(p.mark_price, p.entry_price)::text,
		       p.opened_at, p.value_date,
		       a.swapfree_status
		FROM positions p
		JOIN accounts   a ON a.id = p.account_id
		JOIN instruments i ON i.id = p.instrument_id
		WHERE p.quantity > 0
		  AND i.instrument_type = 'SPOT'
		  AND a.settlement_intent = 'ROLLING_MARGIN'
		ORDER BY p.id`)
	if err != nil {
		return nil, fmt.Errorf("rollover: load positions: %w", err)
	}
	defer rows.Close()
	var out []RolloverPosition
	for rows.Next() {
		var (
			p               RolloverPosition
			vd              *time.Time
			sf              string
			qty, lot, price string
		)
		if err := rows.Scan(&p.ID, &p.AccountID, &p.InstrumentID, &p.Symbol,
			&p.BaseCcy, &p.QuoteCcy, &p.Side, &qty, &lot, &price,
			&p.OpenedAt, &vd, &sf); err != nil {
			return nil, fmt.Errorf("rollover: scan position: %w", err)
		}
		var err error
		if p.Quantity, err = decimal.NewFromString(qty); err != nil {
			return nil, fmt.Errorf("position qty parse %q: %w", qty, err)
		}
		if p.LotSize, err = decimal.NewFromString(lot); err != nil {
			return nil, fmt.Errorf("position lot_size parse %q: %w", lot, err)
		}
		if p.MarkPrice, err = decimal.NewFromString(price); err != nil {
			return nil, fmt.Errorf("position mark parse %q: %w", price, err)
		}
		if vd != nil {
			p.ValueDate, p.HasValueDate = *vd, true
		}
		p.SwapFree = sf == "VERIFIED"
		out = append(out, p)
	}
	return out, rows.Err()
}
