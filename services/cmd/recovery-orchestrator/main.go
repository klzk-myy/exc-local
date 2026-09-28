// Command recovery-orchestrator is the Task 4.3.10 crash-recovery / DR
// orchestration host: Redis epoch-lease fencing + warm-standby promotion,
// the 6-stage pre-open integrity audit, the CANCEL_ONLY → auction →
// NORMAL reopen ladder, per-shard scoped reopen with feed fallback
// (4.3.12), and multi-region DR step orchestration.
//
// Live scope: wires the Postgres-backed audit source, digest store and
// recovery_reports sink when postgres is reachable, the Redis lease
// backend (REDIS_URL or redis.addr), and idles as the long-running
// orchestration daemon. External bank/CLS feeds, the engine telemetry IPC
// and the DR driver are deployment seams — they are injected by site
// wiring; without them the audit's feed stages fail closed rather than
// silently pass.
//
// Shutdown flushes dirty WAL blocks and releases held leader leases.
package main

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"

	"exchange/internal/config"
	"exchange/internal/db"
	"exchange/internal/recovery"
	exredis "exchange/internal/redis"
	"exchange/internal/utils"
	"exchange/pkg/logging"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "recovery-orchestrator: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	level, _ := logging.ParseLevel(cfg.Logging.Level)
	log, err := logging.New(level, cfg.Logging.Format)
	if err != nil {
		return err
	}

	ctx, stop := utils.SignalContext()
	defer stop()

	// Redis lease backend — REDIS_URL overrides redis.addr (dev: redis on
	// 127.0.0.1:6379; compose maps the coordination primary to 16379).
	addr := cfg.Redis.Addr
	if u := strings.TrimSpace(os.Getenv("REDIS_URL")); u != "" {
		addr = strings.TrimPrefix(strings.TrimPrefix(u, "redis://"), "rediss://")
	}
	rdb := exredis.New(addr, cfg.Redis.Password, cfg.Redis.DB)
	leases, err := recovery.NewOrchRedisLeases(rdb)
	if err != nil {
		return err
	}

	// Postgres-backed providers — optional at boot: a dead OLTP must not
	// wedge the fencing daemon, but then the audit's DB stages fail
	// closed (nil provider → stage FAIL) instead of passing vacuously.
	var auditData recovery.OrchAuditData
	var reports recovery.OrchReportSink
	var digests recovery.OrchDigestStore
	pool, perr := db.NewPool(ctx, cfg.Postgres.DSN, cfg.Postgres.MaxConns)
	if perr != nil {
		log.Warn("postgres unavailable at boot; audit DB stages will fail closed", "err", perr)
	} else {
		defer pool.Close()
		if a, err := recovery.NewOrchPgAudit(pool); err == nil {
			auditData = a
		}
		if s, err := recovery.NewOrchPgReportSink(pool); err == nil {
			reports = s
		}
		if d, err := recovery.NewOrchPgDigestStore(pool); err == nil {
			digests = d
		}
	}

	audit := recovery.NewOrchAuditEngine(recovery.OrchAuditConfig{
		Data:    auditData,
		Digests: digests,
		Reports: reports,
		// Engine telemetry, bank/CLS feeds, suspense marker and the
		// post-open reconcile hook are deployment seams — left nil here;
		// their stages fail closed (or defer per the 4.3.12 rules) rather
		// than silently pass.
	})

	shards := parseShards(os.Getenv("EXC_RECOVERY_SHARDS"))
	if len(shards) == 0 {
		shards = []int{0}
	}
	orch, err := recovery.NewRecoveryOrchestrator(
		recovery.OrchConfig{Shards: shards},
		recovery.OrchDeps{Leases: leases, Audit: audit, Reports: reports})
	if err != nil {
		return err
	}

	log.Info("recovery-orchestrator started",
		"env", cfg.Environment, "shards", shards, "redis", addr)
	<-ctx.Done()

	log.Info("recovery-orchestrator shutting down: flushing WAL blocks, releasing leases")
	if err := orch.Shutdown(context.WithoutCancel(ctx)); err != nil {
		log.Error("shutdown flush incomplete", "err", err)
		return err
	}
	return nil
}

// parseShards reads "0,1,2" → []int{0,1,2}; empty/invalid entries are
// dropped. All-invalid input yields nil (caller defaults to shard 0).
func parseShards(s string) []int {
	var out []int
	for _, p := range strings.Split(s, ",") {
		if v, err := strconv.Atoi(strings.TrimSpace(p)); err == nil && v >= 0 {
			out = append(out, v)
		}
	}
	return out
}
