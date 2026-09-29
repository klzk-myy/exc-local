// Unit tests for Task 12.3.1/12.3.2 — validation, token staging,
// backup codes, and fail-closed construction. The PG/Redis-backed
// lifecycle coverage is registration_integration_test.go (EXC_PG_TEST).
package auth

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// fake plumbing for unit-level service tests
// ---------------------------------------------------------------------------

// memTokenCache is the in-memory TokenCache for unit tests (TTLs are
// evaluated lazily against now()).
type memTokenCache struct {
	m    map[string]memEntry
	now  func() time.Time
	sets []string // key order for assertions
}
type memEntry struct {
	val string
	exp time.Time
}

func newMemTokenCache() *memTokenCache {
	return &memTokenCache{m: map[string]memEntry{}, now: time.Now}
}

func (c *memTokenCache) live(key string) (memEntry, bool) {
	e, ok := c.m[key]
	if !ok || c.now().After(e.exp) {
		return memEntry{}, false
	}
	return e, true
}

func (c *memTokenCache) Set(_ context.Context, key, val string, ttl time.Duration) error {
	c.m[key] = memEntry{val: val, exp: c.now().Add(ttl)}
	c.sets = append(c.sets, key)
	return nil
}

func (c *memTokenCache) Take(_ context.Context, key string) (string, bool, error) {
	e, ok := c.live(key)
	if !ok {
		return "", false, nil
	}
	delete(c.m, key)
	return e.val, true, nil
}

func (c *memTokenCache) Get(_ context.Context, key string) (string, bool, error) {
	e, ok := c.live(key)
	return e.val, ok, nil
}

func (c *memTokenCache) Delete(_ context.Context, key string) error {
	delete(c.m, key)
	return nil
}

// captureSender is a recording Sender for unit tests.
type captureSender struct{ msgs []Message }

func (s *captureSender) Send(_ context.Context, m Message) error {
	s.msgs = append(s.msgs, m)
	return nil
}

// ---------------------------------------------------------------------------
// validation
// ---------------------------------------------------------------------------

func TestNormalizeEmail(t *testing.T) {
	if got := normalizeEmail("  Alice@Example.COM "); got != "alice@example.com" {
		t.Fatalf("normalizeEmail = %q", got)
	}
}

func TestValidatePasswordPolicy(t *testing.T) {
	for _, pw := range []string{"", "short", "elevenchars"} {
		if err := validatePassword(pw); err == nil {
			t.Fatalf("password %q accepted", pw)
		}
	}
	if err := validatePassword(strings.Repeat("x", PasswordMaxLen+1)); err == nil {
		t.Fatal("over-72-byte password accepted")
	}
	if err := validatePassword("a-very-reasonable-passphrase"); err != nil {
		t.Fatalf("good password rejected: %v", err)
	}
}

func TestValidateCountry(t *testing.T) {
	if c, err := validateCountry("us"); err != nil || c != "US" {
		t.Fatalf("country normalize failed: %q %v", c, err)
	}
	for _, bad := range []string{"", "USA", "U1", "united states"} {
		if _, err := validateCountry(bad); err == nil {
			t.Fatalf("country %q accepted", bad)
		}
	}
}

// ---------------------------------------------------------------------------
// token staging
// ---------------------------------------------------------------------------

func TestTokenKeyIsDigestOnly(t *testing.T) {
	k1 := tokenKey("pwdreset:", "tok-A")
	k2 := tokenKey("pwdreset:", "tok-A")
	if k1 != k2 {
		t.Fatal("tokenKey not deterministic")
	}
	if strings.Contains(k1, "tok-A") {
		t.Fatal("tokenKey leaks the raw token")
	}
	if !strings.HasPrefix(k1, "pwdreset:") {
		t.Fatalf("prefix lost: %q", k1)
	}
	if tokenKey("verify_email:", "tok-A") == k1 {
		t.Fatal("namespaces collide")
	}
}

func TestPendingKeyShape(t *testing.T) {
	if got := pendingKey(42); got != "2fa:pending:42" {
		t.Fatalf("pendingKey = %q, want 2fa:pending:42", got)
	}
}

// ---------------------------------------------------------------------------
// backup codes
// ---------------------------------------------------------------------------

func TestBackupCodesShapeAndDigest(t *testing.T) {
	codes, digests, err := GenerateBackupCodes()
	if err != nil {
		t.Fatal(err)
	}
	if len(codes) != TOTPBackupCodeCount || len(digests) != TOTPBackupCodeCount {
		t.Fatalf("counts: %d codes %d digests", len(codes), len(digests))
	}
	seen := map[string]bool{}
	for i, c := range codes {
		if len(c) != 9 || c[4] != '-' {
			t.Fatalf("code %d malformed: %q", i, c)
		}
		if seen[c] {
			t.Fatalf("duplicate code %q", c)
		}
		seen[c] = true
		if BackupCodeDigest(c) != digests[i] {
			t.Fatalf("digest mismatch for code %d", i)
		}
	}
	// Normalization: lowercase + no separator digests identically.
	if BackupCodeDigest(strings.ToLower(strings.ReplaceAll(codes[0], "-", ""))) != digests[0] {
		t.Fatal("digest normalization broken")
	}
	if BackupCodeDigest("AAAA-AAAA") == digests[0] {
		t.Fatal("different code, same digest")
	}
}

// ---------------------------------------------------------------------------
// construction fail-closed
// ---------------------------------------------------------------------------

func TestAuthnServiceNilDepsFailClosed(t *testing.T) {
	if _, err := NewAuthnService(nil, nil, nil, nil); err == nil {
		t.Fatal("nil deps accepted")
	}
	// A nil mailer alone must fail — verification/reset links would
	// silently vanish otherwise.
	m, _, _ := testManager(t, SessionConfig{})
	if _, err := NewAuthnService(&UserStore{}, m, newMemTokenCache(), nil); err == nil {
		t.Fatal("nil mailer accepted")
	}
}

func TestTwoFactorServiceNilDepsFailClosed(t *testing.T) {
	if _, err := NewTwoFactorService(nil, nil, nil, "exc"); err == nil {
		t.Fatal("nil deps accepted")
	}
}

// ---------------------------------------------------------------------------
// LogSender
// ---------------------------------------------------------------------------

func TestLogSenderDelivers(t *testing.T) {
	var got string
	s := NewLogSender(func(f string, a ...any) { got = fmt.Sprintf(f, a...) })
	if err := s.Send(context.Background(), Message{
		To: "u@x.io", Kind: MailEmailVerification, Subject: "s", Body: "b",
	}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "u@x.io") {
		t.Fatalf("log line missing recipient: %q", got)
	}
}
