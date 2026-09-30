// volatility_scaler_test.go — Task 19.3.28 volatility scaler tests.
// Coverage: realized-vol computation, 2×-baseline gate, ×1.5 cap,
// neutral default, feed-failure retention + paging, margin-path wiring
// (used_margin × multiplier).
package risk

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"exchange/pkg/decimal"
)

// ---------------------------------------------------------------------------
// Fakes
// ---------------------------------------------------------------------------

type tickVolSrcFake struct {
	mu   sync.Mutex
	mids map[string][]decimal.Decimal
	err  error
}

func (f *tickVolSrcFake) RecentMids(_ context.Context, sym string, _ time.Time) ([]decimal.Decimal, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	return f.mids[sym], nil
}

// zigzag builds a mid series alternating ±pct — log returns stddev ≈ pct.
func zigzag(base, pct float64, n int) []decimal.Decimal {
	out := make([]decimal.Decimal, 0, n)
	px := base
	for i := 0; i < n; i++ {
		out = append(out, decimal.NewFromFloat(px))
		if i%2 == 0 {
			px *= (1 + pct)
		} else {
			px /= (1 + pct)
		}
	}
	return out
}

func newScaler(t *testing.T, src *tickVolSrcFake, alerts *opsAlertSpy) *VolatilityScaler {
	t.Helper()
	s, err := NewVolatilityScaler(VolatilityScalerDeps{
		Source:          src,
		Alerter:         alerts,
		DefaultBaseline: 0.004, // 40bp per-return baseline
	})
	if err != nil {
		t.Fatalf("volatility scaler: %v", err)
	}
	return s
}

// ---------------------------------------------------------------------------
// Scaling semantics
// ---------------------------------------------------------------------------

func TestVolatilityScalerNeutralBelowGate(t *testing.T) {
	src := &tickVolSrcFake{mids: map[string][]decimal.Decimal{
		"EUR/USD": zigzag(1.20, 0.004, 12), // stddev ≈ baseline → ratio 1
	}}
	s := newScaler(t, src, nil)
	s.Refresh(context.Background(), []string{"EUR/USD"})
	if m := s.IMMultiplier("EUR/USD"); m != 1.0 {
		t.Fatalf("multiplier = %v, want 1.0 below 2× gate", m)
	}
}

func TestVolatilityScalerScalesAboveGate(t *testing.T) {
	src := &tickVolSrcFake{mids: map[string][]decimal.Decimal{
		"EUR/USD": zigzag(1.20, 0.010, 12), // ratio ≈ 2.5 → mult ≈ 1.25
	}}
	s := newScaler(t, src, nil)
	s.Refresh(context.Background(), []string{"EUR/USD"})
	m := s.IMMultiplier("EUR/USD")
	if m <= 1.0 || m > 1.5 {
		t.Fatalf("multiplier = %v, want (1.0, 1.5]", m)
	}
	if m < 1.2 {
		t.Fatalf("ratio ≈2.5 should give ≈1.25, got %v", m)
	}
}

func TestVolatilityScalerCapAt1Point5(t *testing.T) {
	src := &tickVolSrcFake{mids: map[string][]decimal.Decimal{
		"EUR/USD": zigzag(1.20, 0.030, 12), // ratio ≈ 7.5 → capped
	}}
	s := newScaler(t, src, nil)
	s.Refresh(context.Background(), []string{"EUR/USD"})
	if m := s.IMMultiplier("EUR/USD"); m != 1.5 {
		t.Fatalf("multiplier = %v, want 1.5 cap", m)
	}
}

func TestVolatilityScalerUnknownSymbolNeutral(t *testing.T) {
	s := newScaler(t, &tickVolSrcFake{mids: map[string][]decimal.Decimal{}}, nil)
	if m := s.IMMultiplier("GBP/USD"); m != 1.0 {
		t.Fatalf("uncomputed symbol must be neutral, got %v", m)
	}
}

func TestVolatilityScalerNoSamplesKeepsPrior(t *testing.T) {
	src := &tickVolSrcFake{mids: map[string][]decimal.Decimal{
		"EUR/USD": zigzag(1.20, 0.020, 12),
	}}
	s := newScaler(t, src, nil)
	ctx := context.Background()
	s.Refresh(ctx, []string{"EUR/USD"})
	if m := s.IMMultiplier("EUR/USD"); m != 1.5 {
		t.Fatalf("want 1.5, got %v", m)
	}
	// Feed goes quiet — prior multiplier retained, never silently disarmed.
	src.mu.Lock()
	src.mids["EUR/USD"] = nil
	src.mu.Unlock()
	s.Refresh(ctx, []string{"EUR/USD"})
	if m := s.IMMultiplier("EUR/USD"); m != 1.5 {
		t.Fatalf("empty feed must retain prior multiplier, got %v", m)
	}
}

func TestVolatilityScalerFeedErrorStreakPages(t *testing.T) {
	alerts := &opsAlertSpy{}
	src := &tickVolSrcFake{err: fmt.Errorf("clickhouse down")}
	s := newScaler(t, src, alerts)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		s.Refresh(ctx, []string{"EUR/USD"})
	}
	if alerts.count() != 1 {
		t.Fatalf("3-streak feed failure must page once, got %d", alerts.count())
	}
	// Never computed → neutral, not fabricated protection.
	if m := s.IMMultiplier("EUR/USD"); m != 1.0 {
		t.Fatalf("never-computed symbol must be neutral, got %v", m)
	}
}

func TestVolatilityScalerBaselineSourceOverride(t *testing.T) {
	src := &tickVolSrcFake{mids: map[string][]decimal.Decimal{
		"EUR/USD": zigzag(1.20, 0.010, 12), // stddev ≈ 0.01
	}}
	// Baseline source says 0.005 → ratio ≈ 2 → borderline; vs default
	// 0.004 → ratio 2.5. Use a higher baseline 0.006 → ratio ≈1.7 <2 → neutral.
	s, err := NewVolatilityScaler(VolatilityScalerDeps{
		Source:   src,
		Baseline: baselineFake{vol: 0.006},
	})
	if err != nil {
		t.Fatal(err)
	}
	s.Refresh(context.Background(), []string{"EUR/USD"})
	if m := s.IMMultiplier("EUR/USD"); m != 1.0 {
		t.Fatalf("baseline 0.006 ⇒ ratio <2 must stay neutral, got %v", m)
	}
}

type baselineFake struct {
	vol float64
	err error
}

func (f baselineFake) BaselineVol(context.Context, string) (float64, bool, error) {
	return f.vol, f.vol > 0, f.err
}

// ---------------------------------------------------------------------------
// Margin path wiring — used_margin × IMMultiplier
// ---------------------------------------------------------------------------

type immFake struct{ mult float64 }

func (f immFake) IMMultiplier(string) float64 { return f.mult }

func TestMarginEvaluateVolatilityScalesUsedMargin(t *testing.T) {
	store := &marginStoreFake{
		category: CategoryRetail,
		balances: []BalanceAmount{{Currency: "USD", Available: d("100000")}},
		positions: []MarginPosition{{
			ID: 1, Symbol: "EUR/USD", Side: "LONG", Quantity: d("10000"),
			EntryPrice: d("1.2000"), MarginUsed: d("4000"),
			QuoteCurrency: "USD", MaxLeverage: 30,
		}},
	}
	marks := markCacheFake{m: map[string]decimal.Decimal{"EUR/USD": d("1.2000")}}

	base := newMarginSvc(t, MarginOptions{Store: store, Marks: marks})
	snap, err := base.Evaluate(context.Background(), 7)
	if err != nil {
		t.Fatal(err)
	}
	if !snap.UsedMargin.Equal(d("4000")) {
		t.Fatalf("baseline used margin = %s, want 4000", snap.UsedMargin)
	}

	scaled := newMarginSvc(t, MarginOptions{
		Store: store, Marks: marks, Volatility: immFake{mult: 1.5},
	})
	snap2, err := scaled.Evaluate(context.Background(), 7)
	if err != nil {
		t.Fatal(err)
	}
	if !snap2.UsedMargin.Equal(d("6000")) {
		t.Fatalf("scaled used margin = %s, want 6000 (×1.5)", snap2.UsedMargin)
	}
}
