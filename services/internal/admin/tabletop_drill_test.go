// Phase-13.5 Task 13.5.3.4 tabletop evidence — Trading-Halt drill.
//
// Drives the REAL halt state machine on dev Redis — the same
// KillSwitchResolver seam orders admission consults and the same
// five-tier circuit-breaker admission gate (risk.CircuitBreakerService
// over RedisBreakerStore). Run:
//
//	EXC_TABLETOP=1 EXC_REDIS_TEST_ADDR=127.0.0.1:16379 \
//	  EXC_REDIS_TEST_PASSWORD=redpass \
//	  go test -v -run TestTabletopTradingHalt ./internal/admin/
//
// The drill is self-timing: elapsed is asserted < 15 min (P1 SLA).
// Drill-scoped keys (account 900000777, symbol DRILLUSD) are cleaned up
// in defer; halt:global is raised then cleared inside the same test.
package admin_test

import (
	"context"
	"os"
	"testing"
	"time"

	"exchange/internal/admin"
	excredis "exchange/internal/redis"
	"exchange/internal/risk"
)

func tabletopRedis(t *testing.T) *excredis.Client {
	t.Helper()
	if os.Getenv("EXC_TABLETOP") != "1" {
		t.Skip("set EXC_TABLETOP=1 to run tabletop drills")
	}
	addr := os.Getenv("EXC_REDIS_TEST_ADDR")
	if addr == "" {
		addr = "127.0.0.1:16379"
	}
	pass := os.Getenv("EXC_REDIS_TEST_PASSWORD")
	if pass == "" {
		pass = "redpass"
	}
	rdb := excredis.New(addr, pass, 0)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := rdb.Ping(ctx); err != nil {
		t.Skipf("dev redis unavailable at %s: %v", addr, err)
	}
	return rdb
}

func TestTabletopTradingHalt(t *testing.T) {
	started := time.Now()
	rdb := tabletopRedis(t)
	ctx := context.Background()
	const acct = int64(900000777)
	const sym = "DRILLUSD"
	resolver := admin.NewKillSwitchResolver(rdb, "DEV")

	// Drill hygiene — no stale flags from a prior run.
	_ = rdb.ClearHalt(ctx)
	_ = rdb.ClearHaltScope(ctx, admin.ScopeAccount, "900000777")
	defer func() {
		_ = rdb.ClearHalt(ctx)
		_ = rdb.ClearHaltScope(ctx, admin.ScopeAccount, "900000777")
	}()

	step := func(n int, what string) {
		t.Logf("step %d: %s (t+%s)", n, what, time.Since(started).Round(time.Millisecond))
	}

	// Step 1 — baseline: venue open.
	step(1, "baseline — admission resolver reports open")
	scope, _, err := resolver.OrderHalt(ctx, acct, sym, "", "")
	if err != nil || scope != "" {
		t.Fatalf("baseline halt check: scope=%q err=%v", scope, err)
	}

	// Step 2 — GLOBAL halt raised (the kill-switch GLOBAL flag, same key
	// orders admission consults).
	step(2, "SetHaltScope GLOBAL — venue-wide trading halt")
	if err := rdb.SetHaltScope(ctx, admin.ScopeGlobal, "", "tabletop T1 global halt"); err != nil {
		t.Fatalf("global halt set: %v", err)
	}
	halted, err := resolver.GlobalHalted(ctx)
	if err != nil || !halted {
		t.Fatalf("GlobalHalted=%v err=%v after set", halted, err)
	}
	scope, detail, err := resolver.OrderHalt(ctx, acct, sym, "", "")
	if err != nil || scope != admin.ScopeGlobal {
		t.Fatalf("expected GLOBAL suspension, got scope=%q err=%v", scope, err)
	}
	t.Logf("order admission suspended: scope=%s detail=%q", scope, detail)

	// Step 3 — resume.
	step(3, "ClearHalt — resume")
	if err := rdb.ClearHalt(ctx); err != nil {
		t.Fatalf("clear: %v", err)
	}
	scope, _, err = resolver.OrderHalt(ctx, acct, sym, "", "")
	if err != nil || scope != "" {
		t.Fatalf("post-resume halt check: scope=%q err=%v", scope, err)
	}

	// Step 4 — scoped ACCOUNT halt: only the target account is gated.
	step(4, "SetHaltScope ACCOUNT 900000777")
	if err := rdb.SetHaltScope(ctx, admin.ScopeAccount, "900000777",
		"tabletop T1 account halt"); err != nil {
		t.Fatalf("account halt set: %v", err)
	}
	scope, _, err = resolver.OrderHalt(ctx, acct, sym, "", "")
	if err != nil || scope != admin.ScopeAccount {
		t.Fatalf("expected ACCOUNT suspension, got %q err=%v", scope, err)
	}
	scope, _, err = resolver.OrderHalt(ctx, acct+1, sym, "", "")
	if err != nil || scope != "" {
		t.Fatalf("unrelated account must stay open, got %q err=%v", scope, err)
	}
	if err := rdb.ClearHaltScope(ctx, admin.ScopeAccount, "900000777"); err != nil {
		t.Fatalf("clear scoped halt: %v", err)
	}

	// Step 5 — five-tier circuit breaker: manual INSTRUMENT trip gates
	// admission, reset clears it.
	step(5, "ManualTrip INSTRUMENT DRILLUSD → AdmitOrder rejects")
	breaker, err := risk.NewCircuitBreakerService(risk.BreakerDeps{
		Store: risk.RedisBreakerStore{C: rdb},
	})
	if err != nil {
		t.Fatalf("breaker svc: %v", err)
	}
	defer func() {
		// Drill cleanup — force the drill breaker closed.
		_ = breaker.ManualReset(ctx, risk.ScopeInstrument, sym, 900000001)
	}()
	if err := breaker.ManualTrip(ctx, risk.ScopeInstrument, sym,
		"tabletop T1 instrument trip", 900000001); err != nil {
		t.Fatalf("manual trip: %v", err)
	}
	if aerr := breaker.AdmitOrder(ctx, acct, sym); aerr == nil {
		t.Fatal("AdmitOrder must reject on an OPEN instrument breaker")
	} else {
		t.Logf("AdmitOrder rejected: %v", aerr)
	}
	// A different instrument stays open.
	if aerr := breaker.AdmitOrder(ctx, acct, "CLEANUSD"); aerr != nil {
		t.Fatalf("unrelated instrument must admit, got %v", aerr)
	}
	step(6, "ManualReset — dual-controlled resume path")
	if err := breaker.ManualReset(ctx, risk.ScopeInstrument, sym, 900000001); err != nil {
		t.Fatalf("manual reset: %v", err)
	}
	if aerr := breaker.AdmitOrder(ctx, acct, sym); aerr != nil {
		t.Fatalf("post-reset admission must pass, got %v", aerr)
	}

	elapsed := time.Since(started)
	t.Logf("TABLETOP trading-halt elapsed=%s (P1 SLA 15m) — PASS", elapsed.Round(time.Millisecond))
	if elapsed > 15*time.Minute {
		t.Fatalf("drill exceeded P1 SLA: %s", elapsed)
	}
}
