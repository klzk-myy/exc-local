// supervisor_test.go — heartbeat-timeout → leader-revocation logic for the
// exchange-watchdogd supervisor (Task 9.3.28, spec §19.13.3 tier 3).
// All seams are faked: ring headers, pid liveness, the Redis lease store,
// the mode setter, demote, statfs and the wall clock.
package watchdog

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"exchange/internal/ipc"
	"exchange/internal/observability"
	exredis "exchange/internal/redis"
)

// ---------------------------------------------------------------------------
// fakes
// ---------------------------------------------------------------------------

type fakeRings struct {
	mu     sync.Mutex
	inHdr  map[int]ipc.RingHeader
	outHdr map[int]ipc.RingHeader
	inErr  map[int]error
	outErr map[int]error
}

func newFakeRings() *fakeRings {
	return &fakeRings{
		inHdr:  map[int]ipc.RingHeader{},
		outHdr: map[int]ipc.RingHeader{},
		inErr:  map[int]error{},
		outErr: map[int]error{},
	}
}

func (f *fakeRings) reader() RingReader {
	return func(path string) (ipc.RingHeader, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		base := filepath.Base(path)
		var shard int
		if _, err := fmt.Sscanf(base, "testbase_%d_", &shard); err != nil {
			// fall back: scan any _N_ infix
			var found bool
			for i := 0; i < 64 && !found; i++ {
				if strings.Contains(base, fmt.Sprintf("_%d_", i)) {
					shard, found = i, true
				}
			}
			if !found {
				return ipc.RingHeader{}, fmt.Errorf("bad path %s", path)
			}
		}
		if strings.HasSuffix(base, "_in") {
			if err := f.inErr[shard]; err != nil {
				return ipc.RingHeader{}, err
			}
			return f.inHdr[shard], nil
		}
		if err := f.outErr[shard]; err != nil {
			return ipc.RingHeader{}, err
		}
		return f.outHdr[shard], nil
	}
}

type fakeLeases struct {
	mu           sync.Mutex
	val          string
	ok           bool
	leaderErr    error
	revokeErr    error
	revokeResult bool // CAS outcome
	revokeCalls  []string
}

func (f *fakeLeases) LeaderValue(_ context.Context, _ int) (string, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.val, f.ok, f.leaderErr
}

func (f *fakeLeases) RevokeLeader(_ context.Context, _ int, expect string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.revokeCalls = append(f.revokeCalls, expect)
	if f.revokeErr != nil {
		return false, f.revokeErr
	}
	if f.revokeResult {
		f.ok = false // CAS delete consumed the lease
	}
	return f.revokeResult, nil
}

func (f *fakeLeases) calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.revokeCalls...)
}

type fakeModes struct {
	mu    sync.Mutex
	calls []exredis.DegradationMode
	err   error
}

func (f *fakeModes) SetDegradationMode(_ context.Context, m exredis.DegradationMode, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, m)
	return f.err
}

func (f *fakeModes) got() []exredis.DegradationMode {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]exredis.DegradationMode(nil), f.calls...)
}

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}
func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

type env struct {
	t      *testing.T
	clk    *fakeClock
	rings  *fakeRings
	leases *fakeLeases
	modes  *fakeModes
	alive  map[uint64]bool
	demote []uint64
	sup    *Supervisor
	reg    *observability.Registry
}

func newEnv(t *testing.T) *env {
	t.Helper()
	clk := &fakeClock{now: time.Unix(1_700_000_000, 0)}
	rings := newFakeRings()
	e := &env{
		t: t, clk: clk, rings: rings,
		leases: &fakeLeases{revokeResult: true},
		modes:  &fakeModes{},
		alive:  map[uint64]bool{},
		reg:    observability.New(),
	}
	e.sup = New(Config{
		Shards:       []int{0},
		ShmDir:       "/dev/shm",
		ShmBase:      "testbase",
		PollInterval: 100 * time.Millisecond,
		StallTimeout: 3 * time.Second,
		WalDir:       "", // disk probe off unless a test enables it
		DemoteSig:    15, // SIGTERM
		Now:          clk.Now,
		Log:          slog.New(slog.NewTextHandler(io.Discard, nil)),
		ReadRing:     rings.reader(),
		PidAlive:     func(pid uint64) bool { return e.alive[pid] },
		Demote:       func(pid uint64) error { e.demote = append(e.demote, pid); return nil },
	}, e.reg, e.leases, e.modes, nil)
	return e
}

func (e *env) tick() { e.sup.Tick(context.Background()) }

// engineBeat makes shard 0's out-ring look alive: pid 4242 alive, fresh beat.
func (e *env) engineBeat(pid uint64) {
	e.rings.outHdr[0] = ipc.RingHeader{
		Head:        10,
		Tail:        10,
		HeartbeatNs: uint64(e.clk.Now().UnixNano()),
		ProducerPid: pid,
		Capacity:    4096,
		SlotPayload: 1024,
		Version:     ipc.ShmVersion,
	}
	e.alive[pid] = true
}

// ingressTraffic simulates active ingress: head advances each call.
func (e *env) ingressTraffic() {
	h := e.rings.inHdr[0]
	h.Head++
	h.Capacity = 4096
	e.rings.inHdr[0] = h
}

func (e *env) exposition() string { return e.reg.String() }

// ---------------------------------------------------------------------------
// heartbeat -> revocation contract
// ---------------------------------------------------------------------------

func TestFreshHeartbeatNoFence(t *testing.T) {
	e := newEnv(t)
	e.engineBeat(4242)
	e.leases.val, e.leases.ok = "node-a:7", true

	e.ingressTraffic()
	e.tick()
	e.ingressTraffic()
	e.clk.advance(100 * time.Millisecond)
	e.engineBeat(4242) // engine keeps beating
	e.tick()

	if got := e.leases.calls(); len(got) != 0 {
		t.Fatalf("healthy engine fenced: %v", got)
	}
	if len(e.demote) != 0 {
		t.Fatalf("healthy engine demoted: %v", e.demote)
	}
	if !strings.Contains(e.exposition(), `daemon_up{name="matching-engine",shard="0"} 1`) {
		t.Fatalf("daemon_up not 1:\n%s", e.exposition())
	}
}

func TestStallUnderIngressRevokesLease(t *testing.T) {
	e := newEnv(t)
	e.engineBeat(4242)
	e.leases.val, e.leases.ok = "node-a:7", true

	// Establish baseline, then freeze the heartbeat and keep ingress moving.
	e.ingressTraffic()
	e.tick()
	e.clk.advance(4 * time.Second) // > 3s stall bound, hb never advanced
	e.ingressTraffic()
	e.tick()

	if got := e.leases.calls(); len(got) != 1 || got[0] != "node-a:7" {
		t.Fatalf("revoke calls = %v, want [node-a:7]", got)
	}
	if len(e.demote) != 1 || e.demote[0] != 4242 {
		t.Fatalf("demote calls = %v, want [4242]", e.demote)
	}
	if !strings.Contains(e.exposition(), `watchdog_leader_revocations_total{shard="0"} 1`) {
		t.Fatalf("revocation metric missing:\n%s", e.exposition())
	}
}

func TestStallWithoutIngressDoesNotFence(t *testing.T) {
	e := newEnv(t)
	e.engineBeat(4242)
	e.leases.val, e.leases.ok = "node-a:7", true

	e.tick() // baseline: in head 0, occupancy 0 — idle engine
	e.clk.advance(10 * time.Second)
	e.tick() // stale hb but NO active ingress → idle engine, not hung

	if got := e.leases.calls(); len(got) != 0 {
		t.Fatalf("idle engine fenced: %v", got)
	}
}

func TestDeadProducerFencesRegardlessOfIngress(t *testing.T) {
	e := newEnv(t)
	e.engineBeat(4242)
	e.leases.val, e.leases.ok = "node-a:7", true
	e.tick()

	// Producer dies with the lease held; no ingress activity needed.
	e.alive[4242] = false
	e.tick()

	if got := e.leases.calls(); len(got) != 1 {
		t.Fatalf("dead producer not fenced: %v", got)
	}
}

func TestNoLeaseHeldNothingToRevoke(t *testing.T) {
	e := newEnv(t)
	e.engineBeat(4242)
	e.leases.ok = false // no leader lease at all

	e.ingressTraffic()
	e.tick()
	e.clk.advance(4 * time.Second)
	e.ingressTraffic()
	e.tick()

	if got := e.leases.calls(); len(got) != 0 {
		t.Fatalf("revoked with no lease held: %v", got)
	}
}

func TestNeverBeatenEngineIsUnproven(t *testing.T) {
	e := newEnv(t)
	// Ring exists, producer pid stamped, but heartbeat never written —
	// startup window: cannot prove a stall, must not fence.
	e.rings.outHdr[0] = ipc.RingHeader{
		ProducerPid: 4242, Capacity: 4096, SlotPayload: 1024, Version: ipc.ShmVersion,
	}
	e.alive[4242] = true
	e.leases.val, e.leases.ok = "node-a:7", true

	e.ingressTraffic()
	e.tick()
	e.clk.advance(10 * time.Second)
	e.ingressTraffic()
	e.tick()

	if got := e.leases.calls(); len(got) != 0 {
		t.Fatalf("unproven engine fenced: %v", got)
	}
}

func TestRevokeErrorIsRetriedAndMetered(t *testing.T) {
	e := newEnv(t)
	e.engineBeat(4242)
	e.leases.val, e.leases.ok = "node-a:7", true
	e.leases.revokeErr = errors.New("redis: connection refused")

	e.ingressTraffic()
	e.tick()
	e.clk.advance(4 * time.Second)
	e.ingressTraffic()
	e.tick()

	if got := e.leases.calls(); len(got) != 1 {
		t.Fatalf("revoke not attempted: %v", got)
	}
	if !strings.Contains(e.exposition(), `watchdog_revoke_errors_total{shard="0"} 1`) {
		t.Fatalf("revoke error not metered:\n%s", e.exposition())
	}
	// Lease still held (CAS never ran) — error cleared, next tick retries.
	e.leases.revokeErr = nil
	e.tick()
	if got := e.leases.calls(); len(got) != 2 {
		t.Fatalf("revoke not retried: %v", got)
	}
}

func TestCASMissDoesNotDemote(t *testing.T) {
	e := newEnv(t)
	e.engineBeat(4242)
	e.leases.val, e.leases.ok = "node-a:7", true
	e.leases.revokeResult = false // lease changed under our read

	e.ingressTraffic()
	e.tick()
	e.clk.advance(4 * time.Second)
	e.ingressTraffic()
	e.tick()

	if len(e.demote) != 0 {
		t.Fatalf("demoted on CAS miss: %v", e.demote)
	}
}

func TestGraceWindowProtectsFollowerPromotion(t *testing.T) {
	e := newEnv(t)
	e.engineBeat(4242)
	e.leases.val, e.leases.ok = "node-a:7", true

	e.ingressTraffic()
	e.tick()
	e.clk.advance(4 * time.Second)
	e.ingressTraffic()
	e.tick()
	if got := e.leases.calls(); len(got) != 1 {
		t.Fatalf("primary not fenced: %v", got)
	}

	// Follower acquires the lease at a new epoch but hasn't written to the
	// ring yet — heartbeat still stale. Grace window must suppress re-fence.
	e.leases.val, e.leases.ok = "node-b:8", true
	e.ingressTraffic()
	e.tick()
	if got := e.leases.calls(); len(got) != 1 {
		t.Fatalf("follower fenced inside grace window: %v", got)
	}
}

func TestPidChangeResetsTracking(t *testing.T) {
	e := newEnv(t)
	e.engineBeat(4242)
	e.leases.val, e.leases.ok = "node-a:7", true

	e.ingressTraffic()
	e.tick()
	e.clk.advance(4 * time.Second)
	e.ingressTraffic()
	e.tick()
	if got := e.leases.calls(); len(got) != 1 {
		t.Fatalf("primary not fenced: %v", got)
	}

	// Engine restarts: fresh pid, heartbeat not yet stamped.
	e.clk.advance(4 * time.Second) // past grace
	e.rings.outHdr[0] = ipc.RingHeader{
		ProducerPid: 5555, Capacity: 4096, SlotPayload: 1024, Version: ipc.ShmVersion,
	}
	e.alive[5555] = true
	e.leases.val, e.leases.ok = "node-b:8", true
	e.ingressTraffic()
	e.tick()

	if got := e.leases.calls(); len(got) != 1 {
		t.Fatalf("restarted engine fenced on predecessor's heartbeat: %v", got)
	}
}

func TestRingVanishedAfterBeatingFences(t *testing.T) {
	e := newEnv(t)
	e.engineBeat(4242)
	e.leases.val, e.leases.ok = "node-a:7", true
	e.ingressTraffic()
	e.tick()

	// Ring image disappears (producer crashed and shm cleaned up); ingress
	// still active. After the stall bound of unreadable samples → fence.
	e.rings.outErr[0] = errors.New("ipc: open /dev/shm/testbase_0_out: no such file")
	for i := 0; i < 5; i++ {
		e.ingressTraffic()
		e.clk.advance(time.Second)
		e.tick()
	}
	if got := e.leases.calls(); len(got) != 1 {
		t.Fatalf("vanished ring not fenced: %v", got)
	}
}

// ---------------------------------------------------------------------------
// watermark, canary, disk
// ---------------------------------------------------------------------------

func TestIngressWatermarkTrips(t *testing.T) {
	e := newEnv(t)
	e.engineBeat(4242)
	e.rings.inHdr[0] = ipc.RingHeader{
		Head: 3900, Tail: 0, Capacity: 4096, SlotPayload: 1024, Version: ipc.ShmVersion,
	}
	e.tick()
	if !strings.Contains(e.exposition(), `watchdog_trips_total{kind="backpressure_crit"`) {
		t.Fatalf("crit watermark trip missing:\n%s", e.exposition())
	}
}

func TestCanaryFailureTripsReadOnly(t *testing.T) {
	e := newEnv(t)
	e.sup.cfg.Canary = func(context.Context) error { return errors.New("timeout") }
	e.sup.cfg.CanaryInterval = time.Second

	e.engineBeat(4242)
	e.tick()

	if got := e.modes.got(); len(got) != 1 || got[0] != exredis.ModeReadOnly {
		t.Fatalf("mode trips = %v, want [ReadOnly]", got)
	}
	if !strings.Contains(e.exposition(), `watchdog_canary_failures_total 1`) {
		t.Fatalf("canary failure metric missing:\n%s", e.exposition())
	}
}

func TestDiskLowTripsMarketDataOnly(t *testing.T) {
	e := newEnv(t)
	e.sup.cfg.WalDir = "/var/lib/exchange/wal"
	e.sup.cfg.DiskInterval = time.Second
	e.sup.cfg.DiskFreeMin = 0.10
	e.sup.cfg.Statfs = func(string) (float64, error) { return 0.05, nil }

	e.engineBeat(4242)
	e.tick()

	if got := e.modes.got(); len(got) != 1 || got[0] != exredis.ModeMarketDataOnly {
		t.Fatalf("mode trips = %v, want [MarketDataOnly]", got)
	}
}

// ---------------------------------------------------------------------------
// sd_notify
// ---------------------------------------------------------------------------

func listenUnixgram(t *testing.T, path string) *net.UnixConn {
	t.Helper()
	l, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: path, Net: "unixgram"})
	if err != nil {
		t.Fatalf("listen unixgram %s: %v", path, err)
	}
	return l
}

func readDatagrams(t *testing.T, l *net.UnixConn, n int) []string {
	t.Helper()
	var out []string
	buf := make([]byte, 4096)
	_ = l.SetReadDeadline(time.Now().Add(2 * time.Second))
	for i := 0; i < n; i++ {
		m, _, err := l.ReadFromUnix(buf)
		if err != nil {
			t.Fatalf("read datagram %d: %v", i, err)
		}
		out = append(out, string(buf[:m]))
	}
	return out
}

func TestNotifierFilesystemSocket(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, "notify.sock")
	l := listenUnixgram(t, sock)
	defer l.Close()

	n := &Notifier{Socket: sock}
	if !n.Enabled() {
		t.Fatal("notifier not enabled")
	}
	if err := n.Ready("probing"); err != nil {
		t.Fatalf("ready: %v", err)
	}
	if err := n.Watchdog(); err != nil {
		t.Fatalf("watchdog: %v", err)
	}
	if err := n.Stopping(); err != nil {
		t.Fatalf("stopping: %v", err)
	}
	got := readDatagrams(t, l, 3)
	joined := strings.Join(got, "|")
	for _, want := range []string{"READY=1", "STATUS=probing", "WATCHDOG=1", "STOPPING=1"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("datagrams %v missing %q", got, want)
		}
	}
}

func TestNotifierDisabledNoop(t *testing.T) {
	var n Notifier
	if n.Enabled() {
		t.Fatal("zero notifier enabled")
	}
	if err := n.Watchdog(); err != nil {
		t.Fatalf("disabled notify errored: %v", err)
	}
}
