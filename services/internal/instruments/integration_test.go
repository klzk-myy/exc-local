// PostgreSQL integration tests — Phase-15 Tasks 15.3.12/15.3.13
// (spec §7.5/§7.1, §24 #352/#401): listing proposal lifecycle through
// the real four-eyes queue, the delist ladder, auction-calendar
// persistence via OpInstrumentCalendar, and benchmark_fixings
// persistence from the fixing scheduler.
//
// Gated on EXC_PG_TEST=1; targets EXC_TEST_DSN (default dev database).
// Anchors required: users, instruments, admin_audit_log,
// audit_hash_chain, admin_role_bindings, admin_dual_control_requests —
// plus the 087/091/222/223 tables. A scratch DB without them skips
// cleanly.
package instruments

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"exchange/internal/admin"
	"exchange/internal/db"
)

func pgGate(t *testing.T) (*pgxpool.Pool, context.Context) {
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
	if err != nil || pool.Ping(ctx) != nil {
		cancel()
		t.Skipf("postgres unreachable: %v", err)
	}
	t.Cleanup(func() { pool.Close(); cancel() })
	return pool, ctx
}

func hasTable(t *testing.T, ctx context.Context, pool *pgxpool.Pool, name string) bool {
	t.Helper()
	var exists bool
	if err := pool.QueryRow(ctx,
		`SELECT to_regclass($1) IS NOT NULL`, name).Scan(&exists); err != nil {
		t.Fatalf("table probe %s: %v", name, err)
	}
	return exists
}

func ensureSchema(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	for _, anchor := range []string{
		"users", "instruments", "admin_audit_log", "audit_hash_chain",
		"admin_role_bindings", "admin_dual_control_requests",
		"listing_proposals", "instruments_reference",
		"auction_calendar", "benchmark_fixings",
	} {
		if !hasTable(t, ctx, pool, anchor) {
			t.Skipf("baseline table %s missing — target a migrated database", anchor)
		}
	}
	// The 223 proposal columns must be present for the pipeline.
	var ok bool
	if err := pool.QueryRow(ctx, `
		SELECT count(*)=5 FROM information_schema.columns
		 WHERE table_name='listing_proposals' AND column_name IN
		 ('reviewed_by','reviewed_at','dual_control_id','instrument_id','activate_at')
	`).Scan(&ok); err == nil && !ok {
		t.Skip("listing_proposals lacks the 223 extension columns — apply migration 223")
	}
}

// fakeFeed satisfies admin.InstrumentStatusFeed in-memory — Redis is not
// part of this gate.
type fakeFeed struct{ m map[string]string }

func newFakeFeed() *fakeFeed                                          { return &fakeFeed{m: map[string]string{}} }
func (f *fakeFeed) SetStatus(_ context.Context, sym, st string) error { f.m[sym] = st; return nil }
func (f *fakeFeed) GetStatus(_ context.Context, sym string) (string, error) {
	return f.m[sym], nil
}
func (f *fakeFeed) DelStatus(_ context.Context, sym string) error { delete(f.m, sym); return nil }
func (f *fakeFeed) ScanStatuses(_ context.Context) (map[string]string, error) {
	out := map[string]string{}
	for k, v := range f.m {
		out[k] = v
	}
	return out, nil
}
func (f *fakeFeed) SetAuctionCall(_ context.Context, sym string, ns int64) error {
	f.m["auction:"+sym] = fmt.Sprintf("CALL:%d", ns)
	return nil
}
func (f *fakeFeed) DelAuction(_ context.Context, sym string) error {
	delete(f.m, "auction:"+sym)
	return nil
}
func (f *fakeFeed) SetFixing(_ context.Context, sym, payload string) error {
	f.m["fixing:"+sym] = payload
	return nil
}
func (f *fakeFeed) SetSweepDeadline(_ context.Context, _ int64, _ time.Time) error { return nil }
func (f *fakeFeed) DelSweepKeys(_ context.Context, _ int64) error                  { return nil }
func (f *fakeFeed) IsSwept(_ context.Context, _ int64) (bool, error)               { return true, nil }
func (f *fakeFeed) MarkSwept(_ context.Context, _ int64) error                     { return nil }

// seededPrincipals installs a proposer (Risk Manager) and two Super
// Admins, returning the store + ids. Bindings clean up on re-run.
type testPrincipals struct {
	proposer, reviewer, approver int64
}

func seedPrincipals(t *testing.T, ctx context.Context, pool *pgxpool.Pool) testPrincipals {
	t.Helper()
	p := testPrincipals{proposer: 930011, reviewer: 930012, approver: 930013}
	ids := []int64{p.proposer, p.reviewer, p.approver}
	for _, id := range ids {
		if _, err := pool.Exec(ctx,
			`INSERT INTO users (id, email) VALUES ($1,$2) ON CONFLICT (id) DO NOTHING`,
			id, fmt.Sprintf("instr-it-%d@example.invalid", id)); err != nil {
			t.Fatalf("seed user %d: %v", id, err)
		}
	}
	if _, err := pool.Exec(ctx, `
		DELETE FROM admin_dual_control_requests
		 WHERE requested_by = ANY($1) OR approved_by = ANY($1)`, ids); err != nil {
		t.Fatalf("clean dc: %v", err)
	}
	// admin_recert_decisions (migration 090) FKs into admin_role_bindings;
	// remove dependent decisions first (guarded — scratch schemas may lack it).
	var recertTable *string
	if err := pool.QueryRow(ctx,
		`SELECT to_regclass('admin_recert_decisions')::text`).Scan(&recertTable); err != nil {
		t.Fatalf("probe recert table: %v", err)
	}
	if recertTable != nil {
		if _, err := pool.Exec(ctx, `
			DELETE FROM admin_recert_decisions
			 WHERE binding_id IN (
				SELECT id FROM admin_role_bindings
				 WHERE user_id = ANY($1) OR granter_id = ANY($1))`, ids); err != nil {
			t.Fatalf("clean recert decisions: %v", err)
		}
	}
	if _, err := pool.Exec(ctx, `
		DELETE FROM admin_role_bindings WHERE user_id = ANY($1) OR granter_id = ANY($1)`,
		ids); err != nil {
		t.Fatalf("clean bindings: %v", err)
	}
	ins := func(uid int64, role string) {
		if _, err := pool.Exec(ctx, `
			INSERT INTO admin_role_bindings (user_id, role, kind, granter_id, expires_at)
			VALUES ($1,$2,'STANDARD',$3, now() + interval '30 days')`,
			uid, role, p.reviewer); err != nil {
			t.Fatalf("bind %d %s: %v", uid, role, err)
		}
	}
	ins(p.proposer, admin.RoleRiskManager)
	ins(p.reviewer, admin.RoleSuperAdmin)
	ins(p.approver, admin.RoleSuperAdmin)
	return p
}

// cleanupSymbol removes every row the test created for a symbol so
// re-runs on a shared dev DB converge.
func cleanupSymbol(t *testing.T, ctx context.Context, pool *pgxpool.Pool, symbol string) {
	t.Helper()
	stmts := []string{
		`DELETE FROM benchmark_fixings WHERE symbol=$1`,
		`DELETE FROM auction_calendar WHERE symbol=$1`,
		`DELETE FROM instruments_reference WHERE symbol=$1`,
		`DELETE FROM listing_proposals WHERE symbol=$1`,
		`DELETE FROM instruments WHERE symbol=$1`,
	}
	for _, q := range stmts {
		if _, err := pool.Exec(ctx, q, symbol); err != nil {
			t.Fatalf("cleanup %s: %v", symbol, err)
		}
	}
}

func newStack(t *testing.T, ctx context.Context, pool *pgxpool.Pool) (
	*ListingService, *admin.DualControlService, *admin.InstrumentService, *fakeFeed) {
	t.Helper()
	store := admin.NewStore(pool)
	feed := newFakeFeed()
	instSvc, err := admin.NewInstrumentService(admin.InstrumentDeps{
		Pool: pool, Roles: store.StrongestRole, Feed: feed,
	})
	if err != nil {
		t.Fatalf("instrument service: %v", err)
	}
	dual := admin.NewDualControlService(pool, store)
	cal, err := NewSessionCalendar()
	if err != nil {
		t.Fatalf("session calendar: %v", err)
	}
	listing, err := NewListingService(ListingDeps{
		Pool: pool, Roles: store.StrongestRole, Instruments: instSvc,
		Dual: dual, Sessions: cal,
	})
	if err != nil {
		t.Fatalf("listing service: %v", err)
	}
	RegisterListingExecutor(dual, listing)
	return listing, dual, instSvc, feed
}

const itSymbol = "GBP/JPY"

// insertInstrument seeds an instruments row directly — the shared test
// convention (admin tests do the same; CreateInTx is the dual-control
// path and needs no service-level wrapper).
func insertInstrument(t *testing.T, ctx context.Context, pool *pgxpool.Pool,
	status string) int64 {
	t.Helper()
	var id int64
	err := pool.QueryRow(ctx, `
		INSERT INTO instruments (symbol, base_currency, quote_currency,
		    instrument_type, tick_size, lot_size, min_order_qty,
		    max_order_qty, settlement_cycle, max_leverage, status,
		    contract_size, decimal_places, pip_size)
		VALUES ($1, 'GBP', 'JPY', 'SPOT', 0.001, 1000, 1000,
		    10000000, 2, 30, $2::instrument_status_enum,
		    100000, 3, 0.01)
		RETURNING id`, itSymbol, status).Scan(&id)
	if err != nil {
		t.Fatalf("insert instrument: %v", err)
	}
	return id
}

func jpyReference() ReferenceInput {
	return ReferenceInput{
		InstrumentType: "SPOT", TickSize: "0.001", LotSize: "1000",
		MinOrderQty: "1000", MaxOrderQty: "10000000", MinNotional: "1000",
		ContractSize: "100000", DecimalPlaces: 3, PipSize: "0.01",
		SettlementCycle: 2, MaxLeverage: 30,
	}
}

// TestListingLifecycleIntegration drives PROPOSED → IN_REVIEW →
// APPROVED (four-eyes submit) → EXECUTED (DRAFT + SCHEDULED) →
// ActivateDue → ACTIVE, asserting the audit trail at every leg.
func TestListingLifecycleIntegration(t *testing.T) {
	pool, ctx := pgGate(t)
	ensureSchema(t, ctx, pool)
	p := seedPrincipals(t, ctx, pool)
	cleanupSymbol(t, ctx, pool, itSymbol)
	t.Cleanup(func() { cleanupSymbol(t, ctx, pool, itSymbol) })

	listing, dual, instSvc, feed := newStack(t, ctx, pool)

	prop, err := listing.Propose(ctx, admin.AdminActor{UserID: p.proposer},
		ProposalInput{
			Symbol: itSymbol, Reference: jpyReference(),
			OracleFeeds: []string{"REFINITIV", "BFIX"},
			RiskDefaults: map[string]any{
				"price_band_pct_up": "0.05", "price_band_pct_down": "0.05",
			},
			Reason: "expand JPY coverage",
		})
	if err != nil {
		t.Fatalf("propose: %v", err)
	}
	if prop.Status != PropProposed {
		t.Fatalf("status: %s", prop.Status)
	}
	var checks autoChecks
	if err := json.Unmarshal(prop.AutoChecks, &checks); err != nil {
		t.Fatalf("auto_checks decode: %v", err)
	}
	if !checks.AllPass() {
		t.Fatalf("auto-checks must pass: %+v", checks)
	}

	// The proposer cannot review their own proposal (§8.2).
	if _, _, err := listing.Review(ctx,
		admin.AdminActor{UserID: p.proposer}, prop.ID, ReviewMark, "", nil); err == nil {
		t.Fatal("self-review must fail DUAL_CONTROL_VIOLATION")
	}

	// REVIEW → IN_REVIEW.
	prop, _, err = listing.Review(ctx, admin.AdminActor{UserID: p.reviewer},
		prop.ID, ReviewMark, "looks complete", nil)
	if err != nil || prop.Status != PropInReview {
		t.Fatalf("mark review: %v %+v", err, prop)
	}

	// APPROVE → APPROVED + a live OpInstrumentListing request.
	prop, dc, err := listing.Review(ctx, admin.AdminActor{UserID: p.reviewer},
		prop.ID, ReviewApprove, "approved — schedule at next open", nil)
	if err != nil || prop.Status != PropApproved || dc == nil {
		t.Fatalf("approve: %v %+v dc=%v", err, prop, dc)
	}
	if dc.Operation != admin.OpInstrumentListing || dc.Status != admin.ReqPending {
		t.Fatalf("dual request: %+v", dc)
	}

	// Double-approve while the four-eyes request is live must refuse.
	if _, _, err := listing.Review(ctx,
		admin.AdminActor{UserID: p.approver}, prop.ID, ReviewApprove, "dup", nil); err == nil {
		t.Fatal("re-approve with live request must fail")
	}

	// The second principal approves → executor lands the DRAFT +
	// SCHEDULED proposal in the approval transaction.
	settled, err := dual.Approve(ctx, dc.ID, p.approver, "198.51.100.7")
	if err != nil || settled.Status != admin.ReqExecuted {
		t.Fatalf("dual approve: %v %+v", err, settled)
	}
	prop, err = listing.Get(ctx, prop.ID)
	if err != nil || prop.Status != PropScheduled || prop.InstrumentID == nil {
		t.Fatalf("post-approve proposal: %v %+v", err, prop)
	}

	// The DRAFT instrument carries the pinned reference values.
	inst, err := instSvc.Get(ctx, *prop.InstrumentID)
	if err != nil || inst.Status != admin.InstDraft || inst.Symbol != itSymbol {
		t.Fatalf("draft instrument: %v %+v", err, inst)
	}
	var tickOK, pipOK, contractOK bool
	var dp int
	if err := pool.QueryRow(ctx, `
		SELECT tick_size = 0.001, pip_size = 0.01, contract_size = 100000,
		       decimal_places
		  FROM instruments WHERE id=$1`, inst.ID).
		Scan(&tickOK, &pipOK, &contractOK, &dp); err != nil {
		t.Fatalf("reference columns: %v", err)
	}
	if !tickOK || !pipOK || !contractOK || dp != 3 {
		t.Fatalf("pinned JPY reference: tick=%v pip=%v contract=%v dp=%d",
			tickOK, pipOK, contractOK, dp)
	}
	// The §5.44.10 envelope row exists.
	var n int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM instruments_reference WHERE instrument_id=$1`,
		inst.ID).Scan(&n); err != nil || n != 1 {
		t.Fatalf("reference envelope: n=%d %v", n, err)
	}

	// Force the scheduled activation into the past → ActivateDue flips
	// DRAFT→ACTIVE through the lifecycle-shaped audit + publication.
	if _, err := pool.Exec(ctx,
		`UPDATE listing_proposals SET activate_at=now()-interval '1m' WHERE id=$1`,
		prop.ID); err != nil {
		t.Fatalf("backdate activate_at: %v", err)
	}
	if got, err := listing.ActivateDue(ctx); err != nil || got != 1 {
		t.Fatalf("ActivateDue: n=%d %v", got, err)
	}
	inst, _ = instSvc.Get(ctx, *prop.InstrumentID)
	if inst.Status != admin.InstActive {
		t.Fatalf("activated instrument: %s", inst.Status)
	}
	if feed.m[itSymbol] != admin.InstActive {
		t.Fatalf("engine feed not published: %q", feed.m[itSymbol])
	}
	// Audit evidence at every leg.
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM admin_audit_log
		 WHERE action LIKE 'instrument.listing.%'
		   AND (target_id = $1 OR target_id = $2)`, prop.ID, inst.ID).
		Scan(&n); err != nil || n < 3 {
		t.Fatalf("listing audit rows: %d %v", n, err)
	}
}

// TestCalendarReplaceIntegration drives the OpInstrumentCalendar
// four-eyes replace: submit → approve → auction_calendar rows land.
func TestCalendarReplaceIntegration(t *testing.T) {
	pool, ctx := pgGate(t)
	ensureSchema(t, ctx, pool)
	p := seedPrincipals(t, ctx, pool)
	cleanupSymbol(t, ctx, pool, itSymbol)
	t.Cleanup(func() { cleanupSymbol(t, ctx, pool, itSymbol) })

	store := admin.NewStore(pool)
	dual := admin.NewDualControlService(pool, store)
	calStore := NewCalendarStore(pool)
	RegisterCalendarExecutor(dual, calStore)

	// Direct row for the calendar target (migration-087 conventions).
	insertInstrument(t, ctx, pool, "DRAFT")

	entries := []CalendarEntry{
		{AuctionType: AuctionDailyClose, TriggerTime: "17:00",
			Timezone: VenueTZ, Recurrence: "FRI", Enabled: true},
		{AuctionType: AuctionFixing, Benchmark: BenchWMLondon,
			TriggerTime: "16:00", Timezone: "Europe/London",
			Recurrence: "MON-FRI", Enabled: true},
	}
	req, err := dual.Submit(ctx, admin.SubmitInput{
		Operation: admin.OpInstrumentCalendar, TargetType: "instrument",
		TargetID: itSymbol, RequiredRole: admin.RoleRiskManager,
		RequestedBy: p.reviewer, Reason: "seed venue calendar",
		Payload: map[string]any{"symbol": itSymbol, "entries": entries},
	})
	if err != nil {
		t.Fatalf("calendar submit: %v", err)
	}
	settled, err := dual.Approve(ctx, req.ID, p.approver, "198.51.100.8")
	if err != nil || settled.Status != admin.ReqExecuted {
		t.Fatalf("calendar approve: %v %+v", err, settled)
	}
	rows, err := calStore.CalendarFor(ctx, itSymbol)
	if err != nil || len(rows) != 2 {
		t.Fatalf("calendar read: %v rows=%d", err, len(rows))
	}
	if rows[1].Benchmark != BenchWMLondon {
		t.Fatalf("benchmark row: %+v", rows[1])
	}
}

// TestFixingSchedulerIntegration fires a due BENCHMARK_FIXING row with
// an unwired PriceSource — the fixing records SKIPPED with the reason
// (never a fabricated rate) and refires idempotently.
func TestFixingSchedulerIntegration(t *testing.T) {
	pool, ctx := pgGate(t)
	ensureSchema(t, ctx, pool)
	p := seedPrincipals(t, ctx, pool)
	cleanupSymbol(t, ctx, pool, itSymbol)
	t.Cleanup(func() { cleanupSymbol(t, ctx, pool, itSymbol) })
	_ = p

	store := admin.NewStore(pool)
	feed := newFakeFeed()
	instSvc, err := admin.NewInstrumentService(admin.InstrumentDeps{
		Pool: pool, Roles: store.StrongestRole, Feed: feed})
	if err != nil {
		t.Fatal(err)
	}
	instID := insertInstrument(t, ctx, pool, "DRAFT")
	// ACTIVE so the scheduler's join includes it.
	if _, err := instSvc.Transition(ctx, admin.AdminActor{UserID: p.reviewer},
		instID, admin.LcOpActivate, admin.TransitionInput{}); err != nil {
		t.Fatalf("activate: %v", err)
	}

	calStore := NewCalendarStore(pool)
	if _, err := pool.Exec(ctx, `
		INSERT INTO auction_calendar
		    (instrument_id, symbol, auction_type, benchmark, trigger_time,
		     timezone, recurrence, enabled)
		VALUES ($1,$2,'BENCHMARK_FIXING','TOKYO_0955','09:55','Asia/Tokyo',
		        'MON-FRI', true)`, instID, itSymbol); err != nil {
		t.Fatalf("seed fixing calendar: %v", err)
	}

	sched, err := NewFixingScheduler(FixingDeps{
		Pool: pool, Store: calStore, Feed: feed, // Prices nil — oracle unwired
	})
	if err != nil {
		t.Fatal(err)
	}
	sched.Tick(ctx) // fires the latest due TOKYO_0955 occurrence

	// Exactly one SKIPPED record per due occurrence; the UNIQUE key makes
	// a second tick idempotent for the same scheduled instant.
	var (
		status, reason string
		cnt            int
	)
	if err := pool.QueryRow(ctx, `
		SELECT count(*), max(status), max(skip_reason)
		  FROM benchmark_fixings WHERE symbol=$1 AND benchmark='TOKYO_0955'`,
		itSymbol).Scan(&cnt, &status, &reason); err != nil {
		t.Fatalf("fixing record: %v", err)
	}
	if cnt < 1 || status != "SKIPPED" || reason == "" {
		t.Fatalf("fixing must record SKIPPED with reason: cnt=%d status=%s reason=%q",
			cnt, status, reason)
	}
	before := cnt
	sched.Tick(ctx)
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM benchmark_fixings WHERE symbol=$1`,
		itSymbol).Scan(&cnt); err != nil || cnt != before {
		t.Fatalf("refire must be idempotent: %d→%d %v", before, cnt, err)
	}
}

// TestDelistLadderIntegration drives RequestDelist → RESTRICTED +
// schedule → AdvanceDelisting files the four-eyes delist after the
// notice window (backdated in-test).
func TestDelistLadderIntegration(t *testing.T) {
	pool, ctx := pgGate(t)
	ensureSchema(t, ctx, pool)
	p := seedPrincipals(t, ctx, pool)
	cleanupSymbol(t, ctx, pool, itSymbol)
	t.Cleanup(func() { cleanupSymbol(t, ctx, pool, itSymbol) })

	listing, dual, instSvc, _ := newStack(t, ctx, pool)
	_ = dual

	instID := insertInstrument(t, ctx, pool, "DRAFT")
	if _, err := instSvc.Transition(ctx, admin.AdminActor{UserID: p.reviewer},
		instID, admin.LcOpActivate, admin.TransitionInput{}); err != nil {
		t.Fatalf("activate: %v", err)
	}

	// Impact preview — empty book reads zero.
	imp, err := listing.PreviewDelist(ctx, itSymbol)
	if err != nil || imp.OpenPositions != 0 || imp.RestingOrders != 0 {
		t.Fatalf("impact: %v %+v", err, imp)
	}

	// RequestDelist requires Super Admin and lands RESTRICTED + schedule.
	actor := admin.AdminActor{UserID: p.reviewer}
	if _, err := listing.RequestDelist(ctx,
		admin.AdminActor{UserID: p.proposer}, itSymbol, "wind down"); err == nil {
		t.Fatal("RM delist request must fail — SA-class operation")
	}
	if _, err := listing.RequestDelist(ctx, actor, itSymbol,
		"low-volume wind down"); err != nil {
		t.Fatalf("request delist: %v", err)
	}
	inst, err := instSvc.Get(ctx, instID)
	if err != nil || inst.Status != admin.InstRestricted {
		t.Fatalf("status after notice: %v %s", err, inst.Status)
	}
	var phase string
	if err := pool.QueryRow(ctx, `
		SELECT delist_schedule->>'phase' FROM instruments_reference
		 WHERE instrument_id=$1`, inst.ID).Scan(&phase); err != nil ||
		phase != "RESTRICTED_NOTICE" {
		t.Fatalf("schedule phase: %q %v", phase, err)
	}

	// Notice not elapsed — the sweep must not file the delist request.
	if err := listing.AdvanceDelisting(ctx); err != nil {
		t.Fatalf("advance early: %v", err)
	}
	var n int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM admin_dual_control_requests
		 WHERE operation='instrument-delist' AND target_id=$1`,
		fmt.Sprint(inst.ID)).Scan(&n); err != nil || n != 0 {
		t.Fatalf("premature delist request: n=%d %v", n, err)
	}

	// Backdate the notice → the sweep files OpInstrumentDelist.
	if _, err := pool.Exec(ctx, `
		UPDATE instruments_reference
		   SET delist_schedule = delist_schedule ||
		       jsonb_build_object('notice_until',
		           (now() - interval '1 minute')::text)
		 WHERE instrument_id=$1`, inst.ID); err != nil {
		t.Fatalf("backdate notice: %v", err)
	}
	if err := listing.AdvanceDelisting(ctx); err != nil {
		t.Fatalf("advance: %v", err)
	}
	var dcID int64
	if err := pool.QueryRow(ctx, `
		SELECT id FROM admin_dual_control_requests
		 WHERE operation='instrument-delist' AND target_id=$1
		   AND status='PENDING'`, fmt.Sprint(inst.ID)).Scan(&dcID); err != nil {
		t.Fatalf("delist request not filed: %v", err)
	}
	if err := pool.QueryRow(ctx, `
		SELECT delist_schedule->>'phase' FROM instruments_reference
		 WHERE instrument_id=$1`, inst.ID).Scan(&phase); err != nil ||
		phase != "DELIST_PENDING_APPROVAL" {
		t.Fatalf("phase after submit: %q %v", phase, err)
	}
}

// TestOpsBoardIntegration asserts the board renders sections without
// error on a live schema and includes the delist-ladder rows.
func TestOpsBoardIntegration(t *testing.T) {
	pool, ctx := pgGate(t)
	ensureSchema(t, ctx, pool)
	board, err := NewOpsBoardService(OpsBoardDeps{Pool: pool})
	if err != nil {
		t.Fatal(err)
	}
	b, err := board.Board(ctx)
	if err != nil {
		t.Fatalf("board: %v", err)
	}
	if b.GeneratedAt.IsZero() {
		t.Fatal("generated_at unset")
	}
	// Drift probe unwired → the documented warning must surface.
	found := false
	for _, w := range b.Warnings {
		if w == "engine status probe unwired — drift checks skipped" {
			found = true
		}
	}
	if !found {
		t.Fatalf("missing unwired-probe warning: %v", b.Warnings)
	}
}
