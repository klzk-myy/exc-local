// RFC 7807 problem-details envelopes (Task 1.3.12): the structured error
// body every gateway returns for rejected requests. The Code extension
// member carries the spec §23 machine-readable code so clients key off it
// rather than matching message strings.
package errors

import (
	"encoding/json"
	stderrors "errors"
	"net/http"
)

// ProblemMediaType is the RFC 7807 response content type.
const ProblemMediaType = "application/problem+json"

// Problem is an RFC 7807 application/problem+json envelope. Code is an
// extension member carrying the spec §23 error code.
type Problem struct {
	Type     string `json:"type"`   // URI identifying the problem type
	Title    string `json:"title"`  // short human-readable summary
	Status   int    `json:"status"` // HTTP status code
	Detail   string `json:"detail,omitempty"`
	Instance string `json:"instance,omitempty"`
	Code     string `json:"code"` // extension: spec §23 machine-readable code
}

// NewProblem builds a problem envelope for a coded failure. Type defaults
// to a stable URN derived from the code.
func NewProblem(status int, code, title, detail string) Problem {
	return Problem{
		Type:   "urn:exc:problem:" + code,
		Title:  title,
		Status: status,
		Detail: detail,
		Code:   code,
	}
}

// ProblemFor converts err into an envelope: a *Error contributes its Code,
// HTTP status (HTTPStatus) and Message; any other error maps to a generic
// 500 envelope that leaks no internal detail (fail-closed, spec §2.7.1).
func ProblemFor(err error) Problem {
	var e *Error
	if stderrors.As(err, &e) {
		return NewProblem(HTTPStatus(e.Code), e.Code, e.Code, e.Message)
	}
	return NewProblem(http.StatusInternalServerError, "INTERNAL_ERROR",
		"internal error", "")
}

// WriteTo serializes p to w with the RFC 7807 media type and p.Status.
// Handler for HTTP edges; FIX rejections map the same code per spec §23.
func (p Problem) WriteTo(w http.ResponseWriter) {
	body, err := json.Marshal(p)
	if err != nil {
		// Marshal can only fail on a non-encodable field — unreachable for
		// this struct, but stay fail-closed.
		body = []byte(`{"type":"urn:exc:problem:INTERNAL_ERROR","title":"internal error","status":500,"code":"INTERNAL_ERROR"}`)
	}
	status := p.Status
	if status < 100 || status > 599 {
		status = http.StatusInternalServerError // never emit an invalid status line
	}
	w.Header().Set("Content-Type", ProblemMediaType)
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// WriteProblem converts err via ProblemFor and writes the envelope.
func WriteProblem(w http.ResponseWriter, err error) {
	ProblemFor(err).WriteTo(w)
}
