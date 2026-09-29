// Phase-13 Tasks 13.3.7/13.3.8 — proof-of-reserves read surface and the
// dual-control API-key expiry-extension endpoint.
//
//	GET /api/v1/solvency/latest                 — public signed snapshot
//	GET /api/v1/solvency/proof?account_id&currency — caller-scoped leaf proof
//	GET /api/v1/account/solvency-proof          — every currency leaf for the caller
//	PUT /api/v1/admin/api-keys/{id}/extend-expiry — four-eyes grace extension
//
// Zero peer-data contract: proof rows carry the caller's own salted leaf
// plus sibling HASHES only — no peer account ids or balances are ever
// emitted (enforced by construction in reconciliation.SolvencyPgStore,
// asserted again here by field whitelisting on the wire shape).
package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"exchange/internal/admin"
	"exchange/internal/auth"
	"exchange/internal/gateway"
	"exchange/internal/middleware"
	"exchange/internal/reconciliation"
	excerrors "exchange/pkg/errors"
)

// solvencyReader is the read seam — *reconciliation.SolvencyPgStore
// satisfies it; tests use fakes.
type solvencyReader interface {
	LatestSnapshot(ctx context.Context) (*reconciliation.Snapshot, error)
	ProofFor(ctx context.Context, accountID int64, currency string) (*reconciliation.ProofResult, error)
	ProofsForAccount(ctx context.Context, accountID int64) ([]reconciliation.ProofResult, error)
}

// SolvencyLatest — GET /api/v1/solvency/latest. Public: root, totals,
// reserve ratios and the detached signature; no per-account data.
func SolvencyLatest(src solvencyReader) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		snap, err := src.LatestSnapshot(r.Context())
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{
			"snapshot_id":        snap.ID,
			"generated_at":       snap.GeneratedAt,
			"leaf_count":         snap.LeafCount,
			"merkle_root":        snap.MerkleRoot,
			"liabilities":        snap.Liabilities,
			"nostro_assets":      snap.NostroAssets,
			"reserve_ratios":     snap.ReserveRatios,
			"solvent":            snap.Solvent,
			"signed_payload":     string(snap.SignedPayload),
			"signature":          snap.Signature,
			"signer_fingerprint": snap.SignerFingerprint,
			"signer_kind":        snap.SignerKind,
		})
	}
}

// SolvencyProof — GET /api/v1/solvency/proof?account_id=N&currency=USD.
// authRead; the requested account_id must be the caller's own — a foreign
// id (or a malformed one) is FORBIDDEN, never an oracle on whose leaves
// exist.
func SolvencyProof(src solvencyReader) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		_, claims, ok := claimsAccount(w, r)
		if !ok {
			return
		}
		raw := r.URL.Query().Get("account_id")
		if raw == "" {
			WriteError(w, "INVALID_REQUEST", "account_id is required",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		if rejectForeignAccount(w, r, claims, raw) {
			return
		}
		currency := r.URL.Query().Get("currency")
		if currency == "" {
			WriteError(w, "INVALID_REQUEST", "currency is required",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		proof, err := src.ProofFor(r.Context(), claims.AccountID, currency)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, proof)
	}
}

// AccountSolvencyProof — GET /api/v1/account/solvency-proof. Every
// currency leaf the caller holds in the latest published snapshot.
func AccountSolvencyProof(src solvencyReader) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		accountID, _, ok := claimsAccount(w, r)
		if !ok {
			return
		}
		proofs, err := src.ProofsForAccount(r.Context(), accountID)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		if proofs == nil {
			proofs = []reconciliation.ProofResult{}
		}
		WriteJSON(w, http.StatusOK, map[string]any{"proofs": proofs})
	}
}

// ---------------------------------------------------------------------------
// Task 13.3.8 — admin expiry extension (four-eyes)
// ---------------------------------------------------------------------------

// AdminAPIKeyExtendExpiry — PUT /api/v1/admin/api-keys/{id}/extend-expiry.
// The grant is NEVER applied inline: the handler submits a PENDING
// admin_dual_control_requests row; a second Super Admin's approval runs
// the registered executor inside the approval transaction (the grant and
// its four-eyes record commit atomically — spec §8.2).
//
// Body: {"until": "<RFC3339>", "reason": "..."} — until is bounded to
// now+180d (auth.APIKeyMaxExpiryOverride; re-validated in-tx at approve
// time so a stale submission cannot smuggle a longer horizon).
func AdminAPIKeyExtendExpiry(dual dualSubmitter, trustProxy bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		keyID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil || keyID <= 0 {
			WriteError(w, "INVALID_REQUEST", "api-key id required",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		var body struct {
			Until  string `json:"until"` // RFC3339 — new sweep deadline
			Reason string `json:"reason"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			WriteError(w, "INVALID_REQUEST", "malformed JSON body",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		until, err := time.Parse(time.RFC3339, body.Until)
		if err != nil || !until.After(time.Now()) {
			WriteError(w, "INVALID_REQUEST",
				"until must be a future RFC3339 time", gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		if until.After(time.Now().Add(auth.APIKeyMaxExpiryOverride)) {
			WriteError(w, "INVALID_REQUEST",
				"until exceeds the 180-day extension bound",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		actor, ok := adminUserID(w, r)
		if !ok {
			return
		}
		req, err := dual.Submit(r.Context(), admin.SubmitInput{
			Operation:  admin.OpAPIKeyExpiryExtend,
			TargetType: "api_key",
			TargetID:   strconv.FormatInt(keyID, 10),
			Payload: map[string]any{
				"api_key_id": keyID,
				"until":      until.UTC().Format(time.RFC3339Nano),
			},
			RequiredRole: admin.RoleSuperAdmin,
			RequestedBy:  actor,
			Reason:       body.Reason,
			ClientIP:     middleware.ClientIP(r, trustProxy),
		})
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusAccepted, map[string]any{
			"dual_control": "required", "request": req,
		})
	}
}

// RegisterAPIKeyExpiryExecutor attaches the four-eyes executor for
// api-key expiry extensions: on the second Super Admin's approval it
// decodes {api_key_id, until} and stamps expiry_override_until INSIDE
// the approval transaction. Bounds are re-checked at execute time (the
// approval may land minutes after submission; a now-past `until` fails
// closed rather than recording a dead grant).
func RegisterAPIKeyExpiryExecutor(dual *admin.DualControlService, ks *auth.KeyStore) {
	dual.RegisterExecutor(admin.OpAPIKeyExpiryExtend,
		func(ctx context.Context, tx pgx.Tx, req *admin.DualControlRequest) error {
			var p struct {
				APIKeyID int64  `json:"api_key_id"`
				Until    string `json:"until"`
			}
			if err := json.Unmarshal(req.Payload, &p); err != nil {
				return excerrors.New("INVALID_REQUEST",
					"api-key expiry extension payload not decodable")
			}
			until, err := time.Parse(time.RFC3339Nano, p.Until)
			if err != nil {
				return excerrors.New("INVALID_REQUEST",
					"api-key expiry extension until not decodable")
			}
			return ks.ExtendExpiryOverrideTx(ctx, tx, p.APIKeyID, until)
		})
}
