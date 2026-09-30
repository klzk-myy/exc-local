// priority_queue.go — Phase-19 Task 19.3.26: the margin engine's
// in-memory priority queue of margin-level risk.
//
// A hand-rolled indexed binary min-heap keyed by margin_level_pct for
// every account with open positions. container/heap alone is not used
// because the engine needs O(log n) in-place re-prioritization of an
// EXISTING account (the dominant operation — every tick re-evaluates an
// already-queued account), plus O(log n) removal and O(1) peek. The
// index map keeps account → slot so a position update never scans.
//
// Ordering contract: the entry with the SMALLEST margin_level_pct is at
// the root (the most-at-risk account). Accounts whose evaluation
// produced no finite level (used_margin == 0 ⇒ level is +∞) carry
// HasLevel=false and sort after every finite level — they can never
// outrank a truly leveraged account.
//
// Concurrency: the heap is NOT goroutine-safe by design; the owning
// MarginEngine serializes all calls on its event loop (and its own
// mutex for external Peek/Ordered probes). Decimal values are copied in
// — no aliasing.
package risk

import (
	"sort"

	"exchange/pkg/decimal"
)

// MarginQueueEntry is one priority-queue slot: the account and its last
// computed margin level.
type MarginQueueEntry struct {
	AccountID int64
	// Level is margin_level_pct (equity / used_margin × 100). Valid only
	// when HasLevel is true.
	Level decimal.Decimal
	// HasLevel=false encodes +∞ (used_margin == 0). Such entries sort
	// behind every finite level — the conservative end of the queue.
	HasLevel bool
}

// MarginLevelHeap is the indexed min-heap of account margin levels.
type MarginLevelHeap struct {
	items []MarginQueueEntry
	pos   map[int64]int // account → items index
}

// NewMarginLevelHeap returns an empty heap.
func NewMarginLevelHeap() *MarginLevelHeap {
	return &MarginLevelHeap{pos: make(map[int64]int)}
}

// Len reports queue membership.
func (h *MarginLevelHeap) Len() int { return len(h.items) }

// Has reports whether the account is queued.
func (h *MarginLevelHeap) Has(accountID int64) bool {
	_, ok := h.pos[accountID]
	return ok
}

// Get returns the account's current entry.
func (h *MarginLevelHeap) Get(accountID int64) (MarginQueueEntry, bool) {
	i, ok := h.pos[accountID]
	if !ok {
		return MarginQueueEntry{}, false
	}
	return h.items[i], true
}

// Upsert inserts or re-prioritizes the account. A nil level marks the
// +∞ slot (no used margin — sorts last). O(log n).
func (h *MarginLevelHeap) Upsert(accountID int64, level *decimal.Decimal) {
	e := MarginQueueEntry{AccountID: accountID}
	if level != nil {
		e.Level = *level
		e.HasLevel = true
	}
	if i, ok := h.pos[accountID]; ok {
		h.items[i] = e
		h.fix(i)
		return
	}
	h.pos[accountID] = len(h.items)
	h.items = append(h.items, e)
	h.up(len(h.items) - 1)
}

// Remove deletes the account. O(log n); false when absent.
func (h *MarginLevelHeap) Remove(accountID int64) bool {
	i, ok := h.pos[accountID]
	if !ok {
		return false
	}
	h.removeAt(i)
	return true
}

// Peek returns the minimum entry (smallest level) without removing it.
func (h *MarginLevelHeap) Peek() (MarginQueueEntry, bool) {
	if len(h.items) == 0 {
		return MarginQueueEntry{}, false
	}
	return h.items[0], true
}

// PopMin removes and returns the minimum entry.
func (h *MarginLevelHeap) PopMin() (MarginQueueEntry, bool) {
	if len(h.items) == 0 {
		return MarginQueueEntry{}, false
	}
	top := h.items[0]
	h.removeAt(0)
	return top, true
}

// Ordered returns a snapshot of all entries sorted worst-first
// (ascending level; +∞ entries last). Used by the breach drain and by
// monitoring/tests. O(n log n) on a copy — the heap itself is untouched.
func (h *MarginLevelHeap) Ordered() []MarginQueueEntry {
	out := append([]MarginQueueEntry(nil), h.items...)
	sort.Slice(out, func(i, j int) bool { return entryLess(out[i], out[j]) })
	return out
}

// ---------------------------------------------------------------------------
// internals — standard sift-up/sift-down on the indexed array
// ---------------------------------------------------------------------------

// entryLess is the strict weak order: finite levels before +∞, then by
// level ascending, account id as a deterministic tie-break.
func entryLess(a, b MarginQueueEntry) bool {
	if a.HasLevel != b.HasLevel {
		return a.HasLevel // finite < +∞
	}
	if c := a.Level.Cmp(b.Level); c != 0 {
		return c < 0
	}
	return a.AccountID < b.AccountID
}

func (h *MarginLevelHeap) swap(i, j int) {
	h.items[i], h.items[j] = h.items[j], h.items[i]
	h.pos[h.items[i].AccountID] = i
	h.pos[h.items[j].AccountID] = j
}

func (h *MarginLevelHeap) up(i int) {
	for i > 0 {
		parent := (i - 1) / 2
		if !entryLess(h.items[i], h.items[parent]) {
			return
		}
		h.swap(i, parent)
		i = parent
	}
}

func (h *MarginLevelHeap) down(i int) {
	n := len(h.items)
	for {
		l, r := 2*i+1, 2*i+2
		smallest := i
		if l < n && entryLess(h.items[l], h.items[smallest]) {
			smallest = l
		}
		if r < n && entryLess(h.items[r], h.items[smallest]) {
			smallest = r
		}
		if smallest == i {
			return
		}
		h.swap(i, smallest)
		i = smallest
	}
}

// fix re-establishes heap order after an in-place value change: the new
// key may need to move either direction.
func (h *MarginLevelHeap) fix(i int) {
	if i > 0 && entryLess(h.items[i], h.items[(i-1)/2]) {
		h.up(i)
		return
	}
	h.down(i)
}

func (h *MarginLevelHeap) removeAt(i int) {
	n := len(h.items) - 1
	if i != n {
		h.swap(i, n)
	}
	delete(h.pos, h.items[n].AccountID)
	h.items = h.items[:n]
	if i < len(h.items) {
		h.fix(i)
	}
}
