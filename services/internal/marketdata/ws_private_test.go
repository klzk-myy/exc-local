// Task 6.3.5 — /ws/v1/orders private order stream tests: auth gating,
// per-account isolation, private replay rings, endpoint restriction.
// Task 6.3.22 — entitlement denials + durable private seq cursors.
// Task 6.3.24 — resync-by-symbol + private resume isolation.
package marketdata

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"exchange/internal/auth"
)

// testIssuer (ws_dispatcher_test.go) mints and verifies test JWTs.

// jwtFor mints a token bound to accountID with the given scopes.
func jwtFor(t *testing.T, i *auth.Issuer, accountID int64, scopes ...string) string {
	t.Helper()
	tok, _, err := i.Issue("user-"+strconv.FormatInt(accountID, 10),
		auth.IssueOptions{AccountID: accountID, Scopes: scopes})
	if err != nil {
		t.Fatalf("issue jwt: %v", err)
	}
	return tok
}

// dialPath connects a WS client to a specific upgrade path.
func dialPath(t *testing.T, cfg Config, path string) (*Server, *websocket.Conn) {
	t.Helper()
	srv := NewServer(cfg)
	httpSrv := httptest.NewServer(http.HandlerFunc(srv.ServeHTTP))
	t.Cleanup(httpSrv.Close)
	u := "ws" + strings.TrimPrefix(httpSrv.URL, "http") + path
	c, _, err := websocket.DefaultDialer.Dial(u, nil)
	if err != nil {
		t.Fatalf("dial %s: %v", path, err)
	}
	t.Cleanup(func() { c.Close() })
	return srv, c
}

// authConn authenticates one conn as accountID (JWT, read+trade scopes).
func authConn(t *testing.T, c *websocket.Conn, token string) {
	t.Helper()
	pv := 1
	sendJSON(t, c, map[string]any{
		"action": "authenticate", "token": token, "protocol_version": pv,
	})
	m := readUntil(t, c, 2*time.Second, func(m map[string]any) bool {
		return m["type"] == "response" || m["type"] == "error"
	})
	if m["type"] != "response" || m["status"] != "ACK" {
		t.Fatalf("authenticate = %+v", m)
	}
}

// TestPrivateOrdersRequireAuth — private:* binds on either surface
// demand an authenticated session with read scope (§10.5 item 2).
func TestPrivateOrdersRequireAuth(t *testing.T) {
	iss := testIssuer(t)
	_, c := dialPath(t, Config{Issuer: iss}, PrivateWSPath)

	sendJSON(t, c, map[string]any{"action": "subscribe",
		"channels": []string{"private:orders"}})
	m := readUntil(t, c, 2*time.Second, func(m map[string]any) bool {
		return m["type"] == "error"
	})
	if m["error"] != "UNAUTHORIZED" {
		t.Fatalf("anonymous private sub error = %v", m["error"])
	}
}

// TestPrivateOrdersScopeGate — an authenticated session WITHOUT read
// scope is refused INSUFFICIENT_SCOPE (API-key scope matrix §8.8).
func TestPrivateOrdersScopeGate(t *testing.T) {
	iss := testIssuer(t)
	_, c := dialPath(t, Config{Issuer: iss}, PrivateWSPath)
	// Minted token carries scopes ["trade"] — "read" absent → the gate
	// must refuse INSUFFICIENT_SCOPE.
	authConn(t, c, jwtFor(t, iss, 1001, "trade"))
	sendJSON(t, c, map[string]any{"action": "subscribe",
		"channels": []string{"private:orders"}})
	m := readUntil(t, c, 2*time.Second, func(m map[string]any) bool {
		return m["type"] == "error" || m["type"] == "subscribed"
	})
	if m["type"] != "error" || m["error"] != "INSUFFICIENT_SCOPE" {
		t.Fatalf("scope-denied private sub = %+v", m)
	}
}

// TestPrivateOrdersIsolation — the core Task 6.3.5 contract: account A's
// order events NEVER reach account B's session, in either direction.
func TestPrivateOrdersIsolation(t *testing.T) {
	iss := testIssuer(t)
	srv, ca := dialPath(t, Config{Issuer: iss}, PrivateWSPath)
	_, cb := dialPathOn(t, srv, PrivateWSPath)

	authConn(t, ca, jwtFor(t, iss, 1001, "read"))
	authConn(t, cb, jwtFor(t, iss, 2002, "read"))
	for _, c := range []*websocket.Conn{ca, cb} {
		sendJSON(t, c, map[string]any{"action": "subscribe",
			"channels": []string{"private:orders"}})
		m := readUntil(t, c, 2*time.Second, func(m map[string]any) bool {
			return m["type"] == "subscribed" || m["type"] == "error"
		})
		if m["type"] != "subscribed" {
			t.Fatalf("private subscribe = %+v", m)
		}
	}

	stream := NewPrivateOrderStream(srv, NewMemSeqStore(), nil, nil)
	// Account 1001's order ack.
	seq, err := stream.Publish(1001, "private:orders", OrderEvent{
		Event: OrderEventAck, OrderID: "ord-a1", Symbol: "EUR/USD",
		Status: "NEW", Side: "BUY", Quantity: "1000000",
	})
	if err != nil || seq != 1 {
		t.Fatalf("publish A: seq=%d err=%v", seq, err)
	}
	// Account 2002's fill.
	if _, err := stream.Publish(2002, "private:orders", OrderEvent{
		Event: OrderEventFill, OrderID: "ord-b1", Symbol: "GBP/USD",
		Status: "FILLED", Side: "SELL", LastFillQty: "500", LastFillPx: "1.25",
	}); err != nil {
		t.Fatalf("publish B: %v", err)
	}

	// A receives exactly its own event, seq-scoped to its own domain.
	m := readUntil(t, ca, 2*time.Second, func(m map[string]any) bool {
		return m["type"] == "event"
	})
	if m["channel"] != "private:orders" || int(m["seq"].(float64)) != 1 {
		t.Fatalf("A event = %+v", m)
	}
	data, _ := m["data"].(map[string]any)
	if data["order_id"] != "ord-a1" {
		t.Fatalf("A received foreign order event: %+v", data)
	}

	// B receives only its own event (its own per-account seq — starts at 1).
	m = readUntil(t, cb, 2*time.Second, func(m map[string]any) bool {
		return m["type"] == "event"
	})
	data, _ = m["data"].(map[string]any)
	if data["order_id"] != "ord-b1" {
		t.Fatalf("B received foreign order event: %+v", data)
	}
	if int(m["seq"].(float64)) != 1 {
		t.Fatalf("B seq = %v — private seqs are per-account", m["seq"])
	}

	// Neither sees the other's frame: drain briefly, expect silence.
	_ = ca.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	for {
		_, b, err := ca.ReadMessage()
		if err != nil {
			break
		}
		t.Fatalf("A received extra frame: %s", b)
	}
}

// TestPrivateResumeIsolation — A's replay ring is invisible to B: B
// resuming with a stale cursor gets a resync directive, never A's frames.
func TestPrivateResumeIsolation(t *testing.T) {
	iss := testIssuer(t)
	srv, ca := dialPath(t, Config{Issuer: iss}, PrivateWSPath)
	authConn(t, ca, jwtFor(t, iss, 1001, "read"))
	sendJSON(t, ca, map[string]any{"action": "subscribe",
		"channels": []string{"private:orders"}})
	readUntil(t, ca, 2*time.Second, func(m map[string]any) bool {
		return m["type"] == "subscribed"
	})

	stream := NewPrivateOrderStream(srv, NewMemSeqStore(), nil, nil)
	for i := 1; i <= 3; i++ {
		if _, err := stream.Publish(1001, "private:orders", OrderEvent{
			Event: OrderEventAck, OrderID: "ord-a" + itoa(i),
			Status: "NEW",
		}); err != nil {
			t.Fatalf("publish: %v", err)
		}
	}
	readUntil(t, ca, 2*time.Second, func(m map[string]any) bool {
		return m["type"] == "event" && int(m["seq"].(float64)) == 3
	})

	// B connects, subscribes, then resumes at last_seq=0 → cold-start:
	// zero of A's frames replay (empty per-account ring → resync).
	_, cb := dialPathOn(t, srv, PrivateWSPath)
	authConn(t, cb, jwtFor(t, iss, 2002, "read"))
	sendJSON(t, cb, map[string]any{
		"action": "resume", "channel": "private:orders", "last_seq": 0,
	})
	var sawEvent bool
	m := readUntil(t, cb, 2*time.Second, func(m map[string]any) bool {
		if m["type"] == "event" {
			sawEvent = true
		}
		return m["type"] == "resync" || m["type"] == "snapshot"
	})
	if sawEvent {
		t.Fatal("B observed A's private frames during resume")
	}
	if m["type"] != "resync" {
		t.Fatalf("B resume verdict = %+v, want resync (empty private ring)", m)
	}
}

// TestPrivateSeqRestartContinuity — the per-account cursor survives a
// stream restart via SeqStore (md:seq:private:*): a fresh
// PrivateOrderStream on the same store continues from the high-water
// mark, never resetting to 0 (Task 6.3.22).
func TestPrivateSeqRestartContinuity(t *testing.T) {
	store := NewMemSeqStore()
	srv := NewServer(Config{})

	p1 := NewPrivateOrderStream(srv, store, nil, nil)
	for i := 0; i < 3; i++ {
		if _, err := p1.Publish(1001, "private:orders", OrderEvent{
			Event: OrderEventAck, OrderID: "x", Status: "NEW",
		}); err != nil {
			t.Fatalf("publish: %v", err)
		}
	}
	// Wait for the async high-water mirror to land.
	waitStore(t, store, PrivateSeqKey("private:orders", 1001), 3)

	// "Restart": a fresh stream on the same durable store.
	p2 := NewPrivateOrderStream(srv, store, nil, nil)
	seq, err := p2.Publish(1001, "private:orders", OrderEvent{
		Event: OrderEventAck, OrderID: "x2", Status: "NEW",
	})
	if err != nil {
		t.Fatalf("restart publish: %v", err)
	}
	if seq != 4 {
		t.Fatalf("post-restart seq = %d, want 4 (cursor continues, no reset)", seq)
	}

	// A different account is unaffected — its own domain, own cursor.
	seq, err = p2.Publish(2002, "private:orders", OrderEvent{
		Event: OrderEventAck, OrderID: "y", Status: "NEW",
	})
	if err != nil || seq != 1 {
		t.Fatalf("account B seq = %d, want 1", seq)
	}
}

// TestPrivateEndpointRejectsPublic — /ws/v1/orders is the private
// surface: public feed channels are refused there (Task 6.3.5 endpoint
// partition); the same channel binds fine on /ws/v1/marketdata.
func TestPrivateEndpointRejectsPublic(t *testing.T) {
	iss := testIssuer(t)
	_, c := dialPath(t, Config{Issuer: iss}, PrivateWSPath)
	authConn(t, c, jwtFor(t, iss, 1001, "read"))
	sendJSON(t, c, map[string]any{"action": "subscribe",
		"channels": []string{"book@EUR/USD"}})
	m := readUntil(t, c, 2*time.Second, func(m map[string]any) bool {
		return m["type"] == "error"
	})
	if m["error"] != "INVALID_REQUEST" {
		t.Fatalf("public sub on private endpoint error = %v", m["error"])
	}
}

// TestEntitlementDenial — a deny-listed symbol rejects
// ENTITLEMENT_REQUIRED at bind (Task 6.3.22, §23 registry code).
func TestEntitlementDenial(t *testing.T) {
	srv, c := dial(t, Config{
		Entitlements: &StaticEntitlements{
			DenySymbols: map[string]bool{"USD/TRY": true},
		},
	})
	_ = srv
	sendJSON(t, c, map[string]any{"action": "subscribe",
		"channels": []string{"depth@USD/TRY:5:100", "book@EUR/USD"}})
	errF := readUntil(t, c, 2*time.Second, func(m map[string]any) bool {
		return m["type"] == "error"
	})
	if errF["error"] != "ENTITLEMENT_REQUIRED" {
		t.Fatalf("denied symbol error = %v", errF["error"])
	}
	sub := readUntil(t, c, 2*time.Second, func(m map[string]any) bool {
		return m["type"] == "subscribed"
	})
	if int(sub["total"].(float64)) != 1 {
		t.Fatalf("subs = %v — entitled channel must still bind", sub["total"])
	}
	// The denial counter moved.
	if srv.Metrics().EntitlementRejections.Load() != 1 {
		t.Fatalf("entitlement rejections = %d",
			srv.Metrics().EntitlementRejections.Load())
	}
}

// TestResyncBySymbol — {"action":"resync","symbol":"EUR/USD"} resolves
// the conn's bound depth channel and replays the missed range
// (§10.9 item-2 client-initiated repair).
func TestResyncBySymbol(t *testing.T) {
	srv, c := dial(t, Config{})
	sendJSON(t, c, map[string]any{"action": "subscribe",
		"channels": []string{"depth@EUR/USD:5:100"}})
	readUntil(t, c, 2*time.Second, func(m map[string]any) bool {
		return m["type"] == "subscribed"
	})
	for i := 1; i <= 4; i++ {
		srv.Publish("depth@EUR/USD:5:100", uint64(i), map[string]any{"n": i})
	}
	readUntil(t, c, 2*time.Second, func(m map[string]any) bool {
		return m["type"] == "event" && int(m["seq"].(float64)) == 4
	})

	sendJSON(t, c, map[string]any{
		"action": "resync", "symbol": "EUR/USD", "last_seq": 2,
	})
	var seqs []int
	m := readUntil(t, c, 2*time.Second, func(m map[string]any) bool {
		if m["type"] == "event" {
			seqs = append(seqs, int(m["seq"].(float64)))
			// The replayed frames carry their ORIGINAL channel token.
			if m["channel"] != "depth@EUR/USD:5:100" {
				t.Fatalf("replayed frame channel = %v", m["channel"])
			}
		}
		return m["type"] == "resumed"
	})
	if got := joinSeqs(seqs); got != "[3 4]" {
		t.Fatalf("resync replayed %s, want [3 4]", got)
	}
	if int(m["count"].(float64)) != 2 {
		t.Fatalf("resumed frame %+v", m)
	}
}

// TestResyncOrderingContinuity — resync emits the resync directive,
// then the snapshot, then live deltas resume with prev_last_seq
// continuity (§10.9 ordering: snapshot seq is the client's new cursor).
func TestResyncOrderingContinuity(t *testing.T) {
	srv, c := dial(t, Config{ReplayBufferMsgs: 4})
	srv.SetSnapshotSource("depth", snapshotFunc(
		func(_ context.Context, channel string) (uint64, any, error) {
			return 9, map[string]any{"is_snapshot": true}, nil
		}))
	sendJSON(t, c, map[string]any{"action": "subscribe",
		"channels": []string{"depth@EUR/USD"}})
	readUntil(t, c, 2*time.Second, func(m map[string]any) bool {
		return m["type"] == "subscribed"
	})
	for i := 1; i <= 9; i++ {
		srv.Publish("depth@EUR/USD", uint64(i), map[string]any{"n": i})
	}
	readUntil(t, c, 2*time.Second, func(m map[string]any) bool {
		return m["type"] == "event" && int(m["seq"].(float64)) == 9
	})

	// Cursor behind the ring horizon → resync directive + snapshot, in
	// that wire order.
	sendJSON(t, c, map[string]any{
		"action": "resync", "channel": "depth@EUR/USD", "last_seq": 1,
	})
	var order []string
	m := readUntil(t, c, 2*time.Second, func(m map[string]any) bool {
		order = append(order, m["type"].(string))
		return m["type"] == "snapshot"
	})
	if len(order) < 2 || order[0] != "resync" || order[len(order)-1] != "snapshot" {
		t.Fatalf("frame order %v — want resync then snapshot", order)
	}
	if int(m["seq"].(float64)) != 9 {
		t.Fatalf("snapshot seq = %v, want 9 (ring tail)", m["seq"])
	}
	// Live deltas continue ordered after the snapshot.
	srv.Publish("depth@EUR/USD", 10, map[string]any{"n": 10})
	m = readUntil(t, c, 2*time.Second, func(m map[string]any) bool {
		return m["type"] == "event" && int(m["seq"].(float64)) == 10
	})
	_ = m
}

func joinSeqs(seqs []int) string {
	out := "["
	for i, s := range seqs {
		if i > 0 {
			out += " "
		}
		out += itoa(s)
	}
	return out + "]"
}

// dialPathOn dials a second conn against an existing test server.
func dialPathOn(t *testing.T, srv *Server, path string) (*Server, *websocket.Conn) {
	t.Helper()
	// The Server is already mounted by dialPath's httptest server — but
	// we don't have its URL. Re-dial via a fresh httptest on the same
	// Server (ServeHTTP is stateless per conn; sharing srv is the point).
	httpSrv := httptest.NewServer(http.HandlerFunc(srv.ServeHTTP))
	t.Cleanup(httpSrv.Close)
	u := "ws" + strings.TrimPrefix(httpSrv.URL, "http") + path
	c, _, err := websocket.DefaultDialer.Dial(u, nil)
	if err != nil {
		t.Fatalf("dial %s: %v", path, err)
	}
	t.Cleanup(func() { c.Close() })
	return srv, c
}

// waitStore blocks until the seq store reaches want for key.
func waitStore(t *testing.T, store SeqStore, key string, want uint64) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		v, _ := store.Load(context.Background(), key)
		if v >= want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("seq store[%s] = %d, want %d", key, v, want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
