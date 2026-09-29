package reconciliation

// dispatch.go — the production Halter + Alerter adapters.
//
// PgHalter is the machine-actor auto-halt (ruling R4): it writes the
// SAME artifacts KillSwitchService commits for an admin SET —
// a trading_suspensions row then the halt:* Redis flag — without
// passing through the §8.2 dual-control request path, because a
// reconciliation mismatch cannot wait on a second approver. The record
// uses initiated_by = 0 (system sentinel; the column has no FK), so
// operators identify machine halts and clear them through the normal
// kill-switch reset surface — dual-control stays on the RESUME path
// where it belongs.
//
// DurableOpsAlerter mirrors the account-freeze alerter: a durable
// funding_ops_alerts row PLUS the shared NATS page — the alert survives
// a pager outage and the findings table is the full report trail.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"exchange/internal/admin"
	"exchange/internal/settlement"
)

// SystemActorID stamps trading_suspensions.initiated_by for
// reconciliation auto-halts — the column carries no FK, so zero is the
// documented machine sentinel (audit rows read "system:reconciliation").
const SystemActorID = 0

// PgHalter emits scoped auto-halts. Flags is the same
// admin.KillSwitchFlagWriter the KillSwitchService uses — *redis.Client
// satisfies it; nil fails closed (a halt that cannot take effect is an
// error, never a silent no-op).
type PgHalter struct {
	pool  *pgxpool.Pool
	flags admin.KillSwitchFlagWriter
}

// NewPgHalter wires the production halter.
func NewPgHalter(pool *pgxpool.Pool, flags admin.KillSwitchFlagWriter) *PgHalter {
	return &PgHalter{pool: pool, flags: flags}
}

// canonicalTarget mirrors admin.canonicalTarget for the scopes the
// engine emits — case-insensitive axes are uppercased so the durable
// row and the Redis flag key agree with the resolver's read path.
func canonicalTarget(scope, target string) string {
	t := strings.TrimSpace(target)
	switch scope {
	case "INSTRUMENT", "INSTRUMENT_CLASS", "RAIL", "REGION", "ENV":
		return strings.ToUpper(t)
	}
	return t
}

// Halt implements Halter: idempotent (an ACTIVE suspension on the
// scope returns its id, never a duplicate row), durable-first then the
// enforcement flag — the same commit-then-flag ordering as
// KillSwitchService.Set.
func (h *PgHalter) Halt(ctx context.Context, scope, target, reason string) (int64, error) {
	if h == nil || h.pool == nil || h.flags == nil {
		return 0, fmt.Errorf("reconciliation halter: pool or flag writer nil")
	}
	sc := strings.ToUpper(strings.TrimSpace(scope))
	if sc == "" {
		sc = admin.ScopeGlobal
	}
	if !admin.ValidScope(sc) {
		return 0, fmt.Errorf("reconciliation halt: invalid scope %q", scope)
	}
	t := canonicalTarget(sc, target)
	if sc == admin.ScopeGlobal {
		t = ""
	} else if t == "" {
		return 0, fmt.Errorf("reconciliation halt: scope %s requires a target", sc)
	}
	if len(reason) < 8 {
		reason = "reconciliation auto-halt"
	}

	tx, err := h.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("reconciliation halt: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Idempotent: reuse the ACTIVE row when the scope already halted —
	// a second reconciliation run on the same divergence is not a new
	// suspension.
	var existing int64
	err = tx.QueryRow(ctx, `
		SELECT suspension_id FROM trading_suspensions
		 WHERE scope = $1::suspension_scope_enum AND target_id = $2
		   AND state = 'ACTIVE'`, sc, t).Scan(&existing)
	if err == nil {
		_ = tx.Rollback(ctx)
		// The suspension row already exists but the halt flag is NOT
		// necessarily raised — Redis flag loss (flush/restart) is only
		// repaired at boot by ReconcileFlags. Re-raise here so a reused
		// suspension reasserts enforcement immediately; SetHaltScope is
		// itself idempotent.
		if ferr := h.flags.SetHaltScope(ctx, sc, t, reason); ferr != nil {
			return existing, fmt.Errorf("reconciliation halt: suspension %d "+
				"active but halt flag re-raise failed: %w", existing, ferr)
		}
		return existing, nil
	}
	if err != pgx.ErrNoRows {
		return 0, fmt.Errorf("reconciliation halt: suspension read: %w", err)
	}

	var id int64
	err = tx.QueryRow(ctx, `
		INSERT INTO trading_suspensions
		    (scope, target_id, reason, initiated_by, approved_by)
		VALUES ($1::suspension_scope_enum, $2, $3, $4, NULL)
		RETURNING suspension_id`,
		sc, t, reason, SystemActorID).Scan(&id)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			// Race with a concurrent SET — surface the winning record.
			var winner int64
			if qerr := h.pool.QueryRow(ctx, `
				SELECT suspension_id FROM trading_suspensions
				 WHERE scope = $1::suspension_scope_enum AND target_id = $2
				   AND state = 'ACTIVE'`, sc, t).Scan(&winner); qerr == nil {
				_ = tx.Rollback(ctx)
				return winner, nil
			}
		}
		return 0, fmt.Errorf("reconciliation halt: suspension insert: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("reconciliation halt: commit: %w", err)
	}
	if err := h.flags.SetHaltScope(ctx, sc, t, reason); err != nil {
		return id, fmt.Errorf("reconciliation halt: suspension %d recorded "+
			"but halt flag raise failed (boot-time ReconcileFlags re-raises): %w",
			id, err)
	}
	return id, nil
}

// DurableOpsAlerter raises the ops page and persists a durable alert
// row — the freezeOpsAlerter twin-write pattern.
type DurableOpsAlerter struct {
	pool *pgxpool.Pool
	page settlement.OpsAlerter // nil → durable row only
}

// NewDurableOpsAlerter wires the alert dispatcher; page is the shared
// NATS ops-alert seam (settlement.PublisherAlerter), nil disables.
func NewDurableOpsAlerter(pool *pgxpool.Pool, page settlement.OpsAlerter) *DurableOpsAlerter {
	return &DurableOpsAlerter{pool: pool, page: page}
}

// Raise persists the funding_ops_alerts row then pages; a page failure
// is returned so the engine logs it — the durable row already landed.
func (a *DurableOpsAlerter) Raise(ctx context.Context, al settlement.OpsAlert) error {
	if a == nil {
		return nil
	}
	var derr error
	if a.pool != nil {
		detail, _ := json.Marshal(al.Details)
		_, derr = a.pool.Exec(ctx, `
			INSERT INTO funding_ops_alerts (code, severity, summary, detail)
			VALUES ($1, $2, $3, $4)`,
			al.Code, al.Severity, al.Summary, detail)
	}
	var perr error
	if a.page != nil {
		perr = a.page.Raise(ctx, al)
	}
	if derr != nil {
		return derr
	}
	return perr
}
