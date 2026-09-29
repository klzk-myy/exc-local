// Phase-11 Task 11.3.12 — multi-dimensional scoped emergency
// kill-switch control plane, plus the Task 11.3.4 global-halt and
// Task 11.3.8 scoped-halt mutation surface.
//
// Mutations are dual-controlled under the §8.2 four-eyes convention:
// GLOBAL set/clear require a distinct approver (approver_id in the same
// call — the §8.2 contract for ops that cannot wait on the
// DualControlService queue); scoped kills are single-approver. Every
// transition lands in trading_suspensions (migration 200) +
// admin_audit_log inside one transaction; the Redis `halt:*`
// enforcement flag raises after commit (Redis cannot join the tx — the
// same post-commit pattern as killSessions in lifecycle.go).
//
// Broadcast: every state change fans out (best-effort, never masking
// committed state) to (a) the public announcement feed, (b) the WS
// "venue.status" advisory channel, and (c) the Aeron/NATS control topic
// `exchange:control:killswitch` consumed by the C++ matching shards
// (Task 11.3.12 replication path). Seam nil-checks log and continue —
// the halt itself must not depend on a downstream transport.
package admin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	excerrors "exchange/pkg/errors"
)

// KillSwitchControlTopic is the Task 11.3.12 control message topic —
// published to Aeron (replicated to C++ matching shards) and bridged to
// NATS for other Go consumers.
const KillSwitchControlTopic = "exchange:control:killswitch"

// VenueStatusChannel is the public WS advisory channel.
const VenueStatusChannel = "venue.status"

// killSwitchRoles may initiate OR approve a suspension (Risk Manager or
// stronger per the route registry; Super Admin holds all permissions).
var killSwitchRoles = map[string]bool{
	"Risk Manager": true,
	"Super Admin":  true,
}

// SuspensionEvent is the broadcast payload for set/clear transitions.
type SuspensionEvent struct {
	Event       string `json:"event"`  // "KILLSWITCH"
	Action      string `json:"action"` // SET | CLEAR
	Scope       string `json:"scope"`
	TargetID    string `json:"target_id"`
	Reason      string `json:"reason"`
	InitiatedBy int64  `json:"initiated_by"`
	ApprovedBy  int64  `json:"approved_by,omitempty"`
	TsMs        int64  `json:"ts_ms"`
}

// SuspensionAnnouncer publishes a public status-page announcement for a
// suspension transition (marketapi store adapter; nil disables).
type SuspensionAnnouncer interface {
	AnnounceSuspension(ctx context.Context, ev SuspensionEvent) error
}

// WSSuspensionPublisher fans the advisory out on VenueStatusChannel —
// *ws.Server satisfies it directly; nil disables.
type WSSuspensionPublisher interface {
	Publish(channel string, data any)
}

// KillSwitchControlPublisher emits the Task 11.3.12 control message on
// KillSwitchControlTopic (NATS/Aeron bridge adapter); nil disables.
type KillSwitchControlPublisher interface {
	PublishControl(ctx context.Context, topic string, ev SuspensionEvent) error
}

// RestingOrderCanceller cancels a counterparty's resting orders — the
// SCOPE_COUNTERPARTY side effect (Task 11.3.12). nil skips the sweep
// (the C++ shard still cancels its side on the control message).
type RestingOrderCanceller interface {
	CancelAccountOrders(ctx context.Context, accountID int64, reason string) (int, error)
}

// KillSwitchFlagWriter raises/clears the Redis enforcement flags —
// *redis.Client satisfies it; nil fails closed (mutations refuse to
// record a suspension that cannot take effect).
type KillSwitchFlagWriter interface {
	SetHaltScope(ctx context.Context, scope, target, reason string) error
	ClearHaltScope(ctx context.Context, scope, target string) error
}

// KillSwitchService is the suspension control plane.
type KillSwitchService struct {
	pool    *pgxpool.Pool
	store   *KillSwitchStore
	flags   KillSwitchFlagWriter
	roles   AdminRoleResolver
	ann     SuspensionAnnouncer
	ws      WSSuspensionPublisher
	ctrl    KillSwitchControlPublisher
	cancels RestingOrderCanceller
	logf    func(format string, args ...any)
	now     func() time.Time
}

// KillSwitchDeps wires the service; Flags, Roles and Store are required
// — a suspension that cannot take effect (no flag writer) or that
// cannot be authorized (no role resolver) must fail closed, not record.
type KillSwitchDeps struct {
	Pool      *pgxpool.Pool
	Store     *KillSwitchStore // nil → built over Pool
	Flags     KillSwitchFlagWriter
	Roles     AdminRoleResolver
	Announcer SuspensionAnnouncer
	WS        WSSuspensionPublisher
	Control   KillSwitchControlPublisher
	Canceller RestingOrderCanceller
	Logf      func(format string, args ...any)
	Now       func() time.Time
}

func NewKillSwitchService(d KillSwitchDeps) (*KillSwitchService, error) {
	if d.Pool == nil {
		return nil, fmt.Errorf("kill-switch: pgx pool is nil")
	}
	if d.Flags == nil {
		return nil, fmt.Errorf("kill-switch: halt flag writer is nil")
	}
	if d.Roles == nil {
		return nil, fmt.Errorf("kill-switch: role resolver is nil")
	}
	st := d.Store
	if st == nil {
		st = NewKillSwitchStore(d.Pool)
	}
	s := &KillSwitchService{
		pool: d.Pool, store: st, flags: d.Flags, roles: d.Roles,
		ann: d.Announcer, ws: d.WS, ctrl: d.Control, cancels: d.Canceller,
		logf: d.Logf, now: d.Now,
	}
	if s.now == nil {
		s.now = time.Now
	}
	if s.logf == nil {
		s.logf = func(string, ...any) {}
	}
	return s, nil
}

// Store exposes the record store for the admin status handler.
func (s *KillSwitchService) Store() *KillSwitchStore { return s.store }

// ---------------------------------------------------------------------------
// Authorization — synchronous four-eyes (§8.2 contract for ops that
// cannot wait on the dual-control queue).
// ---------------------------------------------------------------------------

// requireKillRole resolves a user's admin role fail-closed: nil
// resolver → UNAUTHORIZED_ROLE, store error → INTERNAL_ERROR,
// ineligible role → UNAUTHORIZED_ROLE.
func (s *KillSwitchService) requireKillRole(ctx context.Context, userID int64) (string, error) {
	if s.roles == nil {
		return "", excerrors.New("UNAUTHORIZED_ROLE",
			"kill-switch requires an admin role — resolver unavailable")
	}
	role, err := s.roles(ctx, userID)
	if err != nil {
		return "", excerrors.New("INTERNAL_ERROR",
			"kill-switch role resolution failed: "+err.Error())
	}
	if !killSwitchRoles[role] {
		return "", excerrors.New("UNAUTHORIZED_ROLE",
			"kill-switch requires Risk Manager or stronger")
	}
	return role, nil
}

// requireApprover enforces the dual-control leg: approver must be a
// distinct, non-zero, eligible admin.
func (s *KillSwitchService) requireApprover(ctx context.Context, actor, approver int64) error {
	if approver <= 0 {
		return excerrors.New("DUAL_CONTROL_REQUIRED",
			"kill-switch requires a second authorizer (approver_id)")
	}
	if approver == actor {
		return excerrors.New("DUAL_CONTROL_REQUIRED",
			"approver must be a different admin than the initiator")
	}
	if _, err := s.requireKillRole(ctx, approver); err != nil {
		return err
	}
	return nil
}

// DualControlRequired reports whether the scope's set/clear is
// four-eyes: GLOBAL always; COUNTERPARTY is a destructive scope
// (Task 11.3.8 approver check — the kill cancels resting orders).
func DualControlRequired(scope string) bool {
	return scope == ScopeGlobal || scope == ScopeCounterparty
}

// ---------------------------------------------------------------------------
// Set / Clear
// ---------------------------------------------------------------------------

// normalize validates the operator request into the record shape.
func normalizeScope(scope, target string) (string, string, error) {
	sc := strings.ToUpper(strings.TrimSpace(scope))
	if !ValidScope(sc) {
		return "", "", excerrors.New("INVALID_REQUEST",
			"scope must be one of GLOBAL|ACCOUNT|COUNTERPARTY|INSTRUMENT|"+
				"INSTRUMENT_CLASS|FIX_SESSION|LP|RAIL|REGION|ENV|DESK")
	}
	t := canonicalTarget(sc, target)
	if sc != ScopeGlobal && t == "" {
		return "", "", excerrors.New("INVALID_REQUEST",
			"target_id is required for scope "+sc)
	}
	if sc == ScopeGlobal {
		t = ""
	}
	return sc, t, nil
}

// Set raises a suspension: records ACTIVE row + audit entry in one tx,
// then raises the Redis enforcement flag and broadcasts. GLOBAL and
// COUNTERPARTY require actor.ApproverID (dual control); other scopes
// are single-approver per Task 11.3.8.
func (s *KillSwitchService) Set(ctx context.Context, actor AdminActor,
	scope, target, reason string) (*Suspension, error) {
	if actor.UserID <= 0 {
		return nil, excerrors.New("UNAUTHORIZED", "admin identity required")
	}
	sc, tgt, err := normalizeScope(scope, target)
	if err != nil {
		return nil, err
	}
	reason = strings.TrimSpace(reason)
	if len(reason) < 8 {
		return nil, excerrors.New("INVALID_REQUEST",
			"kill-switch requires an operator reason (>= 8 chars)")
	}
	if _, err := s.requireKillRole(ctx, actor.UserID); err != nil {
		return nil, err
	}
	var approver *int64
	if DualControlRequired(sc) {
		if err := s.requireApprover(ctx, actor.UserID, actor.ApproverID); err != nil {
			return nil, err
		}
		approver = &actor.ApproverID
	} else if actor.ApproverID > 0 && actor.ApproverID != actor.UserID {
		// Optional second signature on scoped kills is recorded but not
		// required — it must still be a resolvable admin.
		if _, err := s.requireKillRole(ctx, actor.ApproverID); err != nil {
			return nil, err
		}
		approver = &actor.ApproverID
	}

	rec := &Suspension{Scope: sc, TargetID: tgt, Reason: reason,
		InitiatedBy: actor.UserID, ApprovedBy: approver, ClientIP: actor.ClientIP}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, excerrors.New("INTERNAL_ERROR", "suspension tx: "+err.Error())
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Idempotent: a repeated SET on an already-suspended scope returns
	// the existing ACTIVE record — the console must not error in an
	// emergency. The partial unique index backstops the race.
	if existing, rerr := s.store.ActiveByScope(ctx, tx, sc, tgt); rerr != nil {
		return nil, excerrors.New("INTERNAL_ERROR", "suspension read: "+rerr.Error())
	} else if existing != nil {
		return existing, nil
	}

	id, err := s.store.InsertActive(ctx, tx, rec)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			// Concurrent SET won the unique-index race — surface the
			// winning record rather than a conflict error.
			if existing, rerr := s.store.ActiveByScope(ctx, tx, sc, tgt); rerr == nil && existing != nil {
				return existing, nil
			}
		}
		return nil, excerrors.New("INTERNAL_ERROR", err.Error())
	}
	rec.SuspensionID = id
	_, _, err = Log(ctx, tx, AuditEntry{
		AdminUserID: actor.UserID, Action: "killswitch.set",
		TargetType: "suspension", TargetID: &id,
		AfterState: rec, IPAddress: actor.ClientIP,
	})
	if err != nil {
		return nil, excerrors.New("INTERNAL_ERROR", "audit write: "+err.Error())
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, excerrors.New("INTERNAL_ERROR", "suspension commit: "+err.Error())
	}

	// Post-commit: raise the enforcement flag. Redis cannot join the tx;
	// a failure here leaves the durable record ACTIVE and ReconcileFlags
	// re-raises on boot — but the caller must know enforcement is not
	// live, so the error is returned, not swallowed.
	if err := s.flags.SetHaltScope(ctx, sc, tgt, reason); err != nil {
		return rec, excerrors.New("INTERNAL_ERROR",
			fmt.Sprintf("suspension %d recorded but halt flag raise failed "+
				"(run reconcile / retry): %v", id, err))
	}

	s.broadcast(ctx, SuspensionEvent{
		Event: "KILLSWITCH", Action: "SET", Scope: sc, TargetID: tgt,
		Reason: reason, InitiatedBy: actor.UserID,
		ApprovedBy: deref(approver), TsMs: s.now().UnixMilli(),
	})

	// SCOPE_COUNTERPARTY (Task 11.3.12): halt order entry AND cancel
	// resting orders for the targeted institutional user.
	if sc == ScopeCounterparty && s.cancels != nil {
		if acctID, perr := strconv.ParseInt(tgt, 10, 64); perr == nil && acctID > 0 {
			if n, cerr := s.cancels.CancelAccountOrders(ctx, acctID,
				"counterparty kill-switch"); cerr != nil {
				s.logf("counterparty kill-switch: resting-order sweep failed "+
					"for account %d: %v", acctID, cerr)
			} else {
				s.logf("counterparty kill-switch: %d resting orders cancelled "+
					"for account %d", n, acctID)
			}
		}
	}
	return rec, nil
}

// Clear resumes a scope: marks the durable record CLEARED + audits in
// one tx, then removes the Redis flag and broadcasts. Dual control
// applies to the same scopes as Set (a GLOBAL reset is itself
// dual-controlled per Task 11.3.4 AC).
func (s *KillSwitchService) Clear(ctx context.Context, actor AdminActor,
	scope, target, reason string) (*Suspension, error) {
	if actor.UserID <= 0 {
		return nil, excerrors.New("UNAUTHORIZED", "admin identity required")
	}
	sc, tgt, err := normalizeScope(scope, target)
	if err != nil {
		return nil, err
	}
	if _, err := s.requireKillRole(ctx, actor.UserID); err != nil {
		return nil, err
	}
	var approver *int64
	if DualControlRequired(sc) {
		if err := s.requireApprover(ctx, actor.UserID, actor.ApproverID); err != nil {
			return nil, err
		}
		approver = &actor.ApproverID
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, excerrors.New("INTERNAL_ERROR", "reset tx: "+err.Error())
	}
	defer func() { _ = tx.Rollback(ctx) }()

	rec, err := s.store.ActiveByScope(ctx, tx, sc, tgt)
	if err != nil {
		return nil, excerrors.New("INTERNAL_ERROR", "suspension read: "+err.Error())
	}
	if rec == nil {
		return nil, excerrors.New("NOT_FOUND",
			"no ACTIVE suspension for "+sc+tgt)
	}
	if err := s.store.MarkCleared(ctx, tx, rec.SuspensionID,
		actor.UserID, approver, reason); err != nil {
		return nil, err
	}
	rec.State = "CLEARED"
	rec.ClearedBy = &actor.UserID
	rec.ClearApprovedBy = approver
	rec.ClearedReason = reason
	now := s.now()
	rec.ClearedAt = &now

	_, _, err = Log(ctx, tx, AuditEntry{
		AdminUserID: actor.UserID, Action: "killswitch.clear",
		TargetType: "suspension", TargetID: &rec.SuspensionID,
		AfterState: rec, IPAddress: actor.ClientIP,
	})
	if err != nil {
		return nil, excerrors.New("INTERNAL_ERROR", "audit write: "+err.Error())
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, excerrors.New("INTERNAL_ERROR", "reset commit: "+err.Error())
	}

	if err := s.flags.ClearHaltScope(ctx, sc, tgt); err != nil {
		return rec, excerrors.New("INTERNAL_ERROR",
			fmt.Sprintf("suspension %d cleared in ledger but halt flag removal "+
				"failed (run reconcile / retry): %v", rec.SuspensionID, err))
	}

	s.broadcast(ctx, SuspensionEvent{
		Event: "KILLSWITCH", Action: "CLEAR", Scope: sc, TargetID: tgt,
		Reason: reason, InitiatedBy: actor.UserID,
		ApprovedBy: deref(approver), TsMs: s.now().UnixMilli(),
	})
	return rec, nil
}

// Status returns the ACTIVE suspension set for the admin surface.
func (s *KillSwitchService) Status(ctx context.Context) ([]Suspension, error) {
	return s.store.ListActive(ctx)
}

// ReconcileFlags re-raises every ACTIVE suspension's Redis flag — run
// at service boot so a Redis flush/failover cannot silently drop the
// halt envelope (PostgreSQL is authoritative; Redis is the hot path).
func (s *KillSwitchService) ReconcileFlags(ctx context.Context) (int, error) {
	rows, err := s.store.ListActive(ctx)
	if err != nil {
		return 0, err
	}
	var raised int
	for _, r := range rows {
		if err := s.flags.SetHaltScope(ctx, r.Scope, r.TargetID, r.Reason); err != nil {
			return raised, fmt.Errorf("reconcile suspension %d: %w",
				r.SuspensionID, err)
		}
		raised++
	}
	return raised, nil
}

// broadcast fans the event out to all transports; delivery failures are
// logged, never mask the committed state (OpsAlerter convention).
func (s *KillSwitchService) broadcast(ctx context.Context, ev SuspensionEvent) {
	if s.ctrl != nil {
		if err := s.ctrl.PublishControl(ctx, KillSwitchControlTopic, ev); err != nil {
			s.logf("kill-switch control publish failed: %v", err)
		}
	}
	if s.ann != nil {
		if err := s.ann.AnnounceSuspension(ctx, ev); err != nil {
			s.logf("kill-switch announcement failed: %v", err)
		}
	}
	if s.ws != nil {
		s.ws.Publish(VenueStatusChannel, ev)
	}
}

func deref(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}

// MarshalJSON — helper for the NATS adapter; keeps the control-message
// wire shape identical to the WS advisory payload.
func (e SuspensionEvent) Marshal() ([]byte, error) { return json.Marshal(e) }
