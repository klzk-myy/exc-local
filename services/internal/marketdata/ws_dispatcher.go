// Task 6.3.10 — in-band request-response dispatcher (spec §10.5 item 6,
// §24 #253).
//
// Two request surfaces share this dispatcher:
//
//  1. `request` method routing: {"action":"request","id":N|"request_id":..,
//     "method":"time","params":{...}} → correlated ACK/NACK. Built-in
//     methods are registered below; Wave-2 producers register data-plane
//     methods (depth snapshot, klines history) via Server.RegisterMethod.
//  2. `order.*` interactive trading actions routed to the Order Gateway
//     pipeline through the ws.Dispatcher seam (Phase-05 Task 5.3.31
//     contract): session gate → scope check → request_id dedup claim →
//     bounded-concurrency dispatch on dedicated goroutines → dedup
//     completion. 500ms of silence from the order pipeline answers a
//     correlated CORE_TIMEOUT error frame.
//
// Write-path serialization is single-writer by construction: every
// response — dispatcher ACKs, market-data events, control frames — is
// marshaled and enqueued onto the conn's `out` channel consumed by the
// one writePump goroutine, so concurrent order ACKs and L2 frames can
// never interleave inside a WebSocket frame (Task 6.3.10 item 3).
package marketdata

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"exchange/internal/ws"

	excerrors "exchange/pkg/errors"
)

// MethodHandler executes one "request" method. params is the frame's
// `params` object (may be empty); the returned value is rendered
// verbatim into the response frame's `data`.
type MethodHandler func(ctx context.Context, sess *ws.Session, params json.RawMessage) (any, error)

// built-in methods — always available, no auth required.
func (s *Server) builtinMethods() map[string]MethodHandler {
	return map[string]MethodHandler{
		// {"action":"request","id":N,"method":"time"} → server clock in
		// epoch ms + RFC3339 — the §8.9 server-time surface over WS.
		"time": func(context.Context, *ws.Session, json.RawMessage) (any, error) {
			n := s.cfg.Now()
			return map[string]any{
				"server_time_ms": n.UnixMilli(),
				"server_time":    n.UTC().Format("2006-01-02T15:04:05.000000Z"),
			}, nil
		},
		// {"action":"request","id":N,"method":"ping"} → application-level
		// liveness probe distinct from the WS control ping.
		"ping": func(context.Context, *ws.Session, json.RawMessage) (any, error) {
			return map[string]any{"pong": true}, nil
		},
	}
}

// RegisterMethod installs a handler for {"action":"request",
// "method":"<name>"}. Wave-2 producers use it for in-band data requests
// (e.g. "depth" snapshot fetch). A name colliding with a built-in is
// rejected — the contract must not silently rebind.
func (s *Server) RegisterMethod(name string, h MethodHandler) error {
	if name == "" || strings.ContainsAny(name, " \t") {
		return excerrors.New("INVALID_REQUEST", "method name must be a single token")
	}
	if _, dup := s.builtinMethods()[name]; dup {
		return excerrors.New("INVALID_REQUEST", "method name reserved")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.methods == nil {
		s.methods = map[string]MethodHandler{}
	}
	if _, dup := s.methods[name]; dup {
		return excerrors.New("INVALID_REQUEST", "method already registered")
	}
	s.methods[name] = h
	return nil
}

func (s *Server) method(name string) MethodHandler {
	if h, ok := s.builtinMethods()[name]; ok {
		return h
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.methods[name]
}

// handleRequest routes {"action":"request","id":N,"method":".."}.
// Dispatch runs synchronously — request methods are read-only and cheap;
// heavyweight methods must be registered with their own internal
// bounding (or exposed as channels instead).
func (c *Conn) handleRequest(f clientFrame) {
	rid := f.rid()
	if rid == "" {
		c.sendError("", "request", "INVALID_REQUEST",
			"request_id or id is required", 0)
		return
	}
	if len(rid) > maxRequestIDLen {
		c.sendError(rid, "request", "INVALID_REQUEST", "request_id too long", 0)
		return
	}
	if f.Method == "" {
		c.sendError(rid, "request", "INVALID_REQUEST", "method is required", 0)
		return
	}
	h := c.srv.method(f.Method)
	if h == nil {
		c.sendError(rid, "request", "INVALID_REQUEST",
			"unknown method "+f.Method, 0)
		return
	}
	sess := c.session()
	data, err := h(context.Background(), &sess, f.Params)
	if err != nil {
		c.sendError(rid, "request", codeOf(err), err.Error(), 0)
		return
	}
	c.sendResponse(rid, "request", "ACK", map[string]any{
		"method": f.Method,
		"result": data,
	})
}

// ---------------------------------------------------------------------------
// order.* interactive trading dispatch (Phase-05 Task 5.3.31 contract,
// routed here per Task 6.3.10 item 1)
// ---------------------------------------------------------------------------

// orderActionScope maps each order.* action to its required §8.8 scope —
// the same table the gateway surface uses (ws.OrderActions owns the
// canonical enumeration; this map mirrors it for the scope lookup that
// must precede dispatch).
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

// handleOrder routes one order.* frame: session gate → scope check →
// request_id dedup claim → bounded-concurrency dispatch → dedup
// completion. Everything after the claim runs on a dedicated goroutine
// so dispatch latency never blocks the read pump or market-data writes
// (Task 6.3.10 item 2 concurrency isolation).
func (c *Conn) handleOrder(f clientFrame) error {
	scope, known := orderActionScope[f.Action]
	if !known {
		c.sendError(f.rid(), f.Action, "INVALID_REQUEST", "unknown order action", 0)
		return nil
	}
	rid := f.rid()
	if rid == "" {
		c.sendError("", f.Action, "INVALID_REQUEST", "request_id is required", 0)
		return nil
	}
	if len(rid) > maxRequestIDLen {
		c.sendError(rid, f.Action, "INVALID_REQUEST", "request_id too long", 0)
		return nil
	}

	sess := c.session()
	if !sess.Authenticated {
		c.sendError(rid, f.Action, "UNAUTHORIZED",
			"authenticate before order actions", 0)
		c.closeConn(CloseAuthExpired, "UNAUTHENTICATED_TRADE_FRAME")
		return errCloseRead
	}
	if !hasScope(sess, scope) {
		c.sendError(rid, f.Action, "INSUFFICIENT_SCOPE", scope+" scope required", 0)
		return nil
	}
	if len(f.Params) == 0 {
		c.sendError(rid, f.Action, "INVALID_REQUEST", "params is required", 0)
		return nil
	}

	key := ws.DedupKey(dedupNamespace(sess), rid)
	ph := ws.PayloadHash(f.Action, f.Params)
	res, err := c.srv.cfg.Dedup.Begin(context.Background(), key, ph, c.srv.cfg.DedupWindow)
	if err != nil {
		c.sendError(rid, f.Action, "INTERNAL_ERROR", "dedup store failure", 0)
		return nil
	}
	switch res.State {
	case ws.DedupReplay:
		c.enqueue(res.Replay)
		return nil
	case ws.DedupMismatch:
		c.sendError(rid, f.Action, "IDEMPOTENCY_KEY_MISMATCH",
			"request_id reused with a different payload", 0)
		return nil
	case ws.DedupInFlight:
		c.sendError(rid, f.Action, "IDEMPOTENCY_KEY_COLLISION",
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
		c.sendError(rid, f.Action, "WS_RATE_EXCEEDED",
			"too many concurrent order actions on this connection", 0)
		_ = c.srv.cfg.Dedup.Complete(context.Background(), key, ph, nil)
		return nil
	}

	go c.dispatchOrder(f, sess, key, ph)
	return nil
}

// dispatchOrder executes the claimed request and emits the terminal
// frame. Terminal frames (ACK/NACK and §23-coded business errors) are
// stored for dedup replay; transient internal failures release the claim
// so a client retry can actually retry. A silent pipeline past
// DispatchTimeout answers CORE_TIMEOUT (Task 6.3.10 item 5).
func (c *Conn) dispatchOrder(f clientFrame, sess ws.Session, key, ph string) {
	defer func() { <-c.sem }()

	ctx, cancel := context.WithTimeout(
		withRequestID(context.Background(), f.rid()),
		c.srv.cfg.DispatchTimeout)
	defer cancel()

	var data any
	status := "ACK"
	var emitErr *excerrors.Error

	if c.srv.cfg.Dispatcher == nil {
		emitErr = excerrors.New("NOT_IMPLEMENTED",
			"order pipeline not wired (Phase-05 Task 5.3.31 seam)")
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

	var frame []byte
	var merr error
	if emitErr != nil {
		frame, merr = marshalFrame(errorFrame{
			Type: "error", RequestID: f.rid(), Action: f.Action,
			Error: emitErr.Code, Message: emitErr.Message,
			TsMs: c.srv.cfg.Now().UnixMilli(),
		})
	} else {
		frame, merr = marshalFrame(responseFrame{
			Type: "response", RequestID: f.rid(), Action: f.Action,
			Status: status, Data: data, TsMs: c.srv.cfg.Now().UnixMilli(),
		})
	}
	if merr != nil {
		frame, _ = marshalFrame(errorFrame{
			Type: "error", RequestID: f.rid(), Action: f.Action,
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
		c.srv.cfg.Logger.Warn("marketdata: dedup complete failed",
			"key", key, "err", cerr)
	}
	c.enqueue(frame)
}

// dedupNamespace scopes the request_id dedup window per Task 5.3.42
// (account-scoped keys, global unscoped keys prohibited) — same key
// shape as the gateway surface so a retry on either endpoint hits the
// same dedup record.
func dedupNamespace(s ws.Session) string {
	if s.AccountID != 0 {
		return "a:" + itoa64(s.AccountID)
	}
	return "u:" + s.Subject
}

// requestIDCtxKey carries the client request_id into Dispatcher calls so
// the order pipeline can stamp order_audit.request_id. NOTE: this is a
// marketdata-local context key — ws.RequestIDFrom reads the ws package's
// own unexported key and will NOT observe this value; adapters for this
// endpoint must call marketdata.RequestIDFrom.
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

func itoa64(v int64) string {
	const digits = "0123456789"
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var buf [20]byte
	pos := len(buf)
	for v > 0 {
		pos--
		buf[pos] = digits[v%10]
		v /= 10
	}
	if neg {
		pos--
		buf[pos] = '-'
	}
	return string(buf[pos:])
}
