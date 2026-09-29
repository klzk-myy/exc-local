// Upload pipeline — virus-scan seam, document validation and the
// S3-backed submit flow for POST /api/v1/kyc/submit (Task 12.3.4).
//
// VirusScanner is deliberately an interface: the development wiring is
// CleanPassScanner (logs + passes — it does NOT claim ClamAV). A
// production deployment injects a real engine (e.g. a clamav TCP client);
// every scan error fails the submission closed.
//
// Encrypted-chunk resume: the desktop-upload retry contract is idempotent
// re-submission — a retried POST creates a new submission row reusing
// document bytes the client re-sends. Chunk assembly is modelled by
// ChunkManifest/ChunkStore below as the honest seam for a future
// multipart-session API; nothing here pretends a chunk server exists.
package compliance

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"path"
	"strings"
	"time"

	"exchange/internal/objectstore"
)

// ---------------------------------------------------------------------------
// VirusScanner — dev clean-pass seam, production engine behind injection
// ---------------------------------------------------------------------------

// VirusScanner inspects one buffered document. Returning ErrInfected (or
// wrapping it) rejects the file; any other error also fails the
// submission (fail-closed — an unavailable scanner is never a pass).
type VirusScanner interface {
	Scan(ctx context.Context, docType string, data []byte) error
}

// ScanLogger is the minimal logging seam (slog-compatible signature).
type ScanLogger func(format string, args ...any)

// CleanPassScanner is the development implementation: it logs the
// document's type/size/digest and returns clean. It performs NO malware
// detection — production must inject a real engine (ClamAV etc.) or
// uploads remain logged-and-unscanned, which is surfaced in wiring logs.
type CleanPassScanner struct{ Log ScanLogger }

// Scan implements VirusScanner.
func (c CleanPassScanner) Scan(ctx context.Context, docType string, data []byte) error {
	if c.Log != nil {
		sum := sha256.Sum256(data)
		c.Log("kyc virus scan clean-pass (dev seam, no engine) doc=%s bytes=%d sha256=%s",
			docType, len(data), hex.EncodeToString(sum[:])[:16])
	}
	return nil
}

// ---------------------------------------------------------------------------
// Document validation
// ---------------------------------------------------------------------------

// Upload size contract (multipart handler enforces total too):
const (
	MaxDocBytes   = 10 << 20 // 10 MiB per document
	MaxTotalBytes = 32 << 20 // 32 MiB per submission
)

// allowedMIME is the safe upload set — PDF + raster images only. The
// sniffed type (http.DetectContentType on the first 512 bytes) AND the
// declared part Content-Type must both land here.
var allowedMIME = map[string]string{
	"application/pdf": ".pdf",
	"image/jpeg":      ".jpg",
	"image/png":       ".png",
}

// validateDoc enforces the per-file contract: non-empty, within size
// cap, sniffed+declared MIME in the allowlist, and document_type present
// in the tier's matrix rows (exact-jurisdiction overlay included).
func validateDoc(d UploadDoc, rows []MatrixRow) (detectedMIME string, err error) {
	d.Type = strings.ToUpper(strings.TrimSpace(d.Type))
	if d.Type == "" {
		return "", fmt.Errorf("%w: document type required", ErrValidation)
	}
	if len(d.Data) == 0 {
		return "", fmt.Errorf("%w: %s: empty file", ErrValidation, d.Type)
	}
	if len(d.Data) > MaxDocBytes {
		return "", fmt.Errorf("%w: %s: exceeds %d-byte cap", ErrValidation, d.Type, MaxDocBytes)
	}
	known := false
	for _, r := range rows {
		if r.DocumentType == d.Type {
			known = true
			break
		}
	}
	if !known {
		return "", fmt.Errorf("%w: document_type %q is not in the requirements matrix for this tier/jurisdiction",
			ErrValidation, d.Type)
	}
	sniffed := http.DetectContentType(d.Data)
	declared := strings.TrimSpace(strings.ToLower(d.ContentType))
	if _, ok := allowedMIME[sniffed]; !ok {
		return "", fmt.Errorf("%w: %s: content looks like %q — only PDF/JPEG/PNG accepted",
			ErrValidation, d.Type, sniffed)
	}
	// Declared type is advisory — the sniffed magic bytes are
	// authoritative. application/octet-stream means "client didn't
	// know" (multipart writers default to it), so it passes through to
	// the sniff check; anything else must be in the allowlist.
	if declared != "" && declared != "application/octet-stream" {
		if _, ok := allowedMIME[declared]; !ok {
			return "", fmt.Errorf("%w: %s: content-type %q not allowed (PDF/JPEG/PNG only)",
				ErrValidation, d.Type, declared)
		}
	}
	return sniffed, nil
}

// requiredFileGroups returns the doc_groups that must be satisfied by an
// uploaded file. TAX is intentionally excluded — tax self-certifications
// are structured records filed via the self-cert endpoint (they can only
// exist once the account reaches T1, so they cannot gate the very
// submission that requests T1); reviewers see the gap via Requirements().
// LIVENESS is a file group here (selfie upload); the actual biometric
// match is a provider-side check (Phase-14 seam).
func requiredFileGroups(rows []MatrixRow) map[string]bool {
	types := map[string]string{} // doc_type → group
	for _, r := range rows {
		types[r.DocumentType] = r.DocGroup
	}
	groups := map[string]bool{}
	for _, r := range rows {
		if r.Required && r.DocGroup != "TAX" {
			groups[r.DocGroup] = true
		}
	}
	return groups
}

// docGroup maps a document_type back to its matrix group.
func docGroup(rows []MatrixRow, docType string) string {
	for _, r := range rows {
		if r.DocumentType == docType {
			return r.DocGroup
		}
	}
	return ""
}

// ---------------------------------------------------------------------------
// Submit — the intake pipeline
// ---------------------------------------------------------------------------

// SubmitInput carries one submission request.
type SubmitInput struct {
	AccountID     int64
	RequestedTier string
	Jurisdiction  string // ISO alpha-2, optional ('' = matrix default '*')
	Documents     []UploadDoc
}

// SubmitResult returns the recorded submission + document ids.
type SubmitResult struct {
	Submission *Submission `json:"submission"`
	Documents  []Document  `json:"documents"`
}

// Submit validates, scans, stores and records one KYC submission.
//
// Ordering is deliberate: the submission row lands first (PENDING_REVIEW
// with sla_due_at = submitted_at + policy SLA), objects are then Put with
// SSE-KMS, then document rows attach. A failure mid-flight marks the
// submission FAILED and best-effort deletes already-written objects —
// a half-intake never reads as pending.
func (s *Service) Submit(ctx context.Context, in SubmitInput) (*SubmitResult, error) {
	tier := strings.ToUpper(strings.TrimSpace(in.RequestedTier))
	if !RequestableTier(tier) {
		return nil, fmt.Errorf("%w: requested_tier must be T1|T2|INSTITUTIONAL", ErrValidation)
	}
	jur := strings.ToUpper(strings.TrimSpace(in.Jurisdiction))
	if jur != "" && len(jur) != 2 {
		return nil, fmt.Errorf("%w: jurisdiction must be an ISO 3166-1 alpha-2 code", ErrValidation)
	}
	if len(in.Documents) == 0 {
		return nil, fmt.Errorf("%w: at least one document required", ErrValidation)
	}
	var total int64
	for _, d := range in.Documents {
		total += int64(len(d.Data))
	}
	if total > MaxTotalBytes {
		return nil, fmt.Errorf("%w: submission exceeds %d-byte total cap", ErrValidation, MaxTotalBytes)
	}
	if s.objects == nil {
		return nil, ErrObjectsUnset
	}

	pol, err := s.store.TierPolicy(ctx, tier)
	if err != nil {
		return nil, err
	}
	if pol == nil {
		return nil, fmt.Errorf("%w: no tier policy for %q", ErrValidation, tier)
	}
	rows, err := s.store.Matrix(ctx, tier, jur)
	if err != nil {
		return nil, err
	}

	// Validate every file first — no partial intake on bad input.
	sniffed := make([]string, len(in.Documents))
	for i := range in.Documents {
		in.Documents[i].Type = strings.ToUpper(strings.TrimSpace(in.Documents[i].Type))
		if sniffed[i], err = validateDoc(in.Documents[i], rows); err != nil {
			return nil, err
		}
	}
	// Required-group completeness: every required file group needs ≥1 doc.
	covered := map[string]bool{}
	for _, d := range in.Documents {
		covered[docGroup(rows, d.Type)] = true
	}
	for g := range requiredFileGroups(rows) {
		if !covered[g] {
			return nil, fmt.Errorf("%w: required document group %s not satisfied", ErrValidation, g)
		}
	}
	// Virus scan — infection is a client fault (ErrInfected → 400);
	// an engine failure fails closed (ErrScanner → 503).
	for _, d := range in.Documents {
		if err := s.scanner.Scan(ctx, d.Type, d.Data); err != nil {
			if errors.Is(err, ErrInfected) {
				return nil, fmt.Errorf("%w: %s", ErrInfected, d.Type)
			}
			return nil, fmt.Errorf("%w: %s", ErrScanner, err)
		}
	}

	now := s.now().UTC()
	sub := &Submission{
		AccountID:     in.AccountID,
		RequestedTier: tier,
		Status:        SubPendingReview,
		Jurisdiction:  jur,
		RiskScore:     0, // provider-side scoring lands with the vendor seam
		SubmittedAt:   now,
		SLADueAt:      now.Add(time.Duration(pol.ManualReviewSLAHours) * time.Hour),
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	if err := s.store.CreateSubmission(ctx, sub); err != nil {
		return nil, err
	}

	uploaded := []string{} // object keys written — cleaned up on failure
	res := &SubmitResult{Submission: sub}
	fail := func(cause error) (*SubmitResult, error) {
		_ = s.store.FailSubmission(ctx, sub.ID)
		for _, k := range uploaded {
			_ = s.objects.Delete(ctx, k)
		}
		return nil, cause
	}

	for i, d := range in.Documents {
		sum := sha256.Sum256(d.Data)
		digest := hex.EncodeToString(sum[:])
		ext := allowedMIME[sniffed[i]]
		key := fmt.Sprintf("kyc/%d/%d/%s-%s%s",
			in.AccountID, sub.ID, strings.ToLower(d.Type), digest[:16], ext)
		if _, err := s.objects.Put(ctx, objectstore.PutInput{
			Key:         key,
			Body:        bytes.NewReader(d.Data),
			Size:        int64(len(d.Data)),
			ContentType: sniffed[i],
			// spec §24 #102 — SSE-KMS on every KYC document object.
			ServerSideEncryption: "aws:kms",
			SSEKMSKeyID:          s.kmsKeyID,
			Metadata: map[string]string{
				"account-id":     fmt.Sprintf("%d", in.AccountID),
				"submission-id":  fmt.Sprintf("%d", sub.ID),
				"document-type":  d.Type,
				"document-group": docGroup(rows, d.Type),
				"filename":       path.Base(d.Filename),
			},
		}); err != nil {
			return fail(fmt.Errorf("compliance: object store put: %w", err))
		}
		uploaded = append(uploaded, key)
		doc := &Document{
			AccountID:    in.AccountID,
			SubmissionID: sub.ID,
			Type:         d.Type,
			ObjectKey:    key,
			Status:       "PENDING",
			SHA256:       digest,
			SizeBytes:    int64(len(d.Data)),
			SSEAlgorithm: "aws:kms",
			CreatedAt:    now,
		}
		if err := s.store.AttachDocument(ctx, doc); err != nil {
			return fail(err)
		}
		doc.ObjectKey = "" // never leak keys through the API path
		res.Documents = append(res.Documents, *doc)
	}
	return res, nil
}

// ---------------------------------------------------------------------------
// Chunk/resume seam (Task 12.3.13 desktop retry + encrypted chunks)
// ---------------------------------------------------------------------------

// ChunkManifest describes one resumable upload session's wire shape —
// desktop clients retry by re-POSTing the manifest; a future multipart
// endpoint consumes this. Stored nowhere in Phase-12: the retry contract
// today is idempotent whole-submission re-POST (a new kyc_submissions
// row replaces the failed one — see Submit's cleanup).
type ChunkManifest struct {
	DocumentType string `json:"document_type"`
	TotalSize    int64  `json:"total_size"`
	SHA256       string `json:"sha256"` // full-object digest — integrity across chunk retries
	ChunkSize    int64  `json:"chunk_size"`
	Chunks       int    `json:"chunks"`
}
