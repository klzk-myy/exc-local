// Task 5.3.17 — webhook registration/management surface over
// internal/webhooks (delivery pipeline: dispatch.go).
//
//	POST   /api/v1/webhooks                       — register endpoint
//	GET    /api/v1/webhooks                       — list endpoints
//	DELETE /api/v1/webhooks/{id}                  — disable endpoint
//	POST   /api/v1/webhooks/{id}/rotate-secret    — rotate signing secret
//	GET    /api/v1/webhooks/{id}/deliveries       — delivery log + DLQ view
//
// Registration returns the signing secret exactly once; deliveries are
// POSTed with X-Webhook-Signature = hex(HMAC-SHA256(secret,
// "<unix>.<body>")) per the Task 5.3.17 contract.
package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"exchange/internal/auth"
	"exchange/internal/gateway"
	"exchange/internal/webhooks"
)

// WebhookHandlers returns register/list/disable/rotate/deliveries
// handlers bound to the store.
func WebhookHandlers(store *webhooks.Store) (register, list, disable, rotateSecret, deliveries http.HandlerFunc) {
	register = func(w http.ResponseWriter, r *http.Request) {
		claims := auth.ClaimsFrom(r.Context())
		if claims == nil || claims.AccountID == 0 {
			WriteError(w, "UNAUTHORIZED",
				"authentication required", gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		var body struct {
			URL    string   `json:"url"`
			Events []string `json:"events"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			WriteError(w, "INVALID_REQUEST",
				"malformed body", gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		actor := claims.AccountID
		if n, err := parseSubjectID(claims.Subject); err == nil && n > 0 {
			actor = n
		}
		ep, secret, err := store.Register(r.Context(), claims.AccountID, actor,
			body.URL, body.Events)
		if err != nil {
			writeWebhookStoreError(w, r, err)
			return
		}
		WriteJSON(w, http.StatusCreated, map[string]any{
			"endpoint": ep,
			// shown exactly once — recipients verify with it.
			"secret": secret,
			"signature": map[string]any{
				"header": webhooks.HeaderSignature,
				"scheme": "hex(HMAC-SHA256(secret, \"<X-Webhook-Timestamp>.<body>\"))",
			},
		})
	}
	list = func(w http.ResponseWriter, r *http.Request) {
		claims := auth.ClaimsFrom(r.Context())
		if claims == nil || claims.AccountID == 0 {
			WriteError(w, "UNAUTHORIZED",
				"authentication required", gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		eps, err := store.List(r.Context(), claims.AccountID)
		if err != nil {
			WriteError(w, "SERVICE_DEGRADED",
				"webhook store unavailable", gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{
			"webhooks":       eps,
			"allowed_events": webhooks.Events(),
		})
	}
	disable = func(w http.ResponseWriter, r *http.Request) {
		claims := auth.ClaimsFrom(r.Context())
		if claims == nil || claims.AccountID == 0 {
			WriteError(w, "UNAUTHORIZED",
				"authentication required", gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		if err := store.Disable(r.Context(), r.PathValue("id"), claims.AccountID); err != nil {
			writeWebhookStoreError(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{
			"endpoint_id": r.PathValue("id"), "status": webhooks.StatusDisabled,
		})
	}
	rotateSecret = func(w http.ResponseWriter, r *http.Request) {
		claims := auth.ClaimsFrom(r.Context())
		if claims == nil || claims.AccountID == 0 {
			WriteError(w, "UNAUTHORIZED",
				"authentication required", gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		var body struct {
			OverlapSeconds int64 `json:"overlap_seconds"` // ≤ 259200 (72h); 0 = immediate
		}
		if r.Body != nil && r.ContentLength > 0 {
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				WriteError(w, "INVALID_REQUEST",
					"malformed body", gateway.RequestIDFrom(r.Context()), nil)
				return
			}
		}
		secret, err := store.RotateSecret(r.Context(), r.PathValue("id"),
			claims.AccountID, time.Duration(body.OverlapSeconds)*time.Second)
		if err != nil {
			writeWebhookStoreError(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{
			"endpoint_id": r.PathValue("id"),
			"secret":      secret, // shown once
			"notice":      "previous secret remains valid within the overlap window via X-Webhook-Signature-Prev",
		})
	}
	deliveries = func(w http.ResponseWriter, r *http.Request) {
		claims := auth.ClaimsFrom(r.Context())
		if claims == nil || claims.AccountID == 0 {
			WriteError(w, "UNAUTHORIZED",
				"authentication required", gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		limit := 200
		if q := r.URL.Query().Get("limit"); q != "" {
			if n, err := strconv.Atoi(q); err == nil && n > 0 && n <= 500 {
				limit = n
			}
		}
		ds, err := store.ListDeliveries(r.Context(), r.PathValue("id"),
			claims.AccountID, limit)
		if err != nil {
			writeWebhookStoreError(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"deliveries": ds})
	}
	return register, list, disable, rotateSecret, deliveries
}

// writeWebhookStoreError maps store errors to registered codes: bad
// input → 400, unknown/foreign endpoint → 404 (no existence oracle),
// anything else → 503.
func writeWebhookStoreError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case err == nil:
		return
	case errors.Is(err, webhooks.ErrEndpointNotFound):
		WriteError(w, "NOT_FOUND",
			"webhook endpoint not found", gateway.RequestIDFrom(r.Context()), nil)
	case isWebhookInputError(err):
		WriteError(w, "INVALID_REQUEST",
			err.Error(), gateway.RequestIDFrom(r.Context()), nil)
	default:
		WriteError(w, "SERVICE_DEGRADED",
			"webhook store unavailable", gateway.RequestIDFrom(r.Context()), nil)
	}
}

// isWebhookInputError classifies store validation errors by message
// prefix — the store's domain errors are fmt-wrapped strings; everything
// starting "webhooks:" that isn't a persistence failure is a 400.
func isWebhookInputError(err error) bool {
	msg := err.Error()
	if len(msg) < 9 || msg[:9] != "webhooks:" {
		return false
	}
	for _, internal := range []string{
		"webhooks: insert", "webhooks: endpoint read", "webhooks: list",
		"webhooks: scan", "webhooks: seal", "webhooks: unseal",
		"webhooks: resolve", "webhooks: enqueue", "webhooks: secret read",
		"webhooks: id generation",
	} {
		if len(msg) >= len(internal) && msg[:len(internal)] == internal {
			return false
		}
	}
	return true
}
