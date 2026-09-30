// handlers_p21privacy.go — Phase-21 Task 21.3.7 (GDPR export/erasure/
// consent) + Task 21.3.26 (public promotion render gate).
//
//	POST /api/v1/account/gdpr/export   — Art. 15/20 manifest (Task 21.3.7)
//	POST /api/v1/account/gdpr/erase    — Art. 17 erasure (account must be
//	                                    CLOSED — Phase-14 offboarding)
//	PUT  /api/v1/account/consent       — per-purpose grant/withdraw
//	GET  /api/v1/account/consent       — current consent state
//	GET  /api/v1/promotions/{id}       — customer-facing render gate:
//	                                    only APPROVED + unexpired
//	                                    content resolves (410 otherwise)
//
// Identity scoping: account_id comes from validated claims
// (claims.AccountID), the acting user from claims.Subject — the caller
// can never request another account's data.
package api

import (
	"net/http"
	"strconv"
	"strings"

	"exchange/internal/auth"
	"exchange/internal/compliance"
	"exchange/internal/content"
	"exchange/internal/gateway"

	excerrors "exchange/pkg/errors"
)

// privacyClaims resolves (accountID, userID) from the auth context.
// Named distinctly from account.go's claimsAccount (same package).
func privacyClaims(r *http.Request) (int64, int64, error) {
	claims := auth.ClaimsFrom(r.Context())
	if claims == nil {
		return 0, 0, excerrors.New("UNAUTHORIZED", "authentication required")
	}
	if claims.AccountID == 0 {
		return 0, 0, excerrors.New("UNAUTHORIZED",
			"account context required")
	}
	uid, err := strconv.ParseInt(claims.Subject, 10, 64)
	if err != nil || uid <= 0 {
		return 0, 0, excerrors.New("UNAUTHORIZED",
			"user identity unresolvable")
	}
	return claims.AccountID, uid, nil
}

// GDPRExport serves POST /api/v1/account/gdpr/export.
func GDPRExport(svc *compliance.GDPRService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		acct, uid, err := privacyClaims(r)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		req, body, err := svc.RequestExport(r.Context(), acct, uid)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		if body == nil {
			// Dedup hit on an open request — report its live status.
			WriteJSON(w, http.StatusAccepted, map[string]any{
				"request": req, "status": req.Status,
			})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Disposition",
			`attachment; filename="gdpr-export.json"`)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	}
}

// GDPRErase serves POST /api/v1/account/gdpr/erase — body must carry
// {"confirm": true}; "reason" is optional.
func GDPRErase(svc *compliance.GDPRService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		acct, uid, err := privacyClaims(r)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		var in struct {
			Confirm bool   `json:"confirm"`
			Reason  string `json:"reason"`
		}
		if !decodeJSONBody(w, r, &in) {
			return
		}
		if !in.Confirm {
			WriteError(w, "INVALID_REQUEST",
				"erasure requires explicit confirm=true — this action is irreversible",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		req, err := svc.RequestErasure(r.Context(), acct, uid, in.Reason)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"request": req})
	}
}

// GDPRRequests serves GET /api/v1/account/gdpr — the account's request
// history (registered route is POST-only for export/erase; the list is
// a convenience under the same privacy surface — keeps the registry
// minimal per Task 5.3.7).
func GDPRRequests(svc *compliance.GDPRService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		acct, _, err := privacyClaims(r)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		list, err := svc.ListRequests(r.Context(), acct)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"requests": list})
	}
}

// ConsentPut serves PUT /api/v1/account/consent —
// {"purpose":"MARKETING","channel":"EMAIL","granted":true} (channel
// optional, defaults ALL; "state":"GRANTED"/"WITHDRAWN" is accepted as
// the alternative spelling).
func ConsentPut(svc *compliance.GDPRService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		acct, uid, err := privacyClaims(r)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		var in struct {
			Purpose string `json:"purpose"`
			Channel string `json:"channel"`
			State   string `json:"state"`
			Granted *bool  `json:"granted"`
		}
		if !decodeJSONBody(w, r, &in) {
			return
		}
		state := strings.ToUpper(strings.TrimSpace(in.State))
		if state == "" {
			if in.Granted == nil {
				WriteError(w, "INVALID_REQUEST",
					"one of state or granted is required",
					gateway.RequestIDFrom(r.Context()), nil)
				return
			}
			if *in.Granted {
				state = compliance.ConsentStateGranted
			} else {
				state = compliance.ConsentStateWithdrawn
			}
		}
		c, err := svc.SetConsent(r.Context(), acct, uid,
			in.Purpose, in.Channel, state, map[string]any{
				"surface": "account-consent-api",
				"ip":      r.RemoteAddr,
			})
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"consent": c})
	}
}

// ConsentList serves GET /api/v1/account/consent.
func ConsentList(svc *compliance.GDPRService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		acct, _, err := privacyClaims(r)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		list, err := svc.ListConsents(r.Context(), acct)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"consents": list})
	}
}

// PromotionPublicRender serves GET /api/v1/promotions/{id} — the
// renderable-content view (metadata + body_ref); PROMOTION_NOT_APPROVED
// (410) for anything not APPROVED+unexpired.
func PromotionPublicRender(gate *content.Gate) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := parsePathID(r.PathValue("id"))
		if !ok {
			WriteError(w, "INVALID_REQUEST", "id must be a positive integer",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		p, err := gate.Renderable(r.Context(), id)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"promotion": p})
	}
}
