// Phase-23 Task 23.3.9 — swap-rate history handler tests. The source
// seam is faked; the reconciliation fixture seeds a fake journal
// through marketdata.ProjectSwapRateDay so the API row is asserted
// against the journal day-for-day (§24 #358).
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

	"exchange/internal/marketapi"
	"exchange/internal/marketdata"
	"exchange/pkg/decimal"
)

// fakeSwapSource satisfies marketdata.SwapRateHistorySource.
type fakeSwapSource struct {
	rows  []marketdata.SwapRateDay
	err   error
	got   marketdata.SwapRateHistoryQuery
	calls int
}

func (f *fakeSwapSource) Query(_ context.Context, q marketdata.SwapRateHistoryQuery) ([]marketdata.SwapRateDay, error) {
	f.calls++
	f.got = q
	return f.rows, f.err
}

var _ marketdata.SwapRateHistorySource = (*fakeSwapSource)(nil)

func swapDeps(src *fakeSwapSource) *SwapRateHistoryDeps {
	return &SwapRateHistoryDeps{
		History:     src,
		Instruments: &fakeMarketStore{instruments: []marketapi.Instrument{{Symbol: "EUR/USD"}}},
		Tiers:       premiumTiers(),
	}
}

// swapJournalFixture seeds a fake journal: Wednesday days=3 accruals
// (LONG+SHORT) reconciled onto the published sheet — the DoD fixture.
func swapJournalFixture() []marketdata.SwapRateDay {
	wed := time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC) // Wednesday
	rate := marketdata.SwapRateRow{
		InstrumentID: 7, Symbol: "EUR/USD", EffectiveDate: wed,
		LongPoints:  decimal.MustFromString("1.254"),
		ShortPoints: decimal.MustFromString("-2.318"),
		Source:      "REFINITIV", IngestedAt: wed.Add(-6 * time.Hour),
	}
	journal := []marketdata.SwapJournalRow{
		{Side: "LONG", Days: 3, MarkupBps: decimal.MustFromString("50.25"), Rows: 11},
		{Side: "SHORT", Days: 3, MarkupBps: decimal.MustFromString("50.25"), Rows: 9},
	}
	return []marketdata.SwapRateDay{marketdata.ProjectSwapRateDay(rate, journal)}
}

func TestHistorySwapRatesTripleWednesday(t *testing.T) {
	src := &fakeSwapSource{rows: swapJournalFixture()}
	rec := httptest.NewRecorder()
	HistorySwapRates(swapDeps(src)).ServeHTTP(rec,
		httptest.NewRequest(http.MethodGet,
			"/api/v1/history/swap-rates?symbol=EUR%2FUSD", nil))
	var env struct {
		Data []struct {
			Symbol         string `json:"symbol"`
			EffectiveDate  string `json:"effective_date"`
			LongPoints     string `json:"long_points"`
			ShortPoints    string `json:"short_points"`
			LongMarkupBps  string `json:"long_markup_bps"`
			ShortMarkupBps string `json:"short_markup_bps"`
			DaysApplied    int    `json:"days_applied"`
			Triple         bool   `json:"triple"`
			AccrualCount   int64  `json:"accrual_count"`
			Source         string `json:"source"`
		} `json:"data"`
	}
	if rec.Code != 200 {
		t.Fatalf("status = %d — %s", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v — %s", err, rec.Body.String())
	}
	if len(env.Data) != 1 {
		t.Fatalf("rows = %d", len(env.Data))
	}
	d := env.Data[0]
	// Exact day-for-day reconciliation against the seeded journal.
	if d.EffectiveDate != "2026-10-07" || !d.Triple || d.DaysApplied != 3 {
		t.Fatalf("triple row = %+v", d)
	}
	if d.LongPoints != "1.25400000" || d.ShortPoints != "-2.31800000" ||
		d.LongMarkupBps != "50.25" || d.ShortMarkupBps != "50.25" ||
		d.AccrualCount != 20 || d.Source != "REFINITIV" {
		t.Fatalf("reconciliation = %+v", d)
	}
	if src.got.Symbol != "EUR/USD" {
		t.Fatalf("symbol filter = %q", src.got.Symbol)
	}
}

func TestHistorySwapRatesFreeTierDelay(t *testing.T) {
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	src := &fakeSwapSource{}
	d := swapDeps(src)
	d.Tiers = nil // fail-closed free
	d.Guard.Now = func() time.Time { return now }
	rec := httptest.NewRecorder()
	HistorySwapRates(d).ServeHTTP(rec,
		httptest.NewRequest(http.MethodGet,
			"/api/v1/history/swap-rates?symbol=EUR%2FUSD", nil))
	if rec.Code != 200 {
		t.Fatalf("status = %d — %s", rec.Code, rec.Body.String())
	}
	wantPub := now.Add(-15 * time.Minute)
	if !src.got.PublishedBefore.Equal(wantPub) {
		t.Fatalf("PublishedBefore = %v, want %v", src.got.PublishedBefore, wantPub)
	}
	if !src.got.From.Equal(now.AddDate(0, 0, -30)) || !src.got.To.Equal(wantPub) {
		t.Fatalf("free window = %v → %v", src.got.From, src.got.To)
	}
}

func TestHistorySwapRatesPremiumRealtime(t *testing.T) {
	src := &fakeSwapSource{}
	d := swapDeps(src)
	d.Guard.Now = func() time.Time { return time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC) }
	rec := httptest.NewRecorder()
	HistorySwapRates(d).ServeHTTP(rec,
		httptest.NewRequest(http.MethodGet, "/api/v1/history/swap-rates", nil))
	if rec.Code != 200 {
		t.Fatalf("status = %d — %s", rec.Code, rec.Body.String())
	}
	if !src.got.PublishedBefore.IsZero() || !src.got.To.IsZero() {
		t.Fatalf("premium must be unclamped: %+v", src.got)
	}
}

func TestHistorySwapRatesTimeoutOutageUnknown(t *testing.T) {
	src := &fakeSwapSource{err: context.DeadlineExceeded}
	d := swapDeps(src)
	d.Guard.Timeout = time.Millisecond
	rec := httptest.NewRecorder()
	HistorySwapRates(d).ServeHTTP(rec,
		httptest.NewRequest(http.MethodGet, "/api/v1/history/swap-rates?symbol=EUR%2FUSD", nil))
	wantErrorStatus(t, rec, http.StatusGatewayTimeout)

	src2 := &fakeSwapSource{err: errors.New("pg down")}
	rec2 := httptest.NewRecorder()
	HistorySwapRates(swapDeps(src2)).ServeHTTP(rec2,
		httptest.NewRequest(http.MethodGet, "/api/v1/history/swap-rates?symbol=EUR%2FUSD", nil))
	wantErrorStatus(t, rec2, http.StatusServiceUnavailable)

	d3 := swapDeps(&fakeSwapSource{})
	d3.Instruments = &fakeMarketStore{}
	rec3 := httptest.NewRecorder()
	HistorySwapRates(d3).ServeHTTP(rec3,
		httptest.NewRequest(http.MethodGet, "/api/v1/history/swap-rates?symbol=NOPE", nil))
	wantErrorStatus(t, rec3, http.StatusNotFound)

	rec4 := httptest.NewRecorder()
	HistorySwapRates(&SwapRateHistoryDeps{}).ServeHTTP(rec4,
		httptest.NewRequest(http.MethodGet, "/api/v1/history/swap-rates", nil))
	wantErrorStatus(t, rec4, http.StatusServiceUnavailable)
}

func TestHistorySwapRatesCSVAndCache(t *testing.T) {
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	kv := newFakeHistoryKV()
	src := &fakeSwapSource{rows: swapJournalFixture()}
	d := swapDeps(src)
	d.Cache = kv
	d.Guard.Now = func() time.Time { return now }
	url := "/api/v1/history/swap-rates?symbol=EUR%2FUSD" +
		"&from=2026-10-07T00:00:00Z&to=2026-10-08T00:00:00Z"
	req := httptest.NewRequest(http.MethodGet, url, nil)
	req.Header.Set("Accept", "text/csv")
	rec := httptest.NewRecorder()
	HistorySwapRates(d).ServeHTTP(rec, req)
	if rec.Code != 200 || rec.Header().Get("Content-Type") != "text/csv" {
		t.Fatalf("csv: %d %q — %s", rec.Code,
			rec.Header().Get("Content-Type"), rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), ",triple,") ||
		!strings.Contains(rec.Body.String(), "3,true,20") {
		t.Fatalf("csv body = %q", rec.Body.String())
	}
	if len(kv.m) != 1 {
		t.Fatalf("cached = %d", len(kv.m))
	}
	// Second request → hit, no store call.
	src2 := &fakeSwapSource{}
	d2 := swapDeps(src2)
	d2.Cache = kv
	d2.Guard.Now = func() time.Time { return now }
	req2 := httptest.NewRequest(http.MethodGet, url, nil)
	req2.Header.Set("Accept", "text/csv")
	rec2 := httptest.NewRecorder()
	HistorySwapRates(d2).ServeHTTP(rec2, req2)
	if rec2.Header().Get("X-History-Cache") != "hit" || src2.calls != 0 {
		t.Fatalf("cache hit: hdr %q calls %d",
			rec2.Header().Get("X-History-Cache"), src2.calls)
	}
}
