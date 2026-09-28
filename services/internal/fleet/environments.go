// environments.go — the §19.16.1 environment context model (migration 091
// `environments`). The admin session targets exactly one environment; this
// file owns listing/resolving it and watermarking switches to production
// in admin_audit_log (Task 7.3.3).
package fleet

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5"

	"exchange/internal/admin"

	excerrors "exchange/pkg/errors"
)

// Environment is one environments row.
type Environment struct {
	ID              int64           `json:"id"`
	Name            string          `json:"name"`
	TopologyProfile json.RawMessage `json:"topology_profile"`
	PromotionPolicy json.RawMessage `json:"promotion_policy"`
	CreatedAt       time.Time       `json:"created_at"`
	UpdatedAt       time.Time       `json:"updated_at"`
}

const environmentCols = `id, name, topology_profile, promotion_policy, created_at, updated_at`

// ListEnvironments returns all three environments ordered dev → staging →
// production. The session context itself is resolved by the middleware —
// this is the registry view backing GET /admin/fleet/environments.
func (s *Service) ListEnvironments(ctx context.Context) ([]Environment, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT `+environmentCols+` FROM environments
		 ORDER BY CASE name WHEN 'dev' THEN 0 WHEN 'staging' THEN 1 ELSE 2 END`)
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "list environments", err)
	}
	defer rows.Close()
	out := []Environment{}
	for rows.Next() {
		var e Environment
		if err := rows.Scan(&e.ID, &e.Name, &e.TopologyProfile,
			&e.PromotionPolicy, &e.CreatedAt, &e.UpdatedAt); err != nil {
			return nil, excerrors.Wrap("INTERNAL_ERROR", "scan environment", err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "environments cursor", err)
	}
	return out, nil
}

// GetEnvironment resolves one environment row by name (normalized).
func (s *Service) GetEnvironment(ctx context.Context, name string) (*Environment, error) {
	var e Environment
	err := s.pool.QueryRow(ctx, `
		SELECT `+environmentCols+` FROM environments WHERE name = $1`,
		admin.NormalizeEnv(name)).
		Scan(&e.ID, &e.Name, &e.TopologyProfile, &e.PromotionPolicy,
			&e.CreatedAt, &e.UpdatedAt)
	if err == pgx.ErrNoRows {
		return nil, excerrors.New("NOT_FOUND",
			"environment "+admin.NormalizeEnv(name)+" not found")
	}
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "read environment", err)
	}
	return &e, nil
}

// WatermarkProdContext writes the §19.16.4 context-switch watermark for
// the actor's session env. Called once per prod-scoped fleet mutation;
// reads skip it to keep the audit log lean.
func (s *Service) WatermarkProdContext(ctx context.Context, tx pgx.Tx, actor Actor, operation string) error {
	env, err := requireEnv(actor)
	if err != nil {
		return err
	}
	return s.watermarkEnv(ctx, tx, actor, operation, env)
}

// watermarkEnv writes the watermark for an explicit target env — used by
// Promote, where the session sits in the source env but the production
// *target* is what must be watermarked.
func (s *Service) watermarkEnv(ctx context.Context, tx pgx.Tx, actor Actor, operation, env string) error {
	if env != EnvProduction {
		return nil
	}
	envRow, err := s.GetEnvironment(ctx, env)
	if err != nil {
		return err
	}
	_, _, err = admin.Log(ctx, tx, admin.AuditEntry{
		AdminUserID: actor.UserID,
		Action:      "fleet.env_context",
		TargetType:  "environment",
		TargetID:    &envRow.ID,
		AfterState: map[string]any{
			"env":       env,
			"operation": operation,
			"note":      "production context switch watermark (spec §19.16.4)",
		},
		IPAddress: actor.ClientIP,
	})
	if err != nil {
		return excerrors.Wrap("INTERNAL_ERROR", "env-context watermark", err)
	}
	return nil
}
