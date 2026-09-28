// Task 8.3.5 scenario 4 — invalid HMAC / Ed25519 signature rejection
// (spec §8.1, §8.7 item 2, §2.7.2 tier L3 edge rejection).
//
// This drives the REAL signing path: auth.KeyStore over the scratch
// PostgreSQL (migration 025+073 schema), auth.SignatureVerifier's full
// §8.7 defense order (key → IP allowlist → 30s timestamp window →
// replay cache → signature), and the gateway Router's registry-mapped
// RFC 7807 envelope — the same code→status mapping writeServiceErr uses
// on the production order routes.
//
// Contract asserted per fault:
//   - wrong/corrupt HMAC signature   → INVALID_SIGNATURE (401)
//   - valid signature, mutated body  → INVALID_SIGNATURE (401)
//   - timestamp outside ±30s         → TIMESTAMP_OUT_OF_WINDOW (401)
//   - identical request replayed     → REPLAY_ATTACK_DETECTED (401)
//   - Ed25519 bad signature          → ASYMMETRIC_KEY_INVALID (401)
//   - unknown key id                 → API_KEY_NOT_FOUND (401)
//   - missing signing headers        → UNAUTHORIZED (401)
//   - every rejection lands before any business handler runs (L3: edge
//     rejection, no internal IPC reached).
//
// Gate: EXC_PG_TEST=1 (the api_keys store is PostgreSQL). The replay
// guard is the package's in-memory test seam; a second subtest variant
// additionally runs the real Redis SETNX guard when EXC_REDIS_TEST=1.
package error_scenarios

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"exchange/internal/auth"
	"exchange/internal/gateway"
)

// memReplayGuard is the documented in-memory ReplayGuard test seam
// (signing.go: "a fake in tests").
type memReplayGuard struct {
	mu   sync.Mutex
	seen map[string]bool
}

func newMemReplayGuard() *memReplayGuard { return &memReplayGuard{seen: map[string]bool{}} }

func (g *memReplayGuard) SetOnce(_ context.Context, key string, _ time.Duration) (bool, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.seen[key] {
		return false, nil
	}
	g.seen[key] = true
	return true, nil
}

// testSecretBox returns a SecretBox over deterministic test key material
// (non-production only — mirrors cmd/gateway's dev fallback).
func testSecretBox(t *testing.T) *auth.SecretBox {
	t.Helper()
	key := sha256.Sum256([]byte("exc.local errscen signature-test data key"))
	box, err := auth.NewSecretBox(key[:])
	if err != nil {
		t.Fatalf("secret box: %v", err)
	}
	return box
}

// seedPrincipal inserts a user + SPOT account and returns their ids.
func seedPrincipal(t *testing.T, pool *pgxpool.Pool) (userID, accountID int64) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	email := fmt.Sprintf("errscen-%d@example.invalid", time.Now().UnixNano())
	if err := pool.QueryRow(ctx,
		`INSERT INTO users (email) VALUES ($1) RETURNING id`, email).Scan(&userID); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	if err := pool.QueryRow(ctx, `
		INSERT INTO accounts (user_id, account_type) VALUES ($1,'SPOT')
		RETURNING id`, userID).Scan(&accountID); err != nil {
		t.Fatalf("seed account: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_, _ = pool.Exec(ctx, `DELETE FROM api_keys WHERE account_id=$1`, accountID)
		_, _ = pool.Exec(ctx, `DELETE FROM accounts WHERE id=$1`, accountID)
		_, _ = pool.Exec(ctx, `DELETE FROM users WHERE id=$1`, userID)
	})
	return userID, accountID
}

// signingEdge mounts the real verifier behind the gateway edge:
// request-id → verify → business handler; errors map through the router
// registry exactly like api.writeServiceErr (same code→status source).
// reached flips only if the request survived verification — the L3
// "rejected before internal IPC" assertion.
func signingEdge(r *gateway.Router, v *auth.SignatureVerifier, reached *bool) http.Handler {
	biz := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		*reached = true
		w.WriteHeader(http.StatusNoContent)
	})
	edge := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var body []byte
		if req.Body != nil {
			body, _ = io.ReadAll(req.Body)
		}
		if _, err := v.Verify(req.Context(),
			auth.SignedRequestFromHTTP(req, body, false)); err != nil {
			r.WriteErr(w, req, err)
			return
		}
		biz.ServeHTTP(w, req)
	})
	return gateway.RequestID(edge)
}

// wireRequest is a raw signed-request tuple, reusable for replays.
type wireRequest struct {
	method, path string
	body         []byte
	keyID        string
	ts           string
	sig          string // base64url
}

func (w wireRequest) toHTTP() *http.Request {
	req := httptest.NewRequest(w.method, "http://exc.test"+w.path, bytes.NewReader(w.body))
	req.Header.Set("X-API-KEY", w.keyID)
	req.Header.Set("X-TIMESTAMP", w.ts)
	req.Header.Set("X-SIGNATURE", w.sig)
	return req
}

// sign builds a wireRequest signed at the given instant.
func sign(keyID string, signer func([]byte) []byte,
	ts time.Time, method, path string, body []byte) wireRequest {
	tsStr := strconv.FormatInt(ts.Unix(), 10)
	canonical := auth.CanonicalRequest(tsStr, method, path, body)
	return wireRequest{
		method: method, path: path, body: body, keyID: keyID, ts: tsStr,
		sig: base64.RawURLEncoding.EncodeToString(signer(canonical)),
	}
}

func hmacSigner(secret string) func([]byte) []byte {
	return func(canonical []byte) []byte {
		m := hmac.New(sha256.New, []byte(secret))
		m.Write(canonical)
		return m.Sum(nil)
	}
}

// requireProblem asserts the exact coded rejection and the RFC 7807
// envelope shape.
func requireProblem(t *testing.T, rec *httptest.ResponseRecorder, code string, status int) {
	t.Helper()
	if rec.Code != status {
		t.Fatalf("HTTP status=%d, want %d; body=%s", rec.Code, status, rec.Body.String())
	}
	p := decodeProblem(t, rec)
	if p.Error != code {
		t.Fatalf("envelope code=%s, want %s; body=%s", p.Error, code, rec.Body.String())
	}
	if p.Status != status {
		t.Fatalf("envelope status=%d, want %d", p.Status, status)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/problem+json" {
		t.Fatalf("content-type=%q, want application/problem+json", ct)
	}
}

func TestSignatureRejection(t *testing.T) {
	pool := pgPool(t)
	userID, accountID := seedPrincipal(t, pool)

	ks, err := auth.NewKeyStore(pool, testSecretBox(t))
	if err != nil {
		t.Fatalf("key store: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	// Issue one HMAC key and one Ed25519 key — the real issuance path.
	hmacKey, hmacSecret, err := ks.CreateHMAC(ctx, auth.KeyRequest{
		AccountID: accountID, UserID: userID, Label: "errscen-hmac",
		Scopes: []string{"trade"},
	})
	if err != nil {
		t.Fatalf("create HMAC key: %v", err)
	}
	edPub, edPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("ed25519 keygen: %v", err)
	}
	edDER, err := x509.MarshalPKIXPublicKey(edPub)
	if err != nil {
		t.Fatalf("marshal ed25519 pubkey: %v", err)
	}
	edKey, err := ks.RegisterAsymmetric(ctx, auth.KeyRequest{
		AccountID: accountID, UserID: userID, Label: "errscen-ed25519",
		Algorithm: auth.AlgEdDSA, PublicKey: edDER, Scopes: []string{"trade"},
	}, auth.KeyTypeEd25519)
	if err != nil {
		t.Fatalf("register ed25519 key: %v", err)
	}
	edSigner := func(c []byte) []byte { return ed25519.Sign(edPriv, c) }

	newVerifier := func() *auth.SignatureVerifier {
		v, err := auth.NewSignatureVerifier(ks, newMemReplayGuard())
		if err != nil {
			t.Fatalf("verifier: %v", err)
		}
		return v
	}

	const path = "/api/v1/orders"
	body := []byte(`{"symbol":"EUR/USD","side":"BUY","type":"LIMIT","quantity":"1000","price":"1.1000"}`)

	t.Run("ValidHMACAccepted", func(t *testing.T) {
		var reached bool
		rec := httptest.NewRecorder()
		signingEdge(newTestRouter(), newVerifier(), &reached).ServeHTTP(rec,
			sign(hmacKey.KeyID, hmacSigner(hmacSecret), time.Now(),
				http.MethodPost, path, body).toHTTP())
		if rec.Code != http.StatusNoContent || !reached {
			t.Fatalf("valid signed request rejected: status=%d body=%s", rec.Code, rec.Body.String())
		}
	})

	t.Run("WrongSecret_InvalidSignature", func(t *testing.T) {
		var reached bool
		rec := httptest.NewRecorder()
		signingEdge(newTestRouter(), newVerifier(), &reached).ServeHTTP(rec,
			sign(hmacKey.KeyID, hmacSigner("not-the-secret"), time.Now(),
				http.MethodPost, path, body).toHTTP())
		requireProblem(t, rec, "INVALID_SIGNATURE", http.StatusUnauthorized)
		if reached {
			t.Fatal("rejected request reached business handler — L3 edge contract violated")
		}
	})

	t.Run("MutatedBody_InvalidSignature", func(t *testing.T) {
		var reached bool
		// Sign body A, then tamper the payload on the wire.
		w := sign(hmacKey.KeyID, hmacSigner(hmacSecret), time.Now(),
			http.MethodPost, path, body)
		w.body = []byte(`{"symbol":"EUR/USD","side":"BUY","type":"LIMIT","quantity":"999999","price":"1.1000"}`)
		rec := httptest.NewRecorder()
		signingEdge(newTestRouter(), newVerifier(), &reached).ServeHTTP(rec, w.toHTTP())
		requireProblem(t, rec, "INVALID_SIGNATURE", http.StatusUnauthorized)
		if reached {
			t.Fatal("mutated-body request reached business handler")
		}
	})

	t.Run("StaleTimestamp_OutOfWindow", func(t *testing.T) {
		var reached bool
		rec := httptest.NewRecorder()
		signingEdge(newTestRouter(), newVerifier(), &reached).ServeHTTP(rec,
			sign(hmacKey.KeyID, hmacSigner(hmacSecret),
				time.Now().Add(-auth.SignatureWindow-time.Minute),
				http.MethodPost, path, body).toHTTP())
		requireProblem(t, rec, "TIMESTAMP_OUT_OF_WINDOW", http.StatusUnauthorized)
		if reached {
			t.Fatal("stale-timestamp request reached business handler")
		}
	})

	t.Run("Replay_ReplayDetected", func(t *testing.T) {
		var reached bool
		edge := signingEdge(newTestRouter(), newVerifier(), &reached)
		w := sign(hmacKey.KeyID, hmacSigner(hmacSecret), time.Now(),
			http.MethodPost, path, body)

		rec1 := httptest.NewRecorder()
		edge.ServeHTTP(rec1, w.toHTTP())
		if rec1.Code != http.StatusNoContent || !reached {
			t.Fatalf("first request rejected: %d %s", rec1.Code, rec1.Body.String())
		}
		// Identical signature tuple replayed inside the window.
		reached = false
		rec2 := httptest.NewRecorder()
		edge.ServeHTTP(rec2, w.toHTTP())
		requireProblem(t, rec2, "REPLAY_ATTACK_DETECTED", http.StatusUnauthorized)
		if reached {
			t.Fatal("replayed request reached business handler")
		}
	})

	t.Run("BadEd25519_AsymmetricKeyInvalid", func(t *testing.T) {
		var reached bool
		// Sign with a DIFFERENT Ed25519 key — valid encoding, wrong signer.
		_, otherPriv, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatalf("other keygen: %v", err)
		}
		rec := httptest.NewRecorder()
		signingEdge(newTestRouter(), newVerifier(), &reached).ServeHTTP(rec,
			sign(edKey.KeyID, func(c []byte) []byte { return ed25519.Sign(otherPriv, c) },
				time.Now(), http.MethodPost, path, body).toHTTP())
		requireProblem(t, rec, "ASYMMETRIC_KEY_INVALID", http.StatusUnauthorized)
		if reached {
			t.Fatal("bad-Ed25519 request reached business handler")
		}
	})

	t.Run("ValidEd25519Accepted", func(t *testing.T) {
		var reached bool
		rec := httptest.NewRecorder()
		signingEdge(newTestRouter(), newVerifier(), &reached).ServeHTTP(rec,
			sign(edKey.KeyID, edSigner, time.Now(), http.MethodPost, path, body).toHTTP())
		if rec.Code != http.StatusNoContent || !reached {
			t.Fatalf("valid ed25519 request rejected: %d %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("UnknownKey_APIKeyNotFound", func(t *testing.T) {
		var reached bool
		rec := httptest.NewRecorder()
		signingEdge(newTestRouter(), newVerifier(), &reached).ServeHTTP(rec,
			sign("ak_nonexistentkey", hmacSigner("x"), time.Now(),
				http.MethodPost, path, body).toHTTP())
		requireProblem(t, rec, "API_KEY_NOT_FOUND", http.StatusUnauthorized)
		if reached {
			t.Fatal("unknown-key request reached business handler")
		}
	})

	t.Run("MissingHeaders_Unauthorized", func(t *testing.T) {
		var reached bool
		req := httptest.NewRequest(http.MethodPost, "http://exc.test"+path,
			bytes.NewReader(body)) // no signing headers at all
		rec := httptest.NewRecorder()
		signingEdge(newTestRouter(), newVerifier(), &reached).ServeHTTP(rec, req)
		requireProblem(t, rec, "UNAUTHORIZED", http.StatusUnauthorized)
		if reached {
			t.Fatal("unsigned request reached business handler")
		}
	})

	// Real-Redis replay guard: the same signed tuple re-presented must be
	// rejected by the SETNX-backed replay cache — the production
	// detection mechanism, not the in-memory seam.
	t.Run("Replay_RedisGuard", func(t *testing.T) {
		c := redisClient(t) // skips unless EXC_REDIS_TEST=1
		v, err := auth.NewSignatureVerifier(ks, auth.NewGoRedisReplayGuard(c.Client))
		if err != nil {
			t.Fatalf("redis verifier: %v", err)
		}
		var reached bool
		edge := signingEdge(newTestRouter(), v, &reached)
		w := sign(hmacKey.KeyID, hmacSigner(hmacSecret), time.Now(),
			http.MethodPost, path, body)
		t.Cleanup(func() { // drop only the replay marker this test created
			cctx, ccancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer ccancel()
			sigHash := sha256.Sum256([]byte(w.keyID + "|" + w.ts + "|" + w.sig))
			_ = c.Del(cctx, fmt.Sprintf("sig_replay:%x", sigHash)).Err()
		})

		rec1 := httptest.NewRecorder()
		edge.ServeHTTP(rec1, w.toHTTP())
		if rec1.Code != http.StatusNoContent || !reached {
			t.Fatalf("first signed request rejected: %d %s", rec1.Code, rec1.Body.String())
		}
		reached = false
		rec2 := httptest.NewRecorder()
		edge.ServeHTTP(rec2, w.toHTTP())
		requireProblem(t, rec2, "REPLAY_ATTACK_DETECTED", http.StatusUnauthorized)
		if reached {
			t.Fatal("replayed request reached business handler (real Redis guard)")
		}
	})
}
