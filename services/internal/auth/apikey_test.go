// Integration tests for the api_keys / oauth_clients stores and the
// signed-request verifier (Tasks 5.3.9, 5.3.38, 5.3.1-OAuth2).
//
// Gated: skipped unless EXC_PG_TEST=1. Applies migrations 002, 003, 025,
// 073, 151 into a throwaway schema so the DDL under test is the real
// thing. DSN: EXC_PG_DSN, else the dev Postgres default.
//
// Run: EXC_PG_TEST=1 EXC_PG_DSN=... go test ./internal/auth/ -v
package auth

import (
	"context"
	"crypto"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func testDSN() string {
	if d := os.Getenv("EXC_PG_DSN"); d != "" {
		return d
	}
	if d := os.Getenv("EXC_TEST_DSN"); d != "" {
		return d
	}
	return "postgres://exchange:exchange_dev@127.0.0.1:5433/exchange?sslmode=disable"
}

// itestAuth applies migrations into a scratch schema and returns a pool
// bound to it plus fixture ids (user, account).
func itestAuth(t *testing.T) (*pgxpool.Pool, int64, int64) {
	t.Helper()
	if os.Getenv("EXC_PG_TEST") != "1" {
		t.Skip("set EXC_PG_TEST=1 to run Postgres integration tests")
	}
	dsn := testDSN()
	schema := fmt.Sprintf("auth_itest_%d", time.Now().UnixNano())
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("dsn: %v", err)
	}
	cfg.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol
	cfg.RuntimeParams["search_path"] = schema + ",public"
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Skipf("postgres unreachable: %v", err)
	}
	if _, err := conn.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		conn.Close(ctx)
		t.Fatalf("create schema: %v", err)
	}
	for _, f := range []string{
		"002_create_users", "003_create_accounts",
		"025_create_api_keys", "073_api_key_asymmetric_types", "151_oauth_clients",
	} {
		sql, err := os.ReadFile("../db/migrations/" + f + ".up.sql")
		if err != nil {
			conn.Close(ctx)
			t.Fatalf("read %s: %v", f, err)
		}
		if _, err := conn.Exec(ctx, string(sql)); err != nil {
			conn.Close(ctx)
			t.Fatalf("apply %s: %v", f, err)
		}
	}
	// Rollback proof: down then re-up for the auth-owned migrations
	// (reverse dependency order; accounts/users stay up).
	for _, f := range []string{
		"151_oauth_clients", "073_api_key_asymmetric_types", "025_create_api_keys",
	} {
		sql, err := os.ReadFile("../db/migrations/" + f + ".down.sql")
		if err != nil {
			conn.Close(ctx)
			t.Fatalf("read down %s: %v", f, err)
		}
		if _, err := conn.Exec(ctx, string(sql)); err != nil {
			conn.Close(ctx)
			t.Fatalf("down %s: %v", f, err)
		}
	}
	for _, f := range []string{
		"025_create_api_keys", "073_api_key_asymmetric_types", "151_oauth_clients",
	} {
		sql, err := os.ReadFile("../db/migrations/" + f + ".up.sql")
		if err != nil {
			conn.Close(ctx)
			t.Fatalf("read re-up %s: %v", f, err)
		}
		if _, err := conn.Exec(ctx, string(sql)); err != nil {
			conn.Close(ctx)
			t.Fatalf("re-up %s: %v", f, err)
		}
	}
	conn.Close(ctx)
	t.Cleanup(func() {
		c2, cancel2 := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel2()
		bare, err := pgx.ParseConfig(dsn)
		if err != nil {
			return
		}
		c, err := pgx.ConnectConfig(c2, bare)
		if err == nil {
			_, _ = c.Exec(c2, "DROP SCHEMA IF EXISTS "+schema+" CASCADE")
			c.Close(c2)
		}
	})

	pcfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("pool dsn: %v", err)
	}
	pcfg.ConnConfig.RuntimeParams["search_path"] = schema + ",public"
	pool, err := pgxpool.NewWithConfig(ctx, pcfg)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)

	var userID, accountID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO users (email) VALUES ($1) RETURNING id`,
		"itest-"+schema+"@example.com").Scan(&userID); err != nil {
		t.Fatalf("fixture user: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO accounts (user_id, account_type) VALUES ($1,'SPOT') RETURNING id`,
		userID).Scan(&accountID); err != nil {
		t.Fatalf("fixture account: %v", err)
	}
	return pool, userID, accountID
}

// fakeReplay is an in-memory ReplayGuard for signing tests.
type fakeReplay struct{ seen map[string]bool }

func (f *fakeReplay) SetOnce(_ context.Context, key string, _ time.Duration) (bool, error) {
	if f.seen[key] {
		return false, nil
	}
	f.seen[key] = true
	return true, nil
}

func testBox(t *testing.T) *SecretBox {
	t.Helper()
	box, err := NewSecretBox([]byte(strings.Repeat("k", 32)))
	if err != nil {
		t.Fatal(err)
	}
	return box
}

func testVerifier(t *testing.T, ks *KeyStore) *SignatureVerifier {
	t.Helper()
	v, err := NewSignatureVerifier(ks, &fakeReplay{seen: map[string]bool{}})
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func signReq(keyID, method, path string, body []byte, ip string) (SignedRequest, []byte) {
	ts := fmt.Sprint(time.Now().Unix())
	canonical := CanonicalRequest(ts, method, path, body)
	return SignedRequest{
		KeyID: keyID, Timestamp: ts, Method: method, Path: path,
		Body: body, RemoteIP: ip,
	}, canonical
}

func TestHMACKeyLifecycleAndVerify(t *testing.T) {
	pool, userID, accountID := itestAuth(t)
	ctx := context.Background()
	ks, err := NewKeyStore(pool, testBox(t))
	if err != nil {
		t.Fatal(err)
	}
	v := testVerifier(t, ks)

	key, secret, err := ks.CreateHMAC(ctx, KeyRequest{
		AccountID: accountID, UserID: userID, Label: "legacy",
		Scopes: []string{"read", "trade"}, IPAllowlist: []string{"10.0.0.0/8"},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if key.KeyType != KeyTypeHMAC || len(key.SecretEnc) == 0 {
		t.Fatalf("HMAC key must store wrapped secret: %+v", key)
	}
	if strings.Contains(string(key.SecretEnc), secret) {
		t.Fatal("raw secret must never be stored")
	}
	// Round-trip: sign canonical, verify.
	req, canonical := signReq(key.KeyID, "POST", "/api/v1/orders", []byte("{}"), "10.9.9.9")
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(canonical)
	req.Signature = base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	got, err := v.Verify(ctx, req)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if got.KeyID != key.KeyID || !contains(got.Scopes, "trade") {
		t.Fatalf("verified key mismatch: %+v", got)
	}
	// IP outside the allowlist → TOKEN_IP_FORBIDDEN before signature work.
	bad := req
	bad.RemoteIP = "192.168.1.1"
	_, err = v.Verify(ctx, bad)
	requireCode(t, err, CodeTokenIPForbidden)
	// Revoked key is indistinguishable from unknown.
	if err := ks.Revoke(ctx, key.KeyID, "test"); err != nil {
		t.Fatal(err)
	}
	_, err = v.Verify(ctx, SignedRequest{KeyID: key.KeyID, Timestamp: req.Timestamp, Signature: "x"})
	requireCode(t, err, CodeAPIKeyNotFound)
}

func TestEd25519KeyEndToEnd(t *testing.T) {
	pool, userID, accountID := itestAuth(t)
	ctx := context.Background()
	ks, _ := NewKeyStore(pool, testBox(t))
	v := testVerifier(t, ks)

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	key, err := ks.RegisterAsymmetric(ctx, KeyRequest{
		AccountID: accountID, UserID: userID, Algorithm: AlgEdDSA,
		PublicKey: der, Scopes: []string{"trade"},
	}, KeyTypeEd25519)
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if len(key.PublicKey) == 0 || len(key.SecretEnc) != 0 {
		t.Fatal("asymmetric key must store public key only")
	}
	req, canonical := signReq(key.KeyID, "GET", "/api/v1/account/balances", nil, "127.0.0.1")
	sig := ed25519.Sign(priv, canonical)
	req.Signature = base64.RawURLEncoding.EncodeToString(sig)
	if _, err := v.Verify(ctx, req); err != nil {
		t.Fatalf("ed25519 verify: %v", err)
	}
	// Tampered signature → ASYMMETRIC_KEY_INVALID.
	bad := req
	bad.Timestamp = fmt.Sprint(time.Now().Unix())
	badCan := CanonicalRequest(bad.Timestamp, bad.Method, bad.Path, bad.Body)
	bad.Signature = base64.RawURLEncoding.EncodeToString(ed25519.Sign(priv, append(badCan, '!')))
	_, err = v.Verify(ctx, bad)
	requireCode(t, err, CodeAsymmetricKeyInvalid)
}

func TestRSAKeyEndToEnd(t *testing.T) {
	pool, userID, accountID := itestAuth(t)
	ctx := context.Background()
	ks, _ := NewKeyStore(pool, testBox(t))
	v := testVerifier(t, ks)

	for _, alg := range []string{AlgRS256, AlgPS256} {
		priv, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			t.Fatal(err)
		}
		der, err := x509.MarshalPKIXPublicKey(&priv.PublicKey)
		if err != nil {
			t.Fatal(err)
		}
		key, err := ks.RegisterAsymmetric(ctx, KeyRequest{
			AccountID: accountID, UserID: userID, Algorithm: alg,
			PublicKey: der, Scopes: []string{"read"},
		}, KeyTypeRSA)
		if err != nil {
			t.Fatalf("register %s: %v", alg, err)
		}
		req, canonical := signReq(key.KeyID, "GET", "/api/v1/positions", nil, "127.0.0.1")
		digest := sha256.Sum256(canonical)
		var sig []byte
		if alg == AlgRS256 {
			sig, err = rsa.SignPKCS1v15(rand.Reader, priv, crypto.SHA256, digest[:])
		} else {
			sig, err = rsa.SignPSS(rand.Reader, priv, crypto.SHA256, digest[:], nil)
		}
		if err != nil {
			t.Fatal(err)
		}
		req.Signature = base64.RawURLEncoding.EncodeToString(sig)
		if _, err := v.Verify(ctx, req); err != nil {
			t.Fatalf("%s verify: %v", alg, err)
		}
	}
	// RSA-1024 must refuse registration.
	weak, _ := rsa.GenerateKey(rand.Reader, 1024)
	der, _ := x509.MarshalPKIXPublicKey(&weak.PublicKey)
	_, err := ks.RegisterAsymmetric(ctx, KeyRequest{
		AccountID: accountID, UserID: userID, Algorithm: AlgRS256, PublicKey: der,
	}, KeyTypeRSA)
	requireCode(t, err, CodeAsymmetricKeyInvalid)
}

func TestKeyRotationOverlap(t *testing.T) {
	pool, userID, accountID := itestAuth(t)
	ctx := context.Background()
	ks, _ := NewKeyStore(pool, testBox(t))
	v := testVerifier(t, ks)

	pub1, priv1, _ := ed25519.GenerateKey(rand.Reader)
	der1, _ := x509.MarshalPKIXPublicKey(pub1)
	old, err := ks.RegisterAsymmetric(ctx, KeyRequest{
		AccountID: accountID, UserID: userID, Algorithm: AlgEdDSA, PublicKey: der1,
	}, KeyTypeEd25519)
	if err != nil {
		t.Fatal(err)
	}
	pub2, _, _ := ed25519.GenerateKey(rand.Reader)
	der2, _ := x509.MarshalPKIXPublicKey(pub2)
	newKey, err := ks.Rotate(ctx, old.KeyID, KeyRequest{
		Algorithm: AlgEdDSA, PublicKey: der2,
	}, time.Hour)
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if newKey.RotatesFromID == nil || *newKey.RotatesFromID != old.ID {
		t.Fatalf("rotation lineage missing: %+v", newKey)
	}
	// Predecessor remains valid inside the overlap window.
	oldReload, err := ks.Get(ctx, old.KeyID)
	if err != nil {
		t.Fatal(err)
	}
	if !oldReload.Active(time.Now()) || oldReload.OverlapUntil == nil {
		t.Fatal("predecessor must stay valid until overlap_until")
	}
	req, canonical := signReq(old.KeyID, "GET", "/api/v1/orders", nil, "127.0.0.1")
	req.Signature = base64.RawURLEncoding.EncodeToString(ed25519.Sign(priv1, canonical))
	if _, err := v.Verify(ctx, req); err != nil {
		t.Fatalf("predecessor must verify during overlap: %v", err)
	}
	// Overlap expired → inactive.
	expired := time.Now().Add(2 * time.Hour)
	if oldReload.Active(expired) {
		t.Fatal("predecessor must be inactive after overlap_until")
	}
	// Zero-overlap rotation revokes immediately.
	pub3, _, _ := ed25519.GenerateKey(rand.Reader)
	der3, _ := x509.MarshalPKIXPublicKey(pub3)
	if _, err := ks.Rotate(ctx, newKey.KeyID, KeyRequest{Algorithm: AlgEdDSA, PublicKey: der3}, 0); err != nil {
		t.Fatal(err)
	}
	newReload, _ := ks.Get(ctx, newKey.KeyID)
	if newReload.Active(time.Now()) {
		t.Fatal("zero-overlap rotation must revoke predecessor immediately")
	}
	// Overlap bound enforced.
	if _, err := ks.Rotate(ctx, old.KeyID, KeyRequest{}, 0); err == nil {
		t.Fatal("rotating an inactive key must fail")
	}
}

func TestSignedRequestReplayAndWindow(t *testing.T) {
	pool, userID, accountID := itestAuth(t)
	ctx := context.Background()
	ks, _ := NewKeyStore(pool, testBox(t))
	v := testVerifier(t, ks)

	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	der, _ := x509.MarshalPKIXPublicKey(pub)
	key, err := ks.RegisterAsymmetric(ctx, KeyRequest{
		AccountID: accountID, UserID: userID, Algorithm: AlgEdDSA, PublicKey: der,
	}, KeyTypeEd25519)
	if err != nil {
		t.Fatal(err)
	}
	req, canonical := signReq(key.KeyID, "GET", "/api/v1/orders", nil, "127.0.0.1")
	req.Signature = base64.RawURLEncoding.EncodeToString(ed25519.Sign(priv, canonical))
	if _, err := v.Verify(ctx, req); err != nil {
		t.Fatalf("first verify: %v", err)
	}
	// Identical request inside the replay window → REPLAY_ATTACK_DETECTED.
	_, err = v.Verify(ctx, req)
	requireCode(t, err, CodeReplayAttackDetected)
	// Stale timestamp → TIMESTAMP_OUT_OF_WINDOW.
	old := req
	old.Timestamp = fmt.Sprint(time.Now().Add(-time.Minute).Unix())
	oldCan := CanonicalRequest(old.Timestamp, old.Method, old.Path, old.Body)
	old.Signature = base64.RawURLEncoding.EncodeToString(ed25519.Sign(priv, oldCan))
	_, err = v.Verify(ctx, old)
	requireCode(t, err, CodeTimestampOutOfWindow)
}

func TestOAuthClientCredentialsGrant(t *testing.T) {
	pool, _, accountID := itestAuth(t)
	ctx := context.Background()
	store, err := NewOAuthClientStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	issuer := testIssuer(t)
	c, secret, err := store.RegisterClient(ctx, accountID, "inst-mm", []string{"read", "trade"})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if !strings.HasPrefix(c.ClientID, "oc_") || secret == "" {
		t.Fatalf("bad client record: %+v", c)
	}
	// Grant: scope narrowing honored.
	grant, err := store.ClientCredentialsGrant(ctx, issuer, c.ClientID, secret, []string{"read"})
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	if len(grant.Scope) != 1 || grant.Scope[0] != "read" {
		t.Fatalf("scope intersection wrong: %v", grant.Scope)
	}
	claims, err := issuer.Parse(grant.AccessToken)
	if err != nil || claims.ClientID != c.ClientID || claims.AccountID != accountID {
		t.Fatalf("grant token: %v %+v", err, claims)
	}
	// Widening rejected.
	if _, err := store.ClientCredentialsGrant(ctx, issuer, c.ClientID, secret, []string{"admin"}); !codeIs(err, CodeInsufficientScope) {
		t.Fatalf("scope widening must fail INSUFFICIENT_SCOPE, got %v", err)
	}
	// Wrong secret / unknown client / revoked client → INVALID_CREDENTIALS.
	if _, err := store.ClientCredentialsGrant(ctx, issuer, c.ClientID, "wrong", nil); !codeIs(err, CodeInvalidCredentials) {
		t.Fatalf("bad secret: %v", err)
	}
	if _, err := store.ClientCredentialsGrant(ctx, issuer, "oc_nope", secret, nil); !codeIs(err, CodeInvalidCredentials) {
		t.Fatalf("unknown client: %v", err)
	}
	if err := store.RevokeClient(ctx, c.ClientID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ClientCredentialsGrant(ctx, issuer, c.ClientID, secret, nil); !codeIs(err, CodeInvalidCredentials) {
		t.Fatalf("revoked client: %v", err)
	}
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
