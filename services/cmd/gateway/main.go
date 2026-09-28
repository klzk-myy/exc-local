// Command gateway is the REST API entrypoint (order-gateway).
//
// Phase-05 wiring (Tasks 5.3.7/5.3.21/5.3.41): the route registry mounts
// every declared endpoint (stubbed 501 until its owning phase lands), the
// error-code registry gates emission, and all responses carry the RFC 7807
// envelope + X-Request-ID correlation.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"exchange/internal/accounts"
	"exchange/internal/api"
	"exchange/internal/auth"
	"exchange/internal/config"
	"exchange/internal/db"
	"exchange/internal/deprecation"
	"exchange/internal/errs"
	"exchange/internal/funding"
	"exchange/internal/gateway"
	"exchange/internal/marketapi"
	"exchange/internal/middleware"
	"exchange/internal/nats"
	"exchange/internal/orders"
	"exchange/internal/promos"
	"exchange/internal/ratelimit"
	"exchange/internal/redis"
	"exchange/internal/risk"
	"exchange/internal/settlement"
	"exchange/internal/tax"
	"exchange/internal/testenv"
	"exchange/internal/timesync"
	"exchange/internal/utils"
	"exchange/internal/webhooks"
	"exchange/internal/ws"
	"exchange/pkg/logging"
)

// apiVersion is the semantic API build emitted in X-API-Version and the
// health schema (Task 5.3.28/R9); overridable via -ldflags
// "-X main.apiVersion=1.2.3".
var apiVersion = "1.0.0"

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "gateway: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	level, _ := logging.ParseLevel(cfg.Logging.Level) // validated by cfg.Validate
	log, err := logging.New(level, cfg.Logging.Format)
	if err != nil {
		return err
	}

	// Task 1.3.7: every service resolves symbols → shard ids. Read the
	// shard:map HASH at startup (written by `exchange cache-shard-map`),
	// falling back to config/sharding.yaml on cache miss/Redis outage;
	// both failing is fatal — an unroutable order gateway must not start.
	shardCtx, shardCancel := context.WithTimeout(context.Background(), 5*time.Second)
	rdb := redis.New(cfg.Redis.Addr, cfg.Redis.Password, cfg.Redis.DB)
	shardMap, shardSrc, err := config.LoadShardMapForService(shardCtx, rdb)
	shardCancel()
	if err != nil {
		_ = rdb.Close()
		return fmt.Errorf("shard map: %w", err)
	}
	defer func() { _ = rdb.Close() }() // Phase-02+ keeps this client for routing/health use
	log.Info("shard map loaded",
		"source", shardSrc.String(),
		"static_symbols", len(shardMap.Entries()),
		"elastic_base", shardMap.ElasticBase(),
		"elastic_count", shardMap.ElasticCount())

	mux := http.NewServeMux()
	mux.HandleFunc("/health", api.Health)

	// Tasks 5.3.2/5.3.27/5.3.34: edge rate-limit + progressive-IP-ban
	// cluster. One Lua round trip per request atomically runs the ban
	// gate → token bucket → weighted counters (internal/ratelimit); a
	// Redis/Sentinel outage fails over to the in-memory backend (spec
	// §4.1) and a total limiter outage is fail-closed 503.
	limiter := ratelimit.NewLimiter(ratelimit.NewRedisBackend(rdb),
		ratelimit.LimiterOptions{Mode: rdb})
	banAdmin := &ratelimit.BanAdmin{B: limiter.Backend()}

	// Task 5.3.40 introspection + tier resolution read PG (accounts →
	// fee_tiers, commission engine, instruments). The pool connects
	// lazily — gateway readiness reports via /health/ready.
	poolCtx, poolCancel := context.WithTimeout(context.Background(), 5*time.Second)
	pool, err := db.NewPool(poolCtx, cfg.Postgres.DSN, cfg.Postgres.MaxConns)
	poolCancel()
	if err != nil {
		return fmt.Errorf("postgres: %w", err)
	}
	defer pool.Close()
	tierResolver := api.PgTierResolver(pool)

	// --- Phase-05 Wave-2 Cluster B (Tasks 5.3.4/5.3.6/5.3.18/5.3.23/5.3.45):
	// account & funding domain wiring. Every wallet mutation flows through
	// settlement.LedgerService.Post (SERIALIZABLE + per-account Redis locks
	// + idempotent journals) — nothing writes balances directly.
	var ledgerPub settlement.Publisher
	var natsClient *nats.Client
	natsCtx, natsCancel := context.WithTimeout(context.Background(), 3*time.Second)
	if nc, nerr := nats.Connect(natsCtx, nats.DefaultConfig(cfg.NATS.URLList()), log); nerr == nil {
		ledgerPub = settlement.NatsPublisher{JS: nc.JetStream()}
		natsClient = nc
		defer nc.Close()
	} else {
		// Fail-operational: journals still commit; a failed BalanceChanged
		// dispatch surfaces as dispatch_pending on the result + an ops
		// alert — never a repost or money loss (spec §5.3/§2.7).
		log.Warn("nats unavailable — ledger balance-event dispatch degraded", "err", nerr)
	}
	natsCancel()

	ledgerSvc, err := settlement.NewLedgerService(pool, rdb, ledgerPub)
	if err != nil {
		return fmt.Errorf("ledger service: %w", err)
	}
	fundStore := funding.NewPgStore(pool)
	// nil RoleResolver → privileged freeze/unfreeze calls reject
	// UNAUTHORIZED_ROLE until Phase-07 wires RBAC (accounts seam); read +
	// AssertMutable paths are unaffected.
	freezeSvc := accounts.NewFreezeService(pool, nil)
	riskLimits := risk.NewLimitsService(risk.NewPgStore(pool), nil, nil)
	if err := riskLimits.Load(context.Background()); err != nil {
		// Do not abort boot — the limits view fails closed per request and
		// the refresher retries (missing/empty table resolves to the §13.6
		// defaults anyway).
		log.Warn("risk limits initial load failed", "err", err)
	}
	riskLimits.StartRefresher(context.Background(), 60*time.Second,
		func(e error) { log.Warn("risk limits refresh failed", "err", e) })
	usdConv := &settlement.RedisUsdConverter{Rdb: rdb}
	var opsAlerter funding.OpsAlerter
	if ledgerPub != nil {
		opsAlerter = settlement.PublisherAlerter{Pub: ledgerPub}
	}
	withdrawalSvc, err := funding.NewWithdrawalService(fundStore, ledgerSvc, freezeSvc)
	if err != nil {
		return fmt.Errorf("withdrawal service: %w", err)
	}
	withdrawalSvc.WithLimits(riskLimits).WithUSDConverter(usdConv).
		WithAlerter(opsAlerter).
		WithLogger(func(f string, a ...any) { log.Error(fmt.Sprintf(f, a...)) })
	transferSvc, err := funding.NewTransferService(fundStore, ledgerSvc, freezeSvc)
	if err != nil {
		return fmt.Errorf("transfer service: %w", err)
	}
	chargebackSvc, err := funding.NewChargebackService(fundStore, freezeSvc)
	if err != nil {
		return fmt.Errorf("chargeback service: %w", err)
	}
	historySvc := funding.NewHistoryService(fundStore)
	// Expiry sweeper: lapses pending withdrawal confirmations past the
	// canonical 15-minute window → AUTO_CANCELLED + ledger hold release.
	sweepCtx, sweepStop := context.WithCancel(context.Background())
	defer sweepStop()
	go func() {
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-sweepCtx.Done():
				return
			case <-t.C:
				if n, err := withdrawalSvc.SweepExpired(sweepCtx, 100); err != nil {
					log.Warn("withdrawal sweep failed", "err", err)
				} else if n > 0 {
					log.Info("withdrawal confirmations auto-cancelled", "count", n)
				}
			}
		}
	}()
	// --- end Cluster B wiring ---

	// ---- Phase-05 Wave-3 platform surface: Tasks 5.3.8 (OpenAPI),
	//      5.3.13 (test env), 5.3.15 (fee promos), 5.3.16 (developer
	//      portal), 5.3.17 (webhooks), 5.3.19 (tax), 5.3.20 (deprecation
	//      policy) ----

	// Secret material: one AES-256-GCM data key backs auth.SecretBox,
	// which seals api_keys.secret_enc (Task 5.3.16) and
	// webhook_endpoints.secret_enc (Task 5.3.17). Production requires
	// secrets.data_key — config.Validate already fails closed without it;
	// non-production falls back to a deterministic dev key so the scratch
	// environment boots (a key change orphans sealed rows — documented).
	dataKey, kerr := config.DecodeDataKey(cfg.Secrets.DataKey)
	if kerr != nil {
		// Non-production only (production never reaches here — Validate
		// rejects an unset/malformed key first).
		dev := sha256.Sum256([]byte("exc.local non-production secret-box data key v1"))
		dataKey = dev[:]
		log.Warn("secrets.data_key unset — non-production deterministic dev key in use")
	}
	secretBox, err := auth.NewSecretBox(dataKey)
	if err != nil {
		return fmt.Errorf("secret box: %w", err)
	}
	keyStore, err := auth.NewKeyStore(pool, secretBox)
	if err != nil {
		return fmt.Errorf("key store: %w", err)
	}
	webhookStore, err := webhooks.NewStore(pool, secretBox)
	if err != nil {
		return fmt.Errorf("webhook store: %w", err)
	}
	testSvc := testenv.New(pool, cfg.Environment)
	promoStore, err := promos.NewStore(pool)
	if err != nil {
		return fmt.Errorf("promo store: %w", err)
	}
	taxSvc, err := tax.NewService(tax.NewPgxSource(pool))
	if err != nil {
		return fmt.Errorf("tax service: %w", err)
	}
	depStore, err := deprecation.NewStore(pool)
	if err != nil {
		return fmt.Errorf("deprecation store: %w", err)
	}
	// 5s TTL cache — the middleware resolves rules per request.
	depRules := deprecation.CachedRules(depStore, 5*time.Second)

	// Webhook dispatcher: drains due PENDING deliveries every 500ms and
	// POSTs them HMAC-signed on the 1s→2s→4s→8s→16s retry ladder (5
	// attempts → DEAD_LETTERED). Shares the sweepCtx lifecycle.
	whDispatch := webhooks.NewDispatcher(webhookStore, nil, 500*time.Millisecond)
	go whDispatch.Run(sweepCtx)

	// Promo-window expiry sweeper: lapses APPLIED windows whose ends_at
	// passed (clears the fee_tiers promo_* columns they set) and expires
	// stale PENDING_APPROVAL rows past the 15-minute four-eyes window.
	go func() {
		t := time.NewTicker(60 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-sweepCtx.Done():
				return
			case <-t.C:
				if n, err := promoStore.ClearExpired(sweepCtx); err != nil {
					log.Warn("promo expiry sweep failed", "err", err)
				} else if n > 0 {
					log.Info("fee promo windows expired", "count", n)
				}
			}
		}
	}()
	// --- end Wave-3 wiring ---

	// Task 5.3.7/5.3.21: mount the route + error-code registries.
	// MountSeedLive wires the live handlers this binary hosts; an
	// unwired live row mounts a fail-closed 503, and any unregistered
	// emitted code fails startup — the registry contract is a startup
	// gate, not a runtime nicety.
	router := gateway.NewRouter(mux, errs.Default)
	banList, banGet, banPut, banDel, banAudit := api.AdminIPBans(limiter.Backend(), banAdmin)
	alPut, alDel := api.AdminIPAllowlist(banAdmin)

	// Wave-3 handler groups (constructed once, mounted via the registry).
	keyCreate, keyList, keyRevoke := api.DeveloperAPIKeys(keyStore)
	whRegister, whList, whDisable, whRotate, whDeliveries := api.WebhookHandlers(webhookStore)
	promoCreate, promoList, promoApprove, promoReject := api.AdminFeePromos(promoStore)
	depAnnounce, depList := api.AdminDeprecations(depStore)

	// Task 5.3.8: the full OpenAPI document replaces the 5.3.7 skeleton —
	// injected via the package-level hook (api imports gateway; the edge
	// cannot run the other way).
	gateway.OpenAPIDocFn = func(r *gateway.Router) map[string]any {
		return api.OpenAPIDocument(r.Routes(), errs.Default, apiVersion)
	}

	// ---- Phase-05 wave-2 cluster C: market data & venue surface ----
	// (Tasks 5.3.5 / 5.3.14 / 5.3.35 / 5.3.43 / 5.3.44).
	// marketapi.PgStore is the single PostgreSQL read model for
	// instruments, the persisted L2 book, the public trade tape, 24h
	// tickers, pre-materialized fx_klines candles, announcements and the
	// maintenance calendar — no prices are synthesized. The TTL cache
	// implements the spec §10.3 cache contract (book 100ms, market reads
	// 1s, instruments 1min). Server time reports the adjtimex(2) clock
	// state via internal/timesync (PTP daemon: Phase-09 Task 9.3.12).
	marketStore := marketapi.NewPgStore(pool)
	marketCache := marketapi.NewCache(nil)
	marketDeps := &api.MarketDeps{Store: marketStore, Book: marketStore, Cache: marketCache}
	venueDeps := &api.VenueDeps{Store: marketStore, Cache: marketCache}
	announceDeps := &api.AnnounceDeps{Announcements: marketStore, Maintenance: marketStore}

	// ---- Phase-05 wave-2 cluster E: WS surface, order pipeline, manual
	//      liquidation, API hardening (Tasks 5.3.26/29/30/31/42) ----

	// Order pipeline (Task 5.3.3 family): PgStore is the read/write
	// model, ShmSubmitter the engine-bound Aeron ring producer. A
	// missing engine image fails closed SERVICE_DEGRADED at dispatch.
	orderStore := orders.NewPgStore(pool)
	orderSubmitter := orders.NewShmSubmitter("") // ipc.DefaultShmBase rings
	orderSvc, err := orders.NewService(orders.Options{
		Store:     orderStore,
		Submitter: orderSubmitter,
		ShardMap:  shardMap,
		Limits:    riskLimits,
		BatchRL:   orders.NewRedisBatchLimiter(rdb.Client, 0),
	})
	if err != nil {
		return fmt.Errorf("order service: %w", err)
	}
	// Engine out-ring consumer: cancel echoes + trade fills update the PG
	// read model; without it pending confirms only time out.
	go orders.NewConsumer(orderSubmitter, orderStore, orderSvc.Pending()).
		Run(sweepCtx, shardIDs(shardMap))

	// accounts.OrderDispatcher ← orders.Service (dead-man sweeper,
	// Task 5.3.33, and close-all, Task 5.3.36, share it). The dispatcher
	// honors CloseOrderRequest.MaxSlippageBps by converting the market
	// close to a synthetic IOC limit at mark±bps (Task 2.3.15).
	orderDisp := orders.NewDispatcher(orderSvc)
	deadman := accounts.NewDeadManService(
		accounts.NewRedisCountdownStore(rdb.Client), orderDisp)
	go func() {
		if err := deadman.Run(sweepCtx, 250*time.Millisecond); err != nil {
			log.Warn("dead-man sweeper exited", "err", err)
		}
	}()

	// WS auth seams: API-key path via the HMAC/Ed25519/RSA verifier
	// (Task 5.3.38) + Redis replay guard; JWT verify via the kid keyring
	// (Task 5.3.1). JWT key material arrives from
	// EXC_JWT_HS256_KEY_B64 (dev/ops bootstrap) — absent keys fail closed
	// to UNAUTHORIZED on the JWT path until the Vault/KMS keyring wiring
	// lands (Phase-13.5 Task 13.5.3.5).
	sigVerifier, err := auth.NewSignatureVerifier(keyStore, auth.NewRedisReplayGuard(rdb))
	if err != nil {
		return fmt.Errorf("signature verifier: %w", err)
	}
	jwtIssuer := auth.NewIssuer("exc.local", "exc-api", 0)
	if raw := os.Getenv("EXC_JWT_HS256_KEY_B64"); raw != "" {
		kb, derr := base64.StdEncoding.DecodeString(raw)
		if derr != nil || jwtIssuer.AddHMACKey("v1", kb, true) != nil {
			return fmt.Errorf("EXC_JWT_HS256_KEY_B64 invalid")
		}
		log.Info("jwt verify keyring: 1 key (EXC_JWT_HS256_KEY_B64)")
	} else if cfg.IsProduction() {
		return fmt.Errorf("no JWT key material configured (EXC_JWT_HS256_KEY_B64)")
	} else {
		log.Warn("no JWT key material — /ws/v1 JWT authenticate fails closed (dev)")
	}

	// Task 5.3.26 + 5.3.31: the unified /ws/v1 endpoint. Trading actions
	// dispatch through ws.NewOrdersDispatcher → orders.Service — never a
	// direct engine call. Dedup is the Redis 60s request_id window
	// (Task 5.3.42); X-Forwarded-For is trusted because Task 5.3.29's
	// HAProxy front is the only supported client path (the listener is
	// not directly exposed in the reference deployment).
	wsSrv := ws.NewServer(ws.Config{
		Issuer:     jwtIssuer,
		Verifier:   sigVerifier,
		Dispatcher: ws.NewOrdersDispatcher(orderSvc, orderStore.AccountByID),
		Countdown:  countdownAdapter{svc: deadman},
		Dedup:      ws.NewRedisDedupStore(rdb.Client),
		Logger:     log,
		TrustProxy: true,
		TierResolver: func(ctx context.Context, sess *ws.Session) ratelimit.Tier {
			var name *string
			if err := pool.QueryRow(ctx, `
				SELECT t.tier_name FROM accounts a
				LEFT JOIN fee_tiers t ON t.id = a.fee_tier_id
				WHERE a.id = $1`, sess.AccountID).Scan(&name); err != nil || name == nil {
				return ratelimit.TierBasic
			}
			return ratelimit.ParseTier(*name)
		},
	})

	// Task 5.3.30: manual liquidation. Role resolution is the Phase-07
	// Task 7.3.1 seam — nil fails closed (UNAUTHORIZED_ROLE) until the
	// admin role store lands. The WAL event publishes to the
	// `margin-events` JetStream stream (Task 1.3.11) for the Phase-19
	// auction machinery; a nil sink (NATS down) fails the endpoint closed
	// SERVICE_DEGRADED.
	var liqSink api.LiquidationEventSink
	if natsClient != nil {
		liqSink = liquidationSink{nc: natsClient}
	}
	liqSvc := api.NewManualLiquidationService(pool, nil, liqSink)
	// --- end cluster E wiring ---

	// Wave-2 cluster A REST order surface (Tasks 5.3.3/5.3.22/5.3.24/
	// 5.3.25/5.3.32/5.3.37/5.3.39). The HMAC verifier built for the WS
	// API-key path is reused for the signed REST path; TrustProxy=true
	// because HAProxy (Task 5.3.29) is the only supported client path.
	orderDeps := &api.OrderDeps{
		SVC:        orderSvc,
		Verifier:   sigVerifier,
		TrustProxy: true,
		// RoleResolver: nil → admin audit/mass-cancel fail closed
		// UNAUTHORIZED_ROLE until Phase-07 lands the role store.
	}

	live := map[string]http.Handler{
		"GET /api/v1/account/rate-limits": http.HandlerFunc(
			api.AccountRateLimits(limiter, tierResolver)),
		"GET /api/v1/account/filters/{symbol}": http.HandlerFunc(
			api.AccountFilters(api.NewPgInstrumentFilterSource(pool))),
		"GET /api/v1/account/commission/{symbol}": http.HandlerFunc(
			api.AccountCommission(
				settlement.NewPgCommissionStore(pool),
				settlement.NewPgProfileFeeModelSource(pool), nil)),
		"GET /api/v1/admin/ip-bans":              http.HandlerFunc(banList),
		"GET /api/v1/admin/ip-bans/audit":        http.HandlerFunc(banAudit),
		"GET /api/v1/admin/ip-bans/{ip}":         http.HandlerFunc(banGet),
		"PUT /api/v1/admin/ip-bans/{ip}":         http.HandlerFunc(banPut),
		"DELETE /api/v1/admin/ip-bans/{ip}":      http.HandlerFunc(banDel),
		"PUT /api/v1/admin/ip-allowlist/{ip}":    http.HandlerFunc(alPut),
		"DELETE /api/v1/admin/ip-allowlist/{ip}": http.HandlerFunc(alDel),
		// --- Phase-05 Wave-2 Cluster B live handlers ---
		"GET /api/v1/account/balances":    http.HandlerFunc(api.AccountBalances(fundStore)),
		"GET /api/v1/positions":           http.HandlerFunc(api.AccountPositions(fundStore)),
		"GET /api/v1/account/risk-limits": http.HandlerFunc(api.AccountRiskLimits(riskLimits, fundStore)),
		"GET /api/v1/deposits/{currency}": http.HandlerFunc(api.DepositInstructions(fundStore)),
		"POST /api/v1/withdrawals":        http.HandlerFunc(api.CreateWithdrawal(withdrawalSvc)),
		"POST /api/v1/withdrawals/{id}/confirm": http.HandlerFunc(
			api.ConfirmWithdrawal(withdrawalSvc)),
		"GET /api/v1/funding":                         http.HandlerFunc(api.FundingHistory(historySvc)),
		"POST /api/v1/transfers":                      http.HandlerFunc(api.CreateTransfer(transferSvc)),
		"GET /api/v1/transfers":                       http.HandlerFunc(api.TransferHistory(historySvc)),
		"POST /api/v1/admin/chargebacks":              http.HandlerFunc(api.AdminChargebackCreate(chargebackSvc)),
		"GET /api/v1/admin/chargebacks":               http.HandlerFunc(api.AdminChargebackList(chargebackSvc)),
		"GET /api/v1/admin/chargebacks/{id}":          http.HandlerFunc(api.AdminChargebackDetail(chargebackSvc)),
		"POST /api/v1/admin/chargebacks/{id}/submit":  http.HandlerFunc(api.AdminChargebackSubmit(chargebackSvc)),
		"POST /api/v1/admin/chargebacks/{id}/resolve": http.HandlerFunc(api.AdminChargebackResolve(chargebackSvc)),
		// --- end Cluster B handlers ---
		// Wave-2 cluster C — market data REST (Task 5.3.5).
		"GET /api/v1/book/{symbol}":   http.HandlerFunc(api.MarketBook(marketDeps)),
		"GET /api/v1/trades/{symbol}": http.HandlerFunc(api.MarketTrades(marketDeps)),
		"GET /api/v1/ticker/{symbol}": http.HandlerFunc(api.MarketTicker(marketDeps)),
		"GET /api/v1/klines/{symbol}": http.HandlerFunc(api.MarketKlines(marketDeps)),
		"GET /api/v1/instruments":     http.HandlerFunc(api.Instruments(marketDeps)),
		// Wave-2 cluster C — venue surface (Tasks 5.3.43/5.3.44).
		"GET /api/v1/time":          http.HandlerFunc(api.ServerTime(timesync.KernelSource, nil)),
		"GET /api/v1/exchange-info": http.HandlerFunc(api.ExchangeInfo(venueDeps)),
		// Wave-2 cluster C — announcements & maintenance (Task 5.3.14).
		"GET /api/v1/announcements":                     http.HandlerFunc(api.Announcements(announceDeps)),
		"GET /api/v1/announcements/{id}":                http.HandlerFunc(api.AnnouncementByID(announceDeps)),
		"POST /api/v1/admin/announcements":              http.HandlerFunc(api.CreateAnnouncement(announceDeps)),
		"GET /api/v1/admin/announcements":               http.HandlerFunc(api.AdminAnnouncements(announceDeps)),
		"PATCH /api/v1/admin/announcements/{id}":        http.HandlerFunc(api.UpdateAnnouncement(announceDeps)),
		"DELETE /api/v1/admin/announcements/{id}":       http.HandlerFunc(api.RetractAnnouncement(announceDeps)),
		"GET /api/v1/maintenance/schedule":              http.HandlerFunc(api.MaintenanceSchedule(announceDeps)),
		"GET /api/v1/admin/maintenance-windows":         http.HandlerFunc(api.AdminMaintenanceWindows(announceDeps)),
		"POST /api/v1/admin/maintenance-windows":        http.HandlerFunc(api.CreateMaintenance(announceDeps)),
		"PATCH /api/v1/admin/maintenance-windows/{id}":  http.HandlerFunc(api.UpdateMaintenance(announceDeps)),
		"DELETE /api/v1/admin/maintenance-windows/{id}": http.HandlerFunc(api.CancelMaintenance(announceDeps)),
		// --- Wave-3 platform surface ---
		// Task 5.3.13 test environment.
		"POST /api/v1/test/reset": http.HandlerFunc(api.TestReset(testSvc)),
		// Task 5.3.15 fees + governed promo windows.
		"GET /api/v1/fees":                           http.HandlerFunc(api.AccountFees(settlement.NewPgxFeeStore(pool))),
		"POST /api/v1/admin/fees/promo":              http.HandlerFunc(promoCreate),
		"GET /api/v1/admin/fees/promos":              http.HandlerFunc(promoList),
		"POST /api/v1/admin/fees/promo/{id}/approve": http.HandlerFunc(promoApprove),
		"POST /api/v1/admin/fees/promo/{id}/reject":  http.HandlerFunc(promoReject),
		// Task 5.3.16 developer portal (Swagger UI + API-key CRUD).
		"GET /developer":                         http.HandlerFunc(api.DeveloperPortal()),
		"POST /api/v1/developer/api-keys":        http.HandlerFunc(keyCreate),
		"GET /api/v1/developer/api-keys":         http.HandlerFunc(keyList),
		"DELETE /api/v1/developer/api-keys/{id}": http.HandlerFunc(keyRevoke),
		// Task 5.3.17 webhooks.
		"POST /api/v1/webhooks":                    http.HandlerFunc(whRegister),
		"GET /api/v1/webhooks":                     http.HandlerFunc(whList),
		"DELETE /api/v1/webhooks/{id}":             http.HandlerFunc(whDisable),
		"POST /api/v1/webhooks/{id}/rotate-secret": http.HandlerFunc(whRotate),
		"GET /api/v1/webhooks/{id}/deliveries":     http.HandlerFunc(whDeliveries),
		// Task 5.3.19 tax reporting (phase + canonical client path).
		"GET /api/v1/tax/report":         http.HandlerFunc(api.TaxReport(taxSvc)),
		"GET /api/v1/account/tax-report": http.HandlerFunc(api.TaxReport(taxSvc)),
		// Task 5.3.20 deprecation policy administration + guide.
		"POST /api/v1/admin/api-deprecations": http.HandlerFunc(depAnnounce),
		"GET /api/v1/admin/api-deprecations":  http.HandlerFunc(depList),
		"GET /developer/migration":            http.HandlerFunc(api.MigrationGuide(depStore)),
		// R9 health schema (Task 5.3.28 item 6): /health/live is always-ok
		// liveness; /health/ready reports mode/shards/version/timestamp.
		"GET /health/live": http.HandlerFunc(api.Health),
		"GET /health/ready": http.HandlerFunc(api.HealthFull(
			func(ctx context.Context) (string, error) {
				st, err := rdb.GetDegradationMode(ctx)
				return string(st.Mode), err
			}, nil, apiVersion)),
		// --- Phase-05 Wave-2 Cluster E live handlers ---
		// Tasks 5.3.26/5.3.31: unified WS endpoint.
		"WS /ws/v1": wsSrv,
		// Task 5.3.30: dual-control manual liquidation.
		"POST /api/v1/admin/liquidation/manual": api.ManualLiquidationHandler(liqSvc, true),
		// Task 5.3.42: published hardening contracts.
		"GET /api/v1/meta/rate-limits": http.HandlerFunc(api.RateLimitsMetaHandler),
		"GET /api/v1/meta/pagination":  http.HandlerFunc(api.ListMetaHandler),
		// --- end Cluster E handlers ---
		// --- Phase-05 Wave-2 Cluster A: order REST surface ---
		"POST /api/v1/orders":        http.HandlerFunc(api.OrderSubmit(orderDeps)),
		"PUT /api/v1/orders/{id}":    http.HandlerFunc(api.OrderModify(orderDeps)),
		"DELETE /api/v1/orders/{id}": http.HandlerFunc(api.OrderCancel(orderDeps)),
		"DELETE /api/v1/orders/all":  http.HandlerFunc(api.OrderCancelAll(orderDeps)),
		"DELETE /api/v1/orders":      http.HandlerFunc(api.OrderMassCancel(orderDeps)),
		"GET /api/v1/orders":         http.HandlerFunc(api.OrderList(orderDeps)),
		"GET /api/v1/orders/{id}":    http.HandlerFunc(api.OrderGet(orderDeps)),
		// Task 5.3.32 batch ops.
		"POST /api/v1/orders/batch":   http.HandlerFunc(api.OrderBatchSubmit(orderDeps)),
		"DELETE /api/v1/orders/batch": http.HandlerFunc(api.OrderBatchCancel(orderDeps)),
		// Task 5.3.37 atomic cancel-replace + keep-priority amend.
		"POST /api/v1/orders/{id}/cancel-replace": http.HandlerFunc(
			api.OrderCancelReplace(orderDeps)),
		"PUT /api/v1/orders/{id}/amend/keep-priority": http.HandlerFunc(
			api.OrderKeepPriority(orderDeps)),
		"GET /api/v1/orders/{id}/amendments": http.HandlerFunc(
			api.OrderAmendments(orderDeps)),
		// Task 5.3.39 dry-run preview.
		"POST /api/v1/orders/test": http.HandlerFunc(api.OrderTest(orderDeps)),
		// Task 5.3.22 order audit + Task 5.3.24/25 admin mass cancel.
		"GET /api/v1/admin/orders/{id}/audit":   http.HandlerFunc(api.AdminOrderAudit(orderDeps)),
		"POST /api/v1/admin/orders/mass-cancel": http.HandlerFunc(api.AdminMassCancel(orderDeps)),
		// --- end Cluster A handlers ---
	}
	if err := router.MountSeedLive(live); err != nil {
		return fmt.Errorf("route registry: %w", err)
	}
	if err := errs.Default.ValidateEmissions(); err != nil {
		return err
	}
	log.Info("route registry mounted", "routes", len(router.Routes()),
		"error_codes", errs.Default.Len())

	srv := &http.Server{
		Addr: cfg.Gateway.Addr(),
		// Task 2.3.6: X-Degradation-Mode on every response — outermost so
		// even early middleware rejections carry the header. Reads the same
		// coordination client the shard map uses; read failures report
		// Maintenance (fail-closed, spec §2.7). Inside it: request-id
		// stamping + panic recovery (Task 5.3.41), request logging, the
		// tiered rate limiter + progressive IP-ban gate
		// (Tasks 5.3.2/5.3.27/5.3.34), then API version negotiation
		// (Task 5.3.28). Rejections emit the registry-gated §8.7
		// envelope via router.WriteError.
		// Cluster E additions to the chain: SecurityHeaders (Task 5.3.29
		// item 9 — HAProxy does NOT emit these; the gateway is the single
		// emitter), Tracing (item 2 W3C traceparent continuation), and
		// Idempotency (Task 5.3.42 — the §8.8 Idempotency-Key surface on
		// money-moving POSTs, Redis ledger + account-scoped keys; the
		// resolver additionally reads X-API-KEY identities so signed
		// callers get the same dedup contract).
		Handler: middleware.DegradationModeHeader(
			rdb, middleware.SecurityHeaders(securityOpts(cfg),
				gateway.RequestID(router.Recover(
					func(f string, a ...any) { log.Error(fmt.Sprintf(f, a...)) },
					middleware.Tracing(
						middleware.Logging(log,
							middleware.RateLimit(limiter, middleware.RateLimitOptions{
								ResolveTier: tierResolver,
								Emit:        router.WriteError,
							})(
								middleware.Idempotency(
									middleware.NewRedisIdemStore(rdb.Client),
									nil, idemResolver(keyStore), router.WriteError)(
									middleware.APIVersion(middleware.VersionConfig{
										Versions: map[int]string{1: apiVersion},
										Emit:     router.WriteError,
									})(
										// Task 5.3.20: announced endpoints carry
										// Deprecation/Sunset/Link; past-sunset endpoints
										// answer 410 ENDPOINT_GONE before the handler.
										deprecation.Middleware(depRules,
											func(w http.ResponseWriter, req *http.Request, _ int, code, msg string) {
												router.WriteError(w, req, code, msg, nil)
											}, nil)(mux)))))))))),
		ReadHeaderTimeout: 5 * time.Second,
	}

	// Bind before announcing readiness so a port conflict exits nonzero
	// instead of logging "started" on a dead socket.
	ln, err := net.Listen("tcp", srv.Addr)
	if err != nil {
		return fmt.Errorf("bind %s: %w", srv.Addr, err)
	}

	ctx, stop := utils.SignalContext()
	defer stop()

	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ln) }()
	log.Info("gateway started", "addr", ln.Addr().String(), "env", cfg.Environment)

	select {
	case <-ctx.Done():
		log.Info("gateway shutting down")
		sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return srv.Shutdown(sctx)
	case err := <-serveErr:
		return err
	}
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
