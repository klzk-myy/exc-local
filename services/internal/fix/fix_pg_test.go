// EXC_PG_TEST=1 gated integration tests for migration 037 (prime brokerage)
// and migration 226 (FIX allocations) — schema application, PB give-up
// status transitions, allocation persistence/corrections, and the
// allocation_events immutability trigger.
//
//	PG DSN: EXC_TEST_DSN or EXC_PG_DSN, default dev DSN below.
//
// Run: EXC_PG_TEST=1 go test ./internal/fix -run PG -v
package fix

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const fixTestDSN = "postgres://exchange:exchange_dev@127.0.0.1:5433/exchange?sslmode=disable"

func fixPGPool(t *testing.T) (*pgxpool.Pool, string) {
	t.Helper()
	if os.Getenv("EXC_PG_TEST") != "1" {
		t.Skip("set EXC_PG_TEST=1 to run Postgres integration tests")
	}
	dsn := os.Getenv("EXC_TEST_DSN")
	if dsn == "" {
		dsn = os.Getenv("EXC_PG_DSN")
	}
	if dsn == "" {
		dsn = fixTestDSN
	}
	schema := fmt.Sprintf("fix_itest_%d", time.Now().UnixNano())

	// Simple-protocol admin conn creates the schema and the accounts stub
	// (migrations 037/226 FK into accounts; only the shape we need).
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	cfg.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol
	admin, err := pgx.ConnectConfig(context.Background(), cfg)
	if err != nil {
		t.Skipf("postgres unreachable at %s: %v", dsn, err)
	}
	if _, err := admin.Exec(context.Background(),
		"CREATE SCHEMA "+schema); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE")
		_ = admin.Close(context.Background())
	})

	cfg.RuntimeParams["search_path"] = schema + ",public"
	conn, err := pgx.ConnectConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("schema conn: %v", err)
	}
	defer conn.Close(context.Background())
	if _, err := conn.Exec(context.Background(), `
		CREATE TABLE accounts (
			id BIGSERIAL PRIMARY KEY,
			parent_account_id BIGINT REFERENCES accounts (id)
		);
		INSERT INTO accounts (id) VALUES (900), (901), (902), (903);
		UPDATE accounts SET parent_account_id = 900 WHERE id IN (901, 902, 903);`); err != nil {
		t.Fatalf("accounts fixture: %v", err)
	}
	migRoot := "../db/migrations/"
	for _, f := range []string{
		migRoot + "037_create_prime_brokerage.up.sql",
		migRoot + "226_fix_allocations.up.sql",
	} {
		sql, rerr := os.ReadFile(f)
		if rerr != nil {
			t.Fatalf("read %s: %v", f, rerr)
		}
		if _, err := conn.Exec(context.Background(), string(sql)); err != nil {
			t.Fatalf("apply %s: %v", f, err)
		}
	}

	pool, err := pgxpool.NewWithConfig(context.Background(), func() *pgxpool.Config {
		c, cerr := pgxpool.ParseConfig(dsn)
		if cerr != nil {
			t.Fatalf("pool cfg: %v", cerr)
		}
		c.ConnConfig.RuntimeParams["search_path"] = schema + ",public"
		return c
	}())
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool, schema
}

func TestPG_PrimeBrokerageSchema(t *testing.T) {
	pool, _ := fixPGPool(t)
	ctx := context.Background()
	store := NewPBStore(pool)

	// Seed a PB + credit-limit (give-up eligibility) row.
	var pbID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO prime_brokers (pb_name, bic_code, fix_comp_id, traiana_code)
		 VALUES ('TestPB','TESTBIC1','PB-COMP-1','TRX-PB1') RETURNING id`).Scan(&pbID); err != nil {
		t.Fatalf("insert pb: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO pb_credit_limits (prime_broker_id, client_account_id, net_open_position_limit)
		 VALUES ($1, 900, 1000000)`, pbID); err != nil {
		t.Fatalf("insert credit limit: %v", err)
	}

	pb, err := store.PrimeBrokerByCompID(ctx, "PB-COMP-1")
	if err != nil || pb.TraianaCode != "TRX-PB1" {
		t.Fatalf("pb lookup: %v %+v", err, pb)
	}
	elig, err := store.EligiblePrimeBrokers(ctx, 900)
	if err != nil || len(elig) != 1 || elig[0].ID != pbID {
		t.Fatalf("eligibility: %v %+v", err, elig)
	}
	if elig2, _ := store.EligiblePrimeBrokers(ctx, 999); len(elig2) != 0 {
		t.Fatal("ineligible account must resolve empty")
	}

	// Give-up lifecycle: PENDING -> AFFIRMED, idempotent re-record.
	g, err := store.RecordGiveUp(ctx, 5001, pbID, 900, 900)
	if err != nil || g.Status != GiveUpPending {
		t.Fatalf("record: %v %+v", err, g)
	}
	g2, err := store.RecordGiveUp(ctx, 5001, pbID, 900, 900)
	if err != nil || g2.ID != g.ID {
		t.Fatalf("re-record must be idempotent: %v %+v", err, g2)
	}
	if err := store.AttachTraianaRef(ctx, g.ID, "TRX-900"); err != nil {
		t.Fatalf("attach ref: %v", err)
	}
	if err := store.UpdateStatusByTraianaID(ctx, "TRX-900", GiveUpAffirmed, ""); err != nil {
		t.Fatalf("affirm: %v", err)
	}
	after, _ := store.GiveUpByID(ctx, g.ID)
	if after.Status != GiveUpAffirmed || after.AffirmedAt == nil {
		t.Fatalf("want AFFIRMED+ts, got %+v", after)
	}
	// Illegal: AFFIRMED -> PENDING rejected.
	if err := store.UpdateGiveUpStatus(ctx, g.ID, GiveUpPending, ""); err == nil {
		t.Fatal("AFFIRMED->PENDING must be illegal")
	}

	// REJECTED path + reason persisted.
	g3, _ := store.RecordGiveUp(ctx, 5002, pbID, 900, 900)
	if err := store.UpdateGiveUpStatus(ctx, g3.ID, GiveUpRejected, "limit breach"); err != nil {
		t.Fatalf("reject: %v", err)
	}
	g3r, _ := store.GiveUpByID(ctx, g3.ID)
	if g3r.Status != GiveUpRejected || g3r.RejectionReason != "limit breach" {
		t.Fatalf("reject state: %+v", g3r)
	}

	// Timeout sweep surface.
	old := time.Now().Add(-2 * time.Hour)
	if _, err := pool.Exec(ctx,
		`UPDATE pb_giveup_trades SET created_at=$1 WHERE id=$2`, old, g3.ID); err != nil {
		t.Fatalf("age give-up: %v", err)
	}
	pend, err := store.PendingOlderThan(ctx, time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatalf("pending sweep: %v", err)
	}
	// g3 is REJECTED — only genuinely PENDING rows returned.
	for _, p := range pend {
		if p.Status != GiveUpPending {
			t.Fatalf("sweep returned non-pending %+v", p)
		}
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO pb_giveup_trades (trade_id, prime_broker_id, executing_broker_account_id, client_account_id, created_at)
		 VALUES (5003,$1,900,900,$2)`, pbID, old); err != nil {
		t.Fatalf("insert aged pending: %v", err)
	}
	pend, _ = store.PendingOlderThan(ctx, time.Now().Add(-time.Hour))
	if len(pend) != 1 {
		t.Fatalf("want 1 aged pending, got %d", len(pend))
	}
}

func TestPG_AllocationStore(t *testing.T) {
	pool, _ := fixPGPool(t)
	ctx := context.Background()
	store := NewAllocationStore(pool)

	alloc := &Allocation{
		AllocID: "PG-A1", TransType: AllocTransNew, Method: AllocMethodManual,
		Status: AllocStatusAccepted, SessionID: "FIX.4.4:EX->MM",
		MasterAccountID: 900, Symbol: "EURUSD", Side: '1',
		ExecQty: decV("100"), ExecRefs: []string{"E1"},
	}
	legs := []AllocationLeg{
		{LegNo: 1, AllocAccount: "901", AccountID: 901, AllocQty: decV("60"), Status: LegBooked},
		{LegNo: 2, AllocAccount: "902", AccountID: 902, AllocQty: decV("40"), Status: LegBooked},
	}
	if err := store.SaveNew(ctx, alloc, legs); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, err := store.LoadByAllocID(ctx, "PG-A1")
	if err != nil || got.Status != AllocStatusAccepted || len(got.Legs) != 2 {
		t.Fatalf("load: %v %+v", err, got)
	}
	if got.Legs[0].ChildExecID == "" || got.ExecRefs[0] != "E1" {
		t.Fatalf("legs/refs: %+v", got)
	}
	evs, err := store.Events(ctx, got.ID)
	if err != nil || len(evs) != 2 {
		t.Fatalf("events: %v %d", err, len(evs))
	}

	// REPLACE: prior row REPLACED, replacement ACCEPTED with fresh events.
	repl := &Allocation{
		AllocID: "PG-A1B", RefAllocID: "PG-A1", TransType: AllocTransReplace,
		Method: AllocMethodManual, Status: AllocStatusAccepted,
		SessionID: "FIX.4.4:EX->MM", MasterAccountID: 900, Symbol: "EURUSD",
		Side: '1', ExecQty: decV("100"), ExecRefs: []string{"E1"},
	}
	replLegs := []AllocationLeg{
		{LegNo: 1, AllocAccount: "901", AccountID: 901, AllocQty: decV("50"), Status: LegBooked},
		{LegNo: 2, AllocAccount: "902", AccountID: 902, AllocQty: decV("50"), Status: LegBooked},
	}
	if err := store.Supersede(ctx, "PG-A1", repl, replLegs); err != nil {
		t.Fatalf("supersede: %v", err)
	}
	prev, _ := store.LoadByAllocID(ctx, "PG-A1")
	if prev.Status != AllocStatusReplaced {
		t.Fatalf("prev status %s", prev.Status)
	}
	for _, l := range prev.Legs {
		if l.Status != LegCancelled {
			t.Fatalf("prev leg not cancelled: %+v", l)
		}
	}

	// CANCEL the replacement.
	cancelled, err := store.Cancel(ctx, "PG-A1B", "test")
	if err != nil || cancelled.Status != AllocStatusCancelled {
		t.Fatalf("cancel: %v %+v", err, cancelled)
	}
	// CANCEL on CANCELLED must reject.
	if _, err := store.Cancel(ctx, "PG-A1B", "again"); err == nil {
		t.Fatal("double cancel must fail")
	}

	// Immutability: UPDATE/DELETE on allocation_events must raise.
	if _, err := pool.Exec(ctx,
		`UPDATE allocation_events SET event_type='X' WHERE allocation_id=$1`, got.ID); err == nil {
		t.Fatal("audit events must be immutable (UPDATE)")
	}
	if _, err := pool.Exec(ctx,
		`DELETE FROM allocation_events WHERE allocation_id=$1`, got.ID); err == nil {
		t.Fatal("audit events must be immutable (DELETE)")
	}
	// seq uniqueness enforced.
	evsAfter, _ := store.Events(ctx, got.ID)
	if len(evsAfter) != len(evs)+1 { // SUPERSEDED event appended on the old row
		t.Fatalf("events after supersede: %d", len(evsAfter))
	}
}
