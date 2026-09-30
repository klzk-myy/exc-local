// Task 3.3.2 — Position Management (Go).
//
// Tracks one NET position row per (account_id, instrument_id) — spec §5.13.
// On each trade fill the service updates quantity, VWAP entry_price and
// unrealized_pnl, books realized P&L on close (in the instrument's quote
// currency, §13.1), and enforces the per-account open-position ceiling
// (risk_limits.max_open_positions, migration 109 — fall back to
// DefaultMaxOpenPositions when unset).
//
// Mark price: Phase-19.5 owns the real PriceOracle (Redis
// mark_price:{symbol}, median of >=2 feeds). Until then the
// MarkPriceProvider seam is bound to LastTradeMarkPriceProvider (mark =
// last traded price). Swap the provider at construction — no other change
// needed when the oracle lands.
//
// Fail-closed (spec §2.7): invalid fills, duplicate application
// (position_fills PK) and limit breaches abort the SERIALIZABLE tx; nothing
// is silently coerced. A failed mark-price lookup does not abort the fill —
// the position is marked at the last trade price and the result is flagged
// MarkStale so callers/alerting see it explicitly.

package settlement

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"exchange/pkg/decimal"
	excerrors "exchange/pkg/errors"
)

// Error codes emitted by this file. Canonical registration (code + HTTP
// status) is Phase-05 Task 5.3.21's registry; names follow spec §23.
const (
	CodePositionLimitExceeded = "POSITION_LIMIT_EXCEEDED"  // HTTP 400, L2
	CodeInvalidFill           = "INVALID_FILL"             // HTTP 400, L2
	CodeMarkPriceUnavailable  = "PRICE_ORACLE_UNAVAILABLE" // HTTP 503, L1
	CodeReduceOnlyViolation   = "REDUCE_ONLY_VIOLATION"    // HTTP 400, L2
)

// Task 19.3.15 — accounts.position_mode values (position_mode_enum,
// migration 233). Mirrored locally because risk imports this package —
// importing back would cycle.
const (
	posModeNetting = "NETTING"
	posModeHedging = "HEDGING"
)

// Position side values (positions.side enum has only LONG/SHORT; FLAT is
// derived: quantity == 0).
const (
	PositionLong  = "LONG"
	PositionShort = "SHORT"
	PositionFlat  = "FLAT"
)

// FillSide is the direction of the account leg of a trade fill.
type FillSide string

const (
	FillBuy  FillSide = "BUY"
	FillSell FillSide = "SELL"
)

// DefaultMaxOpenPositions is the fail-closed ceiling used when neither an
// account-specific nor a global-default risk_limits row supplies
// max_open_positions. Placeholder business value pending Risk Manager
// calibration (spec §13.6 configures exposure limits; position-count is a
// service-level guardrail).
const DefaultMaxOpenPositions = 200

// Position mirrors the positions table (migration 014). P&L amounts are
// denominated in the instrument's QUOTE currency (spec §13.1/§16.4).
type Position struct {
	ID            int64
	AccountID     int64
	InstrumentID  int64
	Side          string // LONG | SHORT (row-side; flat rows keep last side)
	Quantity      decimal.Decimal
	EntryPrice    decimal.Decimal // VWAP of open lots
	MarkPrice     decimal.Decimal
	HasMark       bool // false until first mark is recorded (mark_price NULL)
	UnrealizedPnl decimal.Decimal
	RealizedPnl   decimal.Decimal // cumulative realized, quote currency
	UpdatedAt     time.Time
}

// EffectiveSide returns LONG/SHORT/FLAT — FLAT when Quantity == 0.
func (p *Position) EffectiveSide() string {
	if p == nil || p.Quantity.IsZero() {
		return PositionFlat
	}
	return p.Side
}

// signedQty returns the signed position: +qty LONG, -qty SHORT, 0 FLAT.
func (p *Position) signedQty() decimal.Decimal {
	if p.Quantity.IsZero() {
		return decimal.Zero
	}
	if p.Side == PositionShort {
		return p.Quantity.Neg()
	}
	return p.Quantity
}

// PositionFill is one account leg of an engine trade. The wire TradeFill
// carries order IDs only (wire.TradeFill); callers resolve account and
// instrument upstream (trades row / order cache) before invoking
// ProcessFill.
type PositionFill struct {
	TradeID      uint64
	AccountID    int64
	InstrumentID int64
	Side         FillSide
	Price        decimal.Decimal
	Quantity     decimal.Decimal
	// Task 19.3.15 (HEDGING bookkeeping):
	// ReduceOnly marks a closing fill — under HEDGING it consumes the
	// opposing-side leg and can never open or over-reduce one; under
	// NETTING it may not flip the position.
	ReduceOnly bool
	// PositionID targets one specific hedge leg (MT5-style close-by);
	// 0 = side-derived. The row must belong to the account+instrument
	// and sit on the opposing side of the fill.
	PositionID int64
}

// PositionUpdate is the outcome of applying one fill.
type PositionUpdate struct {
	Position         Position
	RealizedPnLDelta decimal.Decimal // quote-currency P&L this fill realized
	Action           string          // OPENED | INCREASED | REDUCED | CLOSED | REVERSED
	MarkStale        bool            // true when oracle lookup failed → last-trade mark used
	Duplicate        bool            // true when trade_id+account_id already applied
}

// MarkPriceProvider is the seam for the Phase-19.5 PriceOracle. Returns
// the mark price for instrumentID, or an error when no usable mark exists.
type MarkPriceProvider interface {
	MarkPrice(ctx context.Context, instrumentID int64) (decimal.Decimal, error)
}

// ErrNoMarkPrice is returned by providers with no mark for the instrument.
var ErrNoMarkPrice = errors.New("no mark price for instrument")

// LastTradeMarkPriceProvider is the placeholder mark source (mark = last
// trade price) until the Phase-19.5 oracle lands. ProcessFill calls
// Observe on every applied fill; anything that consumes engine trades may
// also Observe directly.
type LastTradeMarkPriceProvider struct {
	mu   sync.RWMutex
	last map[int64]decimal.Decimal
}

// NewLastTradeMarkPriceProvider returns an empty provider.
func NewLastTradeMarkPriceProvider() *LastTradeMarkPriceProvider {
	return &LastTradeMarkPriceProvider{last: map[int64]decimal.Decimal{}}
}

// Observe records price as the last trade price for instrumentID.
func (p *LastTradeMarkPriceProvider) Observe(instrumentID int64, price decimal.Decimal) {
	p.mu.Lock()
	p.last[instrumentID] = price
	p.mu.Unlock()
}

// MarkPrice returns the last observed trade price, or ErrNoMarkPrice when
// no trade has been observed for the instrument.
func (p *LastTradeMarkPriceProvider) MarkPrice(_ context.Context, instrumentID int64) (decimal.Decimal, error) {
	p.mu.RLock()
	v, ok := p.last[instrumentID]
	p.mu.RUnlock()
	if !ok {
		return decimal.Zero, ErrNoMarkPrice
	}
	return v, nil
}

// PositionStore is the persistence seam. The production implementation is
// PgxPositionStore; tests substitute an in-memory store. Implementations
// must run fn inside a SERIALIZABLE transaction and roll back on error.
type PositionStore interface {
	InTx(ctx context.Context, fn func(ctx context.Context, tx PositionTx) error) error
}

// PositionTx is the transactional view used inside PositionStore.InTx.
type PositionTx interface {
	// FillApplied reports whether (tradeID, accountID) was already applied.
	FillApplied(ctx context.Context, tradeID uint64, accountID int64) (bool, error)
	// RecordFill inserts the position_fills dedup/audit row.
	RecordFill(ctx context.Context, f PositionFill, realizedDelta decimal.Decimal) error
	// GetPositionForUpdate locks and returns the NETTING row: the open
	// (quantity <> 0) row when one exists, else the most recent row for
	// reuse — under NETTING there is at most one non-flat row per
	// (account, instrument) by service invariant.
	GetPositionForUpdate(ctx context.Context, accountID, instrumentID int64) (*Position, error)
	// GetSidePositionForUpdate locks the side-scoped row (HEDGING
	// bookkeeping — migration 233 keyed positions by
	// (account, instrument, side)).
	GetSidePositionForUpdate(ctx context.Context, accountID, instrumentID int64, side string) (*Position, error)
	// GetPositionByIDForUpdate locks one leg by primary key for
	// PositionID-targeted hedge reductions.
	GetPositionByIDForUpdate(ctx context.Context, positionID int64) (*Position, error)
	// UpsertPosition writes the position (insert or update by
	// (account_id, instrument_id, side)).
	UpsertPosition(ctx context.Context, p *Position) error
	// CountOpenPositions counts rows with quantity <> 0 for the account.
	CountOpenPositions(ctx context.Context, accountID int64) (int, error)
	// MaxOpenPositions resolves the account's position ceiling: account
	// row, else global-default row, else (0,false) meaning service default.
	MaxOpenPositions(ctx context.Context, accountID int64) (int, bool, error)
	// AccountPositionMode returns accounts.position_mode — 'NETTING' or
	// 'HEDGING' (migration 233; absent/''=>NETTING).
	AccountPositionMode(ctx context.Context, accountID int64) (string, error)
}

// PositionService applies trade fills to net positions.
type PositionService struct {
	store     PositionStore
	oracle    MarkPriceProvider           // nil → lastTrade only
	lastTrade *LastTradeMarkPriceProvider // always populated; fallback mark
	fallback  int                         // default open-position ceiling
}

// NewPositionService builds the service. oracle may be nil (placeholder
// last-trade marking); fallback <= 0 uses DefaultMaxOpenPositions.
func NewPositionService(store PositionStore, oracle MarkPriceProvider, fallback int) *PositionService {
	if fallback <= 0 {
		fallback = DefaultMaxOpenPositions
	}
	return &PositionService{
		store:     store,
		oracle:    oracle,
		lastTrade: NewLastTradeMarkPriceProvider(),
		fallback:  fallback,
	}
}

// LastTradeMarks exposes the placeholder provider so the trade consumer
// can also Observe prices for instruments this service did not fill (e.g.
// the other leg's perspective is identical — same price — so this is
// mainly for external mark seeds/tests).
func (s *PositionService) LastTradeMarks() *LastTradeMarkPriceProvider {
	return s.lastTrade
}

// ProcessTrade applies BOTH legs of an engine trade atomically: the buyer
// leg (FillBuy) and the seller leg (FillSell). Dedup is per leg so a
// partially applied trade completes on retry without double-applying.
func (s *PositionService) ProcessTrade(ctx context.Context, tradeID uint64,
	instrumentID int64, buyerAccountID, sellerAccountID int64,
	price, qty decimal.Decimal) (buy, sell *PositionUpdate, err error) {

	err = s.store.InTx(ctx, func(ctx context.Context, tx PositionTx) error {
		var err error
		buy, err = s.applyInTx(ctx, tx, PositionFill{
			TradeID: tradeID, AccountID: buyerAccountID, InstrumentID: instrumentID,
			Side: FillBuy, Price: price, Quantity: qty,
		})
		if err != nil {
			return err
		}
		sell, err = s.applyInTx(ctx, tx, PositionFill{
			TradeID: tradeID, AccountID: sellerAccountID, InstrumentID: instrumentID,
			Side: FillSell, Price: price, Quantity: qty,
		})
		return err
	})
	return buy, sell, err
}

// ProcessFill applies a single account leg of a trade fill.
func (s *PositionService) ProcessFill(ctx context.Context, f PositionFill) (*PositionUpdate, error) {
	var out *PositionUpdate
	err := s.store.InTx(ctx, func(ctx context.Context, tx PositionTx) error {
		var err error
		out, err = s.applyInTx(ctx, tx, f)
		return err
	})
	return out, err
}

func (s *PositionService) applyInTx(ctx context.Context, tx PositionTx, f PositionFill) (*PositionUpdate, error) {
	if f.TradeID == 0 || f.AccountID == 0 || f.InstrumentID == 0 {
		return nil, excerrors.New(CodeInvalidFill, "fill missing trade/account/instrument id")
	}
	if f.Side != FillBuy && f.Side != FillSell {
		return nil, excerrors.New(CodeInvalidFill, fmt.Sprintf("invalid fill side %q", f.Side))
	}
	if !f.Price.IsPositive() || !f.Quantity.IsPositive() {
		return nil, excerrors.New(CodeInvalidFill, "fill price and quantity must be > 0")
	}

	mode, err := tx.AccountPositionMode(ctx, f.AccountID)
	if err != nil {
		return nil, fmt.Errorf("position mode: %w", err)
	}
	switch mode {
	case "", posModeNetting:
		mode = posModeNetting // absent/pre-233 → retail-safe default
	case posModeHedging:
	default:
		return nil, excerrors.New(CodeInvalidFill,
			fmt.Sprintf("account %d has unrecognized position_mode %q", f.AccountID, mode))
	}

	done, err := tx.FillApplied(ctx, f.TradeID, f.AccountID)
	if err != nil {
		return nil, fmt.Errorf("position fill dedup check: %w", err)
	}
	if done {
		pos, err := s.reloadForDuplicate(ctx, tx, f, mode)
		if err != nil {
			return nil, fmt.Errorf("position reload for duplicate fill: %w", err)
		}
		upd := &PositionUpdate{Duplicate: true, Action: "DUPLICATE"}
		if pos != nil {
			upd.Position = *pos
		}
		return upd, nil
	}

	if mode == posModeHedging {
		return s.applyHedgeInTx(ctx, tx, f)
	}
	return s.applyNetInTx(ctx, tx, f)
}

// reloadForDuplicate resolves the row a replayed fill touched so the
// DUPLICATE ack reports the post-state rather than an arbitrary leg.
func (s *PositionService) reloadForDuplicate(ctx context.Context, tx PositionTx,
	f PositionFill, mode string) (*Position, error) {
	if mode == posModeHedging {
		switch {
		case f.PositionID != 0:
			return tx.GetPositionByIDForUpdate(ctx, f.PositionID)
		case f.ReduceOnly:
			return tx.GetSidePositionForUpdate(ctx, f.AccountID, f.InstrumentID, opposingOf(f.Side))
		default:
			return tx.GetSidePositionForUpdate(ctx, f.AccountID, f.InstrumentID, legSideOf(f.Side))
		}
	}
	return tx.GetPositionForUpdate(ctx, f.AccountID, f.InstrumentID)
}

// applyNetInTx is the NETTING path: one net row per (account,
// instrument); opposing fills reduce/close/flip it with realized P&L.
// reduce_only fills may only shrink the open row — a flip is rejected.
func (s *PositionService) applyNetInTx(ctx context.Context, tx PositionTx, f PositionFill) (*PositionUpdate, error) {
	pos, err := tx.GetPositionForUpdate(ctx, f.AccountID, f.InstrumentID)
	if err != nil {
		return nil, fmt.Errorf("position load: %w", err)
	}
	if pos == nil {
		pos = &Position{
			AccountID:    f.AccountID,
			InstrumentID: f.InstrumentID,
			Side:         PositionLong, // placeholder until apply() sets it
			Quantity:     decimal.Zero,
		}
	}

	if f.ReduceOnly {
		signed := pos.signedQty()
		fq := f.Quantity
		if f.Side == FillSell {
			fq = fq.Neg()
		}
		if signed.IsZero() {
			return nil, excerrors.New(CodeReduceOnlyViolation,
				fmt.Sprintf("reduce-only fill on flat position acct=%d instr=%d", f.AccountID, f.InstrumentID))
		}
		if signed.Sign() == fq.Sign() || fq.Abs().GreaterThan(signed.Abs()) {
			return nil, excerrors.New(CodeReduceOnlyViolation,
				fmt.Sprintf("reduce-only fill qty %s would open/flip net %s acct=%d instr=%d",
					f.Quantity, signed, f.AccountID, f.InstrumentID))
		}
	}

	// Position-count ceiling: only a fill that OPENS a new open position
	// (from nil/FLAT) can breach it. Reductions, closes and reversals keep
	// or lower the count.
	wasOpen := !pos.Quantity.IsZero()
	if !wasOpen {
		if err := s.assertPositionRoom(ctx, tx, f.AccountID); err != nil {
			return nil, err
		}
	}

	realized, action := applyFill(pos, f)
	return s.finishFill(ctx, tx, f, pos, realized, action)
}

// applyHedgeInTx is the HEDGING path: fill-side legs coexist. A plain
// fill opens/increases the SAME-side leg (BUY→LONG, SELL→SHORT);
// reduce_only or PositionID fills consume the OPPOSING-side leg and
// realize P&L — never flipping it (over-close is rejected).
func (s *PositionService) applyHedgeInTx(ctx context.Context, tx PositionTx, f PositionFill) (*PositionUpdate, error) {
	if f.ReduceOnly || f.PositionID != 0 {
		return s.applyHedgeReduce(ctx, tx, f)
	}
	legSide := legSideOf(f.Side)
	leg, err := tx.GetSidePositionForUpdate(ctx, f.AccountID, f.InstrumentID, legSide)
	if err != nil {
		return nil, fmt.Errorf("hedge leg load: %w", err)
	}
	if leg == nil {
		leg = &Position{
			AccountID:    f.AccountID,
			InstrumentID: f.InstrumentID,
			Side:         legSide,
			Quantity:     decimal.Zero,
		}
	}
	if leg.Quantity.IsZero() {
		if err := s.assertPositionRoom(ctx, tx, f.AccountID); err != nil {
			return nil, err
		}
	}
	realized, action := applyFill(leg, f) // signed math → OPENED/INCREASED
	return s.finishFill(ctx, tx, f, leg, realized, action)
}

// applyHedgeReduce consumes the opposing-side leg under HEDGING.
// PositionID targets one specific leg; reduce-only without it resolves
// the leg by side. Over-close is impossible — the engine's own reduce
// sizing should bound it, so exceeding qty is INVALID semantics
// (REDUCE_ONLY_VIOLATION), never a silent clamp.
func (s *PositionService) applyHedgeReduce(ctx context.Context, tx PositionTx, f PositionFill) (*PositionUpdate, error) {
	opposing := opposingOf(f.Side)
	var leg *Position
	var err error
	if f.PositionID != 0 {
		leg, err = tx.GetPositionByIDForUpdate(ctx, f.PositionID)
		if err != nil {
			return nil, fmt.Errorf("targeted hedge leg load: %w", err)
		}
		if leg == nil || leg.AccountID != f.AccountID || leg.InstrumentID != f.InstrumentID {
			return nil, excerrors.New(CodeReduceOnlyViolation,
				fmt.Sprintf("position %d not an open leg of acct=%d instr=%d",
					f.PositionID, f.AccountID, f.InstrumentID))
		}
		if leg.Side != opposing {
			return nil, excerrors.New(CodeReduceOnlyViolation,
				fmt.Sprintf("%s fill cannot close %s leg %d", f.Side, leg.Side, f.PositionID))
		}
	} else {
		leg, err = tx.GetSidePositionForUpdate(ctx, f.AccountID, f.InstrumentID, opposing)
		if err != nil {
			return nil, fmt.Errorf("opposing hedge leg load: %w", err)
		}
	}
	if leg == nil || leg.Quantity.IsZero() {
		return nil, excerrors.New(CodeReduceOnlyViolation,
			fmt.Sprintf("no open %s leg to reduce acct=%d instr=%d",
				opposing, f.AccountID, f.InstrumentID))
	}
	if f.Quantity.GreaterThan(leg.Quantity) {
		return nil, excerrors.New(CodeReduceOnlyViolation,
			fmt.Sprintf("reduce fill %s exceeds %s leg qty %s acct=%d instr=%d",
				f.Quantity, leg.Side, leg.Quantity, f.AccountID, f.InstrumentID))
	}

	closed := f.Quantity
	sign := int64(1)
	if leg.Side == PositionShort {
		sign = -1
	}
	realized := f.Price.Sub(leg.EntryPrice).Mul(closed).Mul(decimal.NewFromInt(sign))
	action := "REDUCED"
	if closed.Equal(leg.Quantity) {
		action = "CLOSED"
	}
	leg.Quantity = leg.Quantity.Sub(closed)
	return s.finishFill(ctx, tx, f, leg, realized, action)
}

// assertPositionRoom enforces the per-account open-position ceiling
// before a new leg/net position opens.
func (s *PositionService) assertPositionRoom(ctx context.Context, tx PositionTx, accountID int64) error {
	max, found, err := tx.MaxOpenPositions(ctx, accountID)
	if err != nil {
		return fmt.Errorf("position limit lookup: %w", err)
	}
	if !found || max <= 0 {
		max = s.fallback
	}
	open, err := tx.CountOpenPositions(ctx, accountID)
	if err != nil {
		return fmt.Errorf("position count: %w", err)
	}
	if open >= max {
		return excerrors.New(CodePositionLimitExceeded,
			fmt.Sprintf("account %d has %d open positions, limit %d", accountID, open, max))
	}
	return nil
}

// finishFill is the shared tail: mark, unrealized, persist, dedup row.
func (s *PositionService) finishFill(ctx context.Context, tx PositionTx,
	f PositionFill, pos *Position, realized decimal.Decimal, action string) (*PositionUpdate, error) {

	pos.RealizedPnl = pos.RealizedPnl.Add(realized)

	// Mark: oracle first (Phase-19.5), else last-trade placeholder.
	// lastTrade is observed with this fill, so the fallback is always fresh.
	s.lastTrade.Observe(f.InstrumentID, f.Price)
	mark, mErr := s.markPrice(ctx, f.InstrumentID)
	upd := &PositionUpdate{RealizedPnLDelta: realized, Action: action}
	if mErr != nil {
		upd.MarkStale = true
		if pos.HasMark {
			mark = pos.MarkPrice // keep last good mark
		} else {
			mark = f.Price
		}
	}
	pos.MarkPrice = mark
	pos.HasMark = true
	pos.UnrealizedPnl = unrealizedPnL(pos, mark)
	pos.UpdatedAt = time.Now().UTC()

	if err := tx.UpsertPosition(ctx, pos); err != nil {
		return nil, fmt.Errorf("position upsert: %w", err)
	}
	if err := tx.RecordFill(ctx, f, realized); err != nil {
		return nil, fmt.Errorf("position fill record: %w", err)
	}
	upd.Position = *pos
	return upd, nil
}

// legSideOf maps a fill direction to the hedge-leg side it opens:
// BUY opens/increases LONG, SELL opens/increases SHORT.
func legSideOf(side FillSide) string {
	if side == FillSell {
		return PositionShort
	}
	return PositionLong
}

// opposingOf maps a fill direction to the leg it reduces: SELL reduces
// LONG, BUY reduces SHORT.
func opposingOf(side FillSide) string {
	if side == FillSell {
		return PositionLong
	}
	return PositionShort
}

func (s *PositionService) markPrice(ctx context.Context, instrumentID int64) (decimal.Decimal, error) {
	if s.oracle != nil {
		if m, err := s.oracle.MarkPrice(ctx, instrumentID); err == nil {
			if !m.IsPositive() {
				return decimal.Zero, excerrors.New(CodeMarkPriceUnavailable, "oracle returned non-positive mark")
			}
			return m, nil
		} else {
			return decimal.Zero, err
		}
	}
	return s.lastTrade.MarkPrice(ctx, instrumentID)
}

// applyFill mutates pos in place: quantity, entry_price (VWAP) and side.
// Returns (realizedPnLDelta in quote currency, action).
func applyFill(pos *Position, f PositionFill) (decimal.Decimal, string) {
	signed := pos.signedQty()
	fq := f.Quantity
	if f.Side == FillSell {
		fq = fq.Neg()
	}
	newSigned := signed.Add(fq)

	switch {
	case signed.IsZero() || signed.Sign() == fq.Sign():
		// Open or increase: VWAP entry.
		total := signed.Abs().Add(fq.Abs())
		if signed.IsZero() {
			pos.EntryPrice = f.Price
		} else {
			pos.EntryPrice = signed.Abs().Mul(pos.EntryPrice).
				Add(fq.Abs().Mul(f.Price)).Div(total)
		}
		pos.Quantity = total
		pos.Side = sideOf(newSigned)
		if signed.IsZero() {
			return decimal.Zero, "OPENED"
		}
		return decimal.Zero, "INCREASED"

	case fq.Abs().LessThan(signed.Abs()):
		// Partial close: realized on the closed slice, entry unchanged.
		closed := fq.Abs()
		realized := f.Price.Sub(pos.EntryPrice).Mul(closed).Mul(decimal.NewFromInt(int64(signed.Sign())))
		pos.Quantity = newSigned.Abs()
		return realized, "REDUCED"

	default:
		// Full close (fq == -signed) or reversal (|fq| > |signed|).
		realized := f.Price.Sub(pos.EntryPrice).Mul(signed.Abs()).Mul(decimal.NewFromInt(int64(signed.Sign())))
		if newSigned.IsZero() {
			pos.Quantity = decimal.Zero // FLAT; side/entry retained for audit
			return realized, "CLOSED"
		}
		pos.Quantity = newSigned.Abs()
		pos.Side = sideOf(newSigned)
		pos.EntryPrice = f.Price // remainder opens at fill price
		return realized, "REVERSED"
	}
}

func sideOf(signed decimal.Decimal) string {
	if signed.IsNegative() {
		return PositionShort
	}
	return PositionLong
}

// unrealizedPnL computes mark-to-market P&L in quote currency:
// signedQty * (mark - entry).
func unrealizedPnL(pos *Position, mark decimal.Decimal) decimal.Decimal {
	return pos.signedQty().Mul(mark.Sub(pos.EntryPrice))
}

// ---------------------------------------------------------------------------
// PgxPositionStore — production PositionStore over pgxpool (PostgreSQL 16).
// ---------------------------------------------------------------------------

// PgxPositionStore implements PositionStore with SERIALIZABLE txs.
type PgxPositionStore struct {
	Pool *pgxpool.Pool
}

// NewPgxPositionStore wraps a pool.
func NewPgxPositionStore(pool *pgxpool.Pool) *PgxPositionStore {
	return &PgxPositionStore{Pool: pool}
}

// InTx runs fn inside a SERIALIZABLE transaction, rolling back on error.
func (s *PgxPositionStore) InTx(ctx context.Context, fn func(ctx context.Context, tx PositionTx) error) error {
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return fmt.Errorf("position tx begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := fn(ctx, pgxPositionTx{tx: tx}); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("position tx commit: %w", err)
	}
	return nil
}

type pgxPositionTx struct{ tx pgx.Tx }

func (t pgxPositionTx) FillApplied(ctx context.Context, tradeID uint64, accountID int64) (bool, error) {
	var n int
	err := t.tx.QueryRow(ctx,
		`SELECT count(*) FROM position_fills WHERE trade_id = $1 AND account_id = $2`,
		int64(tradeID), accountID).Scan(&n)
	return n > 0, err
}

func (t pgxPositionTx) RecordFill(ctx context.Context, f PositionFill, realizedDelta decimal.Decimal) error {
	_, err := t.tx.Exec(ctx,
		`INSERT INTO position_fills (trade_id, account_id, instrument_id, side, quantity, price, realized_pnl)
		 VALUES ($1,$2,$3,$4,$5,$6,$7)`,
		int64(f.TradeID), f.AccountID, f.InstrumentID, string(f.Side), f.Quantity, f.Price, realizedDelta)
	return err
}

// positionCols is the shared SELECT list for the FOR UPDATE reads.
const positionCols = `id, account_id, instrument_id, side, quantity, entry_price,
        mark_price, unrealized_pnl, realized_pnl, updated_at`

func scanPosition(row pgx.Row) (*Position, error) {
	var (
		p       Position
		mark    *decimal.Decimal
		updated time.Time
	)
	err := row.Scan(&p.ID, &p.AccountID, &p.InstrumentID, &p.Side, &p.Quantity,
		&p.EntryPrice, &mark, &p.UnrealizedPnl, &p.RealizedPnl, &updated)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if mark != nil {
		p.MarkPrice, p.HasMark = *mark, true
	}
	p.UpdatedAt = updated
	return &p, nil
}

// GetPositionForUpdate locks the NETTING row: the open (quantity <> 0)
// row when one exists, else the most recently touched row (a flat row
// is reused so NETTING keeps a single row per instrument). Under
// HEDGING callers use GetSidePositionForUpdate instead.
func (t pgxPositionTx) GetPositionForUpdate(ctx context.Context, accountID, instrumentID int64) (*Position, error) {
	return scanPosition(t.tx.QueryRow(ctx,
		`SELECT `+positionCols+`
		   FROM positions
		  WHERE account_id = $1 AND instrument_id = $2
		  ORDER BY (quantity <> 0) DESC, id DESC
		  LIMIT 1 FOR UPDATE`, accountID, instrumentID))
}

// GetSidePositionForUpdate locks the side-scoped leg (HEDGING
// bookkeeping under the (account, instrument, side) unique key).
func (t pgxPositionTx) GetSidePositionForUpdate(ctx context.Context, accountID, instrumentID int64, side string) (*Position, error) {
	return scanPosition(t.tx.QueryRow(ctx,
		`SELECT `+positionCols+`
		   FROM positions
		  WHERE account_id = $1 AND instrument_id = $2 AND side = $3
		  LIMIT 1 FOR UPDATE`, accountID, instrumentID, side))
}

// GetPositionByIDForUpdate locks one leg by primary key.
func (t pgxPositionTx) GetPositionByIDForUpdate(ctx context.Context, positionID int64) (*Position, error) {
	return scanPosition(t.tx.QueryRow(ctx,
		`SELECT `+positionCols+`
		   FROM positions WHERE id = $1 FOR UPDATE`, positionID))
}

func (t pgxPositionTx) UpsertPosition(ctx context.Context, p *Position) error {
	mark := any(nil)
	if p.HasMark {
		mark = p.MarkPrice
	}
	if p.ID == 0 {
		return t.tx.QueryRow(ctx,
			`INSERT INTO positions
			   (account_id, instrument_id, side, quantity, entry_price,
			    mark_price, unrealized_pnl, realized_pnl, updated_at)
			 VALUES ($1,$2,$3,$4,$5,$6,$7,$8, now())
			 RETURNING id`,
			p.AccountID, p.InstrumentID, p.Side, p.Quantity, p.EntryPrice,
			mark, p.UnrealizedPnl, p.RealizedPnl).Scan(&p.ID)
	}
	_, err := t.tx.Exec(ctx,
		`UPDATE positions SET side=$2, quantity=$3, entry_price=$4, mark_price=$5,
		        unrealized_pnl=$6, realized_pnl=$7, updated_at=now()
		  WHERE id=$1`,
		p.ID, p.Side, p.Quantity, p.EntryPrice, mark, p.UnrealizedPnl, p.RealizedPnl)
	return err
}

func (t pgxPositionTx) CountOpenPositions(ctx context.Context, accountID int64) (int, error) {
	var n int
	err := t.tx.QueryRow(ctx,
		`SELECT count(*) FROM positions WHERE account_id = $1 AND quantity <> 0`,
		accountID).Scan(&n)
	return n, err
}

// MaxOpenPositions resolves risk_limits.max_open_positions: the
// account-specific row wins, else the global default row (account_id IS
// NULL), else (0,false) → the service default applies.
func (t pgxPositionTx) MaxOpenPositions(ctx context.Context, accountID int64) (int, bool, error) {
	var v *int
	err := t.tx.QueryRow(ctx,
		`SELECT max_open_positions FROM risk_limits
		  WHERE account_id = $1 AND symbol IS NULL
		  ORDER BY id DESC LIMIT 1`, accountID).Scan(&v)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return 0, false, err
	}
	if v != nil {
		return *v, true, nil
	}
	v = nil
	err = t.tx.QueryRow(ctx,
		`SELECT max_open_positions FROM risk_limits
		  WHERE account_id IS NULL AND symbol IS NULL
		  ORDER BY id DESC LIMIT 1`).Scan(&v)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return 0, false, err
	}
	if v != nil {
		return *v, true, nil
	}
	return 0, false, nil
}

// AccountPositionMode reads accounts.position_mode (migration 233).
// Missing row → NETTING (fail closed to the retail posture).
func (t pgxPositionTx) AccountPositionMode(ctx context.Context, accountID int64) (string, error) {
	var m *string
	err := t.tx.QueryRow(ctx,
		`SELECT position_mode::text FROM accounts WHERE id = $1`,
		accountID).Scan(&m)
	if errors.Is(err, pgx.ErrNoRows) {
		return posModeNetting, nil
	}
	if err != nil {
		return "", err
	}
	if m == nil || *m == "" {
		return posModeNetting, nil
	}
	return *m, nil
}
