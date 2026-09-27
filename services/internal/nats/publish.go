package nats

import (
	"context"
	"fmt"
	"strings"

	"github.com/nats-io/nats.go/jetstream"
)

// Subject builds the per-subject ordering key "{stream}.{shard_id}.{symbol}"
// (e.g. "trades.0.EUR-USD"). Each component must be a single NATS token —
// no dots, spaces or wildcards — otherwise subject routing would silently
// fan out to the wrong consumers.
func Subject(stream string, shardID uint32, symbol string) (string, error) {
	for label, tok := range map[string]string{"stream": stream, "symbol": symbol} {
		if tok == "" {
			return "", fmt.Errorf("nats: %s token must not be empty", label)
		}
		if strings.ContainsAny(tok, ". *>\t\n\r") {
			return "", fmt.Errorf("nats: %s token %q contains a subject separator or wildcard", label, tok)
		}
	}
	return fmt.Sprintf("%s.%d.%s", stream, shardID, symbol), nil
}

// Publish sends payload to "{stream}.{shard}.{symbol}" and blocks until the
// stream's PubAck confirms durable R3 quorum (or ctx expires). shardID must
// be stable per symbol/ordering domain — consumers rely on it for
// per-subject ordering.
func (c *Client) Publish(ctx context.Context, stream string, shardID uint32, symbol string, payload []byte) (*jetstream.PubAck, error) {
	subj, err := Subject(stream, shardID, symbol)
	if err != nil {
		return nil, err
	}
	ack, err := c.js.Publish(ctx, subj, payload)
	if err != nil {
		return nil, fmt.Errorf("nats: publish %q: %w", subj, err)
	}
	return ack, nil
}
