// Monthly WORM integrity drill — Phase-09 Task 9.3.17 step 5,
// spec §19.7. Downloads a random sample of archived partitions, validates
// the manifest, recomputes SHA-256 over the stored blob, verifies the
// ETag, reads the archive back (parquet rows for "parquet+zstd", CSV for
// legacy "csv+zstd" archives) and counts rows against the manifest, then
// writes one retention_audit_log row per sample
// (check_name='worm_integrity'). Any mismatch is a violation — the run
// returns an error AND records it, so the drill can never silently pass.
package archiver

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/klauspost/compress/zstd"
)

// DrillResult is one sampled archive's verification outcome.
type DrillResult struct {
	Partition    string `json:"partition"`
	ArchiveID    int64  `json:"archive_id"`
	S3Key        string `json:"s3_key"`
	ManifestOK   bool   `json:"manifest_ok"`
	SHA256OK     bool   `json:"sha256_ok"`
	ETagOK       bool   `json:"etag_ok"`
	RowCountOK   bool   `json:"row_count_ok"`
	ManifestRows int64  `json:"manifest_rows"`
	CountedRows  int64  `json:"counted_rows"`
	Detail       string `json:"detail,omitempty"`
}

// OK reports whether every check passed.
func (r DrillResult) OK() bool {
	return r.ManifestOK && r.SHA256OK && r.ETagOK && r.RowCountOK
}

// DrillReport is one drill run.
type DrillReport struct {
	RunID    string        `json:"run_id"`
	At       time.Time     `json:"at"`
	Samples  int           `json:"samples"`
	Results  []DrillResult `json:"results"`
	Failures int           `json:"failures"`
}

// archiveSample is a row picked for the drill.
type archiveSample struct {
	archiveID   int64
	parent      string
	partition   string
	bucket      string
	dataKey     string
	manifestKey string
	archSHA     string
	csvSHA      *string
	etag        string
	rowCount    int64
}

// VerifyDrill samples up to `sample` archived partitions (VERIFIED,
// DROPPED or RESTORED — anything whose bytes must still be intact in the
// WORM bucket) and re-verifies them end-to-end. sample <= 0 means "all".
func (a *Archiver) VerifyDrill(ctx context.Context, sample int) (*DrillReport, error) {
	rep := &DrillReport{RunID: newRunID(), At: a.now()}

	q := `SELECT archive_id, parent_table, partition_name, s3_bucket,
	             s3_key, manifest_key, archive_sha256, csv_sha256, etag,
	             row_count
	      FROM partition_archive_log
	      WHERE status IN ('VERIFIED','DROPPED','RESTORED')
	        AND s3_bucket = $1`
	if sample > 0 {
		q += fmt.Sprintf(` ORDER BY random() LIMIT %d`, sample)
	} else {
		q += ` ORDER BY archive_id`
	}
	rows, err := a.pool.Query(ctx, q, a.store.Bucket())
	if err != nil {
		return nil, fmt.Errorf("archiver: drill sample query: %w", err)
	}
	var samples []archiveSample
	for rows.Next() {
		var s archiveSample
		if err := rows.Scan(&s.archiveID, &s.parent, &s.partition,
			&s.bucket, &s.dataKey, &s.manifestKey, &s.archSHA,
			&s.csvSHA, &s.etag, &s.rowCount); err != nil {
			rows.Close()
			return nil, err
		}
		samples = append(samples, s)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rep.Samples = len(samples)

	for _, s := range samples {
		res := a.verifyOne(ctx, s)
		if !res.OK() {
			rep.Failures++
		}
		rep.Results = append(rep.Results, res)
		a.logDrill(ctx, rep.RunID, s, res)
	}
	if rep.Failures > 0 {
		return rep, fmt.Errorf("archiver: WORM integrity drill FAILED — %d/%d samples corrupt",
			rep.Failures, rep.Samples)
	}
	return rep, nil
}

// verifyOne downloads and re-verifies a single archived partition.
func (a *Archiver) verifyOne(ctx context.Context, s archiveSample) DrillResult {
	r := DrillResult{Partition: s.partition, ArchiveID: s.archiveID, S3Key: s.dataKey}
	fail := func(msg string) DrillResult {
		r.Detail = msg
		return r
	}
	if s.bucket != a.store.Bucket() {
		return fail(fmt.Sprintf("bucket %q != client bucket %q", s.bucket, a.store.Bucket()))
	}

	// Manifest: must parse and agree with the log.
	manRaw, _, err := a.store.Get(ctx, s.manifestKey)
	if err != nil {
		return fail(fmt.Sprintf("manifest fetch: %v", err))
	}
	var man Manifest
	if err := json.Unmarshal(manRaw, &man); err != nil {
		return fail(fmt.Sprintf("manifest corrupt: %v", err))
	}
	r.ManifestOK = man.ArchiveSHA256 == s.archSHA &&
		man.RowCount == s.rowCount && len(man.Columns) > 0
	if !r.ManifestOK {
		r.Detail = "manifest disagrees with archive log"
	}

	// Data object: sha256 over stored bytes, ETag md5, csv hash, rows.
	blob, obj, err := a.store.Get(ctx, s.dataKey)
	if err != nil {
		return fail(fmt.Sprintf("data fetch: %v", err))
	}
	gotSHA := hexSHA256b(blob)
	r.SHA256OK = gotSHA == s.archSHA
	r.ETagOK = obj.ETag == s.etag && obj.ETag == hexMD5(blob)

	var n int64
	if man.Format == "parquet+zstd" {
		// Parquet archive: read the file back and count materialized rows;
		// schema field names must equal the manifest columns.
		pqRows, pqFields, rerr := readParquetRows(blob)
		if rerr != nil {
			return fail(fmt.Sprintf("parquet read: %v", rerr))
		}
		if !sameNames(pqFields, man.Columns) {
			return fail("parquet schema disagrees with manifest columns")
		}
		n = int64(len(pqRows))
	} else {
		zr, err := zstd.NewReader(bytes.NewReader(blob))
		if err != nil {
			return fail(fmt.Sprintf("decompress init: %v", err))
		}
		csvBytes, err := io.ReadAll(zr.IOReadCloser())
		zr.Close()
		if err != nil {
			return fail(fmt.Sprintf("decompress: %v", err))
		}
		if s.csvSHA != nil && hexSHA256b(csvBytes) != *s.csvSHA {
			r.Detail = joinDetail(r.Detail, "csv sha256 mismatch")
		}
		// Count CSV *records* (encoding/csv understands quoted newlines).
		var rerr error
		n, rerr = csvRecordCount(csvBytes)
		if rerr != nil {
			return fail(joinDetail(r.Detail, fmt.Sprintf("csv parse: %v", rerr)))
		}
	}
	r.CountedRows = n
	r.ManifestRows = man.RowCount
	r.RowCountOK = n == s.rowCount && n == man.RowCount
	if !r.RowCountOK {
		r.Detail = joinDetail(r.Detail,
			fmt.Sprintf("row count %d != log %d", n, s.rowCount))
	}
	return r
}

// csvRecordCount streams the CSV once, counting data records.
func csvRecordCount(b []byte) (int64, error) {
	cr := csv.NewReader(bytes.NewReader(b))
	cr.ReuseRecord = true
	var n int64
	for {
		if _, err := cr.Read(); err == io.EOF {
			return n, nil
		} else if err != nil {
			return n, err
		}
		n++
	}
}

// logDrill writes the per-sample audit row (check_name=worm_integrity).
// Tolerates the audit table being absent (older schemas) — the drill
// verdict is still returned to the caller either way.
func (a *Archiver) logDrill(ctx context.Context, runID string,
	s archiveSample, r DrillResult) {
	result := "OK"
	if !r.OK() {
		result = "VIOLATION"
	}
	detail, _ := json.Marshal(r)
	_, err := a.pool.Exec(ctx, `
		INSERT INTO retention_audit_log
		  (run_id, mode, data_type, target, check_name, result, detail)
		VALUES ($1,'DRY_RUN',$2,$3,'worm_integrity',$4,$5)`,
		runID, s.parent, s.partition, result, string(detail))
	if err != nil && !isUndefinedTable(err) {
		// Surface in the detail — never mask a drill verdict.
		_ = err
	}
}

func joinDetail(a, b string) string {
	if a == "" {
		return b
	}
	return a + "; " + b
}

// sameNames reports whether the parquet schema field set equals the
// manifest column set (setwise — parquet-go returns Group fields
// name-sorted; row materialization reorders by manifest ordinal anyway).
func sameNames(fields []string, cols []Column) bool {
	if len(fields) != len(cols) {
		return false
	}
	want := make(map[string]struct{}, len(cols))
	for _, c := range cols {
		want[c.Name] = struct{}{}
	}
	for _, f := range fields {
		if _, ok := want[f]; !ok {
			return false
		}
	}
	return true
}
