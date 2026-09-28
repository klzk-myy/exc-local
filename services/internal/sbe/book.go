// Reference client-side book state, fed by the assembler's emitted
// envelopes. Doubles as the Wave-2 convergence verifier: apply every
// envelope in order and the reconstructed book equals the engine book
// (Task 6.3.6 AC: "converge to the engine book exactly").
package sbe

import "sort"

// BookLevel is one price level.
type BookLevel struct {
	PriceTicks int64
	QtyLots    uint64
}

// Book is one instrument's reconstructed L2 state (unsorted maps — call
// BidsSorted/AsksSorted for ordered views).
type Book struct {
	Bids map[int64]uint64 // priceTicks → qtyLots
	Asks map[int64]uint64
	// Status is the last seen trading status; Invalid until a
	// SecurityStatus/SecurityDefinition message arrives.
	Status    TradingStatus
	HasStatus bool
	LastTrade *Trade
}

// BookKeeper maintains per-instrument reconstructed books from an emitted
// stream. Snapshot bursts clear instrument state at SnapshotBegin so a
// snapshot after loss produces exactly the provider's state.
type BookKeeper struct {
	Books map[uint32]*Book
}

// NewBookKeeper returns an empty keeper.
func NewBookKeeper() *BookKeeper {
	return &BookKeeper{Books: make(map[uint32]*Book)}
}

// Apply folds one emitted envelope into state.
func (k *BookKeeper) Apply(e Envelope) {
	switch m := e.Msg.(type) {
	case SnapshotMarker:
		if m.Type == SnapshotBegin {
			if m.InstrumentID == 0 {
				// Whole-channel snapshot rebuild clears all state.
				k.Books = make(map[uint32]*Book)
			} else {
				// Per-instrument rebuild replaces that book wholesale.
				k.Books[m.InstrumentID] = &Book{Bids: map[int64]uint64{}, Asks: map[int64]uint64{}}
			}
		}
	case BookUpdate:
		b := k.book(m.InstrumentID)
		side := b.Bids
		if m.Side == SideAsk {
			side = b.Asks
		}
		switch m.Action {
		case BookActionUpdate:
			side[m.PriceTicks] = m.QtyLots
		case BookActionDelete:
			delete(side, m.PriceTicks)
		}
	case Trade:
		b := k.book(m.InstrumentID)
		tr := m
		b.LastTrade = &tr
	case SecurityStatus:
		b := k.book(m.InstrumentID)
		b.Status, b.HasStatus = m.Status, true
	case SecurityDefinition:
		b := k.book(m.InstrumentID)
		b.Status, b.HasStatus = m.Status, true
	}
}

func (k *BookKeeper) book(id uint32) *Book {
	b, ok := k.Books[id]
	if !ok {
		b = &Book{Bids: map[int64]uint64{}, Asks: map[int64]uint64{}}
		k.Books[id] = b
	}
	return b
}

// BidsSorted returns bid levels descending by price (best first).
func (b *Book) BidsSorted() []BookLevel {
	out := sortedLevels(b.Bids)
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

// AsksSorted returns ask levels ascending by price (best first).
func (b *Book) AsksSorted() []BookLevel {
	return sortedLevels(b.Asks)
}

func sortedLevels(m map[int64]uint64) []BookLevel {
	out := make([]BookLevel, 0, len(m))
	for p, q := range m {
		out = append(out, BookLevel{PriceTicks: p, QtyLots: q})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].PriceTicks < out[j].PriceTicks })
	return out
}

// EqualBooks reports deep equality of two books' level state (test and
// convergence-check helper — ignores status/trade fields).
func EqualBooks(a, b *Book) bool {
	if a == nil || b == nil {
		return a == b
	}
	return equalLevelMap(a.Bids, b.Bids) && equalLevelMap(a.Asks, b.Asks)
}

func equalLevelMap(a, b map[int64]uint64) bool {
	if len(a) != len(b) {
		return false
	}
	for p, q := range a {
		if bq, ok := b[p]; !ok || bq != q {
			return false
		}
	}
	return true
}
