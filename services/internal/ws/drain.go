// Task 6.3.19 — graceful client drain & server-shutdown advisory
// (spec §24 #289, §2.7/§19.7). A planned shutdown must give clients a
// deterministic reconnect signal before the endpoint disappears:
//
//  1. Drain() flips the draining flag — new upgrades are refused
//     pre-upgrade with 503 MAINTENANCE_MODE.
//  2. Every live conn receives a {"type":"server.shutdown",...} advisory
//     carrying reason, retry_after_ms, reconnect endpoint(s) and the
//     drain deadline_ms.
//  3. New subscriptions and order submissions are refused in-band with
//     MAINTENANCE_MODE; cancels (order.cancel, order.countdown_cancel_all),
//     order.status reads, session upkeep (authenticate/refresh_token),
//     ping/resume and unsubscribe stay available until the deadline so
//     clients can flatten risk before reconnecting.
//  4. At the deadline — or when the registry drains naturally —
//     survivors close with RFC 6455 1001 CONNECTION_DRAINING after their
//     write queues flush. The whole sequence is deadline-bound.
//
// FIX-side parity (FIX `News` countdown/advisory) is Phase-18 Task
// 18.3.x; blue-green ordering (replacement endpoint ready before the
// advisory) is a deployment property — the advisory's endpoint fields
// carry whatever the deployer supplies.
package ws

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// DrainAdvisory parameterizes a planned-shutdown drain.
type DrainAdvisory struct {
	// Reason labels the shutdown for clients/logs — e.g. "maintenance",
	// "deploy". Required for a meaningful advisory; "" defaults to
	// "planned_shutdown".
	Reason string
	// RetryAfter is the client reconnect backoff hint rendered as
	// retry_after_ms (WS _ms convention, spec §10.5 item 5).
	RetryAfter time.Duration
	// Endpoint is the preferred reconnect target (e.g. the blue/green
	// sibling URL); "" when none is known.
	Endpoint string
	// Endpoints lists additional alternates; Config.Failover.Endpoints
	// are merged in ahead of these.
	Endpoints []string
	// Deadline bounds the drain; 0 selects Config.DrainDeadline (15s).
	Deadline time.Duration
}

// errDrainInProgress guards against a second Drain call — drain mode is
// terminal by design.
var errDrainInProgress = errors.New("ws: drain already in progress")

// drainAllowed is the in-drain action allowlist (Task 6.3.19 item 2):
// cancels and session upkeep survive; everything else — subscribe, new
// order placement, amend/replace/test/batch (order placement in any
// clothing) — rejects with MAINTENANCE_MODE.
var drainAllowed = map[string]bool{
	"ping":                       true,
	"unsubscribe":                true,
	"resume":                     true,
	"authenticate":               true, // anon conns may still need auth to cancel
	"refresh_token":              true,
	"order.cancel":               true,
	"order.countdown_cancel_all": true,
	"order.status":               true, // read-only
}

// Draining reports whether the server is in drain mode — the health /
// readiness surface consults this to shed traffic before the advisory.
func (s *Server) Draining() bool { return s.draining.Load() }

// Drain runs the planned-shutdown sequence: refuse new upgrades,
// broadcast the server.shutdown advisory, wait for clients to leave
// (or the deadline), then close survivors with 1001
// CONNECTION_DRAINING after a bounded queue flush. Returns nil once the
// drain protocol completes; a cancelled ctx aborts the wait early but
// still force-closes so no conn outlives the drain.
func (s *Server) Drain(ctx context.Context, adv DrainAdvisory) error {
	if !s.draining.CompareAndSwap(false, true) {
		return errDrainInProgress
	}
	deadline := adv.Deadline
	if deadline <= 0 {
		deadline = s.cfg.DrainDeadline
	}
	end := s.cfg.Now().Add(deadline)

	reason := adv.Reason
	if reason == "" {
		reason = "planned_shutdown"
	}
	endpoints := append([]string{}, s.cfg.Failover.Endpoints...)
	endpoints = append(endpoints, adv.Endpoints...)
	endpoint := adv.Endpoint
	if endpoint == "" && len(endpoints) > 0 {
		endpoint = endpoints[0]
	}
	b, _ := marshalFrame(shutdownFrame{
		Type:         "server.shutdown",
		Reason:       reason,
		RetryAfterMs: adv.RetryAfter.Milliseconds(),
		Endpoint:     endpoint,
		Endpoints:    endpoints,
		DeadlineMs:   end.UnixMilli(),
		TsMs:         s.cfg.Now().UnixMilli(),
	})

	s.mu.Lock()
	s.drainDone = make(chan struct{})
	drained := len(s.conns) == 0
	s.mu.Unlock()
	if drained {
		s.drainOnce.Do(func() { close(s.drainDone) })
		return nil
	}
	s.broadcast(b)

	t := time.NewTimer(deadline)
	defer t.Stop()
	select {
	case <-s.drainDone:
		return nil // clients left on their own before the deadline
	case <-t.C:
	case <-ctx.Done():
	}

	// Deadline reached — evict survivors: queued frames (including the
	// advisory) flush before the close frame inside writePump.
	s.forceCloseAll(websocket.CloseGoingAway, DisconnectServer,
		"CONNECTION_DRAINING")

	// Bounded grace for pumps to flush the advisory + close and for
	// unregister to drain the registry; write deadlines already cap each
	// socket write at 1s.
	grace := time.NewTimer(5 * time.Second)
	defer grace.Stop()
	select {
	case <-s.drainDone:
	case <-grace.C:
	case <-ctx.Done():
		return ctx.Err()
	}
	return ctx.Err()
}

// forceCloseAll closes every registered conn — the drain deadline path.
func (s *Server) forceCloseAll(code int, dr DisconnectReason, reason string) {
	s.mu.Lock()
	conns := make([]*Conn, 0, len(s.conns))
	for c := range s.conns {
		conns = append(conns, c)
	}
	s.mu.Unlock()
	for _, c := range conns {
		c.closeWith(code, dr, reason)
	}
}

// broadcast enqueues one frame on every registered conn. Enqueues run
// concurrently because a wedged conn's enqueue can take up to
// SlowConsumerTimeout before the done/timeout escape — serialized sends
// would let one slow consumer stall the whole advisory.
func (s *Server) broadcast(b []byte) {
	s.mu.Lock()
	conns := make([]*Conn, 0, len(s.conns))
	for c := range s.conns {
		conns = append(conns, c)
	}
	s.mu.Unlock()
	var wg sync.WaitGroup
	for _, c := range conns {
		wg.Add(1)
		go func(c *Conn) {
			defer wg.Done()
			c.enqueue(b)
		}(c)
	}
	wg.Wait()
}
