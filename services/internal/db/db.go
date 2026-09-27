// Package db provides the PostgreSQL (pgx/v5) connection pool.
// Schema migrations live in internal/db/migrations (Task 1.3.3).
package db

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// NewPool parses dsn and opens a pgx connection pool. maxConns <= 0 keeps
// the pgx default. The pool is returned without pinging: services decide
// whether connectivity is fatal at their own startup boundary.
func NewPool(ctx context.Context, dsn string, maxConns int32) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("postgres dsn: %w", err)
	}
	if maxConns > 0 {
		cfg.MaxConns = maxConns
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("postgres connect: %w", err)
	}
	return pool, nil
}
