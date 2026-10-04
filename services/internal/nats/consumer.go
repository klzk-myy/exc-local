package nats

import (
	"context"
	"fmt"
	"time"

	"github.com/nats-io/nats.go/jetstream"
)

// Canonical consumer template (Task 1.3.11 §3): durable pull consumers,
// explicit ack → at-least-once delivery semantics.
const (
	// ConsumerAckWait is how long the server waits for an ack before
	// redelivering. 30s comfortably covers a service restart.
	ConsumerAckWait = 30 * time.Second
	// ConsumerMaxDeliver bounds redelivery attempts before the message is
	// dropped (work-queue) — poison messages must alert via advisories,
	// not loop forever.
	ConsumerMaxDeliver = 5
)

// ConsumerOption customises the durable consumer created by EnsureConsumer.
// The defaults already encode the canonical template; options exist for
// filter subjects and for tests that need a short AckWait.
type ConsumerOption func(*jetstream.ConsumerConfig)

// WithFilterSubject restricts the consumer to one subject pattern (e.g.
// "trades.0.EUR-USD" or "trades.0.*"). On work-queue streams, consumers'
// filter subjects must not overlap — each message has exactly one owner.
func WithFilterSubject(filter string) ConsumerOption {
	return func(c *jetstream.ConsumerConfig) { c.FilterSubject = filter }
}

// WithAckWait overrides the redelivery timeout (tests use short windows).
func WithAckWait(d time.Duration) ConsumerOption {
	return func(c *jetstream.ConsumerConfig) { c.AckWait = d }
}

// WithMaxDeliver overrides the redelivery cap.
func WithMaxDeliver(n int) ConsumerOption {
	return func(c *jetstream.ConsumerConfig) { c.MaxDeliver = n }
}

// EnsureConsumer creates (or updates) a durable pull consumer on stream
// with the canonical at-least-once template: explicit ack, AckWait 30s,
// MaxDeliver 5. Durable names must be stable — the durable identity is how
// the server tracks delivery/ack state across restarts.
func (c *Client) EnsureConsumer(ctx context.Context, stream, durable string, opts ...ConsumerOption) (jetstream.Consumer, error) {
	cfg := jetstream.ConsumerConfig{
		Name:          durable,
		Durable:       durable,
		AckPolicy:     jetstream.AckExplicitPolicy,
		AckWait:       ConsumerAckWait,
		MaxDeliver:    ConsumerMaxDeliver,
		DeliverPolicy: jetstream.DeliverAllPolicy,
		ReplayPolicy:  jetstream.ReplayInstantPolicy,
	}
	for _, o := range opts {
		o(&cfg)
	}
	cons, err := c.js.CreateOrUpdateConsumer(ctx, stream, cfg)
	if err != nil {
		return nil, fmt.Errorf("nats: ensure consumer %q on stream %q: %w", durable, stream, err)
	}
	return cons, nil
}

const (
	// consumerRetryBase/Cap bound the exponential backoff between attach
	// attempts in EnsureConsumerRetry.
	consumerRetryBase = 500 * time.Millisecond
	consumerRetryCap  = 15 * time.Second
	// consumerRetryWarnAfter is the consecutive-failure count after which
	// attach failures escalate from Info to Warn (≈ first ~30s of retrying).
	consumerRetryWarnAfter = 6
)

// EnsureConsumerRetry is EnsureConsumer with self-healing retry for the
// boot-order race: a service that starts before a stream's owner has
// provisioned it must NOT lose the consumer goroutine permanently — the
// attach is retried with exponential backoff until it succeeds or ctx
// is cancelled. On each failure a canonical stream (nats.Streams) is
// opportunistically re-ensured, since an absent stream is the usual
// cause and EnsureStream is idempotent. Failures escalate Info→Warn
// after consumerRetryWarnAfter attempts (fail-visible, never silent).
// Returns ctx.Err() when cancelled before attach.
func (c *Client) EnsureConsumerRetry(ctx context.Context, stream, durable string, opts ...ConsumerOption) (jetstream.Consumer, error) {
	wait := consumerRetryBase
	canonical := false
	for _, s := range Streams {
		if s == stream {
			canonical = true
			break
		}
	}
	for attempt := 1; ; attempt++ {
		cons, err := c.EnsureConsumer(ctx, stream, durable, opts...)
		if err == nil {
			if attempt > 1 {
				c.log.Info("nats: consumer attached after retry",
					"stream", stream, "durable", durable, "attempts", attempt)
			}
			return cons, nil
		}
		if ctx.Err() != nil {
			return nil, fmt.Errorf("nats: ensure consumer %q on stream %q: %w", durable, stream, ctx.Err())
		}
		if attempt >= consumerRetryWarnAfter {
			c.log.Warn("nats: consumer attach still failing — will keep retrying",
				"stream", stream, "durable", durable, "attempts", attempt, "err", err)
		} else {
			c.log.Info("nats: consumer attach failed — retrying",
				"stream", stream, "durable", durable, "attempt", attempt, "err", err)
		}
		if canonical {
			if _, serr := c.EnsureStream(ctx, stream); serr != nil {
				c.log.Warn("nats: stream ensure failed", "stream", stream, "err", serr)
			}
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, fmt.Errorf("nats: ensure consumer %q on stream %q: %w", durable, stream, ctx.Err())
		case <-timer.C:
		}
		wait *= 2
		if wait > consumerRetryCap {
			wait = consumerRetryCap
		}
	}
}

// Fetch pulls up to batch messages from the durable consumer, waiting at
// most maxWait for the batch to fill. Callers must Ack (or Nak/Term) each
// message; un-acked messages redeliver after AckWait — that is the
// at-least-once contract. Returns the messages actually received (possibly
// fewer than batch, or empty on timeout).
func Fetch(ctx context.Context, cons jetstream.Consumer, batch int, maxWait time.Duration) ([]jetstream.Msg, error) {
	mb, err := cons.Fetch(batch, jetstream.FetchMaxWait(maxWait))
	if err != nil {
		return nil, fmt.Errorf("nats: fetch: %w", err)
	}
	var msgs []jetstream.Msg
	for m := range mb.Messages() {
		msgs = append(msgs, m)
	}
	if err := mb.Error(); err != nil {
		return msgs, fmt.Errorf("nats: fetch: %w", err)
	}
	return msgs, nil
}

// Fetch is the Client-bound convenience form of the package-level Fetch.
func (c *Client) Fetch(ctx context.Context, cons jetstream.Consumer, batch int, maxWait time.Duration) ([]jetstream.Msg, error) {
	return Fetch(ctx, cons, batch, maxWait)
}

// Subscribe starts a continuous pull consumer invoking handler for every
// message. The handler owns acking: call m.Ack() on success, m.Nak() to
// requeue, or let AckWait expire for redelivery. The returned stop function
// drains the consume loop; it does not delete the durable.
func (c *Client) Subscribe(cons jetstream.Consumer, handler func(jetstream.Msg)) (stop func(), err error) {
	cc, err := cons.Consume(func(m jetstream.Msg) { handler(m) })
	if err != nil {
		return nil, fmt.Errorf("nats: consume: %w", err)
	}
	return func() { cc.Stop() }, nil
}
