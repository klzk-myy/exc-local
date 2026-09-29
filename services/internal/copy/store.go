package copy

import (
	"context"
	stderrors "errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"exchange/pkg/decimal"
)

// Store is the product layer's persistence seam — PgxStore in production,
// fakes in unit tests.
type Store interface {
	// Strategies
	CreateStrategy(ctx context.Context, s *Strategy) (*Strategy, error)
	StrategyByID(ctx context.Context, id int64) (*Strategy, error)
	StrategyForUpdate(ctx context.Context, id int64) (*Strategy, error)
	StrategiesByStatus(ctx context.Context, st StrategyStatus) ([]Strategy, error)
	// StrategiesByManager returns the manager's strategies (all statuses —
	// the fan-out decides which still receive copies; SUSPENDED keeps
	// existing follows per spec).
	StrategiesByManager(ctx context.Context, managerAccountID int64) ([]Strategy, error)
	// ListStrategy flips INCUBATING → LISTED setting listed_at; the WHERE
	// status='INCUBATING' makes the transition atomic (row-level gate —
	// the caller's precondition checks ran inside the same serializable tx
	// where possible, and the predicate still blocks races).
	ListStrategy(ctx context.Context, id int64) error
	// SuspendStrategy flips any non-SUSPENDED strategy to SUSPENDED with a
	// compliance reason + timestamp. Never deletes.
	SuspendStrategy(ctx context.Context, id int64, reason string) error

	// Follows
	CreateFollow(ctx context.Context, f *Follow) (*Follow, error)
	FollowByID(ctx context.Context, id int64) (*Follow, error)
	ActiveFollowsForStrategy(ctx context.Context, strategyID int64) ([]Follow, error)
	// Unfollow flips status ACTIVE → UNFOLLOWED atomically (predicate
	// guards double-unfollow) and returns the updated row.
	Unfollow(ctx context.Context, followID int64) (*Follow, error)

	// Child orders
	// InsertChildOrders inserts intent rows, dedup-keyed per
	// (master_trade_id, follow_id); returns the rows ACTUALLY inserted
	// with their child_ids (deduped replays are absent).
	InsertChildOrders(ctx context.Context, rows []ChildOrder) ([]ChildOrder, error)
	CancelPendingChildren(ctx context.Context, followID int64) ([]ChildOrder, error)
	UpdateChildStatus(ctx context.Context, childID int64, st ChildStatus,
		childOrderID *int64, notice string) error

	// High-water marks + accruals
	HWMForUpdate(ctx context.Context, followID int64) (*HighWaterMark, error)
	RatchetHWM(ctx context.Context, followID int64, newWM decimal.Decimal,
		settledAt time.Time) error
	InsertAccrual(ctx context.Context, a *Accrual) (*Accrual, error)

	// Computed stats inputs (discovery)
	ManagerTrades(ctx context.Context, managerAccountID int64) ([]ManagerFill, error)
	FollowerStats(ctx context.Context, strategyID int64) (count int64, aum decimal.Decimal, err error)
	// InstrumentMinQty resolves the instrument's minimum order quantity —
	// the min-notional floor child orders are scaled against.
	InstrumentMinQty(ctx context.Context, instrumentID int64) (decimal.Decimal, error)
}

// ManagerFill is one executed trade leg attributed to the manager account
// — the raw input for computed-only discovery stats.
type ManagerFill struct {
	TradeID      int64
	InstrumentID int64
	Side         string // BUY | SELL — resolved from buyer/seller account
	Quantity     decimal.Decimal
	Price        decimal.Decimal
	QuoteCcy     string
	BaseCcy      string
	CreatedAt    time.Time
}

// PgxStore is the production Store.
type PgxStore struct {
	pool *pgxpool.Pool
}

// NewPgxStore binds the OLTP pool — fail closed on nil.
func NewPgxStore(pool *pgxpool.Pool) (*PgxStore, error) {
	if pool == nil {
		return nil, fmt.Errorf("copy: store requires a pgx pool (fail closed)")
	}
	return &PgxStore{pool: pool}, nil
}

const strategyCols = `strategy_id, manager_account_id, display_name, description,
	currency, instrument_class, status::text, profit_share_pct::text,
	incubating_since, listed_at, suspended_at, COALESCE(suspend_reason,''), created_at`

func scanStrategy(row pgx.Row) (*Strategy, error) {
	var s Strategy
	var pct string
	err := row.Scan(&s.StrategyID, &s.ManagerAccountID, &s.DisplayName, &s.Description,
		&s.Currency, &s.InstrumentClass, &s.Status, &pct,
		&s.IncubatingSince, &s.ListedAt, &s.SuspendedAt, &s.SuspendReason, &s.CreatedAt)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	s.ProfitSharePct = decimal.RequireFromString(pct)
	return &s, nil
}

// CreateStrategy implements Store — a new profile starts INCUBATING; the
// incubation clock starts at row creation.
func (s *PgxStore) CreateStrategy(ctx context.Context, st *Strategy) (*Strategy, error) {
	row, err := scanStrategy(s.pool.QueryRow(ctx, `
		INSERT INTO strategy_profiles
		    (manager_account_id, display_name, description, currency,
		     instrument_class, profit_share_pct)
		VALUES ($1,$2,$3,$4,$5,$6::numeric)
		RETURNING `+strategyCols,
		st.ManagerAccountID, st.DisplayName, st.Description, st.Currency,
		st.InstrumentClass, st.ProfitSharePct.String()))
	if err != nil {
		return nil, errorf(CodeInternalError, "create strategy: %v", err)
	}
	return row, nil
}

// StrategyByID implements Store; nil,nil when absent.
func (s *PgxStore) StrategyByID(ctx context.Context, id int64) (*Strategy, error) {
	st, err := scanStrategy(s.pool.QueryRow(ctx,
		`SELECT `+strategyCols+` FROM strategy_profiles WHERE strategy_id = $1`, id))
	if err != nil {
		return nil, errorf(CodeInternalError, "read strategy %d: %v", id, err)
	}
	return st, nil
}

// StrategyForUpdate implements Store — locks the row for status transitions.
func (s *PgxStore) StrategyForUpdate(ctx context.Context, id int64) (*Strategy, error) {
	st, err := scanStrategy(s.pool.QueryRow(ctx,
		`SELECT `+strategyCols+` FROM strategy_profiles WHERE strategy_id = $1 FOR UPDATE`, id))
	if err != nil {
		return nil, errorf(CodeInternalError, "lock strategy %d: %v", id, err)
	}
	return st, nil
}

// StrategiesByStatus implements Store — stable strategy_id order.
func (s *PgxStore) StrategiesByStatus(ctx context.Context, st StrategyStatus) ([]Strategy, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+strategyCols+` FROM strategy_profiles WHERE status = $1
		 ORDER BY strategy_id`, string(st))
	if err != nil {
		return nil, errorf(CodeInternalError, "list strategies: %v", err)
	}
	defer rows.Close()
	out := []Strategy{}
	for rows.Next() {
		var st Strategy
		var pct string
		if err := rows.Scan(&st.StrategyID, &st.ManagerAccountID, &st.DisplayName,
			&st.Description, &st.Currency, &st.InstrumentClass, &st.Status, &pct,
			&st.IncubatingSince, &st.ListedAt, &st.SuspendedAt, &st.SuspendReason,
			&st.CreatedAt); err != nil {
			return nil, errorf(CodeInternalError, "scan strategy: %v", err)
		}
		st.ProfitSharePct = decimal.RequireFromString(pct)
		out = append(out, st)
	}
	return out, rows.Err()
}

// StrategiesByManager implements Store.
func (s *PgxStore) StrategiesByManager(ctx context.Context, managerAccountID int64) ([]Strategy, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+strategyCols+` FROM strategy_profiles WHERE manager_account_id = $1
		 ORDER BY strategy_id`, managerAccountID)
	if err != nil {
		return nil, errorf(CodeInternalError, "manager strategies: %v", err)
	}
	defer rows.Close()
	out := []Strategy{}
	for rows.Next() {
		var st Strategy
		var pct string
		if err := rows.Scan(&st.StrategyID, &st.ManagerAccountID, &st.DisplayName,
			&st.Description, &st.Currency, &st.InstrumentClass, &st.Status, &pct,
			&st.IncubatingSince, &st.ListedAt, &st.SuspendedAt, &st.SuspendReason,
			&st.CreatedAt); err != nil {
			return nil, errorf(CodeInternalError, "scan strategy: %v", err)
		}
		st.ProfitSharePct = decimal.RequireFromString(pct)
		out = append(out, st)
	}
	return out, rows.Err()
}

// ListStrategy implements Store — atomic INCUBATING → LISTED.
func (s *PgxStore) ListStrategy(ctx context.Context, id int64) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE strategy_profiles
		   SET status = 'LISTED', listed_at = now(), updated_at = now()
		 WHERE strategy_id = $1 AND status = 'INCUBATING'`, id)
	if err != nil {
		return errorf(CodeInternalError, "list strategy %d: %v", id, err)
	}
	if tag.RowsAffected() == 0 {
		return errorf(CodeForbidden, "strategy %d is not INCUBATING", id)
	}
	return nil
}

// SuspendStrategy implements Store — compliance suspension is a status
// flip, never a delete; existing follows keep running.
func (s *PgxStore) SuspendStrategy(ctx context.Context, id int64, reason string) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE strategy_profiles
		   SET status = 'SUSPENDED', suspended_at = now(), suspend_reason = $2,
		       updated_at = now()
		 WHERE strategy_id = $1 AND status <> 'SUSPENDED'`, id, reason)
	if err != nil {
		return errorf(CodeInternalError, "suspend strategy %d: %v", id, err)
	}
	if tag.RowsAffected() == 0 {
		return errorf(CodeNotFound, "strategy %d not found or already suspended", id)
	}
	return nil
}

const followCols = `follow_id, investor_account_id, strategy_id,
	allocation_notional::text, currency, safety_mode::text,
	stop_loss_cap::text, status::text, unfollowed_at, created_at`

func scanFollow(row pgx.Row) (*Follow, error) {
	var f Follow
	var notional string
	var slc *string
	err := row.Scan(&f.FollowID, &f.InvestorAccountID, &f.StrategyID, &notional,
		&f.Currency, &f.SafetyMode, &slc, &f.Status, &f.UnfollowedAt, &f.CreatedAt)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	f.AllocationNotional = decimal.RequireFromString(notional)
	if slc != nil {
		d := decimal.RequireFromString(*slc)
		f.StopLossCap = &d
	}
	return &f, nil
}

// CreateFollow implements Store — the ACTIVE partial UNIQUE index turns a
// duplicate-follow race into 23505 (surfaced as INVALID_REQUEST).
func (s *PgxStore) CreateFollow(ctx context.Context, f *Follow) (*Follow, error) {
	var slc any
	if f.StopLossCap != nil {
		slc = f.StopLossCap.String()
	}
	row := s.pool.QueryRow(ctx, `
		INSERT INTO copy_follows
		    (investor_account_id, strategy_id, allocation_notional, currency,
		     safety_mode, stop_loss_cap)
		VALUES ($1,$2,$3::numeric,$4,$5::copy_safety_mode_enum,$6::numeric)
		RETURNING `+followCols,
		f.InvestorAccountID, f.StrategyID, f.AllocationNotional.String(),
		f.Currency, string(f.SafetyMode), slc)
	out, err := scanFollow(row)
	if err != nil {
		if isUniqueViolation(err) {
			return nil, errorf(CodeInvalidRequest,
				"investor %d already actively follows strategy %d",
				f.InvestorAccountID, f.StrategyID)
		}
		return nil, errorf(CodeInternalError, "create follow: %v", err)
	}
	return out, nil
}

// FollowByID implements Store.
func (s *PgxStore) FollowByID(ctx context.Context, id int64) (*Follow, error) {
	f, err := scanFollow(s.pool.QueryRow(ctx,
		`SELECT `+followCols+` FROM copy_follows WHERE follow_id = $1`, id))
	if err != nil {
		return nil, errorf(CodeInternalError, "read follow %d: %v", id, err)
	}
	return f, nil
}

// ActiveFollowsForStrategy implements Store — stable follow_id order for
// deterministic fan-out.
func (s *PgxStore) ActiveFollowsForStrategy(ctx context.Context, strategyID int64) ([]Follow, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+followCols+` FROM copy_follows
		 WHERE strategy_id = $1 AND status = 'ACTIVE' ORDER BY follow_id`, strategyID)
	if err != nil {
		return nil, errorf(CodeInternalError, "list follows: %v", err)
	}
	defer rows.Close()
	out := []Follow{}
	for rows.Next() {
		var f Follow
		var notional string
		var slc *string
		if err := rows.Scan(&f.FollowID, &f.InvestorAccountID, &f.StrategyID,
			&notional, &f.Currency, &f.SafetyMode, &slc, &f.Status,
			&f.UnfollowedAt, &f.CreatedAt); err != nil {
			return nil, errorf(CodeInternalError, "scan follow: %v", err)
		}
		f.AllocationNotional = decimal.RequireFromString(notional)
		if slc != nil {
			d := decimal.RequireFromString(*slc)
			f.StopLossCap = &d
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// Unfollow implements Store — ACTIVE → UNFOLLOWED atomically.
func (s *PgxStore) Unfollow(ctx context.Context, followID int64) (*Follow, error) {
	f, err := scanFollow(s.pool.QueryRow(ctx, `
		UPDATE copy_follows
		   SET status = 'UNFOLLOWED', unfollowed_at = now(), updated_at = now()
		 WHERE follow_id = $1 AND status = 'ACTIVE'
		RETURNING `+followCols, followID))
	if err != nil {
		return nil, errorf(CodeInternalError, "unfollow %d: %v", followID, err)
	}
	if f == nil {
		return nil, errorf(CodeNotFound, "active follow %d not found", followID)
	}
	return f, nil
}

// InsertChildOrders implements Store — UNIQUE (master_trade_id,
// follow_id) dedups replays; RETURNING yields the inserted rows with ids.
func (s *PgxStore) InsertChildOrders(ctx context.Context, rows []ChildOrder) ([]ChildOrder, error) {
	out := make([]ChildOrder, 0, len(rows))
	for _, c := range rows {
		var id int64
		err := s.pool.QueryRow(ctx, `
			INSERT INTO copy_child_orders
			    (follow_id, master_trade_id, instrument_id, side, quantity,
			     master_price, status, notice)
			VALUES ($1,$2,$3,$4,$5::numeric,$6::numeric,$7::copy_child_status_enum,NULLIF($8,''))
			ON CONFLICT (master_trade_id, follow_id) DO NOTHING
			RETURNING child_id`,
			c.FollowID, c.MasterTradeID, c.InstrumentID, c.Side,
			c.Quantity.String(), c.MasterPrice.String(), string(c.Status), c.Notice).Scan(&id)
		if err == pgx.ErrNoRows {
			continue // deduped replay row
		}
		if err != nil {
			return out, errorf(CodeInternalError, "insert child order: %v", err)
		}
		c.ChildID = id
		out = append(out, c)
	}
	return out, nil
}

// CancelPendingChildren implements Store — unfollow cancels every PENDING
// child; SUBMITTED ones are handled by the cancel seam in the engine.
func (s *PgxStore) CancelPendingChildren(ctx context.Context, followID int64) ([]ChildOrder, error) {
	rows, err := s.pool.Query(ctx, `
		UPDATE copy_child_orders
		   SET status = 'CANCELLED', notice = 'cancelled by unfollow', updated_at = now()
		 WHERE follow_id = $1 AND status = 'PENDING'
		RETURNING child_id, follow_id, master_trade_id, instrument_id, side,
		          quantity::text, master_price::text, status::text,
		          child_order_id, COALESCE(notice,''), created_at`, followID)
	if err != nil {
		return nil, errorf(CodeInternalError, "cancel children: %v", err)
	}
	defer rows.Close()
	out := []ChildOrder{}
	for rows.Next() {
		var c ChildOrder
		var qty, px string
		if err := rows.Scan(&c.ChildID, &c.FollowID, &c.MasterTradeID,
			&c.InstrumentID, &c.Side, &qty, &px, &c.Status, &c.ChildOrderID,
			&c.Notice, &c.CreatedAt); err != nil {
			return nil, errorf(CodeInternalError, "scan child: %v", err)
		}
		c.Quantity = decimal.RequireFromString(qty)
		c.MasterPrice = decimal.RequireFromString(px)
		out = append(out, c)
	}
	return out, rows.Err()
}

// UpdateChildStatus implements Store.
func (s *PgxStore) UpdateChildStatus(ctx context.Context, childID int64,
	st ChildStatus, childOrderID *int64, notice string) error {
	if _, err := s.pool.Exec(ctx, `
		UPDATE copy_child_orders
		   SET status = $2::copy_child_status_enum, child_order_id = $3,
		       notice = NULLIF($4,''), updated_at = now()
		 WHERE child_id = $1`, childID, string(st), childOrderID, notice); err != nil {
		return errorf(CodeInternalError, "update child %d: %v", childID, err)
	}
	return nil
}

// HWMForUpdate implements Store — creates the zero baseline on first use
// so every ACTIVE follow always has a watermark row.
func (s *PgxStore) HWMForUpdate(ctx context.Context, followID int64) (*HighWaterMark, error) {
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO high_water_marks (follow_id) VALUES ($1)
		ON CONFLICT (follow_id) DO NOTHING`, followID); err != nil {
		return nil, errorf(CodeInternalError, "ensure hwm %d: %v", followID, err)
	}
	var h HighWaterMark
	var wm string
	if err := s.pool.QueryRow(ctx, `
		SELECT follow_id, watermark_pnl::text, last_settled_at
		  FROM high_water_marks WHERE follow_id = $1 FOR UPDATE`, followID).
		Scan(&h.FollowID, &wm, &h.LastSettledAt); err != nil {
		return nil, errorf(CodeInternalError, "lock hwm %d: %v", followID, err)
	}
	h.WatermarkPnL = decimal.RequireFromString(wm)
	return &h, nil
}

// RatchetHWM implements Store — the watermark may only move UP; a lower
// proposed value is a no-op return (loss months never reset the mark).
func (s *PgxStore) RatchetHWM(ctx context.Context, followID int64,
	newWM decimal.Decimal, settledAt time.Time) error {
	if _, err := s.pool.Exec(ctx, `
		UPDATE high_water_marks
		   SET watermark_pnl = $2::numeric, last_settled_at = $3, updated_at = now()
		 WHERE follow_id = $1 AND $2::numeric > watermark_pnl`,
		followID, newWM.String(), settledAt); err != nil {
		return errorf(CodeInternalError, "ratchet hwm %d: %v", followID, err)
	}
	return nil
}

// InsertAccrual implements Store — UNIQUE (follow_id, period_end) makes
// re-settlement of the same period idempotent.
func (s *PgxStore) InsertAccrual(ctx context.Context, a *Accrual) (*Accrual, error) {
	var id int64
	err := s.pool.QueryRow(ctx, `
		INSERT INTO profit_share_accruals
		    (follow_id, strategy_id, period_start, period_end, pnl, currency,
		     watermark_before, watermark_after, accrued_amount, journal_entry_id)
		VALUES ($1,$2,$3,$4,$5::numeric,$6,$7::numeric,$8::numeric,$9::numeric,$10)
		RETURNING accrual_id`,
		a.FollowID, a.StrategyID, a.PeriodStart, a.PeriodEnd, a.PnL.String(),
		a.Currency, a.WatermarkBefore.String(), a.WatermarkAfter.String(),
		a.AccruedAmount.String(), a.JournalEntryID).Scan(&id)
	if err != nil {
		if isUniqueViolation(err) {
			return nil, errorf(CodeIdempotencyCollision,
				"accrual for follow %d period %s already recorded",
				a.FollowID, a.PeriodEnd.Format("2006-01-02"))
		}
		return nil, errorf(CodeInternalError, "insert accrual: %v", err)
	}
	a.AccrualID = id
	a.Status = AccrualAccrued
	return a, nil
}

// ManagerTrades implements Store — every fill where the manager account
// was buyer or seller, in stable (created_at, id) order. The realized-P&L
// computation consumes these verbatim.
func (s *PgxStore) ManagerTrades(ctx context.Context, managerAccountID int64) ([]ManagerFill, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT t.id, t.instrument_id,
		       CASE WHEN t.buyer_account_id = $1 THEN 'BUY' ELSE 'SELL' END,
		       t.quantity::text, t.price::text,
		       i.quote_currency, i.base_currency, t.created_at
		  FROM trades t
		  JOIN instruments i ON i.id = t.instrument_id
		 WHERE t.buyer_account_id = $1 OR t.seller_account_id = $1
		 ORDER BY t.created_at, t.id`, managerAccountID)
	if err != nil {
		return nil, errorf(CodeInternalError, "manager trades: %v", err)
	}
	defer rows.Close()
	out := []ManagerFill{}
	for rows.Next() {
		var f ManagerFill
		var qty, px string
		if err := rows.Scan(&f.TradeID, &f.InstrumentID, &f.Side, &qty, &px,
			&f.QuoteCcy, &f.BaseCcy, &f.CreatedAt); err != nil {
			return nil, errorf(CodeInternalError, "scan trade: %v", err)
		}
		f.Quantity = decimal.RequireFromString(qty)
		f.Price = decimal.RequireFromString(px)
		out = append(out, f)
	}
	return out, rows.Err()
}

// FollowerStats implements Store — follower count + AUM as Σ ACTIVE
// allocation_notional in the strategy currency (the durable in-repo AUM
// read source documented in doc.go).
func (s *PgxStore) FollowerStats(ctx context.Context, strategyID int64) (int64, decimal.Decimal, error) {
	var count int64
	var aum string
	err := s.pool.QueryRow(ctx, `
		SELECT COUNT(*), COALESCE(SUM(allocation_notional),0)::text
		  FROM copy_follows WHERE strategy_id = $1 AND status = 'ACTIVE'`,
		strategyID).Scan(&count, &aum)
	if err != nil {
		return 0, decimal.Zero, errorf(CodeInternalError, "follower stats: %v", err)
	}
	return count, decimal.RequireFromString(aum), nil
}

// InstrumentMinQty implements Store — instruments.min_order_qty is the
// venue's declared minimum; child orders below it after safety scaling
// are recorded SKIPPED_MIN_NOTIONAL.
func (s *PgxStore) InstrumentMinQty(ctx context.Context, instrumentID int64) (decimal.Decimal, error) {
	var q string
	err := s.pool.QueryRow(ctx,
		`SELECT min_order_qty::text FROM instruments WHERE id = $1`, instrumentID).Scan(&q)
	if err == pgx.ErrNoRows {
		return decimal.Zero, errorf(CodeNotFound, "instrument %d not found", instrumentID)
	}
	if err != nil {
		return decimal.Zero, errorf(CodeInternalError, "instrument %d: %v", instrumentID, err)
	}
	return decimal.RequireFromString(q), nil
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return stderrors.As(err, &pgErr) && pgErr.Code == "23505"
}
