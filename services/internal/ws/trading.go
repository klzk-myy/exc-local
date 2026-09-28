package ws

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	excerrors "exchange/pkg/errors"
)

// Task 5.3.31 + spec §10.5 item 6 — interactive trading actions.
//
// Action → required §8.8 scope. The remediation-#35 extended action list
// is canonical (supersedes the original 5-action list); every action the
// spec enumerates is routed here so the owner dispatcher sees a stable
// surface.
var orderActionScope = map[string]string{
	"order.place":                "trade",
	"order.cancel":               "trade",
	"order.modify":               "trade",
	"order.batch":                "trade",
	"order.status":               "read",
	"order.countdown_cancel_all": "trade",
	"order.cancelReplace":        "trade",
	"order.amend.keepPriority":   "trade",
	"order.test":                 "trade",
}

// Result is the dispatcher's per-request outcome: Status "ACK"|"NACK"
// with free-form Data rendered verbatim into the response frame.
type Result struct {
	Status string `json:"-"`
	Data   any    `json:"-"`
}

// Dispatcher is the narrow seam into the order pipeline (Tasks
// 5.3.24/5.3.25, Phase-02 core via Aeron). Implementations must be
// fail-closed: never fabricate an ACK. Errors carrying a §23 code
// (*excerrors.Error) map straight into the error frame; anything else
// surfaces as INTERNAL_ERROR.
type Dispatcher interface {
	Dispatch(ctx context.Context, sess *Session, action string, params json.RawMessage) (*Result, error)
}

// dispatcherFunc adapts a plain function to Dispatcher (tests, thin
// adapters at wiring).
type DispatcherFunc func(ctx context.Context, sess *Session, action string, params json.RawMessage) (*Result, error)

// Dispatch implements Dispatcher.
func (f DispatcherFunc) Dispatch(ctx context.Context, sess *Session, action string, params json.RawMessage) (*Result, error) {
	return f(ctx, sess, action, params)
}

// countdownParams is the order.countdown_cancel_all request body
// (Task 5.3.33 contract: countdown_ms 0 disables, renew=false is a strict
// start).
type countdownParams struct {
	CountdownMs int64 `json:"countdown_ms"`
	Renew       bool  `json:"renew"`
}

// handleOrder routes one order.* frame: session gate → scope check →
// request_id dedup claim → bounded-concurrency dispatch → dedup
// completion. Everything after the claim runs on a dedicated goroutine
// so dispatch latency never blocks the read pump or market-data writes.
//
// Edge cases (task AC/SDD): an unauthenticated order frame is answered
// with the UNAUTHORIZED error frame and the socket closes 4019 — the
// task's "rejected 4019" contract; a duplicate request_id replays the
// stored frame verbatim; same id + different payload is
// IDEMPOTENCY_KEY_MISMATCH.
func (c *Conn) handleOrder(f clientFrame) error {
	scope, known := orderActionScope[f.Action]
	if !known {
		c.sendError(f.RequestID, f.Action, "INVALID_REQUEST", "unknown order action", 0)
		return nil
	}
	if f.RequestID == "" {
		c.sendError("", f.Action, "INVALID_REQUEST", "request_id is required", 0)
		return nil
	}
	if len(f.RequestID) > maxRequestIDLen {
		c.sendError(f.RequestID, f.Action, "INVALID_REQUEST", "request_id too long", 0)
		return nil
	}

	sess := c.session()
	if !sess.Authenticated {
		c.sendError(f.RequestID, f.Action, "UNAUTHORIZED",
			"authenticate before order actions", 0)
		// Task 5.3.31 edge case: unauthenticated trade frame → 4019.
		c.closeWith(CloseAuthExpired, DisconnectServer, "UNAUTHENTICATED_TRADE_FRAME")
		return errCloseRead
	}
	if !sess.hasScope(scope) {
		c.sendError(f.RequestID, f.Action, "INSUFFICIENT_SCOPE",
			scope+" scope required", 0)
		return nil
	}
	if len(f.Params) == 0 {
		c.sendError(f.RequestID, f.Action, "INVALID_REQUEST", "params is required", 0)
		return nil
	}

	key := DedupKey(sess.dedupNamespace(), f.RequestID)
	ph := PayloadHash(f.Action, f.Params)
	res, err := c.srv.cfg.Dedup.Begin(context.Background(), key, ph, c.srv.cfg.DedupWindow)
	if err != nil {
		c.sendError(f.RequestID, f.Action, "INTERNAL_ERROR", "dedup store failure", 0)
		return nil
	}
	switch res.State {
	case DedupReplay:
		c.enqueue(res.Replay)
		return nil
	case DedupMismatch:
		c.sendError(f.RequestID, f.Action, "IDEMPOTENCY_KEY_MISMATCH",
			"request_id reused with a different payload", 0)
		return nil
	case DedupInFlight:
		c.sendError(f.RequestID, f.Action, "IDEMPOTENCY_KEY_COLLISION",
			"request_id already in flight", 0)
		return nil
	}

	select {
	case c.sem <- struct{}{}:
	case <-c.done:
		return errCloseRead
	default:
		// Bound dispatch concurrency per connection — a conn saturated
		// with in-flight orders rejects fast instead of queueing forever.
		c.sendError(f.RequestID, f.Action, "WS_RATE_EXCEEDED",
			"too many concurrent order actions on this connection", 0)
		_ = c.srv.cfg.Dedup.Complete(context.Background(), key, ph, nil)
		return nil
	}

	go c.dispatchOrder(f, sess, key, ph)
	return nil
}

// dispatchOrder executes the claimed request and emits the terminal
// frame. Terminal frames (ACK/NACK and §23-coded business errors) are
// stored for replay; transient internal failures release the claim so a
// client retry can actually retry.
func (c *Conn) dispatchOrder(f clientFrame, sess Session, key, ph string) {
	defer func() { <-c.sem }()

	ctx, cancel := context.WithTimeout(
		withRequestID(context.Background(), f.RequestID),
		c.srv.cfg.DispatchTimeout)
	defer cancel()

	var data any
	status := "ACK"
	var emitErr *excerrors.Error

	if f.Action == "order.countdown_cancel_all" {
		if c.srv.cfg.Countdown == nil {
			emitErr = excerrors.New("NOT_IMPLEMENTED", "countdown cancel-all not wired")
		} else {
			var p countdownParams
			if err := json.Unmarshal(f.Params, &p); err != nil {
				emitErr = excerrors.New("INVALID_REQUEST", "malformed countdown params")
			} else {
				st, exp, err := c.srv.cfg.Countdown.Set(ctx, sess.AccountID, p.CountdownMs, p.Renew)
				if err != nil {
					emitErr = excerrors.New(codeOf(err), err.Error())
				} else {
					data = map[string]any{"server_time": st, "countdown_expiry": exp}
				}
			}
		}
	} else {
		if c.srv.cfg.Dispatcher == nil {
			emitErr = excerrors.New("NOT_IMPLEMENTED",
				"order pipeline not wired (Tasks 5.3.24/5.3.25)")
		} else {
			res, err := c.srv.cfg.Dispatcher.Dispatch(ctx, &sess, f.Action, f.Params)
			if err != nil {
				emitErr = excerrors.New(codeOf(err), err.Error())
				if errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil {
					emitErr = excerrors.New("CORE_TIMEOUT",
						"order dispatch deadline exceeded")
				}
			} else if res != nil {
				data = res.Data
				if res.Status != "" {
					status = res.Status
				}
			}
		}
	}

	// Marshal the terminal frame once — emitted to the client and,
	// unless the outcome was a transient/internal failure, stored for
	// dedup replay (a replayed internal error would be worse than letting
	// the retry re-dispatch).
	var frame []byte
	var merr error
	if emitErr != nil {
		frame, merr = marshalFrame(errorFrame{
			Type: "error", RequestID: f.RequestID, Action: f.Action,
			Error: emitErr.Code, Message: emitErr.Message,
			TsMs: c.srv.cfg.Now().UnixMilli(),
		})
	} else {
		frame, merr = marshalFrame(responseFrame{
			Type: "response", RequestID: f.RequestID, Action: f.Action,
			Status: status, Data: data, TsMs: c.srv.cfg.Now().UnixMilli(),
		})
	}
	if merr != nil {
		frame, _ = marshalFrame(errorFrame{
			Type: "error", RequestID: f.RequestID, Action: f.Action,
			Error: "INTERNAL_ERROR", Message: "response marshal failure",
			TsMs: c.srv.cfg.Now().UnixMilli(),
		})
		emitErr = excerrors.New("INTERNAL_ERROR", "response marshal failure")
	}

	stored := frame
	if emitErr != nil {
		switch emitErr.Code {
		case "INTERNAL_ERROR", "SERVICE_DEGRADED", "CORE_TIMEOUT",
			"GATEWAY_TIMEOUT_MATCHING_ENGINE", "NOT_IMPLEMENTED":
			stored = nil // release the claim — the retry re-dispatches
		}
	}
	if cerr := c.srv.cfg.Dedup.Complete(context.Background(), key, ph, stored); cerr != nil {
		c.srv.cfg.Logger.Warn("ws dedup complete failed",
			"key", key, "err", cerr)
	}
	c.enqueue(frame)
}

// actionList returns the supported order.* action set — used by tests
// and the /api/v1/meta surface.
func actionList() []string {
	out := make([]string, 0, len(orderActionScope))
	for a := range orderActionScope {
		out = append(out, a)
	}
	return out
}

// OrderActions exposes the canonical action set (lexicographic order is
// not guaranteed — callers sort if they need stability).
func OrderActions() []string {
	out := actionList()
	// sort inline to avoid importing sort for one call site
	for i := 0; i < len(out); i++ {
		for j := i + 1; j < len(out); j++ {
			if strings.Compare(out[j], out[i]) < 0 {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}
