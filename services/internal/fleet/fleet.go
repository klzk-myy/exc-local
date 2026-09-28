// Package fleet implements Phase-09 Task 9.3.30 — the environment context
// model, fleet inventory, server actions and promotion gates behind the
// admin console (spec §19.16, §24 #350; migration 091).
//
// Three environments share one control plane: dev auto-deploys, staging
// requires a second eligible approver, production additionally requires
// the interlocks of §19.16.3 (open deploy window, no open P0/P1 incident,
// healthy DR standby) plus fresh gate evidence (soak report, checkpoint
// clearance). Direction is enforced: dev → staging → production only;
// production never demotes and prod→dev data moves are refused outright
// (FORBIDDEN — the task text is normative on the code).
//
// Environment context: every request is answered inside the session's
// target env (X-Admin-Env, resolved by the Task 7.3.11 middleware into
// admin.Identity.Env). Handlers pass it down as Actor.Env and the service
// refuses rows outside that env — a dev-valid binding grants nothing in
// production because the middleware already rejected the binding's env
// axis before the handler ran.
//
// Fail-closed (spec §2.7): a missing/unreachable interlock source
// (deploy_windows, incident feed, DR replication state) blocks a
// production promotion rather than silently passing it.
package fleet

import (
	"context"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"exchange/internal/admin"

	excerrors "exchange/pkg/errors"
)

// §19.16.1 environment vocabulary (admin.NormalizeEnv maps arbitrary
// labels onto it — unrecognized input lands on production, fail-closed).
const (
	EnvDev        = "dev"
	EnvStaging    = "staging"
	EnvProduction = "production"
)

// RoleResolver resolves an admin user id to its strongest §8.2 role —
// the same seam the Phase-05/07 surfaces use (admin.Store.RoleResolver).
// A nil resolver fails closed UNAUTHORIZED_ROLE.
type RoleResolver func(ctx context.Context, userID int64) (string, error)

// Actor is the operating principal: the RBAC-verified identity plus the
// session's target environment.
type Actor struct {
	UserID     int64
	Role       string // strongest satisfying binding role (audit only)
	Env        string // session target env (normalized §19.16 vocabulary)
	ClientIP   string
	ApproverID int64 // four-eyes second principal; 0 = none supplied
}

// Service is the fleet/promotion domain service. All reads and mutations
// are scoped to the actor's environment context.
type Service struct {
	pool    *pgxpool.Pool
	resolve RoleResolver
	probes  Probes
	now     func() time.Time
}

// NewService wires the service on the primary pool. resolve may be nil
// (approver checks then fail closed); probes==nil selects the PG-backed
// defaults described in releases.go.
func NewService(pool *pgxpool.Pool, resolve RoleResolver, probes *Probes) *Service {
	p := Probes{}
	if probes != nil {
		p = *probes
	}
	return &Service{pool: pool, resolve: resolve, probes: p, now: time.Now}
}

// SetClockForTest overrides the clock; tests only.
func (s *Service) SetClockForTest(now func() time.Time) { s.now = now }

// requireEnv returns the actor's normalized target environment —
// env-scoped fleet operations never run against a second env by accident.
func requireEnv(actor Actor) (string, error) {
	env := admin.NormalizeEnv(actor.Env)
	if env == "" {
		return "", excerrors.New("FORBIDDEN",
			"no environment context on the admin session")
	}
	return env, nil
}

// requireApprover validates the four-eyes second principal for a
// sensitive op: supplied, distinct from the maker, and holding a binding
// that satisfies requiredRole.
func (s *Service) requireApprover(ctx context.Context, actor Actor, requiredRole string) error {
	if actor.ApproverID <= 0 {
		return excerrors.New("DUAL_CONTROL_REQUIRED",
			"operation requires a distinct second approver (four-eyes)")
	}
	if actor.ApproverID == actor.UserID {
		return excerrors.New("DUAL_CONTROL_VIOLATION",
			"the initiating principal cannot approve their own action")
	}
	if s.resolve == nil {
		return excerrors.New("UNAUTHORIZED_ROLE",
			"role resolver not configured (approver cannot be verified)")
	}
	role, err := s.resolve(ctx, actor.ApproverID)
	if err != nil {
		return excerrors.Wrap("INTERNAL_ERROR", "approver role lookup", err)
	}
	if !admin.Permits(requiredRole, role) {
		return excerrors.New("UNAUTHORIZED_ROLE",
			"approver must hold a binding satisfying "+requiredRole)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Per-environment secrets resolution (spec §19.16.3: "Config and secrets
// are versioned per environment from Vault/KMS — never copied across
// environments").
// ---------------------------------------------------------------------------

// secretNameRe admits flat Vault key names only — no '/', '..', or
// leading separators, so a resolved path can never traverse into another
// environment's namespace.
var secretNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,126}$`)

// SecretPath resolves the Vault KV-v2 path for a secret inside the
// session's own environment. The env prefix is derived from the session
// context — never from caller-supplied data — so the path cannot
// reference another environment (zero cross-env leakage by construction).
func SecretPath(sessionEnv, name string) (string, error) {
	env := admin.NormalizeEnv(sessionEnv)
	if env == "" {
		return "", excerrors.New("FORBIDDEN",
			"secret resolution requires an environment context")
	}
	if !secretNameRe.MatchString(strings.TrimSpace(name)) {
		return "", excerrors.New("INVALID_REQUEST",
			"secret name must be a flat [A-Za-z0-9._-] identifier")
	}
	return "secret/data/" + env + "/" + strings.TrimSpace(name), nil
}
