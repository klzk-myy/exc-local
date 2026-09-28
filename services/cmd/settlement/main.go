// Command settlement is the FX settlement service.
//
// Scaffold scope (Task 1.3.2): load config, init slog, log startup, idle
// until SIGINT/SIGTERM. Settlement cycles/value-date logic are Phase-03.
// Task 7.3.4: the reserved settlement.port serves /metrics + /healthz
// so Prometheus coverage is uniform before settlement cycles exist.
package main

import (
	"fmt"
	"os"

	"exchange/internal/config"
	"exchange/internal/observability"
	"exchange/internal/utils"
	"exchange/pkg/logging"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "settlement: %v\n", err)
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
	observability.NewMetrics(reg, "settlement")
	go func() {
		if err := observability.ServeMetrics(ctx, cfg.Settlement.Addr(), reg, "settlement"); err != nil {
			log.Error("settlement: metrics listener died", "err", err)
		}
	}()

	log.Info("settlement started", "addr", cfg.Settlement.Addr(), "env", cfg.Environment)
	<-ctx.Done()
	log.Info("settlement shutting down")
	return nil
}
