// Phase-15 Task 15.3.8 — Instrument Maintenance Workflow (spec §7.1
// states/ladder, §7.2 role matrix, §24 #234; migration 219
// instrument_change_requests + instrument_change_log).
//
// This is the governance layer on top of the landed §7.1 lifecycle engine
// (instrument_lifecycle.go — Task 15.3.1/15.3.2). It owns:
//
//  1. Instrument creation maker-checker — the spec's THREE-stage chain:
//     Risk Manager proposes → Compliance Officer reviews → Super Admin
//     approves → the DRAFT row lands via InstrumentService.CreateInTx
//     (the two-eyes OpInstrumentCreate dual-control path remains the §7.2
//     minimum; this is the fuller §15.3.8 item-1 chain — both converge
//     on the same insert + audit).
//  2. Parameter changes — maker-checker where every change takes effect
//     at the NEXT SESSION START (the Task-15.3.7 session boundary —
//     default resolver: next 22:00 UTC daily cut, skipping the weekly
//     close; injectable). Columnar parameters write real instruments
//     columns; margin_rate / trading_hours (no dedicated column) land in
//     instruments.param_overrides (migration 219 — additive deviation).
//  3. Emergency parameter changes — Super Admin only, immediate effect,
//     P1 alert through the Alerter seam + emergency=true audit rows.
//  4. Delisting governance — requests ride the EXISTING four-eyes
//     pipeline (OpInstrumentDelist → InstrumentService.TransitionTx,
//     wired by api.RegisterInstrumentExecutors). This file adds the
//     change_request + change_log trail and SyncDelistRequests, which
//     reconciles open DELIST requests against the dual-control store and
//     emits the security_status event once the executor lands the
//     transition. The §7.5 ladder itself (RESTRICTED 24h notice →
//     DELISTED → 30-day close-only, reduce_only gate) stays owned by the
//     lifecycle engine — RestrictedGrace/DelistedGrace.
//
// instrument_change_log is append-only (migration 219 trigger) — the
// immutable who/what/when/why trail the task mandates.
//
// Events: parameter/creates emit SECURITY_STATUS on the NATS seam
// (subject marketdata.security_status) + INSTRUMENT_PARAMETERS on the
// public venue.instrument_status WS channel. FIX SecurityStatus (35=f)
// republication is the Phase-18 Task-18.3.x binding of the same event —
// deferred by spec §7.1, not implemented here.
package admin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	excerrors "exchange/pkg/errors"
)

// ---------------------------------------------------------------------------
// Vocabulary
// ---------------------------------------------------------------------------

const (
	ChangeCreate = "CREATE"
	ChangeParam  = "PARAM"
	ChangeDelist = "DELIST"

	ChgPendingReview   = "PENDING_REVIEW"   // CREATE: awaiting Compliance review
	ChgPendingApproval = "PENDING_APPROVAL" // awaiting final approver
	ChgScheduled       = "SCHEDULED"        // approved; awaiting effective_at
	ChgApplied         = "APPLIED"
	ChgRejected        = "REJECTED"
	ChgCancelled       = "CANCELLED"
)

// SecurityStatusSubject is the NATS JetStream subject for instrument
// security-status events (the FIX 35=f feed binds here in Phase-18).
const SecurityStatusSubject = "marketdata.security_status"

// paramColumns are instrument parameters backed by a real column —
// writes go through a plain UPDATE (validated).
var paramColumns = map[string]bool{
	"tick_size": true, "lot_size": true,
	"min_order_qty": true, "max_order_qty": true,
	"min_notional": true, "min_price": true, "max_price": true,
	"price_band_pct_up": true, "price_band_pct_down": true,
	"max_spread_pips": true, "max_open_orders": true,
	"max_algo_orders": true, "max_leverage": true,
	"settlement_cycle": true,
}

// paramOverrideKeys are maintenance parameters with no dedicated column —
// they land in instruments.param_overrides (migration 219 JSONB bag).
var paramOverrideKeys = map[string]bool{
	"margin_rate":   true, // per-instrument margin override (Phase-19 consumes)
	"trading_hours": true, // JSON session spec (Phase-15 Task 15.3.7 consumes)
}

// intParams validate as positive integers rather than fixed-point decimals.
var intParams = map[string]bool{
	"max_leverage": true, "settlement_cycle": true,
	"max_open_orders": true, "max_algo_orders": true,
}

// Role sets (spec §7.2 matrix — "Role X" admits X and Super Admin).
var (
	maintProposeRoles = map[string]bool{RoleRiskManager: true, RoleSuperAdmin: true}
	maintReviewRoles  = map[string]bool{RoleComplianceOfficer: true, RoleSuperAdmin: true}
	maintApproveRoles = map[string]bool{RoleSuperAdmin: true}
	maintParamRoles   = map[string]bool{RoleRiskManager: true, RoleSuperAdmin: true}
)

// pairSymbolRe is the canonical AAA/BBB shape (exactly one slash,
// uppercase — the spec's ISO-4217 convention).
var pairSymbolRe = regexp.MustCompile(`^[A-Z]{3}/[A-Z]{3}$`)

// iso4217 is the ISO 4217 alphabetic currency-code set (active codes +
// the fund/precious-metal codes the spec's fiat scope admits; crypto
// codes are deliberately absent — fiat-only venue).
var iso4217 = map[string]bool{
	"AED": true, "AFN": true, "ALL": true, "AMD": true, "ANG": true,
	"AOA": true, "ARS": true, "AUD": true, "AWG": true, "AZN": true,
	"BAM": true, "BBD": true, "BDT": true, "BGN": true, "BHD": true,
	"BIF": true, "BMD": true, "BND": true, "BOB": true, "BOV": true,
	"BRL": true, "BSD": true, "BTN": true, "BWP": true, "BYN": true,
	"BZD": true, "CAD": true, "CDF": true, "CHE": true, "CHF": true,
	"CHW": true, "CLF": true, "CLP": true, "CNY": true, "COP": true,
	"COU": true, "CRC": true, "CUC": true, "CUP": true, "CVE": true,
	"CZK": true, "DJF": true, "DKK": true, "DOP": true, "DZD": true,
	"EGP": true, "ERN": true, "ETB": true, "EUR": true, "FJD": true,
	"FKP": true, "GBP": true, "GEL": true, "GHS": true, "GIP": true,
	"GMD": true, "GNF": true, "GTQ": true, "GYD": true, "HKD": true,
	"HNL": true, "HRK": true, "HTG": true, "HUF": true, "IDR": true,
	"ILS": true, "INR": true, "IQD": true, "IRR": true, "ISK": true,
	"JMD": true, "JOD": true, "JPY": true, "KES": true, "KGS": true,
	"KHR": true, "KMF": true, "KPW": true, "KRW": true, "KWD": true,
	"KYD": true, "KZT": true, "LAK": true, "LBP": true, "LKR": true,
	"LRD": true, "LSL": true, "LYD": true, "MAD": true, "MDL": true,
	"MGA": true, "MKD": true, "MMK": true, "MNT": true, "MOP": true,
	"MRU": true, "MUR": true, "MVR": true, "MWK": true, "MXN": true,
	"MXV": true, "MYR": true, "MZN": true, "NAD": true, "NGN": true,
	"NIO": true, "NOK": true, "NPR": true, "NZD": true, "OMR": true,
	"PAB": true, "PEN": true, "PGK": true, "PHP": true, "PKR": true,
	"PLN": true, "PYG": true, "QAR": true, "RON": true, "RSD": true,
	"RUB": true, "RWF": true, "SAR": true, "SBD": true, "SCR": true,
	"SDG": true, "SEK": true, "SGD": true, "SHP": true, "SLE": true,
	"SLL": true, "SOS": true, "SRD": true, "SSP": true, "STN": true,
	"SVC": true, "SYP": true, "SZL": true, "THB": true, "TJS": true,
	"TMT": true, "TND": true, "TOP": true, "TRY": true, "TTD": true,
	"TWD": true, "TZS": true, "UAH": true, "UGX": true, "USD": true,
	"USN": true, "UYI": true, "UYU": true, "UYW": true, "UZS": true,
	"VED": true, "VES": true, "VND": true, "VUV": true, "WST": true,
	"XAF": true, "XAG": true, "XAU": true, "XBA": true, "XBB": true,
	"XBC": true, "XBD": true, "XCD": true, "XDR": true, "XOF": true,
	"XPD": true, "XPF": true, "XPT": true, "XSU": true, "XTS": true,
	"XUA": true, "XXX": true, "YER": true, "ZAR": true, "ZMW": true,
	"ZWL": true,
}

// ValidatePairSymbol enforces the AAA/BBB ISO-4217 pair convention —
// malformed symbols reject here, never reaching the instruments row.
func ValidatePairSymbol(symbol string) (base, quote string, err error) {
	s := strings.ToUpper(strings.TrimSpace(symbol))
	if !pairSymbolRe.MatchString(s) {
		return "", "", excerrors.New("INVALID_REQUEST",
			"symbol must be exactly AAA/BBB — two three-letter ISO 4217 currencies, one slash")
	}
	base, quote = s[:3], s[4:]
	if !iso4217[base] || !iso4217[quote] {
		return "", "", excerrors.New("INVALID_REQUEST",
			fmt.Sprintf("symbol %q uses a non-ISO-4217 currency code", s))
	}
	if base == quote {
		return "", "", excerrors.New("INVALID_REQUEST",
			"base and quote currencies must differ")
	}
	return base, quote, nil
}

// ---------------------------------------------------------------------------
// Seams
// ---------------------------------------------------------------------------

// SecurityStatusPublisher emits the security_status event on the NATS
// JetStream backbone (subject SecurityStatusSubject). nil → event is
// logged, not published (documented dev seam — the WS event still lands).
type SecurityStatusPublisher interface {
	Publish(ctx context.Context, subject string, payload []byte) error
}

// NextSessionStart resolves when an approved parameter change takes
// effect — the "next session start" of spec §7.4/Task 15.3.7. Injectable;
// DefaultNextSessionStart implements the FX 24/5 daily cut (22:00 UTC,
// the New York close — Saturday is skipped because the venue is closed).
type NextSessionStart func(now time.Time) time.Time

// DefaultNextSessionStart is the 22:00 UTC daily boundary; the Saturday
// boundary does not exist (weekly close Friday 22:00 → reopen Sunday
// 21:00 UTC) and is skipped.
func DefaultNextSessionStart(now time.Time) time.Time {
	now = now.UTC()
	t := time.Date(now.Year(), now.Month(), now.Day(), 22, 0, 0, 0, time.UTC)
	if !now.Before(t) {
		t = t.Add(24 * time.Hour)
	}
	for t.Weekday() == time.Saturday {
		t = t.Add(24 * time.Hour)
	}
	return t
}

// MaintenanceAlerter raises ops alerts — emergency changes fire P1
// (Task 15.3.8 item 6). nil → audit + structured log only.
type MaintenanceAlerter interface {
	Alert(ctx context.Context, severity, code, message string) error
}

// SecurityStatusEvent is the NATS payload (and the future FIX
// SecurityStatus binding shape — Phase-18 owns the FIX encoding).
type SecurityStatusEvent struct {
	Event       string `json:"event"` // "SECURITY_STATUS"
	Symbol      string `json:"symbol"`
	Status      string `json:"status,omitempty"` // lifecycle state when status-driven
	Field       string `json:"field,omitempty"`  // parameter name when param-driven
	OldValue    string `json:"old_value,omitempty"`
	NewValue    string `json:"new_value,omitempty"`
	EffectiveAt int64  `json:"effective_at_ms,omitempty"`
	Emergency   bool   `json:"emergency,omitempty"`
	ChangeID    int64  `json:"change_id"`
	Source      string `json:"source"` // "instrument-maintenance"
	TsMs        int64  `json:"ts_ms"`
}

// ---------------------------------------------------------------------------
// Service
// ---------------------------------------------------------------------------

// InstrumentMaintenanceService is the maker-checker workflow engine.
type InstrumentMaintenanceService struct {
	pool        *pgxpool.Pool
	roles       AdminRoleResolver
	instruments *InstrumentService      // CreateInTx reuse (single insert path)
	dual        *DualControlService     // delist submit (OpInstrumentDelist)
	ws          InstrumentWSPublisher   // venue.instrument_status broadcasts
	nats        SecurityStatusPublisher // security_status backbone event
	next        NextSessionStart
	alerter     MaintenanceAlerter
	now         func() time.Time
	logf        func(format string, args ...any)
}

// InstrumentMaintenanceDeps wires the service. Pool, Roles and
// Instruments are required; Dual may be nil (RequestDelist then fails
// closed with DUAL_CONTROL_REQUIRED on submit).
type InstrumentMaintenanceDeps struct {
	Pool        *pgxpool.Pool
	Roles       AdminRoleResolver
	Instruments *InstrumentService
	Dual        *DualControlService
	WS          InstrumentWSPublisher
	NATS        SecurityStatusPublisher
	NextSession NextSessionStart
	Alerter     MaintenanceAlerter
	Now         func() time.Time
	Logf        func(format string, args ...any)
}

// NewInstrumentMaintenanceService wires the service — missing mandatory
// deps fail closed.
func NewInstrumentMaintenanceService(d InstrumentMaintenanceDeps) (*InstrumentMaintenanceService, error) {
	if d.Pool == nil {
		return nil, fmt.Errorf("instrument maintenance: pgx pool is nil")
	}
	if d.Roles == nil {
		return nil, fmt.Errorf("instrument maintenance: role resolver is nil")
	}
	if d.Instruments == nil {
		return nil, fmt.Errorf("instrument maintenance: lifecycle service is nil")
	}
	s := &InstrumentMaintenanceService{
		pool: d.Pool, roles: d.Roles, instruments: d.Instruments,
		dual: d.Dual, ws: d.WS, nats: d.NATS, next: d.NextSession,
		alerter: d.Alerter, now: d.Now, logf: d.Logf,
	}
	if s.now == nil {
		s.now = time.Now
	}
	if s.next == nil {
		s.next = DefaultNextSessionStart
	}
	if s.logf == nil {
		s.logf = func(string, ...any) {}
	}
	return s, nil
}

func (s *InstrumentMaintenanceService) requireRole(ctx context.Context, userID int64,
	allowed map[string]bool, what string) error {
	role, err := s.roles(ctx, userID)
	if err != nil {
		return excerrors.Wrap("INTERNAL_ERROR", "role resolution", err)
	}
	if !allowed[role] {
		return excerrors.New("UNAUTHORIZED_ROLE",
			what+" requires a binding in the operation's role set")
	}
	return nil
}

// ---------------------------------------------------------------------------
// ChangeRequest — one instrument_change_requests row.
// ---------------------------------------------------------------------------

type ChangeRequest struct {
	ID           int64           `json:"id"`
	InstrumentID *int64          `json:"instrument_id,omitempty"`
	Symbol       string          `json:"symbol"`
	ChangeType   string          `json:"change_type"`
	Field        string          `json:"field"`
	OldValue     *string         `json:"old_value,omitempty"`
	NewValue     *string         `json:"new_value,omitempty"`
	Payload      json.RawMessage `json:"payload"`
	Reason       string          `json:"reason"`
	Stage        string          `json:"stage"`
	RequestedBy  int64           `json:"requested_by"`
	ReviewedBy   *int64          `json:"reviewed_by,omitempty"`
	ApprovedBy   *int64          `json:"approved_by,omitempty"`
	Emergency    bool            `json:"emergency"`
	EffectiveAt  *time.Time      `json:"effective_at,omitempty"`
	CreatedAt    time.Time       `json:"created_at"`
	ReviewedAt   *time.Time      `json:"reviewed_at,omitempty"`
	DecidedAt    *time.Time      `json:"decided_at,omitempty"`
	AppliedAt    *time.Time      `json:"applied_at,omitempty"`
}

const changeCols = `
	id, instrument_id, symbol, change_type, field, old_value, new_value,
	payload, reason, stage, requested_by, reviewed_by, approved_by,
	emergency, effective_at, created_at, reviewed_at, decided_at, applied_at`

func scanChange(row pgx.Row) (*ChangeRequest, error) {
	var c ChangeRequest
	err := row.Scan(&c.ID, &c.InstrumentID, &c.Symbol, &c.ChangeType,
		&c.Field, &c.OldValue, &c.NewValue, &c.Payload, &c.Reason,
		&c.Stage, &c.RequestedBy, &c.ReviewedBy, &c.ApprovedBy,
		&c.Emergency, &c.EffectiveAt, &c.CreatedAt, &c.ReviewedAt,
		&c.DecidedAt, &c.AppliedAt)
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// GetChange loads one change request.
func (s *InstrumentMaintenanceService) GetChange(ctx context.Context, id int64) (*ChangeRequest, error) {
	c, err := scanChange(s.pool.QueryRow(ctx,
		`SELECT `+changeCols+` FROM instrument_change_requests WHERE id=$1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, excerrors.New("NOT_FOUND", fmt.Sprintf("change request %d not found", id))
	}
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "load change request", err)
	}
	return c, nil
}

// ListChanges returns open requests (the ops review queue).
func (s *InstrumentMaintenanceService) ListChanges(ctx context.Context) ([]ChangeRequest, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+changeCols+` FROM instrument_change_requests
		  WHERE stage IN ('PENDING_REVIEW','PENDING_APPROVAL','SCHEDULED')
		  ORDER BY id`)
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "list change requests", err)
	}
	defer rows.Close()
	out := []ChangeRequest{}
	for rows.Next() {
		c, err := scanChange(rows)
		if err != nil {
			return nil, excerrors.Wrap("INTERNAL_ERROR", "scan change request", err)
		}
		out = append(out, *c)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// Creation chain — RM proposes → Compliance reviews → Super Admin applies.
// ---------------------------------------------------------------------------

// ProposeCreate opens a CREATE request (PENDING_REVIEW). The full
// InstrumentCreate payload rides in payload; the symbol is validated to
// the AAA/BBB ISO-4217 convention up front.
func (s *InstrumentMaintenanceService) ProposeCreate(ctx context.Context,
	actor AdminActor, in InstrumentCreate) (*ChangeRequest, error) {

	if err := s.requireRole(ctx, actor.UserID, maintProposeRoles, "instrument creation proposal"); err != nil {
		return nil, err
	}
	base, quote, err := ValidatePairSymbol(in.Symbol)
	if err != nil {
		return nil, err
	}
	in.Symbol = strings.ToUpper(strings.TrimSpace(in.Symbol))
	in.BaseCurrency, in.QuoteCurrency = base, quote
	if strings.TrimSpace(in.Reason) == "" {
		return nil, excerrors.New("INVALID_REQUEST", "a reason is mandatory for instrument creation")
	}
	payload, _ := json.Marshal(in)

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "begin tx", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	c, err := s.insertChange(ctx, tx, insertChange{
		symbol: in.Symbol, changeType: ChangeCreate, field: "INSTRUMENT",
		payload: payload, reason: in.Reason, stage: ChgPendingReview,
		requestedBy: actor.UserID,
	})
	if err != nil {
		return nil, err
	}
	if err := s.logChange(ctx, tx, c.ID, nil, c.Symbol, "SUBMITTED",
		c.Field, nil, nil, actor.UserID, in.Reason); err != nil {
		return nil, err
	}
	if _, _, err := Log(ctx, tx, AuditEntry{
		AdminUserID: actor.UserID, Action: "instrument.change.propose_create",
		TargetType: "instrument",
		AfterState: map[string]any{"change_id": c.ID, "symbol": c.Symbol},
		IPAddress:  actor.ClientIP,
	}); err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "audit log", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "commit", err)
	}
	return c, nil
}

// ReviewCreate is the Compliance-Officer leg — PENDING_REVIEW →
// PENDING_APPROVAL (or REJECTED). The reviewer must differ from the
// proposer (three distinct principals across the chain, §7.2).
func (s *InstrumentMaintenanceService) ReviewCreate(ctx context.Context,
	actor AdminActor, reqID int64, approve bool, note string) (*ChangeRequest, error) {

	if err := s.requireRole(ctx, actor.UserID, maintReviewRoles, "instrument creation review"); err != nil {
		return nil, err
	}
	c, err := s.lockChange(ctx, reqID)
	if err != nil {
		return nil, err
	}
	if c.ChangeType != ChangeCreate || c.Stage != ChgPendingReview {
		return nil, excerrors.New("INVALID_REQUEST",
			fmt.Sprintf("change %d is %s — not awaiting compliance review", reqID, c.Stage))
	}
	if actor.UserID == c.RequestedBy {
		return nil, excerrors.New("DUAL_CONTROL_VIOLATION",
			"the proposer cannot review their own creation request (§8.2)")
	}

	to := ChgPendingApproval
	action := "REVIEWED"
	if !approve {
		to, action = ChgRejected, "REJECTED"
	}
	return s.advance(ctx, c, actor, to, action,
		map[string]string{"reviewed_by": strconv.FormatInt(actor.UserID, 10),
			"reviewed_at": "now()", "decided_at": "now()"}, note)
}

// ApproveCreate is the Super-Admin leg — PENDING_APPROVAL → the DRAFT
// insert runs inside the same transaction via CreateInTx → APPLIED.
// The approver must differ from proposer AND reviewer.
func (s *InstrumentMaintenanceService) ApproveCreate(ctx context.Context,
	actor AdminActor, reqID int64) (*ChangeRequest, *Instrument, error) {

	if err := s.requireRole(ctx, actor.UserID, maintApproveRoles, "instrument creation approval"); err != nil {
		return nil, nil, err
	}
	c, err := s.lockChange(ctx, reqID)
	if err != nil {
		return nil, nil, err
	}
	if c.ChangeType != ChangeCreate || c.Stage != ChgPendingApproval {
		return nil, nil, excerrors.New("INVALID_REQUEST",
			fmt.Sprintf("change %d is %s — not awaiting approval", reqID, c.Stage))
	}
	if actor.UserID == c.RequestedBy ||
		(c.ReviewedBy != nil && actor.UserID == *c.ReviewedBy) {
		return nil, nil, excerrors.New("DUAL_CONTROL_VIOLATION",
			"creation approval requires a third distinct principal (§8.2)")
	}
	var spec InstrumentCreate
	if err := json.Unmarshal(c.Payload, &spec); err != nil {
		return nil, nil, excerrors.Wrap("INTERNAL_ERROR", "decode create payload", err)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, nil, excerrors.Wrap("INTERNAL_ERROR", "begin tx", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	inst, err := s.instruments.CreateInTx(ctx, tx,
		AdminActor{UserID: actor.UserID, ApproverID: actor.UserID, ClientIP: actor.ClientIP}, spec)
	if err != nil {
		return nil, nil, err
	}
	upd, err := s.updateStage(ctx, tx, c.ID, ChgPendingApproval, ChgApplied,
		map[string]string{
			"approved_by": strconv.FormatInt(actor.UserID, 10),
			"applied_at":  "now()",
		})
	if err != nil {
		return nil, nil, err
	}
	if err := s.logChange(ctx, tx, c.ID, &inst.ID, c.Symbol, "APPLIED",
		"INSTRUMENT", nil, strPtr("status=DRAFT"), actor.UserID, spec.Reason); err != nil {
		return nil, nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, nil, excerrors.Wrap("INTERNAL_ERROR", "commit", err)
	}
	s.emitSecurityStatus(ctx, SecurityStatusEvent{
		Event: "SECURITY_STATUS", Symbol: inst.Symbol, Status: inst.Status,
		Field: "INSTRUMENT", NewValue: "CREATED", ChangeID: c.ID,
		Source: "instrument-maintenance", TsMs: s.now().UnixMilli(),
	})
	s.publishWS(InstrumentStatusEvent{
		Event: "INSTRUMENT_CREATED", Symbol: inst.Symbol, To: inst.Status,
		ActorID: actor.UserID, Source: "instrument-maintenance",
		TsMs: s.now().UnixMilli(),
	})
	return upd, inst, nil
}

// ---------------------------------------------------------------------------
// Parameter changes — maker-checker, effective next session start.
// ---------------------------------------------------------------------------

// ParamChangeInput is a single-field scheduled parameter change.
type ParamChangeInput struct {
	InstrumentID int64      `json:"instrument_id"`
	Field        string     `json:"field"`
	NewValue     string     `json:"new_value"`
	Reason       string     `json:"reason"`
	EffectiveAt  *time.Time `json:"effective_at"` // nil → next session boundary
}

// ProposeParamChange is the maker leg (Risk Manager+). Effective
// scheduling is decided at approval (next session boundary by default).
func (s *InstrumentMaintenanceService) ProposeParamChange(ctx context.Context,
	actor AdminActor, in ParamChangeInput) (*ChangeRequest, error) {

	if err := s.requireRole(ctx, actor.UserID, maintParamRoles, "instrument parameter change"); err != nil {
		return nil, err
	}
	if err := validateParam(in.Field, in.NewValue); err != nil {
		return nil, err
	}
	if strings.TrimSpace(in.Reason) == "" {
		return nil, excerrors.New("INVALID_REQUEST", "a reason is mandatory for parameter changes")
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "begin tx", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Lock the instrument row — captures old_value and serializes
	// concurrent proposals against the same instrument (the partial
	// unique index additionally pins one open request per field).
	inst, err := s.instruments.lockRow(ctx, tx, in.InstrumentID)
	if err != nil {
		return nil, err
	}
	if inst.Status == InstDelisted {
		return nil, excerrors.New("INVALID_LIFECYCLE_TRANSITION",
			"DELISTED instruments are terminal — no parameter changes")
	}
	old, err := paramCurrentValue(ctx, tx, inst.ID, in.Field)
	if err != nil {
		return nil, err
	}
	if old != nil && *old == in.NewValue {
		return nil, excerrors.New("INVALID_REQUEST",
			"new_value equals the current value — no change")
	}

	c, err := s.insertChange(ctx, tx, insertChange{
		instrumentID: &inst.ID, symbol: inst.Symbol, changeType: ChangeParam,
		field: in.Field, oldValue: old, newValue: &in.NewValue,
		reason: in.Reason, stage: ChgPendingApproval,
		requestedBy: actor.UserID, effectiveAt: in.EffectiveAt,
	})
	if err != nil {
		return nil, err
	}
	if err := s.logChange(ctx, tx, c.ID, &inst.ID, inst.Symbol, "SUBMITTED",
		in.Field, old, &in.NewValue, actor.UserID, in.Reason); err != nil {
		return nil, err
	}
	if _, _, err := Log(ctx, tx, AuditEntry{
		AdminUserID: actor.UserID, Action: "instrument.change.propose_param",
		TargetType: "instrument", TargetID: &inst.ID,
		AfterState: map[string]any{"change_id": c.ID, "field": in.Field,
			"old": strVal(old), "new": in.NewValue, "reason": in.Reason},
		IPAddress: actor.ClientIP,
	}); err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "audit log", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "commit", err)
	}
	return c, nil
}

// DecideParamChange is the checker leg — a distinct Risk Manager+
// approver schedules the change for the next session boundary (or the
// explicit effective_at on the request); rejection lands REJECTED.
func (s *InstrumentMaintenanceService) DecideParamChange(ctx context.Context,
	actor AdminActor, reqID int64, approve bool, note string) (*ChangeRequest, error) {

	if err := s.requireRole(ctx, actor.UserID, maintParamRoles, "parameter change approval"); err != nil {
		return nil, err
	}
	c, err := s.lockChange(ctx, reqID)
	if err != nil {
		return nil, err
	}
	if c.ChangeType != ChangeParam || c.Stage != ChgPendingApproval {
		return nil, excerrors.New("INVALID_REQUEST",
			fmt.Sprintf("change %d is %s — not awaiting a checker", reqID, c.Stage))
	}
	if actor.UserID == c.RequestedBy {
		return nil, excerrors.New("DUAL_CONTROL_VIOLATION",
			"the maker cannot check their own change (§8.2)")
	}
	if !approve {
		return s.advance(ctx, c, actor, ChgRejected, "REJECTED",
			map[string]string{"approved_by": strconv.FormatInt(actor.UserID, 10),
				"decided_at": "now()"}, note)
	}
	eff := s.next(s.now())
	if c.EffectiveAt != nil {
		eff = c.EffectiveAt.UTC()
		if !eff.After(s.now()) {
			return nil, excerrors.New("INVALID_REQUEST",
				"effective_at must be in the future")
		}
	}
	upd, err := s.advance(ctx, c, actor, ChgScheduled, "APPROVED",
		map[string]string{
			"approved_by":  strconv.FormatInt(actor.UserID, 10),
			"decided_at":   "now()",
			"effective_at": fmt.Sprintf("'%s'::timestamptz", eff.Format(time.RFC3339Nano)),
		}, note)
	if err != nil {
		return nil, err
	}
	return upd, nil
}

// EmergencyParamChange is the Super-Admin-only break-glass path: the
// change applies IMMEDIATELY and a P1 alert fires. Single-principal by
// design (spec §7.4 — emergency authority; the change-log row carries
// emergency=true as the after-the-fact audit).
func (s *InstrumentMaintenanceService) EmergencyParamChange(ctx context.Context,
	actor AdminActor, in ParamChangeInput) (*ChangeRequest, error) {

	if err := s.requireRole(ctx, actor.UserID, maintApproveRoles, "emergency parameter change"); err != nil {
		return nil, err
	}
	if err := validateParam(in.Field, in.NewValue); err != nil {
		return nil, err
	}
	if strings.TrimSpace(in.Reason) == "" {
		return nil, excerrors.New("INVALID_REQUEST",
			"emergency changes must carry a reason — it is the audit record")
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "begin tx", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	inst, err := s.instruments.lockRow(ctx, tx, in.InstrumentID)
	if err != nil {
		return nil, err
	}
	old, err := paramCurrentValue(ctx, tx, inst.ID, in.Field)
	if err != nil {
		return nil, err
	}
	c, err := s.insertChange(ctx, tx, insertChange{
		instrumentID: &inst.ID, symbol: inst.Symbol, changeType: ChangeParam,
		field: in.Field, oldValue: old, newValue: &in.NewValue,
		reason: in.Reason, stage: ChgApplied,
		requestedBy: actor.UserID, approvedBy: &actor.UserID,
		emergency: true, appliedNow: true,
	})
	if err != nil {
		return nil, err
	}
	if err := s.applyParamTx(ctx, tx, inst.ID, in.Field, in.NewValue); err != nil {
		return nil, err
	}
	if err := s.logChange(ctx, tx, c.ID, &inst.ID, inst.Symbol, "APPLIED",
		in.Field, old, &in.NewValue, actor.UserID, in.Reason); err != nil {
		return nil, err
	}
	if _, _, err := Log(ctx, tx, AuditEntry{
		AdminUserID: actor.UserID, Action: "instrument.change.emergency",
		TargetType: "instrument", TargetID: &inst.ID,
		BeforeState: map[string]any{"field": in.Field, "value": strVal(old)},
		AfterState: map[string]any{"field": in.Field, "value": in.NewValue,
			"emergency": true, "reason": in.Reason},
		IPAddress: actor.ClientIP,
	}); err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "audit log", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "commit", err)
	}

	// P1 alert — the change is committed; alert failure never masks it.
	msg := fmt.Sprintf("P1 emergency parameter change: %s %s %s → %s (%s)",
		inst.Symbol, in.Field, strVal(old), in.NewValue, in.Reason)
	if s.alerter != nil {
		if err := s.alerter.Alert(ctx, "P1", "INSTRUMENT_EMERGENCY_CHANGE", msg); err != nil {
			s.logf("instrument maintenance: P1 alert failed: %v", err)
		}
	} else {
		s.logf("instrument maintenance: %s (alerter unwired)", msg)
	}
	s.emitSecurityStatus(ctx, SecurityStatusEvent{
		Event: "SECURITY_STATUS", Symbol: inst.Symbol, Status: inst.Status,
		Field: in.Field, OldValue: strVal(old), NewValue: in.NewValue,
		Emergency: true, ChangeID: c.ID, EffectiveAt: s.now().UnixMilli(),
		Source: "instrument-maintenance", TsMs: s.now().UnixMilli(),
	})
	s.publishWS(InstrumentStatusEvent{
		Event: "INSTRUMENT_PARAMETERS", Symbol: inst.Symbol, To: inst.Status,
		Reason: in.Reason + " (emergency)", ActorID: actor.UserID,
		Source: "instrument-maintenance", TsMs: s.now().UnixMilli(),
	})
	return c, nil
}

// CancelChange lets the maker withdraw a still-open request.
func (s *InstrumentMaintenanceService) CancelChange(ctx context.Context,
	actor AdminActor, reqID int64) (*ChangeRequest, error) {

	c, err := s.lockChange(ctx, reqID)
	if err != nil {
		return nil, err
	}
	if actor.UserID != c.RequestedBy {
		return nil, excerrors.New("UNAUTHORIZED_ROLE",
			"only the requesting admin can withdraw a change")
	}
	switch c.Stage {
	case ChgPendingReview, ChgPendingApproval, ChgScheduled:
	default:
		return nil, excerrors.New("INVALID_REQUEST",
			fmt.Sprintf("change %d is %s — not cancellable", reqID, c.Stage))
	}
	return s.advance(ctx, c, actor, ChgCancelled, "CANCELLED", nil, "")
}

// RejectChange turns down a pending CREATE review/approval or a pending
// PARAM change — role gate matches the stage the request sits at.
func (s *InstrumentMaintenanceService) RejectChange(ctx context.Context,
	actor AdminActor, reqID int64, note string) (*ChangeRequest, error) {

	c, err := s.lockChange(ctx, reqID)
	if err != nil {
		return nil, err
	}
	switch {
	case c.Stage == ChgPendingReview:
		if err := s.requireRole(ctx, actor.UserID, maintReviewRoles, "creation review rejection"); err != nil {
			return nil, err
		}
	case c.Stage == ChgPendingApproval && c.ChangeType == ChangeCreate:
		if err := s.requireRole(ctx, actor.UserID, maintApproveRoles, "creation approval rejection"); err != nil {
			return nil, err
		}
	case c.Stage == ChgPendingApproval:
		if err := s.requireRole(ctx, actor.UserID, maintParamRoles, "parameter change rejection"); err != nil {
			return nil, err
		}
	default:
		return nil, excerrors.New("INVALID_REQUEST",
			fmt.Sprintf("change %d is %s — not rejectable", reqID, c.Stage))
	}
	if actor.UserID == c.RequestedBy {
		return nil, excerrors.New("DUAL_CONTROL_VIOLATION",
			"the requester cannot decide their own change (§8.2)")
	}
	return s.advance(ctx, c, actor, ChgRejected, "REJECTED",
		map[string]string{"decided_at": "now()"}, note)
}

// ---------------------------------------------------------------------------
// Delisting — rides the existing OpInstrumentDelist four-eyes queue.
// ---------------------------------------------------------------------------

// RequestDelist submits the delist through the landed dual-control
// pipeline (OpInstrumentDelist → TransitionTx, Super Admin approver) and
// opens the DELIST change-request trail. The §7.5 ladder is owned by the
// lifecycle engine — RESTRICTED notice / DELISTED close-only.
func (s *InstrumentMaintenanceService) RequestDelist(ctx context.Context,
	actor AdminActor, instrumentID int64, reason string) (*ChangeRequest, *DualControlRequest, error) {

	if s.dual == nil {
		return nil, nil, excerrors.New("DUAL_CONTROL_REQUIRED",
			"delist requires the four-eyes queue — dual-control service unwired")
	}
	// Spec §7.2: delist is a Super-Admin op — and DualControlService.Submit
	// enforces that the maker itself satisfies RequiredRole.
	if err := s.requireRole(ctx, actor.UserID, maintApproveRoles, "delist request"); err != nil {
		return nil, nil, err
	}
	if strings.TrimSpace(reason) == "" {
		return nil, nil, excerrors.New("INVALID_REQUEST", "a reason is mandatory for delisting")
	}
	inst, err := s.instruments.Get(ctx, instrumentID)
	if err != nil {
		return nil, nil, err
	}
	if inst == nil {
		return nil, nil, excerrors.New("NOT_FOUND",
			fmt.Sprintf("instrument %d not found", instrumentID))
	}
	if inst.Status == InstDelisted || inst.Status == InstDraft {
		return nil, nil, excerrors.New("INVALID_LIFECYCLE_TRANSITION",
			fmt.Sprintf("cannot delist from %s", inst.Status))
	}

	dc, err := s.dual.Submit(ctx, SubmitInput{
		Operation:    OpInstrumentDelist,
		TargetType:   "instrument",
		TargetID:     strconv.FormatInt(instrumentID, 10),
		RequiredRole: RoleSuperAdmin,
		RequestedBy:  actor.UserID,
		Reason:       reason,
		ClientIP:     actor.ClientIP,
		Payload: map[string]any{
			"instrument_id": instrumentID, "reason": reason,
			"client_ip": actor.ClientIP,
		},
	})
	if err != nil {
		return nil, nil, err
	}
	payload, _ := json.Marshal(map[string]any{
		"dual_control_id": dc.ID, "ladder": "RESTRICTED 24h → DELISTED → 30d close-only (§7.5)",
	})
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, nil, excerrors.Wrap("INTERNAL_ERROR", "begin tx", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	c, err := s.insertChange(ctx, tx, insertChange{
		instrumentID: &inst.ID, symbol: inst.Symbol, changeType: ChangeDelist,
		field: "status", oldValue: &inst.Status, newValue: strPtr(InstDelisted),
		reason: reason, stage: ChgPendingApproval,
		requestedBy: actor.UserID, payload: payload,
	})
	if err != nil {
		return nil, nil, err
	}
	if err := s.logChange(ctx, tx, c.ID, &inst.ID, inst.Symbol, "SUBMITTED",
		"status", &inst.Status, strPtr(InstDelisted), actor.UserID, reason); err != nil {
		return nil, nil, err
	}
	if _, _, err := Log(ctx, tx, AuditEntry{
		AdminUserID: actor.UserID, Action: "instrument.change.delist",
		TargetType: "instrument", TargetID: &inst.ID,
		AfterState: map[string]any{"change_id": c.ID, "dual_control_id": dc.ID,
			"reason": reason},
		IPAddress: actor.ClientIP,
	}); err != nil {
		return nil, nil, excerrors.Wrap("INTERNAL_ERROR", "audit log", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, nil, excerrors.Wrap("INTERNAL_ERROR", "commit", err)
	}
	return c, dc, nil
}

// ---------------------------------------------------------------------------
// Sweeps — scheduled activations + delist sync.
// ---------------------------------------------------------------------------

// ApplyDue applies every SCHEDULED change whose effective_at has arrived
// (the session-boundary activation) and reconciles DELIST requests
// against the dual-control store. The Task-15.3.7 session-lifecycle
// sweep calls this at each session start (and it is safe to call on any
// housekeeping tick).
func (s *InstrumentMaintenanceService) ApplyDue(ctx context.Context) (applied int, err error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+changeCols+` FROM instrument_change_requests
		  WHERE stage='SCHEDULED' AND effective_at <= now() ORDER BY id`)
	if err != nil {
		return 0, excerrors.Wrap("INTERNAL_ERROR", "due changes scan", err)
	}
	var due []ChangeRequest
	for rows.Next() {
		c, err := scanChange(rows)
		if err != nil {
			rows.Close()
			return applied, excerrors.Wrap("INTERNAL_ERROR", "scan due change", err)
		}
		due = append(due, *c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return applied, excerrors.Wrap("INTERNAL_ERROR", "due changes rows", err)
	}

	for _, c := range due {
		switch c.ChangeType {
		case ChangeParam:
			if err := s.applyScheduled(ctx, &c); err != nil {
				s.logf("instrument maintenance: apply change %d: %v", c.ID, err)
				continue // retry next tick — fail closed, never half-apply
			}
			applied++
		}
	}
	// DELIST reconciliation against the dual-control store.
	if s.dual != nil {
		if err := s.SyncDelistRequests(ctx); err != nil {
			s.logf("instrument maintenance: delist sync: %v", err)
		}
	}
	return applied, nil
}

// applyScheduled commits one due parameter change inside its own tx.
func (s *InstrumentMaintenanceService) applyScheduled(ctx context.Context, c *ChangeRequest) error {
	if c.InstrumentID == nil || c.NewValue == nil {
		return fmt.Errorf("change %d missing instrument/value", c.ID)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return excerrors.Wrap("INTERNAL_ERROR", "begin tx", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// CAS the stage — a concurrent cancel/applier loses cleanly.
	var id int64
	err = tx.QueryRow(ctx, `
		UPDATE instrument_change_requests
		   SET stage='APPLIED', applied_at=now()
		 WHERE id=$1 AND stage='SCHEDULED'
		RETURNING id`, c.ID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil // raced cancel — clean no-op
	}
	if err != nil {
		return excerrors.Wrap("INTERNAL_ERROR", "stage apply", err)
	}
	if err := s.applyParamTx(ctx, tx, *c.InstrumentID, c.Field, *c.NewValue); err != nil {
		return err
	}
	if err := s.logChange(ctx, tx, c.ID, c.InstrumentID, c.Symbol, "APPLIED",
		c.Field, c.OldValue, c.NewValue, c.RequestedBy, c.Reason); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return excerrors.Wrap("INTERNAL_ERROR", "commit", err)
	}
	s.emitSecurityStatus(ctx, SecurityStatusEvent{
		Event: "SECURITY_STATUS", Symbol: c.Symbol,
		Field: c.Field, OldValue: strVal(c.OldValue), NewValue: *c.NewValue,
		ChangeID: c.ID, Source: "instrument-maintenance", TsMs: s.now().UnixMilli(),
	})
	s.publishWS(InstrumentStatusEvent{
		Event: "INSTRUMENT_PARAMETERS", Symbol: c.Symbol, Reason: c.Reason,
		Source: "instrument-maintenance", TsMs: s.now().UnixMilli(),
	})
	return nil
}

// SyncDelistRequests reconciles open DELIST change requests with their
// dual-control outcome: EXECUTED → APPLIED (+security_status event),
// REJECTED/EXPIRED → REJECTED. Idempotent — callable every tick.
func (s *InstrumentMaintenanceService) SyncDelistRequests(ctx context.Context) error {
	rows, err := s.pool.Query(ctx,
		`SELECT `+changeCols+` FROM instrument_change_requests
		  WHERE change_type='DELIST'
		    AND stage IN ('PENDING_APPROVAL','SCHEDULED')`)
	if err != nil {
		return excerrors.Wrap("INTERNAL_ERROR", "delist requests scan", err)
	}
	var open []ChangeRequest
	for rows.Next() {
		c, err := scanChange(rows)
		if err != nil {
			rows.Close()
			return excerrors.Wrap("INTERNAL_ERROR", "scan delist request", err)
		}
		open = append(open, *c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return excerrors.Wrap("INTERNAL_ERROR", "delist rows", err)
	}

	for _, c := range open {
		var p struct {
			DualControlID int64 `json:"dual_control_id"`
		}
		if err := json.Unmarshal(c.Payload, &p); err != nil || p.DualControlID == 0 {
			s.logf("instrument maintenance: delist change %d missing dual_control_id", c.ID)
			continue
		}
		req, err := s.dual.Get(ctx, p.DualControlID)
		if err != nil {
			continue // dual-control store hiccup — retry next tick
		}
		switch req.Status {
		case ReqExecuted:
			s.finishDelist(ctx, &c, req)
		case ReqRejected, ReqExpired:
			s.closeDelist(ctx, &c, ChgRejected, "REJECTED",
				fmt.Sprintf("dual-control request %d %s", req.ID, req.Status))
		}
	}
	return nil
}

// finishDelist marks a DELIST request APPLIED once the four-eyes
// transition has committed; emits the security_status event.
func (s *InstrumentMaintenanceService) finishDelist(ctx context.Context, c *ChangeRequest, req *DualControlRequest) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		s.logf("instrument maintenance: finishDelist tx: %v", err)
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var id int64
	err = tx.QueryRow(ctx, `
		UPDATE instrument_change_requests
		   SET stage='APPLIED', approved_by=$2, applied_at=now()
		 WHERE id=$1 AND stage IN ('PENDING_APPROVAL','SCHEDULED')
		RETURNING id`, c.ID, req.ApprovedBy).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return
	}
	if err != nil {
		s.logf("instrument maintenance: finishDelist stage: %v", err)
		return
	}
	newSt := InstDelisted
	if err := s.logChange(ctx, tx, c.ID, c.InstrumentID, c.Symbol, "APPLIED",
		"status", c.OldValue, &newSt, c.RequestedBy,
		fmt.Sprintf("dual-control %d executed by approver %v", req.ID, req.ApprovedBy)); err != nil {
		s.logf("instrument maintenance: finishDelist log: %v", err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		s.logf("instrument maintenance: finishDelist commit: %v", err)
		return
	}
	var approver int64
	if req.ApprovedBy != nil {
		approver = *req.ApprovedBy
	}
	s.emitSecurityStatus(ctx, SecurityStatusEvent{
		Event: "SECURITY_STATUS", Symbol: c.Symbol, Status: InstDelisted,
		Field: "status", OldValue: strVal(c.OldValue), NewValue: InstDelisted,
		ChangeID: c.ID, Source: "instrument-maintenance", TsMs: s.now().UnixMilli(),
	})
	s.publishWS(InstrumentStatusEvent{
		Event: "INSTRUMENT_STATUS", Symbol: c.Symbol,
		From: strVal(c.OldValue), To: InstDelisted, Reason: c.Reason,
		ActorID: c.RequestedBy, ApproverID: approver,
		Source: "instrument-maintenance", TsMs: s.now().UnixMilli(),
	})
}

func (s *InstrumentMaintenanceService) closeDelist(ctx context.Context, c *ChangeRequest,
	stage, action, note string) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		s.logf("instrument maintenance: closeDelist tx: %v", err)
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var id int64
	err = tx.QueryRow(ctx, `
		UPDATE instrument_change_requests SET stage=$2, decided_at=now()
		 WHERE id=$1 AND stage IN ('PENDING_APPROVAL','SCHEDULED')
		RETURNING id`, c.ID, stage).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return
	}
	if err != nil {
		s.logf("instrument maintenance: closeDelist: %v", err)
		return
	}
	if err := s.logChange(ctx, tx, c.ID, c.InstrumentID, c.Symbol, action,
		c.Field, c.OldValue, c.NewValue, c.RequestedBy, note); err != nil {
		s.logf("instrument maintenance: closeDelist log: %v", err)
		return
	}
	_ = tx.Commit(ctx)
}

// ---------------------------------------------------------------------------
// Parameter application + validation
// ---------------------------------------------------------------------------

// validateParam enforces the parameter whitelist + value shape before a
// request is ever persisted.
func validateParam(field, value string) error {
	if !paramColumns[field] && !paramOverrideKeys[field] {
		return excerrors.New("INVALID_REQUEST",
			fmt.Sprintf("field %q is not a mutable instrument parameter", field))
	}
	if strings.TrimSpace(value) == "" {
		return excerrors.New("INVALID_REQUEST", "new_value is required")
	}
	if intParams[field] {
		n, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
		if err != nil {
			return excerrors.New("INVALID_REQUEST", field+" must be an integer")
		}
		if field == "settlement_cycle" {
			if n < 0 || n > 2 {
				return excerrors.New("INVALID_REQUEST", "settlement_cycle must be 0, 1 or 2")
			}
			return nil
		}
		if n <= 0 {
			return excerrors.New("INVALID_REQUEST", field+" must be positive")
		}
		return nil
	}
	if field == "trading_hours" {
		// JSON session spec (validated structurally here; Task 15.3.7 owns
		// the semantic interpretation).
		var v any
		if len(value) > 2000 || json.Unmarshal([]byte(value), &v) != nil {
			return excerrors.New("INVALID_REQUEST",
				"trading_hours must be a JSON session specification (≤2000 chars)")
		}
		return nil
	}
	// Decimal family — DECIMAL(20,8) quantum, positive.
	if !validDecimal(value) || strings.HasPrefix(value, "-") || value == "0" || value == "0.0" {
		return excerrors.New("INVALID_REQUEST",
			field+" must be a positive decimal")
	}
	if len(value) > 22 {
		return excerrors.New("INVALID_REQUEST", field+" exceeds DECIMAL(20,8) width")
	}
	return nil
}

// paramCurrentValue reads the live value of a columnar or override
// parameter (old_value for the audit trail).
func paramCurrentValue(ctx context.Context, tx pgx.Tx, instrumentID int64, field string) (*string, error) {
	var v *string
	var err error
	if paramColumns[field] {
		err = tx.QueryRow(ctx, fmt.Sprintf(
			`SELECT %s::text FROM instruments WHERE id=$1`, field), instrumentID).Scan(&v)
	} else {
		err = tx.QueryRow(ctx, `
			SELECT param_overrides->>$2 FROM instruments WHERE id=$1`,
			instrumentID, field).Scan(&v)
	}
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "read current "+field, err)
	}
	return v, nil
}

// applyParamTx writes the parameter — a real column for columnar fields,
// a param_overrides merge otherwise. Field names come exclusively from
// the validated whitelist (never user input in the SQL shape).
func (s *InstrumentMaintenanceService) applyParamTx(ctx context.Context, tx pgx.Tx,
	instrumentID int64, field, value string) error {

	var tag pgconn.CommandTag
	var err error
	if paramColumns[field] {
		tag, err = tx.Exec(ctx, fmt.Sprintf(
			`UPDATE instruments SET %s = $2, updated_at = now() WHERE id = $1`,
			field), instrumentID, value)
	} else {
		tag, err = tx.Exec(ctx, `
			UPDATE instruments
			   SET param_overrides = jsonb_set(param_overrides, ARRAY[$2], to_jsonb($3::text), true),
			       updated_at = now()
			 WHERE id = $1`, instrumentID, field, value)
	}
	if err != nil {
		return excerrors.Wrap("INTERNAL_ERROR", "apply parameter "+field, err)
	}
	if tag.RowsAffected() != 1 {
		return excerrors.New("NOT_FOUND",
			fmt.Sprintf("instrument %d not found for parameter apply", instrumentID))
	}
	return nil
}

// ---------------------------------------------------------------------------
// Row helpers
// ---------------------------------------------------------------------------

type insertChange struct {
	instrumentID *int64
	symbol       string
	changeType   string
	field        string
	oldValue     *string
	newValue     *string
	payload      json.RawMessage
	reason       string
	stage        string
	requestedBy  int64
	approvedBy   *int64
	emergency    bool
	effectiveAt  *time.Time
	appliedNow   bool
}

func (s *InstrumentMaintenanceService) insertChange(ctx context.Context, tx pgx.Tx,
	in insertChange) (*ChangeRequest, error) {

	if in.payload == nil {
		in.payload = json.RawMessage(`{}`)
	}
	var appliedAt any
	if in.appliedNow {
		appliedAt = s.now().UTC()
	}
	c, err := scanChange(tx.QueryRow(ctx, `
		INSERT INTO instrument_change_requests
		    (instrument_id, symbol, change_type, field, old_value, new_value,
		     payload, reason, stage, requested_by, approved_by, emergency,
		     effective_at, applied_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)
		RETURNING `+changeCols,
		in.instrumentID, in.symbol, in.changeType, in.field,
		in.oldValue, in.newValue, in.payload, in.reason, in.stage,
		in.requestedBy, in.approvedBy, in.emergency, in.effectiveAt, appliedAt))
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return nil, excerrors.New("INVALID_REQUEST",
				"an open change request already exists for this instrument/field")
		}
		return nil, excerrors.Wrap("INTERNAL_ERROR", "insert change request", err)
	}
	return c, nil
}

// lockChange SELECT FOR UPDATEs a request row (short-lived tx — the
// callers re-do real work under their own transaction).
func (s *InstrumentMaintenanceService) lockChange(ctx context.Context, reqID int64) (*ChangeRequest, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "begin tx", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	c, err := scanChange(tx.QueryRow(ctx,
		`SELECT `+changeCols+` FROM instrument_change_requests WHERE id=$1 FOR UPDATE`, reqID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, excerrors.New("NOT_FOUND", fmt.Sprintf("change request %d not found", reqID))
	}
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "lock change request", err)
	}
	return c, nil
}

// updateStage CAS-advances a request's stage inside the caller's tx.
func (s *InstrumentMaintenanceService) updateStage(ctx context.Context, tx pgx.Tx,
	id int64, from, to string, sets map[string]string) (*ChangeRequest, error) {

	clause := "stage = '" + to + "'"
	for k, v := range sets {
		clause += ", " + k + " = " + v
	}
	c, err := scanChange(tx.QueryRow(ctx, fmt.Sprintf(
		`UPDATE instrument_change_requests SET %s
		  WHERE id = $1 AND stage = '%s' RETURNING `+changeCols,
		clause, from), id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, excerrors.New("INVALID_REQUEST",
			fmt.Sprintf("change %d left stage %s (concurrent decision)", id, from))
	}
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "advance change request", err)
	}
	return c, nil
}

// advance runs the common decide path: stage CAS + change_log + audit in
// one tx. `sets` uses pre-quoted SQL fragments (internal callers only).
func (s *InstrumentMaintenanceService) advance(ctx context.Context, c *ChangeRequest,
	actor AdminActor, to, action string, sets map[string]string, note string) (*ChangeRequest, error) {

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "begin tx", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	upd, err := s.updateStage(ctx, tx, c.ID, c.Stage, to, sets)
	if err != nil {
		return nil, err
	}
	reason := c.Reason
	if note != "" {
		reason += " — " + note
	}
	if err := s.logChange(ctx, tx, c.ID, c.InstrumentID, c.Symbol, action,
		c.Field, c.OldValue, c.NewValue, actor.UserID, reason); err != nil {
		return nil, err
	}
	if _, _, err := Log(ctx, tx, AuditEntry{
		AdminUserID: actor.UserID,
		Action:      "instrument.change." + strings.ToLower(action),
		TargetType:  "instrument", TargetID: c.InstrumentID,
		BeforeState: map[string]any{"change_id": c.ID, "stage": c.Stage},
		AfterState:  map[string]any{"change_id": c.ID, "stage": to, "note": note},
		IPAddress:   actor.ClientIP,
	}); err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "audit log", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "commit", err)
	}
	return upd, nil
}

// logChange appends the immutable instrument_change_log row.
func (s *InstrumentMaintenanceService) logChange(ctx context.Context, tx pgx.Tx,
	requestID int64, instrumentID *int64, symbol, action, field string,
	oldVal, newVal *string, actorID int64, reason string) error {

	_, err := tx.Exec(ctx, `
		INSERT INTO instrument_change_log
		    (request_id, instrument_id, symbol, action, field,
		     old_value, new_value, actor_id, reason)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
		requestID, instrumentID, symbol, action, field,
		oldVal, newVal, actorID, reason)
	if err != nil {
		return excerrors.Wrap("INTERNAL_ERROR", "instrument change log", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Event emission
// ---------------------------------------------------------------------------

// emitSecurityStatus publishes the NATS security_status event — the
// Phase-18 FIX SecurityStatus feed binds to the same payload shape.
func (s *InstrumentMaintenanceService) emitSecurityStatus(ctx context.Context, ev SecurityStatusEvent) {
	if s.nats == nil {
		s.logf("instrument maintenance: security_status unwired — %+v", ev)
		return
	}
	payload, err := json.Marshal(ev)
	if err != nil {
		s.logf("instrument maintenance: marshal security_status: %v", err)
		return
	}
	if err := s.nats.Publish(ctx, SecurityStatusSubject, payload); err != nil {
		s.logf("instrument maintenance: publish security_status %s: %v", ev.Symbol, err)
	}
}

func (s *InstrumentMaintenanceService) publishWS(ev InstrumentStatusEvent) {
	if s.ws != nil {
		s.ws.Publish(InstrumentStatusChannel, ev)
	}
}

// ---------------------------------------------------------------------------
// Small helpers
// ---------------------------------------------------------------------------

func strPtr(s string) *string { return &s }

func strVal(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
