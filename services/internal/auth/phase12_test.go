// Unit tests for Phase-12 Tasks 12.3.7/12.3.8/12.3.9/12.3.12 — the
// challenge store contract, session elevation (ElevateAMR /
// ReissueAccess / RevokeAllExcept / Lookup), clone-detection response,
// anti-phishing bounds and login-history cursors. Storage-backed paths
// are covered by the gated PG/Redis tests.
package auth

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// fakes
// ---------------------------------------------------------------------------

type fakeChallengeStore struct {
	mu   sync.Mutex
	data map[string][]byte
}

func newFakeChallengeStore() *fakeChallengeStore {
	return &fakeChallengeStore{data: map[string][]byte{}}
}

func (f *fakeChallengeStore) PutChallenge(_ context.Context, id string, payload []byte, _ time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.data[id] = payload
	return nil
}

func (f *fakeChallengeStore) ConsumeChallenge(_ context.Context, id string) ([]byte, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.data[id]
	if ok {
		delete(f.data, id) // single-use
	}
	return p, ok, nil
}

type fakeWebAuthnStore struct {
	mu       sync.Mutex
	emails   map[int64]string
	creds    map[string]WebAuthnCredential // key = string(credential_id)
	nextID   int64
	revoked  []int64
	counters map[int64]uint32
}

func newFakeWebAuthnStore() *fakeWebAuthnStore {
	return &fakeWebAuthnStore{
		emails:   map[int64]string{7: "u7@example.com", 9: "u9@example.com"},
		creds:    map[string]WebAuthnCredential{},
		counters: map[int64]uint32{},
		nextID:   1,
	}
}

func (f *fakeWebAuthnStore) CreateCredential(_ context.Context, c WebAuthnCredential) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c.ID = f.nextID
	f.nextID++
	f.creds[string(c.CredentialID)] = c
	return c.ID, nil
}

func (f *fakeWebAuthnStore) ActiveCredentialsForUser(_ context.Context, userID int64) ([]WebAuthnCredential, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []WebAuthnCredential
	for _, c := range f.creds {
		if c.UserID == userID && c.RevokedAt == nil {
			out = append(out, c)
		}
	}
	return out, nil
}

func (f *fakeWebAuthnStore) UpdateSignCount(_ context.Context, id int64, sc uint32) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.counters[id] = sc
	return nil
}

func (f *fakeWebAuthnStore) RevokeCredential(_ context.Context, id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.revoked = append(f.revoked, id)
	for k, c := range f.creds {
		if c.ID == id {
			now := time.Now()
			c.RevokedAt = &now
			f.creds[k] = c
		}
	}
	return nil
}

func (f *fakeWebAuthnStore) UserEmail(_ context.Context, userID int64) (string, error) {
	if e, ok := f.emails[userID]; ok {
		return e, nil
	}
	return "", newError(CodeUnauthorized, "user not found")
}

type fakeFreezer struct {
	mu     sync.Mutex
	frozen []int64
	reason string
}

func (f *fakeFreezer) FreezeUserAccounts(_ context.Context, userID int64, reason string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.frozen = append(f.frozen, userID)
	f.reason = reason
	return nil
}

func testWebAuthnService(t *testing.T, store *fakeWebAuthnStore, chal *fakeChallengeStore) *WebAuthnService {
	t.Helper()
	svc, err := NewWebAuthnService(WebAuthnConfig{
		RPID:          "exc.test",
		RPDisplayName: "Exchange Test",
		RPOrigins:     []string{"https://exc.test"},
	}, store, chal)
	if err != nil {
		t.Fatalf("webauthn service: %v", err)
	}
	return svc
}

// ---------------------------------------------------------------------------
// challenge lifecycle — fail-closed, single-use, user-bound
// ---------------------------------------------------------------------------

func TestWebAuthnRegistrationChallengeSingleUse(t *testing.T) {
	store := newFakeWebAuthnStore()
	chal := newFakeChallengeStore()
	svc := testWebAuthnService(t, store, chal)
	ctx := context.Background()

	challengeID, opts, err := svc.BeginRegistration(ctx, 7)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if challengeID == "" || opts == nil {
		t.Fatal("begin must return challenge id + publicKey options")
	}
	if opts.Response.Challenge.String() == "" {
		t.Fatal("options must carry the WebAuthn challenge")
	}

	// Foreign-user finish: the challenge is consumed AND rejected — the
	// legitimate owner cannot replay it afterwards either.
	_, err = svc.FinishRegistration(ctx, 9, challengeID, "", []byte("{}"))
	requireCode(t, err, CodeWebAuthnFailed)
	_, err = svc.FinishRegistration(ctx, 7, challengeID, "", []byte("{}"))
	requireCode(t, err, CodeWebAuthnFailed) // consumed — gone
}

func TestWebAuthnUnknownChallengeFailsClosed(t *testing.T) {
	svc := testWebAuthnService(t, newFakeWebAuthnStore(), newFakeChallengeStore())
	_, err := svc.FinishRegistration(context.Background(), 7, "nope", "", []byte("{}"))
	requireCode(t, err, CodeWebAuthnFailed)
	_, err = svc.FinishAssertion(context.Background(), 7, "nope", []byte("{}"))
	requireCode(t, err, CodeWebAuthnFailed)
}

// TestWebAuthnChallengeUserBinding proves a challenge minted for user A
// can never complete a ceremony for user B (record.UserID equality).
func TestWebAuthnChallengeUserBinding(t *testing.T) {
	chal := newFakeChallengeStore()
	store := newFakeWebAuthnStore()
	store.creds["c7"] = WebAuthnCredential{
		ID: 1, UserID: 7, CredentialID: []byte("c7"), PublicKey: []byte("pk"),
	}
	svc := testWebAuthnService(t, store, chal)
	ctx := context.Background()
	cid, _, err := svc.BeginAssertion(ctx, 7)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	_, err = svc.FinishAssertion(ctx, 9, cid, []byte("{}"))
	requireCode(t, err, CodeWebAuthnFailed)
}

// ---------------------------------------------------------------------------
// clone detection — Task 12.3.12 part 2 seam
// ---------------------------------------------------------------------------

func TestWebAuthnCloneResponseRevokesAndFreezes(t *testing.T) {
	store := newFakeWebAuthnStore()
	svc := testWebAuthnService(t, store, newFakeChallengeStore())
	freezer := &fakeFreezer{}
	svc.WithFreezer(freezer)

	row := WebAuthnCredential{ID: 41, UserID: 7, CredentialID: []byte("cred")}
	store.creds["cred"] = row

	svc.cloneResponse(context.Background(), 7, row)

	if len(store.revoked) != 1 || store.revoked[0] != 41 {
		t.Fatalf("credential row must be revoked on clone: %v", store.revoked)
	}
	if len(freezer.frozen) != 1 || freezer.frozen[0] != 7 {
		t.Fatalf("account freeze must fire on clone: %v", freezer.frozen)
	}
	if !strings.Contains(freezer.reason, "clone") {
		t.Fatalf("freeze reason must name the trigger: %q", freezer.reason)
	}
}

// ---------------------------------------------------------------------------
// session elevation — §12.6 invariant
// ---------------------------------------------------------------------------

func TestSessionElevateAMRAndReissue(t *testing.T) {
	m, _, _ := testManager(t, SessionConfig{})
	ctx := context.Background()
	b, err := m.Issue(ctx, IssueRequest{
		UserID: "u-7", AccountID: 77, AMR: []string{"pwd"}, Scopes: []string{"read"},
	})
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	s, err := m.ElevateAMR(ctx, b.Session.ID, "fido2")
	if err != nil {
		t.Fatalf("elevate: %v", err)
	}
	if !s.TwoFactorVerified {
		t.Fatal("two_factor_verified must be set by fido2 elevation")
	}
	found := false
	for _, a := range s.AMR {
		if a == "fido2" {
			found = true
		}
	}
	if !found {
		t.Fatalf("amr must carry fido2: %v", s.AMR)
	}
	// Idempotent: a second elevation must not duplicate the AMR entry.
	s2, err := m.ElevateAMR(ctx, b.Session.ID, "fido2")
	if err != nil {
		t.Fatalf("re-elevate: %v", err)
	}
	n := 0
	for _, a := range s2.AMR {
		if a == "fido2" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("amr fido2 must appear once: %v", s2.AMR)
	}
	// The re-issued access token carries the elevated AMR — downstream
	// RequireTwoFactor passes without waiting for a refresh.
	tok, _, err := m.ReissueAccess(ctx, b.Session.ID, []string{"read"})
	if err != nil {
		t.Fatalf("reissue: %v", err)
	}
	claims, err := m.issuer.Parse(tok)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !claims.TwoFactorVerified() {
		t.Fatalf("reissued token must satisfy TwoFactorVerified: %+v", claims.AMR)
	}
	// A revoked session cannot be elevated (fail-closed).
	if err := m.Revoke(ctx, b.Session.ID); err != nil {
		t.Fatal(err)
	}
	_, err = m.ElevateAMR(ctx, b.Session.ID, "fido2")
	if err == nil {
		t.Fatal("elevating a revoked session must fail")
	}
}

func TestSessionRevokeAllExceptKeepsCurrent(t *testing.T) {
	m, store, _ := testManager(t, SessionConfig{})
	ctx := context.Background()
	var keep string
	var sids []string
	for i := 0; i < 3; i++ {
		b, err := m.Issue(ctx, IssueRequest{UserID: "u-9", AccountID: 91, AMR: []string{"pwd"}})
		if err != nil {
			t.Fatalf("issue %d: %v", i, err)
		}
		sids = append(sids, b.Session.ID)
	}
	keep = sids[1]
	n, err := m.RevokeAllExcept(ctx, 91, "u-9", keep)
	if err != nil {
		t.Fatalf("revoke-all-except: %v", err)
	}
	if n != 2 {
		t.Fatalf("expected 2 revocations, got %d", n)
	}
	if _, ok, _ := store.ReadSession(ctx, keep); !ok {
		t.Fatal("current session must survive revoke-all")
	}
	for _, sid := range []string{sids[0], sids[2]} {
		if _, ok, _ := store.ReadSession(ctx, sid); ok {
			t.Fatalf("session %s must be revoked", sid)
		}
	}
}

func TestSessionLookupIsReadOnly(t *testing.T) {
	m, _, _ := testManager(t, SessionConfig{})
	ctx := context.Background()
	b, err := m.Issue(ctx, IssueRequest{UserID: "u-3", AMR: []string{"pwd"}})
	if err != nil {
		t.Fatal(err)
	}
	s, ok, err := m.Lookup(ctx, b.Session.ID)
	if err != nil || !ok || s.ID != b.Session.ID {
		t.Fatalf("lookup: ok=%v err=%v", ok, err)
	}
	if _, ok, _ := m.Lookup(ctx, "no-such"); ok {
		t.Fatal("missing sid must report ok=false")
	}
}

// ---------------------------------------------------------------------------
// anti-phishing bounds (Task 12.3.8)
// ---------------------------------------------------------------------------

func TestValidateAntiPhishingCode(t *testing.T) {
	requireCode(t, ValidateAntiPhishingCode("abc"), CodeInvalidRequest)
	requireCode(t, ValidateAntiPhishingCode(strings.Repeat("x", 33)), CodeInvalidRequest)
	for _, okLen := range []string{"abcd", strings.Repeat("y", 32)} {
		if err := ValidateAntiPhishingCode(okLen); err != nil {
			t.Fatalf("len %d must pass: %v", len(okLen), err)
		}
	}
	// Multi-byte runes count once (char_length parity with the CHECK).
	if err := ValidateAntiPhishingCode("日本語ab"); err != nil { // 6 runes
		t.Fatalf("rune-counted bounds: %v", err)
	}
}

// ---------------------------------------------------------------------------
// login history cursor (Task 12.3.9)
// ---------------------------------------------------------------------------

func TestLoginCursorRoundTrip(t *testing.T) {
	ts := time.Unix(1700000000, 123456789).UTC()
	cur := encodeLoginCursor(ts, 42)
	gotTS, gotID, err := decodeLoginCursor(cur)
	if err != nil || gotID != 42 || !gotTS.Equal(ts) {
		t.Fatalf("cursor round trip: %v %v %v", gotTS, gotID, err)
	}
	if _, _, err := decodeLoginCursor("!!!not-base64!!!"); err == nil {
		t.Fatal("garbage cursor must reject")
	}
	if _, _, err := decodeLoginCursor(b64urlEncode([]byte("not-a-cursor"))); err == nil {
		t.Fatal("malformed cursor must reject")
	}
}

// ---------------------------------------------------------------------------
// webauthn user adapter
// ---------------------------------------------------------------------------

func TestUserHandleRoundTrip(t *testing.T) {
	id, err := userHandleToID(userHandle(7))
	if err != nil || id != 7 {
		t.Fatalf("handle round trip: %v %v", id, err)
	}
	if _, err := userHandleToID([]byte("short")); err == nil {
		t.Fatal("malformed handle must reject")
	}
}

func TestWebAuthnUserAdapter(t *testing.T) {
	store := newFakeWebAuthnStore()
	store.creds["c1"] = WebAuthnCredential{
		ID: 1, UserID: 7, CredentialID: []byte("c1"),
		PublicKey: []byte("pk"), Transports: []string{"usb"},
	}
	svc := testWebAuthnService(t, store, newFakeChallengeStore())
	u, err := svc.loadUser(context.Background(), 7)
	if err != nil {
		t.Fatal(err)
	}
	if u.WebAuthnName() != "u7@example.com" {
		t.Fatalf("rp user name: %q", u.WebAuthnName())
	}
	if len(u.WebAuthnCredentials()) != 1 {
		t.Fatal("active credentials must surface in the adapter")
	}
}
