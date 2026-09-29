// Compliance hold workflow — Phase-14 Task 14.3.10.
//
// A compliance hold keeps the account FROZEN (orders mass-cancelled on
// placement; withdrawals and new order entry reject ACCOUNT_FROZEN via
// the existing status gates) while preserving every position — a hold
// is a legal freeze, never a liquidation.
//
// PlaceHold is the stable Phase-21 seam: sanctions/PEP screening hits
// (Task 21.3.x) call it with a machine trigger_source; the manual
// Compliance Officer path is the only caller today.
//
// Dispositions (officer review dashboard):
//
//	release             — clear the false positive, restore ACTIVE
//	                      (dual-controlled: distinct approver, mirroring
//	                      the freeze/unfreeze four-eyes rule — a released
//	                      account stays FROZEN when other OPEN holds stand);
//	escalate SAR        — mark ESCALATED_SAR only; the actual STR/SAR
//	                      filing is Phase-21 Task 21.3.3 (seam: the hold
//	                      row is the filing source record);
//	escalate closure    — mark ESCALATED_CLOSURE and submit the Task
//	                      14.3.9 forced-closure dual-control request.
//
// SLA: 4h for high-confidence sanctions hits, 24h for everything else;
// SweepSLA marks sla_breached and raises an ops alert — the dashboard
// surfaces the breach until disposition. Every action writes
// admin_audit_log.
package compliance

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	excerrors "exchange/pkg/errors"
)

// defaultHoldID mints the "chold_<28urlsafe>" public id when no custom
// generator is wired (test hook).
func defaultHoldID() (string, error) {
	raw := make([]byte, 21)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return "chold_" + base64.RawURLEncoding.EncodeToString(raw), nil
}

// Hold trigger sources (migration 215 CHECK constraint mirror).
const (
	HoldTriggerManual          = "MANUAL"
	HoldTriggerSanctionsHit    = "SANCTIONS_HIT"    // Phase-21 caller
	HoldTriggerPEPMatch        = "PEP_MATCH"        // Phase-21 caller
	HoldTriggerUnusualActivity = "UNUSUAL_ACTIVITY" // Phase-21 caller
)

// Hold statuses.
const (
	HoldStatusOpen             = "OPEN"
	HoldStatusReleased         = "RELEASED"
	HoldStatusEscalatedSAR     = "ESCALATED_SAR"
	HoldStatusEscalatedClosure = "ESCALATED_CLOSURE"
)

// SLA constants — spec §8.7 / Task 14.3.10 review windows.
const (
	HoldSLAHoursSanctions = 4
	HoldSLAHoursDefault   = 24
)

// RestingCanceller is the order-pipeline seam for the placement-time
// mass cancel (orders.Dispatcher.MassCancel adapter at composition —
// compliance never touches the matching engine directly).
type RestingCanceller interface {
	CancelResting(ctx context.Context, accountID int64,
		reason string) (cancelled int, err error)
}

// HoldAlerter raises the P1 ops alert for partial placement failures
// and SLA breaches (funding_ops_alerts + pager adapter at composition).
type HoldAlerter interface {
	RaiseHold(ctx context.Context, a HoldAlert) error
}

// HoldAlert mirrors the FreezeAlert ops payload shape.
type HoldAlert struct {
	Severity  string            `json:"severity"` // "P1"
	Code      string            `json:"code"`
	AccountID int64             `json:"account_id"`
	Summary   string            `json:"summary"`
	Details   map[string]string `json:"details,omitempty"`
}

// ClosureEscalation submits the forced-closure dual-control request for
// an escalate-to-closure disposition (admin.DualControlService.Submit
// adapter at composition — the second officer's approval executes the
// Task 14.3.9 pipeline).
type ClosureEscalation interface {
	RequestForcedClosure(ctx context.Context, accountID int64,
		reason string, requestedBy int64) (requestID int64, err error)
}

// HoldRoleResolver resolves a user's admin role for the maker-checker
// gates (admin.AdminRoleResolver-compatible signature).
type HoldRoleResolver func(ctx context.Context, userID int64) (string, error)

// HoldUserNotifier emits the owner-facing security notification,
// best-effort (notifications.Service.Notify adapter).
type HoldUserNotifier func(ctx context.Context, userID int64,
	event string, payload map[string]any)

// ---------------------------------------------------------------------------
// Types
// ---------------------------------------------------------------------------

// PlaceHoldRequest is the Phase-21-ready placement input.
type PlaceHoldRequest struct {
	AccountID      int64
	Trigger        string // MANUAL now; SANCTIONS_HIT/PEP_MATCH/UNUSUAL_ACTIVITY from Phase-21
	Reason         string
	EvidenceRef    string // screening hit / case / document reference
	SLAHours       int    // 0 → auto: 4h sanctions, 24h otherwise
	HighConfidence bool   // sanctions-hit confidence flag → 4h SLA
	PlacedBy       int64  // officer user id; 0 for machine triggers (owner id substituted for the freeze event)
}

// Hold is one compliance_holds row.
type Hold struct {
	HoldID        string     `json:"hold_id"`
	AccountID     int64      `json:"account_id"`
	Trigger       string     `json:"trigger_source"`
	Reason        string     `json:"reason"`
	EvidenceRef   string     `json:"evidence_ref,omitempty"`
	Status        string     `json:"status"`
	SLADeadline   time.Time  `json:"sla_deadline"`
	SLABreached   bool       `json:"sla_breached"`
	PlacedBy      int64      `json:"placed_by"`
	PlacedAt      time.Time  `json:"placed_at"`
	ResolvedAt    *time.Time `json:"resolved_at,omitempty"`
	ResolvedBy    *int64     `json:"resolved_by,omitempty"`
	Resolution    string     `json:"resolution,omitempty"`
	AccountStatus string     `json:"account_status"` // joined read for the dashboard
}

// HoldService owns the workflow; every mutator is one transaction:
// hold row + accounts.status + account_freeze_events + admin_audit_log.
type HoldService struct {
	pool      *pgxpool.Pool
	canceller RestingCanceller
	alerter   HoldAlerter
	closure   ClosureEscalation
	resolver  HoldRoleResolver
	notify    HoldUserNotifier
	newID     func() (string, error)
	now       func() time.Time
}

func NewHoldService(pool *pgxpool.Pool, canceller RestingCanceller,
	alerter HoldAlerter, closure ClosureEscalation,
	resolver HoldRoleResolver, notify HoldUserNotifier,
	newID func() (string, error)) *HoldService {
	if newID == nil {
		newID = defaultHoldID
	}
	return &HoldService{pool: pool, canceller: canceller, alerter: alerter,
		closure: closure, resolver: resolver, notify: notify,
		newID: newID, now: time.Now}
}

// ---------------------------------------------------------------------------
// Placement — the Phase-21 seam
// ---------------------------------------------------------------------------

// PlaceHold freezes the account under a durable hold record. The
// transaction (hold row + FROZEN + freeze event + audit) commits
// first — a later cancel failure degrades to a P1 partial-failure
// alert, never an unfrozen account with cancelled orders missing.
func (s *HoldService) PlaceHold(ctx context.Context,
	req PlaceHoldRequest) (*Hold, error) {
	if req.Reason == "" {
		return nil, excerrors.New("INVALID_REQUEST", "reason is required")
	}
	switch req.Trigger {
	case HoldTriggerManual, HoldTriggerSanctionsHit,
		HoldTriggerPEPMatch, HoldTriggerUnusualActivity:
	default:
		return nil, excerrors.New("INVALID_REQUEST",
			"unknown trigger_source "+req.Trigger)
	}

	slaHours := req.SLAHours
	if slaHours <= 0 {
		slaHours = HoldSLAHoursDefault
		if req.Trigger == HoldTriggerSanctionsHit && req.HighConfidence {
			slaHours = HoldSLAHoursSanctions
		}
	}
	holdID, err := s.newID()
	if err != nil {
		return nil, excerrors.New("INTERNAL_ERROR",
			"hold id generation: "+err.Error())
	}
	deadline := s.now().UTC().Add(time.Duration(slaHours) * time.Hour)

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, excerrors.New("INTERNAL_ERROR",
			"begin tx: "+err.Error())
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var status string
	var owner int64
	err = tx.QueryRow(ctx,
		`SELECT status, user_id FROM accounts WHERE id = $1 FOR UPDATE`,
		req.AccountID).Scan(&status, &owner)
	if err == pgx.ErrNoRows {
		return nil, excerrors.New("NOT_FOUND", "account not found")
	}
	if err != nil {
		return nil, excerrors.New("INTERNAL_ERROR",
			"account lock: "+err.Error())
	}
	if status == "CLOSED" {
		return nil, excerrors.New("INVALID_REQUEST",
			"cannot hold a closed account")
	}
	// A hold on an already-FROZEN account is legal — multiple holds may
	// stack; the freeze transition is idempotent via the event stream.
	if status != "FROZEN" {
		if _, err := tx.Exec(ctx,
			`UPDATE accounts SET status='FROZEN', updated_at=now()
			  WHERE id = $1`, req.AccountID); err != nil {
			return nil, excerrors.New("INTERNAL_ERROR",
				"freeze transition: "+err.Error())
		}
	}
	placedBy := req.PlacedBy
	if placedBy == 0 {
		placedBy = owner // machine trigger attribution convention
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO compliance_holds
		     (hold_id, account_id, trigger_source, reason, evidence_ref,
		      status, sla_deadline, placed_by)
		 VALUES ($1,$2,$3,$4,$5,'OPEN',$6,$7)`,
		holdID, req.AccountID, req.Trigger, req.Reason,
		nullStr(req.EvidenceRef), deadline, placedBy); err != nil {
		return nil, excerrors.New("INTERNAL_ERROR",
			"hold insert: "+err.Error())
	}
	meta, _ := json.Marshal(map[string]any{
		"source": "compliance_hold", "hold_id": holdID,
		"trigger": req.Trigger, "sla_hours": slaHours,
		"evidence_ref": req.EvidenceRef,
	})
	if status != "FROZEN" {
		if _, err := tx.Exec(ctx,
			`INSERT INTO account_freeze_events
			     (account_id, action, reason, initiated_by, approved_by,
			      prev_status, new_status, metadata)
			 VALUES ($1,'FREEZE','COMPLIANCE_HOLD',$2,$2,$3,'FROZEN',$4)`,
			req.AccountID, placedBy, status, meta); err != nil {
			return nil, excerrors.New("INTERNAL_ERROR",
				"freeze event: "+err.Error())
		}
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO admin_audit_log
		     (admin_user_id, action, target_type, target_id, after_state)
		 VALUES ($1,'compliance.hold_place','account',$2,$3)`,
		placedBy, req.AccountID, meta); err != nil {
		return nil, excerrors.New("INTERNAL_ERROR",
			"audit insert: "+err.Error())
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, excerrors.New("INTERNAL_ERROR",
			"commit hold: "+err.Error())
	}

	// Post-commit: cancel every resting order (the freeze gate already
	// blocks new entry — cancels drain the book). ≤3 attempts then a
	// P1 alert — the legal hold stands regardless.
	if s.canceller != nil {
		var last error
		for i := 0; i < 3 && ctx.Err() == nil; i++ {
			_, last = s.canceller.CancelResting(ctx, req.AccountID,
				"COMPLIANCE_HOLD")
			if last == nil {
				break
			}
		}
		if last != nil {
			s.raise(ctx, HoldAlert{
				Severity: "P1", Code: "HOLD_CANCEL_FAILED",
				AccountID: req.AccountID,
				Summary: fmt.Sprintf("compliance hold %s placed but "+
					"mass-cancel failed after 3 attempts — resting "+
					"orders may remain: %v", holdID, last),
			})
		}
	}
	if s.notify != nil {
		s.notify(ctx, owner, "security_alert", map[string]any{
			"event":      "compliance.hold_placed",
			"account_id": req.AccountID,
			"hold_id":    holdID,
			"message":    "account placed under compliance review — trading and withdrawals paused",
		})
	}
	return &Hold{
		HoldID: holdID, AccountID: req.AccountID, Trigger: req.Trigger,
		Reason: req.Reason, EvidenceRef: req.EvidenceRef,
		Status: HoldStatusOpen, SLADeadline: deadline,
		PlacedBy: placedBy, PlacedAt: s.now().UTC(),
		AccountStatus: "FROZEN",
	}, nil
}

// ---------------------------------------------------------------------------
// Officer review — list / release / escalate
// ---------------------------------------------------------------------------

// ListHolds feeds the officer dashboard — OPEN holds first, joined to
// the live account status for the frozen-account review view.
func (s *HoldService) ListHolds(ctx context.Context, status string,
	limit int) ([]Hold, error) {
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	q := `SELECT h.hold_id, h.account_id, h.trigger_source, h.reason,
	             COALESCE(h.evidence_ref,''), h.status, h.sla_deadline,
	             h.sla_breached, h.placed_by, h.placed_at, h.resolved_at,
	             h.resolved_by, COALESCE(h.resolution,''), a.status
	        FROM compliance_holds h JOIN accounts a ON a.id = h.account_id`
	args := []any{}
	if status != "" {
		q += ` WHERE h.status = $1`
		args = append(args, status)
	}
	q += ` ORDER BY h.sla_deadline ASC LIMIT ` + fmt.Sprint(limit)
	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, excerrors.New("INTERNAL_ERROR",
			"hold list: "+err.Error())
	}
	defer rows.Close()
	var out []Hold
	for rows.Next() {
		var h Hold
		if err := rows.Scan(&h.HoldID, &h.AccountID, &h.Trigger,
			&h.Reason, &h.EvidenceRef, &h.Status, &h.SLADeadline,
			&h.SLABreached, &h.PlacedBy, &h.PlacedAt, &h.ResolvedAt,
			&h.ResolvedBy, &h.Resolution, &h.AccountStatus); err != nil {
			return nil, excerrors.New("INTERNAL_ERROR",
				"hold scan: "+err.Error())
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// Release clears the hold and restores ACTIVE when no other OPEN hold
// stands. Four-eyes: actor and approver must both resolve to a
// Compliance-Officer-or-above role and be distinct users (same
// convention as FreezeService.Unfreeze).
func (s *HoldService) Release(ctx context.Context, holdID string,
	actor, approver int64, reason string) (*Hold, error) {
	if reason == "" {
		return nil, excerrors.New("INVALID_REQUEST",
			"release reason is required")
	}
	if err := s.checkRole(ctx, actor); err != nil {
		return nil, err
	}
	if approver == 0 || approver == actor {
		return nil, excerrors.New("DUAL_CONTROL_REQUIRED",
			"a distinct approver is required to release a hold")
	}
	if err := s.checkRole(ctx, approver); err != nil {
		return nil, err
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, excerrors.New("INTERNAL_ERROR", "begin tx: "+err.Error())
	}
	defer func() { _ = tx.Rollback(ctx) }()

	h, err := s.lockHold(ctx, tx, holdID)
	if err != nil {
		return nil, err
	}
	if h.Status != HoldStatusOpen {
		return nil, excerrors.New("INVALID_REQUEST",
			"hold "+holdID+" is "+h.Status+" — only OPEN holds release")
	}

	// Other OPEN holds keep the account frozen — release only restores
	// ACTIVE when this was the last standing hold.
	var others int64
	if err := tx.QueryRow(ctx,
		`SELECT count(*) FROM compliance_holds
		  WHERE account_id = $1 AND status = 'OPEN' AND hold_id <> $2`,
		h.AccountID, holdID).Scan(&others); err != nil {
		return nil, excerrors.New("INTERNAL_ERROR",
			"sibling hold count: "+err.Error())
	}
	resolution := reason
	if others > 0 {
		resolution += fmt.Sprintf(
			" (account stays FROZEN — %d other OPEN hold(s))", others)
	}
	if _, err := tx.Exec(ctx,
		`UPDATE compliance_holds
		    SET status='RELEASED', resolved_at=now(), resolved_by=$2,
		        resolution=$3, updated_at=now()
		  WHERE hold_id = $1`, holdID, actor, resolution); err != nil {
		return nil, excerrors.New("INTERNAL_ERROR",
			"hold release: "+err.Error())
	}
	if others == 0 {
		if _, err := tx.Exec(ctx,
			`UPDATE accounts SET status='ACTIVE', updated_at=now()
			  WHERE id = $1 AND status = 'FROZEN'`, h.AccountID); err != nil {
			return nil, excerrors.New("INTERNAL_ERROR",
				"unfreeze transition: "+err.Error())
		}
		meta, _ := json.Marshal(map[string]any{
			"source": "compliance_hold_release", "hold_id": holdID,
		})
		if _, err := tx.Exec(ctx,
			`INSERT INTO account_freeze_events
			     (account_id, action, reason, initiated_by, approved_by,
			      prev_status, new_status, metadata)
			 VALUES ($1,'UNFREEZE','COMPLIANCE_HOLD_RELEASE',$2,$3,
			         'FROZEN','ACTIVE',$4)`,
			h.AccountID, actor, approver, meta); err != nil {
			return nil, excerrors.New("INTERNAL_ERROR",
				"unfreeze event: "+err.Error())
		}
	}
	meta, _ := json.Marshal(map[string]any{
		"hold_id": holdID, "account_id": h.AccountID,
		"approver_id": approver, "resolution": resolution,
	})
	if _, err := tx.Exec(ctx,
		`INSERT INTO admin_audit_log
		     (admin_user_id, action, target_type, target_id, after_state)
		 VALUES ($1,'compliance.hold_release','account',$2,$3)`,
		actor, h.AccountID, meta); err != nil {
		return nil, excerrors.New("INTERNAL_ERROR",
			"audit insert: "+err.Error())
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, excerrors.New("INTERNAL_ERROR",
			"commit release: "+err.Error())
	}
	h.Status = HoldStatusReleased
	now := s.now().UTC()
	h.ResolvedAt, h.ResolvedBy, h.Resolution = &now, &actor, resolution
	if others == 0 {
		h.AccountStatus = "ACTIVE"
	}
	return h, nil
}

// EscalateSAR records the escalation decision only — the actual
// SAR/STR filing ships with Phase-21 Task 21.3.3 against this row.
func (s *HoldService) EscalateSAR(ctx context.Context, holdID string,
	actor int64, detail string) (*Hold, error) {
	if detail == "" {
		return nil, excerrors.New("INVALID_REQUEST",
			"escalation detail is required")
	}
	if err := s.checkRole(ctx, actor); err != nil {
		return nil, err
	}
	return s.resolveEscalation(ctx, holdID, actor,
		HoldStatusEscalatedSAR, "COMPLIANCE_HOLD_ESCALATE_SAR",
		"escalated to SAR — Phase-21 Task 21.3.3 files the report: "+detail)
}

// EscalateToClosure records the escalation and submits the forced-
// closure dual-control request (maker = the escalating officer; a
// distinct approver executes the Task 14.3.9 pipeline).
func (s *HoldService) EscalateToClosure(ctx context.Context, holdID string,
	actor int64, reason string) (*Hold, int64, error) {
	if reason == "" {
		return nil, 0, excerrors.New("INVALID_REQUEST",
			"closure reason is required")
	}
	if err := s.checkRole(ctx, actor); err != nil {
		return nil, 0, err
	}
	if s.closure == nil {
		return nil, 0, excerrors.New("SERVICE_DEGRADED",
			"forced-closure escalation path unavailable")
	}
	h, err := s.resolveEscalation(ctx, holdID, actor,
		HoldStatusEscalatedClosure, "COMPLIANCE_HOLD_ESCALATE_CLOSURE",
		"escalated to closure: "+reason)
	if err != nil {
		return nil, 0, err
	}
	reqID, err := s.closure.RequestForcedClosure(ctx, h.AccountID,
		"compliance hold "+holdID+" escalation: "+reason, actor)
	if err != nil {
		return nil, 0, excerrors.New("SERVICE_DEGRADED",
			"hold escalated but closure request submission failed: "+err.Error())
	}
	return h, reqID, nil
}

// resolveEscalation performs the OPEN→escalated transition + audit row.
func (s *HoldService) resolveEscalation(ctx context.Context, holdID string,
	actor int64, toStatus, action, resolution string) (*Hold, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, excerrors.New("INTERNAL_ERROR", "begin tx: "+err.Error())
	}
	defer func() { _ = tx.Rollback(ctx) }()
	h, err := s.lockHold(ctx, tx, holdID)
	if err != nil {
		return nil, err
	}
	if h.Status != HoldStatusOpen {
		return nil, excerrors.New("INVALID_REQUEST",
			"hold "+holdID+" is "+h.Status+" — only OPEN holds escalate")
	}
	if _, err := tx.Exec(ctx,
		`UPDATE compliance_holds
		    SET status=$2, resolved_at=now(), resolved_by=$3,
		        resolution=$4, updated_at=now()
		  WHERE hold_id = $1`, holdID, toStatus, actor, resolution); err != nil {
		return nil, excerrors.New("INTERNAL_ERROR",
			"hold escalation: "+err.Error())
	}
	meta, _ := json.Marshal(map[string]any{
		"hold_id": holdID, "account_id": h.AccountID,
		"to": toStatus, "resolution": resolution,
	})
	if _, err := tx.Exec(ctx,
		`INSERT INTO admin_audit_log
		     (admin_user_id, action, target_type, target_id, after_state)
		 VALUES ($1,$2,'account',$3,$4)`,
		actor, action, h.AccountID, meta); err != nil {
		return nil, excerrors.New("INTERNAL_ERROR",
			"audit insert: "+err.Error())
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, excerrors.New("INTERNAL_ERROR",
			"commit escalation: "+err.Error())
	}
	h.Status = toStatus
	now := s.now().UTC()
	h.ResolvedAt, h.ResolvedBy, h.Resolution = &now, &actor, resolution
	return h, nil
}

// SweepSLA marks breached OPEN holds and pages ops — the 60s ticker in
// the gateway sweeps it; the dashboard reads sla_breached rows until
// disposition. Alerts are at-most-once per hold (flag set first).
func (s *HoldService) SweepSLA(ctx context.Context, limit int) (int, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.pool.Query(ctx,
		`UPDATE compliance_holds
		    SET sla_breached = true, updated_at = now()
		  WHERE id IN (
		      SELECT id FROM compliance_holds
		      WHERE status = 'OPEN' AND NOT sla_breached
		        AND sla_deadline < now()
		      ORDER BY sla_deadline LIMIT $1)
		  RETURNING hold_id, account_id, trigger_source, sla_deadline`, limit)
	if err != nil {
		return 0, excerrors.New("INTERNAL_ERROR",
			"SLA sweep: "+err.Error())
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var holdID, trigger string
		var accountID int64
		var deadline time.Time
		if err := rows.Scan(&holdID, &accountID, &trigger, &deadline); err != nil {
			continue
		}
		n++
		s.raise(ctx, HoldAlert{
			Severity: "P1", Code: "HOLD_SLA_BREACHED",
			AccountID: accountID,
			Summary: fmt.Sprintf("compliance hold %s breached SLA "+
				"(%s trigger, deadline %s) — officer disposition required",
				holdID, trigger, deadline.Format(time.RFC3339)),
		})
	}
	return n, rows.Err()
}

func (s *HoldService) lockHold(ctx context.Context, tx pgx.Tx,
	holdID string) (*Hold, error) {
	var h Hold
	var ev *string
	err := tx.QueryRow(ctx,
		`SELECT hold_id, account_id, trigger_source, reason, evidence_ref,
		        status, sla_deadline, sla_breached, placed_by, placed_at
		   FROM compliance_holds WHERE hold_id = $1 FOR UPDATE`,
		holdID).Scan(&h.HoldID, &h.AccountID, &h.Trigger, &h.Reason,
		&ev, &h.Status, &h.SLADeadline, &h.SLABreached,
		&h.PlacedBy, &h.PlacedAt)
	if err == pgx.ErrNoRows {
		return nil, excerrors.New("NOT_FOUND", "hold not found")
	}
	if err != nil {
		return nil, excerrors.New("INTERNAL_ERROR",
			"hold lock: "+err.Error())
	}
	if ev != nil {
		h.EvidenceRef = *ev
	}
	return &h, nil
}

// checkRole gates officer actions to Compliance Officer / Super Admin —
// the same accept-set FreezeService uses for freeze/unfreeze.
func (s *HoldService) checkRole(ctx context.Context, userID int64) error {
	if s.resolver == nil {
		return excerrors.New("INTERNAL_ERROR", "role resolver not wired")
	}
	role, err := s.resolver(ctx, userID)
	if err != nil {
		return excerrors.New("INTERNAL_ERROR", "role lookup: "+err.Error())
	}
	if role != "Compliance Officer" && role != "Super Admin" {
		return excerrors.New("UNAUTHORIZED_ROLE",
			"role "+role+" cannot action compliance holds")
	}
	return nil
}

func (s *HoldService) raise(ctx context.Context, a HoldAlert) {
	if s.alerter == nil {
		return
	}
	_ = s.alerter.RaiseHold(ctx, a)
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}
