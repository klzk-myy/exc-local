package copy

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"exchange/internal/admin"
)

// AdminAuditor binds Service.Auditor to the admin_audit_log +
// audit_hash_chain machinery via admin.LogAuto (spec §5.40 — the
// suspension audit is written BEFORE the status flip so an audit failure
// aborts the action, and the chain anchor commits in the same tx).
type AdminAuditor struct {
	pool *pgxpool.Pool
}

// NewAdminAuditor wires the auditor — nil pool fails closed.
func NewAdminAuditor(pool *pgxpool.Pool) (*AdminAuditor, error) {
	if pool == nil {
		return nil, fmt.Errorf("copy: admin auditor requires a pgx pool")
	}
	return &AdminAuditor{pool: pool}, nil
}

// LogSuspension implements Auditor — the compliance-officer action lands
// as "copy.strategy.suspend" on the strategy_profiles target.
func (a *AdminAuditor) LogSuspension(ctx context.Context, strategyID,
	adminUserID int64, ip, reason string) error {
	_, _, err := admin.LogAuto(ctx, a.pool, admin.AuditEntry{
		AdminUserID: adminUserID,
		Action:      "copy.strategy.suspend",
		TargetType:  "strategy_profiles",
		TargetID:    &strategyID,
		AfterState: map[string]any{
			"status": "SUSPENDED",
			"reason": reason,
		},
		IPAddress: ip,
	})
	if err != nil {
		return fmt.Errorf("admin audit log: %w", err)
	}
	return nil
}
