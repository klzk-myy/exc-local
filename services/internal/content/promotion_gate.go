// Package content — Phase-21 Task 21.3.26: the customer-facing
// financial-promotion render gate.
//
// promotion_gate.go is the seam every consumer surface calls before
// showing promotional content (landing render, outbound email/push
// assembly, social post pull). It answers one question — "may this
// promotion version be shown right now?" — and fails closed:
//
//   - no row / unknown version → PROMOTION_NOT_APPROVED (410);
//   - status != APPROVED       → PROMOTION_NOT_APPROVED;
//   - approved_until elapsed   → PROMOTION_NOT_APPROVED (the row is
//     lazily flipped to EXPIRED by the
//     compliance sweep; the gate does not
//     depend on that sweep having run);
//   - store error              → PROMOTION_NOT_APPROVED (the gate never
//     serves unverifiable content).
//
// Marketing-consent checks (account_consent_states, Task 21.3.7) are a
// separate gate on the DELIVERY path — this gate is audience-agnostic
// content eligibility.
package content

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	excerrors "exchange/pkg/errors"
)

// Promotion is the renderable-content view of a head row.
type Promotion struct {
	PromotionID    int64      `json:"promotion_id"`
	Slug           string     `json:"slug"`
	Channel        string     `json:"channel"`
	BodyRef        string     `json:"body_ref"`
	Title          string     `json:"title"`
	Version        int        `json:"version"`
	ApprovalStatus string     `json:"approval_status"`
	ApprovedUntil  *time.Time `json:"approved_until,omitempty"`
}

// Store is the read surface the gate needs (Pg impl below; tests use
// the fixture).
type Store interface {
	PromotionForRender(ctx context.Context, id int64) (*Promotion, error)
	PromotionForRenderBySlug(ctx context.Context, slug string) (*Promotion, error)
}

// Gate serves promotion content eligibility decisions.
type Gate struct {
	store Store
	now   func() time.Time
}

// NewGate binds the store.
func NewGate(store Store) (*Gate, error) {
	if store == nil {
		return nil, fmt.Errorf("content: promotion gate store is nil")
	}
	return &Gate{store: store, now: time.Now}, nil
}

// SetClockForTest overrides the clock; tests only.
func (g *Gate) SetClockForTest(now func() time.Time) { g.now = now }

// Renderable returns the promotion iff it may be shown — APPROVED and
// unexpired. Everything else is PROMOTION_NOT_APPROVED (410).
func (g *Gate) Renderable(ctx context.Context, promotionID int64) (*Promotion, error) {
	p, err := g.store.PromotionForRender(ctx, promotionID)
	if err != nil {
		return nil, err
	}
	return g.check(p)
}

// RenderableSlug is the slug-keyed variant (landing pages reference
// slugs, not internal ids).
func (g *Gate) RenderableSlug(ctx context.Context, slug string) (*Promotion, error) {
	p, err := g.store.PromotionForRenderBySlug(ctx, slug)
	if err != nil {
		return nil, err
	}
	return g.check(p)
}

// CheckCached is the inline variant for callers that already fetched
// the row (cache-hit path — the check is pure and clock-driven).
func (g *Gate) CheckCached(p *Promotion) error {
	_, err := g.check(p)
	return err
}

func (g *Gate) check(p *Promotion) (*Promotion, error) {
	if p == nil {
		return nil, excerrors.New("PROMOTION_NOT_APPROVED",
			"promotion not found")
	}
	if p.ApprovalStatus != "APPROVED" {
		return nil, excerrors.New("PROMOTION_NOT_APPROVED",
			fmt.Sprintf("promotion %d is %s — only APPROVED content may render",
				p.PromotionID, p.ApprovalStatus))
	}
	if p.ApprovedUntil == nil || !p.ApprovedUntil.After(g.now().UTC()) {
		return nil, excerrors.New("PROMOTION_NOT_APPROVED",
			fmt.Sprintf("promotion %d approval expired or missing", p.PromotionID))
	}
	return p, nil
}

// ---------------------------------------------------------------------------
// Pg store (read-only mirror of the compliance-owned schema — the same
// mirror convention as internal/analytics marketing.go).
// ---------------------------------------------------------------------------

// PgPromotionStore reads financial_promotions.
type PgPromotionStore struct{ Pool *pgxpool.Pool }

// NewPgPromotionStore binds the pool.
func NewPgPromotionStore(pool *pgxpool.Pool) *PgPromotionStore {
	return &PgPromotionStore{Pool: pool}
}

const promoGateCols = `promotion_id, slug, channel, body_ref, title,
	version, approval_status, approved_until`

// PromotionForRender implements Store.
func (s *PgPromotionStore) PromotionForRender(ctx context.Context, id int64) (*Promotion, error) {
	var p Promotion
	err := s.Pool.QueryRow(ctx, `
		SELECT `+promoGateCols+` FROM financial_promotions
		 WHERE promotion_id=$1`, id).
		Scan(&p.PromotionID, &p.Slug, &p.Channel, &p.BodyRef, &p.Title,
			&p.Version, &p.ApprovalStatus, &p.ApprovedUntil)
	if err == pgx.ErrNoRows {
		return nil, excerrors.New("PROMOTION_NOT_APPROVED",
			fmt.Sprintf("promotion %d not found", id))
	}
	if err != nil {
		return nil, fmt.Errorf("content: promotion read: %w", err)
	}
	return &p, nil
}

// PromotionForRenderBySlug resolves the latest-version head for a slug.
func (s *PgPromotionStore) PromotionForRenderBySlug(ctx context.Context, slug string) (*Promotion, error) {
	var p Promotion
	err := s.Pool.QueryRow(ctx, `
		SELECT `+promoGateCols+` FROM financial_promotions
		 WHERE slug=$1 ORDER BY version DESC LIMIT 1`, slug).
		Scan(&p.PromotionID, &p.Slug, &p.Channel, &p.BodyRef, &p.Title,
			&p.Version, &p.ApprovalStatus, &p.ApprovedUntil)
	if err == pgx.ErrNoRows {
		return nil, excerrors.New("PROMOTION_NOT_APPROVED",
			fmt.Sprintf("promotion %q not found", slug))
	}
	if err != nil {
		return nil, fmt.Errorf("content: promotion read: %w", err)
	}
	return &p, nil
}
