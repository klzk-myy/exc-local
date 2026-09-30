package reporting

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ---------------------------------------------------------------------------
// PostgreSQL implementations — trade_confirmations (migration 049,
// sibling-owned; columns: confirmation_id BIGSERIAL, trade_id,
// account_id, version, status GENERATED|DELIVERED|ADJUSTED|FAILED,
// file_ref, content_sha256, generated_at, delivered_at, supersedes_id).
// This package only reads rows and stamps delivery — generation writes
// belong to analytics.ConfirmationService.
// ---------------------------------------------------------------------------

const confirmationCols = `confirmation_id, trade_id, account_id, version,
	status::text, file_ref, content_sha256, generated_at, delivered_at,
	supersedes_id`

// PgConfirmationTracker is the production ConfirmationTracker.
type PgConfirmationTracker struct {
	Pool *pgxpool.Pool
}

// NewPgConfirmationTracker wraps a pool.
func NewPgConfirmationTracker(pool *pgxpool.Pool) *PgConfirmationTracker {
	return &PgConfirmationTracker{Pool: pool}
}

func scanConfirmation(row pgx.Row) (*ConfirmationRecord, error) {
	var r ConfirmationRecord
	if err := row.Scan(&r.ConfirmationID, &r.TradeID, &r.AccountID,
		&r.Version, &r.Status, &r.FileRef, &r.ContentSHA256,
		&r.GeneratedAt, &r.DeliveredAt, &r.SupersedesID); err != nil {
		return nil, err
	}
	return &r, nil
}

// PendingGenerated implements ConfirmationTracker — GENERATED rows
// oldest-first (fresh adjusted issues also land GENERATED, so amended
// notes ride the same sweep).
func (s *PgConfirmationTracker) PendingGenerated(ctx context.Context, limit int) ([]ConfirmationRecord, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT `+confirmationCols+`
		FROM trade_confirmations
		WHERE status = 'GENERATED'
		ORDER BY generated_at
		LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("reporting: pending confirmations: %w", err)
	}
	defer rows.Close()
	out := []ConfirmationRecord{}
	for rows.Next() {
		var r ConfirmationRecord
		if err := rows.Scan(&r.ConfirmationID, &r.TradeID, &r.AccountID,
			&r.Version, &r.Status, &r.FileRef, &r.ContentSHA256,
			&r.GeneratedAt, &r.DeliveredAt, &r.SupersedesID); err != nil {
			return nil, fmt.Errorf("reporting: scan pending confirmation: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// MarkDelivered implements ConfirmationTracker — GENERATED→DELIVERED +
// delivered_at. A zero-rows update (already delivered/adjusted) is a
// data condition, surfaced as errNotFound so callers log rather than
// believe they delivered.
func (s *PgConfirmationTracker) MarkDelivered(ctx context.Context, confirmationID int64, at time.Time) error {
	tag, err := s.Pool.Exec(ctx, `
		UPDATE trade_confirmations
		SET status = 'DELIVERED', delivered_at = $2
		WHERE confirmation_id = $1 AND status = 'GENERATED'`,
		confirmationID, at)
	if err != nil {
		return fmt.Errorf("reporting: mark delivered: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return errNotFound(confirmationID)
	}
	return nil
}

// LatestByTrade implements ConfirmationTracker — newest version.
func (s *PgConfirmationTracker) LatestByTrade(ctx context.Context, tradeID int64) (*ConfirmationRecord, error) {
	row, err := scanConfirmation(s.Pool.QueryRow(ctx, `
		SELECT `+confirmationCols+`
		FROM trade_confirmations
		WHERE trade_id = $1
		ORDER BY version DESC
		LIMIT 1`, tradeID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reporting: confirmation by trade: %w", err)
	}
	return row, nil
}

// ---------------------------------------------------------------------------
// Lookup seams (PG impls)
// ---------------------------------------------------------------------------

// PgCategorySource resolves accounts.client_category (migration 042).
// A missing account resolves to RETAIL — the conservative timing.
type PgCategorySource struct {
	Pool *pgxpool.Pool
}

// NewPgCategorySource wraps a pool.
func NewPgCategorySource(pool *pgxpool.Pool) *PgCategorySource {
	return &PgCategorySource{Pool: pool}
}

// Category implements CategorySource.
func (s *PgCategorySource) Category(ctx context.Context, accountID int64) (string, error) {
	var cat string
	err := s.Pool.QueryRow(ctx,
		`SELECT client_category::text FROM accounts WHERE id = $1`,
		accountID).Scan(&cat)
	if errors.Is(err, pgx.ErrNoRows) {
		return "RETAIL", nil
	}
	if err != nil {
		return "", fmt.Errorf("reporting: client_category acct %d: %w", accountID, err)
	}
	return cat, nil
}

// PgRecipientSource resolves the account holder's email
// (accounts ⨝ users).
type PgRecipientSource struct {
	Pool *pgxpool.Pool
}

// NewPgRecipientSource wraps a pool.
func NewPgRecipientSource(pool *pgxpool.Pool) *PgRecipientSource {
	return &PgRecipientSource{Pool: pool}
}

// Email implements RecipientSource — empty string when the account or
// user row is absent (unroutable → channel skipped, never fabricated).
func (s *PgRecipientSource) Email(ctx context.Context, accountID int64) (string, error) {
	var email string
	err := s.Pool.QueryRow(ctx, `
		SELECT u.email
		FROM accounts a JOIN users u ON u.id = a.user_id
		WHERE a.id = $1`, accountID).Scan(&email)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("reporting: recipient email acct %d: %w", accountID, err)
	}
	return email, nil
}
