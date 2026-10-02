// Subprocess delegation — the mechanism this module uses to reach code
// it cannot import: `exchange/internal/**` is internal to module
// `exchange` (Go visibility rules), so internal-package integration legs
// run as `go test -run` subprocesses inside services/, and the C++ core
// is exercised through ctest / gtest binaries in core/build. Delegated
// legs run the services' OWN tests — real assertions over real infra —
// and the subprocess contract treats "matched nothing" and
// "everything skipped" as defects/gates, never silent passes.
package itest

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Outcome is a delegated leg's verdict.
type Outcome struct {
	OK      bool   // assertions held
	Skipped bool   // dependency-gated skip inside the subprocess
	Output  string // trimmed tail for the report
}

// RunGoTest runs `go test <pkg> -run <regex> -count=1` inside services/.
// Live-endpoint env vars are forwarded so gated tests exercise the
// scratch infrastructure the operator pointed us at.
func (e *Env) RunGoTest(ctx context.Context, pkg, runRegex string, extra ...string) Outcome {
	args := append([]string{"test", pkg, "-count=1", "-timeout", "4m"}, extra...)
	if runRegex != "" {
		args = append(args, "-run", runRegex)
	}
	c := exec.CommandContext(ctx, "go", args...)
	c.Dir = e.ServicesDir
	c.Env = append(os.Environ(),
		"EXC_PG_TEST="+os.Getenv("EXC_PG_TEST"),
		"EXC_REDIS_TEST="+os.Getenv("EXC_REDIS_TEST"),
		"EXC_NATS_TEST=1",
		"EXC_TEST_DSN="+e.PostgresDSN,
		"EXC_PG_DSN="+e.PostgresDSN,
		"EXC_REDIS_TEST_ADDR="+e.RedisAddr,
		"EXC_NATS_URLS="+strings.Join(e.NatsURLs, ","),
		// Sentinel-quorum legs: gated suites self-skip without these;
		// host_probe resolves container-announced IPs back to loopback.
		"EXC_SENTINEL_TEST="+os.Getenv("EXC_SENTINEL_TEST"),
		"EXC_SENTINEL_ADDRS="+os.Getenv("EXC_SENTINEL_ADDRS"),
		"EXC_SENTINEL_RESOLVE_MODE="+os.Getenv("EXC_SENTINEL_RESOLVE_MODE"),
	)
	out, err := c.CombinedOutput()
	s := string(out)
	switch {
	case err != nil:
		return Outcome{Output: fmt.Sprintf("go test %s -run %s: %v\n%s", pkg, runRegex, err, tail(s, 40))}
	case strings.Contains(s, "no tests to run") && !strings.Contains(s, "--- PASS") && !strings.Contains(s, "--- SKIP"):
		return Outcome{Output: fmt.Sprintf("go test %s -run %s matched no tests (check regex)", pkg, runRegex)}
	case strings.Contains(s, "--- SKIP") && !strings.Contains(s, "--- PASS"):
		return Outcome{Skipped: true, Output: fmt.Sprintf("go test %s -run %s: all skipped (dependency gate)", pkg, runRegex)}
	}
	return Outcome{OK: true, Output: fmt.Sprintf("go test %s -run %s: PASS", pkg, runRegex)}
}

// RunCTest runs `ctest --test-dir <core build> -R <regex>`.
func (e *Env) RunCTest(ctx context.Context, regex string) Outcome {
	if _, err := exec.LookPath("ctest"); err != nil {
		return Outcome{Skipped: true, Output: "ctest not on PATH"}
	}
	c := exec.CommandContext(ctx, "ctest", "--test-dir", e.CoreBuild, "-R", regex,
		"--output-on-failure", "--no-tests=error", "--timeout", "300")
	out, err := c.CombinedOutput()
	s := string(out)
	if err != nil {
		return Outcome{Output: fmt.Sprintf("ctest -R %s failed: %v\n%s", regex, err, tail(s, 30))}
	}
	return Outcome{OK: true, Output: fmt.Sprintf("ctest -R %s: %s", regex, lastLine(s))}
}

// RunGTest runs one gtest binary with --gtest_filter; a filter matching
// zero tests is a harness defect, not a pass.
func (e *Env) RunGTest(ctx context.Context, binary, filter string) Outcome {
	path := filepath.Join(e.CoreBuild, binary)
	if _, err := os.Stat(path); err != nil {
		return Outcome{Skipped: true, Output: "core test binary " + binary + " absent — build core first"}
	}
	c := exec.CommandContext(ctx, path, "--gtest_filter="+filter, "--gtest_color=no")
	out, err := c.CombinedOutput()
	s := string(out)
	if err != nil {
		return Outcome{Output: fmt.Sprintf("%s --gtest_filter=%s failed: %v\n%s", binary, filter, err, tail(s, 40))}
	}
	if strings.Contains(s, "0 tests from 0 test suites ran") {
		return Outcome{Output: fmt.Sprintf("%s --gtest_filter=%s matched 0 tests (check filter)", binary, filter)}
	}
	return Outcome{OK: true, Output: fmt.Sprintf("%s %s: %s", binary, filter, lastLine(s))}
}

// RunVitest runs one frontend spec file via `npx vitest run <file>`
// inside frontend/. The file path is a filter, not a -t name regex —
// every assertion in the spec executes. A zero-match or all-skipped
// run is a defect, not a pass.
func (e *Env) RunVitest(ctx context.Context, specFile string) Outcome {
	if _, err := os.Stat(filepath.Join(e.FrontendDir, specFile)); err != nil {
		return Outcome{Skipped: true, Output: "frontend spec absent: " + specFile}
	}
	if _, err := os.Stat(filepath.Join(e.FrontendDir, "node_modules", ".bin", "vitest")); err != nil {
		return Outcome{Skipped: true, Output: "frontend deps absent — npm ci in frontend/ first"}
	}
	c := exec.CommandContext(ctx, "npx", "vitest", "run", specFile, "--color=false")
	c.Dir = e.FrontendDir
	out, err := c.CombinedOutput()
	s := string(out)
	switch {
	case err != nil:
		return Outcome{Output: fmt.Sprintf("vitest %s failed: %v\n%s", specFile, err, tail(s, 40))}
	case strings.Contains(s, "No test files found") || strings.Contains(s, "0 failed, 0 passed"):
		return Outcome{Output: fmt.Sprintf("vitest %s matched no tests (check spec path)", specFile)}
	}
	return Outcome{OK: true, Output: fmt.Sprintf("vitest %s: %s", specFile, lastLine(s))}
}

// RunBin runs an arbitrary binary with args, returning combined output.
func (e *Env) RunBin(ctx context.Context, dir, name string, args ...string) (string, error) {
	return e.RunBinEnv(ctx, dir, nil, name, args...)
}

// RunBinEnv is RunBin with extra environment entries (KEY=value).
func (e *Env) RunBinEnv(ctx context.Context, dir string, env []string, name string, args ...string) (string, error) {
	c := exec.CommandContext(ctx, name, args...)
	if dir != "" {
		c.Dir = dir
	}
	c.Env = append(os.Environ(), env...)
	out, err := c.CombinedOutput()
	return string(out), err
}

// tail / lastLine mirror the spec-harness helpers.
func tail(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) == 0 {
		return ""
	}
	return strings.TrimSpace(lines[len(lines)-1])
}

// WaitFor polls cond until true or timeout — used for async propagation
// (engine ack → PG row, fill → read model).
func WaitFor(timeout time.Duration, step time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(step)
	}
	return cond()
}
