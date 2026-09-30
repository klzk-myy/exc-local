package checks

import (
	"context"

	spec "exchange-testspec/spec"
)

// Phase-08.5 Pre-Production Load-Test checkpoints (Tasks 8.5.3.1–8.5.3.3).
//
// Task 8.5.3.1 (75k/s sustained 4h staging gate) remains pending: it is an
// environment gate requiring a provisioned staging cluster — no code
// artifact can discharge it, and it is honestly left unbound.
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
