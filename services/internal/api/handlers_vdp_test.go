// Phase-13.5 Task 13.5.3.8 handler tests — fake service seam, no PG.
package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"exchange/internal/security"
)

type fakeVDP struct {
	submitCalls int
	lastReq     security.SubmitRequest
	out         *security.Disclosure
	err         error
}

func (f *fakeVDP) Submit(_ context.Context, req security.SubmitRequest) (*security.Disclosure, bool, error) {
	f.submitCalls++
	f.lastReq = req
	if f.err != nil {
		return nil, false, f.err
	}
	return f.out, true, nil
}

func (f *fakeVDP) IngestPentest(context.Context, int64, security.SubmitRequest, string) (*security.Disclosure, error) {
	return f.out, f.err
}
func (f *fakeVDP) Triage(context.Context, int64, security.TriageInput, string) (*security.Disclosure, error) {
	return f.out, f.err
}
func (f *fakeVDP) Update(context.Context, int64, security.AdminUpdate, string) (*security.Disclosure, error) {
	return f.out, f.err
}
func (f *fakeVDP) Get(context.Context, int64, int64) (*security.Disclosure, error) {
	return f.out, f.err
}

type vdpAfter = struct {
	Time time.Time
	ID   int64
}

func (f *fakeVDP) List(_ context.Context, _ int64, _ security.AdminFilter,
	_ *vdpAfter, _ int) ([]security.Disclosure, error) {
	return nil, f.err
}

func TestVDPSubmitHappyPath(t *testing.T) {
	f := &fakeVDP{out: &security.Disclosure{
		ID: 7, ReportID: "RPT-1", Status: security.StatusTriaged,
	}}
	h := VDPDisclosureSubmit(f)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/security/disclosures",
		strings.NewReader(`{"report_id":"RPT-1","title":"x","reproduction":"steps","affected_components":["REST"]}`))
	rec := httptest.NewRecorder()
	h(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
	}
	if f.submitCalls != 1 || f.lastReq.Source != security.SourceResearcher {
		t.Fatalf("service not called correctly: %+v", f.lastReq)
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out["report_id"] != "RPT-1" || out["duplicate"] != false {
		t.Fatalf("bad response: %v", out)
	}
}

func TestVDPSubmitHoneypot(t *testing.T) {
	f := &fakeVDP{}
	h := VDPDisclosureSubmit(f)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/security/disclosures",
		strings.NewReader(`{"title":"x","reproduction":"y","website":"http://spam.example"}`))
	rec := httptest.NewRecorder()
	h(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("honeypot must uniform-accept, got %d", rec.Code)
	}
	if f.submitCalls != 0 {
		t.Fatal("honeypot submission must never reach the register")
	}
}

func TestVDPSubmitBodyCap(t *testing.T) {
	f := &fakeVDP{}
	h := VDPDisclosureSubmit(f)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/security/disclosures",
		strings.NewReader(`{"title":"`+strings.Repeat("a", 100<<10)+`"}`))
	rec := httptest.NewRecorder()
	h(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("oversized body must 400, got %d", rec.Code)
	}
	if f.submitCalls != 0 {
		t.Fatal("oversized body must never reach the register")
	}
}

func TestVDPPolicyServing(t *testing.T) {
	doc := &VDPPolicyDoc{Body: []byte("# policy\nscope: all\n")}
	rec := httptest.NewRecorder()
	VDPPolicy(doc)(rec, httptest.NewRequest(http.MethodGet, "/api/v1/security/policy", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "policy") {
		t.Fatalf("policy serve failed: %d %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/markdown") {
		t.Fatalf("content-type %q", ct)
	}
	// Unloaded doc → SERVICE_DEGRADED (no fabricated policy).
	rec2 := httptest.NewRecorder()
	VDPPolicy(nil)(rec2, httptest.NewRequest(http.MethodGet, "/api/v1/security/policy", nil))
	if rec2.Code != http.StatusServiceUnavailable {
		t.Fatalf("missing doc must 503, got %d", rec2.Code)
	}
}
