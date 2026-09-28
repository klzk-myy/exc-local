// Task 5.3.38 item 3 + Task 5.3.9 enforcement — canonical signed-request
// verification, identical across algorithms.
//
// Canonical form (Task 5.3.24 shares this exact construction for its
// HMAC middleware; keep in lock-step):
//
//		stringToSign = timestamp + "\n" + METHOD + "\n" + path + "\n" + sha256hex(body)
//
//	  - METHOD is the upper-cased HTTP verb; path is the request path
//	    including query string (query params are part of the signed
//	    surface).
//	  - timestamp is X-TIMESTAMP: unix seconds or milliseconds (magnitude
//	    detection); |now - ts| must be <= 30s (spec §8.7 window) else
//	    TIMESTAMP_OUT_OF_WINDOW.
//	  - signature is X-SIGNATURE: base64url or hex encoded raw signature.
//	  - Replay: sha256(key_id|timestamp|signature) is SETNX'd into Redis
//	    for 2× the timestamp window; a repeat is REPLAY_ATTACK_DETECTED.
//	  - IP allowlist (Task 5.3.9) is checked BEFORE signature work — a
//	    forbidden IP burns no crypto and leaks nothing about validity.
//
// Error codes (spec §8.7/§23): TOKEN_IP_FORBIDDEN, TIMESTAMP_OUT_OF_WINDOW,
// REPLAY_ATTACK_DETECTED, INVALID_SIGNATURE (HMAC), ASYMMETRIC_KEY_INVALID
// (Ed25519/RSA), API_KEY_NOT_FOUND, UNAUTHORIZED.
package auth

import (
	"context"
	"crypto"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	exchredis "exchange/internal/redis"

	goredis "github.com/redis/go-redis/v9"
)

// SignatureWindow is the spec §8.1/§8.7 signed-request timestamp window.
const SignatureWindow = 30 * time.Second

// CanonicalRequest builds the string-to-sign shared by every key type
// (Task 5.3.38 item 3: identical canonicalization across algorithms).
// ts is the raw X-TIMESTAMP header value — it is signed verbatim so a
// seconds-vs-millis re-format can never diverge signer and verifier.
func CanonicalRequest(ts, method, path string, body []byte) []byte {
	bodyHash := sha256.Sum256(body)
	var b strings.Builder
	b.Grow(len(ts) + len(method) + len(path) + 66)
	b.WriteString(ts)
	b.WriteByte('\n')
	b.WriteString(strings.ToUpper(method))
	b.WriteByte('\n')
	b.WriteString(path)
	b.WriteByte('\n')
	b.WriteString(hex.EncodeToString(bodyHash[:]))
	return []byte(b.String())
}

// SignedRequest is the parsed signing surface of an HTTP request.
type SignedRequest struct {
	KeyID     string // X-API-KEY
	Timestamp string // X-TIMESTAMP
	Signature string // X-SIGNATURE (base64url or hex)
	Method    string // upper-cased verb
	Path      string // path + query (RequestURI)
	Body      []byte
	RemoteIP  string // resolved client IP (token.go RequestClientIP)
}

// SignedRequestFromHTTP extracts the signing surface from r. Body is the
// raw request body — callers that already consumed r.Body must pass the
// buffered bytes themselves.
func SignedRequestFromHTTP(r *http.Request, body []byte, trustedProxy bool) SignedRequest {
	return SignedRequest{
		KeyID:     strings.TrimSpace(r.Header.Get("X-API-KEY")),
		Timestamp: strings.TrimSpace(r.Header.Get("X-TIMESTAMP")),
		Signature: strings.TrimSpace(r.Header.Get("X-SIGNATURE")),
		Method:    r.Method,
		Path:      r.URL.RequestURI(),
		Body:      body,
		RemoteIP:  RequestClientIP(r.RemoteAddr, r.Header, trustedProxy),
	}
}

// ReplayGuard is the once-only marker seam for replay detection —
// redisReplayGuard in production (SETNX PX), a fake in tests.
type ReplayGuard interface {
	// SetOnce returns true when key was first set, false when it already
	// existed (a replayed signature).
	SetOnce(ctx context.Context, key string, ttl time.Duration) (bool, error)
}

type redisReplayGuard struct{ c *exchredis.Client }

// NewRedisReplayGuard binds replay detection to the coordination client.
func NewRedisReplayGuard(c *exchredis.Client) ReplayGuard {
	return redisReplayGuard{c: c}
}

func (g redisReplayGuard) SetOnce(ctx context.Context, key string, ttl time.Duration) (bool, error) {
	ok, err := g.c.SetNX(ctx, key, "1", ttl).Result()
	if err != nil {
		return false, fmt.Errorf("redis replay mark: %w", err)
	}
	return ok, nil
}

// goredisReplayGuard is the same adapter for a bare *goredis.Client —
// tests and embedders that hold the raw client use this form.
func NewGoRedisReplayGuard(c *goredis.Client) ReplayGuard {
	return goredisReplayGuard{c: c}
}

type goredisReplayGuard struct{ c *goredis.Client }

func (g goredisReplayGuard) SetOnce(ctx context.Context, key string, ttl time.Duration) (bool, error) {
	ok, err := g.c.SetNX(ctx, key, "1", ttl).Result()
	if err != nil {
		return false, fmt.Errorf("redis replay mark: %w", err)
	}
	return ok, nil
}

// SignatureVerifier verifies API-key-signed requests. It owns the
// KeyStore (key material) and a ReplayGuard (replay cache) — both may be
// the coordination Redis + OLTP pool per spec §4.
type SignatureVerifier struct {
	keys   *KeyStore
	replay ReplayGuard
	now    func() time.Time
}

// NewSignatureVerifier wires the verifier. replay may be nil only in
// tests — production callers must wire Redis (a nil guard would disable
// replay detection; construction fails closed instead).
func NewSignatureVerifier(keys *KeyStore, replay ReplayGuard) (*SignatureVerifier, error) {
	if keys == nil {
		return nil, newError(CodeAuthInternal, "key store is nil")
	}
	if replay == nil {
		return nil, newError(CodeAuthInternal, "replay guard is nil")
	}
	return &SignatureVerifier{keys: keys, replay: replay, now: time.Now}, nil
}

// parseTimestamp accepts unix seconds or milliseconds.
func parseTimestamp(ts string) (time.Time, error) {
	n, err := strconv.ParseInt(strings.TrimSpace(ts), 10, 64)
	if err != nil {
		return time.Time{}, err
	}
	if n > 1e12 { // milliseconds
		return time.Unix(0, n*int64(time.Millisecond)), nil
	}
	return time.Unix(n, 0), nil
}

// decodeSignature accepts base64url (primary) or hex.
func decodeSignature(sig string) ([]byte, error) {
	if b, err := base64.RawURLEncoding.DecodeString(sig); err == nil {
		return b, nil
	}
	if b, err := base64.URLEncoding.DecodeString(sig); err == nil {
		return b, nil
	}
	return hex.DecodeString(sig)
}

// Verify resolves the key, applies the §8.7 defense order
// (key → IP allowlist → timestamp window → replay → signature) and
// returns the verified key on success. Every failure is a coded error.
func (v *SignatureVerifier) Verify(ctx context.Context, req SignedRequest) (*APIKey, error) {
	if req.KeyID == "" || req.Signature == "" || req.Timestamp == "" {
		return nil, newError(CodeUnauthorized, "missing signature headers")
	}
	key, err := v.keys.Get(ctx, req.KeyID)
	if err != nil {
		return nil, err // API_KEY_NOT_FOUND already coded
	}
	if !key.Active(v.now()) {
		return nil, newError(CodeAPIKeyNotFound, "key inactive")
	}
	// 5.3.9: IP allowlist before any crypto.
	if err := key.IPAllowlist.Check(req.RemoteIP); err != nil {
		return nil, err // TOKEN_IP_FORBIDDEN
	}
	ts, err := parseTimestamp(req.Timestamp)
	if err != nil {
		return nil, newError(CodeTimestampOutOfWindow, "unparseable timestamp")
	}
	skew := v.now().Sub(ts)
	if skew < 0 {
		skew = -skew
	}
	if skew > SignatureWindow {
		return nil, newError(CodeTimestampOutOfWindow, "timestamp outside 30s window")
	}
	// Replay cache: key bound to key_id + timestamp + signature so a
	// legitimate re-request after the window cannot collide.
	sigHash := sha256.Sum256([]byte(req.KeyID + "|" + req.Timestamp + "|" + req.Signature))
	fresh, err := v.replay.SetOnce(ctx,
		"sig_replay:"+hex.EncodeToString(sigHash[:]), 2*SignatureWindow)
	if err != nil {
		return nil, wrapError(CodeAuthInternal, "replay guard", err)
	}
	if !fresh {
		return nil, newError(CodeReplayAttackDetected, "signature seen within replay window")
	}
	canonical := CanonicalRequest(req.Timestamp, req.Method, req.Path, req.Body)
	if err := v.verifySignature(key, canonical, req.Signature); err != nil {
		return nil, err
	}
	if err := v.keys.TouchLastUsed(ctx, key.ID, req.RemoteIP); err != nil {
		return nil, err // bookkeeping failure fails closed (§2.7)
	}
	return key, nil
}

// verifySignature dispatches on the key's declared type/algorithm.
func (v *SignatureVerifier) verifySignature(key *APIKey, msg []byte, sigB64 string) error {
	sig, err := decodeSignature(sigB64)
	if err != nil || len(sig) == 0 {
		return newError(CodeInvalidSignature, "unparseable signature encoding")
	}
	switch key.KeyType {
	case KeyTypeHMAC:
		secret, err := v.keys.RevealHMACSecret(key)
		if err != nil {
			return err
		}
		mac := hmac.New(sha256.New, secret)
		mac.Write(msg)
		if !hmac.Equal(mac.Sum(nil), sig) {
			return newError(CodeInvalidSignature, "HMAC signature mismatch")
		}
		return nil
	case KeyTypeEd25519:
		parsed, err := x509.ParsePKIXPublicKey(key.PublicKey)
		if err != nil {
			return wrapError(CodeAsymmetricKeyInvalid, "stored public key unparseable", err)
		}
		pub, ok := parsed.(ed25519.PublicKey)
		if !ok || !ed25519.Verify(pub, msg, sig) {
			return newError(CodeAsymmetricKeyInvalid, "Ed25519 signature invalid")
		}
		return nil
	case KeyTypeRSA:
		parsed, err := x509.ParsePKIXPublicKey(key.PublicKey)
		if err != nil {
			return wrapError(CodeAsymmetricKeyInvalid, "stored public key unparseable", err)
		}
		pub, ok := parsed.(*rsa.PublicKey)
		if !ok {
			return newError(CodeAsymmetricKeyInvalid, "stored key is not RSA")
		}
		digest := sha256.Sum256(msg)
		switch key.Algorithm {
		case AlgRS256:
			if rsa.VerifyPKCS1v15(pub, crypto.SHA256, digest[:], sig) != nil {
				return newError(CodeAsymmetricKeyInvalid, "RS256 signature invalid")
			}
		case AlgPS256:
			if rsa.VerifyPSS(pub, crypto.SHA256, digest[:], sig, nil) != nil {
				return newError(CodeAsymmetricKeyInvalid, "PS256 signature invalid")
			}
		default:
			return newError(CodeAsymmetricKeyInvalid, "unsupported RSA algorithm")
		}
		return nil
	default:
		return newError(CodeAuthInternal, "unknown key type")
	}
}
