// Task 5.3.40/5.3.42 — per-route request weights.
//
// The default table below is the gateway-side seed: spec §8.8 item 4
// pins the authoritative tabulation to spec §8.3 (Task 5.3.42 registry
// work). Routes not listed charge DefaultWeight. Weight 0 is legal for
// zero-cost paths (the task's "successful cancel/order paths may carry
// zero request weight") — failed requests still charge, since the charge
// happens at edge admission before handler outcome is known.
package ratelimit

import (
	"net/http"
	"strings"
)

// RouteWeight is one row of the weight table. Path is a prefix pattern;
// the longest matching prefix wins. Order marks placement paths whose
// hits also increment the ORDERS usage counters.
type RouteWeight struct {
	Method string // "" = any method
	Path   string // path prefix, e.g. "/api/v1/orders"
	Weight int64
	Order  bool
}

// DefaultWeight is charged for unlisted routes.
const DefaultWeight int64 = 1

// DefaultWeights is the seeded weight table (superseded entries are the
// Task 5.3.42 spec §8.3 tabulation's job to refine — keep both in sync
// when it lands).
var DefaultWeights = []RouteWeight{
	// Order-path traffic is heavier and feeds the ORDERS counters.
	{Method: http.MethodPost, Path: "/api/v1/orders", Weight: 1, Order: true},
	{Method: http.MethodPut, Path: "/api/v1/orders", Weight: 1},
	{Method: http.MethodDelete, Path: "/api/v1/orders", Weight: 1},
	// Read paths.
	{Method: http.MethodGet, Path: "/api/v1/orders", Weight: 5},
	{Method: http.MethodGet, Path: "/api/v1/klines", Weight: 2},
	{Method: http.MethodGet, Path: "/api/v1/history", Weight: 5},
	{Method: http.MethodGet, Path: "/api/v1/book", Weight: 2},
	{Method: http.MethodGet, Path: "/api/v1/trades", Weight: 2},
	{Method: http.MethodGet, Path: "/api/v1/positions", Weight: 2},
	{Method: http.MethodGet, Path: "/api/v1/account", Weight: 2},
	// Money-moving writes.
	{Method: http.MethodPost, Path: "/api/v1/withdrawals", Weight: 5},
	{Method: http.MethodPost, Path: "/api/v1/transfers", Weight: 5},
}

// WeightTable resolves method+path to a weight.
type WeightTable struct {
	rows []RouteWeight
}

// NewWeightTable builds a resolver; nil rows select DefaultWeights.
func NewWeightTable(rows []RouteWeight) *WeightTable {
	if rows == nil {
		rows = DefaultWeights
	}
	return &WeightTable{rows: rows}
}

// Lookup returns (weight, isOrderPath) for method+path: longest path
// prefix wins; a method-qualified row beats a wildcard row at equal
// prefix length.
func (t *WeightTable) Lookup(method, path string) (weight int64, order bool) {
	bestLen := -1
	bestScore := -1
	for _, r := range t.rows {
		if !strings.HasPrefix(path, r.Path) {
			continue
		}
		if r.Method != "" && r.Method != method {
			continue
		}
		score := len(r.Path)*2 + boolScore(r.Method != "")
		if score > bestScore || (score == bestScore && len(r.Path) > bestLen) {
			bestLen, bestScore = len(r.Path), score
			weight, order = r.Weight, r.Order
		}
	}
	if bestLen < 0 {
		return DefaultWeight, false
	}
	if weight < 0 {
		weight = 0
	}
	return weight, order
}

func boolScore(b bool) int {
	if b {
		return 1
	}
	return 0
}
