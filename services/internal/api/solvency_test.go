// Task 13.3.7/13.3.8 handler unit tests — fake seams, no DB.
package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"exchange/internal/admin"
	"exchange/internal/auth"
	"exchange/internal/reconciliation"
	excerrors "exchange/pkg/errors"
)

type fakeSolvReader struct {
	snap   *reconciliation.Snapshot
	proof  *reconciliation.ProofResult
	proofs []reconciliation.ProofResult
	err    error
}

func (f *fakeSolvReader) LatestSnapshot(context.Context) (*reconciliation.Snapshot, error) {
	return f.snap, f.err
}
func (f *fakeSolvReader) ProofFor(context.Context, int64, string) (*reconciliation.ProofResult, error) {
	return f.proof, f.err
}
func (f *fakeSolvReader) ProofsForAccount(context.Context, int64) ([]reconciliation.ProofResult, error) {
	return f.proofs, f.err
}

type fakeDual struct {
	in  admin.SubmitInput
	req *admin.DualControlRequest
	err error
}

func (f *fakeDual) Submit(_ context.Context, in admin.SubmitInput) (*admin.DualControlRequest, error) {
	if f.err != nil {
		return nil, f.err
	}
	f.in = in
	if f.req != nil {
		return f.req, nil
	}
	return &admin.DualControlRequest{ID: 7, Operation: in.Operation,
		Status: admin.ReqPending, RequiredRole: in.RequiredRole}, nil
}

func solvReq(t *testing.T, method, target, body string, claims *auth.Claims) (*httptest.ResponseRecorder, *http.Request) {
	t.Helper()
	var r *http.Request
	if body != "" {
		r = httptest.NewRequest(method, target, strings.NewReader(body))
	} else {
		r = httptest.NewRequest(method, target, nil)
	}
	if claims != nil {
		r = r.WithContext(auth.WithClaims(r.Context(), *claims))
	}
	return httptest.NewRecorder(), r
}

func TestSolvencyLatestPublic(t *testing.T) {
	// No claims at all — the route is authPublic.
	reader := &fakeSolvReader{snap: &reconciliation.Snapshot{
		ID: 5, MerkleRoot: strings.Repeat("ab", 32), LeafCount: 3,
		Solvent: true, Signature: "SIG", SignerKind: "GPG",
	}}
	rec, r := solvReq(t, "GET", "/api/v1/solvency/latest", "", nil)
	SolvencyLatest(reader).ServeHTTP(rec, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "merkle_root") ||
		!strings.Contains(rec.Body.String(), "signature") {
		t.Fatalf("body=%s", rec.Body)
	}
}

func TestSolvencyLatestNonePublished(t *testing.T) {
	reader := &fakeSolvReader{err: excerrors.New("NOT_FOUND", "none")}
	rec, r := solvReq(t, "GET", "/api/v1/solvency/latest", "", nil)
	SolvencyLatest(reader).ServeHTTP(rec, r)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("code=%d", rec.Code)
	}
}

func TestSolvencyProofAuth(t *testing.T) {
	reader := &fakeSolvReader{}

	// Unauthenticated → 401.
	rec, r := solvReq(t, "GET", "/api/v1/solvency/proof?account_id=9&currency=USD", "", nil)
	SolvencyProof(reader).ServeHTTP(rec, r)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("anon code=%d", rec.Code)
	}

	claims := &auth.Claims{Subject: "100", AccountID: 9, Scopes: []string{"read"}}

	// Missing account_id → 400.
	rec, r = solvReq(t, "GET", "/api/v1/solvency/proof?currency=USD", "", claims)
	SolvencyProof(reader).ServeHTTP(rec, r)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("missing account_id code=%d", rec.Code)
	}
	// Missing currency → 400.
	rec, r = solvReq(t, "GET", "/api/v1/solvency/proof?account_id=9", "", claims)
	SolvencyProof(reader).ServeHTTP(rec, r)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("missing currency code=%d", rec.Code)
	}
	// Foreign account_id → 403 (the account-ownership boundary).
	rec, r = solvReq(t, "GET", "/api/v1/solvency/proof?account_id=42&currency=USD", "", claims)
	SolvencyProof(reader).ServeHTTP(rec, r)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("foreign account code=%d", rec.Code)
	}
	// Non-numeric foreign id → 403, not a parse oracle.
	rec, r = solvReq(t, "GET", "/api/v1/solvency/proof?account_id=x&currency=USD", "", claims)
	SolvencyProof(reader).ServeHTTP(rec, r)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("malformed account code=%d", rec.Code)
	}

	// Own account → the proof as stored (hashes-only path).
	reader.proof = &reconciliation.ProofResult{
		AccountID: 9, Currency: "USD", Balance: "10.5",
		Path: []reconciliation.ProofStepJSON{{Position: "right", Hash: "aa"}},
	}
	rec, r = solvReq(t, "GET", "/api/v1/solvency/proof?account_id=9&currency=USD", "", claims)
	SolvencyProof(reader).ServeHTTP(rec, r)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"balance":"10.5"`) {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
}

func TestAccountSolvencyProof(t *testing.T) {
	reader := &fakeSolvReader{proofs: []reconciliation.ProofResult{
		{AccountID: 9, Currency: "USD", Balance: "10"},
		{AccountID: 9, Currency: "EUR", Balance: "0"},
	}}
	claims := &auth.Claims{Subject: "100", AccountID: 9, Scopes: []string{"read"}}
	rec, r := solvReq(t, "GET", "/api/v1/account/solvency-proof", "", claims)
	AccountSolvencyProof(reader).ServeHTTP(rec, r)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"EUR"`) {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	// Empty snapshot for the account → [] not null.
	reader.proofs = nil
	rec, r = solvReq(t, "GET", "/api/v1/account/solvency-proof", "", claims)
	AccountSolvencyProof(reader).ServeHTTP(rec, r)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"proofs":[]`) {
		t.Fatalf("empty proofs body=%s", rec.Body)
	}
}

func TestAdminAPIKeyExtendExpiry(t *testing.T) {
	dual := &fakeDual{}
	h := AdminAPIKeyExtendExpiry(dual, false)

	withAdmin := func(r *http.Request) *http.Request {
		return r.WithContext(admin.WithIdentity(r.Context(),
			&admin.Identity{UserID: 7, Role: admin.RoleSuperAdmin}))
	}

	// Bad path id → 400.
	rec, r := solvReq(t, "PUT", "/api/v1/admin/api-keys/x/extend-expiry",
		`{"until":"2099-01-01T00:00:00Z"}`, nil)
	r.SetPathValue("id", "x")
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad id code=%d", rec.Code)
	}

	// No admin identity → 401 (wiring must attach via RBAC middleware).
	rec, r = solvReq(t, "PUT", "/api/v1/admin/api-keys/9/extend-expiry",
		`{"until":"`+time.Now().Add(24*time.Hour).UTC().Format(time.RFC3339)+`"}`, nil)
	r.SetPathValue("id", "9")
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no identity code=%d", rec.Code)
	}

	// Past until → 400.
	rec, r = solvReq(t, "PUT", "/api/v1/admin/api-keys/9/extend-expiry",
		`{"until":"2000-01-01T00:00:00Z"}`, nil)
	r.SetPathValue("id", "9")
	h.ServeHTTP(rec, withAdmin(r))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("past until code=%d", rec.Code)
	}
	// >180d until → 400.
	rec, r = solvReq(t, "PUT", "/api/v1/admin/api-keys/9/extend-expiry",
		`{"until":"`+time.Now().Add(400*24*time.Hour).UTC().Format(time.RFC3339)+`"}`, nil)
	r.SetPathValue("id", "9")
	h.ServeHTTP(rec, withAdmin(r))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("over-bound until code=%d", rec.Code)
	}
	// Valid → PENDING dual-control request; the grant is NOT applied inline.
	until := time.Now().Add(30 * 24 * time.Hour).UTC().Format(time.RFC3339)
	rec, r = solvReq(t, "PUT", "/api/v1/admin/api-keys/9/extend-expiry",
		`{"until":"`+until+`","reason":"corp ticket"}`, nil)
	r.SetPathValue("id", "9")
	h.ServeHTTP(rec, withAdmin(r))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if dual.in.Operation != admin.OpAPIKeyExpiryExtend ||
		dual.in.TargetType != "api_key" || dual.in.TargetID != "9" ||
		dual.in.RequiredRole != admin.RoleSuperAdmin {
		t.Fatalf("submit %+v", dual.in)
	}
	if !strings.Contains(string(dual.in.Payload.(map[string]any)["until"].(string)), "T") {
		t.Fatalf("payload %+v", dual.in.Payload)
	}
}
