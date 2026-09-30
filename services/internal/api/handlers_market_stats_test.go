// Handler tests for Phase-23 Tasks 23.3.6/23.3.10/23.3.11 — the
// market-stats REST surface. Sources are in-memory fakes; the assertions
// target the contract points: 5m delay for non-premium tiers, the
// 100-account cohort floor, insufficient-data markers, and the
// best-effort cache contract.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"exchange/internal/auth"
	"exchange/internal/marketapi"
	"exchange/internal/marketdata"
	"exchange/internal/ratelimit"
	"exchange/pkg/decimal"
)

// ---------------------------------------------------------------------------
// Fakes
// ---------------------------------------------------------------------------

// fakeSentiment implements SentimentAnalyticsSource and records the
// delay horizon each handler computed — the delay assertion reads it.
type fakeSentiment struct {
	cohort     marketdata.PositionCohort
	found      bool
	gotSym     string
	gotHorizon time.Time
	series     []marketdata.LongShortPoint
	seriesErr  error
}

func (f *fakeSentiment) LatestCohort(sym string, horizon time.Time) (marketdata.PositionCohort, bool) {
	f.gotSym, f.gotHorizon = sym, horizon
	return f.cohort, f.found
}

func (f *fakeSentiment) LongShortSeries(_ string, _ int, _ time.Time,
	_ int) ([]marketdata.LongShortPoint, error) {
	return f.series, f.seriesErr
}

type fakeOIAnalytics struct {
	sample   marketdata.OISample
	found    bool
	series   []marketdata.OICandle
	serErr   error
	gotSec   int
	gotLimit int
}

func (f *fakeOIAnalytics) Latest(sym string) (marketdata.OISample, bool) {
	return f.sample, f.found
}

func (f *fakeOIAnalytics) History(_ string, sec, limit int) ([]marketdata.OICandle, error) {
	f.gotSec, f.gotLimit = sec, limit
	return f.series, f.serErr
}

// fakeTakerFlow implements TakerFlowAnalytics; sleep drives the
// timeout test past the guard's request deadline.
type fakeTakerFlow struct {
	buckets        []marketdata.TakerFlowBucket
	err            error
	gotFrom, gotTo time.Time
}

func (f *fakeTakerFlow) TakerFlow(_ context.Context, _ string,
	from, to time.Time) (marketdata.TakerFlow, error) {
	f.gotFrom, f.gotTo = from, to
	return marketdata.TakerFlow{}, nil
}

func (f *fakeTakerFlow) Buckets(ctx context.Context, _ string,
	from, to time.Time, _ int) ([]marketdata.TakerFlowBucket, error) {
	f.gotFrom, f.gotTo = from, to
	if f.err != nil {
		return nil, f.err
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
		return f.buckets, nil
	}
}

func pubTier(context.Context, *auth.Claims) ratelimit.Tier { return ratelimit.TierPublic }
func proTier(context.Context, *auth.Claims) ratelimit.Tier { return ratelimit.TierProfessional }

func statsCohort(accounts int64) marketdata.PositionCohort {
	fifty := decimal.NewFromInt(100)
	return marketdata.PositionCohort{
		Symbol: "EUR/USD", Accounts: accounts,
		LongAccounts: accounts / 2, ShortAccounts: accounts / 2,
		LongNotional: fifty, ShortNotional: fifty,
		GrossNotional: decimal.NewFromInt(200),
		AsOf:          time.Now().Add(-10 * time.Minute),
	}
}

func decodeData(t *testing.T, rec *httptest.ResponseRecorder, want int) map[string]any {
	t.Helper()
	if rec.Code != want {
		t.Fatalf("status = %d, want %d — body %s", rec.Code, want, rec.Body.String())
	}
	var env struct {
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v — %s", err, rec.Body.String())
	}
	return env.Data
}

func decodeErrMap(t *testing.T, rec *httptest.ResponseRecorder, want int) map[string]any {
	t.Helper()
	if rec.Code != want {
		t.Fatalf("status = %d, want %d — body %s", rec.Code, want, rec.Body.String())
	}
	var env map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v — %s", err, rec.Body.String())
	}
	return env
}

// ---------------------------------------------------------------------------
// GET /api/v1/analytics/open-interest/{symbol}
// ---------------------------------------------------------------------------

func TestOpenInterestEndpoint(t *testing.T) {
	oi := &fakeOIAnalytics{
		sample: marketdata.OISample{
			Symbol:       "EUR/USD",
			OpenInterest: decimal.NewFromInt(4200),
			Notional:     decimal.NewFromInt(4557),
			Positions:    9, AsOf: time.Now(),
		},
		found: true,
		series: []marketdata.OICandle{{
			OpenTimeMs: 1, Open: "100", High: "110", Low: "95",
			Close: "105", Samples: 60,
		}},
	}
	deps := &MarketStatsDeps{
		Instruments: &fakeMarketStore{instruments: []marketapi.Instrument{{Symbol: "EUR/USD"}}}, OI: oi, ResolveTier: pubTier,
	}
	rec := httptest.NewRecorder()
	AnalyticsOpenInterest(deps).ServeHTTP(rec,
		histReq("/api/v1/analytics/open-interest/EUR/USD?interval=4h&limit=10", "EUR/USD"))
	d := decodeData(t, rec, http.StatusOK)
	if oi.gotSec != 14400 || oi.gotLimit != 10 {
		t.Fatalf("history args sec=%d limit=%d", oi.gotSec, oi.gotLimit)
	}
	cur := d["current"].(map[string]any)
	if cur["open_interest"] != "4200.00000000" {
		t.Fatalf("current=%v", cur)
	}
	if _, isStr := cur["open_interest"].(string); !isStr {
		t.Fatal("decimal fields must serialize as strings")
	}
	if d["series"] == nil {
		t.Fatal("series missing")
	}
}

func TestOpenInterestBadInterval(t *testing.T) {
	deps := &MarketStatsDeps{
		Instruments: &fakeMarketStore{instruments: []marketapi.Instrument{{Symbol: "EUR/USD"}}}, OI: &fakeOIAnalytics{},
	}
	rec := httptest.NewRecorder()
	AnalyticsOpenInterest(deps).ServeHTTP(rec,
		histReq("/api/v1/analytics/open-interest/EUR/USD?interval=15m", "EUR/USD"))
	decodeErrMap(t, rec, http.StatusBadRequest)
}

func TestOpenInterestNoDataMarksInsufficient(t *testing.T) {
	deps := &MarketStatsDeps{
		Instruments: &fakeMarketStore{instruments: []marketapi.Instrument{{Symbol: "EUR/USD"}}}, OI: &fakeOIAnalytics{found: false},
	}
	rec := httptest.NewRecorder()
	AnalyticsOpenInterest(deps).ServeHTTP(rec,
		histReq("/api/v1/analytics/open-interest/EUR/USD", "EUR/USD"))
	d := decodeData(t, rec, http.StatusOK)
	if d["current"] != nil || d["insufficient_data"] != true {
		t.Fatalf("no-data symbol must mark insufficient_data, not 0: %v", d)
	}
}

// ---------------------------------------------------------------------------
// GET /api/v1/analytics/long-short-ratio/{symbol}
// ---------------------------------------------------------------------------

func TestLongShortRatioDelayEnforced(t *testing.T) {
	sent := &fakeSentiment{cohort: statsCohort(150), found: true,
		series: []marketdata.LongShortPoint{{
			BucketStartMs: 1, Accounts: 150, LongRatio: "0.500000",
		}}}
	deps := &MarketStatsDeps{
		Instruments: &fakeMarketStore{instruments: []marketapi.Instrument{{Symbol: "EUR/USD"}}}, Sentiment: sent,
		ResolveTier: pubTier,
		Now:         func() time.Time { return time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC) },
	}
	rec := httptest.NewRecorder()
	AnalyticsLongShortRatio(deps).ServeHTTP(rec,
		histReq("/api/v1/analytics/long-short-ratio/EUR/USD?period=5m", "EUR/USD"))
	d := decodeData(t, rec, http.StatusOK)
	// Free tier: horizon must be exactly now−5m.
	want := deps.now().Add(-marketdata.SentimentPublicationDelay)
	if !sent.gotHorizon.Equal(want) {
		t.Fatalf("horizon=%v want %v (5m delayed)", sent.gotHorizon, want)
	}
	if d["delayed"] != true || d["delay_ms"].(float64) != 300000 {
		t.Fatalf("delay fields=%v", d)
	}
}

func TestLongShortRatioPremiumRealtime(t *testing.T) {
	sent := &fakeSentiment{cohort: statsCohort(150), found: true}
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	deps := &MarketStatsDeps{
		Instruments: &fakeMarketStore{instruments: []marketapi.Instrument{{Symbol: "EUR/USD"}}}, Sentiment: sent,
		ResolveTier: proTier,
		Now:         func() time.Time { return now },
	}
	rec := httptest.NewRecorder()
	AnalyticsLongShortRatio(deps).ServeHTTP(rec,
		histReq("/api/v1/analytics/long-short-ratio/EUR/USD", "EUR/USD"))
	d := decodeData(t, rec, http.StatusOK)
	if !sent.gotHorizon.Equal(now) {
		t.Fatalf("premium horizon=%v want now", sent.gotHorizon)
	}
	if d["delayed"] != false {
		t.Fatalf("delayed=%v", d["delayed"])
	}
}

func TestLongShortRatioCohortFloor422(t *testing.T) {
	sent := &fakeSentiment{cohort: statsCohort(37), found: true}
	deps := &MarketStatsDeps{
		Instruments: &fakeMarketStore{instruments: []marketapi.Instrument{{Symbol: "EUR/USD"}}}, Sentiment: sent,
		ResolveTier: pubTier,
	}
	rec := httptest.NewRecorder()
	AnalyticsLongShortRatio(deps).ServeHTTP(rec,
		histReq("/api/v1/analytics/long-short-ratio/EUR/USD", "EUR/USD"))
	env := decodeErrMap(t, rec, http.StatusUnprocessableEntity)
	if !strings.Contains(rec.Body.String(), "INSUFFICIENT_COHORT") {
		t.Fatalf("expected INSUFFICIENT_COHORT: %v", env)
	}
}

func TestLongShortRatioBadPeriod(t *testing.T) {
	deps := &MarketStatsDeps{
		Instruments: &fakeMarketStore{instruments: []marketapi.Instrument{{Symbol: "EUR/USD"}}},
		Sentiment:   &fakeSentiment{},
	}
	rec := httptest.NewRecorder()
	AnalyticsLongShortRatio(deps).ServeHTTP(rec,
		histReq("/api/v1/analytics/long-short-ratio/EUR/USD?period=7d", "EUR/USD"))
	decodeErrMap(t, rec, http.StatusBadRequest)
}

// ---------------------------------------------------------------------------
// GET /api/v1/market/positioning
// ---------------------------------------------------------------------------

func TestMarketPositioning(t *testing.T) {
	sent := &fakeSentiment{cohort: statsCohort(200), found: true}
	deps := &MarketStatsDeps{
		Instruments: &fakeMarketStore{instruments: []marketapi.Instrument{{Symbol: "EUR/USD"}}}, Sentiment: sent,
		ResolveTier: pubTier,
	}
	rec := httptest.NewRecorder()
	MarketPositioning(deps).ServeHTTP(rec, httptest.NewRequest(
		http.MethodGet, "/api/v1/market/positioning?symbol=EUR/USD", nil))
	d := decodeData(t, rec, http.StatusOK)
	if d["accounts"].(float64) != 200 {
		t.Fatalf("accounts=%v", d["accounts"])
	}
	if d["long_ratio"] != "0.500000" || d["long_short_ratio"] != "1.000000" {
		t.Fatalf("ratios=%v", d)
	}
	if _, ok := d["concentration"]; !ok {
		t.Fatal("concentration bands missing")
	}
	// Privacy: no account identifier anywhere in the wire.
	for _, bad := range []string{"account_id", "position_id", "order_id"} {
		if strings.Contains(rec.Body.String(), bad) {
			t.Fatalf("payload leaks %q: %s", bad, rec.Body.String())
		}
	}
}

func TestMarketPositioningSuppressed(t *testing.T) {
	sent := &fakeSentiment{cohort: statsCohort(50), found: true}
	deps := &MarketStatsDeps{
		Instruments: &fakeMarketStore{instruments: []marketapi.Instrument{{Symbol: "EUR/USD"}}}, Sentiment: sent,
		ResolveTier: pubTier,
	}
	rec := httptest.NewRecorder()
	MarketPositioning(deps).ServeHTTP(rec, httptest.NewRequest(
		http.MethodGet, "/api/v1/market/positioning?symbol=EUR/USD", nil))
	decodeErrMap(t, rec, http.StatusUnprocessableEntity)
}

func TestMarketPositioningMissingSymbol(t *testing.T) {
	deps := &MarketStatsDeps{Sentiment: &fakeSentiment{}}
	rec := httptest.NewRecorder()
	MarketPositioning(deps).ServeHTTP(rec, httptest.NewRequest(
		http.MethodGet, "/api/v1/market/positioning", nil))
	decodeErrMap(t, rec, http.StatusBadRequest)
}

// ---------------------------------------------------------------------------
// GET /api/v1/market/taker-volume + /api/v1/analytics/taker-flow
// ---------------------------------------------------------------------------

func bucketWithAccounts(start time.Time, accts int64) marketdata.TakerFlowBucket {
	return marketdata.TakerFlowBucket{
		BucketStart: start,
		Flow: marketdata.TakerFlow{
			BuyNotional:  decimal.NewFromInt(600),
			SellNotional: decimal.NewFromInt(300),
			BuyVolume:    decimal.NewFromInt(60),
			SellVolume:   decimal.NewFromInt(30),
			Trades:       100, Accounts: accts,
		},
	}
}

func TestMarketTakerVolume(t *testing.T) {
	b0 := time.Date(2026, 10, 1, 11, 50, 0, 0, time.UTC)
	flow := &fakeTakerFlow{buckets: []marketdata.TakerFlowBucket{
		bucketWithAccounts(b0, 120),                   // publishable
		bucketWithAccounts(b0.Add(5*time.Minute), 30), // suppressed cell
	}}
	now := time.Date(2026, 10, 1, 12, 30, 0, 0, time.UTC)
	deps := &MarketStatsDeps{
		Instruments: &fakeMarketStore{instruments: []marketapi.Instrument{{Symbol: "EUR/USD"}}}, Flow: flow,
		ResolveTier: pubTier, Now: func() time.Time { return now },
	}
	rec := httptest.NewRecorder()
	MarketTakerVolume(deps).ServeHTTP(rec, httptest.NewRequest(
		http.MethodGet,
		"/api/v1/market/taker-volume?symbol=EUR/USD&interval=5m&limit=2", nil))
	d := decodeData(t, rec, http.StatusOK)
	// Delay enforced inside the query bound: window end = now−5m.
	if !flow.gotTo.Equal(now.Add(-marketdata.SentimentPublicationDelay)) {
		t.Fatalf("window end=%v want now−5m", flow.gotTo)
	}
	pts := d["points"].([]any)
	if len(pts) != 2 {
		t.Fatalf("points=%v", pts)
	}
	p0 := pts[0].(map[string]any)
	p1 := pts[1].(map[string]any)
	if p0["suppressed"] != false || p0["buy_sell_ratio"] != "2.000000" {
		t.Fatalf("p0=%v", p0)
	}
	if p1["suppressed"] != true {
		t.Fatalf("below-floor bucket must mark suppressed: %v", p1)
	}
	if _, leaked := p1["buy_notional"]; leaked {
		t.Fatal("suppressed bucket leaked notional")
	}
}

func TestTakerVolumeAllSuppressed422(t *testing.T) {
	flow := &fakeTakerFlow{buckets: []marketdata.TakerFlowBucket{
		bucketWithAccounts(time.Now(), 10),
	}}
	deps := &MarketStatsDeps{
		Instruments: &fakeMarketStore{instruments: []marketapi.Instrument{{Symbol: "EUR/USD"}}}, Flow: flow, ResolveTier: pubTier,
	}
	rec := httptest.NewRecorder()
	AnalyticsTakerFlow(deps).ServeHTTP(rec,
		histReq("/api/v1/analytics/taker-flow/EUR/USD", "EUR/USD"))
	env := decodeErrMap(t, rec, http.StatusUnprocessableEntity)
	if !strings.Contains(rec.Body.String(), "INSUFFICIENT_COHORT") {
		t.Fatalf("want INSUFFICIENT_COHORT: %v", env)
	}
}

func TestTakerFlowDeadlineMaps504(t *testing.T) {
	flow := &fakeTakerFlow{err: context.DeadlineExceeded}
	deps := &MarketStatsDeps{
		Instruments: &fakeMarketStore{instruments: []marketapi.Instrument{{Symbol: "EUR/USD"}}}, Flow: flow, ResolveTier: pubTier,
	}
	rec := httptest.NewRecorder()
	MarketTakerVolume(deps).ServeHTTP(rec, httptest.NewRequest(
		http.MethodGet, "/api/v1/market/taker-volume?symbol=EUR/USD", nil))
	decodeErrMap(t, rec, http.StatusGatewayTimeout)
}

// ---------------------------------------------------------------------------
// GET /api/v1/market/performance
// ---------------------------------------------------------------------------

// statsDailyFake feeds the VenuePerformanceService under test — the
// service's own seams are exercised end-to-end through the handler.
type statsDailyFake struct{ rows []marketdata.PairDayStats }

func (f *statsDailyFake) DailyStats(_ context.Context, _, _ time.Time) ([]marketdata.PairDayStats, error) {
	return f.rows, nil
}

func statsPerfDay(sym string, day time.Time, spread, lat, fr string) marketdata.PairDayStats {
	s := decimal.MustFromString(spread)
	l := decimal.MustFromString(lat)
	r := decimal.MustFromString(fr)
	sub, fil := int64(100), int64(98)
	return marketdata.PairDayStats{
		Symbol: sym, Day: day,
		AvgSpreadBps: &s, MedianExecLatencyMs: &l, FillRate: &r,
		OrdersSubmitted: &sub, OrdersFilled: &fil,
		Fills: 500, VolumeQuote: decimal.NewFromInt(1_000_000),
	}
}

func perfDeps(t *testing.T, cache *fakeHistoryKV) *MarketStatsDeps {
	t.Helper()
	svc := marketdata.NewVenuePerformanceService(
		marketdata.VenuePerformanceConfig{
			Symbols: []string{"EUR/USD"},
		},
		marketdata.VenuePerformanceDeps{
			Daily: &statsDailyFake{rows: []marketdata.PairDayStats{
				statsPerfDay("EUR/USD", time.Now().AddDate(0, 0, -1),
					"0.40", "12.0", "0.98"),
			}},
		})
	d := &MarketStatsDeps{Performance: svc}
	if cache != nil { // typed nil would poison d.Cache != nil checks
		d.Cache = cache
	}
	return d
}

func TestMarketPerformance(t *testing.T) {
	rec := httptest.NewRecorder()
	MarketPerformance(perfDeps(t, nil)).ServeHTTP(rec, httptest.NewRequest(
		http.MethodGet, "/api/v1/market/performance", nil))
	d := decodeData(t, rec, http.StatusOK)
	if d["status"] != "ok" {
		t.Fatalf("status=%v", d["status"])
	}
	if d["delay_ms"].(float64) != 900000 {
		t.Fatalf("delay_ms=%v want 900000", d["delay_ms"])
	}
	pairs := d["pairs"].([]any)
	if len(pairs) != 1 {
		t.Fatalf("pairs=%v", pairs)
	}
	if strings.Contains(rec.Body.String(), "account_id") {
		t.Fatal("performance payload leaked account field")
	}
}

func TestMarketPerformanceCache(t *testing.T) {
	kv := newFakeHistoryKV()
	d := perfDeps(t, kv)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/market/performance", nil)
	rec1 := httptest.NewRecorder()
	MarketPerformance(d).ServeHTTP(rec1, req)
	decodeData(t, rec1, http.StatusOK)
	if len(kv.m) != 1 {
		t.Fatalf("payload not cached: %v", kv.m)
	}
	if kv.ttl[marketdata.PerformanceCacheKey] != marketdata.PerformanceCacheTTL {
		t.Fatalf("cache ttl=%v want 5m", kv.ttl[marketdata.PerformanceCacheKey])
	}
	// Second hit must be served from cache — poison the store to prove it.
	d.Performance = nil
	rec2 := httptest.NewRecorder()
	MarketPerformance(d).ServeHTTP(rec2, req)
	decodeData(t, rec2, http.StatusOK)
}

func TestMarketPerformanceRedisOutage(t *testing.T) {
	kv := newFakeHistoryKV()
	kv.err = errors.New("redis unreachable")
	d := perfDeps(t, kv)
	rec := httptest.NewRecorder()
	MarketPerformance(d).ServeHTTP(rec, httptest.NewRequest(
		http.MethodGet, "/api/v1/market/performance", nil))
	decodeData(t, rec, http.StatusOK) // outage = best-effort pass-through
}

func TestMarketPerformanceNotConfigured(t *testing.T) {
	rec := httptest.NewRecorder()
	MarketPerformance(&MarketStatsDeps{}).ServeHTTP(rec, httptest.NewRequest(
		http.MethodGet, "/api/v1/market/performance", nil))
	decodeErrMap(t, rec, http.StatusServiceUnavailable)
}
