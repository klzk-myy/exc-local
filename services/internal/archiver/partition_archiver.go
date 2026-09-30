// Package archiver implements Task 4.3.7: the PostgreSQL partition
// archival engine — detach partitions older than the cutoff, export them
// to zstd-compressed Parquet with a deterministic column manifest, upload
// to a WORM (Object Lock compliance) bucket, verify ETag+SHA-256 before
// the local drop, log every step to partition_archive_log, and provide the
// restore drill path. The parquet file is the archive object itself —
// zstd page compression inside the container (format "parquet+zstd"),
// sha256 over the stored bytes, per spec §19.7. Dynamic schemas are built
// per-partition in parquet_export.go.
//
// WORM modelling: ObjectLockMode=COMPLIANCE + retain_until is set on every
// upload. Against devs3 the flag is stored as object metadata and enforced
// on DELETE; against real AWS it maps to native Object Lock.
//
// Failure semantics (fail-closed, spec §2.7):
//   - a partition is only DROPPED after the upload round-trips
//     (ETag==md5(blob) AND sha256 manifest verified AND lock mode seen via
//     HEAD) and the log row is written;
//   - if anything after DETACH fails, the engine re-attaches the partition
//     with its original bound expression so a mid-upload drop cannot leave
//     data detached-but-unarchived.
package archiver

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"exchange/internal/objectstore"
)

// DefaultPartitionBucket is the WORM archive bucket.
const DefaultPartitionBucket = "exchange-partition-archive"

// DefaultParents are the spec-named partitioned parents. Supersedes the
// Task 4.3.7 four-table list: Task 9.3.17 step 1 extends the lifecycle to
// `audit_hash_chain` (7-year audit retention, spec §19.12) plus the
// finance-GL parents `ledger_entries`/`journal_entries`. Non-partitioned
// parents are harmless no-ops in the catalog walk.
var DefaultParents = []string{"orders", "trades", "order_audit",
	"ledger_lines", "ledger_entries", "journal_entries", "audit_hash_chain"}

// RetainYears is the MiFID II / CFTC WORM retention window.
const RetainYears = 5

var identRe = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

func quoteIdent(s string) (string, error) {
	if !identRe.MatchString(s) {
		return "", fmt.Errorf("archiver: unsafe identifier %q", s)
	}
	return `"` + s + `"`, nil
}

// Partition is one child table of a partitioned parent.
type Partition struct {
	ParentSchema string    `json:"parent_schema"`
	Parent       string    `json:"parent"`
	Schema       string    `json:"schema"`
	Name         string    `json:"name"`
	BoundExpr    string    `json:"bound_expr"` // raw pg_get_expr(relpartbound)
	RangeEnd     time.Time `json:"range_end"`  // parsed TO bound
}

// Column describes one exported column (deterministic ordinal order).
type Column struct {
	Name     string `json:"name"`
	DataType string `json:"data_type"`
	Ordinal  int    `json:"ordinal"`
}

// Manifest is the JSON sidecar for one exported partition archive.
type Manifest struct {
	ParentTable   string   `json:"parent_table"`
	Partition     string   `json:"partition"`
	Columns       []Column `json:"columns"`
	RowCount      int64    `json:"row_count"`
	Format        string   `json:"format"` // "parquet+zstd"
	ArchiveSHA256 string   `json:"archive_sha256"`
	SizeBytes     int64    `json:"size_bytes"`
	ExportedAt    string   `json:"exported_at"`
	BoundExpr     string   `json:"bound_expr,omitempty"`
}

// LogEntry mirrors partition_archive_log.
type LogEntry struct {
	ArchiveID   int64
	Partition   string
	Status      string
	S3Key       string
	ManifestKey string
}

// Archiver runs the pipeline against a Postgres pool + object store.
type Archiver struct {
	pool       *pgxpool.Pool
	store      objectstore.Client
	parents    []string
	policies   []ClassPolicy
	cutoffDays int
	retainDays int
	now        func() time.Time
	tmpDir     string // export spool dir; "" = os.TempDir
}

func New(pool *pgxpool.Pool, store objectstore.Client) *Archiver {
	return &Archiver{
		pool:       pool,
		store:      store,
		parents:    append([]string{}, DefaultParents...),
		policies:   DefaultClassPolicies(),
		cutoffDays: 90,
		retainDays: RetainYears * 365,
		now:        func() time.Time { return time.Now().UTC() },
	}
}

func (a *Archiver) SetParents(p []string)       { a.parents = append([]string{}, p...) }
func (a *Archiver) SetCutoffDays(d int)         { a.cutoffDays = d }
func (a *Archiver) SetClock(f func() time.Time) { a.now = f }
func (a *Archiver) SetTmpDir(d string)          { a.tmpDir = d }

// --- catalog ------------------------------------------------------------

// partmanParents returns the configured partman parent set intersected
// with a.parents, or a.parents when partman is absent (nonfatal — the
// pg_inherits catalog walk covers native declarative partitioning too).
func (a *Archiver) partmanParents(ctx context.Context) []string {
	var reg *string
	if err := a.pool.QueryRow(ctx,
		`SELECT to_regclass('partman.part_config')::text`).Scan(&reg); err != nil || reg == nil {
		return a.parents
	}
	rows, err := a.pool.Query(ctx, `SELECT parent_table FROM partman.part_config`)
	if err != nil {
		return a.parents
	}
	defer rows.Close()
	want := map[string]bool{}
	for _, p := range a.parents {
		want[p] = true
	}
	var out []string
	for rows.Next() {
		var pt string
		if rows.Scan(&pt) == nil {
			// parent_table may be schema-qualified.
			short := pt
			if i := strings.LastIndex(pt, "."); i >= 0 {
				short = pt[i+1:]
			}
			if want[short] {
				out = append(out, short)
			}
		}
	}
	if len(out) == 0 {
		return a.parents
	}
	return out
}

// ListPartitions enumerates child partitions of the configured parents
// from pg_inherits, parsing each partition bound for its range end.
func (a *Archiver) ListPartitions(ctx context.Context) ([]Partition, error) {
	parents := a.partmanParents(ctx)
	rows, err := a.pool.Query(ctx, `
		SELECT pn.nspname, p.relname, cn.nspname, c.relname,
		       pg_get_expr(c.relpartbound, c.oid)
		FROM pg_inherits i
		JOIN pg_class p   ON i.inhparent = p.oid
		JOIN pg_namespace pn ON p.relnamespace = pn.oid
		JOIN pg_class c   ON i.inhrelid = c.oid
		JOIN pg_namespace cn ON c.relnamespace = cn.oid
		WHERE p.relname = ANY($1)
		ORDER BY p.relname, c.relname`, parents)
	if err != nil {
		return nil, fmt.Errorf("archiver: list partitions: %w", err)
	}
	defer rows.Close()
	var out []Partition
	for rows.Next() {
		var p Partition
		if err := rows.Scan(&p.ParentSchema, &p.Parent, &p.Schema, &p.Name, &p.BoundExpr); err != nil {
			return nil, err
		}
		p.RangeEnd = parseBoundEnd(p.BoundExpr)
		out = append(out, p)
	}
	return out, rows.Err()
}

var boundToRe = regexp.MustCompile(`(?i)\bTO\s*\(\s*'([^']+)'\s*\)`)

// parseBoundEnd extracts the upper bound from a relpartbound expression
// like FOR VALUES FROM ('2025-06-01 00:00:00+00') TO ('2025-07-01 ...').
func parseBoundEnd(expr string) time.Time {
	m := boundToRe.FindStringSubmatch(expr)
	if m == nil {
		return time.Time{}
	}
	for _, layout := range []string{
		"2006-01-02 15:04:05.999999999-07",
		"2006-01-02 15:04:05.999999999Z07",
		"2006-01-02 15:04:05.999999999",
		"2006-01-02 15:04:05-07",
		"2006-01-02 15:04:05Z07",
		"2006-01-02 15:04:05",
		"2006-01-02",
	} {
		if t, err := time.Parse(layout, m[1]); err == nil {
			return t.UTC()
		}
	}
	return time.Time{}
}

// EligiblePartitions returns attached partitions whose range ended before
// their class hot cutoff (default: 90 days ago, per-parent via policies).
func (a *Archiver) EligiblePartitions(ctx context.Context) ([]Partition, error) {
	all, err := a.ListPartitions(ctx)
	if err != nil {
		return nil, err
	}
	var out []Partition
	for _, p := range all {
		hotDays := a.policyFor(p.Parent).HotDays
		cutoff := a.now().AddDate(0, 0, -hotDays)
		if !p.RangeEnd.IsZero() && p.RangeEnd.Before(cutoff) {
			out = append(out, p)
		}
	}
	return out, nil
}

// Columns returns the deterministic export column order (ordinal).
func (a *Archiver) Columns(ctx context.Context, schema, table string) ([]Column, error) {
	rows, err := a.pool.Query(ctx, `
		SELECT column_name, data_type, ordinal_position
		FROM information_schema.columns
		WHERE table_schema = $1 AND table_name = $2
		ORDER BY ordinal_position`, schema, table)
	if err != nil {
		return nil, fmt.Errorf("archiver: columns %s.%s: %w", schema, table, err)
	}
	defer rows.Close()
	var cols []Column
	for rows.Next() {
		var c Column
		if err := rows.Scan(&c.Name, &c.DataType, &c.Ordinal); err != nil {
			return nil, err
		}
		cols = append(cols, c)
	}
	return cols, rows.Err()
}

// --- pipeline stages ------------------------------------------------------

func (a *Archiver) detach(ctx context.Context, p Partition) error {
	parent, err := quoteIdent(p.Parent)
	if err != nil {
		return err
	}
	ps, err := quoteIdent(p.ParentSchema)
	if err != nil {
		return err
	}
	cs, err := quoteIdent(p.Schema)
	if err != nil {
		return err
	}
	cn, err := quoteIdent(p.Name)
	if err != nil {
		return err
	}
	_, err = a.pool.Exec(ctx, fmt.Sprintf(
		`ALTER TABLE %s.%s DETACH PARTITION %s.%s`, ps, parent, cs, cn))
	return err
}

// reattach restores the partition after a post-detach failure, using the
// original bound expression (e.g. "FOR VALUES FROM (...) TO (...)").
func (a *Archiver) reattach(ctx context.Context, p Partition) error {
	if p.BoundExpr == "" {
		return errors.New("archiver: no bound expr for reattach")
	}
	parent, err := quoteIdent(p.Parent)
	if err != nil {
		return err
	}
	ps, err := quoteIdent(p.ParentSchema)
	if err != nil {
		return err
	}
	cs, err := quoteIdent(p.Schema)
	if err != nil {
		return err
	}
	cn, err := quoteIdent(p.Name)
	if err != nil {
		return err
	}
	_, err = a.pool.Exec(ctx, fmt.Sprintf(
		`ALTER TABLE %s.%s ATTACH PARTITION %s.%s %s`, ps, parent, cs, cn, p.BoundExpr))
	return err
}

// exportResult describes the spooled archive.
type exportResult struct {
	path       string // temp .zst file
	cols       []Column
	rowCount   int64
	archSHA256 string
	sizeBytes  int64
}

// hashWriter writes through to w while updating h.
type hashWriter struct {
	w io.Writer
	h hash.Hash
}

func (hw *hashWriter) Write(p []byte) (int, error) {
	n, err := hw.w.Write(p)
	hw.h.Write(p[:n])
	return n, err
}

// s3Keys: {parent}/{name}/{name}.parquet under a scan-friendly layout —
// manifests live in {parent}/{name}/_manifests/ because Hive/Trino
// treat every non-hidden file under a table's location as data; a sibling
// .manifest.json would be parsed as parquet and fail the scan ("Malformed
// Parquet file"). Engines skip _- and .-prefixed paths.
func s3Keys(p Partition) (dataKey, manifestKey string) {
	base := fmt.Sprintf("%s/%s/%s", p.Parent, p.Name, p.Name)
	return base + ".parquet",
		fmt.Sprintf("%s/%s/_manifests/%s.manifest.json", p.Parent, p.Name, p.Name)
}

func (a *Archiver) logInsert(ctx context.Context, p Partition, ex *exportResult,
	dataKey, manifestKey, etag string, retainUntil time.Time) (int64, error) {
	var id int64
	err := a.pool.QueryRow(ctx, `
		INSERT INTO partition_archive_log
		  (parent_table, partition_name, s3_bucket, s3_key, manifest_key,
		   row_count, size_bytes, archive_sha256, csv_sha256, etag,
		   format, object_lock_mode, retain_until, status)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,'parquet+zstd','COMPLIANCE',$11,'EXPORTED')
		RETURNING archive_id`,
		p.Parent, p.Name, a.store.Bucket(), dataKey, manifestKey,
		ex.rowCount, ex.sizeBytes, ex.archSHA256, ex.archSHA256, etag,
		retainUntil).Scan(&id)
	// csv_sha256 is NOT NULL in the schema — for parquet archives it
	// carries the same archive sha256 (the file is the only artifact).
	return id, err
}

func (a *Archiver) logStatus(ctx context.Context, id int64, status, errText string) error {
	set := ""
	switch status {
	case "DROPPED":
		set = ", dropped_at = now()"
	case "RESTORED":
		set = ", restored_at = now()"
	}
	_, err := a.pool.Exec(ctx, fmt.Sprintf(
		`UPDATE partition_archive_log SET status=$2, error=$3 %s WHERE archive_id=$1`, set),
		id, status, nilIfEmpty(errText))
	return err
}

func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// HeldError is returned when a lifecycle transition is attempted on a
// partition covered by an active compliance hold (spec §19.12 item 5 —
// legal-hold partitions never expire and are never dropped).
type HeldError struct {
	Parent    string
	Partition string
	Holds     []Hold
}

func (e *HeldError) Error() string {
	refs := make([]string, len(e.Holds))
	for i, h := range e.Holds {
		refs[i] = fmt.Sprintf("hold #%d case=%s", h.HoldID, h.CaseRef)
	}
	return fmt.Sprintf("archiver: %s.%s under compliance hold (%s) — transition blocked",
		e.Parent, e.Partition, strings.Join(refs, "; "))
}

// ArchivePartition runs the full pipeline for one attached partition:
// hold check → DETACH → export(parquet+zstd) → PUT data+manifest (WORM) →
// HEAD-verify (etag + lock + manifest hash) → log → DROP. On post-detach
// failure the partition is re-attached with its original bound.
func (a *Archiver) ArchivePartition(ctx context.Context, p Partition) (*LogEntry, error) {
	// 0. Compliance hold gate — held partitions never leave the database.
	held, holds, err := a.partitionHeld(ctx, p.Parent, p.Name)
	if err != nil {
		return nil, err
	}
	if held {
		return nil, &HeldError{Parent: p.Parent, Partition: p.Name, Holds: holds}
	}

	// 1. DETACH — after this the data is a standalone table; a failure path
	//    must reattach before returning.
	if err := a.detach(ctx, p); err != nil {
		return nil, fmt.Errorf("archiver: detach %s: %w", p.Name, err)
	}
	le, err := a.archiveDetached(ctx, p)
	if err != nil {
		if rerr := a.reattach(ctx, p); rerr != nil {
			return nil, fmt.Errorf("%w (reattach of %s FAILED: %v — operator action required)",
				err, p.Name, rerr)
		}
		return nil, err
	}
	return le, nil
}

// archiveDetached runs stages 2–5 for a partition whose data already
// lives in the standalone table p.Schema.p.Name — either freshly DETACHed
// (ArchivePartition) or resident in the warm schema (lifecycle warm→cold).
// export → PUT data+manifest (WORM, per-class retain) → HEAD-verify →
// log → DROP. The caller owns any reattach/compensation semantics.
func (a *Archiver) archiveDetached(ctx context.Context, p Partition) (*LogEntry, error) {
	// 2. Export detached partition → temp .parquet (zstd pages) + sha256.
	ex, err := a.exportParquet(ctx, p)
	if err != nil {
		return nil, fmt.Errorf("archiver: export %s: %w", p.Name, err)
	}
	defer os.Remove(ex.path)

	blob, err := os.ReadFile(ex.path)
	if err != nil {
		return nil, err
	}
	man := Manifest{
		ParentTable: p.Parent, Partition: p.Name, Columns: ex.cols,
		RowCount: ex.rowCount, Format: "parquet+zstd",
		ArchiveSHA256: ex.archSHA256,
		SizeBytes:     ex.sizeBytes, ExportedAt: a.now().Format(time.RFC3339),
		BoundExpr: p.BoundExpr,
	}
	manRaw, _ := json.MarshalIndent(man, "", "  ")

	// 3. WORM upload: data object then manifest, both COMPLIANCE-locked.
	dataKey, manifestKey := s3Keys(p)
	retainUntil := a.now().AddDate(0, 0, a.policyFor(p.Parent).RetainDays)
	obj, err := a.store.Put(ctx, objectstore.PutInput{
		Key:                   dataKey,
		Body:                  bytes.NewReader(blob),
		Size:                  int64(len(blob)),
		ContentType:           "application/vnd.apache.parquet",
		ObjectLockMode:        "COMPLIANCE",
		ObjectLockRetainUntil: retainUntil,
		Metadata: map[string]string{
			"parent":         p.Parent,
			"partition":      p.Name,
			"archive-sha256": ex.archSHA256,
			"row-count":      strconv.FormatInt(ex.rowCount, 10),
		},
	})
	if err != nil {
		return nil, fmt.Errorf("archiver: upload %s: %w", dataKey, err)
	}

	// 4. Upload confirmation: ETag must be md5 of the exact blob bytes.
	wantETag := hexMD5(blob)
	if !strings.EqualFold(obj.ETag, wantETag) {
		return nil, fmt.Errorf("archiver: %s ETag %q != md5 %q — upload not confirmed",
			dataKey, obj.ETag, wantETag)
	}
	if _, err := a.store.Put(ctx, objectstore.PutInput{
		Key:                   manifestKey,
		Body:                  bytes.NewReader(manRaw),
		Size:                  int64(len(manRaw)),
		ContentType:           "application/json",
		ObjectLockMode:        "COMPLIANCE",
		ObjectLockRetainUntil: retainUntil,
	}); err != nil {
		return nil, fmt.Errorf("archiver: upload %s: %w", manifestKey, err)
	}

	// HEAD cross-check: lock state + stored hash metadata.
	head, err := a.store.Head(ctx, dataKey)
	if err != nil {
		return nil, fmt.Errorf("archiver: head %s: %w", dataKey, err)
	}
	if !strings.EqualFold(head.ETag, wantETag) ||
		head.Metadata["archive-sha256"] != ex.archSHA256 {
		return nil, fmt.Errorf("archiver: %s HEAD verification failed (etag=%q sha=%q)",
			dataKey, head.ETag, head.Metadata["archive-sha256"])
	}
	if !strings.EqualFold(head.ObjectLockMode, "COMPLIANCE") {
		return nil, fmt.Errorf("archiver: %s missing COMPLIANCE object lock", dataKey)
	}

	// 5. Log then drop — drop is gated on confirmed upload + persisted log.
	logID, err := a.logInsert(ctx, p, ex, dataKey, manifestKey, obj.ETag, retainUntil)
	if err != nil {
		return nil, fmt.Errorf("archiver: log %s: %w", p.Name, err)
	}
	if err := a.logStatus(ctx, logID, "VERIFIED", ""); err != nil {
		return nil, err
	}
	qs, _ := quoteIdent(p.Schema)
	qn, _ := quoteIdent(p.Name)
	if _, err := a.pool.Exec(ctx, fmt.Sprintf(`DROP TABLE %s.%s`, qs, qn)); err != nil {
		return nil, fmt.Errorf("archiver: drop %s: %w", p.Name, err)
	}
	if err := a.logStatus(ctx, logID, "DROPPED", ""); err != nil {
		return nil, fmt.Errorf("archiver: %s dropped but log update failed: %w", p.Name, err)
	}
	return &LogEntry{ArchiveID: logID, Partition: p.Name, Status: "DROPPED",
		S3Key: dataKey, ManifestKey: manifestKey}, nil
}

// --- restore drill --------------------------------------------------------

// RestoreResult is the drill outcome.
type RestoreResult struct {
	Partition     string `json:"partition"`
	IntoTable     string `json:"into_table"`
	ManifestRows  int64  `json:"manifest_rows"`
	LoadedRows    int64  `json:"loaded_rows"`
	Parity        bool   `json:"parity"`
	ArchiveSHA256 string `json:"archive_sha256"`
}

// RestorePartition downloads an archived partition, verifies hashes,
// recreates the table under intoSchema (default "archive_restore"), COPYs
// the CSV back, and asserts row-count parity. Fail-closed: any hash or
// count mismatch is an error and no state is hidden.
func (a *Archiver) RestorePartition(ctx context.Context, partition, intoSchema string) (*RestoreResult, error) {
	if !identRe.MatchString(partition) {
		return nil, fmt.Errorf("archiver: unsafe partition name %q", partition)
	}
	if intoSchema == "" {
		intoSchema = "archive_restore"
	}
	if !identRe.MatchString(intoSchema) {
		return nil, fmt.Errorf("archiver: unsafe schema %q", intoSchema)
	}

	// Locate the latest archive log row for the partition.
	var logID int64
	var parent, bucket, dataKey, manifestKey, archSHA string
	var rows int64
	err := a.pool.QueryRow(ctx, `
		SELECT archive_id, parent_table, s3_bucket, s3_key, manifest_key,
		       archive_sha256, row_count
		FROM partition_archive_log
		WHERE partition_name = $1
		ORDER BY archive_id DESC LIMIT 1`, partition).
		Scan(&logID, &parent, &bucket, &dataKey, &manifestKey, &archSHA, &rows)
	if err != nil {
		return nil, fmt.Errorf("archiver: no archive log for %q: %w", partition, err)
	}
	if bucket != a.store.Bucket() {
		return nil, fmt.Errorf("archiver: log bucket %q != client bucket %q", bucket, a.store.Bucket())
	}

	// Download + verify manifest and data.
	manRaw, _, err := a.store.Get(ctx, manifestKey)
	if err != nil {
		return nil, fmt.Errorf("archiver: fetch %s: %w", manifestKey, err)
	}
	var man Manifest
	if err := json.Unmarshal(manRaw, &man); err != nil {
		return nil, fmt.Errorf("archiver: manifest %s corrupt: %w", manifestKey, err)
	}
	blob, obj, err := a.store.Get(ctx, dataKey)
	if err != nil {
		return nil, fmt.Errorf("archiver: fetch %s: %w", dataKey, err)
	}
	if got := hexSHA256b(blob); got != man.ArchiveSHA256 || got != archSHA {
		return nil, fmt.Errorf("archiver: sha256 mismatch on %s (got %s, manifest %s, log %s)",
			dataKey, got, man.ArchiveSHA256, archSHA)
	}
	if !strings.EqualFold(obj.ETag, hexMD5(blob)) {
		return nil, fmt.Errorf("archiver: ETag mismatch on %s", dataKey)
	}

	// Read the parquet archive; verify schema fields match the manifest
	// columns before loading (a truncated/foreign file fails closed here).
	pqRows, pqFields, err := readParquetRows(blob)
	if err != nil {
		return nil, err
	}
	if !sameNames(pqFields, man.Columns) {
		return nil, fmt.Errorf("archiver: %s parquet schema %v != manifest columns", dataKey, pqFields)
	}

	// Recreate + load under intoSchema.
	qs, _ := quoteIdent(intoSchema)
	qn, _ := quoteIdent(partition)
	if _, err := a.pool.Exec(ctx,
		fmt.Sprintf(`CREATE SCHEMA IF NOT EXISTS %s`, qs)); err != nil {
		return nil, err
	}
	qp, _ := quoteIdent(man.ParentTable)
	if _, err := a.pool.Exec(ctx, fmt.Sprintf(`DROP TABLE IF EXISTS %s.%s`, qs, qn)); err != nil {
		return nil, err
	}
	// Prefer LIKE parent INCLUDING ALL; fall back to manifest column types
	// when the parent is gone (e.g. restore to a scratch instance).
	createLike := fmt.Sprintf(`CREATE TABLE %s.%s (LIKE public.%s INCLUDING ALL)`, qs, qn, qp)
	if _, err := a.pool.Exec(ctx, createLike); err != nil {
		defs := make([]string, len(man.Columns))
		for i, c := range man.Columns {
			cq, cerr := quoteIdent(c.Name)
			if cerr != nil {
				return nil, cerr
			}
			defs[i] = cq + " " + sanitizeType(c.DataType)
		}
		if _, err2 := a.pool.Exec(ctx, fmt.Sprintf(
			`CREATE TABLE %s.%s (%s)`, qs, qn, strings.Join(defs, ", "))); err2 != nil {
			return nil, fmt.Errorf("archiver: recreate %s: %v / fallback: %w", partition, err, err2)
		}
	}

	conn, err := a.pool.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	loaded, err := a.copyFromRows(ctx, conn.Conn(),
		pgx.Identifier{intoSchema, partition}, man.Columns, pqRows)
	conn.Release()
	if err != nil {
		return nil, fmt.Errorf("archiver: load %s: %w", partition, err)
	}

	var count int64
	if err := a.pool.QueryRow(ctx,
		fmt.Sprintf(`SELECT count(*) FROM %s.%s`, qs, qn)).Scan(&count); err != nil {
		return nil, err
	}
	res := &RestoreResult{
		Partition: partition, IntoTable: intoSchema + "." + partition,
		ManifestRows: man.RowCount, LoadedRows: count,
		Parity:        count == man.RowCount && loaded == man.RowCount,
		ArchiveSHA256: man.ArchiveSHA256,
	}
	if !res.Parity {
		return res, fmt.Errorf("archiver: row-count parity FAILED for %s: manifest=%d loaded=%d counted=%d",
			partition, man.RowCount, loaded, count)
	}
	_ = a.logStatus(ctx, logID, "RESTORED", "")
	return res, nil
}

// sanitizeType bounds the manifest-supplied type to a safe token set —
// belt-and-braces since the manifest is our own signed-ish artifact.
var typeRe = regexp.MustCompile(`^[a-zA-Z ]+(\([0-9, ]+\))?(\[\])*$`)

func sanitizeType(t string) string {
	if typeRe.MatchString(t) {
		return t
	}
	return "text"
}

func hexMD5(b []byte) string {
	s := md5.Sum(b)
	return hex.EncodeToString(s[:])
}
func hexSHA256b(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}
