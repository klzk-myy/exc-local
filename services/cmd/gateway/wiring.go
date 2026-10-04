package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	flatbuffers "github.com/google/flatbuffers/go"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go/jetstream"

	"exchange/internal/admin"
	"exchange/internal/analytics"
	"exchange/internal/api"
	"exchange/internal/auth"
	"exchange/internal/bridge"
	"exchange/internal/compliance"
	"exchange/internal/config"
	"exchange/internal/ipc"
	"exchange/internal/ipc/wire"
	"exchange/internal/marketdata"
	"exchange/internal/marketdata/ohlcv"
	"exchange/internal/middleware"
	"exchange/internal/nats"
	"exchange/internal/notifications"
	"exchange/internal/orders"
	"exchange/internal/redis"
	"exchange/internal/risk"
	"exchange/internal/settlement"
	"exchange/internal/sor"
	"exchange/internal/tracing"
	"exchange/internal/ws"
	"exchange/pkg/decimal"
)

// readiness composes the Task 7.3.6 dependency-checked readiness probe:
// PostgreSQL + Redis + engine IPC rings are required (503 on failure),
// NATS is optional (JetStream dispatch is fail-operational — a down bus
// degrades, never pulls the pod). Mode and shard liveness fold in per
// the R9 schema.
func readiness(rdb *redis.Client, pool *pgxpool.Pool, nc *nats.Client,
	sub *orders.ShmSubmitter, shardMap *config.ShardMap, version string) http.HandlerFunc {
	deps := []api.Dependency{
		{Name: "postgres", Required: true, Probe: pool.Ping},
		{Name: "redis", Required: true, Probe: rdb.Ping},
		{Name: "nats", Required: false, Probe: func(ctx context.Context) error {
			if nc == nil {
				return fmt.Errorf("nats client not configured")
			}
			// FlushWithContext is a real round-trip probe — stronger
			// than Connected() (which only reflects socket state).
			return nc.Conn().FlushWithContext(ctx)
		}},
		// Engine ring probe: every shard's far-end producer (the C++
		// matching engine) must stamp a live pid. A dead engine is a
		// required failure — orders would black-hole.
		{Name: "engine_ipc", Required: true, Probe: func(ctx context.Context) error {
			for _, id := range shardIDs(shardMap) {
				ch, err := sub.Channel(id)
				if err != nil {
					return fmt.Errorf("shard %d channel: %w", id, err)
				}
				if !ch.ProducerAlive() {
					return fmt.Errorf("shard %d engine producer not alive", id)
				}
			}
			return nil
		}},
	}
	shards := func(ctx context.Context) []api.ShardHealth {
		ids := shardIDs(shardMap)
		out := make([]api.ShardHealth, 0, len(ids))
		for _, id := range ids {
			sh := api.ShardHealth{ID: int(id), Status: "ok"}
			ch, err := sub.Channel(id)
			if err != nil || !ch.ProducerAlive() {
				sh.Status = "down"
			} else {
				sh.Leader = true
			}
			out = append(out, sh)
		}
		return out
	}
	return api.HealthReady(func(ctx context.Context) (string, error) {
		st, err := rdb.GetDegradationMode(ctx)
		return string(st.Mode), err
	}, deps, shards, version)
}

// ---------------------------------------------------------------------------
// Phase-12 Task 12.3.5 helpers — funding notifier adapter + WS fanout
// ---------------------------------------------------------------------------

// notifyAdapter adapts a closure to funding.Notifier.
type notifyAdapter struct {
	fn func(ctx context.Context, accountID int64, event string, payload map[string]any)
}

func (a notifyAdapter) Notify(ctx context.Context, accountID int64, event string, payload map[string]any) {
	a.fn(ctx, accountID, event, payload)
}

// categorizerAdapter adapts the Task 14.3.7 categorization service to
// the accounts package's minimal Categorizer seam (Category only — the
// Appropriateness check runs as its own checkAdmission gate upstream).
type categorizerAdapter struct {
	svc *compliance.CategorizationService
}

func (a categorizerAdapter) Category(ctx context.Context, accountID int64) (string, error) {
	c, err := a.svc.Category(ctx, accountID)
	return string(c), err
}

// ---- shm-topology JetStream republisher --------------------------------
//
// In the shm-only deployment there is no Aeron bridge: the engine's only
// outbound transport is the SPSC _out ring and this process is its sole
// consumer. To keep the JetStream streams the bridge would feed alive
// (internal/bridge's route table), the dispatch mirror and the frame tap
// enqueue raw wire frames here; repubQueue.run publishes them under the
// same "{stream}.{shard}.{symbol}" subject + Nats-Msg-Id dedup contract.

// repubEvent is one frame routed for republish. scope namespaces the
// msgID sequence domain: "s" = engine seq (_out frames — the bridge's
// "s{shard}-{seq}" key), "g" = gateway dispatch seq (_in frames).
// Distinct prefixes keep the per-stream dedup window from collapsing
// unrelated events that share a numeric seq.
type repubEvent struct {
	shard   uint16
	scope   string
	seq     uint64
	symbol  string
	payload []byte
	streams []string
}

type repubQueue chan repubEvent

// enqueue copies the frame into the queue or drops when full — the
// republish path must never backpressure order traffic; consumers
// gap-recover via seq cursors.
func (q repubQueue) enqueue(shard uint16, scope string, seq uint64,
	symbol string, payload []byte, streams ...string) {
	select {
	case q <- repubEvent{shard: shard, scope: scope, seq: seq, symbol: symbol,
		payload: append([]byte(nil), payload...), streams: streams}:
	default:
	}
}

// run drains the queue onto JetStream. A nil client (NATS down at boot)
// idles the drain — frames drop at publish time, counted on the second
// power-of-two edge to keep the log quiet.
func (q repubQueue) run(ctx context.Context, nc *nats.Client, log *slog.Logger) {
	pub := &jetstreamFillPublisher{}
	if nc != nil {
		pub.js = nc.JetStream()
	}
	var dropped atomic.Uint64
	for {
		select {
		case <-ctx.Done():
			return
		case it := <-q:
			if pub.js == nil {
				continue
			}
			msgID := fmt.Sprintf("%s%d-%d", it.scope, it.shard, it.seq)
			for _, stream := range it.streams {
				subj, err := nats.Subject(stream, uint32(it.shard), it.symbol)
				if err != nil {
					// Same reroute as the bridge: an invalid token goes to
					// the UNKNOWN ordering domain instead of dropping.
					if subj, err = nats.Subject(stream, uint32(it.shard),
						bridge.UnknownSymbol); err != nil {
						continue
					}
				}
				pctx, cancel := context.WithTimeout(ctx, 2*time.Second)
				err = pub.PublishEvent(pctx, subj, msgID, it.payload)
				cancel()
				if err != nil {
					if n := dropped.Add(1); n&(n-1) == 0 {
						log.Warn("bridge-role republish failed",
							"subject", subj, "err", err, "total", n)
					}
				}
			}
		}
	}
}

// syncOrderIndex adds mutual exclusion to the shared nats.OrderIndex —
// the bridge/marketdata indexes live on a single goroutine, but here
// Put runs on the order-dispatch path while Get runs on the _out frame
// tap.
type syncOrderIndex struct {
	mu  sync.Mutex
	idx *nats.OrderIndex[string]
}

func newSyncOrderIndex(capacity int) *syncOrderIndex {
	return &syncOrderIndex{idx: nats.NewOrderIndex[string](capacity)}
}

func (i *syncOrderIndex) Put(id uint64, sym string) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.idx.Put(id, sym)
}

func (i *syncOrderIndex) Get(id uint64) (string, bool) {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.idx.Get(id)
}

// dispatchMirror wraps the engine-bound Submitter: after a frame lands
// on the _in ring it is also mirrored into the market-data admission
// feed and republished onto the order-lifecycle streams. The engine
// never echoes commands on _out, so the dispatch point is the only
// place OrderNew/OrderAmend events exist for downstream consumers.
type dispatchMirror struct {
	inner  orders.Submitter
	frames chan<- []byte
	repub  repubQueue
	syms   *syncOrderIndex
	res    marketdata.InstrumentResolver
}

func (m *dispatchMirror) Send(ctx context.Context, shard uint16, payload []byte) error {
	if err := m.inner.Send(ctx, shard, payload); err != nil {
		return err
	}
	// Mirror for WireTradeSource's admission index — every frame type
	// flows; the decoder filters. Drop-on-full like the _out tap mirror.
	select {
	case m.frames <- append([]byte(nil), payload...):
	default:
	}
	body, _, _ := tracing.StripAeronTrace(payload)
	ev := ipc.DecodeEvent(body)
	if ev == nil {
		return nil
	}
	switch ev.TypeType() {
	case wire.EventTypeOrderNew:
		on := ipc.EventOrderNew(ev)
		if on == nil {
			return nil
		}
		sym, ok := m.res.Symbol(on.InstrumentId())
		if !ok {
			sym = fmt.Sprintf("instr-%d", on.InstrumentId())
		}
		tok := nats.SymbolToken(sym)
		m.syms.Put(on.OrderId(), tok)
		m.repub.enqueue(shard, "g", ev.Seq(), tok, payload,
			bridge.StreamsForEvent(ev.TypeType())...)
	case wire.EventTypeOrderAmend:
		var t flatbuffers.Table
		if ev.Type(&t) {
			oa := &wire.OrderAmend{}
			oa.Init(t.Bytes, t.Pos)
			tok := bridge.UnknownSymbol
			if s, ok := m.syms.Get(oa.OrderId()); ok {
				tok = s
			}
			m.repub.enqueue(shard, "g", ev.Seq(), tok, payload,
				bridge.StreamsForEvent(ev.TypeType())...)
		}
	}
	return nil
}

func (m *dispatchMirror) Channel(shard uint16) (*ipc.Channel, error) {
	return m.inner.Channel(shard)
}

// jetstreamFillPublisher adapts the JetStream context to the
// settlement.TradeRepublisher seam — publishes with Nats-Msg-Id dedup,
// the bridge.Publisher contract for TradeFill fan-out. It also
// implements settlement.AsyncTradeRepublisher: PublishEventAsync
// pipelines onto PublishAsync and FlushEvents collects every
// outstanding PubAckFuture — per-fill republish stays synchronous
// semantically (commit → all acks → next batch) without paying two
// publish RTTs per fill.
type jetstreamFillPublisher struct {
	js      jetstream.JetStream
	pending []jetstream.PubAckFuture
}

func (p *jetstreamFillPublisher) PublishEvent(ctx context.Context, subject, msgID string, payload []byte) error {
	if p.js == nil {
		return fmt.Errorf("jetstream fill publisher: nil context")
	}
	_, err := p.js.Publish(ctx, subject, payload, jetstream.WithMsgID(msgID))
	return err
}

func (p *jetstreamFillPublisher) PublishEventAsync(subject, msgID string, payload []byte) error {
	if p.js == nil {
		return fmt.Errorf("jetstream fill publisher: nil context")
	}
	f, err := p.js.PublishAsync(subject, payload, jetstream.WithMsgID(msgID))
	if err != nil {
		return err
	}
	p.pending = append(p.pending, f)
	return nil
}

func (p *jetstreamFillPublisher) FlushEvents(ctx context.Context) error {
	// Bounded in-flight: waiting for EVERY outstanding ack each batch
	// would serialize the fill consumer on the slowest ack RTT per
	// flush. Block only while more than repubInFlightCap acks are
	// outstanding — PubAckFutures resolve in publish order on the
	// single NATS connection, so draining oldest-first preserves the
	// commit→publish ordering while a slow ack surfaces as real
	// backlog rather than a per-batch latency tax.
	const repubInFlightCap = 512
	var first error
	// Reap already-resolved acks non-blockingly so failures surface on
	// the first flush after they land rather than only under backlog.
	for len(p.pending) > 0 {
		f := p.pending[0]
		select {
		case <-f.Ok():
			p.pending = p.pending[1:]
		case err := <-f.Err():
			if first == nil {
				first = err
			}
			p.pending = p.pending[1:]
		default:
			goto reaped
		}
	}
reaped:
	for len(p.pending) > repubInFlightCap {
		f := p.pending[0]
		p.pending = p.pending[1:]
		select {
		case <-f.Ok():
		case err := <-f.Err():
			if first == nil {
				first = err
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return first
}

// pnlPublisherFunc adapts a closure to risk.PnlPublisher — the ws hub
// is late-bound (constructed after the order consumer starts), so the
// closure checks wsSrv at publish time rather than capturing a nil.
type pnlPublisherFunc func(accountID int64, channel string, data any)

// PublishPrivate implements risk.PnlPublisher.
func (f pnlPublisherFunc) PublishPrivate(accountID int64, channel string, data any) {
	f(accountID, channel, data)
}

// parseLiquidationClientID decodes the Phase-19 liquidation
// client_order_id conventions (internal/risk):
//
//	liq-{position_id}-{ms}       — direct close order
//	auc-{auction_id}-{ms}        — auction leg
//	auc-fc-{auction_id}-{ms}     — force-cash leg
//
// Anything else is not a liquidation order (isLiq=false).
func parseLiquidationClientID(coid string) (posID, auctionID int64, isAuction, isFC, isLiq bool) {
	if rest, ok := strings.CutPrefix(coid, "liq-"); ok {
		if idStr, _, ok := strings.Cut(rest, "-"); ok {
			posID, _ = strconv.ParseInt(idStr, 10, 64)
			return posID, 0, false, false, posID > 0
		}
		return 0, 0, false, false, false
	}
	if rest, ok := strings.CutPrefix(coid, "auc-"); ok {
		isAuction = true
		if fc, ok := strings.CutPrefix(rest, "fc-"); ok {
			isFC, rest = true, fc
		}
		if idStr, _, ok := strings.Cut(rest, "-"); ok {
			auctionID, _ = strconv.ParseInt(idStr, 10, 64)
		}
		return 0, auctionID, isAuction, isFC, auctionID > 0
	}
	return 0, 0, false, false, false
}

// parseADLClientID extracts the directive sequence from an ADL
// force-close client order id (adl-{adl_seq}). Anything else is not an
// ADL order (isADL=false).
func parseADLClientID(coid string) (seq int64, isADL bool) {
	if rest, ok := strings.CutPrefix(coid, "adl-"); ok {
		seq, _ = strconv.ParseInt(strings.TrimSpace(rest), 10, 64)
		return seq, seq > 0
	}
	return 0, false
}

// redisPatternLister implements risk.MarginLevelHashLister — a generic
// SCAN over the coordination Redis for margin:level:* keys.
type redisPatternLister struct{ c *redis.Client }

// ScanKeys implements risk.MarginLevelHashLister.
func (l redisPatternLister) ScanKeys(ctx context.Context, pattern string) ([]string, error) {
	var keys []string
	iter := l.c.Scan(ctx, 0, pattern, 200).Iterator()
	for iter.Next(ctx) {
		keys = append(keys, iter.Val())
	}
	return keys, iter.Err()
}

// wsNotifyPush fans a notification payload to every account the user
// owns: ws private channels are account-scoped (session binding) while
// notifications are user-scoped, so the adapter expands user→accounts
// and lets Server.PublishPrivate enforce the per-account subscription.
func wsNotifyPush(ctx context.Context, srv *ws.Server, pool *pgxpool.Pool,
	userID int64, channel string, data any) error {
	if srv == nil {
		return nil // hub not constructed yet — no subscribers can exist
	}
	rows, err := pool.Query(ctx,
		`SELECT id FROM accounts WHERE user_id = $1`, userID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var acct int64
		if err := rows.Scan(&acct); err != nil {
			return err
		}
		srv.PublishPrivate(acct, channel, data)
	}
	return rows.Err()
}

// ---------------------------------------------------------------------------
// Cluster E helpers (Tasks 5.3.26/29/30/31/42)
// ---------------------------------------------------------------------------

// securityOpts derives the Task 5.3.29 item-9 header set from config.
// The CORS allowlist is env-driven (EXC_CORS_ORIGINS, comma-separated)
// — an empty list means NO cross-origin reads, the strict default.
// HSTS activates only when the gateway terminates TLS itself or a
// trusted edge exists: the reference deployment terminates at HAProxy,
// so EXC_GW_TLS=1 (or production env) arms HSTS.
func securityOpts(cfg *config.Config) middleware.SecurityOptions {
	var origins []string
	for _, o := range strings.Split(os.Getenv("EXC_CORS_ORIGINS"), ",") {
		if s := strings.TrimSpace(o); s != "" {
			origins = append(origins, s)
		}
	}
	opts := middleware.SecurityOptions{AllowedOrigins: origins}
	if cfg.IsProduction() || os.Getenv("EXC_GW_TLS") == "1" {
		opts.HSTSMaxAge = 63072000 // 2y, spec-conventional
	}
	return opts
}

// idemResolver scopes the idempotency ledger: Bearer claims first (auth
// middleware may precede), else a READ-ONLY API-key lookup — never
// Verify, so the handler's replay guard is not double-consumed.
func idemResolver(ks *auth.KeyStore) middleware.AccountResolver {
	return func(ctx context.Context, r *http.Request) (int64, bool) {
		if id, ok := middleware.ClaimsAccountResolver(ctx, r); ok {
			return id, true
		}
		if ks != nil {
			if kid := strings.TrimSpace(r.Header.Get("X-API-KEY")); kid != "" {
				if key, err := ks.Get(ctx, kid); err == nil && key != nil && key.AccountID != 0 {
					return key.AccountID, true
				}
			}
		}
		return 0, false
	}
}

// ---------------------------------------------------------------------------
// Phase-12 helpers
// ---------------------------------------------------------------------------

// securityEventAdapter bridges the narrow auth.SecurityEventNotifier seam
// (Tasks 12.3.7 clone detection, 12.3.12 lockout) onto the notification
// service's registered security_alert event.
type securityEventAdapter struct{ svc *notifications.Service }

func (a securityEventAdapter) NotifySecurityEvent(ctx context.Context, userID int64,
	event string, attrs map[string]any) error {
	payload := map[string]any{"kind": event}
	for k, v := range attrs {
		payload[k] = v
	}
	_, err := a.svc.Notify(ctx, userID, notifications.EventSecurityAlert, payload)
	return err
}

// webAuthnConfig derives relying-party identity for Task 12.3.7:
// EXC_WEBAUTHN_RP_ID / EXC_WEBAUTHN_RP_NAME / EXC_WEBAUTHN_ORIGINS
// (comma-separated fully-qualified origins) win; otherwise the values
// derive from EXC_PUBLIC_BASE_URL. WebAuthn fails closed on a bad RP
// configuration — the ceremony cannot verify without a matching origin.
func webAuthnConfig() auth.WebAuthnConfig {
	cfg := auth.WebAuthnConfig{
		RPID:          os.Getenv("EXC_WEBAUTHN_RP_ID"),
		RPDisplayName: os.Getenv("EXC_WEBAUTHN_RP_NAME"),
	}
	for _, o := range strings.Split(os.Getenv("EXC_WEBAUTHN_ORIGINS"), ",") {
		if s := strings.TrimSpace(o); s != "" {
			cfg.RPOrigins = append(cfg.RPOrigins, s)
		}
	}
	if base := strings.TrimRight(os.Getenv("EXC_PUBLIC_BASE_URL"), "/"); base != "" {
		if u, err := url.Parse(base); err == nil {
			if cfg.RPID == "" {
				cfg.RPID = u.Hostname()
			}
			if len(cfg.RPOrigins) == 0 && u.Scheme != "" && u.Host != "" {
				cfg.RPOrigins = []string{u.Scheme + "://" + u.Host}
			}
		}
	}
	if cfg.RPID == "" {
		cfg.RPID = "exc.local" // dev default; production must set env
	}
	if cfg.RPDisplayName == "" {
		cfg.RPDisplayName = "Exchange"
	}
	if len(cfg.RPOrigins) == 0 {
		cfg.RPOrigins = []string{"https://" + cfg.RPID}
	}
	return cfg
}

// ---------------------------------------------------------------------------
// Phase-13.5 helpers
// ---------------------------------------------------------------------------

// loadVDPPolicy reads the canonical vulnerability-disclosure policy
// (content/security/policy.md, Task 13.5.3.8) for the public
// GET /api/v1/security/policy route. Resolution order:
// EXC_VDP_POLICY_PATH → ./content/security/policy.md → parent-dir
// fallbacks for binaries run from a subdirectory. A missing document is
// a startup warning + SERVICE_DEGRADED route, never a fabricated policy.
func loadVDPPolicy(log *slog.Logger) *api.VDPPolicyDoc {
	candidates := []string{}
	if p := os.Getenv("EXC_VDP_POLICY_PATH"); p != "" {
		candidates = append(candidates, p)
	}
	candidates = append(candidates,
		"content/security/policy.md",
		"../content/security/policy.md",
		"../../content/security/policy.md")
	for _, p := range candidates {
		b, err := os.ReadFile(p)
		if err == nil && len(b) > 0 {
			log.Info("VDP policy loaded", "path", p, "bytes", len(b))
			return &api.VDPPolicyDoc{
				ContentType: "text/markdown; charset=utf-8",
				Body:        b,
			}
		}
	}
	log.Warn("VDP policy document not found — /api/v1/security/policy will serve SERVICE_DEGRADED",
		"candidates", candidates)
	return nil
}

// --- Task 18.3.15 session.status WS→NATS bridge -----------------------------

// fanoutSessionPub fans session-event publication to the WS advisory
// channel plus the NATS subject of the same name (the FIX gateway's
// SessionStatusService consumes "session.status").
type fanoutSessionPub []admin.SessionPublisher

func (f fanoutSessionPub) Publish(channel string, data any) {
	for _, p := range f {
		p.Publish(channel, data)
	}
}

// sessionNATSPublisher adapts settlement.NatsPublisher to
// admin.SessionPublisher — the channel name doubles as the NATS subject.
// A publish failure is swallowed (WS advisory still landed); NATS
// availability is fail-operational for this advisory surface, matching
// the SessionPublisher contract.
type sessionNATSPublisher struct {
	pub settlement.NatsPublisher
}

func (p sessionNATSPublisher) Publish(channel string, data any) {
	b, err := json.Marshal(data)
	if err != nil {
		return
	}
	_ = p.pub.Publish(context.Background(), channel, b)
}

// runDailyUTC runs fn once per UTC day at minuteOfDay past 00:00 —
// the Phase-20 scheduler for statements (00:45), trial balance (00:15),
// ERP export (01:30) and the RTS28 quarter-roll check (02:00). The tick
// is computed fresh each iteration so DST never drifts the slot (UTC has
// no DST anyway) and a restart mid-window simply waits for the next day;
// every job body is idempotent, so a missed slot is replayable via its
// RunOnce rather than needing catch-up state.
func runDailyUTC(ctx context.Context, log *slog.Logger, name string,
	minuteOfDay int, fn func(context.Context)) {
	go func() {
		for {
			now := time.Now().UTC()
			next := now.Truncate(24 * time.Hour).Add(time.Duration(minuteOfDay) * time.Minute)
			if !next.After(now) {
				next = next.Add(24 * time.Hour)
			}
			t := time.NewTimer(next.Sub(now))
			select {
			case <-ctx.Done():
				t.Stop()
				return
			case <-t.C:
			}
			start := time.Now()
			fn(ctx)
			log.Debug("daily job completed", "job", name,
				"elapsed", time.Since(start).Round(time.Millisecond))
		}
	}()
}

// ---- Phase-3 Task 4 — SOR wiring helpers ---------------------------------

// atomicResolver is a hot-swappable marketdata.MapResolver — the frame
// tap resolves instrument ids on the read-model drain goroutine while
// the refresher swaps snapshots.
type atomicResolver struct{ v atomic.Value } // stores marketdata.MapResolver

// ---- Phase-06 public market-data fanout adapters ---------------------------
// The unified /ws/v1 surface emits the same §10.9 channels the
// marketdata service produces — these two shims adapt its exported
// component seams (ohlcv.Emitter, ohlcv.Source) onto ws.Server.Publish
// and the FanOut tap channels, so no parallel decoder/encoder exists.

// wsKlineEmitter forwards each finished kline@ envelope's Data payload
// through ws.Server.Publish — the server wraps it in its own eventFrame
// (channel/seq/ts_ms), so the KlineFrame envelope is not re-marshaled.
type wsKlineEmitter struct {
	pub func(channel string, data any)
}

func (e wsKlineEmitter) Emit(_ context.Context, f ohlcv.KlineFrame) error {
	e.pub(f.Channel, f.Data)
	return nil
}

// ohlcvTradeSource adapts a FanOut tap of marketdata.TradeEvent into the
// ohlcv.Source contract — field semantics are identical between the two
// TradeEvent projections (events.go documents them as mirrors).
type ohlcvTradeSource struct {
	events <-chan marketdata.TradeEvent
}

func (s ohlcvTradeSource) Events(ctx context.Context) (<-chan ohlcv.TradeEvent, error) {
	out := make(chan ohlcv.TradeEvent, 1024)
	go func() {
		defer close(out)
		for {
			select {
			case <-ctx.Done():
				return
			case ev, ok := <-s.events:
				if !ok {
					return
				}
				select {
				case out <- ohlcv.TradeEvent{
					TradeID:   ev.TradeID,
					Symbol:    ev.Symbol,
					Price:     ev.Price,
					Quantity:  ev.Quantity,
					TakerSide: ohlcv.Side(ev.TakerSide),
					Seq:       ev.Seq,
					Ts:        ev.Ts,
				}:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return out, nil
}

func (a *atomicResolver) Symbol(id uint32) (string, bool) {
	if m, ok := a.v.Load().(marketdata.MapResolver); ok {
		return m.Symbol(id)
	}
	return "", false
}

func (a *atomicResolver) swap(m marketdata.MapResolver) { a.v.Store(m) }

// mapNow exposes the current id→symbol map for enumerating consumers
// (e.g. the flash-crash scenario provider) that need more than a
// single-id lookup — InstrumentResolver.Symbol cannot enumerate.
func (a *atomicResolver) mapNow() marketdata.MapResolver {
	m, _ := a.v.Load().(marketdata.MapResolver)
	return m
}

// loadInstrumentResolver builds the wire instrument_id → canonical
// symbol map from the instruments table — same projection
// EXC_MARKETDATA_INSTRUMENTS supplies to cmd/marketdata, sourced from
// PG so the two services can never disagree.
func loadInstrumentResolver(ctx context.Context, pool *pgxpool.Pool,
	log *slog.Logger) *atomicResolver {
	r := &atomicResolver{}
	refreshInstrumentResolverOnce(ctx, pool, r, log)
	return r
}

func refreshInstrumentResolver(ctx context.Context, pool *pgxpool.Pool,
	r *atomicResolver, every time.Duration, log *slog.Logger) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			refreshInstrumentResolverOnce(ctx, pool, r, log)
		}
	}
}

func refreshInstrumentResolverOnce(ctx context.Context, pool *pgxpool.Pool,
	r *atomicResolver, log *slog.Logger) {
	rows, err := pool.Query(ctx, `SELECT id, symbol FROM instruments`)
	if err != nil {
		log.Warn("sor: instrument resolver refresh failed", "err", err)
		return
	}
	defer rows.Close()
	m := marketdata.MapResolver{}
	for rows.Next() {
		var (
			id  int64
			sym string
		)
		if err := rows.Scan(&id, &sym); err != nil {
			log.Warn("sor: instrument row scan failed", "err", err)
			return
		}
		m[uint32(id)] = sym
	}
	if err := rows.Err(); err != nil || len(m) == 0 {
		log.Warn("sor: instrument resolver refresh empty", "err", err)
		return // keep the last good map — never age to empty
	}
	r.swap(m)
}

// buildSORRouter constructs the router from env:
//
//	EXC_SOR_VENUES          comma list; "loopback:<name>" wires the dev
//	                        adapter. Unset/empty → nil (router never
//	                        consults, all orders submit locally).
//	EXC_SOR_MIN_DEPTH       decimal — required contra-side depth.
//	EXC_SOR_MAX_SPREAD_BPS  decimal — max quoted spread before external.
//
// Fill publishing goes to settlements.*.sorfill (FillBridgeSubjectToken);
// without a NATS client the publisher is nil and events are counted
// (Router's documented nil-publisher semantics).
func buildSORRouter(pool *pgxpool.Pool, nc *nats.Client,
	log *slog.Logger) *sor.Router {
	venueSpec := strings.TrimSpace(os.Getenv("EXC_SOR_VENUES"))
	if venueSpec == "" {
		return nil
	}
	var venues []sor.VenueConnector
	for _, tok := range strings.Split(venueSpec, ",") {
		tok = strings.TrimSpace(tok)
		if tok == "" {
			continue
		}
		kind, name, _ := strings.Cut(tok, ":")
		switch kind {
		case "loopback":
			venues = append(venues,
				sor.NewLoopbackConnector(name, "EXC", name))
		default:
			log.Warn("sor: unknown venue connector kind — skipped",
				"spec", tok)
		}
	}
	if len(venues) == 0 {
		return nil
	}
	th := sor.Thresholds{
		MinDepth:     decimal.RequireFromString(envOrLocal("EXC_SOR_MIN_DEPTH", "500000")),
		MaxSpreadBps: decimal.RequireFromString(envOrLocal("EXC_SOR_MAX_SPREAD_BPS", "15")),
	}
	var pub sor.FillPublisher
	if nc != nil {
		pub = func(ctx context.Context, ev *sor.FillBridgeEvent) error {
			payload, err := json.Marshal(ev)
			if err != nil {
				return err
			}
			_, err = nc.Publish(ctx, "settlements", 0,
				sor.FillBridgeSubjectToken, payload)
			return err
		}
	}
	return sor.NewRouter(sor.NewPgShadowStore(sor.PgxExecer{Pool: pool}),
		venues, pub, th)
}

func envOrLocal(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

// liqShard returns the order's engine shard for the margin-events
// subject (nil ShardID → 0 — pre-shard rows route to the default token).
func liqShard(o *orders.Order) int {
	if o != nil && o.ShardID != nil {
		return *o.ShardID
	}
	return 0
}

// ---- Phase-3 Task 5 — ClickHouse-backed risk sources ---------------------

// chReturnSeries adapts the daily OHLCV projection to
// risk.ReturnSeriesSource: close-to-close fractional returns,
// oldest→newest. ClickHouse down ⇒ error (Refresh fails, persisted
// matrix keeps serving inside its TTL).
type chReturnSeries struct{ store *analytics.OHLCVStore }

func (s chReturnSeries) DailyReturns(ctx context.Context, symbol string,
	days int) ([]float64, error) {
	rows, err := s.store.Query(ctx, analytics.OHLCVQuery{
		Symbol:   symbol,
		Interval: "1D",
		From:     time.Now().UTC().AddDate(0, 0, -(days + 4)),
		Limit:    days + 4,
	})
	if err != nil {
		return nil, err
	}
	if len(rows) < 2 {
		return nil, nil
	}
	out := make([]float64, 0, len(rows)-1)
	for i := 1; i < len(rows); i++ {
		prev := rows[i-1].Close
		if !prev.IsPositive() {
			continue
		}
		out = append(out, rows[i].Close.Div(prev).InexactFloat64()-1)
	}
	return out, nil
}

// chFlashCrash replays historical worst-days as FLASH_CRASH_* scenarios
// (risk.FlashCrashProvider): each instrument's worst 1-day
// close-to-close drawdown and rally over the lookback become two
// replayable symbol-scoped scenarios.
type chFlashCrash struct {
	store    *analytics.OHLCVStore
	resolver marketdata.InstrumentResolver
	lookback int // days
}

func (p chFlashCrash) Scenarios(ctx context.Context) ([]risk.StressScenario, error) {
	// Enumeration needs the whole id→symbol map; the live resolver is the
	// hot-swappable *atomicResolver wrapper, so a bare MapResolver
	// assertion would always fail and silently yield zero scenarios.
	var mr marketdata.MapResolver
	switch r := p.resolver.(type) {
	case marketdata.MapResolver:
		mr = r
	case *atomicResolver:
		mr = r.mapNow()
	}
	if len(mr) == 0 {
		return nil, nil
	}
	lb := p.lookback
	if lb <= 0 {
		lb = 365
	}
	syms := make([]string, 0, len(mr))
	for _, s := range mr {
		syms = append(syms, s)
	}
	sort.Strings(syms)
	var out []risk.StressScenario
	for _, sym := range syms {
		rows, err := p.store.Query(ctx, analytics.OHLCVQuery{
			Symbol:   sym,
			Interval: "1D",
			From:     time.Now().UTC().AddDate(0, 0, -lb),
			Limit:    lb,
		})
		if err != nil {
			return nil, fmt.Errorf("flash-crash replay %s: %w", sym, err)
		}
		var worst, best decimal.Decimal
		var worstDay, bestDay time.Time
		for i := 1; i < len(rows); i++ {
			prev := rows[i-1].Close
			if !prev.IsPositive() {
				continue
			}
			ret := rows[i].Close.Div(prev).Sub(decimal.One)
			if ret.LessThan(worst) {
				worst, worstDay = ret, rows[i].OpenTime
			}
			if ret.GreaterThan(best) {
				best, bestDay = ret, rows[i].OpenTime
			}
		}
		tok := strings.NewReplacer("/", "", " ", "").Replace(sym)
		if !worst.IsZero() {
			out = append(out, risk.StressScenario{
				Name:         fmt.Sprintf("FLASH_CRASH_%s_%s", tok, worstDay.Format("20060102")),
				Kind:         risk.ScenarioKindFlashCrash,
				SymbolShifts: map[string]decimal.Decimal{sym: worst},
				Description: fmt.Sprintf("replay of %s worst daily drawdown %s%% (%s)",
					sym, worst.Mul(decimal.NewFromInt(100)).String(),
					worstDay.Format("2006-01-02")),
			})
		}
		if !best.IsZero() {
			out = append(out, risk.StressScenario{
				Name:         fmt.Sprintf("FLASH_SPIKE_%s_%s", tok, bestDay.Format("20060102")),
				Kind:         risk.ScenarioKindFlashCrash,
				SymbolShifts: map[string]decimal.Decimal{sym: best},
				Description: fmt.Sprintf("replay of %s largest daily rally +%s%% (%s)",
					sym, best.Mul(decimal.NewFromInt(100)).String(),
					bestDay.Format("2006-01-02")),
			})
		}
	}
	return out, nil
}

// activeSymbols returns the symbol universe for the correlation refresh
// — active instruments only; a halted instrument's stale closes poison
// the window.
func activeSymbols(ctx context.Context, pool *pgxpool.Pool) ([]string, error) {
	rows, err := pool.Query(ctx,
		`SELECT symbol FROM instruments WHERE status='ACTIVE' ORDER BY symbol`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}
