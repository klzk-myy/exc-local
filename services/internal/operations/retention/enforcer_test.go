package retention_test

// Phase-09 Task 9.3.22 enforcer coverage — dry-run violations, apply-mode
// purge, hold carve-outs, audit rows, ClickHouse TTL seam. Gated on
// EXC_PG_TEST=1 (scratch DSN default).

import (
	"context"
	"fmt"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"exchange/internal/archiver"
	"exchange/internal/db"
	"exchange/internal/devs3"
	"exchange/internal/objectstore"
	"exchange/internal/operations/retention"
)

func pgPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	if os.Getenv("EXC_PG_TEST") != "1" {
		t.Skip("set EXC_PG_TEST=1 to run PostgreSQL integration tests")
	}
	dsn := os.Getenv("EXC_PG_DSN")
	if dsn == "" {
		dsn = "postgres://exchange:exchange_dev@/migverify?host=/tmp&port=55433&sslmode=disable"
	}
	pool, err := db.NewPool(context.Background(), dsn, 4)
	if err != nil {
		t.Skipf("no postgres: %v", err)
	}
	if err := pool.Ping(context.Background()); err != nil {
		pool.Close()
		t.Skipf("postgres unreachable: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func applyMigrations(t *testing.T, pool *pgxpool.Pool, names ...string) {
	t.Helper()
	for _, n := range names {
		raw, err := os.ReadFile(filepath.Join("..", "..", "db", "migrations", n))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(context.Background(), string(raw)); err != nil {
			if !strings.Contains(err.Error(), "already exists") &&
				!strings.Contains(err.Error(), "duplicate") {
				t.Fatalf("migration %s: %v", n, err)
			}
		}
	}
}

// testPolicy covers a purgeable table class plus a partitioned class.
func testPolicy() *retention.Policy {
	return &retention.Policy{Version: 1, Classes: []retention.DataClass{
		{Name: "itest_dedup", Store: retention.StoreTablePG,
			Table: "itest_dedup", TSColumn: "created_at",
			RetainDays: 7, Purgeable: true,
			Mechanism: "row purge", Basis: "test"},
		{Name: "itest_enf", Store: retention.StorePartitionedPG,
			Table: "itest_enf", HotDays: 0, WarmDays: 365,
			RetainDays: 1825, Mechanism: "tiered", Basis: "test"},
	}}
}

func setupTables(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	for _, s := range []string{
		`DROP TABLE IF EXISTS itest_dedup`,
		`CREATE TABLE itest_dedup (id bigint, created_at timestamptz NOT NULL)`,
		`INSERT INTO itest_dedup VALUES
			(1, now() - interval '30 days'),
			(2, now() - interval '10 days'),
			(3, now())`,
		`DELETE FROM data_retention_holds WHERE parent_table IN ('itest_dedup','itest_enf')`,
		`DROP TABLE IF EXISTS itest_enf CASCADE`,
		`CREATE SCHEMA IF NOT EXISTS warm`,
		`DROP TABLE IF EXISTS warm.itest_enf_p2020_01`,
		`CREATE TABLE itest_enf (id bigint, created_at timestamptz NOT NULL)
			PARTITION BY RANGE (created_at)`,
		`CREATE TABLE itest_enf_p2020_01 PARTITION OF itest_enf
			FOR VALUES FROM ('2020-01-01') TO ('2020-02-01')`,
		`INSERT INTO itest_enf VALUES (1,'2020-01-05')`,
		`DELETE FROM partition_tier_state WHERE parent_table='itest_enf'`,
	} {
		if _, err := pool.Exec(ctx, s); err != nil {
			t.Fatalf("setup: %v\n%s", err, s)
		}
	}
}

func findingsFor(rep *retention.Report, dataType, check string) []retention.Finding {
	var out []retention.Finding
	for _, f := range rep.Findings {
		if f.DataType == dataType && f.Check == check {
			out = append(out, f)
		}
	}
	return out
}

func TestEnforcerDryRunAndApply(t *testing.T) {
	pool := pgPool(t)
	ctx := context.Background()
	applyMigrations(t, pool,
		"120_partition_archive_log.up.sql",
		"194_data_retention_holds.up.sql",
		"195_partition_tier_state.up.sql")
	setupTables(t, pool)

	enf := retention.New(pool, testPolicy())

	// Dry-run: expired dedup rows + old attached partition = violations.
	rep, err := enf.Run(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Mode != "DRY_RUN" || rep.Violations == 0 {
		t.Fatalf("dry-run: %+v", rep)
	}
	exp := findingsFor(rep, "itest_dedup", "expired_rows")
	if len(exp) != 1 || exp[0].Result != retention.ResultViolation {
		t.Fatalf("dedup findings: %+v", exp)
	}
	hw := findingsFor(rep, "itest_enf", "hot_window")
	if len(hw) != 1 || hw[0].Result != retention.ResultViolation {
		t.Fatalf("hot_window findings: %+v", hw)
	}
	// Dry-run never mutates.
	var n int64
	pool.QueryRow(ctx, `SELECT count(*) FROM itest_dedup`).Scan(&n)
	if n != 3 {
		t.Fatalf("dry-run deleted rows: %d", n)
	}
	// Audit rows exist for every finding.
	pool.QueryRow(ctx,
		`SELECT count(*) FROM retention_audit_log WHERE run_id = $1`,
		rep.RunID).Scan(&n)
	if int(n) != len(rep.Findings) {
		t.Fatalf("audit rows %d != findings %d", n, len(rep.Findings))
	}
	pool.QueryRow(ctx, `
		SELECT count(*) FROM retention_audit_log
		WHERE run_id=$1 AND result='VIOLATION'`, rep.RunID).Scan(&n)
	if n < 2 {
		t.Fatalf("violation audit rows %d", n)
	}

	// Apply: expired rows purged; partition moved via the mover seam.
	c, done := devStore(t)
	defer done()
	a := archiver.New(pool, c)
	a.SetClassPolicies([]archiver.ClassPolicy{{
		Parent: "itest_enf", HotDays: 0, WarmDays: 99999, RetainDays: 1825}})
	enf.SetStore(c)
	enf.SetMover(mover{a})

	rep, err = enf.Run(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Mode != "APPLY" || rep.Actions == 0 {
		t.Fatalf("apply: %+v", rep)
	}
	pool.QueryRow(ctx, `SELECT count(*) FROM itest_dedup`).Scan(&n)
	if n != 1 {
		t.Fatalf("purge left %d rows", n)
	}
	// Partition moved to warm schema by the mover.
	var tier string
	if err := pool.QueryRow(ctx, `
		SELECT tier FROM partition_tier_state
		WHERE partition_name='itest_enf_p2020_01'`).Scan(&tier); err != nil || tier != "WARM" {
		t.Fatalf("post-apply tier %q %v", tier, err)
	}
}

func TestEnforcerHoldBlocksPurge(t *testing.T) {
	pool := pgPool(t)
	ctx := context.Background()
	applyMigrations(t, pool,
		"194_data_retention_holds.up.sql")
	setupTables(t, pool)

	if _, err := pool.Exec(ctx, `
		INSERT INTO data_retention_holds
		  (parent_table, case_ref, reason, created_by)
		VALUES ('itest_dedup','CASE-1','litigation','cco')`); err != nil {
		t.Fatal(err)
	}
	enf := retention.New(pool, testPolicy())
	rep, err := enf.Run(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	fs := findingsFor(rep, "itest_dedup", "expired_rows")
	if len(fs) != 1 || fs[0].Result != retention.ResultHeld {
		t.Fatalf("expected HELD: %+v", fs)
	}
	var n int64
	pool.QueryRow(ctx, `SELECT count(*) FROM itest_dedup`).Scan(&n)
	if n != 3 {
		t.Fatalf("purge ran under hold: %d", n)
	}
}

// fakeCH returns a canned count for TTL drift checks.
type fakeCH struct{ body string }

func (f fakeCH) QueryCSV(_ context.Context, _ string) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader(f.body)), nil
}

func TestEnforcerClickHouseTTL(t *testing.T) {
	pool := pgPool(t)
	pol := &retention.Policy{Version: 1, Classes: []retention.DataClass{{
		Name: "ticks", Store: retention.StoreClickHouse,
		Table: "tick_history", TSColumn: "timestamp", RetainDays: 90,
	}}}
	enf := retention.New(pool, pol)

	// No querier → SKIPPED.
	rep, _ := enf.Run(context.Background(), false)
	if findingsFor(rep, "ticks", "ttl_drift")[0].Result != retention.ResultSkipped {
		t.Fatalf("expected SKIPPED: %+v", rep.Findings)
	}

	// Drift → VIOLATION.
	enf.SetCH(fakeCH{body: "42\n"})
	rep, _ = enf.Run(context.Background(), false)
	f := findingsFor(rep, "ticks", "ttl_drift")[0]
	if f.Result != retention.ResultViolation {
		t.Fatalf("expected VIOLATION: %+v", f)
	}
}

// devStore wires a devs3-backed objectstore client.
func devStore(t *testing.T) (objectstore.Client, func()) {
	t.Helper()
	srv, err := devs3.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv)
	c, err := objectstore.NewDev(context.Background(), objectstore.Config{
		Bucket:   fmt.Sprintf("ret-%d", time.Now().UnixNano()),
		Endpoint: ts.URL,
	})
	if err != nil {
		ts.Close()
		t.Fatal(err)
	}
	return c, ts.Close
}

type mover struct{ a *archiver.Archiver }

func (m mover) MoveParent(ctx context.Context, parent string) error {
	return m.a.RunLifecycleParent(ctx, parent)
}
