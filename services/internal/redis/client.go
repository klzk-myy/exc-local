// Package redis provides the go-redis client factory plus typed helpers
// for the spec §4 key schema: sessions, rate limits, leader election,
// account locks, degradation mode, circuit breakers and the global halt
// flag.
//
// Coordination keys live on the noeviction primary (deploy/redis/redis.conf)
// and must never be evicted; evictable cache traffic belongs on the
// redis-cache volatile-lru instance (port 6382 in dev).
//
// Fail-closed contract (spec §2.7): every helper returns errors to the
// caller. Nothing is swallowed; a Redis outage surfaces as an error, never
// as a default/empty value that could be mistaken for healthy state.
package redis

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	goredis "github.com/redis/go-redis/v9"
)

// Canonical TTLs from spec §4 / §18.6.2. The leader lease values are the
// epoch-lease model (remediation #35): 2,000ms TTL refreshed every 500ms —
// the 10s SETNX figures still printed in spec §4.2 are the superseded
// legacy contract and must not be used.
const (
	SessionTTL          = 3600 * time.Second      // session:{token}
	RateLimitTTL        = 2 * time.Second         // rl:{ip}:{second}
	LeaderLeaseTTL      = 2000 * time.Millisecond // engine:leader:{shardId} (§18.6.2)
	LeaderRefreshPeriod = 500 * time.Millisecond  // §18.6.2 heartbeat cadence
	AccountLockTTL      = 10 * time.Second        // account:lock:{accountId}
)

// Key builders (spec §4 naming — single source of truth for key layout).
func sessionKey(token string) string { return "session:" + token }
func rateLimitKey(ip string, sec int64) string {
	return fmt.Sprintf("rl:%s:%d", ip, sec)
}
func leaderKey(shardID int) string    { return fmt.Sprintf("engine:leader:%d", shardID) }
func accountLockKey(id string) string { return "account:lock:" + id }
func circuitBreakerKey(scope, id string) string {
	return fmt.Sprintf("circuit_breaker:%s:%s", scope, id)
}

const (
	keyDegradationMode      = "system:degradation:mode"
	keyDegradationEnteredAt = "system:degradation:entered_at"
	keyDegradationReason    = "system:degradation:reason"
	keyHaltGlobal           = "halt:global"
)

// Client is the coordination-instance client: a pooled *goredis.Client
// extended with the spec §4 typed helpers.
type Client struct {
	*goredis.Client
}

// New returns a Client for the coordination instance with the task's
// connection pooling (PoolSize 20) and conservative context/dial/read/write
// timeouts. The client is lazy: no connection is opened until the first
// command. Callers that must verify connectivity at startup call Ping.
func New(addr, password string, db int) *Client {
	rdb := goredis.NewClient(&goredis.Options{
		Addr:         addr,
		Password:     password,
		DB:           db,
		PoolSize:     20,
		DialTimeout:  5 * time.Second,
		ReadTimeout:  3 * time.Second,
		WriteTimeout: 3 * time.Second,
		PoolTimeout:  4 * time.Second,
	})
	return &Client{Client: rdb}
}

// Ping is the readiness-probe health check: it returns a bare error so
// probes can treat it as a bool.
func (c *Client) Ping(ctx context.Context) error {
	return c.Client.Ping(ctx).Err()
}

// ---------------------------------------------------------------------------
// Sessions — session:{token} HASH TTL 3600s (spec §4.1)
// ---------------------------------------------------------------------------

// Session is the user session record stored in session:{token}.
type Session struct {
	UserID    string `redis:"user_id"`
	AccountID string `redis:"account_id"`
	Tier      string `redis:"tier"`
}

// SetSession writes the session hash with the canonical 3600s TTL.
func (c *Client) SetSession(ctx context.Context, token string, s Session) error {
	return c.setSessionTTL(ctx, token, s, SessionTTL)
}

// setSessionTTL is the TTL-parameterized core; tests use it to verify
// expiry without waiting an hour. HSET+PEXPIRE run inside MULTI so the
// expiry can never be lost to a crash between the two writes.
func (c *Client) setSessionTTL(ctx context.Context, token string, s Session, ttl time.Duration) error {
	key := sessionKey(token)
	pipe := c.TxPipeline()
	pipe.HSet(ctx, key, map[string]any{
		"user_id":    s.UserID,
		"account_id": s.AccountID,
		"tier":       s.Tier,
	})
	pipe.PExpire(ctx, key, ttl)
	_, err := pipe.Exec(ctx)
	if err != nil {
		return fmt.Errorf("redis set session: %w", err)
	}
	return nil
}

// GetSession reads session:{token}. A missing key returns (nil, nil) —
// callers treat a nil session as unauthenticated, which is already the
// fail-closed branch.
func (c *Client) GetSession(ctx context.Context, token string) (*Session, error) {
	var s Session
	if err := c.HGetAll(ctx, sessionKey(token)).Scan(&s); err != nil {
		return nil, fmt.Errorf("redis get session: %w", err)
	}
	if s.UserID == "" && s.AccountID == "" && s.Tier == "" {
		return nil, nil // expired / never existed
	}
	return &s, nil
}

// ---------------------------------------------------------------------------
// Rate limits — rl:{ip}:{second} STRING TTL 2s (spec §4.1)
// ---------------------------------------------------------------------------

// incrRateLimitScript keeps INCR and the 2s window expiry atomic: the
// PEXPIRE is set inside the same Lua invocation that creates the key, so
// a counter can never be left without a TTL.
var incrRateLimitScript = goredis.NewScript(`
local count = redis.call('INCR', KEYS[1])
if count == 1 then
  redis.call('PEXPIRE', KEYS[1], ARGV[1])
end
return count
`)

// IncrRateLimit increments rl:{ip}:{second} and returns the count for this
// window. Callers compare the count against the tier limit; a count of 1
// also arms the 2s window expiry atomically.
func (c *Client) IncrRateLimit(ctx context.Context, ip string, second int64) (int64, error) {
	count, err := incrRateLimitScript.Run(ctx, c.Client,
		[]string{rateLimitKey(ip, second)}, RateLimitTTL.Milliseconds()).Int64()
	if err != nil {
		return 0, fmt.Errorf("redis incr rate limit: %w", err)
	}
	return count, nil
}

// ---------------------------------------------------------------------------
// Leader election — engine:leader:{shardId} STRING (spec §4.2 / §18.6.2)
// ---------------------------------------------------------------------------

// Lease value format is "{token}:{epoch}" per the epoch-lease fencing
// contract: the 64-bit monotonic epoch lets a partitioned ex-primary
// detect that it has been superseded.
func leaderValue(token string, epoch uint64) string {
	return fmt.Sprintf("%s:%d", token, epoch)
}

// TryAcquireLeader attempts SET engine:leader:{shard} {token}:{epoch} NX PX
// ttl. ttl <= 0 falls back to the canonical 2,000ms lease. Returns true
// when the lease was acquired, false when another leader holds it.
func (c *Client) TryAcquireLeader(ctx context.Context, shardID int, token string, epoch uint64, ttl time.Duration) (bool, error) {
	if ttl <= 0 {
		ttl = LeaderLeaseTTL
	}
	ok, err := c.SetNX(ctx, leaderKey(shardID), leaderValue(token, epoch), ttl).Result()
	if err != nil {
		return false, fmt.Errorf("redis acquire leader shard %d: %w", shardID, err)
	}
	return ok, nil
}

// compareAndDelScript releases a token-guarded key: the DEL runs only when
// the stored value still equals ours, so an expired-and-reacquired lease
// can never be deleted out from under the new holder (spec §18.6.2
// mandatory token-checked revocation).
var compareAndDelScript = goredis.NewScript(`
if redis.call('GET', KEYS[1]) == ARGV[1] then
  return redis.call('DEL', KEYS[1])
end
return 0
`)

// compareAndPExpireScript is the refresh half of the lease contract:
// token-checked PEXPIRE so only the current holder can extend the lease.
var compareAndPExpireScript = goredis.NewScript(`
if redis.call('GET', KEYS[1]) == ARGV[1] then
  return redis.call('PEXPIRE', KEYS[1], ARGV[2])
end
return 0
`)

// ReleaseLeader deletes the lease only if it still holds {token}:{epoch}.
// Returns true when the lease was actually released.
func (c *Client) ReleaseLeader(ctx context.Context, shardID int, token string, epoch uint64) (bool, error) {
	n, err := compareAndDelScript.Run(ctx, c.Client,
		[]string{leaderKey(shardID)}, leaderValue(token, epoch)).Int()
	if err != nil {
		return false, fmt.Errorf("redis release leader shard %d: %w", shardID, err)
	}
	return n == 1, nil
}

// RefreshLeader extends the lease TTL only if the caller still holds
// {token}:{epoch} — the 500ms heartbeat renewal of §18.6.2.
func (c *Client) RefreshLeader(ctx context.Context, shardID int, token string, epoch uint64, ttl time.Duration) (bool, error) {
	if ttl <= 0 {
		ttl = LeaderLeaseTTL
	}
	n, err := compareAndPExpireScript.Run(ctx, c.Client,
		[]string{leaderKey(shardID)}, leaderValue(token, epoch), ttl.Milliseconds()).Int()
	if err != nil {
		return false, fmt.Errorf("redis refresh leader shard %d: %w", shardID, err)
	}
	return n == 1, nil
}

// ---------------------------------------------------------------------------
// Account locks — account:lock:{accountId} STRING TTL 10s (spec §4.2)
// ---------------------------------------------------------------------------

// TryLockAccount takes the per-account mutex via SET NX PX. ttl <= 0 uses
// the canonical 10s. The token identifies the holder for safe release.
func (c *Client) TryLockAccount(ctx context.Context, accountID, token string, ttl time.Duration) (bool, error) {
	if ttl <= 0 {
		ttl = AccountLockTTL
	}
	ok, err := c.SetNX(ctx, accountLockKey(accountID), token, ttl).Result()
	if err != nil {
		return false, fmt.Errorf("redis lock account %s: %w", accountID, err)
	}
	return ok, nil
}

// UnlockAccount releases the mutex only if the caller's token still owns
// it (Lua compare-and-del — a timed-out lock reacquired by someone else is
// never deleted by the stale owner).
func (c *Client) UnlockAccount(ctx context.Context, accountID, token string) (bool, error) {
	n, err := compareAndDelScript.Run(ctx, c.Client,
		[]string{accountLockKey(accountID)}, token).Int()
	if err != nil {
		return false, fmt.Errorf("redis unlock account %s: %w", accountID, err)
	}
	return n == 1, nil
}

// ---------------------------------------------------------------------------
// Degradation mode — system:degradation:* (spec §4.2, §2.4)
// ---------------------------------------------------------------------------

// DegradationMode values are the canonical PascalCase set owned by the
// Phase-02 ModeManager (spec §2.4).
type DegradationMode string

const (
	ModeNormal         DegradationMode = "Normal"
	ModeReadOnly       DegradationMode = "ReadOnly"
	ModeMarketDataOnly DegradationMode = "MarketDataOnly"
	ModeSpotOnly       DegradationMode = "SpotOnly"
	ModeThrottled      DegradationMode = "Throttled"
	ModeMaintenance    DegradationMode = "Maintenance"
)

// DegradationState is the full system:degradation:* record.
type DegradationState struct {
	Mode      DegradationMode
	EnteredAt int64  // epoch millis
	Reason    string // free-text cause
}

func validMode(m DegradationMode) bool {
	switch m {
	case ModeNormal, ModeReadOnly, ModeMarketDataOnly,
		ModeSpotOnly, ModeThrottled, ModeMaintenance:
		return true
	}
	return false
}

// GetDegradationMode reads the current degradation record. An absent key
// is Normal (spec §2.4: default no degradation).
func (c *Client) GetDegradationMode(ctx context.Context) (DegradationState, error) {
	mode, err := c.Get(ctx, keyDegradationMode).Result()
	if errors.Is(err, goredis.Nil) {
		return DegradationState{Mode: ModeNormal}, nil
	}
	if err != nil {
		return DegradationState{}, fmt.Errorf("redis get degradation mode: %w", err)
	}
	st := DegradationState{Mode: DegradationMode(mode)}
	if v, err := c.Get(ctx, keyDegradationReason).Result(); err == nil {
		st.Reason = v
	} else if !errors.Is(err, goredis.Nil) {
		return DegradationState{}, fmt.Errorf("redis get degradation reason: %w", err)
	}
	if v, err := c.Get(ctx, keyDegradationEnteredAt).Int64(); err == nil {
		st.EnteredAt = v
	} else if !errors.Is(err, goredis.Nil) {
		return DegradationState{}, fmt.Errorf("redis get degradation entered_at: %w", err)
	}
	return st, nil
}

// SetDegradationMode writes mode + reason + entered_at (epoch millis)
// atomically via MULTI. Unknown modes are rejected before any write —
// a typo'd mode must fail closed, not silently persist.
func (c *Client) SetDegradationMode(ctx context.Context, mode DegradationMode, reason string) error {
	if !validMode(mode) {
		return fmt.Errorf("redis set degradation mode: invalid mode %q", mode)
	}
	pipe := c.TxPipeline()
	pipe.MSet(ctx, map[string]any{
		keyDegradationMode:      string(mode),
		keyDegradationEnteredAt: time.Now().UnixMilli(),
		keyDegradationReason:    reason,
	})
	_, err := pipe.Exec(ctx)
	if err != nil {
		return fmt.Errorf("redis set degradation mode: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Circuit breakers — circuit_breaker:{scope}:{id} HASH (spec §4.2, §2.6)
// ---------------------------------------------------------------------------

// Canonical breaker states (spec §2.6: CLOSED → OPEN(hold) →
// HALF_OPEN(probe) → CLOSED).
const (
	CircuitClosed   = "CLOSED"
	CircuitOpen     = "OPEN"
	CircuitHalfOpen = "HALF_OPEN"
)

// CircuitBreaker is the breaker record: a state plus free-form metadata
// (tripped_at, reason, failure count — whatever the caller needs).
type CircuitBreaker struct {
	State    string
	Metadata map[string]string
}

func validCircuitState(s string) bool {
	switch s {
	case CircuitClosed, CircuitOpen, CircuitHalfOpen:
		return true
	}
	return false
}

// SetCircuitBreaker replaces the breaker hash: state field plus one hash
// field per metadata entry. DEL+HSET run inside MULTI so no stale metadata
// fields survive and readers never see a half-written record.
func (c *Client) SetCircuitBreaker(ctx context.Context, scope, id string, cb CircuitBreaker) error {
	if !validCircuitState(cb.State) {
		return fmt.Errorf("redis set circuit breaker: invalid state %q", cb.State)
	}
	key := circuitBreakerKey(scope, id)
	fields := map[string]any{"state": cb.State}
	for k, v := range cb.Metadata {
		fields[k] = v
	}
	pipe := c.TxPipeline()
	pipe.Del(ctx, key)
	pipe.HSet(ctx, key, fields)
	_, err := pipe.Exec(ctx)
	if err != nil {
		return fmt.Errorf("redis set circuit breaker %s:%s: %w", scope, id, err)
	}
	return nil
}

// GetCircuitBreaker reads the breaker hash. A missing key returns
// (nil, nil): no breaker record means the circuit is effectively closed.
func (c *Client) GetCircuitBreaker(ctx context.Context, scope, id string) (*CircuitBreaker, error) {
	m, err := c.HGetAll(ctx, circuitBreakerKey(scope, id)).Result()
	if err != nil {
		return nil, fmt.Errorf("redis get circuit breaker %s:%s: %w", scope, id, err)
	}
	if len(m) == 0 {
		return nil, nil
	}
	cb := &CircuitBreaker{State: m["state"], Metadata: map[string]string{}}
	for k, v := range m {
		if k != "state" {
			cb.Metadata[k] = v
		}
	}
	return cb, nil
}

// ---------------------------------------------------------------------------
// Global halt — halt:global STRING (spec §4.2)
// ---------------------------------------------------------------------------

// HaltGlobal raises the global trading halt flag, storing the reason (or
// "manual" when empty) as the value for auditability.
func (c *Client) HaltGlobal(ctx context.Context, reason string) error {
	if reason == "" {
		reason = "manual"
	}
	if err := c.Set(ctx, keyHaltGlobal, reason, 0).Err(); err != nil {
		return fmt.Errorf("redis halt global: %w", err)
	}
	return nil
}

// IsHalted reports whether the global halt flag is set.
func (c *Client) IsHalted(ctx context.Context) (bool, error) {
	n, err := c.Exists(ctx, keyHaltGlobal).Result()
	if err != nil {
		return false, fmt.Errorf("redis is halted: %w", err)
	}
	return n > 0, nil
}

// ClearHalt removes the global halt flag (operator resume path).
func (c *Client) ClearHalt(ctx context.Context) error {
	if err := c.Del(ctx, keyHaltGlobal).Err(); err != nil {
		return fmt.Errorf("redis clear halt: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Scoped halt flags — halt:<scope>:<target> STRING (Phase-11 Tasks
// 11.3.8/11.3.12). These are the hot-path enforcement flags the scoped
// kill-switch resolver (admin.KillSwitchResolver) MGETs on every order
// admission; the durable record lives in trading_suspensions
// (migration 198). Key layout is owned here — single source of truth.
// ---------------------------------------------------------------------------

// HaltKey renders the scoped halt flag key. GLOBAL ignores the target
// (halt:global, shared with HaltGlobal). All other scopes render
// halt:<scope-lower>:<target>; callers normalize the target (symbol
// canonicalization, session id, rail name) before calling.
func HaltKey(scope, target string) string {
	if scope == "GLOBAL" || scope == "" {
		return keyHaltGlobal
	}
	return fmt.Sprintf("halt:%s:%s", strings.ToLower(scope), target)
}

// SetHaltScope raises a scoped halt flag storing the operator reason
// ("manual" when empty). scope is the enum token (ACCOUNT, INSTRUMENT,
// ...); GLOBAL routes through HaltGlobal semantics.
func (c *Client) SetHaltScope(ctx context.Context, scope, target, reason string) error {
	if reason == "" {
		reason = "manual"
	}
	if err := c.Set(ctx, HaltKey(scope, target), reason, 0).Err(); err != nil {
		return fmt.Errorf("redis set halt %s: %w", scope, err)
	}
	return nil
}

// ClearHaltScope removes a scoped halt flag.
func (c *Client) ClearHaltScope(ctx context.Context, scope, target string) error {
	if err := c.Del(ctx, HaltKey(scope, target)).Err(); err != nil {
		return fmt.Errorf("redis clear halt %s: %w", scope, err)
	}
	return nil
}

// HaltScopeScan MGETs the given halt keys in ONE round-trip and returns
// key → reason for every flag currently set (missing keys are absent
// from the map). Callers order the key slice by resolution precedence
// and walk it for the first hit — a nil client fails closed upstream.
func (c *Client) HaltScopeScan(ctx context.Context, keys []string) (map[string]string, error) {
	out := make(map[string]string, len(keys))
	if len(keys) == 0 {
		return out, nil
	}
	vals, err := c.MGet(ctx, keys...).Result()
	if err != nil {
		return nil, fmt.Errorf("redis halt scan: %w", err)
	}
	for i, v := range vals {
		if v == nil {
			continue
		}
		if s, ok := v.(string); ok {
			out[keys[i]] = s
		} else {
			out[keys[i]] = "set"
		}
	}
	return out, nil
}
