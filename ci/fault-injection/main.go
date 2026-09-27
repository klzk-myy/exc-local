// Command faultinject is the Phase-01.5 Task 1.5.3.5 negative test suite &
// fault injection harness. It runs the fault corpus against real Phase-01
// binaries/libraries and asserts the spec §2.7 L0-L3 fail-closed
// invariants: every injected fault must produce the expected coded error
// or deterministic halt — never an unhandled panic or partial state
// mutation.
//
// Usage:
//
//	faultinject -walverify bin/walverify -exchange bin/exchange \
//	    -pg-dsn postgres://... -out fault-report.json [-only name,...]
//
// Exit code: 0 when every enabled scenario passes; 1 otherwise (CI gate,
// spec §24 #298).
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// scenario is one fault-injection case. Tiers list the spec §2.7 error
// severities exercised — the suite must cover L0-L3 across scenarios.
type scenario struct {
	Name  string
	Tiers []string
	Desc  string
	Run   func(ctx context.Context, env *env) *Checks
}

type env struct {
	walverify string // path to compiled cpp/walverify
	exchange  string // path to built services/cmd/exchange binary
	pgDSN     string
	workRoot  string // per-run scratch dir
}

// workDir returns (creating) a per-scenario scratch directory.
func (e *env) workDir(name string) string {
	d := filepath.Join(e.workRoot, name)
	_ = os.MkdirAll(d, 0o755)
	return d
}

// wv invokes the walverify tool and parses its single-line JSON output.
func (e *env) wv(ctx context.Context, args ...string) (map[string]any, cmdResult) {
	res := runExec(ctx, "", append([]string{e.walverify}, args...)...)
	return lastJSONLine(res.Stdout), res
}

var registry = []scenario{
	{"wal_corrupt", []string{"L0"},
		"corrupt WAL headers/CRC/torn tail fail closed: reader reports corruption, " +
			"recover truncates at last valid record, BadHeader halts without mutation",
		scenarioWalCorrupt},
	{"wal_crash_recovery", []string{"L0"},
		"kill -9 mid-append: standby replays to last valid seq, zero data loss, " +
			"recovery report emitted",
		scenarioWalCrashRecovery},
	{"clock_jump", []string{"L0"},
		"PTP clock offset >100us / unsynced / probe error -> TIME_SYNC_LOSS_HALT (L0, 503)",
		scenarioClockJump},
	{"audit_chain_tamper", []string{"L0"},
		"audit hash chain tamper -> verify-audit detects + exit 2 (AUDIT_HASH_CORRUPTION)",
		scenarioAuditTamper},
	{"shm_ring_faults", []string{"L1"},
		"invalid Aeron/shm buffer addresses: bad capacity, truncated image, bad magic, " +
			"corrupt slot len, dead producer -> clean errors, no panic",
		scenarioShmFaults},
	{"aeron_unreachable", []string{"L1"},
		"Aeron media driver unreachable / bad CnC dir -> coded clean error, no panic",
		scenarioAeronUnreachable},
	{"pg_serialization_conflict", []string{"L2"},
		"two SERIALIZABLE txns contend -> SQLSTATE 40001 retriable; zero partial mutation; " +
			"audit AppendAuto retry budget -> TRANSACTION_CONFLICT_RETRY_EXHAUSTED",
		scenarioPGConflict},
	{"arithmetic_overflow", []string{"L2"},
		"safe_math add/sub/mul/neg overflow -> detected, out unmodified, " +
			"ARITHMETIC_OVERFLOW_DETECTED (L2, 400)",
		scenarioOverflow},
	{"malformed_frames", []string{"L3"},
		"garbage/truncated FlatBuffers Event -> typed coded rejection, panic never escapes",
		scenarioMalformedFrames},
	{"auth_edge_rejects", []string{"L3"},
		"invalid HMAC signature 401 INVALID_SIGNATURE, replay 401 REPLAY_ATTACK_DETECTED, " +
			"stale timestamp 401 TIMESTAMP_OUT_OF_WINDOW, scope 403 FORBIDDEN — RFC 7807 envelopes",
		scenarioAuthEdge},
}

type scenarioReport struct {
	Name       string   `json:"name"`
	Tiers      []string `json:"tiers"`
	Desc       string   `json:"desc"`
	Status     string   `json:"status"` // PASS | FAIL | ERROR | SKIP
	Checks     []Check  `json:"checks"`
	DurationMs int64    `json:"duration_ms"`
}

type report struct {
	Task        string           `json:"task"`
	Spec        string           `json:"spec"`
	StartedAt   string           `json:"started_at"`
	DurationMs  int64            `json:"duration_ms"`
	Tools       map[string]any   `json:"tools"`
	Scenarios   []scenarioReport `json:"scenarios"`
	TiersHit    map[string]int   `json:"tiers_covered"`
	Totals      map[string]int   `json:"totals"`
}

func main() {
	var (
		walverify = flag.String("walverify", "bin/walverify", "path to walverify binary")
		exchange  = flag.String("exchange", "bin/exchange", "path to exchange CLI binary")
		pgDSN     = flag.String("pg-dsn", os.Getenv("EXC_POSTGRES_DSN"), "postgres DSN (default: dev compose)")
		outPath   = flag.String("out", "fault-report.json", "JSON report output path")
		only      = flag.String("only", "", "comma-separated scenario names to run (default: all)")
		keep      = flag.Bool("keep-work", false, "keep per-scenario scratch dirs")
		timeout   = flag.Duration("scenario-timeout", 55*time.Second, "per-scenario hard timeout (<60s contract)")
	)
	flag.Parse()
	if *pgDSN == "" {
		*pgDSN = "postgres://exchange:exchange_dev@127.0.0.1:5433/exchange?sslmode=disable"
	}

	abs := func(p string) string {
		if a, err := filepath.Abs(p); err == nil {
			return a
		}
		return p
	}
	e := &env{
		walverify: abs(*walverify),
		exchange:  abs(*exchange),
		pgDSN:     *pgDSN,
	}
	work, err := os.MkdirTemp("", "faultinject-*")
	if err != nil {
		fmt.Fprintln(os.Stderr, "faultinject: mktemp:", err)
		os.Exit(2)
	}
	e.workRoot = work
	if !*keep {
		defer os.RemoveAll(work)
	} else {
		fmt.Println("work dir:", work)
	}

	enabled := map[string]bool{}
	for _, n := range strings.Split(*only, ",") {
		if n = strings.TrimSpace(n); n != "" {
			enabled[n] = true
		}
	}

	tools := map[string]any{}
	for name, p := range map[string]string{"walverify": e.walverify, "exchange": e.exchange} {
		st, err := os.Stat(p)
		tools[name] = map[string]any{"path": p, "present": err == nil && !st.IsDir()}
	}

	rep := report{
		Task:      "1.5.3.5",
		Spec:      "spec §2.7, §22.7, §24 #298 — Strict Fail-Closed Zero-Loss Pessimism",
		StartedAt: time.Now().UTC().Format(time.RFC3339),
		Tools:     tools,
		TiersHit:  map[string]int{},
	}
	start := time.Now()

	for _, sc := range registry {
		sr := scenarioReport{Name: sc.Name, Tiers: sc.Tiers, Desc: sc.Desc}
		if len(enabled) > 0 && !enabled[sc.Name] {
			sr.Status = "SKIP"
			rep.Scenarios = append(rep.Scenarios, sr)
			continue
		}
		s0 := time.Now()
		ctx, cancel := context.WithTimeout(context.Background(), *timeout)
		// Run in a goroutine so a hard-stuck call (e.g. a blocking cgo
		// transport call that ignores ctx) is reported as TIMEOUT rather
		// than hanging the whole CI job past its budget.
		type outcome struct {
			checks   *Checks
			panicked any
		}
		done := make(chan outcome, 1)
		go func() {
			var o outcome
			defer func() { o.panicked = recover(); done <- o }()
			o.checks = sc.Run(ctx, e)
		}()
		var o outcome
		timedOut := false
		select {
		case o = <-done:
		case <-ctx.Done():
			timedOut = true
		}
		cancel()
		sr.DurationMs = time.Since(s0).Milliseconds()
		switch {
		case timedOut:
			sr.Status = "FAIL"
			sr.Checks = append(sr.Checks, Check{
				Name:   "scenario_timeout",
				Pass:   false,
				Detail: fmt.Sprintf("exceeded %s budget", *timeout),
			})
		case o.panicked != nil:
			sr.Status = "ERROR"
			sr.Checks = append(sr.Checks, Check{
				Name:   "scenario_panicked",
				Pass:   false,
				Detail: fmt.Sprintf("unhandled panic escaped scenario: %v", o.panicked),
			})
		default:
			sr.Checks = o.checks.list
			if o.checks.allPass() {
				sr.Status = "PASS"
				for _, t := range sc.Tiers {
					rep.TiersHit[t]++
				}
			} else {
				sr.Status = "FAIL"
			}
		}
		rep.Scenarios = append(rep.Scenarios, sr)
		mark := sr.Status
		if mark == "PASS" {
			mark = "\x1b[32mPASS\x1b[0m"
		} else if mark == "SKIP" {
			mark = "\x1b[33mSKIP\x1b[0m"
		} else {
			mark = "\x1b[31m" + mark + "\x1b[0m"
		}
		fmt.Printf("%-28s %s  (%d checks, %dms)\n", sr.Name, mark, len(sr.Checks), sr.DurationMs)
		for _, ch := range sr.Checks {
			if !ch.Pass {
				fmt.Printf("    FAIL %s — %s\n", ch.Name, ch.Detail)
			}
		}
	}
	rep.DurationMs = time.Since(start).Milliseconds()

	passed, failed, errored, skipped, checks, checksFailed := 0, 0, 0, 0, 0, 0
	for _, sr := range rep.Scenarios {
		switch sr.Status {
		case "PASS":
			passed++
		case "FAIL":
			failed++
		case "ERROR":
			errored++
		case "SKIP":
			skipped++
		}
		checks += len(sr.Checks)
		for _, ch := range sr.Checks {
			if !ch.Pass {
				checksFailed++
			}
		}
	}
	rep.Totals = map[string]int{
		"scenarios":      len(rep.Scenarios) - skipped,
		"passed":         passed,
		"failed":         failed,
		"errored":        errored,
		"skipped":        skipped,
		"checks":         checks,
		"checks_failed":  checksFailed,
	}

	// Stable key order for the tiers map in output.
	tiers := make([]string, 0, len(rep.TiersHit))
	for t := range rep.TiersHit {
		tiers = append(tiers, t)
	}
	sort.Strings(tiers)

	data, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		fmt.Fprintln(os.Stderr, "faultinject: marshal report:", err)
		os.Exit(2)
	}
	if err := os.WriteFile(*outPath, append(data, '\n'), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "faultinject: write report:", err)
		os.Exit(2)
	}

	fmt.Printf("\nscenarios: %d run, %d pass, %d fail, %d error, %d skip; checks: %d, failed: %d\n",
		len(rep.Scenarios)-skipped, passed, failed, errored, skipped, checks, checksFailed)
	fmt.Printf("tiers covered: %s\n", strings.Join(tiers, " "))
	fmt.Println("report:", *outPath)

	if failed > 0 || errored > 0 || checksFailed > 0 {
		os.Exit(1)
	}
}
