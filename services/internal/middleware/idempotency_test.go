package middleware

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeIdemStore is an in-memory IdempotencyStore for middleware tests.
type fakeIdemStore struct {
	mu      sync.Mutex
	entries map[string]*struct {
		ph     string
		done   bool
		status int
		body   []byte
		ct     string
	}
	beginErr error
}

func newFakeIdemStore() *fakeIdemStore {
	return &fakeIdemStore{entries: map[string]*struct {
		ph     string
		done   bool
		status int
		body   []byte
		ct     string
	}{}}
}

func (s *fakeIdemStore) Begin(_ context.Context, key, _ string, ph string, _ time.Duration) (IdemResult, error) {
	if s.beginErr != nil {
		return IdemResult{}, s.beginErr
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[key]
	if !ok {
		s.entries[key] = &struct {
			ph     string
			done   bool
			status int
			body   []byte
			ct     string
		}{ph: ph}
		return IdemResult{State: IdemProceed}, nil
	}
	if e.ph != ph {
		return IdemResult{State: IdemMismatch}, nil
	}
	if e.done {
		return IdemResult{State: IdemReplay, HTTPStatus: e.status,
			Body: e.body, ContentType: e.ct}, nil
	}
	return IdemResult{State: IdemInFlight}, nil
}

func (s *fakeIdemStore) Complete(_ context.Context, key, ph string, status int, ct string, body []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[key]
	if !ok || e.ph != ph {
		return errors.New("claim missing or drift")
	}
	if status == 0 {
		delete(s.entries, key)
		return nil
	}
	e.done, e.status, e.body, e.ct = true, status, body, ct
	return nil
}

// emitCode records the §23 code and writes a minimal envelope.
func emitCode(statusFor map[string]int) ErrorEmitter {
	return func(w http.ResponseWriter, _ *http.Request, code, msg string, _ map[string]any) {
		status := statusFor[code]
		if status == 0 {
			status = http.StatusInternalServerError
		}
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"error":"` + code + `"}`))
	}
}

var emitStatuses = map[string]int{
	"UNAUTHORIZED":              401,
	"INVALID_REQUEST":           400,
	"IDEMPOTENCY_KEY_MISMATCH":  422,
	"IDEMPOTENCY_KEY_COLLISION": 409,
	"SERVICE_DEGRADED":          503,
}

const uuidv7 = "018f8b7e-8f6a-7b2c-9d1e-2f3a4b5c6d7e"

func acct42(_ context.Context, _ *http.Request) (int64, bool) { return 42, true }

func newIdemChain(store IdempotencyStore, resolve AccountResolver, next http.HandlerFunc) http.Handler {
	return Idempotency(store, nil, resolve, emitCode(emitStatuses))(next)
}

func TestIsUUIDv7(t *testing.T) {
	if !IsUUIDv7(uuidv7) {
		t.Fatal("valid v7 rejected")
	}
	for _, bad := range []string{
		"", "x", "018f8b7e-8f6a-4b2c-9d1e-2f3a4b5c6d7e", // v4 nibble
		"018f8b7e-8f6a-7b2c-cd1e-2f3a4b5c6d7e", // bad variant
		"018f8b7e8f6a7b2c9d1e2f3a4b5c6d7e",     // no dashes
		"018f8b7e-8f6a-7b2c-9d1e-2f3a4b5c6d7",  // too short
	} {
		if IsUUIDv7(bad) {
			t.Fatalf("bad UUID accepted: %q", bad)
		}
	}
}

func TestIdemUnprotectedRoutePasses(t *testing.T) {
	h := newIdemChain(newFakeIdemStore(), acct42,
		func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })
	r := httptest.NewRequest("GET", "/api/v1/instruments", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("unprotected route: %d", w.Code)
	}
}

func TestIdemRequiresAuthHeaderKeyAndUUIDv7(t *testing.T) {
	next := func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) }

	// No account → UNAUTHORIZED.
	h := newIdemChain(newFakeIdemStore(),
		func(context.Context, *http.Request) (int64, bool) { return 0, false }, next)
	r := httptest.NewRequest("POST", "/api/v1/withdrawals", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 401 {
		t.Fatalf("unauth: %d %s", w.Code, w.Body)
	}

	h = newIdemChain(newFakeIdemStore(), acct42, next)
	// Missing header.
	r = httptest.NewRequest("POST", "/api/v1/withdrawals", nil)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 400 || !strings.Contains(w.Body.String(), "INVALID_REQUEST") {
		t.Fatalf("missing key: %d %s", w.Code, w.Body)
	}
	// Non-v7 key.
	r = httptest.NewRequest("POST", "/api/v1/withdrawals", nil)
	r.Header.Set(IdemKeyHeader, "018f8b7e-8f6a-4b2c-9d1e-2f3a4b5c6d7e")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 400 {
		t.Fatalf("v4 key: %d", w.Code)
	}
}

func TestIdemReplayAndMismatch(t *testing.T) {
	store := newFakeIdemStore()
	var calls int
	next := func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(201)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}
	h := newIdemChain(store, acct42, next)

	do := func(body, key string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "/api/v1/withdrawals",
			strings.NewReader(body))
		r.Header.Set(IdemKeyHeader, key)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}

	w1 := do(`{"amount":10}`, uuidv7)
	if w1.Code != 201 || calls != 1 {
		t.Fatalf("first exec: %d calls=%d", w1.Code, calls)
	}
	// Same key + same payload → replayed verbatim, handler not re-run.
	w2 := do(`{"amount":10}`, uuidv7)
	if w2.Code != 201 || calls != 1 {
		t.Fatalf("replay: %d calls=%d", w2.Code, calls)
	}
	if w2.Header().Get(IdemReplayedHeader) != "true" {
		t.Fatal("missing Idempotency-Replayed header")
	}
	if w2.Body.String() != `{"ok":true}` {
		t.Fatalf("replay body: %s", w2.Body)
	}
	// Same key + different payload → 422.
	w3 := do(`{"amount":99}`, uuidv7)
	if w3.Code != 422 || calls != 1 {
		t.Fatalf("mismatch: %d calls=%d", w3.Code, calls)
	}
	// Same key, different account → independent ledger slot.
	h2 := newIdemChain(store,
		func(context.Context, *http.Request) (int64, bool) { return 77, true }, next)
	r := httptest.NewRequest("POST", "/api/v1/withdrawals", strings.NewReader(`{"amount":10}`))
	r.Header.Set(IdemKeyHeader, uuidv7)
	w4 := httptest.NewRecorder()
	h2.ServeHTTP(w4, r)
	if w4.Code != 201 || calls != 2 {
		t.Fatalf("cross-account isolation: %d calls=%d", w4.Code, calls)
	}
}

func TestIdemInFlightCollision(t *testing.T) {
	store := newFakeIdemStore()
	// Simulate a pending claim by seeding via a first Begin.
	key := IdemKey(42, uuidv7)
	if _, err := store.Begin(context.Background(), key, "POST /api/v1/withdrawals",
		"deadbeef", time.Hour); err != nil {
		t.Fatal(err)
	}
	// Pending claims with the same payload-hash collide 409 — but the
	// middleware hashes the real body, so craft the body to match: we
	// cannot precompute the middleware hash, so instead verify the
	// collision via a store that returns InFlight for the computed hash.
	h := newIdemChain(&inFlightStore{}, acct42,
		func(w http.ResponseWriter, _ *http.Request) { t.Fatal("ran"); w.WriteHeader(200) })
	r := httptest.NewRequest("POST", "/api/v1/withdrawals", strings.NewReader(`{"a":1}`))
	r.Header.Set(IdemKeyHeader, uuidv7)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 409 {
		t.Fatalf("collision: %d %s", w.Code, w.Body)
	}
}

// inFlightStore always reports a pending same-hash claim.
type inFlightStore struct{}

func (s *inFlightStore) Begin(context.Context, string, string, string, time.Duration) (IdemResult, error) {
	return IdemResult{State: IdemInFlight}, nil
}
func (s *inFlightStore) Complete(context.Context, string, string, int, string, []byte) error {
	return nil
}

func TestIdem5xxReleasesClaim(t *testing.T) {
	store := newFakeIdemStore()
	var calls int
	next := func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.WriteHeader(http.StatusBadGateway) // transient
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":1}`))
	}
	h := newIdemChain(store, acct42, next)

	do := func() *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "/api/v1/withdrawals",
			strings.NewReader(`{"a":1}`))
		r.Header.Set(IdemKeyHeader, uuidv7)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	if w := do(); w.Code != 502 {
		t.Fatalf("first: %d", w.Code)
	}
	// 5xx released the claim → retry re-executes instead of replaying.
	if w := do(); w.Code != 200 || calls != 2 {
		t.Fatalf("retry after 5xx: %d calls=%d", w.Code, calls)
	}
	// Now the completed 200 response replays.
	if w := do(); w.Code != 200 || calls != 2 ||
		w.Header().Get(IdemReplayedHeader) != "true" {
		t.Fatalf("post-5xx replay: %d calls=%d", w.Code, calls)
	}
}

func TestIdemStoreFailureFailsClosed(t *testing.T) {
	store := newFakeIdemStore()
	store.beginErr = errors.New("redis down")
	h := newIdemChain(store, acct42,
		func(w http.ResponseWriter, _ *http.Request) { t.Fatal("must not run") })
	r := httptest.NewRequest("POST", "/api/v1/withdrawals", nil)
	r.Header.Set(IdemKeyHeader, uuidv7)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 503 {
		t.Fatalf("store down: %d", w.Code)
	}
}
