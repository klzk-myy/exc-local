// traceability.json reader — READ-ONLY consumption of the spec harness
// artifact (tests/spec/traceability.json is owned by Phase-01.5 tooling;
// this file only unmarshals it).
package itest

import (
	"encoding/json"
	"fmt"
	"os"
)

// TraceCriterion mirrors the tests/spec traceability.json row shape.
type TraceCriterion struct {
	ID                 int      `json:"id"`
	Criterion          string   `json:"criterion"`
	SpecLine           int      `json:"spec_line"`
	Subsection         string   `json:"subsection"`
	OwnerPhase         string   `json:"owner_phase"`
	OwnerPhaseTags     []string `json:"owner_phase_tags"`
	OwnerPhaseDocs     []string `json:"owner_phase_docs"`
	StableTestContract string   `json:"stable_test_contract"`
	PhaseACRefs        []struct {
		Raw string `json:"raw"`
		Doc string `json:"doc"`
	} `json:"phase_ac_refs"`
	Status string `json:"status"`
}

// TraceFile is the traceability.json envelope.
type TraceFile struct {
	Tool             string `json:"tool"`
	Version          string `json:"version"`
	ExpectedCriteria int    `json:"expected_criteria"`
	Summary          struct {
		DeclaredTotal int `json:"declared_total"`
		Criteria      int `json:"criteria"`
		Mapped        int `json:"mapped"`
		Unmapped      int `json:"unmapped"`
	} `json:"summary"`
	Criteria []TraceCriterion `json:"criteria"`
}

// LoadTrace parses tests/spec/traceability.json.
func LoadTrace(path string) (*TraceFile, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("traceability read: %w", err)
	}
	var tf TraceFile
	if err := json.Unmarshal(b, &tf); err != nil {
		return nil, fmt.Errorf("traceability parse: %w", err)
	}
	if len(tf.Criteria) == 0 {
		return nil, fmt.Errorf("traceability %s: zero criteria", path)
	}
	return &tf, nil
}

// Contracts projects the traceability corpus into test-contract rows:
// bound → EXECUTABLE, unbound → PLANNED. Runtime outcomes (PASS/FAIL/
// BLOCKED) are emitted by the suite into the coverage report — the
// committed registry never claims execution results.
func Contracts(tf *TraceFile, bindings map[int][]Binding) []Contract {
	out := make([]Contract, 0, len(tf.Criteria))
	for _, c := range tf.Criteria {
		row := Contract{
			CriterionID:        c.ID,
			Criterion:          c.Criterion,
			Subsection:         c.Subsection,
			OwnerPhase:         c.OwnerPhase,
			OwnerPhaseTags:     c.OwnerPhaseTags,
			OwnerPhaseDocs:     c.OwnerPhaseDocs,
			StableTestContract: c.StableTestContract,
			Status:             "PLANNED",
		}
		for _, r := range c.PhaseACRefs {
			row.PhaseAC = append(row.PhaseAC, r.Raw)
		}
		if bs, ok := bindings[c.ID]; ok && len(bs) > 0 {
			row.Bindings = bs
			if PhaseWindow(c.OwnerPhaseTags) {
				row.Status = "EXECUTABLE"
			} else {
				// Bound legs on later-phase criteria still run when they
				// exist — the row stays honest about which phase owns it.
				row.Status = "EXECUTABLE"
			}
		}
		out = append(out, row)
	}
	return out
}
