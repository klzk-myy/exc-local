// Package admin implements the Phase-07 admin & monitoring domain
// services: the Liquidity Provider management module (Task 7.3.9, this
// file), the CEO daily command pack (Task 7.3.13, command_pack.go) and
// the quarterly board pack generator (Task 7.3.14, board_pack.go).
//
// RBAC note: the Phase-07 role store (Task 7.3.1/7.3.11 — rbac.go /
// scopes.go in this package, landing concurrently) owns the authoritative
// binding lookup. Every privileged method here gates through the
// AdminRoleResolver seam — identical in spirit to api.AdminRoleResolver
// and accounts.RoleResolver — and a nil resolver fails closed with
// UNAUTHORIZED_ROLE. Integration point: wire the seam to the binding
// store's lookup when 7.3.1 lands; scope intersection (desks/regions/
// currencies/env, §8.2a) is additive inside the resolver, not here.
package admin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"

	excerrors "exchange/pkg/errors"
)

// AdminRoleResolver resolves an admin user id to its spec §8.2 role name.
// Phase-05-style seam identical to api.AdminRoleResolver /
// accounts.RoleResolver; nil fails closed on every gated call.
type AdminRoleResolver func(ctx context.Context, adminUserID int64) (string, error)

// AdminActor is the authenticated administrator performing an action;
// ApproverID carries the second (four-eyes) approver on dual-controlled
// operations — mirroring accounts.AdminActor (FreezeService).
type AdminActor struct {
	UserID     int64
	ApproverID int64
	ClientIP   string
}

// Role-name literals follow spec §8.2 verbatim; they are NOT declared as
// package constants here because rbac.go (Task 7.3.1, concurrent) owns the
// canonical role vocabulary for this package.
var (
	// lpManageRoles may create/update LPs, flip FIX session gates and
	// mutate pricing config — the route registry pins RoleRiskManager,
	// and Super Admin holds "all permissions" per §8.2.
	lpManageRoles = map[string]bool{
		"Risk Manager": true,
		"Super Admin":  true,
	}
	// lpReadRoles may read LP entities and scorecards. Read-Only Auditor
	// is excluded: the Task 5.3.7 route rows for
	// /api/v1/admin/liquidity-providers/* pin RoleRiskManager.
	lpReadRoles = map[string]bool{
		"Risk Manager": true,
		"Super Admin":  true,
	}
	// packReadAllRoles see packs in any state.
	packReadAllRoles = map[string]bool{
		"Super Admin":  true,
		"Risk Manager": true,
	}
	// packAuditorRoles see RELEASED packs only (plus CEO_DAILY roll-ups —
	// those never enter a release flow). EXTERNAL_AUDITOR lands with
	// Phase-24 Task 24.3.18; the name resolves through the same seam.
	packAuditorRoles = map[string]bool{
		"Read-Only Auditor": true,
		"EXTERNAL_AUDITOR":  true,
	}
	// packReleaseRoles may initiate or approve a board-pack release.
	packReleaseRoles = map[string]bool{
		"Super Admin": true,
	}
)

// ---------------------------------------------------------------------------
// LP entity
// ---------------------------------------------------------------------------

// LPStatus is the lifecycle state per Task 7.3.9:
// ONBOARDING → ACTIVE → SUSPENDED (resume SUSPENDED→ACTIVE, re-onboard
// SUSPENDED→ONBOARDING). Spec §5.44 item 3's (ACTIVE|SUSPENDED) shorthand
// omits ONBOARDING — the task lifecycle governs.
type LPStatus string

const (
	LPStatusOnboarding LPStatus = "ONBOARDING"
	LPStatusActive     LPStatus = "ACTIVE"
	LPStatusSuspended  LPStatus = "SUSPENDED"
)

func (s LPStatus) valid() bool {
	switch s {
	case LPStatusOnboarding, LPStatusActive, LPStatusSuspended:
		return true
	}
	return false
}

// canTransitionLP guards the lifecycle state machine.
func canTransitionLP(from, to LPStatus) bool {
	switch from {
	case LPStatusOnboarding:
		return to == LPStatusActive || to == LPStatusSuspended
	case LPStatusActive:
		return to == LPStatusSuspended
	case LPStatusSuspended:
		return to == LPStatusActive || to == LPStatusOnboarding
	}
	return false
}

// LP connection types (Task item 1).
const (
	LPConnFIX  = "FIX"
	LPConnREST = "REST"
	LPConnWS   = "WS"
)

func validLPConnType(t string) bool {
	return t == LPConnFIX || t == LPConnREST || t == LPConnWS
}

// DefaultLPStalenessTimeoutMS is the spec §6.5 price staleness gate.
const DefaultLPStalenessTimeoutMS = 5000

// LiquidityProvider is one LP entity row plus its instrument configs.
type LiquidityProvider struct {
	LPID               int64                `json:"lp_id"`
	Name               string               `json:"name"`
	Status             LPStatus             `json:"status"`
	ConnectionType     string               `json:"connection_type"`
	SessionConfig      json.RawMessage      `json:"session_config"`
	Contact            json.RawMessage      `json:"contact"`
	SettlementTerms    json.RawMessage      `json:"settlement_terms"`
	FIXSessionEnabled  bool                 `json:"fix_session_enabled"`
	StalenessTimeoutMS int                  `json:"staleness_timeout_ms"`
	Scorecard          json.RawMessage      `json:"scorecard,omitempty"`
	Instruments        []LPInstrumentConfig `json:"instruments,omitempty"`
	CreatedBy          int64                `json:"created_by,omitempty"`
	CreatedAt          time.Time            `json:"created_at"`
	UpdatedAt          time.Time            `json:"updated_at"`
}

// LPInstrumentConfig is the per-LP per-instrument pricing feed config
// (Task items 2 + 6): quoting gate, spread markup/skew applied before
// market-data distribution, and per-instrument staleness timeout.
// Bps fields are decimal strings — NUMERIC columns, same convention as
// the fee-tier surface.
type LPInstrumentConfig struct {
	InstrumentID       int64  `json:"instrument_id"`
	Symbol             string `json:"symbol,omitempty"`
	Enabled            bool   `json:"enabled"`
	SpreadMarkupBidBps string `json:"spread_markup_bid_bps"`
	SpreadMarkupAskBps string `json:"spread_markup_ask_bps"`
	SkewBps            string `json:"skew_bps"`
	StalenessTimeoutMS int    `json:"staleness_timeout_ms"`
}

// LPScorecard is the per-LP performance snapshot (Task item 4): fill
// ratio, response latency, rejection rate, availability, spread quality —
// plus the §27-audit MM-obligation dimensions (two-sided presence, p99
// latency) so one shape serves both scorecard surfaces.
type LPScorecard struct {
	LPID                int64     `json:"lp_id"`
	Window              string    `json:"window"` // e.g. "1h"
	QuotesReceived      int64     `json:"quotes_received"`
	Fills               int64     `json:"fills"`
	Rejections          int64     `json:"rejections"`
	FillRatio           float64   `json:"fill_ratio"`
	RejectionRate       float64   `json:"rejection_rate"`
	AvgResponseTimeMS   float64   `json:"avg_response_time_ms"`
	P99LatencyMS        float64   `json:"p99_latency_ms"`
	AvailabilityPct     float64   `json:"availability_pct"`
	TwoSidedPresencePct float64   `json:"two_sided_presence_pct"`
	SpreadQualityBps    float64   `json:"spread_quality_bps"` // avg spread vs market mid
	ComputedAt          time.Time `json:"computed_at"`
	Source              string    `json:"source"`          // metrics source identity; "persisted" = cache
	Stale               bool      `json:"stale,omitempty"` // persisted snapshot, not a live read
}

// LPAlert is one persisted performance-alert row (lp_performance_alerts).
type LPAlert struct {
	ID        int64      `json:"id"`
	LPID      int64      `json:"lp_id"`
	Metric    string     `json:"metric"`
	Observed  float64    `json:"observed"`
	Threshold float64    `json:"threshold"`
	Window    string     `json:"window"`
	Status    string     `json:"status"` // OPEN | ACKED | RESOLVED
	EmittedAt time.Time  `json:"emitted_at"`
	AckedBy   *int64     `json:"acked_by,omitempty"`
	AckedAt   *time.Time `json:"acked_at,omitempty"`
}

// ---------------------------------------------------------------------------
// Seams
// ---------------------------------------------------------------------------

// MetricsSource feeds live LP metrics — polled (ClickHouse
// lp_performance_hourly, Phase-06/17 pipeline) or event-fed (execution-log
// stream). A nil source never fabricates data: Scorecard then serves the
// last persisted snapshot marked stale=true.
type MetricsSource interface {
	CollectLPMetrics(ctx context.Context, lpID int64, window time.Duration) (*LPScorecard, error)
}

// AlertSink dispatches LP performance alerts to ops.alerts (Risk Manager
// dashboard / PagerDuty bridge). nil → alerts persist to
// lp_performance_alerts but dispatch is skipped (reported in the result,
// never silently dropped).
type AlertSink interface {
	EmitLPAlert(ctx context.Context, a LPAlert) error
}

// LPThresholds are the Task item-5 alert thresholds over a rolling window.
type LPThresholds struct {
	MinFillRatio       float64       // alert when fill_ratio < this (0.80)
	MinAvailabilityPct float64       // alert when availability < this (95.0)
	Window             time.Duration // evaluation window (1h)
}

// DefaultLPThresholds returns the task-pinned thresholds: fill ratio
// < 80% or availability < 95% over a 1h window.
func DefaultLPThresholds() LPThresholds {
	return LPThresholds{MinFillRatio: 0.80, MinAvailabilityPct: 95.0, Window: time.Hour}
}

// ---------------------------------------------------------------------------
// Requests
// ---------------------------------------------------------------------------

// LPCreate is the create payload.
type LPCreate struct {
	Name               string               `json:"name"`
	ConnectionType     string               `json:"connection_type"`
	SessionConfig      json.RawMessage      `json:"session_config"`
	Contact            json.RawMessage      `json:"contact"`
	SettlementTerms    json.RawMessage      `json:"settlement_terms"`
	FIXSessionEnabled  bool                 `json:"fix_session_enabled"`
	StalenessTimeoutMS int                  `json:"staleness_timeout_ms"`
	Instruments        []LPInstrumentConfig `json:"instruments"`
}

// LPUpdate is the partial-update payload (collection PUT carries lp_id).
// Pointer fields distinguish "absent" from "set to zero value".
type LPUpdate struct {
	LPID               int64                `json:"lp_id"`
	Name               *string              `json:"name"`
	Status             *string              `json:"status"`
	ConnectionType     *string              `json:"connection_type"`
	SessionConfig      json.RawMessage      `json:"session_config"`
	Contact            json.RawMessage      `json:"contact"`
	SettlementTerms    json.RawMessage      `json:"settlement_terms"`
	FIXSessionEnabled  *bool                `json:"fix_session_enabled"`
	StalenessTimeoutMS *int                 `json:"staleness_timeout_ms"`
	Instruments        []LPInstrumentConfig `json:"instruments"` // upserted
	Reason             string               `json:"reason"`      // audit trail
}

func validateJSONB(raw json.RawMessage, field string) (json.RawMessage, error) {
	if len(raw) == 0 {
		return json.RawMessage(`{}`), nil
	}
	var probe any
	if err := json.Unmarshal(raw, &probe); err != nil {
		return nil, excerrors.New("INVALID_REQUEST", field+" must be a JSON object")
	}
	if _, ok := probe.(map[string]any); !ok {
		return nil, excerrors.New("INVALID_REQUEST", field+" must be a JSON object")
	}
	return raw, nil
}

// validBps parses a decimal-string bps value and bounds it to ±100%
// (10000 bps) — a sanity rail against garbage config.
func validBps(s, field string) (string, error) {
	if s == "" {
		return "0", nil
	}
	d, err := decimal.NewFromString(s)
	if err != nil {
		return "", excerrors.New("INVALID_REQUEST", field+" must be a decimal string")
	}
	if d.Abs().GreaterThan(decimal.NewFromInt(10000)) {
		return "", excerrors.New("INVALID_REQUEST", field+" exceeds the ±10000 bps bound")
	}
	return d.String(), nil
}

func validInstrumentConfig(c LPInstrumentConfig) error {
	if c.InstrumentID <= 0 {
		return excerrors.New("INVALID_REQUEST", "instrument_id must be a positive integer")
	}
	if c.StalenessTimeoutMS < 0 {
		return excerrors.New("INVALID_REQUEST", "staleness_timeout_ms must be >= 0")
	}
	if _, err := validBps(c.SpreadMarkupBidBps, "spread_markup_bid_bps"); err != nil {
		return err
	}
	if _, err := validBps(c.SpreadMarkupAskBps, "spread_markup_ask_bps"); err != nil {
		return err
	}
	if _, err := validBps(c.SkewBps, "skew_bps"); err != nil {
		return err
	}
	return nil
}

// ---------------------------------------------------------------------------
// Store seam
// ---------------------------------------------------------------------------

// lpStore is the persistence seam — pgLPStore is production; unit tests
// substitute a fake. Mutations run inside a transaction that also writes
// the admin_audit_log row (house convention: record + audit commit
// atomically, spec §5.9).
type lpStore interface {
	insertLP(ctx context.Context, lp LiquidityProvider, actor AdminActor) (*LiquidityProvider, error)
	getLP(ctx context.Context, lpID int64) (*LiquidityProvider, error)
	listLPs(ctx context.Context, status string) ([]LiquidityProvider, error)
	updateLP(ctx context.Context, upd LPUpdate, actor AdminActor) (*LiquidityProvider, error)
	saveScorecard(ctx context.Context, lpID int64, sc LPScorecard) error
	insertAlert(ctx context.Context, a LPAlert) (*LPAlert, bool, error)
	listAlerts(ctx context.Context, lpID int64, openOnly bool) ([]LPAlert, error)
	countActiveLPs(ctx context.Context) (int, error)
}

type pgLPStore struct{ pool *pgxpool.Pool }

const lpColumns = `lp_id, name, status, connection_type, session_config, contact,
                   settlement_terms, fix_session_enabled, staleness_timeout_ms,
                   scorecard, created_by, created_at, updated_at`

func scanLP(row pgx.Row) (*LiquidityProvider, error) {
	var lp LiquidityProvider
	var createdBy *int64
	err := row.Scan(&lp.LPID, &lp.Name, &lp.Status, &lp.ConnectionType,
		&lp.SessionConfig, &lp.Contact, &lp.SettlementTerms,
		&lp.FIXSessionEnabled, &lp.StalenessTimeoutMS, &lp.Scorecard,
		&createdBy, &lp.CreatedAt, &lp.UpdatedAt)
	if err != nil {
		return nil, err
	}
	if createdBy != nil {
		lp.CreatedBy = *createdBy
	}
	return &lp, nil
}

func (s *pgLPStore) loadInstruments(ctx context.Context, q pgx.Tx, lpID int64) ([]LPInstrumentConfig, error) {
	rows, err := q.Query(ctx, `
		SELECT c.instrument_id, i.symbol, c.enabled,
		       c.spread_markup_bid_bps::text, c.spread_markup_ask_bps::text,
		       c.skew_bps::text, c.staleness_timeout_ms
		  FROM lp_instrument_configs c
		  LEFT JOIN instruments i ON i.id = c.instrument_id
		 WHERE c.lp_id = $1 ORDER BY c.instrument_id`, lpID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []LPInstrumentConfig{}
	for rows.Next() {
		var c LPInstrumentConfig
		var sym *string
		if err := rows.Scan(&c.InstrumentID, &sym, &c.Enabled,
			&c.SpreadMarkupBidBps, &c.SpreadMarkupAskBps,
			&c.SkewBps, &c.StalenessTimeoutMS); err != nil {
			return nil, err
		}
		if sym != nil {
			c.Symbol = *sym
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// lpInstruments is the non-transactional read variant.
func (s *pgLPStore) lpInstruments(ctx context.Context, lpID int64) ([]LPInstrumentConfig, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT c.instrument_id, i.symbol, c.enabled,
		       c.spread_markup_bid_bps::text, c.spread_markup_ask_bps::text,
		       c.skew_bps::text, c.staleness_timeout_ms
		  FROM lp_instrument_configs c
		  LEFT JOIN instruments i ON i.id = c.instrument_id
		 WHERE c.lp_id = $1 ORDER BY c.instrument_id`, lpID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []LPInstrumentConfig{}
	for rows.Next() {
		var c LPInstrumentConfig
		var sym *string
		if err := rows.Scan(&c.InstrumentID, &sym, &c.Enabled,
			&c.SpreadMarkupBidBps, &c.SpreadMarkupAskBps,
			&c.SkewBps, &c.StalenessTimeoutMS); err != nil {
			return nil, err
		}
		if sym != nil {
			c.Symbol = *sym
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// insertAdminAudit appends the admin_audit_log row + its audit_hash_chain
// link inside the caller's transaction via the shared Log helper
// (audit.go, Task 7.3.3) — record and audit commit atomically.
func insertAdminAudit(ctx context.Context, tx pgx.Tx, adminUserID int64,
	action, targetType string, targetID int64, before, after any, clientIP string) error {
	_, _, err := Log(ctx, tx, AuditEntry{
		AdminUserID: adminUserID, Action: action, TargetType: targetType,
		TargetID: &targetID, BeforeState: before, AfterState: after,
		IPAddress: clientIP,
	})
	return err
}

func (s *pgLPStore) insertLP(ctx context.Context, lp LiquidityProvider, actor AdminActor) (*LiquidityProvider, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "begin tx", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var created *LiquidityProvider
	row := tx.QueryRow(ctx, `
		INSERT INTO liquidity_providers
		    (name, status, connection_type, session_config, contact,
		     settlement_terms, fix_session_enabled, staleness_timeout_ms, created_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		RETURNING `+lpColumns,
		lp.Name, string(lp.Status), lp.ConnectionType, lp.SessionConfig,
		lp.Contact, lp.SettlementTerms, lp.FIXSessionEnabled,
		lp.StalenessTimeoutMS, lp.CreatedBy)
	if created, err = scanLP(row); err != nil {
		if isUniqueViolation(err) {
			return nil, excerrors.New("INVALID_REQUEST",
				fmt.Sprintf("liquidity provider name %q already exists", lp.Name))
		}
		return nil, excerrors.Wrap("INTERNAL_ERROR", "insert lp", err)
	}

	for _, c := range lp.Instruments {
		if err := upsertLPInstrument(ctx, tx, created.LPID, c); err != nil {
			return nil, err
		}
	}
	created.Instruments, err = s.loadInstruments(ctx, tx, created.LPID)
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "load instruments", err)
	}

	after, _ := json.Marshal(created)
	if err := insertAdminAudit(ctx, tx, actor.UserID, "liquidity_provider.create",
		"liquidity_provider", created.LPID, nil, json.RawMessage(after), actor.ClientIP); err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "admin audit insert", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "commit tx", err)
	}
	return created, nil
}

func (s *pgLPStore) getLP(ctx context.Context, lpID int64) (*LiquidityProvider, error) {
	lp, err := scanLP(s.pool.QueryRow(ctx,
		`SELECT `+lpColumns+` FROM liquidity_providers WHERE lp_id = $1`, lpID))
	if err == pgx.ErrNoRows {
		return nil, excerrors.New("NOT_FOUND", fmt.Sprintf("liquidity provider %d not found", lpID))
	}
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "read lp", err)
	}
	lp.Instruments, err = s.lpInstruments(ctx, lpID)
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "read lp instruments", err)
	}
	return lp, nil
}

func (s *pgLPStore) listLPs(ctx context.Context, status string) ([]LiquidityProvider, error) {
	q := `SELECT ` + lpColumns + ` FROM liquidity_providers`
	args := []any{}
	if status != "" {
		q += ` WHERE status = $1`
		args = append(args, status)
	}
	q += ` ORDER BY lp_id`
	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "list lps", err)
	}
	defer rows.Close()
	out := []LiquidityProvider{}
	for rows.Next() {
		var lp LiquidityProvider
		var createdBy *int64
		if err := rows.Scan(&lp.LPID, &lp.Name, &lp.Status, &lp.ConnectionType,
			&lp.SessionConfig, &lp.Contact, &lp.SettlementTerms,
			&lp.FIXSessionEnabled, &lp.StalenessTimeoutMS, &lp.Scorecard,
			&createdBy, &lp.CreatedAt, &lp.UpdatedAt); err != nil {
			return nil, excerrors.Wrap("INTERNAL_ERROR", "scan lp", err)
		}
		if createdBy != nil {
			lp.CreatedBy = *createdBy
		}
		out = append(out, lp)
	}
	if err := rows.Err(); err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "list lps cursor", err)
	}
	for i := range out {
		out[i].Instruments, err = s.lpInstruments(ctx, out[i].LPID)
		if err != nil {
			return nil, excerrors.Wrap("INTERNAL_ERROR", "read lp instruments", err)
		}
	}
	return out, nil
}

func (s *pgLPStore) updateLP(ctx context.Context, upd LPUpdate, actor AdminActor) (*LiquidityProvider, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "begin tx", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Lock the row and snapshot the before-state for the audit trail.
	cur, err := scanLP(tx.QueryRow(ctx,
		`SELECT `+lpColumns+` FROM liquidity_providers WHERE lp_id = $1 FOR UPDATE`, upd.LPID))
	if err == pgx.ErrNoRows {
		return nil, excerrors.New("NOT_FOUND",
			fmt.Sprintf("liquidity provider %d not found", upd.LPID))
	}
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "lock lp", err)
	}
	before, _ := json.Marshal(cur)

	// Guarded status transition — the state machine check runs against
	// the locked row so a racing update cannot interleave.
	if upd.Status != nil {
		to := LPStatus(*upd.Status)
		if !to.valid() {
			return nil, excerrors.New("INVALID_REQUEST",
				fmt.Sprintf("invalid status %q", *upd.Status))
		}
		if to != cur.Status && !canTransitionLP(cur.Status, to) {
			return nil, excerrors.New("INVALID_LIFECYCLE_TRANSITION",
				fmt.Sprintf("cannot transition LP %d from %s to %s", cur.LPID, cur.Status, to))
		}
	}

	sets := []string{}
	args := []any{}
	add := func(clause string, v any) {
		args = append(args, v)
		sets = append(sets, fmt.Sprintf(clause, len(args)))
	}
	if upd.Name != nil {
		add("name = $%d", *upd.Name)
	}
	if upd.Status != nil {
		add("status = $%d", *upd.Status)
	}
	if upd.ConnectionType != nil {
		add("connection_type = $%d", *upd.ConnectionType)
	}
	if len(upd.SessionConfig) > 0 {
		add("session_config = $%d", upd.SessionConfig)
	}
	if len(upd.Contact) > 0 {
		add("contact = $%d", upd.Contact)
	}
	if len(upd.SettlementTerms) > 0 {
		add("settlement_terms = $%d", upd.SettlementTerms)
	}
	if upd.FIXSessionEnabled != nil {
		add("fix_session_enabled = $%d", *upd.FIXSessionEnabled)
	}
	if upd.StalenessTimeoutMS != nil {
		add("staleness_timeout_ms = $%d", *upd.StalenessTimeoutMS)
	}
	if len(sets) > 0 {
		args = append(args, upd.LPID)
		q := fmt.Sprintf(`UPDATE liquidity_providers SET %s, updated_at = now() WHERE lp_id = $%d`,
			strings.Join(sets, ", "), len(args))
		if _, err := tx.Exec(ctx, q, args...); err != nil {
			if isUniqueViolation(err) {
				return nil, excerrors.New("INVALID_REQUEST",
					fmt.Sprintf("liquidity provider name %q already exists", *upd.Name))
			}
			return nil, excerrors.Wrap("INTERNAL_ERROR", "update lp", err)
		}
	}
	for _, c := range upd.Instruments {
		if err := upsertLPInstrument(ctx, tx, upd.LPID, c); err != nil {
			return nil, err
		}
	}

	row := tx.QueryRow(ctx,
		`SELECT `+lpColumns+` FROM liquidity_providers WHERE lp_id = $1`, upd.LPID)
	out, err := scanLP(row)
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "re-read lp", err)
	}
	out.Instruments, err = s.loadInstruments(ctx, tx, upd.LPID)
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "load instruments", err)
	}

	after, _ := json.Marshal(out)
	afterWrapped := json.RawMessage(after)
	if upd.Reason != "" {
		wrapped, _ := json.Marshal(map[string]any{
			"lp": json.RawMessage(after), "reason": upd.Reason,
		})
		afterWrapped = wrapped
	}
	if err := insertAdminAudit(ctx, tx, actor.UserID, "liquidity_provider.update",
		"liquidity_provider", upd.LPID, json.RawMessage(before),
		afterWrapped, actor.ClientIP); err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "admin audit insert", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "commit tx", err)
	}
	return out, nil
}

func upsertLPInstrument(ctx context.Context, tx pgx.Tx, lpID int64, c LPInstrumentConfig) error {
	staleness := c.StalenessTimeoutMS
	if staleness == 0 {
		staleness = DefaultLPStalenessTimeoutMS
	}
	bid, _ := validBps(c.SpreadMarkupBidBps, "spread_markup_bid_bps")
	ask, _ := validBps(c.SpreadMarkupAskBps, "spread_markup_ask_bps")
	skew, _ := validBps(c.SkewBps, "skew_bps")
	_, err := tx.Exec(ctx, `
		INSERT INTO lp_instrument_configs
		    (lp_id, instrument_id, enabled, spread_markup_bid_bps,
		     spread_markup_ask_bps, skew_bps, staleness_timeout_ms)
		VALUES ($1, $2, $3, $4::numeric, $5::numeric, $6::numeric, $7)
		ON CONFLICT (lp_id, instrument_id) DO UPDATE SET
		    enabled = EXCLUDED.enabled,
		    spread_markup_bid_bps = EXCLUDED.spread_markup_bid_bps,
		    spread_markup_ask_bps = EXCLUDED.spread_markup_ask_bps,
		    skew_bps = EXCLUDED.skew_bps,
		    staleness_timeout_ms = EXCLUDED.staleness_timeout_ms,
		    updated_at = now()`,
		lpID, c.InstrumentID, c.Enabled, bid, ask, skew, staleness)
	if err != nil {
		var pgErr interface{ SQLState() string }
		if errors.As(err, &pgErr) && pgErr.SQLState() == "23503" {
			return excerrors.New("INVALID_REQUEST",
				fmt.Sprintf("instrument %d does not exist", c.InstrumentID))
		}
		return excerrors.Wrap("INTERNAL_ERROR", "upsert lp instrument", err)
	}
	return nil
}

func (s *pgLPStore) saveScorecard(ctx context.Context, lpID int64, sc LPScorecard) error {
	body, _ := json.Marshal(sc)
	tag, err := s.pool.Exec(ctx,
		`UPDATE liquidity_providers SET scorecard = $2, updated_at = now() WHERE lp_id = $1`,
		lpID, body)
	if err != nil {
		return excerrors.Wrap("INTERNAL_ERROR", "persist scorecard", err)
	}
	if tag.RowsAffected() == 0 {
		return excerrors.New("NOT_FOUND", fmt.Sprintf("liquidity provider %d not found", lpID))
	}
	return nil
}

// insertAlert persists the alert unless an OPEN alert for the same
// (lp_id, metric) already exists — dedup so a sustained breach is one
// incident, not a flood. LPID==0 is stored as NULL: the venue-level
// all_lps_down alert has no owning LP.
func (s *pgLPStore) insertAlert(ctx context.Context, a LPAlert) (*LPAlert, bool, error) {
	var id int64
	var emitted time.Time
	var lpArg any
	if a.LPID > 0 {
		lpArg = a.LPID
	}
	err := s.pool.QueryRow(ctx, `
		INSERT INTO lp_performance_alerts (lp_id, metric, observed, threshold, eval_window)
		SELECT $1::bigint, $2::varchar, $3::numeric, $4::numeric, $5::varchar
		WHERE NOT EXISTS (
		    SELECT 1 FROM lp_performance_alerts
		     WHERE metric = $2 AND status = 'OPEN'
		       AND (lp_id = $1 OR ($1 IS NULL AND lp_id IS NULL)))
		RETURNING id, emitted_at`,
		lpArg, a.Metric, a.Observed, a.Threshold, a.Window).Scan(&id, &emitted)
	if err == pgx.ErrNoRows {
		return nil, false, nil // duplicate OPEN alert suppressed
	}
	if err != nil {
		return nil, false, excerrors.Wrap("INTERNAL_ERROR", "insert alert", err)
	}
	a.ID, a.EmittedAt, a.Status = id, emitted, "OPEN"
	return &a, true, nil
}

func (s *pgLPStore) listAlerts(ctx context.Context, lpID int64, openOnly bool) ([]LPAlert, error) {
	q := `SELECT id, lp_id, metric, observed::float8, threshold::float8, eval_window,
	             status, emitted_at, acked_by, acked_at
	        FROM lp_performance_alerts WHERE lp_id = $1`
	if openOnly {
		q += ` AND status = 'OPEN'`
	}
	q += ` ORDER BY id DESC LIMIT 200`
	rows, err := s.pool.Query(ctx, q, lpID)
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "list alerts", err)
	}
	defer rows.Close()
	out := []LPAlert{}
	for rows.Next() {
		var a LPAlert
		var obs, thr float64
		if err := rows.Scan(&a.ID, &a.LPID, &a.Metric, &obs, &thr, &a.Window,
			&a.Status, &a.EmittedAt, &a.AckedBy, &a.AckedAt); err != nil {
			return nil, excerrors.Wrap("INTERNAL_ERROR", "scan alert", err)
		}
		a.Observed, a.Threshold = obs, thr
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *pgLPStore) countActiveLPs(ctx context.Context) (int, error) {
	var n int
	if err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM liquidity_providers WHERE status = 'ACTIVE'`).Scan(&n); err != nil {
		return 0, excerrors.Wrap("INTERNAL_ERROR", "count active lps", err)
	}
	return n, nil
}

// isUniqueViolation reports a 23505 without importing pgconn's type set
// at call sites.
func isUniqueViolation(err error) bool {
	var st interface{ SQLState() string }
	return errors.As(err, &st) && st.SQLState() == "23505"
}

// ---------------------------------------------------------------------------
// Service
// ---------------------------------------------------------------------------

// LPService implements Task 7.3.9 — LP entity CRUD, lifecycle, per-LP
// pricing config, scorecard computation and performance alerts.
type LPService struct {
	store      lpStore
	resolver   AdminRoleResolver
	metrics    MetricsSource
	alerts     AlertSink
	thresholds LPThresholds
	now        func() time.Time
}

// NewLPService wires the production service. resolver/metrics/alerts may
// be nil — each fails closed at its own boundary (role gate →
// UNAUTHORIZED_ROLE; metrics → persisted snapshot; alerts → persisted but
// undispatched).
func NewLPService(pool *pgxpool.Pool, resolver AdminRoleResolver, metrics MetricsSource, alerts AlertSink) *LPService {
	return newLPService(&pgLPStore{pool: pool}, resolver, metrics, alerts, DefaultLPThresholds())
}

// newLPService is the unit-test seam.
func newLPService(store lpStore, resolver AdminRoleResolver, metrics MetricsSource, alerts AlertSink, th LPThresholds) *LPService {
	return &LPService{
		store: store, resolver: resolver, metrics: metrics,
		alerts: alerts, thresholds: th, now: time.Now,
	}
}

// requireRole gates a privileged call: identity required, resolver wired,
// resolved role eligible.
func (s *LPService) requireRole(ctx context.Context, actor AdminActor, allowed map[string]bool, what string) error {
	if actor.UserID == 0 {
		return excerrors.New("UNAUTHORIZED", "admin identity required")
	}
	if s.resolver == nil {
		return excerrors.New("UNAUTHORIZED_ROLE",
			"role resolver not configured (Phase-07 RBAC seam)")
	}
	role, err := s.resolver(ctx, actor.UserID)
	if err != nil {
		return excerrors.Wrap("INTERNAL_ERROR", "role lookup", err)
	}
	if !allowed[role] {
		return excerrors.New("UNAUTHORIZED_ROLE",
			fmt.Sprintf("%s requires Risk Manager or Super Admin (got %q)", what, role))
	}
	return nil
}

// Create registers a new LP in ONBOARDING. Instruments may be attached at
// creation; each is validated then upserted.
func (s *LPService) Create(ctx context.Context, actor AdminActor, req LPCreate) (*LiquidityProvider, error) {
	if err := s.requireRole(ctx, actor, lpManageRoles, "create liquidity provider"); err != nil {
		return nil, err
	}
	if strings.TrimSpace(req.Name) == "" {
		return nil, excerrors.New("INVALID_REQUEST", "name is required")
	}
	if !validLPConnType(req.ConnectionType) {
		return nil, excerrors.New("INVALID_REQUEST",
			"connection_type must be FIX, REST or WS")
	}
	var err error
	if req.SessionConfig, err = validateJSONB(req.SessionConfig, "session_config"); err != nil {
		return nil, err
	}
	if req.Contact, err = validateJSONB(req.Contact, "contact"); err != nil {
		return nil, err
	}
	if req.SettlementTerms, err = validateJSONB(req.SettlementTerms, "settlement_terms"); err != nil {
		return nil, err
	}
	staleness := req.StalenessTimeoutMS
	if staleness == 0 {
		staleness = DefaultLPStalenessTimeoutMS
	}
	if staleness < 0 {
		return nil, excerrors.New("INVALID_REQUEST", "staleness_timeout_ms must be > 0")
	}
	for _, c := range req.Instruments {
		if err := validInstrumentConfig(c); err != nil {
			return nil, err
		}
	}
	lp := LiquidityProvider{
		Name:               strings.TrimSpace(req.Name),
		Status:             LPStatusOnboarding, // lifecycle entry state
		ConnectionType:     req.ConnectionType,
		SessionConfig:      req.SessionConfig,
		Contact:            req.Contact,
		SettlementTerms:    req.SettlementTerms,
		FIXSessionEnabled:  req.FIXSessionEnabled,
		StalenessTimeoutMS: staleness,
		Instruments:        req.Instruments,
		CreatedBy:          actor.UserID,
	}
	return s.store.insertLP(ctx, lp, actor)
}

// Get returns one LP with its instrument configs.
func (s *LPService) Get(ctx context.Context, actor AdminActor, lpID int64) (*LiquidityProvider, error) {
	if err := s.requireRole(ctx, actor, lpReadRoles, "read liquidity provider"); err != nil {
		return nil, err
	}
	if lpID <= 0 {
		return nil, excerrors.New("INVALID_REQUEST", "lp_id must be a positive integer")
	}
	return s.store.getLP(ctx, lpID)
}

// List returns all LPs (optionally filtered by status).
func (s *LPService) List(ctx context.Context, actor AdminActor, status string) ([]LiquidityProvider, error) {
	if err := s.requireRole(ctx, actor, lpReadRoles, "list liquidity providers"); err != nil {
		return nil, err
	}
	if status != "" && !LPStatus(status).valid() {
		return nil, excerrors.New("INVALID_REQUEST", fmt.Sprintf("invalid status filter %q", status))
	}
	return s.store.listLPs(ctx, status)
}

// Update applies a partial update — scalar fields, guarded lifecycle
// transition, instrument-config upserts — in one audited transaction.
func (s *LPService) Update(ctx context.Context, actor AdminActor, upd LPUpdate) (*LiquidityProvider, error) {
	if err := s.requireRole(ctx, actor, lpManageRoles, "update liquidity provider"); err != nil {
		return nil, err
	}
	if upd.LPID <= 0 {
		return nil, excerrors.New("INVALID_REQUEST", "lp_id is required")
	}
	if upd.Name != nil && strings.TrimSpace(*upd.Name) == "" {
		return nil, excerrors.New("INVALID_REQUEST", "name must not be empty")
	}
	if upd.ConnectionType != nil && !validLPConnType(*upd.ConnectionType) {
		return nil, excerrors.New("INVALID_REQUEST",
			"connection_type must be FIX, REST or WS")
	}
	// Empty JSONB means "not provided" on a partial update — validate only
	// when present so the no-fields check below stays meaningful and the
	// store never overwrites a column with '{}' by accident.
	var err error
	if len(upd.SessionConfig) > 0 {
		if upd.SessionConfig, err = validateJSONB(upd.SessionConfig, "session_config"); err != nil {
			return nil, err
		}
	}
	if len(upd.Contact) > 0 {
		if upd.Contact, err = validateJSONB(upd.Contact, "contact"); err != nil {
			return nil, err
		}
	}
	if len(upd.SettlementTerms) > 0 {
		if upd.SettlementTerms, err = validateJSONB(upd.SettlementTerms, "settlement_terms"); err != nil {
			return nil, err
		}
	}
	if upd.StalenessTimeoutMS != nil && *upd.StalenessTimeoutMS <= 0 {
		return nil, excerrors.New("INVALID_REQUEST", "staleness_timeout_ms must be > 0")
	}
	for _, c := range upd.Instruments {
		if err := validInstrumentConfig(c); err != nil {
			return nil, err
		}
	}
	if upd.Name == nil && upd.Status == nil && upd.ConnectionType == nil &&
		len(upd.SessionConfig) == 0 && len(upd.Contact) == 0 &&
		len(upd.SettlementTerms) == 0 && upd.FIXSessionEnabled == nil &&
		upd.StalenessTimeoutMS == nil && len(upd.Instruments) == 0 {
		return nil, excerrors.New("INVALID_REQUEST", "update carries no fields")
	}
	return s.store.updateLP(ctx, upd, actor)
}

// Scorecard returns the live scorecard when a MetricsSource is wired —
// evaluating thresholds and persisting the snapshot — otherwise the last
// persisted snapshot marked stale (never fabricated).
func (s *LPService) Scorecard(ctx context.Context, actor AdminActor, lpID int64, window time.Duration) (*LPScorecard, []LPAlert, error) {
	if err := s.requireRole(ctx, actor, lpReadRoles, "read LP scorecard"); err != nil {
		return nil, nil, err
	}
	if lpID <= 0 {
		return nil, nil, excerrors.New("INVALID_REQUEST", "lp_id must be a positive integer")
	}
	if window <= 0 {
		window = s.thresholds.Window
	}

	if s.metrics == nil {
		// No live feed wired — serve the persisted snapshot, flagged.
		lp, err := s.store.getLP(ctx, lpID)
		if err != nil {
			return nil, nil, err
		}
		if len(lp.Scorecard) == 0 || string(lp.Scorecard) == "{}" {
			return nil, nil, excerrors.New("NOT_FOUND",
				"no scorecard data: metrics source not wired and no persisted snapshot")
		}
		var sc LPScorecard
		if err := json.Unmarshal(lp.Scorecard, &sc); err != nil {
			return nil, nil, excerrors.Wrap("INTERNAL_ERROR", "decode persisted scorecard", err)
		}
		sc.Stale = true
		if sc.Source == "" {
			sc.Source = "persisted"
		}
		return &sc, nil, nil
	}

	sc, err := s.metrics.CollectLPMetrics(ctx, lpID, window)
	if err != nil {
		// Live read failed — fall back to the persisted snapshot rather
		// than fabricating numbers.
		lp, gerr := s.store.getLP(ctx, lpID)
		if gerr != nil {
			return nil, nil, gerr
		}
		if len(lp.Scorecard) > 0 && string(lp.Scorecard) != "{}" {
			var cached LPScorecard
			if jerr := json.Unmarshal(lp.Scorecard, &cached); jerr == nil {
				cached.Stale = true
				if cached.Source == "" {
					cached.Source = "persisted"
				}
				return &cached, nil, nil
			}
		}
		return nil, nil, excerrors.Wrap("SERVICE_DEGRADED",
			"LP metrics source unavailable and no persisted snapshot", err)
	}
	sc.LPID = lpID
	if sc.Window == "" {
		sc.Window = window.String()
	}
	if sc.ComputedAt.IsZero() {
		sc.ComputedAt = s.now().UTC()
	}

	// Threshold evaluation → persisted (deduped) alert rows + dispatch.
	fired := s.evaluate(ctx, sc)

	// Persist the snapshot (best-effort: scorecard read must not fail on
	// a snapshot write hiccup, but log via the alert-free path — callers
	// see the fresh metrics regardless).
	if err := s.store.saveScorecard(ctx, lpID, *sc); err != nil {
		var e *excerrors.Error
		if errors.As(err, &e) && e.Code == "NOT_FOUND" {
			return nil, nil, e
		}
	}
	return sc, fired, nil
}

// evaluate checks the scorecard against thresholds, persists new alerts
// and dispatches them through the AlertSink seam.
func (s *LPService) evaluate(ctx context.Context, sc *LPScorecard) []LPAlert {
	type check struct {
		metric    string
		observed  float64
		threshold float64
		breach    bool
	}
	checks := []check{
		{"fill_ratio", sc.FillRatio, s.thresholds.MinFillRatio,
			sc.FillRatio < s.thresholds.MinFillRatio},
		{"availability_pct", sc.AvailabilityPct, s.thresholds.MinAvailabilityPct,
			sc.AvailabilityPct < s.thresholds.MinAvailabilityPct},
	}
	fired := []LPAlert{}
	for _, c := range checks {
		if !c.breach {
			continue
		}
		a := LPAlert{
			LPID: sc.LPID, Metric: c.metric, Observed: c.observed,
			Threshold: c.threshold, Window: sc.Window,
		}
		stored, inserted, err := s.store.insertAlert(ctx, a)
		if err != nil || !inserted {
			continue
		}
		if s.alerts != nil {
			if err := s.alerts.EmitLPAlert(ctx, *stored); err == nil {
				fired = append(fired, *stored)
				continue
			}
		}
		// Persisted but undispatched (nil sink or emit failure) — still
		// surface it in the result so callers see the breach.
		fired = append(fired, *stored)
	}
	return fired
}

// Alerts returns the persisted alert trail for an LP.
func (s *LPService) Alerts(ctx context.Context, actor AdminActor, lpID int64, openOnly bool) ([]LPAlert, error) {
	if err := s.requireRole(ctx, actor, lpReadRoles, "read LP alerts"); err != nil {
		return nil, err
	}
	if lpID <= 0 {
		return nil, excerrors.New("INVALID_REQUEST", "lp_id must be a positive integer")
	}
	return s.store.listAlerts(ctx, lpID, openOnly)
}

// Coverage returns how many LPs are ACTIVE — when zero, the venue has no
// external pricing and Task 7.3.9's SDD edge case applies: degrade to
// MarketDataOnly. The mode transition itself is owned by the Phase-02
// ModeManager; EvaluateCoverage persists/dispatches an `all_lps_down`
// venue alert (lp_id NULL) so ops sees the outage even before the mode
// manager reacts.
func (s *LPService) Coverage(ctx context.Context) (active int, allDown bool, err error) {
	n, err := s.store.countActiveLPs(ctx)
	if err != nil {
		return 0, false, err
	}
	return n, n == 0, nil
}

// EvaluateCoverage is the ops-loop entry point: when no LP is ACTIVE it
// raises the deduped venue-level `all_lps_down` alert and dispatches it
// through the AlertSink. Returns whether the venue currently has LP
// coverage; the MarketDataOnly mode switch stays with Phase-02.
func (s *LPService) EvaluateCoverage(ctx context.Context) (bool, error) {
	n, err := s.store.countActiveLPs(ctx)
	if err != nil {
		return false, err
	}
	if n > 0 {
		return true, nil
	}
	stored, inserted, err := s.store.insertAlert(ctx, LPAlert{
		Metric: "all_lps_down", Observed: 0, Threshold: 1,
		Window: s.thresholds.Window.String(),
	})
	if err != nil {
		return false, err
	}
	if inserted && s.alerts != nil {
		_ = s.alerts.EmitLPAlert(ctx, *stored) // persisted regardless; dispatch best-effort
	}
	return false, nil
}
