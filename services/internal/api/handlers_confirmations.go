// Task 20.3.8 — client trade-confirmation portal read.
//
// GET /api/v1/account/confirmations/{trade_id}?format=pdf|json
//
// Streams the archived confirmation document (application/pdf default;
// ?format=json for the machine-readable metadata doc). Generation and
// storage are sibling-owned (analytics.ConfirmationService); this
// handler is the portal retrieval path — the identity is the claims
// account, so a confirmation belonging to another account is
// NOT_FOUND under the sibling contract (foreign trade_ids are
// indistinguishable from missing ones; the admin scope overrides via
// the cross-account tracker for support tooling).
package api

import (
	"context"
	"net/http"
	"strconv"
	"strings"

	"exchange/internal/analytics"
	"exchange/internal/gateway"
	"exchange/internal/reporting"
)

// ConfirmationFileSource is the sibling read seam —
// analytics.ConfirmationService satisfies it directly.
type ConfirmationFileSource interface {
	GetFile(ctx context.Context, accountID, tradeID int64, format string) (*analytics.ConfirmationFile, error)
}

// ConfirmationReadDeps wires AccountConfirmation.
type ConfirmationReadDeps struct {
	Service ConfirmationFileSource
	// AdminLookup resolves a trade's latest confirmation row for the
	// admin scope (support/compliance tooling) — reporting's
	// PgConfirmationTracker satisfies it. Nil → admin reads behave as
	// ordinary own-account reads.
	AdminLookup reporting.ConfirmationTracker
}

// AccountConfirmation serves GET /api/v1/account/confirmations/{trade_id}.
func AccountConfirmation(d ConfirmationReadDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		accountID, claims, ok := claimsAccount(w, r)
		if !ok {
			return
		}
		if d.Service == nil {
			WriteError(w, "SERVICE_DEGRADED", "confirmation service unavailable",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		tradeID, err := strconv.ParseInt(r.PathValue("trade_id"), 10, 64)
		if err != nil || tradeID <= 0 {
			WriteError(w, "INVALID_REQUEST", "invalid trade_id",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		format := strings.ToLower(r.URL.Query().Get("format"))
		if format == "" {
			format = "pdf"
		}
		owner := accountID
		if claims.HasScope(gateway.ScopeAdmin) && d.AdminLookup != nil {
			// Admin path: resolve the owning account for the trade's
			// latest confirmation, then serve through the same read.
			row, err := d.AdminLookup.LatestByTrade(r.Context(), tradeID)
			if err != nil {
				writeServiceErr(w, r, err)
				return
			}
			if row == nil {
				WriteError(w, "NOT_FOUND", "confirmation not found",
					gateway.RequestIDFrom(r.Context()), nil)
				return
			}
			owner = row.AccountID
		}
		f, err := d.Service.GetFile(r.Context(), owner, tradeID, format)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		w.Header().Set("Content-Type", f.ContentType)
		w.Header().Set("Content-Disposition",
			`inline; filename="`+confirmationFilename(f.FileRef)+`"`)
		if f.Row != nil {
			w.Header().Set("X-Confirmation-Status", f.Row.Status)
			w.Header().Set("X-Confirmation-Version", strconv.Itoa(f.Row.Version))
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(f.Body)
	}
}

func confirmationFilename(key string) string {
	if i := strings.LastIndex(key, "/"); i >= 0 {
		return key[i+1:]
	}
	return key
}
