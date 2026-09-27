// Command admin is the admin/monitoring service.
//
// Scaffold scope (Task 1.3.2): load config, init slog, log startup, idle
// until SIGINT/SIGTERM. Admin API and monitoring endpoints are Phase-07.
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
		fmt.Fprintf(os.Stderr, "admin: %v\n", err)
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

	log.Info("admin started", "addr", cfg.Admin.Addr(), "env", cfg.Environment)
	<-ctx.Done()
	log.Info("admin shutting down")
	return nil
}
