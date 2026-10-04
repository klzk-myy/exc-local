// Task 6.3.2 — DeltaSource adapters.
//
// The engine publishes aggregated book state as FlatBuffers
// wire.BookSnapshot events on the {base}_{shard}_out IPC ring (Aeron
// preferred, shared-memory fallback — internal/ipc) and, via the Task
// 3.3.10 Bridge, on the JetStream `analytics.{shard}.{symbol}` stream.
// Both transports feed the same decoder here so there is exactly one
// wire→BookDelta path.
package marketdata

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	flatbuffers "github.com/google/flatbuffers/go"

	"exchange/internal/ipc"
	"exchange/internal/ipc/wire"
	excnats "exchange/internal/nats"
	"exchange/internal/tracing"
)

// InstrumentResolver maps a wire instrument_id to its display symbol
// (e.g. 3 → "EUR/USD"). bridge.MapResolver satisfies this shape.
type InstrumentResolver interface {
	Symbol(instrumentID uint32) (string, bool)
}

// MapResolver is a convenience resolver for config-driven instrument maps.
type MapResolver map[uint32]string

// Symbol implements InstrumentResolver.
func (r MapResolver) Symbol(id uint32) (string, bool) {
	s, ok := r[id]
	return s, ok
}

// ByteSource yields raw wire payloads until ctx is cancelled. The
// channel is closed on termination; a blocking producer applies natural
// backpressure (ByteSource implementations own their drop policy).
type ByteSource func(ctx context.Context) (<-chan []byte, error)

// WireDeltaSource decodes wire.Event BookSnapshot payloads from a raw
// byte stream into BookDelta — one decode path shared by the IPC and
// JetStream adapters. Non-BookSnapshot events and unresolvable
// instruments are skipped (counted via OnDrop, never fatal — a corrupt
// fragment must not kill the feed).
type WireDeltaSource struct {
	src ByteSource
	res InstrumentResolver
	log *slog.Logger

	// OnDrop observes skipped payloads ("malformed" | "unresolved").
	// Metrics wiring is the caller's seam — nil is fine.
	OnDrop func(reason string)
}

// NewWireDeltaSource binds a byte source + instrument resolver.
func NewWireDeltaSource(src ByteSource, res InstrumentResolver, log *slog.Logger) *WireDeltaSource {
	if log == nil {
		log = slog.Default()
	}
	return &WireDeltaSource{src: src, res: res, log: log}
}

// Deltas implements DeltaSource.
func (s *WireDeltaSource) Deltas(ctx context.Context) (<-chan BookDelta, error) {
	raw, err := s.src(ctx)
	if err != nil {
		return nil, err
	}
	out := make(chan BookDelta, 1024)
	go func() {
		defer close(out)
		for {
			select {
			case <-ctx.Done():
				return
			case b, ok := <-raw:
				if !ok {
					return
				}
				d, ok := s.decode(b)
				if !ok {
					continue
				}
				select {
				case out <- d:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return out, nil
}

// decode converts one wire.Event into a BookDelta.
func (s *WireDeltaSource) decode(buf []byte) (BookDelta, bool) {
	// FlatBuffers decode of hostile input can panic — a malformed
	// fragment must never kill the feed goroutine.
	defer func() {
		if r := recover(); r != nil {
			s.log.Error("marketdata: malformed wire payload", "panic", r)
			if s.OnDrop != nil {
				s.OnDrop("malformed")
			}
		}
	}()
	return s.decodeFrame(buf)
}

func (s *WireDeltaSource) decodeFrame(buf []byte) (BookDelta, bool) {
	if len(buf) < 8 { // uoffset + minimal table — not a valid Event
		if s.OnDrop != nil {
			s.OnDrop("malformed")
		}
		return BookDelta{}, false
	}
	body, _, _ := tracing.StripAeronTrace(buf)
	ev := ipc.DecodeEvent(body)
	if ev.TypeType() != wire.EventTypeBookSnapshot {
		return BookDelta{}, false
	}
	var tab flatbuffers.Table
	if !ev.Type(&tab) {
		if s.OnDrop != nil {
			s.OnDrop("malformed")
		}
		return BookDelta{}, false
	}
	var snap wire.BookSnapshot
	snap.Init(tab.Bytes, tab.Pos)

	sym, ok := s.res.Symbol(snap.InstrumentId())
	if !ok {
		s.log.Warn("marketdata: unresolvable instrument", "instrument_id", snap.InstrumentId())
		if s.OnDrop != nil {
			s.OnDrop("unresolved")
		}
		return BookDelta{}, false
	}
	return bookDeltaFromSnapshot(sym, &snap, int64(ev.Ts())), true
}

// DecodeBookDeltaFrame is the frame-tap entry point for processes that
// already own an out-ring reader (orders.Consumer's WithFrameTap) and
// need the same BookDelta projection outside a DeltaSource — Phase-3
// Task 4 uses it to feed sor.BookViewCache in cmd/gateway. It performs
// no panic guard and no drop accounting — callers inside a guarded
// context (the consumer's handle() recovers) rely on that contract.
func DecodeBookDeltaFrame(buf []byte, res InstrumentResolver) (BookDelta, bool) {
	if len(buf) < 8 {
		return BookDelta{}, false
	}
	body, _, _ := tracing.StripAeronTrace(buf)
	ev := ipc.DecodeEvent(body)
	if ev.TypeType() != wire.EventTypeBookSnapshot {
		return BookDelta{}, false
	}
	var tab flatbuffers.Table
	if !ev.Type(&tab) {
		return BookDelta{}, false
	}
	var snap wire.BookSnapshot
	snap.Init(tab.Bytes, tab.Pos)
	sym, ok := res.Symbol(snap.InstrumentId())
	if !ok {
		return BookDelta{}, false
	}
	return bookDeltaFromSnapshot(sym, &snap, int64(ev.Ts())), true
}

func bookDeltaFromSnapshot(sym string, snap *wire.BookSnapshot,
	tsNs int64) BookDelta {
	d := BookDelta{
		Symbol:    sym,
		EngineSeq: snap.Seq(),
		Ts:        time.Unix(0, tsNs),
	}
	var lv wire.PriceLevel
	for i := 0; i < snap.BidsLength(); i++ {
		if snap.Bids(&lv, i) {
			d.Bids = append(d.Bids, Level{
				Price: lv.Price(), Qty: lv.Qty(), Count: lv.Count(),
			})
		}
	}
	for i := 0; i < snap.AsksLength(); i++ {
		if snap.Asks(&lv, i) {
			d.Asks = append(d.Asks, Level{
				Price: lv.Price(), Qty: lv.Qty(), Count: lv.Count(),
			})
		}
	}
	return d
}

// ---------------------------------------------------------------------------
// Transport adapters
// ---------------------------------------------------------------------------

// FanInDeltaSource multiplexes several DeltaSources into one stream —
// the multi-shard attach path. Sources are polled concurrently; conflation
// keys on symbol so interleaving across shards is safe (each symbol lives
// on exactly one shard per the §5.2 router).
func FanInDeltaSource(sources ...DeltaSource) DeltaSource {
	return fanIn{sources: sources}
}

type fanIn struct{ sources []DeltaSource }

func (f fanIn) Deltas(ctx context.Context) (<-chan BookDelta, error) {
	out := make(chan BookDelta, 2048)
	var wg sync.WaitGroup
	for _, s := range f.sources {
		ch, err := s.Deltas(ctx)
		if err != nil {
			return nil, err
		}
		wg.Add(1)
		go func(ch <-chan BookDelta) {
			defer wg.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case d, ok := <-ch:
					if !ok {
						return
					}
					select {
					case out <- d:
					case <-ctx.Done():
						return
					}
				}
			}
		}(ch)
	}
	go func() { wg.Wait(); close(out) }()
	return out, nil
}

// IPCDeltaSource adapts one shard's {base}_{shard}_out ring
// (internal/ipc Channel, EndpointGateway role — consumer of _out).
// Payloads are copied out of ring memory before queueing (Peek/Consume
// aliasing rules).
func IPCDeltaSource(ch *ipc.Channel, res InstrumentResolver, log *slog.Logger) *WireDeltaSource {
	return NewWireDeltaSource(func(ctx context.Context) (<-chan []byte, error) {
		out := make(chan []byte, 4096)
		go func() {
			defer close(out)
			buf := make([]byte, 64<<10)
			for {
				if ctx.Err() != nil {
					return
				}
				n := ch.Poll(buf)
				switch {
				case n > 0:
					cp := make([]byte, n)
					copy(cp, buf[:n])
					select {
					case out <- cp:
					case <-ctx.Done():
						return
					}
				case n < 0:
					buf = make([]byte, len(buf)*2) // widen scratch once
				default:
					select {
					case <-ctx.Done():
						return
					case <-time.After(200 * time.Microsecond):
					}
				}
			}
		}()
		return out, nil
	}, res, log)
}

// JetStreamDeltaSource adapts a durable pull consumer on the bridge's
// republished stream (typically stream "analytics", filter
// "analytics.{shard}.*" or a per-symbol subject) — the fallback path when
// the engine ring is unavailable. Messages are Ack'ed only after handoff
// to the delta channel; a saturated channel leaves them un-acked so
// AckWait redelivery applies (at-least-once — the downstream seq-gap
// contract absorbs duplicates).
func JetStreamDeltaSource(nc *excnats.Client, stream, durable, filter string,
	res InstrumentResolver, log *slog.Logger) *WireDeltaSource {
	return NewWireDeltaSource(func(ctx context.Context) (<-chan []byte, error) {
		opts := []excnats.ConsumerOption{}
		if filter != "" {
			opts = append(opts, excnats.WithFilterSubject(filter))
		}
		cons, err := nc.EnsureConsumerRetry(ctx, stream, durable, opts...)
		if err != nil {
			return nil, fmt.Errorf("marketdata: jetstream consumer: %w", err)
		}
		out := make(chan []byte, 4096)
		go func() {
			defer close(out)
			for ctx.Err() == nil {
				msgs, err := nc.Fetch(ctx, cons, 256, time.Second)
				if err != nil {
					if ctx.Err() != nil {
						return
					}
					log.Warn("marketdata: jetstream fetch failed — retrying",
						"stream", stream, "err", err)
					select {
					case <-ctx.Done():
						return
					case <-time.After(500 * time.Millisecond):
					}
					continue
				}
				for _, m := range msgs {
					data := make([]byte, len(m.Data()))
					copy(data, m.Data())
					select {
					case out <- data:
						_ = m.Ack()
					case <-ctx.Done():
						_ = m.Nak()
						return
					default:
						// Downstream saturated: leave the message un-acked
						// for redelivery instead of silently dropping.
						_ = m.Nak()
					}
				}
			}
		}()
		return out, nil
	}, res, log)
}
