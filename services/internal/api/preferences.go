// Task 12.3.6 — Notification Preferences.
//
//	GET /api/v1/account/notifications/preferences  — read the caller's matrix
//	PUT /api/v1/account/notifications/preferences  — full replace
//
// Identity is the authenticated user (numeric JWT sub — same convention
// as funding/devkeys handlers); a non-user identity (oauth2 client
// grant, no account context) cannot hold per-user prefs → UNAUTHORIZED
// (fail closed §2.7).
//
// Payload shape:
//
//	{
//	  "matrix": {"deposit_confirmed": {"email": true, "sms": false, ...}, ...},
//	  "quiet_hours": {"enabled": true, "start": "22:00", "end": "07:00"}
//	}
//
// Absent matrix cells resolve to the service defaults (email + WS on,
// SMS + push off). Quiet hours are UTC; security_alert and
// liquidation_warning bypass them (ruling documented in
// internal/notifications/doc.go).
package api

import (
	"context"
	"encoding/json"
	"net/http"

	"exchange/internal/gateway"
	"exchange/internal/notifications"
)

// prefStore is the store seam the handlers need (notifications.PgStore
// satisfies it; tests substitute a fake).
type prefStore interface {
	GetPreferences(ctx context.Context, userID int64) (*notifications.Preferences, error)
	PutPreferences(ctx context.Context, p *notifications.Preferences) (*notifications.Preferences, error)
}

// preferencesJSON projects the stored model into the wire shape —
// defaults materialize as an empty matrix (clients render the
// documented default set themselves; the stored row only carries
// explicit choices).
func preferencesJSON(p *notifications.Preferences) map[string]any {
	matrix := p.Matrix
	if matrix == nil {
		matrix = map[string]map[string]bool{}
	}
	return map[string]any{
		"user_id":     p.UserID,
		"matrix":      matrix,
		"quiet_hours": p.Quiet,
		"updated_at":  p.UpdatedAt,
		// Surface the vocabulary + defaults so the settings UI renders
		// the full grid without a schema side-channel.
		"events":   notifications.Events(),
		"channels": notifications.Channels(),
		"defaults": map[string]bool{"email": true, "sms": false, "push": false, "ws": true},
		"quiet_hours_exempt": []string{
			notifications.EventSecurityAlert, notifications.EventLiquidationWarning},
	}
}

// NotificationPreferencesGet serves GET /api/v1/account/notifications/preferences.
func NotificationPreferencesGet(st prefStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID, _, ok := claimsUserID(w, r)
		if !ok {
			return
		}
		if st == nil {
			WriteError(w, "SERVICE_DEGRADED", "preferences store unavailable",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		p, err := st.GetPreferences(r.Context(), userID)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		if p == nil {
			p = notifications.DefaultPreferences(userID)
		}
		WriteJSON(w, http.StatusOK, preferencesJSON(p))
	}
}

// NotificationPreferencesPut serves PUT /api/v1/account/notifications/preferences
// — full replace; validation failures emit INVALID_REQUEST.
func NotificationPreferencesPut(st prefStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID, _, ok := claimsUserID(w, r)
		if !ok {
			return
		}
		if st == nil {
			WriteError(w, "SERVICE_DEGRADED", "preferences store unavailable",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		var body struct {
			Matrix map[string]map[string]bool `json:"matrix"`
			Quiet  notifications.QuietHours   `json:"quiet_hours"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			WriteError(w, "INVALID_REQUEST", "malformed body",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		p := &notifications.Preferences{
			UserID: userID, Matrix: body.Matrix, Quiet: body.Quiet,
		}
		if p.Matrix == nil {
			p.Matrix = map[string]map[string]bool{}
		}
		out, err := st.PutPreferences(r.Context(), p)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, preferencesJSON(out))
	}
}
