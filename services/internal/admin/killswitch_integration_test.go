// PostgreSQL integration test for the Phase-11 kill-switch control
// plane (Tasks 11.3.4/11.3.8/11.3.12): durable set/clear with dual
// control, audit-chain rows, flag writes and reconcile.
//
// Gated on EXC_PG_TEST=1; targets EXC_TEST_DSN (pgGate convention).
// Applies migration 200 when the dev DB lacks it; baseline anchors
// (admin_audit_log, audit_hash_chain) must already exist.
package admin

import (
	"errors"
	"testing"
	"time"

	excerrors "exchange/pkg/errors"
)

// excerrors_code unwraps a coded error for assertions (this package's
// other tests use codeOf-equivalent helpers on their own types).
func excerrors_code(err error) string {
	var e *excerrors.Error
	if errors.As(err, &e) {
		return e.Code
	}
	return ""
}

func TestKillSwitchServiceIntegration(t *testing.T) {
	pool, ctx := pgGate(t)
	for _, anchor := range []string{"admin_audit_log", "audit_hash_chain"} {
		if !hasTable(t, ctx, pool, anchor) {
			t.Skipf("baseline table %s missing — target a migrated database", anchor)
		}
	}
	applyMigration(t, ctx, pool, "trading_suspensions", "200_trading_suspensions.up.sql")
	// Clean slate for the unique ACTIVE index.
	if _, err := pool.Exec(ctx,
		`UPDATE trading_suspensions SET state='CLEARED', cleared_at=now()
		 WHERE state='ACTIVE'`); err != nil {
		t.Fatalf("pre-clean: %v", err)
	}

	flags := &fakeFlagWriter{}
	svc, err := NewKillSwitchService(KillSwitchDeps{
		Pool:  pool,
		Flags: flags,
		Roles: mapResolver(map[int64]string{
			9001: "Risk Manager", 9002: "Super Admin", 9003: "Support Agent",
		}),
		Now: func() time.Time { return time.Now().UTC() },
	})
	if err != nil {
		t.Fatalf("service: %v", err)
	}

	// GLOBAL without a second authorizer → DUAL_CONTROL_REQUIRED.
	if _, err := svc.Set(ctx, AdminActor{UserID: 9001},
		"GLOBAL", "", "market integrity incident"); excerrors_code(err) != "DUAL_CONTROL_REQUIRED" {
		t.Fatalf("global set without approver: %v", err)
	}
	// Same-user approver → DUAL_CONTROL_REQUIRED.
	if _, err := svc.Set(ctx, AdminActor{UserID: 9001, ApproverID: 9001},
		"GLOBAL", "", "market integrity incident"); excerrors_code(err) != "DUAL_CONTROL_REQUIRED" {
		t.Fatalf("self-approved set: %v", err)
	}
	// Ineligible role → UNAUTHORIZED_ROLE.
	if _, err := svc.Set(ctx, AdminActor{UserID: 9003, ApproverID: 9002},
		"GLOBAL", "", "market integrity incident"); excerrors_code(err) != "UNAUTHORIZED_ROLE" {
		t.Fatalf("support agent must not halt: %v", err)
	}

	// GLOBAL set: durable row + flag + audit chain.
	s, err := svc.Set(ctx, AdminActor{UserID: 9001, ApproverID: 9002, ClientIP: "10.0.0.5"},
		"GLOBAL", "", "market integrity incident")
	if err != nil {
		t.Fatalf("global set: %v", err)
	}
	if s.State != "ACTIVE" || s.ApprovedBy == nil || *s.ApprovedBy != 9002 {
		t.Fatalf("record: %+v", s)
	}
	if flags.set["GLOBAL:"] != "market integrity incident" {
		t.Fatalf("global flag not raised: %v", flags.set)
	}

	// Idempotent re-set returns the active record.
	s2, err := svc.Set(ctx, AdminActor{UserID: 9002, ApproverID: 9001},
		"GLOBAL", "", "duplicate set")
	if err != nil || s2.SuspensionID != s.SuspensionID {
		t.Fatalf("idempotent set: %+v %v", s2, err)
	}

	// Scoped kills are single-approver per Task 11.3.8.
	sc, err := svc.Set(ctx, AdminActor{UserID: 9001}, "ACCOUNT", "12345",
		"account-level suspension")
	if err != nil || sc.Scope != ScopeAccount || sc.ApprovedBy != nil {
		t.Fatalf("scoped set: %+v %v", sc, err)
	}
	if flags.set["ACCOUNT:12345"] == "" {
		t.Fatal("account flag not raised")
	}

	// CLEAR the global flag needs dual control too.
	if _, err := svc.Clear(ctx, AdminActor{UserID: 9001}, "GLOBAL", "", "resume"); excerrors_code(err) != "DUAL_CONTROL_REQUIRED" {
		t.Fatalf("global clear without approver: %v", err)
	}
	c, err := svc.Clear(ctx, AdminActor{UserID: 9002, ApproverID: 9001},
		"GLOBAL", "", "incident over")
	if err != nil || c.State != "CLEARED" || c.ClearApprovedBy == nil {
		t.Fatalf("clear: %+v %v", c, err)
	}
	if _, still := flags.set["GLOBAL:"]; still {
		t.Fatal("global flag not removed")
	}

	// Status + reconcile: account suspension still ACTIVE; wipe the flag
	// map and reconcile must re-raise it.
	active, err := svc.Status(ctx)
	if err != nil || len(active) == 0 {
		t.Fatalf("status: %v %v", active, err)
	}
	flags.set = map[string]string{}
	n, err := svc.ReconcileFlags(ctx)
	if err != nil || n != len(active) {
		t.Fatalf("reconcile: %d %v", n, err)
	}
	if flags.set["ACCOUNT:12345"] == "" {
		t.Fatal("reconcile did not re-raise the account flag")
	}

	// Tear down the test-scoped suspension.
	if _, err := svc.Clear(ctx, AdminActor{UserID: 9001},
		"ACCOUNT", "12345", "test cleanup"); err != nil {
		t.Fatalf("cleanup clear: %v", err)
	}

	// Both transitions carried audit-chain anchors.
	var anchored int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM audit_hash_chain
		 WHERE table_name='admin_audit_log'
		   AND record_id IN (SELECT id FROM admin_audit_log
		                      WHERE action LIKE 'killswitch.%'
		                        AND created_at > now() - interval '10 minutes')`).
		Scan(&anchored); err != nil {
		t.Fatalf("chain probe: %v", err)
	}
	if anchored < 3 {
		t.Fatalf("expected ≥3 chain anchors, got %d", anchored)
	}
}
