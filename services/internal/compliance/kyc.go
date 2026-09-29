// Package compliance — Phase-12 Tasks 12.3.4 + 12.3.13.
//
// 12.3.4 KYC submission pipeline: POST /api/v1/kyc/submit accepts a
// multipart document set, virus-scans each file (VirusScanner seam —
// upload.go), stores the bytes in S3 with SSE-KMS (spec §24 #102, via
// internal/objectstore), and records a kyc_submissions row
// (PENDING_REVIEW) plus kyc_documents rows pointing at the object keys.
// accounts.kyc_tier remains 'T0' until review — Phase-14 Task 14.3.4
// owns approve/reject, auto-downgrade and re-verification enforcement;
// this package only computes the SLA + re-verify dates the reviewer and
// the Phase-14 sweeper consume.
//
// 12.3.13 operations matrix: kyc_tier_policies + kyc_ops_matrix
// (migration 204) carry the vendor × document-type × jurisdiction
// requirement grid, liveness/biometric flags, PEP/adverse-media rescreen
// cadences (DAILY for T2/INSTITUTIONAL, WEEKLY for T1 — supersedes the
// "on schedule" wording), document-expiry lead days, applicant
// risk-score step-up/decline thresholds and the 24h manual-review SLA.
// Requirements() is the query API: tier + jurisdiction → policy +
// document rows.
//
// Tax self-certification intake (W-8BEN / W-8BEN-E / W-9) lives in
// taxcerts.go — migration 205 tax_self_certifications is shaped for
// direct consumption by Phase-21 Task 21.3.22 CRS/FATCA reporting.
//
// §14.2 tier limits are enforced through the existing seam: migration
// 203 seeds tier-scoped risk_limits rows (T0: withdraw=0 + volume=0 —
// disabled; T1: $10K/$10K; T2: $100K/unlimited) which the already-wired
// risk.LimitsService.CheckOrder/CheckWithdrawal resolve via
// accounts.kyc_tier. INSTITUTIONAL deliberately has no tier row —
// limits are negotiated account-scoped rows set after manual review.
// Denomination caveat: the limits seam compares transaction-currency
// amounts, so the USD figures act as USD-par caps (exact for USD,
// approximate elsewhere until a converted check lands on the
// Phase-19.5 oracle seam).
package compliance

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"exchange/internal/objectstore"

	"github.com/shopspring/decimal"
)

// ---------------------------------------------------------------------------
// Tiers (spec §14.2)
// ---------------------------------------------------------------------------

const (
	TierT0            = "T0"            // email only — no trading, no withdrawals
	TierT1            = "T1"            // ID + address — $10K/day withdraw, $10K/day trading
	TierT2            = "T2"            // full KYC + source of funds — $100K/day withdraw
	TierInstitutional = "INSTITUTIONAL" // corporate KYB + manual review — negotiated
)

// TierRank orders the KYC ladder; -1 for unknown values (fail-closed
// comparisons treat -1 as the lowest rung).
func TierRank(tier string) int {
	switch tier {
	case TierT0:
		return 0
	case TierT1:
		return 1
	case TierT2:
		return 2
	case TierInstitutional:
		return 3
	}
	return -1
}

// RequestableTier reports whether a client may request this tier in a
// submission (T0 is the unverified default, never requested).
func RequestableTier(tier string) bool {
	switch tier {
	case TierT1, TierT2, TierInstitutional:
		return true
	}
	return false
}

// ---------------------------------------------------------------------------
// Domain types (mirror migrations 203/204)
// ---------------------------------------------------------------------------

// SubmissionStatus enumerates kyc_submissions.status values. Only
// PENDING_REVIEW/FAILED are written by this package — UNDER_REVIEW /
// APPROVED / REJECTED / APPEALED / EXPIRED are Phase-14 lifecycle writes.
const (
	SubPendingReview = "PENDING_REVIEW"
	SubUnderReview   = "UNDER_REVIEW"
	SubApproved      = "APPROVED"
	SubRejected      = "REJECTED"
	SubAppealed      = "APPEALED"
	SubFailed        = "FAILED"
	SubExpired       = "EXPIRED"
)

// Submission is one KYC intake record.
type Submission struct {
	ID            int64      `json:"id"`
	AccountID     int64      `json:"account_id"`
	RequestedTier string     `json:"requested_tier"`
	Status        string     `json:"status"`
	Jurisdiction  string     `json:"jurisdiction,omitempty"`
	RiskScore     int16      `json:"risk_score"`
	SubmittedAt   time.Time  `json:"submitted_at"`
	SLADueAt      time.Time  `json:"sla_due_at"`      // submitted_at + manual_review_sla_hours (24h)
	VerifiedAt    *time.Time `json:"verified_at"`     // Phase-14 write
	ReverifyDueAt *time.Time `json:"reverify_due_at"` // verified_at + reverify_months (Phase-14 sets)
	ReviewedAt    *time.Time `json:"reviewed_at"`
	RejectReason  string     `json:"reject_reason,omitempty"`
	AppealOf      *int64     `json:"appeal_of,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
	UpdatedAt     time.Time  `json:"updated_at"`
}

// Document is one kyc_documents row. ObjectKey/file_url stays server-side
// — the API never returns it.
type Document struct {
	ID           int64      `json:"id"`
	AccountID    int64      `json:"account_id"`
	SubmissionID int64      `json:"submission_id,omitempty"`
	Type         string     `json:"type"`
	ObjectKey    string     `json:"-"`
	Status       string     `json:"status"` // kyc_document_status_enum
	SHA256       string     `json:"sha256"`
	SizeBytes    int64      `json:"size_bytes"`
	SSEAlgorithm string     `json:"sse_algorithm"`
	VerifiedAt   *time.Time `json:"verified_at,omitempty"`
	CreatedAt    time.Time  `json:"created_at"`
}

// TierPolicy mirrors one kyc_tier_policies row.
type TierPolicy struct {
	Tier                 string           `json:"tier"`
	Description          string           `json:"description"`
	LivenessRequired     bool             `json:"liveness_required"`
	BiometricRequired    bool             `json:"biometric_required"`
	RescreenCadence      string           `json:"rescreen_cadence"` // NONE|WEEKLY|DAILY
	ReverifyMonths       int              `json:"reverify_months"`  // 0 = no periodic re-verification
	ManualReviewSLAHours int              `json:"manual_review_sla_hours"`
	StepUpScore          int16            `json:"step_up_score"`
	DeclineScore         int16            `json:"decline_score"`
	DailyWithdrawalUSD   *decimal.Decimal `json:"daily_withdrawal_usd"` // nil = negotiated
	DailyTradingUSD      *decimal.Decimal `json:"daily_trading_usd"`    // nil = unlimited
}

// MatrixRow mirrors one kyc_ops_matrix row.
type MatrixRow struct {
	ID               int64  `json:"id"`
	Tier             string `json:"tier"`
	Jurisdiction     string `json:"jurisdiction"`
	Vendor           string `json:"vendor"`
	DocumentType     string `json:"document_type"`
	DocGroup         string `json:"doc_group"`
	Required         bool   `json:"required"`
	MaxDocAgeDays    *int   `json:"max_doc_age_days,omitempty"`
	DocExpiryLeadDay *int   `json:"doc_expiry_lead_days,omitempty"`
	Notes            string `json:"notes,omitempty"`
}

// UploadDoc is one candidate document inside a submission — bytes already
// buffered by the handler (multipart) or the caller.
type UploadDoc struct {
	Type        string // matrix document_type, e.g. PASSPORT
	Filename    string
	ContentType string // declared MIME
	Data        []byte
}

// Requirements is the 12.3.13 query-API result: tier policy + merged
// document rows ('*' defaults overlaid with the jurisdiction's exact rows).
type Requirements struct {
	Policy    *TierPolicy `json:"policy"`
	Documents []MatrixRow `json:"documents"`
}

// StatusView is the GET /api/v1/kyc/status payload. ClientCategory/NBP
// surface the Task 14.3.7 MiFID II categorization state (migration 042)
// so a client can see its regulatory classification beside its tier.
type StatusView struct {
	AccountID        int64       `json:"account_id"`
	Tier             string      `json:"tier"`
	ClientCategory   string      `json:"client_category"`
	NBP              bool        `json:"nbp"`
	Policy           *TierPolicy `json:"policy,omitempty"`
	LatestSubmission *Submission `json:"latest_submission,omitempty"`
	Documents        []Document  `json:"documents"`
}

// ---------------------------------------------------------------------------
// Store — persistence seam (PgStore in store.go; tests fake it)
// ---------------------------------------------------------------------------

type Store interface {
	AccountTier(ctx context.Context, accountID int64) (string, error)
	// ClientCategory reads accounts.client_category + nbp (migration 042,
	// Task 14.3.7) for the status projection; the same seam the
	// categorization store declares — one read path, not two.
	ClientCategory(ctx context.Context, accountID int64) (category string, nbp bool, err error)
	CreateSubmission(ctx context.Context, sub *Submission) error
	AttachDocument(ctx context.Context, doc *Document) error
	FailSubmission(ctx context.Context, submissionID int64) error
	LatestSubmission(ctx context.Context, accountID int64) (*Submission, error)
	ListAccountDocuments(ctx context.Context, accountID int64) ([]Document, error)
	TierPolicy(ctx context.Context, tier string) (*TierPolicy, error)
	Matrix(ctx context.Context, tier, jurisdiction string) ([]MatrixRow, error)
	OverdueReviews(ctx context.Context, now time.Time, limit int) ([]Submission, error)
	// Tax self-certifications (taxcerts.go)
	InsertSelfCert(ctx context.Context, c *SelfCert) error
	ListSelfCerts(ctx context.Context, accountID int64) ([]SelfCert, error)
	HasSelfCert(ctx context.Context, accountID int64) (bool, error)
}

// ---------------------------------------------------------------------------
// Service
// ---------------------------------------------------------------------------

// Fail-closed error sentinels — the API layer maps ErrValidation →
// INVALID_REQUEST (400) and every other failure → SERVICE_DEGRADED.
var (
	ErrValidation   = errors.New("compliance: validation")
	ErrScanner      = errors.New("compliance: virus scan")
	ErrInfected     = errors.New("compliance: document rejected by virus scan")
	ErrObjectsUnset = errors.New("compliance: document store not configured")
	ErrTierTooLow   = errors.New("compliance: account tier below required")
)

// ObjectPutter is the narrow slice of objectstore.Client the submit path
// needs (Put + Delete for failed-write cleanup).
type ObjectPutter interface {
	Put(ctx context.Context, in objectstore.PutInput) (objectstore.Object, error)
	Delete(ctx context.Context, key string) error
	Bucket() string
}

// Service wires the KYC intake pipeline.
type Service struct {
	store    Store
	objects  ObjectPutter // nil = uploads unavailable (fail closed), reads still work
	scanner  VirusScanner // nil = CleanPassScanner (dev seam)
	kmsKeyID string
	now      func() time.Time
}

// NewService wires the service. objects may be nil (submit degrades,
// status/requirements stay live); scanner nil → dev CleanPassScanner.
func NewService(store Store, objects ObjectPutter, kmsKeyID string, scanner VirusScanner) *Service {
	if scanner == nil {
		scanner = CleanPassScanner{}
	}
	return &Service{store: store, objects: objects, kmsKeyID: kmsKeyID,
		scanner: scanner, now: time.Now}
}

// SetClockForTest overrides the clock; tests only.
func (s *Service) SetClockForTest(now func() time.Time) { s.now = now }

// ComputeReverifyDue returns verified_at + reverify_months for the tier,
// or nil when the policy carries no periodic re-verification (T0/T1).
// Pure computation — the Phase-14 approval path sets the column and its
// sweeper enforces it; this package never transitions status.
func ComputeReverifyDue(p *TierPolicy, verifiedAt time.Time) *time.Time {
	if p == nil || p.ReverifyMonths <= 0 {
		return nil
	}
	d := verifiedAt.AddDate(0, p.ReverifyMonths, 0)
	return &d
}

// StatusView builds GET /api/v1/kyc/status.
func (s *Service) StatusView(ctx context.Context, accountID int64) (*StatusView, error) {
	tier, err := s.store.AccountTier(ctx, accountID)
	if err != nil {
		return nil, err
	}
	cat, nbp, err := s.store.ClientCategory(ctx, accountID)
	if err != nil {
		return nil, err
	}
	pol, err := s.store.TierPolicy(ctx, tier)
	if err != nil {
		return nil, err
	}
	latest, err := s.store.LatestSubmission(ctx, accountID)
	if err != nil {
		return nil, err
	}
	docs, err := s.store.ListAccountDocuments(ctx, accountID)
	if err != nil {
		return nil, err
	}
	for i := range docs {
		docs[i].ObjectKey = "" // never leak object keys
	}
	return &StatusView{
		AccountID: accountID, Tier: tier, ClientCategory: cat, NBP: nbp,
		Policy: pol, LatestSubmission: latest, Documents: docs,
	}, nil
}

// Requirements answers the 12.3.13 query API: tier (+optional
// jurisdiction/document_type/vendor filters) → policy + matrix rows.
// documentType/vendor empty = unfiltered; jurisdiction empty = merged
// default+'exact' grid for the account's resolved jurisdiction.
func (s *Service) Requirements(ctx context.Context, tier, jurisdiction string) (*Requirements, error) {
	tier = strings.ToUpper(strings.TrimSpace(tier))
	if tier == "" {
		tier = TierT1
	}
	pol, err := s.store.TierPolicy(ctx, tier)
	if err != nil {
		return nil, err
	}
	if pol == nil {
		return nil, fmt.Errorf("%w: unknown tier %q", ErrValidation, tier)
	}
	rows, err := s.store.Matrix(ctx, tier, strings.ToUpper(strings.TrimSpace(jurisdiction)))
	if err != nil {
		return nil, err
	}
	return &Requirements{Policy: pol, Documents: rows}, nil
}

// OverdueReviews is the Task-12.3.13 ListOverdueReview query —
// PENDING_REVIEW submissions past sla_due_at (submitted_at + 24h SLA).
// The enforcement/escalation sweeper is Phase-14 scope; this is the
// data feed it consumes.
func (s *Service) OverdueReviews(ctx context.Context, limit int) ([]Submission, error) {
	return s.store.OverdueReviews(ctx, s.now(), limit)
}
