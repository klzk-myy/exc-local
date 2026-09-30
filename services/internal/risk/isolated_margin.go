// isolated_margin.go — Phase-19 Task 19.3.27: isolated margin
// sub-allocation & position-level collateral isolation (spec §5.6a,
// §13.1a, §24 #411; migration 106 positions.isolated_margin_allocated /
// auto_margin_replenish).
//
// THE CONTRACT:
//
//   - Pre-trade/open: the position's required initial margin moves
//     balances.available → balances.locked AND is earmarked on
//     positions.isolated_margin_allocated (atomic; the generated total
//     column stays invariant — the earmark lives inside the account's
//     total).
//   - Level: IsolatedMarginLevel = (allocated + unrealized P&L) /
//     maintenance_margin_required × 100, all USD-normalized. The
//     required-margin leg is the SAME value MarginService computes for
//     the position (positions.margin_used, or qty×mark/leverage
//     fallback, §13.6e volatility-scaled) — the 50% breach therefore
//     matches ESMA's "equity < 50% of initial margin" formulation.
//   - Breach (level ≤ 50%): when auto_margin_replenish is set and the
//     base-currency available balance covers the FULL top-up back to
//     100%, the deficit transfers available → allocated and the
//     position survives (a partial transfer still moves — the funds
//     stay earmarked to the position that then closes — but cannot
//     avert dispatch). Otherwise a POSITION-SCOPED
//     ISOLATED_MARGIN_DEFICIT job enters the durable liquidation queue:
//     dedupKeyFor scopes the marker to the position, so the job can
//     neither suppress nor be suppressed by the account-level path —
//     and the account's other positions/balances are never touched.
//   - Liquidation boundary parity: on every allocation/replenish the
//     service recomputes positions.liquidation_price — the exact mark
//     at which the level hits breach — so the §13.5 scanner's
//     IsolatedBreaches sweep sees the identical trigger the event
//     engine computes. boundary = entry + (breach%×reqQ − allocatedQ) /
//     signedQty; it depends only on allocation & requirement, so event
//     writes suffice (no per-tick rewrite).
//
// Isolation guarantee: everything below operates on ONE position row
// and ONE currency balance row under FOR UPDATE — a deficit on
// position A can neither read nor consume position B's allocation nor
// the account's CROSS margin.
package risk

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"exchange/pkg/decimal"
	excerrors "exchange/pkg/errors"
)

// ErrInsufficientBalance is the typed signal the store returns when the
// available leg cannot cover an isolated allocation; the service maps
// it to MARGIN_INSUFFICIENT at the API boundary.
var ErrInsufficientBalance = errors.New("isolated margin: insufficient available balance")

// ErrPositionNotOpen signals the target position is closed or absent.
var ErrPositionNotOpen = errors.New("isolated margin: position not open")

// Canonical isolated-margin thresholds (Task 19.3.27): breach at ≤50%,
// auto-replenish restores to 100%.
var (
	IsolatedBreachPct  = decimal.RequireFromString("50")
	IsolatedRestorePct = decimal.NewFromInt(100)
)

// ---------------------------------------------------------------------------
// Pure math — exported for the engine and for tests
// ---------------------------------------------------------------------------

// IsolatedLevelPct computes (allocated + uPnL) / mmr × 100 in a common
// numeraire (all three inputs must share it — the engine passes USD).
// nil ⇒ mmr is not positive: the position's requirement is
// unevaluable, a data defect callers must not silently pass.
func IsolatedLevelPct(allocated, upnl, mmr decimal.Decimal) *decimal.Decimal {
	if !mmr.IsPositive() {
		return nil
	}
	l := allocated.Add(upnl).Div(mmr).Mul(decimal.NewFromInt(100))
	return &l
}

// IsolatedTopUpUSD is the deficit to restore the level to 100%
// (allocated + uPnL = mmr). Negative ⇒ already at/above restore —
// returns 0.
func IsolatedTopUpUSD(allocated, upnl, mmr decimal.Decimal) decimal.Decimal {
	need := mmr.Sub(allocated.Add(upnl))
	if need.IsNegative() {
		return decimal.Zero
	}
	return need
}

// IsolatedBoundaryMark solves the mark at which the isolated level hits
// breachPct: allocatedQ + signedQty×(p−entry) = breachPct%×reqQ. All
// quote-currency terms; the allocation converts into quote ccy via
// baseToQuote = baseRate/quoteRate. ok=false when the position's signed
// quantity is zero or the requirement is unevaluable.
func IsolatedBoundaryMark(pos MarginPosition, mark, baseToQuote decimal.Decimal,
	breachPct decimal.Decimal) (decimal.Decimal, bool) {

	signed := pos.Quantity.Abs()
	if pos.Side == "SHORT" {
		signed = signed.Neg()
	}
	if signed.IsZero() {
		return decimal.Zero, false
	}
	req := pos.MarginUsed // QUOTE ccy
	if !req.IsPositive() {
		lev := decimal.NewFromInt(pos.MaxLeverage)
		if pos.MaxLeverage <= 0 {
			lev = decimal.NewFromInt(1)
		}
		if !mark.IsPositive() {
			return decimal.Zero, false
		}
		req = pos.Quantity.Abs().Mul(mark).Div(lev)
	}
	if !req.IsPositive() {
		return decimal.Zero, false
	}
	allocatedQ := pos.IsolatedAllocated.Mul(baseToQuote)
	target := req.Mul(breachPct).Div(decimal.NewFromInt(100))
	p := pos.EntryPrice.Add(target.Sub(allocatedQ).Div(signed))
	return p, true
}

// ---------------------------------------------------------------------------
// Store seam + PG implementation
// ---------------------------------------------------------------------------

// IsolatedMarginStore is the persistence seam for allocation, replenish
// and boundary maintenance. Implementations must keep the
// balance-debit / allocation-credit pair ATOMIC per call.
type IsolatedMarginStore interface {
	// PositionLeg reads one open position joined to its instrument and
	// its owning account id (nil, 0, nil when flat/absent).
	PositionLeg(ctx context.Context, positionID int64) (*MarginPosition, int64, error)
	// MarginModeFor resolves the account's margin mode (the Allocate
	// guard rejects earmarking on non-ISOLATED accounts).
	MarginModeFor(ctx context.Context, accountID int64) (MarginMode, error)
	// BaseCurrency resolves accounts.base_currency.
	BaseCurrency(ctx context.Context, accountID int64) (string, error)
	// Available returns the account's available balance in ccy.
	Available(ctx context.Context, accountID int64, ccy string) (decimal.Decimal, error)
	// Allocate atomically moves amount: balances.available →
	// balances.locked and credits positions.isolated_margin_allocated,
	// and records auto_margin_replenish. ErrInsufficientBalance when
	// the available leg is short; ErrPositionNotOpen when the position
	// is flat.
	Allocate(ctx context.Context, accountID, positionID int64, ccy string,
		amount decimal.Decimal, autoReplenish bool) error
	// Replenish moves min(amount, available) into the position's
	// allocated leg and returns the amount actually moved. 0 + nil is
	// an honest "nothing available".
	Replenish(ctx context.Context, accountID, positionID int64, ccy string,
		amount decimal.Decimal) (decimal.Decimal, error)
	// SetLiquidationBoundary maintains positions.liquidation_price so
	// the §13.5 scanner's IsolatedBreaches sees the same breach the
	// engine computes.
	SetLiquidationBoundary(ctx context.Context, positionID int64,
		price decimal.Decimal) error
}

// PgIsolatedMarginStore implements IsolatedMarginStore over pgx. All
// money columns scan/move as ::text decimals — never float64.
type PgIsolatedMarginStore struct {
	Pool *pgxpool.Pool
}

var _ IsolatedMarginStore = (*PgIsolatedMarginStore)(nil)

// NewPgIsolatedMarginStore binds the pool (nil rejected fail-closed).
func NewPgIsolatedMarginStore(pool *pgxpool.Pool) (*PgIsolatedMarginStore, error) {
	if pool == nil {
		return nil, fmt.Errorf("isolated margin store: nil pgx pool")
	}
	return &PgIsolatedMarginStore{Pool: pool}, nil
}

// PositionLeg implements IsolatedMarginStore — the same projection the
// margin evaluator uses, narrowed to one open position.
func (s *PgIsolatedMarginStore) PositionLeg(ctx context.Context, positionID int64) (*MarginPosition, int64, error) {
	var (
		p                      MarginPosition
		qty, entry, mused, iso string
		mark                   *string
		accountID              int64
	)
	err := s.Pool.QueryRow(ctx, `
		SELECT p.id, p.instrument_id, i.symbol, i.base_currency, i.quote_currency,
		       COALESCE(i.max_leverage, 0),
		       p.side::text, p.quantity::text, p.entry_price::text,
		       p.mark_price::text, p.margin_used::text,
		       COALESCE(p.isolated_margin_allocated, 0)::text,
		       COALESCE(p.auto_margin_replenish, false), p.account_id
		FROM positions p
		JOIN instruments i ON i.id = p.instrument_id
		WHERE p.id = $1 AND p.quantity <> 0`, positionID).
		Scan(&p.ID, &p.InstrumentID, &p.Symbol, &p.BaseCurrency, &p.QuoteCurrency,
			&p.MaxLeverage, &p.Side, &qty, &entry, &mark, &mused, &iso,
			&p.AutoReplenish, &accountID)
	if err == pgx.ErrNoRows {
		return nil, 0, nil
	}
	if err != nil {
		return nil, 0, fmt.Errorf("isolated position leg %d: %w", positionID, err)
	}
	var perr error
	if p.Quantity, perr = decimal.NewFromString(qty); perr != nil {
		return nil, 0, fmt.Errorf("position %d quantity: %w", positionID, perr)
	}
	if p.EntryPrice, perr = decimal.NewFromString(entry); perr != nil {
		return nil, 0, fmt.Errorf("position %d entry: %w", positionID, perr)
	}
	if p.MarginUsed, perr = decimal.NewFromString(mused); perr != nil {
		return nil, 0, fmt.Errorf("position %d margin_used: %w", positionID, perr)
	}
	if p.IsolatedAllocated, perr = decimal.NewFromString(iso); perr != nil {
		return nil, 0, fmt.Errorf("position %d isolated_allocated: %w", positionID, perr)
	}
	if mark != nil && *mark != "" {
		m, merr := decimal.NewFromString(*mark)
		if merr != nil {
			return nil, 0, fmt.Errorf("position %d mark: %w", positionID, merr)
		}
		p.StoredMark = &m
	}
	return &p, accountID, nil
}

// MarginModeFor implements IsolatedMarginStore — the §13.1 category
// default applies when no margin_accounts row exists.
func (s *PgIsolatedMarginStore) MarginModeFor(ctx context.Context, accountID int64) (MarginMode, error) {
	var mode string
	err := s.Pool.QueryRow(ctx,
		`SELECT margin_mode::text FROM margin_accounts WHERE account_id=$1`, accountID).Scan(&mode)
	if err == nil {
		m, ok := normalizeMode(mode)
		if !ok {
			return "", fmt.Errorf("margin mode acct %d: unknown %q", accountID, mode)
		}
		return m, nil
	}
	if err != pgx.ErrNoRows {
		return "", fmt.Errorf("margin mode acct %d: %w", accountID, err)
	}
	var cat string
	if err := s.Pool.QueryRow(ctx,
		`SELECT client_category::text FROM accounts WHERE id=$1`, accountID).Scan(&cat); err != nil {
		return "", fmt.Errorf("margin mode acct %d category: %w", accountID, err)
	}
	return DefaultMode(cat), nil
}

// BaseCurrency implements IsolatedMarginStore.
func (s *PgIsolatedMarginStore) BaseCurrency(ctx context.Context, accountID int64) (string, error) {
	var ccy string
	err := s.Pool.QueryRow(ctx,
		`SELECT base_currency FROM accounts WHERE id=$1`, accountID).Scan(&ccy)
	if err == pgx.ErrNoRows {
		return "", fmt.Errorf("account %d not found", accountID)
	}
	if err != nil {
		return "", fmt.Errorf("base currency acct %d: %w", accountID, err)
	}
	return ccy, nil
}

// Available implements IsolatedMarginStore — 0 (not error) when no row.
func (s *PgIsolatedMarginStore) Available(ctx context.Context, accountID int64, ccy string) (decimal.Decimal, error) {
	var txt string
	err := s.Pool.QueryRow(ctx,
		`SELECT available::text FROM balances WHERE account_id=$1 AND currency=$2`,
		accountID, ccy).Scan(&txt)
	if err == pgx.ErrNoRows {
		return decimal.Zero, nil
	}
	if err != nil {
		return decimal.Zero, fmt.Errorf("available acct %d %s: %w", accountID, ccy, err)
	}
	return decimal.NewFromString(txt)
}

// Allocate implements IsolatedMarginStore — the pre-trade/open lock:
// available → locked on the balance row AND → isolated_margin_allocated
// on the position row, in one transaction. The balance row is locked
// FOR UPDATE so concurrent allocations serialize; the position must be
// open (quantity <> 0) when the earmark lands.
func (s *PgIsolatedMarginStore) Allocate(ctx context.Context, accountID, positionID int64,
	ccy string, amount decimal.Decimal, autoReplenish bool) error {

	if !amount.IsPositive() {
		return fmt.Errorf("isolated allocate: amount must be positive")
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	avail, err := s.lockBalance(ctx, tx, accountID, ccy)
	if err != nil {
		return err
	}
	if avail.LessThan(amount) {
		return ErrInsufficientBalance
	}
	if err := s.lockOpenPosition(ctx, tx, positionID); err != nil {
		return err
	}
	if err := s.moveToAllocated(ctx, tx, accountID, positionID, ccy, amount); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE positions SET auto_margin_replenish = $2, updated_at = now()
		WHERE id = $1`, positionID, autoReplenish); err != nil {
		return fmt.Errorf("isolated allocate flag pos %d: %w", positionID, err)
	}
	return tx.Commit(ctx)
}

// Replenish implements IsolatedMarginStore — auto-margin-replenish:
// moves min(amount, available) base-ccy into the position's allocation
// atomically; returns the amount actually moved (0 when the balance is
// dry — never fabricated).
func (s *PgIsolatedMarginStore) Replenish(ctx context.Context, accountID, positionID int64,
	ccy string, amount decimal.Decimal) (decimal.Decimal, error) {

	if !amount.IsPositive() {
		return decimal.Zero, fmt.Errorf("isolated replenish: amount must be positive")
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return decimal.Zero, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	avail, err := s.lockBalance(ctx, tx, accountID, ccy)
	if err != nil {
		return decimal.Zero, err
	}
	moved := amount
	if avail.LessThan(moved) {
		moved = avail
	}
	if !moved.IsPositive() {
		return decimal.Zero, tx.Commit(ctx) // honest zero — nothing to move
	}
	if err := s.lockOpenPosition(ctx, tx, positionID); err != nil {
		return decimal.Zero, err
	}
	if err := s.moveToAllocated(ctx, tx, accountID, positionID, ccy, moved); err != nil {
		return decimal.Zero, err
	}
	return moved, tx.Commit(ctx)
}

// lockBalance SELECTs the balance row FOR UPDATE; a missing row yields
// zero available (the caller decides insufficient).
func (s *PgIsolatedMarginStore) lockBalance(ctx context.Context, tx pgx.Tx,
	accountID int64, ccy string) (decimal.Decimal, error) {

	var txt string
	err := tx.QueryRow(ctx, `
		SELECT available::text FROM balances
		WHERE account_id=$1 AND currency=$2 FOR UPDATE`, accountID, ccy).Scan(&txt)
	if err == pgx.ErrNoRows {
		return decimal.Zero, nil
	}
	if err != nil {
		return decimal.Zero, fmt.Errorf("balance lock acct %d %s: %w", accountID, ccy, err)
	}
	return decimal.NewFromString(txt)
}

// lockOpenPosition serializes against the close path: the row must be
// open when the allocation lands.
func (s *PgIsolatedMarginStore) lockOpenPosition(ctx context.Context, tx pgx.Tx, positionID int64) error {
	var id int64
	err := tx.QueryRow(ctx,
		`SELECT id FROM positions WHERE id=$1 AND quantity<>0 FOR UPDATE`, positionID).Scan(&id)
	if err == pgx.ErrNoRows {
		return ErrPositionNotOpen
	}
	if err != nil {
		return fmt.Errorf("position lock %d: %w", positionID, err)
	}
	return nil
}

// moveToAllocated performs the available→locked + allocated credit.
func (s *PgIsolatedMarginStore) moveToAllocated(ctx context.Context, tx pgx.Tx,
	accountID, positionID int64, ccy string, amount decimal.Decimal) error {

	tag, err := tx.Exec(ctx, `
		UPDATE balances
		SET available = available - $3::numeric, locked = locked + $3::numeric
		WHERE account_id=$1 AND currency=$2`,
		accountID, ccy, amount.String())
	if err != nil {
		return fmt.Errorf("isolated margin move acct %d: %w", accountID, err)
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("isolated margin move acct %d %s: balance row missing", accountID, ccy)
	}
	tag, err = tx.Exec(ctx, `
		UPDATE positions
		SET isolated_margin_allocated = isolated_margin_allocated + $2::numeric,
		    updated_at = now()
		WHERE id = $1 AND quantity <> 0`, positionID, amount.String())
	if err != nil {
		return fmt.Errorf("isolated margin credit pos %d: %w", positionID, err)
	}
	if tag.RowsAffected() != 1 {
		return ErrPositionNotOpen
	}
	return nil
}

// SetLiquidationBoundary implements IsolatedMarginStore.
func (s *PgIsolatedMarginStore) SetLiquidationBoundary(ctx context.Context,
	positionID int64, price decimal.Decimal) error {

	tag, err := s.Pool.Exec(ctx, `
		UPDATE positions SET liquidation_price=$2::numeric, updated_at=now()
		WHERE id=$1 AND quantity<>0`, positionID, price.String())
	if err != nil {
		return fmt.Errorf("liquidation boundary pos %d: %w", positionID, err)
	}
	if tag.RowsAffected() != 1 {
		return ErrPositionNotOpen
	}
	return nil
}

// ---------------------------------------------------------------------------
// IsolatedMarginService — allocation lifecycle + breach handling
// ---------------------------------------------------------------------------

// IsolatedAllocateRequest is the pre-trade/open allocation call the
// order pipeline makes once the position row exists.
type IsolatedAllocateRequest struct {
	AccountID     int64
	PositionID    int64
	Currency      string          // account base currency (allocated leg denomination)
	Amount        decimal.Decimal // initial margin, base currency
	AutoReplenish bool
}

// IsolatedRateSource is the optional mark/conversion seam the service
// uses for boundary maintenance — production binds *MarginEngine.
type IsolatedRateSource interface {
	RateToUSD(ctx context.Context, ccy string) (decimal.Decimal, bool)
}

// IsolatedMarginDeps wires the service.
type IsolatedMarginDeps struct {
	Store      IsolatedMarginStore   // required
	Dispatcher LiquidationDispatcher // required — position-scoped breach jobs
	Rates      IsolatedRateSource    // optional — liquidation_price maintenance
	Alerter    OpsAlerter
	Now        func() time.Time
	Logf       func(format string, args ...any)
	// BreachPct / RestorePct override the 50/100 task defaults.
	BreachPct  *decimal.Decimal
	RestorePct *decimal.Decimal
}

// IsolatedMarginService is the Task-19.3.27 engine: allocation,
// per-position level checks, auto-replenish, and position-scoped
// liquidation dispatch.
type IsolatedMarginService struct {
	store   IsolatedMarginStore
	disp    LiquidationDispatcher
	rates   IsolatedRateSource
	alerter OpsAlerter
	now     func() time.Time
	logf    func(format string, args ...any)
	breach  decimal.Decimal
	restore decimal.Decimal
}

// NewIsolatedMarginService builds the service.
func NewIsolatedMarginService(d IsolatedMarginDeps) (*IsolatedMarginService, error) {
	if d.Store == nil {
		return nil, excerrors.New(CodeRiskLimitsInternal, "isolated margin: store is nil")
	}
	if d.Dispatcher == nil {
		return nil, excerrors.New(CodeRiskLimitsInternal, "isolated margin: dispatcher is nil")
	}
	s := &IsolatedMarginService{
		store: d.Store, disp: d.Dispatcher, rates: d.Rates,
		alerter: d.Alerter, now: d.Now, logf: d.Logf,
		breach: IsolatedBreachPct, restore: IsolatedRestorePct,
	}
	if d.BreachPct != nil {
		s.breach = *d.BreachPct
	}
	if d.RestorePct != nil {
		s.restore = *d.RestorePct
	}
	if s.now == nil {
		s.now = func() time.Time { return time.Now().UTC() }
	}
	if s.logf == nil {
		s.logf = func(string, ...any) {}
	}
	return s, nil
}

// AllocateInitialMargin implements the pre-trade lock: the position's
// required IM moves available → locked → isolated_margin_allocated.
// Non-ISOLATED accounts are rejected (an earmark on a CROSS/PORTFOLIO
// account is a corrupt allocation). Insufficient funds map to
// MARGIN_INSUFFICIENT (§23 400); after a successful move the
// liquidation boundary is maintained best-effort (a boundary-write
// failure pages P1 — the event engine remains the trigger — but does
// not fail the committed allocation).
func (s *IsolatedMarginService) AllocateInitialMargin(ctx context.Context,
	req IsolatedAllocateRequest) error {

	if req.AccountID <= 0 || req.PositionID <= 0 || req.Currency == "" || !req.Amount.IsPositive() {
		return excerrors.New(CodeInvalidRequest,
			"isolated allocate: account, position, currency and a positive amount are required")
	}
	mode, err := s.store.MarginModeFor(ctx, req.AccountID)
	if err != nil {
		return errCode(CodeRiskLimitsInternal, "isolated margin mode", err)
	}
	if mode != ModeIsolated {
		return excerrors.New(CodeInvalidRequest, fmt.Sprintf(
			"isolated allocate: account %d is in %s mode — sub-allocation requires ISOLATED",
			req.AccountID, mode))
	}
	if err := s.store.Allocate(ctx, req.AccountID, req.PositionID,
		req.Currency, req.Amount, req.AutoReplenish); err != nil {
		if errors.Is(err, ErrInsufficientBalance) {
			return excerrors.New(CodeMarginInsufficient, fmt.Sprintf(
				"isolated allocate: %s available cannot cover %s", req.Currency, req.Amount))
		}
		return errCode(CodeRiskLimitsInternal, "isolated allocate", err)
	}
	if err := s.maintainBoundary(ctx, req.AccountID, req.PositionID); err != nil {
		s.raiseAlert(ctx, SeverityP1, "ISOLATED_BOUNDARY_DEGRADED", fmt.Sprintf(
			"liquidation boundary write failed for position %d", req.PositionID), nil)
	}
	return nil
}

// Release note (documented wiring): position close releases the earmark
// via LiquidationStore.MarkPositionClosed, which zeroes
// isolated_margin_allocated inside the settlement transaction — no
// separate release call exists here so the lifecycle can't be double-
// implemented downstream.

// CheckAccount is the engine seam (isolatedAccountChecker): evaluate
// every position's isolated level from the fresh snapshot and handle
// each breach. rateToUSD is supplied by the caller — the engine's live
// rate function, i.e. the same marks that priced the snapshot.
func (s *IsolatedMarginService) CheckAccount(ctx context.Context, legs *accountLegs,
	snap *MarginSnapshot, rateToUSD func(string) (decimal.Decimal, bool)) error {

	if legs == nil || snap == nil {
		return nil
	}
	baseRate, ok := rateToUSD(legs.baseCcy)
	if !ok || !baseRate.IsPositive() {
		// Defensive: the canonical Evaluate already fails closed when a
		// required conversion is unpriceable — a snapshot exists, so a
		// missing base rate here means the rate map changed mid-pass.
		return fmt.Errorf("isolated check acct %d: no USD rate for base %s",
			snap.AccountID, legs.baseCcy)
	}
	evalByID := make(map[int64]PositionMarginEval, len(snap.Positions))
	for _, ev := range snap.Positions {
		evalByID[ev.PositionID] = ev
	}
	for _, pos := range legs.positions {
		ev, ok := evalByID[pos.ID]
		if !ok {
			s.logf("isolated check acct %d: position %d missing from snapshot eval",
				snap.AccountID, pos.ID)
			continue
		}
		mmrUSD, err := decimal.NewFromString(ev.MarginUSD)
		if err != nil {
			s.logf("isolated check: bad margin eval pos %d: %v", pos.ID, err)
			continue
		}
		upnlUSD, err := decimal.NewFromString(ev.UnrealizedUSD)
		if err != nil {
			s.logf("isolated check: bad upnl eval pos %d: %v", pos.ID, err)
			continue
		}
		allocatedUSD := pos.IsolatedAllocated.Mul(baseRate)
		level := IsolatedLevelPct(allocatedUSD, upnlUSD, mmrUSD)
		if level == nil {
			// A live position with no positive margin requirement is a
			// data defect — page, never silently pass (§2.7).
			s.raiseAlert(ctx, SeverityP1, "ISOLATED_MARGIN_UNDEF", fmt.Sprintf(
				"isolated position %d (acct %d) has no positive margin requirement",
				pos.ID, snap.AccountID), map[string]string{
				"position_id": fmt.Sprint(pos.ID),
				"account_id":  fmt.Sprint(snap.AccountID)})
			continue
		}
		if level.LessThanOrEqual(s.breach) {
			if err := s.HandleDeficit(ctx, snap.AccountID, legs, pos, *level,
				allocatedUSD, upnlUSD, mmrUSD, rateToUSD); err != nil {
				s.logf("isolated deficit handling acct %d pos %d: %v",
					snap.AccountID, pos.ID, err)
			}
		}
	}
	return nil
}

// HandleDeficit resolves one breached position: auto-replenish when the
// flag stands and the FULL top-up is transferable, else position-scoped
// liquidation dispatch. Exposed for direct (non-engine) drivers as well.
func (s *IsolatedMarginService) HandleDeficit(ctx context.Context, accountID int64,
	legs *accountLegs, pos MarginPosition, level, allocatedUSD, upnlUSD, mmrUSD decimal.Decimal,
	rateToUSD func(string) (decimal.Decimal, bool)) error {

	if pos.AutoReplenish {
		restored, rerr := s.tryReplenish(ctx, accountID, legs, pos,
			allocatedUSD, upnlUSD, mmrUSD, rateToUSD)
		if rerr != nil {
			s.logf("isolated replenish acct %d pos %d failed: %v — dispatching liquidation",
				accountID, pos.ID, rerr)
		} else if restored {
			s.logf("isolated replenish acct %d pos %d: level restored toward %s%%",
				accountID, pos.ID, s.restore)
			return nil
		}
	}
	return s.dispatchDeficit(ctx, accountID, pos, level)
}

// tryReplenish attempts the all-or-nothing restore: the moved funds
// must cover the full USD deficit in base currency. A partial move
// still moves (the funds remain earmarked to the position that then
// closes — they return through close settlement) but reports
// restored=false → the deficit dispatch follows.
func (s *IsolatedMarginService) tryReplenish(ctx context.Context, accountID int64,
	legs *accountLegs, pos MarginPosition, allocatedUSD, upnlUSD, mmrUSD decimal.Decimal,
	rateToUSD func(string) (decimal.Decimal, bool)) (bool, error) {

	needUSD := IsolatedTopUpUSD(allocatedUSD, upnlUSD, mmrUSD)
	if !needUSD.IsPositive() {
		return true, nil // restore level already met — nothing to move
	}
	baseRate, ok := rateToUSD(legs.baseCcy)
	if !ok || !baseRate.IsPositive() {
		return false, fmt.Errorf("no USD rate for base %s", legs.baseCcy)
	}
	needBase := needUSD.Div(baseRate)

	// Cheap pre-check on the cached balance (the tx re-verifies under
	// FOR UPDATE — the database is the authority).
	avail := decimal.Zero
	for _, b := range legs.balances {
		if b.Currency == legs.baseCcy {
			avail = b.Available
		}
	}
	if !avail.IsPositive() {
		return false, nil // nothing to move → straight to liquidation
	}
	moved, err := s.store.Replenish(ctx, accountID, pos.ID, legs.baseCcy, needBase)
	if err != nil {
		return false, err
	}
	if moved.LessThan(needBase) {
		return false, nil // partial coverage — level still breached
	}
	if err := s.maintainBoundary(ctx, accountID, pos.ID); err != nil {
		s.raiseAlert(ctx, SeverityP1, "ISOLATED_BOUNDARY_DEGRADED", fmt.Sprintf(
			"liquidation boundary write failed for position %d", pos.ID), nil)
	}
	return true, nil
}

// dispatchDeficit enqueues the position-scoped ISOLATED_MARGIN_DEFICIT
// job. Queue failure pages P0 — an un-liquidatable deficit position is
// a blind spot.
func (s *IsolatedMarginService) dispatchDeficit(ctx context.Context, accountID int64,
	pos MarginPosition, level decimal.Decimal) error {

	enq, err := s.disp.Dispatch(ctx,
		IsolatedDeficitJob(accountID, pos.ID, pos.Symbol, &level, s.now()), nil)
	if err != nil {
		s.raiseAlert(ctx, "P0", "ISOLATED_DISPATCH_FAILED", fmt.Sprintf(
			"isolated deficit dispatch failed acct %d pos %d at %s%%",
			accountID, pos.ID, level), map[string]string{
			"account_id":  fmt.Sprint(accountID),
			"position_id": fmt.Sprint(pos.ID)})
		return err
	}
	if !enq {
		s.logf("isolated deficit acct %d pos %d: coalesced by queue dedup",
			accountID, pos.ID)
	}
	return nil
}

// maintainBoundary recomputes positions.liquidation_price — the exact
// mark at which the level reaches breach — so the §13.5 scanner's
// IsolatedBreaches sees the same trigger the engine computes.
func (s *IsolatedMarginService) maintainBoundary(ctx context.Context,
	accountID, positionID int64) error {

	if s.rates == nil {
		return nil // boundary maintenance disabled — engine remains the trigger
	}
	pos, owner, err := s.store.PositionLeg(ctx, positionID)
	if err != nil || pos == nil {
		return err
	}
	if owner != accountID {
		return fmt.Errorf("boundary pos %d: owner %d ≠ account %d", positionID, owner, accountID)
	}
	baseCcy, err := s.store.BaseCurrency(ctx, accountID)
	if err != nil {
		return err
	}
	baseRate, okB := s.rates.RateToUSD(ctx, baseCcy)
	quoteRate, okQ := s.rates.RateToUSD(ctx, pos.QuoteCurrency)
	if !okB || !okQ || !baseRate.IsPositive() || !quoteRate.IsPositive() {
		return fmt.Errorf("boundary pos %d: missing conversion rate", positionID)
	}
	mark := pos.EntryPrice
	if pos.StoredMark != nil && pos.StoredMark.IsPositive() {
		mark = *pos.StoredMark
	}
	p, ok := IsolatedBoundaryMark(*pos, mark, baseRate.Div(quoteRate), s.breach)
	if !ok || !p.IsPositive() {
		return nil // unsolvable/non-positive boundary — engine detection remains
	}
	return s.store.SetLiquidationBoundary(ctx, positionID, p)
}

func (s *IsolatedMarginService) raiseAlert(ctx context.Context, severity, code, summary string,
	details map[string]string) {
	if s.alerter == nil {
		s.logf("isolated margin: alert %s undeliverable (no alerter): %s", code, summary)
		return
	}
	actx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := s.alerter.Raise(actx, OpsAlert{
		Severity: severity, Code: code, Summary: summary, Details: details,
	}); err != nil {
		s.logf("isolated margin: alert %s dispatch failed: %v", code, err)
	}
}

// compile-time seam assertions.
var _ isolatedAccountChecker = (*IsolatedMarginService)(nil)
