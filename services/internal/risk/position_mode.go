// Phase-19 Task 19.3.15 — position netting vs hedging mode
// (spec §13.6e, §24 #229).
//
// accounts.position_mode (migration 233) selects the fill-application
// posture:
//
//		NETTING  — the ESMA retail default: an opposing fill consumes the
//		           open row, realizes P&L on the closed slice, and only the
//		           residual opens a new net position (one non-flat row per
//		           instrument).
//		HEDGING — the institutional posture: opposing fills open a leg on
//		           the OPPOSING side instead of consuming one — LONG and
//		           SHORT rows coexist (positions gains a (account,
//		           instrument, side) unique key). Reductions are targeted:
//	          a reduce_only fill consumes the opposing-side leg, a
//	          position_id fill consumes that leg specifically.
//
// Mode changes are guarded twice (spec §13.6e): only while the account
// holds zero open positions (an open book has incompatible
// bookkeeping), and RETAIL clients cannot enable HEDGING (ESMA retail
// netting mandate — PROFESSIONAL/ELIGIBLE_COUNTERPARTY may elect it).
package risk

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	excerrors "exchange/pkg/errors"
)

// Position mode values — position_mode_enum (migration 233).
const (
	ModeNetting = "NETTING"
	ModeHedging = "HEDGING"
)

// CodePositionModeBlocked is the spec §23 rejection for a mode toggle
// that open positions or the retail restriction cannot accept
// (409/L2 — registered in the Phase-05 registry).
const CodePositionModeBlocked = "POSITION_MODE_SWITCH_BLOCKED"

// PositionModeStore is the persistence seam (PgPositionModeStore).
type PositionModeStore interface {
	// PositionMode returns accounts.position_mode; "" = column absent or
	// NULL → caller reads NETTING (retail-safe default).
	PositionMode(ctx context.Context, accountID int64) (string, error)
	// ClientCategory returns accounts.client_category for the retail
	// guard; "" = uncategorized → treated as RETAIL (fail closed).
	ClientCategory(ctx context.Context, accountID int64) (string, error)
	// OpenPositionCount counts positions rows with quantity <> 0.
	OpenPositionCount(ctx context.Context, accountID int64) (int64, error)
	// SetPositionMode persists the mode and appends the audit row in
	// one transaction (before/after images recorded).
	SetPositionMode(ctx context.Context, accountID int64, mode, before string, userID int64) error
}

// PositionModeService owns the account posture read + guarded toggle.
type PositionModeService struct {
	store PositionModeStore
}

// NewPositionModeService wires the service.
func NewPositionModeService(store PositionModeStore) *PositionModeService {
	return &PositionModeService{store: store}
}

// Mode returns the account's posture — NETTING when unset (the enum
// column is NOT NULL DEFAULT 'NETTING' but pre-233 databases and test
// stores may report ""). A store error propagates (fail closed).
func (s *PositionModeService) Mode(ctx context.Context, accountID int64) (string, error) {
	m, err := s.store.PositionMode(ctx, accountID)
	if err != nil {
		return "", internalError("position mode", err)
	}
	if m == "" {
		return ModeNetting, nil
	}
	return m, nil
}

// PositionMode implements PositionModeSource (the exposure gate and
// LimitsService consume it).
func (s *PositionModeService) PositionMode(ctx context.Context, accountID int64) (string, error) {
	return s.Mode(ctx, accountID)
}

// SetMode applies a guarded toggle. Idempotent for same-mode requests.
// Rejects:
//   - invalid mode values              → INVALID_REQUEST
//   - RETAIL → HEDGING                 → PRODUCT_NOT_PERMITTED (ESMA
//     retail netting mandate)
//   - any open position (qty <> 0)     → POSITION_MODE_SWITCH_BLOCKED
func (s *PositionModeService) SetMode(ctx context.Context, accountID int64, requested string, userID int64) (string, error) {
	mode := strings.ToUpper(strings.TrimSpace(requested))
	if mode != ModeNetting && mode != ModeHedging {
		return "", excerrors.New(CodeLeverageInvalid,
			fmt.Sprintf("invalid position_mode %q — want NETTING|HEDGING", requested))
	}
	cur, err := s.Mode(ctx, accountID)
	if err != nil {
		return "", err
	}
	if mode == cur {
		return mode, nil // idempotent
	}
	cat, err := s.store.ClientCategory(ctx, accountID)
	if err != nil {
		return "", internalError("client category", err)
	}
	if mode == ModeHedging && cat != CategoryProfessional && cat != CategoryEligible {
		return "", excerrors.New("PRODUCT_NOT_PERMITTED",
			"position hedging is not available to retail accounts (ESMA netting mandate)")
	}
	open, err := s.store.OpenPositionCount(ctx, accountID)
	if err != nil {
		return "", internalError("open positions", err)
	}
	if open > 0 {
		return "", excerrors.New(CodePositionModeBlocked,
			fmt.Sprintf("%d open position(s) — close all positions before switching %s → %s",
				open, cur, mode))
	}
	if err := s.store.SetPositionMode(ctx, accountID, mode, cur, userID); err != nil {
		return "", internalError("set position mode", err)
	}
	return mode, nil
}

// ---------------------------------------------------------------------------
// PgPositionModeStore
// ---------------------------------------------------------------------------

// PgPositionModeStore implements PositionModeStore over pgx.
type PgPositionModeStore struct {
	pool *pgxpool.Pool
}

// NewPgPositionModeStore wraps pool.
func NewPgPositionModeStore(pool *pgxpool.Pool) *PgPositionModeStore {
	return &PgPositionModeStore{pool: pool}
}

func (s *PgPositionModeStore) PositionMode(ctx context.Context, accountID int64) (string, error) {
	var m *string
	err := s.pool.QueryRow(ctx,
		`SELECT position_mode::text FROM accounts WHERE id = $1`, accountID).Scan(&m)
	if errors.Is(err, pgx.ErrNoRows) {
		return ModeNetting, nil
	}
	if err != nil {
		return "", err
	}
	if m == nil {
		return ModeNetting, nil
	}
	return *m, nil
}

func (s *PgPositionModeStore) ClientCategory(ctx context.Context, accountID int64) (string, error) {
	var c *string
	err := s.pool.QueryRow(ctx,
		`SELECT client_category::text FROM accounts WHERE id = $1`, accountID).Scan(&c)
	if errors.Is(err, pgx.ErrNoRows) {
		return CategoryRetail, nil
	}
	if err != nil {
		return "", err
	}
	if c == nil || *c == "" {
		return CategoryRetail, nil
	}
	return *c, nil
}

func (s *PgPositionModeStore) OpenPositionCount(ctx context.Context, accountID int64) (int64, error) {
	var n int64
	err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM positions WHERE account_id = $1 AND quantity <> 0`,
		accountID).Scan(&n)
	return n, err
}

// SetPositionMode writes the mode and the audit row atomically — the
// §13.6e/§24 toggle-audit contract (before/after images, actor id).
func (s *PgPositionModeStore) SetPositionMode(ctx context.Context, accountID int64,
	mode, before string, userID int64) error {

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx,
		`UPDATE accounts SET position_mode = $2::position_mode_enum WHERE id = $1`,
		accountID, mode); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO admin_audit_log (admin_user_id, action, target_type, target_id,
		                             before_state, after_state)
		VALUES ($1, 'account.position_mode.update', 'account', $2,
		        $3::jsonb, $4::jsonb)`,
		userID, accountID,
		fmt.Sprintf(`{"position_mode":%q}`, before),
		fmt.Sprintf(`{"position_mode":%q}`, mode)); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
