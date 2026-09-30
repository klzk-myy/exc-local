// Phase-23 Task 23.3.7 — block-tape history handler tests. The read
// seam is faked; fakes + helpers (fakeHistoryKV, premiumTiers,
// fakeMarketStore) live in handlers_history_test.go.
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

// fakeTapeSource satisfies marketdata.BlockTapeReader over canned rows.
type fakeTapeSource struct {
	rows  []*marketdata.BlockTapeEntry
	err   error
	got   marketdata.BlockTapeQuery
	calls int
}

func (f *fakeTapeSource) Query(_ context.Context, q marketdata.BlockTapeQuery) ([]*marketdata.BlockTapeEntry, error) {
	f.calls++
	f.got = q
	return f.rows, f.err
}

var _ marketdata.BlockTapeReader = (*fakeTapeSource)(nil)

func tapeDeps(src *fakeTapeSource) *BlockTapeHistoryDeps {
	return &BlockTapeHistoryDeps{
		Tape:        src,
		Instruments: &fakeMarketStore{instruments: []marketapi.Instrument{{Symbol: "EUR/USD"}}},
		Tiers:       premiumTiers(),
	}
}

func tapeFixture() []*marketdata.BlockTapeEntry {
	exec := time.Date(2026, 10, 5, 14, 0, 0, 0, time.UTC)
	pub := exec.Add(15 * time.Minute)
	corr := pub.Add(20 * time.Minute)
	cp := decimal.MustFromString("1.0853")
	return []*marketdata.BlockTapeEntry{
		{EntryID: 2004, Kind: marketdata.BlockTapePrint,
			BlockTradeID: 1002, Symbol: "EUR/USD",
			Price: decimal.MustFromString("1.08520125"), Quantity: decimal.MustFromString("920000"),
			NotionalUSD: decimal.MustFromString("998231.15"),
			ExecTs:      exec, PubTs: pub, DelayMs: 900000,
			VenueFlags:  []string{"DELAYED"},
			CorrectedBy: 2005, Bust: true},
		{EntryID: 2005, Kind: marketdata.BlockTapeBust,
			BlockTradeID: 1002, Symbol: "EUR/USD",
			ExecTs: corr, PubTs: corr, OriginalTradeID: 77,
			CorrectedPrice: &cp},
	}
}

func TestHistoryBlockTrades(t *testing.T) {
	src := &fakeTapeSource{rows: tapeFixture()}
	rec := httptest.NewRecorder()
	HistoryBlockTrades(tapeDeps(src)).ServeHTTP(rec,
		histReq("/api/v1/history/block-trades/EUR%2FUSD", "EUR/USD"))
	var env struct {
		Data []struct {
			EntryID         uint64   `json:"entry_id"`
			Kind            string   `json:"kind"`
			BlockTradeID    uint64   `json:"block_trade_id"`
			Price           string   `json:"price"`
			NotionalUSD     string   `json:"notional_usd"`
			Bust            bool     `json:"bust"`
			CorrectedBy     uint64   `json:"corrected_by"`
			Supersedes      uint64   `json:"supersedes"`
			OriginalTradeID uint64   `json:"original_trade_id"`
			VenueFlags      []string `json:"venue_flags"`
		} `json:"data"`
		AccessTier string `json:"access_tier"`
	}
	if rec.Code != 200 {
		t.Fatalf("status = %d — %s", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v — %s", err, rec.Body.String())
	}
	if len(env.Data) != 2 || env.AccessTier != "premium" {
		t.Fatalf("data = %d rows tier %q", len(env.Data), env.AccessTier)
	}
	p, c := env.Data[0], env.Data[1]
	if p.Kind != "PRINT" || p.Price != "1.08520125" || !p.Bust || p.CorrectedBy != 2005 {
		t.Fatalf("print = %+v", p)
	}
	if c.Kind != "BUST" || c.Supersedes != 1002 || c.OriginalTradeID != 77 || !c.Bust {
		t.Fatalf("correction = %+v", c)
	}
	// Anonymity is structural: the wire body must carry NO participant
	// identifier keys — no side, no accounts, no order ids.
	body := rec.Body.String()
	for _, banned := range []string{"account_id", "order_id", "aggressor_side",
		"participant", "counterparty"} {
		if strings.Contains(body, banned) {
			t.Fatalf("body leaks %q: %s", banned, body)
		}
	}
}

func TestHistoryBlockTradesFreeTierDelay(t *testing.T) {
	// nil resolver → free, fail-closed. Guard clock pinned so the
	// 15-minute pub_ts delay horizon is deterministic.
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	src := &fakeTapeSource{}
	d := tapeDeps(src)
	d.Tiers = nil
	d.Guard.Now = func() time.Time { return now }
	rec := httptest.NewRecorder()
	HistoryBlockTrades(d).ServeHTTP(rec,
		histReq("/api/v1/history/block-trades/EUR%2FUSD", "EUR/USD"))
	if rec.Code != 200 {
		t.Fatalf("status = %d — %s", rec.Code, rec.Body.String())
	}
	wantPub := now.Add(-15 * time.Minute)
	if !src.got.PublishedBefore.Equal(wantPub) {
		t.Fatalf("PublishedBefore = %v, want %v", src.got.PublishedBefore, wantPub)
	}
	if !src.got.To.Equal(wantPub) {
		t.Fatalf("free To clamp = %v, want %v", src.got.To, wantPub)
	}
	wantFrom := now.AddDate(0, 0, -30)
	if !src.got.From.Equal(wantFrom) {
		t.Fatalf("free From clamp = %v, want %v", src.got.From, wantFrom)
	}
	if !strings.Contains(rec.Body.String(), `"access_tier":"free"`) ||
		!strings.Contains(rec.Body.String(), `"delayed":true`) {
		t.Fatalf("tier fields missing: %s", rec.Body.String())
	}
}

func TestHistoryBlockTradesPremiumRealtime(t *testing.T) {
	src := &fakeTapeSource{}
	d := tapeDeps(src) // premium tiers
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	d.Guard.Now = func() time.Time { return now }
	rec := httptest.NewRecorder()
	HistoryBlockTrades(d).ServeHTTP(rec,
		histReq("/api/v1/history/block-trades/EUR%2FUSD", "EUR/USD"))
	if rec.Code != 200 {
		t.Fatalf("status = %d — %s", rec.Code, rec.Body.String())
	}
	if !src.got.PublishedBefore.IsZero() || !src.got.To.IsZero() {
		t.Fatalf("premium must be unclamped: %+v", src.got)
	}
}

func TestHistoryBlockTradesCursorRoundTrip(t *testing.T) {
	src := &fakeTapeSource{rows: tapeFixture()}
	// Full page (rows == limit) → next_cursor emitted; the cursor is
	// the LAST row's (pub_ts, entry_id) keyset position.
	req := httptest.NewRequest(http.MethodGet,
		"/api/v1/history/block-trades/EUR%2FUSD?limit=2", nil)
	req.SetPathValue("symbol", "EUR/USD")
	rec := httptest.NewRecorder()
	HistoryBlockTrades(tapeDeps(src)).ServeHTTP(rec, req)
	env := decodeList[json.RawMessage](t, rec, 200)
	if env.NextCursor == "" {
		t.Fatal("no next_cursor")
	}
	req2 := httptest.NewRequest(http.MethodGet,
		"/api/v1/history/block-trades/EUR%2FUSD?limit=2&cursor="+env.NextCursor, nil)
	req2.SetPathValue("symbol", "EUR/USD")
	rec2 := httptest.NewRecorder()
	HistoryBlockTrades(tapeDeps(src)).ServeHTTP(rec2, req2)
	if rec2.Code != 200 {
		t.Fatalf("status = %d — %s", rec2.Code, rec2.Body.String())
	}
	last := tapeFixture()[1]
	if src.got.After == nil || src.got.After.EntryID != last.EntryID {
		t.Fatalf("cursor After = %+v", src.got.After)
	}
	if !src.got.After.Ts.Equal(last.PubTs) {
		t.Fatalf("cursor ts = %v", src.got.After.Ts)
	}
}

func TestHistoryBlockTradesCSV(t *testing.T) {
	src := &fakeTapeSource{rows: tapeFixture()}
	req := histReq("/api/v1/history/block-trades/EUR%2FUSD", "EUR/USD")
	req.Header.Set("Accept", "text/csv")
	rec := httptest.NewRecorder()
	HistoryBlockTrades(tapeDeps(src)).ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status = %d — %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/csv" {
		t.Fatalf("content-type = %q", ct)
	}
	lines := strings.Split(strings.TrimSpace(rec.Body.String()), "\n")
	if len(lines) != 3 || !strings.HasPrefix(lines[0], "entry_id,kind") ||
		!strings.Contains(lines[1], ",PRINT,") || !strings.Contains(lines[2], ",BUST,") {
		t.Fatalf("csv = %q", rec.Body.String())
	}
}

func TestHistoryBlockTradesTimeoutAndOutage(t *testing.T) {
	// Deadline: fake returns ctx error after the (tiny) guard timeout.
	src := &fakeTapeSource{err: context.DeadlineExceeded}
	d := tapeDeps(src)
	d.Guard.Timeout = time.Millisecond
	rec := httptest.NewRecorder()
	HistoryBlockTrades(d).ServeHTTP(rec,
		histReq("/api/v1/history/block-trades/EUR%2FUSD", "EUR/USD"))
	wantErrorStatus(t, rec, http.StatusGatewayTimeout)

	src2 := &fakeTapeSource{err: errors.New("ch down")}
	rec2 := httptest.NewRecorder()
	HistoryBlockTrades(tapeDeps(src2)).ServeHTTP(rec2,
		histReq("/api/v1/history/block-trades/EUR%2FUSD", "EUR/USD"))
	wantErrorStatus(t, rec2, http.StatusServiceUnavailable)
}

func TestHistoryBlockTradesUnknownSymbolAndNotConfigured(t *testing.T) {
	src := &fakeTapeSource{}
	d := tapeDeps(src)
	d.Instruments = &fakeMarketStore{} // empty → every symbol unknown
	rec := httptest.NewRecorder()
	HistoryBlockTrades(d).ServeHTTP(rec,
		histReq("/api/v1/history/block-trades/NOPE", "NOPE"))
	wantErrorStatus(t, rec, http.StatusNotFound)
	if src.calls != 0 {
		t.Fatal("unknown symbol must not reach the store")
	}
	rec2 := httptest.NewRecorder()
	HistoryBlockTrades(&BlockTapeHistoryDeps{}).ServeHTTP(rec2,
		histReq("/api/v1/history/block-trades/EUR%2FUSD", "EUR/USD"))
	wantErrorStatus(t, rec2, http.StatusServiceUnavailable)
}

func TestHistoryBlockTradesClosedIntervalCache(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	kv := newFakeHistoryKV()
	src := &fakeTapeSource{rows: tapeFixture()}
	d := tapeDeps(src)
	d.Cache = kv
	d.Guard.Now = func() time.Time { return now }
	// Closed interval: to = yesterday (≤ now−0 for premium).
	url := "/api/v1/history/block-trades/EUR%2FUSD?from=2026-10-05T00:00:00Z" +
		"&to=2026-10-05T23:00:00Z"
	req := httptest.NewRequest(http.MethodGet, url, nil)
	req.SetPathValue("symbol", "EUR/USD")
	rec := httptest.NewRecorder()
	HistoryBlockTrades(d).ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status = %d — %s", rec.Code, rec.Body.String())
	}
	if len(kv.m) != 1 {
		t.Fatalf("cached bodies = %d", len(kv.m))
	}
	// Second identical request → cache hit, no store call.
	src2 := &fakeTapeSource{}
	d2 := tapeDeps(src2)
	d2.Cache = kv
	d2.Guard.Now = func() time.Time { return now }
	req2 := httptest.NewRequest(http.MethodGet, url, nil)
	req2.SetPathValue("symbol", "EUR/USD")
	rec2 := httptest.NewRecorder()
	HistoryBlockTrades(d2).ServeHTTP(rec2, req2)
	if rec2.Code != 200 || rec2.Header().Get("X-History-Cache") != "hit" {
		t.Fatalf("cache hit: status %d hdr %q", rec2.Code,
			rec2.Header().Get("X-History-Cache"))
	}
	if src2.calls != 0 {
		t.Fatal("cache hit must skip the store")
	}
}
