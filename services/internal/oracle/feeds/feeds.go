// Package feeds holds the Phase-19.5 vendor feed adapters (Task
// 19.5.3.1) plus the dev/test Sim feed.
//
// Vendor adapters are pull-polled HTTP clients over env-configured
// endpoints — Refinitiv and Bloomberg BFIX require licensed endpoints
// and credentials (spec §27 honest seam: no vendor credentials ship in
// the repo), ECB publishes a free daily XML reference. All adapters
// implement oracle.Feed so the aggregation service is source-agnostic.
package feeds

import (
	"context"
	"sync"
	"time"

	"exchange/internal/oracle"
	"exchange/pkg/decimal"
)

// SimFeed is the scripted/dev feed — deterministic, no network. Quotes
// are set via Set; Poll returns a fresh-stamped copy of the current
// values for the requested symbols.
type SimFeed struct {
	name string
	mu   sync.RWMutex
	last map[string]oracle.Quote
	// Age, when >0, back-dates every emitted quote (staleness tests).
	Age   time.Duration
	Fails int // next N polls return an error
	now   func() time.Time
}

// NewSimFeed returns a named scripted feed.
func NewSimFeed(name string) *SimFeed {
	return &SimFeed{name: name, last: map[string]oracle.Quote{}, now: time.Now}
}

// Name implements oracle.Feed.
func (f *SimFeed) Name() string { return f.name }

// Set installs a quote for symbol at the current clock.
func (f *SimFeed) Set(symbol string, mid decimal.Decimal) {
	f.SetWeighted(symbol, mid, decimal.NewFromInt(1))
}

// SetWeighted installs a quote with an index weight.
func (f *SimFeed) SetWeighted(symbol string, mid, weight decimal.Decimal) {
	f.mu.Lock()
	f.last[symbol] = oracle.Quote{
		Symbol: symbol, Mid: mid, Weight: weight,
		Ts: f.now(), Feed: f.name,
	}
	f.mu.Unlock()
}

// Clear removes a symbol (feed silence simulation).
func (f *SimFeed) Clear(symbol string) {
	f.mu.Lock()
	delete(f.last, symbol)
	f.mu.Unlock()
}

// Poll implements oracle.Feed.
func (f *SimFeed) Poll(_ context.Context, symbols []string) ([]oracle.Quote, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Fails > 0 {
		f.Fails--
		return nil, errFeedDown
	}
	out := make([]oracle.Quote, 0, len(symbols))
	for _, sym := range symbols {
		q, ok := f.last[sym]
		if !ok {
			continue
		}
		if f.Age > 0 {
			q.Ts = f.now().Add(-f.Age)
		}
		out = append(out, q)
	}
	return out, nil
}
