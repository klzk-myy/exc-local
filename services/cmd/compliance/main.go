// Command compliance is the compliance/AML service.
//
// Phase-17 Task 17.3.3: the market-abuse signal engine consumes the L3
// order-level stream and persists deduplicated signals to
// surveillance_signals (migration 029) — Phase-21 consumes that table
// for case management. Source: the JetStream "l3" stream republished by
// the bridge (durable consumer "compliance-l3"); absent NATS/Postgres
// the service still boots (fail-loud, spec §2.7) — the pump idles.
//
// Task 7.3.4: the reserved compliance.port serves /metrics + /healthz.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"exchange/internal/config"
	"exchange/internal/db"
	"exchange/internal/marketapi"
	"exchange/internal/marketdata"
	excnats "exchange/internal/nats"
	"exchange/internal/observability"
	"exchange/internal/settlement"
	"exchange/internal/surveillance"
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
	metrics := observability.NewMetrics(reg, "compliance")
	go func() {
		if err := observability.ServeMetrics(ctx, cfg.Compliance.Addr(), reg, "compliance"); err != nil {
			log.Error("compliance: metrics listener died", "err", err)
		}
	}()

	// ---- Phase-17 Task 17.3.3 — L3-driven surveillance signals ----
	// The pump needs Postgres (signal persistence + instrument map) and
	// NATS (the l3 stream). Either absent → idle pump, loud log; the
	// metrics/health surface stays up regardless.
	go startSurveillance(ctx, cfg, reg, metrics, log)

	log.Info("compliance started", "addr", cfg.Compliance.Addr(), "env", cfg.Environment)
	<-ctx.Done()
	log.Info("compliance shutting down")
	return nil
}

// startSurveillance wires the L3 → signals pump. Transient dependency
// failures log and the pump exits — the service-level contract is
// emit-only (a restarted pod resumes the durable consumer where it
// left off; dedup keys make re-delivery idempotent).
func startSurveillance(ctx context.Context, cfg *config.Config,
	reg *observability.Registry, metrics *observability.Metrics,
	log *slog.Logger) {
	if strings.EqualFold(os.Getenv("EXC_SURVEILLANCE_DISABLE"), "1") {
		log.Warn("compliance: surveillance signal pump disabled (EXC_SURVEILLANCE_DISABLE)")
		return
	}
	pool, err := db.NewPool(ctx, cfg.Postgres.DSN, cfg.Postgres.MaxConns)
	if err != nil {
		log.Error("compliance: postgres unavailable — surveillance pump idle",
			"err", err)
		return
	}
	defer pool.Close()

	urls := cfg.NATS.URLList()
	if len(urls) == 0 {
		log.Error("compliance: nats urls not configured — surveillance pump idle")
		return
	}
	nc, err := excnats.Connect(ctx, excnats.DefaultConfig(urls), log)
	if err != nil {
		log.Error("compliance: nats connect failed — surveillance pump idle",
			"err", err)
		return
	}
	defer nc.Close()

	// Subject→symbol map from the instruments table: l3.{shard}.{symbol}
	// tokens carry '-' for '/', so the reverse map is authoritative
	// (the single-dash fallback covers instruments added later).
	symbols := marketdata.SubjectSymbol{}
	if insts, ierr := marketapi.NewPgStore(pool).ListInstruments(ctx); ierr == nil {
		for _, in := range insts {
			symbols[strings.ReplaceAll(in.Symbol, "/", "-")] = in.Symbol
		}
	} else {
		log.Warn("compliance: instrument list unavailable — subject "+
			"symbols resolve via fallback only", "err", ierr)
	}

	src := marketdata.NewJetStreamL3Source(
		marketdata.JetStreamMsgSource(nc, "l3", "compliance-l3", "l3.>", log),
		symbols, log)
	eng, err := surveillance.NewEngine(surveillance.Config{
		Logger:          log,
		AnnounceWindows: csvEnv("EXC_SURVEILLANCE_ANNOUNCE_WINDOWS"),
	}, surveillance.NewPgSink(pool))
	if err != nil {
		log.Error("compliance: surveillance engine build failed", "err", err)
		return
	}

	var lastLag atomic.Uint64

	// §14.9/§24 #392 — SURVEILLANCE_LAG_WARNING: poll the durable
	// consumer's lag; >10,000 sustained raises a P2 ops alert and fires
	// the analysis-worker auto-scale hook (ops.autoscale subject). The
	// degraded-detection policy under backlog is "detect late, never
	// drop": the durable consumer replays the backlog rather than losing
	// coverage; the detection-latency gauge is the SLA surface.
	reg.GaugeFunc("exchange_surveillance_l3_consumer_lag",
		"JetStream l3/compliance-l3 consumer lag (pending + in-flight).",
		func() float64 { return float64(lastLag.Load()) })
	reg.GaugeFunc("exchange_surveillance_detection_latency_seconds",
		"Latest L3 event-time → detection latency (§24 #392 SLA).",
		func() float64 { return eng.DetectionLatency().Seconds() })
	reg.CounterFunc("exchange_surveillance_events_total",
		"L3 events folded into the detectors.",
		func() float64 { return float64(eng.AppliedCount()) })

	natsPub := settlement.NatsPublisher{JS: nc.JetStream()}
	eval := observability.NewEvaluator(
		observability.FanoutSink{
			observability.PublisherSink{Pub: natsPub},
			observability.LogSink{Log: log},
		}, log, observability.WithAlertMetrics(metrics))
	lagProbe := func(pctx context.Context) (uint64, error) {
		st, err := nc.JetStream().Stream(pctx, "l3")
		if err != nil {
			return lastLag.Load(), err
		}
		cons, err := st.Consumer(pctx, "compliance-l3")
		if err != nil {
			return lastLag.Load(), err
		}
		ci, err := cons.Info(pctx)
		if err != nil || ci == nil {
			return lastLag.Load(), err
		}
		lag := ci.NumPending + uint64(max(ci.NumAckPending, 0))
		lastLag.Store(lag)
		return lag, nil
	}
	go surveillance.WatchLag(ctx, lagProbe, eval, natsPub, 15*time.Second)

	log.Info("compliance: surveillance pump consuming l3 stream")
	if err := eng.Run(ctx, src); err != nil && !errors.Is(err, context.Canceled) {
		log.Error("compliance: surveillance pump exited", "err", err)
	}
}

// csvEnv parses a comma-separated env list ("13:30-13:45,21:55-22:00").
func csvEnv(key string) []string {
	var out []string
	for _, t := range strings.Split(os.Getenv(key), ",") {
		if t = strings.TrimSpace(t); t != "" {
			out = append(out, t)
		}
	}
	return out
}
