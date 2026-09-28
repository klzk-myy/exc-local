package archiver_test

// Phase-09 Tasks 9.3.17/9.3.22/9.3.24 coverage: compliance holds,
// hot→warm→cold lifecycle moves with tier bookkeeping, and the WORM
// integrity drill. Gated on EXC_PG_TEST=1 (scratch DSN default).

import (
	"context"
	"fmt"
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
)

func pgPoolGated(t *testing.T) *pgxpool.Pool {
	t.Helper()
	if os.Getenv("EXC_PG_TEST") != "1" {
		t.Skip("set EXC_PG_TEST=1 to run PostgreSQL integration tests")
	}
	dsn := os.Getenv("EXC_PG_DSN")
	if dsn == "" {
		dsn = "postgres://exchange:exchange_dev@/migverify?host=/tmp&port=55433&sslmode=disable"
	}
	ctx := context.Background()
	pool, err := db.NewPool(ctx, dsn, 4)
	if err != nil {
		t.Skipf("no postgres: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Skipf("postgres unreachable: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// applyMigrations runs the real migration files the feature depends on.
func applyMigrations(t *testing.T, pool *pgxpool.Pool, names ...string) {
	t.Helper()
	for _, n := range names {
		raw, err := os.ReadFile(filepath.Join("..", "db", "migrations", n))
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

func wormClientGated(t *testing.T) (objectstore.Client, func()) {
	t.Helper()
	srv, err := devs3.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv)
	c, err := objectstore.NewDev(context.Background(), objectstore.Config{
		Bucket:   fmt.Sprintf("lc-%d", time.Now().UnixNano()),
		Endpoint: ts.URL,
	})
	if err != nil {
		ts.Close()
		t.Fatal(err)
	}
	return c, ts.Close
}

func setupLifecycleTable(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	for _, s := range []string{
		`DROP TABLE IF EXISTS itest_lc CASCADE`,
		`CREATE SCHEMA IF NOT EXISTS warm`,
		`CREATE TABLE itest_lc (
			id bigint NOT NULL, created_at timestamptz NOT NULL
		 ) PARTITION BY RANGE (created_at)`,
		`DROP TABLE IF EXISTS warm.itest_lc_p2020_01`,
		`DROP TABLE IF EXISTS warm.itest_lc_p2020_02`,
		`CREATE TABLE itest_lc_p2020_01 PARTITION OF itest_lc
			FOR VALUES FROM ('2020-01-01') TO ('2020-02-01')`,
		`CREATE TABLE itest_lc_p2020_02 PARTITION OF itest_lc
			FOR VALUES FROM ('2020-02-01') TO ('2020-03-01')`,
		`INSERT INTO itest_lc (id, created_at) VALUES
			(1,'2020-01-05'),(2,'2020-01-06'),(3,'2020-01-07'),
			(4,'2020-02-05'),(5,'2020-02-06')`,
	} {
		if _, err := pool.Exec(ctx, s); err != nil {
			t.Fatalf("setup: %v\n%s", err, s)
		}
	}
}

func lcPolicy(hot, warm int) []archiver.ClassPolicy {
	return []archiver.ClassPolicy{{
		Parent: "itest_lc", HotDays: hot, WarmDays: warm,
		RetainDays: 1825, WarmSchema: "warm",
	}}
}

func countRows(t *testing.T, pool *pgxpool.Pool, q string) int64 {
	t.Helper()
	var n int64
	if err := pool.QueryRow(context.Background(), q).Scan(&n); err != nil {
		t.Fatalf("count: %v\n%s", err, q)
	}
	return n
}

func TestLifecycleHotToWarmToCold(t *testing.T) {
	pool := pgPoolGated(t)
	ctx := context.Background()
	applyMigrations(t, pool,
		"120_partition_archive_log.up.sql",
		"194_data_retention_holds.up.sql",
		"195_partition_tier_state.up.sql")
	setupLifecycleTable(t, pool)
	c, done := wormClientGated(t)
	defer done()

	a := archiver.New(pool, c)
	a.SetClassPolicies(lcPolicy(0, 99999)) // everything past hot, nothing past warm

	// Dry-run first: plans without touching anything.
	rep, err := a.RunLifecycle(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	planned := 0
	for _, m := range rep.Moves {
		if m.Result != "PLANNED" {
			t.Fatalf("dry-run move not PLANNED: %+v", m)
		}
		planned++
	}
	if planned != 2 {
		t.Fatalf("expected 2 planned moves, got %d (%+v)", planned, rep.Moves)
	}
	// Nothing moved yet.
	if countRows(t, pool, `SELECT count(*) FROM itest_lc`) != 5 {
		t.Fatal("dry-run mutated data")
	}

	// Apply: both partitions detach to the warm schema.
	rep, err = a.RunLifecycle(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range rep.Moves {
		if m.Result != "MOVED" || m.ToTier != "WARM" {
			t.Fatalf("unexpected move: %+v", m)
		}
	}
	if got := countRows(t, pool, `SELECT count(*) FROM warm.itest_lc_p2020_01`); got != 3 {
		t.Fatalf("warm rows = %d", got)
	}
	// Detached: no longer reachable through the parent.
	if got := countRows(t, pool, `SELECT count(*) FROM itest_lc`); got != 0 {
		t.Fatalf("parent still sees %d rows", got)
	}
	// Tier bookkeeping.
	var tier string
	if err := pool.QueryRow(ctx, `
		SELECT tier FROM partition_tier_state
		WHERE partition_name='itest_lc_p2020_01'`).Scan(&tier); err != nil || tier != "WARM" {
		t.Fatalf("tier_state %q %v", tier, err)
	}
	if got := countRows(t, pool, `
		SELECT count(*) FROM partition_tier_log WHERE to_tier='WARM' AND result='MOVED'`); got < 2 {
		t.Fatalf("tier_log rows %d", got)
	}

	// Second pass with warm boundary crossed → cold export + drop.
	a.SetClassPolicies(lcPolicy(0, 0))
	rep, err = a.RunLifecycle(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	movedCold := 0
	for _, m := range rep.Moves {
		if m.Result == "MOVED" && m.ToTier == "COLD" {
			movedCold++
			if m.Checksum == "" || m.ArchiveID == 0 {
				t.Fatalf("cold move missing evidence: %+v", m)
			}
		}
	}
	if movedCold != 2 {
		t.Fatalf("expected 2 cold moves: %+v", rep.Moves)
	}
	if countRows(t, pool,
		`SELECT count(*) FROM partition_tier_state WHERE parent_table='itest_lc' AND tier='COLD'`) != 2 {
		t.Fatal("tier_state not COLD")
	}
	// Warm tables gone; objects in the bucket.
	var reg *string
	pool.QueryRow(ctx, `SELECT to_regclass('warm.itest_lc_p2020_01')::text`).Scan(&reg)
	if reg != nil {
		t.Fatal("warm table not dropped")
	}
	objs, err := objectstore.ListAll(ctx, c, "itest_lc/")
	if err != nil || len(objs) != 4 { // 2 partitions × (data+manifest)
		t.Fatalf("s3 objects %d %v", len(objs), err)
	}
}

func TestComplianceHoldBlocksLifecycle(t *testing.T) {
	pool := pgPoolGated(t)
	ctx := context.Background()
	applyMigrations(t, pool,
		"120_partition_archive_log.up.sql",
		"194_data_retention_holds.up.sql",
		"195_partition_tier_state.up.sql")
	setupLifecycleTable(t, pool)
	c, done := wormClientGated(t)
	defer done()

	a := archiver.New(pool, c)
	a.SetClassPolicies(lcPolicy(0, 0))

	// Hold one partition; the other must still move.
	hid, err := a.AddHold(ctx, "itest_lc", "itest_lc_p2020_01",
		"FCA-2026-0001", "regulatory investigation", "cco@exchange", nil)
	if err != nil {
		t.Fatal(err)
	}
	rep, err := a.RunLifecycle(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	var heldMove int
	moved02 := false
	for _, m := range rep.Moves {
		switch {
		case m.Result == "HELD":
			if m.Partition != "itest_lc_p2020_01" {
				t.Fatalf("wrong partition held: %+v", m)
			}
			heldMove++
		case m.Result == "MOVED" && m.Partition == "itest_lc_p2020_02":
			moved02 = true // may move twice in one pass (hot→warm→cold)
		case m.Partition == "itest_lc_p2020_01" && m.Result != "HELD" && m.Result != "SKIPPED":
			t.Fatalf("held partition acted on: %+v", m)
		}
	}
	if heldMove != 1 || !moved02 {
		t.Fatalf("holds/moves %d/%v: %+v", heldMove, moved02, rep.Moves)
	}
	// Held partition still attached, data reachable.
	if got := countRows(t, pool,
		`SELECT count(*) FROM itest_lc WHERE created_at < '2020-02-01'`); got != 3 {
		t.Fatalf("held rows unreachable: %d", got)
	}
	// Direct archive path also blocked.
	parts, _ := a.ListPartitions(ctx)
	for _, p := range parts {
		if p.Name == "itest_lc_p2020_01" {
			_, err := a.ArchivePartition(ctx, p)
			if err == nil || !strings.Contains(err.Error(), "hold") {
				t.Fatalf("expected HeldError, got %v", err)
			}
		}
	}
	// Release → next pass moves it (HELD rows may also appear for its
	// warm→cold stage if a hold were added mid-flight; here none).
	if err := a.ReleaseHold(ctx, hid, "cco@exchange"); err != nil {
		t.Fatal(err)
	}
	rep, err = a.RunLifecycle(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	moved01 := false
	for _, m := range rep.Moves {
		if m.Partition == "itest_lc_p2020_01" && m.Result == "MOVED" {
			moved01 = true
		}
		if m.Partition == "itest_lc_p2020_01" && (m.Result == "ERROR" || m.Result == "HELD") {
			t.Fatalf("post-release move: %+v", m)
		}
	}
	if !moved01 {
		t.Fatalf("released partition never moved: %+v", rep.Moves)
	}
}

// corruptGet wraps the client to damage one object's bytes on download —
// exercises the drill's detection path.
type corruptGet struct {
	objectstore.Client
	key string
}

func (c corruptGet) Get(ctx context.Context, k string) ([]byte, objectstore.Object, error) {
	b, o, err := c.Client.Get(ctx, k)
	if err == nil && strings.Contains(k, c.key) && len(b) > 0 {
		b[0] ^= 0xFF
	}
	return b, o, err
}

func TestVerifyDrillDetectsCorruption(t *testing.T) {
	pool := pgPoolGated(t)
	ctx := context.Background()
	applyMigrations(t, pool,
		"120_partition_archive_log.up.sql",
		"194_data_retention_holds.up.sql",
		"195_partition_tier_state.up.sql")
	setupLifecycleTable(t, pool)
	c, done := wormClientGated(t)
	defer done()

	// Archive one partition straight to cold.
	a := archiver.New(pool, c)
	a.SetClassPolicies(lcPolicy(0, 0))
	if _, err := a.RunLifecycle(ctx, false); err != nil {
		t.Fatal(err)
	}

	// Clean drill passes.
	rep, err := a.VerifyDrill(ctx, 0)
	if err != nil {
		t.Fatalf("clean drill failed: %v", err)
	}
	if rep.Samples == 0 || rep.Failures != 0 {
		t.Fatalf("drill: %+v", rep)
	}
	// Audit rows written.
	if countRows(t, pool, `
		SELECT count(*) FROM retention_audit_log
		WHERE check_name='worm_integrity' AND result='OK'`) < 2 {
		t.Fatal("drill audit rows missing")
	}

	// Corrupt one object → drill must fail and log a VIOLATION.
	ca := archiver.New(pool, corruptGet{Client: c, key: "itest_lc_p2020_01.csv.zst"})
	rep, err = ca.VerifyDrill(ctx, 0)
	if err == nil {
		t.Fatal("corrupt archive passed the drill")
	}
	if rep.Failures == 0 {
		t.Fatalf("corruption undetected: %+v", rep)
	}
	if countRows(t, pool, `
		SELECT count(*) FROM retention_audit_log
		WHERE check_name='worm_integrity' AND result='VIOLATION'`) < 1 {
		t.Fatal("violation not audited")
	}
}
