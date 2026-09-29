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
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base32"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"hash"
	"net/url"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
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

// ===========================================================================
// Task 12.3.2 — 2FA enrollment lifecycle on top of the primitives above.
//
// Non-destructive re-enrollment (spec §12.2 / §8.1 F1):
//   - Setup/Enroll stages the candidate secret under Redis key
//     `2fa:pending:{user_id}` with a 10-minute TTL. The ACTIVE
//     users.totp_secret is never touched at stage time — an abandoned
//     enrollment leaves the live factor fully intact.
//   - Verify checks the submitted code against the staged candidate
//     (when present) and only then atomically swaps the active secret
//     and mints a fresh backup-code set. With no candidate staged, the
//     code is checked against the ACTIVE secret — that path is the
//     session step-up: the session's AMR gains "totp" and a re-issued
//     access token is returned so RequireTwoFactor passes immediately.
//
// Backup codes: 10 single-use recovery codes per enrollment. Only the
// sha256 digest of the normalized code is persisted
// (users.totp_backup_codes TEXT[]) — the plaintext is returned exactly
// once, in the verify response.
// ===========================================================================

// TOTPBackupCodeCount is the spec §12.2 recovery-code set size.
const TOTPBackupCodeCount = 10

// TOTPSetup is the setup/enroll response — the candidate secret and its
// otpauth URI. ExpiresInSec is the staged-candidate lifetime.
type TOTPSetup struct {
	Secret       string
	URI          string
	ExpiresInSec int
}

// TOTPVerifyResult reports what verify accomplished. Exactly one of
// Activated (candidate promoted; BackupCodes populated — shown once) or
// Elevated (code verified against the live secret; session AMR gained
// "totp") is true. AccessToken/ExpiresAt carry a re-minted JWT that
// already bears the totp AMR so subsequent gated calls pass.
type TOTPVerifyResult struct {
	Activated   bool
	Elevated    bool
	BackupCodes []string
	AccessToken string
	ExpiresAt   time.Time
}

// TwoFactorService orchestrates enrollment. users persists the active
// secret + backup digests; cache stages candidates; sessions performs
// the post-verify AMR elevation.
type TwoFactorService struct {
	users    *UserStore
	cache    TokenCache
	sessions *SessionManager
	issuer   string // otpauth issuer label
	now      func() time.Time
}

// NewTwoFactorService binds the orchestrator (all deps mandatory — a
// half-wired 2FA surface must fail closed at construction).
func NewTwoFactorService(users *UserStore, cache TokenCache,
	sessions *SessionManager, issuer string) (*TwoFactorService, error) {
	if users == nil || cache == nil || sessions == nil {
		return nil, newError(CodeAuthInternal,
			"2fa service requires users/cache/sessions")
	}
	if issuer == "" {
		issuer = "exc.local"
	}
	return &TwoFactorService{users: users, cache: cache, sessions: sessions,
		issuer: issuer, now: time.Now}, nil
}

// pendingKey is the spec §12.2 candidate-secret key.
func pendingKey(userID int64) string {
	return "2fa:pending:" + strconv.FormatInt(userID, 10)
}

// Setup generates a candidate secret and stages it under
// 2fa:pending:{user_id} for TOTPPendingTTL. The active secret is never
// touched — this is safe to call on an already-enrolled user (re-key).
// POST /api/v1/auth/2fa/setup and …/enroll share this implementation.
func (s *TwoFactorService) Setup(ctx context.Context, userID int64, accountLabel string) (*TOTPSetup, error) {
	secret, err := GenerateTOTPSecret()
	if err != nil {
		return nil, err
	}
	if err := s.cache.Set(ctx, pendingKey(userID), secret, TOTPPendingTTL); err != nil {
		return nil, err
	}
	if accountLabel == "" {
		accountLabel = strconv.FormatInt(userID, 10)
	}
	return &TOTPSetup{
		Secret:       secret,
		URI:          TOTPURI(s.issuer, accountLabel, secret),
		ExpiresInSec: int(TOTPPendingTTL / time.Second),
	}, nil
}

// Verify admits a TOTP code. Candidate staged → promote it (activate
// 2FA / re-key) and mint a fresh backup-code set. No candidate → the
// code must match the ACTIVE secret, elevating the session AMR so
// RequireTwoFactor passes for this session. A bad code is
// TWO_FACTOR_REQUIRED in every case — no oracle on which path ran.
func (s *TwoFactorService) Verify(ctx context.Context, userID int64,
	sessionID, code string) (*TOTPVerifyResult, error) {

	code = strings.TrimSpace(code)
	if code == "" {
		return nil, newError(CodeTwoFactorRequired, "totp code required")
	}
	candidate, staged, err := s.cache.Get(ctx, pendingKey(userID))
	if err != nil {
		return nil, err
	}
	var secret string
	activate := false
	if staged {
		secret, activate = candidate, true
	} else {
		secret, err = s.users.TOTPSecretForUser(ctx, userID)
		if err != nil {
			return nil, err
		}
		if secret == "" {
			return nil, newError(CodeTwoFactorRequired,
				"no 2FA enrollment pending")
		}
	}
	if !VerifyTOTP(secret, code, s.now().UTC()) {
		return nil, newError(CodeTwoFactorRequired, "invalid 2FA code")
	}
	var backups []string
	if activate {
		// Candidate verified — now (and only now) the active secret is
		// replaced and a fresh backup set is minted.
		codes, digests, err := GenerateBackupCodes()
		if err != nil {
			return nil, err
		}
		if err := s.users.SetTOTPSecret(ctx, userID, secret, digests); err != nil {
			return nil, err
		}
		if err := s.cache.Delete(ctx, pendingKey(userID)); err != nil {
			return nil, err
		}
		backups = codes
	}
	res := &TOTPVerifyResult{Activated: activate, Elevated: !activate,
		BackupCodes: backups}
	if sessionID != "" {
		if _, err := s.sessions.ElevateAMR(ctx, sessionID, "totp"); err != nil {
			return nil, err
		}
		// Re-mint the access JWT so the totp AMR + two_factor_verified
		// are claim-visible now — waiting for the next refresh would
		// stall gated calls for up to the 15-minute token TTL.
		tok, exp, err := s.sessions.ReissueAccess(ctx, sessionID, userSessionScopes)
		if err != nil {
			return nil, err
		}
		res.AccessToken, res.ExpiresAt = tok, exp
	}
	return res, nil
}

// Disable requires BOTH the account password and a live factor (TOTP or
// a backup code) before the secret is cleared — spec §12.2. Every
// session is revoked on success: the credential model changed, all
// outstanding tokens must re-auth.
func (s *TwoFactorService) Disable(ctx context.Context, userID int64,
	password, code string) error {

	u, err := s.users.UserByID(ctx, userID)
	if err != nil {
		return err
	}
	if u == nil || !u.TOTPEnabled() {
		return newError(CodeInvalidRequest, "2FA is not enabled")
	}
	if u.PasswordHash == "" ||
		bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(password)) != nil {
		return newError(CodeInvalidCredentials, "current password incorrect")
	}
	ok, err := s.VerifyFactor(ctx, userID, code)
	if err != nil {
		return err
	}
	if !ok {
		return newError(CodeTwoFactorRequired, "invalid 2FA code")
	}
	if err := s.users.ClearTOTP(ctx, userID); err != nil {
		return err
	}
	return revokeAllUserSessions(ctx, s.users, s.sessions, userID)
}

// VerifyFactor admits a live TOTP code OR a single-use backup code
// against the user's ACTIVE secret — the shared gate used by the
// disable flow and by account-level step-up checks. It never consults
// staged candidates (a pending secret is not yet a real factor).
func (s *TwoFactorService) VerifyFactor(ctx context.Context, userID int64, code string) (bool, error) {
	code = strings.TrimSpace(code)
	if code == "" {
		return false, nil
	}
	secret, err := s.users.TOTPSecretForUser(ctx, userID)
	if err != nil {
		return false, err
	}
	if secret != "" && VerifyTOTP(secret, code, s.now().UTC()) {
		return true, nil
	}
	return s.users.ConsumeBackupCode(ctx, userID, BackupCodeDigest(code))
}

// HasPending reports whether a candidate secret is staged (profile
// surface shows "enrollment in progress" without exposing the seed).
func (s *TwoFactorService) HasPending(ctx context.Context, userID int64) (bool, error) {
	_, staged, err := s.cache.Get(ctx, pendingKey(userID))
	return staged, err
}

// ---------------------------------------------------------------------------
// Backup codes — 10 single-use "XXXX-XXXX" recovery codes; only sha256
// digests of the normalized form are persisted.
// ---------------------------------------------------------------------------

// GenerateBackupCodes returns the plaintext display set (returned to the
// user exactly once) and the digest set persisted on the user row.
func GenerateBackupCodes() (codes []string, digests []string, err error) {
	codes = make([]string, TOTPBackupCodeCount)
	digests = make([]string, TOTPBackupCodeCount)
	for i := range codes {
		raw := make([]byte, 5) // 40 bits → 8 base32 chars
		if _, err := randRead(raw); err != nil {
			return nil, nil, wrapError(CodeAuthInternal, "backup code generation", err)
		}
		b := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(raw)
		codes[i] = b[:4] + "-" + b[4:]
		digests[i] = BackupCodeDigest(codes[i])
	}
	return codes, digests, nil
}

// BackupCodeDigest is the persisted form of a recovery code: sha256 hex
// of the normalized code (uppercase, separators stripped). Comparing
// digests never reveals the code and the DB copy is not a usable
// credential.
func BackupCodeDigest(code string) string {
	norm := strings.Map(func(r rune) rune {
		switch {
		case r == '-' || r == ' ':
			return -1
		case r >= 'a' && r <= 'z':
			return r - 32
		default:
			return r
		}
	}, code)
	sum := sha256.Sum256([]byte(norm))
	return hex.EncodeToString(sum[:])
}
