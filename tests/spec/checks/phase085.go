package checks

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	spec "exchange-testspec/spec"
)

// Phase-08.5 Pre-Production Load-Test checkpoints (Tasks 8.5.3.1–8.5.3.3).
//
// Task 8.5.3.1 (75k/s sustained 4h staging gate) is an environment gate
// requiring a provisioned staging cluster — a CI-time check cannot prove a
// 4h wall-clock run. It is therefore ARTIFACT-GATED like the Phase-02.5
// soak checkpoints: the checker validates the recorded staging report at
// tests/load/results/<run>/staging-report.json (schema/example:
// tests/load/staging-report.example.json) and returns Pending while no
// qualifying artifact exists. A ≥4h artifact is held to every §8.5.7
// row 1–10 criterion: measured-but-violated → Fail; absent/unmeasured →
// Pending (the run.sh BLOCKED rule — unmeasured never counts as pass).
//
// Task 8.5.3.2 evidence: internal/demo service + migration 238
// (account_type_enum +DEMO, demo_expires_at, expiry index) + TierDemo
// 2×-Basic rate limits + DEMO→demo tier substitution at every TierResolver
// seam (REST introspection, WS, L3) + fail-closed funding isolation via
// WrapFundingChecker + 30-day inactivity expiry sweeper wired into the
// gateway's daily jobs. Enabled() gates the whole surface on the "demo"
// deployment label.
//
// Task 8.5.3.3 evidence: tests/load/errblast standalone module — paced
// invalid-order engine (5 rotating defect classes), control-stream health
// probes, latency histogram, crash/5xx verdict policy, live-verified
// against real gateway+oracle binaries (6k invalid requests, 0 5xx, 0
// timeouts). Hysteresis pinned by C++ dwell tests (30 consecutive clear
// seconds, blip resets timer) and Go middleware tests (ReadOnly
// write-reject/read-pass, MarketDataOnly, Maintenance, fail-closed
// unreadable mode, live Redis mode-record round-trip).
func registerPhase085(r *spec.Registry) {
	r.Register("P08.5-T8.5.3.1-C1", ckP085StagingGate,
		"75k/sec staging gate — artifact-gated "+
			"(tests/load/results/<run>/staging-report.json)")
	r.Register("P08.5-T8.5.3.2-C1", ckP085DemoEnv,
		"isolated demo environment mirrors the production API without real funds (§24 #266) — defined first, validated against spec")
	r.Register("P08.5-T8.5.3.3-C1", ckP085ErrBlast,
		"High-stress error injection and degradation mode hysteresis validated (§24 #308) — defined first, validated against spec")
}

// --- Task 8.5.3.2: Demo / Paper-Trading Environment ---------------------------

func ckP085DemoEnv(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"services/internal/demo/demo.go",
			"services/internal/demo/demo_test.go",
			"services/internal/demo/demo_pg_test.go",
			"services/internal/db/migrations/238_demo_accounts.up.sql",
			"services/internal/db/migrations/238_demo_accounts.down.sql"),
		structural(env, "services/internal/db/migrations/238_demo_accounts.up.sql",
			"DEMO", "demo_expires_at", "idx_accounts_demo_expiry"),
		structural(env, "services/internal/demo/demo.go",
			"Service\\) Enabled", "Service\\) Provision", "Service\\) TouchActivity",
			"Service\\) ExpireSweep", "Service\\) WrapFundingChecker",
			"DefaultBalanceUSD", "DefaultLifetime"),
		structural(env, "services/internal/ratelimit/tier.go",
			"TierDemo"),
		structural(env, "services/internal/api/introspection.go",
			"PgTierLookup", "DEMO"),
		structural(env, "services/cmd/gateway/main.go",
			"EXC_DEMO_BALANCE_USD", "WrapFundingChecker", "demo-expiry"),
		gotest("./internal/demo",
			"TestEnabledOnlyOnDemoEnv|TestProvisionRejectsNonDemoEnv|"+
				"TestFundingGateRejectsDemo|TestFundingGateLookupFailureFailsClosed|"+
				"TestExpireSweepIntegration|TestRegisterProvisionsDemoIntegration|"+
				"TestIsDemoAndFundingGateIntegration"),
		gotest("./internal/ratelimit", "TestTierSpecsAreSpecValues"),
		gotest("./internal/api", "TestTierResolverFromLookup"),
	)
}

// --- Task 8.5.3.3: Error Injection + Degradation Hysteresis -------------------

func ckP085ErrBlast(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"tests/load/errblast/go.mod",
			"tests/load/errblast/blast.go",
			"tests/load/errblast/main.go",
			"tests/load/errblast/blast_test.go",
			"tests/load/errblast/live_test.go",
			"tests/load/errblast/README.md"),
		structural(env, "tests/load/errblast/blast.go",
			"negative", "malformed", "signature"),
		structural(env, "tests/load/errblast/main.go",
			"rate", "duration", "5xx"),
		structural(env, "tests/load/errblast/README.md",
			"EXC_ERRBLAST_LIVE"),
		structural(env, "services/internal/middleware/degradation_test.go",
			"TestDegradationGate_ReadOnlyRejectsWritesKeepsReads",
			"TestDegradationGate_MarketDataOnly",
			"TestDegradationGate_FailClosed"),
		structural(env, "core/tests/test_health.cpp",
			"InfraLagSignalEscalatesToReadOnlyImmediately",
			"ReadOnlyRecoveryDwellRequires30ConsecutiveClearSeconds"),
		gotestDir("tests/load/errblast",
			"TestDefect|TestBlast|TestVerdict|TestReport"),
		gotest("./internal/middleware", "TestDegradationGate"),
		gtest("test_health",
			"*InfraLagSignalEscalatesToReadOnlyImmediately:*"+
				"ReadOnlyRecoveryDwellRequires30ConsecutiveClearSeconds*"),
	)
}

// --- Task 8.5.3.1: Staging Load Test (75k/s × 4h gate) -------------------------

// stagingReportJSON is the evidence contract for the 75k/s × 4h staging gate
// (Phase-08.5 Task 8.5.3.1, §8.5.7 AC rows 1–10). A recorded run lands at
// tests/load/results/<run>/staging-report.json — fields are pointers so an
// absent measurement is distinguishable from a measured zero (unmeasured →
// Pending, violated → Fail). Full schema: tests/load/staging-report.example.json.
type stagingReportJSON struct {
	Run               string   `json:"run"`
	Env               *string  `json:"env"`
	DurationS         *float64 `json:"duration_s"`
	TargetRate        *float64 `json:"target_rate"`
	DuplicateTradeIDs *uint64  `json:"duplicate_trade_ids"`
	Throughput        struct {
		Min1S  *float64 `json:"min_1s"`
		Mean1S *float64 `json:"mean_1s"`
	} `json:"throughput"`
	LatencyNs struct {
		P99  *float64 `json:"p99"`
		P999 *float64 `json:"p999"`
	} `json:"latency_ns"`
	ReplicaLagMsMax *float64 `json:"postgres_replica_lag_ms_max"`
	WS              struct {
		Connections *int64   `json:"connections"`
		Drops       *int64   `json:"drops"`
		FanoutP99Ms *float64 `json:"fanout_p99_ms"`
	} `json:"ws"`
	L3DeltaMismatches *int64 `json:"l3_delta_mismatches"`
	RecoveryDrill     *struct {
		OffsetS    *float64 `json:"offset_s"`
		RecoveryMs *float64 `json:"recovery_ms"`
		OrdersLost *int64   `json:"orders_lost"`
	} `json:"recovery_drill"`
	PagerDutyAlerts *int64 `json:"pagerduty_alerts"`
	WalLagAlerts    *int64 `json:"wal_lag_alerts"`
	Memory          struct {
		PeakBytes  *int64 `json:"peak_bytes"`
		LimitBytes *int64 `json:"limit_bytes"`
	} `json:"memory"`
}

// need dereferences a measured criterion; a nil field records an unmeasured
// criterion instead (run.sh BLOCKED semantics — absence never passes).
func need[T any](name string, p *T, missing *[]string) (T, bool) {
	var z T
	if p == nil {
		*missing = append(*missing, name)
		return z, false
	}
	return *p, true
}

func ckP085StagingGate(ctx context.Context, env *spec.Env) spec.Result {
	// Executable substrate: the load harness + probe tools and the committed
	// report schema must exist; the probe module must build.
	if r := spec.RequireFiles(env,
		"tests/load/run.sh", "tests/load/go.mod",
		"tests/load/wsprobe/main.go", "tests/load/errblast/blast.go",
		"tests/load/staging-report.example.json"); r.Status != spec.StatusPass {
		return r
	}
	if out, err := spec.RunOutput(ctx, env.Path("tests", "load"),
		"go", "build", "./..."); err != nil {
		return spec.Failf("tests/load build: %v — %s", err, spec.Tail(out, 8))
	}

	dir, err := latestArtifactRun(env.Path("tests", "load", "results"),
		"staging-report.json")
	if err != nil {
		return spec.Pending("substrate verified; no recorded staging run yet " +
			"(staging-report.json pending under tests/load/results/)")
	}
	raw, err := os.ReadFile(filepath.Join(dir, "staging-report.json"))
	if err != nil {
		return spec.Failf("read staging-report.json: %v", err)
	}
	var rep stagingReportJSON
	if err := json.Unmarshal(raw, &rep); err != nil {
		return spec.Failf("parse staging-report.json: %v", err)
	}

	const needS = 4 * 3600.0
	if rep.DurationS == nil || *rep.DurationS < needS {
		got := 0.0
		if rep.DurationS != nil {
			got = *rep.DurationS
		}
		return spec.Pendingf("substrate verified; recorded staging run %.1fh "+
			"< 4h (%s) — gate pending", got/3600, filepath.Base(dir))
	}

	var missing, bad []string
	if v, ok := need("env", rep.Env, &missing); ok && v != "staging" {
		bad = append(bad, fmt.Sprintf("env %q != staging", v))
	}
	if v, ok := need("target_rate", rep.TargetRate, &missing); ok && v < 75000 {
		bad = append(bad, fmt.Sprintf("target_rate %.0f < 75k", v))
	}
	if v, ok := need("throughput.min_1s", rep.Throughput.Min1S, &missing); ok && v < 75000 {
		bad = append(bad, fmt.Sprintf("throughput.min_1s %.0f/s < 75k", v))
	}
	if v, ok := need("latency_ns.p99", rep.LatencyNs.P99, &missing); ok && v > 50_000 {
		bad = append(bad, fmt.Sprintf("p99 %.0fns > 50µs", v))
	}
	if v, ok := need("postgres_replica_lag_ms_max", rep.ReplicaLagMsMax, &missing); ok && v >= 2000 {
		bad = append(bad, fmt.Sprintf("replica lag %.0fms >= 2s", v))
	}
	if v, ok := need("ws.connections", rep.WS.Connections, &missing); ok && v < 10000 {
		bad = append(bad, fmt.Sprintf("ws.connections %d < 10,000", v))
	}
	if v, ok := need("ws.drops", rep.WS.Drops, &missing); ok && v != 0 {
		bad = append(bad, fmt.Sprintf("ws.drops %d", v))
	}
	if v, ok := need("ws.fanout_p99_ms", rep.WS.FanoutP99Ms, &missing); ok && v >= 50 {
		bad = append(bad, fmt.Sprintf("ws fanout p99 %.1fms >= 50ms", v))
	}
	if v, ok := need("l3_delta_mismatches", rep.L3DeltaMismatches, &missing); ok && v != 0 {
		bad = append(bad, fmt.Sprintf("l3_delta_mismatches %d", v))
	}
	if v, ok := need("duplicate_trade_ids", rep.DuplicateTradeIDs, &missing); ok && v != 0 {
		bad = append(bad, fmt.Sprintf("duplicate_trade_ids %d", v))
	}
	if rep.RecoveryDrill == nil {
		missing = append(missing, "recovery_drill")
	} else {
		if v, ok := need("recovery_drill.recovery_ms", rep.RecoveryDrill.RecoveryMs, &missing); ok && v >= 10_000 {
			bad = append(bad, fmt.Sprintf("recovery drill %.0fms >= 10s", v))
		}
		if v, ok := need("recovery_drill.orders_lost", rep.RecoveryDrill.OrdersLost, &missing); ok && v != 0 {
			bad = append(bad, fmt.Sprintf("recovery drill orders_lost %d", v))
		}
	}
	if v, ok := need("pagerduty_alerts", rep.PagerDutyAlerts, &missing); ok && v != 0 {
		bad = append(bad, fmt.Sprintf("pagerduty_alerts %d", v))
	}
	if v, ok := need("wal_lag_alerts", rep.WalLagAlerts, &missing); ok && v != 0 {
		bad = append(bad, fmt.Sprintf("wal_lag_alerts %d", v))
	}
	peak, okP := need("memory.peak_bytes", rep.Memory.PeakBytes, &missing)
	limit, okL := need("memory.limit_bytes", rep.Memory.LimitBytes, &missing)
	if okP && okL && peak > limit {
		bad = append(bad, fmt.Sprintf("memory peak %d > limit %d", peak, limit))
	}

	if len(bad) > 0 {
		return spec.Failf("staging report %s violates gate: %s",
			filepath.Base(dir), strings.Join(bad, "; "))
	}
	if len(missing) > 0 {
		return spec.Pendingf("staging report %s: unmeasured criteria [%s] — "+
			"unmeasured never passes (run.sh BLOCKED rule)",
			filepath.Base(dir), strings.Join(missing, ", "))
	}
	return spec.Passf("staging gate discharged by %s: 75k/s × %.1fh, p99 %.0fns, "+
		"replica lag %.0fms, %d WS conns 0 drops, L3 exact, drill %.0fms 0 lost, "+
		"0 PD / 0 WAL alerts", filepath.Base(dir), *rep.DurationS/3600,
		*rep.LatencyNs.P99, *rep.ReplicaLagMsMax, *rep.WS.Connections,
		*rep.RecoveryDrill.RecoveryMs)
}
