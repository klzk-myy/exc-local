// Task 5.3.1 item 3 — TOTP (RFC 6238) second factor for sensitive
// operations (withdrawals, balance adjustments per the task AC; the
// gated-action wiring lands with the owning handlers).
//
// Profile: HMAC-SHA1, 30s period, 6 digits — the interoperable
// authenticator-app profile. Verification accepts ±1 step of clock drift
// and compares codes digit-by-digit in constant time (hmac.Equal over the
// decimal strings).
package auth

import (
	"crypto/hmac"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base32"
	"encoding/binary"
	"fmt"
	"hash"
	"net/url"
	"strings"
	"time"
)

const (
	// TOTPPeriod is the RFC 6238 time step for the venue profile.
	TOTPPeriod = 30 * time.Second
	// TOTPDigits is the code length for the venue profile.
	TOTPDigits = 6
	// TOTPSecretBytes is 160 bits — the RFC 4226 recommended minimum.
	TOTPSecretBytes = 20
)

// TOTPAlgorithm selects the HMAC hash. SHA1 is the deployment profile;
// SHA256/SHA512 exist for RFC 6238 test vectors and future hardening.
type TOTPAlgorithm int

const (
	TOTPSHA1 TOTPAlgorithm = iota
	TOTPSHA256
	TOTPSHA512
)

func (a TOTPAlgorithm) hash() func() hash.Hash {
	switch a {
	case TOTPSHA256:
		return sha256.New
	case TOTPSHA512:
		return sha512.New
	default:
		return sha1.New
	}
}

// GenerateTOTPSecret returns a fresh base32 (unpadded) TOTP seed suitable
// for otpauth:// URIs and for staging per spec §8.1 F1 (pending secret in
// `2fa:pending:{user_id}`, never overwriting the active secret until the
// verify step succeeds).
func GenerateTOTPSecret() (string, error) {
	b := make([]byte, TOTPSecretBytes)
	if _, err := randRead(b); err != nil {
		return "", wrapError(CodeAuthInternal, "totp secret generation", err)
	}
	return base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b), nil
}

// DecodeTOTPSecret parses a base32 seed (padding optional, lowercase
// tolerated) into raw bytes.
func DecodeTOTPSecret(s string) ([]byte, error) {
	s = strings.ToUpper(strings.TrimSpace(s))
	if rem := len(s) % 8; rem != 0 {
		s += strings.Repeat("=", 8-rem)
	}
	b, err := base32.StdEncoding.DecodeString(s)
	if err != nil {
		return nil, newError(CodeAPIKeyInvalid, "malformed TOTP secret")
	}
	return b, nil
}

// TOTPCode computes the n-digit code for secret at instant t.
// secret is raw bytes (see DecodeTOTPSecret).
func TOTPCode(secret []byte, t time.Time, period time.Duration, digits int, alg TOTPAlgorithm) (string, error) {
	if period <= 0 || digits <= 0 || digits > 10 {
		return "", newError(CodeAPIKeyInvalid, "bad totp parameters")
	}
	if len(secret) < 10 {
		return "", newError(CodeAPIKeyInvalid, "totp secret too short")
	}
	counter := uint64(t.Unix() / int64(period/time.Second))
	var msg [8]byte
	binary.BigEndian.PutUint64(msg[:], counter)
	mac := hmac.New(alg.hash(), secret)
	mac.Write(msg[:])
	sum := mac.Sum(nil)
	offset := sum[len(sum)-1] & 0x0f
	binCode := (uint32(sum[offset])&0x7f)<<24 |
		(uint32(sum[offset+1])&0xff)<<16 |
		(uint32(sum[offset+2])&0xff)<<8 |
		(uint32(sum[offset+3]) & 0xff)
	mod := uint32(1)
	for i := 0; i < digits; i++ {
		mod *= 10
	}
	return fmt.Sprintf("%0*d", digits, binCode%mod), nil
}

// VerifyTOTP checks code against the venue profile (SHA1/30s/6 digits)
// with ±1 period of drift tolerance. Returns true only on a match;
// malformed input is false, never an error — verification is a boolean
// gate, the caller decides between TWO_FACTOR_REQUIRED and rejection.
func VerifyTOTP(secret string, code string, at time.Time) bool {
	raw, err := DecodeTOTPSecret(secret)
	if err != nil {
		return false
	}
	return verifyTOTP(raw, code, at, TOTPPeriod, TOTPDigits, TOTPSHA1, 1)
}

// verifyTOTP is the parameterized core (also used by tests for the RFC
// 6238 SHA256/SHA512 vectors). skew is the number of adjacent steps
// accepted on each side of the current step.
func verifyTOTP(secret []byte, code string, at time.Time, period time.Duration, digits int, alg TOTPAlgorithm, skew int) bool {
	if code == "" {
		return false
	}
	for step := -skew; step <= skew; step++ {
		want, err := TOTPCode(secret, at.Add(time.Duration(step)*period), period, digits, alg)
		if err != nil {
			return false
		}
		// Constant-time compare over the decimal code: equal length
		// strings only, so length mismatch fast-fails without leaking.
		if len(want) == len(code) && hmac.Equal([]byte(want), []byte(code)) {
			return true
		}
	}
	return false
}

// TOTPURI builds an otpauth:// URI for authenticator enrolment
// (issuer/account shown in the app; the secret is the only sensitive
// component — callers must never log the URI).
func TOTPURI(issuer, account, secret string) string {
	v := url.Values{}
	v.Set("secret", secret)
	v.Set("issuer", issuer)
	v.Set("algorithm", "SHA1")
	v.Set("digits", fmt.Sprintf("%d", TOTPDigits))
	v.Set("period", fmt.Sprintf("%d", int(TOTPPeriod/time.Second)))
	return "otpauth://totp/" + url.PathEscape(issuer) + ":" + url.PathEscape(account) + "?" + v.Encode()
}
