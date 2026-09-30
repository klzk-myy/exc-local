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
	"testing"
	"time"

	"exchange/internal/analytics"
	"exchange/internal/marketapi"
	"exchange/pkg/decimal"
)

// ---------------------------------------------------------------------------
// Fakes
// ---------------------------------------------------------------------------

type fakeTickSource struct {
	ticks []analytics.Tick
	err   error
	got   analytics.TickQuery
}

func (f *fakeTickSource) Query(_ context.Context, q analytics.TickQuery) ([]analytics.Tick, error) {
	f.got = q
	return f.ticks, f.err
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
		"/api/v1/history/ticks/EURUSD?limit=2000", // over the 1000 cap
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
