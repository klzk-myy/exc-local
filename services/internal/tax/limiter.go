// limiter.go — Phase-20 Task 20.3.10: per-account daily tax-report
// generation cap (5 per UTC day).
//
// Redis INCR + PEXPIRE, the same Lua shape as orders/batchrl.go: the
// key names the UTC day so the counter lapses at midnight without a
// sweeper. Read-modify errors fail closed — a limiter that silently
// reports "allowed" when its store is down is not a limiter.
package tax

import (
	"context"
	"errors"
	"fmt"
	"time"

	goredis "github.com/redis/go-redis/v9"
)

// TaxReportsPerDay is the task-pinned per-account cap.
const TaxReportsPerDay = 5

// ErrDailyReportLimit marks the cap being reached — the handler maps
// it to RATE_LIMIT_TIER_EXCEEDED (429).
var ErrDailyReportLimit = errors.New("tax: daily report limit reached (5 per UTC day)")

// DailyLimiter gates report issuance. Allow atomically reserves one
// slot and returns the remaining count for headers.
type DailyLimiter interface {
	Allow(ctx context.Context, accountID int64) (remaining int, err error)
}

// incrReportScript is the batchrl.lua shape: INCR; first touch sets the
// day-boundary TTL; over-limit rolls the counter back so rejected
// requests do not consume the budget.
var incrReportScript = goredis.NewScript(`
local count = redis.call('INCR', KEYS[1])
if count == 1 then
  redis.call('PEXPIRE', KEYS[1], ARGV[1])
end
if count > tonumber(ARGV[2]) then
  redis.call('DECR', KEYS[1])
  return -1
end
return tonumber(ARGV[2]) - count
`)

// RedisDailyLimiter implements DailyLimiter over Redis.
type RedisDailyLimiter struct {
	rdb *goredis.Client
	max int
	now func() time.Time
}

// NewRedisDailyLimiter wires the limiter; max<=0 → TaxReportsPerDay.
func NewRedisDailyLimiter(rdb *goredis.Client, max int) *RedisDailyLimiter {
	if max <= 0 {
		max = TaxReportsPerDay
	}
	return &RedisDailyLimiter{rdb: rdb, max: max, now: time.Now}
}

// SetClockForTest overrides the clock; tests only.
func (l *RedisDailyLimiter) SetClockForTest(now func() time.Time) { l.now = now }

// Allow reserves one report slot for the account's current UTC day.
// Returns ErrDailyReportLimit when the cap is already spent; store
// errors propagate (fail closed — the handler maps them to
// SERVICE_DEGRADED, never a silent allow).
func (l *RedisDailyLimiter) Allow(ctx context.Context, accountID int64) (int, error) {
	if l == nil || l.rdb == nil {
		return 0, errors.New("tax: report limiter unavailable")
	}
	now := l.now().UTC()
	dayEnd := now.Truncate(24 * time.Hour).Add(24 * time.Hour)
	key := fmt.Sprintf("tax:reports:%d:%s", accountID, now.Format("20060102"))
	ttl := dayEnd.Sub(now)
	if ttl <= 0 {
		ttl = time.Second // never a zero/negative PEXPIRE
	}
	res, err := incrReportScript.Run(ctx, l.rdb,
		[]string{key}, ttl.Milliseconds(), l.max).Int()
	if err != nil {
		return 0, fmt.Errorf("tax: limiter: %w", err)
	}
	if res < 0 {
		return 0, ErrDailyReportLimit
	}
	return res, nil
}
