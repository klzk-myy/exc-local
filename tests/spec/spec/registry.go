package spec

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// Status is the outcome of running (or evaluating) one checkpoint.
//
// CI failure policy (documented in tests/spec/README.md):
//   - FAIL the run: StatusFail, StatusTimeout, StatusError, StatusMissing,
//     StatusVanished (for checkpoints recorded checked in the corpus),
//   - never fail: StatusPass, StatusSkip (missing dependency), StatusPending
//     (doc checkbox still `[ ]` and no implementation registered),
//     StatusDropped (checkpoint removed from an unchecked/pending phase).
type Status string

const (
	StatusPass    Status = "pass"    // implemented; ran; assertions held
	StatusFail    Status = "fail"    // implemented; ran; assertion failed
	StatusTimeout Status = "timeout" // implemented; exceeded per-checkpoint timeout
	StatusError   Status = "error"   // implemented; panicked or harness error
	StatusSkip    Status = "skip"    // implemented; dependency unavailable (env-gated)
	StatusPending Status = "pending" // `[ ]` in docs / no implementation registered,
	// or an implementation that can only be discharged on deployment-scale
	// infra (e.g. 72h soak) — env-bound, never fails the run
	StatusMissing  Status = "missing"  // `[x]` in docs but NO implementation — FAIL
	StatusVanished Status = "vanished" // in corpus, absent from docs, was checked — FAIL
	StatusDropped  Status = "dropped"  // in corpus, absent from docs, was unchecked — warn only
)

// Fails reports whether this status fails the run (exits non-zero).
func (s Status) Fails() bool {
	switch s {
	case StatusFail, StatusTimeout, StatusError, StatusMissing, StatusVanished:
		return true
	}
	return false
}

// Result is what a CheckFunc returns.
type Result struct {
	Status Status
	Detail string // human-readable evidence (assertion output, versions, counts)
}

// Result constructors.
func Pass(detail string) Result { return Result{Status: StatusPass, Detail: detail} }
func Fail(detail string) Result { return Result{Status: StatusFail, Detail: detail} }
func Skip(reason string) Result { return Result{Status: StatusSkip, Detail: reason} }

// Pending marks a checkpoint env-bound: implemented, but discharging it
// needs deployment-scale infra no CI runner can provide (72h soak window,
// dedicated bench host). Distinct from Skip — which signals a missing
// runtime dependency and fails under --fail-on-skip (broken infra).
func Pending(reason string) Result { return Result{Status: StatusPending, Detail: reason} }

// Passf / Failf / Skipf are printf variants.
func Passf(format string, a ...any) Result { return Pass(fmt.Sprintf(format, a...)) }
func Failf(format string, a ...any) Result { return Fail(fmt.Sprintf(format, a...)) }
func Skipf(format string, a ...any) Result { return Skip(fmt.Sprintf(format, a...)) }
func Pendingf(format string, a ...any) Result {
	return Pending(fmt.Sprintf(format, a...))
}

// CheckFunc verifies one checkpoint. It must be deterministic, safe to run
// in parallel with other shards' work, and return within the configured
// per-checkpoint timeout. Live-infrastructure checks return Skip when their
// dependency is unreachable (see Env for endpoints and env-var overrides).
type CheckFunc func(ctx context.Context, env *Env) Result

// Entry binds a checkpoint ID to its implementation.
type Entry struct {
	ID     string
	Func   CheckFunc
	Golden bool   // part of the golden corpus (tests/golden)
	Notes  string // provenance / coverage notes
}

// Registry maps checkpoint ID → implementation. Golden corpus cases are
// registered under synthetic IDs ("GOLDEN-<nn>-<slug>") and may
// additionally be bound to document checkpoint IDs they satisfy.
type Registry struct {
	entries map[string]*Entry
}

func NewRegistry() *Registry { return &Registry{entries: map[string]*Entry{}} }

// Register binds id to fn. Panics on double-registration of the same ID —
// a duplicated binding is a harness defect, not a checkpoint outcome.
func (r *Registry) Register(id string, fn CheckFunc, notes string) {
	if _, dup := r.entries[id]; dup {
		panic(fmt.Sprintf("spec: duplicate registration for %s", id))
	}
	r.entries[id] = &Entry{ID: id, Func: fn, Notes: notes}
}

// RegisterGolden registers a golden-corpus case under its GOLDEN-* ID.
func (r *Registry) RegisterGolden(id string, fn CheckFunc, notes string) {
	r.Register(id, fn, notes)
	r.entries[id].Golden = true
}

// Bind attaches an already-registered golden func to a document checkpoint
// ID (the func runs for both IDs — once as corpus evidence, once as the
// checkpoint's implementation).
func (r *Registry) Bind(checkpointID, goldenID string) {
	g, ok := r.entries[goldenID]
	if !ok {
		panic(fmt.Sprintf("spec: Bind: unknown golden id %s", goldenID))
	}
	r.Register(checkpointID, g.Func, "bound to "+goldenID)
}

// Lookup returns the entry for id.
func (r *Registry) Lookup(id string) (*Entry, bool) {
	e, ok := r.entries[id]
	return e, ok
}

// IDs returns all registered IDs sorted.
func (r *Registry) IDs() []string {
	out := make([]string, 0, len(r.entries))
	for id := range r.entries {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// GoldenIDs returns registered golden corpus IDs sorted.
func (r *Registry) GoldenIDs() []string {
	var out []string
	for id, e := range r.entries {
		if e.Golden {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}

// Env carries runtime configuration and live-dependency endpoints.
// Defaults match docker-compose.dev.yml; every endpoint is overridable via
// the documented environment variables so CI can repoint at ephemeral
// services without code changes.
type Env struct {
	RepoRoot     string        // repo root (auto-detected or EXC_REPO_ROOT)
	DocsDir      string        // usually <root>/docs (EXC_DOCS_DIR)
	CoreBuild    string        // ctest --test-dir target (EXC_CORE_BUILD)
	PostgresDSN  string        // EXC_TEST_DSN
	RedisAddr    string        // EXC_REDIS_TEST_ADDR
	NatsURLs     string        // EXC_NATS_URLS
	Sentinels    string        // EXC_SENTINEL_ADDRS (csv)
	ClickHouse   string        // EXC_CLICKHOUSE_HTTP
	CheckTimeout time.Duration // per-checkpoint timeout (--timeout)

	// Wired by the validator before running; lets self-referential
	// checkpoints (e.g. "400+ checkpoints extracted") inspect the harness.
	Registry  *Registry // registered implementations
	Live      *Corpus   // corpus extracted from docs this run
	Committed *Corpus   // committed checkpoints.json snapshot (may be nil)
}

// DefaultEnv builds Env with the documented local defaults.
func DefaultEnv() *Env {
	root := FindRepoRoot()
	return &Env{
		RepoRoot:     root,
		DocsDir:      envOr("EXC_DOCS_DIR", filepath.Join(root, "docs")),
		CoreBuild:    envOr("EXC_CORE_BUILD", filepath.Join(root, "core", "build")),
		PostgresDSN:  envOr("EXC_TEST_DSN", "postgres://exchange:exchange_dev@127.0.0.1:5433/exchange?sslmode=disable"),
		RedisAddr:    envOr("EXC_REDIS_TEST_ADDR", "127.0.0.1:16379"),
		NatsURLs:     envOr("EXC_NATS_URLS", "nats://127.0.0.1:4222"),
		Sentinels:    envOr("EXC_SENTINEL_ADDRS", "127.0.0.1:36379,127.0.0.1:36380,127.0.0.1:36381"),
		ClickHouse:   envOr("EXC_CLICKHOUSE_HTTP", "http://127.0.0.1:8123"),
		CheckTimeout: 30 * time.Second,
	}
}

func envOr(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

// FindRepoRoot walks up from the working directory looking for the repo
// markers (README.md + docs/ + services/go.mod — all must be tracked
// files so fresh clones resolve; AGENTS.md is gitignored and cannot be
// a marker). EXC_REPO_ROOT overrides.
func FindRepoRoot() string {
	if v := os.Getenv("EXC_REPO_ROOT"); v != "" {
		abs, err := filepath.Abs(v)
		if err == nil {
			return abs
		}
	}
	dir, err := os.Getwd()
	if err != nil {
		return "."
	}
	for i := 0; i < 12; i++ {
		if fileExists(filepath.Join(dir, "README.md")) &&
			dirExists(filepath.Join(dir, "docs")) &&
			fileExists(filepath.Join(dir, "services", "go.mod")) {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	cwd, _ := os.Getwd()
	return cwd
}

// ServicesDir returns the services module dir.
func (e *Env) ServicesDir() string { return filepath.Join(e.RepoRoot, "services") }

// Path joins root-relative path segments.
func (e *Env) Path(elem ...string) string {
	return filepath.Join(append([]string{e.RepoRoot}, elem...)...)
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

func dirExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

// FileExists is exported for checks.
func (e *Env) FileExists(rel string) bool { return fileExists(e.Path(rel)) }

// DirExists is exported for checks.
func (e *Env) DirExists(rel string) bool { return dirExists(e.Path(rel)) }
