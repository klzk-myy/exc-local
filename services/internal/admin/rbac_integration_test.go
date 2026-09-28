// PostgreSQL integration tests — Phase-07 Tasks 7.3.1/7.3.2/7.3.11/7.3.12
// (migration 090 admin RBAC: scoped bindings, disjoint systems, dual
// control, recertification, break-glass).
//
// Gated on EXC_PG_TEST=1; targets EXC_TEST_DSN (default dev database).
// Anchors required: users, admin_audit_log, audit_hash_chain — a bare
// scratch DB without them skips cleanly.
package admin

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func ensureRBACSchema(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	for _, anchor := range []string{"users", "admin_audit_log", "audit_hash_chain"} {
		if !hasTable(t, ctx, pool, anchor) {
			t.Skipf("baseline table %s missing — target a migrated database", anchor)
		}
	}
	applyMigration(t, ctx, pool, "admin_role_bindings", "090_admin_rbac.up.sql")
}

// seedUsers inserts throwaway principals in the 9200xx range.
func seedUsers(t *testing.T, ctx context.Context, pool *pgxpool.Pool, ids ...int64) {
	t.Helper()
	for _, id := range ids {
		if _, err := pool.Exec(ctx, `
			INSERT INTO users (id, email)
			VALUES ($1, $2)
			ON CONFLICT (id) DO NOTHING`,
			id, fmt.Sprintf("rbac-it-%d@example.invalid", id)); err != nil {
			t.Fatalf("seed user %d: %v", id, err)
		}
	}
}

// bootstrapSuperAdmin seeds the root binding directly — the chicken/egg
// path a deployment seed performs (Grant requires a SA granter; the first
// SA can only be installed out-of-band, exactly like production).
func bootstrapSuperAdmin(t *testing.T, ctx context.Context, pool *pgxpool.Pool, uid int64) {
	t.Helper()
	if _, err := pool.Exec(ctx, `
		INSERT INTO admin_role_bindings (user_id, role, kind, granter_id, expires_at)
		VALUES ($1, 'Super Admin', 'STANDARD', $1, now() + interval '30 days')
		ON CONFLICT DO NOTHING`, uid); err != nil {
		t.Fatalf("bootstrap SA %d: %v", uid, err)
	}
}

func newTestSvc(pool *pgxpool.Pool) (*Service, *DualControlService, *Store, *[]int64, *[]string) {
	store := NewStore(pool)
	killed := &[]int64{}
	alerts := &[]string{}
	svc := NewService(pool, store,
		func(_ context.Context, uid int64) error {
			*killed = append(*killed, uid)
			return nil
		},
		func(_ context.Context, sev, sum string) error {
			*alerts = append(*alerts, sev+":"+sum)
			return nil
		})
	return svc, NewDualControlService(pool, store), store, killed, alerts
}

// cleanRBACRows makes the test re-runnable on a shared dev database.
func cleanRBACRows(t *testing.T, ctx context.Context, pool *pgxpool.Pool, ids []int64) {
	t.Helper()
	stmts := []string{
		`DELETE FROM admin_break_glass_grants WHERE grantee_id = ANY($1) OR granter_id = ANY($1)`,
		`DELETE FROM admin_recert_decisions WHERE binding_id IN
		    (SELECT id FROM admin_role_bindings WHERE user_id = ANY($1) OR granter_id = ANY($1))
		   OR user_id = ANY($1)`,
		`DELETE FROM admin_dual_control_requests WHERE requested_by = ANY($1) OR approved_by = ANY($1)`,
		`DELETE FROM admin_role_bindings WHERE user_id = ANY($1) OR granter_id = ANY($1)`,
		`DELETE FROM principal_role_systems WHERE principal_id = ANY($1)`,
	}
	for _, q := range stmts {
		if _, err := pool.Exec(ctx, q, ids); err != nil {
			t.Fatalf("cleanup: %v", err)
		}
	}
}

func TestRBACLifecycleIntegration(t *testing.T) {
	pool, ctx := pgGate(t)
	ensureRBACSchema(t, ctx, pool)

	sa, sa2, rmUser, finUser, supUser := int64(920001), int64(920002),
		int64(920003), int64(920004), int64(920005)
	all := []int64{sa, sa2, rmUser, finUser, supUser, 920006, 920007}
	seedUsers(t, ctx, pool, all...)
	cleanRBACRows(t, ctx, pool, all)
	svc, dual, store, killed, alerts := newTestSvc(pool)

	// Bootstrap: deployment-seeded SA binding (30d cap honored).
	bootstrapSuperAdmin(t, ctx, pool, sa)
	bootstrapSuperAdmin(t, ctx, pool, sa2)

	// --- Grant-time scope intersection (§8.2a.1): scoped granter cannot
	// widen. Narrow the second SA to env=dev via direct grant, then try
	// to grant staging — must fail FORBIDDEN.
	if _, err := svc.Grant(ctx, sa, GrantInput{
		UserID: sa2, Role: RoleRiskManager,
		Scope:     &Scope{Env: []string{"dev"}},
		ExpiresAt: time.Now().AddDate(0, 3, 0), Reason: "narrow sa2→rm dev",
	}, 0); err != nil {
		t.Fatalf("scoped grant: %v", err)
	}
	// rmUser: scoped grant from global SA.
	rm, err := svc.Grant(ctx, sa, GrantInput{
		UserID: rmUser, Role: RoleRiskManager,
		Scope:     &Scope{Desks: []string{"FX-SPOT"}, Env: []string{"dev", "staging"}},
		ExpiresAt: time.Now().AddDate(0, 6, 0), Reason: "rm scoped",
	}, 0)
	if err != nil {
		t.Fatalf("grant rm: %v", err)
	}
	if rm.Scope == nil || len(rm.Scope.Desks) != 1 || rm.Scope.Desks[0] != "FX-SPOT" {
		t.Fatalf("granted scope: %+v", rm.Scope)
	}

	// Non-SA cannot grant (UNAUTHORIZED_ROLE — role management is SA-only).
	if _, err := svc.Grant(ctx, rmUser, GrantInput{
		UserID: finUser, Role: RoleFinanceOps,
		ExpiresAt: time.Now().AddDate(0, 1, 0), Reason: "nope",
	}, 0); codeOf(t, err) != "UNAUTHORIZED_ROLE" {
		t.Fatalf("non-SA grant: want UNAUTHORIZED_ROLE, got %v", err)
	}

	// Caps: SA >90d rejected; >12mo rejected.
	if _, err := svc.Grant(ctx, sa, GrantInput{
		UserID: finUser, Role: RoleSuperAdmin,
		ExpiresAt: time.Now().AddDate(0, 4, 0), Reason: "cap",
	}, 0); codeOf(t, err) != "INVALID_REQUEST" {
		t.Fatalf("SA cap: want INVALID_REQUEST, got %v", err)
	}
	if _, err := svc.Grant(ctx, sa, GrantInput{
		UserID: finUser, Role: RoleFinanceOps,
		ExpiresAt: time.Now().AddDate(1, 1, 0), Reason: "cap",
	}, 0); codeOf(t, err) != "INVALID_REQUEST" {
		t.Fatalf("12mo cap: want INVALID_REQUEST, got %v", err)
	}

	// Resolver: rmUser resolves Risk Manager; unknown resolves "".
	if role, err := store.StrongestRole(ctx, rmUser); err != nil || role != RoleRiskManager {
		t.Fatalf("resolve rm: %q %v", role, err)
	}
	if role, err := store.StrongestRole(ctx, 999999); err != nil || role != "" {
		t.Fatalf("unknown user must resolve empty: %q %v", role, err)
	}

	// --- Disjoint systems (§8.2a.2): a CLIENT_DELEGATED principal cannot
	// receive a venue binding — the DB trigger raises the mapped error.
	extUser := int64(920006)
	bgUser := int64(920007)
	if _, err := pool.Exec(ctx, `
		INSERT INTO principal_role_systems (principal_id, role_system, first_granted_by)
		VALUES ($1, 'CLIENT_DELEGATED', $2)`, extUser, sa); err != nil {
		t.Fatalf("seed cross-system principal: %v", err)
	}
	if _, err := svc.Grant(ctx, sa, GrantInput{
		UserID: extUser, Role: RoleFinanceOps,
		ExpiresAt: time.Now().AddDate(0, 1, 0), Reason: "cross-system",
	}, 0); codeOf(t, err) != "FORBIDDEN" {
		t.Fatalf("cross-system grant: want FORBIDDEN, got %v", err)
	}

	// --- Dual control (§8.2): submit → approve by a distinct SA.
	req, err := dual.Submit(ctx, SubmitInput{
		Operation: OpAdminRoleChange, TargetType: "user",
		TargetID: fmt.Sprint(finUser),
		Payload: map[string]any{
			"action": "grant", "user_id": finUser, "role": RoleFinanceOps,
			"expires_at": time.Now().AddDate(0, 2, 0), "reason": "hired",
		},
		RequiredRole: RoleSuperAdmin, RequestedBy: sa, Reason: "hired",
	})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if req.Status != ReqPending {
		t.Fatalf("request state: %+v", req)
	}
	// Same-principal approval → DUAL_CONTROL_VIOLATION.
	if _, err := dual.Approve(ctx, req.ID, sa, ""); codeOf(t, err) != "DUAL_CONTROL_VIOLATION" {
		t.Fatalf("self-approve: want DUAL_CONTROL_VIOLATION, got %v", err)
	}
	// Ineligible approver (RM doesn't satisfy required SA) → UNAUTHORIZED_ROLE.
	if _, err := dual.Approve(ctx, req.ID, rmUser, ""); codeOf(t, err) != "UNAUTHORIZED_ROLE" {
		t.Fatalf("ineligible approver: want UNAUTHORIZED_ROLE, got %v", err)
	}
	// Distinct SA approves → request settles; an OpAdminRoleChange
	// executor is NOT registered here → lands APPROVED.
	settled, err := dual.Approve(ctx, req.ID, sa2, "198.51.100.9")
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	if settled.Status != ReqApproved || settled.ApprovedBy == nil || *settled.ApprovedBy != sa2 {
		t.Fatalf("settled: %+v", settled)
	}
	// Both participants audited.
	var n int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM admin_audit_log
		 WHERE target_type='dual_control_request' AND target_id=$1`, req.ID).Scan(&n); err != nil || n < 2 {
		t.Fatalf("dual-control audit rows: %d %v", n, err)
	}
	// Re-decide a settled request fails.
	if _, err := dual.Approve(ctx, req.ID, sa, ""); codeOf(t, err) != "INVALID_REQUEST" {
		t.Fatalf("re-decide: want INVALID_REQUEST, got %v", err)
	}
	// Window lapse: submit, force expiry, approve → DUAL_CONTROL_REQUIRED.
	req2, err := dual.Submit(ctx, SubmitInput{
		Operation: OpKillSwitch, TargetType: "system", TargetID: "all",
		Payload:      map[string]any{"scope": "all"},
		RequiredRole: RoleRiskManager, RequestedBy: rmUser, Reason: "drill",
	})
	if err != nil {
		t.Fatalf("submit2: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE admin_dual_control_requests SET expires_at = now() - interval '1 minute' WHERE id=$1`,
		req2.ID); err != nil {
		t.Fatalf("force expiry: %v", err)
	}
	if _, err := dual.Approve(ctx, req2.ID, sa, ""); codeOf(t, err) != "DUAL_CONTROL_REQUIRED" {
		t.Fatalf("expired approve: want DUAL_CONTROL_REQUIRED, got %v", err)
	}
	var st string
	_ = pool.QueryRow(ctx, `SELECT status FROM admin_dual_control_requests WHERE id=$1`,
		req2.ID).Scan(&st)
	if st != ReqExpired {
		t.Fatalf("expired row status: %s", st)
	}
	// Reject path.
	req3, err := dual.Submit(ctx, SubmitInput{
		Operation: OpKillSwitch, TargetType: "system", TargetID: "EURUSD",
		Payload: map[string]any{}, RequiredRole: RoleRiskManager,
		RequestedBy: rmUser, Reason: "drill2",
	})
	if err != nil {
		t.Fatalf("submit3: %v", err)
	}
	if rej, err := dual.Reject(ctx, req3.ID, sa, ""); err != nil || rej.Status != ReqRejected {
		t.Fatalf("reject: %+v %v", rej, err)
	}

	// --- Expiry + session kill (§8.2b.1).
	if _, err := pool.Exec(ctx,
		`UPDATE admin_role_bindings SET expires_at = now() - interval '1 second'
		  WHERE id=$1`, rm.ID); err != nil {
		t.Fatalf("backdate expiry: %v", err)
	}
	*killed = nil
	if n, err := svc.ExpireDue(ctx, 100); err != nil || n < 1 {
		t.Fatalf("expire sweep: n=%d err=%v", n, err)
	}
	if role, _ := store.StrongestRole(ctx, rmUser); role != "" {
		t.Fatalf("expired binding still resolves: %q", role)
	}
	gotKill := false
	for _, u := range *killed {
		if u == rmUser {
			gotKill = true
		}
	}
	if !gotKill {
		t.Fatalf("session kill not invoked for %d: %v", rmUser, *killed)
	}

	// --- Break-glass (§8.2b.2): dual-controlled ≤4h incident grant.
	g, err := svc.GrantBreakGlass(ctx, sa, BreakGlassInput{
		GranteeID: bgUser, IncidentRef: fmt.Sprintf("INC-%d", time.Now().Unix()),
		Reason: "production incident requires emergency access",
		TTL:    2 * time.Hour, SecondApproverID: sa2,
	})
	if err != nil {
		t.Fatalf("break-glass grant: %v", err)
	}
	if g.ExpiresAt.Sub(g.GrantedAt) > MaxBreakGlassTTL {
		t.Fatalf("timebox: %+v", g)
	}
	// Self-grant forbidden; missing second approver → DUAL_CONTROL_REQUIRED.
	if _, err := svc.GrantBreakGlass(ctx, sa, BreakGlassInput{
		GranteeID: sa, IncidentRef: "INC-X",
		Reason: "self grant attempt", TTL: time.Hour, SecondApproverID: sa2,
	}); codeOf(t, err) != "DUAL_CONTROL_VIOLATION" {
		t.Fatalf("self grant: %v", err)
	}
	if _, err := svc.GrantBreakGlass(ctx, sa, BreakGlassInput{
		GranteeID: supUser, IncidentRef: "INC-Y",
		Reason: "no second approver supplied", TTL: time.Hour,
	}); codeOf(t, err) != "DUAL_CONTROL_REQUIRED" {
		t.Fatalf("missing approver: %v", err)
	}
	// Unreachable-approver escape fires the P0 alert + audit watermark.
	// Unique incident ref keeps the watermark assertion stable across
	// re-runs on a shared database.
	incZ := fmt.Sprintf("INCZ-%d", time.Now().UnixNano())
	*alerts = nil
	g2, err := svc.GrantBreakGlass(ctx, sa, BreakGlassInput{
		GranteeID: supUser, IncidentRef: incZ,
		Reason: "sev1 — no approver reachable", TTL: time.Hour,
		UnreachableApprover: true,
	})
	if err != nil {
		t.Fatalf("solo break-glass: %v", err)
	}
	if !g2.P0Alert || len(*alerts) == 0 {
		t.Fatalf("P0 alert missing: %+v alerts=%v", g2, *alerts)
	}
	// Watermarked audit row exists.
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM admin_audit_log
		 WHERE action='rbac.break_glass.grant' AND target_id=$1
		   AND after_state->>'incident_ref'=$2`, supUser, incZ).Scan(&n); err != nil || n != 1 {
		t.Fatalf("break-glass watermark: n=%d err=%v", n, err)
	}
	// Grantee resolves Super Admin power while the grant lives.
	if role, _ := store.StrongestRole(ctx, bgUser); role != RoleSuperAdmin {
		t.Fatalf("break-glass resolve: %q", role)
	}
	// Post-review: grantee cannot self-review; another admin can.
	if err := svc.ReviewBreakGlass(ctx, g.ID, bgUser, "self", ""); codeOf(t, err) != "DUAL_CONTROL_VIOLATION" {
		t.Fatalf("self-review: %v", err)
	}
	if err := svc.ReviewBreakGlass(ctx, g.ID, sa2, "post-incident review ok", ""); err != nil {
		t.Fatalf("review: %v", err)
	}
	// Break-glass expiry sweep kills the binding + sessions.
	if _, err := pool.Exec(ctx,
		`UPDATE admin_break_glass_grants SET expires_at = now() - interval '1s' WHERE id=$1`,
		g2.ID); err != nil {
		t.Fatalf("backdate bg: %v", err)
	}
	*killed = nil
	if n, err := svc.ExpireBreakGlass(ctx, 50); err != nil || n < 1 {
		t.Fatalf("bg sweep: n=%d err=%v", n, err)
	}
	if role, _ := store.StrongestRole(ctx, supUser); role != "" {
		t.Fatalf("expired break-glass still resolves: %q", role)
	}

	// --- Recertification (§8.2b.1): campaign + overdue suspension.
	// label is VARCHAR(16) by schema — keep it short.
	camp, err := svc.StartCampaign(ctx, fmt.Sprintf("IT-%d", time.Now().Unix()%1000000),
		time.Now().Add(7*24*time.Hour), sa, "")
	if err != nil {
		t.Fatalf("campaign: %v", err)
	}
	// sa's own STANDARD binding is snapshot PENDING; approve it.
	rep, err := svc.CampaignReport(ctx, camp.ID)
	if err != nil {
		t.Fatalf("report: %v", err)
	}
	if len(rep["decisions"].([]map[string]any)) == 0 {
		t.Fatal("campaign must snapshot active bindings")
	}
	// Force a decision's suspend_at into the past, sweep → SUSPENDED +
	// session kill.
	var decBinding, decUser int64
	if err := pool.QueryRow(ctx, `
		UPDATE admin_recert_decisions SET suspend_at = now() - interval '1s'
		 WHERE id = (
		   SELECT id FROM admin_recert_decisions
		    WHERE campaign_id=$1 AND decision='PENDING'
		    ORDER BY id LIMIT 1)
		 RETURNING binding_id, user_id`, camp.ID).Scan(&decBinding, &decUser); err != nil {
		t.Fatalf("backdate decision: %v", err)
	}
	*killed = nil
	if n, err := svc.SuspendOverdueRecerts(ctx, 100); err != nil || n < 1 {
		t.Fatalf("recert sweep: n=%d err=%v", n, err)
	}
	var bStatus string
	_ = pool.QueryRow(ctx, `SELECT status FROM admin_role_bindings WHERE id=$1`,
		decBinding).Scan(&bStatus)
	if bStatus != StatusSuspended {
		t.Fatalf("overdue binding must suspend: %s", bStatus)
	}
	// Overdue-review enforcement: force g2 review_due_at past (unreviewed)
	// → granter's own bindings suspend.
	if _, err := pool.Exec(ctx,
		`UPDATE admin_break_glass_grants SET review_due_at = now() - interval '1s' WHERE id=$1`,
		g2.ID); err != nil {
		t.Fatalf("backdate review due: %v", err)
	}
	if n, err := svc.EnforceBreakGlassReview(ctx, 50); err != nil || n < 1 {
		t.Fatalf("review enforcement: n=%d err=%v", n, err)
	}
	var saStatus string
	_ = pool.QueryRow(ctx, `
		SELECT status FROM admin_role_bindings
		 WHERE user_id=$1 AND role='Super Admin' AND kind='STANDARD'
		 ORDER BY id DESC LIMIT 1`, sa).Scan(&saStatus)
	if saStatus != StatusSuspended && saStatus != StatusExpired {
		t.Fatalf("granter binding must suspend on overdue review: %q", saStatus)
	}
}

// TestRBACStoreErrors exercises fail-closed reads when the schema is
// absent — never a silent allow.
func TestRBACStoreFailsClosed(t *testing.T) {
	pool, ctx := pgGate(t)
	if hasTable(t, ctx, pool, "admin_role_bindings") {
		t.Skip("schema present — covered by the lifecycle test")
	}
	store := NewStore(pool)
	if _, err := store.ActiveBindings(ctx, 1); err == nil {
		t.Fatal("missing table must surface an error, not an empty allow")
	}
	if _, err := store.StrongestRole(ctx, 1); err == nil {
		t.Fatal("resolver must propagate store errors")
	}
}
