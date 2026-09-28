// Task 5.3.16 — developer-portal API-key management over the existing
// auth.KeyStore machinery (migration 025 + 073).
//
//	POST   /api/v1/developer/api-keys       — issue/register a key
//	GET    /api/v1/developer/api-keys       — list the account's live keys
//	DELETE /api/v1/developer/api-keys/{id}  — revoke (idempotent 404-safe)
//
// Supported key material (spec §8.8):
//   - key_type=HMAC → server mints the shared secret, returned exactly
//     once in the create response (secret_enc storage, AES-256-GCM);
//   - key_type=ED25519/RSA → caller supplies the PUBLIC key (PEM or DER);
//     private material is never accepted or stored.
//
// rate_limit_tier selects the §8.3 per-key quota bucket — basic,
// standard, professional or institutional (case-insensitive; stored
// lowercase so ratelimit.ParseTier resolves it — see the note in
// RateTierOrDefault).
package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"exchange/internal/auth"
	"exchange/internal/gateway"
	"exchange/internal/ratelimit"
)

// rateTierOrDefault validates a client-supplied rate_limit_tier against
// the public §8.3 ladder and returns it lowercased. The api_keys column
// is read by ratelimit.ParseTier, which fails closed to TierPublic on
// unknown values — normalizing to lowercase here keeps the stored tier
// resolvable (the historical insertKey default "STANDARD" predates
// ParseTier's lowercase vocabulary; normalized writes avoid inheriting
// that mismatch for portal-issued keys).
func rateTierOrDefault(in string) (string, bool) {
	if in == "" {
		return string(ratelimit.TierStandard), true
	}
	t := ratelimit.ParseTier(strings.ToLower(in))
	switch t {
	case ratelimit.TierBasic, ratelimit.TierStandard,
		ratelimit.TierProfessional, ratelimit.TierInstitutional:
		return string(t), true
	}
	// public/admin are not sellable key tiers — reject rather than
	// silently up/downgrade.
	return "", false
}

// apiKeyView is the client-safe projection — key_hash, key_prefix and
// secret_enc never leave the store.
type apiKeyView struct {
	KeyID         string     `json:"key_id"`
	Label         string     `json:"label"`
	KeyType       string     `json:"key_type"`
	Algorithm     string     `json:"algorithm,omitempty"`
	Scopes        []string   `json:"scopes"`
	RateLimitTier string     `json:"rate_limit_tier"`
	IPAllowlist   []string   `json:"ip_allowlist,omitempty"`
	Status        string     `json:"status"`
	ExpiresAt     *time.Time `json:"expires_at,omitempty"`
	LastUsedAt    *time.Time `json:"last_used_at,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
	NeedsRotation bool       `json:"needs_rotation"`
}

func keyView(k auth.APIKey) apiKeyView {
	return apiKeyView{
		KeyID: k.KeyID, Label: k.Label, KeyType: string(k.KeyType),
		Algorithm: k.Algorithm, Scopes: k.Scopes,
		RateLimitTier: k.RateLimitTier, IPAllowlist: k.AllowlistRaw,
		Status: k.Status, ExpiresAt: k.ExpiresAt, LastUsedAt: k.LastUsedAt,
		CreatedAt: k.CreatedAt, NeedsRotation: k.NeedsRotation(time.Now()),
	}
}

// DeveloperAPIKeys returns create/list/revoke handlers bound to the
// shared auth.KeyStore (Task 5.3.38 machinery).
func DeveloperAPIKeys(ks *auth.KeyStore) (create, list, revoke http.HandlerFunc) {
	create = func(w http.ResponseWriter, r *http.Request) {
		claims := auth.ClaimsFrom(r.Context())
		if claims == nil || claims.AccountID == 0 {
			WriteError(w, "UNAUTHORIZED",
				"authentication required", gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		var body struct {
			Label         string   `json:"label"`
			KeyType       string   `json:"key_type"`   // HMAC | ED25519 | RSA (default HMAC)
			Algorithm     string   `json:"algorithm"`  // EdDSA | RS256 | PS256 for asymmetric
			PublicKey     string   `json:"public_key"` // PEM or DER-hex/base64 — asymmetric only
			Scopes        []string `json:"scopes"`     // subset of read|trade|transfer|admin
			RateLimitTier string   `json:"rate_limit_tier"`
			IPAllowlist   []string `json:"ip_allowlist"`
			ExpiresAt     *string  `json:"expires_at"` // RFC3339
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			WriteError(w, "INVALID_REQUEST",
				"malformed body", gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		tier, ok := rateTierOrDefault(body.RateLimitTier)
		if !ok {
			WriteError(w, "INVALID_REQUEST",
				"rate_limit_tier must be basic|standard|professional|institutional",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		var expires *time.Time
		if body.ExpiresAt != nil {
			t, err := time.Parse(time.RFC3339, *body.ExpiresAt)
			if err != nil || !t.After(time.Now()) {
				WriteError(w, "INVALID_REQUEST",
					"expires_at must be a future RFC3339 time", gateway.RequestIDFrom(r.Context()), nil)
				return
			}
			expires = &t
		}
		var userID int64
		if n, err := parseSubjectID(claims.Subject); err == nil {
			userID = n
		}
		req := auth.KeyRequest{
			AccountID: claims.AccountID, UserID: userID,
			Label: body.Label, Algorithm: body.Algorithm,
			Scopes: body.Scopes, RateLimitTier: tier,
			IPAllowlist: body.IPAllowlist, ExpiresAt: expires,
		}
		switch kt := auth.KeyType(strings.ToUpper(body.KeyType)); kt {
		case "", auth.KeyTypeHMAC:
			k, secret, err := ks.CreateHMAC(r.Context(), req)
			if err != nil {
				writeKeyStoreError(w, r, err)
				return
			}
			WriteJSON(w, http.StatusCreated, map[string]any{
				"key": keyView(*k),
				// shown exactly once — never retrievable afterwards.
				"secret": secret,
				"notice": "store the secret now; it is never shown again",
			})
		case auth.KeyTypeEd25519, auth.KeyTypeRSA:
			if body.PublicKey == "" {
				WriteError(w, "INVALID_REQUEST",
					"public_key required for asymmetric keys", gateway.RequestIDFrom(r.Context()), nil)
				return
			}
			req.PublicKey = decodePublicKey(body.PublicKey)
			k, err := ks.RegisterAsymmetric(r.Context(), req, kt)
			if err != nil {
				writeKeyStoreError(w, r, err)
				return
			}
			WriteJSON(w, http.StatusCreated, map[string]any{"key": keyView(*k)})
		default:
			WriteError(w, "INVALID_REQUEST",
				"key_type must be HMAC | ED25519 | RSA", gateway.RequestIDFrom(r.Context()), nil)
		}
	}
	list = func(w http.ResponseWriter, r *http.Request) {
		claims := auth.ClaimsFrom(r.Context())
		if claims == nil || claims.AccountID == 0 {
			WriteError(w, "UNAUTHORIZED",
				"authentication required", gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		keys, err := ks.ListByAccount(r.Context(), claims.AccountID)
		if err != nil {
			WriteError(w, "SERVICE_DEGRADED",
				"key store unavailable", gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		out := make([]apiKeyView, 0, len(keys))
		for _, k := range keys {
			out = append(out, keyView(k))
		}
		WriteJSON(w, http.StatusOK, map[string]any{"api_keys": out})
	}
	revoke = func(w http.ResponseWriter, r *http.Request) {
		claims := auth.ClaimsFrom(r.Context())
		if claims == nil || claims.AccountID == 0 {
			WriteError(w, "UNAUTHORIZED",
				"authentication required", gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		keyID := r.PathValue("id")
		if keyID == "" {
			WriteError(w, "INVALID_REQUEST",
				"missing key id", gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		// Ownership check before revoke — a foreign key id must be
		// indistinguishable from unknown (no existence oracle).
		k, err := ks.Get(r.Context(), keyID)
		if err != nil || k.AccountID != claims.AccountID {
			WriteError(w, "NOT_FOUND",
				"api key not found", gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		if err := ks.Revoke(r.Context(), keyID, "REVOKED_BY_OWNER"); err != nil {
			if strings.Contains(err.Error(), "already revoked") ||
				strings.Contains(err.Error(), "not found") {
				WriteError(w, "NOT_FOUND",
					"api key not found", gateway.RequestIDFrom(r.Context()), nil)
				return
			}
			WriteError(w, "SERVICE_DEGRADED",
				"key store unavailable", gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"key_id": keyID, "status": "REVOKED"})
	}
	return create, list, revoke
}
