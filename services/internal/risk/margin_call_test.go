// margin_call_test.go — coverage for margin_call.go + margin_call_store.go
// (Phase-19 Task 19.3.3 item 1; spec §13.3 deposit-window lifecycle,
// §24 #93 class).
//
// Unit legs cover the constructor's fail-closed dep checks, threshold
// tier resolution, and the no-level no-op. Redis-gated legs cover
// stop-out precedence, recovery, the expiry sweeper, and the
// order-entry block seam. PG-gated legs cover PgMarginCallStore
// round-trips; one dual-gated leg drives the full open→restore→stop-out
// lifecycle over real Postgres + Redis.
//
//	EXC_PG_TEST=1    go test ./internal/risk/ -run 'TestPgMarginCall' -v
//	EXC_REDIS_TEST=1 go test ./internal/risk/ -run 'TestRedisMarginCall' -v
package risk

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	excredis "exchange/internal/redis"
)

// ---------------------------------------------------------------------------
// Fakes + helpers
// ---------------------------------------------------------------------------

type mcResolveCall struct {
	acct    int64
	outcome string
	at      time.Time
}
type mcStatusCall struct {
	acct   int64
	status string
}
type mcAuditCall struct {
	acct  int64
	actor int64
}

// marginCallStoreFake is an in-memory MarginCallStore for the service
// legs; the tx-scoped methods record against the shared log (the tx is
// owned by the caller in production and unused by the fake).
type marginCallStoreFake struct {
	cat      string
	nbp      bool
	catErr   error
	override *MarginCallThresholds
	ovErr    error
	open     map[int64]*MarginCallEvent
	expired  []MarginCallEvent
	nextID   int64
	inserted []*MarginCallEvent
	resolved []mcResolveCall
	statuses []mcStatusCall
	audits   []mcAuditCall
	userIDs  map[int64]int64
	userErr  error
}

func (f *marginCallStoreFake) AccountCategory(context.Context, int64) (string, bool, error) {
	if f.catErr != nil {
		return "", false, f.catErr
	}
	c := f.cat
	if c == "" {
		c = CategoryRetail
	}
	return c, f.nbp, nil
}
func (f *marginCallStoreFake) MarginThresholdOverride(context.Context, int64) (*MarginCallThresholds, error) {
	return f.override, f.ovErr
}
func (f *marginCallStoreFake) OpenMarginCall(_ context.Context, acct int64) (*MarginCallEvent, error) {
	if f.open == nil {
		return nil, nil
	}
	return f.open[acct], nil
}
func (f *marginCallStoreFake) InsertMarginCall(_ context.Context, _ pgx.Tx, ev *MarginCallEvent) (int64, error) {
	f.nextID++
	ev.ID = f.nextID
	f.inserted = append(f.inserted, ev)
	return ev.ID, nil
}
func (f *marginCallStoreFake) ResolveMarginCall(_ context.Context, acct int64, outcome string, at time.Time) error {
	f.resolved = append(f.resolved, mcResolveCall{acct, outcome, at})
	if f.open != nil {
		if ev := f.open[acct]; ev != nil {
			ev.Status = outcome
			ev.ResolvedAt = &at
		}
	}
	return nil
}
func (f *marginCallStoreFake) ExpiredOpenMarginCalls(context.Context, time.Time) ([]MarginCallEvent, error) {
	return append([]MarginCallEvent(nil), f.expired...), nil
}
func (f *marginCallStoreFake) SetMarginAccountStatus(_ context.Context, acct int64, status string) error {
	f.statuses = append(f.statuses, mcStatusCall{acct, status})
	return nil
}
func (f *marginCallStoreFake) SetMarginAccountStatusTx(_ context.Context, _ pgx.Tx, acct int64, status string) error {
	f.statuses = append(f.statuses, mcStatusCall{acct, status})
	return nil
}
func (f *marginCallStoreFake) AuditMarginCallRelease(_ context.Context, acct, actor int64) error {
	f.audits = append(f.audits, mcAuditCall{acct, actor})
	return nil
}
func (f *marginCallStoreFake) AccountUserID(_ context.Context, acct int64) (int64, error) {
	if f.userErr != nil {
		return 0, f.userErr
	}
	return f.userIDs[acct], nil
}

var _ MarginCallStore = (*marginCallStoreFake)(nil)

// mcLazyPool returns a never-dialed pool — pgxpool.New is lazy, so a
// non-nil handle satisfies the constructor without a live database.
func mcLazyPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), liqStoreDSN())
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// mcTestService builds a MarginCallService over a lazy pool and an
// unreachable (but non-nil) Redis — safe for the constructor,
// thresholdsFor and no-level Evaluate legs that never touch either.
func mcTestService(t *testing.T, store MarginCallStore, levels MarginLevelReader) *MarginCallService {
	t.Helper()
	rdb := excredis.New("127.0.0.1:1", "", 14)
	q, err := NewLiquidationQueue(rdb, nil)
	if err != nil {
		t.Fatalf("queue: %v", err)
	}
	svc, err := NewMarginCallService(MarginCallDeps{
		Pool: mcLazyPool(t), Redis: rdb, Store: store, Levels: levels, Queue: q})
	if err != nil {
		t.Fatalf("service: %v", err)
	}
	return svc
}

// ---------------------------------------------------------------------------
// Unit — constructor + threshold resolution + no-level no-op
// ---------------------------------------------------------------------------

func TestMarginCallServiceNilDeps(t *testing.T) {
	pool := mcLazyPool(t)
	rdb := excredis.New("127.0.0.1:1", "", 14)
	store := &marginCallStoreFake{}
	levels := staticLevels{}
	queue, err := NewLiquidationQueue(rdb, nil)
	if err != nil {
		t.Fatal(err)
	}
	full := MarginCallDeps{Pool: pool, Redis: rdb, Store: store, Levels: levels, Queue: queue}
	for name, mut := range map[string]func(*MarginCallDeps){
		"nil pool":   func(d *MarginCallDeps) { d.Pool = nil },
		"nil redis":  func(d *MarginCallDeps) { d.Redis = nil },
		"nil store":  func(d *MarginCallDeps) { d.Store = nil },
		"nil levels": func(d *MarginCallDeps) { d.Levels = nil },
		"nil queue":  func(d *MarginCallDeps) { d.Queue = nil },
	} {
		d := full
		mut(&d)
		if _, err := NewMarginCallService(d); err == nil {
			t.Fatalf("%s must fail construction (fail-closed)", name)
		}
	}
	if _, err := NewMarginCallService(full); err != nil {
		t.Fatalf("full deps must build: %v", err)
	}
}

func TestMarginCallThresholdsResolution(t *testing.T) {
	ctx := context.Background()
	f := &marginCallStoreFake{}
	svc := mcTestService(t, f, staticLevels{})

	// Per-account override (migration 235) wins outright.
	f.override = &MarginCallThresholds{
		MarginCallPct: d("150"), StopOutPct: d("90"), Source: "OVERRIDE"}
	th := svc.thresholdsFor(ctx, 7)
	if !th.MarginCallPct.Equal(d("150")) || !th.StopOutPct.Equal(d("90")) || th.Source != "OVERRIDE" {
		t.Fatalf("override: %+v", th)
	}
	// Override read failure → fail closed to the strictest (retail) pair.
	f.override, f.ovErr = nil, fmt.Errorf("pg down")
	th = svc.thresholdsFor(ctx, 7)
	if !th.MarginCallPct.Equal(MarginCallThresholdPct) ||
		!th.StopOutPct.Equal(StopOutRetailPct) || th.Source != "RETAIL" {
		t.Fatalf("override-error fallback: %+v", th)
	}
	// Category tiers: professional 80/30, ECP 80/100, retail 111.1/50.
	f.ovErr = nil
	f.cat = CategoryProfessional
	th = svc.thresholdsFor(ctx, 7)
	if !th.MarginCallPct.Equal(d("80")) || !th.StopOutPct.Equal(d("30")) || th.Source != "PROFESSIONAL" {
		t.Fatalf("professional: %+v", th)
	}
	f.cat = CategoryEligible
	th = svc.thresholdsFor(ctx, 7)
	if !th.MarginCallPct.Equal(d("80")) || !th.StopOutPct.Equal(d("100")) || th.Source != "ECP" {
		t.Fatalf("ecp: %+v", th)
	}
	f.cat = CategoryRetail
	th = svc.thresholdsFor(ctx, 7)
	if !th.MarginCallPct.Equal(d("111.1")) || !th.StopOutPct.Equal(d("50")) || th.Source != "RETAIL" {
		t.Fatalf("retail: %+v", th)
	}
	// Category read failure → retail defaults, no panic.
	f.catErr = fmt.Errorf("pg down")
	th = svc.thresholdsFor(ctx, 7)
	if th.Source != "RETAIL" || !th.MarginCallPct.Equal(MarginCallThresholdPct) {
		t.Fatalf("category-error fallback: %+v", th)
	}
}

func TestMarginCallEvaluateNoLevel(t *testing.T) {
	store := &marginCallStoreFake{}
	svc := mcTestService(t, store, staticLevels{})
	// No computed level → honest no-op: nothing resolved, no status write.
	if err := svc.Evaluate(context.Background(), 7); err != nil {
		t.Fatalf("no-level evaluate: %v", err)
	}
	if len(store.resolved) != 0 || len(store.statuses) != 0 || len(store.inserted) != 0 {
		t.Fatalf("no-level eval must be a no-op: %+v", store)
	}
}

func TestMarginCallAdminReleaseRequiresActor(t *testing.T) {
	svc := mcTestService(t, &marginCallStoreFake{}, staticLevels{})
	if err := svc.AdminRelease(context.Background(), 7, 0); err == nil {
		t.Fatal("actor 0 must be rejected")
	} else {
		requireCode(t, err, "UNAUTHORIZED_ROLE")
	}
}

// ---------------------------------------------------------------------------
// Redis-gated legs (EXC_REDIS_TEST=1, db 14)
// ---------------------------------------------------------------------------

// mcRedis returns the test client and drains the keys this file uses.
func mcRedis(t *testing.T, accts ...int64) (*excredis.Client, *LiquidationQueue) {
	t.Helper()
	rdb := adlTestRedis(t)
	ctx := context.Background()
	keys := []string{LiquidationQueueKey}
	for _, a := range accts {
		keys = append(keys, MarginCallKey(a), MarginCallBlockKey(a),
			LiquidationDedupKey(a), MarginLevelKey(a))
	}
	if err := rdb.Del(ctx, keys...).Err(); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	q, err := NewLiquidationQueue(rdb, nil)
	if err != nil {
		t.Fatal(err)
	}
	return rdb, q
}

func TestRedisMarginCallStopOutSupersedesWindow(t *testing.T) {
	rdb, q := mcRedis(t, 7)
	ctx := context.Background()
	// Open episode in flight (row + window key) — stop-out voids it.
	store := &marginCallStoreFake{
		open: map[int64]*MarginCallEvent{
			7: {ID: 11, AccountID: 7, MarginLevelPct: d("105"), ThresholdPct: d("111.1"),
				Status: MarginCallOpen, NotifiedAt: time.Now(),
				ExpiresAt: time.Now().Add(10 * time.Minute)},
		},
	}
	if err := rdb.Set(ctx, MarginCallKey(7), "11", time.Minute).Err(); err != nil {
		t.Fatal(err)
	}
	levels := staticLevels{7: {AccountID: 7, Equity: d("40"), UsedMargin: d("100"),
		MarginLevelPct: d("40"), Status: "LIQUIDATING"}}
	svc, err := NewMarginCallService(MarginCallDeps{
		Pool: mcLazyPool(t), Redis: rdb, Store: store, Levels: levels, Queue: q})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Evaluate(ctx, 7); err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	// Episode resolved STOP_OUT; account marked LIQUIDATING; the deposit
	// window is voided but the order block survives liquidation.
	if len(store.resolved) != 1 || store.resolved[0].outcome != MarginCallStoppedOut ||
		store.resolved[0].acct != 7 {
		t.Fatalf("resolve: %+v", store.resolved)
	}
	if len(store.statuses) != 1 || store.statuses[0].status != "LIQUIDATING" {
		t.Fatalf("statuses: %+v", store.statuses)
	}
	if n, _ := rdb.Exists(ctx, MarginCallKey(7)).Result(); n != 0 {
		t.Fatal("stop-out must void the deposit-window key")
	}
	job, err := q.Pop(ctx, time.Second)
	if err != nil || job == nil {
		t.Fatalf("pop: %v %+v", err, job)
	}
	if job.AccountID != 7 || job.Reason != LiquidationReasonStopOut {
		t.Fatalf("job %+v, want STOP_OUT acct 7", job)
	}
	if job.MarginLevelPct != "40" {
		t.Fatalf("job level %s, want 40", job.MarginLevelPct)
	}
}

func TestRedisMarginCallRecoverClearsEpisode(t *testing.T) {
	rdb, q := mcRedis(t, 7)
	ctx := context.Background()
	store := &marginCallStoreFake{
		open: map[int64]*MarginCallEvent{
			7: {ID: 12, AccountID: 7, MarginLevelPct: d("105"), ThresholdPct: d("111.1"),
				Status: MarginCallOpen, NotifiedAt: time.Now(),
				ExpiresAt: time.Now().Add(10 * time.Minute)},
		},
	}
	if err := rdb.Set(ctx, MarginCallKey(7), "12", time.Minute).Err(); err != nil {
		t.Fatal(err)
	}
	if err := rdb.Set(ctx, MarginCallBlockKey(7), "12", 0).Err(); err != nil {
		t.Fatal(err)
	}
	levels := staticLevels{7: {AccountID: 7, Equity: d("200"), UsedMargin: d("100"),
		MarginLevelPct: d("200"), Status: "NORMAL"}}
	svc, err := NewMarginCallService(MarginCallDeps{
		Pool: mcLazyPool(t), Redis: rdb, Store: store, Levels: levels, Queue: q})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Evaluate(ctx, 7); err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if len(store.resolved) != 1 || store.resolved[0].outcome != MarginCallRestored {
		t.Fatalf("resolve: %+v", store.resolved)
	}
	if store.open[7].Status != MarginCallRestored {
		t.Fatalf("open event status %s", store.open[7].Status)
	}
	for _, k := range []string{MarginCallKey(7), MarginCallBlockKey(7)} {
		if n, _ := rdb.Exists(ctx, k).Result(); n != 0 {
			t.Fatalf("key %s must be cleared on recovery", k)
		}
	}
	if len(store.statuses) != 1 || store.statuses[0].status != "NORMAL" {
		t.Fatalf("statuses: %+v", store.statuses)
	}
	// Healthy account with no episode/block → pure no-op.
	store.resolved, store.statuses = nil, nil
	if err := svc.Evaluate(ctx, 8); err != nil {
		t.Fatal(err)
	}
	if len(store.resolved) != 0 || len(store.statuses) != 0 {
		t.Fatal("untouched healthy account must be a no-op")
	}
}

func TestRedisMarginCallSweepExpired(t *testing.T) {
	rdb, q := mcRedis(t, 7, 8, 9)
	ctx := context.Background()
	past := time.Now().Add(-time.Minute)
	store := &marginCallStoreFake{
		open: map[int64]*MarginCallEvent{
			7: {ID: 21, AccountID: 7, MarginLevelPct: d("60"), ThresholdPct: d("111.1"),
				Status: MarginCallOpen, ExpiresAt: past, NotifiedAt: past},
			8: {ID: 22, AccountID: 8, MarginLevelPct: d("60"), ThresholdPct: d("111.1"),
				Status: MarginCallOpen, ExpiresAt: past, NotifiedAt: past},
			9: {ID: 23, AccountID: 9, MarginLevelPct: d("60"), ThresholdPct: d("111.1"),
				Status: MarginCallOpen, ExpiresAt: past, NotifiedAt: past},
		},
		expired: []MarginCallEvent{
			{ID: 21, AccountID: 7, MarginLevelPct: d("60"), ThresholdPct: d("111.1"),
				Status: MarginCallOpen, ExpiresAt: past},
			{ID: 22, AccountID: 8, MarginLevelPct: d("60"), ThresholdPct: d("111.1"),
				Status: MarginCallOpen, ExpiresAt: past},
			{ID: 23, AccountID: 9, MarginLevelPct: d("60"), ThresholdPct: d("111.1"),
				Status: MarginCallOpen, ExpiresAt: past},
		},
	}
	levels := staticLevels{
		7: {AccountID: 7, UsedMargin: d("100"), MarginLevelPct: d("60"), Status: "MARGIN_CALL"},
		8: {AccountID: 8, UsedMargin: d("100"), MarginLevelPct: d("200"), Status: "NORMAL"},
		// acct 9: no level → unreadable → fail-closed enqueue.
	}
	svc, err := NewMarginCallService(MarginCallDeps{
		Pool: mcLazyPool(t), Redis: rdb, Store: store, Levels: levels, Queue: q})
	if err != nil {
		t.Fatal(err)
	}
	n, err := svc.SweepExpired(ctx)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if n != 2 {
		t.Fatalf("sweep enqueued %d, want 2 (acct 7 still breached, acct 9 unreadable)", n)
	}
	// Outcomes: 7 LIQUIDATED, 8 RESTORED, 9 LIQUIDATED.
	outcome := map[int64]string{}
	for _, r := range store.resolved {
		outcome[r.acct] = r.outcome
	}
	if outcome[7] != MarginCallLiquidated || outcome[8] != MarginCallRestored ||
		outcome[9] != MarginCallLiquidated {
		t.Fatalf("resolve outcomes: %+v", store.resolved)
	}
	statusOf := map[int64]string{}
	for _, s := range store.statuses {
		statusOf[s.acct] = s.status
	}
	if statusOf[7] != "LIQUIDATING" || statusOf[8] != "NORMAL" || statusOf[9] != "LIQUIDATING" {
		t.Fatalf("statuses: %+v", store.statuses)
	}
	// Two queue entries — MARGIN_CALL_EXPIRED jobs for 7 and 9.
	got := map[int64]string{}
	for i := 0; i < 2; i++ {
		job, err := q.Pop(ctx, time.Second)
		if err != nil || job == nil {
			t.Fatalf("pop %d: %v", i, err)
		}
		got[job.AccountID] = job.Reason
	}
	if got[7] != LiquidationReasonMarginCallExpired || got[9] != LiquidationReasonMarginCallExpired {
		t.Fatalf("queued jobs: %+v", got)
	}
	if n, _ := q.PeekLen(ctx); n != 0 {
		t.Fatalf("queue depth %d after drain", n)
	}
}

func TestRedisMarginCallBlockProbeAndAdminRelease(t *testing.T) {
	rdb, q := mcRedis(t, 7)
	ctx := context.Background()
	store := &marginCallStoreFake{}
	svc, err := NewMarginCallService(MarginCallDeps{
		Pool: mcLazyPool(t), Redis: rdb, Store: store, Levels: staticLevels{}, Queue: q})
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := svc.HasMarginCallBlock(ctx, 7); err != nil || ok {
		t.Fatalf("no block expected: %v %v", ok, err)
	}
	if err := rdb.Set(ctx, MarginCallBlockKey(7), "55", 0).Err(); err != nil {
		t.Fatal(err)
	}
	if ok, err := svc.HasMarginCallBlock(ctx, 7); err != nil || !ok {
		t.Fatalf("block expected: %v %v", ok, err)
	}
	// Audited Risk Manager release lifts the block only (episode intact).
	if err := svc.AdminRelease(ctx, 7, 42); err != nil {
		t.Fatalf("release: %v", err)
	}
	if len(store.audits) != 1 || store.audits[0].acct != 7 || store.audits[0].actor != 42 {
		t.Fatalf("audit: %+v", store.audits)
	}
	if ok, _ := svc.HasMarginCallBlock(ctx, 7); ok {
		t.Fatal("block must be cleared after release")
	}
	// Release never resolves the episode.
	if len(store.resolved) != 0 {
		t.Fatal("admin release must not resolve the episode")
	}
}

// ---------------------------------------------------------------------------
// PG-gated store coverage (EXC_PG_TEST=1)
// ---------------------------------------------------------------------------

func TestPgMarginCallStoreNilPool(t *testing.T) {
	if _, err := NewPgMarginCallStore(nil); err == nil {
		t.Fatal("nil pool must fail construction")
	}
}

func mcStoreFixture(t *testing.T) (*PgMarginCallStore, *pgxpool.Pool) {
	t.Helper()
	pool := marginSchemaFixture(t, marginStoreMigrations)
	st, err := NewPgMarginCallStore(pool)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	return st, pool
}

func TestPgMarginCallStoreCategoryAndUser(t *testing.T) {
	st, pool := mcStoreFixture(t)
	ctx := context.Background()
	acct := liqSeedAccount(t, pool, "RETAIL")

	cat, nbp, err := st.AccountCategory(ctx, acct)
	if err != nil || cat != "RETAIL" || !nbp {
		t.Fatalf("retail: %q nbp=%v %v", cat, nbp, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE accounts SET nbp=FALSE WHERE id=$1`, acct); err != nil {
		t.Fatal(err)
	}
	if _, nbp, err = st.AccountCategory(ctx, acct); err != nil || nbp {
		t.Fatalf("nbp flag: %v %v", nbp, err)
	}
	if _, _, err := st.AccountCategory(ctx, 4242424242); err == nil {
		t.Fatal("missing account must fail closed")
	} else {
		requireCode(t, err, "ACCOUNT_NOT_FOUND")
	}

	// AccountUserID resolves the seeded user; missing → ACCOUNT_NOT_FOUND.
	var wantUser int64
	if err := pool.QueryRow(ctx, `SELECT user_id FROM accounts WHERE id=$1`, acct).
		Scan(&wantUser); err != nil {
		t.Fatal(err)
	}
	if uid, err := st.AccountUserID(ctx, acct); err != nil || uid != wantUser {
		t.Fatalf("user: %d %v, want %d", uid, err, wantUser)
	}
	if _, err := st.AccountUserID(ctx, 4242424242); err == nil {
		t.Fatal("missing account must fail closed")
	}
}

func TestPgMarginCallStoreThresholdOverride(t *testing.T) {
	st, pool := mcStoreFixture(t)
	ctx := context.Background()
	acct := liqSeedAccount(t, pool, "RETAIL")

	ov, err := st.MarginThresholdOverride(ctx, acct)
	if err != nil || ov != nil {
		t.Fatalf("absent override: %+v %v", ov, err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO account_margin_thresholds
		    (account_id, warning_pct, margin_call_pct, stop_out_pct, updated_by)
		VALUES ($1, 160, 150, 90, 0)`, acct); err != nil {
		t.Fatalf("seed override: %v", err)
	}
	ov, err = st.MarginThresholdOverride(ctx, acct)
	if err != nil || ov == nil {
		t.Fatalf("override: %v %+v", err, ov)
	}
	if !ov.MarginCallPct.Equal(d("150")) || !ov.StopOutPct.Equal(d("90")) || ov.Source != "OVERRIDE" {
		t.Fatalf("override decode: %+v", ov)
	}
}

func TestPgMarginCallStoreEpisodeLifecycle(t *testing.T) {
	st, pool := mcStoreFixture(t)
	ctx := context.Background()
	acct := liqSeedAccount(t, pool, "RETAIL")
	liqSeedMarginAccount(t, pool, acct, "CROSS", "")

	if ev, err := st.OpenMarginCall(ctx, acct); err != nil || ev != nil {
		t.Fatalf("no open episode: %+v %v", ev, err)
	}

	// InsertMarginCall + status flip inside one tx.
	expires := time.Now().Add(MarginCallWindowSeconds * time.Second).UTC()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	id, err := st.InsertMarginCall(ctx, tx, &MarginCallEvent{
		AccountID: acct, MarginLevelPct: d("105"), ThresholdPct: MarginCallThresholdPct,
		Status: MarginCallOpen, NotifiedAt: time.Now().UTC(), ExpiresAt: expires})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err := st.SetMarginAccountStatusTx(ctx, tx, acct, "MARGIN_CALL"); err != nil {
		t.Fatalf("status tx: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if id <= 0 {
		t.Fatalf("bad event id %d", id)
	}
	// The §13.3 audit row lands in the same commit.
	var audits int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM admin_audit_log
		WHERE action='risk.margin_call.open' AND target_id=$1`, id).
		Scan(&audits); err != nil || audits != 1 {
		t.Fatalf("audit rows %d %v", audits, err)
	}
	var status string
	if err := pool.QueryRow(ctx,
		`SELECT status::text FROM margin_accounts WHERE account_id=$1`, acct).
		Scan(&status); err != nil || status != "MARGIN_CALL" {
		t.Fatalf("margin status: %q %v", status, err)
	}

	ev, err := st.OpenMarginCall(ctx, acct)
	if err != nil || ev == nil {
		t.Fatalf("open read: %v %+v", err, ev)
	}
	if ev.ID != id || ev.Status != MarginCallOpen ||
		!ev.MarginLevelPct.Equal(d("105")) || !ev.ThresholdPct.Equal(d("111.1")) ||
		ev.ResolvedAt != nil {
		t.Fatalf("event decode: %+v", ev)
	}

	// The partial unique index forbids a second OPEN episode.
	tx2, _ := pool.Begin(ctx)
	if _, err := st.InsertMarginCall(ctx, tx2, &MarginCallEvent{
		AccountID: acct, MarginLevelPct: d("100"), ThresholdPct: d("111.1"),
		Status: MarginCallOpen, NotifiedAt: time.Now(), ExpiresAt: expires}); err == nil {
		t.Fatal("duplicate OPEN episode must violate the unique index")
	}
	_ = tx2.Rollback(ctx)

	// Resolve → RESTORED + resolved_at stamped; second resolve errors.
	at := time.Now().UTC()
	if err := st.ResolveMarginCall(ctx, acct, MarginCallRestored, at); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if ev, err := st.OpenMarginCall(ctx, acct); err != nil || ev != nil {
		t.Fatalf("resolved episode must not read open: %+v %v", ev, err)
	}
	var resStatus string
	var resAt *time.Time
	if err := pool.QueryRow(ctx,
		`SELECT status, resolved_at FROM margin_call_events WHERE id=$1`, id).
		Scan(&resStatus, &resAt); err != nil {
		t.Fatal(err)
	}
	if resStatus != MarginCallRestored || resAt == nil {
		t.Fatalf("resolved row: %s %v", resStatus, resAt)
	}
	if err := st.ResolveMarginCall(ctx, acct, MarginCallRestored, at); err == nil {
		t.Fatal("re-resolve of a closed episode must error")
	}
}

func TestPgMarginCallStoreExpiredScan(t *testing.T) {
	st, pool := mcStoreFixture(t)
	ctx := context.Background()
	past := liqSeedAccount(t, pool, "RETAIL")
	future := liqSeedAccount(t, pool, "RETAIL")
	liqSeedMarginAccount(t, pool, past, "CROSS", "")
	liqSeedMarginAccount(t, pool, future, "CROSS", "")

	now := time.Now().UTC()
	if _, err := pool.Exec(ctx, `
		INSERT INTO margin_call_events
		    (account_id, margin_level_pct, threshold_pct, status, notified_at, expires_at)
		VALUES ($1, 60, 111.1, 'OPEN', $2, $3),
		       ($4, 60, 111.1, 'OPEN', $2, $5)`,
		past, now.Add(-20*time.Minute), now.Add(-5*time.Minute),
		future, now.Add(10*time.Minute)); err != nil {
		t.Fatalf("seed episodes: %v", err)
	}
	got, err := st.ExpiredOpenMarginCalls(ctx, now)
	if err != nil || len(got) != 1 {
		t.Fatalf("expired: %v %+v", err, got)
	}
	if got[0].AccountID != past {
		t.Fatalf("expired acct %d, want %d", got[0].AccountID, past)
	}
}

func TestPgMarginCallStoreStatusAndAuditRelease(t *testing.T) {
	st, pool := mcStoreFixture(t)
	ctx := context.Background()
	acct := liqSeedAccount(t, pool, "RETAIL")

	// No margin_accounts row → error (zero-row update is never silent).
	if err := st.SetMarginAccountStatus(ctx, acct, "LIQUIDATING"); err == nil {
		t.Fatal("missing margin row must error")
	}
	liqSeedMarginAccount(t, pool, acct, "CROSS", "")
	if err := st.SetMarginAccountStatus(ctx, acct, "LIQUIDATING"); err != nil {
		t.Fatalf("set status: %v", err)
	}
	var status string
	if err := pool.QueryRow(ctx,
		`SELECT status::text FROM margin_accounts WHERE account_id=$1`, acct).
		Scan(&status); err != nil || status != "LIQUIDATING" {
		t.Fatalf("status read-back: %q %v", status, err)
	}
	if err := st.SetMarginAccountStatus(ctx, acct, "BOGUS"); err == nil {
		t.Fatal("invalid enum value must error")
	}

	if err := st.AuditMarginCallRelease(ctx, acct, 42); err != nil {
		t.Fatalf("release audit: %v", err)
	}
	var n int
	var actor int64
	if err := pool.QueryRow(ctx, `
		SELECT count(*), max(admin_user_id) FROM admin_audit_log
		WHERE action='risk.margin_call.release' AND target_id=$1`, acct).
		Scan(&n, &actor); err != nil || n != 1 || actor != 42 {
		t.Fatalf("audit row: n=%d actor=%d err=%v", n, actor, err)
	}
}

// ---------------------------------------------------------------------------
// Dual-gated end-to-end lifecycle (EXC_PG_TEST=1 + EXC_REDIS_TEST=1)
// ---------------------------------------------------------------------------

func TestPgMarginCallLifecycleEndToEnd(t *testing.T) {
	if os.Getenv("EXC_PG_TEST") != "1" {
		t.Skip("EXC_PG_TEST not set")
	}
	rdb, q := mcRedis(t)
	ctx := context.Background()
	pool := marginSchemaFixture(t, marginStoreMigrations)
	store, err := NewPgMarginCallStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	levels := RedisMarginLevelReader{C: rdb}
	notices := &notifySpy{}
	svc, err := NewMarginCallService(MarginCallDeps{
		Pool: pool, Redis: rdb, Store: store, Levels: levels,
		Queue: q, Notify: notices, Alerter: &opsAlertSpy{}})
	if err != nil {
		t.Fatal(err)
	}

	// --- acct A: open → idempotent re-entry → stop-out precedence ---
	acct50 := liqSeedAccount(t, pool, "RETAIL")
	acct51 := liqSeedAccount(t, pool, "RETAIL")
	liqSeedMarginAccount(t, pool, acct50, "CROSS", "")
	liqSeedMarginAccount(t, pool, acct51, "CROSS", "")
	// db14 is shared — clear any keys a previous run left at these ids.
	for _, a := range []int64{acct50, acct51} {
		if err := rdb.Del(ctx, MarginCallKey(a), MarginCallBlockKey(a),
			LiquidationDedupKey(a), MarginLevelKey(a)).Err(); err != nil {
			t.Fatal(err)
		}
	}

	// margin-call level (105% ≤ 111.1 canonical, above 50% stop-out).
	if err := levels.SetMarginLevel(ctx, MarginLevel{
		AccountID: acct50, Equity: d("105"), UsedMargin: d("100"),
		MarginLevelPct: d("105"), Status: "MARGIN_CALL", UpdatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	if err := svc.Evaluate(ctx, acct50); err != nil {
		t.Fatalf("open evaluate: %v", err)
	}
	ev, err := store.OpenMarginCall(ctx, acct50)
	if err != nil || ev == nil || ev.Status != MarginCallOpen {
		t.Fatalf("open episode: %v %+v", err, ev)
	}
	if !ev.ThresholdPct.Equal(d("111.1")) || !ev.MarginLevelPct.Equal(d("105")) ||
		ev.ExpiresAt.Before(time.Now().Add(14*time.Minute)) {
		t.Fatalf("episode fields: %+v", ev)
	}
	// Window key ~900s TTL, block key persistent.
	ttl, err := rdb.TTL(ctx, MarginCallKey(acct50)).Result()
	if err != nil || ttl <= 800*time.Second || ttl > 900*time.Second {
		t.Fatalf("window ttl %v", ttl)
	}
	bttl, err := rdb.TTL(ctx, MarginCallBlockKey(acct50)).Result()
	if err != nil || bttl != -1 {
		t.Fatalf("block key must persist (ttl=%v)", bttl)
	}
	var status string
	if err := pool.QueryRow(ctx,
		`SELECT status::text FROM margin_accounts WHERE account_id=$1`, acct50).
		Scan(&status); err != nil || status != "MARGIN_CALL" {
		t.Fatalf("status: %q %v", status, err)
	}
	if notices.count() != 1 {
		t.Fatalf("expected one liquidation_warning, got %d", notices.count())
	}

	// Re-evaluate inside the episode → idempotent no-op.
	if err := svc.Evaluate(ctx, acct50); err != nil {
		t.Fatal(err)
	}
	var opens int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM margin_call_events WHERE account_id=$1 AND status='OPEN'`,
		acct50).Scan(&opens); err != nil || opens != 1 {
		t.Fatalf("open episodes %d", opens)
	}
	if notices.count() != 1 {
		t.Fatal("re-entry must not re-notify")
	}

	// §13.3 precedence: crash to stop-out voids the window, liquidates.
	if err := levels.SetMarginLevel(ctx, MarginLevel{
		AccountID: acct50, Equity: d("40"), UsedMargin: d("100"),
		MarginLevelPct: d("40"), Status: "LIQUIDATING", UpdatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	if err := svc.Evaluate(ctx, acct50); err != nil {
		t.Fatalf("stop-out evaluate: %v", err)
	}
	var resStatus string
	if err := pool.QueryRow(ctx,
		`SELECT status FROM margin_call_events WHERE id=$1`, ev.ID).
		Scan(&resStatus); err != nil || resStatus != MarginCallStoppedOut {
		t.Fatalf("episode outcome: %q %v", resStatus, err)
	}
	if n, _ := rdb.Exists(ctx, MarginCallKey(acct50)).Result(); n != 0 {
		t.Fatal("stop-out must void the window key")
	}
	// The order block outlives the window through liquidation.
	if n, _ := rdb.Exists(ctx, MarginCallBlockKey(acct50)).Result(); n == 0 {
		t.Fatal("block flag must persist through stop-out")
	}
	if err := pool.QueryRow(ctx,
		`SELECT status::text FROM margin_accounts WHERE account_id=$1`, acct50).
		Scan(&status); err != nil || status != "LIQUIDATING" {
		t.Fatalf("status: %q %v", status, err)
	}
	job, err := q.Pop(ctx, time.Second)
	if err != nil || job == nil || job.Reason != LiquidationReasonStopOut ||
		job.AccountID != acct50 {
		t.Fatalf("job: %v %+v", err, job)
	}

	// --- acct 51: open → recovery inside the window → RESTORED ---
	if err := levels.SetMarginLevel(ctx, MarginLevel{
		AccountID: acct51, Equity: d("105"), UsedMargin: d("100"),
		MarginLevelPct: d("105"), Status: "MARGIN_CALL", UpdatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	if err := svc.Evaluate(ctx, acct51); err != nil {
		t.Fatalf("open evaluate: %v", err)
	}
	if err := levels.SetMarginLevel(ctx, MarginLevel{
		AccountID: acct51, Equity: d("220"), UsedMargin: d("100"),
		MarginLevelPct: d("220"), Status: "NORMAL", UpdatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	if err := svc.Evaluate(ctx, acct51); err != nil {
		t.Fatalf("recover evaluate: %v", err)
	}
	var st51 string
	var resAt *time.Time
	if err := pool.QueryRow(ctx,
		`SELECT status, resolved_at FROM margin_call_events WHERE account_id=$1`, acct51).
		Scan(&st51, &resAt); err != nil || st51 != MarginCallRestored || resAt == nil {
		t.Fatalf("restored episode: %q %v", st51, err)
	}
	for _, k := range []string{MarginCallKey(acct51), MarginCallBlockKey(acct51)} {
		if n, _ := rdb.Exists(ctx, k).Result(); n != 0 {
			t.Fatalf("recovery must clear %s", k)
		}
	}
	if err := pool.QueryRow(ctx,
		`SELECT status::text FROM margin_accounts WHERE account_id=$1`, acct51).
		Scan(&status); err != nil || status != "NORMAL" {
		t.Fatalf("status: %q %v", status, err)
	}
}
