// Phase-21 Task 21.3.24 — restricted-list administration (spec
// §14.10.1). The table lives in migration 079 (employee dealing);
// this service owns the CRUD + scheduled-event widening:
//
//   - Compliance Officer / Super Admin mutate (create, retire).
//   - Read-Only Auditor reads (the audit surface joins admin_audit_log).
//   - Blackout windows widen automatically around scheduled events:
//     SyncScheduledEvents scans maintenance_windows (migration 172)
//     and benchmark_fixings (migration 222) for events overlapping a
//     list's window±margin and extends the stored window to cover
//     them — an insider with knowledge of an unannounced maintenance
//     outage cannot lean on a too-narrow blackout.
//
// The order-entry gate itself is the gateway dispatch seam
// (orders.DealingGate → compliance.EmployeeDealingService), which
// reads restricted_lists directly — this package never gates trading.
package admin

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	excerrors "exchange/pkg/errors"
)

// Restricted-list event vocabulary (CHECK mirrors in migration 079).
const (
	RestrictedEventAuction       = "AUCTION"
	RestrictedEventOracleOutage  = "ORACLE_OUTAGE"
	RestrictedEventMaintenance   = "MAINTENANCE"
	RestrictedEventEmergencyRule = "EMERGENCY_RULE_CHANGE"
	RestrictedEventRateFix       = "RATE_FIX"
)

// Restricted-list scopes.
const (
	RestrictedScopeAll   = "ALL_EMPLOYEES"
	RestrictedScopeRole  = "ROLE"
	RestrictedScopeNamed = "NAMED"
)

// RestrictedList is one restricted_lists row.
type RestrictedList struct {
	ID            int64     `json:"id"`
	EventID       string    `json:"event_id"`
	EventType     string    `json:"event_type"`
	Instruments   []string  `json:"instruments"` // empty = venue-wide
	WindowStart   time.Time `json:"window_start"`
	WindowEnd     time.Time `json:"window_end"`
	WidenMinutes  int       `json:"widen_minutes"` // auto-widen margin
	Scope         string    `json:"scope"`
	ScopeRole     *string   `json:"scope_role,omitempty"`
	NamedAccounts []int64   `json:"named_accounts,omitempty"`
	Status        string    `json:"status"`
	Reason        string    `json:"reason"`
	CreatedBy     int64     `json:"created_by"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// restrictedListRoles may administer the list — Compliance Officer
// domain (dealing controls) per spec §8.2.
var restrictedListRoles = map[string]bool{
	RoleComplianceOfficer: true,
	RoleSuperAdmin:        true,
}

// RestrictedListService owns the register. resolver is the shared
// RoleResolver signature (Store.RoleResolver).
type RestrictedListService struct {
	pool     *pgxpool.Pool
	resolver func(ctx context.Context, userID int64) (string, error)
	now      func() time.Time
}

// NewRestrictedListService binds the service.
func NewRestrictedListService(pool *pgxpool.Pool,
	resolver func(ctx context.Context, userID int64) (string, error)) *RestrictedListService {
	return &RestrictedListService{pool: pool, resolver: resolver,
		now: time.Now}
}

// WithClock overrides the clock (tests).
func (s *RestrictedListService) WithClock(f func() time.Time) *RestrictedListService {
	if f != nil {
		s.now = f
	}
	return s
}

func (s *RestrictedListService) checkRole(ctx context.Context,
	userID int64) error {
	if s.resolver == nil {
		return excerrors.New("INTERNAL_ERROR", "role resolver not wired")
	}
	role, err := s.resolver(ctx, userID)
	if err != nil {
		return excerrors.New("INTERNAL_ERROR", "role lookup: "+err.Error())
	}
	if !restrictedListRoles[role] {
		return excerrors.New("UNAUTHORIZED_ROLE",
			"role "+role+" cannot administer restricted lists")
	}
	return nil
}

// validate enforces the shape the gate depends on.
func (l *RestrictedList) validate() error {
	switch l.EventType {
	case RestrictedEventAuction, RestrictedEventOracleOutage,
		RestrictedEventMaintenance, RestrictedEventEmergencyRule,
		RestrictedEventRateFix:
	default:
		return excerrors.New("INVALID_REQUEST",
			"event_type must be AUCTION|ORACLE_OUTAGE|MAINTENANCE|"+
				"EMERGENCY_RULE_CHANGE|RATE_FIX")
	}
	if l.EventID == "" {
		return excerrors.New("INVALID_REQUEST", "event_id is required")
	}
	if !l.WindowEnd.After(l.WindowStart) {
		return excerrors.New("INVALID_REQUEST",
			"window_end must follow window_start")
	}
	switch l.Scope {
	case "", RestrictedScopeAll:
		l.Scope = RestrictedScopeAll
	case RestrictedScopeRole:
		if l.ScopeRole == nil || *l.ScopeRole == "" {
			return excerrors.New("INVALID_REQUEST",
				"SCOPE=ROLE requires scope_role")
		}
	case RestrictedScopeNamed:
		if len(l.NamedAccounts) == 0 {
			return excerrors.New("INVALID_REQUEST",
				"SCOPE=NAMED requires named_accounts")
		}
	default:
		return excerrors.New("INVALID_REQUEST",
			"scope must be ALL_EMPLOYEES|ROLE|NAMED")
	}
	return nil
}

// Create registers a blackout window (Compliance Officer). Multiple
// rows may share an event_id (different scopes); the gate evaluates
// the union.
func (s *RestrictedListService) Create(ctx context.Context, actorID int64,
	l RestrictedList) (*RestrictedList, error) {
	if err := s.checkRole(ctx, actorID); err != nil {
		return nil, err
	}
	if l.WidenMinutes <= 0 {
		l.WidenMinutes = 30
	}
	if err := l.validate(); err != nil {
		return nil, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, excerrors.New("INTERNAL_ERROR", "list tx: "+err.Error())
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var out RestrictedList
	err = tx.QueryRow(ctx, `
		INSERT INTO restricted_lists
		    (event_id, event_type, instruments, window_start, window_end,
		     widen_minutes, scope, scope_role, named_accounts,
		     reason, created_by)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
		RETURNING id, event_id, event_type, instruments, window_start,
		          window_end, widen_minutes, scope, scope_role,
		          named_accounts, status, reason, created_by,
		          created_at, updated_at`,
		l.EventID, l.EventType, l.Instruments, l.WindowStart,
		l.WindowEnd, l.WidenMinutes, l.Scope, l.ScopeRole,
		l.NamedAccounts, l.Reason, actorID).
		Scan(&out.ID, &out.EventID, &out.EventType, &out.Instruments,
			&out.WindowStart, &out.WindowEnd, &out.WidenMinutes,
			&out.Scope, &out.ScopeRole, &out.NamedAccounts, &out.Status,
			&out.Reason, &out.CreatedBy, &out.CreatedAt, &out.UpdatedAt)
	if err != nil {
		return nil, excerrors.New("INTERNAL_ERROR", "list insert: "+err.Error())
	}
	if _, _, err := Log(ctx, tx, AuditEntry{
		AdminUserID: actorID, Action: "restricted_list.create",
		TargetType: "restricted_list", TargetID: &out.ID,
		AfterState: out,
	}); err != nil {
		return nil, excerrors.New("INTERNAL_ERROR", "list audit: "+err.Error())
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, excerrors.New("INTERNAL_ERROR", "list commit: "+err.Error())
	}
	return &out, nil
}

// Retire marks a list inactive — rows are never deleted (audit spine).
func (s *RestrictedListService) Retire(ctx context.Context, actorID,
	id int64) error {
	if err := s.checkRole(ctx, actorID); err != nil {
		return err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return excerrors.New("INTERNAL_ERROR", "retire tx: "+err.Error())
	}
	defer func() { _ = tx.Rollback(ctx) }()
	tag, err := tx.Exec(ctx, `
		UPDATE restricted_lists SET status='RETIRED', updated_at=now()
		 WHERE id = $1 AND status='ACTIVE'`, id)
	if err != nil {
		return excerrors.New("INTERNAL_ERROR", "retire: "+err.Error())
	}
	if tag.RowsAffected() == 0 {
		return excerrors.New("NOT_FOUND", "no active restricted list")
	}
	if _, _, err := Log(ctx, tx, AuditEntry{
		AdminUserID: actorID, Action: "restricted_list.retire",
		TargetType: "restricted_list", TargetID: &id,
	}); err != nil {
		return excerrors.New("INTERNAL_ERROR", "retire audit: "+err.Error())
	}
	return tx.Commit(ctx)
}

// List returns ACTIVE rows (status=” → all), newest first.
func (s *RestrictedListService) List(ctx context.Context,
	status string, limit int) ([]RestrictedList, error) {
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	q := `SELECT id, event_id, event_type, instruments, window_start,
	             window_end, widen_minutes, scope, scope_role,
	             named_accounts, status, reason, created_by,
	             created_at, updated_at
	        FROM restricted_lists`
	args := []any{}
	if status != "" {
		q += " WHERE status = $1"
		args = append(args, status)
	}
	q += fmt.Sprintf(" ORDER BY id DESC LIMIT %d", limit)
	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, excerrors.New("INTERNAL_ERROR", "list read: "+err.Error())
	}
	defer rows.Close()
	var out []RestrictedList
	for rows.Next() {
		var l RestrictedList
		if err := rows.Scan(&l.ID, &l.EventID, &l.EventType,
			&l.Instruments, &l.WindowStart, &l.WindowEnd,
			&l.WidenMinutes, &l.Scope, &l.ScopeRole, &l.NamedAccounts,
			&l.Status, &l.Reason, &l.CreatedBy, &l.CreatedAt,
			&l.UpdatedAt); err != nil {
			return nil, excerrors.New("INTERNAL_ERROR",
				"list row: "+err.Error())
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// SyncScheduledEvents widens ACTIVE windows to cover scheduled events
// that overlap [window_start − margin, window_end + margin]:
// maintenance_windows (SCHEDULED/IN_PROGRESS) and upcoming
// benchmark_fixings rows extend the blackout so staff with privileged
// visibility of the schedule cannot trade inside it. Returns rows
// widened.
func (s *RestrictedListService) SyncScheduledEvents(ctx context.Context) (int, error) {
	// Maintenance windows: venue-wide or instrument-overlapping rows
	// push the restricted window outward.
	res, err := s.pool.Exec(ctx, `
		UPDATE restricted_lists r
		   SET window_start = LEAST(r.window_start, m.starts_at),
		       window_end   = GREATEST(r.window_end, m.ends_at),
		       updated_at   = now()
		  FROM maintenance_windows m
		 WHERE r.status = 'ACTIVE'
		   AND r.event_type = 'MAINTENANCE'
		   AND m.status IN ('SCHEDULED','IN_PROGRESS')
		   AND m.starts_at <= r.window_end + (r.widen_minutes || ' minutes')::interval
		   AND m.ends_at   >= r.window_start - (r.widen_minutes || ' minutes')::interval
		   AND (m.scope <> 'INSTRUMENT'
		        OR m.symbols && r.instruments
		        OR r.instruments = '{}')
		   AND (m.starts_at < r.window_start OR m.ends_at > r.window_end)`)
	if err != nil {
		return 0, excerrors.New("INTERNAL_ERROR", "maintenance widen: "+err.Error())
	}
	n := int(res.RowsAffected())

	// Rate-fix events: the fixing's scheduled_at inside the widened
	// window edge extends the window to cover the fix.
	res, err = s.pool.Exec(ctx, `
		UPDATE restricted_lists r
		   SET window_start = LEAST(r.window_start, f.scheduled_at),
		       window_end   = GREATEST(r.window_end, f.scheduled_at),
		       updated_at   = now()
		  FROM benchmark_fixings f
		 WHERE r.status = 'ACTIVE'
		   AND r.event_type = 'RATE_FIX'
		   AND f.scheduled_at BETWEEN
		       r.window_start - (r.widen_minutes || ' minutes')::interval
		       AND r.window_end + (r.widen_minutes || ' minutes')::interval
		   AND (r.instruments = '{}' OR f.symbol = ANY(r.instruments))
		   AND (f.scheduled_at < r.window_start
		        OR f.scheduled_at > r.window_end)`)
	if err != nil {
		return n, excerrors.New("INTERNAL_ERROR", "fixing widen: "+err.Error())
	}
	return n + int(res.RowsAffected()), nil
}
