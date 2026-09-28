// Task 5.3.41 step 3 — gateway circuit breaker.
//
// Go middleware trip monitor: if the upstream (matching engine /
// persistence) error rate — measured on 5xx responses flowing through the
// breaker — exceeds 15% over a rolling 10s window, the breaker opens and
// every request is rejected SERVICE_DEGRADED (HTTP 503) with a Retry-After
// hint. After a 5s backoff the breaker half-opens: one probe request is
// admitted; on success the window resets and the breaker closes, on
// failure it re-opens for another backoff.
//
// This is the fast local layer; the HAProxy >50%/10s breaker of Task
// 5.3.29 step 6 is the deliberate coarse outer layer (remediation #35 —
// layered, not duplicated).
package gateway

import (
	"net/http"
	"sync"
	"time"
)

// BreakerConfig tunes a Breaker. The zero value is not usable — use
// DefaultBreakerConfig.
type BreakerConfig struct {
	// Window is the rolling measurement window (spec: 10s).
	Window time.Duration
	// Threshold is the error-rate trip point (spec: 0.15 = 15%).
	Threshold float64
	// MinSamples is the minimum request count in the window before the
	// error ratio is evaluated — a single early failure must not trip the
	// fleet. Spec is silent; 20 keeps the trip statistically meaningful
	// without delaying it at production QPS.
	MinSamples int
	// Backoff is the open-state hold before a half-open probe (spec: 5s).
	Backoff time.Duration
	// Now injects the clock for tests; nil → time.Now.
	Now func() time.Time
}

// DefaultBreakerConfig returns the spec §2.7/5.3.41 parameters.
func DefaultBreakerConfig() BreakerConfig {
	return BreakerConfig{
		Window:     10 * time.Second,
		Threshold:  0.15,
		MinSamples: 20,
		Backoff:    5 * time.Second,
		Now:        time.Now,
	}
}

type breakerState int

const (
	breakerClosed breakerState = iota
	breakerOpen
	breakerHalfOpen
)

// bucket is one second of the ring.
type bucket struct {
	sec    int64
	total  int
	errors int
}

// Breaker is a self-contained circuit breaker. The zero value is not
// usable — construct via NewBreaker.
type Breaker struct {
	cfg BreakerConfig

	mu        sync.Mutex
	buckets   []bucket // len = window seconds
	state     breakerState
	openedAt  time.Time
	probing   bool      // a half-open probe is in flight
	trippedAt time.Time // last transition to open (for observability)
}

// NewBreaker builds a breaker. cfg.Now==nil → time.Now.
func NewBreaker(cfg BreakerConfig) *Breaker {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	n := int(cfg.Window / time.Second)
	if n < 1 {
		n = 1
	}
	return &Breaker{cfg: cfg, buckets: make([]bucket, n)}
}

// State reports "closed" | "open" | "half_open" for observability.
func (b *Breaker) State() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	switch b.state {
	case breakerOpen:
		return "open"
	case breakerHalfOpen:
		return "half_open"
	default:
		return "closed"
	}
}

// admit decides whether a request may proceed, updating open→half-open
// transitions. Returns false when the breaker is open or a probe is
// already in flight.
func (b *Breaker) admit() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	switch b.state {
	case breakerOpen:
		if b.cfg.Now().Sub(b.openedAt) < b.cfg.Backoff {
			return false
		}
		// Backoff elapsed — admit a single probe.
		b.state = breakerHalfOpen
		b.probing = true
		return true
	case breakerHalfOpen:
		if b.probing {
			return false // one probe at a time
		}
		b.probing = true
		return true
	default:
		return true
	}
}

// observe records a completed request. failed=true counts a 5xx. In
// half-open state the probe result decides: success → closed (window
// reset), failure → re-open.
func (b *Breaker) observe(failed bool) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.state == breakerHalfOpen {
		b.probing = false
		if failed {
			b.state = breakerOpen
			b.openedAt = b.cfg.Now()
			b.trippedAt = b.openedAt
		} else {
			b.state = breakerClosed
			for i := range b.buckets {
				b.buckets[i] = bucket{}
			}
		}
		return
	}

	sec := b.cfg.Now().Unix()
	idx := int(sec % int64(len(b.buckets)))
	if b.buckets[idx].sec != sec {
		b.buckets[idx] = bucket{sec: sec}
	}
	b.buckets[idx].total++
	if failed {
		b.buckets[idx].errors++
	}

	if b.state != breakerClosed {
		return
	}
	var total, errs int
	cutoff := sec - int64(len(b.buckets))
	for _, bk := range b.buckets {
		if bk.sec > cutoff { // only buckets inside the window
			total += bk.total
			errs += bk.errors
		}
	}
	if total >= b.cfg.MinSamples && float64(errs)/float64(total) > b.cfg.Threshold {
		b.state = breakerOpen
		b.openedAt = b.cfg.Now()
		b.trippedAt = b.openedAt
	}
}

// statusTap records the response status for the breaker while passing
// bytes through to the client writer.
type statusTap struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (s *statusTap) WriteHeader(code int) {
	s.status = code
	s.wrote = true
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusTap) Write(p []byte) (int, error) {
	if !s.wrote {
		s.status = http.StatusOK
		s.wrote = true
	}
	return s.ResponseWriter.Write(p)
}

// Unwrap lets http.ResponseController reach the real writer (flusher,
// hijacker for the WS upgrade path).
func (s *statusTap) Unwrap() http.ResponseWriter { return s.ResponseWriter }

// Wrap returns middleware enforcing the breaker. When open (or a probe is
// already in flight) the request is rejected with SERVICE_DEGRADED 503 +
// Retry-After = backoff seconds; responses ≥500 count toward the error
// rate (upstream health proxy — matching-engine and persistence faults
// surface as 5xx through the handlers this wraps).
func (b *Breaker) Wrap(r *Router, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if !b.admit() {
			w.Header().Set("Retry-After", itoaSec(b.cfg.Backoff))
			r.WriteError(w, req, "SERVICE_DEGRADED",
				"gateway circuit breaker open: upstream error rate exceeded threshold",
				map[string]any{"retry_after": int(b.cfg.Backoff / time.Second)})
			return
		}
		tap := &statusTap{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(tap, req)
		b.observe(tap.status >= 500)
	})
}

// itoaSec renders a duration as whole seconds for Retry-After.
func itoaSec(d time.Duration) string {
	n := int(d / time.Second)
	if n < 1 {
		n = 1
	}
	var buf [12]byte
	pos := len(buf)
	for n > 0 {
		pos--
		buf[pos] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[pos:])
}
