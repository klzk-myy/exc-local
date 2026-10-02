// liquidation_store.go — PgLiquidationStore: the PostgreSQL
// implementation of the LiquidationStore + AuctionStore seams declared
// in liquidation.go / auction.go (Phase-19 Task 19.3.3; spec §5.13
// positions, §5.14 liquidation_auctions, §13.4 auction ladder, §13.5
// scanner, §13.15 item 4; migrations 013/014/015/020/042/106/230).
//
// Conventions mirror margin_store.go: every DECIMAL column scans as
// ::text into the decimal facade — never float64 — and a bad row fails
// the whole read (§2.7 fail-closed: a liquidation scan must never
// silently skip corrupt data).
//
// Mark-price note (applies to OpenPositions, OpenInterest,
// IsolatedBreaches, PositionByID): the venue's latest mark lives in
// Redis (mark:{symbol}, keys.go) and is mirrored onto positions.mark_price
// by the margin engine's write-back path. No dedicated mark table exists
// in the Phase-19 schema — Phase-19.5's oracle keeps updating the same
// column. A NULL mark therefore means "never written", and the only
// honest in-store value left is entry_price; every read below uses
// COALESCE(p.mark_price, p.entry_price). The engine re-reads the live
// Redis mark on the close path — this fallback only bounds the PG view.
package risk

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"exchange/pkg/decimal"
	excerrors "exchange/pkg/errors"
)

// PgLiquidationStore implements LiquidationStore and AuctionStore over
// pgx — one concrete type serving both seams per the Task-19.3.3 design
// (the auction engine is constructed against the same persistence object
// as the liquidation engine).
type PgLiquidationStore struct {
	Pool *pgxpool.Pool
}

var (
	_ LiquidationStore = (*PgLiquidationStore)(nil)
	_ AuctionStore     = (*PgLiquidationStore)(nil)
)

// NewPgLiquidationStore binds the pool. A nil pool is rejected
// fail-closed — a liquidation engine without persistence strands
// bankrupt accounts (§2.7).
func NewPgLiquidationStore(pool *pgxpool.Pool) (*PgLiquidationStore, error) {
	if pool == nil {
		return nil, fmt.Errorf("liquidation store: nil pgx pool")
	}
	return &PgLiquidationStore{Pool: pool}, nil
}

// ---------------------------------------------------------------------------
// Position reads
// ---------------------------------------------------------------------------

// liqPositionCols is the shared LiqPosition projection. mark_price
// coalesces to entry_price (NULL = never marked — see file header);
// liquidation_price coalesces to 0 (LiqPosition documents "may be zero
// when unset"). The margin mode resolves from margin_accounts and falls
// back to the §13.1 category default (MarginService.ModeFor mirror):
// PROFESSIONAL/ELIGIBLE_COUNTERPARTY → PORTFOLIO, otherwise CROSS.
const liqPositionCols = `
	p.id, p.account_id, p.instrument_id, i.symbol,
	p.side::text, p.quantity::text, p.entry_price::text,
	COALESCE(p.mark_price, p.entry_price)::text,
	COALESCE(p.liquidation_price, 0)::text,
	p.unrealized_pnl::text, p.margin_used::text,
	COALESCE(ma.margin_mode::text,
	         CASE WHEN a.client_category::text IN ('PROFESSIONAL','ELIGIBLE_COUNTERPARTY')
	              THEN 'PORTFOLIO' ELSE 'CROSS' END)`

const liqPositionFrom = `
	FROM positions p
	JOIN instruments i ON i.id = p.instrument_id
	LEFT JOIN margin_accounts ma ON ma.account_id = p.account_id
	LEFT JOIN accounts a ON a.id = p.account_id`

// scanLiqPosition decodes one liqPositionCols row. Unparseable decimals
// are errors — a liquidation scan must never skip a position it cannot
// value (§2.7).
func scanLiqPosition(row pgx.Row) (*LiqPosition, error) {
	var (
		p                      LiqPosition
		qty, entry, mark, liq  string
		upnl, marginUsed, mode string
	)
	if err := row.Scan(&p.ID, &p.AccountID, &p.InstrumentID, &p.Symbol,
		&p.Side, &qty, &entry, &mark, &liq, &upnl, &marginUsed, &mode); err != nil {
		return nil, err
	}
	var err error
	if p.Quantity, err = decimal.NewFromString(qty); err != nil {
		return nil, fmt.Errorf("position %d quantity %q: %w", p.ID, qty, err)
	}
	if p.EntryPrice, err = decimal.NewFromString(entry); err != nil {
		return nil, fmt.Errorf("position %d entry_price %q: %w", p.ID, entry, err)
	}
	if p.MarkPrice, err = decimal.NewFromString(mark); err != nil {
		return nil, fmt.Errorf("position %d mark_price %q: %w", p.ID, mark, err)
	}
	if p.LiquidationPrice, err = decimal.NewFromString(liq); err != nil {
		return nil, fmt.Errorf("position %d liquidation_price %q: %w", p.ID, liq, err)
	}
	if p.UnrealizedPnl, err = decimal.NewFromString(upnl); err != nil {
		return nil, fmt.Errorf("position %d unrealized_pnl %q: %w", p.ID, upnl, err)
	}
	if p.MarginUsed, err = decimal.NewFromString(marginUsed); err != nil {
		return nil, fmt.Errorf("position %d margin_used %q: %w", p.ID, marginUsed, err)
	}
	if _, ok := normalizeMode(mode); !ok {
		return nil, fmt.Errorf("position %d: unknown margin mode %q", p.ID, mode)
	}
	p.MarginMode = mode
	return &p, nil
}

func (s *PgLiquidationStore) queryPositions(ctx context.Context,
	where string, args ...any) ([]LiqPosition, error) {

	rows, err := s.Pool.Query(ctx, `SELECT `+liqPositionCols+liqPositionFrom+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []LiqPosition
	for rows.Next() {
		p, err := scanLiqPosition(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *p)
	}
	return out, rows.Err()
}

// OpenPositions implements LiquidationStore — the account's live
// positions, worst unrealized P&L first (§13.3 precedence rule 2: the
// engine closes losers before winners until the level recovers).
func (s *PgLiquidationStore) OpenPositions(ctx context.Context, accountID int64) ([]LiqPosition, error) {
	out, err := s.queryPositions(ctx, `
		WHERE p.account_id = $1 AND p.quantity <> 0
		ORDER BY p.unrealized_pnl ASC, p.id`, accountID)
	if err != nil {
		return nil, fmt.Errorf("open positions acct %d: %w", accountID, err)
	}
	return out, nil
}

// PositionByID implements AuctionStore — (nil, nil) when the row is gone
// or already settled (quantity = 0): the auction is finished regardless
// of its own row state (the advancer retires it via complete()).
func (s *PgLiquidationStore) PositionByID(ctx context.Context, positionID int64) (*LiqPosition, error) {
	p, err := scanLiqPosition(s.Pool.QueryRow(ctx, `SELECT `+liqPositionCols+liqPositionFrom+`
		WHERE p.id = $1 AND p.quantity <> 0`, positionID))
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("position read %d: %w", positionID, err)
	}
	return p, nil
}

// IsolatedBreaches implements LiquidationStore — ISOLATED-mode positions
// whose effective mark has crossed liquidation_price (LONG breaches on
// mark ≤ liq; SHORT on mark ≥ liq). Positions with no liquidation_price
// recorded are excluded — a NULL basis cannot prove a breach.
func (s *PgLiquidationStore) IsolatedBreaches(ctx context.Context) ([]LiqPosition, error) {
	out, err := s.queryPositions(ctx, `
		WHERE ma.margin_mode = 'ISOLATED'
		  AND p.quantity <> 0
		  AND p.liquidation_price > 0
		  AND (
		        (p.side = 'LONG'  AND COALESCE(p.mark_price, p.entry_price) <= p.liquidation_price)
		     OR (p.side = 'SHORT' AND COALESCE(p.mark_price, p.entry_price) >= p.liquidation_price)
		  )
		ORDER BY p.id`)
	if err != nil {
		return nil, fmt.Errorf("isolated breach scan: %w", err)
	}
	return out, nil
}

// OpenInterest implements LiquidationStore — the §13.4 auction-trigger
// denominator: aggregate open margin-position NOTIONAL for the
// instrument, Σ |quantity| × COALESCE(mark_price, entry_price) over
// quantity <> 0 rows (mark fallback per the file-header note — a mark
// table does not exist; positions.mark_price is the oracle mirror and
// entry_price is the last honest value when it was never written). 0 is
// honest output: the engine never opens an auction without a positive
// denominator.
func (s *PgLiquidationStore) OpenInterest(ctx context.Context, instrumentID int64) (decimal.Decimal, error) {
	var txt string
	if err := s.Pool.QueryRow(ctx, `
		SELECT COALESCE(SUM(ABS(quantity) * COALESCE(mark_price, entry_price)), 0)::text
		FROM positions
		WHERE instrument_id = $1 AND quantity <> 0`, instrumentID).Scan(&txt); err != nil {
		return decimal.Zero, fmt.Errorf("open interest instr %d: %w", instrumentID, err)
	}
	d, err := decimal.NewFromString(txt)
	if err != nil {
		return decimal.Zero, fmt.Errorf("open interest instr %d parse %q: %w", instrumentID, txt, err)
	}
	return d, nil
}

// ---------------------------------------------------------------------------
// Margin-account reads/writes
// ---------------------------------------------------------------------------

// MarginMode implements LiquidationStore — the account's
// margin_accounts.margin_mode. A missing margin row is NOT an error: it
// resolves to the §13.1 category default exactly like
// MarginService.ModeFor (institutional → PORTFOLIO, else CROSS), so a
// liquidation never strands on an unmaterialized row. A missing account
// is a data defect and fails closed.
func (s *PgLiquidationStore) MarginMode(ctx context.Context, accountID int64) (string, error) {
	var mode string
	err := s.Pool.QueryRow(ctx, `
		SELECT margin_mode::text FROM margin_accounts WHERE account_id = $1`,
		accountID).Scan(&mode)
	if err == nil {
		m, ok := normalizeMode(mode)
		if !ok {
			return "", fmt.Errorf("margin mode acct %d: unknown mode %q", accountID, mode)
		}
		return string(m), nil
	}
	if err != pgx.ErrNoRows {
		return "", fmt.Errorf("margin mode acct %d: %w", accountID, err)
	}
	var cat string
	err = s.Pool.QueryRow(ctx, `
		SELECT client_category::text FROM accounts WHERE id = $1`, accountID).Scan(&cat)
	if err == pgx.ErrNoRows {
		return "", excerrors.New("ACCOUNT_NOT_FOUND",
			fmt.Sprintf("margin mode: account %d not found", accountID))
	}
	if err != nil {
		return "", fmt.Errorf("margin mode acct %d category: %w", accountID, err)
	}
	return string(DefaultMode(cat)), nil
}

// SetMarginAccountStatus implements LiquidationStore — mirrors the
// margin-call transition (NORMAL | MARGIN_CALL | LIQUIDATING). A
// missing margin_accounts row is a defect (the account was never
// evaluated): the update fails closed rather than fabricating a row
// with an invented margin_mode.
func (s *PgLiquidationStore) SetMarginAccountStatus(ctx context.Context, accountID int64, status string) error {
	tag, err := s.Pool.Exec(ctx, `
		UPDATE margin_accounts
		SET status = $2::margin_account_status_enum, updated_at = now()
		WHERE account_id = $1`, accountID, status)
	if err != nil {
		return fmt.Errorf("set margin status acct %d → %q: %w", accountID, status, err)
	}
	if tag.RowsAffected() == 0 {
		return excerrors.New("NOT_FOUND",
			fmt.Sprintf("margin account %d: no row to set status %q", accountID, status))
	}
	return nil
}

// AccountsForScan implements LiquidationStore — margin account ids in
// CROSS/PORTFOLIO mode, the §13.5 2s scanner's candidate set. ISOLATED
// rows are deliberately excluded: their positions liquidate
// independently through IsolatedBreaches.
func (s *PgLiquidationStore) AccountsForScan(ctx context.Context) ([]int64, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT account_id FROM margin_accounts
		WHERE margin_mode IN ('CROSS','PORTFOLIO')
		ORDER BY account_id`)
	if err != nil {
		return nil, fmt.Errorf("liquidation scan accounts: %w", err)
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("liquidation scan accounts row: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// liquidation_events (migration 230, spec §13.13)
// ---------------------------------------------------------------------------

// RecordLiquidationEvent implements LiquidationStore — persists one
// force-order record consumed by GET /api/v1/account/liquidations.
// Nullable references (auction, margin-call episode, ADL quintile,
// journal) carry through as NULLs.
func (s *PgLiquidationStore) RecordLiquidationEvent(ctx context.Context,
	ev LiquidationEventRow) (int64, error) {

	basis := ev.Basis
	if basis == "" {
		basis = LiquidationBasisMark // mig 236 — never silent
	}
	var id int64
	err := s.Pool.QueryRow(ctx, `
		INSERT INTO liquidation_events
		    (account_id, position_id, instrument_id, auction_id, margin_call_event_id,
		     kind, side, quantity, price, mark_price,
		     insurance_fund_contribution, penalty_amount, adl_quintile, journal_entry_id,
		     liquidation_basis)
		VALUES ($1, $2, $3, $4, $5, $6, $7::position_side_enum,
		        $8::numeric, $9::numeric, $10::numeric, $11::numeric, $12::numeric, $13, $14, $15)
		RETURNING id`,
		ev.AccountID, ev.PositionID, ev.InstrumentID, ev.AuctionID, ev.MarginCallEventID,
		ev.Kind, ev.Side, ev.Quantity.String(), ev.Price.String(), ev.MarkPrice.String(),
		ev.InsuranceFundContribution.String(), ev.PenaltyAmount.String(),
		ev.ADLQuintile, ev.JournalEntryID, basis).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("liquidation event acct %d pos %d: %w",
			ev.AccountID, ev.PositionID, err)
	}
	return id, nil
}

// MarkPositionClosed implements LiquidationStore — zeroes the position
// inside the caller's tx (the wallet settlement rides the same commit).
// quantity/unrealized_pnl/margin_used/isolated_margin_allocated clear to
// 0, the close price records as the final mark, and realized_pnl
// accumulates the passed leg P&L. Zero rows touched means the position
// was already closed or never existed — an error, never a silent pass:
// the tx must not commit a journal against a phantom close (§2.7).
func (s *PgLiquidationStore) MarkPositionClosed(ctx context.Context, tx pgx.Tx,
	positionID int64, closePrice, realizedPnl decimal.Decimal) error {

	tag, err := tx.Exec(ctx, `
		UPDATE positions
		SET quantity = 0,
		    unrealized_pnl = 0,
		    realized_pnl = realized_pnl + $2::numeric,
		    mark_price = $3::numeric,
		    liquidation_price = NULL,
		    margin_used = 0,
		    isolated_margin_allocated = 0,
		    auto_margin_replenish = FALSE,
		    updated_at = now()
		WHERE id = $1 AND quantity <> 0`,
		positionID, realizedPnl.String(), closePrice.String())
	if err != nil {
		return fmt.Errorf("mark position %d closed: %w", positionID, err)
	}
	if tag.RowsAffected() == 0 {
		return excerrors.New("NOT_FOUND",
			fmt.Sprintf("position %d: not open — already closed or missing", positionID))
	}
	return nil
}

// ---------------------------------------------------------------------------
// liquidation_auctions (migration 015, spec §5.14/§13.4)
// ---------------------------------------------------------------------------

// InsertAuction implements LiquidationStore — persists a fresh CALL row
// and returns its id. A non-positive avg_fill_price inserts NULL (no
// fills yet is honest, 0.00 is not); zero phase timestamps insert NULL;
// a zero CreatedAt defers to the column's now() default.
func (s *PgLiquidationStore) InsertAuction(ctx context.Context, a AuctionRow) (int64, error) {
	phase := a.Phase
	if phase == "" {
		phase = AuctionPhaseCall
	}
	var avg, floor, unfilled *string
	if a.AvgFillPrice.IsPositive() {
		v := a.AvgFillPrice.String()
		avg = &v
	}
	if a.FloorPrice.IsPositive() {
		v := a.FloorPrice.String()
		floor = &v
	}
	if a.UnfilledQty.IsPositive() {
		v := a.UnfilledQty.String()
		unfilled = &v
	}
	var start, end, created any
	if !a.PhaseStartAt.IsZero() {
		start = a.PhaseStartAt
	}
	if !a.PhaseEndAt.IsZero() {
		end = a.PhaseEndAt
	}
	if !a.CreatedAt.IsZero() {
		created = a.CreatedAt
	}
	var id int64
	err := s.Pool.QueryRow(ctx, `
		INSERT INTO liquidation_auctions
		    (instrument_id, position_id, phase, floor_price, unfilled_qty, filled_qty,
		     avg_fill_price, phase_start_at, phase_end_at, created_at)
		VALUES ($1, $2, $3::auction_phase_enum, $4::numeric, $5::numeric,
		        $6::numeric, $7::numeric, $8, $9, COALESCE($10::timestamptz, now()))
		RETURNING id`,
		a.InstrumentID, a.PositionID, phase, floor, unfilled,
		a.FilledQty.String(), avg, start, end, created).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("insert auction pos %d instr %d: %w",
			a.PositionID, a.InstrumentID, err)
	}
	return id, nil
}

// auctionCols is the shared liquidation_auctions projection — NULL
// economics read as 0 (never started / never filled).
const auctionCols = `
	id, instrument_id, position_id, phase::text,
	COALESCE(floor_price, 0)::text, COALESCE(unfilled_qty, 0)::text,
	COALESCE(filled_qty, 0)::text, COALESCE(avg_fill_price, 0)::text,
	phase_start_at, phase_end_at, created_at`

func scanAuctionRow(row pgx.Row) (*AuctionRow, error) {
	var (
		a                       AuctionRow
		floor, unfilled, filled string
		avg                     string
		start, end              *time.Time
	)
	if err := row.Scan(&a.ID, &a.InstrumentID, &a.PositionID, &a.Phase,
		&floor, &unfilled, &filled, &avg, &start, &end, &a.CreatedAt); err != nil {
		return nil, err
	}
	var err error
	if a.FloorPrice, err = decimal.NewFromString(floor); err != nil {
		return nil, fmt.Errorf("auction %d floor %q: %w", a.ID, floor, err)
	}
	if a.UnfilledQty, err = decimal.NewFromString(unfilled); err != nil {
		return nil, fmt.Errorf("auction %d unfilled %q: %w", a.ID, unfilled, err)
	}
	if a.FilledQty, err = decimal.NewFromString(filled); err != nil {
		return nil, fmt.Errorf("auction %d filled %q: %w", a.ID, filled, err)
	}
	if a.AvgFillPrice, err = decimal.NewFromString(avg); err != nil {
		return nil, fmt.Errorf("auction %d avg_fill_price %q: %w", a.ID, avg, err)
	}
	if start != nil {
		a.PhaseStartAt = *start
	}
	if end != nil {
		a.PhaseEndAt = *end
	}
	return &a, nil
}

// AuctionByID implements LiquidationStore — (nil, nil) when absent.
func (s *PgLiquidationStore) AuctionByID(ctx context.Context, id int64) (*AuctionRow, error) {
	a, err := scanAuctionRow(s.Pool.QueryRow(ctx,
		`SELECT `+auctionCols+` FROM liquidation_auctions WHERE id = $1`, id))
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("auction read %d: %w", id, err)
	}
	return a, nil
}

// UpdateAuctionPhase implements LiquidationStore — phase + floor +
// window edges in one write. A missing row is an error (the advancer
// must never think a phase persisted when it did not).
func (s *PgLiquidationStore) UpdateAuctionPhase(ctx context.Context, id int64,
	phase string, floor decimal.Decimal, start, end time.Time) error {

	tag, err := s.Pool.Exec(ctx, `
		UPDATE liquidation_auctions
		SET phase = $2::auction_phase_enum, floor_price = $3::numeric,
		    phase_start_at = $4, phase_end_at = $5
		WHERE id = $1`, id, phase, floor.String(), start, end)
	if err != nil {
		return fmt.Errorf("auction %d phase → %q: %w", id, phase, err)
	}
	if tag.RowsAffected() == 0 {
		return excerrors.New("NOT_FOUND", fmt.Sprintf("auction %d: no row for phase update", id))
	}
	return nil
}

// RecordAuctionFill implements LiquidationStore — the monotonic fill
// update the engine's idempotency contract relies on: the row only
// advances to a LARGER filled_qty (WHERE filled_qty <= new). A replay
// carrying an at-or-below state is a no-op, not an error; a truly
// missing row IS an error.
func (s *PgLiquidationStore) RecordAuctionFill(ctx context.Context, id int64,
	filledQty, avgPrice, unfilled decimal.Decimal) error {

	var avg *string
	if avgPrice.IsPositive() {
		v := avgPrice.String()
		avg = &v
	}
	tag, err := s.Pool.Exec(ctx, `
		UPDATE liquidation_auctions
		SET filled_qty = $2::numeric, avg_fill_price = $3::numeric, unfilled_qty = $4::numeric
		WHERE id = $1 AND COALESCE(filled_qty, 0) <= $2::numeric`,
		id, filledQty.String(), avg, unfilled.String())
	if err != nil {
		return fmt.Errorf("auction %d fill record: %w", id, err)
	}
	if tag.RowsAffected() > 0 {
		return nil
	}
	// Guard rejected the write — either a replayed/older fill state
	// (idempotent no-op) or a phantom auction id (error, fail closed).
	var exists bool
	if err := s.Pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM liquidation_auctions WHERE id = $1)`,
		id).Scan(&exists); err != nil {
		return fmt.Errorf("auction %d existence probe: %w", id, err)
	}
	if !exists {
		return excerrors.New("NOT_FOUND", fmt.Sprintf("auction %d: no row for fill record", id))
	}
	return nil
}

// ActiveAuctions implements LiquidationStore — the phase-advancer's
// per-tick input. Rows still holding an unfilled residual always
// qualify (a past-deadline FORCE_CASH residual must keep retrying —
// §13.11 item 2). A fully filled row also qualifies until its phase
// window lapses so a worker crash between the fill write and the
// engine's complete() still gets the terminal event + key retirement on
// the next tick. Rows past both bounds are terminal history, never
// rescanned.
func (s *PgLiquidationStore) ActiveAuctions(ctx context.Context) ([]AuctionRow, error) {
	rows, err := s.Pool.Query(ctx, `SELECT `+auctionCols+`
		FROM liquidation_auctions
		WHERE phase <> 'PARKED'
		  AND (unfilled_qty > 0
		   OR phase_end_at > now())
		ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("active auctions: %w", err)
	}
	defer rows.Close()
	var out []AuctionRow
	for rows.Next() {
		a, err := scanAuctionRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *a)
	}
	return out, rows.Err()
}
