// Task 5.3.1 item 5 + Task 5.3.10 enforcement — AuthMiddleware validates
// the Bearer JWT on every request and, when sessions are wired, enforces
// the session policy (existence, idle/absolute timeouts) so a revoked or
// timed-out session dies immediately rather than at token expiry.
//
// The middleware never writes on success beyond the session touch inside
// Validate; on failure it emits the RFC 7807 envelope (pkg/errors.Problem)
// with the auth-cluster HTTP status map — the shared registry's
// HTTPStatus does not know these codes yet (Task 5.3.21 owns it).
package auth

import (
	stderrors "errors"
	"net/http"
	"strings"

	excerrors "exchange/pkg/errors"
)

// AuthMiddleware returns middleware that requires a valid Bearer access
// token. sessions may be nil — JWT-only mode for surfaces without session
// semantics — but when wired, a present sid claim is validated against
// the live session record (Task 5.3.10 revocation/timeout enforcement).
//
// On success the request context carries Claims (ClaimsFromContext).
// Session data is authoritative for account binding and AMR — a session
// that selected an account or completed 2FA post-issuance wins over the
// stale token claims.
func AuthMiddleware(issuer *Issuer, sessions *SessionManager) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hdr := r.Header.Get("Authorization")
			if hdr == "" || !strings.HasPrefix(hdr, "Bearer ") {
				writeAuthProblem(w, newError(CodeUnauthorized, "missing bearer token"))
				return
			}
			claims, err := issuer.Parse(strings.TrimSpace(hdr[len("Bearer "):]))
			if err != nil {
				writeAuthProblem(w, err)
				return
			}
			if sessions != nil && claims.SessionID != "" {
				sess, err := sessions.Validate(r.Context(), claims.SessionID)
				if err != nil {
					writeAuthProblem(w, err)
					return
				}
				// Session is authoritative for mutable state.
				claims.AccountID = sess.AccountID
				if len(sess.AMR) > 0 {
					claims.AMR = sess.AMR
				}
			}
			next.ServeHTTP(w, r.WithContext(WithClaims(r.Context(), claims)))
		})
	}
}

// RequireScope returns middleware enforcing one §8.8 scope-matrix entry —
// missing scope → INSUFFICIENT_SCOPE (403); missing claims → UNAUTHORIZED
// (the request skipped auth middleware — a wiring bug, fail closed).
func RequireScope(scope string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			c := ClaimsFrom(r.Context())
			if c == nil {
				writeAuthProblem(w, newError(CodeUnauthorized, "unauthenticated request"))
				return
			}
			if !c.HasScope(scope) {
				writeAuthProblem(w, newError(CodeInsufficientScope, "missing required scope "+scope))
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// RequireTwoFactor returns middleware enforcing the 2FA gate for
// sensitive operations (withdrawals, balance adjustments — Task 5.3.1
// AC). Satisfied when the session AMR already carries totp or fido2
// (§12.6 passkey elevation); else TWO_FACTOR_REQUIRED (403).
func RequireTwoFactor() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			c := ClaimsFrom(r.Context())
			if c == nil {
				writeAuthProblem(w, newError(CodeUnauthorized, "unauthenticated request"))
				return
			}
			if !c.TwoFactorVerified() {
				writeAuthProblem(w, newError(CodeTwoFactorRequired, "operation requires second-factor verification"))
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// writeAuthProblem emits the RFC 7807 envelope with the auth-cluster
// status map — ProblemFor would use the global registry that does not
// yet know these codes (Task 5.3.21).
func writeAuthProblem(w http.ResponseWriter, err error) {
	var e *excerrors.Error
	status := http.StatusInternalServerError
	code, msg := "INTERNAL_ERROR", "internal error"
	if stderrors.As(err, &e) {
		code, msg = e.Code, e.Message
		status = HTTPStatusOf(err)
	}
	excerrors.NewProblem(status, code, code, msg).WriteTo(w)
}
