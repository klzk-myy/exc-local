// Handler tests for Task 20.3.13 — GET /api/v1/account/snapshots.
// Source is an in-memory fake; PG coverage is EXC_PG_TEST=1 gated in
// internal/analytics/snapshots_test.go.
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
	"exchange/internal/auth"
	"exchange/pkg/decimal"
)

type fakeSnapshotSource struct {
	rows  []analytics.SnapshotRow
	total int64
	err   error
	got   struct {
		accountID int64
		date      *time.Time
		after     *analytics.SnapshotCursor
		limit     int
	}
}

func (f *fakeSnapshotSource) History(_ context.Context, accountID int64,
	date *time.Time, after *analytics.SnapshotCursor, limit int) ([]analytics.SnapshotRow, int64, error) {
	f.got.accountID, f.got.date, f.got.after, f.got.limit = accountID, date, after, limit
	return f.rows, f.total, f.err
}

var _ SnapshotHistorySource = (*fakeSnapshotSource)(nil)

func snapReq(t *testing.T, url string, accountID int64) (*httptest.ResponseRecorder, *http.Request) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, url, nil)
	if accountID != 0 {
		req = req.WithContext(auth.WithClaims(req.Context(),
			auth.Claims{Subject: "1", AccountID: accountID, Scopes: []string{"read"}}))
	}
	return httptest.NewRecorder(), req
}

func TestAccountSnapshotsRows(t *testing.T) {
	day := time.Date(2026, 9, 27, 0, 0, 0, 0, time.UTC)
	src := &fakeSnapshotSource{total: 2, rows: []analytics.SnapshotRow{
		{ID: 11, AccountID: 42, Currency: "USD",
			Available: decimal.RequireFromString("1000.5"),
			Locked:    decimal.RequireFromString("25.25"),
			PositionsJSON: `[{"position_id":7,"instrument_id":1,"symbol":"EUR/USD",` +
				`"side":"LONG","quantity":"1000.00000000","entry_price":"1.08501000",` +
				`"unrealized_pnl":"0.50000000","realized_pnl":"0.00000000","margin_used":"36.16666667"}]`,
			SnapshotDate: day, Hash: fakeHashA, PrevHash: "genesis",
			CreatedAt: day.Add(23*time.Hour + 59*time.Minute)},
		{ID: 10, AccountID: 42, Currency: "EUR",
			Available: decimal.Zero, Locked: decimal.Zero,
			PositionsJSON: "[]", SnapshotDate: day,
			Hash: "hh", PrevHash: "pp", CreatedAt: day.Add(23 * time.Hour)},
	}}
	rec, req := snapReq(t, "/api/v1/account/snapshots?date=2026-09-27&limit=2", 42)
	AccountSnapshots(src).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d body %s", rec.Code, rec.Body)
	}
	if src.got.accountID != 42 || src.got.date == nil ||
		src.got.date.Format("2006-01-02") != "2026-09-27" {
		t.Fatalf("query = %+v", src.got)
	}
	var env struct {
		Data []struct {
			SnapshotDate string          `json:"snapshot_date"`
			Currency     string          `json:"currency"`
			Available    string          `json:"available"`
			Locked       string          `json:"locked"`
			Total        string          `json:"total"`
			Positions    json.RawMessage `json:"positions"`
			Hash         string          `json:"hash"`
			PrevHash     string          `json:"prev_hash"`
		} `json:"data"`
		NextCursor string `json:"next_cursor"`
		Total      int64  `json:"total"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if env.Total != 2 || len(env.Data) != 2 {
		t.Fatalf("envelope = %+v", env)
	}
	row := env.Data[0]
	if row.Total != "1025.75000000" {
		t.Fatalf("total = %s, want available+locked", row.Total)
	}
	if row.SnapshotDate != "2026-09-27" || row.PrevHash != "genesis" {
		t.Fatalf("row = %+v", row)
	}
	// positions_json passes through as embedded JSON, not a string.
	var pos []map[string]any
	if err := json.Unmarshal(row.Positions, &pos); err != nil || len(pos) != 1 {
		t.Fatalf("positions not embedded JSON: %s err=%v", row.Positions, err)
	}
	if pos[0]["symbol"] != "EUR/USD" {
		t.Fatalf("position = %+v", pos[0])
	}
	if env.NextCursor == "" {
		t.Fatal("full page must emit next_cursor")
	}
}

const fakeHashA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func TestAccountSnapshotsHistoryNoDate(t *testing.T) {
	src := &fakeSnapshotSource{total: 0}
	rec, req := snapReq(t, "/api/v1/account/snapshots", 42)
	AccountSnapshots(src).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d body %s", rec.Code, rec.Body)
	}
	if src.got.date != nil {
		t.Fatalf("date must be nil without ?date=")
	}
	if src.got.limit != snapshotsListSpec.Default {
		t.Fatalf("limit = %d, want spec default %d", src.got.limit, snapshotsListSpec.Default)
	}
}

func TestAccountSnapshotsFailClosed(t *testing.T) {
	src := &fakeSnapshotSource{err: errors.New("pg: conn refused")}
	rec, req := snapReq(t, "/api/v1/account/snapshots", 42)
	AccountSnapshots(src).ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status %d, want 503", rec.Code)
	}
	// Nil store → 503.
	rec2, req2 := snapReq(t, "/api/v1/account/snapshots", 42)
	AccountSnapshots(nil).ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusServiceUnavailable {
		t.Fatalf("nil store → %d", rec2.Code)
	}
}

func TestAccountSnapshotsValidation(t *testing.T) {
	src := &fakeSnapshotSource{}
	cases := []struct {
		url  string
		want int
	}{
		{"/api/v1/account/snapshots?date=27-09-2026", http.StatusBadRequest},
		{"/api/v1/account/snapshots?date=2026-13-99", http.StatusBadRequest},
		{"/api/v1/account/snapshots?limit=99999", http.StatusBadRequest},
		{"/api/v1/account/snapshots?cursor=garbage", http.StatusBadRequest},
		{"/api/v1/account/snapshots?account_id=43", http.StatusForbidden},
	}
	for _, tc := range cases {
		rec, req := snapReq(t, tc.url, 42)
		AccountSnapshots(src).ServeHTTP(rec, req)
		if rec.Code != tc.want {
			t.Fatalf("%s → %d, want %d", tc.url, rec.Code, tc.want)
		}
	}
	rec, req := snapReq(t, "/api/v1/account/snapshots", 0)
	AccountSnapshots(src).ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauth → %d, want 401", rec.Code)
	}
}
