// Unit tests for the JWT issuer/verifier (Task 5.3.1).
package auth

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"strings"
	"testing"
	"time"
)

func testIssuer(t *testing.T) *Issuer {
	t.Helper()
	i := NewIssuer("exc-test", "exc-api", 15*time.Minute)
	if err := i.AddHMACKey("k1", []byte(strings.Repeat("a", 32)), true); err != nil {
		t.Fatalf("add key: %v", err)
	}
	return i
}

func TestJWTRoundTrip(t *testing.T) {
	i := testIssuer(t)
	tok, claims, err := i.Issue("user-7", IssueOptions{
		AccountID: 42, SessionID: "sess-1", Scopes: []string{"read", "trade"},
		AMR: []string{"pwd", "totp"},
	})
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	if !claims.ExpiresAt.Equal(claims.IssuedAt.Add(15 * time.Minute)) {
		t.Fatalf("access TTL must be 15min (spec §8.1), got %v", claims.ExpiresAt.Sub(claims.IssuedAt))
	}
	got, err := i.Parse(tok)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got.Subject != "user-7" || got.AccountID != 42 || got.SessionID != "sess-1" {
		t.Fatalf("claims mismatch: %+v", got)
	}
	if !got.HasScope("trade") || got.HasScope("admin") {
		t.Fatalf("scope round-trip wrong: %v", got.Scopes)
	}
	if !got.TwoFactorVerified() {
		t.Fatalf("amr totp must satisfy TwoFactorVerified: %v", got.AMR)
	}
	if got.KeyID != "k1" {
		t.Fatalf("kid not propagated: %q", got.KeyID)
	}
}

func TestJWTExpiredRejected(t *testing.T) {
	i := testIssuer(t)
	i.now = func() time.Time { return time.Now().Add(-time.Hour) }
	tok, _, err := i.Issue("user-7", IssueOptions{})
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	i.now = time.Now
	if _, err := i.Parse(tok); err == nil {
		t.Fatal("expired token must reject")
	} else if !codeIs(err, CodeUnauthorized) {
		t.Fatalf("expired token → %v, want UNAUTHORIZED", err)
	}
}

func TestJWTWrongKeyRejected(t *testing.T) {
	i := testIssuer(t)
	tok, _, err := i.Issue("user-7", IssueOptions{})
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	other := NewIssuer("exc-test", "exc-api", 0)
	if err := other.AddHMACKey("k1", []byte(strings.Repeat("b", 32)), true); err != nil {
		t.Fatalf("add key: %v", err)
	}
	if _, err := other.Parse(tok); err == nil {
		t.Fatal("token signed with different secret must reject")
	}
}

func TestJWTUnknownKIDRejected(t *testing.T) {
	i := testIssuer(t)
	tok, _, err := i.Issue("user-7", IssueOptions{})
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	empty := NewIssuer("exc-test", "exc-api", 0)
	if _, err := empty.Parse(tok); err == nil {
		t.Fatal("unknown kid must reject")
	}
}

func TestJWTAlgConfusionRejected(t *testing.T) {
	// A token claiming kid=k1 but alg=none must never verify — the
	// keyfunc pins each kid to its registered algorithm.
	i := testIssuer(t)
	// craft: header {alg:none,kid:k1}, payload minimal — sign manually
	header := `{"alg":"none","typ":"JWT","kid":"k1"}`
	payload := `{"sub":"u","typ":"access","exp":` +
		"9999999999" + `}`
	raw := base64url(header) + "." + base64url(payload) + ".x"
	if _, err := i.Parse(raw); err == nil {
		t.Fatal("alg=none token must reject")
	}
}

func TestJWTRotationOverlap(t *testing.T) {
	i := NewIssuer("exc-test", "exc-api", 0)
	if err := i.AddHMACKey("old", []byte(strings.Repeat("o", 32)), true); err != nil {
		t.Fatal(err)
	}
	oldTok, _, err := i.Issue("user-7", IssueOptions{})
	if err != nil {
		t.Fatalf("issue old: %v", err)
	}
	// Rotate: new key becomes active, old stays verify-capable (§8.8).
	if err := i.AddHMACKey("new", []byte(strings.Repeat("n", 32)), true); err != nil {
		t.Fatal(err)
	}
	newTok, c, err := i.Issue("user-7", IssueOptions{})
	if err != nil {
		t.Fatalf("issue new: %v", err)
	}
	if c.KeyID != "new" {
		t.Fatalf("active kid should be 'new', got %q", c.KeyID)
	}
	if _, err := i.Parse(oldTok); err != nil {
		t.Fatalf("pre-rotation token must still verify: %v", err)
	}
	if _, err := i.Parse(newTok); err != nil {
		t.Fatalf("post-rotation token must verify: %v", err)
	}
}

func TestJWTRS256AndEdDSA(t *testing.T) {
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa gen: %v", err)
	}
	edPub, edPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("ed25519 gen: %v", err)
	}
	i := NewIssuer("exc-test", "exc-api", 0)
	if err := i.AddRSAKey("rsa1", rsaKey, nil, false); err != nil {
		t.Fatal(err)
	}
	if err := i.AddEd25519Key("ed1", edPriv, edPub, false); err != nil {
		t.Fatal(err)
	}
	// Activate each signing key in turn.
	i.activeKID = "rsa1"
	tok, _, err := i.Issue("oauth2:oc_x", IssueOptions{ClientID: "oc_x"})
	if err != nil {
		t.Fatalf("rsa issue: %v", err)
	}
	got, err := i.Parse(tok)
	if err != nil || got.ClientID != "oc_x" {
		t.Fatalf("rsa roundtrip: %v %+v", err, got)
	}
	i.activeKID = "ed1"
	tok, _, err = i.Issue("u", IssueOptions{})
	if err != nil {
		t.Fatalf("ed issue: %v", err)
	}
	if _, err := i.Parse(tok); err != nil {
		t.Fatalf("ed roundtrip: %v", err)
	}
}

func TestJWTWeakSecretRejected(t *testing.T) {
	i := NewIssuer("", "", 0)
	if err := i.AddHMACKey("k", []byte("short"), true); err == nil {
		t.Fatal("HS256 secret <32 bytes must be rejected")
	}
}

// base64url encodes raw JSON for hand-crafted tokens.
func base64url(s string) string {
	return b64urlEncode([]byte(s))
}
