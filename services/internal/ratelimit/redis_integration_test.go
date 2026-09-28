// Tasks 5.3.2/5.3.34/5.3.40 — live-Redis integration tests for
// RedisBackend, gated on EXC_REDIS_TEST=1 + EXC_REDIS_TEST_ADDR.
// Verifies the real Lua script end-to-end: canonical keys, TTLs,
// escalation schedule, audit trail, and usage hash layout.
package ratelimit

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"
)

func redisTestClient(t *testing.T) *goredis.Client {
	t.Helper()
	if os.Getenv("EXC_REDIS_TEST") != "1" {
		t.Skip("EXC_REDIS_TEST != 1")
	}
	addr := os.Getenv("EXC_REDIS_TEST_ADDR")
	if addr == "" {
		addr = "127.0.0.1:16379"
	}
	rdb := goredis.NewClient(&goredis.Options{
		Addr:     addr,
		Password: os.Getenv("EXC_REDIS_TEST_PASSWORD"),
	})
	if err := rdb.Ping(context.Background()).Err(); err != nil {
		t.Skipf("redis unreachable at %s: %v", addr, err)
	}
	t.Cleanup(func() { _ = rdb.Close() })
	return rdb
}

// testHitInput builds a deterministic HitInput at a fixed instant.
func testHitInput(ip, key string, tier Tier, weight int64, order bool) HitInput {
	spec := SpecOf(tier)
	return HitInput{
		Tier:        tier,
		Key:         key,
		IP:          ip,
		Weight:      weight,
		Order:       order,
		Rate:        spec.RatePerSec,
		WeightQuota: spec.WeightPerMin,
		Now:         time.Now(),
	}
}

func flushTestKeys(t *testing.T, rdb *goredis.Client, ip, key string, tier Tier) {
	ctx := context.Background()
	pat := fmt.Sprintf("rl:%s:%s:*", tier, key)
	for _, k := range []string{
		banKey(ip), markerKey(ip), strikesKey(ip),
		bucketKey(tier, key), usageKey(key),
	} {
		rdb.Del(ctx, k)
	}
	// window keys are per-second; scan-clean them.
	for _, k := range rdb.Keys(ctx, pat).Val() {
		rdb.Del(ctx, k)
	}
	rdb.SRem(ctx, allowlistKey, ip)
}

func TestRedisHitAndKeys(t *testing.T) {
	rdb := redisTestClient(t)
	ctx := context.Background()
	ip, key := "192.0.2.10", "it-42"
	flushTestKeys(t, rdb, ip, key, TierPublic)

	b := NewRedisBackend(rdb)
	in := testHitInput(ip, key, TierPublic, 1, false)

	res, err := b.Hit(ctx, in)
	if err != nil || res.Status != HitOK {
		t.Fatalf("first hit: %v %v", res.Status, err)
	}
	secWin := in.Now.UnixMilli() / 1000
	wk := windowKey(TierPublic, key, secWin)
	if v := rdb.Get(ctx, wk).Val(); v != "1" {
		t.Fatalf("%s=%q, want 1", wk, v)
	}
	if ttl := rdb.PTTL(ctx, wk).Val(); ttl <= 0 || ttl > RateWindowTTL {
		t.Fatalf("%s ttl=%v, want (0, 2s]", wk, ttl)
	}
	if !rdb.HExists(ctx, bucketKey(TierPublic, key), "tokens").Val() {
		t.Fatal("bucket hash missing")
	}
	// Usage hash must carry the multi-interval fields (Task 5.3.40).
	f := rdb.HGetAll(ctx, usageKey(key)).Val()
	sec := fmt.Sprint(in.Now.Unix())
	min := fmt.Sprint(in.Now.Unix() / 60)
	day := fmt.Sprint(in.Now.Unix() / 86400)
	for _, want := range []string{
		"raw:s:" + sec, "raw:m:" + min, "raw:d:" + day,
		"w:s:" + sec, "w:m:" + min, "w:d:" + day,
	} {
		if _, ok := f[want]; !ok {
			t.Fatalf("usage field %q missing (have %v)", want, f)
		}
	}
	if got := f["raw:s:"+sec]; got != "1" {
		t.Fatalf("raw:s count=%q want 1", got)
	}
	// ORDERS counters only increment on order paths.
	if _, ok := f["ord:s:"+sec]; ok {
		t.Fatal("ord counter must not increment for non-order hit")
	}
	in2 := testHitInput(ip, key, TierPublic, 1, true)
	if _, err := b.Hit(ctx, in2); err != nil {
		t.Fatal(err)
	}
	if got := rdb.HGet(ctx, usageKey(key), "ord:s:"+fmt.Sprint(in2.Now.Unix())).Val(); got != "1" {
		t.Fatalf("ord:s count=%q want 1", got)
	}
}

func TestRedisBurstAndDeny(t *testing.T) {
	rdb := redisTestClient(t)
	ctx := context.Background()
	ip, key := "192.0.2.11", "it-43"
	flushTestKeys(t, rdb, ip, key, TierPublic)

	b := NewRedisBackend(rdb)
	var res HitResult
	var err error
	for i := 0; i < 10; i++ {
		res, err = b.Hit(ctx, testHitInput(ip, key, TierPublic, 1, false))
		if err != nil || res.Status != HitOK {
			t.Fatalf("hit %d: %v %v", i, res.Status, err)
		}
	}
	res, err = b.Hit(ctx, testHitInput(ip, key, TierPublic, 1, false))
	if err != nil || res.Status != HitRateLimited {
		t.Fatalf("hit 11: %v %v, want HitRateLimited", res.Status, err)
	}
	if res.RetryAfter < 1 {
		t.Fatalf("RetryAfter=%d", res.RetryAfter)
	}
	// Deny armed the rl429 marker.
	if rdb.Exists(ctx, markerKey(ip)).Val() != 1 {
		t.Fatal("rl429 marker missing after deny")
	}
	if ttl := rdb.PTTL(ctx, markerKey(ip)).Val(); ttl <= 0 || ttl > offenseWindow {
		t.Fatalf("marker ttl=%v, want (0, 60s]", ttl)
	}
}

func TestRedisBanEscalationAndTTL(t *testing.T) {
	rdb := redisTestClient(t)
	ctx := context.Background()
	ip, key := "192.0.2.12", "it-44"
	flushTestKeys(t, rdb, ip, key, TierPublic)

	b := NewRedisBackend(rdb)
	// 11 hits: deny at #11 arms the marker; #12 is the post-429 offense.
	for i := 0; i < 11; i++ {
		b.Hit(ctx, testHitInput(ip, key, TierPublic, 1, false))
	}
	res, err := b.Hit(ctx, testHitInput(ip, key, TierPublic, 1, false))
	if err != nil || res.Status != HitBannedNew {
		t.Fatalf("offense: %v %v, want HitBannedNew", res.Status, err)
	}
	if res.Ban == nil || res.Ban.Level != 1 {
		t.Fatalf("ban=%+v, want level 1", res.Ban)
	}
	// ip_ban:{ip} key with TTL ≈ 2min (Task 5.3.34 canonical key).
	raw := rdb.Get(ctx, banKey(ip)).Val()
	if raw == "" {
		t.Fatal("ip_ban key missing")
	}
	ttl := rdb.PTTL(ctx, banKey(ip)).Val()
	if ttl <= 100*time.Second || ttl > BanSchedule[0] {
		t.Fatalf("ban ttl=%v, want ≈120s", ttl)
	}
	// Audit trail captured the system ban.
	recs, _ := b.ListAudit(ctx, 10)
	found := false
	for _, r := range recs {
		if r == raw {
			found = true
		}
	}
	if !found {
		t.Fatal("system ban not audited in ip_ban_audit")
	}
	// While banned: HitBanned (no new strike).
	res2, _ := b.Hit(ctx, testHitInput(ip, key, TierPublic, 1, false))
	if res2.Status != HitBanned {
		t.Fatalf("during ban: %v, want HitBanned", res2.Status)
	}
	if rdb.Get(ctx, strikesKey(ip)).Val() != "1" {
		t.Fatalf("strikes=%q, want 1 (no accrual during ban)", rdb.Get(ctx, strikesKey(ip)).Val())
	}
	// Admin override: unban + pardon.
	adm := &BanAdmin{B: b}
	if ok, err := adm.UnbanIP(ctx, ip, "t-op"); err != nil || !ok {
		t.Fatalf("unban: %v %v", ok, err)
	}
	if err := adm.PardonIP(ctx, ip, "t-op"); err != nil {
		t.Fatal(err)
	}
	if rdb.Get(ctx, strikesKey(ip)).Val() != "" {
		t.Fatal("pardon must clear strikes")
	}
	res3, _ := b.Hit(ctx, testHitInput(ip, key, TierPublic, 1, false))
	if res3.Status == HitBanned || res3.Status == HitBannedNew {
		t.Fatalf("post-pardon status=%v", res3.Status)
	}
}

func TestRedisAllowlistSkipsBanMachinery(t *testing.T) {
	rdb := redisTestClient(t)
	ctx := context.Background()
	ip, key := "192.0.2.13", "it-45"
	flushTestKeys(t, rdb, ip, key, TierPublic)

	b := NewRedisBackend(rdb)
	adm := &BanAdmin{B: b}
	if err := adm.AllowlistIP(ctx, ip, "t-op"); err != nil {
		t.Fatal(err)
	}
	defer adm.UnallowlistIP(ctx, ip, "t-op")

	for i := 0; i < 13; i++ {
		b.Hit(ctx, testHitInput(ip, key, TierPublic, 1, false))
	}
	res, _ := b.Hit(ctx, testHitInput(ip, key, TierPublic, 1, false))
	if res.Status == HitBanned || res.Status == HitBannedNew {
		t.Fatalf("allowlisted IP banned: %v", res.Status)
	}
}
