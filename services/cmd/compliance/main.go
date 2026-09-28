// Command compliance is the compliance/AML service.
//
// Scaffold scope (Task 1.3.2): load config, init slog, log startup, idle
// until SIGINT/SIGTERM. KYC/surveillance/reporting land in Phases 14/17/21.
// Task 7.3.4: the reserved compliance.port serves /metrics + /healthz
// so Prometheus coverage is uniform before KYC/surveillance land.
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
		fmt.Fprintf(os.Stderr, "compliance: %v\n", err)
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
	observability.NewMetrics(reg, "compliance")
	go func() {
		if err := observability.ServeMetrics(ctx, cfg.Compliance.Addr(), reg, "compliance"); err != nil {
			log.Error("compliance: metrics listener died", "err", err)
		}
	}()

	log.Info("compliance started", "addr", cfg.Compliance.Addr(), "env", cfg.Environment)
	<-ctx.Done()
	log.Info("compliance shutting down")
	return nil
}
