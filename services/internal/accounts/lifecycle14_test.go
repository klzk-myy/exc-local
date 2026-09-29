// Phase-14 account-lifecycle integration tests — cooling-off
// (Tasks 14.3.11/14.3.12) and closure & offboarding (Task 14.3.9)
// against the dev schema (migrations 063/213 applied).
//
// Gated: EXC_PG_TEST=1 (see integration_test.go testPool).
package accounts

import (
	"context"
	stderrors "errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// extra fakes --------------------------------------------------------------

type fakeCloseDispatcher struct {
	cancels int
	closes  int
	err     error
	pool    *pgxpool.Pool // non-nil → apply the engine effect to PG
}

func (d *fakeCloseDispatcher) MassCancel(ctx context.Context,
	scope MassCancelScope) (*MassCancelResult, error) {
	d.cancels++
	if d.err != nil {
		return nil, d.err
	}
	if d.pool != nil {
		tag, err := d.pool.Exec(ctx,
			`UPDATE orders SET status='CANCELLED'
			  WHERE account_id=$1 AND status = ANY($2)`,
			scope.AccountID, openOrderStatuses)
		if err != nil {
			return nil, err
		}
		return &MassCancelResult{Cancelled: int(tag.RowsAffected())}, nil
	}
	return &MassCancelResult{Cancelled: 2}, nil
}
func (d *fakeCloseDispatcher) SubmitClose(ctx context.Context,
	req CloseOrderRequest) (*OrderAck, error) {
	d.closes++
	if d.err != nil {
		return nil, d.err
	}
	if d.pool != nil {
		if _, err := d.pool.Exec(ctx,
			`UPDATE positions SET quantity=0
			  WHERE account_id=$1 AND instrument_id=$2`,
			req.AccountID, req.InstrumentID); err != nil {
			return nil, err
		}
	}
	return &OrderAck{Accepted: true, OrderID: 9000 + int64(d.closes)}, nil
}

type fakeSweeper struct {
	reqs []SweepRequest
	err  error
}

func (s *fakeSweeper) SweepWithdrawal(ctx context.Context,
	req SweepRequest) (*SweepResult, error) {
	if s.err != nil {
		return nil, s.err
	}
	s.reqs = append(s.reqs, req)
	return &SweepResult{WithdrawalID: 5000 + int64(len(s.reqs)),
		Status: "CONFIRMED"}, nil
}

type fakeBens struct {
	ref string
	ok  bool
}

func (b *fakeBens) VerifiedBeneficiaryFor(ctx context.Context,
	accountID int64, currency string) (string, bool, error) {
	return b.ref, b.ok, nil
}

type fakeNotify struct {
	events []string
}

func (n *fakeNotify) call(ctx context.Context, userID int64,
	event string, payload map[string]any) {
	n.events = append(n.events, event)
}

func lifecycleCleanup(t *testing.T, pool *pgxpool.Pool, aid int64) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, table := range []string{
		"cooling_off_periods", "account_closures", "compliance_holds",
		"orders", "positions", "funding_transactions",
		"settlement_instructions",
	} {
		col := "account_id"
		if table == "cooling_off_periods" {
			_, _ = pool.Exec(ctx,
				`DELETE FROM cooling_off_periods WHERE user_id IN
				 (SELECT user_id FROM accounts WHERE id=$1)`, aid)
			continue
		}
		_, _ = pool.Exec(ctx,
			fmt.Sprintf(`DELETE FROM %s WHERE %s=$1`, table, col), aid)
	}
}

// leveredInst returns an instrument id with max_leverage > 0 (every
// seeded dev instrument qualifies; the first row suffices).
func leveredInst(t *testing.T, pool *pgxpool.Pool) int64 {
	t.Helper()
	var id int64
	if err := pool.QueryRow(context.Background(),
		`SELECT id FROM instruments WHERE max_leverage > 0
		 ORDER BY id LIMIT 1`).Scan(&id); err != nil {
		t.Fatalf("levered instrument: %v", err)
	}
	return id
}

// ---------------------------------------------------------------------------
// Cooling-off — Task 14.3.11/14.3.12
// ---------------------------------------------------------------------------

func TestIntegrationCoolingOffActivateAndGate(t *testing.T) {
	pool := testPool(t)
	uid, aid := seedMaster(t, pool, "T1")
	lifecycleCleanup(t, pool, aid)
	disp := &fakeCloseDispatcher{}
	svc := NewCoolingOffService(pool, disp, &freezeFakeAlerter{},
		UserNotifier((&fakeNotify{}).call))

	p, res, err := svc.Activate(context.Background(), CoolingOffRequest{
		AccountID: aid, Duration: "7d", Acknowledged: true,
		Actor: fmt.Sprint(uid),
	})
	if err != nil {
		t.Fatalf("activate: %v", err)
	}
	if p == nil || p.ID == 0 || p.Duration != "7d" {
		t.Fatalf("period wrong: %+v", p)
	}
	if res == nil || res.PartialFailure {
		t.Fatalf("saga result wrong: %+v", res)
	}
	if !p.ExpiresAt.After(p.StartedAt) {
		t.Fatalf("expires<=started: %+v", p)
	}

	// The immutable row landed.
	var cnt int64
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM cooling_off_periods
		  WHERE user_id=$1 AND expires_at > now()`, uid).Scan(&cnt); err != nil ||
		cnt != 1 {
		t.Fatalf("period rows=%d err=%v", cnt, err)
	}

	// Admission gate: covered owner → COOLING_OFF_ACTIVE.
	if err := svc.AssertLeverageEntryAllowed(context.Background(), aid); err == nil ||
		freezeCodeOf(t, err) != CodeCoolingOffActive {
		t.Fatalf("gate: got %v, want COOLING_OFF_ACTIVE", err)
	}

	// Idempotent replay — same period, no second saga.
	p2, res2, err := svc.Activate(context.Background(), CoolingOffRequest{
		AccountID: aid, Duration: "30d", Acknowledged: true})
	if err != nil || p2 == nil || p2.ID != p.ID || res2 != nil {
		t.Fatalf("replay: p2=%+v res2=%+v err=%v", p2, res2, err)
	}
	if disp.cancels != 0 {
		t.Fatalf("replayed activate re-ran the saga: %d cancels", disp.cancels)
	}

	// Status read surface.
	st, err := svc.Status(context.Background(), uid)
	if err != nil || st == nil || st.ID != p.ID {
		t.Fatalf("status: %+v err=%v", st, err)
	}

	// Validation: bad duration / missing acknowledgement reject before
	// any write.
	if _, _, err := svc.Activate(context.Background(), CoolingOffRequest{
		AccountID: aid, Duration: "2d", Acknowledged: true}); err == nil ||
		freezeCodeOf(t, err) != CodeInvalidRequest {
		t.Fatalf("bad duration: got %v", err)
	}
	if _, _, err := svc.Activate(context.Background(), CoolingOffRequest{
		AccountID: aid, Duration: "1d"}); err == nil ||
		freezeCodeOf(t, err) != CodeInvalidRequest {
		t.Fatalf("unacknowledged: got %v", err)
	}
}

func TestIntegrationCoolingOffDeriskSaga(t *testing.T) {
	pool := testPool(t)
	uid, aid := seedMaster(t, pool, "T1")
	lifecycleCleanup(t, pool, aid)
	inst := leveredInst(t, pool)
	ctx := context.Background()

	// Seed one resting order + one open position on the levered
	// instrument.
	if _, err := pool.Exec(ctx,
		`INSERT INTO orders (account_id, instrument_id, side, order_type,
		     quantity, price, time_in_force, status)
		 VALUES ($1,$2,'BUY','LIMIT',1000,1.05,'GTC','ACTIVE')`, aid, inst); err != nil {
		t.Fatalf("seed order: %v", err)
	}
	var posID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO positions (account_id, instrument_id, side, quantity,
		     entry_price)
		 VALUES ($1,$2,'LONG',5000,1.05) RETURNING id`, aid, inst).
		Scan(&posID); err != nil {
		t.Fatalf("seed position: %v", err)
	}

	disp := &fakeCloseDispatcher{}
	alerts := &freezeFakeAlerter{}
	svc := NewCoolingOffService(pool, disp, alerts,
		UserNotifier((&fakeNotify{}).call))

	p, res, err := svc.Activate(ctx, CoolingOffRequest{
		AccountID: aid, Duration: "1d", Acknowledged: true,
		Actor: fmt.Sprint(uid)})
	if err != nil || p == nil {
		t.Fatalf("activate: %+v err=%v", p, err)
	}
	if res.PartialFailure {
		t.Fatalf("partial saga: %+v", res)
	}
	if disp.cancels != 1 {
		t.Fatalf("mass-cancel calls=%d, want 1 (per levered instrument)",
			disp.cancels)
	}
	if res.OrdersCancelled != 2 {
		t.Fatalf("cancelled=%d", res.OrdersCancelled)
	}
	if disp.closes != 1 || len(res.Closes) != 1 || !res.Closes[0].OK {
		t.Fatalf("closes=%d %+v", disp.closes, res.Closes)
	}
	if len(alerts.alerts) != 0 {
		t.Fatalf("unexpected alerts: %+v", alerts.alerts)
	}
}

// Dispatcher outage: the flag still lands (fail closed on entry), the
// partial failure surfaces in the result AND pages P1.
func TestIntegrationCoolingOffPartialFailure(t *testing.T) {
	pool := testPool(t)
	_, aid := seedMaster(t, pool, "T1")
	lifecycleCleanup(t, pool, aid)
	inst := leveredInst(t, pool)
	ctx := context.Background()
	if _, err := pool.Exec(ctx,
		`INSERT INTO positions (account_id, instrument_id, side, quantity,
		     entry_price)
		 VALUES ($1,$2,'LONG',5000,1.05)`, aid, inst); err != nil {
		t.Fatalf("seed position: %v", err)
	}

	disp := &fakeCloseDispatcher{err: stderrors.New("engine down")}
	alerts := &freezeFakeAlerter{}
	svc := NewCoolingOffService(pool, disp, alerts, nil)
	_, res, err := svc.Activate(ctx, CoolingOffRequest{
		AccountID: aid, Duration: "3d", Acknowledged: true})
	if err != nil {
		t.Fatalf("activate: %v", err)
	}
	if !res.PartialFailure {
		t.Fatalf("dispatch outage not surfaced: %+v", res)
	}
	if len(alerts.alerts) != 1 || alerts.alerts[0].Code != "COOLING_OFF_PARTIAL" {
		t.Fatalf("P1 alert missing: %+v", alerts.alerts)
	}
	// Entry still blocked — the flag landed before the saga ran.
	if err := svc.AssertLeverageEntryAllowed(ctx, aid); err == nil ||
		freezeCodeOf(t, err) != CodeCoolingOffActive {
		t.Fatalf("gate after partial saga: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Closure — Task 14.3.9
// ---------------------------------------------------------------------------

func closureSvcFor(pool *pgxpool.Pool, disp OrderDispatcher,
	sweep Sweeper, bens BeneficiaryLookup, sess *freezeFakeSessions,
	keys *freezeFakeKeys, alerts *freezeFakeAlerter,
	n *fakeNotify) *ClosureService {
	var notify UserNotifier
	if n != nil {
		notify = UserNotifier(n.call)
	}
	return NewClosureService(pool, disp, sweep, bens, sess, keys,
		&freezeFakeOrderLister{}, alerts, notify, nil)
}

func TestIntegrationClosureClientPath(t *testing.T) {
	pool := testPool(t)
	uid, aid := seedMaster(t, pool, "T1")
	lifecycleCleanup(t, pool, aid)
	ctx := context.Background()

	sess := &freezeFakeSessions{n: 4}
	keys := &freezeFakeKeys{n: 2}
	notify := &fakeNotify{}
	svc := closureSvcFor(pool, nil, &fakeSweeper{}, &fakeBens{},
		sess, keys, &freezeFakeAlerter{}, notify)

	res, err := svc.Close(ctx, ClosureRequest{
		AccountID: aid, UserID: uid, Reason: "moving broker",
	})
	if err != nil {
		t.Fatalf("close: %v", err)
	}
	if res.Status != "CLOSED" || res.ClosureID == "" {
		t.Fatalf("result wrong: %+v", res)
	}
	if res.SessionsRevoked != 4 || res.KeysRevoked != 2 {
		t.Fatalf("revocation counts: %+v", res)
	}

	var status, closureID string
	var snap []byte
	if err := pool.QueryRow(ctx,
		`SELECT a.status, c.closure_id, c.preconditions_snapshot::text
		   FROM accounts a JOIN account_closures c ON c.account_id = a.id
		  WHERE a.id=$1`, aid).Scan(&status, &closureID, &snap); err != nil {
		t.Fatalf("closure read: %v", err)
	}
	if status != "CLOSED" || closureID != res.ClosureID || len(snap) == 0 {
		t.Fatalf("closure state wrong: %s %s", status, closureID)
	}
	if len(notify.events) != 1 || notify.events[0] != "security_alert" {
		t.Fatalf("notification missing: %+v", notify.events)
	}

	// No reopening — the terminal state rejects every mutation.
	if _, err := svc.Close(ctx, ClosureRequest{
		AccountID: aid, UserID: uid, Reason: "again"}); err == nil ||
		freezeCodeOf(t, err) != CodeInvalidRequest {
		t.Fatalf("re-close: got %v", err)
	}
}

func TestIntegrationClosureBlocked(t *testing.T) {
	pool := testPool(t)
	uid, aid := seedMaster(t, pool, "T1")
	lifecycleCleanup(t, pool, aid)
	inst := leveredInst(t, pool)
	ctx := context.Background()
	if _, err := pool.Exec(ctx,
		`INSERT INTO orders (account_id, instrument_id, side, order_type,
		     quantity, price, time_in_force, status)
		 VALUES ($1,$2,'BUY','LIMIT',1000,1.05,'GTC','ACTIVE')`, aid, inst); err != nil {
		t.Fatalf("seed order: %v", err)
	}

	svc := closureSvcFor(pool, nil, &fakeSweeper{}, &fakeBens{},
		&freezeFakeSessions{}, &freezeFakeKeys{}, &freezeFakeAlerter{}, nil)
	_, err := svc.Close(ctx, ClosureRequest{
		AccountID: aid, UserID: uid, Reason: "close me"})
	if err == nil || freezeCodeOf(t, err) != CodeAccountCloseBlocked {
		t.Fatalf("got %v, want ACCOUNT_CLOSE_BLOCKED", err)
	}
	var status string
	if err := pool.QueryRow(ctx,
		`SELECT status FROM accounts WHERE id=$1`, aid).Scan(&status); err != nil ||
		status != "ACTIVE" {
		t.Fatalf("blocked close touched status: %s", status)
	}
}

func TestIntegrationClosureSweep(t *testing.T) {
	pool := testPool(t)
	uid, aid := seedMaster(t, pool, "T1")
	lifecycleCleanup(t, pool, aid)
	ctx := context.Background()
	if _, err := pool.Exec(ctx,
		`INSERT INTO balances (account_id, currency, available, locked)
		 VALUES ($1,'USD',1234.5,0)`, aid); err != nil {
		t.Fatalf("seed balance: %v", err)
	}

	sweep := &fakeSweeper{}
	svc := closureSvcFor(pool, nil, sweep,
		&fakeBens{ref: "DE00-BENE-1", ok: true},
		&freezeFakeSessions{}, &freezeFakeKeys{}, &freezeFakeAlerter{}, nil)
	res, err := svc.Close(ctx, ClosureRequest{
		AccountID: aid, UserID: uid, Reason: "withdraw all"})
	if err != nil {
		t.Fatalf("close: %v", err)
	}
	if len(res.Sweeps) != 1 || len(sweep.reqs) != 1 {
		t.Fatalf("sweep missing: %+v", res.Sweeps)
	}
	if sweep.reqs[0].Currency != "USD" ||
		sweep.reqs[0].DestinationRef != "DE00-BENE-1" ||
		sweep.reqs[0].Amount != "1234.50000000" {
		t.Fatalf("sweep request wrong: %+v", sweep.reqs[0])
	}
	var refs string
	if err := pool.QueryRow(ctx,
		`SELECT sweep_refs::text FROM account_closures
		  WHERE account_id=$1`, aid).Scan(&refs); err != nil ||
		refs == "[]" {
		t.Fatalf("sweep_refs not persisted: %q", refs)
	}
}

func TestIntegrationClosureForced(t *testing.T) {
	pool := testPool(t)
	_, aid := seedMaster(t, pool, "T1")
	lifecycleCleanup(t, pool, aid)
	inst := leveredInst(t, pool)
	ctx := context.Background()
	if _, err := pool.Exec(ctx,
		`INSERT INTO orders (account_id, instrument_id, side, order_type,
		     quantity, price, time_in_force, status)
		 VALUES ($1,$2,'BUY','LIMIT',1000,1.05,'GTC','ACTIVE')`, aid, inst); err != nil {
		t.Fatalf("seed order: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO positions (account_id, instrument_id, side, quantity,
		     entry_price)
		 VALUES ($1,$2,'LONG',5000,1.05)`, aid, inst); err != nil {
		t.Fatalf("seed position: %v", err)
	}

	// Force-closure flattens through the dispatcher BEFORE the
	// precondition recount — the pool-backed fake applies the engine
	// effect so the recount sees residuals at zero and the close
	// proceeds (production path: engine writes land via the consumer).
	disp := &fakeCloseDispatcher{pool: pool}
	svc := closureSvcFor(pool, disp, &fakeSweeper{}, &fakeBens{},
		&freezeFakeSessions{}, &freezeFakeKeys{}, &freezeFakeAlerter{}, nil)

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	res, err := svc.ForcedClose(ctx, tx, ClosureRequest{
		AccountID: aid, Reason: "court order", Forced: true,
		Actor: 42, ApproverID: 43, RequestRef: 77,
	})
	if err != nil {
		t.Fatalf("forced close: %v", err)
	}
	if disp.cancels != 1 || disp.closes != 1 {
		t.Fatalf("de-risking skipped: cancels=%d closes=%d",
			disp.cancels, disp.closes)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	var status, forced string
	var approver *int64
	if err := pool.QueryRow(ctx,
		`SELECT a.status, c.forced::text, c.approved_by
		   FROM accounts a JOIN account_closures c ON c.account_id = a.id
		  WHERE a.id=$1`, aid).Scan(&status, &forced, &approver); err != nil {
		t.Fatalf("read: %v", err)
	}
	if status != "CLOSED" || forced != "true" || approver == nil || *approver != 43 {
		t.Fatalf("forced closure state wrong: %s %s %+v", status, forced, approver)
	}
	if res.Preconditions == nil {
		t.Fatalf("preconditions snapshot missing")
	}
}

// Mandatory reason + non-owner session both reject before any work.
func TestIntegrationClosureGuards(t *testing.T) {
	pool := testPool(t)
	uid, aid := seedMaster(t, pool, "T1")
	lifecycleCleanup(t, pool, aid)
	ctx := context.Background()
	svc := closureSvcFor(pool, nil, &fakeSweeper{}, &fakeBens{},
		&freezeFakeSessions{}, &freezeFakeKeys{}, &freezeFakeAlerter{}, nil)

	if _, err := svc.Close(ctx, ClosureRequest{
		AccountID: aid, UserID: uid}); err == nil ||
		freezeCodeOf(t, err) != CodeInvalidRequest {
		t.Fatalf("missing reason: got %v", err)
	}
	if _, err := svc.Close(ctx, ClosureRequest{
		AccountID: aid, UserID: uid + 999, Reason: "r"}); err == nil ||
		freezeCodeOf(t, err) != CodeForbidden {
		t.Fatalf("non-owner: got %v", err)
	}
}
