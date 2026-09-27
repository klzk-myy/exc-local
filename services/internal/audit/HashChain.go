// Package audit implements the tamper-evident audit hash chain for
// `audit_hash_chain` (migration 009, spec §5.8) and the daily Merkle root
// in `audit_merkle_roots` (migration 021) — Task 1.3.8.
//
// Hash scheme (all digests lowercase hex-encoded SHA-256, "|" separated):
//
//	canonical_payload = "<sequence_num>|<id>|<created_at RFC3339Nano UTC>|<hex(payload)>"
//	payload_hash      = sha256hex(table_name | "|" | record_id | "|" | action | "|" | canonical_payload)
//	prev_hash         = sha256hex(prev.payload_hash || prev.prev_hash)   (chain link)
//	genesis prev_hash = sha256hex("")                                    (empty-input digest)
//
// record_id encodes NULL as the literal segment "-"; a present value
// encodes as decimal. canonical_payload covers every stored column except
// the hash columns themselves, so verify-audit can recompute payload_hash
// from the table alone — the schema stores only hashes, never payloads.
//
// payload is optional supplementary evidence bytes (convention: canonical
// JSON of the audited record image). It is fingerprinted but NOT stored:
// emitters should pass nil so the row stays self-verifiable. Rows written
// with opaque payload bytes remain protected by the prev_hash chain and
// the daily Merkle root, but their payload_hash preimage is recomputable
// only with the same payload bytes re-supplied through a PayloadProvider.
//
// Concurrency: Append serializes tail selection with a transaction-scoped
// advisory lock; AppendAuto additionally retries the whole transaction on
// SQLSTATE 23505 (sequence_num unique violation) / 40001 / 40P01 per the
// spec §5.40 retry contract, returning TRANSACTION_CONFLICT_RETRY_EXHAUSTED
// (HTTP 503 registry code) after maxAttempts.
package audit

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	excerrors "exchange/pkg/errors"
)

// appendLockKey is the pg_advisory_xact_lock key serializing audit tail
// reads. Fixed arbitrary constant ("AUDITC" in ASCII hex, low bits).
const appendLockKey int64 = 0x415544495443

// maxAttempts bounds AppendAuto's conflict-retry loop (spec §5.40: 3).
const maxAttempts = 3

// GenesisPrevHash is the prev_hash of the first chain row: SHA-256 of the
// empty input. Documented chain convention — every verifier uses the same
// constant, so a forged genesis row cannot smuggle in a foreign anchor.
const GenesisPrevHash = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

// retryableSQLSTATEs are the conflict classes worth a whole-transaction
// retry: unique violation (racing tail append), serialization failure and
// deadlock per spec §5.40.
var retryableSQLSTATEs = map[string]bool{
	"23505": true, // unique_violation (sequence_num)
	"40001": true, // serialization_failure
	"40P01": true, // deadlock_detected
}

// Entry is one audit_hash_chain row as stored.
type Entry struct {
	ID          int64
	SequenceNum int64
	TableName   string
	RecordID    *int64
	Action      string
	PayloadHash string
	PrevHash    string
	CreatedAt   time.Time
}

// PayloadProvider re-supplies the payload bytes originally passed to
// Append for a row, so verification can recompute payload_hash for
// emitter rows that used opaque payloads. nil means "treat payload as
// empty", which is correct for every row written with Append(payload=nil).
type PayloadProvider func(e Entry) []byte

// sha256hex returns the lowercase hex SHA-256 digest of b.
func sha256hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// canonicalRecordID encodes record_id for the hash preimage: NULL is the
// literal "-" sentinel, present values are decimal.
func canonicalRecordID(recordID *int64) string {
	if recordID == nil {
		return "-"
	}
	return fmt.Sprintf("%d", *recordID)
}

// CanonicalPayload builds the fourth payload_hash preimage segment from the
// remaining stored fields plus the caller payload (hex-encoded; empty when
// nil). Deterministic in the stored row, which is what makes verify-audit
// recomputation possible.
//
// The created_at segment must use the microsecond-truncated UTC instant —
// timestamptz stores 1µs resolution, so Append truncates before hashing and
// inserts the same value it hashed.
func CanonicalPayload(seq, id int64, createdAt time.Time, payload []byte) string {
	return fmt.Sprintf("%d|%d|%s|%x", seq, id,
		createdAt.UTC().Format(time.RFC3339Nano), payload)
}

// PayloadHash computes sha256hex(table_name|record_id|action|canonical_payload).
func PayloadHash(tableName string, recordID *int64, action string, seq, id int64, createdAt time.Time, payload []byte) string {
	preimage := fmt.Sprintf("%s|%s|%s|%s",
		tableName, canonicalRecordID(recordID), action,
		CanonicalPayload(seq, id, createdAt, payload))
	return sha256hex([]byte(preimage))
}

// ChainHash computes the chain link prev_hash = sha256hex(prev.payload_hash || prev.prev_hash).
func ChainHash(prevPayloadHash, prevPrevHash string) string {
	return sha256hex([]byte(prevPayloadHash + prevPrevHash))
}

// validateAppendInput fails closed on fields that would corrupt the
// preimage grammar (the "|" separator) or overflow column widths.
func validateAppendInput(tableName string, recordID *int64, action string) error {
	if tableName == "" || len(tableName) > 64 || strings.ContainsRune(tableName, '|') {
		return fmt.Errorf("audit: table_name %q empty, too long or contains '|'", tableName)
	}
	if action == "" || len(action) > 16 || strings.ContainsRune(action, '|') {
		return fmt.Errorf("audit: action %q empty, too long or contains '|'", action)
	}
	if recordID != nil && *recordID < 0 {
		return fmt.Errorf("audit: record_id %d must be >= 0", *recordID)
	}
	return nil
}

// tailRow is the current chain tip read inside the append transaction.
type tailRow struct {
	id          int64
	sequenceNum int64
	payloadHash string
	prevHash    string
}

// readTail locks the append path and returns the highest-sequence row, or
// nil at genesis. Callers must already hold a transaction.
func readTail(ctx context.Context, tx pgx.Tx) (*tailRow, error) {
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", appendLockKey); err != nil {
		return nil, fmt.Errorf("audit: advisory lock: %w", err)
	}
	var t tailRow
	err := tx.QueryRow(ctx, `
		SELECT id, sequence_num, payload_hash, prev_hash
		FROM audit_hash_chain
		ORDER BY sequence_num DESC
		LIMIT 1`).Scan(&t.id, &t.sequenceNum, &t.payloadHash, &t.prevHash)
	if err == pgx.ErrNoRows {
		return nil, nil // genesis
	}
	if err != nil {
		return nil, fmt.Errorf("audit: read tail: %w", err)
	}
	if t.prevHash == "" {
		// prev_hash is nullable in the schema; a NULL past genesis is
		// treated as corruption rather than silently re-anchored.
		return nil, excerrors.New("AUDIT_CHAIN_BROKEN",
			fmt.Sprintf("audit: tail row sequence_num=%d has NULL/empty prev_hash", t.sequenceNum))
	}
	return &t, nil
}

// buildEntry computes the chained hashes for the next row after tail
// (nil tail = genesis). Pure: same inputs always give the same Entry, which
// is what verify-audit replays.
func buildEntry(tail *tailRow, tableName string, recordID *int64, action string, payload []byte, id, seq int64, createdAt time.Time) Entry {
	prevHash := GenesisPrevHash
	if tail != nil {
		prevHash = ChainHash(tail.payloadHash, tail.prevHash)
	}
	return Entry{
		ID:          id,
		SequenceNum: seq,
		TableName:   tableName,
		RecordID:    recordID,
		Action:      action,
		PayloadHash: PayloadHash(tableName, recordID, action, seq, id, createdAt, payload),
		PrevHash:    prevHash,
		CreatedAt:   createdAt,
	}
}

// isRetryable reports whether err is a Postgres conflict worth a
// whole-transaction retry (unique violation on sequence_num, serialization
// failure, deadlock).
func isRetryable(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return retryableSQLSTATEs[pgErr.Code]
	}
	return false
}

// Append writes one chained audit row inside tx.
//
// The caller owns tx (SERIALIZABLE recommended for balance-adjacent audit
// writes; read-committed suffices under the advisory lock). On a unique
// violation the transaction is already aborted — the caller must retry the
// WHOLE tx so the tail is re-read, not just resubmit the INSERT. Appends
// that must be retried automatically should use AppendAuto.
//
// created_at is set by Append (not the column DEFAULT) because it is part
// of the payload_hash preimage; it is truncated to microseconds to match
// timestamptz storage precision.
func Append(ctx context.Context, tx pgx.Tx, tableName string, recordID *int64, action string, payload []byte) (Entry, error) {
	if err := validateAppendInput(tableName, recordID, action); err != nil {
		return Entry{}, err
	}
	tail, err := readTail(ctx, tx)
	if err != nil {
		return Entry{}, err
	}

	var id int64
	if err := tx.QueryRow(ctx, `SELECT nextval('audit_hash_chain_id_seq')`).Scan(&id); err != nil {
		return Entry{}, fmt.Errorf("audit: allocate id: %w", err)
	}
	seq := int64(1)
	if tail != nil {
		seq = tail.sequenceNum + 1
	}
	createdAt := time.Now().UTC().Truncate(time.Microsecond)
	e := buildEntry(tail, tableName, recordID, action, payload, id, seq, createdAt)

	_, err = tx.Exec(ctx, `
		INSERT INTO audit_hash_chain
		    (id, sequence_num, table_name, record_id, action, payload_hash, prev_hash, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		e.ID, e.SequenceNum, e.TableName, e.RecordID, e.Action,
		e.PayloadHash, e.PrevHash, e.CreatedAt)
	if err != nil {
		return Entry{}, fmt.Errorf("audit: insert seq=%d: %w", seq, err)
	}
	return e, nil
}

// AppendAuto is the pool-based convenience wrapper: it runs Append in its
// own SERIALIZABLE transaction and retries the whole transaction on
// retryable conflicts (max 3 attempts per spec §5.40), then returns
// TRANSACTION_CONFLICT_RETRY_EXHAUSTED.
func AppendAuto(ctx context.Context, pool *pgxpool.Pool, tableName string, recordID *int64, action string, payload []byte) (Entry, error) {
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
		if err != nil {
			return Entry{}, fmt.Errorf("audit: begin tx: %w", err)
		}
		e, err := Append(ctx, tx, tableName, recordID, action, payload)
		if err == nil {
			if err = tx.Commit(ctx); err == nil {
				return e, nil
			}
		}
		_ = tx.Rollback(ctx)
		lastErr = err
		if !isRetryable(err) {
			return Entry{}, err
		}
	}
	return Entry{}, excerrors.Wrap("TRANSACTION_CONFLICT_RETRY_EXHAUSTED",
		fmt.Sprintf("audit: append failed after %d attempts", maxAttempts), lastErr)
}

// Violation describes one detected chain integrity failure.
type Violation struct {
	SequenceNum int64  // row where the failure was detected
	Day         string // UTC date (YYYY-MM-DD) of that row's created_at
	Field       string // payload_hash | prev_hash | sequence_num | merkle_root
	Expected    string
	Actual      string
	Detail      string
}

func (v Violation) String() string {
	return fmt.Sprintf("day=%s sequence_num=%d field=%s expected=%s actual=%s %s",
		v.Day, v.SequenceNum, v.Field, v.Expected, v.Actual, v.Detail)
}

// VerifyReport is the outcome of a chain verification pass.
type VerifyReport struct {
	RowsChecked int
	Violations  []Violation
}

// OK reports a fully verified chain.
func (r VerifyReport) OK() bool { return len(r.Violations) == 0 }

// verifyRows replays the chain over rows ordered by sequence_num starting
// at genesis (the first row must be sequence_num 1). Pure — the DB loader
// and unit tests share it.
func verifyRows(rows []Entry, provider PayloadProvider) VerifyReport {
	rep := VerifyReport{RowsChecked: len(rows)}
	var prev *Entry
	for i := range rows {
		row := rows[i]
		day := row.CreatedAt.UTC().Format("2006-01-02")

		// Sequence contiguity: a gap means a row was deleted or
		// renumbered — the prev_hash link usually catches the same
		// tamper, but the explicit check pinpoints it.
		wantSeq := int64(1)
		if prev != nil {
			wantSeq = prev.SequenceNum + 1
		}
		if row.SequenceNum != wantSeq {
			rep.Violations = append(rep.Violations, Violation{
				SequenceNum: row.SequenceNum, Day: day, Field: "sequence_num",
				Expected: fmt.Sprintf("%d", wantSeq), Actual: fmt.Sprintf("%d", row.SequenceNum),
				Detail: "sequence gap: row deleted or renumbered",
			})
		}

		// Chain link: recompute prev_hash from the previous row.
		wantPrev := GenesisPrevHash
		if prev != nil {
			wantPrev = ChainHash(prev.PayloadHash, prev.PrevHash)
		}
		if row.PrevHash != wantPrev {
			rep.Violations = append(rep.Violations, Violation{
				SequenceNum: row.SequenceNum, Day: day, Field: "prev_hash",
				Expected: wantPrev, Actual: row.PrevHash,
				Detail: "chain link mismatch: predecessor tampered, missing, or this row rewritten",
			})
		}

		// Payload fingerprint: recompute from stored fields (+ payload
		// when a provider re-supplies it).
		var payload []byte
		if provider != nil {
			payload = provider(row)
		}
		wantPayload := PayloadHash(row.TableName, row.RecordID, row.Action,
			row.SequenceNum, row.ID, row.CreatedAt, payload)
		if row.PayloadHash != wantPayload {
			detail := "payload_hash mismatch: stored row fields mutated"
			if provider == nil {
				detail += " (or row was written with an opaque payload — supply a PayloadProvider)"
			}
			rep.Violations = append(rep.Violations, Violation{
				SequenceNum: row.SequenceNum, Day: day, Field: "payload_hash",
				Expected: wantPayload, Actual: row.PayloadHash, Detail: detail,
			})
		}

		prev = &rows[i]
	}
	return rep
}

// loadRowsThrough returns every chain row with created_at < endExclusive
// (timestamptz) ordered by sequence_num — i.e. genesis through endExclusive.
func loadRowsThrough(ctx context.Context, q interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}, endExclusive time.Time) ([]Entry, error) {
	rows, err := q.Query(ctx, `
		SELECT id, sequence_num, table_name, record_id, action, payload_hash, prev_hash, created_at
		FROM audit_hash_chain
		WHERE created_at < $1
		ORDER BY sequence_num`, endExclusive)
	if err != nil {
		return nil, fmt.Errorf("audit: load rows: %w", err)
	}
	defer rows.Close()

	var out []Entry
	for rows.Next() {
		var e Entry
		var prevHash *string
		if err := rows.Scan(&e.ID, &e.SequenceNum, &e.TableName, &e.RecordID,
			&e.Action, &e.PayloadHash, &prevHash, &e.CreatedAt); err != nil {
			return nil, fmt.Errorf("audit: scan row: %w", err)
		}
		if prevHash != nil {
			e.PrevHash = *prevHash
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// VerifyThrough verifies the whole chain from genesis up to (not
// including) endExclusive — call it with the day AFTER the target date to
// cover that date fully. It recomputes payload_hash and prev_hash for
// every retained row. provider is optional; nil assumes empty payloads.
func VerifyThrough(ctx context.Context, pool *pgxpool.Pool, endExclusive time.Time, provider PayloadProvider) (VerifyReport, error) {
	rows, err := loadRowsThrough(ctx, pool, endExclusive)
	if err != nil {
		return VerifyReport{}, err
	}
	return verifyRows(rows, provider), nil
}
