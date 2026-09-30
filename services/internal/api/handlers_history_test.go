// Handler tests for Phase-20 Tasks 20.3.2/20.3.3 — history reads off the
// ClickHouse projection. Sources are in-memory fakes; the live CH path is
// covered by internal/analytics' EXC_CH_TEST-gated tests.
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

	goredis "github.com/redis/go-redis/v9"

	"exchange/internal/analytics"
	"exchange/internal/auth"
	"exchange/internal/marketapi"
	"exchange/internal/marketdata"
	"exchange/pkg/decimal"
)

// ---------------------------------------------------------------------------
// Fakes
// ---------------------------------------------------------------------------

type fakeTickSource struct {
	ticks []analytics.Tick
	err   error
	got   analytics.TickQuery
	calls int
}

func (f *fakeTickSource) Query(_ context.Context, q analytics.TickQuery) ([]analytics.Tick, error) {
	f.calls++
	f.got = q
	return f.ticks, f.err
}

// fakeTradeSource satisfies HistoryTradeSource over canned rows.
type fakeTradeSource struct {
	rows  []*marketdata.HistoryTrade
	err   error
	got   marketdata.TradeHistoryQuery
	calls int
}

func (f *fakeTradeSource) Query(_ context.Context, q marketdata.TradeHistoryQuery) ([]*marketdata.HistoryTrade, error) {
	f.calls++
	f.got = q
	return f.rows, f.err
}

// fakeHistoryKV is an in-memory HistoryKV (best-effort cache seam).
type fakeHistoryKV struct {
	m   map[string]string
	ttl map[string]time.Duration
	err error
}

func newFakeHistoryKV() *fakeHistoryKV {
	return &fakeHistoryKV{m: map[string]string{}, ttl: map[string]time.Duration{}}
}

func (f *fakeHistoryKV) Get(_ context.Context, key string) *goredis.StringCmd {
	if f.err != nil {
		return goredis.NewStringResult("", f.err)
	}
	v, ok := f.m[key]
	if !ok {
		return goredis.NewStringResult("", goredis.Nil)
	}
	return goredis.NewStringResult(v, nil)
}

func (f *fakeHistoryKV) Set(_ context.Context, key string, value interface{}, exp time.Duration) *goredis.StatusCmd {
	if f.err != nil {
		return goredis.NewStatusResult("", f.err)
	}
	switch v := value.(type) {
	case []byte:
		f.m[key] = string(v)
	case string:
		f.m[key] = v
	default:
		f.m[key] = "<?>"
	}
	f.ttl[key] = exp
	return goredis.NewStatusResult("OK", nil)
}

// premiumTiers resolves every caller as Phase-23 premium — tests that
// predate the tiering layer pin it so the free-tier 30d/15min clamp does
// not rewrite their fixtures.
func premiumTiers() marketdata.HistoryTierResolver {
	return func(context.Context, *auth.Claims) (marketdata.HistoryAccess, error) {
		return marketdata.HistoryAccessPremium, nil
	}
}

// staffTiers resolves every caller as staff/compliance (exempt).
func staffTiers() marketdata.HistoryTierResolver {
	return func(context.Context, *auth.Claims) (marketdata.HistoryAccess, error) {
		return marketdata.HistoryAccessStaff, nil
	}
}

type fakeKlineSource struct {
	rows []analytics.CandleRow
	err  error
	got  analytics.OHLCVQuery
}

func (f *fakeKlineSource) Query(_ context.Context, q analytics.OHLCVQuery) ([]analytics.CandleRow, error) {
	f.got = q
	return f.rows, f.err
}

// historyResolver adapts the shared market fake to the InstrumentResolver
// seam (fakeMarketStore already implements InstrumentBySymbol).
var _ InstrumentResolver = (*fakeMarketStore)(nil)

var _ HistoryTickSource = (*fakeTickSource)(nil)
var _ HistoryKlineSource = (*fakeKlineSource)(nil)

func histReq(path, symbol string) *http.Request {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.SetPathValue("symbol", symbol)
	return req
}

func decodeList[T any](t *testing.T, rec *httptest.ResponseRecorder, want int) struct {
	Data       []T    `json:"data"`
	NextCursor string `json:"next_cursor"`
	Limit      int    `json:"limit"`
	Total      int64  `json:"total"`
} {
	t.Helper()
	if rec.Code != want {
		t.Fatalf("status = %d, want %d — body %s", rec.Code, want, rec.Body.String())
	}
	var env struct {
		Data       []T    `json:"data"`
		NextCursor string `json:"next_cursor"`
		Limit      int    `json:"limit"`
		Total      int64  `json:"total"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode envelope: %v — %s", err, rec.Body.String())
	}
	return env
}

func wantErrorStatus(t *testing.T, rec *httptest.ResponseRecorder, want int) {
	t.Helper()
	if rec.Code != want {
		t.Fatalf("status = %d, want %d — body %s", rec.Code, want, rec.Body.String())
	}
	var env ErrorEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode error envelope: %v", err)
	}
	if env.Type != "error" {
		t.Fatalf("envelope type = %q", env.Type)
	}
}

// ---------------------------------------------------------------------------
// Ticks
// ---------------------------------------------------------------------------

func TestHistoryTicks(t *testing.T) {
	ts := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	src := &fakeTickSource{ticks: []analytics.Tick{
		{ShardID: 2, Symbol: "EUR/USD", TradeID: 1002, EventSeq: 43,
			Price: decimal.MustFromString("1.08522"), Qty: decimal.MustFromString("50000"),
			Side: "SELL", Ts: ts},
		{ShardID: 2, Symbol: "EUR/USD", TradeID: 1001, EventSeq: 42,
			Price: decimal.MustFromString("1.08521"), Qty: decimal.MustFromString("100000"),
			Side: "BUY", Ts: ts.Add(-time.Second)},
	}}
	deps := &HistoryDeps{
		Ticks: src,
		Tiers: premiumTiers(),
		Instruments: &fakeMarketStore{
			instruments: []marketapi.Instrument{{Symbol: "EUR/USD"}},
		},
	}
	rec := httptest.NewRecorder()
	HistoryTicks(deps).ServeHTTP(rec, histReq(
		"/api/v1/history/ticks/EURUSD?from=2026-10-06T11:00:00Z&to=2026-10-06T13:00:00Z&limit=2",
		"EUR/USD"))
	env := decodeList[historyTickDoc](t, rec, http.StatusOK)
	if len(env.Data) != 2 {
		t.Fatalf("data len = %d", len(env.Data))
	}
	if env.Data[0].TradeID != 1002 || env.Data[0].Price != "1.08522000" ||
		env.Data[0].Side != "SELL" || env.Data[0].TimeMs != ts.UnixMilli() {
		t.Fatalf("doc[0] = %+v", env.Data[0])
	}
	// A full page (len == limit) yields a next_cursor.
	if env.NextCursor == "" {
		t.Fatal("full page must emit next_cursor")
	}
	if env.Limit != 2 || env.Total != 2 {
		t.Fatalf("env limit/total = %d/%d", env.Limit, env.Total)
	}
	// Query args forwarded correctly.
	if src.got.Symbol != "EUR/USD" || src.got.Limit != 2 ||
		src.got.From.IsZero() || src.got.To.IsZero() {
		t.Fatalf("query = %+v", src.got)
	}
}

func TestHistoryTicksCursor(t *testing.T) {
	src := &fakeTickSource{}
	deps := &HistoryDeps{
		Ticks:       src,
		Tiers:       premiumTiers(),
		Instruments: &fakeMarketStore{instruments: []marketapi.Instrument{{Symbol: "EUR/USD"}}},
	}
	pos := Cursor{CreatedAt: time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC), ID: 1002}
	rec := httptest.NewRecorder()
	HistoryTicks(deps).ServeHTTP(rec, histReq(
		"/api/v1/history/ticks/EURUSD?cursor="+EncodeCursor(pos), "EUR/USD"))
	decodeList[historyTickDoc](t, rec, http.StatusOK)
	if src.got.After == nil || src.got.After.TradeID != 1002 ||
		!src.got.After.Ts.Equal(pos.CreatedAt) {
		t.Fatalf("cursor not forwarded: %+v", src.got.After)
	}
}

func TestHistoryTicksBadParams(t *testing.T) {
	deps := &HistoryDeps{
		Ticks:       &fakeTickSource{},
		Instruments: &fakeMarketStore{instruments: []marketapi.Instrument{{Symbol: "EUR/USD"}}},
	}
	for _, path := range []string{
		"/api/v1/history/ticks/EURUSD?from=garbage",
		"/api/v1/history/ticks/EURUSD?to=yesterday",
		"/api/v1/history/ticks/EURUSD?limit=20000", // over the Phase-23 10000 cap
		"/api/v1/history/ticks/EURUSD?limit=0",
		"/api/v1/history/ticks/EURUSD?cursor=!!!notb64!!!",
	} {
		rec := httptest.NewRecorder()
		HistoryTicks(deps).ServeHTTP(rec, histReq(path, "EUR/USD"))
		wantErrorStatus(t, rec, http.StatusBadRequest)
	}
	// Missing path symbol.
	rec := httptest.NewRecorder()
	HistoryTicks(deps).ServeHTTP(rec, histReq("/api/v1/history/ticks/", ""))
	wantErrorStatus(t, rec, http.StatusBadRequest)
}

func TestHistoryTicksUnknownSymbol(t *testing.T) {
	deps := &HistoryDeps{
		Ticks:       &fakeTickSource{},
		Instruments: &fakeMarketStore{}, // knows nothing
	}
	rec := httptest.NewRecorder()
	HistoryTicks(deps).ServeHTTP(rec, histReq("/api/v1/history/ticks/XXX", "XXX/YYY"))
	wantErrorStatus(t, rec, http.StatusNotFound)
}

func TestHistoryTicksStoreError(t *testing.T) {
	deps := &HistoryDeps{
		Ticks:       &fakeTickSource{err: errors.New("ch down")},
		Instruments: &fakeMarketStore{instruments: []marketapi.Instrument{{Symbol: "EUR/USD"}}},
	}
	rec := httptest.NewRecorder()
	HistoryTicks(deps).ServeHTTP(rec, histReq("/api/v1/history/ticks/EURUSD", "EUR/USD"))
	wantErrorStatus(t, rec, http.StatusServiceUnavailable)
}

func TestHistoryTicksNotConfigured(t *testing.T) {
	rec := httptest.NewRecorder()
	HistoryTicks(&HistoryDeps{}).ServeHTTP(rec, histReq("/api/v1/history/ticks/EURUSD", "EUR/USD"))
	wantErrorStatus(t, rec, http.StatusServiceUnavailable)
}

// ---------------------------------------------------------------------------
// Klines
// ---------------------------------------------------------------------------

func TestHistoryKlines(t *testing.T) {
	ot := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	one := decimal.MustFromString("1.0852")
	src := &fakeKlineSource{rows: []analytics.CandleRow{
		{Symbol: "EUR/USD", Interval: "4h", OpenTime: ot,
			Open: one, High: one, Low: one, Close: one,
			Volume: decimal.MustFromString("100"), QuoteVolume: decimal.MustFromString("108.52"),
			TradeCount: 3},
	}}
	deps := &HistoryDeps{
		Klines:      src,
		Instruments: &fakeMarketStore{instruments: []marketapi.Instrument{{Symbol: "EUR/USD"}}},
	}
	rec := httptest.NewRecorder()
	HistoryKlines(deps).ServeHTTP(rec, histReq(
		"/api/v1/history/klines/EURUSD?interval=4h&from=2026-10-01T00:00:00Z&to=2026-10-07T00:00:00Z",
		"EUR/USD"))
	env := decodeList[historyKlineDoc](t, rec, http.StatusOK)
	if len(env.Data) != 1 || !env.Data[0].Closed || env.Data[0].Open != "1.08520000" ||
		env.Data[0].OpenTimeMs != ot.UnixMilli() || env.Data[0].TradeCount != 3 {
		t.Fatalf("doc = %+v", env.Data)
	}
	if src.got.Interval != "4h" || src.got.Symbol != "EUR/USD" {
		t.Fatalf("query = %+v", src.got)
	}
	// A short page (len < limit) emits no next_cursor.
	if env.NextCursor != "" {
		t.Fatalf("terminal page must not emit next_cursor, got %q", env.NextCursor)
	}
}

func TestHistoryKlinesDefaultInterval(t *testing.T) {
	src := &fakeKlineSource{}
	deps := &HistoryDeps{
		Klines:      src,
		Instruments: &fakeMarketStore{instruments: []marketapi.Instrument{{Symbol: "EUR/USD"}}},
	}
	rec := httptest.NewRecorder()
	HistoryKlines(deps).ServeHTTP(rec, histReq("/api/v1/history/klines/EURUSD", "EUR/USD"))
	decodeList[historyKlineDoc](t, rec, http.StatusOK)
	if src.got.Interval != "1m" || src.got.Limit != 500 {
		t.Fatalf("defaults not applied: %+v", src.got)
	}
}

func TestHistoryKlinesRejectsBadIntervals(t *testing.T) {
	deps := &HistoryDeps{
		Klines:      &fakeKlineSource{},
		Instruments: &fakeMarketStore{instruments: []marketapi.Instrument{{Symbol: "EUR/USD"}}},
	}
	for _, iv := range []string{"1s", "2m", "1Y", "1M%20"} {
		rec := httptest.NewRecorder()
		HistoryKlines(deps).ServeHTTP(rec, histReq(
			"/api/v1/history/klines/EURUSD?interval="+iv, "EUR/USD"))
		wantErrorStatus(t, rec, http.StatusBadRequest)
	}
	// Every persisted interval is accepted.
	for _, iv := range analytics.PersistedIntervalLabels() {
		rec := httptest.NewRecorder()
		HistoryKlines(deps).ServeHTTP(rec, histReq(
			"/api/v1/history/klines/EURUSD?interval="+iv, "EUR/USD"))
		decodeList[historyKlineDoc](t, rec, http.StatusOK)
	}
}

func TestHistoryKlinesUnknownSymbolAndStoreError(t *testing.T) {
	deps := &HistoryDeps{
		Klines:      &fakeKlineSource{},
		Instruments: &fakeMarketStore{},
	}
	rec := httptest.NewRecorder()
	HistoryKlines(deps).ServeHTTP(rec, histReq("/api/v1/history/klines/XXX", "XXX/YYY"))
	wantErrorStatus(t, rec, http.StatusNotFound)

	deps.Instruments = &fakeMarketStore{instruments: []marketapi.Instrument{{Symbol: "EUR/USD"}}}
	deps.Klines = &fakeKlineSource{err: errors.New("ch down")}
	rec = httptest.NewRecorder()
	HistoryKlines(deps).ServeHTTP(rec, histReq("/api/v1/history/klines/EURUSD", "EUR/USD"))
	wantErrorStatus(t, rec, http.StatusServiceUnavailable)
}

func TestHistoryKlinesCursor(t *testing.T) {
	src := &fakeKlineSource{}
	deps := &HistoryDeps{
		Klines:      src,
		Instruments: &fakeMarketStore{instruments: []marketapi.Instrument{{Symbol: "EUR/USD"}}},
	}
	pos := Cursor{CreatedAt: time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)}
	rec := httptest.NewRecorder()
	HistoryKlines(deps).ServeHTTP(rec, histReq(
		"/api/v1/history/klines/EURUSD?interval=1D&cursor="+EncodeCursor(pos), "EUR/USD"))
	decodeList[historyKlineDoc](t, rec, http.StatusOK)
	if src.got.After == nil || !src.got.After.Equal(pos.CreatedAt) {
		t.Fatalf("cursor not forwarded: %+v", src.got.After)
	}
}

// ---------------------------------------------------------------------------
// Phase-23 Task 23.3.4 — access tiers + content negotiation (ticks)
// ---------------------------------------------------------------------------

// histEnv mirrors the extended history envelope for assertions.
type histEnv struct {
	Data       []historyTickDoc `json:"data"`
	NextCursor string           `json:"next_cursor"`
	Limit      int              `json:"limit"`
	Total      int64            `json:"total"`
	AccessTier string           `json:"access_tier"`
	Delayed    bool             `json:"delayed"`
	Degraded   bool             `json:"degraded"`
}

func decodeHistEnv(t *testing.T, rec *httptest.ResponseRecorder, want int) histEnv {
	t.Helper()
	if rec.Code != want {
		t.Fatalf("status = %d, want %d — body %s", rec.Code, want, rec.Body.String())
	}
	var env histEnv
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode envelope: %v — %s", err, rec.Body.String())
	}
	return env
}

func TestHistoryTicksFreeTierClamp(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	src := &fakeTickSource{}
	deps := &HistoryDeps{
		Ticks: src,
		Guard: marketdata.HistoryQueryGuard{Now: func() time.Time { return now }},
		// nil Tiers → free (safe default)
		Instruments: &fakeMarketStore{instruments: []marketapi.Instrument{{Symbol: "EUR/USD"}}},
	}
	rec := httptest.NewRecorder()
	HistoryTicks(deps).ServeHTTP(rec, histReq(
		"/api/v1/history/ticks/EURUSD?from=2020-01-01T00:00:00Z&to=2026-10-06T12:00:00Z",
		"EUR/USD"))
	env := decodeHistEnv(t, rec, http.StatusOK)
	if env.AccessTier != "free" || !env.Delayed || env.Degraded {
		t.Fatalf("env tier flags = %+v", env)
	}
	wantFrom := now.AddDate(0, 0, -30)
	wantTo := now.Add(-15 * time.Minute)
	if !src.got.From.Equal(wantFrom) || !src.got.To.Equal(wantTo) {
		t.Fatalf("clamped bounds = [%s, %s), want [%s, %s)",
			src.got.From, src.got.To, wantFrom, wantTo)
	}
	// Default limit is the Phase-23 contract now.
	if src.got.Limit != 1000 {
		t.Fatalf("default limit = %d, want 1000", src.got.Limit)
	}
}

func TestHistoryTicksPremiumTierPassthrough(t *testing.T) {
	src := &fakeTickSource{}
	deps := &HistoryDeps{
		Ticks:       src,
		Tiers:       premiumTiers(),
		Instruments: &fakeMarketStore{instruments: []marketapi.Instrument{{Symbol: "EUR/USD"}}},
	}
	rec := httptest.NewRecorder()
	HistoryTicks(deps).ServeHTTP(rec, histReq(
		"/api/v1/history/ticks/EURUSD?from=2020-01-01T00:00:00Z&to=2020-06-01T00:00:00Z",
		"EUR/USD"))
	env := decodeHistEnv(t, rec, http.StatusOK)
	if env.AccessTier != "premium" || env.Delayed {
		t.Fatalf("env = %+v", env)
	}
	wantFrom := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	wantTo := time.Date(2020, 6, 1, 0, 0, 0, 0, time.UTC)
	if !src.got.From.Equal(wantFrom) || !src.got.To.Equal(wantTo) {
		t.Fatalf("premium bounds clamped: [%s, %s)", src.got.From, src.got.To)
	}
}

func TestHistoryTicksResolverErrorDegrades(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	src := &fakeTickSource{}
	deps := &HistoryDeps{
		Ticks: src,
		Tiers: func(context.Context, *auth.Claims) (marketdata.HistoryAccess, error) {
			return "", errors.New("tier store down")
		},
		Guard:       marketdata.HistoryQueryGuard{Now: func() time.Time { return now }},
		Instruments: &fakeMarketStore{instruments: []marketapi.Instrument{{Symbol: "EUR/USD"}}},
	}
	rec := httptest.NewRecorder()
	HistoryTicks(deps).ServeHTTP(rec, histReq(
		"/api/v1/history/ticks/EURUSD?from=2020-01-01T00:00:00Z", "EUR/USD"))
	env := decodeHistEnv(t, rec, http.StatusOK)
	if env.AccessTier != "free" || !env.Degraded {
		t.Fatalf("resolver error must degrade to free+degraded: %+v", env)
	}
	if !src.got.From.Equal(now.AddDate(0, 0, -30)) {
		t.Fatalf("degraded free window not clamped: %s", src.got.From)
	}
}

func TestHistoryTicksEmptyWindowSkipsStore(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	src := &fakeTickSource{}
	deps := &HistoryDeps{
		Ticks:       src,
		Guard:       marketdata.HistoryQueryGuard{Now: func() time.Time { return now }},
		Instruments: &fakeMarketStore{instruments: []marketapi.Instrument{{Symbol: "EUR/USD"}}},
	}
	rec := httptest.NewRecorder()
	// Free tier: from inside the delay horizon clamps past the ceiling —
	// a legitimate empty page, not a store round-trip.
	HistoryTicks(deps).ServeHTTP(rec, histReq(
		"/api/v1/history/ticks/EURUSD?from=2026-10-06T11:55:00Z", "EUR/USD"))
	decodeHistEnv(t, rec, http.StatusOK)
	if src.calls != 0 {
		t.Fatalf("empty window must not hit the store, calls = %d", src.calls)
	}
}

func TestHistoryTicksTimeout(t *testing.T) {
	deps := &HistoryDeps{
		Ticks:       &fakeTickSource{err: context.DeadlineExceeded},
		Tiers:       premiumTiers(),
		Instruments: &fakeMarketStore{instruments: []marketapi.Instrument{{Symbol: "EUR/USD"}}},
	}
	rec := httptest.NewRecorder()
	HistoryTicks(deps).ServeHTTP(rec, histReq("/api/v1/history/ticks/EURUSD", "EUR/USD"))
	if rec.Code != http.StatusGatewayTimeout {
		t.Fatalf("status = %d, want 504 — body %s", rec.Code, rec.Body.String())
	}
	var env ErrorEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode error envelope: %v", err)
	}
	if env.Error != "HISTORICAL_QUERY_TIMEOUT" {
		t.Fatalf("code = %q, want HISTORICAL_QUERY_TIMEOUT", env.Error)
	}
}

func TestHistoryTicksCSVFormat(t *testing.T) {
	ts := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	src := &fakeTickSource{ticks: []analytics.Tick{
		{ShardID: 2, Symbol: "EUR/USD", TradeID: 1002, EventSeq: 43,
			Price: decimal.MustFromString("1.08522"), Qty: decimal.MustFromString("50000"),
			Side: "SELL", Ts: ts},
	}}
	deps := &HistoryDeps{
		Ticks:       src,
		Tiers:       premiumTiers(),
		Instruments: &fakeMarketStore{instruments: []marketapi.Instrument{{Symbol: "EUR/USD"}}},
	}
	req := histReq("/api/v1/history/ticks/EURUSD", "EUR/USD")
	req.Header.Set("Accept", "text/csv")
	rec := httptest.NewRecorder()
	HistoryTicks(deps).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d — %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/csv" {
		t.Fatalf("content-type = %q", ct)
	}
	body := rec.Body.String()
	if !strings.HasPrefix(body, "trade_id,event_seq,shard_id,time,price,quantity,side\n") {
		t.Fatalf("csv header missing: %q", body)
	}
	if !strings.Contains(body, "1002,43,2,2026-10-06T12:00:00.000Z,1.08522000,50000.00000000,SELL") {
		t.Fatalf("csv row missing: %q", body)
	}
}

func TestHistoryTicksFIXFormat(t *testing.T) {
	ts := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	src := &fakeTickSource{ticks: []analytics.Tick{
		{ShardID: 2, Symbol: "EUR/USD", TradeID: 1002, EventSeq: 43,
			Price: decimal.MustFromString("1.08522"), Qty: decimal.MustFromString("50000"),
			Side: "SELL", Ts: ts},
	}}
	deps := &HistoryDeps{
		Ticks:       src,
		Tiers:       premiumTiers(),
		Instruments: &fakeMarketStore{instruments: []marketapi.Instrument{{Symbol: "EUR/USD"}}},
	}
	req := histReq("/api/v1/history/ticks/EURUSD", "EUR/USD")
	req.Header.Set("Accept", "application/x-fix")
	rec := httptest.NewRecorder()
	HistoryTicks(deps).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d — %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/x-fix" {
		t.Fatalf("content-type = %q", ct)
	}
	body := rec.Body.String()
	for _, tag := range []string{"11=1002", "17=43", "55=EUR/USD",
		"31=1.08522000", "32=50000.00000000", "54=2", "60=20261006-12:00:00.000"} {
		if !strings.Contains(body, tag) {
			t.Fatalf("fix body missing %q: %q", tag, body)
		}
	}
}

// ---------------------------------------------------------------------------
// Phase-23 Task 23.3.8 — closed-interval response cache (ticks)
// ---------------------------------------------------------------------------

func TestHistoryTicksCacheClosedInterval(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	kv := newFakeHistoryKV()
	src := &fakeTickSource{ticks: []analytics.Tick{
		{ShardID: 1, Symbol: "EUR/USD", TradeID: 7, EventSeq: 3,
			Price: decimal.MustFromString("1.0852"), Qty: decimal.MustFromString("10"),
			Side: "BUY", Ts: now.Add(-2 * time.Hour)},
	}}
	deps := &HistoryDeps{
		Ticks:       src,
		Tiers:       premiumTiers(),
		Guard:       marketdata.HistoryQueryGuard{Now: func() time.Time { return now }},
		Cache:       kv,
		Instruments: &fakeMarketStore{instruments: []marketapi.Instrument{{Symbol: "EUR/USD"}}},
	}
	path := "/api/v1/history/ticks/EURUSD?to=2026-10-06T10:00:00Z" // closed (to < now)
	HistoryTicks(deps).ServeHTTP(httptest.NewRecorder(), histReq(path, "EUR/USD"))
	if src.calls != 1 {
		t.Fatalf("first call hits store once, calls = %d", src.calls)
	}
	if len(kv.m) != 1 {
		t.Fatalf("closed-interval response must be cached, keys = %v", kv.m)
	}
	for _, ttl := range kv.ttl {
		if ttl != 60*time.Second {
			t.Fatalf("cache TTL = %s, want 60s", ttl)
		}
	}
	rec := httptest.NewRecorder()
	HistoryTicks(deps).ServeHTTP(rec, histReq(path, "EUR/USD"))
	if src.calls != 1 {
		t.Fatalf("second call must be a cache hit, calls = %d", src.calls)
	}
	if rec.Header().Get("X-History-Cache") != "hit" {
		t.Fatal("cache hit marker missing")
	}
	decodeHistEnv(t, rec, http.StatusOK)
}

func TestHistoryTicksCacheOpenIntervalBypassed(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	kv := newFakeHistoryKV()
	src := &fakeTickSource{}
	deps := &HistoryDeps{
		Ticks:       src,
		Tiers:       premiumTiers(),
		Guard:       marketdata.HistoryQueryGuard{Now: func() time.Time { return now }},
		Cache:       kv,
		Instruments: &fakeMarketStore{instruments: []marketapi.Instrument{{Symbol: "EUR/USD"}}},
	}
	// No `to` bound — the tail is still moving; never cached.
	path := "/api/v1/history/ticks/EURUSD"
	for i := 0; i < 2; i++ {
		HistoryTicks(deps).ServeHTTP(httptest.NewRecorder(), histReq(path, "EUR/USD"))
	}
	if src.calls != 2 {
		t.Fatalf("open interval must query every time, calls = %d", src.calls)
	}
	if len(kv.m) != 0 {
		t.Fatalf("open interval must never be cached, keys = %v", kv.m)
	}
}

func TestHistoryTicksCacheOutageDoesNotBlock(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	kv := &fakeHistoryKV{m: map[string]string{}, ttl: map[string]time.Duration{},
		err: errors.New("redis down")}
	src := &fakeTickSource{}
	deps := &HistoryDeps{
		Ticks:       src,
		Tiers:       premiumTiers(),
		Guard:       marketdata.HistoryQueryGuard{Now: func() time.Time { return now }},
		Cache:       kv,
		Instruments: &fakeMarketStore{instruments: []marketapi.Instrument{{Symbol: "EUR/USD"}}},
	}
	rec := httptest.NewRecorder()
	HistoryTicks(deps).ServeHTTP(rec, histReq(
		"/api/v1/history/ticks/EURUSD?to=2026-10-06T10:00:00Z", "EUR/USD"))
	decodeHistEnv(t, rec, http.StatusOK) // Redis down → query still runs
	if src.calls != 1 {
		t.Fatalf("cache outage must not block the query, calls = %d", src.calls)
	}
}

// ---------------------------------------------------------------------------
// Phase-23 Task 23.3.1/23.3.8 — trades endpoint + participant masking
// ---------------------------------------------------------------------------

func histTrade(ts time.Time, id uint64) *marketdata.HistoryTrade {
	return &marketdata.HistoryTrade{
		ShardID: 1, Symbol: "EUR/USD", TradeID: id, InstrumentID: 5,
		EventSeq: id * 10, Price: decimal.MustFromString("1.0852"),
		Qty: decimal.MustFromString("1000"), AggressorSide: "BUY", Ts: ts,
		Participants: marketdata.ParticipantFields{
			MakerAccountID: 101, TakerAccountID: 202,
			BuyOrderID: 9001, SellOrderID: 9002,
		},
	}
}

func TestHistoryTrades(t *testing.T) {
	ts := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	src := &fakeTradeSource{rows: []*marketdata.HistoryTrade{histTrade(ts, 42)}}
	deps := &HistoryDeps{
		Trades:      src,
		Tiers:       premiumTiers(),
		Instruments: &fakeMarketStore{instruments: []marketapi.Instrument{{Symbol: "EUR/USD"}}},
	}
	rec := httptest.NewRecorder()
	HistoryTrades(deps).ServeHTTP(rec, histReq(
		"/api/v1/history/trades/EURUSD?from=2026-09-01T00:00:00Z&to=2026-09-02T00:00:00Z",
		"EUR/USD"))
	var env struct {
		Data []struct {
			TradeID        uint64 `json:"trade_id"`
			InstrumentID   int64  `json:"instrument_id"`
			Price          string `json:"price"`
			Side           string `json:"side"`
			MakerAccountID string `json:"maker_account_id"`
			TakerAccountID string `json:"taker_account_id"`
			BuyOrderID     string `json:"buy_order_id"`
			SellOrderID    string `json:"sell_order_id"`
			Time           string `json:"time"`
		} `json:"data"`
		AccessTier string `json:"access_tier"`
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d — %s", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(env.Data) != 1 || env.Data[0].TradeID != 42 ||
		env.Data[0].Price != "1.08520000" || env.Data[0].Side != "BUY" {
		t.Fatalf("doc = %+v", env.Data)
	}
	// Public tape: participant identity is unconditionally masked.
	if env.Data[0].MakerAccountID != "" || env.Data[0].TakerAccountID != "" ||
		env.Data[0].BuyOrderID != "" || env.Data[0].SellOrderID != "" {
		t.Fatalf("public tape leaked participant ids: %+v", env.Data[0])
	}
	if env.AccessTier != "premium" {
		t.Fatalf("access_tier = %q", env.AccessTier)
	}
	if src.got.Symbol != "EUR/USD" || src.got.Limit != 1000 {
		t.Fatalf("query = %+v", src.got)
	}
}

func TestHistoryTradesStaffSeesParticipants(t *testing.T) {
	ts := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	src := &fakeTradeSource{rows: []*marketdata.HistoryTrade{histTrade(ts, 42)}}
	deps := &HistoryDeps{
		Trades:      src,
		Tiers:       staffTiers(),
		Instruments: &fakeMarketStore{instruments: []marketapi.Instrument{{Symbol: "EUR/USD"}}},
	}
	rec := httptest.NewRecorder()
	HistoryTrades(deps).ServeHTTP(rec, histReq("/api/v1/history/trades/EURUSD", "EUR/USD"))
	var env struct {
		Data []struct {
			MakerAccountID string `json:"maker_account_id"`
			BuyOrderID     string `json:"buy_order_id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v — %s", err, rec.Body.String())
	}
	if len(env.Data) != 1 || env.Data[0].MakerAccountID != "101" || env.Data[0].BuyOrderID != "9001" {
		t.Fatalf("staff must see participant ids: %+v", env.Data)
	}
}

func TestHistoryTradesStaffPreOpenMask(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	pre := histTrade(now.Add(-5*time.Minute), 1) // inside [open-15m, open)
	old := histTrade(now.Add(-48*time.Hour), 2)  // earlier session — untouched
	src := &fakeTradeSource{rows: []*marketdata.HistoryTrade{pre, old}}
	deps := &HistoryDeps{
		Trades:      src,
		Tiers:       staffTiers(),
		Instruments: &fakeMarketStore{instruments: []marketapi.Instrument{{Symbol: "EUR/USD"}}},
		SessionOpen: func(string) (time.Time, error) { return now, nil },
	}
	rec := httptest.NewRecorder()
	HistoryTrades(deps).ServeHTTP(rec, histReq("/api/v1/history/trades/EURUSD", "EUR/USD"))
	var env struct {
		Data []struct {
			TradeID        uint64 `json:"trade_id"`
			MakerAccountID string `json:"maker_account_id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v — %s", err, rec.Body.String())
	}
	if len(env.Data) != 2 {
		t.Fatalf("data = %+v", env.Data)
	}
	byID := map[uint64]string{}
	for _, d := range env.Data {
		byID[d.TradeID] = d.MakerAccountID
	}
	if byID[1] != "" {
		t.Fatalf("pre-open row must be masked even for staff: %+v", env.Data)
	}
	if byID[2] != "101" {
		t.Fatalf("in-session row must keep identity for staff: %+v", env.Data)
	}
}

func TestHistoryTradesSessionOpenErrorDegrades(t *testing.T) {
	src := &fakeTradeSource{rows: []*marketdata.HistoryTrade{
		histTrade(time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC), 1)}}
	deps := &HistoryDeps{
		Trades:      src,
		Tiers:       staffTiers(),
		Instruments: &fakeMarketStore{instruments: []marketapi.Instrument{{Symbol: "EUR/USD"}}},
		SessionOpen: func(string) (time.Time, error) {
			return time.Time{}, errors.New("session store down")
		},
	}
	rec := httptest.NewRecorder()
	HistoryTrades(deps).ServeHTTP(rec, histReq("/api/v1/history/trades/EURUSD", "EUR/USD"))
	wantErrorStatus(t, rec, http.StatusServiceUnavailable)
}

func TestHistoryTradesCursorAndErrors(t *testing.T) {
	src := &fakeTradeSource{}
	deps := &HistoryDeps{
		Trades:      src,
		Tiers:       premiumTiers(),
		Instruments: &fakeMarketStore{instruments: []marketapi.Instrument{{Symbol: "EUR/USD"}}},
	}
	pos := Cursor{CreatedAt: time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC), ID: 42}
	rec := httptest.NewRecorder()
	HistoryTrades(deps).ServeHTTP(rec, histReq(
		"/api/v1/history/trades/EURUSD?cursor="+EncodeCursor(pos), "EUR/USD"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d — %s", rec.Code, rec.Body.String())
	}
	if src.got.After == nil || src.got.After.TradeID != 42 ||
		!src.got.After.Ts.Equal(pos.CreatedAt) {
		t.Fatalf("cursor not forwarded: %+v", src.got.After)
	}

	// Unknown symbol → 404.
	deps.Instruments = &fakeMarketStore{}
	rec = httptest.NewRecorder()
	HistoryTrades(deps).ServeHTTP(rec, histReq("/api/v1/history/trades/XXX", "XXX/YYY"))
	wantErrorStatus(t, rec, http.StatusNotFound)

	// Store outage → fail-closed 503.
	deps.Instruments = &fakeMarketStore{instruments: []marketapi.Instrument{{Symbol: "EUR/USD"}}}
	deps.Trades = &fakeTradeSource{err: errors.New("ch down")}
	rec = httptest.NewRecorder()
	HistoryTrades(deps).ServeHTTP(rec, histReq("/api/v1/history/trades/EURUSD", "EUR/USD"))
	wantErrorStatus(t, rec, http.StatusServiceUnavailable)

	// Query timeout → 504 HISTORICAL_QUERY_TIMEOUT.
	deps.Trades = &fakeTradeSource{err: context.DeadlineExceeded}
	rec = httptest.NewRecorder()
	HistoryTrades(deps).ServeHTTP(rec, histReq("/api/v1/history/trades/EURUSD", "EUR/USD"))
	if rec.Code != http.StatusGatewayTimeout {
		t.Fatalf("status = %d, want 504 — %s", rec.Code, rec.Body.String())
	}
	var env ErrorEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if env.Error != "HISTORICAL_QUERY_TIMEOUT" {
		t.Fatalf("code = %q", env.Error)
	}

	// Not configured → 503.
	rec = httptest.NewRecorder()
	HistoryTrades(&HistoryDeps{}).ServeHTTP(rec, histReq("/api/v1/history/trades/EURUSD", "EUR/USD"))
	wantErrorStatus(t, rec, http.StatusServiceUnavailable)
}
