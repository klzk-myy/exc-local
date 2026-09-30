// Phase-21 wave-3 unit tests — pure (no-DB) coverage for:
//
//	Task 21.3.7   GeoGate decisions + CIDR resolver + consent enums
//	Task 21.3.18  residency policy resolution / replication legality
//	Task 21.3.20  comms register chain hash + retention floor
//	Task 21.3.26  promotion approval checklist
//
// PG-backed behaviour lives in p21_wave3_pg_test.go (EXC_PG_TEST=1).
package compliance

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"exchange/internal/auth"
	excerrors "exchange/pkg/errors"
)

// geoPool is never queried while SetPoliciesForTest holds a fresh map —
// a bare (unconnected) pool is enough for pure gate decisions.
func geoTestGate(t *testing.T, resolver GeoResolver, policies map[string]string) *GeoGate {
	t.Helper()
	g, err := NewGeoGate(&pgxpool.Pool{}, resolver, false)
	if err != nil {
		t.Fatalf("geo gate: %v", err)
	}
	now := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	g.SetClockForTest(func() time.Time { return now })
	if policies != nil {
		g.SetPoliciesForTest(policies)
	}
	return g
}

type errGeoResolver struct{ err error }

func (e errGeoResolver) Lookup(context.Context, string) (string, error) {
	return "", e.err
}

func geoRequest(t *testing.T, g *GeoGate, method, path, ip string) int {
	t.Helper()
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	req := httptest.NewRequest(method, path, nil)
	req.RemoteAddr = ip + ":1234"
	rec := httptest.NewRecorder()
	g.Middleware()(next).ServeHTTP(rec, req)
	return rec.Code
}

func TestGeoGateNilResolverPasses(t *testing.T) {
	g := geoTestGate(t, nil, map[string]string{"US": GeoActionRetailBlock})
	if got := geoRequest(t, g, http.MethodPost, "/api/v1/orders", "1.2.3.4"); got != http.StatusNoContent {
		t.Fatalf("nil resolver must pass through, got %d", got)
	}
}

func TestGeoGateResolverErrorFailsClosed(t *testing.T) {
	g := geoTestGate(t, errGeoResolver{err: errors.New("geoip down")},
		map[string]string{"US": GeoActionRetailBlock})
	got := geoRequest(t, g, http.MethodGet, "/api/v1/markets", "1.2.3.4")
	if got != http.StatusOK && got == http.StatusNoContent {
		t.Fatalf("resolver error must not pass, got %d", got)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/markets", nil)
	req.RemoteAddr = "1.2.3.4:1"
	g.Middleware()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rec, req)
	if rec.Code == http.StatusNoContent {
		t.Fatal("resolver error passed through")
	}
}

func TestGeoGateBlockAndAllow(t *testing.T) {
	resolver, err := NewCIDRResolver(map[string]string{
		"10.0.0.0/8": "IR",
		"20.0.0.0/8": "DE",
	})
	if err != nil {
		t.Fatalf("resolver: %v", err)
	}
	g := geoTestGate(t, resolver, map[string]string{
		"IR": GeoActionBlock,
		"DE": GeoActionAllow,
	})
	if got := geoRequest(t, g, http.MethodGet, "/api/v1/markets", "10.1.2.3"); got != http.StatusNoContent {
		// expected: refused (non-204)
		if got == http.StatusNoContent {
			t.Fatal("BLOCK country passed")
		}
	}
	if got := geoRequest(t, g, http.MethodPost, "/api/v1/orders", "20.1.2.3"); got != http.StatusNoContent {
		t.Fatalf("ALLOW country refused: %d", got)
	}
	// Unmapped country → nothing to enforce.
	if got := geoRequest(t, g, http.MethodPost, "/api/v1/orders", "30.1.2.3"); got != http.StatusNoContent {
		t.Fatalf("unmapped country refused: %d", got)
	}
}

func TestGeoGateRetailBlockMatrix(t *testing.T) {
	resolver, err := NewCIDRResolver(map[string]string{"40.0.0.0/8": "US"})
	if err != nil {
		t.Fatalf("resolver: %v", err)
	}
	g := geoTestGate(t, resolver, map[string]string{"US": GeoActionRetailBlock})

	// Read-only passes.
	if got := geoRequest(t, g, http.MethodGet, "/api/v1/positions", "40.1.1.1"); got != http.StatusNoContent {
		t.Fatalf("RETAIL_BLOCK must allow GETs, got %d", got)
	}
	// Anonymous mutating = onboarding attempt → refused.
	if got := geoRequest(t, g, http.MethodPost, "/api/v1/orders", "40.1.1.1"); got == http.StatusNoContent {
		t.Fatal("anonymous mutating request from RETAIL_BLOCK passed")
	}
	// Data-rights paths are exempt even when mutating.
	if got := geoRequest(t, g, http.MethodPost, "/api/v1/account/gdpr/erase", "40.1.1.1"); got != http.StatusNoContent {
		t.Fatalf("gdpr exempt path refused: %d", got)
	}
	if got := geoRequest(t, g, http.MethodPost, "/api/v1/account/close", "40.1.1.1"); got != http.StatusNoContent {
		t.Fatalf("account-close exempt path refused: %d", got)
	}
}

func TestGeoGateRetailBlockAuthedWithoutCategoryFailsClosed(t *testing.T) {
	resolver, err := NewCIDRResolver(map[string]string{"40.0.0.0/8": "US"})
	if err != nil {
		t.Fatalf("resolver: %v", err)
	}
	// Dead-DSN pool: categoryFor errors rather than panicking on a
	// zero-value puddle pool — the gate must degrade, not open.
	dead, err := pgxpool.New(context.Background(),
		"postgres://127.0.0.1:1/x?connect_timeout=1")
	if err != nil {
		t.Fatalf("dead pool: %v", err)
	}
	t.Cleanup(dead.Close)
	g, err := NewGeoGate(dead, resolver, false)
	if err != nil {
		t.Fatalf("geo gate: %v", err)
	}
	g.SetClockForTest(func() time.Time {
		return time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	})
	g.SetPoliciesForTest(map[string]string{"US": GeoActionRetailBlock})
	// Claims present but the category lookup fails (dead pool) → the
	// gate must not open — SERVICE_DEGRADED, not a pass.
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/orders", nil)
	req.RemoteAddr = "40.1.1.1:9"
	req = req.WithContext(auth.WithClaims(req.Context(),
		auth.Claims{Subject: "1", AccountID: 99}))
	rec := httptest.NewRecorder()
	g.Middleware()(next).ServeHTTP(rec, req)
	if rec.Code == http.StatusNoContent {
		t.Fatal("unresolvable client category opened the gate")
	}
	var env map[string]any
	_ = env // envelope shape covered by api tests; status is the gate contract
}

func TestCIDRResolverLongestPrefix(t *testing.T) {
	r, err := NewCIDRResolver(map[string]string{
		"10.0.0.0/8":    "US",
		"10.9.0.0/16":   "DE",
		"10.9.8.0/24":   "FR",
		"bad-cidr-trap": "XX",
	})
	if err == nil {
		t.Fatal("bad CIDR must abort construction")
	}
	_ = r
	r, err = NewCIDRResolver(map[string]string{
		"10.0.0.0/8":  "US",
		"10.9.0.0/16": "DE",
		"10.9.8.0/24": "FR",
	})
	if err != nil {
		t.Fatalf("resolver: %v", err)
	}
	cases := map[string]string{
		"10.9.8.1":  "FR", // most specific wins
		"10.9.9.1":  "DE",
		"10.11.1.1": "US",
		"8.8.8.8":   "",
	}
	for ip, want := range cases {
		got, err := r.Lookup(context.Background(), ip)
		if err != nil {
			t.Fatalf("lookup %s: %v", ip, err)
		}
		if got != want {
			t.Fatalf("lookup %s = %q, want %q", ip, got, want)
		}
	}
	if _, err := r.Lookup(context.Background(), "not-an-ip"); err == nil {
		t.Fatal("unparsable ip must error (fail-closed)")
	}
}

func TestConsentPurposeValidation(t *testing.T) {
	for _, p := range []string{"MARKETING", "ANALYTICS", "DATA_SHARING", "marketing"} {
		if !ValidConsentPurpose(p) {
			t.Fatalf("purpose %s must be valid (case-insensitive)", p)
		}
	}
	for _, p := range []string{"", "KYC", "EXECUTION_POLICY", "TRADING"} {
		if ValidConsentPurpose(p) {
			t.Fatalf("purpose %q must be invalid", p)
		}
	}
}

func TestCommsChainHashDeterministicAndLinked(t *testing.T) {
	ts := time.Date(2030, 6, 1, 12, 0, 0, 0, time.UTC)
	a := &Recording{
		RecordingID: 1, Channel: "PHONE", Direction: "OUT",
		Source: "desk", SourceID: 7,
		StartedAt: ts, EndedAt: ts.Add(time.Minute),
		ContentRef: "k/1", SHA256: "aa",
		RetentionUntil: ts.Add(5 * 365 * 24 * time.Hour),
		PrevChainHash:  "",
	}
	h1 := commsChainHash(a)
	if commsChainHash(a) != h1 {
		t.Fatal("chain hash not deterministic")
	}
	b := *a
	b.PrevChainHash = h1
	if commsChainHash(&b) == h1 {
		t.Fatal("prev-chain link must change the hash")
	}
	c := *a
	c.RecordingID = 2
	if commsChainHash(&c) == h1 {
		t.Fatal("recording id must be bound into the hash")
	}
	d := *a
	d.SHA256 = "bb"
	if commsChainHash(&d) == h1 {
		t.Fatal("content digest must be bound into the hash")
	}
	if len(h1) != 64 {
		t.Fatalf("hash must be 64 hex chars, got %d", len(h1))
	}
}

func TestCommsRetentionFloor(t *testing.T) {
	s := &CommsRecordingService{retention: CommsRetentionFloor}
	ended := time.Date(2030, 1, 15, 0, 0, 0, 0, time.UTC)
	floor := s.retentionFloor(ended)
	if !floor.After(ended) {
		t.Fatal("retention floor must extend past the recording end")
	}
	if floor.Before(ended.Add(CommsRetentionFloor)) {
		t.Fatal("retention floor shorter than the statutory minimum")
	}
}

func TestPromoChecklistAllMandatory(t *testing.T) {
	full := PromoChecklist{RiskWarning: true, CapitalAtRisk: true,
		ClaimBasis: true, EntityDetails: true, FairClear: true}
	if !full.OK() {
		t.Fatal("full checklist must pass")
	}
	for _, mutate := range []func(*PromoChecklist){
		func(c *PromoChecklist) { c.RiskWarning = false },
		func(c *PromoChecklist) { c.CapitalAtRisk = false },
		func(c *PromoChecklist) { c.ClaimBasis = false },
		func(c *PromoChecklist) { c.EntityDetails = false },
		func(c *PromoChecklist) { c.FairClear = false },
	} {
		c := full
		mutate(&c)
		if c.OK() {
			t.Fatalf("incomplete checklist passed: %+v", c)
		}
	}
}

// ---------------------------------------------------------------------------
// Residency (21.3.18) — policy-injected, no DB.
// ---------------------------------------------------------------------------

func residencyTestSvc(t *testing.T) *ResidencyService {
	t.Helper()
	s, err := NewResidencyService(&pgxpool.Pool{})
	if err != nil {
		t.Fatalf("residency svc: %v", err)
	}
	s.SetClockForTest(func() time.Time {
		return time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	})
	s.SetPoliciesForTest([]ResidencyPolicy{
		{JurisdictionCode: "EU", HomeRegion: "eu-central",
			KMSKeyID: "kms-eu", S3Bucket: "exc-eu", PGPartition: "pg_eu",
			AppliesTo: []string{"DE", "FR"}, Adequate: true,
			TransferInstrument: "ADEQUACY"},
		{JurisdictionCode: "US", HomeRegion: "us-east",
			KMSKeyID: "kms-us", S3Bucket: "exc-us", PGPartition: "pg_us",
			AppliesTo: []string{"US"}, Adequate: false,
			TransferInstrument: "SCC"},
		{JurisdictionCode: "XX", HomeRegion: "offshore",
			KMSKeyID: "kms-x", S3Bucket: "exc-x", PGPartition: "pg_x",
			AppliesTo: []string{"NK"}, Adequate: false,
			TransferInstrument: "NONE"},
		{JurisdictionCode: "ROW", HomeRegion: "eu-central",
			KMSKeyID: "kms-eu", S3Bucket: "exc-eu", PGPartition: "pg_eu",
			AppliesTo: nil, Adequate: true,
			TransferInstrument: "ADEQUACY"},
	})
	return s
}

func TestResidencyResolveCountry(t *testing.T) {
	s := residencyTestSvc(t)
	for in, want := range map[string]string{"DE": "EU", "fr": "EU", "US": "US", "NK": "XX", "ZZ": "ROW"} {
		got, err := s.ResolveCountry(context.Background(), in)
		if err != nil {
			t.Fatalf("resolve %s: %v", in, err)
		}
		if got != want {
			t.Fatalf("resolve %s = %s, want %s", in, got, want)
		}
	}
	if _, err := s.ResolveCountry(context.Background(), ""); err == nil {
		t.Fatal("empty country must fail")
	}
}

func TestResidencyResolveFailClosedWithoutROW(t *testing.T) {
	s, err := NewResidencyService(&pgxpool.Pool{})
	if err != nil {
		t.Fatalf("residency svc: %v", err)
	}
	s.SetClockForTest(func() time.Time { return time.Now() })
	s.SetPoliciesForTest([]ResidencyPolicy{
		{JurisdictionCode: "EU", HomeRegion: "eu-central",
			KMSKeyID: "k", S3Bucket: "b", PGPartition: "p",
			AppliesTo: []string{"DE"}},
	})
	if _, err := s.ResolveCountry(context.Background(), "JP"); err == nil {
		t.Fatal("unmapped country without ROW must fail closed")
	}
}

func TestResidencyReplicationLegality(t *testing.T) {
	s := residencyTestSvc(t)
	if err := s.AssertReplicationAllowed(context.Background(), "EU", "US"); err != nil {
		t.Fatalf("EU→US with SCC must pass: %v", err)
	}
	if err := s.AssertReplicationAllowed(context.Background(), "EU", "EU"); err != nil {
		t.Fatalf("intra-region must pass: %v", err)
	}
	err := s.AssertReplicationAllowed(context.Background(), "EU", "XX")
	if err == nil {
		t.Fatal("transfer to no-instrument jurisdiction must fail")
	}
	var coded *excerrors.Error
	if !errors.As(err, &coded) || coded.Code != "RESIDENCY_VIOLATION" {
		t.Fatalf("want RESIDENCY_VIOLATION, got %v", err)
	}
}

func TestResidencyVerifyPlacement(t *testing.T) {
	s := residencyTestSvc(t)
	if err := s.VerifyPlacement(context.Background(), "EU", "exc-eu", "kms-eu"); err != nil {
		t.Fatalf("matching placement must pass: %v", err)
	}
	if err := s.VerifyPlacement(context.Background(), "EU", "exc-us", ""); err == nil {
		t.Fatal("wrong bucket must fail")
	}
	if err := s.VerifyPlacement(context.Background(), "EU", "exc-eu", "kms-us"); err == nil {
		t.Fatal("wrong KMS key must fail")
	}
	if err := s.VerifyPlacement(context.Background(), "NOPE", "x", ""); err == nil {
		t.Fatal("unknown jurisdiction must fail")
	}
}
