// Phase-21 Task 21.3.24 — Employee Dealing & Insider-Information
// Controls (spec §14.10.1, §24 #327).
//
// Order-entry gate (the orders.DealingGate implementation — consulted
// at the gateway admission seam, never the matching engine):
//
//	non-employee account                    → admit
//	employee account + restricted window    → require APPROVED,
//	    covering the instrument/scope          unexpired pre-clearance
//	employee account + SENSITIVE_ROLES role → require APPROVED,
//	    (privileged-information set)           unexpired pre-clearance
//	otherwise                               → admit
//
// Staff accounts are excluded from liquidity/STP/rebate programs —
// ExcludedFromPrograms is the seam the funding/STP surfaces consult.
// Every staff fill lands in employee_trade_reviews for same-day
// officer review; a trade inside a restricted window escalates P1.
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

	"exchange/internal/admin"

	excerrors "exchange/pkg/errors"
)

// ErrPreclearanceRequired → EMPLOYEE_DEALING_PRECLEARANCE_REQUIRED
// (422, pre-registered in errs/codes.go).
var ErrPreclearanceRequired = excerrors.New(
	"EMPLOYEE_DEALING_PRECLEARANCE_REQUIRED",
	"employee order entry on this instrument requires an approved "+
		"pre-clearance (spec §14.10.1)")

// SensitiveRoles is the privileged-information employee_role set —
// staff who could see engine internals, ops schedules, surveillance
// state, LP flow or product roadmaps need pre-clearance on ANY
// instrument, not just restricted windows.
var SensitiveRoles = map[string]bool{
	"CORE_ENGINE":   true,
	"CORE_OPS":      true,
	"COMPLIANCE":    true,
	"LP_MANAGEMENT": true,
	"PRODUCT":       true,
}

// PreClearance is one pre_clearance_requests row.
type PreClearance struct {
	ID           int64      `json:"id"`
	RequestID    string     `json:"request_id"`
	AccountID    int64      `json:"account_id"`
	Instrument   string     `json:"instrument"` // '' → all covered instruments
	Side         string     `json:"side,omitempty"`
	NotionalCap  *string    `json:"notional_cap,omitempty"`
	Reason       string     `json:"reason"`
	Outcome      string     `json:"outcome"` // PENDING|APPROVED|DENIED|EXPIRED
	ExpiresAt    time.Time  `json:"expires_at"`
	RequestedBy  int64      `json:"requested_by"`
	DecidedBy    *int64     `json:"decided_by,omitempty"`
	DecidedAt    *time.Time `json:"decided_at,omitempty"`
	DecisionNote string     `json:"decision_note,omitempty"`
	FirstUsedAt  *time.Time `json:"first_used_at,omitempty"`
	CreatedAt    time.Time  `json:"created_at"`
}

// StaffTradeReview is one employee_trade_reviews row.
type StaffTradeReview struct {
	ID                int64      `json:"id"`
	AccountID         int64      `json:"account_id"`
	OrderID           *int64     `json:"order_id,omitempty"`
	TradeID           *int64     `json:"trade_id,omitempty"`
	Symbol            string     `json:"symbol"`
	Side              string     `json:"side,omitempty"`
	Quantity          *string    `json:"quantity,omitempty"`
	Price             *string    `json:"price,omitempty"`
	RestrictedEventID *string    `json:"restricted_event_id,omitempty"`
	Status            string     `json:"status"` // PENDING|REVIEWED|ESCALATED
	ReviewedBy        *int64     `json:"reviewed_by,omitempty"`
	ReviewedAt        *time.Time `json:"reviewed_at,omitempty"`
	ReviewNote        string     `json:"review_note,omitempty"`
	CreatedAt         time.Time  `json:"created_at"`
}

// EmployeeDealingAlerter raises the P1 staff-trade-in-window page.
type EmployeeDealingAlerter interface {
	RaiseHold(ctx context.Context, a HoldAlert) error
}

// EmployeeDealingService owns pre-clearance, the order-entry gate and
// the staff-trade review queue.
type EmployeeDealingService struct {
	pool     *pgxpool.Pool
	resolver HoldRoleResolver // role resolver (Compliance Officer gate)
	alerter  EmployeeDealingAlerter
	now      func() time.Time
	newID    func() (string, error)
}

// NewEmployeeDealingService binds dependencies.
func NewEmployeeDealingService(pool *pgxpool.Pool,
	resolver HoldRoleResolver,
	alerter EmployeeDealingAlerter) *EmployeeDealingService {
	return &EmployeeDealingService{pool: pool, resolver: resolver,
		alerter: alerter, now: time.Now, newID: defaultPreClearanceID}
}

func defaultPreClearanceID() (string, error) {
	raw := make([]byte, 19)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return "pcl_" + base64.RawURLEncoding.EncodeToString(raw), nil
}

// WithClock / WithIDSource are test hooks.
func (s *EmployeeDealingService) WithClock(f func() time.Time) *EmployeeDealingService {
	if f != nil {
		s.now = f
	}
	return s
}
func (s *EmployeeDealingService) WithIDSource(f func() (string, error)) *EmployeeDealingService {
	if f != nil {
		s.newID = f
	}
	return s
}

func (s *EmployeeDealingService) checkRole(ctx context.Context,
	userID int64) error {
	if s.resolver == nil {
		return excerrors.New("INTERNAL_ERROR", "role resolver not wired")
	}
	role, err := s.resolver(ctx, userID)
	if err != nil {
		return excerrors.New("INTERNAL_ERROR", "role lookup: "+err.Error())
	}
	if role != "Compliance Officer" && role != "Super Admin" {
		return excerrors.New("UNAUTHORIZED_ROLE",
			"role "+role+" cannot administer employee dealing")
	}
	return nil
}

// ---------------------------------------------------------------------------
// Order-entry gate — orders.DealingGate implementation
// ---------------------------------------------------------------------------

// AssertOrderEntry is consulted on every new-order admission for the
// account. Non-employee accounts pass untouched; employee accounts
// need an APPROVED, unexpired pre-clearance when the instrument sits
// inside an active blackout window covering them or when the
// employee's organisational role is in SensitiveRoles.
// Fail-closed: a store error rejects as INTERNAL_ERROR, never admits.
func (s *EmployeeDealingService) AssertOrderEntry(ctx context.Context,
	accountID int64, symbol string) error {
	if accountID <= 0 {
		return nil
	}
	var employee bool
	var role *string
	if err := s.pool.QueryRow(ctx,
		`SELECT employee_account, employee_role FROM accounts WHERE id = $1`,
		accountID).Scan(&employee, &role); err == pgx.ErrNoRows {
		return nil // not resolvable → other gates own the verdict
	} else if err != nil {
		return excerrors.New("INTERNAL_ERROR",
			"employee flag lookup: "+err.Error())
	}
	if !employee {
		return nil
	}
	needsClearance := false
	if role != nil && SensitiveRoles[*role] {
		needsClearance = true
	}
	if !needsClearance {
		if covered, err := s.inRestrictedWindow(ctx,
			accountID, derefStr(role), symbol, s.now().UTC()); err != nil {
			return err
		} else if covered {
			needsClearance = true
		}
	}
	if !needsClearance {
		return nil
	}
	return s.assertClearance(ctx, accountID, symbol)
}

// inRestrictedWindow reports whether an ACTIVE restricted list covers
// (account, employee_role, symbol) at instant t — the effective window
// is the stored window ± widen_minutes.
func (s *EmployeeDealingService) inRestrictedWindow(ctx context.Context,
	accountID int64, empRole, symbol string, t time.Time) (bool, error) {
	var n int
	err := s.pool.QueryRow(ctx, `
		SELECT count(*) FROM restricted_lists
		 WHERE status = 'ACTIVE'
		   AND $4 BETWEEN window_start - (widen_minutes || ' minutes')::interval
		              AND window_end   + (widen_minutes || ' minutes')::interval
		   AND (instruments = '{}' OR $3 = ANY(instruments))
		   AND (scope = 'ALL_EMPLOYEES'
		        OR (scope = 'ROLE'  AND scope_role = $2)
		        OR (scope = 'NAMED' AND $1 = ANY(named_accounts)))`,
		accountID, empRole, symbol, t).Scan(&n)
	if err != nil {
		return false, excerrors.New("INTERNAL_ERROR",
			"restricted window lookup: "+err.Error())
	}
	return n > 0, nil
}

// assertClearance demands an APPROVED, unexpired row covering the
// instrument (exact symbol or blanket ”). The first admission stamps
// first_used_at for the audit trail.
func (s *EmployeeDealingService) assertClearance(ctx context.Context,
	accountID int64, symbol string) error {
	var id int64
	err := s.pool.QueryRow(ctx, `
		SELECT id FROM pre_clearance_requests
		 WHERE account_id = $1
		   AND outcome = 'APPROVED'
		   AND expires_at > now()
		   AND (instrument = '' OR instrument = $2)
		 ORDER BY expires_at DESC LIMIT 1`,
		accountID, symbol).Scan(&id)
	if err == pgx.ErrNoRows {
		return ErrPreclearanceRequired
	}
	if err != nil {
		return excerrors.New("INTERNAL_ERROR",
			"pre-clearance lookup: "+err.Error())
	}
	_, _ = s.pool.Exec(ctx,
		`UPDATE pre_clearance_requests SET first_used_at = COALESCE(first_used_at, now())
		  WHERE id = $1`, id)
	return nil
}

// ---------------------------------------------------------------------------
// Pre-clearance lifecycle
// ---------------------------------------------------------------------------

// RequestClearance files a PENDING request — the employee self-files
// (requested_by = the account's owner). expires_at is the requested
// horizon, hard-capped at 24h (spec §14.10.1).
func (s *EmployeeDealingService) RequestClearance(ctx context.Context,
	accountID int64, userID int64, instrument, side, reason string,
	notionalCap *string, expiresAt time.Time) (*PreClearance, error) {
	if reason == "" {
		return nil, excerrors.New("INVALID_REQUEST",
			"pre-clearance requires a stated purpose")
	}
	if side != "" && side != "BUY" && side != "SELL" {
		return nil, excerrors.New("INVALID_REQUEST",
			"side must be BUY|SELL")
	}
	// The requester must own the account (or be an officer filing for
	// the desk — checked by the caller's route auth; here we bind
	// ownership fail-closed).
	var owner int64
	var employee bool
	if err := s.pool.QueryRow(ctx,
		`SELECT user_id, employee_account FROM accounts WHERE id = $1`,
		accountID).Scan(&owner, &employee); err == pgx.ErrNoRows {
		return nil, excerrors.New("NOT_FOUND", "account not found")
	} else if err != nil {
		return nil, excerrors.New("INTERNAL_ERROR",
			"account lookup: "+err.Error())
	}
	if !employee {
		return nil, excerrors.New("INVALID_REQUEST",
			"account is not flagged employee_account")
	}
	if owner != userID {
		return nil, excerrors.New("UNAUTHORIZED",
			"pre-clearance must be filed by the account owner")
	}
	maxExp := s.now().UTC().Add(24 * time.Hour)
	if expiresAt.IsZero() || expiresAt.After(maxExp) {
		expiresAt = maxExp
	}
	id, err := s.newID()
	if err != nil {
		return nil, excerrors.New("INTERNAL_ERROR", "request id: "+err.Error())
	}
	var p PreClearance
	err = s.pool.QueryRow(ctx, `
		INSERT INTO pre_clearance_requests
		    (request_id, account_id, instrument, side, notional_cap,
		     reason, expires_at, requested_by)
		VALUES ($1,$2,$3,NULLIF($4,''),$5,$6,$7,$8)
		RETURNING id, request_id, account_id, instrument,
		          COALESCE(side,''), notional_cap::text, reason, outcome,
		          expires_at, requested_by, decided_by, decided_at,
		          COALESCE(decision_note,''), first_used_at, created_at`,
		id, accountID, instrument, side, notionalCap, reason,
		expiresAt, userID).
		Scan(&p.ID, &p.RequestID, &p.AccountID, &p.Instrument, &p.Side,
			&p.NotionalCap, &p.Reason, &p.Outcome, &p.ExpiresAt,
			&p.RequestedBy, &p.DecidedBy, &p.DecidedAt, &p.DecisionNote,
			&p.FirstUsedAt, &p.CreatedAt)
	if err != nil {
		return nil, excerrors.New("INTERNAL_ERROR",
			"pre-clearance insert: "+err.Error())
	}
	return &p, nil
}

// RequestClearanceOfficer files a request on the desk's behalf —
// officer role already proved at the route + service gate; the
// requested_by stamp is the account owner (the trader the clearance
// covers), keeping the dual-control invariant that the decider must
// differ from the requester.
func (s *EmployeeDealingService) RequestClearanceOfficer(ctx context.Context,
	officerID, accountID int64, instrument, side, reason string,
	notionalCap *string, expiresAt time.Time) (*PreClearance, error) {
	if err := s.checkRole(ctx, officerID); err != nil {
		return nil, err
	}
	var owner int64
	var employee bool
	if err := s.pool.QueryRow(ctx,
		`SELECT user_id, employee_account FROM accounts WHERE id = $1`,
		accountID).Scan(&owner, &employee); err == pgx.ErrNoRows {
		return nil, excerrors.New("NOT_FOUND", "account not found")
	} else if err != nil {
		return nil, excerrors.New("INTERNAL_ERROR",
			"account lookup: "+err.Error())
	}
	if !employee {
		return nil, excerrors.New("INVALID_REQUEST",
			"account is not flagged employee_account")
	}
	maxExp := s.now().UTC().Add(24 * time.Hour)
	if expiresAt.IsZero() || expiresAt.After(maxExp) {
		expiresAt = maxExp
	}
	id, err := s.newID()
	if err != nil {
		return nil, excerrors.New("INTERNAL_ERROR", "request id: "+err.Error())
	}
	var p PreClearance
	err = s.pool.QueryRow(ctx, `
		INSERT INTO pre_clearance_requests
		    (request_id, account_id, instrument, side, notional_cap,
		     reason, expires_at, requested_by)
		VALUES ($1,$2,$3,NULLIF($4,''),$5,$6,$7,$8)
		RETURNING id, request_id, account_id, instrument,
		          COALESCE(side,''), notional_cap::text, reason, outcome,
		          expires_at, requested_by, decided_by, decided_at,
		          COALESCE(decision_note,''), first_used_at, created_at`,
		id, accountID, instrument, side, notionalCap, reason,
		expiresAt, owner).
		Scan(&p.ID, &p.RequestID, &p.AccountID, &p.Instrument, &p.Side,
			&p.NotionalCap, &p.Reason, &p.Outcome, &p.ExpiresAt,
			&p.RequestedBy, &p.DecidedBy, &p.DecidedAt, &p.DecisionNote,
			&p.FirstUsedAt, &p.CreatedAt)
	if err != nil {
		return nil, excerrors.New("INTERNAL_ERROR",
			"pre-clearance insert: "+err.Error())
	}
	_, _, _ = admin.LogAuto(ctx, s.pool, admin.AuditEntry{
		AdminUserID: officerID, Action: "employee_dealing.request",
		TargetType: "pre_clearance_request", TargetID: &p.ID,
		AfterState: map[string]any{
			"request_id": p.RequestID, "account_id": accountID,
			"instrument": instrument, "filed_for": owner,
		},
	})
	return &p, nil
}

// ResolveRequestID maps the public request_id to the row pk.
func (s *EmployeeDealingService) ResolveRequestID(ctx context.Context,
	requestID string) (int64, error) {
	var id int64
	err := s.pool.QueryRow(ctx,
		`SELECT id FROM pre_clearance_requests WHERE request_id = $1`,
		requestID).Scan(&id)
	if err == pgx.ErrNoRows {
		return 0, excerrors.New("NOT_FOUND", "pre-clearance request not found")
	}
	if err != nil {
		return 0, excerrors.New("INTERNAL_ERROR", "request lookup: "+err.Error())
	}
	return id, nil
}

// DecideClearance is the independent-controller decision —
// decided_by must differ from requested_by (separation of duties;
// the compliance officer is independent of the requester). Decisions
// are append-only (migration-079 trigger); expiry is lazy via
// SweepClearanceExpiry.
func (s *EmployeeDealingService) DecideClearance(ctx context.Context,
	actorID, requestPK int64, approve bool, note string) (*PreClearance, error) {
	if err := s.checkRole(ctx, actorID); err != nil {
		return nil, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, excerrors.New("INTERNAL_ERROR", "decide tx: "+err.Error())
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var requestedBy int64
	var outcome string
	var expires time.Time
	if err := tx.QueryRow(ctx, `
		SELECT requested_by, outcome, expires_at
		  FROM pre_clearance_requests WHERE id = $1 FOR UPDATE`,
		requestPK).Scan(&requestedBy, &outcome, &expires); err == pgx.ErrNoRows {
		return nil, excerrors.New("NOT_FOUND", "pre-clearance not found")
	} else if err != nil {
		return nil, excerrors.New("INTERNAL_ERROR", "request lock: "+err.Error())
	}
	if outcome != "PENDING" {
		return nil, excerrors.New("INVALID_REQUEST",
			"request already "+outcome+" — decisions are final")
	}
	if requestedBy == actorID {
		return nil, excerrors.New("DUAL_CONTROL_REQUIRED",
			"pre-clearance must be decided by an independent controller")
	}
	to := "DENIED"
	if approve {
		to = "APPROVED"
	}
	now := s.now().UTC()
	var p PreClearance
	err = tx.QueryRow(ctx, `
		UPDATE pre_clearance_requests
		   SET outcome = $2, decided_by = $3, decided_at = $4,
		       decision_note = $5, updated_at = now()
		 WHERE id = $1
		RETURNING id, request_id, account_id, instrument,
		          COALESCE(side,''), notional_cap::text, reason, outcome,
		          expires_at, requested_by, decided_by, decided_at,
		          COALESCE(decision_note,''), first_used_at, created_at`,
		requestPK, to, actorID, now, note).
		Scan(&p.ID, &p.RequestID, &p.AccountID, &p.Instrument, &p.Side,
			&p.NotionalCap, &p.Reason, &p.Outcome, &p.ExpiresAt,
			&p.RequestedBy, &p.DecidedBy, &p.DecidedAt, &p.DecisionNote,
			&p.FirstUsedAt, &p.CreatedAt)
	if err != nil {
		return nil, excerrors.New("INTERNAL_ERROR", "decide: "+err.Error())
	}
	if _, _, err := admin.Log(ctx, tx, admin.AuditEntry{
		AdminUserID: actorID, Action: "employee_dealing.decide",
		TargetType: "pre_clearance_request", TargetID: &requestPK,
		AfterState: map[string]any{
			"request_id": p.RequestID, "outcome": to, "note": note,
		},
	}); err != nil {
		return nil, excerrors.New("INTERNAL_ERROR", "decide audit: "+err.Error())
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, excerrors.New("INTERNAL_ERROR", "decide commit: "+err.Error())
	}
	return &p, nil
}

// ListClearances reads requests for the dashboard — officer view
// (accountID 0 = all, else filtered).
func (s *EmployeeDealingService) ListClearances(ctx context.Context,
	accountID int64, outcome string, limit int) ([]PreClearance, error) {
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	q := `SELECT id, request_id, account_id, instrument,
	             COALESCE(side,''),
	             notional_cap::text, reason, outcome, expires_at,
	             requested_by, decided_by, decided_at,
	             COALESCE(decision_note,''),
	             first_used_at, created_at
	        FROM pre_clearance_requests WHERE 1=1`
	args := []any{}
	if accountID > 0 {
		args = append(args, accountID)
		q += fmt.Sprintf(" AND account_id = $%d", len(args))
	}
	if outcome != "" {
		args = append(args, outcome)
		q += fmt.Sprintf(" AND outcome = $%d", len(args))
	}
	q += fmt.Sprintf(" ORDER BY id DESC LIMIT %d", limit)
	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, excerrors.New("INTERNAL_ERROR", "clearance list: "+err.Error())
	}
	defer rows.Close()
	var out []PreClearance
	for rows.Next() {
		var p PreClearance
		if err := rows.Scan(&p.ID, &p.RequestID, &p.AccountID,
			&p.Instrument, &p.Side, &p.NotionalCap, &p.Reason,
			&p.Outcome, &p.ExpiresAt, &p.RequestedBy, &p.DecidedBy,
			&p.DecidedAt, &p.DecisionNote, &p.FirstUsedAt,
			&p.CreatedAt); err != nil {
			return nil, excerrors.New("INTERNAL_ERROR",
				"clearance row: "+err.Error())
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// SweepClearanceExpiry flips PENDING/APPROVED rows past expiry to
// EXPIRED — the lazy-expiry complement to the admission check.
func (s *EmployeeDealingService) SweepClearanceExpiry(ctx context.Context) (int, error) {
	tag, err := s.pool.Exec(ctx, `
		UPDATE pre_clearance_requests SET outcome='EXPIRED', updated_at=now()
		 WHERE outcome IN ('PENDING','APPROVED') AND expires_at <= now()`)
	if err != nil {
		return 0, excerrors.New("INTERNAL_ERROR",
			"clearance expiry: "+err.Error())
	}
	return int(tag.RowsAffected()), nil
}

// ---------------------------------------------------------------------------
// Staff-trade review queue
// ---------------------------------------------------------------------------

// RecordStaffTrade is the fill-observer seam — every fill on an
// employee_account lands here. When the fill lands inside an active
// restricted window for the account, restricted_event_id is stamped
// and the row escalates P1 (insider-window trading is the §14.10.1
// red flag).
func (s *EmployeeDealingService) RecordStaffTrade(ctx context.Context,
	accountID int64, orderID, tradeID *int64, symbol, side string,
	quantity, price *string) (*StaffTradeReview, error) {
	var eventID *string
	_ = s.pool.QueryRow(ctx, `
		SELECT event_id FROM restricted_lists
		 WHERE status = 'ACTIVE'
		   AND now() BETWEEN window_start - (widen_minutes || ' minutes')::interval
		                 AND window_end   + (widen_minutes || ' minutes')::interval
		   AND (instruments = '{}' OR $2 = ANY(instruments))
		   AND (scope = 'ALL_EMPLOYEES'
		        OR (scope = 'NAMED' AND $1 = ANY(named_accounts)))
		 ORDER BY id DESC LIMIT 1`,
		accountID, symbol).Scan(&eventID)

	status := "PENDING"
	if eventID != nil {
		status = "ESCALATED"
	}
	var r StaffTradeReview
	err := s.pool.QueryRow(ctx, `
		INSERT INTO employee_trade_reviews
		    (account_id, order_id, trade_id, symbol, side, quantity,
		     price, restricted_event_id, status)
		VALUES ($1,$2,$3,$4,$5,$6::decimal,$7::decimal,$8,$9)
		RETURNING id, account_id, order_id, trade_id, symbol,
		          COALESCE(side,''),
		          quantity::text, price::text, restricted_event_id,
		          status, reviewed_by, reviewed_at,
		          COALESCE(review_note,''), created_at`,
		accountID, orderID, tradeID, symbol, side, quantity, price,
		eventID, status).
		Scan(&r.ID, &r.AccountID, &r.OrderID, &r.TradeID, &r.Symbol,
			&r.Side, &r.Quantity, &r.Price, &r.RestrictedEventID,
			&r.Status, &r.ReviewedBy, &r.ReviewedAt, &r.ReviewNote,
			&r.CreatedAt)
	if err != nil {
		return nil, excerrors.New("INTERNAL_ERROR",
			"staff trade record: "+err.Error())
	}
	if eventID != nil && s.alerter != nil {
		_ = s.alerter.RaiseHold(ctx, HoldAlert{
			Severity: "P1", Code: "STAFF_TRADE_RESTRICTED_WINDOW",
			AccountID: accountID,
			Summary: fmt.Sprintf(
				"employee trade on %s inside restricted window %s",
				symbol, *eventID),
		})
	}
	return &r, nil
}

// ReviewTrade records the officer's same-day review outcome.
func (s *EmployeeDealingService) ReviewTrade(ctx context.Context,
	actorID, reviewID int64, status, note string) error {
	if status != "REVIEWED" && status != "ESCALATED" {
		return excerrors.New("INVALID_REQUEST",
			"review outcome must be REVIEWED|ESCALATED")
	}
	if err := s.checkRole(ctx, actorID); err != nil {
		return err
	}
	tag, err := s.pool.Exec(ctx, `
		UPDATE employee_trade_reviews
		   SET status = $2, reviewed_by = $3, reviewed_at = now(),
		       review_note = $4
		 WHERE id = $1 AND status IN ('PENDING','ESCALATED')`,
		reviewID, status, actorID, note)
	if err != nil {
		return excerrors.New("INTERNAL_ERROR", "trade review: "+err.Error())
	}
	if tag.RowsAffected() == 0 {
		return excerrors.New("NOT_FOUND", "no pending staff-trade review")
	}
	_, _, _ = admin.LogAuto(ctx, s.pool, admin.AuditEntry{
		AdminUserID: actorID, Action: "employee_dealing.review",
		TargetType: "employee_trade_review", TargetID: &reviewID,
		AfterState: map[string]any{"status": status, "note": note},
	})
	return nil
}

// ListTradeReviews reads the review queue.
func (s *EmployeeDealingService) ListTradeReviews(ctx context.Context,
	status string, limit int) ([]StaffTradeReview, error) {
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	q := `SELECT id, account_id, order_id, trade_id, symbol,
	             COALESCE(side,''),
	             quantity::text, price::text, restricted_event_id,
	             status, reviewed_by, reviewed_at,
	             COALESCE(review_note,''), created_at
	        FROM employee_trade_reviews`
	args := []any{}
	if status != "" {
		q += " WHERE status = $1"
		args = append(args, status)
	}
	q += fmt.Sprintf(" ORDER BY id DESC LIMIT %d", limit)
	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, excerrors.New("INTERNAL_ERROR", "review list: "+err.Error())
	}
	defer rows.Close()
	var out []StaffTradeReview
	for rows.Next() {
		var r StaffTradeReview
		if err := rows.Scan(&r.ID, &r.AccountID, &r.OrderID, &r.TradeID,
			&r.Symbol, &r.Side, &r.Quantity, &r.Price,
			&r.RestrictedEventID, &r.Status, &r.ReviewedBy,
			&r.ReviewedAt, &r.ReviewNote, &r.CreatedAt); err != nil {
			return nil, excerrors.New("INTERNAL_ERROR",
				"review row: "+err.Error())
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// Program exclusion + audit surface
// ---------------------------------------------------------------------------

// ExcludedFromPrograms reports whether the account is a staff account
// — liquidity/STP/rebate programs consult this to skip employees
// (spec §14.10.1: staff never receive venue incentives).
func (s *EmployeeDealingService) ExcludedFromPrograms(ctx context.Context,
	accountID int64) bool {
	var employee bool
	if err := s.pool.QueryRow(ctx,
		`SELECT employee_account FROM accounts WHERE id = $1`,
		accountID).Scan(&employee); err != nil {
		return false
	}
	return employee
}

// DealingAuditRow is one admin_audit_log row in the auditor's
// employee-dealing view — the endpoint filters the action prefix.
type DealingAuditRow struct {
	ID          int64           `json:"id"`
	AdminUserID int64           `json:"admin_user_id"`
	Action      string          `json:"action"`
	TargetType  string          `json:"target_type"`
	TargetID    *int64          `json:"target_id,omitempty"`
	AfterState  json.RawMessage `json:"after_state,omitempty"`
	CreatedAt   time.Time       `json:"created_at"`
}

// Audit reads the employee-dealing slice of admin_audit_log — the
// Read-Only Auditor surface (route-gated); actions emitted by this
// package carry the employee_dealing.* / restricted_list.* prefixes.
func (s *EmployeeDealingService) Audit(ctx context.Context,
	limit int, afterID int64) ([]DealingAuditRow, error) {
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id, admin_user_id, action, target_type, target_id,
		       after_state, created_at
		  FROM admin_audit_log
		 WHERE (action LIKE 'employee_dealing.%'
		    OR action LIKE 'restricted_list.%')
		   AND ($1 = 0 OR id < $1)
		 ORDER BY id DESC LIMIT $2`, afterID, limit)
	if err != nil {
		return nil, excerrors.New("INTERNAL_ERROR",
			"dealing audit: "+err.Error())
	}
	defer rows.Close()
	var out []DealingAuditRow
	for rows.Next() {
		var r DealingAuditRow
		if err := rows.Scan(&r.ID, &r.AdminUserID, &r.Action,
			&r.TargetType, &r.TargetID, &r.AfterState,
			&r.CreatedAt); err != nil {
			return nil, excerrors.New("INTERNAL_ERROR",
				"audit row: "+err.Error())
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func derefStr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
