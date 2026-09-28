// Task 6.3.7 / 6.3.16 / 6.3.21 — abuse-protection and multiplexing
// seams. The per-conn machinery lives in conn.go (token bucket, churn
// window, warning strikes, backpressure monitor); this file carries the
// cross-conn and policy seams.
package ws

import (
	"time"
)

// IPLimiter is the optional IP-level aggregate inbound-rate hook
// (Task 6.3.7): it bounds the combined control-message rate across ALL
// connections from one remote IP, so a NAT-egress flood opening many
// sockets cannot multiply the per-conn ControlRate budget. Production
// wiring may back it with Redis (e.g. a ws:rate:ip:{ip} token bucket)
// for a cluster-wide bound; nil disables the check. The hook runs on
// every inbound data frame — implementations must be fast and must not
// block the read pump.
type IPLimiter interface {
	// Allow consumes one inbound-frame token for ip. retryAfterMs uses
	// the WS _ms duration convention (spec §10.5 item 5).
	Allow(ip string, now time.Time) (retryAfterMs int64, ok bool)
}

// SubAdmitHook vets one channel add beyond the generic MaxSubscriptions
// cap (Task 6.3.16 — the "documented subscription caps" plumbing). The
// marketdata service wires the per-channel-type split (§24 #84: 20 L2 /
// 5 L3 concurrent streams) and the §10.7 symbol-entitlement checks here.
//
// Returns (code, true) to admit; (code, false) rejects the channel with
// the given §23 code — "" falls back to WS_MAX_SUBSCRIPTIONS_EXCEEDED,
// ENTITLEMENT_REQUIRED is the canonical entitlement rejection. The hook
// is invoked under the conn's subMu: it must be cheap and must NOT call
// back into Conn methods that acquire subMu.
type SubAdmitHook func(c *Conn, channel string) (code string, admit bool)

// acceptBucket is one remote IP's reconnect-flood token bucket
// (Task 6.3.21 item 2): ReconnectRate upgrade attempts per
// ReconnectWindow, capacity = rate (one full window of burst).
type acceptBucket struct {
	tokens float64
	at     time.Time
}

// allowReconnect charges one upgrade attempt against ip's bucket.
// Entries idle past a full window are GC'd lazily (at most one sweep
// per window) so the map stays bounded under IP churn.
func (s *Server) allowReconnect(ip string) (retryAfterSec int, ok bool) {
	s.acceptMu.Lock()
	defer s.acceptMu.Unlock()
	now := s.cfg.Now()
	win := s.cfg.ReconnectWindow
	capacity := float64(s.cfg.ReconnectRate)
	rate := capacity / win.Seconds()

	if now.Sub(s.acceptSweep) > win {
		for k, b := range s.accept {
			if now.Sub(b.at) > win {
				delete(s.accept, k)
			}
		}
		s.acceptSweep = now
	}

	b := s.accept[ip]
	if b == nil {
		b = &acceptBucket{tokens: capacity, at: now}
		s.accept[ip] = b
	}
	b.tokens += now.Sub(b.at).Seconds() * rate
	if b.tokens > capacity {
		b.tokens = capacity
	}
	b.at = now
	if b.tokens >= 1 {
		b.tokens--
		return 0, true
	}
	// HTTP surface → retry_after in seconds (spec §10.5 item 5).
	return int((1-b.tokens)/rate) + 1, false
}
