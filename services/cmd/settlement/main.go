// Command settlement is the FX settlement service.
//
// Hosts the mode-aware dispatch daemon (Phase-03 instruction lifecycle +
// Phase-24 Task 24.3.20): every EXC_SETTLE_DISPATCH_INTERVAL (default 5m)
// the GrossNetService releases QUEUED_FOR_NEXT_CYCLE legs whose rolled
// date arrived, re-rolls same-day legs past a closed rail cut-off, and
// dispatches due PENDING settlement legs — GROSS individually, NET as
// one payment per batch. Claim-before-send: payloads persist before the
// dispatcher runs, so a crash leaves a recoverable row, never a
// duplicate payment. The rail send seam is the documented Phase-11 stub
// (NullDispatcher) until banking-rail connectors land.
// Task 7.3.4: settlement.port serves /metrics + /healthz.
//
// Fail closed (§2.7): refuses to start without EXC_SENDER_BIC — a
// settlement dispatcher that cannot sign payments is a wiring defect,
// not a degraded mode.
package main

import (
	"fmt"
	"os"
	"strings"
	"time"

	"exchange/internal/config"
	"exchange/internal/db"
	"exchange/internal/observability"
	"exchange/internal/settlement"
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

	bic := strings.TrimSpace(os.Getenv("EXC_SENDER_BIC"))
	if bic == "" {
		return fmt.Errorf("EXC_SENDER_BIC unset — settlement dispatch cannot sign payments (fail closed)")
	}
	interval := 5 * time.Minute
	if v := strings.TrimSpace(os.Getenv("EXC_SETTLE_DISPATCH_INTERVAL")); v != "" {
		d, perr := time.ParseDuration(v)
		if perr != nil || d <= 0 {
			return fmt.Errorf("EXC_SETTLE_DISPATCH_INTERVAL %q: %w", v, perr)
		}
		interval = d
	}

	pool, err := db.NewPool(ctx, cfg.Postgres.DSN, cfg.Postgres.MaxConns)
	if err != nil {
		return fmt.Errorf("postgres: %w", err)
	}
	defer pool.Close()

	// Rail cut-off enforcement needs the holiday calendar; absent it the
	// gate is skipped and the daemon logs once — generation-time
	// enforcement (gateway) still applies.
	var cutoff *settlement.RailCutoffService
	cal, calErr := settlement.LoadCalendar(ctx, pool)
	if calErr != nil {
		log.Warn("holiday calendar unavailable — dispatch-time rail cut-off " +
			"gate disabled (generate-time gate still applies)")
	} else {
		rc, rerr := settlement.NewRailCutoffService(
			settlement.NewPgxRailScheduleStore(pool), cal, nil)
		if rerr != nil {
			return fmt.Errorf("rail cutoff: %w", rerr)
		}
		cutoff = rc
	}

	grossSvc, err := settlement.NewGrossNetService(
		settlement.NewPgxGrossNetStore(pool),
		settlement.GrossNetOptions{
			SenderBIC:  bic,
			Dispatcher: &settlement.NullDispatcher{},
			Cutoff:     cutoff,
		})
	if err != nil {
		return fmt.Errorf("gross-net service: %w", err)
	}

	reg := observability.New()
	observability.NewMetrics(reg, "settlement")
	go func() {
		if err := observability.ServeMetrics(ctx, cfg.Settlement.Addr(), reg, "settlement"); err != nil {
			log.Error("settlement: metrics listener died", "err", err)
		}
	}()

	log.Info("settlement started", "addr", cfg.Settlement.Addr(),
		"env", cfg.Environment, "dispatch_interval", interval.String(),
		"cutoff_gate", cutoff != nil)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			log.Info("settlement shutting down")
			return nil
		case t := <-ticker.C:
			rep, derr := grossSvc.DispatchDue(ctx, t)
			if derr != nil {
				log.Error("settlement: dispatch pass failed", "err", derr)
				continue
			}
			if rep.Due > 0 || rep.Released > 0 {
				log.Info("settlement dispatch pass",
					"due", rep.Due, "released", rep.Released, "queued", rep.Queued,
					"gross_claimed", rep.GrossClaimed, "net_dispatched", rep.NetDispatched,
					"netted", rep.Netted, "errors", len(rep.Errors)+len(rep.BatchErrors))
			}
			for _, e := range rep.Errors {
				log.Warn("settlement leg dispatch error",
					"instruction_id", e.InstructionID, "err", e.Err)
			}
			for _, e := range rep.BatchErrors {
				log.Warn("settlement batch dispatch error",
					"account", e.Batch.AccountID, "counterparty", e.Batch.CounterpartyID,
					"currency", e.Batch.Currency, "err", e.Err)
			}
		}
	}
}
