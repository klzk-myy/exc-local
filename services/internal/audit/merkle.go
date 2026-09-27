package audit

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Merkle scheme (Task 1.3.8, spec §5.8 note "daily Merkle root"):
//   - leaves are the day's payload_hash digests in sequence_num order,
//     hex-decoded to 32 raw bytes;
//   - each parent is sha256(left || right) over the raw 32-byte digests;
//   - an odd row duplicates the last digest before pairing;
//   - a single leaf IS the root (no self-pairing);
//   - an empty day hashes to sha256("") = GenesisPrevHash, still stored so
//     the root row proves "no audit events that day", not "job didn't run".
//
// dayBounds returns [start, end) for a UTC calendar date.
func dayBounds(date time.Time) (time.Time, time.Time) {
	y, m, d := date.UTC().Date()
	start := time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
	return start, start.AddDate(0, 0, 1)
}

// MerkleRoot builds the binary Merkle root over leaf digests.
func MerkleRoot(leaves [][]byte) []byte {
	if len(leaves) == 0 {
		sum := sha256.Sum256(nil)
		return sum[:]
	}
	level := leaves
	for len(level) > 1 {
		next := make([][]byte, 0, (len(level)+1)/2)
		for i := 0; i < len(level); i += 2 {
			right := level[i]
			if i+1 < len(level) {
				right = level[i+1]
			} // odd count: duplicate last
			sum := sha256.Sum256(append(append([]byte{}, level[i]...), right...))
			next = append(next, sum[:])
		}
		level = next
	}
	return level[0]
}

// dayLeafDigests loads the day's payload_hash values ordered by
// sequence_num and hex-decodes them to raw digests.
func dayLeafDigests(ctx context.Context, q interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}, date time.Time) ([][]byte, error) {
	start, end := dayBounds(date)
	rows, err := q.Query(ctx, `
		SELECT payload_hash
		FROM audit_hash_chain
		WHERE created_at >= $1 AND created_at < $2
		ORDER BY sequence_num`, start, end)
	if err != nil {
		return nil, fmt.Errorf("audit: load day rows: %w", err)
	}
	defer rows.Close()

	var leaves [][]byte
	for rows.Next() {
		var h string
		if err := rows.Scan(&h); err != nil {
			return nil, fmt.Errorf("audit: scan payload_hash: %w", err)
		}
		digest, err := hex.DecodeString(h)
		if err != nil || len(digest) != sha256.Size {
			return nil, fmt.Errorf("audit: payload_hash %q not a 64-char hex digest", h)
		}
		leaves = append(leaves, digest)
	}
	return leaves, rows.Err()
}

// merkleRootOfDay computes (but does not store) the day's root hex plus the
// leaf count it was built over.
func merkleRootOfDay(ctx context.Context, q interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}, date time.Time) (string, int, error) {
	leaves, err := dayLeafDigests(ctx, q, date)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(MerkleRoot(leaves)), len(leaves), nil
}

// ComputeMerkleRoot computes the Merkle root over all chain rows whose
// created_at falls on date (UTC day boundary), then upserts it into
// audit_merkle_roots keyed on date. Returns the root hex and row count.
func ComputeMerkleRoot(ctx context.Context, pool *pgxpool.Pool, date time.Time) (string, int, error) {
	rootHex, n, err := merkleRootOfDay(ctx, pool, date)
	if err != nil {
		return "", 0, err
	}
	y, m, d := date.UTC().Date()
	day := time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
	if _, err := pool.Exec(ctx, `
		INSERT INTO audit_merkle_roots (date, merkle_root)
		VALUES ($1, $2)
		ON CONFLICT (date) DO UPDATE
		SET merkle_root = EXCLUDED.merkle_root,
		    computed_at = now()`,
		day, rootHex); err != nil {
		return "", 0, fmt.Errorf("audit: upsert merkle root: %w", err)
	}
	return rootHex, n, nil
}

// RunDailyMerkleJob is the 00:10 UTC scheduled job body (scheduler wiring
// is Phase-07's; this is the callable it invokes): it computes and stores
// the Merkle root for the PREVIOUS UTC day — the day that just closed when
// the job runs shortly after midnight.
func RunDailyMerkleJob(ctx context.Context, pool *pgxpool.Pool) (time.Time, string, int, error) {
	yesterday := time.Now().UTC().AddDate(0, 0, -1)
	root, n, err := ComputeMerkleRoot(ctx, pool, yesterday)
	if err != nil {
		return time.Time{}, "", 0, err
	}
	y, m, d := yesterday.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC), root, n, nil
}

// StoredMerkleRoot returns the persisted root for date, or "" with
// found=false when none was computed.
func StoredMerkleRoot(ctx context.Context, pool *pgxpool.Pool, date time.Time) (root string, found bool, err error) {
	y, m, d := date.UTC().Date()
	day := time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
	err = pool.QueryRow(ctx,
		`SELECT merkle_root FROM audit_merkle_roots WHERE date = $1`, day).
		Scan(&root)
	if err == pgx.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("audit: read merkle root: %w", err)
	}
	return root, true, nil
}

// VerifyDayMerkle recomputes the day's Merkle root and compares it to the
// stored audit_merkle_roots value. When no stored root exists the check is
// skipped (found=false, no violation) — the root may legitimately not have
// been computed yet for the current day.
func VerifyDayMerkle(ctx context.Context, pool *pgxpool.Pool, date time.Time) (violation *Violation, recomputed string, stored bool, err error) {
	rootHex, _, err := merkleRootOfDay(ctx, pool, date)
	if err != nil {
		return nil, "", false, err
	}
	storedHex, found, err := StoredMerkleRoot(ctx, pool, date)
	if err != nil {
		return nil, "", false, err
	}
	if !found {
		return nil, rootHex, false, nil
	}
	if storedHex != rootHex {
		y, m, d := date.UTC().Date()
		return &Violation{
			SequenceNum: -1, Day: fmt.Sprintf("%04d-%02d-%02d", y, int(m), d),
			Field:    "merkle_root",
			Expected: rootHex, Actual: storedHex,
			Detail: "stored daily root does not match recomputed day tree",
		}, rootHex, true, nil
	}
	return nil, rootHex, true, nil
}
