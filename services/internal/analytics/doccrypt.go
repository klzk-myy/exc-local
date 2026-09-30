package analytics

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base32"
	"fmt"

	"exchange/internal/pdfsec"
)

// DocCipher derives per-account document PINs and encrypts the
// client-facing PDFs this package generates (spec §16 + Phase-20 Task
// 20.3.8 DoD: "email with encrypted PDF"; spec §5.28 marks the archived
// file location "S3 / encrypted storage" — encrypting at render time
// satisfies both, since the S3 object and the email attachment are the
// same ciphertext).
//
// The user password is a deterministic per-account PIN:
//
//	base32lower(HMAC-SHA256(secret, "docpin:"+accountID))[:10]
//
// surfaced to the authenticated account owner through the statements
// list envelope. The owner password is per-document
// (HMAC(secret,"docown:"+fileRef)) and never leaves the platform.
//
// A nil/empty-secret DocCipher is fail-closed: RenderPDF returns an
// error rather than emitting plaintext client documents.
type DocCipher struct {
	secret []byte
}

// NewDocCipher returns nil when secret is empty so services can treat
// an unconfigured deployment as fail-closed at generate time.
func NewDocCipher(secret []byte) *DocCipher {
	if len(secret) == 0 {
		return nil
	}
	return &DocCipher{secret: append([]byte(nil), secret...)}
}

// PIN returns the document-open password for one account — stable
// across documents so the portal can display it once as the account's
// document PIN.
func (c *DocCipher) PIN(accountID int64) string {
	if c == nil {
		return ""
	}
	m := hmac.New(sha256.New, c.secret)
	fmt.Fprintf(m, "docpin:%d", accountID)
	return base32.StdEncoding.WithPadding(base32.NoPadding).
		EncodeToString(m.Sum(nil))[:10]
}

func (c *DocCipher) ownerKey(fileRef string) string {
	m := hmac.New(sha256.New, c.secret)
	fmt.Fprintf(m, "docown:%s", fileRef)
	return base32.StdEncoding.WithPadding(base32.NoPadding).
		EncodeToString(m.Sum(nil))[:16]
}

// RenderPDF renders lines through renderTextPDF and AES-128-encrypts
// the result (pdfsec, V4/R4/AESV2). Returns an error when the cipher is
// unconfigured — callers must never fall back to plaintext for
// client-facing documents.
func (c *DocCipher) RenderPDF(title string, lines []docLine, accountID int64, fileRef string) ([]byte, error) {
	if c == nil {
		return nil, fmt.Errorf("document encryption not configured (EXC_DOCS_SECRET)")
	}
	return pdfsec.EncryptPDF(renderTextPDF(title, lines), c.PIN(accountID), c.ownerKey(fileRef))
}

// Encrypt applies standard-handler encryption to an already-rendered
// PDF at the storage boundary. Returns an error when the cipher is
// unconfigured — callers must never fall back to plaintext for
// client-facing documents.
func (c *DocCipher) Encrypt(pdf []byte, accountID int64, fileRef string) ([]byte, error) {
	if c == nil {
		return nil, fmt.Errorf("document encryption not configured (EXC_DOCS_SECRET)")
	}
	return pdfsec.EncryptPDF(pdf, c.PIN(accountID), c.ownerKey(fileRef))
}
