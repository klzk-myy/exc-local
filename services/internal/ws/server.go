package ws

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"

	"exchange/internal/auth"
	"exchange/internal/middleware"
	"exchange/internal/ratelimit"

	excerrors "exchange/pkg/errors"
)

// PrivateChannels is the §10.5 authenticated-only channel set.
var PrivateChannels = map[string]bool{
	"private:orders":     true,
	"private:executions": true,
	"private:positions":  true,
	"private:balances":   true,
}

// wsConnCaps is the §8.3 WS-connections column keyed by tier label:
// authenticated connections per account. Anonymous connections are bound
// by Config.MaxConnsPerIP instead (spec §8.3 gives Public 0 *authenticated*
// WS sessions; §10.5 explicitly permits anonymous public-feed sockets —
// the per-IP cap is the abuse bound).
var wsConnCaps = map[ratelimit.Tier]int{
	ratelimit.TierBasic:         2,
	ratelimit.TierStandard:      5,
	ratelimit.TierProfessional:  20,
	ratelimit.TierInstitutional: 50,
	ratelimit.TierAdmin:         50,
}

// WSConnCap resolves the per-account connection cap for a tier; unknown
// tiers fail closed to the Basic cap.
func WSConnCap(t ratelimit.Tier) int {
	if n, ok := wsConnCaps[t]; ok {
		return n
	}
	return wsConnCaps[ratelimit.TierBasic]
}

// ConnCapsTable exposes the §8.3 WS cap table for the
// /api/v1/meta/rate-limits publisher (Task 5.3.42 item 4).
func ConnCapsTable() map[string]int {
	out := make(map[string]int, len(wsConnCaps))
	for k, v := range wsConnCaps {
		out[string(k)] = v
	}
	return out
}

// Config tunes Server. Zero values pick spec-pinned defaults.
//
// Subscription & abuse limits contract (Phase-06 Tasks 6.3.7 / 6.3.16 /
// 6.3.19 / 6.3.21; spec §10.5–§10.6, §24 #215/#245/#265/#289/#305):
//
//   - MaxSubscriptions caps total channel bindings per connection at
//     200 → WS_MAX_SUBSCRIPTIONS_EXCEEDED. Per-channel-type splits —
//     the §24 #84 20 L2 / 5 L3 rule — and §10.7 entitlements plug in
//     through SubAdmit; marketdata owns those policies, this package
//     only supplies the generic plumbing.
//   - Inbound control frames consume ControlRate tokens/s per conn
//     (default 100). A breach emits WS_RATE_EXCEEDED with
//     retry_after_ms plus a {"type":"rate_info",...} feedback frame;
//     RateWarnMax warnings inside RateWarnWindow (3 per 60s) force-close
//     the session with 4029 / ABUSE_DISCONNECT. IPLimiter optionally
//     adds a cross-connection per-IP aggregate bound (Redis-backed at
//     wiring, e.g. ws:rate:ip:{ip}).
//   - Subscription churn beyond MaxChurnPerWindow per ChurnWindow
//     terminates with 4003 / ABUSE_DISCONNECT.
//   - The send queue is bounded by OutboundBuffer messages and
//     OutboundMaxBytes bytes; over-threshold for EvictConsecutive
//     EvictCheckInterval ticks — or a producer blocked past
//     SlowConsumerTimeout — evicts with 4008 / WS_SLOW_CONSUMER_DROP.
//   - ReconnectRate per ReconnectWindow bounds new upgrades per remote
//     IP on the accept path (HTTP 429 RATE_LIMIT_TIER_EXCEEDED; the
//     §21.3 client backoff schedule makes ≤6 attempts/min, under the
//     default 10/min).
//   - Drain() issues a server.shutdown advisory, refuses new work
//     (cancels and in-flight responses stay available), and closes
//     survivors with 1001 CONNECTION_DRAINING at DrainDeadline.
//   - OnDisconnect receives a DisconnectInfo with the §24 #245
//     disconnect_reason — the dead-man/CoD consumer's exemption input.
//   - Failover advertises client-facing alternate endpoints and tracks
//     the feed health the marketdata service reports through
//     SetFeedState; transitions broadcast a feed.failover advisory.
type Config struct {
	// Issuer verifies JWT tokens on authenticate/refresh_token
	// (Task 5.3.1). Nil fails closed: JWT auth rejects UNAUTHORIZED.
	Issuer *auth.Issuer
	// Verifier verifies "ak_"-prefixed API-key auth frames
	// (Task 5.3.38). Nil fails closed.
	Verifier *auth.SignatureVerifier
	// Dispatcher executes order.* actions (the order-pipeline seam —
	// Tasks 5.3.24/5.3.25/Phase-02). Nil fails closed NOT_IMPLEMENTED.
	Dispatcher Dispatcher
	// Countdown executes order.countdown_cancel_all (Task 5.3.33 seam).
	// Nil fails closed NOT_IMPLEMENTED.
	Countdown CountdownController
	// Dedup backs the 60s request_id window (Task 5.3.42). Nil selects
	// MemDedupStore; production wiring passes RedisDedupStore.
	Dedup DedupStore
	// TierResolver maps an authenticated session to its §8.3 tier for the
	// per-account connection cap. Nil ⇒ API-key sessions use the key's
	// stored tier, JWT sessions fail closed to Basic.
	TierResolver func(ctx context.Context, sess *Session) ratelimit.Tier
	// IPLimiter is the optional cross-connection per-IP inbound-rate
	// hook (Task 6.3.7). Nil ⇒ per-conn bucket only.
	IPLimiter IPLimiter
	// SubAdmit vets one channel add beyond the generic MaxSubscriptions
	// cap (Task 6.3.16 seam — marketdata wires the 20 L2 / 5 L3 split and
	// §10.7 entitlement checks). Nil ⇒ generic cap only.
	SubAdmit SubAdmitHook
	// Failover is the Task 6.3.21 feed-failover seam consumed by the
	// marketdata service (endpoint advertisement + health transitions).
	Failover FailoverConfig
	// OnDisconnect receives the terminal DisconnectInfo for every
	// unregistered conn — the cancel-on-disconnect/dead-man consumer's
	// signal (Task 6.3.7 item 6: CoD must NOT fire for ABUSE_DISCONNECT).
	// Called asynchronously; must not block.
	OnDisconnect func(DisconnectInfo)

	Logger     *slog.Logger
	TrustProxy bool // honor X-Forwarded-For (set only behind HAProxy)
	Now        func() time.Time

	MaxConnsPerIP       int           // default 256 — anonymous abuse bound
	MaxSubscriptions    int           // default 200 (§10.6 / WS_MAX_SUBSCRIPTIONS_EXCEEDED)
	OutboundBuffer      int           // default 1024 (§10.6 slow-consumer)
	OutboundMaxBytes    int64         // default 4MiB queued-byte eviction threshold
	EvictQueueDepth     int           // slow-consumer depth threshold; 0 ⇒ OutboundBuffer (saturated)
	EvictCheckInterval  time.Duration // default 250ms queue monitor tick
	EvictConsecutive    int           // default 8 consecutive over-threshold ticks → 4008 (≈2.0s)
	SlowConsumerTimeout time.Duration // default 2s → close 4008
	ControlRate         int           // default 100 inbound frames/s
	RateWarnMax         int           // default 3 — forced disconnect after N rate warnings
	RateWarnWindow      time.Duration // default 60s rate-warning window
	MaxChurnPerWindow   int           // default 50 sub changes per ChurnWindow → 4003
	ChurnWindow         time.Duration // default 5s (10/s sustained)
	ReconnectRate       int           // default 10 upgrade attempts per ReconnectWindow per IP → 429
	ReconnectWindow     time.Duration // default 60s reconnect-flood window
	DrainDeadline       time.Duration // default 15s planned-shutdown drain bound
	DispatchTimeout     time.Duration // default 500ms → CORE_TIMEOUT
	DispatchConcurrency int           // per-conn concurrent order actions; default 16
	DedupWindow         time.Duration // default 60s request_id dedup (Task 5.3.42)
	PingInterval        time.Duration // default 30s
	PongWait            time.Duration // default 60s (Task 6.3.7 item 4 heartbeat)
	MaxFrameBytes       int64         // default 64KiB (Task 5.3.29 item 7)
}

func (c *Config) defaults() {
	if c.MaxConnsPerIP <= 0 {
		c.MaxConnsPerIP = 256
	}
	if c.MaxSubscriptions <= 0 {
		c.MaxSubscriptions = 200
	}
	if c.OutboundBuffer <= 0 {
		c.OutboundBuffer = 1024
	}
	if c.OutboundMaxBytes <= 0 {
		c.OutboundMaxBytes = 4 << 20
	}
	if c.EvictCheckInterval <= 0 {
		c.EvictCheckInterval = 250 * time.Millisecond
	}
	if c.EvictConsecutive <= 0 {
		c.EvictConsecutive = 8 // 250ms × 8 = 2.0s — the §10.6 saturation bound
	}
	if c.SlowConsumerTimeout <= 0 {
		c.SlowConsumerTimeout = 2 * time.Second
	}
	if c.ControlRate <= 0 {
		c.ControlRate = 100
	}
	if c.RateWarnMax <= 0 {
		c.RateWarnMax = 3
	}
	if c.RateWarnWindow <= 0 {
		c.RateWarnWindow = 60 * time.Second
	}
	if c.MaxChurnPerWindow <= 0 {
		c.MaxChurnPerWindow = 50
	}
	if c.ChurnWindow <= 0 {
		c.ChurnWindow = 5 * time.Second
	}
	if c.ReconnectRate <= 0 {
		c.ReconnectRate = 10
	}
	if c.ReconnectWindow <= 0 {
		c.ReconnectWindow = 60 * time.Second
	}
	if c.DrainDeadline <= 0 {
		c.DrainDeadline = 15 * time.Second
	}
	if c.DispatchTimeout <= 0 {
		c.DispatchTimeout = 500 * time.Millisecond
	}
	if c.DispatchConcurrency <= 0 {
		c.DispatchConcurrency = 16
	}
	if c.DedupWindow <= 0 {
		c.DedupWindow = 60 * time.Second
	}
	if c.PingInterval <= 0 {
		c.PingInterval = 30 * time.Second
	}
	if c.PongWait <= 0 {
		c.PongWait = 60 * time.Second
	}
	if c.MaxFrameBytes <= 0 {
		c.MaxFrameBytes = 64 << 10
	}
	if c.Dedup == nil {
		c.Dedup = NewMemDedupStore()
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
}

// Server is the /ws/v1 endpoint: connection registry + control protocol
// + trading dispatch + publish fanout.
type Server struct {
	cfg Config
	up  websocket.Upgrader

	mu          sync.Mutex
	conns       map[*Conn]struct{}
	byIP        map[string]int
	byAccount   map[int64]int
	subscribers map[string]map[*Conn]struct{} // channel → conns

	draining  atomic.Bool
	drainDone chan struct{} // created by Drain under mu; closed on empty registry
	drainOnce sync.Once

	feedState atomic.Int32 // FeedState — Task 6.3.21 seam

	acceptMu    sync.Mutex
	accept      map[string]*acceptBucket // reconnect-flood token buckets by IP
	acceptSweep time.Time                // last lazy GC pass
}

// NewServer builds the WS endpoint.
func NewServer(cfg Config) *Server {
	cfg.defaults()
	return &Server{
		cfg: cfg,
		up: websocket.Upgrader{
			ReadBufferSize:  16 << 10,
			WriteBufferSize: 16 << 10,
			// Auth happens in-band after upgrade (no cookie bearer), so
			// origin policy is informational — deployments can tighten it
			// via CheckOrigin.
			CheckOrigin: func(*http.Request) bool { return true },
		},
		conns:       map[*Conn]struct{}{},
		byIP:        map[string]int{},
		byAccount:   map[int64]int{},
		subscribers: map[string]map[*Conn]struct{}{},
		accept:      map[string]*acceptBucket{},
	}
}

// writePreUpgradeError answers a rejected upgrade with the JSON error
// envelope used by every pre-WS rejection (CAPACITY_EXCEEDED,
// RATE_LIMIT_TIER_EXCEEDED, MAINTENANCE_MODE). HTTP durations follow the
// seconds convention (spec §10.5 item 5), so retryAfter is in seconds
// and also rides the RFC 6585 Retry-After header.
func writePreUpgradeError(w http.ResponseWriter, status int, code, msg string, retryAfterSec int) {
	w.Header().Set("Content-Type", "application/problem+json")
	if retryAfterSec > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(retryAfterSec))
	}
	w.WriteHeader(status)
	body := map[string]any{
		"type": "error", "error": code, "message": msg, "status": status,
	}
	if retryAfterSec > 0 {
		body["retry_after"] = retryAfterSec
	}
	_ = json.NewEncoder(w).Encode(body)
}

// ServeHTTP upgrades the connection. The anonymous per-IP cap is enforced
// pre-upgrade (CAPACITY_EXCEEDED 503) and re-checked under the registry
// lock post-upgrade — a close frame races cannot overflow the cap.
// Task 6.3.19/6.3.21 gates precede the cap checks: a draining endpoint
// refuses upgrades (503 MAINTENANCE_MODE) and the per-IP reconnect-flood
// bucket rejects with 429 RATE_LIMIT_TIER_EXCEEDED.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ip := middleware.ClientIP(r, s.cfg.TrustProxy)

	if s.draining.Load() {
		writePreUpgradeError(w, http.StatusServiceUnavailable,
			"MAINTENANCE_MODE", "endpoint draining for planned shutdown", 0)
		return
	}
	if retry, ok := s.allowReconnect(ip); !ok {
		writePreUpgradeError(w, http.StatusTooManyRequests,
			"RATE_LIMIT_TIER_EXCEEDED", "reconnect rate exceeded", retry)
		return
	}

	s.mu.Lock()
	if s.byIP[ip] >= s.cfg.MaxConnsPerIP {
		s.mu.Unlock()
		// Pre-upgrade so a plain JSON error is legal.
		writePreUpgradeError(w, http.StatusServiceUnavailable,
			"CAPACITY_EXCEEDED", "per-IP WebSocket connection cap reached", 0)
		return
	}
	s.mu.Unlock()

	ws, err := s.up.Upgrade(w, r, nil)
	if err != nil {
		return // upgrader already answered
	}
	c := &Conn{
		srv: s, ws: ws, remoteIP: ip, path: r.URL.Path,
		out:      make(chan []byte, s.cfg.OutboundBuffer),
		done:     make(chan struct{}),
		pumpDone: make(chan struct{}),
		subs:     map[string]*subscription{},
		expiryCh: make(chan struct{}, 1),
		sem:      make(chan struct{}, s.cfg.DispatchConcurrency),
	}

	s.mu.Lock()
	if s.draining.Load() {
		s.mu.Unlock()
		// Drain raced the upgrade — close 1001 directly (writePump is
		// not running yet).
		_ = ws.WriteMessage(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseGoingAway,
				"CONNECTION_DRAINING"))
		_ = ws.Close()
		return
	}
	if s.byIP[ip] >= s.cfg.MaxConnsPerIP {
		s.mu.Unlock()
		// writePump is not running yet — emit the close frame directly.
		_ = ws.WriteMessage(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseTryAgainLater,
				"per-IP connection cap"))
		_ = ws.Close()
		return
	}
	s.conns[c] = struct{}{}
	s.byIP[ip]++
	s.mu.Unlock()

	go c.writePump()
	go c.expiryLoop()
	go c.backpressureLoop()
	c.readLoop() // blocks until socket dies
	s.unregister(c)
}

// unregister removes the conn from every index. It waits (bounded) for
// the write pump to flush the close frame so terminal codes reach the
// client before the socket is freed.
func (s *Server) unregister(c *Conn) {
	c.closeWith(websocket.CloseNormalClosure, DisconnectServer, "bye")
	select {
	case <-c.pumpDone:
	case <-time.After(15 * time.Second): // write deadlines cap the wait
	}
	sess := c.session()
	info := DisconnectInfo{
		RemoteIP:  c.remoteIP,
		Reason:    c.disconnectReason(),
		CloseCode: int(c.closeCode.Load()),
	}
	if sess.Authenticated {
		info.AccountID = sess.AccountID
	}
	// Snapshot + clear the sub set under subMu first — fanout reads
	// c.subs under subMu, so touching it under s.mu alone races a
	// concurrent Publish. Lock order stays subMu-free inside s.mu
	// (handleSubscribe holds subMu → s.mu), so the set is cleared here
	// and the channel list is replayed under s.mu below.
	c.subMu.Lock()
	channels := make([]string, 0, len(c.subs))
	for ch := range c.subs {
		channels = append(channels, ch)
	}
	c.subs = map[string]*subscription{}
	c.subMu.Unlock()

	s.mu.Lock()
	delete(s.conns, c)
	if s.byIP[c.remoteIP]--; s.byIP[c.remoteIP] <= 0 {
		delete(s.byIP, c.remoteIP)
	}
	if sess.Authenticated && sess.AccountID != 0 {
		if s.byAccount[sess.AccountID]--; s.byAccount[sess.AccountID] <= 0 {
			delete(s.byAccount, sess.AccountID)
		}
	}
	for _, ch := range channels {
		delete(s.subscribers[ch], c)
		if len(s.subscribers[ch]) == 0 {
			delete(s.subscribers, ch)
		}
	}
	// Wake a pending Drain once the registry empties.
	if s.draining.Load() && len(s.conns) == 0 && s.drainDone != nil {
		s.drainOnce.Do(func() { close(s.drainDone) })
	}
	s.mu.Unlock()
	// The dead-man/CoD consumer runs async — it must never stall conn
	// teardown (Task 6.3.7 item 6; the 1-per-5s mass-cancel throttle is
	// the consumer's problem, not this package's).
	if fn := s.cfg.OnDisconnect; fn != nil {
		go fn(info)
	}
}

// demote drops the auth binding (expiry teardown with surviving public
// subscriptions): the account index is released and the session resets
// to anonymous. Private subs were already removed by the caller.
func (s *Server) demote(c *Conn) {
	s.mu.Lock()
	sess := c.session()
	if sess.Authenticated && sess.AccountID != 0 {
		if s.byAccount[sess.AccountID]--; s.byAccount[sess.AccountID] <= 0 {
			delete(s.byAccount, sess.AccountID)
		}
	}
	s.mu.Unlock()
	c.setSession(Session{ProtocolVer: sess.ProtocolVer})
}

// unsubscribeLocked removes a channel → conn index entry (caller holds
// c.subMu / s.mu as documented per call site).
func (s *Server) unsubscribeLocked(c *Conn, ch string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.subscribers[ch], c)
	if len(s.subscribers[ch]) == 0 {
		delete(s.subscribers, ch)
	}
}

// ---------------------------------------------------------------------------
// Frame routing
// ---------------------------------------------------------------------------

// handleMessage decodes and routes one inbound frame. Returning
// errCloseRead ends the read loop (used after terminal closes).
func (c *Conn) handleMessage(msg []byte) error {
	f, err := parseFrame(msg)
	if err != nil {
		c.sendError("", "", "INVALID_REQUEST", "frame is not valid JSON", 0)
		return nil
	}
	// Task 6.3.19 drain gate: during a planned-shutdown drain new
	// subscriptions and order requests are refused (MAINTENANCE_MODE)
	// while cancels, session upkeep and in-flight responses proceed.
	if c.srv.draining.Load() && !drainAllowed[f.Action] {
		c.sendError(f.RequestID, f.Action, "MAINTENANCE_MODE",
			"endpoint draining for planned shutdown", 0)
		return nil
	}
	switch f.Action {
	case "authenticate":
		c.handleAuthenticate(f)
	case "refresh_token":
		c.handleRefresh(f)
	case "subscribe", "unsubscribe":
		c.handleSubscribe(f, f.Action == "unsubscribe")
	case "ping":
		b, _ := marshalFrame(pongFrame{Type: "pong", TsMs: c.srv.cfg.Now().UnixMilli()})
		c.enqueue(b)
	case "resume":
		c.handleResume(f)
	default:
		if strings.HasPrefix(f.Action, "order.") {
			return c.handleOrder(f)
		}
		if f.Action == "" {
			c.sendError(f.RequestID, "", "INVALID_REQUEST", "missing action", 0)
			return nil
		}
		c.sendError(f.RequestID, f.Action, "INVALID_REQUEST", "unknown action", 0)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Task 5.3.26 — authenticate / refresh_token
// ---------------------------------------------------------------------------

// handleAuthenticate elevates the connection per §10.5 item 1. The frame:
//
//	{"action":"authenticate","token":"<jwt|ak_*>","signature":"<opt>",
//	 "timestamp":<epoch_s>,"protocol_version":1}
//
// API-key tokens carry the ak_ prefix and REQUIRE signature — the signed
// surface is CanonicalRequest(timestamp, "WS", upgrade path, token),
// identical canonicalization to REST signing (Task 5.3.38 item 3).
func (c *Conn) handleAuthenticate(f clientFrame) {
	if f.ProtocolVersion == nil {
		c.sendError(f.RequestID, "authenticate", "INVALID_REQUEST",
			"protocol_version is required", 0)
		return
	}
	if err := middleware.CheckWSProtocolVersion(*f.ProtocolVersion); err != nil {
		c.sendError(f.RequestID, "authenticate", "UNSUPPORTED_PROTOCOL_VERSION",
			"protocol_version not supported", 0)
		return
	}
	if f.Token == "" {
		c.sendError(f.RequestID, "authenticate", "INVALID_REQUEST",
			"token is required", 0)
		return
	}

	var sess Session
	sess.ProtocolVer = *f.ProtocolVersion
	if strings.HasPrefix(f.Token, "ak_") {
		if c.srv.cfg.Verifier == nil {
			c.sendError(f.RequestID, "authenticate", "UNAUTHORIZED",
				"api-key authentication not configured", 0)
			return
		}
		if f.Signature == "" || len(f.Timestamp) == 0 {
			c.sendError(f.RequestID, "authenticate", "INVALID_REQUEST",
				"api-key auth requires signature and timestamp", 0)
			return
		}
		key, err := c.srv.cfg.Verifier.Verify(context.Background(), auth.SignedRequest{
			KeyID:     f.Token,
			Timestamp: f.tsString(),
			Signature: f.Signature,
			Method:    "WS",
			Path:      c.path,
			Body:      []byte(f.Token), // signed preimage = the key id itself
			RemoteIP:  c.remoteIP,
		})
		if err != nil {
			c.sendError(f.RequestID, "authenticate", codeOf(err),
				"api-key authentication failed", 0)
			return
		}
		sess.Authenticated = true
		sess.ViaAPIKey = true
		sess.Subject = "apikey:" + key.KeyID
		sess.AccountID = key.AccountID
		sess.Scopes = key.Scopes
		sess.Tier = key.RateLimitTier
		sess.KeyID = key.KeyID
		// API keys don't expire on the wire; revocation is the only
		// invalidation (spec §8.8 item 3).
	} else {
		if c.srv.cfg.Issuer == nil {
			c.sendError(f.RequestID, "authenticate", "UNAUTHORIZED",
				"jwt authentication not configured", 0)
			return
		}
		claims, err := c.srv.cfg.Issuer.Parse(f.Token)
		if err != nil {
			c.sendError(f.RequestID, "authenticate", "UNAUTHORIZED",
				"token validation failed", 0)
			return
		}
		sess.Authenticated = true
		sess.Subject = claims.Subject
		sess.AccountID = claims.AccountID
		sess.Scopes = claims.Scopes
		sess.KeyID = claims.KeyID
		sess.SessionID = claims.SessionID
		sess.ExpiresAt = claims.ExpiresAt
	}

	// Per-account concurrent-WS cap (spec §8.3 WS-connections column).
	if !c.srv.accountCapAdmit(c, &sess) {
		c.sendError(f.RequestID, "authenticate", "CAPACITY_EXCEEDED",
			"per-account WebSocket connection cap reached for tier", 0)
		return
	}

	sess.RemoteIP = c.remoteIP
	prev := c.session()
	if prev.Authenticated && prev.AccountID != sess.AccountID {
		c.teardownPrivate() // re-auth under a different account must not leak
	}
	c.setSession(sess)

	data := map[string]any{
		"subject":          sess.Subject,
		"account_id":       sess.AccountID,
		"scopes":           sess.Scopes,
		"protocol_version": sess.ProtocolVer,
	}
	if !sess.ExpiresAt.IsZero() {
		data["expires_at_ms"] = sess.ExpiresAt.UnixMilli()
	}
	c.sendResponse(f.RequestID, "authenticate", "ACK", data)
}

// accountCapAdmit checks and reserves the per-account slot. Caller must
// only call this for an authenticated session.
func (s *Server) accountCapAdmit(c *Conn, sess *Session) bool {
	tier := ratelimit.ParseTier(sess.Tier)
	if s.cfg.TierResolver != nil {
		tier = s.cfg.TierResolver(context.Background(), sess)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// Release a prior account binding on this conn (re-auth flow).
	if prev := c.session(); prev.Authenticated && prev.AccountID != 0 &&
		prev.AccountID != sess.AccountID {
		if s.byAccount[prev.AccountID]--; s.byAccount[prev.AccountID] <= 0 {
			delete(s.byAccount, prev.AccountID)
		}
	}
	if prev := c.session(); prev.Authenticated && prev.AccountID == sess.AccountID {
		return true // already counted — re-auth/refresh under same account
	}
	if s.byAccount[sess.AccountID] >= WSConnCap(tier) {
		return false
	}
	s.byAccount[sess.AccountID]++
	return true
}

// handleRefresh implements the §10.5 item 3 in-flight renewal:
// {"action":"refresh_token","token":"<new_jwt>"} — the socket,
// subscriptions and in-flight messages survive untouched.
func (c *Conn) handleRefresh(f clientFrame) {
	sess := c.session()
	if !sess.Authenticated {
		c.sendError(f.RequestID, "refresh_token", "UNAUTHORIZED",
			"authenticate first", 0)
		return
	}
	if sess.ViaAPIKey {
		c.sendError(f.RequestID, "refresh_token", "INVALID_REQUEST",
			"api-key sessions do not expire", 0)
		return
	}
	if f.Token == "" {
		c.sendError(f.RequestID, "refresh_token", "INVALID_REQUEST",
			"token is required", 0)
		return
	}
	if c.srv.cfg.Issuer == nil {
		c.sendError(f.RequestID, "refresh_token", "UNAUTHORIZED",
			"jwt authentication not configured", 0)
		return
	}
	claims, err := c.srv.cfg.Issuer.Parse(f.Token)
	if err != nil {
		c.sendError(f.RequestID, "refresh_token", "UNAUTHORIZED",
			"token validation failed", 0)
		return
	}
	// Renewal binds to the existing session: a different identity is a
	// re-authenticate, not a renewal (fail closed on token swap).
	if claims.Subject != sess.Subject || claims.AccountID != sess.AccountID {
		c.sendError(f.RequestID, "refresh_token", "UNAUTHORIZED",
			"renewal token identity mismatch", 0)
		return
	}
	sess.ExpiresAt = claims.ExpiresAt
	sess.KeyID = claims.KeyID
	c.setSession(sess)
	c.sendResponse(f.RequestID, "refresh_token", "ACK", map[string]any{
		"expires_at_ms": claims.ExpiresAt.UnixMilli(),
	})
}

// teardownPrivate removes every private:* subscription (auth-expiry and
// cross-account re-auth paths).
func (c *Conn) teardownPrivate() {
	c.subMu.Lock()
	defer c.subMu.Unlock()
	for ch, sub := range c.subs {
		if sub.private {
			c.srv.unsubscribeLocked(c, ch)
			delete(c.subs, ch)
		}
	}
}

// ---------------------------------------------------------------------------
// Subscriptions (channel data plane owned by Phase-06; this owns the
// control contract: private auth gate, cap, churn guard, acks)
// ---------------------------------------------------------------------------

var channelNameOK = func() func(string) bool {
	return func(ch string) bool {
		if len(ch) == 0 || len(ch) > 64 {
			return false
		}
		for _, r := range ch {
			switch {
			case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z',
				r >= '0' && r <= '9':
			case r == '_', r == '-', r == '.', r == ':', r == '@':
			default:
				return false
			}
		}
		return true
	}
}()

func (c *Conn) handleSubscribe(f clientFrame, unsub bool) {
	channels, err := f.channelList()
	if err != nil || len(channels) == 0 {
		c.sendError(f.RequestID, f.Action, "INVALID_REQUEST",
			"subscribe requires a non-empty channel list", 0)
		return
	}
	if c.noteChurn() {
		c.sendError(f.RequestID, f.Action, "WS_ABUSE_DETECTED",
			"subscription churn exceeded", 0)
		c.closeWith(CloseAbuse, DisconnectAbuse, string(DisconnectAbuse))
		return
	}

	sess := c.session()
	var accepted []string
	for _, ch := range channels {
		if !channelNameOK(ch) {
			c.sendError(f.RequestID, f.Action, "INVALID_REQUEST",
				"invalid channel name", 0)
			continue
		}
		private := PrivateChannels[ch] || strings.HasPrefix(ch, "private:")
		if private && !PrivateChannels[ch] {
			c.sendError(f.RequestID, f.Action, "INVALID_REQUEST",
				"unknown private channel "+ch, 0)
			continue
		}
		if private {
			if !sess.Authenticated {
				c.sendError(f.RequestID, f.Action, "UNAUTHORIZED",
					"authentication required for "+ch, 0)
				continue
			}
			if !sess.hasScope("read") {
				c.sendError(f.RequestID, f.Action, "INSUFFICIENT_SCOPE",
					"read scope required for "+ch, 0)
				continue
			}
		}

		c.subMu.Lock()
		if unsub {
			if _, ok := c.subs[ch]; ok {
				delete(c.subs, ch)
				c.srv.unsubscribeLocked(c, ch)
				accepted = append(accepted, ch)
			}
			c.subMu.Unlock()
			continue
		}
		if _, dup := c.subs[ch]; !dup {
			if len(c.subs) >= c.srv.cfg.MaxSubscriptions {
				c.subMu.Unlock()
				c.sendError(f.RequestID, f.Action, "WS_MAX_SUBSCRIPTIONS_EXCEEDED",
					"subscription limit reached", 0)
				continue
			}
			// Task 6.3.16 seam: per-channel-type policies (the §24 #84
			// 20 L2 / 5 L3 split, §10.7 entitlements) plug in via
			// SubAdmit — marketdata owns them; the hook must not call
			// back into Conn paths that take subMu.
			if hook := c.srv.cfg.SubAdmit; hook != nil {
				code, ok := hook(c, ch)
				if !ok {
					c.subMu.Unlock()
					if code == "" {
						code = "WS_MAX_SUBSCRIPTIONS_EXCEEDED"
					}
					c.sendError(f.RequestID, f.Action, code,
						"subscription rejected", 0)
					continue
				}
			}
			c.subs[ch] = &subscription{channel: ch, private: private}
			c.srv.subscribe(c, ch)
			accepted = append(accepted, ch)
		} else {
			accepted = append(accepted, ch) // idempotent subscribe
		}
		c.subMu.Unlock()
	}
	if len(accepted) > 0 {
		c.subMu.Lock()
		total := len(c.subs)
		c.subMu.Unlock()
		typ := "subscribed"
		if unsub {
			typ = "unsubscribed"
		}
		b, _ := marshalFrame(subscribedFrame{
			Type: typ, Channels: accepted, Total: total,
			TsMs: c.srv.cfg.Now().UnixMilli(),
		})
		c.enqueue(b)
	}
}

func (s *Server) subscribe(c *Conn, ch string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	set := s.subscribers[ch]
	if set == nil {
		set = map[*Conn]struct{}{}
		s.subscribers[ch] = set
	}
	set[c] = struct{}{}
}

// handleResume answers the Phase-06 resume contract honestly: no ring
// buffer exists yet, so the reply is an explicit resync directive —
// clients refetch a snapshot and resubscribe (fail closed, never a fake
// replay).
func (c *Conn) handleResume(f clientFrame) {
	b, _ := marshalFrame(resyncFrame{
		Type: "resync", Channel: f.Channel, LastSeq: f.LastSeq,
		TsMs: c.srv.cfg.Now().UnixMilli(),
	})
	c.enqueue(b)
}

// ---------------------------------------------------------------------------
// Publish fanout (consumed by the private-event producers and Phase-06
// market-data fanout when it lands)
// ---------------------------------------------------------------------------

// Publish sends an event frame to every subscriber of a public channel.
func (s *Server) Publish(channel string, data any) {
	s.fanout(channel, 0, data)
}

// PublishPrivate sends an event frame to subscribers of a private:*
// channel whose session is bound to accountID — account isolation is the
// whole point of the private: prefix.
func (s *Server) PublishPrivate(accountID int64, channel string, data any) {
	s.fanout(channel, accountID, data)
}

func (s *Server) fanout(channel string, accountID int64, data any) {
	s.mu.Lock()
	conns := make([]*Conn, 0, len(s.subscribers[channel]))
	for c := range s.subscribers[channel] {
		conns = append(conns, c)
	}
	s.mu.Unlock()
	for _, c := range conns {
		c.subMu.Lock()
		sub := c.subs[channel]
		if sub == nil || (sub.private && c.session().AccountID != accountID) {
			c.subMu.Unlock()
			continue
		}
		sub.seq++
		b, err := marshalFrame(eventFrame{
			Type: "event", Channel: channel, Seq: sub.seq,
			Data: data, TsMs: c.srv.cfg.Now().UnixMilli(),
		})
		c.subMu.Unlock()
		if err == nil {
			c.enqueue(b)
		}
	}
}

// Stats is a lightweight observability snapshot for /health consumers.
func (s *Server) Stats() (conns, authed, subs int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for c := range s.conns {
		conns++
		if c.session().Authenticated {
			authed++
		}
	}
	for _, set := range s.subscribers {
		subs += len(set)
	}
	return conns, authed, subs
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// codeOf extracts the §23 code from a domain error; unknown errors
// degrade to INTERNAL_ERROR (registry-gated).
func codeOf(err error) string {
	var e *excerrors.Error
	if errors.As(err, &e) && e.Code != "" {
		return e.Code
	}
	return "INTERNAL_ERROR"
}

// itoa64 renders an int64 — tiny helper for frame construction.
func itoa64(v int64) string { return strconv.FormatInt(v, 10) }
