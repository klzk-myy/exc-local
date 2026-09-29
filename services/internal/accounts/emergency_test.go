// Emergency self-freeze (Task 12.3.10 + 12.3.12 part 3) — PG-gated
// integration tests over the saga's real persistence (accounts, users,
// account_freeze_events, admin_audit_log, unfreeze_requests) with fake
// order/session/key/alert seams.
//
// Gated: EXC_PG_TEST=1 (see integration_test.go testPool).
package accounts

import (
	"context"
	stderrors "errors"
	"testing"

	excerrors "exchange/pkg/errors"
)

// fake seams --------------------------------------------------------------

type freezeFakeDispatcher struct {
	calls     int
	err       error
	result    *MassCancelResult
	failUntil int // fail the first N calls, then succeed
}

func (d *freezeFakeDispatcher) MassCancel(ctx context.Context, scope MassCancelScope) (*MassCancelResult, error) {
	d.calls++
	if d.err != nil {
		return nil, d.err
	}
	if d.calls <= d.failUntil {
		return nil, stderrors.New("engine timeout")
	}
	if d.result != nil {
		return d.result, nil
	}
	return &MassCancelResult{Cancelled: 4}, nil
}
func (d *freezeFakeDispatcher) SubmitClose(ctx context.Context, req CloseOrderRequest) (*OrderAck, error) {
	return &OrderAck{Accepted: true}, nil
}

type freezeFakeSessions struct {
	n     int
	err   error
	keep  string
	calls int
}

func (s *freezeFakeSessions) RevokeAllExcept(ctx context.Context, accountID int64, userID, keepSID string) (int, error) {
	s.calls++
	s.keep = keepSID
	return s.n, s.err
}

type freezeFakeKeys struct {
	n   int
	err error
}

func (k *freezeFakeKeys) RevokeAllKeys(ctx context.Context, accountID int64, reason string) (int, error) {
	return k.n, k.err
}

type freezeFakeOrderLister struct {
	ids []int64
	err error
}

func (l *freezeFakeOrderLister) OpenOrderIDs(ctx context.Context, accountID int64) ([]int64, error) {
	return l.ids, l.err
}

type freezeFakeAlerter struct {
	alerts []FreezeAlert
}

func (a *freezeFakeAlerter) Raise(ctx context.Context, al FreezeAlert) error {
	a.alerts = append(a.alerts, al)
	return nil
}

func freezeCodeOf(t *testing.T, err error) string {
	t.Helper()
	var e *excerrors.Error
	if !stderrors.As(err, &e) {
		t.Fatalf("expected coded error, got %v", err)
	}
	return e.Code
}

// TestIntegrationEmergencyFreezeHappyPath: cancel succeeds first try,
// sessions+keys revoked, account FROZEN with SELF_FREEZE event + audit.
func TestIntegrationEmergencyFreezeHappyPath(t *testing.T) {
	pool := testPool(t)
	uid, aid := seedMaster(t, pool, "T1")

	disp := &freezeFakeDispatcher{result: &MassCancelResult{Cancelled: 7}}
	sess := &freezeFakeSessions{n: 3}
	keys := &freezeFakeKeys{n: 2}
	alerts := &freezeFakeAlerter{}
	svc := NewEmergencyFreezeService(pool, disp, sess, keys,
		&freezeFakeOrderLister{}, alerts)

	res, err := svc.Freeze(context.Background(), EmergencyFreezeRequest{
		AccountID: aid, UserID: uid, SessionID: "sid-keep", ClientIP: "10.0.0.9",
	})
	if err != nil {
		t.Fatalf("freeze: %v", err)
	}
	if res.Status != "FROZEN" || res.OrdersCancelled != 7 ||
		res.SessionsRevoked != 3 || res.APIKeysRevoked != 2 {
		t.Fatalf("result wrong: %+v", res)
	}
	if res.PartialFailure || res.CancelFailed || res.LoginSuspended {
		t.Fatalf("unexpected failure flags: %+v", res)
	}
	if disp.calls != 1 {
		t.Fatalf("cancel called %d times, want 1", disp.calls)
	}
	if sess.keep != "sid-keep" {
		t.Fatalf("keep sid = %q, want sid-keep", sess.keep)
	}
	var status, reason string
	if err := pool.QueryRow(context.Background(),
		`SELECT a.status, e.reason FROM accounts a
		 JOIN account_freeze_events e ON e.account_id = a.id
		 WHERE a.id=$1`, aid).Scan(&status, &reason); err != nil {
		t.Fatalf("freeze state read: %v", err)
	}
	if status != "FROZEN" || reason != SelfFreezeReason {
		t.Fatalf("status=%s reason=%s", status, reason)
	}
	var audit int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM admin_audit_log
		  WHERE action='account.self_freeze' AND target_id=$1`, aid).Scan(&audit); err != nil {
		t.Fatalf("audit read: %v", err)
	}
	if audit != 1 {
		t.Fatalf("audit rows = %d, want 1", audit)
	}
	if len(alerts.alerts) != 0 {
		t.Fatalf("unexpected alerts: %+v", alerts.alerts)
	}
}

// TestIntegrationEmergencyFreezeCancelExhausted: all 3 cancels fail →
// still FROZEN, login suspended, P1 alert carries the stuck order ids.
func TestIntegrationEmergencyFreezeCancelExhausted(t *testing.T) {
	pool := testPool(t)
	uid, aid := seedMaster(t, pool, "T1")

	disp := &freezeFakeDispatcher{err: stderrors.New("matching engine timeout")}
	sess := &freezeFakeSessions{n: 1}
	keys := &freezeFakeKeys{n: 0}
	alerts := &freezeFakeAlerter{}
	svc := NewEmergencyFreezeService(pool, disp, sess, keys,
		&freezeFakeOrderLister{ids: []int64{101, 102, 103}}, alerts)

	res, err := svc.Freeze(context.Background(), EmergencyFreezeRequest{
		AccountID: aid, UserID: uid, SessionID: "sid-1",
	})
	if err != nil {
		t.Fatalf("freeze: %v", err)
	}
	if !res.CancelFailed || !res.LoginSuspended {
		t.Fatalf("expected cancel-failure escalation: %+v", res)
	}
	if disp.calls != EmergencyCancelAttempts {
		t.Fatalf("cancel called %d times, want %d", disp.calls, EmergencyCancelAttempts)
	}
	if len(res.StuckOrderIDs) != 3 {
		t.Fatalf("stuck ids = %v", res.StuckOrderIDs)
	}
	var ustatus string
	if err := pool.QueryRow(context.Background(),
		`SELECT status FROM users WHERE id=$1`, uid).Scan(&ustatus); err != nil {
		t.Fatalf("user status read: %v", err)
	}
	if ustatus != "SUSPENDED" {
		t.Fatalf("owner not login-suspended: %s", ustatus)
	}
	if len(alerts.alerts) != 1 || alerts.alerts[0].Severity != "P1" ||
		alerts.alerts[0].Code != "SELF_FREEZE_CANCEL_STUCK" {
		t.Fatalf("alerts = %+v", alerts.alerts)
	}
	if alerts.alerts[0].Details["order_ids"] == "" {
		t.Fatalf("alert missing order ids: %+v", alerts.alerts[0].Details)
	}
}

// TestIntegrationEmergencyFreezeCancelRetriesRecover: first two
// dispatches fail, third succeeds → no escalation.
func TestIntegrationEmergencyFreezeCancelRetriesRecover(t *testing.T) {
	pool := testPool(t)
	uid, aid := seedMaster(t, pool, "T1")

	disp := &freezeFakeDispatcher{failUntil: 2, result: &MassCancelResult{Cancelled: 1}}
	alerts := &freezeFakeAlerter{}
	svc := NewEmergencyFreezeService(pool, disp, &freezeFakeSessions{},
		&freezeFakeKeys{}, &freezeFakeOrderLister{}, alerts)

	res, err := svc.Freeze(context.Background(), EmergencyFreezeRequest{
		AccountID: aid, UserID: uid,
	})
	if err != nil {
		t.Fatalf("freeze: %v", err)
	}
	if disp.calls != 3 {
		t.Fatalf("cancel calls = %d, want 3", disp.calls)
	}
	if res.CancelFailed || res.LoginSuspended || len(alerts.alerts) != 0 {
		t.Fatalf("unexpected escalation: %+v alerts=%v", res, alerts.alerts)
	}
}

// TestIntegrationEmergencyFreezePartialFailure: session terminator dies
// → freeze stands, PartialFailure flagged, SELF_FREEZE_PARTIAL P1 alert.
func TestIntegrationEmergencyFreezePartialFailure(t *testing.T) {
	pool := testPool(t)
	uid, aid := seedMaster(t, pool, "T1")

	alerts := &freezeFakeAlerter{}
	svc := NewEmergencyFreezeService(pool,
		&freezeFakeDispatcher{}, &freezeFakeSessions{err: stderrors.New("redis down")},
		&freezeFakeKeys{n: 1}, &freezeFakeOrderLister{}, alerts)

	res, err := svc.Freeze(context.Background(), EmergencyFreezeRequest{
		AccountID: aid, UserID: uid,
	})
	if err != nil {
		t.Fatalf("freeze: %v", err)
	}
	if !res.PartialFailure || res.PartialDetail == "" {
		t.Fatalf("partial failure not surfaced: %+v", res)
	}
	if len(alerts.alerts) != 1 || alerts.alerts[0].Code != "SELF_FREEZE_PARTIAL" {
		t.Fatalf("alerts = %+v", alerts.alerts)
	}
	var status string
	if err := pool.QueryRow(context.Background(),
		`SELECT status FROM accounts WHERE id=$1`, aid).Scan(&status); err != nil {
		t.Fatalf("status read: %v", err)
	}
	if status != "FROZEN" {
		t.Fatalf("account not frozen: %s", status)
	}
}

// TestIntegrationEmergencyFreezeGuards: non-owner is FORBIDDEN; a
// FROZEN account cannot be re-self-frozen.
func TestIntegrationEmergencyFreezeGuards(t *testing.T) {
	pool := testPool(t)
	uid, aid := seedMaster(t, pool, "T1")
	otherUID, _ := seedMaster(t, pool, "T1")

	svc := NewEmergencyFreezeService(pool, &freezeFakeDispatcher{},
		&freezeFakeSessions{}, &freezeFakeKeys{}, &freezeFakeOrderLister{}, &freezeFakeAlerter{})

	if err := func() error {
		_, err := svc.Freeze(context.Background(), EmergencyFreezeRequest{
			AccountID: aid, UserID: otherUID})
		return err
	}(); freezeCodeOf(t, err) != CodeForbidden {
		t.Fatalf("foreign self-freeze: %v", err)
	}
	// Freeze once then attempt again → INVALID_REQUEST (status guard).
	if _, err := svc.Freeze(context.Background(), EmergencyFreezeRequest{
		AccountID: aid, UserID: uid}); err != nil {
		t.Fatalf("first freeze: %v", err)
	}
	if _, err := svc.Freeze(context.Background(), EmergencyFreezeRequest{
		AccountID: aid, UserID: uid}); freezeCodeOf(t, err) != CodeInvalidRequest {
		t.Fatalf("re-freeze: %v", err)
	}
}

// TestIntegrationUnfreezeRequestFlow: SUBMITTED → idempotent replay with
// merged refs; wrong-state and wrong-reason rejections.
func TestIntegrationUnfreezeRequestFlow(t *testing.T) {
	pool := testPool(t)
	uid, aid := seedMaster(t, pool, "T1")
	svc := NewUnfreezeService(pool)
	ctx := context.Background()

	// Not frozen → INVALID_REQUEST.
	if _, err := svc.Request(ctx, aid, uid, "", "", ""); freezeCodeOf(t, err) != CodeInvalidRequest {
		t.Fatalf("unfrozen request: %v", err)
	}

	// Self-freeze the account via a direct event + status transition.
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE accounts SET status='FROZEN' WHERE id=$1`, aid); err != nil {
		t.Fatalf("freeze update: %v", err)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO account_freeze_events
		   (account_id, action, reason, initiated_by, approved_by, prev_status, new_status)
		 VALUES ($1,'FREEZE','SELF_FREEZE',$2,$2,'ACTIVE','FROZEN')`, aid, uid); err != nil {
		t.Fatalf("freeze event: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	// Foreign user → FORBIDDEN.
	otherUID, _ := seedMaster(t, pool, "T1")
	if _, err := svc.Request(ctx, aid, otherUID, "", "", ""); freezeCodeOf(t, err) != CodeForbidden {
		t.Fatalf("foreign request: %v", err)
	}

	req, err := svc.Request(ctx, aid, uid, "docref-1", "", "help")
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if req.Status != UnfreezeSubmitted || req.Replayed || req.IDDocumentRef != "docref-1" {
		t.Fatalf("request wrong: %+v", req)
	}
	// Resubmit merges refs and replays the open row.
	req2, err := svc.Request(ctx, aid, uid, "", "live-2", "more")
	if err != nil {
		t.Fatalf("resubmit: %v", err)
	}
	if !req2.Replayed || req2.ID != req.ID ||
		req2.IDDocumentRef != "docref-1" || req2.LivenessRef != "live-2" {
		t.Fatalf("replay wrong: %+v", req2)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM unfreeze_requests WHERE account_id=$1`, aid)
	})

	// A non-SELF_FREEZE reason is a legal hold — the client path refuses.
	uid2, aid2 := seedMaster(t, pool, "T1")
	tx2, _ := pool.Begin(ctx)
	_, _ = tx2.Exec(ctx, `UPDATE accounts SET status='FROZEN' WHERE id=$1`, aid2)
	_, _ = tx2.Exec(ctx,
		`INSERT INTO account_freeze_events
		   (account_id, action, reason, initiated_by, approved_by, prev_status, new_status)
		 VALUES ($1,'FREEZE','LEGAL_HOLD',0,0,'ACTIVE','FROZEN')`, aid2)
	_ = tx2.Commit(ctx)
	if _, err := svc.Request(ctx, aid2, uid2, "", "", ""); freezeCodeOf(t, err) != CodeForbidden {
		t.Fatalf("legal-hold request: %v", err)
	}
}
