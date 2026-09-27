// Traceability matrix builder (Phase-01.5 Task 1.5.3.3).
//
// Parses the master specification's §24 Acceptance Criteria & Traceability
// Matrix (one row per criterion, columns: # | Criterion | Owner Phase |
// Phase AC Reference | Stable Test Contract) and resolves every criterion
// to concrete validation edges found in the docs corpus:
//
//	phase_ac    — declared "Phase NN-Name §AC row M" refs resolved against
//	              the phase doc's ## N.7 Acceptance Criteria table, plus any
//	              AC-table row that cites "(§24 #N)" back at the criterion
//	task_ac     — task-level Definition-of-Done checkbox lines citing §24 #N
//	checkpoint  — extracted "Spec checkpoint:" lines citing §24 #N, bound to
//	              their stable checkpoint ID (P<phase>-T<task>-C<idx>)
//	golden      — GOLDEN-* corpus cases whose registration notes cite §24 #N
//	components  — spec §27.2 component-catalog §24 references (informational
//	              cross-check; does not count toward mapped status)
//
// A criterion is "mapped" when it has ≥1 phase_ac/task_ac/checkpoint/golden
// edge. Missing owner phase, missing/unresolvable AC references, a missing
// stable test contract, colliding contract IDs and dangling §24 #N citations
// are fail-severity defects; stale/expired waivers, committed-matrix drift
// and §24.3 declared-summary mismatches are warn-severity. `trace` exits
// non-zero on any fail-severity defect; --strict also fails on warnings.
//
// Waivers (tests/spec/traceability.waivers.json) are the documented escape
// hatch for a criterion that legitimately has no executable edge yet; they
// can only suppress the unmapped verdict — never a structural defect —
// because spec §24.4 rule 4 prohibits waiving deleted/renamed criteria.
package spec

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// Types
// ---------------------------------------------------------------------------

// TraceACRow is one resolved row inside a declared Phase-AC reference.
type TraceACRow struct {
	Row            int    `json:"row"`
	Exists         bool   `json:"exists"`
	CitesCriterion bool   `json:"cites_criterion"` // the AC row itself cites §24 #<id>
	Text           string `json:"text,omitempty"`  // AC row text
	Line           int    `json:"line,omitempty"`  // line in the phase doc
}

// TraceACRef is one "Phase NN-Name §AC row(s) ..." reference from the matrix.
type TraceACRef struct {
	Raw      string       `json:"raw"` // as written in the matrix cell
	Doc      string       `json:"doc"` // Phase-NN-Name.md
	Rows     []TraceACRow `json:"rows"`
	Resolved bool         `json:"resolved"` // doc exists AND every row exists
}

// TraceCheckpointEdge binds a criterion to an extracted SDD checkpoint.
type TraceCheckpointEdge struct {
	ID          string `json:"id"`          // P<phase>-T<task>-C<idx>
	Implemented bool   `json:"implemented"` // a CheckFunc is registered
	Checked     bool   `json:"checked"`     // doc checkbox state
	Text        string `json:"text,omitempty"`
}

// TraceEdges groups every edge a criterion accumulated. PhaseAC/TaskAC are
// "doc#AC<row>" / "doc:line" location strings; Checkpoints/Golden are IDs.
type TraceEdges struct {
	PhaseAC    []string              `json:"phase_ac,omitempty"`
	TaskAC     []string              `json:"task_ac,omitempty"`
	Checkpoint []TraceCheckpointEdge `json:"checkpoints,omitempty"`
	Golden     []string              `json:"golden,omitempty"`
	Components []string              `json:"spec_components,omitempty"` // §27.2 catalog — informational
}

// EdgeCount counts the edges that establish a mapping (spec §27.2 component
// references are informational and do not count).
func (e *TraceEdges) EdgeCount() int {
	return len(e.PhaseAC) + len(e.TaskAC) + len(e.Checkpoint) + len(e.Golden)
}

// TraceCriterion is one fully-resolved §24 matrix row.
type TraceCriterion struct {
	ID           int          `json:"id"`
	Text         string       `json:"criterion"`
	SpecLine     int          `json:"spec_line"`
	Subsection   string       `json:"subsection"` // "24.1" | "24.2"
	OwnerRaw     string       `json:"owner_phase"`
	OwnerPhases  []string     `json:"owner_phase_tags"` // normalized: "02", "19.5"
	OwnerDocs    []string     `json:"owner_phase_docs"` // resolved Phase-*.md basenames
	ACRefs       []TraceACRef `json:"phase_ac_refs"`
	Contract     string       `json:"stable_test_contract"`
	ContractNote string       `json:"contract_note,omitempty"`
	Edges        TraceEdges   `json:"edges"`
	Status       string       `json:"status"` // mapped | waived | unmapped
	Waiver       *TraceWaiver `json:"waiver,omitempty"`
	Defects      []string     `json:"defects,omitempty"` // defect kinds attached to this criterion
}

// TraceWaiver is one entry from traceability.waivers.json. A waiver
// suppresses ONLY the unmapped verdict for a criterion with zero edges.
type TraceWaiver struct {
	Criterion int    `json:"criterion"` // §24 criterion ID — required
	Reason    string `json:"reason"`    // required, non-empty
	AddedBy   string `json:"added_by,omitempty"`
	Added     string `json:"added,omitempty"`   // YYYY-MM-DD
	Expires   string `json:"expires,omitempty"` // YYYY-MM-DD — past expiry = stale
}

// waiverFile is the on-disk schema of traceability.waivers.json:
//
//	{"version": 1, "waivers": [{"criterion": 288, "reason": "…",
//	 "added_by": "…", "added": "2026-09-27", "expires": "2026-12-31"}]}
type waiverFile struct {
	Version int           `json:"version"`
	Waivers []TraceWaiver `json:"waivers"`
}

// TraceDefect is one matrix-integrity finding. Severity "fail" breaks the
// gate; "warn" breaks only --strict.
type TraceDefect struct {
	Kind      string `json:"kind"`
	Severity  string `json:"severity"` // fail | warn
	Criterion int    `json:"criterion,omitempty"`
	Cited     int    `json:"cited,omitempty"`    // dangling-reference target
	Location  string `json:"location,omitempty"` // file:line
	Detail    string `json:"detail"`
}

// Defect kinds.
const (
	DefectRowCount        = "row_count_mismatch"   // §24.3 declared total ≠ rows found
	DefectMissingID       = "missing_criterion_id" // non-contiguous numbering
	DefectDuplicateID     = "duplicate_criterion_id"
	DefectUnparseableRow  = "unparseable_row"
	DefectMissingOwner    = "missing_owner_phase"    // §24.4 r2: no owner phase
	DefectUnknownPhaseDoc = "unknown_phase_doc"      // owner phase has no doc
	DefectMissingACRef    = "missing_ac_ref"         // §24.4 r2: empty AC column
	DefectBrokenACDoc     = "broken_ac_ref_doc"      // referenced phase doc absent
	DefectBrokenACRow     = "broken_ac_ref_row"      // referenced AC row absent
	DefectMissingContract = "missing_contract"       // §24.4 r2: no stable test contract
	DefectContractCollide = "contract_collision"     // same contract ID on >1 criterion
	DefectDanglingRef     = "dangling_section24_ref" // §24 #N cites a nonexistent criterion
	DefectUnmapped        = "unmapped_criterion"     // zero edges — the core metric
	DefectInvalidWaiver   = "invalid_waiver"
	DefectStaleWaiver     = "stale_waiver"              // warn — criterion already has edges / absent
	DefectExpiredWaiver   = "expired_waiver"            // warn
	DefectMatrixDrift     = "matrix_drift"              // warn — committed artifact ≠ live matrix
	DefectNoCommitted     = "no_committed_matrix"       // warn — nothing committed to diff
	DefectSummaryMismatch = "declared_summary_mismatch" // warn — §24.3 self-declared metrics
)

const (
	SevFail = "fail"
	SevWarn = "warn"
)

// TraceSummary is the matrix-level accounting.
type TraceSummary struct {
	DeclaredTotal  int            `json:"declared_total"` // §24.3 "Total §24 criteria"
	Criteria       int            `json:"criteria"`
	Contiguous     bool           `json:"contiguous"` // IDs exactly 1..N
	Mapped         int            `json:"mapped"`
	Unmapped       int            `json:"unmapped"`
	Waived         int            `json:"waived"`
	EdgeTotals     map[string]int `json:"edge_totals"`
	CheckpointImpl int            `json:"checkpoint_edges_implemented"` // edges with a registered CheckFunc
	FailDefects    int            `json:"fail_defects"`
	WarnDefects    int            `json:"warn_defects"`
}

// TraceMatrix is the committed artifact written to tests/spec/traceability.json.
type TraceMatrix struct {
	Tool        string           `json:"tool"`
	Version     string           `json:"version"`
	GeneratedAt time.Time        `json:"generated_at"`
	RepoRoot    string           `json:"repo_root"`
	Docs        string           `json:"docs"`
	Spec        string           `json:"spec"` // spec file basename
	Expected    int              `json:"expected_criteria"`
	Summary     TraceSummary     `json:"summary"`
	Waivers     []TraceWaiver    `json:"waivers,omitempty"`
	Defects     []TraceDefect    `json:"defects,omitempty"`
	Criteria    []TraceCriterion `json:"criteria"`
}

// ExitCode is the CI contract: fail-severity defects (incl. every unmapped
// criterion) exit non-zero; --strict additionally fails on warnings.
func (m *TraceMatrix) ExitCode(strict bool) int {
	if m.Summary.FailDefects > 0 {
		return 1
	}
	if strict && m.Summary.WarnDefects > 0 {
		return 1
	}
	return 0
}

// ---------------------------------------------------------------------------
// Options & driver
// ---------------------------------------------------------------------------

// TraceOptions tune BuildTraceability.
type TraceOptions struct {
	Expected    int       // >0 overrides the §24.3 declared total
	WaiversPath string    // "" → no waiver file
	LiveCorpus  *Corpus   // nil → Extract(env.DocsDir) is run internally
	Now         time.Time // zero → time.Now() (waiver expiry)
}

// BuildTraceability parses spec §24 plus every Phase-*.md under env.DocsDir,
// resolves all edges, applies waivers and returns the evaluated matrix.
// reg supplies golden-case citations and checkpoint implementation status.
func BuildTraceability(env *Env, reg *Registry, opt TraceOptions) (*TraceMatrix, error) {
	if opt.Now.IsZero() {
		opt.Now = time.Now()
	}
	if reg == nil {
		reg = NewRegistry()
	}

	specPath, err := findSpecDoc(env.DocsDir)
	if err != nil {
		return nil, err
	}
	parsed, err := parseSpec24(specPath)
	if err != nil {
		return nil, err
	}

	// Phase docs: AC tables (declared-ref targets + citing rows) and
	// task-level DoD checkbox citations.
	phaseDocs, err := PhaseDocs(env.DocsDir)
	if err != nil {
		return nil, err
	}
	tag2doc := map[string]string{}         // phase tag "02" → basename
	acTables := map[string]map[int]acRow{} // basename → row → acRow
	acCiteSites := map[int][]string{}      // criterion → "doc#AC<row>" citing sites
	taskACSites := map[int][]string{}      // criterion → "doc:line" sites
	for _, doc := range phaseDocs {
		base := filepath.Base(doc)
		tag, err := PhaseFromFile(base)
		if err != nil {
			continue
		}
		tag2doc[tag] = base
		rows, err := parsePhaseAC(doc)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", base, err)
		}
		acTables[base] = rows
		for rn, ar := range rows {
			for _, n := range ar.refs {
				acCiteSites[n] = append(acCiteSites[n], fmt.Sprintf("%s#AC%d", base, rn))
			}
		}
		for _, site := range parseTaskACCitations(doc) {
			for _, n := range site.refs {
				taskACSites[n] = append(taskACSites[n], site.loc)
			}
		}
	}

	// Checkpoint corpus → §24 citation edges.
	corpus := opt.LiveCorpus
	if corpus == nil {
		corpus, err = Extract(env.DocsDir)
		if err != nil {
			return nil, err
		}
	}
	ckptEdges := map[int][]TraceCheckpointEdge{}
	for _, cp := range corpus.Checkpoints {
		for _, n := range parseS24Refs(cp.Text) {
			_, impl := reg.Lookup(cp.ID)
			ckptEdges[n] = append(ckptEdges[n], TraceCheckpointEdge{
				ID: cp.ID, Implemented: impl, Checked: cp.Checked, Text: cp.Text,
			})
		}
	}

	// Golden corpus: registration notes citing §24 #N.
	goldenEdges := map[int][]string{}
	for _, id := range reg.GoldenIDs() {
		e, _ := reg.Lookup(id)
		for _, n := range parseS24Refs(e.Notes) {
			goldenEdges[n] = append(goldenEdges[n], id)
		}
	}

	// Every §24 #N citation anywhere in docs/*.md + root-level *.md docs
	// (dangling-reference detection covers renamed/deleted criteria).
	sites, err := collectS24Citations(env)
	if err != nil {
		return nil, err
	}

	waivers, werr := loadWaivers(opt.WaiversPath)

	m := &TraceMatrix{
		Tool:        "exchange-testspec/validator trace",
		Version:     Version,
		GeneratedAt: opt.Now.UTC(),
		RepoRoot:    env.RepoRoot,
		Docs:        filepath.ToSlash(env.DocsDir),
		Spec:        filepath.Base(specPath),
	}

	expected := opt.Expected
	if expected <= 0 {
		expected = parsed.declaredTotal
	}
	m.Expected = expected

	fail := func(kind string, crit, cited int, loc, detail string) {
		m.Defects = append(m.Defects, TraceDefect{Kind: kind, Severity: SevFail,
			Criterion: crit, Cited: cited, Location: loc, Detail: detail})
	}
	warn := func(kind string, crit, cited int, loc, detail string) {
		m.Defects = append(m.Defects, TraceDefect{Kind: kind, Severity: SevWarn,
			Criterion: crit, Cited: cited, Location: loc, Detail: detail})
	}

	if werr != nil {
		fail(DefectInvalidWaiver, 0, 0, opt.WaiversPath, werr.Error())
	}
	now := opt.Now.UTC().Format("2006-01-02")
	waiverByID := map[int]*TraceWaiver{}
	for i := range waivers {
		w := &waivers[i]
		if w.Criterion <= 0 || strings.TrimSpace(w.Reason) == "" {
			fail(DefectInvalidWaiver, w.Criterion, 0, opt.WaiversPath,
				"waiver requires positive criterion + non-empty reason")
			continue
		}
		if w.Expires != "" && w.Expires < now {
			warn(DefectExpiredWaiver, w.Criterion, 0, opt.WaiversPath,
				fmt.Sprintf("waiver expired %s — ignored (criterion evaluated on its edges)", w.Expires))
			continue
		}
		if _, dup := waiverByID[w.Criterion]; dup {
			fail(DefectInvalidWaiver, w.Criterion, 0, opt.WaiversPath, "duplicate waiver for criterion")
			continue
		}
		waiverByID[w.Criterion] = w
	}
	m.Waivers = waivers

	// --- structural: count + contiguity -------------------------------------
	if expected > 0 && len(parsed.rows) != expected {
		fail(DefectRowCount, 0, 0, m.Spec,
			fmt.Sprintf("§24 matrix has %d rows; declared/expected total is %d (spec §24.3/§24.4)",
				len(parsed.rows), expected))
	}
	seen := map[int]int{} // id → count
	for _, r := range parsed.rows {
		seen[r.id]++
	}
	for id, n := range seen {
		if n > 1 {
			fail(DefectDuplicateID, id, 0, m.Spec,
				fmt.Sprintf("criterion %d appears %d times", id, n))
		}
	}
	maxID := 0
	for id := range seen {
		if id > maxID {
			maxID = id
		}
	}
	limit := expected
	if maxID > limit {
		limit = maxID
	}
	contiguous := true
	var missingIDs []int
	for i := 1; i <= limit; i++ {
		if seen[i] == 0 {
			missingIDs = append(missingIDs, i)
			contiguous = false
		}
	}
	if len(missingIDs) > 0 {
		fail(DefectMissingID, 0, 0, m.Spec,
			fmt.Sprintf("criterion IDs not contiguous — missing: %v", intRangeStr(missingIDs)))
	}
	m.Summary.Contiguous = contiguous
	m.Summary.DeclaredTotal = parsed.declaredTotal

	// --- per-criterion resolution --------------------------------------------
	contractOwner := map[string][]int{} // contract id → criteria
	for _, r := range parsed.rows {
		c := TraceCriterion{
			ID: r.id, Text: r.text, SpecLine: r.line, Subsection: r.sub,
			OwnerRaw: r.owner, Edges: TraceEdges{},
		}
		var cdef []string

		// Owner phase(s) → phase docs.
		for _, num := range ownerRe.FindAllStringSubmatch(r.owner, -1) {
			tag := normPhaseTag(num[1])
			c.OwnerPhases = append(c.OwnerPhases, tag)
			if doc, ok := tag2doc[tag]; ok {
				c.OwnerDocs = append(c.OwnerDocs, doc)
			} else {
				cdef = append(cdef, DefectUnknownPhaseDoc)
				fail(DefectUnknownPhaseDoc, c.ID, 0, loc(m.Spec, r.line),
					fmt.Sprintf("owner phase %q has no Phase-%s-*.md doc", r.owner, tag))
			}
		}
		if len(c.OwnerPhases) == 0 {
			cdef = append(cdef, DefectMissingOwner)
			fail(DefectMissingOwner, c.ID, 0, loc(m.Spec, r.line),
				fmt.Sprintf("owner cell %q names no phase", r.owner))
		}

		// Declared Phase AC references → resolve rows.
		refs := acRefRe.FindAllStringSubmatch(r.acref, -1)
		if strings.TrimSpace(r.acref) == "" {
			cdef = append(cdef, DefectMissingACRef)
			fail(DefectMissingACRef, c.ID, 0, loc(m.Spec, r.line), "empty Phase AC Reference column")
		} else if len(refs) == 0 {
			cdef = append(cdef, DefectMissingACRef)
			fail(DefectMissingACRef, c.ID, 0, loc(m.Spec, r.line),
				fmt.Sprintf("unparseable Phase AC Reference %q", r.acref))
		}
		edgeSet := map[string]bool{}
		for _, rm := range refs {
			ref := TraceACRef{Raw: strings.TrimSpace(rm[0]), Doc: "Phase-" + rm[1] + "-" + rm[2] + ".md"}
			rows := expandRowSpec(rm[3])
			table, docOK := acTables[ref.Doc]
			if !docOK {
				for _, rn := range rows {
					ref.Rows = append(ref.Rows, TraceACRow{Row: rn})
				}
				cdef = append(cdef, DefectBrokenACDoc)
				fail(DefectBrokenACDoc, c.ID, 0, loc(m.Spec, r.line),
					fmt.Sprintf("AC ref %q targets %s which is not in docs/", ref.Raw, ref.Doc))
			} else {
				ref.Resolved = true
				for _, rn := range rows {
					row := TraceACRow{Row: rn}
					if ar, ok := table[rn]; ok {
						row.Exists = true
						row.Text = ar.text
						row.Line = ar.line
						for _, n := range ar.refs {
							if n == c.ID {
								row.CitesCriterion = true
							}
						}
						edgeSet[ref.Doc+"#AC"+strconv.Itoa(rn)] = true
					} else {
						ref.Resolved = false
						cdef = append(cdef, DefectBrokenACRow)
						fail(DefectBrokenACRow, c.ID, rn, loc(m.Spec, r.line),
							fmt.Sprintf("AC ref %q — %s has no AC row %d", ref.Raw, ref.Doc, rn))
					}
					ref.Rows = append(ref.Rows, row)
				}
			}
			c.ACRefs = append(c.ACRefs, ref)
		}

		// AC rows that cite §24 #N back at the criterion (declared or not).
		for _, e := range acCiteSites[c.ID] {
			edgeSet[e] = true
		}

		// Contract (first backticked ID in the cell).
		if cm := contractRe.FindStringSubmatch(r.contract); cm != nil {
			c.Contract = cm[1]
			c.ContractNote = strings.TrimSpace(contractRe.ReplaceAllString(r.contract, ""))
			contractOwner[c.Contract] = append(contractOwner[c.Contract], c.ID)
		} else {
			cdef = append(cdef, DefectMissingContract)
			fail(DefectMissingContract, c.ID, 0, loc(m.Spec, r.line),
				"Stable Test Contract column has no `ID-nnn` token")
		}

		// Edge sets.
		for e := range edgeSet {
			c.Edges.PhaseAC = append(c.Edges.PhaseAC, e)
		}
		sort.Strings(c.Edges.PhaseAC)
		for _, s := range taskACSites[c.ID] {
			c.Edges.TaskAC = append(c.Edges.TaskAC, s)
		}
		sort.Strings(c.Edges.TaskAC)
		for _, e := range ckptEdges[c.ID] {
			c.Edges.Checkpoint = append(c.Edges.Checkpoint, e)
		}
		sort.Slice(c.Edges.Checkpoint, func(i, j int) bool { return c.Edges.Checkpoint[i].ID < c.Edges.Checkpoint[j].ID })
		for _, g := range goldenEdges[c.ID] {
			c.Edges.Golden = append(c.Edges.Golden, g)
		}
		sort.Strings(c.Edges.Golden)
		for _, comp := range parsed.components[c.ID] {
			c.Edges.Components = append(c.Edges.Components, comp)
		}
		sort.Strings(c.Edges.Components)

		// Status + waiver.
		if c.Edges.EdgeCount() == 0 {
			if w, ok := waiverByID[c.ID]; ok {
				c.Status = "waived"
				c.Waiver = w
				m.Summary.Waived++
			} else {
				c.Status = "unmapped"
				m.Summary.Unmapped++
				cdef = append(cdef, DefectUnmapped)
				fail(DefectUnmapped, c.ID, 0, loc(m.Spec, r.line),
					fmt.Sprintf("criterion %d has zero validation edges (no resolvable phase-AC, checkpoint, task-AC or golden cite)", c.ID))
			}
		} else {
			c.Status = "mapped"
			m.Summary.Mapped++
			if w, ok := waiverByID[c.ID]; ok {
				c.Waiver = w
				warn(DefectStaleWaiver, c.ID, 0, opt.WaiversPath,
					fmt.Sprintf("waiver for criterion %d is stale — %d edge(s) already exist", c.ID, c.Edges.EdgeCount()))
			}
		}
		c.Defects = cdef
		m.Criteria = append(m.Criteria, c)
	}

	// Waivers referencing criteria absent from the matrix are stale.
	for _, w := range waivers {
		if seen[w.Criterion] == 0 {
			warn(DefectStaleWaiver, w.Criterion, 0, opt.WaiversPath,
				"waiver references criterion that does not exist in §24")
		}
	}

	// Contract collisions: one stable ID claimed by >1 criterion.
	colliding := map[int]bool{}
	for id, owners := range contractOwner {
		if len(owners) > 1 {
			for _, cid := range owners {
				colliding[cid] = true
				fail(DefectContractCollide, cid, 0, m.Spec,
					fmt.Sprintf("stable test contract %s is claimed by criteria %v", id, owners))
			}
		}
	}
	for i := range m.Criteria {
		if colliding[m.Criteria[i].ID] {
			m.Criteria[i].Defects = append(m.Criteria[i].Defects, DefectContractCollide)
		}
	}

	// Dangling §24 #N citations across all docs (renamed/deleted criteria).
	for _, s := range sites {
		for _, n := range s.refs {
			if seen[n] == 0 {
				fail(DefectDanglingRef, 0, n, s.loc,
					fmt.Sprintf("cites §24 #%d which does not exist in the §24 matrix", n))
			}
		}
	}
	// §27.2 component-catalog refs to nonexistent criteria dangle too.
	for n, comps := range parsed.components {
		if seen[n] == 0 {
			fail(DefectDanglingRef, 0, n, m.Spec,
				fmt.Sprintf("§27.2 component rows %v cite #%d which does not exist in §24", comps, n))
		}
	}

	// §24.3 declared summary vs computed (stated-count audit).
	m.Summary.Criteria = len(m.Criteria)
	m.Summary.EdgeTotals = map[string]int{}
	for _, c := range m.Criteria {
		m.Summary.EdgeTotals["phase_ac"] += len(c.Edges.PhaseAC)
		m.Summary.EdgeTotals["task_ac"] += len(c.Edges.TaskAC)
		m.Summary.EdgeTotals["checkpoint"] += len(c.Edges.Checkpoint)
		m.Summary.EdgeTotals["golden"] += len(c.Edges.Golden)
		m.Summary.EdgeTotals["spec_components"] += len(c.Edges.Components)
		for _, e := range c.Edges.Checkpoint {
			if e.Implemented {
				m.Summary.CheckpointImpl++
			}
		}
	}
	m.checkDeclaredSummary(parsed)

	m.Recount()
	sort.Slice(m.Criteria, func(i, j int) bool { return m.Criteria[i].ID < m.Criteria[j].ID })
	return m, nil
}

// Recount recomputes defect tallies after late defect appends (e.g. the
// committed-matrix drift merge done by `validator trace`).
func (m *TraceMatrix) Recount() {
	m.Summary.FailDefects, m.Summary.WarnDefects = 0, 0
	for _, d := range m.Defects {
		if d.Severity == SevWarn {
			m.Summary.WarnDefects++
		} else {
			m.Summary.FailDefects++
		}
	}
}

// checkDeclaredSummary compares §24.3's self-declared metrics against the
// computed matrix (stated-count vs actual-row audit, repo convention).
func (m *TraceMatrix) checkDeclaredSummary(p *spec24Parse) {
	check := func(label string, declared, computed int) {
		if declared >= 0 && declared != computed {
			m.Defects = append(m.Defects, TraceDefect{
				Kind: DefectSummaryMismatch, Severity: SevWarn, Location: m.Spec,
				Detail: fmt.Sprintf("§24.3 declares %q = %d; computed %d", label, declared, computed)})
		}
	}
	withAC := 0
	for _, c := range m.Criteria {
		if len(c.ACRefs) > 0 {
			withAC++
		}
	}
	check("Total §24 criteria", p.declaredTotal, len(m.Criteria))
	check("Criteria with explicit phase AC row reference", p.declaredWithACRef, withAC)
	check("Unmapped criteria", p.declaredUnmapped, m.Summary.Unmapped)
	check("Criteria without stable test contract", p.declaredNoContract, m.Summary.Criteria-countContracts(m))
	check("Criteria mapped by owner-phase inference", p.declaredOwnerInferred,
		m.Summary.Criteria-withAC)
}

func countContracts(m *TraceMatrix) int {
	n := 0
	for _, c := range m.Criteria {
		if c.Contract != "" {
			n++
		}
	}
	return n
}

// ---------------------------------------------------------------------------
// Spec §24 parsing
// ---------------------------------------------------------------------------

type rawCriterion struct {
	id       int
	text     string
	owner    string
	acref    string
	contract string
	line     int
	sub      string // "24.1" / "24.2"
}

type citeSite struct {
	loc  string // file:line
	refs []int
}

type spec24Parse struct {
	rows                  []rawCriterion
	declaredTotal         int
	declaredWithACRef     int
	declaredUnmapped      int
	declaredNoContract    int
	declaredOwnerInferred int
	components            map[int][]string // criterion → component labels (§27.2)
}

var (
	spec24HeadRe  = regexp.MustCompile(`^## 24\.\s`)
	h2Re          = regexp.MustCompile(`^## `)
	sub24Re       = regexp.MustCompile(`^### (24\.\d)`)
	rowNumRe      = regexp.MustCompile(`^\|\s*([0-9]+)\s*\|`)
	acRefRe       = regexp.MustCompile(`Phase ([0-9]+(?:\.[0-9]+)?)-([A-Za-z0-9][A-Za-z0-9.-]*?) §AC rows? ([0-9]+(?:[\s,–—-]+[0-9]+)*)`)
	ownerRe       = regexp.MustCompile(`Phase ([0-9]+(?:\.[0-9]+)?)`)
	contractRe    = regexp.MustCompile("`([A-Za-z0-9]+(?:\\.[0-9]+)?-[0-9]+)`")
	s24CiteRe     = regexp.MustCompile(`§24\s+#([0-9]+(?:[\s,;/–—-]+#?[0-9]+)*)`)
	acSectionRe   = regexp.MustCompile(`^## ([0-9]+(?:\.[0-9]+)*)\s+Acceptance Criteria`)
	checkboxRe    = regexp.MustCompile(`^\s*[-*] \[([ xX])\]`)
	compHeadRe    = regexp.MustCompile(`^#### (27\.2\.\d+)\s+Domain \d+:\s*(.+?)\s*$`)
	summaryCellRe = regexp.MustCompile(`([0-9]+)`)
)

// findSpecDoc locates the master specification inside docsDir: any *.md
// containing the "## 24. Acceptance Criteria" heading (prefer a file named
// Specification*.md).
func findSpecDoc(docsDir string) (string, error) {
	matches, err := filepath.Glob(filepath.Join(docsDir, "*.md"))
	if err != nil {
		return "", err
	}
	var fallback string
	for _, p := range matches {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		found := false
		for _, line := range strings.Split(string(b), "\n") {
			if spec24HeadRe.MatchString(line) {
				found = true
				break
			}
		}
		if !found {
			continue
		}
		if strings.HasPrefix(filepath.Base(p), "Specification") {
			return p, nil
		}
		if fallback == "" {
			fallback = p
		}
	}
	if fallback != "" {
		return fallback, nil
	}
	return "", fmt.Errorf("no spec doc with '## 24. Acceptance Criteria' under %s", docsDir)
}

// parseSpec24 extracts §24 matrix rows, the §24.3 declared-summary metrics,
// and the §27.2 component→criteria catalog references.
func parseSpec24(path string) (*spec24Parse, error) {
	fh, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer fh.Close()

	p := &spec24Parse{declaredTotal: -1, declaredWithACRef: -1, declaredUnmapped: -1,
		declaredNoContract: -1, declaredOwnerInferred: -1, components: map[int][]string{}}

	sc := bufio.NewScanner(fh)
	sc.Buffer(make([]byte, 0, 256*1024), 1024*1024)
	inS24 := false  // inside ## 24 section
	inComp := false // inside ### 27.2 component catalog
	sub := ""
	domain := ""
	compCritCol := -1 // column index of "§24 Criteria" in current component table
	lineNo := 0
	for sc.Scan() {
		lineNo++
		line := sc.Text()

		if spec24HeadRe.MatchString(line) {
			inS24 = true
			continue
		}
		if inS24 && h2Re.MatchString(line) {
			inS24 = false
		}
		if strings.HasPrefix(line, "### 27.2") {
			inComp = true
			compCritCol = -1
			continue
		}
		if inComp && strings.HasPrefix(line, "### 27.3") {
			inComp = false
		}
		if inComp {
			if m := compHeadRe.FindStringSubmatch(line); m != nil {
				domain = fmt.Sprintf("§%s %s", m[1], m[2])
				compCritCol = -1
				continue
			}
			cells := splitMDRow(line)
			if len(cells) == 0 {
				compCritCol = -1
				continue
			}
			// Header row: locate the "§24 Criteria" column once per table.
			for i, c := range cells {
				if strings.Contains(c, "§24 Criteria") {
					compCritCol = i
					break
				}
			}
			if compCritCol >= 0 && len(cells) > compCritCol && rowNumRe.MatchString(line) {
				name := ""
				if len(cells) > 1 {
					name = cells[1]
				}
				label := fmt.Sprintf("%s — %s", domain, name)
				for _, n := range parseHashRefList(cells[compCritCol]) {
					p.components[n] = append(p.components[n], label)
				}
			}
			continue
		}
		if !inS24 {
			continue
		}
		if m := sub24Re.FindStringSubmatch(line); m != nil {
			sub = m[1]
			continue
		}
		if m := rowNumRe.FindStringSubmatch(line); m != nil {
			cells := splitMDRow(line)
			if len(cells) != 5 {
				return nil, fmt.Errorf("%s:%d: §24 matrix row has %d cells (want 5 — check for unescaped '|'): %s",
					filepath.Base(path), lineNo, len(cells), truncStr(line, 100))
			}
			id, _ := strconv.Atoi(m[1])
			p.rows = append(p.rows, rawCriterion{
				id: id, text: strings.TrimSpace(cells[1]), owner: strings.TrimSpace(cells[2]),
				acref: strings.TrimSpace(cells[3]), contract: strings.TrimSpace(cells[4]),
				line: lineNo, sub: sub,
			})
			continue
		}
		// §24.3 declared coverage summary: | Metric | value (…note) |
		if cells := splitMDRow(line); len(cells) >= 2 {
			metric := strings.TrimSpace(cells[0])
			val := -1
			if mm := summaryCellRe.FindStringSubmatch(cells[1]); mm != nil {
				val, _ = strconv.Atoi(mm[1])
			}
			switch metric {
			case "Total §24 criteria":
				p.declaredTotal = val
			case "Criteria with explicit phase AC row reference":
				p.declaredWithACRef = val
			case "Unmapped criteria":
				p.declaredUnmapped = val
			case "Criteria without stable test contract":
				p.declaredNoContract = val
			case "Criteria mapped by owner-phase inference":
				p.declaredOwnerInferred = val
			}
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if len(p.rows) == 0 {
		return nil, fmt.Errorf("%s: no §24 matrix rows found", filepath.Base(path))
	}
	return p, nil
}

// ---------------------------------------------------------------------------
// Phase-doc parsing: AC tables + task-AC checkbox citations
// ---------------------------------------------------------------------------

type acRow struct {
	text string
	line int
	refs []int // §24 #N citations inside the row
}

// parsePhaseAC extracts the "## N.7 Acceptance Criteria" table of a phase
// doc: row number → {text, line, §24 refs}.
func parsePhaseAC(path string) (map[int]acRow, error) {
	fh, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer fh.Close()
	out := map[int]acRow{}
	sc := bufio.NewScanner(fh)
	sc.Buffer(make([]byte, 0, 256*1024), 1024*1024)
	inAC := false
	lineNo := 0
	for sc.Scan() {
		lineNo++
		line := sc.Text()
		if acSectionRe.MatchString(line) {
			inAC = true
			continue
		}
		if inAC && h2Re.MatchString(line) {
			break // next top-level section — AC table is terminal
		}
		if !inAC {
			continue
		}
		m := rowNumRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		cells := splitMDRow(line)
		if len(cells) < 2 {
			continue
		}
		n, _ := strconv.Atoi(m[1])
		text := strings.TrimSpace(cells[1])
		out[n] = acRow{text: text, line: lineNo, refs: parseS24Refs(text)}
	}
	return out, sc.Err()
}

// parseTaskACCitations returns file:line sites of task-level checkbox lines
// (Definition-of-Done bullets — not "Spec checkpoint:" lines) citing §24 #N.
func parseTaskACCitations(path string) []citeSite {
	fh, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer fh.Close()
	var out []citeSite
	sc := bufio.NewScanner(fh)
	sc.Buffer(make([]byte, 0, 256*1024), 1024*1024)
	lineNo := 0
	for sc.Scan() {
		lineNo++
		line := sc.Text()
		if !checkboxRe.MatchString(line) || strings.Contains(line, "Spec checkpoint:") {
			continue
		}
		if refs := parseS24Refs(line); len(refs) > 0 {
			out = append(out, citeSite{loc: fmt.Sprintf("%s:%d", filepath.Base(path), lineNo), refs: refs})
		}
	}
	return out
}

// collectS24Citations scans every doc under docsDir plus repo-root *.md
// meta-docs for §24 #N references (dangling-detection surface).
func collectS24Citations(env *Env) ([]citeSite, error) {
	pats := []string{
		filepath.Join(env.DocsDir, "*.md"),
		filepath.Join(env.RepoRoot, "*.md"),
	}
	var files []string
	for _, pat := range pats {
		ms, err := filepath.Glob(pat)
		if err != nil {
			return nil, err
		}
		files = append(files, ms...)
	}
	sort.Strings(files)
	var out []citeSite
	for _, f := range files {
		fh, err := os.Open(f)
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(fh)
		sc.Buffer(make([]byte, 0, 256*1024), 1024*1024)
		lineNo := 0
		for sc.Scan() {
			lineNo++
			if !strings.Contains(sc.Text(), "§24 #") {
				continue
			}
			if refs := parseS24Refs(sc.Text()); len(refs) > 0 {
				out = append(out, citeSite{
					loc: fmt.Sprintf("%s:%d", filepath.Base(f), lineNo), refs: refs})
			}
		}
		fh.Close()
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Reference-grammar helpers
// ---------------------------------------------------------------------------

// splitMDRow splits a markdown table row on UNESCAPED pipes; `\|` inside a
// cell stays literal. Returns trimmed cell contents, or nil for non-rows.
func splitMDRow(line string) []string {
	s := strings.TrimRight(line, " \t")
	if !strings.HasPrefix(strings.TrimSpace(s), "|") {
		return nil
	}
	s = strings.TrimSpace(s)
	var cells []string
	var cur strings.Builder
	for i := 1; i < len(s); i++ {
		c := s[i]
		if c == '\\' && i+1 < len(s) && s[i+1] == '|' {
			cur.WriteByte('|')
			i++
			continue
		}
		if c == '|' {
			cells = append(cells, strings.TrimSpace(cur.String()))
			cur.Reset()
			continue
		}
		cur.WriteByte(c)
	}
	if strings.TrimSpace(cur.String()) != "" {
		cells = append(cells, strings.TrimSpace(cur.String()))
	}
	return cells
}

// parseS24Refs extracts criterion numbers from `§24 #…` citations, handling
// lists (`§24 #1, #2`), slash groups (`§24 #327/#328`) and ranges
// (`§24 #135–137`, `§24 #33–#36`).
func parseS24Refs(text string) []int {
	var out []int
	for _, m := range s24CiteRe.FindAllStringSubmatch(text, -1) {
		out = append(out, expandRefTail(m[1])...)
	}
	return dedupeInts(out)
}

// parseHashRefList parses the §27.2 "§24 Criteria" column: "#1, #2, #33–#36".
func parseHashRefList(cell string) []int {
	return dedupeInts(expandRefTail(strings.ReplaceAll(cell, "#", "")))
}

// expandRefTail parses a citation tail: comma/slash/space-separated items,
// each a number or an a–b (en/em/hyphen) range. Ranges are capped at 512
// members so a typo'd mega-range can't blow up a matrix row.
func expandRefTail(tail string) []int {
	var out []int
	for _, tok := range regexp.MustCompile(`[,;/\s]+`).Split(tail, -1) {
		tok = strings.ReplaceAll(tok, "#", "")
		if tok == "" {
			continue
		}
		if m := regexp.MustCompile(`^([0-9]+)[–—-]([0-9]+)$`).FindStringSubmatch(tok); m != nil {
			lo, _ := strconv.Atoi(m[1])
			hi, _ := strconv.Atoi(m[2])
			if lo > hi {
				lo, hi = hi, lo
			}
			if hi-lo > 512 {
				out = append(out, lo, hi) // absurd range: keep endpoints only
				continue
			}
			for i := lo; i <= hi; i++ {
				out = append(out, i)
			}
			continue
		}
		if m := regexp.MustCompile(`^([0-9]+)$`).FindStringSubmatch(tok); m != nil {
			n, _ := strconv.Atoi(m[1])
			out = append(out, n)
		}
	}
	return out
}

// expandRowSpec parses an AC-ref row spec like "2", "5, 6", "26–27".
func expandRowSpec(spec string) []int {
	return dedupeInts(expandRefTail(spec))
}

func normPhaseTag(num string) string {
	parts := strings.SplitN(num, ".", 2)
	head, err := strconv.Atoi(parts[0])
	if err != nil {
		return num
	}
	tag := fmt.Sprintf("%02d", head)
	if len(parts) == 2 {
		tag += "." + parts[1]
	}
	return tag
}

func dedupeInts(in []int) []int {
	seen := map[int]bool{}
	var out []int
	for _, n := range in {
		if !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	sort.Ints(out)
	return out
}

func loc(file string, line int) string { return fmt.Sprintf("%s:%d", file, line) }

// truncStr truncates s to n runes for embedding in error messages.
func truncStr(s string, n int) string {
	rs := []rune(s)
	if len(rs) <= n {
		return s
	}
	return string(rs[:n-1]) + "…"
}

func intRangeStr(ids []int) string {
	if len(ids) > 12 {
		return fmt.Sprintf("%v … +%d more", ids[:12], len(ids)-12)
	}
	return fmt.Sprintf("%v", ids)
}

// ---------------------------------------------------------------------------
// Waivers
// ---------------------------------------------------------------------------

// loadWaivers parses the waiver file. A missing file is not an error (empty
// waiver set); malformed JSON or schema violations are.
func loadWaivers(path string) ([]TraceWaiver, error) {
	if path == "" {
		return nil, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read waivers: %w", err)
	}
	var wf waiverFile
	if err := json.Unmarshal(b, &wf); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return wf.Waivers, nil
}

// ---------------------------------------------------------------------------
// Persistence & drift
// ---------------------------------------------------------------------------

// LoadTraceMatrix parses a committed traceability.json.
func LoadTraceMatrix(path string) (*TraceMatrix, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var m TraceMatrix
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &m, nil
}

// WriteJSON renders the matrix as indented JSON.
func (m *TraceMatrix) WriteJSON(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}

// TraceDrift compares a committed matrix against the live one: criteria
// added/removed and per-criterion edge-set or contract changes. Each diff
// is a warn-severity matrix_drift defect — CI thus forces regeneration
// (`validator trace --write`) whenever docs move under the committed file.
func TraceDrift(committed, live *TraceMatrix) []TraceDefect {
	var out []TraceDefect
	drift := func(crit int, detail string) {
		out = append(out, TraceDefect{Kind: DefectMatrixDrift, Severity: SevWarn,
			Criterion: crit, Detail: detail})
	}
	old := map[int]TraceCriterion{}
	for _, c := range committed.Criteria {
		old[c.ID] = c
	}
	newIDs := map[int]bool{}
	for _, c := range live.Criteria {
		newIDs[c.ID] = true
		oc, ok := old[c.ID]
		if !ok {
			drift(c.ID, fmt.Sprintf("criterion %d added since committed matrix", c.ID))
			continue
		}
		if oc.Contract != c.Contract {
			drift(c.ID, fmt.Sprintf("criterion %d contract changed %q → %q", c.ID, oc.Contract, c.Contract))
		}
		if canonEdges(oc.Edges) != canonEdges(c.Edges) {
			drift(c.ID, fmt.Sprintf("criterion %d edge set changed", c.ID))
		}
	}
	for id := range old {
		if !newIDs[id] {
			drift(id, fmt.Sprintf("criterion %d removed since committed matrix", id))
		}
	}
	return out
}

// canonEdges canonicalizes an edge set for drift comparison.
func canonEdges(e TraceEdges) string {
	var b strings.Builder
	for _, s := range e.PhaseAC {
		b.WriteString("A:" + s + ";")
	}
	for _, s := range e.TaskAC {
		b.WriteString("T:" + s + ";")
	}
	for _, c := range e.Checkpoint {
		b.WriteString("C:" + c.ID + ";")
	}
	for _, g := range e.Golden {
		b.WriteString("G:" + g + ";")
	}
	return b.String()
}

// ---------------------------------------------------------------------------
// Markdown summary (tests/spec/traceability.md)
// ---------------------------------------------------------------------------

// Markdown renders the human-readable summary committed alongside the JSON.
func (m *TraceMatrix) Markdown() string {
	var b strings.Builder
	w := func(format string, a ...any) { fmt.Fprintf(&b, format, a...) }

	w("# §24 Acceptance Criteria → Test Traceability Matrix\n\n")
	w("Generated by `validator trace --write` (Phase-01.5 Task 1.5.3.3). DO NOT EDIT.\n\n")
	w("* spec: `%s` (§24)\n* generated: %s\n* expected criteria: %d (§24.3 declared total)\n\n",
		m.Spec, m.GeneratedAt.Format(time.RFC3339), m.Expected)

	s := m.Summary
	w("## Summary\n\n")
	w("| Metric | Value |\n|---|---|\n")
	w("| Criteria rows found | %d |\n", s.Criteria)
	w("| Contiguous 1..%d | %v |\n", s.DeclaredTotal, s.Contiguous)
	w("| Mapped (≥1 validation edge) | %d |\n", s.Mapped)
	w("| Unmapped | %d |\n", s.Unmapped)
	w("| Waived | %d |\n", s.Waived)
	w("| Fail-severity defects | %d |\n", s.FailDefects)
	w("| Warn-severity defects | %d |\n", s.WarnDefects)
	w("| Checkpoint edges (registered impl) | %d of %d |\n",
		s.CheckpointImpl, s.EdgeTotals["checkpoint"])
	for _, k := range []string{"phase_ac", "task_ac", "checkpoint", "golden", "spec_components"} {
		w("| Edges: %s | %d |\n", k, s.EdgeTotals[k])
	}
	w("\nEdge model: `phase_ac` = §24-declared `§AC row` refs resolved to real\n")
	w("phase-doc AC rows + AC rows citing `(§24 #N)` back; `task_ac` = task-level\n")
	w("DoD checkbox cites; `checkpoint` = SDD `Spec checkpoint:` lines citing\n")
	w("`§24 #N` bound to `P<phase>-T<task>-C<idx>` IDs; `golden` = GOLDEN-*\n")
	w("corpus cases citing `§24 #N`; `spec_components` = spec §27.2 catalog\n")
	w("references (informational — not counted toward mapped).\n\n")

	// Per-phase owner coverage.
	byPhase := map[string][]int{}
	for _, c := range m.Criteria {
		for _, d := range c.OwnerDocs {
			byPhase[d] = append(byPhase[d], c.ID)
		}
		if len(c.OwnerDocs) == 0 {
			byPhase["(unresolved owner)"] = append(byPhase["(unresolved owner)"], c.ID)
		}
	}
	var phases []string
	for p := range byPhase {
		phases = append(phases, p)
	}
	sort.Strings(phases)
	w("## Coverage by owner phase\n\n| Owner phase doc | Criteria |\n|---|---|\n")
	for _, p := range phases {
		w("| %s | %d |\n", p, len(byPhase[p]))
	}
	w("\n")

	// Defects.
	w("## Defects\n\n")
	if len(m.Defects) == 0 {
		w("none\n\n")
	} else {
		w("| Sev | Kind | Criterion | Location | Detail |\n|---|---|---|---|---|\n")
		for _, d := range m.Defects {
			crit := ""
			if d.Criterion > 0 {
				crit = strconv.Itoa(d.Criterion)
			}
			w("| %s | %s | %s | %s | %s |\n", d.Severity, d.Kind, crit, d.Location,
				strings.ReplaceAll(d.Detail, "|", "\\|"))
		}
		w("\n")
	}

	// Waivers.
	w("## Waivers\n\n")
	if len(m.Waivers) == 0 {
		w("none — waiver file `tests/spec/traceability.waivers.json` is empty.\n\n")
	} else {
		w("| Criterion | Reason | Added | Expires |\n|---|---|---|---|\n")
		for _, wv := range m.Waivers {
			w("| %d | %s | %s | %s |\n", wv.Criterion, wv.Reason, wv.Added, wv.Expires)
		}
		w("\n")
	}

	// Full per-criterion matrix.
	w("## Matrix\n\n")
	w("| # | Status | Contract | Owner docs | AC refs (declared) | Edges | Defects |\n")
	w("|---|---|---|---|---|---|---|\n")
	for _, c := range m.Criteria {
		var decl []string
		for _, r := range c.ACRefs {
			mark := "ok"
			if !r.Resolved {
				mark = "BROKEN"
			}
			decl = append(decl, fmt.Sprintf("%s rows %v [%s]",
				strings.TrimSuffix(r.Doc, ".md"), acRowNums(r.Rows), mark))
		}
		w("| %d | %s | `%s` | %s | %s | %d | %s |\n",
			c.ID, c.Status, c.Contract, strings.Join(c.OwnerDocs, ", "),
			strings.Join(decl, "; "), c.Edges.EdgeCount(), strings.Join(c.Defects, ", "))
	}
	return b.String()
}

func acRowNums(rows []TraceACRow) string {
	var out []string
	for _, r := range rows {
		out = append(out, strconv.Itoa(r.Row))
	}
	return strings.Join(out, ",")
}

// WriteMD writes the markdown summary.
func (m *TraceMatrix) WriteMD(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(m.Markdown()), 0o644)
}
