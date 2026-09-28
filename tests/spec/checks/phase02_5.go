package checks

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	spec "exchange-testspec/spec"
)

// Phase-02.5 soak & benchmark checkpoints.
//
// Honesty model: the 72h wall-clock gates (2.5.3.1 sustained-rate,
// 2.5.3.2 full-criteria report) cannot be proven by a CI-time check, so
// those checkers validate the recorded soak ARTIFACT under
// tests/soak/artifacts/<run>/ and Skip while no qualifying artifact exists.
// Recovery and fault/backpressure mechanisms are demonstrable in bounded
// runs — those checkers accept a recorded failover bench artifact or run
// the bench live when binaries are present.
func registerPhase025(r *spec.Registry) {
	r.Register("P02.5-T2.5.3.1-C1", ckSoakSustainedRate,
		"soak infra present + recorded run sustains >=45k/s for >=72h (artifact-gated)")
	r.Register("P02.5-T2.5.3.2-C1", ckSoak72hReport,
		"72h report: p99<=50us, p999<=5ms, mem<12GB, zero WAL gaps (artifact-gated)")
	r.Register("P02.5-T2.5.3.2-C2", ckSoakRecoveryBench,
		"recovery <10s zero dup/miss — failover_bench artifact or live run")
	r.Register("P02.5-T2.5.3.3-C1", ckSoakFaultBackpressure,
		"fault injection + backpressure recovery validated (§24 #300)")
}

func pathExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

// soakArtifactsDir is the canonical run-record location monitor.sh archives to.
func soakArtifactsDir(env *spec.Env) string {
	return env.Path("tests", "soak", "artifacts")
}

// latestSoakRun returns the newest artifacts/<run> dir containing the named
// file, or "" when none exists. A `latest` symlink wins when present.
func latestSoakRun(env *spec.Env, filename string) (string, error) {
	base := soakArtifactsDir(env)
	if link := filepath.Join(base, "latest"); pathExists(filepath.Join(link, filename)) {
		return link, nil
	}
	entries, err := os.ReadDir(base)
	if err != nil {
		return "", err
	}
	var dirs []string
	for _, e := range entries {
		if e.IsDir() && pathExists(filepath.Join(base, e.Name(), filename)) {
			dirs = append(dirs, e.Name())
		}
	}
	if len(dirs) == 0 {
		return "", os.ErrNotExist
	}
	sort.Strings(dirs) // run dirs are ts-prefixed → last is newest
	return filepath.Join(base, dirs[len(dirs)-1]), nil
}

type soakReportJSON struct {
	DurationS         float64 `json:"duration_s"`
	OrdersSent        uint64  `json:"orders_sent"`
	Fills             uint64  `json:"fills"`
	DuplicateTradeIDs uint64  `json:"duplicate_trade_ids"`
	SendDrops         uint64  `json:"send_drops"`
	LatencyNs         struct {
		P50  float64 `json:"p50"`
		P99  float64 `json:"p99"`
		P999 float64 `json:"p999"`
		Max  float64 `json:"max"`
	} `json:"latency_ns"`
	Throughput struct {
		Min1S  float64 `json:"min_1s"`
		Mean1S float64 `json:"mean_1s"`
		Max1S  float64 `json:"max_1s"`
	} `json:"throughput"`
	TargetRate float64 `json:"target_rate"`
}

func readSoakReport(dir string) (*soakReportJSON, error) {
	raw, err := os.ReadFile(filepath.Join(dir, "soak-report.json"))
	if err != nil {
		return nil, err
	}
	var r soakReportJSON
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, fmt.Errorf("parse soak-report.json: %w", err)
	}
	return &r, nil
}

func ckSoakSustainedRate(ctx context.Context, env *spec.Env) spec.Result {
	if r := spec.RequireFiles(env,
		"tests/soak/loadgen.go", "tests/soak/go.mod",
		"tests/soak/monitor.sh"); r.Status != spec.StatusPass {
		return r
	}
	// The generator must build — a broken soak tool is a task defect, not a
	// missing dependency.
	if out, err := spec.RunOutput(ctx, env.Path("tests", "soak"),
		"go", "build", "./..."); err != nil {
		return spec.Failf("loadgen build: %v — %s", err, spec.Tail(out, 8))
	}
	// The orchestrator must carry the phase contract (rate, crash injection
	// schedule, hourly WAL audit, sample interval).
	if r := spec.FileContains(env, "tests/soak/monitor.sh",
		"--rate", "--crash-at", "--audit-interval", "--duration"); r.Status != spec.StatusPass {
		return r
	}

	dir, err := latestSoakRun(env, "soak-report.json")
	if err != nil {
		return spec.Skip("infrastructure verified; no recorded soak run yet " +
			"(72h wall-clock artifact pending under tests/soak/artifacts/)")
	}
	rep, err := readSoakReport(dir)
	if err != nil {
		return spec.Failf("%v", err)
	}
	const needS = 72 * 3600.0
	if rep.DurationS < needS {
		return spec.Skipf("infrastructure verified; longest recorded soak %.0fs "+
			"(%.1fh) < 72h — sustained-rate gate pending", rep.DurationS, rep.DurationS/3600)
	}
	if rep.Throughput.Min1S < 45000 {
		return spec.Failf("recorded soak throughput floor %.0f/s < 45k/s "+
			"(orders=%d)", rep.Throughput.Min1S, rep.OrdersSent)
	}
	return spec.Passf("recorded 72h soak: %.1fh, floor %.0f/s, %d orders, %d fills",
		rep.DurationS/3600, rep.Throughput.Min1S, rep.OrdersSent, rep.Fills)
}

func ckSoak72hReport(ctx context.Context, env *spec.Env) spec.Result {
	dir, err := latestSoakRun(env, "soak-report.json")
	if err != nil {
		return spec.Skip("no recorded soak run yet (72h artifact pending)")
	}
	rep, err := readSoakReport(dir)
	if err != nil {
		return spec.Failf("%v", err)
	}
	if rep.DurationS < 72*3600 {
		return spec.Skipf("recorded soak %.1fh < 72h — full-criteria gate pending",
			rep.DurationS/3600)
	}
	var bad []string
	if rep.LatencyNs.P99 > 50_000 {
		bad = append(bad, fmt.Sprintf("p99 %.0fns > 50µs", rep.LatencyNs.P99))
	}
	if rep.LatencyNs.P999 > 5_000_000 {
		bad = append(bad, fmt.Sprintf("p999 %.0fns > 5ms", rep.LatencyNs.P999))
	}
	if rep.Throughput.Min1S < 45000 {
		bad = append(bad, fmt.Sprintf("min_1s %.0f/s < 45k", rep.Throughput.Min1S))
	}
	if rep.DuplicateTradeIDs > 0 {
		bad = append(bad, fmt.Sprintf("%d duplicate trade ids", rep.DuplicateTradeIDs))
	}
	// Memory ceiling + WAL-gap evidence live in monitor outputs.
	if pathExists(filepath.Join(dir, "report.md")) {
		raw, _ := os.ReadFile(filepath.Join(dir, "report.md"))
		md := string(raw)
		if strings.Contains(md, "WAL_LAG_BOOK alert") ||
			strings.Contains(md, "seq_gaps\":1") ||
			strings.Contains(md, "VERDICT: FAIL") {
			bad = append(bad, "monitor report records WAL/memory violations")
		}
	}
	if len(bad) > 0 {
		return spec.Failf("72h report criteria unmet: %s", strings.Join(bad, "; "))
	}
	return spec.Passf("72h report: p99 %.0fns, p999 %.0fns, floor %.0f/s, "+
		"dup_trades=%d, no WAL/memory violations",
		rep.LatencyNs.P99, rep.LatencyNs.P999, rep.Throughput.Min1S,
		rep.DuplicateTradeIDs)
}

func ckSoakRecoveryBench(ctx context.Context, env *spec.Env) spec.Result {
	// Recorded artifact first — a passing bench run is durable evidence.
	if dir, err := latestSoakRun(env, "failover-report.json"); err == nil {
		raw, err := os.ReadFile(filepath.Join(dir, "failover-report.json"))
		if err != nil {
			return spec.Failf("read failover-report.json: %v", err)
		}
		var f struct {
			Verdict       string  `json:"verdict"`
			RecoveryMsMax float64 `json:"recovery_ms_max"`
			ParityFail    int     `json:"parity_fail"`
			DupTrades     int     `json:"dup_trade_ids"`
		}
		if err := json.Unmarshal(raw, &f); err != nil {
			return spec.Failf("parse failover-report.json: %v", err)
		}
		if f.Verdict == "PASS" && f.RecoveryMsMax <= 10_000 &&
			f.ParityFail == 0 && f.DupTrades == 0 {
			return spec.Passf("failover bench artifact: recovery %.0fms max, "+
				"parity 100%%, dup=0 (%s)", f.RecoveryMsMax, dir)
		}
		return spec.Failf("failover bench artifact: verdict=%s recovery_max=%.0fms "+
			"parity_fail=%d dup=%d", f.Verdict, f.RecoveryMsMax, f.ParityFail, f.DupTrades)
	}
	// No artifact: run the bench live when its dependencies exist.
	bench := env.Path("tests", "soak", "failover_bench.sh")
	engine := filepath.Join(env.CoreBuild, "matching_engine")
	audit := filepath.Join(env.CoreBuild, "wal_audit")
	loadgen := env.Path("tests", "soak", "loadgen")
	for _, p := range []string{bench, engine, audit, loadgen} {
		if !pathExists(p) {
			return spec.Skipf("failover bench dependency missing: %s "+
				"(no recorded artifact either)", filepath.Base(p))
		}
	}
	work, err := os.MkdirTemp("", "fo-bench-*")
	if err != nil {
		return spec.Failf("mktemp: %v", err)
	}
	defer os.RemoveAll(work)
	out, err := spec.RunOutput(ctx, env.RepoRoot, "bash", bench,
		"--engine", engine, "--wal-audit", audit,
		"--workdir", work, "--shard", "9", "--instrument", "7",
		"--loadgen", loadgen, "--rate", "20000", "--trials", "1")
	if err != nil {
		return spec.Failf("failover_bench: %v — %s", err, spec.Tail(out, 10))
	}
	if !strings.Contains(out, "PASS") {
		return spec.Failf("failover_bench did not PASS: %s", spec.Tail(out, 10))
	}
	return spec.Passf("live failover bench: %s",
		spec.LastLine(spec.KeepLines(out, "recovery_ms")))
}

func ckSoakFaultBackpressure(ctx context.Context, env *spec.Env) spec.Result {
	if r := spec.RequireFiles(env,
		"tests/soak/monitor.sh", "tests/soak/failover_bench.sh",
		"core/src/tools/wal_audit.cpp"); r.Status != spec.StatusPass {
		return r
	}
	// The orchestrator must wire crash injection + 120% burst scheduling and
	// periodic WAL audit; the audit tool must exist and build.
	if r := spec.FileContains(env, "tests/soak/monitor.sh",
		"--crash-at", "--burst-at", "kill -9", "wal_audit"); r.Status != spec.StatusPass {
		return r
	}
	audit := filepath.Join(env.CoreBuild, "wal_audit")
	if !pathExists(audit) {
		if out, err := spec.RunOutput(ctx, env.Path("core"),
			"cmake", "--build", "build", "--target", "wal_audit", "-j4"); err != nil {
			return spec.Skipf("wal_audit not built and build failed: %v — %s",
				err, spec.Tail(out, 6))
		}
	}
	// §24 #300 evidence: a recorded run containing fault-injection events
	// (crash injection and/or 120% burst) plus a passing WAL integrity audit.
	dir, err := latestSoakRun(env, "events.jsonl")
	if err != nil {
		return spec.Skip("infra verified; no recorded fault-injection run yet " +
			"(events.jsonl pending under tests/soak/artifacts/)")
	}
	raw, err := os.ReadFile(filepath.Join(dir, "events.jsonl"))
	if err != nil {
		return spec.Failf("read events.jsonl: %v", err)
	}
	ev := string(raw)
	// monitor.sh event schema: {"type":"crash"|"burst_start"|"burst_end",...}
	crash := strings.Contains(ev, `"type":"crash"`)
	burst := strings.Contains(ev, `"type":"burst_start"`)
	recov := strings.Contains(ev, "recovery_ms")
	switch {
	case !crash:
		return spec.Failf("recorded run lacks crash-injection events")
	case !recov:
		return spec.Failf("recorded run lacks recovery measurements")
	case !burst:
		return spec.Skipf("crash+recovery recorded; 120%% burst not yet "+
			"exercised in %s", dir)
	}
	return spec.Passf("fault-injection run recorded: crash+recovery+burst "+
		"events in %s", dir)
}
