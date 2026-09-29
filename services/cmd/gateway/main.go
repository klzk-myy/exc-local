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
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"exchange/internal/accounts"
	"exchange/internal/admin"
	"exchange/internal/api"
	"exchange/internal/auth"
	"exchange/internal/cache"
	"exchange/internal/compliance"
	"exchange/internal/config"
	"exchange/internal/copy"
	"exchange/internal/db"
	"exchange/internal/delegation"
	"exchange/internal/deprecation"
	"exchange/internal/errs"
	"exchange/internal/flags"
	"exchange/internal/fleet"
	"exchange/internal/funding"
	"exchange/internal/gateway"
	"exchange/internal/ipc"
	"exchange/internal/marketapi"
	"exchange/internal/middleware"
	"exchange/internal/nats"
	"exchange/internal/notifications"
	"exchange/internal/objectstore"
	"exchange/internal/observability"
	"exchange/internal/ops"
	"exchange/internal/orders"
	"exchange/internal/pamm"
	"exchange/internal/position"
	"exchange/internal/promos"
	"exchange/internal/ratelimit"
	"exchange/internal/reconciliation"
	"exchange/internal/redis"
	"exchange/internal/risk"
	"exchange/internal/security"
	"exchange/internal/settlement"
	"exchange/internal/support"
	"exchange/internal/tax"
	"exchange/internal/testenv"
	"exchange/internal/timesync"
	"exchange/internal/tracing"
	"exchange/internal/utils"
	"exchange/internal/webhooks"
	"exchange/internal/ws"
	"exchange/pkg/decimal"
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

	// Task 7.3.4: Prometheus /metrics on the same listener — the 15s
	// scrape cadence (deploy/prometheus/prometheus.yml) is far below the
	// rate limiter thresholds, and keeping it on the main mux means the
	// endpoint itself is inside the request-metrics middleware chain.
	metReg := observability.New()
	met := observability.NewMetrics(metReg, "gateway")
	mux.Handle("/metrics", metReg.Handler())

	// Phase-09 Task 9.3.11: OTLP-compatible tracer (no OTel SDK — the
	// internal model emits OTLP/HTTP JSON a collector fans out to
	// Jaeger/Tempo; deploy/otel/). EXC_OTLP_ENDPOINT selects the
	// collector; EXC_OTLP_FILE a JSONL file (node scrape); neither →
	// Nop. Sampling is spec §19.12.3 head 1% + tail on errors/slow;
	// EXC_TRACE_SAMPLE_ALL=1 overrides for dev/staging.
	var traceExp tracing.Exporter = tracing.NopExporter{}
	var traceBatch *tracing.BatchExporter
	switch {
	case os.Getenv("EXC_OTLP_ENDPOINT") != "":
		traceBatch = tracing.NewBatchExporter(
			&tracing.OTLPHTTPExporter{Endpoint: os.Getenv("EXC_OTLP_ENDPOINT")},
			2048, 500*time.Millisecond, 256)
		traceExp = traceBatch
	case os.Getenv("EXC_OTLP_FILE") != "":
		if fe, err := tracing.NewFileExporter(os.Getenv("EXC_OTLP_FILE")); err == nil {
			traceBatch = tracing.NewBatchExporter(fe, 2048, 500*time.Millisecond, 256)
			traceExp = traceBatch
		} else {
			log.Warn("tracing: EXC_OTLP_FILE open failed", "err", err)
		}
	}
	tracer := tracing.NewTracer("order-gateway", traceExp,
		&tracing.Options{
			SampleAll: os.Getenv("EXC_TRACE_SAMPLE_ALL") == "1",
		})
	if traceBatch != nil {
		metReg.CounterFunc("tracing_spans_dropped",
			"spans dropped by the export queue under backpressure",
			func() float64 { return float64(traceBatch.Dropped()) })
		metReg.CounterFunc("tracing_spans_exported",
			"spans shipped to the OTLP sink",
			func() float64 { return float64(traceBatch.Exported()) })
	}

	// Tasks 5.3.2/5.3.27/5.3.34: edge rate-limit + progressive-IP-ban
	// cluster. One Lua round trip per request atomically runs the ban
	// gate → token bucket → weighted counters (internal/ratelimit); a
	// Redis/Sentinel outage fails over to the in-memory backend (spec
	// §4.1) and a total limiter outage is fail-closed 503.
	// Task 14.3.6: identities crossing 80% of their effective tier rate
	// increment a per-tier counter — the RateLimitUtilizationHigh P2
	// alert reads it (per-account labels are rejected: cardinality).
	rlUtil := metReg.Counter("exchange_rate_limit_utilization_over80_total",
		"identities reaching >=80% of their effective tier rate limit in a 1s window")
	limiter := ratelimit.NewLimiter(ratelimit.NewRedisBackend(rdb),
		ratelimit.LimiterOptions{
			Mode: rdb,
			Utilization: func(id ratelimit.Identity, _, _ int64) {
				rlUtil.With("tier", string(id.Tier)).Inc()
			},
		})
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
	// Phase-07 Task 7.3.1/7.3.11: the venue-admin role store (migration
	// 090 admin_role_bindings). Its resolver feeds every Phase-05 seam —
	// freeze/unfreeze, manual liquidation, order audit/mass-cancel,
	// support tickets. No binding ⇒ "" ⇒ UNAUTHORIZED_ROLE (fail closed).
	adminStore := admin.NewStore(pool)
	adminRoleResolver := adminStore.RoleResolver()
	// Phase-11 kill-switch (Tasks 11.3.4/11.3.8/11.3.12): the Redis-backed
	// resolver is THE suspension lookup — order admission, withdrawal
	// create/dispatch, inbound-wire screening and FIX quote ingress all
	// consult it. Lookup errors fail closed in every consumer.
	killResolver := admin.NewKillSwitchResolver(rdb, cfg.Environment)
	freezeSvc := accounts.NewFreezeService(pool,
		accounts.RoleResolver(adminRoleResolver))
	riskLimits := risk.NewLimitsService(risk.NewPgStore(pool), nil, nil)
	if err := riskLimits.Load(context.Background()); err != nil {
		// Do not abort boot — the limits view fails closed per request and
		// the refresher retries (missing/empty table resolves to the §13.6
		// defaults anyway).
		log.Warn("risk limits initial load failed", "err", err)
	}
	riskLimits.StartRefresher(context.Background(), 60*time.Second,
		func(e error) { log.Warn("risk limits refresh failed", "err", e) })
	// Phase-13 Tasks 13.3.1/13.3.9 — five-tier circuit breaker (spec §2.6):
	// Redis HASH state (circuit_breaker:{scope}:{id}), PG audit
	// (circuit_breaker_events, migration 206), Prometheus
	// circuit_breaker_state/transitions_total, and the order-admission
	// gate wired into orders.Service below. Feeds: engine fill stream
	// (price + volume); ACCOUNT losses and IV bind the documented seams
	// (Phase-19 P&L / Phase-22 options surfaces) — NullIVSource is the
	// dev/null OPTIONS_VOLATILITY feed until then.
	breakerSvc, err := risk.NewCircuitBreakerService(risk.BreakerDeps{
		Store:   risk.RedisBreakerStore{C: rdb},
		Events:  risk.NewPgBreakerEventStore(pool),
		Metrics: risk.NewBreakerMetrics(metReg),
		IV:      risk.NullIVSource{},
		Logf:    func(f string, a ...any) { log.Warn(fmt.Sprintf(f, a...)) },
	})
	if err != nil {
		return fmt.Errorf("circuit-breaker service: %w", err)
	}
	if err := breakerSvc.Load(context.Background()); err != nil {
		// Boot continues: the in-process map is empty and the per-admission
		// read-through hydration still consults Redis — fail closed.
		log.Warn("circuit-breaker state load failed", "err", err)
	}
	usdConv := &settlement.RedisUsdConverter{Rdb: rdb}
	var opsAlerter funding.OpsAlerter
	if ledgerPub != nil {
		opsAlerter = settlement.PublisherAlerter{Pub: ledgerPub}
	}
	// Flapping penalty pages Risk Management via the shared ops alerter;
	// nil alerter (no NATS) degrades to the service log only.
	breakerSvc.WithAlerter(func(ctx context.Context, severity, code, summary string) error {
		if opsAlerter == nil {
			return nil
		}
		return opsAlerter.Raise(ctx, settlement.OpsAlert{
			Severity: severity, Code: code, Summary: summary})
	})
	// Phase-13 Task 13.3.6 — MiFID II RTS 9 order-to-trade ratio monitor:
	// order events (new/modify/cancel) and fills slide through Redis
	// zset windows (otr:events|otr:trades:{account}:{symbol}); a breach
	// (events > ratio × max(trades,1), defaults 500 / 60s — migration
	// 047) raises otr:breach:{account}, rejects new orders with
	// OTR_LIMIT_EXCEEDED while permitting cancels, and pages P2. The 1s
	// sweeper clears the flag when every active pair decays under its
	// limit; the C++ SuspensionRefresher mirrors the same keyspace into
	// PreTradeChecker as the engine-side backstop. Market-maker
	// allowance = a higher-ratio account/symbol-scoped risk_limits row
	// (resolved by riskLimits; no separate MM registry exists).
	otrMon := risk.NewOtrMonitor(rdb.Client, riskLimits).
		WithLogger(func(f string, a ...any) { log.Warn(fmt.Sprintf(f, a...)) }).
		WithMetrics(metReg).
		WithTierResolver(func(ctx context.Context, accountID int64) (string, error) {
			var tier string
			err := pool.QueryRow(ctx,
				`SELECT kyc_tier::text FROM accounts WHERE id = $1`,
				accountID).Scan(&tier)
			return tier, err
		})
	otrSinks := observability.FanoutSink{observability.LogSink{Log: log}}
	if ledgerPub != nil {
		otrSinks = append(otrSinks, observability.PublisherSink{Pub: ledgerPub})
	}
	otrMon.WithAlerts(otrSinks)
	// Task 7.3.10 DLQ inspection: bind the ops-dlq stream if it has been
	// provisioned (`natsctl dlq init`) — the registered route returns a
	// fail-closed 503 until then rather than silently fabricating one.
	var dlqStore observability.Store
	if natsClient != nil {
		dlqCtx, dlqCancel := context.WithTimeout(context.Background(), 3*time.Second)
		if s, derr := observability.OpenJetStreamDLQ(dlqCtx, natsClient.JetStream()); derr == nil {
			dlqStore = s
		} else {
			log.Warn("dlq stream not provisioned — GET /admin/dlq serves 503", "err", derr)
		}
		dlqCancel()
	}
	// Phase-13.5 Task 13.5.3.3 — file-backed sanctions screener.
	// EXC_SANCTIONS_LIST_DIR points at a directory of OFAC SDN CSV /
	// EU / UN consolidated XML / plain-text lists (dev fixture:
	// deploy/security/sanctions-dev; production vendor feeds are
	// Phase-21). A configured-but-unloadable directory is a boot
	// failure — fail closed rather than screening against nothing.
	// Unset keeps the documented residual: STANDARD-tier deposits and
	// withdrawals escalate to PENDING_REVIEW (SANCTIONS_UNAVAILABLE)
	// instead of screening.
	var screener *compliance.ListScreener
	if dir := strings.TrimSpace(os.Getenv("EXC_SANCTIONS_LIST_DIR")); dir != "" {
		screener, err = compliance.NewListScreener(dir)
		if err != nil {
			return fmt.Errorf("sanctions screener: %w", err)
		}
		lists, n, _ := screener.Stats()
		log.Info("sanctions screener loaded",
			"dir", dir, "lists", lists, "entries", n)
	} else {
		log.Warn("EXC_SANCTIONS_LIST_DIR unset — sanctions seam unwired; " +
			"STANDARD-tier funding reviews fail closed to PENDING_REVIEW")
	}
	withdrawalSvc, err := funding.NewWithdrawalService(fundStore, ledgerSvc, freezeSvc)
	if err != nil {
		return fmt.Errorf("withdrawal service: %w", err)
	}
	// Task 11.3.7 beneficiary registry — withdrawals may only target
	// VERIFIED beneficiaries past the 24h hold; verification is admin
	// dual-control (Finance Ops+). Task 11.3.12: the rail suspension
	// check refuses creates on a killed rail with SETTLEMENT_RAIL_REJECTED.
	bankAcctSvc, err := funding.NewBankAccountService(
		funding.NewPgBankAccountStore(pool),
		funding.BeneficiaryRoleResolver(adminRoleResolver))
	if err != nil {
		return fmt.Errorf("bank-account service: %w", err)
	}
	withdrawalSvc.WithLimits(riskLimits).WithUSDConverter(usdConv).
		WithAlerter(opsAlerter).
		WithBeneficiaries(bankAcctSvc).WithRailController(killResolver).
		WithBeneficiaryResolver(fundStore).
		WithLogger(func(f string, a ...any) { log.Error(fmt.Sprintf(f, a...)) })
	if screener != nil {
		withdrawalSvc.WithSanctions(screener)
	}
	transferSvc, err := funding.NewTransferService(fundStore, ledgerSvc, freezeSvc)
	if err != nil {
		return fmt.Errorf("transfer service: %w", err)
	}
	chargebackSvc, err := funding.NewChargebackService(fundStore, freezeSvc)
	if err != nil {
		return fmt.Errorf("chargeback service: %w", err)
	}
	historySvc := funding.NewHistoryService(fundStore)
	// Phase-11 rails+returns cluster (Tasks 11.3.1/11.3.11): rail matrix +
	// selection/dispatch, return-code mapping, third-party deposit guard.
	// Rail gate binds the scoped kill-switch halt flags
	// (halt:rail:{RAIL_ID}, Task 11.3.8/11.3.12 keys owned by
	// internal/redis.HaltKey). Legal-name resolution binds the KYC store
	// when one exists (Phase-14); the pg resolver below fails closed to
	// quarantine on unverifiable names.
	railSvc, err := funding.NewRailService(fundStore, ledgerSvc)
	if err != nil {
		return fmt.Errorf("rail service: %w", err)
	}
	railSvc.WithGate(railGate{rdb: rdb.Client}).WithAlerter(opsAlerter).
		WithLogger(func(f string, a ...any) { log.Error(fmt.Sprintf(f, a...)) })
	depositGuard, err := funding.NewDepositGuard(fundStore, ledgerSvc, railSvc)
	if err != nil {
		return fmt.Errorf("deposit guard: %w", err)
	}
	depositGuard.WithLegalNames(pgLegalNameResolver{pool: pool}).
		WithAlerter(opsAlerter).WithRailController(killResolver).
		WithLogger(func(f string, a ...any) { log.Error(fmt.Sprintf(f, a...)) })
	// Phase-11 stats+fees cluster (Tasks 11.3.5/11.3.9): the versioned
	// funding fee schedule (Finance Ops CRUD + client estimate), the
	// fee-charge engine (journal: DR customer liability → CR
	// funding_fee_revenue) and the currency-conversion engine. The
	// conversion rate seam binds the marketdata pipeline's
	// fx:rate:{CCY}USD keys (RedisCrossRateSource) — the Phase-19.5
	// oracle ReferencePriceSource has no concrete implementation yet;
	// missing/stale rates emit PRICE_ORACLE_UNAVAILABLE rather than a
	// fabricated rate (spec §2.7).
	feeSchedStore := funding.NewPgFeeScheduleStore(pool)
	feeSvc, err := funding.NewFeeService(feeSchedStore, fundStore, ledgerSvc)
	if err != nil {
		return fmt.Errorf("fee service: %w", err)
	}
	feeAdminSvc, err := funding.NewFeeScheduleService(feeSchedStore,
		funding.RoleResolver(adminRoleResolver))
	if err != nil {
		return fmt.Errorf("fee schedule service: %w", err)
	}
	convSvc, err := funding.NewConversionService(
		&funding.RedisCrossRateSource{Rdb: rdb},
		funding.NewPgConversionStore(pool), fundStore)
	if err != nil {
		return fmt.Errorf("conversion service: %w", err)
	}
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

	// Phase-12 Tasks 12.3.4/12.3.13: KYC submission intake + ops matrix.
	// Document bytes go to S3 via internal/objectstore with SSE-KMS
	// (spec §24 #102) — never PostgreSQL. Env: EXC_KYC_S3_BUCKET
	// (unset → submit fails closed SERVICE_DEGRADED; status,
	// requirements and self-certs stay live), EXC_S3_KMS_KEY_ID, and
	// the shared EXC_S3_* set (Endpoint ≠ "" + non-production selects
	// the devs3 stub). Virus scanner is the honest clean-pass dev seam —
	// a production deployment must inject a real engine.
	// Self-certification PII (TIN + fields document) seals through the
	// same SecretBox (migration 210, PII-F1). The boot-time backfill
	// re-encrypts pre-migration plaintext rows; failure is logged, not
	// fatal — sealed writes/reads still work and the next boot retries.
	kycStore, err := compliance.NewPgStore(pool, secretBox)
	if err != nil {
		return fmt.Errorf("kyc store: %w", err)
	}
	if n, berr := kycStore.SealTaxPIIBackfill(context.Background()); berr != nil {
		log.Error("tax PII seal backfill failed — plaintext fallback rows remain",
			"err", berr, "sealed", n)
	} else if n > 0 {
		log.Info("tax PII seal backfill complete", "rows", n)
	}
	var kycObjects objectstore.Client
	if bucket := os.Getenv("EXC_KYC_S3_BUCKET"); bucket != "" {
		ocfg := objectstore.ConfigFromEnv(bucket, os.Getenv)
		var oerr error
		if cfg.Environment != "production" && ocfg.Endpoint != "" {
			kycObjects, oerr = objectstore.NewDev(context.Background(), ocfg)
		} else {
			kycObjects, oerr = objectstore.NewAWS(context.Background(), ocfg)
		}
		if oerr != nil {
			log.Warn("KYC document store init failed — submit fails closed", "err", oerr)
			kycObjects = nil
		}
	}
	if cfg.Environment == "production" {
		log.Warn("KYC virus scanner is the clean-pass dev seam — inject a real engine before production use")
	}
	kycSvc := compliance.NewService(kycStore, kycObjects,
		os.Getenv("EXC_S3_KMS_KEY_ID"),
		compliance.CleanPassScanner{Log: func(f string, a ...any) {
			log.Info(fmt.Sprintf(f, a...))
		}})
	// Phase-14 Task 14.3.7 — MiFID II client categorization: the
	// order-admission gate consults catSvc.Appropriateness on every
	// exposure-adding order (wired into orders.Options.Product below);
	// the admin product-profile route sets client_category through it
	// (Compliance Officer + evidence, audit-logged).
	catSvc, err := compliance.NewCategorizationService(kycStore,
		compliance.RoleResolver(adminRoleResolver))
	if err != nil {
		return fmt.Errorf("categorization service: %w", err)
	}
	// Phase-14 Tasks 14.3.13/14.3.15/14.3.16 — product governance
	// cluster: ProfileService resolves account→profile (pricing_plan is
	// the commission engine's single fee-model source via
	// PgProfileFeeModelSource — no account-level override exists);
	// SwapfreeService owns the request → Compliance decision lifecycle
	// mirrored to accounts.swapfree_status (the Task 3.3.19/3.3.23
	// rollover consumes that column for the zero-Tom-Next skip);
	// TargetMarketService authors the MiFID II per-category target and
	// drives the ≤12-month review sweep; ProductGateService folds
	// instrument_scope + the RETAIL target-market check into the
	// orders.ProductGate admission seam (appropriateness remains the
	// separate Task 14.3.7 gate — both must pass).
	profileSvc := accounts.NewProfileService(pool,
		accounts.RoleResolver(adminRoleResolver))
	swapfreeSvc := accounts.NewSwapfreeService(pool,
		accounts.RoleResolver(adminRoleResolver), accounts.PgxHoldPlacer{})
	var tmAlerter accounts.OpsAlerter
	if opsAlerter != nil {
		tmAlerter = opsAlerter
	}
	targetSvc := accounts.NewTargetMarketService(pool,
		accounts.RoleResolver(adminRoleResolver), tmAlerter)
	productGate := accounts.NewProductGateService(profileSvc, targetSvc,
		categorizerAdapter{catSvc})

	// Phase-14 Task 14.3.8 — PAMM/MAM engine. Invest/redeem post TRANSFER
	// journals through ledgerSvc (2010 ↔ 2170_PAMM_POOL_LIABILITY) —
	// never DEPOSIT/WITHDRAWAL, never the daily fiat counters.
	pammStore, err := pamm.NewPgxStore(pool)
	if err != nil {
		return fmt.Errorf("pamm store: %w", err)
	}
	pammSvc, err := pamm.NewService(pammStore, ledgerSvc, freezeSvc)
	if err != nil {
		return fmt.Errorf("pamm service: %w", err)
	}

	depStore, err := deprecation.NewStore(pool)
	if err != nil {
		return fmt.Errorf("deprecation store: %w", err)
	}
	// 5s TTL cache — the middleware resolves rules per request.
	depRules := deprecation.CachedRules(depStore, 5*time.Second)

	// ---- Phase-11 flows cluster: Tasks 11.3.2 (canonical withdrawal
	//      lifecycle), 11.3.3 (deposit lifecycle + anti-fraud tiers),
	//      11.3.6 (nostro-aware dispatch + dual-control replenishment),
	//      11.3.10 (whitelist mode + 24h timelocks) ----
	//
	// WhitelistService owns the per-account mode + egress lock; the
	// FlowService wraps WithdrawalService with the Phase-11 gates
	// (whitelist/beneficiary/cooldown/hold at create, TOTP step-up at
	// confirm) and hands CONFIRMED withdrawals to the DispatchService,
	// which refuses to rail-send anything the nostro cannot cover.
	whitelistSvc, err := funding.NewWhitelistService(fundStore)
	if err != nil {
		return fmt.Errorf("whitelist service: %w", err)
	}
	dispatchSvc, err := funding.NewDispatchService(fundStore, ledgerSvc)
	if err != nil {
		return fmt.Errorf("nostro dispatch service: %w", err)
	}
	dispatchSvc.WithRails(railSvc).WithAlerter(opsAlerter).
		WithLogger(func(f string, a ...any) { log.Error(fmt.Sprintf(f, a...)) })
	depositSvc, err := funding.NewDepositService(fundStore, ledgerSvc, freezeSvc)
	if err != nil {
		return fmt.Errorf("deposit service: %w", err)
	}
	depositSvc.WithUSDConverter(usdConv).WithAlerter(opsAlerter).
		WithLogger(func(f string, a ...any) { log.Error(fmt.Sprintf(f, a...)) })
	if screener != nil {
		depositSvc.WithSanctions(screener)
	}
	flowSvc, err := funding.NewFlowService(withdrawalSvc, fundStore)
	if err != nil {
		return fmt.Errorf("withdrawal flow service: %w", err)
	}
	flowSvc.WithTOTP(accounts.NewPgxTOTPSecrets(pool, secretBox), auth.VerifyTOTP).
		WithDispatcher(dispatchSvc).
		WithLogger(func(f string, a ...any) { log.Error(fmt.Sprintf(f, a...)) })

	// Flows sweep: every 30s retries CONFIRMED withdrawals held for
	// nostro headroom or destination holds (SweepDue), and escalates
	// PENDING_REVIEW rows whose 4h ops deadline lapsed (SweepReviewSLA).
	go func() {
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-sweepCtx.Done():
				return
			case <-t.C:
				if n, err := dispatchSvc.SweepDue(sweepCtx, 100); err != nil {
					log.Warn("nostro dispatch sweep failed", "err", err)
				} else if n > 0 {
					log.Info("queued withdrawals dispatched", "count", n)
				}
				if n, err := depositSvc.SweepReviewSLA(sweepCtx, 100); err != nil {
					log.Warn("review SLA sweep failed", "err", err)
				} else if n > 0 {
					log.Info("funding review SLA breaches escalated", "count", n)
				}
			}
		}
	}()
	// --- end Phase-11 flows cluster ---

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
	// Phase-14 Task 14.3.16 — target-market review sweeper: flips
	// APPROVED rows past review_due_at to REVIEW_OVERDUE (close-only for
	// that category's opens) and raises one ops alert per newly overdue
	// row; the order gate also evaluates review_due_at lazily so a
	// delayed sweep can never admit a stale retail open.
	go func() {
		t := time.NewTicker(60 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-sweepCtx.Done():
				return
			case <-t.C:
				if n, err := targetSvc.SweepOverdue(sweepCtx, 500); err != nil {
					log.Warn("target-market overdue sweep failed", "err", err)
				} else if n > 0 {
					log.Info("target-market rows marked REVIEW_OVERDUE", "count", n)
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
	venueDeps := &api.VenueDeps{Store: marketStore, Cache: marketCache,
		Profiles: profileSvc} // Task 14.3.13 — per-profile scope publication
	announceDeps := &api.AnnounceDeps{Announcements: marketStore, Maintenance: marketStore}

	// ---- Phase-05 wave-2 cluster E: WS surface, order pipeline, manual
	//      liquidation, API hardening (Tasks 5.3.26/29/30/31/42) ----

	// Order pipeline (Task 5.3.3 family): PgStore is the read/write
	// model, ShmSubmitter the engine-bound Aeron ring producer. A
	// missing engine image fails closed SERVICE_DEGRADED at dispatch.
	orderStore := orders.NewPgStore(pool)
	orderSubmitter := orders.NewShmSubmitter("") // ipc.DefaultShmBase rings
	orderSvc, err := orders.NewService(orders.Options{
		Store:      orderStore,
		Submitter:  orderSubmitter,
		ShardMap:   shardMap,
		Limits:     riskLimits,
		KillSwitch: killResolver, // Tasks 11.3.4/11.3.8 — every admission path consults it
		Otr:        otrMon,       // Task 13.3.6 — RTS 9 OTR event counting + breach gate
		Breakers:   breakerSvc,   // Tasks 13.3.1/13.3.9 — §2.6 five-tier breaker gate
		BatchRL:    orders.NewRedisBatchLimiter(rdb.Client, 0),
		Products:   productGate, // Tasks 14.3.13/14.3.16 — profile scope + retail target market
		Product:    catSvc,      // Task 14.3.7 — MiFID II appropriateness gate
	})
	if err != nil {
		return fmt.Errorf("order service: %w", err)
	}

	// ---- Phase-12 Task 12.3.5 — notification service ----
	//
	// PgStore carries §24 #100 delivery tracking (migration 028
	// notification_deliveries + notification_dead_letters) and the
	// Task 12.3.6 preference rows (migration 202). Queue is the
	// coordination-Redis reliable queue (notifications:pending →
	// processing claim + retry zset, 1s→2s→4s→8s→16s, 5 attempts →
	// dead letter). Senders: dev LogSenders on email/sms/push (real
	// SES/Twilio/FCM providers bind behind the same Sender seam when
	// credentials land) and a WSSender that fans out to the user's
	// account-scoped "private:notifications" channel on /ws/v1 — the
	// Phase-10 frontend's notification subscription. wsSrv is assigned
	// below; the pusher tolerates a nil hub (no subscribers can exist
	// before the endpoint mounts anyway).
	var wsSrv *ws.Server

	// Phase-13 Task 13.3.4 — real-time P&L service: per-position
	// mark-to-market (signedQty × (mark − entry)) aggregated per quote
	// currency, converted to accounts.base_currency through the Task
	// 3.3.9 FX converter. Marks resolve through the Phase-19.5 oracle
	// seam — today the last-trade reference price (orderStore), the
	// same seam order admission uses; PositionService's stored mark is
	// the fallback inside the service. The publisher binds wsSrv
	// lazily (hub constructed below; fills can land first).
	pnlStore := risk.NewPnlPgStore(pool)
	pnlSvc, err := risk.NewPnlService(risk.PnlOptions{
		Store: pnlStore,
		Marks: orderStore, // last-trade reference price — PriceOracle placeholder
		Converter: position.NewConverter(risk.LastTradeRates{
			Instruments: pnlStore,
			Marks:       orderStore,
		}, ""),
		Publisher: pnlPublisherFunc(func(accountID int64, channel string, data any) {
			if wsSrv != nil {
				wsSrv.PublishPrivate(accountID, channel, data)
			}
		}),
	})
	if err != nil {
		return fmt.Errorf("pnl service: %w", err)
	}

	// Phase-14 Task 14.3.14 — copy-trading product layer over the PAMM
	// pro-rata engine. Discovery stats compute from persisted fills via
	// the Task 3.3.9 index converter (same seam P&L uses); a missing rate
	// excludes the strategy — computed-only, fail closed. Suspension runs
	// the hash-chained admin audit BEFORE the status flip.
	copyStore, err := copy.NewPgxStore(pool)
	if err != nil {
		return fmt.Errorf("copy store: %w", err)
	}
	copyAuditor, err := copy.NewAdminAuditor(pool)
	if err != nil {
		return fmt.Errorf("copy auditor: %w", err)
	}
	copySubledger, err := copy.NewSubledgerWriter(pammStore)
	if err != nil {
		return fmt.Errorf("copy subledger: %w", err)
	}
	copySvc, err := copy.NewService(copyStore, catSvc, freezeSvc,
		settlement.ConverterIndexPricer{Conv: position.NewConverter(
			risk.LastTradeRates{Instruments: pnlStore, Marks: orderStore}, "")},
		copyAuditor,
		copy.WithJournalPoster(ledgerSvc),
		copy.WithSubledgerWriter(copySubledger))
	if err != nil {
		return fmt.Errorf("copy service: %w", err)
	}

	notifStore := notifications.NewPgStore(pool)
	notifSvc, err := notifications.NewService(notifications.Options{
		Store: notifStore,
		Queue: notifications.NewQueue(rdb.Client),
		Senders: []notifications.Sender{
			&notifications.LogSender{Ch: notifications.ChannelEmail, Log: log},
			&notifications.LogSender{Ch: notifications.ChannelSMS, Log: log},
			&notifications.LogSender{Ch: notifications.ChannelPush, Log: log},
			&notifications.WSSender{Push: notifications.PusherFunc(
				func(ctx context.Context, userID int64, channel string, data any) error {
					return wsNotifyPush(ctx, wsSrv, pool, userID, channel, data)
				})},
		},
		// Anti-phish seam: PgAntiPhish reads users.anti_phishing_code —
		// until the sibling task's column lands it maps the
		// undefined-column error to "unset" so mail still flows with
		// the "set your anti-phishing code" banner.
		Anti: notifications.PgAntiPhish{Pool: pool},
		Dir:  notifStore,
		Logf: func(f string, a ...any) { log.Warn(fmt.Sprintf(f, a...)) },
	})
	if err != nil {
		return fmt.Errorf("notifications service: %w", err)
	}
	// Dispatcher shares the sweeper lifecycle — a claimed-but-unacked
	// item is requeued on next boot (RequeueAll), never dropped.
	go notifSvc.NewDispatcher().Run(sweepCtx)

	// Real-time copy/PAMM fill fan-out: consume the `trades` JetStream
	// stream the settlements bridge already republishes (documented seam —
	// no second engine subscription). At-least-once delivery is absorbed
	// by the DB dedup keys; a NATS outage degrades copying (fail-
	// operational, consistent with ledger dispatch) and redelivery replays
	// safely. Child orders dispatch through the real order pipeline
	// (copyChildSubmitter → orders.Service.Submit, MARKET/IOC); the
	// durable-PENDING intent row precedes dispatch so a crash leaves a
	// replayable intent, and the "copy:{trade}:{follow}" client order id
	// dedups at the pipeline too.
	pammEngine, err := pamm.NewEngine(pammStore)
	if err != nil {
		return fmt.Errorf("pamm engine: %w", err)
	}
	copyEngine, err := copy.NewEngine(copyStore,
		copy.WithChildSubmitter(&copyChildSubmitter{svc: orderSvc, store: orderStore}),
		copy.WithSkipNotifier(&copySkipNotifier{pool: pool, svc: notifSvc}))
	if err != nil {
		return fmt.Errorf("copy engine: %w", err)
	}
	if natsClient != nil {
		fanout, ferr := pamm.NewTradesFanout(settlement.NewPgxTradeResolver(pool),
			pamm.PoolLegHandler{Engine: pammEngine},
			copy.ManagerLegHandler{Engine: copyEngine})
		if ferr != nil {
			return fmt.Errorf("trades fanout: %w", ferr)
		}
		cons, cerr := natsClient.EnsureConsumer(context.Background(), "trades",
			"pamm_copy_fanout", nats.WithFilterSubject("trades.>"))
		if cerr != nil {
			// Consumer provisioning failure degrades copying only —
			// orders/settlement are unaffected (fail-operational, NATS
			// outage posture per the ledger dispatch comment above).
			log.Warn("pamm/copy fanout consumer unavailable", "err", cerr)
		} else {
			go func() {
				if cerr := fanout.Consume(sweepCtx, cons); cerr != nil {
					log.Error("pamm/copy fanout consumer stopped", "err", cerr)
				}
			}()
			log.Info("pamm/copy trades fanout consuming", "stream", "trades",
				"durable", "pamm_copy_fanout")
		}
	}

	// Phase-14 Task 14.3.2 — auto-halt on anomaly: the detector layer
	// bound to the five-tier breaker (trips land on the canonical
	// machinery; this service adds the P1 page, user notification and
	// auto_halt_events audit — migration 217). Feeds bound below:
	// price/volume via the fill consumer; admission latency + systemic
	// error rate via orders.Service's observer seam. A missing PG pool
	// piece fails closed at construction only for the breaker itself —
	// nil seams degrade to logged skips, mirroring BreakerDeps.
	autoHaltSvc, err := risk.NewAutoHaltService(risk.AutoHaltDeps{
		CB:      breakerSvc,
		Events:  risk.NewPgAutoHaltEventStore(pool),
		Metrics: risk.NewAutoHaltMetrics(metReg),
		Alerter: func(ctx context.Context, severity, code, summary string) error {
			if opsAlerter == nil {
				return nil
			}
			return opsAlerter.Raise(ctx, settlement.OpsAlert{
				Severity: severity, Code: code, Summary: summary})
		},
		Notifier: notifSvc,
		Users: func(ctx context.Context, symbol string) ([]int64, error) {
			rows, err := pool.Query(ctx, `
				SELECT DISTINCT a.user_id FROM accounts a
				 WHERE a.id IN (
				   SELECT p.account_id FROM positions p
				     JOIN instruments i ON i.id = p.instrument_id
				    WHERE i.symbol = $1 AND p.quantity <> 0
				   UNION
				   SELECT o.account_id FROM orders o
				     JOIN instruments i ON i.id = o.instrument_id
				    WHERE i.symbol = $1
				      AND o.status IN ('PENDING','RESERVED','ACTIVE','PARTIALLY_FILLED'))`,
				symbol)
			if err != nil {
				return nil, err
			}
			defer rows.Close()
			var uids []int64
			for rows.Next() {
				var uid int64
				if err := rows.Scan(&uid); err != nil {
					return nil, err
				}
				uids = append(uids, uid)
			}
			return uids, rows.Err()
		},
		Logf: func(f string, a ...any) { log.Warn(fmt.Sprintf(f, a...)) },
	})
	if err != nil {
		return fmt.Errorf("auto-halt service: %w", err)
	}
	orderSvc.WithAdmission(autoHaltSvc.ObserveAdmission)
	go autoHaltSvc.Run(sweepCtx, 2*time.Second)
	// funding.Notifier seam: account→user resolution then Notify —
	// post-commit, best-effort (the funding services never see an
	// error from this adapter).
	fundNotifier := notifyAdapter{fn: func(ctx context.Context, accountID int64, event string, payload map[string]any) {
		var userID int64
		if err := pool.QueryRow(ctx,
			`SELECT user_id FROM accounts WHERE id = $1`, accountID).Scan(&userID); err != nil {
			log.Warn("notifications: account→user resolve failed",
				"account_id", accountID, "event", event, "err", err)
			return
		}
		if _, err := notifSvc.Notify(ctx, userID, event, payload); err != nil {
			log.Warn("notifications: emit failed",
				"event", event, "user_id", userID, "err", err)
		}
	}}
	depositSvc.WithNotifier(fundNotifier)
	dispatchSvc.WithNotifier(fundNotifier)

	// Phase-14 Task 14.3.4 — KYC lifecycle: the admin approve/reject
	// surface (single write path to accounts.kyc_tier, audited in-tx)
	// plus the hourly re-verification sweeper — overdue latest-APPROVED
	// submissions auto-downgrade the account T2→T1 (INSTITUTIONAL lapses
	// also revert client_category → RETAIL + re-arm nbp), each action
	// audit-anchored, ops-alerted and user-notified.
	lifecycleSvc, err := compliance.NewLifecycleService(compliance.LifecycleOptions{
		Store:    kycStore,
		Resolver: compliance.RoleResolver(adminRoleResolver),
		Notifier: fundNotifier,
		Alerter: func(ctx context.Context, severity, code, summary string) error {
			if opsAlerter == nil {
				return nil
			}
			return opsAlerter.Raise(ctx, settlement.OpsAlert{
				Severity: severity, Code: code, Summary: summary})
		},
	})
	if err != nil {
		return fmt.Errorf("kyc lifecycle service: %w", err)
	}
	go func() {
		t := time.NewTicker(time.Hour)
		defer t.Stop()
		for {
			select {
			case <-sweepCtx.Done():
				return
			case <-t.C:
				if n, err := lifecycleSvc.SweepReverify(sweepCtx, 200); err != nil {
					log.Warn("kyc re-verification sweep had failures", "err", err)
				} else if n > 0 {
					log.Warn("kyc re-verification downgrades applied", "count", n)
				}
			}
		}
	}()

	// Engine out-ring consumer: cancel echoes + trade fills update the PG
	// read model; without it pending confirms only time out. The fill
	// hook is the order_filled emitter (Task 12.3.5) — order→account→user
	// resolution then Notify, best-effort, panic-guarded by the consumer.
	go orders.NewConsumer(orderSubmitter, orderStore, orderSvc.Pending()).
		WithFillHook(func(orderID int64, px, qty decimal.Decimal) {
			ctx := context.Background()
			o, oerr := orderStore.GetOrder(ctx, orderID)
			if oerr != nil || o == nil {
				return
			}
			// Phase-13 circuit-breaker feeds: last-trade price →
			// INSTRUMENT move window + MARKET_WIDE aggregate; fill
			// notional → VOLUME_SPIKE minute buckets. Routed through the
			// Phase-14 auto-halt layer, which forwards to the canonical
			// breaker detectors and performs the P1 page + user
			// notification + audit duties on fresh suspensions.
			// Best-effort: a feed error is logged inside the service,
			// never masks the fill notification path.
			if inst, ierr := orderStore.InstrumentByID(ctx, o.InstrumentID); ierr == nil && inst != nil {
				autoHaltSvc.ObservePrice(ctx, inst.Symbol, px)
				autoHaltSvc.ObserveTrade(ctx, inst.Symbol, px.Mul(qty))
				// Task 13.3.6: the fill counts in the account's OTR
				// trades window (denominator); an under-limit verdict
				// re-evaluates the breach flag.
				otrMon.Fill(ctx, o.AccountID, "", inst.Symbol)
			}
			// Task 13.3.4: republish the account's live P&L plus the
			// mark-update fanout to other holders of the instrument.
			if pnlSvc != nil {
				pnlSvc.OnTrade(ctx, o.AccountID, o.InstrumentID)
			}
			var userID int64
			if err := pool.QueryRow(ctx,
				`SELECT user_id FROM accounts WHERE id = $1`, o.AccountID).Scan(&userID); err != nil {
				return
			}
			_, _ = notifSvc.Notify(ctx, userID, notifications.EventOrderFilled, map[string]any{
				"order_id":      o.ID,
				"instrument_id": o.InstrumentID,
				"side":          o.Side,
				"price":         px.String(),
				"quantity":      qty.String(),
			})
		}).Run(sweepCtx, shardIDs(shardMap))

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
	// (Task 5.3.1). JWT key material resolves through the Phase-13.5
	// secret-source seam (Task 13.5.3.5): when EXC_SECRETS_SOURCE is set
	// (or production semantics require it) the issuer loads
	// exchange/<env>/jwt/signing via security.SourceFromEnv + LoadSecrets
	// — Vault KV in production, labeled dev adapters otherwise. The
	// legacy EXC_JWT_HS256_KEY_B64 env var remains the dev/ops bootstrap
	// when no source is configured. Absent or unreachable material fails
	// closed (CONFIG_LOAD_FAILED / no-key error).
	sigVerifier, err := auth.NewSignatureVerifier(keyStore, auth.NewRedisReplayGuard(rdb))
	if err != nil {
		return fmt.Errorf("signature verifier: %w", err)
	}
	jwtIssuer := auth.NewIssuer("exc.local", "exc-api", 0)
	if os.Getenv("EXC_SECRETS_SOURCE") != "" || cfg.IsProduction() {
		if err := loadJWTFromSecretSource(context.Background(), cfg, jwtIssuer, log); err != nil {
			return err
		}
	} else if raw := os.Getenv("EXC_JWT_HS256_KEY_B64"); raw != "" {
		kb, derr := base64.StdEncoding.DecodeString(raw)
		if derr != nil || jwtIssuer.AddHMACKey("v1", kb, true) != nil {
			return fmt.Errorf("EXC_JWT_HS256_KEY_B64 invalid")
		}
		log.Info("jwt verify keyring: 1 key (EXC_JWT_HS256_KEY_B64)")
	} else {
		log.Warn("no JWT key material — /ws/v1 JWT authenticate fails closed (dev)")
	}

	// Task 5.3.26 + 5.3.31: the unified /ws/v1 endpoint. Trading actions
	// dispatch through ws.NewOrdersDispatcher → orders.Service — never a
	// direct engine call. Dedup is the Redis 60s request_id window
	// (Task 5.3.42); X-Forwarded-For is trusted because Task 5.3.29's
	// HAProxy front is the only supported client path (the listener is
	// not directly exposed in the reference deployment).
	wsSrv = ws.NewServer(ws.Config{
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
	// Phase-13 Task 13.3.9: stream every breaker transition to the
	// "admin.circuit_breaker" WS admin-monitor channel (documented seam —
	// no pre-existing admin monitor channel in ws.Server).
	breakerSvc.WithPublisher(wsSrv)
	// Task 13.3.9 sweeper: 1s cadence advances OPEN→HALF_OPEN at hold
	// expiry and resolves 30s probe windows (HALF_OPEN→CLOSED/OPEN).
	go breakerSvc.Run(sweepCtx, time.Second)
	// Task 13.3.6 OTR sweeper: 1s cadence re-evaluates flagged accounts
	// and clears otr:breach:* once every active pair decays under its
	// limit (pure window decay has no triggering event otherwise).
	go otrMon.Run(sweepCtx, time.Second)

	// Phase-11 kill-switch control plane (Tasks 11.3.4/11.3.8/11.3.12):
	// dual-controlled set/clear (GLOBAL + destructive COUNTERPARTY) over
	// the durable trading_suspensions record + halt:* Redis flags, with
	// broadcast to the announcement feed, WS venue.status advisory and
	// the exchange:control:killswitch Aeron/NATS control topic. Boot-time
	// ReconcileFlags re-raises ACTIVE suspensions after a Redis flush —
	// PostgreSQL is authoritative; a reconcile failure is logged not
	// fatal (the resolver fails closed on unreadable flags anyway).
	killSvc, err := admin.NewKillSwitchService(admin.KillSwitchDeps{
		Pool:      pool,
		Flags:     rdb,
		Roles:     admin.AdminRoleResolver(adminRoleResolver),
		Announcer: killSwitchAnnouncer{store: marketStore},
		WS:        wsSrv,
		Control:   killSwitchControlPub{nc: natsClient},
		Canceller: restingOrderCanceller{svc: orderSvc},
		Logf:      func(f string, a ...any) { log.Warn(fmt.Sprintf(f, a...)) },
	})
	if err != nil {
		return fmt.Errorf("kill-switch service: %w", err)
	}
	if n, rerr := killSvc.ReconcileFlags(context.Background()); rerr != nil {
		log.Warn("kill-switch flag reconcile failed", "err", rerr)
	} else if n > 0 {
		log.Info("kill-switch flags reconciled", "active_suspensions", n)
	}

	// Phase-13 Task 13.3.2 — nine-category hourly reconciliation engine.
	// The sweep composes the Phase-04 wallet-diff legs (composed via
	// recovery.RecWalletReconcileDiff, never reimplemented), the core
	// WAL journal replay, and the §5.19/§5.46/§5.21 projections. A
	// MISMATCH pages P1 (durable funding_ops_alerts row + the shared
	// NATS ops-alert subject) and emits a scoped auto-halt through the
	// same artifacts KillSwitchService commits — a trading_suspensions
	// row (initiated_by=0, the machine sentinel) plus the halt:* flag —
	// so the enforcement surface and the admin resume path are
	// identical (engine ruling R4). The scheduler shares the sweeper
	// lifecycle; hourly ±10% jitter, interval injectable for tests.
	reconStore := reconciliation.NewPgStore(pool)
	reconEngine, err := reconciliation.NewEngine(reconciliation.Deps{
		Pool:    pool,
		Store:   reconStore,
		Alerter: reconciliation.NewDurableOpsAlerter(pool, opsAlerter),
		Halter:  reconciliation.NewPgHalter(pool, rdb),
		WalDirs: reconciliationWalDirs(shardMap),
		Logf:    func(f string, a ...any) { log.Warn(fmt.Sprintf(f, a...)) },
	}, reconciliation.DefaultCheckers(reconciliation.NewPgLegs(pool), nil, nil))
	if err != nil {
		return fmt.Errorf("reconciliation engine: %w", err)
	}
	go reconEngine.RunScheduler(sweepCtx, reconciliationInterval())

	// Phase-13 Task 13.3.7 — solvency proof read surface. The store
	// serves the published-snapshot reads; generation runs out-of-process
	// (`exchange solvency-tree`, deploy/crons/solvency-tree.sh, 22:00
	// UTC) so the signing key never needs to live in the gateway's env —
	// and a signing failure can never degrade the trading surface.
	solvStore, err := reconciliation.NewSolvencyStore(pool)
	if err != nil {
		return fmt.Errorf("solvency store: %w", err)
	}

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
	liqSvc := api.NewManualLiquidationService(pool,
		api.AdminRoleResolver(adminRoleResolver), liqSink)

	// Phase-07 Tasks 7.3.7/7.3.3 — support tickets / complaints /
	// read-only support-view + the admin audit query surface.
	//
	// RoleResolver seam: the shared admin-role store (admin.Store,
	// migration 090) is constructed beside freezeSvc above — no binding
	// resolves to "" which the services map to UNAUTHORIZED_ROLE (fail
	// closed, spec §2.7).
	ticketSvc := support.NewService(pool, support.RoleResolver(adminRoleResolver), supportAlerter{nc: natsClient})
	supportViewSvc := admin.NewSupportViewService(pool)

	// Phase-07 Task 7.3.9 — LP management. MetricsSource/AlertSink stay
	// nil until the Phase-06/17 lp_performance pipeline + ops.alerts
	// bridge land: scorecards then serve the last persisted snapshot
	// marked stale (never fabricated), and threshold alerts persist to
	// lp_performance_alerts undispatched.
	lpSvc := admin.NewLPService(pool, admin.AdminRoleResolver(adminRoleResolver), nil, nil)

	// Phase-07 Tasks 7.3.13/7.3.14 — governance packs. OpsSource stays
	// nil until a ModeManager/SLO reader is wired; the ops section then
	// renders STALE and generation never blocks. Delivery nil → QUEUED.
	packSvc := admin.NewGovernancePackService(pool,
		admin.AdminRoleResolver(adminRoleResolver), nil, nil)

	// Task 7.3.7 SLA sweeper: marks breached complaint-ack / complaint-
	// final / first-response SLAs and raises ops alerts
	// (COMPLAINT_SLA_BREACH stays internal — surfaced as an alert, never
	// an API error). Each SLA kind alerts once per ticket via the
	// once-only escalations arithmetic in tickets.go.
	go func() {
		tick := time.NewTicker(60 * time.Second)
		defer tick.Stop()
		for {
			select {
			case <-sweepCtx.Done():
				return
			case <-tick.C:
				if n, err := ticketSvc.SweepAlerts(sweepCtx, time.Now().UTC()); err != nil {
					log.Warn("support SLA sweep", "err", err)
				} else if n > 0 {
					log.Warn("support SLA breaches flagged", "tickets", n)
				}
			}
		}
	}()
	// --- end cluster E wiring ---

	// Wave-2 cluster A REST order surface (Tasks 5.3.3/5.3.22/5.3.24/
	// 5.3.25/5.3.32/5.3.37/5.3.39). The HMAC verifier built for the WS
	// API-key path is reused for the signed REST path; TrustProxy=true
	// because HAProxy (Task 5.3.29) is the only supported client path.
	orderDeps := &api.OrderDeps{
		SVC:          orderSvc,
		Verifier:     sigVerifier,
		TrustProxy:   true,
		RoleResolver: adminRoleResolver, // Phase-07 role store (fail-closed on no binding)
	}

	// --- Phase-07 Tasks 7.3.1/7.3.2/7.3.11/7.3.12 — admin RBAC cluster ---
	// Session store on the coordination Redis (§4.1 noeviction): binding
	// expiry/revocation kills sessions, and the middleware re-validates
	// the sid claim so a kill lands on the very next request.
	sessStore := auth.NewRedisSessionStore(rdb)
	sessMgr, sessErr := auth.NewSessionManager(sessStore, jwtIssuer, auth.SessionConfig{})
	if sessErr != nil {
		return fmt.Errorf("session manager: %w", sessErr)
	}

	// ---- Phase-12 Tasks 12.3.1/12.3.2/12.3.3 — registration, TOTP 2FA,
	//      profile & account API keys ----
	// userStore persists migration-027 credentials + profile columns and
	// seals totp_secret through the same SecretBox contract
	// accounts.PgxTOTPSecrets reads; tokenCache stages the ephemeral
	// single-use tokens (email verify 24h / password reset 1h / TOTP
	// challenge 5m / pending 2FA candidate 10m) on the coordination
	// Redis. The mail seam is the dev LogSender — the orchestrator swaps
	// in the Phase-12 Task 12.3.5 notification pipeline when it lands.
	// The LoginRecorder seam stays nil until the Task 12.3.9
	// login-history store (migration 069) is wired — events are dropped,
	// never guessed.
	userStore, err := auth.NewUserStore(pool, secretBox)
	if err != nil {
		return fmt.Errorf("user store: %w", err)
	}
	tokenCache := auth.NewRedisTokenCache(rdb.Client)
	authnSvc, err := auth.NewAuthnService(userStore, sessMgr, tokenCache,
		auth.NewLogSender(func(f string, a ...any) { log.Info(fmt.Sprintf(f, a...)) }))
	if err != nil {
		return fmt.Errorf("authn service: %w", err)
	}
	authnSvc.WithMailBase(os.Getenv("EXC_PUBLIC_BASE_URL")).
		WithLogger(func(f string, a ...any) { log.Warn(fmt.Sprintf(f, a...)) })
	twoFactorSvc, err := auth.NewTwoFactorService(userStore, tokenCache, sessMgr, "exc.local")
	if err != nil {
		return fmt.Errorf("2fa service: %w", err)
	}
	acctKeyCreate, acctKeyList, acctKeyRevoke := api.AccountAPIKeys(keyStore)

	// --- Phase-12 Tasks 12.3.7/12.3.8/12.3.9/12.3.12 — account security ---
	// Login history sink (migration 069) + Redis brute-force lockout
	// (Task 12.3.12) plug into the authn seams; security events route
	// through the notification service's security_alert event.
	secNotify := securityEventAdapter{svc: notifSvc}
	loginHistorySvc, err := auth.NewLoginHistoryService(pool)
	if err != nil {
		return fmt.Errorf("login history service: %w", err)
	}
	lockoutSvc := auth.NewLockoutService(rdb).
		WithNotifier(secNotify).
		WithLogger(func(f string, a ...any) { log.Warn(fmt.Sprintf(f, a...)) })
	authnSvc.WithRecorder(loginHistorySvc).WithLockout(lockoutSvc)
	antiPhishSvc, err := auth.NewAntiPhishingService(pool)
	if err != nil {
		return fmt.Errorf("anti-phishing service: %w", err)
	}
	// WebAuthn relying-party identity: EXC_WEBAUTHN_RP_ID /
	// EXC_WEBAUTHN_ORIGINS override; else derived from EXC_PUBLIC_BASE_URL.
	waCfg := webAuthnConfig()
	waStore, err := auth.NewPgWebAuthnStore(pool)
	if err != nil {
		return fmt.Errorf("webauthn store: %w", err)
	}
	waSvc, err := auth.NewWebAuthnService(waCfg, waStore,
		auth.NewRedisWebAuthnChallengeStore(rdb))
	if err != nil {
		return fmt.Errorf("webauthn service: %w", err)
	}
	// Task 12.3.12 part 2: clone detection freezes via the machine seam
	// (accounts.SecurityFreezeService — the dual-control FreezeService
	// cannot serve an automated response).
	waSvc.WithFreezer(accounts.NewSecurityFreezeService(pool)).
		WithNotifier(secNotify).
		WithLogger(func(f string, a ...any) { log.Warn(fmt.Sprintf(f, a...)) })
	passkeyDeps := api.PasskeyDeps{
		WebAuthn: waSvc, Sessions: sessMgr, Users: userStore,
		Lockout: lockoutSvc, Recorder: loginHistorySvc,
	}
	// --- end Phase-12 authn/2FA/profile/security wiring ---
	// §8.2b session kill: sessions index on the bound account once one is
	// selected, else on the user — revoke BOTH indexes.
	killSessions := func(ctx context.Context, userID int64) error {
		uid := strconv.FormatInt(userID, 10)
		if err := sessMgr.RevokeAll(ctx, 0, uid); err != nil {
			return err
		}
		rows, err := pool.Query(ctx,
			`SELECT id FROM accounts WHERE user_id=$1`, userID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var acct int64
			if err := rows.Scan(&acct); err != nil {
				return err
			}
			if err := sessMgr.RevokeAll(ctx, acct, uid); err != nil {
				return err
			}
		}
		return rows.Err()
	}
	var rbacAlerter admin.Alerter
	if opsAlerter != nil {
		oa := opsAlerter
		rbacAlerter = func(ctx context.Context, severity, summary string) error {
			return oa.Raise(ctx, settlement.OpsAlert{
				Severity: severity, Code: "RBAC_LIFECYCLE", Summary: summary})
		}
	}
	adminSvc := admin.NewService(pool, adminStore, killSessions, rbacAlerter)
	dualSvc := admin.NewDualControlService(pool, adminStore)
	api.RegisterRoleChangeExecutor(dualSvc, adminSvc)
	// Task 13.3.8: the expiry-extension executor — the grant applies
	// inside the second approver's transaction, never at submit time.
	api.RegisterAPIKeyExpiryExecutor(dualSvc, keyStore)

	// Phase-13 Task 13.3.8 — API-key privilege auto-expiry policy. The
	// in-process sweeper re-runs the idempotent warn→revoke→restore pass
	// hourly (the daily `exchange api-key-expiry-sweep` cron job is the
	// scheduled-of-record path); a query-level failure logs loudly and
	// retries next tick — a silent skip would defeat the defense.
	keyExpiry, err := auth.NewKeyExpiryPolicy(keyStore, secNotify)
	if err != nil {
		return fmt.Errorf("api-key expiry policy: %w", err)
	}
	keyExpiry.WithLogger(func(f string, a ...any) { log.Warn(fmt.Sprintf(f, a...)) })
	go func() {
		t := time.NewTicker(time.Hour)
		defer t.Stop()
		for {
			select {
			case <-sweepCtx.Done():
				return
			case <-t.C:
				res, serr := keyExpiry.Sweep(sweepCtx)
				if serr != nil {
					log.Warn("api-key expiry sweep failed", "err", serr)
				} else if res.Warned+res.Revoked+res.Restored > 0 {
					log.Info("api-key expiry sweep", "warned", res.Warned,
						"revoked", res.Revoked, "restored", res.Restored)
				}
			}
		}
	}()
	// Phase-13 Task 13.3.9: dual-controlled breaker reset — the second
	// approver's approval runs ManualReset inside the decision tx; a
	// failed close aborts and leaves the request PENDING for retry.
	api.RegisterBreakerResetExecutor(dualSvc, breakerSvc)

	// ---- Phase-12 Tasks 12.3.10/12.3.11 + 12.3.12(part 3) ----
	// Client-side delegation (migration 074): master-account workforce
	// RBAC (CLIENT_* roles, explicit account/instrument scopes) plus the
	// M-of-N approval engine. Disjoint from the venue-admin RBAC above —
	// principal_role_systems (migration 090) enforces the exclusion.
	// Revocations kill sessions through the shared killSessions helper;
	// the withdrawal create path consults the M-of-N policy via
	// flowSvc's approval gate.
	delegSvc := delegation.NewService(pool,
		delegatedSessionKiller{kill: killSessions})
	flowSvc.WithApprovalGate(delegSvc)

	// Self-service emergency freeze (migration 152 + 201): session-only
	// panic button — mass-cancel (≤3 retries) → FROZEN/SELF_FREEZE →
	// other sessions + API keys die; on exhausted cancels the owner
	// login suspends and a P1 alert carries the stuck order ids.
	emergencyFreezeSvc := accounts.NewEmergencyFreezeService(pool,
		orderDisp, sessMgr, apiKeyRevoker{ks: keyStore},
		openOrderLister{store: orderStore},
		freezeOpsAlerter{pool: pool, page: opsAlerter})
	unfreezeSvc := accounts.NewUnfreezeService(pool)

	// --- Phase-14 Tasks 14.3.9–14.3.12: account lifecycle wiring ---
	// Best-effort user notification adapter shared by cooling-off,
	// closure and holds (same contract as fundNotifier above — a
	// delivery failure never blocks the state transition).
	acctNotify := accounts.UserNotifier(func(ctx context.Context,
		userID int64, event string, payload map[string]any) {
		if _, err := notifSvc.Notify(ctx, userID, event, payload); err != nil {
			log.Warn("notifications: emit failed",
				"event", event, "user_id", userID, "err", err)
		}
	})

	// Task 14.3.11/14.3.12 — cooling-off self-exclusion. The irrevocable
	// window row lands before the leveraged de-risking saga (mass-cancel
	// + reduce-only closes on the shared dispatcher), so a dispatch
	// outage still fails closed on order admission; residual failures
	// page P1 via the existing ops alerter. The admission gate binds
	// post-construction — the service needs orderDisp, which needs
	// orderSvc, so the With* pattern (same as WithAdmission) breaks the
	// construction cycle.
	coolingSvc := accounts.NewCoolingOffService(pool, orderDisp,
		freezeOpsAlerter{pool: pool, page: opsAlerter}, acctNotify)
	orderSvc.WithCoolingOff(coolingSvc)

	// Task 14.3.9 — account closure & offboarding. Client path requires
	// the route's RequireTwoFactor wrap; the forced path is the
	// OpAccountClosure four-eyes executor. The residual sweep reuses
	// the Phase-11 withdrawal pipeline verbatim — beneficiary +
	// sanctions + cooldown gates apply, the hold posting is GL-balanced
	// and CONFIRMED rows hand off to the banking rails.
	closureSvc := accounts.NewClosureService(pool, orderDisp,
		closureSweeper{flow: flowSvc, inner: withdrawalSvc, disp: dispatchSvc},
		closureBens{pool: pool},
		sessMgr, apiKeyRevoker{ks: keyStore},
		openOrderLister{store: orderStore},
		freezeOpsAlerter{pool: pool, page: opsAlerter}, acctNotify, nil)
	api.RegisterAccountClosureExecutor(dualSvc, closureSvc)
	// Phase-14 Task 14.3.13 — profile pricing/scope/divisor mutations
	// run through the four-eyes queue; approval applies them in-tx.
	api.RegisterProductProfileExecutor(dualSvc, profileSvc)

	// Task 14.3.10 — compliance holds. PlaceHold is the Phase-21
	// sanctions/PEP seam (machine triggers call it directly); the
	// manual officer endpoint is the only caller today. Release is
	// four-eyes (same convention as unfreeze); escalate-to-closure
	// submits the OpAccountClosure request above. Placement freezes and
	// mass-cancels through the existing paths — positions never
	// liquidate under a hold.
	holdSvc := compliance.NewHoldService(pool,
		holdRestingCanceller{disp: orderDisp},
		holdOpsAlerter{inner: freezeOpsAlerter{pool: pool, page: opsAlerter}},
		closureEscalation{dual: dualSvc},
		compliance.HoldRoleResolver(adminRoleResolver),
		compliance.HoldUserNotifier(acctNotify), nil)

	// Hold SLA sweeper (60s): mark sla_breached on overdue OPEN holds
	// and page P1 — the officer dashboard surfaces breaches until
	// disposition (4h high-confidence sanctions / 24h default).
	go func() {
		tick := time.NewTicker(60 * time.Second)
		defer tick.Stop()
		for {
			select {
			case <-sweepCtx.Done():
				return
			case <-tick.C:
				if n, err := holdSvc.SweepSLA(sweepCtx, 200); err != nil {
					log.Warn("compliance-hold SLA sweep", "err", err)
				} else if n > 0 {
					log.Warn("compliance holds breached SLA", "count", n)
				}
			}
		}
	}()

	// Task 14.3.12 — JetStream ingest into the canonical signed-delivery
	// pipeline (Phase-05 Task 5.3.17 owns POST/sign/retry/dead-letter —
	// the Phase-14 divergent schedule was superseded; this consumer is
	// the event→queue leg the task calls for).
	if natsClient != nil {
		go func() {
			if err := webhooks.NewJetStreamIngest(natsClient,
				webhookStore, log).Run(sweepCtx); err != nil {
				log.Warn("webhook jetstream ingest stopped", "err", err)
			}
		}()
	}

	// RBACMiddleware on the registry: every route declaring Auth.Role gets
	// identity → binding → role → env/scope enforcement — stubs included.
	rbacMW := admin.NewMiddleware(adminStore, jwtIssuer, sessMgr, cfg.Environment)
	router.SetWrapper(rbacMW.WrapRoute)

	// Lifecycle sweeps (30s): binding expiry + session kill, recert
	// suspension lag, break-glass expiry + overdue-review enforcement,
	// four-eyes window lapse. Each is audited; a sweep failure logs and
	// retries next tick — never silently allows.
	go func() {
		tick := time.NewTicker(30 * time.Second)
		defer tick.Stop()
		for {
			select {
			case <-sweepCtx.Done():
				return
			case <-tick.C:
				if n, err := adminSvc.ExpireDue(sweepCtx, 500); err != nil {
					log.Warn("rbac expiry sweep", "err", err)
				} else if n > 0 {
					log.Info("rbac bindings expired", "count", n)
				}
				if n, err := adminSvc.SuspendOverdueRecerts(sweepCtx, 500); err != nil {
					log.Warn("recert sweep", "err", err)
				} else if n > 0 {
					log.Warn("recert suspensions applied", "count", n)
				}
				if n, err := adminSvc.ExpireBreakGlass(sweepCtx, 200); err != nil {
					log.Warn("break-glass expiry sweep", "err", err)
				} else if n > 0 {
					log.Info("break-glass grants expired", "count", n)
				}
				if n, err := adminSvc.EnforceBreakGlassReview(sweepCtx, 200); err != nil {
					log.Warn("break-glass review enforcement", "err", err)
				} else if n > 0 {
					log.Warn("break-glass reviews overdue — granters suspended", "count", n)
				}
				if n, err := dualSvc.ExpireDue(sweepCtx); err != nil {
					log.Warn("dual-control expiry sweep", "err", err)
				} else if n > 0 {
					log.Info("dual-control requests expired", "count", n)
				}
				// Task 12.3.11: lapse PENDING client approval requests
				// past their policy window (audited per row).
				if n, err := delegSvc.ExpireApprovals(sweepCtx, 500); err != nil {
					log.Warn("delegation approval expiry sweep", "err", err)
				} else if n > 0 {
					log.Info("delegation approval requests expired", "count", n)
				}
			}
		}
	}()

	rbacDeps := &api.RBACDeps{
		Store: adminStore, SVC: adminSvc, Dual: dualSvc,
		Routes: gateway.SeedRoutes(), TrustProxy: true,
	}
	// --- end RBAC cluster wiring ---

	// Phase-09 Task 9.3.30: environment/fleet/promotion-gate backend
	// (migration 091). The env-scoped role/env enforcement is the
	// Task 7.3.11 middleware on the route metadata (Env="env-scoped"
	// requires an explicit env axis on the binding); the service adds the
	// promotion direction lattice and §19.16.3 interlock probes.
	fleetSvc := fleet.NewService(pool, fleet.RoleResolver(adminRoleResolver), nil)

	// ---- Phase-09 ops wiring (Tasks 9.3.6/9.3.7/9.3.8/9.3.10/9.3.25) ----
	//
	// Feature flags (9.3.7): PG-backed store, Redis flags:{name} cache.
	flagStore, err := flags.NewStore(pool, rdb.Client)
	if err != nil {
		return fmt.Errorf("flag store: %w", err)
	}
	flagList, flagCreate, flagGet, flagUpdate, flagToggle, flagDel, flagAdvance :=
		api.FlagHandlers(flagStore)

	// ---- Phase-13.5 Tasks 13.5.3.8/13.5.3.9 — Vulnerability Disclosure
	//      Program (spec §19.11.2, §24 #332) ----
	// Register: migration 081. Role resolution shares the Phase-07
	// admin-role store seam — nil fails closed UNAUTHORIZED_ROLE. The
	// change-freeze probe reads the `vdp_change_freeze` feature flag:
	// ops sets it during deploy freeze windows; a CRITICAL disclosure
	// triaged inside one is flagged expedited_path + P1 notice (the
	// documented emergency-change lane, policy.md §SLAs).
	vdpSvc := security.NewService(pool,
		security.RoleResolver(adminRoleResolver),
		vdpAlerter{nc: natsClient}).
		WithChangeFreeze(func(ctx context.Context) bool {
			return flagStore.Enabled(ctx, "vdp_change_freeze", flags.EvalContext{})
		})
	vdpPolicy := loadVDPPolicy(log)
	go func() {
		// VDP SLA sweeper — same 60s cadence as the support sweep:
		// claims each breached milestone clock once and pages
		// VDP_SLA_BREACH (P2, Security/DevOps) on ops.alerts.security.
		tick := time.NewTicker(60 * time.Second)
		defer tick.Stop()
		for {
			select {
			case <-sweepCtx.Done():
				return
			case <-tick.C:
				if n, err := vdpSvc.SweepAlerts(sweepCtx, time.Now().UTC()); err != nil {
					log.Warn("vdp SLA sweep", "err", err)
				} else if n > 0 {
					log.Warn("vdp SLA breaches flagged", "disclosures", n)
				}
			}
		}
	}()
	// --- end VDP wiring ---

	// Cache warming (9.3.8): boot-time "deploy" pass plus the warm:trigger
	// pub/sub funnel the watchdog/DR coordinator publish on recovery and
	// failover.
	warmer := cache.New(&cache.Env{
		Pool: pool, RDB: rdb.Client, Log: log,
	}, cache.DefaultUnits())
	go func() {
		if rep := warmer.Run(sweepCtx, "deploy"); !rep.OK() {
			log.Warn("boot cache warm incomplete — check warm:state:* markers")
		}
	}()
	go func() {
		if err := warmer.RunOnTriggers(sweepCtx); err != nil && sweepCtx.Err() == nil {
			log.Error("warm trigger loop exited", "err", err)
		}
	}()

	// Deprecation ops (9.3.6): sunset sweep + usage compaction; the
	// middleware telemetry sink is wired into the apiSurface below.
	sunsetTotal := metReg.Counter("deprecation_sunset_total",
		"Deprecation rules transitioned to SUNSET by the sweep.")
	depSweeper := &deprecation.Sweeper{
		Store: depStore, RDB: rdb.Client, Log: log,
		OnSunset: func(n int) { sunsetTotal.With().Add(float64(n)) },
	}
	go depSweeper.Run(sweepCtx)

	// Load shedding (9.3.10): inbound queue depth is the summed Aeron
	// ingress-ring occupancy; capacity drives the §2.7 80%/95%
	// watermarks. A channel that cannot open reports -1 → conservative
	// stage-1 posture, never silent health.
	shedder := middleware.NewShedder(middleware.ShedConfig{
		Depth: func() int64 {
			var d int64
			for _, id := range shardIDs(shardMap) {
				ch, err := orderSubmitter.Channel(id)
				if err != nil {
					return -1
				}
				d += int64(ch.Occupancy())
			}
			return d
		},
		Capacity: int64(len(shardIDs(shardMap))) * int64(ipc.DefaultRingCapacity),
	})
	go shedder.Run(sweepCtx)
	metReg.GaugeFunc("load_shed_stage",
		"Active load-shed stage (0=off).",
		func() float64 { return float64(shedder.Stage()) })
	metReg.CounterFunc("load_shed_rejected_total",
		"Requests shed under Task 9.3.10.",
		func() float64 { return float64(shedder.Stats().ShedTotal) })

	// Status aggregation (9.3.25): the exporter probes the same
	// dependencies readiness does, publishes status:current +
	// status:component:* + status:events into Redis each second, and
	// records transitions/incidents into the ops_* tables (migration
	// 197). NATS is non-critical (JetStream dispatch is
	// fail-operational); a dead engine ring is critical.
	statusAgg := ops.NewAggregator(pool, rdb.Client, rdb, []ops.Component{
		{Name: "postgres", Critical: true, Probe: pool.Ping},
		{Name: "redis", Critical: true, Probe: rdb.Ping},
		{Name: "nats", Probe: func(ctx context.Context) error {
			if natsClient == nil {
				return fmt.Errorf("nats client not configured")
			}
			return natsClient.Conn().FlushWithContext(ctx)
		}},
		{Name: "engine_shards", Critical: true, Probe: func(ctx context.Context) error {
			for _, id := range shardIDs(shardMap) {
				ch, err := orderSubmitter.Channel(id)
				if err != nil {
					return fmt.Errorf("shard %d channel: %w", id, err)
				}
				if !ch.ProducerAlive() {
					return fmt.Errorf("shard %d engine producer not alive", id)
				}
			}
			return nil
		}},
	}, log)
	go statusAgg.Run(sweepCtx, time.Second)

	// Drain latch (9.3.23): flips on signal — readiness reports
	// unhealthy immediately while in-flight work drains.
	drainFlag := &middleware.DrainFlag{}
	// ---- end Phase-09 ops wiring ----

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
		// Phase-13 Task 13.3.4 — real-time realized/unrealized P&L rollup.
		"GET /api/v1/account/pnl":         http.HandlerFunc(api.AccountPnL(pnlSvc)),
		"GET /api/v1/deposits/{currency}": http.HandlerFunc(api.DepositInstructions(fundStore)),
		// Task 11.3.2: withdrawal create/confirm run the Phase-11 flow
		// wrapper — whitelist/cooldown/beneficiary gates at create, the
		// TOTP step-up at confirm, then nostro-aware dispatch.
		// Task 12.3.2 adds the session-AMR gate on submission: the caller
		// must have proven the second factor this session (totp/fido2);
		// confirm then demands the fresh per-withdrawal code.
		"POST /api/v1/withdrawals": auth.RequireTwoFactor()(
			http.HandlerFunc(api.CreateWithdrawalFlow(flowSvc))),
		"POST /api/v1/withdrawals/{id}/confirm": http.HandlerFunc(
			api.ConfirmWithdrawalStepUp(flowSvc)),
		"POST /api/v1/admin/withdrawals/{id}/approve": http.HandlerFunc(
			api.AdminWithdrawalReview(flowSvc, true)),
		"POST /api/v1/admin/withdrawals/{id}/reject": http.HandlerFunc(
			api.AdminWithdrawalReview(flowSvc, false)),
		"GET /api/v1/funding": http.HandlerFunc(api.FundingHistory(historySvc)),
		// Phase-11 Task 11.3.7 — beneficiary registry (client + admin).
		"POST /api/v1/funding/bank-accounts":   http.HandlerFunc(api.FundingBankAccountCreate(bankAcctSvc)),
		"GET /api/v1/funding/bank-accounts":    http.HandlerFunc(api.FundingBankAccountList(bankAcctSvc)),
		"DELETE /api/v1/funding/bank-accounts": http.HandlerFunc(api.FundingBankAccountDelete(bankAcctSvc)),
		"GET /api/v1/admin/funding/bank-accounts": http.HandlerFunc(
			api.AdminBankAccountList(bankAcctSvc)),
		"POST /api/v1/admin/funding/bank-accounts/{id}/verify": http.HandlerFunc(
			api.AdminBankAccountVerify(bankAcctSvc, true)),
		"POST /api/v1/admin/funding/bank-accounts/{id}/reject": http.HandlerFunc(
			api.AdminBankAccountReject(bankAcctSvc, true)),
		// Phase-11 Tasks 11.3.4/11.3.8/11.3.12 — kill-switch control plane.
		"POST /api/v1/admin/kill-switch": http.HandlerFunc(
			api.AdminKillSwitchSet(killSvc, true)),
		"POST /api/v1/admin/kill-switch/reset": http.HandlerFunc(
			api.AdminKillSwitchReset(killSvc, true)),
		"GET /api/v1/admin/kill-switch": http.HandlerFunc(
			api.AdminKillSwitchStatus(killSvc)),
		// Phase-13 Tasks 13.3.1/13.3.9 — circuit-breaker admin surface.
		// Trip is single-approver Risk Manager; reset goes through the
		// four-eyes queue (admin.OpCircuitBreakerReset executor above).
		"POST /api/v1/admin/circuit-breaker/{symbol}": http.HandlerFunc(
			api.AdminCircuitBreakerTrip(api.CircuitBreakerDeps{
				Breakers: breakerSvc, Dual: dualSvc, TrustProxy: true})),
		"POST /api/v1/admin/circuit-breaker/{symbol}/reset": http.HandlerFunc(
			api.AdminCircuitBreakerReset(api.CircuitBreakerDeps{
				Breakers: breakerSvc, Dual: dualSvc, TrustProxy: true})),
		"POST /api/v1/transfers":                      http.HandlerFunc(api.CreateTransfer(transferSvc)),
		"GET /api/v1/transfers":                       http.HandlerFunc(api.TransferHistory(historySvc)),
		"POST /api/v1/admin/chargebacks":              http.HandlerFunc(api.AdminChargebackCreate(chargebackSvc)),
		"GET /api/v1/admin/chargebacks":               http.HandlerFunc(api.AdminChargebackList(chargebackSvc)),
		"GET /api/v1/admin/chargebacks/{id}":          http.HandlerFunc(api.AdminChargebackDetail(chargebackSvc)),
		"POST /api/v1/admin/chargebacks/{id}/submit":  http.HandlerFunc(api.AdminChargebackSubmit(chargebackSvc)),
		"POST /api/v1/admin/chargebacks/{id}/resolve": http.HandlerFunc(api.AdminChargebackResolve(chargebackSvc)),
		// --- end Cluster B handlers ---
		// --- Phase-11 rails+returns live handlers (Tasks 11.3.1/11.3.11) ---
		"GET /api/v1/funding/rails":           http.HandlerFunc(api.FundingRails(railSvc)),
		"POST /api/v1/funding/rail-selection": http.HandlerFunc(api.FundingRailSelection(railSvc)),
		"POST /api/v1/admin/funding/inbound-wires": http.HandlerFunc(
			api.AdminInboundWire(depositGuard)),
		"GET /api/v1/admin/funding/quarantine": http.HandlerFunc(
			api.AdminQuarantineList(fundStore)),
		"POST /api/v1/admin/funding/quarantine/{id}/resolve": http.HandlerFunc(
			api.AdminQuarantineResolve(depositGuard)),
		"POST /api/v1/admin/funding/returns": http.HandlerFunc(
			api.AdminRailReturn(railSvc)),
		// --- Phase-11 flows cluster (Tasks 11.3.2/11.3.3/11.3.6/11.3.10) ---
		"POST /api/v1/deposits": http.HandlerFunc(
			api.CreateDepositIntent(depositSvc)),
		"POST /api/v1/admin/funding/deposits": http.HandlerFunc(
			api.AdminDepositIngest(depositSvc)),
		"POST /api/v1/admin/funding/deposits/{id}/confirm": http.HandlerFunc(
			api.AdminDepositConfirm(depositSvc)),
		"POST /api/v1/admin/funding/deposits/{id}/review": http.HandlerFunc(
			api.AdminDepositReview(depositSvc)),
		"GET /api/v1/funding/withdrawal-whitelist": http.HandlerFunc(
			api.GetWithdrawalWhitelist(whitelistSvc)),
		"POST /api/v1/funding/withdrawal-whitelist/enable": http.HandlerFunc(
			api.SetWithdrawalWhitelist(whitelistSvc, true)),
		"POST /api/v1/funding/withdrawal-whitelist/disable": http.HandlerFunc(
			api.SetWithdrawalWhitelist(whitelistSvc, false)),
		"GET /api/v1/admin/funding/nostro": http.HandlerFunc(
			api.AdminNostroCoverage(dispatchSvc)),
		"GET /api/v1/admin/funding/nostro/replenishments": http.HandlerFunc(
			api.AdminReplenishmentList(dispatchSvc)),
		"POST /api/v1/admin/funding/nostro/replenishments": http.HandlerFunc(
			api.AdminReplenishmentCreate(dispatchSvc)),
		"POST /api/v1/admin/funding/nostro/replenishments/{id}/decide": http.HandlerFunc(
			api.AdminReplenishmentDecide(dispatchSvc)),
		"GET /api/v1/admin/funding/ops-alerts": http.HandlerFunc(
			api.AdminFundingOpsAlerts(dispatchSvc)),
		// --- Phase-13 Task 13.3.2 reconciliation report surface ---
		"GET /api/v1/admin/reconciliation/latest": http.HandlerFunc(
			api.AdminReconciliationLatest(reconStore)),
		"GET /api/v1/admin/reconciliation/runs": http.HandlerFunc(
			api.AdminReconciliationRuns(reconStore)),
		// --- Phase-13 Task 13.3.7 solvency proof surface ---
		"GET /api/v1/solvency/latest": http.HandlerFunc(
			api.SolvencyLatest(solvStore)),
		"GET /api/v1/solvency/proof": http.HandlerFunc(
			api.SolvencyProof(solvStore)),
		"GET /api/v1/account/solvency-proof": http.HandlerFunc(
			api.AccountSolvencyProof(solvStore)),
		// --- Phase-14 Tasks 14.3.8/14.3.14 — PAMM + copy trading ---
		"POST /api/v1/pamm/pools": http.HandlerFunc(
			api.PammPoolCreate(pammSvc)),
		"POST /api/v1/pamm/pools/{id}/invest": http.HandlerFunc(
			api.PammInvest(pammSvc)),
		"POST /api/v1/pamm/pools/{id}/redeem": http.HandlerFunc(
			api.PammRedeem(pammSvc)),
		"GET /api/v1/copy/strategies": http.HandlerFunc(
			api.CopyStrategies(copySvc)),
		"POST /api/v1/copy/strategies": http.HandlerFunc(
			api.CopyStrategyCreate(copySvc)),
		"POST /api/v1/copy/strategies/{id}/list": http.HandlerFunc(
			api.CopyStrategyList(copySvc)),
		"POST /api/v1/copy/follows": http.HandlerFunc(
			api.CopyFollow(copySvc)),
		"DELETE /api/v1/copy/follows/{id}": http.HandlerFunc(
			api.CopyUnfollow(copySvc)),
		"POST /api/v1/admin/copy/strategies/{id}/suspend": http.HandlerFunc(
			api.AdminCopyStrategySuspend(copySvc, true)),
		"GET /api/v1/public/proof-of-reserves/daily-root": http.HandlerFunc(
			api.SolvencyLatest(solvStore)),
		// --- Phase-13 Task 13.3.8 API-key expiry extension (four-eyes) ---
		"PUT /api/v1/admin/api-keys/{id}/extend-expiry": http.HandlerFunc(
			api.AdminAPIKeyExtendExpiry(dualSvc, true)),
		// --- end flows cluster ---
		// --- Phase-11 stats+fees cluster (Tasks 11.3.5/11.3.9) ---
		"GET /api/v1/stats/24h":          http.HandlerFunc(api.MarketStats24hAll(marketDeps)),
		"GET /api/v1/stats/24h/{symbol}": http.HandlerFunc(api.MarketStats24h(marketDeps)),
		"POST /api/v1/funding/fee-estimate": http.HandlerFunc(
			api.FundingFeeEstimate(feeSvc)),
		"POST /api/v1/funding/convert": http.HandlerFunc(api.FundingConvert(convSvc)),
		"GET /api/v1/funding/conversions": http.HandlerFunc(
			api.FundingConversions(convSvc)),
		"GET /api/v1/admin/funding/fees": http.HandlerFunc(
			api.AdminFundingFeeList(feeAdminSvc, true)),
		"POST /api/v1/admin/funding/fees": http.HandlerFunc(
			api.AdminFundingFeeCreate(feeAdminSvc, true)),
		"GET /api/v1/admin/funding/fees/{id}": http.HandlerFunc(
			api.AdminFundingFeeGet(feeAdminSvc, true)),
		"PUT /api/v1/admin/funding/fees/{id}": http.HandlerFunc(
			api.AdminFundingFeeUpdate(feeAdminSvc, true)),
		"DELETE /api/v1/admin/funding/fees/{id}": http.HandlerFunc(
			api.AdminFundingFeeRetire(feeAdminSvc, true)),
		"GET /api/v1/admin/funding/fees/{id}/versions": http.HandlerFunc(
			api.AdminFundingFeeVersions(feeAdminSvc, true)),
		// --- end Phase-11 handlers ---
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
		// Phase-12 Task 12.3.6 — notification preferences (GET/PUT).
		"GET /api/v1/account/notifications/preferences": http.HandlerFunc(
			api.NotificationPreferencesGet(notifStore)),
		"PUT /api/v1/account/notifications/preferences": http.HandlerFunc(
			api.NotificationPreferencesPut(notifStore)),
		// --- Wave-3 platform surface ---
		// Task 5.3.13 test environment.
		"POST /api/v1/test/reset": http.HandlerFunc(api.TestReset(testSvc)),
		// Task 14.3.3 testnet — preset seeding + simulated funding (same
		// fail-closed non-production gate; never touches funding rails).
		"POST /api/v1/test/seed":               http.HandlerFunc(api.TestSeed(testSvc)),
		"POST /api/v1/test/reset-seed":         http.HandlerFunc(api.TestResetSeed(testSvc)),
		"POST /api/v1/test/funding/deposit":    http.HandlerFunc(api.TestFundDeposit(testSvc)),
		"POST /api/v1/test/funding/withdrawal": http.HandlerFunc(api.TestFundWithdraw(testSvc)),
		// Task 5.3.15 fees + governed promo windows.
		"GET /api/v1/fees":                           http.HandlerFunc(api.AccountFees(settlement.NewPgxFeeStore(pool))),
		"POST /api/v1/admin/fees/promo":              http.HandlerFunc(promoCreate),
		"GET /api/v1/admin/fees/promos":              http.HandlerFunc(promoList),
		"POST /api/v1/admin/fees/promo/{id}/approve": http.HandlerFunc(promoApprove),
		"POST /api/v1/admin/fees/promo/{id}/reject":  http.HandlerFunc(promoReject),
		// Task 5.3.16 developer portal (Swagger UI + API-key CRUD).
		"GET /developer": http.HandlerFunc(api.DeveloperPortal()),
		// 12.3.2 DoD: "API key creation" is 2FA-gated on every path —
		// account + developer portal alike.
		"POST /api/v1/developer/api-keys":        auth.RequireTwoFactor()(http.HandlerFunc(keyCreate)),
		"GET /api/v1/developer/api-keys":         http.HandlerFunc(keyList),
		"DELETE /api/v1/developer/api-keys/{id}": http.HandlerFunc(keyRevoke),

		// ---- Phase-12 Task 12.3.1 — registration & password auth ----
		"POST /api/v1/auth/register":        http.HandlerFunc(api.AuthRegister(authnSvc)),
		"POST /api/v1/auth/verify-email":    http.HandlerFunc(api.AuthVerifyEmail(authnSvc)),
		"POST /api/v1/auth/login":           http.HandlerFunc(api.AuthLogin(authnSvc)),
		"POST /api/v1/auth/refresh":         http.HandlerFunc(api.AuthRefresh(authnSvc)),
		"POST /api/v1/auth/logout":          http.HandlerFunc(api.AuthLogout(authnSvc)),
		"POST /api/v1/auth/forgot-password": http.HandlerFunc(api.AuthForgotPassword(authnSvc)),
		"POST /api/v1/auth/reset-password":  http.HandlerFunc(api.AuthResetPassword(authnSvc)),

		// ---- Phase-12 Task 12.3.2 — TOTP 2FA lifecycle ----
		"POST /api/v1/auth/2fa/setup":   http.HandlerFunc(api.TwoFactorSetup(twoFactorSvc)),
		"POST /api/v1/auth/2fa/enroll":  http.HandlerFunc(api.TwoFactorEnroll(twoFactorSvc)),
		"POST /api/v1/auth/2fa/verify":  http.HandlerFunc(api.TwoFactorVerify(twoFactorSvc)),
		"POST /api/v1/auth/2fa/disable": http.HandlerFunc(api.TwoFactorDisable(twoFactorSvc)),

		// ---- Phase-12 Task 12.3.7 — WebAuthn/FIDO2 passkeys ----
		"POST /api/v1/auth/passkey/assert": http.HandlerFunc(api.PasskeyAssert(passkeyDeps)),
		"POST /api/v1/account/webauthn/register": http.HandlerFunc(
			api.WebAuthnRegister(waSvc)),
		"POST /api/v1/account/webauthn/authenticate": http.HandlerFunc(
			api.WebAuthnAuthenticate(waSvc, sessMgr)),

		// ---- Phase-12 Task 12.3.8 — anti-phishing code (2FA-gated) ----
		"PUT /api/v1/account/settings/anti-phishing-code": http.HandlerFunc(
			api.AntiPhishingSet(antiPhishSvc)),

		// ---- Phase-12 Task 12.3.9 — device mgmt & login history ----
		"GET /api/v1/account/login-history": http.HandlerFunc(
			api.AccountLoginHistory(loginHistorySvc)),
		"GET /api/v1/account/sessions": http.HandlerFunc(
			api.AccountSessions(sessMgr)),
		"DELETE /api/v1/account/sessions/{id}": http.HandlerFunc(
			api.AccountSessionDelete(sessMgr)),
		"DELETE /api/v1/account/sessions": http.HandlerFunc(
			api.AccountSessionsDeleteAll(sessMgr)),

		// ---- Phase-12 Task 12.3.10 (+12.3.12 part 3) — emergency
		//      freeze + unfreeze request ----
		"POST /api/v1/account/emergency-freeze": http.HandlerFunc(
			api.EmergencyFreeze(emergencyFreezeSvc, true)),
		"POST /api/v1/account/unfreeze-request": http.HandlerFunc(
			api.UnfreezeRequest(unfreezeSvc)),

		// ---- Phase-14 Tasks 14.3.9/14.3.11 — closure + cooling-off ----
		// Closure carries the §12.2 second-factor session gate (spec
		// RequireTwoFactor); cooling-off activation takes the explicit
		// acknowledged:true consent inside the request body — the window
		// is irrevocable by contract (no cancel/shorten surface exists).
		"POST /api/v1/account/close": auth.RequireTwoFactor()(
			api.AccountClose(closureSvc, true)),
		"POST /api/v1/account/cooling-off": http.HandlerFunc(
			api.AccountCoolingOff(coolingSvc, true)),

		// ---- Phase-12 Task 12.3.11 — delegated logins + M-of-N ----
		"GET /api/v1/account/delegated-users": http.HandlerFunc(
			api.DelegatedUsersList(delegSvc)),
		"POST /api/v1/account/delegated-users": http.HandlerFunc(
			api.DelegatedUserCreate(delegSvc)),
		"PUT /api/v1/account/delegated-users/{id}": http.HandlerFunc(
			api.DelegatedUserUpdate(delegSvc)),
		"DELETE /api/v1/account/delegated-users/{id}": http.HandlerFunc(
			api.DelegatedUserRevoke(delegSvc)),
		"POST /api/v1/account/delegated-users/revoke-all": http.HandlerFunc(
			api.DelegatedUsersRevokeAll(delegSvc)),
		"GET /api/v1/account/approval-policies": http.HandlerFunc(
			api.ApprovalPoliciesList(delegSvc)),
		"PUT /api/v1/account/approval-policies": http.HandlerFunc(
			api.ApprovalPolicySet(delegSvc)),
		"DELETE /api/v1/account/approval-policies/{id}": http.HandlerFunc(
			api.ApprovalPolicyDisable(delegSvc)),
		"GET /api/v1/account/approval-requests": http.HandlerFunc(
			api.ApprovalRequestsList(delegSvc)),
		"POST /api/v1/account/approval-requests/{id}/decide": http.HandlerFunc(
			api.ApprovalRequestDecide(delegSvc)),

		// ---- Phase-12 Task 12.3.3 — profile & account API keys ----
		// Key creation carries the §12.2 second-factor session gate;
		// the account surface shares the developer-portal key-store
		// implementation (already claims-account-scoped).
		"GET /api/v1/account/profile":          http.HandlerFunc(api.AccountProfileGet(userStore)),
		"PUT /api/v1/account/profile":          http.HandlerFunc(api.AccountProfileUpdate(userStore)),
		"POST /api/v1/account/change-password": http.HandlerFunc(api.AccountChangePassword(authnSvc)),
		"GET /api/v1/account/api-keys":         http.HandlerFunc(acctKeyList),
		"POST /api/v1/account/api-keys": auth.RequireTwoFactor()(
			http.HandlerFunc(acctKeyCreate)),
		"DELETE /api/v1/account/api-keys/{id}": http.HandlerFunc(acctKeyRevoke),
		// Task 5.3.17 webhooks.
		"POST /api/v1/webhooks":                    http.HandlerFunc(whRegister),
		"GET /api/v1/webhooks":                     http.HandlerFunc(whList),
		"DELETE /api/v1/webhooks/{id}":             http.HandlerFunc(whDisable),
		"POST /api/v1/webhooks/{id}/rotate-secret": http.HandlerFunc(whRotate),
		"GET /api/v1/webhooks/{id}/deliveries":     http.HandlerFunc(whDeliveries),
		// Task 5.3.19 tax reporting (phase + canonical client path).
		"GET /api/v1/tax/report":         http.HandlerFunc(api.TaxReport(taxSvc)),
		"GET /api/v1/account/tax-report": http.HandlerFunc(api.TaxReport(taxSvc)),
		// Phase-12 Tasks 12.3.4/12.3.13 — KYC intake + ops matrix +
		// tax self-certification (approve/reject live below — Task 14.3.4).
		"POST /api/v1/kyc/submit":             http.HandlerFunc(api.KYCSubmit(kycSvc)),
		"GET /api/v1/kyc/status":              http.HandlerFunc(api.KYCStatus(kycSvc)),
		"GET /api/v1/kyc/requirements":        http.HandlerFunc(api.KYCRequirements(kycSvc)),
		"POST /api/v1/kyc/self-certification": http.HandlerFunc(api.KYCSelfCertSubmit(kycSvc)),
		"GET /api/v1/kyc/self-certification":  http.HandlerFunc(api.KYCSelfCertList(kycSvc)),
		// Phase-14 Task 14.3.4 — KYC lifecycle: Compliance-Officer
		// approve/reject (tier assign, reverify horizon, audit, notify).
		"POST /api/v1/admin/kyc/{id}/approve": http.HandlerFunc(api.KYCApproveHandler(lifecycleSvc, true)),
		"POST /api/v1/admin/kyc/{id}/reject":  http.HandlerFunc(api.KYCRejectHandler(lifecycleSvc, true)),
		// Phase-14 Task 14.3.7 — MiFID II categorization: client
		// appropriateness assessment + admin category assignment.
		"POST /api/v1/account/appropriateness":            http.HandlerFunc(api.AppropriatenessSubmit(catSvc)),
		"GET /api/v1/account/appropriateness":             http.HandlerFunc(api.AppropriatenessStatus(catSvc)),
		"PUT /api/v1/admin/accounts/{id}/product-profile": http.HandlerFunc(api.AdminClientCategory(catSvc, true)),
		// Phase-14 Tasks 14.3.13/14.3.15/14.3.16 — product governance.
		// Note: the PUT product-profile route above is categorization
		// (Task 14.3.7); profile assignment mounts POST on the same path.
		"POST /api/v1/account/swap-free/request":                http.HandlerFunc(api.AccountSwapFreeRequest(swapfreeSvc)),
		"GET /api/v1/account/swap-free":                         http.HandlerFunc(api.AccountSwapFreeStatus(swapfreeSvc)),
		"POST /api/v1/admin/swap-free/{id}/approve":             http.HandlerFunc(api.AdminSwapFreeDecide(swapfreeSvc, "approve", true)),
		"POST /api/v1/admin/swap-free/{id}/reject":              http.HandlerFunc(api.AdminSwapFreeDecide(swapfreeSvc, "reject", true)),
		"POST /api/v1/admin/swap-free/{id}/revoke":              http.HandlerFunc(api.AdminSwapFreeDecide(swapfreeSvc, "revoke", true)),
		"POST /api/v1/admin/product-profiles":                   http.HandlerFunc(api.AdminProductProfileSubmit(dualSvc, "create", true)),
		"PUT /api/v1/admin/product-profiles":                    http.HandlerFunc(api.AdminProductProfileSubmit(dualSvc, "update", true)),
		"GET /api/v1/admin/product-profiles":                    http.HandlerFunc(api.AdminProductProfileList(profileSvc)),
		"POST /api/v1/admin/accounts/{id}/product-profile":      http.HandlerFunc(api.AdminAssignProductProfile(profileSvc, true)),
		"PUT /api/v1/admin/product-profiles/{id}/target-market": http.HandlerFunc(api.AdminTargetMarketUpsert(targetSvc, true)),
		"POST /api/v1/admin/product-target-markets/{id}/review": http.HandlerFunc(api.AdminTargetMarketReview(targetSvc, true)),
		"GET /api/v1/admin/product-target-markets":              http.HandlerFunc(api.AdminTargetMarketList(targetSvc, true)),
		// Task 5.3.20 deprecation policy administration + guide.
		"POST /api/v1/admin/api-deprecations": http.HandlerFunc(depAnnounce),
		"GET /api/v1/admin/api-deprecations":  http.HandlerFunc(depList),
		"GET /developer/migration":            http.HandlerFunc(api.MigrationGuide(depStore)),
		// --- Phase-09 ops surface ---
		// Task 9.3.6: deprecated-route usage telemetry for operators.
		"GET /api/v1/admin/api-deprecations/usage": http.HandlerFunc(
			api.AdminDeprecationUsage(depStore, rdb,
				api.AdminRoleResolver(adminRoleResolver))),
		// Task 9.3.7: feature-flag CRUD/toggle/rollout-step administration.
		"GET /api/v1/admin/flags":                 http.HandlerFunc(flagList),
		"POST /api/v1/admin/flags":                http.HandlerFunc(flagCreate),
		"GET /api/v1/admin/flags/{name}":          http.HandlerFunc(flagGet),
		"PUT /api/v1/admin/flags/{name}":          http.HandlerFunc(flagUpdate),
		"POST /api/v1/admin/flags/{name}":         http.HandlerFunc(flagToggle),
		"DELETE /api/v1/admin/flags/{name}":       http.HandlerFunc(flagDel),
		"POST /api/v1/admin/flags/{name}/advance": http.HandlerFunc(flagAdvance),
		// Task 9.3.8: operator-triggered cache warm.
		"POST /api/v1/admin/cache/warm": http.HandlerFunc(api.AdminCacheWarm(warmer)),
		// Task 9.3.25: public status + incidents, admin ops-health export.
		"GET /api/v1/system/status":    http.HandlerFunc(api.SystemStatus(rdb)),
		"GET /api/v1/system/incidents": http.HandlerFunc(api.SystemIncidents(pool)),
		"GET /api/v1/admin/ops/health": http.HandlerFunc(
			api.AdminOpsHealth(&api.OpsExportDeps{
				Pool: pool, RDB: rdb,
				Resolver:  api.AdminRoleResolver(adminRoleResolver),
				ShedStats: shedder.Stats,
			})),
		// R9 health schema (Task 5.3.28 item 6): /health/live is always-ok
		// liveness; /health/ready (Task 7.3.6) probes PostgreSQL, Redis,
		// NATS (optional — ledger dispatch is fail-operational) and every
		// engine IPC ring producer, reporting per-dependency detail +
		// 503 when a required dependency or the mode is down. /health and
		// /ready are the Task 7.3.6 aliases.
		"GET /health/live": http.HandlerFunc(api.Health),
		"GET /health":      http.HandlerFunc(api.Health),
		"GET /health/ready": middleware.ReadyGate(drainFlag, router.WriteError,
			http.HandlerFunc(readiness(rdb, pool, natsClient, orderSubmitter, shardMap, apiVersion))),
		"GET /ready": middleware.ReadyGate(drainFlag, router.WriteError,
			http.HandlerFunc(readiness(rdb, pool, natsClient, orderSubmitter, shardMap, apiVersion))),
		// --- Phase-07 Task 7.3.3: admin audit query + verify ---
		"GET /api/v1/admin/audit":        http.HandlerFunc(api.AdminAuditLog(pool, adminRoleResolver)),
		"GET /api/v1/admin/audit-log":    http.HandlerFunc(api.AdminAuditLog(pool, adminRoleResolver)),
		"GET /api/v1/admin/audit/verify": http.HandlerFunc(api.AdminAuditVerify(pool, adminRoleResolver)),
		// --- Phase-07 Task 7.3.7: support tickets / complaints ---
		"GET /api/v1/support/tickets":                   http.HandlerFunc(api.SupportTicketList(ticketSvc)),
		"POST /api/v1/support/tickets":                  http.HandlerFunc(api.SupportTicketCreate(ticketSvc)),
		"GET /api/v1/support/tickets/{id}":              http.HandlerFunc(api.SupportTicketGet(ticketSvc)),
		"GET /api/v1/admin/support/tickets":             http.HandlerFunc(api.AdminSupportTicketList(ticketSvc)),
		"GET /api/v1/admin/support/tickets/{id}":        http.HandlerFunc(api.AdminSupportTicketGet(ticketSvc)),
		"PUT /api/v1/admin/support/tickets":             api.AdminSupportTicketUpdate(ticketSvc, true),
		"POST /api/v1/admin/support/tickets/{id}/notes": api.AdminSupportTicketNote(ticketSvc, true),
		"GET /api/v1/admin/support/complaints/register": http.HandlerFunc(api.AdminComplaintRegister(ticketSvc)),
		"GET /api/v1/admin/support/accounts/{id}":       api.AdminSupportView(supportViewSvc, adminRoleResolver, true),
		// --- Phase-13.5 Tasks 13.5.3.8/13.5.3.9: vulnerability disclosure ---
		"POST /api/v1/security/disclosures":                   http.HandlerFunc(api.VDPDisclosureSubmit(vdpSvc)),
		"GET /api/v1/security/policy":                         http.HandlerFunc(api.VDPPolicy(vdpPolicy)),
		"POST /api/v1/admin/security/disclosures/intake":      api.AdminVDPIntake(vdpSvc, true),
		"GET /api/v1/admin/security/disclosures":              http.HandlerFunc(api.AdminVDPList(vdpSvc)),
		"GET /api/v1/admin/security/disclosures/{id}":         http.HandlerFunc(api.AdminVDPGet(vdpSvc)),
		"POST /api/v1/admin/security/disclosures/{id}/triage": api.AdminVDPTriage(vdpSvc, true),
		"PUT /api/v1/admin/security/disclosures/{id}":         api.AdminVDPUpdate(vdpSvc, true),
		// --- Phase-07 Task 7.3.9: LP management ---
		"GET /api/v1/admin/liquidity-providers":                http.HandlerFunc(api.AdminLPList(lpSvc, true)),
		"POST /api/v1/admin/liquidity-providers":               http.HandlerFunc(api.AdminLPCreate(lpSvc, true)),
		"GET /api/v1/admin/liquidity-providers/{id}":           http.HandlerFunc(api.AdminLPGet(lpSvc, true)),
		"PUT /api/v1/admin/liquidity-providers":                http.HandlerFunc(api.AdminLPUpdate(lpSvc, true)),
		"PUT /api/v1/admin/liquidity-providers/{id}":           http.HandlerFunc(api.AdminLPUpdate(lpSvc, true)),
		"GET /api/v1/admin/liquidity-providers/{id}/scorecard": http.HandlerFunc(api.AdminLPScorecard(lpSvc, true)),
		"GET /api/v1/admin/liquidity-providers/{id}/alerts":    http.HandlerFunc(api.AdminLPAlerts(lpSvc, true)),
		// --- Phase-07 Tasks 7.3.13/7.3.14: governance packs ---
		"GET /api/v1/admin/governance-packs":               http.HandlerFunc(api.AdminPackList(packSvc, true)),
		"GET /api/v1/admin/governance-packs/{id}":          http.HandlerFunc(api.AdminPackGet(packSvc, true)),
		"POST /api/v1/admin/governance-packs/generate":     http.HandlerFunc(api.AdminPackGenerate(packSvc, true)),
		"POST /api/v1/admin/governance-packs/{id}/release": http.HandlerFunc(api.AdminPackRelease(packSvc, true)),
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
		// Phase-14 Task 14.3.1 — OCO pair (spec §6.2/§6.5).
		"POST /api/v1/orders/oco": http.HandlerFunc(api.OrderSubmitOCO(orderDeps)),
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
		// Task 7.3.10: DLQ review surface (registry row exists; replay/
		// discard controls live on the admin service mux + natsctl until
		// Phase-05 registers the POST routes).
		"GET /api/v1/admin/dlq": observability.DLQHandler(dlqStore),
		// Phase-07 RBAC lifecycle surface (Tasks 7.3.1/7.3.2/7.3.11/7.3.12).
		"GET /api/v1/admin/roles":     http.HandlerFunc(api.AdminRoles(rbacDeps)),
		"GET /api/v1/admin/bindings":  http.HandlerFunc(api.AdminBindings(rbacDeps)),
		"POST /api/v1/admin/bindings": http.HandlerFunc(api.AdminGrantBinding(rbacDeps)),
		"POST /api/v1/admin/bindings/{id}/revoke": http.HandlerFunc(
			api.AdminRevokeBinding(rbacDeps)),
		"GET /api/v1/admin/dual-control": http.HandlerFunc(
			api.AdminDualControlList(rbacDeps)),
		"POST /api/v1/admin/dual-control/{id}/approve": http.HandlerFunc(
			api.AdminDualControlApprove(rbacDeps)),
		"POST /api/v1/admin/dual-control/{id}/reject": http.HandlerFunc(
			api.AdminDualControlReject(rbacDeps)),
		// ---- Phase-14 Task 14.3.9/14.3.10/14.3.12 — forced closure
		//      maker (dual-control queue), compliance holds, webhook
		//      dead-letter review ----
		"POST /api/v1/admin/accounts/{id}/close": http.HandlerFunc(
			api.AdminAccountClose(dualSvc, true)),
		"POST /api/v1/admin/compliance/holds": http.HandlerFunc(
			api.AdminHoldPlace(holdSvc, true)),
		"GET /api/v1/admin/compliance/holds": http.HandlerFunc(
			api.AdminHoldList(holdSvc)),
		"POST /api/v1/admin/compliance/holds/{id}/release": http.HandlerFunc(
			api.AdminHoldRelease(holdSvc, true)),
		"POST /api/v1/admin/compliance/holds/{id}/escalate": http.HandlerFunc(
			api.AdminHoldEscalate(holdSvc, true)),
		"GET /api/v1/admin/webhooks/dead-letters": http.HandlerFunc(
			api.AdminWebhookDeadLetters(webhookStore)),
		"POST /api/v1/admin/webhooks/dead-letters/{id}/retransmit": http.HandlerFunc(
			api.AdminWebhookRetransmit(webhookStore, true)),
		"POST /api/v1/admin/recert":     http.HandlerFunc(api.AdminRecertStart(rbacDeps)),
		"GET /api/v1/admin/recert/{id}": http.HandlerFunc(api.AdminRecertReport(rbacDeps)),
		"POST /api/v1/admin/recert/{id}/decisions": http.HandlerFunc(
			api.AdminRecertDecide(rbacDeps)),
		"POST /api/v1/admin/break-glass": http.HandlerFunc(api.AdminBreakGlassGrant(rbacDeps)),
		"POST /api/v1/admin/break-glass/{id}/review": http.HandlerFunc(
			api.AdminBreakGlassReview(rbacDeps)),
		// --- Phase-09 Task 9.3.30: fleet / releases / promotion gates ---
		"GET /api/v1/admin/fleet/environments": http.HandlerFunc(
			api.AdminFleetEnvironments(fleetSvc, true)),
		"GET /api/v1/admin/fleet/hosts": http.HandlerFunc(
			api.AdminFleetHosts(fleetSvc, true)),
		"GET /api/v1/admin/fleet/topology": http.HandlerFunc(
			api.AdminFleetTopology(fleetSvc, true)),
		"POST /api/v1/admin/fleet/hosts/{id}/drain": http.HandlerFunc(
			api.AdminHostAction(fleetSvc, fleet.ActionDrain, true)),
		"POST /api/v1/admin/fleet/hosts/{id}/cordon": http.HandlerFunc(
			api.AdminHostAction(fleetSvc, fleet.ActionCordon, true)),
		"POST /api/v1/admin/fleet/hosts/{id}/decommission": http.HandlerFunc(
			api.AdminHostAction(fleetSvc, fleet.ActionDecommission, true)),
		"GET /api/v1/admin/releases": http.HandlerFunc(
			api.AdminReleaseList(fleetSvc, true)),
		"POST /api/v1/admin/releases": http.HandlerFunc(
			api.AdminReleaseCreate(fleetSvc, true)),
		"POST /api/v1/admin/releases/{id}/promote": http.HandlerFunc(
			api.AdminReleasePromote(fleetSvc, true)),
	}
	if err := router.MountSeedLive(live); err != nil {
		return fmt.Errorf("route registry: %w", err)
	}
	if err := errs.Default.ValidateEmissions(); err != nil {
		return err
	}
	log.Info("route registry mounted", "routes", len(router.Routes()),
		"error_codes", errs.Default.Len())

	// API surface, built innermost → outward so the wrap order stays
	// legible: mux → deprecation (Task 5.3.20 Sunset/410 ENDPOINT_GONE)
	// → API version negotiation (Task 5.3.28) → Idempotency (Task 5.3.42
	// — §8.8 Idempotency-Key on money-moving POSTs, Redis ledger +
	// account-scoped keys; the resolver additionally reads X-API-KEY
	// identities so signed callers get the same dedup contract) →
	// OptionalAuthMiddleware — the REST claims seam: Bearer JWTs are
	// verified and claims attached for every downstream consumer
	// (claimsAccount/adminActor handlers, the Idempotency account
	// resolver, the RBAC wrap). Anonymous requests pass through —
	// endpoint auth decisions stay with the handlers; invalid tokens
	// still reject fail-closed. The whole surface sits behind
	// DegradationGate — the enforcement half of Task 2.3.6 (ReadOnly
	// bars writes, MarketDataOnly serves only market reads, Maintenance
	// bars everything but health).
	apiSurface := middleware.DegradationGate(rdb, router.WriteError)(
		// Task 11.3.4: HTTP-layer kill-switch — while halt:global is
		// raised (or unreadable), order-admission writes reject
		// TRADING_HALTED; cancels/reads/WS pass (CANCEL_EXEMPT precedent).
		middleware.KillSwitchGate(killResolver, router.WriteError)(
			auth.OptionalAuthMiddleware(jwtIssuer, sessMgr)(
				// Task 9.3.10: staged tiered shedding inside OptionalAuth so the
				// tier resolver sees claims; cancels bypass entirely.
				middleware.Shedding(shedder, middleware.ShedOptions{
					ResolveTier: func(r *http.Request) ratelimit.Tier {
						return tierResolver(r.Context(), auth.ClaimsFrom(r.Context()))
					},
					Emit: router.WriteError,
				})(
					// Task 9.3.23: during drain new non-cancel work rejects
					// 503; in-flight requests finish inside srv.Shutdown.
					middleware.RejectWhenDraining(drainFlag, router.WriteError)(
						middleware.Idempotency(
							middleware.NewRedisIdemStore(rdb.Client),
							nil, idemResolver(keyStore), router.WriteError)(
							middleware.APIVersion(middleware.VersionConfig{
								Versions: map[int]string{1: apiVersion},
								Emit:     router.WriteError,
							})(
								// Task 9.3.6: headers/410 unchanged; the Redis
								// sink records per-key usage telemetry.
								deprecation.MiddlewareWithTelemetry(depRules,
									func(w http.ResponseWriter, req *http.Request, _ int, code, msg string) {
										router.WriteError(w, req, code, msg, nil)
									}, nil,
									deprecation.RedisHitSink(rdb.Client))(mux))))))))

	srv := &http.Server{
		Addr: cfg.Gateway.Addr(),
		// Task 2.3.6: X-Degradation-Mode on every response — outermost so
		// even early middleware rejections carry the header. Reads the same
		// coordination client the shard map uses; read failures report
		// Maintenance (fail-closed, spec §2.7). Inside it: request-id
		// stamping + panic recovery (Task 5.3.41), request logging,
		// SecurityHeaders (Task 5.3.29 item 9 — HAProxy does NOT emit
		// these; the gateway is the single emitter), Tracing (item 2 W3C
		// traceparent continuation), then the tiered rate limiter +
		// progressive IP-ban gate (Tasks 5.3.2/5.3.27/5.3.34) in front of
		// the apiSurface chain above. Rejections emit the registry-gated
		// §8.7 envelope via router.WriteError.
		Handler: met.HTTPMiddleware(
			middleware.DegradationModeHeader(
				rdb, middleware.SecurityHeaders(securityOpts(cfg),
					gateway.RequestID(router.Recover(
						func(f string, a ...any) { log.Error(fmt.Sprintf(f, a...)) },
						middleware.Tracing(
							tracing.Middleware(tracer)(
								middleware.Logging(log,
									middleware.RateLimit(limiter, middleware.RateLimitOptions{
										ResolveTier: tierResolver,
										Emit:        router.WriteError,
										// Public-tier IP bucketing trusts XFF
										// only when the gateway is known to sit
										// behind HAProxy (EXC_TRUST_PROXY=1,
										// same convention as cmd/marketdata) —
										// a spoofable source IP would defeat
										// the Task 5.3.34 ban machinery.
										TrustProxy: os.Getenv("EXC_TRUST_PROXY") == "1",
									})(apiSurface))))))))),
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
		// Task 9.3.23 ordering: readiness flips unhealthy immediately
		// (Kubernetes stops routing new traffic while the PreStop window
		// elapses), new work rejects 503, WS clients get the reconnect
		// advisory, in-flight HTTP drains under the 30s budget, then
		// background loops stop via sweepStop.
		drainFlag.Set()
		err := middleware.RunSteps(context.Background(), log, []middleware.Step{
			{Name: "ws_drain", Timeout: 15 * time.Second, Fn: func(c context.Context) error {
				return wsSrv.Drain(c, ws.DrainAdvisory{
					Reason:     "maintenance",
					RetryAfter: 2 * time.Second,
				})
			}},
			{Name: "http_drain", Timeout: 30 * time.Second, Fn: srv.Shutdown},
		})
		sweepStop()
		return err
	case err := <-serveErr:
		return err
	}
}

// ---------------------------------------------------------------------------
// Cluster E helpers (Tasks 5.3.26/29/30/31/42) + Task 7.3.6 readiness
// ---------------------------------------------------------------------------

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

// pnlPublisherFunc adapts a closure to risk.PnlPublisher — the ws hub
// is late-bound (constructed after the order consumer starts), so the
// closure checks wsSrv at publish time rather than capturing a nil.
type pnlPublisherFunc func(accountID int64, channel string, data any)

// PublishPrivate implements risk.PnlPublisher.
func (f pnlPublisherFunc) PublishPrivate(accountID int64, channel string, data any) {
	f(accountID, channel, data)
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
