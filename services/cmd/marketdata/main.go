// Command marketdata is the market-data WebSocket distribution service
// (Phase-06 Tasks 6.3.1/6.3.2/6.3.9/6.3.10, spec §10).
//
// Boot sequence: load shared config, init slog, connect the coordination
// Redis (seq mirror + WS dedup window), build the DeltaSource for engine
// book deltas, construct marketdata.Server, mount /ws/v1/marketdata (and
// /ws/v1/orders — the private surface shares the connection machinery),
// bind marketdata.addr, run until SIGINT/SIGTERM with graceful shutdown.
//
// Delta-source selection (env, all optional — the WS surface stays up
// fail-loud even with no feed):
//
//	EXC_MARKETDATA_SOURCE        "ipc" (default) | "jetstream" | "none"
//	EXC_MARKETDATA_IPC_BASE      shm ring base   (default "exchange_ipc")
//	EXC_MARKETDATA_IPC_SHARDS    shard list      (default "0")
//	EXC_MARKETDATA_JS_STREAM     JetStream stream  (default "analytics")
//	EXC_MARKETDATA_JS_FILTER     subject filter  (default "analytics.*.*")
//	EXC_MARKETDATA_INSTRUMENTS   "id:SYM,id:SYM" instrument map (required
//	                             for wire decode — e.g. "3:EUR/USD,7:GBP/USD")
//
// Stream-producer seams for Wave 2 (trades/bbo/aggTrades/kline/...):
// producers call server.Publish(channel, seq, data); snapshot fallback is
// registered per channel type via server.SetSnapshotSource; in-band data
// requests via server.RegisterMethod.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"exchange/internal/api"
	"exchange/internal/auth"
	"exchange/internal/config"
	"exchange/internal/db"
	"exchange/internal/ipc"
	"exchange/internal/marketdata"
	excnats "exchange/internal/nats"
	"exchange/internal/observability"
	excredis "exchange/internal/redis"
	"exchange/internal/utils"
	"exchange/internal/ws"
	"exchange/pkg/logging"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "marketdata: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	level, _ := logging.ParseLevel(cfg.Logging.Level)
	log, err := logging.New(level, cfg.Logging.Format)
	if err != nil {
		return err
	}

	ctx, stop := utils.SignalContext()
	defer stop()

	// Coordination Redis — md:seq cursor mirror + the shared WS dedup
	// window (Task 6.3.22 item 2: same 60s window as the gateway).
	rdb := excredis.New(cfg.Redis.Addr, cfg.Redis.Password, cfg.Redis.DB)
	defer func() { _ = rdb.Close() }()
	pingCtx, pingCancel := context.WithTimeout(ctx, 5*time.Second)
	pingErr := rdb.Ping(pingCtx)
	pingCancel()
	if pingErr != nil {
		// Fail-loud but not fatal: the WS surface must still come up —
		// seq mirror falls back to in-memory cursors and dedup to the
		// in-memory store; both log loudly. Redis flapping is an L1
		// degradation signal, not a boot blocker (spec §2.7).
		log.Error("marketdata: redis ping failed — seq/dedup running in-memory",
			"addr", cfg.Redis.Addr, "err", pingErr)
	}

	var seqStore marketdata.SeqStore = marketdata.NewMemSeqStore()
	var dedup ws.DedupStore = ws.NewMemDedupStore()
	var journal marketdata.GapJournal = marketdata.NewMemGapJournal()
	if pingErr == nil {
		seqStore = marketdata.NewRedisSeqStore(rdb.Client)
		dedup = ws.NewRedisDedupStore(rdb.Client)
		// Task 6.3.22 item 1: durable bounded gap journal — survives
		// restarts so seq discontinuities are auditable after reboot.
		journal = marketdata.NewRedisGapJournal(rdb.Client)
	}

	// WS auth seams for the /ws/v1/orders private surface (Task 6.3.5):
	// JWT verify via the shared kid keyring (EXC_JWT_HS256_KEY_B64,
	// same env as the gateway); API-key verify via KeyStore+SecretBox
	// when Postgres is reachable — either path absent fails CLOSED for
	// that auth type (UNAUTHORIZED), never open.
	jwtIssuer := auth.NewIssuer("exc.local", "exc-api", 0)
	if raw := os.Getenv("EXC_JWT_HS256_KEY_B64"); raw != "" {
		kb, derr := base64.StdEncoding.DecodeString(raw)
		if derr != nil || jwtIssuer.AddHMACKey("v1", kb, true) != nil {
			return fmt.Errorf("EXC_JWT_HS256_KEY_B64 invalid")
		}
	} else {
		log.Warn("no JWT key material — private WS JWT auth fails closed")
	}
	var sigVerifier *auth.SignatureVerifier
	// Phase-23 Task 23.3.3 premium-feed gate — bound unconditionally:
	// a missing subscription store denies every premium bind (fail-
	// closed), never widens access on an infra miss. Public channels
	// pass through ungated by design (spec §10.7).
	entitlements := &marketdata.FeedEntitlements{}
	if pool, perr := db.NewPool(ctx, cfg.Postgres.DSN, cfg.Postgres.MaxConns); perr == nil {
		defer pool.Close()
		entitlements.Store = marketdata.NewPgxFeedSubscriptionStore(pool)
		dataKey, derr := config.DecodeDataKey(cfg.Secrets.DataKey)
		if derr != nil {
			dev := sha256.Sum256([]byte("exc.local non-production secret-box data key v1"))
			dataKey = dev[:]
		}
		if box, berr := auth.NewSecretBox(dataKey); berr == nil {
			if ks, kerr := auth.NewKeyStore(pool, box); kerr == nil {
				if v, verr := auth.NewSignatureVerifier(ks,
					auth.NewRedisReplayGuard(rdb)); verr == nil {
					sigVerifier = v
				} else {
					log.Error("marketdata: signature verifier build failed — api-key auth fails closed",
						"err", verr)
				}
			}
		}
	} else {
		log.Warn("marketdata: postgres unreachable — api-key auth fails closed", "err", perr)
	}

	srv := marketdata.NewServer(marketdata.Config{
		Logger:       log,
		Issuer:       jwtIssuer,
		Verifier:     sigVerifier,
		Dedup:        dedup,
		Journal:      journal,
		Entitlements: entitlements,
		TrustProxy:   cfg.Environment != "production" && os.Getenv("EXC_TRUST_PROXY") == "1",
	})

	// Task 6.3.5: the private order stream — order-lifecycle producers
	// (bridge feedback / dispatcher outcomes) call orderStream.Publish,
	// which sequences per (channel, account) on md:seq:private:* and
	// fans out strictly to the bound account via PublishPrivate.
	orderStream := marketdata.NewPrivateOrderStream(srv, seqStore, journal, log)
	_ = orderStream // producer wave wires the event source; seam is live

	// L2 conflation pipeline. Wire the engine delta source first —
	// fail-loud: a missing/broken feed never fabricates book data, the
	// conflator just sees no deltas (Push stays available for embedders).
	src, srcErr := buildDeltaSource(ctx, cfg, log)
	if srcErr != nil {
		log.Error("marketdata: delta source unavailable — L2 feed idle", "err", srcErr)
	}

	// ===== WAVE-2 ADDITIVE BLOCK (Tasks 6.3.3/6.3.4/6.3.11–13/6.3.20/6.3.23) =====
	// BBO (Task 6.3.11) needs the RAW pre-conflation delta stream. The
	// _out ring is SPSC — a second consumer cannot attach — so the tap is
	// a TeeDeltaSource mirror installed here (see producers.go).
	bboTap := make(chan marketdata.BookDelta, 8192)
	if src != nil {
		src = marketdata.TeeDeltaSource(src, bboTap)
	}
	// ===== END WAVE-2 ADDITIVE BLOCK =====

	conflator := marketdata.NewConflator(marketdata.ConflatorConfig{
		Logger: log,
		// Task 6.3.15: multiplex subscribed depth@{sym}:{lvl}:{cad}
		// variants off the internal 20-level state.
		VariantSource: srv.ActiveDepthVariants,
		// Task 6.3.22: durable gap journal for input drops / restarts.
		Journal: journal,
	}, src, seqStore, srv.Publish, srv.Metrics())
	srv.SetSnapshotSource("book", conflator)
	srv.SetSnapshotSource("depth", conflator)

	// Task 6.3.17: referencePrice@{symbol} — the reference source is the
	// Phase-19.5 Price Oracle seam. No concrete oracle client exists in
	// this service's tree yet; when one is wired (ReferencePriceSource),
	// construct RefPriceStream here and register it as the
	// referencePrice snapshot source. Until then the channel parses and
	// subscribes cleanly but carries no fabricated data (fail-closed).
	go func() {
		if err := conflator.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			log.Error("marketdata: conflator exited", "err", err)
		}
	}()

	// ===== WAVE-2 ADDITIVE BLOCK — producer startup (producers.go) =====
	sp := startStreamProducers(ctx, cfg, srv, bboTap, rdb.Client, log)
	_ = sp // retained for future /healthz producer-depth reporting
	// ===== END WAVE-2 ADDITIVE BLOCK =====

	// HTTP surface: WS endpoints + health/readiness + Prometheus.
	//
	// Task 7.3.4: pull-metrics read internal/marketdata's exported atomics
	// directly — the package's Metrics conventions are reused verbatim (no
	// second counter set, no internal/marketdata changes).
	reg := observability.New()
	met := observability.NewMetrics(reg, "marketdata")
	mdm := srv.Metrics()
	reg.GaugeFunc("ws_connections_active",
		"Currently open marketdata WS connections.",
		func() float64 { c, _, _ := srv.Stats(); return float64(c) })
	reg.GaugeFunc("ws_connections_authed",
		"Authenticated marketdata WS connections.",
		func() float64 { _, a, _ := srv.Stats(); return float64(a) })
	reg.GaugeFunc("ws_subscriptions_active",
		"Active channel subscriptions across all connections.",
		func() float64 { _, _, s := srv.Stats(); return float64(s) })
	reg.GaugeFunc("ws_push_latency_seconds",
		"Marketdata send-side push latency (reservoir snapshot).",
		func() float64 {
			_, mean, _, _, _ := mdm.LatencySnapshot()
			return mean.Seconds()
		}, "quantile", "mean")
	reg.GaugeFunc("ws_push_latency_seconds",
		"Marketdata send-side push latency (reservoir snapshot).",
		func() float64 {
			_, _, p50, _, _ := mdm.LatencySnapshot()
			return p50.Seconds()
		}, "quantile", "0.5")
	reg.GaugeFunc("ws_push_latency_seconds",
		"Marketdata send-side push latency (reservoir snapshot).",
		func() float64 {
			_, _, _, p99, _ := mdm.LatencySnapshot()
			return p99.Seconds()
		}, "quantile", "0.99")
	reg.CounterFunc("marketdata_conns_opened_total",
		"WS connections opened since boot.",
		func() float64 { return float64(mdm.ConnsOpened.Load()) })
	reg.CounterFunc("marketdata_frames_published_total",
		"Frames enqueued to subscriber queues.",
		func() float64 { return float64(mdm.FramesPublished.Load()) })
	reg.CounterFunc("marketdata_frames_dropped_total",
		"Frames dropped on slow-consumer saturation (Task 6.3.21).",
		func() float64 { return float64(mdm.FramesDropped.Load()) })
	reg.CounterFunc("marketdata_deltas_received_total",
		"Engine book deltas consumed by the conflator.",
		func() float64 { return float64(mdm.DeltasReceived.Load()) })
	reg.CounterFunc("marketdata_deltas_dropped_total",
		"Input-queue saturation drops (fail-loud gap).",
		func() float64 { return float64(mdm.DeltasDropped.Load()) })
	reg.CounterFunc("marketdata_conflation_emits_total",
		"Conflated L2 frames emitted downstream.",
		func() float64 { return float64(mdm.ConflationEmits.Load()) })
	reg.CounterFunc("marketdata_resync_directives_total",
		"Resync frames emitted for gap repair.",
		func() float64 { return float64(mdm.ResyncDirectives.Load()) })
	reg.CounterFunc("marketdata_entitlement_rejections_total",
		"ENTITLEMENT_REQUIRED subscription rejections (§10.7).",
		func() float64 { return float64(mdm.EntitlementRejections.Load()) })
	reg.VecFunc("marketdata_frames_by_tier_total",
		"Frames delivered per entitlement tier (Task 6.3.22).", "counter",
		func() []observability.PullSample {
			byTier := mdm.FramesByTier()
			out := make([]observability.PullSample, 0, len(byTier))
			for tier, n := range byTier {
				out = append(out, observability.PullSample{
					Labels: []string{"tier", tier}, Value: float64(n)})
			}
			return out
		})

	mux := http.NewServeMux()
	mux.Handle("/metrics", reg.Handler())
	mux.Handle("/ws/v1/marketdata", srv)
	mux.Handle("/ws/v1/orders", srv) // private surface shares conn machinery
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		conns, authed, subs := srv.Stats()
		_, mean, _, p99, _ := srv.Metrics().LatencySnapshot()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"status":"ok","conns":%d,"authed":%d,"subscriptions":%d,`+
			`"push_mean_ns":%d,"push_p99_ns":%d}`,
			conns, authed, subs, mean.Nanoseconds(), p99.Nanoseconds())
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if err := rdb.Ping(ctx); err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprintf(w, `{"status":"degraded","redis":%q}`, err.Error())
			return
		}
		fmt.Fprint(w, `{"status":"ready"}`)
	})
	// R9 probe surface (Task 7.3.6) — deploy/k8s/marketdata-service
	// probes /health/live + /health/ready on the ws port.
	mux.HandleFunc("/health/live", api.Health)
	mux.HandleFunc("/health/ready", api.HealthReady(nil, []api.Dependency{
		{Name: "redis", Required: true, Probe: func(ctx context.Context) error {
			return api.DependencyErr("redis", rdb.Ping(ctx))
		}},
	}, nil, ""))

	httpSrv := &http.Server{
		Addr: cfg.MarketData.Addr(),
		// Task 7.3.4: instrument request counts/latency/status for the
		// WS-upgrade + health routes (long-lived WS conns report once at
		// close — connection counts come from the gauges above).
		Handler:           met.HTTPMiddleware(mux),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		log.Info("marketdata listening", "addr", httpSrv.Addr,
			"env", cfg.Environment)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("marketdata: listener died", "err", err)
			stop()
		}
	}()

	<-ctx.Done()
	log.Info("marketdata shutting down")
	// Task 9.3.23: reconnect advisory → 5s voluntary-leave window → force
	// close, then HTTP drain under the 30s contract. Liveness stays ok
	// throughout; readiness (K8s) was already dropped by the LB draining
	// hook upstream.
	drainCtx, drainCancel := context.WithTimeout(context.Background(), 15*time.Second)
	if err := srv.Drain(drainCtx, "maintenance", 5*time.Second); err != nil {
		log.Warn("marketdata ws drain incomplete", "err", err)
	}
	drainCancel()
	shCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return httpSrv.Shutdown(shCtx)
}

// buildDeltaSource selects the engine book-delta feed. "ipc" attaches to
// each configured shard's {base}_{shard}_out ring (gateway role); a ring
// that will not attach is logged and skipped — if every ring fails the
// JetStream fallback is tried when a stream is configured. "none" leaves
// the feed unwired on purpose (dry-run/deploy-before-engine).
func buildDeltaSource(ctx context.Context, cfg *config.Config, log *slog.Logger) (marketdata.DeltaSource, error) {
	res := instrumentResolver()
	source := strings.ToLower(strings.TrimSpace(envOr("EXC_MARKETDATA_SOURCE", "ipc")))
	switch source {
	case "none":
		return nil, nil
	case "jetstream":
		return jetstreamSource(ctx, cfg, res, log)
	default:
		return ipcSource(res, log)
	}
}

// ipcSource attaches the shared-memory out rings for every configured
// shard (EndpointGateway consumes _out). Fan-in merges shards into one
// delta stream — conflation keys on symbol so shard interleaving is
// safe (each symbol lives on exactly one shard by the §5.2 router).
func ipcSource(res marketdata.InstrumentResolver, log *slog.Logger) (marketdata.DeltaSource, error) {
	base := envOr("EXC_MARKETDATA_IPC_BASE", ipc.DefaultShmBase)
	shards, err := shardList(envOr("EXC_MARKETDATA_IPC_SHARDS", "0"))
	if err != nil {
		return nil, err
	}
	sources := make([]marketdata.DeltaSource, 0, len(shards))
	var firstErr error
	for _, sh := range shards {
		// Attach passes the default geometry — OpenRing ignores it on a
		// pre-existing ring (the shared header's capacity/slot_payload are
		// authoritative); passing 0 trips the power-of-two precheck before
		// the header is ever read.
		ch, err := ipc.OpenChannel(base, sh, ipc.EndpointGateway, false,
			ipc.DefaultRingCapacity, ipc.DefaultRingSlotPayload)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			log.Error("marketdata: ipc ring attach failed",
				"base", base, "shard", sh, "err", err)
			continue
		}
		log.Info("marketdata: ipc delta source attached",
			"ring", ipc.OutName(base, sh))
		sources = append(sources, marketdata.IPCDeltaSource(ch, res, log))
	}
	if len(sources) == 0 {
		return nil, fmt.Errorf("no ipc rings attached: %w", firstErr)
	}
	return marketdata.FanInDeltaSource(sources...), nil
}

// jetstreamSource consumes the bridge's republished engine stream —
// the fallback path when the process is not colocated with the engine.
func jetstreamSource(ctx context.Context, cfg *config.Config,
	res marketdata.InstrumentResolver, log *slog.Logger) (marketdata.DeltaSource, error) {
	urls := cfg.NATS.URLList()
	if len(urls) == 0 {
		return nil, errors.New("nats urls not configured")
	}
	nc, err := excnats.Connect(ctx, excnats.DefaultConfig(urls), log)
	if err != nil {
		return nil, err
	}
	stream := envOr("EXC_MARKETDATA_JS_STREAM", "analytics")
	filter := envOr("EXC_MARKETDATA_JS_FILTER", "analytics.*.*")
	return marketdata.JetStreamDeltaSource(nc, stream, "marketdata-l2", filter, res, log), nil
}

// instrumentResolver parses EXC_MARKETDATA_INSTRUMENTS ("3:EUR/USD,...")
// into the wire instrument_id → display-symbol map the decoder needs.
func instrumentResolver() marketdata.InstrumentResolver {
	m := marketdata.MapResolver{}
	for _, pair := range strings.Split(os.Getenv("EXC_MARKETDATA_INSTRUMENTS"), ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		idStr, sym, ok := strings.Cut(pair, ":")
		if !ok {
			continue
		}
		id, err := strconv.ParseUint(strings.TrimSpace(idStr), 10, 32)
		if err != nil || sym == "" {
			continue
		}
		m[uint32(id)] = strings.TrimSpace(sym)
	}
	return m
}

func shardList(s string) ([]uint16, error) {
	var out []uint16
	for _, tok := range strings.Split(s, ",") {
		tok = strings.TrimSpace(tok)
		if tok == "" {
			continue
		}
		v, err := strconv.ParseUint(tok, 10, 16)
		if err != nil {
			return nil, fmt.Errorf("invalid shard %q", tok)
		}
		out = append(out, uint16(v))
	}
	if len(out) == 0 {
		return nil, errors.New("no shards configured")
	}
	return out, nil
}

func envOr(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}
