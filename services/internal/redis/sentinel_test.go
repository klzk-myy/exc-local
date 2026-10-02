// Integration + unit tests for the Sentinel failover client (Task 1.3.9,
// spec §4.5 / §24 #181).
//
// Two run modes, selected entirely by env:
//
//	a) In-network (production-shaped): run inside a container attached to
//	   exc-dev_redis-ha (pinned sentinel IPs — container names also resolve) with
//	     EXC_SENTINEL_TEST=1
//	     EXC_SENTINEL_ADDRS=10.99.0.21:26379,10.99.0.22:26379,10.99.0.23:26379
//	     EXC_SENTINEL_RESOLVE_MODE=as_announced
//	   Announced addrs (10.99.0.11-13:6379) are routable on exc-dev_redis-ha.
//
//	b) Host-side dev: run on the host with
//	     EXC_SENTINEL_TEST=1
//	     EXC_SENTINEL_ADDRS=127.0.0.1:36379,127.0.0.1:36380,127.0.0.1:36381
//	     EXC_SENTINEL_RESOLVE_MODE=host_probe
//	     EXC_REDIS_HOST_ADDRS=127.0.0.1:16379,127.0.0.1:16380,127.0.0.1:16381
//	   The resolver joins Sentinel's runid records to host ports.
//
// The failover drill (TestFailoverDrill) additionally needs
// EXC_REDIS_FAILOVER_DRILL=1 and an external operator to stop the primary
// while the test polls: `sudo docker stop exc-dev-redis-primary-1`.
package redis

import (
	"context"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

func csvEnv(name, def string) []string {
	v := os.Getenv(name)
	if v == "" {
		v = def
	}
	var out []string
	for _, p := range strings.Split(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// failoverTestConfig builds the FailoverConfig from env, mirroring the
// config/redis-sentinel.yaml dev defaults.
func failoverTestConfig(t *testing.T) FailoverConfig {
	t.Helper()
	if os.Getenv("EXC_SENTINEL_TEST") != "1" {
		t.Skip("set EXC_SENTINEL_TEST=1 to run Sentinel integration tests")
	}
	sentinels := csvEnv("EXC_SENTINEL_ADDRS",
		"127.0.0.1:36379,127.0.0.1:36380,127.0.0.1:36381")
	hostAddrs := csvEnv("EXC_REDIS_HOST_ADDRS",
		"127.0.0.1:16379,127.0.0.1:16380,127.0.0.1:16381")
	mode := SentinelResolveMode(os.Getenv("EXC_SENTINEL_RESOLVE_MODE"))
	if mode == "" {
		// Loopback sentinel addrs imply a host-side run needing the probe;
		// otherwise assume the process is in-network.
		if strings.HasPrefix(sentinels[0], "127.") || strings.HasPrefix(sentinels[0], "localhost") {
			mode = ResolveHostProbe
		} else {
			mode = ResolveAsAnnounced
		}
	}
	master := os.Getenv("EXC_SENTINEL_MASTER")
	if master == "" {
		master = "mymaster"
	}
	// Test keyspace isolation — never default to DB 0, the live
	// gateway's keyspace. db 13 matches the general test-client
	// convention; EXC_SENTINEL_TEST_DB overrides when a caller wants
	// a dedicated DB.
	testDB := 13
	if v := os.Getenv("EXC_SENTINEL_TEST_DB"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			testDB = n
		}
	}
	return FailoverConfig{
		MasterName:    master,
		SentinelAddrs: sentinels,
		DB:            testDB,
		PoolSize:      10,
		DialTimeout:   2 * time.Second,
		ReadTimeout:   2 * time.Second,
		WriteTimeout:  2 * time.Second,
		ResolveMode:   mode,
		AnnounceMap: map[string]string{
			"10.99.0.11:6379":      "127.0.0.1:16379",
			"10.99.0.12:6379":      "127.0.0.1:16380",
			"10.99.0.13:6379":      "127.0.0.1:16381",
			"redis-primary:6379":   "127.0.0.1:16379",
			"redis-replica-1:6379": "127.0.0.1:16380",
			"redis-replica-2:6379": "127.0.0.1:16381",
		},
		HostAddrs: hostAddrs,
	}
}

func failoverTestClient(t *testing.T) *FailoverClient {
	t.Helper()
	cfg := failoverTestConfig(t)
	fc, err := NewFailoverClient(cfg)
	if err != nil {
		t.Fatalf("NewFailoverClient: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := fc.Ping(ctx); err != nil {
		_ = fc.Close()
		t.Skipf("sentinel topology unreachable (%s): %v", cfg.SentinelAddrs, err)
	}
	t.Cleanup(func() { _ = fc.Close() })
	return fc
}

// ---------------------------------------------------------------------------
// Unit tests (ungated): validation + resolver translation.
// ---------------------------------------------------------------------------

func TestFailoverConfigValidation(t *testing.T) {
	base := FailoverConfig{
		MasterName:    "mymaster",
		SentinelAddrs: []string{"127.0.0.1:26379"},
		ResolveMode:   ResolveAsAnnounced,
	}

	bad := []struct {
		name string
		mut  func(*FailoverConfig)
	}{
		{"empty master", func(c *FailoverConfig) { c.MasterName = " " }},
		{"no sentinels", func(c *FailoverConfig) { c.SentinelAddrs = nil }},
		{"bad sentinel addr", func(c *FailoverConfig) { c.SentinelAddrs = []string{"nohostport"} }},
		{"unknown mode", func(c *FailoverConfig) { c.ResolveMode = "bogus" }},
		{"announce_map empty", func(c *FailoverConfig) { c.ResolveMode = ResolveAnnounceMap }},
		{"announce_map bad value", func(c *FailoverConfig) {
			c.ResolveMode = ResolveAnnounceMap
			c.AnnounceMap = map[string]string{"a:1": "notaddr"}
		}},
		{"host_probe no addrs", func(c *FailoverConfig) { c.ResolveMode = ResolveHostProbe }},
		{"host_probe bad addr", func(c *FailoverConfig) {
			c.ResolveMode = ResolveHostProbe
			c.HostAddrs = []string{"missing:port:"}
		}},
		{"negative retries", func(c *FailoverConfig) { c.MaxRetries = -1 }},
	}
	for _, tc := range bad {
		c := base
		tc.mut(&c)
		if _, err := NewFailoverClient(c); err == nil {
			t.Errorf("%s: expected construction error, got nil", tc.name)
		}
	}

	// Valid config constructs lazily (no dial) and closes cleanly.
	c := base
	fc, err := NewFailoverClient(c)
	if err != nil {
		t.Fatalf("valid config: %v", err)
	}
	if fc.Client == nil {
		t.Fatal("valid config: nil inner client")
	}
	if err := fc.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}

func TestSentinelAddrResolver(t *testing.T) {
	cfg := FailoverConfig{
		MasterName:    "mymaster",
		SentinelAddrs: []string{"127.0.0.1:26379", "127.0.0.1:26380"},
		ResolveMode:   ResolveAsAnnounced,
		AnnounceMap:   map[string]string{"redis-primary:6379": "127.0.0.1:16379"},
	}
	if err := cfg.validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	r := newSentinelAddrResolver(cfg)
	ctx := context.Background()

	// Sentinel addrs always pass through untouched (the dialer is shared
	// with sentinel connections).
	for _, s := range cfg.SentinelAddrs {
		got, err := r.resolve(ctx, s)
		if err != nil || got != s {
			t.Fatalf("sentinel addr %s: got %q err %v", s, got, err)
		}
	}
	// as_announced passes addrs verbatim — including ones present in the
	// static map (the map must not shadow this mode).
	for _, a := range []string{"redis-primary:6379", "172.18.0.7:6379"} {
		got, err := r.resolve(ctx, a)
		if err != nil || got != a {
			t.Fatalf("as_announced %s: got %q err %v", a, got, err)
		}
	}

	// announce_map mode: hit rewrites, miss fails closed.
	cfg.ResolveMode = ResolveAnnounceMap
	r = newSentinelAddrResolver(cfg)
	got, err := r.resolve(ctx, "redis-primary:6379")
	if err != nil || got != "127.0.0.1:16379" {
		t.Fatalf("announce_map hit: got %q err %v", got, err)
	}
	if _, err := r.resolve(ctx, "172.18.0.99:6379"); err == nil {
		t.Fatal("announce_map miss should fail closed")
	}

	// OnSwitch fires on announced-addr change, not on first observation.
	switched := make(chan string, 2)
	cfg.ResolveMode = ResolveAsAnnounced
	cfg.OnSwitch = func(a string) { switched <- a }
	r = newSentinelAddrResolver(cfg)
	_, _ = r.resolve(ctx, "172.18.0.7:6379") // first observation: no switch
	select {
	case a := <-switched:
		t.Fatalf("OnSwitch fired on first observation (%s)", a)
	case <-time.After(20 * time.Millisecond):
	}
	_, _ = r.resolve(ctx, "172.18.0.9:6379") // master switched
	select {
	case a := <-switched:
		if a != "172.18.0.9:6379" {
			t.Fatalf("OnSwitch addr %q", a)
		}
	case <-time.After(time.Second):
		t.Fatal("OnSwitch did not fire on master switch")
	}
}

// ---------------------------------------------------------------------------
// Gated integration tests — require the live sentinel topology.
// ---------------------------------------------------------------------------

func TestFailoverClientBasics(t *testing.T) {
	fc := failoverTestClient(t)
	ctx := testCtx(t)

	// Resolved master addr observed through the dialer.
	if a := fc.AnnouncedMasterAddr(); a == "" {
		t.Fatal("AnnouncedMasterAddr empty after Ping — dialer never resolved")
	} else {
		t.Logf("announced master addr: %s", a)
	}

	// We are really talking to the master, not a replica.
	info, err := fc.Do(ctx, "INFO", "replication").Text()
	if err != nil {
		t.Fatalf("INFO replication: %v", err)
	}
	if !strings.Contains(info, "role:master") {
		t.Fatalf("failover client not connected to master:\n%s", info)
	}

	// Same key space / helpers as the plain Client (spec §4 contract).
	token := "ft-" + uniq(t)
	t.Cleanup(func() { fc.Del(context.Background(), sessionKey(token)) })
	want := Session{UserID: "u-ha", AccountID: "a-ha", Tier: "T1"}
	if err := fc.SetSession(ctx, token, want); err != nil {
		t.Fatalf("SetSession via failover client: %v", err)
	}
	got, err := fc.GetSession(ctx, token)
	if err != nil || got == nil || *got != want {
		t.Fatalf("GetSession via failover client: got %+v err %v", got, err)
	}

	// Leader-lease key space reachable through the HA path.
	shard := 9500 + int(time.Now().UnixNano()%400)
	key := leaderKey(shard)
	t.Cleanup(func() { fc.Del(context.Background(), key) })
	ok, err := fc.TryAcquireLeader(ctx, shard, "ha-node", 1, 0)
	if err != nil || !ok {
		t.Fatalf("TryAcquireLeader via failover client: ok=%v err=%v", ok, err)
	}

	// Active health probe: at least one successful round-trip observed.
	reports := make(chan error, 4)
	stop := fc.StartHealthProbe(context.Background(), 30*time.Millisecond,
		func(_ time.Duration, err error) { reports <- err })
	time.Sleep(120 * time.Millisecond)
	stop()
	select {
	case err := <-reports:
		if err != nil {
			t.Fatalf("health probe reported error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("health probe produced no report")
	}
}

// TestFailoverDrill is the master-switch measurement. While the test
// polls, an operator runs: sudo docker stop exc-dev-redis-primary-1.
// Sentinel (down-after 2000ms, quorum 2) promotes a replica ~2-3s later;
// the drill measures switch->first-successful-command (<100ms target) and
// verifies the session written pre-failover survives on the new master.
func TestFailoverDrill(t *testing.T) {
	if os.Getenv("EXC_REDIS_FAILOVER_DRILL") != "1" {
		t.Skip("set EXC_REDIS_FAILOVER_DRILL=1 (and stop the primary externally) to run the failover drill")
	}
	cfg := failoverTestConfig(t)
	switchedCh := make(chan string, 4)
	cfg.OnSwitch = func(a string) { switchedCh <- a }
	fc, err := NewFailoverClient(cfg)
	if err != nil {
		t.Fatalf("NewFailoverClient: %v", err)
	}
	t.Cleanup(func() { _ = fc.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := fc.Ping(ctx); err != nil {
		t.Skipf("sentinel topology unreachable: %v", err)
	}

	// Session written on the OLD master, must survive promotion.
	token := "drill-" + uniq(t)
	want := Session{UserID: "u-drill", AccountID: "a-drill", Tier: "T2"}
	if err := fc.SetSession(ctx, token, want); err != nil {
		t.Fatalf("SetSession pre-failover: %v", err)
	}
	t.Cleanup(func() { fc.Del(context.Background(), sessionKey(token)) })

	initial, err := SentinelMasterAddr(ctx, cfg.SentinelAddrs[0], cfg.MasterName)
	if err != nil {
		t.Fatalf("initial master addr: %v", err)
	}
	t.Logf("initial master (sentinel view): %s; announced via dialer: %s",
		initial, fc.AnnouncedMasterAddr())

	wait := 45 * time.Second
	if v := os.Getenv("EXC_FAILOVER_DRILL_WAIT"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			wait = d
		}
	}
	deadline := time.Now().Add(wait)
	loopStart := time.Now()

	var switchAt, firstErrAt, firstSuccessAt time.Time
	var switchAddr string
	polls := 0
	for time.Now().Before(deadline) {
		polls++
		// Watchdog: sentinel's own view of the master.
		pctx, pcancel := context.WithTimeout(context.Background(), time.Second)
		cur, serr := SentinelMasterAddr(pctx, cfg.SentinelAddrs[0], cfg.MasterName)
		pcancel()
		if serr == nil && cur != initial && switchAt.IsZero() {
			switchAt = time.Now()
			switchAddr = cur
			t.Logf("master switch detected via sentinel: %s -> %s", initial, cur)
		}
		// Data path: keep reading the session.
		cctx, ccancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
		got, cerr := fc.GetSession(cctx, token)
		ccancel()
		if cerr != nil {
			if firstErrAt.IsZero() {
				firstErrAt = time.Now()
				t.Logf("first command error at loop T+%v: %v", firstErrAt.Sub(loopStart), cerr)
			}
		} else if !switchAt.IsZero() {
			firstSuccessAt = time.Now()
			if got == nil || *got != want {
				t.Fatalf("session lost across failover: got %+v want %+v", got, want)
			}
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	if switchAt.IsZero() {
		t.Fatalf("no master switch observed within %v — was the primary stopped?", wait)
	}
	if firstSuccessAt.IsZero() {
		t.Fatal("commands never succeeded after master switch")
	}
	reconnect := firstSuccessAt.Sub(switchAt)
	t.Logf("failover timing: switch_at=%v reconnect=%v first_err_at=%v polls=%d",
		switchAt.Format("15:04:05.000"), reconnect, firstErrAt.Format("15:04:05.000"), polls)
	if reconnect > 100*time.Millisecond {
		t.Fatalf("reconnect %v exceeds 100ms target", reconnect)
	}

	// Post-switch: lease acquisition works on the new master (same key
	// space the C++ LeaderElection will use in Phase-02).
	shard := 9700 + int(time.Now().UnixNano()%200)
	key := leaderKey(shard)
	t.Cleanup(func() { fc.Del(context.Background(), key) })
	ok, err := fc.TryAcquireLeader(context.Background(), shard, "drill-node", 7, 0)
	if err != nil || !ok {
		t.Fatalf("TryAcquireLeader on new master: ok=%v err=%v", ok, err)
	}
	t.Logf("leader lease acquired on new master (%s); announced=%s", switchAddr, fc.AnnouncedMasterAddr())

	select {
	case a := <-switchedCh:
		t.Logf("OnSwitch observed new announced addr: %s", a)
	case <-time.After(200 * time.Millisecond):
		t.Log("OnSwitch not observed (pool may have kept a pre-switch conn)")
	}
}
