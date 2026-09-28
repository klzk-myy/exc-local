// Task 5.3.15 — PG-backed promo lifecycle tests. Gated: EXC_PG_TEST=1,
// DSN via EXC_PG_DSN or EXC_TEST_DSN (default: scratch w2d on the /tmp socket).
//
// fee_promo_windows comes from the real 181 migration; fee_tiers is
// mirrored minimal DDL (the real 012 table has columns irrelevant to
// the promo seam — the store only touches id + promo_*).
//
// Run: EXC_PG_TEST=1 go test ./internal/promos/ -run Integration -v
package promos

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"
)

func promoTestDSN() string {
	if d := os.Getenv("EXC_PG_DSN"); d != "" {
		return d
	}
	if d := os.Getenv("EXC_TEST_DSN"); d != "" {
		return d
	}
	return "postgres://postgres@/w2d?host=/tmp&port=55433"
}

func promoItest(t *testing.T) (*Store, *pgxpool.Pool, context.Context) {
	t.Helper()
	if os.Getenv("EXC_PG_TEST") != "1" {
		t.Skip("set EXC_PG_TEST=1 to run PostgreSQL integration tests")
	}
	ctx := context.Background()
	schema := fmt.Sprintf("promos_itest_%d", time.Now().UnixNano())

	cfg, err := pgx.ParseConfig(promoTestDSN())
	if err != nil {
		t.Fatalf("dsn: %v", err)
	}
	cfg.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol
	cfg.RuntimeParams["search_path"] = schema + ",public"
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Skipf("postgres unreachable: %v", err)
	}
	ddl, err := os.ReadFile("../db/migrations/181_fee_promo_windows.up.sql")
	if err != nil {
		conn.Close(ctx)
		t.Fatalf("read migration 181: %v", err)
	}
	if _, err := conn.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		conn.Close(ctx)
		t.Fatalf("create schema: %v", err)
	}
	if _, err := conn.Exec(ctx, `
		CREATE TABLE fee_tiers (
		    id               BIGINT PRIMARY KEY,
		    maker_bps        DECIMAL(6,4) NOT NULL,
		    taker_bps        DECIMAL(6,4) NOT NULL,
		    promo_maker_bps  DECIMAL(6,4),   -- 181 widens to DECIMAL(9,4)
		    promo_taker_bps  DECIMAL(6,4),
		    promo_until      TIMESTAMPTZ
		)`); err != nil {
		conn.Close(ctx)
		t.Fatalf("fee_tiers ddl: %v", err)
	}
	if _, err := conn.Exec(ctx, string(ddl)); err != nil {
		conn.Close(ctx)
		t.Fatalf("apply 181: %v", err)
	}
	if _, err := conn.Exec(ctx,
		`INSERT INTO fee_tiers (id, maker_bps, taker_bps) VALUES (1, 10, 20)`); err != nil {
		conn.Close(ctx)
		t.Fatalf("seed tier: %v", err)
	}
	conn.Close(ctx)

	poolCfg, err := pgxpool.ParseConfig(promoTestDSN())
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
		c, err := pgx.Connect(ctx, promoTestDSN())
		if err == nil {
			_, _ = c.Exec(ctx, "DROP SCHEMA IF EXISTS "+schema+" CASCADE")
			c.Close(ctx)
		}
	})
	st, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	return st, pool, ctx
}

// Full lifecycle: create → four-eyes → apply writes promo_* → second
// approve rejected → reject path → list.
func TestIntegrationPromoLifecycle(t *testing.T) {
	st, pool, ctx := promoItest(t)
	ends := time.Now().Add(2 * time.Hour)
	maker := decimal.NewFromInt(5)

	w, err := st.Create(ctx, 1, 100, &maker, nil, ends, nil)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if w.Status != StatusPending {
		t.Fatalf("status=%s want PENDING_APPROVAL", w.Status)
	}
	// Unknown tier fails fast.
	if _, err := st.Create(ctx, 999, 100, &maker, nil, ends, nil); !errors.Is(err, ErrTierMissing) {
		t.Fatalf("missing tier err=%v want ErrTierMissing", err)
	}
	// Four-eyes: creator cannot approve.
	if _, err := st.Approve(ctx, w.ID, 100); !errors.Is(err, ErrFourEyes) {
		t.Fatalf("self-approve err=%v want ErrFourEyes", err)
	}
	// Second principal approves.
	w2, err := st.Approve(ctx, w.ID, 200)
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	if w2.Status != StatusApplied || w2.ApprovedBy == nil || *w2.ApprovedBy != 200 {
		t.Fatalf("applied window=%+v", w2)
	}
	// fee_tiers got the promo values.
	var pm, pu *string
	var pt *string
	if err := pool.QueryRow(ctx,
		`SELECT promo_maker_bps::text, promo_taker_bps::text, promo_until::text
		   FROM fee_tiers WHERE id=1`).Scan(&pm, &pt, &pu); err != nil {
		t.Fatalf("tier read: %v", err)
	}
	if pm == nil || *pm != "5.0000" {
		t.Fatalf("promo_maker_bps=%v", pm)
	}
	if pt != nil {
		t.Fatalf("promo_taker_bps=%v — untouched side must stay NULL", *pt)
	}
	if pu == nil {
		t.Fatal("promo_until not set")
	}
	// Re-approve rejected.
	if _, err := st.Approve(ctx, w.ID, 300); !errors.Is(err, ErrNotPending) {
		t.Fatalf("re-approve err=%v want ErrNotPending", err)
	}
	// Reject path: creator cannot reject their own.
	w3, err := st.Create(ctx, 1, 100, nil, decN(t, 2), ends, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Reject(ctx, w3.ID, 100, nil); !errors.Is(err, ErrFourEyes) {
		t.Fatalf("self-reject err=%v want ErrFourEyes", err)
	}
	wr, err := st.Reject(ctx, w3.ID, 200, nil)
	if err != nil {
		t.Fatal(err)
	}
	if wr.Status != StatusRejected {
		t.Fatalf("status=%s want REJECTED", wr.Status)
	}
	// List surfaces both.
	list, err := st.List(ctx, "", 0)
	if err != nil || len(list) != 2 {
		t.Fatalf("list=%v err=%v", list, err)
	}
	pending, err := st.List(ctx, StatusPending, 0)
	if err != nil || len(pending) != 0 {
		t.Fatalf("pending list=%v err=%v", pending, err)
	}
}

// Approval-window enforcement: a window created 16+ minutes ago cannot
// be applied; an ends_at that passed inside the approval window reports
// ErrBadWindow.
func TestIntegrationApprovalWindow(t *testing.T) {
	st, pool, ctx := promoItest(t)
	maker := decimal.NewFromInt(5)

	// Backdate created_at past the 15-minute window.
	w, err := st.Create(ctx, 1, 100, &maker, nil,
		time.Now().Add(2*time.Hour), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE fee_promo_windows SET created_at = now() - interval '20 minutes'
		  WHERE id=$1`, w.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Approve(ctx, w.ID, 200); !errors.Is(err, ErrTooLate) {
		t.Fatalf("stale approve err=%v want ErrTooLate", err)
	}

	// Ends inside the approval window but already passed.
	w2, err := st.Create(ctx, 1, 100, &maker, nil,
		time.Now().Add(50*time.Millisecond), nil)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	if _, err := st.Approve(ctx, w2.ID, 200); !errors.Is(err, ErrBadWindow) {
		t.Fatalf("elapsed ends_at err=%v want ErrBadWindow", err)
	}
}

// ClearExpired: an applied window past ends_at clears the tier's
// promo_* columns and flips to EXPIRED; a later overlapping window's
// promo_until is preserved by the equality guard.
func TestIntegrationClearExpired(t *testing.T) {
	st, pool, ctx := promoItest(t)
	maker := decimal.NewFromInt(5)

	w, err := st.Create(ctx, 1, 100, &maker, nil,
		time.Now().Add(time.Hour), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Approve(ctx, w.ID, 200); err != nil {
		t.Fatal(err)
	}
	// Simulate expiry: move ends_at into the past and align the tier's
	// promo_until (the guard requires exact equality).
	past := time.Now().Add(-time.Minute).UTC()
	if _, err := pool.Exec(ctx,
		`UPDATE fee_promo_windows SET ends_at=$1 WHERE id=$2`, past, w.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE fee_tiers SET promo_until=$1 WHERE id=1`, past); err != nil {
		t.Fatal(err)
	}
	n, err := st.ClearExpired(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n < 2 {
		t.Fatalf("cleared=%d want ≥2 (tier + window)", n)
	}
	var promoUntil *time.Time
	var pm *decimal.Decimal
	if err := pool.QueryRow(ctx,
		`SELECT promo_until, promo_maker_bps FROM fee_tiers WHERE id=1`).
		Scan(&promoUntil, &pm); err != nil {
		t.Fatal(err)
	}
	if promoUntil != nil || pm != nil {
		t.Fatal("stale promo values not cleared")
	}
	got, err := st.Get(ctx, w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusExpired {
		t.Fatalf("status=%s want EXPIRED", got.Status)
	}

	// Overlap guard: apply a fresh window, then expire an older one —
	// the guard must not wipe the newer window's promo_until.
	w2, err := st.Create(ctx, 1, 100, &maker, nil,
		time.Now().Add(2*time.Hour), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Approve(ctx, w2.ID, 200); err != nil {
		t.Fatal(err)
	}
	// Backdate an orphan older window that also points at the tier, with
	// a different ends_at — the tier's promo_until no longer equals it.
	oldEnds := time.Now().Add(-time.Minute).UTC()
	if _, err := pool.Exec(ctx, `
		INSERT INTO fee_promo_windows
		    (fee_tier_id, promo_maker_bps, ends_at, status, created_by,
		     approved_by, approved_at)
		VALUES (1, 7, $1, 'APPLIED', 1, 2, now() - interval '1 hour')`,
		oldEnds); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ClearExpired(ctx); err != nil {
		t.Fatal(err)
	}
	var stillSet time.Time
	if err := pool.QueryRow(ctx,
		`SELECT promo_until FROM fee_tiers WHERE id=1`).Scan(&stillSet); err != nil {
		t.Fatal(err)
	}
	if stillSet.Before(time.Now()) {
		t.Fatalf("overlap guard wiped active promo: %v", stillSet)
	}
}
