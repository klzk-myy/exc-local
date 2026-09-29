// Task 12.3.10 (+ Task 12.3.12 part 3) — account emergency security.
//
// POST /api/v1/account/emergency-freeze — the panic button. Session
// authentication only (no extra 2FA by design: a compromised 2FA device
// must not lock out the owner). The accounts.EmergencyFreezeService
// saga mass-cancels orders (≤3 retries), freezes the account with
// SELF_FREEZE, terminates other sessions, revokes API keys and raises
// the P1 alert on any partial failure.
//
// POST /api/v1/account/unfreeze-request — opens the durable
// re-verification request (migration 201). The identity re-verification
// workflow itself (government ID + liveness + new 2FA setup) is the
// Phase-14 admin surface — this endpoint only owns the honest
// SUBMITTED state machine edge.
package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"

	"exchange/internal/accounts"
	"exchange/internal/gateway"
	"exchange/internal/middleware"
)

// emergencyFreezer is the accounts.EmergencyFreezeService seam.
type emergencyFreezer interface {
	Freeze(ctx context.Context, req accounts.EmergencyFreezeRequest) (*accounts.EmergencyFreezeResult, error)
}

// EmergencyFreeze serves POST /api/v1/account/emergency-freeze.
// Requires a session-bound principal: the numeric JWT subject is
// mandatory — a non-user subject is refused UNAUTHORIZED, matching the
// "current session" contract.
func EmergencyFreeze(svc emergencyFreezer, trustProxy bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		accountID, claims, ok := claimsAccount(w, r)
		if !ok {
			return
		}
		if svc == nil {
			WriteError(w, "SERVICE_DEGRADED", "emergency freeze unavailable",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		userID, err := parseSubjectID(claims.Subject)
		if err != nil || userID <= 0 {
			WriteError(w, "UNAUTHORIZED", "session-bound user identity required",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		res, err := svc.Freeze(r.Context(), accounts.EmergencyFreezeRequest{
			AccountID: accountID,
			UserID:    userID,
			SessionID: claims.SessionID,
			ClientIP:  middleware.ClientIP(r, trustProxy),
		})
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, res)
	}
}

// unfreezeRequester is the accounts.UnfreezeService seam.
type unfreezeRequester interface {
	Request(ctx context.Context, accountID, userID int64,
		idDocRef, livenessRef, note string) (*accounts.UnfreezeRequest, error)
}

type unfreezeRequestBody struct {
	IDDocumentRef string `json:"id_document_ref,omitempty"`
	LivenessRef   string `json:"liveness_ref,omitempty"`
	Note          string `json:"note,omitempty"`
}

// UnfreezeRequest serves POST /api/v1/account/unfreeze-request — opens
// (or idempotently replays) the account's open re-verification request.
// The body is optional — an empty body still opens the request.
func UnfreezeRequest(svc unfreezeRequester) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		accountID, claims, ok := claimsAccount(w, r)
		if !ok {
			return
		}
		if svc == nil {
			WriteError(w, "SERVICE_DEGRADED", "unfreeze requests unavailable",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		var body unfreezeRequestBody
		if r.Body != nil {
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil && err != io.EOF {
				WriteError(w, "INVALID_REQUEST", "malformed JSON body",
					gateway.RequestIDFrom(r.Context()), nil)
				return
			}
		}
		userID, err := parseSubjectID(claims.Subject)
		if err != nil || userID <= 0 {
			WriteError(w, "UNAUTHORIZED", "session-bound user identity required",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		res, err := svc.Request(r.Context(), accountID, userID,
			body.IDDocumentRef, body.LivenessRef, body.Note)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		if res.Replayed {
			WriteJSON(w, http.StatusOK, res)
			return
		}
		WriteJSON(w, http.StatusCreated, res)
	}
}
