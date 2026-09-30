package risk

// Task 19.5.3.6 — risk-side stale-price fallback: tier resolution,
// flash-freeze deferral, haircut pricing, STALE_MARK basis.

import (
	"context"
	"errors"
	"testing"

	"exchange/pkg/decimal"
)

// fakeStaleFb is the in-memory StaleFallbackSource for engine tests.
type fakeStaleFb struct {
	ref    *StaleFallbackRef
	frozen bool
}

func (f *fakeStaleFb) Reference(_ context.Context, _ string) (*StaleFallbackRef, error) {
	return f.ref, nil
}
func (f *fakeStaleFb) Frozen(_ context.Context, _ string) (bool, error) {
	return f.frozen, nil
}

func TestStaleRefPricingDirection(t *testing.T) {
	ref := StaleFallbackRef{
		Symbol: "EUR/USD", Tier: TierStaleShort,
		LastMark: decimal.RequireFromString("1.1000"), State: StaleFallbackPrice,
	}
	// Long liquidation sells at mark×0.98; short buys at mark×1.02 —
	// always pessimistic for the liquidated side.
	if px := ref.ReferencePrice(true); !px.Equal(decimal.RequireFromString("1.078")) {
		t.Fatalf("long ref %s, want 1.078", px)
	}
	if px := ref.ReferencePrice(false); !px.Equal(decimal.RequireFromString("1.122")) {
		t.Fatalf("short ref %s, want 1.122", px)
	}
}

func TestStaleFallbackFrozenDefers(t *testing.T) {
	svc := &LiquidationService{staleFb: &fakeStaleFb{frozen: true}}
	p := LiqPosition{ID: 1, AccountID: 7, Symbol: "EUR/USD", Side: "LONG",
		Quantity: decimal.NewFromInt(10), MarkPrice: decimal.One}
	_, _, err := svc.staleFallback(context.Background(), p, "EUR/USD")
	var frozen errFlashFrozen
	if !errors.As(err, &frozen) {
		t.Fatalf("frozen close must return errFlashFrozen, got %v", err)
	}
}

func TestStaleFallbackFreshPassthrough(t *testing.T) {
	pos := LiqPosition{ID: 1, AccountID: 7, Symbol: "EUR/USD", Side: "LONG",
		Quantity: decimal.NewFromInt(10), MarkPrice: decimal.RequireFromString("1.10")}
	// No source bound → stored mark, no ref (Phase-19 default).
	svc := &LiquidationService{}
	px, ref, err := svc.staleFallback(context.Background(), pos, "EUR/USD")
	if err != nil || ref != nil || !px.Equal(pos.MarkPrice) {
		t.Fatalf("unwired fallback must pass through: px=%s ref=%v err=%v", px, ref, err)
	}
	// Healthy oracle (nil ref) → same pass-through.
	svc = &LiquidationService{staleFb: &fakeStaleFb{}}
	px, ref, err = svc.staleFallback(context.Background(), pos, "EUR/USD")
	if err != nil || ref != nil || !px.Equal(pos.MarkPrice) {
		t.Fatalf("healthy ref must pass through: px=%s ref=%v err=%v", px, ref, err)
	}
	// Active ref → haircut-adjusted price.
	svc = &LiquidationService{staleFb: &fakeStaleFb{ref: &StaleFallbackRef{
		Symbol: "EUR/USD", Tier: TierStaleMedium, State: StaleFallbackPrice,
		LastMark: decimal.RequireFromString("1.10"),
	}}}
	px, ref, err = svc.staleFallback(context.Background(), pos, "EUR/USD")
	if err != nil || ref == nil {
		t.Fatalf("expected active ref, err=%v", err)
	}
	// Medium tier: long close at 1.10×0.95 = 1.045.
	if !px.Equal(decimal.RequireFromString("1.045")) {
		t.Fatalf("stale ref px %s, want 1.045 (5%% haircut)", px)
	}
}

func TestStaleFallbackTiersMirror(t *testing.T) {
	// The risk-side tier constants must track the oracle's rungs —
	// pinned here so a renumber on one side can't silently diverge
	// (the JSON wire carries the tier as an int).
	if !StaleFallbackTier(1).Haircut().Equal(decimal.RequireFromString("0.02")) ||
		!StaleFallbackTier(2).Haircut().Equal(decimal.RequireFromString("0.05")) ||
		!StaleFallbackTier(3).Haircut().Equal(decimal.RequireFromString("0.10")) {
		t.Fatal("tier haircut table drifted from Task 19.5.3.6 (2/5/10%)")
	}
	med := StaleFallbackRef{Tier: TierStaleMedium}
	short := StaleFallbackRef{Tier: TierStaleShort}
	lng := StaleFallbackRef{Tier: TierStaleLong}
	if !med.AuctionOnly() || short.AuctionOnly() {
		t.Fatal("auction-only boundary drifted (15s tier)")
	}
	if !lng.ForceCash() {
		t.Fatal("force-cash must engage on the 60s tier")
	}
}
