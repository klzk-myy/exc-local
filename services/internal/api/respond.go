// Shared gateway response helpers: the spec §8.7 error envelope used by
// REST handlers and (through the Emit seam) the edge middleware.
//
// Every emission funnels through the Task 5.3.21 registry (errs.Default):
// the HTTP status comes from the registry and unregistered codes record a
// startup-fatal violation instead of leaking a bogus status (fail-closed,
// spec §2.7.1). Media type is application/problem+json to match the
// gateway.Router envelope convention.
package api

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"exchange/internal/errs"
)

// ErrorEnvelope is the canonical gateway error body (spec §8.7 item 1).
// type is always "error"; error carries the §23 machine-readable code.
// retry_after is the HTTP-seconds duration member per the §10.5 item 5
// duration-unit convention.
type ErrorEnvelope struct {
	Type       string         `json:"type"`
	Error      string         `json:"error"`
	Message    string         `json:"message"`
	Status     int            `json:"status"`
	RequestID  string         `json:"request_id,omitempty"`
	Timestamp  string         `json:"timestamp"`
	RetryAfter int            `json:"retry_after,omitempty"`
	Details    map[string]any `json:"details,omitempty"`
}

// WriteError serializes the §8.7 envelope for a registered §23 code. The
// HTTP status is resolved from the registry (unknown codes degrade to
// INTERNAL_ERROR 500 and are recorded by the emission gate).
// details["retry_after"] (int seconds) additionally sets the RFC 6585
// Retry-After header — the §8.3/§24 #192 contract on 429/418/503.
func WriteError(w http.ResponseWriter, code, message, requestID string, details map[string]any) {
	e := errs.Default.NewError(code, message)
	status := errs.Default.HTTPStatus(e.Code)
	env := ErrorEnvelope{
		Type:      "error",
		Error:     e.Code,
		Message:   message,
		Status:    status,
		RequestID: requestID,
		Timestamp: time.Now().UTC().Format("2006-01-02T15:04:05.000Z"),
		Details:   details,
	}
	if ra, ok := details["retry_after"].(int); ok && ra > 0 {
		env.RetryAfter = ra
	}
	if ra, ok := details["retry_after"].(int64); ok && ra > 0 {
		env.RetryAfter = int(ra)
	}
	if env.RetryAfter > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(env.RetryAfter))
	}
	body, err := json.Marshal(env)
	if err != nil {
		body = []byte(`{"type":"error","error":"INTERNAL_ERROR","message":"internal error","status":500}`)
		status = http.StatusInternalServerError
	}
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// WriteJSON serializes a success payload.
func WriteJSON(w http.ResponseWriter, status int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		WriteError(w, "INTERNAL_ERROR", "internal error", "", nil)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}
