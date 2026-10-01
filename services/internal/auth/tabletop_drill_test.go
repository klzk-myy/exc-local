// Phase-13.5 Task 13.5.3.4 tabletop evidence — Security-Incident
// lockout drill.
//
// Drives the REAL §12.6 brute-force lockout state machine on dev Redis:
// LockoutThreshold consecutive failures inside LockoutWindow lock the
// identifier for 15 minutes; the login seam fails closed and returns
// 423 ACCOUNT_LOCKED_AUTH_FAILURES while locked.
//
//	EXC_TABLETOP=1 EXC_REDIS_TEST_ADDR=127.0.0.1:16379 \
//	  EXC_REDIS_TEST_PASSWORD=redpass \
//	  go test -v -run TestTabletopSecurityIncident ./internal/auth/
//
// Drill identifier is scoped ("tabletop:incident-135") and cleared in
// defer so the drill leaves no residual lock.
package auth

import (
	"context"
	"os"
	"testing"
	"time"

	excredis "exchange/internal/redis"
)

func TestTabletopSecurityIncident(t *testing.T) {
	started := time.Now()
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
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := rdb.Ping(ctx); err != nil {
		t.Skipf("dev redis unavailable at %s: %v", addr, err)
	}

	const user = "tabletop:incident-135"
	svc := NewLockoutService(rdb)
	defer func() {
		// Drill hygiene — remove the lock + counter keys.
		_ = rdb.Del(ctx, authFailuresKey(user), authLockKey(user)).Err()
	}()
	// Clear any residue from a prior run BEFORE baseline check.
	_ = rdb.Del(ctx, authFailuresKey(user), authLockKey(user)).Err()

	step := func(n int, what string) {
		t.Logf("step %d: %s (t+%s)", n, what, time.Since(started).Round(time.Millisecond))
	}

	step(1, "baseline — identifier not locked")
	locked, _, err := svc.CheckLock(ctx, user)
	if err != nil || locked {
		t.Fatalf("baseline CheckLock: locked=%v err=%v", locked, err)
	}

	step(2, "record failures until threshold")
	for i := 1; i <= LockoutThreshold; i++ {
		n, nowLocked, retry, err := svc.RecordFailure(ctx, user)
		if err != nil {
			t.Fatalf("RecordFailure %d: %v", i, err)
		}
		if i < LockoutThreshold {
			if nowLocked || n != i {
				t.Fatalf("failure %d must not lock yet (n=%d locked=%v)", i, n, nowLocked)
			}
			continue
		}
		if !nowLocked {
			t.Fatalf("failure %d must trigger the lock", i)
		}
		if retry <= 0 || retry > 15*time.Minute {
			t.Fatalf("lock retryAfter out of range: %s", retry)
		}
		t.Logf("lock engaged at failure %d, retryAfter=%s", n, retry.Round(time.Second))
	}

	step(3, "fail-closed check — login seam sees the lock")
	locked, retry, err := svc.CheckLock(ctx, user)
	if err != nil || !locked {
		t.Fatalf("CheckLock after lock: locked=%v err=%v", locked, err)
	}
	t.Logf("423 ACCOUNT_LOCKED_AUTH_FAILURES path active, Retry-After=%s", retry.Round(time.Second))

	step(4, "while locked, further failures return remaining lock TTL")
	_, stillLocked, retry2, err := svc.RecordFailure(ctx, user)
	if err != nil || !stillLocked || retry2 <= 0 {
		t.Fatalf("locked RecordFailure: locked=%v retry=%v err=%v", stillLocked, retry2, err)
	}

	step(5, "design check — the lock is time-bound, not success-bound")
	// ClearFailures resets the consecutive-failure counter but
	// deliberately does NOT clear the active lock (lockout.go:145 — a
	// correct-password guess during lockout must not release it).
	if err := svc.ClearFailures(ctx, user); err != nil {
		t.Fatalf("ClearFailures: %v", err)
	}
	locked, retry3, err := svc.CheckLock(ctx, user)
	if err != nil || !locked {
		t.Fatalf("lock must persist until TTL, got locked=%v err=%v", locked, err)
	}
	t.Logf("lock survives counter clear — persists for %s (time-bound)", retry3.Round(time.Second))
	// Operator release path is an explicit Redis key deletion by the
	// on-call runbook step (docs/runbooks/security-incident-response.md);
	// the defer below performs it as drill hygiene.

	elapsed := time.Since(started)
	t.Logf("TABLETOP security-incident lockout elapsed=%s (P1 SLA 15m) — PASS", elapsed.Round(time.Millisecond))
	if elapsed > 15*time.Minute {
		t.Fatalf("drill exceeded P1 SLA: %s", elapsed)
	}
}
