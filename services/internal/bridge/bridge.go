package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"exchange/internal/tracing"
	"exchange/internal/ipc"
)

// Publisher abstracts the JetStream/core-NATS sink so the Bridge is
// testable without a live cluster. The production adapter wraps
// internal/nats.Client (see cmd/bridge).
type Publisher interface {
	// PublishEvent publishes payload to a JetStream subject. msgID is the
	// Nats-Msg-Id used for stream dedup on publisher retry.
	PublishEvent(ctx context.Context, subject, msgID string, payload []byte) error
	// PublishHeartbeat publishes to a core-NATS subject (no stream ack —
	// liveness is ephemeral by design).
	PublishHeartbeat(subject string, payload []byte) error
	// Connected reports the underlying connection state.
	Connected() bool
}

// Bridge fans one engine shard's Aeron event stream out to JetStream.
// Ingest (HandleFragment) and egress (Run) are decoupled by the bounded
// buffer so a NATS outage never backpressures the Aeron subscription — the
// engine is insulated from cold-path stalls (spec §2.3.1).
type Bridge struct {
	cfg  Config
	pub  Publisher
	res  Resolver
	log  *slog.Logger
	m    *Metrics
	buf  *eventBuffer
	oidx *orderIndex
	wake chan struct{} // cap-1 ingest -> drain notification
}

// New validates cfg and returns a ready Bridge.
func New(cfg Config, pub Publisher, res Resolver, log *slog.Logger) (*Bridge, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if pub == nil {
		return nil, fmt.Errorf("bridge: publisher must not be nil")
	}
	if res == nil {
		res = MapResolver{}
	}
	if log == nil {
		log = slog.Default()
	}
	return &Bridge{
		cfg:  cfg,
		pub:  pub,
		res:  res,
		log:  log,
		m:    newMetrics(cfg.ShardID),
		buf:  newEventBuffer(cfg.BufferSize),
		oidx: newOrderIndex(cfg.OrderIndexSize),
		wake: make(chan struct{}, 1),
	}, nil
}

// Metrics exposes the counters (for the /metrics handler and tests).
func (b *Bridge) Metrics() *Metrics { return b.m }

// Snapshot returns current counter values.
func (b *Bridge) Snapshot() Snapshot {
	s := Snapshot{
		Received:    b.m.received.Load(),
		Published:   b.m.published.Load(),
		Dropped:     b.m.dropped.Load(),
		Malformed:   b.m.malformed.Load(),
		Unrouted:    b.m.unrouted.Load(),
		Reconnects:  b.m.reconnects.Load(),
		PublishErr:  b.m.publishErr.Load(),
		Heartbeats:  b.m.heartbeats.Load(),
		BufferDepth: b.m.bufferDepth.Load(),
		BufferCap:   b.cfg.BufferSize,
	}
	return s
}

// HandleFragment is the aeron.FragmentHandler. buf aliases the
// driver-mapped log buffer (zero-copy) and is only valid for the duration
// of this call — the payload is copied into the buffer slot before return.
// A malformed fragment must never panic into the cgo trampoline, so decode
// is guarded.
func (b *Bridge) HandleFragment(buf []byte) {
	defer func() {
		if r := recover(); r != nil {
			b.m.incMalformed()
			b.log.Error("bridge: malformed fragment dropped", "panic", r)
		}
	}()

	if len(buf) < 8 { // uoffset + minimal table — can't be a valid Event
		b.m.incMalformed()
		return
	}
	// Task 9.3.11 — strip the echoed EXCTRACE block for decode; the
	// republished payload keeps it so the trace survives the NATS hop.
	body, _, _ := tracing.StripAeronTrace(buf)
	ev := ipc.DecodeEvent(body)
	subjects := b.route(ev)
	if len(subjects) == 0 {
		return // TimeTick / NONE / undecodable union member — not republished
	}

	payload := make([]byte, len(buf))
	copy(payload, buf)

	evicted := b.buf.Push(bufferedEvent{
		subjects: subjects,
		msgID:    fmt.Sprintf("s%d-%d", b.cfg.ShardID, ev.Seq()),
		payload:  payload,
		seq:      ev.Seq(),
	})
	b.m.incReceived()
	if evicted {
		// Drop-oldest keeps the pipe unblocked (fail-open toward the engine,
		// fail-loud toward operators): count it and warn — downstream
		// consumers detect the discontinuity by seq gap.
		b.m.incDropped()
		b.log.Warn("bridge: buffer full, evicted oldest event",
			"shard", b.cfg.ShardID, "buffer_size", b.cfg.BufferSize)
	}
	b.m.setBufferDepth(int64(b.buf.Len()))
	select {
	case b.wake <- struct{}{}:
	default:
	}
}

// Run is the egress loop: it drains the buffer FIFO to JetStream and emits
// heartbeats until ctx is cancelled. Publish failures retry the head of
// line after ReconnectWait, so ordering is preserved across outages —
// buffered events replay in order on reconnect.
func (b *Bridge) Run(ctx context.Context) error {
	go b.heartbeatLoop(ctx)

	degraded := false
	for {
		e, gen, ok := b.buf.Head()
		if !ok {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-b.wake:
				continue
			case <-time.After(50 * time.Millisecond):
				continue // periodic ctx check while idle
			}
		}

		for subjIdx := 0; subjIdx < len(e.subjects); {
			subj := e.subjects[subjIdx]
			pctx, cancel := context.WithTimeout(ctx, b.cfg.PublishTimeout)
			start := time.Now()
			err := b.pub.PublishEvent(pctx, subj, e.msgID, e.payload)
			cancel()
			if err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				b.m.incPublishErr()
				if !degraded {
					degraded = true
					b.log.Warn("bridge: nats publish failing — buffering",
						"subject", subj, "err", err, "depth", b.buf.Len())
				}
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(b.cfg.ReconnectWait):
				}
				// If a full buffer evicted this head mid-fanout, restart on
				// the new head — the dropped event's unpublished subjects
				// are lost with it (counted via bridge_events_dropped_total).
				if b.buf.HeadGen() != gen {
					break
				}
				continue // retry head-of-line
			}
			b.m.observePublishNanos(time.Since(start).Nanoseconds())
			b.m.incPublished()
			if degraded {
				degraded = false
				b.m.incReconnects()
				b.log.Info("bridge: nats publish recovered — replaying buffer",
					"depth", b.buf.Len())
			}
			subjIdx++
		}

		// Pop only if the head we finished is still the head (unchanged gen).
		b.buf.PopIfGen(gen)
		b.m.setBufferDepth(int64(b.buf.Len()))
	}
}

// heartbeatLoop publishes "bridge.health.<shard>" every HeartbeatInterval
// (spec §2.3.1/Task 3.3.10: 5s cadence; absence triggers P1 alert).
func (b *Bridge) heartbeatLoop(ctx context.Context) {
	t := time.NewTicker(b.cfg.HeartbeatInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			payload, err := json.Marshal(struct {
				ShardID     uint32 `json:"shard_id"`
				Ts          string `json:"ts"`
				Connected   bool   `json:"nats_connected"`
				Published   uint64 `json:"events_published_total"`
				BufferDepth int64  `json:"buffer_depth"`
				Dropped     uint64 `json:"events_dropped_total"`
			}{
				ShardID:     b.cfg.ShardID,
				Ts:          now.UTC().Format(time.RFC3339Nano),
				Connected:   b.pub.Connected(),
				Published:   b.m.published.Load(),
				BufferDepth: b.m.bufferDepth.Load(),
				Dropped:     b.m.dropped.Load(),
			})
			if err != nil {
				continue // static struct — marshal cannot fail
			}
			if err := b.pub.PublishHeartbeat(b.cfg.HeartbeatSubject(), payload); err != nil {
				b.log.Warn("bridge: heartbeat publish failed",
					"subject", b.cfg.HeartbeatSubject(), "err", err)
				continue
			}
			b.m.incHeartbeats()
		}
	}
}
