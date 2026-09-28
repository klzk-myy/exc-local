// Task 5.3.30 — manual liquidation admin endpoint.
//
// POST /api/v1/admin/liquidation/manual
//
//	{account_id, instrument_id?, reason, override_auction, approver_id}
//
// Guarantees (task AC):
//   - Risk Manager (or stronger) RBAC via the AdminRoleResolver seam —
//     Phase-07 Task 7.3.1 owns the real role store; a nil resolver fails
//     closed with UNAUTHORIZED_ROLE.
//   - Dual control: a distinct, non-zero approver who is themselves a
//     resolvable admin. Both ids land in admin_audit_log AND the
//     audit_hash_chain row (spec §5.8/§5.9), in the same transaction as
//     the liquidation record so the audit trail cannot be lost.
//   - WAL MANUAL_LIQUIDATION emission goes through the
//     LiquidationEventSink seam — command wiring publishes it to the
//     `margin-events` JetStream stream (Task 1.3.11) where Phase-19's
//     auction machinery consumes it. nil sink ⇒ SERVICE_DEGRADED, no
//     DB write (fail closed, never a silent stub).
//   - override_auction=true skips the 5s CALL phase → FORCE_CASH at
//     mark; an explicit non-trivial reason is mandatory.
//   - This endpoint NEVER writes orders/balances itself — force-close is
//     the liquidation machinery's job (spec §13.4).
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"exchange/internal/audit"
	"exchange/internal/auth"
	"exchange/internal/gateway"
	"exchange/internal/middleware"

	excerrors "exchange/pkg/errors"
)

// AdminRoleResolver resolves an admin user id to its §8.2 role name.
// Phase-05 seam identical in spirit to accounts.RoleResolver; defined
// locally so the api package does not take a domain dependency.
type AdminRoleResolver func(ctx context.Context, adminUserID int64) (string, error)

// EligibleLiquidationRoles may initiate OR approve a manual liquidation
// (Risk Manager or stronger; task text). Support/auditor roles cannot.
var EligibleLiquidationRoles = map[string]bool{
	"Risk Manager": true,
	"Super Admin":  true,
}

// ManualLiquidationRequest is the decoded request body. account_id and
// instrument_id accept JSON strings or numbers (the task schema shows
// strings; BIGINT columns want ints).
type ManualLiquidationRequest struct {
	AccountID       json.RawMessage `json:"account_id"`
	InstrumentID    json.RawMessage `json:"instrument_id"`
	Reason          string          `json:"reason"`
	OverrideAuction bool            `json:"override_auction"`
	ApproverID      json.RawMessage `json:"approver_id"`
}

// PositionSnapshot is one position marked for liquidation — the
// estimated_fills payload AND the event payload the auction machinery
// consumes.
type PositionSnapshot struct {
	PositionID   int64  `json:"position_id"`
	InstrumentID int64  `json:"instrument_id"`
	Symbol       string `json:"symbol"`
	Side         string `json:"side"`     // LONG | SHORT
	Quantity     string `json:"quantity"` // decimal string — exact
	MarkPrice    string `json:"mark_price,omitempty"`
}

// ManualLiquidationEvent is the MANUAL_LIQUIDATION WAL-event payload
// (source: MANUAL) routed to the Phase-19 auction machinery.
type ManualLiquidationEvent struct {
	Event           string             `json:"event"`  // "MANUAL_LIQUIDATION"
	Source          string             `json:"source"` // "MANUAL"
	LiquidationID   int64              `json:"liquidation_id"`
	AccountID       int64              `json:"account_id"`
	InstrumentID    *int64             `json:"instrument_id"`
	OverrideAuction bool               `json:"override_auction"`
	Reason          string             `json:"reason"`
	Positions       []PositionSnapshot `json:"positions"`
	InitiatedBy     int64              `json:"initiated_by"`
	ApprovedBy      int64              `json:"approved_by"`
	TsMs            int64              `json:"ts_ms"`
}

// LiquidationEventSink delivers the MANUAL_LIQUIDATION event to the
// auction machinery (JetStream `margin-events` in production).
type LiquidationEventSink interface {
	EmitManualLiquidation(ctx context.Context, ev ManualLiquidationEvent) error
}

// AdminActor pairs the initiator with the four-eyes approver.
type AdminActor struct {
	UserID     int64
	ApproverID int64
	ClientIP   string
}

// LiquidationResult is the endpoint response body.
type LiquidationResult struct {
	LiquidationID   int64              `json:"liquidation_id"`
	AccountID       int64              `json:"account_id"`
	InstrumentID    *int64             `json:"instrument_id"`
	OverrideAuction bool               `json:"override_auction"`
	Source          string             `json:"source"` // "MANUAL"
	Status          string             `json:"status"`
	EstimatedFills  []PositionSnapshot `json:"estimated_fills"`
	Approvals       map[string]any     `json:"approvals"`
	AuditSeq        int64              `json:"audit_seq"`
	CreatedAt       time.Time          `json:"created_at"`
}

// recordParams is the durable record input for the liquidationStore tx.
type recordParams struct {
	AccountID       int64
	InstrumentID    *int64 // nil = all open positions
	Reason          string
	OverrideAuction bool
	InitiatedBy     int64
	ApprovedBy      int64
	ClientIP        string
}

// liquidationStore is the atomic-record seam: account lock, open-
// position read, manual_liquidations insert, admin_audit_log entry and
// the audit_hash_chain link in ONE transaction. PG is the production
// implementation; unit tests substitute a fake.
type liquidationStore interface {
	RecordLiquidation(ctx context.Context, p recordParams) (id, auditSeq int64, positions []PositionSnapshot, err error)
	MarkEmitFailed(ctx context.Context, id int64)
}

// pgLiquidationStore is the production liquidationStore.
type pgLiquidationStore struct{ pool *pgxpool.Pool }

func (s *pgLiquidationStore) MarkEmitFailed(ctx context.Context, id int64) {
	_, _ = s.pool.Exec(ctx,
		`UPDATE manual_liquidations SET status='EMIT_FAILED', updated_at=now() WHERE id=$1`, id)
}

// RecordLiquidation performs the atomic record: account lock, open-
// position read, manual_liquidations insert, admin_audit_log entry and
// the audit_hash_chain link (spec §5.8) — all in ONE transaction so the
// liquidation record cannot commit without its audit trail.
func (s *pgLiquidationStore) RecordLiquidation(ctx context.Context, p recordParams) (int64, int64, []PositionSnapshot, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, 0, nil, excerrors.Wrap("INTERNAL_ERROR", "begin tx", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var status string
	err = tx.QueryRow(ctx,
		`SELECT status FROM accounts WHERE id = $1 FOR UPDATE`, p.AccountID).Scan(&status)
	if err == pgx.ErrNoRows {
		return 0, 0, nil, excerrors.New("NOT_FOUND", fmt.Sprintf("account %d not found", p.AccountID))
	}
	if err != nil {
		return 0, 0, nil, excerrors.Wrap("INTERNAL_ERROR", "lock account", err)
	}

	// Open positions = rows with a live quantity. Manual liquidation
	// deliberately ignores account status — FROZEN/SUSPENDED accounts
	// are exactly the ones Risk Managers need to force-close.
	q := `SELECT p.id, p.instrument_id, i.symbol, p.side::text,
	             p.quantity::text, COALESCE(p.mark_price::text,'')
	        FROM positions p
	        JOIN instruments i ON i.id = p.instrument_id
	       WHERE p.account_id = $1 AND p.quantity <> 0`
	args := []any{p.AccountID}
	if p.InstrumentID != nil {
		q += " AND p.instrument_id = $2"
		args = append(args, *p.InstrumentID)
	}
	q += " ORDER BY p.id FOR UPDATE OF p"
	rows, err := tx.Query(ctx, q, args...)
	if err != nil {
		return 0, 0, nil, excerrors.Wrap("INTERNAL_ERROR", "read positions", err)
	}
	defer rows.Close()
	var positions []PositionSnapshot
	for rows.Next() {
		var ps PositionSnapshot
		if err := rows.Scan(&ps.PositionID, &ps.InstrumentID, &ps.Symbol,
			&ps.Side, &ps.Quantity, &ps.MarkPrice); err != nil {
			return 0, 0, nil, excerrors.Wrap("INTERNAL_ERROR", "scan position", err)
		}
		positions = append(positions, ps)
	}
	if err := rows.Err(); err != nil {
		return 0, 0, nil, excerrors.Wrap("INTERNAL_ERROR", "positions cursor", err)
	}
	if len(positions) == 0 {
		return 0, 0, nil, excerrors.New("NOT_FOUND",
			"account has no open positions matching the criteria")
	}

	positionsJSON, _ := json.Marshal(positions)
	var instArg any
	if p.InstrumentID != nil {
		instArg = *p.InstrumentID
	}
	var recID int64
	err = tx.QueryRow(ctx, `
		INSERT INTO manual_liquidations
		    (account_id, instrument_id, reason, override_auction, source,
		     status, initiated_by, approved_by, positions)
		VALUES ($1, $2, $3, $4, 'MANUAL', 'DISPATCHED', $5, $6, $7)
		RETURNING id`, p.AccountID, instArg, p.Reason, p.OverrideAuction,
		p.InitiatedBy, p.ApprovedBy, positionsJSON).Scan(&recID)
	if err != nil {
		return 0, 0, nil, excerrors.Wrap("INTERNAL_ERROR", "insert liquidation", err)
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO admin_audit_log
		    (admin_user_id, action, target_type, target_id, before_state, after_state, ip_address)
		VALUES ($1, 'liquidation.manual', 'account', $2,
		        jsonb_build_object('open_positions', $3::int),
		        jsonb_build_object('liquidation_id', $4::bigint,
		            'reason', $5::text, 'override_auction', $6::bool,
		            'approved_by', $7::bigint, 'positions', $8::jsonb),
		        NULLIF($9, '')::inet)`,
		p.InitiatedBy, p.AccountID, len(positions), recID,
		p.Reason, p.OverrideAuction, p.ApprovedBy, positionsJSON,
		p.ClientIP); err != nil {
		return 0, 0, nil, excerrors.Wrap("INTERNAL_ERROR", "admin audit insert", err)
	}

	// Audit hash chain (spec §5.8) in the SAME transaction — the
	// liquidation record cannot commit without its chain link.
	entry, err := audit.Append(ctx, tx, "manual_liquidations", &recID,
		"MANUAL_LIQ", positionsJSON)
	if err != nil {
		return 0, 0, nil, excerrors.Wrap("INTERNAL_ERROR", "audit chain append", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, 0, nil, excerrors.Wrap("INTERNAL_ERROR", "commit tx", err)
	}
	return recID, entry.SequenceNum, positions, nil
}

// ManualLiquidationService orchestrates the endpoint.
type ManualLiquidationService struct {
	store    liquidationStore
	resolver AdminRoleResolver
	sink     LiquidationEventSink
}

// NewManualLiquidationService wires the service; sink==nil fails closed
// at request time (the endpoint then reports SERVICE_DEGRADED rather
// than silently recording an undispatched liquidation).
func NewManualLiquidationService(pool *pgxpool.Pool, resolver AdminRoleResolver, sink LiquidationEventSink) *ManualLiquidationService {
	return &ManualLiquidationService{store: &pgLiquidationStore{pool: pool}, resolver: resolver, sink: sink}
}

// newManualLiquidationService is the seam for unit tests.
func newManualLiquidationService(store liquidationStore, resolver AdminRoleResolver, sink LiquidationEventSink) *ManualLiquidationService {
	return &ManualLiquidationService{store: store, resolver: resolver, sink: sink}
}

// parseID accepts a JSON number or decimal string → int64.
func parseFlexID(raw json.RawMessage, field string) (int64, bool, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return 0, false, nil // absent
	}
	s := strings.Trim(string(raw), `"`)
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil || v <= 0 {
		return 0, true, fmt.Errorf("%s must be a positive integer", field)
	}
	return v, true, nil
}

// Execute runs the full flow: role gate → dual control → tx
// (account+positions check, liquidation row, admin audit, hash chain)
// → post-commit WAL event emit.
func (s *ManualLiquidationService) Execute(ctx context.Context, actor AdminActor, req ManualLiquidationRequest) (*LiquidationResult, error) {
	if s.sink == nil {
		return nil, excerrors.New("SERVICE_DEGRADED",
			"liquidation event sink not wired")
	}
	// --- Role gate (Risk Manager or stronger) ---
	if actor.UserID == 0 {
		return nil, excerrors.New("UNAUTHORIZED", "admin identity required")
	}
	if s.resolver == nil {
		return nil, excerrors.New("UNAUTHORIZED_ROLE",
			"role resolver not configured (Phase-07 RBAC stub boundary)")
	}
	role, err := s.resolver(ctx, actor.UserID)
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "role lookup", err)
	}
	if !EligibleLiquidationRoles[role] {
		return nil, excerrors.New("UNAUTHORIZED_ROLE",
			"manual liquidation requires Risk Manager or Super Admin")
	}
	// --- Dual control / four-eyes ---
	var approverID int64
	var present bool
	if approverID, present, err = parseFlexID(req.ApproverID, "approver_id"); err != nil {
		return nil, excerrors.New("INVALID_REQUEST", err.Error())
	} else if !present || approverID == actor.UserID {
		return nil, excerrors.New("DUAL_CONTROL_REQUIRED",
			"manual liquidation requires a distinct second approver (four-eyes)")
	}
	approverRole, err := s.resolver(ctx, approverID)
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "approver role lookup", err)
	}
	if !EligibleLiquidationRoles[approverRole] {
		return nil, excerrors.New("UNAUTHORIZED_ROLE",
			"approver must hold Risk Manager or Super Admin")
	}

	// --- Payload validation ---
	accountID, present, err := parseFlexID(req.AccountID, "account_id")
	if err != nil {
		return nil, excerrors.New("INVALID_REQUEST", err.Error())
	}
	if !present {
		return nil, excerrors.New("INVALID_REQUEST", "account_id is required")
	}
	instrumentID, hasInstrument, err := parseFlexID(req.InstrumentID, "instrument_id")
	if err != nil {
		return nil, excerrors.New("INVALID_REQUEST", err.Error())
	}
	if strings.TrimSpace(req.Reason) == "" {
		return nil, excerrors.New("INVALID_REQUEST", "reason is required")
	}
	if req.OverrideAuction && len(strings.TrimSpace(req.Reason)) < 10 {
		return nil, excerrors.New("INVALID_REQUEST",
			"override_auction requires an explicit justification in reason")
	}

	// --- Transactional record + audit (single tx via the store seam) ---
	createdAt := time.Now().UTC()
	var instPtr *int64
	if hasInstrument {
		v := instrumentID
		instPtr = &v
	}
	recID, auditSeq, positions, err := s.store.RecordLiquidation(ctx, recordParams{
		AccountID: accountID, InstrumentID: instPtr,
		Reason: req.Reason, OverrideAuction: req.OverrideAuction,
		InitiatedBy: actor.UserID, ApprovedBy: approverID,
		ClientIP: actor.ClientIP,
	})
	if err != nil {
		return nil, err
	}

	// --- WAL event emit (post-commit; Phase-19 machinery consumes) ---
	ev := ManualLiquidationEvent{
		Event: "MANUAL_LIQUIDATION", Source: "MANUAL",
		LiquidationID: recID, AccountID: accountID, InstrumentID: instPtr,
		OverrideAuction: req.OverrideAuction, Reason: req.Reason,
		Positions:   positions,
		InitiatedBy: actor.UserID, ApprovedBy: approverID,
		TsMs: time.Now().UnixMilli(),
	}
	if err := s.sink.EmitManualLiquidation(ctx, ev); err != nil {
		// The record is committed and audited; mark it so operators can
		// see dispatch never reached the machinery (a retry by the admin
		// creates a fresh, separately-approved record — dual control is
		// not replayable).
		s.store.MarkEmitFailed(ctx, recID)
		return nil, excerrors.Wrap("SERVICE_DEGRADED",
			fmt.Sprintf("liquidation %d recorded but event emit failed", recID), err)
	}

	return &LiquidationResult{
		LiquidationID:   recID,
		AccountID:       accountID,
		InstrumentID:    instPtr,
		OverrideAuction: req.OverrideAuction,
		Source:          "MANUAL",
		Status:          "DISPATCHED",
		EstimatedFills:  positions,
		Approvals: map[string]any{
			"initiated_by": actor.UserID,
			"approved_by":  approverID,
			"roles":        []string{role, approverRole},
		},
		AuditSeq:  auditSeq,
		CreatedAt: createdAt,
	}, nil
}

// ManualLiquidationHandler is the HTTP surface for the service.
func ManualLiquidationHandler(svc *ManualLiquidationService, trustProxy bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims := auth.ClaimsFrom(r.Context())
		if claims == nil {
			WriteError(w, "UNAUTHORIZED", "authentication required",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		actorID, err := strconv.ParseInt(claims.Subject, 10, 64)
		if err != nil || actorID <= 0 {
			WriteError(w, "UNAUTHORIZED", "admin identity unresolvable",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		var req ManualLiquidationRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			WriteError(w, "INVALID_REQUEST", "malformed body",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		res, err := svc.Execute(r.Context(), AdminActor{
			UserID:   actorID,
			ClientIP: middleware.ClientIP(r, trustProxy),
		}, req)
		if err != nil {
			var e *excerrors.Error
			code, msg := "INTERNAL_ERROR", "internal error"
			if errors.As(err, &e) {
				code, msg = e.Code, e.Message
			}
			WriteError(w, code, msg, gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		WriteJSON(w, http.StatusOK, res)
	}
}
