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
