package spec

import (
	"context"
	"fmt"
	"runtime/debug"
	"sort"
	"time"
)

// Options tune a Runner invocation.
type Options struct {
	Shard      int             // run only checkpoints hashed to this shard (-1 = all)
	Shards     int             // total shard count (default NumShards)
	Timeout    time.Duration   // per-checkpoint timeout (default 30s)
	Retries    int             // re-attempts allowed for flaky recovery (default 1)
	FailOnSkip bool            // treat dependency-skips as failures (strict CI)
	OnlyIDs    map[string]bool // optional allow-list (--only)
}

// Runner executes a shard's checkpoints against the registry.
type Runner struct {
	Env      *Env
	Registry *Registry
	// Corpus is the committed snapshot (tests/spec/checkpoints.json); used to
	// detect checkpoints that vanished from docs while marked done.
	Corpus *Corpus
}

// NewRunner builds a runner; corpus may be nil (vanished detection off).
func NewRunner(env *Env, reg *Registry, corpus *Corpus) *Runner {
	return &Runner{Env: env, Registry: reg, Corpus: corpus}
}

// Run extracts the live checkpoint set, runs the shard's share, and returns
// the report. Wall-clock budget is caller-controlled via ctx.
func (rn *Runner) Run(ctx context.Context, opt Options) (*Report, error) {
	if opt.Shards <= 0 {
		opt.Shards = NumShards
	}
	if opt.Timeout <= 0 {
		opt.Timeout = rn.Env.CheckTimeout
	}
	start := time.Now()

	live, err := Extract(rn.Env.DocsDir)
	if err != nil {
		return nil, fmt.Errorf("extract checkpoints: %w", err)
	}
	rn.Env.Live = live
	rn.Env.Registry = rn.Registry
	rn.Env.Committed = rn.Corpus

	rep := &Report{
		Tool:        "exchange-testspec/validator",
		Version:     Version,
		GeneratedAt: start.UTC(),
		RepoRoot:    rn.Env.RepoRoot,
		Docs:        rn.Env.DocsDir,
		Shard:       opt.Shard,
		Shards:      opt.Shards,
		Extracted:   live.ExtractedCount,
		RawGrep:     live.RawGrepCount,
		FailOnSkip:  opt.FailOnSkip,
	}
	if rn.Corpus != nil {
		rep.CorpusFile = "tests/spec/checkpoints/checkpoints.json"
	}

	// 1) Live checkpoints assigned to this shard.
	var work []Checkpoint
	for _, cp := range live.Checkpoints {
		if opt.Shard >= 0 && ShardFor(cp.ID) != opt.Shard {
			continue
		}
		if opt.OnlyIDs != nil && !opt.OnlyIDs[cp.ID] {
			continue
		}
		work = append(work, cp)
	}

	// 2) Registered GOLDEN-* cases in this shard (corpus evidence rows).
	// ShardFor is defined mod NumShards, so goldens stay put regardless of
	// the --shards flag value.
	var goldens []*Entry
	for _, id := range rn.Registry.GoldenIDs() {
		if opt.Shard >= 0 && ShardFor(id) != opt.Shard {
			continue
		}
		if opt.OnlyIDs != nil && !opt.OnlyIDs[id] {
			continue
		}
		goldens = append(goldens, rn.Registry.entries[id])
	}

	// 3) Vanished/dropped detection: corpus IDs absent from the live set.
	type vanishedRow struct {
		id      string
		checked bool
		phase   string
		task    string
	}
	var vanished []vanishedRow
	if rn.Corpus != nil {
		liveIDs := map[string]bool{}
		for _, cp := range live.Checkpoints {
			liveIDs[cp.ID] = true
		}
		for _, cp := range rn.Corpus.Checkpoints {
			if liveIDs[cp.ID] {
				continue
			}
			if opt.Shard >= 0 && ShardFor(cp.ID) != opt.Shard {
				continue
			}
			vanished = append(vanished, vanishedRow{cp.ID, cp.Checked, cp.Phase, cp.Task})
		}
	}

	// Execute live checkpoints.
	for _, cp := range work {
		rec := rn.execCheckpoint(ctx, cp, opt)
		rep.Checkpoints = append(rep.Checkpoints, rec)
	}
	// Execute golden corpus cases.
	for _, e := range goldens {
		rec := rn.execEntry(ctx, e, opt)
		rep.Checkpoints = append(rep.Checkpoints, rec)
	}
	// Append vanished/dropped rows (no execution — the checkpoint is gone).
	for _, v := range vanished {
		st := StatusDropped
		detail := "checkpoint removed from docs while phase unchecked — regenerate corpus (validator extract --write)"
		if v.checked {
			st = StatusVanished
			detail = "checkpoint recorded [x] in corpus but absent from docs — completed-phase coverage must not disappear"
		}
		rep.Checkpoints = append(rep.Checkpoints, Record{
			CheckpointID: v.id, Phase: v.phase, Task: v.task,
			Status: st, Shard: ShardFor(v.id), Detail: detail,
		})
	}

	sort.Slice(rep.Checkpoints, func(i, j int) bool {
		return rep.Checkpoints[i].CheckpointID < rep.Checkpoints[j].CheckpointID
	})

	for _, r := range rep.Checkpoints {
		rep.Summary.Total++
		rep.Summary.DurationMs += r.DurationMs
		switch r.Status {
		case StatusPass:
			rep.Summary.Pass++
		case StatusFail:
			rep.Summary.Fail++
		case StatusTimeout:
			rep.Summary.Timeout++
		case StatusError:
			rep.Summary.Error++
		case StatusSkip:
			rep.Summary.Skip++
		case StatusPending:
			rep.Summary.Pending++
		case StatusMissing:
			rep.Summary.Missing++
		case StatusVanished:
			rep.Summary.Vanished++
		case StatusDropped:
			rep.Summary.Dropped++
		}
		if r.Flaky {
			rep.Summary.Flaky++
		}
	}
	_ = time.Since(start)
	return rep, nil
}

// execCheckpoint resolves the checkpoint's implementation status then runs
// it when implemented.
func (rn *Runner) execCheckpoint(ctx context.Context, cp Checkpoint, opt Options) Record {
	rec := Record{
		CheckpointID: cp.ID, Phase: cp.Phase, Task: cp.Task,
		Text: cp.Text, Shard: cp.Shard,
	}
	e, ok := rn.Registry.Lookup(cp.ID)
	if !ok {
		if cp.Checked {
			rec.Status = StatusMissing
			rec.Detail = "task marked [x] in docs but no checkpoint implementation is registered"
		} else {
			rec.Status = StatusPending
			rec.Detail = "no implementation registered; doc checkbox still [ ]"
		}
		return rec
	}
	return rn.runWithPolicy(ctx, cp.ID, e.Func, opt, rec)
}

func (rn *Runner) execEntry(ctx context.Context, e *Entry, opt Options) Record {
	rec := Record{
		CheckpointID: e.ID, Phase: "golden", Task: "corpus",
		Text: e.Notes, Shard: ShardFor(e.ID),
	}
	return rn.runWithPolicy(ctx, e.ID, e.Func, opt, rec)
}

// runWithPolicy executes fn with per-checkpoint timeout, panic capture and
// the configured retry policy. A checkpoint that fails then passes within
// --retries is reported pass + flaky:true (flakiness is surfaced, not hidden).
func (rn *Runner) runWithPolicy(ctx context.Context, id string, fn CheckFunc, opt Options, rec Record) Record {
	attempts := 0
	var res Result
	var dur time.Duration
	for attempts <= opt.Retries {
		attempts++
		r := rn.call(ctx, fn, opt.Timeout)
		dur += r.dur
		res = r.res
		if res.Status != StatusFail && res.Status != StatusError && res.Status != StatusTimeout {
			break
		}
		// brief backoff — real races (port reuse, cache warm) settle fast
		select {
		case <-ctx.Done():
			res = Result{Status: StatusError, Detail: "run context cancelled: " + ctx.Err().Error()}
			attempts = opt.Retries + 1
		case <-time.After(250 * time.Millisecond):
		}
	}
	rec.Status = res.Status
	rec.Detail = res.Detail
	rec.DurationMs = dur.Milliseconds()
	rec.Attempts = attempts
	if attempts > 1 && rec.Status == StatusPass {
		rec.Flaky = true
		rec.Detail = fmt.Sprintf("[flaky: passed on attempt %d/%d] %s", attempts, opt.Retries+1, res.Detail)
	}
	return rec
}

type callOut struct {
	res Result
	dur time.Duration
}

// call runs fn in its own goroutine with a hard timeout and panic capture.
// A checkpoint may not take the harness down with it.
func (rn *Runner) call(ctx context.Context, fn CheckFunc, timeout time.Duration) callOut {
	done := make(chan callOut, 1)
	go func() {
		start := time.Now()
		defer func() {
			if p := recover(); p != nil {
				done <- callOut{
					res: Result{Status: StatusError,
						Detail: fmt.Sprintf("panic: %v\n%s", p, tail(string(debug.Stack()), 15))},
					dur: time.Since(start),
				}
			}
		}()
		done <- callOut{res: fn(ctx, rn.Env), dur: time.Since(start)}
	}()
	select {
	case r := <-done:
		return r
	case <-ctx.Done():
		return callOut{res: Result{Status: StatusError, Detail: "run context cancelled: " + ctx.Err().Error()}}
	case <-time.After(timeout):
		return callOut{res: Result{Status: StatusTimeout,
			Detail: fmt.Sprintf("checkpoint exceeded timeout %s", timeout)}}
	}
}
