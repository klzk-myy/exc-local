// Delayed-release gate — shared by the liquidation feed (Task 6.3.13,
// mandatory 2s anti-front-running delay, §24 #263) and the block-trade
// tape (Task 6.3.20, MiFID II deferred publication).
//
// Invariant: an item is NEVER emitted before its release time — the gate
// is the only publication path, emission happens strictly after the
// delay elapses, and Push → emit ordering is preserved for equal
// release times (FIFO within a deadline).
//
// Release anchoring: releaseAt = min(event_ts, now) + delay. An event
// that has already aged past its delay in transit (JetStream redelivery,
// consumer restart) releases immediately rather than being held again;
// an event with a future timestamp anchors at now (conservative — a
// corrupt far-future timestamp cannot hold an item hostage, nor can it
// jump the queue ahead of its delay).
package marketdata

import (
	"container/heap"
	"context"
	"sync"
	"sync/atomic"
	"time"
)

// delayItem is one queued publication.
type delayItem[T any] struct {
	v         T
	releaseAt time.Time
	order     uint64 // FIFO tiebreak for equal release times
}

// delayHeap is a min-heap ordered by (releaseAt, order).
type delayHeap[T any] []delayItem[T]

func (h delayHeap[T]) Len() int { return len(h) }
func (h delayHeap[T]) Less(i, j int) bool {
	if h[i].releaseAt.Equal(h[j].releaseAt) {
		return h[i].order < h[j].order
	}
	return h[i].releaseAt.Before(h[j].releaseAt)
}
func (h delayHeap[T]) Swap(i, j int) { h[i], h[j] = h[j], h[i] }
func (h *delayHeap[T]) Push(x any)   { *h = append(*h, x.(delayItem[T])) }
func (h *delayHeap[T]) Pop() any {
	old := *h
	n := len(old)
	it := old[n-1]
	*h = old[:n-1]
	return it
}

// DelayGate holds items for a fixed delay then emits them FIFO-ordered
// by release time. Push is goroutine-safe; exactly one Run loop owns the
// emit side.
type DelayGate[T any] struct {
	delay time.Duration
	now   func() time.Time
	emit  func(T)

	mu   sync.Mutex
	h    delayHeap[T]
	seq  uint64
	wake chan struct{} // cap-1: re-arm the timer when an earlier item lands

	held     atomic.Int64 // current queue depth
	released atomic.Int64
}

// NewDelayGate builds a gate with the given hold time. emit is invoked
// from the Run goroutine in release order. now is injectable for tests;
// nil → time.Now.
func NewDelayGate[T any](delay time.Duration, emit func(T), now func() time.Time) *DelayGate[T] {
	if now == nil {
		now = time.Now
	}
	return &DelayGate[T]{
		delay: delay, now: now, emit: emit,
		wake: make(chan struct{}, 1),
	}
}

// Push queues an item anchored at its event time. A zero ts anchors at
// the arrival time (now+delay — the delay still applies in full).
func (g *DelayGate[T]) Push(v T, ts time.Time) {
	anchor := g.now()
	if !ts.IsZero() && ts.Before(anchor) {
		anchor = ts // already-aged events release earlier, never later
	}
	release := anchor.Add(g.delay)
	g.mu.Lock()
	g.seq++
	heap.Push(&g.h, delayItem[T]{v: v, releaseAt: release, order: g.seq})
	g.mu.Unlock()
	g.held.Add(1)
	select {
	case g.wake <- struct{}{}:
	default:
	}
}

// Pending reports the current queue depth.
func (g *DelayGate[T]) Pending() int64 { return g.held.Load() }

// Released reports the total emitted count.
func (g *DelayGate[T]) Released() int64 { return g.released.Load() }

// Run emits due items until ctx is cancelled. Items still queued at
// cancellation are dropped (the feed is restart-unsafe by design — the
// upstream stream re-delivers through JetStream on reconnect).
func (g *DelayGate[T]) Run(ctx context.Context) error {
	timer := time.NewTimer(time.Hour)
	if !timer.Stop() {
		<-timer.C
	}
	for {
		// Drain every due item, then compute the next wake.
		g.mu.Lock()
		now := g.now()
		var due []T
		for len(g.h) > 0 && !g.h[0].releaseAt.After(now) {
			due = append(due, heap.Pop(&g.h).(delayItem[T]).v)
		}
		var wait time.Duration
		armed := false
		if len(g.h) > 0 {
			wait = g.h[0].releaseAt.Sub(now)
			if wait < 0 {
				wait = 0
			}
			armed = true
		}
		g.mu.Unlock()

		for _, v := range due {
			g.emit(v)
			g.held.Add(-1)
			g.released.Add(1)
		}

		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		if armed {
			timer.Reset(wait)
		}

		if !armed {
			// Parked: wait for a push or cancellation.
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-g.wake:
			}
			continue
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-g.wake:
		case <-timer.C:
		}
	}
}
