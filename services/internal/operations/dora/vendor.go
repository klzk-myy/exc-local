// DORA Art. 28 ICT third-party register (Phase-09 Task 9.3.15 item 4,
// spec §19.5, §24 #171): the vendor register as a managed data store —
// provider CRUD, review/renewal/substitution-test audit events, and a
// due-diligence sweep that pages when renewals approach, reviews go
// overdue, or a HIGH/MEDIUM-concentration exit plan goes untested.
package dora

import (
	"context"
	"fmt"
	"strings"
	"time"

	excerrors "exchange/pkg/errors"
)

// Concentration tiers (register contract: concentration risk column).
const (
	ConcLow    = "LOW"
	ConcMedium = "MEDIUM"
	ConcHigh   = "HIGH"
)

// Provider lifecycle states.
const (
	ProviderActive  = "ACTIVE"
	ProviderExiting = "EXITING"
	ProviderRetired = "RETIRED"
)

// Review event kinds (ict_provider_reviews.kind).
const (
	ReviewKindReview           = "REVIEW"
	ReviewKindRenewal          = "RENEWAL"
	ReviewKindSubstitutionTest = "SUBSTITUTION_TEST"
	ReviewKindConcentration    = "CONCENTRATION_REASSESS"
	ReviewKindExitPlan         = "EXIT_PLAN"
)

// Review outcomes.
const (
	ReviewPass     = "PASS"
	ReviewFail     = "FAIL"
	ReviewAccepted = "ACCEPTED"
	ReviewDeferred = "DEFERRED"
)

// Vendor sweep thresholds.
const (
	// RenewalAlertLeadDays — page when renewal_at approaches inside the
	// termination-notice runway (30d default when no notice recorded).
	RenewalAlertLeadDays = 30
	// SubstTestStaleDays — a HIGH/MEDIUM exit plan untested for a year
	// is a remediation item (docs/ops/dora-third-party-register.md §3).
	SubstTestStaleDays = 365
)

// Provider is one register row (migration 285).
type Provider struct {
	ID                      int64
	Name                    string
	ICTService              string
	FunctionsSupported      string
	LocationsSubcontractors string
	Concentration           string // LOW | MEDIUM | HIGH
	ContractTerms           string
	TerminationNoticeDays   *int
	ExitStrategy            string
	SubstitutionPlan        string
	LastSubstitutionTestAt  *time.Time
	RenewalAt               *time.Time
	NextReviewAt            *time.Time
	Owner                   string
	Status                  string // ACTIVE | EXITING | RETIRED
	Notes                   string
	CreatedAt, UpdatedAt    time.Time
}

// Review is one audit event on a provider (ict_provider_reviews).
type Review struct {
	ID          int64
	ProviderID  int64
	Kind        string
	Outcome     string
	EvidenceRef string
	Notes       string
	Actor       *int64
	ReviewedAt  time.Time
}

// VendorAlert is one actionable finding from Sweep.
type VendorAlert struct {
	Code       string // ICT_REVIEW_OVERDUE | ICT_RENEWAL_DUE | ICT_EXIT_TEST_STALE
	ProviderID int64
	Name       string
	Summary    string
	DueAt      time.Time
}

// VendorStore is the persistence seam (PgxStore implements it over
// migration 285; tests substitute an in-memory store).
type VendorStore interface {
	InsertProvider(ctx context.Context, p *Provider) (int64, error)
	UpdateProvider(ctx context.Context, p *Provider) (bool, error)
	SetProviderStatus(ctx context.Context, id int64, status string,
		at time.Time) (bool, error)
	GetProvider(ctx context.Context, id int64) (*Provider, bool, error)
	ListProviders(ctx context.Context, status string) ([]Provider, error)
	InsertReview(ctx context.Context, r *Review) (int64, error)
	ListReviews(ctx context.Context, providerID int64) ([]Review, error)
	// TouchReviewSchedule persists review-driven state transitions:
	// next review date + last substitution-test stamp.
	SetProviderSchedule(ctx context.Context, id int64, nextReview *time.Time,
		lastTest *time.Time) error
}

// VendorService owns the register workflow.
type VendorService struct {
	store VendorStore
	now   func() time.Time
}

// NewVendorService wires the service over the store seam.
func NewVendorService(store VendorStore) (*VendorService, error) {
	if store == nil {
		return nil, excerrors.New("INTERNAL_ERROR", "dora: vendor store not wired")
	}
	return &VendorService{store: store,
		now: func() time.Time { return time.Now().UTC() }}, nil
}

// SetClockForTest swaps the clock.
func (s *VendorService) SetClockForTest(now func() time.Time) {
	s.now = now
}

func validConcentration(c string) bool {
	return c == ConcLow || c == ConcMedium || c == ConcHigh
}
func validProviderStatus(st string) bool {
	return st == ProviderActive || st == ProviderExiting ||
		st == ProviderRetired
}
func validReviewKind(k string) bool {
	switch k {
	case ReviewKindReview, ReviewKindRenewal, ReviewKindSubstitutionTest,
		ReviewKindConcentration, ReviewKindExitPlan:
		return true
	}
	return false
}
func validReviewOutcome(o string) bool {
	switch o {
	case ReviewPass, ReviewFail, ReviewAccepted, ReviewDeferred:
		return true
	}
	return false
}

// Create registers a provider row.
func (s *VendorService) Create(ctx context.Context, p *Provider) (*Provider, error) {
	p.Name = strings.TrimSpace(p.Name)
	p.ICTService = strings.TrimSpace(p.ICTService)
	if p.Name == "" || p.ICTService == "" {
		return nil, excerrors.New("INVALID_REQUEST",
			"provider name and ict_service are required")
	}
	if p.Concentration == "" {
		p.Concentration = ConcMedium
	}
	if !validConcentration(p.Concentration) {
		return nil, excerrors.New("INVALID_REQUEST",
			"concentration must be LOW, MEDIUM or HIGH")
	}
	if p.Status == "" {
		p.Status = ProviderActive
	}
	if !validProviderStatus(p.Status) {
		return nil, excerrors.New("INVALID_REQUEST",
			"status must be ACTIVE, EXITING or RETIRED")
	}
	// Art. 28 fail-closed: a HIGH/MEDIUM concentration row requires a
	// documented exit strategy at registration.
	if p.Concentration != ConcLow && strings.TrimSpace(p.ExitStrategy) == "" {
		return nil, excerrors.New("INVALID_REQUEST",
			"HIGH/MEDIUM concentration requires an exit_strategy")
	}
	id, err := s.store.InsertProvider(ctx, p)
	if err != nil {
		return nil, fmt.Errorf("dora: provider insert: %w", err)
	}
	p.ID = id
	return p, nil
}

// Update amends a register row (contract terms, exit plan, cadence).
func (s *VendorService) Update(ctx context.Context, p *Provider) (*Provider, error) {
	if p.ID == 0 {
		return nil, excerrors.New("INVALID_REQUEST", "provider id required")
	}
	cur, ok, err := s.store.GetProvider(ctx, p.ID)
	if err != nil {
		return nil, fmt.Errorf("dora: provider lookup: %w", err)
	}
	if !ok {
		return nil, excerrors.New("INVALID_REQUEST", fmt.Sprintf("provider %d not found", p.ID))
	}
	if cur.Status == ProviderRetired {
		return nil, excerrors.New("INVALID_REQUEST",
			fmt.Sprintf("provider %d is RETIRED", p.ID))
	}
	if p.Concentration != "" && !validConcentration(p.Concentration) {
		return nil, excerrors.New("INVALID_REQUEST",
			"concentration must be LOW, MEDIUM or HIGH")
	}
	ok, err = s.store.UpdateProvider(ctx, p)
	if err != nil {
		return nil, fmt.Errorf("dora: provider update: %w", err)
	}
	if !ok {
		return nil, excerrors.New("INVALID_REQUEST",
			fmt.Sprintf("provider %d not found", p.ID))
	}
	return p, nil
}

// Retire moves a provider to RETIRED (terminal).
func (s *VendorService) Retire(ctx context.Context, id int64, actor int64) error {
	ok, err := s.store.SetProviderStatus(ctx, id, ProviderRetired, s.now())
	if err != nil {
		return fmt.Errorf("dora: provider retire: %w", err)
	}
	if !ok {
		return excerrors.New("INVALID_REQUEST", fmt.Sprintf("provider %d not found", id))
	}
	return nil
}

// List returns register rows ("" or a status filter).
func (s *VendorService) List(ctx context.Context, status string) ([]Provider, error) {
	if status != "" && !validProviderStatus(status) {
		return nil, excerrors.New("INVALID_REQUEST",
			"status must be ACTIVE, EXITING or RETIRED")
	}
	return s.store.ListProviders(ctx, status)
}

// Get returns one row.
func (s *VendorService) Get(ctx context.Context, id int64) (*Provider, error) {
	p, ok, err := s.store.GetProvider(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("dora: provider lookup: %w", err)
	}
	if !ok {
		return nil, excerrors.New("INVALID_REQUEST", fmt.Sprintf("provider %d not found", id))
	}
	return p, nil
}

// RecordReview appends an audit event and advances the review cadence:
// a PASS/ACCEPTED REVIEW or RENEWAL pushes next_review_at +1y; a
// SUBSTITUTION_TEST refreshes last_substitution_test_at.
func (s *VendorService) RecordReview(ctx context.Context, r *Review) (*Review, error) {
	if r.ProviderID == 0 {
		return nil, excerrors.New("INVALID_REQUEST", "provider_id required")
	}
	if !validReviewKind(r.Kind) {
		return nil, excerrors.New("INVALID_REQUEST",
			"kind must be REVIEW, RENEWAL, SUBSTITUTION_TEST, CONCENTRATION_REASSESS or EXIT_PLAN")
	}
	if !validReviewOutcome(r.Outcome) {
		return nil, excerrors.New("INVALID_REQUEST",
			"outcome must be PASS, FAIL, ACCEPTED or DEFERRED")
	}
	p, ok, err := s.store.GetProvider(ctx, r.ProviderID)
	if err != nil {
		return nil, fmt.Errorf("dora: provider lookup: %w", err)
	}
	if !ok {
		return nil, excerrors.New("INVALID_REQUEST",
			fmt.Sprintf("provider %d not found", r.ProviderID))
	}
	if r.ReviewedAt.IsZero() {
		r.ReviewedAt = s.now()
	}
	id, err := s.store.InsertReview(ctx, r)
	if err != nil {
		return nil, fmt.Errorf("dora: review insert: %w", err)
	}
	r.ID = id
	// Cadence bookkeeping — a completed review/renewal resets the annual
	// clock; a substitution test refreshes the exit-plan evidence stamp.
	if r.Outcome == ReviewPass || r.Outcome == ReviewAccepted {
		var nextReview, lastTest *time.Time
		if r.Kind == ReviewKindReview || r.Kind == ReviewKindRenewal {
			nr := r.ReviewedAt.AddDate(1, 0, 0)
			nextReview = &nr
		}
		if r.Kind == ReviewKindSubstitutionTest {
			lastTest = &r.ReviewedAt
		}
		if nextReview != nil || lastTest != nil {
			if err := s.store.SetProviderSchedule(ctx, p.ID,
				nextReview, lastTest); err != nil {
				return nil, fmt.Errorf("dora: schedule update: %w", err)
			}
		}
	}
	return r, nil
}

// Reviews lists a provider's audit events.
func (s *VendorService) Reviews(ctx context.Context, providerID int64) ([]Review, error) {
	return s.store.ListReviews(ctx, providerID)
}

// Sweep finds due register obligations on ACTIVE rows:
//   - next_review_at overdue           → ICT_REVIEW_OVERDUE (P2)
//   - renewal_at inside the notice     → ICT_RENEWAL_DUE (P2)
//     runway (termination_notice_days,
//     default 30d)
//   - HIGH/MEDIUM exit plan untested   → ICT_EXIT_TEST_STALE (P2)
//     for > 12 months (or never)
func (s *VendorService) Sweep(ctx context.Context) ([]VendorAlert, error) {
	rows, err := s.store.ListProviders(ctx, ProviderActive)
	if err != nil {
		return nil, fmt.Errorf("dora: vendor sweep list: %w", err)
	}
	now := s.now()
	var out []VendorAlert
	for _, p := range rows {
		if p.NextReviewAt != nil && !p.NextReviewAt.After(now) {
			out = append(out, VendorAlert{
				Code: "ICT_REVIEW_OVERDUE", ProviderID: p.ID, Name: p.Name,
				Summary: fmt.Sprintf("%s (%s): annual review overdue since %s",
					p.Name, p.ICTService, p.NextReviewAt.Format("2006-01-02")),
				DueAt: *p.NextReviewAt,
			})
		}
		if p.RenewalAt != nil {
			lead := RenewalAlertLeadDays
			if p.TerminationNoticeDays != nil &&
				*p.TerminationNoticeDays > lead {
				lead = *p.TerminationNoticeDays
			}
			if p.RenewalAt.Before(now.AddDate(0, 0, lead)) {
				out = append(out, VendorAlert{
					Code: "ICT_RENEWAL_DUE", ProviderID: p.ID, Name: p.Name,
					Summary: fmt.Sprintf("%s (%s): contract renewal %s inside %dd notice runway",
						p.Name, p.ICTService,
						p.RenewalAt.Format("2006-01-02"), lead),
					DueAt: *p.RenewalAt,
				})
			}
		}
		if p.Concentration != ConcLow {
			stale := p.LastSubstitutionTestAt == nil ||
				p.LastSubstitutionTestAt.Before(now.AddDate(-1, 0, 0))
			if stale {
				when := time.Time{}
				if p.LastSubstitutionTestAt != nil {
					when = *p.LastSubstitutionTestAt
				}
				out = append(out, VendorAlert{
					Code: "ICT_EXIT_TEST_STALE", ProviderID: p.ID, Name: p.Name,
					Summary: fmt.Sprintf("%s (%s): %s-concentration exit plan substitution test stale (last: %s)",
						p.Name, p.ICTService, p.Concentration,
						when.Format("2006-01-02")),
					DueAt: when,
				})
			}
		}
	}
	return out, nil
}
