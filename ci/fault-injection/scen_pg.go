// PostgreSQL serialization-conflict scenario (L2 tier): two contending
// SERIALIZABLE transactions must resolve to exactly one winner; the loser
// gets SQLSTATE 40001 surfaced as a coded retriable error with zero
// partial state mutation (spec §5.3, §5.40, §2.7.2 L2).

package main

import (
	"context"
	stderrors "errors"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"exchange/internal/audit"
	"exchange/internal/db"
	excerrors "exchange/pkg/errors"
)

// specSQLSTATERetryable mirrors audit.retryableSQLSTATEs (unexported there):
// the spec §5.40 conflict classes worth a whole-transaction retry.
var specSQLSTATERetryable = map[string]bool{
	"23505": true, // unique_violation (sequence_num tail race)
	"40001": true, // serialization_failure
	"40P01": true, // deadlock_detected
}

func pgCode(err error) string {
	var pgErr *pgconn.PgError
	if stderrors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}

func scenarioPGConflict(ctx context.Context, e *env) *Checks {
	c := &Checks{}

	pool, err := db.NewPool(ctx, e.pgDSN, 8)
	if err != nil {
		c.ok("pg:connect", false, err.Error())
		return c
	}
	defer pool.Close()
	if err := pool.Ping(ctx); err != nil {
		c.ok("pg:ping", false, err.Error()+" (compose dev postgres at 127.0.0.1:5433)")
		return c
	}
	c.info("pg:connected", e.pgDSN)

	const acct = 999999001 // synthetic fixture account (mirrors test_serializable.sql)
	const ccy = "USD"
	cleanup := func() {
		ctx2, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = pool.Exec(ctx2,
			"DELETE FROM balances WHERE account_id=$1 AND currency=$2", acct, ccy)
	}
	cleanup()
	defer cleanup()

	// Fixture row: available=100, locked=0, version=0.
	_, err = pool.Exec(ctx, `INSERT INTO balances (account_id, currency, available, locked)
		VALUES ($1,$2,100,0)
		ON CONFLICT (account_id, currency)
		DO UPDATE SET available=100, locked=0, version=0`, acct, ccy)
	if err != nil {
		c.ok("pg:fixture_insert", false, err.Error())
		return c
	}
	var rowsBefore int64
	_ = pool.QueryRow(ctx,
		"SELECT count(*) FROM balances WHERE account_id=$1", acct).Scan(&rowsBefore)

	// --- contended SERIALIZABLE pair --------------------------------------
	// Txn A: lock + update, hold ~400ms, commit.
	// Txn B: same row; blocks on FOR UPDATE, then SSI aborts it on commit.
	type bResult struct {
		err       error // error observed anywhere in B's txn (incl. commit)
		stage     string
		committed bool
	}
	bCh := make(chan bResult, 1)

	connA, err := pool.Acquire(ctx)
	if err != nil {
		c.ok("pg:acquire_a", false, err.Error())
		return c
	}
	txA, err := connA.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		connA.Release()
		c.ok("pg:begin_a", false, err.Error())
		return c
	}
	var avail int64
	err = txA.QueryRow(ctx,
		"SELECT available FROM balances WHERE account_id=$1 AND currency=$2 FOR UPDATE",
		acct, ccy).Scan(&avail)
	if err == nil {
		_, err = txA.Exec(ctx,
			"UPDATE balances SET available=available-10, version=version+1 "+
				"WHERE account_id=$1 AND currency=$2", acct, ccy)
	}
	if err != nil {
		_ = txA.Rollback(ctx)
		connA.Release()
		c.ok("pg:txnA_setup", false, err.Error())
		return c
	}

	// B runs concurrently: blocks on A's row lock, then loses to SSI.
	go func() {
		ctx2, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		connB, err := pool.Acquire(ctx2)
		if err != nil {
			bCh <- bResult{err: err, stage: "acquire"}
			return
		}
		defer connB.Release()
		txB, err := connB.BeginTx(ctx2, pgx.TxOptions{IsoLevel: pgx.Serializable})
		if err != nil {
			bCh <- bResult{err: err, stage: "begin"}
			return
		}
		var a int64
		if err = txB.QueryRow(ctx2,
			"SELECT available FROM balances WHERE account_id=$1 AND currency=$2 FOR UPDATE",
			acct, ccy).Scan(&a); err != nil {
			_ = txB.Rollback(ctx2)
			bCh <- bResult{err: err, stage: "select_for_update"}
			return
		}
		if _, err = txB.Exec(ctx2,
			"UPDATE balances SET available=available-20, version=version+1 "+
				"WHERE account_id=$1 AND currency=$2", acct, ccy); err != nil {
			_ = txB.Rollback(ctx2)
			bCh <- bResult{err: err, stage: "update"}
			return
		}
		if err = txB.Commit(ctx2); err != nil {
			bCh <- bResult{err: err, stage: "commit"}
			return
		}
		bCh <- bResult{committed: true}
	}()

	time.Sleep(400 * time.Millisecond) // let B block on the row lock
	errA := txA.Commit(ctx)
	connA.Release()
	c.okf("pg:txnA_commits", errA == nil, "err=%v", errA)

	var b bResult
	select {
	case b = <-bCh:
	case <-time.After(15 * time.Second):
		c.ok("pg:txnB_terminated", false, "B never finished — lock wait stuck")
		return c
	}
	code := pgCode(b.err)
	c.okf("pg:txnB_aborted_40001", !b.committed && code == "40001",
		"committed=%v stage=%q sqlstate=%q err=%v", b.committed, b.stage, code, b.err)
	c.okf("pg:40001_is_retriable_class", specSQLSTATERetryable[code],
		"sqlstate=%q", code)

	// Zero partial mutation: exactly A's -10 applied; B's -20 rolled back.
	var availAfter, verAfter, rowsAfter int64
	err = pool.QueryRow(ctx,
		"SELECT available, version, (SELECT count(*) FROM balances WHERE account_id=$1) "+
			"FROM balances WHERE account_id=$1 AND currency=$2",
		acct, ccy).Scan(&availAfter, &verAfter, &rowsAfter)
	c.okf("pg:final_state_exact", err == nil && availAfter == 90 && verAfter == 1,
		"available=%d want 90, version=%d want 1, err=%v", availAfter, verAfter, err)
	c.okf("pg:row_count_unchanged", rowsAfter == rowsBefore,
		"rows %d -> %d", rowsBefore, rowsAfter)

	// --- §5.40 retry contract: audit.AppendAuto under persistent conflict ---
	pgAuditRetryScenario(ctx, c, pool)

	return c
}

// auditAppendLockKey mirrors audit.appendLockKey (unexported there) — the
// pg_advisory_xact_lock key serializing audit tail reads.
const auditAppendLockKey int64 = 0x415544495443

// pgAuditRetryScenario proves the §5.40 retry contract deterministically:
// each audit append attempt is parked on the advisory tail lock while a
// dedicated conflictor commits an overlapping read/write set, forcing
// SQLSTATE 40001 on every attempt until the retry budget yields
// TRANSACTION_CONFLICT_RETRY_EXHAUSTED — a coded retriable-class error.
//
// Gating trick: session-level pg_advisory_lock and the xact-level lock in
// audit.Append share one lock space. Holding the session key blocks the
// attempt's first statement *after its snapshot is taken* (verified
// empirically — the abort lands on the INSERT), so a conflictor that
// commits during the block is always "concurrent" to the attempt:
// conflictor's predicate SIREAD on audit_hash_chain + committed state vs
// the attempt's tail-read + INSERT = dangerous structure -> 40001 abort.
func pgAuditRetryScenario(ctx context.Context, c *Checks, pool *pgxpool.Pool) {
	// Seed a chain row — the dangerous structure needs a tail to touch.
	if _, err := audit.AppendAuto(ctx, pool, "fault_probe_seed", nil, "INSERT", nil); err != nil {
		c.ok("pg:audit_seed", false, err.Error())
		return
	}
	var chainBefore int64
	_ = pool.QueryRow(ctx, "SELECT count(*) FROM audit_hash_chain").Scan(&chainBefore)

	hold, err := pool.Acquire(ctx)
	if err != nil {
		c.ok("pg:hold_acquire", false, err.Error())
		return
	}
	defer hold.Release()
	if _, err = hold.Exec(ctx, "SELECT pg_advisory_lock($1)", auditAppendLockKey); err != nil {
		c.ok("pg:hold_lock", false, err.Error())
		return
	}
	unlock := func() {
		cx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = hold.Exec(cx, "SELECT pg_advisory_unlock($1)", auditAppendLockKey)
	}
	defer unlock()

	// Launch the real AppendAuto (3-attempt §5.40 budget) — it will be
	// parked on the advisory xact lock the moment it begins.
	appendDone := make(chan error, 1)
	go func() {
		_, err := audit.AppendAuto(ctx, pool, "fault_probe_blocked", nil, "INSERT", nil)
		appendDone <- err
	}()

	// Per attempt: arm a conflictor, let the blocked attempt's snapshot
	// predate its commit, then release. Relock immediately afterwards so
	// the NEXT attempt queues behind our session lock (FIFO grant order).
	for attempt := 1; attempt <= 3; attempt++ {
		conf, err := pool.Acquire(ctx)
		if err != nil {
			c.ok("pg:conflictor_acquire", false, err.Error())
			return
		}
		txW, err := conf.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
		if err != nil {
			conf.Release()
			c.okf("pg:conflictor_begin_%d", false, "err=%v", err)
			return
		}
		var n int64
		err = txW.QueryRow(ctx, "SELECT count(*) FROM audit_hash_chain").Scan(&n)
		if err == nil {
			_, err = txW.Exec(ctx,
				"UPDATE audit_hash_chain SET table_name = table_name "+
					"WHERE sequence_num = (SELECT max(sequence_num) FROM audit_hash_chain)")
		}
		if err != nil {
			_ = txW.Rollback(ctx)
			conf.Release()
			c.okf("pg:conflictor_arm_%d", false, "err=%v", err)
			return
		}
		time.Sleep(80 * time.Millisecond) // attempt is parked on the lock now
		err = txW.Commit(ctx)
		conf.Release()
		if err != nil {
			c.okf("pg:conflictor_commit_%d", false, "err=%v", err)
			return
		}
		unlock() // attempt proceeds into the committed conflict -> 40001
		// Re-acquire before the next attempt's lock request is queued —
		// blocks until the just-aborted attempt's xact lock is released.
		if attempt < 3 {
			if _, err = hold.Exec(ctx, "SELECT pg_advisory_lock($1)", auditAppendLockKey); err != nil {
				c.okf("pg:relock_%d", false, "err=%v", err)
				return
			}
		}
	}

	var aErr error
	select {
	case aErr = <-appendDone:
	case <-time.After(15 * time.Second):
		c.ok("pg:appendauto_terminated", false, "AppendAuto never returned")
		return
	}
	c.okf("pg:appendauto_coded",
		aErr != nil && errCode(aErr) == "TRANSACTION_CONFLICT_RETRY_EXHAUSTED",
		"err=%v code=%q", aErr, errCode(aErr))
	c.okf("pg:exhaustion_wraps_40001", pgCode(aErr) == "40001",
		"cause sqlstate=%q err=%v", pgCode(aErr), aErr)
	// Severity classification: retry-exhausted conflicts are L2 boundary
	// rejects (fail-closed default), surfaced to the client as a coded error.
	c.okf("pg:exhaustion_severity_L2",
		excerrors.SeverityOf(aErr) == excerrors.SeverityL2,
		"severity=%s", excerrors.SeverityOf(aErr))

	// Zero partial mutation: no half-written chain row survived the 3 aborts.
	var chainAfter int64
	_ = pool.QueryRow(ctx, "SELECT count(*) FROM audit_hash_chain").Scan(&chainAfter)
	c.okf("pg:chain_rowcount_unchanged", chainAfter == chainBefore,
		"rows %d -> %d", chainBefore, chainAfter)

	// Concurrent-append storm on the REAL AppendAuto path. The advisory
	// lock serializes the critical section, but SIREAD predicate locks
	// outlive commits, so some writers legitimately exhaust the 3-attempt
	// budget under 12-way serializable contention — that IS the fail-closed
	// §5.40 contract, not a defect. Assert the actual invariants instead:
	//   * every failure is a coded TRANSACTION_CONFLICT_RETRY_EXHAUSTED
	//     error whose cause is SQLSTATE 40001 (no silent loss, no panic);
	//   * committed rows == successful appends (exactly-once: a committed
	//     append is never lost, a failed append never half-lands);
	//   * the chain still verifies end-to-end.
	// Rows persist across runs — measure the delta, not the absolute count.
	var stormRowsBefore int64
	_ = pool.QueryRow(ctx,
		"SELECT count(*) FROM audit_hash_chain WHERE table_name='fault_probe_storm'").Scan(&stormRowsBefore)
	const storm = 12
	var wg sync.WaitGroup
	stormErrs := make(chan error, storm)
	for i := 0; i < storm; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := audit.AppendAuto(ctx, pool, "fault_probe_storm", nil, "INSERT", nil)
			stormErrs <- err
		}(i)
	}
	wg.Wait()
	close(stormErrs)
	successes, failures, coded := 0, 0, true
	for err := range stormErrs {
		if err == nil {
			successes++
			continue
		}
		failures++
		if errCode(err) != "TRANSACTION_CONFLICT_RETRY_EXHAUSTED" ||
			pgCode(err) != "40001" {
			coded = false
		}
	}
	c.okf("pg:storm_failures_coded", coded,
		"failures=%d (all must be TRANSACTION_CONFLICT_RETRY_EXHAUSTED/40001)",
		failures)
	rep, verr := audit.VerifyThrough(ctx, pool,
		time.Now().UTC().AddDate(0, 0, 1), nil)
	c.okf("pg:chain_verifies_after_storm", verr == nil && rep.OK(),
		"violations=%v err=%v", rep.Violations, verr)
	var stormRowsAfter int64
	_ = pool.QueryRow(ctx,
		"SELECT count(*) FROM audit_hash_chain WHERE table_name='fault_probe_storm'").Scan(&stormRowsAfter)
	c.okf("pg:storm_exactly_once", stormRowsAfter-stormRowsBefore == int64(successes),
		"committed delta=%d, successful appends=%d, failed=%d",
		stormRowsAfter-stormRowsBefore, successes, failures)
	c.okf("pg:storm_accounted", successes+failures == storm,
		"%d/%d", successes+failures, storm)
}
