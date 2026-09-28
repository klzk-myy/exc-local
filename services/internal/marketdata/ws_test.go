// Tasks 6.3.1/6.3.9/6.3.10 — end-to-end WS tests over real sockets.
package marketdata

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"exchange/internal/ws"
)

// dial starts a test server + websocket client.
func dial(t *testing.T, cfg Config) (*Server, *websocket.Conn) {
	t.Helper()
	srv := NewServer(cfg)
	httpSrv := httptest.NewServer(http.HandlerFunc(srv.ServeHTTP))
	t.Cleanup(httpSrv.Close)
	u := "ws" + strings.TrimPrefix(httpSrv.URL, "http") + "/ws/v1/marketdata"
	c, _, err := websocket.DefaultDialer.Dial(u, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { c.Close() })
	return srv, c
}

// sendJSON writes one client frame.
func sendJSON(t *testing.T, c *websocket.Conn, v any) {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := c.WriteMessage(websocket.TextMessage, b); err != nil {
		t.Fatalf("write: %v", err)
	}
}

// readFrame reads one JSON frame with a deadline.
func readFrame(t *testing.T, c *websocket.Conn, d time.Duration) map[string]any {
	t.Helper()
	_ = c.SetReadDeadline(time.Now().Add(d))
	_, b, err := c.ReadMessage()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("frame not JSON: %v — %s", err, b)
	}
	return m
}

// readUntil reads frames until pred matches or deadline expires.
func readUntil(t *testing.T, c *websocket.Conn, d time.Duration,
	pred func(map[string]any) bool) map[string]any {
	t.Helper()
	end := time.Now().Add(d)
	for time.Now().Before(end) {
		m := readFrame(t, c, time.Until(end))
		if pred(m) {
			return m
		}
	}
	t.Fatal("no matching frame before deadline")
	return nil
}

// TestSubscribeLimitsL2 — 20 book@/depth@ channels accepted; the 21st is
// rejected WS_MAX_SUBSCRIPTIONS_EXCEEDED (§24 #84).
func TestSubscribeLimitsL2(t *testing.T) {
	_, c := dial(t, Config{})

	channels := make([]string, 0, 21)
	for i := 0; i < 20; i++ {
		channels = append(channels, fmt.Sprintf("book@SYM%02d/XX", i))
	}
	sendJSON(t, c, map[string]any{"action": "subscribe", "channels": channels})
	m := readUntil(t, c, 2*time.Second, func(m map[string]any) bool {
		return m["type"] == "subscribed"
	})
	if got := int(m["total"].(float64)); got != 20 {
		t.Fatalf("total subs = %d, want 20", got)
	}

	sendJSON(t, c, map[string]any{"action": "subscribe",
		"channels": []string{"book@OVER/XX"}})
	m = readUntil(t, c, 2*time.Second, func(m map[string]any) bool {
		return m["type"] == "error"
	})
	if m["error"] != "WS_MAX_SUBSCRIPTIONS_EXCEEDED" {
		t.Fatalf("21st L2 sub: error = %v", m["error"])
	}

	// depth@ shares the L2 budget — still saturated.
	sendJSON(t, c, map[string]any{"action": "subscribe",
		"channels": []string{"depth@EUR/USD"}})
	m = readUntil(t, c, 2*time.Second, func(m map[string]any) bool {
		return m["type"] == "error"
	})
	if m["error"] != "WS_MAX_SUBSCRIPTIONS_EXCEEDED" {
		t.Fatalf("depth@ under saturated L2 budget: error = %v", m["error"])
	}
}

// TestSubscribeLimitsL3 — 5 L3-class channels accepted; the 6th rejected.
func TestSubscribeLimitsL3(t *testing.T) {
	_, c := dial(t, Config{})
	channels := make([]string, 0, 6)
	for i := 0; i < 5; i++ {
		channels = append(channels, fmt.Sprintf("l3@SYM%d/XX", i))
	}
	sendJSON(t, c, map[string]any{"action": "subscribe", "channels": channels})
	m := readUntil(t, c, 2*time.Second, func(m map[string]any) bool {
		return m["type"] == "subscribed"
	})
	if got := int(m["total"].(float64)); got != 5 {
		t.Fatalf("total = %d, want 5", got)
	}
	sendJSON(t, c, map[string]any{"action": "subscribe",
		"channels": []string{"l3Book@EUR/USD"}})
	m = readUntil(t, c, 2*time.Second, func(m map[string]any) bool {
		return m["type"] == "error"
	})
	if m["error"] != "WS_MAX_SUBSCRIPTIONS_EXCEEDED" {
		t.Fatalf("6th L3 sub: error = %v", m["error"])
	}
}

// TestSubscribeUnknownChannel — untyped channels reject INVALID_REQUEST
// and never bind (a typo must not reserve a slot that never carries data).
func TestSubscribeUnknownChannel(t *testing.T) {
	_, c := dial(t, Config{})
	sendJSON(t, c, map[string]any{"action": "subscribe",
		"channels": []string{"bogus@EUR/USD", "book@EUR/USD"}})
	errF := readUntil(t, c, 2*time.Second, func(m map[string]any) bool {
		return m["type"] == "error"
	})
	if errF["error"] != "INVALID_REQUEST" {
		t.Fatalf("unknown channel error = %v", errF["error"])
	}
	sub := readUntil(t, c, 2*time.Second, func(m map[string]any) bool {
		return m["type"] == "subscribed"
	})
	if got := int(sub["total"].(float64)); got != 1 {
		t.Fatalf("total = %d, want 1 (partial batch still binds valid)", got)
	}
}

// TestHeartbeatStaleClientDrop — a client that never pongs is dropped
// inside ~PongWait (Task 6.3.1: ping 30s / pong 60s — shortened here).
func TestHeartbeatStaleClientDrop(t *testing.T) {
	_, c := dial(t, Config{
		PingInterval: 40 * time.Millisecond,
		PongWait:     120 * time.Millisecond,
	})
	// Suppress the gorilla auto-pong (PingHandler is the client's reply
	// hook, not PongHandler) so the conn is genuinely stale.
	c.SetPingHandler(func(string) error { return nil })

	start := time.Now()
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, _, err := c.ReadMessage(); err != nil {
			if time.Since(start) > 2*time.Second {
				t.Fatal("drop took too long — read deadline, not the server, fired")
			}
			return // server closed — heartbeat enforcement worked
		}
	}
	t.Fatal("stale connection survived past pong deadline")
}

// TestHeartbeatHealthyClientSurvives — default pong handler answers the
// server ping; the conn stays up across several ping intervals.
func TestHeartbeatHealthyClientSurvives(t *testing.T) {
	_, c := dial(t, Config{
		PingInterval: 40 * time.Millisecond,
		PongWait:     200 * time.Millisecond,
	})
	sendJSON(t, c, map[string]any{"action": "ping"})
	m := readFrame(t, c, 2*time.Second)
	if m["type"] != "pong" {
		t.Fatalf("app ping reply = %v", m["type"])
	}
	// Stay alive across ~3 ping intervals.
	time.Sleep(150 * time.Millisecond)
	sendJSON(t, c, map[string]any{"action": "ping"})
	m = readFrame(t, c, 2*time.Second)
	if m["type"] != "pong" {
		t.Fatalf("second app ping reply = %v", m["type"])
	}
}

// TestRequestReplyCorrelation — {"action":"request","id":N,"method":"time"}
// answers a correlated ACK; unknown methods answer INVALID_REQUEST.
func TestRequestReplyCorrelation(t *testing.T) {
	_, c := dial(t, Config{})
	sendJSON(t, c, map[string]any{
		"action": "request", "id": 42, "method": "time",
	})
	m := readUntil(t, c, 2*time.Second, func(m map[string]any) bool {
		return m["type"] == "response"
	})
	if m["request_id"] != "42" || m["status"] != "ACK" || m["action"] != "request" {
		t.Fatalf("response = %+v", m)
	}
	data, _ := m["data"].(map[string]any)
	res, _ := data["result"].(map[string]any)
	if res["server_time_ms"] == nil {
		t.Fatalf("time result missing server_time_ms: %v", data)
	}

	sendJSON(t, c, map[string]any{
		"action": "request", "request_id": "abc-9", "method": "nope",
	})
	m = readUntil(t, c, 2*time.Second, func(m map[string]any) bool {
		return m["type"] == "error"
	})
	if m["request_id"] != "abc-9" || m["error"] != "INVALID_REQUEST" {
		t.Fatalf("unknown method response = %+v", m)
	}
}

// TestRegisterMethodDispatch — Server.RegisterMethod routes custom
// request methods through the same correlated envelope.
func TestRegisterMethodDispatch(t *testing.T) {
	srv, c := dial(t, Config{})
	err := srv.RegisterMethod("echo", func(_ context.Context, _ *ws.Session, p json.RawMessage) (any, error) {
		return map[string]any{"echo": string(p)}, nil
	})
	if err != nil {
		t.Fatalf("RegisterMethod: %v", err)
	}
	sendJSON(t, c, map[string]any{
		"action": "request", "id": "e1", "method": "echo",
		"params": map[string]any{"hello": "world"},
	})
	m := readUntil(t, c, 2*time.Second, func(m map[string]any) bool {
		return m["type"] == "response"
	})
	if m["request_id"] != "e1" || m["status"] != "ACK" {
		t.Fatalf("echo response = %+v", m)
	}
	if err := srv.RegisterMethod("time", nil); err == nil {
		t.Fatal("registering over a builtin accepted")
	}
}

// TestEventFanoutAndResume — published frames reach subscribers; resume
// with last_seq replays the missed range verbatim (Task 6.3.9).
func TestEventFanoutAndResume(t *testing.T) {
	srv, c := dial(t, Config{})
	sendJSON(t, c, map[string]any{"action": "subscribe",
		"channels": []string{"book@EUR/USD"}})
	readUntil(t, c, 2*time.Second, func(m map[string]any) bool {
		return m["type"] == "subscribed"
	})

	for i := 1; i <= 5; i++ {
		srv.Publish("book@EUR/USD", uint64(i), map[string]any{"n": i})
	}
	// Drain the 5 live events (they also populate the ring).
	for i := 1; i <= 5; i++ {
		m := readUntil(t, c, 2*time.Second, func(m map[string]any) bool {
			return m["type"] == "event"
		})
		if int(m["seq"].(float64)) != i {
			t.Fatalf("live event seq = %v, want %d", m["seq"], i)
		}
	}

	// Resume from seq 2 → replay 3,4,5 verbatim then "resumed".
	sendJSON(t, c, map[string]any{
		"action": "resume", "channel": "book@EUR/USD", "last_seq": 2,
	})
	var seqs []int
	m := readUntil(t, c, 2*time.Second, func(m map[string]any) bool {
		if m["type"] == "event" {
			seqs = append(seqs, int(m["seq"].(float64)))
		}
		return m["type"] == "resumed"
	})
	if fmt.Sprint(seqs) != "[3 4 5]" {
		t.Fatalf("replayed seqs %v, want [3 4 5]", seqs)
	}
	if int(m["count"].(float64)) != 3 || int(m["to_seq"].(float64)) != 5 {
		t.Fatalf("resumed frame %+v", m)
	}

	// Live streaming continues post-resume.
	srv.Publish("book@EUR/USD", 6, map[string]any{"n": 6})
	m = readUntil(t, c, 2*time.Second, func(m map[string]any) bool {
		return m["type"] == "event" && m["seq"] == float64(6)
	})
	_ = m
}

// TestResumeGapTooLarge — a cursor outside the ring horizon gets an
// explicit resync + snapshot when a SnapshotSource is wired.
func TestResumeGapTooLarge(t *testing.T) {
	srv, c := dial(t, Config{ReplayBufferMsgs: 4})

	// Wire a stub snapshot source for "book".
	srv.SetSnapshotSource("book", snapshotFunc(
		func(_ context.Context, channel string) (uint64, any, error) {
			return 9, map[string]any{"symbol": "EUR/USD", "levels": true}, nil
		}))

	// Fill the ring with 9 frames; only the last 4 fit.
	for i := 1; i <= 9; i++ {
		srv.Publish("book@EUR/USD", uint64(i), map[string]any{"n": i})
	}
	sendJSON(t, c, map[string]any{
		"action": "resume", "channel": "book@EUR/USD", "last_seq": 1,
	})
	m := readUntil(t, c, 2*time.Second, func(m map[string]any) bool {
		return m["type"] == "resync"
	})
	if m["reason"] != "gap_too_large" || m["channel"] != "book@EUR/USD" {
		t.Fatalf("resync frame %+v", m)
	}
	m = readUntil(t, c, 2*time.Second, func(m map[string]any) bool {
		return m["type"] == "snapshot"
	})
	if int(m["seq"].(float64)) != 9 || m["reason"] != "gap_too_large" {
		t.Fatalf("snapshot frame %+v", m)
	}
}

// TestResumeInvalidSeq — a cursor ahead of the tail is invalid.
func TestResumeInvalidSeq(t *testing.T) {
	srv, c := dial(t, Config{})
	for i := 1; i <= 3; i++ {
		srv.Publish("book@EUR/USD", uint64(i), map[string]any{"n": i})
	}
	sendJSON(t, c, map[string]any{
		"action": "resume", "channel": "book@EUR/USD", "last_seq": 99,
	})
	m := readUntil(t, c, 2*time.Second, func(m map[string]any) bool {
		return m["type"] == "resync"
	})
	if m["reason"] != "invalid_sequence" {
		t.Fatalf("resync %+v", m)
	}
}

// TestConcurrentWritesRace — hammer Publish + request handling + control
// frames concurrently; every frame the client reads must be intact JSON
// (frame interleaving = corruption). Race detector exercises the paths.
func TestConcurrentWritesRace(t *testing.T) {
	srv, c := dial(t, Config{})
	sendJSON(t, c, map[string]any{"action": "subscribe",
		"channels": []string{"book@EUR/USD"}})
	readUntil(t, c, 2*time.Second, func(m map[string]any) bool {
		return m["type"] == "subscribed"
	})

	var wg sync.WaitGroup
	// Producer goroutines.
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func(base int) {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				srv.Publish("book@EUR/USD", uint64(base*1000+i),
					map[string]any{"g": base, "n": i})
			}
		}(g)
	}
	// Control-frame goroutine.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 20; i++ {
			b, _ := json.Marshal(map[string]any{
				"action": "request", "id": fmt.Sprintf("r%d", i), "method": "ping",
			})
			_ = c.WriteMessage(websocket.TextMessage, b)
		}
	}()
	wg.Wait()

	// Drain frames for a bit — every one must be standalone valid JSON.
	_ = c.SetReadDeadline(time.Now().Add(800 * time.Millisecond))
	responses := 0
	for {
		_, b, err := c.ReadMessage()
		if err != nil {
			break
		}
		var m map[string]any
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatalf("interleaved/corrupt frame: %v — %s", err, b)
		}
		if m["type"] == "response" {
			responses++
		}
	}
	if responses == 0 {
		t.Fatal("no request responses observed during concurrent load")
	}
}

// snapshotFunc adapts a func to SnapshotSource for tests.
type snapshotFunc func(ctx context.Context, channel string) (uint64, any, error)

func (f snapshotFunc) Snapshot(ctx context.Context, channel string) (uint64, any, error) {
	return f(ctx, channel)
}
