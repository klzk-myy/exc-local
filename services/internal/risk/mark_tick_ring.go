// mark_tick_ring.go — in-memory mid-price ring implementing
// TickVolatilitySource for the VolatilityScaler (Task 19.3.28). The
// MarginEngine's MarkObserver fan-out feeds every consumed mark tick
// into the ring; RecentMids serves the trailing lookback window,
// oldest→newest, exactly the realized-vol input contract.
//
// Deliberately RAM-only: restart loses the window, the scaler keeps its
// last multipliers until fresh samples accumulate (no fabricated vol).
package risk

import (
	"context"
	"sync"
	"time"

	"exchange/pkg/decimal"
)

// markTickRingCapacity bounds stored mids per symbol — at a 1s tick
// cadence this is >68 minutes of history, comfortably over the
// scaler's 1h window.
const markTickRingCapacity = 4096

// MarkTickRing implements MarkObserver (feed) + TickVolatilitySource
// (read). Safe for concurrent use.
type MarkTickRing struct {
	mu    sync.Mutex
	ticks map[string][]tickSample
}

type tickSample struct {
	at time.Time
	px decimal.Decimal
}

// NewMarkTickRing builds the empty ring.
func NewMarkTickRing() *MarkTickRing {
	return &MarkTickRing{ticks: map[string][]tickSample{}}
}

// Observe implements MarkObserver — every engine tick appends one mid.
// Non-positive prices are ignored (a ring of garbage is worse than a
// short ring).
func (r *MarkTickRing) Observe(symbol string, price decimal.Decimal, at time.Time) {
	if symbol == "" || !price.IsPositive() {
		return
	}
	r.mu.Lock()
	q := r.ticks[symbol]
	q = append(q, tickSample{at: at, px: price})
	if len(q) > markTickRingCapacity {
		q = q[len(q)-markTickRingCapacity:]
	}
	r.ticks[symbol] = q
	r.mu.Unlock()
}

// RecentMids implements TickVolatilitySource — mids at-or-after `since`,
// oldest→newest. The ring prune happens lazily on read so stale symbols
// accumulate nothing (the ring only grows on Observe).
func (r *MarkTickRing) RecentMids(_ context.Context, symbol string,
	since time.Time) ([]decimal.Decimal, error) {

	r.mu.Lock()
	q := r.ticks[symbol]
	cut := 0
	for cut < len(q) && q[cut].at.Before(since) {
		cut++
	}
	out := make([]decimal.Decimal, 0, len(q)-cut)
	for _, s := range q[cut:] {
		out = append(out, s.px)
	}
	if cut > 0 { // drop pruned head so the slice never grows on reads alone
		r.ticks[symbol] = append([]tickSample(nil), q[cut:]...)
	}
	r.mu.Unlock()
	return out, nil
}

// MarkObservers fans one Observe call out to every bound observer —
// used to feed the stub mark provider AND the tick ring from one seam.
type markObserverFanout []MarkObserver

// Observe implements MarkObserver.
func (f markObserverFanout) Observe(symbol string, price decimal.Decimal, at time.Time) {
	for _, o := range f {
		if o != nil {
			o.Observe(symbol, price, at)
		}
	}
}

// FanOutMarkObservers binds several observers behind one MarkObserver.
func FanOutMarkObservers(obs ...MarkObserver) MarkObserver {
	return markObserverFanout(obs)
}
