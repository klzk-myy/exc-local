// Package ratelimit implements the Phase-05 edge rate-limiting cluster:
//
//	Task 5.3.2  — 5-tier token-bucket rate limiting (spec §8.3)
//	Task 5.3.27 — standard X-RateLimit-* + Retry-After semantics (the HTTP
//	              surface lives in internal/middleware/rate_limit.go)
//	Task 5.3.34 — progressive post-429 IP ban escalation (§24 #258)
//	Task 5.3.40 — multi-interval RAW_REQUESTS / REQUEST_WEIGHT / ORDERS
//	              counters backing the introspection endpoints
//
// Redis key schema (spec §4.1 + Phase-05 remediation #35):
//
//	rl:{tier}:{accountId|ip}:{second}   STRING  per-second window counter (TTL 2s)
//	rl:tb:{tier}:{accountId|ip}         HASH    token bucket {tokens, ts} (idle TTL)
//	rl:usage:{accountId|ip}             HASH    multi-interval usage counters (TTL ~2d)
//	rl429:{ip}                          STRING  post-429 offense marker (TTL 60s)
//	ip_ban:{ip}                         STRING  ban record JSON (TTL = ban duration)
//	ip_ban_strikes:{ip}                 STRING  escalating strike counter (TTL 24h sliding)
//	ip_allowlist                        SET     IPs exempt from the ban machinery
//	ip_ban_audit                        LIST    append-only ban/override audit (capped)
//
// Fail-closed contract (spec §2.7): backend errors surface to the caller;
// the Limiter falls back to the in-memory backend during a Redis/Sentinel
// outage (spec §4.1/§18.3 failover rule) and only reports failure when
// both backends fail.
package ratelimit

import "fmt"

// Tier is the caller's rate-limit class (spec §8.3 five tiers plus the
// Admin identity named in Task 5.3.27, which shares the Institutional
// quota — operators are throttled, never unlimited).
type Tier string

const (
	TierPublic        Tier = "public"        // anonymous, keyed by IP
	TierBasic         Tier = "basic"         // authenticated basic
	TierStandard      Tier = "standard"      // authenticated standard
	TierProfessional  Tier = "professional"  // authenticated pro
	TierInstitutional Tier = "institutional" // institutional / market maker
	TierAdmin         Tier = "admin"         // operator identity (Task 5.3.27)
)

// Spec declares the §8.3 contract for one tier.
type Spec struct {
	RatePerSec   int64 // sustained REST request rate
	BurstFactor  int64 // burst capacity multiplier of RatePerSec ("2x for 500ms")
	WeightPerMin int64 // REQUEST_WEIGHT quota per rolling minute
	KeyedByIP    bool  // Public tier is keyed by IP; all others by accountID
}

// Specs is the spec §8.3 tier table. Institutional is 2,000+/s — the "+"
// headroom is expressed through BurstFactor, not an unbounded rate.
var Specs = map[Tier]Spec{
	TierPublic:        {RatePerSec: 5, BurstFactor: 2, WeightPerMin: 300, KeyedByIP: true},
	TierBasic:         {RatePerSec: 20, BurstFactor: 2, WeightPerMin: 1200},
	TierStandard:      {RatePerSec: 100, BurstFactor: 2, WeightPerMin: 6000},
	TierProfessional:  {RatePerSec: 500, BurstFactor: 2, WeightPerMin: 30000},
	TierInstitutional: {RatePerSec: 2000, BurstFactor: 2, WeightPerMin: 120000},
	TierAdmin:         {RatePerSec: 2000, BurstFactor: 2, WeightPerMin: 120000},
}

// SpecOf returns the tier contract; unknown tiers fail closed to Public
// (spec §2.7 — an unrecognised identity gets the strictest quota, never
// the loosest).
func SpecOf(t Tier) Spec {
	if s, ok := Specs[t]; ok {
		return s
	}
	return Specs[TierPublic]
}

// ThrottlePolicy maps degradation to a per-tier rate multiplier under
// `Throttled` mode (spec §8.3: "lower tiers reduced first"). The default
// graduates the reduction — Public hardest, Institutional untouched.
var DefaultThrottle = map[Tier]float64{
	TierPublic:        0.10,
	TierBasic:         0.25,
	TierStandard:      0.50,
	TierProfessional:  0.75,
	TierInstitutional: 1.00,
	TierAdmin:         1.00,
}

// EffectiveRate returns the tier's req/s under the given multiplier,
// floored at 1 (a live-but-degraded caller still gets a trickle; zero
// would silently convert Throttled into a ban).
func EffectiveRate(s Spec, multiplier float64) int64 {
	r := int64(float64(s.RatePerSec) * multiplier)
	if r < 1 {
		return 1
	}
	return r
}

// ParseTier maps a stored tier name (accounts→fee_tiers.tier_name or an
// API-key tier label) to a Tier. Unknown names fail closed to Public.
func ParseTier(name string) Tier {
	switch t := Tier(name); t {
	case TierPublic, TierBasic, TierStandard,
		TierProfessional, TierInstitutional, TierAdmin:
		return t
	}
	return TierPublic
}

func (t Tier) String() string { return string(t) }

// validateSpec guards operator-provided tier overrides.
func validateSpec(s Spec) error {
	if s.RatePerSec <= 0 {
		return fmt.Errorf("ratelimit: rate_per_sec must be > 0")
	}
	if s.BurstFactor < 1 {
		return fmt.Errorf("ratelimit: burst_factor must be >= 1")
	}
	if s.WeightPerMin <= 0 {
		return fmt.Errorf("ratelimit: weight_per_min must be > 0")
	}
	return nil
}
