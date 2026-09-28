// Unit tests for RFC 6238 TOTP (Task 5.3.1 item 3) — RFC 6238 Appendix B
// test vectors (8-digit codes, 30s step, ASCII seeds).
package auth

import (
	"testing"
	"time"
)

// RFC 6238 Appendix B vectors.
func TestTOTPRFC6238Vectors(t *testing.T) {
	cases := []struct {
		seedASCII string
		alg       TOTPAlgorithm
		at        int64 // unix seconds
		want      string
	}{
		{"12345678901234567890", TOTPSHA1, 59, "94287082"},
		{"12345678901234567890", TOTPSHA1, 1111111109, "07081804"},
		{"12345678901234567890", TOTPSHA1, 1111111111, "14050471"},
		{"12345678901234567890", TOTPSHA1, 1234567890, "89005924"},
		{"12345678901234567890", TOTPSHA1, 2000000000, "69279037"},
		{"12345678901234567890", TOTPSHA1, 20000000000, "65353130"},
		{"12345678901234567890123456789012", TOTPSHA256, 59, "46119246"},
		{"12345678901234567890123456789012", TOTPSHA256, 1111111109, "68084774"},
		{"12345678901234567890123456789012", TOTPSHA256, 2000000000, "90698825"},
		{"1234567890123456789012345678901234567890123456789012345678901234", TOTPSHA512, 59, "90693936"},
		{"1234567890123456789012345678901234567890123456789012345678901234", TOTPSHA512, 1111111111, "99943326"},
		{"1234567890123456789012345678901234567890123456789012345678901234", TOTPSHA512, 20000000000, "47863826"},
	}
	for _, c := range cases {
		got, err := TOTPCode([]byte(c.seedASCII), time.Unix(c.at, 0), 30*time.Second, 8, c.alg)
		if err != nil {
			t.Fatalf("TOTPCode(%d): %v", c.at, err)
		}
		if got != c.want {
			t.Fatalf("TOTPCode(alg=%d t=%d) = %s, want %s", c.alg, c.at, got, c.want)
		}
	}
}

func TestTOTPVerifyDriftWindow(t *testing.T) {
	secret, err := GenerateTOTPSecret()
	if err != nil {
		t.Fatalf("gen: %v", err)
	}
	raw, err := DecodeTOTPSecret(secret)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	now := time.Now()
	code, err := TOTPCode(raw, now, TOTPPeriod, TOTPDigits, TOTPSHA1)
	if err != nil {
		t.Fatalf("code: %v", err)
	}
	if !VerifyTOTP(secret, code, now) {
		t.Fatal("current code must verify")
	}
	// ±1 step drift tolerated (venue profile window).
	if !VerifyTOTP(secret, code, now.Add(TOTPPeriod)) {
		t.Fatal("code must verify within +1 step")
	}
	if !VerifyTOTP(secret, code, now.Add(-TOTPPeriod)) {
		t.Fatal("code must verify within -1 step")
	}
	if VerifyTOTP(secret, code, now.Add(4*TOTPPeriod)) {
		t.Fatal("code must NOT verify beyond ±1 step")
	}
	if VerifyTOTP(secret, "000000", now) && code != "000000" {
		t.Fatal("wrong code must not verify")
	}
	if VerifyTOTP("!!not-base32!!", code, now) {
		t.Fatal("malformed secret must not verify")
	}
}

func TestTOTPSecretRoundTrip(t *testing.T) {
	s, err := GenerateTOTPSecret()
	if err != nil {
		t.Fatalf("gen: %v", err)
	}
	raw, err := DecodeTOTPSecret(s)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(raw) != TOTPSecretBytes {
		t.Fatalf("secret size %d, want %d", len(raw), TOTPSecretBytes)
	}
	// lowercase tolerated
	if _, err := DecodeTOTPSecret(s[:len(s)-1] + "x"); err != nil {
		// last char replaced with lowercase 'x' — still valid base32
		t.Fatalf("lowercase decode: %v", err)
	}
}

func TestTOTPURI(t *testing.T) {
	u := TOTPURI("Exchange", "user@example.com", "JBSWY3DPEHPK3PXP")
	if u == "" || u[:15] != "otpauth://totp/" {
		t.Fatalf("bad uri: %s", u)
	}
}
