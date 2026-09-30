// collateral_service_test.go — Task 19.3.28 intraday re-haircut monitor
// tests. Coverage: >100bps deviation triggers recompute, breach fans
// out to the margin-call seam, sub-trigger ticks stay quiet, cooldown
// bounds the fan-out, day rollover re-anchors, non-collateral symbols
// are ignored.
package risk

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Fakes
// ---------------------------------------------------------------------------

type collSchedReaderFake struct {
	ccys []string
	err  error
}

func (f collSchedReaderFake) EligibleCurrencies(context.Context) ([]string, error) {
	return f.ccys, f.err
}

type markDeltaFake struct {
	ch chan MarkTick
}

func (f *markDeltaFake) Marks(context.Context) (<-chan MarkTick, error) { return f.ch, nil }

type marginEvalFake struct {
	mu     sync.Mutex
	status string
	calls  []int64
	err    error
}

func (f *marginEvalFake) Evaluate(_ context.Context, acct int64) (*MarginSnapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, acct)
	if f.err != nil {
		return nil, f.err
	}
	return &MarginSnapshot{AccountID: acct, Status: f.status}, nil
}
func (f *marginEvalFake) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

type callEvalFake struct {
	mu    sync.Mutex
	accts []int64
	err   error
}

func (f *callEvalFake) Evaluate(_ context.Context, acct int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.accts = append(f.accts, acct)
	return f.err
}
func (f *callEvalFake) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.accts)
}

// monitorRig wires the monitor with in-memory seams.
type monitorRig struct {
	mon     *CollateralMonitor
	margin  *marginEvalFake
	calls   *callEvalFake
	accts   []int64
	clock   *fakeClock
	symbols map[string]string // sym → ccy (resolved via pairs fake)
}

func newMonitorRig(t *testing.T, status string, accts []int64) *monitorRig {
	t.Helper()
	r := &monitorRig{
		margin: &marginEvalFake{status: status},
		calls:  &callEvalFake{},
		accts:  accts,
		clock:  newFakeClock(),
	}
	mon, err := NewCollateralMonitor(CollateralMonitorDeps{
		Schedule: collSchedReaderFake{ccys: []string{"EUR", "JPY"}},
		Marks:    &markDeltaFake{ch: make(chan MarkTick, 8)},
		Pairs: func(_ context.Context, ccys []string) (map[string]FxPair, error) {
			out := map[string]FxPair{}
			for _, c := range ccys {
				out[c] = FxPair{Symbol: c + "/USD"}
			}
			return out, nil
		},
		Accounts: CollateralAccountFunc(func(_ context.Context, ccy string) ([]int64, error) {
			return r.accts, nil
		}),
		Margin:   r.margin,
		Calls:    r.calls,
		Now:      r.clock.Now,
		Cooldown: 10 * time.Second,
	})
	if err != nil {
		t.Fatalf("monitor: %v", err)
	}
	r.mon = mon
	r.mon.refreshPairs(context.Background())
	return r
}

func tickAt(sym, px string, at time.Time) MarkTick {
	return MarkTick{Symbol: sym, Price: d(px), Source: MarkSourceOracle, Ts: at}
}

// ---------------------------------------------------------------------------
// Trigger semantics
// ---------------------------------------------------------------------------

func TestCollateralMonitorTriggerRecomputesAndCalls(t *testing.T) {
	r := newMonitorRig(t, "MARGIN_CALL", []int64{7, 9})
	ctx := context.Background()
	day := time.Date(2026, 9, 29, 10, 0, 0, 0, time.UTC)

	r.mon.HandleTick(ctx, tickAt("EUR/USD", "1.2000", day)) // anchors, no trigger
	if r.margin.count() != 0 {
		t.Fatal("anchor tick must not trigger")
	}
	// +110bps > 100bps trigger.
	r.mon.HandleTick(ctx, tickAt("EUR/USD", "1.2132", day.Add(time.Minute)))
	if r.margin.count() != 2 {
		t.Fatalf("expected 2 recomputes, got %d", r.margin.count())
	}
	// Both accounts breach (status MARGIN_CALL) → margin-call seam per account.
	if r.calls.count() != 2 {
		t.Fatalf("expected 2 margin-call evaluations, got %d", r.calls.count())
	}
}

func TestCollateralMonitorBelowTriggerStaysQuiet(t *testing.T) {
	r := newMonitorRig(t, "MARGIN_CALL", []int64{7})
	ctx := context.Background()
	day := time.Date(2026, 9, 29, 10, 0, 0, 0, time.UTC)

	r.mon.HandleTick(ctx, tickAt("EUR/USD", "1.2000", day))
	r.mon.HandleTick(ctx, tickAt("EUR/USD", "1.2090", day.Add(time.Minute))) // +75bps
	if r.margin.count() != 0 {
		t.Fatalf("75bps must not trigger, got %d evals", r.margin.count())
	}
}

func TestCollateralMonitorNormalStatusSkipsCallSeam(t *testing.T) {
	r := newMonitorRig(t, "NORMAL", []int64{7})
	ctx := context.Background()
	day := time.Date(2026, 9, 29, 10, 0, 0, 0, time.UTC)

	r.mon.HandleTick(ctx, tickAt("EUR/USD", "1.2000", day))
	r.mon.HandleTick(ctx, tickAt("EUR/USD", "1.2150", day.Add(time.Minute))) // +125bps
	if r.margin.count() != 1 {
		t.Fatalf("recompute expected, got %d", r.margin.count())
	}
	if r.calls.count() != 0 {
		t.Fatal("NORMAL snapshot must not invoke the margin-call seam")
	}
}

func TestCollateralMonitorCooldownBoundsFanout(t *testing.T) {
	r := newMonitorRig(t, "MARGIN_CALL", []int64{7})
	ctx := context.Background()
	day := time.Date(2026, 9, 29, 10, 0, 0, 0, time.UTC)

	r.mon.HandleTick(ctx, tickAt("EUR/USD", "1.2000", day))
	r.mon.HandleTick(ctx, tickAt("EUR/USD", "1.2130", day.Add(time.Minute)))   // trigger 1
	r.mon.HandleTick(ctx, tickAt("EUR/USD", "1.2255", day.Add(2*time.Minute))) // trigger 2 — inside cooldown
	if r.margin.count() != 1 {
		t.Fatalf("cooldown must bound recompute, got %d", r.margin.count())
	}
	// Advance the clock past the cooldown — next trigger recomputes.
	r.clock.Add(11 * time.Second)
	r.mon.HandleTick(ctx, tickAt("EUR/USD", "1.2400", day.Add(3*time.Minute))) // trigger 3
	if r.margin.count() != 2 {
		t.Fatalf("post-cooldown trigger must recompute, got %d", r.margin.count())
	}
}

func TestCollateralMonitorDayRolloverReanchors(t *testing.T) {
	r := newMonitorRig(t, "MARGIN_CALL", []int64{7})
	ctx := context.Background()

	r.mon.HandleTick(ctx, tickAt("EUR/USD", "1.2000", time.Date(2026, 9, 29, 23, 0, 0, 0, time.UTC)))
	// Next UTC day opens 4% away — a new session anchors, not a trigger.
	r.mon.HandleTick(ctx, tickAt("EUR/USD", "1.2480", time.Date(2026, 9, 30, 1, 0, 0, 0, time.UTC)))
	if r.margin.count() != 0 {
		t.Fatalf("day rollover must re-anchor not trigger, got %d", r.margin.count())
	}
}

func TestCollateralMonitorUnwatchedSymbolIgnored(t *testing.T) {
	r := newMonitorRig(t, "MARGIN_CALL", []int64{7})
	ctx := context.Background()
	day := time.Date(2026, 9, 29, 10, 0, 0, 0, time.UTC)

	r.mon.HandleTick(ctx, tickAt("XAU/USD", "2000", day))
	r.mon.HandleTick(ctx, tickAt("XAU/USD", "2100", day.Add(time.Minute))) // +5% but not collateral
	if r.margin.count() != 0 {
		t.Fatal("non-collateral symbol must not trigger")
	}
}

func TestCollateralMonitorEvalErrorStreakPages(t *testing.T) {
	alerts := &opsAlertSpy{}
	r := newMonitorRig(t, "MARGIN_CALL", []int64{7, 8, 9})
	r.margin.err = fmt.Errorf("store down")
	r.mon.alerter = alerts
	ctx := context.Background()
	day := time.Date(2026, 9, 29, 10, 0, 0, 0, time.UTC)

	r.mon.HandleTick(ctx, tickAt("EUR/USD", "1.2000", day))
	r.mon.HandleTick(ctx, tickAt("EUR/USD", "1.2130", day.Add(time.Minute))) // 3 eval failures
	if alerts.count() != 1 {
		t.Fatalf("3-streak eval failure must page once, got %d", alerts.count())
	}
}
