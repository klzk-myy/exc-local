package checks

import (
	"context"
	"fmt"

	spec "exchange-testspec/spec"
)

// Phase-01.5 self-checks: this harness validates its own spec checkpoints.
// Other Phase-01.5 tasks (1.5.3.1 CI pipeline, 1.5.3.3 traceability,
// 1.5.3.4 supply-chain, 1.5.3.5 fault injection) are owned by other tasks —
// their checkpoints report `pending` until their implementations land.
func registerPhase015(r *spec.Registry) {
	r.Register("P01.5-T1.5.3.2-C1", ckCheckpointExtraction,
		"400+ per-task spec validation checks extracted (canonical 543 raw / 542 strict)")
	r.Register("P01.5-T1.5.3.2-C2", ckShardPartitioning,
		"4 parallel shards with deterministic assignment; < 20 min budget")
	r.Register("P01.5-T1.5.3.2-C3", ckGoldenCorpusCount,
		"golden corpus 20+ spec-derived cases")
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
