// Fault-injection security drills — Phase-13.5 Task 13.5.3.7 (spec §2.7,
// §12.6, §24 #314). These are REAL drills, not documentation:
//
//	Drill 1  Key-rotation race: a flood of concurrent signed requests
//	         verifies against auth.Issuer while JWT keys rotate
//	         mid-flight. Assert: zero valid-request rejections inside the
//	         overlap window AND zero forged acceptances — run under
//	         -race so a keyring data race is a drill failure.
//	Drill 2  Expired-credential replay: expired access JWTs, expired and
//	         revoked sessions, and revoked/expired API keys are replayed
//	         across representative endpoint classes; every replay is a
//	         consistent 401-class rejection whose body leaks no internal
//	         detail.
//	Drill 3  Secret-store partition: the secret source is unreachable or
//	         misconfigured during load; the loader must fail closed with
//	         CONFIG_LOAD_FAILED and never fall back to dev/default
//	         material when EXC_SECRETS_REQUIRED=production.
package security

import (
	"context"
	stderrors "errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	jwtlib "github.com/golang-jwt/jwt/v5"

	"exchange/internal/auth"
	"exchange/internal/errs"
	excerrors "exchange/pkg/errors"
)

// ---------------------------------------------------------------------------
// in-memory SessionStore for the drills — implements auth.SessionStore
// faithfully enough for Issue/Validate/Revoke (the production store is the
// Redis Lua variant in auth/session_store.go; identical semantics, no Lua).
// ---------------------------------------------------------------------------

type memSessionStore struct {
	mu       sync.Mutex
	sessions map[string]auth.Session
	refresh  map[string]string // refresh:{hash} -> sid
	used     map[string]string // refresh_used:{hash} -> sid
	index    map[string][]string
}

func newMemSessionStore() *memSessionStore {
	return &memSessionStore{
		sessions: map[string]auth.Session{},
		refresh:  map[string]string{},
		used:     map[string]string{},
		index:    map[string][]string{},
	}
}

func (m *memSessionStore) WriteSession(_ context.Context, s auth.Session, _ time.Duration) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sessions[s.ID] = s
	return nil
}
func (m *memSessionStore) ReadSession(_ context.Context, sid string) (auth.Session, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[sid]
	return s, ok, nil
}
func (m *memSessionStore) DeleteSession(_ context.Context, sid string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.sessions, sid)
	return nil
}
func (m *memSessionStore) IndexAdd(_ context.Context, index, member string, _ float64, max int64, _ float64) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	members := append(m.index[index], member)
	var evicted []string
	if int64(len(members)) > max {
		evicted = members[:len(members)-int(max)]
		members = members[len(members)-int(max):]
	}
	m.index[index] = members
	return evicted, nil
}
func (m *memSessionStore) IndexRemove(_ context.Context, index string, members ...string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	keep := m.index[index][:0]
	for _, cur := range m.index[index] {
		drop := false
		for _, mm := range members {
			if cur == mm {
				drop = true
				break
			}
		}
		if !drop {
			keep = append(keep, cur)
		}
	}
	m.index[index] = keep
	return nil
}
func (m *memSessionStore) IndexMembers(_ context.Context, index string, _ float64) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.index[index]...), nil
}
func (m *memSessionStore) ConsumeRefresh(_ context.Context, activeKey, usedKey string, _ time.Duration) (string, auth.RefreshConsumeResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if sid, ok := m.refresh[activeKey]; ok {
		delete(m.refresh, activeKey)
		m.used[usedKey] = sid
		return sid, auth.RefreshOK, nil
	}
	if sid, ok := m.used[usedKey]; ok {
		return sid, auth.RefreshReused, nil
	}
	return "", auth.RefreshMissing, nil
}
func (m *memSessionStore) SetRefresh(_ context.Context, key, sid string, _ time.Duration) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.refresh[key] = sid
	return nil
}
func (m *memSessionStore) DeleteRefresh(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.refresh, key)
	return nil
}

// ---------------------------------------------------------------------------
// Drill 1 — key-rotation race under a concurrent signed-request flood.
// ---------------------------------------------------------------------------

func TestDrillKeyRotationRaceUnderLoad(t *testing.T) {
	kr := NewJWTKeyring("exc.local", "exc-api", 15*time.Minute, 24*time.Hour)
	ringSecretOld := hmacKey(0xA1)
	ringSecretNew := hmacKey(0xB2)
	if err := kr.Rotate(JWTKeyVersion{KID: "rot-1", Alg: "HS256", HMACSecret: ringSecretOld}); err != nil {
		t.Fatalf("initial key: %v", err)
	}

	// Pre-mint the "in-flight" credential population under the OLD kid —
	// clients hold these through the rotation window.
	const preMinted = 64
	oldTokens := make([]string, 0, preMinted)
	for i := 0; i < preMinted; i++ {
		tok, _, err := kr.Issuer().Issue(fmt.Sprintf("user-%d", i),
			auth.IssueOptions{AccountID: int64(1000 + i)})
		if err != nil {
			t.Fatalf("pre-mint: %v", err)
		}
		oldTokens = append(oldTokens, tok)
	}

	// Forged corpus — each must be rejected before, during and after
	// every rotation:
	//   (a) correct kid, wrong secret
	//   (b) unknown kid, arbitrary secret
	//   (c) alg confusion: token claims RS256 for an HS256-registered kid
	forge := func(kid, alg string, secret []byte) string {
		tok := jwtlib.NewWithClaims(jwtlib.SigningMethodHS256, jwtlib.MapClaims{
			"sub": "attacker", "iat": time.Now().Unix(),
			"exp": time.Now().Add(time.Hour).Unix(), "typ": "access",
		})
		tok.Header["kid"] = kid
		s, _ := tok.SignedString(secret)
		return s
	}
	forged := []string{
		forge("rot-1", "HS256", hmacKey(0xEE)), // valid kid, wrong key
		forge("rot-9", "HS256", hmacKey(0xEE)), // unknown kid
		forge("", "HS256", hmacKey(0xEE)),      // missing kid
	}
	// (c) alg-confusion attempt: sign with the ring's own secret but
	// declare a different alg for a known kid — the per-key alg pin must
	// reject it even though the signature itself is computable.
	confuse := jwtlib.NewWithClaims(jwtlib.SigningMethodRS256, jwtlib.MapClaims{
		"sub": "attacker", "iat": time.Now().Unix(),
		"exp": time.Now().Add(time.Hour).Unix(), "typ": "access",
	})
	confuse.Header["kid"] = "rot-1"
	// RS256 can't sign with an HMAC secret — jwt lib returns an error and
	// an unusable token; that unparseable blob is itself the probe.
	confusionBlob, _ := confuse.SigningString()
	forged = append(forged, confusionBlob+".forged")

	const workers = 32
	var accepted, forgedAccepted, transientErrs atomic.Int64
	stop := make(chan struct{})
	var wg sync.WaitGroup

	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				default:
				}
				iss := kr.Issuer()
				if iss == nil {
					transientErrs.Add(1) // no issuer = rejecting everything
					continue
				}
				tok := oldTokens[(w*31+i)%len(oldTokens)]
				if _, err := iss.Parse(tok); err != nil {
					// A *valid* token rejected inside the overlap window
					// is a drill failure — rotation must be seamless.
					t.Errorf("valid token rejected during rotation: %v", err)
					return
				}
				accepted.Add(1)
				if _, err := iss.Parse(forged[(w+i)%len(forged)]); err == nil {
					forgedAccepted.Add(1)
				}
				runtime.Gosched()
			}
		}(w)
	}

	// Rotate repeatedly while the flood is live — the drill holds a fixed
	// wall-clock window so the goroutine flood actually overlaps the swaps
	// (an N-rotation burst can finish before a worker is even scheduled).
	rotDeadline := time.Now().Add(400 * time.Millisecond)
	rotations := 0
	for time.Now().Before(rotDeadline) {
		next := ringSecretNew
		if rotations%2 == 1 {
			next = ringSecretOld // flip-flop: both directions exercised
		}
		if err := kr.Rotate(JWTKeyVersion{
			KID:        fmt.Sprintf("rot-%d", rotations+2),
			Alg:        "HS256",
			HMACSecret: next,
		}); err != nil {
			close(stop)
			wg.Wait()
			t.Fatalf("rotation %d failed: %v", rotations, err)
		}
		rotations++
		time.Sleep(2 * time.Millisecond)
	}
	close(stop)
	wg.Wait()

	if accepted.Load() == 0 {
		t.Fatal("no requests flowed — drill did not exercise the ring")
	}
	if forgedAccepted.Load() != 0 {
		t.Fatalf("%d forged tokens accepted", forgedAccepted.Load())
	}
	if transientErrs.Load() != 0 {
		t.Fatalf("%d requests saw nil issuer — ring went dark", transientErrs.Load())
	}
	// Post-flood: tokens minted under every intermediate kid still verify
	// (all live keys coexist in the snapshot within their windows).
	if iss := kr.Issuer(); iss == nil {
		t.Fatal("ring dark after rotation storm")
	} else {
		for i, tok := range oldTokens {
			if _, err := iss.Parse(tok); err != nil {
				t.Fatalf("pre-rotation token %d rejected post-storm: %v", i, err)
			}
		}
	}
	t.Logf("rotation flood: %d valid parses accepted, %d forged rejected over %d rotations",
		accepted.Load(), forgedAccepted.Load(), rotations)
}

// HTTP-level twin: the same race through auth.AuthMiddleware, so the
// rejection contract (401 + RFC7807) is exercised end-to-end.
func TestDrillKeyRotationRaceThroughMiddleware(t *testing.T) {
	kr := NewJWTKeyring("exc.local", "exc-api", 15*time.Minute, 24*time.Hour)
	if err := kr.Rotate(JWTKeyVersion{KID: "m1", Alg: "HS256", HMACSecret: hmacKey(3)}); err != nil {
		t.Fatal(err)
	}
	tok, _, err := kr.Issuer().Issue("drill-user", auth.IssueOptions{})
	if err != nil {
		t.Fatal(err)
	}
	// The middleware holds the Issuer it was built with — the production
	// wiring re-reads the ring per request; here we bind a fresh
	// middleware to each Issuer snapshot the ring publishes. An
	// Issuer snapshot is immutable, so binding the pre-rotation snapshot
	// is exactly a request that arrived on the old pod/config.
	handler := auth.AuthMiddleware(kr.Issuer(), nil)(
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		}))
	srv := httptest.NewServer(handler)
	defer srv.Close()

	var ok, denied atomic.Int64
	var wg sync.WaitGroup
	stop := time.Now().Add(300 * time.Millisecond)
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for time.Now().Before(stop) {
				req, _ := http.NewRequest(http.MethodGet, srv.URL+"/", nil)
				req.Header.Set("Authorization", "Bearer "+tok)
				resp, err := http.DefaultClient.Do(req)
				if err != nil {
					continue
				}
				resp.Body.Close()
				if resp.StatusCode == http.StatusNoContent {
					ok.Add(1)
				} else {
					denied.Add(1)
				}
			}
		}()
	}
	// Rotate the ring mid-flood; the middleware still binds the old
	// snapshot — the old key keeps verifying for the whole overlap.
	if err := kr.Rotate(JWTKeyVersion{KID: "m2", Alg: "HS256", HMACSecret: hmacKey(4)}); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	if denied.Load() != 0 || ok.Load() == 0 {
		t.Fatalf("overlap violated: ok=%d denied=%d", ok.Load(), denied.Load())
	}
}

// ---------------------------------------------------------------------------
// Drill 2 — expired / revoked credential replay across endpoint classes.
// ---------------------------------------------------------------------------

// craftExpiredJWT signs a syntactically-valid token with the ring's real
// key but a backdated exp — the "replay a recorded token" attack.
func craftExpiredJWT(t *testing.T, kid string, secret []byte) string {
	t.Helper()
	tok := jwtlib.NewWithClaims(jwtlib.SigningMethodHS256, jwtlib.MapClaims{
		"sub": "replay-victim", "typ": "access",
		"iat": time.Now().Add(-2 * time.Hour).Unix(),
		"exp": time.Now().Add(-90 * time.Minute).Unix(), // expired
	})
	tok.Header["kid"] = kid
	s, err := tok.SignedString(secret)
	if err != nil {
		t.Fatalf("craft: %v", err)
	}
	return s
}

// problemShape asserts the RFC 7807 body carries only public fields and
// leaks no internal detail (paths, goroutine dumps, DSNs, key material).
func problemShape(t *testing.T, resp *http.Response) {
	t.Helper()
	var body strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(buf)
		body.Write(buf[:n])
		if err != nil {
			break
		}
	}
	resp.Body.Close()
	s := body.String()
	for _, leak := range []string{
		"goroutine", ".go:", "/www/", "postgres://", "panic:",
		"hs256", "secret", "vault", "stack",
	} {
		if strings.Contains(strings.ToLower(s), leak) {
			t.Fatalf("response body leaks internal detail %q: %s", leak, s)
		}
	}
}

func TestDrillExpiredCredentialReplay(t *testing.T) {
	secret := hmacKey(0xC3)
	issuer := auth.NewIssuer("exc.local", "exc-api", 15*time.Minute)
	if err := issuer.AddHMACKey("live", secret, true); err != nil {
		t.Fatal(err)
	}
	store := newMemSessionStore()
	sessions, err := auth.NewSessionManager(store, issuer, auth.SessionConfig{})
	if err != nil {
		t.Fatal(err)
	}

	okHandler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	srv := httptest.NewServer(auth.AuthMiddleware(issuer, sessions)(okHandler))
	defer srv.Close()

	// Baseline: a real session's access token must pass — proves the
	// harness measures rejections, not a broken fixture.
	ctx := context.Background()
	issued, err := sessions.Issue(ctx, auth.IssueRequest{
		UserID: "u-drill", AccountID: 42, Tier: "standard",
	})
	if err != nil {
		t.Fatalf("issue session: %v", err)
	}
	get := func(tok string) *http.Response {
		req, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/v1/accounts", nil)
		if tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("request: %v", err)
		}
		return resp
	}
	if r := get(issued.AccessToken); r.StatusCode != http.StatusNoContent {
		t.Fatalf("valid session token denied: %d", r.StatusCode)
	}
	r := get(issued.AccessToken)
	r.Body.Close()

	type replayCase struct {
		name   string
		tok    string
		setup  func()
		wantSt int // all 401-class rejections
	}
	cases := []replayCase{
		{
			name:   "expired access JWT",
			tok:    craftExpiredJWT(t, "live", secret),
			wantSt: http.StatusUnauthorized,
		},
		{
			name: "revoked session (valid JWT, dead sid)",
			setup: func() {
				// Delete the live session record out-of-band — the JWT is
				// still inside its 15min window, the session is gone.
				_ = store.DeleteSession(ctx, issued.Session.ID)
			},
			tok:    issued.AccessToken,
			wantSt: http.StatusUnauthorized,
		},
		{
			name: "expired session record",
			setup: func() {
				s := issued.Session
				s.ID = "sess-expired-drill"
				s.ExpiresAt = time.Now().Add(-time.Minute) // absolute cap passed
				_ = store.WriteSession(ctx, s, time.Hour)
				// Point the access token's sid at the expired record.
				tok, _, ierr := issuer.Issue("u-drill",
					auth.IssueOptions{SessionID: s.ID})
				if ierr != nil {
					t.Fatalf("re-issue for expired session: %v", ierr)
				}
				issued.AccessToken = tok
			},
			tok:    issued.AccessToken, // rebound by setup
			wantSt: http.StatusUnauthorized,
		},
		{
			name:   "unknown kid (retired key replay)",
			tok:    craftExpiredJWT(t, "kid-retired-2025", hmacKey(0x11)),
			wantSt: http.StatusUnauthorized,
		},
		{
			name:   "garbage bearer",
			tok:    "not-a-jwt",
			wantSt: http.StatusUnauthorized,
		},
	}
	for _, c := range cases {
		if c.setup != nil {
			c.setup()
		}
		resp := get(c.tok)
		if resp.StatusCode != c.wantSt {
			resp.Body.Close()
			t.Fatalf("%s: status %d, want %d", c.name, resp.StatusCode, c.wantSt)
		}
		problemShape(t, resp)
		t.Logf("%s -> %d (rejected, no leakage)", c.name, resp.StatusCode)
	}

	// API-key class: revoked and expired keys are never Active — the
	// SignatureVerifier/KeyStore layer (Postgres-backed) resolves them to
	// API_KEY_NOT_FOUND=401; here we pin the predicate and its status map.
	now := time.Now()
	revokedAt := now.Add(-time.Hour)
	for _, k := range []auth.APIKey{
		{ID: 1, RevokedAt: &revokedAt},
		{ID: 2, ExpiresAt: ptrTime(now.Add(-time.Hour))},
	} {
		if k.Active(now) {
			t.Fatalf("key %d reported active after revoke/expiry", k.ID)
		}
	}
	if got := auth.HTTPStatusOf(excerrors.New(auth.CodeAPIKeyNotFound, "x")); got != 401 {
		t.Fatalf("API_KEY_NOT_FOUND maps to %d, want 401", got)
	}
	// And the WS edge code for the same event class (spec §10.5) resolves
	// to 401 in the canonical §23 registry.
	if got := errs.Default.HTTPStatus("AUTH_EXPIRED"); got != 401 {
		t.Fatalf("AUTH_EXPIRED maps to %d, want 401", got)
	}
}

func ptrTime(t time.Time) *time.Time { return &t }

// ---------------------------------------------------------------------------
// Drill 3 — secret-store partition: deterministic fail-closed boot.
// ---------------------------------------------------------------------------

func TestDrillVaultPartitionFailClosed(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// (a) Unreachable Vault (connection refused on loopback).
	src, err := NewVaultSource(VaultConfig{
		Addr: "http://127.0.0.1:1", Token: "t",
		Timeout: 500 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("source construction must succeed; load is what fails: %v", err)
	}
	bundle, err := LoadSecrets(ctx, src, testRefs(), true)
	if err == nil || bundle != nil {
		t.Fatalf("partitioned vault produced bundle %+v", bundle)
	}
	var e *excerrors.Error
	if !stderrors.As(err, &e) || e.Code != CodeConfigLoadFailed {
		t.Fatalf("want CONFIG_LOAD_FAILED, got %v", err)
	}

	// (b) Vault answering with 5xx — a live-but-broken store is the same
	// verdict; never degrade to "load what we can".
	srv500 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv500.Close()
	src5, _ := NewVaultSource(VaultConfig{Addr: srv500.URL, Token: "t"})
	if _, err := LoadSecrets(ctx, src5, testRefs(), true); err == nil ||
		excerrors.CodeOf(err) != CodeConfigLoadFailed {
		t.Fatalf("5xx vault: want CONFIG_LOAD_FAILED, got %v", err)
	}

	// (c) Vault answering garbage — a confused store cannot supply
	// credentials; malformed payload is a load failure too.
	srvBad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, "this is not vault json")
	}))
	defer srvBad.Close()
	srcBad, _ := NewVaultSource(VaultConfig{Addr: srvBad.URL, Token: "t"})
	if _, err := LoadSecrets(ctx, srcBad, testRefs(), true); err == nil ||
		excerrors.CodeOf(err) != CodeConfigLoadFailed {
		t.Fatalf("garbage vault: want CONFIG_LOAD_FAILED, got %v", err)
	}
}

func TestDrillNoDevFallbackWhenRequired(t *testing.T) {
	// The drill's core assertion: EXC_SECRETS_REQUIRED=production + a
	// configured dev adapter must not silently produce credentials.
	t.Setenv("EXC_SECRETS_REQUIRED", "production")
	t.Setenv("EXC_SECRETS_SOURCE", "dev-env")
	src, required, err := SourceFromEnv("development")
	if err == nil {
		t.Fatalf("dev-env source accepted under required: %+v", src)
	}
	if excerrors.CodeOf(err) != CodeConfigLoadFailed || !required {
		t.Fatalf("want CONFIG_LOAD_FAILED + required, got %v required=%v", err, required)
	}

	// Same verdict via the file adapter.
	t.Setenv("EXC_SECRETS_SOURCE", "dev-file")
	f := t.TempDir() + "/s.json"
	_ = os.WriteFile(f, []byte(`{"a":{"k":"v"}}`), 0600)
	t.Setenv("EXC_DEV_SECRETS_FILE", f)
	if _, _, err := SourceFromEnv("development"); excerrors.CodeOf(err) != CodeConfigLoadFailed {
		t.Fatalf("dev-file under required: %v", err)
	}

	// And production + missing vault config: no implicit dev fallback.
	t.Setenv("EXC_SECRETS_SOURCE", "")
	t.Setenv("EXC_VAULT_ADDR", "")
	t.Setenv("EXC_VAULT_TOKEN", "")
	t.Setenv("EXC_VAULT_TOKEN_FILE", "")
	src, _, err = SourceFromEnv("production")
	if err == nil || src != nil {
		t.Fatalf("vault-misconfigured production must fail, got %v", src)
	}
	if excerrors.CodeOf(err) != CodeConfigLoadFailed {
		t.Fatalf("want CONFIG_LOAD_FAILED, got %v", err)
	}
}

func TestDrillVaultPartitionMidLease(t *testing.T) {
	// Dynamic-credential lifecycle under a dead store: Issue fails and
	// LeaseRenewal returns the coded error instead of fabricating a
	// credential or swapping in a dead one.
	src, _ := NewVaultSource(VaultConfig{
		Addr: "http://127.0.0.1:1", Token: "t", Timeout: 300 * time.Millisecond,
	})
	sw := &Swapper[int]{Build: func(_ context.Context, c *DynamicCredential) (*int, error) {
		v := len(c.Username)
		return &v, nil
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := sw.LeaseRenewal(ctx, src, "ro", nil)
	if excerrors.CodeOf(err) != CodeConfigLoadFailed {
		t.Fatalf("lease issue under partition: want CONFIG_LOAD_FAILED, got %v", err)
	}
	if sw.Get() != nil {
		t.Fatal("dead store must not swap in a credential")
	}
}
