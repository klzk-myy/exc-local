// Tests for the Phase-15 instrument lifecycle (Tasks 15.3.1/15.3.9):
// transition matrix + §7.2 role matrix + reopening-auction rule run as
// pure unit tests; stateful behavior runs against Postgres
// (EXC_PG_TEST=1 → instruments rows, admin_audit_log entries, dual
// queue) and Redis (EXC_REDIS_TEST=1 → the instrument:status:{symbol}
// engine feed, auction key, sweep bookkeeping).
package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	excredis "exchange/internal/redis"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// --- pure unit tests -------------------------------------------------------

func TestLifecycleTransitionMatrix(t *testing.T) {
	allowed := [][2]string{
		{InstDraft, InstActive},
		{InstActive, InstCancelOnly}, {InstActive, InstSuspended},
		{InstActive, InstHalted}, {InstActive, InstRestricted},
		{InstActive, InstDelisted},
		{InstCancelOnly, InstActive}, {InstCancelOnly, InstSuspended},
		{InstCancelOnly, InstHalted}, {InstCancelOnly, InstDelisted},
		{InstRestricted, InstActive}, {InstRestricted, InstCancelOnly},
		{InstRestricted, InstSuspended}, {InstRestricted, InstHalted},
		{InstRestricted, InstDelisted},
		{InstSuspended, InstActive}, {InstSuspended, InstHalted},
		{InstSuspended, InstDelisted},
		{InstHalted, InstActive}, {InstHalted, InstSuspended},
		{InstHalted, InstDelisted},
	}
	for _, e := range allowed {
		if !lifecycleTransitions[e[0]][e[1]] {
			t.Fatalf("matrix must admit %s → %s", e[0], e[1])
		}
	}
	denied := [][2]string{
		{InstDraft, InstSuspended}, {InstDraft, InstHalted},
		{InstDraft, InstCancelOnly}, {InstDraft, InstDelisted},
		{InstDelisted, InstActive}, {InstDelisted, InstHalted},
		{InstDelisted, InstDraft}, // terminal — nothing escapes
		{InstSuspended, InstCancelOnly}, {InstSuspended, InstRestricted},
		{InstHalted, InstCancelOnly}, {InstHalted, InstRestricted},
		{InstCancelOnly, InstDraft}, {InstActive, InstDraft},
	}
	for _, e := range denied {
		if lifecycleTransitions[e[0]][e[1]] {
			t.Fatalf("matrix must reject %s → %s", e[0], e[1])
		}
	}
}

func TestLifecycleRoleMatrix(t *testing.T) {
	cases := []struct {
		op    string
		roles []string
	}{
		{LcOpActivate, []string{RoleRiskManager, RoleSuperAdmin}},
		{LcOpRestrict, []string{RoleRiskManager, RoleSuperAdmin}},
		{LcOpCancelOnly, []string{RoleRiskManager, RoleComplianceOfficer, RoleSuperAdmin}},
		{LcOpSuspend, []string{RoleComplianceOfficer, RoleSuperAdmin}},
		{LcOpHalt, []string{RoleRiskManager, RoleSuperAdmin}},
		{LcOpResume, []string{RoleRiskManager, RoleSuperAdmin}},
		{LcOpDelist, []string{RoleSuperAdmin}},
	}
	for _, c := range cases {
		spec, ok := lifecycleOps[c.op]
		if !ok {
			t.Fatalf("op %s missing", c.op)
		}
		for _, r := range c.roles {
			if !spec.roles[r] {
				t.Fatalf("op %s must admit %s", c.op, r)
			}
		}
		// No role outside the set may pass.
		for _, r := range []string{RoleSuperAdmin, RoleRiskManager,
			RoleComplianceOfficer, RoleFinanceOps, RoleSupportAgent,
			RoleReadOnlyAuditor} {
			in := false
			for _, want := range c.roles {
				in = in || want == r
			}
			if !in && spec.roles[r] {
				t.Fatalf("op %s admits unexpected role %s", c.op, r)
			}
		}
	}
	// §7.2 dual-control set.
	for _, op := range []string{LcOpResume, LcOpDelist} {
		if !DualControlledOp(op) {
			t.Fatalf("op %s must be four-eyes", op)
		}
	}
	for _, op := range []string{LcOpActivate, LcOpSuspend, LcOpHalt,
		LcOpRestrict, LcOpCancelOnly} {
		if DualControlledOp(op) {
			t.Fatalf("op %s is single-approver per §7.2", op)
		}
	}
	// Reason mandatory on the destructive controls.
	for _, op := range []string{LcOpSuspend, LcOpHalt, LcOpCancelOnly,
		LcOpRestrict, LcOpDelist} {
		if !lifecycleOps[op].reasonReq {
			t.Fatalf("op %s must demand a reason", op)
		}
	}
}

func TestReopeningAuctionRule(t *testing.T) {
	cases := []struct {
		from    string
		in      TransitionInput
		auction bool
	}{
		{InstHalted, TransitionInput{}, true},
		{InstSuspended, TransitionInput{}, true},
		{InstHalted, TransitionInput{SkipAuction: true}, false},
		{InstSuspended, TransitionInput{SkipAuction: true}, false},
		{InstRestricted, TransitionInput{}, false}, // coherent book resumes direct
		{InstCancelOnly, TransitionInput{}, false},
		{InstRestricted, TransitionInput{Auction: true}, true},
		{InstCancelOnly, TransitionInput{Auction: true}, true},
		{InstRestricted, TransitionInput{SkipAuction: true, Auction: true}, true},
	}
	for _, c := range cases {
		if got := reopeningAuction(c.from, c.in); got != c.auction {
			t.Fatalf("reopeningAuction(%s,%+v)=%v want %v",
				c.from, c.in, got, c.auction)
		}
	}
}

// --- fakes -----------------------------------------------------------------

// fakeFeed is an in-memory InstrumentStatusFeed.
type fakeFeed struct {
	mu        sync.Mutex
	status    map[string]string
	auctions  map[string]string
	deadlines map[int64]int64
	swept     map[int64]bool
	setCalls  int
}

func newFakeFeed() *fakeFeed {
	return &fakeFeed{status: map[string]string{}, auctions: map[string]string{},
		deadlines: map[int64]int64{}, swept: map[int64]bool{}}
}

func (f *fakeFeed) SetStatus(_ context.Context, sym, st string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.setCalls++
	f.status[sym] = st
	return nil
}
func (f *fakeFeed) GetStatus(_ context.Context, sym string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.status[sym], nil
}
func (f *fakeFeed) DelStatus(_ context.Context, sym string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.status, sym)
	return nil
}
func (f *fakeFeed) ScanStatuses(_ context.Context) (map[string]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]string{}
	for k, v := range f.status {
		out[k] = v
	}
	return out, nil
}
func (f *fakeFeed) SetAuctionCall(_ context.Context, sym string, ns int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.auctions[sym] = fmt.Sprintf("CALL:%d", ns)
	return nil
}
func (f *fakeFeed) DelAuction(_ context.Context, sym string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.auctions, sym)
	return nil
}
func (f *fakeFeed) SetSweepDeadline(_ context.Context, id int64, d time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deadlines[id] = d.UnixMilli()
	return nil
}
func (f *fakeFeed) DelSweepKeys(_ context.Context, id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.deadlines, id)
	delete(f.swept, id)
	return nil
}
func (f *fakeFeed) IsSwept(_ context.Context, id int64) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.swept[id], nil
}
func (f *fakeFeed) MarkSwept(_ context.Context, id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.swept[id] = true
	return nil
}

type fakeCanceller struct {
	mu    sync.Mutex
	calls []int64
	err   error
}

func (c *fakeCanceller) CancelInstrumentOrders(_ context.Context, id int64,
	_ string) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return 0, c.err
	}
	c.calls = append(c.calls, id)
	return 3, nil
}

type fakeWS struct {
	mu     sync.Mutex
	events []InstrumentStatusEvent
}

func (w *fakeWS) Publish(_ string, data any) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if ev, ok := data.(InstrumentStatusEvent); ok {
		w.events = append(w.events, ev)
	}
}

// bindRole inserts an ACTIVE STANDARD binding directly (test bootstrap —
// mirrors bootstrapSuperAdmin's deployment-seed shortcut; the Service.Grant
// path's granter-eligibility machinery is exercised by the rbac suite).
func bindRole(t *testing.T, ctx context.Context, pool *pgxpool.Pool,
	userID int64, role string, granter int64) {
	t.Helper()
	if _, err := pool.Exec(ctx, `
		INSERT INTO admin_role_bindings (user_id, role, kind, granter_id, expires_at)
		VALUES ($1, $2, 'STANDARD', $3, now() + interval '30 days')
		ON CONFLICT DO NOTHING`, userID, role, granter); err != nil {
		t.Fatalf("bind %s→%d: %v", role, userID, err)
	}
}

// --- Postgres-gated tests --------------------------------------------------

func lifecycleTestSvc(t *testing.T, pool *pgxpool.Pool,
	feed *fakeFeed, cancels *fakeCanceller, wsp *fakeWS,
	roles AdminRoleResolver) *InstrumentService {
	t.Helper()
	svc, err := NewInstrumentService(InstrumentDeps{
		Pool: pool, Roles: roles, Feed: feed, Canceller: cancels, WS: wsp,
	})
	if err != nil {
		t.Fatalf("NewInstrumentService: %v", err)
	}
	return svc
}

// insertTestInstrument creates a row in the given status with a unique
// symbol; the row is deleted on test cleanup.
func insertTestInstrument(t *testing.T, ctx context.Context, pool *pgxpool.Pool,
	status string) (*Instrument, string) {
	t.Helper()
	symbol := fmt.Sprintf("LC%s%d", status[:1], time.Now().UnixNano()%1_000_000_000)
	symbol = strings.ToUpper(symbol)
	var id int64
	err := pool.QueryRow(ctx, `
		INSERT INTO instruments (symbol, base_currency, quote_currency,
		    instrument_type, tick_size, lot_size, min_order_qty,
		    max_order_qty, settlement_cycle, max_leverage, status)
		VALUES ($1, 'EUR', 'USD', 'SPOT', 0.00001, 1000, 1000,
		    10000000, 1, 30, $2::instrument_status_enum)
		RETURNING id`, symbol, status).Scan(&id)
	if err != nil {
		t.Fatalf("insert test instrument: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM instruments WHERE id = $1`, id)
	})
	return &Instrument{ID: id, Symbol: symbol, Status: status}, symbol
}

// codeOf comes from lp_test.go (coded-error extraction helper).

func TestInstrumentLifecyclePGTransitions(t *testing.T) {
	pool, ctx := pgGate(t)
	feed, cancels, wsp := newFakeFeed(), &fakeCanceller{}, &fakeWS{}
	store := NewStore(pool)

	sa, rm, co := int64(930001), int64(930002), int64(930003)
	seedUsers(t, ctx, pool, sa, rm, co)
	cleanRBACRows(t, ctx, pool, []int64{sa, rm, co})
	bootstrapSuperAdmin(t, ctx, pool, sa)
	bindRole(t, ctx, pool, rm, RoleRiskManager, sa)
	bindRole(t, ctx, pool, co, RoleComplianceOfficer, sa)

	svc := lifecycleTestSvc(t, pool, feed, cancels, wsp, store.RoleResolver())
	inst, _ := insertTestInstrument(t, ctx, pool, InstDraft)

	// Role gate: Compliance Officer cannot activate (RM|SA only).
	if _, err := svc.Transition(ctx, AdminActor{UserID: co}, inst.ID,
		LcOpActivate, TransitionInput{Reason: "x"}); codeOf(t, err) != "UNAUTHORIZED_ROLE" {
		t.Fatalf("CO activate must be UNAUTHORIZED_ROLE, got %v", err)
	}

	// DRAFT → ACTIVE (Risk Manager).
	got, err := svc.Transition(ctx, AdminActor{UserID: rm}, inst.ID,
		LcOpActivate, TransitionInput{Reason: "listing approved"})
	if err != nil {
		t.Fatalf("activate: %v", err)
	}
	if got.Status != InstActive {
		t.Fatalf("status = %s want ACTIVE", got.Status)
	}
	if feed.status[inst.Symbol] != InstActive {
		t.Fatalf("engine feed not published: %v", feed.status)
	}

	// ACTIVE → RESTRICTED → CANCEL_ONLY → SUSPENDED → HALTED — every
	// control state reachable; reason mandatory on each.
	if _, err := svc.Transition(ctx, AdminActor{UserID: rm}, inst.ID,
		LcOpRestrict, TransitionInput{}); codeOf(t, err) != "INVALID_REQUEST" {
		t.Fatalf("restrict without reason must fail INVALID_REQUEST: %v", err)
	}
	for _, step := range []struct {
		op, want string
	}{
		{LcOpRestrict, InstRestricted},
		{LcOpCancelOnly, InstCancelOnly},
		{LcOpSuspend, InstSuspended},
		{LcOpHalt, InstHalted},
	} {
		if _, err := svc.Transition(ctx, AdminActor{UserID: sa}, inst.ID,
			step.op, TransitionInput{Reason: "ops " + step.op}); err != nil {
			t.Fatalf("%s: %v", step.op, err)
		}
		var st string
		if err := pool.QueryRow(ctx,
			`SELECT status::text FROM instruments WHERE id=$1`,
			inst.ID).Scan(&st); err != nil || st != step.want {
			t.Fatalf("%s: status=%s err=%v want %s", step.op, st, err, step.want)
		}
	}

	// Suspend is Compliance Officer+ — a Risk Manager-only binding rejects.
	if _, err := svc.Transition(ctx, AdminActor{UserID: rm}, inst.ID,
		LcOpSuspend, TransitionInput{Reason: "rm try"}); codeOf(t, err) != "UNAUTHORIZED_ROLE" {
		t.Fatalf("RM suspend must be UNAUTHORIZED_ROLE, got %v", err)
	}
	if _, err := svc.Transition(ctx, AdminActor{UserID: co}, inst.ID,
		LcOpSuspend, TransitionInput{Reason: "compliance flag"}); err != nil {
		t.Fatalf("CO suspend: %v", err)
	}

	// Invalid transition: SUSPENDED → CANCEL_ONLY is not a permitted edge.
	if _, err := svc.Transition(ctx, AdminActor{UserID: sa}, inst.ID,
		LcOpCancelOnly, TransitionInput{Reason: "downgrade"}); codeOf(t, err) != "INVALID_LIFECYCLE_TRANSITION" {
		t.Fatalf("SUSPENDED→CANCEL_ONLY must be INVALID_LIFECYCLE_TRANSITION: %v", err)
	}

	// Audit trail: every transition logged an instrument.lifecycle.* row
	// targeting this instrument, in-hash-chain order.
	var n int64
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM admin_audit_log
		 WHERE target_type='instrument' AND target_id=$1
		   AND action LIKE 'instrument.lifecycle.%'`, inst.ID).Scan(&n); err != nil {
		t.Fatalf("audit count: %v", err)
	}
	// activate, restrict, cancel-only, suspend(SA), halt, suspend(CO)
	if n != 6 {
		t.Fatalf("audit rows=%d want 6", n)
	}
	var links int64
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM audit_hash_chain c
		  JOIN admin_audit_log a ON a.id = c.record_id
		 WHERE c.table_name='admin_audit_log' AND a.target_id=$1`,
		inst.ID).Scan(&links); err != nil {
		t.Fatalf("chain count: %v", err)
	}
	if links < 6 {
		t.Fatalf("hash-chain links=%d want ≥6", links)
	}
	if len(wsp.events) == 0 {
		t.Fatal("WS events must be published per transition")
	}
}

func TestInstrumentLifecycleDualControlPG(t *testing.T) {
	pool, ctx := pgGate(t)
	feed := newFakeFeed()
	store := NewStore(pool)

	sa, rm, rm2 := int64(930101), int64(930102), int64(930103)
	seedUsers(t, ctx, pool, sa, rm, rm2)
	cleanRBACRows(t, ctx, pool, []int64{sa, rm, rm2})
	bootstrapSuperAdmin(t, ctx, pool, sa)
	bindRole(t, ctx, pool, rm, RoleRiskManager, sa)
	bindRole(t, ctx, pool, rm2, RoleRiskManager, sa)

	svc := lifecycleTestSvc(t, pool, feed, &fakeCanceller{}, &fakeWS{},
		store.RoleResolver())
	dual := NewDualControlService(pool, store)
	// Mirror cmd/gateway's post-commit hook wiring.
	dual.SetOnExecuted(func(ctx context.Context, req *DualControlRequest) {
		if req.Operation != OpInstrumentResume &&
			req.Operation != OpInstrumentDelist {
			return
		}
		id, err := strconv.ParseInt(req.TargetID, 10, 64)
		if err != nil {
			t.Fatalf("post-commit target: %v", err)
		}
		if err := svc.PublishCommitted(ctx, id); err != nil {
			t.Fatalf("PublishCommitted: %v", err)
		}
	})

	// Wire the executors the same way api.RegisterInstrumentExecutors
	// does (kept local so the admin package test stays self-contained).
	dual.RegisterExecutor(OpInstrumentResume,
		func(ctx context.Context, tx pgx.Tx, req *DualControlRequest) error {
			var p struct {
				InstrumentID int64  `json:"instrument_id"`
				Reason       string `json:"reason"`
				SkipAuction  bool   `json:"skip_auction"`
			}
			if err := json.Unmarshal(req.Payload, &p); err != nil {
				return err
			}
			return svc.TransitionTx(ctx, tx, AdminActor{
				UserID: req.RequestedBy, ApproverID: *req.ApprovedBy,
			}, p.InstrumentID, LcOpResume,
				TransitionInput{Reason: p.Reason, SkipAuction: p.SkipAuction})
		})
	dual.RegisterExecutor(OpInstrumentDelist,
		func(ctx context.Context, tx pgx.Tx, req *DualControlRequest) error {
			var p struct {
				InstrumentID int64  `json:"instrument_id"`
				Reason       string `json:"reason"`
			}
			if err := json.Unmarshal(req.Payload, &p); err != nil {
				return err
			}
			return svc.TransitionTx(ctx, tx, AdminActor{
				UserID: req.RequestedBy, ApproverID: *req.ApprovedBy,
			}, p.InstrumentID, LcOpDelist, TransitionInput{Reason: p.Reason})
		})

	inst, _ := insertTestInstrument(t, ctx, pool, InstSuspended)

	// Direct call on a dual-controlled op must refuse.
	if _, err := svc.Transition(ctx, AdminActor{UserID: rm}, inst.ID,
		LcOpResume, TransitionInput{}); codeOf(t, err) != "DUAL_CONTROL_REQUIRED" {
		t.Fatalf("direct resume must be DUAL_CONTROL_REQUIRED, got %v", err)
	}

	// Maker submits; approver must be a different eligible principal.
	req, err := dual.Submit(ctx, SubmitInput{
		Operation: OpInstrumentResume, TargetType: "instrument",
		TargetID:     strconv.FormatInt(inst.ID, 10),
		RequiredRole: RoleRiskManager, RequestedBy: rm,
		Reason: "resume after review",
		Payload: map[string]any{"instrument_id": inst.ID,
			"reason": "resume after review", "skip_auction": true},
	})
	if err != nil {
		t.Fatalf("submit resume: %v", err)
	}
	if _, err := dual.Approve(ctx, req.ID, rm, "10.0.0.1"); err == nil {
		t.Fatal("self-approval must fail")
	}
	if _, err := dual.Approve(ctx, req.ID, rm2, "10.0.0.2"); err != nil {
		t.Fatalf("second-approver approve: %v", err)
	}
	var st string
	if err := pool.QueryRow(ctx,
		`SELECT status::text FROM instruments WHERE id=$1`,
		inst.ID).Scan(&st); err != nil || st != InstActive {
		t.Fatalf("post-approve status=%s err=%v", st, err)
	}
	// The post-commit hook (PublishCommitted) publishes the feed — the
	// in-tx executor cannot, so this proves the dual path emits the same
	// side effects as a direct transition.
	if feed.status[inst.Symbol] != InstActive {
		t.Fatalf("post-commit feed not published: %v", feed.status)
	}
	// skip_auction=true above → no CALL key; a stale one must be cleared.
	if _, ok := feed.auctions[inst.Symbol]; ok {
		t.Fatalf("skip_auction resume must not arm CALL: %v", feed.auctions)
	}

	// A resume WITHOUT skip_auction arms the reopening CALL key
	// (instrument:auction:{symbol} = CALL:{unix_ns}).
	if _, err := svc.Transition(ctx, AdminActor{UserID: sa}, inst.ID,
		LcOpSuspend, TransitionInput{Reason: "second flag"}); err != nil {
		t.Fatalf("re-suspend: %v", err)
	}
	req2, err := dual.Submit(ctx, SubmitInput{
		Operation: OpInstrumentResume, TargetType: "instrument",
		TargetID:     strconv.FormatInt(inst.ID, 10),
		RequiredRole: RoleRiskManager, RequestedBy: rm2,
		Reason: "auctioned resume",
		Payload: map[string]any{"instrument_id": inst.ID,
			"reason": "auctioned resume", "skip_auction": false},
	})
	if err != nil {
		t.Fatalf("submit resume 2: %v", err)
	}
	if _, err := dual.Approve(ctx, req2.ID, rm, "10.0.0.4"); err != nil {
		t.Fatalf("approve resume 2: %v", err)
	}
	av := feed.auctions[inst.Symbol]
	if !strings.HasPrefix(av, "CALL:") {
		t.Fatalf("auctioned resume must arm CALL key, got %q", av)
	}
	ns, err := strconv.ParseInt(strings.TrimPrefix(av, "CALL:"), 10, 64)
	if err != nil || ns <= time.Now().UnixNano() {
		t.Fatalf("CALL deadline must be a future unix-ns: %q", av)
	}

	// Reconcile remains the drift-repair for anything the hook missed.
	if err := svc.Reconcile(ctx); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if feed.status[inst.Symbol] != InstActive {
		t.Fatalf("reconciler did not converge feed: %v", feed.status)
	}

	// Delist through the queue: maker SA, second SA approver.
	sa2 := int64(930104)
	seedUsers(t, ctx, pool, sa2)
	cleanRBACRows(t, ctx, pool, []int64{sa2})
	bootstrapSuperAdmin(t, ctx, pool, sa2)
	dreq, err := dual.Submit(ctx, SubmitInput{
		Operation: OpInstrumentDelist, TargetType: "instrument",
		TargetID:     strconv.FormatInt(inst.ID, 10),
		RequiredRole: RoleSuperAdmin, RequestedBy: sa,
		Reason:  "end-of-life",
		Payload: map[string]any{"instrument_id": inst.ID, "reason": "end-of-life"},
	})
	if err != nil {
		t.Fatalf("submit delist: %v", err)
	}
	if _, err := dual.Approve(ctx, dreq.ID, sa2, "10.0.0.3"); err != nil {
		t.Fatalf("approve delist: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`SELECT status::text FROM instruments WHERE id=$1`,
		inst.ID).Scan(&st); err != nil || st != InstDelisted {
		t.Fatalf("post-delist status=%s err=%v", st, err)
	}
	// DELISTED is terminal.
	if _, err := svc.Transition(ctx, AdminActor{UserID: sa}, inst.ID,
		LcOpActivate, TransitionInput{Reason: "x"}); codeOf(t, err) != "INVALID_LIFECYCLE_TRANSITION" {
		t.Fatalf("DELISTED→ACTIVE must fail: %v", err)
	}
}

func TestInstrumentLifecycleSuspendSweepPG(t *testing.T) {
	pool, ctx := pgGate(t)
	feed, cancels := newFakeFeed(), &fakeCanceller{}
	store := NewStore(pool)

	sa := int64(930201)
	seedUsers(t, ctx, pool, sa)
	cleanRBACRows(t, ctx, pool, []int64{sa})
	bootstrapSuperAdmin(t, ctx, pool, sa)

	svc := lifecycleTestSvc(t, pool, feed, cancels, &fakeWS{},
		store.RoleResolver())

	// A SUSPENDED instrument whose updated_at is backdated past grace.
	susp, _ := insertTestInstrument(t, ctx, pool, InstSuspended)
	if _, err := pool.Exec(ctx,
		`UPDATE instruments SET updated_at = now() - interval '10 minutes'
		  WHERE id = $1`, susp.ID); err != nil {
		t.Fatalf("backdate: %v", err)
	}
	// A CANCEL_ONLY instrument (Task 15.3.9 — persistent, NO sweep).
	co, _ := insertTestInstrument(t, ctx, pool, InstCancelOnly)
	if _, err := pool.Exec(ctx,
		`UPDATE instruments SET updated_at = now() - interval '1 day'
		  WHERE id = $1`, co.ID); err != nil {
		t.Fatalf("backdate co: %v", err)
	}
	// A SUSPENDED instrument still inside its grace window.
	fresh, _ := insertTestInstrument(t, ctx, pool, InstSuspended)

	if err := svc.Sweep(ctx); err != nil {
		t.Fatalf("sweep 1: %v", err)
	}
	if len(cancels.calls) != 1 || cancels.calls[0] != susp.ID {
		t.Fatalf("sweep calls=%v want exactly [%d]", cancels.calls, susp.ID)
	}
	if !feed.swept[susp.ID] {
		t.Fatal("swept marker not set")
	}
	if feed.deadlines[susp.ID] == 0 || feed.deadlines[fresh.ID] == 0 {
		t.Fatal("sweep deadline keys must be published")
	}
	// Idempotent: a second pass must not re-cancel.
	if err := svc.Sweep(ctx); err != nil {
		t.Fatalf("sweep 2: %v", err)
	}
	if len(cancels.calls) != 1 {
		t.Fatalf("sweep not idempotent: %v", cancels.calls)
	}

	// Resume-after-sweep still works (AC §7.1: resume valid post
	// mass-cancel). Resume is four-eyes — exercise the in-tx executor
	// path directly.
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin resume tx: %v", err)
	}
	if err := svc.TransitionTx(ctx, tx, AdminActor{UserID: sa, ApproverID: sa},
		susp.ID, LcOpResume, TransitionInput{Reason: "reviewed"}); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("resume after suspend: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit resume: %v", err)
	}
	var st string
	if err := pool.QueryRow(ctx,
		`SELECT status::text FROM instruments WHERE id=$1`,
		susp.ID).Scan(&st); err != nil || st != InstActive {
		t.Fatalf("post-resume status=%s err=%v", st, err)
	}
	// SUSPENDED exit clears the sweep bookkeeping (via the reconciler —
	// the in-tx executor cannot publish).
	if err := svc.Reconcile(ctx); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if feed.swept[susp.ID] || feed.deadlines[susp.ID] != 0 {
		t.Fatal("sweep keys must clear on SUSPENDED exit")
	}
	// CANCEL_ONLY still untouched — no timer exists for it.
	if len(cancels.calls) != 1 {
		t.Fatalf("CANCEL_ONLY must never trigger a sweep: %v", cancels.calls)
	}

	// Reconcile: DRAFT carries no key; non-DRAFT rows carry their enum.
	draft, _ := insertTestInstrument(t, ctx, pool, InstDraft)
	feed.status[draft.Symbol] = InstActive // stale garbage
	if err := svc.Reconcile(ctx); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if _, ok := feed.status[draft.Symbol]; ok {
		t.Fatal("DRAFT must not carry a status key")
	}
	// Purge: a Redis key with no PG row is deleted.
	feed.status["GHOST/PAIR"] = InstActive
	if err := svc.Reconcile(ctx); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if _, ok := feed.status["GHOST/PAIR"]; ok {
		t.Fatal("orphan status key must be purged")
	}
}

func TestInstrumentLifecycleUpdatePG(t *testing.T) {
	pool, ctx := pgGate(t)
	feed := newFakeFeed()
	store := NewStore(pool)

	sa := int64(930301)
	seedUsers(t, ctx, pool, sa)
	cleanRBACRows(t, ctx, pool, []int64{sa})
	bootstrapSuperAdmin(t, ctx, pool, sa)
	svc := lifecycleTestSvc(t, pool, feed, &fakeCanceller{}, &fakeWS{},
		store.RoleResolver())

	inst, _ := insertTestInstrument(t, ctx, pool, InstDraft)
	newTick := "0.0001"
	got, err := svc.Update(ctx, AdminActor{UserID: sa}, inst.ID,
		InstrumentUpdate{TickSize: &newTick, Reason: "re-tick"})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if !strings.HasPrefix(got.TickSize, "0.0001") {
		t.Fatalf("tick=%s", got.TickSize)
	}
	// Update on a control-state instrument is rejected.
	if _, err := svc.Transition(ctx, AdminActor{UserID: sa}, inst.ID,
		LcOpActivate, TransitionInput{Reason: "go"}); err != nil {
		t.Fatalf("activate: %v", err)
	}
	if _, err := svc.Transition(ctx, AdminActor{UserID: sa}, inst.ID,
		LcOpHalt, TransitionInput{Reason: "freeze"}); err != nil {
		t.Fatalf("halt: %v", err)
	}
	if _, err := svc.Update(ctx, AdminActor{UserID: sa}, inst.ID,
		InstrumentUpdate{TickSize: &newTick}); codeOf(t, err) != "INVALID_LIFECYCLE_TRANSITION" {
		t.Fatalf("update under HALTED must fail INVALID_LIFECYCLE_TRANSITION: %v", err)
	}
}

func TestInstrumentLifecycleCreateTxPG(t *testing.T) {
	pool, ctx := pgGate(t)
	sa := int64(930401)
	seedUsers(t, ctx, pool, sa)
	svc := lifecycleTestSvc(t, pool, newFakeFeed(), &fakeCanceller{},
		&fakeWS{}, NewStore(pool).RoleResolver())

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	i, err := svc.CreateInTx(ctx, tx, AdminActor{UserID: sa, ApproverID: sa},
		InstrumentCreate{
			Symbol:       "LCNEW" + fmt.Sprint(time.Now().UnixNano()%1_000_000),
			BaseCurrency: "EUR", QuoteCurrency: "GBP",
			InstrumentType: "SPOT", TickSize: "0.00001",
			LotSize: "1000", MinOrderQty: "1000", MaxOrderQty: "5000000",
			MaxLeverage: 30, SettlementCycle: 1, Reason: "new listing",
		})
	if err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("CreateInTx: %v", err)
	}
	if i.Status != InstDraft {
		t.Fatalf("new instrument status=%s want DRAFT", i.Status)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM instruments WHERE id=$1`, i.ID)
	})
	var st string
	if err := pool.QueryRow(ctx, `SELECT status::text FROM instruments
		WHERE id=$1`, i.ID).Scan(&st); err != nil || st != InstDraft {
		t.Fatalf("status=%s err=%v", st, err)
	}
}

// --- Redis-gated tests -----------------------------------------------------

func redisGate(t *testing.T) (*excredis.Client, context.Context) {
	t.Helper()
	if os.Getenv("EXC_REDIS_TEST") != "1" {
		t.Skip("set EXC_REDIS_TEST=1 to run Redis integration tests")
	}
	addr := os.Getenv("EXC_REDIS_TEST_ADDR")
	if addr == "" {
		addr = "127.0.0.1:16379"
	}
	c := excredis.New(addr, os.Getenv("EXC_REDIS_TEST_PASSWORD"), 13)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	if err := c.Ping(ctx); err != nil {
		_ = c.Close()
		t.Skipf("redis unreachable at %s: %v", addr, err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c, ctx
}

func TestInstrumentLifecycleRedisFeed(t *testing.T) {
	c, ctx := redisGate(t)
	feed := RedisStatusFeed{C: c.Client}
	sym := "LCIT" + fmt.Sprint(time.Now().UnixNano()%1_000_000) + "/USD"
	defer func() {
		_ = feed.DelStatus(ctx, sym)
		_ = feed.DelAuction(ctx, sym)
		_ = feed.DelSweepKeys(ctx, 99900001)
	}()

	if err := feed.SetStatus(ctx, sym, InstSuspended); err != nil {
		t.Fatalf("SetStatus: %v", err)
	}
	v, err := feed.GetStatus(ctx, sym)
	if err != nil || v != InstSuspended {
		t.Fatalf("GetStatus=%q err=%v", v, err)
	}
	// Value is the plain enum word, not JSON.
	if strings.Contains(v, "{") || strings.Contains(v, `"`) {
		t.Fatalf("status key must hold the plain enum word: %q", v)
	}

	if err := feed.SetAuctionCall(ctx, sym, 1_700_000_000_000_000_000); err != nil {
		t.Fatalf("SetAuctionCall: %v", err)
	}
	av, err := c.Get(ctx, instrumentAuctionKey(sym)).Result()
	if err != nil {
		t.Fatalf("auction key read: %v", err)
	}
	if !strings.HasPrefix(av, "CALL:") {
		t.Fatalf("auction key value %q must be CALL:{unix_ns}", av)
	}

	deadline := time.Now().Add(SuspendedGrace)
	if err := feed.SetSweepDeadline(ctx, 99900001, deadline); err != nil {
		t.Fatalf("SetSweepDeadline: %v", err)
	}
	swept, err := feed.IsSwept(ctx, 99900001)
	if err != nil || swept {
		t.Fatalf("IsSwept=%v err=%v want false", swept, err)
	}
	if err := feed.MarkSwept(ctx, 99900001); err != nil {
		t.Fatalf("MarkSwept: %v", err)
	}
	swept, err = feed.IsSwept(ctx, 99900001)
	if err != nil || !swept {
		t.Fatalf("IsSwept=%v err=%v want true", swept, err)
	}
	if err := feed.DelSweepKeys(ctx, 99900001); err != nil {
		t.Fatalf("DelSweepKeys: %v", err)
	}
	swept, _ = feed.IsSwept(ctx, 99900001)
	if swept {
		t.Fatal("DelSweepKeys must clear the swept marker")
	}

	m, err := feed.ScanStatuses(ctx)
	if err != nil {
		t.Fatalf("ScanStatuses: %v", err)
	}
	if m[sym] != InstSuspended {
		t.Fatalf("ScanStatuses missing %s: %v", sym, m)
	}
	if err := feed.DelStatus(ctx, sym); err != nil {
		t.Fatalf("DelStatus: %v", err)
	}
}
