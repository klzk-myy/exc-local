// Phase-12 KYC handler tests — auth gate, multipart submit contract,
// self-cert validation wiring. Service runs on an in-memory fake store +
// fake object putter (the wire/PG paths are covered in
// internal/compliance and internal/objectstore tests).
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"exchange/internal/auth"
	"exchange/internal/compliance"
	"exchange/internal/objectstore"
)

// ---------------------------------------------------------------------------
// Fakes (compliance.Store + compliance.ObjectPutter)
// ---------------------------------------------------------------------------

type fakeKYCStore struct {
	tier  string
	pols  map[string]*compliance.TierPolicy
	rows  []compliance.MatrixRow
	subs  []*compliance.Submission
	certs []compliance.SelfCert
	next  int64
}

func (f *fakeKYCStore) AccountTier(ctx context.Context, id int64) (string, error) {
	return f.tier, nil
}
func (f *fakeKYCStore) CreateSubmission(ctx context.Context, s *compliance.Submission) error {
	f.next++
	s.ID = f.next
	f.subs = append(f.subs, s)
	return nil
}
func (f *fakeKYCStore) AttachDocument(ctx context.Context, d *compliance.Document) error {
	f.next++
	d.ID = f.next
	return nil
}
func (f *fakeKYCStore) FailSubmission(ctx context.Context, id int64) error { return nil }
func (f *fakeKYCStore) LatestSubmission(ctx context.Context, id int64) (*compliance.Submission, error) {
	if len(f.subs) == 0 {
		return nil, nil
	}
	return f.subs[len(f.subs)-1], nil
}
func (f *fakeKYCStore) ListAccountDocuments(ctx context.Context, id int64) ([]compliance.Document, error) {
	return nil, nil
}
func (f *fakeKYCStore) TierPolicy(ctx context.Context, t string) (*compliance.TierPolicy, error) {
	return f.pols[t], nil
}
func (f *fakeKYCStore) Matrix(ctx context.Context, tier, jur string) ([]compliance.MatrixRow, error) {
	var out []compliance.MatrixRow
	for _, r := range f.rows {
		if r.Tier == tier && (r.Jurisdiction == "*" || r.Jurisdiction == jur) {
			out = append(out, r)
		}
	}
	return out, nil
}
func (f *fakeKYCStore) OverdueReviews(ctx context.Context, n time.Time, l int) ([]compliance.Submission, error) {
	return nil, nil
}
func (f *fakeKYCStore) InsertSelfCert(ctx context.Context, c *compliance.SelfCert) error {
	f.next++
	c.ID = f.next
	f.certs = append(f.certs, *c)
	return nil
}
func (f *fakeKYCStore) ListSelfCerts(ctx context.Context, id int64) ([]compliance.SelfCert, error) {
	return f.certs, nil
}
func (f *fakeKYCStore) HasSelfCert(ctx context.Context, id int64) (bool, error) {
	return len(f.certs) > 0, nil
}

type fakeObjects struct {
	last objectstore.PutInput
	err  error
}

func (f *fakeObjects) Put(ctx context.Context, in objectstore.PutInput) (objectstore.Object, error) {
	if f.err != nil {
		return objectstore.Object{}, f.err
	}
	f.last = in
	return objectstore.Object{Key: in.Key}, nil
}
func (f *fakeObjects) Delete(ctx context.Context, key string) error { return nil }
func (f *fakeObjects) Bucket() string                               { return "kyc-test" }

func kycTestSvc(store *fakeKYCStore, objects *fakeObjects) *compliance.Service {
	return compliance.NewService(store, objects, "kms-key-test", nil)
}

func kycClaimsReq(t *testing.T, method, path string, body *bytes.Buffer, contentType string) *http.Request {
	t.Helper()
	var r *http.Request
	if body != nil {
		r = httptest.NewRequest(method, path, body)
	} else {
		r = httptest.NewRequest(method, path, nil)
	}
	if contentType != "" {
		r.Header.Set("Content-Type", contentType)
	}
	return r.WithContext(auth.WithClaims(r.Context(),
		auth.Claims{Subject: "7", AccountID: 7, Scopes: []string{"read"}}))
}

func kycFixtureStore(tier string) *fakeKYCStore {
	return &fakeKYCStore{
		tier: tier,
		pols: map[string]*compliance.TierPolicy{
			compliance.TierT0: {Tier: compliance.TierT0, ManualReviewSLAHours: 24},
			compliance.TierT1: {Tier: compliance.TierT1, ManualReviewSLAHours: 24,
				RescreenCadence: "WEEKLY", LivenessRequired: true},
		},
		rows: []compliance.MatrixRow{
			{Tier: "T1", Jurisdiction: "*", Vendor: "MANUAL", DocumentType: "PASSPORT", DocGroup: "IDENTITY", Required: true},
			{Tier: "T1", Jurisdiction: "*", Vendor: "MANUAL", DocumentType: "UTILITY_BILL", DocGroup: "ADDRESS", Required: true},
			{Tier: "T1", Jurisdiction: "*", Vendor: "MANUAL", DocumentType: "LIVENESS_SELFIE", DocGroup: "LIVENESS", Required: true},
		},
	}
}

// multipartBody builds a doc_-prefixed multipart request body.
func multipartBody(t *testing.T, tier, jur string, files map[string][]byte) (*bytes.Buffer, string) {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	_ = w.WriteField("requested_tier", tier)
	if jur != "" {
		_ = w.WriteField("jurisdiction", jur)
	}
	for field, data := range files {
		fw, err := w.CreateFormFile(field, field+".bin")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fw.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return &buf, w.FormDataContentType()
}

var (
	pdfDoc  = []byte("%PDF-1.4 test\n")
	pngDoc  = append([]byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A}, []byte("x")...)
	jpegDoc = append([]byte{0xFF, 0xD8, 0xFF, 0xE0}, []byte("x")...)
)

// ---------------------------------------------------------------------------
// Auth + submit
// ---------------------------------------------------------------------------

func TestKYCSubmitRequiresAuth(t *testing.T) {
	svc := kycTestSvc(kycFixtureStore("T0"), &fakeObjects{})
	r := httptest.NewRequest(http.MethodPost, "/api/v1/kyc/submit", nil)
	rec := httptest.NewRecorder()
	KYCSubmit(svc).ServeHTTP(rec, r)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauth submit: %d", rec.Code)
	}
}

func TestKYCSubmitHappyPath(t *testing.T) {
	objects := &fakeObjects{}
	svc := kycTestSvc(kycFixtureStore("T0"), objects)
	body, ct := multipartBody(t, "T1", "US", map[string][]byte{
		"doc_PASSPORT":        jpegDoc,
		"doc_UTILITY_BILL":    pdfDoc,
		"doc_LIVENESS_SELFIE": pngDoc,
	})
	rec := httptest.NewRecorder()
	KYCSubmit(svc).ServeHTTP(rec, kycClaimsReq(t, http.MethodPost, "/api/v1/kyc/submit", body, ct))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("submit: %d body=%s", rec.Code, rec.Body)
	}
	if objects.last.ServerSideEncryption != "aws:kms" {
		t.Fatal("SSE-KMS must ride every document put (spec §24 #102)")
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	sub, _ := out["submission"].(map[string]any)
	if sub["status"] != "PENDING_REVIEW" || sub["requested_tier"] != "T1" {
		t.Fatalf("submission shape: %v", sub)
	}
}

func TestKYCSubmitValidationErrors(t *testing.T) {
	svc := kycTestSvc(kycFixtureStore("T0"), &fakeObjects{})
	// No doc parts → INVALID_REQUEST.
	body, ct := multipartBody(t, "T1", "", nil)
	rec := httptest.NewRecorder()
	KYCSubmit(svc).ServeHTTP(rec, kycClaimsReq(t, http.MethodPost, "/api/v1/kyc/submit", body, ct))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("no docs: %d", rec.Code)
	}
	// Not multipart → INVALID_REQUEST.
	rec = httptest.NewRecorder()
	r := kycClaimsReq(t, http.MethodPost, "/api/v1/kyc/submit",
		bytes.NewBufferString(`{"x":1}`), "application/json")
	KYCSubmit(svc).ServeHTTP(rec, r)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("non-multipart: %d", rec.Code)
	}
}

func TestKYCSubmitStoreDownDegrades(t *testing.T) {
	svc := compliance.NewService(kycFixtureStore("T0"), nil, "", nil) // objects nil → fail closed
	body, ct := multipartBody(t, "T1", "", map[string][]byte{
		"doc_PASSPORT":        jpegDoc,
		"doc_UTILITY_BILL":    pdfDoc,
		"doc_LIVENESS_SELFIE": pngDoc,
	})
	rec := httptest.NewRecorder()
	KYCSubmit(svc).ServeHTTP(rec, kycClaimsReq(t, http.MethodPost, "/api/v1/kyc/submit", body, ct))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("store-down submit must be 503, got %d", rec.Code)
	}
}

func TestKYCStatus(t *testing.T) {
	store := kycFixtureStore("T0")
	svc := kycTestSvc(store, &fakeObjects{})
	rec := httptest.NewRecorder()
	KYCStatus(svc).ServeHTTP(rec, kycClaimsReq(t, http.MethodGet, "/api/v1/kyc/status", nil, ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d", rec.Code)
	}
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out["tier"] != "T0" {
		t.Fatalf("tier=%v", out["tier"])
	}
}

func TestKYCRequirements(t *testing.T) {
	svc := kycTestSvc(kycFixtureStore("T0"), &fakeObjects{})
	rec := httptest.NewRecorder()
	r := kycClaimsReq(t, http.MethodGet, "/api/v1/kyc/requirements?tier=T1&jurisdiction=US", nil, "")
	KYCRequirements(svc).ServeHTTP(rec, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("requirements: %d", rec.Code)
	}
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	pol, _ := out["policy"].(map[string]any)
	if pol["rescreen_cadence"] != "WEEKLY" {
		t.Fatalf("policy: %v", pol)
	}
}

// ---------------------------------------------------------------------------
// Self-certification
// ---------------------------------------------------------------------------

func TestKYCSelfCertTierGate(t *testing.T) {
	svc := kycTestSvc(kycFixtureStore("T0"), &fakeObjects{})
	body := bytes.NewBufferString(`{"form_type":"W-9","tin":"123456789","tin_country":"US","fields":{"legal_name":"A"}}`)
	rec := httptest.NewRecorder()
	r := kycClaimsReq(t, http.MethodPost, "/api/v1/kyc/self-certification", body, "application/json")
	KYCSelfCertSubmit(svc).ServeHTTP(rec, r)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("T0 self-cert: %d body=%s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "KYC_REQUIRED") {
		t.Fatal("tier gate must emit the registered KYC_REQUIRED code")
	}
}

func TestKYCSelfCertHappyPath(t *testing.T) {
	svc := kycTestSvc(kycFixtureStore("T1"), &fakeObjects{})
	body := bytes.NewBufferString(`{"form_type":"W-9","tin":"123-45-6789","tin_country":"US","tin_kind":"SSN","fields":{"legal_name":"Jane Doe"}}`)
	rec := httptest.NewRecorder()
	r := kycClaimsReq(t, http.MethodPost, "/api/v1/kyc/self-certification", body, "application/json")
	KYCSelfCertSubmit(svc).ServeHTTP(rec, r)
	if rec.Code != http.StatusCreated {
		t.Fatalf("self-cert: %d body=%s", rec.Code, rec.Body)
	}
	var cert map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &cert)
	if cert["tin_kind"] != "SSN" || cert["status"] != "VALIDATED" {
		t.Fatalf("cert: %v", cert)
	}
}

func TestKYCSelfCertBadTIN(t *testing.T) {
	svc := kycTestSvc(kycFixtureStore("T1"), &fakeObjects{})
	body := bytes.NewBufferString(`{"form_type":"W-9","tin":"1234","tin_country":"US","fields":{"legal_name":"A"}}`)
	rec := httptest.NewRecorder()
	r := kycClaimsReq(t, http.MethodPost, "/api/v1/kyc/self-certification", body, "application/json")
	KYCSelfCertSubmit(svc).ServeHTTP(rec, r)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad TIN: %d", rec.Code)
	}
}

var _ = errors.Is // silence unused import if handlers change
