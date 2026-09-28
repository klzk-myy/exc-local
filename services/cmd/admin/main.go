// Command admin is the admin/monitoring service (Phase-07).
//
// Current scope (Tasks 7.3.4/7.3.8/7.3.10): it serves /metrics on its
// configured address, samples the Aeron media-driver CnC counters file
// (driver status, NAK/loss, subscriber-position lag), mirrors JetStream
// stream/consumer gauges and bridge.health heartbeat staleness, runs the
// alert-rule evaluator (L0 delta → P0, sustained L1 → P1, L2 spike >5%/1m
// → P2, subscriber lag >1000 → P2, bridge buffer >10000 → P1, consumer
// pending >50000 → P1, heartbeat stale >15s → P1, error-rate anomaly →
// P2) and serves the DLQ admin surface (GET /api/v1/admin/dlq plus the
// replay/discard POSTs the route registry has not yet declared — those
// mount here only, pending Phase-05 route rows).
//
// RBAC on the admin endpoints lands with Task 7.3.1; the listener binds
// cfg.Admin.Addr() (127.0.0.1-ish in production topology behind HAProxy).
package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"time"

	gonats "github.com/nats-io/nats.go"

	"exchange/internal/config"
	excnats "exchange/internal/nats"
	"exchange/internal/observability"
	"exchange/internal/utils"
	"exchange/pkg/logging"
)

// natsPublisher adapts internal/nats.Client to the alert Publisher seam.
type natsPublisher struct{ c *excnats.Client }

func (p natsPublisher) Publish(ctx context.Context, subject string, payload []byte) error {
	_, err := p.c.JetStream().Publish(ctx, subject, payload)
	return err
}

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

	reg := observability.New()
	met := observability.NewMetrics(reg, "admin")

	// NATS (fail-operational at boot — the service degrades to
	// Aeron-only monitoring and keeps retrying implicitly through the
	// monitor's per-tick probe errors).
	var nc *excnats.Client
	var dlqStore observability.Store
	var natsMon *observability.NATSMonitor
	natsCtx, natsCancel := context.WithTimeout(ctx, 3*time.Second)
	if c, nerr := excnats.Connect(natsCtx, excnats.DefaultConfig(cfg.NATS.URLList()), log); nerr == nil {
		nc = c
		natsMon = observability.NewNATSMonitor(reg, c, log)
		go natsMon.Run(ctx)
		// DLQ stream provisioning is an operator action (same convention
		// as natsctl init / canonical streams). Open is a no-create bind:
		// a missing stream yields a 503 DLQ surface, not a boot failure.
		if s, derr := observability.OpenJetStreamDLQ(natsCtx, c.JetStream()); derr == nil {
			dlqStore = s
		} else {
			log.Warn("admin: DLQ stream not provisioned — surface returns 503 until `natsctl dlq init`", "err", derr)
		}
	} else {
		log.Warn("admin: nats unavailable — JetStream monitoring/DLQ degraded", "err", nerr)
	}
	natsCancel()
	if nc != nil {
		defer nc.Close()
	}

	// Bridge heartbeat liveness (bridge.health.<shard>, 5s cadence).
	var hbWatcher *observability.BridgeHeartbeatWatcher
	if nc != nil {
		w, herr := observability.NewBridgeHeartbeatWatcher(reg,
			func(subject string, cb func(string, []byte)) (func(), error) {
				sub, err := nc.Conn().Subscribe(subject,
					func(m *gonats.Msg) { cb(m.Subject, m.Data) })
				if err != nil {
					return nil, err
				}
				return func() { _ = sub.Unsubscribe() }, nil
			}, log)
		if herr != nil {
			log.Warn("admin: heartbeat subscribe failed", "err", herr)
		} else {
			hbWatcher = w
			defer w.Close()
		}
	}

	// Aeron media-driver counters (host-local; absent on pure-API nodes).
	cncMon := observability.NewCnCMonitor(reg, os.Getenv("EXC_AERON_DIR"), log)
	go cncMon.Run(ctx)

	// Alert evaluator — canonical Task 7.3.8/7.3.10 rule set.
	var sink observability.Sink = observability.LogSink{Log: log}
	if nc != nil {
		sink = observability.FanoutSink{
			observability.PublisherSink{Pub: natsPublisher{c: nc}},
			observability.LogSink{Log: log},
		}
	}
	ev := observability.NewEvaluator(sink, log, observability.WithAlertMetrics(met))
	srcs := observability.StandardSources{
		L0Errors:           func() float64 { return met.ErrorCount(observability.TierL0) },
		L1Errors:           func() float64 { return met.ErrorCount(observability.TierL1) },
		L2Errors:           func() float64 { return met.ErrorCount(observability.TierL2) },
		TotalRequests:      nil, // admin service serves little HTTP — ratio stays inert
		AeronSubscriberLag: cncMon.SubscriberLagValue,
	}
	if hbWatcher != nil {
		srcs.BridgeBufferDepth = hbWatcher.MaxBufferDepth
		srcs.BridgeHeartbeatAge = hbWatcher.MaxHeartbeatAge
	}
	if natsMon != nil {
		srcs.NATSConsumerPending = natsMon.MaxConsumerPending
	}
	observability.AddStandardRules(ev, srcs)
	go ev.Run(ctx)

	mux := http.NewServeMux()
	mux.Handle("/metrics", reg.Handler())
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"status":"ok","nats_connected":%v}`,
			nc != nil && nc.Connected())
	})
	mux.HandleFunc("GET /api/v1/admin/dlq", observability.DLQHandler(dlqStore))
	mux.HandleFunc("POST /api/v1/admin/dlq/replay",
		observability.DLQActionHandler(dlqStore, "replay"))
	mux.HandleFunc("POST /api/v1/admin/dlq/discard",
		observability.DLQActionHandler(dlqStore, "discard"))

	srv := &http.Server{
		Addr:              cfg.Admin.Addr(),
		Handler:           met.HTTPMiddleware(mux),
		ReadHeaderTimeout: 5 * time.Second,
	}
	ln, err := net.Listen("tcp", srv.Addr)
	if err != nil {
		return fmt.Errorf("admin: bind %s: %w", srv.Addr, err)
	}
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("admin: listener died", "err", err)
		}
	}()

	log.Info("admin started", "addr", ln.Addr().String(), "env", cfg.Environment,
		"nats", nc != nil)
	<-ctx.Done()

	sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(sctx)
	log.Info("admin shutting down")
	return nil
}
