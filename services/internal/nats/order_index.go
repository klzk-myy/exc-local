package nats

import "strings"

// SymbolToken normalizes a canonical symbol ("EUR/USD") to its NATS
// subject token ("EUR-USD") — the wire convention Subject() validates.
func SymbolToken(symbol string) string {
	return strings.ReplaceAll(symbol, "/", "-")
}

// OrderIndex maps engine order_id → a routing value (symbol token for
// subject routing; orderAdmission for marketdata's aggressor index) so
// order-keyed wire events (TradeFill/OrderCancel/OrderAmend carry order
// ids but no instrument_id) resolve their ordering domain. It is
// populated from OrderNew events and bounded by FIFO eviction — engine
// order ids are monotonic, so evicting the oldest insertion first
// approximates LRU closely enough at a fraction of the cost.
//
// Shared by the Aeron→JetStream bridge (symbol tokens), the marketdata
// wire sources (admission records), and the gateway's shm-topology
// republisher — one bounded index, three consumers.
type OrderIndex[V any] struct {
	m     map[uint64]V
	fifo  []uint64 // insertion order of live keys
	start int      // head index into fifo
	cap   int
}

func NewOrderIndex[V any](capacity int) *OrderIndex[V] {
	if capacity < 1 {
		capacity = 1
	}
	return &OrderIndex[V]{m: make(map[uint64]V, 1024), cap: capacity}
}

func (o *OrderIndex[V]) Put(orderID uint64, v V) {
	if _, exists := o.m[orderID]; exists {
		o.m[orderID] = v
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
	o.m[orderID] = v
	o.fifo = append(o.fifo, orderID)
}

func (o *OrderIndex[V]) Get(orderID uint64) (V, bool) {
	v, ok := o.m[orderID]
	return v, ok
}

func (o *OrderIndex[V]) Len() int { return len(o.m) }
