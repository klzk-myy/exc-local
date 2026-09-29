// Cooling-off / self-exclusion — Phase-14 Task 14.3.11 + Task 14.3.12.
//
// POST /api/v1/account/cooling-off activates a strictly irrevocable
// self-exclusion window ({1d,3d,7d,30d}, acknowledged:true required).
// Activation runs the Task 14.3.12 de-risking saga:
//
//  1. insert the immutable cooling_off_periods row — the flag lands
//     FIRST so leveraged order entry rejects (COOLING_OFF_ACTIVE) even
//     when the downstream machinery is unreachable (fail closed);
//  2. mass-cancel every resting order on leveraged instruments across
//     the user's non-SPOT accounts (existing OrderDispatcher seam,
//     emergency-freeze retry budget of 3);
//  3. market-close every open leveraged position (reduce-only close
//     with slippage protection, same machinery as close-all);
//  4. residual failures page P1 — a position-close failure is
//     fail-loud, never silent.
//
// Spot conversions and withdrawals stay available: the admission gate
// (orders.Options.CoolingOff → AssertLeverageEntryAllowed) rejects only
// margin-account order entry on marginable instruments, and the
// funding path sees no status change.
//
// Irrevocability is contractual, not just DB-shaped: this type exposes
// no cancel/shorten/update methods and no HTTP route mutates a period.
package accounts

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"exchange/pkg/decimal"
)

// openOrderStatuses mirrors orders.OpenStatuses verbatim — this package
// cannot import orders (orders imports accounts for the dispatcher
// seam), so the resting-on-book status set is restated here. The two
// must stay identical: orders/types.go is the owner.
var openOrderStatuses = []string{
	"PENDING", "RESERVED", "ACTIVE", "PARTIALLY_FILLED",
}

// coolingOffDurations maps the four regulated window choices to their
// lengths — the DB CHECK constrains duration to the same set.
var coolingOffDurations = map[string]time.Duration{
	"1d":  24 * time.Hour,
	"3d":  72 * time.Hour,
	"7d":  7 * 24 * time.Hour,
	"30d": 30 * 24 * time.Hour,
}

// CoolingOffPeriod is one active-or-elapsed self-exclusion window.
type CoolingOffPeriod struct {
	ID             int64     `json:"id"`
	UserID         int64     `json:"user_id"`
	Duration       string    `json:"duration"` // "1d"|"3d"|"7d"|"30d"
	StartedAt      time.Time `json:"started_at"`
	ExpiresAt      time.Time `json:"expires_at"`
	AcknowledgedAt time.Time `json:"acknowledged_at"`
}

// CoolingOffRequest is the activation input (duration + explicit
// acknowledgement — the handler requires acknowledged:true).
type CoolingOffRequest struct {
	AccountID    int64
	Duration     string
	Acknowledged bool
	Actor        string // session user id for audit/notifications
	IP           string
	RequestID    string
}

// CoolingOffResult reports the de-risking saga outcome.
type CoolingOffResult struct {
	OrdersCancelled int           `json:"orders_cancelled"`
	Closes          []CloseResult `json:"closes"`
	PartialFailure  bool          `json:"partial_failure"`
	Detail          string        `json:"detail,omitempty"`
}

// UserNotifier emits one user-facing notification, best-effort — a
// delivery failure must never block the caller's state transition.
// Wire notifications.Service.Notify via an adapter at composition;
// the event token must come from its vocabulary ("security_alert"
// for account-state notifications).
type UserNotifier func(ctx context.Context, userID int64, event string,
	payload map[string]any)

// CoolingOffService activates windows and answers the order-admission
// gate. OrderDispatcher is the same seam FreezeService/EmergencyFreeze
// use; FreezeAlerter carries the fail-loud P1 for partial saga results.
type CoolingOffService struct {
	pool    *pgxpool.Pool
	disp    OrderDispatcher
	alerter FreezeAlerter
	notify  UserNotifier
	now     func() time.Time
}

// NewCoolingOffService wires the pool plus the existing dispatcher /
// P1-alerter / notification seams (alerter+notify may be nil — a nil
// dispatcher means the saga reports total partial failure, which the
// alert then carries; the flag itself is always durable first).
func NewCoolingOffService(pool *pgxpool.Pool, disp OrderDispatcher,
	alerter FreezeAlerter, notify UserNotifier) *CoolingOffService {
	return &CoolingOffService{pool: pool, disp: disp, alerter: alerter,
		notify: notify, now: time.Now}
}

// Activate validates, persists the irrevocable window, then runs the
// leveraged de-risking saga. An already-active window replays
// idempotently (same period, no second saga — retry-safe). Saga
// failures never undo the flag: they surface in result.PartialFailure
// and raise a P1 alert.
func (s *CoolingOffService) Activate(ctx context.Context,
	req CoolingOffRequest) (*CoolingOffPeriod, *CoolingOffResult, error) {
	dur, ok := coolingOffDurations[req.Duration]
	if !ok {
		return nil, nil, newError(CodeInvalidRequest,
			"duration must be one of 1d, 3d, 7d, 30d")
	}
	if !req.Acknowledged {
		return nil, nil, newError(CodeInvalidRequest,
			"acknowledged:true is required — cooling-off is irrevocable")
	}

	// Resolve owner + state. CLOSED accounts cannot start a window.
	var userID int64
	var status string
	if err := s.pool.QueryRow(ctx,
		`SELECT user_id, status FROM accounts WHERE id = $1`,
		req.AccountID).Scan(&userID, &status); err != nil {
		if err == pgx.ErrNoRows {
			return nil, nil, newError(CodeNotFound, "account not found")
		}
		return nil, nil, errorf("INTERNAL_ERROR", "account lookup: %v", err)
	}
	if status == "CLOSED" {
		return nil, nil, newError(CodeInvalidRequest,
			"closed accounts cannot start a cooling-off period")
	}

	// Idempotent replay — a live window returns without a second saga.
	if p, err := s.activeForUser(ctx, userID); err != nil {
		return nil, nil, errorf("INTERNAL_ERROR", "cooling-off lookup: %v", err)
	} else if p != nil {
		return p, nil, nil
	}

	started := s.now().UTC()
	p := &CoolingOffPeriod{
		UserID:         userID,
		Duration:       req.Duration,
		StartedAt:      started,
		ExpiresAt:      started.Add(dur),
		AcknowledgedAt: started,
	}
	if err := s.pool.QueryRow(ctx,
		`INSERT INTO cooling_off_periods
		     (user_id, duration, started_at, expires_at, acknowledged_at)
		 VALUES ($1, $2::interval, $3, $4, $5)
		 RETURNING id`,
		userID, intervalString(dur), started, p.ExpiresAt,
		p.AcknowledgedAt).Scan(&p.ID); err != nil {
		return nil, nil, errorf("INTERNAL_ERROR", "cooling-off insert: %v", err)
	}

	// Saga — the flag is durable above, so any dispatch failure leaves
	// the account fail-closed (entry blocked) rather than unprotected.
	res := s.deriskLeveraged(ctx, userID, req)
	if res.PartialFailure {
		s.raiseAlert(ctx, req.AccountID, "COOLING_OFF_PARTIAL",
			"cooling-off de-risking incomplete — leveraged positions may "+
				"remain open: "+res.Detail, nil, nil)
	}
	if s.notify != nil {
		s.notify(ctx, userID, "security_alert", map[string]any{
			"event":      "cooling_off.activated",
			"account_id": req.AccountID,
			"period_id":  p.ID,
			"expires_at": p.ExpiresAt.Format(time.RFC3339),
			"message": "self-exclusion active — leveraged order entry " +
				"blocked; spot conversions and withdrawals remain " +
				"available; the period cannot be cancelled or shortened",
		})
	}
	return p, res, nil
}

// deriskLeveraged cancels resting orders on leveraged instruments and
// market-closes leveraged positions for every non-SPOT account owned
// by the user. Each op retries ≤3 (emergency-freeze retry budget);
// failures aggregate into PartialFailure rather than aborting siblings.
func (s *CoolingOffService) deriskLeveraged(ctx context.Context,
	userID int64, req CoolingOffRequest) *CoolingOffResult {
	res := &CoolingOffResult{}
	if s.disp == nil {
		res.PartialFailure = true
		res.Detail = "order dispatcher not wired"
		return res
	}
	rows, err := s.pool.Query(ctx,
		`SELECT id, account_type FROM accounts
		  WHERE user_id = $1 AND status <> 'CLOSED'`, userID)
	if err != nil {
		res.PartialFailure = true
		res.Detail = "account list: " + err.Error()
		return res
	}
	var accts []struct {
		id  int64
		typ string
	}
	for rows.Next() {
		var a struct {
			id  int64
			typ string
		}
		if err := rows.Scan(&a.id, &a.typ); err != nil {
			rows.Close()
			res.PartialFailure = true
			res.Detail = "account scan: " + err.Error()
			return res
		}
		accts = append(accts, a)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		res.PartialFailure = true
		res.Detail = "account list: " + err.Error()
		return res
	}

	for _, a := range accts {
		if a.typ == "SPOT" {
			continue // spot-only accounts carry no leveraged exposure
		}
		s.cancelLeveragedOrders(ctx, a.id, res)
		s.closeLeveragedPositions(ctx, a.id, req, res)
	}
	return res
}

// cancelLeveragedOrders mass-cancels per instrument — only instruments
// with a non-zero leverage cap are touched; orders on purely spot
// instruments are never cancelled by self-exclusion.
func (s *CoolingOffService) cancelLeveragedOrders(ctx context.Context,
	accountID int64, res *CoolingOffResult) {
	instRows, err := s.pool.Query(ctx,
		`SELECT DISTINCT o.instrument_id
		   FROM orders o JOIN instruments i ON i.id = o.instrument_id
		  WHERE o.account_id = $1 AND o.status = ANY($2)
		    AND i.max_leverage > 0`,
		accountID, openOrderStatuses)
	if err != nil {
		res.PartialFailure = true
		res.Detail = appendDetail(res.Detail,
			"leveraged open-order list: "+err.Error())
		return
	}
	var instIDs []int64
	for instRows.Next() {
		var id int64
		if err := instRows.Scan(&id); err != nil {
			instRows.Close()
			res.PartialFailure = true
			res.Detail = appendDetail(res.Detail,
				"instrument scan: "+err.Error())
			return
		}
		instIDs = append(instIDs, id)
	}
	instRows.Close()
	for _, instID := range instIDs {
		var last error
		for i := 0; i < EmergencyCancelAttempts && ctx.Err() == nil; i++ {
			var mc *MassCancelResult
			mc, last = s.disp.MassCancel(ctx, MassCancelScope{
				AccountID:    accountID,
				InstrumentID: instID,
				Reason:       "COOLING_OFF",
			})
			if last == nil {
				if mc != nil {
					res.OrdersCancelled += mc.Cancelled
				}
				break
			}
		}
		if last != nil {
			res.PartialFailure = true
			res.Detail = appendDetail(res.Detail, fmt.Sprintf(
				"mass-cancel acct %d inst %d: %v", accountID, instID, last))
		}
	}
}

// closeLeveragedPositions submits reduce-only market closes for every
// open position on a marginable instrument (a position on a non-SPOT
// account whose instrument carries a leverage cap is leveraged by
// construction — same test checkBalance applies for margin).
func (s *CoolingOffService) closeLeveragedPositions(ctx context.Context,
	accountID int64, req CoolingOffRequest, res *CoolingOffResult) {
	posRows, err := s.pool.Query(ctx,
		`SELECT p.id, p.account_id, p.instrument_id, i.symbol, p.side,
		        p.quantity::text, p.entry_price::text
		   FROM positions p JOIN instruments i ON i.id = p.instrument_id
		  WHERE p.account_id = $1 AND p.quantity <> 0
		    AND i.max_leverage > 0`, accountID)
	if err != nil {
		res.PartialFailure = true
		res.Detail = appendDetail(res.Detail,
			"leveraged position list: "+err.Error())
		return
	}
	var poss []OpenPosition
	for posRows.Next() {
		var p OpenPosition
		var qty, entry string
		if err := posRows.Scan(&p.ID, &p.AccountID, &p.InstrumentID,
			&p.Symbol, &p.Side, &qty, &entry); err != nil {
			posRows.Close()
			res.PartialFailure = true
			res.Detail = appendDetail(res.Detail,
				"position scan: "+err.Error())
			return
		}
		if p.Quantity, err = decimal.NewFromString(qty); err != nil {
			posRows.Close()
			res.PartialFailure = true
			res.Detail = appendDetail(res.Detail,
				"position quantity parse: "+err.Error())
			return
		}
		if p.EntryPrice, err = decimal.NewFromString(entry); err != nil {
			posRows.Close()
			res.PartialFailure = true
			res.Detail = appendDetail(res.Detail,
				"position entry price parse: "+err.Error())
			return
		}
		poss = append(poss, p)
	}
	posRows.Close()
	for _, p := range poss {
		cr := CloseResult{
			PositionID:   p.ID,
			InstrumentID: p.InstrumentID,
			Symbol:       p.Symbol,
			Side:         p.Side,
			Quantity:     p.Quantity,
		}
		var ack *OrderAck
		var last error
		for i := 0; i < EmergencyCancelAttempts && ctx.Err() == nil; i++ {
			ack, last = s.disp.SubmitClose(ctx, CloseOrderRequest{
				AccountID:      accountID,
				InstrumentID:   p.InstrumentID,
				Side:           closeSide(p.Side),
				Quantity:       p.Quantity,
				ReduceOnly:     true,
				MaxSlippageBps: 25, // conservative venue guard
				ClientOrderID: fmt.Sprintf("cooloff-%d-%d-%d",
					accountID, p.ID, s.now().UnixMilli()),
			})
			if last == nil && ack != nil && ack.Accepted {
				break
			}
		}
		switch {
		case last != nil:
			cr.Code = codeOf(last)
			cr.Detail = last.Error()
			res.PartialFailure = true
		case ack == nil || !ack.Accepted:
			cr.Code = CodeCloseAllPartialFailure
			if ack != nil {
				cr.Detail = ack.Detail
				cr.OrderID = ack.OrderID
				cr.ClientOrderID = ack.ClientOrderID
			}
			res.PartialFailure = true
		default:
			cr.OK = true
			cr.OrderID = ack.OrderID
			cr.ClientOrderID = ack.ClientOrderID
		}
		res.Closes = append(res.Closes, cr)
	}
}

// ---------------------------------------------------------------------------
// Order-admission gate — Task 14.3.12 "account flag" read
// ---------------------------------------------------------------------------

// AssertLeverageEntryAllowed is the orders.Options.CoolingOff seam
// (satisfies the orders-side gate interface structurally — no import).
// It rejects with COOLING_OFF_ACTIVE while a live window covers the
// account's owner. Lookup failures reject with SERVICE_DEGRADED —
// leveraged entry on unverifiable self-exclusion state fails closed
// (spec §2.7).
func (s *CoolingOffService) AssertLeverageEntryAllowed(ctx context.Context,
	accountID int64) error {
	p, err := s.activeForAccount(ctx, accountID)
	if err != nil {
		return errorf("SERVICE_DEGRADED",
			"cooling-off state unverifiable — leveraged order rejected: %v", err)
	}
	if p != nil {
		return errorf(CodeCoolingOffActive,
			"self-exclusion active until %s — leveraged order entry blocked",
			p.ExpiresAt.Format(time.RFC3339))
	}
	return nil
}

// Status returns the user's live window, nil when none is active —
// handler/test read seam (no mutation surface exists anywhere).
func (s *CoolingOffService) Status(ctx context.Context,
	userID int64) (*CoolingOffPeriod, error) {
	p, err := s.activeForUser(ctx, userID)
	if err != nil {
		return nil, errorf("INTERNAL_ERROR", "cooling-off lookup: %v", err)
	}
	return p, nil
}

func (s *CoolingOffService) activeForAccount(ctx context.Context,
	accountID int64) (*CoolingOffPeriod, error) {
	var userID int64
	if err := s.pool.QueryRow(ctx,
		`SELECT user_id FROM accounts WHERE id = $1`,
		accountID).Scan(&userID); err != nil {
		return nil, fmt.Errorf("account user lookup: %w", err)
	}
	return s.activeForUser(ctx, userID)
}

func (s *CoolingOffService) activeForUser(ctx context.Context,
	userID int64) (*CoolingOffPeriod, error) {
	var p CoolingOffPeriod
	var iv string
	err := s.pool.QueryRow(ctx,
		`SELECT id, user_id, duration::text, started_at, expires_at,
		        acknowledged_at
		   FROM cooling_off_periods
		  WHERE user_id = $1 AND expires_at > now()
		  ORDER BY expires_at DESC LIMIT 1`, userID).
		Scan(&p.ID, &p.UserID, &iv, &p.StartedAt, &p.ExpiresAt,
			&p.AcknowledgedAt)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("cooling-off lookup: %w", err)
	}
	for label, d := range coolingOffDurations {
		if intervalString(d) == iv {
			p.Duration = label
			break
		}
	}
	return &p, nil
}

// intervalString renders the duration in day units so it round-trips
// through Postgres interval text ("7 days"), matching both the CHECK
// constraint literals and the map-key lookup above.
func intervalString(d time.Duration) string {
	return fmt.Sprintf("%d days", int(d/(24*time.Hour)))
}

func (s *CoolingOffService) raiseAlert(ctx context.Context, accountID int64,
	code, summary string, cause error, orderIDs []int64) {
	if s.alerter == nil {
		return
	}
	a := FreezeAlert{Severity: "P1", Code: code, AccountID: accountID,
		Summary: summary, Details: map[string]string{}}
	if cause != nil {
		a.Details["error"] = cause.Error()
	}
	_ = s.alerter.Raise(ctx, a)
}
