// Task 5.3.2/5.3.27/5.3.34/5.3.40 — edge rate-limit engine.
//
// One Lua round trip per request performs, atomically:
//  1. IP allowlist check (Task 5.3.34/§8.8 allowlist bypass — allowlisted
//     IPs skip the ban machinery but stay rate-limited).
//  2. Active-ban gate: `ip_ban:{ip}` present → banned.
//  3. Post-429 offense: `rl429:{ip}` marker present (a request within 60s
//     of a 429 per the task's "within 1min of 429" wording) → strike ++,
//     timed ban per escalation schedule, audit row appended.
//  4. Token bucket `rl:tb:{tier}:{key}` (capacity = BurstFactor×rate,
//     refill = effective rate — the "2x for 500ms" burst of §8.3).
//  5. Per-second window counter `rl:{tier}:{key}:{second}` INCR+PEXPIRE
//     (spec §4.1 key scheme, pinned by remediation #35) feeding
//     X-RateLimit-* header computation.
//  6. Multi-interval usage counters `rl:usage:{key}` (RAW_REQUESTS,
//     REQUEST_WEIGHT, ORDERS over 1s/1m/1d windows — Task 5.3.40).
//  7. REQUEST_WEIGHT per-minute quota check → REQUEST_WEIGHT_EXCEEDED.
//  8. On deny, the rl429 marker is armed so follow-on requests escalate.
package ratelimit

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	goredis "github.com/redis/go-redis/v9"

	exchredis "exchange/internal/redis"
)

// HitStatus is the outcome of an edge hit.
type HitStatus int

const (
	HitOK             HitStatus = iota // request admitted
	HitRateLimited                     // token bucket empty → 429 RATE_LIMIT_TIER_EXCEEDED
	HitWeightExceeded                  // per-minute weight quota → 429 REQUEST_WEIGHT_EXCEEDED
	HitBanned                          // active ban record → 418 IP_BANNED
	HitBannedNew                       // this request triggered a new ban → 418 IP_BANNED
)

// Ban is the ip_ban:{ip} record (Task 5.3.34).
type Ban struct {
	IP        string `json:"ip"`
	Level     int    `json:"level"`        // escalation level 1..3 (capped)
	Strikes   int64  `json:"strikes"`      // lifetime offense count (24h sliding)
	Reason    string `json:"reason"`       // "post-429 abuse" or admin-supplied
	BannedAt  int64  `json:"banned_at_ms"` // epoch ms
	ExpiresAt int64  `json:"expires_at_ms"`
	Actor     string `json:"actor"` // "system" or admin identity
}

// BanSchedule is the Task 5.3.34 escalation ladder: strike 1 → 2min,
// strike 2 → 30min, strike ≥3 → 24h.
var BanSchedule = []time.Duration{2 * time.Minute, 30 * time.Minute, 24 * time.Hour}

// banDuration maps a strike count onto the schedule (clamped at the top).
func banDuration(strikes int64) (level int, dur time.Duration) {
	level = int(strikes)
	if level < 1 {
		level = 1
	}
	if level > len(BanSchedule) {
		level = len(BanSchedule)
	}
	return level, BanSchedule[level-1]
}

// HitInput is one admission decision request.
type HitInput struct {
	Tier        Tier      // resolved caller tier
	Key         string    // bucket identity — accountID for account tiers, IP for Public
	IP          string    // client IP (ban machinery; always populated)
	Weight      int64     // route weight cost (≥1; Task 5.3.40/5.3.42)
	Order       bool      // true on order-placement paths → ORDERS counters
	Rate        int64     // effective req/s after throttle multiplier (≥1)
	WeightQuota int64     // REQUEST_WEIGHT per-minute quota (>0)
	Now         time.Time // clock injection point for tests
}

// HitResult is the edge decision with everything the HTTP middleware
// needs to emit headers and error envelopes.
type HitResult struct {
	Status     HitStatus
	Limit      int64 // effective req/s actually enforced
	Remaining  int64 // tokens left after this hit (floor)
	ResetEpoch int64 // epoch seconds when the 1s window counter resets
	RetryAfter int64 // seconds to tell the client (429) / until ban expiry (418)
	Ban        *Ban  // populated on HitBanned/HitBannedNew
}

// Backend is the decision store seam. RedisBackend is authoritative;
// MemBackend is the spec §4.1/§18.3 Sentinel-failover fallback and the
// unit-test double — both implement identical semantics.
type Backend interface {
	Hit(ctx context.Context, in HitInput) (HitResult, error)
	Usage(ctx context.Context, key string) (map[string]int64, error)
	BanInfo(ctx context.Context, ip string) (*Ban, error)
	ListBans(ctx context.Context) ([]Ban, error)
	SetBan(ctx context.Context, b Ban) error
	ClearBan(ctx context.Context, ip string) (removed bool, err error)
	ClearStrikes(ctx context.Context, ip string) error
	IsAllowlisted(ctx context.Context, ip string) (bool, error)
	AddAllowlist(ctx context.Context, ip string) error
	RemoveAllowlist(ctx context.Context, ip string) error
	Audit(ctx context.Context, rec string) error
	ListAudit(ctx context.Context, n int64) ([]string, error)
}

// ---------------------------------------------------------------------------
// Redis backend
// ---------------------------------------------------------------------------

// evaler is the narrow go-redis seam (*redis.Client embeds
// *goredis.Client which satisfies it; tests substitute MemBackend at the
// Backend level instead of scripting).
type evaler interface {
	Eval(ctx context.Context, script string, keys []string, args ...interface{}) *goredis.Cmd
}

// hitScript is the single-round-trip edge decision described above.
// KEYS: 1 ip_ban:{ip} · 2 rl429:{ip} · 3 ip_ban_strikes:{ip} ·
//
//	4 ip_allowlist · 5 rl:tb:{tier}:{key} · 6 rl:{tier}:{key}:{sec} ·
//	7 rl:usage:{key} · 8 ip_ban_audit
var hitScript = `
local now          = tonumber(ARGV[1])
local ip           = ARGV[2]
local cost         = tonumber(ARGV[3])
local rate_per_ms  = tonumber(ARGV[4])
local capacity     = tonumber(ARGV[5])
local ctr_ttl      = tonumber(ARGV[6])
local bucket_ttl   = tonumber(ARGV[7])
local strike_ttl   = tonumber(ARGV[8])
local marker_ttl   = tonumber(ARGV[9])
local ban1         = tonumber(ARGV[10])
local ban2         = tonumber(ARGV[11])
local ban3         = tonumber(ARGV[12])
local sec_win      = tonumber(ARGV[13])
local min_win      = tonumber(ARGV[14])
local day_win      = tonumber(ARGV[15])
local weight_quota = tonumber(ARGV[16])
local is_order     = tonumber(ARGV[17])

-- 1-3: ban machinery (skipped for allowlisted IPs)
if redis.call('SISMEMBER', KEYS[4], ip) == 0 then
  local ban = redis.call('GET', KEYS[1])
  if ban then
    local ttl = redis.call('PTTL', KEYS[1])
    return {3, 0, ttl, ban}
  end
  if redis.call('EXISTS', KEYS[2]) == 1 then
    local strikes = redis.call('INCR', KEYS[3])
    redis.call('EXPIRE', KEYS[3], strike_ttl)
    local dur = ban3
    local level = strikes
    if strikes == 1 then dur = ban1 elseif strikes == 2 then dur = ban2 end
    if level > 3 then level = 3 end
    local expires = now + dur
    local rec = '{"ip":"' .. ip .. '","level":' .. level ..
                ',"strikes":' .. strikes ..
                ',"reason":"post-429 abuse","banned_at_ms":' .. now ..
                ',"expires_at_ms":' .. expires .. ',"actor":"system"}'
    redis.call('SET', KEYS[1], rec, 'PX', dur)
    redis.call('DEL', KEYS[2])
    redis.call('RPUSH', KEYS[8], rec)
    redis.call('LTRIM', KEYS[8], -10000, -1)
    return {4, 0, dur, rec}
  end
end

-- 4: token bucket
local b = redis.call('HMGET', KEYS[5], 'tokens', 'ts')
local tokens = tonumber(b[1]); local ts = tonumber(b[2])
if tokens == nil then tokens = capacity; ts = now end
if now > ts then tokens = math.min(capacity, tokens + (now - ts) * rate_per_ms) end
local allowed = 0
local deny = 0
local wmin = tonumber(redis.call('HGET', KEYS[7], 'w:m:' .. min_win)) or 0
if tokens >= cost then
  if wmin + cost <= weight_quota then
    tokens = tokens - cost
    allowed = 1
  else
    deny = 2
  end
else
  deny = 1
end
redis.call('HMSET', KEYS[5], 'tokens', tokens, 'ts', now)
redis.call('PEXPIRE', KEYS[5], bucket_ttl)

-- 5: per-second window counter (spec §4.1)
local count = redis.call('INCR', KEYS[6])
if count == 1 then redis.call('PEXPIRE', KEYS[6], ctr_ttl) end

-- 6: multi-interval usage counters (Task 5.3.40); failed requests stay
-- charged — raw + weight always increment, orders only when flagged.
redis.call('HINCRBY', KEYS[7], 'raw:s:' .. sec_win, 1)
redis.call('HINCRBY', KEYS[7], 'raw:m:' .. min_win, 1)
redis.call('HINCRBY', KEYS[7], 'raw:d:' .. day_win, 1)
redis.call('HINCRBY', KEYS[7], 'w:s:' .. sec_win, cost)
redis.call('HINCRBY', KEYS[7], 'w:m:' .. min_win, cost)
redis.call('HINCRBY', KEYS[7], 'w:d:' .. day_win, cost)
if is_order == 1 then
  redis.call('HINCRBY', KEYS[7], 'ord:s:' .. sec_win, 1)
  redis.call('HINCRBY', KEYS[7], 'ord:m:' .. min_win, 1)
  redis.call('HINCRBY', KEYS[7], 'ord:d:' .. day_win, 1)
end
redis.call('PEXPIRE', KEYS[7], 172800000)

-- 8: arm the post-429 marker on any deny (non-allowlisted)
if allowed == 0 and redis.call('SISMEMBER', KEYS[4], ip) == 0 then
  redis.call('SET', KEYS[2], now, 'PX', marker_ttl)
end

return {deny == 0 and 0 or deny, math.floor(tokens), count, 0}
`

// RedisBackend executes the edge scripts against the coordination Redis.
type RedisBackend struct {
	rdb evaler
}

// NewRedisBackend wires the backend to a coordination-instance client
// (*exchredis.Client or *goredis.Client).
func NewRedisBackend(rdb evaler) *RedisBackend { return &RedisBackend{rdb: rdb} }

// Canonical Redis key layout (spec §4.1 + remediation #35 key scheme).
func bucketKey(tier Tier, key string) string { return fmt.Sprintf("rl:tb:%s:%s", tier, key) }
func windowKey(tier Tier, key string, sec int64) string {
	return fmt.Sprintf("rl:%s:%s:%d", tier, key, sec)
}
func usageKey(key string) string  { return "rl:usage:" + key }
func banKey(ip string) string     { return "ip_ban:" + ip }
func markerKey(ip string) string  { return "rl429:" + ip }
func strikesKey(ip string) string { return "ip_ban_strikes:" + ip }

const (
	allowlistKey = "ip_allowlist"
	auditListKey = "ip_ban_audit"
	auditMaxLen  = 10000
)

// Hit executes the edge decision script. See package docs for semantics.
func (b *RedisBackend) Hit(ctx context.Context, in HitInput) (HitResult, error) {
	nowMs := in.Now.UnixMilli()
	secWin := nowMs / 1000
	minWin := nowMs / 60000
	dayWin := nowMs / 86400000
	spec := SpecOf(in.Tier)
	capacity := float64(in.Rate * spec.BurstFactor)

	res, err := b.rdb.Eval(ctx, hitScript, []string{
		banKey(in.IP), markerKey(in.IP), strikesKey(in.IP), allowlistKey,
		bucketKey(in.Tier, in.Key), windowKey(in.Tier, in.Key, secWin),
		usageKey(in.Key), auditListKey,
	},
		nowMs, in.IP, in.Weight,
		float64(in.Rate)/1000.0, // refill per ms
		capacity,
		RateWindowTTL.Milliseconds(),
		bucketIdleTTL(spec.BurstFactor).Milliseconds(),
		int64(strikeWindow/time.Second),
		offenseWindow.Milliseconds(),
		BanSchedule[0].Milliseconds(), BanSchedule[1].Milliseconds(), BanSchedule[2].Milliseconds(),
		secWin, minWin, dayWin,
		in.WeightQuota,
		boolInt(in.Order),
	).Slice()
	if err != nil {
		return HitResult{}, fmt.Errorf("ratelimit hit: %w", err)
	}
	return parseHitResult(res, in, secWin, minWin)
}

// parseHitResult converts the Lua return vector into a HitResult.
func parseHitResult(res []interface{}, in HitInput, secWin, minWin int64) (HitResult, error) {
	if len(res) < 4 {
		return HitResult{}, fmt.Errorf("ratelimit hit: short reply %v", res)
	}
	status, _ := res[0].(int64)
	out := HitResult{
		Limit:      in.Rate,
		ResetEpoch: secWin + 1,
	}
	switch HitStatus(status) {
	case HitOK:
		out.Status = HitOK
		out.Remaining, _ = res[1].(int64)
	case HitRateLimited:
		out.Status = HitRateLimited
		out.Remaining, _ = res[1].(int64)
		out.RetryAfter = secWin + 1 - in.Now.Unix()
		if out.RetryAfter < 1 {
			out.RetryAfter = 1
		}
	case HitWeightExceeded:
		out.Status = HitWeightExceeded
		out.Remaining, _ = res[1].(int64)
		out.RetryAfter = (minWin+1)*60 - in.Now.Unix()
		if out.RetryAfter < 1 {
			out.RetryAfter = 1
		}
	case HitBanned, HitBannedNew:
		out.Status = HitStatus(status)
		var rec string
		switch v := res[3].(type) {
		case string:
			rec = v
		case []byte:
			rec = string(v)
		}
		if rec != "" {
			var ban Ban
			if err := json.Unmarshal([]byte(rec), &ban); err == nil {
				out.Ban = &ban
				out.RetryAfter = (ban.ExpiresAt - in.Now.UnixMilli()) / 1000
				if out.RetryAfter < 1 {
					out.RetryAfter = 1
				}
			}
		}
		if out.Ban == nil {
			// PTTL path (existing ban): res[2] carries ms remaining.
			if ms, ok := res[2].(int64); ok {
				out.RetryAfter = ms / 1000
				if out.RetryAfter < 1 {
					out.RetryAfter = 1
				}
				out.Ban = &Ban{IP: in.IP, ExpiresAt: in.Now.UnixMilli() + ms}
			}
		}
	default:
		return HitResult{}, fmt.Errorf("ratelimit hit: unknown status %d", status)
	}
	return out, nil
}

// Usage reads the rl:usage:{key} hash verbatim (field → count).
func (b *RedisBackend) Usage(ctx context.Context, key string) (map[string]int64, error) {
	m, err := b.rdb.Eval(ctx, `return redis.call('HGETALL', KEYS[1])`,
		[]string{usageKey(key)}).Slice()
	if err != nil {
		return nil, fmt.Errorf("ratelimit usage: %w", err)
	}
	out := make(map[string]int64, len(m)/2)
	for i := 0; i+1 < len(m); i += 2 {
		name, _ := m[i].(string)
		v, _ := parseInt64(m[i+1])
		out[name] = v
	}
	return out, nil
}

// BanInfo returns the active ban record for ip; nil when unbanned.
func (b *RedisBackend) BanInfo(ctx context.Context, ip string) (*Ban, error) {
	res, err := b.rdb.Eval(ctx, `return redis.call('GET', KEYS[1]) or false`,
		[]string{banKey(ip)}).Result()
	if err != nil {
		return nil, fmt.Errorf("ratelimit ban info: %w", err)
	}
	s, ok := res.(string)
	if !ok || s == "" {
		return nil, nil
	}
	var ban Ban
	if err := json.Unmarshal([]byte(s), &ban); err != nil {
		return nil, fmt.Errorf("ratelimit ban info decode: %w", err)
	}
	return &ban, nil
}

// ListBans scans ip_ban:* for the admin review surface. SCAN is used so
// the gateway never blocks on KEYS over a hot keyspace.
func (b *RedisBackend) ListBans(ctx context.Context) ([]Ban, error) {
	res, err := b.rdb.Eval(ctx, `
local out = {}
local cur = '0'
repeat
  local s = redis.call('SCAN', cur, 'MATCH', 'ip_ban:*', 'COUNT', 200)
  cur = s[1]
  for _, k in ipairs(s[2]) do
    if k ~= 'ip_ban_audit' and not string.match(k, '^ip_ban_strikes:') then
      local v = redis.call('GET', k)
      if v then out[#out+1] = v end
    end
  end
until cur == '0'
return out`, nil).Slice()
	if err != nil {
		return nil, fmt.Errorf("ratelimit list bans: %w", err)
	}
	out := make([]Ban, 0, len(res))
	for _, item := range res {
		s, ok := item.(string)
		if !ok {
			continue
		}
		var ban Ban
		if err := json.Unmarshal([]byte(s), &ban); err == nil {
			out = append(out, ban)
		}
	}
	return out, nil
}

// SetBan writes an admin/manual ban with TTL = expires_at - now.
func (b *RedisBackend) SetBan(ctx context.Context, ban Ban) error {
	rec, err := json.Marshal(ban)
	if err != nil {
		return fmt.Errorf("ratelimit set ban encode: %w", err)
	}
	ttl := ban.ExpiresAt - time.Now().UnixMilli()
	if ttl <= 0 {
		return fmt.Errorf("ratelimit set ban: expiry in the past")
	}
	if err := b.rdb.Eval(ctx,
		`return redis.call('SET', KEYS[1], ARGV[1], 'PX', ARGV[2])`,
		[]string{banKey(ban.IP)}, string(rec), ttl).Err(); err != nil {
		return fmt.Errorf("ratelimit set ban: %w", err)
	}
	return nil
}

// ClearBan deletes the active ban (admin override); strikes are kept so a
// re-offense re-escalates — ClearStrikes is the full pardon.
func (b *RedisBackend) ClearBan(ctx context.Context, ip string) (bool, error) {
	n, err := b.rdb.Eval(ctx, `return redis.call('DEL', KEYS[1])`,
		[]string{banKey(ip)}).Int()
	if err != nil {
		return false, fmt.Errorf("ratelimit clear ban: %w", err)
	}
	return n == 1, nil
}

// ClearStrikes resets the escalation counter (full pardon).
func (b *RedisBackend) ClearStrikes(ctx context.Context, ip string) error {
	if err := b.rdb.Eval(ctx, `return redis.call('DEL', KEYS[1])`,
		[]string{strikesKey(ip)}).Err(); err != nil {
		return fmt.Errorf("ratelimit clear strikes: %w", err)
	}
	return nil
}

// IsAllowlisted reports ban-machinery exemption.
func (b *RedisBackend) IsAllowlisted(ctx context.Context, ip string) (bool, error) {
	n, err := b.rdb.Eval(ctx, `return redis.call('SISMEMBER', KEYS[1], ARGV[1])`,
		[]string{allowlistKey}, ip).Int()
	if err != nil {
		return false, fmt.Errorf("ratelimit allowlist check: %w", err)
	}
	return n == 1, nil
}

// AddAllowlist / RemoveAllowlist manage the exemption set.
func (b *RedisBackend) AddAllowlist(ctx context.Context, ip string) error {
	if err := b.rdb.Eval(ctx, `return redis.call('SADD', KEYS[1], ARGV[1])`,
		[]string{allowlistKey}, ip).Err(); err != nil {
		return fmt.Errorf("ratelimit allowlist add: %w", err)
	}
	return nil
}

func (b *RedisBackend) RemoveAllowlist(ctx context.Context, ip string) error {
	if err := b.rdb.Eval(ctx, `return redis.call('SREM', KEYS[1], ARGV[1])`,
		[]string{allowlistKey}, ip).Err(); err != nil {
		return fmt.Errorf("ratelimit allowlist remove: %w", err)
	}
	return nil
}

// Audit appends a JSON record to the capped ip_ban_audit list
// (Task 5.3.34 audit trail for system bans and admin overrides).
func (b *RedisBackend) Audit(ctx context.Context, rec string) error {
	if err := b.rdb.Eval(ctx, `
redis.call('RPUSH', KEYS[1], ARGV[1])
redis.call('LTRIM', KEYS[1], -`+fmt.Sprint(auditMaxLen)+`, -1)
return 1`, []string{auditListKey}, rec).Err(); err != nil {
		return fmt.Errorf("ratelimit audit: %w", err)
	}
	return nil
}

// ListAudit returns the newest n audit records.
func (b *RedisBackend) ListAudit(ctx context.Context, n int64) ([]string, error) {
	res, err := b.rdb.Eval(ctx, `return redis.call('LRANGE', KEYS[1], -ARGV[1], -1)`,
		[]string{auditListKey}, n).Slice()
	if err != nil {
		return nil, fmt.Errorf("ratelimit list audit: %w", err)
	}
	out := make([]string, 0, len(res))
	for _, item := range res {
		if s, ok := item.(string); ok {
			out = append(out, s)
		}
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Limiter — tier resolution, throttle, failover
// ---------------------------------------------------------------------------

// ModeReader resolves the active degradation mode (*exchredis.Client
// satisfies it). Nil reader ⇒ Normal.
type ModeReader interface {
	GetDegradationMode(ctx context.Context) (exchredis.DegradationState, error)
}

// LimiterOptions tunes a Limiter.
type LimiterOptions struct {
	Throttle map[Tier]float64 // nil ⇒ DefaultThrottle
	Mode     ModeReader       // nil ⇒ always Normal
	Now      func() time.Time // nil ⇒ time.Now
	Fallback Backend          // nil ⇒ fresh MemBackend
}

// Limiter is the facade the middleware calls: resolves the effective
// tier rate (× throttle under Throttled mode), executes Hit against the
// primary backend, and falls back to the in-memory backend when the
// primary errors (spec §4.1 Sentinel-failover rule).
type Limiter struct {
	primary  Backend
	fallback Backend
	throttle map[Tier]float64
	mode     ModeReader
	now      func() time.Time
}

// NewLimiter wires a limiter over the primary backend.
func NewLimiter(primary Backend, opts LimiterOptions) *Limiter {
	l := &Limiter{
		primary:  primary,
		fallback: opts.Fallback,
		throttle: opts.Throttle,
		mode:     opts.Mode,
		now:      opts.Now,
	}
	if l.fallback == nil {
		l.fallback = NewMemBackend()
	}
	if l.throttle == nil {
		l.throttle = DefaultThrottle
	}
	if l.now == nil {
		l.now = time.Now
	}
	return l
}

// multiplier resolves the degradation throttle for the tier: only
// `Throttled` reduces; every other mode (and read failure — the mode
// reader is advisory here, fail-closed lives in the gate decision) is 1.0.
func (l *Limiter) multiplier(ctx context.Context, t Tier) float64 {
	if l.mode == nil {
		return 1
	}
	st, err := l.mode.GetDegradationMode(ctx)
	if err != nil || st.Mode != exchredis.ModeThrottled {
		return 1
	}
	if m, ok := l.throttle[t]; ok {
		return m
	}
	return l.throttle[TierPublic] // unknown tier ⇒ strictest
}

// Check resolves the effective quota and executes one edge hit.
func (l *Limiter) Check(ctx context.Context, id Identity, weight int64, order bool) (HitResult, error) {
	spec := SpecOf(id.Tier)
	mult := l.multiplier(ctx, id.Tier)
	in := HitInput{
		Tier:        id.Tier,
		Key:         id.Key,
		IP:          id.IP,
		Weight:      weight,
		Order:       order,
		Rate:        EffectiveRate(spec, mult),
		WeightQuota: int64(float64(spec.WeightPerMin) * mult),
		Now:         l.now(),
	}
	if in.Weight < 1 {
		in.Weight = 1
	}
	if in.WeightQuota < in.Rate {
		in.WeightQuota = in.Rate // never below one sustained second's worth
	}
	res, err := l.primary.Hit(ctx, in)
	if err == nil {
		return res, nil
	}
	fb, ferr := l.fallback.Hit(ctx, in)
	if ferr != nil {
		return HitResult{}, fmt.Errorf("ratelimit: primary %v; fallback %v", err, ferr)
	}
	return fb, nil
}

// Usage exposes the backend for introspection/admin plumbing.
func (l *Limiter) Backend() Backend { return l.primary }

// EffectiveLimit resolves the tier's post-throttle quotas — the same
// values Check enforces — for the introspection endpoint (Task 5.3.40:
// clients see the limits currently applied to them).
func (l *Limiter) EffectiveLimit(ctx context.Context, t Tier) (ratePerSec, weightPerMin int64) {
	spec := SpecOf(t)
	mult := l.multiplier(ctx, t)
	ratePerSec = EffectiveRate(spec, mult)
	weightPerMin = int64(float64(spec.WeightPerMin) * mult)
	if weightPerMin < ratePerSec {
		weightPerMin = ratePerSec
	}
	return ratePerSec, weightPerMin
}

// Usage reads the caller's multi-interval usage view (Task 5.3.40).
func (l *Limiter) Usage(ctx context.Context, id Identity) (Usage, error) {
	rate, _ := l.EffectiveLimit(ctx, id.Tier)
	u, err := ReadUsage(ctx, l.primary, id, l.now(), rate)
	if err != nil {
		return ReadUsage(ctx, l.fallback, id, l.now(), rate)
	}
	return u, nil
}

// Identity is the resolved caller identity for a request.
type Identity struct {
	Tier Tier
	Key  string // accountID string for account tiers; IP for Public
	IP   string
}

// Window counter TTL — spec §4.1 arms 2s on the per-second keys.
const RateWindowTTL = 2 * time.Second

// offenseWindow is the Task 5.3.34 "within 1min of 429" marker TTL.
const offenseWindow = 60 * time.Second

// strikeWindow is the sliding escalation horizon (strikes older than
// 24h are forgotten — a one-off burst cannot perma-escalate a client).
const strikeWindow = 24 * time.Hour

// bucketIdleTTL expires token-bucket hashes after ~2 burst refill
// periods (min 10s) so idle identities do not leak keys.
func bucketIdleTTL(burstFactor int64) time.Duration {
	d := time.Duration(2*burstFactor) * time.Second
	if d < 10*time.Second {
		d = 10 * time.Second
	}
	return d
}

func boolInt(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

func parseInt64(v interface{}) (int64, error) {
	switch n := v.(type) {
	case int64:
		return n, nil
	case string:
		var out int64
		_, err := fmt.Sscanf(n, "%d", &out)
		return out, err
	default:
		return 0, fmt.Errorf("not an integer: %v", v)
	}
}
