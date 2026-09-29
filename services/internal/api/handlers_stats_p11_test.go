// Handler tests — Phase-11 Task 11.3.5 (24h market statistics) and Task
// 11.3.9 (fee-estimate + fee-schedule admin + currency conversion).
// httptest + fake service seams; persistence is covered by the
// marketapi/funding PG integration tests.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"exchange/internal/funding"
	"exchange/internal/marketapi"
	"exchange/pkg/decimal"
	excerrors "exchange/pkg/errors"
)

// --- fakes ---------------------------------------------------------------

type fakeFeeEstimator struct {
	res *funding.FeeEstimate
	err error
	got funding.FeeEstimateRequest
}

func (f *fakeFeeEstimator) Estimate(_ context.Context,
	req funding.FeeEstimateRequest) (*funding.FeeEstimate, error) {
	f.got = req
	return f.res, f.err
}

type fakeFeeAdmin struct {
	tier   *funding.FundingFeeTier
	rows   []funding.FundingFeeTier
	err    error
	gotF   funding.FeeTierFilter
	gotID  int64
	gotCre funding.FeeTierCreate
	gotUpd funding.FeeTierUpdate
}

func (f *fakeFeeAdmin) List(_ context.Context, _ funding.FeeAdminActor,
	fl funding.FeeTierFilter) ([]funding.FundingFeeTier, error) {
	f.gotF = fl
	return f.rows, f.err
}

func (f *fakeFeeAdmin) Get(_ context.Context, _ funding.FeeAdminActor,
	id int64) (*funding.FundingFeeTier, error) {
	f.gotID = id
	return f.tier, f.err
}

func (f *fakeFeeAdmin) Versions(_ context.Context, _ funding.FeeAdminActor,
	id int64) ([]funding.FundingFeeTier, error) {
	f.gotID = id
	return f.rows, f.err
}

func (f *fakeFeeAdmin) Create(_ context.Context, _ funding.FeeAdminActor,
	req funding.FeeTierCreate) (*funding.FundingFeeTier, error) {
	f.gotCre = req
	return f.tier, f.err
}

func (f *fakeFeeAdmin) Update(_ context.Context, _ funding.FeeAdminActor,
	id int64, req funding.FeeTierUpdate) (*funding.FundingFeeTier, error) {
	f.gotID, f.gotUpd = id, req
	return f.tier, f.err
}

func (f *fakeFeeAdmin) Retire(_ context.Context, _ funding.FeeAdminActor,
	id int64) (*funding.FundingFeeTier, error) {
	f.gotID = id
	return f.tier, f.err
}

type fakeConvQuoter struct {
	res  *funding.ConversionResult
	rows []funding.ConversionRecord
	err  error
	got  funding.ConversionRequest
}

func (f *fakeConvQuoter) Quote(_ context.Context,
	req funding.ConversionRequest) (*funding.ConversionResult, error) {
	f.got = req
	return f.res, f.err
}

func (f *fakeConvQuoter) History(_ context.Context, _ int64,
	limit int) ([]funding.ConversionRecord, error) {
	return f.rows[:min(limit, len(f.rows))], f.err
}

// --- Stats (Task 11.3.5) ---------------------------------------------------

func TestStats24hAll(t *testing.T) {
	st := marketapi.Stats24h{Symbol: "EUR/USD", Window: "24h",
		Volume: "1000", QuoteVolume: "1084", TradeCount: 3}
	deps := &MarketDeps{
		Store: &fakeMarketStore{statsAll: []marketapi.Stats24h{st}},
		Cache: marketapi.NewCache(nil),
	}
	rec := doReq(MarketStats24hAll(deps), http.MethodGet,
		"/api/v1/stats/24h", nil, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
	}
	m := decode(t, rec)
	if m["count"].(float64) != 1 || m["window"] != "24h" {
		t.Fatalf("payload %+v", m)
	}
	data := m["data"].([]any)
	if len(data) != 1 || data[0].(map[string]any)["symbol"] != "EUR/USD" {
		t.Fatalf("data %+v", data)
	}
	if _, ok := m["server_time_ms"]; !ok {
		t.Fatal("missing server_time_ms")
	}
}

func TestStats24hAll_StoreErrorDegraded(t *testing.T) {
	deps := &MarketDeps{
		Store: &fakeMarketStore{err: errors.New("pg down")},
		Cache: marketapi.NewCache(nil),
	}
	rec := doReq(MarketStats24hAll(deps), http.MethodGet,
		"/api/v1/stats/24h", nil, "")
	if rec.Code != http.StatusServiceUnavailable ||
		problemCode(t, rec) != "SERVICE_DEGRADED" {
		t.Fatalf("got %d %s", rec.Code, rec.Body.String())
	}
}

func TestStats24hSymbol(t *testing.T) {
	st := marketapi.Stats24h{Symbol: "EUR/USD", Window: "24h",
		Volume: "5", QuoteVolume: "5.5", TradeCount: 1}
	deps := &MarketDeps{
		Store: &fakeMarketStore{stats: &st},
		Cache: marketapi.NewCache(nil),
	}
	rec := doReq(MarketStats24h(deps), http.MethodGet,
		"/api/v1/stats/24h/EURUSD", map[string]string{"symbol": "EUR/USD"}, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
	}
	var body marketapi.Stats24h
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Symbol != "EUR/USD" || body.TradeCount != 1 {
		t.Fatalf("body %+v", body)
	}
}

func TestStats24hSymbol_Unknown404(t *testing.T) {
	deps := &MarketDeps{
		Store: &fakeMarketStore{stats: &marketapi.Stats24h{Symbol: "EUR/USD"}},
		Cache: marketapi.NewCache(nil),
	}
	rec := doReq(MarketStats24h(deps), http.MethodGet,
		"/api/v1/stats/24h/ZZZ", map[string]string{"symbol": "ZZZ/ZZZ"}, "")
	if rec.Code != http.StatusNotFound || problemCode(t, rec) != "NOT_FOUND" {
		t.Fatalf("got %d %s", rec.Code, rec.Body.String())
	}
}

// --- Fee estimate + admin (Task 11.3.9) ------------------------------------

func TestFundingFeeEstimate(t *testing.T) {
	svc := &fakeFeeEstimator{res: &funding.FeeEstimate{
		Rail: "SWIFT", Currency: "USD", Direction: "WITHDRAWAL",
		AccountID: 7, AccountTier: "T2",
		Amount:       decimal.NewFromInt(10_000),
		ScheduledFee: decimal.NewFromInt(15),
		Fee:          decimal.NewFromInt(15),
		NetAmount:    decimal.NewFromInt(9_985),
	}}
	h := FundingFeeEstimate(svc)

	// unauthenticated → 401
	rec := newRec()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/x", strings.NewReader("{}")))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauth: %d", rec.Code)
	}

	// malformed body → 400
	rec = newRec()
	h.ServeHTTP(rec, userCtx(httptest.NewRequest(http.MethodPost, "/x",
		strings.NewReader("{bad")), 7))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("malformed: %d", rec.Code)
	}

	// non-positive amount → 400
	rec = newRec()
	h.ServeHTTP(rec, userCtx(httptest.NewRequest(http.MethodPost, "/x",
		strings.NewReader(`{"rail":"swift","currency":"usd","direction":"withdrawal","amount":"0"}`)), 7))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("amount: %d", rec.Code)
	}

	// happy — account comes from the claims, never the body
	rec = newRec()
	h.ServeHTTP(rec, userCtx(httptest.NewRequest(http.MethodPost, "/x",
		strings.NewReader(`{"rail":"swift","currency":"usd","direction":"withdrawal","amount":"10000"}`)), 7))
	if rec.Code != http.StatusOK {
		t.Fatalf("happy: %d %s", rec.Code, rec.Body.String())
	}
	if svc.got.AccountID != 7 || svc.got.Rail != "swift" {
		t.Fatalf("req %+v", svc.got)
	}
	var est funding.FeeEstimate
	if err := json.Unmarshal(rec.Body.Bytes(), &est); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if est.Fee.String() != "15" || est.NetAmount.String() != "9985" {
		t.Fatalf("est %+v", est)
	}

	// coded service error → mapped status
	svc.res, svc.err = nil, excerrors.New("FEE_TIER_NOT_FOUND", "no schedule")
	rec = newRec()
	h.ServeHTTP(rec, userCtx(httptest.NewRequest(http.MethodPost, "/x",
		strings.NewReader(`{"rail":"swift","currency":"usd","direction":"withdrawal","amount":"10"}`)), 7))
	if rec.Code != http.StatusNotFound || problemCode(t, rec) != "FEE_TIER_NOT_FOUND" {
		t.Fatalf("tier404: %d %s", rec.Code, rec.Body.String())
	}
	svc.err = excerrors.New(funding.CodeFundingFeeExceedsAmount, "fee ≥ amount")
	rec = newRec()
	h.ServeHTTP(rec, userCtx(httptest.NewRequest(http.MethodPost, "/x",
		strings.NewReader(`{"rail":"swift","currency":"usd","direction":"withdrawal","amount":"10"}`)), 7))
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("exceeds: %d", rec.Code)
	}
}

func TestAdminFundingFee_AuthGate(t *testing.T) {
	handlers := map[string]http.HandlerFunc{
		"list":     AdminFundingFeeList(&fakeFeeAdmin{}, false),
		"create":   AdminFundingFeeCreate(&fakeFeeAdmin{}, false),
		"get":      AdminFundingFeeGet(&fakeFeeAdmin{}, false),
		"update":   AdminFundingFeeUpdate(&fakeFeeAdmin{}, false),
		"retire":   AdminFundingFeeRetire(&fakeFeeAdmin{}, false),
		"versions": AdminFundingFeeVersions(&fakeFeeAdmin{}, false),
	}
	for name, h := range handlers {
		rec := newRec()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/x",
			strings.NewReader("{}")))
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s unauth: %d", name, rec.Code)
		}
	}
}

func TestAdminFundingFeeList_Filters(t *testing.T) {
	svc := &fakeFeeAdmin{rows: []funding.FundingFeeTier{{ID: 1, Rail: "SWIFT"}}}
	rec := newRec()
	req := adminCtxB(httptest.NewRequest(http.MethodGet,
		"/x?rail=swift&currency=usd&direction=withdrawal&tier=t2&all=1&limit=10", nil))
	AdminFundingFeeList(svc, false).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d %s", rec.Code, rec.Body.String())
	}
	if svc.gotF.Rail != "SWIFT" || svc.gotF.Currency != "USD" ||
		svc.gotF.Direction != "WITHDRAWAL" || svc.gotF.Tier != "T2" ||
		!svc.gotF.IncludeAll || svc.gotF.Limit != 10 {
		t.Fatalf("filter %+v", svc.gotF)
	}
}

func TestAdminFundingFeeCreate(t *testing.T) {
	svc := &fakeFeeAdmin{tier: &funding.FundingFeeTier{ID: 9, Version: 1}}
	rec := newRec()
	AdminFundingFeeCreate(svc, false).ServeHTTP(rec,
		adminCtxB(httptest.NewRequest(http.MethodPost, "/x",
			strings.NewReader(`{"rail":"swift","currency":"usd","direction":"withdrawal","flat_fee":"5","percentage_bps":"10","min_fee":"0"}`))))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	if svc.gotCre.Rail != "swift" || svc.gotCre.FlatFee != "5" {
		t.Fatalf("req %+v", svc.gotCre)
	}
	// coded role rejection → 403 UNAUTHORIZED_ROLE
	svc.tier, svc.err = nil, excerrors.New("UNAUTHORIZED_ROLE", "finance ops only")
	rec = newRec()
	AdminFundingFeeCreate(svc, false).ServeHTTP(rec,
		adminCtxB(httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(`{}`))))
	if rec.Code != http.StatusForbidden || problemCode(t, rec) != "UNAUTHORIZED_ROLE" {
		t.Fatalf("role: %d %s", rec.Code, rec.Body.String())
	}
}

func TestAdminFundingFeeGet_BadID(t *testing.T) {
	rec := newRec()
	req := adminCtxB(httptest.NewRequest(http.MethodGet, "/x", nil))
	req.SetPathValue("id", "abc")
	AdminFundingFeeGet(&fakeFeeAdmin{}, false).ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad id: %d", rec.Code)
	}
}

func TestAdminFundingFeeUpdateAndRetire(t *testing.T) {
	svc := &fakeFeeAdmin{tier: &funding.FundingFeeTier{ID: 3, Version: 2}}
	rec := newRec()
	req := adminCtxB(httptest.NewRequest(http.MethodPut, "/x",
		strings.NewReader(`{"flat_fee":"6","percentage_bps":"0","min_fee":"0"}`)))
	req.SetPathValue("id", "3")
	AdminFundingFeeUpdate(svc, false).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || svc.gotID != 3 || svc.gotUpd.FlatFee != "6" {
		t.Fatalf("update: %d %+v", rec.Code, svc.gotUpd)
	}

	rec = newRec()
	req = adminCtxB(httptest.NewRequest(http.MethodDelete, "/x", nil))
	req.SetPathValue("id", "3")
	AdminFundingFeeRetire(svc, false).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || svc.gotID != 3 {
		t.Fatalf("retire: %d id=%d", rec.Code, svc.gotID)
	}
}

func TestAdminFundingFeeVersions(t *testing.T) {
	svc := &fakeFeeAdmin{rows: []funding.FundingFeeTier{
		{ID: 3, Version: 2}, {ID: 1, Version: 1}}}
	rec := newRec()
	req := adminCtxB(httptest.NewRequest(http.MethodGet, "/x", nil))
	req.SetPathValue("id", "1")
	AdminFundingFeeVersions(svc, false).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || svc.gotID != 1 {
		t.Fatalf("versions: %d %s", rec.Code, rec.Body.String())
	}
	m := decode(t, rec)
	if m["count"].(float64) != 2 {
		t.Fatalf("payload %+v", m)
	}
}

// --- Conversion (Task 11.3.9 item 4) ---------------------------------------

func TestFundingConvert(t *testing.T) {
	svc := &fakeConvQuoter{res: &funding.ConversionResult{
		FromCurrency: "EUR", ToCurrency: "USD",
		AmountFrom: decimal.NewFromInt(100),
		AmountTo:   decimal.NewFromInt(108),
		Converted:  true,
	}}
	h := FundingConvert(svc)

	// unauthenticated → 401
	rec := newRec()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/x", strings.NewReader("{}")))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauth: %d", rec.Code)
	}

	// bad amount → 400
	rec = newRec()
	h.ServeHTTP(rec, userCtx(httptest.NewRequest(http.MethodPost, "/x",
		strings.NewReader(`{"from_currency":"EUR","amount":"-1"}`)), 7))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("amount: %d", rec.Code)
	}

	// happy
	rec = newRec()
	h.ServeHTTP(rec, userCtx(httptest.NewRequest(http.MethodPost, "/x",
		strings.NewReader(`{"from_currency":"EUR","to_currency":"USD","amount":"100"}`)), 7))
	if rec.Code != http.StatusOK {
		t.Fatalf("happy: %d %s", rec.Code, rec.Body.String())
	}
	if svc.got.AccountID != 7 || svc.got.FromCurrency != "EUR" {
		t.Fatalf("req %+v", svc.got)
	}

	// absent rate source → 503 PRICE_ORACLE_UNAVAILABLE, never a rate
	svc.res, svc.err = nil, excerrors.New("PRICE_ORACLE_UNAVAILABLE", "no rate")
	rec = newRec()
	h.ServeHTTP(rec, userCtx(httptest.NewRequest(http.MethodPost, "/x",
		strings.NewReader(`{"from_currency":"EUR","amount":"100"}`)), 7))
	if rec.Code != http.StatusServiceUnavailable ||
		problemCode(t, rec) != "PRICE_ORACLE_UNAVAILABLE" {
		t.Fatalf("oracle: %d %s", rec.Code, rec.Body.String())
	}
}

func TestFundingConversions(t *testing.T) {
	svc := &fakeConvQuoter{rows: []funding.ConversionRecord{{ID: 1}, {ID: 2}}}
	rec := newRec()
	FundingConversions(svc).ServeHTTP(rec,
		userCtx(httptest.NewRequest(http.MethodGet, "/x?limit=1", nil), 7))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d %s", rec.Code, rec.Body.String())
	}
	m := decode(t, rec)
	if m["count"].(float64) != 1 || m["limit"].(float64) != 1 {
		t.Fatalf("payload %+v", m)
	}
}
