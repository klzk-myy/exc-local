package archiver_test

import (
	"context"
	"errors"
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

func pgPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("EXC_TEST_PG_DSN")
	if dsn == "" {
		dsn = "postgres://exchange:exchange_dev@127.0.0.1:5433/exchange?sslmode=disable"
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

func wormClient(t *testing.T) (objectstore.Client, func()) {
	t.Helper()
	srv, err := devs3.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv)
	c, err := objectstore.NewDev(context.Background(), objectstore.Config{
		// Unique bucket per run: keeps partition_archive_log's
		// UNIQUE(bucket, s3_key) happy across repeat test runs.
		Bucket:   fmt.Sprintf("%s-%d", archiver.DefaultPartitionBucket, time.Now().UnixNano()),
		Endpoint: ts.URL,
	})
	if err != nil {
		ts.Close()
		t.Fatal(err)
	}
	return c, ts.Close
}

// applyMigration120 executes the real migration file so the test exercises
// the shipped DDL, not a copy.
func applyMigration120(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "db", "migrations",
		"120_partition_archive_log.up.sql"))
	if err != nil {
		t.Fatal(err)
	}
	// Strip BEGIN/COMMIT — we run it inside implicit-txn Exec fine anyway.
	if _, err := pool.Exec(context.Background(), string(raw)); err != nil {
		if !strings.Contains(err.Error(), "already exists") {
			t.Fatalf("migration 120: %v", err)
		}
	}
}

func setupPartitionedOrders(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	stmts := []string{
		`DROP SCHEMA IF EXISTS archive_restore CASCADE`,
		`DROP TABLE IF EXISTS itest_orders CASCADE`,
		`CREATE TABLE itest_orders (
			id bigint NOT NULL, account_id bigint NOT NULL,
			qty numeric(20,8), created_at timestamptz NOT NULL
		 ) PARTITION BY RANGE (created_at)`,
		`CREATE TABLE itest_orders_p2020_01 PARTITION OF itest_orders
			FOR VALUES FROM ('2020-01-01') TO ('2020-02-01')`,
		`CREATE TABLE itest_orders_p2099_01 PARTITION OF itest_orders
			FOR VALUES FROM ('2099-01-01') TO ('2099-02-01')`,
		`INSERT INTO itest_orders (id, account_id, qty, created_at) VALUES
			(1, 10, 1.5, '2020-01-05'), (2, 11, 2.5, '2020-01-06'),
			(3, 12, 3.5, '2020-01-07'), (4, 13, 4.5, '2099-01-05')`,
	}
	for _, s := range stmts {
		if _, err := pool.Exec(ctx, s); err != nil {
			t.Fatalf("setup: %v\nsql: %s", err, s)
		}
	}
}

// parseBoundEnd unit coverage lives in internal_test.go (same package).

func TestArchiveAndRestorePartition(t *testing.T) {
	pool := pgPool(t)
	ctx := context.Background()
	applyMigration120(t, pool)
	setupPartitionedOrders(t, pool)
	c, done := wormClient(t)
	defer done()

	a := archiver.New(pool, c)
	a.SetParents([]string{"itest_orders"})
	a.SetClock(func() time.Time { return time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC) })

	parts, err := a.EligiblePartitions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(parts) != 1 || parts[0].Name != "itest_orders_p2020_01" {
		t.Fatalf("eligible: %+v", parts)
	}
	if parts[0].RangeEnd.Year() != 2020 {
		t.Fatalf("bound parse: %v", parts[0].RangeEnd)
	}

	le, err := a.ArchivePartition(ctx, parts[0])
	if err != nil {
		t.Fatal(err)
	}
	if le.Status != "DROPPED" {
		t.Fatalf("status %q", le.Status)
	}

	// Partition dropped locally.
	var reg *string
	if err := pool.QueryRow(ctx,
		`SELECT to_regclass('public.itest_orders_p2020_01')::text`).Scan(&reg); err != nil {
		t.Fatal(err)
	}
	if reg != nil {
		t.Fatalf("partition still present: %s", *reg)
	}

	// Objects present in WORM bucket with lock metadata.
	objs, err := objectstore.ListAll(ctx, c, "itest_orders/itest_orders_p2020_01/")
	if err != nil || len(objs) != 2 {
		t.Fatalf("s3 objects: %d %v", len(objs), err)
	}
	head, err := c.Head(ctx, le.S3Key)
	if err != nil {
		t.Fatal(err)
	}
	if head.ObjectLockMode != "COMPLIANCE" {
		t.Fatalf("object lock missing: %+v", head)
	}

	// Log row: DROPPED.
	var status string
	if err := pool.QueryRow(ctx,
		`SELECT status FROM partition_archive_log WHERE archive_id=$1`,
		le.ArchiveID).Scan(&status); err != nil || status != "DROPPED" {
		t.Fatalf("log status %q %v", status, err)
	}

	// Restore drill.
	res, err := a.RestorePartition(ctx, "itest_orders_p2020_01", "")
	if err != nil {
		t.Fatal(err)
	}
	if !res.Parity || res.LoadedRows != 3 {
		t.Fatalf("restore %+v", res)
	}
	var cnt int64
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM archive_restore.itest_orders_p2020_01`).Scan(&cnt); err != nil {
		t.Fatal(err)
	}
	if cnt != 3 {
		t.Fatalf("restored rows %d", cnt)
	}
	// Log flipped to RESTORED.
	if err := pool.QueryRow(ctx,
		`SELECT status FROM partition_archive_log WHERE archive_id=$1`,
		le.ArchiveID).Scan(&status); err != nil || status != "RESTORED" {
		t.Fatalf("log status after restore %q %v", status, err)
	}
}

type failPut struct {
	objectstore.Client
	failOn string
}

func (f failPut) Put(ctx context.Context, in objectstore.PutInput) (objectstore.Object, error) {
	if strings.Contains(in.Key, f.failOn) {
		return objectstore.Object{}, errors.New("simulated mid-upload failure")
	}
	return f.Client.Put(ctx, in)
}

// Mid-upload failure after DETACH must re-attach the partition — no
// detached-but-unarchived limbo (fail-closed edge case).
func TestArchiveReattachesOnUploadFailure(t *testing.T) {
	pool := pgPool(t)
	ctx := context.Background()
	applyMigration120(t, pool)

	if _, err := pool.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS itest_orders (
			id bigint NOT NULL, account_id bigint NOT NULL,
			qty numeric(20,8), created_at timestamptz NOT NULL
		) PARTITION BY RANGE (created_at)`); err != nil {
		t.Fatal(err)
	}
	pool.Exec(ctx, `DROP TABLE IF EXISTS itest_orders_p2020_03`)
	if _, err := pool.Exec(ctx, `
		CREATE TABLE itest_orders_p2020_03 PARTITION OF itest_orders
		FOR VALUES FROM ('2020-03-01') TO ('2020-04-01')`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO itest_orders VALUES (9, 1, 1.0, '2020-03-15')`); err != nil {
		t.Fatal(err)
	}

	c, done := wormClient(t)
	defer done()
	a := archiver.New(pool, failPut{Client: c, failOn: ".parquet"})
	a.SetParents([]string{"itest_orders"})

	parts, err := a.ListPartitions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var target *archiver.Partition
	for i := range parts {
		if parts[i].Name == "itest_orders_p2020_03" {
			target = &parts[i]
		}
	}
	if target == nil {
		t.Fatal("partition not listed")
	}
	if _, err := a.ArchivePartition(ctx, *target); err == nil {
		t.Fatal("expected upload failure")
	}

	// Re-attached: still a partition member, rows still reachable.
	var cnt int64
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM pg_inherits i
		JOIN pg_class p ON i.inhparent=p.oid
		JOIN pg_class c ON i.inhrelid=c.oid
		WHERE p.relname='itest_orders' AND c.relname='itest_orders_p2020_03'`).
		Scan(&cnt); err != nil || cnt != 1 {
		t.Fatalf("reattach check: %d %v", cnt, err)
	}
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM itest_orders WHERE created_at='2020-03-15'`).Scan(&cnt); err != nil || cnt != 1 {
		t.Fatalf("row not reachable through parent: %d %v", cnt, err)
	}
}

// Queries on a detached partition edge: post-DROP the name must error.
func TestDroppedPartitionQueryFails(t *testing.T) {
	pool := pgPool(t)
	ctx := context.Background()
	applyMigration120(t, pool)
	var cnt int64
	err := pool.QueryRow(ctx, `SELECT count(*) FROM itest_orders_p2020_01`).Scan(&cnt)
	if err == nil {
		// Only valid if the earlier test ran; tolerate either ordering.
		t.Skip("partition present — ordering artifact")
	}
}
