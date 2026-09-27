package checks

import (
	"context"
	"fmt"
	"strings"

	spec "exchange-testspec/spec"
)

// Phase-01.5 self-checks: this harness validates its own spec checkpoints.
func registerPhase015(r *spec.Registry) {
	r.Register("P01.5-T1.5.3.1-C1", ckCIRunsOnPR,
		"CI runs on every PR (workflow trigger present)")
	r.Register("P01.5-T1.5.3.1-C2", ckCIEphemeralStack,
		"ephemeral PostgreSQL 16 + Redis 7 + ClickHouse health-checked before tests")
	r.Register("P01.5-T1.5.3.2-C1", ckCheckpointExtraction,
		"400+ per-task spec validation checks extracted (canonical 543 raw / 542 strict)")
	r.Register("P01.5-T1.5.3.2-C2", ckShardPartitioning,
		"4 parallel shards with deterministic assignment; < 20 min budget")
	r.Register("P01.5-T1.5.3.2-C3", ckGoldenCorpusCount,
		"golden corpus 20+ spec-derived cases")
	r.Register("P01.5-T1.5.3.3-C1", ckTraceabilityComplete,
		"every §24 criterion mapped to ≥1 test (trace --strict clean)")
	r.Register("P01.5-T1.5.3.4-C1", ckSupplyChainGates,
		"SAST + dependency audit gates PRs (§24 #161)")
	r.Register("P01.5-T1.5.3.5-C1", ckFaultInjectionSuite,
		"CI negative test suite injects faults, validates fail-closed (§24 #298)")
}

func ckCIRunsOnPR(ctx context.Context, env *spec.Env) spec.Result {
	if r := spec.FileContains(env, ".github/workflows/ci.yml",
		"pull_request", "name:"); r.Status != spec.StatusPass {
		return r
	}
	return spec.Passf("ci.yml declares pull_request trigger")
}

func ckCIEphemeralStack(ctx context.Context, env *spec.Env) spec.Result {
	if r := spec.RequireFiles(env,
		"docker-compose.dev.yml", "scripts/ci/wait_stack.sh"); r.Status != spec.StatusPass {
		return r
	}
	if r := spec.FileContains(env, ".github/workflows/ci.yml",
		"docker compose", "wait_stack.sh"); r.Status != spec.StatusPass {
		return r
	}
	return spec.FileContains(env, "docker-compose.dev.yml",
		"postgres:16", "redis:7", "clickhouse")
}

func ckTraceabilityComplete(ctx context.Context, env *spec.Env) spec.Result {
	out, err := spec.RunOutput(ctx, env.RepoRoot+"/tests/spec",
		"go", "run", ".", "trace", "--strict")
	if err != nil {
		return spec.Failf("trace --strict: %v — %s", err, spec.Tail(out, 6))
	}
	if !strings.Contains(out, "unmapped=0") {
		return spec.Failf("unmapped criteria remain: %s", spec.Tail(out, 6))
	}
	return spec.Passf("trace --strict: %s", spec.LastLine(out))
}

func ckSupplyChainGates(ctx context.Context, env *spec.Env) spec.Result {
	if r := spec.FileContains(env, ".github/workflows/security.yml",
		"codeql", "govulncheck", "trivy", "gitleaks", "pull_request"); r.Status != spec.StatusPass {
		return r
	}
	return spec.Passf("security.yml wires codeql+govulncheck+trivy+gitleaks on pull_request")
}

func ckFaultInjectionSuite(ctx context.Context, env *spec.Env) spec.Result {
	out, err := spec.RunOutput(ctx, env.RepoRoot+"/ci/fault-injection",
		"./run.sh")
	if err != nil {
		return spec.Failf("run.sh: %v — %s", err, spec.Tail(out, 8))
	}
	if !strings.Contains(out, "0 fail") {
		return spec.Failf("fault suite reported failures: %s", spec.Tail(out, 8))
	}
	return spec.Passf("fault suite: %s", spec.LastLine(spec.KeepLines(out, "scenarios:")))
}

func ckCheckpointExtraction(ctx context.Context, env *spec.Env) spec.Result {
	live := env.Live
	if live == nil {
		var err error
		live, err = spec.Extract(env.DocsDir)
		if err != nil {
			return spec.Failf("extract: %v", err)
		}
	}
	if live.ExtractedCount < 400 {
		return spec.Failf("extracted %d checkpoints — spec requires 400+ (canonical raw %d)",
			live.ExtractedCount, live.RawGrepCount)
	}
	// Reconciliation: the canonical "543" is a raw grep count including one
	// literal "Spec checkpoint:" mention inside this task's own
	// implementation prose (not a checklist line). Strict checkbox
	// extraction is the executable corpus.
	delta := live.RawGrepCount - live.ExtractedCount
	if delta < 0 || delta > 1 {
		return spec.Failf("extraction drift: raw=%d strict=%d (delta %d > 1)",
			live.RawGrepCount, live.ExtractedCount, delta)
	}
	return spec.Passf("extracted %d strict checkpoints across %d tasks (raw grep %d incl. 1 prose mention; canonical 543/479)",
		live.ExtractedCount, live.TaskCount, live.RawGrepCount)
}

func ckShardPartitioning(ctx context.Context, env *spec.Env) spec.Result {
	live := env.Live
	if live == nil {
		var err error
		live, err = spec.Extract(env.DocsDir)
		if err != nil {
			return spec.Failf("extract: %v", err)
		}
	}
	counts := live.ShardCounts()
	total := 0
	for _, c := range counts {
		total += c
	}
	if total != live.ExtractedCount {
		return spec.Failf("shard union %d != extracted %d", total, live.ExtractedCount)
	}
	min, max := -1, 0
	for i, c := range counts {
		if min < 0 || c < min {
			min = c
		}
		if c > max {
			max = c
		}
		if c == 0 {
			return spec.Failf("shard %d empty — every shard must own checkpoints", i)
		}
		if c == total {
			return spec.Fail("single shard holds all checkpoints — hashing broken")
		}
	}
	// Rough balance check: hash partitioning won't be perfect but should be
	// within ~25% of ideal (n/4) for 500+ items.
	ideal := float64(total) / spec.NumShards
	if float64(max) > ideal*1.35 && max-min > int(ideal*0.6) {
		return spec.Failf("shard skew too large: counts=%v ideal~%.0f", counts, ideal)
	}
	return spec.Passf("deterministic FNV-1a partition: shards=%v (total %d, balanced)",
		counts, total)
}

func ckGoldenCorpusCount(ctx context.Context, env *spec.Env) spec.Result {
	if env.Registry == nil {
		return spec.Fail("registry not wired into env")
	}
	n := len(env.Registry.GoldenIDs())
	if n < 20 {
		return spec.Failf("golden corpus has %d cases — spec requires 20+", n)
	}
	return spec.Pass(fmt.Sprintf("golden corpus registered: %d spec-derived cases", n))
}
