package auth

// webauthn.go — Phase-12 Task 12.3.7: WebAuthn Level 2 / FIDO2 passkey
// ceremonies (spec §12.6).
//
// Protocol verification (challenge, origin, RP ID, authenticator data,
// attestation statement, assertion signature, COSE public keys —
// ES256/RS256/EdDSA — and user-verification flags) is delegated to
// github.com/go-webauthn/webauthn. This package owns what the library
// deliberately does not: challenge issuance/storage with single-use
// semantics, credential persistence, the atomic sign-counter update, the
// clone-detection response (credential revocation + account freeze), and
// the §12.6 session-elevation invariant (two_factor_verified + amr:fido2).
//
// Two ceremonies are supported:
//   - Registration (authenticated): BeginRegistration → FinishRegistration.
//   - Step-up assertion (authenticated): BeginAssertion → FinishAssertion.
//   - Discoverable / passwordless passkey login (unauthenticated, for the
//     cluster-1 login handler): BeginPasskeyLogin → FinishPasskeyLogin.
//
// All verification failures emit WEBAUTHN_VERIFICATION_FAILED — internal
// detail (which check failed) is never surfaced to the caller.

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"time"

	"github.com/go-webauthn/webauthn/protocol"
	webauthnlib "github.com/go-webauthn/webauthn/webauthn"
)

// WebAuthn challenge Redis key prefix: webauthn:challenge:{id}.
const webAuthnChallengePrefix = "webauthn:challenge:"

// WebAuthnConfig carries relying-party identity. Origins must be fully
// qualified (scheme + host + optional port); ChallengeTTL defaults to the
// §12.6-specified ~60 seconds.
type WebAuthnConfig struct {
	RPID          string
	RPDisplayName string
	RPOrigins     []string
	ChallengeTTL  time.Duration
}

// WebAuthnCredential is the persisted credential row (migration 068).
type WebAuthnCredential struct {
	ID           int64
	UserID       int64
	CredentialID []byte
	PublicKey    []byte // COSE_Key (CBOR) as returned by attestation
	SignCount    uint32
	Transports   []string
	AAGUID       []byte
	Flags        uint8 // authenticator data flags byte captured at registration
	Name         string
	RevokedAt    *time.Time
	CreatedAt    time.Time
	LastUsedAt   *time.Time
}

// WebAuthnStore is the credential/user persistence seam (PG impl in
// webauthn_store.go).
type WebAuthnStore interface {
	CreateCredential(ctx context.Context, c WebAuthnCredential) (int64, error)
	// ActiveCredentialsForUser returns non-revoked credentials for ceremonies.
	ActiveCredentialsForUser(ctx context.Context, userID int64) ([]WebAuthnCredential, error)
	// UpdateSignCount records the verified post-ceremony counter value and
	// stamps last_used_at.
	UpdateSignCount(ctx context.Context, id int64, signCount uint32) error
	// RevokeCredential deactivates a credential (clone detection keeps the
	// row as a forensic record; it is never deleted).
	RevokeCredential(ctx context.Context, id int64) error
	// UserEmail resolves the display identifier for the RP user entity.
	UserEmail(ctx context.Context, userID int64) (string, error)
}

// WebAuthnChallengeStore stores a pending ceremony under
// webauthn:challenge:{id} with ~60s TTL and single-use consume.
type WebAuthnChallengeStore interface {
	PutChallenge(ctx context.Context, id string, payload []byte, ttl time.Duration) error
	// ConsumeChallenge returns the payload and deletes the key atomically
	// (Redis GETDEL). ok=false when missing or expired — fail-closed.
	ConsumeChallenge(ctx context.Context, id string) (payload []byte, ok bool, err error)
}

// SecurityFreezer is the clone-detection account-freeze seam (Task
// 12.3.12 part 2). The administrative FreezeService cannot be reused —
// it requires a human admin actor + second approver; a clone event is an
// automated machine response. Implemented by
// accounts.SecurityFreezeService (users.status + accounts.status).
type SecurityFreezer interface {
	FreezeUserAccounts(ctx context.Context, userID int64, reason string) error
}

// SecurityEventNotifier is the narrow notification seam for security
// events (lockout + clone detection). The Phase-12 notification service
// (Task 12.3.5, other agent) supplies the concrete implementation; a nil
// notifier leaves the event in the audit log only.
type SecurityEventNotifier interface {
	NotifySecurityEvent(ctx context.Context, userID int64, event string, attrs map[string]any) error
}

// Security-event kinds emitted by this package.
const (
	SecurityEventAuthLocked          = "account_locked_auth_failures"
	SecurityEventWebAuthnClone       = "webauthn_clone_detected"
	SecurityEventPasskeyAsserted     = "passkey_asserted"
	SecurityEventPasskeyRegistered   = "passkey_registered"
	SecurityEventSessionsRevokedAll  = "sessions_revoked_all"
	SecurityEventAntiPhishingUpdated = "anti_phishing_code_updated"
)

// WebAuthnService runs the ceremonies.
type WebAuthnService struct {
	w        *webauthnlib.WebAuthn
	store    WebAuthnStore
	chal     WebAuthnChallengeStore
	freezer  SecurityFreezer
	notifier SecurityEventNotifier
	logf     func(string, ...any)
	ttl      time.Duration
}

// NewWebAuthnService constructs the service; RPID and at least one origin
// are required — fail-closed on empty relying-party identity.
func NewWebAuthnService(cfg WebAuthnConfig, store WebAuthnStore, chal WebAuthnChallengeStore) (*WebAuthnService, error) {
	ttl := cfg.ChallengeTTL
	if ttl <= 0 {
		ttl = 60 * time.Second
	}
	w, err := webauthnlib.New(&webauthnlib.Config{
		RPID:          cfg.RPID,
		RPDisplayName: cfg.RPDisplayName,
		RPOrigins:     cfg.RPOrigins,
		Timeouts: webauthnlib.TimeoutsConfig{
			Login:        webauthnlib.TimeoutConfig{Enforce: true, Timeout: ttl, TimeoutUVD: ttl},
			Registration: webauthnlib.TimeoutConfig{Enforce: true, Timeout: ttl, TimeoutUVD: ttl},
		},
	})
	if err != nil {
		return nil, err
	}
	return &WebAuthnService{w: w, store: store, chal: chal, ttl: ttl}, nil
}

// WithFreezer/WithNotifier wire the optional security seams.
func (s *WebAuthnService) WithFreezer(f SecurityFreezer) *WebAuthnService {
	s.freezer = f
	return s
}

func (s *WebAuthnService) WithNotifier(n SecurityEventNotifier) *WebAuthnService {
	s.notifier = n
	return s
}

func (s *WebAuthnService) WithLogger(f func(string, ...any)) *WebAuthnService {
	s.logf = f
	return s
}

// ListCredentials is the account-settings read path.
func (s *WebAuthnService) ListCredentials(ctx context.Context, userID int64) ([]WebAuthnCredential, error) {
	return s.store.ActiveCredentialsForUser(ctx, userID)
}

// ---------------------------------------------------------------------------
// Registration ceremony
// ---------------------------------------------------------------------------

type webAuthnChallengeRecord struct {
	Kind    string                  `json:"kind"` // "registration" | "login" | "passkey"
	UserID  int64                   `json:"user_id"`
	Session webauthnlib.SessionData `json:"session"`
}

// BeginRegistration issues PublicKeyCredentialCreationOptions for the
// authenticated user. The library SessionData is stored under
// webauthn:challenge:{id} for ~60s, consumed exactly once.
func (s *WebAuthnService) BeginRegistration(ctx context.Context, userID int64) (challengeID string, options *protocol.CredentialCreation, err error) {
	user, err := s.loadUser(ctx, userID)
	if err != nil {
		return "", nil, wrapError(CodeAuthInternal, "webauthn user load", err)
	}
	creation, sessionData, err := s.w.BeginRegistration(user)
	if err != nil {
		return "", nil, newError(CodeWebAuthnFailed, "webauthn registration init failed")
	}
	challengeID, err = randomToken(24)
	if err != nil {
		return "", nil, wrapError(CodeAuthInternal, "webauthn challenge id", err)
	}
	if err := s.putChallenge(ctx, challengeID, webAuthnChallengeRecord{
		Kind: "registration", UserID: userID, Session: *sessionData,
	}); err != nil {
		return "", nil, err
	}
	return challengeID, creation, nil
}

// FinishRegistration verifies the attestation response and persists the
// credential. Returns the stored credential id.
func (s *WebAuthnService) FinishRegistration(ctx context.Context, userID int64, challengeID string, name string, attestationJSON []byte) (int64, error) {
	rec, ok, err := s.consumeChallenge(ctx, challengeID)
	if err != nil {
		return 0, wrapError(CodeAuthInternal, "webauthn challenge read", err)
	}
	if !ok || rec.Kind != "registration" || rec.UserID != userID {
		// Expired / replayed / foreign-user challenge — all fail closed.
		return 0, newError(CodeWebAuthnFailed, "webauthn registration challenge invalid")
	}
	user, err := s.loadUser(ctx, userID)
	if err != nil {
		return 0, wrapError(CodeAuthInternal, "webauthn user load", err)
	}
	parsed, err := protocol.ParseCredentialCreationResponseBody(bytes.NewReader(attestationJSON))
	if err != nil {
		return 0, newError(CodeWebAuthnFailed, "webauthn attestation parse failed")
	}
	cred, err := s.w.CreateCredential(user, rec.Session, parsed)
	if err != nil {
		return 0, newError(CodeWebAuthnFailed, "webauthn attestation verification failed")
	}
	transports := make([]string, 0, len(cred.Transport))
	for _, t := range cred.Transport {
		transports = append(transports, string(t))
	}
	row := WebAuthnCredential{
		UserID:       userID,
		CredentialID: cred.ID,
		PublicKey:    cred.PublicKey,
		SignCount:    cred.Authenticator.SignCount,
		Transports:   transports,
		AAGUID:       cred.Authenticator.AAGUID,
		Flags:        byte(parsed.Response.AttestationObject.AuthData.Flags),
		Name:         name,
	}
	id, err := s.store.CreateCredential(ctx, row)
	if err != nil {
		return 0, err
	}
	s.notify(ctx, userID, SecurityEventPasskeyRegistered, map[string]any{"credential_id": id})
	return id, nil
}

// ---------------------------------------------------------------------------
// Step-up assertion ceremony (authenticated MFA elevation)
// ---------------------------------------------------------------------------

// BeginAssertion issues PublicKeyCredentialRequestOptions scoped to the
// caller's registered credentials (allowCredentials populated).
func (s *WebAuthnService) BeginAssertion(ctx context.Context, userID int64) (challengeID string, options *protocol.CredentialAssertion, err error) {
	user, err := s.loadUser(ctx, userID)
	if err != nil {
		return "", nil, wrapError(CodeAuthInternal, "webauthn user load", err)
	}
	assertion, sessionData, err := s.w.BeginLogin(user)
	if err != nil {
		return "", nil, newError(CodeWebAuthnFailed, "webauthn login init failed")
	}
	challengeID, err = randomToken(24)
	if err != nil {
		return "", nil, wrapError(CodeAuthInternal, "webauthn challenge id", err)
	}
	if err := s.putChallenge(ctx, challengeID, webAuthnChallengeRecord{
		Kind: "login", UserID: userID, Session: *sessionData,
	}); err != nil {
		return "", nil, err
	}
	return challengeID, assertion, nil
}

// FinishAssertion verifies the assertion signature + counter, persists
// the new counter, and enforces the clone-detection response. It returns
// the credential row so the caller can elevate the session (amr:fido2)
// and reissue the access token — together these satisfy the §12.6
// invariant "assertion ⇒ strong MFA" in one logical step.
func (s *WebAuthnService) FinishAssertion(ctx context.Context, userID int64, challengeID string, assertionJSON []byte) (*WebAuthnCredential, error) {
	rec, ok, err := s.consumeChallenge(ctx, challengeID)
	if err != nil {
		return nil, wrapError(CodeAuthInternal, "webauthn challenge read", err)
	}
	if !ok || rec.Kind != "login" || rec.UserID != userID {
		return nil, newError(CodeWebAuthnFailed, "webauthn login challenge invalid")
	}
	user, rowMap, err := s.loadUserMapped(ctx, userID)
	if err != nil {
		return nil, wrapError(CodeAuthInternal, "webauthn user load", err)
	}
	parsed, err := protocol.ParseCredentialRequestResponseBody(bytes.NewReader(assertionJSON))
	if err != nil {
		return nil, newError(CodeWebAuthnFailed, "webauthn assertion parse failed")
	}
	cred, err := s.w.ValidateLogin(user, rec.Session, parsed)
	if err != nil {
		return nil, newError(CodeWebAuthnFailed, "webauthn assertion verification failed")
	}
	row := rowMap[string(cred.ID)]
	if cred.Authenticator.CloneWarning {
		s.cloneResponse(ctx, userID, row)
		return nil, newError(CodeWebAuthnFailed, "webauthn credential clone detected")
	}
	if err := s.store.UpdateSignCount(ctx, row.ID, cred.Authenticator.SignCount); err != nil {
		return nil, wrapError(CodeAuthInternal, "webauthn sign counter persist", err)
	}
	s.notify(ctx, userID, SecurityEventPasskeyAsserted, map[string]any{"credential_id": row.ID})
	row.SignCount = cred.Authenticator.SignCount
	return &row, nil
}

// ---------------------------------------------------------------------------
// Discoverable passkey login (unauthenticated; cluster-1 seam)
// ---------------------------------------------------------------------------

// BeginPasskeyLogin issues an allowCredentials-free assertion challenge
// for resident-key (discoverable) login.
func (s *WebAuthnService) BeginPasskeyLogin(ctx context.Context) (challengeID string, options *protocol.CredentialAssertion, err error) {
	assertion, sessionData, err := s.w.BeginDiscoverableLogin()
	if err != nil {
		return "", nil, newError(CodeWebAuthnFailed, "webauthn passkey login init failed")
	}
	challengeID, err = randomToken(24)
	if err != nil {
		return "", nil, wrapError(CodeAuthInternal, "webauthn challenge id", err)
	}
	if err := s.putChallenge(ctx, challengeID, webAuthnChallengeRecord{
		Kind: "passkey", UserID: 0, Session: *sessionData,
	}); err != nil {
		return "", nil, err
	}
	return challengeID, assertion, nil
}

// FinishPasskeyLogin verifies a discoverable assertion and returns the
// authenticated userID. The caller owns session creation; the returned
// credential + the "fido2" AMR method let the login handler satisfy
// §12.6 (possession + PIN/biometric ⇒ MFA when UV was performed).
func (s *WebAuthnService) FinishPasskeyLogin(ctx context.Context, challengeID string, assertionJSON []byte) (int64, *WebAuthnCredential, error) {
	rec, ok, err := s.consumeChallenge(ctx, challengeID)
	if err != nil {
		return 0, nil, wrapError(CodeAuthInternal, "webauthn challenge read", err)
	}
	if !ok || rec.Kind != "passkey" {
		return 0, nil, newError(CodeWebAuthnFailed, "webauthn passkey challenge invalid")
	}
	parsed, err := protocol.ParseCredentialRequestResponseBody(bytes.NewReader(assertionJSON))
	if err != nil {
		return 0, nil, newError(CodeWebAuthnFailed, "webauthn assertion parse failed")
	}
	var resolvedUserID int64
	handler := func(rawID, userHandle []byte) (webauthnlib.User, error) {
		uid, err := userHandleToID(userHandle)
		if err != nil {
			return nil, err
		}
		user, _, err := s.loadUserMapped(ctx, uid)
		if err != nil {
			return nil, err
		}
		resolvedUserID = uid
		return user, nil
	}
	user, cred, err := s.w.ValidatePasskeyLogin(handler, rec.Session, parsed)
	if err != nil {
		return 0, nil, newError(CodeWebAuthnFailed, "webauthn passkey verification failed")
	}
	uid := resolvedUserID
	if parsedUserID, perr := userHandleToID(user.WebAuthnID()); perr == nil {
		uid = parsedUserID
	}
	_, rowMap, err := s.loadUserMapped(ctx, uid)
	if err != nil {
		return 0, nil, wrapError(CodeAuthInternal, "webauthn user load", err)
	}
	row := rowMap[string(cred.ID)]
	if cred.Authenticator.CloneWarning {
		s.cloneResponse(ctx, uid, row)
		return 0, nil, newError(CodeWebAuthnFailed, "webauthn credential clone detected")
	}
	if err := s.store.UpdateSignCount(ctx, row.ID, cred.Authenticator.SignCount); err != nil {
		return 0, nil, wrapError(CodeAuthInternal, "webauthn sign counter persist", err)
	}
	s.notify(ctx, uid, SecurityEventPasskeyAsserted, map[string]any{"credential_id": row.ID})
	row.SignCount = cred.Authenticator.SignCount
	return uid, &row, nil
}

// ---------------------------------------------------------------------------
// Internals
// ---------------------------------------------------------------------------

// cloneResponse implements §12.6 clone detection (Task 12.3.12 part 2):
// a decreasing or non-increasing signature counter (when the stored value
// is non-zero) means the credential private key exists in more than one
// place. Response: revoke the credential, freeze the user's accounts, and
// record the event. The ceremony is rejected either way.
func (s *WebAuthnService) cloneResponse(ctx context.Context, userID int64, row WebAuthnCredential) {
	if s.logf != nil {
		s.logf("webauthn clone detection: user=%d credential_row=%d", userID, row.ID)
	}
	if row.ID != 0 {
		if err := s.store.RevokeCredential(ctx, row.ID); err != nil && s.logf != nil {
			s.logf("webauthn clone: credential revoke failed user=%d: %v", userID, err)
		}
	}
	if s.freezer != nil {
		if err := s.freezer.FreezeUserAccounts(ctx, userID, "webauthn credential clone detected"); err != nil && s.logf != nil {
			s.logf("webauthn clone: account freeze failed user=%d: %v", userID, err)
		}
	}
	s.notify(ctx, userID, SecurityEventWebAuthnClone, map[string]any{"credential_row": row.ID})
}

func (s *WebAuthnService) putChallenge(ctx context.Context, id string, rec webAuthnChallengeRecord) error {
	payload, err := json.Marshal(rec)
	if err != nil {
		return wrapError(CodeAuthInternal, "webauthn challenge encode", err)
	}
	if err := s.chal.PutChallenge(ctx, id, payload, s.ttl); err != nil {
		return wrapError(CodeAuthInternal, "webauthn challenge store", err)
	}
	return nil
}

func (s *WebAuthnService) consumeChallenge(ctx context.Context, id string) (webAuthnChallengeRecord, bool, error) {
	payload, ok, err := s.chal.ConsumeChallenge(ctx, id)
	if err != nil || !ok {
		return webAuthnChallengeRecord{}, ok, err
	}
	var rec webAuthnChallengeRecord
	if err := json.Unmarshal(payload, &rec); err != nil {
		return webAuthnChallengeRecord{}, false, wrapError(CodeAuthInternal, "webauthn challenge decode", err)
	}
	return rec, true, nil
}

func (s *WebAuthnService) notify(ctx context.Context, userID int64, event string, attrs map[string]any) {
	if s.notifier == nil {
		return
	}
	if err := s.notifier.NotifySecurityEvent(ctx, userID, event, attrs); err != nil && s.logf != nil {
		s.logf("security event %s notify failed user=%d: %v", event, userID, err)
	}
}

// loadUser adapts stored rows to the library's webauthn.User contract.
func (s *WebAuthnService) loadUser(ctx context.Context, userID int64) (webauthnlib.User, error) {
	u, _, err := s.loadUserMapped(ctx, userID)
	return u, err
}

func (s *WebAuthnService) loadUserMapped(ctx context.Context, userID int64) (webauthnlib.User, map[string]WebAuthnCredential, error) {
	email, err := s.store.UserEmail(ctx, userID)
	if err != nil {
		return nil, nil, err
	}
	rows, err := s.store.ActiveCredentialsForUser(ctx, userID)
	if err != nil {
		return nil, nil, err
	}
	creds := make([]webauthnlib.Credential, 0, len(rows))
	rowMap := make(map[string]WebAuthnCredential, len(rows))
	for _, r := range rows {
		transports := make([]protocol.AuthenticatorTransport, 0, len(r.Transports))
		for _, t := range r.Transports {
			transports = append(transports, protocol.AuthenticatorTransport(t))
		}
		creds = append(creds, webauthnlib.Credential{
			ID:        r.CredentialID,
			PublicKey: r.PublicKey,
			Transport: transports,
			Flags:     webauthnlib.NewCredentialFlags(protocol.AuthenticatorFlags(r.Flags)),
			Authenticator: webauthnlib.Authenticator{
				AAGUID:    r.AAGUID,
				SignCount: r.SignCount,
			},
		})
		rowMap[string(r.CredentialID)] = r
	}
	return webAuthnUser{
		id:    userHandle(userID),
		name:  email,
		creds: creds,
	}, rowMap, nil
}

// webAuthnUser implements webauthnlib.User.
type webAuthnUser struct {
	id    []byte
	name  string
	creds []webauthnlib.Credential
}

func (u webAuthnUser) WebAuthnID() []byte                            { return u.id }
func (u webAuthnUser) WebAuthnName() string                          { return u.name }
func (u webAuthnUser) WebAuthnDisplayName() string                   { return u.name }
func (u webAuthnUser) WebAuthnCredentials() []webauthnlib.Credential { return u.creds }

// userHandle encodes users.id as an 8-byte big-endian user handle — the
// non-personally-identifying opaque ID WebAuthn requires (§14.6.1).
func userHandle(userID int64) []byte {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], uint64(userID))
	return b[:]
}

func userHandleToID(h []byte) (int64, error) {
	if len(h) != 8 {
		return 0, newError(CodeWebAuthnFailed, "webauthn user handle malformed")
	}
	return int64(binary.BigEndian.Uint64(h)), nil
}
