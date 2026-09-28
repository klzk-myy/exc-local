package ws

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"exchange/internal/auth"

	excerrors "exchange/pkg/errors"
)

// testIssuer mints real JWTs for the auth paths.
func testIssuer(t *testing.T, ttl time.Duration) *auth.Issuer {
	t.Helper()
	i := auth.NewIssuer("", "", ttl)
	if err := i.AddHMACKey("k1", []byte("0123456789abcdef0123456789abcdef"), true); err != nil {
		t.Fatal(err)
	}
	return i
}

func dial(t *testing.T, srv *Server) *websocket.Conn {
	t.Helper()
	httpSrv := httptest.NewServer(srv)
	t.Cleanup(httpSrv.Close)
	url := "ws" + strings.TrimPrefix(httpSrv.URL, "http") + "/ws/v1"
	c, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

// readMsg reads one server frame as generic JSON within a deadline.
func readMsg(t *testing.T, c *websocket.Conn) map[string]any {
	t.Helper()
	_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, raw, err := c.ReadMessage()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("frame not JSON: %v", err)
	}
	return m
}

func send(t *testing.T, c *websocket.Conn, v any) {
	t.Helper()
	if err := c.WriteJSON(v); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func authenticate(t *testing.T, c *websocket.Conn, token string) map[string]any {
	t.Helper()
	send(t, c, map[string]any{
		"action": "authenticate", "request_id": "auth-1",
		"token": token, "protocol_version": 1,
	})
	return readMsg(t, c)
}

func TestAuthenticateRequiresProtocolVersion(t *testing.T) {
	srv := NewServer(Config{Issuer: testIssuer(t, time.Minute)})
	c := dial(t, srv)
	send(t, c, map[string]any{"action": "authenticate", "token": "x"})
	m := readMsg(t, c)
	if m["error"] != "INVALID_REQUEST" {
		t.Fatalf("want INVALID_REQUEST, got %v", m)
	}
}

func TestAuthenticateRejectsBadProtocolVersion(t *testing.T) {
	srv := NewServer(Config{Issuer: testIssuer(t, time.Minute)})
	c := dial(t, srv)
	send(t, c, map[string]any{
		"action": "authenticate", "token": "x", "protocol_version": 99})
	m := readMsg(t, c)
	if m["error"] != "UNSUPPORTED_PROTOCOL_VERSION" {
		t.Fatalf("want UNSUPPORTED_PROTOCOL_VERSION, got %v", m)
	}
}

func TestAuthenticateJWTAndSubscribePrivate(t *testing.T) {
	iss := testIssuer(t, time.Minute)
	tok, _, err := iss.Issue("u-1", auth.IssueOptions{
		AccountID: 42, Scopes: []string{"read", "trade"}})
	if err != nil {
		t.Fatal(err)
	}
	srv := NewServer(Config{Issuer: iss})
	c := dial(t, srv)

	m := authenticate(t, c, tok)
	if m["type"] != "response" || m["status"] != "ACK" {
		t.Fatalf("auth ack: %v", m)
	}
	data := m["data"].(map[string]any)
	if data["account_id"].(float64) != 42 {
		t.Fatalf("account_id: %v", data)
	}

	// Private channel now subscribes.
	send(t, c, map[string]any{
		"action": "subscribe", "request_id": "s1",
		"channels": []string{"private:orders"}})
	m = readMsg(t, c)
	if m["type"] != "subscribed" {
		t.Fatalf("subscribe private: %v", m)
	}
}

func TestPrivateChannelRequiresAuth(t *testing.T) {
	srv := NewServer(Config{Issuer: testIssuer(t, time.Minute)})
	c := dial(t, srv)
	send(t, c, map[string]any{
		"action": "subscribe", "channels": []string{"private:orders"}})
	m := readMsg(t, c)
	if m["error"] != "UNAUTHORIZED" {
		t.Fatalf("private sub unauthenticated: %v", m)
	}
	// Public channel remains open to anonymous conns.
	send(t, c, map[string]any{
		"action": "subscribe", "channels": []string{"public:ticker@EURUSD"}})
	m = readMsg(t, c)
	if m["type"] != "subscribed" {
		t.Fatalf("public sub: %v", m)
	}
}

func TestReadScopedKeyCannotTrade(t *testing.T) {
	iss := testIssuer(t, time.Minute)
	tok, _, _ := iss.Issue("u-1", auth.IssueOptions{
		AccountID: 42, Scopes: []string{"read"}})
	srv := NewServer(Config{
		Issuer: iss,
		Dispatcher: DispatcherFunc(func(context.Context, *Session, string, json.RawMessage) (*Result, error) {
			return &Result{Status: "ACK"}, nil
		}),
	})
	c := dial(t, srv)
	authenticate(t, c, tok)
	send(t, c, map[string]any{
		"action": "order.place", "request_id": "o1",
		"params": map[string]any{"symbol": "EURUSD"}})
	m := readMsg(t, c)
	if m["error"] != "INSUFFICIENT_SCOPE" {
		t.Fatalf("read-scoped order.place: %v", m)
	}
	// order.status only needs read scope → dispatches.
	send(t, c, map[string]any{
		"action": "order.status", "request_id": "o2",
		"params": map[string]any{"order_id": 7}})
	m = readMsg(t, c)
	if m["type"] != "response" || m["status"] != "ACK" {
		t.Fatalf("order.status: %v", m)
	}
}

func TestUnauthenticatedOrderFrameCloses4019(t *testing.T) {
	var ran atomic.Bool
	srv := NewServer(Config{
		Dispatcher: DispatcherFunc(func(context.Context, *Session, string, json.RawMessage) (*Result, error) {
			ran.Store(true)
			return &Result{Status: "ACK"}, nil
		}),
	})
	c := dial(t, srv)
	send(t, c, map[string]any{
		"action": "order.place", "request_id": "o1",
		"params": map[string]any{"x": 1}})
	m := readMsg(t, c)
	if m["error"] != "UNAUTHORIZED" {
		t.Fatalf("want UNAUTHORIZED, got %v", m)
	}
	// Next read must surface the 4019 close frame.
	_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
	for {
		_, _, err := c.ReadMessage()
		if err == nil {
			continue
		}
		var ce *websocket.CloseError
		if e, ok := err.(*websocket.CloseError); ok {
			ce = e
			if ce.Code != CloseAuthExpired {
				t.Fatalf("close code %d, want 4019", ce.Code)
			}
		} else {
			t.Fatalf("read err %v not a close frame", err)
		}
		if ran.Load() {
			t.Fatal("dispatcher ran for unauthenticated request")
		}
		return
	}
}

func TestOrderDispatchAckAndDedupReplay(t *testing.T) {
	iss := testIssuer(t, time.Minute)
	tok, _, _ := iss.Issue("u-1", auth.IssueOptions{
		AccountID: 42, Scopes: []string{"trade"}})
	var calls atomic.Int32
	srv := NewServer(Config{
		Issuer: iss,
		Dispatcher: DispatcherFunc(func(ctx context.Context, s *Session, action string, params json.RawMessage) (*Result, error) {
			calls.Add(1)
			if RequestIDFrom(ctx) == "" {
				t.Error("request_id not propagated into dispatch ctx")
			}
			return &Result{Status: "ACK", Data: map[string]any{"order_id": "9"}}, nil
		}),
	})
	c := dial(t, srv)
	authenticate(t, c, tok)

	params := map[string]any{"symbol": "EURUSD", "quantity": "1"}
	send(t, c, map[string]any{
		"action": "order.place", "request_id": "dup-1", "params": params})
	m1 := readMsg(t, c)
	if m1["type"] != "response" || m1["status"] != "ACK" {
		t.Fatalf("place ack: %v", m1)
	}
	// Identical retry → verbatim replay, dispatcher NOT re-invoked.
	send(t, c, map[string]any{
		"action": "order.place", "request_id": "dup-1", "params": params})
	m2 := readMsg(t, c)
	if m2["type"] != "response" || m2["status"] != "ACK" || m2["ts_ms"] != m1["ts_ms"] {
		t.Fatalf("replay mismatch: %v vs %v", m2, m1)
	}
	if calls.Load() != 1 {
		t.Fatalf("dispatcher invoked %d times — dedup failed", calls.Load())
	}
	// Same request_id, different payload → IDEMPOTENCY_KEY_MISMATCH.
	send(t, c, map[string]any{
		"action": "order.place", "request_id": "dup-1",
		"params": map[string]any{"symbol": "USDJPY", "quantity": "2"}})
	m3 := readMsg(t, c)
	if m3["error"] != "IDEMPOTENCY_KEY_MISMATCH" {
		t.Fatalf("mismatch: %v", m3)
	}
}

func TestOrderDispatchBusinessErrorStored(t *testing.T) {
	iss := testIssuer(t, time.Minute)
	tok, _, _ := iss.Issue("u-1", auth.IssueOptions{
		AccountID: 42, Scopes: []string{"trade"}})
	srv := NewServer(Config{
		Issuer: iss,
		Dispatcher: DispatcherFunc(func(context.Context, *Session, string, json.RawMessage) (*Result, error) {
			return nil, excerrors.New("ORDER_NOT_FOUND", "no such order")
		}),
	})
	c := dial(t, srv)
	authenticate(t, c, tok)
	send(t, c, map[string]any{
		"action": "order.cancel", "request_id": "c1",
		"params": map[string]any{"order_id": 5}})
	m := readMsg(t, c)
	if m["type"] != "error" || m["error"] != "ORDER_NOT_FOUND" {
		t.Fatalf("business error frame: %v", m)
	}
}

func TestOrderInFlightCollision(t *testing.T) {
	iss := testIssuer(t, time.Minute)
	tok, _, _ := iss.Issue("u-1", auth.IssueOptions{
		AccountID: 42, Scopes: []string{"trade"}})
	release := make(chan struct{})
	srv := NewServer(Config{
		Issuer: iss,
		Dispatcher: DispatcherFunc(func(ctx context.Context, s *Session, a string, p json.RawMessage) (*Result, error) {
			<-release
			return &Result{Status: "ACK"}, nil
		}),
	})
	c := dial(t, srv)
	authenticate(t, c, tok)

	params := map[string]any{"order_id": 1}
	send(t, c, map[string]any{
		"action": "order.cancel", "request_id": "col-1", "params": params})
	send(t, c, map[string]any{
		"action": "order.cancel", "request_id": "col-1", "params": params})
	// The second frame collides while the first is still dispatching.
	m := readMsg(t, c)
	if m["error"] != "IDEMPOTENCY_KEY_COLLISION" {
		t.Fatalf("collision: %v", m)
	}
	close(release)
	m = readMsg(t, c)
	if m["type"] != "response" || m["status"] != "ACK" {
		t.Fatalf("first dispatch ack: %v", m)
	}
}

func TestOrderMissingRequestID(t *testing.T) {
	iss := testIssuer(t, time.Minute)
	tok, _, _ := iss.Issue("u-1", auth.IssueOptions{AccountID: 42})
	srv := NewServer(Config{Issuer: iss})
	c := dial(t, srv)
	authenticate(t, c, tok)
	send(t, c, map[string]any{
		"action": "order.place", "params": map[string]any{"x": 1}})
	m := readMsg(t, c)
	if m["error"] != "INVALID_REQUEST" {
		t.Fatalf("missing request_id: %v", m)
	}
}

func TestCountdownNilFailsClosed(t *testing.T) {
	iss := testIssuer(t, time.Minute)
	tok, _, _ := iss.Issue("u-1", auth.IssueOptions{
		AccountID: 42, Scopes: []string{"trade"}})
	srv := NewServer(Config{Issuer: iss}) // no Countdown wired
	c := dial(t, srv)
	authenticate(t, c, tok)
	send(t, c, map[string]any{
		"action": "order.countdown_cancel_all", "request_id": "cd1",
		"params": map[string]any{"countdown_ms": 5000}})
	m := readMsg(t, c)
	if m["error"] != "NOT_IMPLEMENTED" {
		t.Fatalf("nil countdown: %v", m)
	}
}

func TestRefreshTokenBindsIdentity(t *testing.T) {
	iss := testIssuer(t, time.Minute)
	tok1, _, _ := iss.Issue("u-1", auth.IssueOptions{AccountID: 42})
	tok2, _, _ := iss.Issue("u-2", auth.IssueOptions{AccountID: 43})
	srv := NewServer(Config{Issuer: iss})
	c := dial(t, srv)
	authenticate(t, c, tok1)

	// Refresh with a different identity → fail closed UNAUTHORIZED.
	send(t, c, map[string]any{
		"action": "refresh_token", "request_id": "r1", "token": tok2})
	m := readMsg(t, c)
	if m["error"] != "UNAUTHORIZED" {
		t.Fatalf("identity swap on refresh: %v", m)
	}
	// Same-identity refresh → ACK with new expiry.
	tok1b, _, _ := iss.Issue("u-1", auth.IssueOptions{AccountID: 42})
	send(t, c, map[string]any{
		"action": "refresh_token", "request_id": "r2", "token": tok1b})
	m = readMsg(t, c)
	if m["type"] != "response" || m["status"] != "ACK" {
		t.Fatalf("refresh ack: %v", m)
	}
	if _, ok := m["data"].(map[string]any)["expires_at_ms"]; !ok {
		t.Fatalf("refresh ack missing expires_at_ms: %v", m)
	}
}

func TestRefreshRequiresAuth(t *testing.T) {
	srv := NewServer(Config{Issuer: testIssuer(t, time.Minute)})
	c := dial(t, srv)
	send(t, c, map[string]any{
		"action": "refresh_token", "request_id": "r1", "token": "x"})
	m := readMsg(t, c)
	if m["error"] != "UNAUTHORIZED" {
		t.Fatalf("anon refresh: %v", m)
	}
}

func TestAuthExpiryDemotesWithPublicSubs(t *testing.T) {
	iss := testIssuer(t, 1500*time.Millisecond)
	tok, _, _ := iss.Issue("u-1", auth.IssueOptions{
		AccountID: 42, Scopes: []string{"read", "trade"}})
	srv := NewServer(Config{Issuer: iss})
	c := dial(t, srv)
	authenticate(t, c, tok)

	// One public + one private subscription.
	send(t, c, map[string]any{
		"action": "subscribe", "channels": []string{"public:ticker@EURUSD"}})
	readMsg(t, c)
	send(t, c, map[string]any{
		"action": "subscribe", "channels": []string{"private:orders"}})
	readMsg(t, c)

	// Wait for expiry → AUTH_EXPIRED frame; public sub keeps the socket
	// alive in demoted anonymous mode.
	var sawExpired bool
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && !sawExpired {
		m := readMsg(t, c)
		if m["error"] == "AUTH_EXPIRED" {
			sawExpired = true
		}
	}
	if !sawExpired {
		t.Fatal("no AUTH_EXPIRED frame")
	}
	// Demoted: order actions now reject UNAUTHORIZED and close 4019.
	send(t, c, map[string]any{
		"action": "order.place", "request_id": "post-exp",
		"params": map[string]any{"x": 1}})
	m := readMsg(t, c)
	if m["error"] != "UNAUTHORIZED" {
		t.Fatalf("post-expiry order: %v", m)
	}
}

func TestAuthExpiryCloses4019WithoutPublicSubs(t *testing.T) {
	iss := testIssuer(t, 1500*time.Millisecond)
	tok, _, _ := iss.Issue("u-1", auth.IssueOptions{AccountID: 42})
	srv := NewServer(Config{Issuer: iss})
	c := dial(t, srv)
	authenticate(t, c, tok)
	send(t, c, map[string]any{
		"action": "subscribe", "channels": []string{"private:orders"}})
	readMsg(t, c)

	// No public subs → after AUTH_EXPIRED the socket closes 4019.
	_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
	var sawExpired bool
	for {
		_, raw, err := c.ReadMessage()
		if err != nil {
			ce, ok := err.(*websocket.CloseError)
			if !ok {
				t.Fatalf("read err %v", err)
			}
			if !sawExpired {
				t.Fatal("closed before AUTH_EXPIRED frame")
			}
			if ce.Code != CloseAuthExpired {
				t.Fatalf("close code %d want 4019", ce.Code)
			}
			return
		}
		var m map[string]any
		_ = json.Unmarshal(raw, &m)
		if m["error"] == "AUTH_EXPIRED" {
			sawExpired = true
		}
	}
}

func TestResumeAnswersResync(t *testing.T) {
	srv := NewServer(Config{})
	c := dial(t, srv)
	send(t, c, map[string]any{
		"action": "resume", "channel": "public:ticker@EURUSD", "last_seq": 7})
	m := readMsg(t, c)
	if m["type"] != "resync" || m["channel"] != "public:ticker@EURUSD" {
		t.Fatalf("resume: %v", m)
	}
}

func TestPingPong(t *testing.T) {
	srv := NewServer(Config{})
	c := dial(t, srv)
	send(t, c, map[string]any{"action": "ping"})
	m := readMsg(t, c)
	if m["type"] != "pong" {
		t.Fatalf("ping: %v", m)
	}
}

func TestPerIPConnectionCap(t *testing.T) {
	srv := NewServer(Config{MaxConnsPerIP: 1})
	c1 := dial(t, srv)
	defer c1.Close()
	// Second conn from same IP gets pre-upgrade 503 JSON.
	httpSrv := httptest.NewServer(srv)
	defer httpSrv.Close()
	url := "ws" + strings.TrimPrefix(httpSrv.URL, "http") + "/ws/v1"
	_, resp, err := websocket.DefaultDialer.Dial(url, nil)
	if err == nil {
		t.Fatal("second conn should be refused")
	}
	if resp == nil || resp.StatusCode != 503 {
		t.Fatalf("want 503 pre-upgrade, got %v", resp)
	}
}
