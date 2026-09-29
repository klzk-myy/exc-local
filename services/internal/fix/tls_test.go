// tls_test.go — Task 18.3.11: the certificate validation matrix
// (spec §9.7/§24 #167). Exercises VerifyPeer across every admission
// branch: no cert, expired, unbound, rollover window, environment,
// identity, revocation seams, and the happy path.
package fix

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"math/big"
	"testing"
	"time"
)

// --- test PKI ---------------------------------------------------------------

func makeCA(t *testing.T) (*x509.Certificate, *ecdsa.PrivateKey) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return cert, key
}

func makeLeaf(t *testing.T, ca *x509.Certificate, caKey *ecdsa.PrivateKey,
	cn string, sans []string, notBefore, notAfter time.Time) *x509.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: cn},
		DNSNames:     sans,
		NotBefore:    notBefore,
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, &key.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return cert
}

// --- seams -------------------------------------------------------------------

type memBindings map[string]bindingEntry

type bindingEntry struct {
	b   *CertBinding
	p   MatchPath
	err error
}

func (m memBindings) BindingByFingerprint(_ context.Context,
	fp string) (*CertBinding, MatchPath, error) {
	e, ok := m[fp]
	if !ok {
		return nil, "", nil
	}
	return e.b, e.p, e.err
}

type fakeRevocation struct {
	revoked bool
	err     error
	calls   int
}

func (f *fakeRevocation) Revoked(_ context.Context, _, _ *x509.Certificate) (bool, error) {
	f.calls++
	return f.revoked, f.err
}

// certCode extracts the CertError code (test-local: allocation.go's
// package-level codeOf() is the production helper).
func certCode(t *testing.T, err error) string {
	t.Helper()
	var ce *CertError
	if !errors.As(err, &ce) {
		t.Fatalf("expected CertError, got %v", err)
	}
	return ce.Code
}

// TestCertificateMatrix — every admission branch of VerifyPeer.
func TestCertificateMatrix(t *testing.T) {
	now := time.Now()
	ctx := context.Background()
	ca, caKey := makeCA(t)
	leaf := makeLeaf(t, ca, caKey, "FIRMA-ORDER-ENTRY",
		[]string{"firma.example"}, now.Add(-time.Hour), now.Add(24*time.Hour))
	fp := CertFingerprint(leaf)

	state := tls.ConnectionState{PeerCertificates: []*x509.Certificate{leaf}}
	binding := &CertBinding{
		SessionID:   "sess-1",
		Fingerprint: fp,
		CN:          "FIRMA-ORDER-ENTRY",
		Environment: "production",
	}
	rev := &fakeRevocation{}
	opts := &TLSOptions{
		Environment: "production",
		Lookup:      memBindings{fp: {b: binding, p: MatchPrimary}},
		Revocation:  rev,
		Now:         func() time.Time { return now },
	}

	// 1. Happy path — bound, valid, in-window primary cert.
	if err := opts.VerifyPeer(ctx, state); err != nil {
		t.Fatalf("valid cert rejected: %v", err)
	}

	// 2. No peer certificates.
	if err := opts.VerifyPeer(ctx, tls.ConnectionState{}); certCode(t, err) != "MTLS_CERT_REQUIRED" {
		t.Fatalf("missing cert: %v", err)
	}

	// 3. Expired certificate.
	expired := makeLeaf(t, ca, caKey, "FIRMA-ORDER-ENTRY", nil,
		now.Add(-48*time.Hour), now.Add(-time.Hour))
	opts.Lookup = memBindings{CertFingerprint(expired): {b: &CertBinding{
		SessionID: "s", Environment: "production"}, p: MatchPrimary}}
	if err := opts.VerifyPeer(ctx, tls.ConnectionState{
		PeerCertificates: []*x509.Certificate{expired}}); certCode(t, err) != "MTLS_CERT_EXPIRED" {
		t.Fatalf("expired cert: %v", err)
	}

	// 4. Not-yet-valid certificate.
	future := makeLeaf(t, ca, caKey, "FIRMA-ORDER-ENTRY", nil,
		now.Add(time.Hour), now.Add(48*time.Hour))
	if err := opts.VerifyPeer(ctx, tls.ConnectionState{
		PeerCertificates: []*x509.Certificate{future}}); certCode(t, err) != "MTLS_CERT_EXPIRED" {
		t.Fatalf("not-yet-valid cert: %v", err)
	}
	opts.Lookup = memBindings{fp: {b: binding, p: MatchPrimary}}

	// 5. Unprovisioned fingerprint.
	opts.Lookup = memBindings{}
	if err := opts.VerifyPeer(ctx, state); certCode(t, err) != "MTLS_CERT_NOT_BOUND" {
		t.Fatalf("unbound cert: %v", err)
	}
	opts.Lookup = memBindings{fp: {b: binding, p: MatchPrimary}}

	// 6. Environment mismatch — staging cert on production listener.
	opts.Lookup = memBindings{fp: {b: &CertBinding{
		SessionID: "s", Fingerprint: fp, Environment: "staging"}, p: MatchPrimary}}
	if err := opts.VerifyPeer(ctx, state); certCode(t, err) != "MTLS_ENV_MISMATCH" {
		t.Fatalf("env mismatch: %v", err)
	}
	opts.Lookup = memBindings{fp: {b: binding, p: MatchPrimary}}

	// 7. Identity mismatch — bound CN matches neither Subject CN nor SAN.
	opts.Lookup = memBindings{fp: {b: &CertBinding{
		SessionID: "s", Fingerprint: fp, CN: "OTHER-FIRM",
		Environment: "production"}, p: MatchPrimary}}
	if err := opts.VerifyPeer(ctx, state); certCode(t, err) != "MTLS_IDENTITY_MISMATCH" {
		t.Fatalf("CN/SAN mismatch: %v", err)
	}
	opts.Lookup = memBindings{fp: {b: binding, p: MatchPrimary}}

	// 8. Identity via SAN (CN differs, SAN contains bound name).
	opts.Lookup = memBindings{fp: {b: &CertBinding{
		SessionID: "s", Fingerprint: fp, CN: "firma.example",
		Environment: "production"}, p: MatchPrimary}}
	if err := opts.VerifyPeer(ctx, state); err != nil {
		t.Fatalf("SAN-bound cert rejected: %v", err)
	}
	opts.Lookup = memBindings{fp: {b: binding, p: MatchPrimary}}

	// 9. Revoked certificate.
	rev.revoked = true
	if err := opts.VerifyPeer(ctx, state); certCode(t, err) != "MTLS_CERT_REVOKED" {
		t.Fatalf("revoked cert: %v", err)
	}
	rev.revoked = false

	// 10. Revocation checker error fails closed.
	rev.err = errors.New("CRL fetch failed")
	if err := opts.VerifyPeer(ctx, state); certCode(t, err) != "MTLS_REVOCATION_UNVERIFIABLE" {
		t.Fatalf("revocation error: %v", err)
	}
	rev.err = nil

	// 11. Nil revocation checker fails closed.
	opts.Revocation = nil
	if err := opts.VerifyPeer(ctx, state); certCode(t, err) != "MTLS_REVOCATION_UNVERIFIABLE" {
		t.Fatalf("nil revocation: %v", err)
	}
	opts.Revocation = rev

	// 12. Nil binding lookup fails closed.
	opts.Lookup = nil
	if err := opts.VerifyPeer(ctx, state); certCode(t, err) != "MTLS_BINDING_UNAVAILABLE" {
		t.Fatalf("nil lookup: %v", err)
	}
	opts.Lookup = memBindings{fp: {b: binding, p: MatchPrimary}}

	// 13. Lookup error fails closed.
	opts.Lookup = memBindings{fp: {err: errors.New("pg down")}}
	if err := opts.VerifyPeer(ctx, state); certCode(t, err) != "MTLS_BINDING_UNAVAILABLE" {
		t.Fatalf("lookup error: %v", err)
	}
	opts.Lookup = memBindings{fp: {b: binding, p: MatchPrimary}}
}

// TestRolloverWindow — dual-cert rollover: inside the window the
// rollover fingerprint verifies (audited as "rollover"); after the
// window the same credential rejects.
func TestRolloverWindow(t *testing.T) {
	now := time.Now()
	ctx := context.Background()
	ca, caKey := makeCA(t)
	oldLeaf := makeLeaf(t, ca, caKey, "FIRMA", nil,
		now.Add(-time.Hour), now.Add(24*time.Hour))
	newLeaf := makeLeaf(t, ca, caKey, "FIRMA", nil,
		now.Add(-time.Hour), now.Add(24*time.Hour))
	newFP := CertFingerprint(newLeaf)
	windowEnd := now.Add(time.Hour)

	binding := &CertBinding{
		SessionID:           "sess-1",
		Fingerprint:         CertFingerprint(oldLeaf),
		RolloverFingerprint: newFP,
		RolloverEndsAt:      &windowEnd,
		Environment:         "production",
	}
	var events []CertEvent
	opts := &TLSOptions{
		Environment: "production",
		Lookup: memBindings{
			binding.Fingerprint: {b: binding, p: MatchPrimary},
			newFP:               {b: binding, p: MatchRollover},
		},
		Revocation: &fakeRevocation{},
		Now:        func() time.Time { return now },
		Audit:      func(e CertEvent) { events = append(events, e) },
	}

	// Both credentials verify inside the window.
	if err := opts.VerifyPeer(ctx, tls.ConnectionState{
		PeerCertificates: []*x509.Certificate{oldLeaf}}); err != nil {
		t.Fatalf("primary cert inside window rejected: %v", err)
	}
	if err := opts.VerifyPeer(ctx, tls.ConnectionState{
		PeerCertificates: []*x509.Certificate{newLeaf}}); err != nil {
		t.Fatalf("rollover cert inside window rejected: %v", err)
	}
	// Rollover acceptance is audited as MatchRollover.
	var sawRollover bool
	for _, e := range events {
		if e.Path == MatchRollover && e.Outcome == "accept" {
			sawRollover = true
		}
	}
	if !sawRollover {
		t.Fatal("rollover-path acceptance not audited")
	}

	// After the window the rollover path rejects.
	opts.Now = func() time.Time { return windowEnd.Add(time.Second) }
	if err := opts.VerifyPeer(ctx, tls.ConnectionState{
		PeerCertificates: []*x509.Certificate{newLeaf}}); certCode(t, err) != "MTLS_CERT_ROLLOVER_EXPIRED" {
		t.Fatalf("rollover cert after window: %v", err)
	}
	// Primary still verifies after the window.
	if err := opts.VerifyPeer(ctx, tls.ConnectionState{
		PeerCertificates: []*x509.Certificate{oldLeaf}}); err != nil {
		t.Fatalf("primary cert after window rejected: %v", err)
	}
}

// TestServerTLSConfigProfile — TLS 1.3-only, mTLS required, fail-closed
// constructor validation.
func TestServerTLSConfigProfile(t *testing.T) {
	o := TLSOptions{RequireClientCert: true}
	if _, err := ServerTLSConfig(o); err == nil {
		t.Fatal("no server cert accepted")
	}
}
