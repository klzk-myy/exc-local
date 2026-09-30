// PG-gated integration coverage for the Phase-24 nostro backoffice —
// gated on EXC_PG_TEST=1 / EXC_TEST_DSN (funding/integration_test.go
// convention: per-test schema + verbatim migration application).
package backoffice

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// boTestPool stands up a throwaway schema; every test that needs real
// PostgreSQL calls it first.
func boTestPool(t *testing.T) (context.Context, *pgxpool.Pool) {
	t.Helper()
	if os.Getenv("EXC_PG_TEST") != "1" {
		t.Skip("set EXC_PG_TEST=1 to run postgres-gated tests")
	}
	dsn := os.Getenv("EXC_TEST_DSN")
	if dsn == "" {
		dsn = "postgres://postgres:postgres@127.0.0.1:55433/postgres?sslmode=disable"
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pgxpool: %v", err)
	}
	schema := fmt.Sprintf("bo24_it_%d_%d", time.Now().UnixNano(), rand.Intn(1e6))
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	t.Cleanup(func() {
		admin.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE")
		admin.Close()
	})
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("dsn: %v", err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return ctx, pool
}

// execAll runs a DDL preamble plus verbatim migration files (in order)
// into the test schema.
func execAll(t *testing.T, ctx context.Context, pool *pgxpool.Pool, preamble string, files ...string) {
	t.Helper()
	if preamble != "" {
		if _, err := pool.Exec(ctx, preamble); err != nil {
			t.Fatalf("preamble: %v", err)
		}
	}
	for _, f := range files {
		body, err := os.ReadFile(filepath.Join("..", "db", "migrations", f))
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		if _, err := pool.Exec(ctx, string(body)); err != nil {
			t.Fatalf("apply %s: %v", f, err)
		}
	}
}

// boSchema is the migration chain the nostro backoffice reads/writes,
// with the funding_transactions/accounts stub parents 199 requires.
func boSchema(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	execAll(t, ctx, pool, `
		CREATE TABLE funding_transactions (
		    id BIGSERIAL PRIMARY KEY,
		    type VARCHAR(16) NOT NULL DEFAULT 'DEPOSIT',
		    status VARCHAR(24) NOT NULL DEFAULT 'PENDING',
		    review_deadline TIMESTAMPTZ);
		CREATE TABLE accounts (id BIGSERIAL PRIMARY KEY);`,
		"018_create_nostro_accounts.up.sql",
		"019_create_settlement_instructions.up.sql",
		"035_create_swift_messages.up.sql",
		"112_settlement_dispatch_and_nostro_movements.up.sql",
		"199_funding_flow_extensions.up.sql",
		"261_backoffice_nostro_recon.up.sql")
}

// seedLeg inserts one settlement instruction + its PENDING nostro
// movement — the Phase-03 rows the poster consumes.
func seedLeg(t *testing.T, ctx context.Context, pool *pgxpool.Pool,
	acctID int64, ccy, amount, direction, swiftRef string) (legID, movID int64) {
	t.Helper()
	dir := "RECEIVE"
	if direction == "DEBIT" {
		dir = "PAY"
	}
	if err := pool.QueryRow(ctx, `
		INSERT INTO settlement_instructions
		    (trade_id, account_id, currency, amount, direction,
		     settlement_date, nostro_account_id, swift_message_id,
		     dispatched_at)
		VALUES (1,1,$1,$2::numeric,$3,now()::date,$4,$5,now())
		RETURNING id`,
		ccy, amount, dir, acctID, swiftRef).Scan(&legID); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `
		INSERT INTO nostro_movements
		    (settlement_instruction_id, nostro_account_id, currency,
		     amount, direction)
		VALUES ($1,$2,$3,$4::numeric,$5) RETURNING id`,
		legID, acctID, ccy, amount, direction).Scan(&movID); err != nil {
		t.Fatal(err)
	}
	return legID, movID
}

// TestPgNostroStore_Integration — real PG: registry dedup, movement
// posting (credit/debit), idempotent replay, the P1 overdrawn alert.
func TestPgNostroStore_Integration(t *testing.T) {
	ctx, pool := boTestPool(t)
	boSchema(t, ctx, pool)

	store := NewPgNostroStore(pool)
	svc, err := NewNostroService(store)
	if err != nil {
		t.Fatal(err)
	}

	a, err := svc.CreateAccount(ctx, CreateAccountInput{
		Currency: "USD", BankName: "Deutsche Bank", BankCode: "DEUTDEFF",
		AccountNumber: "100-1", Role: RoleNostro})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := svc.CreateAccount(ctx, CreateAccountInput{
		Currency: "USD", BankName: "Deutsche Bank", BankCode: "DEUTDEFF",
		AccountNumber: "100-1"}); err == nil {
		t.Fatal("dedup index must reject the same account twice")
	}
	if _, err := svc.CreateAccount(ctx, CreateAccountInput{
		Currency: "USD", BankName: "Deutsche Bank", BankCode: "DEUTDEFF",
		AccountNumber: "100-1", Role: RoleVostro}); err == nil {
		t.Fatal("vostro twin at the same locator must also dedup")
	}

	_, movID := seedLeg(t, ctx, pool, a.ID, "USD", "250.5", "CREDIT", "REF-1")
	res, err := svc.PostMovement(ctx, movID)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Posted || res.BalanceAfter.String() != "250.5" {
		t.Fatalf("post: %+v", res)
	}
	res, err = svc.PostMovement(ctx, movID) // replay
	if err != nil || res.Posted {
		t.Fatalf("replay must no-op: %+v %v", res, err)
	}

	_, mov2 := seedLeg(t, ctx, pool, a.ID, "USD", "900", "DEBIT", "REF-2")
	res, err = svc.PostMovement(ctx, mov2)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Posted || !res.Overdrawn {
		t.Fatalf("overdrawn post: %+v", res)
	}
	var code string
	if err := pool.QueryRow(ctx, `
		SELECT code FROM funding_ops_alerts
		 WHERE code = 'NOSTRO_OVERDRAWN' AND severity = 'P1'
		 ORDER BY id DESC LIMIT 1`).Scan(&code); err != nil {
		t.Fatalf("durable P1 overdrawn alert missing: %v", err)
	}

	accts, err := svc.ListAccounts(ctx, AccountFilter{Currency: "USD"})
	if err != nil || len(accts) != 1 {
		t.Fatalf("list: %+v %v", accts, err)
	}
	if accts[0].Balance.String() != "-649.5" {
		t.Fatalf("balance = %s", accts[0].Balance)
	}
}
