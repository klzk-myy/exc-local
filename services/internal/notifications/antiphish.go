package notifications

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PgAntiPhish binds AntiPhishLookup to users.anti_phishing_code — the
// column owned by the sibling Phase-12 task (spec §5.16, 4–32 chars).
//
// Seam ruling: while that migration is unlanded the column does not
// exist; PostgreSQL then reports SQLSTATE 42703 (undefined_column),
// which PgAntiPhish maps to ("", nil) — the unset-code branch — so
// pre-landed schema still delivers with the "set your anti-phishing
// code" banner instead of dead-lettering every outbound email. Any
// other error propagates (the dispatcher logs + renders the banner;
// it never blocks a send on a banner lookup).
type PgAntiPhish struct {
	Pool *pgxpool.Pool
}

// Code implements AntiPhishLookup.
func (p PgAntiPhish) Code(ctx context.Context, userID int64) (string, error) {
	if p.Pool == nil {
		return "", nil
	}
	var code *string
	err := p.Pool.QueryRow(ctx,
		`SELECT anti_phishing_code FROM users WHERE id = $1`, userID).Scan(&code)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "42703" {
			return "", nil // column not landed yet — treat as unset
		}
		return "", fmt.Errorf("notifications: anti-phish lookup: %w", err)
	}
	if code == nil {
		return "", nil
	}
	return *code, nil
}
