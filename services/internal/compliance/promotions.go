// promotions.go — Phase-21 Task 21.3.26: financial promotions &
// outbound-communications compliance.
//
// Spec surface: §14.10.3/§14.14 financial promotions (fair, clear, not
// misleading; risk warnings; balanced claims), §23
// PROMOTION_NOT_APPROVED (410). Scope: landing pages, ads, email/push
// marketing, social posts, pricing/performance claims. Affiliate and
// influencer content stays out of scope (rulings R4/R10).
//
// Model: financial_promotions is the versioned HEAD; every lifecycle
// transition appends an immutable financial_promotion_versions row.
// Approval requires a Compliance Officer; promotions containing
// pricing/performance/return claims additionally require a distinct
// second approver (dual control). Approval expiry is bounded at
// 12 months — the render gate (internal/content) refuses past
// approved_until.
package compliance

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"exchange/internal/audit"
	excerrors "exchange/pkg/errors"
)

// Promotion status values.
const (
	PromoStatusDraft         = "DRAFT"
	PromoStatusPendingReview = "PENDING_REVIEW"
	PromoStatusApproved      = "APPROVED"
	PromoStatusRejected      = "REJECTED"
	PromoStatusExpired       = "EXPIRED"
	PromoStatusWithdrawn     = "WITHDRAWN"
)

// Promotion channels (migration 246 CHECK list).
var promotionChannels = map[string]bool{
	"LANDING": true, "AD": true, "EMAIL": true,
	"PUSH": true, "SOCIAL": true, "IN_APP": true,
}

// PromoApprovalBound is the maximum approval validity (spec: 12 months).
const PromoApprovalBound = 365 * 24 * time.Hour

// Promotion is the HEAD row.
type Promotion struct {
	PromotionID     int64           `json:"promotion_id"`
	Slug            string          `json:"slug"`
	Channel         string          `json:"channel"`
	BodyRef         string          `json:"body_ref"`
	Title           string          `json:"title"`
	Version         int             `json:"version"`
	ApprovalStatus  string          `json:"approval_status"`
	ContainsClaim   bool            `json:"contains_claim"`
	Checklist       json.RawMessage `json:"checklist"`
	SubmittedBy     int64           `json:"submitted_by"`
	Approver        *int64          `json:"approver,omitempty"`
	SecondApprover  *int64          `json:"second_approver,omitempty"`
	ApprovedAt      *time.Time      `json:"approved_at,omitempty"`
	ApprovedUntil   *time.Time      `json:"approved_until,omitempty"`
	RejectedBy      *int64          `json:"rejected_by,omitempty"`
	RejectedAt      *time.Time      `json:"rejected_at,omitempty"`
	RejectionReason *string         `json:"rejection_reason,omitempty"`
	CreatedAt       time.Time       `json:"created_at"`
	UpdatedAt       time.Time       `json:"updated_at"`
}

// PromoChecklist is the approval evidence the Compliance Officer
// asserts — every element must be true to approve.
type PromoChecklist struct {
	RiskWarning   bool `json:"risk_warning"`    // prominent risk warning present
	CapitalAtRisk bool `json:"capital_at_risk"` // capital-at-risk statement present
	ClaimBasis    bool `json:"claim_basis"`     // every price/performance claim substantiated
	EntityDetails bool `json:"entity_details"`  // legal entity + regulator disclosure present
	FairClear     bool `json:"fair_clear"`      // fair/clear/not-misleading review done
}

// OK reports whether the checklist is approval-complete.
func (c PromoChecklist) OK() bool {
	return c.RiskWarning && c.CapitalAtRisk && c.ClaimBasis &&
		c.EntityDetails && c.FairClear
}

// PromotionService owns the approval workflow.
type PromotionService struct {
	pool     *pgxpool.Pool
	resolver HoldRoleResolver
	now      func() time.Time
}

// NewPromotionService binds dependencies.
func NewPromotionService(pool *pgxpool.Pool, resolver HoldRoleResolver) (*PromotionService, error) {
	if pool == nil || resolver == nil {
		return nil, fmt.Errorf("compliance: promotions requires pool and resolver")
	}
	return &PromotionService{pool: pool, resolver: resolver, now: time.Now}, nil
}

// SetClockForTest overrides the clock; tests only.
func (s *PromotionService) SetClockForTest(now func() time.Time) { s.now = now }

// ---------------------------------------------------------------------------
// Draft lifecycle
// ---------------------------------------------------------------------------

// PromotionInput is the create/update payload.
type PromotionInput struct {
	Slug          string
	Channel       string
	BodyRef       string
	Title         string
	ContainsClaim bool
}

func (in PromotionInput) validate() error {
	if strings.TrimSpace(in.Slug) == "" || strings.TrimSpace(in.BodyRef) == "" {
		return excerrors.New("INVALID_REQUEST", "slug and body_ref are required")
	}
	if !promotionChannels[strings.ToUpper(in.Channel)] {
		return excerrors.New("INVALID_REQUEST",
			"channel must be one of LANDING|AD|EMAIL|PUSH|SOCIAL|IN_APP")
	}
	return nil
}

// Create inserts a new promotion at version 1 / DRAFT and records the
// initial CREATED ledger row. Re-using a live slug bumps a NEW
// promotion version row instead (content revision under the same key).
func (s *PromotionService) Create(ctx context.Context, actor int64,
	in PromotionInput) (*Promotion, error) {
	if err := in.validate(); err != nil {
		return nil, err
	}
	slug := strings.TrimSpace(in.Slug)
	channel := strings.ToUpper(in.Channel)

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("promotions: create tx: %w", err)
	}
	defer tx.Rollback(ctx)

	// Slug re-use = new version on the existing head row.
	var existingID int64
	var existingVersion int
	var existingStatus string
	err = tx.QueryRow(ctx, `
		SELECT promotion_id, version, approval_status
		  FROM financial_promotions WHERE slug=$1
		 ORDER BY version DESC LIMIT 1 FOR UPDATE`, slug).
		Scan(&existingID, &existingVersion, &existingStatus)
	switch {
	case err == pgx.ErrNoRows:
		var id int64
		if err := tx.QueryRow(ctx, `
			INSERT INTO financial_promotions
			    (slug, channel, body_ref, title, version,
			     approval_status, contains_claim, submitted_by)
			VALUES ($1,$2,$3,$4,1,'DRAFT',$5,$6)
			RETURNING promotion_id`,
			slug, channel, in.BodyRef, in.Title, in.ContainsClaim, actor).
			Scan(&id); err != nil {
			return nil, fmt.Errorf("promotions: insert: %w", err)
		}
		if err := s.appendVersionTx(ctx, tx, id, 1, channel, in.BodyRef,
			in.Title, in.ContainsClaim, "CREATED", actor, nil, "initial draft"); err != nil {
			return nil, err
		}
		if _, err := audit.Append(ctx, tx, "financial_promotions", &id,
			"CREATED", nil); err != nil {
			return nil, fmt.Errorf("promotions: audit: %w", err)
		}
		if err := tx.Commit(ctx); err != nil {
			return nil, fmt.Errorf("promotions: create commit: %w", err)
		}
		return s.Get(ctx, id)
	case err != nil:
		return nil, fmt.Errorf("promotions: slug lookup: %w", err)
	default:
		// Revision on a live slug — only DRAFT/REJECTED heads accept a
		// new version; an APPROVED head requires WITHDRAW first so a
		// live claim is never silently swapped.
		if existingStatus == PromoStatusApproved {
			return nil, excerrors.New("INVALID_REQUEST",
				"approved promotion must be withdrawn before a revision")
		}
		newVer := existingVersion + 1
		if _, err := tx.Exec(ctx, `
			UPDATE financial_promotions
			   SET channel=$2, body_ref=$3, title=$4, version=$5,
			       approval_status='DRAFT', contains_claim=$6,
			       approver=NULL, second_approver=NULL, approved_at=NULL,
			       approved_until=NULL, updated_at=now()
			 WHERE promotion_id=$1`,
			existingID, channel, in.BodyRef, in.Title, newVer,
			in.ContainsClaim); err != nil {
			return nil, fmt.Errorf("promotions: revise: %w", err)
		}
		if err := s.appendVersionTx(ctx, tx, existingID, newVer, channel,
			in.BodyRef, in.Title, in.ContainsClaim, "REVISION", actor, nil,
			"content revision"); err != nil {
			return nil, err
		}
		if _, err := audit.Append(ctx, tx, "financial_promotions",
			&existingID, "REVISED", nil); err != nil {
			return nil, fmt.Errorf("promotions: audit: %w", err)
		}
		if err := tx.Commit(ctx); err != nil {
			return nil, fmt.Errorf("promotions: revise commit: %w", err)
		}
		return s.Get(ctx, existingID)
	}
}

// SubmitForReview moves DRAFT → PENDING_REVIEW (the author signals the
// draft is ready for compliance sign-off).
func (s *PromotionService) SubmitForReview(ctx context.Context,
	promotionID, actor int64) (*Promotion, error) {
	return s.mutate(ctx, promotionID, actor, "SUBMITTED",
		PromoStatusDraft, PromoStatusPendingReview, "")
}

// ---------------------------------------------------------------------------
// Approval — checklist + dual control for claims
// ---------------------------------------------------------------------------

// Approve marks PENDING_REVIEW (or DRAFT) → APPROVED. Claims require a
// distinct second approver. approvedUntil is optional; when supplied it
// must be ≤ now+12 months; when absent the approval expires at the
// 12-month bound.
func (s *PromotionService) Approve(ctx context.Context, promotionID,
	officer int64, checklist PromoChecklist, secondApprover *int64,
	approvedUntil *time.Time) (*Promotion, error) {
	if err := s.checkPromoRole(ctx, officer); err != nil {
		return nil, err
	}
	if !checklist.OK() {
		return nil, excerrors.New("INVALID_REQUEST",
			"approval checklist incomplete — risk warning, capital-at-risk, "+
				"claim basis, entity details and fair-clear review are mandatory")
	}
	now := s.now().UTC()
	bound := now.Add(PromoApprovalBound)
	until := bound
	if approvedUntil != nil {
		if approvedUntil.After(bound) {
			return nil, excerrors.New("INVALID_REQUEST",
				"approved_until exceeds the 12-month approval bound")
		}
		if !approvedUntil.After(now) {
			return nil, excerrors.New("INVALID_REQUEST",
				"approved_until must be in the future")
		}
		until = approvedUntil.UTC()
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("promotions: approve tx: %w", err)
	}
	defer tx.Rollback(ctx)

	p, err := s.lockPromotion(ctx, tx, promotionID)
	if err != nil {
		return nil, err
	}
	if p.ApprovalStatus != PromoStatusPendingReview &&
		p.ApprovalStatus != PromoStatusDraft {
		return nil, excerrors.New("INVALID_REQUEST",
			fmt.Sprintf("promotion %d is %s — approve requires DRAFT/PENDING_REVIEW",
				promotionID, p.ApprovalStatus))
	}
	if p.ContainsClaim {
		if secondApprover == nil || *secondApprover == officer {
			return nil, excerrors.New("DUAL_CONTROL_REQUIRED",
				"pricing/performance claims require a distinct second approver")
		}
		if err := s.checkPromoRole(ctx, *secondApprover); err != nil {
			return nil, err
		}
	}
	if _, err := tx.Exec(ctx, `
		UPDATE financial_promotions
		   SET approval_status='APPROVED', approver=$2, second_approver=$3,
		       approved_at=$4, approved_until=$5, checklist=$6,
		       updated_at=now()
		 WHERE promotion_id=$1`,
		promotionID, officer, secondApprover, now, until,
		mustJSON(checklist)); err != nil {
		return nil, fmt.Errorf("promotions: approve write: %w", err)
	}
	if err := s.appendVersionTx(ctx, tx, promotionID, p.Version, p.Channel,
		p.BodyRef, p.Title, p.ContainsClaim, "APPROVED", officer,
		checklist, ""); err != nil {
		return nil, err
	}
	if _, err := audit.Append(ctx, tx, "financial_promotions",
		&promotionID, "APPROVED", nil); err != nil {
		return nil, fmt.Errorf("promotions: approve audit: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("promotions: approve commit: %w", err)
	}
	return s.Get(ctx, promotionID)
}

// Reject marks the draft REJECTED with a mandatory reason.
func (s *PromotionService) Reject(ctx context.Context, promotionID,
	officer int64, reason string) (*Promotion, error) {
	if strings.TrimSpace(reason) == "" {
		return nil, excerrors.New("INVALID_REQUEST",
			"rejection reason required")
	}
	if err := s.checkPromoRole(ctx, officer); err != nil {
		return nil, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	p, err := s.lockPromotion(ctx, tx, promotionID)
	if err != nil {
		return nil, err
	}
	if p.ApprovalStatus != PromoStatusDraft &&
		p.ApprovalStatus != PromoStatusPendingReview {
		return nil, excerrors.New("INVALID_REQUEST",
			fmt.Sprintf("promotion %d is %s", promotionID, p.ApprovalStatus))
	}
	if _, err := tx.Exec(ctx, `
		UPDATE financial_promotions
		   SET approval_status='REJECTED', rejected_by=$2, rejected_at=now(),
		       rejection_reason=$3, updated_at=now()
		 WHERE promotion_id=$1`, promotionID, officer, reason); err != nil {
		return nil, err
	}
	if err := s.appendVersionTx(ctx, tx, promotionID, p.Version, p.Channel,
		p.BodyRef, p.Title, p.ContainsClaim, "REJECTED", officer, nil,
		reason); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return s.Get(ctx, promotionID)
}

// Withdraw pulls an APPROVED promotion (e.g. stale claim, regulator
// request) — the gate refuses it immediately.
func (s *PromotionService) Withdraw(ctx context.Context, promotionID,
	officer int64, reason string) (*Promotion, error) {
	if err := s.checkPromoRole(ctx, officer); err != nil {
		return nil, err
	}
	return s.mutate(ctx, promotionID, officer, "WITHDRAWN",
		PromoStatusApproved, PromoStatusWithdrawn, reason)
}

// ExpireSweep flips APPROVED promotions whose approved_until has passed
// to EXPIRED — the render gate independently refuses expired rows, so
// the sweep is ledger hygiene, not the enforcement point.
func (s *PromotionService) ExpireSweep(ctx context.Context) (int, error) {
	res, err := s.pool.Exec(ctx, `
		WITH expired AS (
		    UPDATE financial_promotions
		       SET approval_status='EXPIRED', updated_at=now()
		     WHERE approval_status='APPROVED' AND approved_until < now()
		    RETURNING promotion_id, version, channel, body_ref, title,
		              contains_claim)
		INSERT INTO financial_promotion_versions
		    (promotion_id, version, channel, body_ref, title,
		     contains_claim, decision, actor, note)
		SELECT promotion_id, version, channel, body_ref, title,
		       contains_claim, 'EXPIRED', 0, 'approval expiry sweep'
		  FROM expired`)
	if err != nil {
		return 0, fmt.Errorf("promotions: expire sweep: %w", err)
	}
	return int(res.RowsAffected()), nil
}

// mutate is the small shared transition helper for reject-free moves.
func (s *PromotionService) mutate(ctx context.Context, promotionID, actor int64,
	decision, wantStatus, toStatus, note string) (*Promotion, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	p, err := s.lockPromotion(ctx, tx, promotionID)
	if err != nil {
		return nil, err
	}
	if p.ApprovalStatus != wantStatus {
		return nil, excerrors.New("INVALID_REQUEST",
			fmt.Sprintf("promotion %d is %s — %s requires %s",
				promotionID, p.ApprovalStatus, decision, wantStatus))
	}
	if _, err := tx.Exec(ctx, `
		UPDATE financial_promotions SET approval_status=$2, updated_at=now()
		 WHERE promotion_id=$1`, promotionID, toStatus); err != nil {
		return nil, err
	}
	if err := s.appendVersionTx(ctx, tx, promotionID, p.Version, p.Channel,
		p.BodyRef, p.Title, p.ContainsClaim, decision, actor, nil, note); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return s.Get(ctx, promotionID)
}

// ---------------------------------------------------------------------------
// Read surface
// ---------------------------------------------------------------------------

const promoCols = `promotion_id, slug, channel, body_ref, title, version,
	approval_status, contains_claim, checklist, submitted_by, approver,
	second_approver, approved_at, approved_until, rejected_by,
	rejected_at, rejection_reason, created_at, updated_at`

// Get returns one promotion head.
func (s *PromotionService) Get(ctx context.Context, promotionID int64) (*Promotion, error) {
	var p Promotion
	err := s.pool.QueryRow(ctx, `
		SELECT `+promoCols+` FROM financial_promotions
		 WHERE promotion_id=$1`, promotionID).
		Scan(&p.PromotionID, &p.Slug, &p.Channel, &p.BodyRef, &p.Title,
			&p.Version, &p.ApprovalStatus, &p.ContainsClaim, &p.Checklist,
			&p.SubmittedBy, &p.Approver, &p.SecondApprover, &p.ApprovedAt,
			&p.ApprovedUntil, &p.RejectedBy, &p.RejectedAt,
			&p.RejectionReason, &p.CreatedAt, &p.UpdatedAt)
	if err == pgx.ErrNoRows {
		return nil, excerrors.New("PROMOTION_NOT_APPROVED",
			fmt.Sprintf("promotion %d not found", promotionID))
	}
	if err != nil {
		return nil, fmt.Errorf("promotions: get: %w", err)
	}
	return &p, nil
}

// List returns promotion heads (optional status filter).
func (s *PromotionService) List(ctx context.Context, status string,
	limit int) ([]Promotion, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	var rows pgx.Rows
	var err error
	if status != "" {
		rows, err = s.pool.Query(ctx, `
			SELECT `+promoCols+` FROM financial_promotions
			 WHERE approval_status=$1 ORDER BY promotion_id DESC LIMIT $2`,
			strings.ToUpper(status), limit)
	} else {
		rows, err = s.pool.Query(ctx, `
			SELECT `+promoCols+` FROM financial_promotions
			 ORDER BY promotion_id DESC LIMIT $1`, limit)
	}
	if err != nil {
		return nil, fmt.Errorf("promotions: list: %w", err)
	}
	defer rows.Close()
	out := []Promotion{}
	for rows.Next() {
		var p Promotion
		if err := rows.Scan(&p.PromotionID, &p.Slug, &p.Channel,
			&p.BodyRef, &p.Title, &p.Version, &p.ApprovalStatus,
			&p.ContainsClaim, &p.Checklist, &p.SubmittedBy, &p.Approver,
			&p.SecondApprover, &p.ApprovedAt, &p.ApprovedUntil,
			&p.RejectedBy, &p.RejectedAt, &p.RejectionReason,
			&p.CreatedAt, &p.UpdatedAt); err != nil {
			return nil, fmt.Errorf("promotions: list scan: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// internals
// ---------------------------------------------------------------------------

func (s *PromotionService) lockPromotion(ctx context.Context, tx pgx.Tx,
	promotionID int64) (*Promotion, error) {
	var p Promotion
	err := tx.QueryRow(ctx, `
		SELECT `+promoCols+` FROM financial_promotions
		 WHERE promotion_id=$1 FOR UPDATE`, promotionID).
		Scan(&p.PromotionID, &p.Slug, &p.Channel, &p.BodyRef, &p.Title,
			&p.Version, &p.ApprovalStatus, &p.ContainsClaim, &p.Checklist,
			&p.SubmittedBy, &p.Approver, &p.SecondApprover, &p.ApprovedAt,
			&p.ApprovedUntil, &p.RejectedBy, &p.RejectedAt,
			&p.RejectionReason, &p.CreatedAt, &p.UpdatedAt)
	if err == pgx.ErrNoRows {
		return nil, excerrors.New("PROMOTION_NOT_APPROVED",
			fmt.Sprintf("promotion %d not found", promotionID))
	}
	if err != nil {
		return nil, fmt.Errorf("promotions: lock: %w", err)
	}
	return &p, nil
}

func (s *PromotionService) appendVersionTx(ctx context.Context, tx pgx.Tx,
	promotionID int64, version int, channel, bodyRef, title string,
	containsClaim bool, decision string, actor int64,
	checklist any, note string) error {
	var cl []byte
	if checklist != nil {
		cl = mustJSON(checklist)
	} else {
		cl = []byte("{}")
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO financial_promotion_versions
		    (promotion_id, version, channel, body_ref, title,
		     contains_claim, decision, actor, checklist, note)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`,
		promotionID, version, channel, bodyRef, title, containsClaim,
		decision, actor, cl, note)
	if err != nil {
		return fmt.Errorf("promotions: version ledger: %w", err)
	}
	return nil
}

func (s *PromotionService) checkPromoRole(ctx context.Context, userID int64) error {
	role, err := s.resolver(ctx, userID)
	if err != nil {
		return excerrors.New("INTERNAL_ERROR", "role lookup: "+err.Error())
	}
	if role != "Compliance Officer" && role != "Super Admin" {
		return excerrors.New("UNAUTHORIZED_ROLE",
			"role "+role+" cannot approve promotions")
	}
	return nil
}
