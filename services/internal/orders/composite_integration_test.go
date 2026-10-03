// PG-gated composite-store integration coverage (Phase-16 Tasks
// 16.3.14 + 16.3.20) — runs only when EXC_PG_TEST=1. Applies the real
// migrations 075 + 225 into a throwaway schema over a faithful orders/
// dedup fixture (identical column names/types for every column
// insertOrderInTx and orderCols touch — the fixture mirrors the union
// of migrations 005+ rather than replaying 200 files).
//
//	EXC_PG_TEST=1 go test ./internal/orders/ -run TestIT -v
package orders

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"exchange/pkg/decimal"
)

func itDSN() string {
	if d := os.Getenv("EXC_PG_DSN"); d != "" {
		return d
	}
	if d := os.Getenv("EXC_TEST_DSN"); d != "" {
		return d
	}
	return "postgres://postgres@/w2d?host=/tmp&port=55433"
}

// itPool builds a throwaway-schema pool; skips cleanly when PG is down
// so the gate stays a no-op on machines without a scratch server.
func itPool(t *testing.T) (context.Context, *pgxpool.Pool) {
	t.Helper()
	if os.Getenv("EXC_PG_TEST") != "1" {
		t.Skip("set EXC_PG_TEST=1 to run postgres-gated tests")
	}
	ctx := context.Background()
	schema := fmt.Sprintf("orders_it_%d", time.Now().UnixNano())
	admin, err := pgx.Connect(ctx, itDSN())
	if err != nil {
		t.Skipf("postgres unreachable: %v", err)
	}
	defer admin.Close(ctx)
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	cfg, err := pgxpool.ParseConfig(itDSN())
	if err != nil {
		t.Fatalf("dsn: %v", err)
	}
	// Simple protocol — migration files are multi-statement bodies.
	cfg.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Skipf("postgres unreachable: %v", err)
	}
	t.Cleanup(func() {
		pool.Close()
		c, err := pgx.Connect(context.Background(), itDSN())
		if err == nil {
			_, _ = c.Exec(context.Background(),
				"DROP SCHEMA IF EXISTS "+schema+" CASCADE")
			c.Close(context.Background())
		}
	})
	return ctx, pool
}

func execMigration(t *testing.T, ctx context.Context,
	pool *pgxpool.Pool, name string) {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("..", "db", "migrations", name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	if _, err := pool.Exec(ctx, string(body)); err != nil {
		t.Fatalf("exec %s: %v", name, err)
	}
}

// itFixture mirrors the union of every orders/dedup column the store
// layer touches (insertOrderInTx + orderCols + ApplyFill) plus the FK
// parents migrations 075/225 reference.
const itFixture = `
CREATE TYPE order_side_enum   AS ENUM ('BUY', 'SELL');
CREATE TYPE order_type_enum   AS ENUM (
    'LIMIT', 'MARKET', 'STOP', 'STOP_LIMIT', 'ICEBERG', 'TWAP', 'VWAP',
    'TRAILING_STOP', 'BRACKET', 'OCO', 'SPREAD', 'SCALE', 'PEG', 'FIXING',
    'MOO', 'MOC');
CREATE TYPE time_in_force_enum AS ENUM ('GTC', 'IOC', 'FOK', 'GTD', 'DAY');
CREATE TYPE order_status_enum  AS ENUM (
    'PENDING', 'RESERVED', 'ACTIVE', 'PARTIALLY_FILLED',
    'FILLED', 'CANCELLED', 'REJECTED', 'EXPIRED');

CREATE TABLE accounts (id BIGSERIAL PRIMARY KEY);
CREATE TABLE instruments (id BIGSERIAL PRIMARY KEY);

CREATE TABLE orders (
    id              BIGSERIAL PRIMARY KEY,
    account_id      BIGINT NOT NULL REFERENCES accounts (id),
    instrument_id   BIGINT NOT NULL REFERENCES instruments (id),
    client_order_id VARCHAR(64),
    side            order_side_enum    NOT NULL,
    order_type      order_type_enum    NOT NULL,
    quantity        DECIMAL(28,8)      NOT NULL,
    quote_quantity  DECIMAL(28,8),
    price           DECIMAL(20,8),
    stop_price      DECIMAL(20,8),
    display_qty     DECIMAL(28,8),
    time_in_force   time_in_force_enum NOT NULL,
    status          order_status_enum  NOT NULL DEFAULT 'PENDING',
    filled_qty      DECIMAL(28,8)      NOT NULL DEFAULT 0,
    avg_fill_price  DECIMAL(20,8),
    shard_id        SMALLINT,
    book_seq        BIGINT,
    order_seq       BIGINT,
    post_only       BOOLEAN NOT NULL DEFAULT false,
    reduce_only     BOOLEAN NOT NULL DEFAULT false,
    stp_mode        VARCHAR(16),
    session_id      VARCHAR(64),
    oco_group_id    BIGINT,
    peg_mode        VARCHAR(16),
    peg_offset      DECIMAL(28,8),
    peg_limit       DECIMAL(20,8),
    trigger_source  VARCHAR(16),
    hidden          BOOLEAN NOT NULL DEFAULT false,
    gslo            BOOLEAN NOT NULL DEFAULT false,
    fixing_benchmark VARCHAR(32),
    algo_type       VARCHAR(32),
    algo_params     JSONB,
    -- union mirrors migrations 103/284 (cod_exempt arrives via the
    -- applied 229 migration).
    discretionary_offset_pips DECIMAL(10,4) NOT NULL DEFAULT 0.0,
    gtd_expire_at   TIMESTAMPTZ,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now());

CREATE TABLE client_order_id_dedup (
    account_id      BIGINT NOT NULL,
    client_order_id VARCHAR(64) NOT NULL,
    order_id        BIGINT NOT NULL,
    request_hash    VARCHAR(128),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (account_id, client_order_id));

INSERT INTO accounts (id) VALUES (1);
INSERT INTO instruments (id) VALUES (1);`

// compositeFixture applies the fixture + both composite migrations.
func compositeFixture(t *testing.T, ctx context.Context,
	pool *pgxpool.Pool) *PgStore {
	t.Helper()
	if _, err := pool.Exec(ctx, itFixture); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	execMigration(t, ctx, pool, "075_opo_order_lists.up.sql")
	execMigration(t, ctx, pool, "225_bracket_orders.up.sql")
	execMigration(t, ctx, pool, "229_orders_cod_exempt.up.sql")
	return NewPgStore(pool)
}

// The fill-proportional placement contract (§24 #87): parent fills →
// delta-quantity SL/TP pair through the shared dedup/OCO insert path;
// replay places nothing; partials accumulate placed_qty exactly.
func TestITBracketFillCascade(t *testing.T) {
	ctx, pool := itPool(t)
	st := compositeFixture(t, ctx, pool)

	qty := decimal.MustFromString("1000")
	trig := decimal.MustFromString("1.04000")
	tpTrig := decimal.MustFromString("1.06000")
	parent, b, err := st.InsertOrderBracketTx(ctx, InsertParams{
		AccountID: 1, InstrumentID: 1, ClientOrderID: "bp-1",
		Side: SideBuy, OrderType: TypeLimit, Quantity: qty,
		Price: &trig, TimeInForce: TIFGTC,
		ShardID: 0, OrderSeq: 1, RequestHash: "h1",
	}, BracketChild{TriggerPrice: &trig},
		BracketChild{TriggerPrice: &tpTrig}, nil)
	if err != nil {
		t.Fatalf("InsertOrderBracketTx: %v", err)
	}
	if b == nil || b.State != BracketWorking || b.ParentOrderID != parent.ID {
		t.Fatalf("bracket row: %+v", b)
	}
	// Dedup replay surfaces the conflict row — same payload replays.
	_, _, err = st.InsertOrderBracketTx(ctx, InsertParams{
		AccountID: 1, InstrumentID: 1, ClientOrderID: "bp-1",
		Side: SideBuy, OrderType: TypeLimit, Quantity: qty,
		TimeInForce: TIFGTC, ShardID: 0, OrderSeq: 2, RequestHash: "h1",
	}, BracketChild{}, BracketChild{}, nil)
	if err == nil {
		t.Fatal("dedup replay re-inserted")
	}
	if DedupConflictRow(err) == nil {
		t.Fatalf("replay error %v is not a dedup conflict", err)
	}

	// Partial fill 400 → first proportional pair.
	if err := st.ApplyFill(ctx, parent.ID,
		decimal.MustFromString("1.05"), decimal.MustFromString("400")); err != nil {
		t.Fatalf("apply fill: %v", err)
	}
	limit := decimal.MustFromString("1.03900")
	mkLeg := func(coid string, trigP *decimal.Decimal, lim *decimal.Decimal) InsertParams {
		typ := TypeStop
		if lim != nil {
			typ = TypeStopLimit
		}
		return InsertParams{
			AccountID: 1, InstrumentID: 1, ClientOrderID: coid,
			Side: SideSell, OrderType: typ, Quantity: decimal.Zero,
			Price: lim, StopPrice: trigP, TimeInForce: TIFGTC,
			ShardID: 0, OrderSeq: 100, RequestHash: "c",
		}
	}
	sl, tp, err := st.PlaceBracketChildrenTx(ctx, b.ID, 9001,
		"brk1.1.sl", "brk1.1.tp",
		mkLeg("brk1.1.sl", &trig, &limit),
		mkLeg("brk1.1.tp", &tpTrig, nil))
	if err != nil || sl == nil || tp == nil {
		t.Fatalf("first placement: sl=%v tp=%v err=%v", sl, tp, err)
	}
	for _, c := range [2]*Order{sl, tp} {
		if !c.Quantity.Equal(decimal.MustFromString("400")) {
			t.Fatalf("child qty %s, want 400 (the fill delta)", c.Quantity)
		}
		if c.Side != SideSell || c.OcoGroupID == nil || *c.OcoGroupID != 9001 {
			t.Fatalf("child leg mis-built: %+v", c)
		}
	}
	if sl.OrderType != TypeStopLimit || tp.OrderType != TypeStop {
		t.Fatalf("child types: sl=%s tp=%s", sl.OrderType, tp.OrderType)
	}
	// Replay the same fill — delta now zero → nothing placed.
	sl2, tp2, err := st.PlaceBracketChildrenTx(ctx, b.ID, 9002,
		"brk1.2.sl", "brk1.2.tp",
		mkLeg("brk1.2.sl", &trig, &limit),
		mkLeg("brk1.2.tp", &tpTrig, nil))
	if err != nil || sl2 != nil || tp2 != nil {
		t.Fatalf("replayed fill placed children: %v %v %v", sl2, tp2, err)
	}
	// Remaining 600 → second pair; placed_qty lands at 1000.
	if err := st.ApplyFill(ctx, parent.ID,
		decimal.MustFromString("1.05"), decimal.MustFromString("600")); err != nil {
		t.Fatalf("second fill: %v", err)
	}
	sl3, _, err := st.PlaceBracketChildrenTx(ctx, b.ID, 9003,
		"brk1.3.sl", "brk1.3.tp",
		mkLeg("brk1.3.sl", &trig, &limit),
		mkLeg("brk1.3.tp", &tpTrig, nil))
	if err != nil || sl3 == nil {
		t.Fatalf("second placement: %v", err)
	}
	if !sl3.Quantity.Equal(decimal.MustFromString("600")) {
		t.Fatalf("second pair qty %s, want 600", sl3.Quantity)
	}
	b2, err := st.BracketByParent(ctx, parent.ID)
	if err != nil || b2 == nil {
		t.Fatalf("bracket reload: %v", err)
	}
	if !b2.PlacedQty.Equal(decimal.MustFromString("1000")) || b2.ChildSeq != 2 {
		t.Fatalf("placed_qty/child_seq: %+v", b2)
	}
	if b2.ChildSL.TriggerPrice == nil || b2.ChildTP.TriggerPrice == nil {
		t.Fatalf("child config JSONB did not round-trip: %+v", b2)
	}
	// State CAS + open-child sweep.
	if ok, _ := st.SetBracketState(ctx, b.ID,
		[]string{BracketWorking}, BracketFilled); !ok {
		t.Fatal("WORKING→FILLED CAS failed")
	}
	if ok, _ := st.SetBracketState(ctx, b.ID,
		[]string{BracketWorking}, BracketCancelled); ok {
		t.Fatal("CAS from a stale state succeeded")
	}
	if len(st.mustOpenChildIDs(t, ctx, b.ID)) != 4 {
		t.Fatalf("open child sweep wrong")
	}
	for _, id := range st.mustOpenChildIDs(t, ctx, b.ID) {
		_ = st.ApplyCancel(ctx, id)
	}
	if ids := st.mustOpenChildIDs(t, ctx, b.ID); len(ids) != 0 {
		t.Fatalf("terminal children still open: %v", ids)
	}
	// Terminal group places nothing.
	sl4, _, err := st.PlaceBracketChildrenTx(ctx, b.ID, 9004,
		"x.sl", "x.tp", mkLeg("x.sl", &trig, nil), mkLeg("x.tp", &tpTrig, nil))
	if err != nil || sl4 != nil {
		t.Fatalf("terminal group placement: %v %v", sl4, err)
	}
}

func (s *PgStore) mustOpenChildIDs(t *testing.T, ctx context.Context,
	bracketID int64) []int64 {
	t.Helper()
	ids, err := s.BracketOpenChildIDs(ctx, bracketID)
	if err != nil {
		t.Fatalf("open children: %v", err)
	}
	return ids
}

// OPOCO activation (§24 #287): the locked EXECUTING list stamps both
// pending legs through the shared dedup/OCO path, flips ALL_DONE with
// proceeds bookkeeping, and replays as ok=false; paging partitions
// open vs history on the state set.
func TestITOrderListActivation(t *testing.T) {
	ctx, pool := itPool(t)
	st := compositeFixture(t, ctx, pool)

	qty := decimal.MustFromString("1000")
	px := decimal.MustFromString("1.05000")
	legs := []OrderListLeg{
		{LegIndex: 0, Role: "WORKING", State: "PLACED",
			Params: []byte(`{"symbol":"EURUSD","side":"BUY","type":"LIMIT"}`)},
		{LegIndex: 1, Role: "PENDING", State: "PENDING",
			Params: []byte(`{"symbol":"EURUSD","side":"SELL","type":"LIMIT","price":"1.06000"}`)},
		{LegIndex: 2, Role: "PENDING", State: "PENDING",
			Params: []byte(`{"symbol":"EURUSD","side":"SELL","type":"STOP","stop_price":"1.04000"}`)},
	}
	l, w, err := st.InsertOrderListTx(ctx, &OrderList{
		AccountID: 1, InstrumentID: 1, ContingencyType: ContingencyOPOCO,
		ClientOrderID: "ol-1",
	}, InsertParams{
		AccountID: 1, InstrumentID: 1, ClientOrderID: "w-1",
		Side: SideBuy, OrderType: TypeLimit, Quantity: qty,
		Price: &px, TimeInForce: TIFGTC,
		ShardID: 0, OrderSeq: 1, RequestHash: "w",
	}, legs)
	if err != nil {
		t.Fatalf("InsertOrderListTx: %v", err)
	}
	if l == nil || l.State != ListStateExecuting || w == nil {
		t.Fatalf("list insert: %+v %v", l, w)
	}
	_, gotLegs, err := st.OrderListGet(ctx, l.ID)
	if err != nil || len(gotLegs) != 3 {
		t.Fatalf("legs persisted: %d %v", len(gotLegs), err)
	}
	if gotLegs[1].OrderID != nil || gotLegs[1].State != "PENDING" {
		t.Fatalf("pending leg pre-activation: %+v", gotLegs[1])
	}

	grp := int64(777)
	pending := []InsertParams{
		{AccountID: 1, InstrumentID: 1, ClientOrderID: "ol1.p1",
			Side: SideSell, OrderType: TypeLimit,
			Quantity: decimal.MustFromString("900"),
			Price:    &px, TimeInForce: TIFGTC, ShardID: 0, OrderSeq: 50,
			RequestHash: "p1"},
		{AccountID: 1, InstrumentID: 1, ClientOrderID: "ol1.p2",
			Side: SideSell, OrderType: TypeStop,
			Quantity:  decimal.MustFromString("900"),
			StopPrice: &px, TimeInForce: TIFGTC, ShardID: 0, OrderSeq: 51,
			RequestHash: "p2"},
	}
	placed, ok, err := st.ActivatePendingTx(ctx, l.ID,
		decimal.MustFromString("990"), decimal.MustFromString("900"),
		&grp, pending)
	if err != nil || !ok || len(placed) != 2 {
		t.Fatalf("activate: n=%d ok=%v err=%v", len(placed), ok, err)
	}
	for _, p := range placed {
		if p.OcoGroupID == nil || *p.OcoGroupID != grp {
			t.Fatalf("OPOCO leg missing shared oco_group_id: %+v", p)
		}
	}
	l2, legs2, err := st.OrderListGet(ctx, l.ID)
	if err != nil || l2.State != ListStateAllDone {
		t.Fatalf("post-activation list: %+v %v", l2, err)
	}
	if !l2.LockedProceeds.Equal(decimal.MustFromString("990")) ||
		!l2.NetPendingQty.Equal(decimal.MustFromString("900")) {
		t.Fatalf("proceeds bookkeeping: %+v", l2)
	}
	for _, lg := range legs2 {
		if lg.Role == "PENDING" && (lg.OrderID == nil || lg.State != "PLACED") {
			t.Fatalf("leg not stamped: %+v", lg)
		}
	}
	// Replay → ok=false, no second insert.
	if _, ok2, err := st.ActivatePendingTx(ctx, l.ID,
		decimal.Zero, decimal.Zero, nil, pending); err != nil || ok2 {
		t.Fatalf("activation replay: ok=%v err=%v", ok2, err)
	}
	// Open vs history paging partition: ALL_DONE is an OPEN state (the
	// placed legs are still live) — the list leaves the open page only
	// when it closes.
	open, _, err := st.OrderListsPage(ctx, 1, true, time.Time{}, 0, 10)
	if err != nil || len(open) != 1 {
		t.Fatalf("open page: %d %v", len(open), err)
	}
	hist, _, err := st.OrderListsPage(ctx, 1, false, time.Time{}, 0, 10)
	if err != nil || len(hist) != 0 {
		t.Fatalf("history page before close: %d %v", len(hist), err)
	}
	if _, err := st.SetOrderListState(ctx, l.ID,
		[]string{ListStateAllDone}, ListStateCancelled, ""); err != nil {
		t.Fatalf("close list: %v", err)
	}
	open, _, _ = st.OrderListsPage(ctx, 1, true, time.Time{}, 0, 10)
	hist, total, err := st.OrderListsPage(ctx, 1, false, time.Time{}, 0, 10)
	if err != nil || len(hist) != 1 || total != 1 || len(open) != 0 {
		t.Fatalf("post-close paging: open=%d hist=%+v total=%d err=%v",
			len(open), hist, total, err)
	}
	// Working-leg resolution + leg-order resolution.
	byW, _, err := st.OrderListByWorking(ctx, w.ID)
	if err != nil || byW == nil || byW.ID != l.ID {
		t.Fatalf("by-working: %+v %v", byW, err)
	}
	byLeg, _, err := st.OrderListByLegOrder(ctx, placed[0].ID)
	if err != nil || byLeg == nil || byLeg.ID != l.ID {
		t.Fatalf("by-leg-order: %+v %v", byLeg, err)
	}
}
