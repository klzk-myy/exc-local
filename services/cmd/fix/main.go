// Command fix is the FIX protocol gateway (fix-gateway).
//
// Scaffold scope (Task 1.3.2): load config, init slog, log startup, idle
// until SIGINT/SIGTERM. QuickFIX session acceptors land in Phase-18.
package main

import (
	"fmt"
	"os"

	"exchange/internal/config"
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

	log.Info("fix started", "addr", cfg.Fix.Addr(), "env", cfg.Environment)
	<-ctx.Done()
	log.Info("fix shutting down")
	return nil
}
