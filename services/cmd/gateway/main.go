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
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go/jetstream"

	"exchange/internal/accounts"
	"exchange/internal/admin"
	"exchange/internal/algo"
	"exchange/internal/analytics"
	"exchange/internal/api"
	"exchange/internal/auth"
	"exchange/internal/backoffice"
	"exchange/internal/bots"
	"exchange/internal/cache"
	"exchange/internal/compliance"
	regreport "exchange/internal/compliance/reporting"
	"exchange/internal/compliance/venue"
	"exchange/internal/config"
	"exchange/internal/content"
	"exchange/internal/copy"
	"exchange/internal/db"
	"exchange/internal/delegation"
	"exchange/internal/demo"
	"exchange/internal/deprecation"
	"exchange/internal/errs"
	"exchange/internal/fix"
	"exchange/internal/flags"
	"exchange/internal/fleet"
	"exchange/internal/funding"
	"exchange/internal/gateway"
	"exchange/internal/instruments"
	"exchange/internal/ipc"
	excmargin "exchange/internal/margin"
	"exchange/internal/marketapi"
	"exchange/internal/marketdata"
	"exchange/internal/marketmaking"
	"exchange/internal/middleware"
	"exchange/internal/nats"
	"exchange/internal/notifications"
	"exchange/internal/objectstore"
	"exchange/internal/observability"
	"exchange/internal/ops"
	"exchange/internal/oracle"
	"exchange/internal/oracle/rates"
	"exchange/internal/orders"
	"exchange/internal/pamm"
	"exchange/internal/position"
	"exchange/internal/promos"
	"exchange/internal/ratelimit"
	"exchange/internal/reconciliation"
	"exchange/internal/redis"
	"exchange/internal/reporting"
	"exchange/internal/risk"
	"exchange/internal/security"
	"exchange/internal/settlement"
	"exchange/internal/strategies"
	"exchange/internal/support"
	"exchange/internal/tax"
	"exchange/internal/testenv"
	"exchange/internal/timesync"
	"exchange/internal/tracing"
	"exchange/internal/utils"
	"exchange/internal/webhooks"
	"exchange/internal/ws"
	"exchange/pkg/decimal"
	excerrors "exchange/pkg/errors"
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
		// Own the canonical stream set at boot — idempotent
		// CreateOrUpdate, so every environment converges without a
		// separate provisioning step (Phase-01 Task 1.3.11).
		ensCtx, ensCancel := context.WithTimeout(context.Background(), 30*time.Second)
		if _, serr := nc.EnsureStreams(ensCtx); serr != nil {
			log.Warn("nats stream ensure failed — publishers may reject until reconciled", "err", serr)
		}
		ensCancel()
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
	// Phase-08.5 Task 8.5.3.2 — demo / paper trading environment
	// (§24 #266). Enabled only when the deployment label is "demo":
	// registration then mints DEMO accounts seeded with virtual USD
	// (EXC_DEMO_BALANCE_USD, default 100000). fundChecker keeps the
	// freeze semantics and adds the fail-closed demo rejection — real
	// banking rails are unreachable from DEMO accounts on EVERY
	// funding/pool-facing AssertMutable call site below.
	demoSvc := demo.New(pool, cfg.Environment).
		WithLogger(func(f string, a ...any) { log.Warn(fmt.Sprintf("demo: "+f, a...)) })
	if v := strings.TrimSpace(os.Getenv("EXC_DEMO_BALANCE_USD")); v != "" {
		if d, derr := decimal.NewFromString(v); derr == nil && d.IsPositive() {
			demoSvc.WithInitialBalance(d)
		} else {
			log.Warn("EXC_DEMO_BALANCE_USD invalid — keeping default",
				"value", v)
		}
	}
	fundChecker := demoSvc.WrapFundingChecker(freezeSvc)
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
	// Phase-21 sanctions/screening components share the same ops-alert
	// adapter (compliance.Alerter has the identical signature).
	compAlerter := compliance.Alerter(func(ctx context.Context,
		severity, code, summary string) error {
		if opsAlerter == nil {
			return nil
		}
		return opsAlerter.Raise(ctx, settlement.OpsAlert{
			Severity: severity, Code: code, Summary: summary})
	})
	// Sweep-loop context — created early so the Phase-21 sanctions
	// cluster (heartbeat/refresher/replay) can bind its goroutines
	// alongside the funding/KYC sweeps below.
	sweepCtx, sweepStop := context.WithCancel(context.Background())
	defer sweepStop()
	// Phase-21 Task 21.3.11 — ongoing transaction monitoring. Reads the
	// funding/transfer flow tables (PgActivitySource, read-only) and
	// routes findings to audit-backed cases; the funding Notifier seam
	// taps terminal events below (post-commit, best-effort).
	monSvc, err := compliance.NewMonitoringService(compliance.MonitoringOptions{
		Src:     compliance.NewPgActivitySource(pool),
		Cases:   compliance.AuditCaseSink{Auditor: compliance.AdminAuditSink{Pool: pool}},
		Alerter: compAlerter,
	})
	if err != nil {
		return fmt.Errorf("monitoring service: %w", err)
	}
	// Hourly dormant-reactivation sweep.
	go func() {
		t := time.NewTicker(time.Hour)
		defer t.Stop()
		for {
			select {
			case <-sweepCtx.Done():
				return
			case <-t.C:
				if n, derr := monSvc.SweepDormant(sweepCtx, 500); derr != nil {
					log.Warn("dormant-reactivation sweep failed", "err", derr)
				} else if n > 0 {
					log.Warn("dormant accounts reactivated", "count", n)
				}
			}
		}
	}()
	// Phase-13 Task 13.3.6 — MiFID II RTS 9 order-to-trade ratio monitor:
	// order events (new/modify/cancel) and fills slide through Redis
	// zset windows (otr:events|otr:trades:{account}:{symbol}); a breach
	// (events > ratio × max(trades,1), defaults 500 / 60s — migration
	// 047) raises otr:breach:{account}, rejects new orders with
	// OTR_LIMIT_EXCEEDED while permitting cancels, and pages P2. The 1s
	// sweeper clears the flag when every active pair decays under its
	// limit; the C++ SuspensionRefresher mirrors the same keyspace into
	// PreTradeChecker as the engine-side backstop. Market-maker
	// allowance = mm_programs.otr_allowance (migration 045, Phase-18
	// Task 18.3.10) via the WithMMAllowance seam — it applies ONLY over
	// the venue default; an explicitly scoped risk_limits row still
	// wins (supersedes the prior "MM allowance = higher-ratio
	// risk_limits row" mechanism note).
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
	// Phase-18 Task 18.3.10 — market-maker program (migration 045):
	// program enrollment/entitlement, per-minute obligation compliance,
	// MMP lockout tracking and GL-reconciled rebates. The ACTIVE-program
	// snapshot feeds the OTR allowance lookup (no per-event PG reads);
	// rebate posting rides the same ledgerSvc JournalPoster seam as the
	// funding fee service (zero GL-bypass invariant).
	mmSvc := marketmaking.NewService(marketmaking.NewPgStore(pool), marketmaking.Options{
		Poster: ledgerSvc,
		Alerts: otrSinks,
		Logger: func(f string, a ...any) { log.Warn(fmt.Sprintf(f, a...)) },
	})
	if err := mmSvc.Load(context.Background()); err != nil {
		// Boot continues: entitlement reads fall through to PG
		// (fail-closed) and the OTR allowance is simply absent until the
		// refresher lands a snapshot.
		log.Warn("mm programs initial load failed", "err", err)
	}
	mmSvc.StartRefresher(context.Background(), 60*time.Second,
		func(e error) { log.Warn("mm programs refresh failed", "err", e) })
	otrMon.WithMMAllowance(mmSvc)
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
	sanctionsDir := strings.TrimSpace(os.Getenv("EXC_SANCTIONS_LIST_DIR"))
	if sanctionsDir != "" {
		screener, err = compliance.NewListScreener(sanctionsDir)
		if err != nil {
			return fmt.Errorf("sanctions screener: %w", err)
		}
		lists, n, _ := screener.Stats()
		log.Info("sanctions screener loaded",
			"dir", sanctionsDir, "lists", lists, "entries", n)
	} else {
		log.Warn("EXC_SANCTIONS_LIST_DIR unset — sanctions seam unwired; " +
			"STANDARD-tier funding reviews fail closed to PENDING_REVIEW")
	}
	// Phase-21 Tasks 21.3.1/21.3.11/21.3.23 — provider-outage
	// quarantine, pending-screen queue, C++-hook flag publisher and the
	// scheduled vendor refresh. Funding binds the QUARANTINED wrapper —
	// an outage parks must-screen flows in the queue instead of passing.
	var (
		sanctionsGate  *compliance.ProviderGate
		sanctionsQueue compliance.ScreenQueue
		sanctionsFlags = compliance.NewFlagPublisher(rdb).
				WithLogger(func(f string, a ...any) {
				log.Info(fmt.Sprintf(f, a...))
			})
		gatedScreener      *compliance.QuarantinedScreener
		sanctionsRefresher *compliance.VendorRefresher
		sanctionsReplayer  *compliance.QueueReplayer
		screeningStore     = compliance.NewPgScreeningStore(pool)
		screeningSvc       *compliance.ScreeningService // bound after holdSvc
	)
	if screener != nil {
		// Vendor feeds are env-declared (EXC_SANCTIONS_FEEDS, JSON array
		// of ListFeed) — the same names feed the provider gate's health
		// registry so a required feed's outage quarantines the scope.
		var feeds []compliance.ListFeed
		var providers []string
		if raw := strings.TrimSpace(os.Getenv("EXC_SANCTIONS_FEEDS")); raw != "" {
			if uerr := json.Unmarshal([]byte(raw), &feeds); uerr != nil {
				return fmt.Errorf("EXC_SANCTIONS_FEEDS parse: %w", uerr)
			}
			for _, f := range feeds {
				providers = append(providers, "feed:"+f.Name)
			}
		}
		sanctionsQueue = compliance.NewRedisScreenQueue(rdb)
		sanctionsGate = compliance.NewProviderGate(providers).
			WithAlerter(compAlerter).
			WithAuditor(compliance.AdminAuditSink{Pool: pool})
		screener.WithProviderGate(sanctionsGate)
		gatedScreener = compliance.NewQuarantinedScreener(
			screener, sanctionsGate, sanctionsQueue)
		sanctionsReplayer = compliance.NewQueueReplayer(sanctionsQueue, screener).
			WithFlags(sanctionsFlags).
			WithAlerter(compAlerter).
			WithAuditor(compliance.AdminAuditSink{Pool: pool})
		sanctionsGate.OnRecover(func(ctx context.Context) {
			if rep, rerr := sanctionsReplayer.Replay(ctx); rerr != nil {
				log.Error("sanctions queue replay failed", "err", rerr)
			} else if rep.Claimed > 0 {
				log.Info("sanctions queue replayed",
					"claimed", rep.Claimed, "hits", rep.HitCount,
					"backlog", rep.Backlog)
			}
		})
		if len(feeds) > 0 {
			sanctionsRefresher, err = compliance.NewVendorRefresher(
				screener, sanctionsDir, feeds, nil)
			if err != nil {
				return fmt.Errorf("sanctions refresher: %w", err)
			}
			sanctionsRefresher.WithGate(sanctionsGate).
				WithAlerter(compAlerter).
				WithAuditor(compliance.AdminAuditSink{Pool: pool})
			go sanctionsRefresher.Run(sweepCtx)
		} else {
			log.Warn("EXC_SANCTIONS_FEEDS unset — vendor refresh unbound; " +
				"list reloads remain operator-driven")
		}
		go sanctionsFlags.HeartbeatLoop(sweepCtx)
	}
	withdrawalSvc, err := funding.NewWithdrawalService(fundStore, ledgerSvc, fundChecker)
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
	if gatedScreener != nil {
		withdrawalSvc.WithSanctions(gatedScreener)
	}
	transferSvc, err := funding.NewTransferService(fundStore, ledgerSvc, fundChecker)
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
	// (sweepCtx/sweepStop are created above — Phase-21 sanctions
	// cluster goroutines share them.)
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
	// Task 20.3.10 — the daily generation cap (5 reports per account per
	// UTC day) rides the coordination Redis via an atomic INCR+EXPIRE
	// Lua script; a Redis outage fails closed (503 at the handler, never
	// an uncapped report flood).
	taxLimiter := tax.NewRedisDailyLimiter(rdb.Client, tax.TaxReportsPerDay)

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
	pammSvc, err := pamm.NewService(pammStore, ledgerSvc, fundChecker)
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
	depositSvc, err := funding.NewDepositService(fundStore, ledgerSvc, fundChecker)
	if err != nil {
		return fmt.Errorf("deposit service: %w", err)
	}
	depositSvc.WithUSDConverter(usdConv).WithAlerter(opsAlerter).
		WithLogger(func(f string, a ...any) { log.Error(fmt.Sprintf(f, a...)) })
	if gatedScreener != nil {
		depositSvc.WithSanctions(gatedScreener)
	}
	flowSvc, err := funding.NewFlowService(withdrawalSvc, fundStore)
	if err != nil {
		return fmt.Errorf("withdrawal flow service: %w", err)
	}
	flowSvc.WithTOTP(accounts.NewPgxTOTPSecrets(pool, secretBox), auth.VerifyTOTP).
		WithDispatcher(dispatchSvc).
		WithLogger(func(f string, a ...any) { log.Error(fmt.Sprintf(f, a...)) })

	// ---- Phase-21 Tasks 21.3.2/21.3.3/21.3.6 — AML core ----
	// Constructed before the flows sweep starts so no CONFIRMED
	// withdrawal or dual-source deposit resolves without the FATF
	// travel-rule gate: outbound wires >= $1,000 carry the
	// originator/beneficiary record into MT103 50K/59 or hold in the
	// dispatch queue; inbound wires missing the data park in
	// PENDING_REVIEW. sarSvc also feeds hold escalation, signal ingest
	// and the AML scoring pipeline below.
	travelSvc, err := compliance.NewTravelRuleService(pool)
	if err != nil {
		return fmt.Errorf("travel rule service: %w", err)
	}
	sarSvc, err := compliance.NewSARService(pool,
		compliance.HoldRoleResolver(adminRoleResolver))
	if err != nil {
		return fmt.Errorf("sar service: %w", err)
	}
	amlSvc, err := compliance.NewAMLService(pool, sarSvc)
	if err != nil {
		return fmt.Errorf("aml service: %w", err)
	}
	amlSvc.WithAlerter(holdOpsAlerter{inner: freezeOpsAlerter{pool: pool, page: opsAlerter}})
	dispatchSvc.WithTravelRule(travelSvc)
	depositSvc.WithTravelRule(travelSvc)
	// ---- end Phase-21 AML core ----

	// ---- Phase-21 Tasks 21.3.4/.5/.9/.14/.16 — regulatory transaction/
	// trade reporting cluster ----
	// Canonical event store + version-pinned schema registry (migration
	// 054, spec §5.32/§14.1a), transport ledger (059, §5.36), the MiFID
	// II RTS 22 / EMIR REFIT / CFTC Parts 43-45 regime adapters and the
	// APA/ARM/TR/SDR dispatch pump. Reportable events arrive from the
	// `trades` and `settlements` JetStream streams via the independent
	// durable consumers provisioned below (LimitsPolicy fan-out,
	// explicit-ack at-least-once — never WorkQueue).
	regStore, err := regreport.NewPgStore(pool)
	if err != nil {
		return fmt.Errorf("regulatory reporting store: %w", err)
	}
	regCfg := regreport.ConfigFromEnv()
	regCfg.Alerter = regreport.AlertFunc(func(ctx context.Context, sev, code, summary string) error {
		if opsAlerter == nil {
			return nil
		}
		return opsAlerter.Raise(ctx, settlement.OpsAlert{
			Severity: sev, Code: code, Summary: summary})
	})
	regSvc, err := regreport.NewService(regStore, regCfg)
	if err != nil {
		return fmt.Errorf("regulatory reporting service: %w", err)
	}
	mifidRep, err := compliance.NewMiFIDReporter(regSvc)
	if err != nil {
		return fmt.Errorf("mifid reporter: %w", err)
	}
	emirRep, err := compliance.NewEMIRReporter(regSvc,
		compliance.ForwardPointsOracle(rates.NewStore(rdb.Client).SwapPointFor))
	if err != nil {
		return fmt.Errorf("emir reporter: %w", err)
	}
	// EMIR REFIT NEWTs carry required valuation/margin/notional — the
	// enrich hook prices them off the Phase-19.5 forward-points oracle.
	// A stale/missing quote fails closed (consumer NAK → redelivery);
	// nothing is fabricated.
	regSvc.Cfg.DerivativeEnrich = emirRep.EnrichNEWT
	dfRep, err := compliance.NewDoddFrankReporter(regSvc)
	if err != nil {
		return fmt.Errorf("dodd-frank reporter: %w", err)
	}
	regLedger, err := compliance.NewSubmissionsLedger(pool)
	if err != nil {
		return fmt.Errorf("regulatory submissions ledger: %w", err)
	}
	// Vendor endpoints come from env (EXC_APA_URL / EXC_ARM_URL /
	// EXC_TR_URL / EXC_SDR_URL). An absent client is fail-closed: the
	// dispatcher ledger-marks ENDPOINT_UNCONFIGURED and pages P1 — a
	// report is never silently dropped or claimed sent.
	regClients := map[regreport.Destination]compliance.VendorClient{}
	if c, cerr := compliance.APAClientFromEnv(); cerr == nil {
		regClients[regreport.DestinationAPA] = c
	} else {
		log.Warn("APA endpoint unconfigured — transparency submissions park", "err", cerr)
	}
	if c, cerr := compliance.ARMClientFromEnv(); cerr == nil {
		regClients[regreport.DestinationARM] = c
	} else {
		log.Warn("ARM endpoint unconfigured — RTS 22 submissions park", "err", cerr)
	}
	if c, cerr := compliance.TRClientFromEnv(); cerr == nil {
		regClients[regreport.DestinationTR] = c
	} else {
		log.Warn("TR endpoint unconfigured — EMIR submissions park", "err", cerr)
	}
	if c, cerr := compliance.SDRClientFromEnv(); cerr == nil {
		regClients[regreport.DestinationSDR] = c
	} else {
		log.Warn("SDR endpoint unconfigured — CFTC submissions park", "err", cerr)
	}
	regDispatch := &compliance.SubmissionDispatcher{
		Svc: regSvc, Ledger: regLedger, Clients: regClients,
		Alert: regCfg.Alerter,
	}
	// Dispatch sweep every 5s + daily reconciliation passes for the
	// derivative regimes (spec §14.1a — zero unexplained divergence is
	// the acceptance state; breaks surface in the repair queue).
	go func() {
		t := time.NewTicker(5 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-sweepCtx.Done():
				return
			case <-t.C:
				if n, derr := regDispatch.DispatchOnce(sweepCtx, 100); derr != nil {
					log.Warn("regulatory dispatch sweep failed", "err", derr)
				} else if n > 0 {
					log.Info("regulatory artifacts dispatched", "count", n)
				}
			}
		}
	}()
	go func() {
		t := time.NewTicker(24 * time.Hour)
		defer t.Stop()
		for {
			select {
			case <-sweepCtx.Done():
				return
			case <-t.C:
				for _, regime := range []regreport.Regime{
					regreport.RegimeEMIRREFIT, regreport.RegimeCFTCP45} {
					if rep, rerr := regSvc.Reconcile(sweepCtx, regime); rerr != nil {
						log.Warn("regulatory reconcile failed", "regime", regime, "err", rerr)
					} else {
						log.Info("regulatory reconcile pass", "regime", rep.Regime,
							"checked", rep.CheckedEvents, "open", rep.OpenInternal,
							"breaks_opened", len(rep.BreaksOpened))
					}
				}
			}
		}
	}()
	// Durable consumers — independent cursors on `trades` and
	// `settlements` (at-least-once; replay is absorbed by UTI
	// idempotency + the (uti, regime, report_seq) collision key).
	if natsClient != nil {
		execCons, errc := regreport.NewExecutionConsumer(regSvc)
		if errc == nil {
			if cons, cerr := natsClient.EnsureConsumer(context.Background(), "trades",
				regreport.DurableTrades, nats.WithFilterSubject("trades.>")); cerr != nil {
				log.Warn("regulatory trades consumer unavailable", "err", cerr)
			} else {
				go func() {
					if cerr := execCons.Consume(sweepCtx, cons); cerr != nil {
						log.Error("regulatory trades consumer stopped", "err", cerr)
					}
				}()
				log.Info("regulatory reporting consuming", "stream", "trades",
					"durable", regreport.DurableTrades)
			}
		}
		settleCons, errs := regreport.NewSettlementConsumer(regSvc)
		if errs == nil {
			if cons, cerr := natsClient.EnsureConsumer(context.Background(), "settlements",
				regreport.DurableSettlements, nats.WithFilterSubject("settlements.>")); cerr != nil {
				log.Warn("regulatory settlements consumer unavailable", "err", cerr)
			} else {
				go func() {
					if cerr := settleCons.Consume(sweepCtx, cons); cerr != nil {
						log.Error("regulatory settlements consumer stopped", "err", cerr)
					}
				}()
				log.Info("regulatory reporting consuming", "stream", "settlements",
					"durable", regreport.DurableSettlements)
			}
		}
	} else {
		log.Warn("NATS unavailable — regulatory reporting consumers deferred")
	}
	regDeps := api.RegReportingDeps{
		Svc: regSvc, Ledger: regLedger,
		MiFID: mifidRep, EMIR: emirRep, DoddFrank: dfRep,
		TrustProxy: true,
	}
	// ---- end Phase-21 regulatory reporting ----

	// ---- Phase-21 wave-2 governance cluster: Basel III capital pack
	// (21.3.13), FX Global Code 55-principle review (21.3.17),
	// regulatory-change watch register (21.3.25), execution-policy
	// lifecycle + order-entry consent gate (21.3.28) ----
	baselSvc, err := compliance.NewBaselService(pool)
	if err != nil {
		return fmt.Errorf("basel service: %w", err)
	}
	baselSvc.WithAlerter(holdOpsAlerter{inner: freezeOpsAlerter{pool: pool, page: opsAlerter}}).
		WithRateConverter(func(ctx context.Context, ccy string) (decimal.Decimal, error) {
			return usdConv.ToUSD(ctx, ccy, decimal.NewFromInt(1))
		})

	fxgcSvc, err := compliance.NewFXGCService(pool,
		compliance.HoldRoleResolver(adminRoleResolver))
	if err != nil {
		return fmt.Errorf("fx global code service: %w", err)
	}
	fxgcSvc.WithAlerter(holdOpsAlerter{inner: freezeOpsAlerter{pool: pool, page: opsAlerter}}).
		WithClockEvidence(func(ctx context.Context) (int64, bool, error) {
			// P10 probe reads the deploy-role PTP status file —
			// grandmaster-synced AND |offset| ≤ 1µs is adherence.
			rd, err := timesync.StatsFileReader{
				Path: timesync.DefaultPTPStatusPath}.Read(ctx)
			if err != nil {
				return 0, false, err
			}
			off := rd.OffsetNs
			if off < 0 {
				off = -off
			}
			return off, rd.Synced, nil
		})

	regChangeSvc, err := compliance.NewRegChangeService(pool,
		compliance.HoldRoleResolver(adminRoleResolver))
	if err != nil {
		return fmt.Errorf("regulatory change service: %w", err)
	}
	regChangeSvc.WithAlerter(holdOpsAlerter{inner: freezeOpsAlerter{pool: pool, page: opsAlerter}})
	// The 10-business-day triage SLA counts on the venue's core
	// regulatory calendar — the Fed / TARGET2 / BoE settlement holiday
	// union already maintained for Task 3.3.8.
	if hrows, herr := pool.Query(context.Background(), `
		SELECT DISTINCT holiday_date FROM currency_holidays
		WHERE currency IN ('USD','EUR','GBP')`); herr == nil {
		var hols []time.Time
		for hrows.Next() {
			var d time.Time
			if hrows.Scan(&d) == nil {
				hols = append(hols, d)
			}
		}
		hrows.Close()
		regChangeSvc.WithHolidays(hols)
	} else {
		log.Warn("regulatory holiday calendar unavailable — weekday-only SLA", "err", herr)
	}

	execPolicySvc, err := compliance.NewExecutionPolicyService(pool,
		compliance.HoldRoleResolver(adminRoleResolver))
	if err != nil {
		return fmt.Errorf("execution policy service: %w", err)
	}
	execPolicySvc.WithAlerter(holdOpsAlerter{inner: freezeOpsAlerter{pool: pool, page: opsAlerter}})

	// Task 21.3.28 — the consent gate composes behind the profile /
	// target-market gate: every non-reduce-only admission also needs a
	// consent row for the ACTIVE policy version (PRODUCT_NOT_PERMITTED
	// refusal; probe failure fails closed).
	consentGate := &compliance.PolicyConsentGate{
		Inner: productGate, Policy: execPolicySvc}

	// Basel III EOD snapshot — daily 03:00 UTC (after the Phase-20
	// 02:00 RTS28 slot), idempotent on 'eod:{period}'; breach pages
	// fire inside Snapshot → raiseBreaches.
	runDailyUTC(sweepCtx, log, "basel-eod", 180, func(ctx context.Context) {
		if rep, created, err := baselSvc.RunEOD(ctx); err != nil {
			log.Warn("basel EOD snapshot failed", "err", err)
		} else if created {
			log.Info("basel EOD snapshot stored",
				"period", rep.Period.Format("2006-01-02"),
				"car", rep.CAR.String(), "leverage", rep.LeverageRatio.String(),
				"car_breach", rep.CARBreach, "lev_breach", rep.LeverageBreach,
				"inputs_complete", rep.InputsComplete)
		}
	})
	// Regulatory-change SLA + execution-policy review: hourly — the
	// 10-business-day triage breach (RULEBOOK_VERSION_STALE P1), the
	// 90-day effective-date window (REGULATORY_DEADLINE_APPROACHING)
	// and the overdue annual-review freeze each dedup on WORM audit
	// markers, so rerunning is safe.
	go func() {
		t := time.NewTicker(time.Hour)
		defer t.Stop()
		for {
			select {
			case <-sweepCtx.Done():
				return
			case <-t.C:
				if sla, dl, err := regChangeSvc.Sweep(sweepCtx); err != nil {
					log.Warn("regulatory change sweep failed", "err", err)
				} else if sla+dl > 0 {
					log.Info("regulatory change alerts raised",
						"triage_sla", sla, "deadline", dl)
				}
				if n, err := execPolicySvc.SweepOverdueReview(sweepCtx); err != nil {
					log.Warn("execution-policy review sweep failed", "err", err)
				} else if n > 0 {
					log.Info("execution-policy overdue review paged", "flagged", n)
				}
			}
		}
	}()
	// Task 21.3.15 — regulated-venue governance: member/DEA register +
	// admission gate, rulebook/product versioning, market-control record,
	// cases/conflicts, self-assessment/CCO report, launch prerequisites.
	// The admission gate wraps the product/consent chain — a member's
	// trading access fails closed on absent due diligence, agreements,
	// admission, review currency or jurisdiction licensing.
	venueSvc, err := venue.NewService(pool,
		compliance.HoldRoleResolver(adminRoleResolver))
	if err != nil {
		return fmt.Errorf("venue governance service: %w", err)
	}
	venueSvc.WithAlerter(holdOpsAlerter{inner: freezeOpsAlerter{pool: pool, page: opsAlerter}})
	venueGate := &venue.AdmissionGate{Inner: consentGate, Members: venueSvc}
	// ---- end Phase-21 wave-2 governance cluster ----

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
	// EXC_IPC_BASE namespaces the shm rings away from the default
	// "exchange_ipc" base — test harnesses run a parallel stack without
	// colliding with a live gateway's segments.
	orderSubmitter := orders.NewShmSubmitter(os.Getenv("EXC_IPC_BASE"))
	orderSvc, err := orders.NewService(orders.Options{
		Store:      orderStore,
		Submitter:  orderSubmitter,
		ShardMap:   shardMap,
		Limits:     riskLimits,
		KillSwitch: killResolver, // Tasks 11.3.4/11.3.8 — every admission path consults it
		Otr:        otrMon,       // Task 13.3.6 — RTS 9 OTR event counting + breach gate
		Breakers:   breakerSvc,   // Tasks 13.3.1/13.3.9 — §2.6 five-tier breaker gate
		BatchRL:    orders.NewRedisBatchLimiter(rdb.Client, 0),
		Products:   venueGate, // Tasks 14.3.13/14.3.16 + 21.3.28 + 21.3.15 — venue member admission wraps the product/consent chain
		Product:    catSvc,    // Task 14.3.7 — MiFID II appropriateness gate
	})
	if err != nil {
		return fmt.Errorf("order service: %w", err)
	}

	// Phase-18 Task 18.3.10 — MMP tracker: the sliding-window fill
	// counter mirrors engine-side protection from the read-model fill
	// feed; on breach it mass-cancels the MM's resting orders on the
	// breached instrument through the canonical Task 5.3.25 scope path
	// (account+instrument, reason "mmp") and holds MMP_LOCKED_OUT until
	// the explicit reset (REST …/mmp-reset or FIX 35=a).
	mmTracker := marketmaking.NewMMPTracker(mmSvc,
		func(ctx context.Context, p *marketmaking.Program, instrumentID int64) (int, error) {
			res, err := orderSvc.MassCancel(ctx, orders.MassCancelScope{
				AccountID:    p.AccountID,
				InstrumentID: instrumentID,
				Reason:       "mmp",
			}, "system:mmp", "", "")
			if err != nil {
				return 0, err
			}
			return res.Cancelled, nil
		}).WithAlerts(otrSinks).
		WithLogger(func(f string, a ...any) { log.Warn(fmt.Sprintf(f, a...)) })

	// ---- Phase-16 advanced-order seams ----
	// GSLO (Task 16.3.16): premium debit/refund journals + the
	// per-instrument gap-liability cap + insurance-fund gap absorption —
	// all through ledgerSvc (never direct balance writes). A nil seam
	// would fail gslo submissions closed, so wire unconditionally.
	gsloSvc := algo.NewGSLOService(pool, ledgerSvc, ledgerPub)
	orderSvc.WithGSLO(gsloSvc)
	// Task 16.3.22 conditional-trigger + pegged admission guards:
	// MARK_PRICE/INDEX_PRICE freshness fails closed
	// (CONDITIONAL_TRIGGER_ORACLE_STALE) until the Phase-19.5 oracle
	// binds the feed seam; pegged orders need a viable BBO
	// (PEGGED_PRICING_UNAVAILABLE).
	orderSvc.WithConditional(algo.NewTriggerGuard(pool, nil, nil))
	// Composite persistence (Tasks 16.3.14/.20) — PgStore satisfies the
	// seam once migrations 075/225 apply.
	orderSvc.WithComposite(orderStore)

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
	copySvc, err := copy.NewService(copyStore, catSvc, fundChecker,
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
	// Task 3.3.4 + Task 3.3.13 fee/commission wiring — the resolver
	// computes the per-side trading fee (delivery currency) and the
	// raw-model commission at resolve; FEE/commission journals post
	// inside the fill commit and the trades row records the recon
	// expected-fee values.
	tradeFeeSvc, ferr := settlement.NewFeeService(settlement.NewPgxFeeStore(pool), nil, "", nil)
	if ferr != nil {
		return fmt.Errorf("trade fee service: %w", ferr)
	}
	commEng, cerr := settlement.NewCommissionEngine(
		settlement.NewPgProfileFeeModelSource(pool),
		settlement.NewPgCommissionStore(pool), usdConv, nil)
	if cerr != nil {
		return fmt.Errorf("commission engine: %w", cerr)
	}
	fillCal, cerr := settlement.LoadCalendar(context.Background(), pool)
	if cerr != nil {
		log.Warn("fill resolver: holiday calendar unavailable — PD fills fail closed", "err", cerr)
	}
	tradeResolver := settlement.NewPgxTradeResolver(pool, tradeFeeSvc, commEng, fillCal)
	if natsClient != nil {
		fanout, ferr := pamm.NewTradesFanout(tradeResolver,
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

	// Phase-16 — algo order framework (Tasks 16.3.1/2/6/7/8/12/18/21).
	// Children dispatch ONLY through algoChildExecutor → orders.Service:
	// admission gates, risk checks, balance sufficiency and §8.7 dedup
	// all still apply — the framework never writes orders directly.
	// Seam bindings:
	//   Quote   — persisted-book top-of-book (orders ACTIVE rows);
	//   Ref     — last-trade reference (empty-book fallback);
	//   Pips    — instruments.pip_size (discretion band unit);
	//   Profiles— trailing-24h bucketed tape profile (VWAP); a
	//             ClickHouse bucket source binds the same seam in
	//             Phase-23 — flat fallback is documented in vwap.go;
	//   Volume  — TradeVolumeTracker fed by the `trades` JetStream
	//             stream when NATS is wired, else the PgVolumeSource
	//             tape read (degraded freshness, never disabled).
	volTracker := algo.NewTradeVolumeTracker(time.Hour)
	var volSrc algo.VolumeSource = algo.NewPgVolumeSource(pool)
	if natsClient != nil {
		volCons, verr := natsClient.EnsureConsumer(context.Background(),
			"trades", "algo_vp_volume", nats.WithFilterSubject("trades.>"))
		if verr != nil {
			log.Warn("algo VP volume consumer unavailable — tape fallback",
				"err", verr)
		} else {
			stop, serr := natsClient.Subscribe(volCons, volTracker.Consume)
			if serr != nil {
				log.Warn("algo VP volume subscribe failed — tape fallback",
					"err", serr)
			} else {
				volSrc = volTracker
				go func() { <-sweepCtx.Done(); stop() }()
				log.Info("algo VP volume tracker consuming", "stream", "trades",
					"durable", "algo_vp_volume")
			}
		}
	}
	algoEngine, err := algo.NewEngine(algo.Options{
		Store:    algo.NewPgStore(pool),
		Exec:     &algoChildExecutor{svc: orderSvc, store: orderStore},
		Quote:    algo.NewPgTopOfBook(pool),
		Ref:      algo.NewPgRefPrice(pool),
		Pips:     algo.NewPgPipSize(pool),
		Profiles: algo.NewPgVolumeProfile(pool),
		Volume:   volSrc,
		// Task 16.3.22 — deterministic parent/child cancel-race
		// reconcile after every parent cancel CAS.
		Race: algo.NewRaceGuard(pool, nil),
		Logf: func(f string, a ...any) { log.Info(fmt.Sprintf("algo: "+f, a...)) },
	})
	if err != nil {
		return fmt.Errorf("algo engine: %w", err)
	}
	go algoEngine.Run(sweepCtx) // delayed-dispatch sweeper + crash adoption

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
		// Phase-21 Task 21.3.11 — the terminal funding events also feed
		// ongoing monitoring (post-commit, best-effort, never blocks).
		monSvc.ObserveFundingNotification(ctx, accountID, event, payload)
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
		// Task 21.3.11 — post-commit screening seam: runs AFTER the
		// approval transaction lands, so a screen failure never rolls
		// back a committed approval; it routes to holds/alerts instead.
		PostApproveHook: func(ctx context.Context, accountID int64) {
			if screeningSvc == nil {
				log.Warn("kyc post-approve screen skipped — screening unbound",
					"account_id", accountID)
				return
			}
			sub, serr := screeningStore.SubjectFor(ctx, accountID)
			if serr != nil {
				log.Error("kyc post-approve subject materialize failed",
					"account_id", accountID, "err", serr)
				return
			}
			if _, serr := screeningSvc.ScreenOnboarding(ctx, sub); serr != nil {
				log.Error("kyc post-approve screen failed",
					"account_id", accountID, "err", serr)
			}
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

	// Phase-16 Task 16.3.19 — grid bot engine. Every child leg rides the
	// same orders.Service admission/risk/balance/dispatch pipeline as a
	// manual LIMIT order; fills loop back through the consumer's fill
	// hook below (gridEngine.OnFill) for adjacent-level counter placement
	// and realized-PnL accounting. Persisted in migration-071 tables.
	gridEngine := bots.NewEngine(bots.NewPgStore(pool), orderSvc, orderStore, nil)

	// Phase-19.5 mark source (Task 19.5.3.4): the PriceOracle's
	// published Redis marks are primary — median of ≥2 fresh feeds with
	// provenance + staleness on every read. The last-trade stub stays
	// bound as the no-oracle dev fallback and as the fill-hook Observe
	// target (the chain delegates writes through). Phase-19 consumers
	// are untouched — the frozen MarkPriceProvider seam carries both.
	stubMarkProv := risk.NewStubMarkPriceProvider().WithFallback(risk.PgLastTradeFallback(pool))
	oracleProv := oracle.NewProvider(rdb.Client)
	markProv := risk.NewChainedMarkPriceProvider(
		oracle.NewRiskMarkProvider(oracleProv), stubMarkProv)
	// Task 19.5.3.7 — fail-closed staleness interceptor: a symbol whose
	// oracle health reads UNAVAILABLE (or absent) rejects
	// position-increasing margin orders with PRICE_ORACLE_UNAVAILABLE
	// inside checkAdmission; reduce_only bypasses it.
	orderSvc.WithOracleGate(oracleProv)
	// markCache is the Redis mark keyspace (mark:{symbol}) — the
	// Phase-19.5 oracle writes these keys; today the fill hook publishes
	// last-trade deltas through the same seam so the event-driven
	// margin engine (19.3.26) and collateral monitor (19.3.28) have a
	// live feed.
	markCache := risk.NewRedisMarkCache(rdb.Client)

	// Phase-20 Task 20.3.14 — MiFID II ex-ante cost preview + ex-post
	// annual cost reconciliation (spec §16.10, §24 #379). Every seam
	// resolves to an already-live store: oracleProv is the mark seam
	// (PRICE_ORACLE_UNAVAILABLE fails closed, never a fabricated mark),
	// fundStore the account-meta seam, and the Redis cross-rate source
	// the conversion seam — the same one the funding conversion service
	// consumes, so preview and settlement price identically.
	costsSvc, err := analytics.NewCostsDisclosureService(analytics.CostsDeps{
		Marks:       oracleProv,
		Instruments: analytics.NewPgCostInstrumentSource(pool),
		FeeModels:   settlement.NewPgProfileFeeModelSource(pool),
		Commissions: settlement.NewPgCommissionStore(pool),
		Swap:        settlement.NewPgSwapRateStore(pool),
		Spread:      analytics.NewPgSpreadBpsSource(pool),
		Accounts:    fundStore,
		Conv:        &funding.RedisCrossRateSource{Rdb: rdb},
		Activity:    analytics.NewPgCostActivitySource(pool),
		Trades:      analytics.NewPgTradeFillSource(pool),
	})
	if err != nil {
		return fmt.Errorf("costs disclosure service: %w", err)
	}

	// Phase-20 Task 20.3.16 — marketing-ops report. The inventory store
	// reads the Phase-21-owned financial_promotions relation (no Phase-20
	// migration creates it); a missing table degrades to the registered
	// SERVICE_DEGRADED 503, never an empty report. Cohort cells carry the
	// 100-record anonymity floor — sub-floor cells report
	// INSUFFICIENT_COHORT with the count withheld.
	marketingSvc, err := analytics.NewMarketingReportService(
		analytics.NewPgPromoInventoryStore(pool),
		analytics.NewPgConsentCohortStore(pool))
	if err != nil {
		return fmt.Errorf("marketing report service: %w", err)
	}

	// Phase-20 Task 20.3.15 — retail leveraged-position depreciation
	// notices (MiFID II 10% rule, spec §16.9, §24 #375). Episode HWM state
	// persists in depreciation_episodes (migration 237) — restarts never
	// re-notify a live episode nor lose a reset. The event is registered
	// critical → quiet-hours bypass preserves the statutory
	// same-business-day delivery.
	depSvc, err := analytics.NewDepreciationService(analytics.DepreciationDeps{
		Marks:     oracleProv,
		Positions: analytics.NewPgDepreciationPositionSource(pool),
		Episodes:  analytics.NewPgDepreciationStore(pool),
		Notifier:  notifSvc,
		Margin:    risk.RedisMarginLevelReader{C: rdb},
		Logf:      func(f string, a ...any) { log.Warn(fmt.Sprintf(f, a...)) },
	})
	if err != nil {
		return fmt.Errorf("depreciation service: %w", err)
	}
	depSvc.Start(sweepCtx, time.Hour, func(err error) {
		log.Warn("depreciation hourly sweep failed", "err", err)
	})
	// EOD catch-up at each UTC business-day close (00:05 into the new
	// day): re-evaluates the open set plus positions flattened during the
	// day just ended — an intra-day close below a threshold still emits
	// its notice ("notice on close if crossed").
	go func() {
		for {
			now := time.Now().UTC()
			next := now.Truncate(24 * time.Hour).Add(24*time.Hour + 5*time.Minute)
			t := time.NewTimer(next.Sub(now))
			select {
			case <-sweepCtx.Done():
				t.Stop()
				return
			case <-t.C:
			}
			dayStart := time.Now().UTC().Truncate(24 * time.Hour).Add(-24 * time.Hour)
			if _, serr := depSvc.EndOfDaySweep(sweepCtx, dayStart); serr != nil {
				log.Warn("depreciation EOD sweep failed", "err", serr)
			}
		}
	}()

	// ---- Phase-20 analytics & reporting services (Tasks 20.3.1–20.3.13)
	// ClickHouse read side: ingestion is cmd/analytics' owned surface —
	// the gateway only queries. A boot dial failure degrades every
	// CH-backed endpoint to SERVICE_DEGRADED 503 (handlers fail closed
	// on nil deps) instead of blocking gateway start.
	var chConn analytics.Conn
	if c, derr := analytics.Dial(context.Background(), analytics.ConfigFromEnv()); derr != nil {
		log.Warn("clickhouse unavailable — analytics/reporting endpoints fail closed",
			"addr", analytics.ConfigFromEnv().Addr, "err", derr)
	} else {
		chConn = c
		defer func() { _ = c.Close() }()
	}
	// Interface-typed so a CH outage leaves these genuinely nil — the
	// handlers' `dep == nil` fail-closed checks only work on a nil
	// interface, not a typed-nil concrete store.
	histDeps := &api.HistoryDeps{Instruments: marketStore}
	anDeps := &api.AnalyticsDeps{}
	var incomeSrc api.IncomeHistorySource
	var tcaRep analytics.ReportQuerier
	var tcaReports *analytics.CHReportStore
	if chConn != nil {
		histDeps.Ticks = analytics.NewTickStore(chConn)
		histDeps.Klines = analytics.NewOHLCVStore(chConn)
		anDeps.PnL = analytics.NewPnLStore(chConn)
		anDeps.Stats = analytics.NewVolumeStatsStore(chConn, pgAccountTierLookup{pool: pool})
		incomeSrc = analytics.NewIncomeStore(chConn)
		tcaReports = analytics.NewCHReportStore(chConn)
		tcaRep = tcaReports
	}
	// Phase-21 Task 21.3.19 — public RTS 27/28 best-execution reporting.
	// The services read ClickHouse through analytics.Conn and persist
	// artifacts in PG (rts27_daily_stats / rts27_reports / rts28_reports,
	// migration 251). Nil-able while chConn is down — handlers + the
	// materialization job fail closed SERVICE_DEGRADED.
	var rts27Svc *compliance.RTS27Service
	var rts28Svc *compliance.RTS28Service
	if chConn != nil {
		var rerr error
		rts27Svc, rerr = compliance.NewRTS27Service(pool, chConn,
			compliance.HoldRoleResolver(adminRoleResolver))
		if rerr != nil {
			return fmt.Errorf("rts27 service: %w", rerr)
		}
		rts28Svc, rerr = compliance.NewRTS28Service(pool, chConn,
			compliance.HoldRoleResolver(adminRoleResolver))
		if rerr != nil {
			return fmt.Errorf("rts28 service: %w", rerr)
		}
	}

	// Phase-23 market-data products — history/export/stats read
	// surfaces. Every source seam is interface-typed + nil-able: a CH
	// outage leaves the handler answering SERVICE_DEGRADED, never an
	// empty page standing in for "unavailable" (spec §2.7). Tier
	// resolution fails CLOSED to the free/delayed view — under-
	// entitlement is never a violation.
	histTier := marketdata.RateTierHistoryResolver(tierResolver)
	histDeps.Tiers = histTier
	histDeps.Cache = rdb.Client
	histDeps.SessionOpen = func(string) (time.Time, error) {
		// Venue-wide 24/5 weekly open (Sunday 21:00 UTC) — the bounded
		// pre-open masking window anchors to it, instrument-independent.
		return marketdata.VenueWeekOpenUTC(time.Now().UTC()), nil
	}
	if chConn != nil {
		histDeps.Trades = marketdata.NewTradeHistoryStore(chConn)
	}
	// Task 23.3.7 — published anonymous block tape (correction lineage
	// annotated by the store; schema has no participant columns).
	blockTapeDeps := &api.BlockTapeHistoryDeps{
		Instruments: marketStore, Tiers: histTier, Cache: rdb.Client,
	}
	if chConn != nil {
		blockTapeDeps.Tape = marketdata.NewBlockTapeStore(chConn)
	}
	// Task 23.3.9 — swap-rate history reads the Task 3.3.11 accrual
	// journal in PG directly (no JetStream→CH projection exists).
	swapRateDeps := &api.SwapRateHistoryDeps{
		History:     marketdata.NewPgSwapRateHistory(pool),
		Instruments: marketStore, Tiers: histTier, Cache: rdb.Client,
	}
	// Task 23.3.2 — data export (CSV/JSON/Parquet): ≤50k rows inline,
	// async through export_jobs → S3 → link email (24h expiry). The
	// worker sweeps due jobs once a minute; the email rides the dev log
	// seam until production binds SMTP/SES (same convention as 20.3.8).
	exportSvc := &marketdata.ExportService{
		Jobs:       marketdata.NewPgJobStore(pool),
		IntervalOK: analytics.PersistedInterval,
		LinkBase:   strings.TrimRight(os.Getenv("EXC_PUBLIC_BASE_URL"), "/"),
		Logf:       func(f string, a ...any) { log.Warn(fmt.Sprintf(f, a...)) },
	}
	if chConn != nil {
		exportSvc.Trades = marketdata.NewCHTradesStore(chConn)
		exportSvc.Ticks = api.ExportTickSource{Store: analytics.NewTickStore(chConn)}
		exportSvc.Klines = api.ExportKlineSource{Store: analytics.NewOHLCVStore(chConn)}
	}
	exportSvc.Notifier = marketdata.ReportingNotifier{
		Mail:     reporting.LogEmailSender{Log: log},
		LinkBase: exportSvc.LinkBase,
	}
	exportSvc.Recipients = reporting.NewPgRecipientSource(pool)
	go exportSvc.Run(sweepCtx, time.Minute)
	exportDeps := &api.ExportDeps{Exporter: exportSvc, Instruments: marketStore}

	// Phase-23 Task 23.3.3 — premium-feed subscription billing. The
	// sweeper posts monthly renewals through the §5.3 double-entry
	// poster once a day (06:00 UTC, after the venue-governance slot)
	// and republishes each committed charge to JetStream "funding".
	// A nil poster fails closed — nothing is marked billed unpaid.
	premiumFeedBiller := marketdata.NewPremiumFeedBiller(
		marketdata.NewPgxFeedSubscriptionStore(pool), ledgerSvc,
		marketdata.BillingEventSinkFunc(
			func(ctx context.Context, ev marketdata.PremiumFeedBillingEvent) error {
				if natsClient == nil {
					return nil
				}
				payload, _ := json.Marshal(ev)
				_, err := natsClient.Publish(ctx, "funding", 0,
					fmt.Sprintf("premium-feed-%d", ev.SubscriptionID), payload)
				return err
			}), nil, log)
	runDailyUTC(sweepCtx, log, "premium-feed-billing", 360,
		func(ctx context.Context) {
			n, berr := premiumFeedBiller.BillDue(ctx)
			if berr != nil {
				log.Error("premium feed billing sweep failed", "err", berr)
				return
			}
			if n > 0 {
				log.Info("premium feed billing sweep", "billed", n)
			}
		})
	// Phase-21 Task 21.3.15/19 daily jobs — 05:00 UTC, after the
	// Phase-20 02:00 RTS28 and Phase-21 03:00 Basel slots: materialize
	// yesterday's RTS 27 daily stats from ClickHouse and sweep the
	// member-governance overdue set (ANNUAL_ATTESTATION_OVERDUE P1).
	runDailyUTC(sweepCtx, log, "venue-governance", 300, func(ctx context.Context) {
		if rts27Svc != nil {
			day := time.Now().UTC().Add(-24 * time.Hour)
			if n, err := rts27Svc.MaterializeDay(ctx, day); err != nil {
				log.Warn("rts27 daily materialization failed",
					"day", day.Format("2006-01-02"), "err", err)
			} else if n > 0 {
				log.Info("rts27 daily stats materialized",
					"day", day.Format("2006-01-02"), "rows", n)
			}
		}
		if n, err := venueSvc.SweepOverdue(ctx); err != nil {
			log.Warn("venue governance sweep failed", "err", err)
		} else if n > 0 {
			log.Info("venue members overdue review paged", "flagged", n)
		}
	})

	// Document store for statements / confirmations / invoices / RTS28
	// (5–7y retention targets — Tasks 20.3.6–20.3.9). S3 is the durable
	// backend (EXC_REPORTS_S3_BUCKET; dev binds the devs3 stub through
	// EXC_S3_ENDPOINT). No bucket → in-process MemFileStore with a loud
	// warning: acceptable in dev only, documents regenerate on demand.
	var reportDocs analytics.FileStore
	var reportObjects objectstore.Client
	if bucket := os.Getenv("EXC_REPORTS_S3_BUCKET"); bucket != "" {
		ocfg := objectstore.ConfigFromEnv(bucket, os.Getenv)
		var oc objectstore.Client
		var oerr error
		if cfg.Environment != "production" && ocfg.Endpoint != "" {
			oc, oerr = objectstore.NewDev(context.Background(), ocfg)
		} else {
			oc, oerr = objectstore.NewAWS(context.Background(), ocfg)
		}
		if oerr != nil {
			log.Warn("reports object store init failed — document fetch degrades",
				"err", oerr)
			reportDocs = analytics.NewMemFileStore()
		} else {
			reportObjects = oc
			reportDocs = &analytics.S3FileStore{Client: oc, Prefix: "reports"}
		}
	} else {
		log.Warn("EXC_REPORTS_S3_BUCKET unset — report documents held in-process (dev only)")
		reportDocs = analytics.NewMemFileStore()
	}
	// Phase-23 Task 23.3.2 — export artifacts ride the same S3 client
	// (async jobs Put under their own key prefix; nil → Admit fails
	// closed for async exports only, sync still serves).
	exportSvc.Objects = reportObjects

	// Task 20.3.8 — client-facing PDFs (statements, confirmations,
	// invoices) are AES-128 encrypted at render (pdfsec V4/R4). The
	// document-open password is a per-account PIN derived from
	// EXC_DOCS_SECRET; unset ⇒ the services fail closed at generate
	// time rather than emitting plaintext client documents.
	docCipher := analytics.NewDocCipher([]byte(os.Getenv("EXC_DOCS_SECRET")))
	if docCipher == nil {
		log.Warn("EXC_DOCS_SECRET unset — statement/confirmation/invoice generation will fail closed")
	}

	stmtSvc, err := analytics.NewStatementService(pool, reportDocs)
	if err != nil {
		return fmt.Errorf("statement service: %w", err)
	}
	stmtSvc.SetDocCipher(docCipher)
	invSvc, err := analytics.NewInvoiceService(pool, reportDocs, nil)
	if err != nil {
		return fmt.Errorf("invoice service: %w", err)
	}
	invSvc.SetDocCipher(docCipher)
	confSvc, err := analytics.NewConfirmationService(pool, reportDocs)
	if err != nil {
		return fmt.Errorf("confirmation service: %w", err)
	}
	confSvc.SetDocCipher(docCipher)
	tbSvc, err := analytics.NewTrialBalanceService(pool)
	if err != nil {
		return fmt.Errorf("trial balance service: %w", err)
	}
	snapStore := analytics.NewSnapshotStore(pool)

	// Task 20.3.8 — confirmation delivery engine: portal fetch is the
	// delivery marker; email renders through the log seam (production
	// binds SMTP/SES — ops seam); MT515 submits through the log adapter
	// until Phase-24's SWIFT transport owns the uplink.
	confTracker := reporting.NewPgConfirmationTracker(pool)
	confCats := reporting.NewPgCategorySource(pool)
	confDeliv := &reporting.Delivery{
		Tracker:    confTracker,
		Docs:       reportDocs,
		Email:      reporting.LogEmailSender{Log: log},
		Recipients: reporting.NewPgRecipientSource(pool),
		Categories: confCats,
		MT515:      reporting.LogMT515Submitter{Log: log},
		Logf:       func(f string, a ...any) { log.Warn(fmt.Sprintf(f, a...)) },
	}
	confEngine, err := reporting.NewConfirmationService(reporting.ServiceOptions{
		Generator:  analyticsConfirmationGenerator{svc: confSvc},
		Tracker:    confTracker,
		Delivery:   confDeliv,
		Categories: confCats,
		Logf:       func(f string, a ...any) { log.Warn(fmt.Sprintf(f, a...)) },
	})
	if err != nil {
		return fmt.Errorf("confirmation delivery: %w", err)
	}
	// T+1 sweep — institutional rows are already due (dueAt == generated)
	// so the same sweep is both the retail schedule and the retry path
	// for failed immediate dispatches.
	confSched := reporting.NewDeliveryScheduler(confTracker, confDeliv, confCats)
	confSched.Logf = func(f string, a ...any) { log.Warn(fmt.Sprintf(f, a...)) }
	go confSched.Run(sweepCtx, time.Minute)

	if natsClient != nil {
		// Per-fill generation consumer — trades.> fills become contract
		// notes within the MiFID II 60s window (AckAfterInsert ordering:
		// generation writes durable PG rows before the ack).
		if confConsumer, cerr := reporting.NewConfirmationConsumer(confEngine); cerr != nil {
			log.Warn("confirmation consumer init failed", "err", cerr)
		} else if cons, cerr := natsClient.EnsureConsumer(context.Background(), "trades",
			"confirmations_delivery", nats.WithFilterSubject("trades.>")); cerr != nil {
			log.Warn("confirmation consumer unavailable", "err", cerr)
		} else {
			go func() {
				if cerr := confConsumer.Consume(sweepCtx, cons); cerr != nil &&
					!errors.Is(cerr, context.Canceled) {
					log.Error("confirmation consumer stopped", "err", cerr)
				}
			}()
			log.Info("confirmation delivery consuming", "stream", "trades",
				"durable", "confirmations_delivery")
		}
	}

	// Task 20.3.9 — TCA: per-fill slippage vs arrival/session-VWAP/fix
	// into tca_results (CH), plus the quarterly RTS28 rollup. Both need
	// the CH conn; without it the consumer simply isn't armed (no fake
	// analytics are ever emitted).
	var rts28 *analytics.RTS28SummaryJob
	if chConn != nil && natsClient != nil {
		tcaEngine, terr := analytics.NewTCAEngine(
			analytics.NewCHTCASink(chConn),
			analytics.OracleArrivalSource{P: oracleProv},
			analytics.CHSessionVWAP{CH: chConn},
			analytics.RedisFixSource{Rdb: rdb.Client})
		if terr != nil {
			log.Warn("TCA engine init failed", "err", terr)
		} else if tcaCons, terr := analytics.NewTCAFillConsumer(tcaEngine,
			tradeResolver,
			analytics.NewPgTCAOrderSource(pool),
			analytics.NewPgTCASymbolSource(pool)); terr != nil {
			log.Warn("TCA consumer init failed", "err", terr)
		} else if cons, cerr := natsClient.EnsureConsumer(context.Background(),
			"trades", "tca_fills", nats.WithFilterSubject("trades.>")); cerr != nil {
			log.Warn("TCA consumer unavailable", "err", cerr)
		} else {
			stop, serr := natsClient.Subscribe(cons, func(m jetstream.Msg) {
				if err := tcaCons.HandleMsg(sweepCtx, m); err != nil {
					log.Warn("TCA fill failed", "err", err)
				}
			})
			if serr != nil {
				log.Warn("TCA subscribe failed", "err", serr)
			} else {
				go func() { <-sweepCtx.Done(); stop() }()
				log.Info("TCA fill consumer running", "stream", "trades",
					"durable", "tca_fills")
			}
		}
		if j, jerr := analytics.NewRTS28SummaryJob(tcaReports,
			analytics.NewCHTCASink(chConn), reportObjects); jerr != nil {
			log.Warn("RTS28 job init failed", "err", jerr)
		} else {
			rts28 = j
		}
	}
	_ = rts28 // scheduled below alongside the other Phase-20 jobs

	// Nightly/daily jobs — all RunOnce bodies are idempotent (UNIQUE
	// upserts / checksum-gated batches), so a retried slot is safe.
	stmtJob := analytics.NewStatementJob(stmtSvc)
	runDailyUTC(sweepCtx, log, "statements", 45,
		func(ctx context.Context) {
			d, m, jerr := stmtJob.RunOnce(ctx)
			if jerr != nil {
				log.Warn("statement generation failed", "err", jerr)
				return
			}
			log.Info("statements generated", "generated", d.Generated,
				"failed", d.Failed, "monthly", m != nil)
		})
	tbJob := analytics.NewDailyTrialBalanceJob(tbSvc)
	runDailyUTC(sweepCtx, log, "trial-balance", 15,
		func(ctx context.Context) {
			day := time.Now().UTC().Truncate(24 * time.Hour).Add(-24 * time.Hour)
			tb, checks, jerr := tbJob.RunOnce(ctx, day)
			if jerr != nil {
				log.Warn("trial balance job failed", "err", jerr)
				return
			}
			for _, c := range checks {
				if !c.OK {
					log.Error("GL reconciliation variance", "check", c.Name,
						"currency", c.Currency, "variance", c.Variance.String())
				}
			}
			log.Info("trial balance persisted", "day", day.Format("2006-01-02"),
				"currencies", len(tb.Currencies))
		})
	// Task 20.3.7 — ERP nightly batch. Adapter selection is env-driven:
	// EXC_ERP_WEBHOOK_URL → webhook POST; EXC_ERP_DROP_DIR → SFTP drop
	// directory (a real SFTP client substitutes the same seam). Neither
	// configured → the job is not armed (fail-quiet, logged once).
	var erpAdapter analytics.ERPAdapter
	switch {
	case os.Getenv("EXC_ERP_WEBHOOK_URL") != "":
		erpAdapter = &analytics.WebhookAdapter{URL: os.Getenv("EXC_ERP_WEBHOOK_URL")}
	case os.Getenv("EXC_ERP_DROP_DIR") != "":
		erpAdapter = &analytics.SFTPDropAdapter{
			Dir:      os.Getenv("EXC_ERP_DROP_DIR"),
			Endpoint: os.Getenv("EXC_ERP_SFTP_ENDPOINT")}
	default:
		log.Warn("no ERP adapter configured (EXC_ERP_WEBHOOK_URL/EXC_ERP_DROP_DIR) — nightly export not armed")
	}
	if erpAdapter != nil {
		erpSvc, eerr := analytics.NewERPBatchService(pool, erpAdapter, tbSvc)
		if eerr != nil {
			log.Warn("ERP batch service init failed", "err", eerr)
		} else {
			erpJob := analytics.NewERPNightlyJob(erpSvc)
			runDailyUTC(sweepCtx, log, "erp-export", 90,
				func(ctx context.Context) {
					d, jerr := erpJob.RunOnce(ctx)
					if jerr != nil {
						log.Warn("ERP nightly export failed", "err", jerr)
						return
					}
					log.Info("ERP batch delivered", "run_id", d.RunID, "ref", d.DeliveryRef)
				})
		}
	}
	if rts28 != nil {
		runDailyUTC(sweepCtx, log, "rts28", 120,
			func(ctx context.Context) {
				now := time.Now().UTC()
				// Quarter roll: run on the first two days of each quarter
				// for the just-ended quarter (day 2 is the retry window —
				// the rollup is idempotent on its (quarter, venue) keys).
				qs := analytics.QuarterContaining(now)
				if now.Sub(qs) > 48*time.Hour {
					return
				}
				prev := qs.AddDate(0, -3, 0)
				n, jerr := rts28.RunQuarter(ctx, prev)
				if jerr != nil {
					log.Warn("RTS28 quarterly rollup failed", "quarter", prev, "err", jerr)
					return
				}
				log.Info("RTS28 quarterly rollup persisted",
					"quarter", prev.Format("2006-01-02"), "rows", n)
			})
	}

	// §13.12/§13.4a ADV yardstick: the instrument:adv:{id} Redis mirror
	// is the fast path for venues publishing precomputed analytics; the
	// trailing 7-day trades-ledger mean is the system of record, and a
	// 60s per-instrument memoization bounds the per-tick PG cost the
	// event-driven margin engine would otherwise impose.
	advSrc := &risk.CachedADVSource{
		Src: risk.ChainedADVSource{
			risk.NewRedisADVSource(rdb.Client),
			risk.PgADVSource{Pool: pool},
		},
		TTL: time.Minute,
	}
	// Forward declarations: the liquidation service and margin-call
	// service are constructed once the Phase-19 services below assemble
	// (they need the fund, which needs the ledger). The consumer fill
	// hook guards on nil — a fill arriving before construction logs and
	// skips rather than blocking the read-model drain.
	var liqSvc *risk.LiquidationService
	var liqStore *risk.PgLiquidationStore
	var adlStore *risk.PgADLStore
	var bilatSvc *risk.BilateralCreditService

	// Phase-03 Task 3.3.10 settlement leg: the orders consumer below is
	// the out-ring's sole reader (SPSC contract — a second shm reader
	// would steal frames), so fills reach BalanceService through a
	// per-shard frame tap feeding settlement.FuncSource queues. Each
	// shard gets its own FillConsumer so EngineFill.ShardID stays
	// truthful (engine trade_ids are shard-local). FillConsumer batches
	// (batch 100 / 10ms flush) and halts fail-closed on settlement
	// errors; processed_trades dedup makes restart replay safe.
	balanceSvc, err := settlement.NewBalanceService(pool, ledgerSvc, opsAlerter)
	if err != nil {
		return fmt.Errorf("balance service: %w", err)
	}
	settleQueues := make(map[uint16]chan []byte)
	var fillConsumers []*settlement.FillConsumer
	for _, sh := range shardIDs(shardMap) {
		q := make(chan []byte, 1<<14)
		settleQueues[sh] = q
		src := settlement.FuncSource(func(limit int, deliver func([]byte)) int {
			n := 0
			for n < limit {
				select {
				case p := <-q:
					deliver(p)
					n++
				default:
					return n
				}
			}
			return n
		})
		fc, ferr := settlement.NewFillConsumer(balanceSvc,
			tradeResolver, src, int64(sh), 0, 0)
		if ferr != nil {
			return fmt.Errorf("fill consumer shard %d: %w", sh, ferr)
		}
		// Post-commit fill republish: in the shm-only topology there is no
		// Aeron bridge, so trades/settlements JetStream consumers starve
		// without this fan-out. Same subject + Nats-Msg-Id contract as the
		// bridge — coexistence dedups at the stream level.
		if natsClient != nil {
			fc.WithRepublisher(jetstreamFillPublisher{js: natsClient.JetStream()}).
				WithLogger(func(f string, a ...any) { log.Error(fmt.Sprintf("settlement: "+f, a...)) })
		} else {
			log.Warn("nats unavailable — fill republish disabled; "+
				"trades/settlements JetStream streams will starve until restart",
				"shard", sh)
		}
		fillConsumers = append(fillConsumers, fc)
		go func() {
			// Fail-closed halt + restart-with-backoff: the queue survives
			// a consumer instance (undelivered frames are re-pumped), so
			// a transient halt drains through. Note the dev caveat: a
			// fill committed-to-queue but uncommitted-to-ledger is only
			// recoverable by WAL/recon — production redelivery rides the
			// bridge → JetStream settlements stream instead.
			for {
				err := fc.Run(sweepCtx)
				if sweepCtx.Err() != nil {
					return
				}
				log.Error("settlement fill consumer halted — restarting; "+
					"unsettled fills surface via reconciliation",
					"shard", sh, "err", err)
				select {
				case <-sweepCtx.Done():
					return
				case <-time.After(250 * time.Millisecond):
				}
			}
		}()
	}
	// Per-shard consumer counters → /metrics. RepubDropped is the
	// dead-letter signal: a nonzero value means committed fills could
	// not be re-emitted to JetStream (deterministic subject failure)
	// and ops must reconcile that shard's stream.
	metReg.VecFunc("settlement_fill_consumer",
		"Fill consumer counters per shard (malformed, republished, repub_dropped).",
		"counter", func() []observability.PullSample {
			var out []observability.PullSample
			for _, c := range fillConsumers {
				m := c.Metrics()
				sh := strconv.FormatInt(c.ShardID(), 10)
				for name, v := range map[string]float64{
					"malformed":     float64(m.Malformed),
					"republished":   float64(m.Republished),
					"repub_dropped": float64(m.RepubDropped),
				} {
					out = append(out, observability.PullSample{
						Labels: []string{"shard", sh, "counter", name}, Value: v})
				}
			}
			return out
		})
	// Settlement queue depth/capacity — the in-process buffer between the
	// out-ring drain and the fill consumer. A wedged consumer fills the
	// queue and backpressures the out-ring; without this gauge the stall
	// is invisible until IPCRingSaturated pages.
	metReg.VecFunc("settlement_queue_depth",
		"Settlement fill queue depth per shard (depth and capacity).",
		"gauge", func() []observability.PullSample {
			out := make([]observability.PullSample, 0, len(settleQueues)*2)
			for sh, q := range settleQueues {
				lbl := strconv.Itoa(int(sh))
				out = append(out,
					observability.PullSample{Labels: []string{"shard", lbl, "kind", "depth"}, Value: float64(len(q))},
					observability.PullSample{Labels: []string{"shard", lbl, "kind", "capacity"}, Value: float64(cap(q))})
			}
			return out
		})

	// Engine out-ring consumer: cancel echoes + trade fills update the PG
	// read model; without it pending confirms only time out. The fill
	// hook is the order_filled emitter (Task 12.3.5) — order→account→user
	// resolution then Notify, best-effort, panic-guarded by the consumer.
	go orders.NewConsumer(orderSubmitter, orderStore, orderSvc.Pending()).
		WithFrameTap(func(sh uint16, p []byte) {
			// Copy: the drain buffer is reused by the consumer loop.
			// Blocking send is the backpressure contract — a settlement
			// queue >16K frames deep means settlement has halted and the
			// read-model drain stalls rather than silently dropping fills.
			if q := settleQueues[sh]; q != nil {
				select {
				case q <- append([]byte(nil), p...):
				case <-sweepCtx.Done():
				}
			}
		}).
		WithFillHook(func(orderID int64, px, qty decimal.Decimal) {
			ctx := context.Background()
			// Phase-16 composite/auction lifecycle: bracket parent fills
			// place proportional SL/TP OCO children; order-list fills
			// activate pending legs; auction fills emit order.auction_fill.
			orderSvc.OnFill(ctx, orderID, px, qty)
			// Task 16.3.19: grid children observe their own fills through
			// this same consumer path — a non-grid order ID resolves to
			// no grid_bot_orders row and returns immediately. Errors are
			// logged, never mask the read-model drain.
			if err := gridEngine.OnFill(ctx, orderID, px, qty); err != nil {
				log.Warn("grid fill hook failed", "order_id", orderID, "err", err)
			}
			o, oerr := orderStore.GetOrder(ctx, orderID)
			if oerr != nil || o == nil {
				return
			}
			// Phase-19: every engine fill feeds the mark placeholder —
			// the last-trade price IS the mark until Phase-19.5's oracle
			// takes over the same seam.
			if inst, ierr := orderStore.InstrumentByID(ctx, o.InstrumentID); ierr == nil && inst != nil {
				markProv.Observe(inst.Symbol, px, time.Now())
				// Publish the mark delta (SET mark:{symbol} + PUBLISH)
				// — the margin engine's RedisMarkSource consumes it.
				// Best-effort: a publish failure must not stall the
				// read-model drain; the 2s scanner remains authoritative.
				if err := markCache.PublishMarkDelta(ctx, inst.Symbol, px, time.Now()); err != nil {
					log.Warn("mark delta publish", "symbol", inst.Symbol, "err", err)
				}
			}
			// Phase-19 liquidation fills reconcile through RecordFill:
			// liq-{position_id}-{ms} direct closes, auc-{auction_id}-{ms}
			// auction legs, auc-fc-{auction_id}-{ms} force-cash legs.
			// The position's side inverts the close order's side.
			// ADL force-close fills reconcile against their directive:
			// adl-{adl_seq} — the store's CompleteADLFill resolves the
			// counterparty position and writes both event rows atomically.
			// The liquidated-side context isn't persisted on the
			// directive (migration 230) — LiquidatedAccountID=0 skips the
			// supplementary event; the liquidated side already carries
			// its own liquidation_events rows from the original close.
			if adlStore != nil {
				if seq, isADL := parseADLClientID(o.ClientOrderID); isADL {
					mark := decimal.Zero
					if inst, ierr := orderStore.InstrumentByID(ctx, o.InstrumentID); ierr == nil && inst != nil {
						if m, merr := markProv.GetMarkPrice(inst.Symbol); merr == nil {
							mark = m
						}
					}
					if _, err := adlStore.CompleteADLFill(ctx, risk.ADLFillReport{
						AdlSeq: seq, FilledQty: qty, FillPrice: px, MarkPrice: mark,
					}); err != nil {
						log.Error("adl fill reconcile failed — ops reconcile required",
							"order_id", orderID, "client_order_id", o.ClientOrderID, "err", err)
					}
				}
			}
			if liqSvc != nil && liqStore != nil {
				posID, auctionID, isAuction, isFC, isLiq := parseLiquidationClientID(o.ClientOrderID)
				if isLiq {
					mark := decimal.Zero
					if inst, ierr := orderStore.InstrumentByID(ctx, o.InstrumentID); ierr == nil && inst != nil {
						if m, merr := markProv.GetMarkPrice(inst.Symbol); merr == nil {
							mark = m
						}
					}
					if isAuction && auctionID > 0 {
						if row, rerr := liqStore.AuctionByID(ctx, auctionID); rerr == nil && row != nil {
							posID = row.PositionID
						}
					}
					if posID > 0 {
						side := "LONG"
						if o.Side == "BUY" {
							side = "SHORT"
						}
						f := risk.LiquidationFill{
							PositionID: posID, AccountID: o.AccountID,
							InstrumentID: o.InstrumentID, Side: side,
							Qty: qty, Price: px, MarkPrice: mark,
							IsAuction: isAuction, IsForceCash: isFC,
						}
						if isAuction {
							f.AuctionID = &auctionID
						}
						if err := liqSvc.RecordFill(ctx, f); err != nil {
							log.Error("liquidation fill reconcile failed — ops reconcile required",
								"order_id", orderID, "client_order_id", o.ClientOrderID, "err", err)
						}
					}
				}
			}
			// Task 19.3.10: every fill consumes the order's bilateral
			// credit reservation pro-rata (quote-ccy notional → USD).
			// No reservation row = no-op, so ordinary flow is untouched;
			// engine-reserved matches reconcile here.
			if bilatSvc != nil {
				if inst, ierr := orderStore.InstrumentByID(ctx, o.InstrumentID); ierr == nil && inst != nil {
					if err := bilatSvc.ConsumeFill(ctx, o.ID, inst.QuoteCurrency, px.Mul(qty)); err != nil {
						log.Warn("bilateral credit consume", "order_id", orderID, "err", err)
					}
				}
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
				// Phase-18 Task 18.3.10: quote-sourced fills (mq:*
				// client_order_id attribution from the FIX mass-quote
				// path) count in the MMP sliding window and accrue the
				// maker rebate. Both degrade to no-ops when no ACTIVE
				// program covers the account/instrument; errors log,
				// never mask the read-model drain.
				if strings.HasPrefix(o.ClientOrderID, "mq:") {
					if _, err := mmTracker.OnFill(ctx, o.AccountID, o.InstrumentID); err != nil {
						log.Warn("mmp fill hook failed",
							"order_id", orderID, "err", err)
					}
					if _, err := mmSvc.AccrueRebate(ctx, o.AccountID, o.InstrumentID,
						fmt.Sprintf("fill:%d:%s:%s", o.ID, px.String(), qty.String()),
						qty, px); err != nil {
						log.Warn("mm rebate accrual failed",
							"order_id", orderID, "err", err)
					}
				}
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
		}).WithGSLOHook(func(orderID int64, px, qty decimal.Decimal) {
		// Task 16.3.16: GSLO gap absorption — a fill worse than the
		// guaranteed stop compensates the client from the insurance
		// fund. Errors log, never mask the read-model drain.
		if err := gsloSvc.OnFill(context.Background(), orderID, px, qty); err != nil {
			log.Warn("gslo gap hook failed", "order_id", orderID, "err", err)
		}
	}).WithCancelHook(func(orderID int64, _ uint8) {
		// Phase-16 composite/auction lifecycle: bracket parent cancels
		// cascade to children; list leg cancels advance the list;
		// engine-cancelled MOO/MOC emit order.cancelled.
		orderSvc.OnCancel(context.Background(), orderID)
		// Task 19.3.10: cancel/reject releases the order's bilateral
		// credit reservation (idempotent — no row means no-op).
		if bilatSvc != nil {
			if err := bilatSvc.ReleaseOrder(context.Background(), orderID); err != nil {
				log.Warn("bilateral credit release", "order_id", orderID, "err", err)
			}
		}
	}).WithTracer(tracer).Run(sweepCtx, shardIDs(shardMap))

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
		// Task 8.5.3.2: shares api.PgTierLookup so WS honours the same
		// DEMO→TierDemo (2× Basic) substitution as REST — one policy.
		TierResolver: func(ctx context.Context, sess *ws.Session) ratelimit.Tier {
			name, err := api.PgTierLookup(pool)(ctx, sess.AccountID)
			if err != nil || name == "" {
				return ratelimit.TierBasic
			}
			return ratelimit.ParseTier(name)
		},
	})
	// Phase-13 Task 13.3.9: stream every breaker transition to the
	// "admin.circuit_breaker" WS admin-monitor channel (documented seam —
	// no pre-existing admin monitor channel in ws.Server).
	breakerSvc.WithPublisher(wsSrv)

	// ---- Phase-17 Tasks 17.3.2/17.3.4 — L3 order-level surface ----
	//
	// The dedicated /ws/v1/l3/{symbol} endpoint shares ws.Server's auth
	// seams (JWT keyring, API-key HMAC/Ed25519 verifier, fee_tiers
	// resolver) but runs its own hub: a 100,000-event per-symbol replay
	// ring, NO conflation (every order event forwards), non-blocking
	// per-subscriber outboxes (5,000-msg lag or 1.5s socket saturation
	// ⇒ L3_CONSUMER_OVERRUN), last_seq resume and gap→snapshot recovery
	// (L3_SEQUENCE_GAP_DETECTED).
	l3Journal := marketdata.NewRedisGapJournal(rdb.Client)
	l3Hub := marketdata.NewL3Hub(marketdata.L3HubConfig{
		Logger: log, Journal: l3Journal})
	l3Srv := marketdata.NewL3Server(marketdata.L3ServerConfig{
		Hub: l3Hub, Issuer: jwtIssuer, Verifier: sigVerifier,
		Logger: log, TrustProxy: true,
		// Task 8.5.3.2: same shared lookup — DEMO accounts get TierDemo
		// (2× Basic) on the L3 surface exactly as on REST/WS.
		TierResolver: func(ctx context.Context, sess *ws.Session) ratelimit.Tier {
			name, err := api.PgTierLookup(pool)(ctx, sess.AccountID)
			if err != nil || name == "" {
				return ratelimit.TierBasic
			}
			return ratelimit.ParseTier(name)
		},
	})
	// Feed pump: the bridge republishes L3OrderEvent rows on the
	// JetStream "l3" stream (l3.{shard}.{symbol}); the SPSC _out ring
	// keeps its single consumer (bridge), the gateway rides the durable
	// republish. Absent NATS the hub idles — the endpoint still answers
	// control frames honestly (empty snapshot/resync).
	if natsClient != nil {
		l3Symbols := marketdata.SubjectSymbol{}
		if insts, ierr := marketStore.ListInstruments(context.Background()); ierr == nil {
			for _, in := range insts {
				l3Symbols[strings.ReplaceAll(in.Symbol, "/", "-")] = in.Symbol
			}
		}
		l3src := marketdata.NewJetStreamL3Source(
			marketdata.JetStreamMsgSource(natsClient, "l3", "gateway-l3", "l3.>", log),
			l3Symbols, log)
		go func() {
			if err := l3Hub.Run(sweepCtx, l3src); err != nil &&
				!errors.Is(err, context.Canceled) {
				log.Error("l3 feed pump exited", "err", err)
			}
		}()
	} else {
		log.Warn("nats unavailable — L3 order-level feed idle (endpoint serves resync)")
	}

	// WAL snapshot REST: the dedicated reader prescans the symbol's
	// shard dir for a WAL position marker, replays BOOK_SNAPSHOT+tail
	// forward to it and cursor-paginates — the matching loop is never
	// touched (spec §11.1 asynchronous reconstruction).
	l3SnapReader := marketdata.NewL3SnapshotReader(marketdata.L3SnapshotDeps{
		WalDirs: func(symbol string) []string {
			return walDirsForSymbol(shardMap, symbol)
		},
		InstrumentID: func(symbol string) (uint32, bool) {
			inst, ierr := marketStore.InstrumentBySymbol(context.Background(), symbol)
			if ierr != nil || inst == nil {
				return 0, false
			}
			return uint32(inst.ID), true
		},
		Shard: func(symbol string) (int, bool) {
			return shardMap.GetShard(symbol), true
		},
	})
	l3SnapDeps := &api.L3SnapshotDeps{Reader: l3SnapReader}
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

	// ---- Phase-15 Tasks 15.3.4 / 15.3.7 — market schedule + 24/5
	//      weekly session lifecycle ----
	//
	// MarketScheduleService owns the market:hours Redis projection the
	// C++ PreTradeChecker polls (spec §1 window SUN 21:00 → FRI 22:00
	// UTC + market_schedule_overrides, migration 221). Boot reconcile
	// unconditionally rewrites the key — a restarted gateway never
	// leaves a stale schedule in front of the engine. A publish failure
	// retries on a 30s loop: an absent market:hours is a fail-closed
	// hazard, not a boot-fatal one (the C++ gate rejects on a missing
	// key anyway).
	schedSvc, err := admin.NewMarketScheduleService(pool, rdb,
		admin.AdminRoleResolver(adminRoleResolver))
	if err != nil {
		return fmt.Errorf("market schedule service: %w", err)
	}
	go func() {
		for {
			if err := schedSvc.Reconcile(sweepCtx); err != nil {
				log.Warn("market:hours reconcile failed — retrying in 30s", "err", err)
				select {
				case <-sweepCtx.Done():
					return
				case <-time.After(30 * time.Second):
					continue
				}
			}
			log.Info("market:hours published", "key", admin.MarketHoursKey)
			return
		}
	}()

	// SessionService drives the §6.7 weekly machine
	// (OPEN → PRE_CLOSE → CLOSED → PRE_OPEN → OPEN) across
	// session:state:{shard} keys. The Friday-close transition fires the
	// Task 3.3.7 Tom-Next roll through the RolloverRunner seam — bound
	// to a real settlement.RolloverService when the calendar/swap
	// stores resolve; otherwise nil (the seam logs + the daemon, when
	// deployed, is the backstop). Pending effects persist in
	// session:ctl so a mid-transition crash is resumed by any replica.
	var rolloverRunner admin.RolloverRunner
	if cal, cerr := settlement.LoadCalendar(context.Background(), pool); cerr != nil {
		log.Warn("session lifecycle: holiday calendar unavailable — rollover seam unwired",
			"err", cerr)
	} else {
		swapStore := settlement.NewPgSwapRateStore(pool)
		swapEngine, eerr := settlement.NewSwapEngine(swapStore, swapStore,
			cal, ledgerSvc, ledgerPub, nil, nil)
		swapFees, ferr := settlement.NewSwapFreeFeeService(pool, ledgerSvc)
		if eerr != nil || ferr != nil {
			log.Warn("session lifecycle: rollover service construction failed — seam unwired",
				"engine_err", eerr, "fees_err", ferr)
		} else if rsvc, rerr := settlement.NewRolloverService(pool, swapEngine, cal, swapFees, rdb); rerr != nil {
			log.Warn("session lifecycle: rollover service wiring failed", "err", rerr)
		} else {
			rolloverRunner = func(ctx context.Context, now time.Time) (string, error) {
				rep, rerr := rsvc.RunOnce(ctx, now)
				return fmt.Sprintf("run=%d rolled=%d skipped=%q errors=%d",
					rep.RunID, rep.PositionsRolled, rep.SkipReason, len(rep.Errors)), rerr
			}
		}
	}
	// Task 18.3.15 — session.status WS→NATS bridge: the FIX gateway's
	// SessionStatusService consumes subject "session.status" (same name
	// as the WS advisory channel); both get the verbatim SessionEvent
	// payload. natsClient nil → WS-only, same as before.
	var sessionPub admin.SessionPublisher = wsSrv
	if natsClient != nil {
		sessionPub = fanoutSessionPub{wsSrv,
			sessionNATSPublisher{settlement.NatsPublisher{JS: natsClient.JetStream()}}}
	}
	sessSvc, err := admin.NewSessionService(admin.SessionDeps{
		RDB:      rdb,
		Shards:   shardIDsAsInt(shardMap),
		Pub:      sessionPub,
		Rollover: rolloverRunner,
		Symbols: func(ctx context.Context) ([]string, error) {
			rows, err := pool.Query(ctx,
				`SELECT symbol FROM instruments WHERE status = 'ACTIVE' ORDER BY symbol`)
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
		},
		Alert: admin.Alerter(func(ctx context.Context, severity, summary string) error {
			if opsAlerter == nil {
				return nil
			}
			return opsAlerter.Raise(ctx, settlement.OpsAlert{
				Severity: severity, Code: "SESSION_EFFECT_FAILED", Summary: summary})
		}),
		Logf: func(f string, a ...any) { log.Warn(fmt.Sprintf(f, a...)) },
	})
	if err != nil {
		return fmt.Errorf("session lifecycle service: %w", err)
	}
	// Boot reconcile (writes missing/stale session:state:* keys), then the
	// 5s scheduler. The Evaluate path is shared — reconcile IS a evaluate.
	if err := sessSvc.Reconcile(context.Background()); err != nil {
		log.Warn("session reconcile failed — scheduler will retry", "err", err)
	}
	go func() {
		if err := sessSvc.Run(sweepCtx, 5*time.Second); err != nil {
			log.Warn("session lifecycle loop exited", "err", err)
		}
	}()
	// --- end Phase-15 session wiring ---

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
	manLiqSvc := api.NewManualLiquidationService(pool,
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

	// Phase-16 algo surface — shares the order auth path (Bearer or
	// HMAC-signed API key, trade/read scopes).
	algoDeps := &api.AlgoDeps{OrderDeps: *orderDeps, Engine: algoEngine, Bots: gridEngine}

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
	// Task 8.5.3.2 — demo deployment: registration mints DEMO+virtual-seed
	// accounts; login/refresh re-arm the 30-day inactivity deadline.
	authnSvc.WithDemo(demoSvc)
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

	// ---- Phase-24 Tasks 24.3.6 / 24.3.7 / 24.3.19 — settlement ops ----
	// Failed-settlement exceptions, PB give-up reconciliation and the
	// write-off authority matrix. Resolution mutations never run inline —
	// they land inside the dual-control approval transaction via the
	// registered executors; ops alerts ride the shared NATS alerter.
	boDual := api.NewBackofficeDualQueue(dualSvc)
	var boAlerter backoffice.OpsAlertSink
	if opsAlerter != nil {
		oa := opsAlerter
		boAlerter = boOpsAlertSink{raise: func(ctx context.Context, sev, code, summary string, details map[string]string) error {
			return oa.Raise(ctx, settlement.OpsAlert{
				Severity: sev, Code: code, Summary: summary, Details: details})
		}}
	}
	settlementExcSvc := backoffice.NewExceptionService(
		backoffice.NewPgxExceptionStore(pool), boDual)
	pbReconSvc := backoffice.NewPBReconService(backoffice.NewPgxReconStore(pool),
		backoffice.PBReconOptions{OnAlert: func(ctx context.Context, code, summary string, detail map[string]any) {
			if opsAlerter == nil {
				return
			}
			d := map[string]string{}
			for k, v := range detail {
				d[k] = fmt.Sprint(v)
			}
			_ = opsAlerter.Raise(ctx, settlement.OpsAlert{
				Severity: "P1", Code: code, Summary: summary, Details: d})
		}})
	settlementOpsSvc := backoffice.NewOpsService(backoffice.NewPgxOpsStore(pool)).
		WithDual(boDual).WithAlerter(boAlerter)
	api.RegisterSettlementExceptionExecutor(dualSvc, settlementExcSvc)
	api.RegisterSettlementWriteOffExecutor(dualSvc, settlementOpsSvc)
	api.RegisterPBBreakResolveExecutor(dualSvc, pbReconSvc)

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

	// Task 8.5.3.2 — demo expiry sweep: DEMO accounts whose 30-day
	// inactivity deadline lapsed are mass-cancelled, closed (status→
	// CLOSED + account_closures + chained audit row), and lose sessions/
	// API keys — the same close-out conventions as Task 14.3.9. Runs on
	// the daily-UTC cadence; the sweep is idempotent and per-account
	// failures are logged without starving the batch.
	demoSvc.WithOrderCanceller(demoOrderCanceller{disp: orderDisp}).
		WithSessionTerminator(sessMgr).
		WithCredentialRevoker(apiKeyRevoker{ks: keyStore})
	runDailyUTC(sweepCtx, log, "demo-expiry", 30,
		func(ctx context.Context) {
			n, cerr := demoSvc.ExpireSweep(ctx, 500)
			if cerr != nil {
				log.Warn("demo expiry sweep failed", "err", cerr)
			}
			if n > 0 {
				log.Info("demo accounts expired", "closed", n)
			}
		})
	// Phase-14 Task 14.3.13 — profile pricing/scope/divisor mutations
	// run through the four-eyes queue; approval applies them in-tx.
	api.RegisterProductProfileExecutor(dualSvc, profileSvc)

	// Phase-15 Tasks 15.3.1/15.3.2/15.3.9 — instrument lifecycle state
	// machine (spec §7.1/§7.2). The service owns the transition matrix,
	// the instrument:status:{symbol} engine feed the C++ pre-trade
	// checker polls, the instrument:auction:{symbol} reopening-CALL
	// control key, the SUSPENDED 5-minute grace mass-cancel through
	// the orders dispatcher (the same pipeline compliance holds and
	// forced closures use), and the boot/periodic publication
	// reconciler — the reconciler also converges the feed after
	// dual-controlled transitions whose commits cannot publish in-tx.
	instrumentSvc, err := admin.NewInstrumentService(admin.InstrumentDeps{
		Pool:      pool,
		Roles:     adminRoleResolver,
		Feed:      admin.RedisStatusFeed{C: rdb.Client},
		Canceller: instrumentOrderCanceller{disp: orderDisp},
		WS:        wsSrv,
		OnStatus:  func() { marketCache.Forget("instruments:all") },
		Logf:      func(f string, a ...any) { log.Warn(fmt.Sprintf("instrument lifecycle: "+f, a...)) },
	})
	if err != nil {
		return err
	}
	api.RegisterInstrumentExecutors(dualSvc, instrumentSvc)
	// Phase-19 Tasks 19.3.21/19.3.24 — margin-model parameter changes
	// and entity_leverage_policy cells ride the §8.2 four-eyes queue:
	// approval evaluates the §13.12 ParamChangeGate (linked PASS run,
	// validator independence, freshness, parameter scope) or upserts
	// the effective-dated policy cell inside the approval tx. A gate
	// refusal fails the executor — the request stays PENDING and the
	// rejected attempt is recorded on margin_model_param_changes.
	if runStore, rerr := risk.NewPgModelRunStore(pool); rerr != nil {
		log.Warn("margin param gate: run store unavailable", "err", rerr)
	} else if chStore, cerr := risk.NewPgParamChangeStore(pool); cerr != nil {
		log.Warn("margin param gate: change store unavailable", "err", cerr)
	} else if pGate, gerr := risk.NewParamChangeGate(runStore, chStore, 0, nil); gerr != nil {
		log.Warn("margin param gate: construction failed", "err", gerr)
	} else {
		api.RegisterMarginParamChangeExecutor(dualSvc, pGate, chStore)
	}
	api.RegisterEntityLeveragePolicyExecutor(dualSvc)
	// Dual-approved instrument ops publish their engine feed + auction
	// arm + WS event post-commit — the approval tx cannot write Redis.
	// PublishCommitted re-derives the side effects from the committed
	// audit row (the four-eyes record), so dual-path transitions emit
	// exactly what a direct transition would.
	dualSvc.SetOnExecuted(func(ctx context.Context, req *admin.DualControlRequest) {
		if req.Operation != admin.OpInstrumentResume &&
			req.Operation != admin.OpInstrumentDelist {
			return
		}
		id, err := strconv.ParseInt(req.TargetID, 10, 64)
		if err != nil {
			log.Warn("instrument lifecycle: post-commit publish bad target", "target", req.TargetID, "err", err)
			return
		}
		if err := instrumentSvc.PublishCommitted(ctx, id); err != nil {
			log.Warn("instrument lifecycle: post-commit publish", "instrument", id, "err", err)
		}
	})
	go instrumentSvc.Run(sweepCtx, time.Second)

	// Phase-15 Tasks 15.3.11–13 — reference/session seams, listing
	// proposals + the §7.5 delist ladder, the ops board, and the
	// auction/fixing schedulers (spec §7.1/§7.4/§7.5, §24 #343/#352/
	// #401). The auction + fixing control keys ride the extended
	// admin.RedisStatusFeed surface (same instrument:auction:{symbol}
	// family the reopening CALL owns). The fixing scheduler's
	// PriceSource reads the oracle's published mark (RedisFixingMarkSource)
	// — absent/stale marks still record SKIPPED with the reason rather
	// than a fabricated rate (spec §2.7).
	instCalStore := instruments.NewCalendarStore(pool)
	instFeed := admin.RedisStatusFeed{C: rdb.Client}
	sessionCal, err := instruments.NewSessionCalendar()
	if err != nil {
		return fmt.Errorf("session calendar: %w", err)
	}
	instHolCal, calErr := settlement.LoadCalendar(sweepCtx, pool)
	if calErr != nil {
		log.Warn("instruments: holiday calendar unavailable — value-date/fixing seams fail closed",
			"err", calErr)
	}
	sessionSvc := instruments.NewSessionService(sessionCal, instHolCal)

	// ---- Phase-24 Tasks 24.3.1–.5 — nostro ledger, reconciliation,
	//      settlement confirmations, SWIFT journal, compliance exports ----
	// One PgNostroStore serves all four store seams (nostro accounts,
	// recon runs/breaks, instruction lookup, SWIFT journal) — they are
	// views over the same durable rows (migrations 018/035/261).
	nostroStore := backoffice.NewPgNostroStore(pool)
	nostroSvc, err := backoffice.NewNostroService(nostroStore)
	if err != nil {
		return fmt.Errorf("nostro service: %w", err)
	}
	swiftTracker, serr := backoffice.NewSwiftTracker(nostroStore)
	if serr != nil {
		return fmt.Errorf("swift tracker: %w", serr)
	}
	nostroReconSvc, serr := backoffice.NewNostroReconService(nostroStore, nostroSvc)
	if serr != nil {
		return fmt.Errorf("nostro recon: %w", serr)
	}
	nostroReconSvc.WithAlerter(opsAlerter).
		WithLogger(func(f string, a ...any) { log.Warn(fmt.Sprintf("nostro recon: "+f, a...)) })
	// The bank-statement polling seam is bound through the Task 24.3.12
	// ingested-statement surface — RunDaily then diffs nostro movements
	// against real statement rows, never a fabrication.
	nostroReconSvc.WithStatements(boNostroStatements{pool: pool})
	compReportSvc, serr := backoffice.NewComplianceReportService(
		regStore, amlSvc, sarSvc, baselSvc)
	if serr != nil {
		return fmt.Errorf("compliance reports: %w", serr)
	}

	// ---- Phase-24 Tasks 24.3.8/.9/.12/.20/.21 — CLS PvP, bilateral
	//      netting + SSI, statement ingestion, rail cut-offs, suspense ----
	// CLS: the member-side ISO 20022 transport adapter ships as a
	// deployment seam (EXC_CLS_* / HSM-bound credentials) — Member is nil
	// so dispatch-class operations fail closed CLS_MEMBER_UNAVAILABLE;
	// ingest/status/finality bookkeeping remains live.
	clsSvc, serr := settlement.NewClsPvpService(settlement.NewPgxClsStore(pool),
		settlement.ClsPvpOptions{
			Member:  nil,
			Poster:  ledgerSvc,
			Alerter: opsAlerter,
		})
	if serr != nil {
		return fmt.Errorf("cls pvp: %w", serr)
	}
	ssiSvc, serr := settlement.NewSsiService(settlement.NewPgxSsiStore(pool), nil)
	if serr != nil {
		return fmt.Errorf("ssi service: %w", serr)
	}
	var railCutoffSvc *settlement.RailCutoffService
	if instHolCal != nil {
		rc, rerr := settlement.NewRailCutoffService(
			settlement.NewPgxRailScheduleStore(pool), instHolCal, nil)
		if rerr != nil {
			return fmt.Errorf("rail cutoff: %w", rerr)
		}
		railCutoffSvc = rc
	} else {
		log.Warn("Phase-24: holiday calendar unavailable — rail cut-off, " +
			"netting dispatch and settlement-confirmation seams fail closed")
	}
	nettingSvc, serr := settlement.NewNettingService(settlement.NewPgxNettingStore(pool),
		settlement.NettingServiceOptions{
			SSI:      settlement.NewPgxSsiStore(pool),
			Cls:      clsSvc,
			Cutoff:   railCutoffSvc, // nil → dispatch refuses (fail-closed)
			Calendar: instHolCal,
			Alerter:  opsAlerter,
		})
	if serr != nil {
		return fmt.Errorf("netting service: %w", serr)
	}
	// Task 24.3.21: unmatched statement credits route through the single
	// Phase-11 quarantine pipeline — ScreenInbound decides attribution,
	// never the settlement side.
	suspenseSvc, serr := settlement.NewSuspenseService(suspenseGuard{g: depositGuard})
	if serr != nil {
		return fmt.Errorf("suspense router: %w", serr)
	}
	pgStmtStore := settlement.NewPgxStatementStore(pool)
	stmtIngestSvc, serr := settlement.NewStatementIngestionService(
		pgStmtStore,
		settlement.StatementIngestionOptions{
			Parser:   backoffice.StatementParsers{},
			Suspense: suspenseSvc,
			Alerter:  opsAlerter,
		})
	if serr != nil {
		return fmt.Errorf("statement ingestion: %w", serr)
	}
	// Task 24.3.3: MT900/MT910 finality flips the settlement instruction
	// SETTLED inside the settlement store's tx — that needs a live
	// SettlementService, which requires the venue BIC (EXC_SENDER_BIC)
	// and the holiday calendar; missing either fails the whole confirm
	// seam closed rather than settling off-ledger. A nil settleSvc also
	// parks the manual value-date-roll endpoint (fail-closed handler is
	// mounted conditionally below).
	var settleSvc *settlement.SettlementService
	var confirmationSvc *backoffice.ConfirmationService
	if bic := strings.TrimSpace(os.Getenv("EXC_SENDER_BIC")); bic != "" && instHolCal != nil {
		// Poster carries the PD fee journals inside the confirm tx —
		// §5.45.2 fee recognition rides the settlement-confirm commit.
		settleStore := settlement.NewPgxSettlementStore(pool)
		settleStore.Poster = ledgerSvc
		settleSvc, serr = settlement.NewSettlementService(
			settleStore, instHolCal,
			settlement.SettlementOptions{
				SenderBIC: bic,
				Cutoff:    railCutoffSvc,
			})
		if serr != nil {
			return fmt.Errorf("settlement service: %w", serr)
		}
		cs, cerr := backoffice.NewConfirmationService(nostroStore,
			boSettlementConfirmer{svc: settleSvc}, nostroSvc)
		if cerr != nil {
			return fmt.Errorf("confirmation service: %w", cerr)
		}
		confirmationSvc = cs.WithRecorder(swiftTracker).
			WithCalendar(instHolCal).
			WithAlerter(opsAlerter)
		// Two-business-day overdue sweep — unconfirmed dispatches page P2
		// (deduped via the ops-alert journal).
		go func() {
			t := time.NewTicker(time.Hour)
			defer t.Stop()
			for {
				select {
				case <-sweepCtx.Done():
					return
				case <-t.C:
					if _, err := confirmationSvc.SweepOverdue(sweepCtx, 500); err != nil {
						log.Warn("confirmation overdue sweep", "err", err)
					}
				}
			}
		}()
	} else {
		log.Warn("Phase-24: EXC_SENDER_BIC or holiday calendar missing — " +
			"settlement-confirmation endpoint fails closed SERVICE_DEGRADED")
	}
	// Daily nostro reconciliation — 04:30 UTC, after the EOD statement
	// window and before treasury checks.
	runDailyUTC(sweepCtx, log, "nostro-recon", 270, func(ctx context.Context) {
		if _, err := nostroReconSvc.RunDaily(ctx,
			time.Now().UTC().AddDate(0, 0, -1), nil); err != nil {
			log.Warn("nostro recon daily run", "err", err)
		}
	})

	// ---- Phase-24 Tasks 24.3.10/.15 — bunched-order + post-trade
	//      allocations ----
	// PgAllocStore posts the 2090 park/unpark journals through the shared
	// ledger inside its own SERIALIZABLE tx. Execs/Reports stay nil: the
	// exec_id→trade resolver and the 35=AK FIX initiator are session-
	// bound deployment seams — nil fails the FIX ingest path closed
	// while the admin surface stays live (§27 deviation).
	allocStore, aerr := backoffice.NewPgAllocStore(pool, ledgerSvc)
	if aerr != nil {
		return fmt.Errorf("allocation store: %w", aerr)
	}
	allocEngine, aerr := backoffice.NewEngine(backoffice.EngineDeps{
		Store:   allocStore,
		Roles:   adminRoleResolver,
		Execs:   nil,
		Reports: nil,
		Alerter: opsAlerter,
	})
	if aerr != nil {
		return fmt.Errorf("allocation engine: %w", aerr)
	}
	api.RegisterAllocationCorrectExecutor(dualSvc, allocEngine)
	allocDeps := api.AllocationDeps{
		Backend: allocEngine, Dual: dualSvc, TrustProxy: true}
	// T+0 unallocated-remainder sweep — hourly; groups past the
	// end-of-day cutoff escalate to a durable P2 ops alert.
	go func() {
		t := time.NewTicker(time.Hour)
		defer t.Stop()
		for {
			select {
			case <-sweepCtx.Done():
				return
			case <-t.C:
				now := time.Now().UTC()
				cutoff := time.Date(now.Year(), now.Month(), now.Day(),
					23, 0, 0, 0, time.UTC)
				if res, err := allocEngine.EscalateUnallocated(sweepCtx, cutoff); err != nil {
					log.Warn("allocation unallocated sweep", "err", err)
				} else if res != nil && res.Escalated > 0 {
					log.Warn("allocation T+0 unallocated escalations",
						"groups", res.GroupIDs)
				}
			}
		}
	}()

	// ---- Phase-24 Tasks 24.3.13/.14 — CSDR discipline + PB credit
	//      restitution — plus the Task 24.3.19 ops monitors ----
	// FX legs classify FX_CLOSEOUT (never CSDR on this venue); the
	// classifier resolves regime per leg so a future securities line
	// fails into the right discipline. Restitution adjusts PB DSL/NOP
	// inside the caller's tx via the Phase-19 store's RestituteInTx.
	csdrSvc := backoffice.NewCSDRService(
		backoffice.NewPgxFailStore(pool), backoffice.NewPgxLegClassifier(pool))
	restitutionSvc, rerr := backoffice.NewRestitutionService(
		backoffice.NewPgxRestitutionStore(pool),
		backoffice.CreditAdjusterFunc(func(ctx context.Context, tx any,
			clientID int64, pair string, dslUSD, nopUSD decimal.Decimal) (int, error) {
			raw, ok := tx.(pgx.Tx)
			if !ok {
				return 0, excerrors.New("SERVICE_DEGRADED",
					"pb credit restitution requires a pgx transaction")
			}
			return risk.RestituteInTx(ctx, raw, risk.RestitutionInput{
				ClientID:       clientID,
				CurrencyPair:   pair,
				DSLConsumedUSD: dslUSD,
				NOPConsumedUSD: nopUSD})
		}))
	if rerr != nil {
		return fmt.Errorf("restitution service: %w", rerr)
	}
	restitutionSvc.WithAlerter(boRestitutionAlerter{a: opsAlerter})
	// Margin recalc and the FIX drop-copy notifier stay unwired — the
	// recalc flag rides the restitution row for the risk service, and
	// the drop-copy transport is a session-bound deployment seam.
	buyInSvc := backoffice.NewBuyInService(
		backoffice.BuyInStoreOf(backoffice.NewPgxFailStore(pool)),
		boMarkPricer{pool: pool, mark: markProv},
		func(ctx context.Context, code, summary string, detail map[string]any) {
			if opsAlerter == nil {
				return
			}
			d := map[string]string{}
			for k, v := range detail {
				d[k] = fmt.Sprint(v)
			}
			_ = opsAlerter.Raise(ctx, settlement.OpsAlert{
				Severity: "P2", Code: code, Summary: summary, Details: d})
		})
	// Ops monitors: the close-out mark source and the rail cut-off
	// evaluator join the Task 24.3.19 service; the rail-dispatch seam
	// stays unwired — failover payments park (fail-closed) until a
	// production transport binds.
	settlementOpsSvc.
		WithPricer(boMarkPricer{pool: pool, mark: markProv})
	if railCutoffSvc != nil {
		settlementOpsSvc.WithCutoffs(boCutoffEvaluator{svc: railCutoffSvc})
	}
	// Daily fail detection + penalty accrual (CSDR/FX close-out) and the
	// buy-in notify/execute ladders — 06:00/06:15 UTC.
	runDailyUTC(sweepCtx, log, "csdr-fails", 360, func(ctx context.Context) {
		day := time.Now().UTC().AddDate(0, 0, -1)
		if _, err := csdrSvc.DetectFails(ctx, day); err != nil {
			log.Warn("csdr fail detection", "err", err)
		}
		if _, err := csdrSvc.AccruePenalties(ctx, day); err != nil {
			log.Warn("csdr penalty accrual", "err", err)
		}
	})
	runDailyUTC(sweepCtx, log, "csdr-buyin", 375, func(ctx context.Context) {
		day := time.Now().UTC()
		if _, err := buyInSvc.NotifyDue(ctx, day); err != nil {
			log.Warn("buy-in notifications", "err", err)
		}
		if _, err := buyInSvc.ExecuteDue(ctx, day); err != nil {
			log.Warn("buy-in executions", "err", err)
		}
	})
	// Hourly settlement-ops monitors: aging escalation, nostro funding
	// thresholds, CLS pay-in funding, failover dispatch retry, and the
	// Herstatt exposure window.
	go func() {
		t := time.NewTicker(time.Hour)
		defer t.Stop()
		for {
			select {
			case <-sweepCtx.Done():
				return
			case <-t.C:
				if _, err := settlementOpsSvc.SweepAging(sweepCtx); err != nil {
					log.Warn("settlement aging sweep", "err", err)
				}
				if _, err := settlementOpsSvc.EvaluateNostroThresholds(sweepCtx); err != nil {
					log.Warn("nostro threshold evaluation", "err", err)
				}
				if _, err := settlementOpsSvc.CLSPayInMonitor(sweepCtx); err != nil {
					log.Warn("cls pay-in monitor", "err", err)
				}
				if _, err := settlementOpsSvc.ProcessFailover(sweepCtx); err != nil {
					log.Warn("settlement failover", "err", err)
				}
				if _, err := settlementOpsSvc.MonitorHerstatt(sweepCtx); err != nil {
					log.Warn("herstatt monitor", "err", err)
				}
			}
		}
	}()

	// ---- Phase-24 Tasks 24.3.11/.16–.18 — client money, treasury,
	//      independent assurance ----
	// One backoffice.PgStore (migration 056/082/083 tables) serves the
	// client-money, treasury and assurance store interfaces.
	boStore, berr := backoffice.NewPgStore(pool)
	if berr != nil {
		return fmt.Errorf("backoffice store: %w", berr)
	}
	treasurySvc, terr := backoffice.NewTreasuryService(backoffice.TreasuryDeps{
		Store:    boStore,
		Outflows: pgStressedOutflows{pool: pool},
		Alerter:  opsAlerter,
		Resolver: adminRoleResolver,
	})
	if terr != nil {
		return fmt.Errorf("treasury service: %w", terr)
	}
	assuranceSvc, terr := backoffice.NewAssuranceService(backoffice.AssuranceDeps{
		Store:    boStore,
		Alerter:  opsAlerter,
		Resolver: adminRoleResolver,
	})
	if terr != nil {
		return fmt.Errorf("client-money assurance: %w", terr)
	}
	clientMoneySvc, terr := backoffice.NewClientMoneyService(backoffice.ClientMoneyDeps{
		Store:      boStore,
		Poster:     backoffice.PgJournalPoster{L: ledgerSvc},
		Fund:       cmFundSource{},
		House:      treasurySvc,
		NBP:        cmNBPSource{pool: pool},
		Statements: cmStatementSource{pool: pool},
		Suspension: cmSuspension{h: reconciliation.NewPgHalter(pool, rdb)},
		Notices:    nil, // regtech dispatcher is a deployment seam — notices persist PENDING + retry
		Alerter:    opsAlerter,
		Resolver:   adminRoleResolver,
	})
	if terr != nil {
		return fmt.Errorf("client money service: %w", terr)
	}
	// The daily segregation reconciliation, stress test and liquidity
	// assessment are deliberately NOT scheduled here — each requires a
	// finance-write role (RunDailyReconciliation/RunStressTest/
	// EvaluateLiquidity call requireRole), and the distinct-principal
	// sign-off is a regulatory control that must carry a human actor id.
	// They run through the admin surface (/api/v1/admin/client-money/*,
	// /api/v1/admin/treasury/*). The deadline/notice retry sweep is
	// ungated and does run on a cadence.
	go func() {
		t := time.NewTicker(15 * time.Minute)
		defer t.Stop()
		for {
			select {
			case <-sweepCtx.Done():
				return
			case <-t.C:
				if err := clientMoneySvc.SweepDeadlines(sweepCtx); err != nil {
					log.Warn("client money deadline sweep", "err", err)
				}
				if err := clientMoneySvc.DispatchNotices(sweepCtx); err != nil {
					log.Warn("client money notice dispatch", "err", err)
				}
			}
		}
	}()

	listingSvc, err := instruments.NewListingService(instruments.ListingDeps{
		Pool:        pool,
		Roles:       adminRoleResolver,
		Instruments: instrumentSvc,
		Dual:        dualSvc,
		Sessions:    sessionCal,
		WS:          wsSrv,
		Logf:        func(f string, a ...any) { log.Warn(fmt.Sprintf("listing: "+f, a...)) },
	})
	if err != nil {
		return fmt.Errorf("listing service: %w", err)
	}
	instruments.RegisterListingExecutor(dualSvc, listingSvc)
	instruments.RegisterCalendarExecutor(dualSvc, instCalStore)

	opsBoardSvc, err := instruments.NewOpsBoardService(instruments.OpsBoardDeps{
		Pool: pool,
		Feed: instFeed,
		Logf: func(f string, a ...any) { log.Warn(fmt.Sprintf("ops board: "+f, a...)) },
	})
	if err != nil {
		return fmt.Errorf("ops board service: %w", err)
	}

	auctionSched, err := instruments.NewAuctionScheduler(instruments.AuctionDeps{
		Pool:        pool,
		Store:       instCalStore,
		Feed:        instFeed,
		Orders:      instruments.NewPgAuctionOrders(pool),
		Canceller:   auctionTypeCanceller{disp: orderDisp},
		Instruments: instrumentSvc,
		WS:          wsSrv,
		Logf:        func(f string, a ...any) { log.Warn(fmt.Sprintf("auction scheduler: "+f, a...)) },
	})
	if err != nil {
		return fmt.Errorf("auction scheduler: %w", err)
	}
	fixingSched, err := instruments.NewFixingScheduler(instruments.FixingDeps{
		Pool:     pool,
		Store:    instCalStore,
		Holidays: instHolCal,
		// Phase-19.5 oracle seam now wired: the recorded fix is the
		// oracle mark live at the firing instant (staleness-bounded —
		// absent/stale marks still record SKIPPED, never fabricated).
		Prices: oracle.NewRedisFixingMarkSource(rdb),
		Feed:   instFeed,
		Orders: instruments.NewPgFixingOrders(pool),
		WS:     wsSrv,
		Logf:   func(f string, a ...any) { log.Warn(fmt.Sprintf("fixing scheduler: "+f, a...)) },
	})
	if err != nil {
		return fmt.Errorf("fixing scheduler: %w", err)
	}
	go auctionSched.Run(sweepCtx, 5*time.Second)
	go fixingSched.Run(sweepCtx, 10*time.Second)

	// Phase-16 Task 16.3.25 — MOO/MOC queue: the freeze gate rejects
	// cancel/amend during the T-30s window + armed CALL; the private
	// notify seam emits order.queued / order.auction_fill /
	// order.cancelled frames; the injector replays queued orders onto
	// the engine as MARKET OrderNew once a CALL arms.
	orderSvc.WithAuction(auctionFreezeGate{feed: instFeed, cal: instCalStore})
	orderSvc.WithNotify(wsSrv.PublishPrivate)
	go orders.NewInjector(orderSvc, instFeed, 2*time.Second,
		func(f string, a ...any) {
			log.Warn(fmt.Sprintf("auction injector: "+f, a...))
		}).Run(sweepCtx)
	// Phase-16 Tasks 16.3.14/.20 — boot composite recovery: brackets
	// whose parent filled while down get children placed; EXECUTING
	// lists re-drive activation or close. Durable rows are the truth.
	orderSvc.RecoverComposites(sweepCtx, func(f string, a ...any) {
		log.Warn(fmt.Sprintf("composite recovery: "+f, a...))
	})

	// Phase-16 Task 16.3.9 — order-level fixing executor: FIXING orders
	// are persisted + RESERVED at admission (never dispatched to the
	// engine); the service consumes benchmark_fixings rows and crosses
	// residuals exactly at the published rate — unmatched imbalance
	// stays queued/audited, never fabricated into LP fills. Bound via
	// With* (same late-binding pattern as WithAdmission) since the
	// calendar store only exists here.
	fixingSvc, ferr := algo.NewFixingService(algo.FixingDeps{
		Pool:      pool,
		Poster:    ledgerSvc,
		TxPoster:  ledgerSvc,
		Calendar:  instCalStore,
		Ref:       orderStore,
		Pub:       ledgerPub,
		ValueDate: instHolCal.RollValueDate,
		Logf:      func(f string, a ...any) { log.Warn(fmt.Sprintf("fixing svc: "+f, a...)) },
	})
	if ferr != nil {
		return fmt.Errorf("fixing service: %w", ferr)
	}
	orderSvc.WithFixing(fixingSvc)
	go fixingSvc.Run(sweepCtx, 15*time.Second)
	// Listing/delist ladder sweep: scheduled activations + the §7.5
	// notice→approval→close-only progression on a 30s cadence.
	go func() {
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-sweepCtx.Done():
				return
			case <-t.C:
				if n, err := listingSvc.ActivateDue(sweepCtx); err != nil {
					log.Warn("listing activation sweep", "err", err)
				} else if n > 0 {
					log.Info("listing activations applied", "count", n)
				}
				if err := listingSvc.AdvanceDelisting(sweepCtx); err != nil {
					log.Warn("delist ladder sweep", "err", err)
				}
			}
		}
	}()
	// Phase-16 Task 16.3.21 — recurring FX conversion, drift-triggered
	// rebalancing and the approved strategy marketplace (migration 077,
	// §24 #296). SessionService is the market-hours gate; every run leg
	// is a firm CLOB order through orders.Service — ruling R14 forbids a
	// principal/RFQ conversion path, so none exists here. The sweep
	// claims due slots idempotently (one open run per strategy) and
	// market-closed slots are recorded SKIPPED, never faked-executed.
	stratSvc, err := strategies.NewService(strategies.Options{
		Store:    strategies.NewStore(pool),
		Orders:   orderSvc,
		Read:     orderStore,
		Sessions: sessionSvc,
		Book:     marketStore,
		Balances: fundStore,
		USD:      usdConv,
	})
	if err != nil {
		return fmt.Errorf("strategies service: %w", err)
	}
	go func() {
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-sweepCtx.Done():
				return
			case <-t.C:
				if n, err := stratSvc.Sweep(sweepCtx); err != nil {
					log.Warn("strategy sweep failed", "err", err)
				} else if n > 0 {
					log.Info("strategy runs dispatched", "count", n)
				}
			}
		}
	}()

	// Phase-16 Tasks 16.3.19/16.3.21 REST deps — both share the order
	// auth seam (Bearer claims or HMAC-signed API key, read/trade
	// scopes); handlers stay thin, semantics live in bots/strategies.
	gridDeps := &api.GridBotDeps{Engine: gridEngine, Orders: orderDeps}
	stratDeps := &api.StrategyDeps{SVC: stratSvc, Orders: orderDeps, TrustProxy: true}

	// Phase-15 Task 15.3.5 — trade bust / price-adjust (spec §5.29,
	// §7.2, §7.3.4, §24 #138). The correction runs through the landed
	// §5.3 contract: SERIALIZABLE tx + per-account Redis mutexes +
	// LedgerService.PostJournal (balanced reversal/adjustment journals,
	// idempotency-keyed) + post-commit BalanceChanged dispatch.
	tradeBustSvc, err := admin.NewTradeBustService(admin.TradeBustDeps{
		Pool:      pool,
		Poster:    ledgerSvc,
		Locks:     rdb,
		Publisher: ledgerPub,
		Roles:     adminRoleResolver,
		Notifier:  notifSvc,
		Logf:      func(f string, a ...any) { log.Warn(fmt.Sprintf("trade bust: "+f, a...)) },
	})
	if err != nil {
		return fmt.Errorf("trade bust service: %w", err)
	}

	// Phase-15 Task 15.3.8 — instrument maintenance maker-checker
	// (spec §7.1/§7.2/§7.4, §24 #234): three-stage creation chain,
	// effective-dated parameter changes at the next session boundary,
	// Super-Admin emergency changes with P1 alert, delisting governance
	// riding the OpInstrumentDelist four-eyes queue. ApplyDue is the
	// session-boundary activation sweep — it also reconciles delist
	// requests against the dual-control store.
	maintSvc, err := admin.NewInstrumentMaintenanceService(admin.InstrumentMaintenanceDeps{
		Pool:        pool,
		Roles:       adminRoleResolver,
		Instruments: instrumentSvc,
		Dual:        dualSvc,
		WS:          wsSrv,
		NATS:        settlement.NatsPublisher{JS: natsJet(natsClient)},
		Alerter:     maintenanceAlerter{opsAlerter},
		Logf:        func(f string, a ...any) { log.Warn(fmt.Sprintf("instrument maintenance: "+f, a...)) },
	})
	if err != nil {
		return fmt.Errorf("instrument maintenance service: %w", err)
	}
	go func() {
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-sweepCtx.Done():
				return
			case <-t.C:
				if _, err := maintSvc.ApplyDue(sweepCtx); err != nil {
					log.Warn("instrument maintenance sweep", "err", err)
				}
				if _, err := tradeBustSvc.ExpirePending(sweepCtx); err != nil {
					log.Warn("trade bust expire sweep", "err", err)
				}
			}
		}
	}()

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
	// Phase-21 Task 21.3.3 — escalate-to-SAR now drafts the report
	// against the hold row (dedup anchor 'hold:{id}').
	holdSvc.WithSARDraft(sarSvc)

	// ---- Phase-21 wave-2 cluster: Tasks 21.3.8/21.3.12/21.3.21/
	//      21.3.24/21.3.27 — market-abuse enforcement, surveillance
	//      case management, RTS 6 algo/DEA controls, employee dealing,
	//      signal tuning + audit-trail query (migrations 079/239-242).
	//      All reuse the seams above — holdSvc (freeze + resting-order
	//      cancel), killSvc (SCOPE_ACCOUNT restrict), sarSvc
	//      (escalation drafts), the shared role resolver and the
	//      ScanAccountsResolver hash→account probe. ----
	accountResolver := compliance.ScanAccountsResolver(pool)
	enforceSvc := compliance.NewEnforcementService(pool, rdb.Client,
		holdSvc, killSvc, accountResolver,
		compliance.HoldRoleResolver(adminRoleResolver),
		compliance.EnforcementNotifier(acctNotify))
	orderSvc.WithEnforcementGate(enforceSvc)

	rts6Svc := compliance.NewRTS6Service(pool,
		compliance.HoldRoleResolver(adminRoleResolver),
		holdOpsAlerter{inner: freezeOpsAlerter{pool: pool, page: opsAlerter}})
	orderSvc.WithAlgoCertGate(rts6Svc)

	caseSvc := compliance.NewCaseService(pool,
		compliance.HoldRoleResolver(adminRoleResolver),
		accountResolver, sarSvc, enforceSvc,
		holdOpsAlerter{inner: freezeOpsAlerter{pool: pool, page: opsAlerter}})
	// Task 21.3.21 — AML monitoring findings now open real
	// surveillance cases (the AuditCaseSink was the pre-case fallback).
	monSvc.WithCaseSink(caseSvc)

	dealingSvc := compliance.NewEmployeeDealingService(pool,
		compliance.HoldRoleResolver(adminRoleResolver),
		holdOpsAlerter{inner: freezeOpsAlerter{pool: pool, page: opsAlerter}})
	orderSvc.WithDealingGate(dealingSvc)

	restrictedSvc := admin.NewRestrictedListService(pool,
		adminRoleResolver)
	tuningSvc := compliance.NewTuningService(pool,
		compliance.HoldRoleResolver(adminRoleResolver))
	reportingVals := compliance.NewReportingValues(pool,
		compliance.HoldRoleResolver(adminRoleResolver))
	auditTrail := compliance.NewAuditTrailQuery(pool)

	// ---- Phase-21 wave-3: GDPR & geo-block (21.3.7), data residency
	//      (21.3.18), MiFID II comms recording (21.3.20), CRS/FATCA tax
	//      reporting (21.3.22), financial promotions (21.3.26).
	//      Migrations 062/243-246. ----
	// GDPR exports persist through the reports object store when one is
	// configured (SHA-256'd artifact + request row); without a sink the
	// manifest stays inline (dev only) — the service degrades
	// gracefully rather than refusing exports.
	gdprSvc, err := compliance.NewGDPRService(pool, sessMgr, reportObjects)
	if err != nil {
		return fmt.Errorf("gdpr service: %w", err)
	}

	// Geo-block resolver — EXC_GEO_CIDR_MAP points at a JSON
	// {"CIDR": "ISO-alpha-2", ...} map (a GeoIP2 adapter slots behind
	// the same GeoResolver interface). Unset → nil resolver →
	// unattributed traffic passes but every resolver ERROR fails
	// closed; a configured-but-broken map aborts boot rather than
	// running the gate blind.
	var geoResolver compliance.GeoResolver
	if p := strings.TrimSpace(os.Getenv("EXC_GEO_CIDR_MAP")); p != "" {
		b, rerr := os.ReadFile(p)
		if rerr != nil {
			return fmt.Errorf("geo cidr map: %w", rerr)
		}
		var m map[string]string
		if jerr := json.Unmarshal(b, &m); jerr != nil {
			return fmt.Errorf("geo cidr map: %w", jerr)
		}
		geoResolver, err = compliance.NewCIDRResolver(m)
		if err != nil {
			return fmt.Errorf("geo cidr map: %w", err)
		}
	}
	geoGate, err := compliance.NewGeoGate(pool, geoResolver,
		os.Getenv("EXC_TRUST_PROXY") == "1")
	if err != nil {
		return fmt.Errorf("geo gate: %w", err)
	}

	residencySvc, err := compliance.NewResidencyService(pool)
	if err != nil {
		return fmt.Errorf("residency service: %w", err)
	}

	// Comms recording requires WORM-capable object storage — a dedicated
	// bucket via EXC_COMMS_S3_BUCKET (object-lock enforced by the
	// bucket policy), falling back to the reports bucket. With no
	// object store at all the service is absent and the live registry
	// rows mount the fail-closed 503 shim.
	var commsObjects objectstore.Client
	if bucket := strings.TrimSpace(os.Getenv("EXC_COMMS_S3_BUCKET")); bucket != "" {
		ocfg := objectstore.ConfigFromEnv(bucket, os.Getenv)
		var oerr error
		if cfg.Environment != "production" && ocfg.Endpoint != "" {
			commsObjects, oerr = objectstore.NewDev(context.Background(), ocfg)
		} else {
			commsObjects, oerr = objectstore.NewAWS(context.Background(), ocfg)
		}
		if oerr != nil {
			return fmt.Errorf("comms object store: %w", oerr)
		}
	} else {
		commsObjects = reportObjects
	}
	var commsSvc *compliance.CommsRecordingService
	if commsObjects != nil {
		commsSvc, err = compliance.NewCommsRecordingService(pool, commsObjects,
			os.Getenv("EXC_S3_KMS_KEY_ID"),
			compliance.HoldRoleResolver(adminRoleResolver))
		if err != nil {
			return fmt.Errorf("comms recording service: %w", err)
		}
	} else {
		log.Warn("no object store — comms recording routes serve fail-closed 503")
	}

	// CRS/FATCA reporting stamps the reporting-FI identity on every
	// artifact — EXC_TAX_VENUE_NAME / _IN / _COUNTRY configure it.
	// Unset in dev leaves the routes live-but-degraded (503 shim).
	var taxReportSvc *compliance.TaxReportService
	if v := os.Getenv("EXC_TAX_VENUE_NAME"); strings.TrimSpace(v) != "" {
		taxReportSvc, err = compliance.NewTaxReportService(pool, kycStore,
			compliance.HoldRoleResolver(adminRoleResolver),
			compliance.TaxVenue{
				Name:    v,
				IN:      os.Getenv("EXC_TAX_VENUE_IN"),
				Country: os.Getenv("EXC_TAX_VENUE_COUNTRY"),
			})
		if err != nil {
			return fmt.Errorf("tax report service: %w", err)
		}
	} else {
		log.Warn("EXC_TAX_VENUE_* unset — tax-reporting routes serve fail-closed 503")
	}

	promoSvc, err := compliance.NewPromotionService(pool,
		compliance.HoldRoleResolver(adminRoleResolver))
	if err != nil {
		return fmt.Errorf("promotion service: %w", err)
	}
	promoGate, err := content.NewGate(content.NewPgPromotionStore(pool))
	if err != nil {
		return fmt.Errorf("promotion render gate: %w", err)
	}

	// Sweeps: case SLA breach pages (60s — same cadence as hold SLA),
	// unassigned-case round-robin + enforcement auto-ladder + RTS 6
	// cert/assessment expiry + pre-clearance expiry + restricted-list
	// widening (hourly — the same idioms as the AML sweep above).
	go func() {
		t := time.NewTicker(60 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-sweepCtx.Done():
				return
			case <-t.C:
				if n, err := caseSvc.SweepSLA(sweepCtx); err != nil {
					log.Warn("surveillance case SLA sweep", "err", err)
				} else if n > 0 {
					log.Warn("surveillance cases breached SLA", "count", n)
				}
			}
		}
	}()
	go func() {
		t := time.NewTicker(time.Hour)
		defer t.Stop()
		for {
			select {
			case <-sweepCtx.Done():
				return
			case <-t.C:
				if n, err := caseSvc.SweepUnassigned(sweepCtx, 50); err != nil {
					log.Warn("case auto-assign sweep", "err", err)
				} else if n > 0 {
					log.Info("surveillance cases auto-assigned", "count", n)
				}
				if n, err := enforceSvc.AutoEnforce(sweepCtx, 200); err != nil {
					log.Warn("enforcement auto sweep", "err", err)
				} else if n > 0 {
					log.Info("enforcement auto-actions taken", "count", n)
				}
				if n, err := rts6Svc.SweepCertExpiry(sweepCtx); err != nil {
					log.Warn("rts6 cert expiry sweep", "err", err)
				} else if n > 0 {
					log.Info("algo certifications expired", "count", n)
				}
				if _, err := rts6Svc.SweepAssessmentDue(sweepCtx); err != nil {
					log.Warn("rts6 assessment sweep", "err", err)
				}
				if n, err := dealingSvc.SweepClearanceExpiry(sweepCtx); err != nil {
					log.Warn("pre-clearance expiry sweep", "err", err)
				} else if n > 0 {
					log.Info("pre-clearances expired", "count", n)
				}
				if n, err := restrictedSvc.SyncScheduledEvents(sweepCtx); err != nil {
					log.Warn("restricted-list widening sweep", "err", err)
				} else if n > 0 {
					log.Info("restricted windows widened for scheduled events", "count", n)
				}
				// Task 21.3.26 — approved-until expiry; a lapsed
				// promotion leaves the render gate immediately.
				if n, err := promoSvc.ExpireSweep(sweepCtx); err != nil {
					log.Warn("promotion expiry sweep", "err", err)
				} else if n > 0 {
					log.Info("financial promotions expired", "count", n)
				}
			}
		}
	}()
	// Task 21.3.20 — comms-recording drain: pending register rows get
	// their WORM object write + chain link on a 30s cadence (the row
	// is durable before the object lands; a stopped sweep stalls
	// uploads, never loses them).
	if commsSvc != nil {
		go func() {
			t := time.NewTicker(30 * time.Second)
			defer t.Stop()
			for {
				select {
				case <-sweepCtx.Done():
					return
				case <-t.C:
					if n, err := commsSvc.Drain(sweepCtx, 100); err != nil {
						log.Warn("comms recording drain", "err", err)
					} else if n > 0 {
						log.Debug("comms recordings uploaded", "count", n)
					}
				}
			}
		}()
	}
	// ---- end Phase-21 wave-2 cluster ----

	// Phase-21 Tasks 21.3.11/21.3.23 — the screening service binds the
	// screener + quarantine wrapper + hold workflow + C++-hook flag
	// publisher. It serves the KYC post-approve hook (bound above at
	// the lifecycle service), the delta-rescreen hook, the replay-hit
	// routing and the admin screening endpoints. Unbound screener →
	// unbound service (the documented unwired residual).
	if screener != nil {
		screeningSvc, err = compliance.NewScreeningService(
			compliance.ScreeningOptions{
				Screener: screener,
				Gated:    gatedScreener,
				Store:    screeningStore,
				Holds:    holdSvc,
				Flags:    sanctionsFlags,
				Alerter:  compAlerter,
				Auditor:  compliance.AdminAuditSink{Pool: pool},
			})
		if err != nil {
			return fmt.Errorf("screening service: %w", err)
		}
		sanctionsReplayer.WithOnHit(screeningSvc.HandleReplayHit)
		// List-update deltas re-screen the roster off the reload path —
		// the hook spawns so a large account base never blocks Reload.
		screener.WithDeltaHook(func(d compliance.ListDelta) {
			go func() {
				if n, derr := screeningSvc.DeltaRescreen(sweepCtx, d); derr != nil {
					log.Error("sanctions delta rescreen failed", "err", derr)
				} else if n > 0 {
					log.Warn("sanctions delta produced new hits",
						"accounts", n)
				}
			}()
		})
		// Daily rescreen sweep — the 30-day PEP/ongoing-monitoring
		// cadence (screening stamps ride the audit trail until the
		// sibling screening-table migration lands).
		go func() {
			t := time.NewTicker(24 * time.Hour)
			defer t.Stop()
			for {
				select {
				case <-sweepCtx.Done():
					return
				case <-t.C:
					if n, rerr := screeningSvc.RescreenDue(sweepCtx, 500); rerr != nil {
						log.Warn("pep rescreen sweep failed", "err", rerr)
					} else if n > 0 {
						log.Info("pep rescreen sweep completed", "screened", n)
					}
				}
			}
		}()
	} else {
		log.Warn("screening service unbound — EXC_SANCTIONS_LIST_DIR required")
	}

	// Phase-21 Task 21.3.23 — ARM/APA submission repair + resubmission:
	// sweeps the migration-059 transport ledger for PENDING/NACK rows,
	// retransmits via the same vendor clients the dispatcher uses, and
	// dead-letters to FAILED + P1 past the 2h post-recovery deadline.
	resubmitSvc := compliance.NewResubmissionService(
		compliance.NewPgSubmissionRepairStore(pool),
		repairResubmitter{clients: regClients}).
		WithAlerter(compAlerter).
		WithAuditor(compliance.AdminAuditSink{Pool: pool})
	go resubmitSvc.Run(sweepCtx, 30*time.Second)

	// Screening admin dep bundle — nil members surface 503 at their
	// endpoints (unwired screener → every route still mounts live).
	screeningDeps := api.ScreeningAdminDeps{
		Gate: sanctionsGate, Screener: screener,
		Refresher: sanctionsRefresher, Replayer: sanctionsReplayer,
		Queue: sanctionsQueue, Screening: screeningSvc,
		SubjectFor: screeningStore.SubjectFor,
	}

	// Phase-21 Tasks 21.3.3/21.3.6 — surveillance-signal SAR ingest +
	// AML business-day sweep. Two JetStream bindings cover both event
	// spellings (§14.1c 'surveillance.signals.>' and the task's
	// 'compliance' stream); sar_reports.source_ref dedup makes the
	// overlap harmless. DraftFromOpenSignals is the PG backstop —
	// Phase-17 detectors write surveillance_signals directly, so the
	// pipeline never depends on a NATS publisher existing.
	if natsClient != nil {
		sarIngA := compliance.NewSARSignalIngest(natsClient, sarSvc, log)
		go func() {
			if err := sarIngA.Run(sweepCtx); err != nil && sweepCtx.Err() == nil {
				log.Warn("sar signal ingest (surveillance) stopped", "err", err)
			}
		}()
		sarIngB := sarIngA.WithBinding(compliance.SARSignalStreamC,
			compliance.SARSignalSubjectC, compliance.SARSignalDurableC)
		go func() {
			if err := sarIngB.Run(sweepCtx); err != nil && sweepCtx.Err() == nil {
				log.Warn("sar signal ingest (compliance) stopped", "err", err)
			}
		}()
	}
	go func() {
		t := time.NewTicker(time.Hour)
		defer t.Stop()
		for {
			select {
			case <-sweepCtx.Done():
				return
			case <-t.C:
				day := time.Now().UTC()
				if n, err := amlSvc.ScanBusinessDay(sweepCtx, day); err != nil {
					log.Warn("aml ctr/structuring scan failed", "err", err)
				} else if n > 0 {
					log.Info("aml ctr triggers evaluated", "accounts", n)
				}
				if n, err := sarSvc.DraftFromOpenSignals(sweepCtx, 200); err != nil {
					log.Warn("sar open-signal poll failed", "err", err)
				} else if n > 0 {
					log.Info("sar drafts opened from signals", "count", n)
				}
				if n, err := sarSvc.Overdue(sweepCtx, 100); err == nil && len(n) > 0 {
					log.Warn("SAR reports past 30-day FinCEN deadline", "count", len(n))
				}
			}
		}
	}()

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

	// ---- Phase-09 Task 9.3.29 item 4 — secrets inventory (spec
	//      §19.14, §24 #340; migration 089) ----
	// The metadata register behind docs/ops/secrets-inventory.md: Super
	// Admin gated (doc §5 "Security + SRE leads"), every mutation
	// audited through admin_audit_log inside the row transaction. The
	// evaluator runs hourly — the 14-day P2 lead makes that plenty —
	// paging SECRET_ROTATION_OVERDUE once per overdue row until a
	// mark-rotated clears it.
	secretsInvSvc := security.NewInventoryService(security.NewPgInventoryStore(pool),
		security.RoleResolver(adminRoleResolver),
		secretsAlerter{nc: natsClient}, nil)
	go func() {
		tick := time.NewTicker(time.Hour)
		defer tick.Stop()
		for {
			select {
			case <-sweepCtx.Done():
				return
			case <-tick.C:
				if _, err := secretsInvSvc.EvaluateRotation(sweepCtx); err != nil {
					log.Warn("secrets inventory eval", "err", err)
				}
			}
		}
	}()
	// --- end secrets-inventory wiring ---

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

	// ---- Phase-23 stats surfaces (Tasks 23.3.6/.10/.11) ----
	// The OI and sentiment producers run in-gateway against the shared
	// position store so the REST reads serve the same delayed rings the
	// WS feed publishes (emit is a no-op here — fan-out lives in
	// cmd/marketdata). Below-cohort and delayed horizons are enforced by
	// the producers/handlers, never by trusting the caller.
	symByID := map[int64]string{}
	idBySym := map[string]int64{}
	var perfSyms []string
	if insts, ierr := marketStore.ListInstruments(context.Background()); ierr == nil {
		for _, in := range insts {
			symByID[in.ID] = in.Symbol
			idBySym[in.Symbol] = in.ID
			perfSyms = append(perfSyms, in.Symbol)
		}
	} else {
		log.Warn("instrument list unavailable — Phase-23 stats surfaces degrade",
			"err", ierr)
	}
	noopEmit := func(string, uint64, any) {}
	oiProd := marketdata.NewOIProducer(marketdata.OIProducerConfig{
		Logger: log, Symbols: perfSyms},
		marketdata.NewPgxOpenInterestSource(pool, symByID, nil), noopEmit)
	go func() {
		if err := oiProd.Run(sweepCtx); err != nil &&
			!errors.Is(err, context.Canceled) {
			log.Error("oi producer exited", "err", err)
		}
	}()
	var sentFlow marketdata.TakerFlowSource
	if chConn != nil {
		sentFlow = marketdata.NewTakerFlowStore(chConn)
	}
	sentProd := marketdata.NewSentimentProducer(marketdata.SentimentProducerConfig{
		Logger: log, Symbols: perfSyms},
		marketdata.NewPgxPositionCohortSource(pool, idBySym, nil),
		sentFlow, noopEmit)
	go func() {
		if err := sentProd.Run(sweepCtx); err != nil &&
			!errors.Is(err, context.Canceled) {
			log.Error("sentiment producer exited", "err", err)
		}
	}()
	var flowSrc api.TakerFlowAnalytics
	if chConn != nil {
		flowSrc = marketdata.NewTakerFlowStore(chConn)
	}
	// Task 23.3.11 — aggregate-only venue stats; rts27_daily_stats
	// rows feed spread/latency; uptime rides the ops ledger; RTS-27
	// quarterly figures come from the published-artifact register.
	// Reference stays nil — TCA rows carry slippage, not the published
	// fill-rate/latency metrics, so no independent second source exists
	// yet (honest seam; the divergence-hold path is covered by tests).
	var venueFills marketdata.VenueFillRateSource
	if vs, ok := anDeps.Stats.(*analytics.VolumeStatsStore); ok && vs != nil {
		venueFills = api.NewVenueFillRateSource(vs)
	}
	perfSvc := marketdata.NewVenuePerformanceService(
		marketdata.VenuePerformanceConfig{Logger: log, Symbols: perfSyms},
		marketdata.VenuePerformanceDeps{
			Daily:  marketdata.NewPgVenueDayStatsSource(pool),
			Fills:  venueFills,
			Uptime: statusAgg,
			RTS27:  api.RTS27FiguresSource{Svc: rts27Svc},
			Alerts: observability.LogSink{Log: log},
		})
	statsDeps := &api.MarketStatsDeps{
		Instruments: marketStore,
		OI:          oiProd,
		Sentiment:   sentProd,
		Flow:        flowSrc,
		Performance: perfSvc,
		Cache:       rdb.Client,
		ResolveTier: tierResolver,
		Guard:       marketdata.HistoryQueryGuard{},
	}

	// Drain latch (9.3.23): flips on signal — readiness reports
	// unhealthy immediately while in-flight work drains.
	drainFlag := &middleware.DrainFlag{}
	// ---- end Phase-09 ops wiring ----

	// Phase-18 Task 18.3.9: one checker instance satisfies both the
	// account- and api-key-existence seams of the fix-session handler.
	fixPgCheckers := fix.NewPgCheckers(pool)

	// ---- Phase-19 multi-asset margin API surface (Tasks 19.3.1/.8/.14–.17/.23) ----
	// The §23 row for MARGIN_MODE_SWITCH_BLOCKED lands with the Phase-19
	// spec-sync; until then the gateway registers it locally so the live
	// margin-mode handler emits 409, not the unregistered-code 500 shim.
	if _, ok := errs.Default.Lookup(risk.CodeMarginModeBlocked); !ok {
		if err := errs.Default.Register(errs.CodeDef{
			Code:        risk.CodeMarginModeBlocked,
			HTTPStatus:  http.StatusConflict,
			Description: "Margin-mode switch rejected: open positions exist (Phase-19 Task 19.3.1)",
			Owner:       "Phase-19 Task 19.3.1",
		}); err != nil {
			log.Warn("margin-mode blocked code registration failed", "err", err)
		}
	}
	// Services are bound nil-safely: a construction failure logs and the
	// handler serves SERVICE_DEGRADED rather than aborting gateway boot
	// (partial Phase-19 migrations must not take the whole API down).
	var collateralSvc *risk.CollateralService
	if cst, cerr := risk.NewPgCollateralScheduleStore(pool); cerr != nil {
		log.Warn("phase19 collateral store unavailable", "err", cerr)
	} else if c, cerr := risk.NewCollateralService(risk.CollateralDeps{
		Store:   cst,
		Alerter: opsAlerter,
		Logf:    func(f string, a ...any) { log.Warn(fmt.Sprintf(f, a...)) },
	}); cerr != nil {
		log.Warn("phase19 collateral service unavailable", "err", cerr)
	} else {
		collateralSvc = c
	}
	var marginSvc *risk.MarginService
	if mst, merr := risk.NewPgMarginStore(pool); merr != nil {
		log.Warn("phase19 margin store unavailable", "err", merr)
	} else {
		mo := risk.MarginOptions{
			Store: mst,
			Marks: risk.NewRedisMarkCache(rdb.Client),
			// §13.12 add-on denominators — OI from the positions
			// aggregate (PgMarginStore.OpenInterest), ADV from the
			// instrument:adv:{id} Redis mirror the analytics rollup
			// owns (absent ⇒ the pessimistic cap leg applies).
			OI:  mst,
			ADV: advSrc,
		}
		if collateralSvc != nil {
			// Task 19.3.8 §13.6b — the service requires the haircut/
			// concentration valuator in production; bind only when the
			// construction above succeeded (typed-nil would panic).
			mo.Collateral = collateralSvc
		}
		// Task 19.3.25 back-fit (post-Phase-22) — option-delta equity
		// linkage: the PG source prices every OPEN option_position's
		// delta at the current market (GK for EUROPEAN, trinomial
		// lattice bump-and-reprice for AMERICAN, intrinsic for the
		// expired-row lifecycle gap). Marks ride the SAME chained
		// provider as margin evaluation (oracle primary, last-trade
		// stub fallback); curves come from the oracle's published
		// curve:{ccy} pillars. The IV-surface vol seam is deliberately
		// unwired — no vol publisher exists yet (same posture as the
		// Phase-23 greeks feed): an account holding a live option leg
		// fails the delta leg closed with VOLATILITY_SURFACE_UNAVAILABLE
		// rather than fabricating exposure; accounts with no option
		// book short-circuit to zero before any market read.
		if osrc, oerr := risk.NewPgOptionDeltaSource(pool,
			risk.RedisOptionMarketSource{
				Marks:  markProv,
				Curves: rates.NewStore(rdb.Client),
			},
			risk.MarginUSDRateSource{
				Pairs: mst,
				Marks: risk.NewRedisMarkCache(rdb.Client),
			}); oerr != nil {
			log.Warn("phase19 option delta source unavailable", "err", oerr)
		} else {
			mo.OptionMargin = risk.DeltaOptionMarginEvaluator{Source: osrc}
		}
		// Task 19.3.25 §15.7 — recognized spread relief: the Phase-22
		// detection service's APPLIED rows feed MarginService's
		// PORTFOLIO aggregation (single-grant SpreadOffsetBook per
		// evaluation). A nil nakedMargin is honest — SpreadOffsets/
		// LiveOffsets read persisted APPLIED rows and never reprice
		// legs; Recompute repricing is invoked by the Phase-22 engine.
		if ss, serr := excmargin.NewSpreadOffsetService(pool, nil,
			func(f string, a ...any) { log.Warn(fmt.Sprintf(f, a...)) }); serr != nil {
			log.Warn("phase22 spread offset service unavailable", "err", serr)
		} else {
			mo.SpreadOffsets = ss
		}
		if m, merr := risk.NewMarginService(mo); merr != nil {
			log.Warn("phase19 margin service unavailable", "err", merr)
		} else {
			marginSvc = m
		}
	}
	levStore := risk.NewPgLeverageStore(pool)
	levSvc := risk.NewLeverageService(levStore, risk.NewRedisLeverageCache(rdb))
	// Task 19.3.24 / spec §13.14 — the venue document publishes the
	// effective per-entity leverage ceilings.
	venueDeps.LeveragePolicies = levStore
	posModeSvc := risk.NewPositionModeService(risk.NewPgPositionModeStore(pool))
	var fundSvc *risk.InsuranceFundService
	if f, ferr := risk.NewInsuranceFundService(risk.InsuranceFundDeps{
		Pool:    pool,
		Redis:   rdb,
		Poster:  ledgerSvc,
		Pub:     ledgerPub,
		Alerter: opsAlerter,
		Logf:    func(f string, a ...any) { log.Warn(fmt.Sprintf(f, a...)) },
	}); ferr != nil {
		log.Warn("phase19 insurance fund service unavailable", "err", ferr)
	} else {
		fundSvc = f
	}
	// Task 19.3.9 — retail NBP service. Fund + GL poster are mandatory
	// (restitution must debit the fund or fail); a nil fund degrades
	// the whole seam to absent rather than half-wired.
	var nbpSvc *risk.NBPService
	if fundSvc != nil {
		if nStore, nerr := risk.NewPgNBPStore(pool); nerr != nil {
			log.Warn("phase19 nbp store unavailable", "err", nerr)
		} else if ns, nerr2 := risk.NewNBPService(risk.NBPDeps{
			Store:   nStore,
			Redis:   rdb,
			Levels:  risk.RedisMarginLevelReader{C: rdb},
			Fund:    fundSvc,
			Poster:  ledgerSvc,
			Alerter: opsAlerter,
			Logf:    func(f string, a ...any) { log.Warn(fmt.Sprintf(f, a...)) },
		}); nerr2 != nil {
			log.Warn("phase19 nbp service unavailable", "err", nerr2)
		} else {
			nbpSvc = ns
			// §13.6c daily sweep at 17:00 ET — catches deficits the
			// post-liquidation hook missed (e.g. overnight gaps).
			go func(svc *risk.NBPService) {
				loc, lerr := time.LoadLocation("America/New_York")
				if lerr != nil {
					log.Warn("nbp sweep: ET tz unavailable — running 22:00 UTC fallback")
					loc = time.UTC
				}
				for {
					now := time.Now().In(loc)
					next := time.Date(now.Year(), now.Month(), now.Day(), 17, 0, 0, 0, loc)
					if !next.After(now) {
						next = next.Add(24 * time.Hour)
					}
					select {
					case <-sweepCtx.Done():
						return
					case <-time.After(time.Until(next)):
						if n, err := svc.SweepOnce(sweepCtx); err != nil && sweepCtx.Err() == nil {
							log.Warn("nbp daily sweep", "err", err)
						} else if n > 0 {
							log.Info("nbp daily sweep restituted accounts", "count", n)
						}
					}
				}
			}(nbpSvc)
		}
	}
	// Phase-19 Tasks 19.3.3/19.3.4/19.3.16 — margin-call + liquidation
	// lifecycle construction. Every dep is required; a failed leg is
	// logged loudly and the consumer hooks stay nil-guarded (§2.7: a
	// half-wired engine silently skipping stop-outs is worse than an
	// absent one).
	liqReader := risk.RedisMarginLevelReader{C: rdb}
	liqQueue, qerr := risk.NewLiquidationQueue(rdb, func(f string, a ...any) {
		log.Warn(fmt.Sprintf(f, a...))
	})
	if qerr != nil {
		log.Warn("phase19 liquidation queue unavailable", "err", qerr)
	}
	if ls, lerr := risk.NewPgLiquidationStore(pool); lerr != nil {
		log.Warn("phase19 liquidation store unavailable", "err", lerr)
	} else {
		liqStore = ls
	}
	var auctionEng *risk.AuctionEngine
	var marginCallSvc *risk.MarginCallService
	if liqStore != nil {
		if ae, aerr := risk.NewAuctionEngine(risk.AuctionDeps{
			Redis:    rdb,
			Store:    liqStore,
			Dispatch: orders.NewDispatcher(orderSvc),
			Marks:    markProv,
			Alerter:  opsAlerter,
			Logf:     func(f string, a ...any) { log.Warn(fmt.Sprintf(f, a...)) },
		}); aerr != nil {
			log.Warn("phase19 auction engine unavailable", "err", aerr)
		} else {
			auctionEng = ae
		}
	}
	if liqQueue != nil {
		if mcStore, merr := risk.NewPgMarginCallStore(pool); merr != nil {
			log.Warn("phase19 margin-call store unavailable", "err", merr)
		} else if mc, cerr := risk.NewMarginCallService(risk.MarginCallDeps{
			Pool:    pool,
			Redis:   rdb,
			Store:   mcStore,
			Levels:  liqReader,
			Queue:   liqQueue,
			Notify:  notifSvc,
			Alerter: opsAlerter,
			Logf:    func(f string, a ...any) { log.Warn(fmt.Sprintf(f, a...)) },
		}); cerr != nil {
			log.Warn("phase19 margin-call service unavailable", "err", cerr)
		} else {
			marginCallSvc = mc
			// §13.6d order-entry block: position-increasing orders
			// reject MARGIN_CALL_EXCEEDED while the account's block
			// flag stands; reduce_only bypasses it inside
			// checkAdmission.
			orderSvc.WithMarginCall(mc)
		}
	}
	// Task 19.3.19 — ADL indicator publisher (adl:indicator:* hashes +
	// adl:priority:* ZSETs refreshed on the 2s scanner cadence) and the
	// depletion-fallback engine bound into the liquidation service.
	var adlEng *risk.ADLEngine
	var adlPub *risk.ADLIndicatorPublisher
	if fundSvc != nil {
		if as, aerr := risk.NewPgADLStore(pool); aerr != nil {
			log.Warn("phase19 adl store unavailable", "err", aerr)
		} else {
			adlStore = as
			scorer, serr := risk.NewADLScorer(
				risk.NewRedisMarkCache(rdb.Client), levSvc)
			if serr != nil {
				log.Warn("phase19 adl scorer unavailable", "err", serr)
			} else {
				if ap, perr := risk.NewADLIndicatorPublisher(risk.ADLPublisherDeps{
					Redis:    rdb,
					Universe: as,
					Scorer:   scorer,
					Logf:     func(f string, a ...any) { log.Warn(fmt.Sprintf(f, a...)) },
				}); perr != nil {
					log.Warn("phase19 adl publisher unavailable", "err", perr)
				} else {
					adlPub = ap
				}
				if ae, eerr := risk.NewADLEngine(risk.ADLEngineDeps{
					Store:    as,
					Scorer:   scorer,
					Fund:     fundSvc,
					Dispatch: orders.NewDispatcher(orderSvc),
					Alerter:  opsAlerter,
					Logf:     func(f string, a ...any) { log.Warn(fmt.Sprintf(f, a...)) },
				}); eerr != nil {
					log.Warn("phase19 adl engine unavailable", "err", eerr)
				} else {
					adlEng = ae
				}
			}
		}
	}
	if liqStore != nil && liqQueue != nil {
		if ls, lerr := risk.NewLiquidationService(risk.LiquidationDeps{
			Pool:       pool,
			Redis:      rdb,
			Store:      liqStore,
			Levels:     liqReader,
			Queue:      liqQueue,
			Dispatch:   orders.NewDispatcher(orderSvc),
			Fund:       fundSvc,
			Auction:    auctionEng,
			MarginCall: marginCallSvc,
			NBP:        nbpSvc,
			ADL:        adlEng,
			// §13.4a/6a ADV slicing yardstick — same instrument:adv:{id}
			// mirror the margin add-on reads.
			ADV: advSrc,
			// Task 19.5.3.6 stale-price ladder: reads the oracle's
			// oracle:fallback:{sym} + :freeze keys — tiered haircuts,
			// auction-only ≥15s, FORCE_CASH >60s, flash-crash freeze.
			StaleFallback: risk.NewRedisStaleFallbackSource(rdb.Client),
			Alerter:       opsAlerter,
			Logf:          func(f string, a ...any) { log.Warn(fmt.Sprintf(f, a...)) },
		}); lerr != nil {
			log.Warn("phase19 liquidation service unavailable", "err", lerr)
		} else {
			liqSvc = ls
			// Worker: BLPOP-paced job consumption under the per-account
			// lock (anti-stranding + delayed-retry promotion handled in
			// the engine).
			go func() {
				for {
					if err := liqSvc.ConsumeOnce(sweepCtx); err != nil && sweepCtx.Err() == nil {
						log.Warn("liquidation worker", "err", err)
						select {
						case <-sweepCtx.Done():
							return
						case <-time.After(200 * time.Millisecond):
						}
					}
					if sweepCtx.Err() != nil {
						return
					}
				}
			}()
			// §13.4a scanner: 2s cadence — delayed retry promotion,
			// margin-call expiry sweep, auction phase advancement,
			// isolated-leg breaches, account stop-out scan.
			go func() {
				t := time.NewTicker(2 * time.Second)
				defer t.Stop()
				for {
					select {
					case <-sweepCtx.Done():
						return
					case <-t.C:
						if _, err := liqSvc.ScanOnce(sweepCtx); err != nil && sweepCtx.Err() == nil {
							log.Warn("liquidation scan", "err", err)
						}
						// §13.5 ADL priority refresh rides the same
						// 2s cadence (AdlScanCadence).
						if adlPub != nil {
							if _, err := adlPub.TickOnce(sweepCtx); err != nil && sweepCtx.Err() == nil {
								log.Warn("adl indicator tick", "err", err)
							}
						}
					}
				}
			}()
		}
	}
	// Phase-19.5 Task 19.5.3.7 step 3 — downstream cascade broadcast:
	// the oracle publishes oracle:health:{symbol} every tick; this
	// surface exposes it on /metrics (2=OK, 1=DEGRADED, 0=UNAVAILABLE)
	// and pages L1 on an UNAVAILABLE transition — the conditional-
	// trigger suspension path already reads the same keys (Phase-16
	// CONDITIONAL_TRIGGER_ORACLE_STALE).
	metReg.VecFunc("exchange_oracle_health",
		"Phase-19.5 oracle health per symbol (2=OK,1=DEGRADED,0=UNAVAILABLE)",
		"gauge", func() []observability.PullSample {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			var out []observability.PullSample
			iter := rdb.Client.Scan(ctx, 0, "oracle:health:*", 256).Iterator()
			for iter.Next(ctx) {
				key := iter.Val()
				sym := strings.TrimPrefix(key, "oracle:health:")
				v, err := rdb.Client.Get(ctx, key).Result()
				if err != nil {
					continue
				}
				score := 0.0
				switch oracle.HealthState(v) {
				case oracle.HealthOK:
					score = 2
				case oracle.HealthDegraded:
					score = 1
				}
				out = append(out, observability.PullSample{
					Labels: []string{"symbol", sym}, Value: score})
			}
			return out
		})
	go func() {
		// UNAVAILABLE-transition pager — reads the oracle-published
		// health keys on a 5s cadence and raises PRICE_ORACLE_UNAVAILABLE
		// on a fresh outage (edge-triggered, per symbol).
		last := map[string]oracle.HealthState{}
		t := time.NewTicker(5 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-sweepCtx.Done():
				return
			case <-t.C:
			}
			ctx, cancel := context.WithTimeout(sweepCtx, 2*time.Second)
			iter := rdb.Client.Scan(ctx, 0, "oracle:health:*", 256).Iterator()
			for iter.Next(ctx) {
				key, sym := iter.Val(), strings.TrimPrefix(iter.Val(), "oracle:health:")
				v, err := rdb.Client.Get(ctx, key).Result()
				if err != nil {
					continue
				}
				h := oracle.HealthState(v)
				if h == oracle.HealthUnavailable && last[sym] != oracle.HealthUnavailable &&
					opsAlerter != nil {
					_ = opsAlerter.Raise(ctx, settlement.OpsAlert{
						Severity: settlement.SeverityP1,
						Code:     "PRICE_ORACLE_UNAVAILABLE",
						Summary:  fmt.Sprintf("oracle UNAVAILABLE on %s — fewer than 2 fresh feeds; margin orders halting", sym),
						Details:  map[string]string{"symbol": sym},
					})
				}
				last[sym] = h
			}
			cancel()
		}
	}()
	// Phase-19 Tasks 19.3.26/19.3.27/19.3.28 — the event-driven margin
	// engine, isolated-margin service, volatility scaler and intraday
	// collateral monitor. The engine consumes mark:* deltas (published
	// by the fill hook), evaluates only affected accounts, publishes
	// margin:level hashes, and dispatches stop-outs to the durable
	// queue — the 2s scanner above stays as the belt-and-braces path.
	if liqQueue != nil {
		if liqDispatch, derr := risk.NewQueueLiquidationDispatcher(liqQueue); derr != nil {
			log.Warn("phase19 queue dispatcher unavailable", "err", derr)
		} else if mStore, merr := risk.NewPgMarginStore(pool); merr != nil {
			log.Warn("phase19 engine margin store unavailable", "err", merr)
		} else {
			var isoSvc *risk.IsolatedMarginService
			if isoStore, ierr := risk.NewPgIsolatedMarginStore(pool); ierr != nil {
				log.Warn("phase19 isolated margin store unavailable", "err", ierr)
			} else if sv, verr := risk.NewIsolatedMarginService(risk.IsolatedMarginDeps{
				Store: isoStore, Dispatcher: liqDispatch, Alerter: opsAlerter,
				Logf: func(f string, a ...any) { log.Warn(fmt.Sprintf(f, a...)) },
			}); verr != nil {
				log.Warn("phase19 isolated margin service unavailable", "err", verr)
			} else {
				isoSvc = sv
			}
			sink, skerr := risk.NewRedisMarginSnapshotSink(rdb, mStore)
			if skerr != nil {
				log.Warn("phase19 margin snapshot sink unavailable", "err", skerr)
			}
			// Volatility scaler fed by the in-memory tick ring — the
			// engine's MarkObserver fan-out records every consumed tick
			// into it (no ClickHouse tick source exists pre-Phase-19.5;
			// the ring accumulates from runtime marks, restart-safe).
			tickRing := risk.NewMarkTickRing()
			volScaler, verr := risk.NewVolatilityScaler(risk.VolatilityScalerDeps{
				Source: tickRing, Alerter: opsAlerter,
				Logf: func(f string, a ...any) { log.Warn(fmt.Sprintf(f, a...)) },
			})
			if verr != nil {
				log.Warn("phase19 volatility scaler unavailable", "err", verr)
				volScaler = nil
			}
			// Correlation matrix: Redis-persisted warm start; Refresh
			// needs a ReturnSeriesSource (ClickHouse daily closes —
			// Phase-19.5 binding) so the engine serves the persisted
			// matrix and skips refresh until then.
			corrMx := risk.NewCorrelationMatrix(risk.CorrelationMatrixDeps{
				Redis: rdb.Client,
				Audit: risk.NewRedisCorrelationAudit(rdb.Client),
				Logf:  func(f string, a ...any) { log.Warn(fmt.Sprintf(f, a...)) },
			})
			if err := corrMx.LoadPersisted(sweepCtx); err != nil && sweepCtx.Err() == nil {
				log.Warn("phase19 correlation matrix warm load", "err", err)
			}
			obs := []risk.MarkObserver{markProv, tickRing}
			var vol risk.IMMultiplierSource
			if volScaler != nil {
				vol = volScaler
			}
			if eng, eerr := risk.NewMarginEngine(risk.MarginEngineDeps{
				Store:       mStore,
				Marks:       markCache,
				Source:      risk.NewRedisMarkSource(rdb.Client, nil),
				Dispatcher:  liqDispatch,
				Isolated:    isoSvc,
				Sink:        sink,
				Correlation: corrMx,
				Collateral:  collateralSvc,
				Volatility:  vol,
				OI:          mStore,
				ADV:         advSrc,
				Thresholds: risk.NewMarginThresholdService(
					risk.NewPgMarginThresholdStore(pool)),
				Observer: risk.FanOutMarkObservers(obs...),
				Alerter:  opsAlerter,
				Logf:     func(f string, a ...any) { log.Warn(fmt.Sprintf(f, a...)) },
			}); eerr != nil {
				log.Warn("phase19 margin engine unavailable", "err", eerr)
			} else {
				go func() {
					if err := eng.Run(sweepCtx); err != nil && sweepCtx.Err() == nil {
						log.Warn("margin engine stopped", "err", err)
					}
				}()
				// Volatility refresh loop: rescale every minute over
				// the symbols the engine is tracking (positions' marks).
				if volScaler != nil {
					go func(vs *risk.VolatilityScaler, me *risk.MarginEngine) {
						t := time.NewTicker(time.Minute)
						defer t.Stop()
						for {
							select {
							case <-sweepCtx.Done():
								return
							case <-t.C:
								vs.Refresh(sweepCtx, me.TrackedSymbols())
							}
						}
					}(volScaler, eng)
				}
				// Task 19.3.28 — intraday collateral haircut monitor:
				// >100bps currency moves re-anchor the day and trigger
				// margin re-evaluation of accounts holding that ccy.
				if collateralSvc != nil && marginSvc != nil {
					if mon, merr := risk.NewCollateralMonitor(risk.CollateralMonitorDeps{
						Schedule: collateralSvc,
						Marks:    risk.NewRedisMarkSource(rdb.Client, nil),
						Pairs:    mStore.FxPairInstruments,
						Accounts: risk.CollateralAccountFunc(func(ctx context.Context, ccy string) ([]int64, error) {
							rows, err := pool.Query(ctx,
								`SELECT DISTINCT account_id FROM balances WHERE currency=$1`, ccy)
							if err != nil {
								return nil, err
							}
							defer rows.Close()
							var ids []int64
							for rows.Next() {
								var id int64
								if err := rows.Scan(&id); err != nil {
									return nil, err
								}
								ids = append(ids, id)
							}
							return ids, rows.Err()
						}),
						Margin:  marginSvc,
						Calls:   marginCallSvc,
						Alerter: opsAlerter,
						Logf:    func(f string, a ...any) { log.Warn(fmt.Sprintf(f, a...)) },
					}); merr != nil {
						log.Warn("phase19 collateral monitor unavailable", "err", merr)
					} else {
						go func() {
							if err := mon.Run(sweepCtx); err != nil && sweepCtx.Err() == nil {
								log.Warn("collateral monitor stopped", "err", err)
							}
						}()
					}
				}
			}
		}
	}
	// Task 19.3.10 — bilateral credit service: PG authority over
	// credit_groups/relationships/reservations (migration 053), shm
	// credit-matrix publication for the engine's match-time screen, and
	// the divergence probe. The engine-side reserve protocol owns
	// pre-commit decisions; the Go side reconciles fills/cancels
	// (ConsumeFill/ReleaseOrder hooks below), sweeps TTL-dead
	// reservations and verifies matrix↔PG parity.
	if bcStore, berr := risk.NewPgBilateralCreditStore(pool); berr != nil {
		log.Warn("phase19 bilateral credit store unavailable", "err", berr)
	} else {
		var cw risk.CreditCellWriter
		var cr risk.CreditCellReader
		cmName := os.Getenv("EXC_CREDIT_MATRIX_SHM")
		if cmName == "" {
			cmName = "credit_matrix"
		}
		if cm, cerr := ipc.OpenCreditMatrix(cmName, true); cerr != nil {
			log.Warn("phase19 credit matrix shm unavailable — publishing disabled", "err", cerr)
		} else {
			cw, cr = cm, cm
		}
		if bs, berr2 := risk.NewBilateralCreditService(risk.BilateralCreditOptions{
			Store:   bcStore,
			Conv:    position.NewConverter(risk.LastTradeRates{Instruments: pnlStore, Marks: orderStore}, ""),
			Redis:   rdb,
			Alerter: opsAlerter,
			Writer:  cw,
			Reader:  cr,
			Logf:    func(f string, a ...any) { log.Warn(fmt.Sprintf(f, a...)) },
		}); berr2 != nil {
			log.Warn("phase19 bilateral credit service unavailable", "err", berr2)
		} else {
			bilatSvc = bs
			if cw != nil {
				if n, perr := bs.PublishSnapshot(sweepCtx); perr != nil {
					log.Warn("bilateral credit snapshot publish", "err", perr)
				} else if n > 0 {
					log.Info("bilateral credit matrix published", "cells", n)
				}
			}
			// Sweeper (reservation TTL / orphan release) + divergence
			// verify — the shm matrix must never silently diverge from
			// the PG authority (spec §13.8).
			go func(svc *risk.BilateralCreditService) {
				sweepT := time.NewTicker(5 * time.Second)
				verifyT := time.NewTicker(30 * time.Second)
				defer sweepT.Stop()
				defer verifyT.Stop()
				for {
					select {
					case <-sweepCtx.Done():
						return
					case <-sweepT.C:
						if n, err := svc.Sweep(sweepCtx); err != nil && sweepCtx.Err() == nil {
							log.Warn("bilateral credit sweep", "err", err)
						} else if n > 0 {
							log.Info("bilateral credit reservations swept", "count", n)
						}
					case <-verifyT.C:
						if cw == nil {
							continue
						}
						if n, err := svc.VerifyMatrix(sweepCtx); err != nil && sweepCtx.Err() == nil {
							log.Warn("bilateral credit verify", "err", err)
						} else if n > 0 {
							log.Warn("bilateral credit matrix divergence corrected", "cells", n)
						}
					}
				}
			}(bilatSvc)
		}
	}
	// Task 19.3.16 item 4 — margin:level hash diffs → private:margin WS
	// push at 500ms cadence (the engine publishes the hash; this loop is
	// a read-only change feed — never a second writer).
	risk.NewMarginLevelWatcher(liqReader, redisPatternLister{c: rdb},
		func(_ context.Context, acct int64, channel, _ string, payload any) error {
			if wsSrv != nil {
				wsSrv.PublishPrivate(acct, channel, payload)
			}
			return nil
		}).Start(sweepCtx, 500*time.Millisecond,
		func(e error) { log.Warn("margin level watcher", "err", e) })
	// Task 19.3.13 — margin-model validation drivers. The weekly stress
	// suite and the daily predicted-vs-realized backtest both persist
	// margin_model_runs rows (migration 064); FlashCrashProvider is the
	// Phase-20 tick-replay seam and stays nil here (the engine treats a
	// nil provider as "skip flash-crash scenarios").
	if runStore, rerr := risk.NewPgModelRunStore(pool); rerr != nil {
		log.Warn("phase19 model-run store unavailable", "err", rerr)
	} else {
		if pSrc, perr := risk.NewPgStressPortfolioSource(pool); perr != nil {
			log.Warn("phase19 stress source unavailable", "err", perr)
		} else if eng, eerr := risk.NewStressEngine(pSrc, runStore,
			risk.NewPgFundBalanceSource(pool), nil, opsAlerter,
			risk.StressEngineConfig{}, nil,
			func(f string, a ...any) { log.Warn(fmt.Sprintf(f, a...)) }); eerr != nil {
			log.Warn("phase19 stress engine unavailable", "err", eerr)
		} else {
			sched := &risk.StressScheduler{Engine: eng}
			go func() {
				t := time.NewTicker(time.Hour)
				defer t.Stop()
				for {
					select {
					case <-sweepCtx.Done():
						return
					case <-t.C:
						if ran, err := sched.RunDue(sweepCtx); err != nil && sweepCtx.Err() == nil {
							log.Warn("stress suite scheduler", "err", err)
						} else if ran {
							log.Info("weekly margin stress suite ran")
						}
					}
				}
			}()
		}
		if bSrc, berr := risk.NewPgBacktestSource(pool); berr != nil {
			log.Warn("phase19 backtest source unavailable", "err", berr)
		} else if bt, bterr := risk.NewBacktester(bSrc, runStore, opsAlerter, nil,
			func(f string, a ...any) { log.Warn(fmt.Sprintf(f, a...)) }); bterr != nil {
			log.Warn("phase19 backtester unavailable", "err", bterr)
		} else {
			go func() {
				// Hourly poll — RunDue is catch-up-aware and no-ops
				// once yesterday's register row exists.
				t := time.NewTicker(time.Hour)
				defer t.Stop()
				for {
					select {
					case <-sweepCtx.Done():
						return
					case <-t.C:
						if _, err := bt.RunDue(sweepCtx); err != nil && sweepCtx.Err() == nil {
							log.Warn("margin backtest scheduler", "err", err)
						}
					}
				}
			}()
		}
	}
	// Positions-view decoration: margin:level hash + margin/position
	// modes + ADL indicator + per-position effective leverage. Every
	// reader is nil-tolerant — a missing seam omits its fields.
	positionsDec := &api.PositionsDecoration{
		ADL:         api.RedisADLIndicatorReader{C: rdb},
		MarginLevel: risk.RedisMarginLevelReader{C: rdb},
		Leverage:    levSvc,
	}
	if marginSvc != nil {
		positionsDec.MarginMode = marginSvc
	}
	if posModeSvc != nil {
		positionsDec.PositionMode = posModeSvc
	}
	// True-nil interface bindings for the possibly-failed constructions —
	// assigning a typed-nil pointer into an interface would defeat the
	// handlers' nil checks.
	var marginModeH api.MarginModeSetter
	if marginSvc != nil {
		marginModeH = marginSvc
	}
	var fundH api.InsuranceFundReader
	if fundSvc != nil {
		fundH = fundSvc
	}
	var collateralH api.CollateralScheduleUpdater
	if collateralSvc != nil {
		collateralH = collateralSvc
	}

	// Task 5.3.11 — sub-accounts family surface (list/aggregate/create +
	// per-sub-account API keys + admin limit). The handler package owns
	// the handlers; the wiring layer adapts auth.Claims into its
	// Identity seam so the package stays auth-import-free.
	subAccountSvc := accounts.NewSubAccountService(pool)
	accountCluster := &accounts.Handler{
		Subs:   subAccountSvc,
		Keys:   accounts.NewAPIKeyService(pool, subAccountSvc, secretBox),
		Freeze: freezeSvc,
		ResolveIdentity: func(r *http.Request) *accounts.Identity {
			c := auth.ClaimsFrom(r.Context())
			if c == nil {
				return nil
			}
			uid, _ := strconv.ParseInt(c.Subject, 10, 64)
			return &accounts.Identity{
				AccountID:     c.AccountID,
				UserID:        uid,
				Scopes:        c.Scopes,
				TwoFactorDone: c.TwoFactorVerified(),
			}
		},
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
		"GET /api/v1/account/balances": http.HandlerFunc(api.AccountBalances(fundStore)),
		// Task 5.3.11 sub-accounts family surface.
		"GET /api/v1/account/sub-accounts": http.HandlerFunc(
			accountCluster.ListSubAccounts),
		"GET /api/v1/account/sub-accounts/aggregate": http.HandlerFunc(
			accountCluster.AggregateSubAccounts),
		"POST /api/v1/account/sub-accounts": http.HandlerFunc(
			accountCluster.CreateSubAccount),
		"POST /api/v1/account/sub-accounts/{id}/api-keys": http.HandlerFunc(
			accountCluster.CreateSubAccountAPIKey),
		"DELETE /api/v1/account/sub-accounts/{id}/api-keys/{keyId}": http.HandlerFunc(
			accountCluster.RevokeSubAccountAPIKey),
		"PUT /api/v1/admin/accounts/{id}/sub-account-limit": http.HandlerFunc(
			accountCluster.SetSubAccountLimit),
		"GET /api/v1/positions":           http.HandlerFunc(api.AccountPositions(fundStore)),
		"GET /api/v1/account/risk-limits": http.HandlerFunc(api.AccountRiskLimits(riskLimits, fundStore)),
		// --- Phase-19 multi-asset margin surface (Tasks 19.3.1/.15–.17/.23) ---
		"GET /api/v1/account/positions": http.HandlerFunc(
			api.AccountPositionsEnriched(fundStore, positionsDec)),
		"POST /api/v1/account/margin-mode": http.HandlerFunc(
			api.AccountMarginMode(marginModeH)),
		"POST /api/v1/account/leverage": http.HandlerFunc(
			api.AccountLeverageSet(levSvc, levStore)),
		"GET /api/v1/account/liquidations": http.HandlerFunc(
			api.AccountLiquidations(api.NewPgLiquidationLister(pool))),
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
		// --- Phase-24 settlement ops (Tasks 24.3.6 / 24.3.7) ---
		// Resolution submits to the §8.2 four-eyes queue; the mutation
		// lands inside the approval tx via the registered executors.
		"GET /api/v1/admin/pb-reconciliation": http.HandlerFunc(
			api.AdminPBReconciliation(pbReconSvc)),
		"POST /api/v1/admin/settlement-exceptions/{id}/resolve": http.HandlerFunc(
			api.AdminSettlementExceptionResolve(settlementExcSvc)),
		"GET /api/v1/admin/settlement-exceptions/{id}": http.HandlerFunc(
			api.AdminSettlementExceptionGet(settlementExcSvc)),
		// --- Phase-24 nostro / recon / SWIFT / confirmations /
		//     compliance exports (Tasks 24.3.1–.5) ---
		"GET /api/v1/admin/nostro-accounts": http.HandlerFunc(
			api.AdminNostroAccountList(nostroSvc)),
		"POST /api/v1/admin/nostro-accounts": http.HandlerFunc(
			api.AdminNostroAccountCreate(nostroSvc)),
		"GET /api/v1/admin/nostro-reconciliation": http.HandlerFunc(
			api.AdminNostroReconciliation(nostroReconSvc)),
		"POST /api/v1/admin/nostro-reconciliation/run": http.HandlerFunc(
			api.AdminNostroReconRun(nostroReconSvc)),
		"POST /api/v1/admin/nostro-reconciliation/breaks/{id}/resolve": http.HandlerFunc(
			api.AdminNostroBreakResolve(nostroReconSvc)),
		"GET /api/v1/admin/swift-messages": http.HandlerFunc(
			api.AdminSwiftMessages(swiftTracker)),
		"GET /api/v1/admin/compliance-report": http.HandlerFunc(
			api.AdminComplianceReport(compReportSvc)),
		// --- Phase-24 settlement ops: statements / CLS / SSI / netting /
		//     rail / suspense (Tasks 24.3.8/.9/.12/.20/.21) ---
		"POST /api/v1/admin/settlement/statements": http.HandlerFunc(
			api.AdminIngestStatement(stmtIngestSvc)),
		"GET /api/v1/admin/settlement/statements": http.HandlerFunc(
			api.AdminListStatements(pgStmtStore)),
		"GET /api/v1/admin/settlement/statements/{id}/entries": http.HandlerFunc(
			api.AdminStatementEntries(pgStmtStore)),
		"POST /api/v1/admin/settlement/cls/instructions": http.HandlerFunc(
			api.AdminClsSubmit(clsSvc)),
		"POST /api/v1/admin/settlement/cls/instructions/{ref}/dispatch": http.HandlerFunc(
			api.AdminClsDispatch(clsSvc)),
		"POST /api/v1/admin/settlement/cls/instructions/{ref}/amend": http.HandlerFunc(
			api.AdminClsAmend(clsSvc)),
		"POST /api/v1/admin/settlement/cls/instructions/{ref}/rescind": http.HandlerFunc(
			api.AdminClsRescind(clsSvc)),
		"POST /api/v1/admin/settlement/cls/instructions/{ref}/pay-in": http.HandlerFunc(
			api.AdminClsPayIn(clsSvc)),
		"POST /api/v1/admin/settlement/cls/instructions/{ref}/finality": http.HandlerFunc(
			api.AdminClsFinality(clsSvc)),
		"POST /api/v1/admin/settlement/cls/instructions/{ref}/status": http.HandlerFunc(
			api.AdminClsStatus(clsSvc)),
		"GET /api/v1/admin/settlement/ssi": http.HandlerFunc(
			api.AdminSsiList(ssiSvc)),
		"POST /api/v1/admin/settlement/ssi": http.HandlerFunc(
			api.AdminSsiRegister(ssiSvc)),
		"POST /api/v1/admin/settlement/ssi/{id}/revoke": http.HandlerFunc(
			api.AdminSsiRevoke(ssiSvc)),
		"POST /api/v1/admin/settlement/netting/run": http.HandlerFunc(
			api.AdminNettingRun(nettingSvc)),
		"GET /api/v1/admin/settlement/netting/batches": http.HandlerFunc(
			api.AdminNettingBatches(nettingSvc)),
		"GET /api/v1/admin/settlement/netting/batches/{id}/lines": http.HandlerFunc(
			api.AdminNettingBatchLines(nettingSvc)),
		"POST /api/v1/admin/settlement/netting/batches/{id}/dispatch": http.HandlerFunc(
			api.AdminNettingDispatch(nettingSvc)),
		"POST /api/v1/admin/settlement/netting/batches/{id}/settle": http.HandlerFunc(
			api.AdminNettingSettle(nettingSvc)),
		"POST /api/v1/admin/settlement/netting/batches/{id}/bust": http.HandlerFunc(
			api.AdminNettingBust(nettingSvc)),
		"POST /api/v1/admin/settlement/suspense/route": http.HandlerFunc(
			api.AdminSuspenseRoute(suspenseSvc)),
		"POST /api/v1/admin/settlement/suspense/{id}/resolve": http.HandlerFunc(
			api.AdminSuspenseResolve(suspenseSvc)),
		// --- Phase-24 allocations (Tasks 24.3.10/.15) ---
		"POST /api/v1/allocations": http.HandlerFunc(
			api.AllocationsCreate(allocDeps)),
		"POST /api/v1/admin/allocations/groups": http.HandlerFunc(
			api.AdminAllocationGroupCreate(allocDeps)),
		"GET /api/v1/admin/allocations/groups/{id}": http.HandlerFunc(
			api.AdminAllocationGroupGet(allocDeps)),
		"POST /api/v1/admin/allocations/groups/{id}/fills": http.HandlerFunc(
			api.AdminAllocationAttachFills(allocDeps)),
		"POST /api/v1/admin/allocations/groups/{id}/allocate": http.HandlerFunc(
			api.AdminAllocationAllocate(allocDeps)),
		"POST /api/v1/admin/allocations/groups/{id}/eligibility": http.HandlerFunc(
			api.AdminAllocationEligibility(allocDeps)),
		"POST /api/v1/admin/allocations/groups/{id}/submit": http.HandlerFunc(
			api.AdminAllocationSubmit(allocDeps)),
		"POST /api/v1/admin/allocations/{id}/claim": http.HandlerFunc(
			api.AdminAllocationClaim(allocDeps)),
		"POST /api/v1/admin/allocations/{id}/reject": http.HandlerFunc(
			api.AdminAllocationReject(allocDeps)),
		"POST /api/v1/admin/allocations/{id}/cancel": http.HandlerFunc(
			api.AdminAllocationCancel(allocDeps)),
		"POST /api/v1/admin/allocations/{id}/correct": http.HandlerFunc(
			api.AdminAllocationCorrect(allocDeps)),
		"POST /api/v1/admin/allocations/escalate": http.HandlerFunc(
			api.AdminAllocationEscalate(allocDeps)),
		// --- Phase-24 client money / treasury / assurance
		//     (Tasks 24.3.11/.17/.18) ---
		"GET /api/v1/admin/treasury/own-funds": http.HandlerFunc(
			api.AdminTreasuryOwnFunds(treasurySvc)),
		"GET /api/v1/admin/treasury/contingent-capital": http.HandlerFunc(
			api.AdminContingentCapitalList(treasurySvc)),
		"POST /api/v1/admin/treasury/contingent-capital": http.HandlerFunc(
			api.AdminContingentCapitalCreate(treasurySvc)),
		"GET /api/v1/admin/client-money/audits": http.HandlerFunc(
			api.AdminClientMoneyAudits(assuranceSvc)),
		"POST /api/v1/admin/client-money/audits": http.HandlerFunc(
			api.AdminClientMoneyAuditCreate(assuranceSvc)),
		"POST /api/v1/admin/client-money/audits/{id}/evidence-pack": http.HandlerFunc(
			api.AdminClientMoneyEvidencePack(assuranceSvc)),
		"GET /api/v1/admin/client-money/certifications": http.HandlerFunc(
			api.AdminClientMoneyCertificationList(assuranceSvc)),
		"POST /api/v1/admin/client-money/certifications": http.HandlerFunc(
			api.AdminClientMoneyCertificationCreate(assuranceSvc)),
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
		// Task 5.3.19 tax reporting (phase + canonical client path) —
		// the 20.3.10 daily-cap limiter rides the variadic dep.
		"GET /api/v1/tax/report":         http.HandlerFunc(api.TaxReport(taxSvc, taxLimiter)),
		"GET /api/v1/account/tax-report": http.HandlerFunc(api.TaxReport(taxSvc, taxLimiter)),
		// Phase-20 Task 20.3.14 — MiFID II ex-ante cost preview +
		// ex-post annual reconciliation (?annual=YEAR).
		"GET /api/v1/account/cost-preview": http.HandlerFunc(api.AccountCostPreview(costsSvc)),
		// Phase-20 Task 20.3.16 — marketing-ops report over the
		// Phase-21-owned promotions/consent relations (503 while those
		// tables are absent).
		"GET /api/v1/admin/promotions/report": http.HandlerFunc(api.AdminPromotionsReport(marketingSvc)),
		// Phase-20 read surface — every dep fails closed when its store
		// is nil (CH down at boot → 503 SERVICE_DEGRADED, never an empty
		// report standing in for "unavailable").
		"GET /api/v1/history/ticks/{symbol}":  http.HandlerFunc(api.HistoryTicks(histDeps)),
		"GET /api/v1/history/klines/{symbol}": http.HandlerFunc(api.HistoryKlines(histDeps)),
		// Phase-23 market-data products — history/trades + export
		// (23.3.2/.4), block tape (23.3.7), swap-rate series (23.3.9),
		// stats surfaces (23.3.6/.10/.11). Same fail-closed convention:
		// unwired sources answer SERVICE_DEGRADED.
		"GET /api/v1/history/trades/{symbol}":             http.HandlerFunc(api.HistoryTrades(histDeps)),
		"GET /api/v1/history/trades/{symbol}/export":      http.HandlerFunc(api.HistoryTradesExport(exportDeps)),
		"GET /api/v1/export-jobs":                         http.HandlerFunc(api.ExportJobList(exportDeps)),
		"GET /api/v1/export-jobs/{id}":                    http.HandlerFunc(api.ExportJobStatus(exportDeps)),
		"GET /api/v1/export-jobs/{id}/download":           http.HandlerFunc(api.ExportJobDownload(exportDeps)),
		"GET /api/v1/history/block-trades/{symbol}":       http.HandlerFunc(api.HistoryBlockTrades(blockTapeDeps)),
		"GET /api/v1/history/swap-rates":                  http.HandlerFunc(api.HistorySwapRates(swapRateDeps)),
		"GET /api/v1/analytics/open-interest/{symbol}":    http.HandlerFunc(api.AnalyticsOpenInterest(statsDeps)),
		"GET /api/v1/analytics/long-short-ratio/{symbol}": http.HandlerFunc(api.AnalyticsLongShortRatio(statsDeps)),
		"GET /api/v1/analytics/taker-flow/{symbol}":       http.HandlerFunc(api.AnalyticsTakerFlow(statsDeps)),
		"GET /api/v1/market/taker-volume":                 http.HandlerFunc(api.MarketTakerVolume(statsDeps)),
		"GET /api/v1/market/positioning":                  http.HandlerFunc(api.MarketPositioning(statsDeps)),
		"GET /api/v1/market/performance":                  http.HandlerFunc(api.MarketPerformance(statsDeps)),
		"GET /api/v1/analytics/volume":                    http.HandlerFunc(api.AnalyticsVolume(anDeps)),
		"GET /api/v1/analytics/stats":                     http.HandlerFunc(api.AnalyticsStats(anDeps)),
		"GET /api/v1/analytics/pnl":                       http.HandlerFunc(api.AnalyticsPnL(anDeps)),
		"GET /api/v1/account/statements":                  http.HandlerFunc(api.AccountStatements(stmtSvc)),
		"GET /api/v1/account/statements/{id}/download":    http.HandlerFunc(api.AccountStatementDownload(stmtSvc)),
		"GET /api/v1/account/confirmations/{trade_id}": http.HandlerFunc(api.AccountConfirmation(api.ConfirmationReadDeps{
			Service: confSvc, AdminLookup: confTracker})),
		"GET /api/v1/account/income":    http.HandlerFunc(api.AccountIncome(incomeSrc)),
		"GET /api/v1/account/snapshots": http.HandlerFunc(api.AccountSnapshots(snapStore)),
		"GET /api/v1/reports/tca/{account_id}": http.HandlerFunc(api.ReportsTCA(api.TCAReportDeps{
			Reports: tcaRep, Classes: analytics.NewPgInstrumentClassResolver(pool)})),
		"GET /api/v1/admin/finance/trial-balance": http.HandlerFunc(api.AdminTrialBalance(tbSvc)),
		"GET /api/v1/admin/finance/pnl":           http.HandlerFunc(api.AdminFinancePnL(tbSvc)),
		"GET /api/v1/admin/finance/balance-sheet": http.HandlerFunc(api.AdminFinanceBalanceSheet(tbSvc)),
		"GET /api/v1/admin/invoices":              http.HandlerFunc(api.AdminInvoices(invSvc)),
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
		// Phase-18 Task 18.3.9 item 5 — live FIX-session entitlement /
		// throttle / cancel-on-disconnect administration (fix_sessions).
		"PUT /api/v1/admin/fix-sessions/{id}": http.HandlerFunc(
			api.FixSessionUpdateHandler(fix.NewPgStore(pool),
				fixPgCheckers, fixPgCheckers)),
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
		// --- Phase-15 Tasks 15.3.1/15.3.2/15.3.9: instrument lifecycle ---
		// §7.2 role/dual-control matrix: create/resume/delist go through
		// the four-eyes queue (202 PENDING); the rest are single-approver.
		"GET /api/v1/admin/instruments":                   api.AdminInstrumentsList(instrumentSvc),
		"POST /api/v1/admin/instruments":                  api.AdminInstrumentCreate(dualSvc),
		"PUT /api/v1/admin/instruments/{id}":              api.AdminInstrumentUpdate(instrumentSvc, true),
		"POST /api/v1/admin/instruments/{id}/activate":    api.AdminInstrumentTransition(instrumentSvc, admin.LcOpActivate, true),
		"POST /api/v1/admin/instruments/{id}/suspend":     api.AdminInstrumentTransition(instrumentSvc, admin.LcOpSuspend, true),
		"POST /api/v1/admin/instruments/{id}/restrict":    api.AdminInstrumentTransition(instrumentSvc, admin.LcOpRestrict, true),
		"POST /api/v1/admin/instruments/{id}/cancel-only": api.AdminInstrumentTransition(instrumentSvc, admin.LcOpCancelOnly, true),
		"POST /api/v1/admin/instruments/{id}/halt":        api.AdminInstrumentTransition(instrumentSvc, admin.LcOpHalt, true),
		"POST /api/v1/admin/instruments/{id}/resume":      api.AdminInstrumentResume(dualSvc),
		"POST /api/v1/admin/instruments/{id}/delist":      api.AdminInstrumentDelist(dualSvc),
		// --- Phase-15 Tasks 15.3.12/15.3.13: listing proposals, ops
		// board, auction calendar. APPROVE reviews and the calendar PUT
		// file four-eyes requests (202 PENDING); reads stay live.
		"GET /api/v1/admin/listing-proposals":                     http.HandlerFunc(api.AdminListingProposalsList(listingSvc)),
		"POST /api/v1/admin/listing-proposals":                    http.HandlerFunc(api.AdminListingProposalCreate(listingSvc, true)),
		"POST /api/v1/admin/listing-proposals/{id}/review":        http.HandlerFunc(api.AdminListingProposalReview(listingSvc, true)),
		"GET /api/v1/admin/ops-board":                             http.HandlerFunc(api.AdminOpsBoard(opsBoardSvc)),
		"GET /api/v1/admin/instruments/{symbol}/auction-calendar": http.HandlerFunc(api.AdminAuctionCalendarGet(instCalStore)),
		"PUT /api/v1/admin/instruments/{symbol}/auction-calendar": http.HandlerFunc(api.AdminAuctionCalendarPut(dualSvc, true)),
		// --- Phase-15 Task 15.3.5: obvious-error trade bust / price-adjust ---
		// Risk Manager route gate + §8.2 two-principal execution inside
		// the service; 202 PENDING when approver_id is omitted.
		"POST /api/v1/admin/trades/{id}/bust": api.AdminTradeBust(tradeBustSvc),
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
		// --- Phase-09 Task 9.3.29 item 4: secrets inventory ---
		"GET /api/v1/admin/security/secrets-inventory": http.HandlerFunc(
			api.AdminSecretsInventoryList(secretsInvSvc)),
		"PUT /api/v1/admin/security/secrets-inventory":                      api.AdminSecretsInventoryUpsert(secretsInvSvc, true),
		"POST /api/v1/admin/security/secrets-inventory/{name}/mark-rotated": api.AdminSecretsInventoryMarkRotated(secretsInvSvc, true),
		// --- Phase-07 Task 7.3.9: LP management ---
		"GET /api/v1/admin/liquidity-providers":                http.HandlerFunc(api.AdminLPList(lpSvc, true)),
		"POST /api/v1/admin/liquidity-providers":               http.HandlerFunc(api.AdminLPCreate(lpSvc, true)),
		"GET /api/v1/admin/liquidity-providers/{id}":           http.HandlerFunc(api.AdminLPGet(lpSvc, true)),
		"PUT /api/v1/admin/liquidity-providers":                http.HandlerFunc(api.AdminLPUpdate(lpSvc, true)),
		"PUT /api/v1/admin/liquidity-providers/{id}":           http.HandlerFunc(api.AdminLPUpdate(lpSvc, true)),
		"GET /api/v1/admin/liquidity-providers/{id}/scorecard": http.HandlerFunc(api.AdminLPScorecard(lpSvc, true)),
		"GET /api/v1/admin/liquidity-providers/{id}/alerts":    http.HandlerFunc(api.AdminLPAlerts(lpSvc, true)),
		// --- Phase-18 Task 18.3.10: market-maker program admin ---
		"GET /api/v1/admin/mm-programs":  http.HandlerFunc(api.AdminMMProgramList(mmSvc, true)),
		"POST /api/v1/admin/mm-programs": http.HandlerFunc(api.AdminMMProgramEnroll(mmSvc, true)),
		"GET /api/v1/admin/mm-programs/{id}": http.HandlerFunc(
			api.AdminMMProgramGet(mmSvc, true)),
		"PUT /api/v1/admin/mm-programs/{id}": http.HandlerFunc(
			api.AdminMMProgramUpdate(mmSvc, true)),
		"POST /api/v1/admin/mm-programs/{id}/suspend": http.HandlerFunc(
			api.AdminMMProgramSuspend(mmSvc, true)),
		"POST /api/v1/admin/mm-programs/{id}/resume": http.HandlerFunc(
			api.AdminMMProgramResume(mmSvc, true)),
		"POST /api/v1/admin/mm-programs/{id}/mmp-reset": http.HandlerFunc(
			api.AdminMMProgramMMPReset(mmTracker, true)),
		"GET /api/v1/admin/mm-programs/{id}/compliance": http.HandlerFunc(
			api.AdminMMProgramCompliance(mmSvc, true)),
		"GET /api/v1/admin/mm-programs/{id}/rebates": http.HandlerFunc(
			api.AdminMMProgramRebates(mmSvc, true)),
		"POST /api/v1/admin/mm-programs/rebates/post": http.HandlerFunc(
			api.AdminMMRebatePost(mmSvc, true)),
		// --- Phase-07 Tasks 7.3.13/7.3.14: governance packs ---
		"GET /api/v1/admin/governance-packs":               http.HandlerFunc(api.AdminPackList(packSvc, true)),
		"GET /api/v1/admin/governance-packs/{id}":          http.HandlerFunc(api.AdminPackGet(packSvc, true)),
		"POST /api/v1/admin/governance-packs/generate":     http.HandlerFunc(api.AdminPackGenerate(packSvc, true)),
		"POST /api/v1/admin/governance-packs/{id}/release": http.HandlerFunc(api.AdminPackRelease(packSvc, true)),
		// --- Phase-05 Wave-2 Cluster E live handlers ---
		// Tasks 5.3.26/5.3.31: unified WS endpoint.
		"WS /ws/v1": wsSrv,
		// Phase-17 Task 17.3.2 — dedicated premium L3 order-level stream
		// + WAL-reconstructed point-in-time snapshot.
		"WS /ws/v1/l3/{symbol}": l3Srv,
		// Registry declares auth.required + scope=read (authRead) — the
		// handler does not consult claims, so the scope gate wraps the
		// mount (F-L3-AUTH-1: premium L3 book was served anonymously).
		"GET /api/v1/market-data/l3-snapshot/{symbol}": auth.RequireScope("read")(
			http.HandlerFunc(api.MarketDataL3Snapshot(l3SnapDeps))),
		// Task 5.3.30: dual-control manual liquidation.
		"POST /api/v1/admin/liquidation/manual": api.ManualLiquidationHandler(manLiqSvc, true),
		// Phase-19 Tasks 19.3.8/19.3.14 — insurance-fund admin view and
		// the collateral-schedule write (audit-logged; ≤5s propagation via
		// the service cache TTL + post-write invalidation).
		"GET /api/v1/admin/insurance-fund": http.HandlerFunc(
			api.AdminInsuranceFund(fundH, adminRoleResolver)),
		"PUT /api/v1/admin/collateral-schedule": http.HandlerFunc(
			api.AdminCollateralSchedulePut(collateralH, adminRoleResolver)),
		// Phase-19 Tasks 19.3.21/19.3.24 — §13.12 model-parameter change
		// proposals and the §13.14 entity leverage matrix ride the §8.2
		// dual-control queue (makers create PENDING requests; a second
		// principal's approval runs the gated executor).
		"POST /api/v1/admin/margin-param-changes": http.HandlerFunc(
			api.AdminMarginParamChangeSubmit(dualSvc, adminRoleResolver)),
		"POST /api/v1/admin/entity-leverage-policy": http.HandlerFunc(
			api.AdminEntityLeveragePolicySubmit(dualSvc, adminRoleResolver)),
		"GET /api/v1/admin/entity-leverage-policy": http.HandlerFunc(
			api.AdminEntityLeveragePolicyList(levStore, adminRoleResolver)),
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
		// Phase-16 Tasks 16.3.14/.20/.24 — bracket/OTO submit +
		// OPO/OPOCO composite order lists.
		"POST /api/v1/orders/bracket":     http.HandlerFunc(api.OrderBracketSubmit(orderDeps)),
		"GET /api/v1/order-lists":         http.HandlerFunc(api.OrderListsOpen(orderDeps)),
		"POST /api/v1/order-lists":        http.HandlerFunc(api.OrderListSubmit(orderDeps)),
		"GET /api/v1/order-lists/history": http.HandlerFunc(api.OrderListsHistory(orderDeps)),
		"GET /api/v1/order-lists/{id}":    http.HandlerFunc(api.OrderListGet(orderDeps)),
		"DELETE /api/v1/order-lists/{id}": http.HandlerFunc(api.OrderListCancel(orderDeps)),

		// Phase-16 algo surface (Tasks 16.3.1/2/6/7/8/18/21).
		"POST /api/v1/orders/twap":             http.HandlerFunc(api.AlgoSubmitTyped(algoDeps, algo.TypeTWAP)),
		"POST /api/v1/orders/vwap":             http.HandlerFunc(api.AlgoSubmitTyped(algoDeps, algo.TypeVWAP)),
		"POST /api/v1/orders/scaled":           http.HandlerFunc(api.AlgoSubmitTyped(algoDeps, algo.TypeScaled)),
		"POST /api/v1/orders/spread":           http.HandlerFunc(api.AlgoSubmitTyped(algoDeps, algo.TypeSpread)),
		"POST /api/v1/orders/algo":             http.HandlerFunc(api.AlgoSubmit(algoDeps)),
		"POST /api/v1/orders/algo/{id}/pause":  http.HandlerFunc(api.AlgoPause(algoDeps)),
		"POST /api/v1/orders/algo/{id}/resume": http.HandlerFunc(api.AlgoResume(algoDeps)),
		"DELETE /api/v1/orders/algo/{id}":      http.HandlerFunc(api.AlgoCancel(algoDeps)),
		"GET /api/v1/algo-orders":              http.HandlerFunc(api.AlgoList(algoDeps)),
		"DELETE /api/v1/algo-orders":           http.HandlerFunc(api.AlgoCancelAll(algoDeps)),
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
		// ---- Phase-21 Tasks 21.3.1/21.3.11/21.3.23 — sanctions
		//      screening admin surface (status / forced refresh /
		//      queue replay / per-account screen / adverse-media intake).
		//      Nil deps fail closed to 503 inside the handlers.
		"GET /api/v1/admin/sanctions/status": http.HandlerFunc(
			api.AdminSanctionsStatus(screeningDeps)),
		"POST /api/v1/admin/sanctions/refresh": http.HandlerFunc(
			api.AdminSanctionsRefresh(screeningDeps, true)),
		"POST /api/v1/admin/sanctions/queue/replay": http.HandlerFunc(
			api.AdminSanctionsQueueReplay(screeningDeps, true)),
		"POST /api/v1/admin/compliance/screening/accounts/{id}": http.HandlerFunc(
			api.AdminScreenAccount(screeningDeps, true)),
		"POST /api/v1/admin/compliance/screening/adverse-media": http.HandlerFunc(
			api.AdminAdverseMediaIntake(screeningDeps, true)),
		// ---- Phase-21 Tasks 21.3.2/21.3.3/21.3.6 — travel rule, SAR
		//      lifecycle, FinCEN MSB/AML register ----
		"POST /api/v1/admin/sar":     http.HandlerFunc(api.AdminSARCreate(sarSvc, true)),
		"GET /api/v1/admin/sar":      http.HandlerFunc(api.AdminSARList(sarSvc)),
		"GET /api/v1/admin/sar/{id}": http.HandlerFunc(api.AdminSARGet(sarSvc)),
		"POST /api/v1/admin/sar/{id}/review": http.HandlerFunc(
			api.AdminSARReview(sarSvc, true)),
		"POST /api/v1/admin/sar/{id}/approve": http.HandlerFunc(
			api.AdminSARApprove(sarSvc, true)),
		"POST /api/v1/admin/sar/{id}/file": http.HandlerFunc(
			api.AdminSARFile(sarSvc, true)),
		"POST /api/v1/admin/sar/{id}/reject": http.HandlerFunc(
			api.AdminSARReject(sarSvc, true)),
		"GET /api/v1/admin/travel-rule": http.HandlerFunc(
			api.AdminTravelRuleList(travelSvc)),
		"GET /api/v1/admin/travel-rule/{id}": http.HandlerFunc(
			api.AdminTravelRuleGet(travelSvc)),
		"POST /api/v1/admin/travel-rule/{id}/supply": http.HandlerFunc(
			api.AdminTravelRuleSupply(travelSvc, true)),
		"GET /api/v1/admin/ctr": http.HandlerFunc(api.AdminCTRList(amlSvc)),
		"GET /api/v1/admin/aml/monitoring": http.HandlerFunc(
			api.AdminAMLMonitoring(amlSvc)),
		"GET /api/v1/admin/aml/artifacts": http.HandlerFunc(
			api.AdminAMLArtifactList(amlSvc)),
		"POST /api/v1/admin/aml/artifacts": http.HandlerFunc(
			api.AdminAMLArtifactRegister(amlSvc,
				compliance.HoldRoleResolver(adminRoleResolver), true)),
		"GET /api/v1/admin/aml/program": http.HandlerFunc(
			api.AdminAMLProgramStatus(amlSvc)),
		// ---- Phase-21 Tasks 21.3.4/.5/.9/.14/.16 — regulatory reporting ----
		"GET /api/v1/admin/emir-report": http.HandlerFunc(
			api.AdminRegEvents(api.RegReportingDeps{
				Svc: regDeps.Svc, Ledger: regDeps.Ledger, TrustProxy: true,
				ForceRegime: regreport.RegimeEMIRREFIT})),
		"GET /api/v1/admin/regreporting/events": http.HandlerFunc(
			api.AdminRegEvents(regDeps)),
		"GET /api/v1/admin/regreporting/events/{id}": http.HandlerFunc(
			api.AdminRegEventDetail(regDeps)),
		"GET /api/v1/admin/regreporting/submissions": http.HandlerFunc(
			api.AdminRegSubmissions(regDeps)),
		"GET /api/v1/admin/regreporting/queue": http.HandlerFunc(
			api.AdminRegQueue(regDeps)),
		"POST /api/v1/admin/regreporting/submissions/{id}/resubmit": http.HandlerFunc(
			api.AdminRegResubmit(regDeps)),
		"POST /api/v1/admin/regreporting/breaks/{id}/resolve": http.HandlerFunc(
			api.AdminRegBreakResolve(regDeps)),
		"POST /api/v1/admin/regreporting/party-identifiers": http.HandlerFunc(
			api.AdminRegPartyUpsert(regDeps)),
		"POST /api/v1/admin/regreporting/acks": http.HandlerFunc(
			api.AdminRegAckIngest(regDeps)),
		"POST /api/v1/admin/regreporting/reconcile": http.HandlerFunc(
			api.AdminRegReconcile(regDeps)),
		// ---- Phase-21 wave-2 governance mounts ----
		// Task 21.3.13 — Basel III capital & leverage pack.
		"GET /api/v1/admin/basel-report": http.HandlerFunc(
			api.AdminBaselReport(baselSvc)),
		// Task 21.3.17 — FX Global Code 55-principle review.
		"GET /api/v1/admin/fx-global-code/assessments": http.HandlerFunc(
			api.AdminFXGCList(fxgcSvc)),
		"POST /api/v1/admin/fx-global-code/assessments": http.HandlerFunc(
			api.AdminFXGCStart(fxgcSvc, true)),
		"GET /api/v1/admin/fx-global-code/assessments/{id}": http.HandlerFunc(
			api.AdminFXGCGet(fxgcSvc)),
		"POST /api/v1/admin/fx-global-code/assessments/{id}/verdicts": http.HandlerFunc(
			api.AdminFXGCVerdict(fxgcSvc, true)),
		"POST /api/v1/admin/fx-global-code/assessments/{id}/complete": http.HandlerFunc(
			api.AdminFXGCComplete(fxgcSvc, true)),
		"POST /api/v1/admin/fx-global-code/assessments/{id}/sign": http.HandlerFunc(
			api.AdminFXGCSign(fxgcSvc, true)),
		"POST /api/v1/admin/fx-global-code/assessments/{id}/publish": http.HandlerFunc(
			api.AdminFXGCPublish(fxgcSvc, true)),
		// Task 21.3.25 — regulatory change watch register (reads pass
		// the role resolver so the auditor scope is enforced in-service).
		"GET /api/v1/admin/regulatory-changes": http.HandlerFunc(
			api.AdminRegChangeList(regChangeSvc,
				compliance.HoldRoleResolver(adminRoleResolver), true)),
		"POST /api/v1/admin/regulatory-changes": http.HandlerFunc(
			api.AdminRegChangeCreate(regChangeSvc, true)),
		"GET /api/v1/admin/regulatory-changes/{id}/impact": http.HandlerFunc(
			api.AdminRegChangeImpactGet(regChangeSvc,
				compliance.HoldRoleResolver(adminRoleResolver), true)),
		"PUT /api/v1/admin/regulatory-changes/{id}/impact": http.HandlerFunc(
			api.AdminRegChangeImpactPut(regChangeSvc, true)),
		"POST /api/v1/admin/regulatory-changes/{id}/transition": http.HandlerFunc(
			api.AdminRegChangeTransition(regChangeSvc, true)),
		"POST /api/v1/admin/regulatory-changes/{id}/correspondence": http.HandlerFunc(
			api.AdminRegChangeCorrespondence(regChangeSvc, true)),
		"POST /api/v1/admin/regulatory-changes/impacts/{id}/done": http.HandlerFunc(
			api.AdminRegChangeImpactDone(regChangeSvc, true)),
		// Task 21.3.28 — execution policy lifecycle + consent.
		"GET /api/v1/execution-policy": http.HandlerFunc(
			api.PublicExecutionPolicy(execPolicySvc)),
		"PUT /api/v1/account/consent": http.HandlerFunc(
			api.AccountConsent(execPolicySvc, true)),
		"GET /api/v1/admin/execution-policies": http.HandlerFunc(
			api.AdminPolicyList(execPolicySvc)),
		"POST /api/v1/admin/execution-policies": http.HandlerFunc(
			api.AdminPolicyDraft(execPolicySvc, true)),
		"POST /api/v1/admin/execution-policies/{id}/activate": http.HandlerFunc(
			api.AdminPolicyActivate(execPolicySvc, true)),
		"POST /api/v1/admin/execution-policies/{id}/review": http.HandlerFunc(
			api.AdminPolicyReview(execPolicySvc, true)),

		// ---- Phase-21 wave-2 mounts: enforcement (21.3.8), cases
		//      (21.3.21), RTS 6/DEA (21.3.12), employee dealing +
		//      restricted lists (21.3.24), tuning/audit-trail/reporting
		//      values (21.3.27).
		"POST /api/v1/admin/enforcement/{signal_id}": http.HandlerFunc(
			api.AdminEnforce(enforceSvc, true)),
		"GET /api/v1/admin/enforcement": http.HandlerFunc(
			api.AdminEnforcementList(enforceSvc)),
		"GET /api/v1/admin/surveillance/cases": http.HandlerFunc(
			api.AdminSurveillanceCases(caseSvc)),
		"GET /api/v1/admin/surveillance/cases/{id}": http.HandlerFunc(
			api.AdminSurveillanceCaseGet(caseSvc)),
		"POST /api/v1/admin/surveillance/cases/{id}/assign": http.HandlerFunc(
			api.AdminSurveillanceCaseAssign(caseSvc, true)),
		"POST /api/v1/admin/surveillance/cases/{id}/evidence": http.HandlerFunc(
			api.AdminSurveillanceCaseEvidence(caseSvc, true)),
		"POST /api/v1/admin/surveillance/cases/{id}/disposition": http.HandlerFunc(
			api.AdminSurveillanceCaseDisposition(caseSvc, true)),
		"GET /api/v1/admin/surveillance/summary": http.HandlerFunc(
			api.AdminSurveillanceSummary(caseSvc)),
		"GET /api/v1/admin/algo-certifications": http.HandlerFunc(
			api.AdminAlgoCertList(rts6Svc)),
		"POST /api/v1/admin/algo-certifications": http.HandlerFunc(
			api.AdminAlgoCertify(rts6Svc, true)),
		"POST /api/v1/admin/algo-certifications/{id}/transition": http.HandlerFunc(
			api.AdminAlgoCertTransition(rts6Svc, true)),
		"GET /api/v1/admin/dea/controls": http.HandlerFunc(
			api.AdminDEAGet(rts6Svc)),
		"POST /api/v1/admin/dea/controls": http.HandlerFunc(
			api.AdminDEASet(rts6Svc, true)),
		"POST /api/v1/admin/dea/controls/{session_id}/suspend": http.HandlerFunc(
			api.AdminDEASuspend(rts6Svc, true)),
		"GET /api/v1/admin/rts6/self-assessments": http.HandlerFunc(
			api.AdminRTS6Assessments(rts6Svc)),
		"POST /api/v1/admin/rts6/self-assessments": http.HandlerFunc(
			api.AdminRTS6FileAssessment(rts6Svc, true)),
		"GET /api/v1/admin/order-records/{order_id}/export": http.HandlerFunc(
			api.AdminOrderLifecycleExport(rts6Svc)),
		"GET /api/v1/admin/restricted-lists": http.HandlerFunc(
			api.AdminRestrictedList(restrictedSvc)),
		"POST /api/v1/admin/restricted-lists": http.HandlerFunc(
			api.AdminRestrictedListCreate(restrictedSvc, true)),
		"DELETE /api/v1/admin/restricted-lists": http.HandlerFunc(
			api.AdminRestrictedListRetire(restrictedSvc, true)),
		"POST /api/v1/admin/pre-clearance": http.HandlerFunc(
			api.AdminPreClearance(dealingSvc, true)),
		"GET /api/v1/admin/pre-clearance": http.HandlerFunc(
			api.AdminPreClearanceList(dealingSvc)),
		"GET /api/v1/admin/employee-dealing/audit": http.HandlerFunc(
			api.AdminEmployeeDealingAudit(dealingSvc)),
		"GET /api/v1/admin/surveillance/tuning": http.HandlerFunc(
			api.AdminTuningList(tuningSvc)),
		"POST /api/v1/admin/surveillance/tuning": http.HandlerFunc(
			api.AdminTuningPropose(tuningSvc, true)),
		"POST /api/v1/admin/surveillance/tuning/{signal}/activate": http.HandlerFunc(
			api.AdminTuningActivate(tuningSvc, true)),
		"GET /api/v1/admin/surveillance/tuning/{signal}/backtest": http.HandlerFunc(
			api.AdminTuningBacktest(tuningSvc)),
		"GET /api/v1/admin/audit/trail": http.HandlerFunc(
			api.AdminAuditTrail(auditTrail, adminRoleResolver)),
		"GET /api/v1/admin/audit/chain": http.HandlerFunc(
			api.AdminAuditChain(auditTrail, adminRoleResolver)),
		"GET /api/v1/admin/reporting-values": http.HandlerFunc(
			api.AdminReportingValuesList(reportingVals)),
		"POST /api/v1/admin/reporting-values": http.HandlerFunc(
			api.AdminReportingValuesSet(reportingVals, true)),

		// ---- Phase-21 wave-3 mounts: GDPR & geo (21.3.7), data
		//      residency (21.3.18), comms recording (21.3.20),
		//      CRS/FATCA tax reporting (21.3.22), financial promotions
		//      (21.3.26). ----
		"POST /api/v1/account/gdpr/export": http.HandlerFunc(
			api.GDPRExport(gdprSvc)),
		"POST /api/v1/account/gdpr/erase": http.HandlerFunc(
			api.GDPRErase(gdprSvc)),
		"GET /api/v1/account/gdpr": http.HandlerFunc(
			api.GDPRRequests(gdprSvc)),
		"GET /api/v1/account/gdpr/consent": http.HandlerFunc(
			api.ConsentList(gdprSvc)),
		"PUT /api/v1/account/gdpr/consent": http.HandlerFunc(
			api.ConsentPut(gdprSvc)),
		"GET /api/v1/admin/data-residency/policies": http.HandlerFunc(
			api.AdminResidencyPolicies(residencySvc)),
		"GET /api/v1/admin/data-residency/access-log": http.HandlerFunc(
			api.AdminResidencyAccessLog(residencySvc)),
		"POST /api/v1/admin/accounts/{id}/jurisdiction": http.HandlerFunc(
			api.AdminAccountJurisdictionPin(residencySvc)),
		"GET /api/v1/admin/promotions": http.HandlerFunc(
			api.AdminPromotionsList(promoSvc)),
		"POST /api/v1/admin/promotions": http.HandlerFunc(
			api.AdminPromotionsCreate(promoSvc)),
		"PUT /api/v1/admin/promotions": http.HandlerFunc(
			api.AdminPromotionsRevise(promoSvc)),
		"POST /api/v1/admin/promotions/{id}/submit": http.HandlerFunc(
			api.AdminPromotionSubmit(promoSvc)),
		"POST /api/v1/admin/promotions/{id}/approve": http.HandlerFunc(
			api.AdminPromotionApprove(promoSvc)),
		"POST /api/v1/admin/promotions/{id}/reject": http.HandlerFunc(
			api.AdminPromotionReject(promoSvc)),
		"POST /api/v1/admin/promotions/{id}/withdraw": http.HandlerFunc(
			api.AdminPromotionWithdraw(promoSvc)),
		"GET /api/v1/promotions/{id}": http.HandlerFunc(
			api.PromotionPublicRender(promoGate)),
		// ---- end Phase-21 wave-3 mounts ----
		// ---- end Phase-21 wave-2 mounts ----
		// ---- end Phase-21 AML mounts ----
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
		// --- Phase-15 Tasks 15.3.4/15.3.7 — market schedule + session ---
		"GET /api/v1/session/status": http.HandlerFunc(api.SessionStatus(sessSvc)),
		"GET /api/v1/admin/market-schedule": http.HandlerFunc(
			api.AdminMarketSchedule(schedSvc, true)),
		"GET /api/v1/admin/market-schedule/overrides": http.HandlerFunc(
			api.AdminScheduleOverrideList(schedSvc, true)),
		"POST /api/v1/admin/market-schedule/overrides": http.HandlerFunc(
			api.AdminScheduleOverrideCreate(schedSvc, true)),
		"PUT /api/v1/admin/market-schedule/overrides/{id}": http.HandlerFunc(
			api.AdminScheduleOverrideUpdate(schedSvc, true)),
		"DELETE /api/v1/admin/market-schedule/overrides/{id}": http.HandlerFunc(
			api.AdminScheduleOverrideDelete(schedSvc, true)),
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
		// --- Phase-16 Task 16.3.19 — grid bots ---
		"POST /api/v1/bots/grid":        http.HandlerFunc(api.GridBotCreate(gridDeps)),
		"GET /api/v1/bots/grid":         http.HandlerFunc(api.GridBotList(gridDeps)),
		"GET /api/v1/bots/grid/{id}":    http.HandlerFunc(api.GridBotGet(gridDeps)),
		"DELETE /api/v1/bots/grid/{id}": http.HandlerFunc(api.GridBotStop(gridDeps)),
		// Phase-10 Task 10.3.26 — pause/resume (children keep working;
		// fills book, no new legs while PAUSED; resume re-arms).
		"POST /api/v1/bots/grid/{id}/pause":  http.HandlerFunc(api.GridBotPause(gridDeps)),
		"POST /api/v1/bots/grid/{id}/resume": http.HandlerFunc(api.GridBotResume(gridDeps)),
		// --- Phase-16 Task 16.3.21 — strategies + marketplace ---
		"POST /api/v1/strategies":             http.HandlerFunc(api.StrategyCreate(stratDeps)),
		"GET /api/v1/strategies":              http.HandlerFunc(api.StrategyList(stratDeps)),
		"GET /api/v1/strategies/{id}":         http.HandlerFunc(api.StrategyGet(stratDeps)),
		"POST /api/v1/strategies/{id}/pause":  http.HandlerFunc(api.StrategyPause(stratDeps)),
		"POST /api/v1/strategies/{id}/resume": http.HandlerFunc(api.StrategyResume(stratDeps)),
		"DELETE /api/v1/strategies/{id}":      http.HandlerFunc(api.StrategyCancel(stratDeps)),
		"GET /api/v1/strategy-templates":      http.HandlerFunc(api.StrategyTemplates(stratDeps)),
		"POST /api/v1/strategy-templates":     http.HandlerFunc(api.StrategyTemplatePublish(stratDeps)),
		"POST /api/v1/strategy-templates/{id}/instantiate": http.HandlerFunc(
			api.StrategyTemplateInstantiate(stratDeps)),
		"GET /api/v1/admin/strategy-templates": http.HandlerFunc(
			api.AdminStrategyTemplates(stratDeps)),
		"POST /api/v1/admin/strategy-templates/{id}/approve": http.HandlerFunc(
			api.AdminStrategyTemplateDecide(stratDeps, true)),
		"POST /api/v1/admin/strategy-templates/{id}/reject": http.HandlerFunc(
			api.AdminStrategyTemplateDecide(stratDeps, false)),
	}
	// Phase-21 wave-3 conditional mounts — object-store- and config-
	// dependent services stay absent (fail-closed 503 shim) when their
	// backend cannot be constructed; mounting a handler over a nil
	// service would panic instead of degrading.
	// Phase-24 conditional mounts — the settlement-confirmation and
	// value-date-roll endpoints need a live SettlementService
	// (EXC_SENDER_BIC + holiday calendar); the rail-schedule surface
	// needs the cut-off service. Mounting a handler over a nil service
	// would panic instead of degrading to the registered 503 shim.
	if confirmationSvc != nil {
		live["POST /api/v1/admin/settlement-confirmations"] = http.HandlerFunc(
			api.AdminSettlementConfirmation(confirmationSvc))
	}
	if settleSvc != nil {
		live["POST /api/v1/admin/settlement/instructions/{id}/roll"] = http.HandlerFunc(
			api.AdminRollInstruction(settleSvc))
	}
	if railCutoffSvc != nil {
		live["GET /api/v1/admin/settlement/rail-schedules"] = http.HandlerFunc(
			api.AdminRailSchedules(railCutoffSvc))
		live["POST /api/v1/admin/settlement/rail-schedules/evaluate"] = http.HandlerFunc(
			api.AdminRailEvaluate(railCutoffSvc))
	}
	if commsSvc != nil {
		live["GET /api/v1/admin/comms-recordings"] = http.HandlerFunc(
			api.AdminCommsList(commsSvc))
		live["GET /api/v1/admin/comms-recordings/{id}"] = http.HandlerFunc(
			api.AdminCommsGet(commsSvc))
		live["POST /api/v1/admin/comms-recordings/{id}/retrieve"] = http.HandlerFunc(
			api.AdminCommsRetrieve(commsSvc))
		live["POST /api/v1/admin/comms-recordings/verify-day"] = http.HandlerFunc(
			api.AdminCommsVerifyDay(commsSvc))
	}
	if taxReportSvc != nil {
		live["GET /api/v1/admin/tax-reporting/runs"] = http.HandlerFunc(
			api.AdminTaxRunList(taxReportSvc))
		live["POST /api/v1/admin/tax-reporting/runs"] = http.HandlerFunc(
			api.AdminTaxRunGenerate(taxReportSvc))
		live["GET /api/v1/admin/tax-reporting/runs/{id}"] = http.HandlerFunc(
			api.AdminTaxRunGet(taxReportSvc))
		live["GET /api/v1/admin/tax-reporting/runs/{id}/xml"] = http.HandlerFunc(
			api.AdminTaxRunXML(taxReportSvc))
		live["POST /api/v1/admin/tax-reporting/runs/{id}/review"] = http.HandlerFunc(
			api.AdminTaxRunReview(taxReportSvc))
		live["POST /api/v1/admin/tax-reporting/runs/{id}/approve"] = http.HandlerFunc(
			api.AdminTaxRunApprove(taxReportSvc))
		live["POST /api/v1/admin/tax-reporting/runs/{id}/reject"] = http.HandlerFunc(
			api.AdminTaxRunReject(taxReportSvc))
		live["POST /api/v1/admin/tax-reporting/runs/{id}/submit"] = http.HandlerFunc(
			api.AdminTaxRunSubmit(taxReportSvc))
	}
	// Phase-21 Tasks 21.3.15/21.3.19 — regulated-venue governance +
	// public RTS 27/28 best-execution surface. The deps carry nil-able
	// services (RTS 27/28 absent while ClickHouse is down) — handlers
	// fail closed SERVICE_DEGRADED rather than 501/panic.
	vgDeps := api.VenueGovDeps{Venue: venueSvc, RTS27: rts27Svc, RTS28: rts28Svc}
	live["GET /api/v1/admin/venue/members"] = http.HandlerFunc(
		api.AdminVenueMemberList(vgDeps))
	live["POST /api/v1/admin/venue/members"] = http.HandlerFunc(
		api.AdminVenueMemberRegister(vgDeps, true))
	live["GET /api/v1/admin/venue/members/{id}"] = http.HandlerFunc(
		api.AdminVenueMemberGet(vgDeps))
	live["POST /api/v1/admin/venue/members/{id}/due-diligence"] = http.HandlerFunc(
		api.AdminVenueMemberDueDiligence(vgDeps, true))
	live["POST /api/v1/admin/venue/members/{id}/agreements"] = http.HandlerFunc(
		api.AdminVenueMemberAgreement(vgDeps, true))
	live["POST /api/v1/admin/venue/members/{id}/products"] = http.HandlerFunc(
		api.AdminVenueMemberProducts(vgDeps, true))
	live["POST /api/v1/admin/venue/members/{id}/decision"] = http.HandlerFunc(
		api.AdminVenueMemberDecision(vgDeps, true))
	live["POST /api/v1/admin/venue/members/{id}/suspend"] = http.HandlerFunc(
		api.AdminVenueMemberSuspend(vgDeps, true))
	live["POST /api/v1/admin/venue/members/{id}/reinstate"] = http.HandlerFunc(
		api.AdminVenueMemberReinstate(vgDeps, true))
	live["POST /api/v1/admin/venue/members/{id}/terminate"] = http.HandlerFunc(
		api.AdminVenueMemberTerminate(vgDeps, true))
	live["POST /api/v1/admin/venue/members/{id}/appeals"] = http.HandlerFunc(
		api.AdminVenueMemberAppeal(vgDeps, true))
	live["POST /api/v1/admin/venue/members/{id}/appeal-decision"] = http.HandlerFunc(
		api.AdminVenueMemberAppealDecision(vgDeps, true))
	live["POST /api/v1/admin/venue/members/{id}/reviews"] = http.HandlerFunc(
		api.AdminVenueMemberReview(vgDeps, true))
	live["GET /api/v1/admin/venue/rulebooks"] = http.HandlerFunc(
		api.AdminVenueRulebookList(vgDeps))
	live["POST /api/v1/admin/venue/rulebooks"] = http.HandlerFunc(
		api.AdminVenueRulebookDraft(vgDeps, true))
	live["GET /api/v1/admin/venue/rulebooks/{id}"] = http.HandlerFunc(
		api.AdminVenueRulebookGet(vgDeps))
	live["POST /api/v1/admin/venue/rulebooks/{id}/file"] = http.HandlerFunc(
		api.AdminVenueRulebookFile(vgDeps, true))
	live["POST /api/v1/admin/venue/rulebooks/{id}/regulator-decision"] = http.HandlerFunc(
		api.AdminVenueRulebookRegDecision(vgDeps, true))
	live["POST /api/v1/admin/venue/rulebooks/{id}/approve"] = http.HandlerFunc(
		api.AdminVenueRulebookApprove(vgDeps, true))
	live["POST /api/v1/admin/venue/rulebooks/{id}/activate"] = http.HandlerFunc(
		api.AdminVenueRulebookActivate(vgDeps, true))
	live["POST /api/v1/admin/venue/rulebooks/{id}/notices"] = http.HandlerFunc(
		api.AdminVenueNoticeIssue(vgDeps, true))
	live["POST /api/v1/admin/venue/rulebooks/{id}/acks"] = http.HandlerFunc(
		api.AdminVenueAck(vgDeps, true))
	live["GET /api/v1/admin/venue/interventions"] = http.HandlerFunc(
		api.AdminVenueInterventionList(vgDeps))
	live["POST /api/v1/admin/venue/interventions"] = http.HandlerFunc(
		api.AdminVenueInterventionRecord(vgDeps, true))
	live["POST /api/v1/admin/venue/interventions/{id}/lift"] = http.HandlerFunc(
		api.AdminVenueInterventionLift(vgDeps, true))
	live["GET /api/v1/admin/venue/cases"] = http.HandlerFunc(
		api.AdminVenueCaseList(vgDeps))
	live["POST /api/v1/admin/venue/cases"] = http.HandlerFunc(
		api.AdminVenueCaseOpen(vgDeps, true))
	live["GET /api/v1/admin/venue/cases/{id}"] = http.HandlerFunc(
		api.AdminVenueCaseGet(vgDeps))
	live["POST /api/v1/admin/venue/cases/{id}/evidence"] = http.HandlerFunc(
		api.AdminVenueCaseEvidence(vgDeps, true))
	live["POST /api/v1/admin/venue/cases/{id}/transition"] = http.HandlerFunc(
		api.AdminVenueCaseTransition(vgDeps, true))
	live["GET /api/v1/admin/venue/conflicts"] = http.HandlerFunc(
		api.AdminVenueConflictList(vgDeps))
	live["POST /api/v1/admin/venue/conflicts"] = http.HandlerFunc(
		api.AdminVenueConflictDeclare(vgDeps, true))
	live["POST /api/v1/admin/venue/conflicts/{id}/resolve"] = http.HandlerFunc(
		api.AdminVenueConflictResolve(vgDeps, true))
	live["GET /api/v1/admin/venue/self-assessments"] = http.HandlerFunc(
		api.AdminVenueAssessmentList(vgDeps))
	live["POST /api/v1/admin/venue/self-assessments"] = http.HandlerFunc(
		api.AdminVenueAssessmentFile(vgDeps, true))
	live["POST /api/v1/admin/venue/self-assessments/{id}/complete"] = http.HandlerFunc(
		api.AdminVenueAssessmentComplete(vgDeps, true))
	live["GET /api/v1/admin/venue/cco-reports"] = http.HandlerFunc(
		api.AdminVenueCCOList(vgDeps))
	live["POST /api/v1/admin/venue/cco-reports"] = http.HandlerFunc(
		api.AdminVenueCCOGenerate(vgDeps, true))
	live["POST /api/v1/admin/venue/cco-reports/{id}/sign"] = http.HandlerFunc(
		api.AdminVenueCCOSign(vgDeps, true))
	live["POST /api/v1/admin/venue/cco-reports/{id}/file"] = http.HandlerFunc(
		api.AdminVenueCCOFile(vgDeps, true))
	live["GET /api/v1/admin/venue/launch-prerequisites"] = http.HandlerFunc(
		api.AdminVenuePrereqList(vgDeps))
	live["POST /api/v1/admin/venue/launch-prerequisites"] = http.HandlerFunc(
		api.AdminVenuePrereqEvidence(vgDeps, true))
	live["POST /api/v1/admin/venue/launch-prerequisites/{id}/expire"] = http.HandlerFunc(
		api.AdminVenuePrereqExpire(vgDeps, true))
	live["GET /api/v1/admin/venue/launch-gate"] = http.HandlerFunc(
		api.AdminVenueLaunchGate(vgDeps))
	live["GET /api/v1/admin/mifid-report"] = http.HandlerFunc(
		api.AdminMiFIDReport(vgDeps))
	live["POST /api/v1/admin/bestexec/rts27/materialize"] = http.HandlerFunc(
		api.AdminRTS27Materialize(vgDeps, true))
	live["POST /api/v1/admin/bestexec/rts27/generate"] = http.HandlerFunc(
		api.AdminRTS27Generate(vgDeps, true))
	live["GET /api/v1/admin/bestexec/rts27"] = http.HandlerFunc(
		api.AdminRTS27List(vgDeps))
	live["GET /api/v1/admin/bestexec/rts27/{id}"] = http.HandlerFunc(
		api.AdminRTS27Get(vgDeps))
	live["POST /api/v1/admin/bestexec/rts27/{id}/publish"] = http.HandlerFunc(
		api.AdminRTS27Publish(vgDeps, true))
	live["POST /api/v1/admin/bestexec/rts28/generate"] = http.HandlerFunc(
		api.AdminRTS28Generate(vgDeps, true))
	live["GET /api/v1/admin/bestexec/rts28"] = http.HandlerFunc(
		api.AdminRTS28List(vgDeps))
	live["GET /api/v1/admin/bestexec/rts28/{id}"] = http.HandlerFunc(
		api.AdminRTS28Get(vgDeps))
	live["POST /api/v1/admin/bestexec/rts28/{id}/publish"] = http.HandlerFunc(
		api.AdminRTS28Publish(vgDeps, true))
	live["GET /api/v1/venue/best-execution/rts27"] = http.HandlerFunc(
		api.PublicRTS27List(vgDeps))
	live["GET /api/v1/venue/best-execution/rts27/{id}"] = http.HandlerFunc(
		api.PublicRTS27Get(vgDeps))
	live["GET /api/v1/venue/best-execution/rts27/{id}/csv"] = http.HandlerFunc(
		api.PublicRTS27CSV(vgDeps))
	live["GET /api/v1/venue/best-execution/rts28"] = http.HandlerFunc(
		api.PublicRTS28List(vgDeps))
	live["GET /api/v1/venue/best-execution/rts28/{id}"] = http.HandlerFunc(
		api.PublicRTS28Get(vgDeps))
	live["GET /api/v1/venue/best-execution/rts28/{id}/csv"] = http.HandlerFunc(
		api.PublicRTS28CSV(vgDeps))
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
				// Task 21.3.7: geo-block gate sits inside OptionalAuth —
				// claims are attached when present (RETAIL_BLOCK category
				// resolution needs the account id) while anonymous
				// mutating requests (registration) are still fenced.
				geoGate.Middleware()(
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
										deprecation.RedisHitSink(rdb.Client))(mux)))))))))

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
			// Phase-17: the L3 surface drains beside the unified endpoint —
			// its conns close 1001 with the drain reason.
			{Name: "l3_drain", Timeout: 15 * time.Second, Fn: func(c context.Context) error {
				l3Srv.Drain()
				return nil
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

// jetstreamFillPublisher adapts the JetStream context to the
// settlement.TradeRepublisher seam — publishes with Nats-Msg-Id dedup,
// the bridge.Publisher contract for TradeFill fan-out.
type jetstreamFillPublisher struct{ js jetstream.JetStream }

func (p jetstreamFillPublisher) PublishEvent(ctx context.Context, subject, msgID string, payload []byte) error {
	if p.js == nil {
		return fmt.Errorf("jetstream fill publisher: nil context")
	}
	_, err := p.js.Publish(ctx, subject, payload, jetstream.WithMsgID(msgID))
	return err
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
