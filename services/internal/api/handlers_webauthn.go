// Phase-12 Task 12.3.7 — WebAuthn/FIDO2 passkey HTTP surface.
//
// Routes (routes_v1.go):
//
//	POST /api/v1/account/webauthn/register      authUser — registration ceremony
//	POST /api/v1/account/webauthn/authenticate  authUser — step-up assertion
//	POST /api/v1/auth/passkey/assert            public   — discoverable passkey login
//
// Each endpoint is two-phase over one route, matching the navigator
// credentials API contract: a request WITHOUT "challenge_id" starts the
// ceremony (server mints a single-use challenge under
// webauthn:challenge:{id}, ~60s TTL, and returns the publicKey options);
// a request WITH "challenge_id" + "credential" completes it.
//
// The step-up path enforces the §12.6 invariant atomically in the
// handler: a verified assertion updates the stored sign_count, marks the
// session two_factor_verified + amr ["fido2"] via ElevateAMR, and the
// response carries a re-issued JWT bearing amr:["fido2"] — downstream
// RequireTwoFactor passes on the very next request.
package api

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"exchange/internal/auth"
	"exchange/internal/gateway"
	"exchange/internal/middleware"
)

// PasskeyDeps bundles the passkey-login collaborators.
type PasskeyDeps struct {
	WebAuthn *auth.WebAuthnService
	Sessions *auth.SessionManager
	Users    *auth.UserStore
	Lockout  auth.AuthLockout   // optional — same gate as password login
	Recorder auth.LoginRecorder // optional login_history sink
}

// WebAuthnRegister serves POST /api/v1/account/webauthn/register.
func WebAuthnRegister(svc *auth.WebAuthnService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		uid, _, ok := claimsUserID(w, r)
		if !ok {
			return
		}
		var body struct {
			ChallengeID string          `json:"challenge_id"`
			Name        string          `json:"name"`
			Credential  json.RawMessage `json:"credential"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			WriteError(w, "INVALID_REQUEST", "malformed JSON body",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		if body.ChallengeID == "" {
			challengeID, options, err := svc.BeginRegistration(r.Context(), uid)
			if err != nil {
				writeAuthnErr(w, r, err)
				return
			}
			WriteJSON(w, http.StatusOK, map[string]any{
				"challenge_id": challengeID,
				"publicKey":    options.Response,
			})
			return
		}
		if len(body.Credential) == 0 {
			WriteError(w, "INVALID_REQUEST", "credential attestation required",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		id, err := svc.FinishRegistration(r.Context(), uid, body.ChallengeID, body.Name, body.Credential)
		if err != nil {
			writeAuthnErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusCreated, map[string]any{
			"credential_id": id,
			"name":          body.Name,
			"status":        "registered",
		})
	}
}

// WebAuthnAuthenticate serves POST /api/v1/account/webauthn/authenticate —
// the authenticated step-up ceremony. On a verified assertion the session
// gains the fido2 AMR + two_factor_verified and a fresh access token is
// returned (§12.6 elevation invariant).
func WebAuthnAuthenticate(svc *auth.WebAuthnService, sess *auth.SessionManager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		uid, claims, ok := claimsUserID(w, r)
		if !ok {
			return
		}
		var body struct {
			ChallengeID string          `json:"challenge_id"`
			Credential  json.RawMessage `json:"credential"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			WriteError(w, "INVALID_REQUEST", "malformed JSON body",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		if body.ChallengeID == "" {
			challengeID, options, err := svc.BeginAssertion(r.Context(), uid)
			if err != nil {
				writeAuthnErr(w, r, err)
				return
			}
			WriteJSON(w, http.StatusOK, map[string]any{
				"challenge_id": challengeID,
				"publicKey":    options.Response,
			})
			return
		}
		if len(body.Credential) == 0 {
			WriteError(w, "INVALID_REQUEST", "credential assertion required",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		cred, err := svc.FinishAssertion(r.Context(), uid, body.ChallengeID, body.Credential)
		if err != nil {
			writeAuthnErr(w, r, err)
			return
		}
		// §12.6: assertion ⇒ strong MFA — elevate + re-issue in one path.
		elevated, err := sess.ElevateAMR(r.Context(), claims.SessionID, "fido2")
		if err != nil {
			writeAuthnErr(w, r, err)
			return
		}
		access, exp, err := sess.ReissueAccess(r.Context(), claims.SessionID, claims.Scopes)
		if err != nil {
			writeAuthnErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{
			"status":              "ok",
			"two_factor_verified": elevated.TwoFactorVerified,
			"amr":                 elevated.AMR,
			"sign_count":          cred.SignCount,
			"access_token":        access,
			"token_type":          "Bearer",
			"expires_at":          exp.UTC().Format(time.RFC3339),
		})
	}
}

// PasskeyAssert serves POST /api/v1/auth/passkey/assert — the
// discoverable (resident-key) passwordless login. A verified assertion
// with user verification issues a session bearing AMR ["fido2"] +
// two_factor_verified — possession + biometric/PIN is strong MFA per
// §12.6. The lockout gate mirrors password login.
func PasskeyAssert(deps PasskeyDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			ChallengeID string          `json:"challenge_id"`
			Credential  json.RawMessage `json:"credential"`
			Device      string          `json:"device"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			WriteError(w, "INVALID_REQUEST", "malformed JSON body",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		if body.ChallengeID == "" {
			challengeID, options, err := deps.WebAuthn.BeginPasskeyLogin(r.Context())
			if err != nil {
				writeAuthnErr(w, r, err)
				return
			}
			WriteJSON(w, http.StatusOK, map[string]any{
				"challenge_id": challengeID,
				"publicKey":    options.Response,
			})
			return
		}
		if len(body.Credential) == 0 {
			WriteError(w, "INVALID_REQUEST", "credential assertion required",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		uid, _, err := deps.WebAuthn.FinishPasskeyLogin(r.Context(), body.ChallengeID, body.Credential)
		if err != nil {
			writeAuthnErr(w, r, err)
			return
		}
		lockID := strconv.FormatInt(uid, 10)
		ip := middleware.ClientIP(r, true)
		ua := r.UserAgent()
		if deps.Lockout != nil {
			locked, retryAfter, lerr := deps.Lockout.CheckLock(r.Context(), lockID)
			if lerr != nil {
				writeAuthnErr(w, r, lerr)
				return
			}
			if locked {
				if deps.Recorder != nil {
					_ = deps.Recorder.Record(r.Context(), auth.LoginEvent{
						UserID: uid, Result: auth.LoginResultLocked,
						IP: ip, UserAgent: ua, Timestamp: time.Now().UTC()})
				}
				WriteError(w, "ACCOUNT_LOCKED_AUTH_FAILURES",
					"account locked after consecutive authentication failures",
					gateway.RequestIDFrom(r.Context()),
					map[string]any{"retry_after": int(retryAfter.Seconds()) + 1})
				return
			}
		}
		u, err := deps.Users.UserByID(r.Context(), uid)
		if err != nil {
			writeAuthnErr(w, r, err)
			return
		}
		if u == nil || u.Status != "ACTIVE" {
			WriteError(w, "FORBIDDEN", "account is not active",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		accountID, err := deps.Users.DefaultAccountID(r.Context(), uid)
		if err != nil {
			writeAuthnErr(w, r, err)
			return
		}
		tier, err := deps.Users.AccountTierName(r.Context(), accountID)
		if err != nil {
			writeAuthnErr(w, r, err)
			return
		}
		iss, err := deps.Sessions.Issue(r.Context(), auth.IssueRequest{
			UserID: lockID, AccountID: accountID,
			Tier: tier, Device: body.Device, IP: ip, UserAgent: ua,
			AMR: []string{"fido2"}, Scopes: []string{"read", "trade", "transfer"},
			TwoFactorVerified: true,
		})
		if err != nil {
			writeAuthnErr(w, r, err)
			return
		}
		if deps.Lockout != nil {
			_ = deps.Lockout.ClearFailures(r.Context(), lockID)
		}
		if deps.Recorder != nil {
			_ = deps.Recorder.Record(r.Context(), auth.LoginEvent{
				UserID: uid, Result: auth.LoginResultSuccess,
				IP: ip, UserAgent: ua, SessionID: iss.Session.ID,
				Timestamp: time.Now().UTC()})
		}
		WriteJSON(w, http.StatusOK, sessionBundleJSON(iss, map[string]any{
			"user_id":    uid,
			"account_id": iss.Session.AccountID,
			"two_factor": true,
		}))
	}
}
