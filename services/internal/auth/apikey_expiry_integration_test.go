// EXC_PG_TEST=1 gated integration tests — Task 13.3.8 API-key privilege
// auto-expiry sweep against a scratch-schema PostgreSQL.
//
//	PG: EXC_PG_DSN or postgres://exchange:exchange_dev@127.0.0.1:5433/exchange?sslmode=disable
//
// Run: EXC_PG_TEST=1 go test ./internal/auth -run KeyExpiry -v
package auth

import (
	"context"
	"fmt"
	"os"
	"sort"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const keyExpTestDSN = "postgres://exchange:exchange_dev@127.0.0.1:5433/exchange?sslmode=disable"

type keyExpFixture struct {
	pool  *pgxpool.Pool
	store *KeyStore
	sink  *keyExpSink
}

type keyExpSink struct{ events []keyExpEvent }
type keyExpEvent struct {
	userID int64
	event  string
	attrs  map[string]any
}

func (s *keyExpSink) NotifySecurityEvent(_ context.Context, userID int64,
	event string, attrs map[string]any) error {
	s.events = append(s.events, keyExpEvent{userID, event, attrs})
	return nil
}

// keyExpFixture applies migrations 002/003/025/208 into a throwaway
// schema — the columns the sweep touches plus the FK parents.
func keyExpFixtureSetup(t *testing.T) *keyExpFixture {
	t.Helper()
	if os.Getenv("EXC_PG_TEST") != "1" {
		t.Skip("set EXC_PG_TEST=1 to run Postgres integration tests")
	}
	dsn := os.Getenv("EXC_PG_DSN")
	if dsn == "" {
		dsn = keyExpTestDSN
	}
	schema := fmt.Sprintf("keyexp_itest_%d", time.Now().UnixNano())
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	boot, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Skipf("postgres unreachable (%v)", err)
	}
	if err := boot.Ping(ctx); err != nil {
		boot.Close(ctx)
		t.Skipf("postgres unreachable (%v)", err)
	}
	if _, err := boot.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		boot.Close(ctx)
		t.Fatalf("create schema: %v", err)
	}
	t.Cleanup(func() {
		c2, cancel2 := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel2()
		conn, err := pgx.Connect(c2, dsn)
		if err == nil {
			_, _ = conn.Exec(c2, "DROP SCHEMA IF EXISTS "+schema+" CASCADE")
			conn.Close(c2)
		}
	})
	boot.Close(ctx)

	for _, m := range []string{
		"002_create_users.up.sql",
		"003_create_accounts.up.sql",
		"025_create_api_keys.up.sql",
		"208_api_key_auto_expiry.up.sql",
	} {
		keyExpMig(t, ctx, dsn, schema, "../db/migrations/"+m)
	}

	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema + ",public"
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)

	// nil SecretBox — the sweep never mints/unwraps secrets.
	store, err := NewKeyStore(pool, nil)
	if err != nil {
		t.Fatalf("keystore: %v", err)
	}
	return &keyExpFixture{pool: pool, store: store, sink: &keyExpSink{}}
}

func keyExpMig(t *testing.T, ctx context.Context, dsn, schema, file string) {
	t.Helper()
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	cfg.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol
	cfg.RuntimeParams["search_path"] = schema + ",public"
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Skipf("postgres unreachable: %v", err)
	}
	defer conn.Close(ctx)
	sql, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("read %s: %v", file, err)
	}
	if _, err := conn.Exec(ctx, string(sql)); err != nil {
		t.Fatalf("apply %s: %v", file, err)
	}
}

// mkKey inserts a user+account+api_key with the given age/scopes/allowlist
// and returns the api_keys.id.
func (f *keyExpFixture) mkKey(t *testing.T, age time.Duration,
	scopes []string, allowlist []string) int64 {
	t.Helper()
	ctx := context.Background()
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	var uid, aid, kid int64
	if err := f.pool.QueryRow(ctx,
		`INSERT INTO users (email) VALUES ($1) RETURNING id`,
		"keyexp-"+suffix+"@x.test").Scan(&uid); err != nil {
		t.Fatalf("user: %v", err)
	}
	if err := f.pool.QueryRow(ctx,
		`INSERT INTO accounts (user_id, account_type) VALUES ($1,'SPOT') RETURNING id`,
		uid).Scan(&aid); err != nil {
		t.Fatalf("account: %v", err)
	}
	err := f.pool.QueryRow(ctx, `
		INSERT INTO api_keys (key_id, account_id, user_id, scopes,
		                      ip_allowlist, created_at)
		VALUES ($1,$2,$3,$4,$5, now() - $6::interval) RETURNING id`,
		"k"+suffix, aid, uid, scopes, allowlist,
		fmt.Sprintf("%f seconds", age.Seconds())).Scan(&kid)
	if err != nil {
		t.Fatalf("api key: %v", err)
	}
	return kid
}

// keyRow reads back the sweep-visible columns.
func (f *keyExpFixture) keyRow(t *testing.T, id int64) (scopes, revoked []string,
	revokedAt, notifiedAt, override *time.Time) {
	t.Helper()
	err := f.pool.QueryRow(context.Background(), `
		SELECT scopes, revoked_scopes, permissions_revoked_at,
		       expiry_notified_at, expiry_override_until
		  FROM api_keys WHERE id=$1`, id).
		Scan(&scopes, &revoked, &revokedAt, &notifiedAt, &override)
	if err != nil {
		t.Fatalf("keyRow: %v", err)
	}
	return
}

func eqScopes(a, b []string) bool {
	as, bs := append([]string{}, a...), append([]string{}, b...)
	sort.Strings(as)
	sort.Strings(bs)
	if len(as) != len(bs) {
		return false
	}
	for i := range as {
		if as[i] != bs[i] {
			return false
		}
	}
	return true
}

func TestKeyExpirySweepRevokesStaleUnallowlistedKey(t *testing.T) {
	f := keyExpFixtureSetup(t)
	ctx := context.Background()
	old := f.mkKey(t, 91*24*time.Hour, []string{"read", "trade", "transfer"}, nil)
	fresh := f.mkKey(t, 30*24*time.Hour, []string{"read", "trade"}, nil)
	allowlisted := f.mkKey(t, 91*24*time.Hour, []string{"trade"}, []string{"10.0.0.1"})
	readOnly := f.mkKey(t, 200*24*time.Hour, []string{"read"}, nil)

	pol, err := NewKeyExpiryPolicy(f.store, f.sink)
	if err != nil {
		t.Fatalf("policy: %v", err)
	}
	res, err := pol.Sweep(ctx)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if res.Revoked != 1 || res.Restored != 0 {
		t.Fatalf("result %+v", res)
	}

	scopes, revoked, revokedAt, _, _ := f.keyRow(t, old)
	if revokedAt == nil || !eqScopes(scopes, []string{"read"}) ||
		!eqScopes(revoked, []string{"trade", "transfer"}) {
		t.Fatalf("old key: scopes=%v revoked=%v at=%v", scopes, revoked, revokedAt)
	}
	// Untouched rows.
	for id, name := range map[int64]string{fresh: "fresh", allowlisted: "allowlisted", readOnly: "readOnly"} {
		s, r, at, _, _ := f.keyRow(t, id)
		if at != nil || len(r) != 0 {
			t.Fatalf("%s key %d touched: revoked_at=%v", name, id, at)
		}
		_ = s
	}
	// Idempotent: second sweep revokes nothing.
	res2, err := pol.Sweep(ctx)
	if err != nil {
		t.Fatalf("Sweep2: %v", err)
	}
	if res2.Revoked != 0 {
		t.Fatalf("second sweep revoked %d keys", res2.Revoked)
	}
}

func TestKeyExpirySweepWarnsSevenDaysOut(t *testing.T) {
	f := keyExpFixtureSetup(t)
	ctx := context.Background()
	warnKey := f.mkKey(t, 85*24*time.Hour, []string{"trade"}, nil)
	pastDue := f.mkKey(t, 95*24*time.Hour, []string{"trade"}, nil)

	pol, err := NewKeyExpiryPolicy(f.store, f.sink)
	if err != nil {
		t.Fatalf("policy: %v", err)
	}
	res, err := pol.Sweep(ctx)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if res.Warned != 1 || res.Revoked != 1 {
		t.Fatalf("result %+v — 85d key warned, 95d key revoked", res)
	}
	_, _, _, notifiedAt, _ := f.keyRow(t, warnKey)
	if notifiedAt == nil {
		t.Fatal("warning not stamped")
	}
	if len(f.sink.events) != 2 {
		t.Fatalf("events=%d want 2 (warning + revocation notice)", len(f.sink.events))
	}
	var sawWarn, sawRevoke bool
	for _, ev := range f.sink.events {
		if ev.event == "api_key_expiry_warning" {
			sawWarn = true
		}
		if ev.event == "api_key_permissions_revoked" {
			sawRevoke = true
		}
	}
	if !sawWarn || !sawRevoke {
		t.Fatalf("events %+v", f.sink.events)
	}

	// Once-only: second sweep emits no further warning.
	f.sink.events = nil
	res2, err := pol.Sweep(ctx)
	if err != nil {
		t.Fatalf("Sweep2: %v", err)
	}
	if res2.Warned != 0 || len(f.sink.events) != 0 {
		t.Fatalf("re-warned: %+v events=%v", res2, f.sink.events)
	}
	_ = pastDue
}

func TestKeyExpirySweepRestoresOnAllowlist(t *testing.T) {
	f := keyExpFixtureSetup(t)
	ctx := context.Background()
	id := f.mkKey(t, 95*24*time.Hour, []string{"read", "trade"}, nil)

	pol, err := NewKeyExpiryPolicy(f.store, f.sink)
	if err != nil {
		t.Fatalf("policy: %v", err)
	}
	if _, err := pol.Sweep(ctx); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	scopes, revoked, at, _, _ := f.keyRow(t, id)
	if !eqScopes(scopes, []string{"read"}) || at == nil || len(revoked) != 1 {
		t.Fatalf("pre-restore state: scopes=%v revoked=%v at=%v", scopes, revoked, at)
	}

	// Holder configures an allowlist → next sweep restores verbatim.
	if _, err := f.pool.Exec(ctx,
		`UPDATE api_keys SET ip_allowlist='{203.0.113.7}' WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	res, err := pol.Sweep(ctx)
	if err != nil {
		t.Fatalf("Sweep restore: %v", err)
	}
	if res.Restored != 1 {
		t.Fatalf("restored=%d", res.Restored)
	}
	scopes, revoked, at, notifiedAt, _ := f.keyRow(t, id)
	if !eqScopes(scopes, []string{"read", "trade"}) || at != nil ||
		len(revoked) != 0 || notifiedAt != nil {
		t.Fatalf("post-restore: scopes=%v revoked=%v at=%v notified=%v",
			scopes, revoked, at, notifiedAt)
	}
	// Restore notice emitted.
	var saw bool
	for _, ev := range f.sink.events {
		if ev.event == "api_key_permissions_restored" {
			saw = true
		}
	}
	if !saw {
		t.Fatalf("no restore notification in %+v", f.sink.events)
	}
}

func TestKeyExpiryOverrideBlocksRevoke(t *testing.T) {
	f := keyExpFixtureSetup(t)
	ctx := context.Background()
	id := f.mkKey(t, 95*24*time.Hour, []string{"trade"}, nil)

	// Dual-control grant lands → sweep must leave the key alone.
	tx, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	until := time.Now().Add(30 * 24 * time.Hour).UTC()
	if err := f.store.ExtendExpiryOverrideTx(ctx, tx, id, until); err != nil {
		t.Fatalf("override tx: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	pol, err := NewKeyExpiryPolicy(f.store, f.sink)
	if err != nil {
		t.Fatalf("policy: %v", err)
	}
	res, err := pol.Sweep(ctx)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if res.Revoked != 0 || res.Warned != 0 {
		t.Fatalf("override failed: %+v", res)
	}
	scopes, _, _, _, ov := f.keyRow(t, id)
	if ov == nil || !eqScopes(scopes, []string{"trade"}) {
		t.Fatalf("overridden key state: scopes=%v override=%v", scopes, ov)
	}

	// Bounds: past and >180d extensions refuse.
	tx2, _ := f.pool.Begin(ctx)
	if err := f.store.ExtendExpiryOverrideTx(ctx, tx2, id, time.Now().Add(-time.Hour)); err == nil {
		t.Fatal("past extension accepted")
	}
	if err := f.store.ExtendExpiryOverrideTx(ctx, tx2, id,
		time.Now().Add(400*24*time.Hour)); err == nil {
		t.Fatal(">180d extension accepted")
	}
	tx2.Rollback(ctx)
}
