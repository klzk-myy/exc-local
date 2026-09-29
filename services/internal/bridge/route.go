package bridge

import (
	"fmt"

	flatbuffers "github.com/google/flatbuffers/go"

	"exchange/internal/ipc"
	"exchange/internal/ipc/wire"
	excnats "exchange/internal/nats"
)

// UnknownSymbol is the subject symbol-token used when an event's
// instrument cannot be resolved (e.g. a TradeFill for an order placed
// before this Bridge started). The event is still published — consumers
// dedup on the JetStream Nats-Msg-Id — but the unrouted counter tracks how
// often the fallback fires.
const UnknownSymbol = "UNKNOWN"

// streamsForEvent is the routing table from engine event types to
// canonical JetStream streams (spec §2.3.1 stream list). One event may fan
// out to several streams; each is published on "{stream}.{shard}.{symbol}".
//
//	TradeFill              -> trades + settlements (settlement consumes fills)
//	OrderNew/Amend/Cancel  -> compliance + analytics (order lifecycle / audit)
//	BookSnapshot           -> analytics (tick capture for ClickHouse)
//	L3OrderEvent (9)       -> l3 (Phase-17 premium order-level feed; the
//	                        row carries instrument_id — the order index
//	                        backstops rows routed before a resolver sync)
//	TimeTick / NONE        -> not republished (inbound-only control messages)
func streamsForEvent(et wire.EventType) []string {
	switch et {
	case wire.EventTypeTradeFill:
		return []string{"trades", "settlements"}
	case wire.EventTypeOrderNew, wire.EventTypeOrderAmend, wire.EventTypeOrderCancel:
		return []string{"compliance", "analytics"}
	case wire.EventTypeBookSnapshot:
		return []string{"analytics"}
	case ipc.EventTypeL3OrderEvent:
		return []string{"l3"}
	default:
		return nil
	}
}

// Resolver maps a wire instrument_id to its symbol token (e.g. 3 ->
// "EUR-USD"). The wire schema carries instrument_id, not the symbol string.
type Resolver interface {
	Symbol(instrumentID uint32) (string, bool)
}

// MapResolver resolves from the configured instruments map.
type MapResolver map[uint32]string

func (r MapResolver) Symbol(instrumentID uint32) (string, bool) {
	s, ok := r[instrumentID]
	return s, ok
}

// fallbackSymbol keeps per-instrument ordering intact even when the
// resolver has no configured entry: "instr-3" is a valid single NATS token.
func fallbackSymbol(instrumentID uint32) string {
	return fmt.Sprintf("instr-%d", instrumentID)
}

// orderIndex maps engine order_id -> symbol token so order-keyed events
// (TradeFill, OrderCancel, OrderAmend carry order ids but no
// instrument_id) can be routed. It is populated from OrderNew events seen
// on the engine's outbound stream (ack echo) and bounded by FIFO eviction —
// engine order ids are monotonic, so evicting the oldest insertion first
// approximates LRU closely enough at a fraction of the cost.
type orderIndex struct {
	m     map[uint64]string
	fifo  []uint64 // insertion order of live keys
	start int      // head index into fifo
	cap   int
}

func newOrderIndex(capacity int) *orderIndex {
	if capacity < 1 {
		capacity = 1
	}
	return &orderIndex{m: make(map[uint64]string, 1024), cap: capacity}
}

func (o *orderIndex) put(orderID uint64, symbol string) {
	if _, exists := o.m[orderID]; exists {
		o.m[orderID] = symbol
		return
	}
	for len(o.m) >= o.cap {
		old := o.fifo[o.start]
		o.start++
		delete(o.m, old)
	}
	// Compact the FIFO tail lazily so it doesn't grow without bound.
	if o.start > 0 && o.start*2 >= len(o.fifo) {
		o.fifo = append([]uint64(nil), o.fifo[o.start:]...)
		o.start = 0
	}
	o.m[orderID] = symbol
	o.fifo = append(o.fifo, orderID)
}

func (o *orderIndex) get(orderID uint64) (string, bool) {
	s, ok := o.m[orderID]
	return s, ok
}

func (o *orderIndex) len() int { return len(o.m) }

// resolveInstrument maps instrument_id to a symbol token via the
// configured resolver, falling back to "instr-<id>" so ordering domains
// stay per-instrument even with an unconfigured id.
func (b *Bridge) resolveInstrument(instrumentID uint32) (string, bool) {
	if s, ok := b.res.Symbol(instrumentID); ok {
		return s, true
	}
	return fallbackSymbol(instrumentID), false
}

// route decodes just enough of the Event envelope (zero-copy — buf aliases
// the Aeron log buffer) to pick target subjects, and maintains the order
// index. Returns nil subjects for events that are not republished.
func (b *Bridge) route(ev *wire.Event) []string {
	streams := streamsForEvent(ev.TypeType())
	if len(streams) == 0 {
		return nil
	}

	var symbol string
	resolved := true
	var t flatbuffers.Table
	switch ev.TypeType() {
	case wire.EventTypeTradeFill:
		tf := ipc.EventTradeFill(ev)
		if tf == nil {
			return nil
		}
		if s, ok := b.oidx.get(tf.BuyOrderId()); ok {
			symbol = s
		} else if s, ok := b.oidx.get(tf.SellOrderId()); ok {
			symbol = s
		} else {
			symbol, resolved = UnknownSymbol, false
		}
	case wire.EventTypeOrderNew:
		on := ipc.EventOrderNew(ev)
		if on == nil {
			return nil
		}
		symbol, resolved = b.resolveInstrument(on.InstrumentId())
		b.oidx.put(on.OrderId(), symbol)
	case wire.EventTypeOrderCancel:
		if !ev.Type(&t) {
			return nil
		}
		oc := &wire.OrderCancel{}
		oc.Init(t.Bytes, t.Pos)
		if s, ok := b.oidx.get(oc.OrderId()); ok {
			symbol = s
		} else {
			symbol, resolved = UnknownSymbol, false
		}
	case wire.EventTypeOrderAmend:
		if !ev.Type(&t) {
			return nil
		}
		oa := &wire.OrderAmend{}
		oa.Init(t.Bytes, t.Pos)
		if s, ok := b.oidx.get(oa.OrderId()); ok {
			symbol = s
		} else {
			symbol, resolved = UnknownSymbol, false
		}
	case wire.EventTypeBookSnapshot:
		if !ev.Type(&t) {
			return nil
		}
		bs := &wire.BookSnapshot{}
		bs.Init(t.Bytes, t.Pos)
		symbol, resolved = b.resolveInstrument(bs.InstrumentId())
	case ipc.EventTypeL3OrderEvent:
		// Phase-17 Task 17.3.2 — the L3 row carries instrument_id, so
		// routing resolves it directly; the OrderNew-fed admission index
		// backstops rows whose instrument_id is zero or unconfigured
		// (e.g. a resolver lagging the feed). Total misses land in the
		// UNKNOWN ordering domain — consumers still get a strictly
		// ordered subject.
		l3, ok := ipc.DecodeL3OrderEvent(ev)
		if !ok {
			return nil
		}
		if s, hit := b.resolveInstrument(l3.InstrumentID); hit {
			symbol = s
		} else if s, hit := b.oidx.get(l3.OrderID); hit {
			symbol = s
		} else {
			symbol, resolved = UnknownSymbol, false
		}
	default:
		return nil
	}

	subjects := make([]string, 0, len(streams))
	for _, stream := range streams {
		subj, err := excnats.Subject(stream, b.cfg.ShardID, symbol)
		if err != nil {
			// Configured symbol is not a valid single NATS token — route to
			// the UNKNOWN ordering domain rather than dropping the event.
			b.log.Warn("bridge: invalid symbol token, rerouting",
				"stream", stream, "symbol", symbol, "err", err)
			subj, err = excnats.Subject(stream, b.cfg.ShardID, UnknownSymbol)
			if err != nil {
				continue // unreachable: constant token
			}
			resolved = false
		}
		subjects = append(subjects, subj)
	}
	if len(subjects) == 0 {
		return nil
	}
	if !resolved {
		b.m.unrouted.Add(1)
	}
	return subjects
}
