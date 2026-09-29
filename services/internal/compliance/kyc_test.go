// Unit tests for the Phase-12 KYC intake (12.3.4) and ops-matrix /
// tax self-certification logic (12.3.13). Fake store + fake object
// putter — no PostgreSQL/devs3 needed (the SSE-KMS wire assertion lives
// in internal/objectstore/sse_test.go).
package compliance

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"exchange/internal/objectstore"
)

// ---------------------------------------------------------------------------
// Fakes
// ---------------------------------------------------------------------------

type fakePut struct {
	in   objectstore.PutInput
	body []byte
}

type fakeObjects struct {
	puts    []fakePut
	deleted []string
	putErr  error
}

func (f *fakeObjects) Put(ctx context.Context, in objectstore.PutInput) (objectstore.Object, error) {
	if f.putErr != nil {
		return objectstore.Object{}, f.putErr
	}
	var b []byte
	if in.Body != nil {
		buf := make([]byte, in.Size)
		n, _ := in.Body.Read(buf)
		b = buf[:n]
	}
	f.puts = append(f.puts, fakePut{in: in, body: b})
	return objectstore.Object{Key: in.Key, Size: in.Size}, nil
}
func (f *fakeObjects) Delete(ctx context.Context, key string) error {
	f.deleted = append(f.deleted, key)
	return nil
}
func (f *fakeObjects) Bucket() string { return "kyc-test" }

type fakeStore struct {
	tier     string
	cat      string
	nbp      bool
	policies map[string]*TierPolicy
	matrix   []MatrixRow
	subs     []*Submission
	docs     []Document
	certs    []SelfCert
	nextID   int64
	failSub  bool
}

func testPolicies() map[string]*TierPolicy {
	return map[string]*TierPolicy{
		TierT0: {Tier: TierT0, ManualReviewSLAHours: 24, RescreenCadence: "NONE"},
		TierT1: {Tier: TierT1, ManualReviewSLAHours: 24, RescreenCadence: "WEEKLY",
			LivenessRequired: true, StepUpScore: 40, DeclineScore: 80},
		TierT2: {Tier: TierT2, ManualReviewSLAHours: 24, RescreenCadence: "DAILY",
			LivenessRequired: true, BiometricRequired: true, ReverifyMonths: 12,
			StepUpScore: 40, DeclineScore: 80},
		TierInstitutional: {Tier: TierInstitutional, ManualReviewSLAHours: 24,
			RescreenCadence: "DAILY", LivenessRequired: true, BiometricRequired: true,
			ReverifyMonths: 24, StepUpScore: 40, DeclineScore: 80},
	}
}

// testMatrix mirrors the migration-204 seeds minimally.
func testMatrix() []MatrixRow {
	rows := []MatrixRow{}
	add := func(tier, jur, doc, group string, req bool) {
		rows = append(rows, MatrixRow{Tier: tier, Jurisdiction: jur,
			Vendor: "MANUAL", DocumentType: doc, DocGroup: group, Required: req})
	}
	for _, d := range []string{"PASSPORT", "NATIONAL_ID", "DRIVING_LICENCE"} {
		add(TierT1, "*", d, "IDENTITY", true)
		add(TierT2, "*", d, "IDENTITY", true)
	}
	for _, d := range []string{"UTILITY_BILL", "BANK_STATEMENT"} {
		add(TierT1, "*", d, "ADDRESS", true)
		add(TierT2, "*", d, "ADDRESS", true)
	}
	add(TierT1, "*", "LIVENESS_SELFIE", "LIVENESS", true)
	add(TierT2, "*", "LIVENESS_SELFIE", "LIVENESS", true)
	add(TierT2, "*", "SOURCE_OF_FUNDS", "FUNDS", true)
	add(TierInstitutional, "*", "CERTIFICATE_OF_INCORPORATION", "CORPORATE", true)
	add(TierInstitutional, "*", "UBO_DECLARATION", "CORPORATE", true)
	add(TierInstitutional, "*", "SIGNATORY_ID", "IDENTITY", true)
	add(TierInstitutional, "*", "SOURCE_OF_FUNDS", "FUNDS", true)
	add(TierT1, "US", "TAX_SELF_CERT_W9", "TAX", true)
	return rows
}

func (f *fakeStore) AccountTier(ctx context.Context, accountID int64) (string, error) {
	return f.tier, nil
}
func (f *fakeStore) ClientCategory(ctx context.Context, accountID int64) (string, bool, error) {
	if f.cat == "" {
		return string(CategoryRetail), f.nbp, nil
	}
	return f.cat, f.nbp, nil
}
func (f *fakeStore) CreateSubmission(ctx context.Context, sub *Submission) error {
	f.nextID++
	sub.ID = f.nextID
	f.subs = append(f.subs, sub)
	return nil
}
func (f *fakeStore) AttachDocument(ctx context.Context, doc *Document) error {
	f.nextID++
	doc.ID = f.nextID
	f.docs = append(f.docs, *doc)
	return nil
}
func (f *fakeStore) FailSubmission(ctx context.Context, id int64) error {
	f.failSub = true
	for _, s := range f.subs {
		if s.ID == id {
			s.Status = SubFailed
		}
	}
	return nil
}
func (f *fakeStore) LatestSubmission(ctx context.Context, accountID int64) (*Submission, error) {
	if len(f.subs) == 0 {
		return nil, nil
	}
	return f.subs[len(f.subs)-1], nil
}
func (f *fakeStore) ListAccountDocuments(ctx context.Context, accountID int64) ([]Document, error) {
	return f.docs, nil
}
func (f *fakeStore) TierPolicy(ctx context.Context, tier string) (*TierPolicy, error) {
	return f.policies[tier], nil
}
func (f *fakeStore) Matrix(ctx context.Context, tier, jur string) ([]MatrixRow, error) {
	var out []MatrixRow
	for _, r := range f.matrix {
		if r.Tier == tier && (r.Jurisdiction == "*" || r.Jurisdiction == jur) {
			out = append(out, r)
		}
	}
	return out, nil
}
func (f *fakeStore) OverdueReviews(ctx context.Context, now time.Time, limit int) ([]Submission, error) {
	var out []Submission
	for _, s := range f.subs {
		if s.Status == SubPendingReview && s.SLADueAt.Before(now) {
			out = append(out, *s)
		}
	}
	return out, nil
}
func (f *fakeStore) InsertSelfCert(ctx context.Context, c *SelfCert) error {
	f.nextID++
	c.ID = f.nextID
	f.certs = append(f.certs, *c)
	return nil
}
func (f *fakeStore) ListSelfCerts(ctx context.Context, accountID int64) ([]SelfCert, error) {
	return f.certs, nil
}
func (f *fakeStore) HasSelfCert(ctx context.Context, accountID int64) (bool, error) {
	return len(f.certs) > 0, nil
}

type failScanner struct{ err error }

func (s failScanner) Scan(ctx context.Context, docType string, data []byte) error {
	return s.err
}

var (
	pdfBytes  = []byte("%PDF-1.4 fake pdf body for tests\n")
	pngBytes  = append([]byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A}, []byte("IHDR-data")...)
	jpegBytes = append([]byte{0xFF, 0xD8, 0xFF, 0xE0}, []byte("JFIF-body")...)
)

func t1Docs() []UploadDoc {
	return []UploadDoc{
		{Type: "PASSPORT", Filename: "pass.jpg", ContentType: "image/jpeg", Data: jpegBytes},
		{Type: "UTILITY_BILL", Filename: "bill.pdf", ContentType: "application/pdf", Data: pdfBytes},
		{Type: "LIVENESS_SELFIE", Filename: "selfie.png", ContentType: "image/png", Data: pngBytes},
	}
}

func newSvc(store *fakeStore, objects *fakeObjects) *Service {
	svc := NewService(store, objects, "kms-key-1", nil)
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	svc.SetClockForTest(func() time.Time { return now })
	return svc
}

// ---------------------------------------------------------------------------
// Tier ladder + re-verify computation (§14.2)
// ---------------------------------------------------------------------------

func TestTierRank(t *testing.T) {
	if TierRank(TierT0) != 0 || TierRank(TierT1) != 1 ||
		TierRank(TierT2) != 2 || TierRank(TierInstitutional) != 3 {
		t.Fatal("tier ranks wrong")
	}
	if TierRank("BOGUS") != -1 {
		t.Fatal("unknown tier must rank -1")
	}
	if RequestableTier(TierT0) {
		t.Fatal("T0 must not be requestable — it is the unverified default")
	}
}

func TestComputeReverifyDue(t *testing.T) {
	verified := time.Date(2026, 1, 31, 0, 0, 0, 0, time.UTC)
	pols := testPolicies()
	if d := ComputeReverifyDue(pols[TierT2], verified); d == nil ||
		d.Year() != 2027 || d.Month() != 1 || d.Day() != 31 {
		t.Fatalf("T2 reverify = %v, want +12mo", d)
	}
	if d := ComputeReverifyDue(pols[TierInstitutional], verified); d == nil ||
		d.Year() != 2028 || d.Month() != 1 || d.Day() != 31 {
		t.Fatalf("INSTITUTIONAL reverify = %v, want +24mo", d)
	}
	if d := ComputeReverifyDue(pols[TierT1], verified); d != nil {
		t.Fatalf("T1 must carry no re-verify date, got %v", d)
	}
}

// ---------------------------------------------------------------------------
// Submit
// ---------------------------------------------------------------------------

func TestSubmitHappyPath(t *testing.T) {
	store := &fakeStore{tier: TierT0, policies: testPolicies(), matrix: testMatrix()}
	objects := &fakeObjects{}
	svc := newSvc(store, objects)

	res, err := svc.Submit(context.Background(), SubmitInput{
		AccountID: 42, RequestedTier: TierT1, Jurisdiction: "US",
		Documents: t1Docs(),
	})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	sub := res.Submission
	if sub.Status != SubPendingReview {
		t.Fatalf("status %q, want PENDING_REVIEW", sub.Status)
	}
	// 24h manual-review SLA — enforceable via submitted_at + sla_due_at.
	if !sub.SLADueAt.Equal(sub.SubmittedAt.Add(24 * time.Hour)) {
		t.Fatalf("sla_due %v != submitted+24h", sub.SLADueAt)
	}
	if store.tier != TierT0 {
		t.Fatal("submit must not promote the tier — Phase-14 owns approval")
	}
	if len(objects.puts) != 3 || len(store.docs) != 3 {
		t.Fatalf("puts=%d docs=%d, want 3/3", len(objects.puts), len(store.docs))
	}
	for i, p := range objects.puts {
		if p.in.ServerSideEncryption != "aws:kms" || p.in.SSEKMSKeyID != "kms-key-1" {
			t.Fatalf("put %d missing SSE-KMS: alg=%q key=%q (spec §24 #102)",
				i, p.in.ServerSideEncryption, p.in.SSEKMSKeyID)
		}
		if p.in.Metadata["account-id"] != "42" {
			t.Fatalf("put %d metadata missing account-id", i)
		}
	}
	for _, d := range res.Documents {
		if d.ObjectKey != "" {
			t.Fatal("object key leaked into the API-facing result")
		}
		if len(d.SHA256) != 64 || d.SSEAlgorithm != "aws:kms" {
			t.Fatalf("doc integrity/sse columns missing: %+v", d)
		}
	}
}

func TestSubmitValidation(t *testing.T) {
	store := &fakeStore{tier: TierT0, policies: testPolicies(), matrix: testMatrix()}
	svc := newSvc(store, &fakeObjects{})
	ctx := context.Background()

	cases := []struct {
		name string
		in   SubmitInput
	}{
		{"bad tier", SubmitInput{AccountID: 1, RequestedTier: "T9", Documents: t1Docs()}},
		{"T0 request", SubmitInput{AccountID: 1, RequestedTier: TierT0, Documents: t1Docs()}},
		{"no docs", SubmitInput{AccountID: 1, RequestedTier: TierT1}},
		{"bad jurisdiction", SubmitInput{AccountID: 1, RequestedTier: TierT1,
			Jurisdiction: "USA", Documents: t1Docs()}},
		{"missing group", SubmitInput{AccountID: 1, RequestedTier: TierT1,
			Documents: t1Docs()[:1]}}, // IDENTITY only — ADDRESS/LIVENESS missing
		{"unknown type", SubmitInput{AccountID: 1, RequestedTier: TierT1, Documents: append(
			t1Docs(), UploadDoc{Type: "PASSPORT_PHOTO", Data: jpegBytes})}},
		{"empty file", SubmitInput{AccountID: 1, RequestedTier: TierT1, Documents: append(
			t1Docs(), UploadDoc{Type: "BANK_STATEMENT", Data: nil})}},
		{"bad mime", SubmitInput{AccountID: 1, RequestedTier: TierT1, Documents: []UploadDoc{
			{Type: "PASSPORT", Data: []byte("#!/bin/sh\nrm -rf /")},
			{Type: "UTILITY_BILL", Data: pdfBytes},
			{Type: "LIVENESS_SELFIE", Data: pngBytes},
		}}},
	}
	for _, tc := range cases {
		_, err := svc.Submit(ctx, tc.in)
		if !errors.Is(err, ErrValidation) {
			t.Fatalf("%s: err=%v, want ErrValidation", tc.name, err)
		}
	}
}

func TestSubmitOversize(t *testing.T) {
	store := &fakeStore{tier: TierT0, policies: testPolicies(), matrix: testMatrix()}
	svc := newSvc(store, &fakeObjects{})
	big := append([]byte("%PDF-1.4 "), make([]byte, MaxDocBytes)...)
	_, err := svc.Submit(context.Background(), SubmitInput{
		AccountID: 1, RequestedTier: TierT1,
		Documents: []UploadDoc{{Type: "PASSPORT", Data: big}},
	})
	if !errors.Is(err, ErrValidation) {
		t.Fatalf("oversize: %v", err)
	}
}

func TestSubmitScannerFailClosed(t *testing.T) {
	store := &fakeStore{tier: TierT0, policies: testPolicies(), matrix: testMatrix()}
	// Engine failure → fail closed (ErrScanner), nothing persisted.
	svc := newSvc(store, &fakeObjects{})
	svc.scanner = failScanner{err: errors.New("engine down")}
	_, err := svc.Submit(context.Background(), SubmitInput{
		AccountID: 1, RequestedTier: TierT1, Documents: t1Docs()})
	if !errors.Is(err, ErrScanner) {
		t.Fatalf("scanner failure: %v", err)
	}
	if len(store.subs) != 0 {
		t.Fatal("scanner failure must not persist a submission")
	}
	// Infection → ErrInfected (client fault).
	svc.scanner = failScanner{err: ErrInfected}
	_, err = svc.Submit(context.Background(), SubmitInput{
		AccountID: 1, RequestedTier: TierT1, Documents: t1Docs()})
	if !errors.Is(err, ErrInfected) {
		t.Fatalf("infected doc: %v", err)
	}
}

func TestSubmitNoObjectStore(t *testing.T) {
	store := &fakeStore{tier: TierT0, policies: testPolicies(), matrix: testMatrix()}
	svc := NewService(store, nil, "", nil) // objects unset → fail closed
	_, err := svc.Submit(context.Background(), SubmitInput{
		AccountID: 1, RequestedTier: TierT1, Documents: t1Docs()})
	if !errors.Is(err, ErrObjectsUnset) {
		t.Fatalf("nil store: %v", err)
	}
}

func TestSubmitObjectFailureCleansUp(t *testing.T) {
	store := &fakeStore{tier: TierT0, policies: testPolicies(), matrix: testMatrix()}
	objects := &fakeObjects{putErr: errors.New("s3 down")}
	svc := newSvc(store, objects)
	_, err := svc.Submit(context.Background(), SubmitInput{
		AccountID: 1, RequestedTier: TierT1, Documents: t1Docs()})
	if err == nil || len(store.subs) != 1 || !store.failSub {
		t.Fatalf("object failure must fail + mark FAILED: err=%v subs=%v failSub=%v",
			err, len(store.subs), store.failSub)
	}
	for _, s := range store.subs {
		if s.Status != SubFailed {
			t.Fatalf("half-intake left status %q", s.Status)
		}
	}
}

func TestSubmitT2RequiresFundsDoc(t *testing.T) {
	store := &fakeStore{tier: TierT0, policies: testPolicies(), matrix: testMatrix()}
	svc := newSvc(store, &fakeObjects{})
	docs := append(t1Docs(),
		UploadDoc{Type: "SOURCE_OF_FUNDS", Data: pdfBytes, ContentType: "application/pdf"})
	res, err := svc.Submit(context.Background(), SubmitInput{
		AccountID: 7, RequestedTier: TierT2, Documents: docs})
	if err != nil {
		t.Fatalf("T2 submit: %v", err)
	}
	if res.Submission.RequestedTier != TierT2 {
		t.Fatal("requested tier not recorded")
	}
	// Without the funds doc the FUNDS group fails closed.
	_, err = svc.Submit(context.Background(), SubmitInput{
		AccountID: 7, RequestedTier: TierT2, Documents: t1Docs()})
	if !errors.Is(err, ErrValidation) {
		t.Fatalf("T2 without FUNDS doc: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Requirements query (12.3.13)
// ---------------------------------------------------------------------------

func TestRequirementsMerged(t *testing.T) {
	store := &fakeStore{policies: testPolicies(), matrix: testMatrix()}
	svc := newSvc(store, &fakeObjects{})
	req, err := svc.Requirements(context.Background(), "T1", "US")
	if err != nil {
		t.Fatal(err)
	}
	if req.Policy.RescreenCadence != "WEEKLY" || !req.Policy.LivenessRequired {
		t.Fatalf("T1 policy wrong: %+v", req.Policy)
	}
	var sawUS, sawDefault bool
	for _, r := range req.Documents {
		if r.Jurisdiction == "US" {
			sawUS = true
		}
		if r.Jurisdiction == "*" {
			sawDefault = true
		}
	}
	if !sawUS || !sawDefault {
		t.Fatal("matrix merge must carry both default and jurisdiction rows")
	}
	// Unknown tier fails closed.
	if _, err := svc.Requirements(context.Background(), "T9", ""); !errors.Is(err, ErrValidation) {
		t.Fatalf("unknown tier: %v", err)
	}
}

func TestOverdueReviews(t *testing.T) {
	store := &fakeStore{policies: testPolicies(), matrix: testMatrix()}
	svc := newSvc(store, &fakeObjects{})
	if _, err := svc.Submit(context.Background(), SubmitInput{
		AccountID: 1, RequestedTier: TierT1, Documents: t1Docs()}); err != nil {
		t.Fatal(err)
	}
	// sla_due is +24h — overdue at +25h, clear before.
	if got, _ := svc.OverdueReviews(context.Background(), 10); len(got) != 0 {
		t.Fatal("fresh submission must not be overdue")
	}
	// sla_due is +24h past the frozen fixture clock (2026-11-15T12Z) —
	// overdue at +25h, clear before.
	later := time.Date(2026, 11, 16, 13, 0, 0, 0, time.UTC)
	svc.SetClockForTest(func() time.Time { return later })
	if got, _ := svc.OverdueReviews(context.Background(), 10); len(got) != 1 {
		t.Fatalf("want 1 overdue review, got %d", len(got))
	}
}

// ---------------------------------------------------------------------------
// Tax self-certification (12.3.13)
// ---------------------------------------------------------------------------

func TestValidateSelfCertW9(t *testing.T) {
	svc := NewService(nil, nil, "", nil)
	fields := json.RawMessage(`{"legal_name":"Jane Doe","address":"1 Main St"}`)
	// Valid SSN → VALIDATED + kind detected.
	cert, err := svc.ValidateSelfCert(&SelfCertInput{
		AccountID: 1, FormType: "W-9", TIN: "123-45-6789",
		TINCountry: "US", TINKind: "SSN", Fields: fields})
	if err != nil || cert.TINKind != "SSN" || cert.Status != "VALIDATED" {
		t.Fatalf("W-9 SSN: cert=%+v err=%v", cert, err)
	}
	// ITIN pattern (9xx, mid 70-88).
	cert, err = svc.ValidateSelfCert(&SelfCertInput{
		AccountID: 1, FormType: "W-9", TIN: "900701234",
		TINCountry: "US", Fields: fields})
	if err != nil || cert.TINKind != "ITIN" {
		t.Fatalf("W-9 ITIN: cert=%+v err=%v", cert, err)
	}
	// SSN-excluded area (666) must fail.
	if _, err := svc.ValidateSelfCert(&SelfCertInput{
		AccountID: 1, FormType: "W-9", TIN: "666-45-6789",
		TINCountry: "US", TINKind: "SSN", Fields: fields}); !errors.Is(err, ErrValidation) {
		t.Fatalf("SSN area 666 must reject: %v", err)
	}
	// 8 digits must fail.
	if _, err := svc.ValidateSelfCert(&SelfCertInput{
		AccountID: 1, FormType: "W-9", TIN: "12345678",
		TINCountry: "US", Fields: fields}); !errors.Is(err, ErrValidation) {
		t.Fatalf("8-digit TIN must reject: %v", err)
	}
	// W-9 requires US country.
	if _, err := svc.ValidateSelfCert(&SelfCertInput{
		AccountID: 1, FormType: "W-9", TIN: "123456789",
		TINCountry: "DE", Fields: fields}); !errors.Is(err, ErrValidation) {
		t.Fatalf("W-9 non-US must reject: %v", err)
	}
	// Missing legal_name must fail.
	if _, err := svc.ValidateSelfCert(&SelfCertInput{
		AccountID: 1, FormType: "W-9", TIN: "123456789",
		TINCountry: "US", Fields: json.RawMessage(`{"x":1}`)}); !errors.Is(err, ErrValidation) {
		t.Fatalf("missing legal_name must reject: %v", err)
	}
}

func TestValidateSelfCertW8Passthrough(t *testing.T) {
	svc := NewService(nil, nil, "", nil)
	fields := json.RawMessage(`{"legal_name":"GmbH","entity_type":"CORPORATION"}`)
	cert, err := svc.ValidateSelfCert(&SelfCertInput{
		AccountID: 1, FormType: "W-8BEN-E", TIN: "DE-813456789",
		TINCountry: "DE", Fields: fields})
	if err != nil || cert.TINKind != "" || cert.Status != "VALIDATED" {
		t.Fatalf("non-US passthrough: cert=%+v err=%v", cert, err)
	}
	// The passthrough marker must land in fields — honest record that no
	// jurisdiction format check ran.
	var m map[string]any
	if err := json.Unmarshal(cert.Fields, &m); err != nil ||
		m["tin_validation"] != "PASSTHROUGH_NON_US" {
		t.Fatalf("passthrough marker missing: %s", cert.Fields)
	}
}

func TestSubmitSelfCertTierGate(t *testing.T) {
	store := &fakeStore{tier: TierT0, policies: testPolicies()}
	svc := newSvc(store, &fakeObjects{})
	_, err := svc.SubmitSelfCert(context.Background(), SelfCertInput{
		AccountID: 1, FormType: "W-9", TIN: "123456789", TINCountry: "US",
		Fields: json.RawMessage(`{"legal_name":"A"}`)})
	if !errors.Is(err, ErrTierTooLow) {
		t.Fatalf("T0 must reject self-cert: %v", err)
	}
	store.tier = TierT1
	cert, err := svc.SubmitSelfCert(context.Background(), SelfCertInput{
		AccountID: 1, FormType: "W-9", TIN: "123-45-6789", TINCountry: "US",
		TINKind: "SSN", Fields: json.RawMessage(`{"legal_name":"A"}`)})
	if err != nil || cert.ID == 0 {
		t.Fatalf("T1 self-cert: cert=%+v err=%v", cert, err)
	}
}

// ---------------------------------------------------------------------------
// PII-F1 sealing (migration 210) — fail-closed box semantics, no PG needed
// ---------------------------------------------------------------------------

type failBox struct{}

func (failBox) Seal([]byte) ([]byte, error) { return nil, errors.New("seal engine down") }
func (failBox) Open([]byte) ([]byte, error) { return nil, errors.New("seal engine down") }

func TestPgStoreNilBoxFailsClosed(t *testing.T) {
	// A zero-value pool pointer is enough to reach the box check.
	if _, err := NewPgStore(&pgxpool.Pool{}, nil); err == nil {
		t.Fatal("nil box must fail closed at construction")
	}
}

func TestInsertSelfCertSealFailureFailsClosed(t *testing.T) {
	// Seal failure must abort before any INSERT — plaintext PII never
	// reaches the table (the pool is never touched).
	s := &PgStore{box: failBox{}}
	err := s.InsertSelfCert(context.Background(), &SelfCert{
		AccountID: 1, FormType: "W-9", TIN: "123456789",
		Fields: json.RawMessage(`{"legal_name":"A"}`)})
	if err == nil {
		t.Fatal("seal failure must fail the insert")
	}
	// Nil box on a hand-built store is also an error, not a panic.
	s = &PgStore{}
	if err := s.InsertSelfCert(context.Background(), &SelfCert{
		Fields: json.RawMessage(`{"legal_name":"A"}`)}); err == nil {
		t.Fatal("nil box must fail the insert")
	}
}
