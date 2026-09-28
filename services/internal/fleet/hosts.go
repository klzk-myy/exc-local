// hosts.go — fleet inventory + dual-controlled server actions
// (spec §19.16.2, migration 091 `fleet_hosts` / `server_actions`).
//
// Host lifecycle: ACTIVE → DRAINING → MAINTENANCE → ACTIVE |
// DECOMMISSIONED (terminal). Actions: drain (ACTIVE→DRAINING), cordon
// (ACTIVE|DRAINING→MAINTENANCE), reboot (recorded, state preserved —
// post-reboot reactivation is the health-sync's job, not the operator's),
// decommission (any non-terminal → DECOMMISSIONED).
//
// Prod server actions are dual-controlled sensitive ops (§19.16.4):
// the request carries approver_id, a distinct principal holding a binding
// that satisfies the route role (Super Admin).
package fleet

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"exchange/internal/admin"
	"exchange/internal/gateway"

	excerrors "exchange/pkg/errors"
)

// Host states (migration 091 CHECK).
const (
	HostActive         = "ACTIVE"
	HostDraining       = "DRAINING"
	HostMaintenance    = "MAINTENANCE"
	HostDecommissioned = "DECOMMISSIONED"
)

// Host health rollup values.
const (
	HealthHealthy  = "HEALTHY"
	HealthDegraded = "DEGRADED"
	HealthDown     = "DOWN"
	HealthUnknown  = "UNKNOWN"
)

// Server-action vocabulary (migration 091 CHECK).
const (
	ActionDrain        = "DRAIN"
	ActionCordon       = "CORDON"
	ActionReboot       = "REBOOT"
	ActionDecommission = "DECOMMISSION"
)

// Host is one fleet_hosts row joined with its environment name.
type Host struct {
	ID        int64           `json:"id"`
	Env       string          `json:"env"`
	Hostname  string          `json:"hostname"`
	Role      string          `json:"role"`
	ShardID   *int            `json:"shard_id,omitempty"`
	AZ        string          `json:"az,omitempty"`
	Rack      string          `json:"rack,omitempty"`
	HWSpec    json.RawMessage `json:"hw_spec"`
	Health    string          `json:"health"`
	State     string          `json:"state"`
	CreatedAt time.Time       `json:"created_at"`
	UpdatedAt time.Time       `json:"updated_at"`
}

const hostCols = `h.id, e.name, h.hostname, h.role, h.shard_id, h.az, h.rack,
	h.hw_spec, h.health, h.state, h.created_at, h.updated_at`

const hostJoin = ` FROM fleet_hosts h JOIN environments e ON e.id = h.environment_id`

func scanHost(row interface{ Scan(...any) error }) (Host, error) {
	var h Host
	err := row.Scan(&h.ID, &h.Env, &h.Hostname, &h.Role, &h.ShardID,
		&h.AZ, &h.Rack, &h.HWSpec, &h.Health, &h.State,
		&h.CreatedAt, &h.UpdatedAt)
	return h, err
}

// ListHosts returns the inventory for the actor's environment context —
// no cross-env view exists by design (§19.16.1 "one environment at a
// time"). Optional filters: state, role.
func (s *Service) ListHosts(ctx context.Context, actor Actor, state, role string) ([]Host, error) {
	env, err := requireEnv(actor)
	if err != nil {
		return nil, err
	}
	q := `SELECT ` + hostCols + hostJoin + ` WHERE e.name = $1`
	args := []any{env}
	if state != "" {
		args = append(args, state)
		q += fmt.Sprintf(" AND h.state = $%d", len(args))
	}
	if role != "" {
		args = append(args, role)
		q += fmt.Sprintf(" AND h.role = $%d", len(args))
	}
	q += " ORDER BY h.role, h.hostname"
	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "list hosts", err)
	}
	defer rows.Close()
	out := []Host{}
	for rows.Next() {
		h, err := scanHost(rows)
		if err != nil {
			return nil, excerrors.Wrap("INTERNAL_ERROR", "scan host", err)
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// TopologyView is the GET /admin/fleet/topology payload: shard→host map
// plus role and health rollups for one environment.
type TopologyView struct {
	Env    string              `json:"env"`
	Shards map[string][]Host   `json:"shards"` // "0","1",… — shard-less hosts under "unsharded"
	Roles  map[string][]string `json:"roles"`  // role → hostnames
	Health map[string]int      `json:"health"` // health → count
}

// Topology renders the cluster view of one environment (§19.16.2: shard→
// host map with role rollups; K8s/HPA, Sentinel, NATS, CH and PG lag views
// are populated by the health sync — absent sources simply have no rows).
func (s *Service) Topology(ctx context.Context, actor Actor, env string) (*TopologyView, error) {
	sessEnv, err := requireEnv(actor)
	if err != nil {
		return nil, err
	}
	target := admin.NormalizeEnv(env)
	if target == "" {
		target = sessEnv
	}
	if target != sessEnv {
		return nil, excerrors.New("FORBIDDEN",
			"topology must be read inside the session's environment context")
	}
	hosts, err := s.ListHosts(ctx, actor, "", "")
	if err != nil {
		return nil, err
	}
	tv := &TopologyView{
		Env:    target,
		Shards: map[string][]Host{},
		Roles:  map[string][]string{},
		Health: map[string]int{},
	}
	for _, h := range hosts {
		key := "unsharded"
		if h.ShardID != nil {
			key = fmt.Sprintf("%d", *h.ShardID)
		}
		tv.Shards[key] = append(tv.Shards[key], h)
		tv.Roles[h.Role] = append(tv.Roles[h.Role], h.Hostname)
		tv.Health[h.Health]++
	}
	return tv, nil
}

// actionTransition is the host-state effect of each action.
// REBOOT preserves the state (nil target).
var actionTransition = map[string]struct {
	from map[string]bool
	to   string // "" = state preserved
}{
	ActionDrain:        {from: map[string]bool{HostActive: true}, to: HostDraining},
	ActionCordon:       {from: map[string]bool{HostActive: true, HostDraining: true}, to: HostMaintenance},
	ActionReboot:       {from: map[string]bool{HostActive: true, HostDraining: true, HostMaintenance: true}, to: ""},
	ActionDecommission: {from: map[string]bool{HostActive: true, HostDraining: true, HostMaintenance: true}, to: HostDecommissioned},
}

// ServerAction is one server_actions row.
type ServerAction struct {
	ID          int64      `json:"id"`
	HostID      int64      `json:"host_id"`
	Action      string     `json:"action"`
	Status      string     `json:"status"`
	RequestedBy int64      `json:"requested_by"`
	ApprovedBy  *int64     `json:"approved_by,omitempty"`
	Reason      string     `json:"reason"`
	CreatedAt   time.Time  `json:"created_at"`
	DecidedAt   *time.Time `json:"decided_at,omitempty"`
}

// RequestAction applies a server action to a host inside the actor's env
// context: transition legality, dual control in production, durable
// server_actions row, host state update, prod-context watermark and the
// admin_audit_log entry — all in one transaction.
func (s *Service) RequestAction(ctx context.Context, actor Actor, hostID int64, action, reason string) (*ServerAction, error) {
	if actor.UserID <= 0 {
		return nil, excerrors.New("UNAUTHORIZED", "admin identity required")
	}
	sessEnv, err := requireEnv(actor)
	if err != nil {
		return nil, err
	}
	spec, ok := actionTransition[action]
	if !ok {
		return nil, excerrors.New("INVALID_REQUEST",
			"action must be one of DRAIN|CORDON|REBOOT|DECOMMISSION")
	}
	if reason == "" {
		return nil, excerrors.New("INVALID_REQUEST", "reason is required")
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "begin tx", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var h Host
	err = tx.QueryRow(ctx, `
		SELECT `+hostCols+hostJoin+` WHERE h.id = $1 FOR UPDATE OF h`, hostID).
		Scan(&h.ID, &h.Env, &h.Hostname, &h.Role, &h.ShardID, &h.AZ, &h.Rack,
			&h.HWSpec, &h.Health, &h.State, &h.CreatedAt, &h.UpdatedAt)
	if err == pgx.ErrNoRows {
		return nil, excerrors.New("NOT_FOUND", fmt.Sprintf("host %d not found", hostID))
	}
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "lock host", err)
	}
	// The session env context is the hard boundary — no cross-env action.
	if h.Env != sessEnv {
		return nil, excerrors.New("FORBIDDEN",
			"host is outside the session's environment context")
	}
	if !spec.from[h.State] {
		return nil, excerrors.New("INVALID_LIFECYCLE_TRANSITION",
			fmt.Sprintf("host %d is %s; %s requires %s", hostID, h.State,
				action, "an eligible source state"))
	}
	// §19.16.4: prod server actions are dual-controlled sensitive ops.
	var approver *int64
	if sessEnv == EnvProduction {
		if err := s.requireApprover(ctx, actor, gateway.RoleSuperAdmin); err != nil {
			return nil, err
		}
		v := actor.ApproverID
		approver = &v
	}
	// Watermark the prod context switch (spec §19.16.4).
	if err := s.WatermarkProdContext(ctx, tx, actor, "host."+action); err != nil {
		return nil, err
	}

	var sa ServerAction
	err = tx.QueryRow(ctx, `
		INSERT INTO server_actions
		    (host_id, action, status, requested_by, approved_by, reason,
		     decided_at)
		VALUES ($1, $2, 'EXECUTED', $3, $4, $5, now())
		RETURNING id, host_id, action, status, requested_by, approved_by,
		          reason, created_at, decided_at`,
		hostID, action, actor.UserID, approver, reason).
		Scan(&sa.ID, &sa.HostID, &sa.Action, &sa.Status, &sa.RequestedBy,
			&sa.ApprovedBy, &sa.Reason, &sa.CreatedAt, &sa.DecidedAt)
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "insert server_action", err)
	}
	if spec.to != "" {
		if _, err := tx.Exec(ctx, `
			UPDATE fleet_hosts SET state = $2, updated_at = now()
			 WHERE id = $1`, hostID, spec.to); err != nil {
			return nil, excerrors.Wrap("INTERNAL_ERROR", "update host state", err)
		}
	}
	after := spec.to
	if after == "" {
		after = h.State // REBOOT preserves state
	}
	if _, _, err := admin.Log(ctx, tx, admin.AuditEntry{
		AdminUserID: actor.UserID,
		Action:      "fleet.host." + lower(action),
		TargetType:  "fleet_host",
		TargetID:    &hostID,
		BeforeState: map[string]any{"state": h.State, "env": h.Env},
		AfterState: map[string]any{
			"state":       after,
			"action_id":   sa.ID,
			"approved_by": approver,
			"reason":      reason,
		},
		IPAddress: actor.ClientIP,
	}); err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "audit log", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "commit", err)
	}
	return &sa, nil
}

func lower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + 32
		}
	}
	return string(b)
}
