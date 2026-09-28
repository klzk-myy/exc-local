// Task 6.3.10 — order.* dispatch, dedup and timeout tests.
package marketdata

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"exchange/internal/auth"
	"exchange/internal/ws"
)

func testIssuer(t *testing.T) *auth.Issuer {
	t.Helper()
	i := auth.NewIssuer("", "", time.Minute)
	if err := i.AddHMACKey("k1", []byte("0123456789abcdef0123456789abcdef"), true); err != nil {
		t.Fatal(err)
	}
	return i
}

func authenticate(t *testing.T, c *websocket.Conn, token string) map[string]any {
	t.Helper()
	sendJSON(t, c, map[string]any{
		"action": "authenticate", "request_id": "auth-1",
		"token": token, "protocol_version": 1,
	})
	return readFrame(t, c, 3*time.Second)
}

// TestOrderDispatchAckAndDedup — authenticated order.* frames dispatch
// through the seam; identical retries replay the stored frame without
// re-invoking the pipeline (Task 6.3.22 item 2 shared dedup window).
func TestOrderDispatchAckAndDedup(t *testing.T) {
	iss := testIssuer(t)
	tok, _, err := iss.Issue("u-1", auth.IssueOptions{
		AccountID: 42, Scopes: []string{"trade"}})
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	var calls atomic.Int32
	srv, c := dial(t, Config{
		Issuer: iss,
		Dispatcher: ws.DispatcherFunc(func(ctx context.Context, s *ws.Session,
			action string, params json.RawMessage) (*ws.Result, error) {
			calls.Add(1)
			if RequestIDFrom(ctx) == "" {
				t.Error("request_id not propagated into dispatch ctx")
			}
			return &ws.Result{Status: "ACK", Data: map[string]any{"order_id": "9"}}, nil
		}),
	})
	_ = srv
	m := authenticate(t, c, tok)
	if m["status"] != "ACK" {
		t.Fatalf("auth: %v", m)
	}

	params := map[string]any{"symbol": "EURUSD", "quantity": "1"}
	sendJSON(t, c, map[string]any{
		"action": "order.place", "request_id": "dup-1", "params": params})
	m1 := readUntil(t, c, 2*time.Second, func(m map[string]any) bool {
		return m["type"] == "response" || m["type"] == "error"
	})
	if m1["type"] != "response" || m1["status"] != "ACK" {
		t.Fatalf("place ack: %v", m1)
	}
	// Identical retry → verbatim replay, no re-dispatch.
	sendJSON(t, c, map[string]any{
		"action": "order.place", "request_id": "dup-1", "params": params})
	m2 := readUntil(t, c, 2*time.Second, func(m map[string]any) bool {
		return m["type"] == "response" || m["type"] == "error"
	})
	if m2["type"] != "response" || m2["ts_ms"] != m1["ts_ms"] {
		t.Fatalf("dedup replay mismatch: %v vs %v", m2, m1)
	}
	if calls.Load() != 1 {
		t.Fatalf("dispatcher invoked %d times — dedup failed", calls.Load())
	}
	// Same request_id, different payload → mismatch.
	sendJSON(t, c, map[string]any{
		"action": "order.place", "request_id": "dup-1",
		"params": map[string]any{"symbol": "USDJPY", "quantity": "2"}})
	m3 := readUntil(t, c, 2*time.Second, func(m map[string]any) bool {
		return m["type"] == "error"
	})
	if m3["error"] != "IDEMPOTENCY_KEY_MISMATCH" {
		t.Fatalf("payload drift: %v", m3)
	}
}

// TestOrderDispatchTimeout — a silent pipeline past DispatchTimeout
// answers a correlated CORE_TIMEOUT (Task 6.3.10 item 5).
func TestOrderDispatchTimeout(t *testing.T) {
	iss := testIssuer(t)
	tok, _, _ := iss.Issue("u-1", auth.IssueOptions{
		AccountID: 42, Scopes: []string{"trade"}})
	_, c := dial(t, Config{
		Issuer:          iss,
		DispatchTimeout: 60 * time.Millisecond,
		Dispatcher: ws.DispatcherFunc(func(ctx context.Context, _ *ws.Session,
			_ string, _ json.RawMessage) (*ws.Result, error) {
			<-ctx.Done() // never answers — simulates a silent engine
			return nil, ctx.Err()
		}),
	})
	if m := authenticate(t, c, tok); m["status"] != "ACK" {
		t.Fatalf("auth: %v", m)
	}
	sendJSON(t, c, map[string]any{
		"action": "order.place", "request_id": "t-1",
		"params": map[string]any{"symbol": "EURUSD"}})
	m := readUntil(t, c, 3*time.Second, func(m map[string]any) bool {
		return m["type"] == "error"
	})
	if m["error"] != "CORE_TIMEOUT" || m["request_id"] != "t-1" {
		t.Fatalf("timeout frame = %+v", m)
	}
}

// TestUnauthenticatedOrderCloses — an unauthenticated order frame rejects
// UNAUTHORIZED and the conn closes 4019 (fail-closed trading gate).
func TestUnauthenticatedOrderCloses(t *testing.T) {
	var ran atomic.Bool
	_, c := dial(t, Config{
		Dispatcher: ws.DispatcherFunc(func(context.Context, *ws.Session,
			string, json.RawMessage) (*ws.Result, error) {
			ran.Store(true)
			return &ws.Result{Status: "ACK"}, nil
		}),
	})
	sendJSON(t, c, map[string]any{
		"action": "order.place", "request_id": "o1",
		"params": map[string]any{"x": 1}})
	m := readFrame(t, c, 3*time.Second)
	if m["error"] != "UNAUTHORIZED" {
		t.Fatalf("want UNAUTHORIZED, got %v", m)
	}
	_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
	for {
		_, _, err := c.ReadMessage()
		if err == nil {
			continue
		}
		if ce, ok := err.(*websocket.CloseError); ok {
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
