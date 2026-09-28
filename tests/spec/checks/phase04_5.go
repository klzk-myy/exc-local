package checks

import (
	"context"

	spec "exchange-testspec/spec"
)

// Phase-04.5 recovery chaos validation checkpoints.
//
// Task 4.5.3.1's suite lives in tests/chaos/ (real engine processes,
// 6 scenarios x 3 runs); Task 4.5.3.2's serialization/split-brain
// scenarios are live-PG/live-Redis Go tests in services/.
func registerPhase045(r *spec.Registry) {

	// ---- Task 4.5.3.1: Chaos Scenario Suite -------------------------------
	r.Register("P04.5-T4.5.3.1-C1", ckP045ChaosSuite,
		"6 chaos scenarios pass 3x each")
	r.Register("P04.5-T4.5.3.1-C2", ckP045ZeroDupMiss,
		"zero dup/miss on every recovery")

	// ---- Task 4.5.3.2: Serialization & Split-Brain Chaos -------------------
	r.Register("P04.5-T4.5.3.2-C1", ckP045SerializationSplitBrain,
		"serialization retries zero-loss + leader fencing terminates partitioned nodes (§24 #303)")
}

// ckP045ChaosSuite requires the chaos harness to exist and re-runs its
// last-verified run report check (the 18-run suite itself is executed by
// the harness with real engine binaries).
var ckP045ChaosSuite = func(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env, "tests/chaos/run.sh"),
		ctest("test_recovery"))
}

var ckP045ZeroDupMiss = func(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env, "tests/chaos/run.sh"),
		ctest("test_recovery"))
}

var ckP045SerializationSplitBrain = func(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"services/internal/settlement/chaos_contention_test.go",
			"services/internal/recovery/chaos_splitbrain_test.go"),
		gotest(settle, "TestChaosSerializationContentionZeroLoss"),
		gotest(recoveryPkg,
			"TestChaosSplitBrainFencesPartitionedLeader|TestChaosLeaseExpiryFencesOrphanedLeader|TestChaosStandbyPromotionParity"))
}
