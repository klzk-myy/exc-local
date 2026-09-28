// Task 9.3.7 — flag admin handlers end-to-end over real PG + Redis.
//
// Gated: EXC_PG_TEST=1 + EXC_REDIS_TEST=1 (same harness as
// internal/flags/store_integration_test.go — scratch migverify on the
// /tmp socket, Redis DB 15 flushed per test). Applies migration 193's
// DDL verbatim so the handler test runs the shipped schema, not a copy.
package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	goredis "github.com/redis/go-redis/v9"

	"exchange/internal/auth"
	"exchange/internal/flags"
)

func flagTestPool(t *testing.T) *pgxpool.Pool {
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
	// Serialize against internal/flags' store test — both share the
	// scratch migverify DB and recreate feature_flags; the advisory lock
	// (same key) is held for the test's duration.
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
	ddl, err := os.ReadFile("../db/migrations/193_feature_flags.up.sql")
	if err != nil {
		t.Fatalf("read migration 193: %v", err)
	}
	if _, err := pool.Exec(context.Background(),
		`DROP TABLE IF EXISTS feature_flags`); err != nil {
		t.Fatalf("drop: %v", err)
	}
	if _, err := pool.Exec(context.Background(), string(ddl)); err != nil {
		t.Fatalf("apply 193: %v", err)
	}
	return pool
}

func flagTestRedis(t *testing.T) goredis.Cmdable {
	t.Helper()
	if os.Getenv("EXC_REDIS_TEST") != "1" {
		t.Skip("EXC_REDIS_TEST not set")
	}
	addr := os.Getenv("EXC_REDIS_TEST_ADDR")
	if addr == "" {
		addr = "127.0.0.1:16379"
	}
	// DB 14 — dedicated test index, distinct from the flags package's
	// DB 15 so concurrent package tests never flush each other's keys.
	rdb := goredis.NewClient(&goredis.Options{Addr: addr, DB: 14})
	if err := rdb.FlushDB(context.Background()).Err(); err != nil {
		t.Fatalf("redis: %v", err)
	}
	t.Cleanup(func() { _ = rdb.Close() })
	return rdb
}

func flagReq(t *testing.T, method, path, name, body string) *http.Request {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if name != "" {
		req.SetPathValue("name", name)
	}
	return req.WithContext(auth.WithClaims(req.Context(),
		auth.Claims{Subject: "9001", AccountID: 1, Scopes: []string{"admin"}}))
}

func TestFlagHandlersIntegration(t *testing.T) {
	pool := flagTestPool(t)
	rdb := flagTestRedis(t)
	st, err := flags.NewStore(pool, rdb)
	if err != nil {
		t.Fatal(err)
	}
	list, create, get, update, toggle, del, advance := FlagHandlers(st)

	call := func(h http.HandlerFunc, req *http.Request) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h(rec, req)
		return rec
	}

	// Create.
	rec := call(create, flagReq(t, "POST", "/api/v1/admin/flags", "",
		`{"name":"canary","enabled":true,"rollout_pct":0,"description":"d"}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	var created flags.Flag
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("create body: %v", err)
	}
	if created.Version != 1 || created.StageIdx != -1 {
		t.Fatalf("created: %+v", created)
	}

	// Get + list.
	if rec := call(get, flagReq(t, "GET", "/api/v1/admin/flags/canary", "canary", "")); rec.Code != http.StatusOK {
		t.Fatalf("get: %d %s", rec.Code, rec.Body)
	}
	rec = call(list, flagReq(t, "GET", "/api/v1/admin/flags", "", ""))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "canary") {
		t.Fatalf("list: %d %s", rec.Code, rec.Body)
	}

	// Update (PUT) — bump rollout manually.
	rec = call(update, flagReq(t, "PUT", "/api/v1/admin/flags/canary", "canary",
		`{"rollout_pct":50}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("update: %d %s", rec.Code, rec.Body)
	}
	var updated flags.Flag
	_ = json.Unmarshal(rec.Body.Bytes(), &updated)
	if updated.RolloutPct != 50 || updated.Version != 2 {
		t.Fatalf("updated: %+v", updated)
	}

	// Toggle off.
	rec = call(toggle, flagReq(t, "POST", "/api/v1/admin/flags/canary", "canary",
		`{"enabled":false}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("toggle: %d %s", rec.Code, rec.Body)
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &updated)
	if updated.Enabled {
		t.Fatal("toggle did not disable")
	}

	// Advance: -1 → 0 arms stage 0 (1%).
	rec = call(advance, flagReq(t, "POST", "/api/v1/admin/flags/canary/advance", "canary", ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("advance: %d %s", rec.Code, rec.Body)
	}
	var adv struct {
		Flag       flags.Flag `json:"flag"`
		LadderDone bool       `json:"ladder_done"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &adv)
	if adv.Flag.StageIdx != 0 || adv.LadderDone {
		t.Fatalf("advance: %+v", adv)
	}

	// Delete → subsequent get is 404.
	rec = call(del, flagReq(t, "DELETE", "/api/v1/admin/flags/canary", "canary", ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body)
	}
	rec = call(get, flagReq(t, "GET", "/api/v1/admin/flags/canary", "canary", ""))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("get after delete: %d want 404", rec.Code)
	}

	// Every mutation landed in admin_audit_log (feature_flag.* actions).
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM admin_audit_log WHERE action LIKE 'feature_flag.%'`).
		Scan(&n); err != nil || n < 4 {
		t.Fatalf("audit rows=%d err=%v (want ≥4)", n, err)
	}
}
