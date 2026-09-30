// Command fix is the FIX protocol gateway (Phase-18). Binary naming
// deviation (documented in internal/fix/doc.go): the phase plan names
// services/cmd/fixgateway; the repo convention is single-word service
// names and the scaffold already lived here — it stays cmd/fix.
//
// Wiring: PostgreSQL (fix_sessions seq state + entitlement, orders read
// model), Redis (shard map, kill-switch, canonical dead-man countdown),
// Aeron aeron:ipc (orders_in publication multi-publisher submitter +
// orders_out lifecycle feed), QuickFIX/Go acceptor + initiators.
// fix.enabled=false → metrics/health only (scaffold behaviour preserved).
package main

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	"exchange/internal/accounts"
	"exchange/internal/admin"
	"exchange/internal/auth"
	"exchange/internal/compliance"
	"exchange/internal/config"
	"exchange/internal/db"
	"exchange/internal/fix"
	"exchange/internal/ipc"
	aeronclient "exchange/internal/ipc/aeron"
	"exchange/internal/marketapi"
	"exchange/internal/marketdata"
	"exchange/internal/marketmaking"
	excnats "exchange/internal/nats"
	"exchange/internal/observability"
	"exchange/internal/orders"
	"exchange/internal/redis"
	"exchange/internal/risk"
	"exchange/internal/utils"
	"exchange/pkg/logging"
)

const aeronTimeout = 5 * time.Second

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "fix: %v\n", err)
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
	slogger := slog.Default()

	ctx, stop := utils.SignalContext()
	defer stop()

	reg := observability.New()
	observability.NewMetrics(reg, "fix")
	go func() {
		if err := observability.ServeMetrics(ctx, cfg.Fix.Addr(), reg, "fix"); err != nil {
			log.Error("fix: metrics listener died", "err", err)
		}
	}()

	if !cfg.Fix.Enabled && len(cfg.Fix.OutwardSessions) == 0 {
		log.Info("fix started (acceptor disabled — metrics/health only)",
			"addr", cfg.Fix.Addr(), "env", cfg.Environment)
		<-ctx.Done()
		log.Info("fix shutting down")
		return nil
	}

	// ---- Shared infrastructure ------------------------------------------------

	rdb := redis.New(cfg.Redis.Addr, cfg.Redis.Password, cfg.Redis.DB)
	defer func() { _ = rdb.Close() }()

	shardCtx, shardCancel := context.WithTimeout(context.Background(), 5*time.Second)
	shardMap, shardSrc, err := config.LoadShardMapForService(shardCtx, rdb)
	shardCancel()
	if err != nil {
		return fmt.Errorf("shard map: %w", err)
	}
	log.Info("shard map loaded", "source", shardSrc.String(),
		"static_symbols", len(shardMap.Entries()))

	poolCtx, poolCancel := context.WithTimeout(context.Background(), 5*time.Second)
	pool, err := db.NewPool(poolCtx, cfg.Postgres.DSN, cfg.Postgres.MaxConns)
	poolCancel()
	if err != nil {
		return fmt.Errorf("postgres: %w", err)
	}
	defer pool.Close()

	// ---- Order pipeline (canonical orders.Service; Aeron submitter) -----------

	orderStore := orders.NewPgStore(pool)
	riskLimits := risk.NewLimitsService(risk.NewPgStore(pool), nil, nil)
	if err := riskLimits.Load(context.Background()); err != nil {
		log.Warn("risk limits initial load failed (fails closed per request)", "err", err)
	}
	riskLimits.StartRefresher(context.Background(), 60*time.Second,
		func(e error) { log.Warn("risk limits refresh failed", "err", e) })
	killResolver := admin.NewKillSwitchResolver(rdb, cfg.Environment)

	aero, err := aeronclient.Connect(cfg.Fix.AeronDir,
		uint64(cfg.Fix.AeronDriverTimeoutMS))
	if err != nil {
		return fmt.Errorf("aeron connect: %w", err)
	}
	defer aero.Close()

	pub, err := aero.AddPublication(fix.OrdersInURI, fix.OrdersInStream, aeronTimeout)
	if err != nil {
		return fmt.Errorf("aeron publish %q stream %d: %w",
			fix.OrdersInURI, fix.OrdersInStream, err)
	}
	submitter := fix.NewAeronSubmitter(pub)

	// Admission-gate parity with cmd/gateway: the canonical
	// orders.Service fails CLOSED when Breakers or Product is nil, so a
	// FIX session submitting through this process must consult the same
	// §2.6 five-tier breaker and MiFID II gates REST order entry does —
	// otherwise every FIX order rejects with CIRCUIT_BREAKER_OPEN /
	// SERVICE_DEGRADED.
	breakerSvc, err := risk.NewCircuitBreakerService(risk.BreakerDeps{
		Store:   risk.RedisBreakerStore{C: rdb},
		Events:  risk.NewPgBreakerEventStore(pool),
		Metrics: risk.NewBreakerMetrics(reg),
		IV:      risk.NullIVSource{},
		Logf:    func(f string, a ...any) { log.Warn(fmt.Sprintf(f, a...)) },
	})
	if err != nil {
		return fmt.Errorf("circuit-breaker service: %w", err)
	}
	if err := breakerSvc.Load(context.Background()); err != nil {
		// Boot continues: per-admission read-through hydration still
		// consults Redis — fail closed.
		log.Warn("circuit-breaker state load failed", "err", err)
	}
	go breakerSvc.Run(ctx, time.Second)

	// RTS 9 OTR monitor (Phase-13 Task 13.3.6): FIX sessions are exactly
	// the high-rate channel the ratio gate exists for. No MM-program
	// allowance here — mm_svc lives in cmd/gateway, and its absence
	// simply applies the more conservative venue default.
	otrMon := risk.NewOtrMonitor(rdb.Client, riskLimits).
		WithLogger(func(f string, a ...any) { log.Warn(fmt.Sprintf(f, a...)) }).
		WithMetrics(reg).
		WithTierResolver(func(ctx context.Context, accountID int64) (string, error) {
			var tier string
			err := pool.QueryRow(ctx,
				`SELECT kyc_tier::text FROM accounts WHERE id = $1`,
				accountID).Scan(&tier)
			return tier, err
		})
	go otrMon.Run(ctx, time.Second)

	// MiFID II appropriateness (Phase-14 Task 14.3.7): the Product gate
	// needs the compliance categorization service → KYC store → PII
	// SecretBox. Same data-key fallback rules as cmd/gateway (production
	// never reaches the dev key — config.Validate rejects it first). A
	// nil RoleResolver is safe: the gate path never consults roles.
	dataKey, kerr := config.DecodeDataKey(cfg.Secrets.DataKey)
	if kerr != nil {
		dev := sha256.Sum256([]byte("exc.local non-production secret-box data key v1"))
		dataKey = dev[:]
		log.Warn("secrets.data_key unset — non-production deterministic dev key in use")
	}
	secretBox, err := auth.NewSecretBox(dataKey)
	if err != nil {
		return fmt.Errorf("secret box: %w", err)
	}
	kycStore, err := compliance.NewPgStore(pool, secretBox)
	if err != nil {
		return fmt.Errorf("kyc store: %w", err)
	}
	catSvc, err := compliance.NewCategorizationService(kycStore, nil)
	if err != nil {
		return fmt.Errorf("categorization service: %w", err)
	}

	orderSvc, err := orders.NewService(orders.Options{
		Store:      orderStore,
		Submitter:  submitter,
		ShardMap:   shardMap,
		Limits:     riskLimits,
		KillSwitch: killResolver, // fail-closed admission gate
		Breakers:   breakerSvc,   // §2.6 five-tier breaker — REQUIRED (nil rejects all)
		Product:    catSvc,       // MiFID II appropriateness — REQUIRED (nil rejects all)
		Otr:        otrMon,       // RTS 9 OTR gate (optional but required for FIX parity)
		// Products (profile scope), cooling-off, batch RL, commission,
		// GSLO/auction/composite seams stay nil: profile gates are
		// pre-095-absent by design, the rest are REST/UX-only surfaces
		// the FIX order-entry contract does not carry.
	})
	if err != nil {
		return fmt.Errorf("order service: %w", err)
	}

	// ---- Dead-man (Task 18.3.16): the CANONICAL account countdown — the
	// shared Redis timer + atomic mass-cancel dispatcher of Phase-05 Task
	// 5.3.33. The sweeper runs in this process too: Lua pops are atomic,
	// so REST/WS/FIX sweepers never double-fire.
	orderDisp := orders.NewDispatcher(orderSvc)
	deadman := accounts.NewDeadManService(
		accounts.NewRedisCountdownStore(rdb.Client), orderDisp)
	go func() {
		if err := deadman.Run(ctx, 250*time.Millisecond); err != nil {
			log.Warn("dead-man sweeper exited", "err", err)
		}
	}()

	// ---- FIX application + session policy ------------------------------------

	fixStore := fix.NewPgStore(pool)
	cod := fix.NewCoDExecutor(fixStore, fix.NewSessionCanceller(orderSvc),
		fix.NewRedisCoDRateGate(rdb.Client), nil /* audit sink */, slogger, nil)

	// ---- NATS status feeds (Tasks 18.3.3/18.3.15): marketdata.security_status
	// is published by admin instrument maintenance; session.status is
	// bridged from the WS advisory channel in cmd/gateway. Connect is
	// best-effort — nil sources park the loops (35=g still answers
	// snapshot/current-state queries); the Publish* methods remain live.
	var fixNats *excnats.Client
	if nc, nerr := excnats.Connect(ctx, excnats.DefaultConfig(
		cfg.NATS.URLList()), log); nerr == nil {
		fixNats = nc
		defer nc.Close()
	} else {
		log.Warn("fix: nats connect failed — status feeds parked", "err", nerr)
	}
	mdLogf := func(f string, a ...any) { slogger.Warn(fmt.Sprintf(f, a...)) }
	var secSrc fix.StatusSource
	var sessSrc fix.SessionSource
	if fixNats != nil {
		secSrc = fix.SecurityStatusNATS(fixNats, mdLogf)
		sessSrc = fix.SessionLifecycleNATS(fixNats, mdLogf)
	}
	mdKnown := mdKnownSymbol(marketapi.NewPgStore(pool))
	tss, err := fix.NewSessionStatusService(fix.SessionStatusDeps{
		Sender:          fix.QuickFIXSender(),
		SecurityStatus:  secSrc,
		SessionEvents:   sessSrc,
		InstrumentState: redisInstrumentState(rdb),
		VenueState:      redisVenueState(rdb),
		Known:           mdKnown,
	})
	if err != nil {
		return fmt.Errorf("fix tss: %w", err)
	}
	go func() {
		if err := tss.Run(ctx); err != nil {
			log.Warn("fix tss loop exited", "err", err)
		}
	}()

	// ---- Market data (Task 18.3.3): FIX 35=V snapshot+incremental fan-out.
	mds, err := fix.NewMarketDataService(fix.MarketDataDeps{
		Sender:      fix.QuickFIXSender(),
		DeltaSource: mdDeltaSource(log, slogger),
		Known:       mdKnown,
		Entitled:    mdSessionEntitled(fixStore),
	})
	if err != nil {
		return fmt.Errorf("fix mds: %w", err)
	}
	go func() {
		if err := mds.Run(ctx); err != nil {
			log.Warn("fix md loop exited", "err", err)
		}
	}()

	// ---- Mass quoting (Task 18.3.7 wire half + Task 11.3.12 SCOPE_LP) ---
	// mm_programs entitlement + MMP lockout share the gateway's service
	// family; the LP kill-switch gate resolves the session account →
	// liquidity_providers row (lp_accounts, mig 271) and refuses the
	// whole set while the firm CLOB keeps trading.
	mmStore := marketmaking.NewPgStore(pool)
	mmSvc := marketmaking.NewService(mmStore, marketmaking.Options{
		Logger: func(f string, a ...any) { log.Warn(fmt.Sprintf(f, a...)) },
	})
	if err := mmSvc.Load(context.Background()); err != nil {
		// Boot continues: entitlement reads fall through to PG
		// (fail-closed) until the refresher lands a snapshot.
		log.Warn("mm programs initial load failed", "err", err)
	}
	mmSvc.StartRefresher(context.Background(), 60*time.Second,
		func(e error) { log.Warn("mm programs refresh failed", "err", e) })
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
		}).WithLogger(func(f string, a ...any) { log.Warn(fmt.Sprintf(f, a...)) })
	qs, err := fix.NewQuoteService(orderSvc, mmSvc, mmTracker, nil)
	if err != nil {
		return fmt.Errorf("fix quoting: %w", err)
	}
	qs.WithLPGate(mmStore, killResolver).
		WithLogger(func(f string, a ...any) { log.Warn(fmt.Sprintf(f, a...)) })
	// Task 7.3.9 feed seam: accepted mass quotes + 35=Z withdrawals
	// publish onto the "quotes" JetStream stream
	// (quotes.lp.{lp_id}.{symbol-token}) for the marketdata
	// LPBookProducer → lpBook@{lpID}/{symbol} distribution. The sink is
	// buffered/async so a broker stall never sits on quote admission; a
	// missing NATS leaves the seam unbound and quoting fully
	// functional (distribution is best-effort, spec §2.7).
	if fixNats != nil {
		quoteSink := fix.NewJetStreamQuoteSink(fixNats, fix.QuoteSinkConfig{
			Logger: mdLogf,
		})
		qs.WithQuoteEventSink(quoteSink)
		go func() {
			if err := quoteSink.Run(ctx); err != nil &&
				!errors.Is(err, context.Canceled) {
				log.Warn("fix: lp quote sink exited", "err", err)
			}
		}()
	}

	app := fix.NewApp(fix.Options{
		Store:       fixStore,
		Orders:      orderSvc,
		OrderRead:   orderStore,
		DeadMan:     deadman,
		SubAccounts: accounts.NewSubAccountService(pool),
		Log:         log,
		TSS:         tss,
		MDS:         mds,
		QS:          qs,
		CoD:         cod,
	})

	// The drainer exists from boot: AdmitLogon latches the instant Drain
	// starts so reconnecting clients get a Logon refusal rather than a
	// session that is about to Logout (Task 18.3.17 gate half).
	drainer := fix.NewDrainer(
		fix.LiveDrainRegistry{Sessions: app.ActiveSessions},
		fix.DrainConfig{}, log, nil)
	app.SetLogonGate(drainer)

	// ---- QuickFIX acceptor + initiators ---------------------------------------

	gw, err := fix.NewGateway(app, cfg.Fix,
		fix.NewPgMessageStoreFactory(fixStore), log)
	if err != nil {
		return err
	}
	if err := gw.Start(); err != nil {
		return err
	}

	// ---- Engine lifecycle feed: aeron orders_out → canonical consumer →
	//      ExecutionReports for FIX-attributed orders ---------------------------

	feedSrc := fix.NewAeronFeedSource(nil)
	sub, err := aero.AddSubscription(fix.OrdersOutURI, fix.OrdersOutStream,
		feedSrc.Handler(), aeronTimeout)
	if err != nil {
		return fmt.Errorf("aeron subscribe %q stream %d: %w",
			fix.OrdersOutURI, fix.OrdersOutStream, err)
	}
	feedSrc.Sub = sub
	defer sub.Close()

	consumer := orders.NewConsumer(submitter, orderStore, orderSvc.Pending())
	fillH, cancelH := app.ReportHooks(orderStore)
	consumer.WithFillHook(fillH).WithCancelHook(cancelH)
	go fix.NewOutFeed(feedSrc, consumer.HandleFragment).Run(ctx)

	log.Info("fix started",
		"metrics", cfg.Fix.Addr(),
		"acceptor", cfg.Fix.AcceptorAddr(),
		"outward_sessions", len(cfg.Fix.OutwardSessions),
		"sender_comp_id", cfg.Fix.SenderCompID,
		"tls", cfg.Fix.TLSEnabled, "env", cfg.Environment)

	<-ctx.Done()

	// Graceful drain (Task 9.3.23 item 3 + Task 18.3.17 drain half):
	// Drain latches the logon gate (the boot-bound drainer's AdmitLogon
	// now returns ErrDraining → App.FromAdmin rejects new Logons), sends
	// the News(35=B) maintenance advisory, then Logout(35=5) with
	// Text(58)="Scheduled maintenance" to every active session, waits
	// ≤10s for peer Logouts and force-closes stragglers. gw.Stop then
	// performs quickfixgo's own teardown (its Logout fan-out is
	// idempotent for already-drained sessions).
	drainCtx, drainCancel := context.WithTimeout(context.Background(),
		fix.DrainWait+2*time.Second)
	defer drainCancel()
	if err := drainer.Drain(drainCtx); err != nil {
		log.Warn("fix: drain had errors", "err", err)
	}
	gw.Stop(drainCtx)
	log.Info("fix shutting down")
	return nil
}

// --- Task 18.3.3 market-data wiring helpers ---------------------------------

// mdDeltaSource attaches each configured shard's {base}_{shard}_out ring —
// the same shared-memory book-delta feed cmd/marketdata consumes
// (EXC_MARKETDATA_IPC_BASE / _SHARDS / _INSTRUMENTS env contract, plus
// EXC_FIX_MD_SOURCE=none to leave the feed unwired on purpose). A ring
// that will not attach is logged and skipped; when none attach the
// service runs PushDelta-driven only — warned at boot, never silent.
func mdDeltaSource(log *slog.Logger, slogger *slog.Logger) marketdata.DeltaSource {
	switch strings.ToLower(strings.TrimSpace(envOr("EXC_FIX_MD_SOURCE", "ipc"))) {
	case "none", "off":
		return nil
	}
	res := marketdata.MapResolver{}
	for _, pair := range strings.Split(os.Getenv("EXC_MARKETDATA_INSTRUMENTS"), ",") {
		idStr, sym, ok := strings.Cut(strings.TrimSpace(pair), ":")
		if !ok || sym == "" {
			continue
		}
		id, err := strconv.ParseUint(strings.TrimSpace(idStr), 10, 32)
		if err != nil {
			continue
		}
		res[uint32(id)] = sym
	}
	base := envOr("EXC_MARKETDATA_IPC_BASE", ipc.DefaultShmBase)
	var sources []marketdata.DeltaSource
	for _, tok := range strings.Split(envOr("EXC_MARKETDATA_IPC_SHARDS", "0"), ",") {
		tok = strings.TrimSpace(tok)
		if tok == "" {
			continue
		}
		sh, err := strconv.ParseUint(tok, 10, 16)
		if err != nil {
			log.Error("fix md: bad shard token", "token", tok)
			continue
		}
		ch, err := ipc.OpenChannel(base, uint16(sh), ipc.EndpointGateway, false,
			ipc.DefaultRingCapacity, ipc.DefaultRingSlotPayload)
		if err != nil {
			log.Error("fix md: ipc ring attach failed",
				"ring", ipc.OutName(base, uint16(sh)), "err", err)
			continue
		}
		log.Info("fix md: ipc delta source attached",
			"ring", ipc.OutName(base, uint16(sh)))
		sources = append(sources, marketdata.IPCDeltaSource(ch, res, slogger))
	}
	if len(sources) == 0 {
		log.Warn("fix md: no ipc rings attached — 35=V serves snapshots and " +
			"rejects but streams no incrementals until a delta source binds")
		return nil
	}
	return marketdata.FanInDeltaSource(sources...)
}

// mdKnownSymbol validates 35=V symbols against the instruments table —
// canonical "EUR/USD" form; a lookup failure fails closed (unknown →
// 35=Y reject rather than a fabricated subscription).
func mdKnownSymbol(store *marketapi.PgStore) func(string) bool {
	return func(sym string) bool {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_, err := store.InstrumentBySymbol(ctx, sym)
		return err == nil
	}
}

// mdSessionEntitled applies the 046 allowed_instruments list to 35=V
// subscriptions — "" = all instruments; an unresolvable session fails
// closed. Unlike order entitlement this scope also admits drop-copy
// sessions (read-only sessions may still watch the tape).
func mdSessionEntitled(store *fix.PgStore) func(string, string) bool {
	return func(sessionID, symbol string) bool {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		sess, err := store.SessionByID(ctx, sessionID)
		if err != nil || sess == nil {
			return false
		}
		if sess.AllowedInstruments == "" {
			return true
		}
		for _, tok := range strings.Split(sess.AllowedInstruments, ",") {
			if strings.TrimSpace(tok) == symbol {
				return true
			}
		}
		return false
	}
}

func envOr(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

// --- Task 18.3.15 session-status resolvers -----------------------------------

// redisInstrumentState resolves a symbol's current lifecycle state from
// the Phase-15 control-plane feed — instrument:status:{symbol} = plain
// enum word ("ACTIVE", "HALTED", ...). Unresolvable → (false) → 35=g
// snapshot replies 340=6 rather than fabricate a state.
func redisInstrumentState(rdb *redis.Client) func(string) (string, bool) {
	return func(sym string) (string, bool) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		v, err := rdb.Client.Get(ctx, "instrument:status:"+sym).Result()
		if err != nil || v == "" {
			return "", false
		}
		return v, true
	}
}

// redisVenueState aggregates the per-shard session:state:{shard} HASHes
// written by the 24/5 session machine (Phase-15 Task 15.3.7). A
// unanimous shard state is reported; divergence or an unreadable poll
// returns false — the 35=g venue snapshot replies 340=6 rather than
// fabricate a venue state.
func redisVenueState(rdb *redis.Client) func() (string, bool) {
	return func() (string, bool) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		var cursor uint64
		state, seen := "", false
		for {
			keys, next, err := rdb.Client.Scan(ctx, cursor, "session:state:*", 64).Result()
			if err != nil {
				return "", false
			}
			for _, k := range keys {
				v, err := rdb.Client.HGet(ctx, k, "state").Result()
				if err != nil || v == "" {
					continue
				}
				if !seen {
					state, seen = v, true
				} else if v != state {
					return "", false // divergent shards — never fabricate
				}
			}
			if next == 0 {
				break
			}
			cursor = next
		}
		return state, seen
	}
}
