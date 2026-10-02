// account_state_test.go — AccountStateProjector coverage (Phase-3 Task
// 3.3.1). The projector is SQL+Redis plumbing — unit coverage lives in
// the C++ peer (core/tests/test_account_state.cpp parses every field
// shape this publisher emits); these tests are the end-to-end contract
// gate, env-gated like the package's other store tests:
//
//	EXC_PG_TEST=1 EXC_PG_DSN=... EXC_REDIS_TEST=1 EXC_REDIS_DSN=... \
//	    go test ./internal/risk/ -run TestAccountState -v
package risk

import (
	"context"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	goredis "github.com/redis/go-redis/v9"
)

func acctStateFixture(t *testing.T) (*AccountStateProjector, *pgxpool.Pool, *goredis.Client, context.Context) {
	t.Helper()
	if os.Getenv("EXC_PG_TEST") != "1" {
		t.Skip("EXC_PG_TEST not set")
	}
	if os.Getenv("EXC_REDIS_TEST") != "1" {
		t.Skip("EXC_REDIS_TEST not set")
	}
	dsn := os.Getenv("EXC_PG_DSN")
	if dsn == "" {
		dsn = "postgres://exchange:exchange_dev@127.0.0.1:5433/exchange?sslmode=disable"
	}
	addr := os.Getenv("EXC_REDIS_DSN")
	if addr == "" {
		addr = "127.0.0.1:16379"
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Skipf("postgres unreachable (%v)", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Skipf("postgres unreachable (%v)", err)
	}
	rdb := goredis.NewClient(&goredis.Options{Addr: addr})
	if err := rdb.Ping(ctx).Err(); err != nil {
		pool.Close()
		rdb.Close()
		t.Skipf("redis unreachable (%v)", err)
	}
	p, err := NewAccountStateProjector(pool, rdb, nil)
	if err != nil {
		t.Fatalf("projector: %v", err)
	}
	t.Cleanup(func() {
		rdb.Del(ctx, AccountStateKey)
		pool.Close()
		rdb.Close()
	})
	return p, pool, rdb, ctx
}

// TestAccountStatePublishShape — one fixture row per field shape, then
// verify the hash carries exactly the grammar AccountStateCache parses:
// "7" -> "ACTIVE,T2,PROFESSIONAL,CANCEL_BOTH,1", "a:7:USD" -> scaled
// units, "p:7:1" -> signed net, "i:{id}" -> "EUR/USD", "__hb__" fresh.
func TestAccountStatePublishShape(t *testing.T) {
	p, pool, rdb, ctx := acctStateFixture(t)

	// Scratch user + account — the accounts row needs a users FK.
	var uid int64
	if err := pool.QueryRow(ctx, `
		INSERT INTO users (email, password_hash)
		VALUES ('acct_state_itest@example.com', 'x')
		RETURNING id`).Scan(&uid); err != nil {
		t.Fatalf("users fixture: %v", err)
	}
	var aid int64
	if err := pool.QueryRow(ctx, `
		INSERT INTO accounts (user_id, account_type, kyc_tier, status,
		                      client_category, default_stp_mode)
		VALUES ($1, 'SPOT', 'T2', 'ACTIVE', 'PROFESSIONAL', 'CANCEL_BOTH')
		RETURNING id`, uid).Scan(&aid); err != nil {
		t.Fatalf("accounts fixture: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO balances (account_id, currency, available)
		VALUES ($1, 'USD', 1234.5678), ($1, 'EUR', 10)`, aid); err != nil {
		t.Fatalf("balances fixture: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO positions (account_id, instrument_id, side, quantity,
		                       entry_price)
		VALUES ($1, 1, 'LONG', 3.0, 1.1), ($1, 1, 'SHORT', 1.0, 1.2)`,
		aid); err != nil {
		t.Fatalf("positions fixture: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DELETE FROM positions WHERE account_id=$1", aid)
		_, _ = pool.Exec(ctx, "DELETE FROM balances WHERE account_id=$1", aid)
		_, _ = pool.Exec(ctx, "DELETE FROM accounts WHERE id=$1", aid)
		_, _ = pool.Exec(ctx, "DELETE FROM users WHERE id=$1", uid)
	})

	if err := p.Publish(ctx); err != nil {
		t.Fatalf("publish: %v", err)
	}
	h, err := rdb.HGetAll(ctx, AccountStateKey).Result()
	if err != nil {
		t.Fatalf("hgetall: %v", err)
	}

	want := "ACTIVE,T2,PROFESSIONAL,CANCEL_BOTH,1"
	if h[strconv.FormatInt(aid, 10)] != want {
		t.Fatalf("account field: got %q want %q", h[strconv.FormatInt(aid, 10)], want)
	}
	// 1234.5678 USD -> 123456780000 at 1e8 scale.
	if h["a:"+strconv.FormatInt(aid, 10)+":USD"] != "123456780000" {
		t.Fatalf("balance field: got %q", h["a:"+strconv.FormatInt(aid, 10)+":USD"])
	}
	// net = 3.0 LONG - 1.0 SHORT = +2.0 units -> 200000000.
	if h["p:"+strconv.FormatInt(aid, 10)+":1"] != "200000000" {
		t.Fatalf("position field: got %q", h["p:"+strconv.FormatInt(aid, 10)+":1"])
	}
	// Instruments projected — EUR/USD is seed id 1.
	if h["i:1"] != "EUR/USD" {
		t.Fatalf("instrument field: got %q", h["i:1"])
	}
	hb, _ := strconv.ParseInt(h["__hb__"], 10, 64)
	if hb <= 0 || time.Now().Unix()-hb > 30 {
		t.Fatalf("heartbeat not fresh: %q", h["__hb__"])
	}
}

// TestAccountStatePublishSwap — a second publish must atomically replace
// the hash (RENAME): staging leaves no :next key behind and stale fields
// from a deleted account disappear.
func TestAccountStatePublishSwap(t *testing.T) {
	p, _, rdb, ctx := acctStateFixture(t)
	if err := p.Publish(ctx); err != nil {
		t.Fatalf("publish 1: %v", err)
	}
	// Poison a stale field into the live hash — publish 2 must drop it.
	if err := rdb.HSet(ctx, AccountStateKey, "999999999",
		"ACTIVE,T0,RETAIL,-,0").Err(); err != nil {
		t.Fatalf("poison: %v", err)
	}
	if err := p.Publish(ctx); err != nil {
		t.Fatalf("publish 2: %v", err)
	}
	if exists, _ := rdb.Exists(ctx, accountStateNextKey).Result(); exists != 0 {
		t.Fatal("staging key :next leaked past RENAME")
	}
	if v, _ := rdb.HGet(ctx, AccountStateKey, "999999999").Result(); v != "" {
		t.Fatal("stale field survived the atomic swap")
	}
}

func TestAccountStateProjectorNilDeps(t *testing.T) {
	if _, err := NewAccountStateProjector(nil, nil, nil); err == nil {
		t.Fatal("nil pool accepted")
	}
}
