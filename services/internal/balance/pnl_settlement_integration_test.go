// Integration test for PgxStore + journal construction against dev
// PostgreSQL.
//
// Gated: skipped unless EXC_PG_TEST=1. Target defaults to the dev database
// at localhost:5433 (docker-compose.dev.yml); override with EXC_PG_DSN.
//
// The JournalPoster here is a SQL stub that mimics settlement.LedgerService
// semantics (validates the journal, then applies wallet Effects to
// `balances`): the real poster additionally requires Redis + NATS +
// migrations 036/102 (ledger_entries/journal_sums), which is wired by the
// orchestrator.
//
// Run: EXC_PG_TEST=1 go test ./internal/balance/ -run Integration -v
package balance

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"exchange/internal/ledger"
	"exchange/internal/position"
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

// sqlPoster is a slim stand-in for settlement.LedgerService: validates the
// journal, writes real journal_entries + ledger_lines rows (so the
// account_code FK and the deferred zero-sum trigger DO fire against the
// seeded chart), then applies wallet Effects to `balances`. The real
// poster additionally takes Redis account locks, writes
// ledger_entries/journal_sums (migration 102) and dispatches
// BalanceChanged — wired by the orchestrator.
type sqlPoster struct {
	pool *pgxpool.Pool
}

func (p *sqlPoster) Post(ctx context.Context, j ledger.Journal) (ledger.PostResult, error) {
	if err := j.Validate(); err != nil {
		return ledger.PostResult{}, err
	}
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return ledger.PostResult{}, err
	}
	defer tx.Rollback(ctx)

	var jid int64
	err = tx.QueryRow(ctx,
		`INSERT INTO journal_entries (entry_type, reference_id, description, posted_by, idempotency_key)
		 VALUES ($1,$2,$3,$4,$5)
		 ON CONFLICT (idempotency_key) WHERE idempotency_key IS NOT NULL DO NOTHING
		 RETURNING id`,
		string(j.EntryType), j.ReferenceID, j.Description, j.PostedBy, j.IdempotencyKey).
		Scan(&jid)
	if errors.Is(err, pgx.ErrNoRows) {
		// idempotent replay — resolve to the committed journal
		if err := tx.Rollback(ctx); err != nil {
			return ledger.PostResult{}, err
		}
		var prev int64
		if err := p.pool.QueryRow(ctx,
			`SELECT id FROM journal_entries WHERE idempotency_key=$1`, j.IdempotencyKey).
			Scan(&prev); err != nil {
			return ledger.PostResult{}, err
		}
		return ledger.PostResult{JournalID: prev, Committed: true, Replayed: true}, nil
	}
	if err != nil {
		return ledger.PostResult{}, err
	}
	for _, l := range j.Lines {
		if _, err := tx.Exec(ctx,
			`INSERT INTO ledger_lines (journal_entry_id, account_code, debit_amount, credit_amount, currency, narrative)
			 VALUES ($1,$2,$3,$4,$5,$6)`,
			jid, l.AccountCode, l.Debit, l.Credit, l.Currency, l.Narrative); err != nil {
			return ledger.PostResult{}, err
		}
	}
	for _, e := range j.Effects {
		if _, err := tx.Exec(ctx,
			`INSERT INTO balances (account_id, currency, available, locked)
			 VALUES ($1,$2,$3,0)
			 ON CONFLICT (account_id, currency) DO UPDATE
			   SET available = balances.available + EXCLUDED.available,
			       version   = balances.version + 1`,
			e.AccountID, e.Currency, e.AvailableDelta); err != nil {
			return ledger.PostResult{}, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return ledger.PostResult{}, err
	}
	return ledger.PostResult{JournalID: jid, Committed: true}, nil
}

func seedAccountMode(t *testing.T, pool *pgxpool.Pool, base, mode string) int64 {
	t.Helper()
	ctx := context.Background()
	email := fmt.Sprintf("bal_it_%d@example.com", time.Now().UnixNano())
	var uid, aid int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO users (email, status) VALUES ($1,'ACTIVE') RETURNING id`, email).
		Scan(&uid); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO accounts (user_id, account_type, base_currency, pnl_settlement_mode)
		 VALUES ($1,'MARGIN',$2,$3) RETURNING id`, uid, base, mode).
		Scan(&aid); err != nil {
		t.Fatalf("seed account: %v", err)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		pool.Exec(ctx, `DELETE FROM currency_conversions WHERE account_id=$1`, aid)
		pool.Exec(ctx, `DELETE FROM balances WHERE account_id=$1`, aid)
		pool.Exec(ctx, `DELETE FROM accounts WHERE id=$1`, aid)
		pool.Exec(ctx, `DELETE FROM users WHERE id=$1`, uid)
	})
	return aid
}

// stubRatesIT serves GBP/USD 1.25 for the integration conversion.
type stubRatesIT struct{}

func (stubRatesIT) MidRate(_ context.Context, p position.Pair) (decimal.Decimal, error) {
	if p.Base == "GBP" && p.Quote == "USD" {
		return decimal.MustFromString("1.25"), nil
	}
	return decimal.Zero, position.ErrPairNotFound
}

func TestIntegration_SettleQuoteAndSweep(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	poster := &sqlPoster{pool: pool}
	conv := position.NewConverter(stubRatesIT{}, "")

	// QUOTE_CURRENCY account: GBP P&L books to GBP line.
	aid := seedAccountMode(t, pool, "USD", SettleQuoteCurrency)
	svc := NewSettlementService(NewPgxStore(pool), conv, poster)
	res, err := svc.SettleRealizedPnL(ctx, SettlementRequest{
		AccountID: aid, TradeID: time.Now().UnixNano(),
		QuoteCurrency: "GBP", RealizedPnL: decimal.MustFromString("42.5"),
	})
	if err != nil {
		t.Fatal(err)
	}
	var avail decimal.Decimal
	if err := pool.QueryRow(ctx,
		`SELECT available FROM balances WHERE account_id=$1 AND currency='GBP'`, aid).
		Scan(&avail); err != nil {
		t.Fatal(err)
	}
	if !avail.Equal(decimal.MustFromString("42.5")) || res.BookedCurrency != "GBP" {
		t.Fatalf("GBP balance=%s res=%+v", avail, res)
	}

	// SWEEP_TO_BASE account: GBP P&L converts to USD via stub oracle.
	aid2 := seedAccountMode(t, pool, "USD", SettleSweepToBase)
	svc2 := NewSettlementService(NewPgxStore(pool), conv, poster)
	trade2 := time.Now().UnixNano() + 1
	res2, err := svc2.SettleRealizedPnL(ctx, SettlementRequest{
		AccountID: aid2, TradeID: trade2,
		QuoteCurrency: "GBP", RealizedPnL: decimal.MustFromString("80"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx,
		`SELECT available FROM balances WHERE account_id=$1 AND currency='USD'`, aid2).
		Scan(&avail); err != nil {
		t.Fatal(err)
	}
	// GBP/USD = 1.25 → 80 GBP = 100 USD
	if !avail.Equal(decimal.MustFromString("100")) || !res2.Converted {
		t.Fatalf("USD balance=%s res=%+v", avail, res2)
	}
	var nConv int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM currency_conversions WHERE account_id=$1`, aid2).
		Scan(&nConv); err != nil {
		t.Fatal(err)
	}
	if nConv != 2 {
		t.Fatalf("expected 2 conversion rows (receipt+sweep), got %d", nConv)
	}

	// replay → duplicate, no double-book
	res3, err := svc2.SettleRealizedPnL(ctx, SettlementRequest{
		AccountID: aid2, TradeID: trade2,
		QuoteCurrency: "GBP", RealizedPnL: decimal.MustFromString("80"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res3.Duplicate {
		t.Fatal("replayed settlement must be flagged duplicate")
	}
	if err := pool.QueryRow(ctx,
		`SELECT available FROM balances WHERE account_id=$1 AND currency='USD'`, aid2).
		Scan(&avail); err != nil {
		t.Fatal(err)
	}
	if !avail.Equal(decimal.MustFromString("100")) {
		t.Fatalf("double-booked: USD=%s want 100", avail)
	}
}
