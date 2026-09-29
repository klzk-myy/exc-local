package delegation

import (
	"fmt"

	excerrors "exchange/pkg/errors"
)

// Error codes emitted by the delegation cluster — all are §23-registered
// (internal/errs); no new codes are minted here.
const (
	CodeInvalidRequest         = "INVALID_REQUEST"
	CodeUnauthorized           = "UNAUTHORIZED"
	CodeForbidden              = "FORBIDDEN"
	CodeNotFound               = "NOT_FOUND"
	CodeInsufficientScope      = "INSUFFICIENT_SCOPE"
	CodeUnauthorizedRole       = "UNAUTHORIZED_ROLE"
	CodeDualControlViolation   = "DUAL_CONTROL_VIOLATION"
	CodeMultiValidatorRequired = "MULTI_VALIDATOR_REQUIRED"
	CodeInvalidLifecycle       = "INVALID_LIFECYCLE_TRANSITION"
)

func newError(code, msg string) *excerrors.Error { return excerrors.New(code, msg) }

func errorf(code, format string, args ...any) *excerrors.Error {
	return excerrors.New(code, fmt.Sprintf(format, args...))
}
