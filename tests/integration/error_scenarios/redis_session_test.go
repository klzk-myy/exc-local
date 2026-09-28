// Task 8.3.5 scenario 3 — Redis eviction / outage during session lookup
// (spec §4.1 session keyspace, §2.7 fail-closed pessimism).
//
// Contracts asserted — the failure mode matters, not just "an error":
//
//  1. Evicted/missing session: after session:{token} is deleted (the
//     eviction the suite injects — a real DEL on the key we created,
//     plus the TTL-expiry variant), the store must report a MISS —
//     (nil, nil), never a fabricated session. Callers map a miss to
//     unauthenticated; the dangerous failure (treating evicted as
//     logged-in) is what this test exists to catch.
//  2. Eviction mid-request: a token valid at T0 and gone at T1 — the
//     second lookup must observe the miss, no sticky in-process cache
//     may resurrect it.
//  3. Redis outage: a client pointed at a dead endpoint must surface an
//     error from GetSession/IsHalted — never (nil, nil) which upstream
//     code would read as "session absent but Redis healthy".
//  4. Gateway fail-closed degradation surface: with the coordination
//     store unreachable, middleware.DegradationModeHeader must emit
//     X-Degradation-Mode: Maintenance (strictest-safe), never Normal —
//     the L1 transition contract of §2.4/§2.7.3.
//
// Gate: subtests 1–2 need EXC_REDIS_TEST=1 (dedicated logical DB index,
// scoped cleanup only — see helpers_test.go); 3–4 run in-process.
package error_scenarios

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"exchange/internal/middleware"
	exchredis "exchange/internal/redis"
)

// TestRedisEviction_SessionLookupMiss writes a session, evicts it (DEL —
// the deterministic equivalent of an allkeys-lru eviction or FLUSHDB on
// shared infra, which this suite deliberately never issues), and asserts
// the store reports a clean miss.
func TestRedisEviction_SessionLookupMiss(t *testing.T) {
	c := redisClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	tok := redisTestToken(t, c)
	want := exchredis.Session{UserID: "u-errscen", AccountID: "9900", Tier: "standard"}
	if err := c.SetSession(ctx, tok, want); err != nil {
		t.Fatalf("seed session: %v", err)
	}

	got, err := c.GetSession(ctx, tok)
	if err != nil || got == nil {
		t.Fatalf("pre-eviction lookup: session=%v err=%v, want hit", got, err)
	}
	if got.UserID != want.UserID || got.AccountID != want.AccountID {
		t.Fatalf("pre-eviction session fields drifted: %+v", got)
	}

	// Inject the eviction.
	if err := c.Del(ctx, "session:"+tok).Err(); err != nil {
		t.Fatalf("evict: %v", err)
	}

	got, err = c.GetSession(ctx, tok)
	if err != nil {
		t.Fatalf("post-eviction lookup must be a miss, not an error: %v", err)
	}
	if got != nil {
		t.Fatalf("evicted session resurrected: %+v — fail-closed violation", got)
	}
}

// TestRedisEviction_SessionTTLExpiry exercises the expiry-side eviction:
// a session written with a trivially short TTL must be gone when it
// lapses — the store never returns a stale authenticated view.
func TestRedisEviction_SessionTTLExpiry(t *testing.T) {
	c := redisClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	tok := redisTestToken(t, c)
	// Write the raw key directly at 50ms so we exercise real expiry, not
	// a simulated clock.
	if err := c.HSet(ctx, "session:"+tok,
		map[string]any{"user_id": "u-errscen", "account_id": "9900", "tier": "basic"}).Err(); err != nil {
		t.Fatalf("seed short-ttl session: %v", err)
	}
	if err := c.PExpire(ctx, "session:"+tok, 50*time.Millisecond).Err(); err != nil {
		t.Fatalf("arm ttl: %v", err)
	}

	deadline := time.Now().Add(3 * time.Second)
	for {
		got, err := c.GetSession(ctx, tok)
		if err != nil {
			t.Fatalf("post-expiry lookup must be a miss, not an error: %v", err)
		}
		if got == nil {
			return // expired — contract holds
		}
		if time.Now().After(deadline) {
			t.Fatalf("session still readable 3s past 50ms TTL: %+v", got)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestRedisOutage_SessionLookupFailsClosed points the session client at a
// dead port. The contract: lookup returns an ERROR (fail-closed), never
// (nil, nil) — callers must not interpret a Redis outage as "session
// expired → fall through to some lesser auth path".
func TestRedisOutage_SessionLookupFailsClosed(t *testing.T) {
	// Port 1 is a guaranteed dead endpoint on loopback.
	dead := exchredis.New("127.0.0.1:1", "", 0)
	defer func() { _ = dead.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	sess, err := dead.GetSession(ctx, "errscen:outage-probe")
	if err == nil {
		t.Fatal("session lookup against dead Redis returned no error")
	}
	if sess != nil {
		t.Fatalf("dead Redis fabricated session %+v", sess)
	}

	// Same for the halt gate — a coordination outage must not read as
	// "not halted".
	halted, err := dead.IsHalted(ctx)
	if err == nil {
		t.Fatal("IsHalted against dead Redis returned no error")
	}
	if halted {
		t.Fatal("dead Redis reported halt state — ambiguous truth leak")
	}
}

// TestRedisOutage_DegradationHeaderFailClosed drives the real gateway
// middleware over a dead coordination client: every response must carry
// X-Degradation-Mode: Maintenance (strictest-safe) — never Normal.
func TestRedisOutage_DegradationHeaderFailClosed(t *testing.T) {
	dead := exchredis.New("127.0.0.1:1", "", 0)
	defer func() { _ = dead.Close() }()

	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	h := middleware.DegradationModeHeader(dead, ok)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/orders", nil))

	got := rec.Header().Get(middleware.DegradationHeader)
	if got != string(exchredis.ModeMaintenance) {
		t.Fatalf("X-Degradation-Mode=%q on Redis outage, want %q (fail-closed)",
			got, exchredis.ModeMaintenance)
	}
}

// TestRedisOutage_DegradationHeaderNilReader covers the unwired-reader
// case — nil reader must also resolve to Maintenance, never Normal.
func TestRedisOutage_DegradationHeaderNilReader(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	h := middleware.DegradationModeHeader(nil, ok)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/orders", nil))

	if got := rec.Header().Get(middleware.DegradationHeader); got != string(exchredis.ModeMaintenance) {
		t.Fatalf("X-Degradation-Mode=%q with nil reader, want Maintenance", got)
	}
}
