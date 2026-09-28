// Time-based response cache backing the spec §10.3 cache contract
// (book 100ms, trades/ticker/klines 1s, instruments 1min). A mutex-held
// singleflight fetch: concurrent misses on one key collapse into one store
// call. Entries are per-key TTL'd; there is no invalidation hook — TTL
// expiry is the coherence bound documented by the endpoint contract.
package marketapi

import (
	"sync"
	"time"
)

// Cache is a small TTL cache for market-data reads.
type Cache struct {
	mu    sync.Mutex
	items map[string]cacheEntry
	now   func() time.Time
}

type cacheEntry struct {
	val any
	exp time.Time
}

// NewCache returns a cache using the wall clock. now==nil → time.Now.
func NewCache(now func() time.Time) *Cache {
	if now == nil {
		now = time.Now
	}
	return &Cache{items: make(map[string]cacheEntry), now: now}
}

// Do returns the cached value for key when fresh, else runs fn (under the
// cache mutex — one store call per key per TTL window) and caches a
// non-error result for ttl. Errors are never cached: a degraded store
// must not pin a stale success.
func (c *Cache) Do(key string, ttl time.Duration, fn func() (any, error)) (any, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.items[key]; ok && c.now().Before(e.exp) {
		return e.val, nil
	}
	v, err := fn()
	if err != nil {
		return nil, err
	}
	c.items[key] = cacheEntry{val: v, exp: c.now().Add(ttl)}
	return v, nil
}

// Forget evicts key (used by tests and the venue-version seam).
func (c *Cache) Forget(key string) {
	c.mu.Lock()
	delete(c.items, key)
	c.mu.Unlock()
}
