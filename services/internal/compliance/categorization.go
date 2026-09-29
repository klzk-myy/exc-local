// Phase-14 Task 14.3.7 — MiFID II client categorization &
// appropriateness (spec §5.2 accounts.client_category, §14.2, §24 #132).
//
// Client category is the regulatory axis orthogonal to kyc_tier (Task
// 14.3.4 verifies identity; this file classifies the client):
//
//	RETAIL                 — onboarding default; ESMA leverage caps,
//	                         negative-balance protection (§13.6c), no
//	                         binary options.
//	PROFESSIONAL           — elective/per-se; higher leverage, options
//	                         permitted, no NBP. Upgrade requires the
//	                         Compliance-Officer workflow + documented
//	                         MiFID II Annex II eligibility evidence.
//	ELIGIBLE_COUNTERPARTY  — institutional flow, negotiable terms; the
//	                         Task 14.3.4 INSTITUTIONAL approval path
//	                         assigns it automatically. No appropriateness
//	                         test required (MiFID II art. 30).
//
// Product gating (the §24 #132 rule, conservative mapping — see
// gateForClass): SPOT is exempt for every category; FORWARD/SWAP/NDF
// are leveraged classes requiring an unexpired appropriateness PASS for
// RETAIL and PROFESSIONAL; OPTION is PROFESSIONAL/ECP-only — migration
// 001's instrument_type carries no binary/vanilla subtype (Phase-22
// owns option_type), so every OPTION is conservatively held under the
// retail binary-option ban until the subtype lands and vanilla options
// can open to appropriateness-passed retail.
//
// Appropriateness assessments are recorded per (account,
// instrument_class): outcome derives server-side from score against
// AppropriatenessPassMark; expires_at = assessed_at + 12 months,
// enforced at evaluation time. The admission gate consults the LATEST
// assessment for the class — a newer FAIL supersedes an older PASS, so
// a stale or failed re-test revokes permission.
package compliance

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	excerrors "exchange/pkg/errors"
)

// ClientCategory mirrors accounts.client_category (migration 042).
type ClientCategory string

const (
	CategoryRetail       ClientCategory = "RETAIL"
	CategoryProfessional ClientCategory = "PROFESSIONAL"
	CategoryECP          ClientCategory = "ELIGIBLE_COUNTERPARTY"
)

// Valid reports whether c is a category the enum can hold.
func (c ClientCategory) Valid() bool {
	switch c {
	case CategoryRetail, CategoryProfessional, CategoryECP:
		return true
	}
	return false
}

// Instrument classes (mirroring instruments.instrument_type, migration
// 001). SPOT is declared here for the gate's exempt arm; the assessment
// vocabulary itself is the gated set only.
const (
	ClassSpot    = "SPOT"
	ClassForward = "FORWARD"
	ClassSwap    = "SWAP"
	ClassNDF     = "NDF"
	ClassOption  = "OPTION"
)

// AppropriatenessPassMark is the pass threshold on the 0–100 assessment
// score. Question-level grading semantics are a later refinement; the
// venue records the questionnaire verbatim (answers_json) and enforces
// this mark server-side so a client cannot self-declare PASS.
const AppropriatenessPassMark = 60

// AppropriatenessMonths is the 12-month assessment validity (MiFID II
// periodic re-assessment); expiry is enforced at evaluation time.
const AppropriatenessMonths = 12

// gatedClasses are the instrument classes an assessment may be recorded
// for (the appropriateness_assessments.instrument_class CHECK).
var gatedClasses = map[string]bool{
	ClassForward: true, ClassSwap: true, ClassNDF: true, ClassOption: true,
}

// Assessment mirrors one appropriateness_assessments row (migration
// 042). Answers stay server-side — the API never echoes the archived
// questionnaire back.
type Assessment struct {
	ID              int64           `json:"assessment_id"`
	AccountID       int64           `json:"account_id"`
	InstrumentClass string          `json:"instrument_class"`
	Outcome         string          `json:"outcome"` // PASS | FAIL
	Score           int             `json:"score"`
	Answers         json.RawMessage `json:"-"`
	AssessedAt      time.Time       `json:"assessed_at"`
	ExpiresAt       time.Time       `json:"expires_at"`
}

// CategoryStatus is the client-facing view for GET
// /api/v1/account/appropriateness: category, NBP entitlement and the
// recorded assessment set.
type CategoryStatus struct {
	AccountID      int64          `json:"account_id"`
	ClientCategory ClientCategory `json:"client_category"`
	NBP            bool           `json:"nbp"`
	Assessments    []Assessment   `json:"assessments"`
}

// CategoryChange is the audit-record payload for a category transition.
type CategoryChange struct {
	AccountID    int64          `json:"account_id"`
	From         ClientCategory `json:"from"`
	To           ClientCategory `json:"to"`
	NBP          bool           `json:"nbp"`
	Evidence     string         `json:"evidence,omitempty"`
	ChangedBy    int64          `json:"changed_by"`
	AuditSeq     int64          `json:"audit_seq"`
	OpenExposure bool           `json:"open_exposure"` // true = downgrade landed with open positions (close-only posture applies)
}

// ---------------------------------------------------------------------------
// Store seam — PgStore satisfies it; unit tests fake it
// ---------------------------------------------------------------------------

type CategoryStore interface {
	// ClientCategory reads accounts.client_category + accounts.nbp.
	// Missing account → ("", false, nil) → callers fail closed.
	ClientCategory(ctx context.Context, accountID int64) (category string, nbp bool, err error)
	// AccountUserID resolves accounts.user_id (audit attribution +
	// notification routing).
	AccountUserID(ctx context.Context, accountID int64) (int64, error)
	// LatestAssessment returns the newest assessment for the class —
	// any outcome — or nil when none exists.
	LatestAssessment(ctx context.Context, accountID int64, class string) (*Assessment, error)
	// InsertAssessment persists one assessment row and fills a.ID.
	InsertAssessment(ctx context.Context, a *Assessment) error
	// ListAssessments returns the account's assessments newest-first.
	ListAssessments(ctx context.Context, accountID int64, limit int) ([]Assessment, error)
	// SetCategoryTx atomically: locks the account, writes
	// client_category + nbp, detects open derivative positions for the
	// downgrade report, and appends the admin_audit_log row +
	// audit_hash_chain link. audit is the fully-populated admin entry
	// the service builds (before/after states marshaled by the writer).
	SetCategoryTx(ctx context.Context, p SetCategoryTx) (*CategoryChange, error)
}

// SetCategoryTx carries the store call inputs; the audit entry is
// opaque to this package — the store forwards it to the admin audit
// writer inside the same transaction.
type SetCategoryTx struct {
	AccountID  int64
	Category   ClientCategory
	NBP        bool
	ReviewerID int64
	ClientIP   string
	Evidence   string
	Now        time.Time
	// AuditBefore/AuditAfter are the JSON-marshalable state payloads
	// for admin_audit_log (the store owns the marshal+insert so the
	// audited row and the account write commit together).
	Action string
}

// ---------------------------------------------------------------------------
// Service
// ---------------------------------------------------------------------------

// RoleResolver resolves an admin user id to its §8.2 role name — the
// Phase-07 admin_role_bindings store satisfies it in production.
// The categorization/lifecycle services share this shape with
// api.AdminRoleResolver so the same resolver wires all three.
type RoleResolver func(ctx context.Context, adminUserID int64) (string, error)

// EligibleComplianceRoles may approve KYC reviews and set client
// categories — Compliance Officer or stronger (spec §8.2).
var EligibleComplianceRoles = map[string]bool{
	"Compliance Officer": true,
	"Super Admin":        true,
}

// CategorizationService owns category reads, the appropriateness gate
// and the Compliance-Officer category workflow.
type CategorizationService struct {
	store    CategoryStore
	resolver RoleResolver
	now      func() time.Time
}

// NewCategorizationService wires the service; resolver nil fails the
// admin surface closed (UNAUTHORIZED_ROLE), the read/gate paths keep
// working — the gate itself never consults roles.
func NewCategorizationService(store CategoryStore, resolver RoleResolver) (*CategorizationService, error) {
	if store == nil {
		return nil, fmt.Errorf("compliance: categorization store is nil")
	}
	return &CategorizationService{store: store, resolver: resolver, now: time.Now}, nil
}

// SetClockForTest overrides the clock; tests only.
func (s *CategorizationService) SetClockForTest(now func() time.Time) { s.now = now }

// Category resolves the account's MiFID II category. Fail-closed: a
// store error or an empty/unknown stored value surfaces an error rather
// than defaulting to RETAIL semantics silently.
func (s *CategorizationService) Category(ctx context.Context, accountID int64) (ClientCategory, error) {
	cat, _, err := s.store.ClientCategory(ctx, accountID)
	if err != nil {
		return "", fmt.Errorf("compliance: category read: %w", err)
	}
	c := ClientCategory(cat)
	if !c.Valid() {
		return "", excerrors.New("SERVICE_DEGRADED",
			fmt.Sprintf("client category unresolvable for account %d", accountID))
	}
	return c, nil
}

// NBP reports the account's negative-balance-protection entitlement —
// the persisted accounts.nbp flag Phase-19 Task 19.3.9 consumes
// (spec §13.6c; nbp = category='RETAIL', kept in sync by the audited
// write path). Fail-closed: unreadable ⇒ error, never an assumed flag.
func (s *CategorizationService) NBP(ctx context.Context, accountID int64) (bool, error) {
	cat, nbp, err := s.store.ClientCategory(ctx, accountID)
	if err != nil {
		return false, fmt.Errorf("compliance: nbp read: %w", err)
	}
	if !ClientCategory(cat).Valid() {
		return false, excerrors.New("SERVICE_DEGRADED",
			fmt.Sprintf("client category unresolvable for account %d", accountID))
	}
	return nbp, nil
}

// gateForClass documents the §24 #132 mapping; returns whether the
// class requires an appropriateness verdict at all, whether RETAIL is
// barred outright, and whether an unrecognized class was passed (the
// caller fails closed on it).
func gateForClass(class string) (gated, retailBarred, recognized bool) {
	switch class {
	case ClassSpot:
		return false, false, true // unleveraged spot FX exempt for every category
	case ClassForward, ClassSwap, ClassNDF:
		return true, false, true
	case ClassOption:
		return true, true, true // conservative §24 #132 — no binary/vanilla subtype yet
	}
	return true, true, false // unrecognized class → fail closed as gated+barred
}

// Appropriateness is the order-admission gate consumed by
// internal/orders (and later Phase-14 sibling clusters): nil = the
// account may trade this instrument class; otherwise a coded
// PRODUCT_NOT_PERMITTED / SERVICE_DEGRADED rejection. Resolution:
// SPOT passes for everyone; ELIGIBLE_COUNTERPARTY is exempt (art. 30);
// RETAIL is barred from OPTION outright; every other gated class needs
// the LATEST assessment to be PASS with expires_at still in the future.
func (s *CategorizationService) Appropriateness(ctx context.Context, accountID int64, instrumentClass string) error {
	class := strings.ToUpper(strings.TrimSpace(instrumentClass))
	gated, retailBarred, recognized := gateForClass(class)
	if !recognized {
		return excerrors.New("PRODUCT_NOT_PERMITTED",
			fmt.Sprintf("unrecognized instrument class %q — rejected (fail closed)", instrumentClass))
	}
	if !gated {
		return nil
	}
	cat, err := s.Category(ctx, accountID)
	if err != nil {
		return err // SERVICE_DEGRADED — an unresolvable category never admits
	}
	if cat == CategoryECP {
		return nil
	}
	if retailBarred && cat == CategoryRetail {
		return excerrors.New("PRODUCT_NOT_PERMITTED",
			fmt.Sprintf("%s instruments are restricted to PROFESSIONAL/ELIGIBLE_COUNTERPARTY clients (§24 #132)", class))
	}
	latest, err := s.store.LatestAssessment(ctx, accountID, class)
	if err != nil {
		return excerrors.Wrap("SERVICE_DEGRADED", "appropriateness read failed", err)
	}
	now := s.now().UTC()
	if latest == nil {
		return excerrors.New("PRODUCT_NOT_PERMITTED",
			fmt.Sprintf("appropriateness assessment required before trading %s instruments", class))
	}
	if latest.Outcome != "PASS" {
		return excerrors.New("PRODUCT_NOT_PERMITTED",
			fmt.Sprintf("latest %s appropriateness assessment outcome is FAIL", class))
	}
	if !latest.ExpiresAt.After(now) {
		return excerrors.New("PRODUCT_NOT_PERMITTED",
			fmt.Sprintf("%s appropriateness assessment expired %s — re-assessment required",
				class, latest.ExpiresAt.Format("2006-01-02")))
	}
	return nil
}

// AssessmentInput is one submitted questionnaire.
type AssessmentInput struct {
	AccountID       int64
	InstrumentClass string
	Score           int
	Answers         json.RawMessage // verbatim archive — optional shape
}

// SubmitAssessment records one appropriateness test: validates the
// gated class and score, derives outcome from the pass mark, and sets
// expires_at = assessed_at + 12 months.
func (s *CategorizationService) SubmitAssessment(ctx context.Context, in AssessmentInput) (*Assessment, error) {
	class := strings.ToUpper(strings.TrimSpace(in.InstrumentClass))
	if !gatedClasses[class] {
		return nil, excerrors.New("INVALID_REQUEST",
			fmt.Sprintf("instrument_class must be one of FORWARD|SWAP|NDF|OPTION (got %q)", in.InstrumentClass))
	}
	if in.Score < 0 || in.Score > 100 {
		return nil, excerrors.New("INVALID_REQUEST", "score must be 0–100")
	}
	if _, err := s.Category(ctx, in.AccountID); err != nil {
		return nil, err
	}
	now := s.now().UTC()
	outcome := "FAIL"
	if in.Score >= AppropriatenessPassMark {
		outcome = "PASS"
	}
	answers := in.Answers
	if len(answers) == 0 {
		answers = json.RawMessage("{}")
	}
	a := &Assessment{
		AccountID:       in.AccountID,
		InstrumentClass: class,
		Outcome:         outcome,
		Score:           in.Score,
		Answers:         answers,
		AssessedAt:      now,
		ExpiresAt:       now.AddDate(0, AppropriatenessMonths, 0),
	}
	if err := s.store.InsertAssessment(ctx, a); err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "assessment persist", err)
	}
	return a, nil
}

// Status builds the client-facing GET view: category, NBP flag and the
// newest-first assessment list (bounded).
func (s *CategorizationService) Status(ctx context.Context, accountID int64) (*CategoryStatus, error) {
	cat, nbp, err := s.store.ClientCategory(ctx, accountID)
	if err != nil {
		return nil, fmt.Errorf("compliance: category read: %w", err)
	}
	c := ClientCategory(cat)
	if !c.Valid() {
		return nil, excerrors.New("ACCOUNT_NOT_FOUND", "account not found")
	}
	list, err := s.store.ListAssessments(ctx, accountID, 50)
	if err != nil {
		return nil, fmt.Errorf("compliance: assessments read: %w", err)
	}
	return &CategoryStatus{AccountID: accountID, ClientCategory: c,
		NBP: nbp, Assessments: list}, nil
}

// SetCategory is the Compliance-Officer category workflow: role gate →
// evidence requirement for upgrades → atomic store write + admin audit.
// Downgrades (→ RETAIL) are permitted with open positions — the order
// gate then enforces close-only posture naturally; the change record
// reports open_exposure so the audit trail shows it.
func (s *CategorizationService) SetCategory(ctx context.Context, actor ReviewActor,
	accountID int64, category, evidence string) (*CategoryChange, error) {
	if s.resolver == nil {
		return nil, excerrors.New("UNAUTHORIZED_ROLE",
			"role resolver not configured — category changes rejected")
	}
	role, err := s.resolver(ctx, actor.AdminUserID)
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "role lookup", err)
	}
	if !EligibleComplianceRoles[role] {
		return nil, excerrors.New("UNAUTHORIZED_ROLE",
			"client categorization requires Compliance Officer or Super Admin")
	}
	to := ClientCategory(strings.ToUpper(strings.TrimSpace(category)))
	if !to.Valid() {
		return nil, excerrors.New("INVALID_REQUEST",
			"category must be RETAIL|PROFESSIONAL|ELIGIBLE_COUNTERPARTY")
	}
	evidence = strings.TrimSpace(evidence)
	if to != CategoryRetail && evidence == "" {
		return nil, excerrors.New("INVALID_REQUEST",
			"upgrade to "+string(to)+" requires documented MiFID II Annex II eligibility evidence")
	}
	ch, err := s.store.SetCategoryTx(ctx, SetCategoryTx{
		AccountID:  accountID,
		Category:   to,
		NBP:        to == CategoryRetail,
		ReviewerID: actor.AdminUserID,
		ClientIP:   actor.ClientIP,
		Evidence:   evidence,
		Now:        s.now().UTC(),
		Action:     "account.client_category",
	})
	if err != nil {
		return nil, err
	}
	return ch, nil
}
