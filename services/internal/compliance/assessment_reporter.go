// FX Global Code assessment reporting — Phase-21 Task 21.3.17 item 3
// (spec §14.6): the Statement of Commitment generator and the exportable
// evidence-pack renderer the annual review publishes.
//
//   - GenerateStatement assembles the formal Statement of Commitment —
//     cover sheet (framework edition, review period, aggregate score,
//     verdict counts), the six-theme verdict table, every PARTIAL /
//     NON_ADHERENT principle with its evidence summary and remediation
//     ticket, and the non-adherent list the signatory explicitly
//     acknowledges. The body is sha256-pinned on the fx_gc_statements
//     row (migration 060) so the public-register entry cites an
//     immutable artefact.
//   - ExportReport renders the machine-readable evidence pack for the
//     audit/annual-review surface — run metadata + full 55-row matrix
//     with verdicts, automation flags and remediation refs.
//
// Both derive strictly from the committed verdict set — the artefact
// never mutates the matrix it reports on (spec §14.13 sibling edge
// case: evidence pack snapshots, never live edits).
package compliance

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"exchange/internal/audit"
	excerrors "exchange/pkg/errors"
)

// GenerateStatement builds and stores the Statement of Commitment for a
// run. Idempotent on run_id (the statements row is UNIQUE) — a replay
// keeps the first-generated artefact so a signed statement is never
// silently re-rendered under a changed hash.
func (s *FXGCService) GenerateStatement(ctx context.Context, runID int64) error {
	run, err := s.GetRun(ctx, runID)
	if err != nil {
		return err
	}
	rows, err := s.ListMatrix(ctx, runID)
	if err != nil {
		return err
	}
	body := renderStatement(run, rows, s.now())
	sha := statementHash(body)

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return excerrors.Wrap("INTERNAL_ERROR", "fxgc stmt tx", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var id int64
	err = tx.QueryRow(ctx, `
		INSERT INTO fx_gc_statements (run_id, code_version, period, body, body_sha256)
		VALUES ($1,$2,$3,$4,$5)
		ON CONFLICT (run_id) DO NOTHING
		RETURNING id`, runID, run.CodeVersion, run.Period, body, sha).Scan(&id)
	if err == pgx.ErrNoRows {
		// Idempotent replay — the original artefact stands.
		if err := tx.Commit(ctx); err != nil {
			return excerrors.Wrap("INTERNAL_ERROR", "fxgc stmt dedup commit", err)
		}
		return nil
	}
	if err != nil {
		return excerrors.Wrap("INTERNAL_ERROR", "fxgc stmt insert", err)
	}
	if _, err := audit.Append(ctx, tx, "fx_gc_statements", &id,
		"FXGC_STATEMENT", nil); err != nil {
		return excerrors.Wrap("INTERNAL_ERROR", "fxgc stmt audit", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return excerrors.Wrap("INTERNAL_ERROR", "fxgc stmt commit", err)
	}
	return nil
}

// themeOrder renders the six §14.6 themes in Global Code order.
var themeOrder = []string{
	ThemeEthics, ThemeGovernance, ThemeExecution,
	ThemeInformationSharing, ThemeRiskCompliance,
	ThemeConfirmationSettlement,
}

// themeLabel is the human-readable heading for the statement body.
var themeLabel = map[string]string{
	ThemeEthics:                 "Ethics",
	ThemeGovernance:             "Governance",
	ThemeExecution:              "Execution",
	ThemeInformationSharing:     "Information Sharing",
	ThemeRiskCompliance:         "Risk Management & Compliance",
	ThemeConfirmationSettlement: "Confirmation & Settlement",
}

// renderStatement assembles the formal Statement of Commitment text —
// a Disclosure Cover Sheet layout (spec §14.6) the executive signs and
// the venue publishes to the public register.
func renderStatement(run *AssessmentRun, rows []AssessmentRow,
	now time.Time) string {
	var b strings.Builder
	fmt.Fprintf(&b, "STATEMENT OF COMMITMENT TO THE FX GLOBAL CODE\n")
	fmt.Fprintf(&b, "============================================\n\n")
	fmt.Fprintf(&b, "Institution: the Exchange (venue operator)\n")
	fmt.Fprintf(&b, "Code edition: %s\n", run.CodeVersion)
	fmt.Fprintf(&b, "Review period: %s\n", run.Period)
	fmt.Fprintf(&b, "Generated: %s\n\n", now.UTC().Format(time.RFC3339))
	fmt.Fprintf(&b, "The Institution has undertaken a self-assessment of its "+
		"adherence to the %d principles of the FX Global Code "+
		"(\"the Code\") and has implemented the measures described "+
		"below to support the Code's objectives.\n\n", run.PrinciplesTotal)

	if run.Score != nil {
		fmt.Fprintf(&b, "Aggregate adherence score: %.2f%% of %d principles ADHERENT\n",
			*run.Score, run.PrinciplesTotal)
	}
	fmt.Fprintf(&b, "Verdict counts: ADHERENT=%d PARTIAL=%d NON_ADHERENT=%d PENDING=%d\n\n",
		run.PrinciplesAdherent, run.PrinciplesPartial,
		run.PrinciplesNon, run.PrinciplesPending)

	byTheme := map[string][]AssessmentRow{}
	for _, r := range rows {
		byTheme[r.Theme] = append(byTheme[r.Theme], r)
	}
	for _, theme := range themeOrder {
		themeRows := byTheme[theme]
		if len(themeRows) == 0 {
			continue
		}
		fmt.Fprintf(&b, "== %s (principles %d-%d) ==\n", themeLabel[theme],
			themeRows[0].PrincipleID, themeRows[len(themeRows)-1].PrincipleID)
		for _, r := range themeRows {
			title := fxGCPrincipleTitles[r.PrincipleID]
			how := "officer-assessed"
			if r.Automated {
				how = "engine-verified"
			}
			fmt.Fprintf(&b, "  P%-2d %-12s %s (%s)\n",
				r.PrincipleID, r.AdherenceStatus, title, how)
			if r.AdherenceStatus == AdherencePartial ||
				r.AdherenceStatus == AdherenceNon {
				fmt.Fprintf(&b, "       evidence: %s\n", r.EvidenceSummary)
				if r.RemediationRef != "" {
					fmt.Fprintf(&b, "       remediation: %s\n", r.RemediationRef)
				}
			}
		}
		b.WriteString("\n")
	}

	// The signatory explicitly acknowledges the non-adherent set.
	var gaps []int
	for _, r := range rows {
		if r.AdherenceStatus == AdherenceNon {
			gaps = append(gaps, r.PrincipleID)
		}
	}
	sort.Ints(gaps)
	if len(gaps) > 0 {
		fmt.Fprintf(&b, "The following principles are NON_ADHERENT and "+
			"carry remediation commitments acknowledged by the signatory: %v\n\n", gaps)
	} else {
		b.WriteString("No principle is assessed NON_ADHERENT for this period.\n\n")
	}
	b.WriteString("Signature: ____________________  (executive signatory)\n")
	b.WriteString("Date:      ____________________\n")
	return b.String()
}

// ---------------------------------------------------------------------------
// Exportable evidence pack
// ---------------------------------------------------------------------------

// ExportReport is the machine-readable annual-review export — run
// metadata plus the full matrix, the artefact the CCO annual report
// (Task 21.3.13) cites and the audit surface ships.
type ExportReport struct {
	Run        AssessmentRun   `json:"run"`
	Matrix     []AssessmentRow `json:"matrix"`
	Statement  *Statement      `json:"statement,omitempty"`
	ExportedAt time.Time       `json:"exported_at"`
}

// Export renders the evidence pack for a run — read-only; the live
// matrix is never touched.
func (s *FXGCService) Export(ctx context.Context, runID int64) (*ExportReport, error) {
	run, err := s.GetRun(ctx, runID)
	if err != nil {
		return nil, err
	}
	rows, err := s.ListMatrix(ctx, runID)
	if err != nil {
		return nil, err
	}
	if rows == nil {
		rows = []AssessmentRow{}
	}
	rep := &ExportReport{Run: *run, Matrix: rows, ExportedAt: s.now().UTC()}
	if stmt, serr := s.GetStatement(ctx, runID); serr == nil {
		rep.Statement = stmt
	}
	return rep, nil
}
