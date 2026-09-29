package fix

import (
	"sync"
	"time"
)

// tokenBucket is a per-session inbound throttle (spec §9.3
// max_msgs_per_sec): rate = max_msgs_per_sec/sec, burst = one second's
// worth so a client at exactly the cap never false-trips at the
// boundary; anything beyond it is *rejected* by the caller — never
// silently dropped (§24 #137).
type tokenBucket struct {
	mu       sync.Mutex
	rate     float64 // tokens per second
	burst    float64
	tokens   float64
	lastFill time.Time
}

func newTokenBucket(perSec int, now time.Time) *tokenBucket {
	r := float64(perSec)
	if r <= 0 {
		r = 100
	}
	return &tokenBucket{rate: r, burst: r, tokens: r, lastFill: now}
}

// allow consumes one token; false = over the configured rate.
func (b *tokenBucket) allow(now time.Time) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if d := now.Sub(b.lastFill); d > 0 {
		b.tokens += d.Seconds() * b.rate
		if b.tokens > b.burst {
			b.tokens = b.burst
		}
		b.lastFill = now
	}
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// setRate updates the budget live (admin throttle change lands on a
// connected session without a reconnect).
func (b *tokenBucket) setRate(perSec int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	r := float64(perSec)
	if r <= 0 {
		r = 100
	}
	b.rate, b.burst = r, r
	if b.tokens > b.burst {
		b.tokens = b.burst
	}
}

// throttleSet owns the per-session buckets keyed by canonical
// session_id; entries are dropped on disconnect.
type throttleSet struct {
	mu  sync.Mutex
	m   map[string]*tokenBucket
	now func() time.Time
}

func newThrottleSet(now func() time.Time) *throttleSet {
	if now == nil {
		now = time.Now
	}
	return &throttleSet{m: map[string]*tokenBucket{}, now: now}
}

// Allow reports whether sessionID may consume one inbound message at
// the given per-second cap.
func (t *throttleSet) Allow(sessionID string, perSec int) bool {
	t.mu.Lock()
	b, ok := t.m[sessionID]
	if !ok {
		b = newTokenBucket(perSec, t.now())
		t.m[sessionID] = b
	}
	t.mu.Unlock()
	return b.allow(t.now())
}

// SetRate applies a live admin cap change.
func (t *throttleSet) SetRate(sessionID string, perSec int) {
	t.mu.Lock()
	if b, ok := t.m[sessionID]; ok {
		b.setRate(perSec)
	}
	t.mu.Unlock()
}

// Drop releases the session's bucket (called on logout/disconnect).
func (t *throttleSet) Drop(sessionID string) {
	t.mu.Lock()
	delete(t.m, sessionID)
	t.mu.Unlock()
}
