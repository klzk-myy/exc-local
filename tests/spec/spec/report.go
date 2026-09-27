package spec

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Record is one report row — the per-checkpoint pass/fail contract from
// Phase-01.5 Task 1.5.3.2 ("JSON report with pass/fail per checkpoint").
type Record struct {
	CheckpointID string `json:"checkpoint_id"`
	Phase        string `json:"phase"`
	Task         string `json:"task"`
	Text         string `json:"text,omitempty"`
	Status       Status `json:"status"`
	Shard        int    `json:"shard"`
	DurationMs   int64  `json:"duration_ms"`
	Detail       string `json:"detail,omitempty"`
	Attempts     int    `json:"attempts,omitempty"` // >1 means flaky-recovered
	Flaky        bool   `json:"flaky,omitempty"`    // passed after ≥1 retry
}

// Summary aggregates counts for CI dashboards and the run's exit code.
type Summary struct {
	Total      int   `json:"total"`
	Pass       int   `json:"pass"`
	Fail       int   `json:"fail"`
	Timeout    int   `json:"timeout"`
	Error      int   `json:"error"`
	Skip       int   `json:"skip"`
	Pending    int   `json:"pending"`
	Missing    int   `json:"missing"`
	Vanished   int   `json:"vanished"`
	Dropped    int   `json:"dropped"`
	Flaky      int   `json:"flaky"`
	DurationMs int64 `json:"duration_ms"`
}

// Failures is the number of records in failing statuses.
func (s *Summary) Failures() int {
	return s.Fail + s.Timeout + s.Error + s.Missing + s.Vanished
}

// Report is the JSON document written to --report=<path>.
type Report struct {
	Tool        string    `json:"tool"`
	Version     string    `json:"version"`
	GeneratedAt time.Time `json:"generated_at"`
	RepoRoot    string    `json:"repo_root"`
	Docs        string    `json:"docs"`
	Shard       int       `json:"shard"`
	Shards      int       `json:"shards"`
	Extracted   int       `json:"extracted"`   // checkpoints extracted from docs this run
	RawGrep     int       `json:"raw_grep"`    // literal "Spec checkpoint:" lines (canonical count)
	CorpusFile  string    `json:"corpus_file"` // committed corpus used for vanished-detection
	FailOnSkip  bool      `json:"fail_on_skip"`
	Summary     Summary   `json:"summary"`
	Checkpoints []Record  `json:"checkpoints"`
}

// Write renders the report as indented JSON to path (creating parents).
func (r *Report) Write(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}

// ExitCode is the CI contract: non-zero on any failure-status record.
// --fail-on-skip additionally fails when records were skipped (strict mode
// for CI runners where infra is guaranteed up).
func (r *Report) ExitCode() int {
	if r.Summary.Failures() > 0 {
		return 1
	}
	if r.FailOnSkip && r.Summary.Skip > 0 {
		return 1
	}
	return 0
}

// Merge combines per-shard report files into one aggregate report.
// Shard fields are taken from the first report.
func Merge(reports []*Report) *Report {
	out := &Report{
		Tool:        "exchange-testspec/validator",
		Version:     Version,
		GeneratedAt: time.Now().UTC(),
		Shards:      NumShards,
	}
	if len(reports) > 0 {
		out.RepoRoot = reports[0].RepoRoot
		out.Docs = reports[0].Docs
		out.Extracted = reports[0].Extracted
		out.RawGrep = reports[0].RawGrep
		out.CorpusFile = reports[0].CorpusFile
	}
	for _, r := range reports {
		out.Checkpoints = append(out.Checkpoints, r.Checkpoints...)
		out.Summary.Total += r.Summary.Total
		out.Summary.Pass += r.Summary.Pass
		out.Summary.Fail += r.Summary.Fail
		out.Summary.Timeout += r.Summary.Timeout
		out.Summary.Error += r.Summary.Error
		out.Summary.Skip += r.Summary.Skip
		out.Summary.Pending += r.Summary.Pending
		out.Summary.Missing += r.Summary.Missing
		out.Summary.Vanished += r.Summary.Vanished
		out.Summary.Dropped += r.Summary.Dropped
		out.Summary.Flaky += r.Summary.Flaky
		out.Summary.DurationMs += r.Summary.DurationMs
	}
	return out
}

// LoadReport parses a report file (used by `merge`).
func LoadReport(path string) (*Report, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var r Report
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &r, nil
}

// Version of the harness (bump on schema changes).
const Version = "1.0.0"
