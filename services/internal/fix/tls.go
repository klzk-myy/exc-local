// tls.go — Task 18.3.11: FIXS mutual TLS 1.3 with certificate-to-session
// binding (spec §9.7, §24 #167).
//
// The venue speaks the FIX Trading Community FIXS profile: TLS 1.3 with
// mutual authentication on every order-entry and administrative acceptor.
// Transport identity is bound BEFORE the FIX Logon is evaluated — the
// client certificate's SHA-256 fingerprint (or its rollover twin, inside
// the rotation window) must resolve to a provisioned fix_sessions row
// whose environment label matches this listener, or the TLS handshake
// itself fails and no FIX message is ever read (fail closed, §2.7).
//
// Dual-certificate rollover (spec §9.7): a row may carry
// cert_rollover_fingerprint + cert_rotation_ends_at; while the window is
// open either fingerprint verifies, and every rollover-path acceptance
// is emitted on the Audit seam. After the window the rollover
// fingerprint stops resolving.
package fix

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"fmt"
	"time"

	"golang.org/x/crypto/ocsp"
)

// CertFingerprint is the canonical certificate identity: lowercase hex
// SHA-256 over the DER (migration 052 fix_sessions.cert_fingerprint).
func CertFingerprint(cert *x509.Certificate) string {
	sum := sha256.Sum256(cert.Raw)
	return hex.EncodeToString(sum[:])
}

// CertBinding is the certificate identity row joined to its session.
// Populated from the migration-052 columns on fix_sessions.
type CertBinding struct {
	SessionID           string
	AccountID           *int64
	Fingerprint         string // cert_fingerprint — primary identity
	RolloverFingerprint string // cert_rollover_fingerprint
	RolloverEndsAt      *time.Time
	CN                  string // cert_cn — expected Subject CN or SAN
	Environment         string // environment label this binding serves
}

// MatchPath records which credential verified — "primary" or
// "rollover" (auditable dual-cert rollover).
type MatchPath string

const (
	MatchPrimary  MatchPath = "primary"
	MatchRollover MatchPath = "rollover"
)

// BindingLookup resolves a presented certificate fingerprint to the
// session binding provisioned for it (PgBindingLookup in production).
// A lookup MISS (nil, nil) means the certificate is not provisioned —
// always a handshake rejection.
type BindingLookup interface {
	BindingByFingerprint(ctx context.Context, fingerprint string) (*CertBinding, MatchPath, error)
}

// RevocationChecker is the revocation seam (CRL distribution point /
// internal revocation list; stapled OCSP is checked separately by
// ServerTLSConfig). A nil checker on a listener that requires client
// certs fails closed — revoked status is unverifiable (spec §2.7).
type RevocationChecker interface {
	Revoked(ctx context.Context, cert *x509.Certificate, issuer *x509.Certificate) (bool, error)
}

// CertEvent is one mTLS admission decision, emitted for the audit log
// (dual-cert rollover auditability per spec §9.7).
type CertEvent struct {
	At          time.Time
	SessionID   string // "" when no binding resolved
	Fingerprint string
	Path        MatchPath
	RemoteAddr  string
	Outcome     string // "accept" | "reject"
	Detail      string
}

// CertAudit receives every accept/reject decision.
type CertAudit func(CertEvent)

// TLSOptions builds a FIXS server configuration.
type TLSOptions struct {
	// Certificates is the server certificate chain (required).
	Certificates []tls.Certificate
	// ClientCAs verifies presented client certificates (required when
	// RequireClientCert).
	ClientCAs *x509.CertPool
	// RequireClientCert gates mutual auth: order-entry/admin listeners
	// set true (spec §9.7). When false the listener is TLS-only
	// (market data / pre-provisioned drop copy).
	RequireClientCert bool
	// Environment is this listener's deployment label; a binding whose
	// environment differs is rejected (a staging cert never opens a
	// production session).
	Environment string
	// Lookup resolves certificate fingerprints to session bindings.
	// Required when RequireClientCert — nil fails closed.
	Lookup BindingLookup
	// Revocation is the CRL/venue-revocation seam. Required when
	// RequireClientCert — nil fails closed.
	Revocation RevocationChecker
	// Now and Audit default to wall clock / no-op.
	Now   func() time.Time
	Audit CertAudit
}

func (o *TLSOptions) now() time.Time {
	if o.Now != nil {
		return o.Now()
	}
	return time.Now()
}

func (o *TLSOptions) audit(ev CertEvent) {
	ev.At = o.now()
	if o.Audit != nil {
		o.Audit(ev)
	}
}

// CertError is a coded TLS admission rejection. The code is a Text(58)-
// style token (the handshake dies before Logon — there is no FIX
// channel to report through; the audit event + log carry the detail).
type CertError struct {
	Code   string
	Detail string
}

func (e *CertError) Error() string { return e.Code + ": " + e.Detail }

// VerifyPeer implements the certificate admission decision — called
// from tls.Config.VerifyConnection after the standard chain build.
// Order: presented-cert sanity → fingerprint lookup → environment
// binding → CN/SAN match → revocation (stapled OCSP first, then the
// RevocationChecker seam). Any failure rejects before FIX Logon.
func (o *TLSOptions) VerifyPeer(ctx context.Context, state tls.ConnectionState) error {
	ev := CertEvent{Outcome: "reject"}
	defer func() { o.audit(ev) }()

	if len(state.PeerCertificates) == 0 {
		ev.Detail = "no client certificate presented"
		ev.Outcome = "reject"
		return &CertError{Code: "MTLS_CERT_REQUIRED", Detail: ev.Detail}
	}
	leaf := state.PeerCertificates[0]
	fp := CertFingerprint(leaf)
	ev.Fingerprint = fp

	now := o.now()
	if now.Before(leaf.NotBefore) || now.After(leaf.NotAfter) {
		ev.Detail = "certificate outside validity window"
		return &CertError{Code: "MTLS_CERT_EXPIRED", Detail: ev.Detail}
	}
	if o.Lookup == nil {
		ev.Detail = "binding lookup unavailable"
		return &CertError{Code: "MTLS_BINDING_UNAVAILABLE", Detail: ev.Detail}
	}
	binding, path, err := o.Lookup.BindingByFingerprint(ctx, fp)
	if err != nil {
		ev.Detail = fmt.Sprintf("binding lookup failed: %v", err)
		return &CertError{Code: "MTLS_BINDING_UNAVAILABLE", Detail: ev.Detail}
	}
	if binding == nil {
		ev.Detail = "certificate fingerprint not provisioned"
		return &CertError{Code: "MTLS_CERT_NOT_BOUND", Detail: ev.Detail}
	}
	ev.SessionID = binding.SessionID
	ev.Path = path
	// Rollover re-check with this listener's clock — the store query
	// already filtered on expiry, but the decision must not depend on
	// store-side time skew.
	if path == MatchRollover &&
		(binding.RolloverEndsAt == nil || !now.Before(*binding.RolloverEndsAt)) {
		ev.Detail = "rollover certificate presented after rotation window"
		return &CertError{Code: "MTLS_CERT_ROLLOVER_EXPIRED", Detail: ev.Detail}
	}
	if binding.Environment != o.Environment {
		ev.Detail = fmt.Sprintf("certificate bound to environment %q, listener is %q",
			binding.Environment, o.Environment)
		return &CertError{Code: "MTLS_ENV_MISMATCH", Detail: ev.Detail}
	}
	if binding.CN != "" && binding.CN != leaf.Subject.CommonName &&
		!hasSAN(leaf, binding.CN) {
		ev.Detail = fmt.Sprintf("subject CN/SAN does not match bound %q", binding.CN)
		return &CertError{Code: "MTLS_IDENTITY_MISMATCH", Detail: ev.Detail}
	}
	// Stapled OCSP: a presented staple that decodes wins over the
	// checker seam; a malformed staple is treated as revocation-status
	// unverifiable and rejects (fail closed).
	if len(state.OCSPResponse) > 0 {
		issuer := issuerOf(state)
		resp, perr := ocsp.ParseResponseForCert(state.OCSPResponse, leaf, issuer)
		if perr != nil {
			ev.Detail = fmt.Sprintf("stapled OCSP undecodable: %v", perr)
			return &CertError{Code: "MTLS_REVOCATION_UNVERIFIABLE", Detail: ev.Detail}
		}
		if resp.Status == ocsp.Revoked {
			ev.Detail = "stapled OCSP status revoked"
			return &CertError{Code: "MTLS_CERT_REVOKED", Detail: ev.Detail}
		}
		if now.After(resp.NextUpdate) && !resp.NextUpdate.IsZero() {
			ev.Detail = "stapled OCSP response stale"
			return &CertError{Code: "MTLS_REVOCATION_UNVERIFIABLE", Detail: ev.Detail}
		}
	}
	if o.Revocation == nil {
		ev.Detail = "revocation checker unavailable"
		return &CertError{Code: "MTLS_REVOCATION_UNVERIFIABLE", Detail: ev.Detail}
	}
	revoked, rerr := o.Revocation.Revoked(ctx, leaf, issuerOf(state))
	if rerr != nil {
		ev.Detail = fmt.Sprintf("revocation check failed: %v", rerr)
		return &CertError{Code: "MTLS_REVOCATION_UNVERIFIABLE", Detail: ev.Detail}
	}
	if revoked {
		ev.Detail = "certificate revoked"
		return &CertError{Code: "MTLS_CERT_REVOKED", Detail: ev.Detail}
	}

	ev.Outcome = "accept"
	return nil
}

func hasSAN(cert *x509.Certificate, want string) bool {
	for _, d := range cert.DNSNames {
		if d == want {
			return true
		}
	}
	for _, u := range cert.URIs {
		if u.String() == want {
			return true
		}
	}
	return false
}

func issuerOf(state tls.ConnectionState) *x509.Certificate {
	if len(state.PeerCertificates) > 1 {
		return state.PeerCertificates[1]
	}
	return nil
}

// ServerTLSConfig builds the FIXS TLS 1.3 server configuration. With
// RequireClientCert the handshake verifies the client chain AND runs
// the certificate→session binding via VerifyConnection — the peer is
// dead before a FIX byte is parsed.
func ServerTLSConfig(o TLSOptions) (*tls.Config, error) {
	if len(o.Certificates) == 0 {
		return nil, fmt.Errorf("fix: server certificate required")
	}
	cfg := &tls.Config{
		MinVersion:   tls.VersionTLS13,
		MaxVersion:   tls.VersionTLS13,
		Certificates: o.Certificates,
	}
	if !o.RequireClientCert {
		cfg.ClientAuth = tls.NoClientCert
		return cfg, nil
	}
	if o.ClientCAs == nil {
		return nil, fmt.Errorf("fix: mTLS listener requires ClientCAs pool")
	}
	cfg.ClientAuth = tls.RequireAndVerifyClientCert
	cfg.ClientCAs = o.ClientCAs
	cfg.VerifyConnection = func(cs tls.ConnectionState) error {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return o.VerifyPeer(ctx, cs)
	}
	return cfg, nil
}

// PeerFingerprint extracts the bound certificate fingerprint from a
// live connection — used by the Logon path to correlate the session's
// transport identity with the message-level CompID.
func PeerFingerprint(conn *tls.Conn) (string, bool) {
	state := conn.ConnectionState()
	if len(state.PeerCertificates) == 0 {
		return "", false
	}
	return CertFingerprint(state.PeerCertificates[0]), true
}
