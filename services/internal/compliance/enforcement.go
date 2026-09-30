// Package compliance — Phase-21 Task 21.3.8: Market-Abuse Enforcement.
//
// Phase-17 (services/internal/surveillance) detects and persists
// surveillance_signals but must never throttle, cancel or flag
// accounts live. This service is the Phase-21 enforcement half: it
// consumes persisted signals and converts them into enforcement
// actions through existing seams — never duplicating detector logic
// and never touching the matching engine directly:
//
//	WARN     → advisory row + owner notification (notify seam)
//	THROTTLE → Redis admission cap (enforce:throttle:{account_id}) —
//	           orders.Service consults CheckAdmission on every new
//	           order (the gateway dispatch seam, not the C++ core)
//	RESTRICT → SCOPE_ACCOUNT kill-switch suspension via
//	           admin.KillSwitchService (engine-flagged order halt)
//	SUSPEND  → compliance hold via HoldService.PlaceHold
//	           (UNUSUAL_ACTIVITY trigger — freezes the account,
//	           cancels resting orders, preserves positions)
//	DISMISS  → marks the signal DISMISSED (no action row materialises)
//
// Every action lands in enforcement_actions (migration 239); the
// UNIQUE(signal_id, action) partial index makes retries idempotent.
// Severe signal classes never auto-escalate past WARN — a Compliance
// Officer reviews via POST /api/v1/admin/enforcement/{signal_id}.
package compliance

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	goredis "github.com/redis/go-redis/v9"

	"exchange/internal/admin"
	"exchange/internal/marketdata"

	excerrors "exchange/pkg/errors"
)

// Action vocabulary (CHECK constraint mirrors migration 239).
const (
	EnfActionWarn     = "WARN"
	EnfActionThrottle = "THROTTLE"
	EnfActionRestrict = "RESTRICT"
	EnfActionSuspend  = "SUSPEND"
	EnfActionDismiss  = "DISMISS"
)

const (
	// EnforcementThrottleDefault caps order-entry messages/sec for a
	// throttled account (spec §14.9.1 graduated response).
	EnforcementThrottleDefault = 5
	// EnforcementTTLDefault bounds THROTTLE/RESTRICT actions — review
	// cadence forces officers to re-affirm, never silently permanent.
	EnforcementTTLDefault = 24 * time.Hour
	// autoSweepPage bounds the OPEN-signal scan per sweep pass.
	autoSweepPage = 200
)

// severeSignalTypes may not be auto-escalated beyond WARN — insider
// dealing, wash trading and front-running all demand an officer's
// eyes (MAR Art. 16 / spec §14.9.2 evidence review) before the venue
// restricts or suspends.
var severeSignalTypes = map[string]bool{
	"INSIDER_DEALING": true,
	"WASH_TRADING":    true,
	"FRONT_RUNNING":   true,
}

// EnforcementSignal is the surveillance_signals projection the service
// enforces on (schema pinned by migration 029).
type EnforcementSignal struct {
	ID         int64  `json:"id"`
	SignalType string `json:"signal_type"`
	Symbol     string `json:"symbol"`
	// account_hash is BIGINT — the engine writes int64(hash), so the
	// signed value is the canonical stored form; bitcast to uint64
	// only when hashing for comparison (marketdata.L3AccountHash).
	AccountHash int64           `json:"account_hash"`
	Status      string          `json:"status"`
	Evidence    json.RawMessage `json:"evidence"`
	CreatedAt   time.Time       `json:"created_at"`
}

// EnforcementAction is one enforcement_actions row.
type EnforcementAction struct {
	ActionID    string          `json:"action_id"`
	SignalID    *int64          `json:"signal_id,omitempty"`
	CaseID      *int64          `json:"case_id,omitempty"`
	AccountID   *int64          `json:"account_id,omitempty"`
	AccountHash int64           `json:"account_hash"` // signed BIGINT form (see EnforcementSignal)
	Action      string          `json:"action"`
	Source      string          `json:"source"`
	Params      json.RawMessage `json:"params"`
	Status      string          `json:"status"`
	ActorID     int64           `json:"actor_id"`
	Note        string          `json:"note"`
	ExpiresAt   *time.Time      `json:"expires_at,omitempty"`
	CreatedAt   time.Time       `json:"created_at"`
}

// EnforcementRequest is the officer/auto input to Enforce.
type EnforcementRequest struct {
	SignalID  int64 // 0 → ad-hoc action on account_id (no signal anchor)
	CaseID    int64 // >0 → action taken from a surveillance case
	AccountID int64 // resolved account; 0 → resolved via AccountHash
	Action    string
	Params    map[string]any // throttle: max_msgs_per_sec, ttl_seconds
	Source    string         // AUTO | MANUAL
	ActorID   int64          // officer id; system id for AUTO
	Note      string
	TTL       time.Duration // 0 → EnforcementTTLDefault
}

// ---------------------------------------------------------------------------
// Seams
// ---------------------------------------------------------------------------

// HoldPlacer is the SUSPEND seam — HoldService.PlaceHold-compatible.
type HoldPlacer interface {
	PlaceHold(ctx context.Context, req PlaceHoldRequest) (*Hold, error)
}

// ScopedSuspender is the RESTRICT seam — admin.KillSwitchService-
// compatible (SCOPE_ACCOUNT only; no global/counterparty scope may be
// reached from this path).
type ScopedSuspender interface {
	Set(ctx context.Context, actor admin.AdminActor,
		scope, target, reason string) (*admin.Suspension, error)
	Clear(ctx context.Context, actor admin.AdminActor,
		scope, target, reason string) (*admin.Suspension, error)
}

// AccountResolver inverts the marketdata.L3AccountHash pseudonym to a
// real accounts.id. The hash is salted FNV-1a (one-way) — the gateway
// wires a scanning resolver (ScanAccountsResolver) that caches hits.
type AccountResolver func(ctx context.Context, accountHash uint64) (int64, bool)

// EnforcementNotifier is the WARN advisory seam — the owner-facing
// notification (notifications.Service.Notify adapter; best-effort).
type EnforcementNotifier func(ctx context.Context, userID int64,
	event string, payload map[string]any)

// EnforcementService converts surveillance signals into enforcement.
type EnforcementService struct {
	pool      *pgxpool.Pool
	rdb       goredis.Cmdable // nil → THROTTLE degrades to record-only
	holds     HoldPlacer      // nil → SUSPEND fails closed
	suspender ScopedSuspender // nil → RESTRICT fails closed
	resolve   AccountResolver // nil → pseudonym-only actions
	resolver  HoldRoleResolver
	notify    EnforcementNotifier
	now       func() time.Time
	newID     func() (string, error)
}

// NewEnforcementService binds the dependencies; resolver is the admin
// role resolver (admin.AdminRoleResolver-compatible).
func NewEnforcementService(pool *pgxpool.Pool, rdb goredis.Cmdable,
	holds HoldPlacer, suspender ScopedSuspender,
	resolve AccountResolver, resolver HoldRoleResolver,
	notify EnforcementNotifier) *EnforcementService {
	return &EnforcementService{
		pool: pool, rdb: rdb, holds: holds, suspender: suspender,
		resolve: resolve, resolver: resolver, notify: notify,
		now: time.Now, newID: defaultEnforcementID,
	}
}

// defaultEnforcementID mints the "enf_<28urlsafe>" public id (same
// generator discipline as defaultHoldID).
func defaultEnforcementID() (string, error) {
	raw := make([]byte, 21)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return "enf_" + base64.RawURLEncoding.EncodeToString(raw), nil
}

// WithIDSource overrides the action-id generator (tests).
func (s *EnforcementService) WithIDSource(f func() (string, error)) *EnforcementService {
	if f != nil {
		s.newID = f
	}
	return s
}

// WithClock overrides the clock (tests / sweeps).
func (s *EnforcementService) WithClock(f func() time.Time) *EnforcementService {
	if f != nil {
		s.now = f
	}
	return s
}

// ---------------------------------------------------------------------------
// Officer endpoint — POST /api/v1/admin/enforcement/{signal_id}
// ---------------------------------------------------------------------------

// Enforce applies one action against a signal (or ad-hoc against an
// account when SignalID==0). Compliance Officer / Super Admin only;
// the officer's action commits atomically with its enforcement row +
// admin audit + signal status transition.
func (s *EnforcementService) Enforce(ctx context.Context,
	req EnforcementRequest) (*EnforcementAction, error) {
	if req.Action != EnfActionWarn && req.Action != EnfActionThrottle &&
		req.Action != EnfActionRestrict && req.Action != EnfActionSuspend &&
		req.Action != EnfActionDismiss {
		return nil, excerrors.New("INVALID_REQUEST",
			"action must be WARN|THROTTLE|RESTRICT|SUSPEND|DISMISS")
	}
	if req.Source == "" {
		req.Source = "MANUAL"
	}
	if req.Source == "MANUAL" && req.ActorID > 0 {
		if err := s.checkRole(ctx, req.ActorID); err != nil {
			return nil, err
		}
	}
	if req.ActorID <= 0 {
		return nil, excerrors.New("UNAUTHORIZED", "actor id required")
	}

	var sig *EnforcementSignal
	if req.SignalID > 0 {
		var err error
		sig, err = s.loadSignal(ctx, req.SignalID)
		if err != nil {
			return nil, err
		}
	}

	accountID := req.AccountID
	var accountHash int64
	if sig != nil {
		accountHash = sig.AccountHash
	}
	if accountID <= 0 && accountHash != 0 && s.resolve != nil {
		if id, ok := s.resolve(ctx, uint64(accountHash)); ok {
			accountID = id
		}
	}
	if accountHash == 0 && accountID > 0 {
		accountHash = int64(marketdata.L3AccountHash(uint64(accountID)))
	}
	if accountID <= 0 && req.Action != EnfActionDismiss {
		return nil, excerrors.New("ACCOUNT_NOT_FOUND",
			"signal account_hash is not resolvable to an account — "+
				"enforcement requires an account identity")
	}

	params := req.Params
	if params == nil {
		params = map[string]any{}
	}
	ttl := req.TTL
	if ttl <= 0 {
		ttl = EnforcementTTLDefault
	}
	var expiresAt *time.Time
	if req.Action == EnfActionThrottle || req.Action == EnfActionRestrict {
		t := s.now().UTC().Add(ttl)
		expiresAt = &t
	}
	rate := EnforcementThrottleDefault
	if req.Action == EnfActionThrottle {
		if v, ok := params["max_msgs_per_sec"].(float64); ok && v > 0 {
			rate = int(v)
		}
		params["max_msgs_per_sec"] = rate
	}

	actionID, err := s.newID()
	if err != nil {
		return nil, excerrors.New("INTERNAL_ERROR", "action id: "+err.Error())
	}
	act := &EnforcementAction{
		ActionID: actionID, AccountHash: accountHash,
		Action: req.Action, Source: req.Source, ActorID: req.ActorID,
		Note: req.Note, ExpiresAt: expiresAt, CreatedAt: s.now().UTC(),
		Status: "ACTIVE",
	}
	if accountID > 0 {
		act.AccountID = &accountID
	}
	if req.SignalID > 0 {
		act.SignalID = &req.SignalID
	}
	if req.CaseID > 0 {
		act.CaseID = &req.CaseID
	}

	// Persist first — the row is the idempotency anchor; side effects
	// follow so a failed side effect never leaves an invisible freeze.
	if err := s.persist(ctx, act, params, sig); err != nil {
		return nil, err
	}

	// Side effects by action — fail the request (not the record) when a
	// mandatory seam is missing so a degraded deploy cannot silently
	// "enforce" nothing.
	switch req.Action {
	case EnfActionWarn:
		if s.notify != nil && accountID > 0 {
			if owner, ok := s.accountOwner(ctx, accountID); ok {
				s.notify(ctx, owner, "enforcement.warn", map[string]any{
					"action_id": actionID, "signal_id": req.SignalID,
					"signal_type": sigType(sig), "symbol": sigSymbol(sig),
				})
			}
		}
	case EnfActionThrottle:
		if s.rdb == nil {
			return act, excerrors.New("SERVICE_DEGRADED",
				"throttle backend unavailable — action recorded but not enforced")
		}
		if err := s.setThrottle(ctx, accountID, rate, ttl); err != nil {
			return act, excerrors.Wrap("INTERNAL_ERROR",
				"throttle flag", err)
		}
	case EnfActionRestrict:
		if s.suspender == nil {
			return act, excerrors.New("SERVICE_DEGRADED",
				"kill-switch seam unwired — action recorded but not enforced")
		}
		_, err := s.suspender.Set(ctx, admin.AdminActor{UserID: req.ActorID},
			admin.ScopeAccount, strconv.FormatInt(accountID, 10),
			fmt.Sprintf("enforcement %s on signal %d: %s",
				actionID, req.SignalID, req.Note))
		if err != nil {
			return act, err
		}
	case EnfActionSuspend:
		if s.holds == nil {
			return act, excerrors.New("SERVICE_DEGRADED",
				"hold seam unwired — action recorded but not enforced")
		}
		h, err := s.holds.PlaceHold(ctx, PlaceHoldRequest{
			AccountID: accountID,
			Trigger:   HoldTriggerUnusualActivity,
			Reason: fmt.Sprintf("market-abuse enforcement %s (signal %d): %s",
				actionID, req.SignalID, req.Note),
			EvidenceRef: fmt.Sprintf("enforcement:%s", actionID),
			PlacedBy:    req.ActorID,
		})
		if err != nil {
			return act, err
		}
		params["hold_id"] = h.HoldID
	}

	// Signal lifecycle: an actioned signal advances to CASED (029
	// CHECK keeps the vocabulary OPEN|CASED|DISMISSED); DISMISS marks
	// it DISMISSED. Ad-hoc actions (no signal) skip the transition.
	if sig != nil {
		to := "CASED"
		if req.Action == EnfActionDismiss {
			to = "DISMISSED"
		}
		if err := s.markSignal(ctx, sig.ID, to); err != nil {
			return act, err
		}
	}
	return act, nil
}

// ---------------------------------------------------------------------------
// Auto-sweep — graduated response over OPEN signals
// ---------------------------------------------------------------------------

// AutoEnforce consumes OPEN surveillance signals in bounded pages.
// Policy: severe classes (WASH_TRADING / INSIDER_DEALING /
// FRONT_RUNNING) auto-WARN once and wait for officer review;
// repeatable classes follow the WARN → THROTTLE ladder keyed on the
// account's recent action history. Returns actions taken.
func (s *EnforcementService) AutoEnforce(ctx context.Context,
	limit int) (int, error) {
	if limit <= 0 || limit > autoSweepPage {
		limit = autoSweepPage
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id, signal_type, symbol, account_hash, status,
		       evidence, created_at
		  FROM surveillance_signals
		 WHERE status = 'OPEN'
		 ORDER BY id ASC
		 LIMIT $1`, limit)
	if err != nil {
		return 0, excerrors.New("INTERNAL_ERROR",
			"enforcement sweep scan: "+err.Error())
	}
	defer rows.Close()

	var sigs []EnforcementSignal
	for rows.Next() {
		var sg EnforcementSignal
		var ev []byte
		if err := rows.Scan(&sg.ID, &sg.SignalType, &sg.Symbol,
			&sg.AccountHash, &sg.Status, &ev, &sg.CreatedAt); err != nil {
			return 0, excerrors.New("INTERNAL_ERROR",
				"enforcement sweep row: "+err.Error())
		}
		sg.Evidence = ev
		sigs = append(sigs, sg)
	}
	if err := rows.Err(); err != nil {
		return 0, excerrors.New("INTERNAL_ERROR",
			"enforcement sweep: "+err.Error())
	}

	taken := 0
	for _, sg := range sigs {
		// Signals already actioned once are skipped by the dedup
		// index; the ladder picks the next rung from history.
		next := EnfActionWarn
		if severeSignalTypes[sg.SignalType] {
			next = EnfActionWarn // officer review owns escalation
		} else if s.priorActions(ctx, sg.AccountHash) > 0 {
			next = EnfActionThrottle
		}
		_, err := s.Enforce(ctx, EnforcementRequest{
			SignalID: sg.ID, Action: next, Source: "AUTO",
			ActorID: s.systemActor(ctx, sg.AccountHash),
			Note:    "auto " + next + " on " + sg.SignalType + " signal",
		})
		if err != nil {
			// Duplicate/unresolvable rows are per-signal skips, never
			// sweep-fatal — the dedup index makes a retry a no-op.
			if excerrors.CodeOf(err) == "ACCOUNT_NOT_FOUND" ||
				isUniqueViolation(err) {
				continue
			}
			return taken, err
		}
		taken++
	}
	return taken, nil
}

// priorActions counts ACTIVE/EXPIRED enforcement history on the
// pseudonym in the trailing 7 days — the ladder rung selector.
func (s *EnforcementService) priorActions(ctx context.Context,
	accountHash int64) int {
	var n int
	_ = s.pool.QueryRow(ctx, `
		SELECT count(*) FROM enforcement_actions
		 WHERE account_hash = $1
		   AND created_at > now() - interval '7 days'`,
		accountHash).Scan(&n)
	return n
}

// ---------------------------------------------------------------------------
// Admission seam — orders.EnforcementGate implementation
// ---------------------------------------------------------------------------

// CheckAdmission is the orders.Service EnforcementGate: an ACTIVE
// THROTTLE row (Redis cap flag) rate-limits new-order admission; the
// RESTRICT/SUSPEND paths are already covered by the kill-switch /
// account-status gates upstream. Returns RATE_LIMIT_TIER_EXCEEDED
// over the cap; nil rdb fails OPEN for this gate only (freeze/
// suspend enforcement lives on the durable seams, not the cache).
func (s *EnforcementService) CheckAdmission(ctx context.Context,
	accountID int64) error {
	if s.rdb == nil || accountID <= 0 {
		return nil
	}
	key := fmt.Sprintf("enforce:throttle:%d", accountID)
	raw, err := s.rdb.Get(ctx, key).Result()
	if err == goredis.Nil {
		return nil
	}
	if err != nil {
		return nil // cache fault: durable gates still hold
	}
	var cap struct {
		Rate int `json:"rate"`
	}
	if json.Unmarshal([]byte(raw), &cap) != nil || cap.Rate <= 0 {
		return nil
	}
	ctr := fmt.Sprintf("enforce:thr:ctr:%d:%d", accountID, s.now().Unix())
	n, err := s.rdb.Incr(ctx, ctr).Result()
	if err != nil {
		return nil
	}
	if n == 1 {
		_ = s.rdb.Expire(ctx, ctr, 2*time.Second).Err()
	}
	if n > int64(cap.Rate) {
		return excerrors.New("RATE_LIMIT_TIER_EXCEEDED",
			fmt.Sprintf("market-abuse throttle: %d msg/s cap exceeded", cap.Rate))
	}
	return nil
}

// ---------------------------------------------------------------------------
// Reads (officer dashboard)
// ---------------------------------------------------------------------------

// List returns the enforcement ledger, newest first.
func (s *EnforcementService) List(ctx context.Context, accountID int64,
	limit int) ([]EnforcementAction, error) {
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	q := `SELECT action_id, signal_id, case_id, account_id, account_hash,
	             action, source, params, status, actor_id, note,
	             expires_at, created_at
	        FROM enforcement_actions`
	args := []any{}
	if accountID > 0 {
		q += " WHERE account_id = $1"
		args = append(args, accountID)
	}
	q += fmt.Sprintf(" ORDER BY id DESC LIMIT %d", limit)
	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, excerrors.New("INTERNAL_ERROR", "enforcement list: "+err.Error())
	}
	defer rows.Close()
	var out []EnforcementAction
	for rows.Next() {
		var a EnforcementAction
		if err := rows.Scan(&a.ActionID, &a.SignalID, &a.CaseID,
			&a.AccountID, &a.AccountHash, &a.Action, &a.Source,
			&a.Params, &a.Status, &a.ActorID, &a.Note,
			&a.ExpiresAt, &a.CreatedAt); err != nil {
			return nil, excerrors.New("INTERNAL_ERROR",
				"enforcement row: "+err.Error())
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// Internals
// ---------------------------------------------------------------------------

func (s *EnforcementService) loadSignal(ctx context.Context,
	id int64) (*EnforcementSignal, error) {
	var sg EnforcementSignal
	var ev []byte
	err := s.pool.QueryRow(ctx, `
		SELECT id, signal_type, symbol, account_hash, status,
		       evidence, created_at
		  FROM surveillance_signals WHERE id = $1`, id).
		Scan(&sg.ID, &sg.SignalType, &sg.Symbol, &sg.AccountHash,
			&sg.Status, &ev, &sg.CreatedAt)
	if err == pgx.ErrNoRows {
		return nil, excerrors.New("NOT_FOUND", "surveillance signal not found")
	}
	if err != nil {
		return nil, excerrors.New("INTERNAL_ERROR", "signal load: "+err.Error())
	}
	sg.Evidence = ev
	return &sg, nil
}

// persist writes the action row + admin audit in one tx; the
// UNIQUE(signal_id, action) index makes a duplicate request a
// no-op-ish 23505 → the caller surfaces it as already-enforced.
func (s *EnforcementService) persist(ctx context.Context,
	a *EnforcementAction, params map[string]any, sig *EnforcementSignal) error {
	pb, err := json.Marshal(params)
	if err != nil {
		return excerrors.New("INVALID_REQUEST", "params not JSON-marshalable")
	}
	a.Params = pb
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return excerrors.New("INTERNAL_ERROR", "enforcement tx: "+err.Error())
	}
	defer func() { _ = tx.Rollback(ctx) }()
	_, err = tx.Exec(ctx, `
		INSERT INTO enforcement_actions
		    (action_id, signal_id, case_id, account_id, account_hash,
		     action, source, params, actor_id, note, expires_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`,
		a.ActionID, a.SignalID, a.CaseID, a.AccountID, a.AccountHash,
		a.Action, a.Source, a.Params, a.ActorID, a.Note, a.ExpiresAt)
	if err != nil {
		if isUniqueViolation(err) {
			return excerrors.New("ENFORCEMENT_ACTION_EXISTS",
				"action already enforced for this signal")
		}
		return excerrors.New("INTERNAL_ERROR",
			"enforcement insert: "+err.Error())
	}
	var tid *int64
	if a.SignalID != nil {
		tid = a.SignalID
	} else if a.AccountID != nil {
		tid = a.AccountID
	}
	if _, _, err := admin.Log(ctx, tx, admin.AuditEntry{
		AdminUserID: a.ActorID,
		Action:      "enforcement." + a.Action,
		TargetType:  "surveillance_signal",
		TargetID:    tid,
		AfterState: map[string]any{
			"action_id": a.ActionID, "signal_id": a.SignalID,
			"case_id": a.CaseID, "account_id": a.AccountID,
			"account_hash": a.AccountHash, "source": a.Source,
			"params": a.Params, "signal_type": sigType(sig),
			"symbol": sigSymbol(sig), "note": a.Note,
		},
	}); err != nil {
		return excerrors.New("INTERNAL_ERROR", "enforcement audit: "+err.Error())
	}
	return tx.Commit(ctx)
}

func (s *EnforcementService) markSignal(ctx context.Context,
	id int64, status string) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE surveillance_signals SET status = $2, updated_at = now()
		  WHERE id = $1 AND status = 'OPEN'`, id, status)
	if err != nil {
		return excerrors.New("INTERNAL_ERROR", "signal transition: "+err.Error())
	}
	return nil
}

func (s *EnforcementService) setThrottle(ctx context.Context,
	accountID int64, rate int, ttl time.Duration) error {
	v, _ := json.Marshal(map[string]any{"rate": rate})
	return s.rdb.Set(ctx,
		fmt.Sprintf("enforce:throttle:%d", accountID), v, ttl).Err()
}

func (s *EnforcementService) accountOwner(ctx context.Context,
	accountID int64) (int64, bool) {
	var uid int64
	if err := s.pool.QueryRow(ctx,
		`SELECT user_id FROM accounts WHERE id = $1`,
		accountID).Scan(&uid); err != nil {
		return 0, false
	}
	return uid, true
}

// systemActor returns the attributable id for AUTO rows — the affected
// account owner when resolvable (lifecycle.go convention: audit
// admin_user_id cannot be 0), else 1 (bootstrap admin).
func (s *EnforcementService) systemActor(ctx context.Context,
	accountHash int64) int64 {
	if s.resolve != nil {
		if id, ok := s.resolve(ctx, uint64(accountHash)); ok {
			if uid, ok2 := s.accountOwner(ctx, id); ok2 {
				return uid
			}
		}
	}
	return 1
}

func (s *EnforcementService) checkRole(ctx context.Context, userID int64) error {
	if s.resolver == nil {
		return excerrors.New("INTERNAL_ERROR", "role resolver not wired")
	}
	role, err := s.resolver(ctx, userID)
	if err != nil {
		return excerrors.New("INTERNAL_ERROR", "role lookup: "+err.Error())
	}
	if role != "Compliance Officer" && role != "Super Admin" {
		return excerrors.New("UNAUTHORIZED_ROLE",
			"role "+role+" cannot take enforcement actions")
	}
	return nil
}

func sigType(s *EnforcementSignal) string {
	if s == nil {
		return ""
	}
	return s.SignalType
}
func sigSymbol(s *EnforcementSignal) string {
	if s == nil {
		return ""
	}
	return s.Symbol
}

// isUniqueViolation lives in submission_dispatcher.go (same package).

// ---------------------------------------------------------------------------
// AccountResolver over accounts — the gateway wires this; the salted
// hash is one-way so resolution scans candidate ids and caches hits.
// ---------------------------------------------------------------------------

// ScanAccountsResolver resolves L3 account_hash → accounts.id by
// hashing candidate ids with marketdata.L3AccountHash. Positive hits
// are cached for the pool's life (the mapping is stable); misses
// rescan so newly-registered accounts resolve on the next attempt.
// Suitable for the compliance admin path — per-signal O(accounts).
func ScanAccountsResolver(pool *pgxpool.Pool) AccountResolver {
	cache := map[uint64]int64{}
	return func(ctx context.Context, accountHash uint64) (int64, bool) {
		if id, ok := cache[accountHash]; ok {
			return id, true
		}
		rows, err := pool.Query(ctx, `SELECT id FROM accounts`)
		if err != nil {
			return 0, false
		}
		defer rows.Close()
		var found int64
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				return 0, false
			}
			h := marketdata.L3AccountHash(uint64(id))
			cache[h] = id // warm every id — amortises the scan
			if h == accountHash {
				found = id
			}
		}
		if rows.Err() != nil {
			return 0, false
		}
		return found, found > 0
	}
}
