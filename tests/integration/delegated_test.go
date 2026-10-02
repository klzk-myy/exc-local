// Delegated legs — every non-e2e binding in itest.CriterionBindings is
// executed here as a subprocess (services' own `go test -run`, ctest,
// gtest filters, repo CLIs) or BLOCKED with the gate reason. Identical
// legs run once and credit every criterion that binds them.
package integration

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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

// gateFor maps a binding's Needs tag(s) to the env gate reason ("" = go).
// "+"-joined tags require every listed dependency; the first failing
// gate's reason is returned verbatim.
func gateFor(ctx context.Context, needs string) string {
	for _, tag := range strings.Split(needs, "+") {
		if r := gateTag(ctx, tag); r != "" {
			return r
		}
	}
	return ""
}

// gateTag evaluates one Needs tag.
func gateTag(ctx context.Context, tag string) string {
	switch tag {
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
		return env.GateSentinel(ctx)
	case "aeron":
		if _, err := os.Stat(env.Root + "/core/third_party/aeron/bin/aeronmd"); err != nil {
			return "aeron media driver (aeronmd) not vendored: " + err.Error()
		}
		return ""
	case "frontend":
		if _, err := os.Stat(env.FrontendDir + "/node_modules/.bin/vitest"); err != nil {
			return "frontend deps absent — npm ci in frontend/ first"
		}
		return ""
	default:
		return "unknown gate " + tag
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
	case itest.BindVitest:
		o = env.RunVitest(ctx, b.Pkg)
	case itest.BindCompose:
		// Compose/topology legs dispatch to the repo drill scripts the
		// owning phase authored — a leg is BLOCKED only when no runner
		// has been bound to its Run name yet.
		switch b.Run {
		case "redis-dr":
			out, err := env.RunBin(ctx, env.Root,
				"bash", "deploy/scripts/redis_failover_drill.sh", "--no-go-client")
			if err != nil {
				return "FAIL", "redis failover drill: " + err.Error() + "\n" + trunc(out, 400)
			}
			return "PASS", trunc(out, 400)
		case "ch-backup":
			out, err := env.RunBin(ctx, env.Root,
				"bash", "deploy/scripts/ch_restore_drill.sh")
			if err != nil {
				return "FAIL", "ch restore drill: " + err.Error() + "\n" + trunc(out, 400)
			}
			return "PASS", trunc(out, 400)
		case "haproxy-topology":
			// run_drill.sh is an interactive drill: it starts the proxy +
			// stub backends and exits, expecting a later `stop`. Without
			// the chained stop the suite leaks a bound :8080/:8443
			// container plus the stub processes on every run.
			out, err := env.RunBin(ctx, env.Root,
				"bash", "-c", "bash deploy/haproxy/test/run_drill.sh; rc=$?; bash deploy/haproxy/test/run_drill.sh stop >/dev/null 2>&1 || true; exit $rc")
			if err != nil {
				return "FAIL", "haproxy topology: " + err.Error() + "\n" + trunc(out, 400)
			}
			return "PASS", trunc(out, 400)
		case "dr-failover":
			// Criterion #44 credits only the parent region-failover row:
			// a real secondary cutover (or the runner's PASS verdict on
			// failover-runbook.sh). Composite sub-legs passing locally are
			// evidence but do not promote the criterion.
			rows, detail, err := runDRDrill(ctx, "D1")
			if err != nil {
				return "FAIL", "dr-failover runner: " + err.Error() + "\n" + detail
			}
			return drillRowVerdict(rows, "D1", "region-failover", detail)
		case "pg-pitr":
			out, err := env.RunBin(ctx, env.Root,
				"bash", "deploy/postgres/pitr_smoke.sh")
			if ee, ok := err.(*exec.ExitError); ok && ee.ExitCode() == 77 {
				return "BLOCKED", "pitr_smoke self-skip (pg binaries absent): " + trunc(out, 300)
			}
			if err != nil {
				return "FAIL", "pitr_smoke: " + err.Error() + "\n" + trunc(out, 400)
			}
			return "PASS", trunc(out, 400)
		case "pg-rpo-rto":
			out, err := env.RunBin(ctx, env.Root,
				"bash", "deploy/scripts/pg_failover_drill.sh")
			if err != nil {
				return "FAIL", "pg failover drill: " + err.Error() + "\n" + trunc(out, 400)
			}
			return "PASS", trunc(out, 400)
		default:
			return "BLOCKED", "compose leg unrun (no scenario runner bound for " + b.Run + ")"
		}
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

// drillRow is one results.jsonl record emitted by dr_drill_runner.sh.
type drillRow struct {
	Drill     string `json:"drill"`
	Component string `json:"component"`
	Verdict   string `json:"verdict"` // PASS | FAIL | SKIP
	Detail    string `json:"detail"`
}

// runDRDrill executes scripts/ops/dr_drill_runner.sh for the given drill
// set into a scratch evidence dir (never docs/incidents/drills/) and
// returns the parsed results rows. Runner process errors surface as err
// alongside verbatim output. EXC_DR_WAL_DIR / EXC_DR_SNAP_DIR enable the
// D1 local-composite legs when provided.
func runDRDrill(ctx context.Context, drills string) ([]drillRow, string, error) {
	if _, err := exec.LookPath("jq"); err != nil {
		return nil, "", fmt.Errorf("jq absent (drill runner dependency): %w", err)
	}
	dir, err := os.MkdirTemp("", "drdrill-*")
	if err != nil {
		return nil, "", err
	}
	args := []string{"scripts/ops/dr_drill_runner.sh",
		"--drills", drills,
		"--out", dir,
		"--report", filepath.Join(dir, "report.md")}
	if v := os.Getenv("EXC_DR_WAL_DIR"); v != "" {
		args = append(args, "--wal-dir", v)
	}
	if v := os.Getenv("EXC_DR_SNAP_DIR"); v != "" {
		args = append(args, "--snap-dir", v)
	}
	out, runErr := env.RunBin(ctx, env.Root, "bash", args...)
	rows, perr := parseDrillResults(filepath.Join(dir, "results.jsonl"))
	switch {
	case perr != nil && runErr != nil:
		return nil, trunc(out, 400), fmt.Errorf("runner failed (%v) and results unreadable (%v)", runErr, perr)
	case perr != nil:
		return nil, trunc(out, 400), fmt.Errorf("results.jsonl unreadable: %w", perr)
	case runErr != nil:
		// Non-zero exit with valid results → verdicts come from the rows;
		// keep the output for detail context.
		return rows, trunc(out, 400), nil
	}
	return rows, trunc(out, 400), nil
}

// parseDrillResults reads a dr_drill_runner.sh results.jsonl file.
func parseDrillResults(path string) ([]drillRow, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var rows []drillRow
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var r drillRow
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			return nil, fmt.Errorf("bad results row %q: %w", trunc(line, 120), err)
		}
		rows = append(rows, r)
	}
	return rows, sc.Err()
}

// drillRowVerdict maps the rows for one drill/component to a leg status:
// any FAIL → FAIL; a PASS → PASS; otherwise BLOCKED (SKIP or absent rows
// never fabricate a pass).
func drillRowVerdict(rows []drillRow, drill, component, detail string) (string, string) {
	found := false
	for _, r := range rows {
		if r.Drill != drill || r.Component != component {
			continue
		}
		found = true
		switch r.Verdict {
		case "FAIL":
			return "FAIL", fmt.Sprintf("%s/%s FAIL: %s\n%s", drill, component, r.Detail, detail)
		case "PASS":
			return "PASS", fmt.Sprintf("%s/%s PASS: %s", drill, component, r.Detail)
		}
	}
	if !found {
		return "BLOCKED", fmt.Sprintf("no %s/%s row emitted\n%s", drill, component, detail)
	}
	return "BLOCKED", fmt.Sprintf("%s/%s SKIP (prerequisite infra absent)\n%s", drill, component, detail)
}

// drillCatalogVerdict maps a full-catalog run to a leg status: any FAIL →
// FAIL; any SKIP → BLOCKED (incomplete evidence — the criterion states all
// RPO/RTO targets met); only an all-PASS catalog credits PASS.
func drillCatalogVerdict(rows []drillRow, detail string) (string, string) {
	var fails, skips []string
	for _, r := range rows {
		switch r.Verdict {
		case "FAIL":
			fails = append(fails, r.Drill+"/"+r.Component+": "+r.Detail)
		case "SKIP":
			skips = append(skips, r.Drill+"/"+r.Component)
		}
	}
	switch {
	case len(fails) > 0:
		return "FAIL", "drill failures: " + strings.Join(fails, "; ") + "\n" + detail
	case len(rows) == 0:
		return "BLOCKED", "runner emitted no rows\n" + detail
	case len(skips) > 0:
		return "BLOCKED", fmt.Sprintf("%d/%d drills skipped (infra absent): %s",
			len(skips), len(rows), strings.Join(skips, ", "))
	default:
		return "PASS", fmt.Sprintf("all %d drill components PASS", len(rows))
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
	case "dr-drill-runner":
		rows, detail, err := runDRDrill(ctx, "D1,D2,D3,D4,D5,D6,D7")
		if err != nil {
			return "FAIL", "dr-drill-runner: " + err.Error() + "\n" + detail
		}
		return drillCatalogVerdict(rows, detail)
	case "pentest":
		// Phase-13.5 harness: seeds fixtures, boots a real gateway with the
		// documented dev JWT key, runs the black-box suite then the
		// in-process white-box suite. Needs the dev PG/Redis/NATS trio.
		out, err := env.RunBin(ctx, env.Root+"/tests/pentest",
			"bash", "run.sh")
		if err != nil {
			return "FAIL", "pentest harness: " + err.Error() + "\n" + trunc(out, 400)
		}
		return "PASS", trunc(out, 400)
	case "bluegreen-gate":
		if _, err := exec.LookPath("python3"); err != nil {
			return "BLOCKED", "python3 absent (mock gateway dependency)"
		}
		out, err := env.RunBin(ctx, env.Root,
			"bash", "deploy/scripts/tests/bluegreen_gate_test.sh")
		if err != nil {
			return "FAIL", "bluegreen gate suite: " + err.Error() + "\n" + trunc(out, 400)
		}
		return "PASS", trunc(out, 400)
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
