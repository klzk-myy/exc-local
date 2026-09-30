// Task 12.3.1 — user registration & password authentication.
//
// Surface (handlers: internal/api/authn.go):
//
//	POST /api/v1/auth/register             email + bcrypt password + country
//	POST /api/v1/auth/verify-email         emailed token link → verified
//	POST /api/v1/auth/login                two-phase: password → (TOTP) → JWT pair
//	POST /api/v1/auth/refresh              refresh rotation (session.go)
//	POST /api/v1/auth/logout               session revocation
//	POST /api/v1/auth/forgot-password      reset email (1h token expiry)
//	POST /api/v1/auth/reset-password       consume token → new hash, revoke sessions
//
// Storage contract (migration 027, spec §5.16):
//   - users.password_hash is bcrypt cost-12 — the §8.8 work factor shared
//     with oauth_clients (bcryptCost, oauth.go).
//   - users.email stores the normalized (lowercased) form so the UNIQUE
//     constraint is case-insensitive in effect.
//   - email_verified_at NULL = unverified; login is not blocked on it —
//     the verified flag rides the login/profile responses and funding
//     gates stay with KYC tiering (spec §12.1).
//   - users.totp_secret stores base64(SecretBox.Seal(seed)) — the exact
//     read contract accounts.PgxTOTPSecrets implements for the
//     withdrawal/close-all step-up paths; a nil box means the plaintext
//     base32 seed (dev-only deployments).
//
// Ephemeral state lives on the coordination Redis (noeviction):
//
//	verify_email:{sha256}   → user_id   TTL 24h — single-use link token
//	pwdreset:{sha256}       → user_id   TTL 1h  — spec §12.1 expiry
//	login_chal:{sha256}     → user_id   TTL 5m  — two-phase TOTP challenge
//	2fa:pending:{user_id}   → b32 seed  TTL 10m — spec §12.2 re-enrollment
//
// Fail-closed: unknown email, bad password, suspended user and TOTP
// failures never distinguish beyond the emitted code; the bcrypt
// compare always runs (dummy hash for unknown users, same as the
// OAuth2 grant path).
package auth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/mail"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
	"golang.org/x/crypto/bcrypt"
)

// errEmailTaken is the private duplicate-email sentinel — the handler
// deliberately returns the same "verification email sent" shape as a
// real registration (no existence oracle on the registration path).
var errEmailTaken = errors.New("auth: email already registered")

// Password policy (bcrypt input bound is 72 bytes — longer inputs are
// silently truncated by the algorithm, so the bound is enforced instead
// of letting the caller believe the tail mattered).
const (
	PasswordMinLen = 12
	PasswordMaxLen = 72
)

// Verification-link and challenge lifetimes.
const (
	EmailVerifyTokenTTL = 24 * time.Hour
	PasswordResetTTL    = time.Hour // spec §12.1 — "1h expiry"
	LoginChallengeTTL   = 5 * time.Minute
	TOTPPendingTTL      = 10 * time.Minute // spec §12.2 candidate-secret window
)

// userSessionScopes is the scope grant for interactive user sessions —
// the full client matrix (admin is venue-side only, never user-issued).
var userSessionScopes = []string{"read", "trade", "transfer"}

// ---------------------------------------------------------------------------
// UserStore — PostgreSQL persistence for users (002 + 027) and the
// default SPOT account created at registration (003).
// ---------------------------------------------------------------------------

// User is one users row as the auth/profile surface needs it.
type User struct {
	ID              int64
	Email           string
	Phone           string
	Country         string
	FullName        string
	Address         string
	Status          string // ACTIVE | SUSPENDED | CLOSED (§5.16)
	KYCStatus       string
	PasswordHash    string
	EmailVerifiedAt *time.Time
	CreatedAt       time.Time

	totpSecretEnc *string // stored form; unseal via store's SecretBox
}

// TOTPEnabled reports whether a TOTP secret is bound — the derived form
// of the spec §12.2 two_factor_enabled flag (totp_secret IS NOT NULL).
func (u User) TOTPEnabled() bool { return u.totpSecretEnc != nil && *u.totpSecretEnc != "" }

// UserStore persists users rows. box seals/unseals totp_secret the same
// way accounts.PgxTOTPSecrets reads it — nil means plaintext base32 in
// the column (dev only).
type UserStore struct {
	pool *pgxpool.Pool
	box  *SecretBox
	now  func() time.Time
}

// NewUserStore binds the store.
func NewUserStore(pool *pgxpool.Pool, box *SecretBox) (*UserStore, error) {
	if pool == nil {
		return nil, newError(CodeAuthInternal, "user store pool is nil")
	}
	return &UserStore{pool: pool, box: box, now: time.Now}, nil
}

const userCols = `id, email, phone, country, full_name, address, status,
	kyc_status, COALESCE(password_hash,''), totp_secret, email_verified_at, created_at`

func scanUser(scan func(dest ...any) error) (*User, error) {
	var u User
	var phone, country, name, addr *string
	err := scan(&u.ID, &u.Email, &phone, &country, &name, &addr,
		&u.Status, &u.KYCStatus, &u.PasswordHash, &u.totpSecretEnc,
		&u.EmailVerifiedAt, &u.CreatedAt)
	if err != nil {
		return nil, err
	}
	if phone != nil {
		u.Phone = *phone
	}
	if country != nil {
		u.Country = *country
	}
	if name != nil {
		u.FullName = *name
	}
	if addr != nil {
		u.Address = *addr
	}
	return &u, nil
}

// accountProvisionFunc mints the user's initial trading account inside
// the registration transaction — the production path writes the default
// SPOT row; the Task 8.5.3.2 demo seam substitutes DEMO+virtual-seed.
type accountProvisionFunc func(ctx context.Context, tx pgx.Tx, userID int64) (accountID int64, err error)

// provisionSpotAccount is the production default: one SPOT account.
func provisionSpotAccount(ctx context.Context, tx pgx.Tx, userID int64) (int64, error) {
	var accountID int64
	if err := tx.QueryRow(ctx,
		`INSERT INTO accounts (user_id, account_type) VALUES ($1,'SPOT') RETURNING id`,
		userID).Scan(&accountID); err != nil {
		return 0, wrapError(CodeAuthInternal, "account insert", err)
	}
	return accountID, nil
}

// CreateUserWithAccount inserts the user and their default SPOT account
// in one transaction — registration must never leave a user without a
// trading account context (api_keys.account_id is NOT NULL).
func (s *UserStore) CreateUserWithAccount(ctx context.Context, email, passwordHash, country string) (userID, accountID int64, err error) {
	return s.createUserTx(ctx, email, passwordHash, country, provisionSpotAccount)
}

// createUserTx is the shared registration transaction: user row first,
// then the provisioned account — both commit or roll back together, so
// a provisioning failure can never strand an account-less user.
func (s *UserStore) createUserTx(ctx context.Context, email, passwordHash, country string, provision accountProvisionFunc) (userID, accountID int64, err error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return 0, 0, wrapError(CodeAuthInternal, "register tx begin", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	err = tx.QueryRow(ctx,
		`INSERT INTO users (email, password_hash, country) VALUES ($1,$2,$3) RETURNING id`,
		email, passwordHash, country).Scan(&userID)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return 0, 0, errEmailTaken
		}
		return 0, 0, wrapError(CodeAuthInternal, "user insert", err)
	}
	if accountID, err = provision(ctx, tx, userID); err != nil {
		return 0, 0, err
	}
	if err = tx.Commit(ctx); err != nil {
		return 0, 0, wrapError(CodeAuthInternal, "register tx commit", err)
	}
	return userID, accountID, nil
}

// UserByEmail returns the normalized-email user, or (nil, nil) — the
// login path needs a soft miss, not an error, to keep the no-oracle
// timing path.
func (s *UserStore) UserByEmail(ctx context.Context, email string) (*User, error) {
	u, err := scanUser(s.pool.QueryRow(ctx,
		`SELECT `+userCols+` FROM users WHERE email=$1`, email).Scan)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, wrapError(CodeAuthInternal, "user read", err)
	}
	return u, nil
}

// UserByID returns the user or (nil, nil).
func (s *UserStore) UserByID(ctx context.Context, id int64) (*User, error) {
	u, err := scanUser(s.pool.QueryRow(ctx,
		`SELECT `+userCols+` FROM users WHERE id=$1`, id).Scan)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, wrapError(CodeAuthInternal, "user read", err)
	}
	return u, nil
}

// SetPasswordHash replaces the bcrypt digest (password change/reset).
func (s *UserStore) SetPasswordHash(ctx context.Context, id int64, hash string) error {
	if _, err := s.pool.Exec(ctx,
		`UPDATE users SET password_hash=$2, updated_at=now() WHERE id=$1`, id, hash); err != nil {
		return wrapError(CodeAuthInternal, "password update", err)
	}
	return nil
}

// MarkEmailVerified stamps email_verified_at; idempotent — a replayed
// link is a no-op once the column is set.
func (s *UserStore) MarkEmailVerified(ctx context.Context, id int64) error {
	if _, err := s.pool.Exec(ctx,
		`UPDATE users SET email_verified_at=COALESCE(email_verified_at, now()), updated_at=now()
		 WHERE id=$1`, id); err != nil {
		return wrapError(CodeAuthInternal, "email verify stamp", err)
	}
	return nil
}

// UpdateProfile writes the self-declared contact fields (Task 12.3.3).
// Verified legal PII belongs to kyc_profiles — these columns are the
// client-editable profile only.
func (s *UserStore) UpdateProfile(ctx context.Context, id int64, fullName, address, phone string) error {
	if _, err := s.pool.Exec(ctx,
		`UPDATE users SET full_name=$2, address=$3, phone=$4, updated_at=now()
		 WHERE id=$1`, id, fullName, address, phone); err != nil {
		return wrapError(CodeAuthInternal, "profile update", err)
	}
	return nil
}

// SetTOTPSecret persists the enrolled secret in the PgxTOTPSecerts
// storage contract — base64(SecretBox.Seal(seed)) when a box is wired,
// else the plaintext base32 seed — plus the backup-code digest list.
func (s *UserStore) SetTOTPSecret(ctx context.Context, id int64, seed string, backupDigests []string) error {
	stored := seed
	if s.box != nil {
		blob, err := s.box.Seal([]byte(seed))
		if err != nil {
			return wrapError(CodeAuthInternal, "totp secret seal", err)
		}
		stored = base64.StdEncoding.EncodeToString(blob)
	}
	if backupDigests == nil {
		backupDigests = []string{}
	}
	if _, err := s.pool.Exec(ctx,
		`UPDATE users SET totp_secret=$2, totp_backup_codes=$3, updated_at=now()
		 WHERE id=$1`, id, stored, backupDigests); err != nil {
		return wrapError(CodeAuthInternal, "totp secret write", err)
	}
	return nil
}

// ClearTOTP removes the active secret and all backup codes (disable).
func (s *UserStore) ClearTOTP(ctx context.Context, id int64) error {
	if _, err := s.pool.Exec(ctx,
		`UPDATE users SET totp_secret=NULL, totp_backup_codes='{}', updated_at=now()
		 WHERE id=$1`, id); err != nil {
		return wrapError(CodeAuthInternal, "totp clear", err)
	}
	return nil
}

// TOTPSecretForUser unwraps the enrolled secret for the user — same
// decode as accounts.PgxTOTPSecrets.TOTPSecretForAccount. Empty when
// not enrolled.
func (s *UserStore) TOTPSecretForUser(ctx context.Context, id int64) (string, error) {
	var secret *string
	err := s.pool.QueryRow(ctx, `SELECT totp_secret FROM users WHERE id=$1`, id).Scan(&secret)
	if err == pgx.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", wrapError(CodeAuthInternal, "totp secret read", err)
	}
	if secret == nil || *secret == "" {
		return "", nil
	}
	if s.box == nil {
		return *secret, nil
	}
	blob, err := base64.StdEncoding.DecodeString(*secret)
	if err != nil {
		return "", wrapError(CodeAuthInternal, "totp secret decode", err)
	}
	raw, err := s.box.Open(blob)
	if err != nil {
		return "", wrapError(CodeAuthInternal, "totp secret unwrap", err)
	}
	return string(raw), nil
}

// ConsumeBackupCode atomically removes one digest from
// totp_backup_codes — single-use by construction (array_remove inside
// the same UPDATE as the ANY() match).
func (s *UserStore) ConsumeBackupCode(ctx context.Context, id int64, digestHex string) (bool, error) {
	tag, err := s.pool.Exec(ctx,
		`UPDATE users SET totp_backup_codes=array_remove(totp_backup_codes,$2), updated_at=now()
		 WHERE id=$1 AND $2 = ANY(totp_backup_codes)`, id, digestHex)
	if err != nil {
		return false, wrapError(CodeAuthInternal, "backup code consume", err)
	}
	return tag.RowsAffected() > 0, nil
}

// DefaultAccountID resolves the user's primary (oldest master) account.
func (s *UserStore) DefaultAccountID(ctx context.Context, userID int64) (int64, error) {
	var id int64
	err := s.pool.QueryRow(ctx,
		`SELECT id FROM accounts WHERE user_id=$1 AND parent_account_id IS NULL
		 ORDER BY id LIMIT 1`, userID).Scan(&id)
	if err == pgx.ErrNoRows {
		return 0, nil
	}
	if err != nil {
		return 0, wrapError(CodeAuthInternal, "default account read", err)
	}
	return id, nil
}

// AccountIDsOfUser lists every account owned by the user — session
// revocation must sweep every per-account index (§8.2b precedent).
func (s *UserStore) AccountIDsOfUser(ctx context.Context, userID int64) ([]int64, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id FROM accounts WHERE user_id=$1`, userID)
	if err != nil {
		return nil, wrapError(CodeAuthInternal, "account list", err)
	}
	defer rows.Close()
	out := []int64{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, wrapError(CodeAuthInternal, "account scan", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// AccountTierName resolves the §8.3 rate-limit tier for the account —
// same fee_tiers join the WS gateway uses; empty when unresolvable.
func (s *UserStore) AccountTierName(ctx context.Context, accountID int64) (string, error) {
	var name *string
	if err := s.pool.QueryRow(ctx, `
		SELECT t.tier_name FROM accounts a
		LEFT JOIN fee_tiers t ON t.id = a.fee_tier_id
		WHERE a.id = $1`, accountID).Scan(&name); err != nil || name == nil {
		return "", nil // unresolvable tier is a soft miss, never a login failure
	}
	return *name, nil
}

// AccountKYCTier returns the account's kyc_tier (T0/T1/T2) for the
// login/profile responses.
func (s *UserStore) AccountKYCTier(ctx context.Context, accountID int64) (string, error) {
	var tier string
	if err := s.pool.QueryRow(ctx,
		`SELECT kyc_tier::text FROM accounts WHERE id=$1`, accountID).Scan(&tier); err != nil {
		return "", nil
	}
	return tier, nil
}

// ---------------------------------------------------------------------------
// TokenCache — ephemeral single-use token staging on the coordination
// Redis (verification links, reset links, TOTP challenges, pending
// 2FA candidate secrets).
// ---------------------------------------------------------------------------

type TokenCache interface {
	// Set stores key → value with a TTL.
	Set(ctx context.Context, key, value string, ttl time.Duration) error
	// Take atomically reads and deletes the key (GETDEL) — the
	// single-use link/challenge consume.
	Take(ctx context.Context, key string) (string, bool, error)
	// Get reads without consuming (pending-secret lookup).
	Get(ctx context.Context, key string) (string, bool, error)
	// Delete removes the key.
	Delete(ctx context.Context, key string) error
}

// RedisTokenCache implements TokenCache on the go-redis client.
type RedisTokenCache struct {
	c *redis.Client
}

// NewRedisTokenCache binds the cache.
func NewRedisTokenCache(c *redis.Client) *RedisTokenCache {
	return &RedisTokenCache{c: c}
}

func (t *RedisTokenCache) Set(ctx context.Context, key, value string, ttl time.Duration) error {
	if err := t.c.Set(ctx, key, value, ttl).Err(); err != nil {
		return wrapError(CodeAuthInternal, "token cache set", err)
	}
	return nil
}

func (t *RedisTokenCache) Take(ctx context.Context, key string) (string, bool, error) {
	v, err := t.c.GetDel(ctx, key).Result()
	if err == redis.Nil {
		return "", false, nil
	}
	if err != nil {
		return "", false, wrapError(CodeAuthInternal, "token cache take", err)
	}
	return v, true, nil
}

func (t *RedisTokenCache) Get(ctx context.Context, key string) (string, bool, error) {
	v, err := t.c.Get(ctx, key).Result()
	if err == redis.Nil {
		return "", false, nil
	}
	if err != nil {
		return "", false, wrapError(CodeAuthInternal, "token cache get", err)
	}
	return v, true, nil
}

func (t *RedisTokenCache) Delete(ctx context.Context, key string) error {
	if err := t.c.Del(ctx, key).Err(); err != nil {
		return wrapError(CodeAuthInternal, "token cache delete", err)
	}
	return nil
}

// tokenKey hashes a bearer token into its cache key — only the digest
// is ever stored, so a Redis read can never recover a usable link.
func tokenKey(prefix, token string) string {
	sum := sha256.Sum256([]byte(token))
	return prefix + hex.EncodeToString(sum[:])
}

// ---------------------------------------------------------------------------
// LoginRecorder — the login_history sink seam. Phase-12 Task 12.3.9
// owns the contract (login_history.go: LoginEvent, the
// LoginResult* vocabulary, the LoginRecorder interface) and migration
// 069; this service only emits. Optional: a nil recorder means the
// audit table is not wired — events are dropped, never guessed.
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// AuthnService — the registration/login/password-reset orchestrator.
// ---------------------------------------------------------------------------

// RegisterRequest is POST /api/v1/auth/register's payload.
type RegisterRequest struct {
	Email       string
	Password    string
	Country     string
	AcceptTerms bool
	IP          string
	UserAgent   string
}

// Registration is the create response. Duplicate emails return the same
// shape (no oracle) with EmailVerificationRequired still true.
type Registration struct {
	UserID                    int64
	AccountID                 int64
	Email                     string
	EmailVerificationRequired bool
}

// LoginRequest is POST /api/v1/auth/login's payload.
type LoginRequest struct {
	Email             string
	Password          string
	TOTPCode          string // phase-2 submission
	Challenge         string // phase-2 challenge token from phase-1
	IP                string
	UserAgent         string
	Device            string
	DeviceFingerprint string
}

// LoginOutcome is either the issued session bundle or the phase-1
// TOTP challenge (RequiresTOTP=true, no tokens).
type LoginOutcome struct {
	RequiresTOTP bool
	Challenge    string
	Issued       *IssuedSession
	User         *User
	KYCTier      string
}

// DemoProvisioner is the Phase-08.5 Task 8.5.3.2 demo-environment seam
// (*demo.Service at composition — this package never imports demo). On
// a deployment labelled "demo" registration substitutes a DEMO account
// + virtual balance for the default SPOT row inside the same
// transaction, and the login/refresh paths re-arm the 30-day
// inactivity deadline through TouchActivity.
type DemoProvisioner interface {
	// Enabled reports whether this deployment is the demo environment.
	Enabled() bool
	// ProvisionTx inserts the DEMO account (+ expiry deadline + virtual
	// seed) on the registration transaction.
	ProvisionTx(ctx context.Context, tx pgx.Tx, userID int64) (accountID int64, err error)
	// TouchActivity re-arms accounts.demo_expires_at; a non-DEMO
	// account is a no-op (the UPDATE predicate filters).
	TouchActivity(ctx context.Context, accountID int64) error
}

// AuthnService wires users + sessions + token cache + mailer.
type AuthnService struct {
	users    *UserStore
	sessions *SessionManager
	cache    TokenCache
	mail     Sender
	recorder LoginRecorder
	lockout  AuthLockout     // Phase-12 Task 12.3.12 seam (nil = disabled)
	demo     DemoProvisioner // Phase-08.5 Task 8.5.3.2 seam (nil = never demo)
	issuer   string          // otpauth/mail brand label
	mailBase string          // verification/reset link base URL
	now      func() time.Time
	logf     func(format string, args ...any)
}

// NewAuthnService binds the orchestrator. users, sessions, cache and
// mail are mandatory (a nil mailer would silently drop verification
// links — fail closed at construction per spec §2.7).
func NewAuthnService(users *UserStore, sessions *SessionManager, cache TokenCache, mail Sender) (*AuthnService, error) {
	if users == nil || sessions == nil || cache == nil || mail == nil {
		return nil, newError(CodeAuthInternal,
			"authn service requires users/sessions/cache/mail")
	}
	return &AuthnService{
		users: users, sessions: sessions, cache: cache, mail: mail,
		issuer: "exc.local", mailBase: "https://exc.local",
		now: time.Now, logf: func(string, ...any) {},
	}, nil
}

// WithRecorder attaches the login-history sink (cluster-2 seam; nil ok).
func (s *AuthnService) WithRecorder(r LoginRecorder) *AuthnService {
	s.recorder = r
	return s
}

// WithDemo attaches the Task 8.5.3.2 demo provisioner. A nil or
// disabled provisioner leaves registration on the production SPOT path
// — the demo substitution fires only when Enabled() reports the demo
// deployment label.
func (s *AuthnService) WithDemo(d DemoProvisioner) *AuthnService {
	s.demo = d
	return s
}

// touchDemo re-arms the demo inactivity deadline after a successful
// authentication (login or refresh — API use keeps a demo account
// alive). Best-effort: an activity-bump failure is logged, never fatal
// to an authenticated session.
func (s *AuthnService) touchDemo(ctx context.Context, accountID int64) {
	if s.demo == nil || !s.demo.Enabled() || accountID <= 0 {
		return
	}
	if err := s.demo.TouchActivity(ctx, accountID); err != nil {
		s.logf("auth: demo activity touch failed for account %d: %v", accountID, err)
	}
}

// WithLockout attaches the Task 12.3.12 brute-force lockout. The lock
// check runs before password verification and the counter feeds on every
// authentication failure (bad password AND failed 2FA); the fifth
// consecutive failure inside the 5-minute window rejects with 423
// ACCOUNT_LOCKED_AUTH_FAILURES for the following 15 minutes. A nil
// lockout leaves the gate disabled — the failure mode is counter-free,
// never silently permissive in the reverse direction.
func (s *AuthnService) WithLockout(l AuthLockout) *AuthnService {
	s.lockout = l
	return s
}

// WithMailBase overrides the link base URL (public origin of the SPA).
func (s *AuthnService) WithMailBase(base string) *AuthnService {
	if b := strings.TrimRight(strings.TrimSpace(base), "/"); b != "" {
		s.mailBase = b
	}
	return s
}

// WithLogger attaches the service logger for non-fatal observations.
func (s *AuthnService) WithLogger(logf func(string, ...any)) *AuthnService {
	if logf != nil {
		s.logf = logf
	}
	return s
}

// record emits a login-history event through the optional seam —
// audit-sink failures are logged, never fatal to authentication (the
// recorder is an observability surface, not an authorization gate).
func (s *AuthnService) record(ctx context.Context, ev LoginEvent) {
	if s.recorder == nil {
		return
	}
	if err := s.recorder.Record(ctx, ev); err != nil {
		s.logf("auth: login recorder write failed: %v", err)
	}
}

// validatePassword enforces the credential policy.
func validatePassword(pw string) error {
	if len(pw) < PasswordMinLen {
		return newError(CodeInvalidRequest,
			fmt.Sprintf("password must be at least %d characters", PasswordMinLen))
	}
	if len(pw) > PasswordMaxLen {
		return newError(CodeInvalidRequest,
			"password exceeds the bcrypt 72-byte input bound")
	}
	return nil
}

// validateCountry enforces ISO 3166-1 alpha-2.
func validateCountry(c string) (string, error) {
	c = strings.ToUpper(strings.TrimSpace(c))
	if len(c) != 2 || c[0] < 'A' || c[0] > 'Z' || c[1] < 'A' || c[1] > 'Z' {
		return "", newError(CodeInvalidRequest, "country must be an ISO 3166-1 alpha-2 code")
	}
	return c, nil
}

// Register creates the user + default SPOT account and dispatches the
// verification email. A duplicate email returns the same success shape
// with no mail sent — the endpoint is not an existence oracle.
func (s *AuthnService) Register(ctx context.Context, req RegisterRequest) (*Registration, error) {
	email := normalizeEmail(req.Email)
	parsed, parseErr := mail.ParseAddress(email)
	if parseErr != nil || parsed.Address != email || len(email) > 255 {
		return nil, newError(CodeInvalidRequest, "invalid email address")
	}
	country, err := validateCountry(req.Country)
	if err != nil {
		return nil, err
	}
	if !req.AcceptTerms {
		return nil, newError(CodeInvalidRequest, "terms of service must be accepted")
	}
	if err := validatePassword(req.Password); err != nil {
		return nil, err
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcryptCost)
	if err != nil {
		return nil, wrapError(CodeAuthInternal, "password hash", err)
	}
	// Task 8.5.3.2 — a demo deployment provisions account_type='DEMO'
	// (+virtual seed) instead of the default SPOT row, inside the same
	// registration transaction.
	provision := provisionSpotAccount
	if s.demo != nil && s.demo.Enabled() {
		provision = s.demo.ProvisionTx
	}
	userID, accountID, err := s.users.createUserTx(ctx, email, string(hash), country, provision)
	if err == errEmailTaken {
		return &Registration{Email: email, EmailVerificationRequired: true}, nil
	}
	if err != nil {
		return nil, err
	}
	tok, err := randomToken(32)
	if err != nil {
		return nil, wrapError(CodeAuthInternal, "verify token generation", err)
	}
	if err := s.cache.Set(ctx, tokenKey("verify_email:", tok),
		strconv.FormatInt(userID, 10), EmailVerifyTokenTTL); err != nil {
		return nil, err
	}
	if err := s.mail.Send(ctx, Message{
		To:      email,
		Kind:    MailEmailVerification,
		Subject: "Verify your exc.local account",
		Body: fmt.Sprintf("Verify your email within %dh: %s/verify-email?token=%s",
			int(EmailVerifyTokenTTL/time.Hour), s.mailBase, tok),
	}); err != nil {
		return nil, wrapError(CodeAuthInternal, "verification mail dispatch", err)
	}
	return &Registration{
		UserID: userID, AccountID: accountID, Email: email,
		EmailVerificationRequired: true,
	}, nil
}

// VerifyEmail consumes a verification token and marks the user.
func (s *AuthnService) VerifyEmail(ctx context.Context, token string) (int64, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return 0, newError(CodeInvalidRequest, "missing verification token")
	}
	uidStr, ok, err := s.cache.Take(ctx, tokenKey("verify_email:", token))
	if err != nil {
		return 0, err
	}
	if !ok {
		return 0, newError(CodeInvalidRequest, "invalid or expired verification token")
	}
	uid, err := strconv.ParseInt(uidStr, 10, 64)
	if err != nil {
		return 0, wrapError(CodeAuthInternal, "verify token decode", err)
	}
	if err := s.users.MarkEmailVerified(ctx, uid); err != nil {
		return 0, err
	}
	return uid, nil
}

// Login authenticates email+password. When the user has TOTP enrolled
// and supplies no code, phase 1 returns RequiresTOTP + a 5-minute
// challenge token; phase 2 resubmits email+password+challenge+totp_code
// and the session is issued with AMR ["pwd","totp"]. Sessions always
// bind the user's default account so downstream account-scoped gates
// resolve immediately.
func (s *AuthnService) Login(ctx context.Context, req LoginRequest) (*LoginOutcome, error) {
	email := normalizeEmail(req.Email)
	u, err := s.users.UserByEmail(ctx, email)
	if err != nil {
		return nil, err
	}
	// Task 12.3.12 lockout: the identifier is the user id when the
	// account resolves, else the normalized email (unknown-user failures
	// still count — a spray against one target email cannot bypass the
	// gate by never resolving a row).
	lockID := "email:" + email
	if u != nil {
		lockID = strconv.FormatInt(u.ID, 10)
	}
	if s.lockout != nil {
		locked, retryAfter, err := s.lockout.CheckLock(ctx, lockID)
		if err != nil {
			return nil, wrapError(CodeAuthInternal, "lockout check", err)
		}
		if locked {
			var uid int64
			if u != nil {
				uid = u.ID
			}
			s.record(ctx, LoginEvent{UserID: uid, Result: LoginResultLocked,
				IP: req.IP, UserAgent: req.UserAgent, DeviceFingerprint: req.DeviceFingerprint,
				Timestamp: s.now().UTC()})
			return nil, LockedError(retryAfter)
		}
	}
	// failAuth records the failure + lockout count, then reports the
	// canonical rejection: LockedError when the threshold just crossed,
	// INVALID_CREDENTIALS otherwise.
	failAuth := func(uid int64, result LoginResult, msg string) (*LoginOutcome, error) {
		s.record(ctx, LoginEvent{UserID: uid, Result: result,
			IP: req.IP, UserAgent: req.UserAgent, DeviceFingerprint: req.DeviceFingerprint,
			Timestamp: s.now().UTC()})
		if lockedErr, err := s.countFailure(ctx, lockID, uid, req); err != nil {
			return nil, err
		} else if lockedErr != nil {
			return nil, lockedErr
		}
		return nil, newError(CodeInvalidCredentials, msg)
	}
	// Timing parity: compare always runs (dummy hash on the miss path).
	hash := string(dummyBcryptHash)
	if u != nil && u.PasswordHash != "" {
		hash = u.PasswordHash
	}
	cmpErr := bcrypt.CompareHashAndPassword([]byte(hash), []byte(req.Password))
	if u == nil || u.PasswordHash == "" || cmpErr != nil {
		var uid int64
		if u != nil {
			uid = u.ID
		}
		return failAuth(uid, LoginResultFailed, "invalid credentials")
	}
	if u.Status != "ACTIVE" {
		// CLOSED/SUSPENDED share the FORBIDDEN wire code; the detail
		// stays in the message (never an existence oracle — the caller
		// already holds the password).
		return nil, newError(CodeForbidden, "account is "+strings.ToLower(u.Status))
	}
	amr := []string{"pwd"}
	if u.TOTPEnabled() {
		if req.TOTPCode == "" {
			// Phase 1: mint the challenge the phase-2 submission presents.
			chal, err := randomToken(24)
			if err != nil {
				return nil, wrapError(CodeAuthInternal, "challenge generation", err)
			}
			if err := s.cache.Set(ctx, tokenKey("login_chal:", chal),
				strconv.FormatInt(u.ID, 10), LoginChallengeTTL); err != nil {
				return nil, err
			}
			return &LoginOutcome{RequiresTOTP: true, Challenge: chal}, nil
		}
		// Phase 2: the challenge must bind this user, then the code must
		// verify — TOTP first, then a single-use backup code.
		chalUID, ok, err := s.cache.Take(ctx, tokenKey("login_chal:", req.Challenge))
		if err != nil {
			return nil, err
		}
		if !ok || chalUID != strconv.FormatInt(u.ID, 10) {
			s.record(ctx, LoginEvent{UserID: u.ID, Result: LoginResult2FAFailed,
				IP: req.IP, UserAgent: req.UserAgent, DeviceFingerprint: req.DeviceFingerprint,
				Timestamp: s.now().UTC()})
			if lockedErr, err := s.countFailure(ctx, lockID, u.ID, req); err != nil {
				return nil, err
			} else if lockedErr != nil {
				return nil, lockedErr
			}
			return nil, newError(CodeTwoFactorRequired, "invalid or expired 2FA challenge")
		}
		secret, err := s.users.TOTPSecretForUser(ctx, u.ID)
		if err != nil {
			return nil, err
		}
		ok = VerifyTOTP(secret, req.TOTPCode, s.now().UTC())
		if !ok {
			ok, err = s.users.ConsumeBackupCode(ctx, u.ID, BackupCodeDigest(req.TOTPCode))
			if err != nil {
				return nil, err
			}
		}
		if !ok {
			s.record(ctx, LoginEvent{UserID: u.ID, Result: LoginResult2FAFailed,
				IP: req.IP, UserAgent: req.UserAgent, DeviceFingerprint: req.DeviceFingerprint,
				Timestamp: s.now().UTC()})
			if lockedErr, err := s.countFailure(ctx, lockID, u.ID, req); err != nil {
				return nil, err
			} else if lockedErr != nil {
				return nil, lockedErr
			}
			return nil, newError(CodeTwoFactorRequired, "invalid 2FA code")
		}
		amr = append(amr, "totp")
	}
	accountID, err := s.users.DefaultAccountID(ctx, u.ID)
	if err != nil {
		return nil, err
	}
	tier, err := s.users.AccountTierName(ctx, accountID)
	if err != nil {
		return nil, err
	}
	iss, err := s.sessions.Issue(ctx, IssueRequest{
		UserID: strconv.FormatInt(u.ID, 10), AccountID: accountID,
		Tier: tier, Device: req.Device, IP: req.IP, UserAgent: req.UserAgent,
		AMR: amr, Scopes: userSessionScopes,
		TwoFactorVerified: len(amr) > 1,
	})
	if err != nil {
		return nil, err
	}
	if s.lockout != nil {
		if err := s.lockout.ClearFailures(ctx, lockID); err != nil {
			// Counter hygiene must not fail a verified login — log only.
			s.logf("auth: lockout clear failed for %s: %v", lockID, err)
		}
	}
	kycTier, _ := s.users.AccountKYCTier(ctx, accountID)
	s.touchDemo(ctx, accountID)
	s.record(ctx, LoginEvent{UserID: u.ID, Result: LoginResultSuccess,
		IP: req.IP, UserAgent: req.UserAgent, DeviceFingerprint: req.DeviceFingerprint,
		SessionID: iss.Session.ID, Timestamp: s.now().UTC()})
	return &LoginOutcome{Issued: iss, User: u, KYCTier: kycTier}, nil
}

// countFailure feeds the lockout counter for one failed authentication;
// it returns the 423 rejection when the fifth consecutive failure just
// armed the lock (and records the LOCKED history row), else (nil, nil).
func (s *AuthnService) countFailure(ctx context.Context, lockID string, uid int64, req LoginRequest) (error, error) {
	if s.lockout == nil {
		return nil, nil
	}
	_, locked, retryAfter, err := s.lockout.RecordFailure(ctx, lockID)
	if err != nil {
		return nil, wrapError(CodeAuthInternal, "lockout record", err)
	}
	if !locked {
		return nil, nil
	}
	s.record(ctx, LoginEvent{UserID: uid, Result: LoginResultLocked,
		IP: req.IP, UserAgent: req.UserAgent, DeviceFingerprint: req.DeviceFingerprint,
		Timestamp: s.now().UTC()})
	return LockedError(retryAfter), nil
}

// Refresh rotates the pair via the session manager (rotation + reuse
// detection are owned by session.go). A successful rotation also counts
// as demo activity — the 30-day inactivity clock resets on live API
// use, not just fresh logins.
func (s *AuthnService) Refresh(ctx context.Context, refreshToken string) (*IssuedSession, error) {
	iss, err := s.sessions.Refresh(ctx, refreshToken, userSessionScopes)
	if err != nil {
		return nil, err
	}
	s.touchDemo(ctx, iss.Session.AccountID)
	return iss, nil
}

// Logout revokes the session bound to the caller's JWT. A sessionless
// grant (oauth2 client_credentials) is a no-op success — logout is
// idempotent.
func (s *AuthnService) Logout(ctx context.Context, sessionID string) error {
	if sessionID == "" {
		return nil
	}
	return s.sessions.Revoke(ctx, sessionID)
}

// RequestPasswordReset mails a 1-hour single-use reset link when the
// address resolves; the response is identical either way (no oracle).
func (s *AuthnService) RequestPasswordReset(ctx context.Context, email string) error {
	u, err := s.users.UserByEmail(ctx, normalizeEmail(email))
	if err != nil {
		return err
	}
	if u == nil || u.Status != "ACTIVE" {
		return nil // identical success shape; nothing dispatched
	}
	tok, err := randomToken(32)
	if err != nil {
		return wrapError(CodeAuthInternal, "reset token generation", err)
	}
	if err := s.cache.Set(ctx, tokenKey("pwdreset:", tok),
		strconv.FormatInt(u.ID, 10), PasswordResetTTL); err != nil {
		return err
	}
	if err := s.mail.Send(ctx, Message{
		To:      u.Email,
		Kind:    MailPasswordReset,
		Subject: "exc.local password reset",
		Body: fmt.Sprintf("Reset your password within %d minute(s): %s/reset-password?token=%s",
			int(PasswordResetTTL/time.Minute), s.mailBase, tok),
	}); err != nil {
		return wrapError(CodeAuthInternal, "reset mail dispatch", err)
	}
	return nil
}

// ConfirmPasswordReset consumes the token, replaces the hash, and
// revokes every session — a reset is a credential-rotation event, all
// outstanding sessions must die (fail-closed §2.7).
func (s *AuthnService) ConfirmPasswordReset(ctx context.Context, token, newPassword string) (int64, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return 0, newError(CodeInvalidRequest, "missing reset token")
	}
	uidStr, ok, err := s.cache.Take(ctx, tokenKey("pwdreset:", token))
	if err != nil {
		return 0, err
	}
	if !ok {
		return 0, newError(CodeInvalidRequest, "invalid or expired reset token")
	}
	uid, err := strconv.ParseInt(uidStr, 10, 64)
	if err != nil {
		return 0, wrapError(CodeAuthInternal, "reset token decode", err)
	}
	if err := validatePassword(newPassword); err != nil {
		return 0, err
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(newPassword), bcryptCost)
	if err != nil {
		return 0, wrapError(CodeAuthInternal, "password hash", err)
	}
	if err := s.users.SetPasswordHash(ctx, uid, string(hash)); err != nil {
		return 0, err
	}
	if err := revokeAllUserSessions(ctx, s.users, s.sessions, uid); err != nil {
		return 0, err
	}
	return uid, nil
}

// ChangePassword verifies the current credential, replaces the hash,
// and revokes every OTHER session (the session performing the change
// survives — spec Task 12.3.3 semantics).
func (s *AuthnService) ChangePassword(ctx context.Context, userID int64,
	currentPassword, newPassword, keepSessionID string) error {

	u, err := s.users.UserByID(ctx, userID)
	if err != nil {
		return err
	}
	if u == nil || u.PasswordHash == "" ||
		bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(currentPassword)) != nil {
		return newError(CodeInvalidCredentials, "current password incorrect")
	}
	if err := validatePassword(newPassword); err != nil {
		return err
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(newPassword), bcryptCost)
	if err != nil {
		return wrapError(CodeAuthInternal, "password hash", err)
	}
	if err := s.users.SetPasswordHash(ctx, userID, string(hash)); err != nil {
		return err
	}
	return s.revokeOtherSessions(ctx, userID, keepSessionID)
}

// revokeAllUserSessions kills every session bound to the user — the
// user-index set plus every per-account index. Shared by the
// password-reset flow, the 2FA disable flow, and account kill paths.
func revokeAllUserSessions(ctx context.Context, users *UserStore,
	sessions *SessionManager, userID int64) error {

	uid := strconv.FormatInt(userID, 10)
	if err := sessions.RevokeAll(ctx, 0, uid); err != nil {
		return err
	}
	accounts, err := users.AccountIDsOfUser(ctx, userID)
	if err != nil {
		return err
	}
	for _, a := range accounts {
		if err := sessions.RevokeAll(ctx, a, uid); err != nil {
			return err
		}
	}
	return nil
}

// revokeOtherSessions kills every session except keepSessionID.
func (s *AuthnService) revokeOtherSessions(ctx context.Context, userID int64, keepSID string) error {
	uid := strconv.FormatInt(userID, 10)
	seen := map[string]bool{}
	listIdx := func(accountID int64) error {
		sessions, err := s.sessions.List(ctx, accountID, uid)
		if err != nil {
			return err
		}
		for _, sess := range sessions {
			if sess.ID == keepSID || seen[sess.ID] {
				continue
			}
			seen[sess.ID] = true
			if err := s.sessions.Revoke(ctx, sess.ID); err != nil {
				return err
			}
		}
		return nil
	}
	if err := listIdx(0); err != nil {
		return err
	}
	accounts, err := s.users.AccountIDsOfUser(ctx, userID)
	if err != nil {
		return err
	}
	for _, a := range accounts {
		if err := listIdx(a); err != nil {
			return err
		}
	}
	return nil
}
