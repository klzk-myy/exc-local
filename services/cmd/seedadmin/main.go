// Command seedadmin creates the master venue-admin account
// (admin@exc.local / Password123!) with a Super Admin binding.
//
// Idempotent: re-running is a no-op when the email already exists.
//
// Usage:
//
//	go run ./cmd/seedadmin
//	EXC_CONFIG=config.yaml go run ./cmd/seedadmin   # custom config
//
// Connection comes from the standard config loader
// (config.yaml or EXC_POSTGRES_DSN env).
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"golang.org/x/crypto/bcrypt"

	"exchange/internal/config"
	"exchange/internal/db"
	exchangeadmin "exchange/internal/admin"
)

const (
	adminEmail    = "admin@exc.local"
	adminPassword = "Password123!"
	adminCountry  = "US"
	adminFullName = "Master Admin"
)

func main() {
	if err := run(); err != nil {
		log.Fatalf("seedadmin: %v", err)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("config load: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	pool, err := db.NewPool(ctx, cfg.Postgres.DSN, cfg.Postgres.MaxConns)
	if err != nil {
		return fmt.Errorf("postgres connect: %w", err)
	}
	defer pool.Close()

	if err := pool.Ping(ctx); err != nil {
		return fmt.Errorf("postgres ping: %w", err)
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(adminPassword), bcrypt.DefaultCost)
	if err != nil {
		return fmt.Errorf("bcrypt: %w", err)
	}

	tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var userID int64
	err = tx.QueryRow(ctx, `
		INSERT INTO users (email, password_hash, country, full_name, status, kyc_status, email_verified_at)
		VALUES ($1, $2, $3, $4, 'ACTIVE', 'APPROVED', now())
		ON CONFLICT (email) DO UPDATE SET
			password_hash     = EXCLUDED.password_hash,
			status            = 'ACTIVE',
			kyc_status        = 'APPROVED',
			email_verified_at = COALESCE(users.email_verified_at, now()),
			updated_at        = now()
		RETURNING id`, adminEmail, string(hash), adminCountry, adminFullName).Scan(&userID)
	if err != nil {
		return fmt.Errorf("upsert user: %w", err)
	}

	// Ensure a SPOT account exists for the user.
	var accountID int64
	err = tx.QueryRow(ctx, `
		INSERT INTO accounts (user_id, account_type, fee_tier_id, status, kyc_tier)
		SELECT $1, 'SPOT', COALESCE(
		    (SELECT id FROM fee_tiers WHERE tier_name='STANDARD'),
		    (SELECT id FROM fee_tiers LIMIT 1)), 'ACTIVE', 'T2'
		WHERE NOT EXISTS (
		    SELECT 1 FROM accounts WHERE user_id = $1 AND parent_account_id IS NULL)
		RETURNING id`, userID).Scan(&accountID)
	if err != nil && err != pgx.ErrNoRows {
		return fmt.Errorf("ensure account: %w", err)
	}
	if accountID == 0 {
		if err := tx.QueryRow(ctx,
			`SELECT id FROM accounts WHERE user_id=$1 AND parent_account_id IS NULL ORDER BY id LIMIT 1`,
			userID).Scan(&accountID); err != nil {
			return fmt.Errorf("resolve account: %w", err)
		}
	}

	// Bind Super Admin (idempotent on user_id+role+status).
	// STANDARD Super Admin bindings are capped at granted_at + 90 days
	// (migration 090 arb_super_admin_cap; general cap 12 months).
	// 89 days leaves margin for granted_at (DB now()) vs Go clock skew.
	_, err = tx.Exec(ctx, `
		INSERT INTO admin_role_bindings
		    (user_id, role, kind, granter_id, expires_at, status)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT DO NOTHING`,
		userID, exchangeadmin.RoleSuperAdmin, exchangeadmin.KindStandard,
		userID, time.Now().UTC().Add(89*24*time.Hour), exchangeadmin.StatusActive)
	if err != nil {
		// Some schemas use a unique constraint rather than DO NOTHING —
		// ignore unique-violation as "already bound".
		if pgErr, ok := err.(*pgconn.PgError); ok && pgErr.Code == "23505" {
			// already present — fine
		} else {
			return fmt.Errorf("bind super admin: %w", err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}

	log.Printf("seedadmin: OK — user_id=%d account_id=%d email=%s role=Super Admin",
		userID, accountID, adminEmail)
	log.Printf("seedadmin: login at /api/v1/auth/login with email=%s password=%s",
		adminEmail, adminPassword)
	return nil
}

func init() {
	log.SetFlags(0)
	log.SetOutput(os.Stderr)
}
