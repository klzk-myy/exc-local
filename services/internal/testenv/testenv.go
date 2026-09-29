// Package testenv implements Phase-05 Task 5.3.13 — the test-environment
// account reset endpoint.
//
// Contract:
//   - POST /api/v1/test/reset clears an account's orders, positions,
//     fills, journal/ledger rows and restores balances to a zero
//     baseline (spec §12.10: one click → deterministic clean slate);
//   - the endpoint exists ONLY in non-production deployments
//     (development / staging / test / sandbox); in production the
//     service reports Disabled and the handler must fail closed;
//   - resets are rate-limited to one per account per 5 minutes
//     (in-memory limiter keyed on account id — the cooldown is
//     per-instance, documented deviation: §12.10 does not pin the
//     limiter backend).
//
// Reset order respects the dependency map in spec §5: children are
// deleted before parents, and trades is never deleted wholesale — only
// rows where the account is buyer OR seller (each row deleted once via
// OR, not two passes).
package testenv

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"exchange/pkg/decimal"
)

// Service wraps the reset store + cooldown limiter.
type Service struct {
	pool    *pgxpool.Pool
	env     string
	limiter *Limiter
	now     func() time.Time
}

// ErrDisabled is returned when the reset is invoked under a production
// deployment — the handler maps it to FORBIDDEN.
var ErrDisabled = errors.New("testenv: reset disabled in production")

// ErrCooldown is returned when the account reset within the window.
var ErrCooldown = errors.New("testenv: reset rate limit — try again later")

// ErrInvalidPreset is returned when a seed request names an unknown preset.
var ErrInvalidPreset = errors.New("testenv: unknown balance preset")

// ErrInvalidAmount is returned when a simulated-funding amount fails
// validation (parse error, non-positive, or above the safety cap).
var ErrInvalidAmount = errors.New("testenv: invalid simulated amount")

// ErrInvalidCurrency is returned for non-ISO-4217-shaped currency codes.
var ErrInvalidCurrency = errors.New("testenv: invalid currency")

// ErrInsufficientBalance is returned when a simulated withdrawal exceeds
// the available wallet balance — the semantics deliberately mirror the
// production INSUFFICIENT_BALANCE code so testnet clients exercise the
// same error path.
var ErrInsufficientBalance = errors.New("testenv: insufficient simulated balance")

// ResetCooldown is the Task 5.3.13 window.
const ResetCooldown = 5 * time.Minute

// New wires the service. env is config.Environment verbatim; empty env
// fails closed to production behaviour (disabled).
func New(pool *pgxpool.Pool, env string) *Service {
	return &Service{
		pool:    pool,
		env:     strings.ToLower(strings.TrimSpace(env)),
		limiter: NewLimiter(ResetCooldown),
		now:     time.Now,
	}
}

// SetClockForTest overrides the clock; tests only.
func (s *Service) SetClockForTest(now func() time.Time) {
	s.now = now
	s.limiter.now = now
}

// Enabled reports whether the environment permits resets. "testnet"
// (Phase-14 Task 14.3.3) is an explicit non-production label: a testnet
// deployment is still never production, and any production/empty/unknown
// label keeps every test endpoint disabled — fail closed, including the
// testnet/production mismatch case.
func (s *Service) Enabled() bool {
	switch s.env {
	case "development", "dev", "staging", "stage", "test", "testing", "sandbox", "local", "ci", "testnet":
		return true
	}
	return false
}

// IsTestnet reports whether this deployment is the dedicated testnet
// environment (Task 14.3.3 — testnet.exchange.com). Testnet carries the
// same fail-closed contract as every other non-production label; the
// distinction only matters for observability/ops reporting.
func (s *Service) IsTestnet() bool { return s.env == "testnet" }

// ResetAccount clears the account's market/fund state inside one
// transaction and returns the per-table row counts. Publishes nothing —
// reset is a test-environment op, not a domain event.
//
// Deletion order follows spec §5 dependency direction — every child row
// is removed before its parent. A shared trade journal touches both
// counterparties, so journals are deleted only once they are fully
// unreferenced (the counterparty's ledger rows keep them alive).
// Auth/session/KYC rows are deliberately out of scope — the reset
// contract is "balances, orders, positions" (plus the ledger trail that
// must stay consistent with them).
func (s *Service) ResetAccount(ctx context.Context, accountID int64) (map[string]int64, error) {
	if err := s.gate(accountID); err != nil {
		return nil, err
	}
	return s.resetTx(ctx, accountID)
}

// gate enforces the shared precondition for every test-environment
// mutation: non-production environment, positive account id, and the
// 5-minute per-account cooldown. ResetTo consumes exactly one cooldown
// slot — a reset+seed is still one reset.
func (s *Service) gate(accountID int64) error {
	if !s.Enabled() {
		return ErrDisabled
	}
	if accountID <= 0 {
		return fmt.Errorf("testenv: account id must be positive")
	}
	if !s.limiter.Allow(accountID) {
		return ErrCooldown
	}
	return nil
}

func (s *Service) resetTx(ctx context.Context, accountID int64) (map[string]int64, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return nil, fmt.Errorf("testenv: reset tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	counts := map[string]int64{}
	exec := func(label, q string, args ...any) error {
		tag, err := tx.Exec(ctx, q, args...)
		if err != nil {
			return fmt.Errorf("testenv: reset %s: %w", label, err)
		}
		counts[label] = tag.RowsAffected()
		return nil
	}

	// Journals this account's ledger rows point at — remembered before
	// ledger_entries go, so shared journals stay reachable for the
	// counterparty.
	if err := exec("reset_journals_tmp",
		`CREATE TEMP TABLE _reset_journals ON COMMIT DROP AS
		 SELECT DISTINCT journal_entry_id FROM ledger_entries
		  WHERE account_id=$1 AND journal_entry_id IS NOT NULL`,
		accountID); err != nil {
		return nil, err
	}
	delete(counts, "reset_journals_tmp")

	// Carry-trade tree: yields/totals under the account's allocations,
	// legs under the account's positions, then the allocations.
	if err := exec("carry_yield_records",
		`DELETE FROM carry_yield_records WHERE allocation_id IN
		  (SELECT id FROM carry_trade_allocations WHERE account_id=$1)`,
		accountID); err != nil {
		return nil, err
	}
	if err := exec("carry_yield_totals",
		`DELETE FROM carry_yield_totals WHERE allocation_id IN
		  (SELECT id FROM carry_trade_allocations WHERE account_id=$1)`,
		accountID); err != nil {
		return nil, err
	}
	if err := exec("carry_trade_legs",
		`DELETE FROM carry_trade_legs WHERE position_id IN
		  (SELECT id FROM positions WHERE account_id=$1)`,
		accountID); err != nil {
		return nil, err
	}
	if err := exec("carry_trade_allocations",
		`DELETE FROM carry_trade_allocations WHERE account_id=$1`,
		accountID); err != nil {
		return nil, err
	}
	// Journal-referencing, account-owned audit rows before the journal sweep.
	if err := exec("swap_free_admin_fee_assessments",
		`DELETE FROM swap_free_admin_fee_assessments WHERE account_id=$1`,
		accountID); err != nil {
		return nil, err
	}
	if err := exec("swap_accrual_records",
		`DELETE FROM swap_accrual_records WHERE account_id=$1`,
		accountID); err != nil {
		return nil, err
	}
	if err := exec("currency_conversions",
		`DELETE FROM currency_conversions WHERE account_id=$1`,
		accountID); err != nil {
		return nil, err
	}
	if err := exec("dust_sweeps",
		`DELETE FROM dust_sweeps WHERE account_id=$1`,
		accountID); err != nil {
		return nil, err
	}
	// Market side: fills → positions → orders → processed dedup → trades.
	if err := exec("position_fills",
		`DELETE FROM position_fills WHERE account_id=$1`,
		accountID); err != nil {
		return nil, err
	}
	if err := exec("positions",
		`DELETE FROM positions WHERE account_id=$1`,
		accountID); err != nil {
		return nil, err
	}
	// Order children before the orders sweep: the §5.4a idempotency
	// ledger and the order-audit trail both FK orders.id (migration 154
	// / order lifecycle audit) — deleting parents first trips 23503.
	if err := exec("client_order_id_dedup",
		`DELETE FROM client_order_id_dedup WHERE account_id=$1`,
		accountID); err != nil {
		return nil, err
	}
	if err := exec("order_audit",
		`DELETE FROM order_audit WHERE order_id IN
		  (SELECT id FROM orders WHERE account_id=$1)`,
		accountID); err != nil {
		return nil, err
	}
	if err := exec("orders",
		`DELETE FROM orders WHERE account_id=$1`,
		accountID); err != nil {
		return nil, err
	}
	if err := exec("processed_trades",
		`DELETE FROM processed_trades WHERE trade_id IN
		  (SELECT id FROM trades WHERE buyer_account_id=$1 OR seller_account_id=$1)`,
		accountID); err != nil {
		return nil, err
	}
	// Either-side trades: single OR pass — each row deleted exactly once.
	if err := exec("trades",
		`DELETE FROM trades WHERE buyer_account_id=$1 OR seller_account_id=$1`,
		accountID); err != nil {
		return nil, err
	}
	// Funding side: confirmations/chargebacks → funding txns → transfers.
	if err := exec("withdrawal_confirmations",
		`DELETE FROM withdrawal_confirmations WHERE withdrawal_id IN
		  (SELECT id FROM funding_transactions WHERE account_id=$1)`,
		accountID); err != nil {
		return nil, err
	}
	if err := exec("chargebacks",
		`DELETE FROM chargebacks WHERE account_id=$1`,
		accountID); err != nil {
		return nil, err
	}
	if err := exec("funding_transactions",
		`DELETE FROM funding_transactions WHERE account_id=$1`,
		accountID); err != nil {
		return nil, err
	}
	if err := exec("transfers",
		`DELETE FROM transfers
		  WHERE account_id=$1 OR from_account_id=$1 OR to_account_id=$1`,
		accountID); err != nil {
		return nil, err
	}
	// Webhook test traffic.
	if err := exec("webhook_deliveries",
		`DELETE FROM webhook_deliveries WHERE account_id=$1`,
		accountID); err != nil {
		return nil, err
	}
	if err := exec("webhook_endpoints",
		`DELETE FROM webhook_endpoints WHERE account_id=$1`,
		accountID); err != nil {
		return nil, err
	}
	// Ledger: per-wallet verification cache first (its last_entry_id FKs
	// to ledger_entries), then the ledger rows, then orphaned journals
	// (only journals nothing still references — shared trade journals
	// survive for the counterparty).
	if err := exec("journal_sums",
		`DELETE FROM journal_sums WHERE account_id=$1`,
		accountID); err != nil {
		return nil, err
	}
	if err := exec("ledger_entries",
		`DELETE FROM ledger_entries WHERE account_id=$1`,
		accountID); err != nil {
		return nil, err
	}
	if err := exec("ledger_lines",
		`DELETE FROM ledger_lines WHERE journal_entry_id IN (
		  SELECT journal_entry_id FROM _reset_journals j
		   WHERE NOT EXISTS (SELECT 1 FROM ledger_entries le
		                      WHERE le.journal_entry_id = j.journal_entry_id))`,
	); err != nil {
		return nil, err
	}
	if err := exec("journal_entries",
		`DELETE FROM journal_entries WHERE id IN (
		  SELECT journal_entry_id FROM _reset_journals j
		   WHERE NOT EXISTS (SELECT 1 FROM ledger_entries le
		                      WHERE le.journal_entry_id = j.journal_entry_id)
		     AND NOT EXISTS (SELECT 1 FROM ledger_lines ll
		                      WHERE ll.journal_entry_id = j.journal_entry_id)
		     AND NOT EXISTS (SELECT 1 FROM dust_sweeps ds
		                      WHERE ds.journal_entry_id = j.journal_entry_id)
		     AND NOT EXISTS (SELECT 1 FROM carry_yield_records cyr
		                      WHERE cyr.journal_entry_id = j.journal_entry_id)
		     AND NOT EXISTS (SELECT 1 FROM currency_conversions cc
		                      WHERE cc.journal_entry_id = j.journal_entry_id)
		     AND NOT EXISTS (SELECT 1 FROM swap_accrual_records sar
		                      WHERE sar.journal_entry_id = j.journal_entry_id)
		     AND NOT EXISTS (SELECT 1 FROM swap_free_admin_fee_assessments sfa
		                      WHERE sfa.journal_entry_id = j.journal_entry_id))`,
	); err != nil {
		return nil, err
	}
	// Balances last — re-zero rather than delete so downstream reads stay
	// shape-stable (balances.total is GENERATED; lock version bumps).
	if err := exec("balances_zeroed",
		`UPDATE balances SET available=0, locked=0, version=version+1
		  WHERE account_id=$1`, accountID); err != nil {
		return nil, err
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("testenv: reset commit: %w", err)
	}
	return counts, nil
}

// ---------------------------------------------------------------------------
// Cooldown limiter — in-memory token gate, one reset / account / window.
// ---------------------------------------------------------------------------

// Limiter is a per-account fixed-window gate.
type Limiter struct {
	mu    sync.Mutex
	last  map[int64]time.Time
	width time.Duration
	now   func() time.Time
}

// NewLimiter builds a limiter with the given cooldown window.
func NewLimiter(width time.Duration) *Limiter {
	return &Limiter{last: map[int64]time.Time{}, width: width, now: time.Now}
}

// Allow records a reset for accountID if the window has elapsed; returns
// false while still cooling down.
func (l *Limiter) Allow(accountID int64) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if prev, ok := l.last[accountID]; ok && l.now().Sub(prev) < l.width {
		return false
	}
	l.last[accountID] = l.now()
	return true
}

// ---------------------------------------------------------------------------
// Phase-14 Task 14.3.3 — testnet presets & simulated funding
// ---------------------------------------------------------------------------
//
// Presets give every test account a deterministic, documented wallet set.
// Simulated funding mutates balances rows ONLY — no funding_transactions,
// no ledger trail, and NEVER a call into internal/funding (real banking
// rails). That isolation is structural: this package cannot import
// internal/funding (funding imports settlement/ledger chains the test
// surface must not touch), and the handlers below wire no funding
// dependency. Simulated balances intentionally diverge from the journal
// ledger — testnet is not reconciliation-grade, and ops treats the
// `testnet` environment label as out-of-scope for ledger audit.

// Preset names a documented balance fixture.
type Preset string

// PresetStandard is the Task 14.3.3 default wallet: enough fiat across
// major settlement currencies to exercise spot, margin and conversion
// flows without external top-ups.
const PresetStandard Preset = "standard"

// presetBalances — fiat-only fixture amounts (8dp strings; balances is
// DECIMAL(28,8)).
var presetBalances = map[Preset]map[string]string{
	PresetStandard: {
		"USD": "100000.00000000",
		"EUR": "50000.00000000",
		"GBP": "25000.00000000",
		"CHF": "50000.00000000",
		"JPY": "10000000.00000000",
	},
}

// Presets lists the named fixtures — handler validation + docs source.
func Presets() []Preset {
	out := make([]Preset, 0, len(presetBalances))
	for p := range presetBalances {
		out = append(out, p)
	}
	return out
}

// isoCurrency limits simulated funding to ISO-4217-shaped codes — the
// same fiat-only domain as production balances (VARCHAR(3)).
var isoCurrency = regexp.MustCompile(`^[A-Z]{3}$`)

// maxSimulatedAmount is a defence-in-depth bound on a single simulated
// movement — a typo must not mint absurd testnet balances.
var maxSimulatedAmount = decimal.NewFromInt(1_000_000_000_000)

// Seed writes the named preset into the account's wallets (upsert per
// currency; balances not named by the preset are left untouched — call
// ResetTo for a clean-slate+seed). Seed does NOT consume the reset
// cooldown: it is an additive fixture, not a destructive reset.
func (s *Service) Seed(ctx context.Context, accountID int64, preset Preset) (map[string]string, error) {
	if !s.Enabled() {
		return nil, ErrDisabled
	}
	if accountID <= 0 {
		return nil, fmt.Errorf("testenv: account id must be positive")
	}
	amounts, ok := presetBalances[preset]
	if !ok {
		return nil, ErrInvalidPreset
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return nil, fmt.Errorf("testenv: seed tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	for ccy, amt := range amounts {
		if _, err := tx.Exec(ctx,
			`INSERT INTO balances (account_id, currency, available, locked, version)
			 VALUES ($1, $2, $3, 0, 1)
			 ON CONFLICT (account_id, currency)
			 DO UPDATE SET available=$3, locked=0, version=balances.version+1`,
			accountID, ccy, amt); err != nil {
			return nil, fmt.Errorf("testenv: seed %s: %w", ccy, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("testenv: seed commit: %w", err)
	}
	return amounts, nil
}

// ResetTo is the canonical testnet provisioning call: one 5-minute
// cooldown slot covers reset → clean slate → seeded preset.
func (s *Service) ResetTo(ctx context.Context, accountID int64, preset Preset) (map[string]int64, map[string]string, error) {
	if _, ok := presetBalances[preset]; !ok {
		return nil, nil, ErrInvalidPreset
	}
	if err := s.gate(accountID); err != nil {
		return nil, nil, err
	}
	counts, err := s.resetTx(ctx, accountID)
	if err != nil {
		return nil, nil, err
	}
	// The preset is validated above, so the seed tx cannot fail on
	// unknown names; a storage failure surfaces verbatim.
	amounts, err := s.Seed(ctx, accountID, preset)
	if err != nil {
		return counts, nil, err
	}
	return counts, amounts, nil
}

// SimulateDeposit credits the wallet directly — testnet faucet. No
// funding_transactions row, no rail adapter, no compliance hold: the
// contract is "fake money appears", full stop.
func (s *Service) SimulateDeposit(ctx context.Context, accountID int64, currency, amount string) (string, error) {
	ccy, amt, err := s.validateFund(accountID, currency, amount)
	if err != nil {
		return "", err
	}
	var available string
	err = s.pool.QueryRow(ctx,
		`INSERT INTO balances (account_id, currency, available, locked, version)
		 VALUES ($1, $2, $3, 0, 1)
		 ON CONFLICT (account_id, currency)
		 DO UPDATE SET available=balances.available+$3, version=balances.version+1
		 RETURNING available::text`,
		accountID, ccy, amt.String()).Scan(&available)
	if err != nil {
		return "", fmt.Errorf("testenv: simulated deposit: %w", err)
	}
	return available, nil
}

// SimulateWithdrawal debits the wallet if the available balance covers
// the amount — the production INSUFFICIENT_BALANCE path shape, minus any
// rail. Fails closed on shortfall; no row is written.
func (s *Service) SimulateWithdrawal(ctx context.Context, accountID int64, currency, amount string) (string, error) {
	ccy, amt, err := s.validateFund(accountID, currency, amount)
	if err != nil {
		return "", err
	}
	var available string
	err = s.pool.QueryRow(ctx,
		`UPDATE balances SET available=available-$3, version=version+1
		  WHERE account_id=$1 AND currency=$2 AND available >= $3
		 RETURNING available::text`,
		accountID, ccy, amt.String()).Scan(&available)
	if err == pgx.ErrNoRows {
		return "", ErrInsufficientBalance
	}
	if err != nil {
		return "", fmt.Errorf("testenv: simulated withdrawal: %w", err)
	}
	return available, nil
}

func (s *Service) validateFund(accountID int64, currency, amount string) (string, decimal.Decimal, error) {
	var zero decimal.Decimal
	if !s.Enabled() {
		return "", zero, ErrDisabled
	}
	if accountID <= 0 {
		return "", zero, fmt.Errorf("testenv: account id must be positive")
	}
	ccy := strings.ToUpper(strings.TrimSpace(currency))
	if !isoCurrency.MatchString(ccy) {
		return "", zero, ErrInvalidCurrency
	}
	amt, err := decimal.NewFromString(strings.TrimSpace(amount))
	if err != nil || !amt.IsPositive() || amt.GreaterThan(maxSimulatedAmount) {
		return "", zero, ErrInvalidAmount
	}
	return ccy, amt, nil
}
