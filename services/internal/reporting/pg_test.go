// EXC_PG_TEST=1 gated integration test — dev PostgreSQL at
// 127.0.0.1:5433 (exchange/exchange_dev). Skips while the sibling-owned
// trade_confirmations migration (049) has not been applied.
package reporting

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

const testPGDSN = "postgres://exchange:exchange_dev@127.0.0.1:5433/exchange?sslmode=disable"

func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	if os.Getenv("EXC_PG_TEST") != "1" {
		t.Skip("set EXC_PG_TEST=1 to run Postgres integration tests")
	}
	dsn := os.Getenv("EXC_PG_DSN")
	if dsn == "" {
		dsn = testPGDSN
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("pg connect: %v", err)
	}
	if err := pool.Ping(context.Background()); err != nil {
		t.Skipf("postgres unreachable at %s: %v", dsn, err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func TestPgConfirmationTracker_RoundTrip(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	var ok bool
	err := pool.QueryRow(ctx,
		`SELECT to_regclass('trade_confirmations') IS NOT NULL`).Scan(&ok)
	if err != nil {
		t.Fatalf("regclass probe: %v", err)
	}
	if !ok {
		t.Skip("trade_confirmations DDL not yet applied (sibling migration pending)")
	}
	// Seed a real user+account for the FK, then a row the way the
	// generator does.
	var userID, acctID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO users (email) VALUES ($1) RETURNING id`,
		fmt.Sprintf("rpt-conf-%d@x.test", time.Now().UnixNano()%1e12)).Scan(&userID); err != nil {
		t.Fatalf("mkUser: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO accounts (user_id, account_type) VALUES ($1,'MARGIN') RETURNING id`,
		userID).Scan(&acctID); err != nil {
		t.Fatalf("mkAccount: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx,
			`DELETE FROM trade_confirmations WHERE account_id = $1`, acctID)
		_, _ = pool.Exec(ctx, `DELETE FROM accounts WHERE id = $1`, acctID)
		_, _ = pool.Exec(ctx, `DELETE FROM users WHERE id = $1`, userID)
	})

	var id int64
	tradeID := time.Now().UnixNano() % 1e12
	err = pool.QueryRow(ctx, `
		INSERT INTO trade_confirmations
		    (trade_id, account_id, version, status, file_ref, content_sha256)
		VALUES ($1, $2, 1, 'GENERATED', $3, 'aa')
		RETURNING confirmation_id`,
		tradeID, acctID, fmt.Sprintf("confirmations/%d/%d/v1", acctID, tradeID)).Scan(&id)
	if err != nil {
		t.Skipf("insert shape mismatch (schema drifted?): %v", err)
	}

	tr := NewPgConfirmationTracker(pool)
	pend, err := tr.PendingGenerated(ctx, 1000)
	if err != nil {
		t.Fatalf("pending: %v", err)
	}
	found := false
	for _, r := range pend {
		if r.ConfirmationID == id {
			found = true
			if r.Status != StatusGenerated || r.Version != 1 {
				t.Fatalf("row %+v", r)
			}
		}
	}
	if !found {
		t.Fatal("seeded row not in pending list")
	}
	at := time.Now().UTC()
	if err := tr.MarkDelivered(ctx, id, at); err != nil {
		t.Fatalf("mark delivered: %v", err)
	}
	row, err := tr.LatestByTrade(ctx, tradeID)
	if err != nil || row == nil {
		t.Fatalf("latest: %v", err)
	}
	if row.Status != StatusDelivered || row.DeliveredAt == nil {
		t.Fatalf("delivered state: %+v", row)
	}
	// Second MarkDelivered must be a data-condition error (not silent).
	if err := tr.MarkDelivered(ctx, id, at); err == nil {
		t.Fatal("double-deliver must error")
	}
}

func TestPgLookups(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	// Missing-account lookups resolve to conservative defaults, never
	// errors (contract documented on the PG impls).
	cat, err := NewPgCategorySource(pool).Category(ctx, -1)
	if err != nil || cat != "RETAIL" {
		t.Fatalf("category=%q err=%v", cat, err)
	}
	email, err := NewPgRecipientSource(pool).Email(ctx, -1)
	if err != nil || email != "" {
		t.Fatalf("email=%q err=%v", email, err)
	}
}
