// Task 6.3.21 item 3 — feed-failover seam. The SBE A/B multicast
// arbitration, TCP replay and snapshot recovery machinery belongs to
// the marketdata service (Tasks 6.3.6/6.3.24); what THIS package owns is
// the client-facing half: the advertised alternate-endpoint list and the
// health-transition broadcast that tells WS consumers to fail over.
// Everything here is interface-level plumbing — the marketdata service
// is the only caller of SetFeedState.
package ws

// FeedState is the upstream feed health the marketdata service reports.
type FeedState int32

const (
	FeedHealthy  FeedState = iota // normal operation
	FeedDegraded                  // one leg lost / partial staleness
	FeedDown                      // dual-loss — clients should fail over
)

func (f FeedState) String() string {
	switch f {
	case FeedHealthy:
		return "healthy"
	case FeedDegraded:
		return "degraded"
	case FeedDown:
		return "down"
	}
	return "unknown"
}

// FailoverConfig wires the client-facing failover surface.
type FailoverConfig struct {
	// Endpoints is the ordered client-facing alternate WS endpoint list
	// advertised in feed.failover advisories and merged into
	// server.shutdown drain advisories (preferred target first).
	Endpoints []string
	// OnTransition runs asynchronously after each SetFeedState change —
	// the seam for marketdata-side bookkeeping/alerts. Must not block.
	OnTransition func(prev, cur FeedState)
}

// FeedState returns the last health state set through SetFeedState.
func (s *Server) FeedState() FeedState {
	return FeedState(s.feedState.Load())
}

// SetFeedState records a feed health transition and, when Failover
// carries endpoints, broadcasts a {"type":"feed.failover",...} advisory
// to every connected client. Repeat calls with the same state are
// no-ops — the advisory is a transition edge, not a heartbeat.
func (s *Server) SetFeedState(st FeedState) {
	prev := FeedState(s.feedState.Swap(int32(st)))
	if prev == st {
		return
	}
	if fn := s.cfg.Failover.OnTransition; fn != nil {
		go fn(prev, st)
	}
	if len(s.cfg.Failover.Endpoints) == 0 {
		return
	}
	b, err := marshalFrame(feedStatusFrame{
		Type:      "feed.failover",
		State:     st.String(),
		Endpoints: s.cfg.Failover.Endpoints,
		TsMs:      s.cfg.Now().UnixMilli(),
	})
	if err == nil {
		s.broadcast(b)
	}
}
