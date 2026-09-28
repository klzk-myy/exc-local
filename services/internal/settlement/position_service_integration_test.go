// Integration test for PgxPositionStore against dev PostgreSQL.
//
// Gated: skipped unless EXC_PG_TEST=1. Target defaults to the dev database
// at localhost:5433 (docker-compose.dev.yml); override with EXC_PG_DSN.
//
// Run: EXC_PG_TEST=1 go test ./internal/settlement/ -run Integration -v
package settlement

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"exchange/pkg/decimal"
)

func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	if os.Getenv("EXC_PG_TEST") != "1" {
		t.Skip("set EXC_PG_TEST=1 to run PostgreSQL integration tests")
	}
	dsn := os.Getenv("EXC_PG_DSN")
	if dsn == "" {
		dsn = "postgres://exchange:exchange_dev@localhost:5433/exchange?sslmode=disable"
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Skipf("postgres unreachable: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Skipf("postgres unreachable: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// seedAccount inserts a throwaway user+account and returns the account id.
func seedAccount(t *testing.T, pool *pgxpool.Pool) int64 {
	t.Helper()
	ctx := context.Background()
	email := fmt.Sprintf("pos_it_%d@example.com", time.Now().UnixNano())
	var uid, aid int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO users (email, status) VALUES ($1,'ACTIVE') RETURNING id`, email).
		Scan(&uid); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO accounts (user_id, account_type) VALUES ($1,'MARGIN') RETURNING id`, uid).
		Scan(&aid); err != nil {
		t.Fatalf("seed account: %v", err)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		pool.Exec(ctx, `DELETE FROM position_fills WHERE account_id=$1`, aid)
		pool.Exec(ctx, `DELETE FROM positions WHERE account_id=$1`, aid)
		pool.Exec(ctx, `DELETE FROM currency_conversions WHERE account_id=$1`, aid)
		pool.Exec(ctx, `DELETE FROM balances WHERE account_id=$1`, aid)
		pool.Exec(ctx, `DELETE FROM accounts WHERE id=$1`, aid)
		pool.Exec(ctx, `DELETE FROM users WHERE id=$1`, uid)
	})
	return aid
}

func TestIntegration_PositionLifecycle(t *testing.T) {
	pool := testPool(t)
	aid := seedAccount(t, pool)
	svc := NewPositionService(NewPgxPositionStore(pool), nil, 0)
	ctx := context.Background()

	instr := int64(1) // EUR/USD seed row
	tradeID := uint64(time.Now().UnixNano())
	open, err := svc.ProcessFill(ctx, PositionFill{
		TradeID: tradeID, AccountID: aid, InstrumentID: instr,
		Side: FillBuy, Price: decimal.MustFromString("1.1000"), Quantity: decimal.MustFromString("10000"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if open.Action != "OPENED" || !open.Position.EntryPrice.Equal(decimal.MustFromString("1.1")) {
		t.Fatalf("open: %+v", open.Position)
	}

	close, err := svc.ProcessFill(ctx, PositionFill{
		TradeID: tradeID + 1, AccountID: aid, InstrumentID: instr,
		Side: FillSell, Price: decimal.MustFromString("1.1200"), Quantity: decimal.MustFromString("4000"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if close.Action != "REDUCED" || !close.RealizedPnLDelta.Equal(decimal.MustFromString("80")) {
		t.Fatalf("close: %+v realized=%s", close.Position, close.RealizedPnLDelta)
	}

	// persisted state check
	var qty, realized, unreal decimal.Decimal
	var mark *decimal.Decimal
	err = pool.QueryRow(ctx,
		`SELECT quantity, realized_pnl, unrealized_pnl, mark_price FROM positions
		  WHERE account_id=$1 AND instrument_id=$2`, aid, instr).
		Scan(&qty, &realized, &unreal, &mark)
	if err != nil {
		t.Fatal(err)
	}
	if !qty.Equal(decimal.MustFromString("6000")) || !realized.Equal(decimal.MustFromString("80")) {
		t.Fatalf("persisted qty=%s realized=%s", qty, realized)
	}
	// mark = last trade price 1.12 → unrealized = 6000*(1.12-1.10)=120
	if mark == nil || !unreal.Equal(decimal.MustFromString("120")) {
		t.Fatalf("unrealized=%s mark=%v", unreal, mark)
	}

	// duplicate replay is a no-op
	dup, err := svc.ProcessFill(ctx, PositionFill{
		TradeID: tradeID, AccountID: aid, InstrumentID: instr,
		Side: FillBuy, Price: decimal.MustFromString("1.1"), Quantity: decimal.MustFromString("1"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !dup.Duplicate {
		t.Fatal("replayed fill must be flagged duplicate")
	}
}
