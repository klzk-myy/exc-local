// Task 12.3.3 — user self-service profile & account-scoped API keys.
//
// Routes (routes_v1.go; all live):
//
//	GET    /api/v1/account/profile          authUser
//	PUT    /api/v1/account/profile          authUser
//	POST   /api/v1/account/change-password  authUser
//	GET    /api/v1/account/api-keys         authUser
//	POST   /api/v1/account/api-keys         authUser + RequireTwoFactor
//	DELETE /api/v1/account/api-keys/{id}    authUser
//
// The profile columns are the client-editable contact set added by
// migration 027 (full_name/address) plus the 002 phone — legal PII
// stays in kyc_profiles. The API-key surface reuses the Task 5.3.16
// developer-portal implementation verbatim: it is already
// account-scoped (claims.AccountID), the create op gains the spec
// §12.2 second-factor gate at the mount.
package api

import (
	"encoding/json"
	"net/http"
	"time"

	"exchange/internal/auth"
	"exchange/internal/gateway"
)

// profileJSON is the self-service profile wire shape.
func profileJSON(u *auth.User) map[string]any {
	var verified any
	if u.EmailVerifiedAt != nil {
		verified = u.EmailVerifiedAt.UTC().Format(time.RFC3339)
	}
	return map[string]any{
		"user_id":            u.ID,
		"email":              u.Email,
		"full_name":          u.FullName,
		"address":            u.Address,
		"phone":              u.Phone,
		"country":            u.Country,
		"status":             u.Status,
		"kyc_status":         u.KYCStatus,
		"email_verified_at":  verified,
		"two_factor_enabled": u.TOTPEnabled(),
		"created_at":         u.CreatedAt.UTC().Format(time.RFC3339),
	}
}

// AccountProfileGet serves GET /api/v1/account/profile.
func AccountProfileGet(users *auth.UserStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		uid, _, ok := claimsUserID(w, r)
		if !ok {
			return
		}
		u, err := users.UserByID(r.Context(), uid)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		if u == nil {
			WriteError(w, "UNAUTHORIZED", "user not found",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		WriteJSON(w, http.StatusOK, profileJSON(u))
	}
}

// AccountProfileUpdate serves PUT /api/v1/account/profile — replaces
// the client-editable contact fields. Empty strings clear a field;
// absent JSON keys also clear (PUT replace semantics on this resource).
func AccountProfileUpdate(users *auth.UserStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		uid, _, ok := claimsUserID(w, r)
		if !ok {
			return
		}
		var body struct {
			FullName string `json:"full_name"`
			Address  string `json:"address"`
			Phone    string `json:"phone"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			WriteError(w, "INVALID_REQUEST", "malformed JSON body",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		if len(body.FullName) > 128 || len(body.Address) > 255 || len(body.Phone) > 32 {
			WriteError(w, "INVALID_REQUEST",
				"field length exceeds column bound (name 128, address 255, phone 32)",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		if err := users.UpdateProfile(r.Context(), uid,
			body.FullName, body.Address, body.Phone); err != nil {
			writeServiceErr(w, r, err)
			return
		}
		u, err := users.UserByID(r.Context(), uid)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, profileJSON(u))
	}
}

// AccountChangePassword serves POST /api/v1/account/change-password —
// verifies the current password, installs the new bcrypt hash, and
// revokes every session except the one making the change.
func AccountChangePassword(svc *auth.AuthnService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		uid, claims, ok := claimsUserID(w, r)
		if !ok {
			return
		}
		var body struct {
			CurrentPassword string `json:"current_password"`
			NewPassword     string `json:"new_password"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			WriteError(w, "INVALID_REQUEST", "malformed JSON body",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		keepSID := ""
		if claims != nil {
			keepSID = claims.SessionID
		}
		if err := svc.ChangePassword(r.Context(), uid,
			body.CurrentPassword, body.NewPassword, keepSID); err != nil {
			writeAuthnErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"status": "ok"})
	}
}

// AccountAPIKeys returns the account-scoped key management handlers —
// the Task 5.3.16 implementation, already bound to claims.AccountID.
// Callers mount create under auth.RequireTwoFactor() (spec §12.2: key
// creation is a sensitive op); list/revoke need only authUser.
func AccountAPIKeys(ks *auth.KeyStore) (create, list, revoke http.HandlerFunc) {
	return DeveloperAPIKeys(ks)
}
