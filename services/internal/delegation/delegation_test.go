// Delegation (Task 12.3.11) — pure-unit policy/scope checks plus a
// PG-gated integration suite over the migration-074 schema.
//
// Gated tests: EXC_PG_TEST=1 (same convention as internal/accounts).
package delegation

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"exchange/pkg/decimal"
)

// ---- pure unit tests (no DB) -------------------------------------------

func TestValidRole(t *testing.T) {
	for _, r := range []Role{RoleReadOnly, RoleFinanceManager, RoleTrader, RoleApprover} {
		if !ValidRole(r) {
			t.Fatalf("role %s rejected", r)
		}
	}
	for _, r := range []Role{"", "SUPER_ADMIN", "Finance Ops", "CLIENT_OWNER"} {
		if ValidRole(r) {
			t.Fatalf("role %q accepted — venue roles must never leak in", r)
		}
	}
}

func TestScopeContainment(t *testing.T) {
	s := Scope{AccountIDs: []int64{10, 20}, Instruments: []string{"EURUSD"}}
	if !s.CoversAccount(10) || !s.CoversAccount(20) || s.CoversAccount(30) {
		t.Fatalf("account scope wrong")
	}
	if !s.CoversInstrument("EURUSD") || s.CoversInstrument("USDJPY") {
		t.Fatalf("instrument scope wrong")
	}
	if (Scope{}).CoversAccount(1) || (Scope{}).CoversInstrument("X") {
		t.Fatalf("empty scope must grant nothing (fail closed)")
	}
}

func TestPolicyApplies(t *testing.T) {
	p := &Policy{} // no threshold → always applies
	if !p.Applies(decimal.NewFromInt(1), "USD") {
		t.Fatalf("threshold-less policy must cover every amount")
	}
	th := decimal.NewFromInt(100000)
	p2 := &Policy{ThresholdAmount: &th, ThresholdCurrency: "USD"}
	if !p2.Applies(decimal.NewFromInt(100000), "USD") ||
		p2.Applies(decimal.NewFromInt(99999), "USD") ||
		p2.Applies(decimal.NewFromInt(999999), "EUR") {
		t.Fatalf("threshold policy scoping wrong")
	}
}

// TestRoleCapabilityMatrix pins the task item-2 restrictions.
func TestRoleCapabilityMatrix(t *testing.T) {
	cases := []struct {
		role   Role
		action Action
		want   bool
	}{
		{RoleReadOnly, ActionRead, true},
		{RoleReadOnly, ActionTrade, false},
		{RoleReadOnly, ActionWithdrawal, false},
		{RoleTrader, ActionTrade, true},
		{RoleTrader, ActionWithdrawal, false},     // traders cannot withdraw
		{RoleTrader, ActionSecurityChange, false}, // or change security
		{RoleFinanceManager, ActionInternalTransfer, true},
		{RoleFinanceManager, ActionWithdrawal, false}, // transfers stay in-family
		{RoleFinanceManager, ActionTrade, false},
		{RoleApprover, ActionApprove, true},
		{RoleApprover, ActionWithdrawal, false}, // approvers never initiate
		{RoleApprover, ActionTrade, false},
	}
	for _, c := range cases {
		if got := roleActions[c.role][c.action]; got != c.want {
			t.Fatalf("role %s action %s = %v, want %v", c.role, c.action, got, c.want)
		}
	}
}

// ---- PG-gated integration ----------------------------------------------

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

// seedUser + seedAccount create throwaway fixtures; cleanup removes
// every delegation row + the account/user.
func seedUser(t *testing.T, pool *pgxpool.Pool) int64 {
	t.Helper()
	var uid int64
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO users (email, status)
		 VALUES ($1,'ACTIVE') RETURNING id`,
		fmt.Sprintf("deleg_it_%d@example.com", time.Now().UnixNano())).Scan(&uid); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	return uid
}

func seedMaster(t *testing.T, pool *pgxpool.Pool) (uid, aid int64) {
	t.Helper()
	uid = seedUser(t, pool)
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO accounts (user_id, account_type, kyc_tier)
		 VALUES ($1,'MARGIN','T1') RETURNING id`, uid).Scan(&aid); err != nil {
		t.Fatalf("seed master: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = pool.Exec(ctx,
			`DELETE FROM client_approval_decisions WHERE request_id IN
			   (SELECT id FROM client_approval_requests WHERE master_account_id=$1)`, aid)
		_, _ = pool.Exec(ctx,
			`DELETE FROM client_approval_requests WHERE master_account_id=$1`, aid)
		_, _ = pool.Exec(ctx,
			`DELETE FROM client_approval_policies WHERE master_account_id=$1`, aid)
		_, _ = pool.Exec(ctx,
			`DELETE FROM client_delegation_events WHERE master_account_id=$1`, aid)
		_, _ = pool.Exec(ctx,
			`DELETE FROM client_role_bindings WHERE delegated_user_id IN
			   (SELECT id FROM client_delegated_users WHERE master_account_id=$1)`, aid)
		// principal_role_systems must go BEFORE the delegated users —
		// the subselect reads the rows being deleted.
		_, _ = pool.Exec(ctx,
			`DELETE FROM principal_role_systems WHERE principal_id IN
			   (SELECT user_id FROM client_delegated_users WHERE master_account_id=$1)`, aid)
		_, _ = pool.Exec(ctx,
			`DELETE FROM client_delegated_users WHERE master_account_id=$1`, aid)
		_, _ = pool.Exec(ctx, `DELETE FROM accounts WHERE id=$1`, aid)
		_, _ = pool.Exec(ctx, `DELETE FROM users WHERE id=$1`, uid)
	})
	return uid, aid
}

type killRecorder struct{ killed []int64 }

func (k *killRecorder) KillUserSessions(ctx context.Context, userID int64) error {
	k.killed = append(k.killed, userID)
	return nil
}

// TestIntegrationDelegationLifecycle: create → authorize → scope update
// → revoke (sessions killed) → audit rows present.
func TestIntegrationDelegationLifecycle(t *testing.T) {
	pool := testPool(t)
	masterUID, masterAID := seedMaster(t, pool)
	killer := &killRecorder{}
	svc := NewService(pool, killer)
	ctx := context.Background()

	delegateUID := seedUser(t, pool)
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM principal_role_systems WHERE principal_id=$1`, delegateUID)
		_, _ = pool.Exec(ctx, `DELETE FROM users WHERE id=$1`, delegateUID)
	})

	du, err := svc.CreateDelegatedUser(ctx, masterAID, masterUID,
		CreateDelegatedUserRequest{
			UserID: delegateUID, DisplayName: "Desk Trader 1",
			Role:  RoleTrader,
			Scope: Scope{AccountIDs: []int64{masterAID}, Instruments: []string{"EURUSD"}},
		})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if du.Status != StatusActive || du.Role != RoleTrader || du.BindingID == 0 {
		t.Fatalf("delegated user wrong: %+v", du)
	}

	// Enforcement: trader may trade EURUSD on the master, nothing else.
	if err := svc.Authorize(ctx, masterAID, delegateUID, ActionTrade,
		Target{AccountID: masterAID, Instrument: "EURUSD"}); err != nil {
		t.Fatalf("in-scope trade denied: %v", err)
	}
	if err := svc.Authorize(ctx, masterAID, delegateUID, ActionTrade,
		Target{AccountID: masterAID, Instrument: "USDJPY"}); err == nil {
		t.Fatalf("out-of-scope instrument allowed")
	}
	if err := svc.Authorize(ctx, masterAID, delegateUID, ActionWithdrawal,
		Target{AccountID: masterAID}); err == nil {
		t.Fatalf("trader withdrawal allowed")
	}
	// Master principal is unrestricted by delegation.
	if err := svc.Authorize(ctx, masterAID, masterUID, ActionWithdrawal,
		Target{AccountID: masterAID}); err != nil {
		t.Fatalf("master denied: %v", err)
	}

	// Scope change + audit.
	if _, err := svc.UpdateBinding(ctx, masterAID, masterUID, du.ID,
		RoleTrader, Scope{AccountIDs: []int64{masterAID},
			Instruments: []string{"EURUSD", "USDJPY"}}, nil); err != nil {
		t.Fatalf("update binding: %v", err)
	}
	if err := svc.Authorize(ctx, masterAID, delegateUID, ActionTrade,
		Target{AccountID: masterAID, Instrument: "USDJPY"}); err != nil {
		t.Fatalf("widened scope denied: %v", err)
	}

	// Revoke → identity gone, sessions killed.
	if err := svc.Revoke(ctx, masterAID, masterUID, du.ID, "test"); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if len(killer.killed) != 1 || killer.killed[0] != delegateUID {
		t.Fatalf("session kill = %v", killer.killed)
	}
	id, err := svc.ResolveIdentity(ctx, masterAID, delegateUID)
	if err != nil || id != nil {
		t.Fatalf("revoked delegate still resolves: %+v", id)
	}

	var events int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM client_delegation_events
		  WHERE master_account_id=$1`, masterAID).Scan(&events); err != nil {
		t.Fatalf("audit read: %v", err)
	}
	if events < 3 { // DELEGATION_CREATED + SCOPE_CHANGED + DELEGATION_REVOKED
		t.Fatalf("audit events = %d, want >= 3", events)
	}
}

// TestIntegrationApprovals: 2-of-N policy → request → self-approve
// refused → M-of-N tally → APPROVED → consume through the withdrawal
// gate seam.
func TestIntegrationApprovals(t *testing.T) {
	pool := testPool(t)
	masterUID, masterAID := seedMaster(t, pool)
	svc := NewService(pool, &killRecorder{})
	ctx := context.Background()

	approver1, approver2 := seedUser(t, pool), seedUser(t, pool)
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM principal_role_systems WHERE principal_id IN ($1,$2)`, approver1, approver2)
		_, _ = pool.Exec(ctx, `DELETE FROM users WHERE id IN ($1,$2)`, approver1, approver2)
	})
	for i, uid := range []int64{approver1, approver2} {
		if _, err := svc.CreateDelegatedUser(ctx, masterAID, masterUID,
			CreateDelegatedUserRequest{
				UserID: uid, DisplayName: fmt.Sprintf("Approver %d", i),
				Role:  RoleApprover,
				Scope: Scope{AccountIDs: []int64{masterAID}},
			}); err != nil {
			t.Fatalf("create approver %d: %v", i, err)
		}
	}

	if _, err := svc.SetPolicy(ctx, masterAID, masterUID,
		OpWithdrawal, 2, nil, "", 3600); err != nil {
		t.Fatalf("set policy: %v", err)
	}

	// Withdrawal gate: no request yet → records one, not approved.
	approved, reqID, err := svc.CheckWithdrawal(ctx, masterAID, masterUID,
		"USD", decimal.NewFromInt(50000), "IBAN-1")
	if err != nil {
		t.Fatalf("gate: %v", err)
	}
	if approved || reqID == 0 {
		t.Fatalf("gate should record pending request, got approved=%v id=%d", approved, reqID)
	}
	// Replay the same withdrawal → same pending request, still gated.
	approved, reqID2, err := svc.CheckWithdrawal(ctx, masterAID, masterUID,
		"USD", decimal.NewFromInt(50000), "IBAN-1")
	if err != nil || approved || reqID2 != reqID {
		t.Fatalf("gate replay: approved=%v id=%d err=%v", approved, reqID2, err)
	}

	// Anti-self-approval: the requester holds no approver hat, and even
	// an approver that requested the op can't decide its own request.
	if _, err := svc.Decide(ctx, reqID, masterUID, true, ""); err == nil {
		t.Fatalf("non-approver decided")
	}
	if _, err := svc.Decide(ctx, reqID, approver1, true, "ok"); err != nil {
		t.Fatalf("approver1 decide: %v", err)
	}
	// One vote only — duplicate decide rejects.
	if _, err := svc.Decide(ctx, reqID, approver1, true, "dup"); err == nil {
		t.Fatalf("duplicate vote accepted")
	}
	req, err := svc.Decide(ctx, reqID, approver2, true, "ok")
	if err != nil {
		t.Fatalf("approver2 decide: %v", err)
	}
	if req.Status != ReqApproved || req.ApprovalsCount != 2 {
		t.Fatalf("request not approved: %+v", req)
	}

	// Approved resubmission consumes the request and proceeds.
	approved, consumedID, err := svc.CheckWithdrawal(ctx, masterAID, masterUID,
		"USD", decimal.NewFromInt(50000), "IBAN-1")
	if err != nil || !approved || consumedID != reqID {
		t.Fatalf("approved replay: approved=%v id=%d err=%v", approved, consumedID, err)
	}
	// Single-use: the next identical withdrawal opens a NEW request.
	approved, reqID3, err := svc.CheckWithdrawal(ctx, masterAID, masterUID,
		"USD", decimal.NewFromInt(50000), "IBAN-1")
	if err != nil || approved || reqID3 == reqID {
		t.Fatalf("consumed request must not replay: approved=%v id=%d", approved, reqID3)
	}
}

// TestIntegrationDelegationGuards: foreign-master access denied;
// delegate cannot manage delegation; approver cannot initiate a
// governed request; disjoint role system enforced.
func TestIntegrationDelegationGuards(t *testing.T) {
	pool := testPool(t)
	masterUID, masterAID := seedMaster(t, pool)
	foreignUID, _ := seedMaster(t, pool)
	svc := NewService(pool, &killRecorder{})
	ctx := context.Background()

	if _, err := svc.List(ctx, masterAID, foreignUID); err == nil {
		t.Fatalf("foreign list allowed")
	}
	if _, err := svc.CreateDelegatedUser(ctx, masterAID, foreignUID,
		CreateDelegatedUserRequest{UserID: seedUser(t, pool),
			DisplayName: "x", Role: RoleReadOnly,
			Scope: Scope{AccountIDs: []int64{masterAID}}}); err == nil {
		t.Fatalf("foreign create allowed")
	}

	// A delegate cannot create delegates.
	delegateUID := seedUser(t, pool)
	if _, err := svc.CreateDelegatedUser(ctx, masterAID, masterUID,
		CreateDelegatedUserRequest{UserID: delegateUID, DisplayName: "ro",
			Role: RoleReadOnly, Scope: Scope{AccountIDs: []int64{masterAID}}}); err != nil {
		t.Fatalf("create delegate: %v", err)
	}
	if _, err := svc.CreateDelegatedUser(ctx, masterAID, delegateUID,
		CreateDelegatedUserRequest{UserID: seedUser(t, pool),
			DisplayName: "nested", Role: RoleTrader,
			Scope: Scope{AccountIDs: []int64{masterAID}}}); err == nil {
		t.Fatalf("delegated user minted another delegate")
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM principal_role_systems WHERE principal_id=$1`, delegateUID)
		_, _ = pool.Exec(ctx, `DELETE FROM users WHERE id=$1`, delegateUID)
	})

	// Disjoint role system: a VENUE_ADMIN principal can't be delegated.
	adminUID := seedUser(t, pool)
	if _, err := pool.Exec(ctx,
		`INSERT INTO principal_role_systems (principal_id, role_system, first_granted_by)
		 VALUES ($1,'VENUE_ADMIN',$2)`, adminUID, masterUID); err != nil {
		t.Fatalf("seed role system: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM principal_role_systems WHERE principal_id=$1`, adminUID)
		_, _ = pool.Exec(ctx, `DELETE FROM users WHERE id=$1`, adminUID)
	})
	if _, err := svc.CreateDelegatedUser(ctx, masterAID, masterUID,
		CreateDelegatedUserRequest{UserID: adminUID, DisplayName: "admin-delegate",
			Role: RoleReadOnly, Scope: Scope{AccountIDs: []int64{masterAID}}}); err == nil {
		t.Fatalf("venue-admin principal became a client delegate")
	}

	// Approver can't initiate a governed op.
	if _, err := svc.SetPolicy(ctx, masterAID, masterUID, OpWithdrawal, 1, nil, "", 0); err != nil {
		t.Fatalf("set policy: %v", err)
	}
	approverUID := seedUser(t, pool)
	if _, err := svc.CreateDelegatedUser(ctx, masterAID, masterUID,
		CreateDelegatedUserRequest{UserID: approverUID, DisplayName: "appr",
			Role: RoleApprover, Scope: Scope{AccountIDs: []int64{masterAID}}}); err != nil {
		t.Fatalf("create approver: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM principal_role_systems WHERE principal_id=$1`, approverUID)
		_, _ = pool.Exec(ctx, `DELETE FROM users WHERE id=$1`, approverUID)
	})
	if _, err := svc.RequestApproval(ctx, RequestApprovalInput{
		MasterAccount: masterAID, Operation: OpWithdrawal,
		Payload:     map[string]any{"amount": "1", "currency": "USD"},
		RequestedBy: approverUID,
	}); err == nil {
		t.Fatalf("approver initiated a governed withdrawal")
	}
}

// TestIntegrationApprovalExpirySweep: a policy with a short window
// produces a PENDING request the sweep lapses with an audit row.
func TestIntegrationApprovalExpirySweep(t *testing.T) {
	pool := testPool(t)
	masterUID, masterAID := seedMaster(t, pool)
	svc := NewService(pool, &killRecorder{})
	ctx := context.Background()

	approverUID := seedUser(t, pool)
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM principal_role_systems WHERE principal_id=$1`, approverUID)
		_, _ = pool.Exec(ctx, `DELETE FROM users WHERE id=$1`, approverUID)
	})
	if _, err := svc.CreateDelegatedUser(ctx, masterAID, masterUID,
		CreateDelegatedUserRequest{UserID: approverUID, DisplayName: "appr",
			Role: RoleApprover, Scope: Scope{AccountIDs: []int64{masterAID}}}); err != nil {
		t.Fatalf("create approver: %v", err)
	}
	if _, err := svc.SetPolicy(ctx, masterAID, masterUID, OpInternalTransfer,
		1, nil, "", 60); err != nil {
		t.Fatalf("set policy: %v", err)
	}
	req, err := svc.RequestApproval(ctx, RequestApprovalInput{
		MasterAccount: masterAID, Operation: OpInternalTransfer,
		Payload:     map[string]any{"amount": "999", "currency": "USD"},
		RequestedBy: masterUID,
	})
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	// Force-expire then sweep.
	if _, err := pool.Exec(ctx,
		`UPDATE client_approval_requests SET expires_at=now()-interval '1s'
		  WHERE id=$1`, req.ID); err != nil {
		t.Fatalf("force expiry: %v", err)
	}
	n, err := svc.ExpireApprovals(ctx, 100)
	if err != nil || n != 1 {
		t.Fatalf("sweep: n=%d err=%v", n, err)
	}
	var status string
	if err := pool.QueryRow(ctx,
		`SELECT status FROM client_approval_requests WHERE id=$1`,
		req.ID).Scan(&status); err != nil {
		t.Fatalf("request read: %v", err)
	}
	if status != ReqExpired {
		t.Fatalf("status = %s, want EXPIRED", status)
	}
}

// TestIntegrationRevokeAll: emergency master revocation kills every
// delegation + every session.
func TestIntegrationRevokeAll(t *testing.T) {
	pool := testPool(t)
	masterUID, masterAID := seedMaster(t, pool)
	killer := &killRecorder{}
	svc := NewService(pool, killer)
	ctx := context.Background()

	u1, u2 := seedUser(t, pool), seedUser(t, pool)
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM principal_role_systems WHERE principal_id IN ($1,$2)`, u1, u2)
		_, _ = pool.Exec(ctx, `DELETE FROM users WHERE id IN ($1,$2)`, u1, u2)
	})
	for i, uid := range []int64{u1, u2} {
		if _, err := svc.CreateDelegatedUser(ctx, masterAID, masterUID,
			CreateDelegatedUserRequest{
				UserID: uid, DisplayName: fmt.Sprintf("d%d", i),
				Role:  RoleReadOnly,
				Scope: Scope{AccountIDs: []int64{masterAID}},
			}); err != nil {
			t.Fatalf("create %d: %v", i, err)
		}
	}
	n, err := svc.RevokeAll(ctx, masterAID, masterUID, "panic")
	if err != nil || n != 2 {
		t.Fatalf("revoke-all: n=%d err=%v", n, err)
	}
	if len(killer.killed) != 2 {
		t.Fatalf("sessions killed = %v", killer.killed)
	}
	var active int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM client_delegated_users
		  WHERE master_account_id=$1 AND status='ACTIVE'`, masterAID).Scan(&active); err != nil {
		t.Fatalf("count: %v", err)
	}
	if active != 0 {
		t.Fatalf("active delegations remain: %d", active)
	}
}
