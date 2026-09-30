// spread_offsets.go — Phase-19 Task 19.3.25 item 2: option spread
// offsets applied pre-SIMM (spec §15.7 double-count rule; Phase-22
// Task 22.3.13).
//
// PHASE-22 STUB — ordering seam, not spread mechanics.
//
// The §15.7 invariant this file pins down: a spread offset may be
// applied exactly once. The correct application point is BEFORE the
// portfolio/SIMM aggregation consumes leg margins — applying it again
// inside the aggregator double-counts the benefit. This file defines
// the typed contract and the single-use guard; Phase-22 supplies the
// per-strategy offset table (Task 22.3.13).
package risk

import (
	"fmt"
	"sync"

	"exchange/pkg/decimal"
)

// SpreadOffset describes one option-spread pair recognized for margin
// offset: two legs of the same account's book whose combined worst-case
// is strictly bounded (e.g., a vertical spread). Phase-22 Task 22.3.13
// enumerates the strategy catalog; Phase-19 ships the mechanics.
type SpreadOffset struct {
	AccountID     int64
	LongLegID     int64 // position ids of the offset pair
	ShortLegID    int64
	OffsetUSD     decimal.Decimal // margin relief granted for the pair
	Strategy      string          // Phase-22 strategy tag (VERTICAL/CALENDAR/…)
	ConsumedBySIM bool            // set once SIMM has applied it — second apply is an error
}

// SpreadOffsetBook is the per-account offset ledger. The single-use
// guard is load-bearing: the §15.7 "never double-counted" rule is an
// ordering invariant, and a stateless helper cannot enforce it across
// two evaluation passes — the book does.
type SpreadOffsetBook struct {
	mu      sync.Mutex
	applied map[string]bool
}

// NewSpreadOffsetBook builds an empty book.
func NewSpreadOffsetBook() *SpreadOffsetBook {
	return &SpreadOffsetBook{applied: make(map[string]bool)}
}

// spreadKey is the canonical pair key — unordered across the two legs.
func spreadKey(o SpreadOffset) string {
	a, b := o.LongLegID, o.ShortLegID
	if a > b {
		a, b = b, a
	}
	return fmt.Sprintf("%d:%d:%d", o.AccountID, a, b)
}

// Apply marks the pair consumed and returns the offset. A second Apply
// for the same pair returns zero — the offset is already reflected in
// the aggregate; re-granting it would violate §15.7.
func (b *SpreadOffsetBook) Apply(o SpreadOffset) decimal.Decimal {
	if !o.OffsetUSD.IsPositive() {
		return decimal.Zero
	}
	k := spreadKey(o)
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.applied[k] {
		return decimal.Zero
	}
	b.applied[k] = true
	return o.OffsetUSD
}

// TotalOffset applies a candidate set once, summing the granted relief.
// It is the pre-SIMM application point the margin aggregator calls.
func (b *SpreadOffsetBook) TotalOffset(offsets []SpreadOffset) decimal.Decimal {
	total := decimal.Zero
	for _, o := range offsets {
		total = total.Add(b.Apply(o))
	}
	return total
}

// Reset clears the book — each fresh margin evaluation builds a new
// book so offsets are re-granted per evaluation, not cumulatively.
func (b *SpreadOffsetBook) Reset() {
	b.mu.Lock()
	b.applied = make(map[string]bool)
	b.mu.Unlock()
}

// SpreadOffsetSource enumerates an account's recognized spread pairs.
// Phase-22 Task 22.3.13 implements this over the derivatives book;
// NullSpreadOffsetSource is the Phase-19 binding (no option book →
// no offsets).
type SpreadOffsetSource interface {
	SpreadOffsets(accountID int64) ([]SpreadOffset, error)
}

// NullSpreadOffsetSource reports no offsets — correct until the Phase-22
// option book exists.
type NullSpreadOffsetSource struct{}

// SpreadOffsets always returns nil.
func (NullSpreadOffsetSource) SpreadOffsets(int64) ([]SpreadOffset, error) {
	return nil, nil
}
