// balance_service_test.go — unit tests for Tasks 3.3.1 / 3.3.18 /
// 3.3.22(balance). All seams are in-memory; no Postgres/Redis required
// (integration coverage is the EXC_PG_TEST-gated suite pattern used by
// position_service_integration_test.go).
package settlement

import (
	"context"
	stderrors "errors"
	"sync"
	"testing"
	"time"

	flatbuffers "github.com/google/flatbuffers/go"
	"github.com/jackc/pgx/v5/pgconn"

	"exchange/internal/ipc"
	"exchange/internal/ledger"
	"exchange/pkg/decimal"
	excerrors "exchange/pkg/errors"
)

// ---------------------------------------------------------------------------
// Fakes
// ---------------------------------------------------------------------------

// fakeBalanceStore emulates the SERIALIZABLE tx seam: ops accumulate into
// a per-tx buffer and only merge on commit; queued commitFailures surface
// AFTER fn runs (commit-time serialization failure) and roll back.
type fakeBalanceStore struct {
	mu             sync.Mutex
	calls          int
	commits        int
	rollbacks      int
	commitFailures []error
	processed      map[int64]int64 // trade_id → shard_id
	posted         []ledger.Journal
	posterErr      error // injected PostJournal failure (in-tx abort)
}

func newFakeStore() *fakeBalanceStore {
	return &fakeBalanceStore{processed: map[int64]int64{}}
}

func (s *fakeBalanceStore) InTx(ctx context.Context, fn func(ctx context.Context, tx balanceTx) error) error {
	s.mu.Lock()
	s.calls++
	s.mu.Unlock()
	tx := &fakeBalanceTx{s: s, pending: map[int64]int64{}}
	if err := fn(ctx, tx); err != nil {
		s.mu.Lock()
		s.rollbacks++
		s.mu.Unlock()
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.commitFailures) > 0 {
		e := s.commitFailures[0]
		s.commitFailures = s.commitFailures[1:]
		s.rollbacks++
		return e
	}
	for id, sh := range tx.pending {
		s.processed[id] = sh
	}
	s.posted = append(s.posted, tx.posts...)
	s.commits++
	return nil
}

// committed reports the committed tx count (race-safe read for the
// consumer poll loop).
func (s *fakeBalanceStore) committed() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.commits
}

type fakeBalanceTx struct {
	s       *fakeBalanceStore
	pending map[int64]int64
	posts   []ledger.Journal
}

func (t *fakeBalanceTx) RecordProcessed(_ context.Context, tradeID, shardID int64) (bool, error) {
	t.s.mu.Lock()
	defer t.s.mu.Unlock()
	if _, ok := t.s.processed[tradeID]; ok {
		return false, nil
	}
	if _, ok := t.pending[tradeID]; ok {
		return false, nil
	}
	t.pending[tradeID] = shardID
	return true, nil
}

func (t *fakeBalanceTx) RecordTrade(_ context.Context, _ ResolvedTrade) error { return nil }

func (t *fakeBalanceTx) PostJournal(_ context.Context, j ledger.Journal) (ledger.PostResult, error) {
	if t.s.posterErr != nil {
		return ledger.PostResult{}, t.s.posterErr
	}
	if err := j.Validate(); err != nil {
		return ledger.PostResult{}, err
	}
	if err := j.ValidateAccounts(ledger.DefaultChart()); err != nil {
		return ledger.PostResult{}, err
	}
	t.posts = append(t.posts, j)
	return ledger.PostResult{JournalID: int64(len(t.posts))}, nil
}

type fakeLocker struct {
	busy        bool
	err         error
	lockCalls   int
	unlockCalls int
	lockedOrder []int64
}

func (l *fakeLocker) LockAccounts(_ context.Context, ids []int64, _ string) ([]int64, error) {
	l.lockCalls++
	if l.err != nil {
		return nil, l.err
	}
	if l.busy {
		return nil, excerrors.New(ledger.CodeAccountBusy, "account mutex held")
	}
	l.lockedOrder = append([]int64{}, ids...)
	return ids, nil
}
func (l *fakeLocker) UnlockAccounts(ids []int64, _ string) { l.unlockCalls++ }

type fakeDispatcher struct{ events []ledger.BalanceEvent }

func (d *fakeDispatcher) Dispatch(_ context.Context, evs []ledger.BalanceEvent) error {
	d.events = append(d.events, evs...)
	return nil
}

type fakeManagedPoster struct {
	journals []ledger.Journal
	err      error
}

func (p *fakeManagedPoster) Post(_ context.Context, j ledger.Journal) (ledger.PostResult, error) {
	if p.err != nil {
		return ledger.PostResult{}, p.err
	}
	if err := j.Validate(); err != nil {
		return ledger.PostResult{}, err
	}
	if err := j.ValidateAccounts(ledger.DefaultChart()); err != nil {
		return ledger.PostResult{}, err
	}
	p.journals = append(p.journals, j)
	return ledger.PostResult{JournalID: int64(len(p.journals)), Committed: true}, nil
}

type fakeAlerter struct{ alerts []OpsAlert }

func (a *fakeAlerter) Raise(_ context.Context, al OpsAlert) error {
	a.alerts = append(a.alerts, al)
	return nil
}

type fakeResolver struct {
	m   map[uint64]ResolvedTrade
	err error
}

func (r *fakeResolver) Resolve(_ context.Context, f EngineFill) (ResolvedTrade, error) {
	if r.err != nil {
		return ResolvedTrade{}, r.err
	}
	rt, ok := r.m[f.TradeID]
	if !ok {
		return ResolvedTrade{}, excerrors.New(CodeTradeFillUnresolvable, "unknown trade")
	}
	rt.Fill = f
	return rt, nil
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func dec(s string) decimal.Decimal { return decimal.MustFromString(s) }

func resolvedRM(tradeID uint64, buyer, seller int64) ResolvedTrade {
	return ResolvedTrade{
		Fill:            EngineFill{TradeID: tradeID, BuyOrderID: tradeID*10 + 1, SellOrderID: tradeID*10 + 2, Price: dec("1.25"), Qty: dec("10000"), EngineSeq: tradeID, ShardID: 0},
		InstrumentID:    1,
		BuyerAccountID:  buyer,
		SellerAccountID: seller,
		BaseCurrency:    "EUR",
		QuoteCurrency:   "USD",
		BuyerIntent:     IntentRollingMargin,
		SellerIntent:    IntentRollingMargin,
	}
}

func requireCodeT(t *testing.T, err error, code string) {
	t.Helper()
	var e *excerrors.Error
	if !stderrors.As(err, &e) || e.Code != code {
		t.Fatalf("want code %s, got %v", code, err)
	}
}

func lineOf(t *testing.T, j ledger.Journal, code string, debit bool) ledger.Line {
	t.Helper()
	for _, l := range j.Lines {
		if l.AccountCode == code && (l.Debit.IsPositive() == debit) {
			return l
		}
	}
	t.Fatalf("line %s debit=%v not found in %v", code, debit, j.Lines)
	return ledger.Line{}
}

func effectOf(t *testing.T, j ledger.Journal, acct int64, ccy string) ledger.AccountEffect {
	t.Helper()
	for _, e := range j.Effects {
		if e.AccountID == acct && e.Currency == ccy {
			return e
		}
	}
	t.Fatalf("effect acct=%d ccy=%s not found", acct, ccy)
	return ledger.AccountEffect{}
}

// ---------------------------------------------------------------------------
// Task 3.3.1 — four-legged mutation correctness
// ---------------------------------------------------------------------------

func TestRollingFillJournalFourLegs(t *testing.T) {
	rt := resolvedRM(42, 1001, 2002)
	rt.BuyerFee = dec("0.5")  // EUR — deducted from base received
	rt.SellerFee = dec("2.5") // USD — deducted from quote received

	j, err := buildFillJournal(rt, "test")
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if err := j.ValidateAccounts(ledger.DefaultChart()); err != nil {
		t.Fatalf("accounts: %v", err)
	}
	if j.IdempotencyKey != "trade-fill:42" {
		t.Fatalf("idempotency key %q", j.IdempotencyKey)
	}
	if j.EntryType != ledger.EntryTradeFill || j.ReferenceID != 42 {
		t.Fatalf("entry type/ref: %s/%d", j.EntryType, j.ReferenceID)
	}

	// GL lines — quote side: 12500 USD released; 12497.5 to seller + 2.5 fee.
	dr := lineOf(t, j, ledger.CustomerLiability("USD"), true)
	if !dr.Debit.Equal(dec("12500")) {
		t.Fatalf("buyer quote debit %s", dr.Debit)
	}
	cr := lineOf(t, j, ledger.CustomerLiability("USD"), false)
	if !cr.Credit.Equal(dec("12497.5")) {
		t.Fatalf("seller quote credit %s", cr.Credit)
	}
	fr := lineOf(t, j, ledger.TradingFeeRevenue("USD"), false)
	if !fr.Credit.Equal(dec("2.5")) {
		t.Fatalf("seller fee line %s", fr.Credit)
	}
	// Base side: 10000 EUR delivered; 9999.5 to buyer + 0.5 fee.
	dr = lineOf(t, j, ledger.CustomerLiability("EUR"), true)
	if !dr.Debit.Equal(dec("10000")) {
		t.Fatalf("seller base debit %s", dr.Debit)
	}
	cr = lineOf(t, j, ledger.CustomerLiability("EUR"), false)
	if !cr.Credit.Equal(dec("9999.5")) {
		t.Fatalf("buyer base credit %s", cr.Credit)
	}
	fr = lineOf(t, j, ledger.TradingFeeRevenue("EUR"), false)
	if !fr.Credit.Equal(dec("0.5")) {
		t.Fatalf("buyer fee line %s", fr.Credit)
	}

	// Wallet effects — the four legs.
	e := effectOf(t, j, 1001, "USD")
	if !e.LockedDelta.Equal(dec("-12500")) || !e.AvailableDelta.IsZero() {
		t.Fatalf("buyer quote effect %+v", e)
	}
	e = effectOf(t, j, 1001, "EUR")
	if !e.AvailableDelta.Equal(dec("9999.5")) || !e.LockedDelta.IsZero() {
		t.Fatalf("buyer base effect %+v", e)
	}
	e = effectOf(t, j, 2002, "EUR")
	if !e.LockedDelta.Equal(dec("-10000")) || !e.AvailableDelta.IsZero() {
		t.Fatalf("seller base effect %+v", e)
	}
	e = effectOf(t, j, 2002, "USD")
	if !e.AvailableDelta.Equal(dec("12497.5")) || !e.LockedDelta.IsZero() {
		t.Fatalf("seller quote effect %+v", e)
	}
}

func TestRollingFillJournalZeroFees(t *testing.T) {
	j, err := buildFillJournal(resolvedRM(7, 1, 2), "test")
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if len(j.Lines) != 4 {
		t.Fatalf("zero-fee journal should have exactly 4 lines, got %d", len(j.Lines))
	}
}

func TestRollingFillFeeExceedsProceedsAborts(t *testing.T) {
	rt := resolvedRM(8, 1, 2)
	rt.SellerFee = dec("13000") // > 12500 quote proceeds
	_, err := buildFillJournal(rt, "test")
	requireCodeT(t, err, CodeFeeExceedsProceeds)
	rt.SellerFee = decimal.Zero
	rt.BuyerFee = dec("10001") // > 10000 base delivered
	_, err = buildFillJournal(rt, "test")
	requireCodeT(t, err, CodeFeeExceedsProceeds)
}

func TestFillJournalInvalidInputRejected(t *testing.T) {
	rt := resolvedRM(0, 1, 2) // zero trade id
	if _, err := buildFillJournal(rt, "test"); err == nil {
		t.Fatal("zero trade_id must be rejected")
	}
	rt = resolvedRM(9, 1, 2)
	rt.Fill.Qty = decimal.Zero
	if _, err := buildFillJournal(rt, "test"); err == nil {
		t.Fatal("zero qty must be rejected")
	}
}

// ---------------------------------------------------------------------------
// Task 3.3.22 (balance side) — PHYSICAL_DELIVERY vs ROLLING_MARGIN
// ---------------------------------------------------------------------------

func TestPhysicalDeliveryJournalLocksDeliverables(t *testing.T) {
	rt := resolvedRM(55, 1001, 2002)
	rt.BuyerIntent = IntentPhysicalDelivery
	rt.SellerIntent = IntentPhysicalDelivery
	rt.BuyerFee = dec("0.5") // PD ignores receive-side fee netting at execution
	rt.SellerFee = dec("2.5")

	j, err := buildFillJournal(rt, "test")
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if err := j.ValidateAccounts(ledger.DefaultChart()); err != nil {
		t.Fatalf("accounts: %v", err)
	}
	if len(j.Lines) != 4 {
		t.Fatalf("delivery journal should have 4 reclass lines, got %d", len(j.Lines))
	}
	// Buyer's quote deliverable reclassified 2010 → 2011.
	dr := lineOf(t, j, ledger.CustomerLiability("USD"), true)
	if !dr.Debit.Equal(dec("12500")) {
		t.Fatalf("quote reclass debit %s", dr.Debit)
	}
	cr := lineOf(t, j, ledger.PendingSettlementDelivery("USD"), false)
	if !cr.Credit.Equal(dec("12500")) {
		t.Fatalf("pending-delivery quote credit %s", cr.Credit)
	}
	// Seller's base deliverable reclassified 2010 → 2011.
	dr = lineOf(t, j, ledger.CustomerLiability("EUR"), true)
	if !dr.Debit.Equal(dec("10000")) {
		t.Fatalf("base reclass debit %s", dr.Debit)
	}
	cr = lineOf(t, j, ledger.PendingSettlementDelivery("EUR"), false)
	if !cr.Credit.Equal(dec("10000")) {
		t.Fatalf("pending-delivery base credit %s", cr.Credit)
	}
	// No fee revenue lines at execution (fee travels with settlement-confirm).
	for _, l := range j.Lines {
		if l.AccountCode == ledger.TradingFeeRevenue("USD") || l.AccountCode == ledger.TradingFeeRevenue("EUR") {
			t.Fatalf("PD journal must not book fee revenue: %v", l)
		}
	}
	// Wallet: deliverables move available → locked (net-zero total).
	e := effectOf(t, j, 1001, "USD")
	if !e.AvailableDelta.Equal(dec("-12500")) || !e.LockedDelta.Equal(dec("12500")) {
		t.Fatalf("buyer quote park %+v", e)
	}
	e = effectOf(t, j, 2002, "EUR")
	if !e.AvailableDelta.Equal(dec("-10000")) || !e.LockedDelta.Equal(dec("10000")) {
		t.Fatalf("seller base park %+v", e)
	}
	if len(j.Effects) != 2 {
		t.Fatalf("PD journal must carry exactly the two parked legs, got %d", len(j.Effects))
	}
}

func TestMixedSettlementIntentAborts(t *testing.T) {
	rt := resolvedRM(56, 1, 2)
	rt.BuyerIntent = IntentPhysicalDelivery // seller stays ROLLING_MARGIN
	_, err := buildFillJournal(rt, "test")
	requireCodeT(t, err, CodeSettlementIntentMismatch)
}

func TestInvalidIntentRejected(t *testing.T) {
	rt := resolvedRM(57, 1, 2)
	rt.BuyerIntent = "GIBBERISH"
	if _, err := buildFillJournal(rt, "test"); err == nil {
		t.Fatal("invalid intent must be rejected")
	}
}

// ---------------------------------------------------------------------------
// Task 3.3.1 — batching, dedup, locking through ProcessFills
// ---------------------------------------------------------------------------

func TestProcessFillsSingleTxBatch(t *testing.T) {
	store := newFakeStore()
	svc := newBalanceServiceForTest(store, &fakeLocker{}, &fakeDispatcher{}, &fakeManagedPoster{}, &fakeAlerter{})
	fills := []ResolvedTrade{resolvedRM(1, 11, 12), resolvedRM(2, 13, 14), resolvedRM(3, 15, 16)}
	outcomes, err := svc.ProcessFills(context.Background(), fills)
	if err != nil {
		t.Fatalf("process: %v", err)
	}
	if len(outcomes) != 3 || store.commits != 1 || len(store.posted) != 3 {
		t.Fatalf("outcomes=%d commits=%d posted=%d", len(outcomes), store.commits, len(store.posted))
	}
	for _, o := range outcomes {
		if !o.Applied || o.Duplicate {
			t.Fatalf("outcome %+v", o)
		}
	}
}

func TestProcessFillsIdempotentReplay(t *testing.T) {
	store := newFakeStore()
	svc := newBalanceServiceForTest(store, &fakeLocker{}, &fakeDispatcher{}, &fakeManagedPoster{}, &fakeAlerter{})
	ctx := context.Background()
	if _, err := svc.ProcessFills(ctx, []ResolvedTrade{resolvedRM(9, 1, 2)}); err != nil {
		t.Fatalf("first: %v", err)
	}
	outcomes, err := svc.ProcessFills(ctx, []ResolvedTrade{resolvedRM(9, 1, 2)})
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if len(outcomes) != 1 || !outcomes[0].Duplicate || outcomes[0].Applied {
		t.Fatalf("replay outcome %+v", outcomes[0])
	}
	if len(store.posted) != 1 {
		t.Fatalf("replayed trade must not repost journal (posted=%d)", len(store.posted))
	}
}

func TestProcessFillsDuplicateWithinBatch(t *testing.T) {
	store := newFakeStore()
	svc := newBalanceServiceForTest(store, &fakeLocker{}, &fakeDispatcher{}, &fakeManagedPoster{}, &fakeAlerter{})
	outcomes, err := svc.ProcessFills(context.Background(),
		[]ResolvedTrade{resolvedRM(10, 1, 2), resolvedRM(10, 1, 2)})
	if err != nil {
		t.Fatalf("process: %v", err)
	}
	if !outcomes[0].Applied || !outcomes[1].Duplicate || len(store.posted) != 1 {
		t.Fatalf("outcomes %+v posted=%d", outcomes, len(store.posted))
	}
}

// ---------------------------------------------------------------------------
// Task 3.3.1 — account mutex
// ---------------------------------------------------------------------------

func TestMutexContentionAbortsBeforeTx(t *testing.T) {
	store := newFakeStore()
	locker := &fakeLocker{busy: true}
	svc := newBalanceServiceForTest(store, locker, &fakeDispatcher{}, &fakeManagedPoster{}, &fakeAlerter{})
	_, err := svc.ProcessFills(context.Background(), []ResolvedTrade{resolvedRM(1, 1, 2)})
	requireCodeT(t, err, ledger.CodeAccountBusy)
	if store.calls != 0 {
		t.Fatalf("no tx may begin when the mutex is held (calls=%d)", store.calls)
	}
}

func TestLocksAcquiredSortedAndReleased(t *testing.T) {
	store := newFakeStore()
	locker := &fakeLocker{}
	svc := newBalanceServiceForTest(store, locker, &fakeDispatcher{}, &fakeManagedPoster{}, &fakeAlerter{})
	fills := []ResolvedTrade{resolvedRM(1, 99, 3), resolvedRM(2, 42, 7)}
	if _, err := svc.ProcessFills(context.Background(), fills); err != nil {
		t.Fatalf("process: %v", err)
	}
	want := []int64{3, 7, 42, 99}
	if len(locker.lockedOrder) != len(want) {
		t.Fatalf("locked %v", locker.lockedOrder)
	}
	for i, id := range want {
		if locker.lockedOrder[i] != id {
			t.Fatalf("lock order %v, want %v", locker.lockedOrder, want)
		}
	}
	if locker.unlockCalls == 0 {
		t.Fatal("locks must be released after commit")
	}
}

func TestMissingLockBackendFailsClosed(t *testing.T) {
	svc := newBalanceServiceForTest(newFakeStore(), nil, &fakeDispatcher{}, &fakeManagedPoster{}, &fakeAlerter{})
	_, err := svc.ProcessFills(context.Background(), []ResolvedTrade{resolvedRM(1, 1, 2)})
	requireCodeT(t, err, ledger.CodeLedgerLockUnavailable)
}

// ---------------------------------------------------------------------------
// Task 3.3.18 — serialization retry math & imbalance abort
// ---------------------------------------------------------------------------

func TestSerializationRetrySucceeds(t *testing.T) {
	store := newFakeStore()
	store.commitFailures = []error{
		&pgconn.PgError{Code: "40001"},
		&pgconn.PgError{Code: "40P01"},
	}
	svc := newBalanceServiceForTest(store, &fakeLocker{}, &fakeDispatcher{}, &fakeManagedPoster{}, &fakeAlerter{})
	outcomes, err := svc.ProcessFills(context.Background(), []ResolvedTrade{resolvedRM(1, 1, 2)})
	if err != nil {
		t.Fatalf("retry should succeed: %v", err)
	}
	if store.calls != 3 || store.commits != 1 || store.rollbacks != 2 {
		t.Fatalf("calls=%d commits=%d rollbacks=%d", store.calls, store.commits, store.rollbacks)
	}
	if !outcomes[0].Applied {
		t.Fatalf("outcome %+v", outcomes[0])
	}
	if len(store.posted) != 1 {
		t.Fatal("retried commit must post the journal exactly once")
	}
}

func TestSerializationRetryExhausted(t *testing.T) {
	store := newFakeStore()
	store.commitFailures = []error{
		&pgconn.PgError{Code: "40001"},
		&pgconn.PgError{Code: "40001"},
		&pgconn.PgError{Code: "40001"},
	}
	svc := newBalanceServiceForTest(store, &fakeLocker{}, &fakeDispatcher{}, &fakeManagedPoster{}, &fakeAlerter{})
	_, err := svc.ProcessFills(context.Background(), []ResolvedTrade{resolvedRM(1, 1, 2)})
	requireCodeT(t, err, ledger.CodeTxnConflictExhausted)
	if store.calls != 3 || store.commits != 0 {
		t.Fatalf("calls=%d commits=%d", store.calls, store.commits)
	}
}

func TestNonRetryableErrorNoRetry(t *testing.T) {
	store := newFakeStore()
	store.posterErr = excerrors.New(ledger.CodeInsufficientBalance, "locked would go negative")
	svc := newBalanceServiceForTest(store, &fakeLocker{}, &fakeDispatcher{}, &fakeManagedPoster{}, &fakeAlerter{})
	_, err := svc.ProcessFills(context.Background(), []ResolvedTrade{resolvedRM(1, 1, 2)})
	requireCodeT(t, err, ledger.CodeInsufficientBalance)
	if store.calls != 1 {
		t.Fatalf("non-retryable error must not retry (calls=%d)", store.calls)
	}
}

func TestImbalanceAbortPropagatesAndPagesP0(t *testing.T) {
	store := newFakeStore()
	store.posterErr = excerrors.New(ledger.CodeLedgerImbalanceAbort, "stored sums diverge")
	alerter := &fakeAlerter{}
	svc := newBalanceServiceForTest(store, &fakeLocker{}, &fakeDispatcher{}, &fakeManagedPoster{}, alerter)
	_, err := svc.ProcessFills(context.Background(), []ResolvedTrade{resolvedRM(1, 1, 2)})
	requireCodeT(t, err, ledger.CodeLedgerImbalanceAbort)
	if store.commits != 0 {
		t.Fatal("imbalanced journal must never commit")
	}
	if len(alerter.alerts) != 1 || alerter.alerts[0].Severity != SeverityP0 ||
		alerter.alerts[0].Code != ledger.CodeLedgerImbalanceAbort {
		t.Fatalf("P0 alert expected, got %+v", alerter.alerts)
	}
}

func TestBatchIsolationContainsPoisonedFill(t *testing.T) {
	// Fill 2 is poisoned (fee exceeds proceeds). The batch aborts; the
	// isolation pass must still commit fills 1 and 3.
	store := newFakeStore()
	svc := newBalanceServiceForTest(store, &fakeLocker{}, &fakeDispatcher{}, &fakeManagedPoster{}, &fakeAlerter{})
	poison := resolvedRM(2, 13, 14)
	poison.SellerFee = dec("999999")
	outcomes, err := svc.ProcessFills(context.Background(),
		[]ResolvedTrade{resolvedRM(1, 11, 12), poison, resolvedRM(3, 15, 16)})
	if err == nil {
		t.Fatal("poisoned batch must surface an error")
	}
	if len(outcomes) != 2 {
		t.Fatalf("good fills must commit despite the poison: %+v", outcomes)
	}
	if len(store.posted) != 2 {
		t.Fatalf("posted=%d", len(store.posted))
	}
}

// ---------------------------------------------------------------------------
// Task 3.3.18 — settlement-rail compensation
// ---------------------------------------------------------------------------

func TestCompensationJournalZeroSum(t *testing.T) {
	r := RailRejection{
		InstructionID: 77, AccountID: 1001, Currency: "USD",
		Principal: dec("10000"), WireFee: dec("25"), Rail: "SWIFT", ReturnCode: "AC01",
	}
	j, err := buildCompensationJournal(r, "test")
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if err := j.ValidateAccounts(ledger.DefaultChart()); err != nil {
		t.Fatalf("accounts: %v", err)
	}
	if j.IdempotencyKey != "settlement-comp:77" || j.EntryType != ledger.EntrySettlement {
		t.Fatalf("idem=%q type=%s", j.IdempotencyKey, j.EntryType)
	}
	dr := lineOf(t, j, ledger.NostroClearing("USD"), true)
	if !dr.Debit.Equal(dec("9975")) {
		t.Fatalf("nostro return %s", dr.Debit)
	}
	fr := lineOf(t, j, ledger.BankRailFeeExpense("USD"), true)
	if !fr.Debit.Equal(dec("25")) {
		t.Fatalf("wire-fee contra %s", fr.Debit)
	}
	cr := lineOf(t, j, ledger.CustomerLiability("USD"), false)
	if !cr.Credit.Equal(dec("10000")) {
		t.Fatalf("client re-credit %s", cr.Credit)
	}
	e := effectOf(t, j, 1001, "USD")
	if !e.AvailableDelta.Equal(dec("10000")) {
		t.Fatalf("wallet effect %+v", e)
	}
}

func TestCompensateRailRejectionFlow(t *testing.T) {
	poster := &fakeManagedPoster{}
	alerter := &fakeAlerter{}
	svc := newBalanceServiceForTest(newFakeStore(), &fakeLocker{}, &fakeDispatcher{}, poster, alerter)
	res, err := svc.CompensateRailRejection(context.Background(), RailRejection{
		InstructionID: 88, AccountID: 5, Currency: "EUR",
		Principal: dec("5000"), WireFee: dec("10"), Rail: "SEPA", ReturnCode: "AM04",
	})
	if err != nil {
		t.Fatalf("compensate: %v", err)
	}
	if !res.Alerted || !res.PostResult.Committed {
		t.Fatalf("result %+v", res)
	}
	if len(poster.journals) != 1 {
		t.Fatal("compensation journal not posted")
	}
	if len(alerter.alerts) != 1 || alerter.alerts[0].Severity != SeverityP1 {
		t.Fatalf("ops alert %+v", alerter.alerts)
	}
}

func TestCompensateInvalidRejection(t *testing.T) {
	svc := newBalanceServiceForTest(newFakeStore(), &fakeLocker{}, &fakeDispatcher{}, &fakeManagedPoster{}, &fakeAlerter{})
	_, err := svc.CompensateRailRejection(context.Background(), RailRejection{
		InstructionID: 1, AccountID: 1, Currency: "USD",
		Principal: dec("100"), WireFee: dec("150"), // fee > principal
	})
	requireCodeT(t, err, CodeCompensationInvalid)
}

func TestCompensatePostFailureStillAlertsP0(t *testing.T) {
	alerter := &fakeAlerter{}
	svc := newBalanceServiceForTest(newFakeStore(), &fakeLocker{}, &fakeDispatcher{},
		&fakeManagedPoster{err: stderrors.New("db down")}, alerter)
	_, err := svc.CompensateRailRejection(context.Background(), RailRejection{
		InstructionID: 9, AccountID: 1, Currency: "USD",
		Principal: dec("100"), Rail: "SWIFT", ReturnCode: "RR04",
	})
	if err == nil {
		t.Fatal("posting failure must propagate")
	}
	if len(alerter.alerts) != 1 || alerter.alerts[0].Severity != SeverityP0 {
		t.Fatalf("failed compensation must page P0: %+v", alerter.alerts)
	}
}

// ---------------------------------------------------------------------------
// Consumer — wire decode, batching, fail-closed resolution
// ---------------------------------------------------------------------------

func encodeFill(b *flatbuffers.Builder, seq, tradeID, buyID, sellID uint64, price, qty int64) []byte {
	buf := ipc.EncodeTradeFillEvent(b, seq, 123, tradeID, buyID, sellID, price, qty, int64(seq))
	out := make([]byte, len(buf))
	copy(out, buf)
	return out
}

func TestConsumerDecodesAndFlushes(t *testing.T) {
	store := newFakeStore()
	svc := newBalanceServiceForTest(store, &fakeLocker{}, &fakeDispatcher{}, &fakeManagedPoster{}, &fakeAlerter{})
	resolver := &fakeResolver{m: map[uint64]ResolvedTrade{42: resolvedRM(42, 1, 2)}}

	fb := flatbuffers.NewBuilder(256)
	frame := encodeFill(fb, 1, 42, 101, 202, 125000000 /*1.25*/, 10000_00000000 /*10000*/)
	var delivered [][]byte
	src := FuncSource(func(limit int, deliver func([]byte)) int {
		if len(delivered) > 0 {
			return 0
		}
		delivered = append(delivered, frame)
		deliver(frame)
		return 1
	})

	c, err := NewFillConsumer(svc, resolver, src, 3, 1, time.Millisecond)
	if err != nil {
		t.Fatalf("consumer: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()
	deadline := time.Now().Add(2 * time.Second)
	for store.committed() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	cancel()
	<-done
	if store.committed() != 1 || len(store.posted) != 1 {
		t.Fatalf("commits=%d posted=%d", store.committed(), len(store.posted))
	}
	if store.processed[42] != 3 {
		t.Fatalf("shard id recorded = %d", store.processed[42])
	}
	// qty*price = 12500 USD quote leg must appear in the posted journal.
	e := effectOf(t, store.posted[0], 1, "USD")
	if !e.LockedDelta.Equal(dec("-12500")) {
		t.Fatalf("buyer locked quote %+v", e)
	}
}

func TestConsumerMalformedFrameCounted(t *testing.T) {
	resolver := &fakeResolver{m: map[uint64]ResolvedTrade{}}
	src := FuncSource(func(limit int, deliver func([]byte)) int {
		deliver([]byte{0xde, 0xad})
		return 1
	})
	c, err := NewFillConsumer(&BalanceService{}, resolver, src, 0, 10, time.Hour)
	if err != nil {
		t.Fatalf("consumer: %v", err)
	}
	// Deliver directly through the decode path.
	if _, ok := c.decodeFragment([]byte{0xde, 0xad}); ok {
		t.Fatalf("malformed should be skipped, not fatal")
	}
	if c.Metrics().Malformed != 1 {
		t.Fatalf("malformed=%d", c.Metrics().Malformed)
	}
}

func TestConsumerResolutionFailureHalts(t *testing.T) {
	store := newFakeStore()
	svc := newBalanceServiceForTest(store, &fakeLocker{}, &fakeDispatcher{}, &fakeManagedPoster{}, &fakeAlerter{})
	resolver := &fakeResolver{err: excerrors.New(CodeTradeFillUnresolvable, "no orders")}
	src := FuncSource(func(limit int, deliver func([]byte)) int {
		fb := flatbuffers.NewBuilder(256)
		deliver(encodeFill(fb, 1, 7, 11, 12, 100000000, 100000000))
		return 1
	})
	c, _ := NewFillConsumer(svc, resolver, src, 0, 10, time.Hour)
	err := c.Run(context.Background())
	requireCodeT(t, err, CodeTradeFillUnresolvable)
	if store.calls != 0 {
		t.Fatalf("unresolvable fill must never reach a tx (calls=%d)", store.calls)
	}
}
