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
	"sync"
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

// AsyncTradeRepublisher is the optional pipelined variant of
// TradeRepublisher: PublishEventAsync issues without waiting on a
// per-message ack; FlushEvents bounds the outstanding-ack window
// (waiting oldest-first once an implementation-defined in-flight cap
// is exceeded) and returns the first failure seen. republish()
// prefers it when the bound republisher supports it — 2 synchronous
// publishes per fill is the out-ring consumer's throughput ceiling
// otherwise, and draining every outstanding ack per batch pays the
// slowest ack RTT each flush.
type AsyncTradeRepublisher interface {
	TradeRepublisher
	PublishEventAsync(subject, msgID string, payload []byte) error
	FlushEvents(ctx context.Context) error
}

// BacklogFill is one committed fill eligible for the boot-time republish
// repair — the durable fields needed to re-emit its Event frame under
// the bridge's subject/msgID contract.
type BacklogFill struct {
	TradeID   int64
	Symbol    string
	EngineSeq int64
	Raw       []byte
}

// BacklogSource is the optional resolver seam feeding republishBacklog:
// it lists fills committed to processed_trades within lookback whose raw
// frames were persisted in the same transaction (migration 281) — the
// crash window between ledger commit and JetStream publish.
type BacklogSource interface {
	RepublishBacklog(ctx context.Context, shardID int64, lookback time.Duration) ([]BacklogFill, error)
}

// RepublishBacklogLookback bounds the boot repair scan. The gap is the
// commit→publish window (microseconds normally); one hour of backlog is
// far beyond any restart gap while keeping the scan trivially small.
const RepublishBacklogLookback = time.Hour

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
	// adoptPayload marks source-delivered buffers as exclusively owned
	// (settle-queue taps allocate per frame). decodeFragment can adopt
	// them as Fill.Raw instead of paying a second copy — Aeron sources
	// must keep the default because their payloads alias the log buffer.
	adoptPayload bool

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

// WithOwnedPayloads tells the consumer the source hands it exclusively-
// owned payload buffers, so Fill.Raw may adopt them without copying.
func (c *FillConsumer) WithOwnedPayloads() *FillConsumer {
	c.adoptPayload = true
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

// ShardID reports the shard this consumer drains — used for the
// per-shard metric labels.
func (c *FillConsumer) ShardID() int64 { return c.shardID }

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
	// Repair the committed-but-unpublished crash window before consuming:
	// fills whose settlement commit landed but whose JetStream publish
	// never ran. Nats-Msg-Id dedup makes re-emitted frames no-ops, so
	// scanning a generous lookback is safe; only the gap actually lands.
	// Backlog repair runs CONCURRENTLY with the pump: a busy restart can
	// carry tens of thousands of lookback fills, and a synchronous
	// repair would stall settlement behind them — the settleQueue
	// backpressures the drain while fresh fills pile up. The republish
	// is a stream-level no-op for already-published frames, so ordering
	// vs live batches doesn't matter. An error still fails the consumer
	// (fail-closed) — checked alongside the worker error in the loop.
	backlogErr := make(chan error, 1)
	bctx, bcancel := context.WithCancel(ctx)
	defer bcancel() // a Run exit must not leave a second backlog publisher alive
	go func() { backlogErr <- c.republishBacklog(bctx) }()
	pending := make([]ResolvedTrade, 0, c.batchMax)
	fillsBuf := make([]EngineFill, 0, c.batchMax)
	var oldest time.Time // timestamp of pending[0]'s enqueue

	// Flush pipeline: the pump loop hands full/expired batches to one
	// flush worker so the PG commit + republish wait of batch N overlaps
	// the pump+resolve of batch N+1. A single worker keeps settlement
	// order; the bounded channel preserves backpressure (a full queue
	// stalls the pump → frameTap → ring, same fail-closed contract as
	// the inline flush it replaces). First worker error propagates and
	// fails the consumer — restart replays via processed_trades.
	batchQ := make(chan []ResolvedTrade, 4)
	workerErr := make(chan error, 1)
	workerDone := make(chan struct{})
	// Batch slice pool: each flushed batch hands ownership to the
	// worker, which returns it — the per-iteration make() churn was
	// the consumer's dominant allocation under sustained fills.
	batchPool := &sync.Pool{New: func() any {
		return make([]ResolvedTrade, 0, c.batchMax)
	}}
	// The worker commits with a WithoutCancel context: once a batch is
	// enqueued it must commit even if Run's ctx is cancelled mid-flight —
	// a cancelled commit between trades/journal/processed_trades is the
	// partial-loss window the shutdown drain ordering exists to close
	// (spec §2.7). The commit's own tx deadlines still bound it.
	commitCtx := context.WithoutCancel(ctx)
	go func() {
		defer close(workerDone)
		for b := range batchQ {
			if _, err := c.svc.ProcessFills(commitCtx, b); err != nil {
				workerErr <- err
				return
			}
			if err := c.republish(commitCtx, b); err != nil {
				workerErr <- err
				return
			}
			c.flushed.Add(1)
			batchPool.Put(b[:0])
		}
	}()
	defer func() {
		close(batchQ)
		<-workerDone
	}()

	// flush enqueues the pending batch and re-arms the accumulator. The
	// worker owns the slice it receives — pending is never reused. No
	// ctx.Done escape: an enqueued batch commits (WithoutCancel worker)
	// or the worker is dead and its error propagates — a third "send
	// abandoned" outcome would strand resolved fills that already left
	// the settlement queue.
	flush := func() error {
		if len(pending) == 0 {
			return nil
		}
		// Prioritize a dead worker's error over the send: when both cases
		// are ready Go picks randomly, and enqueueing behind a dead worker
		// silently strands the batch (WAL/recon recovers it, but the
		// error should surface instead of a false success).
		select {
		case err := <-workerErr:
			return err
		default:
		}
		select {
		case err := <-workerErr:
			return err
		case batchQ <- pending:
		}
		pending = batchPool.Get().([]ResolvedTrade)[:0]
		return nil
	}

	// shutdownDrain is the ctx-done exit path: keep pumping until the
	// source runs dry, resolve what was pulled, and flush — frames a
	// Pump already removed from the settlement queue are owned by this
	// consumer and die with it, so cancellation must trigger a drain,
	// not abandonment. Terminates because the producer (out-ring frame
	// tap) is stopped before this consumer's ctx is cancelled — the
	// ring_drain → settle_drain → sweepStop ordering — so Pump→0 is
	// guaranteed. Resolution runs on commitCtx: cancelling mid-resolve
	// would strand exactly the in-flight window the ordering exists to
	// protect.
	shutdownDrain := func() error {
		// Bound the sweep anyway: a producer that outlives the stop
		// ordering would feed this loop forever. The budget covers
		// several times the real in-process queue depth (16K frames,
		// batchMax windows) — exhausting it means the contract was
		// violated, which must fail loudly rather than hang shutdown
		// or silently abandon the tail of a live stream.
		budget := c.batchMax * 64
		for budget > 0 {
			fills := fillsBuf[:0]
			n := c.source.Pump(c.batchMax, func(payload []byte) {
				f, ok := c.decodeFragment(payload)
				if !ok {
					return
				}
				fills = append(fills, f)
			})
			if n < 0 {
				if ferr := flush(); ferr != nil {
					return ferr
				}
				return fmt.Errorf("fill consumer: transport pump error")
			}
			if n == 0 {
				return flush()
			}
			budget -= n
			rts, err := c.resolveFills(commitCtx, fills)
			if err != nil {
				if ferr := flush(); ferr != nil {
					return ferr
				}
				return err
			}
			pending = append(pending, rts...)
			if len(pending) >= c.batchMax {
				if err := flush(); err != nil {
					return err
				}
			}
		}
		if ferr := flush(); ferr != nil {
			return ferr
		}
		if c.logf != nil {
			c.logf("fill consumer: shutdown drain budget exhausted — producer outlived stop ordering, tail abandoned")
		}
		return fmt.Errorf("fill consumer: shutdown drain exceeded")
	}

	for {
		if err := ctx.Err(); err != nil {
			// Shutdown: drain the queue and hand every owned fill to the
			// worker before leaving — the deferred batchQ close lets it
			// commit them (WithoutCancel), so a graceful stop loses
			// nothing already pulled from the settlement queue.
			if derr := shutdownDrain(); derr != nil {
				return derr
			}
			return err
		}
		select {
		case err := <-workerErr:
			return err
		case err := <-backlogErr:
			if err != nil {
				return err
			}
			backlogErr = nil // closed book — repair finished cleanly
		default:
		}
		capHint := c.batchMax - len(pending)
		if capHint <= 0 {
			capHint = 1
		}
		fills := fillsBuf[:0]
		n := c.source.Pump(capHint, func(payload []byte) {
			f, ok := c.decodeFragment(payload)
			if !ok {
				return
			}
			fills = append(fills, f)
		})
		if n < 0 {
			// Flush resolved work before failing — a transport halt is no
			// reason to strand pending fills already out of the queue.
			if ferr := flush(); ferr != nil {
				return ferr
			}
			return fmt.Errorf("fill consumer: transport pump error")
		}
		if len(fills) > 0 {
			// commitCtx, not ctx: pumped frames are owned by this
			// consumer — a mid-resolve cancellation would strand them.
			rts, err := c.resolveFills(commitCtx, fills)
			if err != nil {
				// Still flush already-resolved pending fills before
				// failing closed — stranding them is the same loss
				// window the shutdown ordering exists to close.
				if ferr := flush(); ferr != nil {
					return ferr
				}
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
				if derr := shutdownDrain(); derr != nil {
					return derr
				}
				return ctx.Err()
			case <-time.After(remaining):
			}
		case n == 0:
			// Idle: poll cadence. Small sleep keeps CPU idle without
			// adding meaningful latency (fills flush at 10ms anyway).
			select {
			case <-ctx.Done():
				if derr := shutdownDrain(); derr != nil {
					return derr
				}
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
		// The frame must survive until the post-commit republish:
		// adopt when the source guarantees ownership, copy otherwise
		// (Aeron payloads alias the log buffer).
		Raw: c.rawFor(payload),
	}, true
}

// rawFor returns a payload the fill may retain: adopted verbatim when
// the source guarantees exclusive ownership, defensively copied
// otherwise.
func (c *FillConsumer) rawFor(payload []byte) []byte {
	if c.adoptPayload {
		return payload
	}
	return append([]byte(nil), payload...)
}

// republish fans the just-committed fills out to the JetStream trades and
// settlements streams — the bridge routing table for TradeFill. Symbol
// comes from the resolved trade (PG instruments.symbol, '/'→'-' for the
// NATS token); msgID mirrors the bridge's "s{shard}-{seq}" so a concurrent
// bridge relay of the same frame dedups at the stream level. When the
// republisher supports AsyncTradeRepublisher the whole batch issues
// pipelined and a single FlushEvents waits for all acks — the per-fill
// sync publish was the consumer's throughput ceiling (~300 fills/s).
func (c *FillConsumer) republish(ctx context.Context, trades []ResolvedTrade) error {
	if c.repub == nil {
		return nil
	}
	if ar, ok := c.repub.(AsyncTradeRepublisher); ok {
		for i := range trades {
			rt := &trades[i]
			if len(rt.Fill.Raw) == 0 {
				continue // resolved upstream (tests, non-wire sources)
			}
			if err := c.publishFillFrameAsync(ar, int64(rt.Fill.TradeID),
				rt.Symbol, int64(rt.Fill.EngineSeq), rt.Fill.Raw); err != nil {
				return err
			}
		}
		return ar.FlushEvents(ctx)
	}
	for i := range trades {
		rt := &trades[i]
		if len(rt.Fill.Raw) == 0 {
			continue // resolved upstream (tests, non-wire sources)
		}
		if err := c.publishFillFrame(ctx, int64(rt.Fill.TradeID), rt.Symbol,
			int64(rt.Fill.EngineSeq), rt.Fill.Raw); err != nil {
			return err
		}
	}
	return nil
}

// republishBacklog re-emits persisted raw frames for fills committed
// within RepublishBacklogLookback — the durable repair for the
// commit→publish crash window (migration 281). No-op without a
// republisher (bridge topology) or a resolver lacking BacklogSource.
func (c *FillConsumer) republishBacklog(ctx context.Context) error {
	src, ok := c.resolver.(BacklogSource)
	if !ok || c.repub == nil {
		return nil
	}
	fills, err := src.RepublishBacklog(ctx, c.shardID, RepublishBacklogLookback)
	if err != nil {
		return fmt.Errorf("fill republish backlog scan: %w", err)
	}
	// Sync publishes on purpose: this runs CONCURRENTLY with the pump
	// loop (see Run), and the async republisher's pending-ack state is
	// owned by the flush worker — sharing it across goroutines would
	// interleave ack ownership. Sync publish is an independent
	// request-reply, safe alongside the pipelined path; the repair rate
	// is secondary to keeping live fills flowing (the lookback rescan
	// on the next restart picks up any stragglers).
	for i := range fills {
		f := &fills[i]
		if len(f.Raw) == 0 {
			continue
		}
		if err := c.publishFillFrame(ctx, f.TradeID, f.Symbol, f.EngineSeq, f.Raw); err != nil {
			return fmt.Errorf("fill republish backlog trade %d: %w", f.TradeID, err)
		}
	}
	if len(fills) > 0 {
		c.logf("fill republish backlog: re-emitted %d committed fills (shard %d)",
			len(fills), c.shardID)
	}
	return nil
}

// publishFillFrame emits one committed fill's raw frame to both bridge
// streams — subject from the resolved symbol ('/'→'-'), msgID the
// bridge's "s{shard}-{seq}" dedup key. A deterministic subject failure
// dead-letters (count + log); a publish error aborts the caller.
func (c *FillConsumer) publishFillFrame(ctx context.Context, tradeID int64,
	symbol string, engineSeq int64, raw []byte) error {
	sym := excnats.SymbolToken(symbol)
	msgID := fmt.Sprintf("s%d-%d", c.shardID, engineSeq)
	for _, stream := range []string{"trades", "settlements"} {
		subj, err := excnats.Subject(stream, uint32(c.shardID), sym)
		if err != nil {
			// Deterministic — the symbol/stream token is our own
			// resolved data; it will fail identically on every replay.
			// The fill is already committed, so aborting would
			// restart-loop the consumer forever. Dead-letter: count,
			// log, and move on.
			c.repubDropped.Add(1)
			c.logf("fill republish dead-letter trade %d stream %s symbol %q: %v",
				tradeID, stream, sym, err)
			continue
		}
		if err := c.repub.PublishEvent(ctx, subj, msgID, raw); err != nil {
			return fmt.Errorf("fill republish %s trade %d: %w", subj, tradeID, err)
		}
		c.republished.Add(1)
	}
	return nil
}

// publishFillFrameAsync is the pipelined variant for
// AsyncTradeRepublisher — identical subject/msgID/dead-letter contract,
// issues without waiting for the ack (FlushEvents collects them once
// per batch).
func (c *FillConsumer) publishFillFrameAsync(ar AsyncTradeRepublisher,
	tradeID int64, symbol string, engineSeq int64, raw []byte) error {
	sym := excnats.SymbolToken(symbol)
	msgID := fmt.Sprintf("s%d-%d", c.shardID, engineSeq)
	for _, stream := range []string{"trades", "settlements"} {
		subj, err := excnats.Subject(stream, uint32(c.shardID), sym)
		if err != nil {
			c.repubDropped.Add(1)
			c.logf("fill republish dead-letter trade %d stream %s symbol %q: %v",
				tradeID, stream, sym, err)
			continue
		}
		if err := ar.PublishEventAsync(subj, msgID, raw); err != nil {
			return fmt.Errorf("fill republish %s trade %d: %w", subj, tradeID, err)
		}
		c.republished.Add(1)
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

// sameCommittedFill is the replay-vs-collision discriminator on the
// dedup path (spec §2.7). processed_trades keys on the engine's per-shard
// trade counter, which is only monotone while the journal tail survives
// a restart — a boot that replays no TRADE rows (snapshot-covered span,
// aux-only adoption, a lost journal generation) re-issues ids the ledger
// already committed, and a bare ON CONFLICT would then silently drop a
// NEW fill while the read model marks its orders FILLED.
//
// A dedup-suppressed fill is therefore proven a replay by content: the
// committed row's persisted raw_frame (migration 281) must decode to the
// same fill — same id, same legs, same price and quantity. Frame seq is
// excluded on purpose: it is stamped at emit time, and boot recovery
// legitimately rebuilds it from the journal seq (fill_recover.go), so it
// differs on honest replays. A divergent conflict — or a committed row
// with no frame to prove against (pre-281) — is a counter regression,
// and the caller fails closed rather than swallowing the fill.
func sameCommittedFill(stored []byte, f EngineFill) (same bool) {
	defer func() {
		// Malformed stored bytes can panic FlatBuffers accessors — an
		// unprovable replay is a collision, not a crash.
		if r := recover(); r != nil {
			same = false
		}
	}()
	if len(stored) == 0 {
		return false
	}
	body, _, _ := tracing.StripAeronTrace(stored)
	ev := ipc.DecodeEvent(body)
	if ev == nil || ev.TypeType() != wire.EventTypeTradeFill {
		return false
	}
	tf := ipc.EventTradeFill(ev)
	if tf == nil {
		return false
	}
	return tf.TradeId() == f.TradeID &&
		tf.BuyOrderId() == f.BuyOrderID &&
		tf.SellOrderId() == f.SellOrderID &&
		decimal.NewFromScaled(tf.Price()).Equal(f.Price) &&
		decimal.NewFromScaled(tf.Qty()).Equal(f.Qty)
}
