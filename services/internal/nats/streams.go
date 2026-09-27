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
}

const (
	// StreamReplicas keeps every stream R3-replicated across the cluster.
	StreamReplicas = 3
	// StreamMaxAge is the 7-day replay window.
	StreamMaxAge = 7 * 24 * time.Hour
)

// streamConfig builds the canonical StreamConfig for one stream.
func streamConfig(name string) jetstream.StreamConfig {
	return jetstream.StreamConfig{
		Name:      name,
		Subjects:  []string{name + ".>"},
		Retention: jetstream.WorkQueuePolicy, // removed once a consumer acks
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
