package bridge

import "sync"

// bufferedEvent is one engine event awaiting publication. Publish targets
// are precomputed at ingest so the drain loop does no flatbuffers work.
type bufferedEvent struct {
	subjects []string // JetStream subjects, e.g. trades.0.EUR-USD
	msgID    string   // Nats-Msg-Id for stream dedup: "s<shard>-<seq>"
	payload  []byte   // flatbuffers Event bytes (owned copy)
	seq      uint64   // engine event seq (diagnostics)
}

// eventBuffer is a bounded FIFO ring shared by the ingest goroutine
// (Push, called from the Aeron FragmentHandler) and the egress goroutine
// (Head/Pop, the drain loop). Push never blocks: at capacity the oldest
// event is evicted (drop-oldest) so the engine is never stalled by
// cold-path backpressure (spec §2.3.1). The mutex is held only for pointer
// bookkeeping — payload copies happen before Push, publishes after Head.
//
// gen increments on every head change (Pop or eviction). The drain loop
// compares gen across publish retries to detect that the event it was
// fanning out was evicted mid-flight and restart on the new head.
type eventBuffer struct {
	mu     sync.Mutex
	events []bufferedEvent
	head   int // index of oldest live element
	len    int
	gen    uint64
}

func newEventBuffer(capacity int) *eventBuffer {
	if capacity < 1 {
		capacity = 1
	}
	return &eventBuffer{events: make([]bufferedEvent, capacity)}
}

// Push appends e, evicting the oldest event when full. Returns true when an
// eviction happened.
func (b *eventBuffer) Push(e bufferedEvent) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	evicted := false
	if b.len == len(b.events) {
		b.events[b.head] = bufferedEvent{} // release payload reference
		b.head = (b.head + 1) % len(b.events)
		b.len--
		b.gen++
		evicted = true
	}
	b.events[(b.head+b.len)%len(b.events)] = e
	b.len++
	return evicted
}

// Head returns a copy of the oldest event and the current generation, or
// ok=false when empty. The copy shares subjects/payload (immutable after
// Push); gen lets the caller detect head eviction before Pop.
func (b *eventBuffer) Head() (e bufferedEvent, gen uint64, ok bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.len == 0 {
		return bufferedEvent{}, b.gen, false
	}
	return b.events[b.head], b.gen, true
}

// HeadGen returns the current head generation.
func (b *eventBuffer) HeadGen() uint64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.gen
}

// Pop removes the oldest event after it has been fully published.
func (b *eventBuffer) Pop() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.len == 0 {
		return
	}
	b.events[b.head] = bufferedEvent{} // release payload reference
	b.head = (b.head + 1) % len(b.events)
	b.len--
	b.gen++
}

// PopIfGen removes the head only if gen still matches — i.e. the caller's
// Head() result wasn't invalidated by an intervening eviction or Pop.
func (b *eventBuffer) PopIfGen(gen uint64) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.len == 0 || b.gen != gen {
		return false
	}
	b.events[b.head] = bufferedEvent{}
	b.head = (b.head + 1) % len(b.events)
	b.len--
	b.gen++
	return true
}

// Len is the number of buffered events.
func (b *eventBuffer) Len() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.len
}
