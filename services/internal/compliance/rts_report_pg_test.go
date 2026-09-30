// PostgreSQL-gated integration tests for Task 21.3.19 RTS 27/28 —
// migration 251 schema round-trip, DRAFT/PUBLISHED lifecycle and the
// public-read boundary (DRAFT artifacts never surface). ClickHouse-
// backed aggregation is covered separately (rts_ch_test.go).
// EXC_PG_TEST=1.
package compliance

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	excerrors "exchange/pkg/errors"
)

func rtsPool(t *testing.T) (context.Context, *pgxpool.Pool) {
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
	schema := fmt.Sprintf("rts_it_%d_%d", time.Now().UnixNano(), rand.Intn(1e6))
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

func rtsExecFile(t *testing.T, ctx context.Context, pool *pgxpool.Pool,
	name string) {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("..", "db", "migrations", name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	if _, err := pool.Exec(ctx, string(body)); err != nil {
		t.Fatalf("exec %s: %v", name, err)
	}
}

func rtsSchema(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(ctx, `
		CREATE TABLE instruments (
		    id              BIGSERIAL PRIMARY KEY,
		    symbol          VARCHAR(32) NOT NULL,
		    instrument_type VARCHAR(16) NOT NULL,
		    base_currency   CHAR(3) NOT NULL,
		    quote_currency  CHAR(3) NOT NULL
		);
		INSERT INTO instruments (symbol, instrument_type, base_currency,
		                       quote_currency)
		VALUES ('EURUSD','SPOT','EUR','USD')`); err != nil {
		t.Fatalf("anchors: %v", err)
	}
	rtsExecFile(t, ctx, pool, "009_create_audit_hash_chain.up.sql")
	rtsExecFile(t, ctx, pool, "251_best_execution_reports.up.sql")
}

func rtsOfficer(context.Context, int64) (string, error) {
	return "Compliance Officer", nil
}

// Daily stats → quarterly DRAFT → publish → public reads. Versioning:
// regeneration lands version+1 and the older DRAFT stays private.
func TestITRTS27Lifecycle(t *testing.T) {
	ctx, pool := rtsPool(t)
	rtsSchema(t, ctx, pool)
	svc, err := NewRTS27Service(pool, nil, rtsOfficer)
	if err != nil {
		t.Fatal(err)
	}
	// No ClickHouse → materialization fails closed.
	if _, err := svc.MaterializeDay(ctx, time.Now()); excerrors.CodeOf(err) != "SERVICE_DEGRADED" {
		t.Fatalf("nil CH must fail closed SERVICE_DEGRADED, got %v", err)
	}
	// Seed one daily-stats row (as MaterializeDay would have written).
	if _, err := pool.Exec(ctx, `
		INSERT INTO rts27_daily_stats
		    (day, instrument_id, symbol, product_type, pair_class,
		     fills, volume_base, volume_quote, vwap,
		     price_min, price_max, price_median, price_mean,
		     agg_buy_fills, agg_sell_fills, unknown_fills,
		     agg_buy_volume, agg_sell_volume,
		     orders_submitted, orders_filled, fill_rate,
		     median_order_to_fill_ms, tca_fills,
		     slip_arrival_avg_bps, slip_arrival_med_bps,
		     slip_vwap_avg_bps, improvement_avg,
		     spread_avg_bps, inputs_complete, gaps)
		VALUES ('2026-01-15', 1, 'EURUSD', 'SPOT', 'FX_MAJOR',
		        100, 5000, 6200, 1.24, 1.20, 1.28, 1.24, 1.24,
		        60, 35, 5, 3000, 1900,
		        120, 100, 0.8333, 42.0, 80,
		        1.5, 1.4, 2.1, 0.02,
		        NULL, true, '[]'::jsonb)`); err != nil {
		t.Fatalf("seed daily stats: %v", err)
	}
	reps, err := svc.GenerateQuarter(ctx, time.Date(2026, 1, 1, 0, 0, 0, 0,
		time.UTC), 100)
	if err != nil || len(reps) != 1 {
		t.Fatalf("generate: n=%d err=%v", len(reps), err)
	}
	r := reps[0]
	if r.Status != "DRAFT" || r.Version != 1 ||
		r.InstrumentClass != "SPOT:FX_MAJOR" {
		t.Fatalf("draft: %+v", r)
	}
	// DRAFT never reaches the public surface.
	if err := func() error {
		_, err := svc.GetPublished(ctx, r.ID)
		return err
	}(); excerrors.CodeOf(err) != "NOT_FOUND" {
		t.Fatalf("draft must not be public, got %v", err)
	}
	pub, err := svc.Publish(ctx, r.ID, 100)
	if err != nil || pub.Status != "PUBLISHED" {
		t.Fatalf("publish: %+v err=%v", pub, err)
	}
	// Regeneration versions forward.
	reps2, err := svc.GenerateQuarter(ctx, time.Date(2026, 2, 15, 0, 0, 0,
		0, time.UTC), 100)
	if err != nil || len(reps2) != 1 || reps2[0].Version != 2 {
		t.Fatalf("regeneration must land v2: %+v err=%v", reps2, err)
	}
	l, err := svc.ListPublished(ctx)
	if err != nil || len(l) != 1 {
		t.Fatalf("public list: n=%d err=%v", len(l), err)
	}
	if l[0].CSV != "" {
		t.Fatal("public list must not carry the CSV blob")
	}
	got, err := svc.GetPublished(ctx, r.ID)
	if err != nil || got.CSV == "" {
		t.Fatalf("public get: %+v err=%v", got, err)
	}
	// Second publish of the same row is refused.
	if _, err := svc.Publish(ctx, r.ID, 100); err == nil {
		t.Fatal("re-publish must fail")
	}
}

// RTS 28 publish requires the qualitative assessment (RTS 28 table 3);
// the public boundary mirrors RTS 27.
func TestITRTS28Lifecycle(t *testing.T) {
	ctx, pool := rtsPool(t)
	rtsSchema(t, ctx, pool)
	svc, err := NewRTS28Service(pool, nil, rtsOfficer)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.GenerateYear(ctx, 2026, 100); excerrors.CodeOf(err) != "SERVICE_DEGRADED" {
		t.Fatalf("nil CH must fail closed, got %v", err)
	}
	var id int64
	if err := pool.QueryRow(ctx, `
		INSERT INTO rts28_reports (year, instrument_class, categories, csv,
		                           generated_by)
		VALUES (2026,'SPOT:FX_MAJOR','{"RETAIL":{}}'::jsonb,'y,c\n',100)
		RETURNING id`).Scan(&id); err != nil {
		t.Fatalf("seed rts28: %v", err)
	}
	// Publish without narrative is refused.
	if _, err := svc.Publish(ctx, id, 100, "  "); excerrors.CodeOf(err) != "INVALID_REQUEST" {
		t.Fatalf("blank qualitative assessment must reject, got %v", err)
	}
	if _, err := svc.GetPublished(ctx, id); excerrors.CodeOf(err) != "NOT_FOUND" {
		t.Fatalf("draft not public, got %v", err)
	}
	rep, err := svc.Publish(ctx, id, 100,
		"Execution quality satisfactory; venue coverage complete.")
	if err != nil || rep.Status != "PUBLISHED" ||
		rep.QualitativeAssessment == "" {
		t.Fatalf("publish: %+v err=%v", rep, err)
	}
}
