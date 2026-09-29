// target_market.go — Retail Product Target-Market Governance
// (Phase-14 Task 14.3.16; spec §14.12, §24 #376; migration 099).
//
// Every product profile carries a per-client-category MiFID II target
// market. The order gate (product_gate.go) reads it for RETAIL
// admission; this service owns the Compliance-Officer authoring and the
// ≤12-month periodic review:
//
//   - Upsert defines/refreshes the (profile, category) row — audit-logged.
//   - Review re-approves or narrows the market and rolls review_due_at
//     forward (≤ now + 12 months, enforced here — the bound cannot be a
//     timestamptz CHECK) — audit-logged.
//   - SweepOverdue flips APPROVED rows past review_due_at to
//     REVIEW_OVERDUE (close-only for that category's opens — the gate
//     also evaluates review_due_at lazily so a delayed sweep still
//     cannot admit) and raises a once-only ops alert per row.
package accounts

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"exchange/internal/admin"
	"exchange/internal/settlement"
)

// TargetMarket is one product_target_markets row.
type TargetMarket struct {
	ID                   int64      `json:"id"`
	ProfileID            int64      `json:"profile_id"`
	ClientCategory       string     `json:"client_category"`
	KnowledgeExperience  string     `json:"knowledge_experience"`
	RiskTolerance        string     `json:"risk_tolerance"`
	PositiveClasses      []string   `json:"positive_classes"`
	NegativeClasses      []string   `json:"negative_classes"`
	NegativeTarget       string     `json:"negative_target"`
	DistributionStrategy string     `json:"distribution_strategy"`
	Status               string     `json:"status"`
	ReviewDueAt          time.Time  `json:"review_due_at"`
	LastReviewedAt       time.Time  `json:"last_reviewed_at"`
	LastReviewedBy       *int64     `json:"last_reviewed_by,omitempty"`
	AlertedAt            *time.Time `json:"alerted_at,omitempty"`
	CreatedAt            time.Time  `json:"created_at"`
	UpdatedAt            time.Time  `json:"updated_at"`
}

// PositiveClass / NegativeClass test the class lists.
func (t *TargetMarket) PositiveClass(class string) bool {
	class = strings.ToUpper(strings.TrimSpace(class))
	for _, c := range t.PositiveClasses {
		if c == class {
			return true
		}
	}
	return false
}
func (t *TargetMarket) NegativeClass(class string) bool {
	class = strings.ToUpper(strings.TrimSpace(class))
	for _, c := range t.NegativeClasses {
		if c == class {
			return true
		}
	}
	return false
}

// Overdue reports whether the review window has elapsed — the gate
// consults it lazily (status REVIEW_OVERDUE is the swept projection; a
// row can be overdue before the sweeper marks it).
func (t *TargetMarket) Overdue(now time.Time) bool {
	return t.Status == TargetStatusReviewOverdue || !now.Before(t.ReviewDueAt)
}

// Knowledge/risk band vocabularies (migration 099 CHECK mirrors).
var (
	knowledgeBands = map[string]bool{"NONE": true, "BASIC": true, "INTERMEDIATE": true, "ADVANCED": true}
	riskBands      = map[string]bool{"LOW": true, "MEDIUM": true, "HIGH": true}
	distStrategies = map[string]bool{"ADVISED": true, "NON_ADVISED": true}
)

// clientCategories mirrors the §14.2 category enum without importing it.
var clientCategories = map[string]bool{
	"RETAIL": true, "PROFESSIONAL": true, "ELIGIBLE_COUNTERPARTY": true,
}

// ReviewHorizon is the MiFID II ≤12-month review bound.
const ReviewHorizon = 365 * 24 * time.Hour

// TargetMarketInput is the upsert/review payload.
type TargetMarketInput struct {
	ProfileID            int64    `json:"profile_id"`
	ClientCategory       string   `json:"client_category"`
	KnowledgeExperience  string   `json:"knowledge_experience"`
	RiskTolerance        string   `json:"risk_tolerance"`
	PositiveClasses      []string `json:"positive_classes"`
	NegativeClasses      []string `json:"negative_classes"`
	NegativeTarget       string   `json:"negative_target"`
	DistributionStrategy string   `json:"distribution_strategy"`
	ReviewDueAt          string   `json:"review_due_at"` // RFC3339; ≤ now+12mo
}

func (in *TargetMarketInput) validate(now time.Time) error {
	if in.ProfileID <= 0 {
		return newError(CodeInvalidRequest, "profile_id required")
	}
	in.ClientCategory = strings.ToUpper(strings.TrimSpace(in.ClientCategory))
	if !clientCategories[in.ClientCategory] {
		return errorf(CodeInvalidRequest,
			"client_category must be RETAIL|PROFESSIONAL|ELIGIBLE_COUNTERPARTY")
	}
	in.KnowledgeExperience = strings.ToUpper(strings.TrimSpace(in.KnowledgeExperience))
	if in.KnowledgeExperience == "" {
		in.KnowledgeExperience = "BASIC"
	}
	if !knowledgeBands[in.KnowledgeExperience] {
		return errorf(CodeInvalidRequest,
			"knowledge_experience must be NONE|BASIC|INTERMEDIATE|ADVANCED")
	}
	in.RiskTolerance = strings.ToUpper(strings.TrimSpace(in.RiskTolerance))
	if in.RiskTolerance == "" {
		in.RiskTolerance = "MEDIUM"
	}
	if !riskBands[in.RiskTolerance] {
		return errorf(CodeInvalidRequest, "risk_tolerance must be LOW|MEDIUM|HIGH")
	}
	pos, err := canonicalClasses(in.PositiveClasses)
	if err != nil {
		return err
	}
	if len(pos) == 0 {
		return newError(CodeInvalidRequest,
			"positive_classes must contain at least one instrument class")
	}
	in.PositiveClasses = pos
	neg, err := canonicalClasses(in.NegativeClasses)
	if err != nil {
		return err
	}
	in.NegativeClasses = neg
	// A class cannot be both in and out of the target market.
	for _, c := range neg {
		for _, p := range pos {
			if c == p {
				return errorf(CodeInvalidRequest,
					"class %s cannot appear in both positive and negative target", c)
			}
		}
	}
	in.DistributionStrategy = strings.ToUpper(strings.TrimSpace(in.DistributionStrategy))
	if in.DistributionStrategy == "" {
		in.DistributionStrategy = "NON_ADVISED"
	}
	if !distStrategies[in.DistributionStrategy] {
		return errorf(CodeInvalidRequest,
			"distribution_strategy must be ADVISED|NON_ADVISED")
	}
	due, err := time.Parse(time.RFC3339, strings.TrimSpace(in.ReviewDueAt))
	if err != nil {
		return errorf(CodeInvalidRequest,
			"review_due_at must be RFC3339 (≤ now + 12 months)")
	}
	if !due.After(now) || due.After(now.Add(ReviewHorizon)) {
		return errorf(CodeInvalidRequest,
			"review_due_at must be in the future and ≤ 12 months out")
	}
	in.ReviewDueAt = due.UTC().Format(time.RFC3339)
	return nil
}

// OpsAlerter is the durable ops-alert seam (reconciliation's
// DurableOpsAlerter / settlement's PublisherAlerter satisfy it); nil
// disables paging — the status flip itself still persists.
type OpsAlerter interface {
	Raise(ctx context.Context, a settlement.OpsAlert) error
}

// TargetMarketService owns the authoring/review/sweep lifecycle.
type TargetMarketService struct {
	pool     *pgxpool.Pool
	resolver RoleResolver
	alerter  OpsAlerter
	now      func() time.Time
}

// NewTargetMarketService wires the service; resolver nil fails the admin
// surface closed; alerter may be nil (status flips persist regardless).
func NewTargetMarketService(pool *pgxpool.Pool, resolver RoleResolver, alerter OpsAlerter) *TargetMarketService {
	return &TargetMarketService{pool: pool, resolver: resolver, alerter: alerter, now: time.Now}
}

func (s *TargetMarketService) requireRole(ctx context.Context, adminUserID int64) error {
	if s.resolver == nil {
		return newError(CodeUnauthorizedRole,
			"role resolver not configured — target-market administration rejected")
	}
	role, err := s.resolver(ctx, adminUserID)
	if err != nil {
		return errorf("INTERNAL_ERROR", "role lookup: %v", err)
	}
	if !profileAdminRoles[role] {
		return newError(CodeUnauthorizedRole,
			"target-market administration requires Compliance Officer or Super Admin")
	}
	return nil
}

// ForProfile is the gate read: newest row for (profile, category), nil
// when none exists (callers fail closed).
func (s *TargetMarketService) ForProfile(ctx context.Context, profileID int64, category string) (*TargetMarket, error) {
	return scanTargetMarket(s.pool.QueryRow(ctx, `
		SELECT id, profile_id, client_category, knowledge_experience,
		       risk_tolerance, positive_classes, negative_classes,
		       negative_target, distribution_strategy, status,
		       review_due_at, last_reviewed_at, last_reviewed_by,
		       alerted_at, created_at, updated_at
		  FROM product_target_markets
		 WHERE profile_id = $1 AND client_category = $2`, profileID, category))
}

func scanTargetMarket(row pgx.Row) (*TargetMarket, error) {
	var t TargetMarket
	err := row.Scan(&t.ID, &t.ProfileID, &t.ClientCategory,
		&t.KnowledgeExperience, &t.RiskTolerance, &t.PositiveClasses,
		&t.NegativeClasses, &t.NegativeTarget, &t.DistributionStrategy,
		&t.Status, &t.ReviewDueAt, &t.LastReviewedAt, &t.LastReviewedBy,
		&t.AlertedAt, &t.CreatedAt, &t.UpdatedAt)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, errorf("INTERNAL_ERROR", "target-market scan: %v", err)
	}
	return &t, nil
}

// List serves the admin review queue; filter "overdue" limits to rows
// needing review now (past due or swept REVIEW_OVERDUE).
func (s *TargetMarketService) List(ctx context.Context, actor AdminActor, overdueOnly bool) ([]TargetMarket, error) {
	if err := s.requireRole(ctx, actor.UserID); err != nil {
		return nil, err
	}
	q := `SELECT id, profile_id, client_category, knowledge_experience,
	             risk_tolerance, positive_classes, negative_classes,
	             negative_target, distribution_strategy, status,
	             review_due_at, last_reviewed_at, last_reviewed_by,
	             alerted_at, created_at, updated_at
	        FROM product_target_markets`
	args := []any{}
	if overdueOnly {
		q += ` WHERE status = 'REVIEW_OVERDUE' OR review_due_at <= now()`
	}
	q += ` ORDER BY review_due_at`
	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, errorf("INTERNAL_ERROR", "target-market list: %v", err)
	}
	defer rows.Close()
	var out []TargetMarket
	for rows.Next() {
		var t TargetMarket
		if err := rows.Scan(&t.ID, &t.ProfileID, &t.ClientCategory,
			&t.KnowledgeExperience, &t.RiskTolerance, &t.PositiveClasses,
			&t.NegativeClasses, &t.NegativeTarget, &t.DistributionStrategy,
			&t.Status, &t.ReviewDueAt, &t.LastReviewedAt, &t.LastReviewedBy,
			&t.AlertedAt, &t.CreatedAt, &t.UpdatedAt); err != nil {
			return nil, errorf("INTERNAL_ERROR", "target-market scan: %v", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// Upsert defines or refreshes the (profile, category) target market.
// Creating a row lands APPROVED with the ≤12-month review horizon.
func (s *TargetMarketService) Upsert(ctx context.Context, actor AdminActor, in TargetMarketInput) (*TargetMarket, error) {
	if err := s.requireRole(ctx, actor.UserID); err != nil {
		return nil, err
	}
	if err := in.validate(s.now().UTC()); err != nil {
		return nil, err
	}
	// The positive target can never exceed the profile's tradeable scope.
	prof, err := scanProfile(s.pool.QueryRow(ctx, `
		SELECT profile_id, code, pricing_plan, instrument_scope,
		       subunit_divisor, min_deposit::text, status, created_at, updated_at
		  FROM account_product_profiles WHERE profile_id = $1`, in.ProfileID))
	if err != nil {
		return nil, err
	}
	if prof == nil {
		return nil, errorf(CodeNotFound, "product profile %d not found", in.ProfileID)
	}
	for _, c := range in.PositiveClasses {
		if !prof.InScope(c) {
			return nil, errorf(CodeInvalidRequest,
				"positive class %s exceeds profile %s instrument scope", c, prof.Code)
		}
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, errorf("INTERNAL_ERROR", "begin tx: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var before *TargetMarket
	if b, berr := scanTargetMarket(tx.QueryRow(ctx, `
		SELECT id, profile_id, client_category, knowledge_experience,
		       risk_tolerance, positive_classes, negative_classes,
		       negative_target, distribution_strategy, status,
		       review_due_at, last_reviewed_at, last_reviewed_by,
		       alerted_at, created_at, updated_at
		  FROM product_target_markets
		 WHERE profile_id = $1 AND client_category = $2 FOR UPDATE`,
		in.ProfileID, in.ClientCategory)); berr != nil {
		return nil, berr
	} else {
		before = b
	}
	due, _ := time.Parse(time.RFC3339, in.ReviewDueAt)
	tm, err := scanTargetMarket(tx.QueryRow(ctx, `
		INSERT INTO product_target_markets
		    (profile_id, client_category, knowledge_experience,
		     risk_tolerance, positive_classes, negative_classes,
		     negative_target, distribution_strategy, status,
		     review_due_at, last_reviewed_at, last_reviewed_by)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,'APPROVED',$9,now(),$10)
		ON CONFLICT (profile_id, client_category) DO UPDATE SET
		     knowledge_experience = EXCLUDED.knowledge_experience,
		     risk_tolerance       = EXCLUDED.risk_tolerance,
		     positive_classes     = EXCLUDED.positive_classes,
		     negative_classes     = EXCLUDED.negative_classes,
		     negative_target      = EXCLUDED.negative_target,
		     distribution_strategy = EXCLUDED.distribution_strategy,
		     status               = 'APPROVED',
		     review_due_at        = EXCLUDED.review_due_at,
		     last_reviewed_at     = now(),
		     last_reviewed_by     = EXCLUDED.last_reviewed_by,
		     alerted_at           = NULL,
		     updated_at           = now()
		RETURNING id, profile_id, client_category, knowledge_experience,
		       risk_tolerance, positive_classes, negative_classes,
		       negative_target, distribution_strategy, status,
		       review_due_at, last_reviewed_at, last_reviewed_by,
		       alerted_at, created_at, updated_at`,
		in.ProfileID, in.ClientCategory, in.KnowledgeExperience,
		in.RiskTolerance, in.PositiveClasses, in.NegativeClasses,
		in.NegativeTarget, in.DistributionStrategy, due, actor.UserID))
	if err != nil {
		return nil, err
	}
	if _, _, err := admin.Log(ctx, tx, admin.AuditEntry{
		AdminUserID: actor.UserID,
		Action:      "target_market.upsert",
		TargetType:  "product_target_market",
		TargetID:    &tm.ID,
		BeforeState: before,
		AfterState:  tm,
		IPAddress:   actor.ClientIP,
	}); err != nil {
		return nil, errorf("INTERNAL_ERROR", "audit log: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, errorf("INTERNAL_ERROR", "commit: %v", err)
	}
	return tm, nil
}

// ReviewAction enumerates the review dispositions.
const (
	ReviewApprove = "APPROVE" // re-approve as-is, roll review_due_at
	ReviewNarrow  = "NARROW"  // re-approve with a narrowed market
	ReviewSuspend = "SUSPEND" // distribution suspended (gate rejects)
)

// Review re-approves or narrows the market per the periodic review
// workflow. narrow carries the revised TargetMarketInput when action is
// NARROW (other fields — profile/category/bands/strategy — revalidate);
// review_due_at is required on APPROVE/NARROW, ≤ now+12mo.
func (s *TargetMarketService) Review(ctx context.Context, actor AdminActor,
	id int64, action string, narrow *TargetMarketInput) (*TargetMarket, error) {
	if err := s.requireRole(ctx, actor.UserID); err != nil {
		return nil, err
	}
	action = strings.ToUpper(strings.TrimSpace(action))

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, errorf("INTERNAL_ERROR", "begin tx: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	before, err := scanTargetMarket(tx.QueryRow(ctx, `
		SELECT id, profile_id, client_category, knowledge_experience,
		       risk_tolerance, positive_classes, negative_classes,
		       negative_target, distribution_strategy, status,
		       review_due_at, last_reviewed_at, last_reviewed_by,
		       alerted_at, created_at, updated_at
		  FROM product_target_markets WHERE id = $1 FOR UPDATE`, id))
	if err != nil {
		return nil, err
	}
	if before == nil {
		return nil, errorf(CodeNotFound, "target-market row %d not found", id)
	}

	var (
		tm    *TargetMarket
		query string
		args  []any
	)
	switch action {
	case ReviewSuspend:
		query = `UPDATE product_target_markets
		            SET status='SUSPENDED', last_reviewed_at=now(),
		                last_reviewed_by=$2, updated_at=now()
		          WHERE id=$1
		     RETURNING id, profile_id, client_category, knowledge_experience,
		       risk_tolerance, positive_classes, negative_classes,
		       negative_target, distribution_strategy, status,
		       review_due_at, last_reviewed_at, last_reviewed_by,
		       alerted_at, created_at, updated_at`
		args = []any{id, actor.UserID}
	case ReviewApprove, ReviewNarrow:
		in := TargetMarketInput{
			ProfileID:            before.ProfileID,
			ClientCategory:       before.ClientCategory,
			KnowledgeExperience:  before.KnowledgeExperience,
			RiskTolerance:        before.RiskTolerance,
			PositiveClasses:      before.PositiveClasses,
			NegativeClasses:      before.NegativeClasses,
			NegativeTarget:       before.NegativeTarget,
			DistributionStrategy: before.DistributionStrategy,
		}
		if action == ReviewNarrow {
			if narrow == nil {
				return nil, newError(CodeInvalidRequest,
					"NARROW review requires the narrowed market payload")
			}
			in = *narrow
			in.ProfileID = before.ProfileID
			in.ClientCategory = before.ClientCategory // immutable axis
		} else {
			// APPROVE keeps the recorded market but must roll the review
			// horizon forward — the caller supplies the new due date on
			// the payload's review_due_at (≤ now + 12 months).
			if narrow == nil || strings.TrimSpace(narrow.ReviewDueAt) == "" {
				return nil, newError(CodeInvalidRequest,
					"APPROVE review requires a new review_due_at (≤ 12 months)")
			}
			in.ReviewDueAt = narrow.ReviewDueAt
		}
		if err := in.validate(s.now().UTC()); err != nil {
			return nil, err
		}
		due, _ := time.Parse(time.RFC3339, in.ReviewDueAt)
		query = `UPDATE product_target_markets
		            SET knowledge_experience=$2, risk_tolerance=$3,
		                positive_classes=$4, negative_classes=$5,
		                negative_target=$6, distribution_strategy=$7,
		                status='APPROVED', review_due_at=$8,
		                last_reviewed_at=now(), last_reviewed_by=$9,
		                alerted_at=NULL, updated_at=now()
		          WHERE id=$1
		     RETURNING id, profile_id, client_category, knowledge_experience,
		       risk_tolerance, positive_classes, negative_classes,
		       negative_target, distribution_strategy, status,
		       review_due_at, last_reviewed_at, last_reviewed_by,
		       alerted_at, created_at, updated_at`
		args = []any{id, in.KnowledgeExperience, in.RiskTolerance,
			in.PositiveClasses, in.NegativeClasses, in.NegativeTarget,
			in.DistributionStrategy, due, actor.UserID}
	default:
		return nil, errorf(CodeInvalidRequest,
			"action must be APPROVE|NARROW|SUSPEND")
	}
	tm, err = scanTargetMarket(tx.QueryRow(ctx, query, args...))
	if err != nil {
		return nil, err
	}
	if _, _, err := admin.Log(ctx, tx, admin.AuditEntry{
		AdminUserID: actor.UserID,
		Action:      "target_market.review." + strings.ToLower(action),
		TargetType:  "product_target_market",
		TargetID:    &id,
		BeforeState: before,
		AfterState:  tm,
		IPAddress:   actor.ClientIP,
	}); err != nil {
		return nil, errorf("INTERNAL_ERROR", "audit log: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, errorf("INTERNAL_ERROR", "commit: %v", err)
	}
	return tm, nil
}

// SweepOverdue flips APPROVED rows past review_due_at to REVIEW_OVERDUE
// and raises one ops alert per newly overdue row (alerted_at NULL → the
// row alerts once, re-approval clears the stamp). Returns rows flipped.
func (s *TargetMarketService) SweepOverdue(ctx context.Context, limit int) (int64, error) {
	if limit <= 0 {
		limit = 500
	}
	rows, err := s.pool.Query(ctx, `
		UPDATE product_target_markets
		   SET status='REVIEW_OVERDUE', alerted_at=now(), updated_at=now()
		 WHERE id IN (
		   SELECT id FROM product_target_markets
		    WHERE status='APPROVED' AND review_due_at <= now()
		    ORDER BY review_due_at LIMIT $1)
		 RETURNING id, profile_id, client_category`, limit)
	if err != nil {
		return 0, errorf("INTERNAL_ERROR", "overdue sweep: %v", err)
	}
	defer rows.Close()
	var n int64
	for rows.Next() {
		var id, profileID int64
		var cat string
		if err := rows.Scan(&id, &profileID, &cat); err != nil {
			return n, errorf("INTERNAL_ERROR", "overdue scan: %v", err)
		}
		n++
		if s.alerter != nil {
			_ = s.alerter.Raise(ctx, settlement.OpsAlert{
				Severity: settlement.SeverityP1,
				Code:     "TARGET_MARKET_REVIEW_OVERDUE",
				Summary:  "product_target_markets overdue — profile close-only for category opens",
				Details: map[string]string{
					"target_market_id": fmt.Sprintf("%d", id),
					"profile_id":       fmt.Sprintf("%d", profileID),
					"client_category":  cat,
				},
			})
		}
	}
	return n, rows.Err()
}
