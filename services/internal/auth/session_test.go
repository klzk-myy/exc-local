// Unit tests for the session lifecycle (Tasks 5.3.1, 5.3.10) driven
// against an in-memory SessionStore — Redis semantics are covered
// separately in session_store_test.go against a live instance.
package auth

import (
	"context"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// In-memory SessionStore fake — same contract as redisSessionStore:
// hash storage with TTL (checked lazily via a shared clock), ZSET indexes
// with score-ordered eviction, atomic refresh consume.
// ---------------------------------------------------------------------------

type fakeZMember struct {
	member string
	score  float64
}

type fakeSessionStore struct {
	mu       sync.Mutex
	sessions map[string]Session
	zsets    map[string][]fakeZMember // index → sorted by score asc
	refresh  map[string]string        // refresh:{hash} → sid
	used     map[string]string        // refresh_used:{hash} → sid
	now      func() time.Time
	writeTTL map[string]time.Duration // last WriteSession ttl per sid
}

func newFakeSessionStore(now func() time.Time) *fakeSessionStore {
	return &fakeSessionStore{
		sessions: map[string]Session{},
		zsets:    map[string][]fakeZMember{},
		refresh:  map[string]string{},
		used:     map[string]string{},
		writeTTL: map[string]time.Duration{},
		now:      now,
	}
}

func (f *fakeSessionStore) WriteSession(_ context.Context, s Session, ttl time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sessions[s.ID] = s
	f.writeTTL[s.ID] = ttl
	return nil
}

func (f *fakeSessionStore) ReadSession(_ context.Context, sid string) (Session, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.sessions[sid]
	return s, ok, nil
}

func (f *fakeSessionStore) DeleteSession(_ context.Context, sid string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.sessions, sid)
	return nil
}

func (f *fakeSessionStore) IndexAdd(_ context.Context, index, member string, score float64, max int64, cutoff float64) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	zs := f.zsets[index]
	kept := zs[:0]
	for _, m := range zs {
		if m.score >= cutoff && m.member != member {
			kept = append(kept, m)
		}
	}
	kept = append(kept, fakeZMember{member, score})
	sort.Slice(kept, func(i, j int) bool { return kept[i].score < kept[j].score })
	var evicted []string
	for int64(len(kept)) > max {
		evicted = append(evicted, kept[0].member)
		kept = kept[1:]
	}
	f.zsets[index] = kept
	return evicted, nil
}

func (f *fakeSessionStore) IndexRemove(_ context.Context, index string, members ...string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	del := map[string]bool{}
	for _, m := range members {
		del[m] = true
	}
	zs := f.zsets[index]
	kept := zs[:0]
	for _, m := range zs {
		if !del[m.member] {
			kept = append(kept, m)
		}
	}
	f.zsets[index] = kept
	return nil
}

func (f *fakeSessionStore) IndexMembers(_ context.Context, index string, cutoff float64) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, m := range f.zsets[index] {
		if m.score >= cutoff {
			out = append(out, m.member)
		}
	}
	return out, nil
}

func (f *fakeSessionStore) ConsumeRefresh(_ context.Context, activeKey, usedKey string, _ time.Duration) (string, RefreshConsumeResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if sid, ok := f.refresh[activeKey]; ok {
		delete(f.refresh, activeKey)
		f.used[usedKey] = sid
		return sid, RefreshOK, nil
	}
	if sid, ok := f.used[usedKey]; ok {
		return sid, RefreshReused, nil
	}
	return "", RefreshMissing, nil
}

func (f *fakeSessionStore) SetRefresh(_ context.Context, key, sid string, _ time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.refresh[key] = sid
	return nil
}

func (f *fakeSessionStore) DeleteRefresh(_ context.Context, key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.refresh, key)
	return nil
}

// ---------------------------------------------------------------------------

func testManager(t *testing.T, cfg SessionConfig) (*SessionManager, *fakeSessionStore, *time.Time) {
	t.Helper()
	clk := time.Now()
	store := newFakeSessionStore(func() time.Time { return clk })
	issuer := NewIssuer("exc-test", "exc-api", 15*time.Minute)
	if err := issuer.AddHMACKey("k1", []byte(strings.Repeat("a", 32)), true); err != nil {
		t.Fatal(err)
	}
	m, err := NewSessionManager(store, issuer, cfg)
	if err != nil {
		t.Fatalf("manager: %v", err)
	}
	m.now = func() time.Time { return clk }
	return m, store, &clk
}

func TestSessionIssueValidateRevoke(t *testing.T) {
	m, _, _ := testManager(t, SessionConfig{})
	ctx := context.Background()
	b, err := m.Issue(ctx, IssueRequest{
		UserID: "u-1", AccountID: 1001, Tier: "BASIC",
		Device: "chrome-osx", IP: "192.0.2.1", AMR: []string{"pwd"},
	})
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	if b.AccessToken == "" || b.RefreshToken == "" {
		t.Fatal("bundle must carry access + refresh tokens")
	}
	// Access token carries the session binding.
	claims, err := m.issuer.Parse(b.AccessToken)
	if err != nil || claims.SessionID != b.Session.ID {
		t.Fatalf("access token must bind sid: %v %v", err, claims)
	}
	s, err := m.Validate(ctx, b.Session.ID)
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if s.UserID != "u-1" || s.Device != "chrome-osx" {
		t.Fatalf("device tracking lost: %+v", s)
	}
	if err := m.Revoke(ctx, b.Session.ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	requireCode(t, func() error { _, e := m.Validate(ctx, b.Session.ID); return e }(), CodeSessionRevoked)
	// Revocation also kills the refresh token.
	_, err = m.Refresh(ctx, b.RefreshToken, nil)
	requireCode(t, err, CodeUnauthorized)
}

func TestSessionAccountCapEvictsOldest(t *testing.T) {
	cfg := SessionConfig{MaxPerAccount: 3}
	m, _, clk := testManager(t, cfg)
	ctx := context.Background()
	var issued []*IssuedSession
	for i := 0; i < 4; i++ {
		b, err := m.Issue(ctx, IssueRequest{UserID: "u-1", AccountID: 1001, IP: "10.0.0." + string(rune('1'+i))})
		if err != nil {
			t.Fatalf("issue %d: %v", i, err)
		}
		issued = append(issued, b)
		*clk = clk.Add(time.Second) // distinct scores
	}
	// Oldest session evicted (5.3.10 + §8.8 oldest-first).
	_, err := m.Validate(ctx, issued[0].Session.ID)
	requireCode(t, err, CodeSessionRevoked)
	for _, b := range issued[1:] {
		if _, err := m.Validate(ctx, b.Session.ID); err != nil {
			t.Fatalf("surviving session must validate: %v", err)
		}
	}
	list, err := m.List(ctx, 1001, "")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 3 {
		t.Fatalf("session list must show 3 live sessions, got %d", len(list))
	}
}

func TestSessionIPCapEvictsOldest(t *testing.T) {
	cfg := SessionConfig{MaxPerIP: 2}
	m, _, clk := testManager(t, cfg)
	ctx := context.Background()
	ids := []string{}
	for i := 0; i < 3; i++ {
		b, err := m.Issue(ctx, IssueRequest{UserID: "u-" + string(rune('a'+i)), AccountID: int64(1000 + i), IP: "203.0.113.5"})
		if err != nil {
			t.Fatalf("issue %d: %v", i, err)
		}
		ids = append(ids, b.Session.ID)
		*clk = clk.Add(time.Second)
	}
	if _, err := m.Validate(ctx, ids[0]); !codeIs(err, CodeSessionRevoked) {
		t.Fatalf("oldest same-IP session must be evicted, got %v", err)
	}
}

func TestSessionIdleAndAbsoluteTimeouts(t *testing.T) {
	m, _, clk := testManager(t, SessionConfig{})
	ctx := context.Background()
	b, err := m.Issue(ctx, IssueRequest{UserID: "u-1", AccountID: 1})
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	// Idle >30min → SESSION_EXPIRED (§8.8).
	*clk = clk.Add(31 * time.Minute)
	_, err = m.Validate(ctx, b.Session.ID)
	requireCode(t, err, CodeSessionExpired)

	// Fresh session, advance past 8h absolute → SESSION_EXPIRED.
	b2, err := m.Issue(ctx, IssueRequest{UserID: "u-1", AccountID: 1})
	if err != nil {
		t.Fatalf("issue2: %v", err)
	}
	*clk = clk.Add(9 * time.Hour)
	_, err = m.Validate(ctx, b2.Session.ID)
	requireCode(t, err, CodeSessionExpired)
	// Refresh after the session was torn down is rejected too —
	// the marker is gone, so this presents as UNAUTHORIZED.
	_, err = m.Refresh(ctx, b2.RefreshToken, nil)
	requireCode(t, err, CodeUnauthorized)
	// A refresh consumed against a vanished-but-not-expired-cleaned
	// session reports SESSION_EXPIRED (Redis TTL eviction path).
	b3, err := m.Issue(ctx, IssueRequest{UserID: "u-1", AccountID: 1})
	if err != nil {
		t.Fatal(err)
	}
	_ = m.store.DeleteSession(ctx, b3.Session.ID) // simulate Redis TTL drop
	_, err = m.Refresh(ctx, b3.RefreshToken, nil)
	requireCode(t, err, CodeSessionExpired)
}

func TestSessionRefreshRotationAndReuseDetection(t *testing.T) {
	m, _, _ := testManager(t, SessionConfig{})
	ctx := context.Background()
	b, err := m.Issue(ctx, IssueRequest{UserID: "u-1", AccountID: 5})
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	// Refresh produces a NEW access token and rotates the refresh token.
	b2, err := m.Refresh(ctx, b.RefreshToken, []string{"read"})
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if b2.AccessToken == b.AccessToken || b2.RefreshToken == b.RefreshToken {
		t.Fatal("refresh must rotate both tokens")
	}
	// Reusing the consumed refresh token revokes the whole session (§8.8).
	_, err = m.Refresh(ctx, b.RefreshToken, nil)
	requireCode(t, err, CodeUnauthorized)
	_, err = m.Validate(ctx, b.Session.ID)
	requireCode(t, err, CodeSessionRevoked)
	// And the rotated token is dead too.
	_, err = m.Refresh(ctx, b2.RefreshToken, nil)
	requireCode(t, err, CodeUnauthorized)
}

func TestSessionRevokeAllAndList(t *testing.T) {
	m, _, _ := testManager(t, SessionConfig{})
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		if _, err := m.Issue(ctx, IssueRequest{UserID: "u-9", AccountID: 77}); err != nil {
			t.Fatal(err)
		}
	}
	list, err := m.List(ctx, 77, "")
	if err != nil || len(list) != 2 {
		t.Fatalf("list: %v %d", err, len(list))
	}
	if err := m.RevokeAll(ctx, 77, ""); err != nil {
		t.Fatalf("revokeAll: %v", err)
	}
	list, err = m.List(ctx, 77, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 0 {
		t.Fatalf("all sessions revoked, list must be empty, got %d", len(list))
	}
}

func TestSessionNilDepsFailClosed(t *testing.T) {
	if _, err := NewSessionManager(nil, testIssuer(t), SessionConfig{}); err == nil {
		t.Fatal("nil store must reject")
	}
	if _, err := NewSessionManager(newFakeSessionStore(time.Now), nil, SessionConfig{}); err == nil {
		t.Fatal("nil issuer must reject")
	}
}
