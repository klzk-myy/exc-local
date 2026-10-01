// margin_engine_test.go — Phase-19 Task 19.3.26 coverage: heap ordering
// under churn, the tick→evaluate→heap→dispatch path with fakes, the
// <50µs-per-account budget assertion + benchmark, cold-leg refresh
// queueing, and the gated Redis enqueue leg.
package risk

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"sync"
	"testing"
	"time"

	excredis "exchange/internal/redis"
	"exchange/pkg/decimal"
)

// ---------------------------------------------------------------------------
// Heap unit coverage
// ---------------------------------------------------------------------------

func TestMarginLevelHeapOrdering(t *testing.T) {
	h := NewMarginLevelHeap()
	lvl := func(v string) *decimal.Decimal { d := d(v); return &d }

	h.Upsert(3, lvl("80"))
	h.Upsert(1, lvl("30"))
	h.Upsert(2, lvl("50"))
	h.Upsert(9, nil) // +∞ — sorts last
	if h.Len() != 4 {
		t.Fatalf("len %d", h.Len())
	}
	got := h.Ordered()
	want := []int64{1, 2, 3, 9}
	for i, w := range want {
		if got[i].AccountID != w {
			t.Fatalf("ordered[%d]=%d want %d", i, got[i].AccountID, w)
		}
	}
	// Re-prioritize: account 1 recovers to 200, account 3 crashes to 10.
	h.Upsert(1, lvl("200"))
	h.Upsert(3, lvl("10"))
	top, ok := h.Peek()
	if !ok || top.AccountID != 3 {
		t.Fatalf("peek %+v ok=%v want acct 3", top, ok)
	}
	// Remove mid + pop ordering.
	if !h.Remove(2) {
		t.Fatal("remove acct 2")
	}
	var pops []int64
	for {
		e, ok := h.PopMin()
		if !ok {
			break
		}
		pops = append(pops, e.AccountID)
	}
	if fmt.Sprint(pops) != "[3 1 9]" {
		t.Fatalf("pop order %v", pops)
	}
	if h.Len() != 0 {
		t.Fatalf("heap not drained: %d", h.Len())
	}
}

// TestMarginLevelHeapChurn hammers random upserts/removes and verifies
// (a) pop order is non-decreasing and (b) heap contents match a shadow
// map — the continuous-sort-order invariant under throughput.
func TestMarginLevelHeapChurn(t *testing.T) {
	h := NewMarginLevelHeap()
	shadow := map[int64]decimal.Decimal{}
	rnd := rand.New(rand.NewSource(42))

	for i := 0; i < 20000; i++ {
		acct := int64(rnd.Intn(500) + 1)
		if rnd.Intn(5) == 0 {
			h.Remove(acct)
			delete(shadow, acct)
			continue
		}
		l := decimal.NewFromInt(int64(rnd.Intn(30000))).Div(decimal.NewFromInt(100))
		h.Upsert(acct, &l)
		shadow[acct] = l
	}
	if int64(len(shadow)) != int64(h.Len()) {
		t.Fatalf("heap %d vs shadow %d", h.Len(), len(shadow))
	}
	// Ordered snapshot must be non-decreasing and complete.
	ord := h.Ordered()
	if len(ord) != len(shadow) {
		t.Fatalf("ordered len %d vs %d", len(ord), len(shadow))
	}
	for i := 1; i < len(ord); i++ {
		if ord[i].HasLevel && ord[i-1].HasLevel &&
			ord[i].Level.LessThan(ord[i-1].Level) {
			t.Fatalf("ordered not sorted at %d: %s < %s",
				i, ord[i].Level, ord[i-1].Level)
		}
	}
	// Drain — every pop must be the current shadow minimum.
	for h.Len() > 0 {
		e, _ := h.PopMin()
		want, ok := shadow[e.AccountID]
		if !ok {
			t.Fatalf("popped unknown acct %d", e.AccountID)
		}
		if !want.Equal(e.Level) {
			t.Fatalf("acct %d level %s vs shadow %s", e.AccountID, e.Level, want)
		}
		// Minimality: no remaining shadow level below the popped one.
		for _, v := range shadow {
			if v.LessThan(e.Level) {
				t.Fatalf("popped %s while %s remains", e.Level, v)
			}
		}
		delete(shadow, e.AccountID)
	}
}

// ---------------------------------------------------------------------------
// Engine fakes
// ---------------------------------------------------------------------------

// engineStoreFake is an in-memory MarginStore for the leg loader.
type engineStoreFake struct {
	mu     sync.Mutex
	accts  map[int64]*MarginAccount
	cats   map[int64]string
	bases  map[int64]string
	bals   map[int64][]BalanceAmount
	poss   map[int64][]MarginPosition
	pairs  map[string]FxPair
	oidx   map[string][]int64
	writes []MarginSnapshot
}

func newEngineStoreFake() *engineStoreFake {
	return &engineStoreFake{
		accts: map[int64]*MarginAccount{},
		cats:  map[int64]string{},
		bases: map[int64]string{},
		bals:  map[int64][]BalanceAmount{},
		poss:  map[int64][]MarginPosition{},
		pairs: map[string]FxPair{},
		oidx:  map[string][]int64{},
	}
}

func (f *engineStoreFake) MarginAccount(_ context.Context, id int64) (*MarginAccount, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.accts[id], nil
}
func (f *engineStoreFake) SetMarginMode(context.Context, int64, MarginMode) error {
	return fmt.Errorf("fake: unsupported")
}
func (f *engineStoreFake) OpenPositionCount(_ context.Context, id int64) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return int64(len(f.poss[id])), nil
}
func (f *engineStoreFake) AccountCategory(_ context.Context, id int64) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.cats[id]
	if !ok {
		return "", fmt.Errorf("fake: account %d not found", id)
	}
	return c, nil
}
func (f *engineStoreFake) AccountBaseCurrency(_ context.Context, id int64) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if c, ok := f.bases[id]; ok {
		return c, nil
	}
	return "USD", nil
}
func (f *engineStoreFake) Balances(_ context.Context, id int64) ([]BalanceAmount, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]BalanceAmount(nil), f.bals[id]...), nil
}
func (f *engineStoreFake) MarginPositions(_ context.Context, id int64) ([]MarginPosition, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]MarginPosition(nil), f.poss[id]...), nil
}
func (f *engineStoreFake) FxPairInstruments(_ context.Context, ccys []string) (map[string]FxPair, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]FxPair{}
	for _, c := range ccys {
		if fp, ok := f.pairs[c]; ok {
			out[c] = fp
		}
	}
	return out, nil
}
func (f *engineStoreFake) OpenPositionIndex(context.Context) (map[string][]int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make(map[string][]int64, len(f.oidx))
	for k, v := range f.oidx {
		out[k] = append([]int64(nil), v...)
	}
	return out, nil
}
func (f *engineStoreFake) WriteMarginSnapshot(_ context.Context, s MarginSnapshot) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.writes = append(f.writes, s)
	return nil
}

// dispatcherSpy records jobs; returns configurable dedup result.
type dispatcherSpy struct {
	mu   sync.Mutex
	jobs []LiquidationJob
	enq  bool
	err  error
}

func (s *dispatcherSpy) Dispatch(_ context.Context, job LiquidationJob, _ *MarginSnapshot) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.jobs = append(s.jobs, job)
	return s.enq, s.err
}
func (s *dispatcherSpy) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.jobs)
}
func (s *dispatcherSpy) last() LiquidationJob {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.jobs[len(s.jobs)-1]
}

// sinkSpy records publish calls.
type sinkSpy struct {
	mu        sync.Mutex
	published []MarginSnapshot
	changed   []bool
	err       error
}

func (s *sinkSpy) Publish(_ context.Context, snap *MarginSnapshot, changed bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.published = append(s.published, *snap)
	s.changed = append(s.changed, changed)
	return s.err
}
func (s *sinkSpy) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.published)
}

// ---------------------------------------------------------------------------
// Engine rig
// ---------------------------------------------------------------------------

// seedCrossAccount wires a CROSS retail account: USD base, USD balance,
// one EUR/USD LONG (qty 10000 @ 1.10, margin_used 1000).
func seedCrossAccount(f *engineStoreFake, acct int64, avail, locked string, posID int64) {
	mark := d("1.10")
	f.cats[acct] = "RETAIL"
	f.bases[acct] = "USD"
	f.accts[acct] = &MarginAccount{AccountID: acct, Mode: ModeCross, Status: "NORMAL"}
	f.bals[acct] = []BalanceAmount{
		{Currency: "USD", Available: d(avail), Locked: d(locked)},
	}
	f.poss[acct] = []MarginPosition{{
		ID: posID, InstrumentID: 1, Symbol: "EUR/USD", Side: "LONG",
		Quantity: d("10000"), EntryPrice: d("1.10"), StoredMark: &mark,
		MarginUsed: d("1000"), QuoteCurrency: "USD", BaseCurrency: "EUR",
		MaxLeverage: 30,
	}}
	f.oidx["EUR/USD"] = append(f.oidx["EUR/USD"], acct)
}

func newEngineRig(t *testing.T) (*MarginEngine, *engineStoreFake, *dispatcherSpy, *sinkSpy) {
	t.Helper()
	f := newEngineStoreFake()
	dspy := &dispatcherSpy{enq: true}
	sink := &sinkSpy{}
	e, err := NewMarginEngine(MarginEngineDeps{
		Store:      f,
		Marks:      markCacheFake{m: map[string]decimal.Decimal{"EUR/USD": d("1.10"), "GBP/USD": d("1.25")}},
		Dispatcher: dspy,
		Sink:       sink,
	})
	if err != nil {
		t.Fatalf("engine: %v", err)
	}
	return e, f, dspy, sink
}

func TestMarginEngineTickEvaluatesAndDispatchesStopOut(t *testing.T) {
	e, f, dspy, sink := newEngineRig(t)
	ctx := context.Background()
	seedCrossAccount(f, 7, "800", "200", 101)

	if err := e.RefreshIndex(ctx); err != nil {
		t.Fatalf("index: %v", err)
	}
	if err := e.WarmAccount(ctx, 7); err != nil {
		t.Fatalf("warm: %v", err)
	}
	// Mark 1.10 → equity 1000, used 1000 → level 100 → NORMAL.
	ent, ok := e.HeapEntry(7)
	if !ok || !ent.HasLevel || !ent.Level.Equal(d("100")) {
		t.Fatalf("heap entry %+v want level 100", ent)
	}
	if dspy.count() != 0 {
		t.Fatal("healthy account must not dispatch")
	}
	if sink.count() != 1 || !sink.changed[0] {
		t.Fatalf("first publish expected with status change: %d %v", sink.count(), sink.changed)
	}

	// Tick 1.05 → uPnL −500 → equity 500 → level 50 → breach (≤50%).
	e.PushMark(ctx, MarkTick{Symbol: "EUR/USD", Price: d("1.05"),
		Source: MarkSourceOracle, Ts: time.Now()})
	if dspy.count() != 1 {
		t.Fatalf("breach must dispatch once, got %d", dspy.count())
	}
	job := dspy.last()
	if job.Reason != LiquidationReasonStopOut || job.AccountID != 7 ||
		job.PositionID != 0 {
		t.Fatalf("job %+v", job)
	}
	if job.MarginLevelPct != "50" {
		t.Fatalf("job level %s want 50", job.MarginLevelPct)
	}
	ent, _ = e.HeapEntry(7)
	if !ent.Level.Equal(d("50")) {
		t.Fatalf("heap level %s want 50", ent.Level)
	}
	// Still-breached tick inside the redispatch window → suppressed.
	e.PushMark(ctx, MarkTick{Symbol: "EUR/USD", Price: d("1.04"),
		Source: MarkSourceOracle, Ts: time.Now()})
	if dspy.count() != 1 {
		t.Fatalf("redispatch window must suppress, got %d", dspy.count())
	}
	// Recovery clears the gate → next breach re-dispatches.
	e.PushMark(ctx, MarkTick{Symbol: "EUR/USD", Price: d("1.12"),
		Source: MarkSourceOracle, Ts: time.Now()})
	e.PushMark(ctx, MarkTick{Symbol: "EUR/USD", Price: d("1.05"),
		Source: MarkSourceOracle, Ts: time.Now()})
	if dspy.count() != 2 {
		t.Fatalf("post-recovery breach must dispatch again, got %d", dspy.count())
	}
	// Status flipped back to LIQUIDATING — two publishes with change.
	last := sink.published[sink.count()-1]
	if last.Status != "LIQUIDATING" {
		t.Fatalf("published status %s", last.Status)
	}
}

func TestMarginEngineUnrelatedTickDoesNotEval(t *testing.T) {
	e, f, dspy, _ := newEngineRig(t)
	ctx := context.Background()
	seedCrossAccount(f, 7, "800", "200", 101)
	if err := e.RefreshIndex(ctx); err != nil {
		t.Fatal(err)
	}
	if err := e.WarmAccount(ctx, 7); err != nil {
		t.Fatal(err)
	}
	ent0, _ := e.HeapEntry(7)
	// A symbol the account neither holds nor converts through.
	e.PushMark(ctx, MarkTick{Symbol: "AUD/USD", Price: d("0.66"), Ts: time.Now()})
	ent1, _ := e.HeapEntry(7)
	if !ent0.Level.Equal(ent1.Level) {
		t.Fatal("unrelated tick must not re-evaluate")
	}
	if dspy.count() != 0 {
		t.Fatal("no dispatch expected")
	}
}

func TestMarginEngineConversionPairTickReprices(t *testing.T) {
	e, f, dspy, _ := newEngineRig(t)
	ctx := context.Background()
	// Account holds a USD/JPY position (JPY-quoted, converts via the
	// USD/JPY inverse pair) plus a EUR balance converting via EUR/USD.
	mark := d("150.00")
	f.cats[9] = "RETAIL"
	f.bases[9] = "USD"
	f.accts[9] = &MarginAccount{AccountID: 9, Mode: ModeCross, Status: "NORMAL"}
	f.bals[9] = []BalanceAmount{
		{Currency: "USD", Available: d("500")},
		{Currency: "EUR", Available: d("1000")}, // converts via EUR/USD 1.10
	}
	f.poss[9] = []MarginPosition{{
		ID: 201, InstrumentID: 3, Symbol: "USD/JPY", Side: "LONG",
		Quantity: d("10000"), EntryPrice: d("150.00"), StoredMark: &mark,
		MarginUsed:    d("50000"), // JPY
		QuoteCurrency: "JPY", BaseCurrency: "USD", MaxLeverage: 30,
	}}
	f.oidx["USD/JPY"] = []int64{9}
	f.pairs["EUR"] = FxPair{Symbol: "EUR/USD"}
	f.pairs["JPY"] = FxPair{Symbol: "USD/JPY", Inverted: true}

	e2, err := NewMarginEngine(MarginEngineDeps{
		Store: f,
		Marks: markCacheFake{m: map[string]decimal.Decimal{
			"EUR/USD": d("1.10"), "USD/JPY": d("150.00")}},
		Dispatcher: dspy,
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = e
	if err := e2.RefreshIndex(ctx); err != nil {
		t.Fatal(err)
	}
	if err := e2.WarmAccount(ctx, 9); err != nil {
		t.Fatal(err)
	}
	// Equity: 500 USD + 1000 EUR×1.10 + 0 upnl = 1600.
	// Used: 50000 JPY × (1/150) = 333.33 → level = 1600/333.33 = 480%.
	ent, ok := e2.HeapEntry(9)
	if !ok || !ent.HasLevel {
		t.Fatalf("heap %+v", ent)
	}
	// EUR/USD ticks reprice the EUR balance leg — the account is
	// affected even though it holds no EUR/USD position.
	e2.PushMark(ctx, MarkTick{Symbol: "EUR/USD", Price: d("0.70"), Ts: time.Now()})
	ent2, _ := e2.HeapEntry(9)
	// Equity now 500 + 700 = 1200 → level 360%.
	if !ent2.Level.LessThan(ent.Level) {
		t.Fatalf("EUR crash must reprice equity: %s !< %s", ent2.Level, ent.Level)
	}
	if dspy.count() != 0 {
		t.Fatal("no breach expected")
	}
}

func TestMarginEngineColdAccountQueuesRefresh(t *testing.T) {
	e, f, dspy, _ := newEngineRig(t)
	ctx := context.Background()
	seedCrossAccount(f, 7, "800", "200", 101)
	if err := e.RefreshIndex(ctx); err != nil {
		t.Fatal(err)
	}
	if e.LegsLoaded(7) {
		t.Fatal("legs must not be loaded yet")
	}
	// Tick arrives before legs — no evaluation, refresh queued.
	e.PushMark(ctx, MarkTick{Symbol: "EUR/USD", Price: d("1.01"), Ts: time.Now()})
	select {
	case acct := <-e.refreshCh:
		if acct != 7 {
			t.Fatalf("refresh acct %d", acct)
		}
	case <-time.After(time.Second):
		t.Fatal("cold account must queue a leg refresh")
	}
	if dspy.count() != 0 {
		t.Fatal("unevaluated cold account must not dispatch")
	}
}

// ---------------------------------------------------------------------------
// Isolated delegation (engine → IsolatedMarginService integration)
// ---------------------------------------------------------------------------

func TestMarginEngineIsolatedDelegatesPositionScope(t *testing.T) {
	ctx := context.Background()
	f := newEngineStoreFake()
	dspy := &dispatcherSpy{enq: true}
	isoStore := newIsoStoreFake()

	acct := int64(8)
	mark := d("1.10")
	f.cats[acct] = "RETAIL"
	f.bases[acct] = "USD"
	f.accts[acct] = &MarginAccount{AccountID: acct, Mode: ModeIsolated, Status: "NORMAL"}
	f.bals[acct] = []BalanceAmount{{Currency: "USD", Available: d("5000")}}
	f.poss[acct] = []MarginPosition{
		{ID: 101, InstrumentID: 1, Symbol: "EUR/USD", Side: "LONG",
			Quantity: d("10000"), EntryPrice: d("1.10"), StoredMark: &mark,
			MarginUsed: d("1000"), QuoteCurrency: "USD", BaseCurrency: "EUR",
			MaxLeverage: 30, IsolatedAllocated: d("1000"), AutoReplenish: false},
		{ID: 102, InstrumentID: 2, Symbol: "GBP/USD", Side: "LONG",
			Quantity: d("5000"), EntryPrice: d("1.25"), StoredMark: &gmark,
			MarginUsed: d("500"), QuoteCurrency: "USD", BaseCurrency: "GBP",
			MaxLeverage: 30, IsolatedAllocated: d("500"), AutoReplenish: false},
	}
	f.oidx["EUR/USD"] = []int64{acct}
	f.oidx["GBP/USD"] = []int64{acct}
	isoStore.modes[acct] = ModeIsolated
	isoStore.bases[acct] = "USD"
	isoStore.bals[acct] = map[string]isoBal{"USD": {avail: d("5000")}}
	isoStore.poss[101] = &isoPos{allocated: d("1000"), auto: false, accountID: acct}
	isoStore.poss[102] = &isoPos{allocated: d("500"), auto: false, accountID: acct}

	svc, err := NewIsolatedMarginService(IsolatedMarginDeps{
		Store: isoStore, Dispatcher: dspy})
	if err != nil {
		t.Fatal(err)
	}
	e, err := NewMarginEngine(MarginEngineDeps{
		Store: f,
		Marks: markCacheFake{m: map[string]decimal.Decimal{
			"EUR/USD": d("1.10"), "GBP/USD": d("1.25")}},
		Dispatcher: dspy, Isolated: svc,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.RefreshIndex(ctx); err != nil {
		t.Fatal(err)
	}
	if err := e.WarmAccount(ctx, acct); err != nil {
		t.Fatal(err)
	}
	// EUR/USD 1.04 → pos101 uPnL −600 → (1000−600)/1000 = 40% breach;
	// pos102 untouched (no GBP tick).
	e.PushMark(ctx, MarkTick{Symbol: "EUR/USD", Price: d("1.04"), Ts: time.Now()})
	if dspy.count() != 1 {
		t.Fatalf("isolated breach must dispatch once, got %d", dspy.count())
	}
	job := dspy.last()
	if job.Reason != LiquidationReasonIsolatedDeficit ||
		job.AccountID != acct || job.PositionID != 101 {
		t.Fatalf("position-scoped job expected, got %+v", job)
	}
	// No account-level dispatch and no cross-contamination.
	for _, j := range dspy.jobs {
		if j.Reason == LiquidationReasonStopOut {
			t.Fatal("isolated account must never emit account-level stop-out")
		}
	}
	if !isoStore.bals[acct]["USD"].avail.Equal(d("5000")) {
		t.Fatal("auto-replenish off → balance must be untouched")
	}
	if !isoStore.poss[102].allocated.Equal(d("500")) {
		t.Fatal("other position's allocation must be untouched")
	}
}

// ---------------------------------------------------------------------------
// Latency budget — <50µs per account per tick evaluation
// ---------------------------------------------------------------------------

func TestMarginEngineEvalLatencyBudget(t *testing.T) {
	ctx := context.Background()
	f := newEngineStoreFake()
	dspy := &dispatcherSpy{enq: true}
	const N = 60
	for i := 0; i < N; i++ {
		seedCrossAccount(f, int64(1000+i), "800", "200", int64(5000+i))
	}
	e, err := NewMarginEngine(MarginEngineDeps{
		Store:      f,
		Marks:      markCacheFake{m: map[string]decimal.Decimal{"EUR/USD": d("1.10")}},
		Dispatcher: dspy,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.RefreshIndex(ctx); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < N; i++ {
		if err := e.WarmAccount(ctx, int64(1000+i)); err != nil {
			t.Fatal(err)
		}
	}
	// Healthy tick — every account holds EUR/USD so all 60 evaluate.
	start := time.Now()
	const reps = 20
	for i := 0; i < reps; i++ {
		e.PushMark(ctx, MarkTick{Symbol: "EUR/USD",
			Price: d("1.1001"), Ts: time.Now()})
	}
	perAcct := time.Since(start) / (reps * N)
	budget := 50 * time.Microsecond
	if raceDetectorOn {
		budget = 500 * time.Microsecond // -race skews wall-clock ~10×
	}
	if perAcct > budget {
		t.Fatalf("per-account evaluation %v exceeds %v budget", perAcct, budget)
	}
	if dspy.count() != 0 {
		t.Fatalf("no breach expected at healthy marks, got %d dispatches", dspy.count())
	}
	t.Logf("per-account evaluation: %v (%d accounts × %d ticks)", perAcct, N, reps)
}

// BenchmarkMarginEngineTickEval measures the hot path.
func BenchmarkMarginEngineTickEval(b *testing.B) {
	ctx := context.Background()
	f := newEngineStoreFake()
	dspy := &dispatcherSpy{enq: true}
	const N = 100
	for i := 0; i < N; i++ {
		seedCrossAccount(f, int64(2000+i), "800", "200", int64(9000+i))
	}
	e, err := NewMarginEngine(MarginEngineDeps{
		Store:      f,
		Marks:      markCacheFake{m: map[string]decimal.Decimal{"EUR/USD": d("1.10")}},
		Dispatcher: dspy,
	})
	if err != nil {
		b.Fatal(err)
	}
	if err := e.RefreshIndex(ctx); err != nil {
		b.Fatal(err)
	}
	for i := 0; i < N; i++ {
		if err := e.WarmAccount(ctx, int64(2000+i)); err != nil {
			b.Fatal(err)
		}
	}
	tick := MarkTick{Symbol: "EUR/USD", Price: d("1.1001"), Ts: time.Now()}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		e.PushMark(ctx, tick)
	}
	b.ReportMetric(float64(b.Elapsed())/float64(int64(b.N)*N)/float64(time.Microsecond),
		"µs/acct")
}

// ---------------------------------------------------------------------------
// Gated Redis leg — real queue dedup (EXC_REDIS_TEST=1)
// ---------------------------------------------------------------------------

func TestMarginEngineRealQueueDedup(t *testing.T) {
	if os.Getenv("EXC_REDIS_TEST") != "1" {
		t.Skip("EXC_REDIS_TEST not set")
	}
	addr := os.Getenv("EXC_REDIS_TEST_ADDR")
	if addr == "" {
		addr = "127.0.0.1:16379"
	}
	rdb := excredis.New(addr, os.Getenv("EXC_REDIS_TEST_PASSWORD"), 14)
	ctx := context.Background()
	if err := rdb.Ping(ctx); err != nil {
		t.Skipf("redis unreachable: %v", err)
	}
	defer func() { _ = rdb.Close() }()
	if err := rdb.FlushDB(ctx).Err(); err != nil {
		t.Fatalf("flushdb: %v", err)
	}

	q, err := NewLiquidationQueue(rdb, func(string, ...any) {})
	if err != nil {
		t.Fatal(err)
	}
	disp, err := NewQueueLiquidationDispatcher(q)
	if err != nil {
		t.Fatal(err)
	}
	f := newEngineStoreFake()
	seedCrossAccount(f, 7, "800", "200", 101)
	e, err := NewMarginEngine(MarginEngineDeps{
		Store:      f,
		Marks:      markCacheFake{m: map[string]decimal.Decimal{"EUR/USD": d("1.10")}},
		Dispatcher: disp, RedispatchInterval: time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.RefreshIndex(ctx); err != nil {
		t.Fatal(err)
	}
	if err := e.WarmAccount(ctx, 7); err != nil {
		t.Fatal(err)
	}
	// Two breaching ticks → ONE durable job (queue dedup coalesces).
	e.PushMark(ctx, MarkTick{Symbol: "EUR/USD", Price: d("1.05"), Ts: time.Now()})
	time.Sleep(5 * time.Millisecond) // pass the engine redispatch window
	e.PushMark(ctx, MarkTick{Symbol: "EUR/USD", Price: d("1.04"), Ts: time.Now()})
	n, err := q.PeekLen(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("queue depth %d — dedup must coalesce to 1", n)
	}
	job, err := q.Pop(ctx, time.Second)
	if err != nil || job == nil {
		t.Fatalf("pop: %v %+v", err, job)
	}
	if job.AccountID != 7 || job.Reason != LiquidationReasonStopOut {
		t.Fatalf("job %+v", job)
	}
	if job.Equity == "" || job.UsedMargin == "" || job.MarginLevelPct == "" {
		t.Fatalf("advisory fields missing: %+v", job)
	}
}

var gmark = d("1.25")
