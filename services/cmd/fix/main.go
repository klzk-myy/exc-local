// Command fix is the FIX protocol gateway (fix-gateway).
//
// Scaffold scope (Task 1.3.2): load config, init slog, log startup, idle
// until SIGINT/SIGTERM. QuickFIX session acceptors land in Phase-18.
// Task 7.3.4: the reserved fix.port already serves /metrics + /healthz
// so Prometheus coverage is uniform before session acceptors exist.
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

	ctx, stop := utils.SignalContext()
	defer stop()

	reg := observability.New()
	observability.NewMetrics(reg, "fix")
	go func() {
		if err := observability.ServeMetrics(ctx, cfg.Fix.Addr(), reg, "fix"); err != nil {
			log.Error("fix: metrics listener died", "err", err)
		}
	}()

	log.Info("fix started", "addr", cfg.Fix.Addr(), "env", cfg.Environment)
	<-ctx.Done()
	// Task 9.3.23 item 3 (FIX logout sequence: send 35=5 with
	// Text(58)="Scheduled maintenance", wait ≤10s for peer Logouts) is a
	// no-op until the Phase-18 QuickFIX acceptor exists — there are no
	// sessions to log out. The sequencing hook belongs here, ahead of
	// process exit, once internal/fixgw lands.
	log.Info("fix shutting down")
	return nil
}
