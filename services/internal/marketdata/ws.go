// Task 6.3.1 — market-data WebSocket server (spec §10.1, §10.5, §10.6).
//
// Goroutine-per-connection over gorilla/websocket. The wire contract is
// the unified §10.5 grammar (action discriminator, response/error/event
// envelopes, registered §23 codes, RFC 6455 private close codes) —
// identical to the gateway's /ws/v1 surface so clients speak one
// protocol to both endpoints.
//
// The connection/session machinery below re-implements the internal/ws
// contract rather than embedding ws.Server: that package keeps its conn
// loop and subscribe/resume handlers package-private, and the Phase-06
// additions this file owns — per-class subscription budgets (20 L2 /
// 5 L3), channel-scoped sequence cursors for resume replay, and the
// `request` method router — have no seam to attach to. Every exported ws
// seam that does exist (ws.Session, ws.PrivateChannels, ws.Dispatcher,
// ws.DedupStore, ws.WSConnCap, ws.PayloadHash/DedupKey, close codes) is
// reused here.
package marketdata

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"

	"exchange/internal/auth"
	"exchange/internal/middleware"
	"exchange/internal/ratelimit"
	"exchange/internal/ws"

	excerrors "exchange/pkg/errors"
)

// Registered RFC 6455 private-use close codes, shared with internal/ws
// (spec §10.6, §23).
const (
	CloseAuthExpired  = ws.CloseAuthExpired  // 4019 AUTH_EXPIRED
	CloseAbuse        = ws.CloseAbuse        // 4003 WS_ABUSE_DETECTED
	CloseSlowConsumer = ws.CloseSlowConsumer // 4008 slow-consumer drop
)

var errBadParams = errors.New("marketdata: params must be a channel array or object")

// Config tunes Server. Zero values pick spec-pinned defaults.
type Config struct {
	// Issuer verifies JWT tokens on authenticate/refresh_token
	// (Task 5.3.1 seam). Nil fails closed: JWT auth rejects UNAUTHORIZED
	// and private:* channels are unreachable on this endpoint.
	Issuer *auth.Issuer
	// Verifier verifies "ak_"-prefixed API-key auth frames
	// (Task 5.3.38 seam). Nil fails closed.
	Verifier *auth.SignatureVerifier
	// Dispatcher executes order.* actions through the Order Gateway
	// pipeline (Task 6.3.10 seam → Phase-05 Task 5.3.31). Nil fails
	// closed NOT_IMPLEMENTED once a session is authenticated.
	Dispatcher ws.Dispatcher
	// Dedup backs the 60s request_id window for order.* actions
	// (Task 5.3.42). Nil selects an in-memory store; production wiring
	// passes ws.NewRedisDedupStore so the window is shared with the
	// gateway surface.
	Dedup ws.DedupStore
	// TierResolver maps an authenticated session to its §8.3 tier for
	// the per-account connection cap (ws.WSConnCap). Nil ⇒ API-key
	// sessions use the key's stored tier, JWT sessions fail closed to
	// Basic.
	TierResolver func(ctx context.Context, sess *ws.Session) ratelimit.Tier
	// Entitlements vets channel binds per spec §10.7 (Task 6.3.22):
	// symbol-level access control on WS streams mirroring the FIX
	// SESSION_NOT_ENTITLED model. Nil ⇒ open (deployment does not gate
	// market data — public FX channels are unrestricted by default).
	Entitlements EntitlementChecker
	// Journal records durable seq-gap entries (Task 6.3.22 item 1).
	// Nil disables journaling — the seq cursor contract still holds.
	Journal GapJournal

	Logger     *slog.Logger
	TrustProxy bool // honor X-Forwarded-For (set only behind HAProxy)
	Now        func() time.Time

	MaxConnsPerIP       int           // default 256 — anonymous abuse bound
	MaxSubscriptions    int           // default 200 total channels (§10.6)
	MaxL2Subscriptions  int           // default 20 L2-class (book@/depth@) per §24 #84
	MaxL3Subscriptions  int           // default 5 L3-class (l3@/l3Book@) per §24 #84
	OutboundBuffer      int           // default 1024 (§10.6 slow-consumer)
	SlowConsumerTimeout time.Duration // default 2s → close 4008
	ControlRate         int           // default 100 inbound frames/s
	MaxChurnPerWindow   int           // default 50 sub changes per ChurnWindow → 4003
	ChurnWindow         time.Duration // default 5s
	DispatchTimeout     time.Duration // default 500ms → CORE_TIMEOUT
	DispatchConcurrency int           // per-conn concurrent order actions; default 16
	DedupWindow         time.Duration // default 60s request_id dedup
	PingInterval        time.Duration // default 30s server ping
	PongWait            time.Duration // default 60s client pong deadline
	MaxFrameBytes       int64         // default 64KiB
	ReplayBufferMsgs    int           // default 10,000 (Task 6.3.9 ring cap)
	ReplayBufferAge     time.Duration // default 60s (Task 6.3.9 ring horizon)
}

func (c *Config) defaults() {
	if c.MaxConnsPerIP <= 0 {
		c.MaxConnsPerIP = 256
	}
	if c.MaxSubscriptions <= 0 {
		c.MaxSubscriptions = 200
	}
	if c.MaxL2Subscriptions <= 0 {
		c.MaxL2Subscriptions = 20
	}
	if c.MaxL3Subscriptions <= 0 {
		c.MaxL3Subscriptions = 5
	}
	if c.OutboundBuffer <= 0 {
		c.OutboundBuffer = 1024
	}
	if c.SlowConsumerTimeout <= 0 {
		c.SlowConsumerTimeout = 2 * time.Second
	}
	if c.ControlRate <= 0 {
		c.ControlRate = 100
	}
	if c.MaxChurnPerWindow <= 0 {
		c.MaxChurnPerWindow = 50
	}
	if c.ChurnWindow <= 0 {
		c.ChurnWindow = 5 * time.Second
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
	if c.ReplayBufferMsgs <= 0 {
		c.ReplayBufferMsgs = 10_000
	}
	if c.ReplayBufferAge <= 0 {
		c.ReplayBufferAge = 60 * time.Second
	}
	if c.Dedup == nil {
		c.Dedup = ws.NewMemDedupStore()
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
}

// subscription is one active channel binding on a connection.
type subscription struct {
	ch    Channel
	class ChannelClass
}

// channelState is the hub's per-channel bookkeeping: the subscriber set
// and the Task 6.3.9 replay buffers. Public channels keep ONE ring keyed
// by the channel token; private:* channels keep ONE RING PER ACCOUNT
// (spec §10.5 / Task 6.3.9 item 7: private sequences are keyed by
// (user_id, channel) — a shared ring would leak other accounts' frames
// and inflate gaps on replay).
type channelState struct {
	subs   map[*Conn]struct{}
	ring   *ringBuffer           // public channel replay buffer
	priv   map[int64]*ringBuffer // private channel per-account rings
	isPriv bool                  // private:* — Publish never fans out here
}

// privRing returns (creating on demand) the per-account replay ring.
// Caller holds s.mu.
func (cs *channelState) privRing(cfg *Config, accountID int64) *ringBuffer {
	if cs.priv == nil {
		cs.priv = map[int64]*ringBuffer{}
	}
	rb := cs.priv[accountID]
	if rb == nil {
		rb = newRingBuffer(cfg.ReplayBufferMsgs,
			cfg.ReplayBufferAge, cfg.Now)
		cs.priv[accountID] = rb
	}
	return rb
}

// empty reports whether the channel state can be GC'd — no subscribers
// and no replayable frames in any ring.
// Caller holds s.mu.
func (cs *channelState) empty() bool {
	if len(cs.subs) > 0 {
		return false
	}
	if cs.ring != nil && cs.ring.len() > 0 {
		return false
	}
	for _, rb := range cs.priv {
		if rb.len() > 0 {
			return false
		}
	}
	return true
}

// prunePriv drops fully-expired per-account rings (a disconnected user's
// ring survives for the 60s resume horizon, then self-cleans on the next
// touch — account churn can never grow the index unboundedly).
// Caller holds s.mu.
func (cs *channelState) prunePriv() {
	for acct, rb := range cs.priv {
		if rb.expired() {
			delete(cs.priv, acct)
		}
	}
}

// Server is the /ws/v1/marketdata endpoint: connection registry +
// §10.5 control protocol + typed-channel fanout + resume ring buffers.
type Server struct {
	cfg Config
	up  websocket.Upgrader

	mu          sync.Mutex
	conns       map[*Conn]struct{}
	byIP        map[string]int
	byAccount   map[int64]int
	channels    map[string]*channelState
	snapSources map[string]SnapshotSource // keyed by channel TYPE
	methods     map[string]MethodHandler  // request-method registry
	// depthIdx indexes live depth@{symbol}:{levels}:{cadence} variants
	// per symbol (Task 6.3.15) — the conflator's VariantSource reads it
	// to multiplex the internal 20-level snapshot into param channels.
	depthIdx map[string]map[DepthVariant]int

	metrics *Metrics
}

// NewServer builds the marketdata WS endpoint.
func NewServer(cfg Config) *Server {
	cfg.defaults()
	return &Server{
		cfg: cfg,
		up: websocket.Upgrader{
			ReadBufferSize:  16 << 10,
			WriteBufferSize: 16 << 10,
			// Auth happens in-band after upgrade; origin policy is
			// informational — deployments tighten it via CheckOrigin.
			CheckOrigin: func(*http.Request) bool { return true },
		},
		conns:       map[*Conn]struct{}{},
		byIP:        map[string]int{},
		byAccount:   map[int64]int{},
		channels:    map[string]*channelState{},
		snapSources: map[string]SnapshotSource{},
		depthIdx:    map[string]map[DepthVariant]int{},
		metrics:     NewMetrics(),
	}
}

// Metrics exposes the send-latency instrumentation (Task 6.3.2 SLA:
// p99 WS push ≤ 100ms) plus frame/conn counters for /health consumers.
func (s *Server) Metrics() *Metrics { return s.metrics }

// SetSnapshotSource registers the full-state provider for a channel
// type ("book", "depth", ...) consulted when a resume cursor falls
// outside the replay horizon (Task 6.3.9 item 5). Producer waves wire
// their snapshotter here; nil disables the snapshot fallback for that
// type, leaving the client a resync directive.
func (s *Server) SetSnapshotSource(channelType string, src SnapshotSource) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if src == nil {
		delete(s.snapSources, channelType)
		return
	}
	s.snapSources[channelType] = src
}

func (s *Server) snapshotSource(channelType string) SnapshotSource {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.snapSources[channelType]
}

// ServeHTTP upgrades the connection. The anonymous per-IP cap is
// enforced pre-upgrade (CAPACITY_EXCEEDED 503) and re-checked under the
// registry lock post-upgrade.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ip := middleware.ClientIP(r, s.cfg.TrustProxy)

	s.mu.Lock()
	if s.byIP[ip] >= s.cfg.MaxConnsPerIP {
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"type": "error", "error": "CAPACITY_EXCEEDED",
			"message": "per-IP WebSocket connection cap reached", "status": 503,
		})
		return
	}
	s.mu.Unlock()

	sock, err := s.up.Upgrade(w, r, nil)
	if err != nil {
		return // upgrader already answered
	}
	c := &Conn{
		srv: s, ws: sock, remoteIP: ip, path: r.URL.Path,
		out:      make(chan []byte, s.cfg.OutboundBuffer),
		done:     make(chan struct{}),
		pumpDone: make(chan struct{}),
		subs:     map[string]*subscription{},
		expiryCh: make(chan struct{}, 1),
		sem:      make(chan struct{}, s.cfg.DispatchConcurrency),
	}

	s.mu.Lock()
	if s.byIP[ip] >= s.cfg.MaxConnsPerIP {
		s.mu.Unlock()
		_ = sock.WriteMessage(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseTryAgainLater,
				"per-IP connection cap"))
		_ = sock.Close()
		return
	}
	s.conns[c] = struct{}{}
	s.byIP[ip]++
	s.metrics.ConnsOpened.Add(1)
	s.mu.Unlock()

	go c.writePump()
	go c.expiryLoop()
	c.readLoop() // blocks until socket dies
	s.unregister(c)
}

// unregister removes the conn from every index, waiting (bounded) for
// the write pump to flush the close frame first. Lock order is
// subMu → s.mu here because c.subs is detached under subMu first; the
// channel-index cleanup then runs under s.mu alone — no path holds s.mu
// while acquiring subMu, so no lock-order cycle exists.
func (s *Server) unregister(c *Conn) {
	c.closeConn(websocket.CloseNormalClosure, "bye")
	select {
	case <-c.pumpDone:
	case <-time.After(15 * time.Second): // write deadlines cap the wait
	}
	c.subMu.Lock()
	chans := make([]Channel, 0, len(c.subs))
	for _, sub := range c.subs {
		chans = append(chans, sub.ch)
	}
	c.subs = map[string]*subscription{}
	c.subMu.Unlock()

	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.conns, c)
	if s.byIP[c.remoteIP]--; s.byIP[c.remoteIP] <= 0 {
		delete(s.byIP, c.remoteIP)
	}
	if sess := c.session(); sess.Authenticated && sess.AccountID != 0 {
		if s.byAccount[sess.AccountID]--; s.byAccount[sess.AccountID] <= 0 {
			delete(s.byAccount, sess.AccountID)
		}
	}
	for _, ch := range chans {
		s.unnoteVariantLocked(ch)
		if cs := s.channels[ch.Raw]; cs != nil {
			delete(cs.subs, c)
			cs.prunePriv()
			if cs.empty() {
				delete(s.channels, ch.Raw)
			}
		}
	}
}

// demote drops the auth binding (expiry teardown with surviving public
// subscriptions): the account index is released and the session resets
// to anonymous.
func (s *Server) demote(c *Conn) {
	s.mu.Lock()
	sess := c.session()
	if sess.Authenticated && sess.AccountID != 0 {
		if s.byAccount[sess.AccountID]--; s.byAccount[sess.AccountID] <= 0 {
			delete(s.byAccount, sess.AccountID)
		}
	}
	s.mu.Unlock()
	c.setSession(ws.Session{ProtocolVer: sess.ProtocolVer})
}

// unsubscribeLocked removes a channel → conn index entry (caller holds
// c.subMu as documented per call site; takes s.mu internally).
func (s *Server) unsubscribeLocked(c *Conn, ch Channel) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.unnoteVariantLocked(ch)
	if cs := s.channels[ch.Raw]; cs != nil {
		delete(cs.subs, c)
		cs.prunePriv()
		if cs.empty() {
			delete(s.channels, ch.Raw)
		}
	}
}

// subscribe indexes a conn under a channel, creating the channel state
// (with its replay buffer) on first use.
func (s *Server) subscribe(c *Conn, ch Channel) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cs := s.channels[ch.Raw]
	if cs == nil {
		cs = &channelState{
			subs: map[*Conn]struct{}{},
			ring: newRingBuffer(s.cfg.ReplayBufferMsgs,
				s.cfg.ReplayBufferAge, s.cfg.Now),
			isPriv: ch.Private,
		}
		s.channels[ch.Raw] = cs
	}
	cs.subs[c] = struct{}{}
	s.noteVariantLocked(ch)
}

// ---------------------------------------------------------------------------
// Depth-variant index (Task 6.3.15) — the conflator multiplexes the
// internal 20-level snapshot into subscribed {levels}:{cadence} variants.
// ---------------------------------------------------------------------------

// noteVariantLocked records one live depth-variant subscription.
// Caller holds s.mu.
func (s *Server) noteVariantLocked(ch Channel) {
	if ch.Type != "depth" || ch.Params == "" {
		return // bare depth@{symbol} is served by the master 20:100 emit
	}
	m := s.depthIdx[ch.Target]
	if m == nil {
		m = map[DepthVariant]int{}
		s.depthIdx[ch.Target] = m
	}
	m[ch.Depth]++
}

// unnoteVariantLocked releases one live depth-variant subscription.
// Caller holds s.mu.
func (s *Server) unnoteVariantLocked(ch Channel) {
	if ch.Type != "depth" || ch.Params == "" {
		return
	}
	m := s.depthIdx[ch.Target]
	if m == nil {
		return
	}
	if m[ch.Depth]--; m[ch.Depth] <= 0 {
		delete(m, ch.Depth)
	}
	if len(m) == 0 {
		delete(s.depthIdx, ch.Target)
	}
}

// ActiveSymbolsFor returns the distinct symbols carrying at least one
// live subscription under channelType ("referencePrice", ...) — the
// per-symbol poll seam for producer streams (Task 6.3.17's
// RefPriceStream polls only what clients actually consume). Returns nil
// when no channel of the type is bound.
func (s *Server) ActiveSymbolsFor(channelType string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	seen := map[string]bool{}
	prefix := channelType + "@"
	for raw, cs := range s.channels {
		if len(cs.subs) == 0 || !strings.HasPrefix(raw, prefix) {
			continue
		}
		ch, err := ParseChannel(raw)
		if err != nil || seen[ch.Target] {
			continue
		}
		seen[ch.Target] = true
		out = append(out, ch.Target)
	}
	return out
}

// ActiveDepthVariants returns the live depth-variant subscriptions per
// symbol — the conflator's VariantSource seam (wired as
// ConflatorConfig.VariantSource in cmd/marketdata).
func (s *Server) ActiveDepthVariants() map[string][]DepthVariant {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string][]DepthVariant, len(s.depthIdx))
	for sym, m := range s.depthIdx {
		vars := make([]DepthVariant, 0, len(m))
		for v := range m {
			vars = append(vars, v)
		}
		out[sym] = vars
	}
	return out
}

// ---------------------------------------------------------------------------
// Entitlements (Task 6.3.22 item 3, spec §10.7)
// ---------------------------------------------------------------------------

// PrivateWSPath is the private-surface upgrade path (Task 6.3.5/6.3.1:
// /ws/v1/orders is the auth-required endpoint). Only private:* channels
// may be subscribed there — public feeds live on /ws/v1/marketdata.
const PrivateWSPath = "/ws/v1/orders"

// onPrivateEndpoint reports whether the conn upgraded on the private
// surface (path-scoped policy, not per-request guessing).
func (c *Conn) onPrivateEndpoint() bool { return c.path == PrivateWSPath }

// checkEntitlement runs the §10.7 entitlement gate for one channel bind.
// Denied binds always surface ENTITLEMENT_REQUIRED (§23, HTTP 403) with
// the checker's wire-safe reason. A nil checker admits: entitlement
// tables are a deployment artifact (backoffice-driven), and a service
// booted without one serves the public FX surface open.
func (s *Server) checkEntitlement(sess *ws.Session, ch Channel) (ok bool, reason string) {
	if s.cfg.Entitlements == nil {
		return true, ""
	}
	ok, reason = s.cfg.Entitlements.Check(sess, ch)
	if !ok {
		s.metrics.EntitlementRejections.Add(1)
	}
	return ok, reason
}

// ---------------------------------------------------------------------------
// Publish fanout — producers (L2 conflator, Wave-2 streams) feed frames here.
// ---------------------------------------------------------------------------

// Publish emits one channel event to every subscriber and appends the
// marshaled frame to the channel's replay buffer. seq is the
// channel-scoped sequence (for L2 channels: the md:seq cursor's
// last_seq) — clients use it as the resume cursor. Non-monotonic seqs
// are still delivered (the producer owns the seq domain) but are not
// appended to the ring — a seq regression can never poison the replay
// horizon.
//
// Fanout never blocks: subscribers hold a 1024-deep outbound queue and a
// client that cannot drain within SlowConsumerTimeout is evicted with
// 4008 — the conflation pipeline must never stall on a wedged socket
// (§10.6 slow-consumer policy).
func (s *Server) Publish(channel string, seq uint64, data any) {
	started := s.cfg.Now()
	b, err := marshalFrame(eventFrame{
		Type: "event", Channel: channel, Seq: seq,
		Data: data, TsMs: started.UnixMilli(),
	})
	if err != nil {
		s.cfg.Logger.Error("marketdata: publish marshal failed",
			"channel", channel, "err", err)
		return
	}

	s.mu.Lock()
	cs := s.channels[channel]
	if cs == nil {
		cs = &channelState{
			subs: map[*Conn]struct{}{},
			ring: newRingBuffer(s.cfg.ReplayBufferMsgs,
				s.cfg.ReplayBufferAge, s.cfg.Now),
		}
		s.channels[channel] = cs
	}
	// Defense in depth (Task 6.3.5): the public Publish path NEVER
	// appends to or fans out on a private channel — private frames are
	// account-scoped and must flow through PublishPrivate's per-account
	// ring. A misrouted producer call is dropped loudly, not leaked.
	if cs.isPriv {
		s.mu.Unlock()
		s.cfg.Logger.Error("marketdata: Publish on private channel refused — use PublishPrivate",
			"channel", channel)
		return
	}
	cs.ring.append(seq, b)
	conns := make([]*Conn, 0, len(cs.subs))
	for c := range cs.subs {
		conns = append(conns, c)
	}
	s.mu.Unlock()

	for _, c := range conns {
		c.subMu.Lock()
		_, subscribed := c.subs[channel]
		tier := ""
		if subscribed {
			tier = c.session().Tier
		}
		c.subMu.Unlock()
		if !subscribed {
			continue
		}
		c.enqueue(b)
		s.metrics.ObserveTierDelivery(tier) // §10.7 fair-use counters
	}
	s.metrics.FramesPublished.Add(1)
	s.metrics.ObserveSend(time.Since(started))
}

// PublishPrivate emits to subscribers of a private:* channel whose
// session binds accountID — account isolation is the whole point of the
// private: prefix. The frame lands on the channel's PER-ACCOUNT replay
// ring (Task 6.3.9 item 7: private sequences are keyed by
// (user_id, channel)); a shared ring would replay other accounts' order
// events to a reconnecting client — that leak is a defect, not a
// feature, so the private ring is strictly partitioned.
func (s *Server) PublishPrivate(accountID int64, channel string, seq uint64, data any) {
	if !strings.HasPrefix(channel, "private:") {
		// Fail closed: a private publish on a public channel token is a
		// producer wiring bug — never smuggle account data onto the
		// public fanout/ring.
		s.cfg.Logger.Error("marketdata: PublishPrivate on non-private channel refused",
			"channel", channel, "account", accountID)
		return
	}
	started := s.cfg.Now()
	b, err := marshalFrame(eventFrame{
		Type: "event", Channel: channel, Seq: seq,
		Data: data, TsMs: started.UnixMilli(),
	})
	if err != nil {
		s.cfg.Logger.Error("marketdata: publish marshal failed",
			"channel", channel, "err", err)
		return
	}
	s.mu.Lock()
	cs := s.channels[channel]
	if cs == nil {
		cs = &channelState{
			subs: map[*Conn]struct{}{},
			ring: newRingBuffer(s.cfg.ReplayBufferMsgs,
				s.cfg.ReplayBufferAge, s.cfg.Now),
			isPriv: true, // PublishPrivate only ever serves private:*
		}
		s.channels[channel] = cs
	} else if !cs.isPriv {
		// A private publish landing on a channel created for public use
		// is a producer wiring bug — mark it so Publish stays barred.
		cs.isPriv = true
	}
	cs.privRing(&s.cfg, accountID).append(seq, b)
	cs.prunePriv()
	conns := make([]*Conn, 0, len(cs.subs))
	for c := range cs.subs {
		conns = append(conns, c)
	}
	s.mu.Unlock()
	for _, c := range conns {
		c.subMu.Lock()
		sub, subscribed := c.subs[channel]
		sess := c.session()
		ok := subscribed && sub != nil && sess.AccountID == accountID
		c.subMu.Unlock()
		if ok {
			c.enqueue(b)
			s.metrics.ObserveTierDelivery(sess.Tier)
		}
	}
	s.metrics.FramesPublished.Add(1)
	s.metrics.PrivateFrames.Add(1)
	s.metrics.ObserveSend(time.Since(started))
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
	for _, cs := range s.channels {
		subs += len(cs.subs)
	}
	return conns, authed, subs
}

// channelTail returns the replay buffer tail seq for a channel (0 when
// empty/absent) — resume uses it to classify cursors; tests assert it.
func (s *Server) channelTail(channel string) uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	if cs := s.channels[channel]; cs != nil {
		return cs.ring.tailSeq()
	}
	return 0
}

// ---------------------------------------------------------------------------
// Conn — one upgraded WebSocket connection.
// ---------------------------------------------------------------------------

// Conn is one upgraded WebSocket connection. All ws writes happen in
// writePump (gorilla requires a single writer); producers enqueue framed
// bytes on out. Teardown: closeConn flips done once, the pump emits the
// close frame and returns, serve() unregisters the conn.
type Conn struct {
	srv      *Server
	ws       *websocket.Conn
	remoteIP string
	path     string // upgrade path — the WS signature surface

	out       chan []byte
	done      chan struct{}
	pumpDone  chan struct{} // closed by writePump AFTER the close frame goes out
	closeOne  sync.Once
	closeCode atomic.Int32
	closeWhy  atomic.Value // string

	sessMu sync.Mutex
	sess   ws.Session

	subMu sync.Mutex
	subs  map[string]*subscription
	l2n   int // live L2-class subs (budget: MaxL2Subscriptions)
	l3n   int // live L3-class subs (budget: MaxL3Subscriptions)

	satSince atomic.Int64 // unixnano when `out` first stayed full (0 = draining)

	expiryCh chan struct{}

	rateMu sync.Mutex
	tokens float64   // control-frame token bucket
	rateAt time.Time // last refill

	churnMu sync.Mutex
	churn   []time.Time // recent subscribe/unsubscribe timestamps

	sem chan struct{} // concurrent order-action bound
}

func (c *Conn) session() ws.Session {
	c.sessMu.Lock()
	defer c.sessMu.Unlock()
	return c.sess
}

func (c *Conn) setSession(s ws.Session) {
	c.sessMu.Lock()
	c.sess = s
	c.sessMu.Unlock()
	select {
	case c.expiryCh <- struct{}{}:
	default:
	}
}

// enqueue offers a frame to the write pump — NON-BLOCKING (Task 6.3.21
// item 1): a fanout must never stall on one wedged socket. A saturated
// buffer drops the frame and starts a saturation timer; a client whose
// buffer stays full for SlowConsumerTimeout (2s) is terminated with 4008.
// Dropped frames are counted; the channel's seq envelope surfaces the
// hole to the client (§10.9 gap detection → resync).
func (c *Conn) enqueue(b []byte) {
	select {
	case c.out <- b:
		c.satSince.Store(0)
		return
	case <-c.done:
		return
	default:
	}
	now := c.srv.cfg.Now()
	for {
		sat := c.satSince.Load()
		if sat == 0 {
			if c.satSince.CompareAndSwap(0, now.UnixNano()) {
				c.srv.metrics.FramesDropped.Add(1)
				return
			}
			continue
		}
		if time.Unix(0, sat).Add(c.srv.cfg.SlowConsumerTimeout).Before(now) {
			c.closeConn(CloseSlowConsumer, "WS_SLOW_CONSUMER_DROP")
			return
		}
		c.srv.metrics.FramesDropped.Add(1)
		return
	}
}

// closeConn initiates teardown: the write pump emits the close frame
// (best-effort 1s deadline) and its deferred ws.Close() frees the
// socket. Idempotent — the first close code wins.
func (c *Conn) closeConn(code int, reason string) {
	c.closeOne.Do(func() {
		c.closeCode.Store(int32(code))
		c.closeWhy.Store(reason)
		close(c.done)
	})
}

// writePump is the sole writer and owns socket teardown. On done it
// first drains queued outbound frames (a terminal error frame must
// precede its close frame on the wire), emits the close control frame,
// then returns; the deferred ws.Close() frees the socket. Ping frames
// run on the 30s cadence (§10.6 heartbeat).
func (c *Conn) writePump() {
	ping := time.NewTicker(c.srv.cfg.PingInterval)
	defer ping.Stop()
	defer close(c.pumpDone)
	defer c.ws.Close()
	for {
		select {
		case b := <-c.out:
			_ = c.ws.SetWriteDeadline(c.srv.cfg.Now().Add(10 * time.Second))
			if err := c.ws.WriteMessage(websocket.TextMessage, b); err != nil {
				return
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

// flushOut drains queued outbound frames best-effort on teardown.
func (c *Conn) flushOut() {
	for {
		select {
		case b := <-c.out:
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

// allowControl is the per-connection control-frame token bucket
// (§10.6: breach emits WS_RATE_EXCEEDED). Bucket refills at ControlRate
// tokens/s with a 1-second burst capacity.
func (c *Conn) allowControl() (retryAfterMs int64, ok bool) {
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
	if c.tokens >= 1 {
		c.tokens--
		return 0, true
	}
	return int64((1-c.tokens)/rate*1000) + 1, false
}

// noteChurn records a subscription change. Sustained churn above
// MaxChurnPerWindow within ChurnWindow terminates the session with 4003
// + WS_ABUSE_DETECTED (§10.6 abuse patterns).
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
// AUTH_EXPIRED, tears down private subscriptions, then either demotes
// the connection to anonymous (public subs remain) or closes with 4019
// when no public subscriptions survive (§10.5 item 5, §10.6).
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

	c.subMu.Lock()
	remaining := 0
	for ch, sub := range c.subs {
		if sub.ch.Private {
			c.srv.unsubscribeLocked(c, sub.ch)
			delete(c.subs, ch)
			continue
		}
		remaining++
	}
	c.subMu.Unlock()

	if remaining == 0 {
		c.closeConn(CloseAuthExpired, "AUTH_EXPIRED")
		return
	}
	c.srv.demote(c)
}

// readLoop is the inbound pump: one frame → one control action. Order
// actions run on dedicated goroutines (bounded by sem) so a slow engine
// dispatch never stalls market-data writes or control frames
// (Task 6.3.10 concurrency isolation).
//
// Heartbeat: the read deadline is armed for PongWait (60s) at connect
// and re-armed by every client pong (gorilla PongHandler); a client that
// does not answer the 30s ping within 60s fails its next ReadMessage and
// the conn tears down (Task 6.3.1 AC).
func (c *Conn) readLoop() {
	c.ws.SetReadLimit(c.srv.cfg.MaxFrameBytes)
	_ = c.ws.SetReadDeadline(c.srv.cfg.Now().Add(c.srv.cfg.PongWait))
	c.ws.SetPongHandler(func(string) error {
		return c.ws.SetReadDeadline(c.srv.cfg.Now().Add(c.srv.cfg.PongWait))
	})
	for {
		mt, msg, err := c.ws.ReadMessage()
		if err != nil {
			return
		}
		if mt != websocket.TextMessage && mt != websocket.BinaryMessage {
			continue
		}
		if ms, ok := c.allowControl(); !ok {
			c.sendError("", "", "WS_RATE_EXCEEDED",
				"control message rate exceeded", ms)
			continue
		}
		if hErr := c.handleMessage(msg); hErr != nil {
			return
		}
	}
}

// errCloseRead signals handleMessage wants the read loop to stop.
var errCloseRead = errors.New("marketdata: close read loop")

// codeOf extracts the §23 code from a domain error; unknown errors
// degrade to INTERNAL_ERROR (registry-gated).
func codeOf(err error) string {
	var e *excerrors.Error
	if errors.As(err, &e) && e.Code != "" {
		return e.Code
	}
	return "INTERNAL_ERROR"
}
