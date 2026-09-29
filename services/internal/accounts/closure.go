// Account closure & offboarding — Phase-14 Task 14.3.9.
//
// POST /api/v1/account/close (client path, RequireTwoFactor-wrapped at
// the route) and the forced-closure dual-control executor share one
// pipeline:
//
//  1. ownership / role gate;
//  2. precondition read — open positions, open orders, pending
//     settlements, pending funding transactions (and residual locked
//     balances) must all be zero, else ACCOUNT_CLOSE_BLOCKED carrying
//     the blocking breakdown;
//  3. residual sweep — every remaining balance drains to a verified
//     beneficiary through the Phase-11 withdrawal pipeline (the
//     pipeline's own gate enforces VERIFIED + the 24h hold and posts
//     the GL-balanced CustomerLiability→ClearingTransit journal; the
//     emitted withdrawal ids land in sweep_refs);
//  4. one transaction flips accounts.status → CLOSED and inserts the
//     account_closures row (preconditions_snapshot + sweep_refs +
//     completed_at) — read-only retention for audit;
//  5. post-commit revocation — all sessions ("" keepSID), all API keys
//     — then a post-verify recount; anything that raced in during the
//     sweep window raises a P1 for the desk.
//
// Forced closure (Compliance Officer via admin.OpAccountClosure
// dual-control) runs the same pipeline inside the approval
// transaction: it mass-cancels + market-closes first so the officer
// does not have to wait for the client to flatten, and it requires no
// client 2FA.
//
// Reopening is prohibited by construction: nothing anywhere writes
// CLOSED → anything; AssertMutable, order admission, funding and
// auth-facing account reads all fail closed on the terminal state.
// Per-account sweeps already exclude it — every account scan in the
// repo filters status='ACTIVE' (margin/VIP/fee scans included).
package accounts

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"exchange/pkg/decimal"
)

// defaultClosureID mints the "aclose_<28urlsafe>" public id when no
// custom generator is wired (test hook).
func defaultClosureID() (string, error) {
	raw := make([]byte, 21)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return "aclose_" + base64.RawURLEncoding.EncodeToString(raw), nil
}

// pendingFundingStatuses is the funding_transactions set that blocks
// closure — anything the rails may still move money on.
var pendingFundingStatuses = []string{
	"PENDING", "CONFIRMED", "PENDING_REVIEW",
}

// ---------------------------------------------------------------------------
// Seams
// ---------------------------------------------------------------------------

// Sweeper sends one residual balance through the Phase-11 withdrawal
// pipeline and returns the durable funding reference. The composition
// adapter runs Create→Confirm→Release so the sweep is GL-balanced
// (CustomerLiability→ClearingTransit hold posting) and rail-dispatched
// exactly like a client withdrawal — no bespoke money movement.
type Sweeper interface {
	SweepWithdrawal(ctx context.Context, req SweepRequest) (*SweepResult, error)
}

// SweepRequest is one residual-sweep withdrawal.
type SweepRequest struct {
	AccountID      int64
	UserID         int64
	Currency       string
	Amount         string // decimal text — full available balance
	DestinationRef string // verified beneficiary ref_code
	IdempotencyKey string // "closure-sweep:{account}:{currency}"
}

// SweepResult is the pipeline's acceptance reference for the row.
type SweepResult struct {
	WithdrawalID int64  `json:"withdrawal_id"`
	Status       string `json:"status"`
}

// BeneficiaryLookup resolves the client's verified beneficiary for one
// currency when the request does not name a destination. ok=false
// means none is withdrawable today (VERIFIED + 24h hold enforced in
// the adapter); a distinct error means the lookup itself failed.
type BeneficiaryLookup interface {
	VerifiedBeneficiaryFor(ctx context.Context, accountID int64,
		currency string) (ref string, ok bool, err error)
}

// ---------------------------------------------------------------------------
// Service
// ---------------------------------------------------------------------------

// ClosurePreconditions is the blocking read persisted verbatim into
// account_closures.preconditions_snapshot.
type ClosurePreconditions struct {
	OpenPositions      int64            `json:"open_positions"`
	OpenOrders         int64            `json:"open_orders"`
	PendingSettlements int64            `json:"pending_settlements"`
	PendingFunding     int64            `json:"pending_funding"`
	LockedBalances     int64            `json:"locked_balances"`
	CheckedAt          time.Time        `json:"checked_at"`
	Balances           []ClosureBalance `json:"balances"`
}

// ClosureBalance is one balances-row snapshot at precondition time.
type ClosureBalance struct {
	Currency  string `json:"currency"`
	Available string `json:"available"`
	Locked    string `json:"locked"`
}

// ClosureRequest carries the client- or officer-initiated close.
type ClosureRequest struct {
	AccountID    int64
	UserID       int64  // authenticated session user (client path)
	Reason       string // mandatory, non-empty
	Destinations map[string]string
	Forced       bool  // true only from the dual-control executor
	Actor        int64 // placing officer (forced) — equals UserID on client path
	ApproverID   int64 // dual-control approver (forced)
	RequestRef   int64 // admin_dual_control_requests.id (forced)
	IP           string
	RequestID    string
}

// ClosureResult is the endpoint/executor payload.
type ClosureResult struct {
	ClosureID       string                `json:"closure_id"`
	AccountID       int64                 `json:"account_id"`
	Status          string                `json:"status"` // CLOSED
	Sweeps          []SweepResult         `json:"sweeps"`
	Preconditions   *ClosurePreconditions `json:"preconditions"`
	SessionsRevoked int                   `json:"sessions_revoked"`
	KeysRevoked     int                   `json:"api_keys_revoked"`
	PartialFailure  bool                  `json:"partial_failure"`
	Detail          string                `json:"detail,omitempty"`
}

// ClosureService runs the offboarding pipeline over existing seams —
// the only new machinery is the precondition read, the durable row and
// the CLOSED transition; every effect underneath (cancel, close,
// withdrawal, session/key revocation, audit) reuses prior phases.
type ClosureService struct {
	pool     *pgxpool.Pool
	disp     OrderDispatcher // forced path only — may be nil
	sweeper  Sweeper
	bens     BeneficiaryLookup
	sessions SessionTerminator
	keys     CredentialRevoker
	orders   OpenOrderLister
	alerter  FreezeAlerter
	notify   UserNotifier
	newID    func() (string, error)
	now      func() time.Time
}

// NewClosureService wires the pool plus existing seams; sweeper+bens
// are mandatory for the client path (forced executor degrades to
// blocked when either is nil — fail closed, never a partial sweep).
func NewClosureService(pool *pgxpool.Pool, disp OrderDispatcher,
	sweeper Sweeper, bens BeneficiaryLookup,
	sessions SessionTerminator, keys CredentialRevoker,
	orders OpenOrderLister, alerter FreezeAlerter, notify UserNotifier,
	newID func() (string, error)) *ClosureService {
	if newID == nil {
		newID = defaultClosureID
	}
	return &ClosureService{pool: pool, disp: disp, sweeper: sweeper,
		bens: bens, sessions: sessions, keys: keys, orders: orders,
		alerter: alerter, notify: notify, newID: newID, now: time.Now}
}

// ---------------------------------------------------------------------------
// Client path — POST /api/v1/account/close (2FA-wrapped route)
// ---------------------------------------------------------------------------

// Close runs the client-initiated closure. Only the account owner may
// close the account (same rule as emergency self-freeze). The route's
// RequireTwoFactor middleware is the 2FA gate — this layer trusts the
// session elevation the middleware verified.
func (s *ClosureService) Close(ctx context.Context,
	req ClosureRequest) (*ClosureResult, error) {
	if req.Reason == "" {
		return nil, newError(CodeInvalidRequest, "reason is required")
	}
	var owner int64
	var status string
	if err := s.pool.QueryRow(ctx,
		`SELECT user_id, status FROM accounts WHERE id = $1`,
		req.AccountID).Scan(&owner, &status); err != nil {
		if err == pgx.ErrNoRows {
			return nil, newError(CodeNotFound, "account not found")
		}
		return nil, errorf("INTERNAL_ERROR", "account lookup: %v", err)
	}
	if owner != req.UserID {
		return nil, newError(CodeForbidden,
			"account closure requires the account owner session")
	}
	if status == string(StatusClosed) {
		return nil, newError(CodeInvalidRequest, "account is already closed")
	}
	if status != string(StatusActive) {
		return nil, errorf(CodeInvalidRequest,
			"cannot close account in status %s — resolve holds first", status)
	}
	return s.runPipeline(ctx, req, owner)
}

// ---------------------------------------------------------------------------
// Forced path — admin.OpAccountClosure dual-control executor
// ---------------------------------------------------------------------------

// ForcedClose runs inside the dual-control approval transaction. The
// caller (admin executor) owns the tx lifecycle: any returned error
// rolls the approval back and leaves the request PENDING — retriable.
// The pipeline reuses ctx-scoped side effects (dispatcher, sweep) then
// commits the status flip + durable row through tx so approval and
// closure commit atomically.
func (s *ClosureService) ForcedClose(ctx context.Context, tx pgx.Tx,
	req ClosureRequest) (*ClosureResult, error) {
	if req.Reason == "" {
		return nil, newError(CodeInvalidRequest,
			"mandatory reason required for forced closure")
	}
	var owner int64
	var status string
	if err := tx.QueryRow(ctx,
		`SELECT user_id, status FROM accounts WHERE id = $1 FOR UPDATE`,
		req.AccountID).Scan(&owner, &status); err != nil {
		if err == pgx.ErrNoRows {
			return nil, newError(CodeNotFound, "account not found")
		}
		return nil, errorf("INTERNAL_ERROR", "account lookup: %v", err)
	}
	if status == string(StatusClosed) {
		return nil, newError(CodeInvalidRequest, "account is already closed")
	}
	// Forced path legal from ACTIVE/SUSPENDED/FROZEN — a compliance
	// hold escalate-to-closure arrives FROZEN by definition.
	if status != string(StatusActive) &&
		status != string(StatusSuspended) &&
		status != string(StatusFrozen) {
		return nil, errorf(CodeInvalidRequest,
			"cannot close account in status %s", status)
	}

	// Forced de-risking first: the officer cannot wait for the client
	// to flatten — mass-cancel then market-close through the shared
	// dispatcher. Failures block the closure (ACCOUNT_CLOSE_BLOCKED
	// with the residual counts below) — the desk resolves and retries
	// the PENDING request.
	if s.disp != nil {
		if _, err := s.disp.MassCancel(ctx, MassCancelScope{
			AccountID: req.AccountID, Reason: "ACCOUNT_CLOSURE",
		}); err != nil {
			return nil, errorf(CodeAccountCloseBlocked,
				"forced-closure mass-cancel failed: %v", err)
		}
		if err := s.closeOpenPositions(ctx, req); err != nil {
			return nil, err
		}
	}
	res, err := s.runPipelineTx(ctx, tx, req, owner)
	if err != nil {
		return nil, err
	}
	// Session + key revocation inside the executor — they land before
	// the approval commit; a commit failure merely forces re-login on
	// an account that stayed open (documented, recoverable).
	s.revokeAccess(ctx, req, owner, res)
	return res, nil
}

// ---------------------------------------------------------------------------
// Shared pipeline
// ---------------------------------------------------------------------------

// runPipeline is the non-tx client path: preconditions → sweep →
// close tx → post-commit revocation/notification/verify.
func (s *ClosureService) runPipeline(ctx context.Context,
	req ClosureRequest, owner int64) (*ClosureResult, error) {
	pre, err := s.preconditions(ctx, req.AccountID)
	if err != nil {
		return nil, err
	}
	if err := blocked(pre); err != nil {
		return nil, err
	}
	sweeps, err := s.sweepBalances(ctx, req, owner, pre)
	if err != nil {
		return nil, err
	}
	res, err := s.closeTx(ctx, nil, req, owner, pre, sweeps)
	if err != nil {
		return nil, err
	}
	s.revokeAccess(ctx, req, owner, res)
	s.postCommit(ctx, req, owner, res)
	return res, nil
}

// runPipelineTx is the forced path inside the approval tx: the
// precondition recount and close both run on tx so the approval and
// the closure commit atomically.
func (s *ClosureService) runPipelineTx(ctx context.Context, tx pgx.Tx,
	req ClosureRequest, owner int64) (*ClosureResult, error) {
	pre, err := s.preconditions(ctx, req.AccountID)
	if err != nil {
		return nil, err
	}
	if err := blocked(pre); err != nil {
		return nil, err
	}
	sweeps, err := s.sweepBalances(ctx, req, owner, pre)
	if err != nil {
		return nil, err
	}
	res, err := s.closeTx(ctx, tx, req, owner, pre, sweeps)
	if err != nil {
		return nil, err
	}
	s.postVerify(ctx, req, res)
	return res, nil
}

// preconditions counts every blocking artifact and snapshots balances.
func (s *ClosureService) preconditions(ctx context.Context,
	accountID int64) (*ClosurePreconditions, error) {
	p := &ClosurePreconditions{CheckedAt: s.now().UTC()}
	if err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM positions
		  WHERE account_id = $1 AND quantity <> 0`, accountID).
		Scan(&p.OpenPositions); err != nil {
		return nil, errorf("INTERNAL_ERROR", "position count: %v", err)
	}
	if err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM orders
		  WHERE account_id = $1 AND status = ANY($2)`,
		accountID, openOrderStatuses).Scan(&p.OpenOrders); err != nil {
		return nil, errorf("INTERNAL_ERROR", "open order count: %v", err)
	}
	if err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM settlement_instructions
		  WHERE account_id = $1 AND status = 'PENDING'`, accountID).
		Scan(&p.PendingSettlements); err != nil {
		return nil, errorf("INTERNAL_ERROR", "settlement count: %v", err)
	}
	if err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM funding_transactions
		  WHERE account_id = $1 AND status = ANY($2)`,
		accountID, pendingFundingStatuses).Scan(&p.PendingFunding); err != nil {
		return nil, errorf("INTERNAL_ERROR", "funding count: %v", err)
	}
	rows, err := s.pool.Query(ctx,
		`SELECT currency, available::text, locked::text
		   FROM balances WHERE account_id = $1
		   ORDER BY currency`, accountID)
	if err != nil {
		return nil, errorf("INTERNAL_ERROR", "balance snapshot: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var b ClosureBalance
		if err := rows.Scan(&b.Currency, &b.Available, &b.Locked); err != nil {
			return nil, errorf("INTERNAL_ERROR", "balance scan: %v", err)
		}
		p.Balances = append(p.Balances, b)
		if b.Locked != "0" && b.Locked != "0.00000000" {
			p.LockedBalances++
		}
	}
	if err := rows.Err(); err != nil {
		return nil, errorf("INTERNAL_ERROR", "balance snapshot: %v", err)
	}
	return p, nil
}

// blocked formats the ACCOUNT_CLOSE_BLOCKED rejection with every
// remaining blocker named — the response must tell the client exactly
// what to resolve.
func blocked(p *ClosurePreconditions) error {
	var reasons []string
	if p.OpenPositions > 0 {
		reasons = append(reasons,
			fmt.Sprintf("%d open position(s)", p.OpenPositions))
	}
	if p.OpenOrders > 0 {
		reasons = append(reasons,
			fmt.Sprintf("%d open order(s)", p.OpenOrders))
	}
	if p.PendingSettlements > 0 {
		reasons = append(reasons,
			fmt.Sprintf("%d pending settlement(s)", p.PendingSettlements))
	}
	if p.PendingFunding > 0 {
		reasons = append(reasons,
			fmt.Sprintf("%d pending funding transaction(s)", p.PendingFunding))
	}
	if p.LockedBalances > 0 {
		reasons = append(reasons,
			fmt.Sprintf("%d locked balance(s)", p.LockedBalances))
	}
	if len(reasons) == 0 {
		return nil
	}
	return errorf(CodeAccountCloseBlocked,
		"account closure blocked: %s remain", joinComma(reasons))
}

func joinComma(parts []string) string {
	out := parts[0]
	for _, p := range parts[1:] {
		out += ", " + p
	}
	return out
}

// sweepBalances drains every available balance through the withdrawal
// pipeline. A missing sweep seam or unresolvable beneficiary fails the
// close with ACCOUNT_CLOSE_BLOCKED — never a partial close.
func (s *ClosureService) sweepBalances(ctx context.Context,
	req ClosureRequest, owner int64,
	pre *ClosurePreconditions) ([]SweepResult, error) {
	out := []SweepResult{}
	for _, b := range pre.Balances {
		if b.Available == "" || b.Available == "0" || b.Available == "0.00000000" {
			continue
		}
		if s.sweeper == nil {
			return nil, errorf(CodeAccountCloseBlocked,
				"residual sweep pipeline unavailable for %s %s",
				b.Available, b.Currency)
		}
		dest := req.Destinations[b.Currency]
		if dest == "" {
			if s.bens == nil {
				return nil, errorf(CodeAccountCloseBlocked,
					"no destination for %s %s and no verified "+
						"beneficiary resolver", b.Available, b.Currency)
			}
			ref, ok, err := s.bens.VerifiedBeneficiaryFor(ctx,
				req.AccountID, b.Currency)
			if err != nil {
				return nil, errorf("INTERNAL_ERROR",
					"beneficiary lookup %s: %v", b.Currency, err)
			}
			if !ok {
				return nil, errorf(CodeAccountCloseBlocked,
					"no verified beneficiary for %s %s — provide "+
						"destinations.%s", b.Available, b.Currency, b.Currency)
			}
			dest = ref
		}
		res, err := s.sweeper.SweepWithdrawal(ctx, SweepRequest{
			AccountID:      req.AccountID,
			UserID:         owner,
			Currency:       b.Currency,
			Amount:         b.Available,
			DestinationRef: dest,
			IdempotencyKey: fmt.Sprintf("closure-sweep:%d:%s",
				req.AccountID, b.Currency),
		})
		if err != nil {
			return nil, errorf(CodeAccountCloseBlocked,
				"residual sweep for %s %s failed: %v",
				b.Available, b.Currency, err)
		}
		if res != nil {
			out = append(out, *res)
		}
	}
	return out, nil
}

// closeTx performs the atomic terminal transition. tx!=nil reuses the
// dual-control approval transaction (forced path); tx==nil opens its
// own (client path).
func (s *ClosureService) closeTx(ctx context.Context, tx pgx.Tx,
	req ClosureRequest, owner int64,
	pre *ClosurePreconditions, sweeps []SweepResult) (*ClosureResult, error) {
	own := tx == nil
	var err error
	if own {
		tx, err = s.pool.Begin(ctx)
		if err != nil {
			return nil, errorf("INTERNAL_ERROR", "begin tx: %v", err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
	}

	var prev string
	err = tx.QueryRow(ctx,
		`SELECT status FROM accounts WHERE id = $1 FOR UPDATE`,
		req.AccountID).Scan(&prev)
	if err == pgx.ErrNoRows {
		return nil, newError(CodeNotFound, "account not found")
	}
	if err != nil {
		return nil, errorf("INTERNAL_ERROR", "lock account: %v", err)
	}
	if prev == string(StatusClosed) {
		return nil, newError(CodeInvalidRequest, "account is already closed")
	}
	legal := map[string]bool{
		string(StatusActive):    true,
		string(StatusSuspended): req.Forced,
		string(StatusFrozen):    req.Forced,
	}
	if !legal[prev] {
		return nil, errorf(CodeInvalidRequest,
			"cannot close account in status %s", prev)
	}

	closureID, err := s.newID()
	if err != nil {
		return nil, errorf("INTERNAL_ERROR", "closure id: %v", err)
	}
	snap, err := json.Marshal(pre)
	if err != nil {
		return nil, errorf("INTERNAL_ERROR", "snapshot marshal: %v", err)
	}
	sweepRefs, err := json.Marshal(sweeps)
	if err != nil {
		return nil, errorf("INTERNAL_ERROR", "sweep refs marshal: %v", err)
	}

	if _, err := tx.Exec(ctx,
		`UPDATE accounts SET status = 'CLOSED', updated_at = now()
		  WHERE id = $1`, req.AccountID); err != nil {
		return nil, errorf("INTERNAL_ERROR", "close account: %v", err)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO account_closures
		     (closure_id, account_id, user_id, reason, forced,
		      initiated_by, approved_by, dual_control_request_id,
		      preconditions_snapshot, sweep_refs, status, completed_at)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,'COMPLETED',now())`,
		closureID, req.AccountID, owner, req.Reason, req.Forced,
		initiator(req), approver(req), requestRef(req),
		snap, sweepRefs); err != nil {
		return nil, errorf("INTERNAL_ERROR", "closure record: %v", err)
	}
	meta, _ := json.Marshal(map[string]any{
		"status": "CLOSED", "closure_id": closureID, "forced": req.Forced,
		"sweep_count": len(sweeps),
	})
	if _, err := tx.Exec(ctx,
		`INSERT INTO admin_audit_log
		     (admin_user_id, action, target_type, target_id, after_state, ip_address)
		 VALUES ($1,'account.close','account',$2,$3,NULLIF($4,'')::inet)`,
		initiator(req), req.AccountID, meta, req.IP); err != nil {
		return nil, errorf("INTERNAL_ERROR", "audit insert: %v", err)
	}
	if own {
		if err := tx.Commit(ctx); err != nil {
			return nil, errorf("INTERNAL_ERROR", "commit close: %v", err)
		}
	}
	return &ClosureResult{
		ClosureID: closureID, AccountID: req.AccountID,
		Status: string(StatusClosed), Sweeps: sweeps,
		Preconditions: pre,
	}, nil
}

func initiator(req ClosureRequest) int64 {
	if req.Actor != 0 {
		return req.Actor
	}
	return req.UserID
}

func approver(req ClosureRequest) any {
	if req.ApproverID == 0 {
		return nil
	}
	return req.ApproverID
}

func requestRef(req ClosureRequest) any {
	if req.RequestRef == 0 {
		return nil
	}
	return req.RequestRef
}

// closeOpenPositions market-closes every residual open position for
// the forced path (positions found after the mass-cancel).
func (s *ClosureService) closeOpenPositions(ctx context.Context,
	req ClosureRequest) error {
	rows, err := s.pool.Query(ctx,
		`SELECT p.id, p.instrument_id, i.symbol, p.side, p.quantity::text
		   FROM positions p JOIN instruments i ON i.id = p.instrument_id
		  WHERE p.account_id = $1 AND p.quantity <> 0`, req.AccountID)
	if err != nil {
		return errorf("INTERNAL_ERROR", "position list: %v", err)
	}
	type pos struct {
		id, instID int64
		symbol     string
		side       string
		qty        string
	}
	var poss []pos
	for rows.Next() {
		var p pos
		if err := rows.Scan(&p.id, &p.instID, &p.symbol, &p.side, &p.qty); err != nil {
			rows.Close()
			return errorf("INTERNAL_ERROR", "position scan: %v", err)
		}
		poss = append(poss, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return errorf("INTERNAL_ERROR", "position list: %v", err)
	}
	for _, p := range poss {
		qty, err := decimal.NewFromString(p.qty)
		if err != nil {
			return errorf("INTERNAL_ERROR", "position qty: %v", err)
		}
		var last error
		for i := 0; i < EmergencyCancelAttempts && ctx.Err() == nil; i++ {
			var ack *OrderAck
			ack, last = s.disp.SubmitClose(ctx, CloseOrderRequest{
				AccountID: req.AccountID, InstrumentID: p.instID,
				Side:           closeSide(p.side),
				Quantity:       qty,
				ReduceOnly:     true,
				MaxSlippageBps: 25,
				ClientOrderID: fmt.Sprintf("aclose-%d-%d-%d",
					req.AccountID, p.id, s.now().UnixMilli()),
			})
			if last == nil && ack != nil && ack.Accepted {
				break
			}
		}
		if last != nil {
			return errorf(CodeAccountCloseBlocked,
				"forced-closure position close failed (%s %s): %v",
				p.symbol, p.side, last)
		}
	}
	return nil
}

// revokeAccess drops every session and API key — the closed account
// keeps read-only history but loses all write/auth surface.
func (s *ClosureService) revokeAccess(ctx context.Context,
	req ClosureRequest, owner int64, res *ClosureResult) {
	if s.sessions == nil {
		res.PartialFailure = true
		res.Detail = appendDetail(res.Detail, "session terminator not wired")
	} else if n, err := s.sessions.RevokeAllExcept(ctx, req.AccountID,
		fmt.Sprint(owner), ""); err != nil {
		res.PartialFailure = true
		res.Detail = appendDetail(res.Detail,
			"session revocation: "+err.Error())
	} else {
		res.SessionsRevoked = n
	}
	if s.keys == nil {
		res.PartialFailure = true
		res.Detail = appendDetail(res.Detail, "credential revoker not wired")
	} else if n, err := s.keys.RevokeAllKeys(ctx, req.AccountID,
		"account_closed"); err != nil {
		res.PartialFailure = true
		res.Detail = appendDetail(res.Detail,
			"api key revocation: "+err.Error())
	} else {
		res.KeysRevoked = n
	}
}

// postCommit notifies the owner and verifies nothing raced into the
// gap between preconditions and the CLOSED commit (fail-loud).
func (s *ClosureService) postCommit(ctx context.Context,
	req ClosureRequest, owner int64, res *ClosureResult) {
	s.postVerify(ctx, req, res)
	if s.notify != nil {
		s.notify(ctx, owner, "security_alert", map[string]any{
			"event":      "account.closed",
			"account_id": req.AccountID,
			"closure_id": res.ClosureID,
			"forced":     req.Forced,
			"message":    "account closed — access revoked; records retained read-only",
		})
	}
	if res.PartialFailure {
		s.raiseClosureAlert(ctx, req.AccountID, "CLOSURE_PARTIAL",
			"account closure completed with a partial failure: "+res.Detail)
	}
}

// postVerify recounts blockers after commit; a non-zero count means an
// order or position landed during the sweep window — page the desk.
func (s *ClosureService) postVerify(ctx context.Context,
	req ClosureRequest, res *ClosureResult) {
	post, err := s.preconditions(ctx, req.AccountID)
	if err != nil {
		s.raiseClosureAlert(ctx, req.AccountID, "CLOSURE_VERIFY_FAILED",
			"post-closure verification unreadable: "+err.Error())
		return
	}
	if post.OpenPositions+post.OpenOrders > 0 {
		s.raiseClosureAlert(ctx, req.AccountID, "CLOSURE_RESIDUAL",
			fmt.Sprintf("closed account %d has %d open position(s), "+
				"%d open order(s) — manual desk cleanup required",
				req.AccountID, post.OpenPositions, post.OpenOrders))
	}
}

func (s *ClosureService) raiseClosureAlert(ctx context.Context,
	accountID int64, code, summary string) {
	if s.alerter == nil {
		return
	}
	_ = s.alerter.Raise(ctx, FreezeAlert{
		Severity: "P1", Code: code, AccountID: accountID,
		Summary: summary, Details: map[string]string{},
	})
}
