// Per-criterion result registry + machine-readable coverage report.
//
// Emission contract (Task 8.3.1 step 4): the suite writes
// reports/coverage.json and prints a status-count summary. EXECUTABLE
// legs that pass promote the criterion to PASS; a leg failure demotes
// to FAIL; an unsatisfied gate reports BLOCKED with the reason. A
// criterion is PASS only when every bound leg ran and held — partial
// evidence never inflates the verdict.
package itest

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// ResultStatus is the runtime outcome for one criterion.
type ResultStatus string

const (
	StatusPass       ResultStatus = "PASS"
	StatusFail       ResultStatus = "FAIL"
	StatusBlocked    ResultStatus = "BLOCKED"
	StatusExecutable ResultStatus = "EXECUTABLE"
	StatusPlanned    ResultStatus = "PLANNED"
)

// LegResult is one executed binding leg's outcome.
type LegResult struct {
	Test    string    `json:"test"` // Go test name or delegated leg id
	Kind    string    `json:"kind"`
	Status  string    `json:"status"` // PASS|FAIL|BLOCKED
	Detail  string    `json:"detail,omitempty"`
	At      time.Time `json:"at"`
	Elapsed string    `json:"elapsed,omitempty"`
}

// CriterionResult aggregates a criterion's leg outcomes.
type CriterionResult struct {
	CriterionID        int          `json:"criterion_id"`
	StableTestContract string       `json:"stable_test_id"`
	OwnerPhase         string       `json:"owner_phase"`
	Status             ResultStatus `json:"status"`
	Legs               []LegResult  `json:"legs,omitempty"`
	BlockedReason      string       `json:"blocked_reason,omitempty"`
}

// Registry collects leg results during the run.
type Registry struct {
	mu        sync.Mutex
	legs      map[int][]LegResult
	contracts map[int]Contract
	start     time.Time
}

// NewRegistry binds results to the contract rows.
func NewRegistry(contracts []Contract) *Registry {
	r := &Registry{legs: map[int][]LegResult{}, contracts: map[int]Contract{}, start: time.Now()}
	for _, c := range contracts {
		r.contracts[c.CriterionID] = c
	}
	return r
}

// HasLegs reports whether any leg outcome was recorded. Callers use it
// to avoid overwriting the committed coverage report when a filtered
// test run executes no bound legs — an empty run has no evidence to
// emit and must not clobber prior PASS/FAIL/BLOCKED results.
func (r *Registry) HasLegs() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.legs) > 0
}

// Record adds one leg outcome for a criterion. kind is the leg label
// (e.g. "e2e:orders", "gotest:./internal/settlement", "gtest:test_wal").
func (r *Registry) Record(criterionID int, kind, status, detail string, elapsed time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.legs[criterionID] = append(r.legs[criterionID], LegResult{
		Test: kind, Kind: kind, Status: status, Detail: detail,
		At: time.Now().UTC(), Elapsed: elapsed.String(),
	})
}

// Recordf is Record with formatted detail.
func (r *Registry) Recordf(criterionID int, kind, status, format string, args ...any) {
	r.Record(criterionID, kind, status, fmt.Sprintf(format, args...), 0)
}

// Finalize computes per-criterion verdicts:
//
//	PASS    — at least one leg ran and every executed leg passed
//	          (a leg must have RUN: all-blocked criteria are BLOCKED)
//	FAIL    — any leg failed
//	BLOCKED — every bound leg was gated (reasons aggregated)
//	EXECUTABLE — bound, nothing ran (suite filtered)
//	PLANNED — unbound
func (r *Registry) Finalize() []CriterionResult {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]CriterionResult, 0, len(r.contracts))
	for id, c := range r.contracts {
		cr := CriterionResult{
			CriterionID:        id,
			StableTestContract: c.StableTestContract,
			OwnerPhase:         c.OwnerPhase,
			Status:             StatusPlanned,
		}
		legs := r.legs[id]
		if len(c.Bindings) > 0 {
			cr.Status = StatusExecutable
		}
		if len(legs) > 0 {
			var passes, fails, blocked int
			var reasons []string
			for _, l := range legs {
				switch l.Status {
				case string(StatusPass):
					passes++
				case string(StatusFail):
					fails++
				default:
					blocked++
					if l.Detail != "" {
						reasons = append(reasons, l.Detail)
					}
				}
			}
			switch {
			case fails > 0:
				cr.Status = StatusFail
			case passes > 0 && blocked == 0:
				cr.Status = StatusPass
			case passes > 0 && blocked > 0:
				// Partial evidence: criterion stays EXECUTABLE — PASS
				// requires the whole bound surface (Phase-08 forbids
				// false PASS). The legs that ran are still on record.
				cr.Status = StatusExecutable
				cr.BlockedReason = "partial: " + strings.Join(reasons, "; ")
			case blocked > 0:
				cr.Status = StatusBlocked
				cr.BlockedReason = strings.Join(reasons, "; ")
			}
			cr.Legs = legs
		}
		out = append(out, cr)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CriterionID < out[j].CriterionID })
	return out
}

// Report is reports/coverage.json.
type Report struct {
	Tool      string         `json:"tool"`
	Version   string         `json:"version"`
	Generated string         `json:"generated_at"`
	Elapsed   string         `json:"elapsed"`
	Summary   map[string]int `json:"summary"`
	// PhaseWindow splits Phase 1–7-owned criteria (the executable
	// window for Task 8.3.1) from later-phase rows.
	WindowCounts map[string]int    `json:"phase_1_7_counts"`
	Criteria     []CriterionResult `json:"criteria"`
}

// Emit writes the JSON report and returns the summary counts.
func (r *Registry) Emit(path string) (*Report, error) {
	final := r.Finalize()
	rep := &Report{
		Tool: "exchange-integration/8.3.1", Version: "1.0.0",
		Generated:    time.Now().UTC().Format(time.RFC3339),
		Elapsed:      time.Since(r.start).String(),
		Summary:      map[string]int{},
		WindowCounts: map[string]int{},
		Criteria:     final,
	}
	for _, c := range final {
		rep.Summary[string(c.Status)]++
		row := r.contracts[c.CriterionID]
		if PhaseWindow(row.OwnerPhaseTags) {
			rep.WindowCounts[string(c.Status)]++
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	b, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		return nil, err
	}
	return rep, nil
}

// SummaryLine renders the machine-readable status counts (one line,
// key=value pairs — greppable from CI logs).
func (rep *Report) SummaryLine() string {
	keys := []string{"PASS", "FAIL", "EXECUTABLE", "BLOCKED", "PLANNED"}
	var b strings.Builder
	b.WriteString("COVERAGE")
	total := 0
	for _, k := range keys {
		fmt.Fprintf(&b, " %s=%d", k, rep.Summary[k])
		total += rep.Summary[k]
	}
	fmt.Fprintf(&b, " total=%d | phase1-7", total)
	for _, k := range keys {
		fmt.Fprintf(&b, " %s=%d", k, rep.WindowCounts[k])
	}
	return b.String()
}
