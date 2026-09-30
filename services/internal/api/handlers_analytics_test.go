// Handler tests — Phase-20 Tasks 20.3.4/20.3.5 (/api/v1/analytics/*).
// Fake PnLReporter/StatsReporter seams; no ClickHouse needed.
package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"exchange/internal/analytics"
	"exchange/pkg/decimal"
)

type fakePnLReporter struct {
	rows []analytics.PnLRow
	err  error
}

func (f *fakePnLReporter) Report(_ context.Context, _ int64, _, _ time.Time) ([]analytics.PnLRow, error) {
	return f.rows, f.err
}

type fakeStatsReporter struct {
	volume []analytics.VolumeRow
	fills  []analytics.FillRate
	tiers  []analytics.TierTradeRow
	err    error
}

func (f *fakeStatsReporter) Volume(context.Context, time.Time, time.Time, string, string, int) ([]analytics.VolumeRow, error) {
	return f.volume, f.err
}
func (f *fakeStatsReporter) FillRates(context.Context, time.Time, time.Time) ([]analytics.FillRate, error) {
	return f.fills, f.err
}
func (f *fakeStatsReporter) TradesPerTier(context.Context, time.Time, time.Time) ([]analytics.TierTradeRow, error) {
	return f.tiers, f.err
}

var analyticsFixedNow = time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)

func analyticsDeps(pnl PnLReporter, stats StatsReporter) *AnalyticsDeps {
	return &AnalyticsDeps{PnL: pnl, Stats: stats,
		Now: func() time.Time { return analyticsFixedNow }}
}

func samplePnLRows() []analytics.PnLRow {
	return []analytics.PnLRow{
		{AccountID: 7, InstrumentID: 101, Symbol: "EUR/USD",
			Day:        time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC),
			Realized:   decimal.RequireFromString("10.5"),
			Unrealized: decimal.RequireFromString("-2.25"),
			Fees:       decimal.RequireFromString("0.75")},
	}
}

// --- /api/v1/analytics/pnl -------------------------------------------------

func TestAnalyticsPnLRequiresClaims(t *testing.T) {
	h := AnalyticsPnL(analyticsDeps(&fakePnLReporter{}, nil))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/analytics/pnl", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if env := decodeErr(t, rec); env.Error != "UNAUTHORIZED" {
		t.Fatalf("code = %s", env.Error)
	}
}

func TestAnalyticsPnLRejectsForeignAccount(t *testing.T) {
	h := AnalyticsPnL(analyticsDeps(&fakePnLReporter{}, nil))
	rec := httptest.NewRecorder()
	req := userCtx(httptest.NewRequest(http.MethodGet,
		"/api/v1/analytics/pnl?account_id=99", nil), 7)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
}

func TestAnalyticsPnLNilStoreDegraded(t *testing.T) {
	h := AnalyticsPnL(&AnalyticsDeps{})
	rec := httptest.NewRecorder()
	req := userCtx(httptest.NewRequest(http.MethodGet, "/api/v1/analytics/pnl", nil), 7)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	if env := decodeErr(t, rec); env.Error != "SERVICE_DEGRADED" {
		t.Fatalf("code = %s", env.Error)
	}
}

func TestAnalyticsPnLJSONReport(t *testing.T) {
	h := AnalyticsPnL(analyticsDeps(&fakePnLReporter{rows: samplePnLRows()}, nil))
	rec := httptest.NewRecorder()
	req := userCtx(httptest.NewRequest(http.MethodGet,
		"/api/v1/analytics/pnl?from=2026-09-19&to=2026-09-21", nil), 7)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{
		`"account_id":7`, `"instrument_id":101`, `"symbol":"EUR/USD"`,
		`"day":"2026-09-20"`,
		`"realized":"10.5"`, `"unrealized":"-2.25"`, `"fees":"0.75"`,
		`"net":"7.5"`, // 10.5 − 2.25 − 0.75
		`"from":"2026-09-19T00:00:00Z"`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("body missing %s: %s", want, body)
		}
	}
}

func TestAnalyticsPnLWindowValidation(t *testing.T) {
	h := AnalyticsPnL(analyticsDeps(&fakePnLReporter{}, nil))
	for _, q := range []string{
		"from=2026-09-21&to=2026-09-19", // from >= to
		"from=not-a-date",
		"to=bogus",
		"from=2020-01-01&to=2026-09-21", // >397d window
	} {
		rec := httptest.NewRecorder()
		req := userCtx(httptest.NewRequest(http.MethodGet,
			"/api/v1/analytics/pnl?"+q, nil), 7)
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s: status = %d, want 400", q, rec.Code)
		}
	}
}

func TestAnalyticsPnLCSVExport(t *testing.T) {
	h := AnalyticsPnL(analyticsDeps(&fakePnLReporter{rows: samplePnLRows()}, nil))
	rec := httptest.NewRecorder()
	req := userCtx(httptest.NewRequest(http.MethodGet,
		"/api/v1/analytics/pnl?format=csv", nil), 7)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/csv") {
		t.Fatalf("content-type = %s", ct)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"account_id,7",
		"account_id,instrument_id,symbol,day,realized,unrealized,fees,net",
		"7,101,EUR/USD,2026-09-20,10.5,-2.25,0.75,7.5",
		"realized,unrealized,fees,net",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("csv missing %q:\n%s", want, body)
		}
	}
}

func TestAnalyticsPnLPDFExport(t *testing.T) {
	h := AnalyticsPnL(analyticsDeps(&fakePnLReporter{rows: samplePnLRows()}, nil))
	rec := httptest.NewRecorder()
	req := userCtx(httptest.NewRequest(http.MethodGet,
		"/api/v1/analytics/pnl?format=pdf", nil), 7)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/pdf" {
		t.Fatalf("content-type = %s", ct)
	}
	if body := rec.Body.Bytes(); len(body) < 100 || !strings.HasPrefix(string(body), "%PDF-1.4") {
		t.Fatalf("not a PDF (%d bytes)", len(body))
	}
}

func TestAnalyticsPnLAcceptNegotiatesCSV(t *testing.T) {
	h := AnalyticsPnL(analyticsDeps(&fakePnLReporter{rows: samplePnLRows()}, nil))
	rec := httptest.NewRecorder()
	req := userCtx(httptest.NewRequest(http.MethodGet, "/api/v1/analytics/pnl", nil), 7)
	req.Header.Set("Accept", "text/csv")
	h.ServeHTTP(rec, req)
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/csv") {
		t.Fatalf("content-type = %s, want csv", ct)
	}
}

func TestAnalyticsPnLBadFormatAndStoreError(t *testing.T) {
	h := AnalyticsPnL(analyticsDeps(&fakePnLReporter{rows: samplePnLRows()}, nil))
	rec := httptest.NewRecorder()
	req := userCtx(httptest.NewRequest(http.MethodGet,
		"/api/v1/analytics/pnl?format=xls", nil), 7)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("format=xls status = %d, want 400", rec.Code)
	}

	h2 := AnalyticsPnL(analyticsDeps(&fakePnLReporter{err: errors.New("ch down")}, nil))
	rec2 := httptest.NewRecorder()
	req2 := userCtx(httptest.NewRequest(http.MethodGet, "/api/v1/analytics/pnl", nil), 7)
	h2.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusServiceUnavailable {
		t.Fatalf("store error status = %d, want 503", rec2.Code)
	}
	if env := decodeErr(t, rec2); env.Error != "SERVICE_DEGRADED" {
		t.Fatalf("code = %s", env.Error)
	}
}

// --- /api/v1/analytics/volume ----------------------------------------------

func TestAnalyticsVolumeHappyPath(t *testing.T) {
	stats := &fakeStatsReporter{volume: []analytics.VolumeRow{
		{Symbol: "EUR/USD",
			BucketStart: time.Date(2026, 9, 20, 14, 0, 0, 0, time.UTC),
			Granularity: "1h",
			Volume:      decimal.RequireFromString("1500.5"),
			QuoteVolume: decimal.RequireFromString("1650.2"),
			TradeCount:  12},
	}}
	h := AnalyticsVolume(analyticsDeps(nil, stats))
	rec := httptest.NewRecorder()
	// Public endpoint — no claims needed.
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet,
		"/api/v1/analytics/volume?from=2026-09-20&to=2026-09-21&symbol=EUR/USD&granularity=1h", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{
		`"symbol":"EUR/USD"`, `"granularity":"1h"`, `"volume":"1500.5"`,
		`"quote_volume":"1650.2"`, `"trade_count":12`, `"count":1`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("body missing %s: %s", want, body)
		}
	}
}

func TestAnalyticsVolumeValidatesAndDegrades(t *testing.T) {
	h := AnalyticsVolume(analyticsDeps(nil, &fakeStatsReporter{}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet,
		"/api/v1/analytics/volume?granularity=5m", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("granularity=5m status = %d, want 400", rec.Code)
	}

	hNil := AnalyticsVolume(&AnalyticsDeps{})
	rec2 := httptest.NewRecorder()
	hNil.ServeHTTP(rec2, httptest.NewRequest(http.MethodGet, "/api/v1/analytics/volume", nil))
	if rec2.Code != http.StatusServiceUnavailable {
		t.Fatalf("nil store status = %d, want 503", rec2.Code)
	}

	hErr := AnalyticsVolume(analyticsDeps(nil, &fakeStatsReporter{err: errors.New("ch down")}))
	rec3 := httptest.NewRecorder()
	hErr.ServeHTTP(rec3, httptest.NewRequest(http.MethodGet, "/api/v1/analytics/volume", nil))
	if rec3.Code != http.StatusServiceUnavailable {
		t.Fatalf("store error status = %d, want 503", rec3.Code)
	}
}

// --- /api/v1/analytics/stats -----------------------------------------------

func TestAnalyticsStatsHappyPath(t *testing.T) {
	stats := &fakeStatsReporter{
		fills: []analytics.FillRate{
			{Symbol: "EUR/USD", OrdersSubmitted: 100, OrdersFilled: 97},
			{Symbol: "USD/JPY", OrdersSubmitted: 0, OrdersFilled: 0},
		},
		tiers: []analytics.TierTradeRow{
			{Symbol: "EUR/USD", Tier: "RETAIL", TradeCount: 60,
				Volume: decimal.RequireFromString("800")},
			{Symbol: "EUR/USD", Tier: "PROFESSIONAL", TradeCount: 37,
				Volume: decimal.RequireFromString("700")},
		},
	}
	h := AnalyticsStats(analyticsDeps(nil, stats))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/analytics/stats", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{
		`"orders_submitted":100`, `"orders_filled":97`, `"fill_rate":"0.97"`,
		`"tier":"RETAIL"`, `"trade_count":60`, `"volume":"800"`,
		`"trade_counts_by_tier"`, `"totals"`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("body missing %s: %s", want, body)
		}
	}
	// Zero-submission symbol carries null fill_rate — not a fabricated 0.
	if !strings.Contains(body, `"fill_rate":null`) {
		t.Fatalf("expected null fill_rate for zero submissions: %s", body)
	}
}

func TestAnalyticsStatsDegradedPaths(t *testing.T) {
	hNil := AnalyticsStats(&AnalyticsDeps{})
	rec := httptest.NewRecorder()
	hNil.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/analytics/stats", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("nil store status = %d, want 503", rec.Code)
	}

	hErr := AnalyticsStats(analyticsDeps(nil, &fakeStatsReporter{err: errors.New("ch down")}))
	rec2 := httptest.NewRecorder()
	hErr.ServeHTTP(rec2, httptest.NewRequest(http.MethodGet, "/api/v1/analytics/stats", nil))
	if rec2.Code != http.StatusServiceUnavailable {
		t.Fatalf("store error status = %d, want 503", rec2.Code)
	}
}

// --- pdfdoc ----------------------------------------------------------------

func TestRenderPDFDocStructure(t *testing.T) {
	doc := RenderPDFDoc("Test Report", []PDFLine{
		{Text: "line one"},
		{Text: "esc (paren) \\ slash", Size: 10},
	})
	s := string(doc)
	for _, frag := range []string{
		"%PDF-1.4", "/Type /Catalog", "/Type /Pages", "/BaseFont /Courier",
		"xref", "%%EOF", "(Test Report)", `\(paren\)`, `\\ slash`,
	} {
		if !strings.Contains(s, frag) {
			t.Fatalf("pdf missing %q", frag)
		}
	}
	// Pagination: 60 lines → more than one page.
	doc2 := RenderPDFDoc("T", func() []PDFLine {
		ls := make([]PDFLine, 60)
		for i := range ls {
			ls[i] = PDFLine{Text: "x"}
		}
		return ls
	}())
	if !strings.Contains(string(doc2), "/Count 2") {
		t.Fatal("60 lines must paginate to 2 pages")
	}
}
