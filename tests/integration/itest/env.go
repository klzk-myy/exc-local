// Package itest is the Phase-08 Task 8.3.1 integration-test framework:
// environment gates, process lifecycle, subprocess delegation and the
// §24 criterion contract registry plumbing.
//
// Env-gate conventions follow services' own integration tests
// (services/internal/testenv):
//
//	EXC_PG_TEST=1            enable PostgreSQL-backed legs
//	EXC_PG_DSN / EXC_TEST_DSN  scratch DSN (default: migverify on the /tmp
//	                         socket, port 55433 — the designated scratch DB)
//	EXC_REDIS_TEST=1         enable Redis-backed legs
//	EXC_REDIS_TEST_ADDR      default 127.0.0.1:16379 (compose dev Redis;
//	                         host :6379 is auth-protected)
//	EXC_REDIS_TEST_DB        default 9 — isolated logical DB so the suite
//	                         never collides with other consumers of the
//	                         shared dev Redis
//	EXC_NATS_URLS            default nats://127.0.0.1:4222
//	EXC_CORE_BIN             matching_engine path (default
//	                         <root>/core/build/matching_engine)
//	EXC_CORE_BUILD           ctest tree (default <root>/core/build)
//	EXC_STACK_SHARDS         engine shards to boot for the live stack
//	                         (default "0 1 2 3 4 5 6 7" — the full
//	                         sharding.yaml set; the readiness probe
//	                         requires every declared shard)
//	EXC_IT_REPORT            coverage report path (default
//	                         <tests/integration>/reports/coverage.json)
//
// Docker Compose bring-up (the task's reference environment) is probed
// via the docker socket; when unavailable the suite composes the same
// topology bare-metal: real matching_engine binaries + the real gateway
// binary + scratch PG/Redis/NATS. Unsatisfiable legs report BLOCKED —
// never a fabricated PASS (Phase-08 rule, spec §2.7).
package itest

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// Repo root + dependency resolution
// ---------------------------------------------------------------------------

// RepoRoot walks up from the working directory to the exchange repo root
// (README.md + docs/ + services/go.mod — the same markers the spec
// harness uses; all tracked files so fresh clones resolve — AGENTS.md
// is gitignored and cannot be a marker). EXC_REPO_ROOT overrides.
func RepoRoot() string {
	if r := os.Getenv("EXC_REPO_ROOT"); r != "" {
		return r
	}
	wd, err := os.Getwd()
	if err != nil {
		return "."
	}
	for d := wd; ; d = filepath.Dir(d) {
		mark := func(rel string) bool {
			_, err := os.Stat(filepath.Join(d, rel))
			return err == nil
		}
		if mark("README.md") && mark("docs") && mark(filepath.Join("services", "go.mod")) {
			return d
		}
		if filepath.Dir(d) == d {
			return wd
		}
	}
}

// Env is the resolved dependency set for one suite invocation.
type Env struct {
	Root        string
	PostgresDSN string
	RedisAddr   string
	RedisDB     int
	NatsURLs    []string
	CoreBin     string // matching_engine binary
	CoreBuild   string // ctest tree
	ServicesDir string
	FrontendDir string // vitest legs (BindVitest)
	DocsDir     string
	TracePath   string // tests/spec/traceability.json (read-only)
	ReportPath  string
	Shards      []int
	// IPCBase namespaces the suite's /dev/shm ring objects
	// ({base}_{shard}_{in,out,snap,snap_ack} + the credit matrix). The
	// per-pid default keeps concurrent/leftover stacks — including a
	// live dev gateway on "exchange_ipc" — from sharing ring state: a
	// stale segment on a foreign base can never backpressure or
	// cross-deliver into this stack.
	IPCBase string
}

// DefaultEnv resolves the environment; nothing is probed yet (probes are
// per-test gates).
func DefaultEnv() *Env {
	root := RepoRoot()
	e := &Env{
		Root: root,
		PostgresDSN: envFirst("EXC_PG_DSN", "EXC_TEST_DSN",
			"postgres://exchange:exchange_dev@/migverify?host=/tmp&port=55433&sslmode=disable"),
		RedisAddr:   envOr("EXC_REDIS_TEST_ADDR", "127.0.0.1:16379"),
		RedisDB:     envInt("EXC_REDIS_TEST_DB", 9),
		NatsURLs:    splitCSV(envOr("EXC_NATS_URLS", "nats://127.0.0.1:4222")),
		CoreBin:     envOr("EXC_CORE_BIN", filepath.Join(root, "core", "build", "matching_engine")),
		CoreBuild:   envOr("EXC_CORE_BUILD", filepath.Join(root, "core", "build")),
		ServicesDir: filepath.Join(root, "services"),
		FrontendDir: filepath.Join(root, "frontend"),
		DocsDir:     filepath.Join(root, "docs"),
		TracePath:   filepath.Join(root, "tests", "spec", "traceability.json"),
		ReportPath:  envOr("EXC_IT_REPORT", filepath.Join(root, "tests", "integration", "reports", "coverage.json")),
		IPCBase:     envOr("EXC_IPC_BASE", fmt.Sprintf("excit%d", os.Getpid())),
	}
	shards := envOr("EXC_STACK_SHARDS", "0 1 2 3 4 5 6 7")
	for _, s := range strings.Fields(shards) {
		if n, err := strconv.Atoi(s); err == nil {
			e.Shards = append(e.Shards, n)
		}
	}
	return e
}

func envOr(k, def string) string {
	if v := strings.TrimSpace(os.Getenv(k)); v != "" {
		return v
	}
	return def
}

func envFirst(keys ...string) string {
	for _, k := range keys[:len(keys)-1] {
		if v := strings.TrimSpace(os.Getenv(k)); v != "" {
			return v
		}
	}
	return keys[len(keys)-1]
}

func envInt(k string, def int) int {
	if v := strings.TrimSpace(os.Getenv(k)); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
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

// ---------------------------------------------------------------------------
// Gates — each returns "" when satisfied, else the BLOCKED reason.
// ---------------------------------------------------------------------------

// GatePG reports "" when PostgreSQL legs may run.
func (e *Env) GatePG(ctx context.Context) string {
	if os.Getenv("EXC_PG_TEST") != "1" {
		return "EXC_PG_TEST=1 not set (PostgreSQL legs disabled)"
	}
	return e.probePG(ctx)
}

// GatePGUnflagged probes the DSN regardless of the flag — used by legs
// that only need connectivity evidence (still recorded as gated when the
// flag is absent).
func (e *Env) probePG(ctx context.Context) string {
	cctx, cancel := context.WithTimeout(ctx, 6*time.Second)
	defer cancel()
	conn, err := pgConnect(cctx, e.PostgresDSN)
	if err != nil {
		return fmt.Sprintf("postgres unreachable at %s: %v", e.PostgresDSN, err)
	}
	_ = conn.Close(cctx)
	return ""
}

// GateRedis reports "" when Redis legs may run.
func (e *Env) GateRedis(ctx context.Context) string {
	if os.Getenv("EXC_REDIS_TEST") != "1" {
		return "EXC_REDIS_TEST=1 not set (Redis legs disabled)"
	}
	return e.probeRedis(ctx)
}

func (e *Env) probeRedis(ctx context.Context) string {
	cctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	return redisPing(cctx, e.RedisAddr, e.RedisDB)
}

// GateSentinel reports "" when a live Sentinel quorum is reachable —
// EXC_SENTINEL_ADDRS is a CSV of host:port sentinel endpoints (the dev
// compose maps 127.0.0.1:36379-36381). The gate TCP-probes each listed
// addr and requires ≥2 reachable (quorum=2 per deploy/redis/sentinel.conf);
// the bound legs then run the EXC_SENTINEL_TEST suite which probes again.
func (e *Env) GateSentinel(ctx context.Context) string {
	if r := e.GateRedis(ctx); r != "" {
		return r
	}
	raw := os.Getenv("EXC_SENTINEL_ADDRS")
	if raw == "" {
		return "EXC_SENTINEL_ADDRS unset (CSV of sentinel host:port, dev = 127.0.0.1:36379,127.0.0.1:36380,127.0.0.1:36381)"
	}
	reachable := 0
	for _, a := range strings.Split(raw, ",") {
		a = strings.TrimSpace(a)
		if a == "" {
			continue
		}
		cctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		conn, err := (&net.Dialer{}).DialContext(cctx, "tcp", a)
		cancel()
		if err == nil {
			conn.Close()
			reachable++
		}
	}
	if reachable < 2 {
		return fmt.Sprintf("sentinel quorum unmet: %d reachable (need ≥2) in EXC_SENTINEL_ADDRS=%q", reachable, raw)
	}
	return ""
}

// GateNATS probes the JetStream seed.
func (e *Env) GateNATS(ctx context.Context) string {
	cctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	return natsProbe(cctx, e.NatsURLs)
}

// GateEngine reports "" when the matching_engine binary exists.
func (e *Env) GateEngine() string {
	st, err := os.Stat(e.CoreBin)
	if err != nil || st.IsDir() {
		return fmt.Sprintf("matching_engine binary absent at %s — build core (cmake --build core/build)", e.CoreBin)
	}
	return ""
}

// GateCoreTests reports "" when the ctest tree is available.
func (e *Env) GateCoreTests() string {
	if _, err := os.Stat(filepath.Join(e.CoreBuild, "CTestTestfile.cmake")); err != nil {
		return fmt.Sprintf("core build tree absent at %s — build core first", e.CoreBuild)
	}
	return ""
}

// GateDockerCompose reports "" when the docker daemon is usable.
func (e *Env) GateDockerCompose() string {
	if _, err := exec_LookPath("docker"); err != nil {
		return "docker CLI not on PATH"
	}
	// The socket check catches permission-denied (this host) without
	// spawning `docker info`.
	sock := envOr("DOCKER_HOST", "unix:///var/run/docker.sock")
	sock = strings.TrimPrefix(sock, "unix://")
	if strings.HasPrefix(sock, "/") {
		if conn, err := net.DialTimeout("unix", sock, 2*time.Second); err == nil {
			conn.Close()
		} else {
			return fmt.Sprintf("docker socket %s unusable: %v", sock, err)
		}
	}
	return ""
}

// gateNote is the standard BLOCKED reason formatter.
func gateNote(reason string) string { return "BLOCKED: " + reason }
