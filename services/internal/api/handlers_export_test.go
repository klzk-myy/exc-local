// Handler tests for Phase-23 Task 23.3.2 — the export REST surface.
// Sources/jobs/objects are in-memory fakes; auth claims are injected
// directly (middleware coverage lives in authn_integration_test.go).
package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"exchange/internal/auth"
	"exchange/internal/marketapi"
	"exchange/internal/marketdata"
	"exchange/internal/objectstore"
	"exchange/pkg/decimal"
)

// exportTape is the marketdata.TapeQuerier fake — returns canned rows.
type exportTape struct {
	rows []marketdata.TradesRow
	got  []marketdata.TapeQuery
}

func (f *exportTape) QueryTape(_ context.Context, q marketdata.TapeQuery) ([]marketdata.TradesRow, error) {
	f.got = append(f.got, q)
	var out []marketdata.TradesRow
	for _, r := range f.rows {
		if len(out) >= q.Limit {
			break
		}
		out = append(out, r)
	}
	return out, nil
}

var exportBase = time.Date(2026, 1, 2, 15, 4, 5, 0, time.UTC)

func exportTapeRows(n int) []marketdata.TradesRow {
	out := make([]marketdata.TradesRow, n)
	for i := 0; i < n; i++ {
		out[i] = marketdata.TradesRow{
			TradeID: uint64(1000 - i),
			Ts:      exportBase.Add(-time.Duration(i) * time.Second),
			Symbol:  "EURUSD", Side: "BUY",
			Price: decimal.MustFromString("1.0855"),
			Qty:   decimal.MustFromString("10"),
		}
	}
	return out
}

// exportDeps wires a fully-faked export surface for account 7.
func exportDeps(tape *exportTape) (*ExportDeps, *marketdata.MemJobStore) {
	jobs := marketdata.NewMemJobStore()
	return &ExportDeps{
		Exporter: &marketdata.ExportService{
			Trades: tape, Ticks: tape,
			Klines:     &exportKlines{},
			IntervalOK: func(iv string) bool { return iv == "1m" },
			Jobs:       jobs,
			Objects:    &exportObjects{objs: map[string][]byte{}},
		},
		Instruments: &fakeMarketStore{
			instruments: []marketapi.Instrument{{Symbol: "EURUSD"}},
		},
	}, jobs
}

type exportKlines struct{}

func (exportKlines) QueryKlines(context.Context, marketdata.KlineQuery) ([]marketdata.KlineRow, error) {
	return nil, nil
}

// exportObjects is the in-memory ObjectStore seam.
type exportObjects struct {
	objs map[string][]byte
}

func (f *exportObjects) Put(_ context.Context, in objectstore.PutInput) (objectstore.Object, error) {
	body, err := io.ReadAll(in.Body)
	if err != nil {
		return objectstore.Object{}, err
	}
	f.objs[in.Key] = body
	return objectstore.Object{Key: in.Key, Size: int64(len(body))}, nil
}

func (f *exportObjects) Get(_ context.Context, key string) ([]byte, objectstore.Object, error) {
	body, ok := f.objs[key]
	if !ok {
		return nil, objectstore.Object{}, errors.New("missing key")
	}
	return body, objectstore.Object{Key: key, Size: int64(len(body))}, nil
}

func (f *exportObjects) Delete(_ context.Context, key string) error {
	delete(f.objs, key)
	return nil
}

func exportReq(path, symbol string, accountID int64) *http.Request {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if symbol != "" {
		req.SetPathValue("symbol", symbol)
	}
	return req.WithContext(auth.WithClaims(req.Context(),
		auth.Claims{Subject: "u7", AccountID: accountID, Scopes: []string{"read"}}))
}

func decodeExportErr(t *testing.T, rec *httptest.ResponseRecorder, want int) string {
	t.Helper()
	if rec.Code != want {
		t.Fatalf("status = %d, want %d — body %s", rec.Code, want, rec.Body.String())
	}
	var env struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("error envelope parse: %v — %s", err, rec.Body.String())
	}
	return env.Error
}

func TestExportSyncCSV(t *testing.T) {
	tape := &exportTape{rows: exportTapeRows(2)}
	deps, _ := exportDeps(tape)
	rec := httptest.NewRecorder()
	HistoryTradesExport(deps).ServeHTTP(rec,
		exportReq("/api/v1/history/trades/EURUSD/export?format=csv", "EURUSD", 7))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d — %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/csv" {
		t.Fatalf("content-type %q", ct)
	}
	if rec.Header().Get("X-Export-Rows") != "2" {
		t.Fatalf("X-Export-Rows %q", rec.Header().Get("X-Export-Rows"))
	}
	first := strings.SplitN(rec.Body.String(), "\n", 2)[0]
	if first != "trade_id,time,symbol,side,price,quantity" {
		t.Fatalf("header %q", first)
	}
	if cd := rec.Header().Get("Content-Disposition"); !strings.Contains(cd, "trades_EURUSD") {
		t.Fatalf("content-disposition %q", cd)
	}
}

func TestExportSyncJSON(t *testing.T) {
	tape := &exportTape{rows: exportTapeRows(1)}
	deps, _ := exportDeps(tape)
	rec := httptest.NewRecorder()
	HistoryTradesExport(deps).ServeHTTP(rec,
		exportReq("/api/v1/history/trades/EURUSD/export?format=json", "EURUSD", 7))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d — %s", rec.Code, rec.Body.String())
	}
	var env struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("body %q: %v", rec.Body.String(), err)
	}
	if len(env.Data) != 1 || env.Data[0]["price"] != "1.08550000" {
		t.Fatalf("data %+v", env.Data)
	}
}

func TestExportAsyncFlag(t *testing.T) {
	tape := &exportTape{rows: exportTapeRows(1)}
	deps, jobs := exportDeps(tape)
	rec := httptest.NewRecorder()
	HistoryTradesExport(deps).ServeHTTP(rec,
		exportReq("/api/v1/history/trades/EURUSD/export?format=csv&async=1", "EURUSD", 7))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status %d — %s", rec.Code, rec.Body.String())
	}
	var env struct {
		Job struct {
			ID     int64  `json:"id"`
			Status string `json:"status"`
		} `json:"job"`
		StatusURL string `json:"status_url"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("envelope: %v", err)
	}
	if env.Job.ID == 0 || env.Job.Status != "PENDING" {
		t.Fatalf("job %+v", env.Job)
	}
	if env.StatusURL != "/api/v1/export-jobs/1" {
		t.Fatalf("status_url %q", env.StatusURL)
	}
	got, _ := jobs.Get(context.Background(), env.Job.ID)
	if got == nil || got.AccountID != 7 || got.RowLimit != marketdata.ExportMaxRows {
		t.Fatalf("persisted job %+v", got)
	}
}

func TestExportAsyncByBound(t *testing.T) {
	// limit over the 50k inline ceiling enqueues even without ?async=1.
	tape := &exportTape{rows: exportTapeRows(1)}
	deps, _ := exportDeps(tape)
	rec := httptest.NewRecorder()
	HistoryTradesExport(deps).ServeHTTP(rec,
		exportReq("/api/v1/history/trades/EURUSD/export?format=parquet&limit=50001", "EURUSD", 7))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status %d — %s", rec.Code, rec.Body.String())
	}
}

func TestExportLimitBoundary(t *testing.T) {
	tape := &exportTape{rows: exportTapeRows(1)}
	deps, _ := exportDeps(tape)
	// 1,000,000 — the cap itself is admitted (async).
	rec := httptest.NewRecorder()
	HistoryTradesExport(deps).ServeHTTP(rec,
		exportReq("/api/v1/history/trades/EURUSD/export?format=csv&limit=1000000", "EURUSD", 7))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("limit=1,000,000 status %d — %s", rec.Code, rec.Body.String())
	}
	// 1,000,001 — one row over the cap is EXPORT_LIMIT_EXCEEDED.
	rec = httptest.NewRecorder()
	HistoryTradesExport(deps).ServeHTTP(rec,
		exportReq("/api/v1/history/trades/EURUSD/export?format=csv&limit=1000001", "EURUSD", 7))
	if code := decodeExportErr(t, rec, http.StatusBadRequest); code != "EXPORT_LIMIT_EXCEEDED" {
		t.Fatalf("code %q", code)
	}
}

func TestExportFormatInvalid(t *testing.T) {
	tape := &exportTape{rows: exportTapeRows(1)}
	deps, _ := exportDeps(tape)
	rec := httptest.NewRecorder()
	HistoryTradesExport(deps).ServeHTTP(rec,
		exportReq("/api/v1/history/trades/EURUSD/export?format=xlsx", "EURUSD", 7))
	if code := decodeExportErr(t, rec, http.StatusBadRequest); code != "EXPORT_FORMAT_INVALID" {
		t.Fatalf("code %q", code)
	}
}

func TestExportUnknownSymbol(t *testing.T) {
	tape := &exportTape{rows: exportTapeRows(1)}
	deps, _ := exportDeps(tape)
	rec := httptest.NewRecorder()
	HistoryTradesExport(deps).ServeHTTP(rec,
		exportReq("/api/v1/history/trades/NOPE/export?format=csv", "NOPE", 7))
	if code := decodeExportErr(t, rec, http.StatusNotFound); code != "NOT_FOUND" {
		t.Fatalf("code %q", code)
	}
}

func TestExportRequiresAuth(t *testing.T) {
	deps, _ := exportDeps(&exportTape{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet,
		"/api/v1/history/trades/EURUSD/export?format=csv", nil)
	req.SetPathValue("symbol", "EURUSD")
	HistoryTradesExport(deps).ServeHTTP(rec, req)
	if code := decodeExportErr(t, rec, http.StatusUnauthorized); code != "UNAUTHORIZED" {
		t.Fatalf("code %q", code)
	}
}

func TestExportJobStatusOwnerScoped(t *testing.T) {
	tape := &exportTape{rows: exportTapeRows(1)}
	deps, jobs := exportDeps(tape)
	id, err := jobs.Insert(context.Background(), marketdata.Job{
		AccountID: 7, Kind: "trades", Symbol: "EURUSD", Format: "csv",
		Status: marketdata.JobPending, CreatedAt: exportBase,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Owner sees the job.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/export-jobs/"+itoa(id), nil)
	req.SetPathValue("id", itoa(id))
	req = req.WithContext(auth.WithClaims(req.Context(),
		auth.Claims{AccountID: 7, Scopes: []string{"read"}}))
	ExportJobStatus(deps).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("owner status %d — %s", rec.Code, rec.Body.String())
	}
	var doc struct {
		ID     int64  `json:"id"`
		Status string `json:"status"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil || doc.ID != id {
		t.Fatalf("doc %v", err)
	}
	// A different account gets the same 404 as a miss — the endpoint
	// never confirms the job exists to a non-owner.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/v1/export-jobs/"+itoa(id), nil)
	req.SetPathValue("id", itoa(id))
	req = req.WithContext(auth.WithClaims(req.Context(),
		auth.Claims{AccountID: 8, Scopes: []string{"read"}}))
	ExportJobStatus(deps).ServeHTTP(rec, req)
	if code := decodeExportErr(t, rec, http.StatusNotFound); code != "EXPORT_JOB_NOT_FOUND" {
		t.Fatalf("cross-account code %q", code)
	}
	// Unknown id — same 404.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/v1/export-jobs/4242", nil)
	req.SetPathValue("id", "4242")
	req = req.WithContext(auth.WithClaims(req.Context(),
		auth.Claims{AccountID: 7, Scopes: []string{"read"}}))
	ExportJobStatus(deps).ServeHTTP(rec, req)
	if code := decodeExportErr(t, rec, http.StatusNotFound); code != "EXPORT_JOB_NOT_FOUND" {
		t.Fatalf("unknown id code %q", code)
	}
}

func TestExportJobListOwnOnly(t *testing.T) {
	tape := &exportTape{rows: exportTapeRows(1)}
	deps, jobs := exportDeps(tape)
	for i := 0; i < 3; i++ {
		if _, err := jobs.Insert(context.Background(), marketdata.Job{
			AccountID: 7, Kind: "trades", Symbol: "EURUSD", Format: "csv",
			Status:    marketdata.JobPending,
			CreatedAt: exportBase.Add(time.Duration(i) * time.Second),
		}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := jobs.Insert(context.Background(), marketdata.Job{
		AccountID: 8, Kind: "trades", Symbol: "EURUSD", Format: "csv",
		Status:    marketdata.JobPending,
		CreatedAt: exportBase.Add(99 * time.Second),
	}); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/export-jobs", nil)
	req = req.WithContext(auth.WithClaims(req.Context(),
		auth.Claims{AccountID: 7, Scopes: []string{"read"}}))
	ExportJobList(deps).ServeHTTP(rec, req)
	env := decodeList[exportJobDoc](t, rec, http.StatusOK)
	if len(env.Data) != 3 {
		t.Fatalf("data len %d — account 8's job must be invisible", len(env.Data))
	}
	if env.Data[0].ID != 3 || env.Data[2].ID != 1 {
		t.Fatalf("newest-first order wrong: %+v", env.Data)
	}
	// Keyset continuation returns the remainder, not repeats.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet,
		"/api/v1/export-jobs?limit=2", nil)
	req = req.WithContext(auth.WithClaims(req.Context(),
		auth.Claims{AccountID: 7, Scopes: []string{"read"}}))
	ExportJobList(deps).ServeHTTP(rec, req)
	env = decodeList[exportJobDoc](t, rec, http.StatusOK)
	if len(env.Data) != 2 || env.NextCursor == "" {
		t.Fatalf("page1 %+v cursor %q", env.Data, env.NextCursor)
	}
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet,
		"/api/v1/export-jobs?limit=2&cursor="+env.NextCursor, nil)
	req = req.WithContext(auth.WithClaims(req.Context(),
		auth.Claims{AccountID: 7, Scopes: []string{"read"}}))
	ExportJobList(deps).ServeHTTP(rec, req)
	env = decodeList[exportJobDoc](t, rec, http.StatusOK)
	if len(env.Data) != 1 || env.Data[0].ID != 1 {
		t.Fatalf("page2 %+v", env.Data)
	}
}

func itoa(v int64) string { return strconv.FormatInt(v, 10) }
