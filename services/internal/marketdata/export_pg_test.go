// Gated: skipped unless EXC_PG_TEST=1 (same convention as
// internal/accounts/integration_test.go). Target defaults to the dev
// database; override with EXC_PG_DSN.
//
// Run: EXC_PG_TEST=1 go test ./internal/marketdata/ -run IntegrationExportJob -v
package marketdata

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func exportTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	if os.Getenv("EXC_PG_TEST") != "1" {
		t.Skip("set EXC_PG_TEST=1 to run PostgreSQL integration tests")
	}
	dsn := os.Getenv("EXC_PG_DSN")
	if dsn == "" {
		dsn = "postgres://exchange:exchange_dev@localhost:5433/exchange?sslmode=disable"
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Skipf("postgres unreachable: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Skipf("postgres unreachable: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// IntegrationExportJobLifecycle drives the real export_jobs table
// (migration 256) through the full PENDING→RUNNING→COMPLETED lattice.
func TestIntegrationExportJobLifecycle(t *testing.T) {
	pool := exportTestPool(t)
	store := NewPgJobStore(pool)
	ctx := context.Background()
	acct := time.Now().UnixNano()/1000 + 7_000_000_000_000 // throwaway owner

	id, err := store.Insert(ctx, Job{
		AccountID: acct, Kind: "trades", Symbol: "EURUSD",
		Format: "csv", RowLimit: ExportMaxRows,
		CreatedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			"DELETE FROM export_jobs WHERE account_id = $1", acct)
	})

	got, err := store.Get(ctx, id)
	if err != nil || got == nil {
		t.Fatalf("get: %v %v", got, err)
	}
	if got.Status != JobPending || got.RowLimit != ExportMaxRows {
		t.Fatalf("job %+v", got)
	}

	claimed, err := store.ClaimPending(ctx, 10)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	var mine *Job
	for i := range claimed {
		if claimed[i].ID == id {
			mine = &claimed[i]
		}
	}
	if mine == nil {
		t.Fatal("claim did not return the seeded job")
	}
	if mine.Status != JobRunning || mine.StartedAt == nil {
		t.Fatalf("claimed %+v", mine)
	}
	// A claimed row is invisible to a second claim pass.
	again, err := store.ClaimPending(ctx, 10)
	if err != nil {
		t.Fatalf("re-claim: %v", err)
	}
	for _, j := range again {
		if j.ID == id {
			t.Fatal("RUNNING row re-claimed — SKIP LOCKED contract broken")
		}
	}

	exp := time.Now().UTC().Add(ExportLinkTTL)
	if err := store.Complete(ctx, id, Completion{
		ObjectRef: "exports/7/1.csv", RowCount: 12,
		SHA256: "aaaa", Truncated: false, ExpiresAt: exp,
		At: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("complete: %v", err)
	}
	got, _ = store.Get(ctx, id)
	if got.Status != JobCompleted || got.RowCount != 12 ||
		got.ObjectRef != "exports/7/1.csv" || got.ExpiresAt == nil {
		t.Fatalf("completed %+v", got)
	}

	// Unnotified scan finds it; MarkNotified removes it.
	un, err := store.Unnotified(ctx, 100)
	if err != nil {
		t.Fatalf("unnotified: %v", err)
	}
	found := false
	for _, j := range un {
		if j.ID == id {
			found = true
		}
	}
	if !found {
		t.Fatal("completed job missing from unnotified scan")
	}
	if err := store.MarkNotified(ctx, id, time.Now().UTC()); err != nil {
		t.Fatalf("mark notified: %v", err)
	}
	un, _ = store.Unnotified(ctx, 100)
	for _, j := range un {
		if j.ID == id {
			t.Fatal("notified job still in unnotified scan")
		}
	}

	// Owner-scoped keyset list round-trips through real SQL.
	page, err := store.ListForAccount(ctx, acct, time.Time{}, 0, 10)
	if err != nil || len(page) != 1 || page[0].ID != id {
		t.Fatalf("list: %v %+v", err, page)
	}
}

// IntegrationExportJobFail drives the RUNNING→FAILED terminal path.
func TestIntegrationExportJobFail(t *testing.T) {
	pool := exportTestPool(t)
	store := NewPgJobStore(pool)
	ctx := context.Background()
	acct := time.Now().UnixNano()/1000 + 8_000_000_000_000

	id, err := store.Insert(ctx, Job{
		AccountID: acct, Kind: "ticks", Symbol: "USDJPY",
		Format: "parquet", RowLimit: 1000,
	})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			"DELETE FROM export_jobs WHERE account_id = $1", acct)
	})
	claimed, err := store.ClaimPending(ctx, 10)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	seen := false
	for _, j := range claimed {
		if j.ID == id {
			seen = true
		}
	}
	if !seen {
		t.Fatal("job not claimed")
	}
	if err := store.Fail(ctx, id, "render: ch timeout", time.Now().UTC()); err != nil {
		t.Fatalf("fail: %v", err)
	}
	got, _ := store.Get(ctx, id)
	if got.Status != JobFailed || got.Error != "render: ch timeout" {
		t.Fatalf("failed %+v", got)
	}
}
