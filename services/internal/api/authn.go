// Task 12.3.1 + 12.3.2 — HTTP surface for user registration,
// authentication, and TOTP 2FA.
//
// Routes (routes_v1.go; all live):
//
//	POST /api/v1/auth/register             public
//	POST /api/v1/auth/verify-email         public  (link token)
//	POST /api/v1/auth/login                public  (two-phase: TOTP challenge)
//	POST /api/v1/auth/refresh              public  (opaque refresh token)
//	POST /api/v1/auth/logout               authUser (revokes the bound session)
//	POST /api/v1/auth/forgot-password      public  (1h reset link)
//	POST /api/v1/auth/reset-password       public  (consumes token)
//	POST /api/v1/auth/2fa/setup            authUser (stages candidate secret)
//	POST /api/v1/auth/2fa/enroll           authUser (same stage semantics)
//	POST /api/v1/auth/2fa/verify           authUser (activate OR step-up)
//	POST /api/v1/auth/2fa/disable          authUser (password + live factor)
//
// Error mapping: auth-package service errors translate onto the
// registered §23 codes — unregistered cluster codes (SESSION_EXPIRED,
// SESSION_REVOKED, OAUTH_CLIENT_INVALID) collapse to UNAUTHORIZED rather
// than leaking internals.
package api

import (
	"context"
	"encoding/json"
	stderrors "errors"
	"net/http"
	"strconv"
	"time"

	"exchange/internal/admin"
	"exchange/internal/auth"
	"exchange/internal/gateway"
	"exchange/internal/middleware"
	excerrors "exchange/pkg/errors"
)

// claimsUserID resolves the numeric users.id of the caller —
// registration-issued JWTs carry sub=strconv(userID). Non-numeric subs
// (oauth2: client_credentials) reject UNAUTHORIZED: these endpoints are
// user-self-service only.
func claimsUserID(w http.ResponseWriter, r *http.Request) (int64, *auth.Claims, bool) {
	claims := auth.ClaimsFrom(r.Context())
	if claims == nil {
		WriteError(w, "UNAUTHORIZED", "authentication required",
			gateway.RequestIDFrom(r.Context()), nil)
		return 0, nil, false
	}
	uid, err := parseSubjectID(claims.Subject)
	if err != nil || uid <= 0 {
		WriteError(w, "UNAUTHORIZED", "user session required",
			gateway.RequestIDFrom(r.Context()), nil)
		return 0, nil, false
	}
	return uid, claims, true
}

// writeAuthnErr maps auth service errors onto the registered wire codes.
// Session-lifecycle cluster codes collapse onto UNAUTHORIZED — the
// refresh/logout surface must not leak which failure fired.
func writeAuthnErr(w http.ResponseWriter, r *http.Request, err error) {
	var e *excerrors.Error
	if !stderrors.As(err, &e) {
		WriteError(w, "INTERNAL_ERROR", "internal error",
			gateway.RequestIDFrom(r.Context()), nil)
		return
	}
	switch e.Code {
	case "UNAUTHORIZED", "SESSION_EXPIRED", "SESSION_REVOKED",
		"OAUTH_CLIENT_INVALID":
		// Unregistered lifecycle codes collapse onto the canonical
		// 401 — no oracle on which auth failure fired.
		WriteError(w, "UNAUTHORIZED", e.Message,
			gateway.RequestIDFrom(r.Context()), nil)
	default:
		writeServiceErr(w, r, err)
	}
}

// ---------------------------------------------------------------------------
// Registration / verification / reset
// ---------------------------------------------------------------------------

// AuthRegister serves POST /api/v1/auth/register — creates the user +
// default SPOT account and dispatches the verification email.
func AuthRegister(svc *auth.AuthnService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Email       string `json:"email"`
			Password    string `json:"password"`
			Country     string `json:"country"`
			AcceptTerms bool   `json:"accept_terms"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			WriteError(w, "INVALID_REQUEST", "malformed JSON body",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		reg, err := svc.Register(r.Context(), auth.RegisterRequest{
			Email: body.Email, Password: body.Password, Country: body.Country,
			AcceptTerms: body.AcceptTerms,
			IP:          middleware.ClientIP(r, true), UserAgent: r.UserAgent(),
		})
		if err != nil {
			writeAuthnErr(w, r, err)
			return
		}
		out := map[string]any{
			"email":                       reg.Email,
			"email_verification_required": reg.EmailVerificationRequired,
		}
		if reg.UserID > 0 {
			out["user_id"] = reg.UserID
			out["account_id"] = reg.AccountID
		}
		WriteJSON(w, http.StatusCreated, out)
	}
}

// AuthVerifyEmail consumes the emailed link token —
// POST /api/v1/auth/verify-email {token}.
func AuthVerifyEmail(svc *auth.AuthnService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Token string `json:"token"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			WriteError(w, "INVALID_REQUEST", "malformed JSON body",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		uid, err := svc.VerifyEmail(r.Context(), body.Token)
		if err != nil {
			writeAuthnErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{
			"user_id":        uid,
			"email_verified": true,
		})
	}
}

// AuthForgotPassword dispatches the 1-hour reset link —
// POST /api/v1/auth/forgot-password {email}. The response is uniform:
// no existence oracle.
func AuthForgotPassword(svc *auth.AuthnService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Email string `json:"email"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			WriteError(w, "INVALID_REQUEST", "malformed JSON body",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		if err := svc.RequestPasswordReset(r.Context(), body.Email); err != nil {
			writeAuthnErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"status": "ok"})
	}
}

// AuthResetPassword consumes the reset token —
// POST /api/v1/auth/reset-password {token, new_password}.
func AuthResetPassword(svc *auth.AuthnService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Token       string `json:"token"`
			NewPassword string `json:"new_password"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			WriteError(w, "INVALID_REQUEST", "malformed JSON body",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		if _, err := svc.ConfirmPasswordReset(r.Context(), body.Token, body.NewPassword); err != nil {
			writeAuthnErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"status": "ok"})
	}
}

// ---------------------------------------------------------------------------
// Login / refresh / logout
// ---------------------------------------------------------------------------

// AuthLogin serves POST /api/v1/auth/login — password phase; when the
// account has TOTP enrolled the first call returns requires_totp +
// challenge, and the client resubmits with totp_code + challenge.
//
// rolesFor resolves the caller's §8.2 venue-admin role for the login
// response's `roles` UX hint (nil-tolerant: test rigs pass nil). The
// response hint only unlocks client navigation — every admin API call is
// re-authorized server-side against live bindings, so a lookup failure
// omits the hint rather than failing login.
func AuthLogin(svc *auth.AuthnService, rolesFor admin.AdminRoleResolver) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Email     string `json:"email"`
			Password  string `json:"password"`
			TOTPCode  string `json:"totp_code"`
			Challenge string `json:"challenge"`
			Device    string `json:"device"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			WriteError(w, "INVALID_REQUEST", "malformed JSON body",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		res, err := svc.Login(r.Context(), auth.LoginRequest{
			Email: body.Email, Password: body.Password,
			TOTPCode: body.TOTPCode, Challenge: body.Challenge,
			IP:        middleware.ClientIP(r, true),
			UserAgent: r.UserAgent(),
			Device:    body.Device,
		})
		if err != nil {
			writeAuthnErr(w, r, err)
			return
		}
		if res.RequiresTOTP {
			WriteJSON(w, http.StatusOK, map[string]any{
				"requires_totp": true,
				"challenge":     res.Challenge,
			})
			return
		}
		WriteJSON(w, http.StatusOK, sessionBundleJSON(res.Issued, map[string]any{
			"user_id":    res.User.ID,
			"email":      res.User.Email,
			"account_id": res.Issued.Session.AccountID,
			"kyc_tier":   res.KYCTier,
			"two_factor": res.User.TOTPEnabled(),
			"roles":      activeAdminRoles(r.Context(), rolesFor, res.User.ID),
		}))
	}
}

// activeAdminRoles returns the caller's live §8.2 roles for the login
// response hint. Always non-nil (empty = trader). Resolver errors omit
// the hint — login itself must not fail on an RBAC lookup.
func activeAdminRoles(ctx context.Context, rolesFor admin.AdminRoleResolver, userID int64) []string {
	roles := []string{}
	if rolesFor == nil {
		return roles
	}
	if role, err := rolesFor(ctx, userID); err == nil && role != "" {
		roles = append(roles, role)
	}
	return roles
}

// AuthRefresh rotates the opaque refresh token —
// POST /api/v1/auth/refresh {refresh_token}.
func AuthRefresh(svc *auth.AuthnService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			RefreshToken string `json:"refresh_token"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			WriteError(w, "INVALID_REQUEST", "malformed JSON body",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		iss, err := svc.Refresh(r.Context(), body.RefreshToken)
		if err != nil {
			writeAuthnErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, sessionBundleJSON(iss, nil))
	}
}

// AuthLogout revokes the session bound to the caller's JWT —
// POST /api/v1/auth/logout. Idempotent: a sessionless grant answers ok.
func AuthLogout(svc *auth.AuthnService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims := auth.ClaimsFrom(r.Context())
		if claims == nil {
			WriteError(w, "UNAUTHORIZED", "authentication required",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		if err := svc.Logout(r.Context(), claims.SessionID); err != nil {
			writeAuthnErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"status": "ok"})
	}
}

// sessionBundleJSON is the shared token-pair wire shape.
func sessionBundleJSON(iss *auth.IssuedSession, extra map[string]any) map[string]any {
	out := map[string]any{
		"access_token":  iss.AccessToken,
		"refresh_token": iss.RefreshToken,
		"token_type":    "Bearer",
		"expires_at":    iss.ExpiresAt.UTC().Format(time.RFC3339),
		"expires_in":    int(time.Until(iss.ExpiresAt).Seconds()),
		"session_id":    iss.Session.ID,
	}
	for k, v := range extra {
		out[k] = v
	}
	return out
}

// ---------------------------------------------------------------------------
// TOTP 2FA
// ---------------------------------------------------------------------------

// twoFactorSetup is the shared body of setup + enroll: both stage a
// candidate secret non-destructively (spec §12.2).
func twoFactorSetup(svc *auth.TwoFactorService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		uid, claims, ok := claimsUserID(w, r)
		if !ok {
			return
		}
		label := strconv.FormatInt(uid, 10)
		if claims != nil && claims.AccountID != 0 {
			label = strconv.FormatInt(claims.AccountID, 10)
		}
		setup, err := svc.Setup(r.Context(), uid, label)
		if err != nil {
			writeAuthnErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{
			"secret":      setup.Secret,
			"otpauth_uri": setup.URI,
			"expires_in":  setup.ExpiresInSec,
		})
	}
}

// TwoFactorSetup serves POST /api/v1/auth/2fa/setup.
func TwoFactorSetup(svc *auth.TwoFactorService) http.HandlerFunc {
	return twoFactorSetup(svc)
}

// TwoFactorEnroll serves POST /api/v1/auth/2fa/enroll — identical stage
// semantics (initial enrollment and re-key share the candidate flow).
func TwoFactorEnroll(svc *auth.TwoFactorService) http.HandlerFunc {
	return twoFactorSetup(svc)
}

// TwoFactorVerify serves POST /api/v1/auth/2fa/verify — candidate staged
// → activate (returns fresh backup codes); else code-vs-active → session
// step-up (returns an elevated access token).
func TwoFactorVerify(svc *auth.TwoFactorService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		uid, claims, ok := claimsUserID(w, r)
		if !ok {
			return
		}
		var body struct {
			Code string `json:"code"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			WriteError(w, "INVALID_REQUEST", "malformed JSON body",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		sid := ""
		if claims != nil {
			sid = claims.SessionID
		}
		res, err := svc.Verify(r.Context(), uid, sid, body.Code)
		if err != nil {
			writeAuthnErr(w, r, err)
			return
		}
		out := map[string]any{
			"two_factor_enabled": true,
		}
		if res.Activated {
			out["enabled"] = true
			codes := res.BackupCodes
			if codes == nil {
				codes = []string{}
			}
			out["backup_codes"] = codes
		}
		if res.AccessToken != "" {
			out["access_token"] = res.AccessToken
			out["expires_at"] = res.ExpiresAt.UTC().Format(time.RFC3339)
		}
		WriteJSON(w, http.StatusOK, out)
	}
}

// TwoFactorDisable serves POST /api/v1/auth/2fa/disable — requires the
// account password AND a live TOTP/backup code; on success every session
// is revoked (re-auth required).
func TwoFactorDisable(svc *auth.TwoFactorService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		uid, _, ok := claimsUserID(w, r)
		if !ok {
			return
		}
		var body struct {
			Password string `json:"password"`
			Code     string `json:"code"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			WriteError(w, "INVALID_REQUEST", "malformed JSON body",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		if err := svc.Disable(r.Context(), uid, body.Password, body.Code); err != nil {
			writeAuthnErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{
			"two_factor_enabled": false,
		})
	}
}
