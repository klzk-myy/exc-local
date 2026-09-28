package accounts

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// SecretCodec seals/unseals credential material. Satisfied by
// auth.SecretBox (AES-256-GCM envelope); kept as an interface so this
// package never imports the auth cluster — the wiring layer adapts it.
type SecretCodec interface {
	Seal(plaintext []byte) ([]byte, error)
	Open(blob []byte) ([]byte, error)
}

// §8.8 scope vocabulary. Sub-account keys (Task 5.3.11 item 7) may carry
// read and/or trade only — `transfer` (the money-moving scope) and
// `admin` are never issuable on a sub-account key.
const (
	ScopeRead     = "read"
	ScopeTrade    = "trade"
	ScopeTransfer = "transfer"
	ScopeAdmin    = "admin"
)

var subAccountAllowedScopes = map[string]bool{
	ScopeRead:  true,
	ScopeTrade: true,
}

// IssuedAPIKey is the one-time provisioning response. Plaintext secret
// is returned exactly once; only key_id/key_hash (and the sealed
// secret_enc, when a SecretBox is configured) are persisted.
type IssuedAPIKey struct {
	ID        int64     `json:"id"`
	KeyID     string    `json:"key_id"` // public identifier (X-API-KEY)
	Secret    string    `json:"secret"` // plaintext — shown once
	AccountID int64     `json:"account_id"`
	Label     string    `json:"label"`
	Scopes    []string  `json:"scopes"`
	CreatedAt time.Time `json:"created_at"`
}

// APIKeySummary is the listable (never-secret) view of a key.
type APIKeySummary struct {
	ID        int64      `json:"id"`
	KeyID     string     `json:"key_id"`
	AccountID int64      `json:"account_id"`
	KeyPrefix string     `json:"key_prefix"`
	Label     string     `json:"label"`
	Scopes    []string   `json:"scopes"`
	Status    string     `json:"status"`
	CreatedAt time.Time  `json:"created_at"`
	RevokedAt *time.Time `json:"revoked_at,omitempty"`
}

// APIKeyService provisions programmatic credentials bound to a specific
// sub-account (Task 5.3.11 item 7). Keys are minted with account_id =
// sub-account id so every downstream scope/rate-limit check treats the
// key as that sub-account — the master cannot be impersonated through
// one.
//
// box is optional: when configured, the shared secret is also sealed into
// api_keys.secret_enc so Task 5.3.24's HMAC verifier can authenticate the
// key. Without it the row carries key_hash only — still listable and
// revocable, and fail-closed at verification (no verifiable secret).
type APIKeyService struct {
	pool *pgxpool.Pool
	subs *SubAccountService
	box  SecretCodec
}

// NewAPIKeyService builds the service. subs is used for the ownership
// check (key's account must be a live sub-account of the caller).
func NewAPIKeyService(pool *pgxpool.Pool, subs *SubAccountService, box SecretCodec) *APIKeyService {
	return &APIKeyService{pool: pool, subs: subs, box: box}
}

// IssueForSubAccount creates an API key restricted to subID. masterID
// must own subID; scopes must be non-empty and ⊆ {read, trade} — a
// request containing transfer/admin is rejected outright (fail-closed,
// never silently narrowed). createdBy is the master's owning user id.
func (s *APIKeyService) IssueForSubAccount(ctx context.Context, masterID, subID int64,
	scopes []string, label string, createdBy int64) (*IssuedAPIKey, error) {

	if len(scopes) == 0 {
		return nil, newError(CodeInvalidRequest, "api key requires at least one scope")
	}
	seen := map[string]bool{}
	for _, sc := range scopes {
		if !subAccountAllowedScopes[sc] {
			return nil, errorf(CodeInsufficientScope,
				"scope %q is not issuable on sub-account keys (allowed: read, trade)", sc)
		}
		if seen[sc] {
			return nil, errorf(CodeInvalidRequest, "duplicate scope %q", sc)
		}
		seen[sc] = true
	}

	owned, err := s.subs.IsSubAccountOf(ctx, masterID, subID)
	if err != nil {
		return nil, err
	}
	if !owned {
		return nil, errorf(CodeNotFound,
			"sub-account %d not found under master %d", subID, masterID)
	}

	// The key's user_id mirrors the sub-account's owning user — the
	// account family shares one user principal.
	var userID int64
	if err := s.pool.QueryRow(ctx,
		`SELECT user_id FROM accounts WHERE id = $1`, subID).Scan(&userID); err != nil {
		return nil, errorf("INTERNAL_ERROR", "read sub-account owner: %v", err)
	}

	idRaw := make([]byte, 9)
	secRaw := make([]byte, 32)
	if _, err := rand.Read(idRaw); err != nil {
		return nil, errorf("INTERNAL_ERROR", "key id generation: %v", err)
	}
	if _, err := rand.Read(secRaw); err != nil {
		return nil, errorf("INTERNAL_ERROR", "key secret generation: %v", err)
	}
	keyID := "sub_" + base64.RawURLEncoding.EncodeToString(idRaw)
	secret := base64.RawURLEncoding.EncodeToString(secRaw)
	sum := sha256.Sum256([]byte(keyID + "." + secret))
	keyHash := hex.EncodeToString(sum[:])

	var secretEnc []byte
	if s.box != nil {
		if secretEnc, err = s.box.Seal([]byte(secret)); err != nil {
			return nil, errorf("INTERNAL_ERROR", "seal key secret: %v", err)
		}
	}
	if createdBy == 0 {
		createdBy = userID
	}

	var id int64
	var createdAt time.Time
	err = s.pool.QueryRow(ctx,
		`INSERT INTO api_keys
		 (key_id, account_id, user_id, key_hash, secret_enc, key_prefix,
		  label, scopes, created_by)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		 RETURNING id, created_at`,
		keyID, subID, userID, keyHash, secretEnc, keyID,
		label, scopes, createdBy).
		Scan(&id, &createdAt)
	if err != nil {
		return nil, errorf("INTERNAL_ERROR", "insert api key: %v", err)
	}

	return &IssuedAPIKey{
		ID:        id,
		KeyID:     keyID,
		Secret:    secret,
		AccountID: subID,
		Label:     label,
		Scopes:    scopes,
		CreatedAt: createdAt,
	}, nil
}

// RevokeForSubAccount revokes keyID, verifying the master→sub→key
// ownership chain (Task 5.3.11 AC: master can revoke sub-account keys).
// Idempotent: revoking an already-revoked key succeeds silently.
func (s *APIKeyService) RevokeForSubAccount(ctx context.Context, masterID, subID, keyID int64) error {
	owned, err := s.subs.IsSubAccountOf(ctx, masterID, subID)
	if err != nil {
		return err
	}
	if !owned {
		return errorf(CodeNotFound, "sub-account %d not found under master %d", subID, masterID)
	}
	tag, err := s.pool.Exec(ctx,
		`UPDATE api_keys
		    SET status = 'REVOKED', revoked_at = now(),
		        revoke_reason = 'MASTER_REVOKE', updated_at = now()
		  WHERE id = $1 AND account_id = $2 AND status = 'ACTIVE'`,
		keyID, subID)
	if err != nil {
		return errorf("INTERNAL_ERROR", "revoke api key: %v", err)
	}
	if tag.RowsAffected() == 0 {
		var exists bool
		if err := s.pool.QueryRow(ctx,
			`SELECT EXISTS(SELECT 1 FROM api_keys WHERE id = $1 AND account_id = $2)`,
			keyID, subID).Scan(&exists); err != nil {
			return errorf("INTERNAL_ERROR", "revoke api key check: %v", err)
		}
		if !exists {
			return errorf(CodeNotFound, "api key %d not found on sub-account %d", keyID, subID)
		}
		// Already revoked — idempotent success.
	}
	return nil
}

// ListForSubAccount returns the non-secret view of a sub-account's keys.
func (s *APIKeyService) ListForSubAccount(ctx context.Context, masterID, subID int64) ([]APIKeySummary, error) {
	owned, err := s.subs.IsSubAccountOf(ctx, masterID, subID)
	if err != nil {
		return nil, err
	}
	if !owned {
		return nil, errorf(CodeNotFound,
			"sub-account %d not found under master %d", subID, masterID)
	}
	rows, err := s.pool.Query(ctx,
		`SELECT id, key_id, account_id, key_prefix, label, scopes, status, created_at, revoked_at
		   FROM api_keys WHERE account_id = $1 ORDER BY id`, subID)
	if err != nil {
		return nil, errorf("INTERNAL_ERROR", "list api keys: %v", err)
	}
	defer rows.Close()
	out := []APIKeySummary{}
	for rows.Next() {
		var k APIKeySummary
		var scopes []string
		if err := rows.Scan(&k.ID, &k.KeyID, &k.AccountID, &k.KeyPrefix, &k.Label,
			&scopes, &k.Status, &k.CreatedAt, &k.RevokedAt); err != nil {
			return nil, errorf("INTERNAL_ERROR", "scan api key: %v", err)
		}
		k.Scopes = scopes
		out = append(out, k)
	}
	return out, rows.Err()
}
