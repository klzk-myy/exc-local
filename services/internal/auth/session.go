// Task 5.3.10 — session management with concurrent limits, plus the
// refresh-token lifecycle half of Task 5.3.1 (issue / refresh / revoke).
//
// Canonical contract:
//   - spec §4.1: session:{token} HASH, TTL 3600s, fields user_id /
//     account_id / tier — this file keeps those exact field names and the
//     canonical key prefix so the WS gateway (Phase-06) reads the same
//     shape.
//   - spec §8.8 item 3: idle 30min vs absolute 8h timeouts; oldest-first
//     eviction at 5/account or 20/IP caps; refresh rotation + reuse
//     detection.
//   - spec §8.1: session stored in Redis with 1h TTL — the Redis TTL is
//     the *storage* bound (re-armed on activity); idle/absolute policy is
//     enforced at validation, so a session idle 31 minutes rejects even
//     while its hash still exists.
//
// Redis keyspace (session-internal indexes; §4.1 defines only the hash):
//
//	session:{sid}              HASH   canonical session record
//	sess:acct:{accountID}      ZSET   member=sid score=created unixnano —
//	                                  per-account cap + listing + eviction
//	sess:user:{userID}         ZSET   same when no account bound yet
//	sess:ip:{ip}               ZSET   per-IP cap (20)
//	refresh:{sha256(token)}    STRING → sid, TTL = 7d refresh lifetime
//	refresh_used:{sha256}      STRING → sid, TTL = 7d; reuse detector
//
// Fail-closed: any store error propagates as AUTH_INTERNAL — sessions are
// never silently presumed valid (spec §2.7).
package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"time"
)

// SessionConfig carries the spec §8.8 lifecycle constants. Zero values
// fall back to the canonical figures.
type SessionConfig struct {
	MaxPerAccount int           // concurrent cap per account (default 5)
	MaxPerIP      int           // concurrent cap per source IP (default 20)
	IdleTTL       time.Duration // idle timeout (default 30min)
	AbsoluteTTL   time.Duration // absolute session lifetime (default 8h)
	RefreshTTL    time.Duration // refresh-token lifetime (default 7d)
	StorageTTL    time.Duration // Redis hash TTL bound (default 1h, §4.1)
}

func (c *SessionConfig) withDefaults() {
	if c.MaxPerAccount <= 0 {
		c.MaxPerAccount = 5
	}
	if c.MaxPerIP <= 0 {
		c.MaxPerIP = 20
	}
	if c.IdleTTL <= 0 {
		c.IdleTTL = 30 * time.Minute
	}
	if c.AbsoluteTTL <= 0 {
		c.AbsoluteTTL = 8 * time.Hour
	}
	if c.RefreshTTL <= 0 {
		c.RefreshTTL = 7 * 24 * time.Hour
	}
	if c.StorageTTL <= 0 {
		c.StorageTTL = time.Hour
	}
}

// Session is one active session record as stored under session:{sid}.
type Session struct {
	ID           string    `json:"id"`
	UserID       string    `json:"user_id"`
	AccountID    int64     `json:"account_id"`
	Tier         string    `json:"tier"`
	Device       string    `json:"device,omitempty"`     // client-supplied device label
	IP           string    `json:"ip,omitempty"`         // login source IP (device tracking)
	UserAgent    string    `json:"user_agent,omitempty"` // client UA (device tracking)
	AMR          []string  `json:"amr,omitempty"`        // auth methods ("pwd","totp","fido2")
	CreatedAt    time.Time `json:"created_at"`
	LastActiveAt time.Time `json:"last_active_at"`
	ExpiresAt    time.Time `json:"expires_at"` // absolute cap = CreatedAt + AbsoluteTTL

	refreshHash string // sha256 of the live refresh token; unexported
}

// IssueRequest describes a new login/session.
type IssueRequest struct {
	UserID    string
	AccountID int64  // 0 = not yet bound to a trading account
	Tier      string // rate-limit tier (§8.3)
	Device    string
	IP        string
	UserAgent string
	AMR       []string // auth methods already satisfied (e.g. ["pwd","totp"])
	Scopes    []string // access-token scope grant
}

// IssuedSession is the bundle returned to the client on login/refresh.
type IssuedSession struct {
	AccessToken  string    // 15min JWT
	RefreshToken string    // 7d opaque bearer
	ExpiresAt    time.Time // access-token expiry
	Session      Session
}

// RefreshConsumeResult is the outcome of the atomic refresh rotation.
type RefreshConsumeResult int

const (
	RefreshMissing RefreshConsumeResult = iota // unknown/expired token
	RefreshOK                                  // consumed; sid returned
	RefreshReused                              // previously-rotated token: reuse detected
)

// SessionStore is the narrow persistence seam over the coordination
// Redis. redisSessionStore (session_store.go) is production; unit tests
// substitute an in-memory fake.
type SessionStore interface {
	// WriteSession stores the hash and arms its TTL atomically.
	WriteSession(ctx context.Context, s Session, ttl time.Duration) error
	// ReadSession loads session:{sid}; (zero, false, nil) when absent.
	ReadSession(ctx context.Context, sid string) (Session, bool, error)
	// DeleteSession removes session:{sid}.
	DeleteSession(ctx context.Context, sid string) error
	// IndexAdd prunes members scored < cutoff, inserts member at score,
	// and if cardinality exceeds max evicts the oldest members,
	// returning their ids.
	IndexAdd(ctx context.Context, index, member string, score float64, max int64, cutoff float64) (evicted []string, err error)
	// IndexRemove deletes members from the index zset.
	IndexRemove(ctx context.Context, index string, members ...string) error
	// IndexMembers prunes members scored < cutoff and returns all ids.
	IndexMembers(ctx context.Context, index string, cutoff float64) ([]string, error)
	// ConsumeRefresh atomically: if refresh:{hash} holds a sid, delete it
	// and mark refresh_used:{hash}=sid (TTL usedTTL) → (sid, RefreshOK);
	// else if refresh_used:{hash} exists → (sid, RefreshReused);
	// else → ("", RefreshMissing).
	ConsumeRefresh(ctx context.Context, activeKey, usedKey string, usedTTL time.Duration) (string, RefreshConsumeResult, error)
	// SetRefresh stores refresh:{hash} → sid with TTL.
	SetRefresh(ctx context.Context, key, sid string, ttl time.Duration) error
	// DeleteRefresh removes refresh:{hash}.
	DeleteRefresh(ctx context.Context, key string) error
}

// SessionManager drives the session lifecycle against a SessionStore.
type SessionManager struct {
	store  SessionStore
	issuer *Issuer
	cfg    SessionConfig
	now    func() time.Time
}

// NewSessionManager wires a manager. issuer mints access tokens; it must
// not be nil (sessions without tokens are meaningless at the API edge).
func NewSessionManager(store SessionStore, issuer *Issuer, cfg SessionConfig) (*SessionManager, error) {
	if store == nil {
		return nil, newError(CodeAuthInternal, "session store is nil")
	}
	if issuer == nil {
		return nil, newError(CodeAuthInternal, "token issuer is nil")
	}
	cfg.withDefaults()
	return &SessionManager{store: store, issuer: issuer, cfg: cfg, now: time.Now}, nil
}

// refreshKey / refreshUsedKey derive the refresh-token index keys. Only
// the sha256 of the bearer is ever stored — a Redis read can never
// recover a usable token.
func refreshKey(token string) string {
	sum := sha256.Sum256([]byte(token))
	return "refresh:" + hex.EncodeToString(sum[:])
}
func refreshUsedKey(token string) string {
	sum := sha256.Sum256([]byte(token))
	return "refresh_used:" + hex.EncodeToString(sum[:])
}

// accountIndex / userIndex / ipIndex name the concurrency indexes.
func accountIndex(accountID int64) string {
	return "sess:acct:" + strconv.FormatInt(accountID, 10)
}
func userIndex(userID string) string { return "sess:user:" + userID }
func ipIndex(ip string) string       { return "sess:ip:" + ip }

// principalIndex picks the per-principal cap index: the bound account
// when present, else the user (login may precede account selection).
func principalIndex(s Session) string {
	if s.AccountID > 0 {
		return accountIndex(s.AccountID)
	}
	return userIndex(s.UserID)
}

// Issue creates a session: enforces the §8.8 concurrent caps with
// oldest-first eviction, stores the canonical hash, binds a refresh
// token, and mints the 15-minute access JWT.
func (m *SessionManager) Issue(ctx context.Context, req IssueRequest) (*IssuedSession, error) {
	if req.UserID == "" {
		return nil, newError(CodeAuthInternal, "session requires a user id")
	}
	now := m.now()
	sid, err := randomToken(24)
	if err != nil {
		return nil, wrapError(CodeAuthInternal, "session id generation", err)
	}
	refresh, err := randomToken(32)
	if err != nil {
		return nil, wrapError(CodeAuthInternal, "refresh token generation", err)
	}
	s := Session{
		ID:           sid,
		UserID:       req.UserID,
		AccountID:    req.AccountID,
		Tier:         req.Tier,
		Device:       req.Device,
		IP:           req.IP,
		UserAgent:    req.UserAgent,
		AMR:          req.AMR,
		CreatedAt:    now,
		LastActiveAt: now,
		ExpiresAt:    now.Add(m.cfg.AbsoluteTTL),
		refreshHash:  refreshKey(refresh),
	}
	if err := m.writeSession(ctx, s); err != nil {
		return nil, err
	}
	cutoff := float64(now.Add(-m.cfg.AbsoluteTTL).UnixNano())
	// Per-principal cap (5/account) then per-IP cap (20) — both evict
	// oldest-first. Eviction happens after the new session is written so
	// a victim is never dropped before its replacement exists.
	if evicted, err := m.store.IndexAdd(ctx, principalIndex(s), sid,
		float64(now.UnixNano()), int64(m.cfg.MaxPerAccount), cutoff); err != nil {
		return nil, wrapError(CodeAuthInternal, "session account index", err)
	} else if err := m.revokeMany(ctx, evicted); err != nil {
		return nil, err
	}
	if s.IP != "" {
		if evicted, err := m.store.IndexAdd(ctx, ipIndex(s.IP), sid,
			float64(now.UnixNano()), int64(m.cfg.MaxPerIP), cutoff); err != nil {
			return nil, wrapError(CodeAuthInternal, "session ip index", err)
		} else if err := m.revokeMany(ctx, evicted); err != nil {
			return nil, err
		}
	}
	if err := m.store.SetRefresh(ctx, refreshKey(refresh), sid, m.cfg.RefreshTTL); err != nil {
		return nil, wrapError(CodeAuthInternal, "refresh index", err)
	}
	access, exp, err := m.issueAccess(s, req.Scopes)
	if err != nil {
		return nil, err
	}
	return &IssuedSession{AccessToken: access, RefreshToken: refresh, ExpiresAt: exp, Session: s}, nil
}

// Validate enforces the session policy on each request: existence (not
// revoked/expired in Redis), absolute cap, and idle timeout. A passing
// call re-arms the storage TTL and stamps last_active — this is the
// idle-timeout sliding window.
func (m *SessionManager) Validate(ctx context.Context, sid string) (Session, error) {
	if sid == "" {
		return Session{}, newError(CodeUnauthorized, "no session bound to token")
	}
	s, ok, err := m.store.ReadSession(ctx, sid)
	if err != nil {
		return Session{}, wrapError(CodeAuthInternal, "session read", err)
	}
	if !ok {
		return Session{}, newError(CodeSessionRevoked, "session not found or revoked")
	}
	now := m.now()
	if !now.Before(s.ExpiresAt) {
		_ = m.revokeOne(ctx, s) // absolute 8h cap reached
		return Session{}, newError(CodeSessionExpired, "session absolute lifetime exceeded")
	}
	if now.Sub(s.LastActiveAt) > m.cfg.IdleTTL {
		_ = m.revokeOne(ctx, s) // idle 30min timeout
		return Session{}, newError(CodeSessionExpired, "session idle timeout")
	}
	s.LastActiveAt = now
	if err := m.writeSession(ctx, s); err != nil {
		return Session{}, err
	}
	return s, nil
}

// Refresh rotates the refresh token and mints a fresh access token.
// Rotation is atomic at the store layer (ConsumeRefresh); a token
// presented after rotation triggers reuse detection — the whole session
// is revoked (spec §8.8).
func (m *SessionManager) Refresh(ctx context.Context, refreshToken string, scopes []string) (*IssuedSession, error) {
	if refreshToken == "" {
		return nil, newError(CodeUnauthorized, "missing refresh token")
	}
	sid, state, err := m.store.ConsumeRefresh(ctx,
		refreshKey(refreshToken), refreshUsedKey(refreshToken), m.cfg.RefreshTTL)
	if err != nil {
		return nil, wrapError(CodeAuthInternal, "refresh consume", err)
	}
	switch state {
	case RefreshMissing:
		return nil, newError(CodeUnauthorized, "unknown or expired refresh token")
	case RefreshReused:
		// Reuse detected: the token was already rotated — treat as theft
		// and kill the session outright.
		if sid != "" {
			_ = m.Revoke(ctx, sid)
		}
		return nil, newError(CodeUnauthorized, "refresh token reuse detected; session revoked")
	}
	s, ok, err := m.store.ReadSession(ctx, sid)
	if err != nil {
		return nil, wrapError(CodeAuthInternal, "session read", err)
	}
	if !ok {
		// The refresh marker was live but the session hash is gone: the
		// session expired (Redis TTL) or was revoked out-of-band.
		return nil, newError(CodeSessionExpired, "session already expired or revoked")
	}
	now := m.now()
	if !now.Before(s.ExpiresAt) {
		_ = m.revokeOne(ctx, s)
		return nil, newError(CodeSessionExpired, "session absolute lifetime exceeded")
	}
	// Rotate: new refresh token supersedes; the consumed one is already
	// marked used by ConsumeRefresh.
	refresh, err := randomToken(32)
	if err != nil {
		return nil, wrapError(CodeAuthInternal, "refresh token generation", err)
	}
	s.refreshHash = refreshKey(refresh)
	s.LastActiveAt = now
	if err := m.writeSession(ctx, s); err != nil {
		return nil, err
	}
	if err := m.store.SetRefresh(ctx, refreshKey(refresh), sid, m.cfg.RefreshTTL); err != nil {
		return nil, wrapError(CodeAuthInternal, "refresh index", err)
	}
	access, exp, err := m.issueAccess(s, scopes)
	if err != nil {
		return nil, err
	}
	return &IssuedSession{AccessToken: access, RefreshToken: refresh, ExpiresAt: exp, Session: s}, nil
}

// Revoke deletes one session: hash, both concurrency indexes, and the
// live refresh marker (DELETE /api/v1/account/sessions/{id} backing op).
func (m *SessionManager) Revoke(ctx context.Context, sid string) error {
	s, ok, err := m.store.ReadSession(ctx, sid)
	if err != nil {
		return wrapError(CodeAuthInternal, "session read", err)
	}
	if !ok {
		return nil // already gone — revoke is idempotent
	}
	return m.revokeOne(ctx, s)
}

// RevokeAll revokes every session on the principal index (logout-all,
// FROZEN legal-hold session kill per spec §5.16 note).
func (m *SessionManager) RevokeAll(ctx context.Context, accountID int64, userID string) error {
	index := accountIndex(accountID)
	if accountID <= 0 {
		index = userIndex(userID)
	}
	cutoff := float64(m.now().Add(-m.cfg.AbsoluteTTL).UnixNano())
	sids, err := m.store.IndexMembers(ctx, index, cutoff)
	if err != nil {
		return wrapError(CodeAuthInternal, "session index read", err)
	}
	return m.revokeMany(ctx, sids)
}

// List returns the live sessions on the principal index for
// GET /api/v1/account/sessions — newest first; expired entries are
// dropped lazily on read.
func (m *SessionManager) List(ctx context.Context, accountID int64, userID string) ([]Session, error) {
	index := accountIndex(accountID)
	if accountID <= 0 {
		index = userIndex(userID)
	}
	cutoff := float64(m.now().Add(-m.cfg.AbsoluteTTL).UnixNano())
	sids, err := m.store.IndexMembers(ctx, index, cutoff)
	if err != nil {
		return nil, wrapError(CodeAuthInternal, "session index read", err)
	}
	out := make([]Session, 0, len(sids))
	now := m.now()
	for _, sid := range sids {
		s, ok, err := m.store.ReadSession(ctx, sid)
		if err != nil {
			return nil, wrapError(CodeAuthInternal, "session read", err)
		}
		if !ok || !now.Before(s.ExpiresAt) {
			continue
		}
		out = append(out, s)
	}
	// Newest first for the sessions screen (spec §21.x ordering).
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}

// writeSession persists the hash with TTL = min(StorageTTL, absolute
// remaining). The TTL is the storage bound; policy timeouts are enforced
// in Validate.
func (m *SessionManager) writeSession(ctx context.Context, s Session) error {
	ttl := m.cfg.StorageTTL
	if rem := s.ExpiresAt.Sub(m.now()); rem < ttl {
		ttl = rem
	}
	if ttl <= 0 {
		ttl = time.Second // never write a non-expiring session record
	}
	if err := m.store.WriteSession(ctx, s, ttl); err != nil {
		return wrapError(CodeAuthInternal, "session write", err)
	}
	return nil
}

// revokeOne removes one session from every index plus its refresh marker.
func (m *SessionManager) revokeOne(ctx context.Context, s Session) error {
	if err := m.store.DeleteSession(ctx, s.ID); err != nil {
		return wrapError(CodeAuthInternal, "session delete", err)
	}
	indexes := []string{principalIndex(s)}
	if s.IP != "" {
		indexes = append(indexes, ipIndex(s.IP))
	}
	for _, idx := range indexes {
		if err := m.store.IndexRemove(ctx, idx, s.ID); err != nil {
			return wrapError(CodeAuthInternal, "session index remove", err)
		}
	}
	if s.refreshHash != "" {
		// refreshHash already carries the "refresh:" prefix form
		if err := m.store.DeleteRefresh(ctx, s.refreshHash); err != nil {
			return wrapError(CodeAuthInternal, "refresh delete", err)
		}
	}
	return nil
}

func (m *SessionManager) revokeMany(ctx context.Context, sids []string) error {
	for _, sid := range sids {
		s, ok, err := m.store.ReadSession(ctx, sid)
		if err != nil {
			return wrapError(CodeAuthInternal, "session read", err)
		}
		if !ok {
			continue
		}
		if err := m.revokeOne(ctx, s); err != nil {
			return err
		}
	}
	return nil
}

// issueAccess mints the 15-minute JWT bound to the session id.
func (m *SessionManager) issueAccess(s Session, scopes []string) (string, time.Time, error) {
	tok, claims, err := m.issuer.Issue(s.UserID, IssueOptions{
		AccountID: s.AccountID,
		SessionID: s.ID,
		Scopes:    scopes,
		AMR:       s.AMR,
	})
	if err != nil {
		return "", time.Time{}, err
	}
	return tok, claims.ExpiresAt, nil
}

// hashFields is the canonical session:{sid} hash shape (spec §4.1 field
// names first; extension fields after). refresh_hash stores the sha256
// hex of the live refresh token so revocation can drop the index entry.
func (s Session) hashFields() (map[string]string, error) {
	amr, err := json.Marshal(s.AMR)
	if err != nil {
		return nil, err
	}
	f := map[string]string{
		"user_id":        s.UserID,
		"account_id":     strconv.FormatInt(s.AccountID, 10),
		"tier":           s.Tier,
		"device":         s.Device,
		"ip":             s.IP,
		"user_agent":     s.UserAgent,
		"amr":            string(amr),
		"created_at":     s.CreatedAt.UTC().Format(time.RFC3339Nano),
		"last_active_at": s.LastActiveAt.UTC().Format(time.RFC3339Nano),
		"expires_at":     s.ExpiresAt.UTC().Format(time.RFC3339Nano),
		"refresh_hash":   s.refreshHash,
	}
	return f, nil
}

// sessionFromHash parses a session:{sid} hash body back into a Session.
// Corrupt field values fail closed as a read error, never a zero session
// that could be mistaken for valid.
func sessionFromHash(sid string, f map[string]string) (Session, error) {
	s := Session{ID: sid, UserID: f["user_id"], Tier: f["tier"],
		Device: f["device"], IP: f["ip"], UserAgent: f["user_agent"],
		refreshHash: f["refresh_hash"]}
	var err error
	if s.AccountID, err = strconv.ParseInt(f["account_id"], 10, 64); err != nil && f["account_id"] != "" {
		return Session{}, err
	}
	if len(f["amr"]) > 0 {
		if err := json.Unmarshal([]byte(f["amr"]), &s.AMR); err != nil {
			return Session{}, err
		}
	}
	parse := func(v string) (time.Time, error) {
		if v == "" {
			return time.Time{}, nil
		}
		return time.Parse(time.RFC3339Nano, v)
	}
	if s.CreatedAt, err = parse(f["created_at"]); err != nil {
		return Session{}, err
	}
	if s.LastActiveAt, err = parse(f["last_active_at"]); err != nil {
		return Session{}, err
	}
	if s.ExpiresAt, err = parse(f["expires_at"]); err != nil {
		return Session{}, err
	}
	return s, nil
}
