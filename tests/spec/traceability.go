// `validator trace` — §24 criteria→test traceability matrix (Phase-01.5
// Task 1.5.3.3). Builds the matrix from the spec + phase docs + registry,
// prints the report, and enforces the gate:
//
//	validator trace               — report; exit 1 on any fail-severity defect
//	validator trace --strict      — also fail on warn-severity defects
//	validator trace --write       — regenerate committed artifacts
//	                                (tests/spec/traceability.json + .md)
//	validator trace --waivers=…   — waiver file (default
//	                                tests/spec/traceability.waivers.json)
//	validator trace --expected=N  — override §24.3 declared criterion total
//
// Exit code is non-zero when any criterion is unmapped (zero edges to
// phase-AC / task-AC / checkpoint / golden) or any fail-severity defect is
// present — missing matrix columns, broken AC refs, colliding contract IDs,
// dangling §24 citations. Without --write the live matrix is diffed against
// the committed traceability.json; drift is a warning (fail under --strict).
package main

import (
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"

	spec "exchange-testspec/spec"
)

func cmdTrace(args []string) error {
	fs := flag.NewFlagSet("trace", flag.ContinueOnError)
	strict := fs.Bool("strict", false, "fail on warn-severity defects too (CI gate)")
	write := fs.Bool("write", false, "regenerate tests/spec/traceability.{json,md}")
	waivers := fs.String("waivers", "", "waiver file (default <root>/tests/spec/traceability.waivers.json)")
	expected := fs.Int("expected", 0, "declared criterion total (default: spec §24.3)")
	docs := fs.String("docs", "", "docs dir (default <root>/docs)")
	jsonOut := fs.String("json", "", "JSON output path (with --write; default tests/spec/traceability.json)")
	mdOut := fs.String("md", "", "markdown output path (with --write; default tests/spec/traceability.md)")
	showUnmapped := fs.Bool("unmapped", false, "print unmapped criteria verbosely")
	if err := fs.Parse(args); err != nil {
		return err
	}

	env := spec.DefaultEnv()
	if *docs != "" {
		env.DocsDir = *docs
	}
	reg := buildRegistry()

	waiverPath := *waivers
	if waiverPath == "" {
		waiverPath = env.Path("tests", "spec", "traceability.waivers.json")
	}
	committedPath := *jsonOut
	if committedPath == "" {
		committedPath = env.Path("tests", "spec", "traceability.json")
	}
	mdPath := *mdOut
	if mdPath == "" {
		mdPath = env.Path("tests", "spec", "traceability.md")
	}

	m, err := spec.BuildTraceability(env, reg, spec.TraceOptions{
		Expected:    *expected,
		WaiversPath: waiverPath,
	})
	if err != nil {
		return err
	}

	if *write {
		if err := m.WriteJSON(committedPath); err != nil {
			return err
		}
		if err := m.WriteMD(mdPath); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "wrote %s\nwrote %s\n", committedPath, mdPath)
	} else {
		// Drift vs the committed artifact — stale generated files are a warn
		// so the gate forces `trace --write` when docs legitimately change.
		old, lerr := spec.LoadTraceMatrix(committedPath)
		switch {
		case lerr != nil:
			m.Defects = append(m.Defects, spec.TraceDefect{
				Kind: "no_committed_matrix", Severity: spec.SevWarn, Location: committedPath,
				Detail: "no committed traceability.json — run `validator trace --write`",
			})
		default:
			m.Defects = append(m.Defects, spec.TraceDrift(old, m)...)
		}
		m.Recount()
	}

	printTraceReport(m)
	if *showUnmapped {
		printUnmapped(m)
	}
	os.Exit(m.ExitCode(*strict))
	return nil
}

// printTraceReport writes the summary to stderr and the per-criterion matrix
// to stdout (so `trace | grep` works without log noise).
func printTraceReport(m *spec.TraceMatrix) {
	s := m.Summary
	fmt.Fprintf(os.Stderr,
		"traceability: criteria=%d declared=%d contiguous=%v mapped=%d unmapped=%d waived=%d\n",
		s.Criteria, s.DeclaredTotal, s.Contiguous, s.Mapped, s.Unmapped, s.Waived)
	fmt.Fprintf(os.Stderr,
		"edges: phase_ac=%d task_ac=%d checkpoint=%d (impl %d) golden=%d spec_components=%d\n",
		s.EdgeTotals["phase_ac"], s.EdgeTotals["task_ac"], s.EdgeTotals["checkpoint"],
		s.CheckpointImpl, s.EdgeTotals["golden"], s.EdgeTotals["spec_components"])
	fmt.Fprintf(os.Stderr, "defects: fail=%d warn=%d\n", s.FailDefects, s.WarnDefects)

	// Defects sorted: fail first, then by criterion.
	defs := append([]spec.TraceDefect(nil), m.Defects...)
	sort.SliceStable(defs, func(i, j int) bool {
		if defs[i].Severity != defs[j].Severity {
			return defs[i].Severity == spec.SevFail
		}
		if defs[i].Criterion != defs[j].Criterion {
			return defs[i].Criterion < defs[j].Criterion
		}
		return defs[i].Kind < defs[j].Kind
	})
	for _, d := range defs {
		crit := ""
		if d.Criterion > 0 {
			crit = fmt.Sprintf(" #%d", d.Criterion)
		}
		fmt.Fprintf(os.Stderr, "  [%s] %-28s%s %s — %s\n",
			d.Severity, d.Kind, crit, d.Location, d.Detail)
	}

	// Compact table on stdout: only criteria needing attention (unmapped,
	// waived, or carrying defects) — the full 414-row matrix lives in
	// traceability.md / traceability.json.
	fmt.Printf("%-5s %-9s %-10s %-30s %-30s %-5s %s\n",
		"ID", "STATUS", "CONTRACT", "OWNERS", "EDGES(ac/task/ck/gld)", "N", "DEFECTS")
	for _, c := range m.Criteria {
		edges := fmt.Sprintf("%d/%d/%d/%d",
			len(c.Edges.PhaseAC), len(c.Edges.TaskAC),
			len(c.Edges.Checkpoint), len(c.Edges.Golden))
		row := fmt.Sprintf("%-5d %-9s %-10s %-30s %-30s %-5d %s",
			c.ID, c.Status, c.Contract, strings.Join(c.OwnerDocs, "+"),
			edges, c.Edges.EdgeCount(), strings.Join(c.Defects, ","))
		if c.Status != "mapped" || len(c.Defects) > 0 {
			fmt.Println(row)
		}
	}
}

// printUnmapped dumps full detail for every criterion with zero validation
// edges (the --unmapped diagnostic). Writes nothing when coverage is total.
func printUnmapped(m *spec.TraceMatrix) {
	n := 0
	for _, c := range m.Criteria {
		if c.Status != "unmapped" {
			continue
		}
		n++
		fmt.Printf("\n--- UNMAPPED #%d (spec line %d, %s) ---\n", c.ID, c.SpecLine, c.Subsection)
		fmt.Printf("criterion : %s\n", c.Text)
		fmt.Printf("owner     : %s → %v\n", c.OwnerRaw, c.OwnerDocs)
		fmt.Printf("ac refs   : %v\ncontract  : %s\n", c.ACRefs, c.Contract)
		fmt.Printf("edges     : %+v\ndefects   : %v\n", c.Edges, c.Defects)
	}
	if n == 0 {
		fmt.Fprintln(os.Stderr, "(no unmapped criteria — all 414 rows carry ≥1 validation edge)")
	}
}
