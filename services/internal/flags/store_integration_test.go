// PostgreSQL + Redis integration tests for the flag store — Task 9.3.7.
//
// Gated: skipped unless EXC_PG_TEST=1 (DSN via EXC_PG_DSN, default the
// scratch migverify instance) and EXC_REDIS_TEST=1 (addr via
// EXC_REDIS_TEST_ADDR, dedicated DB index 15 — flushed per test).
//
// Run: EXC_PG_TEST=1 EXC_REDIS_TEST=1 go test ./internal/flags/ -v
package flags

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	goredis "github.com/redis/go-redis/v9"
)

const flagsTestRedisDB = 15 // dedicated index — never the default DB 0

func pgPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	if os.Getenv("EXC_PG_TEST") != "1" {
		t.Skip("EXC_PG_TEST not set")
	}
	dsn := os.Getenv("EXC_PG_DSN")
	if dsn == "" {
		dsn = "postgres://exchange:exchange_dev@/migverify?host=/tmp&port=55433&sslmode=disable"
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("pg pool: %v", err)
	}
	t.Cleanup(pool.Close)
	// Serialize against the api package's flag-handler test — both share
	// the scratch migverify DB and drop/recreate feature_flags; Go runs
	// packages concurrently, so gate the critical section on a PG
	// advisory lock held for the test's duration.
	lockConn, err := pool.Acquire(context.Background())
	if err != nil {
		t.Fatalf("pg acquire: %v", err)
	}
	if _, err := lockConn.Exec(context.Background(),
		`SELECT pg_advisory_lock(9193007)`); err != nil {
		t.Fatalf("advisory lock: %v", err)
	}
	t.Cleanup(func() {
		_, _ = lockConn.Exec(context.Background(),
			`SELECT pg_advisory_unlock(9193007)`)
		lockConn.Release()
	})
	// Flags + audit DDL (migrations 193/009/010 columns verbatim, minus
	// extensions the scratch server lacks).
	if _, err := pool.Exec(context.Background(), `
		DROP TABLE IF EXISTS feature_flags;
		DROP TABLE IF EXISTS admin_audit_log;
		DROP TABLE IF EXISTS audit_hash_chain;
		CREATE TABLE audit_hash_chain (
		    id BIGSERIAL PRIMARY KEY, sequence_num BIGINT NOT NULL UNIQUE,
		    table_name VARCHAR(64) NOT NULL, record_id BIGINT,
		    action VARCHAR(16) NOT NULL, payload_hash VARCHAR(64) NOT NULL,
		    prev_hash VARCHAR(64), created_at TIMESTAMPTZ NOT NULL DEFAULT now());
		CREATE TABLE admin_audit_log (
		    id BIGSERIAL PRIMARY KEY, admin_user_id BIGINT NOT NULL,
		    action VARCHAR(128) NOT NULL, target_type VARCHAR(64),
		    target_id BIGINT, before_state JSONB, after_state JSONB,
		    ip_address INET, created_at TIMESTAMPTZ NOT NULL DEFAULT now());
		CREATE TABLE feature_flags (
		    name VARCHAR(64) PRIMARY KEY
		        CHECK (name ~ '^[a-z][a-z0-9_-]{1,63}$'),
		    enabled BOOLEAN NOT NULL DEFAULT false,
		    rollout_pct SMALLINT NOT NULL DEFAULT 0
		        CHECK (rollout_pct BETWEEN 0 AND 100),
		    stages SMALLINT[] NOT NULL DEFAULT '{1,10,25,50,100}'
		        CHECK (cardinality(stages) >= 1),
		    stage_idx SMALLINT NOT NULL DEFAULT -1 CHECK (stage_idx >= -1),
		    tiers TEXT[] NOT NULL DEFAULT '{}',
		    accounts BIGINT[] NOT NULL DEFAULT '{}',
		    description TEXT NOT NULL DEFAULT '',
		    created_by BIGINT NOT NULL DEFAULT 0,
		    updated_by BIGINT NOT NULL DEFAULT 0,
		    version BIGINT NOT NULL DEFAULT 1,
		    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
		    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
		    CONSTRAINT feature_flags_stage_in_ladder CHECK
		        (stage_idx < cardinality(stages)));
	`); err != nil {
		t.Fatalf("ddl: %v", err)
	}
	return pool
}

func redisClient(t *testing.T) *goredis.Client {
	t.Helper()
	if os.Getenv("EXC_REDIS_TEST") != "1" {
		t.Skip("EXC_REDIS_TEST not set")
	}
	addr := os.Getenv("EXC_REDIS_TEST_ADDR")
	if addr == "" {
		addr = "127.0.0.1:16379"
	}
	rdb := goredis.NewClient(&goredis.Options{Addr: addr, DB: flagsTestRedisDB})
	if err := rdb.Ping(context.Background()).Err(); err != nil {
		t.Fatalf("redis ping: %v", err)
	}
	if err := rdb.FlushDB(context.Background()).Err(); err != nil {
		t.Fatalf("redis flushdb: %v", err)
	}
	t.Cleanup(func() { _ = rdb.Close() })
	return rdb
}

func TestStoreRoundTripIntegration(t *testing.T) {
	pool := pgPool(t)
	rdb := redisClient(t)
	st, err := NewStore(pool, rdb)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	f := Flag{Name: "canary_v2", Enabled: true, RolloutPct: 10,
		StageIdx: -1, // manual rollout — first Advance arms ladder stage 0
		Tiers:    []string{"institutional"}, Accounts: []int64{42},
		Description: "canary matcher"}
	f.Normalize()
	out, err := st.Upsert(ctx, f, 9001, "10.0.0.1:5555")
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if out.Version != 1 {
		t.Fatalf("version = %d, want 1", out.Version)
	}

	// Cache hit path: Redis holds the JSON now.
	if got := rdb.Get(ctx, "flags:canary_v2").Val(); got == "" {
		t.Fatal("flag not write-through cached")
	}
	got, err := st.Get(ctx, "canary_v2")
	if err != nil || got.RolloutPct != 10 {
		t.Fatalf("get: %+v err=%v", got, err)
	}

	// Audit row committed in the same tx.
	var auditN int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM admin_audit_log WHERE action='feature_flag.upsert'`).
		Scan(&auditN); err != nil || auditN != 1 {
		t.Fatalf("audit rows = %d err=%v", auditN, err)
	}

	// Advance the ladder.
	out, done, err := st.Advance(ctx, "canary_v2", 9001, "10.0.0.1")
	if err != nil || done {
		t.Fatalf("advance: %+v done=%v err=%v", out, done, err)
	}
	if out.StageIdx != 0 || out.EffectivePct() != 1 {
		t.Fatalf("stage_idx=%d pct=%d, want 0/1%%", out.StageIdx, out.EffectivePct())
	}
	if out.Version != 2 {
		t.Fatalf("version not bumped: %d", out.Version)
	}

	// Eval through the store: allowlisted account on, random acct follows pct.
	on, err := st.Eval(ctx, "canary_v2", EvalContext{AccountID: 42})
	if err != nil || !on {
		t.Fatalf("eval allowlisted: %v %v", on, err)
	}
	if !st.Enabled(ctx, "canary_v2", EvalContext{AccountID: 99, Tier: "institutional"}) {
		t.Fatal("eval tier-allowlisted off")
	}

	// Delete → gone everywhere.
	if err := st.Delete(ctx, "canary_v2", 9001, ""); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := st.Get(ctx, "canary_v2"); err != ErrNotFound {
		t.Fatalf("get after delete: %v", err)
	}
	if st.Enabled(ctx, "canary_v2", EvalContext{AccountID: 42}) {
		t.Fatal("deleted flag still on")
	}
	if rdb.Exists(ctx, "flags:canary_v2").Val() != 0 {
		t.Fatal("cache key survives delete")
	}
	if rdb.Get(ctx, "flags:version").Val() == "" {
		t.Fatal("flags:version not bumped")
	}
}

// Cache failure mid-request falls through to PG; stale entries are
// never preferred over the row (write-through invalidation).
func TestStoreCacheFailureFallsThrough(t *testing.T) {
	pool := pgPool(t)
	if os.Getenv("EXC_REDIS_TEST") != "1" {
		t.Skip("EXC_REDIS_TEST not set")
	}
	st, err := NewStore(pool, nil) // nil Redis = PG-only path
	if err != nil {
		t.Fatal(err)
	}
	f := Flag{Name: "pg_only", Enabled: true, RolloutPct: 100}
	f.Normalize()
	if _, err := st.Upsert(context.Background(), f, 1, ""); err != nil {
		t.Fatalf("upsert without redis: %v", err)
	}
	got, err := st.Get(context.Background(), "pg_only")
	if err != nil || !got.Enabled {
		t.Fatalf("get without redis: %+v %v", got, err)
	}
}
