// Task 9.3.23 — marketdata WS graceful drain (spec §24 #216, task item 3).
//
// On shutdown the server sends {"type":"system.reconnect","reason":...}
// to every live connection — clients reconnect with their last_seq
// resume cursor per §10.1/Task 6.3.9 — then closes sockets with RFC 6455
// 1001 after write queues flush (the write pump already drains `out`
// before emitting the close frame).
//
// Ordering (uniform service contract):
//  1. draining flag latches → new upgrades refuse 503 MAINTENANCE_MODE;
//  2. advisory broadcast to all live conns;
//  3. deadline-bound wait for voluntary disconnects;
//  4. remaining conns closed 1001, pumps awaited ≤ 5s each.
package marketdata

import (
	"context"
	"encoding/json"
	"time"

	"github.com/gorilla/websocket"
)

// reconnectFrame is the client-facing drain advisory (task item 3).
type reconnectFrame struct {
	Type         string `json:"type"` // "system.reconnect"
	Reason       string `json:"reason"`
	RetryAfterMs int64  `json:"retry_after_ms"`
	DeadlineMs   int64  `json:"deadline_ms,omitempty"`
	TsMs         int64  `json:"ts_ms"`
}

// Draining reports whether the server has begun the shutdown drain —
// readiness and the shed exemption surface consult it.
func (s *Server) Draining() bool { return s.draining.Load() }

// Drain runs the marketdata shutdown sequence. Reason labels the drain
// for clients ("maintenance" per the task contract); deadline bounds the
// voluntary-leave wait (0 → 5s). Idempotent-latched: a second call
// reports immediately once the flag is set.
func (s *Server) Drain(ctx context.Context, reason string, deadline time.Duration) error {
	if !s.draining.CompareAndSwap(false, true) {
		return nil // drain already in progress — terminal, idempotent
	}
	if reason == "" {
		reason = "maintenance"
	}
	if deadline <= 0 {
		deadline = 5 * time.Second
	}
	end := s.cfg.Now().Add(deadline)

	frame, _ := json.Marshal(reconnectFrame{
		Type:         "system.reconnect",
		Reason:       reason,
		RetryAfterMs: 1000,
		DeadlineMs:   end.UnixMilli(),
		TsMs:         s.cfg.Now().UnixMilli(),
	})

	s.mu.Lock()
	conns := make([]*Conn, 0, len(s.conns))
	for c := range s.conns {
		conns = append(conns, c)
	}
	s.mu.Unlock()

	for _, c := range conns {
		c.enqueue(frame)
	}
	// Give write pumps a bounded window to flush the advisory before the
	// close frame queues behind it.
	select {
	case <-ctx.Done():
	case <-time.After(minDur(deadline, 2*time.Second)):
	}

	// Close survivors — closeConn emits the close frame after flushing
	// queued frames; readLoop exits on socket teardown and unregister()
	// does the index cleanup.
	deadlineCh := time.After(deadline)
	for _, c := range conns {
		c.closeConn(websocket.CloseGoingAway, "server draining: "+reason)
		select {
		case <-c.pumpDone:
		case <-deadlineCh:
		case <-ctx.Done():
		}
	}
	return ctx.Err()
}

func minDur(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}
