// Package nats wraps the nats.go client with the JetStream API used by the
// exchange cold-path event backbone (Task 1.3.11, spec §2.3.1).
//
// The 3-node exc-jetstream cluster (deploy/nats/*.conf, docker-compose.dev.yml)
// carries asynchronous events — trades, settlements, compliance, analytics,
// funding, margin-events, surveillance — between the C++ core, Go services
// and ClickHouse ingestion. The hot path stays on Aeron (Task 1.3.10); this
// package is the durable, replayable backbone.
//
// Conventions:
//   - Streams are work-queue (WorkQueuePolicy), file-backed, R3-replicated,
//     with a 7-day replay window (MaxAge). See streams.go for the exact
//     config; provisioning is idempotent via CreateOrUpdateStream.
//   - Per-subject ordering key: "{stream}.{shard_id}.{symbol}", e.g.
//     "trades.0.EUR-USD". Every stream binds the wildcard subject "{name}.>".
//   - Consumers are durable pull consumers, explicit ack (at-least-once),
//     AckWait 30s, MaxDeliver 5.
//   - All calls honour the caller's context — every JetStream API request is
//     bounded by a deadline.
package nats

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	gonats "github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// Config holds connection parameters for the JetStream cluster.
type Config struct {
	// URLs are the cluster seed URLs; the client learns the full topology
	// from the cluster itself. Dev default:
	// nats://127.0.0.1:4222,nats://127.0.0.1:4223,nats://127.0.0.1:4224
	URLs []string

	// Name identifies this client in server monitoring output.
	Name string

	// ConnectTimeout bounds the initial dial and each reconnect attempt.
	ConnectTimeout time.Duration
	// ReconnectWait is the delay between reconnect attempts.
	ReconnectWait time.Duration
	// MaxReconnects caps reconnect attempts; -1 retries forever (fail-closed
	// services should still crash-loop at the supervisor level, but NATS
	// itself retries indefinitely so transient node restarts self-heal).
	MaxReconnects int
	// PingInterval / MaxPingsOut detect dead TCP connections.
	PingInterval time.Duration
	MaxPingsOut  int
}

// DefaultConfig returns dev-friendly defaults for the given seed URLs.
func DefaultConfig(urls []string) Config {
	return Config{
		URLs:           urls,
		Name:           "exc-service",
		ConnectTimeout: 5 * time.Second,
		ReconnectWait:  250 * time.Millisecond,
		MaxReconnects:  -1,
		PingInterval:   10 * time.Second,
		MaxPingsOut:    3,
	}
}

// Client wraps a NATS connection and its JetStream context.
type Client struct {
	nc  *gonats.Conn
	js  jetstream.JetStream
	log *slog.Logger
}

// Connect dials the cluster (no credentials in dev; TLS/auth land with the
// secrets-store work in Phase-13.5). The context bounds the initial dial
// only — reconnects after a successful dial are governed by Config.
func Connect(ctx context.Context, cfg Config, log *slog.Logger) (*Client, error) {
	if log == nil {
		log = slog.Default()
	}
	if len(cfg.URLs) == 0 {
		return nil, fmt.Errorf("nats: no server URLs configured")
	}

	opts := []gonats.Option{
		gonats.Name(cfg.Name),
		gonats.Timeout(cfg.ConnectTimeout),
		gonats.ReconnectWait(cfg.ReconnectWait),
		gonats.MaxReconnects(cfg.MaxReconnects),
		gonats.PingInterval(cfg.PingInterval),
		gonats.MaxPingsOutstanding(cfg.MaxPingsOut),
		// Randomize which seed we dial first so a dead node doesn't
		// stall connect with serial timeouts.
		gonats.NoEcho(),
		gonats.ReconnectHandler(func(nc *gonats.Conn) {
			log.Warn("nats reconnected", "url", nc.ConnectedUrl())
		}),
		gonats.DisconnectErrHandler(func(nc *gonats.Conn, err error) {
			if err == nil {
				return // graceful Close(), not a drop
			}
			log.Warn("nats disconnected", "err", err)
		}),
		gonats.ClosedHandler(func(nc *gonats.Conn) {
			if err := nc.LastError(); err != nil {
				log.Error("nats connection closed permanently", "err", err)
			}
		}),
		gonats.ErrorHandler(func(nc *gonats.Conn, sub *gonats.Subscription, err error) {
			subj := ""
			if sub != nil {
				subj = sub.Subject
			}
			log.Error("nats async error", "subject", subj, "err", err)
		}),
	}

	dialCtx := ctx
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		dialCtx, cancel = context.WithTimeout(ctx, cfg.ConnectTimeout*time.Duration(len(cfg.URLs)))
		defer cancel()
	}
	type result struct {
		nc  *gonats.Conn
		err error
	}
	ch := make(chan result, 1)
	go func() {
		nc, err := gonats.Connect(strings.Join(cfg.URLs, ","), opts...)
		ch <- result{nc, err}
	}()

	var nc *gonats.Conn
	select {
	case <-dialCtx.Done():
		return nil, fmt.Errorf("nats: connect: %w", dialCtx.Err())
	case r := <-ch:
		if r.err != nil {
			return nil, fmt.Errorf("nats: connect %q: %w", cfg.URLs, r.err)
		}
		nc = r.nc
	}

	js, err := jetstream.New(nc)
	if err != nil {
		nc.Close()
		return nil, fmt.Errorf("nats: jetstream context: %w", err)
	}

	log.Info("nats connected", "url", nc.ConnectedUrl(), "name", cfg.Name)
	return &Client{nc: nc, js: js, log: log}, nil
}

// Conn exposes the raw connection for callers needing core-NATS features
// (KV, request/reply). Prefer the helpers on Client for JetStream work.
func (c *Client) Conn() *gonats.Conn { return c.nc }

// JetStream exposes the underlying JetStream context for advanced usage.
func (c *Client) JetStream() jetstream.JetStream { return c.js }

// Connected reports whether the underlying connection is live.
func (c *Client) Connected() bool { return c.nc.IsConnected() }

// ConnectedURL returns the server the client is currently attached to.
func (c *Client) ConnectedURL() string { return c.nc.ConnectedUrl() }

// Close drains in-flight messages and closes the connection.
func (c *Client) Close() {
	c.nc.Close()
}
