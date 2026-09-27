// Command validator is the spec validation harness CLI (Phase-01.5 Task
// 1.5.3.2). Subcommands:
//
//	validator run      — execute checkpoints for one shard; JSON report
//	validator extract  — re-extract corpus from docs; --write regenerates
//	                     checkpoints/checkpoints.json + stubs.gen.go + PENDING.md
//	validator list     — print the extracted checkpoint table
//	validator stubs    — print checkpoint IDs with no implementation
//	validator merge    — merge per-shard reports into one
//	validator trace    — §24 criteria→test traceability matrix; --strict is
//	                     the CI gate (0 unmapped, no defects), --write
//	                     regenerates tests/spec/traceability.{json,md}
//	                     (Phase-01.5 Task 1.5.3.3)
//
// CI contract (scripts/ci/shard_runner.sh):
//
//	validator run --shard=<i> --shards=4 --report=<dir>/shard-<i>.json
//	exits non-zero when any checkpoint lands in a failing status.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"go/format"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"exchange-testspec/checkpoints"
	"exchange-testspec/checks"
	"exchange-testspec/golden"
	spec "exchange-testspec/spec"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "run":
		err = cmdRun(os.Args[2:])
	case "extract":
		err = cmdExtract(os.Args[2:])
	case "list":
		err = cmdList(os.Args[2:])
	case "stubs":
		err = cmdStubs(os.Args[2:])
	case "merge":
		err = cmdMerge(os.Args[2:])
	case "trace":
		err = cmdTrace(os.Args[2:])
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "validator:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintf(os.Stderr, `usage: validator <command> [flags]

  run      --shard=<i> --shards=4 --report=<path> [--timeout=30s] [--retries=1]
             [--fail-on-skip] [--only=ID,ID,...]
  extract  [--write]                          regenerate corpus artifacts
  list     [--shard=<i>] [--status]           print checkpoint table
  stubs                                       print unimplemented checkpoint IDs
  merge    --reports=<glob> --report=<path>   merge per-shard JSON reports
  trace    [--strict] [--write] [--waivers=p] [--expected=N] [--unmapped]
             §24 criteria→test traceability matrix; fails on unmapped
             criteria / fail-severity defects (all defects under --strict)

env: EXC_REPO_ROOT EXC_DOCS_DIR EXC_CORE_BUILD EXC_TEST_DSN
     EXC_REDIS_TEST_ADDR EXC_NATS_URLS EXC_SENTINEL_ADDRS EXC_CLICKHOUSE_HTTP
`)
}

// buildRegistry wires every implementation: golden corpus first, then
// document-checkpoint bindings (checks may Bind() to golden cases).
func buildRegistry() *spec.Registry {
	r := spec.NewRegistry()
	golden.RegisterAll(r)
	checks.RegisterAll(r)
	return r
}

func committedCorpus() *spec.Corpus {
	c, err := checkpoints.Load()
	if err != nil || c.ExtractedCount == 0 {
		return nil // no committed corpus yet — vanished detection off
	}
	return c
}

func cmdRun(args []string) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	shard := fs.Int("shard", -1, "shard index to run (-1 = all)")
	shards := fs.Int("shards", spec.NumShards, "total shard count")
	reportPath := fs.String("report", "", "write JSON report to path")
	timeout := fs.Duration("timeout", 30*time.Second, "per-checkpoint timeout")
	retries := fs.Int("retries", 1, "retries for flaky recovery")
	failOnSkip := fs.Bool("fail-on-skip", false, "treat dependency skips as failures")
	only := fs.String("only", "", "comma-separated checkpoint ID allow-list")
	docs := fs.String("docs", "", "docs dir (default <root>/docs)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *shard >= *shards {
		return fmt.Errorf("--shard %d >= --shards %d", *shard, *shards)
	}
	if *shards != spec.NumShards {
		return fmt.Errorf("--shards must be %d (canonical shard grid per Phase-01.5); got %d",
			spec.NumShards, *shards)
	}

	env := spec.DefaultEnv()
	if *docs != "" {
		env.DocsDir = *docs
	}
	env.CheckTimeout = *timeout
	reg := buildRegistry()

	opt := spec.Options{
		Shard: *shard, Shards: *shards, Timeout: *timeout,
		Retries: *retries, FailOnSkip: *failOnSkip,
	}
	if *only != "" {
		opt.OnlyIDs = map[string]bool{}
		for _, id := range strings.Split(*only, ",") {
			opt.OnlyIDs[strings.TrimSpace(id)] = true
		}
	}

	runner := spec.NewRunner(env, reg, committedCorpus())
	rep, err := runner.Run(context.Background(), opt)
	if err != nil {
		return err
	}

	if *reportPath != "" {
		if err := rep.Write(*reportPath); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "report: %s\n", *reportPath)
	}
	printSummary(rep)
	os.Exit(rep.ExitCode())
	return nil
}

func printSummary(r *spec.Report) {
	fmt.Fprintf(os.Stderr,
		"shard=%d/%d total=%d pass=%d fail=%d timeout=%d error=%d skip=%d pending=%d missing=%d vanished=%d dropped=%d flaky=%d\n",
		r.Shard, r.Shards, r.Summary.Total, r.Summary.Pass, r.Summary.Fail,
		r.Summary.Timeout, r.Summary.Error, r.Summary.Skip, r.Summary.Pending,
		r.Summary.Missing, r.Summary.Vanished, r.Summary.Dropped, r.Summary.Flaky)
}

// ---------------------------------------------------------------------------

func cmdExtract(args []string) error {
	fs := flag.NewFlagSet("extract", flag.ContinueOnError)
	write := fs.Bool("write", false, "regenerate committed corpus artifacts")
	docs := fs.String("docs", "", "docs dir")
	if err := fs.Parse(args); err != nil {
		return err
	}
	env := spec.DefaultEnv()
	if *docs != "" {
		env.DocsDir = *docs
	}
	live, err := spec.Extract(env.DocsDir)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "extracted=%d raw_grep=%d tasks=%d shards=%v\n",
		live.ExtractedCount, live.RawGrepCount, live.TaskCount, live.ShardCounts())

	if !*write {
		// Diff mode: compare against committed corpus; drift = non-zero exit.
		old := committedCorpus()
		if old == nil {
			fmt.Fprintln(os.Stderr, "no committed corpus — run: validator extract --write")
			return nil
		}
		added, removed := diffCorpus(live, old)
		for _, id := range added {
			fmt.Printf("added   %s\n", id)
		}
		for _, id := range removed {
			fmt.Printf("removed %s\n", id)
		}
		if len(added)+len(removed) > 0 {
			return fmt.Errorf("corpus drift: %d added, %d removed — run extract --write to regenerate",
				len(added), len(removed))
		}
		fmt.Fprintln(os.Stderr, "corpus matches committed snapshot")
		return nil
	}
	return writeCorpusArtifacts(env, live)
}

// writeCorpusArtifacts regenerates checkpoints.json + stubs.gen.go + PENDING.md.
func writeCorpusArtifacts(env *spec.Env, live *spec.Corpus) error {
	reg := buildRegistry()
	dir := filepath.Join(env.RepoRoot, "tests", "spec", "checkpoints")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}

	b, err := json.MarshalIndent(live, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "checkpoints.json"), append(b, '\n'), 0o644); err != nil {
		return err
	}

	// Pending stub list: extracted checkpoints with no registered impl.
	var pending []spec.Checkpoint
	for _, cp := range live.Checkpoints {
		if _, ok := reg.Lookup(cp.ID); !ok {
			pending = append(pending, cp)
		}
	}
	var gen strings.Builder
	gen.WriteString("// Code generated by `validator extract --write`. DO NOT EDIT.\n\n")
	gen.WriteString("package checkpoints\n\n")
	gen.WriteString("// PendingStubs lists extracted checkpoint IDs with no registered\n")
	gen.WriteString("// implementation at generation time — the scaffold the runner reports\n// as pending/missing.\nvar PendingStubs = []string{\n")
	for _, cp := range pending {
		gen.WriteString(fmt.Sprintf("\t%q, // %s:%d [%s]\n", cp.ID, cp.File, cp.Line, trunc(cp.Text, 60)))
	}
	gen.WriteString("}\n")
	src, err := format.Source([]byte(gen.String()))
	if err != nil {
		src = []byte(gen.String()) // malformed generation shouldn't block corpus
	}
	if err := os.WriteFile(filepath.Join(dir, "stubs.gen.go"), src, 0o644); err != nil {
		return err
	}

	var md strings.Builder
	md.WriteString("# Pending checkpoint implementations\n\n")
	md.WriteString("Generated by `validator extract --write`. Each stub is a checkpoint with no\n")
	md.WriteString("registered CheckFunc. Implement in `tests/spec/checks/` (or `golden/`) as:\n\n")
	md.WriteString("```go\nr.Register(\"P05-T5.3.1-C1\", func(ctx context.Context, env *spec.Env) spec.Result {\n    // assertions…\n    return spec.Pass(\"evidence\")\n}, \"checkpoint text\")\n```\n\n")
	md.WriteString("| Checkpoint | Task | Status | Text |\n|---|---|---|---|\n")
	for _, cp := range pending {
		state := "[ ]"
		if cp.Checked {
			state = "[x]"
		}
		md.WriteString(fmt.Sprintf("| `%s` | %s (%s:%d) | %s | %s |\n",
			cp.ID, cp.Task, cp.File, cp.Line, state, trunc(cp.Text, 100)))
	}
	if err := os.WriteFile(filepath.Join(dir, "PENDING.md"), []byte(md.String()), 0o644); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "wrote %s (%d checkpoints, %d pending stubs)\n",
		dir, live.ExtractedCount, len(pending))
	return nil
}

func diffCorpus(live, committed *spec.Corpus) (added, removed []string) {
	have := map[string]bool{}
	for _, cp := range live.Checkpoints {
		have[cp.ID] = true
	}
	old := map[string]bool{}
	for _, cp := range committed.Checkpoints {
		old[cp.ID] = true
		if !have[cp.ID] {
			removed = append(removed, cp.ID)
		}
	}
	for _, cp := range live.Checkpoints {
		if !old[cp.ID] {
			added = append(added, cp.ID)
		}
	}
	sort.Strings(added)
	sort.Strings(removed)
	return
}

// trunc is rune-safe truncation for embedding checkpoint text in generated
// Go comments and markdown cells.
func trunc(s string, n int) string {
	s = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' || r == '|' {
			return ' '
		}
		if r < 0x20 {
			return -1
		}
		return r
	}, s)
	if len([]rune(s)) <= n {
		return s
	}
	rs := []rune(s)
	return string(rs[:n-1]) + "…"
}

// ---------------------------------------------------------------------------

func cmdList(args []string) error {
	fs := flag.NewFlagSet("list", flag.ContinueOnError)
	shard := fs.Int("shard", -1, "filter to shard")
	docs := fs.String("docs", "", "docs dir")
	if err := fs.Parse(args); err != nil {
		return err
	}
	env := spec.DefaultEnv()
	if *docs != "" {
		env.DocsDir = *docs
	}
	live, err := spec.Extract(env.DocsDir)
	if err != nil {
		return err
	}
	reg := buildRegistry()
	for _, cp := range live.Checkpoints {
		if *shard >= 0 && cp.Shard != *shard {
			continue
		}
		state := "pending"
		if _, ok := reg.Lookup(cp.ID); ok {
			state = "impl"
		} else if cp.Checked {
			state = "MISSING"
		}
		fmt.Printf("%-24s shard=%d %-8s %s:%d  %s\n",
			cp.ID, cp.Shard, state, cp.File, cp.Line, trunc(cp.Text, 70))
	}
	return nil
}

func cmdStubs(args []string) error {
	fs := flag.NewFlagSet("stubs", flag.ContinueOnError)
	docs := fs.String("docs", "", "docs dir")
	if err := fs.Parse(args); err != nil {
		return err
	}
	env := spec.DefaultEnv()
	if *docs != "" {
		env.DocsDir = *docs
	}
	live, err := spec.Extract(env.DocsDir)
	if err != nil {
		return err
	}
	reg := buildRegistry()
	n := 0
	for _, cp := range live.Checkpoints {
		if _, ok := reg.Lookup(cp.ID); !ok {
			fmt.Printf("%s  %s:%d  %s\n", cp.ID, cp.File, cp.Line, trunc(cp.Text, 70))
			n++
		}
	}
	fmt.Fprintf(os.Stderr, "%d checkpoints lack implementations\n", n)
	return nil
}

func cmdMerge(args []string) error {
	fs := flag.NewFlagSet("merge", flag.ContinueOnError)
	pat := fs.String("reports", "", "glob of per-shard reports")
	out := fs.String("report", "", "output path for merged report")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *pat == "" || *out == "" {
		return fmt.Errorf("merge requires --reports=<glob> and --report=<path>")
	}
	matches, err := filepath.Glob(*pat)
	if err != nil || len(matches) == 0 {
		return fmt.Errorf("no reports match %q", *pat)
	}
	var reps []*spec.Report
	for _, m := range matches {
		r, err := spec.LoadReport(m)
		if err != nil {
			return err
		}
		reps = append(reps, r)
	}
	merged := spec.Merge(reps)
	if err := merged.Write(*out); err != nil {
		return err
	}
	printSummary(merged)
	os.Exit(merged.ExitCode())
	return nil
}
