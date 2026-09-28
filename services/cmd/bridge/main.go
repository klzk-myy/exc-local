// Command bridge is the Aeron-to-NATS Bridge Service (Task 3.3.10, spec
// §2.3.1, §24 #210): one instance per matching-engine shard, colocated
// with the engine, republishing Aeron IPC events to JetStream subjects
// "{stream}.{shard_id}.{symbol}".
//
// Boot sequence: load shared config (logging, nats urls) + the `bridge:`
// section, connect JetStream, attach the Aeron subscription on the
// engine's outbound channel, then run the drain loop. Stream provisioning
// is an operator action (`natsctl init`), not a boot side effect — an
// un-provisioned stream surfaces as publish errors and the bounded buffer
// absorbs the backlog.
//
// The engine is never blocked: on NATS outage the bridge buffers up to
// bridge.buffer_size events (default 100k) and replays in order on
// reconnect. Oldest events are evicted past the bound and counted.
package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"exchange/internal/bridge"
	"exchange/internal/config"
	ipcaeron "exchange/internal/ipc/aeron"
	excnats "exchange/internal/nats"
	"exchange/internal/utils"
	"exchange/pkg/logging"
)

// natsPublisher adapts internal/nats.Client to bridge.Publisher.
type natsPublisher struct {
	c *excnats.Client
}

func (p *natsPublisher) PublishEvent(ctx context.Context, subject, msgID string, payload []byte) error {
	_, err := p.c.JetStream().Publish(ctx, subject, payload,
		jetstream.WithMsgID(msgID))
	return err
}

func (p *natsPublisher) PublishHeartbeat(subject string, payload []byte) error {
	return p.c.Conn().Publish(subject, payload)
}

func (p *natsPublisher) Connected() bool { return p.c.Connected() }

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "bridge: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	bcfg, err := bridge.Load()
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

	// NATS JetStream — cold-path backbone.
	urls := cfg.NATS.URLList()
	if bcfg.NATSURLs != "" {
		urls = splitURLs(bcfg.NATSURLs)
	}
	ncfg := excnats.DefaultConfig(urls)
	ncfg.Name = fmt.Sprintf("bridge-shard-%d", bcfg.ShardID)
	nats, err := excnats.Connect(ctx, ncfg, log)
	if err != nil {
		return err
	}
	defer nats.Close()

	res, err := bcfg.Resolver()
	if err != nil {
		return err
	}
	b, err := bridge.New(*bcfg, &natsPublisher{c: nats}, res, log)
	if err != nil {
		return err
	}

	// Aeron — subscribe to this shard's outbound engine stream.
	aero, err := ipcaeron.Connect(bcfg.AeronDir, uint64(bcfg.AeronDriverTimeout/time.Millisecond))
	if err != nil {
		return fmt.Errorf("bridge: aeron connect: %w", err)
	}
	defer aero.Close()

	sub, err := aero.AddSubscription(bcfg.AeronChannel(), bcfg.AeronStreamID,
		b.HandleFragment, 5*time.Second)
	if err != nil {
		return fmt.Errorf("bridge: aeron subscribe %q stream %d: %w",
			bcfg.AeronChannel(), bcfg.AeronStreamID, err)
	}
	defer sub.Close()

	// Metrics + health HTTP endpoint.
	if bcfg.MetricsAddr != "" {
		mux := http.NewServeMux()
		mux.Handle("/metrics", b.Metrics().Handler())
		mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"status":"ok","shard":%d,"nats_connected":%v,"buffer_depth":%d}`,
				bcfg.ShardID, nats.Connected(), b.Snapshot().BufferDepth)
		})
		srv := &http.Server{Addr: bcfg.MetricsAddr, Handler: mux,
			ReadHeaderTimeout: 5 * time.Second}
		go func() {
			<-ctx.Done()
			sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = srv.Shutdown(sctx)
		}()
		go func() {
			if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Error("bridge: metrics listener died", "err", err)
			}
		}()
		log.Info("bridge metrics endpoint", "addr", bcfg.MetricsAddr)
	}

	// Aeron poll loop — single goroutine per Subscription (see
	// internal/ipc/aeron doc). Idle strategy: short sleep — cold path, the
	// poll thread does not need to burn a core.
	go func() {
		for ctx.Err() == nil {
			n := sub.Poll(10)
			// Task 7.3.8: Aeron-side counters (fragment delivery rate,
			// poll errors, subscription image presence).
			b.Metrics().ObserveAeronPoll(n, sub.IsConnected())
			if n < 0 {
				log.Error("bridge: aeron poll error", "shard", bcfg.ShardID)
				select {
				case <-ctx.Done():
					return
				case <-time.After(time.Second):
				}
				continue
			}
			if n == 0 {
				select {
				case <-ctx.Done():
					return
				case <-time.After(bcfg.AeronPollIdleWait):
				}
			}
		}
	}()

	log.Info("bridge started",
		"shard", bcfg.ShardID,
		"aeron_uri", bcfg.AeronChannel(),
		"stream_id", bcfg.AeronStreamID,
		"nats", nats.ConnectedURL(),
		"buffer_size", bcfg.BufferSize,
		"heartbeat", bcfg.HeartbeatSubject(),
		"env", cfg.Environment)

	if err := b.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	// Task 9.3.23 item 4: flush the in-memory buffer to JetStream before
	// termination. A residual buffer fails closed (nonzero exit) — the
	// WAL replay/archive covers it, but silent loss is not acceptable.
	fctx, fcancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer fcancel()
	if err := b.Flush(fctx); err != nil {
		log.Error("bridge: shutdown flush incomplete", "err", err)
		return fmt.Errorf("bridge flush: %w", err)
	}
	log.Info("bridge shutting down", "shard", bcfg.ShardID)
	return nil
}

func splitURLs(s string) []string {
	var out []string
	for _, u := range strings.Split(s, ",") {
		if u = strings.TrimSpace(u); u != "" {
			out = append(out, u)
		}
	}
	return out
}
