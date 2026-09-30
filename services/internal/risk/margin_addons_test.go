// margin_addons_test.go — unit coverage for the §13.12 concentration +
// liquidity add-on wiring in MarginService.Evaluate (Task 19.3.21):
// provisioned sources charge the add-ons, missing/erroring denominators
// take each helper's pessimistic leg, ISOLATED mode skips them, and an
// unprovisioned seam leaves base margin untouched.
package risk

import (
	"context"
	"fmt"
	"testing"

	"exchange/pkg/decimal"
)

// staticOI / staticADV are the provisioned-source fakes — a nil option
// field is "feature off", a wired fake with zero/error is "data
// missing" (pessimistic leg).
type staticOI struct {
	v   decimal.Decimal
	err error
}

func (s staticOI) OpenInterest(context.Context, int64) (decimal.Decimal, error) {
	return s.v, s.err
}

type staticADV struct {
	v   decimal.Decimal
	err error
}

func (s staticADV) ADV(context.Context, int64) (decimal.Decimal, error) {
	return s.v, s.err
}

// marginEvalFixture basis for every case: EUR/USD 10k long, mark 1.20 →
// notional 12000 USD, base margin 1000 USD, equity 3500, mode CROSS.
func TestMarginServiceEvaluateAddonsUnprovisioned(t *testing.T) {
	store, marks := marginEvalFixture()
	svc := newMarginSvc(t, MarginOptions{Store: store, Marks: marks})
	snap, err := svc.Evaluate(context.Background(), 7)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if !snap.UsedMargin.Equal(d("1000")) {
		t.Fatalf("unprovisioned sources must not charge add-ons: used = %s, want 1000",
			snap.UsedMargin)
	}
}

func TestMarginServiceEvaluateConcentrationAddonCharged(t *testing.T) {
	store, marks := marginEvalFixture()
	// OI 30000 → share 12000/30000 = 0.40, excess 0.15 →
	// addon = 1000 × 0.15 × 0.5 = 75.
	svc := newMarginSvc(t, MarginOptions{Store: store, Marks: marks,
		OI: staticOI{v: d("30000")}})
	snap, err := svc.Evaluate(context.Background(), 7)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if !snap.UsedMargin.Equal(d("1075")) {
		t.Fatalf("used = %s, want 1075 (base 1000 + concentration 75)", snap.UsedMargin)
	}
}

func TestMarginServiceEvaluateConcentrationBelowThreshold(t *testing.T) {
	store, marks := marginEvalFixture()
	// OI 100000 → share 0.12 < 0.25 threshold → no add-on.
	svc := newMarginSvc(t, MarginOptions{Store: store, Marks: marks,
		OI: staticOI{v: d("100000")}})
	snap, err := svc.Evaluate(context.Background(), 7)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if !snap.UsedMargin.Equal(d("1000")) {
		t.Fatalf("used = %s, want 1000 (share below 25%% threshold)", snap.UsedMargin)
	}
}

func TestMarginServiceEvaluateLiquidityAddonCharged(t *testing.T) {
	store, marks := marginEvalFixture()
	// ADV 4000 → unwind days 12000/4000 = 3, excess 2 →
	// addon = 1000 × min(2×0.10, 0.50) = 200.
	svc := newMarginSvc(t, MarginOptions{Store: store, Marks: marks,
		ADV: staticADV{v: d("4000")}})
	snap, err := svc.Evaluate(context.Background(), 7)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if !snap.UsedMargin.Equal(d("1200")) {
		t.Fatalf("used = %s, want 1200 (base 1000 + liquidity 200)", snap.UsedMargin)
	}
}

func TestMarginServiceEvaluateAddonsPessimisticOnMissingData(t *testing.T) {
	store, marks := marginEvalFixture()
	// Provisioned sources returning no usable data take the pessimistic
	// legs: OI 0 → share 1 → concentration 1000×0.75×0.5 = 375; ADV 0 →
	// capped 50% → 500. Total add-on 875 → used 1875.
	svc := newMarginSvc(t, MarginOptions{Store: store, Marks: marks,
		OI:  staticOI{v: decimal.Zero},
		ADV: staticADV{v: decimal.Zero},
	})
	snap, err := svc.Evaluate(context.Background(), 7)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if !snap.UsedMargin.Equal(d("1875")) {
		t.Fatalf("used = %s, want 1875 (1000 + 375 + 500)", snap.UsedMargin)
	}
}

func TestMarginServiceEvaluateAddonsPessimisticOnSourceError(t *testing.T) {
	store, marks := marginEvalFixture()
	svc := newMarginSvc(t, MarginOptions{Store: store, Marks: marks,
		ADV: staticADV{err: fmt.Errorf("redis down")}})
	snap, err := svc.Evaluate(context.Background(), 7)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if !snap.UsedMargin.Equal(d("1500")) {
		t.Fatalf("used = %s, want 1500 (1000 + capped 500)", snap.UsedMargin)
	}
}

func TestMarginServiceEvaluateAddonsSkipIsolated(t *testing.T) {
	mark := d("1.10")
	store := &marginStoreFake{
		account:  &MarginAccount{AccountID: 7, Mode: ModeIsolated},
		category: CategoryRetail,
		balances: []BalanceAmount{{Currency: "USD", Available: d("40")}},
		positions: []MarginPosition{{
			ID: 1, Symbol: "EUR/USD", Side: "LONG", Quantity: d("10000"),
			EntryPrice: d("1.10"), StoredMark: &mark, MarginUsed: d("1000"),
			QuoteCurrency: "USD", BaseCurrency: "EUR", MaxLeverage: 30,
			IsolatedAllocated: d("100"),
		}},
	}
	marks := markCacheFake{m: map[string]decimal.Decimal{"EUR/USD": d("1.10")}}
	// Missing-data sources would charge 875 in shared-pool modes —
	// ISOLATED used margin is the allocated collateral alone.
	svc := newMarginSvc(t, MarginOptions{Store: store, Marks: marks,
		OI:  staticOI{v: decimal.Zero},
		ADV: staticADV{v: decimal.Zero},
	})
	snap, err := svc.Evaluate(context.Background(), 7)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if !snap.UsedMargin.Equal(d("100")) {
		t.Fatalf("isolated used = %s, want 100 (add-ons never charge allocated collateral)",
			snap.UsedMargin)
	}
}
