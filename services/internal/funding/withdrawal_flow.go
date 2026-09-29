// Withdrawal lifecycle additions — Phase-11 Task 11.3.2 on top of the
// Phase-05 WithdrawalService (withdrawals.go, unchanged):
//
//   - FlowService wraps the Phase-05 service with the Phase-11 gates:
//     whitelist mode + account-scoped deactivation lock + beneficiary
//     timelock (Task 11.3.10), the 30-minute same-destination cooldown
//     and the 24h unverified-destination hold (Task 11.3.2 steps 5–6),
//     the TOTP step-up at confirmation (spec §12.6/§21 funding screen:
//     "2FA confirmation"), and the nostro-aware dispatch hand-off
//     (Task 11.3.6).
//   - AdminApprove/AdminReject action the >$50K PENDING_REVIEW tier
//     behind the registered four-eyes routes (admin/withdrawals/{id}).
//
// Gate order at create: account mutable → whitelist/lock/beneficiary →
// cooldown → limits/tiers (inherited from the inner service). Any gate
// failure rejects before the PENDING row exists — no reservation is
// ever taken for a destination that cannot be dispatched to.
package funding

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"exchange/pkg/decimal"
)

// WithdrawalCooldown is the 30-minute same-bank-account cooldown after a
// COMPLETED withdrawal (spec §23 WITHDRAWAL_COOLDOWN_ACTIVE row).
const WithdrawalCooldown = 30 * time.Minute

// UnverifiedDestinationHold is the 24-hour hold applied to withdrawals
// whose destination is not a verified beneficiary (Task 11.3.2 step 6).
const UnverifiedDestinationHold = 24 * time.Hour

// TOTPSecretProvider resolves the account owner's TOTP seed
// (*accounts.PgxTOTPSecrets — same seam CloseAllService uses).
type TOTPSecretProvider interface {
	TOTPSecretForAccount(ctx context.Context, accountID int64) (string, error)
}

// TOTPVerifier verifies an RFC 6238 code (auth.VerifyTOTP:
// SHA1/30s/6 digits, ±1 step).
type TOTPVerifier func(secret, code string, at time.Time) bool

// FlowStore is the persistence seam FlowService consumes — a subset of
// Store plus the migration-199 extensions (PgStore satisfies it).
type FlowStore interface {
	Store
	BeneficiaryByDestination(ctx context.Context, accountID int64, destination string) (*BeneficiaryRow, error)
	WhitelistSettings(ctx context.Context, accountID int64) (*WhitelistSettingsRow, error)
	DestinationHold(ctx context.Context, tx pgx.Tx, accountID int64, destination string) (*DestinationHoldRow, error)
	UpsertDestinationHold(ctx context.Context, tx pgx.Tx, accountID int64, destination string, unlockedAt time.Time) (*DestinationHoldRow, error)
	LastCompletedWithdrawalAt(ctx context.Context, accountID int64, destination string) (*time.Time, error)
	WithdrawalByID(ctx context.Context, id int64) (*WithdrawalRow, error)
	SetWithdrawalHoldUntil(ctx context.Context, tx pgx.Tx, id int64, until *time.Time) error
	SetWithdrawalReview(ctx context.Context, tx pgx.Tx, id int64, status string, reviewedBy int64, at time.Time) error
}

// WithdrawalDispatcher is the Task 11.3.6 hand-off seam
// (*DispatchService): Release is invoked once a withdrawal reaches
// CONFIRMED — it either dispatches to the rail or queues for nostro
// headroom. nil → CONFIRMED withdrawals wait for the sweep.
type WithdrawalDispatcher interface {
	Release(ctx context.Context, withdrawalID int64) (*ReleaseResult, error)
}

// WithdrawalApprovalGate is the Task 12.3.11 client multi-validator
// (M-of-N) seam consulted at withdrawal create. When the account's
// active WITHDRAWAL policy covers the amount, the gate records (or
// replays) the pending approval request and returns approved=false —
// the withdrawal row is never created and no funds are reserved until
// the M-of-N quorum decides; a resubmission whose request is APPROVED
// is consumed and proceeds. Errors fail closed.
//
// Implemented by delegation.Service.CheckWithdrawal — the interface
// lives in funding so the wiring direction stays one-way.
type WithdrawalApprovalGate interface {
	CheckWithdrawal(ctx context.Context, accountID, userID int64,
		currency string, amount decimal.Decimal, destination string) (approved bool, approvalID int64, err error)
}

// FlowService wraps *WithdrawalService with the Phase-11 gates.
type FlowService struct {
	inner    *WithdrawalService
	store    FlowStore
	totp     TOTPSecretProvider // nil → 2FA fails closed
	verify   TOTPVerifier       // nil → fail closed
	dispatch WithdrawalDispatcher
	gate     WithdrawalApprovalGate // nil → no M-of-N policy enforcement
	clock    func() time.Time
	logf     func(format string, args ...any)
}

// NewFlowService wires the wrapper; inner + store are mandatory.
func NewFlowService(inner *WithdrawalService, store FlowStore) (*FlowService, error) {
	if inner == nil || store == nil {
		return nil, fmt.Errorf("funding: flow service requires withdrawal service and store")
	}
	return &FlowService{inner: inner, store: store, clock: time.Now}, nil
}

// WithTOTP wires the step-up verification seam.
func (s *FlowService) WithTOTP(p TOTPSecretProvider, v TOTPVerifier) *FlowService {
	s.totp, s.verify = p, v
	return s
}

// WithDispatcher wires the nostro-aware dispatch hand-off.
func (s *FlowService) WithDispatcher(d WithdrawalDispatcher) *FlowService {
	s.dispatch = d
	return s
}

// WithApprovalGate wires the Task 12.3.11 delegation M-of-N gate —
// optional; nil keeps the pre-delegation behaviour.
func (s *FlowService) WithApprovalGate(g WithdrawalApprovalGate) *FlowService {
	s.gate = g
	return s
}

// WithClock overrides the clock (tests).
func (s *FlowService) WithClock(c func() time.Time) *FlowService {
	s.clock = c
	return s
}

// WithLogger wires a log sink.
func (s *FlowService) WithLogger(f func(format string, args ...any)) *FlowService {
	s.logf = f
	return s
}

// ---------------------------------------------------------------------------
// Create — gate checks then delegate to the Phase-05 create path.
// ---------------------------------------------------------------------------

// Create applies the Phase-11 gates (whitelist mode / account egress
// lock / beneficiary timelock / 30-minute same-destination cooldown /
// 24h unverified-destination hold) and delegates to WithdrawalService.
func (s *FlowService) Create(ctx context.Context, req CreateWithdrawalRequest) (*WithdrawalResult, error) {
	dest := strings.TrimSpace(req.ReferenceAccount)
	if dest != "" {
		now := s.clock().UTC()
		outcome, err := CheckWithdrawalGate(ctx, s.store, now, req.AccountID, dest)
		if err != nil {
			return nil, err
		}
		// 30-minute same-destination cooldown (after a COMPLETED payout).
		last, err := s.store.LastCompletedWithdrawalAt(ctx, req.AccountID, dest)
		if err != nil {
			return nil, err
		}
		if last != nil && last.Add(WithdrawalCooldown).After(now) {
			return nil, errf("WITHDRAWAL_COOLDOWN_ACTIVE",
				"30-minute cooldown to this bank account until %s",
				last.Add(WithdrawalCooldown).Format(time.RFC3339))
		}
		// Unverified destination (ALLOW_ALL only — the whitelist gate
		// rejects unverified destinations under WHITELIST_ONLY): the
		// first-seen registry ages the destination for 24h. When the
		// Task 11.3.7 beneficiary registry seam is wired it is the
		// authority — unregistered destinations are rejected by the
		// inner create (BANK_ACCOUNT_NOT_VERIFIED), so no first-seen
		// hold row is stamped for a destination that can never pay out.
		var holdUntil *time.Time
		if outcome.LockedUntil != nil {
			holdUntil = outcome.LockedUntil
		} else if outcome.Unverified && s.inner.bens == nil {
			tx, terr := s.store.BeginTx(ctx)
			if terr != nil {
				return nil, wrapCode("INTERNAL_ERROR", "destination hold tx", terr)
			}
			h, herr := s.store.UpsertDestinationHold(ctx, tx, req.AccountID,
				dest, now.Add(UnverifiedDestinationHold))
			if herr != nil {
				_ = tx.Rollback(ctx)
				return nil, herr
			}
			if cerr := tx.Commit(ctx); cerr != nil {
				return nil, wrapCode("INTERNAL_ERROR", "destination hold commit", cerr)
			}
			if h.UnlockedAt.After(now) {
				holdUntil = &h.UnlockedAt
			}
		}
		// Task 12.3.11 M-of-N gate — a governed withdrawal records its
		// approval request and stops here (MULTI_VALIDATOR_REQUIRED);
		// the resubmission proceeds once the request is APPROVED and the
		// gate consumes it. Delegated logins are refused outright.
		if err := s.checkApprovalGate(ctx, req); err != nil {
			return nil, err
		}
		res, err := s.inner.Create(ctx, req)
		if err != nil {
			return nil, err
		}
		if holdUntil != nil && res.WithdrawalID > 0 && !res.Replayed {
			// Stamp the hold — the withdrawal stays CONFIRMED-but-held;
			// the dispatcher refuses it until hold_until lapses. Failure
			// fails closed: cancel the row so nothing dispatches unheld.
			if herr := s.stampHold(ctx, res.WithdrawalID, *holdUntil); herr != nil {
				s.failUnsafeWithdrawal(ctx, res.WithdrawalID)
				return nil, herr
			}
		}
		return res, nil
	}
	return s.inner.Create(ctx, req)
}

// checkApprovalGate consults the delegation M-of-N policy for the
// account (Task 12.3.11). approved=false → MULTI_VALIDATOR_REQUIRED
// (409) carrying the pending request id; a gate error propagates —
// never let an unverifiable governance check silently skip policy.
func (s *FlowService) checkApprovalGate(ctx context.Context, req CreateWithdrawalRequest) error {
	if s.gate == nil {
		return nil
	}
	amount, err := parseMoney(req.Amount)
	if err != nil {
		return err // inner.Create would reject identically — fail early
	}
	approved, approvalID, err := s.gate.CheckWithdrawal(ctx, req.AccountID,
		req.UserID, strings.ToUpper(strings.TrimSpace(req.Currency)),
		amount, strings.TrimSpace(req.ReferenceAccount))
	if err != nil {
		return err
	}
	if !approved {
		return errf("MULTI_VALIDATOR_REQUIRED",
			"withdrawal requires institutional approval — decision pending on request %d",
			approvalID)
	}
	return nil
}

// stampHold sets hold_until on the freshly created withdrawal.
func (s *FlowService) stampHold(ctx context.Context, withdrawalID int64, until time.Time) error {
	tx, err := s.store.BeginTx(ctx)
	if err != nil {
		return wrapCode("INTERNAL_ERROR", "hold stamp tx", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.store.SetWithdrawalHoldUntil(ctx, tx, withdrawalID, &until); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return wrapCode("INTERNAL_ERROR", "hold stamp commit", err)
	}
	return nil
}

// failUnsafeWithdrawal fails a withdrawal whose hold stamp could not be
// persisted — never leave a dispatchable unverified-destination
// withdrawal without its hold (fail closed). The reserved funds are
// released through the same compensating journal the sweeper uses.
func (s *FlowService) failUnsafeWithdrawal(ctx context.Context, withdrawalID int64) {
	if err := s.store.MarkWithdrawalFailed(ctx, withdrawalID); err != nil {
		s.log("funding: fail-closed mark-failed for withdrawal %d failed: %v", withdrawalID, err)
		return
	}
	if w, err := s.store.WithdrawalByID(ctx, withdrawalID); err == nil && w != nil {
		s.inner.releaseHold(ctx, w)
	}
}

// ---------------------------------------------------------------------------
// Confirm — TOTP step-up then the Phase-05 confirm; CONFIRMED results
// hand off to the nostro-aware dispatcher.
// ---------------------------------------------------------------------------

// StepUpConfirmRequest is ConfirmWithdrawalRequest plus the 2FA inputs
// the handler resolves (X-2FA-Token header + session AMR elevation).
type StepUpConfirmRequest struct {
	ConfirmWithdrawalRequest
	TOTPToken        string // X-2FA-Token header
	SessionTwoFactor bool   // claims.TwoFactorVerified()
}

// ConfirmStepUp enforces the TOTP step-up (spec §21: "withdrawal … 2FA
// confirmation"; §12.6 precedence: session AMR elevation satisfies the
// gate without a fresh code) then delegates to the Phase-05 confirm
// path. Confirmation remains idempotent — a re-confirm replays the
// stored result.
func (s *FlowService) ConfirmStepUp(ctx context.Context, req StepUpConfirmRequest) (*WithdrawalResult, error) {
	if err := s.checkTwoFactor(ctx, req.AccountID, req.TOTPToken, req.SessionTwoFactor); err != nil {
		return nil, err
	}
	res, err := s.inner.Confirm(ctx, req.ConfirmWithdrawalRequest)
	if err != nil {
		return nil, err
	}
	if res.Status == FundingConfirmed && s.dispatch != nil {
		if _, derr := s.dispatch.Release(ctx, res.WithdrawalID); derr != nil {
			// The confirmation committed; a failed dispatch hand-off is
			// picked up by the dispatcher sweep — never mask the confirm.
			s.log("funding: dispatch hand-off for withdrawal %d failed: %v",
				res.WithdrawalID, derr)
		}
	}
	return res, nil
}

// checkTwoFactor mirrors the CloseAllService precedence exactly:
// session AMR elevation satisfies; else a fresh X-2FA-Token must verify
// against the account owner's enrolled secret.
func (s *FlowService) checkTwoFactor(ctx context.Context, accountID int64,
	token string, session2FA bool) error {
	if session2FA {
		return nil
	}
	if s.totp == nil {
		return errCode("TWO_FACTOR_REQUIRED", "2FA provider not configured")
	}
	if token == "" {
		return errCode("TWO_FACTOR_REQUIRED", "X-2FA-Token header required")
	}
	secret, err := s.totp.TOTPSecretForAccount(ctx, accountID)
	if err != nil {
		return wrapCode("INTERNAL_ERROR", "2FA secret lookup", err)
	}
	if secret == "" {
		return errCode("TWO_FACTOR_REQUIRED", "2FA not enrolled for account owner")
	}
	if s.verify == nil || !s.verify(secret, token, s.clock().UTC()) {
		return errCode("TWO_FACTOR_REQUIRED", "invalid 2FA token")
	}
	return nil
}

// ---------------------------------------------------------------------------
// Admin review — POST /api/v1/admin/withdrawals/{id}/approve|reject
// (Task 5.3.46 routes; Finance Ops, four-eyes approver).
// ---------------------------------------------------------------------------

// AdminReview approves (CONFIRMED → dispatch) or rejects (FAILED +
// compensating hold release) a PENDING_REVIEW withdrawal.
// approverID must be a distinct non-zero second principal — the
// Phase-05 four-eyes contract (spec §8.2).
func (s *FlowService) AdminReview(ctx context.Context, adminID, approverID,
	withdrawalID int64, approve bool, note string) (*WithdrawalResult, error) {
	if approverID <= 0 {
		return nil, errCode("DUAL_CONTROL_REQUIRED",
			"approver_id required — withdrawal review is four-eyes")
	}
	if approverID == adminID {
		return nil, errCode("DUAL_CONTROL_VIOLATION",
			"approver must differ from the reviewing admin")
	}
	tx, err := s.store.BeginTx(ctx)
	if err != nil {
		return nil, wrapCode("INTERNAL_ERROR", "review tx", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	w, err := s.store.WithdrawalForUpdate(ctx, tx, withdrawalID)
	if err != nil {
		return nil, err
	}
	if w.Status != FundingPendingReview {
		return nil, errf("INVALID_LIFECYCLE_TRANSITION",
			"withdrawal %d is %s — only PENDING_REVIEW is reviewable",
			withdrawalID, w.Status)
	}
	now := s.clock().UTC()
	newStatus := FundingFailed
	if approve {
		newStatus = FundingConfirmed
	}
	if err := s.store.SetWithdrawalReview(ctx, tx, withdrawalID, newStatus,
		adminID, now); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, wrapCode("INTERNAL_ERROR", "review commit", err)
	}

	if approve {
		if s.dispatch != nil {
			if _, derr := s.dispatch.Release(ctx, withdrawalID); derr != nil {
				s.log("funding: dispatch hand-off for approved withdrawal %d failed: %v",
					withdrawalID, derr)
			}
		}
	} else {
		// Compensating journal: release the reserved hold (locked →
		// available, transit liability reversed) — Task 11.3.11 step 3.
		s.inner.releaseHold(ctx, w)
	}
	return &WithdrawalResult{
		WithdrawalID: withdrawalID,
		Status:       newStatus,
		Currency:     w.Currency,
		Amount:       w.Amount.String(),
		USDAmount:    decPtr(w.USDAmount),
		ReviewTier:   w.ReviewTier,
	}, nil
}

func (s *FlowService) log(format string, args ...any) {
	if s.logf != nil {
		s.logf(format, args...)
	}
}
