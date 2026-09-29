// Persistence for grid_bots / grid_bot_orders (migration 071).
//
// Invariants enforced here, not just in the engine:
//   - the R13 five-bot cap is checked inside the create transaction under
//     the account row's FOR UPDATE lock — concurrent creates serialize on
//     the account and cannot both pass the count check;
//   - fills accumulate idempotently (filled_qty CAS against qty);
//   - counter-order claiming rides the source_child_id unique index.
package bots

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"exchange/pkg/decimal"
)

// PgStore persists grid bots + children on the OLTP pool.
type PgStore struct {
	pool *pgxpool.Pool
}

func NewPgStore(pool *pgxpool.Pool) *PgStore { return &PgStore{pool: pool} }

const botCols = `bot_id, account_id, instrument_id, symbol, lower_price::text,
	upper_price::text, grid_count, mode::text, total_investment::text,
	leverage::text, take_profit_price::text, stop_loss_price::text,
	status::text, realized_pnl::text, pnl_currency, fills_count,
	reference_price::text, stopped_at, stop_reason, created_at, updated_at`

func scanBot(row pgx.Row) (*GridBot, error) {
	var b GridBot
	var low, up, inv, lev, pnl string
	var tp, sl, ref *string
	var stoppedAt *time.Time
	var stopReason *string
	if err := row.Scan(&b.BotID, &b.AccountID, &b.InstrumentID, &b.Symbol,
		&low, &up, &b.GridCount, &b.Mode, &inv, &lev, &tp, &sl,
		&b.Status, &pnl, &b.PnLCurrency, &b.FillsCount, &ref,
		&stoppedAt, &stopReason, &b.CreatedAt, &b.UpdatedAt); err != nil {
		return nil, err
	}
	b.LowerPrice = decimal.RequireFromString(low)
	b.UpperPrice = decimal.RequireFromString(up)
	b.TotalInvestment = decimal.RequireFromString(inv)
	b.Leverage = decimal.RequireFromString(lev)
	b.RealizedPnL = decimal.RequireFromString(pnl)
	if tp != nil {
		d := decimal.RequireFromString(*tp)
		b.TakeProfitPrice = &d
	}
	if sl != nil {
		d := decimal.RequireFromString(*sl)
		b.StopLossPrice = &d
	}
	if ref != nil {
		d := decimal.RequireFromString(*ref)
		b.ReferencePrice = &d
	}
	b.StoppedAt = stoppedAt
	if stopReason != nil {
		b.StopReason = *stopReason
	}
	return &b, nil
}

const childCols = `id, bot_id, level_index, side, price::text, qty::text,
	order_id, client_order_id, status::text, filled_qty::text,
	avg_fill_price::text, source_child_id, realized_pnl::text,
	note, filled_at, created_at, updated_at`

func scanChild(row pgx.Row) (*GridChild, error) {
	var c GridChild
	var px, qty, fq, pnl string
	var orderID, srcID *int64
	var clientID, note *string
	var avg *string
	var filledAt *time.Time
	if err := row.Scan(&c.ID, &c.BotID, &c.LevelIndex, &c.Side, &px, &qty,
		&orderID, &clientID, &c.Status, &fq, &avg, &srcID, &pnl,
		&note, &filledAt, &c.CreatedAt, &c.UpdatedAt); err != nil {
		return nil, err
	}
	c.Price = decimal.RequireFromString(px)
	c.Qty = decimal.RequireFromString(qty)
	c.FilledQty = decimal.RequireFromString(fq)
	c.RealizedPnL = decimal.RequireFromString(pnl)
	c.OrderID = orderID
	c.SourceChildID = srcID
	if clientID != nil {
		c.ClientOrderID = *clientID
	}
	if note != nil {
		c.Note = *note
	}
	if avg != nil {
		d := decimal.RequireFromString(*avg)
		c.AvgFillPrice = &d
	}
	c.FilledAt = filledAt
	return &c, nil
}

func isNoRows(err error) bool { return err == pgx.ErrNoRows }

// CreateBot inserts the parent row + initial child intent rows in one
// transaction. The account row lock serializes concurrent creates so the
// R13 count check cannot race; exceeding MaxConcurrentBots surfaces
// MAX_GRID_BOTS_EXCEEDED.
func (s *PgStore) CreateBot(ctx context.Context, b *GridBot,
	children []GridChild) (*GridBot, error) {

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Serialize concurrent creates for this account.
	if _, err := tx.Exec(ctx,
		`SELECT id FROM accounts WHERE id = $1 FOR UPDATE`, b.AccountID); err != nil {
		return nil, fmt.Errorf("account lock: %w", err)
	}
	var running int
	if err := tx.QueryRow(ctx,
		`SELECT count(*) FROM grid_bots WHERE account_id=$1 AND status='RUNNING'`,
		b.AccountID).Scan(&running); err != nil {
		return nil, fmt.Errorf("grid bot count: %w", err)
	}
	if running >= MaxConcurrentBots {
		return nil, errorf(CodeMaxGridBotsExceeded,
			"account %d already runs %d grid bots (max %d)",
			b.AccountID, running, MaxConcurrentBots)
	}
	var id int64
	var created time.Time
	err = tx.QueryRow(ctx, `
		INSERT INTO grid_bots (account_id, instrument_id, symbol,
		    lower_price, upper_price, grid_count, mode, total_investment,
		    leverage, take_profit_price, stop_loss_price, status,
		    pnl_currency, reference_price)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,'RUNNING',$12,$13)
		RETURNING bot_id, created_at`,
		b.AccountID, b.InstrumentID, b.Symbol,
		b.LowerPrice.String(), b.UpperPrice.String(), b.GridCount, b.Mode,
		b.TotalInvestment.String(), b.Leverage.String(),
		decPtr(b.TakeProfitPrice), decPtr(b.StopLossPrice),
		b.PnLCurrency, decPtr(b.ReferencePrice)).Scan(&id, &created)
	if err != nil {
		return nil, fmt.Errorf("grid_bots insert: %w", err)
	}
	b.BotID = id
	b.CreatedAt = created
	for i := range children {
		children[i].BotID = id
		if _, err := tx.Exec(ctx, `
			INSERT INTO grid_bot_orders (bot_id, level_index, side, price,
			    qty, status, source_child_id, note)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
			id, children[i].LevelIndex, children[i].Side,
			children[i].Price.String(), children[i].Qty.String(),
			children[i].Status, children[i].SourceChildID,
			nilStr(children[i].Note)); err != nil {
			return nil, fmt.Errorf("grid_bot_orders insert lvl %d: %w",
				children[i].LevelIndex, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit grid bot: %w", err)
	}
	return b, nil
}

// GetBot returns the bot only when owned by accountID.
func (s *PgStore) GetBot(ctx context.Context, accountID, botID int64) (*GridBot, error) {
	b, err := scanBot(s.pool.QueryRow(ctx,
		`SELECT `+botCols+` FROM grid_bots WHERE bot_id=$1 AND account_id=$2`,
		botID, accountID))
	if isNoRows(err) {
		return nil, nil
	}
	return b, err
}

// BotByID is the internal (non-account-scoped) lookup used by the fill
// path, where the child row already pins ownership.
func (s *PgStore) BotByID(ctx context.Context, botID int64) (*GridBot, error) {
	b, err := scanBot(s.pool.QueryRow(ctx,
		`SELECT `+botCols+` FROM grid_bots WHERE bot_id=$1`, botID))
	if isNoRows(err) {
		return nil, nil
	}
	return b, err
}

func (s *PgStore) ListBots(ctx context.Context, accountID int64) ([]GridBot, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+botCols+` FROM grid_bots WHERE account_id=$1
		 ORDER BY created_at DESC, bot_id DESC`, accountID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []GridBot
	for rows.Next() {
		b, err := scanBot(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *b)
	}
	return out, rows.Err()
}

func (s *PgStore) Children(ctx context.Context, botID int64) ([]GridChild, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+childCols+` FROM grid_bot_orders WHERE bot_id=$1
		 ORDER BY level_index, id`, botID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []GridChild
	for rows.Next() {
		c, err := scanChild(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *c)
	}
	return out, rows.Err()
}

// OpenChildren returns children whose engine order may still be live —
// the cancel set for bot stop.
func (s *PgStore) OpenChildren(ctx context.Context, botID int64) ([]GridChild, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+childCols+` FROM grid_bot_orders WHERE bot_id=$1
		  AND status IN ('PENDING','WORKING') AND order_id IS NOT NULL
		 ORDER BY id`, botID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []GridChild
	for rows.Next() {
		c, err := scanChild(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *c)
	}
	return out, rows.Err()
}

// ChildByOrderID resolves the child row owning an engine order id
// (fill-hook lookup; nil when the order is not grid-owned).
func (s *PgStore) ChildByOrderID(ctx context.Context, orderID int64) (*GridChild, error) {
	c, err := scanChild(s.pool.QueryRow(ctx,
		`SELECT `+childCols+` FROM grid_bot_orders WHERE order_id=$1`, orderID))
	if isNoRows(err) {
		return nil, nil
	}
	return c, err
}

// BindOrder records the admitted order id on a PENDING child → WORKING.
func (s *PgStore) BindOrder(ctx context.Context, childID, orderID int64,
	clientOrderID string) error {

	_, err := s.pool.Exec(ctx, `
		UPDATE grid_bot_orders SET order_id=$2, client_order_id=$3,
		    status='WORKING', updated_at=now()
		WHERE id=$1 AND status='PENDING'`, childID, orderID, clientOrderID)
	return err
}

// RejectChild marks a PENDING/WORKING child REJECTED with the pipeline
// error code as the note — honest per-leg accounting.
func (s *PgStore) RejectChild(ctx context.Context, childID int64, note string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE grid_bot_orders SET status='REJECTED', note=$2, updated_at=now()
		WHERE id=$1 AND status IN ('PENDING','WORKING')`, childID, note)
	return err
}

// ApplyChildFill folds one engine fill into the child accounting —
// volume-weighted avg + cumulative qty, flipping to FILLED at completion
// (idempotent: redelivered fills cannot exceed qty or double-count).
// Returns the fresh child and whether THIS call completed the fill.
func (s *PgStore) ApplyChildFill(ctx context.Context, childID int64,
	px, qty decimal.Decimal) (*GridChild, bool, error) {

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var c GridChild
	var curQty, curFilled string
	var avgNull *string
	var botID int64
	err = tx.QueryRow(ctx, `
		SELECT bot_id, qty::text, filled_qty::text,
		       avg_fill_price::text, status::text
		FROM grid_bot_orders WHERE id=$1 FOR UPDATE`, childID).
		Scan(&botID, &curQty, &curFilled, &avgNull, &c.Status)
	if err != nil {
		if isNoRows(err) {
			return nil, false, nil
		}
		return nil, false, err
	}
	if c.Status != ChildWorking && c.Status != ChildPending {
		return nil, false, nil // terminal/redelivered — idempotent no-op
	}
	botQty := decimal.RequireFromString(curQty)
	filled := decimal.RequireFromString(curFilled).Add(qty)
	if filled.GreaterThan(botQty) {
		filled = botQty // engine over-report guard — clamp, never overshoot
	}
	avg := px
	if avgNull != nil {
		prevFilled := decimal.RequireFromString(curFilled)
		prevAvg := decimal.RequireFromString(*avgNull)
		avg = prevAvg.Mul(prevFilled).Add(px.Mul(qty)).Div(filled)
	}
	full := filled.GreaterThanOrEqual(botQty)
	newStatus := ChildWorking
	var filledAt any
	if full {
		newStatus = ChildFilled
		filledAt = time.Now().UTC()
	}
	if _, err := tx.Exec(ctx, `
		UPDATE grid_bot_orders SET filled_qty=$2::numeric,
		    avg_fill_price=$3::numeric, status=$4, filled_at=$5,
		    updated_at=now()
		WHERE id=$1`, childID, filled.String(), avg.String(),
		newStatus, filledAt); err != nil {
		return nil, false, err
	}
	if full {
		if _, err := tx.Exec(ctx,
			`UPDATE grid_bots SET fills_count=fills_count+1, updated_at=now()
			 WHERE bot_id=$1`, botID); err != nil {
			return nil, false, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, false, err
	}
	fresh, err := s.ChildByID(ctx, childID)
	return fresh, full, err
}

func (s *PgStore) ChildByID(ctx context.Context, childID int64) (*GridChild, error) {
	c, err := scanChild(s.pool.QueryRow(ctx,
		`SELECT `+childCols+` FROM grid_bot_orders WHERE id=$1`, childID))
	if isNoRows(err) {
		return nil, nil
	}
	return c, err
}

// LevelOccupied reports whether a live (PENDING/WORKING) child already
// covers (level, side) — the price-collision skip rule.
func (s *PgStore) LevelOccupied(ctx context.Context, botID int64,
	level int, side string) (bool, error) {

	var n int
	err := s.pool.QueryRow(ctx, `
		SELECT count(*) FROM grid_bot_orders
		WHERE bot_id=$1 AND level_index=$2 AND side=$3
		  AND status IN ('PENDING','WORKING')`, botID, level, side).Scan(&n)
	return n > 0, err
}

// ClaimCounter inserts the counter-order row for a filled source child.
// The source_child_id unique index makes this idempotent: claimed=false
// means a counter was already claimed (at-least-once dedup), and the
// caller must NOT submit again. c.Status/c.Note are honored so
// collision skips persist as SKIPPED rather than phantom PENDING legs.
func (s *PgStore) ClaimCounter(ctx context.Context, c *GridChild) (int64, bool, error) {
	status := c.Status
	if status == "" {
		status = ChildPending
	}
	var id int64
	err := s.pool.QueryRow(ctx, `
		INSERT INTO grid_bot_orders (bot_id, level_index, side, price, qty,
		    status, source_child_id, note)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
		ON CONFLICT (source_child_id) WHERE source_child_id IS NOT NULL
		DO NOTHING
		RETURNING id`,
		c.BotID, c.LevelIndex, c.Side, c.Price.String(), c.Qty.String(),
		status, c.SourceChildID, nilStr(c.Note)).Scan(&id)
	if isNoRows(err) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("counter claim: %w", err)
	}
	return id, true, nil
}

// CreditRoundTrip books a completed grid round-trip: the counter child's
// realized_pnl plus the bot-level cumulative add — one transaction so
// the parent rollup can never drift from child accounting.
func (s *PgStore) CreditRoundTrip(ctx context.Context, botID, counterChildID int64,
	pnl decimal.Decimal) error {

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `
		UPDATE grid_bot_orders SET realized_pnl=realized_pnl+$2::numeric,
		    updated_at=now()
		WHERE id=$1`, counterChildID, pnl.String()); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE grid_bots SET realized_pnl=realized_pnl+$2::numeric,
		    updated_at=now()
		WHERE bot_id=$1`, botID, pnl.String()); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// Transition flips bot status atomically; returns false when the bot was
// already terminal (stop/delete idempotency).
func (s *PgStore) Transition(ctx context.Context, botID int64,
	from []string, to, reason string) (bool, error) {

	tag, err := s.pool.Exec(ctx, `
		UPDATE grid_bots SET status=$3::grid_bot_status_enum, stop_reason=$4,
		    stopped_at=now(), updated_at=now()
		WHERE bot_id=$1 AND status = ANY($2::grid_bot_status_enum[])`,
		botID, from, to, nilStr(reason))
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// CancelChild marks a child CANCELLED after the engine-side cancel.
func (s *PgStore) CancelChild(ctx context.Context, childID int64) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE grid_bot_orders SET status='CANCELLED', updated_at=now()
		WHERE id=$1 AND status IN ('PENDING','WORKING')`, childID)
	return err
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
