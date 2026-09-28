// Shared helpers for the Task 5.3.16/5.3.17 developer-surface handlers.
package api

import (
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"exchange/internal/gateway"
	excerrors "exchange/pkg/errors"
)

// parseSubjectID resolves a numeric user id out of a JWT sub; non-numeric
// subjects (e.g. "oauth2:client") yield an error — callers fall back to
// the account id for audit columns.
func parseSubjectID(sub string) (int64, error) {
	return strconv.ParseInt(sub, 10, 64)
}

// decodePublicKey accepts PEM text (the common case — the client pastes
// the PEM block verbatim) or a base64/hex DER blob.
func decodePublicKey(s string) []byte {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "-----") {
		return []byte(s) // PEM — normalizePublicKey decodes it
	}
	if der, err := base64.StdEncoding.DecodeString(s); err == nil {
		return der
	}
	if der, err := hex.DecodeString(s); err == nil {
		return der
	}
	return []byte(s) // let the store reject it cleanly
}

// writeKeyStoreError maps auth-cluster coded errors onto registered
// §23 codes — the auth codes themselves (API_KEY_INVALID & friends) are
// not yet in errs.Default, so the gateway surface translates rather than
// leaking an unregistered emission.
func writeKeyStoreError(w http.ResponseWriter, r *http.Request, err error) {
	var e *excerrors.Error
	if errors.As(err, &e) {
		switch e.Code {
		case "API_KEY_NOT_FOUND":
			WriteError(w, "NOT_FOUND",
				"api key not found", gateway.RequestIDFrom(r.Context()), nil)
			return
		case "API_KEY_INVALID", "ASYMMETRIC_KEY_INVALID":
			WriteError(w, "INVALID_REQUEST",
				e.Message, gateway.RequestIDFrom(r.Context()), nil)
			return
		}
	}
	WriteError(w, "SERVICE_DEGRADED",
		"key store unavailable", gateway.RequestIDFrom(r.Context()), nil)
}
