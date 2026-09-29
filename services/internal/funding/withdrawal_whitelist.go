// Withdrawal whitelist mode — Phase-11 Task 11.3.10, spec §5.23
// (bank_accounts registry) + §5.44 item 11 (withdrawal_whitelist_settings,
// migration 078) + §24 #391.
//
// Semantics:
//   - WHITELIST_ONLY restricts withdrawals to VERIFIED bank_accounts
//     beneficiaries whose 24h addition timelock (unlocked_at) has lapsed.
//   - Disabling WHITELIST_ONLY latches the account-scoped egress lock
//     (withdrawal_lock_until = now + 24h) and the re-enable latch
//     (timelock_until = now + 24h — re-enable is rate-limited; the
//     documented unlock path is to wait out the latch; no API shortens
//     it). The lock is strictly account-scoped — never platform-wide.
//   - An account with no settings row defaults to ALLOW_ALL.
package funding

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// Whitelist codes (spec §23 rows registered under Task 11.3.10).
const (
	// CodeWithdrawalWhitelistOnly — 422: WHITELIST_ONLY mode is active
	// and the destination resolves to no VERIFIED bank_accounts row.
	CodeWithdrawalWhitelistOnly = "WITHDRAWAL_WHITELIST_ONLY"
	// (BENEFICIARY_HOLD_ACTIVE — the destination's 24h addition
	// timelock — is declared in bank_accounts.go by the Task 11.3.7
	// beneficiary-registry cluster; this package reuses that const.)
	// CodeWithdrawalWhitelistLocked — 423: the account-scoped egress
	// lock is active (whitelist-only mode was disabled < 24h ago).
	CodeWithdrawalWhitelistLocked = "WITHDRAWAL_WHITELIST_LOCKED"
	// CodeWhitelistChangeLocked — 429: re-enable is rate-limited until
	// timelock_until lapses (the latch set by the last disable).
	CodeWhitelistChangeLocked = "WHITELIST_CHANGE_LOCKED"
)

// WhitelistLockDuration is the canonical 24-hour account-scoped
// deactivation safety lock (and the re-enable latch window).
const WhitelistLockDuration = 24 * time.Hour

// WhitelistStore is the persistence seam (PgStore satisfies it).
type WhitelistStore interface {
	BeginTx(ctx context.Context) (pgx.Tx, error)
	WhitelistSettings(ctx context.Context, accountID int64) (*WhitelistSettingsRow, error)
	WhitelistSettingsForUpdate(ctx context.Context, tx pgx.Tx, accountID int64) (*WhitelistSettingsRow, error)
	UpsertWhitelistSettings(ctx context.Context, tx pgx.Tx, r WhitelistSettingsRow) error
	WhitelistedBeneficiaries(ctx context.Context, accountID int64) ([]BeneficiaryRow, error)
}

// WhitelistService owns the account-scoped whitelist mode.
type WhitelistService struct {
	store WhitelistStore
	clock func() time.Time
	logf  func(format string, args ...any)
}

// NewWhitelistService wires the service; store is mandatory
// (fail closed — an absent store refuses the configuration surface).
func NewWhitelistService(store WhitelistStore) (*WhitelistService, error) {
	if store == nil {
		return nil, fmt.Errorf("funding: whitelist service requires store")
	}
	return &WhitelistService{store: store, clock: time.Now}, nil
}

// WithClock overrides the clock (tests).
func (s *WhitelistService) WithClock(c func() time.Time) *WhitelistService {
	s.clock = c
	return s
}

// WithLogger wires a log sink.
func (s *WhitelistService) WithLogger(f func(format string, args ...any)) *WhitelistService {
	s.logf = f
	return s
}

// WhitelistView is the GET response: effective settings + the whitelist
// membership (VERIFIED beneficiaries) and lock state.
type WhitelistView struct {
	Mode                string           `json:"mode"`
	WhitelistOnly       bool             `json:"whitelist_only_enabled"`
	TimelockUntil       *time.Time       `json:"timelock_until,omitempty"`
	WithdrawalLockUntil *time.Time       `json:"withdrawal_lock_until,omitempty"`
	WithdrawalsLocked   bool             `json:"withdrawals_locked"`
	ReenableLocked      bool             `json:"reenable_locked"`
	Beneficiaries       []BeneficiaryRow `json:"beneficiaries"`
	UpdatedAt           *time.Time       `json:"updated_at,omitempty"`
}

// Get returns the effective whitelist state for the account.
func (s *WhitelistService) Get(ctx context.Context, accountID int64) (*WhitelistView, error) {
	row, err := s.store.WhitelistSettings(ctx, accountID)
	if err != nil {
		return nil, err
	}
	bens, err := s.store.WhitelistedBeneficiaries(ctx, accountID)
	if err != nil {
		return nil, err
	}
	return s.view(row, bens), nil
}

func (s *WhitelistService) view(row *WhitelistSettingsRow,
	bens []BeneficiaryRow) *WhitelistView {
	now := s.clock().UTC()
	v := &WhitelistView{Mode: WhitelistModeAllowAll, Beneficiaries: bens}
	if bens == nil {
		v.Beneficiaries = []BeneficiaryRow{}
	}
	if row == nil {
		return v
	}
	v.Mode = row.Mode
	v.WhitelistOnly = row.Mode == WhitelistModeOnly
	v.TimelockUntil = row.TimelockUntil
	v.WithdrawalLockUntil = row.WithdrawalLockUntil
	v.WithdrawalsLocked = row.WithdrawalLockUntil != nil && row.WithdrawalLockUntil.After(now)
	v.ReenableLocked = row.TimelockUntil != nil && row.TimelockUntil.After(now)
	v.UpdatedAt = &row.UpdatedAt
	return v
}

// Enable switches the account to WHITELIST_ONLY. Re-enable is refused
// while the deactivation latch (timelock_until) is in the future —
// WHITELIST_CHANGE_LOCKED 429 carries the remaining window.
func (s *WhitelistService) Enable(ctx context.Context, accountID, userID int64) (*WhitelistView, error) {
	tx, err := s.store.BeginTx(ctx)
	if err != nil {
		return nil, wrapCode("INTERNAL_ERROR", "whitelist tx", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	row, err := s.store.WhitelistSettingsForUpdate(ctx, tx, accountID)
	if err != nil {
		return nil, err
	}
	now := s.clock().UTC()
	if row != nil && row.TimelockUntil != nil && row.TimelockUntil.After(now) &&
		row.Mode != WhitelistModeOnly {
		return nil, errf(CodeWhitelistChangeLocked,
			"whitelist-only re-enable is rate-limited until %s (24h deactivation latch)",
			row.TimelockUntil.Format(time.RFC3339))
	}
	up := WhitelistSettingsRow{
		AccountID: accountID,
		Mode:      WhitelistModeOnly,
		UpdatedBy: &userID,
	}
	// Enabling clears the egress latch — the account opted back into the
	// strictest mode; the 24h deactivation lock only applies to ALLOW_ALL.
	if err := s.store.UpsertWhitelistSettings(ctx, tx, up); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, wrapCode("INTERNAL_ERROR", "whitelist enable commit", err)
	}
	return s.Get(ctx, accountID)
}

// Disable switches the account to ALLOW_ALL and latches the
// account-scoped 24h egress lock + the 24h re-enable latch. Idempotent:
// disabling an already-ALLOW_ALL account refreshes nothing unless it
// actually transitions (a second disable replays the current view).
func (s *WhitelistService) Disable(ctx context.Context, accountID, userID int64) (*WhitelistView, error) {
	tx, err := s.store.BeginTx(ctx)
	if err != nil {
		return nil, wrapCode("INTERNAL_ERROR", "whitelist tx", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	row, err := s.store.WhitelistSettingsForUpdate(ctx, tx, accountID)
	if err != nil {
		return nil, err
	}
	now := s.clock().UTC()
	if row == nil || row.Mode == WhitelistModeAllowAll {
		// Nothing to disable — replay the effective view.
		_ = tx.Rollback(ctx)
		return s.Get(ctx, accountID)
	}
	lockUntil := now.Add(WhitelistLockDuration)
	up := WhitelistSettingsRow{
		AccountID:           accountID,
		Mode:                WhitelistModeAllowAll,
		TimelockUntil:       &lockUntil, // re-enable latch
		WithdrawalLockUntil: &lockUntil, // account-scoped egress lock
		UpdatedBy:           &userID,
	}
	if err := s.store.UpsertWhitelistSettings(ctx, tx, up); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, wrapCode("INTERNAL_ERROR", "whitelist disable commit", err)
	}
	s.log("funding: account %d disabled whitelist-only mode — egress locked until %s",
		accountID, lockUntil.Format(time.RFC3339))
	return s.Get(ctx, accountID)
}

// GateCheck is the withdrawal-create seam (consumed by FlowService):
//
//	lock_until in future      → WITHDRAWAL_WHITELIST_LOCKED (423)
//	mode WHITELIST_ONLY + no verified beneficiary row
//	                          → WITHDRAWAL_WHITELIST_ONLY (422)
//	verified beneficiary still inside unlocked_at
//	                          → BENEFICIARY_TIMELOCK_ACTIVE (422)
//
// Under ALLOW_ALL a non-verified destination returns
// GateOutcome{Unverified:true} so the caller applies the 24h
// unverified-destination hold instead of rejecting.
type GateOutcome struct {
	Beneficiary *BeneficiaryRow // matched bank_accounts row (any status)
	LockedUntil *time.Time      // destination hold_until to stamp (ALLOW_ALL)
	Unverified  bool            // no VERIFIED beneficiary matched
}

// GateStore is the read seam the withdrawal gate consumes.
type GateStore interface {
	WhitelistSettings(ctx context.Context, accountID int64) (*WhitelistSettingsRow, error)
	BeneficiaryByDestination(ctx context.Context, accountID int64, destination string) (*BeneficiaryRow, error)
}

// CheckWithdrawalGate evaluates the whitelist/lock gate for one
// destination (reference_account). It is called inside the caller's
// decision path — all errors are fail-closed.
func CheckWithdrawalGate(ctx context.Context, store GateStore,
	now time.Time, accountID int64, destination string) (*GateOutcome, error) {
	row, err := store.WhitelistSettings(ctx, accountID)
	if err != nil {
		return nil, err
	}
	if row != nil && row.WithdrawalLockUntil != nil && row.WithdrawalLockUntil.After(now) {
		return nil, errf(CodeWithdrawalWhitelistLocked,
			"withdrawals locked until %s — whitelist-only mode was disabled less than 24h ago",
			row.WithdrawalLockUntil.Format(time.RFC3339))
	}
	ben, err := store.BeneficiaryByDestination(ctx, accountID, destination)
	if err != nil {
		return nil, err
	}
	out := &GateOutcome{Beneficiary: ben}
	verified := ben != nil && ben.Status == BeneficiaryVerified
	timelocked := verified && ben.UnlockedAt != nil && ben.UnlockedAt.After(now)

	if row != nil && row.Mode == WhitelistModeOnly {
		if !verified {
			return nil, errCode(CodeWithdrawalWhitelistOnly,
				"whitelist-only mode is enabled — destination is not a verified beneficiary")
		}
		if timelocked {
			return nil, errf(CodeBeneficiaryHoldActive,
				"beneficiary %d unlocked at %s — 24h addition timelock still active",
				ben.BankAccountID, ben.UnlockedAt.Format(time.RFC3339))
		}
		return out, nil
	}
	// ALLOW_ALL: verified-but-locked beneficiary → hold until unlocked_at;
	// unregistered destination → caller stamps first-seen +24h hold.
	if timelocked {
		out.LockedUntil = ben.UnlockedAt
		return out, nil
	}
	if !verified {
		out.Unverified = true
	}
	return out, nil
}

func (s *WhitelistService) log(format string, args ...any) {
	if s.logf != nil {
		s.logf(format, args...)
	}
}
