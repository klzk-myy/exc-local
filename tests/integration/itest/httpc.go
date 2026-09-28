// HTTP client for the live-stack legs: the exact §8.7/§8.8 signing
// surface the gateway verifies —
//
//	X-API-KEY    api_keys.key_id
//	X-TIMESTAMP  unix seconds, |now-ts| <= 30s
//	X-SIGNATURE  base64url(HMAC-SHA256(secret, ts\nMETHOD\npath?query\nsha256hex(body)))
//
// plus HS256 access-JWT minting for the admin/RBAC surface (the gateway
// verifies kid-pinned "v1" keys seeded via EXC_JWT_HS256_KEY_B64).
package itest

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// APIKey is a seeded HMAC api_keys row (raw secret held only here).
type APIKey struct {
	KeyID   string
	Secret  string // raw HMAC secret — the b64url string form the store seals
	Account int64
	User    int64
	Scopes  []string
}

// Sign mints the X-* signature headers for one request.
func (k *APIKey) Sign(method, pathQuery string, body []byte) (map[string]string, error) {
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	bodyHash := sha256.Sum256(body)
	canonical := ts + "\n" + strings.ToUpper(method) + "\n" + pathQuery + "\n" + hex.EncodeToString(bodyHash[:])
	mac := hmac.New(sha256.New, []byte(k.Secret))
	mac.Write([]byte(canonical))
	return map[string]string{
		"X-API-KEY":   k.KeyID,
		"X-TIMESTAMP": ts,
		"X-SIGNATURE": base64.RawURLEncoding.EncodeToString(mac.Sum(nil)),
	}, nil
}

// SignAt mints headers with an explicit timestamp (window/replay tests).
func (k *APIKey) SignAt(ts int64, method, pathQuery string, body []byte) map[string]string {
	tss := strconv.FormatInt(ts, 10)
	bodyHash := sha256.Sum256(body)
	canonical := tss + "\n" + strings.ToUpper(method) + "\n" + pathQuery + "\n" + hex.EncodeToString(bodyHash[:])
	mac := hmac.New(sha256.New, []byte(k.Secret))
	mac.Write([]byte(canonical))
	return map[string]string{
		"X-API-KEY":   k.KeyID,
		"X-TIMESTAMP": tss,
		"X-SIGNATURE": base64.RawURLEncoding.EncodeToString(mac.Sum(nil)),
	}
}

// ---------------------------------------------------------------------------
// JWT (admin/RBAC surface) — mirrors internal/auth/jwt.go wire shape.
// ---------------------------------------------------------------------------

// MintJWT builds an HS256 access token the gateway's kid="v1" keyring
// accepts (iss exc.local / aud exc-api / typ access).
func MintJWT(keyB64, subject string, accountID int64, scopes []string) (string, error) {
	kb, err := base64.StdEncoding.DecodeString(strings.TrimSpace(keyB64))
	if err != nil {
		return "", err
	}
	now := time.Now()
	hdr, _ := json.Marshal(map[string]any{"alg": "HS256", "kid": "v1", "typ": "JWT"})
	// NumericDate claims marshal as unix seconds (jwt.RegisteredClaims
	// convention); the gateway rejects millisecond epochs.
	claims, _ := json.Marshal(map[string]any{
		"iss": "exc.local", "aud": "exc-api", "sub": subject,
		"iat": now.Unix(), "nbf": now.Unix(), "exp": now.Add(15 * time.Minute).Unix(),
		"jti": fmt.Sprintf("itest-%d", now.UnixNano()),
		"typ": "access", "account_id": accountID, "scopes": scopes,
	})
	segs := base64.RawURLEncoding.EncodeToString(hdr) + "." + base64.RawURLEncoding.EncodeToString(claims)
	mac := hmac.New(sha256.New, kb)
	mac.Write([]byte(segs))
	return segs + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), nil
}

// ---------------------------------------------------------------------------
// api_keys secret sealing — mirrors internal/auth/secrets.go:
// secret_enc = nonce(12B) || AES-256-GCM(dataKey, nonce, secret).
// ---------------------------------------------------------------------------

// SealSecret encrypts an HMAC secret exactly as auth.SecretBox.Seal does,
// so tests can seed api_keys rows the gateway can unwrap.
func SealSecret(dataKey []byte, plaintext []byte) ([]byte, error) {
	if len(dataKey) != 32 {
		return nil, fmt.Errorf("data key must be 32 bytes")
	}
	blk, err := aes.NewCipher(dataKey)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(blk)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return aead.Seal(nonce, nonce, plaintext, nil), nil
}

// SecretKeyHash is the api_keys.key_hash fingerprint: hex(sha256(secret)).
func SecretKeyHash(secret string) string {
	fp := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(fp[:])
}

// ---------------------------------------------------------------------------
// HTTP helpers
// ---------------------------------------------------------------------------

// Client wraps an http.Client bound to the stack base URL.
type Client struct {
	Base   string
	HTTP   *http.Client
	Tracer string // optional X-Forwarded-For identity override
}

// NewClient builds a client; xff is sent as X-Forwarded-For on every
// request (TrustProxy=true in the gateway — lets per-IP ban/ratelimit
// legs use isolated identities without poisoning 127.0.0.1).
func NewClient(base, xff string) *Client {
	return &Client{Base: strings.TrimRight(base, "/"), HTTP: &http.Client{Timeout: 15 * time.Second}, Tracer: xff}
}

// Do issues one request; headers may include a signer-produced map.
func (c *Client) Do(ctx context.Context, method, pathQuery string, body []byte, headers map[string]string) (*http.Response, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.Base+pathQuery, bytes.NewReader(body))
	if err != nil {
		return nil, nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.Tracer != "" {
		req.Header.Set("X-Forwarded-For", c.Tracer)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, b, nil
}

// Signed performs an HMAC-signed request for key. A nonce query
// parameter makes every signature unique: the gateway's replay cache
// keys on key_id|timestamp|signature, so two identical calls inside one
// timestamp second would otherwise read as REPLAY_ATTACK_DETECTED.
// Handlers ignore the extra param (query parsers read named keys only).
func (c *Client) Signed(ctx context.Context, k *APIKey, method, pathQuery string, body []byte) (*http.Response, []byte, error) {
	sep := "?"
	if strings.Contains(pathQuery, "?") {
		sep = "&"
	}
	pq := pathQuery + sep + "_sn=" + strconv.FormatInt(time.Now().UnixNano(), 36)
	h, err := k.Sign(method, pq, body)
	if err != nil {
		return nil, nil, err
	}
	return c.Do(ctx, method, pq, body, h)
}

// SignedH is Signed plus caller-supplied headers (e.g. Idempotency-Key on
// the money-moving endpoints where the middleware requires a UUIDv7).
func (c *Client) SignedH(ctx context.Context, k *APIKey, method, pathQuery string,
	body []byte, extra map[string]string) (*http.Response, []byte, error) {
	sep := "?"
	if strings.Contains(pathQuery, "?") {
		sep = "&"
	}
	pq := pathQuery + sep + "_sn=" + strconv.FormatInt(time.Now().UnixNano(), 36)
	h, err := k.Sign(method, pq, body)
	if err != nil {
		return nil, nil, err
	}
	for k2, v := range extra {
		h[k2] = v
	}
	return c.Do(ctx, method, pq, body, h)
}

// BearerH is Bearer plus caller-supplied headers.
func (c *Client) BearerH(ctx context.Context, tok, method, pathQuery string,
	body []byte, extra map[string]string) (*http.Response, []byte, error) {
	h := map[string]string{"Authorization": "Bearer " + tok}
	for k, v := range extra {
		h[k] = v
	}
	return c.Do(ctx, method, pathQuery, body, h)
}

// NewIdemKey mints a UUIDv7 (48-bit unix-ms prefix, version 7, RFC
// variant) — the shape middleware.IsUUIDv7 accepts on the
// Idempotency-Key header.
func NewIdemKey() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	ms := uint64(time.Now().UnixMilli())
	b[0], b[1], b[2] = byte(ms>>40), byte(ms>>32), byte(ms>>24)
	b[3], b[4], b[5] = byte(ms>>16), byte(ms>>8), byte(ms)
	b[6] = (b[6] & 0x0f) | 0x70 // version 7
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10xx
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// Bearer performs a JWT-authenticated request.
func (c *Client) Bearer(ctx context.Context, tok, method, pathQuery string, body []byte) (*http.Response, []byte, error) {
	return c.Do(ctx, method, pathQuery, body, map[string]string{"Authorization": "Bearer " + tok})
}

// Envelope is the RFC 7807 error envelope shape (Task 5.3.21).
type Envelope struct {
	Type      string `json:"type"`
	Error     string `json:"error"`
	Title     string `json:"title"`
	Message   string `json:"message"`
	Status    int    `json:"status"`
	RequestID string `json:"request_id"`
}

// DecodeEnvelope parses the error envelope; returns false when absent.
func DecodeEnvelope(b []byte) (Envelope, bool) {
	var e Envelope
	if err := json.Unmarshal(b, &e); err != nil {
		return e, false
	}
	return e, e.Error != "" || e.Type != "" || e.Title != ""
}
