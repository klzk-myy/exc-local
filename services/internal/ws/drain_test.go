// Phase-06 Task 6.3.19 (graceful drain & shutdown advisory) and the
// Task 6.3.21 feed-failover seam tests.
package ws

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"exchange/internal/auth"
)

// TestDrainAdvisoryOrderingAndDeadline covers Task 6.3.19 end to end:
// advisory first, in-band refusal of new work with MAINTENANCE_MODE,
// cancels stay available, survivor close 1001 CONNECTION_DRAINING at
// the deadline, upgrades refused during the drain.
func TestDrainAdvisoryOrderingAndDeadline(t *testing.T) {
	iss := testIssuer(t, time.Minute)
	tok, _, err := iss.Issue("u-1", auth.IssueOptions{
		AccountID: 42, Scopes: []string{"read", "trade"}})
	if err != nil {
		t.Fatal(err)
	}
	srv := NewServer(Config{
		Issuer: iss,
		Dispatcher: DispatcherFunc(func(_ context.Context, _ *Session, _ string, _ json.RawMessage) (*Result, error) {
			return &Result{Status: "ACK"}, nil
		}),
	})
	c := dial(t, srv)
	authenticate(t, c, tok)
	send(t, c, map[string]any{
		"action": "subscribe", "channels": []string{"public:ticker@EURUSD"}})
	readMsg(t, c) // subscribed ack

	drainDone := make(chan error, 1)
	go func() {
		drainDone <- srv.Drain(context.Background(), DrainAdvisory{
			Reason:     "deploy",
			RetryAfter: 2 * time.Second,
			Endpoint:   "wss://b.example.com/ws/v1",
			Deadline:   400 * time.Millisecond,
		})
	}()

	if !srv.Draining() {
		// Drain may not have flipped yet — spin briefly.
		deadline := time.Now().Add(2 * time.Second)
		for !srv.Draining() && time.Now().Before(deadline) {
			time.Sleep(2 * time.Millisecond)
		}
		if !srv.Draining() {
			t.Fatal("server never entered drain mode")
		}
	}

	// 1. Advisory frame arrives first, ahead of everything else.
	m := readMsg(t, c)
	if m["type"] != "server.shutdown" {
		t.Fatalf("want server.shutdown advisory, got %v", m)
	}
	if m["reason"] != "deploy" {
		t.Fatalf("advisory reason: %v", m)
	}
	if m["endpoint"] != "wss://b.example.com/ws/v1" {
		t.Fatalf("advisory endpoint: %v", m)
	}
	if v, ok := m["retry_after_ms"].(float64); !ok || v != 2000 {
		t.Fatalf("advisory retry_after_ms: %v", m)
	}
	if v, ok := m["deadline_ms"].(float64); !ok || v <= float64(m["ts_ms"].(float64)) {
		t.Fatalf("advisory deadline_ms: %v", m)
	}

	// 2. New work refused in-band; cancels still dispatch.
	send(t, c, map[string]any{
		"action": "order.place", "request_id": "p1",
		"params": map[string]any{"symbol": "EURUSD"}})
	send(t, c, map[string]any{
		"action": "subscribe", "channels": []string{"public:ticker@GBPUSD"}})
	send(t, c, map[string]any{
		"action": "order.cancel", "request_id": "c1",
		"params": map[string]any{"order_id": 7}})

	m = readMsg(t, c)
	if m["error"] != "MAINTENANCE_MODE" || m["request_id"] != "p1" {
		t.Fatalf("drain order.place: %v", m)
	}
	m = readMsg(t, c)
	if m["error"] != "MAINTENANCE_MODE" {
		t.Fatalf("drain subscribe: %v", m)
	}
	m = readMsg(t, c)
	if m["type"] != "response" || m["status"] != "ACK" || m["request_id"] != "c1" {
		t.Fatalf("drain cancel must stay available: %v", m)
	}

	// 3. New upgrades refused pre-upgrade while draining.
	httpSrv := httptest.NewServer(srv)
	defer httpSrv.Close()
	url := "ws" + strings.TrimPrefix(httpSrv.URL, "http") + "/ws/v1"
	_, resp, derr := websocket.DefaultDialer.Dial(url, nil)
	if derr == nil {
		t.Fatal("upgrade during drain should be refused")
	}
	if resp == nil || resp.StatusCode != 503 {
		t.Fatalf("want 503, got %v", resp)
	}
	var body map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&body)
	resp.Body.Close()
	if body["error"] != "MAINTENANCE_MODE" {
		t.Fatalf("drain rejection body: %v", body)
	}

	// 4. At the deadline the survivor closes 1001 CONNECTION_DRAINING.
	_, ce := readUntilClose(t, c)
	if ce.Code != websocket.CloseGoingAway {
		t.Fatalf("drain close code %d, want 1001", ce.Code)
	}
	if ce.Text != "CONNECTION_DRAINING" {
		t.Fatalf("drain close reason %q", ce.Text)
	}
	if err := <-drainDone; err != nil {
		t.Fatalf("Drain returned %v", err)
	}
}

// TestDrainReturnsWhenClientsLeave checks the fast path: the drain
// completes before the deadline once the registry empties.
func TestDrainReturnsWhenClientsLeave(t *testing.T) {
	srv := NewServer(Config{})
	c := dial(t, srv)

	done := make(chan error, 1)
	go func() {
		done <- srv.Drain(context.Background(), DrainAdvisory{
			Reason:   "maintenance",
			Deadline: 30 * time.Second, // must never be reached
		})
	}()
	readMsg(t, c) // advisory
	_ = c.WriteMessage(websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Drain: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("drain did not complete after last client left")
	}
}

// TestDrainTwiceFails — drain is terminal, not re-entrant.
func TestDrainTwiceFails(t *testing.T) {
	srv := NewServer(Config{})
	done := make(chan error, 1)
	go func() {
		done <- srv.Drain(context.Background(), DrainAdvisory{Deadline: 50 * time.Millisecond})
	}()
	// Wait for the flag to flip so the second call sees an active drain.
	deadline := time.Now().Add(2 * time.Second)
	for !srv.Draining() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !srv.Draining() {
		t.Fatal("first Drain never entered drain mode")
	}
	if err := srv.Drain(context.Background(), DrainAdvisory{}); err == nil {
		t.Fatal("second Drain must fail")
	}
	<-done
}

// TestDrainAdvisoryMergesFailoverEndpoints — the advisory falls back to
// the FailoverConfig endpoint list when the caller supplies none.
func TestDrainAdvisoryMergesFailoverEndpoints(t *testing.T) {
	srv := NewServer(Config{
		Failover: FailoverConfig{Endpoints: []string{"wss://feed-b/ws/v1", "wss://feed-c/ws/v1"}},
	})
	c := dial(t, srv)
	done := make(chan error, 1)
	go func() {
		done <- srv.Drain(context.Background(), DrainAdvisory{
			Reason: "deploy", Deadline: 300 * time.Millisecond,
		})
	}()
	m := readMsg(t, c)
	if m["type"] != "server.shutdown" {
		t.Fatalf("want advisory, got %v", m)
	}
	if m["endpoint"] != "wss://feed-b/ws/v1" {
		t.Fatalf("endpoint fallback: %v", m)
	}
	eps, _ := m["endpoints"].([]any)
	if len(eps) != 2 || eps[0] != "wss://feed-b/ws/v1" || eps[1] != "wss://feed-c/ws/v1" {
		t.Fatalf("endpoints: %v", m)
	}
	_, ce := readUntilClose(t, c)
	if ce.Code != websocket.CloseGoingAway {
		t.Fatalf("close code %d", ce.Code)
	}
	<-done
}

// TestFeedFailoverAdvisory covers the Task 6.3.21 seam: a health
// transition broadcasts feed.failover with the configured endpoint
// list, edge-triggered (no repeat on same state).
func TestFeedFailoverAdvisory(t *testing.T) {
	type transition struct{ prev, cur FeedState }
	tr := make(chan transition, 4)
	srv := NewServer(Config{
		Failover: FailoverConfig{
			Endpoints:    []string{"wss://feed-b/ws/v1"},
			OnTransition: func(p, c FeedState) { tr <- transition{p, c} },
		},
	})
	c := dial(t, srv)

	srv.SetFeedState(FeedDown)
	m := readMsg(t, c)
	if m["type"] != "feed.failover" || m["state"] != "down" {
		t.Fatalf("failover advisory: %v", m)
	}
	eps, _ := m["endpoints"].([]any)
	if len(eps) != 1 || eps[0] != "wss://feed-b/ws/v1" {
		t.Fatalf("failover endpoints: %v", m)
	}
	select {
	case got := <-tr:
		if got.prev != FeedHealthy || got.cur != FeedDown {
			t.Fatalf("transition %+v", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("OnTransition not invoked")
	}
	if srv.FeedState() != FeedDown {
		t.Fatal("FeedState getter")
	}

	// Same state → no new advisory; the channel must stay quiet.
	srv.SetFeedState(FeedDown)
	srv.SetFeedState(FeedDegraded)
	m = readMsg(t, c)
	if m["type"] != "feed.failover" || m["state"] != "degraded" {
		t.Fatalf("expected single degraded advisory, got %v", m)
	}
}
