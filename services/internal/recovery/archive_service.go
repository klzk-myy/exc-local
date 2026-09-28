// archive_service.go — Task 4.3.2: WAL S3 archive with upload confirmation
// before local trim, archive index, and 90-day→Glacier retention modelling.
//
// Layout (spec): s3://exchange-wal/{shard}/{date}/{segment}
// Index:         s3://exchange-wal/{shard}/index.json
//
// Zero-loss guard (spec §3.5/§24): ArchiveSegment verifies the returned
// ETag equals md5(local bytes) before the caller is allowed to trim the
// local file; TrimSegment deletes only after that confirmation. Index
// update happens before local delete as well, so a crash mid-pipeline can
// leave a segment uploaded+indexed but still local — replay prefers S3 and
// the operator can re-run; it can never leave the only copy deleted.
//
// Retention modelling: real S3 lifecycle rules (90 days → GLACIER) cannot
// be exercised against the dev stub, so the service (a) publishes a
// lifecycle.json policy document next to the index describing the rule
// applied to the bucket, and (b) carries per-segment storage_class /
// glacier_transition_at state in index.json, flipped by ApplyLifecycle —
// the same state a real transition would produce for observability.
package recovery

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"exchange/internal/objectstore"
)

// DefaultWalBucket is the spec-named archive bucket.
const DefaultWalBucket = "exchange-wal"

// Lifecycle rule: STANDARD → GLACIER after 90 days (spec/Task 4.3.2 #4).
const DefaultRetentionDays = 90

// SegmentMeta is one index.json row.
type SegmentMeta struct {
	Segment    string `json:"segment"` // local filename, e.g. 00000000000001048576.wal
	Key        string `json:"key"`     // full object key {shard}/{date}/{segment}
	Shard      uint16 `json:"shard"`
	Date       string `json:"date"` // UTC date of first entry (or upload date)
	SizeBytes  int64  `json:"size_bytes"`
	SHA256     string `json:"sha256"` // hex sha256 of segment bytes
	ETag       string `json:"etag"`   // S3 ETag = hex md5 (verified at upload)
	FirstSeq   uint64 `json:"first_seq"`
	LastSeq    uint64 `json:"last_seq"`
	EntryCount uint64 `json:"entry_count"`
	Pads       uint64 `json:"pads,omitempty"`
	Corrupt    bool   `json:"corrupt"`     // scan detected torn/garbage tail
	ArchivedAt string `json:"archived_at"` // RFC3339 UTC
	// StorageClass is STANDARD while hot and GLACIER once ApplyLifecycle
	// has moved the segment past retention. GlacierTransitionAt records
	// when the transition was applied (modelled; see package doc).
	StorageClass        string `json:"storage_class"`
	GlacierTransitionAt string `json:"glacier_transition_at,omitempty"`
}

// ArchiveIndex is s3://{bucket}/{shard}/index.json.
type ArchiveIndex struct {
	Shard     uint16        `json:"shard"`
	Bucket    string        `json:"bucket"`
	UpdatedAt string        `json:"updated_at"`
	Segments  []SegmentMeta `json:"segments"`
}

// IndexKey is the canonical index object key for a shard.
func IndexKey(shard uint16) string { return fmt.Sprintf("%d/index.json", shard) }

// ArchiveService owns WAL-segment archiving for one bucket.
type ArchiveService struct {
	store         objectstore.Client
	retentionDays int
	now           func() time.Time
}

// NewArchiveService binds the service to a bucket client.
func NewArchiveService(store objectstore.Client) *ArchiveService {
	return &ArchiveService{
		store:         store,
		retentionDays: DefaultRetentionDays,
		now:           func() time.Time { return time.Now().UTC() },
	}
}

// SetRetentionDays overrides the 90-day lifecycle window (tests).
func (s *ArchiveService) SetRetentionDays(d int) { s.retentionDays = d }

// SetClock overrides the clock (tests).
func (s *ArchiveService) SetClock(f func() time.Time) { s.now = f }

// ErrUploadNotConfirmed is returned when S3 does not confirm the upload
// with a matching ETag — the local segment MUST NOT be trimmed.
var ErrUploadNotConfirmed = errors.New("archive: S3 upload not confirmed (ETag mismatch)")

// segmentDate derives the archive date dir from the first entry's
// timestamp; falls back to fallback for empty segments.
func segmentDate(entries []Entry, fallback time.Time) string {
	if len(entries) > 0 && entries[0].TimestampNs > 0 {
		return time.Unix(0, int64(entries[0].TimestampNs)).UTC().Format("2006-01-02")
	}
	return fallback.UTC().Format("2006-01-02")
}

// ObjectKey builds {shard}/{date}/{segment}.
func ObjectKey(shard uint16, date, segment string) string {
	return fmt.Sprintf("%d/%s/%s", shard, date, segment)
}

// ArchiveSegment scans, uploads, verifies, and indexes one segment.
// data is the complete segment image. Returns the index entry on success;
// on any failure the segment is left fully local (caller decides trim).
func (s *ArchiveService) ArchiveSegment(ctx context.Context, shard uint16,
	segName string, data []byte, dateFallback time.Time) (*SegmentMeta, error) {

	if segName == "" || strings.Contains(segName, "/") {
		return nil, fmt.Errorf("archive: bad segment name %q", segName)
	}
	scan, entries, err := ScanSegment(data)
	if err != nil {
		return nil, fmt.Errorf("archive: scan %s: %w", segName, err)
	}

	meta := &SegmentMeta{
		Segment:   segName,
		Shard:     shard,
		Date:      segmentDate(entries, dateFallback),
		SizeBytes: int64(len(data)),
		Corrupt:   scan.Corrupt,
	}
	meta.SHA256 = hexSHA256(data)
	meta.EntryCount = scan.Entries
	meta.LastSeq = scan.LastSeq
	meta.Pads = scan.Pads
	if len(entries) > 0 {
		meta.FirstSeq = entries[0].Seq
	}
	meta.Key = ObjectKey(shard, meta.Date, segName)
	meta.ArchivedAt = s.now().Format(time.RFC3339)
	meta.StorageClass = "STANDARD"

	obj, err := s.store.Put(ctx, objectstore.PutInput{
		Key:  meta.Key,
		Body: bytes.NewReader(data),
		Size: int64(len(data)),
		Metadata: map[string]string{
			"shard":     fmt.Sprint(shard),
			"segment":   segName,
			"sha256":    meta.SHA256,
			"first-seq": fmt.Sprint(meta.FirstSeq),
			"last-seq":  fmt.Sprint(scan.LastSeq),
			"corrupt":   fmt.Sprint(scan.Corrupt),
			"scanner":   "exchange-wal-archive",
		},
	})
	if err != nil {
		return nil, err
	}

	// Upload confirmation: the returned ETag must equal md5 of the exact
	// bytes we sent. Anything else means partial/corrupt storage — the
	// local file stays put and the caller must NOT trim.
	want := hexMD5(data)
	if obj.ETag == "" || !strings.EqualFold(obj.ETag, want) {
		return nil, fmt.Errorf("archive: %s: %w (etag=%q want=%q)",
			meta.Key, ErrUploadNotConfirmed, obj.ETag, want)
	}
	meta.ETag = obj.ETag

	// Index AFTER upload confirmation, still before any local trim.
	if err := s.appendIndex(ctx, shard, *meta); err != nil {
		return nil, err
	}
	return meta, nil
}

// TrimSegment archives a segment file then deletes it locally — the
// only sanctioned local-trim path (zero-loss guard).
func (s *ArchiveService) TrimSegment(ctx context.Context, shard uint16,
	path string) (*SegmentMeta, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("archive: read %s: %w", path, err)
	}
	st, _ := os.Stat(path)
	fallback := s.now()
	if st != nil {
		fallback = st.ModTime()
	}
	meta, err := s.ArchiveSegment(ctx, shard, filepath.Base(path), data, fallback)
	if err != nil {
		return nil, err // local file untouched — fail closed
	}
	if err := os.Remove(path); err != nil {
		return nil, fmt.Errorf("archive: uploaded+indexed but local trim failed for %s: %w", path, err)
	}
	return meta, nil
}

// ArchiveDir archives every sealed segment in dir: all *.wal files except
// the lexically greatest (the segment the engine may still be appending
// to). Returns per-segment results; the first failure aborts (fail closed)
// with already-archived segments listed in the result.
func (s *ArchiveService) ArchiveDir(ctx context.Context, walDir string,
	shard uint16) ([]SegmentMeta, error) {
	ents, err := os.ReadDir(walDir)
	if err != nil {
		return nil, fmt.Errorf("archive: read dir %s: %w", walDir, err)
	}
	var segs []string
	for _, e := range ents {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".wal") {
			segs = append(segs, e.Name())
		}
	}
	sort.Strings(segs)
	if len(segs) == 0 {
		return nil, nil
	}
	sealed := segs[:len(segs)-1] // newest stays local: it may be live
	var out []SegmentMeta
	for _, name := range sealed {
		m, err := s.TrimSegment(ctx, shard, filepath.Join(walDir, name))
		if err != nil {
			return out, err
		}
		out = append(out, *m)
	}
	return out, nil
}

// --- index --------------------------------------------------------------

// LoadIndex reads the archive index for a shard. A missing index is not an
// error (fresh shard); a corrupt one IS (fail closed — we cannot tell what
// is archived).
func (s *ArchiveService) LoadIndex(ctx context.Context, shard uint16) (*ArchiveIndex, error) {
	data, _, err := s.store.Get(ctx, IndexKey(shard))
	if err != nil {
		if objectstore.IsNotFound(err) {
			return &ArchiveIndex{Shard: shard, Bucket: s.store.Bucket()}, nil
		}
		return nil, err
	}
	var idx ArchiveIndex
	if err := json.Unmarshal(data, &idx); err != nil {
		return nil, fmt.Errorf("archive: index.json for shard %d corrupt: %w", shard, err)
	}
	return &idx, nil
}

func (s *ArchiveService) putIndex(ctx context.Context, idx *ArchiveIndex) error {
	idx.UpdatedAt = s.now().Format(time.RFC3339)
	idx.Bucket = s.store.Bucket()
	raw, err := json.MarshalIndent(idx, "", "  ")
	if err != nil {
		return err
	}
	_, err = s.store.Put(ctx, objectstore.PutInput{
		Key:         IndexKey(idx.Shard),
		Body:        strings.NewReader(string(raw)),
		Size:        int64(len(raw)),
		ContentType: "application/json",
	})
	return err
}

// appendIndex read-modify-writes index.json. Single-writer assumption: the
// WAL archiver for a shard is a singleton (leader-side job), matching the
// single-writer WAL itself.
func (s *ArchiveService) appendIndex(ctx context.Context, shard uint16, meta SegmentMeta) error {
	idx, err := s.LoadIndex(ctx, shard)
	if err != nil {
		return err
	}
	// Idempotent re-archive: replace an existing row for the same segment.
	replaced := false
	for i := range idx.Segments {
		if idx.Segments[i].Segment == meta.Segment {
			idx.Segments[i] = meta
			replaced = true
			break
		}
	}
	if !replaced {
		idx.Segments = append(idx.Segments, meta)
	}
	sort.Slice(idx.Segments, func(i, j int) bool {
		return idx.Segments[i].Segment < idx.Segments[j].Segment
	})
	return s.putIndex(ctx, idx)
}

// ApplyLifecycle flips segments whose archive date is older than the
// retention window from STANDARD to GLACIER in the index — the modelled
// half of the bucket lifecycle policy — and (re)publishes lifecycle.json
// describing the rule. Returns the number newly transitioned.
func (s *ArchiveService) ApplyLifecycle(ctx context.Context, shard uint16) (int, error) {
	idx, err := s.LoadIndex(ctx, shard)
	if err != nil {
		return 0, err
	}
	cutoff := s.now().AddDate(0, 0, -s.retentionDays)
	n := 0
	for i := range idx.Segments {
		m := &idx.Segments[i]
		if m.StorageClass == "GLACIER" {
			continue
		}
		d, terr := time.Parse("2006-01-02", m.Date)
		if terr != nil {
			continue
		}
		if d.Before(cutoff) {
			m.StorageClass = "GLACIER"
			m.GlacierTransitionAt = s.now().Format(time.RFC3339)
			n++
		}
	}
	if err := s.putIndex(ctx, idx); err != nil {
		return 0, err
	}
	return n, s.putLifecyclePolicy(ctx)
}

// lifecycleDoc is the modelled bucket lifecycle policy — the same rule a
// real deployment expresses as an S3 Lifecycle configuration.
type lifecycleDoc struct {
	RuleID         string `json:"rule_id"`
	Status         string `json:"status"`
	Prefix         string `json:"prefix"`
	TransitionDays int    `json:"transition_days"`
	StorageClass   string `json:"storage_class"`
	NoncurrentDays int    `json:"noncurrent_version_days,omitempty"`
	Bucket         string `json:"bucket"`
	GeneratedAt    string `json:"generated_at"`
}

func (s *ArchiveService) putLifecyclePolicy(ctx context.Context) error {
	doc := lifecycleDoc{
		RuleID:         "wal-segment-glacier",
		Status:         "Enabled",
		Prefix:         "",
		TransitionDays: s.retentionDays,
		StorageClass:   "GLACIER",
		Bucket:         s.store.Bucket(),
		GeneratedAt:    s.now().Format(time.RFC3339),
	}
	raw, _ := json.MarshalIndent(doc, "", "  ")
	_, err := s.store.Put(ctx, objectstore.PutInput{
		Key:         "lifecycle.json",
		Body:        strings.NewReader(string(raw)),
		Size:        int64(len(raw)),
		ContentType: "application/json",
	})
	return err
}

// Status is the archive-status view: index entries plus a HEAD cross-check
// so the CLI can flag index/object divergence.
type StatusRow struct {
	SegmentMeta
	Present bool `json:"present"` // object actually in S3
}

// Status loads the index and HEADs each segment.
func (s *ArchiveService) Status(ctx context.Context, shard uint16) ([]StatusRow, error) {
	idx, err := s.LoadIndex(ctx, shard)
	if err != nil {
		return nil, err
	}
	rows := make([]StatusRow, 0, len(idx.Segments))
	for _, m := range idx.Segments {
		row := StatusRow{SegmentMeta: m}
		if _, err := s.store.Head(ctx, m.Key); err == nil {
			row.Present = true
		}
		rows = append(rows, row)
	}
	return rows, nil
}

// --- helpers ------------------------------------------------------------

func hexMD5(b []byte) string {
	sum := md5.Sum(b)
	return hex.EncodeToString(sum[:])
}

func hexSHA256(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
