// Redis-backed batch rate limiter — the Task 5.3.32 key
// `rl:batch:{accountId}:{second}` with atomic INCR+PEXPIRE, mirroring
// the coordination client's rate-limit primitive shape. Fail-closed:
// a Redis error bubbles up so the handler decides (the batch endpoint
// treats it as internal error, never silently admits).
package orders

import (
	"context"
	"fmt"
	"time"

	goredis "github.com/redis/go-redis/v9"
)

// BatchRateLimitPerSecond is the default per-account batch admission
// ceiling — 10 batch requests per rolling second (max submit payload
// 10 orders, so a saturated caller is still bounded at ~100 order
// intents/s/account).
const BatchRateLimitPerSecond int64 = 10

var incrBatchScript = goredis.NewScript(`
local count = redis.call('INCR', KEYS[1])
if count == 1 then
  redis.call('PEXPIRE', KEYS[1], ARGV[1])
end
return count
`)

// RedisBatchLimiter implements BatchRateLimiter over the coordination
// Redis (account-scoped key, never global — spec §8.8).
type RedisBatchLimiter struct {
	rdb    *goredis.Client
	limit  int64
	ttl    time.Duration
	prefix string
}

// NewRedisBatchLimiter binds the limiter. limit<=0 uses the default.
func NewRedisBatchLimiter(rdb *goredis.Client, limit int64) *RedisBatchLimiter {
	if limit <= 0 {
		limit = BatchRateLimitPerSecond
	}
	return &RedisBatchLimiter{
		rdb: rdb, limit: limit, ttl: 2 * time.Second, prefix: "rl:batch",
	}
}

// AllowBatch consumes one rl:batch:{accountId}:{second} slot; count >
// limit means refuse.
func (l *RedisBatchLimiter) AllowBatch(ctx context.Context, accountID int64) (bool, error) {
	key := fmt.Sprintf("%s:%d:%d", l.prefix, accountID, time.Now().Unix())
	count, err := incrBatchScript.Run(ctx, l.rdb, []string{key},
		l.ttl.Milliseconds()).Int64()
	if err != nil {
		return false, fmt.Errorf("redis incr batch limit: %w", err)
	}
	return count <= l.limit, nil
}
