// Task 5.3.41 — unified error envelope and error mapping.
//
// All gateway error responses (REST and WS frames) conform to the spec §8.7
// RFC 7807 envelope:
//
//	{"type":"error","error":"<CODE>","message":"...","status":N,
//	 "request_id":"...","timestamp":"...","details":{...}}
//
// Internal error classes map to codes + HTTP status through the Task 5.3.21
// registry: *pkgerrors.Error contributes its Code; any other error degrades
// to INTERNAL_ERROR 500 — never leaking internals (fail-closed, §2.7.1).
//
// Request correlation: gateway.RequestID stamps every request with
// "req-<hex>" (honouring an inbound client-supplied id), stores it in the
// request context for RequestIDFrom, and echoes it on the response header.
// The envelope writer resolves the id from context first, then the
// already-stamped response header, then mints one — so the envelope is
// correct even when the middleware was absent from the chain.
package gateway

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	stderrors "errors"
	"net/http"
	"strconv"
	"time"

	"exchange/internal/errs"
	pkgerrors "exchange/pkg/errors"
)

// RequestIDHeader is the canonical correlation header (spec §8.7).
const RequestIDHeader = "X-Request-Id"

type requestIDKey struct{}

// RequestID middleware stamps a correlation id on every request: an
// inbound X-Request-Id (≤128 chars) is honoured, otherwise "req-<16 hex
// bytes>" is minted. The id is stored in the context (RequestIDFrom),
// echoed on the response header, and propagated onto the request header so
// any downstream reader sees the same id.
func RequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get(RequestIDHeader)
		if id == "" || len(id) > 128 {
			id = newRequestID()
		}
		w.Header().Set(RequestIDHeader, id)
		r.Header.Set(RequestIDHeader, id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), requestIDKey{}, id)))
	})
}

// RequestIDFrom returns the correlation id stamped by the RequestID
// middleware, or "" when absent — consumed by handlers, other packages'
// response helpers, and the error envelope.
func RequestIDFrom(ctx context.Context) string {
	if id, ok := ctx.Value(requestIDKey{}).(string); ok {
		return id
	}
	return ""
}

// newRequestID mints "req-<16 hex bytes>"; on a crypto/rand failure it
// falls back to a timestamp-derived id rather than none (fail-closed).
func newRequestID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "req-" + hex.EncodeToString([]byte(time.Now().UTC().Format("20060102150405.000000000")))
	}
	return "req-" + hex.EncodeToString(b[:])
}

// resolveRequestID picks the correlation id for an envelope: the
// middleware-stamped context value wins; then the response header (a
// request-id middleware from another layer already ran); then the inbound
// client header; else mint one and stamp it so client and log correlate.
func resolveRequestID(w http.ResponseWriter, req *http.Request) string {
	if req != nil {
		if id := RequestIDFrom(req.Context()); id != "" {
			return id
		}
	}
	if id := w.Header().Get(RequestIDHeader); id != "" {
		return id
	}
	if req != nil {
		if id := req.Header.Get(RequestIDHeader); id != "" && len(id) <= 128 {
			return id
		}
	}
	id := newRequestID()
	w.Header().Set(RequestIDHeader, id)
	return id
}

// Envelope is the spec §8.7 unified error body for REST and WS surfaces.
type Envelope struct {
	Type      string         `json:"type"`       // always "error"
	Error     string         `json:"error"`      // spec §23 machine-readable code
	Message   string         `json:"message"`    //
	Status    int            `json:"status"`     // HTTP status (WS frames carry it too)
	RequestID string         `json:"request_id"` // X-Request-Id correlation
	Timestamp string         `json:"timestamp"`  // RFC3339 millisecond UTC
	Details   map[string]any `json:"details,omitempty"`
	// RetryAfterSeconds populates the HTTP Retry-After header when >0
	// (429/503 rate-limit and degradation rejections, spec §8.3).
	RetryAfterSeconds int `json:"retry_after,omitempty"`
}

// MarshalJSON emits the envelope; "type" is forced to "error".
func (e Envelope) MarshalJSON() ([]byte, error) {
	type alias Envelope
	e.Type = "error"
	return json.Marshal(alias(e))
}

// WriteError serializes the RFC 7807 envelope for code+message. The HTTP
// status is resolved from the Task 5.3.21 registry — an unregistered code
// is funnelled through the emission gate (Registry.NewError) so it records
// a startup-fatal violation instead of silently leaking a bogus status.
// Content-Type is application/problem+json (RFC 7807 media type); the body
// carries the §8.7 envelope fields.
func (r *Router) WriteError(w http.ResponseWriter, req *http.Request, code, message string, details map[string]any) {
	e := r.reg.NewError(code, message) // emission gate — records violations
	status := r.reg.HTTPStatus(e.Code)
	env := Envelope{
		Error:     e.Code,
		Message:   message,
		Status:    status,
		RequestID: resolveRequestID(w, req),
		Timestamp: time.Now().UTC().Format("2006-01-02T15:04:05.000Z"),
		Details:   details,
	}
	if status == http.StatusTooManyRequests || status == http.StatusServiceUnavailable {
		// RFC 6585: rejection envelope echoes the retry hint; callers may
		// refine via details["retry_after"].
		if ra, ok := details["retry_after"].(int); ok && ra > 0 {
			env.RetryAfterSeconds = ra
			w.Header().Set("Retry-After", strconv.Itoa(ra))
		}
	}
	body, merr := json.Marshal(env)
	if merr != nil {
		body = []byte(`{"type":"error","error":"INTERNAL_ERROR","message":"internal error","status":500}`)
		status = http.StatusInternalServerError
	}
	w.Header().Set("Content-Type", pkgerrors.ProblemMediaType)
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// WriteErr maps an arbitrary error to the envelope: *pkgerrors.Error keeps
// its Code (through the emission gate — an unregistered code still lands
// on INTERNAL_ERROR and records a violation); anything else is an
// INTERNAL_ERROR 500 with no internal detail leaked.
func (r *Router) WriteErr(w http.ResponseWriter, req *http.Request, err error) {
	var e *pkgerrors.Error
	if stderrors.As(err, &e) {
		r.WriteError(w, req, e.Code, e.Message, nil)
		return
	}
	r.WriteError(w, req, errs.CodeInternalError, "internal error", nil)
}

// ErrHandler adapts an error-returning handler to http.Handler: nil error
// means the handler already wrote a success response; a non-nil error is
// mapped through WriteErr. This is the Task 5.3.41 mapping seam — handlers
// return *pkgerrors.Error (or errs.NewError output) and never write error
// bodies themselves.
func (r *Router) ErrHandler(fn func(http.ResponseWriter, *http.Request) error) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if err := fn(w, req); err != nil {
			r.WriteErr(w, req, err)
		}
	})
}

// Recover converts a handler panic into an INTERNAL_ERROR envelope (500)
// instead of an aborted connection — panic details go to the caller's log
// hook, never to the client (fail-closed §2.7.1).
func (r *Router) Recover(logf func(format string, args ...any), next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				if logf != nil {
					logf("gateway: panic in %s %s: %v", req.Method, req.URL.Path, rec)
				}
				r.WriteError(w, req, errs.CodeInternalError, "internal error", nil)
			}
		}()
		next.ServeHTTP(w, req)
	})
}
