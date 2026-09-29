// Error codes emitted by the auth cluster (Tasks 5.3.1, 5.3.9, 5.3.10,
// 5.3.38). Codes marked §23 are already registered in the spec error-code
// registry; anything new must be registered via Phase-05 Task 5.3.21.
package auth

import (
	stderrors "errors"
	"net/http"

	excerrors "exchange/pkg/errors"
)

const (
	// §23 registered codes.
	CodeUnauthorized           = "UNAUTHORIZED"                 // 401 — missing/invalid auth token
	CodeInvalidCredentials     = "INVALID_CREDENTIALS"          // 401 — bad login/client-credentials pair
	CodeTokenIPForbidden       = "TOKEN_IP_FORBIDDEN"           // 403 — API token IP mismatch
	CodeInsufficientScope      = "INSUFFICIENT_SCOPE"           // 403 — key lacks endpoint scope
	CodeInvalidSignature       = "INVALID_SIGNATURE"            // 401 — request signature mismatch
	CodeTimestampOutOfWindow   = "TIMESTAMP_OUT_OF_WINDOW"      // 401 — >30s timestamp skew
	CodeReplayAttackDetected   = "REPLAY_ATTACK_DETECTED"       // 401 — signature reuse in window
	CodeAsymmetricKeyInvalid   = "ASYMMETRIC_KEY_INVALID"       // 401 — Ed25519/RSA key or signature invalid
	CodeTwoFactorRequired      = "TWO_FACTOR_REQUIRED"          // 403 — TOTP needed for sensitive op
	CodeAccountLockedAuthFails = "ACCOUNT_LOCKED_AUTH_FAILURES" // 423 — 5 consecutive auth failures
	CodeWebAuthnFailed         = "WEBAUTHN_VERIFICATION_FAILED" // 401 — WebAuthn signature/counter check failed (§23, Phase-12)

	// Session/key lifecycle codes pending Task 5.3.21 registration.
	CodeSessionExpired     = "SESSION_EXPIRED"      // 401 — idle or absolute session timeout
	CodeSessionRevoked     = "SESSION_REVOKED"      // 401 — explicit/logout/all-device revocation
	CodeAPIKeyNotFound     = "API_KEY_NOT_FOUND"    // 401 — unknown/revoked/expired key_id
	CodeAPIKeyInvalid      = "API_KEY_INVALID"      // 400 — malformed key registration request
	CodeOAuthClientInvalid = "OAUTH_CLIENT_INVALID" // 400 — bad client registration request
	CodeAuthInternal       = "AUTH_INTERNAL"        // 500 — auth subsystem failure (fail-closed)

	// Shared §23 codes referenced by auth services (already registered
	// in errs.Default — declared here so auth code stops borrowing
	// semantically-wrong cluster codes for request validation).
	CodeForbidden      = "FORBIDDEN"       // 403 — generic deny
	CodeInvalidRequest = "INVALID_REQUEST" // 400 — malformed request
)

// authHTTPStatus maps auth-cluster codes to their spec §23 HTTP status.
// Unknown codes map to 500 — an unclassified failure must never be
// reported to the client as a client fault (fail-closed, spec §2.7.1).
func authHTTPStatus(code string) int {
	switch code {
	case CodeUnauthorized, CodeInvalidCredentials, CodeInvalidSignature,
		CodeTimestampOutOfWindow, CodeReplayAttackDetected,
		CodeAsymmetricKeyInvalid, CodeSessionExpired, CodeSessionRevoked,
		CodeAPIKeyNotFound, CodeWebAuthnFailed:
		return http.StatusUnauthorized
	case CodeTokenIPForbidden, CodeInsufficientScope, CodeTwoFactorRequired:
		return http.StatusForbidden
	case CodeAPIKeyInvalid, CodeOAuthClientInvalid:
		return http.StatusBadRequest
	case CodeAccountLockedAuthFails:
		return http.StatusLocked
	default:
		return http.StatusInternalServerError
	}
}

// newError builds a coded auth error.
func newError(code, msg string) *excerrors.Error {
	return excerrors.New(code, msg)
}

// wrapError builds a coded auth error wrapping a lower-level cause.
func wrapError(code, msg string, cause error) *excerrors.Error {
	return excerrors.Wrap(code, msg, cause)
}

// HTTPStatusOf resolves the HTTP status for err: auth codes use the
// auth-cluster map, other *errors.Error codes fall back to the shared
// registry, and anything else maps to 500.
func HTTPStatusOf(err error) int {
	var e *excerrors.Error
	if stderrors.As(err, &e) {
		if s := authHTTPStatus(e.Code); s != http.StatusInternalServerError {
			return s
		}
		// AUTH_INTERNAL deliberately lands on 500 here.
		return excerrors.HTTPStatus(e.Code)
	}
	return http.StatusInternalServerError
}
