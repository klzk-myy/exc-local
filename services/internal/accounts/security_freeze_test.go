// Integration test for SecurityFreezeService — the Task 12.3.12 machine
// freeze seam used by WebAuthn clone detection.
//
// Gated: skipped unless EXC_PG_TEST=1 (dev DB must carry migrations
// 003 + 152).
package accounts

import (
	"context"
	"strings"
	"testing"
)

func TestSecurityFreezeUserAccounts(t *testing.T) {
	pool := testPool(t)
	uid, aid := seedMaster(t, pool, "T1")
	ctx := context.Background()

	svc := NewSecurityFreezeService(pool)
	if err := svc.FreezeUserAccounts(ctx, uid, "webauthn credential clone detected"); err != nil {
		t.Fatalf("freeze: %v", err)
	}

	var astatus, ustatus string
	if err := pool.QueryRow(ctx,
		`SELECT status::text FROM accounts WHERE id=$1`, aid).Scan(&astatus); err != nil {
		t.Fatal(err)
	}
	if astatus != "FROZEN" {
		t.Fatalf("account must be FROZEN, got %s", astatus)
	}
	if err := pool.QueryRow(ctx,
		`SELECT status::text FROM users WHERE id=$1`, uid).Scan(&ustatus); err != nil {
		t.Fatal(err)
	}
	if ustatus != "SUSPENDED" {
		t.Fatalf("user must be SUSPENDED, got %s", ustatus)
	}
	// Audit row marks the system trigger distinctly from a legal hold.
	var action, meta string
	err := pool.QueryRow(ctx,
		`SELECT action::text, metadata::text FROM account_freeze_events
		  WHERE account_id=$1 ORDER BY id DESC LIMIT 1`, aid).Scan(&action, &meta)
	if err != nil {
		t.Fatalf("freeze event: %v", err)
	}
	if action != "FREEZE" {
		t.Fatalf("action=%s", action)
	}
	if got := meta; got == "" || !strings.Contains(got, "webauthn_clone") {
		t.Fatalf("metadata must carry the system trigger: %s", got)
	}
	// Idempotent: freezing again is a no-op, not an error.
	if err := svc.FreezeUserAccounts(ctx, uid, "again"); err != nil {
		t.Fatalf("re-freeze must be idempotent: %v", err)
	}
}
