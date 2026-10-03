// Tasks 5.3.2/5.3.27/5.3.34/5.3.40 — deterministic unit tests against
// MemBackend (the Lua mirror). The injected clock makes every window
// boundary and ban duration exact.
package ratelimit

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	exchredis "exchange/internal/redis"
)

// testClock is a manually-advanced clock shared by the Limiter and the
// MemBackend so hits and ban checks see the same instant.
type testClock struct{ ms atomic.Int64 }

func newTestClock() *testClock {
	c := &testClock{}
	c.ms.Store(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).UnixMilli())
	return c
}
func (c *testClock) now() time.Time          { return time.UnixMilli(c.ms.Load()) }
func (c *testClock) advance(d time.Duration) { c.ms.Add(d.Milliseconds()) }

func newTestRig(t *testing.T) (*Limiter, *MemBackend, *testClock) {
	t.Helper()
	ck := newTestClock()
	mem := NewMemBackend()
	mem.SetNow(ck.now)
	l := NewLimiter(mem, LimiterOptions{Now: ck.now, Fallback: mem})
	return l, mem, ck
}

func ipIdentity(ip string) Identity {
	return Identity{Tier: TierPublic, Key: ip, IP: ip}
}

func acctIdentity(id int64) Identity {
	return Identity{Tier: TierBasic, Key: fmt.Sprint(id), IP: "10.0.0.9"}
}

func TestTierSpecsAreSpecValues(t *testing.T) {
	want := map[Tier]int64{
		TierPublic: 5, TierBasic: 20, TierStandard: 100,
		TierProfessional: 500, TierInstitutional: 2000, TierAdmin: 2000,
	}
	for tier, rate := range want {
		if got := SpecOf(tier).RatePerSec; got != rate {
			t.Errorf("tier %s rate = %d, want %d", tier, got, rate)
		}
		if SpecOf(tier).BurstFactor != 2 {
			t.Errorf("tier %s burst factor = %d, want 2", tier, SpecOf(tier).BurstFactor)
		}
	}
	if got := SpecOf("bogus").RatePerSec; got != 5 {
		t.Fatalf("unknown tier must fail closed to Public (5), got %d", got)
	}
}

func TestBurstCapacityThenRateLimited(t *testing.T) {
	l, _, _ := newTestRig(t)
	id := ipIdentity("203.0.113.7")
	// Public: rate 5/s, burst 2x → 10 tokens capacity.
	for i := 0; i < 10; i++ {
		res, err := l.Check(context.Background(), id, 1, false)
		if err != nil || res.Status != HitOK {
			t.Fatalf("hit %d: status=%v err=%v, want HitOK", i, res.Status, err)
		}
	}
	res, err := l.Check(context.Background(), id, 1, false)
	if err != nil || res.Status != HitRateLimited {
		t.Fatalf("hit 11: status=%v err=%v, want HitRateLimited", res.Status, err)
	}
	if res.RetryAfter < 1 {
		t.Fatalf("RetryAfter = %d, want ≥1", res.RetryAfter)
	}
	if res.Limit != 5 {
		t.Fatalf("Limit = %d, want 5", res.Limit)
	}
	if res.ResetEpoch <= 0 {
		t.Fatalf("ResetEpoch = %d, want epoch seconds", res.ResetEpoch)
	}
}

func TestBucketRefillsOverTime(t *testing.T) {
	l, mem, ck := newTestRig(t)
	// Allowlisted so the post-429 marker/ban machinery doesn't convert the
	// first deny into an offense — this test isolates bucket refill.
	if err := mem.AddAllowlist(context.Background(), "203.0.113.8"); err != nil {
		t.Fatal(err)
	}
	id := ipIdentity("203.0.113.8")
	for i := 0; i < 10; i++ {
		if res, _ := l.Check(context.Background(), id, 1, false); res.Status != HitOK {
			t.Fatalf("hit %d denied unexpectedly", i)
		}
	}
	if res, _ := l.Check(context.Background(), id, 1, false); res.Status != HitRateLimited {
		t.Fatalf("expected deny at capacity")
	}
	ck.advance(time.Second) // refill 5 tokens
	for i := 0; i < 5; i++ {
		res, _ := l.Check(context.Background(), id, 1, false)
		if res.Status != HitOK {
			t.Fatalf("post-refill hit %d: status=%v", i, res.Status)
		}
	}
}

func TestWeightQuotaExceeded(t *testing.T) {
	mem := NewMemBackend()
	now := time.Now()
	in := HitInput{
		Tier: TierBasic, Key: "42", IP: "10.0.0.1",
		Weight: 6, Rate: 20, WeightQuota: 10, Now: now,
	}
	res, err := mem.Hit(context.Background(), in)
	if err != nil || res.Status != HitOK {
		t.Fatalf("first hit: %v %v", res.Status, err)
	}
	res, err = mem.Hit(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != HitWeightExceeded {
		t.Fatalf("second hit status=%v, want HitWeightExceeded", res.Status)
	}
	if res.RetryAfter < 1 {
		t.Fatalf("weight RetryAfter = %d, want ≥1", res.RetryAfter)
	}
}

// denyThenOffend forces a rate-limit deny for ip at the current instant
// (arming the rl429 marker at the first denied hit), then returns the
// result of the next request — the post-429 offense per Task 5.3.34.
func denyThenOffend(t *testing.T, l *Limiter, ip string) HitResult {
	t.Helper()
	id := ipIdentity(ip)
	for i := 0; i < 11; i++ { // Public capacity is 10; hit 11 denies + arms
		l.Check(context.Background(), id, 1, false)
	}
	res, err := l.Check(context.Background(), id, 1, false) // offense
	if err != nil {
		t.Fatalf("offense hit: %v", err)
	}
	return res
}

func TestProgressiveBanEscalation(t *testing.T) {
	l, mem, ck := newTestRig(t)
	ip := "198.51.100.10"
	durs := []time.Duration{2 * time.Minute, 30 * time.Minute, 24 * time.Hour}
	for strike, want := range durs {
		res := denyThenOffend(t, l, ip)
		if res.Status != HitBannedNew {
			t.Fatalf("strike %d: status=%v, want HitBannedNew", strike+1, res.Status)
		}
		if res.Ban == nil || res.Ban.Level != strike+1 {
			t.Fatalf("strike %d: ban=%+v", strike+1, res.Ban)
		}
		got := time.Duration(res.Ban.ExpiresAt-res.Ban.BannedAt) * time.Millisecond
		if got != want {
			t.Fatalf("strike %d ban duration=%v, want %v", strike+1, got, want)
		}
		// While banned, further hits return HitBanned without striking.
		res2, _ := l.Check(context.Background(), ipIdentity(ip), 1, false)
		if res2.Status != HitBanned {
			t.Fatalf("during ban status=%v, want HitBanned", res2.Status)
		}
		if res2.RetryAfter < 1 {
			t.Fatalf("banned RetryAfter=%d", res2.RetryAfter)
		}
		// BanInfo round-trips for the admin surface.
		bi, err := mem.BanInfo(context.Background(), ip)
		if err != nil || bi == nil || bi.IP != ip {
			t.Fatalf("BanInfo: %v %+v", err, bi)
		}
		// Expire the ban + let the bucket refill, then re-offend.
		ck.advance(want + 2*time.Second)
	}
	if bans, _ := mem.ListBans(context.Background()); len(bans) != 0 {
		t.Fatalf("after expiry ListBans=%d, want 0", len(bans))
	}
}

func TestAllowlistBypassesBanMachinery(t *testing.T) {
	l, mem, ck := newTestRig(t)
	ip := "198.51.100.20"
	if err := mem.AddAllowlist(context.Background(), ip); err != nil {
		t.Fatal(err)
	}
	res := denyThenOffend(t, l, ip)
	// Allowlisted: still rate-limited, never banned.
	if res.Status != HitRateLimited {
		t.Fatalf("allowlisted offense status=%v, want HitRateLimited", res.Status)
	}
	ck.advance(time.Second)
	res, _ = l.Check(context.Background(), ipIdentity(ip), 1, false)
	if res.Status != HitOK {
		t.Fatalf("allowlisted recovery status=%v, want HitOK", res.Status)
	}
}

func TestBanAdminOverrideAndAudit(t *testing.T) {
	l, mem, _ := newTestRig(t)
	admin := &BanAdmin{B: mem}
	ip := "198.51.100.30"

	ban, err := admin.BanIP(context.Background(), ip, "manual review", "op-1", time.Hour)
	if err != nil || ban.Actor != "op-1" {
		t.Fatalf("BanIP: %v %+v", err, ban)
	}
	res, _ := l.Check(context.Background(), ipIdentity(ip), 1, false)
	if res.Status != HitBanned {
		t.Fatalf("post-BanIP status=%v", res.Status)
	}
	ok, err := admin.UnbanIP(context.Background(), ip, "op-1")
	if err != nil || !ok {
		t.Fatalf("UnbanIP: %v %v", ok, err)
	}
	res, _ = l.Check(context.Background(), ipIdentity(ip), 1, false)
	if res.Status == HitBanned || res.Status == HitBannedNew {
		t.Fatalf("post-unban status=%v", res.Status)
	}

	// Pardon clears strikes.
	_ = denyThenOffend(t, l, ip)
	if err := admin.PardonIP(context.Background(), ip, "op-2"); err != nil {
		t.Fatal(err)
	}
	audit, err := mem.ListAudit(context.Background(), 100)
	if err != nil || len(audit) < 3 {
		t.Fatalf("audit records=%d, want ≥3 (ban,unban,pardon + system)", len(audit))
	}
}

func TestUsageCountersMultiInterval(t *testing.T) {
	l, _, _ := newTestRig(t)
	id := acctIdentity(77)
	for i := 0; i < 3; i++ {
		l.Check(context.Background(), id, 1, i == 0) // one order hit
	}
	u, err := l.Usage(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if u.Tier != TierBasic || u.RatePerSec != 20 {
		t.Fatalf("usage tier/rate: %+v", u)
	}
	if got := u.RawRequests[0].Count; got != 3 {
		t.Fatalf("raw 1s count=%d, want 3", got)
	}
	if got := u.RawRequests[1].Count; got != 3 {
		t.Fatalf("raw 1m count=%d, want 3", got)
	}
	if got := u.Orders[0].Count; got != 1 {
		t.Fatalf("orders 1s count=%d, want 1", got)
	}
	if got := u.RequestWeight[1].Count; got != 3 {
		t.Fatalf("weight 1m count=%d, want 3", got)
	}
}

// stubModeReader feeds the limiter a fixed degradation state.
type stubModeReader struct{ mode exchredis.DegradationMode }

func (s stubModeReader) GetDegradationMode(context.Context) (exchredis.DegradationState, error) {
	return exchredis.DegradationState{Mode: s.mode}, nil
}

func TestThrottledModeReducesLowerTiersFirst(t *testing.T) {
	ck := newTestClock()
	mem := NewMemBackend()
	mem.SetNow(ck.now)
	l := NewLimiter(mem, LimiterOptions{
		Now:      ck.now,
		Fallback: mem,
		Mode:     stubModeReader{mode: exchredis.ModeThrottled},
	})
	// Public throttled at 0.10 → rate 1/s, burst 2x → capacity 2.
	id := ipIdentity("203.0.113.50")
	for i := 0; i < 2; i++ {
		if res, _ := l.Check(context.Background(), id, 1, false); res.Status != HitOK {
			t.Fatalf("throttled hit %d: %v", i, res.Status)
		}
	}
	res, _ := l.Check(context.Background(), id, 1, false)
	if res.Status != HitRateLimited || res.Limit != 1 {
		t.Fatalf("throttled deny status=%v limit=%d, want 429@1/s",
			res.Status, res.Limit)
	}
	// Institutional is untouched at 1.0 → full 2000/s.
	res, _ = l.Check(context.Background(),
		Identity{Tier: TierInstitutional, Key: "900", IP: "10.0.0.5"}, 1, false)
	if res.Status != HitOK || res.Limit != 2000 {
		t.Fatalf("institutional under Throttled: status=%v limit=%d",
			res.Status, res.Limit)
	}
	// Non-Throttled modes do not throttle.
	l2 := NewLimiter(mem, LimiterOptions{Now: ck.now,
		Mode: stubModeReader{mode: exchredis.ModeReadOnly}})
	res, _ = l2.Check(context.Background(), ipIdentity("203.0.113.51"), 1, false)
	if res.Limit != 5 {
		t.Fatalf("ReadOnly must not throttle: limit=%d", res.Limit)
	}
}

func TestEffectiveRateThrottle(t *testing.T) {
	spec := SpecOf(TierPublic)
	if got := EffectiveRate(spec, DefaultThrottle[TierPublic]); got != 1 {
		// 5 * 0.10 → 0.5 → floored at 1 (live-but-degraded trickle)
		t.Fatalf("public throttled rate=%d, want 1", got)
	}
	if got := EffectiveRate(SpecOf(TierStandard), DefaultThrottle[TierStandard]); got != 50 {
		t.Fatalf("standard throttled rate=%d, want 50", got)
	}
	if got := EffectiveRate(SpecOf(TierInstitutional), DefaultThrottle[TierInstitutional]); got != 2000 {
		t.Fatalf("institutional throttled rate=%d, want 2000 (untouched)", got)
	}
}

// errBackend fails every call — exercises the Limiter failover path.
type errBackend struct{ MemBackend }

var errBackendDown = errors.New("backend down")

func (e *errBackend) Hit(context.Context, HitInput) (HitResult, error) {
	return HitResult{}, errBackendDown
}

func TestLimiterFailsOverToMemBackend(t *testing.T) {
	ck := newTestClock()
	mem := NewMemBackend()
	mem.SetNow(ck.now)
	l := NewLimiter(&errBackend{}, LimiterOptions{Now: ck.now, Fallback: mem})
	res, err := l.Check(context.Background(), ipIdentity("203.0.113.99"), 1, false)
	if err != nil {
		t.Fatalf("failover Check: %v", err)
	}
	if res.Status != HitOK {
		t.Fatalf("failover status=%v, want HitOK", res.Status)
	}
}

func TestBothBackendsDownFailsClosed(t *testing.T) {
	l := NewLimiter(&errBackend{}, LimiterOptions{Fallback: &errBackend{}})
	if _, err := l.Check(context.Background(), ipIdentity("203.0.113.1"), 1, false); err == nil {
		t.Fatal("both backends down must surface an error (fail-closed)")
	}
}

// Task 14.3.6 — the UtilizationObserver fires once per identity per
// window-second when the 1s window count reaches 80% of the effective
// rate; denied hits count too (charged), and a fresh window re-arms.
func TestUtilizationObserver80Percent(t *testing.T) {
	ck := newTestClock()
	mem := NewMemBackend()
	mem.SetNow(ck.now)
	type obs struct {
		key   string
		limit int64
		count int64
	}
	var seen []obs
	l := NewLimiter(mem, LimiterOptions{
		Now: ck.now, Fallback: mem,
		Utilization: func(id Identity, limit, windowCount int64) {
			seen = append(seen, obs{id.Key, limit, windowCount})
		},
	})
	id := ipIdentity("203.0.113.60") // Public: 5/s → 80% = 4
	for i := 0; i < 3; i++ {         // counts 1,2,3 — under 80%
		if _, err := l.Check(context.Background(), id, 1, false); err != nil {
			t.Fatal(err)
		}
	}
	if len(seen) != 0 {
		t.Fatalf("emitted below 80%%: %+v", seen)
	}
	if _, err := l.Check(context.Background(), id, 1, false); err != nil {
		t.Fatal(err) // count 4 = 80% → emit once
	}
	if len(seen) != 1 || seen[0].count != 4 || seen[0].limit != 5 {
		t.Fatalf("one emission at count=4/limit=5 expected: %+v", seen)
	}
	// Same window-second: further crossings must not re-emit.
	for i := 0; i < 6; i++ {
		_, _ = l.Check(context.Background(), id, 1, false)
	}
	if len(seen) != 1 {
		t.Fatalf("dedup must hold within the window: %d emissions", len(seen))
	}
	// Next second: the counter restarts — crossing 80% re-arms.
	ck.advance(time.Second)
	for i := 0; i < 4; i++ {
		_, _ = l.Check(context.Background(), id, 1, false)
	}
	if len(seen) != 2 {
		t.Fatalf("next-window crossing must emit again: %d", len(seen))
	}
	// A different identity crossing in the same window emits its own.
	id2 := acctIdentity(7) // Basic: 20/s → 80% = 16
	for i := 0; i < 16; i++ {
		_, _ = l.Check(context.Background(), id2, 1, false)
	}
	if len(seen) != 3 || seen[2].key != "7" {
		t.Fatalf("per-identity emission expected: %+v", seen)
	}
}

func TestAuthenticatedKeysIsolateAccounts(t *testing.T) {
	l, _, _ := newTestRig(t)
	a := Identity{Tier: TierBasic, Key: "1", IP: "10.0.1.1"}
	b := Identity{Tier: TierBasic, Key: "2", IP: "10.0.1.2"} // distinct IP:
	// a's 429 marker must not spill onto b's offense tracking
	for i := 0; i < 40; i++ { // Basic capacity is 40 (20×2)
		if res, _ := l.Check(context.Background(), a, 1, false); res.Status != HitOK {
			t.Fatalf("acct1 hit %d denied", i)
		}
	}
	if res, _ := l.Check(context.Background(), a, 1, false); res.Status != HitRateLimited {
		t.Fatalf("acct1 hit 41 status=%v", res.Status)
	}
	if res, _ := l.Check(context.Background(), b, 1, false); res.Status != HitOK {
		t.Fatalf("acct2 must have an independent bucket")
	}
}

// TestPrivateIPNotBanned verifies that private/LAN IPs (loopback, RFC 1918,
// link-local) can be rate-limited but NEVER escalated to an IP_BANNED 418 —
// even after repeated post-429 abuse that would normally trigger the
// Task 5.3.34 escalation ladder. Locking out a loopback/LAN client would
// cut off the local operator and every NAT'ed workstation at the edge.
func TestPrivateIPNotBanned(t *testing.T) {
	privateIPs := []string{
		"127.0.0.1", "127.0.0.53", "127.255.255.255", // loopback
		"10.0.0.1", "10.255.255.255", // RFC 1918 Class A
		"172.16.0.1", "172.31.255.255", // RFC 1918 Class B
		"192.168.0.1", "192.168.255.255", // RFC 1918 Class C
		"169.254.1.1", // link-local
		"::1", "fd00::1", // IPv6 loopback + ULA
	}
	for _, ip := range privateIPs {
		l, mem, ck := newTestRig(t)
		id := ipIdentity(ip)
		// Slam well past Public capacity to arm the 429 marker.
		for i := 0; i < 15; i++ {
			l.Check(context.Background(), id, 1, false)
		}
		// Follow-up request would normally be a ban offense for a public IP.
		res, err := l.Check(context.Background(), id, 1, false)
		if err != nil {
			t.Fatalf("%s: %v", ip, err)
		}
		if res.Status == HitBanned || res.Status == HitBannedNew {
			t.Fatalf("%s: got %v — private IP must never be banned", ip, res.Status)
		}
		// BanInfo must be clean for the admin surface.
		if bi, _ := mem.BanInfo(context.Background(), ip); bi != nil {
			t.Fatalf("%s: BanInfo=%+v, want nil", ip, bi)
		}
		// Confirm it's still rate-limited (just not banned) — bucket drained.
		if res.Status != HitRateLimited && res.Status != HitWeightExceeded {
			// After refill it may be OK; advance and re-check the next
			// window is also clean (no stale strike carrying over).
			ck.advance(2 * time.Second)
			res2, _ := l.Check(context.Background(), id, 1, false)
			if res2.Status == HitBanned || res2.Status == HitBannedNew {
				t.Fatalf("%s: post-refill %v — private IP leaking into ban state", ip, res2.Status)
			}
		}
	}
}

// TestPrivateIPRateLimitedStillWorks confirms the token bucket still gates
// private IPs — only the ban machinery is exempt, not rate limiting.
func TestPrivateIPRateLimitedStillWorks(t *testing.T) {
	l, _, _ := newTestRig(t)
	id := ipIdentity("10.0.0.7")
	// Public: rate 5/s, burst 2x → 10 tokens.
	for i := 0; i < 10; i++ {
		if res, _ := l.Check(context.Background(), id, 1, false); res.Status != HitOK {
			t.Fatalf("private hit %d denied early", i)
		}
	}
	if res, _ := l.Check(context.Background(), id, 1, false); res.Status != HitRateLimited {
		t.Fatalf("private IP must still be rate-limited at capacity: got %v", res.Status)
	}
}
