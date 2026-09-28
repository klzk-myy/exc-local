package ws

import (
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

// Registered RFC 6455 private-use close codes (spec §10.6, §23 and the
// §25 implementation-matrix WS row which pins 4029 for WS_RATE_EXCEEDED).
const (
	CloseAuthExpired  = 4019 // AUTH_EXPIRED — re-authenticate required
	CloseAbuse        = 4003 // WS_ABUSE_DETECTED — subscription churn abuse
	CloseSlowConsumer = 4008 // policy violation — outbound buffer not drained
	CloseRateExceeded = 4029 // WS_RATE_EXCEEDED strikes exhausted — forced disconnect
)

// DisconnectReason is the §24 #245 / Phase-06 Task 6.3.7 item 6
// disconnect_reason discriminator reported to the OnDisconnect consumer
// (the dead-man/cancel-on-disconnect owner). The three task-pinned wire
// values are CLIENT_DISCONNECT, NETWORK_TIMEOUT and ABUSE_DISCONNECT;
// SERVER_SHUTDOWN additionally classifies server-initiated closes that
// fall outside that enum (drain advisory, AUTH_EXPIRED teardown, normal
// unregister) so the consumer can tell "endpoint went away" from
// "client vanished". CoD eligibility per spec: CLIENT_DISCONNECT and
// NETWORK_TIMEOUT trigger; ABUSE_DISCONNECT and SERVER_SHUTDOWN do NOT
// (abuse disconnects leave orders resting but block new order entry
// until reconnection + re-authentication — an unauthenticated fresh
// conn is inherently blocked, so no extra machinery is needed here).
// The 1-mass-cancel-per-account-per-5s CoD throttle lives in the
// dead-man service that consumes OnDisconnect, not in this package.
type DisconnectReason string

const (
	DisconnectClient  DisconnectReason = "CLIENT_DISCONNECT"
	DisconnectNetwork DisconnectReason = "NETWORK_TIMEOUT"
	DisconnectAbuse   DisconnectReason = "ABUSE_DISCONNECT"
	DisconnectServer  DisconnectReason = "SERVER_SHUTDOWN"
)

// DisconnectInfo is the terminal-conn record delivered to
// Config.OnDisconnect — the dead-man/CoD consumer's input.
type DisconnectInfo struct {
	RemoteIP  string
	AccountID int64 // 0 for anonymous connections
	Reason    DisconnectReason
	CloseCode int
}

// subscription is one active channel binding on a connection.
type subscription struct {
	channel string
	private bool
	seq     uint64 // per (conn, channel) monotonic event sequence
}

// Conn is one upgraded WebSocket connection. All ws writes happen in
// writePump (gorilla requires a single writer); producers enqueue framed
// bytes on out. Teardown: closeConn flips done once, the pump emits the
// close frame and returns, serve() unregisters the conn.
type Conn struct {
	srv      *Server
	ws       *websocket.Conn
	remoteIP string
	path     string // upgrade path — the WS signature surface

	out        chan []byte
	outBytes   atomic.Int64 // bytes currently queued in out (slow-consumer monitor)
	done       chan struct{}
	pumpDone   chan struct{} // closed by writePump AFTER the close frame goes out
	closeOne   sync.Once
	closeCode  atomic.Int32
	closeWhy   atomic.Value // string — wire close reason
	discReason atomic.Value // DisconnectReason — terminal classification

	sessMu sync.Mutex
	sess   Session

	subMu sync.Mutex
	subs  map[string]*subscription

	// expiryCh (cap 1) pokes the expiry watcher when sess.ExpiresAt moves.
	expiryCh chan struct{}

	rateMu sync.Mutex
	tokens float64     // control-frame token bucket
	rateAt time.Time   // last refill
	warns  []time.Time // recent WS_RATE_EXCEEDED warnings

	churnMu sync.Mutex
	churn   []time.Time // recent subscribe/unsubscribe timestamps

	sem chan struct{} // concurrent order-action bound
}

// session returns a copy of the current session under lock.
func (c *Conn) session() Session {
	c.sessMu.Lock()
	defer c.sessMu.Unlock()
	return c.sess
}

// setSession replaces the session and pokes the expiry watcher.
func (c *Conn) setSession(s Session) {
	c.sessMu.Lock()
	c.sess = s
	c.sessMu.Unlock()
	select {
	case c.expiryCh <- struct{}{}:
	default:
	}
}

// enqueue offers a frame to the write pump. A client that fails to drain
// the 1024-deep buffer within SlowConsumerTimeout is dropped with 4008
// (spec §10.6 slow-consumer policy) — the conflation pipeline must never
// stall on a wedged socket.
func (c *Conn) enqueue(b []byte) {
	t := time.NewTimer(c.srv.cfg.SlowConsumerTimeout)
	defer t.Stop()
	select {
	case c.out <- b:
		c.outBytes.Add(int64(len(b)))
	case <-c.done:
	case <-t.C:
		c.closeWith(CloseSlowConsumer, DisconnectNetwork, "WS_SLOW_CONSUMER_DROP")
	}
}

// closeWith initiates teardown: the write pump emits the close frame
// (best-effort 1s deadline) and its deferred ws.Close() frees the
// socket, failing the read loop's next ReadMessage. Idempotent — the
// first close wins for BOTH the wire code/reason and the recorded
// DisconnectReason, so a server-initiated teardown (drain, abuse, slow
// consumer) is never reclassified by the read error it provokes. The
// socket must NOT be closed here: doing so races the pump's flushOut +
// close-frame write and the client sees an abnormal closure (1006)
// instead of the real close code.
func (c *Conn) closeWith(code int, dr DisconnectReason, reason string) {
	c.closeOne.Do(func() {
		c.discReason.Store(dr)
		c.closeCode.Store(int32(code))
		c.closeWhy.Store(reason)
		close(c.done)
	})
}

// disconnectReason returns the terminal classification; conns that die
// before any classification degrade to DisconnectServer (fail-closed —
// the conn was torn down by this side or never fully established).
func (c *Conn) disconnectReason() DisconnectReason {
	if v := c.discReason.Load(); v != nil {
		if dr, ok := v.(DisconnectReason); ok {
			return dr
		}
	}
	return DisconnectServer
}

// writePump is the sole writer and owns socket teardown. On done it
// first drains any queued outbound frames (a terminal error frame must
// precede its close frame on the wire), emits the close control frame,
// then returns; the deferred ws.Close() frees the socket. pumpDone lets
// unregister wait for the flush instead of racing it. Ping frames keep
// intermediaries from reaping idle conns.
func (c *Conn) writePump() {
	ping := time.NewTicker(c.srv.cfg.PingInterval)
	defer ping.Stop()
	defer close(c.pumpDone)
	defer c.ws.Close()
	for {
		select {
		case b := <-c.out:
			c.outBytes.Add(-int64(len(b)))
			_ = c.ws.SetWriteDeadline(c.srv.cfg.Now().Add(10 * time.Second))
			if err := c.ws.WriteMessage(websocket.TextMessage, b); err != nil {
				return // conn dead; serve() cleanup path handles registry
			}
		case <-ping.C:
			_ = c.ws.SetWriteDeadline(c.srv.cfg.Now().Add(10 * time.Second))
			if err := c.ws.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		case <-c.done:
			c.flushOut()
			code := int(c.closeCode.Load())
			why, _ := c.closeWhy.Load().(string)
			_ = c.ws.SetWriteDeadline(c.srv.cfg.Now().Add(time.Second))
			_ = c.ws.WriteMessage(websocket.CloseMessage,
				websocket.FormatCloseMessage(code, why))
			return
		}
	}
}

// flushOut drains queued outbound frames best-effort on teardown so a
// terminal error frame (AUTH_EXPIRED, WS_ABUSE_DETECTED, UNAUTHORIZED)
// reaches the client ahead of the close frame that triggered teardown.
func (c *Conn) flushOut() {
	for {
		select {
		case b := <-c.out:
			c.outBytes.Add(-int64(len(b)))
			_ = c.ws.SetWriteDeadline(c.srv.cfg.Now().Add(time.Second))
			if err := c.ws.WriteMessage(websocket.TextMessage, b); err != nil {
				return
			}
		default:
			return
		}
	}
}

// sendError emits a canonical WS error frame (§10.5 item 4 field order).
func (c *Conn) sendError(requestID, action, code, message string, retryAfterMs int64) {
	b, err := marshalFrame(errorFrame{
		Type:         "error",
		RequestID:    requestID,
		Action:       action,
		Error:        code,
		Message:      message,
		TsMs:         c.srv.cfg.Now().UnixMilli(),
		RetryAfterMs: retryAfterMs,
	})
	if err != nil {
		return // never block the read loop on a marshal surprise
	}
	c.enqueue(b)
}

// sendResponse emits the ACK/NACK response frame (§10.5 item 6).
func (c *Conn) sendResponse(requestID, action, status string, data any) {
	if status == "" {
		status = "ACK"
	}
	b, err := marshalFrame(responseFrame{
		Type: "response", RequestID: requestID, Action: action,
		Status: status, Data: data, TsMs: c.srv.cfg.Now().UnixMilli(),
	})
	if err != nil {
		c.sendError(requestID, action, "INTERNAL_ERROR", "response marshal failure", 0)
		return
	}
	c.enqueue(b)
}

// rateVerdict is one inbound-frame admission decision (Task 6.3.7).
type rateVerdict struct {
	ok           bool
	retryAfterMs int64 // WS _ms convention (§10.5 item 5) for WS_RATE_EXCEEDED
	remaining    int64 // tokens left in the bucket (rate_info.remaining)
	resetMs      int64 // ms until the bucket refills to capacity (rate_info.reset_ms)
}

// allowControl is the per-connection control-frame token bucket
// (spec §10.6: breach emits WS_RATE_EXCEEDED). Bucket refills at
// ControlRate tokens/s with a 1-second burst capacity.
func (c *Conn) allowControl() rateVerdict {
	c.rateMu.Lock()
	defer c.rateMu.Unlock()
	now := c.srv.cfg.Now()
	rate := float64(c.srv.cfg.ControlRate)
	if c.rateAt.IsZero() {
		c.tokens = rate
		c.rateAt = now
	}
	c.tokens += now.Sub(c.rateAt).Seconds() * rate
	if c.tokens > rate {
		c.tokens = rate
	}
	c.rateAt = now
	resetMs := func() int64 { // ms until the bucket refills to capacity
		ms := int64(((rate - c.tokens) / rate) * 1000)
		if ms < 0 {
			return 0
		}
		return ms
	}
	if c.tokens >= 1 {
		c.tokens--
		return rateVerdict{ok: true, remaining: int64(c.tokens), resetMs: resetMs()}
	}
	return rateVerdict{
		retryAfterMs: int64((1-c.tokens)/rate*1000) + 1,
		resetMs:      resetMs(),
	}
}

// checkRate is the inbound-frame admission gate: the per-conn bucket
// first, then the optional cross-connection per-IP aggregate limiter
// (Config.IPLimiter — the Task 6.3.7 IP-level hook a deployment may back
// with Redis so NAT-egress floods are bounded cluster-wide).
func (c *Conn) checkRate() rateVerdict {
	v := c.allowControl()
	if !v.ok {
		return v
	}
	if lim := c.srv.cfg.IPLimiter; lim != nil {
		if ms, ok := lim.Allow(c.remoteIP, c.srv.cfg.Now()); !ok {
			return rateVerdict{retryAfterMs: ms, resetMs: ms}
		}
	}
	return v
}

// noteRateWarning records one WS_RATE_EXCEEDED emission inside
// RateWarnWindow; the caller force-closes on RateWarnMax (Task 6.3.7
// item 2: "3 warnings in 60s → forced disconnect").
func (c *Conn) noteRateWarning() (strikes int) {
	c.rateMu.Lock()
	defer c.rateMu.Unlock()
	now := c.srv.cfg.Now()
	cutoff := now.Add(-c.srv.cfg.RateWarnWindow)
	kept := c.warns[:0]
	for _, t := range c.warns {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	c.warns = append(kept, now)
	return len(c.warns)
}

// sendRateInfo emits the Task 6.3.7 item 5 throttle-feedback frame.
func (c *Conn) sendRateInfo(remaining, resetMs int64) {
	b, err := marshalFrame(rateInfoFrame{
		Type: "rate_info", Remaining: remaining, ResetMs: resetMs,
		TsMs: c.srv.cfg.Now().UnixMilli(),
	})
	if err == nil {
		c.enqueue(b)
	}
}

// noteChurn records a subscription change. Sustained churn above
// MaxChurnPerWindow within ChurnWindow terminates the session with 4003
// + WS_ABUSE_DETECTED (spec §10.6 abuse patterns).
func (c *Conn) noteChurn() (abusive bool) {
	c.churnMu.Lock()
	defer c.churnMu.Unlock()
	now := c.srv.cfg.Now()
	cutoff := now.Add(-c.srv.cfg.ChurnWindow)
	kept := c.churn[:0]
	for _, t := range c.churn {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	c.churn = append(kept, now)
	return len(c.churn) > c.srv.cfg.MaxChurnPerWindow
}

// expiryLoop watches sess.ExpiresAt for JWT sessions. On expiry it emits
// AUTH_EXPIRED, tears down private subscriptions, then either demotes the
// connection to anonymous (public subs remain) or closes with 4019 when
// no public subscriptions survive (spec §10.5 item 5, §10.6).
func (c *Conn) expiryLoop() {
	var timer *time.Timer
	var fire <-chan time.Time
	rearm := func() {
		s := c.session()
		if timer != nil {
			timer.Stop()
			fire = nil
		}
		if !s.Authenticated || s.ExpiresAt.IsZero() {
			return
		}
		d := time.Until(s.ExpiresAt)
		if d < 0 {
			d = 0
		}
		if timer == nil {
			timer = time.NewTimer(d)
		} else {
			timer.Reset(d)
		}
		fire = timer.C
	}
	for {
		select {
		case <-c.done:
			if timer != nil {
				timer.Stop()
			}
			return
		case <-c.expiryCh:
			rearm()
		case <-fire:
			fire = nil
			c.onAuthExpiry()
		}
	}
}

// onAuthExpiry performs the §10.5 expiry teardown.
func (c *Conn) onAuthExpiry() {
	s := c.session()
	if !s.Authenticated || s.ExpiresAt.IsZero() || c.srv.cfg.Now().Before(s.ExpiresAt) {
		return // renewed in the meantime — watcher was stale
	}
	b, _ := marshalFrame(errorFrame{
		Type: "error", Error: "AUTH_EXPIRED",
		Message: "authentication token expired without renewal",
		TsMs:    c.srv.cfg.Now().UnixMilli(), Code: CloseAuthExpired,
	})
	c.enqueue(b)

	// Tear down private channels; demote when public subs survive,
	// otherwise terminate with 4019.
	c.subMu.Lock()
	remaining := 0
	for ch, sub := range c.subs {
		if sub.private {
			c.srv.unsubscribeLocked(c, ch)
			delete(c.subs, ch)
			continue
		}
		remaining++
	}
	c.subMu.Unlock()

	if remaining == 0 {
		c.closeWith(CloseAuthExpired, DisconnectServer, "AUTH_EXPIRED")
		return
	}
	c.srv.demote(c)
}

// readLoop is the inbound pump: one frame → one control action. Order
// actions run on dedicated goroutines (bounded by sem) so a slow engine
// dispatch never stalls market-data writes or control frames.
func (c *Conn) readLoop() {
	c.ws.SetReadLimit(c.srv.cfg.MaxFrameBytes)
	_ = c.ws.SetReadDeadline(c.srv.cfg.Now().Add(c.srv.cfg.PongWait))
	c.ws.SetPongHandler(func(string) error {
		return c.ws.SetReadDeadline(c.srv.cfg.Now().Add(c.srv.cfg.PongWait))
	})
	for {
		mt, msg, err := c.ws.ReadMessage()
		if err != nil {
			c.noteReadDeath(err)
			return
		}
		if mt != websocket.TextMessage && mt != websocket.BinaryMessage {
			continue
		}
		v := c.checkRate()
		if !v.ok {
			// Task 6.3.7 items 2+5: canonical WS_RATE_EXCEEDED warning
			// with retry_after_ms, followed by the rate_info feedback
			// frame; RateWarnMax strikes inside RateWarnWindow → forced
			// disconnect as abuse (4029 / ABUSE_DISCONNECT).
			c.sendError("", "", "WS_RATE_EXCEEDED",
				"control message rate exceeded", v.retryAfterMs)
			c.sendRateInfo(v.remaining, v.resetMs)
			if c.noteRateWarning() >= c.srv.cfg.RateWarnMax {
				c.sendError("", "", "WS_ABUSE_DETECTED",
					"rate-limit warning budget exhausted; session terminated", 0)
				c.closeWith(CloseRateExceeded, DisconnectAbuse,
					string(DisconnectAbuse))
				return
			}
			continue
		}
		if hErr := c.handleMessage(msg); hErr != nil {
			return
		}
	}
}

// noteReadDeath classifies the terminal read error into the §24 #245
// disconnect_reason discriminator: a client-sent close frame is
// CLIENT_DISCONNECT; anything else (heartbeat/pong timeout, RST,
// abnormal 1006) is NETWORK_TIMEOUT. Server-initiated closes win the
// race because closeWith is first-wins — when the pump tears the socket
// down for drain/abuse/expiry the read error here cannot reclassify it.
func (c *Conn) noteReadDeath(err error) {
	var ce *websocket.CloseError
	if errors.As(err, &ce) && ce.Code != websocket.CloseAbnormalClosure {
		code := ce.Code
		if code < 1000 || code > 2999 {
			code = websocket.CloseNormalClosure
		}
		c.closeWith(code, DisconnectClient, string(DisconnectClient))
		return
	}
	c.closeWith(websocket.CloseGoingAway, DisconnectNetwork,
		string(DisconnectNetwork))
}

// backpressureLoop is the Task 6.3.21 send-queue monitor: when the
// outbound queue stays over the depth or byte threshold for
// EvictConsecutive EvictCheckInterval ticks (default ≈2.0s of
// saturation), the conn is evicted with 4008 / WS_SLOW_CONSUMER_DROP.
// This covers the "queue stays full but no producer is mid-enqueue"
// case the enqueue-block timeout alone cannot see.
func (c *Conn) backpressureLoop() {
	depth := c.srv.cfg.EvictQueueDepth
	if depth <= 0 || depth > cap(c.out) {
		depth = cap(c.out)
	}
	maxBytes := c.srv.cfg.OutboundMaxBytes
	tick := time.NewTicker(c.srv.cfg.EvictCheckInterval)
	defer tick.Stop()
	strikes := 0
	for {
		select {
		case <-c.done:
			return
		case <-tick.C:
		}
		over := len(c.out) >= depth ||
			(maxBytes > 0 && c.outBytes.Load() > maxBytes)
		if over {
			strikes++
		} else {
			strikes = 0
		}
		if strikes >= c.srv.cfg.EvictConsecutive {
			c.closeWith(CloseSlowConsumer, DisconnectNetwork,
				"WS_SLOW_CONSUMER_DROP")
			return
		}
	}
}

// errCloseRead signals handleMessage wants the read loop to stop.
var errCloseRead = errors.New("ws: close read loop")
