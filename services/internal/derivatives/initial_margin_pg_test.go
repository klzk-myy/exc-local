// initial_margin_pg_test.go — PostgreSQL-backed coverage for migration
// 253 (umr_im_assessments): scratch-schema up/down round trip plus a
// persisted CALL_ISSUED assessment and same-day rerun idempotency via
// the (account_id, assessment_date) upsert. Gated on EXC_PG_TEST=1;
// DSN resolution mirrors pg_test.go (EXC_TEST_DSN, EXC_PG_DSN, then the
// 55433 default).
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

type umrFakeSens struct{ rows []IMSensitivity }

func (f umrFakeSens) Sensitivities(_ context.Context, _ int64) ([]IMSensitivity, error) {
	return f.rows, nil
}

type umrFakePosted struct{ v decimal.Decimal }

func (f umrFakePosted) PostedIMUSD(_ context.Context, _ int64) (decimal.Decimal, error) {
	return f.v, nil
}

type umrFakeIssuer struct{ calls int }

func (f *umrFakeIssuer) IssueIMCall(_ context.Context, _ *UMRAssessment) error {
	f.calls++
	return nil
}

// umrScratch provisions a scratch schema carrying the migration-043
// accounts.umr_in_scope anchor and migration 253, and returns a pool
// pinned to it.
func umrScratch(t *testing.T, up bool) *pgxpool.Pool {
	t.Helper()
	if os.Getenv("EXC_PG_TEST") != "1" {
		t.Skip("set EXC_PG_TEST=1 to run postgres-gated tests")
	}
	dsn := os.Getenv("EXC_TEST_DSN")
	if dsn == "" {
		dsn = os.Getenv("EXC_PG_DSN")
	}
	if dsn == "" {
		dsn = "postgres://postgres:postgres@127.0.0.1:55433/postgres?sslmode=disable"
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pgxpool: %v", err)
	}
	schema := fmt.Sprintf("umr_it_%d_%d", time.Now().UnixNano(), rand.Intn(1e6))
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

	// accounts anchor with the migration-043 umr_in_scope column.
	if _, err := pool.Exec(ctx, `
		CREATE TABLE accounts (
		    id           BIGSERIAL PRIMARY KEY,
		    umr_in_scope BOOLEAN NOT NULL DEFAULT false
		)`); err != nil {
		t.Fatalf("accounts anchor: %v", err)
	}
	for _, f := range []string{"253_umr_im_assessments.up.sql"} {
		sql, err := os.ReadFile(filepath.Join("..", "db", "migrations", f))
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		if _, err := pool.Exec(ctx, string(sql)); err != nil {
			t.Fatalf("apply %s: %v", f, err)
		}
	}
	if !up { // down-migration probe
		sql, err := os.ReadFile(filepath.Join("..", "db", "migrations",
			"253_umr_im_assessments.down.sql"))
		if err != nil {
			t.Fatalf("read down: %v", err)
		}
		if _, err := pool.Exec(ctx, string(sql)); err != nil {
			t.Fatalf("apply down: %v", err)
		}
	}
	return pool
}

func TestPgUMRIMAssess_PersistAndIdempotency(t *testing.T) {
	pool := umrScratch(t, true)
	ctx := context.Background()
	if _, err := pool.Exec(ctx,
		`INSERT INTO accounts (id, umr_in_scope) VALUES (7,true),(8,false)`); err != nil {
		t.Fatalf("seed accounts: %v", err)
	}
	params := DefaultUMRParams()
	params.MinTransferUSD = decimal.NewFromInt(1) // shrink MTA for the fixture
	issuer := &umrFakeIssuer{}
	svc, err := NewUMRIMService(pool, params,
		umrFakeSens{rows: []IMSensitivity{{
			PositionRef: 1, Label: "EUR/USD NDF",
			BaseCCY: "EUR", QuoteCCY: "USD",
			DeltaUSD: decimal.NewFromInt(10_000_000),
		}}},
		umrFakePosted{v: decimal.Zero}, nil, issuer, nil)
	if err != nil {
		t.Fatal(err)
	}
	day := time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC)

	n, err := svc.AssessDue(ctx, day)
	if err != nil {
		t.Fatalf("AssessDue: %v", err)
	}
	if n != 1 {
		t.Fatalf("AssessDue assessed %d want 1 (only umr_in_scope)", n)
	}
	a, err := svc.OutstandingCall(ctx, 7)
	if err != nil {
		t.Fatalf("OutstandingCall: %v", err)
	}
	if a == nil || a.Status != UMRStatusCallIssued {
		t.Fatalf("expected CALL_ISSUED row, got %+v", a)
	}
	if issuer.calls != 1 {
		t.Fatalf("issuer calls=%d want 1", issuer.calls)
	}
	// Same-day rerun: upsert the existing row, never a duplicate.
	if _, err := svc.Assess(ctx, 7, day); err != nil {
		t.Fatalf("rerun Assess: %v", err)
	}
	var rows int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM umr_im_assessments WHERE account_id=7`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Fatalf("rows=%d want 1 — same-day rerun must upsert", rows)
	}
}

func TestPgUMRIM_DownMigration(t *testing.T) {
	pool := umrScratch(t, false)
	var n int
	if err := pool.QueryRow(context.Background(), `
		SELECT count(*) FROM information_schema.tables
		WHERE table_schema=current_schema()
		  AND table_name='umr_im_assessments'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatal("umr_im_assessments survived the down migration")
	}
}
