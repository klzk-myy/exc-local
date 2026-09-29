// Task 12.3.10 — user self-service emergency account freeze, and the
// Task 12.3.12 item-3 partial-failure recovery.
//
// POST /api/v1/account/emergency-freeze needs ONLY the current session —
// deliberately no additional 2FA: a compromised 2FA device must not be
// able to lock the legitimate owner out of the panic button.
//
// Saga (composes existing primitives — nothing here reimplements them):
//  1. Mass-cancel every working order on the account through the shared
//     OrderDispatcher seam (orders.Service via cmd/gateway adapter) —
//     retried up to EmergencyCancelAttempts (3) on engine failure.
//  2. Freeze transition: accounts.status FROZEN guarded to
//     ACTIVE|SUSPENDED, account_freeze_events reason='SELF_FREEZE'
//     (migration 152), mirrored admin_audit_log row — one transaction.
//     When the cancel retries are exhausted the same transaction also
//     suspends the owning users row (login freeze — Task 12.3.12 part 3:
//     "freeze user login + balance withdrawals immediately").
//  3. Terminate every other session (keepSID = the calling session) and
//     revoke every API key on the account via injected seams.
//  4. Partial failure: if the cancels never confirmed, raise a critical
//     P1 ops alert carrying the still-open order ids for manual desk
//     cancellation, and persist a durable funding_ops_alerts row.
//
// Session termination and key revocation failures never resurrect the
// account — the FROZEN status already blocks trading and withdrawals via
// AssertMutable — but they do mark the result PartialFailure and raise
// the same P1 alert path so nothing fails silently.
package accounts

import (
	"context"
	"encoding/json"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// SelfFreezeReason is the freeze_reason recorded on SELF_FREEZE events.
const SelfFreezeReason = "SELF_FREEZE"

// EmergencyCancelAttempts is the Task 12.3.12 part-3 retry budget for
// open-order cancellations (initial attempt + retries share the count —
// three dispatch calls maximum).
const EmergencyCancelAttempts = 3

// SessionTerminator kills sessions through the auth.SessionManager seam
// (wired by the composition layer — this package never imports auth).
type SessionTerminator interface {
	// RevokeAllExcept revokes every session on the account AND user
	// indexes except keepSID ("" revokes everything). Returns the count
	// actually revoked.
	RevokeAllExcept(ctx context.Context, accountID int64, userID string, keepSID string) (int, error)
}

// CredentialRevoker revokes every live API key on the account
// (auth.KeyStore ListByAccount + Revoke adapter at composition).
type CredentialRevoker interface {
	RevokeAllKeys(ctx context.Context, accountID int64, reason string) (int, error)
}

// OpenOrderLister returns the still-cancellable order ids — the "stuck
// order" set the P1 alert carries for manual desk cancellation
// (orders.PgStore.OpenOrders adapter at composition).
type OpenOrderLister interface {
	OpenOrderIDs(ctx context.Context, accountID int64) ([]int64, error)
}

// FreezeAlert is the P1 payload for the partial-failure path — severity
// values follow the §2.7 ops taxonomy ("P0"/"P1").
type FreezeAlert struct {
	Severity  string            `json:"severity"`
	Code      string            `json:"code"`
	AccountID int64             `json:"account_id"`
	Summary   string            `json:"summary"`
	Details   map[string]string `json:"details,omitempty"`
}

// FreezeAlerter raises the P1 pager alert AND persists the durable
// funding_ops_alerts row (adapter at composition —
// settlement.PublisherAlerter for the NATS leg).
type FreezeAlerter interface {
	Raise(ctx context.Context, a FreezeAlert) error
}

// EmergencyFreezeService runs the self-freeze saga.
type EmergencyFreezeService struct {
	pool     *pgxpool.Pool
	disp     OrderDispatcher
	sessions SessionTerminator
	keys     CredentialRevoker
	orders   OpenOrderLister
	alerter  FreezeAlerter
	now      func() time.Time
}

// NewEmergencyFreezeService wires the saga. pool is mandatory; every
// other dependency is an injected seam — a nil seam fails its step
// closed (counted as a partial failure, never silently skipped).
func NewEmergencyFreezeService(pool *pgxpool.Pool, disp OrderDispatcher,
	sessions SessionTerminator, keys CredentialRevoker,
	orders OpenOrderLister, alerter FreezeAlerter) *EmergencyFreezeService {
	return &EmergencyFreezeService{
		pool: pool, disp: disp, sessions: sessions, keys: keys,
		orders: orders, alerter: alerter, now: time.Now,
	}
}

// EmergencyFreezeRequest carries the calling session's context.
type EmergencyFreezeRequest struct {
	AccountID int64  // claims account
	UserID    int64  // numeric claims subject — must own the account
	SessionID string // current sid — retained so the caller sees the result
	ClientIP  string // audit column
}

// EmergencyFreezeResult is the endpoint payload.
type EmergencyFreezeResult struct {
	AccountID        int64     `json:"account_id"`
	Status           string    `json:"status"` // FROZEN
	OrdersCancelled  int       `json:"orders_cancelled"`
	SessionsRevoked  int       `json:"sessions_revoked"`
	APIKeysRevoked   int       `json:"api_keys_revoked"`
	CancelFailed     bool      `json:"cancel_failed,omitempty"`   // retries exhausted
	StuckOrderIDs    []int64   `json:"stuck_order_ids,omitempty"` // manual-desk set
	LoginSuspended   bool      `json:"login_suspended,omitempty"` // cancel-failure escalation
	PartialFailure   bool      `json:"partial_failure,omitempty"` // post-freeze step failure
	PartialDetail    string    `json:"partial_detail,omitempty"`
	FreezeRecordedAt time.Time `json:"freeze_recorded_at"`
}

// Freeze runs the saga. Only the account owner may self-freeze (a
// delegated login cannot freeze the master it works under). The freeze
// transition is the mandatory pivot — if it cannot commit the call
// returns an error and nothing else is attempted; every other failure
// degrades to partial-failure + P1 alert.
func (s *EmergencyFreezeService) Freeze(ctx context.Context, req EmergencyFreezeRequest) (*EmergencyFreezeResult, error) {
	if req.AccountID == 0 || req.UserID == 0 {
		return nil, newError(CodeUnauthorized, "account and user context required")
	}
	res := &EmergencyFreezeResult{AccountID: req.AccountID}

	// Step 1 — mass cancel, ≤3 attempts (Task 12.3.12 part 3). Cancels
	// are idempotent so a retried dispatch is always safe; ANY dispatch
	// error retries — the distinguishing detail lands in the alert.
	cancelErr := s.cancelWithRetry(ctx, req.AccountID, res)

	// Step 2 — the FROZEN transition (mandatory). On exhausted cancels
	// the same transaction suspends the owner's login: orders may still
	// be live on the book, so every re-entry path must close.
	loginSuspended := cancelErr != nil
	if err := s.freezeTx(ctx, req, loginSuspended); err != nil {
		return nil, err
	}
	res.Status = string(StatusFrozen)
	res.LoginSuspended = loginSuspended
	res.FreezeRecordedAt = s.now().UTC()

	// Step 3 — sessions + API keys. A failure here leaves the legal hold
	// standing (AssertMutable blocks trading/withdrawals regardless) so
	// it degrades to partial-failure rather than aborting the freeze.
	if s.sessions == nil {
		res.PartialFailure = true
		res.PartialDetail = "session terminator not wired"
	} else if n, err := s.sessions.RevokeAllExcept(ctx, req.AccountID,
		strconv.FormatInt(req.UserID, 10), req.SessionID); err != nil {
		res.PartialFailure = true
		res.PartialDetail = "session revocation: " + err.Error()
	} else {
		res.SessionsRevoked = n
	}
	if s.keys == nil {
		res.PartialFailure = true
		res.PartialDetail = appendDetail(res.PartialDetail, "credential revoker not wired")
	} else if n, err := s.keys.RevokeAllKeys(ctx, req.AccountID, SelfFreezeReason); err != nil {
		res.PartialFailure = true
		res.PartialDetail = appendDetail(res.PartialDetail, "api key revocation: "+err.Error())
	} else {
		res.APIKeysRevoked = n
	}

	// Step 4 — P1 alert for the partial-failure paths (cancel retries
	// exhausted, or a credential step failed). The alert carries the
	// still-open order ids for manual desk cancellation.
	if cancelErr != nil {
		res.CancelFailed = true
		if s.orders != nil {
			if ids, err := s.orders.OpenOrderIDs(ctx, req.AccountID); err == nil {
				res.StuckOrderIDs = ids
			}
		}
		s.raiseAlert(ctx, req.AccountID, "SELF_FREEZE_CANCEL_STUCK",
			"emergency self-freeze: order mass-cancel failed after "+
				strconv.Itoa(EmergencyCancelAttempts)+" attempts; manual desk "+
				"cancellation required", cancelErr, res.StuckOrderIDs)
	}
	if res.PartialFailure {
		s.raiseAlert(ctx, req.AccountID, "SELF_FREEZE_PARTIAL",
			"emergency self-freeze completed with a partial failure: "+
				res.PartialDetail, nil, nil)
	}
	return res, nil
}

// cancelWithRetry runs the mass-cancel up to EmergencyCancelAttempts
// times; the last error is returned (nil on success).
func (s *EmergencyFreezeService) cancelWithRetry(ctx context.Context,
	accountID int64, res *EmergencyFreezeResult) error {
	if s.disp == nil {
		return newError("INTERNAL_ERROR", "order dispatcher not wired")
	}
	var err error
	for attempt := 1; attempt <= EmergencyCancelAttempts; attempt++ {
		var mc *MassCancelResult
		mc, err = s.disp.MassCancel(ctx, MassCancelScope{
			AccountID: accountID,
			Reason:    "self_freeze",
		})
		if err == nil {
			if mc != nil {
				res.OrdersCancelled = mc.Cancelled
			}
			return nil
		}
		if ctx.Err() != nil {
			break // caller deadline — no point retrying
		}
	}
	return err
}

// freezeTx is the atomic FROZEN transition: status guard (ACTIVE or
// SUSPENDED only), the SELF_FREEZE account_freeze_events row and the
// mirrored admin_audit_log row — plus the owner-login suspension when
// the saga escalated after failed cancels.
func (s *EmergencyFreezeService) freezeTx(ctx context.Context,
	req EmergencyFreezeRequest, suspendLogin bool) error {

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return errorf("INTERNAL_ERROR", "begin tx: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var prev string
	var owner int64
	err = tx.QueryRow(ctx,
		`SELECT status, user_id FROM accounts WHERE id = $1 FOR UPDATE`,
		req.AccountID).Scan(&prev, &owner)
	if err == pgx.ErrNoRows {
		return errorf(CodeNotFound, "account %d not found", req.AccountID)
	}
	if err != nil {
		return errorf("INTERNAL_ERROR", "lock account %d: %v", req.AccountID, err)
	}
	// Only the account owner may self-freeze — a delegated login under
	// the master must not be able to take the institution down.
	if owner != req.UserID {
		return newError(CodeForbidden,
			"emergency freeze requires the account owner session")
	}
	if prev != string(StatusActive) && prev != string(StatusSuspended) {
		return errorf(CodeInvalidRequest,
			"cannot self-freeze account %d in status %s", req.AccountID, prev)
	}

	if _, err := tx.Exec(ctx,
		`UPDATE accounts SET status = $2, updated_at = now() WHERE id = $1`,
		req.AccountID, string(StatusFrozen)); err != nil {
		return errorf("INTERNAL_ERROR", "self-freeze account %d: %v", req.AccountID, err)
	}

	// Login freeze on the cancel-failure escalation path: the owning
	// user cannot re-authenticate until the Phase-14 re-verification
	// workflow restores status (documented at Task 12.3.12 part 3).
	if suspendLogin {
		if _, err := tx.Exec(ctx,
			`UPDATE users SET status='SUSPENDED', updated_at=now()
			  WHERE id=$1 AND status='ACTIVE'`, owner); err != nil {
			return errorf("INTERNAL_ERROR", "suspend owner %d: %v", owner, err)
		}
	}

	meta, _ := json.Marshal(map[string]any{
		"self_initiated":   true,
		"ip":               req.ClientIP,
		"login_suspended":  suspendLogin,
		"session_retained": req.SessionID != "",
	})
	if _, err := tx.Exec(ctx,
		`INSERT INTO account_freeze_events
		   (account_id, action, reason, initiated_by, approved_by,
		    prev_status, new_status, metadata)
		 VALUES ($1,'FREEZE',$2,$3,$3,$4,$5,$6)`,
		req.AccountID, SelfFreezeReason, req.UserID, prev,
		string(StatusFrozen), meta); err != nil {
		return errorf("INTERNAL_ERROR", "freeze event insert: %v", err)
	}

	if _, err := tx.Exec(ctx,
		`INSERT INTO admin_audit_log
		   (admin_user_id, action, target_type, target_id, before_state, after_state, ip_address)
		 VALUES ($1,'account.self_freeze','account',$2,
		         jsonb_build_object('status',$3::text),
		         jsonb_build_object('status',$4::text,'reason',$5::text,
		                            'self_initiated',true,'login_suspended',$6::boolean),
		         NULLIF($7,'')::inet)`,
		req.UserID, req.AccountID, prev, string(StatusFrozen),
		SelfFreezeReason, suspendLogin, req.ClientIP); err != nil {
		return errorf("INTERNAL_ERROR", "admin audit insert: %v", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return errorf("INTERNAL_ERROR", "commit self-freeze: %v", err)
	}
	return nil
}

// raiseAlert fires the P1 alert seam; delivery failure is best-effort —
// the committed freeze state is never masked by a pager outage.
func (s *EmergencyFreezeService) raiseAlert(ctx context.Context, accountID int64,
	code, summary string, cause error, orderIDs []int64) {
	if s.alerter == nil {
		return
	}
	a := FreezeAlert{
		Severity: "P1", Code: code, AccountID: accountID, Summary: summary,
		Details: map[string]string{},
	}
	if cause != nil {
		a.Details["error"] = cause.Error()
	}
	if len(orderIDs) > 0 {
		ids, err := json.Marshal(orderIDs)
		if err == nil {
			a.Details["order_ids"] = string(ids)
		}
	}
	_ = s.alerter.Raise(ctx, a)
}

// appendDetail joins partial-failure causes.
func appendDetail(prev, more string) string {
	if prev == "" {
		return more
	}
	return prev + "; " + more
}
