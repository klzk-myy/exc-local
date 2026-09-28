// Task 5.3.31 — WS-trading adapter over the shared order pipeline.
//
// OrdersDispatcher routes the order.* action set through
// internal/orders.Service — the same validation → dedup → persist →
// engine-IPC path the REST order endpoints use (Tasks 5.3.3/5.3.22/
// 5.3.24/5.3.25/5.3.32/5.3.37/5.3.39). The WS transport adds only the
// request_id-correlated envelope and the 60s replay window; dispatch,
// stale-seq fencing, client_order_id dedup, batch atomicity and the
// rl:batch:{acct}:{sec} limiter live in orders.Service and behave
// identically on both transports.
package ws

import (
	"context"
	"encoding/json"
	"strconv"

	"exchange/internal/orders"

	excerrors "exchange/pkg/errors"
)

// requestIDCtxKey carries the client request_id into Dispatcher calls so
// the order pipeline can stamp order_audit.request_id.
type requestIDCtxKey struct{}

func withRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDCtxKey{}, id)
}

// RequestIDFrom returns the client request_id bound to a dispatch ctx.
func RequestIDFrom(ctx context.Context) string {
	if v, ok := ctx.Value(requestIDCtxKey{}).(string); ok {
		return v
	}
	return ""
}

// OrderService is the narrow slice of *orders.Service the WS trading
// surface needs — an interface so unit tests can fake it.
type OrderService interface {
	Submit(ctx context.Context, acct *orders.Account, req *orders.SubmitRequest) (*orders.Ack, error)
	Cancel(ctx context.Context, acct *orders.Account, orderID int64, actor, requestID, ip string) (*orders.Ack, error)
	Modify(ctx context.Context, acct *orders.Account, orderID int64, req *orders.ModifyRequest, actor, requestID, ip string) (*orders.Order, error)
	CancelReplace(ctx context.Context, acct *orders.Account, orderID int64, req *orders.CancelReplaceRequest, actor, requestID, ip string) (*orders.Order, error)
	AmendKeepPriority(ctx context.Context, acct *orders.Account, orderID int64, req *orders.KeepPriorityRequest, actor, requestID, ip string) (*orders.Order, error)
	BatchSubmit(ctx context.Context, acct *orders.Account, reqs []*orders.SubmitRequest, requestID, ip string) ([]orders.BatchResult, error)
	DryRun(ctx context.Context, acct *orders.Account, req *orders.SubmitRequest) (*orders.Preview, error)
	GetOrder(ctx context.Context, acct *orders.Account, orderID int64) (*orders.Order, error)
}

// AccountLookup resolves the session's account row for the pipeline.
// (*orders.PgStore).AccountByID satisfies it.
type AccountLookup func(ctx context.Context, accountID int64) (*orders.Account, error)

// NewOrdersDispatcher binds the shared order pipeline to the WS
// dispatcher seam. svc==nil or lookup==nil fails closed at call time.
func NewOrdersDispatcher(svc OrderService, lookup AccountLookup) Dispatcher {
	return &ordersDispatcher{svc: svc, lookup: lookup}
}

type ordersDispatcher struct {
	svc    OrderService
	lookup AccountLookup
}

func (a *ordersDispatcher) account(ctx context.Context, sess *Session) (*orders.Account, error) {
	if a.svc == nil || a.lookup == nil {
		return nil, excerrors.New("NOT_IMPLEMENTED", "order pipeline not wired")
	}
	if sess.AccountID == 0 {
		return nil, excerrors.New("UNAUTHORIZED",
			"order actions require an account-bound token")
	}
	acct, err := a.lookup(ctx, sess.AccountID)
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "account lookup", err)
	}
	if acct == nil {
		return nil, excerrors.New("ACCOUNT_NOT_FOUND", "account not found")
	}
	return acct, nil
}

func (a *ordersDispatcher) actor(sess *Session) string {
	return "account:" + strconv.FormatInt(sess.AccountID, 10)
}

// payloadOrderID extracts {"order_id": <uint64|string>} — decimals are
// accepted as JSON numbers or strings like the REST surface.
func payloadOrderID(raw json.RawMessage) (int64, error) {
	var body struct {
		OrderID json.RawMessage `json:"order_id"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return 0, excerrors.New("INVALID_REQUEST", "malformed payload")
	}
	if len(body.OrderID) == 0 {
		return 0, excerrors.New("INVALID_REQUEST", "order_id is required")
	}
	s := string(body.OrderID)
	if s[0] == '"' {
		var str string
		if err := json.Unmarshal(body.OrderID, &str); err != nil {
			return 0, excerrors.New("INVALID_REQUEST", "order_id must be an integer")
		}
		s = str
	}
	id, err := strconv.ParseInt(s, 10, 64)
	if err != nil || id <= 0 {
		return 0, excerrors.New("INVALID_REQUEST", "order_id must be a positive integer")
	}
	return id, nil
}

// Dispatch implements Dispatcher for the order.* action set.
func (a *ordersDispatcher) Dispatch(ctx context.Context, sess *Session,
	action string, params json.RawMessage) (*Result, error) {

	acct, err := a.account(ctx, sess)
	if err != nil {
		return nil, err
	}
	rid := RequestIDFrom(ctx)

	switch action {
	case "order.place", "order.test":
		sr, err := orders.ParseSubmit(params)
		if err != nil {
			return nil, err
		}
		sr.SessionID = sess.SessionID
		if action == "order.test" {
			pv, err := a.svc.DryRun(ctx, acct, sr)
			if err != nil {
				return nil, err
			}
			return &Result{Status: "ACK", Data: pv}, nil
		}
		ack, err := a.svc.Submit(ctx, acct, sr)
		if err != nil {
			return nil, err
		}
		return &Result{Status: "ACK", Data: ack}, nil

	case "order.cancel":
		orderID, err := payloadOrderID(params)
		if err != nil {
			return nil, err
		}
		ack, err := a.svc.Cancel(ctx, acct, orderID,
			a.actor(sess), rid, sess.RemoteIP)
		if err != nil {
			return nil, err
		}
		return &Result{Status: "ACK", Data: ack}, nil

	case "order.modify":
		orderID, err := payloadOrderID(params)
		if err != nil {
			return nil, err
		}
		mr, err := orders.ParseModify(params)
		if err != nil {
			return nil, err
		}
		o, err := a.svc.Modify(ctx, acct, orderID, mr,
			a.actor(sess), rid, sess.RemoteIP)
		if err != nil {
			return nil, err
		}
		return &Result{Status: "ACK", Data: o.View()}, nil

	case "order.cancelReplace":
		orderID, err := payloadOrderID(params)
		if err != nil {
			return nil, err
		}
		cr, err := orders.ParseCancelReplace(params)
		if err != nil {
			return nil, err
		}
		o, err := a.svc.CancelReplace(ctx, acct, orderID, cr,
			a.actor(sess), rid, sess.RemoteIP)
		if err != nil {
			return nil, err
		}
		return &Result{Status: "ACK", Data: o.View()}, nil

	case "order.amend.keepPriority":
		orderID, err := payloadOrderID(params)
		if err != nil {
			return nil, err
		}
		kp, err := orders.ParseKeepPriority(params)
		if err != nil {
			return nil, err
		}
		o, err := a.svc.AmendKeepPriority(ctx, acct, orderID, kp,
			a.actor(sess), rid, sess.RemoteIP)
		if err != nil {
			return nil, err
		}
		return &Result{Status: "ACK", Data: o.View()}, nil

	case "order.batch":
		var body struct {
			Orders []json.RawMessage `json:"orders"`
		}
		if err := json.Unmarshal(params, &body); err != nil || len(body.Orders) == 0 {
			return nil, excerrors.New("INVALID_REQUEST",
				"batch requires a non-empty orders array")
		}
		reqs := make([]*orders.SubmitRequest, 0, len(body.Orders))
		for i, raw := range body.Orders {
			sr, err := orders.ParseSubmit(raw)
			if err != nil {
				return nil, excerrors.New("INVALID_REQUEST",
					"orders["+strconv.Itoa(i)+"]: "+err.Error())
			}
			sr.SessionID = sess.SessionID
			reqs = append(reqs, sr)
		}
		results, err := a.svc.BatchSubmit(ctx, acct, reqs, rid, sess.RemoteIP)
		if err != nil {
			return nil, err
		}
		return &Result{Status: "ACK", Data: map[string]any{"results": results}}, nil

	case "order.status":
		orderID, err := payloadOrderID(params)
		if err != nil {
			return nil, err
		}
		o, err := a.svc.GetOrder(ctx, acct, orderID)
		if err != nil {
			return nil, err
		}
		return &Result{Status: "ACK", Data: o.View()}, nil
	}
	return nil, excerrors.New("INVALID_REQUEST", "unknown order action "+action)
}
