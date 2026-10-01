package nats

import (
	"context"
	"fmt"
	"time"

	"github.com/nats-io/nats.go/jetstream"
)

// Canonical event streams for the cold-path backbone (Task 1.3.11 §2, spec
// §2.3.1). Every stream binds the wildcard subject "{name}.>" so events are
// published on "{stream}.{shard_id}.{symbol}" (e.g. "trades.0.EUR-USD").
var Streams = []string{
	"trades",
	"settlements",
	"compliance",
	"analytics",
	"funding",
	"margin-events",
	"surveillance",
	// Post-commit ledger notifications: account.balance.changed.{id} and
	// account.swap.charged.{id} (spec §5.3 invariant 4). Without this
	// stream the JetStream publisher's PubAck never arrives and the
	// fill consumer halts on BALANCE_EVENT_DISPATCH_FAILED.
	"account",
	// Phase-17 Task 17.3.2 — premium order-level feed republished by the
	// bridge (l3.{shard}.{symbol}). Distinct from "analytics" because its
	// retention cadence and premium-tier consumers differ.
	"l3",
	// Task 7.3.9 feed seam — venue-side LP quote events published by the
	// FIX mass-quote path on quotes.lp.{lp_id}.{symbol-token}, consumed
	// by marketdata's LPBookProducer. A dedicated stream (not
	// "analytics"): the subject space must not collide with the bridge's
	// FlatBuffers wire-event subjects, and per-LP ordering is a distinct
	// ordering domain from the engine's {shard}.{symbol} key.
	"quotes",
	// Ops-alert trail (Task 13.3.2 + §5.3): ops.alerts.{settlement,
	// reconciliation, risk, monitoring, backoffice} — the durable
	// P1/P2 page path. Without it every OpsAlert publish fails
	// PubAck-less and the reconciliation/liquidation alert trail is
	// silent.
	"ops",
}

const (
	// StreamReplicas keeps every stream R3-replicated across the cluster.
	StreamReplicas = 3
	// StreamMaxAge is the 7-day replay window.
	StreamMaxAge = 7 * 24 * time.Hour
)

// streamConfig builds the canonical StreamConfig for one stream.
//
// Retention is LimitsPolicy, not WorkQueue: spec §2.3.1 makes every
// stream a fan-out topic — "Settlement, Risk, Compliance, Analytics, and
// Market Data services each maintain independent NATS JetStream consumer
// groups with durable cursors". WorkQueue retention deletes a message on
// the FIRST consumer ack (silently starving every other consumer group)
// and rejects overlapping filter subjects, so it is unusable here. The
// 7-day MaxAge + DiscardOld bounds storage instead; per-consumer
// at-least-once is preserved by each durable's own ack floor.
func streamConfig(name string) jetstream.StreamConfig {
	return jetstream.StreamConfig{
		Name:      name,
		Subjects:  []string{name + ".>"},
		Retention: jetstream.LimitsPolicy, // retained for all consumer groups until MaxAge
		Storage:   jetstream.FileStorage,
		Replicas:  StreamReplicas,
		MaxAge:    StreamMaxAge,
		Discard:   jetstream.DiscardOld,
		// 2-minute dedup window on Nats-Msg-Id for publisher retries.
		Duplicates: 2 * time.Minute,
	}
}

// EnsureStreams creates or reconciles all canonical streams. It is
// idempotent: CreateOrUpdateStream creates missing streams and updates
// existing ones whose config diverged. Returns per-stream info for callers
// that want to verify/print the result.
func (c *Client) EnsureStreams(ctx context.Context) ([]*jetstream.StreamInfo, error) {
	infos := make([]*jetstream.StreamInfo, 0, len(Streams))
	for _, name := range Streams {
		s, err := c.js.CreateOrUpdateStream(ctx, streamConfig(name))
		if err != nil {
			return infos, fmt.Errorf("nats: ensure stream %q: %w", name, err)
		}
		info, err := s.Info(ctx)
		if err != nil {
			return infos, fmt.Errorf("nats: stream %q info: %w", name, err)
		}
		c.log.Info("nats stream ensured",
			"stream", name,
			"subjects", info.Config.Subjects,
			"replicas", info.Config.Replicas,
			"retention", info.Config.Retention.String(),
			"max_age", info.Config.MaxAge,
			"msgs", info.State.Msgs)
		infos = append(infos, info)
	}
	return infos, nil
}

// EnsureStream creates or reconciles a single canonical stream by name.
func (c *Client) EnsureStream(ctx context.Context, name string) (*jetstream.StreamInfo, error) {
	s, err := c.js.CreateOrUpdateStream(ctx, streamConfig(name))
	if err != nil {
		return nil, fmt.Errorf("nats: ensure stream %q: %w", name, err)
	}
	return s.Info(ctx)
}
