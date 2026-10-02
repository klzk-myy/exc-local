// bookview.go — the BookView source for Router.Route (IMP-PLAN Phase-3
// Task 4). The gateway's orders consumer is the engine out-ring's sole
// reader; this cache rides its frame tap (same contract as the
// settlement feed) and keeps the latest top-of-book + visible depth per
// symbol so Submit's SOR consult reads memory, never the ring.
package sor

import (
	"sync"
	"time"

	"exchange/pkg/decimal"
)

// Level is one aggregated book level in 1e8-scaled wire units — the
// marketdata.Level shape duplicated field-for-field so sor never imports
// marketdata (marketdata imports orders, orders imports sor: a sor→
// marketdata edge would close the cycle).
type Level struct {
	Price int64
	Qty   int64
	Count uint32
}

// BookViewStaleness bounds how old an entry may be before View reports
// no-BBO — a dead feed must not anchor routing decisions on phantom
// liquidity. Same 10s order of magnitude the oracle staleness gates use.
const BookViewStaleness = 10 * time.Second

// BookViewCache is the production sor.BookView source: per-symbol latest
// book top + aggregate visible depth, fed by decoded engine book
// snapshots. Safe for concurrent use — Observe runs on the consumer
// goroutine while Route reads from submit goroutines.
type BookViewCache struct {
	mu  sync.RWMutex
	m   map[string]stampedView
	Now func() time.Time // nil → time.Now
}

type stampedView struct {
	v  BookView
	at time.Time
}

func NewBookViewCache() *BookViewCache {
	return &BookViewCache{m: make(map[string]stampedView)}
}

// Observe folds one decoded engine book snapshot into the view —
// the caller (cmd/gateway's frame tap) supplies symbol + levels decoded
// via marketdata.DecodeBookDeltaFrame.
func (c *BookViewCache) Observe(symbol string, bids, asks []Level) {
	if symbol == "" {
		return
	}
	v := BookView{Symbol: symbol}
	var bidDepth, askDepth decimal.Decimal
	if len(bids) > 0 {
		v.BestBid = decimal.NewFromScaled(bids[0].Price)
		for _, lv := range bids {
			bidDepth = bidDepth.Add(decimal.NewFromScaled(lv.Qty))
		}
	}
	if len(asks) > 0 {
		v.BestAsk = decimal.NewFromScaled(asks[0].Price)
		for _, lv := range asks {
			askDepth = askDepth.Add(decimal.NewFromScaled(lv.Qty))
		}
	}
	v.BidDepth = bidDepth
	v.AskDepth = askDepth
	v.HasBBO = len(bids) > 0 || len(asks) > 0

	now := time.Now()
	if c.Now != nil {
		now = c.Now()
	}
	c.mu.Lock()
	c.m[symbol] = stampedView{v: v, at: now}
	c.mu.Unlock()
}

// View returns the live BookView for symbol. Entries older than
// BookViewStaleness report HasBBO=false — the router's
// ShouldRouteExternal then treats the book as empty rather than routing
// off stale numbers (fail-open toward external only makes sense when
// venues exist; with none, Submit never consults the router at all).
func (c *BookViewCache) View(symbol string) BookView {
	c.mu.RLock()
	sv, ok := c.m[symbol]
	c.mu.RUnlock()
	if !ok {
		return BookView{Symbol: symbol}
	}
	now := time.Now()
	if c.Now != nil {
		now = c.Now()
	}
	if now.Sub(sv.at) > BookViewStaleness {
		return BookView{Symbol: symbol}
	}
	return sv.v
}
