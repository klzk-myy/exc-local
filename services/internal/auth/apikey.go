// Task 5.3.38 — Ed25519 and RSA programmatic API keys; also the Task
// 5.3.9 persistence for per-key IP allowlists and the HMAC-key issuance
// surface Task 5.3.24 signs against.
//
// Schema: migration 025 creates api_keys (HMAC secret_enc, scopes,
// ip_allowlist, expiry); migration 073 (spec §5.39) adds key_type /
// public_key / algorithm / rotates_from_id / overlap_until.
//
// Invariants (spec §8.8 item 3, §24 #283):
//   - asymmetric keys store the PUBLIC half only — private key material
//     never enters the platform;
//   - HMAC secrets are recoverable for verification, so they are stored
//     AES-256-GCM wrapped (secret_enc) under a SecretBox data key;
//   - scope matrix is {read, trade, transfer, admin} — anything else is
//     rejected at registration;
//   - rotation records lineage (rotates_from_id) and keeps the
//     predecessor valid until overlap_until;
//   - Ed25519 recommended; mandatory for FIX (Phase-18 checks KeyType).
package auth

import (
	"context"
	"crypto/ed25519"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"net/netip"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// API key types and signing algorithms (migration 073 enum + CHECK).
type KeyType string

const (
	KeyTypeHMAC    KeyType = "HMAC"
	KeyTypeEd25519 KeyType = "ED25519"
	KeyTypeRSA     KeyType = "RSA"
)

const (
	AlgHMACSHA256 = "HMAC-SHA256"
	AlgEdDSA      = "EdDSA"
	AlgRS256      = "RS256" // RSA PKCS#1 v1.5 + SHA-256
	AlgPS256      = "PS256" // RSA-PSS + SHA-256
)

// ValidScopes is the spec §8.8 item 3 API-key scope matrix.
var ValidScopes = map[string]bool{
	"read":     true,
	"trade":    true,
	"transfer": true,
	"admin":    true,
}

// Ed25519RotationAge is the §8.8 recommended rotation age surfaced to
// clients via NeedsRotation.
const Ed25519RotationAge = 90 * 24 * time.Hour

// MaxRotationOverlap bounds overlap_until on a rotated predecessor key —
// rotation windows are operational continuity, not a way to keep a dead
// key alive indefinitely.
const MaxRotationOverlap = 72 * time.Hour

// APIKey is one api_keys row (schema per migration 025 as extended by the
// developer-portal owner + 073 asymmetric columns).
type APIKey struct {
	ID            int64
	KeyID         string // public identifier sent as X-API-KEY
	AccountID     int64
	UserID        int64
	KeyHash       string // hex SHA-256 of the credential (fingerprint/lookup)
	KeyPrefix     string // display prefix — never the secret
	Label         string
	KeyType       KeyType
	Algorithm     string
	PublicKey     []byte // DER (PKIX); nil for HMAC
	SecretEnc     []byte // AES-GCM wrapped HMAC secret; nil for asymmetric
	Scopes        []string
	RateLimitTier string      // §8.3 tier bucket the key maps to
	IPAllowlist   IPAllowlist // parsed
	AllowlistRaw  []string    // as stored
	Status        string      // ACTIVE | REVOKED (status column is authoritative)
	ExpiresAt     *time.Time
	LastUsedAt    *time.Time
	LastUsedIP    string
	RevokedAt     *time.Time
	RevokeReason  string
	CreatedBy     *int64
	RotatesFromID *int64
	OverlapUntil  *time.Time
	// Task 13.3.8 auto-expiry bookkeeping (migration 208): the 90-day
	// no-allowlist policy strips trade/transfer scopes — the key row
	// survives, the revocation is recorded and restorable.
	PermissionsRevokedAt *time.Time
	RevokedScopes        []string   // privileged scopes stripped, restorable on allowlist config
	ExpiryNotifiedAt     *time.Time // T-7d warning sent (idempotency gate)
	ExpiryOverrideUntil  *time.Time // dual-control grace extension past the 90d deadline
	CreatedAt            time.Time
}

// Active reports the request-time validity: status ACTIVE, not revoked,
// not past expires_at, and (for rotated predecessors) not past
// overlap_until.
func (k APIKey) Active(now time.Time) bool {
	if k.Status != "" && k.Status != "ACTIVE" {
		return false
	}
	if k.RevokedAt != nil {
		return false
	}
	if k.ExpiresAt != nil && !now.Before(*k.ExpiresAt) {
		return false
	}
	if k.OverlapUntil != nil && !now.Before(*k.OverlapUntil) {
		return false
	}
	return true
}

// NeedsRotation reports whether an Ed25519 key has aged past the §8.8
// 90-day rotation recommendation.
func (k APIKey) NeedsRotation(now time.Time) bool {
	return k.KeyType == KeyTypeEd25519 && now.Sub(k.CreatedAt) > Ed25519RotationAge
}

// KeyRequest is a key-issuance/registration request.
type KeyRequest struct {
	AccountID     int64
	UserID        int64
	Label         string
	Algorithm     string   // EdDSA | RS256 | PS256 (asymmetric) — required for Register*/Rotate
	PublicKey     []byte   // PEM or DER PKIX — asymmetric only
	Scopes        []string // subset of ValidScopes
	RateLimitTier string   // §8.3 tier bucket; default STANDARD
	IPAllowlist   []string // IPs/CIDRs (Task 5.3.9); empty = unrestricted
	ExpiresAt     *time.Time
}

// keyStoreQuerier is the pgx surface used (pool or tx).
type keyStoreQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// KeyStore persists api_keys rows. box wraps/unwraps HMAC secrets; it may
// be nil only when HMAC issuance is never used — CreateHMAC then fails
// closed.
type KeyStore struct {
	pool *pgxpool.Pool
	box  *SecretBox
	now  func() time.Time
}

// NewKeyStore binds the store. box is required for HMAC keys; nil is
// legal for asymmetric-only deployments.
func NewKeyStore(pool *pgxpool.Pool, box *SecretBox) (*KeyStore, error) {
	if pool == nil {
		return nil, newError(CodeAuthInternal, "key store pool is nil")
	}
	return &KeyStore{pool: pool, box: box, now: time.Now}, nil
}

// newKeyID mints the public key identifier. api_keys.key_id is
// VARCHAR(32): "ak_" + 21 random bytes → 3+28 = 31 chars.
func newKeyID() (string, error) {
	t, err := randomToken(21)
	if err != nil {
		return "", err
	}
	return "ak_" + t, nil
}

// validateKeyRequest enforces scope + allowlist validity before insert.
func validateKeyRequest(req KeyRequest) (IPAllowlist, error) {
	for _, s := range req.Scopes {
		if !ValidScopes[s] {
			return nil, newError(CodeAPIKeyInvalid, "invalid scope "+s)
		}
	}
	al, err := ParseIPAllowlist(req.IPAllowlist)
	if err != nil {
		return nil, err
	}
	return al, nil
}

// CreateHMAC issues a legacy shared-secret key (Task 5.3.24 migration
// compat — deprecated for new institutional accounts per Task 5.3.38).
// Returns the row plus the raw secret, shown once and never stored raw.
func (s *KeyStore) CreateHMAC(ctx context.Context, req KeyRequest) (*APIKey, string, error) {
	if s.box == nil {
		return nil, "", newError(CodeAuthInternal, "HMAC issuance requires a configured secret box")
	}
	al, err := validateKeyRequest(req)
	if err != nil {
		return nil, "", err
	}
	keyID, err := newKeyID()
	if err != nil {
		return nil, "", wrapError(CodeAuthInternal, "key id generation", err)
	}
	rawSecret, err := randomToken(32)
	if err != nil {
		return nil, "", wrapError(CodeAuthInternal, "secret generation", err)
	}
	enc, err := s.box.Seal([]byte(rawSecret))
	if err != nil {
		return nil, "", wrapError(CodeAuthInternal, "secret wrap", err)
	}
	fp := sha256.Sum256([]byte(rawSecret))
	k, err := s.insertKey(ctx, s.pool, req, keyID, KeyTypeHMAC, AlgHMACSHA256, nil, enc, fp[:], al, nil, nil)
	if err != nil {
		return nil, "", err
	}
	return k, rawSecret, nil
}

// RegisterAsymmetric stores a public-key API key. Only Ed25519 (EdDSA)
// and RSA-2048/4096 (RS256/PS256) are accepted — private keys are never
// accepted or stored (§24 #283).
func (s *KeyStore) RegisterAsymmetric(ctx context.Context, req KeyRequest, kt KeyType) (*APIKey, error) {
	al, err := validateKeyRequest(req)
	if err != nil {
		return nil, err
	}
	pub, err := normalizePublicKey(kt, req.Algorithm, req.PublicKey)
	if err != nil {
		return nil, err
	}
	keyID, err := newKeyID()
	if err != nil {
		return nil, wrapError(CodeAuthInternal, "key id generation", err)
	}
	fp := sha256.Sum256(pub) // public-key fingerprint, not a secret hash
	return s.insertKey(ctx, s.pool, req, keyID, kt, req.Algorithm, pub, nil, fp[:], al, nil, nil)
}

// Rotate issues a successor key for predecessor keyID and bounds the
// predecessor's remaining life to overlap (≤72h; 0 = immediate revoke).
// The new key inherits the predecessor's account/scopes/allowlist unless
// overridden in req. Lineage lands on rotates_from_id for audit.
func (s *KeyStore) Rotate(ctx context.Context, predecessorKeyID string, req KeyRequest, overlap time.Duration) (*APIKey, error) {
	if overlap < 0 || overlap > MaxRotationOverlap {
		return nil, newError(CodeAPIKeyInvalid, "rotation overlap must be within 0..72h")
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return nil, wrapError(CodeAuthInternal, "rotate tx begin", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	old, err := s.getForUpdate(ctx, tx, predecessorKeyID)
	if err != nil {
		return nil, err
	}
	if !old.Active(s.now()) {
		return nil, newError(CodeAPIKeyNotFound, "cannot rotate an inactive key")
	}
	if old.KeyType == KeyTypeHMAC {
		return nil, newError(CodeAPIKeyInvalid, "rotate HMAC keys by re-issuance (new secret)")
	}
	// Inherit unset fields from the predecessor.
	if req.AccountID == 0 {
		req.AccountID = old.AccountID
	}
	if req.UserID == 0 {
		req.UserID = old.UserID
	}
	if req.Label == "" {
		req.Label = old.Label
	}
	if len(req.Scopes) == 0 {
		req.Scopes = old.Scopes
	}
	if len(req.IPAllowlist) == 0 {
		req.IPAllowlist = old.AllowlistRaw
	}
	al, err := validateKeyRequest(req)
	if err != nil {
		return nil, err
	}
	pub, err := normalizePublicKey(old.KeyType, req.Algorithm, req.PublicKey)
	if err != nil {
		return nil, err
	}
	keyID, err := newKeyID()
	if err != nil {
		return nil, wrapError(CodeAuthInternal, "key id generation", err)
	}
	fp := sha256.Sum256(pub)
	k, err := s.insertKey(ctx, tx, req, keyID, old.KeyType, req.Algorithm, pub, nil, fp[:], al, &old.ID, nil)
	if err != nil {
		return nil, err
	}
	// Bound the predecessor: overlap_until = now+overlap, or straight
	// revocation when the window is zero.
	if overlap > 0 {
		until := s.now().Add(overlap)
		if _, err := tx.Exec(ctx,
			`UPDATE api_keys SET overlap_until=$2, updated_at=now() WHERE id=$1`,
			old.ID, until); err != nil {
			return nil, wrapError(CodeAuthInternal, "rotation overlap update", err)
		}
	} else {
		if _, err := tx.Exec(ctx,
			`UPDATE api_keys SET status='REVOKED', revoked_at=now(), revoke_reason='ROTATED', updated_at=now() WHERE id=$1`,
			old.ID); err != nil {
			return nil, wrapError(CodeAuthInternal, "rotation revoke", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, wrapError(CodeAuthInternal, "rotate tx commit", err)
	}
	return k, nil
}

// Revoke marks a key revoked (status + revoked_at); idempotent — a dead
// key stays dead.
func (s *KeyStore) Revoke(ctx context.Context, keyID, reason string) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE api_keys SET status='REVOKED', revoked_at=now(), revoke_reason=$2, updated_at=now()
		 WHERE key_id=$1 AND revoked_at IS NULL`, keyID, reason)
	if err != nil {
		return wrapError(CodeAuthInternal, "key revoke", err)
	}
	if tag.RowsAffected() == 0 {
		return newError(CodeAPIKeyNotFound, "key not found or already revoked")
	}
	return nil
}

// Get returns the key row for keyID regardless of state — callers decide
// validity via Active(); VerifySignedRequest maps inactive to
// API_KEY_NOT_FOUND so revoked/expired keys are indistinguishable from
// unknown ones (no existence oracle).
func (s *KeyStore) Get(ctx context.Context, keyID string) (*APIKey, error) {
	return s.getOne(ctx, s.pool, `SELECT `+apiKeyCols+` FROM api_keys WHERE key_id=$1`, keyID)
}

func (s *KeyStore) getForUpdate(ctx context.Context, tx pgx.Tx, keyID string) (*APIKey, error) {
	return s.getOne(ctx, tx, `SELECT `+apiKeyCols+` FROM api_keys WHERE key_id=$1 FOR UPDATE`, keyID)
}

// apiKeyCols matches the migration-025 extended schema + 073 columns +
// 208 auto-expiry bookkeeping columns.
const apiKeyCols = `id, key_id, account_id, user_id, key_hash, key_prefix, label,
	key_type, algorithm, public_key, secret_enc, scopes, rate_limit_tier,
	ip_allowlist, status, expires_at, last_used_at, last_used_ip,
	revoke_reason, created_by, rotates_from_id, overlap_until, revoked_at,
	permissions_revoked_at, revoked_scopes, expiry_notified_at,
	expiry_override_until, created_at`

// scanKey reads one apiKeys row from rows/row.
func scanKey(scan func(dest ...any) error) (*APIKey, error) {
	var k APIKey
	var allowlist []string
	var lastUsedIP *netip.Addr
	var keyHash, keyPrefix, tier, status, reason *string
	err := scan(
		&k.ID, &k.KeyID, &k.AccountID, &k.UserID, &keyHash, &keyPrefix,
		&k.Label, &k.KeyType, &k.Algorithm, &k.PublicKey, &k.SecretEnc,
		&k.Scopes, &tier, &allowlist, &status, &k.ExpiresAt,
		&k.LastUsedAt, &lastUsedIP, &reason, &k.CreatedBy,
		&k.RotatesFromID, &k.OverlapUntil, &k.RevokedAt,
		&k.PermissionsRevokedAt, &k.RevokedScopes, &k.ExpiryNotifiedAt,
		&k.ExpiryOverrideUntil, &k.CreatedAt)
	if err != nil {
		return nil, err
	}
	if keyHash != nil {
		k.KeyHash = *keyHash
	}
	if keyPrefix != nil {
		k.KeyPrefix = *keyPrefix
	}
	if tier != nil {
		k.RateLimitTier = *tier
	}
	if status != nil {
		k.Status = *status
	}
	if reason != nil {
		k.RevokeReason = *reason
	}
	if lastUsedIP != nil {
		k.LastUsedIP = lastUsedIP.String()
	}
	k.AllowlistRaw = allowlist
	al, err := ParseIPAllowlist(allowlist)
	if err != nil {
		// A row with an unparseable allowlist is corrupt state — fail
		// closed rather than silently permitting any IP.
		return nil, wrapError(CodeAuthInternal, "stored allowlist corrupt", err)
	}
	k.IPAllowlist = al
	return &k, nil
}

func (s *KeyStore) getOne(ctx context.Context, q keyStoreQuerier, sql string, arg any) (*APIKey, error) {
	k, err := scanKey(q.QueryRow(ctx, sql, arg).Scan)
	if err == pgx.ErrNoRows {
		return nil, newError(CodeAPIKeyNotFound, "unknown api key")
	}
	if err != nil {
		return nil, wrapError(CodeAuthInternal, "key read", err)
	}
	return k, nil
}

// ListByAccount returns all live keys for the account (newest first) —
// backing API-key CRUD list (spec §12).
func (s *KeyStore) ListByAccount(ctx context.Context, accountID int64) ([]APIKey, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+apiKeyCols+` FROM api_keys
		 WHERE account_id=$1 AND status='ACTIVE' AND revoked_at IS NULL
		 ORDER BY id DESC`, accountID)
	if err != nil {
		return nil, wrapError(CodeAuthInternal, "key list", err)
	}
	defer rows.Close()
	out := []APIKey{}
	for rows.Next() {
		k, err := scanKey(rows.Scan)
		if err != nil {
			return nil, wrapError(CodeAuthInternal, "key scan", err)
		}
		out = append(out, *k)
	}
	return out, rows.Err()
}

// TouchLastUsed records key usage (audit/observability). Errors surface —
// the caller decides whether a bookkeeping failure rejects the request.
func (s *KeyStore) TouchLastUsed(ctx context.Context, keyID int64, ip string) error {
	var arg any
	if ip != "" {
		arg = ip
	}
	_, err := s.pool.Exec(ctx,
		`UPDATE api_keys SET last_used_at=now(), last_used_ip=$2::inet, updated_at=now()
		 WHERE id=$1`, keyID, arg)
	if err != nil {
		return wrapError(CodeAuthInternal, "key last-used update", err)
	}
	return nil
}

// RevealHMACSecret unwraps the stored shared secret for verification.
// Only HMAC keys carry secret_enc; asymmetric keys return an error.
func (s *KeyStore) RevealHMACSecret(k *APIKey) ([]byte, error) {
	if k.KeyType != KeyTypeHMAC || len(k.SecretEnc) == 0 {
		return nil, newError(CodeAuthInternal, "no HMAC secret stored for key")
	}
	if s.box == nil {
		return nil, newError(CodeAuthInternal, "secret box not configured")
	}
	raw, err := s.box.Open(k.SecretEnc)
	if err != nil {
		return nil, wrapError(CodeAuthInternal, "secret unwrap", err)
	}
	return raw, nil
}

// insertKey writes a new api_keys row and returns it loaded.
func (s *KeyStore) insertKey(ctx context.Context, q keyStoreQuerier, req KeyRequest,
	keyID string, kt KeyType, alg string, pub, secretEnc, hash []byte, al IPAllowlist,
	rotatesFrom *int64, overlapUntil *time.Time) (*APIKey, error) {

	var allowRaw []string // NULL = unrestricted (025 schema semantics)
	for _, p := range al {
		allowRaw = append(allowRaw, p.String())
	}
	scopes := req.Scopes
	if scopes == nil {
		scopes = []string{} // scopes is NOT NULL
	}
	tier := req.RateLimitTier
	if tier == "" {
		tier = "STANDARD"
	}
	var keyHash, keyPrefix any
	if len(hash) > 0 {
		keyHash = hex.EncodeToString(hash)
	}
	if len(keyID) > 8 {
		keyPrefix = keyID[:8]
	}
	var id int64
	err := q.QueryRow(ctx,
		`INSERT INTO api_keys
		 (key_id, account_id, user_id, key_hash, key_prefix, label, key_type,
		  algorithm, public_key, secret_enc, scopes, rate_limit_tier,
		  ip_allowlist, status, expires_at, created_by, rotates_from_id, overlap_until)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,'ACTIVE',$14,$15,$16,$17)
		 RETURNING id`,
		keyID, req.AccountID, req.UserID, keyHash, keyPrefix, req.Label,
		string(kt), alg, pub, secretEnc, scopes, tier,
		allowRaw, req.ExpiresAt, req.UserID, rotatesFrom, overlapUntil).Scan(&id)
	if err != nil {
		return nil, wrapError(CodeAuthInternal, "key insert", err)
	}
	return s.getOne(ctx, q, `SELECT `+apiKeyCols+` FROM api_keys WHERE id=$1`, id)
}

// normalizePublicKey validates and DER-normalizes a public key for the
// declared key type/algorithm. Accepts PEM ("-----BEGIN PUBLIC KEY-----")
// or raw DER.
func normalizePublicKey(kt KeyType, alg string, raw []byte) ([]byte, error) {
	if len(raw) == 0 {
		return nil, newError(CodeAPIKeyInvalid, "public key required")
	}
	der := raw
	if blk, _ := pem.Decode(raw); blk != nil {
		der = blk.Bytes
	}
	parsed, err := x509.ParsePKIXPublicKey(der)
	if err != nil {
		return nil, wrapError(CodeAsymmetricKeyInvalid, "unparseable public key", err)
	}
	switch kt {
	case KeyTypeEd25519:
		pub, ok := parsed.(ed25519.PublicKey)
		if !ok || len(pub) != ed25519.PublicKeySize {
			return nil, newError(CodeAsymmetricKeyInvalid, "not an Ed25519 public key")
		}
		if alg != AlgEdDSA {
			return nil, newError(CodeAPIKeyInvalid, "Ed25519 keys use algorithm EdDSA")
		}
	case KeyTypeRSA:
		pub, ok := parsed.(*rsa.PublicKey)
		if !ok {
			return nil, newError(CodeAsymmetricKeyInvalid, "not an RSA public key")
		}
		bits := pub.N.BitLen()
		if bits != 2048 && bits != 4096 {
			return nil, newError(CodeAsymmetricKeyInvalid, "RSA key must be 2048 or 4096 bits")
		}
		if alg != AlgRS256 && alg != AlgPS256 {
			return nil, newError(CodeAPIKeyInvalid, "RSA keys use algorithm RS256 or PS256")
		}
	default:
		return nil, newError(CodeAPIKeyInvalid, fmt.Sprintf("unsupported key type %q", kt))
	}
	return der, nil
}
