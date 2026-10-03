// MemBackend — the spec §4.1/§18.3 Sentinel-failover rate-limit backend
// and the deterministic unit-test double. It mirrors the Redis Lua
// semantics one-for-one (same windows, same escalation, same audit log)
// so behaviour is identical whichever side serves the decision.
package ratelimit

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// memBucket is the token bucket hash analogue {tokens, ts_ms}.
type memBucket struct {
	tokens float64
	ts     int64
	exp    int64 // epoch ms deadline for GC
}

// memEnt is a counter with an expiry deadline (window counters, strikes).
type memEnt struct {
	count int64
	exp   int64
}

// MemBackend keeps the full edge state in process. During a Sentinel
// master failover (spec §4.1: "rate limits fall back to in-memory Go
// token buckets") this is what Limiter.Check lands on; ban state set
// here is best-effort — the Redis records reassert authority on return.
type MemBackend struct {
	mu        sync.Mutex
	buckets   map[string]*memBucket
	windows   map[string]*memEnt
	usage     map[string]map[string]int64
	bans      map[string]*memBan
	strikes   map[string]*memEnt
	markers   map[string]int64 // ip → expiry ms
	allowlist map[string]bool
	audit     []string
	now       func() time.Time // test injection; nil ⇒ time.Now
}

type memBan struct {
	ban Ban
	exp int64
}

// NewMemBackend returns an empty in-memory backend.
func NewMemBackend() *MemBackend {
	return &MemBackend{
		buckets:   map[string]*memBucket{},
		windows:   map[string]*memEnt{},
		usage:     map[string]map[string]int64{},
		bans:      map[string]*memBan{},
		strikes:   map[string]*memEnt{},
		markers:   map[string]int64{},
		allowlist: map[string]bool{},
	}
}

// SetNow injects a clock for deterministic tests.
func (m *MemBackend) SetNow(f func() time.Time) { m.now = f }

func (m *MemBackend) clock() time.Time {
	if m.now != nil {
		return m.now()
	}
	return time.Now()
}

func (m *MemBackend) nowMs() int64 { return m.clock().UnixMilli() }

// gc drops expired entries opportunistically on each hit so a busy
// fallback never grows without bound.
func (m *MemBackend) gc(now int64) {
	for k, v := range m.buckets {
		if v.exp <= now {
			delete(m.buckets, k)
		}
	}
	for k, v := range m.windows {
		if v.exp <= now {
			delete(m.windows, k)
		}
	}
	for k, v := range m.bans {
		if v.exp <= now {
			delete(m.bans, k)
		}
	}
	for k, v := range m.strikes {
		if v.exp <= now {
			delete(m.strikes, k)
		}
	}
	for k, exp := range m.markers {
		if exp <= now {
			delete(m.markers, k)
		}
	}
}

// Hit mirrors hitScript exactly.
func (m *MemBackend) Hit(_ context.Context, in HitInput) (HitResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := in.Now.UnixMilli()
	m.gc(now)

	secWin := now / 1000
	minWin := now / 60000
	dayWin := now / 86400000
	spec := SpecOf(in.Tier)
	capacity := float64(in.Rate * spec.BurstFactor)

	// 1-3: ban machinery (allowlisted and private/LAN IPs bypass it).
	isPrivate := isPrivateIP(in.IP)
	if !m.allowlist[in.IP] && !isPrivate {
		if b, ok := m.bans[in.IP]; ok && b.exp > now {
			return HitResult{
				Status:     HitBanned,
				Limit:      in.Rate,
				ResetEpoch: secWin + 1,
				RetryAfter: max64(1, (b.exp-now)/1000),
				Ban:        &b.ban,
			}, nil
		}
		if exp, ok := m.markers[in.IP]; ok && exp > now {
			st := m.strikes[in.IP]
			if st == nil {
				st = &memEnt{}
				m.strikes[in.IP] = st
			}
			st.count++
			st.exp = now + strikeWindow.Milliseconds()
			level, dur := banDuration(st.count)
			ban := Ban{
				IP: in.IP, Level: level, Strikes: st.count,
				Reason:   "post-429 abuse",
				BannedAt: now, ExpiresAt: now + dur.Milliseconds(),
				Actor: "system",
			}
			m.bans[in.IP] = &memBan{ban: ban, exp: ban.ExpiresAt}
			delete(m.markers, in.IP)
			if rec, err := json.Marshal(ban); err == nil {
				m.appendAudit(string(rec))
			}
			return HitResult{
				Status:     HitBannedNew,
				Limit:      in.Rate,
				ResetEpoch: secWin + 1,
				RetryAfter: max64(1, dur.Milliseconds()/1000),
				Ban:        &ban,
			}, nil
		}
	}

	// 4: token bucket.
	bk := bucketKey(in.Tier, in.Key)
	b := m.buckets[bk]
	if b == nil {
		b = &memBucket{tokens: capacity, ts: now}
		m.buckets[bk] = b
	}
	if now > b.ts {
		b.tokens += float64(now-b.ts) * float64(in.Rate) / 1000.0
		if b.tokens > capacity {
			b.tokens = capacity
		}
	}
	b.ts = now
	b.exp = now + bucketIdleTTL(spec.BurstFactor).Milliseconds()

	ukey := usageKey(in.Key)
	u := m.usage[ukey]
	if u == nil {
		u = map[string]int64{}
		m.usage[ukey] = u
	}
	wmin := u[fmt.Sprintf("w:m:%d", minWin)]

	var status HitStatus
	switch {
	case b.tokens >= float64(in.Weight) && wmin+in.Weight <= in.WeightQuota:
		b.tokens -= float64(in.Weight)
		status = HitOK
	case b.tokens >= float64(in.Weight):
		status = HitWeightExceeded
	default:
		status = HitRateLimited
	}

	// 5: window counter.
	wk := windowKey(in.Tier, in.Key, secWin)
	w := m.windows[wk]
	if w == nil {
		w = &memEnt{}
		m.windows[wk] = w
	}
	w.count++
	w.exp = now + RateWindowTTL.Milliseconds()

	// 6: usage counters (failed requests stay charged).
	u[fmt.Sprintf("raw:s:%d", secWin)]++
	u[fmt.Sprintf("raw:m:%d", minWin)]++
	u[fmt.Sprintf("raw:d:%d", dayWin)]++
	u[fmt.Sprintf("w:s:%d", secWin)] += in.Weight
	u[fmt.Sprintf("w:m:%d", minWin)] += in.Weight
	u[fmt.Sprintf("w:d:%d", dayWin)] += in.Weight
	if in.Order {
		u[fmt.Sprintf("ord:s:%d", secWin)]++
		u[fmt.Sprintf("ord:m:%d", minWin)]++
		u[fmt.Sprintf("ord:d:%d", dayWin)]++
	}

	// 8: arm the post-429 marker on deny (non-allowlisted, non-private).
	if status != HitOK && !m.allowlist[in.IP] && !isPrivate {
		m.markers[in.IP] = now + offenseWindow.Milliseconds()
	}

	res := HitResult{
		Status:      status,
		Limit:       in.Rate,
		Remaining:   int64(b.tokens),
		ResetEpoch:  secWin + 1,
		WindowCount: w.count,
	}
	switch status {
	case HitRateLimited:
		res.RetryAfter = max64(1, secWin+1-in.Now.Unix())
	case HitWeightExceeded:
		res.RetryAfter = max64(1, (minWin+1)*60-in.Now.Unix())
	}
	return res, nil
}

func (m *MemBackend) appendAudit(rec string) {
	m.audit = append(m.audit, rec)
	if len(m.audit) > auditMaxLen {
		m.audit = m.audit[len(m.audit)-auditMaxLen:]
	}
}

// Usage returns a copy of the usage hash for the identity key.
func (m *MemBackend) Usage(_ context.Context, key string) (map[string]int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[string]int64{}
	for k, v := range m.usage[usageKey(key)] {
		out[k] = v
	}
	return out, nil
}

// BanInfo mirrors RedisBackend.BanInfo.
func (m *MemBackend) BanInfo(_ context.Context, ip string) (*Ban, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if b, ok := m.bans[ip]; ok && b.exp > m.nowMs() {
		ban := b.ban
		return &ban, nil
	}
	return nil, nil
}

// ListBans mirrors RedisBackend.ListBans (deterministic order by IP).
func (m *MemBackend) ListBans(_ context.Context) ([]Ban, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.nowMs()
	out := make([]Ban, 0, len(m.bans))
	for _, b := range m.bans {
		if b.exp > now {
			out = append(out, b.ban)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].IP < out[j].IP })
	return out, nil
}

// SetBan mirrors RedisBackend.SetBan.
func (m *MemBackend) SetBan(_ context.Context, ban Ban) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if ban.ExpiresAt <= m.nowMs() {
		return fmt.Errorf("ratelimit set ban: expiry in the past")
	}
	m.bans[ban.IP] = &memBan{ban: ban, exp: ban.ExpiresAt}
	return nil
}

// ClearBan mirrors RedisBackend.ClearBan.
func (m *MemBackend) ClearBan(_ context.Context, ip string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.bans[ip]; ok {
		delete(m.bans, ip)
		return true, nil
	}
	return false, nil
}

// ClearStrikes mirrors RedisBackend.ClearStrikes.
func (m *MemBackend) ClearStrikes(_ context.Context, ip string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.strikes, ip)
	return nil
}

// IsAllowlisted mirrors RedisBackend.IsAllowlisted.
func (m *MemBackend) IsAllowlisted(_ context.Context, ip string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.allowlist[ip], nil
}

// AddAllowlist mirrors RedisBackend.AddAllowlist.
func (m *MemBackend) AddAllowlist(_ context.Context, ip string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.allowlist[ip] = true
	return nil
}

// RemoveAllowlist mirrors RedisBackend.RemoveAllowlist.
func (m *MemBackend) RemoveAllowlist(_ context.Context, ip string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.allowlist, ip)
	return nil
}

// Audit mirrors RedisBackend.Audit.
func (m *MemBackend) Audit(_ context.Context, rec string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.appendAudit(rec)
	return nil
}

// ListAudit mirrors RedisBackend.ListAudit.
func (m *MemBackend) ListAudit(_ context.Context, n int64) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	start := len(m.audit) - int(n)
	if start < 0 {
		start = 0
	}
	out := make([]string, len(m.audit)-start)
	copy(out, m.audit[start:])
	return out, nil
}

// UsageView decodes a raw usage-hash map into typed counters for the
// introspection endpoint (Task 5.3.40).
type IntervalUsage struct {
	Interval    string `json:"interval"` // "1s" | "1m" | "1d"
	Count       int64  `json:"count"`
	WindowStart int64  `json:"window_start"` // epoch seconds
	ResetEpoch  int64  `json:"reset_epoch"`  // epoch seconds
	Limit       int64  `json:"limit"`        // enforced quota; -1 = unmetered
}

// Usage is the introspection payload: current-window counters for the
// three metered kinds, plus the identity's tier contract.
type Usage struct {
	Tier          Tier            `json:"tier"`
	RatePerSec    int64           `json:"rate_per_sec"`
	RawRequests   []IntervalUsage `json:"raw_requests"`
	RequestWeight []IntervalUsage `json:"request_weight"`
	Orders        []IntervalUsage `json:"orders"`
}

// ReadUsage assembles the Usage view for key under the current windows.
// Counters are account-wide for account tiers and IP-wide for Public
// (Task 5.3.40 item 3).
func ReadUsage(ctx context.Context, b Backend, in Identity, now time.Time, effRate int64) (Usage, error) {
	fields, err := b.Usage(ctx, in.Key)
	if err != nil {
		return Usage{}, err
	}
	spec := SpecOf(in.Tier)
	secWin := now.Unix()
	minWin := now.Unix() / 60
	dayWin := now.Unix() / 86400

	get := func(f string) int64 { return fields[f] }
	u := Usage{
		Tier:       in.Tier,
		RatePerSec: effRate,
		RawRequests: []IntervalUsage{
			{Interval: "1s", Count: get(fmt.Sprintf("raw:s:%d", secWin)), WindowStart: secWin, ResetEpoch: secWin + 1, Limit: effRate},
			{Interval: "1m", Count: get(fmt.Sprintf("raw:m:%d", minWin)), WindowStart: minWin * 60, ResetEpoch: (minWin + 1) * 60, Limit: effRate * 60},
			{Interval: "1d", Count: get(fmt.Sprintf("raw:d:%d", dayWin)), WindowStart: dayWin * 86400, ResetEpoch: (dayWin + 1) * 86400, Limit: -1},
		},
		RequestWeight: []IntervalUsage{
			{Interval: "1s", Count: get(fmt.Sprintf("w:s:%d", secWin)), WindowStart: secWin, ResetEpoch: secWin + 1, Limit: -1},
			{Interval: "1m", Count: get(fmt.Sprintf("w:m:%d", minWin)), WindowStart: minWin * 60, ResetEpoch: (minWin + 1) * 60, Limit: spec.WeightPerMin},
			{Interval: "1d", Count: get(fmt.Sprintf("w:d:%d", dayWin)), WindowStart: dayWin * 86400, ResetEpoch: (dayWin + 1) * 86400, Limit: -1},
		},
		Orders: []IntervalUsage{
			{Interval: "1s", Count: get(fmt.Sprintf("ord:s:%d", secWin)), WindowStart: secWin, ResetEpoch: secWin + 1, Limit: -1},
			{Interval: "1m", Count: get(fmt.Sprintf("ord:m:%d", minWin)), WindowStart: minWin * 60, ResetEpoch: (minWin + 1) * 60, Limit: -1},
			{Interval: "1d", Count: get(fmt.Sprintf("ord:d:%d", dayWin)), WindowStart: dayWin * 86400, ResetEpoch: (dayWin + 1) * 86400, Limit: -1},
		},
	}
	return u, nil
}

// IsUsageField reports whether a usage-hash field belongs to the metered
// kinds (helper for tests and future expiry sweeps).
func IsUsageField(f string) bool {
	return strings.HasPrefix(f, "raw:") || strings.HasPrefix(f, "w:") || strings.HasPrefix(f, "ord:")
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
