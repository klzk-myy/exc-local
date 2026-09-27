// PostgreSQL serialization-conflict scenario (L2 tier): two contending
// SERIALIZABLE transactions must resolve to exactly one winner; the loser
// gets SQLSTATE 40001 surfaced as a coded retriable error with zero
// partial state mutation (spec §5.3, §5.40, §2.7.2 L2).

package main

import (
	"context"
	stderrors "errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"exchange/internal/audit"
	"exchange/internal/db"
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
		err      error // error observed anywhere in B's txn (incl. commit)
		stage    string
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

// pgAuditRetryScenario holds a SERIALIZABLE blocker that predicate-reads
// audit_hash_chain and writes its tail row, forcing AppendAuto's
// SERIALIZABLE commits to abort 40001 until the 3-attempt budget exhausts
// to TRANSACTION_CONFLICT_RETRY_EXHAUSTED (spec §5.40).
func pgAuditRetryScenario(ctx context.Context, c *Checks, pool *pgxpool.Pool) {
	// The dangerous structure needs a non-empty chain (blocker writes the
	// tail row the appender's tail-read touches). Seed one probe row.
	if _, err := audit.AppendAuto(ctx, pool, "fault_probe_seed", nil, "INSERT", nil); err != nil {
		c.ok("pg:audit_seed", false, err.Error())
		return
	}
	var chainBefore int64
	_ = pool.QueryRow(ctx, "SELECT count(*) FROM audit_hash_chain").Scan(&chainBefore)

	const outerAttempts = 3
	for outer := 1; outer <= outerAttempts; outer++ {
		bconn, err := pool.Acquire(ctx)
		if err != nil {
			c.ok("pg:blocker_acquire", false, err.Error())
			return
		}
		btx, err := bconn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
		if err != nil {
			bconn.Release()
			c.ok("pg:blocker_begin", false, err.Error())
			return
		}
		// Predicate-read the whole table + write the tail row: appender's
		// tail-read hits the written row (wr edge) and its INSERT overlaps
		// the predicate (rw edge) -> dangerous structure -> appender aborts.
		var n int64
		err = btx.QueryRow(ctx, "SELECT count(*) FROM audit_hash_chain").Scan(&n)
		if err == nil {
			_, err = btx.Exec(ctx,
				"UPDATE audit_hash_chain SET table_name = table_name "+
					"WHERE sequence_num = (SELECT max(sequence_num) FROM audit_hash_chain)")
		}
		if err != nil {
			_ = btx.Rollback(ctx)
			bconn.Release()
			c.okf("pg:blocker_arm", false, "err=%v", err)
			return
		}

		_, aErr := audit.AppendAuto(ctx, pool, "fault_probe_blocked", nil, "INSERT", nil)
		_ = btx.Rollback(ctx)
		bconn.Release()

		if aErr == nil {
			// SSI may choose the blocker as victim on rare schedules —
			// retry the whole sub-scenario rather than flake.
			if outer < outerAttempts {
				continue
			}
			c.okf("pg:appendauto_exhausted", false,
				"AppendAuto committed under held serializable conflict after %d tries", outer)
			return
		}
		c.okf("pg:appendauto_coded",
			errCode(aErr) == "TRANSACTION_CONFLICT_RETRY_EXHAUSTED",
			"err=%v code=%q", aErr, errCode(aErr))
		var pgErr *pgconn.PgError
		c.okf("pg:exhaustion_wraps_40001", stderrors.As(aErr, &pgErr) &&
			pgErr.Code == "40001",
			"cause sqlstate=%q", pgCode(aErr))

		// Zero partial mutation: no half-written chain row survived.
		var chainAfter int64
		_ = pool.QueryRow(ctx, "SELECT count(*) FROM audit_hash_chain").Scan(&chainAfter)
		c.okf("pg:chain_rowcount_unchanged", chainAfter == chainBefore,
			"rows %d -> %d", chainBefore, chainAfter)

		// And the chain itself still verifies end-to-end.
		rep, verr := audit.VerifyThrough(ctx, pool,
			time.Now().UTC().AddDate(0, 0, 1), nil)
		c.okf("pg:chain_still_verifies", verr == nil && rep.OK(),
			"violations=%v err=%v", rep.Violations, verr)
		return
	}
}
