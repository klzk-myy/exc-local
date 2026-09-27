// Package errors provides typed errors carrying stable machine-readable
// codes, so services, gateways and clients can key off Code rather than
// matching message strings.
//
// PHASE-05 owner: the canonical error-code registry (every emitted code +
// HTTP status) lands in Phase-05 Task 5.3.21. Codes used before then are
// scaffold placeholders and must be registered there.
package errors

import "fmt"

// Error is a coded error returned by exchange services.
type Error struct {
	Code    string // stable machine-readable code, e.g. "CONFIG_INVALID"
	Message string // human-readable detail
	Cause   error  // wrapped underlying error, optional
}

// New returns a coded error with no wrapped cause.
func New(code, message string) *Error {
	return &Error{Code: code, Message: message}
}

// Wrap returns a coded error wrapping cause.
func Wrap(code, message string, cause error) *Error {
	return &Error{Code: code, Message: message, Cause: cause}
}

func (e *Error) Error() string {
	if e.Cause != nil {
		return fmt.Sprintf("%s: %s: %v", e.Code, e.Message, e.Cause)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

// Unwrap exposes the wrapped cause for errors.Is/As.
func (e *Error) Unwrap() error { return e.Cause }
