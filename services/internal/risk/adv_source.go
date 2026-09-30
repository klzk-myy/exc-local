// adv_source.go — ADV (average daily volume) sources for the §13.12
// liquidity add-on and the §13.4a/Task-19.3.16-item-6a liquidation
// slicing yardstick.
//
// The Redis mirror (RedisADVSource, markprice.go) is the fast path for
// venues that publish precomputed analytics; PgADVSource is the system
// of record — a trailing 7-day mean of traded notional from the
// partitioned trades table. ChainedADVSource layers them so production
// never starves on an unpopulated mirror, and CachedADVSource bounds
// the per-tick evaluation cost the event-driven engine would otherwise
// impose on PG.
package risk

import (
	"context"
	"sync"
	"time"

	"exchange/pkg/decimal"

	"github.com/jackc/pgx/v5/pgxpool"
)

// PgADVSource computes trailing ADV from the trades ledger: the mean of
// daily traded notional over the last 7 days (ADV denominates in
// notional — quantity × price — because both consumers compare against
// position notional, never contract count). An instrument with no fills
// in the window returns zero, and the pessimistic legs downstream
// (margin add-on) stay armed.
type PgADVSource struct {
	Pool *pgxpool.Pool
	// WindowDays defaults to 7 when ≤ 0.
	WindowDays int
}

func (s PgADVSource) ADV(ctx context.Context, instrumentID int64) (decimal.Decimal, error) {
	days := s.WindowDays
	if days <= 0 {
		days = 7
	}
	var v string
	err := s.Pool.QueryRow(ctx, `
		SELECT COALESCE(SUM(quantity * price) / $2::numeric, 0)::text
		  FROM trades
		 WHERE instrument_id = $1
		   AND created_at >= now() - ($2 || ' days')::interval`,
		instrumentID, days).Scan(&v)
	if err != nil {
		return decimal.Zero, err
	}
	return decimal.RequireFromString(v), nil
}

var _ ADVSource = PgADVSource{}

// ChainedADVSource tries sources in order and returns the first
// positive result; a source that errors or has no data falls through to
// the next. An all-missing chain returns zero — the pessimistic leg is
// downstream policy, not hidden here.
type ChainedADVSource []ADVSource

func (c ChainedADVSource) ADV(ctx context.Context, instrumentID int64) (decimal.Decimal, error) {
	var lastErr error
	for _, src := range c {
		if src == nil {
			continue
		}
		v, err := src.ADV(ctx, instrumentID)
		if err != nil {
			lastErr = err
			continue
		}
		if v.IsPositive() {
			return v, nil
		}
	}
	if lastErr != nil {
		return decimal.Zero, lastErr
	}
	return decimal.Zero, nil
}

var _ ADVSource = ChainedADVSource(nil)

// CachedADVSource memoizes ADV reads per instrument for ttl — the
// event-driven margin engine evaluates on every mark tick and a
// position-level PG aggregate per tick is a self-DoS. ADV moves slowly
// (trailing 7-day mean), so a 60s staleness bound is honest.
type CachedADVSource struct {
	Src ADVSource
	TTL time.Duration
	Now func() time.Time // nil → UTC wall clock

	mu    sync.Mutex
	cache map[int64]advCacheEntry
}

type advCacheEntry struct {
	v       decimal.Decimal
	expires time.Time
}

var _ ADVSource = (*CachedADVSource)(nil)

func (c *CachedADVSource) ADV(ctx context.Context, instrumentID int64) (decimal.Decimal, error) {
	now := time.Now().UTC()
	if c.Now != nil {
		now = c.Now()
	}
	c.mu.Lock()
	if c.cache == nil {
		c.cache = map[int64]advCacheEntry{}
	}
	if e, ok := c.cache[instrumentID]; ok && now.Before(e.expires) {
		c.mu.Unlock()
		return e.v, nil
	}
	c.mu.Unlock()

	v, err := c.Src.ADV(ctx, instrumentID)
	if err != nil {
		return decimal.Zero, err // errors never cache — next caller retries
	}
	ttl := c.TTL
	if ttl <= 0 {
		ttl = time.Minute
	}
	c.mu.Lock()
	c.cache[instrumentID] = advCacheEntry{v: v, expires: now.Add(ttl)}
	c.mu.Unlock()
	return v, nil
}
