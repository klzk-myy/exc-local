package audit

import (
	"context"
	"os"
	"testing"
	"time"

	"exchange/internal/db"
)

// Integration test against the dev database. Skips when Postgres is
// unreachable so `go test ./...` stays hermetic off the dev box. Override
// the target with EXC_TEST_DSN.
const defaultTestDSN = "postgres://exchange:exchange_dev@127.0.0.1:5433/exchange?sslmode=disable"

const itestTable = "audit_chain_itest"

func TestAuditChainIntegration(t *testing.T) {
	dsn := os.Getenv("EXC_TEST_DSN")
	if dsn == "" {
		dsn = defaultTestDSN
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool, err := db.NewPool(ctx, dsn, 4)
	if err != nil {
		t.Skipf("postgres unreachable (%v) — skipping integration test", err)
	}
	// Registered FIRST so t.Cleanup LIFO ordering closes the pool LAST —
	// the data cleanup below still has a live pool.
	t.Cleanup(func() { pool.Close() })
	pingCtx, pingCancel := context.WithTimeout(ctx, 3*time.Second)
	err = pool.Ping(pingCtx)
	pingCancel()
	if err != nil {
		t.Skipf("postgres unreachable (%v) — skipping integration test", err)
	}

	// Pre-clean the whole chain: leftover rows from earlier suite packages
	// (e.g. manual_liquidations appended with opaque position payloads by
	// internal/api integration tests) cannot be re-fingerprinted without a
	// PayloadProvider, so the fresh-chain verify below would report them as
	// violations. The scratch DB is disposable and this test requires a
	// genesis-aligned chain, so wipe rather than scope.
	if _, err := pool.Exec(ctx, `DELETE FROM audit_hash_chain`); err != nil {
		t.Fatalf("pre-clean: %v", err)
	}
	var preCount int64
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM audit_hash_chain`).Scan(&preCount); err != nil {
		t.Fatalf("count: %v", err)
	}

	rid1, rid2, rid3 := int64(1), int64(2), int64(3)
	var lastSeq int64
	for i, rid := range []*int64{&rid1, &rid2, &rid3, nil} {
		e, err := AppendAuto(ctx, pool, itestTable, rid, "INSERT", nil)
		if err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
		if e.PayloadHash == "" || e.PrevHash == "" || e.SequenceNum == 0 {
			t.Fatalf("append %d returned incomplete entry %+v", i, e)
		}
		lastSeq = e.SequenceNum
	}
	t.Logf("inserted %d chain rows (last seq=%d)", 4, lastSeq)
	t.Cleanup(func() {
		cleanupCtx, c := context.WithTimeout(context.Background(), 10*time.Second)
		defer c()
		if _, err := pool.Exec(cleanupCtx, `DELETE FROM audit_hash_chain WHERE table_name = $1`, itestTable); err != nil {
			t.Logf("cleanup delete failed: %v", err)
		}
		// Restore the merkle anchor for today over the remaining rows.
		if _, _, err := ComputeMerkleRoot(cleanupCtx, pool, time.Now().UTC()); err != nil {
			t.Logf("cleanup merkle recompute failed: %v", err)
		}
	})

	// Whole-chain verify must be clean before tampering.
	rep, err := VerifyThrough(ctx, pool, time.Now().UTC().AddDate(0, 0, 1), nil)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if !rep.OK() {
		t.Fatalf("fresh chain reported violations: %+v", rep.Violations)
	}

	// Tamper with the middle row's record_id — the DoD case.
	target := preCount + 2
	if _, err := pool.Exec(ctx,
		`UPDATE audit_hash_chain SET record_id = record_id + 1 WHERE sequence_num = $1`,
		target); err != nil {
		t.Fatalf("tamper: %v", err)
	}
	rep, err = VerifyThrough(ctx, pool, time.Now().UTC().AddDate(0, 0, 1), nil)
	if err != nil {
		t.Fatalf("verify after tamper: %v", err)
	}
	if rep.OK() {
		t.Fatal("tampered record_id not detected")
	}
	if !hasViolationOn(rep, target, "payload_hash") {
		t.Fatalf("expected payload_hash violation at seq=%d, got %+v", target, rep.Violations)
	}
	t.Logf("tamper detected: %+v", rep.Violations)

	// Restore the row, confirm clean again, then delete test rows.
	if _, err := pool.Exec(ctx,
		`UPDATE audit_hash_chain SET record_id = record_id - 1 WHERE sequence_num = $1`,
		target); err != nil {
		t.Fatalf("restore: %v", err)
	}
	rep, err = VerifyThrough(ctx, pool, time.Now().UTC().AddDate(0, 0, 1), nil)
	if err != nil {
		t.Fatalf("verify after restore: %v", err)
	}
	if !rep.OK() {
		t.Fatalf("restored chain reported violations: %+v", rep.Violations)
	}

	if _, err := pool.Exec(ctx, `DELETE FROM audit_hash_chain WHERE table_name = $1`, itestTable); err != nil {
		t.Fatalf("delete test rows: %v", err)
	}

	// Daily Merkle root over today's (now test-row-free) rows stores fine.
	root, n, err := ComputeMerkleRoot(ctx, pool, time.Now().UTC())
	if err != nil {
		t.Fatalf("compute merkle: %v", err)
	}
	if len(root) != 64 {
		t.Fatalf("root %q not 64 hex chars", root)
	}
	stored, found, err := StoredMerkleRoot(ctx, pool, time.Now().UTC())
	if err != nil || !found || stored != root {
		t.Fatalf("stored root mismatch: stored=%q found=%v err=%v computed=%q", stored, found, err, root)
	}
	t.Logf("merkle root for today: rows=%d root=%s", n, root)

	// Post-cleanup chain must verify clean end-to-end.
	rep, err = VerifyThrough(ctx, pool, time.Now().UTC().AddDate(0, 0, 1), nil)
	if err != nil {
		t.Fatalf("final verify: %v", err)
	}
	if !rep.OK() {
		t.Fatalf("post-cleanup chain violations: %+v", rep.Violations)
	}
}
