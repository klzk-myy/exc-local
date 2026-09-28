package gateway

import (
	"encoding/json"
	stderrors "errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"exchange/internal/errs"
	pkgerrors "exchange/pkg/errors"
)

func TestEnvelopeShape(t *testing.T) {
	r := NewRouter(http.NewServeMux(), errs.New())
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/x", nil)
	req.Header.Set(RequestIDHeader, "req-test-1")
	r.WriteError(rec, req, "INSUFFICIENT_BALANCE", "not enough", map[string]any{"available": 1})

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status %d, want 400", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != pkgerrors.ProblemMediaType {
		t.Fatalf("content-type %q, want %q", ct, pkgerrors.ProblemMediaType)
	}
	var env Envelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if env.Type != "error" || env.Error != "INSUFFICIENT_BALANCE" ||
		env.Message != "not enough" || env.Status != 400 ||
		env.RequestID != "req-test-1" || env.Timestamp == "" {
		t.Fatalf("envelope malformed: %+v", env)
	}
	if _, err := time.Parse("2006-01-02T15:04:05.000Z", env.Timestamp); err != nil {
		t.Fatalf("timestamp %q not RFC3339-ms", env.Timestamp)
	}
}

func TestRequestIDMiddleware(t *testing.T) {
	var seen string
	h := RequestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = RequestIDFrom(r.Context())
		w.WriteHeader(200)
	}))

	// Inbound id is honoured, echoed, and propagated to the request header.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set(RequestIDHeader, "client-supplied")
	h.ServeHTTP(rec, req)
	if seen != "client-supplied" || rec.Header().Get(RequestIDHeader) != "client-supplied" ||
		req.Header.Get(RequestIDHeader) != "client-supplied" {
		t.Fatalf("request id propagation failed: seen=%q resp=%q req=%q",
			seen, rec.Header().Get(RequestIDHeader), req.Header.Get(RequestIDHeader))
	}

	// Absent → minted, echoed, ctx-visible.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest("GET", "/", nil)
	h.ServeHTTP(rec, req)
	if !strings.HasPrefix(seen, "req-") || len(seen) != 4+32 {
		t.Fatalf("generated request id %q", seen)
	}
	if rec.Header().Get(RequestIDHeader) != seen {
		t.Fatal("minted id not echoed on response header")
	}
}

func TestRequestIDResolution(t *testing.T) {
	r := NewRouter(http.NewServeMux(), errs.New())

	// Middleware-stamped response header wins.
	rec := httptest.NewRecorder()
	rec.Header().Set(RequestIDHeader, "mw-stamped")
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set(RequestIDHeader, "client-supplied")
	r.WriteError(rec, req, "NOT_FOUND", "x", nil)
	var env Envelope
	_ = json.Unmarshal(rec.Body.Bytes(), &env)
	if env.RequestID != "mw-stamped" {
		t.Fatalf("request id %q, want mw-stamped", env.RequestID)
	}

	// Absent everywhere → minted and stamped for correlation.
	rec = httptest.NewRecorder()
	r.WriteError(rec, httptest.NewRequest("GET", "/", nil), "NOT_FOUND", "x", nil)
	_ = json.Unmarshal(rec.Body.Bytes(), &env)
	if !strings.HasPrefix(env.RequestID, "req-") {
		t.Fatalf("minted request id %q", env.RequestID)
	}
	if rec.Header().Get(RequestIDHeader) != env.RequestID {
		t.Fatal("minted id not echoed on the response header")
	}
}

func TestWriteErrMapping(t *testing.T) {
	reg := errs.New()
	r := NewRouter(http.NewServeMux(), reg)

	// *pkgerrors.Error maps to its code + registered status.
	rec := httptest.NewRecorder()
	r.WriteErr(rec, httptest.NewRequest("GET", "/", nil),
		pkgerrors.New("STALE_MODIFY", "stale seq"))
	if rec.Code != http.StatusConflict {
		t.Fatalf("coded err status %d, want 409", rec.Code)
	}
	var env Envelope
	_ = json.Unmarshal(rec.Body.Bytes(), &env)
	if env.Error != "STALE_MODIFY" {
		t.Fatalf("coded err envelope %q", env.Error)
	}

	// Wrapped errors resolve through errors.As.
	rec = httptest.NewRecorder()
	r.WriteErr(rec, httptest.NewRequest("GET", "/", nil),
		pkgerrors.Wrap("ORDER_NOT_FOUND", "missing", stderrors.New("pg")))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("wrapped err status %d, want 404", rec.Code)
	}

	// Generic errors are fail-closed INTERNAL_ERROR 500, no detail leak.
	rec = httptest.NewRecorder()
	r.WriteErr(rec, httptest.NewRequest("GET", "/", nil),
		stderrors.New("secret internal detail"))
	if rec.Code != 500 {
		t.Fatalf("generic err status %d, want 500", rec.Code)
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &env)
	if env.Error != errs.CodeInternalError || strings.Contains(env.Message, "secret") {
		t.Fatalf("generic envelope leaked internals: %+v", env)
	}
}

func TestErrHandlerAdapter(t *testing.T) {
	r := NewRouter(http.NewServeMux(), errs.New())
	mux := r.Mux()
	mux.Handle("GET /ok", r.ErrHandler(func(w http.ResponseWriter, _ *http.Request) error {
		w.WriteHeader(http.StatusTeapot) // handler wrote success itself
		return nil
	}))
	mux.Handle("GET /fail", r.ErrHandler(func(http.ResponseWriter, *http.Request) error {
		return pkgerrors.New("MARKET_CLOSED", "outside window")
	}))

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/ok", nil))
	if rec.Code != http.StatusTeapot {
		t.Fatalf("nil-error path status %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/fail", nil))
	if rec.Code != http.StatusConflict {
		t.Fatalf("error path status %d, want 409", rec.Code)
	}
}

func TestRecoverMiddleware(t *testing.T) {
	r := NewRouter(http.NewServeMux(), errs.New())
	var logged string
	h := r.Recover(func(f string, a ...any) { logged = f }, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("boom")
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if rec.Code != 500 {
		t.Fatalf("panic response %d, want 500", rec.Code)
	}
	var env Envelope
	_ = json.Unmarshal(rec.Body.Bytes(), &env)
	if env.Error != errs.CodeInternalError {
		t.Fatalf("panic envelope code %q", env.Error)
	}
	if logged == "" {
		t.Fatal("panic was not reported to the log hook")
	}
}
