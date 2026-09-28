// Task 5.3.20 — PG-backed deprecation-store tests. Gated:
// EXC_PG_TEST=1, DSN via EXC_PG_DSN or EXC_TEST_DSN (default: scratch w2d, /tmp socket).
//
// Applies the real 182 migration — it has no extension dependencies.
//
// Run: EXC_PG_TEST=1 go test ./internal/deprecation/ -run Integration -v
package deprecation

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func depTestDSN() string {
	if d := os.Getenv("EXC_PG_DSN"); d != "" {
		return d
	}
	if d := os.Getenv("EXC_TEST_DSN"); d != "" {
		return d
	}
	return "postgres://postgres@/w2d?host=/tmp&port=55433"
}

func depItest(t *testing.T) (*Store, *pgxpool.Pool, context.Context) {
	t.Helper()
	if os.Getenv("EXC_PG_TEST") != "1" {
		t.Skip("set EXC_PG_TEST=1 to run PostgreSQL integration tests")
	}
	ctx := context.Background()
	schema := fmt.Sprintf("dep_itest_%d", time.Now().UnixNano())

	cfg, err := pgx.ParseConfig(depTestDSN())
	if err != nil {
		t.Fatalf("dsn: %v", err)
	}
	cfg.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol
	cfg.RuntimeParams["search_path"] = schema + ",public"
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Skipf("postgres unreachable: %v", err)
	}
	ddl, err := os.ReadFile("../db/migrations/182_api_deprecations.up.sql")
	if err != nil {
		conn.Close(ctx)
		t.Fatalf("read migration 182: %v", err)
	}
	if _, err := conn.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		conn.Close(ctx)
		t.Fatalf("create schema: %v", err)
	}
	if _, err := conn.Exec(ctx, string(ddl)); err != nil {
		conn.Close(ctx)
		t.Fatalf("apply 182: %v", err)
	}
	conn.Close(ctx)

	poolCfg, err := pgxpool.ParseConfig(depTestDSN())
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
		c, err := pgx.Connect(ctx, depTestDSN())
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

// Round-trip against the real 182 table: announce, list, uniqueness
// indexes, and the six-month CHECK at the DB layer.
func TestIntegrationDeprecations(t *testing.T) {
	st, pool, ctx := depItest(t)

	post := "POST"
	created, err := st.Announce(ctx, Rule{
		Path:        "/api/v1/legacy-orders",
		Method:      &post,
		AnnouncedAt: time.Now().UTC(),
		SunsetAt:    time.Now().UTC().Add(MinNotice + time.Hour),
		Replacement: strptr("/api/v1/orders"),
		CreatedBy:   42,
	})
	if err != nil {
		t.Fatalf("announce: %v", err)
	}
	if created.ID == 0 {
		t.Fatal("no id assigned")
	}
	if created.MigrationURL != "/developer/migration" {
		t.Fatalf("migration_url default lost: %q", created.MigrationURL)
	}

	// Duplicate (method, path) → unique index violation.
	if _, err := st.Announce(ctx, Rule{
		Path:      "/api/v1/legacy-orders",
		Method:    &post,
		SunsetAt:  time.Now().UTC().Add(MinNotice + time.Hour),
		CreatedBy: 42,
	}); err == nil {
		t.Fatal("duplicate rule accepted")
	}
	// The NULL-method slot is separate: an all-method rule on the same
	// path is legal (its own unique index).
	if _, err := st.Announce(ctx, Rule{
		Path:      "/api/v1/legacy-orders",
		SunsetAt:  time.Now().UTC().Add(MinNotice + time.Hour),
		CreatedBy: 42,
	}); err != nil {
		t.Fatalf("NULL-method rule rejected: %v", err)
	}

	// Sunset inside the notice window is rejected by Validate before the
	// DB — verify the DB CHECK would catch it anyway.
	if _, dbErr := pool.Exec(ctx, `
		INSERT INTO api_deprecations
		    (path, method, announced_at, sunset_at, created_by)
		VALUES ('/api/v1/too-soon', NULL, now(), now() + interval '1 month', 1)`); dbErr == nil {
		t.Fatal("six-month CHECK violated")
	}

	list, err := st.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Fatalf("list=%d want 2 (too-soon insert rejected)", len(list))
	}
	if list[0].Path == list[1].Path && list[0].Method == nil && list[1].Method == nil {
		t.Fatal("NULL-method dedupe broken")
	}
}

// End-to-end middleware over the real store: announced → headers;
// sunset passed → 410 ENDPOINT_GONE.
func TestIntegrationMiddlewareGone(t *testing.T) {
	st, _, ctx := depItest(t)
	// Sunset already passed; announced ~8 months ago so the DB six-month
	// (calendar) CHECK holds with margin.
	rule := Rule{
		Path:        "/api/v1/legacy",
		AnnouncedAt: time.Now().UTC().Add(-240 * 24 * time.Hour),
		SunsetAt:    time.Now().UTC().Add(-time.Minute),
		CreatedBy:   1,
	}
	if _, err := st.Announce(ctx, rule); err != nil {
		t.Fatalf("announce sunset rule: %v", err)
	}
	h := Middleware(st, writeProblem, nil)(http.HandlerFunc(next200))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/v1/legacy", nil))
	if rec.Code != http.StatusGone {
		t.Fatalf("past-sunset status=%d want 410", rec.Code)
	}
}
