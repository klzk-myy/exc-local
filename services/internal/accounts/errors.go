package accounts

import (
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgconn"

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
	CodeAccountCloseBlocked    = "ACCOUNT_CLOSE_BLOCKED"      // 409, §23
	CodeCoolingOffActive       = "COOLING_OFF_ACTIVE"         // 409, §23
	CodeServiceDegraded        = "SERVICE_DEGRADED"           // 503, §23
	CodeProductNotPermitted    = "PRODUCT_NOT_PERMITTED"      // 403, §23
	CodeAccountNotFound        = "ACCOUNT_NOT_FOUND"          // 404, §23
)

func newError(code, msg string) *excerrors.Error { return excerrors.New(code, msg) }

func errorf(code, format string, args ...any) *excerrors.Error {
	return excerrors.New(code, fmt.Sprintf(format, args...))
}

// isUniqueViolation reports the Postgres 23505 unique-constraint breach —
// used to map the swapfree live-request and profile-code uniqueness
// guards onto client-visible INVALID_REQUEST rejections.
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}
