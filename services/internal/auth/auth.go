// Package auth implements the Phase-05 authentication cluster:
//
//   - Task 5.3.1  — JWT access tokens (15min) + refresh (7d) + OAuth2
//     client-credentials grant + TOTP 2FA + Redis session storage
//   - Task 5.3.9  — per-token IP allowlists (token.go)
//   - Task 5.3.10 — session lifecycle with concurrent-session policy
//     (session.go)
//   - Task 5.3.38 — Ed25519/RSA programmatic API keys (apikey.go,
//     signing.go)
//
// Canonical contract: spec §8.1 (auth surfaces), §8.7 (signed-request
// error codes), §8.8 item 3 (auth/session/key lifecycle — kid rotation,
// refresh rotation + reuse detection, idle 30min / absolute 8h, oldest-
// first eviction at 5/account and 20/IP, scope matrix, 90-day Ed25519
// rotation), §4.1 (session:{token} Redis hash, 3600s TTL).
package auth

import (
	"time"
)

// Claims is the authenticated identity attached to a request, resolved
// from a verified JWT (jwt.go) or an API-key signed request (signing.go).
type Claims struct {
	Subject   string    // stable user identity (token "sub"); "oauth2:{client_id}" for client grants
	AccountID int64     // trading account context, 0 = none selected
	Scopes    []string  // granted scopes (read/trade/transfer/admin matrix, §8.8)
	SessionID string    // bound session id (sid), empty for sessionless grants
	AMR       []string  // authentication methods references ("pwd","totp","fido2" — §12.6)
	ClientID  string    // OAuth2 client id when grant_type=client_credentials
	TokenType string    // "access" — reserved for future token classes
	KeyID     string    // kid used to verify this token (rotation forensics)
	IssuedAt  time.Time // iat
	ExpiresAt time.Time // exp
}

// HasScope reports whether the claims grant the given scope.
func (c Claims) HasScope(scope string) bool {
	for _, s := range c.Scopes {
		if s == scope {
			return true
		}
	}
	return false
}

// TwoFactorVerified reports whether the session already satisfies the
// 2FA requirement for sensitive operations (TOTP verify or FIDO2
// assertion elevation, spec §12.6/§8.1 remediation F2).
func (c Claims) TwoFactorVerified() bool {
	for _, m := range c.AMR {
		if m == "totp" || m == "fido2" {
			return true
		}
	}
	return false
}

// Claims attach/detach helpers live in context.go (WithClaims /
// ClaimsFrom) — shared with the other Phase-05 auth surfaces.
