package reporting

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"
)

// errNotFound is the miss for MarkDelivered on an unknown id.
type errNotFound int64

func (e errNotFound) Error() string {
	return fmt.Sprintf("reporting: confirmation %d not found", int64(e))
}

// MemConfirmationStore is the in-memory ConfirmationTracker + seedable
// row source for tests (generation is faked by inserting rows directly).
type MemConfirmationStore struct {
	mu   sync.Mutex
	next int64
	rows map[int64]*ConfirmationRecord
}

// NewMemConfirmationStore builds the empty store.
func NewMemConfirmationStore() *MemConfirmationStore {
	return &MemConfirmationStore{next: 1, rows: map[int64]*ConfirmationRecord{}}
}

// Seed inserts a row exactly as the generator would (GENERATED unless
// status set). Returns the assigned id.
func (s *MemConfirmationStore) Seed(r ConfirmationRecord) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := s.next
	s.next++
	r.ConfirmationID = id
	if r.Status == "" {
		r.Status = StatusGenerated
	}
	s.rows[id] = &r
	return id
}

// PendingGenerated implements ConfirmationTracker — oldest-first.
func (s *MemConfirmationStore) PendingGenerated(_ context.Context, limit int) ([]ConfirmationRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []ConfirmationRecord
	for _, r := range s.rows {
		if r.Status == StatusGenerated {
			out = append(out, *r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].GeneratedAt.Before(out[j].GeneratedAt) })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// MarkDelivered implements ConfirmationTracker — GENERATED→DELIVERED
// only (mirrors the sibling semantic).
func (s *MemConfirmationStore) MarkDelivered(_ context.Context, confirmationID int64, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.rows[confirmationID]
	if !ok {
		return errNotFound(confirmationID)
	}
	r.DeliveredAt = &at
	if r.Status == StatusGenerated {
		r.Status = StatusDelivered
	}
	return nil
}

// LatestByTrade implements ConfirmationTracker.
func (s *MemConfirmationStore) LatestByTrade(_ context.Context, tradeID int64) (*ConfirmationRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var best *ConfirmationRecord
	for _, r := range s.rows {
		if r.TradeID != tradeID {
			continue
		}
		if best == nil || r.Version > best.Version {
			cp := *r
			best = &cp
		}
	}
	return best, nil
}

// Row returns a copy of a seeded row (test assertions).
func (s *MemConfirmationStore) Row(id int64) (ConfirmationRecord, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.rows[id]
	if !ok {
		return ConfirmationRecord{}, false
	}
	return *r, true
}
