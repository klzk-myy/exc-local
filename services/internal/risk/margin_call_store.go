// margin_call_store.go — Postgres implementation of MarginCallStore
// (Phase-19 Task 19.3.3; tables margin_call_events / margin_accounts /
// account_margin_thresholds, migrations 013/230/235).
//
// Conventions mirror liquidation_store.go: NUMERIC columns projected as
// ::text into decimal.NewFromString, zero-row mutations are errors,
// every leg that must commit atomically runs inside the caller's tx.
package risk

import (
	"context"
	"fmt"
	"time"

	"exchange/pkg/decimal"
	excerrors "exchange/pkg/errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PgMarginCallStore persists the §13.3 margin-call lifecycle.
type PgMarginCallStore struct {
	Pool *pgxpool.Pool
}

// NewPgMarginCallStore binds the pool; nil is rejected fail-closed.
func NewPgMarginCallStore(pool *pgxpool.Pool) (*PgMarginCallStore, error) {
	if pool == nil {
		return nil, fmt.Errorf("margin call store: nil pgx pool")
	}
	return &PgMarginCallStore{Pool: pool}, nil
}

var _ MarginCallStore = (*PgMarginCallStore)(nil)

// AccountCategory resolves client_category + the nbp flag — both drive
// the tier threshold and the §13.6c restitution classification.
func (s *PgMarginCallStore) AccountCategory(ctx context.Context, accountID int64) (string, bool, error) {
	var category string
	var nbp bool
	err := s.Pool.QueryRow(ctx, `
		SELECT client_category::text, COALESCE(nbp, FALSE)
		  FROM accounts WHERE id = $1`, accountID).Scan(&category, &nbp)
	if err == pgx.ErrNoRows {
		return "", false, excerrors.New("ACCOUNT_NOT_FOUND",
			fmt.Sprintf("account %d: no row", accountID))
	}
	if err != nil {
		return "", false, fmt.Errorf("margin call store: category acct %d: %w", accountID, err)
	}
	return category, nbp, nil
}

// MarginThresholdOverride reads the per-account override row
// (migration 235); (nil, nil) when unset.
func (s *PgMarginCallStore) MarginThresholdOverride(ctx context.Context, accountID int64) (*MarginCallThresholds, error) {
	var mc, so string
	err := s.Pool.QueryRow(ctx, `
		SELECT margin_call_pct::text, stop_out_pct::text
		  FROM account_margin_thresholds WHERE account_id = $1`, accountID).
		Scan(&mc, &so)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("margin call store: thresholds acct %d: %w", accountID, err)
	}
	mcp, err := decimal.NewFromString(mc)
	if err != nil {
		return nil, fmt.Errorf("margin call store: margin_call_pct %q: %w", mc, err)
	}
	sop, err := decimal.NewFromString(so)
	if err != nil {
		return nil, fmt.Errorf("margin call store: stop_out_pct %q: %w", so, err)
	}
	return &MarginCallThresholds{MarginCallPct: mcp, StopOutPct: sop, Source: "OVERRIDE"}, nil
}

// scanMarginCallEvents maps rows to events — single projection shared
// by the single-row and list reads.
func scanMarginCallEvents(rows pgx.Rows) ([]MarginCallEvent, error) {
	defer rows.Close()
	var out []MarginCallEvent
	for rows.Next() {
		var ev MarginCallEvent
		var lvl, thr string
		if err := rows.Scan(&ev.ID, &ev.AccountID, &lvl, &thr, &ev.Status,
			&ev.NotifiedAt, &ev.ExpiresAt, &ev.ResolvedAt); err != nil {
			return nil, err
		}
		var err error
		if ev.MarginLevelPct, err = decimal.NewFromString(lvl); err != nil {
			return nil, fmt.Errorf("margin_call_events %d level %q: %w", ev.ID, lvl, err)
		}
		if ev.ThresholdPct, err = decimal.NewFromString(thr); err != nil {
			return nil, fmt.Errorf("margin_call_events %d threshold %q: %w", ev.ID, thr, err)
		}
		out = append(out, ev)
	}
	return out, rows.Err()
}

const marginCallCols = `id, account_id, margin_level_pct::text, threshold_pct::text,
	status, notified_at, expires_at, resolved_at`

// OpenMarginCall returns the OPEN episode or (nil, nil).
func (s *PgMarginCallStore) OpenMarginCall(ctx context.Context, accountID int64) (*MarginCallEvent, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT `+marginCallCols+`
		  FROM margin_call_events
		 WHERE account_id = $1 AND status = 'OPEN'`, accountID)
	if err != nil {
		return nil, fmt.Errorf("margin call store: open acct %d: %w", accountID, err)
	}
	evs, err := scanMarginCallEvents(rows)
	if err != nil {
		return nil, fmt.Errorf("margin call store: open acct %d: %w", accountID, err)
	}
	if len(evs) == 0 {
		return nil, nil
	}
	if len(evs) > 1 {
		// The partial unique index forbids this — treat as corruption.
		return nil, fmt.Errorf("margin call store: %d OPEN events acct %d", len(evs), accountID)
	}
	return &evs[0], nil
}

// InsertMarginCall persists the OPEN episode + the §13.3 audit row in
// the caller's transaction — the event and the audit trail commit or
// abort together.
func (s *PgMarginCallStore) InsertMarginCall(ctx context.Context, tx pgx.Tx, ev *MarginCallEvent) (int64, error) {
	var id int64
	err := tx.QueryRow(ctx, `
		INSERT INTO margin_call_events
			(account_id, margin_level_pct, threshold_pct, status, notified_at, expires_at)
		VALUES ($1, $2::decimal, $3::decimal, 'OPEN', $4, $5)
		RETURNING id`,
		ev.AccountID, ev.MarginLevelPct.String(), ev.ThresholdPct.String(),
		ev.NotifiedAt, ev.ExpiresAt).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("margin call store: insert acct %d: %w", ev.AccountID, err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO admin_audit_log (admin_user_id, action, target_type, target_id,
		                             before_state, after_state)
		VALUES (0, 'risk.margin_call.open', 'margin_call_event', $1, NULL,
		        jsonb_build_object('account_id', $2::bigint,
		                           'margin_level_pct', $3::text,
		                           'threshold_pct', $4::text,
		                           'expires_at', $5::text))`,
		id, ev.AccountID, ev.MarginLevelPct.String(), ev.ThresholdPct.String(),
		ev.ExpiresAt.Format(time.RFC3339Nano)); err != nil {
		return 0, fmt.Errorf("margin call store: audit insert: %w", err)
	}
	return id, nil
}

// ResolveMarginCall closes the OPEN episode with the outcome. A missing
// or already-resolved episode is an error — silently succeeding would
// mask a lifecycle divergence.
func (s *PgMarginCallStore) ResolveMarginCall(ctx context.Context, accountID int64, outcome string, at time.Time) error {
	tag, err := s.Pool.Exec(ctx, `
		UPDATE margin_call_events
		   SET status = $2, resolved_at = $3
		 WHERE account_id = $1 AND status = 'OPEN'`, accountID, outcome, at)
	if err != nil {
		return fmt.Errorf("margin call store: resolve acct %d: %w", accountID, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("margin call store: no OPEN episode acct %d", accountID)
	}
	return nil
}

// ExpiredOpenMarginCalls lists OPEN episodes past their deposit window.
func (s *PgMarginCallStore) ExpiredOpenMarginCalls(ctx context.Context, now time.Time) ([]MarginCallEvent, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT `+marginCallCols+`
		  FROM margin_call_events
		 WHERE status = 'OPEN' AND expires_at <= $1
		 ORDER BY id`, now)
	if err != nil {
		return nil, fmt.Errorf("margin call store: expired scan: %w", err)
	}
	evs, err := scanMarginCallEvents(rows)
	if err != nil {
		return nil, fmt.Errorf("margin call store: expired scan: %w", err)
	}
	return evs, nil
}

// SetMarginAccountStatus updates margin_accounts.status.
func (s *PgMarginCallStore) SetMarginAccountStatus(ctx context.Context, accountID int64, status string) error {
	tag, err := s.Pool.Exec(ctx, `
		UPDATE margin_accounts SET status = $2::margin_account_status_enum
		 WHERE account_id = $1`, accountID, status)
	if err != nil {
		return fmt.Errorf("margin call store: status acct %d: %w", accountID, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("margin call store: no margin_accounts row acct %d", accountID)
	}
	return nil
}

// SetMarginAccountStatusTx is the tx-scoped variant — the status flips
// with the lifecycle row in the same commit.
func (s *PgMarginCallStore) SetMarginAccountStatusTx(ctx context.Context, tx pgx.Tx, accountID int64, status string) error {
	tag, err := tx.Exec(ctx, `
		UPDATE margin_accounts SET status = $2::margin_account_status_enum
		 WHERE account_id = $1`, accountID, status)
	if err != nil {
		return fmt.Errorf("margin call store: status tx acct %d: %w", accountID, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("margin call store: no margin_accounts row acct %d (tx)", accountID)
	}
	return nil
}

// AuditMarginCallRelease records the dual-control manual release.
func (s *PgMarginCallStore) AuditMarginCallRelease(ctx context.Context, accountID int64, actor int64) error {
	if _, err := s.Pool.Exec(ctx, `
		INSERT INTO admin_audit_log (admin_user_id, action, target_type, target_id,
		                             before_state, after_state)
		VALUES ($1, 'risk.margin_call.release', 'margin_call_event', $2, NULL,
		        jsonb_build_object('released_by', $1::bigint, 'account_id', $2::bigint))`,
		actor, accountID); err != nil {
		return fmt.Errorf("margin call store: release audit acct %d: %w", accountID, err)
	}
	return nil
}

// AccountUserID resolves the notification target for the account.
func (s *PgMarginCallStore) AccountUserID(ctx context.Context, accountID int64) (int64, error) {
	var userID int64
	err := s.Pool.QueryRow(ctx, `SELECT user_id FROM accounts WHERE id = $1`, accountID).Scan(&userID)
	if err == pgx.ErrNoRows {
		return 0, excerrors.New("ACCOUNT_NOT_FOUND",
			fmt.Sprintf("account %d: no row", accountID))
	}
	if err != nil {
		return 0, fmt.Errorf("margin call store: user acct %d: %w", accountID, err)
	}
	return userID, nil
}
