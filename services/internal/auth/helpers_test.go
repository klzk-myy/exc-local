// Shared test helpers for the auth package.
package auth

import (
	stderrors "errors"
	"testing"

	excerrors "exchange/pkg/errors"
)

// codeIs reports whether err carries the given machine code.
func codeIs(err error, code string) bool {
	var e *excerrors.Error
	return stderrors.As(err, &e) && e.Code == code
}

// requireCode fails unless err carries code.
func requireCode(t *testing.T, err error, code string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected error %s, got nil", code)
	}
	if !codeIs(err, code) {
		t.Fatalf("expected code %s, got %v", code, err)
	}
}
