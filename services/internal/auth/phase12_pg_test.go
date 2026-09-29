// PostgreSQL-gated tests for Phase-12 persistence (Tasks 12.3.7/8/9):
// migrations 068/069/070 up + down + re-up inside a throwaway schema,
// webauthn_credentials CRUD + counter guard, login_history write/read/
// pagination, and users.anti_phishing_code set/get/constraint.
//
// Gated: skipped unless EXC_PG_TEST=1. DSN via EXC_PG_DSN (dev default).
package auth

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// itestPhase12 applies the Phase-12 migration set (and prerequisites)
// into a scratch schema; returns the pool + fixture user id.
func itestPhase12(t *testing.T) (*pgxpool.Pool, int64) {
	t.Helper()
	if os.Getenv("EXC_PG_TEST") != "1" {
		t.Skip("set EXC_PG_TEST=1 to run Postgres integration tests")
	}
	dsn := testDSN()
	schema := fmt.Sprintf("p12_itest_%d", time.Now().UnixNano())
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("dsn: %v", err)
	}
	cfg.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol
	cfg.RuntimeParams["search_path"] = schema + ",public"
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Skipf("postgres unreachable: %v", err)
	}
	if _, err := conn.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		conn.Close(ctx)
		t.Fatalf("create schema: %v", err)
	}
	apply := func(names []string, dir string) {
		t.Helper()
		for _, f := range names {
			sql, err := os.ReadFile("../db/migrations/" + f + "." + dir + ".sql")
			if err != nil {
				conn.Close(ctx)
				t.Fatalf("read %s.%s: %v", f, dir, err)
			}
			if _, err := conn.Exec(ctx, string(sql)); err != nil {
				conn.Close(ctx)
				t.Fatalf("apply %s.%s: %v", f, dir, err)
			}
		}
	}
	// Up: prerequisites then Phase-12 migrations.
	apply([]string{
		"002_create_users", "003_create_accounts",
		"068_webauthn_credentials", "069_login_history",
		"070_users_anti_phishing_code",
	}, "up")
	// Down + re-up proof for this task's migrations (reverse order).
	apply([]string{
		"070_users_anti_phishing_code", "069_login_history",
		"068_webauthn_credentials",
	}, "down")
	apply([]string{
		"068_webauthn_credentials", "069_login_history",
		"070_users_anti_phishing_code",
	}, "up")
	conn.Close(ctx)
	t.Cleanup(func() {
		c2, cancel2 := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel2()
		bare, err := pgx.ParseConfig(dsn)
		if err != nil {
			return
		}
		c, err := pgx.ConnectConfig(c2, bare)
		if err == nil {
			_, _ = c.Exec(c2, "DROP SCHEMA IF EXISTS "+schema+" CASCADE")
			c.Close(c2)
		}
	})

	pcfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("pool dsn: %v", err)
	}
	pcfg.ConnConfig.RuntimeParams["search_path"] = schema + ",public"
	pool, err := pgxpool.NewWithConfig(ctx, pcfg)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)

	var userID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO users (email) VALUES ($1) RETURNING id`,
		"p12-"+schema+"@example.com").Scan(&userID); err != nil {
		t.Fatalf("fixture user: %v", err)
	}
	return pool, userID
}

func TestPgWebAuthnCredentialStore(t *testing.T) {
	pool, userID := itestPhase12(t)
	ctx := context.Background()
	store, err := NewPgWebAuthnStore(pool)
	if err != nil {
		t.Fatal(err)
	}

	id, err := store.CreateCredential(ctx, WebAuthnCredential{
		UserID: userID, CredentialID: []byte("cred-A"), PublicKey: []byte("pkA"),
		SignCount: 3, Transports: []string{"usb", "internal"}, Name: "yubikey",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// Duplicate credential_id is rejected (UNIQUE).
	if _, err := store.CreateCredential(ctx, WebAuthnCredential{
		UserID: userID, CredentialID: []byte("cred-A"), PublicKey: []byte("pkB"),
	}); !codeIs(err, CodeWebAuthnFailed) {
		t.Fatalf("duplicate credential must fail closed: %v", err)
	}
	creds, err := store.ActiveCredentialsForUser(ctx, userID)
	if err != nil || len(creds) != 1 {
		t.Fatalf("list: %v len=%d", err, len(creds))
	}
	c := creds[0]
	if c.SignCount != 3 || c.Name != "yubikey" || len(c.Transports) != 2 {
		t.Fatalf("credential round trip: %+v", c)
	}
	// Counter updates never rewind (GREATEST guard): a stale lower value
	// is a no-op on the counter column.
	if err := store.UpdateSignCount(ctx, id, 9); err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateSignCount(ctx, id, 4); err != nil {
		t.Fatal(err)
	}
	creds, _ = store.ActiveCredentialsForUser(ctx, userID)
	if creds[0].SignCount != 9 {
		t.Fatalf("sign_count must not rewind: %d", creds[0].SignCount)
	}
	if creds[0].LastUsedAt == nil {
		t.Fatal("last_used_at must be stamped by UpdateSignCount")
	}
	// Clone response: revoke ⇒ credential leaves the active set; counter
	// update on a revoked row fails closed.
	if err := store.RevokeCredential(ctx, id); err != nil {
		t.Fatal(err)
	}
	if creds, _ := store.ActiveCredentialsForUser(ctx, userID); len(creds) != 0 {
		t.Fatal("revoked credential must leave the active set")
	}
	if err := store.UpdateSignCount(ctx, id, 10); !codeIs(err, CodeWebAuthnFailed) {
		t.Fatalf("counter write on revoked credential must fail: %v", err)
	}
}

func TestPgLoginHistory(t *testing.T) {
	pool, userID := itestPhase12(t)
	ctx := context.Background()
	svc, err := NewLoginHistoryService(pool)
	if err != nil {
		t.Fatal(err)
	}

	// Invalid result vocabulary is rejected before insert.
	if err := svc.RecordLogin(ctx, LoginEvent{UserID: userID, Result: "BOGUS"}); !codeIs(err, CodeInvalidRequest) {
		t.Fatalf("bad result must reject: %v", err)
	}
	// Four events with explicit timestamps (newest last).
	base := time.Now().UTC().Add(-time.Hour)
	for i, res := range []LoginResult{
		LoginResultFailed, LoginResult2FAFailed, LoginResultLocked, LoginResultSuccess,
	} {
		if err := svc.RecordLogin(ctx, LoginEvent{
			UserID: userID, Timestamp: base.Add(time.Duration(i) * time.Minute),
			IP: "192.0.2.7", UserAgent: "ua", Result: res, SessionID: "sid",
		}); err != nil {
			t.Fatalf("record %d: %v", i, err)
		}
	}
	// Page 1: newest first, limit 2 → cursor.
	p1, err := svc.List(ctx, userID, 2, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(p1.Events) != 2 || p1.NextCursor == "" {
		t.Fatalf("page1: %v %q", p1.Events, p1.NextCursor)
	}
	if p1.Events[0].Result != LoginResultSuccess || p1.Events[1].Result != LoginResultLocked {
		t.Fatalf("ordering: %v", p1.Events)
	}
	p2, err := svc.List(ctx, userID, 2, p1.NextCursor)
	if err != nil {
		t.Fatal(err)
	}
	if len(p2.Events) != 2 || p2.NextCursor != "" {
		t.Fatalf("page2: %v %q", p2.Events, p2.NextCursor)
	}
	if p2.Events[0].Result != LoginResult2FAFailed {
		t.Fatalf("keyset continuation wrong: %v", p2.Events)
	}
	// 90-day window excludes stale rows.
	old := time.Now().UTC().Add(-91 * 24 * time.Hour)
	if err := svc.RecordLogin(ctx, LoginEvent{
		UserID: userID, Timestamp: old, Result: LoginResultSuccess,
	}); err != nil {
		t.Fatal(err)
	}
	all, err := svc.List(ctx, userID, 100, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(all.Events) != 4 {
		t.Fatalf("90d window must exclude older rows: %d", len(all.Events))
	}
	// Cross-user isolation: another user sees nothing.
	if p, _ := svc.List(ctx, userID+999, 100, ""); len(p.Events) != 0 {
		t.Fatal("foreign history must not leak")
	}
	// Geo stays NULL without a resolver — never fabricated.
	if all.Events[0].GeoCity != "" || all.Events[0].GeoCountry != "" {
		t.Fatal("geo fields must be NULL without a resolver")
	}
}

func TestPgAntiPhishingCode(t *testing.T) {
	pool, userID := itestPhase12(t)
	ctx := context.Background()
	svc, err := NewAntiPhishingService(pool)
	if err != nil {
		t.Fatal(err)
	}
	// Unset → nil (the notification banner convention).
	code, err := svc.Get(ctx, userID)
	if err != nil || code != nil {
		t.Fatalf("unset must read nil: %v %v", code, err)
	}
	if err := svc.Set(ctx, userID, "blue-tiger-42"); err != nil {
		t.Fatal(err)
	}
	code, err = svc.Get(ctx, userID)
	if err != nil || code == nil || *code != "blue-tiger-42" {
		t.Fatalf("get: %v %v", code, err)
	}
	// Service-level bounds.
	requireCode(t, svc.Set(ctx, userID, "abc"), CodeInvalidRequest)
	// The CHECK constraint backs the bound too (defence in depth) —
	// write a too-short value straight through the pool.
	if _, err := pool.Exec(ctx,
		`UPDATE users SET anti_phishing_code='xy' WHERE id=$1`, userID); err == nil {
		t.Fatal("CHECK must reject <4 chars")
	}
	// Clear.
	if err := svc.Set(ctx, userID, ""); err != nil {
		t.Fatal(err)
	}
	if code, _ := svc.Get(ctx, userID); code != nil {
		t.Fatal("cleared code must read nil")
	}
}
