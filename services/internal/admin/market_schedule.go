// market_schedule.go — Phase-15 Task 15.3.4 (Go side): the `market:hours`
// Redis publication the C++ PreTradeChecker cluster consumes for 24/5
// trading-hours enforcement (spec §1, §6.7; §24 #217/#343).
//
// Ownership boundary:
//
//	THIS FILE owns the market:hours key — PostgreSQL
//	market_schedule_overrides (migration 221) is the system of record;
//	the Redis key is a read-through projection rewritten (a) on every
//	committed override mutation and (b) unconditionally at gateway boot
//	(Reconcile), so the C++ poller always sees a verified state.
//
//	core/src/risk/PreTradeChecker.cpp (Task 15.3.3/15.3.4 C++ side) owns
//	the admission check: new orders outside the window reject
//	MARKET_CLOSED; cancels always allowed.
//
// Published payload (JSON STRING, no TTL — coordination instance):
//
//	{"open_utc":"SUN 21:00","close_utc":"FRI 22:00",
//	 "pre_open_utc":"SUN 20:45",
//	 "overrides":[{"date":"YYYY-MM-DD","closed":true},
//	              {"date":"YYYY-MM-DD","closed":false,
//	               "open":"HH:MM","close":"HH:MM"}],
//	 "published_at":"<RFC3339>","version":<seq>}
//
// Contract notes for the C++ consumer (documented seam — core does not
// yet read this key):
//   - open_utc/close_utc are the canonical weekly window (Sunday 21:00 →
//     Friday 22:00 UTC). pre_open_utc (Sunday 20:45 UTC) is the order-
//     entry boundary: the §6.7 PRE_OPEN auction CALL accepts orders into
//     the book from that instant while matching stays suppressed by the
//     instrument:auction:{symbol} CALL key (session_lifecycle.go).
//     Without this field the hours gate would reject pre-open order
//     entry 20:45–21:00 and the CALL phase could never accumulate.
//   - overrides is sorted by date and only carries future/current rows;
//     closed=true → the whole UTC date is untradeable; closed=false →
//     open/close bound a shortened session inside the canonical window
//     for that date.
//   - version increments on every republish; a consumer that sees an
//     unchanged version may skip re-parsing.
package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	excredis "exchange/internal/redis"
	excerrors "exchange/pkg/errors"
)

// MarketHoursKey is the coordination-instance key the C++ hours gate
// polls. No TTL — the schedule is durable state, not a cache entry.
const MarketHoursKey = "market:hours"

// Canonical weekly window (spec §1 / §6.7): Sydney open Sunday 21:00 UTC
// → New York close Friday 22:00 UTC; pre-open order entry from 20:45.
const (
	marketOpenUTC    = "SUN 21:00"
	marketCloseUTC   = "FRI 22:00"
	marketPreOpenUTC = "SUN 20:45"
)

// ScheduleOverride is one market_schedule_overrides row — a single UTC
// civil date either fully closed or carrying a shortened session.
type ScheduleOverride struct {
	ID        int64     `json:"id"`
	Date      string    `json:"date"` // YYYY-MM-DD (UTC)
	Closed    bool      `json:"closed"`
	Open      string    `json:"open,omitempty"`  // "HH:MM" UTC, partial only
	Close     string    `json:"close,omitempty"` // "HH:MM" UTC, partial only
	Reason    string    `json:"reason"`
	CreatedBy int64     `json:"created_by"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	UpdatedBy *int64    `json:"updated_by,omitempty"`
}

// OverrideInput is the CRUD request shape. Date is a UTC civil date;
// Closed=true forbids Open/Close, Closed=false requires both.
type OverrideInput struct {
	Date   string `json:"date"`
	Closed bool   `json:"closed"`
	Open   string `json:"open,omitempty"`
	Close  string `json:"close,omitempty"`
	Reason string `json:"reason"`
}

// marketHoursDoc is the exact payload written to market:hours.
type marketHoursDoc struct {
	OpenUTC     string             `json:"open_utc"`
	CloseUTC    string             `json:"close_utc"`
	PreOpenUTC  string             `json:"pre_open_utc"`
	Overrides   []ScheduleOverride `json:"overrides"`
	PublishedAt string             `json:"published_at"`
	Version     int64              `json:"version"`
}

// MarketScheduleService owns the schedule document and its Redis
// projection.
type MarketScheduleService struct {
	pool  *pgxpool.Pool
	rdb   *excredis.Client
	roles AdminRoleResolver // nil fails closed on every gated call
	now   func() time.Time
}

// NewMarketScheduleService wires the service; pool and rdb are required
// (fail-closed), roles nil is allowed but rejects every call.
func NewMarketScheduleService(pool *pgxpool.Pool, rdb *excredis.Client, roles AdminRoleResolver) (*MarketScheduleService, error) {
	if pool == nil {
		return nil, fmt.Errorf("market schedule: nil pgx pool")
	}
	if rdb == nil {
		return nil, fmt.Errorf("market schedule: nil redis client")
	}
	return &MarketScheduleService{pool: pool, rdb: rdb, roles: roles, now: time.Now}, nil
}

// SetClockForTest overrides the clock; tests only.
func (s *MarketScheduleService) SetClockForTest(now func() time.Time) { s.now = now }

// scheduleReadRoles may inspect the schedule document and overrides.
// Every resolvable venue-admin role reads; mutations need lpManageRoles.
var scheduleWriteRoles = map[string]bool{
	RoleRiskManager: true,
	RoleSuperAdmin:  true,
}

func (s *MarketScheduleService) requireRole(ctx context.Context, actor AdminActor, write bool) error {
	if s.roles == nil {
		return excerrors.New("UNAUTHORIZED_ROLE", "admin role resolver unavailable")
	}
	role, err := s.roles(ctx, actor.UserID)
	if err != nil || role == "" {
		return excerrors.New("UNAUTHORIZED_ROLE", "caller is not a venue admin")
	}
	if write && !scheduleWriteRoles[role] {
		return excerrors.New("UNAUTHORIZED_ROLE",
			"market schedule mutations require Risk Manager or Super Admin")
	}
	return nil
}

// ---------------------------------------------------------------------------
// Validation
// ---------------------------------------------------------------------------

var hhmmRe = func(s string) bool {
	if len(s) != 5 || s[2] != ':' {
		return false
	}
	h, err1 := strconv.Atoi(s[:2])
	m, err2 := strconv.Atoi(s[3:])
	return err1 == nil && err2 == nil && h >= 0 && h <= 23 && m >= 0 && m <= 59
}

// validate enforces the shape the CHECK constraints (and the C++
// consumer) rely on, so violations surface as clean coded errors.
func (s *MarketScheduleService) validate(in OverrideInput) error {
	d, err := time.ParseInLocation("2006-01-02", strings.TrimSpace(in.Date), time.UTC)
	if err != nil {
		return excerrors.New("INVALID_REQUEST", "date must be YYYY-MM-DD (UTC civil date)")
	}
	// Overrides are forward-looking operational tools — a past-date row
	// can never take effect and would only muddy the audit trail.
	today := time.Date(s.now().UTC().Year(), s.now().UTC().Month(), s.now().UTC().Day(),
		0, 0, 0, 0, time.UTC)
	if d.Before(today) {
		return excerrors.New("INVALID_REQUEST", "override date must not be in the past")
	}
	if strings.TrimSpace(in.Reason) == "" || len(in.Reason) > 512 {
		return excerrors.New("INVALID_REQUEST", "reason required (≤512 chars)")
	}
	if in.Closed {
		if in.Open != "" || in.Close != "" {
			return excerrors.New("INVALID_REQUEST",
				"closed=true overrides must not carry open/close times")
		}
		return nil
	}
	if !hhmmRe(in.Open) || !hhmmRe(in.Close) {
		return excerrors.New("INVALID_REQUEST",
			"partial overrides require open and close in HH:MM UTC")
	}
	if in.Open >= in.Close {
		return excerrors.New("INVALID_REQUEST",
			"partial override requires open < close within the UTC day")
	}
	return nil
}

// ---------------------------------------------------------------------------
// Redis projection
// ---------------------------------------------------------------------------

// Publish loads the override set and rewrites market:hours atomically
// (single SET — readers never observe a half-written document). Rows
// whose date is already past are dropped from the projection (they stay
// on the PG audit record — yesterday's override has no engine meaning).
func (s *MarketScheduleService) Publish(ctx context.Context) (*marketHoursDoc, error) {
	today := time.Date(s.now().UTC().Year(), s.now().UTC().Month(), s.now().UTC().Day(),
		0, 0, 0, 0, time.UTC)
	rows, err := s.pool.Query(ctx, `
		SELECT override_id, override_date, closed,
		       COALESCE(to_char(open_utc,'HH24:MI'),''), COALESCE(to_char(close_utc,'HH24:MI'),''),
		       reason, created_by, created_at, updated_at, updated_by
		  FROM market_schedule_overrides
		 WHERE override_date >= $1
		 ORDER BY override_date`, today)
	if err != nil {
		return nil, fmt.Errorf("market schedule: load overrides: %w", err)
	}
	defer rows.Close()
	ovs, err := scanOverrides(rows)
	if err != nil {
		return nil, err
	}
	doc := s.buildDoc(ovs)
	payload, err := json.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("market schedule: marshal: %w", err)
	}
	if err := s.rdb.Set(ctx, MarketHoursKey, payload, 0).Err(); err != nil {
		return nil, fmt.Errorf("market schedule: publish %s: %w", MarketHoursKey, err)
	}
	return doc, nil
}

// Reconcile is the boot-time contract: unconditionally rewrite
// market:hours from PG so a restarted gateway never leaves a stale (or
// missing) schedule for the C++ poller. Identity: Publish.
func (s *MarketScheduleService) Reconcile(ctx context.Context) error {
	_, err := s.Publish(ctx)
	return err
}

func (s *MarketScheduleService) buildDoc(ovs []ScheduleOverride) *marketHoursDoc {
	version := s.now().UnixNano() // monotonic; ordering beats durability here
	return &marketHoursDoc{
		OpenUTC:     marketOpenUTC,
		CloseUTC:    marketCloseUTC,
		PreOpenUTC:  marketPreOpenUTC,
		Overrides:   ovs,
		PublishedAt: s.now().UTC().Format(time.RFC3339),
		Version:     version,
	}
}

// Current returns the merged schedule document as served to admins —
// the same payload the C++ consumer sees, read back from PG (not the
// Redis projection) so drift between the two is visible.
func (s *MarketScheduleService) Current(ctx context.Context, actor AdminActor) (*marketHoursDoc, error) {
	if err := s.requireRole(ctx, actor, false); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `
		SELECT override_id, override_date, closed,
		       COALESCE(to_char(open_utc,'HH24:MI'),''), COALESCE(to_char(close_utc,'HH24:MI'),''),
		       reason, created_by, created_at, updated_at, updated_by
		  FROM market_schedule_overrides
		 ORDER BY override_date`)
	if err != nil {
		return nil, fmt.Errorf("market schedule: list overrides: %w", err)
	}
	defer rows.Close()
	ovs, err := scanOverrides(rows)
	if err != nil {
		return nil, err
	}
	return s.buildDoc(ovs), nil
}

func scanOverrides(rows pgx.Rows) ([]ScheduleOverride, error) {
	out := []ScheduleOverride{}
	for rows.Next() {
		var (
			o      ScheduleOverride
			d      time.Time
			op, cl string
			ub     *int64
		)
		if err := rows.Scan(&o.ID, &d, &o.Closed, &op, &cl,
			&o.Reason, &o.CreatedBy, &o.CreatedAt, &o.UpdatedAt, &ub); err != nil {
			return nil, fmt.Errorf("market schedule: scan override: %w", err)
		}
		o.Date = d.Format("2006-01-02")
		o.Open, o.Close = op, cl
		o.UpdatedBy = ub
		out = append(out, o)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// CRUD — every mutation commits the PG row AND its audit row in one tx,
// then republishes the Redis projection. A Redis publish failure after
// commit returns an error (the admin sees the write but the engine's
// copy is stale) — the boot reconcile / next mutation rewrites it.
// ---------------------------------------------------------------------------

// ListOverrides returns every override (including expired rows — the
// admin view is the full audit-visible table, unlike the projection).
func (s *MarketScheduleService) ListOverrides(ctx context.Context, actor AdminActor) ([]ScheduleOverride, error) {
	if err := s.requireRole(ctx, actor, false); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `
		SELECT override_id, override_date, closed,
		       COALESCE(to_char(open_utc,'HH24:MI'),''), COALESCE(to_char(close_utc,'HH24:MI'),''),
		       reason, created_by, created_at, updated_at, updated_by
		  FROM market_schedule_overrides
		 ORDER BY override_date`)
	if err != nil {
		return nil, fmt.Errorf("market schedule: list: %w", err)
	}
	defer rows.Close()
	return scanOverrides(rows)
}

// CreateOverride inserts one override row (UNIQUE date — a second row
// for the same civil date conflicts INVALID_REQUEST) and republishes.
func (s *MarketScheduleService) CreateOverride(ctx context.Context, actor AdminActor, in OverrideInput) (*ScheduleOverride, error) {
	if err := s.requireRole(ctx, actor, true); err != nil {
		return nil, err
	}
	if err := s.validate(in); err != nil {
		return nil, err
	}
	d, _ := time.ParseInLocation("2006-01-02", strings.TrimSpace(in.Date), time.UTC)

	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return nil, fmt.Errorf("market schedule: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var o ScheduleOverride
	var openArg, closeArg any
	if !in.Closed {
		openArg, closeArg = in.Open, in.Close
	}
	err = tx.QueryRow(ctx, `
		INSERT INTO market_schedule_overrides
		    (override_date, closed, open_utc, close_utc, reason, created_by)
		VALUES ($1, $2, $3::time, $4::time, $5, $6)
		RETURNING override_id, override_date, closed,
		          COALESCE(to_char(open_utc,'HH24:MI'),''), COALESCE(to_char(close_utc,'HH24:MI'),''),
		          reason, created_by, created_at, updated_at, updated_by`,
		d, in.Closed, openArg, closeArg, strings.TrimSpace(in.Reason), actor.UserID).
		Scan(&o.ID, &d, &o.Closed, &o.Open, &o.Close,
			&o.Reason, &o.CreatedBy, &o.CreatedAt, &o.UpdatedAt, &o.UpdatedBy)
	if err != nil {
		return nil, fmt.Errorf("market schedule: insert: %w", mapPGConflict(err))
	}
	o.Date = d.Format("2006-01-02")
	o.UpdatedBy = nil

	if _, _, err := Log(ctx, tx, AuditEntry{
		AdminUserID: actor.UserID,
		Action:      "market_schedule.override.create",
		TargetType:  "market_schedule_override",
		TargetID:    &o.ID,
		AfterState:  o,
		IPAddress:   actor.ClientIP,
	}); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("market schedule: commit: %w", err)
	}
	if _, err := s.Publish(ctx); err != nil {
		return &o, excerrors.Wrap("SERVICE_DEGRADED",
			"override committed but market:hours republish failed", err)
	}
	return &o, nil
}

// UpdateOverride replaces the mutable fields of one row (date is the
// identity and stays fixed; reason is mandatory on every change).
func (s *MarketScheduleService) UpdateOverride(ctx context.Context, actor AdminActor, id int64, in OverrideInput) (*ScheduleOverride, error) {
	if err := s.requireRole(ctx, actor, true); err != nil {
		return nil, err
	}
	in.Date = "" // date is immutable via update — re-key by delete+create
	if strings.TrimSpace(in.Reason) == "" || len(in.Reason) > 512 {
		return nil, excerrors.New("INVALID_REQUEST", "reason required (≤512 chars)")
	}
	if in.Closed {
		if in.Open != "" || in.Close != "" {
			return nil, excerrors.New("INVALID_REQUEST",
				"closed=true overrides must not carry open/close times")
		}
	} else {
		if !hhmmRe(in.Open) || !hhmmRe(in.Close) {
			return nil, excerrors.New("INVALID_REQUEST",
				"partial overrides require open and close in HH:MM UTC")
		}
		if in.Open >= in.Close {
			return nil, excerrors.New("INVALID_REQUEST",
				"partial override requires open < close within the UTC day")
		}
	}

	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return nil, fmt.Errorf("market schedule: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var before ScheduleOverride
	var bd time.Time
	var bOpen, bClose string
	var bub *int64
	err = tx.QueryRow(ctx, `
		SELECT override_id, override_date, closed,
		       COALESCE(to_char(open_utc,'HH24:MI'),''), COALESCE(to_char(close_utc,'HH24:MI'),''),
		       reason, created_by, created_at, updated_at, updated_by
		  FROM market_schedule_overrides WHERE override_id = $1 FOR UPDATE`, id).
		Scan(&before.ID, &bd, &before.Closed, &bOpen, &bClose,
			&before.Reason, &before.CreatedBy, &before.CreatedAt, &before.UpdatedAt, &bub)
	if err != nil {
		if err == pgx.ErrNoRows {
			return nil, excerrors.New("NOT_FOUND", "schedule override not found")
		}
		return nil, fmt.Errorf("market schedule: load %d: %w", id, err)
	}
	before.Date, before.Open, before.Close, before.UpdatedBy =
		bd.Format("2006-01-02"), bOpen, bClose, bub

	var openArg, closeArg any
	if !in.Closed {
		openArg, closeArg = in.Open, in.Close
	}
	var o ScheduleOverride
	var nd time.Time
	err = tx.QueryRow(ctx, `
		UPDATE market_schedule_overrides
		   SET closed = $2, open_utc = $3::time, close_utc = $4::time,
		       reason = $5, updated_by = $6, updated_at = now()
		 WHERE override_id = $1
		RETURNING override_id, override_date, closed,
		          COALESCE(to_char(open_utc,'HH24:MI'),''), COALESCE(to_char(close_utc,'HH24:MI'),''),
		          reason, created_by, created_at, updated_at, updated_by`,
		id, in.Closed, openArg, closeArg, strings.TrimSpace(in.Reason), actor.UserID).
		Scan(&o.ID, &nd, &o.Closed, &o.Open, &o.Close,
			&o.Reason, &o.CreatedBy, &o.CreatedAt, &o.UpdatedAt, &o.UpdatedBy)
	if err != nil {
		return nil, fmt.Errorf("market schedule: update: %w", mapPGConflict(err))
	}
	o.Date = nd.Format("2006-01-02")

	if _, _, err := Log(ctx, tx, AuditEntry{
		AdminUserID: actor.UserID,
		Action:      "market_schedule.override.update",
		TargetType:  "market_schedule_override",
		TargetID:    &o.ID,
		BeforeState: before,
		AfterState:  o,
		IPAddress:   actor.ClientIP,
	}); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("market schedule: commit: %w", err)
	}
	if _, err := s.Publish(ctx); err != nil {
		return &o, excerrors.Wrap("SERVICE_DEGRADED",
			"override committed but market:hours republish failed", err)
	}
	return &o, nil
}

// DeleteOverride removes one row (the audit row preserves it) and
// republishes the projection.
func (s *MarketScheduleService) DeleteOverride(ctx context.Context, actor AdminActor, id int64) error {
	if err := s.requireRole(ctx, actor, true); err != nil {
		return err
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return fmt.Errorf("market schedule: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var before ScheduleOverride
	var bd time.Time
	var bOpen, bClose string
	var bub *int64
	err = tx.QueryRow(ctx, `
		DELETE FROM market_schedule_overrides WHERE override_id = $1
		RETURNING override_id, override_date, closed,
		          COALESCE(to_char(open_utc,'HH24:MI'),''), COALESCE(to_char(close_utc,'HH24:MI'),''),
		          reason, created_by, created_at, updated_at, updated_by`, id).
		Scan(&before.ID, &bd, &before.Closed, &bOpen, &bClose,
			&before.Reason, &before.CreatedBy, &before.CreatedAt, &before.UpdatedAt, &bub)
	if err != nil {
		if err == pgx.ErrNoRows {
			return excerrors.New("NOT_FOUND", "schedule override not found")
		}
		return fmt.Errorf("market schedule: delete: %w", err)
	}
	before.Date, before.Open, before.Close, before.UpdatedBy =
		bd.Format("2006-01-02"), bOpen, bClose, bub

	if _, _, err := Log(ctx, tx, AuditEntry{
		AdminUserID: actor.UserID,
		Action:      "market_schedule.override.delete",
		TargetType:  "market_schedule_override",
		TargetID:    &before.ID,
		BeforeState: before,
		IPAddress:   actor.ClientIP,
	}); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("market schedule: commit: %w", err)
	}
	if _, err := s.Publish(ctx); err != nil {
		return excerrors.Wrap("SERVICE_DEGRADED",
			"override deleted but market:hours republish failed", err)
	}
	return nil
}

// mapPGConflict renders unique violations as INVALID_REQUEST rather
// than leaking a raw 23505 (same discipline as the LP service).
func mapPGConflict(err error) error {
	if err == nil {
		return nil
	}
	if strings.Contains(err.Error(), "market_schedule_overrides_date_uq") {
		return excerrors.New("INVALID_REQUEST",
			"an override already exists for that date — update or delete it first")
	}
	return err
}
