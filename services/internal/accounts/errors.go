package accounts

import (
	"fmt"

	excerrors "exchange/pkg/errors"
)

// Error codes emitted by the account-state cluster. §23-registered codes
// reuse the registry names verbatim; cluster-specific rejections that
// carry no §23 row reuse the generic registered codes (INVALID_REQUEST /
// FORBIDDEN / NOT_FOUND) — no new codes are minted here, and Task 5.3.21's
// registry (internal/errs) resolves their HTTP status.
const (
	CodeInvalidRequest         = "INVALID_REQUEST"            // 400, §23
	CodeUnauthorized           = "UNAUTHORIZED"               // 401, §23
	CodeForbidden              = "FORBIDDEN"                  // 403, §23
	CodeNotFound               = "NOT_FOUND"                  // 404, §23
	CodeInsufficientScope      = "INSUFFICIENT_SCOPE"         // 403, §23
	CodeAccountFrozen          = "ACCOUNT_FROZEN"             // 403, §23
	CodeTwoFactorRequired      = "TWO_FACTOR_REQUIRED"        // 403, §26 auth matrix
	CodeDualControlRequired    = "DUAL_CONTROL_REQUIRED"      // 400, §26 maker-checker matrix
	CodeUnauthorizedRole       = "UNAUTHORIZED_ROLE"          // 403, §26 maker-checker matrix
	CodeCountdownInvalid       = "COUNTDOWN_INVALID_DURATION" // 400, §23
	CodeCountdownAlreadyActive = "COUNTDOWN_ALREADY_ACTIVE"   // 409, §23
	CodeCloseAllPartialFailure = "CLOSE_ALL_PARTIAL_FAILURE"  // 409, §23
)

func newError(code, msg string) *excerrors.Error { return excerrors.New(code, msg) }

func errorf(code, format string, args ...any) *excerrors.Error {
	return excerrors.New(code, fmt.Sprintf(format, args...))
}
