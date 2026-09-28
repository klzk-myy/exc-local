// Integration tests for the account-state cluster against dev PostgreSQL.
//
// Gated: skipped unless EXC_PG_TEST=1. Target defaults to the dev database
// at localhost:5433 (docker-compose.dev.yml); override with EXC_PG_DSN.
// Requires migrations 003/004/014 (+025 api_keys, +067 max_sub_accounts,
// +152 account_freeze_events) applied.
//
// Run: EXC_PG_TEST=1 go test ./internal/accounts/ -run Integration -v
package accounts

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"exchange/pkg/decimal"
)

func testPool(t *testing.T) *pgxpool.Pool {
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

// seedMaster inserts a throwaway user + master account; returns
// (userID, accountID). tier is a kyc_tier_enum value.
func seedMaster(t *testing.T, pool *pgxpool.Pool, tier string) (int64, int64) {
	t.Helper()
	ctx := context.Background()
	email := fmt.Sprintf("acct_it_%d@example.com", time.Now().UnixNano())
	var uid, aid int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO users (email, status) VALUES ($1,'ACTIVE') RETURNING id`, email).
		Scan(&uid); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO accounts (user_id, account_type, kyc_tier)
		 VALUES ($1,'MARGIN',$2) RETURNING id`, uid, tier).Scan(&aid); err != nil {
		t.Fatalf("seed master account: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = pool.Exec(ctx, `DELETE FROM account_freeze_events WHERE account_id IN
			(SELECT id FROM accounts WHERE id=$1 OR parent_account_id=$1)`, aid)
		_, _ = pool.Exec(ctx, `DELETE FROM api_keys WHERE account_id IN
			(SELECT id FROM accounts WHERE id=$1 OR parent_account_id=$1)`, aid)
		_, _ = pool.Exec(ctx, `DELETE FROM balances WHERE account_id IN
			(SELECT id FROM accounts WHERE id=$1 OR parent_account_id=$1)`, aid)
		_, _ = pool.Exec(ctx, `DELETE FROM admin_audit_log WHERE target_type='account' AND target_id IN
			(SELECT id FROM accounts WHERE id=$1 OR parent_account_id=$1)`, aid)
		_, _ = pool.Exec(ctx, `DELETE FROM accounts WHERE parent_account_id=$1`, aid)
		_, _ = pool.Exec(ctx, `DELETE FROM accounts WHERE id=$1`, aid)
		_, _ = pool.Exec(ctx, `DELETE FROM users WHERE id=$1`, uid)
	})
	return uid, aid
}

func TestIntegrationSubAccountLifecycle(t *testing.T) {
	pool := testPool(t)
	_, master := seedMaster(t, pool, "T1")
	svc := NewSubAccountService(pool)
	ctx := context.Background()

	sub, err := svc.Create(ctx, master)
	if err != nil {
		t.Fatalf("create sub-account: %v", err)
	}
	if sub.MasterID != master || sub.Status != StatusActive || !sub.TradingEnabled {
		t.Fatalf("sub-account wrong: %+v", sub)
	}
	// Segregated balance write goes through the balances table directly
	// only in tests (production path: ledger service).
	if _, err := pool.Exec(ctx,
		`INSERT INTO balances (account_id, currency, available, locked)
		 VALUES ($1,'USD', 1000, 250)`, sub.ID); err != nil {
		t.Fatalf("seed balance: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO balances (account_id, currency, available, locked)
		 VALUES ($1,'USD', 500, 0)`, master); err != nil {
		t.Fatalf("seed master balance: %v", err)
	}

	list, err := svc.List(ctx, master)
	if err != nil || len(list) != 1 {
		t.Fatalf("list: %v len=%d", err, len(list))
	}
	if len(list[0].Balances) != 1 || !list[0].Balances[0].Total.Equal(decimal.NewFromInt(1250)) {
		t.Fatalf("sub balance wrong: %+v", list[0].Balances)
	}

	view, err := svc.Aggregate(ctx, master)
	if err != nil {
		t.Fatalf("aggregate: %v", err)
	}
	if len(view.Balances.ByCurrency) != 1 {
		t.Fatalf("family currencies=%+v", view.Balances.ByCurrency)
	}
	if !view.Balances.ByCurrency[0].Total.Equal(decimal.NewFromInt(1750)) {
		t.Fatalf("family USD total=%s, want 1750 (500 master + 1250 sub)", view.Balances.ByCurrency[0].Total)
	}
}

func TestIntegrationSubAccountCeiling(t *testing.T) {
	pool := testPool(t)
	_, master := seedMaster(t, pool, "T0")
	svc := NewSubAccountService(pool)
	ctx := context.Background()

	// Retail tier default is 20 — force ceiling 1 via admin path.
	if err := svc.SetLimit(ctx, master, 1); err != nil {
		t.Fatalf("set limit: %v", err)
	}
	if _, err := svc.Create(ctx, master); err != nil {
		t.Fatalf("first sub: %v", err)
	}
	if _, err := svc.Create(ctx, master); err == nil {
		t.Fatal("second sub must reject at ceiling 1")
	} else {
		requireCode(t, err, CodeForbidden)
	}
	// Ceiling beyond the 1,000 hard cap is rejected before touching PG.
	if err := svc.SetLimit(ctx, master, 1001); err == nil {
		t.Fatal("limit 1001 must reject")
	} else {
		requireCode(t, err, CodeInvalidRequest)
	}
	if err := svc.SetLimit(ctx, master, 1000); err != nil {
		t.Fatalf("limit 1000 must accept: %v", err)
	}
}

func TestIntegrationSubAccountNestingDenied(t *testing.T) {
	pool := testPool(t)
	_, master := seedMaster(t, pool, "T1")
	svc := NewSubAccountService(pool)
	ctx := context.Background()

	sub, err := svc.Create(ctx, master)
	if err != nil {
		t.Fatalf("create sub: %v", err)
	}
	if _, err := svc.Create(ctx, sub.ID); err == nil {
		t.Fatal("sub-account of sub-account must reject")
	} else {
		requireCode(t, err, CodeForbidden)
	}
	ok, err := svc.IsSubAccountOf(ctx, master, sub.ID)
	if err != nil || !ok {
		t.Fatalf("ownership check: %v ok=%v", err, ok)
	}
	ok, _ = svc.IsSubAccountOf(ctx, sub.ID, master)
	if ok {
		t.Fatal("master must not be a sub-account of its child")
	}
}

func TestIntegrationFreezeLifecycle(t *testing.T) {
	pool := testPool(t)
	_, master := seedMaster(t, pool, "T1")
	resolver := func(context.Context, int64) (string, error) { return RoleComplianceOfficer, nil }
	svc := NewFreezeService(pool, resolver)
	ctx := context.Background()

	if err := svc.AssertMutable(ctx, master); err != nil {
		t.Fatalf("active account must be mutable: %v", err)
	}
	// Same-actor approval violates dual control.
	err := svc.Freeze(ctx, AdminActor{UserID: 9001, Role: RoleComplianceOfficer, ApproverID: 9001},
		master, "court order 123", "10.0.0.1")
	requireCode(t, err, CodeDualControlRequired)

	err = svc.Freeze(ctx, AdminActor{UserID: 9001, ApproverID: 9002},
		master, "court order 123", "10.0.0.1")
	if err != nil {
		t.Fatalf("freeze: %v", err)
	}
	// FROZEN rejects mutations.
	requireCode(t, svc.AssertMutable(ctx, master), CodeAccountFrozen)
	st, _ := svc.StatusOf(ctx, master)
	if st != StatusFrozen {
		t.Fatalf("status=%s, want FROZEN", st)
	}
	// Double freeze rejected.
	requireCode(t, svc.Freeze(ctx, AdminActor{UserID: 9001, ApproverID: 9002},
		master, "again", ""), CodeInvalidRequest)

	// Audit trail rows.
	events, err := svc.FreezeHistory(ctx, master)
	if err != nil || len(events) != 1 {
		t.Fatalf("freeze history len=%d err=%v", len(events), err)
	}
	if events[0].Reason != "court order 123" || events[0].ApprovedBy != 9002 ||
		events[0].PrevStatus != "ACTIVE" || events[0].NewStatus != "FROZEN" {
		t.Fatalf("freeze event wrong: %+v", events[0])
	}
	var auditN int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM admin_audit_log
		  WHERE target_type='account' AND target_id=$1 AND action='account.freeze'`,
		master).Scan(&auditN); err != nil || auditN != 1 {
		t.Fatalf("admin audit rows=%d err=%v", auditN, err)
	}

	// Unfreeze restores ACTIVE.
	if err := svc.Unfreeze(ctx, AdminActor{UserID: 9002, ApproverID: 9001},
		master, "hold released", ""); err != nil {
		t.Fatalf("unfreeze: %v", err)
	}
	if err := svc.AssertMutable(ctx, master); err != nil {
		t.Fatalf("post-unfreeze mutable: %v", err)
	}
	events, _ = svc.FreezeHistory(ctx, master)
	if len(events) != 2 || events[0].Action != "UNFREEZE" {
		t.Fatalf("history after unfreeze: %+v", events)
	}
}

func TestIntegrationFreezeRoleGate(t *testing.T) {
	pool := testPool(t)
	_, master := seedMaster(t, pool, "T1")
	// Support Agent is below Compliance Officer — must reject.
	resolver := func(context.Context, int64) (string, error) { return "Support Agent", nil }
	svc := NewFreezeService(pool, resolver)
	err := svc.Freeze(context.Background(),
		AdminActor{UserID: 1, ApproverID: 2}, master, "x", "")
	requireCode(t, err, CodeUnauthorizedRole)
}

func TestIntegrationSubAccountAPIKeys(t *testing.T) {
	pool := testPool(t)
	uid, master := seedMaster(t, pool, "T2")
	subs := NewSubAccountService(pool)
	keys := NewAPIKeyService(pool, subs, nil)
	ctx := context.Background()

	sub, err := subs.Create(ctx, master)
	if err != nil {
		t.Fatalf("create sub: %v", err)
	}
	issued, err := keys.IssueForSubAccount(ctx, master, sub.ID,
		[]string{ScopeRead, ScopeTrade}, "mm-bot", uid)
	if err != nil {
		t.Fatalf("issue key: %v", err)
	}
	if issued.KeyID == "" || issued.Secret == "" || issued.AccountID != sub.ID {
		t.Fatalf("issued key malformed: %+v", issued)
	}
	// transfer scope is never issuable on a sub-account key.
	if _, err := keys.IssueForSubAccount(ctx, master, sub.ID,
		[]string{ScopeRead, ScopeTransfer}, "bad", uid); err == nil {
		t.Fatal("transfer scope must reject")
	} else {
		requireCode(t, err, CodeInsufficientScope)
	}
	// Key on an account the master does not own → NOT_FOUND.
	if _, err := keys.IssueForSubAccount(ctx, master, master,
		[]string{ScopeRead}, "self", uid); err == nil {
		t.Fatal("key on non-sub-account must reject")
	} else {
		requireCode(t, err, CodeNotFound)
	}
	// Stored row carries hash/prefix only — no plaintext.
	var plaintext int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM api_keys WHERE id=$1 AND key_hash IS NOT NULL
		  AND key_id=$2`, issued.ID, issued.KeyID).Scan(&plaintext); err != nil || plaintext != 1 {
		t.Fatalf("stored key row check n=%d err=%v", plaintext, err)
	}
	list, err := keys.ListForSubAccount(ctx, master, sub.ID)
	if err != nil || len(list) != 1 || list[0].Status != "ACTIVE" {
		t.Fatalf("key list: %+v err=%v", list, err)
	}
	if err := keys.RevokeForSubAccount(ctx, master, sub.ID, issued.ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	// Idempotent revoke.
	if err := keys.RevokeForSubAccount(ctx, master, sub.ID, issued.ID); err != nil {
		t.Fatalf("idempotent revoke: %v", err)
	}
	list, _ = keys.ListForSubAccount(ctx, master, sub.ID)
	if list[0].Status != "REVOKED" || list[0].RevokedAt == nil {
		t.Fatalf("revoked state wrong: %+v", list[0])
	}
}
