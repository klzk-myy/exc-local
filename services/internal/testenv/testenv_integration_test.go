// Task 5.3.13 — PG-backed reset test. Gated: EXC_PG_TEST=1, DSN via
// EXC_PG_DSN or EXC_TEST_DSN (default: scratch w2d on the /tmp socket, port 55433).
//
// The reset sweeps ~20 tables across many real migrations, several of
// which depend on pg_partman / extensions the scratch server lacks —
// so this test mirrors minimal DDL (identical column names/types for
// the fields the reset references, no FK graph — deletion order is
// what is under test). The webhook tables come from the real 180
// migration.
//
// Run: EXC_PG_TEST=1 go test ./internal/testenv/ -run Integration -v
package testenv

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func testDSN() string {
	if d := os.Getenv("EXC_PG_DSN"); d != "" {
		return d
	}
	if d := os.Getenv("EXC_TEST_DSN"); d != "" {
		return d
	}
	return "postgres://postgres@/w2d?host=/tmp&port=55433"
}

func itest(t *testing.T) (*Service, *pgxpool.Pool, context.Context) {
	t.Helper()
	if os.Getenv("EXC_PG_TEST") != "1" {
		t.Skip("set EXC_PG_TEST=1 to run PostgreSQL integration tests")
	}
	ctx := context.Background()
	schema := fmt.Sprintf("testenv_itest_%d", time.Now().UnixNano())

	cfg, err := pgx.ParseConfig(testDSN())
	if err != nil {
		t.Fatalf("dsn: %v", err)
	}
	cfg.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol
	cfg.RuntimeParams["search_path"] = schema + ",public"
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Skipf("postgres unreachable: %v", err)
	}
	if _, err := conn.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		conn.Close(ctx)
		t.Fatalf("create schema: %v", err)
	}
	ddl := `
	CREATE TABLE carry_trade_allocations (id BIGINT PRIMARY KEY, account_id BIGINT NOT NULL);
	CREATE TABLE positions (id BIGINT PRIMARY KEY, account_id BIGINT NOT NULL);
	CREATE TABLE carry_trade_legs (id BIGINT PRIMARY KEY, position_id BIGINT NOT NULL);
	CREATE TABLE journal_entries (id BIGINT PRIMARY KEY);
	CREATE TABLE carry_yield_records (id BIGINT PRIMARY KEY, allocation_id BIGINT NOT NULL, journal_entry_id BIGINT);
	CREATE TABLE carry_yield_totals (id BIGINT PRIMARY KEY, allocation_id BIGINT NOT NULL);
	CREATE TABLE swap_free_admin_fee_assessments (id BIGINT PRIMARY KEY, account_id BIGINT NOT NULL, journal_entry_id BIGINT);
	CREATE TABLE swap_accrual_records (id BIGINT PRIMARY KEY, account_id BIGINT NOT NULL, journal_entry_id BIGINT);
	CREATE TABLE currency_conversions (id BIGINT PRIMARY KEY, account_id BIGINT NOT NULL, journal_entry_id BIGINT);
	CREATE TABLE dust_sweeps (id BIGINT PRIMARY KEY, account_id BIGINT NOT NULL, journal_entry_id BIGINT);
	CREATE TABLE position_fills (id BIGINT PRIMARY KEY, account_id BIGINT NOT NULL);
	CREATE TABLE orders (id BIGINT PRIMARY KEY, account_id BIGINT NOT NULL);
	CREATE TABLE trades (id BIGINT PRIMARY KEY, buyer_account_id BIGINT NOT NULL, seller_account_id BIGINT NOT NULL);
	CREATE TABLE processed_trades (id BIGINT PRIMARY KEY, trade_id BIGINT NOT NULL);
	CREATE TABLE funding_transactions (id BIGINT PRIMARY KEY, account_id BIGINT NOT NULL);
	CREATE TABLE withdrawal_confirmations (id BIGINT PRIMARY KEY, withdrawal_id BIGINT NOT NULL);
	CREATE TABLE chargebacks (id BIGINT PRIMARY KEY, account_id BIGINT NOT NULL);
	CREATE TABLE transfers (id BIGINT PRIMARY KEY, account_id BIGINT, from_account_id BIGINT, to_account_id BIGINT);
	CREATE TABLE webhook_endpoints (id BIGINT PRIMARY KEY, account_id BIGINT NOT NULL);
	CREATE TABLE webhook_deliveries (id BIGINT PRIMARY KEY, account_id BIGINT NOT NULL);
	CREATE TABLE journal_sums (id BIGINT PRIMARY KEY, account_id BIGINT NOT NULL, last_entry_id BIGINT);
	CREATE TABLE ledger_entries (id BIGINT PRIMARY KEY, account_id BIGINT NOT NULL, journal_entry_id BIGINT);
	CREATE TABLE ledger_lines (id BIGINT PRIMARY KEY, journal_entry_id BIGINT NOT NULL);
	CREATE TABLE balances (account_id BIGINT NOT NULL, currency VARCHAR(3) NOT NULL,
	    available DECIMAL(28,8) NOT NULL DEFAULT 0, locked DECIMAL(28,8) NOT NULL DEFAULT 0,
	    version BIGINT NOT NULL DEFAULT 0, PRIMARY KEY (account_id, currency));`
	if _, err := conn.Exec(ctx, ddl); err != nil {
		conn.Close(ctx)
		t.Fatalf("ddl: %v", err)
	}
	conn.Close(ctx)

	poolCfg, err := pgxpool.ParseConfig(testDSN())
	if err != nil {
		t.Fatalf("pool dsn: %v", err)
	}
	poolCfg.ConnConfig.RuntimeParams["search_path"] = schema + ",public"
	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		t.Skipf("postgres unreachable: %v", err)
	}
	t.Cleanup(func() {
		pool.Close()
		c, err := pgx.Connect(ctx, testDSN())
		if err == nil {
			_, _ = c.Exec(ctx, "DROP SCHEMA IF EXISTS "+schema+" CASCADE")
			c.Close(ctx)
		}
	})
	return New(pool, "test"), pool, ctx
}

func count(t *testing.T, ctx context.Context, pool *pgxpool.Pool, q string, args ...any) int64 {
	t.Helper()
	var n int64
	if err := pool.QueryRow(ctx, q, args...).Scan(&n); err != nil {
		t.Fatalf("count %q: %v", q, err)
	}
	return n
}

// Account 1 is the reset target; account 2 is the counterparty that
// shares a journal entry and owns its own state — it must survive.
func TestIntegrationResetAccount(t *testing.T) {
	svc, pool, ctx := itest(t)

	seed := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, q, args...); err != nil {
			t.Fatalf("seed %q: %v", q, err)
		}
	}
	// Shared journal: referenced by ledger rows of BOTH accounts.
	seed(`INSERT INTO journal_entries (id) VALUES (100)`)
	seed(`INSERT INTO journal_entries (id) VALUES (101)`)
	seed(`INSERT INTO ledger_lines (id, journal_entry_id) VALUES (1, 100)`)
	seed(`INSERT INTO ledger_entries (id, account_id, journal_entry_id) VALUES (1, 1, 100)`)
	seed(`INSERT INTO ledger_entries (id, account_id, journal_entry_id) VALUES (2, 2, 100)`)
	// Journal 101 referenced only by account 1 → must be deleted.
	seed(`INSERT INTO ledger_entries (id, account_id, journal_entry_id) VALUES (3, 1, 101)`)
	seed(`INSERT INTO ledger_lines (id, journal_entry_id) VALUES (2, 101)`)
	// Market state for account 1; account 2 has its own order.
	seed(`INSERT INTO orders (id, account_id) VALUES (10, 1)`)
	seed(`INSERT INTO orders (id, account_id) VALUES (11, 2)`)
	seed(`INSERT INTO positions (id, account_id) VALUES (20, 1)`)
	seed(`INSERT INTO position_fills (id, account_id) VALUES (30, 1)`)
	// Trade between account 1 and account 2 — either-side delete.
	seed(`INSERT INTO trades (id, buyer_account_id, seller_account_id) VALUES (40, 1, 2)`)
	seed(`INSERT INTO trades (id, buyer_account_id, seller_account_id) VALUES (41, 2, 3)`)
	seed(`INSERT INTO processed_trades (id, trade_id) VALUES (50, 40)`)
	seed(`INSERT INTO processed_trades (id, trade_id) VALUES (51, 41)`)
	// Funding + webhooks.
	seed(`INSERT INTO funding_transactions (id, account_id) VALUES (60, 1)`)
	seed(`INSERT INTO withdrawal_confirmations (id, withdrawal_id) VALUES (61, 60)`)
	seed(`INSERT INTO chargebacks (id, account_id) VALUES (62, 1)`)
	seed(`INSERT INTO transfers (id, account_id, from_account_id, to_account_id) VALUES (63, 2, 1, 2)`)
	seed(`INSERT INTO webhook_endpoints (id, account_id) VALUES (70, 1)`)
	seed(`INSERT INTO webhook_deliveries (id, account_id) VALUES (71, 1)`)
	// Balances.
	seed(`INSERT INTO balances (account_id, currency, available, locked) VALUES (1, 'USD', 1000, 50)`)
	seed(`INSERT INTO balances (account_id, currency, available, locked) VALUES (2, 'USD', 500, 0)`)

	counts, err := svc.ResetAccount(ctx, 1)
	if err != nil {
		t.Fatalf("reset: %v", err)
	}
	for _, tbl := range []string{"orders", "positions", "position_fills",
		"trades", "funding_transactions", "webhook_endpoints",
		"webhook_deliveries", "ledger_entries"} {
		if counts[tbl] == 0 {
			t.Fatalf("reset[%s]=0 — expected rows deleted", tbl)
		}
	}

	// Account 1's rows gone.
	for _, q := range []string{
		`SELECT count(*) FROM orders WHERE account_id=1`,
		`SELECT count(*) FROM positions WHERE account_id=1`,
		`SELECT count(*) FROM funding_transactions WHERE account_id=1`,
		`SELECT count(*) FROM webhook_endpoints WHERE account_id=1`,
		`SELECT count(*) FROM ledger_entries WHERE account_id=1`,
		`SELECT count(*) FROM trades WHERE id=40`, // either-side delete
	} {
		if n := count(t, ctx, pool, q); n != 0 {
			t.Fatalf("%s → %d rows remain", q, n)
		}
	}
	// Counterparty rows survive.
	if n := count(t, ctx, pool, `SELECT count(*) FROM orders WHERE account_id=2`); n != 1 {
		t.Fatal("counterparty order deleted")
	}
	if n := count(t, ctx, pool, `SELECT count(*) FROM ledger_entries WHERE account_id=2`); n != 1 {
		t.Fatal("counterparty ledger row deleted")
	}
	if n := count(t, ctx, pool, `SELECT count(*) FROM trades WHERE id=41`); n != 1 {
		t.Fatal("foreign trade deleted")
	}
	if n := count(t, ctx, pool, `SELECT count(*) FROM processed_trades WHERE trade_id=41`); n != 1 {
		t.Fatal("foreign processed_trade deleted")
	}
	// Shared journal survives; unreferenced journal is gone.
	if n := count(t, ctx, pool, `SELECT count(*) FROM journal_entries WHERE id=100`); n != 1 {
		t.Fatal("shared journal 100 deleted")
	}
	if n := count(t, ctx, pool, `SELECT count(*) FROM journal_entries WHERE id=101`); n != 0 {
		t.Fatal("orphaned journal 101 survived")
	}
	// Balances re-zeroed (not deleted).
	var avail, locked string
	if err := pool.QueryRow(ctx,
		`SELECT available::text, locked::text FROM balances WHERE account_id=1 AND currency='USD'`).
		Scan(&avail, &locked); err != nil {
		t.Fatalf("balance read: %v", err)
	}
	if avail != "0.00000000" && avail != "0" {
		t.Fatalf("available=%s want 0", avail)
	}
	if n := count(t, ctx, pool,
		`SELECT count(*) FROM balances WHERE account_id=2 AND available=500`); n != 1 {
		t.Fatal("counterparty balance touched")
	}

	// Cooldown is armed: an immediate second reset is rate-limited.
	if _, err := svc.ResetAccount(ctx, 1); err == nil {
		t.Fatal("second reset inside cooldown accepted")
	}
}
