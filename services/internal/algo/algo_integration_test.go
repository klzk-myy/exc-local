// PG-gated integration coverage for the algo framework — runs only when
// EXC_PG_TEST=1. Applies migration 224 into a throwaway schema.
//
//	EXC_PG_TEST=1 go test ./internal/algo/ -run TestIT -v
package algo

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"exchange/pkg/decimal"
)

func json224(s string) json.RawMessage { return json.RawMessage(s) }

func itPool(t *testing.T) (context.Context, *pgxpool.Pool) {
	t.Helper()
	if os.Getenv("EXC_PG_TEST") != "1" {
		t.Skip("set EXC_PG_TEST=1 to run postgres-gated tests")
	}
	dsn := os.Getenv("EXC_TEST_DSN")
	if dsn == "" {
		dsn = "postgres://exchange:exchange_dev@127.0.0.1:5433/exchange?sslmode=disable"
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pgxpool: %v", err)
	}
	schema := fmt.Sprintf("algo_it_%d_%d", time.Now().UnixNano(), rand.Intn(1e6))
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

func execSQL(t *testing.T, ctx context.Context, pool *pgxpool.Pool, name string) {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("..", "db", "migrations", name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	if _, err := pool.Exec(ctx, string(body)); err != nil {
		t.Fatalf("exec %s: %v", name, err)
	}
}

// Minimal fixtures satisfying migration 224's FKs (accounts, orders) plus
// the instruments/trades tables the read seams query.
const itFixture = `
CREATE TABLE accounts (id BIGSERIAL PRIMARY KEY);
CREATE TABLE instruments (
    id BIGSERIAL PRIMARY KEY, symbol VARCHAR(32) UNIQUE NOT NULL,
    pip_size DECIMAL(10,8) NOT NULL DEFAULT 0.0001);
CREATE TABLE orders (
    id BIGSERIAL PRIMARY KEY, account_id BIGINT NOT NULL,
    instrument_id BIGINT NOT NULL REFERENCES instruments(id),
    side VARCHAR(4) NOT NULL, price DECIMAL(20,8), quantity DECIMAL(28,8),
    filled_qty DECIMAL(28,8) NOT NULL DEFAULT 0,
    status VARCHAR(20) NOT NULL DEFAULT 'ACTIVE');
CREATE TABLE trades (
    id BIGSERIAL PRIMARY KEY, instrument_id BIGINT NOT NULL,
    price DECIMAL(20,8), quantity DECIMAL(28,8),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now());
INSERT INTO accounts (id) VALUES (1);
INSERT INTO instruments (symbol, pip_size) VALUES ('EUR/USD', 0.0001);
`

func setupIT(t *testing.T) (context.Context, *pgxpool.Pool, *PgStore) {
	ctx, pool := itPool(t)
	if _, err := pool.Exec(ctx, itFixture); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	execSQL(t, ctx, pool, "224_algo_orders.up.sql")
	return ctx, pool, NewPgStore(pool)
}

func itEngine(t *testing.T, ctx context.Context, pool *pgxpool.Pool,
	fs *PgStore, fe *fakeExec) *Engine {
	e, err := NewEngine(Options{
		Store: fs, Exec: fe,
		Quote: NewPgTopOfBook(pool), Ref: NewPgRefPrice(pool),
		Pips: NewPgPipSize(pool),
	})
	if err != nil {
		t.Fatalf("engine: %v", err)
	}
	return e
}

// Parent round-trip: insert → status CAS → children → recovery list.
func TestITParentLifecyclePersisted(t *testing.T) {
	ctx, pool, fs := setupIT(t)
	_ = pool
	e := itEngine(t, ctx, pool, fs, newFakeExec())
	p, err := e.Submit(ctx, 1, &SubmitRequest{
		AlgoType: TypeTWAP, Symbol: "EUR/USD", Side: "BUY",
		TotalQty: decimal.RequireFromString("10"),
		Params:   json224(`{"interval_secs":10,"duration_secs":120}`),
	})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	stored, err := fs.GetParent(ctx, p.ID)
	if err != nil || stored == nil {
		t.Fatalf("get parent: %v", err)
	}
	if stored.Status != StatusRunning {
		t.Fatalf("status %s", stored.Status)
	}
	// CAS guard: bogus transition fails.
	ok, _ := fs.CASStatus(ctx, p.ID, []string{StatusNew}, StatusPaused, "")
	if ok {
		t.Fatal("CAS allowed RUNNING→? from NEW")
	}
	// Cancel persists + terminal CAS.
	if _, err := e.Cancel(ctx, 1, p.ID, "it"); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	stored, _ = fs.GetParent(ctx, p.ID)
	if stored.Status != StatusCancelled || stored.CompletedAt == nil {
		t.Fatalf("terminal row wrong: %+v", stored)
	}
	actives, _ := fs.ActiveParents(ctx)
	for _, a := range actives {
		if a.ID == p.ID {
			t.Fatal("cancelled parent still active")
		}
	}
	// Migration down file exists for rollback parity.
	if _, err := os.Stat(filepath.Join("..", "db", "migrations",
		"224_algo_orders.down.sql")); err != nil {
		t.Fatal("down migration missing")
	}
}

// Child rows persist with derived client_order_id; order_id lands after
// dispatch; state cursor survives (restart-replay seam).
func TestITChildRowsAndIdempotentCID(t *testing.T) {
	ctx, pool, fs := setupIT(t)
	fe := newFakeExec()
	e := itEngine(t, ctx, pool, fs, fe)
	p, err := e.Submit(ctx, 1, &SubmitRequest{
		AlgoType: TypeScaled, Symbol: "EUR/USD", Side: "SELL",
		TotalQty: decimal.RequireFromString("6"),
		Params:   json224(`{"levels":3,"distribution":"EQUAL","level_prices":["1.11","1.12","1.13"]}`),
	})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		children, _ := fs.Children(ctx, p.ID)
		if len(children) == 3 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	children, err := fs.Children(ctx, p.ID)
	if err != nil || len(children) != 3 {
		t.Fatalf("children: %v n=%d", err, len(children))
	}
	for i, c := range children {
		want := fmt.Sprintf("algo:%d:%d", p.ID, c.Seq)
		if c.ClientOrderID != want {
			t.Fatalf("child %d cid %q != %q", i, c.ClientOrderID, want)
		}
		if c.OrderID == nil {
			t.Fatalf("child %d missing order_id", i)
		}
		if c.DispatchedAt == nil {
			t.Fatalf("child %d missing dispatched_at audit", i)
		}
	}
	// Parent idempotent replay.
	p2, err := e.Submit(ctx, 1, &SubmitRequest{
		AlgoType: TypeScaled, Symbol: "EUR/USD", Side: "SELL",
		TotalQty:      decimal.RequireFromString("6"),
		Params:        json224(`{"levels":3,"distribution":"EQUAL","level_prices":["1.11","1.12","1.13"]}`),
		ClientOrderID: "it-scaled-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	p3, err := e.Submit(ctx, 1, &SubmitRequest{
		AlgoType: TypeScaled, Symbol: "EUR/USD", Side: "SELL",
		TotalQty:      decimal.RequireFromString("6"),
		Params:        json224(`{"levels":3,"distribution":"EQUAL","level_prices":["1.11","1.12","1.13"]}`),
		ClientOrderID: "it-scaled-1",
	})
	if err != nil || p3.ID != p2.ID {
		t.Fatalf("idempotent parent replay failed: %v (%d vs %d)", err, p3.ID, p2.ID)
	}
}

// PgTopOfBook mid + PgRefPrice + PgVolumeSource read seams over the
// fixture tables.
func TestITReadSeams(t *testing.T) {
	ctx, pool, _ := setupIT(t)
	// Book: bid 1.10 / ask 1.12 resting → mid 1.11.
	for _, row := range [][2]string{{"BUY", "1.10"}, {"SELL", "1.12"}} {
		if _, err := pool.Exec(ctx,
			`INSERT INTO orders (account_id, instrument_id, side, price, quantity)
			 VALUES (1, 1, $1, $2::numeric, 100)`, row[0], row[1]); err != nil {
			t.Fatal(err)
		}
	}
	bid, ask, ok, err := NewPgTopOfBook(pool).BestBidAsk(ctx, "EUR/USD")
	if err != nil || !ok {
		t.Fatalf("top of book: %v ok=%v", err, ok)
	}
	mid := bid.Add(ask).Div(decimal.NewFromInt(2))
	if !mid.Equal(decimal.RequireFromString("1.11")) {
		t.Fatalf("mid %s != 1.11", mid)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO trades (instrument_id, price, quantity) VALUES (1, 1.1050, 500)`); err != nil {
		t.Fatal(err)
	}
	ref, err := NewPgRefPrice(pool).ReferencePrice(ctx, "EUR/USD")
	if err != nil || ref == nil || !ref.Equal(decimal.RequireFromString("1.1050")) {
		t.Fatalf("reference: %v %v", ref, err)
	}
	vol, err := NewPgVolumeSource(pool).VolumeSince(ctx, "EUR/USD", time.Now().Add(-time.Hour))
	if err != nil || !vol.Equal(decimal.RequireFromString("500")) {
		t.Fatalf("volume: %v %v", vol, err)
	}
	prof, err := NewPgVolumeProfile(pool).VolumeProfile(ctx, "EUR/USD", 4)
	if err != nil || len(prof) != 4 {
		t.Fatalf("profile: %v %v", prof, err)
	}
}
