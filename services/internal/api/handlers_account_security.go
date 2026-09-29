// Phase-12 Tasks 12.3.8/12.3.9 — account security self-service surface.
//
// Routes (routes_v1.go):
//
//	GET    /api/v1/account/sessions                    list active sessions
//	DELETE /api/v1/account/sessions                    revoke all but current
//	DELETE /api/v1/account/sessions/{id}               revoke one session
//	GET    /api/v1/account/login-history               90-day paginated audit
//	PUT    /api/v1/account/settings/anti-phishing-code set phrase (requires 2FA)
//
// Account scoping is always the authenticated claims identity — foreign
// session/history access is FORBIDDEN/404, never a filtered view.
package api

import (
	"encoding/json"
	"net/http"
	"time"

	"exchange/internal/auth"
	"exchange/internal/gateway"
)

// sessionJSON projects an auth.Session for the device-management screen.
// The refresh hash and other unexported internals never leave the
// process; the id is the client-visible handle used by DELETE.
func sessionJSON(s auth.Session, currentSID string) map[string]any {
	return map[string]any{
		"id":             s.ID,
		"device":         s.Device,
		"ip":             s.IP,
		"user_agent":     s.UserAgent,
		"amr":            s.AMR,
		"two_factor":     s.TwoFactorVerified,
		"created_at":     s.CreatedAt.UTC().Format(time.RFC3339),
		"last_active_at": s.LastActiveAt.UTC().Format(time.RFC3339),
		"expires_at":     s.ExpiresAt.UTC().Format(time.RFC3339),
		"current":        s.ID == currentSID,
	}
}

// AccountSessions serves GET /api/v1/account/sessions — the union of the
// user index and the bound-account index (a session created before
// account selection lives on the user index).
func AccountSessions(sess *auth.SessionManager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		uid, claims, ok := claimsUserID(w, r)
		if !ok {
			return
		}
		subj := claims.Subject
		byID := map[string]auth.Session{}
		if l, err := sess.List(r.Context(), 0, subj); err != nil {
			writeAuthnErr(w, r, err)
			return
		} else {
			for _, s := range l {
				byID[s.ID] = s
			}
		}
		if claims.AccountID > 0 {
			if l, err := sess.List(r.Context(), claims.AccountID, subj); err != nil {
				writeAuthnErr(w, r, err)
				return
			} else {
				for _, s := range l {
					byID[s.ID] = s
				}
			}
		}
		out := make([]map[string]any, 0, len(byID))
		for _, s := range byID {
			out = append(out, sessionJSON(s, claims.SessionID))
		}
		WriteJSON(w, http.StatusOK, map[string]any{
			"user_id":  uid,
			"sessions": out,
		})
	}
}

// AccountSessionDelete serves DELETE /api/v1/account/sessions/{id} —
// ownership is verified against the claims principal before revoke; a
// foreign or unknown id returns NOT_FOUND (no session-existence oracle).
func AccountSessionDelete(sess *auth.SessionManager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		uid, claims, ok := claimsUserID(w, r)
		if !ok {
			return
		}
		sid := r.PathValue("id")
		s, found, err := sess.Lookup(r.Context(), sid)
		if err != nil {
			writeAuthnErr(w, r, err)
			return
		}
		owns := found && (s.UserID == claims.Subject ||
			(claims.AccountID > 0 && s.AccountID == claims.AccountID))
		if !owns {
			WriteError(w, "NOT_FOUND", "session not found",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		if err := sess.Revoke(r.Context(), sid); err != nil {
			writeAuthnErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{
			"status": "revoked", "session_id": sid, "user_id": uid,
		})
	}
}

// AccountSessionsDeleteAll serves DELETE /api/v1/account/sessions —
// revokes every session on the user's indexes except the caller's own.
func AccountSessionsDeleteAll(sess *auth.SessionManager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		uid, claims, ok := claimsUserID(w, r)
		if !ok {
			return
		}
		n, err := sess.RevokeAllExcept(r.Context(), claims.AccountID,
			claims.Subject, claims.SessionID)
		if err != nil {
			writeAuthnErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{
			"status": "ok", "revoked": n, "user_id": uid,
			"kept_session_id": claims.SessionID,
		})
	}
}

// AccountLoginHistory serves GET /api/v1/account/login-history — the
// caller's own 90-day trail, keyset-paginated (?limit=&cursor=).
func AccountLoginHistory(svc *auth.LoginHistoryService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		uid, _, ok := claimsUserID(w, r)
		if !ok {
			return
		}
		limit := parseLimitQuery(r.URL.Query().Get("limit"), 25, 100)
		page, err := svc.List(r.Context(), uid, limit, r.URL.Query().Get("cursor"))
		if err != nil {
			writeAuthnErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{
			"user_id":     uid,
			"events":      page.Events,
			"next_cursor": page.NextCursor,
		})
	}
}

// AntiPhishingSet serves PUT /api/v1/account/settings/anti-phishing-code.
// Per spec §5.16/§12.6 the setter requires a second-factor-verified
// session — claims.AMR carrying totp or fido2. {code:""} clears.
func AntiPhishingSet(svc *auth.AntiPhishingService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		uid, claims, ok := claimsUserID(w, r)
		if !ok {
			return
		}
		if !claims.TwoFactorVerified() {
			WriteError(w, "TWO_FACTOR_REQUIRED",
				"second-factor verification required to change the anti-phishing code",
				gateway.RequestIDFrom(r.Context()), nil)
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
		if err := svc.Set(r.Context(), uid, body.Code); err != nil {
			writeAuthnErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{
			"status":  "ok",
			"set":     body.Code != "",
			"user_id": uid,
		})
	}
}
