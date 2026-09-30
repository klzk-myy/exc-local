// PostgreSQL integration test — Phase-09 Task 9.3.29 item 4 secrets
// inventory (migration 089). Gated on EXC_PG_TEST=1; targets
// EXC_TEST_DSN (default: the dev database). Requires migration 089
// applied; the test applies it idempotently when secrets_inventory is
// absent. admin_audit_log + audit_hash_chain (migrations 010/011) are
// assumed present — they anchor the whole admin suite.
package security

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"exchange/internal/db"
)

func invPgGate(t *testing.T) (*pgxpool.Pool, context.Context) {
	t.Helper()
	if os.Getenv("EXC_PG_TEST") != "1" {
		t.Skip("set EXC_PG_TEST=1 to run Postgres integration tests")
	}
	dsn := os.Getenv("EXC_TEST_DSN")
	if dsn == "" {
		dsn = "postgres://exchange:exchange_dev@127.0.0.1:5433/exchange?sslmode=disable"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	pool, err := db.NewPool(ctx, dsn, 4)
	if err != nil {
		cancel()
		t.Skipf("postgres unreachable: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		cancel()
		t.Skipf("postgres unreachable: %v", err)
	}
	t.Cleanup(func() { pool.Close(); cancel() })

	for _, need := range []string{"secrets_inventory", "admin_audit_log", "audit_hash_chain"} {
		var has bool
		if err := pool.QueryRow(ctx, `
			SELECT EXISTS (SELECT 1 FROM information_schema.tables
			 WHERE table_name = $1)`, need).Scan(&has); err != nil {
			t.Fatalf("table check %s: %v", need, err)
		}
		if !has && need == "secrets_inventory" {
			up, err := os.ReadFile("../db/migrations/089_secrets_inventory.up.sql")
			if err != nil {
				t.Fatalf("read migration 089: %v", err)
			}
			if _, err := pool.Exec(ctx, string(up)); err != nil {
				t.Fatalf("apply migration 089: %v", err)
			}
			continue
		}
		if !has {
			t.Fatalf("anchor table %s missing — apply base migrations", need)
		}
	}
	return pool, ctx
}

func superAdminResolver() RoleResolver {
	return func(context.Context, int64) (string, error) { return "Super Admin", nil }
}

func invEntry(name string, class SecretClass) InventoryEntryInput {
	return InventoryEntryInput{
		SecretName: name, Class: class,
		Category: "test", Owner: "Platform / test",
		Consumers:         []string{"gateway"},
		TTL:               90 * 24 * time.Hour,
		RotationProcedure: "rotate-secrets.sh " + name,
	}
}

func TestInventoryStoreCRUD(t *testing.T) {
	pool, ctx := invPgGate(t)
	store := NewPgInventoryStore(pool)
	name := "it-jwt-hs256-key"
	defer pool.Exec(ctx, `DELETE FROM secrets_inventory WHERE secret_name LIKE 'it-%'`)

	e, err := store.Upsert(ctx, invEntry(name, ClassJWTSigningKey))
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if e.SecretName != name || e.AlertLead != defaultAlertWindow {
		t.Fatalf("upsert row: %+v", e)
	}

	// Get + List.
	got, ok, err := store.Get(ctx, name)
	if err != nil || !ok || got.SecretName != name {
		t.Fatalf("get: ok=%v err=%v", ok, err)
	}
	if _, ok, _ := store.Get(ctx, "it-nonexistent"); ok {
		t.Fatal("get: phantom row")
	}
	list, err := store.List(ctx)
	if err != nil || len(list) == 0 {
		t.Fatalf("list: %v", err)
	}

	// LastRotated: inventoried-but-never-rotated → found, zero time.
	at, found, err := store.LastRotated(ctx, name)
	if err != nil || !found || !at.IsZero() {
		t.Fatalf("lastrotated unrotated: found=%v at=%v err=%v", found, at, err)
	}

	// MarkRotated stamps the timestamp.
	marked, err := store.MarkRotated(ctx, name, time.Now().UTC())
	if err != nil || marked.LastRotatedAt == nil {
		t.Fatalf("mark: %v %+v", err, marked)
	}
	at, _, _ = store.LastRotated(ctx, name)
	if at.IsZero() {
		t.Fatal("lastrotated after mark still zero")
	}
}

func TestInventoryOverdueTransition(t *testing.T) {
	pool, ctx := invPgGate(t)
	store := NewPgInventoryStore(pool)
	defer pool.Exec(ctx, `DELETE FROM secrets_inventory WHERE secret_name LIKE 'it-%'`)

	stale := time.Now().UTC().Add(-100 * 24 * time.Hour) // past the 90d TTL
	in := invEntry("it-stale-banking-key", ClassBankingAPIKey)
	in.LastRotatedAt = &stale
	if _, err := store.Upsert(ctx, in); err != nil {
		t.Fatalf("upsert stale: %v", err)
	}

	over, err := store.Overdue(ctx, time.Now().UTC())
	if err != nil {
		t.Fatalf("overdue: %v", err)
	}
	var names []string
	for _, e := range over {
		names = append(names, e.SecretName)
	}
	if len(names) == 0 {
		t.Fatal("overdue set empty — stale row not flagged")
	}
	found := false
	for _, n := range names {
		found = found || n == "it-stale-banking-key"
	}
	if !found {
		t.Fatalf("overdue set %v missing it-stale-banking-key", names)
	}

	// Rotation clears the overdue flag.
	if _, err := store.MarkRotated(ctx, "it-stale-banking-key", time.Now().UTC()); err != nil {
		t.Fatalf("mark: %v", err)
	}
	over, _ = store.Overdue(ctx, time.Now().UTC())
	for _, e := range over {
		if e.SecretName == "it-stale-banking-key" {
			t.Fatal("still overdue after mark-rotated")
		}
	}
}

func TestInventoryServiceAuditAndAlerts(t *testing.T) {
	pool, ctx := invPgGate(t)
	store := NewPgInventoryStore(pool)
	al := &recAlerter{}
	svc := NewInventoryService(store, superAdminResolver(), al, nil)
	defer pool.Exec(ctx, `DELETE FROM secrets_inventory WHERE secret_name LIKE 'it-%'`)

	// Upsert writes the row AND its admin_audit_log + hash-chain link
	// in one transaction (doc §5).
	if _, err := svc.UpsertEntry(ctx, 1,
		invEntry("it-audited", ClassAPIKeyMaterial), "127.0.0.1"); err != nil {
		t.Fatalf("svc upsert: %v", err)
	}
	var n int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM admin_audit_log
		 WHERE action = 'secrets_inventory.upsert'`).Scan(&n); err != nil {
		t.Fatalf("audit count: %v", err)
	}
	if n == 0 {
		t.Fatal("upsert produced no admin_audit_log row")
	}

	// Service-level MarkRotated audits too, emergency marks carry the
	// incident ref through to the audit payload.
	if _, err := svc.MarkRotated(ctx, 1, "it-audited",
		RotationMark{Emergency: true, IncidentRef: "INC-TEST-1"}, "127.0.0.1"); err != nil {
		t.Fatalf("svc mark: %v", err)
	}
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM admin_audit_log
		 WHERE action = 'secrets_inventory.mark_rotated'
		   AND after_state->>'incident_ref' = 'INC-TEST-1'`).Scan(&n); err != nil {
		t.Fatalf("mark audit count: %v", err)
	}
	if n == 0 {
		t.Fatal("emergency mark produced no audited incident_ref")
	}

	// Evaluator: an overdue row pages SECRET_ROTATION_OVERDUE exactly
	// once until cleared.
	stale := time.Now().UTC().Add(-91 * 24 * time.Hour)
	in := invEntry("it-overdue-alert", ClassRedisPassword)
	in.LastRotatedAt = &stale
	if _, err := svc.UpsertEntry(ctx, 1, in, ""); err != nil {
		t.Fatalf("upsert stale: %v", err)
	}
	if _, err := svc.EvaluateRotation(ctx); err != nil {
		t.Fatalf("eval 1: %v", err)
	}
	if _, err := svc.EvaluateRotation(ctx); err != nil {
		t.Fatalf("eval 2: %v", err)
	}
	count := 0
	for _, r := range al.raised {
		if r[0] == "P2" && r[1] == CodeSecretRotationOverdue {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("overdue alerts=%d want exactly 1 (deduped)", count)
	}
	// MarkRotated clears the latch — a later breach pages again.
	if _, err := svc.MarkRotated(ctx, 1, "it-overdue-alert", RotationMark{}, ""); err != nil {
		t.Fatalf("clear mark: %v", err)
	}
	if _, err := svc.EvaluateRotation(ctx); err != nil {
		t.Fatalf("eval 3: %v", err)
	}
	for _, r := range al.raised {
		if r[1] == CodeSecretRotationOverdue && count != 1 {
			t.Fatalf("post-clear eval re-paged: %+v", al.raised)
		}
	}
}

func TestInventoryCoverageIntegration(t *testing.T) {
	pool, ctx := invPgGate(t)
	store := NewPgInventoryStore(pool)
	svc := NewInventoryService(store, superAdminResolver(), nil, nil)
	defer pool.Exec(ctx, `DELETE FROM secrets_inventory WHERE secret_name LIKE 'it-cov-%'`)

	// Populate one row per registered class; coverage of the class leg
	// must pass, and a required ref without a row must fail the report.
	for i, c := range []SecretClass{
		ClassJWTSigningKey, ClassAPIKeyMaterial, ClassDBCredential,
		ClassRedisPassword, ClassAeronToken, ClassTLSCertificate,
		ClassBankingAPIKey,
	} {
		in := invEntry("it-cov-"+string(rune('a'+i)), c)
		if _, err := store.Upsert(ctx, in); err != nil {
			t.Fatalf("upsert class %s: %v", c, err)
		}
	}
	rep, err := svc.CoverageCheck(ctx, []SecretRef{
		{Name: "it-cov-a", Required: true},
		{Name: "it-cov-missing", Required: true},
	})
	if err != nil {
		t.Fatalf("coverage: %v", err)
	}
	if rep.OK || len(rep.MissingSecrets) != 1 {
		t.Fatalf("coverage should fail closed on missing ref: %+v", rep)
	}
	rep, err = svc.CoverageCheck(ctx, []SecretRef{{Name: "it-cov-a", Required: true}})
	if err != nil || !rep.OK {
		t.Fatalf("full coverage should pass: rep=%+v err=%v", rep, err)
	}
}
