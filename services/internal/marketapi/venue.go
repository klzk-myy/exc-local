// Task 5.3.44 — unified venue-info document (spec §8.9 item 2, §24 #356).
//
// GET /api/v1/exchange-info returns one machine-readable document:
// venue timezone + server time, the canonical 24/5 trading schedule, the
// §8.8 rate-limit/weight table, and per-symbol status, order-type
// permissions and the Task 5.3.35 filter set — replacing N discovery
// calls. The ETag is a content hash over the *stable* members (symbols +
// rate tables); volatile fields (server_time_ms, generated_at) are
// excluded so conditional requests (If-None-Match → 304) actually hold.
// Instrument mutations bump instruments.updated_at → new ETag → clients
// refreshing on the WS system.status signal (Task 15.3.8 / Phase-06)
// observe the change on the next poll.
package marketapi

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"time"

	"exchange/internal/ratelimit"
)

// RateLimitDoc is one tier row of the venue-level §8.3/§8.8 contract.
type RateLimitDoc struct {
	Tier         string `json:"tier"`
	RatePerSec   int64  `json:"rate_per_sec"`
	BurstFactor  int64  `json:"burst_factor"`
	WeightPerMin int64  `json:"weight_per_min"`
	KeyedBy      string `json:"keyed_by"` // "ip" | "account"
}

// RouteWeightDoc is one row of the §8.8 per-route weight table.
type RouteWeightDoc struct {
	Method string `json:"method"` // "" = any
	Path   string `json:"path"`   // prefix match, longest wins
	Weight int64  `json:"weight"`
	Order  bool   `json:"order"` // also feeds the ORDERS counters
}

// SymbolDoc is the per-instrument member of the venue document.
// updated_at_ms is part of the ETag input — an instrument mutation bumps
// the venue tag once the 1min instruments cache expires.
type SymbolDoc struct {
	Instrument
	Filters      []Filter     `json:"filters"`
	OrderTypes   []string     `json:"order_types"`
	TradingHours TradingHours `json:"trading_hours"`
	Settlement   string       `json:"settlement"`
	Permissions  Permissions  `json:"permissions"`
	UpdatedAtMs  int64        `json:"updated_at_ms"`
}

// Permissions discloses the §7.1 status gates a client must respect for
// order entry on this symbol.
type Permissions struct {
	NewOrdersAllowed    bool `json:"new_orders_allowed"`
	MarketOrdersAllowed bool `json:"market_orders_allowed"`
}

// VenueInfo is the Task 5.3.44 document.
type VenueInfo struct {
	Timezone     string           `json:"timezone"` // "UTC"
	ServerTimeMs int64            `json:"server_time_ms"`
	TradingHours TradingHours     `json:"trading_hours"`
	RateLimits   []RateLimitDoc   `json:"rate_limits"`
	RouteWeights []RouteWeightDoc `json:"route_weights"`
	Symbols      []SymbolDoc      `json:"symbols"`
}

// VenueRateLimits renders the ratelimit.Specs tier table in a stable order.
func VenueRateLimits() []RateLimitDoc {
	order := []ratelimit.Tier{
		ratelimit.TierPublic, ratelimit.TierBasic, ratelimit.TierStandard,
		ratelimit.TierProfessional, ratelimit.TierInstitutional, ratelimit.TierAdmin,
	}
	out := make([]RateLimitDoc, 0, len(order))
	for _, t := range order {
		s := ratelimit.SpecOf(t)
		keyed := "account"
		if s.KeyedByIP {
			keyed = "ip"
		}
		out = append(out, RateLimitDoc{
			Tier:         t.String(),
			RatePerSec:   s.RatePerSec,
			BurstFactor:  s.BurstFactor,
			WeightPerMin: s.WeightPerMin,
			KeyedBy:      keyed,
		})
	}
	return out
}

// VenueRouteWeights renders the gateway weight table (Task 5.3.42 seed).
func VenueRouteWeights() []RouteWeightDoc {
	rows := ratelimit.DefaultWeights
	out := make([]RouteWeightDoc, 0, len(rows))
	for _, r := range rows {
		out = append(out, RouteWeightDoc{
			Method: r.Method, Path: r.Path, Weight: r.Weight, Order: r.Order,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Path != out[j].Path {
			return out[i].Path < out[j].Path
		}
		return out[i].Method < out[j].Method
	})
	return out
}

// BuildVenueInfo assembles the venue document at `now`.
func BuildVenueInfo(instruments []Instrument, now time.Time) VenueInfo {
	syms := make([]SymbolDoc, 0, len(instruments))
	for _, inst := range instruments {
		syms = append(syms, SymbolDoc{
			Instrument:   inst,
			Filters:      inst.Filters(),
			OrderTypes:   inst.OrderTypes(),
			TradingHours: VenueTradingHours(),
			Settlement:   inst.SettlementLabel(),
			Permissions: Permissions{
				NewOrdersAllowed:    inst.NewOrdersAllowed(),
				MarketOrdersAllowed: inst.MarketOrdersAllowed(),
			},
			UpdatedAtMs: inst.UpdatedAt.UnixMilli(),
		})
	}
	return VenueInfo{
		Timezone:     "UTC",
		ServerTimeMs: now.UnixMilli(),
		TradingHours: VenueTradingHours(),
		RateLimits:   VenueRateLimits(),
		RouteWeights: VenueRouteWeights(),
		Symbols:      syms,
	}
}

// VenueETag computes the content hash over the stable document members —
// symbols (which carry updated_at) plus the rate tables. server_time_ms is
// deliberately excluded: a clock tick must not invalidate the cache.
// Returns the quoted ETag header value.
func VenueETag(v VenueInfo) (string, error) {
	stable := struct {
		RateLimits   []RateLimitDoc   `json:"rate_limits"`
		RouteWeights []RouteWeightDoc `json:"route_weights"`
		Symbols      []SymbolDoc      `json:"symbols"`
	}{v.RateLimits, v.RouteWeights, v.Symbols}
	body, err := json.Marshal(stable)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(body)
	return `"` + hex.EncodeToString(sum[:16]) + `"`, nil
}
