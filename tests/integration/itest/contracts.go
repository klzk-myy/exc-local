// §24 test-contract registry: every criterion maps to owner phase,
// stable test ID, phase AC reference and status — generated into
// contracts.json by cmd/contractgen and consumed by the suite.
//
// Status vocabulary (Task 8.3.1, extended honestly for env gating):
//
//	PLANNED     contract defined; no executable binding yet
//	            (later-phase owners, or Phase 1–7 criteria whose legs
//	            still need a harness that does not exist on this host)
//	EXECUTABLE  binding exists; not executed in the last run
//	PASS        bound leg(s) ran and held
//	FAIL        bound leg(s) ran and an assertion failed
//	BLOCKED     binding exists but an env gate denied execution this run
//	            (reason recorded verbatim — e.g. no docker socket,
//	            sentinel quorum absent). BLOCKED is an extension of the
//	            task's four-value enum; it prevents PASS-laundering.
package itest

// BindingKind enumerates how a criterion is made executable.
type BindingKind string

const (
	BindGoTest  BindingKind = "gotest"  // `go test <pkg> -run <regex>` inside services/
	BindCTest   BindingKind = "ctest"   // `ctest -R <regex>` in core/build
	BindGTest   BindingKind = "gtest"   // one gtest binary + filter
	BindE2E     BindingKind = "e2e"     // named leg inside this suite's live stack
	BindBin     BindingKind = "bin"     // run a repo binary/CLI against live deps
	BindCompose BindingKind = "compose" // requires docker-compose topology (Task spec reference env)
	BindVitest  BindingKind = "vitest"  // `npx vitest run <pkg>` inside frontend/
)

// Binding is one executable leg bound to a criterion.
type Binding struct {
	Kind   BindingKind `json:"kind"`
	Pkg    string      `json:"pkg,omitempty"`    // gotest: services-relative package
	Run    string      `json:"run,omitempty"`    // gotest: -run regex; ctest: -R regex
	Binary string      `json:"binary,omitempty"` // gtest/bin: binary name
	Filter string      `json:"filter,omitempty"` // gtest filter
	Test   string      `json:"test,omitempty"`   // e2e: Go test name in this suite
	Note   string      `json:"note,omitempty"`
	// Legs that need infra this host may lack carry a gate tag; the
	// runner records BLOCKED (with reason) when unsatisfied.
	Needs string `json:"needs,omitempty"` // "", "pg", "redis", "nats", "engine", "docker", "sentinel", "aeron"; "+"-joined = all required
}

// Contract is one contracts.json row.
type Contract struct {
	CriterionID        int       `json:"criterion_id"`
	Criterion          string    `json:"criterion"`
	Subsection         string    `json:"subsection"`
	OwnerPhase         string    `json:"owner_phase"`
	OwnerPhaseTags     []string  `json:"owner_phase_tags"`
	OwnerPhaseDocs     []string  `json:"owner_phase_docs"`
	PhaseAC            []string  `json:"phase_ac_refs"`
	StableTestContract string    `json:"stable_test_id"`
	Status             string    `json:"status"` // PLANNED|EXECUTABLE|PASS|FAIL|BLOCKED
	Bindings           []Binding `json:"bindings,omitempty"`
	Note               string    `json:"note,omitempty"`
}

// PhaseWindow returns whether the criterion's owner phase is inside the
// Phase 1–7 executable window (including the embedded buffer phases
// 01.5/02.5/04.5, whose acceptance sits inside the Phase-08 gate).
func PhaseWindow(tags []string) bool {
	for _, t := range tags {
		switch t {
		case "01", "01.5", "02", "02.5", "03", "04", "04.5", "05", "06", "07":
			return true
		}
	}
	return false
}
