// Delegated legs — every non-e2e binding in itest.CriterionBindings is
// executed here as a subprocess (services' own `go test -run`, ctest,
// gtest filters, repo CLIs) or BLOCKED with the gate reason. Identical
// legs run once and credit every criterion that binds them.
package integration

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"exchange-integration/itest"
)

// legKey dedups identical delegated invocations shared across criteria.
func legKey(b itest.Binding) string {
	return fmt.Sprintf("%s|%s|%s|%s|%s", b.Kind, b.Pkg, b.Run, b.Binary, b.Filter)
}

// gateFor maps a binding's Needs tag to the env gate reason ("" = go).
func gateFor(ctx context.Context, needs string) string {
	switch needs {
	case "":
		return ""
	case "pg":
		return env.GatePG(ctx)
	case "redis":
		return env.GateRedis(ctx)
	case "nats":
		return env.GateNATS(ctx)
	case "engine":
		return env.GateEngine()
	case "docker":
		return env.GateDockerCompose()
	case "sentinel":
		if r := env.GateRedis(ctx); r != "" {
			return r
		}
		return "sentinel quorum topology not provisioned on this host (EXC_SENTINEL_ADDRS unset)"
	case "aeron":
		return "aeron media driver (aeronmd) not provisioned on this host"
	default:
		return "unknown gate " + needs
	}
}

// runDelegated executes one bound leg and returns (status, detail).
// status ∈ PASS | FAIL | BLOCKED.
func runDelegated(ctx context.Context, b itest.Binding) (string, string) {
	if r := gateFor(ctx, b.Needs); r != "" {
		return "BLOCKED", r
	}
	var o itest.Outcome
	switch b.Kind {
	case itest.BindGoTest:
		// TestRBACLifecycleIntegration is bound under two criteria
		// (#26 with FailsClosed, #348 alone) and is not idempotent on a
		// shared scratch DB — its fixed 9200xx principals leave ACTIVE
		// bindings that collide on re-run. Scrub the namespaced residue
		// before each invocation (same SQL the stack seed runs).
		if b.Pkg == "./internal/admin" && strings.Contains(b.Run, "TestRBACLifecycleIntegration") {
			if pool, err := env.Pool(ctx); err == nil {
				itest.ScrubDelegatedResidue(ctx, pool)
				pool.Close()
			}
		}
		o = env.RunGoTest(ctx, b.Pkg, b.Run)
	case itest.BindCTest:
		if r := env.GateCoreTests(); r != "" {
			return "BLOCKED", r
		}
		o = env.RunCTest(ctx, b.Run)
	case itest.BindGTest:
		if r := env.GateCoreTests(); r != "" {
			return "BLOCKED", r
		}
		o = env.RunGTest(ctx, b.Binary, b.Filter)
	case itest.BindBin:
		return runBinLeg(ctx, b)
	case itest.BindCompose:
		return "BLOCKED", "compose leg unrun (docker gate passed but no compose scenario runner bound)"
	default:
		return "BLOCKED", "unknown binding kind " + string(b.Kind)
	}
	switch {
	case o.OK:
		return "PASS", o.Output
	case o.Skipped:
		return "BLOCKED", o.Output
	default:
		return "FAIL", o.Output
	}
}

// runBinLeg handles repo-CLI bindings.
func runBinLeg(ctx context.Context, b itest.Binding) (string, string) {
	switch b.Binary {
	case "faultinject":
		out, err := env.RunBin(ctx, env.Root+"/ci/fault-injection",
			"bash", "run.sh")
		if err != nil {
			return "FAIL", "faultinject harness: " + err.Error() + "\n" + trunc(out, 400)
		}
		return "PASS", trunc(out, 400)
	case "snapbench":
		out, err := env.RunBin(ctx, env.CoreBuild, env.CoreBuild+"/snapbench")
		if err != nil {
			return "FAIL", "snapbench: " + err.Error() + "\n" + trunc(out, 300)
		}
		// Evidence-only: snapbench measures snapshot build/serialize
		// throughput, not the sustained 50k ord/s + p99 tick-to-trade
		// the criteria require — recorded but never promotes to PASS.
		return "BLOCKED", "evidence-only (soak harness absent): " + trunc(out, 300)
	default:
		return "BLOCKED", "unhandled bin binding " + b.Binary
	}
}

// TestDelegatedLegs executes every non-e2e bound leg once (deduped) and
// records the outcome against each criterion that binds it.
func TestDelegatedLegs(t *testing.T) {
	ctx := context.Background()

	// Collect unique legs → criteria.
	type legRef struct {
		binding itest.Binding
		critIDs []int
	}
	legs := map[string]*legRef{}
	var order []string
	for id, bs := range itest.CriterionBindings {
		for _, b := range bs {
			if b.Kind == itest.BindE2E {
				continue // executed by the named live-stack tests
			}
			k := legKey(b)
			if _, ok := legs[k]; !ok {
				legs[k] = &legRef{binding: b}
				order = append(order, k)
			}
			legs[k].critIDs = append(legs[k].critIDs, id)
		}
	}
	sort.Strings(order)

	for _, k := range order {
		lr := legs[k]
		b := lr.binding
		sort.Ints(lr.critIDs)
		name := fmt.Sprintf("%s:%s%s%s%s", b.Kind, b.Pkg, b.Run, b.Binary, b.Filter)
		if len(name) > 80 {
			name = name[:80]
		}
		t.Run(name, func(t *testing.T) {
			start := time.Now()
			st, detail := runDelegated(ctx, b)
			elapsed := time.Since(start)
			for _, id := range lr.critIDs {
				reg.Record(id, fmt.Sprintf("%s:%s%s%s", b.Kind, b.Pkg+b.Run, b.Binary, b.Filter),
					st, detail, elapsed)
			}
			switch st {
			case "PASS":
				t.Log(detail)
			case "BLOCKED":
				t.Skipf("BLOCKED: %s", detail)
			default:
				t.Fatalf("FAIL: %s", detail)
			}
		})
	}
}
