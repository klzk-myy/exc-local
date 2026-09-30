// Package watchdog implements the exchange-watchdogd platform supervisor
// (Task 9.3.28 item 2, spec §19.13.3 tier 3): the out-of-process node
// watchdog that runs on bare-metal matching hosts (and as a DaemonSet on
// service nodes).
//
// Duties implemented here:
//
//   - IPC heartbeat monitoring — samples the engine's shared-memory ring
//     headers ({base}_{shard}_{in,out} under /dev/shm). The producer
//     heartbeat_ns field is stamped by the matching thread on every write
//     batch, so its age is loop-level liveness — strictly stronger than
//     process aliveness or the Aeron conductor heartbeat.
//   - Leader fencing — when a shard primary holds engine:leader:{shard}
//     but its IPC heartbeat is stale beyond StallTimeout under active
//     ingress (or its producer pid is dead), the lease is revoked through
//     the token-checked compare-and-delete in internal/redis, then the
//     hung producer is demoted (SIGTERM) so its warm follower can promote.
//   - Ingress watermark supervision — ring utilization >80%/>95% trips
//     CAPACITY_EXCEEDED/CRITICAL_BACKPRESSURE telemetry per spec.
//   - Synthetic canary — an optional external probe command on a fixed
//     cadence; an over-budget probe trips ModeManager to ReadOnly.
//   - NVMe quota — WAL filesystem free-space ratio below the bound trips
//     MarketDataOnly degradation.
//
// Fail-closed (spec §2.7): a missing ring, an absent lease read, or a
// Redis outage is an error surfaced in metrics/logs — never silently
// treated as healthy. Fencing requires positive evidence (a held lease
// plus a proven stall signal); absence of evidence fences nothing, and
// the 2s lease TTL (§18.6.2) self-expires a dead leader anyway.
package watchdog

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"exchange/internal/ipc"
	"exchange/internal/observability"
	exredis "exchange/internal/redis"
)

// ---------------------------------------------------------------------------
// Pluggable seams (production wiring lives in cmd/watchdogd; tests fake them)
// ---------------------------------------------------------------------------

// LeaseStore is the Redis epoch-lease facade — the subset of
// recovery.OrchLeaseBackend this daemon needs (engine:leader:{shard},
// value "{token}:{epoch}", spec §4.2/§18.6.2). *recovery.OrchRedisLeases
// satisfies it.
type LeaseStore interface {
	// LeaderValue reads the current lease value; ok=false when absent.
	LeaderValue(ctx context.Context, shardID int) (value string, ok bool, err error)
	// RevokeLeader deletes the lease only if it still equals expectValue
	// (token-checked delete — never kill a re-acquired lease).
	RevokeLeader(ctx context.Context, shardID int, expectValue string) (bool, error)
}

// ModeSetter writes degradation-mode transitions; *exredis.Client
// satisfies it via SetDegradationMode (system:degradation:* keys).
type ModeSetter interface {
	SetDegradationMode(ctx context.Context, mode exredis.DegradationMode, reason string) error
}

// RingReader snapshots a ring header read-only. Default ipc.ReadRingHeader.
type RingReader func(path string) (ipc.RingHeader, error)

// PidAliveFunc probes a stamped producer pid. Default ipc.PidAlive.
type PidAliveFunc func(pid uint64) bool

// Demoter demotes a fenced producer — production sends SIGTERM so the
// engine drains its snapshot/WAL tail before systemd restarts or the
// follower promotes (runbook §4). Must not be called for pid 0.
type Demoter func(pid uint64) error

// StatfsFunc returns the free-space ratio (0..1] of the filesystem holding
// path. Default statfsFreeRatio.
type StatfsFunc func(path string) (float64, error)

// CanaryProbe is one synthetic-order probe attempt: nil error within the
// budget = healthy. Wired in cmd/watchdogd to an external probe command
// (there is no in-band order injector — a canary that cannot run is
// reported via watchdog_canary_configured=0, never faked).
type CanaryProbe func(ctx context.Context) error

// ---------------------------------------------------------------------------
// Config
// ---------------------------------------------------------------------------

// Config drives Supervisor. Durations <= 0 fall back to the spec defaults.
type Config struct {
	// Shards is the set of matching-engine shards co-located on this host.
	Shards []int
	// ShmDir is the shared-memory base (default /dev/shm).
	ShmDir string
	// ShmBase is the ring name prefix (default ipc.DefaultShmBase).
	ShmBase string

	// PollInterval is the probe cadence (default 100ms; the unit's
	// WatchdogSec=500ms needs ≥2 pets per window).
	PollInterval time.Duration
	// StallTimeout is the IPC-heartbeat staleness bound before fencing is
	// considered (default 3s — spec §19.13.3 tier 3).
	StallTimeout time.Duration
	// IngressWarnRatio / IngressCritRatio are the ring utilization trip
	// lines (defaults 0.80 / 0.95 per spec).
	IngressWarnRatio float64
	IngressCritRatio float64

	// CanaryInterval / CanaryTimeout bound the synthetic probe (spec:
	// 1.0s cadence, 250ms budget). Canary is the probe func; nil disables
	// the subsystem and publishes watchdog_canary_configured=0.
	CanaryInterval time.Duration
	CanaryTimeout  time.Duration
	Canary         CanaryProbe

	// WalDir is the WAL filesystem probed for free space (default
	// /var/lib/exchange/wal); "" disables the NVMe quota probe.
	WalDir string
	// DiskFreeMin is the free-ratio trip line (default 0.10 per spec).
	DiskFreeMin float64
	// DiskInterval between statfs samples (default 5s).
	DiskInterval time.Duration

	// DemoteSig is sent to the fenced producer pid (default SIGTERM; 0
	// disables demotion — revocation still runs).
	DemoteSig syscall.Signal

	// Now overrides the wall clock (tests).
	Now func() time.Time
	// Log destination; nil → slog.Default().
	Log *slog.Logger

	// seams — nil selects the production defaults.
	ReadRing RingReader
	PidAlive PidAliveFunc
	Demote   Demoter
	Statfs   StatfsFunc
}

func (c *Config) defaults() {
	if len(c.Shards) == 0 {
		c.Shards = []int{0}
	}
	if c.ShmDir == "" {
		c.ShmDir = "/dev/shm"
	}
	if c.ShmBase == "" {
		c.ShmBase = ipc.DefaultShmBase
	}
	if c.PollInterval <= 0 {
		c.PollInterval = 100 * time.Millisecond
	}
	if c.StallTimeout <= 0 {
		c.StallTimeout = 3 * time.Second
	}
	if c.IngressWarnRatio <= 0 {
		c.IngressWarnRatio = 0.80
	}
	if c.IngressCritRatio <= 0 {
		c.IngressCritRatio = 0.95
	}
	if c.CanaryInterval <= 0 {
		c.CanaryInterval = time.Second
	}
	if c.CanaryTimeout <= 0 {
		c.CanaryTimeout = 250 * time.Millisecond
	}
	if c.WalDir == "" {
		c.WalDir = "/var/lib/exchange/wal"
	}
	if c.DiskFreeMin <= 0 {
		c.DiskFreeMin = 0.10
	}
	if c.DiskInterval <= 0 {
		c.DiskInterval = 5 * time.Second
	}
	if c.DemoteSig == 0 {
		// Zero value must not mean "disabled": callers opt out with -1.
		// (signal 0 is the existence-probe signal, never a valid demote.)
		c.DemoteSig = syscall.SIGTERM
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	if c.Log == nil {
		c.Log = slog.Default()
	}
	if c.ReadRing == nil {
		c.ReadRing = ipc.ReadRingHeader
	}
	if c.PidAlive == nil {
		c.PidAlive = ipc.PidAlive
	}
	if c.Demote == nil {
		c.Demote = func(pid uint64) error {
			return syscall.Kill(int(pid), c.DemoteSig)
		}
	}
	if c.Statfs == nil {
		c.Statfs = statfsFreeRatio
	}
}

// ---------------------------------------------------------------------------
// Per-shard monitor state
// ---------------------------------------------------------------------------

type shardMon struct {
	shard int

	// ingress progress tracking
	lastInHead uint64
	haveIn     bool

	// egress heartbeat tracking
	hbNs        uint64    // last observed heartbeat value (realtime ns)
	hbChangeAt  time.Time // local time hbNs last advanced
	everBeat    bool      // a non-zero heartbeat has been observed
	outErrSince time.Time // first error of the current out-ring outage
	haveOutErr  bool

	// fencing
	fenced      bool      // we revoked while this stall episode runs
	fencedLease string    // lease value already revoked (log dedup)
	hbAtFenceNs uint64    // heartbeat value at fence time; progress re-arms
	graceUntil  time.Time // post-revoke window for the follower to promote
	lastPid     uint64    // last observed producer pid (demote target)
	fenceLogAt  time.Time
	stallLogged bool

	// watermark
	warned, critted bool
}

// ---------------------------------------------------------------------------
// Supervisor
// ---------------------------------------------------------------------------

// Supervisor runs the probe matrix. Construct with New; Run it.
type Supervisor struct {
	cfg    Config
	leases LeaseStore
	modes  ModeSetter
	notify *Notifier

	mons map[int]*shardMon

	// metrics
	reg          *observability.Registry
	up           *observability.GaugeVec
	hbTs         *observability.GaugeVec
	hbAge        *observability.GaugeVec
	inUtil       *observability.GaugeVec
	inDrops      *observability.GaugeVec
	revocations  *observability.CounterVec
	revokeErrs   *observability.CounterVec
	probeErrs    *observability.CounterVec
	trips        *observability.CounterVec
	canaryCfg    *observability.GaugeVec
	canaryLat    *observability.HistogramVec
	canaryFail   *observability.CounterVec
	diskFree     *observability.GaugeVec
	pollsTotal   *observability.CounterVec
	leasePresent *observability.GaugeVec

	mu            sync.Mutex
	lastCanaryAt  time.Time
	canaryTripped bool
	lastDiskAt    time.Time
	diskTripped   bool
	status        string
}

// New builds the supervisor. leases may be nil — fencing is then disabled
// but every stall is still detected, logged and metered (fail-open would
// mean pretending health; fail-closed here means the daemon stays loud).
func New(cfg Config, reg *observability.Registry, leases LeaseStore, modes ModeSetter, notify *Notifier) *Supervisor {
	cfg.defaults()
	if reg == nil {
		reg = observability.New()
	}
	s := &Supervisor{
		cfg:    cfg,
		leases: leases,
		modes:  modes,
		notify: notify,
		mons:   map[int]*shardMon{},
		reg:    reg,
		up: reg.Gauge("daemon_up",
			"1 when the supervised daemon's IPC endpoint is readable, its producer is alive and its heartbeat is fresh."),
		hbTs: reg.Gauge("watchdog_heartbeat_timestamp_seconds",
			"Unix-seconds timestamp of the last observed producer heartbeat."),
		hbAge: reg.Gauge("watchdog_heartbeat_age_seconds",
			"Age of the most recent IPC producer heartbeat."),
		inUtil: reg.Gauge("watchdog_ingress_ring_utilization",
			"Ingress ring occupancy/capacity per shard (shedding at 0.8, critical at 0.95)."),
		inDrops: reg.Gauge("watchdog_ingress_drops_total",
			"Ingress ring producer drop counter as last sampled."),
		revocations: reg.Counter("watchdog_leader_revocations_total",
			"engine:leader:{shard} leases revoked by this supervisor."),
		revokeErrs: reg.Counter("watchdog_revoke_errors_total",
			"Leader revocation attempts that failed (Redis error or CAS miss on a changed lease)."),
		probeErrs: reg.Counter("watchdog_probe_errors_total",
			"Probe read failures by probe name and shard."),
		trips: reg.Counter("watchdog_trips_total",
			"Trip events raised by kind (ipc_stall, pid_dead, backpressure_warn, backpressure_crit, canary, disk_quota, revoke_error)."),
		canaryCfg: reg.Gauge("watchdog_canary_configured",
			"1 when a synthetic canary probe is configured; 0 means canary coverage is absent (never faked)."),
		canaryLat: reg.Histogram("watchdog_canary_latency_seconds",
			"Synthetic canary probe latency; trip budget is --canary-timeout.",
			[]float64{0.01, 0.05, 0.1, 0.25, 0.5, 1, 2.5}),
		canaryFail: reg.Counter("watchdog_canary_failures_total",
			"Canary probes that errored or exceeded the timeout budget."),
		diskFree: reg.Gauge("watchdog_disk_free_ratio",
			"Free-space ratio of the WAL filesystem (trip below --disk-free-min)."),
		pollsTotal: reg.Counter("watchdog_polls_total",
			"Supervisor probe cycles completed."),
		leasePresent: reg.Gauge("watchdog_leader_lease_present",
			"1 when a leader lease is currently held for the shard."),
	}
	for _, sh := range cfg.Shards {
		s.mons[sh] = &shardMon{shard: sh}
	}
	if cfg.Canary == nil {
		s.canaryCfg.With().Set(0)
	} else {
		s.canaryCfg.With().Set(1)
	}
	s.status = "init"
	return s
}

// Registry exposes the metric registry (for the /metrics listener and
// co-hosted monitors such as the Aeron CnC and PTP samplers).
func (s *Supervisor) Registry() *observability.Registry { return s.reg }

func (s *Supervisor) now() time.Time { return s.cfg.Now() }

// StatusLine is the current sd_notify STATUS text.
func (s *Supervisor) StatusLine() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.status
}

func (s *Supervisor) setStatus(st string) {
	s.mu.Lock()
	s.status = st
	s.mu.Unlock()
}

// ---------------------------------------------------------------------------
// Probe cycle
// ---------------------------------------------------------------------------

// Tick runs one probe cycle over every shard plus the canary/disk
// cadenced probes. Exported for tests; Run loops on it.
func (s *Supervisor) Tick(ctx context.Context) {
	now := s.now()
	for _, sh := range s.cfg.Shards {
		s.probeShard(ctx, s.mons[sh], now)
	}
	s.maybeCanary(ctx, now)
	s.maybeDisk(ctx, now)
	s.pollsTotal.With().Inc()
}

// Run loops Tick on PollInterval and pets the systemd watchdog after each
// completed cycle. A blocked probe cycle means no pet — systemd trips at
// WatchdogSec (fail-closed on our own hang). Returns on ctx cancel.
func (s *Supervisor) Run(ctx context.Context) error {
	log := s.cfg.Log
	if s.notify != nil && s.notify.Enabled() {
		if err := s.notify.Ready("exchange-watchdogd probing"); err != nil {
			log.Warn("sd_notify READY failed", "err", err)
		}
	}
	t := time.NewTicker(s.cfg.PollInterval)
	defer t.Stop()
	for {
		s.Tick(ctx)
		if s.notify != nil && s.notify.Enabled() {
			if err := s.notify.Watchdog(); err != nil {
				log.Error("sd_notify watchdog pet failed — systemd will trip this unit", "err", err)
			}
		}
		select {
		case <-ctx.Done():
			if s.notify != nil && s.notify.Enabled() {
				_ = s.notify.Stopping()
			}
			return nil
		case <-t.C:
		}
	}
}

// ---------------------------------------------------------------------------
// Shard probe: ring headers -> stall verdict -> fence
// ---------------------------------------------------------------------------

func (s *Supervisor) probeShard(ctx context.Context, m *shardMon, now time.Time) {
	log := s.cfg.Log.With("shard", m.shard)
	inPath := filepath.Join(s.cfg.ShmDir, ipc.InName(s.cfg.ShmBase, uint16(m.shard)))
	outPath := filepath.Join(s.cfg.ShmDir, ipc.OutName(s.cfg.ShmBase, uint16(m.shard)))

	// --- ingress ring (gateway -> engine): watermark + activity ---------
	activeIngress := false
	inHdr, inErr := s.cfg.ReadRing(inPath)
	if inErr != nil {
		s.probeErrs.With("probe", "ingress_ring", "shard", itoa(m.shard)).Inc()
	} else {
		s.inUtil.With("shard", itoa(m.shard)).Set(inHdr.Utilization())
		s.inDrops.With("shard", itoa(m.shard)).Set(float64(inHdr.Drops))
		if m.haveIn && inHdr.Head > m.lastInHead {
			activeIngress = true
		}
		if inHdr.Occupancy() > 0 {
			activeIngress = true
		}
		m.lastInHead, m.haveIn = inHdr.Head, true
		s.checkWatermark(m, inHdr, log)
	}

	// --- egress ring (engine -> gateway): heartbeat + producer liveness --
	var hbStale, pidDead bool
	outHdr, outErr := s.cfg.ReadRing(outPath)
	switch {
	case outErr == nil:
		if m.haveOutErr {
			log.Info("egress ring readable again", "path", outPath)
		}
		m.haveOutErr = false
		if outHdr.ProducerPid != m.lastPid && m.lastPid != 0 {
			// Producer identity changed (engine restart / ring re-create):
			// reset heartbeat tracking for the new producer — its startup
			// window must not inherit the predecessor's stale heartbeat.
			log.Info("engine producer pid changed; resetting heartbeat tracking",
				"old_pid", m.lastPid, "new_pid", outHdr.ProducerPid)
			m.hbNs = outHdr.HeartbeatNs
			m.hbChangeAt = now
			m.everBeat = outHdr.HeartbeatNs != 0
			m.fenced = false
			m.fencedLease = ""
		}
		m.lastPid = outHdr.ProducerPid
		if outHdr.HeartbeatNs != 0 {
			if outHdr.HeartbeatNs != m.hbNs {
				m.hbNs = outHdr.HeartbeatNs
				m.hbChangeAt = now
			}
			m.everBeat = true
			s.hbTs.With("daemon", "matching-engine", "shard", itoa(m.shard)).
				Set(float64(outHdr.HeartbeatNs) / 1e9)
		}
		var hbAge time.Duration
		if m.everBeat {
			hbAge = now.Sub(time.Unix(0, int64(m.hbNs)))
			if hbAge < 0 {
				hbAge = 0 // realtime clock stepped back — clamp, never fabricate
			}
			s.hbAge.With("shard", itoa(m.shard)).Set(hbAge.Seconds())
		} else {
			s.hbAge.With("shard", itoa(m.shard)).Set(-1) // never beaten: unknown
		}
		hbStale = m.everBeat && hbAge > s.cfg.StallTimeout

		pidAlive := s.cfg.PidAlive(outHdr.ProducerPid)
		pidDead = outHdr.ProducerPid != 0 && !pidAlive

		up := 0.0
		if pidAlive && !hbStale {
			up = 1
		}
		s.up.With("name", "matching-engine", "shard", itoa(m.shard)).Set(up)

		if pidDead {
			s.trips.With("kind", "pid_dead", "shard", itoa(m.shard)).Inc()
			log.Error("[P0] engine producer pid dead while IPC image persists",
				"pid", outHdr.ProducerPid)
		}
	case errors.Is(outErr, ipc.ErrRingUninitialized):
		// Image exists but producer never configured it — not yet alive,
		// not yet provably stalled. No fence without heartbeat evidence.
		s.probeErrs.With("probe", "egress_ring", "shard", itoa(m.shard)).Inc()
		s.up.With("name", "matching-engine", "shard", itoa(m.shard)).Set(0)
	default:
		s.probeErrs.With("probe", "egress_ring", "shard", itoa(m.shard)).Inc()
		s.up.With("name", "matching-engine", "shard", itoa(m.shard)).Set(0)
		if m.everBeat {
			if !m.haveOutErr {
				m.outErrSince = now
				m.haveOutErr = true
			}
			// Ring we once observed is now unreadable for longer than the
			// stall bound: the liveness evidence vanished — fence-eligible.
			if now.Sub(m.outErrSince) > s.cfg.StallTimeout {
				hbStale = true
			}
		}
	}

	// --- fence decision ---------------------------------------------------
	stalled := pidDead || (hbStale && activeIngress)
	if now.Before(m.graceUntil) {
		// We just fenced this shard — the follower is replaying to the tail
		// before it can acquire the lease and beat. Suppress verdicts; the
		// stall itself remains metered through daemon_up/hb_age.
		return
	}
	if !stalled {
		if m.stallLogged {
			log.Info("engine stall cleared",
				"heartbeat_ns", m.hbNs, "ingress_head", m.lastInHead)
			m.stallLogged = false
		}
		// Recovery: heartbeat advanced past the point where we fenced —
		// this may be a new (restarted) leader; re-arm fencing.
		if m.fenced && outErr == nil && m.hbNs > m.hbAtFenceNs {
			m.fenced = false
		}
		return
	}

	if !m.stallLogged {
		m.stallLogged = true
		s.trips.With("kind", "ipc_stall", "shard", itoa(m.shard)).Inc()
		hbAgeMs := int64(-1)
		if m.everBeat {
			hbAgeMs = now.Sub(time.Unix(0, int64(m.hbNs))).Milliseconds()
		}
		log.Error("[P0] engine IPC stall detected under active ingress",
			"heartbeat_ns", m.hbNs, "heartbeat_age_ms", hbAgeMs,
			"ingress_head", m.lastInHead, "pid_dead", pidDead,
			"stall_timeout", s.cfg.StallTimeout.String())
	}
	s.fence(ctx, m, now, log)
}

// fence revokes the leader lease and demotes the hung producer. Every
// revocation goes through the token-checked CAS delete: a lease re-acquired
// between our read and our delete is never touched.
func (s *Supervisor) fence(ctx context.Context, m *shardMon, now time.Time, log *slog.Logger) {
	if s.leases == nil {
		if m.fenceLogAt.IsZero() || now.Sub(m.fenceLogAt) > 10*time.Second {
			m.fenceLogAt = now
			log.Error("[P0] stall confirmed but no lease store configured — cannot fence; relying on 2s lease TTL self-expiry")
		}
		return
	}
	val, ok, err := s.leases.LeaderValue(ctx, m.shard)
	if err != nil {
		s.probeErrs.With("probe", "lease_read", "shard", itoa(m.shard)).Inc()
		log.Error("leader lease read failed during stall — fencing deferred, lease TTL still bounds", "err", err)
		return
	}
	s.leasePresent.With("shard", itoa(m.shard)).Set(boolf(ok))
	if !ok {
		// No lease held: nothing to revoke. The stall is still metered —
		// a hung non-leader isn't a split-brain risk.
		return
	}
	if m.fenced && val == m.fencedLease {
		return // already revoked this exact lease; CAS value cannot recur
	}
	revoked, err := s.leases.RevokeLeader(ctx, m.shard, val)
	if err != nil {
		s.revokeErrs.With("shard", itoa(m.shard)).Inc()
		s.trips.With("kind", "revoke_error", "shard", itoa(m.shard)).Inc()
		log.Error("leader lease revocation failed — retrying next cycle",
			"lease", val, "err", err)
		return
	}
	if !revoked {
		// CAS miss: the stored value changed under us (expired + reacquired
		// or manually revoked). Correct outcome either way — do not log as
		// an error, but record it so flapping is visible.
		s.revokeErrs.With("shard", itoa(m.shard)).Inc()
		log.Warn("leader lease changed under revocation CAS — holder already gone or superseded",
			"expected", val)
		return
	}
	m.fenced = true
	m.fencedLease = val
	m.hbAtFenceNs = m.hbNs
	m.graceUntil = now.Add(s.cfg.StallTimeout)
	s.revocations.With("shard", itoa(m.shard)).Inc()
	log.Error("[P0] revoked leader lease of stalled primary",
		"lease", val, "shard", m.shard)
	s.setStatus(fmt.Sprintf("fenced shard %d lease %s", m.shard, val))

	// Demote the hung producer so the warm follower can promote cleanly.
	if s.cfg.DemoteSig > 0 && m.lastPid > 0 {
		if derr := s.cfg.Demote(m.lastPid); derr != nil {
			log.Error("demote signal to fenced producer failed", "pid", m.lastPid, "err", derr)
		} else {
			log.Warn("demote signal sent to fenced producer", "pid", m.lastPid, "sig", s.cfg.DemoteSig)
		}
	}
}

// checkWatermark fires the ingress utilization trip lines (spec: >80%
// shedding signal, >95% critical backpressure).
func (s *Supervisor) checkWatermark(m *shardMon, h ipc.RingHeader, log *slog.Logger) {
	u := h.Utilization()
	sh := itoa(m.shard)
	switch {
	case u > s.cfg.IngressCritRatio:
		if !m.critted {
			m.critted = true
			log.Error("[P0] CRITICAL_BACKPRESSURE: ingress ring utilization",
				"util", u, "capacity", h.Capacity)
		}
		s.trips.With("kind", "backpressure_crit", "shard", sh).Inc()
	case u > s.cfg.IngressWarnRatio:
		if !m.warned {
			m.warned = true
			log.Warn("CAPACITY_EXCEEDED: ingress ring utilization above warn line",
				"util", u, "capacity", h.Capacity)
		}
		s.trips.With("kind", "backpressure_warn", "shard", sh).Inc()
	default:
		m.warned, m.critted = false, false
	}
}

// ---------------------------------------------------------------------------
// Canary — synthetic probe (spec: 1.0s cadence, 250ms budget, ReadOnly trip)
// ---------------------------------------------------------------------------

func (s *Supervisor) maybeCanary(ctx context.Context, now time.Time) {
	c := s.cfg.Canary
	if c == nil {
		return
	}
	if !s.lastCanaryAt.IsZero() && now.Sub(s.lastCanaryAt) < s.cfg.CanaryInterval {
		return
	}
	s.lastCanaryAt = now

	cctx, cancel := context.WithTimeout(ctx, s.cfg.CanaryTimeout)
	start := time.Now()
	err := c(cctx)
	cancel()
	lat := time.Since(start).Seconds()
	s.canaryLat.With().Observe(lat)

	if err == nil {
		s.canaryTripped = false
		return
	}
	s.canaryFail.With().Inc()
	s.trips.With("kind", "canary", "shard", "-").Inc()
	s.cfg.Log.Error("[P0] synthetic canary probe failed — tripping ModeManager to ReadOnly",
		"latency_s", lat, "budget", s.cfg.CanaryTimeout.String(), "err", err)
	if s.modes != nil && !s.canaryTripped {
		s.canaryTripped = true
		if merr := s.modes.SetDegradationMode(ctx, exredis.ModeReadOnly,
			"watchdogd: synthetic canary probe failed"); merr != nil {
			s.cfg.Log.Error("ReadOnly trip write failed", "err", merr)
		}
	}
}

// ---------------------------------------------------------------------------
// NVMe quota — WAL filesystem free space (spec: <10% trips MarketDataOnly)
// ---------------------------------------------------------------------------

func (s *Supervisor) maybeDisk(ctx context.Context, now time.Time) {
	if s.cfg.WalDir == "" {
		return
	}
	if !s.lastDiskAt.IsZero() && now.Sub(s.lastDiskAt) < s.cfg.DiskInterval {
		return
	}
	s.lastDiskAt = now

	free, err := s.cfg.Statfs(s.cfg.WalDir)
	if err != nil {
		s.probeErrs.With("probe", "disk", "shard", "-").Inc()
		s.cfg.Log.Warn("WAL filesystem stat failed", "dir", s.cfg.WalDir, "err", err)
		return
	}
	s.diskFree.With("mount", s.cfg.WalDir).Set(free)
	if free < s.cfg.DiskFreeMin {
		s.trips.With("kind", "disk_quota", "shard", "-").Inc()
		if !s.diskTripped {
			s.diskTripped = true
			s.cfg.Log.Error("[P1] WAL filesystem free space below bound — tripping MarketDataOnly",
				"dir", s.cfg.WalDir, "free_ratio", free, "min", s.cfg.DiskFreeMin)
			if s.modes != nil {
				if merr := s.modes.SetDegradationMode(ctx, exredis.ModeMarketDataOnly,
					"watchdogd: WAL filesystem free space below bound"); merr != nil {
					s.cfg.Log.Error("MarketDataOnly trip write failed", "err", merr)
				}
			}
		}
	} else if s.diskTripped && free > s.cfg.DiskFreeMin+0.05 {
		// Recovered with hysteresis; ModeManager owns the mode clear.
		s.diskTripped = false
		s.cfg.Log.Info("WAL filesystem free space recovered", "dir", s.cfg.WalDir, "free_ratio", free)
	}
}

// statfsFreeRatio is the production StatfsFunc.
func statfsFreeRatio(path string) (float64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, err
	}
	if st.Blocks == 0 {
		return 0, fmt.Errorf("statfs %s: zero blocks", path)
	}
	return float64(st.Bavail) / float64(st.Blocks), nil
}

func itoa(i int) string { return fmt.Sprintf("%d", i) }
func boolf(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
