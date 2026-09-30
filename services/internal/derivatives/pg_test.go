// pg_test.go — PostgreSQL-backed coverage for PgxContractStore against
// migration 254. Gated on EXC_PG_TEST=1 + EXC_TEST_DSN; runs in a
// scratch schema per internal/compliance/integration_test.go convention.
package derivatives

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"exchange/pkg/decimal"
)

// pgStore builds a PgxContractStore on a throwaway schema with the
// migration-254 anchors (accounts/instruments/settlement_instructions).
func pgStore(t *testing.T) *PgxContractStore {
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
	schema := fmt.Sprintf("deriv_it_%d_%d", time.Now().UnixNano(), rand.Intn(1e6))
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

	// Minimal anchors the migration references (mirrors migrations
	// 003/001/019/112 shapes — only the columns this package touches).
	if _, err := pool.Exec(ctx, `
		CREATE TABLE accounts (id BIGSERIAL PRIMARY KEY);
		CREATE TABLE instruments (id BIGSERIAL PRIMARY KEY);
		CREATE TYPE settlement_direction_enum AS ENUM ('PAY','RECEIVE');
		CREATE TYPE settlement_status_enum    AS ENUM ('PENDING','SETTLED','FAILED','RECONCILED');
		CREATE TABLE settlement_instructions (
		    id              BIGSERIAL PRIMARY KEY,
		    trade_id        BIGINT NOT NULL,
		    account_id      BIGINT NOT NULL,
		    currency        VARCHAR(3) NOT NULL,
		    amount          DECIMAL(28,8) NOT NULL,
		    direction       settlement_direction_enum NOT NULL,
		    settlement_date DATE,
		    status          settlement_status_enum NOT NULL DEFAULT 'PENDING',
		    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
		    settled_at      TIMESTAMPTZ
		);
		CREATE UNIQUE INDEX settlement_instructions_leg_ux
		    ON settlement_instructions (trade_id, account_id, currency, direction);
		INSERT INTO accounts (id) VALUES (7);
		INSERT INTO instruments (id) VALUES (42),(44);
	`); err != nil {
		t.Fatalf("anchors: %v", err)
	}
	up, err := os.ReadFile(filepath.Join("..", "db", "migrations", "254_derivative_contracts.up.sql"))
	if err != nil {
		t.Fatalf("read migration: %v", err)
	}
	if _, err := pool.Exec(ctx, string(up)); err != nil {
		t.Fatalf("apply migration: %v", err)
	}
	return NewPgxContractStore(pool)
}

func TestPgxContractStoreRoundTrip(t *testing.T) {
	store := pgStore(t)
	ctx := context.Background()
	pair, err := NewPair("EUR", "USD")
	if err != nil {
		t.Fatalf("pair: %v", err)
	}
	one := decimal.RequireFromString("1.10")
	fwd := decimal.RequireFromString("1.095")
	vd := day(2026, 2, 10)
	spot := day(2026, 1, 9)
	var id int64
	err = store.InTx(ctx, func(ctx context.Context, tx ContractTx) error {
		got, created, err := tx.InsertContract(ctx, &Contract{
			TradeID: 5001, AccountID: 7, InstrumentID: 42,
			Kind: KindForward, Side: SideBuy, Pair: pair,
			Notional: decimal.RequireFromString("1000000"),
			SpotRate: one, ForwardRate: fwd, SwapPoints: fwd.Sub(one),
			SpotValueDate:  spot,
			ValueDate:      vd,
			Status:         StatusOpen,
			IdempotencyKey: "itest:1",
		})
		if err != nil {
			return err
		}
		if !created {
			return fmt.Errorf("first insert must create")
		}
		id = got
		_, err = tx.InsertLegs(ctx, []SettlementLeg{
			{ContractID: id, TradeID: 5001, AccountID: 7, Tag: LegTagFar,
				Currency: "EUR", Amount: decimal.RequireFromString("1000000"),
				Direction: LegReceive, ValueDate: vd},
			{ContractID: id, TradeID: 5001, AccountID: 7, Tag: LegTagFar,
				Currency: "USD", Amount: fwd.Mul(decimal.RequireFromString("1000000")).Round(8),
				Direction: LegPay, ValueDate: vd},
		})
		return err
	})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	err = store.InTx(ctx, func(ctx context.Context, tx ContractTx) error {
		got, err := tx.ContractForUpdate(ctx, id)
		if err != nil {
			return err
		}
		if got.Pair.Symbol() != "EUR/USD" || !got.ForwardRate.Equal(fwd) {
			return fmt.Errorf("round trip: %+v", got)
		}
		legs, err := tx.ContractLegs(ctx, id)
		if err != nil {
			return err
		}
		if len(legs) != 2 {
			return fmt.Errorf("legs=%d want 2", len(legs))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	// Fill replay: same idempotency key returns the existing row.
	err = store.InTx(ctx, func(ctx context.Context, tx ContractTx) error {
		rid, created, err := tx.InsertContract(ctx, &Contract{
			TradeID: 5001, AccountID: 7, InstrumentID: 42,
			Kind: KindForward, Side: SideBuy, Pair: pair,
			Notional: decimal.RequireFromString("1000000"),
			SpotRate: one, ForwardRate: fwd, SwapPoints: fwd.Sub(one),
			SpotValueDate: spot, ValueDate: vd,
			Status: StatusOpen, IdempotencyKey: "itest:1",
		})
		if err != nil {
			return err
		}
		if created || rid != id {
			return fmt.Errorf("replay created=%v rid=%d want %d", created, rid, id)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("idem: %v", err)
	}
}

func TestPgxFixingOutcomeAtomic(t *testing.T) {
	store := pgStore(t)
	ctx := context.Background()
	pair, _ := NewPair("USD", "BRL")
	fd := day(2026, 1, 15)
	var id int64
	err := store.InTx(ctx, func(ctx context.Context, tx ContractTx) error {
		var err error
		id, _, err = tx.InsertContract(ctx, &Contract{
			TradeID: 5002, AccountID: 7, InstrumentID: 44,
			Kind: KindNDF, Side: SideBuy, Pair: pair,
			Notional:    decimal.RequireFromString("1000"),
			SpotRate:    decimal.RequireFromString("5.00"),
			ForwardRate: decimal.RequireFromString("5.10"),
			SwapPoints:  decimal.RequireFromString("0.10"),
			ValueDate:   fd, SpotValueDate: day(2026, 1, 9),
			NdfFixingDate:      &fd,
			NdfFixingSource:    "CENTRAL_BANK:PTAX",
			SettlementCurrency: "USD",
			Status:             StatusOpen,
		})
		return err
	})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	// Fixing observation + outcome stamp.
	err = store.InTx(ctx, func(ctx context.Context, tx ContractTx) error {
		if _, err := tx.InsertNdfFixing(ctx, &NdfFixing{
			ContractID: id, FixingDate: fd, Source: "CENTRAL_BANK:PTAX",
			Rate: decimal.RequireFromString("5.25"),
		}); err != nil {
			return err
		}
		if err := tx.SetContractFixing(ctx, id, decimal.RequireFromString("5.25")); err != nil {
			return err
		}
		return tx.SetContractOutcome(ctx, id,
			decimal.RequireFromString("5.25"),
			decimal.RequireFromString("28571.42857143"),
			StatusSettled, time.Now())
	})
	if err != nil {
		t.Fatalf("outcome: %v", err)
	}
	// Re-fix on a settled contract must conflict.
	err = store.InTx(ctx, func(ctx context.Context, tx ContractTx) error {
		return tx.SetContractFixing(ctx, id, decimal.RequireFromString("5.30"))
	})
	if codeOf(t, err) != CodeDerivativeStateConflict {
		t.Fatalf("re-fix: %v", err)
	}
}
