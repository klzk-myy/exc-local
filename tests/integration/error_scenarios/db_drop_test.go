// Task 8.3.5 scenario 2 — PostgreSQL connection drop during a balance
// update (spec §2.7.2 L2: synchronous atomic rejection, zero
// side-effects, automatic rollback of reservations; §5.40 transaction
// isolation).
//
// Fault model: the balance mutation runs inside a real SERIALIZABLE
// transaction on the scratch migverify instance; a second connection
// then kills the transaction's backend mid-flight (the same failure a
// dropped socket, failover or OOM kill produces — pg_terminate_backend
// severs the session exactly as a TCP reset does).
//
// Contract asserted — never "something errored":
//   - the in-flight statement or the commit MUST fail (no ambiguous
//     silent success);
//   - after reconnect, the balance row MUST equal its pre-transaction
//     value — atomic rollback, zero partial mutation;
//   - the killed transaction's row lock MUST be released (no wedged
//     lock blocking the next update);
//   - the pool MUST recover — subsequent reads succeed on fresh
//     connections (pgxpool re-dials).
//
// Gate: EXC_PG_TEST=1 (EXC_PG_DSN overrides the migverify default).
// The probe row lives in the real `balances` table under a synthetic
// account id so the assertion exercises the production row shape
// (available/locked/total GENERATED, optimistic-lock version).
package error_scenarios

import (
	"context"
	stderrors "errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// probeAccountID is the synthetic account whose balance row the kill
// tests mutate — far above any seeded range, deleted on cleanup.
const probeAccountID int64 = 9_900_000_001

// setupProbeRow upserts the probe row to a known available balance and
// registers its deletion.
func setupProbeRow(t *testing.T, pool *pgxpool.Pool, available string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := pool.Exec(ctx, `
		INSERT INTO balances (account_id, currency, available, locked)
		VALUES ($1,'USD',$2::numeric,0)
		ON CONFLICT (account_id, currency)
		DO UPDATE SET available=$2::numeric, locked=0`, probeAccountID, available); err != nil {
		t.Fatalf("seed probe balance: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = pool.Exec(ctx,
			`DELETE FROM balances WHERE account_id=$1`, probeAccountID)
	})
}

// readProbeBalance returns (available, locked, version).
func readProbeBalance(t *testing.T, pool *pgxpool.Pool) (avail, locked string, version int64) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := pool.QueryRow(ctx, `
		SELECT available::text, locked::text, version
		FROM balances WHERE account_id=$1 AND currency='USD'`,
		probeAccountID).Scan(&avail, &locked, &version); err != nil {
		t.Fatalf("read probe balance: %v", err)
	}
	return avail, locked, version
}

// terminateBackend kills pid's session from a second pooled connection,
// waiting until pg_stat_activity confirms the backend is gone so the
// rollback is verifiably complete before assertions run.
func terminateBackend(t *testing.T, pool *pgxpool.Pool, pid int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var ok bool
	if err := pool.QueryRow(ctx,
		`SELECT pg_terminate_backend($1)`, pid).Scan(&ok); err != nil {
		t.Fatalf("pg_terminate_backend: %v", err)
	}
	if !ok {
		t.Fatalf("pg_terminate_backend(%d) returned false", pid)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var n int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM pg_stat_activity WHERE pid=$1`, pid).Scan(&n); err == nil && n == 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("backend %d still visible after terminate", pid)
}

// TestPGDrop_BalanceCommitKilled kills the backend between the balance
// UPDATE and COMMIT — the update must never become visible.
func TestPGDrop_BalanceCommitKilled(t *testing.T) {
	pool := pgPool(t)
	setupProbeRow(t, pool, "1000.00000000")
	wantAvail, wantLocked, wantVer := readProbeBalance(t, pool)

	ctx := context.Background()
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	var pid int
	if err := tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatalf("backend pid: %v", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE balances SET available=available+500, version=version+1
		WHERE account_id=$1 AND currency='USD'`, probeAccountID); err != nil {
		t.Fatalf("in-tx update: %v", err)
	}

	terminateBackend(t, pool, pid)

	// The tx is dead: Commit must report failure, not phantom success.
	commitErr := tx.Commit(ctx)
	if commitErr == nil {
		t.Fatal("commit succeeded after backend kill — silent partial write")
	}
	// Deferred Rollback must be a no-op safe call, not a panic or a
	// second hidden failure mode.
	if rbErr := tx.Rollback(ctx); rbErr != nil &&
		!stderrors.Is(rbErr, pgx.ErrTxClosed) && !stderrors.Is(rbErr, pgx.ErrTxCommitRollback) {
		t.Fatalf("post-kill rollback returned unexpected error: %v", rbErr)
	}

	gotAvail, gotLocked, gotVer := readProbeBalance(t, pool)
	if gotAvail != wantAvail || gotLocked != wantLocked || gotVer != wantVer {
		t.Fatalf("balance mutated by killed tx: (%s,%s,v%d) != (%s,%s,v%d) — L2 zero-side-effects violated",
			gotAvail, gotLocked, gotVer, wantAvail, wantLocked, wantVer)
	}
}

// TestPGDrop_BalanceKillMidStatement kills the backend while its UPDATE
// is blocked on pg_sleep — the statement must error in-band, the tx must
// be unusable afterwards, and the row must be untouched.
func TestPGDrop_BalanceKillMidStatement(t *testing.T) {
	pool := pgPool(t)
	setupProbeRow(t, pool, "2000.00000000")
	wantAvail, _, _ := readProbeBalance(t, pool)

	ctx := context.Background()
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	var pid int
	if err := tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatalf("backend pid: %v", err)
	}

	// The balance update lands inside the tx first; a following
	// long-running statement keeps the tx in flight so the kill lands
	// mid-transaction — the same shape as a socket drop mid-commit.
	if _, err := tx.Exec(ctx, `
		UPDATE balances SET available=available+500, version=version+1
		WHERE account_id=$1 AND currency='USD'`, probeAccountID); err != nil {
		t.Fatalf("in-tx update: %v", err)
	}
	execErr := make(chan error, 1)
	go func() {
		var slept int
		execErr <- tx.QueryRow(ctx, `SELECT 1 FROM pg_sleep(5)`).Scan(&slept)
	}()
	// Give the statement a beat to reach the backend before the kill.
	time.Sleep(300 * time.Millisecond)
	terminateBackend(t, pool, pid)

	select {
	case err := <-execErr:
		if err == nil {
			t.Fatal("mid-statement kill produced no error — unacceptable ambiguity")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("statement neither failed nor returned within 10s of the kill — conn dead but caller wedged")
	}

	// Any further use of the dead tx must fail in-band.
	if _, err := tx.Exec(ctx, `SELECT 1`); err == nil {
		t.Fatal("dead transaction accepted further statements")
	}
	_ = tx.Rollback(ctx)

	gotAvail, _, _ := readProbeBalance(t, pool)
	if gotAvail != wantAvail {
		t.Fatalf("balance after mid-statement kill=%s, want %s — atomicity violated", gotAvail, wantAvail)
	}
}

// TestPGDrop_LockReleasedAndPoolRecovers proves a killed transaction does
// not leave a wedged row lock: a fresh transaction must acquire and
// complete a competing update promptly, and the pool keeps serving.
func TestPGDrop_LockReleasedAndPoolRecovers(t *testing.T) {
	pool := pgPool(t)
	setupProbeRow(t, pool, "3000.00000000")

	ctx := context.Background()

	// Tx A takes the row lock (FOR UPDATE-style via the real update),
	// then dies holding it.
	txA, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		t.Fatalf("begin A: %v", err)
	}
	var pidA int
	if err := txA.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pidA); err != nil {
		t.Fatalf("pid A: %v", err)
	}
	if _, err := txA.Exec(ctx, `
		UPDATE balances SET locked=locked+100
		WHERE account_id=$1 AND currency='USD'`, probeAccountID); err != nil {
		t.Fatalf("tx A lock update: %v", err)
	}
	terminateBackend(t, pool, pidA)
	_ = txA.Rollback(ctx)

	// Tx B must now acquire the same row and commit inside a tight
	// deadline — a leaked lock would stall it indefinitely.
	bctx, bcancel := context.WithTimeout(ctx, 5*time.Second)
	defer bcancel()
	txB, err := pool.BeginTx(bctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		t.Fatalf("begin B: %v", err)
	}
	if _, err := txB.Exec(bctx, `
		UPDATE balances SET available=available+50, version=version+1
		WHERE account_id=$1 AND currency='USD'`, probeAccountID); err != nil {
		t.Fatalf("tx B blocked by leaked lock or failed: %v", err)
	}
	if err := txB.Commit(bctx); err != nil {
		t.Fatalf("tx B commit: %v", err)
	}

	avail, locked, _ := readProbeBalance(t, pool)
	if avail != "3050.00000000" || locked != "0.00000000" {
		t.Fatalf("post-recovery balance (avail=%s, locked=%s), want 3050/0 — tx A's lock update leaked or tx B's commit lost",
			avail, locked)
	}

	// Pool health: N consecutive reads on fresh sessions.
	for i := 0; i < 3; i++ {
		var one int
		rctx, rcancel := context.WithTimeout(ctx, 5*time.Second)
		err := pool.QueryRow(rctx, `SELECT 1`).Scan(&one)
		rcancel()
		if err != nil {
			t.Fatalf("pool did not recover after backend kills: %v", err)
		}
	}
}
