// Tasks 7.3.1/7.3.11 — venue-admin RBAC core: the six §8.2 roles, the
// role×route permission matrix generated from the gateway route registry
// (spec §8.2 normative rule), the admin_role_bindings store (migration
// 090) and the RoleResolver adapters consumed by the Phase-05 seams
// (accounts.FreezeService, api.ManualLiquidationService,
// api.OrderDeps).
//
// Fail-closed (spec §2.7): no bindings ⇒ no permissions; store errors
// propagate as INTERNAL_ERROR, never as a silent allow.
package admin

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"exchange/internal/gateway"
)

// §8.2 role names (canonical spellings owned by the gateway registry).
const (
	RoleSuperAdmin        = gateway.RoleSuperAdmin        // "Super Admin"
	RoleRiskManager       = gateway.RoleRiskManager       // "Risk Manager"
	RoleComplianceOfficer = gateway.RoleComplianceOfficer // "Compliance Officer"
	RoleFinanceOps        = gateway.RoleFinanceOps        // "Finance Ops"
	RoleSupportAgent      = gateway.RoleSupportAgent      // "Support Agent"
	RoleReadOnlyAuditor   = gateway.RoleReadOnlyAuditor   // "Read-Only Auditor"
)

// VenueRoles is the complete §8.2 venue-admin role set.
var VenueRoles = []string{
	RoleSuperAdmin, RoleRiskManager, RoleComplianceOfficer,
	RoleFinanceOps, RoleSupportAgent, RoleReadOnlyAuditor,
}

// Role-system names for principal_role_systems (§8.2a.2 disjoint sets).
const (
	SystemVenueAdmin      = "VENUE_ADMIN"
	SystemClientDelegated = "CLIENT_DELEGATED" // migration 074 (Phase-12)
	SystemExternalAuditor = "EXTERNAL_AUDITOR" // Phase-24 Task 24.3.18
)

// Binding kind values.
const (
	KindStandard   = "STANDARD"
	KindBreakGlass = "BREAK_GLASS"
)

// Binding status lifecycle.
const (
	StatusActive    = "ACTIVE"
	StatusSuspended = "SUSPENDED"
	StatusRevoked   = "REVOKED"
	StatusExpired   = "EXPIRED"
)

// RoleSummary carries the normative §8.2 permission line per role —
// surfaced verbatim by GET /api/v1/admin/roles.
var RoleSummary = map[string]string{
	RoleSuperAdmin:        "All permissions, including role management",
	RoleRiskManager:       "Kill-switch, halt, force liquidation, margin adjustments",
	RoleComplianceOfficer: "Sanctions flags, KYC review, travel rule, SAR filing",
	RoleFinanceOps:        "Balance adjustments, withdrawal approval, funding operations",
	RoleSupportAgent:      "Read-only user data, notifications, ticket management",
	RoleReadOnlyAuditor:   "Read-only dashboards, no mutation",
}

// ValidRole reports whether name is one of the six §8.2 venue roles.
func ValidRole(name string) bool {
	_, ok := RoleSummary[name]
	return ok
}

// roleRank orders roles for "strongest active binding" resolution when a
// principal holds more than one binding. The named operational roles are
// deliberately coequal — none subsumes another's domain.
func roleRank(role string) int {
	switch role {
	case RoleSuperAdmin:
		return 100
	case RoleRiskManager, RoleComplianceOfficer, RoleFinanceOps:
		return 50
	case RoleSupportAgent:
		return 20
	case RoleReadOnlyAuditor:
		return 10
	}
	return 0
}

// Permits reports whether held satisfies required. The enforceable matrix
// is role × route (spec §8.2): a route declares its required role, and the
// role satisfies it directly; "Super Admin — all permissions" satisfies
// every requirement; the gateway.RoleAnyAdmin "*" sentinel is satisfied
// by any of the six §8.2 roles (the operation row carries the real gate).
// Nothing else implies a permission.
func Permits(required, held string) bool {
	switch required {
	case "":
		return true // route is not role-gated
	case gateway.RoleAnyAdmin:
		return ValidRole(held)
	}
	return held == required || held == RoleSuperAdmin
}

// PermittedRoutes returns the sorted-ish registry keys ("METHOD path") of
// role-gated routes a binding for held satisfies — the §8.2 permission
// matrix row, generated from route registry metadata.
func PermittedRoutes(routes []gateway.Route, held string) []string {
	out := []string{}
	for _, rt := range routes {
		if rt.Auth.Role == "" {
			continue
		}
		if Permits(rt.Auth.Role, held) {
			out = append(out, rt.Method+" "+rt.Path)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Bindings store (migration 090 admin_role_bindings).
// ---------------------------------------------------------------------------

// Binding is one admin_role_bindings row.
type Binding struct {
	ID            int64      `json:"id"`
	UserID        int64      `json:"user_id"`
	Role          string     `json:"role"`
	Kind          string     `json:"kind"`
	Scope         *Scope     `json:"scope,omitempty"` // nil = global
	GranterID     int64      `json:"granter_id"`
	GrantedAt     time.Time  `json:"granted_at"`
	ExpiresAt     time.Time  `json:"expires_at"`
	Status        string     `json:"status"`
	SuspendedAt   *time.Time `json:"suspended_at,omitempty"`
	SuspendReason string     `json:"suspend_reason,omitempty"`
	RevokedAt     *time.Time `json:"revoked_at,omitempty"`
	RevokedBy     *int64     `json:"revoked_by,omitempty"`
	RevokeReason  string     `json:"revoke_reason,omitempty"`
}

// Store is the PostgreSQL binding store. All reads/writes go through here
// — nothing else reads admin_role_bindings.
type Store struct {
	pool *pgxpool.Pool
	now  func() time.Time
}

// NewStore wires the store on the primary pool.
func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool, now: time.Now}
}

// SetClockForTest overrides the clock; tests only.
func (s *Store) SetClockForTest(now func() time.Time) { s.now = now }

const bindingCols = `id, user_id, role, kind, scope, granter_id, granted_at,
	expires_at, status, suspended_at, COALESCE(suspend_reason,''),
	revoked_at, revoked_by, COALESCE(revoke_reason,'')`

func scanBinding(row interface {
	Scan(dest ...any) error
}) (Binding, error) {
	var b Binding
	var raw []byte
	err := row.Scan(&b.ID, &b.UserID, &b.Role, &b.Kind, &raw,
		&b.GranterID, &b.GrantedAt, &b.ExpiresAt, &b.Status,
		&b.SuspendedAt, &b.SuspendReason, &b.RevokedAt, &b.RevokedBy,
		&b.RevokeReason)
	if err != nil {
		return Binding{}, err
	}
	if len(raw) > 0 && string(raw) != "null" {
		sc, err := ParseScope(raw)
		if err != nil {
			return Binding{}, fmt.Errorf("binding %d scope: %w", b.ID, err)
		}
		b.Scope = sc
	}
	return b, nil
}

// ActiveBindings returns the user's live venue bindings (status ACTIVE,
// not yet expired). A principal with none has no admin permissions.
func (s *Store) ActiveBindings(ctx context.Context, userID int64) ([]Binding, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT `+bindingCols+`
		  FROM admin_role_bindings
		 WHERE user_id = $1 AND status = 'ACTIVE' AND expires_at > now()
		 ORDER BY id`, userID)
	if err != nil {
		return nil, fmt.Errorf("admin: active bindings: %w", err)
	}
	defer rows.Close()
	out := []Binding{}
	for rows.Next() {
		b, err := scanBinding(rows)
		if err != nil {
			return nil, fmt.Errorf("admin: scan binding: %w", err)
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// ListBindings returns bindings for the admin surface (all statuses;
// optional filters applied in SQL).
func (s *Store) ListBindings(ctx context.Context, userID int64, status string, limit int) ([]Binding, error) {
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	q := `SELECT ` + bindingCols + ` FROM admin_role_bindings WHERE TRUE`
	args := []any{}
	if userID > 0 {
		args = append(args, userID)
		q += fmt.Sprintf(" AND user_id = $%d", len(args))
	}
	if status != "" {
		args = append(args, status)
		q += fmt.Sprintf(" AND status = $%d", len(args))
	}
	args = append(args, limit)
	q += fmt.Sprintf(" ORDER BY id DESC LIMIT $%d", len(args))
	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("admin: list bindings: %w", err)
	}
	defer rows.Close()
	out := []Binding{}
	for rows.Next() {
		b, err := scanBinding(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// StrongestRole returns the highest-ranked ACTIVE venue role for the
// principal, or "" when none — the single-role contract the Phase-05
// resolver seams expect ("", nil maps to UNAUTHORIZED_ROLE at the seam).
func (s *Store) StrongestRole(ctx context.Context, userID int64) (string, error) {
	bindings, err := s.ActiveBindings(ctx, userID)
	if err != nil {
		return "", err
	}
	best := ""
	bestRank := -1
	for _, b := range bindings {
		if r := roleRank(b.Role); r > bestRank {
			best, bestRank = b.Role, r
		}
	}
	return best, nil
}

// RoleResolver adapts the store to the resolver signature shared by the
// Phase-05 seams (accounts.RoleResolver, api.AdminRoleResolver,
// api.OrderDeps.RoleResolver — all `func(ctx, userID) (string, error)`).
// No binding ⇒ ("", nil) → the seam maps it to UNAUTHORIZED_ROLE.
func (s *Store) RoleResolver() func(ctx context.Context, userID int64) (string, error) {
	return func(ctx context.Context, userID int64) (string, error) {
		return s.StrongestRole(ctx, userID)
	}
}
