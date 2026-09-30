// PG-gated integration coverage for Task 16.3.19 — runs only when
// EXC_PG_TEST=1:
//
//	EXC_PG_TEST=1 EXC_TEST_DSN='postgres://...' \
//	    go test ./internal/bots/ -run TestIT -v
//
// The order pipeline and read model are fakes recording calls; the
// durable store is real PostgreSQL so the cap, claim and PnL SQL paths
// execute against the true 071 schema.
package bots

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"exchange/internal/orders"
	"exchange/pkg/decimal"
	excerrors "exchange/pkg/errors"
)

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
	schema := fmt.Sprintf("bots_it_%d_%d", time.Now().UnixNano(), rand.Intn(1e6))
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

// fixtureDDL is the minimal upstream surface 071 depends on — the child
// order_id FK only needs orders.id.
const fixtureDDL = `
CREATE TABLE accounts (
    id           BIGSERIAL PRIMARY KEY,
    user_id      BIGINT NOT NULL,
    account_type VARCHAR(16) NOT NULL DEFAULT 'SPOT',
    status       VARCHAR(12) NOT NULL DEFAULT 'ACTIVE'
);
CREATE TABLE instruments (
    id             BIGSERIAL PRIMARY KEY,
    symbol         VARCHAR(20) NOT NULL,
    base_currency  VARCHAR(3)  NOT NULL,
    quote_currency VARCHAR(3)  NOT NULL
);
CREATE TABLE orders (
    id              BIGSERIAL PRIMARY KEY,
    client_order_id VARCHAR(64),
    status          VARCHAR(16) NOT NULL DEFAULT 'ACTIVE'
);
`

func itSchema(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(ctx, fixtureDDL); err != nil {
		t.Fatalf("fixture ddl: %v", err)
	}
	body, err := os.ReadFile(filepath.Join("..", "db", "migrations",
		"071_grid_bots.up.sql"))
	if err != nil {
		t.Fatalf("read migration: %v", err)
	}
	if _, err := pool.Exec(ctx, string(body)); err != nil {
		t.Fatalf("exec 071: %v", err)
	}
	// Task 10.3.26: PAUSED status + widened live-bot indexes. The file's
	// top-level ALTER TYPE ... ADD VALUE must commit before 'PAUSED' is
	// referenced — a multi-statement simple-Query Exec wraps the whole
	// file in one implicit transaction (55P04), so split at the BEGIN
	// boundary exactly as psql -f's per-statement apply would.
	body, err = os.ReadFile(filepath.Join("..", "db", "migrations",
		"275_grid_bot_pause.up.sql"))
	if err != nil {
		t.Fatalf("read migration: %v", err)
	}
	parts := strings.SplitN(string(body), "BEGIN;", 2)
	if len(parts) != 2 {
		t.Fatal("275 migration missing BEGIN boundary")
	}
	for i, part := range parts {
		stmt := part
		if i == 1 {
			stmt = "BEGIN;" + stmt
		}
		if _, err := pool.Exec(ctx, stmt); err != nil {
			t.Fatalf("exec 275 part %d: %v", i, err)
		}
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO accounts (id, user_id, status) VALUES (10,1,'ACTIVE');
		INSERT INTO instruments (id, symbol, base_currency, quote_currency)
		  VALUES (1,'EUR/USD','EUR','USD');`); err != nil {
		t.Fatalf("seed: %v", err)
	}
}

// fakePipe records submissions/cancels; every Submit mints a real orders
// row so the child FK binds truthfully.
type fakePipe struct {
	pool      *pgxpool.Pool
	nextID    int64
	mu        sync.Mutex
	submitted []*orders.SubmitRequest
	cancelled []int64
	inst      *orders.Instrument
	acct      *orders.Account
}

func (f *fakePipe) Submit(ctx context.Context, _ *orders.Account,
	req *orders.SubmitRequest) (*orders.Ack, error) {

	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	if _, err := f.pool.Exec(ctx,
		`INSERT INTO orders (id, client_order_id, status) VALUES ($1,$2,'ACTIVE')`,
		f.nextID, req.ClientOrderID); err != nil {
		return nil, err
	}
	f.submitted = append(f.submitted, req)
	return &orders.Ack{OrderID: f.nextID, Status: "ACTIVE"}, nil
}

func (f *fakePipe) Cancel(_ context.Context, _ *orders.Account, orderID int64,
	_, _, _ string) (*orders.Ack, error) {

	f.mu.Lock()
	defer f.mu.Unlock()
	f.cancelled = append(f.cancelled, orderID)
	return &orders.Ack{OrderID: orderID, Status: "CANCELLED"}, nil
}

func (f *fakePipe) AccountByID(_ context.Context, id int64) (*orders.Account, error) {
	if f.acct != nil && f.acct.ID == id {
		return f.acct, nil
	}
	return nil, nil
}

func (f *fakePipe) InstrumentBySymbol(_ context.Context,
	symbol string) (*orders.Instrument, error) {

	if f.inst != nil && f.inst.Symbol == symbol {
		return f.inst, nil
	}
	return nil, nil
}

// fakeRM serves reference price, balances and order reads.
type fakeRM struct {
	ref    decimal.Decimal
	bal    decimal.Decimal
	orders map[int64]*orders.Order
}

func (f *fakeRM) ReferencePrice(_ context.Context, _ int64) (*decimal.Decimal, error) {
	return &f.ref, nil
}

func (f *fakeRM) AvailableBalance(_ context.Context, _ int64,
	_ string) (*decimal.Decimal, error) {

	return &f.bal, nil
}

func (f *fakeRM) GetOrder(_ context.Context, orderID int64) (*orders.Order, error) {
	if f.orders != nil {
		return f.orders[orderID], nil
	}
	return nil, nil
}

func itEngine(t *testing.T, pool *pgxpool.Pool) (*Engine, *fakePipe) {
	t.Helper()
	inst := &orders.Instrument{
		ID: 1, Symbol: "EUR/USD", BaseCurrency: "EUR", QuoteCurrency: "USD",
		Status:      "ACTIVE",
		TickSize:    decimal.RequireFromString("0.0001"),
		LotSize:     decimal.RequireFromString("1"),
		MinOrderQty: decimal.RequireFromString("1"),
		MinNotional: decimal.Zero,
	}
	pipe := &fakePipe{
		pool: pool,
		inst: inst,
		acct: &orders.Account{ID: 10, UserID: 1, Type: "SPOT", Status: "ACTIVE"},
	}
	rm := &fakeRM{
		ref: decimal.RequireFromString("1.0500"),
		bal: decimal.RequireFromString("1000000"),
	}
	return NewEngine(NewPgStore(pool), pipe, rm, nil), pipe
}

func gridReq() CreateRequest {
	return CreateRequest{
		Symbol: "EUR/USD", UpperPrice: "1.1000", LowerPrice: "1.0000",
		GridCount: 11, Mode: "ARITHMETIC", TotalInvestment: "1100",
	}
}

// IT: create → child fill → adjacent opposite order → round-trip PnL,
// plus idempotent redelivery and stop-canonicalisation.
func TestITGridLifecycleFillCounterPnL(t *testing.T) {
	ctx, pool := itPool(t)
	itSchema(t, ctx, pool)
	eng, pipe := itEngine(t, pool)

	det, err := eng.Create(ctx, 10, gridReq())
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// 11 levels; the 1.0500 level sits exactly on the reference → 10 legs.
	working := 0
	var buy *GridChild
	for i := range det.Children {
		if det.Children[i].Status == ChildWorking {
			working++
		}
		if det.Children[i].Side == "BUY" && det.Children[i].LevelIndex == 4 {
			c := det.Children[i]
			buy = &c
		}
	}
	if working != 10 || buy == nil {
		t.Fatalf("children: working=%d buy=%v", working, buy)
	}
	if len(pipe.submitted) != 10 {
		t.Fatalf("submitted %d", len(pipe.submitted))
	}

	// Fill the BUY child at level 4 → SELL counter at level 5.
	if err := eng.OnFill(ctx, *buy.OrderID, buy.Price, buy.Qty); err != nil {
		t.Fatalf("onfill: %v", err)
	}
	det2, err := eng.Detail(ctx, 10, det.Bot.BotID)
	if err != nil {
		t.Fatalf("detail: %v", err)
	}
	var counter *GridChild
	for i := range det2.Children {
		c := &det2.Children[i]
		if c.SourceChildID != nil && *c.SourceChildID == buy.ID {
			counter = c
		}
		if c.ID == buy.ID && c.Status != ChildFilled {
			t.Fatalf("source child status %s", c.Status)
		}
	}
	if counter == nil {
		t.Fatal("no counter order recorded")
	}
	if counter.Side != "SELL" || counter.LevelIndex != 5 ||
		counter.Status != ChildWorking || counter.OrderID == nil {
		t.Fatalf("counter %+v", counter)
	}
	// Counter price must sit exactly on the level-5 grid price.
	if !counter.Price.Equal(d(t, "1.0500")) {
		t.Fatalf("counter price %s", counter.Price)
	}

	// Idempotent redelivery — no second counter, no double PnL.
	if err := eng.OnFill(ctx, *buy.OrderID, buy.Price, buy.Qty); err != nil {
		t.Fatalf("redelivery: %v", err)
	}
	det3, _ := eng.Detail(ctx, 10, det.Bot.BotID)
	counters := 0
	for i := range det3.Children {
		if det3.Children[i].SourceChildID != nil &&
			*det3.Children[i].SourceChildID == buy.ID {
			counters++
		}
	}
	if counters != 1 {
		t.Fatalf("counters %d", counters)
	}

	// Fill the counter at 1.0500 → round-trip (1.0500 − 1.0400) × qty.
	if err := eng.OnFill(ctx, *counter.OrderID, counter.Price, counter.Qty); err != nil {
		t.Fatalf("counter fill: %v", err)
	}
	det4, _ := eng.Detail(ctx, 10, det.Bot.BotID)
	wantPnL := d(t, "1.0500").Sub(d(t, "1.0400")).Mul(buy.Qty)
	if !det4.Bot.RealizedPnL.Equal(wantPnL) {
		t.Fatalf("realized_pnl %s want %s", det4.Bot.RealizedPnL, wantPnL)
	}
	if det4.Bot.FillsCount != 2 {
		t.Fatalf("fills_count %d", det4.Bot.FillsCount)
	}

	// DELETE: bot → STOPPED, every live child cancelled through the pipe.
	stopped, err := eng.Stop(ctx, pipe.acct, det.Bot.BotID)
	if err != nil {
		t.Fatalf("stop: %v", err)
	}
	if stopped.Bot.Status != StatusStopped {
		t.Fatalf("status %s", stopped.Bot.Status)
	}
	if len(pipe.cancelled) == 0 {
		t.Fatal("no cancels dispatched")
	}
	open, _ := NewPgStore(pool).OpenChildren(ctx, det.Bot.BotID)
	if len(open) != 0 {
		t.Fatalf("open children %d", len(open))
	}
}

// IT: pause freezes placement while fills keep booking; resume re-arms
// the suspended flips eagerly (Phase-10 Task 10.3.26 semantics).
func TestITGridBotPauseFreezeResume(t *testing.T) {
	ctx, pool := itPool(t)
	itSchema(t, ctx, pool)
	eng, pipe := itEngine(t, pool)

	det, err := eng.Create(ctx, 10, gridReq())
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	var buy *GridChild
	for i := range det.Children {
		if det.Children[i].Side == "BUY" && det.Children[i].LevelIndex == 4 {
			c := det.Children[i]
			buy = &c
		}
	}
	if buy == nil {
		t.Fatal("no level-4 BUY child")
	}

	// Pause — PAUSED without stopped_at (a pause is not a stop).
	paused, err := eng.Pause(ctx, pipe.acct, det.Bot.BotID)
	if err != nil {
		t.Fatalf("pause: %v", err)
	}
	if paused.Bot.Status != StatusPaused {
		t.Fatalf("status %s", paused.Bot.Status)
	}
	if paused.Bot.StoppedAt != nil || paused.Bot.StopReason != "" {
		t.Fatalf("pause stamped stop fields: %+v", paused.Bot)
	}
	// Idempotent retry.
	if _, err := eng.Pause(ctx, pipe.acct, det.Bot.BotID); err != nil {
		t.Fatalf("pause retry: %v", err)
	}

	// Fill during pause: the child books FILLED but no counter leg is
	// claimed or submitted — the bot is frozen.
	submittedBefore := len(pipe.submitted)
	if err := eng.OnFill(ctx, *buy.OrderID, buy.Price, buy.Qty); err != nil {
		t.Fatalf("paused onfill: %v", err)
	}
	if len(pipe.submitted) != submittedBefore {
		t.Fatalf("paused bot submitted %d new legs",
			len(pipe.submitted)-submittedBefore)
	}
	det2, err := eng.Detail(ctx, 10, det.Bot.BotID)
	if err != nil {
		t.Fatalf("detail: %v", err)
	}
	for i := range det2.Children {
		c := &det2.Children[i]
		if c.ID == buy.ID && c.Status != ChildFilled {
			t.Fatalf("source child status %s", c.Status)
		}
		if c.SourceChildID != nil && *c.SourceChildID == buy.ID {
			t.Fatalf("frozen bot claimed counter %+v", c)
		}
	}
	if det2.Bot.FillsCount != 1 {
		t.Fatalf("fills_count %d", det2.Bot.FillsCount)
	}

	// Resume — RUNNING again and the suspended flip re-arms eagerly:
	// a SELL counter appears at level 5 bound to a real order.
	resumed, err := eng.Resume(ctx, pipe.acct, det.Bot.BotID)
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if resumed.Bot.Status != StatusRunning {
		t.Fatalf("status %s", resumed.Bot.Status)
	}
	var counter *GridChild
	for i := range resumed.Children {
		c := &resumed.Children[i]
		if c.SourceChildID != nil && *c.SourceChildID == buy.ID {
			counter = c
		}
	}
	if counter == nil {
		t.Fatal("resume did not re-arm the frozen flip")
	}
	if counter.Side != "SELL" || counter.LevelIndex != 5 ||
		counter.Status != ChildWorking || counter.OrderID == nil {
		t.Fatalf("rearmed counter %+v", counter)
	}

	// Post-resume fills behave normally — counter fill books the
	// round-trip and spawns the next flip.
	if err := eng.OnFill(ctx, *counter.OrderID, counter.Price, counter.Qty); err != nil {
		t.Fatalf("resumed onfill: %v", err)
	}
	det3, _ := eng.Detail(ctx, 10, det.Bot.BotID)
	wantPnL := d(t, "1.0500").Sub(d(t, "1.0400")).Mul(buy.Qty)
	if !det3.Bot.RealizedPnL.Equal(wantPnL) {
		t.Fatalf("realized_pnl %s want %s", det3.Bot.RealizedPnL, wantPnL)
	}

	// Stop works from PAUSED too — children still unwind.
	if _, err := eng.Pause(ctx, pipe.acct, det.Bot.BotID); err != nil {
		t.Fatalf("re-pause: %v", err)
	}
	stopped, err := eng.Stop(ctx, pipe.acct, det.Bot.BotID)
	if err != nil {
		t.Fatalf("stop from paused: %v", err)
	}
	if stopped.Bot.Status != StatusStopped {
		t.Fatalf("status %s", stopped.Bot.Status)
	}
	open, _ := NewPgStore(pool).OpenChildren(ctx, det.Bot.BotID)
	if len(open) != 0 {
		t.Fatalf("open children %d", len(open))
	}
	// Terminal bots cannot pause/resume.
	if _, err := eng.Pause(ctx, pipe.acct, det.Bot.BotID); err == nil {
		t.Fatal("pause on stopped bot succeeded")
	}
	if _, err := eng.Resume(ctx, pipe.acct, det.Bot.BotID); err == nil {
		t.Fatal("resume on stopped bot succeeded")
	}
}

// IT: a PAUSED bot still occupies an R13 concurrency slot — pausing
// must never free a slot for a sixth bot.
func TestITGridBotPausedHoldsCapSlot(t *testing.T) {
	ctx, pool := itPool(t)
	itSchema(t, ctx, pool)
	eng, pipe := itEngine(t, pool)

	for i := 0; i < MaxConcurrentBots; i++ {
		if _, err := eng.Create(ctx, 10, gridReq()); err != nil {
			t.Fatalf("create %d: %v", i, err)
		}
	}
	if _, err := eng.Pause(ctx, pipe.acct, 1); err != nil {
		t.Fatalf("pause: %v", err)
	}
	_, err := eng.Create(ctx, 10, gridReq())
	var e *excerrors.Error
	if err == nil || !errors.As(err, &e) || e.Code != CodeMaxGridBotsExceeded {
		t.Fatalf("create over paused slot: %v", err)
	}
}

// IT: the R13 five-concurrent-bot cap is enforced inside the create tx.
func TestITGridBotConcurrentCap(t *testing.T) {
	ctx, pool := itPool(t)
	itSchema(t, ctx, pool)
	eng, _ := itEngine(t, pool)

	for i := 0; i < MaxConcurrentBots; i++ {
		if _, err := eng.Create(ctx, 10, gridReq()); err != nil {
			t.Fatalf("create %d: %v", i, err)
		}
	}
	_, err := eng.Create(ctx, 10, gridReq())
	var e *excerrors.Error
	if err == nil || !errors.As(err, &e) || e.Code != CodeMaxGridBotsExceeded {
		t.Fatalf("6th create: %v", err)
	}
	// A stopped bot frees a slot.
	if _, err := eng.Stop(ctx,
		&orders.Account{ID: 10, UserID: 1, Type: "SPOT", Status: "ACTIVE"},
		1); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if _, err := eng.Create(ctx, 10, gridReq()); err != nil {
		t.Fatalf("create after stop: %v", err)
	}
}
