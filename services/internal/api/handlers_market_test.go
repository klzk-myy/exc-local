// Wave-2 cluster C handler tests — market data REST (Task 5.3.5),
// server time (5.3.43), venue info (5.3.44), announcements &
// maintenance (5.3.14). Stores are in-memory fakes; integration coverage
// over real PostgreSQL lives in internal/marketapi/pg_integration_test.go.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"exchange/internal/auth"
	"exchange/internal/marketapi"
	"exchange/internal/marketdata"

	"github.com/shopspring/decimal"
)

// ---------------------------------------------------------------------------
// Fakes
// ---------------------------------------------------------------------------

type fakeMarketStore struct {
	instruments []marketapi.Instrument
	trades      []marketapi.Trade
	ticker      *marketapi.Ticker
	klines      []marketapi.Kline
	book        *marketapi.BookSnapshot
	stats       *marketapi.Stats24h
	statsAll    []marketapi.Stats24h
	err         error
}

func (f *fakeMarketStore) ListInstruments(context.Context) ([]marketapi.Instrument, error) {
	return f.instruments, f.err
}

func (f *fakeMarketStore) InstrumentBySymbol(_ context.Context, sym string) (*marketapi.Instrument, error) {
	if f.err != nil {
		return nil, f.err
	}
	for i := range f.instruments {
		if f.instruments[i].Symbol == sym {
			return &f.instruments[i], nil
		}
	}
	return nil, nil
}

func (f *fakeMarketStore) RecentTrades(context.Context, string, int) ([]marketapi.Trade, error) {
	return f.trades, f.err
}

func (f *fakeMarketStore) Ticker24h(_ context.Context, sym string, _ time.Time) (*marketapi.Ticker, error) {
	return f.ticker, f.err
}

func (f *fakeMarketStore) Klines(context.Context, string, string, time.Time, time.Time, int) ([]marketapi.Kline, error) {
	return f.klines, f.err
}

func (f *fakeMarketStore) Snapshot(context.Context, string, int) (*marketapi.BookSnapshot, error) {
	return f.book, f.err
}

func (f *fakeMarketStore) Stats24h(_ context.Context, sym string, _ time.Time) (*marketapi.Stats24h, error) {
	if f.err != nil || f.stats == nil {
		return nil, f.err
	}
	if f.stats.Symbol == sym {
		return f.stats, nil
	}
	return nil, nil
}

func (f *fakeMarketStore) Stats24hAll(context.Context, time.Time) ([]marketapi.Stats24h, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.statsAll, nil
}

type fakeAnnounceStore struct {
	items  map[int64]*marketapi.Announcement
	nextID int64
	err    error
}

func newFakeAnnounceStore() *fakeAnnounceStore {
	return &fakeAnnounceStore{items: map[int64]*marketapi.Announcement{}, nextID: 1}
}

func (f *fakeAnnounceStore) ListAnnouncements(_ context.Context, flt marketapi.AnnouncementFilter) ([]marketapi.Announcement, error) {
	if f.err != nil {
		return nil, f.err
	}
	var out []marketapi.Announcement
	for _, a := range f.items {
		if flt.IncludeAll || a.Live(time.Now()) {
			if flt.Category == "" || a.Category == flt.Category {
				out = append(out, *a)
			}
		}
	}
	if flt.Limit > 0 && len(out) > flt.Limit {
		out = out[:flt.Limit]
	}
	return out, nil
}

func (f *fakeAnnounceStore) Announcement(_ context.Context, id int64) (*marketapi.Announcement, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.items[id], nil
}

func (f *fakeAnnounceStore) CreateAnnouncement(_ context.Context, a marketapi.Announcement) (*marketapi.Announcement, error) {
	if f.err != nil {
		return nil, f.err
	}
	a.ID = f.nextID
	f.nextID++
	f.items[a.ID] = &a
	return &a, nil
}

func (f *fakeAnnounceStore) UpdateAnnouncement(_ context.Context, a marketapi.Announcement) (*marketapi.Announcement, error) {
	if f.err != nil {
		return nil, f.err
	}
	if f.items[a.ID] == nil {
		return nil, nil
	}
	f.items[a.ID] = &a
	return &a, nil
}

func (f *fakeAnnounceStore) RetractAnnouncement(_ context.Context, id int64, _ string) (bool, error) {
	if f.err != nil {
		return false, f.err
	}
	a := f.items[id]
	if a == nil {
		return false, nil
	}
	a.Status = "RETRACTED"
	return true, nil
}

type fakeMaintStore struct {
	items  map[int64]*marketapi.MaintenanceWindow
	nextID int64
	err    error
}

func newFakeMaintStore() *fakeMaintStore {
	return &fakeMaintStore{items: map[int64]*marketapi.MaintenanceWindow{}, nextID: 1}
}

func (f *fakeMaintStore) UpcomingMaintenance(_ context.Context, now time.Time) ([]marketapi.MaintenanceWindow, error) {
	if f.err != nil {
		return nil, f.err
	}
	var out []marketapi.MaintenanceWindow
	for _, m := range f.items {
		if (m.Status == "SCHEDULED" || m.Status == "IN_PROGRESS") && m.EndsAt.After(now) {
			out = append(out, *m)
		}
	}
	return out, nil
}

func (f *fakeMaintStore) ListMaintenance(_ context.Context, _ int) ([]marketapi.MaintenanceWindow, error) {
	if f.err != nil {
		return nil, f.err
	}
	var out []marketapi.MaintenanceWindow
	for _, m := range f.items {
		out = append(out, *m)
	}
	return out, nil
}

func (f *fakeMaintStore) CreateMaintenance(_ context.Context, m marketapi.MaintenanceWindow) (*marketapi.MaintenanceWindow, error) {
	if f.err != nil {
		return nil, f.err
	}
	m.ID = f.nextID
	f.nextID++
	f.items[m.ID] = &m
	return &m, nil
}

func (f *fakeMaintStore) UpdateMaintenance(_ context.Context, m marketapi.MaintenanceWindow) (*marketapi.MaintenanceWindow, error) {
	if f.err != nil {
		return nil, f.err
	}
	if f.items[m.ID] == nil {
		return nil, nil
	}
	f.items[m.ID] = &m
	return &m, nil
}

func (f *fakeMaintStore) CancelMaintenance(_ context.Context, id int64, _ string) (bool, error) {
	if f.err != nil {
		return false, f.err
	}
	m := f.items[id]
	if m == nil {
		return false, nil
	}
	m.Status = "CANCELLED"
	return true, nil
}

// ---------------------------------------------------------------------------
// Fixtures & helpers
// ---------------------------------------------------------------------------

func fixtureInstrument() marketapi.Instrument {
	minP, spread := "0.00001", "50"
	open, algo := int64(200), int64(50)
	return marketapi.Instrument{
		ID: 1, Symbol: "EUR/USD", BaseCurrency: "EUR", QuoteCurrency: "USD",
		InstrumentType: "SPOT", Status: "ACTIVE",
		TickSize: "0.00001", LotSize: "1000",
		MinOrderQty: "1000", MaxOrderQty: "100000000", MinNotional: "10",
		MinPrice: &minP, MaxPrice: nil,
		PriceBandPctUp: "2.00", PriceBandPctDown: "5.00",
		MaxSpreadPips: &spread, MaxOpenOrders: &open, MaxAlgoOrders: &algo,
		MaxLeverage: 30, SettlementCycle: 1,
		UpdatedAt: time.UnixMilli(1700000000000),
	}
}

func marketDeps() *MarketDeps {
	f := &fakeMarketStore{
		instruments: []marketapi.Instrument{fixtureInstrument()},
		trades: []marketapi.Trade{
			{ID: 7, Symbol: "EUR/USD", Price: "1.08421", Quantity: "5000", Seq: 3, TimeMs: 1700000001000},
		},
		ticker: &marketapi.Ticker{
			Symbol: "EUR/USD", Window: "24h", Volume: "5000",
			QuoteVolume: "5421.05", TradeCount: 1, ServerTimeMs: 1700000002000,
		},
		klines: []marketapi.Kline{
			{OpenTimeMs: 1699999980000, Open: "1.084", High: "1.085",
				Low: "1.083", Close: "1.0842", Volume: "5000",
				QuoteVolume: "5420", TradeCount: 1, Closed: true},
		},
		book: &marketapi.BookSnapshot{
			Symbol: "EUR/USD", Seq: 42, Depth: 20,
			Bids:        []marketapi.BookLevel{{Price: "1.08410", Quantity: "10000"}},
			Asks:        []marketapi.BookLevel{{Price: "1.08425", Quantity: "8000"}},
			UpdatedAtMs: 1700000002000,
		},
	}
	return &MarketDeps{Store: f, Book: f, Cache: marketapi.NewCache(nil)}
}

func decode(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("body=%s err=%v", rec.Body.String(), err)
	}
	return m
}

func doReq(h http.HandlerFunc, method, path string, pathVals map[string]string, query string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path+query, nil)
	for k, v := range pathVals {
		req.SetPathValue(k, v)
	}
	rec := httptest.NewRecorder()
	h(rec, req)
	return rec
}

func problemCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var e ErrorEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
		t.Fatalf("error body=%s err=%v", rec.Body.String(), err)
	}
	return e.Error
}

// ---------------------------------------------------------------------------
// Market data (Task 5.3.5)
// ---------------------------------------------------------------------------

func TestBookSnapshotEndpoint(t *testing.T) {
	d := marketDeps()
	rec := doReq(MarketBook(d), "GET", "/api/v1/book/EUR/USD",
		map[string]string{"symbol": "EUR/USD"}, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	m := decode(t, rec)
	if m["seq"].(float64) != 42 || len(m["bids"].([]any)) != 1 {
		t.Fatalf("book=%v", m)
	}
	// depth bound
	rec = doReq(MarketBook(d), "GET", "/api/v1/book/EUR/USD",
		map[string]string{"symbol": "EUR/USD"}, "?depth=500")
	if rec.Code != http.StatusBadRequest || problemCode(t, rec) != "INVALID_REQUEST" {
		t.Fatalf("depth 500: status=%d", rec.Code)
	}
	// unknown symbol → 404
	d.Book = &fakeMarketStore{}
	rec = doReq(MarketBook(d), "GET", "/api/v1/book/ZZZ/AAA",
		map[string]string{"symbol": "ZZZ/AAA"}, "")
	if rec.Code != http.StatusNotFound || problemCode(t, rec) != "NOT_FOUND" {
		t.Fatalf("unknown symbol: status=%d", rec.Code)
	}
	// store failure → 503 SERVICE_DEGRADED (fail-closed, never empty book);
	// fresh cache so the earlier success isn't served stale.
	d.Book = &fakeMarketStore{err: errors.New("pg down")}
	d.Cache = marketapi.NewCache(nil)
	rec = doReq(MarketBook(d), "GET", "/api/v1/book/EUR/USD",
		map[string]string{"symbol": "EUR/USD"}, "")
	if rec.Code != http.StatusServiceUnavailable || problemCode(t, rec) != "SERVICE_DEGRADED" {
		t.Fatalf("store err: status=%d", rec.Code)
	}
}

func TestTradesEndpoint(t *testing.T) {
	d := marketDeps()
	rec := doReq(MarketTrades(d), "GET", "/api/v1/trades/EUR/USD",
		map[string]string{"symbol": "EUR/USD"}, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	m := decode(t, rec)
	if m["count"].(float64) != 1 {
		t.Fatalf("trades=%v", m)
	}
	// unknown symbol → 404
	d.Store = &fakeMarketStore{}
	d.Cache = marketapi.NewCache(nil)
	rec = doReq(MarketTrades(d), "GET", "/api/v1/trades/ZZZ/AAA",
		map[string]string{"symbol": "ZZZ/AAA"}, "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown symbol: status=%d", rec.Code)
	}
	// limit validation
	d.Store = marketDeps().Store
	d.Cache = marketapi.NewCache(nil)
	rec = doReq(MarketTrades(d), "GET", "/api/v1/trades/EUR/USD",
		map[string]string{"symbol": "EUR/USD"}, "?limit=0")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("limit 0: status=%d", rec.Code)
	}
}

func TestTickerEndpointQuietAndMissing(t *testing.T) {
	d := marketDeps()
	rec := doReq(MarketTicker(d), "GET", "/api/v1/ticker/EUR/USD",
		map[string]string{"symbol": "EUR/USD"}, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	m := decode(t, rec)
	if m["trade_count"].(float64) != 1 || m["volume"] != "5000" {
		t.Fatalf("ticker=%v", m)
	}
	// Quiet symbol: zeroed ticker, price fields null — no fabricated prices.
	d.Store = &fakeMarketStore{
		instruments: []marketapi.Instrument{fixtureInstrument()},
		ticker:      &marketapi.Ticker{Symbol: "EUR/USD", Window: "24h", Volume: "0", QuoteVolume: "0"},
	}
	d.Cache = marketapi.NewCache(nil)
	rec = doReq(MarketTicker(d), "GET", "/api/v1/ticker/EUR/USD",
		map[string]string{"symbol": "EUR/USD"}, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("quiet status=%d", rec.Code)
	}
	m = decode(t, rec)
	if m["last"] != nil || m["high"] != nil {
		t.Fatalf("quiet ticker fabricates prices: %v", m)
	}
	// Unknown symbol → 404.
	d.Store = &fakeMarketStore{}
	d.Cache = marketapi.NewCache(nil)
	rec = doReq(MarketTicker(d), "GET", "/api/v1/ticker/ZZZ",
		map[string]string{"symbol": "ZZZ"}, "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown: status=%d", rec.Code)
	}
}

func TestKlinesEndpoint(t *testing.T) {
	d := marketDeps()
	rec := doReq(MarketKlines(d), "GET", "/api/v1/klines/EUR/USD",
		map[string]string{"symbol": "EUR/USD"}, "?interval=1m")
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	m := decode(t, rec)
	if m["count"].(float64) != 1 || m["interval"] != "1m" {
		t.Fatalf("klines=%v", m)
	}
	// Non-canonical interval → 400.
	rec = doReq(MarketKlines(d), "GET", "/api/v1/klines/EUR/USD",
		map[string]string{"symbol": "EUR/USD"}, "?interval=7m")
	if rec.Code != http.StatusBadRequest || problemCode(t, rec) != "INVALID_REQUEST" {
		t.Fatalf("interval 7m: status=%d", rec.Code)
	}
	// Unknown symbol → 404.
	d.Store = &fakeMarketStore{}
	d.Cache = marketapi.NewCache(nil)
	rec = doReq(MarketKlines(d), "GET", "/api/v1/klines/ZZZ",
		map[string]string{"symbol": "ZZZ"}, "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown: status=%d", rec.Code)
	}
}

func TestInstrumentsEndpoint(t *testing.T) {
	d := marketDeps()
	rec := doReq(Instruments(d), "GET", "/api/v1/instruments", nil, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	m := decode(t, rec)
	if m["count"].(float64) != 1 {
		t.Fatalf("instruments=%v", m)
	}
	row := m["data"].([]any)[0].(map[string]any)
	if row["symbol"] != "EUR/USD" || len(row["filters"].([]any)) != 6 {
		t.Fatalf("row=%v", row)
	}
	// Store failure → 503, not an empty list.
	d.Store = &fakeMarketStore{err: errors.New("pg down")}
	d.Cache = marketapi.NewCache(nil)
	rec = doReq(Instruments(d), "GET", "/api/v1/instruments", nil, "")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("store err: status=%d", rec.Code)
	}
}

// ---------------------------------------------------------------------------
// Server time & venue info (Tasks 5.3.43/5.3.44)
// ---------------------------------------------------------------------------

func TestServerTime(t *testing.T) {
	now := func() time.Time { return time.UnixMilli(1700000000123).UTC() }
	src := func() (time.Duration, bool, error) { return 12 * time.Microsecond, true, nil }
	rec := doReq(ServerTime(src, now), "GET", "/api/v1/time", nil, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d", rec.Code)
	}
	m := decode(t, rec)
	if m["server_time_ms"].(float64) != 1700000000123 || m["timezone"] != "UTC" {
		t.Fatalf("time=%v", m)
	}
	if m["synchronized"] != true || m["offset_us"].(float64) != 12 {
		t.Fatalf("sync fields=%v", m)
	}
	// Nil source still serves the wall clock without claiming sync state.
	rec = doReq(ServerTime(nil, now), "GET", "/api/v1/time", nil, "")
	m = decode(t, rec)
	if _, ok := m["synchronized"]; ok {
		t.Fatalf("nil src must not claim sync: %v", m)
	}
}

func TestExchangeInfoETagAnd304(t *testing.T) {
	d := &VenueDeps{Store: marketDeps().Store, Cache: marketapi.NewCache(nil)}
	h := ExchangeInfo(d)
	rec := doReq(h, "GET", "/api/v1/exchange-info", nil, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	etag := rec.Header().Get("ETag")
	if etag == "" {
		t.Fatal("missing ETag")
	}
	m := decode(t, rec)
	for _, key := range []string{"timezone", "server_time_ms", "trading_hours",
		"rate_limits", "route_weights", "symbols"} {
		if _, ok := m[key]; !ok {
			t.Fatalf("doc missing %q: %v", key, m)
		}
	}
	sym := m["symbols"].([]any)[0].(map[string]any)
	if len(sym["filters"].([]any)) != 6 || len(sym["order_types"].([]any)) != 16 {
		t.Fatalf("symbol doc=%v", sym)
	}
	perms := sym["permissions"].(map[string]any)
	if perms["new_orders_allowed"] != true || perms["market_orders_allowed"] != true {
		t.Fatalf("permissions=%v", perms)
	}
	// Conditional request → 304.
	req := httptest.NewRequest("GET", "/api/v1/exchange-info", nil)
	req.Header.Set("If-None-Match", etag)
	rec = httptest.NewRecorder()
	h(rec, req)
	if rec.Code != http.StatusNotModified {
		t.Fatalf("304 expected, got %d", rec.Code)
	}
	// A mutation flips the tag after the instruments cache expires.
	inst := fixtureInstrument()
	inst.UpdatedAt = inst.UpdatedAt.Add(time.Hour)
	fresh := &fakeMarketStore{instruments: []marketapi.Instrument{inst}}
	d2 := &VenueDeps{Store: fresh, Cache: marketapi.NewCache(nil)}
	rec = httptest.NewRecorder()
	req = httptest.NewRequest("GET", "/api/v1/exchange-info", nil)
	req.Header.Set("If-None-Match", etag)
	ExchangeInfo(d2)(rec, req)
	if rec.Code == http.StatusNotModified {
		t.Fatal("etag must flip on instrument change")
	}
}

// ---------------------------------------------------------------------------
// Announcements & maintenance (Task 5.3.14)
// ---------------------------------------------------------------------------

func announceDeps() *AnnounceDeps {
	return &AnnounceDeps{
		Announcements: newFakeAnnounceStore(),
		Maintenance:   newFakeMaintStore(),
	}
}

func adminCtx(r *http.Request) *http.Request {
	return r.WithContext(auth.WithClaims(r.Context(),
		auth.Claims{Subject: "ops-1", AccountID: 1, Scopes: []string{"admin"}}))
}

func TestAnnouncementCRUD(t *testing.T) {
	d := announceDeps()
	create := CreateAnnouncement(d)
	patch := UpdateAnnouncement(d)
	del := RetractAnnouncement(d)
	listPub := Announcements(d)
	listAdmin := AdminAnnouncements(d)
	getOne := AnnouncementByID(d)

	// validation: missing title/body → 400
	req := httptest.NewRequest("POST", "/api/v1/admin/announcements",
		strings.NewReader(`{"title":"x"}`))
	rec := httptest.NewRecorder()
	create(rec, adminCtx(req))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad create status=%d", rec.Code)
	}
	// bad category → 400
	req = httptest.NewRequest("POST", "/api/v1/admin/announcements",
		strings.NewReader(`{"title":"t","body":"b","category":"BOGUS"}`))
	rec = httptest.NewRecorder()
	create(rec, adminCtx(req))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad category status=%d", rec.Code)
	}
	// create
	req = httptest.NewRequest("POST", "/api/v1/admin/announcements",
		strings.NewReader(`{"title":"Fee update","body":"fees change","category":"PRODUCT"}`))
	rec = httptest.NewRecorder()
	create(rec, adminCtx(req))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", rec.Code, rec.Body.String())
	}
	m := decode(t, rec)
	if m["created_by"] != "ops-1" || m["status"] != "PUBLISHED" {
		t.Fatalf("created=%v", m)
	}
	id := int64(m["id"].(float64))

	// public list sees it; draft doesn't.
	rec = doReq(listPub, "GET", "/api/v1/announcements", nil, "")
	if rec.Code != http.StatusOK || decode(t, rec)["count"].(float64) != 1 {
		t.Fatalf("public list=%v", rec.Body.String())
	}
	rec = doReq(getOne, "GET", "/api/v1/announcements/1",
		map[string]string{"id": "1"}, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("get status=%d", rec.Code)
	}

	// draft → invisible publicly, visible to admin.
	req = httptest.NewRequest("POST", "/api/v1/admin/announcements",
		strings.NewReader(`{"title":"Draft","body":"wip","status":"DRAFT"}`))
	rec = httptest.NewRecorder()
	create(rec, adminCtx(req))
	draftID := int64(decode(t, rec)["id"].(float64))
	rec = doReq(listPub, "GET", "/api/v1/announcements", nil, "")
	if decode(t, rec)["count"].(float64) != 1 {
		t.Fatal("draft leaked to public list")
	}
	rec = doReq(listAdmin, "GET", "/api/v1/admin/announcements", nil, "")
	if decode(t, rec)["count"].(float64) != 2 {
		t.Fatal("admin list should include draft")
	}
	req = httptest.NewRequest("GET", "/api/v1/announcements/x", nil)
	req.SetPathValue("id", itoa64(draftID))
	rec = httptest.NewRecorder()
	getOne(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("draft get status=%d, want 404", rec.Code)
	}

	// patch: retract → public 404.
	req = httptest.NewRequest("PATCH", "/api/v1/admin/announcements/x",
		strings.NewReader(`{"status":"RETRACTED"}`))
	req.SetPathValue("id", "1")
	rec = httptest.NewRecorder()
	patch(rec, adminCtx(req))
	if rec.Code != http.StatusOK || decode(t, rec)["status"] != "RETRACTED" {
		t.Fatalf("patch status=%d body=%s", rec.Code, rec.Body.String())
	}
	rec = doReq(getOne, "GET", "/api/v1/announcements/1",
		map[string]string{"id": "1"}, "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("retracted get status=%d, want 404", rec.Code)
	}

	// delete = retract transition; unknown id → 404.
	req = httptest.NewRequest("DELETE", "/api/v1/admin/announcements/99", nil)
	req.SetPathValue("id", "99")
	rec = httptest.NewRecorder()
	del(rec, adminCtx(req))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("del unknown status=%d", rec.Code)
	}
	req = httptest.NewRequest("DELETE", "/api/v1/admin/announcements/1", nil)
	req.SetPathValue("id", itoa64(id))
	rec = httptest.NewRecorder()
	del(rec, adminCtx(req))
	if rec.Code != http.StatusOK || decode(t, rec)["status"] != "RETRACTED" {
		t.Fatalf("del status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestMaintenanceScheduleCRUD(t *testing.T) {
	d := announceDeps()
	pub := MaintenanceSchedule(d)
	create := CreateMaintenance(d)
	patch := UpdateMaintenance(d)
	cancel := CancelMaintenance(d)
	listAll := AdminMaintenanceWindows(d)

	// ends <= starts → 400.
	req := httptest.NewRequest("POST", "/api/v1/admin/maintenance-windows",
		strings.NewReader(`{"title":"w","starts_at":"2026-01-02T00:00:00Z","ends_at":"2026-01-01T00:00:00Z"}`))
	rec := httptest.NewRecorder()
	create(rec, adminCtx(req))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad window status=%d", rec.Code)
	}
	// create scheduled window.
	req = httptest.NewRequest("POST", "/api/v1/admin/maintenance-windows",
		strings.NewReader(`{"title":"Gateway upgrade","scope":"GATEWAY","symbols":["EUR/USD"],
			"starts_at":"2099-01-01T00:00:00Z","ends_at":"2099-01-01T02:00:00Z"}`))
	rec = httptest.NewRecorder()
	create(rec, adminCtx(req))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", rec.Code, rec.Body.String())
	}
	m := decode(t, rec)
	if m["scope"] != "GATEWAY" || m["status"] != "SCHEDULED" || m["created_by"] != "ops-1" {
		t.Fatalf("created=%v", m)
	}

	// public schedule shows it.
	rec = doReq(pub, "GET", "/api/v1/maintenance/schedule", nil, "")
	if rec.Code != http.StatusOK || decode(t, rec)["count"].(float64) != 1 {
		t.Fatalf("schedule=%s", rec.Body.String())
	}
	// admin list.
	rec = doReq(listAll, "GET", "/api/v1/admin/maintenance-windows", nil, "")
	if decode(t, rec)["count"].(float64) != 1 {
		t.Fatal("admin list empty")
	}

	// patch to CANCELLED → drops off public schedule.
	req = httptest.NewRequest("PATCH", "/api/v1/admin/maintenance-windows/1",
		strings.NewReader(`{"status":"CANCELLED"}`))
	req.SetPathValue("id", "1")
	rec = httptest.NewRecorder()
	patch(rec, adminCtx(req))
	if rec.Code != http.StatusOK {
		t.Fatalf("patch status=%d", rec.Code)
	}
	rec = doReq(pub, "GET", "/api/v1/maintenance/schedule", nil, "")
	if decode(t, rec)["count"].(float64) != 0 {
		t.Fatal("cancelled window still on public schedule")
	}
	// delete → cancel transition; unknown → 404.
	req = httptest.NewRequest("DELETE", "/api/v1/admin/maintenance-windows/77", nil)
	req.SetPathValue("id", "77")
	rec = httptest.NewRecorder()
	cancel(rec, adminCtx(req))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("cancel unknown status=%d", rec.Code)
	}
}

func itoa64(n int64) string { return strconv.FormatInt(n, 10) }

// ---------------------------------------------------------------------------
// Task-7 shadowed-route adapters — market/depth, market-data/snapshot,
// market/open-interest, instruments/{symbol}/swap-rates
// ---------------------------------------------------------------------------

func TestMarketDepthQuerySpelling(t *testing.T) {
	d := marketDeps()
	// ?symbol= + ?limit= map onto the Book.Snapshot seam.
	rec := doReq(MarketDepth(d), "GET", "/api/v1/market/depth", nil,
		"?symbol=EUR/USD&limit=20")
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	m := decode(t, rec)
	if m["seq"].(float64) != 42 {
		t.Fatalf("book=%v", m)
	}
	// missing symbol → 400, out-of-range limit → 400, unknown symbol → 404.
	if rec := doReq(MarketDepth(d), "GET", "/api/v1/market/depth", nil, ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("no symbol: status=%d", rec.Code)
	}
	if rec := doReq(MarketDepth(d), "GET", "/api/v1/market/depth", nil,
		"?symbol=EUR/USD&limit=0"); rec.Code != http.StatusBadRequest {
		t.Fatalf("limit 0: status=%d", rec.Code)
	}
	d.Book = &fakeMarketStore{}
	if rec := doReq(MarketDepth(d), "GET", "/api/v1/market/depth", nil,
		"?symbol=ZZZ/AAA"); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown symbol: status=%d", rec.Code)
	}
}

func TestMarketOpenInterestQuerySpelling(t *testing.T) {
	deps := &MarketStatsDeps{
		Instruments: &fakeMarketStore{instruments: []marketapi.Instrument{{Symbol: "EUR/USD"}}},
		OI: &fakeOIAnalytics{
			found: true,
			sample: marketdata.OISample{
				Symbol: "EUR/USD", OpenInterest: decimal.NewFromInt(4200),
				Notional: decimal.NewFromInt(4557), Positions: 9, AsOf: time.Now(),
			},
		},
		ResolveTier: pubTier,
	}
	rec := doReq(MarketOpenInterest(deps), "GET", "/api/v1/market/open-interest", nil,
		"?symbol=EUR/USD")
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec := doReq(MarketOpenInterest(deps), "GET", "/api/v1/market/open-interest", nil, ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("no symbol: status=%d", rec.Code)
	}
}

func TestInstrumentSwapRatesPathSpelling(t *testing.T) {
	// Path-param spelling feeds the history handler's ?symbol= contract —
	// assert the source sees the path-derived symbol.
	src := &fakeSwapSource{rows: swapJournalFixture()}
	d := swapDeps(src)
	rec := doReq(InstrumentSwapRates(d), "GET", "/api/v1/instruments/EUR/USD/swap-rates",
		map[string]string{"symbol": "EUR/USD"}, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if src.got.Symbol != "EUR/USD" {
		t.Fatalf("path symbol not forwarded: got %q", src.got.Symbol)
	}
	if rec := doReq(InstrumentSwapRates(d), "GET", "/api/v1/instruments//swap-rates",
		map[string]string{"symbol": ""}, ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("empty symbol: status=%d", rec.Code)
	}
}
