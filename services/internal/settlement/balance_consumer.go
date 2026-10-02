// balance_consumer.go — Trade-fill consumption & batching
// (Phase-03 Task 3.3.1 items 1+6; spec §2.3/§5.3).
//
// FillConsumer drains engine TradeFill fragments from the outbound IPC
// channel — Aeron `aeron:ipc?alias=orders_out` (stream 1002) via
// AeronSource, or the mandated shm fallback `{base}_{shard}_out` via
// ShmSource — resolves each fill through a TradeResolver, and flushes to
// BalanceService.ProcessFills in batches of BatchMax (100) or after
// FlushEvery (10ms), whichever comes first (Task 3.3.1 step 6).
//
// Loss discipline (spec §2.7): a fragment is consumed into `pending` only
// after decoding; a settlement failure aborts Run (the process restarts
// and replays — processed_trades dedup makes replay safe). A malformed
// frame can never panic into the cgo trampoline: decode is guarded and
// counted on the Malformed counter. Non-TradeFill events on the channel
// are skipped (counter only).
package settlement

import (
	"context"
	"fmt"
	"math"
	"strings"
	"sync/atomic"
	"time"

	"exchange/internal/ipc"
	"exchange/internal/ipc/aeron"
	"exchange/internal/ipc/wire"
	excnats "exchange/internal/nats"
	"exchange/internal/tracing"
	"exchange/pkg/decimal"
)

// Batching contract (Task 3.3.1 step 6).
const (
	// FillBatchMax is the maximum fills committed per transaction.
	FillBatchMax = 100
	// FillFlushEvery bounds fill-to-commit latency while idle.
	FillFlushEvery = 10 * time.Millisecond
)

// FillSource pumps inbound fragments to the consumer. Production
// adapters: AeronSource (preferred) and ShmSource (mandated fallback).
type FillSource interface {
	// Pump delivers up to limit fragments via deliver and returns the
	// count delivered; <0 signals a transport error.
	Pump(limit int, deliver func(payload []byte)) int
}

// FuncSource adapts a plain function to FillSource (tests, NATS-bridged
// feeds).
type FuncSource func(limit int, deliver func(payload []byte)) int

// Pump implements FillSource.
func (f FuncSource) Pump(limit int, deliver func(payload []byte)) int { return f(limit, deliver) }

// AeronSource adapts *aeron.Subscription. The subscription is created
// with AeronSource.Handler as its FragmentHandler; Pump then swaps in the
// caller's deliver for the duration of one Poll.
//
//	sub, _ := client.AddSubscription(uri, 1002, src.Handler(), timeout)
type AeronSource struct {
	Sub     *aeron.Subscription
	deliver atomic.Pointer[func([]byte)]
}

// NewAeronSource wraps sub.
func NewAeronSource(sub *aeron.Subscription) *AeronSource { return &AeronSource{Sub: sub} }

// Handler is the FragmentHandler to register at AddSubscription time.
// Fragments arriving outside a Pump call are dropped — Poll must only
// run inside Pump.
func (s *AeronSource) Handler() aeron.FragmentHandler {
	return func(buf []byte) {
		defer func() { _ = recover() }() // never panic across the cgo boundary
		if d := s.deliver.Load(); d != nil {
			(*d)(buf)
		}
	}
}

// Pump implements FillSource over aeron Subscription.Poll.
func (s *AeronSource) Pump(limit int, deliver func(payload []byte)) int {
	if s.Sub == nil {
		return -1
	}
	s.deliver.Store(&deliver)
	defer s.deliver.Store(nil)
	return s.Sub.Poll(limit)
}

// ShmSource drains the fallback shm channel ({base}_{shard}_out,
// EndpointGateway side). Payloads are copied off the ring before Consume.
type ShmSource struct {
	Ch *ipc.Channel
}

// NewShmSource wraps an open gateway-side channel.
func NewShmSource(ch *ipc.Channel) *ShmSource { return &ShmSource{Ch: ch} }

// Pump implements FillSource over the shm ring.
func (s *ShmSource) Pump(limit int, deliver func(payload []byte)) int {
	if s.Ch == nil {
		return -1
	}
	n := 0
	for n < limit {
		p := s.Ch.Peek()
		if p == nil {
			break
		}
		cp := make([]byte, len(p))
		copy(cp, p) // ring memory aliases — copy before Consume
		s.Ch.Consume()
		deliver(cp)
		n++
	}
	return n
}

// TradeRepublisher publishes a committed fill's raw Event frame to a
// JetStream subject — the bridge.Publisher contract (Nats-Msg-Id dedup).
// The gateway uses it to re-emit fills that only ever rode the shm ring:
// without an Aeron bridge in the loop, `trades.*`/`settlements.*`
// consumers (analytics, TCA, regulatory reporting) starve.
type TradeRepublisher interface {
	PublishEvent(ctx context.Context, subject, msgID string, payload []byte) error
}

// FillConsumer batches resolved fills into BalanceService commits.
// Construct with NewFillConsumer; Run until ctx cancellation or a
// settlement failure (fail-closed — Run returns the error).
type FillConsumer struct {
	svc      *BalanceService
	resolver TradeResolver
	source   FillSource
	repub    TradeRepublisher
	shardID  int64
	batchMax int
	flush    time.Duration

	malformed    atomic.Uint64 // undecodable frames (counted, not fatal)
	nonFill      atomic.Uint64 // non-TradeFill events skipped
	resolved     atomic.Uint64 // fills resolved + queued
	flushed      atomic.Uint64 // commits completed
	republished  atomic.Uint64 // committed fills re-emitted to JetStream
	repubDropped atomic.Uint64 // committed fills whose republish hit a
	// deterministic failure (invalid subject token — our own data, so
	// retrying can never succeed). The settlement commit already
	// landed; aborting would wedge the ring in a restart loop, so the
	// counter + log line carry the ops-visible signal instead.
	logf func(format string, args ...any) // nil → no-op
}

// NewFillConsumer wires the consumer. batchMax <= 0 defaults to
// FillBatchMax; flushEvery <= 0 defaults to FillFlushEvery.
func NewFillConsumer(svc *BalanceService, resolver TradeResolver, source FillSource,
	shardID int64, batchMax int, flushEvery time.Duration) (*FillConsumer, error) {
	if svc == nil || resolver == nil || source == nil {
		return nil, fmt.Errorf("fill consumer: service, resolver and source are all required (fail-closed)")
	}
	if batchMax <= 0 {
		batchMax = FillBatchMax
	}
	if flushEvery <= 0 {
		flushEvery = FillFlushEvery
	}
	c := &FillConsumer{
		svc: svc, resolver: resolver, source: source,
		shardID: shardID, batchMax: batchMax, flush: flushEvery,
	}
	c.logf = func(string, ...any) {}
	return c, nil
}

// WithRepublisher re-emits each committed fill's raw frame to
// "{trades,settlements}.{shard}.{symbol}" after the settlement commit —
// the same contract the Aeron bridge publishes (raw payload verbatim,
// Nats-Msg-Id "s{shard}-{engine_seq}"), so bridge+gateway coexistence
// dedups cleanly and a consumer restart re-emits on ring replay.
// A publish failure aborts the flush loop — Run returns it, the process
// restarts, and replayed frames republish under the same msg id.
func (c *FillConsumer) WithRepublisher(r TradeRepublisher) *FillConsumer {
	c.repub = r
	return c
}

// WithLogger sets the diagnostic sink used for dead-letter republish
// events (deterministic subject failures — counted, not fatal).
func (c *FillConsumer) WithLogger(l func(format string, args ...any)) *FillConsumer {
	if l != nil {
		c.logf = l
	}
	return c
}

// ConsumerMetrics is a snapshot of the consumer counters.
type ConsumerMetrics struct {
	Malformed    uint64
	NonFill      uint64
	Resolved     uint64
	Flushed      uint64
	Republished  uint64
	RepubDropped uint64
}

// Metrics snapshots the counters.
func (c *FillConsumer) Metrics() ConsumerMetrics {
	return ConsumerMetrics{
		Malformed: c.malformed.Load(), NonFill: c.nonFill.Load(),
		Resolved: c.resolved.Load(), Flushed: c.flushed.Load(),
		Republished: c.republished.Load(), RepubDropped: c.repubDropped.Load(),
	}
}

// Run is the consume loop: pump fragments → resolve fills → flush at
// batchMax fills or flushEvery since the first pending fill. Returns
// ctx.Err() on cancellation; any settlement error is returned verbatim
// (fail-closed halt — restart replays safely via processed_trades).
func (c *FillConsumer) Run(ctx context.Context) error {
	pending := make([]ResolvedTrade, 0, c.batchMax)
	var oldest time.Time // timestamp of pending[0]'s enqueue

	flush := func() error {
		if len(pending) == 0 {
			return nil
		}
		if _, err := c.svc.ProcessFills(ctx, pending); err != nil {
			return err
		}
		if err := c.republish(ctx, pending); err != nil {
			return err
		}
		c.flushed.Add(1)
		pending = pending[:0]
		return nil
	}

	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		capHint := c.batchMax - len(pending)
		if capHint <= 0 {
			capHint = 1
		}
		fills := make([]EngineFill, 0, capHint)
		n := c.source.Pump(capHint, func(payload []byte) {
			f, ok := c.decodeFragment(payload)
			if !ok {
				return
			}
			fills = append(fills, f)
		})
		if n < 0 {
			return fmt.Errorf("fill consumer: transport pump error")
		}
		if len(fills) > 0 {
			rts, err := c.resolveFills(ctx, fills)
			if err != nil {
				return err // resolution failure is fail-closed
			}
			if len(pending) == 0 && len(rts) > 0 {
				oldest = time.Now()
			}
			pending = append(pending, rts...)
		}
		switch {
		case len(pending) >= c.batchMax:
			if err := flush(); err != nil {
				return err
			}
		case len(pending) > 0 && time.Since(oldest) >= c.flush:
			if err := flush(); err != nil {
				return err
			}
		case len(pending) > 0:
			// Below the batch ceiling inside the flush window — short nap
			// so the loop doesn't spin-hot while waiting for the window.
			remaining := c.flush - time.Since(oldest)
			if remaining <= 0 {
				remaining = time.Millisecond
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(remaining):
			}
		case n == 0:
			// Idle: poll cadence. Small sleep keeps CPU idle without
			// adding meaningful latency (fills flush at 10ms anyway).
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Millisecond):
			}
		}
	}
}

// decodeFragment decodes one Event frame into an EngineFill (1e8-scaled
// wire ints → decimal). ok=false marks malformed frames and non-fill
// events (counted, non-fatal). Decode never touches I/O — resolution is
// deferred to resolveFills so a pump window resolves in one shot.
func (c *FillConsumer) decodeFragment(payload []byte) (fill EngineFill, ok bool) {
	defer func() {
		if r := recover(); r != nil {
			c.malformed.Add(1)
			fill, ok = EngineFill{}, false
		}
	}()
	if len(payload) < 8 {
		c.malformed.Add(1)
		return EngineFill{}, false
	}
	// Task 9.3.11 — strip the echoed EXCTRACE block before decode.
	body, _, _ := tracing.StripAeronTrace(payload)
	ev := ipc.DecodeEvent(body)
	if ev.TypeType() != wire.EventTypeTradeFill {
		c.nonFill.Add(1)
		return EngineFill{}, false
	}
	tf := ipc.EventTradeFill(ev)
	if tf == nil || tf.TradeId() > math.MaxInt64 {
		c.malformed.Add(1)
		return EngineFill{}, false
	}
	return EngineFill{
		TradeID:     tf.TradeId(),
		BuyOrderID:  tf.BuyOrderId(),
		SellOrderID: tf.SellOrderId(),
		Price:       decimal.NewFromScaled(tf.Price()),
		Qty:         decimal.NewFromScaled(tf.Qty()),
		EngineSeq:   tf.Seq(),
		ShardID:     c.shardID,
		// Aeron payloads alias the log buffer — copy so the frame
		// survives until the post-commit republish.
		Raw: append([]byte(nil), payload...),
	}, true
}

// republish fans the just-committed fills out to the JetStream trades and
// settlements streams — the bridge routing table for TradeFill. Symbol
// comes from the resolved trade (PG instruments.symbol, '/'→'-' for the
// NATS token); msgID mirrors the bridge's "s{shard}-{seq}" so a concurrent
// bridge relay of the same frame dedups at the stream level.
func (c *FillConsumer) republish(ctx context.Context, trades []ResolvedTrade) error {
	if c.repub == nil {
		return nil
	}
	for i := range trades {
		rt := &trades[i]
		if len(rt.Fill.Raw) == 0 {
			continue // resolved upstream (tests, non-wire sources)
		}
		symbol := strings.ReplaceAll(rt.Symbol, "/", "-")
		msgID := fmt.Sprintf("s%d-%d", c.shardID, rt.Fill.EngineSeq)
		for _, stream := range []string{"trades", "settlements"} {
			subj, err := excnats.Subject(stream, uint32(c.shardID), symbol)
			if err != nil {
				// Deterministic — the symbol/stream token is our own
				// resolved data; it will fail identically on every
				// replay. The fill is already committed, so aborting
				// would restart-loop the consumer forever. Dead-letter:
				// count, log, and move on.
				c.repubDropped.Add(1)
				c.logf("fill republish dead-letter trade %d stream %s symbol %q: %v",
					rt.Fill.TradeID, stream, symbol, err)
				continue
			}
			if err := c.repub.PublishEvent(ctx, subj, msgID, rt.Fill.Raw); err != nil {
				return fmt.Errorf("fill republish %s trade %d: %w", subj, rt.Fill.TradeID, err)
			}
			c.republished.Add(1)
		}
	}
	return nil
}

// resolveFills resolves one pump window. A BatchTradeResolver answers the
// whole window in two queries; anything else falls back to per-fill
// Resolve with identical fail-closed semantics — an unresolvable fill is
// never silently dropped.
func (c *FillConsumer) resolveFills(ctx context.Context, fills []EngineFill) ([]ResolvedTrade, error) {
	var rts []ResolvedTrade
	if br, ok := c.resolver.(BatchTradeResolver); ok {
		var err error
		rts, err = br.ResolveBatch(ctx, fills)
		if err != nil {
			return nil, err
		}
	} else {
		rts = make([]ResolvedTrade, 0, len(fills))
		for _, f := range fills {
			rt, err := c.resolver.Resolve(ctx, f)
			if err != nil {
				return nil, err
			}
			rts = append(rts, rt)
		}
	}
	c.resolved.Add(uint64(len(rts)))
	return rts, nil
}
