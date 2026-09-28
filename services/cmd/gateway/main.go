// Command gateway is the REST API entrypoint (order-gateway).
//
// Phase-05 wiring (Tasks 5.3.7/5.3.21/5.3.41): the route registry mounts
// every declared endpoint (stubbed 501 until its owning phase lands), the
// error-code registry gates emission, and all responses carry the RFC 7807
// envelope + X-Request-ID correlation.
package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"time"

	"exchange/internal/api"
	"exchange/internal/config"
	"exchange/internal/db"
	"exchange/internal/errs"
	"exchange/internal/gateway"
	"exchange/internal/middleware"
	"exchange/internal/ratelimit"
	"exchange/internal/redis"
	"exchange/internal/settlement"
	"exchange/internal/utils"
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

	// Task 5.3.7/5.3.21: mount the route + error-code registries.
	// MountSeedLive wires the live handlers this binary hosts; an
	// unwired live row mounts a fail-closed 503, and any unregistered
	// emitted code fails startup — the registry contract is a startup
	// gate, not a runtime nicety.
	router := gateway.NewRouter(mux, errs.Default)
	banList, banGet, banPut, banDel, banAudit := api.AdminIPBans(limiter.Backend(), banAdmin)
	alPut, alDel := api.AdminIPAllowlist(banAdmin)
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
		// R9 health schema (Task 5.3.28 item 6): /health/live is always-ok
		// liveness; /health/ready reports mode/shards/version/timestamp.
		"GET /health/live": http.HandlerFunc(api.Health),
		"GET /health/ready": http.HandlerFunc(api.HealthFull(
			func(ctx context.Context) (string, error) {
				st, err := rdb.GetDegradationMode(ctx)
				return string(st.Mode), err
			}, nil, apiVersion)),
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
		Handler: middleware.DegradationModeHeader(
			rdb, gateway.RequestID(router.Recover(
				func(f string, a ...any) { log.Error(fmt.Sprintf(f, a...)) },
				middleware.Logging(log,
					middleware.RateLimit(limiter, middleware.RateLimitOptions{
						ResolveTier: tierResolver,
						Emit:        router.WriteError,
					})(
						middleware.APIVersion(middleware.VersionConfig{
							Versions: map[int]string{1: apiVersion},
							Emit:     router.WriteError,
						})(mux)))))),
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
