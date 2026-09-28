// Task 5.3.40/5.3.34 — handler-level tests with stubbed sources.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"exchange/internal/auth"
	"exchange/internal/middleware"
	"exchange/internal/ratelimit"
	"exchange/internal/settlement"
)

func authedReq(t *testing.T, path string, accountID int64) *http.Request {
	t.Helper()
	req := httptest.NewRequest("GET", path, nil)
	return req.WithContext(auth.WithClaims(req.Context(),
		auth.Claims{Subject: "u", AccountID: accountID, Scopes: []string{"read"}}))
}

// stubRateSource returns a fixed usage view.
type stubRateSource struct {
	usage ratelimit.Usage
	err   error
}

func (s stubRateSource) Usage(context.Context, ratelimit.Identity) (ratelimit.Usage, error) {
	return s.usage, s.err
}
func (s stubRateSource) EffectiveLimit(context.Context, ratelimit.Tier) (int64, int64) {
	return 20, 1200
}

func TestAccountRateLimits(t *testing.T) {
	src := stubRateSource{usage: ratelimit.Usage{
		Tier: TierBasicRat(), RatePerSec: 20,
		RawRequests:   []ratelimit.IntervalUsage{{Interval: "1s", Count: 3, Limit: 20}},
		RequestWeight: []ratelimit.IntervalUsage{{Interval: "1m", Count: 9, Limit: 1200}},
	}}
	h := AccountRateLimits(src,
		TierResolverFromLookup(func(context.Context, int64) (string, error) {
			return "standard", nil
		}))
	rec := httptest.NewRecorder()
	h(rec, authedReq(t, "/api/v1/account/rate-limits", 77))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["tier"] != "standard" {
		t.Fatalf("tier=%v, want standard", body["tier"])
	}
	if body["account_id"].(float64) != 77 {
		t.Fatalf("account_id=%v", body["account_id"])
	}
	if _, ok := body["raw_requests"]; !ok {
		t.Fatal("raw_requests missing")
	}
}

func TestAccountRateLimitsUnauthenticated(t *testing.T) {
	h := AccountRateLimits(stubRateSource{}, nil)
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest("GET", "/api/v1/account/rate-limits", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d, want 401", rec.Code)
	}
	var env map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &env)
	if env["error"] != "UNAUTHORIZED" {
		t.Fatalf("envelope=%v", env)
	}
}

func TestAccountRateLimitsForeignAccountForbidden(t *testing.T) {
	h := AccountRateLimits(stubRateSource{}, nil)
	rec := httptest.NewRecorder()
	h(rec, authedReq(t, "/api/v1/account/rate-limits?account_id=99", 77))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status=%d, want 403", rec.Code)
	}
}

func TestAccountRateLimitsStoreDown(t *testing.T) {
	h := AccountRateLimits(stubRateSource{err: errors.New("down")}, nil)
	rec := httptest.NewRecorder()
	h(rec, authedReq(t, "/api/v1/account/rate-limits", 77))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d, want 503", rec.Code)
	}
}

// TierBasicRat keeps the fixture readable.
func TierBasicRat() ratelimit.Tier { return ratelimit.TierBasic }

// --- filters ---

type stubFilterSource struct {
	f   *InstrumentFilters
	err error
}

func (s stubFilterSource) Filters(context.Context, string) (*InstrumentFilters, error) {
	return s.f, s.err
}

func TestAccountFilters(t *testing.T) {
	h := AccountFilters(stubFilterSource{f: &InstrumentFilters{
		Symbol: "EURUSD", Status: "TRADING", TickSize: "0.00001",
	}})
	req := authedReq(t, "/api/v1/account/filters/EURUSD", 77)
	req.SetPathValue("symbol", "EURUSD")
	rec := httptest.NewRecorder()
	h(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d", rec.Code)
	}
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	f, _ := body["filters"].(map[string]any)
	if f["symbol"] != "EURUSD" {
		t.Fatalf("body=%v", body)
	}
}

func TestAccountFiltersUnknownSymbol404(t *testing.T) {
	h := AccountFilters(stubFilterSource{})
	req := authedReq(t, "/api/v1/account/filters/NOPE", 77)
	req.SetPathValue("symbol", "NOPE")
	rec := httptest.NewRecorder()
	h(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d, want 404", rec.Code)
	}
}

// --- commission ---

type stubCommissionStore struct {
	tiers []settlement.CommissionTier
	vol   decimal.Decimal
	err   error
}

func (s stubCommissionStore) LoadCommissionTiers(context.Context) ([]settlement.CommissionTier, error) {
	return s.tiers, s.err
}
func (s stubCommissionStore) MonthlyVolumeUSD(context.Context, int64, time.Time) (decimal.Decimal, error) {
	return s.vol, nil
}

type stubFeeModel struct{ m settlement.FeeModel }

func (s stubFeeModel) FeeModel(context.Context, int64) (settlement.FeeModel, error) {
	return s.m, nil
}

func TestAccountCommission(t *testing.T) {
	h := AccountCommission(
		stubCommissionStore{
			tiers: []settlement.CommissionTier{{
				TierName:         "t1",
				RatePerMillion:   decimal.NewFromInt(30),
				MinMonthlyVolume: decimal.Zero,
			}},
			vol: decimal.NewFromInt(5000),
		},
		stubFeeModel{m: settlement.FeeModelRawSpreadCommission},
		nil)
	req := authedReq(t, "/api/v1/account/commission/EURUSD", 77)
	req.SetPathValue("symbol", "EURUSD")
	rec := httptest.NewRecorder()
	h(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body["fee_model"] != "RAW_SPREAD_COMMISSION" {
		t.Fatalf("fee_model=%v", body["fee_model"])
	}
	if body["commission_tier"] == nil {
		t.Fatal("commission_tier missing for RAW_SPREAD_COMMISSION model")
	}
	if body["symbol"] != "EURUSD" {
		t.Fatalf("symbol=%v", body["symbol"])
	}
}

// --- tier resolver ---

func TestTierResolverFromLookup(t *testing.T) {
	claims := &auth.Claims{AccountID: 1}
	if got := TierResolverFromLookup(nil)(context.Background(), claims); got != ratelimit.TierBasic {
		t.Fatalf("nil lookup tier=%v", got)
	}
	if got := TierResolverFromLookup(func(context.Context, int64) (string, error) {
		return "institutional", nil
	})(context.Background(), claims); got != ratelimit.TierInstitutional {
		t.Fatalf("tier=%v", got)
	}
	// Lookup error fails closed to Public.
	if got := TierResolverFromLookup(func(context.Context, int64) (string, error) {
		return "", errors.New("down")
	})(context.Background(), claims); got != ratelimit.TierPublic {
		t.Fatalf("lookup error tier=%v, want public", got)
	}
	// Unknown name on an authed account → Basic, never Public.
	if got := TierResolverFromLookup(func(context.Context, int64) (string, error) {
		return "garbage", nil
	})(context.Background(), claims); got != ratelimit.TierBasic {
		t.Fatalf("unknown tier=%v, want basic", got)
	}
	if got := TierResolverFromLookup(nil)(context.Background(), nil); got != ratelimit.TierPublic {
		t.Fatalf("nil claims tier=%v, want public", got)
	}
}

var _ middleware.TierResolver = TierResolverFromLookup(nil) // seam compiles
