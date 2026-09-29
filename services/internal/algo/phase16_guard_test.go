// Phase-16 Task 16.3.9/16/22 unit tests — vocabulary mapping, GSLO
// premium/exposure math, reservation parsing and the oracle-staleness
// admission gate. PG-free (freshness + book seams faked).
package algo

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"exchange/internal/orders"
	"exchange/pkg/decimal"
	excerrors "exchange/pkg/errors"
)

func TestSchedulerOrderBenchmarkRoundTrip(t *testing.T) {
	for _, orderB := range []string{
		orders.FixingBenchmarkWMR4PM,
		orders.FixingBenchmarkECB1415,
		orders.FixingBenchmarkTokyo0955,
	} {
		sb, ok := SchedulerBenchmark(orderB)
		if !ok {
			t.Fatalf("SchedulerBenchmark(%q) unmapped", orderB)
		}
		back, ok := OrderBenchmark(sb)
		if !ok || back != orderB {
			t.Fatalf("round trip %q → %q → %q", orderB, sb, back)
		}
	}
	if _, ok := SchedulerBenchmark("WM_REFINITIV_4PM_LDN"); ok {
		t.Fatal("superseded plan string must not map")
	}
	if _, ok := OrderBenchmark("ECB_1415"); ok {
		t.Fatal("canonical order vocab must not be a scheduler value")
	}
}

func TestGSLOPremiumFormula(t *testing.T) {
	// premium = qty × rate × |ref − stop| (quote) — the task's
	// notional × rate × distance/current simplification.
	qty := decimal.RequireFromString("100000")
	ref := decimal.RequireFromString("1.10")
	stop := decimal.RequireFromString("1.08")
	p := premiumOf(qty, ref, stop, decimal.RequireFromString("10")) // 10 bps
	// 100000 × 0.001 × 0.02 = 2.00 quote
	if p.Cmp(decimal.RequireFromString("2")) != 0 {
		t.Fatalf("premium %s, want 2", p)
	}
	// Zero distance → zero premium (stop at market is free).
	if got := premiumOf(qty, ref, ref, decimal.RequireFromString("10")); !got.IsZero() {
		t.Fatalf("zero-distance premium %s", got)
	}
}

func TestGSLOGapLiability(t *testing.T) {
	g := gapLiability(decimal.RequireFromString("50000"),
		decimal.RequireFromString("1.20"), decimal.RequireFromString("1.10"))
	// 50000 × 0.10 = 5000
	if g.Cmp(decimal.RequireFromString("5000")) != 0 {
		t.Fatalf("gap liability %s, want 5000", g)
	}
}

func TestReservationOfParsesAlgoParams(t *testing.T) {
	if r, err := reservationOf(nil); err != nil || r != nil {
		t.Fatalf("empty params → %v %v", r, err)
	}
	if _, err := reservationOf([]byte("{bad json")); err == nil {
		t.Fatal("malformed params must error")
	}
	blob, _ := json.Marshal(map[string]any{"fixing_reservation": map[string]string{
		"currency": "USD", "amount": "1100.00", "consumed": "550.00"}})
	r, err := reservationOf(blob)
	if err != nil || r == nil {
		t.Fatalf("parse: %v %v", r, err)
	}
	if r.amount().Cmp(decimal.RequireFromString("1100")) != 0 ||
		r.consumed().Cmp(decimal.RequireFromString("550")) != 0 ||
		r.remaining().Cmp(decimal.RequireFromString("550")) != 0 {
		t.Fatalf("reservation math: %+v", r)
	}
}

func TestTriggerGuardStaleMarkFailsClosed(t *testing.T) {
	pool, err := pgxpool.New(context.Background(),
		"postgres://unused:unused@127.0.0.1:1/x")
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	inst := &orders.Instrument{ID: 7, Symbol: "EURUSD"}

	// Unwired freshness seam → MARK/INDEX triggers fail closed.
	g := NewTriggerGuard(pool, nil, func() time.Time { return time.Now() })
	req := &orders.SubmitRequest{TriggerSource: "MARK_PRICE"}
	err = g.AdmitConditional(context.Background(), inst, req)
	if err == nil {
		t.Fatal("unwired oracle must reject MARK trigger")
	}
	var ce *excerrors.Error
	if e, ok := err.(*excerrors.Error); !ok || e.Code != "CONDITIONAL_TRIGGER_ORACLE_STALE" {
		_ = ce
		t.Fatalf("want CONDITIONAL_TRIGGER_ORACLE_STALE, got %v", err)
	}

	// Fresh feed (1s old) passes admission.
	g = NewTriggerGuard(pool, func(_ context.Context, _ int64, _ string) (*time.Time, error) {
		ts := time.Now().Add(-time.Second)
		return &ts, nil
	}, func() time.Time { return time.Now() })
	if err := g.AdmitConditional(context.Background(), inst, req); err != nil {
		t.Fatalf("fresh feed must admit: %v", err)
	}

	// Stale feed (10s old) suspends evaluation.
	g = NewTriggerGuard(pool, func(_ context.Context, _ int64, _ string) (*time.Time, error) {
		ts := time.Now().Add(-10 * time.Second)
		return &ts, nil
	}, func() time.Time { return time.Now() })
	err = g.AdmitConditional(context.Background(), inst, req)
	if err == nil {
		t.Fatal("10s-stale feed must reject")
	}
	if e, ok := err.(*excerrors.Error); !ok || e.Code != "CONDITIONAL_TRIGGER_ORACLE_STALE" {
		t.Fatalf("want CONDITIONAL_TRIGGER_ORACLE_STALE, got %v", err)
	}
}

func TestFixingVocabRejectsSupersededStrings(t *testing.T) {
	// The order-level CHECK (migration 038) + this gate keep plan-era
	// strings out of the canonical path.
	for _, bad := range []string{"WM_REFINITIV_4PM_LDN", "ECB_1415_CET", "WM_LONDON_4PM"} {
		if _, ok := SchedulerBenchmark(bad); ok {
			t.Fatalf("superseded %q must not map", bad)
		}
	}
}
