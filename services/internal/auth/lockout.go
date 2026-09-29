package auth

// lockout.go — Phase-12 Task 12.3.12 part 1: brute-force auth lockout.
//
// auth_failures:{user_id} counts consecutive authentication failures
// inside a 5-minute sliding window (the counter key's TTL is re-armed on
// each failure — consecutive attempts keep the window alive). The fifth
// consecutive failure sets auth_failures:{user_id}:lock for 15 minutes;
// callers reject the attempt with HTTP 423 + ACCOUNT_LOCKED_AUTH_FAILURES.
//
// Cluster-1 (login handler) contract — call sites:
//   before password check : CheckLock — locked ⇒ 423 (do NOT record).
//   on auth failure       : RecordFailure — locked=true ⇒ 423.
//   on success            : ClearFailures (lock is not cleared by it —
//                           the lock must outlive its TTL; a successful
//                           login elsewhere must not lift a lockout).
//
// Atomicity: the counter increment, window re-arm and lock transition
// run in a single Lua script, so a burst of parallel failures cannot
// overshoot the threshold or produce divergent counters.

import (
	"context"
	"fmt"
	"strconv"
	"time"

	exchredis "exchange/internal/redis"

	goredis "github.com/redis/go-redis/v9"
)

const (
	// LockoutWindow is the §12.6 consecutive-failure counting window.
	LockoutWindow = 5 * time.Minute
	// LockoutThreshold — the fifth consecutive failure locks.
	LockoutThreshold = 5
	// LockoutDuration — 15-minute lock per spec §12.6.
	LockoutDuration = 15 * time.Minute
)

// AuthLockout is the interface cluster-1's login handler consumes
// (LockoutService implements it; tests substitute fakes).
type AuthLockout interface {
	CheckLock(ctx context.Context, userID string) (locked bool, retryAfter time.Duration, err error)
	RecordFailure(ctx context.Context, userID string) (failures int, locked bool, retryAfter time.Duration, err error)
	ClearFailures(ctx context.Context, userID string) error
}

// LockoutService is the Redis-backed counter/lock for login attempts.
type LockoutService struct {
	c        *exchredis.Client
	notifier SecurityEventNotifier
	logf     func(string, ...any)
}

func NewLockoutService(c *exchredis.Client) *LockoutService {
	return &LockoutService{c: c}
}

func (s *LockoutService) WithNotifier(n SecurityEventNotifier) *LockoutService {
	s.notifier = n
	return s
}

func (s *LockoutService) WithLogger(f func(string, ...any)) *LockoutService {
	s.logf = f
	return s
}

func authFailuresKey(userID string) string { return "auth_failures:" + userID }
func authLockKey(userID string) string     { return "auth_failures:" + userID + ":lock" }

// CheckLock reports whether the identifier is inside an active 15-minute
// lock and how long remains (for Retry-After). fail-closed: a Redis
// error surfaces, never a false "not locked".
func (s *LockoutService) CheckLock(ctx context.Context, userID string) (locked bool, retryAfter time.Duration, err error) {
	ttl, err := s.c.PTTL(ctx, authLockKey(userID)).Result()
	if err != nil {
		return false, 0, fmt.Errorf("redis lockout check: %w", err)
	}
	if ttl <= 0 {
		return false, 0, nil
	}
	return true, ttl, nil
}

var lockoutRecordScript = goredis.NewScript(`
local lock = redis.call('PTTL', KEYS[2])
if lock > 0 then
  return {0, lock}
end
local n = redis.call('INCR', KEYS[1])
redis.call('PEXPIRE', KEYS[1], tonumber(ARGV[1]))
if n >= tonumber(ARGV[2]) then
  redis.call('SET', KEYS[2], '1', 'PX', tonumber(ARGV[3]))
  redis.call('DEL', KEYS[1])
  return {n, tonumber(ARGV[3])}
end
return {n, 0}
`)

// RecordFailure counts one failed authentication. When the returned
// locked flag is true the account is now inside the 15-minute lock —
// the caller rejects with 423 ACCOUNT_LOCKED_AUTH_FAILURES and the
// security notification has already been emitted (non-blocking).
func (s *LockoutService) RecordFailure(ctx context.Context, userID string) (failures int, locked bool, retryAfter time.Duration, err error) {
	res, err := lockoutRecordScript.Run(ctx, s.c.Client,
		[]string{authFailuresKey(userID), authLockKey(userID)},
		int64(LockoutWindow.Milliseconds()),
		LockoutThreshold,
		int64(LockoutDuration.Milliseconds())).Int64Slice()
	if err != nil {
		return 0, false, 0, fmt.Errorf("redis lockout record: %w", err)
	}
	if len(res) != 2 {
		return 0, false, 0, fmt.Errorf("redis lockout record: bad result %v", res)
	}
	failures = int(res[0])
	retryAfter = time.Duration(res[1]) * time.Millisecond
	if res[1] > 0 && failures >= LockoutThreshold {
		// The transition only fires when the threshold crossed — not for
		// already-locked attempts (script returns {0, ttl}).
		if s.logf != nil {
			s.logf("auth lockout: %s locked for %s after %d consecutive failures",
				userID, LockoutDuration, failures)
		}
		if s.notifier != nil {
			if nid, nerr := strconv.ParseInt(userID, 10, 64); nerr == nil {
				_ = s.notifier.NotifySecurityEvent(ctx, nid, SecurityEventAuthLocked,
					map[string]any{"failures": failures, "retry_after_s": int(LockoutDuration.Seconds())})
			}
		}
		return failures, true, retryAfter, nil
	}
	if res[1] > 0 {
		return failures, true, retryAfter, nil
	}
	return failures, false, 0, nil
}

// ClearFailures resets the consecutive-failure counter after a
// successful authentication. The active lock key is deliberately NOT
// cleared — lock expiry is time-bound, not success-bound.
func (s *LockoutService) ClearFailures(ctx context.Context, userID string) error {
	if err := s.c.Del(ctx, authFailuresKey(userID)).Err(); err != nil {
		return fmt.Errorf("redis lockout clear: %w", err)
	}
	return nil
}

// LockedError returns the coded 423 rejection for a locked identifier.
func LockedError(retryAfter time.Duration) error {
	return wrapError(CodeAccountLockedAuthFails,
		fmt.Sprintf("account locked after consecutive authentication failures; retry in %ds",
			int(retryAfter.Seconds())+1), nil)
}
