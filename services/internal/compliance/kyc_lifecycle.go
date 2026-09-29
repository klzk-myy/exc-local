// Phase-14 Task 14.3.4 — KYC lifecycle management (spec §14.2).
//
// Boundary: Phase-12 Task 12.3.4 owns the intake (documents → S3
// SSE-KMS → PENDING_REVIEW). This file owns everything after:
//   - the Compliance-Officer review decision (approve/reject), which is
//     the single write path to accounts.kyc_tier — tier limits flow
//     through the already-seeded tier-scoped risk_limits rows;
//   - re-verification bookkeeping (verified_at + reverify_months —
//     12mo T2 / 24mo INSTITUTIONAL from kyc_tier_policies, migration 204);
//   - the hourly re-verification sweeper: an overdue latest-APPROVED
//     submission auto-downgrades the account T2→T1 (an INSTITUTIONAL
//     approval additionally reverts client_category → RETAIL and
//     re-arms nbp — ECP status derives from the lapsed KYB), marks the
//     submission EXPIRED, writes the admin audit row + hash chain, then
//     raises a compliance ops alert and a user notification.
//
// Every transition writes admin_audit_log (+ audit_hash_chain) inside
// the SAME transaction as the state change — an audited action and its
// tamper-evident anchor commit or fail together (spec §5.8/§5.9).
package compliance

import (
	"context"
	"fmt"
	"strings"
	"time"

	excerrors "exchange/pkg/errors"
)

// ---------------------------------------------------------------------------
// Seams
// ---------------------------------------------------------------------------

// ReviewActor is the acting Compliance Officer (or stronger) plus the
// request context that lands in admin_audit_log.
type ReviewActor struct {
	AdminUserID int64
	ClientIP    string
}

// Notifier emits a user-facing event AFTER the decision/downgrade
// commit; account→user resolution lives in the adapter (the
// notifyAdapter convention shared with funding.Notifier — identical
// signature so one adapter satisfies both). Errors are swallowed by
// convention: a committed state transition never rolls back for a
// notification failure.
type Notifier interface {
	Notify(ctx context.Context, accountID int64, event string, payload map[string]any)
}

// Alerter raises the compliance-side ops alert for sweep actions — the
// (severity, code, summary) closure shape the wiring layer binds to the
// settlement.OpsAlert machinery; nil = alerts degrade to the audit row
// only.
type Alerter func(ctx context.Context, severity, code, summary string) error

// EventKYCDowngraded is the notification token for the re-verification
// downgrade (registered in internal/notifications' event vocabulary).
const EventKYCDowngraded = "kyc_tier_downgraded"

// EventKYCApproved/EventKYCRejected are the canonical decision tokens —
// kept here so emitters and tests name one symbol; the values match the
// notifications package vocabulary exactly.
const (
	EventKYCApproved = "kyc_approved"
	EventKYCRejected = "kyc_rejected"
)

// ---------------------------------------------------------------------------
// Store seam — PgStore implements it; unit tests fake it
// ---------------------------------------------------------------------------

// DecisionTx is one review decision applied atomically.
type DecisionTx struct {
	SubmissionID int64
	Approve      bool
	Reason       string // required on reject — lands in reject_reason
	ReviewerID   int64
	ClientIP     string
	Now          time.Time
	// ReverifyMonths comes from kyc_tier_policies for the requested
	// tier (0 = no periodic re-verification); the service resolves it
	// pre-tx so the store writes the value, not the policy lookup.
	ReverifyMonths int
}

// DecisionResult is the committed review outcome.
type DecisionResult struct {
	SubmissionID   int64      `json:"submission_id"`
	AccountID      int64      `json:"account_id"`
	Decision       string     `json:"decision"` // APPROVED | REJECTED
	RequestedTier  string     `json:"requested_tier"`
	AssignedTier   string     `json:"assigned_tier,omitempty"` // account.kyc_tier after the decision
	ClientCategory string     `json:"client_category,omitempty"`
	ReverifyDueAt  *time.Time `json:"reverify_due_at,omitempty"`
	AuditSeq       int64      `json:"audit_seq"`
	ReviewedAt     time.Time  `json:"reviewed_at"`
}

// OverdueReverify is one sweep candidate: the account's LATEST approved
// submission whose reverify_due_at passed while the account still sits
// at the elevated tier.
type OverdueReverify struct {
	SubmissionID  int64     `json:"submission_id"`
	AccountID     int64     `json:"account_id"`
	UserID        int64     `json:"user_id"` // audit attribution + notify routing
	RequestedTier string    `json:"requested_tier"`
	ReverifyDueAt time.Time `json:"reverify_due_at"`
}

// DowngradeTx is one sweep downgrade applied atomically.
type DowngradeTx struct {
	OverdueReverify
	Now time.Time
}

// LifecycleStore is the tx-atomic persistence seam.
type LifecycleStore interface {
	// SubmissionByID loads one submission row (nil when absent) — the
	// service resolves the requested tier + policy pre-tx.
	SubmissionByID(ctx context.Context, submissionID int64) (*Submission, error)
	// DecideSubmissionTx runs the review decision in one transaction:
	// lock the submission FOR UPDATE, validate the transition
	// (PENDING_REVIEW|UNDER_REVIEW only), write the decision columns +
	// the account effects (tier; INSTITUTIONAL adds client_category ECP
	// + nbp=false), flip document rows to their verdict, and append the
	// admin audit row + chain link.
	DecideSubmissionTx(ctx context.Context, p DecisionTx) (*DecisionResult, error)
	// OverdueReverifications feeds the sweep: latest-APPROVED
	// submissions past reverify_due_at whose account still sits at T2.
	OverdueReverifications(ctx context.Context, now time.Time, limit int) ([]OverdueReverify, error)
	// DowngradeReverifyTx applies one sweep action in one transaction:
	// account T2→T1 (+category RETAIL + nbp when the lapsed approval
	// was INSTITUTIONAL), submission → EXPIRED, audit row + chain.
	// Returns applied=false when the account no longer sits at T2
	// (concurrent re-verification or an earlier pass) — idempotent.
	DowngradeReverifyTx(ctx context.Context, p DowngradeTx) (applied bool, err error)
	// TierPolicy reads kyc_tier_policies (shared with kyc.go).
	TierPolicy(ctx context.Context, tier string) (*TierPolicy, error)
}

// ---------------------------------------------------------------------------
// Service
// ---------------------------------------------------------------------------

// LifecycleService owns the post-intake KYC lifecycle.
type LifecycleService struct {
	store    LifecycleStore
	resolver RoleResolver
	notifier Notifier
	alerter  Alerter
	now      func() time.Time
}

// LifecycleOptions wires the service. Store is mandatory; a nil
// resolver fails the admin surface closed (UNAUTHORIZED_ROLE); a nil
// notifier/alerter degrades those post-commit effects (the committed
// transition + audit row still stand — documented, same convention as
// the funding services).
type LifecycleOptions struct {
	Store    LifecycleStore
	Resolver RoleResolver
	Notifier Notifier
	Alerter  Alerter
	Now      func() time.Time
}

// NewLifecycleService validates wiring and builds the service.
func NewLifecycleService(o LifecycleOptions) (*LifecycleService, error) {
	if o.Store == nil {
		return nil, fmt.Errorf("compliance: lifecycle store is nil")
	}
	s := &LifecycleService{
		store:    o.Store,
		resolver: o.Resolver,
		notifier: o.Notifier,
		alerter:  o.Alerter,
		now:      o.Now,
	}
	if s.now == nil {
		s.now = time.Now
	}
	return s, nil
}

// SetClockForTest overrides the clock; tests only.
func (s *LifecycleService) SetClockForTest(now func() time.Time) { s.now = now }

// requireComplianceRole enforces the §8.2 role gate shared by approve,
// reject and the categorization workflow.
func (s *LifecycleService) requireComplianceRole(ctx context.Context, actor ReviewActor, op string) error {
	if actor.AdminUserID <= 0 {
		return excerrors.New("UNAUTHORIZED", "admin identity required")
	}
	if s.resolver == nil {
		return excerrors.New("UNAUTHORIZED_ROLE",
			"role resolver not configured — "+op+" rejected")
	}
	role, err := s.resolver(ctx, actor.AdminUserID)
	if err != nil {
		return excerrors.Wrap("INTERNAL_ERROR", "role lookup", err)
	}
	if !EligibleComplianceRoles[role] {
		return excerrors.New("UNAUTHORIZED_ROLE",
			op+" requires Compliance Officer or Super Admin")
	}
	return nil
}

// notify emits post-commit via the wired seam (nil-seam safe — the
// notification is a best-effort effect of an already-committed fact).
func (s *LifecycleService) notify(ctx context.Context, accountID int64, event string, payload map[string]any) {
	if s.notifier == nil {
		return
	}
	s.notifier.Notify(ctx, accountID, event, payload)
}

// Approve runs the Compliance-Officer approval: role gate → atomic
// decision tx (submission → APPROVED, verified_at, reverify_due_at =
// verified + policy months; account tier; INSTITUTIONAL adds ECP
// category + drops nbp) → kyc_approved notification.
func (s *LifecycleService) Approve(ctx context.Context, actor ReviewActor, submissionID int64) (*DecisionResult, error) {
	if err := s.requireComplianceRole(ctx, actor, "kyc.approve"); err != nil {
		return nil, err
	}
	if submissionID <= 0 {
		return nil, excerrors.New("INVALID_REQUEST", "submission id must be positive")
	}
	// Reverify months resolve from the submission's requested tier —
	// read pre-tx (policy rows are quasi-static seeds).
	res, err := s.decide(ctx, submissionID, true, "", actor)
	if err != nil {
		return nil, err
	}
	s.notify(ctx, res.AccountID, EventKYCApproved, map[string]any{
		"submission_id":   res.SubmissionID,
		"requested_tier":  res.RequestedTier,
		"assigned_tier":   res.AssignedTier,
		"client_category": res.ClientCategory,
		"reverify_due_at": res.ReverifyDueAt,
	})
	return res, nil
}

// Reject runs the Compliance-Officer rejection: role gate → mandatory
// reason → atomic decision tx → kyc_rejected notification. Account
// tier is never touched on a rejection.
func (s *LifecycleService) Reject(ctx context.Context, actor ReviewActor, submissionID int64, reason string) (*DecisionResult, error) {
	if err := s.requireComplianceRole(ctx, actor, "kyc.reject"); err != nil {
		return nil, err
	}
	if submissionID <= 0 {
		return nil, excerrors.New("INVALID_REQUEST", "submission id must be positive")
	}
	if strings.TrimSpace(reason) == "" {
		return nil, excerrors.New("INVALID_REQUEST", "reject reason is required")
	}
	res, err := s.decide(ctx, submissionID, false, strings.TrimSpace(reason), actor)
	if err != nil {
		return nil, err
	}
	s.notify(ctx, res.AccountID, EventKYCRejected, map[string]any{
		"submission_id":  res.SubmissionID,
		"requested_tier": res.RequestedTier,
		"reason":         reason,
	})
	return res, nil
}

// decide resolves the tier policy (approve only) and runs the atomic
// store transition.
func (s *LifecycleService) decide(ctx context.Context, submissionID int64, approve bool, reason string, actor ReviewActor) (*DecisionResult, error) {
	p := DecisionTx{
		SubmissionID: submissionID,
		Approve:      approve,
		Reason:       reason,
		ReviewerID:   actor.AdminUserID,
		ClientIP:     actor.ClientIP,
		Now:          s.now().UTC(),
	}
	if approve {
		// The store reports the requested tier inside the tx; the policy
		// read happens first so a missing policy fails before any write.
		pol, err := s.policyForSubmission(ctx, submissionID)
		if err != nil {
			return nil, err
		}
		if pol != nil {
			p.ReverifyMonths = pol.ReverifyMonths
		}
	}
	res, err := s.store.DecideSubmissionTx(ctx, p)
	if err != nil {
		return nil, err
	}
	return res, nil
}

// policyForSubmission reads the submission's requested tier then the
// matching kyc_tier_policies row. A missing/closed submission surfaces
// as INVALID_REQUEST; a missing policy fails closed (the reverify
// horizon must come from the seeded matrix, never a guess).
func (s *LifecycleService) policyForSubmission(ctx context.Context, submissionID int64) (*TierPolicy, error) {
	sub, err := s.store.SubmissionByID(ctx, submissionID)
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "submission read", err)
	}
	if sub == nil {
		return nil, excerrors.New("INVALID_REQUEST",
			fmt.Sprintf("kyc submission %d not found", submissionID))
	}
	pol, err := s.store.TierPolicy(ctx, sub.RequestedTier)
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "tier policy read", err)
	}
	if pol == nil {
		return nil, excerrors.New("SERVICE_DEGRADED",
			fmt.Sprintf("no tier policy for %q — approve cannot compute reverify horizon", sub.RequestedTier))
	}
	return pol, nil
}

// SweepReverify is the hourly re-verification pass (Task 14.3.4 item
// 4): each overdue latest-APPROVED submission auto-downgrades its
// account T2→T1 (INSTITUTIONAL lapses also revert client_category →
// RETAIL and re-arm nbp — the ECP designation derived from the KYB),
// marks the submission EXPIRED, writes the audit row, then emits the
// compliance alert + user notification. Per-row failures are logged
// through the alerter and the pass continues — one poisoned row never
// stalls the cohort; the count of applied downgrades is returned.
func (s *LifecycleService) SweepReverify(ctx context.Context, limit int) (int, error) {
	rows, err := s.store.OverdueReverifications(ctx, s.now().UTC(), limit)
	if err != nil {
		return 0, excerrors.Wrap("SERVICE_DEGRADED", "reverify sweep feed", err)
	}
	applied := 0
	var firstErr error
	for _, r := range rows {
		ok, derr := s.store.DowngradeReverifyTx(ctx, DowngradeTx{OverdueReverify: r, Now: s.now().UTC()})
		if derr != nil {
			if firstErr == nil {
				firstErr = derr
			}
			s.alert(ctx, "P2", "KYC_REVERIFY_DOWNGRADE_FAILED",
				fmt.Sprintf("re-verify downgrade failed for account %d (submission %d): %v",
					r.AccountID, r.SubmissionID, derr))
			continue
		}
		if !ok {
			continue // concurrent transition already resolved the account
		}
		applied++
		s.alert(ctx, "P2", "KYC_REVERIFY_DOWNGRADE",
			fmt.Sprintf("account %d auto-downgraded T2→T1: re-verification overdue since %s (submission %d, tier %s)",
				r.AccountID, r.ReverifyDueAt.Format("2006-01-02"), r.SubmissionID, r.RequestedTier))
		s.notify(ctx, r.AccountID, EventKYCDowngraded, map[string]any{
			"submission_id":   r.SubmissionID,
			"requested_tier":  r.RequestedTier,
			"assigned_tier":   "T1",
			"reverify_due_at": r.ReverifyDueAt,
		})
	}
	return applied, firstErr
}

// alert raises the wired ops alert; a nil seam degrades silently — the
// admin_audit_log row written inside the downgrade tx is the durable
// record either way.
func (s *LifecycleService) alert(ctx context.Context, severity, code, summary string) {
	if s.alerter == nil {
		return
	}
	_ = s.alerter(ctx, severity, code, summary)
}
