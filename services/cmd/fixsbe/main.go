// Command fixsbe is the SBE binary order-entry gateway (Phase-3 Task 4 /
// Phase-18 Tasks 18.3.8/18.3.17, spec §9.5, §24 #284/#289).
//
// Wiring: PostgreSQL (fixsbe_sessions session store, fixsbe_schema_registry
// schema lifecycle, orders read model), Redis (shard map, kill-switch,
// breakers, OTR), Aeron aeron:ipc (orders_in publication multi-publisher
// submitter + orders_out lifecycle feed), TLS 1.3 listener serving the
// sniffing Mux — SBE frames to ConnServer (Ed25519 session-key handshake
// bound to SNI + TLS keying material); tag-value on this port is refused
// (tag-value order entry lives on cmd/fix's acceptor).
//
// fixsbe.enabled=false → metrics/health only. TLS cert+key are mandatory
// when enabled — the session proof exports channel keying material, so no
// plaintext mode exists (config.Validate enforces).
package main

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"sync"
	"time"

	"exchange/internal/admin"
	"exchange/internal/auth"
	"exchange/internal/compliance"
	"exchange/internal/config"
	"exchange/internal/db"
	"exchange/internal/fix"
	"exchange/internal/fixsbe"
	aeronclient "exchange/internal/ipc/aeron"
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
		fmt.Fprintf(os.Stderr, "fixsbe: %v\n", err)
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

	reg := observability.New()
	observability.NewMetrics(reg, "fixsbe")
	go func() {
		if err := observability.ServeMetrics(ctx, cfg.FixSBE.Addr(), reg, "fixsbe"); err != nil {
			log.Error("fixsbe: metrics listener died", "err", err)
		}
	}()

	if !cfg.FixSBE.Enabled {
		log.Info("fixsbe started (listener disabled — metrics/health only)",
			"addr", cfg.FixSBE.Addr(), "env", cfg.Environment)
		<-ctx.Done()
		log.Info("fixsbe shutting down")
		return nil
	}

	// ---- Shared infrastructure ----------------------------------------------

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

	// ---- Order pipeline (canonical orders.Service — admission-gate parity
	//      with cmd/fix / cmd/gateway: nil gates fail CLOSED) ------------------

	orderStore := orders.NewPgStore(pool)
	riskLimits := risk.NewLimitsService(risk.NewPgStore(pool), nil, nil)
	if err := riskLimits.Load(context.Background()); err != nil {
		log.Warn("risk limits initial load failed (fails closed per request)", "err", err)
	}
	riskLimits.StartRefresher(context.Background(), 60*time.Second,
		func(e error) { log.Warn("risk limits refresh failed", "err", e) })
	killResolver := admin.NewKillSwitchResolver(rdb, cfg.Environment)

	aero, err := aeronclient.Connect(cfg.FixSBE.AeronDir,
		uint64(cfg.FixSBE.AeronDriverTimeoutMS))
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
		log.Warn("circuit-breaker state load failed", "err", err)
	}
	go breakerSvc.Run(ctx, time.Second)

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
		KillSwitch: killResolver,
		Breakers:   breakerSvc,
		Product:    catSvc,
		Otr:        otrMon,
		// §5.4 DAY orders expire at the weekly session close (Fri 22:00
		// UTC, §6.7); spec §27 R8 caps every resting order at 90d. Same
		// binding as cmd/gateway — without it SBE-submitted DAY orders
		// carry no gtd_expiry and the engine rejects ORDER_INVALID.
		DayExpiry: func(now time.Time) *time.Time {
			if t := admin.NextSessionClose(now); !t.IsZero() {
				return &t
			}
			return nil
		},
	})
	if err != nil {
		return fmt.Errorf("order service: %w", err)
	}

	// ---- Engine lifecycle feed: orders_out → canonical consumer (read-model
	//      projection + pending-confirm resolution for Submit acks). Aeron
	//      subscriptions are pub/sub — this reader coexists with cmd/fix and
	//      cmd/gateway consumers on the same stream.
	outSub, err := aero.AddSubscription(fix.OrdersOutURI, fix.OrdersOutStream,
		orders.NewConsumer(submitter, orderStore, orderSvc.Pending()).HandleFragment,
		aeronTimeout)
	if err != nil {
		return fmt.Errorf("aeron subscribe %q stream %d: %w",
			fix.OrdersOutURI, fix.OrdersOutStream, err)
	}
	defer outSub.Close()
	go func() {
		for ctx.Err() == nil {
			n := outSub.Poll(10)
			if n < 0 {
				log.Warn("fixsbe: orders_out poll error")
				return
			}
			if n == 0 {
				// Idle wait. Without it this goroutine busy-spins a full
				// core whenever no orders_out fragments are pending — the
				// same defect fixed in cmd/xshardrelay. Masked today only
				// because fixsbe.enabled=false returns before reaching this
				// loop; it would surface the moment SBE order entry is
				// switched on.
				select {
				case <-ctx.Done():
					return
				case <-time.After(100 * time.Microsecond):
				}
			}
		}
	}()

	// ---- Session store + schema registry (migration 228) --------------------

	sessions := fixsbe.NewPgSessionStore(pool)
	registry, err := fixsbe.LoadRegistry(context.Background(), pool)
	if err != nil {
		return fmt.Errorf("sbe schema registry: %w", err)
	}

	// ---- TLS 1.3 listener + SNI/keying-material binding ----------------------

	cert, err := tls.LoadX509KeyPair(cfg.FixSBE.TLSCert, cfg.FixSBE.TLSKey)
	if err != nil {
		return fmt.Errorf("tls keypair: %w", err)
	}
	tlsCfg := &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS13,
	}
	if cfg.FixSBE.MTLSRequired {
		caPEM, err := os.ReadFile(cfg.FixSBE.TLSCA)
		if err != nil {
			return fmt.Errorf("tls ca: %w", err)
		}
		caPool := x509.NewCertPool()
		if !caPool.AppendCertsFromPEM(caPEM) {
			return errors.New("fixsbe.tls_ca: no parseable CA certificates")
		}
		tlsCfg.ClientAuth = tls.RequireAndVerifyClientCert
		tlsCfg.ClientCAs = caPool
		log.Info("fixsbe: mtls client-cert gate armed")
	}
	sni := fixsbe.NewSNIBinder()
	sni.WrapGetConfigForClient(tlsCfg)

	// ---- Gateway + negotiator + conn server ----------------------------------

	gw := &fixsbe.Gateway{
		Orders:      orderSvc,
		Instruments: orderStore,
		ClientIDs:   orderStore,
	}
	neg := &fixsbe.Negotiator{
		Registry:    registry,
		Sessions:    sessions,
		Environment: cfg.Environment,
	}
	srv := &fixsbe.ConnServer{
		Gateway:    gw,
		Negotiator: neg,
		SNI:        sni.ServerName,
		KeyingMat: func(c net.Conn) ([]byte, error) {
			tc, ok := c.(*tls.Conn)
			if !ok {
				return nil, errors.New("fixsbe: non-TLS conn reached keying-material export")
			}
			return fixsbe.TLSKeyingMaterial(tc)
		},
	}

	// Drain registry — live sessions register here so a maintenance
	// Drainer can broadcast News advisories (Task 18.3.17).
	var (
		drainMu sync.Mutex
		targets = map[string]fixsbe.DrainTarget{}
	)
	register := func(t fixsbe.DrainTarget) {
		drainMu.Lock()
		targets[t.SessionID()] = t
		drainMu.Unlock()
	}
	drainer := &fixsbe.Drainer{
		Source: func() []fixsbe.DrainTarget {
			drainMu.Lock()
			defer drainMu.Unlock()
			out := make([]fixsbe.DrainTarget, 0, len(targets))
			for _, t := range targets {
				out = append(out, t)
			}
			return out
		},
	}
	_ = drainer // broadcast loop starts when an operator opens a window

	mux := &fixsbe.Mux{
		OnSBE: func(ctx context.Context, conn net.Conn) error {
			defer sni.Forget(conn)
			var ids []string
			err := srv.Serve(ctx, conn, func(t fixsbe.DrainTarget) {
				register(t)
				ids = append(ids, t.SessionID())
			})
			drainMu.Lock()
			for _, id := range ids {
				delete(targets, id)
			}
			drainMu.Unlock()
			return err
		},
		// OnTagValue nil → tag-value refuses on this port; cmd/fix owns it.
	}

	ln, err := tls.Listen("tcp", cfg.FixSBE.ListenAddr(), tlsCfg)
	if err != nil {
		return fmt.Errorf("listen %s: %w", cfg.FixSBE.ListenAddr(), err)
	}
	defer ln.Close()

	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()

	log.Info("fixsbe started",
		"metrics", cfg.FixSBE.Addr(),
		"listen", cfg.FixSBE.ListenAddr(),
		"mtls", cfg.FixSBE.MTLSRequired,
		"env", cfg.Environment)

	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				log.Info("fixsbe shutting down")
				return nil
			}
			log.Warn("fixsbe: accept failed", "err", err)
			continue
		}
		go func() {
			if err := mux.Serve(context.Background(), conn); err != nil &&
				!errors.Is(err, net.ErrClosed) {
				slog.Debug("fixsbe: conn ended", "err", err)
			}
		}()
	}
}
