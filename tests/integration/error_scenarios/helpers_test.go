// Package error_scenarios implements Phase-08 Task 8.3.5 — the
// multi-service fault-injection and error-tier suite (spec §2.7, §8.7,
// §24 #307).
//
// Environment gates (matching the repo's existing convention):
//   - EXC_PG_TEST=1        enables PostgreSQL-backed scenarios
//     (EXC_PG_DSN overrides; default is the scratch migration-verify
//     instance over the /tmp unix socket).
//   - EXC_REDIS_TEST=1     enables Redis-backed scenarios
//     (EXC_REDIS_TEST_ADDR, EXC_REDIS_TEST_PASSWORD, EXC_REDIS_TEST_DB;
//     default addr 127.0.0.1:16379, default DB index 14 — a dedicated
//     logical DB so the suite never touches sibling keyspaces on shared
//     infra; cleanup deletes only keys this run created).
//
// Everything else runs purely in-process (shm rings, HTTP handler chains,
// injected clocks) and needs no external services.
package error_scenarios

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"exchange/internal/errs"
	"exchange/internal/gateway"
	exchredis "exchange/internal/redis"
)

// DefaultPGDSN is the Task-8.3.5 scratch database (migverify over the
// /tmp unix socket). Override with EXC_PG_DSN.
const DefaultPGDSN = "postgres://exchange:exchange_dev@/migverify?host=/tmp&port=55433&sslmode=disable"

// testRedisDB is the dedicated logical DB index used when
// EXC_REDIS_TEST_DB is unset. The suite writes only errscen:* keys and
// deletes exactly the keys it created — FLUSHDB is never issued against
// shared infrastructure.
const testRedisDB = 14

// pgPool returns a pooled connection to the scratch OLTP instance, or
// skips when the env gate is unset / the server is unreachable.
func pgPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	if os.Getenv("EXC_PG_TEST") != "1" {
		t.Skip("set EXC_PG_TEST=1 to run PostgreSQL fault-injection tests")
	}
	dsn := os.Getenv("EXC_PG_DSN")
	if dsn == "" {
		dsn = DefaultPGDSN
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Skipf("postgres DSN unusable: %v", err)
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

// redisClient returns a coordination-style client bound to the test DB
// index, or skips when the env gate is unset / the server is unreachable.
func redisClient(t *testing.T) *exchredis.Client {
	t.Helper()
	if os.Getenv("EXC_REDIS_TEST") != "1" {
		t.Skip("set EXC_REDIS_TEST=1 to run Redis fault-injection tests")
	}
	addr := os.Getenv("EXC_REDIS_TEST_ADDR")
	if addr == "" {
		addr = "127.0.0.1:16379"
	}
	db := testRedisDB
	if raw := os.Getenv("EXC_REDIS_TEST_DB"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			t.Fatalf("EXC_REDIS_TEST_DB %q invalid", raw)
		}
		db = n
	}
	c := exchredis.New(addr, os.Getenv("EXC_REDIS_TEST_PASSWORD"), db)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.Ping(ctx); err != nil {
		_ = c.Close()
		t.Skipf("redis unreachable at %s db %d: %v", addr, db, err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// redisTestToken returns a collision-free session token under the
// suite's errscen: prefix and registers its deletion — scoped cleanup,
// never FLUSHDB, on shared infrastructure.
func redisTestToken(t *testing.T, c *exchredis.Client) string {
	t.Helper()
	tok := "errscen:" + strconv.FormatInt(time.Now().UnixNano(), 36) +
		":" + strconv.Itoa(os.Getpid())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = c.Del(ctx, "session:"+tok).Err()
	})
	return tok
}

// problem is the decoded spec §8.7 RFC 7807 envelope.
type problem struct {
	Type       string         `json:"type"`
	Error      string         `json:"error"`
	Message    string         `json:"message"`
	Status     int            `json:"status"`
	RequestID  string         `json:"request_id"`
	Timestamp  string         `json:"timestamp"`
	Details    map[string]any `json:"details"`
	RetryAfter int            `json:"retry_after"`
}

// decodeProblem parses a recorder body and enforces the §8.7 envelope
// invariants every error response must carry.
func decodeProblem(t *testing.T, rec *httptest.ResponseRecorder) problem {
	t.Helper()
	var p problem
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatalf("error body is not the §8.7 envelope: %v\nbody=%s",
			err, rec.Body.String())
	}
	if p.Type != "error" {
		t.Fatalf("envelope type=%q, want \"error\"", p.Type)
	}
	if p.Error == "" {
		t.Fatal("envelope missing machine-readable error code")
	}
	if p.Status == 0 {
		t.Fatal("envelope missing status")
	}
	if p.RequestID == "" {
		t.Fatal("envelope missing request_id correlation")
	}
	if p.Timestamp == "" {
		t.Fatal("envelope missing timestamp")
	}
	return p
}

// newTestRouter builds a gateway Router on a fresh error registry so the
// suite exercises the real emission gate without polluting
// errs.Default's emitted-set bookkeeping.
func newTestRouter() *gateway.Router {
	return gateway.NewRouter(nil, errs.New())
}
