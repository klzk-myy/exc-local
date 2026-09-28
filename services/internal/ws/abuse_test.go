// Phase-06 Tasks 6.3.7 / 6.3.16 / 6.3.21 — rate limiting, abuse
// protection, subscription caps, slow-consumer eviction and the
// disconnect_reason / OnDisconnect seam.
package ws

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// readUntilClose drains frames until the close frame arrives, returning
// every decoded frame and the terminal close error.
func readUntilClose(t *testing.T, c *websocket.Conn) ([]map[string]any, *websocket.CloseError) {
	t.Helper()
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	var frames []map[string]any
	for {
		_, raw, err := c.ReadMessage()
		if err != nil {
			ce, ok := err.(*websocket.CloseError)
			if !ok {
				t.Fatalf("read err %v — expected a close frame, frames=%v", err, frames)
			}
			return frames, ce
		}
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatalf("frame not JSON: %v", err)
		}
		frames = append(frames, m)
	}
}

// TestControlRateWarningEmitsRateInfo covers Task 6.3.7 items 2+5: a
// breached per-conn control bucket emits the canonical WS_RATE_EXCEEDED
// warning with retry_after_ms followed by the rate_info feedback frame.
func TestControlRateWarningEmitsRateInfo(t *testing.T) {
	srv := NewServer(Config{ControlRate: 2, RateWarnMax: 100})
	c := dial(t, srv)

	for i := 0; i < 4; i++ {
		send(t, c, map[string]any{"action": "ping"})
	}
	m := readMsg(t, c)
	if m["type"] != "pong" {
		t.Fatalf("frame1: %v", m)
	}
	m = readMsg(t, c)
	if m["type"] != "pong" {
		t.Fatalf("frame2: %v", m)
	}
	// Token bucket empty → warning + feedback, in that order.
	m = readMsg(t, c)
	if m["type"] != "error" || m["error"] != "WS_RATE_EXCEEDED" {
		t.Fatalf("want WS_RATE_EXCEEDED, got %v", m)
	}
	if v, ok := m["retry_after_ms"].(float64); !ok || v <= 0 {
		t.Fatalf("retry_after_ms missing/nonpositive: %v", m)
	}
	m = readMsg(t, c)
	if m["type"] != "rate_info" {
		t.Fatalf("want rate_info, got %v", m)
	}
	if _, ok := m["remaining"]; !ok {
		t.Fatalf("rate_info missing remaining: %v", m)
	}
	if _, ok := m["reset_ms"]; !ok {
		t.Fatalf("rate_info missing reset_ms: %v", m)
	}
	// Next throttle emits the same pair.
	m = readMsg(t, c)
	if m["error"] != "WS_RATE_EXCEEDED" {
		t.Fatalf("want second WS_RATE_EXCEEDED, got %v", m)
	}
	m = readMsg(t, c)
	if m["type"] != "rate_info" {
		t.Fatalf("want second rate_info, got %v", m)
	}
}

// TestRateWarningStrikesTerminate covers Task 6.3.7 item 2 tail:
// RateWarnMax warnings inside RateWarnWindow force-close the session
// with 4029 / ABUSE_DISCONNECT (§25 matrix WS_RATE_EXCEEDED close code).
func TestRateWarningStrikesTerminate(t *testing.T) {
	dc := make(chan DisconnectInfo, 1)
	srv := NewServer(Config{
		ControlRate:    1,
		RateWarnMax:    3,
		OnDisconnect:   func(i DisconnectInfo) { dc <- i },
		RateWarnWindow: time.Minute,
	})
	c := dial(t, srv)

	// 1 token of capacity → first ping OK; every later frame trips a
	// warning. The 3rd warning terminates.
	for i := 0; i < 6; i++ {
		send(t, c, map[string]any{"action": "ping"})
	}
	frames, ce := readUntilClose(t, c)
	if ce.Code != CloseRateExceeded {
		t.Fatalf("close code %d, want %d; frames=%v", ce.Code, CloseRateExceeded, frames)
	}
	if ce.Text != string(DisconnectAbuse) {
		t.Fatalf("close reason %q, want %q", ce.Text, DisconnectAbuse)
	}
	var sawRate, sawAbuse bool
	for _, m := range frames {
		if m["error"] == "WS_RATE_EXCEEDED" {
			sawRate = true
		}
		if m["error"] == "WS_ABUSE_DETECTED" {
			sawAbuse = true
		}
	}
	if !sawRate || !sawAbuse {
		t.Fatalf("expected WS_RATE_EXCEEDED warnings + terminal WS_ABUSE_DETECTED, frames=%v", frames)
	}
	select {
	case info := <-dc:
		if info.Reason != DisconnectAbuse {
			t.Fatalf("OnDisconnect reason %q, want ABUSE_DISCONNECT", info.Reason)
		}
		if info.CloseCode != CloseRateExceeded {
			t.Fatalf("OnDisconnect code %d, want %d", info.CloseCode, CloseRateExceeded)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("OnDisconnect not invoked")
	}
}

// TestChurnTerminatesWithAbuse covers the churn branch of Task 6.3.7
// item 3 + the item 6 discriminator: sustained subscribe/unsubscribe
// churn terminates with WS_ABUSE_DETECTED, close 4003 and the
// ABUSE_DISCONNECT wire reason.
func TestChurnTerminatesWithAbuse(t *testing.T) {
	dc := make(chan DisconnectInfo, 1)
	srv := NewServer(Config{
		MaxChurnPerWindow: 3,
		ChurnWindow:       5 * time.Second,
		OnDisconnect:      func(i DisconnectInfo) { dc <- i },
	})
	c := dial(t, srv)

	for i := 0; i < 4; i++ {
		send(t, c, map[string]any{
			"action":   "subscribe",
			"channels": []string{"public:ticker@EURUSD"}})
	}
	frames, ce := readUntilClose(t, c)
	if ce.Code != CloseAbuse {
		t.Fatalf("close code %d, want %d", ce.Code, CloseAbuse)
	}
	if ce.Text != string(DisconnectAbuse) {
		t.Fatalf("close reason %q, want %q", ce.Text, DisconnectAbuse)
	}
	var sawAbuse bool
	for _, m := range frames {
		if m["error"] == "WS_ABUSE_DETECTED" {
			sawAbuse = true
		}
	}
	if !sawAbuse {
		t.Fatalf("missing terminal WS_ABUSE_DETECTED frame: %v", frames)
	}
	select {
	case info := <-dc:
		if info.Reason != DisconnectAbuse || info.CloseCode != CloseAbuse {
			t.Fatalf("OnDisconnect %+v", info)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("OnDisconnect not invoked")
	}
}

// fakeIPLimiter denies after `n` admissions — the Task 6.3.7 IP-aggregate
// hook test double.
type fakeIPLimiter struct {
	n     int
	calls int
	ms    int64
}

func (f *fakeIPLimiter) Allow(string, time.Time) (int64, bool) {
	f.calls++
	if f.calls > f.n {
		return f.ms, false
	}
	return 0, true
}

// TestIPAggregateLimiterHook verifies a deny from the cross-connection
// IP limiter surfaces as the same WS_RATE_EXCEEDED warning + rate_info
// pair the per-conn bucket produces.
func TestIPAggregateLimiterHook(t *testing.T) {
	srv := NewServer(Config{
		ControlRate: 1000, // per-conn bucket never trips — isolates the hook
		RateWarnMax: 100,
		IPLimiter:   &fakeIPLimiter{n: 2, ms: 750},
	})
	c := dial(t, srv)
	for i := 0; i < 4; i++ {
		send(t, c, map[string]any{"action": "ping"})
	}
	readMsg(t, c) // pong
	readMsg(t, c) // pong
	m := readMsg(t, c)
	if m["error"] != "WS_RATE_EXCEEDED" || m["retry_after_ms"].(float64) != 750 {
		t.Fatalf("IP-limited warning: %v", m)
	}
	m = readMsg(t, c)
	if m["type"] != "rate_info" {
		t.Fatalf("want rate_info, got %v", m)
	}
}

// TestReconnectThrottle429 covers Task 6.3.21 item 2: more than
// ReconnectRate upgrades per ReconnectWindow per IP → HTTP 429 with
// RATE_LIMIT_TIER_EXCEEDED and a Retry-After header.
func TestReconnectThrottle429(t *testing.T) {
	srv := NewServer(Config{ReconnectRate: 2, ReconnectWindow: time.Minute})
	c1 := dial(t, srv)
	defer c1.Close()
	c2 := dial(t, srv)
	defer c2.Close()

	httpSrv := httptest.NewServer(srv)
	defer httpSrv.Close()
	url := "ws" + strings.TrimPrefix(httpSrv.URL, "http") + "/ws/v1"
	_, resp, err := websocket.DefaultDialer.Dial(url, nil)
	if err == nil {
		t.Fatal("third conn should be refused")
	}
	if resp == nil || resp.StatusCode != 429 {
		t.Fatalf("want 429, got %v", resp)
	}
	if resp.Header.Get("Retry-After") == "" {
		t.Fatal("missing Retry-After header")
	}
	var body map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&body)
	resp.Body.Close()
	if body["error"] != "RATE_LIMIT_TIER_EXCEEDED" {
		t.Fatalf("body: %v", body)
	}
	if body["retry_after"].(float64) <= 0 {
		t.Fatalf("retry_after missing: %v", body)
	}
}

// TestSubAdmitHookEnforcesPerChannelCap covers the Task 6.3.16 plumbing:
// the generic cap stays, and the marketdata-owned per-channel-type
// split (20 L2 / 5 L3 in production) lands through SubAdmit.
func TestSubAdmitHookEnforcesPerChannelCap(t *testing.T) {
	srv := NewServer(Config{
		SubAdmit: func(c *Conn, ch string) (string, bool) {
			// Test policy: max 1 depth@ subscription per conn.
			if strings.HasPrefix(ch, "depth@") {
				n := 0
				for s := range c.subs {
					if strings.HasPrefix(s, "depth@") {
						n++
					}
				}
				if n >= 1 {
					return "WS_MAX_SUBSCRIPTIONS_EXCEEDED", false
				}
			}
			return "", true
		},
	})
	c := dial(t, srv)
	send(t, c, map[string]any{
		"action":   "subscribe",
		"channels": []string{"depth@EURUSD", "depth@GBPUSD", "public:ticker@EURUSD"}})

	m := readMsg(t, c)
	if m["error"] != "WS_MAX_SUBSCRIPTIONS_EXCEEDED" {
		t.Fatalf("per-type cap rejection: %v", m)
	}
	m = readMsg(t, c)
	if m["type"] != "subscribed" {
		t.Fatalf("want subscribed ack, got %v", m)
	}
	chans, _ := m["channels"].([]any)
	if len(chans) != 2 || chans[0] != "depth@EURUSD" || chans[1] != "public:ticker@EURUSD" {
		t.Fatalf("accepted channels: %v", m)
	}
}

// TestBackpressureEvictionSynthetic drives the Task 6.3.21 queue monitor
// directly: a conn whose outbound queue stays saturated for
// EvictConsecutive ticks is evicted with 4008 / WS_SLOW_CONSUMER_DROP.
func TestBackpressureEvictionSynthetic(t *testing.T) {
	srv := NewServer(Config{
		OutboundBuffer:      4,
		OutboundMaxBytes:    1 << 20, // byte path stays under — depth-only check
		EvictCheckInterval:  10 * time.Millisecond,
		EvictConsecutive:    3,
		SlowConsumerTimeout: time.Minute,
	})
	c := &Conn{
		srv:      srv,
		out:      make(chan []byte, srv.cfg.OutboundBuffer),
		done:     make(chan struct{}),
		pumpDone: make(chan struct{}),
	}
	go c.backpressureLoop()

	// Saturate and hold — no pump draining.
	for i := 0; i < cap(c.out); i++ {
		c.out <- []byte(strings.Repeat("x", 64))
		c.outBytes.Add(64)
	}
	select {
	case <-c.done:
	case <-time.After(3 * time.Second):
		t.Fatal("conn not evicted after sustained saturation")
	}
	if got := int(c.closeCode.Load()); got != CloseSlowConsumer {
		t.Fatalf("close code %d, want %d", got, CloseSlowConsumer)
	}
	if why, _ := c.closeWhy.Load().(string); why != "WS_SLOW_CONSUMER_DROP" {
		t.Fatalf("close reason %q", why)
	}
	if c.disconnectReason() != DisconnectNetwork {
		t.Fatalf("disc reason %q", c.disconnectReason())
	}
}

// TestBackpressureEvictionBytes exercises the byte-threshold path of the
// queue monitor.
func TestBackpressureEvictionBytes(t *testing.T) {
	srv := NewServer(Config{
		OutboundBuffer:      100,
		OutboundMaxBytes:    10,  // one 64B frame exceeds
		EvictQueueDepth:     100, // depth path stays under
		EvictCheckInterval:  10 * time.Millisecond,
		EvictConsecutive:    2,
		SlowConsumerTimeout: time.Minute,
	})
	c := &Conn{
		srv:      srv,
		out:      make(chan []byte, srv.cfg.OutboundBuffer),
		done:     make(chan struct{}),
		pumpDone: make(chan struct{}),
	}
	go c.backpressureLoop()
	c.out <- []byte(strings.Repeat("x", 64))
	c.outBytes.Add(64)
	select {
	case <-c.done:
	case <-time.After(3 * time.Second):
		t.Fatal("conn not evicted on byte threshold")
	}
	if got := int(c.closeCode.Load()); got != CloseSlowConsumer {
		t.Fatalf("close code %d, want %d", got, CloseSlowConsumer)
	}
}

// TestSlowConsumerEvictionLive is the wire-level Task 6.3.21 AC: a
// client that stops reading lets the outbound queue saturate; the
// monitor (NOT the 30s enqueue-block timeout) evicts with 4008 and the
// close frame reaches the client once it drains.
func TestSlowConsumerEvictionLive(t *testing.T) {
	srv := NewServer(Config{
		OutboundBuffer:      2,
		OutboundMaxBytes:    1 << 22,
		EvictCheckInterval:  10 * time.Millisecond,
		EvictConsecutive:    4,
		SlowConsumerTimeout: 30 * time.Second,
	})
	c := dial(t, srv)
	send(t, c, map[string]any{
		"action": "subscribe", "channels": []string{"pub:x"}})
	readMsg(t, c) // subscribed ack

	// Publisher: large frames until the queue backs up and the conn is
	// evicted; enqueue escapes via done on teardown.
	payload := strings.Repeat("p", 32<<10)
	var published atomic.Int32
	pub := make(chan struct{})
	go func() {
		defer close(pub)
		for i := 0; i < 400; i++ {
			srv.Publish("pub:x", payload)
			published.Add(1)
		}
	}()

	// Stall the client so TCP buffers + server queue saturate.
	time.Sleep(700 * time.Millisecond)

	// Now drain: expect queued events, then the 4008 close.
	_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
	var ce *websocket.CloseError
	for {
		_, _, err := c.ReadMessage()
		if err != nil {
			var ok bool
			ce, ok = err.(*websocket.CloseError)
			if !ok {
				t.Fatalf("read err %v — expected close frame", err)
			}
			break
		}
	}
	if ce.Code != CloseSlowConsumer {
		t.Fatalf("close code %d, want %d", ce.Code, CloseSlowConsumer)
	}
	<-pub
	if published.Load() <= 2 {
		t.Fatalf("publish loop never saturated the queue (%d)", published.Load())
	}
}

// TestOnDisconnectClientClose covers the CLIENT_DISCONNECT branch of
// the Task 6.3.7 item 6 discriminator.
func TestOnDisconnectClientClose(t *testing.T) {
	dc := make(chan DisconnectInfo, 1)
	srv := NewServer(Config{OnDisconnect: func(i DisconnectInfo) { dc <- i }})
	c := dial(t, srv)
	_ = c.WriteMessage(websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
	select {
	case info := <-dc:
		if info.Reason != DisconnectClient {
			t.Fatalf("reason %q, want CLIENT_DISCONNECT", info.Reason)
		}
		if info.CloseCode != websocket.CloseNormalClosure {
			t.Fatalf("code %d, want 1000", info.CloseCode)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("OnDisconnect not invoked")
	}
}

// TestOnDisconnectNetworkTimeout covers the NETWORK_TIMEOUT branch: an
// abrupt TCP drop (no close frame) classifies as a network failure —
// the CoD-eligible bucket.
func TestOnDisconnectNetworkTimeout(t *testing.T) {
	dc := make(chan DisconnectInfo, 1)
	srv := NewServer(Config{OnDisconnect: func(i DisconnectInfo) { dc <- i }})
	c := dial(t, srv)
	// Abrupt close — no close frame on the wire.
	tcp := c.UnderlyingConn()
	c.Close()
	_ = tcp
	select {
	case info := <-dc:
		if info.Reason != DisconnectNetwork {
			t.Fatalf("reason %q, want NETWORK_TIMEOUT", info.Reason)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("OnDisconnect not invoked")
	}
}

// TestAnonymousDisconnectCarriesNoAccount sanity-checks AccountID=0 on
// anonymous teardowns.
func TestAnonymousDisconnectCarriesNoAccount(t *testing.T) {
	var got atomic.Int64
	done := make(chan struct{})
	var once sync.Once
	srv := NewServer(Config{OnDisconnect: func(i DisconnectInfo) {
		got.Store(i.AccountID)
		once.Do(func() { close(done) })
	}})
	c := dial(t, srv)
	_ = c.WriteMessage(websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("OnDisconnect not invoked")
	}
	if got.Load() != 0 {
		t.Fatalf("anonymous conn carried account %d", got.Load())
	}
}
