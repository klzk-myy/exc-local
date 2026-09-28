package spec

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"
	goredis "github.com/redis/go-redis/v9"
)

// ---------------------------------------------------------------------------
// Subprocess helpers — used to reach into code this module cannot import
// (services/internal/** is internal to module `exchange`; the C++ suite is
// exercised through ctest).
// ---------------------------------------------------------------------------

// RunCTest runs `ctest --test-dir <env.CoreBuild> -R <regex> --output-on-failure`.
// Returns Skip when the C++ build tree is absent (missing-dependency edge
// case — core not built on this runner), Fail on non-zero exit.
func RunCTest(ctx context.Context, env *Env, regex string) Result {
	ctestFile := filepath.Join(env.CoreBuild, "CTestTestfile.cmake")
	if _, err := os.Stat(ctestFile); err != nil {
		return Skipf("core build tree absent (%s) — build core first (cmake --build core/build)", env.CoreBuild)
	}
	if _, err := exec.LookPath("ctest"); err != nil {
		return Skip("ctest not on PATH")
	}
	c := exec.CommandContext(ctx, "ctest", "--test-dir", env.CoreBuild, "-R", regex,
		"--output-on-failure", "--no-tests=error")
	out, err := c.CombinedOutput()
	if err != nil {
		return Failf("ctest -R %s failed: %v\n%s", regex, err, tail(string(out), 30))
	}
	return Passf("ctest -R %s: %s", regex, lastLine(string(out)))
}

// RunGTest runs a single gtest binary from the core build tree with a
// --gtest_filter pattern. More precise than ctest -R (which selects whole
// binaries) — use it to pin checkpoint evidence to specific gtest suites.
// A filter matching zero tests is a harness defect and FAILS loudly.
func RunGTest(ctx context.Context, env *Env, binary, filter string) Result {
	path := filepath.Join(env.CoreBuild, binary)
	if _, err := os.Stat(path); err != nil {
		return Skipf("core test binary %s absent — build core first", binary)
	}
	c := exec.CommandContext(ctx, path, "--gtest_filter="+filter, "--gtest_color=no")
	out, err := c.CombinedOutput()
	s := string(out)
	if err != nil {
		return Failf("%s --gtest_filter=%s failed: %v\n%s", binary, filter, err, tail(s, 40))
	}
	// gtest prints "0 tests from 0 test suites ran" when the filter matched
	// nothing — a typo'd filter must be loud, not green.
	if strings.Contains(s, "0 tests from 0 test suites ran") {
		return Failf("%s --gtest_filter=%s matched 0 tests (check filter)", binary, filter)
	}
	return Passf("%s %s: %s", binary, filter, lastLine(s))
}

// RunGoTest runs `go test <pkg> -run <regex> [-count=1]` inside the services
// module. Integration-gated tests receive the live-endpoint env vars so they
// exercise real infrastructure when present (they self-skip otherwise — the
// skip still counts as PASS at this level since the package's own skip
// semantics apply; dependency probes in checks/ report Skip explicitly).
func RunGoTest(ctx context.Context, env *Env, pkg, runRegex string, extra ...string) Result {
	if _, err := exec.LookPath("go"); err != nil {
		return Skip("go toolchain not on PATH")
	}
	args := append([]string{"test", pkg, "-count=1"}, extra...)
	if runRegex != "" {
		args = append(args, "-run", runRegex)
	}
	c := exec.CommandContext(ctx, "go", args...)
	c.Dir = env.ServicesDir()
	c.Env = append(os.Environ(),
		"EXC_REDIS_TEST=1", "EXC_NATS_TEST=1", "EXC_SENTINEL_TEST=1",
		"EXC_REDIS_TEST_ADDR="+env.RedisAddr,
		"EXC_NATS_URLS="+env.NatsURLs,
		"EXC_SENTINEL_ADDRS="+env.Sentinels,
		"EXC_TEST_DSN="+env.PostgresDSN,
	)
	// PG-gated tests self-skip unless EXC_PG_TEST=1 — enable live verification
	// only when the operator explicitly pointed the harness at a DSN (an
	// explicitly-provided unreachable DSN failing is correct; silently
	// skipping it would be a false pass).
	if os.Getenv("EXC_TEST_DSN") != "" {
		c.Env = append(c.Env, "EXC_PG_TEST=1",
			"EXC_PG_DSN="+env.PostgresDSN)
	}
	out, err := c.CombinedOutput()
	s := string(out)
	if err != nil {
		return Failf("go test %s -run %s failed: %v\n%s", pkg, runRegex, err, tail(s, 40))
	}
	// Honest reporting: a bad -run regex matches nothing and `go test`
	// still exits 0 — treat "no tests to run" as a harness defect (fail),
	// and all-skipped packages as dependency-skip (not pass).
	if strings.Contains(s, "no tests to run") && !strings.Contains(s, "--- PASS") && !strings.Contains(s, "--- SKIP") {
		return Failf("go test %s -run %s matched no tests (check regex)", pkg, runRegex)
	}
	if strings.Contains(s, "--- SKIP") && !strings.Contains(s, "--- PASS") {
		return Skipf("go test %s -run %s: all tests skipped (dependency unavailable)", pkg, runRegex)
	}
	return Passf("go test %s -run %s passed", pkg, runRegex)
}

// GoBuildServices verifies `go build ./cmd/...` succeeds inside services —
// every cmd/ binary must compile.
func GoBuildServices(ctx context.Context, env *Env) Result {
	c := exec.CommandContext(ctx, "go", "build", "./cmd/...")
	c.Dir = env.ServicesDir()
	out, err := c.CombinedOutput()
	if err != nil {
		return Failf("go build ./cmd/... failed: %v\n%s", err, tail(string(out), 40))
	}
	return Pass("all services/cmd binaries compile")
}

// ---------------------------------------------------------------------------
// Structural helpers
// ---------------------------------------------------------------------------

// FileContains asserts every pattern (regex) occurs in rel (root-relative).
func FileContains(env *Env, rel string, patterns ...string) Result {
	path := env.Path(rel)
	b, err := os.ReadFile(path)
	if err != nil {
		return Failf("%s unreadable: %v", rel, err)
	}
	var missing []string
	for _, p := range patterns {
		if !regexp.MustCompile(p).Match(b) {
			missing = append(missing, p)
		}
	}
	if len(missing) > 0 {
		return Failf("%s missing patterns %v", rel, missing)
	}
	return Passf("%s contains %d required patterns", rel, len(patterns))
}

// FileLacks asserts NO pattern matches inside rel — guards "never X" rules.
func FileLacks(env *Env, rel string, patterns ...string) Result {
	path := env.Path(rel)
	b, err := os.ReadFile(path)
	if err != nil {
		return Failf("%s unreadable: %v", rel, err)
	}
	var found []string
	for _, p := range patterns {
		if regexp.MustCompile(p).Match(b) {
			found = append(found, p)
		}
	}
	if len(found) > 0 {
		return Failf("%s contains forbidden patterns %v", rel, found)
	}
	return Passf("%s clean of %d forbidden patterns", rel, len(patterns))
}

// RequireFiles asserts each root-relative file exists; returns their list.
func RequireFiles(env *Env, rels ...string) Result {
	var missing []string
	for _, r := range rels {
		if !env.FileExists(r) {
			missing = append(missing, r)
		}
	}
	if len(missing) > 0 {
		return Failf("missing files: %v", missing)
	}
	return Passf("all %d files present", len(rels))
}

// GrepCount returns matches of pattern in file (for count assertions).
func GrepCount(env *Env, rel, pattern string) (int, error) {
	b, err := os.ReadFile(env.Path(rel))
	if err != nil {
		return 0, err
	}
	return len(regexp.MustCompile(pattern).FindAll(b, -1)), nil
}

// ---------------------------------------------------------------------------
// Live-dependency helpers (env-gated; return Skip when unreachable)
// ---------------------------------------------------------------------------

// PgPool opens a pgx pool to the dev Postgres (EXC_TEST_DSN). Second return
// is "" on success or the failure reason (caller returns Skip(reason)).
func PgPool(ctx context.Context, env *Env) (*pgxpool.Pool, string) {
	cctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	pool, err := pgxpool.New(cctx, env.PostgresDSN)
	if err != nil {
		return nil, fmt.Sprintf("postgres parse/connect: %v", err)
	}
	if err := pool.Ping(cctx); err != nil {
		pool.Close()
		return nil, fmt.Sprintf("postgres unreachable at %s: %v", env.PostgresDSN, err)
	}
	return pool, ""
}

// Redis opens a go-redis client to EXC_REDIS_TEST_ADDR and pings it.
func Redis(ctx context.Context, env *Env) (*goredis.Client, string) {
	c := goredis.NewClient(&goredis.Options{Addr: env.RedisAddr})
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := c.Ping(cctx).Err(); err != nil {
		c.Close()
		return nil, fmt.Sprintf("redis unreachable at %s: %v", env.RedisAddr, err)
	}
	return c, ""
}

// Nats connects to EXC_NATS_URLS.
func Nats(ctx context.Context, env *Env) (*nats.Conn, string) {
	nc, err := nats.Connect(env.NatsURLs,
		nats.Timeout(5*time.Second),
		nats.Name("testspec-validator"))
	if err != nil {
		return nil, fmt.Sprintf("nats unreachable at %s: %v", env.NatsURLs, err)
	}
	return nc, ""
}

// ---------------------------------------------------------------------------
// misc
// ---------------------------------------------------------------------------

func tail(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) == 0 {
		return ""
	}
	return strings.TrimSpace(lines[len(lines)-1])
}

// RunOutput executes a subprocess in dir and returns combined output.
// Exported so checkpoint CheckFuncs can drive repo tooling (validator
// subcommands, fault-injection suite) without re-implementing exec plumbing.
func RunOutput(ctx context.Context, dir, name string, args ...string) (string, error) {
	c := exec.CommandContext(ctx, name, args...)
	c.Dir = dir
	out, err := c.CombinedOutput()
	return string(out), err
}

// Tail returns the last n lines of s.
func Tail(s string, n int) string { return tail(s, n) }

// LastLine returns the final non-empty content line of s.
func LastLine(s string) string { return lastLine(s) }

// KeepLines returns the first line of s containing needle (empty if none).
func KeepLines(s, needle string) string {
	for _, l := range strings.Split(s, "\n") {
		if strings.Contains(l, needle) {
			return strings.TrimSpace(l)
		}
	}
	return ""
}
