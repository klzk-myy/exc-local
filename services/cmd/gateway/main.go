// Command gateway is the REST API entrypoint (order-gateway).
//
// Scaffold scope (Task 1.3.2): load config, init slog, bind the configured
// port, serve a stub /health. Real routes are registered in Phase-05.
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
	"exchange/internal/middleware"
	"exchange/internal/redis"
	"exchange/internal/utils"
	"exchange/pkg/logging"
)

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
		rdb.Close()
		return fmt.Errorf("shard map: %w", err)
	}
	defer rdb.Close() // Phase-02+ keeps this client for routing/health use
	log.Info("shard map loaded",
		"source", shardSrc.String(),
		"static_symbols", len(shardMap.Entries()),
		"elastic_base", shardMap.ElasticBase(),
		"elastic_count", shardMap.ElasticCount())

	mux := http.NewServeMux()
	mux.HandleFunc("/health", api.Health)
	srv := &http.Server{
		Addr:              cfg.Gateway.Addr(),
		Handler:           middleware.Logging(log, mux),
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
