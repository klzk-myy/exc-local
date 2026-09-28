package recovery

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"exchange/pkg/decimal"
	excerrors "exchange/pkg/errors"
)

// ---------------------------------------------------------------------------
// Fake clock: After advances the virtual clock and fires instantly, so the
// 120s feed deadline and 60s/5s ladder windows run deterministically.
// ---------------------------------------------------------------------------

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 9, 27, 0, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) After(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
	ch := make(chan time.Time, 1)
	ch <- c.now
	return ch
}

// ---------------------------------------------------------------------------
// Fakes
// ---------------------------------------------------------------------------

type orchGLRow struct {
	Journal  uint64
	Currency string
	Debit    decimal.Decimal
	Credit   decimal.Decimal
}

type orchDeltaRow struct {
	Journal     uint64
	AccountCode string
	Currency    string
	Net         decimal.Decimal
}

type fakeAuditData struct {
	glRows     []orchGLRow
	deltaRows  []orchDeltaRow
	journalSeq uint64
	bookSeq    uint64

	coverage   []OrchNostroCoverage
	negatives  []OrchNegativeBalance
	orphans    []uint64
	monotonic  bool
	marginViol []OrchMarginViolation
	omnibus    []OrchBankMovement

	mu      sync.Mutex
	glCalls []uint64 // recorded sinceJournalSeq values
	err     error    // if set, all data calls fail
}

func (f *fakeAuditData) failErr() error { return f.err }

func (f *fakeAuditData) GLSums(_ context.Context, _ int, since uint64) ([]OrchGLSum, error) {
	f.mu.Lock()
	f.glCalls = append(f.glCalls, since)
	f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	type acc struct{ d, c decimal.Decimal }
	agg := map[string]*acc{}
	for _, r := range f.glRows {
		if r.Journal <= since {
			continue
		}
		a, ok := agg[r.Currency]
		if !ok {
			a = &acc{decimal.Zero, decimal.Zero}
			agg[r.Currency] = a
		}
		a.d = a.d.Add(r.Debit)
		a.c = a.c.Add(r.Credit)
	}
	var out []OrchGLSum
	for ccy, a := range agg {
		out = append(out, OrchGLSum{Currency: ccy, Debits: a.d, Credits: a.c})
	}
	return out, nil
}

func (f *fakeAuditData) BalanceDeltas(_ context.Context, _ int, since uint64) ([]OrchBalanceDelta, error) {
	if f.err != nil {
		return nil, f.err
	}
	type key struct{ code, ccy string }
	agg := map[key]decimal.Decimal{}
	for _, r := range f.deltaRows {
		if r.Journal <= since {
			continue
		}
		k := key{r.AccountCode, r.Currency}
		agg[k] = agg[k].Add(r.Net)
	}
	var out []OrchBalanceDelta
	for k, v := range agg {
		out = append(out, OrchBalanceDelta{AccountCode: k.code, Currency: k.ccy, Net: v})
	}
	return out, nil
}

func (f *fakeAuditData) BookSeqWatermark(context.Context, int) (uint64, error) {
	return f.bookSeq, f.err
}
func (f *fakeAuditData) JournalWatermark(context.Context, int) (uint64, error) {
	return f.journalSeq, f.err
}
func (f *fakeAuditData) NostroCoverage(context.Context, int) ([]OrchNostroCoverage, error) {
	return f.coverage, f.err
}
func (f *fakeAuditData) NegativeBalances(context.Context, int) ([]OrchNegativeBalance, error) {
	return f.negatives, f.err
}
func (f *fakeAuditData) ExecutionChain(context.Context, int) ([]uint64, bool, error) {
	return f.orphans, f.monotonic, f.err
}
func (f *fakeAuditData) MarginViolations(context.Context, int) ([]OrchMarginViolation, error) {
	return f.marginViol, f.err
}
func (f *fakeAuditData) OmnibusMovements(context.Context, int) ([]OrchBankMovement, error) {
	return f.omnibus, f.err
}

type fakeTelemetry struct {
	book, wal map[int]uint64
	err       error
}

func (f *fakeTelemetry) BookSeqVsWALTail(_ context.Context, shard int) (uint64, uint64, error) {
	if f.err != nil {
		return 0, 0, f.err
	}
	return f.book[shard], f.wal[shard], nil
}

// fakeFeed returns err every call until callsLeft hits 0, then succeeds.
type fakeBankFeed struct {
	mu         sync.Mutex
	unreachN   int // number of leading unreachable failures
	movements  []OrchBankMovement
	calls      int
	alwaysDown bool
}

func (f *fakeBankFeed) IntradayMovements(context.Context, int) ([]OrchBankMovement, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.alwaysDown || f.calls <= f.unreachN {
		return nil, fmt.Errorf("mt942 feed: %w", OrchErrFeedUnreachable)
	}
	return f.movements, nil
}

type fakeCLSFeed struct {
	mu         sync.Mutex
	legs       []OrchCLSLeg
	alwaysDown bool
	calls      int
}

func (f *fakeCLSFeed) SettlementFinality(context.Context, int) ([]OrchCLSLeg, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.alwaysDown {
		return nil, fmt.Errorf("cls feed: %w", OrchErrFeedUnreachable)
	}
	return f.legs, nil
}

type fakeLeases struct {
	mu      sync.Mutex
	values  map[int]string
	revoked []string
	acq     map[int]string // last acquired value
}

func newFakeLeases() *fakeLeases {
	return &fakeLeases{values: map[int]string{}, acq: map[int]string{}}
}

func (f *fakeLeases) LeaderValue(_ context.Context, shard int) (string, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.values[shard]
	return v, ok, nil
}

func (f *fakeLeases) RevokeLeader(_ context.Context, shard int, expect string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.values[shard] != expect {
		return false, nil
	}
	delete(f.values, shard)
	f.revoked = append(f.revoked, expect)
	return true, nil
}

func (f *fakeLeases) AcquireLeader(_ context.Context, shard int, token string, epoch uint64, _ time.Duration) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, taken := f.values[shard]; taken {
		return false, nil
	}
	f.values[shard] = fmt.Sprintf("%s:%d", token, epoch)
	f.acq[shard] = f.values[shard]
	return true, nil
}

type fakeStandbys struct{ token string }

func (f fakeStandbys) NextStandby(context.Context, int) (string, error) { return f.token, nil }

type fakeLiveness struct{ alive map[string]bool }

func (f fakeLiveness) LeaderAlive(_ context.Context, _ int, token string) (bool, error) {
	return f.alive[token], nil
}

type fakeFlusher struct {
	mu      sync.Mutex
	flushed []int
	err     error
}

func (f *fakeFlusher) FlushDirtyBlocks(_ context.Context, shard int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.flushed = append(f.flushed, shard)
	return f.err
}

type fakeDR struct {
	lag      time.Duration
	master   string
	replayed map[int]uint64
	tails    map[int]uint64
	err      error
	steps    []string
}

func (f *fakeDR) VerifyPGSemiSync(context.Context) (time.Duration, error) {
	f.steps = append(f.steps, "pg")
	return f.lag, f.err
}
func (f *fakeDR) PromoteRedisSecondary(context.Context) (string, error) {
	f.steps = append(f.steps, "redis")
	return f.master, f.err
}
func (f *fakeDR) ReplayWALArchive(_ context.Context, shard int, from uint64) (uint64, error) {
	f.steps = append(f.steps, fmt.Sprintf("wal%d", shard))
	if f.err != nil {
		return 0, f.err
	}
	return f.replayed[shard] + from, nil
}
func (f *fakeDR) SecondaryWALTail(_ context.Context, shard int) (uint64, error) {
	return f.tails[shard], nil
}

type fakeReports struct {
	mu   sync.Mutex
	rows []OrchRecoveryReport
}

func (f *fakeReports) Record(_ context.Context, r OrchRecoveryReport) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rows = append(f.rows, r)
	return nil
}

type fakeAlerts struct {
	mu  sync.Mutex
	p1s []string
}

func (f *fakeAlerts) RaiseP1(_ context.Context, summary string, _ map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.p1s = append(f.p1s, summary)
}

type fakeSuspense struct {
	mu      sync.Mutex
	flagged []int
}

func (f *fakeSuspense) FlagSuspense(_ context.Context, shard int, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.flagged = append(f.flagged, shard)
	return nil
}

type fakeRecon struct {
	mu        sync.Mutex
	scheduled []OrchStageID
}

func (f *fakeRecon) SchedulePostOpenReconcile(_ context.Context, _ int, stage OrchStageID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.scheduled = append(f.scheduled, stage)
	return nil
}

type fakeFix struct {
	mu       sync.Mutex
	statuses []OrchSessionStatus
	gapFills int
}

func (f *fakeFix) BroadcastTradingSessionStatus(_ context.Context, _ int, s OrchSessionStatus) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.statuses = append(f.statuses, s)
	return nil
}
func (f *fakeFix) ResolveGaps(context.Context, int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gapFills++
	return nil
}

type fakeWS struct {
	mu      sync.Mutex
	resumed []int
}

func (f *fakeWS) ResumeClients(_ context.Context, shard int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resumed = append(f.resumed, shard)
	return nil
}

// ---------------------------------------------------------------------------
// Builders
// ---------------------------------------------------------------------------

func dec(s string) decimal.Decimal { return decimal.MustFromString(s) }

// healthyData returns a fully clean audit fixture for shard tests.
func healthyData() *fakeAuditData {
	return &fakeAuditData{
		glRows: []orchGLRow{
			{Journal: 10, Currency: "USD", Debit: dec("100"), Credit: dec("0")},
			{Journal: 10, Currency: "USD", Debit: dec("0"), Credit: dec("100")},
		},
		journalSeq: 10,
		coverage: []OrchNostroCoverage{
			{Currency: "USD", Required: dec("500"), NostroCash: dec("500")},
		},
		monotonic: true,
		omnibus: []OrchBankMovement{
			{Ref: "R1", Currency: "USD", Amount: dec("10"), Direction: "CREDIT"},
		},
	}
}

// orchFeeds bundles the two external feeds so builders take one arg.
type orchFeeds struct {
	bank OrchBankFeed
	cls  OrchCLSFeed
}

func healthyFeeds() orchFeeds {
	return orchFeeds{
		bank: &fakeBankFeed{movements: []OrchBankMovement{
			{Ref: "R1", Currency: "USD", Amount: dec("10"), Direction: "CREDIT"},
		}},
		cls: &fakeCLSFeed{legs: []OrchCLSLeg{
			{SettlementID: "CLS1", Currency: "USD", Status: "SETTLED"},
		}},
	}
}

func newTestEngine(clk *fakeClock, data OrchAuditData, tel *fakeTelemetry,
	f orchFeeds, extras ...func(*OrchAuditConfig)) *OrchAuditEngine {
	cfg := OrchAuditConfig{
		Data:     data,
		Engine:   tel,
		Bank:     f.bank,
		CLS:      f.cls,
		Now:      clk.Now,
		After:    clk.After,
		FeedPoll: time.Second,
	}
	for _, x := range extras {
		x(&cfg)
	}
	return NewOrchAuditEngine(cfg)
}

func newTestOrch(t *testing.T, clk *fakeClock, shards []int,
	eng *OrchAuditEngine, deps ...func(*OrchDeps)) *RecoveryOrchestrator {
	t.Helper()
	d := OrchDeps{
		Audit: eng,
		Now:   clk.Now,
		After: clk.After,
	}
	for _, x := range deps {
		x(&d)
	}
	o, err := NewRecoveryOrchestrator(OrchConfig{Shards: shards}, d)
	if err != nil {
		t.Fatalf("orchestrator: %v", err)
	}
	return o
}

func stageByID(a *OrchShardAudit, id OrchStageID) OrchStageResult {
	for _, s := range a.Stages {
		if s.Stage == id {
			return s
		}
	}
	return OrchStageResult{Stage: id, Verdict: "MISSING"}
}

// ---------------------------------------------------------------------------
// 6-stage engine: all-pass path
// ---------------------------------------------------------------------------

func TestAuditAllStagesPass(t *testing.T) {
	clk := newFakeClock()
	f := healthyFeeds()
	eng := newTestEngine(clk, healthyData(), &fakeTelemetry{
		book: map[int]uint64{0: 42}, wal: map[int]uint64{0: 42},
	}, f)

	a := eng.AuditShard(context.Background(), 0)
	if a.Fail || a.HardFail || a.FallbackUsed {
		t.Fatalf("clean fixture failed audit: %+v", a)
	}
	for _, id := range []OrchStageID{
		OrchStageLedgerZeroSum, OrchStageNonNegative, OrchStageBookWALSeq,
		OrchStageMonotonic, OrchStageNostroRecon, OrchStageCLSFinality, OrchStageMargin,
	} {
		if r := stageByID(a, id); r.Verdict != OrchVerdictPass {
			t.Fatalf("stage %s verdict %s err %v", id, r.Verdict, r.Err)
		}
	}
	if f.bank.(*fakeBankFeed).calls == 0 || f.cls.(*fakeCLSFeed).calls == 0 {
		t.Fatal("external feeds were not consulted")
	}
}

// ---------------------------------------------------------------------------
// Fail paths: each spec stage fails closed
// ---------------------------------------------------------------------------

func TestAuditLedgerImbalanceFreeze(t *testing.T) {
	clk := newFakeClock()
	data := healthyData()
	data.glRows = append(data.glRows,
		orchGLRow{Journal: 11, Currency: "USD", Debit: dec("7"), Credit: dec("0")})
	data.journalSeq = 11
	f := healthyFeeds()
	reports := &fakeReports{}
	eng := newTestEngine(clk, data, &fakeTelemetry{
		book: map[int]uint64{0: 1}, wal: map[int]uint64{0: 1},
	}, f, func(c *OrchAuditConfig) { c.Reports = reports })

	a := eng.AuditShard(context.Background(), 0)
	if !a.Fail || !a.HardFail {
		t.Fatalf("ledger imbalance must fail hard: %+v", a)
	}
	r := stageByID(a, OrchStageLedgerZeroSum)
	if r.Verdict != OrchVerdictFail {
		t.Fatalf("stage1 verdict %s", r.Verdict)
	}
	var coded *excerrors.Error
	if !errors.As(r.Err, &coded) || coded.Code != OrchCodeLedgerImbalanceAbort {
		t.Fatalf("want LEDGER_IMBALANCE_ABORT, got %v", r.Err)
	}
	// Imbalance must be recorded to recovery_reports.
	found := false
	for _, row := range reports.rows {
		if row.Stage == string(OrchStageLedgerZeroSum) && row.Outcome == string(OrchVerdictFail) {
			found = true
		}
	}
	if !found {
		t.Fatal("no recovery_reports row for the imbalance")
	}
}

func TestAuditNegativeBalanceFails(t *testing.T) {
	clk := newFakeClock()
	data := healthyData()
	data.negatives = []OrchNegativeBalance{{AccountID: 7, Currency: "USD", Total: dec("-1")}}
	f := healthyFeeds()
	eng := newTestEngine(clk, data, &fakeTelemetry{
		book: map[int]uint64{0: 1}, wal: map[int]uint64{0: 1},
	}, f)
	a := eng.AuditShard(context.Background(), 0)
	if !a.Fail || a.HardFail {
		t.Fatalf("neg-balance is a normal (non-hard) fail: %+v", a)
	}
	if stageByID(a, OrchStageNonNegative).Verdict != OrchVerdictFail {
		t.Fatal("stage 2 should fail")
	}
}

func TestAuditBookWALMismatchFails(t *testing.T) {
	clk := newFakeClock()
	f := healthyFeeds()
	eng := newTestEngine(clk, healthyData(), &fakeTelemetry{
		book: map[int]uint64{0: 100}, wal: map[int]uint64{0: 101},
	}, f)
	a := eng.AuditShard(context.Background(), 0)
	if !a.Fail || stageByID(a, OrchStageBookWALSeq).Verdict != OrchVerdictFail {
		t.Fatalf("book/wal divergence must fail: %+v", a)
	}
}

func TestAuditOrphanTradeFails(t *testing.T) {
	clk := newFakeClock()
	data := healthyData()
	data.orphans = []uint64{991}
	f := healthyFeeds()
	eng := newTestEngine(clk, data, &fakeTelemetry{
		book: map[int]uint64{0: 1}, wal: map[int]uint64{0: 1},
	}, f)
	a := eng.AuditShard(context.Background(), 0)
	if !a.Fail || stageByID(a, OrchStageMonotonic).Verdict != OrchVerdictFail {
		t.Fatalf("orphan trade must fail: %+v", a)
	}
}

func TestAuditNostroMismatchFails(t *testing.T) {
	clk := newFakeClock()
	// Bank feed shows a movement the omnibus never recorded.
	f := orchFeeds{
		bank: &fakeBankFeed{movements: []OrchBankMovement{
			{Ref: "R1", Currency: "USD", Amount: dec("10"), Direction: "CREDIT"},
			{Ref: "R9", Currency: "USD", Amount: dec("999"), Direction: "DEBIT"},
		}},
		cls: healthyFeeds().cls,
	}
	eng := newTestEngine(clk, healthyData(), &fakeTelemetry{
		book: map[int]uint64{0: 1}, wal: map[int]uint64{0: 1},
	}, f)
	a := eng.AuditShard(context.Background(), 0)
	if !a.Fail || stageByID(a, OrchStageNostroRecon).Verdict != OrchVerdictFail {
		t.Fatalf("nostro mismatch must fail (data arrived inconsistent): %+v", a)
	}
}

func TestAuditCLSUnfinalFails(t *testing.T) {
	clk := newFakeClock()
	f := orchFeeds{
		bank: healthyFeeds().bank,
		cls: &fakeCLSFeed{legs: []OrchCLSLeg{
			{SettlementID: "CLS9", Currency: "USD", Status: "PENDING"},
		}},
	}
	eng := newTestEngine(clk, healthyData(), &fakeTelemetry{
		book: map[int]uint64{0: 1}, wal: map[int]uint64{0: 1},
	}, f)
	a := eng.AuditShard(context.Background(), 0)
	if !a.Fail || stageByID(a, OrchStageCLSFinality).Verdict != OrchVerdictFail {
		t.Fatalf("unfinal CLS leg must fail: %+v", a)
	}
}

// ---------------------------------------------------------------------------
// 120s feed deferral (4.3.12)
// ---------------------------------------------------------------------------

func TestAuditFeedDeferral(t *testing.T) {
	clk := newFakeClock()
	f := orchFeeds{
		bank: &fakeBankFeed{alwaysDown: true}, // MT942 unreachable for the whole window
		cls: &fakeCLSFeed{legs: []OrchCLSLeg{
			{SettlementID: "CLS1", Status: "SETTLED"},
		}},
	}
	reports := &fakeReports{}
	susp := &fakeSuspense{}
	recon := &fakeRecon{}
	eng := newTestEngine(clk, healthyData(), &fakeTelemetry{
		book: map[int]uint64{0: 1}, wal: map[int]uint64{0: 1},
	}, f, func(c *OrchAuditConfig) {
		c.Reports = reports
		c.Suspense = susp
		c.Recon = recon
	})

	a := eng.AuditShard(context.Background(), 0)
	if a.Fail {
		t.Fatalf("feed outage must defer, not fail: %+v", a)
	}
	if !a.FallbackUsed {
		t.Fatal("FallbackUsed not set on deferral")
	}
	r := stageByID(a, OrchStageNostroRecon)
	if r.Verdict != OrchVerdictDeferred {
		t.Fatalf("nostro stage verdict %s", r.Verdict)
	}
	// The poll loop ran until the 120s deadline.
	if clk.Now().Sub(time.Date(2026, 9, 27, 0, 0, 0, 0, time.UTC)) < OrchFeedDeadline {
		t.Fatal("deferral happened before the 120s deadline")
	}
	if len(susp.flagged) != 1 || susp.flagged[0] != 0 {
		t.Fatal("nostro suspense not flagged")
	}
	if len(recon.scheduled) != 1 || recon.scheduled[0] != OrchStageNostroRecon {
		t.Fatal("post-open reconcile not scheduled")
	}
	found := false
	for _, row := range reports.rows {
		if row.Stage == string(OrchStageNostroRecon) && row.Outcome == string(OrchVerdictDeferred) {
			found = true
		}
	}
	if !found {
		t.Fatal("deferral not recorded in recovery_reports")
	}
}

// ---------------------------------------------------------------------------
// Zero-sum exemption: ledger imbalance freezes even when feeds also defer
// ---------------------------------------------------------------------------

func TestAuditZeroSumExemptFromFallback(t *testing.T) {
	clk := newFakeClock()
	data := healthyData()
	data.glRows = append(data.glRows,
		orchGLRow{Journal: 11, Currency: "EUR", Debit: dec("3"), Credit: dec("0")})
	f := orchFeeds{
		bank: &fakeBankFeed{alwaysDown: true},
		cls:  &fakeCLSFeed{alwaysDown: true},
	}
	eng := newTestEngine(clk, data, &fakeTelemetry{
		book: map[int]uint64{0: 1}, wal: map[int]uint64{0: 1},
	}, f)

	a := eng.AuditShard(context.Background(), 0)
	if !a.Fail || !a.HardFail {
		t.Fatalf("zero-sum failure must hard-fail even with feeds down: %+v", a)
	}
	if stageByID(a, OrchStageLedgerZeroSum).Verdict != OrchVerdictFail {
		t.Fatal("stage 1 must FAIL, never defer")
	}
	// Feed stages still deferred — only the verdict rollup freezes.
	if stageByID(a, OrchStageNostroRecon).Verdict != OrchVerdictDeferred {
		t.Fatal("nostro should have deferred")
	}
}

// ---------------------------------------------------------------------------
// Stage overrun → fail closed + P1 (4.3.11)
// ---------------------------------------------------------------------------

// blockingData stalls GLSums past its budget using a real sleep.
type blockingData struct {
	*fakeAuditData
	block time.Duration
}

func (b *blockingData) GLSums(ctx context.Context, shard int, since uint64) ([]OrchGLSum, error) {
	// Deliberately ignores ctx: the runStage budget must fire while the
	// provider is still blocked — a provider that aborts on ctx.Done would
	// race the budget branch and mask the overrun semantics under test.
	time.Sleep(b.block)
	return b.fakeAuditData.GLSums(ctx, shard, since)
}

func TestAuditStageOverrunFailsClosed(t *testing.T) {
	clk := newFakeClock()
	data := &blockingData{fakeAuditData: healthyData(), block: 300 * time.Millisecond}
	f := healthyFeeds()
	alerts := &fakeAlerts{}
	eng := newTestEngine(clk, data, &fakeTelemetry{
		book: map[int]uint64{0: 1}, wal: map[int]uint64{0: 1},
	}, f, func(c *OrchAuditConfig) {
		c.Budgets = OrchDefaultStageBudgets()
		c.Budgets.Ledger = 30 * time.Millisecond // force overrun with real clock
		c.Alerts = alerts
		// runStage uses context timeouts (real time) — fake clock only
		// drives feed waits; the budget overrun fires via sctx.Done().
	})
	a := eng.AuditShard(context.Background(), 0)
	if !a.Fail {
		t.Fatal("overrun must fail closed")
	}
	r := stageByID(a, OrchStageLedgerZeroSum)
	if !r.Overrun {
		t.Fatalf("stage not marked overrun: %+v", r)
	}
	if len(alerts.p1s) == 0 {
		t.Fatal("no P1 raised for stage overrun")
	}
}

// ---------------------------------------------------------------------------
// Digest fast path + shard-local fallback (4.3.11)
// ---------------------------------------------------------------------------

func TestAuditDigestFastPathAndFallback(t *testing.T) {
	clk := newFakeClock()
	data := healthyData()
	data.journalSeq = 2000
	// Window [1001..2000] activity the digest should certify.
	data.glRows = append(data.glRows,
		orchGLRow{Journal: 1500, Currency: "USD", Debit: dec("5"), Credit: dec("0")},
		orchGLRow{Journal: 1500, Currency: "USD", Debit: dec("0"), Credit: dec("5")})
	data.deltaRows = []orchDeltaRow{
		{Journal: 1500, AccountCode: "2010_CUSTOMER_LIABILITY_USD", Currency: "USD", Net: dec("5")},
	}
	data.bookSeq = 77

	store := NewOrchMemDigestStore()
	cp := &OrchCheckpointer{Store: store, Data: data, Now: clk.Now}

	// Checkpoint at 1000 trades covering window (0..1000], then 2000.
	for _, tc := range []uint64{1000, 2000} {
		// book seq/journal watermark advance between checkpoints.
		d, written, err := cp.MaybeCheckpoint(context.Background(), 0, tc)
		if err != nil {
			t.Fatalf("checkpoint %d: %v", tc, err)
		}
		if !written {
			t.Fatalf("checkpoint %d not written", tc)
		}
		if d.CheckpointSeq != tc {
			t.Fatalf("checkpoint seq %d", d.CheckpointSeq)
		}
		data.journalSeq += 0 // watermark already at 2000 — digest 2 covers (1000.journal? )
	}

	// Non-boundary trade count must not write.
	if _, written, err := cp.MaybeCheckpoint(context.Background(), 0, 2500); err != nil || written {
		t.Fatal("non-boundary checkpoint written")
	}

	f := healthyFeeds()
	eng := newTestEngine(clk, data, &fakeTelemetry{
		book: map[int]uint64{0: 1}, wal: map[int]uint64{0: 1},
	}, f, func(c *OrchAuditConfig) { c.Digests = store })

	data.glCalls = nil
	a := eng.AuditShard(context.Background(), 0)
	if a.Fail {
		t.Fatalf("digest-verified audit failed: %+v", a)
	}
	r := stageByID(a, OrchStageLedgerZeroSum)
	if fb, _ := r.Detail["digest_fallback"].(bool); fb {
		t.Fatalf("valid digest must not trigger fallback: %+v", r.Detail)
	}
	// GL scan must have been windowed at the digest's journal_seq (2000),
	// i.e. the post-checkpoint tail only — not a full scan.
	usedWindow := false
	for _, since := range data.glCalls {
		if since == 2000 {
			usedWindow = true
		}
	}
	if !usedWindow {
		t.Fatalf("expected windowed scan since=2000, got %v", data.glCalls)
	}
}

func TestAuditDigestMismatchFullScanFallback(t *testing.T) {
	clk := newFakeClock()
	data := healthyData()
	data.journalSeq = 10
	store := NewOrchMemDigestStore()
	// Plant a digest whose hash does NOT match the recomputed window.
	bad := OrchDigest{
		ShardID:          0,
		CheckpointSeq:    1000,
		JournalSeq:       5,
		BookSeq:          9,
		GLZeroSumHash:    strings.Repeat("ab", 32),
		BalanceDeltaHash: strings.Repeat("cd", 32),
		TradeCount:       1000,
	}
	if err := store.StoreDigest(context.Background(), bad); err != nil {
		t.Fatal(err)
	}
	f := healthyFeeds()
	eng := newTestEngine(clk, data, &fakeTelemetry{
		book: map[int]uint64{0: 1}, wal: map[int]uint64{0: 1},
	}, f, func(c *OrchAuditConfig) { c.Digests = store })

	data.glCalls = nil
	a := eng.AuditShard(context.Background(), 0)
	if a.Fail {
		t.Fatalf("healthy data should pass even on digest fallback: %+v", a)
	}
	if !a.DigestFallback {
		t.Fatal("digest mismatch did not flag shard-local fallback")
	}
	fullScan := false
	for _, since := range data.glCalls {
		if since == 0 {
			fullScan = true
		}
	}
	if !fullScan {
		t.Fatalf("fallback must full-scan (since=0), got %v", data.glCalls)
	}
}

// ---------------------------------------------------------------------------
// Fencing & promotion (4.3.10 step 1)
// ---------------------------------------------------------------------------

func TestLeaderEpochVerification(t *testing.T) {
	clk := newFakeClock()
	leases := newFakeLeases()
	leases.values[0] = "node-a:5"
	o := newTestOrch(t, clk, []int{0},
		newTestEngine(clk, healthyData(), &fakeTelemetry{
			book: map[int]uint64{0: 1}, wal: map[int]uint64{0: 1},
		}, healthyFeeds()),
		func(d *OrchDeps) { d.Leases = leases })

	chk, err := o.VerifyLeaderEpoch(context.Background(), 0, 5)
	if err != nil || !chk.Matches || !chk.LeasePresent || chk.Epoch != 5 {
		t.Fatalf("epoch check: %+v err %v", chk, err)
	}
	chk, err = o.VerifyLeaderEpoch(context.Background(), 0, 4)
	if err != nil || chk.Matches {
		t.Fatalf("epoch_local != epoch_current must not match: %+v", chk)
	}
}

func TestFenceAndPromoteDeadLeader(t *testing.T) {
	clk := newFakeClock()
	leases := newFakeLeases()
	leases.values[0] = "dead-node:3"
	o := newTestOrch(t, clk, []int{0},
		newTestEngine(clk, healthyData(), &fakeTelemetry{
			book: map[int]uint64{0: 1}, wal: map[int]uint64{0: 1},
		}, healthyFeeds()),
		func(d *OrchDeps) {
			d.Leases = leases
			d.Standbys = fakeStandbys{token: "warm-b"}
			d.Liveness = fakeLiveness{alive: map[string]bool{}}
		})

	p, err := o.FenceAndPromote(context.Background(), 0)
	if err != nil {
		t.Fatalf("promote: %v", err)
	}
	if !p.Promoted || !p.RevokedStale || p.Token != "warm-b" || p.Epoch != 4 {
		t.Fatalf("bad promotion: %+v", p)
	}
	if !p.WithinRTO {
		t.Fatal("promotion exceeded RTO")
	}
	if leases.values[0] != "warm-b:4" {
		t.Fatalf("lease not held by standby: %q", leases.values[0])
	}
}

func TestFenceSkipsLiveLeader(t *testing.T) {
	clk := newFakeClock()
	leases := newFakeLeases()
	leases.values[0] = "live-node:3"
	o := newTestOrch(t, clk, []int{0},
		newTestEngine(clk, healthyData(), &fakeTelemetry{
			book: map[int]uint64{0: 1}, wal: map[int]uint64{0: 1},
		}, healthyFeeds()),
		func(d *OrchDeps) {
			d.Leases = leases
			d.Standbys = fakeStandbys{token: "warm-b"}
			d.Liveness = fakeLiveness{alive: map[string]bool{"live-node": true}}
		})
	p, err := o.FenceAndPromote(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if p.Promoted || p.RevokedStale {
		t.Fatalf("live leader must not be fenced: %+v", p)
	}
	if leases.values[0] != "live-node:3" {
		t.Fatal("live leader's lease was disturbed")
	}
}

// ---------------------------------------------------------------------------
// Reopen ladder: CANCEL_ONLY grace → auction → NORMAL (4.3.10 step 4)
// ---------------------------------------------------------------------------

func TestReopenLadderAndAdmissionGate(t *testing.T) {
	clk := newFakeClock()
	f := healthyFeeds()
	fix := &fakeFix{}
	ws := &fakeWS{}
	var transitions []OrchShardState
	var mu sync.Mutex
	o := newTestOrch(t, clk, []int{0},
		newTestEngine(clk, healthyData(), &fakeTelemetry{
			book: map[int]uint64{0: 1}, wal: map[int]uint64{0: 1},
		}, f),
		func(d *OrchDeps) {
			d.Fix = fix
			d.WS = ws
			d.OnStateChange = func(_ int, _, to OrchShardState) {
				mu.Lock()
				transitions = append(transitions, to)
				mu.Unlock()
			}
		})

	// While DOWN (pre-audit), new orders are rejected SERVICE_DEGRADED.
	if err := o.AdmitOrder(0, OrchOrderAdmission{}); err == nil {
		t.Fatal("order admitted while DOWN")
	} else {
		var e *excerrors.Error
		if !errors.As(err, &e) || e.Code != OrchCodeServiceDegraded {
			t.Fatalf("want SERVICE_DEGRADED, got %v", err)
		}
	}

	v, err := o.ReopenShard(context.Background(), 0)
	if err != nil || v != OrchVerdictReopen {
		t.Fatalf("reopen: verdict %s err %v", v, err)
	}
	if got := o.State(0); got != OrchStateNormal {
		t.Fatalf("state %s after reopen", got)
	}

	mu.Lock()
	seq := append([]OrchShardState(nil), transitions...)
	mu.Unlock()
	want := []OrchShardState{OrchStateAuditing, OrchStateCancelOnly, OrchStateAuction, OrchStateNormal}
	if len(seq) != len(want) {
		t.Fatalf("transitions %v want %v", seq, want)
	}
	for i := range want {
		if seq[i] != want[i] {
			t.Fatalf("transitions %v want %v", seq, want)
		}
	}
	// FIX broadcasts: PRE_OPEN, AUCTION, OPEN.
	if len(fix.statuses) != 3 ||
		fix.statuses[0] != OrchSessionPreOpen ||
		fix.statuses[1] != OrchSessionAuction ||
		fix.statuses[2] != OrchSessionOpen {
		t.Fatalf("fix broadcasts: %v", fix.statuses)
	}
	if fix.gapFills != 1 || len(ws.resumed) != 1 {
		t.Fatal("resync seams not invoked")
	}
	// Virtual clock advanced by the full ladder: 60s grace + 5s auction.
	if clk.Now().Sub(time.Date(2026, 9, 27, 0, 0, 0, 0, time.UTC)) != 65*time.Second {
		t.Fatalf("ladder did not spend the grace+auction windows; clock advanced %s",
			clk.Now().Sub(time.Date(2026, 9, 27, 0, 0, 0, 0, time.UTC)))
	}
}

func TestCancelOnlyRejectsNewOrders(t *testing.T) {
	clk := newFakeClock()
	o := newTestOrch(t, clk, []int{0},
		newTestEngine(clk, healthyData(), &fakeTelemetry{
			book: map[int]uint64{0: 1}, wal: map[int]uint64{0: 1},
		}, healthyFeeds()))
	o.setState(0, OrchStateCancelOnly)

	err := o.AdmitOrder(0, OrchOrderAdmission{IsCancel: false})
	var e *excerrors.Error
	if !errors.As(err, &e) || e.Code != OrchCodeCancelOnly {
		t.Fatalf("want ORDER_REJECTED_CANCEL_ONLY_MODE, got %v", err)
	}
	if err := o.AdmitOrder(0, OrchOrderAdmission{IsCancel: true}); err != nil {
		t.Fatalf("cancel must pass in CANCEL_ONLY: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Per-shard scoped reopen (4.3.12)
// ---------------------------------------------------------------------------

func TestPerShardScopedReopen(t *testing.T) {
	clk := newFakeClock()
	// Shard 1's book diverged from WAL; shards 0,2 healthy.
	f := healthyFeeds()
	tel := &fakeTelemetry{
		book: map[int]uint64{0: 5, 1: 90, 2: 7},
		wal:  map[int]uint64{0: 5, 1: 91, 2: 7},
	}
	o := newTestOrch(t, clk, []int{0, 1, 2},
		newTestEngine(clk, healthyData(), tel, f))

	res := o.ReopenAll(context.Background())
	if len(res.ShardErrs) != 0 {
		t.Fatalf("shard errors: %v", res.ShardErrs)
	}
	if len(res.Frozen) != 1 || res.Frozen[0] != 1 {
		t.Fatalf("frozen shards: %v", res.Frozen)
	}
	if len(res.Reopened) != 2 || res.Reopened[0] != 0 || res.Reopened[1] != 2 {
		t.Fatalf("reopened shards: %v", res.Reopened)
	}
	if o.State(1) != OrchStateFrozen {
		t.Fatalf("shard 1 state %s", o.State(1))
	}
	if o.State(0) != OrchStateNormal || o.State(2) != OrchStateNormal {
		t.Fatal("healthy shards did not reach NORMAL")
	}

	// Cross-shard basket touching the frozen shard → SERVICE_DEGRADED.
	err := o.AdmitOrder(0, OrchOrderAdmission{BasketShards: []int{0, 1}})
	var e *excerrors.Error
	if !errors.As(err, &e) || e.Code != OrchCodeServiceDegraded {
		t.Fatalf("basket to frozen shard must degrade: %v", err)
	}
	// Basket over healthy shards passes.
	if err := o.AdmitOrder(0, OrchOrderAdmission{BasketShards: []int{0, 2}}); err != nil {
		t.Fatalf("healthy basket rejected: %v", err)
	}
	// Direct order on frozen shard rejected.
	if err := o.AdmitOrder(1, OrchOrderAdmission{IsCancel: true}); err == nil {
		t.Fatal("frozen shard accepted traffic")
	}
}

// ---------------------------------------------------------------------------
// DR failover orchestration (4.3.10 step 2)
// ---------------------------------------------------------------------------

func TestDRFailoverSequence(t *testing.T) {
	clk := newFakeClock()
	dr := &fakeDR{
		lag:      2 * time.Second, // within RPO ≤15s
		master:   "10.0.0.9:6379",
		replayed: map[int]uint64{0: 40, 1: 55},
		tails:    map[int]uint64{0: 10, 1: 12},
	}
	o := newTestOrch(t, clk, []int{0, 1},
		newTestEngine(clk, healthyData(), &fakeTelemetry{
			book: map[int]uint64{0: 1, 1: 1}, wal: map[int]uint64{0: 1, 1: 1},
		}, healthyFeeds()),
		func(d *OrchDeps) { d.DR = dr })

	rep, err := o.RunDRFailover(context.Background())
	if err != nil {
		t.Fatalf("dr failover: %v", err)
	}
	if !rep.WithinRTO {
		t.Fatal("DR exceeded RTO")
	}
	// Ordered steps: pg verify, sentinel promote, wal replay per shard.
	if len(rep.Steps) != 4 ||
		rep.Steps[0].Name != "pg_semisync_verify" ||
		rep.Steps[1].Name != "redis_sentinel_promotion" ||
		rep.Steps[2].Name != "wal_archive_replay_shard_0" ||
		rep.Steps[3].Name != "wal_archive_replay_shard_1" {
		t.Fatalf("dr steps: %+v", rep.Steps)
	}
}

func TestDRFailoverAbortsOnRPOBreach(t *testing.T) {
	clk := newFakeClock()
	dr := &fakeDR{lag: 20 * time.Second} // > RPO 15s
	o := newTestOrch(t, clk, []int{0},
		newTestEngine(clk, healthyData(), &fakeTelemetry{
			book: map[int]uint64{0: 1}, wal: map[int]uint64{0: 1},
		}, healthyFeeds()),
		func(d *OrchDeps) { d.DR = dr })
	_, err := o.RunDRFailover(context.Background())
	if err == nil {
		t.Fatal("unsafe promotion allowed")
	}
	if len(dr.steps) != 1 {
		t.Fatalf("failover must abort before sentinel/wal steps: %v", dr.steps)
	}
}

// ---------------------------------------------------------------------------
// Shutdown: WAL dirty-block flush + lease release (4.3.10)
// ---------------------------------------------------------------------------

func TestShutdownFlushesWALAndReleasesLeases(t *testing.T) {
	clk := newFakeClock()
	leases := newFakeLeases()
	flusher := &fakeFlusher{}
	o := newTestOrch(t, clk, []int{0, 1},
		newTestEngine(clk, healthyData(), &fakeTelemetry{
			book: map[int]uint64{0: 1, 1: 1}, wal: map[int]uint64{0: 1, 1: 1},
		}, healthyFeeds()),
		func(d *OrchDeps) {
			d.Leases = leases
			d.Standbys = fakeStandbys{token: "warm-b"}
			d.Liveness = fakeLiveness{alive: map[string]bool{}}
			d.Flusher = flusher
		})
	if _, err := o.FenceAndPromote(context.Background(), 0); err != nil {
		t.Fatal(err)
	}
	if err := o.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	if len(flusher.flushed) != 2 {
		t.Fatalf("flushed shards: %v", flusher.flushed)
	}
	if _, ok := leases.values[0]; ok {
		t.Fatal("held lease not released on shutdown")
	}
}
