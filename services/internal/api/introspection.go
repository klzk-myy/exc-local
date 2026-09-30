// Task 5.3.40 — rate-limit and account trading introspection
// (§24 #288). Handlers:
//
//	GET /api/v1/account/rate-limits          — multi-interval usage view
//	GET /api/v1/account/filters/{symbol}     — effective instrument filters
//	GET /api/v1/account/commission/{symbol}  — effective commission/fee view
//
// All three require an authenticated account context (401 UNAUTHORIZED
// without claims; 403 FORBIDDEN when the queried account differs from
// the claim's). Prevented-match / amendment / SOR-allocation queries are
// the data-plane counterpart owned by Tasks 5.3.37 + 18.3.14 — their
// stores plug into the same handler seam when those land.
package api

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"

	"exchange/internal/auth"
	"exchange/internal/gateway"
	"exchange/internal/middleware"
	"exchange/internal/ratelimit"
	"exchange/internal/settlement"
)

// ---------------------------------------------------------------------------
// GET /api/v1/account/rate-limits
// ---------------------------------------------------------------------------

// RateLimitSource is the seam the introspection handler needs.
type RateLimitSource interface {
	Usage(ctx context.Context, id ratelimit.Identity) (ratelimit.Usage, error)
	EffectiveLimit(ctx context.Context, t ratelimit.Tier) (ratePerSec, weightPerMin int64)
}

// AccountRateLimits serves GET /api/v1/account/rate-limits.
// resolveTier maps the caller's claims to a tier (same resolver the
// middleware uses so clients see the limits actually enforced).
func AccountRateLimits(src RateLimitSource, resolveTier middleware.TierResolver) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims := auth.ClaimsFrom(r.Context())
		if claims == nil || claims.AccountID == 0 {
			WriteError(w, "UNAUTHORIZED",
				"authentication required", gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		// An account_id query override is honored only when it matches
		// the claim — clients may never introspect a foreign account.
		if q := r.URL.Query().Get("account_id"); q != "" {
			n, err := strconv.ParseInt(q, 10, 64)
			if err != nil || n != claims.AccountID {
				WriteError(w, "FORBIDDEN",
					"cannot introspect another account",
					gateway.RequestIDFrom(r.Context()), nil)
				return
			}
		}
		tier := ratelimit.TierBasic
		if resolveTier != nil {
			tier = resolveTier(r.Context(), claims)
		}
		id := ratelimit.Identity{
			Tier: tier,
			Key:  strconv.FormatInt(claims.AccountID, 10),
			IP:   middleware.ClientIP(r, false),
		}
		usage, err := src.Usage(r.Context(), id)
		if err != nil {
			WriteError(w, "SERVICE_DEGRADED",
				"rate-limit store unavailable",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		rate, weightQuota := src.EffectiveLimit(r.Context(), tier)
		WriteJSON(w, http.StatusOK, map[string]any{
			"account_id":           claims.AccountID,
			"tier":                 string(tier),
			"rate_per_sec":         rate,
			"weight_quota_per_min": weightQuota,
			"raw_requests":         usage.RawRequests,
			"request_weight":       usage.RequestWeight,
			"orders":               usage.Orders,
		})
	}
}

// TierLookup resolves the rate-limit tier label for an account —
// implemented over PG by PgTierResolver; tests inject a static func.
type TierLookup func(ctx context.Context, accountID int64) (string, error)

// PgTierResolver builds a middleware.TierResolver over a PG pool,
// reading accounts.fee_tier_id → fee_tiers.tier_name (the spec §8.3 tier
// names basic/standard/professional/institutional match the fee-tier
// ladder). NULL/absent fee tier ⇒ TierBasic; store errors fail closed
// to TierPublic.
//
// Phase-08.5 Task 8.5.3.2: a DEMO account resolves to the demo tier
// (2× the production Basic quota) regardless of any fee_tier binding —
// the sandbox must never hand a demo identity a production-tier quota.
// PgTierLookup is exported separately so the WS/L3 resolver closures
// (different claims type, same policy) share one query definition.
func PgTierResolver(pool *pgxpool.Pool) middleware.TierResolver {
	return TierResolverFromLookup(PgTierLookup(pool))
}

// PgTierLookup is the TierLookup half of PgTierResolver — same query,
// same DEMO substitution, reusable by every TierResolver seam.
func PgTierLookup(pool *pgxpool.Pool) TierLookup {
	return func(ctx context.Context, accountID int64) (string, error) {
		var name *string
		var accountType string
		err := pool.QueryRow(ctx, `
			SELECT a.account_type::text, t.tier_name FROM accounts a
			LEFT JOIN fee_tiers t ON t.id = a.fee_tier_id
			WHERE a.id = $1`, accountID).Scan(&accountType, &name)
		if err != nil {
			return "", err
		}
		if accountType == "DEMO" {
			return string(ratelimit.TierDemo), nil
		}
		if name == nil {
			return "", nil
		}
		return *name, nil
	}
}

// TierResolverFromLookup adapts a TierLookup to middleware.TierResolver.
// Lookup error ⇒ TierPublic; empty/unknown label on an authenticated
// account ⇒ TierBasic (never anonymous — the account proved identity).
func TierResolverFromLookup(lookup TierLookup) middleware.TierResolver {
	return func(ctx context.Context, claims *auth.Claims) ratelimit.Tier {
		if claims == nil {
			return ratelimit.TierPublic
		}
		if lookup == nil {
			return ratelimit.TierBasic
		}
		name, err := lookup(ctx, claims.AccountID)
		if err != nil {
			return ratelimit.TierPublic
		}
		if name == "" {
			return ratelimit.TierBasic
		}
		t := ratelimit.ParseTier(name)
		if t == ratelimit.TierPublic {
			return ratelimit.TierBasic
		}
		return t
	}
}

// ---------------------------------------------------------------------------
// GET /api/v1/account/filters/{symbol}
// ---------------------------------------------------------------------------

// InstrumentFilters is the effective filter set for one symbol — the
// same structured-filter shape Task 5.3.35 exposes on /instruments.
type InstrumentFilters struct {
	Symbol           string `json:"symbol"`
	Status           string `json:"status"`
	TickSize         string `json:"tick_size"`
	LotSize          string `json:"lot_size"`
	MinOrderQty      string `json:"min_order_qty"`
	MaxOrderQty      string `json:"max_order_qty"`
	MinNotional      string `json:"min_notional"`
	PriceBandPctUp   string `json:"price_band_pct_up"`
	PriceBandPctDown string `json:"price_band_pct_down"`
	MaxLeverage      int    `json:"max_leverage"`
	SettlementCycle  int    `json:"settlement_cycle"`
}

// InstrumentFilterSource resolves per-symbol filters; nil,nil = unknown
// symbol → 404.
type InstrumentFilterSource interface {
	Filters(ctx context.Context, symbol string) (*InstrumentFilters, error)
}

// AccountFilters serves GET /api/v1/account/filters/{symbol}. The
// response pairs the instrument's filter set with the account context so
// a client can pre-validate against the rules that will actually apply.
func AccountFilters(src InstrumentFilterSource) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims := auth.ClaimsFrom(r.Context())
		if claims == nil || claims.AccountID == 0 {
			WriteError(w, "UNAUTHORIZED",
				"authentication required", gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		symbol := r.PathValue("symbol")
		if symbol == "" {
			WriteError(w, "INVALID_REQUEST",
				"missing symbol", gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		f, err := src.Filters(r.Context(), symbol)
		if err != nil {
			WriteError(w, "SERVICE_DEGRADED",
				"instrument store unavailable",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		if f == nil {
			WriteError(w, "NOT_FOUND",
				"unknown symbol", gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{
			"account_id": claims.AccountID,
			"filters":    f,
		})
	}
}

// ---------------------------------------------------------------------------
// GET /api/v1/account/commission/{symbol}
// ---------------------------------------------------------------------------

// CommissionStore covers the commission engine's read side (the PG store
// in internal/settlement satisfies it).
type CommissionStore interface {
	LoadCommissionTiers(ctx context.Context) ([]settlement.CommissionTier, error)
	MonthlyVolumeUSD(ctx context.Context, accountID int64, month time.Time) (decimal.Decimal, error)
}

// FeeModelSource resolves the account's §5.41 pricing plan.
type FeeModelSource interface {
	FeeModel(ctx context.Context, accountID int64) (settlement.FeeModel, error)
}

// AccountCommission serves GET /api/v1/account/commission/{symbol}:
// fee model + resolved commission tier + month-to-date volume. The
// symbol parameter scopes the report; commission tiers are global per
// account so the response is identical across symbols, and the symbol is
// echoed for client bookkeeping.
func AccountCommission(store CommissionStore, models FeeModelSource, clock func() time.Time) http.HandlerFunc {
	if clock == nil {
		clock = time.Now
	}
	return func(w http.ResponseWriter, r *http.Request) {
		claims := auth.ClaimsFrom(r.Context())
		if claims == nil || claims.AccountID == 0 {
			WriteError(w, "UNAUTHORIZED",
				"authentication required", gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		symbol := r.PathValue("symbol")
		model, err := models.FeeModel(r.Context(), claims.AccountID)
		if err != nil {
			WriteError(w, "SERVICE_DEGRADED",
				"fee model unavailable", gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		tiers, err := store.LoadCommissionTiers(r.Context())
		if err != nil {
			WriteError(w, "SERVICE_DEGRADED",
				"commission tiers unavailable", gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		vol, err := store.MonthlyVolumeUSD(r.Context(), claims.AccountID, clock().UTC())
		if err != nil {
			WriteError(w, "SERVICE_DEGRADED",
				"monthly volume unavailable", gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		resp := map[string]any{
			"account_id":         claims.AccountID,
			"symbol":             symbol,
			"fee_model":          string(model),
			"monthly_volume_usd": vol.String(),
		}
		if model == settlement.FeeModelRawSpreadCommission {
			if tier, ok := settlement.TierForVolume(tiers, vol); ok {
				resp["commission_tier"] = map[string]any{
					"name":               tier.TierName,
					"rate_per_lot":       tier.RatePerLot.String(),
					"rate_per_million":   tier.RatePerMillion.String(),
					"min_monthly_volume": tier.MinMonthlyVolume.String(),
				}
			}
		}
		WriteJSON(w, http.StatusOK, resp)
	}
}
