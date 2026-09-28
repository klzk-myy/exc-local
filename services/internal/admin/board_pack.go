// board_pack.go — Phase-07 Task 7.3.14, spec §7.6/§5.43, §24 #379.
//
// BOARD_QUARTERLY (and BOARD_ADHOC for emergency sessions) governance
// packs: the eight-section assembly runs the same live-source machinery
// as the CEO roll-up (command_pack.go), but board sections mark ABSENT —
// with owning task + narrative due date — when a source store is not
// provisioned, and release is dual-controlled and immutable:
//
//   - maker/checker: the releasing admin supplies a distinct, eligible
//     second approver (mirroring accounts.FreezeService's four-eyes
//     pattern); both ids land on the row, the schema CHECK enforces
//     distinctness, and the admin_audit_log + audit_hash_chain entries
//     commit in the same transaction;
//   - released packs are immutable — a BEFORE UPDATE/DELETE trigger on
//     governance_packs rejects every post-release mutation;
//   - content is hash-chained via prev_pack_hash and each section
//     records its source version so a pack is reproducible;
//   - Read-Only Auditor and EXTERNAL_AUDITOR read released packs (the
//     read gate lives in GovernancePackService.Get/List).
package admin

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	excerrors "exchange/pkg/errors"
)

// ---------------------------------------------------------------------------
// Dual-control seam
// ---------------------------------------------------------------------------

// DualControlApprover validates that approver is a distinct, eligible
// second principal for a board-pack release (spec §8.2 four-eyes). The
// default implementation resolves both roles through AdminRoleResolver;
// when Task 7.3.2's pending-approval queue lands it can substitute a
// queue-backed approver without touching this surface.
type DualControlApprover interface {
	ApproveRelease(ctx context.Context, initiator, approver int64) error
}

// roleDualControl is the in-request maker-checker approver: distinct
// non-zero approver who also resolves to an eligible release role.
type roleDualControl struct{ resolver AdminRoleResolver }

func (d roleDualControl) ApproveRelease(ctx context.Context, initiator, approver int64) error {
	if approver == 0 || approver == initiator {
		return excerrors.New("DUAL_CONTROL_REQUIRED",
			"board-pack release requires a distinct second approver (four-eyes)")
	}
	if d.resolver == nil {
		return excerrors.New("UNAUTHORIZED_ROLE",
			"role resolver not configured (Phase-07 RBAC seam)")
	}
	role, err := d.resolver(ctx, approver)
	if err != nil {
		return excerrors.Wrap("INTERNAL_ERROR", "approver role lookup", err)
	}
	if !packReleaseRoles[role] {
		return excerrors.New("UNAUTHORIZED_ROLE",
			fmt.Sprintf("release approver must hold Super Admin (got %q)", role))
	}
	return nil
}

// WithApprover substitutes the dual-control seam (unit tests / future
// approval-queue wiring). nil resets to the role-checking default.
func (s *GovernancePackService) WithApprover(d DualControlApprover) *GovernancePackService {
	if d == nil {
		d = roleDualControl{resolver: s.resolver}
	}
	s.approver = d
	return s
}

// ---------------------------------------------------------------------------
// Board generation + release
// ---------------------------------------------------------------------------

// GenerateBoard assembles a BOARD_QUARTERLY or BOARD_ADHOC pack —
// on-demand (emergency sessions use kind BOARD_ADHOC with an AdhocPeriod
// label) or scheduled quarterly. Same assembly path, same hash rule.
func (s *GovernancePackService) GenerateBoard(ctx context.Context, actor AdminActor,
	kind string, period PackPeriod) (*GovernancePack, error) {
	if kind != PackKindBoardQuarterly && kind != PackKindBoardAdhoc {
		return nil, excerrors.New("INVALID_REQUEST",
			"board generation accepts BOARD_QUARTERLY|BOARD_ADHOC")
	}
	return s.Generate(ctx, actor, kind, period)
}

// Release performs the dual-controlled, immutable release of a generated
// board pack. actor is the maker; actor.ApproverID is the checker
// (distinct, eligible — see DualControlApprover). After release the row
// is frozen by the schema trigger and its hash pinned.
func (s *GovernancePackService) Release(ctx context.Context, actor AdminActor,
	packID int64, reason string) (*GovernancePack, error) {
	if packID <= 0 {
		return nil, excerrors.New("INVALID_REQUEST", "pack_id must be a positive integer")
	}
	if actor.UserID == 0 {
		return nil, excerrors.New("UNAUTHORIZED", "admin identity required")
	}
	if s.resolver == nil {
		return nil, excerrors.New("UNAUTHORIZED_ROLE",
			"role resolver not configured (Phase-07 RBAC seam)")
	}
	role, err := s.resolver(ctx, actor.UserID)
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "role lookup", err)
	}
	if !packReleaseRoles[role] {
		return nil, excerrors.New("UNAUTHORIZED_ROLE",
			fmt.Sprintf("board-pack release requires Super Admin (got %q)", role))
	}
	approver := s.approver
	if approver == nil {
		approver = roleDualControl{resolver: s.resolver}
	}
	if err := approver.ApproveRelease(ctx, actor.UserID, actor.ApproverID); err != nil {
		return nil, err
	}
	return s.store.releasePack(ctx, packID, actor.UserID, actor.ApproverID,
		reason, actor.ClientIP)
}

// ---------------------------------------------------------------------------
// Board section sources (spec §7.6.2 — the eight sections)
// ---------------------------------------------------------------------------

// boardDefaultSources is the Task 7.3.14 section set. Each probe names
// the owning store; sections whose owners have not landed mark ABSENT
// with owner + due date (task edge case), never fail the pack.
func boardDefaultSources(pool *pgxpool.Pool) []PackSource {
	return []PackSource{
		sqlSectionSource{pool, "cco_report", "Phase-21 Task 21.3.15", "cco_reports",
			`SELECT to_jsonb(t) FROM (
			    SELECT count(*) AS reports, COALESCE(max(id),0) AS max_id
			      FROM cco_reports) t`},
		sqlSectionSource{pool, "finance_summary", "Phase-20 Task 20.3.7", "balances",
			`SELECT to_jsonb(t) FROM (
			    SELECT jsonb_object_agg(currency, tot) AS client_equity_by_currency,
			           (SELECT COALESCE(sum(buyer_fee+seller_fee),0)::text
			              FROM trades WHERE created_at >= $1 AND created_at < $2) AS fee_revenue_period,
			           (SELECT COALESCE(max(account_id),0) FROM balances) AS max_id
			      FROM (SELECT currency, sum(total)::text AS tot
			              FROM balances GROUP BY currency) x) t`},
		sqlSectionSource{pool, "margin_validation_insurance", "Phase-19 Tasks 19.3.13/19.3.14", "insurance_fund",
			`SELECT to_jsonb(t) FROM (
			    SELECT COALESCE(sum(balance),0)::text AS insurance_fund_total,
			           COALESCE(min(depletion_threshold),0)::text AS depletion_threshold,
			           (to_regclass('margin_validation_runs') IS NOT NULL) AS margin_validation_available,
			           COALESCE(max(id),0) AS max_id
			      FROM insurance_fund) t`},
		sqlSectionSource{pool, "incident_rca_log", "Phase-09 Task 9.3.18", "incidents",
			`SELECT to_jsonb(t) FROM (
			    SELECT count(*) FILTER (WHERE created_at >= $1 AND created_at < $2) AS incidents_in_period,
			           count(*) FILTER (WHERE rca_status = 'OPEN') AS open_rcas,
			           COALESCE(max(id),0) AS max_id
			      FROM incidents) t`},
		sqlSectionSource{pool, "bcp_exercise_status", "Phase-09 Task 9.3.27", "bcp_exercises",
			`SELECT to_jsonb(t) FROM (
			    SELECT count(*) FILTER (WHERE exercised_at >= $1 AND exercised_at < $2) AS exercises_in_period,
			           COALESCE(max(id),0) AS max_id
			      FROM bcp_exercises) t`},
		sqlSectionSource{pool, "audit_evidence", "Phase-24 Task 24.3.18", "audit_merkle_roots",
			`SELECT to_jsonb(t) FROM (
			    SELECT count(*) FILTER (WHERE date >= $1::date AND date < $2::date) AS daily_roots_in_period,
			           (SELECT merkle_root FROM audit_merkle_roots ORDER BY date DESC LIMIT 1) AS latest_root,
			           COALESCE(max(root_id),0) AS max_id
			      FROM audit_merkle_roots) t`},
		sqlSectionSource{pool, "promotions_summary", "Phase-21 Task 21.3.26", "financial_promotions",
			`SELECT to_jsonb(t) FROM (
			    SELECT count(*) AS promotions,
			           count(*) FILTER (WHERE status = 'APPROVED') AS approved,
			           COALESCE(max(id),0) AS max_id
			      FROM financial_promotions) t`},
		sqlSectionSource{pool, "regulatory_change_impacts", "Phase-21 Task 21.3.25", "regulatory_changes",
			`SELECT to_jsonb(t) FROM (
			    SELECT count(*) AS changes,
			           count(*) FILTER (WHERE impact_assessed_at IS NOT NULL) AS assessed,
			           COALESCE(max(id),0) AS max_id
			      FROM regulatory_changes) t`},
	}
}
