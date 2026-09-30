package oracle

import (
	"context"
	"time"

	"exchange/pkg/decimal"
)

// ---------------------------------------------------------------------------
// Service loop — 1s publish cadence (Task 19.5.3.2 step 3)
// ---------------------------------------------------------------------------

// TickOnce runs one evaluation round: poll every feed, filter
// staleness, exclude divergent outliers, compute + publish mark/index,
// and record per-feed health. Exported for tests and for the systemd
// timer path; Run drives it on PublishCadence.
func (s *Service) TickOnce(ctx context.Context) []MarkResult {
	now := s.now()
	bySymbol := map[string][]Quote{}

	s.mu.Lock()
	for _, e := range s.feeds {
		// Per-feed poll; a slow feed must not starve the round — the
		// caller's ctx carries the service deadline.
		quotes, err := e.feed.Poll(ctx, s.symbols)
		if err != nil {
			e.errs++
			continue
		}
		for _, q := range quotes {
			if q.Feed == "" {
				q.Feed = e.feed.Name()
			}
			if !q.Mid.IsPositive() {
				continue // non-positive quotes never enter a cohort
			}
			bySymbol[q.Symbol] = append(bySymbol[q.Symbol], q)
			e.ticks++
			if q.Ts.After(e.lastTickAt) {
				e.lastTickAt = q.Ts
			}
			if q.Ts.After(e.lastBySym[q.Symbol]) {
				e.lastBySym[q.Symbol] = q.Ts
			}
		}
	}
	s.mu.Unlock()

	results := make([]MarkResult, 0, len(s.symbols))
	for _, sym := range s.symbols {
		qs := bySymbol[sym]
		fresh, stale := filterFresh(qs, now, s.staleAfter)
		kept, dropped := excludeDivergent(fresh, s.divBps)

		res := MarkResult{
			Symbol:    sym,
			At:        now,
			Stale:     feedNames(stale),
			Divergent: dropped,
		}
		var state HealthState
		switch {
		case len(kept) >= MinFeeds:
			c := cohort{quotes: kept}
			res.Mark, res.Index = c.median(), c.vwap()
			res.Fresh = feedNames(kept)
			res.OK = true
			state = HealthOK
			if len(dropped) > 0 {
				state = HealthDegraded // divergent source excluded this round
			}
			s.mu.Lock()
			s.lastIdx[sym], s.lastIdxAt[sym] = res.Mark, now
			s.mu.Unlock()
		case len(fresh) > 0:
			state = HealthDegraded // fresh but short of the ≥2 floor
		default:
			state = HealthUnavailable
		}

		// Publication is fail-closed at the write level: a failed mark
		// write still records health so consumers can gate on it.
		if res.OK {
			if err := s.pub.PublishMark(ctx, res); err != nil {
				res.OK = false
				state = HealthDegraded
			}
		}
		staleSec := staleSeconds(qs, now)
		_ = s.pub.PublishHealth(ctx, sym, state, staleSec)

		// Stale-price fallback (Task 19.5.3.6): the tracker emits the
		// tiered reference while the symbol is stale and clears the
		// fallback keys on recovery.
		if s.fallback != nil {
			if ref := s.fallback.Observe(res, staleSec); ref != nil {
				_ = s.pub.PublishFallback(ctx, ref)
			} else if !res.OK {
				// stale but unanchored (no last mark) — still clear so a
				// resurrected episode can't ride yesterday's reference
				if cp, ok := s.pub.(interface {
					ClearFallback(context.Context, string) error
				}); ok {
					_ = cp.ClearFallback(ctx, sym)
				}
			} else {
				if cp, ok := s.pub.(interface {
					ClearFallback(context.Context, string) error
				}); ok {
					_ = cp.ClearFallback(ctx, sym)
				}
			}
		}
		results = append(results, res)
	}
	return results
}

// Run drives TickOnce on PublishCadence until ctx cancels.
func (s *Service) Run(ctx context.Context) {
	t := time.NewTicker(PublishCadence)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.TickOnce(ctx)
		}
	}
}

// Health returns the worst per-symbol state — the cascade signal
// consumers (margin orders, conditional triggers) gate on.
func (s *Service) Health(ctx context.Context) HealthState {
	symbols := s.Symbols()
	if len(symbols) == 0 {
		return HealthUnavailable
	}
	worst := HealthOK
	for _, sym := range symbols {
		// Freshness is per (feed, symbol): a feed live on EUR/USD but
		// silent on USD/TRY does not count toward USD/TRY's floor.
		s.mu.RLock()
		fresh := 0
		for _, e := range s.feeds {
			if t, ok := e.lastBySym[sym]; ok && s.now().Sub(t) <= s.staleAfter {
				fresh++
			}
		}
		s.mu.RUnlock()
		switch {
		case fresh >= MinFeeds:
			// stays OK for this symbol
		case fresh > 0:
			if worst == HealthOK {
				worst = HealthDegraded
			}
		default:
			worst = HealthUnavailable
		}
	}
	return worst
}

// feedNames extracts feed attributions.
func feedNames(qs []Quote) []string {
	out := make([]string, len(qs))
	for i, q := range qs {
		out[i] = q.Feed
	}
	return out
}

// LastMark exposes the most recent published mark + stamp — the
// flash-crash reference and stale-fallback base (Task 19.5.3.6).
func (s *Service) LastMark(symbol string) (decimal.Decimal, time.Time, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	m, ok := s.lastIdx[symbol]
	if !ok {
		return decimal.Zero, time.Time{}, false
	}
	return m, s.lastIdxAt[symbol], true
}
