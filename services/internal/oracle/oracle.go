// Package oracle is the Phase-19.5 Price Oracle & Mark Price Service
// (spec §15.3, §19.5 operational row, §24 #45/#120/#134/#196/#321).
//
// The service aggregates independent vendor feeds (Refinitiv, Bloomberg
// BFIX, ECB reference rates — feeds/ adapters are env-configured; the
// Sim feed covers dev/tests), computes mark (median of ≥2 fresh feeds)
// and index (volume-weighted) prices every second, enforces the 5s
// staleness gate fail-closed, drops divergent outliers (>25 bps), and
// publishes to the Redis keyspaces contracted by consumers:
//
//	mark:{symbol}              — decimal + pub/sub delta (risk engine
//	                             batch MGET contract, Task 19.3.1)
//	mark_price:{symbol}        — §26 traceability row contract
//	oracle:mark:{sym}[:ts]     — 1e8-scaled int + unix-ns (C++
//	                             PriceOracleFeed.hpp, Phase-16 Task 16.3.17)
//	index_price:{symbol}       — §26 traceability row contract
//	oracle:index:{sym}[:ts]    — C++ index trigger contract
//	oracle:staleness:{symbol}  — seconds-since-freshest-quote (§26 row)
//	oracle:health:{symbol}     — OK | DEGRADED | UNAVAILABLE
//	oracle:health              — JSON service-level health summary
//
// plus an optional Aeron publication seam (spec §26: publisher at
// 224.0.1.1:40456) bound through Publisher.Aeron.
package oracle

import (
	"context"
	"fmt"
	"sync"
	"time"

	"exchange/pkg/decimal"
)

// Quote is one feed's observation of a symbol. Weight is the feed's
// liquidity/volume weighting used for the index VWAP; zero weight falls
// back to equal weighting so feeds that carry no size still contribute.
type Quote struct {
	Symbol string
	Mid    decimal.Decimal
	Weight decimal.Decimal
	Ts     time.Time // feed-side stamp; staleness is measured against this
	Feed   string    // feed name for audit/divergence attribution
}

// Feed is a pull-based price source. WS-style feeds poll their internal
// latest-tick cache; REST feeds fetch. Poll MUST return promptly —
// the service bounds it with a per-cycle context deadline.
type Feed interface {
	Name() string
	Poll(ctx context.Context, symbols []string) ([]Quote, error)
}

// FeedHealth is the per-feed operational view (Task 19.5.3.1 step 5).
type FeedHealth struct {
	Name       string
	LastTickAt time.Time // zero = never delivered
	Ticks      int64
	Errors     int64
	Stale      bool
}

// HealthState is the service-level oracle health published for consumers
// (Task 19.5.3.4 cascade + Task 19.5.3.7 staleness interceptor).
type HealthState string

const (
	HealthOK          HealthState = "OK"          // ≥2 fresh feeds everywhere
	HealthDegraded    HealthState = "DEGRADED"    // some symbol short of fresh feeds
	HealthUnavailable HealthState = "UNAVAILABLE" // no symbol has ≥2 fresh feeds
)

// feedEntry is a registered feed plus its health bookkeeping.
type feedEntry struct {
	feed       Feed
	lastTickAt time.Time
	lastBySym  map[string]time.Time // per-symbol freshest quote stamp
	ticks      int64
	errs       int64
}

// Service is the single PriceOracle (Task 19.5.3.4 — one oracle, all
// consumers read from it; no duplicate integrations).
type Service struct {
	mu      sync.RWMutex
	feeds   map[string]*feedEntry // keyed by feed Name()
	symbols []string
	now     func() time.Time

	staleAfter time.Duration // 5s per spec §19.5/§13.1
	divBps     int64         // divergence exclusion threshold, 25 bps

	pub       Publisher                  // Redis/Aeron/CH publication seam
	lastIdx   map[string]decimal.Decimal // last published mark per symbol (flash-crash ref)
	lastIdxAt map[string]time.Time
	fallback  *FallbackTracker // stale-price liquidation ladder (Task 19.5.3.6)
}

// Options configures the oracle service.
type Options struct {
	// Feeds are the registered sources; at least two independent feeds
	// must be configured (spec floor for mark computation).
	Feeds []Feed
	// Symbols is the instrument set to publish marks/indexes for.
	Symbols []string
	// StaleAfter overrides the 5s staleness gate (tests).
	StaleAfter time.Duration
	// DivergenceBps overrides the 25bps outlier threshold (tests).
	DivergenceBps int64
	// Publisher is required — no silent marks.
	Publisher Publisher
	// Now injects the clock (tests).
	Now func() time.Time
}

// NewService wires the oracle. It fails closed at construction when
// fewer than two feeds are configured — a single-source oracle can
// never satisfy the ≥2-fresh contract and must not start.
func NewService(o Options) (*Service, error) {
	if o.Publisher == nil {
		return nil, fmt.Errorf("oracle: Publisher required (fail closed)")
	}
	if len(o.Feeds) < 2 {
		return nil, fmt.Errorf("oracle: need ≥2 independent feeds, got %d", len(o.Feeds))
	}
	stale := o.StaleAfter
	if stale <= 0 {
		stale = StaleAfter
	}
	div := o.DivergenceBps
	if div <= 0 {
		div = DivergenceBps
	}
	now := o.Now
	if now == nil {
		now = time.Now
	}
	s := &Service{
		feeds:      map[string]*feedEntry{},
		symbols:    append([]string(nil), o.Symbols...),
		now:        now,
		staleAfter: stale,
		divBps:     div,
		pub:        o.Publisher,
		lastIdx:    map[string]decimal.Decimal{},
		lastIdxAt:  map[string]time.Time{},
		fallback:   NewFallbackTracker(now),
	}
	for _, f := range o.Feeds {
		if f == nil {
			return nil, fmt.Errorf("oracle: nil feed")
		}
		if _, dup := s.feeds[f.Name()]; dup {
			return nil, fmt.Errorf("oracle: duplicate feed %q", f.Name())
		}
		s.feeds[f.Name()] = &feedEntry{feed: f, lastBySym: map[string]time.Time{}}
	}
	return s, nil
}

// Canonical cadences/thresholds — spec §19.5, §13.1, Task 19.5.3.6/3.7.
const (
	// StaleAfter is the 5-second feed staleness gate.
	StaleAfter = 5 * time.Second
	// DivergenceBps drops a feed whose quote diverges from the cohort
	// median by more than 25 basis points.
	DivergenceBps int64 = 25
	// PublishCadence is the 1s mark/index update frequency.
	PublishCadence = time.Second
	// MinFeeds is the fail-closed floor for mark computation.
	MinFeeds = 2
)

// Symbols returns the configured instrument list.
func (s *Service) Symbols() []string { return append([]string(nil), s.symbols...) }

// FeedHealth returns the per-feed operational snapshot.
func (s *Service) FeedHealth() []FeedHealth {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]FeedHealth, 0, len(s.feeds))
	for _, e := range s.feeds {
		out = append(out, FeedHealth{
			Name:       e.feed.Name(),
			LastTickAt: e.lastTickAt,
			Ticks:      e.ticks,
			Errors:     e.errs,
			Stale:      e.lastTickAt.IsZero() || s.now().Sub(e.lastTickAt) > s.staleAfter,
		})
	}
	return out
}
