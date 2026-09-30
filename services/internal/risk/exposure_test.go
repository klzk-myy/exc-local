// exposure_test.go — unit coverage for the position-mode-aware exposure
// gate (exposure.go; Phase-19 Tasks 19.3.5/19.3.15; spec §13.6).
//
// Coverage map:
//   - exposureIncrements: HEDGING full-add vs NETTING residual math
//   - CheckExposure: validation, cap enforcement (symbol/short/account),
//     reject-on-breach with MAX_EXPOSURE_EXCEEDED
//   - fail-closed: unbound deps, store errors, unreadable mode reads as
//     the conservative full-add posture
package risk

import (
	"context"
	"fmt"
	"testing"

	"exchange/pkg/decimal"
)

// ---------------------------------------------------------------------------
// Fakes
// ---------------------------------------------------------------------------

type expLimitsFake struct{ lim EffectiveLimits }

func (f expLimitsFake) EffectiveLimits(int64, string, string) EffectiveLimits { return f.lim }

type expSourceFake struct {
	rows []SymbolExposure
	err  error
}

func (f expSourceFake) SymbolExposures(context.Context, int64) ([]SymbolExposure, error) {
	return f.rows, f.err
}

// ---------------------------------------------------------------------------
// exposureIncrements — the mode-aware residual math
// ---------------------------------------------------------------------------

func TestExposureIncrements(t *testing.T) {
	N := d("1000")
	long := d("600")
	short := d("400")
	cases := []struct {
		name              string
		mode, side        string
		notional, l, s    decimal.Decimal
		wantGross, wantSh decimal.Decimal
	}{
		// HEDGING — every fill adds gross; SELL also adds short.
		{"hedge buy", ModeHedging, "BUY", N, long, short, N, decimal.Zero},
		{"hedge sell", ModeHedging, "SELL", N, long, short, N, N},
		// Unreadable/unknown modes read as the conservative full-add.
		{"empty mode buy", "", "BUY", N, long, short, N, decimal.Zero},
		{"empty mode sell", "", "SELL", N, long, short, N, N},
		{"bogus mode sell", "WEIRD", "SELL", N, long, short, N, N},
		// NETTING — opposing fill first consumes the open side; only the
		// residual opens new net exposure.
		{"net buy <= short", ModeNetting, "BUY", d("300"), long, short,
			decimal.Zero, decimal.Zero},
		{"net buy == short", ModeNetting, "BUY", d("400"), long, short,
			decimal.Zero, decimal.Zero},
		{"net buy > short", ModeNetting, "BUY", N, long, short,
			d("600"), decimal.Zero},
		{"net sell <= long", ModeNetting, "SELL", d("500"), long, short,
			decimal.Zero, decimal.Zero},
		{"net sell > long", ModeNetting, "SELL", N, long, short,
			d("400"), d("400")}, // residual is new SHORT exposure
	}
	for _, c := range cases {
		g, s := exposureIncrements(c.mode, c.side, c.notional, c.l, c.s)
		if !g.Equal(c.wantGross) || !s.Equal(c.wantSh) {
			t.Fatalf("%s: got gross=%s short=%s, want %s/%s",
				c.name, g, s, c.wantGross, c.wantSh)
		}
	}
}

// ---------------------------------------------------------------------------
// CheckExposure — validation + unbound fail-closed
// ---------------------------------------------------------------------------

func expReq() OrderRequest {
	return OrderRequest{
		AccountID: 7, KycTier: "T1", Symbol: "EURUSD", Side: "BUY",
		Quantity: d("1000"), Price: d("1.10"),
	}
}

func TestExposureCheckUnboundFailsClosed(t *testing.T) {
	svc := NewExposureService(nil, expSourceFake{}, nil)
	if err := svc.CheckExposure(context.Background(), expReq()); err == nil {
		t.Fatal("nil limits resolver must reject")
	} else {
		requireCode(t, err, CodeRiskLimitsInternal)
	}
	svc = NewExposureService(expLimitsFake{}, nil, nil)
	if err := svc.CheckExposure(context.Background(), expReq()); err == nil {
		t.Fatal("nil exposure source must reject")
	} else {
		requireCode(t, err, CodeRiskLimitsInternal)
	}
}

func TestExposureCheckValidation(t *testing.T) {
	lim := EffectiveLimits{}
	svc := NewExposureService(expLimitsFake{lim}, expSourceFake{}, nil)
	ctx := context.Background()

	// reduce_only is never exposure-increasing — admitted even with a
	// zeroed limit set.
	req := expReq()
	req.ReduceOnly = true
	if err := svc.CheckExposure(ctx, req); err != nil {
		t.Fatalf("reduce_only must pass: %v", err)
	}

	req = expReq()
	req.Quantity = decimal.Zero
	requireCode(t, svc.CheckExposure(ctx, req), CodeOrderRejected)

	req = expReq()
	req.Price = decimal.Zero
	requireCode(t, svc.CheckExposure(ctx, req), CodeOrderRejected)

	req = expReq()
	req.Side = "SELL"
	if err := svc.CheckExposure(ctx, req); err != nil {
		t.Fatalf("nil caps = unlimited must pass: %v", err)
	}
}

// ---------------------------------------------------------------------------
// CheckExposure — reject-on-breach per cap
// ---------------------------------------------------------------------------

func TestExposureCheckSymbolCap(t *testing.T) {
	ctx := context.Background()
	lim := EffectiveLimits{MaxNotionalExposure: dec("5000")}
	rows := []SymbolExposure{
		{Symbol: "EURUSD", GrossNotional: d("4000")},
		{Symbol: "USDJPY", GrossNotional: d("4100")},
	}
	svc := NewExposureService(expLimitsFake{lim}, expSourceFake{rows: rows}, nil)

	// Existing 4000 + BUY 1000×1.10=1100 → 5100 > 5000 → reject.
	requireCode(t, svc.CheckExposure(ctx, expReq()), CodeMaxExposureExceeded)

	// A smaller order under the residual headroom passes.
	req := expReq()
	req.Quantity = d("800") // 880 notional → 4880 ≤ 5000
	if err := svc.CheckExposure(ctx, req); err != nil {
		t.Fatalf("within cap must pass: %v", err)
	}

	// A different symbol's cap is evaluated independently — USDJPY has
	// 4100 gross so the same 1100 order breaches there too (5200 > 5000).
	req = expReq()
	req.Symbol = "USDJPY"
	requireCode(t, svc.CheckExposure(ctx, req), CodeMaxExposureExceeded)
}

func TestExposureCheckShortCap(t *testing.T) {
	ctx := context.Background()
	lim := EffectiveLimits{MaxShortExposure: dec("2000")}
	rows := []SymbolExposure{
		{Symbol: "EURUSD", GrossNotional: d("1500"), ShortNotional: d("1500")},
	}
	svc := NewExposureService(expLimitsFake{lim}, expSourceFake{rows: rows}, nil)

	// SELL 1100 notional: short 1500 + 1100 > 2000 → reject.
	req := expReq()
	req.Side = "SELL"
	requireCode(t, svc.CheckExposure(ctx, req), CodeMaxExposureExceeded)

	// BUY adds no short exposure — the short cap cannot fire.
	req = expReq()
	req.Quantity = d("1000000") // huge gross but no short increment
	if err := svc.CheckExposure(ctx, req); err != nil {
		t.Fatalf("BUY must not trip the short cap: %v", err)
	}
}

func TestExposureCheckAccountCap(t *testing.T) {
	ctx := context.Background()
	lim := EffectiveLimits{MaxAccountNotional: dec("10000")}
	rows := []SymbolExposure{
		{Symbol: "EURUSD", GrossNotional: d("4000")},
		{Symbol: "USDJPY", GrossNotional: d("5500")},
	}
	svc := NewExposureService(expLimitsFake{lim}, expSourceFake{rows: rows}, nil)

	// Account gross 9500 + 1100 > 10000 → reject even though EURUSD is
	// individually small.
	requireCode(t, svc.CheckExposure(ctx, expReq()), CodeMaxExposureExceeded)
}

// ---------------------------------------------------------------------------
// CheckExposure — position mode changes the math
// ---------------------------------------------------------------------------

func TestExposureCheckNettingResidual(t *testing.T) {
	ctx := context.Background()
	lim := EffectiveLimits{MaxNotionalExposure: dec("5000")}
	// NETTING account long 4000 gross on EURUSD; a SELL consumes it.
	rows := []SymbolExposure{
		{Symbol: "EURUSD", GrossNotional: d("4000"), ShortNotional: decimal.Zero},
	}
	modes := PositionModeFunc(func(context.Context, int64) (string, error) {
		return ModeNetting, nil
	})
	svc := NewExposureService(expLimitsFake{lim}, expSourceFake{rows: rows}, modes)

	// SELL 1100 < long 4000 → pure reduction, zero increment → pass.
	req := expReq()
	req.Side = "SELL"
	if err := svc.CheckExposure(ctx, req); err != nil {
		t.Fatalf("netting reduction must pass: %v", err)
	}
	// SELL 6000 → residual 2000 new exposure → 4000+2000=6000 > 5000 → reject.
	req.Quantity = d("6000")
	req.Price = decimal.One
	requireCode(t, svc.CheckExposure(ctx, req), CodeMaxExposureExceeded)
}

func TestExposureCheckModeFallbackConservative(t *testing.T) {
	ctx := context.Background()
	lim := EffectiveLimits{MaxNotionalExposure: dec("5000")}
	rows := []SymbolExposure{
		{Symbol: "EURUSD", GrossNotional: d("4000")},
	}
	// Mode store error → conservative full-add: SELL 1100 counts fully
	// (it would be a pure reduction under NETTING).
	modes := PositionModeFunc(func(context.Context, int64) (string, error) {
		return "", fmt.Errorf("pg down")
	})
	svc := NewExposureService(expLimitsFake{lim}, expSourceFake{rows: rows}, modes)
	req := expReq()
	req.Side = "SELL"
	requireCode(t, svc.CheckExposure(ctx, req), CodeMaxExposureExceeded)

	// Explicit HEDGING reads identically.
	modes = PositionModeFunc(func(context.Context, int64) (string, error) {
		return ModeHedging, nil
	})
	svc = NewExposureService(expLimitsFake{lim}, expSourceFake{rows: rows}, modes)
	requireCode(t, svc.CheckExposure(ctx, req), CodeMaxExposureExceeded)
}

func TestExposureCheckStoreError(t *testing.T) {
	svc := NewExposureService(expLimitsFake{EffectiveLimits{}},
		expSourceFake{err: fmt.Errorf("pg down")}, nil)
	if err := svc.CheckExposure(context.Background(), expReq()); err == nil {
		t.Fatal("exposure source failure must reject fail-closed")
	} else {
		requireCode(t, err, CodeRiskLimitsInternal)
	}
}
