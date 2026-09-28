// Task 5.3.20 — deprecation policy unit tests (validation, matching,
// middleware, cache) plus the PG store test in
// deprecation_integration_test.go.
package deprecation

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func futureRule() Rule {
	return Rule{
		Path:         "/api/v1/legacy",
		AnnouncedAt:  time.Now().UTC(),
		SunsetAt:     time.Now().UTC().Add(MinNotice + time.Hour),
		MigrationURL: "/developer/migration",
		CreatedBy:    1,
	}
}

func TestValidate(t *testing.T) {
	s := &Store{now: time.Now} // pool unused by Validate
	if err := s.Validate(futureRule()); err != nil {
		t.Fatalf("valid rule rejected: %v", err)
	}
	bad := futureRule()
	bad.Path = "api/v1/legacy"
	if err := s.Validate(bad); err == nil {
		t.Fatal("relative path accepted")
	}
	m := "TRACE"
	bad = futureRule()
	bad.Method = &m
	if err := s.Validate(bad); err == nil {
		t.Fatal("unsupported method accepted")
	}
	m = "POST"
	r := futureRule()
	r.Method = &m
	if err := s.Validate(r); err != nil {
		t.Fatalf("valid method rejected: %v", err)
	}
	// Six-month floor: sunset before announced+MinNotice must fail.
	bad = futureRule()
	bad.SunsetAt = bad.AnnouncedAt.Add(MinNotice - time.Second)
	if err := s.Validate(bad); err == nil {
		t.Fatal("sunset inside six-month notice window accepted")
	}
	// Boundary: exactly MinNotice after announce is legal.
	ok := futureRule()
	ok.SunsetAt = ok.AnnouncedAt.Add(MinNotice)
	if err := s.Validate(ok); err != nil {
		t.Fatalf("boundary rule rejected: %v", err)
	}
}

// match scoring: exact method+path wins over NULL-method; longest prefix
// wins among prefix rules; sunset time is irrelevant to matching.
func TestMatch(t *testing.T) {
	now := time.Now()
	post := "POST"
	rules := []Rule{
		{Path: "/api/v1/legacy", MatchPrefix: true, SunsetAt: now.Add(time.Hour)},
		{Path: "/api/v1/legacy/sub", MatchPrefix: true, SunsetAt: now.Add(time.Hour)},
		{Path: "/api/v1/legacy/sub/exact", Method: &post, SunsetAt: now.Add(time.Hour)},
		{Path: "/api/v1/legacy/sub/exact", SunsetAt: now.Add(time.Hour)},
	}
	// Longest prefix wins.
	if r := match(rules, "GET", "/api/v1/legacy/sub/other", now); r == nil || r.Path != "/api/v1/legacy/sub" {
		t.Fatalf("prefix match=%+v", r)
	}
	// Exact method+path beats NULL-method exact.
	if r := match(rules, "POST", "/api/v1/legacy/sub/exact", now); r == nil || r.Method == nil {
		t.Fatalf("method-scoped match=%+v", r)
	}
	// NULL-method exact still matches a different method.
	if r := match(rules, "GET", "/api/v1/legacy/sub/exact", now); r == nil || r.Path != "/api/v1/legacy/sub/exact" || r.Method != nil {
		t.Fatalf("any-method match=%+v", r)
	}
	// No match → nil.
	if r := match(rules, "GET", "/api/v1/orders", now); r != nil {
		t.Fatalf("unexpected match: %+v", r)
	}
	// Method-scoped rule doesn't catch a different method on its path —
	// the any-method rule is what applies.
	if r := match(rules, "DELETE", "/api/v1/legacy/sub/exact", now); r == nil || r.Method != nil {
		t.Fatalf("delete should hit NULL-method rule: %+v", r)
	}
}

type staticRules struct {
	rules []Rule
	err   error
	calls int
}

func (s *staticRules) Active(context.Context) ([]Rule, error) {
	s.calls++
	return s.rules, s.err
}

func next200(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

func writeProblem(w http.ResponseWriter, _ *http.Request, code int, errCode, msg string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(code)
	_, _ = w.Write([]byte(`{"type":"error","error":"` + errCode + `","message":"` + msg + `"}`))
}

func TestMiddlewareAnnounced(t *testing.T) {
	rule := futureRule()
	rule.Replacement = strptr("/api/v1/orders")
	src := &staticRules{rules: []Rule{rule}}
	h := Middleware(src, writeProblem, nil)(http.HandlerFunc(next200))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/v1/legacy", nil))
	if rec.Code != 200 {
		t.Fatalf("announced endpoint should still serve: %d", rec.Code)
	}
	if rec.Header().Get("Deprecation") == "" {
		t.Fatal("missing Deprecation header")
	}
	if rec.Header().Get("Sunset") == "" {
		t.Fatal("missing Sunset header")
	}
	link := rec.Header().Get("Link")
	if link == "" || !strings.Contains(link, "deprecation") ||
		!strings.Contains(link, "/developer/migration") {
		t.Fatalf("missing migration Link header: %q", link)
	}
	if rec.Body.String() != "ok" {
		t.Fatalf("body=%q", rec.Body.String())
	}
}

func TestMiddlewareGone(t *testing.T) {
	rule := futureRule()
	rule.AnnouncedAt = time.Now().Add(-MinNotice - time.Hour)
	rule.SunsetAt = time.Now().Add(-time.Minute) // past sunset
	src := &staticRules{rules: []Rule{rule}}
	h := Middleware(src, writeProblem, nil)(http.HandlerFunc(next200))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/v1/legacy", nil))
	if rec.Code != http.StatusGone {
		t.Fatalf("past-sunset status=%d want 410", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "ENDPOINT_GONE") {
		t.Fatalf("body=%q", rec.Body.String())
	}
}

func TestMiddlewarePassThrough(t *testing.T) {
	src := &staticRules{rules: []Rule{futureRule()}}
	h := Middleware(src, writeProblem, nil)(http.HandlerFunc(next200))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/v1/orders", nil))
	if rec.Code != 200 || rec.Header().Get("Deprecation") != "" {
		t.Fatalf("unrelated request touched: %d %v", rec.Code, rec.Header())
	}
	// A store failure degrades to pass-through, never a 5xx.
	bad := &staticRules{err: errors.New("db down")}
	h = Middleware(bad, writeProblem, nil)(http.HandlerFunc(next200))
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/v1/legacy", nil))
	if rec.Code != 200 {
		t.Fatalf("rules fetch failure should pass through: %d", rec.Code)
	}
}

func TestCachedRules(t *testing.T) {
	src := &staticRules{rules: []Rule{futureRule()}}
	c := CachedRules(src, time.Hour)
	if _, err := c.Active(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Active(context.Background()); err != nil {
		t.Fatal(err)
	}
	if src.calls != 1 {
		t.Fatalf("cache missed: calls=%d", src.calls)
	}
	// Stale set survives a refresh error (nanosecond TTL forces the
	// refresh path on the second call).
	src2 := &staticRules{rules: []Rule{futureRule()}}
	c2 := CachedRules(src2, time.Nanosecond)
	if _, err := c2.Active(context.Background()); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Nanosecond)
	src2.err = errors.New("blip")
	if _, err := c2.Active(context.Background()); err != nil {
		t.Fatalf("stale-set fallback failed: %v", err)
	}
	// With no cached set the error propagates.
	bad := &staticRules{err: errors.New("db down")}
	c3 := CachedRules(bad, time.Nanosecond)
	if _, err := c3.Active(context.Background()); err == nil {
		t.Fatal("first-load error swallowed")
	}
}

func TestStoreNilPool(t *testing.T) {
	if _, err := NewStore(nil); err == nil {
		t.Fatal("nil pool accepted")
	}
}

func strptr(s string) *string { return &s }
