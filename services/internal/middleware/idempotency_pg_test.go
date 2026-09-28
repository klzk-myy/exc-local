// Task 5.3.42 — PG-backed IdempotencyStore integration test.
//
// Gated: EXC_PG_TEST=1; DSN via EXC_PG_DSN / EXC_TEST_DSN (default: the
// scratch w2d database on the /tmp socket, port 55433). Requires the
// real schema — migration 154's idempotency_keys table, which carries a
// FK to accounts, so the test borrows the scratch account row.
package middleware

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func pgTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	if os.Getenv("EXC_PG_TEST") != "1" {
		t.Skip("set EXC_PG_TEST=1 to run PostgreSQL integration tests")
	}
	dsn := os.Getenv("EXC_PG_DSN")
	if dsn == "" {
		dsn = os.Getenv("EXC_TEST_DSN")
	}
	if dsn == "" {
		dsn = "postgres://postgres@/w2d?host=/tmp&port=55433"
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("pg pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func scratchAccount(t *testing.T, pool *pgxpool.Pool) int64 {
	t.Helper()
	var id int64
	if err := pool.QueryRow(context.Background(),
		`SELECT id FROM accounts ORDER BY id LIMIT 1`).Scan(&id); err != nil {
		t.Skipf("scratch DB has no accounts row: %v", err)
	}
	return id
}

func TestPGIdemStoreLifecycle(t *testing.T) {
	pool := pgTestPool(t)
	acct := scratchAccount(t, pool)
	ctx := context.Background()
	s := NewPGIdemStore(pool)

	// CHAR(64) column — hashes must be 64 chars (sha256 hex) in practice.
	pha := strings.Repeat("a", 64)
	phb := strings.Repeat("b", 64)
	phc := strings.Repeat("c", 64)
	phd := strings.Repeat("d", 64)

	key := IdemKey(acct, "018f8b7e-0001-7b2c-9d1e-2f3a4b5c6d7e")
	defer func() {
		_, _ = pool.Exec(ctx,
			`DELETE FROM idempotency_keys WHERE account_id=$1`, acct)
	}()

	res, err := s.Begin(ctx, key, "POST /api/v1/withdrawals", pha, time.Hour)
	if err != nil || res.State != IdemProceed {
		t.Fatalf("begin: %+v %v", res, err)
	}
	// Pending claim, same payload → in-flight.
	res, err = s.Begin(ctx, key, "POST /api/v1/withdrawals", pha, time.Hour)
	if err != nil || res.State != IdemInFlight {
		t.Fatalf("in-flight: %+v %v", res, err)
	}
	// Same key, different payload → mismatch.
	res, err = s.Begin(ctx, key, "POST /api/v1/withdrawals", phb, time.Hour)
	if err != nil || res.State != IdemMismatch {
		t.Fatalf("mismatch: %+v %v", res, err)
	}
	// Complete → replay returns the stored response verbatim.
	if err := s.Complete(ctx, key, pha, 201, "application/json",
		[]byte(`{"withdrawal_id":9}`)); err != nil {
		t.Fatalf("complete: %v", err)
	}
	res, err = s.Begin(ctx, key, "POST /api/v1/withdrawals", pha, time.Hour)
	if err != nil || res.State != IdemReplay {
		t.Fatalf("replay: %+v %v", res, err)
	}
	if res.HTTPStatus != 201 || string(res.Body) != `{"withdrawal_id":9}` {
		t.Fatalf("replay payload: %d %s", res.HTTPStatus, res.Body)
	}

	// status=0 releases the claim (transient-retry path).
	key2 := IdemKey(acct, "018f8b7e-0002-7b2c-9d1e-2f3a4b5c6d7e")
	if _, err := s.Begin(ctx, key2, "POST /api/v1/withdrawals", phc, time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := s.Complete(ctx, key2, phc, 0, "", nil); err != nil {
		t.Fatalf("release: %v", err)
	}
	res, err = s.Begin(ctx, key2, "POST /api/v1/withdrawals", phc, time.Hour)
	if err != nil || res.State != IdemProceed {
		t.Fatalf("re-begin after release: %+v %v", res, err)
	}

	// Non-JSON response body → wrapped raw replay.
	key3 := IdemKey(acct, "018f8b7e-0003-7b2c-9d1e-2f3a4b5c6d7e")
	if _, err := s.Begin(ctx, key3, "POST /x", phd, time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := s.Complete(ctx, key3, phd, 200, "text/plain",
		[]byte("plain body")); err != nil {
		t.Fatal(err)
	}
	res, err = s.Begin(ctx, key3, "POST /x", phd, time.Hour)
	if err != nil || res.State != IdemReplay ||
		string(res.Body) != "plain body" || res.ContentType != "text/plain" {
		t.Fatalf("raw replay: %+v %v", res, err)
	}
}
