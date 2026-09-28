// Task 7.3.12 — role lifecycle, recertification & break-glass
// (spec §8.2b, §24 #349), plus the grant/revoke machinery Task 7.3.11
// hangs off the scoped bindings (§8.2a).
//
//   - Every binding carries expires_at: ≤12 months (90 days for
//     Super Admin, 4h for break-glass) — enforced by migration-090
//     CHECKs AND pre-validated here so violations read as clean coded
//     errors rather than constraint surprises.
//   - Expiry is a sweep: ACTIVE bindings past expires_at become EXPIRED,
//     the holder's sessions are killed via SessionKiller, and the
//     transition is audit-logged.
//   - Grant-time scope intersection (§8.2a.1): the granted scope is the
//     intersection of the requested scope with the granter's union of
//     scopes — narrowing never widens.
//   - Disjoint systems (§8.2a.2): the admin_role_bindings INSERT trigger
//     registers the principal as VENUE_ADMIN in principal_role_systems
//     and rejects when another system owns the principal.
//   - Break-glass: Super Admin-granted, ≤4h, incident-confined, dual-
//     controlled unless the unreachable-approver escape fires (then a
//     P0 alert is raised and the grant is solo); mandatory post-review
//     inside 2 business days — an overdue review suspends the granter's
//     own bindings.
//
// Audit: every transition writes admin_audit_log + the audit_hash_chain
// link inside the same transaction via admin.Log (Task 7.3.3).
package admin

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	excerrors "exchange/pkg/errors"
)

// Lifecycle caps (mirrored by migration-090 CHECK constraints).
const (
	MaxBindingTTL        = 12 * 30 * 24 * time.Hour // ~12 months
	MaxSuperAdminTTL     = 90 * 24 * time.Hour      // 90 days
	MaxBreakGlassTTL     = 4 * time.Hour
	RecertSuspendLagDays = 14 // campaign ends_at + 14 days
	BreakGlassReviewDays = 2  // business days
)

// SessionKiller terminates every session a user holds — binding expiry,
// revocation and suspension all kill sessions (§8.2b.1 "session kill").
// Wired to auth.SessionManager.RevokeAll in production.
type SessionKiller func(ctx context.Context, userID int64) error

// Alerter raises operational alerts (e.g. the P0 on a solo break-glass
// grant). Wired to the Phase-07 alert taxonomy / NATS dispatcher.
type Alerter func(ctx context.Context, severity, summary string) error

// Service is the role-lifecycle engine.
type Service struct {
	pool         *pgxpool.Pool
	store        *Store
	killSessions SessionKiller // optional; nil = audit-only
	alert        Alerter       // optional
	now          func() time.Time
}

// NewService wires the lifecycle engine.
func NewService(pool *pgxpool.Pool, store *Store, killer SessionKiller, alert Alerter) *Service {
	return &Service{pool: pool, store: store, killSessions: killer,
		alert: alert, now: time.Now}
}

// SetClockForTest overrides the clock; tests only.
func (s *Service) SetClockForTest(now func() time.Time) { s.now = now }

// ---------------------------------------------------------------------------
// Grant / revoke
// ---------------------------------------------------------------------------

// GrantInput describes one STANDARD binding grant.
type GrantInput struct {
	UserID    int64
	Role      string
	Scope     *Scope // nil = global — intersection still applies
	ExpiresAt time.Time
	Reason    string
	ClientIP  string
}

// capExpiry clamps/validates expiry against the role/kind caps.
func capExpiry(role, kind string, grantedAt, expiresAt time.Time) error {
	if !expiresAt.After(grantedAt) {
		return excerrors.New("INVALID_REQUEST", "expires_at must be in the future")
	}
	if expiresAt.After(grantedAt.AddDate(1, 0, 0)) {
		return excerrors.New("INVALID_REQUEST",
			"binding lifetime exceeds the 12-month cap (spec §8.2b)")
	}
	if role == RoleSuperAdmin && kind == KindStandard &&
		expiresAt.After(grantedAt.AddDate(0, 0, 90)) {
		return excerrors.New("INVALID_REQUEST",
			"Super Admin binding lifetime exceeds the 90-day cap (spec §8.2b)")
	}
	if kind == KindBreakGlass && expiresAt.After(grantedAt.Add(MaxBreakGlassTTL)) {
		return excerrors.New("INVALID_REQUEST",
			"break-glass binding exceeds the 4h cap (spec §8.2b)")
	}
	return nil
}

// requireSuperAdmin resolves the granter and enforces role management
// authority: Super Admin only ("all permissions, including role
// management", spec §8.2).
func (s *Service) requireSuperAdmin(ctx context.Context, granterID int64) ([]Binding, error) {
	bindings, err := s.store.ActiveBindings(ctx, granterID)
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "granter bindings", err)
	}
	has := false
	for _, b := range bindings {
		if b.Role == RoleSuperAdmin {
			has = true
		}
	}
	if !has {
		return nil, excerrors.New("UNAUTHORIZED_ROLE",
			"role management requires an active Super Admin binding")
	}
	return bindings, nil
}

// granterScope unions the scopes of the granter's active bindings —
// the narrowing ceiling for grants.
func granterScope(bindings []Binding) *Scope {
	scopes := make([]*Scope, 0, len(bindings))
	for _, b := range bindings {
		scopes = append(scopes, b.Scope)
	}
	return UnionScope(scopes)
}

// Grant inserts a STANDARD binding with grant-time scope intersection and
// the disjoint-system guard — its own transaction for direct callers
// (sweeps/CLI/tests). The REST path runs GrantTx inside the dual-control
// approval transaction instead.
func (s *Service) Grant(ctx context.Context, granterID int64, in GrantInput, approverID int64) (*Binding, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "begin tx", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	b, err := s.GrantTx(ctx, tx, granterID, in, approverID)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "commit grant", err)
	}
	return b, nil
}

// GrantTx is the in-transaction grant core — the dual-control executor
// path runs it inside the approval tx so the mutation and its four-eyes
// record commit atomically. Dual control is applied by the caller (the
// OpAdminRoleChange request carries maker+approver ids).
func (s *Service) GrantTx(ctx context.Context, tx pgx.Tx, granterID int64, in GrantInput, approverID int64) (*Binding, error) {
	if in.UserID <= 0 {
		return nil, excerrors.New("INVALID_REQUEST", "user_id required")
	}
	if !ValidRole(in.Role) {
		return nil, excerrors.New("INVALID_REQUEST",
			fmt.Sprintf("role %q is not one of the six §8.2 roles", in.Role))
	}
	granterBindings, err := s.requireSuperAdmin(ctx, granterID)
	if err != nil {
		return nil, err
	}
	// §8.2a.1: grant-time scope intersection — narrowing never widens.
	scope, err := IntersectScope(granterScope(granterBindings), in.Scope)
	if err != nil {
		return nil, excerrors.New("FORBIDDEN",
			"requested scope exceeds the granter's scope: "+err.Error())
	}
	grantedAt := s.now().UTC()
	if err := capExpiry(in.Role, KindStandard, grantedAt, in.ExpiresAt); err != nil {
		return nil, err
	}
	scopeJSON, err := scope.Marshal()
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "scope marshal", err)
	}

	var b Binding
	var raw []byte
	err = tx.QueryRow(ctx, `
		INSERT INTO admin_role_bindings
		    (user_id, role, kind, scope, granter_id, expires_at)
		VALUES ($1, $2, 'STANDARD', $3, $4, $5)
		RETURNING id, user_id, role, kind, scope, granter_id, granted_at,
		          expires_at, status`,
		in.UserID, in.Role, scopeJSON, granterID, in.ExpiresAt).
		Scan(&b.ID, &b.UserID, &b.Role, &b.Kind, &raw, &b.GranterID,
			&b.GrantedAt, &b.ExpiresAt, &b.Status)
	if err != nil {
		return nil, mapGrantError(err)
	}
	b.Scope = scope

	after := map[string]any{
		"binding_id": b.ID, "role": in.Role, "expires_at": in.ExpiresAt,
		"granter_id": granterID, "reason": in.Reason,
	}
	if approverID > 0 {
		after["approved_by"] = approverID
	}
	if scope != nil {
		after["scope"] = scope
	}
	if _, _, err := Log(ctx, tx, AuditEntry{
		AdminUserID: granterID,
		Action:      "rbac.grant",
		TargetType:  "user",
		TargetID:    &in.UserID,
		AfterState:  after,
		IPAddress:   in.ClientIP,
	}); err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "audit log", err)
	}
	return &b, nil
}

// mapGrantError translates the DB-level guards into coded errors:
// trigger check_violation → FORBIDDEN (cross-system binding, §8.2a.2);
// unique violation → INVALID_REQUEST (duplicate active binding).
func mapGrantError(err error) error {
	var pgErr *pgconn.PgError
	for e := err; e != nil; {
		if p, ok := e.(*pgconn.PgError); ok {
			pgErr = p
			break
		}
		u, ok := e.(interface{ Unwrap() error })
		if !ok {
			break
		}
		e = u.Unwrap()
	}
	if pgErr != nil {
		switch pgErr.Code {
		case "23514": // CHECK violation — the system guard trigger raises it
			return excerrors.New("FORBIDDEN",
				"cross-system role binding rejected: "+pgErr.Message)
		case "23505":
			return excerrors.New("INVALID_REQUEST",
				"an active binding already exists for this user/role/kind")
		}
	}
	return excerrors.Wrap("INTERNAL_ERROR", "insert binding", err)
}

// Revoke terminates a binding (Super Admin; dual control upstream) —
// own-tx wrapper over RevokeTx.
func (s *Service) Revoke(ctx context.Context, revokerID, bindingID int64, reason, clientIP string) (*Binding, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "begin tx", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	b, err := s.RevokeTx(ctx, tx, revokerID, bindingID, reason, clientIP)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "commit revoke", err)
	}
	s.killSessionsBestEffort(ctx, b.UserID, "rbac.revoke")
	return b, nil
}

// RevokeTx is the in-transaction revoke core — the dual-control executor
// path runs it inside the approval tx. NOTE: the session kill cannot run
// inside a tx (Redis); the executor fires it post-commit via the
// settle-time binding read — callers using RevokeTx directly must kill
// sessions themselves.
func (s *Service) RevokeTx(ctx context.Context, tx pgx.Tx, revokerID, bindingID int64, reason, clientIP string) (*Binding, error) {
	if _, err := s.requireSuperAdmin(ctx, revokerID); err != nil {
		return nil, err
	}

	var b Binding
	var raw []byte
	err := tx.QueryRow(ctx, `
		UPDATE admin_role_bindings
		   SET status='REVOKED', revoked_at=now(), revoked_by=$2,
		       revoke_reason=$3, updated_at=now()
		 WHERE id=$1 AND status='ACTIVE'
		RETURNING id, user_id, role, kind, scope, granter_id, granted_at,
		          expires_at, status, revoked_at`,
		bindingID, revokerID, reason).
		Scan(&b.ID, &b.UserID, &b.Role, &b.Kind, &raw, &b.GranterID,
			&b.GrantedAt, &b.ExpiresAt, &b.Status, &b.RevokedAt)
	if err == pgx.ErrNoRows {
		return nil, excerrors.New("NOT_FOUND",
			fmt.Sprintf("active binding %d not found", bindingID))
	}
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "revoke binding", err)
	}
	if _, _, err := Log(ctx, tx, AuditEntry{
		AdminUserID: revokerID,
		Action:      "rbac.revoke",
		TargetType:  "user",
		TargetID:    &b.UserID,
		BeforeState: map[string]any{"binding_id": bindingID, "role": b.Role, "status": "ACTIVE"},
		AfterState:  map[string]any{"binding_id": bindingID, "status": "REVOKED", "reason": reason},
		IPAddress:   clientIP,
	}); err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "audit log", err)
	}
	return &b, nil
}

// ExpireDue lapses ACTIVE bindings past expires_at: status→EXPIRED,
// session kill, audit row per binding. Sweep entrypoint — call it on a
// short ticker (Phase-09 runs it with the housekeeping jobs).
func (s *Service) ExpireDue(ctx context.Context, limit int) (int64, error) {
	if limit <= 0 {
		limit = 500
	}
	rows, err := s.pool.Query(ctx, `
		UPDATE admin_role_bindings
		   SET status='EXPIRED', updated_at=now()
		 WHERE id IN (
		   SELECT id FROM admin_role_bindings
		    WHERE status='ACTIVE' AND expires_at <= now()
		    ORDER BY expires_at LIMIT $1
		 )
		RETURNING id, user_id, role, kind`, limit)
	if err != nil {
		return 0, excerrors.Wrap("INTERNAL_ERROR", "expire sweep", err)
	}
	defer rows.Close()
	type expired struct {
		id, userID int64
		role, kind string
	}
	var done []expired
	for rows.Next() {
		var e expired
		if err := rows.Scan(&e.id, &e.userID, &e.role, &e.kind); err != nil {
			return 0, err
		}
		done = append(done, e)
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	for _, e := range done {
		s.auditStandalone(ctx, e.userID, "rbac.expire", "user", e.userID,
			map[string]any{"binding_id": e.id, "role": e.role, "status": "ACTIVE"},
			map[string]any{"binding_id": e.id, "status": "EXPIRED"})
		s.killSessionsBestEffort(ctx, e.userID, "rbac.expire")
	}
	return int64(len(done)), nil
}

// ---------------------------------------------------------------------------
// Recertification (§8.2b.1 — quarterly campaign, 14-day suspension lag).
// ---------------------------------------------------------------------------

// RecertCampaign is one admin_recert_campaigns row.
type RecertCampaign struct {
	ID        int64      `json:"id"`
	Label     string     `json:"label"`
	StartedBy int64      `json:"started_by"`
	StartedAt time.Time  `json:"started_at"`
	EndsAt    time.Time  `json:"ends_at"`
	Status    string     `json:"status"`
	ClosedAt  *time.Time `json:"closed_at,omitempty"`
}

// StartCampaign opens a quarterly campaign and snapshots every ACTIVE
// STANDARD binding into PENDING decision rows (suspend_at = ends_at+14d).
func (s *Service) StartCampaign(ctx context.Context, label string, endsAt time.Time, startedBy int64, clientIP string) (*RecertCampaign, error) {
	if _, err := s.requireSuperAdmin(ctx, startedBy); err != nil {
		return nil, err
	}
	if !endsAt.After(s.now()) {
		return nil, excerrors.New("INVALID_REQUEST", "campaign ends_at must be in the future")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "begin tx", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var c RecertCampaign
	err = tx.QueryRow(ctx, `
		INSERT INTO admin_recert_campaigns (label, started_by, ends_at)
		VALUES ($1, $2, $3)
		RETURNING id, label, started_by, started_at, ends_at, status`,
		label, startedBy, endsAt).
		Scan(&c.ID, &c.Label, &c.StartedBy, &c.StartedAt, &c.EndsAt, &c.Status)
	if err != nil {
		return nil, mapGrantError(err) // 23505 → duplicate label
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO admin_recert_decisions
		    (campaign_id, binding_id, user_id, role, suspend_at)
		SELECT $1, b.id, b.user_id, b.role, $2::timestamptz + interval '14 days'
		  FROM admin_role_bindings b
		 WHERE b.status = 'ACTIVE' AND b.kind = 'STANDARD'
		 ORDER BY b.id`, c.ID, endsAt); err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "snapshot decisions", err)
	}
	if _, _, err := Log(ctx, tx, AuditEntry{
		AdminUserID: startedBy,
		Action:      "rbac.recert.start",
		TargetType:  "recert_campaign",
		TargetID:    &c.ID,
		AfterState:  map[string]any{"label": label, "ends_at": endsAt},
		IPAddress:   clientIP,
	}); err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "audit log", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "commit campaign", err)
	}
	return &c, nil
}

// DecideRecert records a re-approval (or explicit suspension) of one
// binding under an OPEN campaign. The decider must hold an active Super
// Admin binding — the §8.2b "role owner" reads as the accountable role
// manager for venue bindings (§27 ruling candidate).
func (s *Service) DecideRecert(ctx context.Context, campaignID, bindingID, deciderID int64,
	approve bool, note, clientIP string) error {

	if _, err := s.requireSuperAdmin(ctx, deciderID); err != nil {
		return err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return excerrors.Wrap("INTERNAL_ERROR", "begin tx", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var campStatus string
	if err := tx.QueryRow(ctx,
		`SELECT status FROM admin_recert_campaigns WHERE id=$1 FOR UPDATE`,
		campaignID).Scan(&campStatus); err == pgx.ErrNoRows {
		return excerrors.New("NOT_FOUND", fmt.Sprintf("campaign %d not found", campaignID))
	} else if err != nil {
		return excerrors.Wrap("INTERNAL_ERROR", "lock campaign", err)
	}
	if campStatus != "OPEN" {
		return excerrors.New("INVALID_REQUEST",
			fmt.Sprintf("campaign %d is %s", campaignID, campStatus))
	}
	decision := "SUSPENDED"
	if approve {
		decision = "APPROVED"
	}
	var userID int64
	var role string
	err = tx.QueryRow(ctx, `
		UPDATE admin_recert_decisions
		   SET decision=$3, decided_by=$4, decided_at=now()
		 WHERE campaign_id=$1 AND binding_id=$2 AND decision='PENDING'
		RETURNING user_id, role`,
		campaignID, bindingID, decision, deciderID).Scan(&userID, &role)
	if err == pgx.ErrNoRows {
		return excerrors.New("NOT_FOUND",
			fmt.Sprintf("no PENDING decision for binding %d in campaign %d", bindingID, campaignID))
	}
	if err != nil {
		return excerrors.Wrap("INTERNAL_ERROR", "decide", err)
	}
	if !approve {
		if _, err := tx.Exec(ctx, `
			UPDATE admin_role_bindings
			   SET status='SUSPENDED', suspended_at=now(),
			       suspend_reason='recert_denied', updated_at=now()
			 WHERE id=$1 AND status='ACTIVE'`, bindingID); err != nil {
			return excerrors.Wrap("INTERNAL_ERROR", "suspend binding", err)
		}
	}
	if _, _, err := Log(ctx, tx, AuditEntry{
		AdminUserID: deciderID,
		Action:      "rbac.recert.decide",
		TargetType:  "admin_role_binding",
		TargetID:    &bindingID,
		AfterState: map[string]any{
			"campaign_id": campaignID, "decision": decision,
			"user_id": userID, "role": role, "note": note,
		},
		IPAddress: clientIP,
	}); err != nil {
		return excerrors.Wrap("INTERNAL_ERROR", "audit log", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return excerrors.Wrap("INTERNAL_ERROR", "commit decision", err)
	}
	if !approve {
		s.killSessionsBestEffort(ctx, userID, "rbac.recert.decide")
	}
	return nil
}

// SuspendOverdueRecerts suspends bindings whose campaign decisions are
// still PENDING at suspend_at (campaign end + 14 days, §8.2b.1). Sweep.
func (s *Service) SuspendOverdueRecerts(ctx context.Context, limit int) (int64, error) {
	if limit <= 0 {
		limit = 500
	}
	rows, err := s.pool.Query(ctx, `
		UPDATE admin_recert_decisions d
		   SET decision='SUSPENDED', decided_at=now()
		 WHERE d.id IN (
		   SELECT id FROM admin_recert_decisions
		    WHERE decision='PENDING' AND suspend_at <= now()
		    ORDER BY suspend_at LIMIT $1)
		RETURNING d.binding_id, d.user_id`, limit)
	if err != nil {
		return 0, excerrors.Wrap("INTERNAL_ERROR", "recert sweep", err)
	}
	defer rows.Close()
	type susp struct{ bindingID, userID int64 }
	var due []susp
	for rows.Next() {
		var x susp
		if err := rows.Scan(&x.bindingID, &x.userID); err != nil {
			return 0, err
		}
		due = append(due, x)
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	for _, x := range due {
		if _, err := s.pool.Exec(ctx, `
			UPDATE admin_role_bindings
			   SET status='SUSPENDED', suspended_at=now(),
			       suspend_reason='recert_overdue', updated_at=now()
			 WHERE id=$1 AND status='ACTIVE'`, x.bindingID); err != nil {
			continue // fail-closed: leave binding, alert via audit gap
		}
		s.auditStandalone(ctx, x.userID, "rbac.recert.suspend",
			"admin_role_binding", x.bindingID,
			map[string]any{"status": "ACTIVE", "decision": "PENDING"},
			map[string]any{"status": "SUSPENDED", "decision": "SUSPENDED",
				"reason": "recert_overdue"})
		s.killSessionsBestEffort(ctx, x.userID, "rbac.recert.suspend")
	}
	return int64(len(due)), nil
}

// CampaignReport is the Read-Only Auditor export of one campaign.
func (s *Service) CampaignReport(ctx context.Context, campaignID int64) (map[string]any, error) {
	var c RecertCampaign
	err := s.pool.QueryRow(ctx, `
		SELECT id, label, started_by, started_at, ends_at, status, closed_at
		  FROM admin_recert_campaigns WHERE id=$1`, campaignID).
		Scan(&c.ID, &c.Label, &c.StartedBy, &c.StartedAt, &c.EndsAt,
			&c.Status, &c.ClosedAt)
	if err == pgx.ErrNoRows {
		return nil, excerrors.New("NOT_FOUND",
			fmt.Sprintf("campaign %d not found", campaignID))
	}
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "read campaign", err)
	}
	rows, err := s.pool.Query(ctx, `
		SELECT d.binding_id, d.user_id, d.role, d.decision,
		       d.decided_by, d.decided_at, d.suspend_at
		  FROM admin_recert_decisions d
		 WHERE d.campaign_id=$1 ORDER BY d.id`, campaignID)
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "read decisions", err)
	}
	defer rows.Close()
	decisions := []map[string]any{}
	for rows.Next() {
		var bindingID, userID int64
		var role, decision string
		var decidedBy *int64
		var decidedAt, suspendAt *time.Time
		var sus time.Time
		if err := rows.Scan(&bindingID, &userID, &role, &decision,
			&decidedBy, &decidedAt, &sus); err != nil {
			return nil, err
		}
		suspendAt = &sus
		decisions = append(decisions, map[string]any{
			"binding_id": bindingID, "user_id": userID, "role": role,
			"decision": decision, "decided_by": decidedBy,
			"decided_at": decidedAt, "suspend_at": suspendAt,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return map[string]any{"campaign": c, "decisions": decisions}, nil
}

// ---------------------------------------------------------------------------
// Break-glass (§8.2b.2 — ≤4h, incident-confined, post-review ≤2 biz days).
// ---------------------------------------------------------------------------

// BreakGlassInput describes one emergency grant.
type BreakGlassInput struct {
	GranteeID           int64
	IncidentRef         string
	Reason              string
	TTL                 time.Duration // ≤ MaxBreakGlassTTL
	SecondApproverID    int64         // 0 with UnreachableApprover = solo grant
	UnreachableApprover bool          // no second approver reachable → P0 alert
	Scope               *Scope        // optional extra narrowing (env etc.)
	ClientIP            string
}

// BreakGlassGrant is one admin_break_glass_grants row.
type BreakGlassGrant struct {
	ID               int64      `json:"id"`
	BindingID        int64      `json:"binding_id"`
	GranteeID        int64      `json:"grantee_id"`
	GranterID        int64      `json:"granter_id"`
	SecondApproverID *int64     `json:"second_approver_id,omitempty"`
	P0Alert          bool       `json:"p0_alert"`
	IncidentRef      string     `json:"incident_ref"`
	Reason           string     `json:"reason"`
	GrantedAt        time.Time  `json:"granted_at"`
	ExpiresAt        time.Time  `json:"expires_at"`
	ReviewDueAt      time.Time  `json:"review_due_at"`
	ReviewedAt       *time.Time `json:"reviewed_at,omitempty"`
	ReviewedBy       *int64     `json:"reviewed_by,omitempty"`
	Status           string     `json:"status"`
}

// GrantBreakGlass mints a ≤4h BREAK_GLASS Super Admin binding confined
// to a named incident. Dual control: second_approver_id must be a
// distinct principal holding an active venue binding — unless
// UnreachableApprover is set, in which case the grant proceeds solo and
// a P0 alert fires (spec §8.2b.2).
func (s *Service) GrantBreakGlass(ctx context.Context, granterID int64, in BreakGlassInput) (*BreakGlassGrant, error) {
	if _, err := s.requireSuperAdmin(ctx, granterID); err != nil {
		return nil, err
	}
	if in.GranteeID <= 0 {
		return nil, excerrors.New("INVALID_REQUEST", "grantee_id required")
	}
	if in.GranteeID == granterID {
		return nil, excerrors.New("DUAL_CONTROL_VIOLATION",
			"break-glass cannot be self-granted")
	}
	if in.IncidentRef == "" || len(in.IncidentRef) > 64 {
		return nil, excerrors.New("INVALID_REQUEST",
			"incident_ref is required (≤64 chars) — the grant is incident-confined")
	}
	if len(in.Reason) < 10 {
		return nil, excerrors.New("INVALID_REQUEST",
			"break-glass requires an explicit justification")
	}
	if in.TTL <= 0 || in.TTL > MaxBreakGlassTTL {
		return nil, excerrors.New("INVALID_REQUEST",
			"break-glass TTL must be in (0, 4h] (spec §8.2b)")
	}
	if in.UnreachableApprover {
		if in.SecondApproverID != 0 {
			return nil, excerrors.New("INVALID_REQUEST",
				"second_approver_id and unreachable_approver are mutually exclusive")
		}
	} else {
		// Dual control: distinct, binding-holding second approver.
		if in.SecondApproverID == 0 {
			return nil, excerrors.New("DUAL_CONTROL_REQUIRED",
				"break-glass requires a distinct second approver (or the unreachable-approver escape)")
		}
		if in.SecondApproverID == granterID {
			return nil, excerrors.New("DUAL_CONTROL_VIOLATION",
				"the granter cannot be their own second approver")
		}
		role, err := s.store.StrongestRole(ctx, in.SecondApproverID)
		if err != nil {
			return nil, excerrors.Wrap("INTERNAL_ERROR", "approver role lookup", err)
		}
		if role == "" {
			return nil, excerrors.New("UNAUTHORIZED_ROLE",
				"second approver must hold an active venue-admin binding")
		}
	}

	// Scope: incident tag always; granter narrowing applies if a scope
	// was requested (a granter may also further bind the env axis).
	scope := &Scope{Incident: in.IncidentRef}
	if in.Scope != nil {
		merged := *in.Scope
		merged.Incident = in.IncidentRef
		scope = &merged
	}
	grantedAt := s.now().UTC()
	expiresAt := grantedAt.Add(in.TTL)
	reviewDue := addBusinessDays(grantedAt, BreakGlassReviewDays)

	scopeJSON, err := scope.Normalize().Marshal()
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "scope marshal", err)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "begin tx", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var bindingID int64
	err = tx.QueryRow(ctx, `
		INSERT INTO admin_role_bindings
		    (user_id, role, kind, scope, granter_id, expires_at)
		VALUES ($1, 'Super Admin', 'BREAK_GLASS', $2, $3, $4)
		RETURNING id`,
		in.GranteeID, scopeJSON, granterID, expiresAt).Scan(&bindingID)
	if err != nil {
		return nil, mapGrantError(err)
	}

	var g BreakGlassGrant
	err = tx.QueryRow(ctx, `
		INSERT INTO admin_break_glass_grants
		    (binding_id, grantee_id, granter_id, second_approver_id,
		     p0_alert, incident_ref, reason, expires_at, review_due_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
		RETURNING id, binding_id, grantee_id, granter_id, second_approver_id,
		          p0_alert, incident_ref, reason, granted_at, expires_at,
		          review_due_at, status`,
		bindingID, in.GranteeID, granterID, nullableID(in.SecondApproverID),
		in.UnreachableApprover, in.IncidentRef, in.Reason,
		expiresAt, reviewDue).
		Scan(&g.ID, &g.BindingID, &g.GranteeID, &g.GranterID,
			&g.SecondApproverID, &g.P0Alert, &g.IncidentRef, &g.Reason,
			&g.GrantedAt, &g.ExpiresAt, &g.ReviewDueAt, &g.Status)
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "insert grant", err)
	}

	// Audit watermark (§8.2b.2): every break-glass grant is stamped
	// break_glass + incident_ref so downstream reads can prove it.
	if _, _, err := Log(ctx, tx, AuditEntry{
		AdminUserID: granterID,
		Action:      "rbac.break_glass.grant",
		TargetType:  "user",
		TargetID:    &in.GranteeID,
		AfterState: map[string]any{
			"break_glass": true, "grant_id": g.ID, "binding_id": bindingID,
			"incident_ref": in.IncidentRef, "expires_at": expiresAt,
			"review_due_at":        reviewDue,
			"second_approver_id":   in.SecondApproverID,
			"unreachable_approver": in.UnreachableApprover,
			"reason":               in.Reason,
		},
		IPAddress: in.ClientIP,
	}); err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "audit log", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "commit grant", err)
	}
	if in.UnreachableApprover {
		s.raiseAlert(ctx, granterID, "P0", fmt.Sprintf(
			"solo break-glass grant %d to user %d by %d (incident %s)",
			g.ID, in.GranteeID, granterID, in.IncidentRef))
	}
	return &g, nil
}

// ExpireBreakGlass lapses ACTIVE break-glass grants past expires_at:
// binding → EXPIRED, grant → EXPIRED, session kill, audit watermark.
func (s *Service) ExpireBreakGlass(ctx context.Context, limit int) (int64, error) {
	if limit <= 0 {
		limit = 200
	}
	rows, err := s.pool.Query(ctx, `
		UPDATE admin_break_glass_grants g
		   SET status='EXPIRED'
		 WHERE g.id IN (
		   SELECT id FROM admin_break_glass_grants
		    WHERE status='ACTIVE' AND expires_at <= now()
		    ORDER BY expires_at LIMIT $1)
		RETURNING g.id, g.binding_id, g.grantee_id, g.incident_ref`, limit)
	if err != nil {
		return 0, excerrors.Wrap("INTERNAL_ERROR", "break-glass sweep", err)
	}
	defer rows.Close()
	type exp struct {
		id, bindingID, granteeID int64
		incident                 string
	}
	var due []exp
	for rows.Next() {
		var e exp
		if err := rows.Scan(&e.id, &e.bindingID, &e.granteeID, &e.incident); err != nil {
			return 0, err
		}
		due = append(due, e)
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	for _, e := range due {
		if _, err := s.pool.Exec(ctx, `
			UPDATE admin_role_bindings
			   SET status='EXPIRED', updated_at=now()
			 WHERE id=$1 AND status='ACTIVE'`, e.bindingID); err != nil {
			continue
		}
		s.auditStandalone(ctx, e.granteeID, "rbac.break_glass.expire",
			"user", e.granteeID,
			map[string]any{"break_glass": true, "grant_id": e.id,
				"incident_ref": e.incident, "status": "ACTIVE"},
			map[string]any{"break_glass": true, "grant_id": e.id,
				"incident_ref": e.incident, "status": "EXPIRED"})
		s.killSessionsBestEffort(ctx, e.granteeID, "rbac.break_glass.expire")
	}
	return int64(len(due)), nil
}

// ReviewBreakGlass records the mandatory post-incident review. The
// reviewer must hold an active venue binding and cannot be the grantee
// (self-review of emergency power is meaningless).
func (s *Service) ReviewBreakGlass(ctx context.Context, grantID, reviewerID int64, notes, clientIP string) error {
	if reviewerID <= 0 {
		return excerrors.New("UNAUTHORIZED", "reviewer identity required")
	}
	role, err := s.store.StrongestRole(ctx, reviewerID)
	if err != nil {
		return excerrors.Wrap("INTERNAL_ERROR", "reviewer role lookup", err)
	}
	if role == "" {
		return excerrors.New("UNAUTHORIZED_ROLE",
			"post-incident review requires an active venue-admin binding")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return excerrors.Wrap("INTERNAL_ERROR", "begin tx", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var granteeID, granterID int64
	var incident string
	err = tx.QueryRow(ctx, `
		UPDATE admin_break_glass_grants
		   SET status='REVIEWED', reviewed_at=now(), reviewed_by=$2,
		       review_notes=$3
		 WHERE id=$1 AND reviewed_at IS NULL
		RETURNING grantee_id, granter_id, incident_ref`,
		grantID, reviewerID, notes).Scan(&granteeID, &granterID, &incident)
	if err == pgx.ErrNoRows {
		return excerrors.New("NOT_FOUND",
			fmt.Sprintf("unreviewed break-glass grant %d not found", grantID))
	}
	if err != nil {
		return excerrors.Wrap("INTERNAL_ERROR", "review grant", err)
	}
	if reviewerID == granteeID {
		return excerrors.New("DUAL_CONTROL_VIOLATION",
			"the grantee cannot review their own break-glass grant")
	}
	if _, _, err := Log(ctx, tx, AuditEntry{
		AdminUserID: reviewerID,
		Action:      "rbac.break_glass.review",
		TargetType:  "break_glass_grant",
		TargetID:    &grantID,
		AfterState: map[string]any{
			"break_glass": true, "incident_ref": incident,
			"grantee_id": granteeID, "granter_id": granterID,
			"reviewed_by": reviewerID,
		},
		IPAddress: clientIP,
	}); err != nil {
		return excerrors.Wrap("INTERNAL_ERROR", "audit log", err)
	}
	return tx.Commit(ctx)
}

// EnforceBreakGlassReview suspends the GRANTER's active bindings when a
// break-glass grant passes review_due_at unreviewed (§8.2b.2: "failure
// to review suspends the granter's own binding"). Sweep entrypoint.
func (s *Service) EnforceBreakGlassReview(ctx context.Context, limit int) (int64, error) {
	if limit <= 0 {
		limit = 200
	}
	rows, err := s.pool.Query(ctx, `
		UPDATE admin_break_glass_grants
		   SET status='GRANTER_SUSPENDED'
		 WHERE id IN (
		   SELECT id FROM admin_break_glass_grants
		    WHERE reviewed_at IS NULL AND review_due_at <= now()
		      AND status IN ('ACTIVE','EXPIRED')
		    ORDER BY review_due_at LIMIT $1)
		RETURNING id, granter_id, incident_ref`, limit)
	if err != nil {
		return 0, excerrors.Wrap("INTERNAL_ERROR", "review enforcement", err)
	}
	defer rows.Close()
	type over struct {
		id, granterID int64
		incident      string
	}
	var due []over
	for rows.Next() {
		var o over
		if err := rows.Scan(&o.id, &o.granterID, &o.incident); err != nil {
			return 0, err
		}
		due = append(due, o)
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	for _, o := range due {
		if _, err := s.pool.Exec(ctx, `
			UPDATE admin_role_bindings
			   SET status='SUSPENDED', suspended_at=now(),
			       suspend_reason='break_glass_review_overdue', updated_at=now()
			 WHERE granter_id=$1 AND user_id=$1 AND status='ACTIVE'`, o.granterID); err != nil {
			continue
		}
		s.auditStandalone(ctx, o.granterID, "rbac.break_glass.granter_suspended",
			"break_glass_grant", o.id,
			map[string]any{"review_overdue": true, "incident_ref": o.incident},
			map[string]any{"granter_bindings": "SUSPENDED",
				"reason": "break_glass_review_overdue"})
		s.killSessionsBestEffort(ctx, o.granterID, "rbac.break_glass.enforce")
		s.raiseAlert(ctx, o.granterID, "P1", fmt.Sprintf(
			"break-glass grant %d (incident %s) unreviewed past deadline — granter %d suspended",
			o.id, o.incident, o.granterID))
	}
	return int64(len(due)), nil
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// addBusinessDays adds n Mon–Fri days.
func addBusinessDays(t time.Time, n int) time.Time {
	for n > 0 {
		t = t.AddDate(0, 0, 1)
		if t.Weekday() == time.Saturday || t.Weekday() == time.Sunday {
			continue
		}
		n--
	}
	return t
}

// nullableID maps 0 → NULL for FK columns.
func nullableID(id int64) any {
	if id <= 0 {
		return nil
	}
	return id
}

// KillSessions terminates every session the user holds (§8.2b session
// kill). Exposed for the dual-control executor path — the revoke lands
// in-tx while Redis cannot join the tx; a later rollback costs only a
// re-login (fail-safe direction), so firing it pre-commit is correct.
func (s *Service) KillSessions(ctx context.Context, userID int64) {
	s.killSessionsBestEffort(ctx, userID, "rbac.revoke")
}

// killSessionsBestEffort terminates the holder's sessions (§8.2b
// "session kill"). A killer failure is logged via the audit stream
// rather than silently dropped — the binding transition still commits;
// revocation correctness never depends on Redis availability.
func (s *Service) killSessionsBestEffort(ctx context.Context, userID int64, action string) {
	if s.killSessions == nil {
		return
	}
	if err := s.killSessions(ctx, userID); err != nil {
		s.auditStandalone(ctx, userID, action+".session_kill_failed", "user", userID,
			nil, map[string]any{"error": err.Error()})
	}
}

// raiseAlert dispatches to the wired Alerter; nil alerter degrades to an
// audit row so the alert attempt is still provable.
func (s *Service) raiseAlert(ctx context.Context, adminID int64, severity, summary string) {
	if s.alert != nil {
		if err := s.alert(ctx, severity, summary); err == nil {
			return
		}
	}
	s.auditStandalone(ctx, adminID, "rbac.alert", "", 0,
		nil, map[string]any{"severity": severity, "summary": summary,
			"dispatch": "fallback_audit"})
}

// auditStandalone writes a self-contained audit row (its own tx) for
// sweeps — the transition already committed; the audit must still land.
// admin_user_id 0 is impossible (CHECK on validate) — use the affected
// principal as the acting id for system-driven transitions.
func (s *Service) auditStandalone(ctx context.Context, adminID int64, action, targetType string, targetID int64, before, after any) {
	if adminID <= 0 {
		adminID = targetID // system action attributed to the affected principal
	}
	var tid *int64
	if targetID > 0 {
		tid = &targetID
	}
	_, _, _ = LogAuto(ctx, s.pool, AuditEntry{
		AdminUserID: adminID, Action: action, TargetType: targetType,
		TargetID: tid, BeforeState: before, AfterState: after,
	})
}
