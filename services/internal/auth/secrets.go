// Secret material helpers for the auth cluster (Tasks 5.3.1, 5.3.38).
//
// randomToken mints opaque bearer strings (refresh tokens, jti, key ids).
// wrapSecret/unwrapSecret provide AES-256-GCM envelope encryption for the
// only secret material the platform must recover — HMAC shared secrets
// (api_keys.secret_enc) — since an HMAC verifier needs the raw key. The
// data key comes from configuration (Vault/KMS in production per
// Phase-13.5 Task 13.5.3.5); a missing key fails closed at construction.
package auth

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"fmt"
)

// randRead is the indirection over crypto/rand used by tests.
var randRead = rand.Read

// b64urlEncode encodes without padding — the bearer-token alphabet used
// for refresh tokens, key ids and session ids.
func b64urlEncode(b []byte) string {
	return base64.RawURLEncoding.EncodeToString(b)
}

// randomToken returns a URL-safe bearer string of n random bytes.
func randomToken(n int) (string, error) {
	b := make([]byte, n)
	if _, err := randRead(b); err != nil {
		return "", fmt.Errorf("rand: %w", err)
	}
	return b64urlEncode(b), nil
}

// SecretBox wraps/unwraps small secrets under a 32-byte AES-256-GCM data
// key. Output is nonce||ciphertext (nonce prepended, GCM tag appended).
type SecretBox struct {
	aead cipher.AEAD
}

// NewSecretBox requires exactly 32 bytes of key material.
func NewSecretBox(key []byte) (*SecretBox, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("secret box key must be 32 bytes, got %d", len(key))
	}
	aead, err := cipher.NewGCM(mustAES(key))
	if err != nil {
		return nil, fmt.Errorf("gcm: %w", err)
	}
	return &SecretBox{aead: aead}, nil
}

func mustAES(key []byte) cipher.Block {
	b, err := aes.NewCipher(key)
	if err != nil {
		panic(err) // unreachable: key length checked by caller
	}
	return b
}

// Seal encrypts plaintext; output is nonce||ciphertext.
func (s *SecretBox) Seal(plaintext []byte) ([]byte, error) {
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := randRead(nonce); err != nil {
		return nil, fmt.Errorf("nonce: %w", err)
	}
	return s.aead.Seal(nonce, nonce, plaintext, nil), nil
}

// Open decrypts nonce||ciphertext. Any tamper is an error.
func (s *SecretBox) Open(blob []byte) ([]byte, error) {
	if len(blob) < s.aead.NonceSize()+s.aead.Overhead() {
		return nil, fmt.Errorf("sealed blob too short")
	}
	nonce, ct := blob[:s.aead.NonceSize()], blob[s.aead.NonceSize():]
	pt, err := s.aead.Open(nil, nonce, ct, nil)
	if err != nil {
		return nil, fmt.Errorf("unseal: %w", err)
	}
	return pt, nil
}
