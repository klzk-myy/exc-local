// Production SessionStore over the coordination Redis client (spec §4.1
// keyspace). Mirrors the conventions of internal/redis/client.go:
// HSET+PEXPIRE inside MULTI so expiry is never lost to a crash, and Lua
// for multi-step index/refresh operations that must be atomic.
package auth

import (
	"context"
	"fmt"
	"time"

	exchredis "exchange/internal/redis"

	goredis "github.com/redis/go-redis/v9"
)

// redisSessionStore implements SessionStore on the noeviction
// coordination instance (deploy/redis/redis.conf): session state must
// never be evicted.
type redisSessionStore struct {
	c *exchredis.Client
}

// NewRedisSessionStore binds a SessionStore to the coordination client.
func NewRedisSessionStore(c *exchredis.Client) SessionStore {
	return &redisSessionStore{c: c}
}

// sessionIndexAddScript prunes stale members, inserts the new session,
// and evicts the oldest members beyond max — atomically, so a burst of
// concurrent logins can never overshoot the §8.8 cap.
var sessionIndexAddScript = goredis.NewScript(`
redis.call('ZREMRANGEBYSCORE', KEYS[1], '-inf', ARGV[1])
redis.call('ZADD', KEYS[1], ARGV[2], ARGV[3])
redis.call('PEXPIRE', KEYS[1], ARGV[5])
local n = redis.call('ZCARD', KEYS[1])
local max = tonumber(ARGV[4])
if n > max then
  local evict = redis.call('ZRANGE', KEYS[1], 0, n - max - 1)
  for _, m in ipairs(evict) do
    redis.call('ZREM', KEYS[1], m)
  end
  return evict
end
return {}
`)

// sessionIndexMembersScript prunes stale members then returns all ids —
// list path for GET /api/v1/account/sessions.
var sessionIndexMembersScript = goredis.NewScript(`
redis.call('ZREMRANGEBYSCORE', KEYS[1], '-inf', ARGV[1])
return redis.call('ZRANGE', KEYS[1], 0, -1)
`)

// refreshConsumeScript atomically rotates a refresh token:
//
//	refresh:{hash}      → sid  => DEL + mark refresh_used:{hash} → OK
//	refresh_used:{hash} → sid  => reuse detected → REUSED
//	neither             => MISSING
var refreshConsumeScript = goredis.NewScript(`
local sid = redis.call('GET', KEYS[1])
if sid then
  redis.call('DEL', KEYS[1])
  redis.call('SET', KEYS[2], sid, 'PX', ARGV[1])
  return {'OK', sid}
end
local used = redis.call('GET', KEYS[2])
if used then
  return {'REUSED', used}
end
return {'MISSING', ''}
`)

// WriteSession stores session:{sid} with all canonical §4.1 fields and
// arms ttl inside the same MULTI (write+expire can never split).
func (r *redisSessionStore) WriteSession(ctx context.Context, s Session, ttl time.Duration) error {
	fields, err := s.hashFields()
	if err != nil {
		return fmt.Errorf("session encode: %w", err)
	}
	key := "session:" + s.ID
	pipe := r.c.TxPipeline()
	anyFields := make(map[string]any, len(fields))
	for k, v := range fields {
		anyFields[k] = v
	}
	pipe.HSet(ctx, key, anyFields)
	pipe.PExpire(ctx, key, ttl)
	if _, err := pipe.Exec(ctx); err != nil {
		return fmt.Errorf("redis write session: %w", err)
	}
	return nil
}

// ReadSession loads session:{sid}. Missing → (zero,false,nil), already
// the fail-closed branch for callers.
func (r *redisSessionStore) ReadSession(ctx context.Context, sid string) (Session, bool, error) {
	raw, err := r.c.HGetAll(ctx, "session:"+sid).Result()
	if err != nil {
		return Session{}, false, fmt.Errorf("redis read session: %w", err)
	}
	if len(raw) == 0 {
		return Session{}, false, nil
	}
	s, err := sessionFromHash(sid, raw)
	if err != nil {
		return Session{}, false, fmt.Errorf("session decode %s: %w", sid, err)
	}
	return s, true, nil
}

func (r *redisSessionStore) DeleteSession(ctx context.Context, sid string) error {
	if err := r.c.Del(ctx, "session:"+sid).Err(); err != nil {
		return fmt.Errorf("redis delete session: %w", err)
	}
	return nil
}

// IndexAdd runs the atomic prune+insert+evict script. indexTTL bounds the
// index itself at 8h so orphan index keys self-clean.
func (r *redisSessionStore) IndexAdd(ctx context.Context, index, member string, score float64, max int64, cutoff float64) (evicted []string, err error) {
	res, err := sessionIndexAddScript.Run(ctx, r.c.Client,
		[]string{index}, int64(cutoff), int64(score), member, max,
		int64((8*time.Hour + time.Minute).Milliseconds())).StringSlice()
	if err != nil {
		return nil, fmt.Errorf("redis session index add %s: %w", index, err)
	}
	return res, nil
}

func (r *redisSessionStore) IndexRemove(ctx context.Context, index string, members ...string) error {
	if len(members) == 0 {
		return nil
	}
	args := make([]any, 0, len(members))
	for _, m := range members {
		args = append(args, m)
	}
	if err := r.c.ZRem(ctx, index, args...).Err(); err != nil {
		return fmt.Errorf("redis session index remove %s: %w", index, err)
	}
	return nil
}

func (r *redisSessionStore) IndexMembers(ctx context.Context, index string, cutoff float64) ([]string, error) {
	res, err := sessionIndexMembersScript.Run(ctx, r.c.Client,
		[]string{index}, int64(cutoff)).StringSlice()
	if err != nil {
		return nil, fmt.Errorf("redis session index members %s: %w", index, err)
	}
	return res, nil
}

// ConsumeRefresh performs the atomic consume/mark/reuse-check script.
func (r *redisSessionStore) ConsumeRefresh(ctx context.Context, activeKey, usedKey string, usedTTL time.Duration) (string, RefreshConsumeResult, error) {
	res, err := refreshConsumeScript.Run(ctx, r.c.Client,
		[]string{activeKey, usedKey}, usedTTL.Milliseconds()).StringSlice()
	if err != nil {
		return "", RefreshMissing, fmt.Errorf("redis refresh consume: %w", err)
	}
	if len(res) != 2 {
		return "", RefreshMissing, fmt.Errorf("redis refresh consume: bad result %v", res)
	}
	switch res[0] {
	case "OK":
		return res[1], RefreshOK, nil
	case "REUSED":
		return res[1], RefreshReused, nil
	default:
		return "", RefreshMissing, nil
	}
}

func (r *redisSessionStore) SetRefresh(ctx context.Context, key, sid string, ttl time.Duration) error {
	if err := r.c.Set(ctx, key, sid, ttl).Err(); err != nil {
		return fmt.Errorf("redis set refresh: %w", err)
	}
	return nil
}

func (r *redisSessionStore) DeleteRefresh(ctx context.Context, key string) error {
	if err := r.c.Del(ctx, key).Err(); err != nil {
		return fmt.Errorf("redis delete refresh: %w", err)
	}
	return nil
}
