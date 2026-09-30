// Command watchdogd is the exchange-watchdogd Tier-3 platform supervisor
// (Task 9.3.28 item 2, spec §19.13.3). Deployed as
// /opt/exchange/bin/exchange-watchdogd by the exchange-watchdogd.service
// unit (deploy/systemd/) — the Go package dir keeps the spec's
// services/cmd/watchdogd/ naming; the installed binary name is set by the
// build/install step.
//
// Duties (spec §19.13.3 tier 3 probe matrix):
//
//   - IPC heartbeat monitoring of every local matching-engine shard via
//     the /dev/shm SPSC ring headers (producer heartbeat_ns + pid).
//   - Leader-lock revocation: a primary holding engine:leader:{shard}
//     whose heartbeat stalls >3s under active ingress (or whose producer
//     pid is dead) loses its lease via token-checked CAS delete, then is
//     demoted with SIGTERM so the warm follower can promote.
//   - Ingress ring watermark supervision (>80% CAPACITY_EXCEEDED,
//     >95% CRITICAL_BACKPRESSURE telemetry).
//   - Synthetic canary probe (--canary-cmd): over-budget/error trips
//     ModeManager to ReadOnly. No in-band order injector exists; with no
//     command configured the subsystem reports watchdog_canary_configured=0
//     rather than fabricating health.
//   - Aeron media-driver CnC counter sampling (--aeron-dir) and PTP skew
//     monitoring (--ptp) via the existing observability/timesync monitors.
//   - NVMe quota: WAL filesystem free space <10% trips MarketDataOnly.
//
// The daemon itself is systemd-supervised (Type=notify,
// WatchdogSec=500ms): it pets WATCHDOG=1 after every completed probe
// cycle, so its own hang trips the Tier-1 watchdog.
//
// Fail-closed: missing heartbeat / lost lock evidence → revoke + fence;
// probe failures are metered and logged, never silently healthy.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"

	"exchange/internal/observability"
	"exchange/internal/recovery"
	exredis "exchange/internal/redis"
	"exchange/internal/timesync"
	"exchange/internal/utils"
	"exchange/internal/watchdog"
	"exchange/pkg/logging"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "watchdogd: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		aeronDir       = flag.String("aeron-dir", envOr("AERON_DIR", "/dev/shm/aeron-exchange"), "Aeron media driver dir (cnc.dat location)")
		redisSentinel  = flag.String("redis-sentinel", envOr("EXC_SENTINEL_ADDRS", ""), "comma-separated sentinel host:port list; empty uses --redis-addr")
		redisAddr      = flag.String("redis-addr", envOr("REDIS_ADDR", ""), "direct coordination Redis host:port (dev; mutually exclusive with --redis-sentinel)")
		redisMaster    = flag.String("redis-master-name", envOr("EXC_SENTINEL_MASTER", "mymaster"), "sentinel monitor name")
		redisResolve   = flag.String("redis-resolve-mode", envOr("EXC_SENTINEL_RESOLVE_MODE", "as_announced"), "sentinel announced-addr resolution: as_announced|announce_map|host_probe")
		redisPassword  = flag.String("redis-password", envOr("EXC_SENTINEL_PASSWORD", ""), "coordination Redis password")
		noRedis        = flag.Bool("no-redis", false, "explicitly run without Redis (fencing disabled — stalls still detected/metered)")
		metricsAddr    = flag.String("metrics", "127.0.0.1:9110", "Prometheus /metrics listen address")
		shardsCSV      = flag.String("shards", envOr("EXC_WATCHDOG_SHARDS", "0"), "comma-separated shard ids supervised on this host")
		shmBase        = flag.String("shm-base", envOr("EXC_SHM_BASE", "exchange_ipc"), "shm ring name prefix ({base}_{shard}_{in,out})")
		shmDir         = flag.String("shm-dir", "/dev/shm", "shared-memory directory")
		pollInterval   = flag.Duration("poll-interval", 100*time.Millisecond, "probe cycle cadence (must stay well under WatchdogSec=500ms)")
		stallTimeout   = flag.Duration("stall-timeout", 3*time.Second, "IPC heartbeat staleness bound before fencing (spec §19.13.3)")
		canaryInterval = flag.Duration("canary-interval", time.Second, "synthetic canary cadence")
		canaryTimeout  = flag.Duration("canary-timeout", 250*time.Millisecond, "synthetic canary budget; exceeded trips ReadOnly")
		canaryCmd      = flag.String("canary-cmd", envOr("EXC_CANARY_CMD", ""), "external synthetic-order probe command (empty = canary disabled, reported as unconfigured)")
		walDir         = flag.String("wal-dir", "/var/lib/exchange/wal", "WAL filesystem path for the NVMe quota probe (empty disables)")
		diskFreeMin    = flag.Float64("disk-free-min", 0.10, "WAL filesystem free-ratio trip line")
		noDemote       = flag.Bool("no-demote", false, "never SIGTERM a fenced producer (revocation still runs)")
		ptp            = flag.Bool("ptp", true, "sample PTP clock skew (bare-metal hosts expect ptp4l; --ptp=false on K8s DaemonSet nodes)")
		ptpExpected    = flag.Bool("ptp-expected", true, "raise P1 when no PTP source is available")
		logLevel       = flag.String("log-level", envOr("EXC_LOG_LEVEL", "info"), "debug|info|warn|error")
		logFormat      = flag.String("log-format", envOr("EXC_LOG_FORMAT", "json"), "json|text")
	)
	flag.Parse()

	level, err := logging.ParseLevel(*logLevel)
	if err != nil {
		return err
	}
	log, err := logging.New(level, *logFormat)
	if err != nil {
		return err
	}

	shards, err := parseShards(*shardsCSV)
	if err != nil {
		return err
	}

	// --- coordination Redis (lease fencing + degradation-mode trips) ------
	var leases watchdog.LeaseStore
	var modes watchdog.ModeSetter
	var rdb *exredis.Client
	switch {
	case *noRedis:
		log.Warn("--no-redis: fencing disabled; stalls detected+metered only (lease TTL self-expiry still bounds)")
	case *redisSentinel != "":
		fc, err := exredis.NewFailoverClient(exredis.FailoverConfig{
			MasterName:    *redisMaster,
			SentinelAddrs: splitCSV(*redisSentinel),
			Password:      *redisPassword,
			ResolveMode:   exredis.SentinelResolveMode(*redisResolve),
		})
		if err != nil {
			return err
		}
		rdb = fc.Client
		log.Info("redis sentinel failover client configured",
			"master", *redisMaster, "sentinels", *redisSentinel, "resolve", *redisResolve)
	case *redisAddr != "":
		rdb = exredis.New(*redisAddr, *redisPassword, 0)
		log.Info("redis direct client configured", "addr", *redisAddr)
	default:
		return fmt.Errorf("no Redis coordination endpoint: pass --redis-sentinel or --redis-addr (or --no-redis to explicitly disable fencing)")
	}
	if rdb != nil {
		ctx0, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := rdb.Ping(ctx0); err != nil {
			cancel()
			return fmt.Errorf("redis coordination ping: %w", err)
		}
		cancel()
		l, err := recovery.NewOrchRedisLeases(rdb)
		if err != nil {
			return err
		}
		leases = l
		modes = rdb
		defer rdb.Close()
	}

	// --- supervisor --------------------------------------------------------
	reg := observability.New()
	demoteSig := syscall.SIGTERM
	if *noDemote {
		demoteSig = -1
	}
	cfg := watchdog.Config{
		Shards:         shards,
		ShmDir:         *shmDir,
		ShmBase:        *shmBase,
		PollInterval:   *pollInterval,
		StallTimeout:   *stallTimeout,
		CanaryInterval: *canaryInterval,
		CanaryTimeout:  *canaryTimeout,
		Canary:         canaryProbe(*canaryCmd),
		WalDir:         *walDir,
		DiskFreeMin:    *diskFreeMin,
		DemoteSig:      demoteSig,
		Log:            log,
	}
	sup := watchdog.New(cfg, reg, leases, modes, watchdog.NewNotifier())

	ctx, stop := utils.SignalContext()
	defer stop()

	// --- side monitors sharing the registry --------------------------------
	if *aeronDir != "" {
		cnc := observability.NewCnCMonitor(reg, *aeronDir, log)
		cnc.Interval = time.Second // supervisor cadence, not the 15s scrape default
		go cnc.Run(ctx)
	}
	if *ptp {
		pm := timesync.NewPTPMonitor(reg, nil, *ptpExpected)
		pm.Interval = time.Second
		pm.OnAlert = func(a timesync.PTPAlert) {
			log.Error("[P1] PTP violation", "kind", a.Kind, "detail", a.Detail)
		}
		go pm.Run(ctx)
	}

	go func() {
		if err := observability.ServeMetrics(ctx, *metricsAddr, reg, "exchange-watchdogd"); err != nil {
			log.Error("metrics listener failed", "addr", *metricsAddr, "err", err)
		}
	}()

	log.Info("exchange-watchdogd started",
		"shards", shards, "shm_dir", *shmDir, "shm_base", *shmBase,
		"stall_timeout", stallTimeout.String(), "poll", pollInterval.String(),
		"aeron_dir", *aeronDir, "metrics", *metricsAddr,
		"canary_cmd", *canaryCmd != "", "fencing", leases != nil)
	return sup.Run(ctx)
}

// canaryProbe wraps --canary-cmd in a subprocess exec with the budgeted
// context; nil when unconfigured (canary coverage is reported as absent,
// never faked). The command is the deployment's synthetic-order injector —
// e.g. a gateway REST submit/cancel pair against a reserved canary account.
func canaryProbe(cmdline string) watchdog.CanaryProbe {
	if strings.TrimSpace(cmdline) == "" {
		return nil
	}
	return func(ctx context.Context) error {
		cmd := exec.CommandContext(ctx, "sh", "-c", cmdline)
		out, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("canary probe %q: %w (%s)", cmdline, err, strings.TrimSpace(string(out)))
		}
		return nil
	}
}

// parseShards reads "0,1,2" → []int{0,1,2}; any malformed entry fails the
// whole flag (a silently dropped shard would be unsupervised — fail closed).
func parseShards(s string) ([]int, error) {
	var out []int
	for _, p := range strings.Split(s, ",") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		v, err := strconv.Atoi(p)
		if err != nil || v < 0 {
			return nil, fmt.Errorf("--shards: bad shard id %q", p)
		}
		out = append(out, v)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("--shards: empty shard list")
	}
	return out, nil
}

func splitCSV(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
