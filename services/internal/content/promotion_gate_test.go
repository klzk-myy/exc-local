// Phase-21 Task 21.3.26 — promotion render gate: APPROVED + unexpired
// is the only renderable state; every other path is
// PROMOTION_NOT_APPROVED (the store returns the head row, the gate owns
// the decision).
package content

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	excerrors "exchange/pkg/errors"
)

type fixtureStore struct {
	byID   map[int64]*Promotion
	bySlug map[string]*Promotion
}

func (f *fixtureStore) PromotionForRender(_ context.Context, id int64) (*Promotion, error) {
	p, ok := f.byID[id]
	if !ok {
		return nil, fmt.Errorf("not found")
	}
	return p, nil
}

func (f *fixtureStore) PromotionForRenderBySlug(_ context.Context, slug string) (*Promotion, error) {
	p, ok := f.bySlug[slug]
	if !ok {
		return nil, fmt.Errorf("not found")
	}
	return p, nil
}

func testGate(t *testing.T) (*Gate, *fixtureStore, time.Time) {
	t.Helper()
	fx := &fixtureStore{byID: map[int64]*Promotion{}, bySlug: map[string]*Promotion{}}
	g, err := NewGate(fx)
	if err != nil {
		t.Fatalf("gate: %v", err)
	}
	now := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	g.SetClockForTest(func() time.Time { return now })
	return g, fx, now
}

func codeOf(err error) string {
	var e *excerrors.Error
	if errors.As(err, &e) {
		return e.Code
	}
	return ""
}

func TestGateApprovedUnexpiredRenders(t *testing.T) {
	g, fx, now := testGate(t)
	until := now.Add(30 * 24 * time.Hour)
	fx.byID[7] = &Promotion{PromotionID: 7, Slug: "promo", Channel: "EMAIL",
		BodyRef: "ref", Version: 2, ApprovalStatus: "APPROVED", ApprovedUntil: &until}
	p, err := g.Renderable(context.Background(), 7)
	if err != nil || p.PromotionID != 7 {
		t.Fatalf("approved+unexpired must render: %v", err)
	}
}

func TestGateRefusesNonApproved(t *testing.T) {
	g, fx, now := testGate(t)
	until := now.Add(time.Hour)
	for _, st := range []string{"DRAFT", "PENDING_REVIEW", "REJECTED", "EXPIRED", "WITHDRAWN"} {
		fx.byID[1] = &Promotion{PromotionID: 1, ApprovalStatus: st, ApprovedUntil: &until}
		if _, err := g.Renderable(context.Background(), 1); codeOf(err) != "PROMOTION_NOT_APPROVED" {
			t.Fatalf("status %s must refuse with PROMOTION_NOT_APPROVED, got %v", st, err)
		}
	}
}

func TestGateRefusesExpiredApproval(t *testing.T) {
	g, fx, now := testGate(t)
	past := now.Add(-time.Minute)
	fx.byID[9] = &Promotion{PromotionID: 9, ApprovalStatus: "APPROVED", ApprovedUntil: &past}
	if _, err := g.Renderable(context.Background(), 9); codeOf(err) != "PROMOTION_NOT_APPROVED" {
		t.Fatalf("expired approval must refuse, got %v", err)
	}
	// APPROVED with no approved_until is likewise unrenderable —
	// approval evidence is never optional.
	fx.byID[9].ApprovedUntil = nil
	if _, err := g.Renderable(context.Background(), 9); codeOf(err) != "PROMOTION_NOT_APPROVED" {
		t.Fatalf("missing approved_until must refuse, got %v", err)
	}
}

func TestGateSlugPathAndCheckCached(t *testing.T) {
	g, fx, now := testGate(t)
	until := now.Add(time.Hour)
	p := &Promotion{PromotionID: 3, Slug: "winter", ApprovalStatus: "APPROVED",
		ApprovedUntil: &until}
	fx.bySlug["winter"] = p
	if _, err := g.RenderableSlug(context.Background(), "winter"); err != nil {
		t.Fatalf("slug render: %v", err)
	}
	if err := g.CheckCached(p); err != nil {
		t.Fatalf("cached check: %v", err)
	}
	p.ApprovalStatus = "DRAFT"
	if err := g.CheckCached(p); codeOf(err) != "PROMOTION_NOT_APPROVED" {
		t.Fatalf("cached draft must refuse, got %v", err)
	}
}
