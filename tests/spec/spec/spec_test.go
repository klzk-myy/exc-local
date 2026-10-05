package spec

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// docsRoot returns the repo docs dir for framework self-tests.
func docsRoot(t *testing.T) string {
	t.Helper()
	env := DefaultEnv()
	if dirExists(env.DocsDir) {
		return env.DocsDir
	}
	t.Skip("docs dir not found")
	return ""
}

func TestExtractCounts(t *testing.T) {
	c, err := Extract(docsRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	// Canonical corpus: 569 strict checkpoints (570 raw grep lines incl. the
	// harness task's own prose mention) across 506 tasks in 31 phase files
	// (Phase-10.5 added 27).
	if c.ExtractedCount != 569 {
		t.Errorf("extracted=%d want 569", c.ExtractedCount)
	}
	if c.RawGrepCount != 570 {
		t.Errorf("raw grep=%d want 570", c.RawGrepCount)
	}
	if c.TaskCount != 506 {
		t.Errorf("tasks=%d want 506", c.TaskCount)
	}
	seen := map[string]bool{}
	for _, cp := range c.Checkpoints {
		if seen[cp.ID] {
			t.Fatalf("duplicate checkpoint ID %s", cp.ID)
		}
		seen[cp.ID] = true
		if cp.Task == "-" {
			t.Errorf("checkpoint %s not under a task header", cp.ID)
		}
	}
}

func TestCheckpointIDFormat(t *testing.T) {
	if got := checkpointID("01", "1.3.6", 2); got != "P01-T1.3.6-C2" {
		t.Fatalf("id=%s", got)
	}
	if got := checkpointID("19.5", "19.5.3.6", 1); got != "P19.5-T19.5.3.6-C1" {
		t.Fatalf("id=%s", got)
	}
}

func TestShardDeterminismAndCoverage(t *testing.T) {
	// Deterministic: same ID always maps to the same shard.
	for _, id := range []string{"P01-T1.3.6-C2", "P05-T5.3.7-C1", "GOLDEN-01-x"} {
		if ShardFor(id) != ShardFor(id) {
			t.Fatalf("ShardFor(%s) non-deterministic", id)
		}
		if s := ShardFor(id); s < 0 || s >= NumShards {
			t.Fatalf("ShardFor(%s)=%d out of range", id, s)
		}
	}
	c, err := Extract(docsRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	counts := c.ShardCounts()
	for i, n := range counts {
		if n == 0 {
			t.Fatalf("shard %d empty — every shard must own work", i)
		}
		if n == c.ExtractedCount {
			t.Fatalf("shard %d holds all checkpoints", i)
		}
	}
}

func TestRunnerStatuses(t *testing.T) {
	reg := NewRegistry()
	reg.Register("T-ok", func(context.Context, *Env) Result { return Pass("fine") }, "")
	reg.Register("T-bad", func(context.Context, *Env) Result { return Fail("broken") }, "")
	reg.Register("T-skip", func(context.Context, *Env) Result { return Skip("no dep") }, "")
	reg.Register("T-slow", func(context.Context, *Env) Result {
		time.Sleep(5 * time.Second)
		return Pass("late")
	}, "")
	reg.Register("T-panic", func(context.Context, *Env) Result {
		panic("boom")
	}, "")

	// Docs fixture: one checked checkpoint bound to T-ok's ID? Use a fake
	// docs dir — simplest: point extraction at a fixture we control.
	dir := t.TempDir()
	doc := `# Phase-99 — fixture

### Task 99.3.1: X

**SDD Checklist (MANDATORY):**
- [x] Spec checkpoint: ok one — defined
- [x] Spec checkpoint: bad one — defined
- [x] Spec checkpoint: skip me — defined
- [x] Spec checkpoint: slow one — defined
- [x] Spec checkpoint: panic one — defined
- [ ] Spec checkpoint: pending one — defined
- [x] Spec checkpoint: never implemented — defined
`
	fixture := []struct{ doc, id string }{
		{"ok one", "T-ok"}, {"bad one", "T-bad"}, {"skip me", "T-skip"},
		{"slow one", "T-slow"}, {"panic one", "T-panic"},
	}
	_ = fixture
	// IDs derive from doc position: C1..C7 for task 99.3.1.
	ids := map[string]string{
		"P99-T99.3.1-C1": "T-ok", "P99-T99.3.1-C2": "T-bad",
		"P99-T99.3.1-C3": "T-skip", "P99-T99.3.1-C4": "T-slow",
		"P99-T99.3.1-C5": "T-panic",
	}
	reg2 := NewRegistry()
	for cid, tid := range ids {
		e, _ := reg.Lookup(tid)
		reg2.Register(cid, e.Func, "")
	}
	if err := os.WriteFile(filepath.Join(dir, "Phase-99-Fixture.md"), []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	env := DefaultEnv()
	env.DocsDir = dir
	env.CheckTimeout = 200 * time.Millisecond

	r := NewRunner(env, reg2, nil)
	rep, err := r.Run(context.Background(), Options{Shard: -1, Timeout: 200 * time.Millisecond, Retries: 0})
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]Record{}
	for _, rec := range rep.Checkpoints {
		byID[rec.CheckpointID] = rec
	}
	want := map[string]Status{
		"P99-T99.3.1-C1": StatusPass,
		"P99-T99.3.1-C2": StatusFail,
		"P99-T99.3.1-C3": StatusSkip,
		"P99-T99.3.1-C4": StatusTimeout,
		"P99-T99.3.1-C5": StatusError,
		"P99-T99.3.1-C6": StatusPending,
		"P99-T99.3.1-C7": StatusMissing,
	}
	for id, st := range want {
		if byID[id].Status != st {
			t.Errorf("%s: got %s want %s", id, byID[id].Status, st)
		}
	}
	if rep.ExitCode() == 0 {
		t.Error("exit code must be non-zero with failures present")
	}
}

func TestRunnerVanished(t *testing.T) {
	dir := t.TempDir()
	// Docs now lack P01-T1.3.1-C1 which the committed corpus recorded as [x].
	if err := os.WriteFile(filepath.Join(dir, "Phase-01-X.md"),
		[]byte("# P\n### Task 1.3.1: X\n- [ ] Spec checkpoint: other — x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	env := DefaultEnv()
	env.DocsDir = dir
	reg := NewRegistry()
	// Corpus holds two IDs the fixture doc no longer contains: one that was
	// recorded checked (completed phase → vanished = FAIL) and one recorded
	// unchecked (pending phase → dropped = warn only).
	corpus := &Corpus{Checkpoints: []Checkpoint{
		{ID: "P01-T1.3.9-C1", Phase: "01", Task: "1.3.9", Checked: true, Shard: 0},
		{ID: "P02-T2.3.1-C1", Phase: "02", Task: "2.3.1", Checked: false, Shard: 1},
	}}
	rep, err := NewRunner(env, reg, corpus).Run(context.Background(),
		Options{Shard: -1, Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	var vanished, dropped bool
	for _, r := range rep.Checkpoints {
		if r.CheckpointID == "P01-T1.3.9-C1" && r.Status == StatusVanished {
			vanished = true
		}
		if r.CheckpointID == "P02-T2.3.1-C1" && r.Status == StatusDropped {
			dropped = true
		}
	}
	if !vanished || !dropped {
		t.Errorf("want vanished(checked)+dropped(unchecked), got %v", rep.Checkpoints)
	}
}

func TestRunnerFlakyRetry(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "Phase-99-F.md"),
		[]byte("### Task 99.3.1: X\n- [x] Spec checkpoint: flaky — x\n"), 0o644)
	env := DefaultEnv()
	env.DocsDir = dir
	attempts := 0
	reg := NewRegistry()
	reg.Register("P99-T99.3.1-C1", func(context.Context, *Env) Result {
		attempts++
		if attempts == 1 {
			return Fail("transient")
		}
		return Pass("recovered")
	}, "")
	rep, err := NewRunner(env, reg, nil).Run(context.Background(),
		Options{Shard: -1, Timeout: time.Second, Retries: 1})
	if err != nil {
		t.Fatal(err)
	}
	rec := rep.Checkpoints[0]
	if rec.Status != StatusPass || !rec.Flaky || rec.Attempts != 2 {
		t.Errorf("want pass+flaky+2 attempts, got %s flaky=%v attempts=%d",
			rec.Status, rec.Flaky, rec.Attempts)
	}
}

func TestReportExitCode(t *testing.T) {
	r := &Report{}
	r.Summary.Fail = 1
	if r.ExitCode() == 0 {
		t.Error("fail must exit non-zero")
	}
	r2 := &Report{}
	r2.Summary.Skip = 3
	if r2.ExitCode() != 0 {
		t.Error("skip alone must not fail (default policy)")
	}
	r2.FailOnSkip = true
	if r2.ExitCode() == 0 {
		t.Error("fail-on-skip must fail on skips")
	}
}
