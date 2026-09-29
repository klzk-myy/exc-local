// Persistence for strategies / strategy_templates / strategy_runs
// (migration 077).
//
// Invariants:
//   - run claiming is slot-idempotent: UNIQUE(strategy_id, scheduled_for)
//     plus the one-open-run partial index make duplicate scheduler ticks
//     and retries safe;
//   - schedule advancement happens in the same transaction as the run
//     claim, so a claimed slot can never double-fire;
//   - admin approval mutations write their admin_audit_log +
//     audit_hash_chain rows inside the same transaction (admin.Log).
package strategies

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"exchange/internal/admin"
	"exchange/pkg/decimal"
)

// Store persists strategy state on the OLTP pool.
type Store struct {
	pool *pgxpool.Pool
}

func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

const stratCols = `strategy_id, account_id, kind::text, label, from_currency,
	to_currency, amount::text, schedule, targets, drift_band_pct::text,
	template_id, status::text, next_run_at, last_run_at, realized_pnl::text,
	total_fees::text, total_spread_cost::text, total_notional::text,
	high_water_pnl::text, max_drawdown::text, run_count, created_at, updated_at`

func scanStrategy(row pgx.Row) (*Strategy, error) {
	var s Strategy
	var amount, band *string
	var targets []byte
	var templateID *int64
	var nextRun, lastRun *time.Time
	var label, fromCCY, toCCY, schedule *string
	if err := row.Scan(&s.StrategyID, &s.AccountID, &s.Kind, &label,
		&fromCCY, &toCCY, &amount, &schedule, &targets,
		&band, &templateID, &s.Status, &nextRun, &lastRun,
		strDec(&s.RealizedPnL), strDec(&s.TotalFees), strDec(&s.TotalSpreadCost),
		strDec(&s.TotalNotional), strDec(&s.HighWaterPnL), strDec(&s.MaxDrawdown),
		&s.RunCount, &s.CreatedAt, &s.UpdatedAt); err != nil {
		return nil, err
	}
	if label != nil {
		s.Label = *label
	}
	if fromCCY != nil {
		s.FromCurrency = *fromCCY
	}
	if toCCY != nil {
		s.ToCurrency = *toCCY
	}
	if schedule != nil {
		s.Schedule = *schedule
	}
	if amount != nil {
		d := decimal.RequireFromString(*amount)
		s.Amount = &d
	}
	if band != nil {
		d := decimal.RequireFromString(*band)
		s.DriftBandPct = &d
	}
	if targets != nil {
		if err := json.Unmarshal(targets, &s.Targets); err != nil {
			return nil, fmt.Errorf("targets decode: %w", err)
		}
	}
	s.TemplateID = templateID
	s.NextRunAt = nextRun
	s.LastRunAt = lastRun
	return &s, nil
}

// strDec adapts *decimal.Decimal to a string scan target.
type strDecT struct{ d *decimal.Decimal }

func (s strDecT) Scan(src any) error {
	switch v := src.(type) {
	case string:
		*s.d = decimal.RequireFromString(v)
	case nil:
		*s.d = decimal.Zero
	default:
		return fmt.Errorf("dec scan from %T", src)
	}
	return nil
}

func strDec(d *decimal.Decimal) strDecT { return strDecT{d: d} }

const runCols = `run_id, strategy_id, account_id, scheduled_for, kind::text,
	status::text, skip_reason, order_ids, legs, notional::text, currency,
	expected_value::text, executed_value::text, fees::text,
	spread_cost::text, realized_pnl::text, drawdown::text, started_at,
	completed_at, created_at, updated_at`

func scanRun(row pgx.Row) (*Run, error) {
	var r Run
	var skip *string
	var orderIDs []int64
	var legs []byte
	var expected, executed *string
	var started, completed *time.Time
	if err := row.Scan(&r.RunID, &r.StrategyID, &r.AccountID, &r.ScheduledFor,
		&r.Kind, &r.Status, &skip, &orderIDs, &legs, strDec(&r.Notional),
		&r.Currency, &expected, &executed, strDec(&r.Fees),
		strDec(&r.SpreadCost), strDec(&r.RealizedPnL), strDec(&r.Drawdown),
		&started, &completed, &r.CreatedAt, &r.UpdatedAt); err != nil {
		return nil, err
	}
	if skip != nil {
		r.SkipReason = *skip
	}
	r.OrderIDs = orderIDs
	if r.OrderIDs == nil {
		r.OrderIDs = []int64{}
	}
	if legs != nil {
		if err := json.Unmarshal(legs, &r.Legs); err != nil {
			return nil, fmt.Errorf("legs decode: %w", err)
		}
	}
	if r.Legs == nil {
		r.Legs = []RunLeg{}
	}
	if expected != nil {
		d := decimal.RequireFromString(*expected)
		r.ExpectedValue = &d
	}
	if executed != nil {
		d := decimal.RequireFromString(*executed)
		r.ExecutedValue = &d
	}
	r.StartedAt = started
	r.CompletedAt = completed
	return &r, nil
}

// ---- strategies ---------------------------------------------------------

// Create inserts the strategy row (config already validated).
func (s *Store) Create(ctx context.Context, st *Strategy) error {
	var targets any
	if st.Targets != nil {
		b, err := json.Marshal(st.Targets)
		if err != nil {
			return err
		}
		targets = b
	}
	return s.pool.QueryRow(ctx, `
		INSERT INTO strategies (account_id, kind, label, from_currency,
		    to_currency, amount, schedule, targets, drift_band_pct,
		    template_id, status, next_run_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)
		RETURNING strategy_id, created_at, updated_at`,
		st.AccountID, st.Kind, st.Label, nilStr(st.FromCurrency),
		nilStr(st.ToCurrency), decPtr(st.Amount), nilStr(st.Schedule),
		targets, decPtr(st.DriftBandPct), st.TemplateID,
		st.Status, st.NextRunAt).
		Scan(&st.StrategyID, &st.CreatedAt, &st.UpdatedAt)
}

// Get returns the account-scoped strategy (nil when absent).
func (s *Store) Get(ctx context.Context, accountID, strategyID int64) (*Strategy, error) {
	st, err := scanStrategy(s.pool.QueryRow(ctx,
		`SELECT `+stratCols+` FROM strategies
		 WHERE strategy_id=$1 AND account_id=$2`, strategyID, accountID))
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	return st, err
}

// GetByID resolves a strategy without account scoping (scheduler path).
func (s *Store) GetByID(ctx context.Context, strategyID int64) (*Strategy, error) {
	st, err := scanStrategy(s.pool.QueryRow(ctx,
		`SELECT `+stratCols+` FROM strategies WHERE strategy_id=$1`, strategyID))
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	return st, err
}

func (s *Store) List(ctx context.Context, accountID int64) ([]Strategy, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+stratCols+` FROM strategies WHERE account_id=$1
		 ORDER BY created_at DESC, strategy_id DESC`, accountID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Strategy
	for rows.Next() {
		st, err := scanStrategy(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *st)
	}
	return out, rows.Err()
}

// SetStatus flips lifecycle (PAUSED↔ACTIVE→CANCELLED); returns the
// resulting row. CANCELLED is terminal — the CAS refuses re-entry.
func (s *Store) SetStatus(ctx context.Context, accountID, strategyID int64,
	to string) (*Strategy, error) {

	var from any
	switch to {
	case StatusPaused:
		from = StatusActive
	case StatusActive:
		from = StatusPaused
	case StatusCancelled:
		from = nil // any non-terminal
	}
	st, err := scanStrategy(s.pool.QueryRow(ctx, `
		UPDATE strategies SET status=$3::strategy_status_enum, updated_at=now()
		WHERE strategy_id=$1 AND account_id=$2
		  AND status <> 'CANCELLED'
		  AND ($4::strategy_status_enum IS NULL OR status = $4::strategy_status_enum)
		RETURNING `+stratCols, strategyID, accountID, to, from))
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	return st, err
}

// DueRecurring lists ACTIVE recurring strategies whose next_run_at is due.
func (s *Store) DueRecurring(ctx context.Context, now time.Time) ([]Strategy, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+stratCols+` FROM strategies
		 WHERE status='ACTIVE' AND kind='RECURRING_CONVERSION'
		   AND next_run_at <= $1 ORDER BY next_run_at`, now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Strategy
	for rows.Next() {
		st, err := scanStrategy(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *st)
	}
	return out, rows.Err()
}

// ActiveRebalances lists ACTIVE rebalance strategies for drift sweeps.
func (s *Store) ActiveRebalances(ctx context.Context) ([]Strategy, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+stratCols+` FROM strategies
		 WHERE status='ACTIVE' AND kind='REBALANCE' ORDER BY strategy_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Strategy
	for rows.Next() {
		st, err := scanStrategy(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *st)
	}
	return out, rows.Err()
}

// ClaimRun inserts the PENDING run for a slot and advances the strategy
// clock in one transaction. Returns the run id + claimed flag; a false
// flag means the slot was already claimed (idempotent dedup) or an open
// run still covers the strategy.
func (s *Store) ClaimRun(ctx context.Context, st *Strategy, slot time.Time) (int64, bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var status string
	var nextRun *time.Time
	if err := tx.QueryRow(ctx, `
		SELECT status::text, next_run_at FROM strategies
		WHERE strategy_id=$1 FOR UPDATE`, st.StrategyID).
		Scan(&status, &nextRun); err != nil {
		return 0, false, err
	}
	if status != StatusActive || nextRun == nil || nextRun.After(slot) {
		return 0, false, nil // not due / paused / cancelled meanwhile
	}
	var runID int64
	err = tx.QueryRow(ctx, `
		INSERT INTO strategy_runs (strategy_id, account_id, scheduled_for,
		    kind, status, started_at)
		VALUES ($1,$2,$3,$4,'PENDING',now())
		ON CONFLICT (strategy_id, scheduled_for) DO NOTHING
		RETURNING run_id`, st.StrategyID, st.AccountID, slot, st.Kind).
		Scan(&runID)
	if err == pgx.ErrNoRows {
		return 0, false, nil // slot already claimed
	}
	if err != nil {
		return 0, false, fmt.Errorf("run claim: %w", err)
	}
	// Advance the schedule inside the claim tx: the next slot is owed
	// regardless of this run's outcome (a skipped run never re-fires).
	if _, err := tx.Exec(ctx, `
		UPDATE strategies SET next_run_at=$2, last_run_at=$3, updated_at=now()
		WHERE strategy_id=$1`,
		st.StrategyID, NextSlot(st.Schedule, slot), slot); err != nil {
		return 0, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, false, err
	}
	return runID, true, nil
}

// ClaimRebalanceRun opens one PENDING rebalance run; the one-open-run
// partial unique index suppresses overlaps (ON CONFLICT rides it).
func (s *Store) ClaimRebalanceRun(ctx context.Context, st *Strategy,
	at time.Time) (int64, bool, error) {

	var runID int64
	err := s.pool.QueryRow(ctx, `
		INSERT INTO strategy_runs (strategy_id, account_id, scheduled_for,
		    kind, status, started_at)
		VALUES ($1,$2,$3,'REBALANCE','PENDING',now())
		ON CONFLICT (strategy_id)
		WHERE status IN ('PENDING','SUBMITTED')
		DO NOTHING
		RETURNING run_id`, st.StrategyID, st.AccountID, at).Scan(&runID)
	if err == pgx.ErrNoRows {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("rebalance claim: %w", err)
	}
	return runID, true, nil
}

// HasOpenRun reports whether a non-terminal run covers the strategy.
func (s *Store) HasOpenRun(ctx context.Context, strategyID int64) (bool, error) {
	var n int
	err := s.pool.QueryRow(ctx, `
		SELECT count(*) FROM strategy_runs
		WHERE strategy_id=$1 AND status IN ('PENDING','SUBMITTED')`,
		strategyID).Scan(&n)
	return n > 0, err
}

// FinishRun stamps a terminal run outcome.
func (s *Store) FinishRun(ctx context.Context, runID int64, status, skipReason string,
	legs []RunLeg, orderIDs []int64) error {

	if orderIDs == nil {
		orderIDs = []int64{} // BIGINT[] encodes '[]', never NULL
	}
	if legs == nil {
		legs = []RunLeg{}
	}
	lb, err := json.Marshal(legs)
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx, `
		UPDATE strategy_runs SET status=$2::strategy_run_status_enum,
		    skip_reason=$3, legs=$4, order_ids=$5, completed_at=now(),
		    updated_at=now()
		WHERE run_id=$1 AND status IN ('PENDING','SUBMITTED')`,
		runID, status, nilStr(skipReason), lb, orderIDs)
	return err
}

// MarkRunSubmitted records the dispatched order ids + expected value and
// flips PENDING → SUBMITTED.
func (s *Store) MarkRunSubmitted(ctx context.Context, runID int64,
	orderIDs []int64, legs []RunLeg, notional decimal.Decimal, ccy string,
	expected *decimal.Decimal) error {

	if orderIDs == nil {
		orderIDs = []int64{}
	}
	if legs == nil {
		legs = []RunLeg{}
	}
	lb, err := json.Marshal(legs)
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx, `
		UPDATE strategy_runs SET status='SUBMITTED', order_ids=$2, legs=$3,
		    notional=$4::numeric, currency=$5, expected_value=$6::numeric,
		    updated_at=now()
		WHERE run_id=$1 AND status='PENDING'`,
		runID, orderIDs, lb, notional.String(), ccy, decPtr(expected))
	return err
}

// openRunFor returns the strategy's single non-terminal run, if any.
func (s *Store) openRunFor(ctx context.Context, strategyID int64) (*Run, error) {
	r, err := scanRun(s.pool.QueryRow(ctx,
		`SELECT `+runCols+` FROM strategy_runs
		 WHERE strategy_id=$1 AND status IN ('PENDING','SUBMITTED')
		 ORDER BY run_id DESC LIMIT 1`, strategyID))
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	return r, err
}

// FeeForOrder sums the account-facing fee for an order from the trades
// table (buyer_fee for BUY legs, seller_fee for SELL legs).
func (s *Store) FeeForOrder(ctx context.Context, orderID int64,
	side string) (decimal.Decimal, error) {

	col := "seller_fee"
	if side == "BUY" {
		col = "buyer_fee"
	}
	var txt *string
	err := s.pool.QueryRow(ctx, fmt.Sprintf(
		`SELECT COALESCE(SUM(%s),0)::text FROM trades WHERE %s = $1`,
		col, map[string]string{"SELL": "sell_order_id", "BUY": "buy_order_id"}[side]),
		orderID).Scan(&txt)
	if err != nil {
		return decimal.Zero, err
	}
	if txt == nil {
		return decimal.Zero, nil
	}
	return decimal.RequireFromString(*txt), nil
}

// OpenRuns lists non-terminal runs for the reconcile pass.
func (s *Store) OpenRuns(ctx context.Context) ([]Run, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+runCols+` FROM strategy_runs
		 WHERE status IN ('PENDING','SUBMITTED') ORDER BY run_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Run
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *r)
	}
	return out, rows.Err()
}

// CompleteRun folds a terminal run into the strategy rollup (one tx):
// run outcome + cumulative PnL/fees/notional + high-water/drawdown.
func (s *Store) CompleteRun(ctx context.Context, r *Run, st *Strategy) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if r.Legs == nil {
		r.Legs = []RunLeg{}
	}
	if r.OrderIDs == nil {
		r.OrderIDs = []int64{}
	}
	lb, err := json.Marshal(r.Legs)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE strategy_runs SET status=$2::strategy_run_status_enum,
		    skip_reason=$3, legs=$4, order_ids=$5, executed_value=$6::numeric,
		    fees=$7::numeric, spread_cost=$8::numeric, realized_pnl=$9::numeric,
		    completed_at=now(), updated_at=now()
		WHERE run_id=$1`,
		r.RunID, r.Status, nilStr(r.SkipReason), lb, r.OrderIDs,
		decPtr(r.ExecutedValue), r.Fees.String(), r.SpreadCost.String(),
		r.RealizedPnL.String()); err != nil {
		return err
	}
	cum := st.RealizedPnL.Add(r.RealizedPnL)
	hwm := st.HighWaterPnL
	if cum.GreaterThan(hwm) {
		hwm = cum
	}
	dd := hwm.Sub(cum)
	if dd.LessThan(st.MaxDrawdown) {
		dd = st.MaxDrawdown
	}
	if _, err := tx.Exec(ctx, `
		UPDATE strategies SET realized_pnl=realized_pnl+$2::numeric,
		    total_fees=total_fees+$3::numeric,
		    total_spread_cost=total_spread_cost+$4::numeric,
		    total_notional=total_notional+$5::numeric,
		    high_water_pnl=$6::numeric, max_drawdown=$7::numeric,
		    run_count=run_count+1, updated_at=now()
		WHERE strategy_id=$1`,
		st.StrategyID, r.RealizedPnL.String(), r.Fees.String(),
		r.SpreadCost.String(), r.Notional.String(),
		hwm.String(), dd.String()); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx,
		`UPDATE strategy_runs SET drawdown=$2::numeric WHERE run_id=$1`,
		r.RunID, dd.String()); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// Runs lists the strategy's recent runs (detail endpoint).
func (s *Store) Runs(ctx context.Context, strategyID int64, limit int) ([]Run, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.pool.Query(ctx,
		`SELECT `+runCols+` FROM strategy_runs WHERE strategy_id=$1
		 ORDER BY scheduled_for DESC LIMIT $2`, strategyID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Run
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *r)
	}
	return out, rows.Err()
}

// ---- templates -----------------------------------------------------------

const tmplCols = `template_id, name, description, kind::text, config,
	status::text, publisher_account_id, approved_by, approved_at,
	rejected_at, reject_reason, created_at, updated_at`

func scanTemplate(row pgx.Row) (*Template, error) {
	var t Template
	var approvedBy *int64
	var approvedAt, rejectedAt *time.Time
	var reason *string
	if err := row.Scan(&t.TemplateID, &t.Name, &t.Description, &t.Kind,
		&t.Config, &t.Status, &t.PublisherAccountID, &approvedBy,
		&approvedAt, &rejectedAt, &reason, &t.CreatedAt, &t.UpdatedAt); err != nil {
		return nil, err
	}
	t.ApprovedBy = approvedBy
	t.ApprovedAt = approvedAt
	t.RejectedAt = rejectedAt
	if reason != nil {
		t.RejectReason = *reason
	}
	return &t, nil
}

func (s *Store) PublishTemplate(ctx context.Context, t *Template) error {
	return s.pool.QueryRow(ctx, `
		INSERT INTO strategy_templates (name, description, kind, config,
		    status, publisher_account_id)
		VALUES ($1,$2,$3,$4,$5,$6)
		RETURNING template_id, created_at, updated_at`,
		t.Name, t.Description, t.Kind, t.Config, t.Status,
		t.PublisherAccountID).Scan(&t.TemplateID, &t.CreatedAt, &t.UpdatedAt)
}

// Template loads one row regardless of status.
func (s *Store) Template(ctx context.Context, templateID int64) (*Template, error) {
	t, err := scanTemplate(s.pool.QueryRow(ctx,
		`SELECT `+tmplCols+` FROM strategy_templates WHERE template_id=$1`,
		templateID))
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	return t, err
}

// Templates lists rows; status filter "" → all (admin review), else the
// marketplace serves APPROVED only.
func (s *Store) Templates(ctx context.Context, status string) ([]Template, error) {
	q := `SELECT ` + tmplCols + ` FROM strategy_templates`
	var args []any
	if status != "" {
		q += ` WHERE status = $1::strategy_template_status_enum`
		args = append(args, status)
	}
	q += ` ORDER BY created_at DESC`
	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Template
	for rows.Next() {
		t, err := scanTemplate(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *t)
	}
	return out, rows.Err()
}

// DecideTemplate applies an admin approve/reject inside one transaction
// with the admin_audit_log + hash-chain row (spec §8.2 auditability).
// `reason` is the human-readable reject note; the audit action string
// is never repurposed as the reason. Returns the updated row; nil when
// the template was already decided.
func (s *Store) DecideTemplate(ctx context.Context, templateID int64,
	approve bool, reason string, actor admin.AuditEntry) (*Template, error) {

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var prior string
	if err := tx.QueryRow(ctx,
		`SELECT status::text FROM strategy_templates WHERE template_id=$1 FOR UPDATE`,
		templateID).Scan(&prior); err != nil {
		if err == pgx.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	if prior != TemplatePending {
		return nil, errorf(CodeTemplateNotApproved,
			"template %d already %s", templateID, prior)
	}
	to := TemplateApproved
	var approvedBy any
	var approvedAt any
	var rejectedAt any
	var rejectReason any
	if approve {
		approvedBy = actor.AdminUserID
		approvedAt = time.Now().UTC()
	} else {
		to = TemplateRejected
		rejectedAt = time.Now().UTC()
		rejectReason = nilStr(reason)
	}
	row := tx.QueryRow(ctx, `
		UPDATE strategy_templates SET status=$2::strategy_template_status_enum,
		    approved_by=$3, approved_at=$4, rejected_at=$5, reject_reason=$6,
		    updated_at=now()
		WHERE template_id=$1 RETURNING `+tmplCols,
		templateID, to, approvedBy, approvedAt, rejectedAt, rejectReason)
	t, err := scanTemplate(row)
	if err != nil {
		return nil, err
	}
	if _, _, err := admin.Log(ctx, tx, actor); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return t, nil
}

func decPtr(d *decimal.Decimal) any {
	if d == nil {
		return nil
	}
	return d.String()
}

func nilStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}
