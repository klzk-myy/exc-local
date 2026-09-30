// Package demo implements Phase-08.5 Task 8.5.3.2 — the demo / paper
// trading environment (spec §24 #266; ESMA/FCA/ASIC practice-account
// mandate for retail FX).
//
// A deployment labelled "demo" (config.environment — the same env-label
// convention testenv.IsTestnet follows; serve as demo-api./demo-ws.
// {domain}) is the user-accessible sandbox: registration mints an
// account_type='DEMO' account seeded with virtual USD (default
// 100,000; EXC_DEMO_BALANCE_USD override) instead of the default SPOT
// account — same REST/WS/FIX surface, no KYC requirement, rate limits
// relaxed to 2x the Basic production tier (ratelimit.TierDemo).
//
// Isolation contract (fail-closed, spec §2.7):
//
//   - Virtual funds: the seed writes balances rows directly — the same
//     convention as the testnet faucet (testenv.Seed): no
//     funding_transactions row, no GL journal, and this package can
//     never reach internal/funding's rail adapters.
//   - No real banking: WrapFundingChecker decorates the shared
//     AssertMutable gate the deposit/withdrawal/transfer services call
//     before any money moves — DEMO accounts are rejected with the
//     registered FORBIDDEN code on every wired funding path, in every
//     environment (a stray DEMO row on production is still denied).
//   - The "demo" label is deliberately NOT in testenv's non-production
//     allowlist: the reset/seed/simulated-funding test endpoints stay
//     disabled on a user-facing demo deployment (fail closed).
//   - Expiry: demo_expires_at (migration 238) is the precomputed
//     30-day inactivity deadline, bumped by the auth login/refresh
//     path (TouchActivity) so the daily ExpireSweep is a pure indexed
//     read. Expiry closes the account through the same conventions as
//     the Task 14.3.9 closure pipeline: mass-cancel resting orders
//     first, then one transaction flips status → CLOSED and writes the
//     account_closures row + hash-chained admin_audit_log entry, then
//     sessions and API keys are revoked post-commit.
//
// Developer-portal note ("Sandbox / Demo Environment"): the demo
// deployment is the documented practice surface — virtual funds only,
// accounts expire after 30 days of inactivity, and no endpoint accepts
// real deposits or withdrawals from a DEMO account.
package demo

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"exchange/internal/admin"
	"exchange/pkg/decimal"
	excerrors "exchange/pkg/errors"
)

// EnvLabel is the deployment label that turns registration into demo
// provisioning — config.Environment verbatim ("demo").
const EnvLabel = "demo"

// DefaultLifetime is the Task 8.5.3.2 inactivity window.
const DefaultLifetime = 30 * 24 * time.Hour

// DefaultBalanceUSD is the Task 8.5.3.2 default virtual seed.
var DefaultBalanceUSD = decimal.NewFromInt(100_000)

// maxSeedUSD is a defence-in-depth bound on the virtual seed — the same
// safety cap the testnet faucet applies to simulated movements.
var maxSeedUSD = decimal.NewFromInt(1_000_000_000_000)

// ErrDisabled is returned when provisioning is attempted on a
// non-demo deployment — the sandbox must never mint DEMO rows on
// production (fail closed, spec §2.7).
var ErrDisabled = errors.New("demo: provisioning disabled — deployment is not env=demo")

// ExpiryReason is the account_closures.reason recorded by the sweep.
const ExpiryReason = "demo_inactivity_expiry"

// ---------------------------------------------------------------------------
// Seams (defined locally — demo never imports the funding/orders/accounts
// packages; the composition layer adapts them).
// ---------------------------------------------------------------------------

// MutableChecker mirrors funding.MutableChecker — the account status
// gate every money-movement service calls first.
type MutableChecker interface {
	AssertMutable(ctx context.Context, accountID int64) error
}

// OrderCanceller mass-cancels an account's resting orders — the
// composition layer binds the shared accounts.OrderDispatcher seam.
type OrderCanceller interface {
	MassCancelAccount(ctx context.Context, accountID int64, reason string) (cancelled int, err error)
}

// SessionTerminator mirrors accounts.SessionTerminator.
type SessionTerminator interface {
	RevokeAllExcept(ctx context.Context, accountID int64, userID string, keepSID string) (int, error)
}

// CredentialRevoker mirrors accounts.CredentialRevoker.
type CredentialRevoker interface {
	RevokeAllKeys(ctx context.Context, accountID int64, reason string) (int, error)
}

// ---------------------------------------------------------------------------
// Service
// ---------------------------------------------------------------------------

// Service is the demo-environment orchestrator: provisioning, the
// funding isolation gate, activity bumps, and the daily expiry sweep.
type Service struct {
	pool     *pgxpool.Pool
	env      string
	balance  decimal.Decimal
	lifetime time.Duration
	cancel   OrderCanceller
	sessions SessionTerminator
	keys     CredentialRevoker
	now      func() time.Time
	newID    func() (string, error)
	logf     func(format string, args ...any)
}

// New wires the service. env is config.Environment verbatim; anything
// other than "demo" leaves Enabled()=false and provisioning disabled
// (fail closed — an empty/unknown label is production behaviour, same
// as testenv).
func New(pool *pgxpool.Pool, env string) *Service {
	return &Service{
		pool:     pool,
		env:      strings.ToLower(strings.TrimSpace(env)),
		balance:  DefaultBalanceUSD,
		lifetime: DefaultLifetime,
		now:      time.Now,
		newID:    defaultClosureID,
		logf:     func(string, ...any) {},
	}
}

// Enabled reports whether this deployment is the demo environment —
// the sole gate for demo provisioning (registration consults it).
func (s *Service) Enabled() bool { return s.env == EnvLabel }

// WithInitialBalance overrides the virtual USD seed
// (EXC_DEMO_BALANCE_USD). Non-positive or above-cap values are rejected
// — a misconfigured seed must never mint absurd demo balances.
func (s *Service) WithInitialBalance(d decimal.Decimal) *Service {
	if d.IsPositive() && !d.GreaterThan(maxSeedUSD) {
		s.balance = d
	}
	return s
}

// WithLifetime overrides the inactivity window (tests); non-positive
// values keep the default.
func (s *Service) WithLifetime(d time.Duration) *Service {
	if d > 0 {
		s.lifetime = d
	}
	return s
}

// WithOrderCanceller attaches the resting-order cancel seam used by the
// expiry sweep (optional: a nil canceller skips the cancel step).
func (s *Service) WithOrderCanceller(c OrderCanceller) *Service {
	s.cancel = c
	return s
}

// WithSessionTerminator attaches session revocation for expired
// accounts (optional; nil = best-effort skipped step, logged).
func (s *Service) WithSessionTerminator(t SessionTerminator) *Service {
	s.sessions = t
	return s
}

// WithCredentialRevoker attaches API-key revocation for expired
// accounts (optional; nil = best-effort skipped step, logged).
func (s *Service) WithCredentialRevoker(r CredentialRevoker) *Service {
	s.keys = r
	return s
}

// WithLogger attaches the service logger for non-fatal observations.
func (s *Service) WithLogger(f func(format string, args ...any)) *Service {
	if f != nil {
		s.logf = f
	}
	return s
}

// SetClockForTest overrides the clock; tests only.
func (s *Service) SetClockForTest(now func() time.Time) { s.now = now }

// ---------------------------------------------------------------------------
// Provisioning — registration seam
// ---------------------------------------------------------------------------

// Provision creates a DEMO account for userID seeded with the virtual
// USD balance in its own transaction — the standalone provisioning call
// (tests, admin tooling). Registration uses ProvisionTx instead so the
// user + account + seed commit atomically.
func (s *Service) Provision(ctx context.Context, userID int64) (accountID int64, err error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return 0, excerrors.Wrap("INTERNAL_ERROR", "demo: provision tx begin", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	accountID, err = s.ProvisionTx(ctx, tx, userID)
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, excerrors.Wrap("INTERNAL_ERROR", "demo: provision commit", err)
	}
	return accountID, nil
}

// ProvisionTx inserts the DEMO account (with the 30-day inactivity
// deadline armed) and seeds the virtual USD wallet on tx — the
// auth.DemoProvisioner seam: the registration transaction owns commit,
// so a seed failure rolls the whole signup back (no user row without an
// account, spec §12.1 contract).
func (s *Service) ProvisionTx(ctx context.Context, tx pgx.Tx, userID int64) (int64, error) {
	if !s.Enabled() {
		return 0, ErrDisabled
	}
	expiresAt := s.now().UTC().Add(s.lifetime)
	var accountID int64
	if err := tx.QueryRow(ctx,
		`INSERT INTO accounts (user_id, account_type, demo_expires_at)
		 VALUES ($1,'DEMO',$2) RETURNING id`,
		userID, expiresAt).Scan(&accountID); err != nil {
		return 0, excerrors.Wrap("INTERNAL_ERROR", "demo: account insert", err)
	}
	// Virtual seed: direct balances upsert — the testenv.Seed precedent.
	// No funding_transactions row, no ledger journal: demo money is not
	// reconciliation-grade and must never enter the real-money trail.
	if _, err := tx.Exec(ctx,
		`INSERT INTO balances (account_id, currency, available, locked, version)
		 VALUES ($1,'USD',$2,0,1)
		 ON CONFLICT (account_id, currency)
		 DO UPDATE SET available=$2, locked=0, version=balances.version+1`,
		accountID, s.balance.String()); err != nil {
		return 0, excerrors.Wrap("INTERNAL_ERROR", "demo: seed balance", err)
	}
	return accountID, nil
}

// ---------------------------------------------------------------------------
// Activity — the 30-day deadline refresh
// ---------------------------------------------------------------------------

// TouchActivity re-arms demo_expires_at on a DEMO account — called by
// the auth login/refresh paths so "inactivity" covers every
// authenticated surface (not just fresh logins). Non-DEMO accounts are
// a silent no-op: the UPDATE predicate does the filtering so the hot
// auth path never pays a second read.
func (s *Service) TouchActivity(ctx context.Context, accountID int64) error {
	if accountID <= 0 {
		return nil
	}
	_, err := s.pool.Exec(ctx,
		`UPDATE accounts
		    SET demo_expires_at = $2, updated_at = now()
		  WHERE id = $1 AND account_type = 'DEMO' AND status <> 'CLOSED'`,
		accountID, s.now().UTC().Add(s.lifetime))
	if err != nil {
		return excerrors.Wrap("INTERNAL_ERROR", "demo: activity touch", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Funding isolation
// ---------------------------------------------------------------------------

// IsDemo reports whether accountID carries account_type='DEMO'. A
// missing account is NOT demo — the caller's own not-found path owns
// that rejection.
func (s *Service) IsDemo(ctx context.Context, accountID int64) (bool, error) {
	var isDemo bool
	err := s.pool.QueryRow(ctx,
		`SELECT account_type='DEMO' FROM accounts WHERE id=$1`,
		accountID).Scan(&isDemo)
	if err == pgx.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, excerrors.Wrap("INTERNAL_ERROR", "demo: account type read", err)
	}
	return isDemo, nil
}

// fundingChecker wraps the shared AssertMutable gate with the demo
// rejection — the inner status check runs first, then DEMO accounts
// are refused the registered FORBIDDEN code. Wired in place of the
// bare freeze gate into every funding-facing MutableChecker
// (deposits, withdrawals, transfers, chargebacks, PAMM/copy joins).
type fundingChecker struct {
	inner  MutableChecker
	isDemo func(ctx context.Context, accountID int64) (bool, error)
}

// WrapFundingChecker returns the MutableChecker the funding services
// are constructed with instead of the bare status gate. The demo check
// applies in every environment — a DEMO row on a non-demo deployment
// still cannot reach the rails (defence in depth).
func (s *Service) WrapFundingChecker(inner MutableChecker) MutableChecker {
	return fundingChecker{inner: inner, isDemo: s.IsDemo}
}

func (g fundingChecker) AssertMutable(ctx context.Context, accountID int64) error {
	if g.inner != nil {
		if err := g.inner.AssertMutable(ctx, accountID); err != nil {
			return err
		}
	}
	isDemo, err := g.isDemo(ctx, accountID)
	if err != nil {
		return err // lookup failure fails closed — never an unmetered pass
	}
	if isDemo {
		return excerrors.New("FORBIDDEN",
			"demo accounts cannot use real funding rails")
	}
	return nil
}

// ---------------------------------------------------------------------------
// Expiry sweep — daily UTC job
// ---------------------------------------------------------------------------

// expiredRow is one sweep candidate.
type expiredRow struct {
	AccountID int64
	UserID    int64
	ExpiresAt time.Time
}

// ExpireSweep closes every DEMO account whose inactivity deadline has
// lapsed. Each account gets the closure-convention treatment: optional
// mass-cancel of resting orders (cancel failure skips the account — a
// CLOSED account must never keep working orders), then one transaction
// flips status → CLOSED and writes the account_closures row plus the
// hash-chained admin_audit_log entry, then sessions/API keys are
// revoked best-effort post-commit. Idempotent: already-CLOSED rows are
// filtered by the feed predicate and re-verified under FOR UPDATE.
//
// Returns the count actually closed; per-account failures are joined
// into the returned error (the sweep continues — one bad account must
// not starve the rest of the batch).
func (s *Service) ExpireSweep(ctx context.Context, limit int) (int, error) {
	if limit <= 0 || limit > 5000 {
		limit = 500
	}
	now := s.now().UTC()
	// DEMO sub-accounts inherit the master's type (subaccounts.go) but
	// carry no demo_expires_at — the deadline falls back to created_at +
	// lifetime so an inherited DEMO row can never outlive the contract.
	// The partial index serves the common (non-NULL) arm of the predicate.
	rows, err := s.pool.Query(ctx,
		`SELECT id, user_id,
		        COALESCE(demo_expires_at, created_at + $3 * interval '1 second')
		   FROM accounts
		  WHERE account_type = 'DEMO'
		    AND status <> 'CLOSED'
		    AND COALESCE(demo_expires_at, created_at + $3 * interval '1 second') < $1
		  ORDER BY 3
		  LIMIT $2`, now, limit, s.lifetime.Seconds())
	if err != nil {
		return 0, excerrors.Wrap("INTERNAL_ERROR", "demo: expired feed", err)
	}
	var due []expiredRow
	for rows.Next() {
		var r expiredRow
		if err := rows.Scan(&r.AccountID, &r.UserID, &r.ExpiresAt); err != nil {
			rows.Close()
			return 0, excerrors.Wrap("INTERNAL_ERROR", "demo: expired scan", err)
		}
		due = append(due, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, excerrors.Wrap("INTERNAL_ERROR", "demo: expired feed", err)
	}

	closed := 0
	var errs []error
	for _, r := range due {
		if err := s.expireOne(ctx, r, now); err != nil {
			s.logf("demo: expire account %d failed: %v", r.AccountID, err)
			errs = append(errs, fmt.Errorf("account %d: %w", r.AccountID, err))
			continue
		}
		closed++
	}
	return closed, errors.Join(errs...)
}

// expireOne runs the single-account expiry.
func (s *Service) expireOne(ctx context.Context, r expiredRow, now time.Time) error {
	// Resting orders die first — a closed account must not keep working
	// orders on the book (same ordering as the Task 14.3.9 forced path).
	cancelled := 0
	if s.cancel != nil {
		n, err := s.cancel.MassCancelAccount(ctx, r.AccountID, "demo_expiry")
		if err != nil {
			return excerrors.Wrap("INTERNAL_ERROR",
				"demo: expiry mass-cancel", err)
		}
		cancelled = n
	}
	closureID, err := s.newID()
	if err != nil {
		return excerrors.Wrap("INTERNAL_ERROR", "demo: closure id", err)
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return excerrors.Wrap("INTERNAL_ERROR", "demo: expire tx begin", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var status string
	var curExpiry *time.Time
	if err := tx.QueryRow(ctx,
		`SELECT status::text, demo_expires_at FROM accounts WHERE id=$1 FOR UPDATE`,
		r.AccountID).Scan(&status, &curExpiry); err != nil {
		return excerrors.Wrap("INTERNAL_ERROR", "demo: expire lock", err)
	}
	if status == "CLOSED" {
		return nil // raced closure — nothing to do (idempotent)
	}
	// Re-verify under the row lock: a login/refresh racing between the
	// feed read and here re-arms the deadline — an active demo user must
	// never be closed out mid-session.
	if curExpiry != nil && curExpiry.After(now) {
		return nil
	}
	if _, err := tx.Exec(ctx,
		`UPDATE accounts SET status='CLOSED', updated_at=$2 WHERE id=$1`,
		r.AccountID, now); err != nil {
		return excerrors.Wrap("INTERNAL_ERROR", "demo: expire close", err)
	}
	snapshot, _ := json.Marshal(map[string]any{
		"reason":                ExpiryReason,
		"demo_expires_at":       r.ExpiresAt.UTC().Format(time.RFC3339Nano),
		"open_orders_cancelled": cancelled,
	})
	if _, err := tx.Exec(ctx,
		`INSERT INTO account_closures
		     (closure_id, account_id, user_id, reason, forced,
		      initiated_by, preconditions_snapshot, sweep_refs,
		      status, completed_at)
		 VALUES ($1,$2,$3,$4,true,$5,$6,'[]','COMPLETED',$7)`,
		closureID, r.AccountID, r.UserID, ExpiryReason, r.UserID,
		snapshot, now); err != nil {
		return excerrors.Wrap("INTERNAL_ERROR", "demo: closure record", err)
	}
	// Same audit convention as the Task 14.3.9 close path — a system
	// sweep has no admin actor, so the affected principal carries the
	// attribution (lifecycle.go convention for sweep actors).
	if _, _, err := admin.Log(ctx, tx, admin.AuditEntry{
		AdminUserID: r.UserID,
		Action:      "account.close",
		TargetType:  "account",
		TargetID:    &r.AccountID,
		BeforeState: map[string]any{"status": status, "account_type": "DEMO"},
		AfterState: map[string]any{
			"status":     "CLOSED",
			"reason":     ExpiryReason,
			"closure_id": closureID,
		},
	}); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return excerrors.Wrap("INTERNAL_ERROR", "demo: expire commit", err)
	}

	// Post-commit: the closed account loses its auth surface (same
	// convention as revokeAccess in the closure pipeline). Best-effort —
	// every mutating gate already fails closed on CLOSED; a revocation
	// failure is logged, never fatal to the sweep.
	uid := fmt.Sprint(r.UserID)
	if s.sessions != nil {
		if _, err := s.sessions.RevokeAllExcept(ctx, r.AccountID, uid, ""); err != nil {
			s.logf("demo: session revoke account %d: %v", r.AccountID, err)
		}
	}
	if s.keys != nil {
		if _, err := s.keys.RevokeAllKeys(ctx, r.AccountID, "account_closed"); err != nil {
			s.logf("demo: api-key revoke account %d: %v", r.AccountID, err)
		}
	}
	return nil
}

// defaultClosureID mints the "demoexp_<28urlsafe>" public id (the
// account_closures.closure_id VARCHAR(40) contract — same shape as the
// closure pipeline's aclose_ ids).
func defaultClosureID() (string, error) {
	raw := make([]byte, 21)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return "demoexp_" + base64.RawURLEncoding.EncodeToString(raw), nil
}
