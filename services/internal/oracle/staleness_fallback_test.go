package oracle

import (
	"testing"
	"time"

	"exchange/pkg/decimal"
)

// ---------------------------------------------------------------------------
// Task 19.5.3.6 — stale-price ladder + flash-crash breaker
// ---------------------------------------------------------------------------

func okResult(sym, px string, at time.Time) MarkResult {
	return MarkResult{Symbol: sym, Mark: decimal.RequireFromString(px), At: at, OK: true}
}
func failResult(sym string, at time.Time) MarkResult {
	return MarkResult{Symbol: sym, At: at, OK: false}
}

func TestTierLadder(t *testing.T) {
	cases := []struct {
		age      time.Duration
		wantTier StalenessTier
		wantH    string
	}{
		{4 * time.Second, TierFresh, "0"},
		{6 * time.Second, TierStaleShort, "0.02"},
		{30 * time.Second, TierStaleMedium, "0.05"},
		{90 * time.Second, TierStaleLong, "0.10"},
	}
	for _, c := range cases {
		got := TierStaleness(c.age)
		if got != c.wantTier {
			t.Fatalf("age %s → tier %v, want %v", c.age, got, c.wantTier)
		}
		if !got.HaircutPct().Equal(decimal.RequireFromString(c.wantH)) {
			t.Fatalf("tier %v haircut %s, want %s", got, got.HaircutPct(), c.wantH)
		}
	}
	if !TierStaleMedium.AuctionOnly() || TierStaleShort.AuctionOnly() {
		t.Fatal("auction-only must engage at ≥15s staleness")
	}
	if !TierStaleLong.ForceCash() || TierStaleMedium.ForceCash() {
		t.Fatal("force-cash must engage only past 60s")
	}
}

func TestFallbackTrackerStaleReference(t *testing.T) {
	t0 := time.Now()
	var now = t0
	tr := NewFallbackTracker(func() time.Time { return now })

	// Fresh marks anchor the baseline.
	if r := tr.Observe(okResult("EUR/USD", "1.1000", now), 0); r != nil {
		t.Fatal("fresh mark must not emit a fallback")
	}
	// Go stale at t0+10s (6s staleness → TierStaleShort, 2% haircut).
	now = t0.Add(10 * time.Second)
	ref := tr.Observe(failResult("EUR/USD", now), 6.0)
	if ref == nil || ref.State != FallbackStalePrice || ref.Tier != TierStaleShort {
		t.Fatalf("stale ref %+v, want STALE_PRICE/SHORT", ref)
	}
	// Long close: mark×0.98 = 1.0780
	if px := ref.ReferencePrice(true); !px.Equal(decimal.RequireFromString("1.078")) {
		t.Fatalf("long ref px %s, want 1.078", px)
	}
	// Short close: mark×1.02 = 1.1220
	if px := ref.ReferencePrice(false); !px.Equal(decimal.RequireFromString("1.122")) {
		t.Fatalf("short ref px %s, want 1.122", px)
	}
	// Age to 30s → medium tier.
	now = t0.Add(40 * time.Second)
	ref = tr.Observe(failResult("EUR/USD", now), 30.0)
	if ref.Tier != TierStaleMedium || !ref.Tier.AuctionOnly() {
		t.Fatalf("ref %+v, want MEDIUM/auction-only", ref)
	}
	// Recovery clears the episode.
	now = t0.Add(41 * time.Second)
	if r := tr.Observe(okResult("EUR/USD", "1.1005", now), 0); r != nil {
		t.Fatal("recovery must clear the fallback")
	}
}

func TestFlashCrashFreeze(t *testing.T) {
	t0 := time.Now()
	now := t0
	tr := NewFallbackTracker(func() time.Time { return now })

	// Mark prints 1.0000 then spikes to 1.0600 (>5%) inside 1s —
	// then feeds go stale at once (the signature the breaker arms on).
	tr.Observe(okResult("EUR/USD", "1.0000", now), 0)
	now = t0.Add(500 * time.Millisecond)
	tr.Observe(okResult("EUR/USD", "1.0600", now), 0)
	now = t0.Add(time.Second) // staleness onset 500ms after the spike
	ref := tr.Observe(failResult("EUR/USD", now), 6.0)
	if ref == nil || ref.State != FallbackFlashCool {
		t.Fatalf("expected FLASH_COOL freeze, got %+v", ref)
	}
	if !tr.Frozen("EUR/USD") {
		t.Fatal("tracker must report frozen during the cooling window")
	}
	// After the 5s cooling the ladder takes over — no re-freeze.
	now = now.Add(FlashCoolingPeriod + time.Millisecond)
	ref = tr.Observe(failResult("EUR/USD", now), 12.0)
	if ref == nil || ref.State != FallbackStalePrice {
		t.Fatalf("post-cooling ref %+v, want STALE_PRICE", ref)
	}
	if tr.Frozen("EUR/USD") {
		t.Fatal("freeze must lapse after the cooling period")
	}
}

func TestFlashCrashNotArmedOnSlowMove(t *testing.T) {
	t0 := time.Now()
	now := t0
	tr := NewFallbackTracker(func() time.Time { return now })

	// >5% move but 2s apart — outside the 1s window, no freeze.
	tr.Observe(okResult("EUR/USD", "1.0000", now), 0)
	now = t0.Add(2 * time.Second)
	tr.Observe(okResult("EUR/USD", "1.0600", now), 0)
	now = now.Add(6 * time.Second)
	ref := tr.Observe(failResult("EUR/USD", now), 6.0)
	if ref != nil && ref.State == FallbackFlashCool {
		t.Fatal("slow >5% move must not arm the flash breaker")
	}
}
