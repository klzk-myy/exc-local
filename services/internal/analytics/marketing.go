// marketing.go — Phase-20 Task 20.3.16 (spec §16.11, §24 #381):
// marketing-operations reporting — promo inventory + consent cohorts.
//
// Owned-surface reporting only: no campaign attribution, no per-user
// engagement tracking, no purchased lists (referrals/affiliates stay
// out per rulings R4/R10 — the non-scope statement rides every
// response so audits stop requesting it while they stand).
//
// Store ownership boundary: both source tables are Phase-21 schema —
// financial_promotions (Task 21.3.26) and account_consents (Task
// 21.3.7). This package ships the read surface + Pg implementations;
// when the table is absent the store returns ErrSourceUnavailable and
// the handler emits a clean 503 with a source_unavailable note — the
// migration belongs to Phase-21 and is deliberately NOT created here.
package analytics

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	excerrors "exchange/pkg/errors"
)

// ErrMarketingSourceUnavailable marks the Phase-21-owned source
// relation being absent (42P01 undefined_table). The handler maps it to
// SERVICE_DEGRADED (503) with a "source_unavailable" details note.
var ErrMarketingSourceUnavailable = excerrors.New("SERVICE_DEGRADED",
	"source relation unavailable")

// MarketingNonScopeStatement is the fixed non-scope declaration carried
// on every response (task item 3).
const MarketingNonScopeStatement = "no campaign attribution, per-user tracking, or purchased lists — referrals/affiliates out of scope per R4/R10"

// ---------------------------------------------------------------------------
// Promo inventory (financial_promotions — Phase-21 Task 21.3.26)
// ---------------------------------------------------------------------------

// PromotionRow mirrors the prescribed financial_promotions shape
// (Phase-21 Task 21.3.26): approval_status ∈ DRAFT|APPROVED|EXPIRED|
// REJECTED; created_at may be absent in the landed schema — SLA fields
// treat NULL as "unevaluable" rather than guessing.
type PromotionRow struct {
	PromotionID    int64      `json:"promotion_id"`
	Channel        string     `json:"channel"`
	BodyRef        string     `json:"body_ref"`
	Version        int        `json:"version"`
	ApprovalStatus string     `json:"approval_status"`
	Approver       *int64     `json:"approver,omitempty"`
	CreatedAt      *time.Time `json:"created_at,omitempty"`
	ApprovedAt     *time.Time `json:"approved_at,omitempty"`
	ApprovedUntil  *time.Time `json:"approved_until,omitempty"`
}

// PromoInventoryStore lists the inventory rows.
type PromoInventoryStore interface {
	Promotions(ctx context.Context) ([]PromotionRow, error)
}

// ConsentCohort is one aggregated cohort cell — counts only, never
// identifiers (k-anonymity floor CohortFloor).
type ConsentCohort struct {
	Purpose  string `json:"purpose"`
	Channel  string `json:"channel"`
	State    string `json:"state"`
	Count    *int64 `json:"count,omitempty"` // nil when below the floor
	Result   string `json:"result"`          // "OK" | "INSUFFICIENT_COHORT"
	RowCount int64  `json:"-"`               // internal — suppressed below floor
}

// CohortFloor is the minimum cohort size a report may disclose
// (task: 100 — same guard family as Task 23.3.6).
const CohortFloor = 100

// ConsentCohortStore aggregates consent state. Jurisdiction axis is
// deferred: the prescribed Task 21.3.7 shape (account_consents:
// account_id, purpose, channel, state, updated_at) carries no
// jurisdiction column — cohorts group by (purpose, channel, state) and
// the report notes the deferral honestly.
type ConsentCohortStore interface {
	ConsentCohorts(ctx context.Context, purpose string) ([]ConsentCohort, error)
}

// ---------------------------------------------------------------------------
// Report assembly
// ---------------------------------------------------------------------------

// PromoReport is the marketing-ops payload.
type PromoReport struct {
	AsOf           time.Time        `json:"as_of"`
	ByStatus       map[string]int64 `json:"by_status"`
	ByChannel      map[string]int64 `json:"by_channel"`
	SLACompliant   int64            `json:"sla_compliant"`
	SLABreached    int64            `json:"sla_breached"`
	SLAUnevaluable int64            `json:"sla_unevaluable"` // no created_at anchor
	DraftsAging    int64            `json:"drafts_aging"`    // DRAFT older than the review window
	ExpiringIn30D  int64            `json:"expiring_in_30d"`
	ExpiringIn90D  int64            `json:"expiring_in_90d"`
	ExpiredFlagged int64            `json:"expired_flagged"` // APPROVED but approved_until already past —
	// content may still be served from cache (flagged; the Task 21.3.26
	// render gate owns enforcement)
	ReviewWindowDays int `json:"review_window_days"`
}

// MarketingReport is the full response: promo inventory + consent
// cohorts + the non-scope statement.
type MarketingReport struct {
	AsOf             time.Time       `json:"as_of"`
	Promotions       PromoReport     `json:"promotions"`
	ConsentCohorts   []ConsentCohort `json:"consent_cohorts"`
	CohortFloor      int64           `json:"cohort_floor"`
	JurisdictionNote string          `json:"jurisdiction_note"`
	NonScope         string          `json:"non_scope_statement"`
}

// MarketingReportService composes the two stores.
type MarketingReportService struct {
	promos   PromoInventoryStore
	consents ConsentCohortStore
	now      func() time.Time
	// ReviewWindow bounds DRAFT→APPROVED latency (business-calendar
	// approximation: calendar days — documented proxy since holiday
	// calendars are per-currency, not per-review).
	ReviewWindow time.Duration
}

// NewMarketingReportService wires the service; both stores are required
// (a partial wiring fails closed at the handler instead of half-filling
// a compliance read).
func NewMarketingReportService(p PromoInventoryStore, c ConsentCohortStore) (*MarketingReportService, error) {
	if p == nil || c == nil {
		return nil, fmt.Errorf("marketing: promo and consent stores are required")
	}
	return &MarketingReportService{
		promos: p, consents: c, now: time.Now,
		ReviewWindow: 5 * 24 * time.Hour, // 5-day default review window
	}, nil
}

// SetClockForTest overrides the clock; tests only.
func (s *MarketingReportService) SetClockForTest(now func() time.Time) {
	s.now = now
}

// Report builds the aggregate.
func (s *MarketingReportService) Report(ctx context.Context) (*MarketingReport, error) {
	now := s.now().UTC()
	promos, err := s.promos.Promotions(ctx)
	if err != nil {
		return nil, err
	}
	consents, err := s.consents.ConsentCohorts(ctx, "MARKETING")
	if err != nil {
		return nil, err
	}

	pr := PromoReport{
		AsOf: now, ByStatus: map[string]int64{}, ByChannel: map[string]int64{},
		ReviewWindowDays: int(s.ReviewWindow / (24 * time.Hour)),
	}
	for _, p := range promos {
		st := strings.ToUpper(p.ApprovalStatus)
		pr.ByStatus[st]++
		ch := strings.ToUpper(p.Channel)
		if ch == "" {
			ch = "UNSPECIFIED"
		}
		pr.ByChannel[ch]++
		switch st {
		case "APPROVED":
			switch {
			case p.ApprovedAt == nil:
				pr.SLAUnevaluable++ // status claims approval without a timestamp
			case p.CreatedAt == nil:
				pr.SLAUnevaluable++ // no anchor to measure latency
			case p.ApprovedAt.Sub(*p.CreatedAt) <= s.ReviewWindow:
				pr.SLACompliant++
			default:
				pr.SLABreached++
			}
			if p.ApprovedUntil != nil {
				rem := p.ApprovedUntil.Sub(now)
				switch {
				case rem < 0:
					pr.ExpiredFlagged++ // approved content past expiry — cache may still serve it
				case rem <= 30*24*time.Hour:
					pr.ExpiringIn30D++
					pr.ExpiringIn90D++
				case rem <= 90*24*time.Hour:
					pr.ExpiringIn90D++
				}
			}
		case "DRAFT":
			if p.CreatedAt != nil && now.Sub(*p.CreatedAt) > s.ReviewWindow {
				pr.DraftsAging++
			}
		}
	}

	for i := range consents {
		c := &consents[i]
		if c.RowCount < CohortFloor {
			c.Result = "INSUFFICIENT_COHORT"
			c.Count = nil // suppressed — counts-only with the floor
		} else {
			c.Result = "OK"
			v := c.RowCount
			c.Count = &v
		}
	}

	return &MarketingReport{
		AsOf:           now,
		Promotions:     pr,
		ConsentCohorts: consents,
		CohortFloor:    CohortFloor,
		JurisdictionNote: "cohorts group by (purpose, channel, state) — the " +
			"jurisdiction axis lands with the Task 21.3.7 consent schema",
		NonScope: MarketingNonScopeStatement,
	}, nil
}

// ---------------------------------------------------------------------------
// Pg implementations (Phase-21-owned relations — 503-with-note on absence)
// ---------------------------------------------------------------------------

// isMissingRelation reports whether err is PG 42P01 (undefined_table) —
// the Phase-21-owned source not yet landed.
func isMissingRelation(err error) bool {
	var pgerr *pgconn.PgError
	if errors.As(err, &pgerr) && pgerr.Code == "42P01" {
		return true
	}
	return err != nil && strings.Contains(err.Error(), "does not exist")
}

// PgPromoInventoryStore reads financial_promotions (Phase-21 Task
// 21.3.26 schema).
type PgPromoInventoryStore struct{ Pool *pgxpool.Pool }

// NewPgPromoInventoryStore binds the pool.
func NewPgPromoInventoryStore(pool *pgxpool.Pool) *PgPromoInventoryStore {
	return &PgPromoInventoryStore{Pool: pool}
}

// Promotions implements PromoInventoryStore; a missing relation
// degrades to ErrMarketingSourceUnavailable, never an empty report.
func (s *PgPromoInventoryStore) Promotions(ctx context.Context) ([]PromotionRow, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT promotion_id, channel, body_ref, version, approval_status,
		       approver, created_at, approved_at, approved_until
		  FROM financial_promotions ORDER BY promotion_id`)
	if err != nil {
		if isMissingRelation(err) {
			return nil, ErrMarketingSourceUnavailable
		}
		return nil, fmt.Errorf("marketing: promotions scan: %w", err)
	}
	defer rows.Close()
	out := []PromotionRow{}
	for rows.Next() {
		var p PromotionRow
		if err := rows.Scan(&p.PromotionID, &p.Channel, &p.BodyRef,
			&p.Version, &p.ApprovalStatus, &p.Approver,
			&p.CreatedAt, &p.ApprovedAt, &p.ApprovedUntil); err != nil {
			return nil, fmt.Errorf("marketing: promotion scan: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// PgConsentCohortStore reads account_consents (Phase-21 Task 21.3.7
// prescribed shape).
type PgConsentCohortStore struct{ Pool *pgxpool.Pool }

// NewPgConsentCohortStore binds the pool.
func NewPgConsentCohortStore(pool *pgxpool.Pool) *PgConsentCohortStore {
	return &PgConsentCohortStore{Pool: pool}
}

// ConsentCohorts aggregates by (purpose, channel, state); purpose="" is
// all purposes. Counts are raw — the service applies the k-anonymity
// floor so the store stays honest and testable.
func (s *PgConsentCohortStore) ConsentCohorts(ctx context.Context, purpose string) ([]ConsentCohort, error) {
	where := ""
	args := []any{}
	if purpose != "" {
		args = append(args, strings.ToUpper(purpose))
		where = "WHERE purpose = $1"
	}
	rows, err := s.Pool.Query(ctx, `
		SELECT purpose, COALESCE(NULLIF(channel,''),'UNSPECIFIED'),
		       state, COUNT(*)
		  FROM account_consents `+where+`
		 GROUP BY purpose, COALESCE(NULLIF(channel,''),'UNSPECIFIED'), state
		 ORDER BY 1, 2, 3`, args...)
	if err != nil {
		if isMissingRelation(err) {
			return nil, ErrMarketingSourceUnavailable
		}
		return nil, fmt.Errorf("marketing: consent cohorts: %w", err)
	}
	defer rows.Close()
	out := []ConsentCohort{}
	for rows.Next() {
		var c ConsentCohort
		if err := rows.Scan(&c.Purpose, &c.Channel, &c.State, &c.RowCount); err != nil {
			return nil, fmt.Errorf("marketing: cohort scan: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
