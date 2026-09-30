// Parquet partition export — spec §19.7 requires "columnar Parquet format
// compressed with ZSTD". The parquet file itself is the archive object
// (zstd page compression inside the container, not an outer .zst wrap).
//
// Dynamic schema: partitions are discovered from information_schema at
// runtime, so the schema is built per-partition from []Column rather than
// per-table Go structs. Physical mapping:
//
//	bigint/smallint/integer/oid  -> INT64 leaf (widen; restores via cast)
//	boolean                      -> BOOLEAN leaf
//	bytea                        -> BYTE_ARRAY leaf
//	timestamptz/timestamp        -> TIMESTAMP(MICROS) — pg resolution is µs
//	double/float                 -> DOUBLE leaf
//	everything else              -> BYTE_ARRAY UTF8 string
//	  (numeric as canonical text is the lossless choice — pg numeric is
//	  arbitrary-scale, no fixed decimal(p,s) fits; text round-trips
//	  exactly through COPY/CopyFrom and stays columnar. date/jsonb/
//	  uuid/enums likewise restore via their pg text casts.)
//
// Every leaf is OPTIONAL — nullable columns write nil.
package archiver

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql/driver"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/parquet-go/parquet-go"
	"github.com/parquet-go/parquet-go/compress/zstd"
)

// parquetDataExt is the archive object extension (format recorded in the
// manifest and log as "parquet+zstd").
const parquetDataExt = ".parquet"

var parquetWriterOpts = []parquet.WriterOption{
	parquet.MaxRowsPerRowGroup(50000),
	parquet.Compression(&zstd.Codec{}),
	parquet.CreatedBy("exchange", "archiver", "task-9.3.17"),
}

// pgTypeToNode maps an information_schema data_type to a parquet leaf.
func pgTypeToNode(dataType string) (parquet.Node, error) {
	switch dataType {
	case "smallint", "integer", "bigint", "oid", "regtype":
		return parquet.Optional(parquet.Int(64)), nil
	case "boolean":
		return parquet.Optional(parquet.Leaf(parquet.BooleanType)), nil
	case "bytea":
		return parquet.Optional(parquet.Leaf(parquet.ByteArrayType)), nil
	case "real", "double precision":
		return parquet.Optional(parquet.Leaf(parquet.DoubleType)), nil
	case "timestamp without time zone", "timestamp with time zone":
		return parquet.Optional(parquet.Timestamp(parquet.Microsecond)), nil
	case "character varying", "character", "text", "name", "uuid",
		"jsonb", "json", "numeric", "date", "interval", "inet", "cidr",
		"macaddr", "USER-DEFINED":
		return parquet.Optional(parquet.String()), nil
	}
	return nil, fmt.Errorf("archiver: no parquet mapping for pg type %q", dataType)
}

// buildSchema assembles the partition's parquet schema from its live
// column list (deterministic ordinal order — same contract as the CSV
// manifest it replaces).
func buildSchema(cols []Column) (*parquet.Schema, error) {
	g := make(parquet.Group, len(cols))
	for i, c := range cols {
		n, err := pgTypeToNode(c.DataType)
		if err != nil {
			return nil, err
		}
		g[c.Name] = n
		_ = i
	}
	return parquet.NewSchema("partition", g), nil
}

// coerceValue normalizes a pgx scan value into the parquet leaf type the
// column's schema node expects. NULL stays nil.
func coerceValue(dt string, v any) (any, error) {
	if v == nil {
		return nil, nil
	}
	switch dt {
	case "smallint", "integer", "bigint", "oid", "regtype":
		switch n := v.(type) {
		case int64:
			return n, nil
		case int32:
			return int64(n), nil
		case int16:
			return int64(n), nil
		}
		return nil, fmt.Errorf("archiver: %s value %T not int", dt, v)
	case "boolean":
		if b, ok := v.(bool); ok {
			return b, nil
		}
		return nil, fmt.Errorf("archiver: boolean value %T", v)
	case "real", "double precision":
		switch f := v.(type) {
		case float64:
			return f, nil
		case float32:
			return float64(f), nil
		}
		return nil, fmt.Errorf("archiver: %s value %T not float", dt, v)
	case "timestamp without time zone", "timestamp with time zone":
		if t, ok := v.(time.Time); ok {
			return t, nil
		}
		return nil, fmt.Errorf("archiver: timestamp value %T", v)
	case "bytea":
		if b, ok := v.([]byte); ok {
			return b, nil
		}
		return nil, fmt.Errorf("archiver: bytea value %T", v)
	}
	// String-mapped types: prefer the canonical pg text form —
	// driver.Valuer (pgtype.Numeric & friends) yields the exact text COPY
	// produces, which is also the form pg parses on restore.
	if val, ok := v.(driver.Valuer); ok {
		if dv, derr := val.Value(); derr == nil {
			switch t := dv.(type) {
			case string:
				return t, nil
			case []byte:
				return string(t), nil
			}
		}
	}
	switch t := v.(type) {
	case string:
		return t, nil
	case []byte:
		return string(t), nil
	default:
		return fmt.Sprintf("%v", v), nil
	}
}

// exportParquet streams the partition into a zstd-compressed Parquet file
// in the archiver's temp dir, computing the archive SHA-256 over the
// stored bytes. Replaces the csv+zstd pipeline.
func (a *Archiver) exportParquet(ctx context.Context, p Partition) (*exportResult, error) {
	cols, err := a.Columns(ctx, p.Schema, p.Name)
	if err != nil {
		return nil, err
	}
	if len(cols) == 0 {
		return nil, fmt.Errorf("archiver: %s.%s has no columns", p.Schema, p.Name)
	}
	schema, err := buildSchema(cols)
	if err != nil {
		return nil, err
	}
	qs, err := quoteIdent(p.Schema)
	if err != nil {
		return nil, err
	}
	qn, err := quoteIdent(p.Name)
	if err != nil {
		return nil, err
	}
	qcols := make([]string, len(cols))
	for i, c := range cols {
		q, qerr := quoteIdent(c.Name)
		if qerr != nil {
			return nil, qerr
		}
		qcols[i] = q
	}

	tmp, err := os.CreateTemp(a.tmpDir, "part-archive-*.parquet")
	if err != nil {
		return nil, err
	}
	path := tmp.Name()
	fail := func(e error) (*exportResult, error) {
		tmp.Close()
		os.Remove(path)
		return nil, e
	}
	archHash := sha256.New()
	dst := &hashWriter{w: tmp, h: archHash}
	pw := parquet.NewGenericWriter[any](dst,
		append([]parquet.WriterOption{schema}, parquetWriterOpts...)...)

	rows, err := a.pool.Query(ctx,
		fmt.Sprintf(`SELECT %s FROM %s.%s`, strings.Join(qcols, ", "), qs, qn))
	if err != nil {
		pw.Close()
		return fail(err)
	}
	var n int64
	dts := make([]string, len(cols))
	for i, c := range cols {
		dts[i] = c.DataType
	}
	for rows.Next() {
		vals, verr := rows.Values()
		if verr != nil {
			rows.Close()
			pw.Close()
			return fail(verr)
		}
		m := make(map[string]any, len(cols))
		for i, c := range cols {
			cv, cerr := coerceValue(c.DataType, vals[i])
			if cerr != nil {
				rows.Close()
				pw.Close()
				return fail(cerr)
			}
			m[c.Name] = cv
		}
		if _, werr := pw.Write([]any{m}); werr != nil {
			rows.Close()
			pw.Close()
			return fail(fmt.Errorf("archiver: parquet write: %w", werr))
		}
		n++
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		pw.Close()
		return fail(err)
	}
	if err := pw.Close(); err != nil {
		return fail(fmt.Errorf("archiver: parquet finalize: %w", err))
	}
	if err := tmp.Sync(); err != nil {
		return fail(err)
	}
	tmp.Close()

	st, _ := os.Stat(path)
	return &exportResult{
		path:       path,
		cols:       cols,
		rowCount:   n,
		archSHA256: hex.EncodeToString(archHash.Sum(nil)),
		sizeBytes:  st.Size(),
	}, nil
}

// readParquetRows materializes a parquet archive blob into row maps plus
// the file's schema field names — shared by the restore path and the
// monthly WORM integrity drill.
func readParquetRows(blob []byte) (rows []map[string]any, fields []string, err error) {
	f, err := parquet.OpenFile(bytes.NewReader(blob), int64(len(blob)))
	if err != nil {
		return nil, nil, fmt.Errorf("archiver: parquet open: %w", err)
	}
	for _, fld := range f.Schema().Fields() {
		fields = append(fields, fld.Name())
	}
	r := parquet.NewGenericReader[any](f, f.Schema())
	buf := make([]any, 4096)
	for {
		n, rerr := r.Read(buf)
		for _, v := range buf[:n] {
			m, ok := v.(map[string]any)
			if !ok {
				return nil, nil, fmt.Errorf("archiver: parquet row %T not a map", v)
			}
			rows = append(rows, m)
		}
		if rerr != nil {
			if rerr == io.EOF {
				break
			}
			return nil, nil, fmt.Errorf("archiver: parquet read: %w", rerr)
		}
		if n == 0 {
			break
		}
	}
	return rows, fields, nil
}

// colList returns the ordered column names for INSERT/CopyFrom.
func colList(cols []Column) []string {
	names := make([]string, len(cols))
	for i, c := range cols {
		names[i] = c.Name
	}
	return names
}

// copyFromRows inserts the parquet-materialized rows back via the pgx
// CopyFrom fast path (binary copy, not text CSV — same atomicity class).
func (a *Archiver) copyFromRows(ctx context.Context, conn *pgx.Conn,
	ident pgx.Identifier, cols []Column, rows []map[string]any) (int64, error) {
	names := colList(cols)
	src := pgx.CopyFromSlice(len(rows), func(i int) ([]any, error) {
		rec := make([]any, len(cols))
		for j, c := range cols {
			v := rows[i][c.Name]
			// Timestamp leaf round-trips as int64 micros — pgx wants
			// time.Time for timestamptz.
			if v != nil && (c.DataType == "timestamp with time zone" ||
				c.DataType == "timestamp without time zone") {
				if us, ok := v.(int64); ok {
					v = time.UnixMicro(us).UTC()
				}
			}
			rec[j] = v
		}
		return rec, nil
	})
	return conn.CopyFrom(ctx, ident, names, src)
}
