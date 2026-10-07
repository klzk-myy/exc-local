// balance_service_test.go — unit tests for Tasks 3.3.1 / 3.3.18 /
// 3.3.22(balance). All seams are in-memory; no Postgres/Redis required
// (integration coverage is the EXC_PG_TEST-gated suite pattern used by
// position_service_integration_test.go).
package settlement

import (
	"context"
	stderrors "errors"
	"strings"
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
	processed      map[int64]int64  // trade_id → shard_id
	frames         map[int64][]byte // trade_id → committed raw_frame
	posted         []ledger.Journal
	posterErr      error // injected PostJournal failure (in-tx abort)
}

func newFakeStore() *fakeBalanceStore {
	return &fakeBalanceStore{processed: map[int64]int64{}, frames: map[int64][]byte{}}
}

func (s *fakeBalanceStore) InTx(ctx context.Context, fn func(ctx context.Context, tx balanceTx) error) error {
	s.mu.Lock()
	s.calls++
	s.mu.Unlock()
	tx := &fakeBalanceTx{s: s, pending: map[int64]int64{}, pendingFrames: map[int64][]byte{}}
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
	for id, fr := range tx.pendingFrames {
		s.frames[id] = fr
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
	s             *fakeBalanceStore
	pending       map[int64]int64
	pendingFrames map[int64][]byte
	posts         []ledger.Journal
}

func (t *fakeBalanceTx) RecordProcessed(_ context.Context, tradeID, shardID int64, raw []byte) (bool, error) {
	t.s.mu.Lock()
	defer t.s.mu.Unlock()
	if _, ok := t.s.processed[tradeID]; ok {
		return false, nil
	}
	if _, ok := t.pending[tradeID]; ok {
		return false, nil
	}
	t.pending[tradeID] = shardID
	t.pendingFrames[tradeID] = raw
	return true, nil
}

func (t *fakeBalanceTx) ProcessedFrame(_ context.Context, tradeID int64) ([]byte, error) {
	t.s.mu.Lock()
	defer t.s.mu.Unlock()
	if fr, ok := t.s.frames[tradeID]; ok {
		return fr, nil
	}
	return t.pendingFrames[tradeID], nil
}

func (t *fakeBalanceTx) RecordTrade(_ context.Context, _ ResolvedTrade) error { return nil }

func (t *fakeBalanceTx) AccrueVolume(_ context.Context, _ CommissionVolumeAccrual) error {
	return nil
}

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
	busyTimes   int
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
	if l.busyTimes > 0 {
		l.busyTimes--
		return nil, excerrors.New(ledger.CodeAccountBusy, "account mutex held")
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

// withRawFrame (re)encodes Fill.Raw from the fill's current fields —
// helpers that mutate price/qty after resolvedRM must call it last so the
// committed frame and the resolved fill stay wire-identical (the dedup
// collision check compares them verbatim).
func withRawFrame(rt ResolvedTrade) ResolvedTrade {
	f := &rt.Fill
	f.Raw = ipc.EncodeTradeFillEvent(flatbuffers.NewBuilder(256),
		uint64(f.EngineSeq), 0, f.TradeID, f.BuyOrderID, f.SellOrderID,
		decimal.Scaled(f.Price), decimal.Scaled(f.Qty), int64(f.EngineSeq))
	return rt
}

func resolvedRM(tradeID uint64, buyer, seller int64) ResolvedTrade {
	f := EngineFill{TradeID: tradeID, BuyOrderID: tradeID*10 + 1, SellOrderID: tradeID*10 + 2, Price: dec("1.25"), Qty: dec("10000"), EngineSeq: tradeID, ShardID: 0}
	return withRawFrame(ResolvedTrade{
		Fill:            f,
		InstrumentID:    1,
		BuyerAccountID:  buyer,
		SellerAccountID: seller,
		BaseCurrency:    "EUR",
		QuoteCurrency:   "USD",
		BuyerIntent:     IntentRollingMargin,
		SellerIntent:    IntentRollingMargin,
	})
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

	// GL lines — gross delivery; the fill journal carries NO fee legs
	// (fees post as separate FEE journals from ResolvedTrade.FeeJournals).
	dr := lineOf(t, j, ledger.CustomerLiability("USD"), true)
	if !dr.Debit.Equal(dec("12500")) {
		t.Fatalf("buyer quote debit %s", dr.Debit)
	}
	cr := lineOf(t, j, ledger.CustomerLiability("USD"), false)
	if !cr.Credit.Equal(dec("12500")) {
		t.Fatalf("seller quote credit %s", cr.Credit)
	}
	dr = lineOf(t, j, ledger.CustomerLiability("EUR"), true)
	if !dr.Debit.Equal(dec("10000")) {
		t.Fatalf("seller base debit %s", dr.Debit)
	}
	cr = lineOf(t, j, ledger.CustomerLiability("EUR"), false)
	if !cr.Credit.Equal(dec("10000")) {
		t.Fatalf("buyer base credit %s", cr.Credit)
	}
	if len(j.Lines) != 4 {
		t.Fatalf("fill journal must carry exactly 4 gross lines, got %d", len(j.Lines))
	}

	// Wallet effects — the four legs, gross.
	e := effectOf(t, j, 1001, "USD")
	if !e.LockedDelta.Equal(dec("-12500")) || !e.AvailableDelta.IsZero() {
		t.Fatalf("buyer quote effect %+v", e)
	}
	e = effectOf(t, j, 1001, "EUR")
	if !e.AvailableDelta.Equal(dec("10000")) || !e.LockedDelta.IsZero() {
		t.Fatalf("buyer base effect %+v", e)
	}
	e = effectOf(t, j, 2002, "EUR")
	if !e.LockedDelta.Equal(dec("-10000")) || !e.AvailableDelta.IsZero() {
		t.Fatalf("seller base effect %+v", e)
	}
	e = effectOf(t, j, 2002, "USD")
	if !e.AvailableDelta.Equal(dec("12500")) || !e.LockedDelta.IsZero() {
		t.Fatalf("seller quote effect %+v", e)
	}
}

// TestFeeJournalPostsInsideCommit — the Task 3.3.4 contract: a resolved
// fill carrying FeeJournals posts fill + fee legs in ONE commit, keyed
// fee:{trade}:{acct}:{role}; the recon collected-fee leg is the wallet
// debit on the FEE-typed ledger row.
func TestFeeJournalPostsInsideCommit(t *testing.T) {
	store := newFakeStore()
	svc := newBalanceServiceForTest(store, &fakeLocker{}, &fakeDispatcher{}, &fakeManagedPoster{}, &fakeAlerter{})
	rt := resolvedRM(42, 1, 2)
	// Buyer fee 0.5 EUR (maker side per resolvedRM's fixtures) + seller
	// fee 2.5 USD — mirrors the tape values; journals built by the
	// production resolver enrich path.
	bq := FeeQuote{AccountID: 1001, Role: RoleTaker, Currency: "EUR",
		Notional: dec("10000"), RateBps: dec("0.5"), Amount: dec("0.5"), TierName: "test"}
	sq := FeeQuote{AccountID: 2002, Role: RoleMaker, Currency: "USD",
		Notional: dec("12500"), RateBps: dec("2"), Amount: dec("2.5"), TierName: "test"}
	bj, err := BuildFeeJournal(bq, 42, "test")
	if err != nil {
		t.Fatalf("buyer fee journal: %v", err)
	}
	sj, err := BuildFeeJournal(sq, 42, "test")
	if err != nil {
		t.Fatalf("seller fee journal: %v", err)
	}
	rt.BuyerFee, rt.SellerFee = bq.Amount, sq.Amount
	rt.FeeJournals = []ledger.Journal{bj, sj}
	outcomes, err := svc.ProcessFills(context.Background(), []ResolvedTrade{rt})
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	if len(outcomes) != 1 || !outcomes[0].Applied {
		t.Fatalf("outcomes %+v", outcomes)
	}
	// Fill + two FEE journals in one commit.
	if len(store.posted) != 3 {
		t.Fatalf("posted %d journals, want 3 (fill + 2 fee legs)", len(store.posted))
	}
	keys := map[string]bool{}
	feeDebits := decimal.Zero
	for _, j := range store.posted {
		keys[j.IdempotencyKey] = true
		if j.EntryType == ledger.EntryFee {
			for _, e := range j.Effects {
				if e.AvailableDelta.IsNegative() {
					feeDebits = feeDebits.Add(e.AvailableDelta.Abs())
				}
			}
		}
	}
	for _, want := range []string{"trade-fill:42", "fee:42:1001:TAKER", "fee:42:2002:MAKER"} {
		if !keys[want] {
			t.Fatalf("missing journal %q — have %v", want, keys)
		}
	}
	if !feeDebits.Equal(dec("3")) {
		t.Fatalf("fee wallet debits %s, want 3 (0.5 EUR + 2.5 USD)", feeDebits)
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

// enrichResolver builds a resolver with a map-keyed fake tier store —
// drives resolver enrich tests without a pool.
func enrichResolver(t *testing.T, tiers map[int64]FeeTier) *PgxTradeResolver {
	t.Helper()
	svc, err := NewFeeService(&fakeFeeStore{tiers: tiers}, nil, "test", nil)
	if err != nil {
		t.Fatalf("fee service: %v", err)
	}
	return &PgxTradeResolver{Fees: svc}
}

// TestFeeExceedsProceedsAborts — the bound moved to resolver enrich: a
// tier rate producing a fee larger than the received amount fails closed
// before the journal is ever built.
func TestFeeExceedsProceedsAborts(t *testing.T) {
	r := enrichResolver(t, map[int64]FeeTier{
		1: {ID: 1, TierName: "absurd", TakerBps: dec("20000")},
		2: {ID: 1, TierName: "absurd", TakerBps: dec("20000")},
	})
	rt := resolvedRM(8, 1, 2) // qty 10000 @1.25; taker = higher order_seq side
	// Buyer taker (boSeq>soSeq): base fee 20000bps × 10000 = 20000 > qty.
	err := r.enrich(context.Background(), &rt, legFacts{boSeq: 2, soSeq: 1, symbol: "EUR/USD"})
	requireCodeT(t, err, CodeFeeExceedsProceeds)
	// Seller taker: quote fee 20000bps × 12500 = 25000 > proceeds.
	rt = resolvedRM(8, 1, 2)
	err = r.enrich(context.Background(), &rt, legFacts{boSeq: 1, soSeq: 2, symbol: "EUR/USD"})
	requireCodeT(t, err, CodeFeeExceedsProceeds)
}

// TestEnrichComputesMakerTakerFees — resting side prices at maker_bps,
// incoming at taker_bps; buyer fee denominated in base, seller in quote.
func TestEnrichComputesMakerTakerFees(t *testing.T) {
	tier := FeeTier{ID: 1, TierName: "std", MakerBps: dec("1"), TakerBps: dec("2")}
	r := enrichResolver(t, map[int64]FeeTier{1: tier, 2: tier})
	rt := resolvedRM(9, 1, 2) // 10000 EUR @ 1.25 → 12500 USD
	// Sell order rested first (soSeq<boSeq) → seller MAKER, buyer TAKER.
	if err := r.enrich(context.Background(), &rt, legFacts{
		boSeq: 2, soSeq: 1, symbol: "EUR/USD"}); err != nil {
		t.Fatalf("enrich: %v", err)
	}
	// Buyer taker: 10000 base × 2bps = 2.00000000 EUR.
	if !rt.BuyerFee.Equal(dec("2")) {
		t.Fatalf("buyer fee %s, want 2 (base-denominated taker)", rt.BuyerFee)
	}
	// Seller maker: 12500 quote × 1bps = 1.25000000 USD.
	if !rt.SellerFee.Equal(dec("1.25")) {
		t.Fatalf("seller fee %s, want 1.25 (quote-denominated maker)", rt.SellerFee)
	}
	if len(rt.FeeJournals) != 2 {
		t.Fatalf("want 2 fee journals, got %d", len(rt.FeeJournals))
	}
	// Reverse the order_seq — roles flip.
	rt = resolvedRM(10, 1, 2)
	if err := r.enrich(context.Background(), &rt, legFacts{
		boSeq: 1, soSeq: 2, symbol: "EUR/USD"}); err != nil {
		t.Fatalf("enrich: %v", err)
	}
	if !rt.BuyerFee.Equal(dec("1")) || !rt.SellerFee.Equal(dec("2.5")) {
		t.Fatalf("role flip: buyer %s seller %s", rt.BuyerFee, rt.SellerFee)
	}
}

// TestEnrichZeroRateTier — a 0/0 tier yields zero fees and no fee
// journals (free-tier fills stay 4-line journals).
func TestEnrichZeroRateTier(t *testing.T) {
	r := enrichResolver(t, map[int64]FeeTier{
		1: {ID: 1, TierName: "free"}, 2: {ID: 1, TierName: "free"}})
	rt := resolvedRM(11, 1, 2)
	if err := r.enrich(context.Background(), &rt, legFacts{
		boSeq: 2, soSeq: 1, symbol: "EUR/USD"}); err != nil {
		t.Fatalf("enrich: %v", err)
	}
	if !rt.BuyerFee.IsZero() || !rt.SellerFee.IsZero() || len(rt.FeeJournals) != 0 {
		t.Fatalf("zero-tier must stay fee-free: %+v", rt.FeeJournals)
	}
}

// TestEnrichMissingTierFailsClosed — FEE_TIER_NOT_FOUND aborts the fill
// (§2.7: never silently settle fee-free).
func TestEnrichMissingTierFailsClosed(t *testing.T) {
	r := enrichResolver(t, map[int64]FeeTier{}) // no tier for acct 1
	rt := resolvedRM(12, 1, 2)
	err := r.enrich(context.Background(), &rt, legFacts{boSeq: 2, soSeq: 1})
	if err == nil {
		t.Fatal("missing fee tier must fail closed")
	}
	var coded *excerrors.Error
	if !stderrors.As(err, &coded) || coded.Cause == nil ||
		!strings.Contains(coded.Cause.Error(), CodeFeeTierNotFound) {
		t.Fatalf("want wrapped FEE_TIER_NOT_FOUND, got %v", err)
	}
}

// TestEnrichMakerRebate — a negative maker rate flips the fee journal:
// the account is credited from fee revenue (spec §8.5).
func TestEnrichMakerRebate(t *testing.T) {
	tier := FeeTier{ID: 1, TierName: "vip", MakerBps: dec("-0.5"), TakerBps: dec("1")}
	r := enrichResolver(t, map[int64]FeeTier{1: tier, 2: tier})
	rt := resolvedRM(13, 1, 2)
	// Seller maker → negative maker_bps → rebate: SellerFee < 0 and the
	// journal credits the seller wallet.
	if err := r.enrich(context.Background(), &rt, legFacts{
		boSeq: 2, soSeq: 1, symbol: "EUR/USD"}); err != nil {
		t.Fatalf("enrich: %v", err)
	}
	if !rt.SellerFee.Equal(dec("-0.625")) { // 12500 × -0.5bps
		t.Fatalf("seller rebate %s, want -0.625", rt.SellerFee)
	}
	var rebateFound bool
	for _, j := range rt.FeeJournals {
		if j.IdempotencyKey == "fee:13:2:MAKER" {
			rebateFound = true
			if len(j.Effects) != 1 || !j.Effects[0].AvailableDelta.Equal(dec("0.625")) {
				t.Fatalf("rebate effect %+v", j.Effects)
			}
		}
	}
	if !rebateFound {
		t.Fatalf("maker rebate journal missing: %+v", rt.FeeJournals)
	}
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

// Regression — engine trade-id counter regression across restart: a fill
// whose trade_id is already committed but whose CONTENT differs is not a
// replay — it's a re-issued id. Dedup must fail closed (spec §2.7), not
// silently strand the new fill's settlement leg while the read model
// marks its orders FILLED.
func TestProcessFillsTradeIDCollisionFailsClosed(t *testing.T) {
	store := newFakeStore()
	svc := newBalanceServiceForTest(store, &fakeLocker{}, &fakeDispatcher{}, &fakeManagedPoster{}, &fakeAlerter{})
	ctx := context.Background()
	if _, err := svc.ProcessFills(ctx, []ResolvedTrade{resolvedRM(9, 1, 2)}); err != nil {
		t.Fatalf("first: %v", err)
	}
	// Same trade_id, different legs — the counter-regression signature.
	clash := resolvedRM(9, 1, 2)
	clash.Fill.BuyOrderID += 100
	clash.Fill.Raw = ipc.EncodeTradeFillEvent(flatbuffers.NewBuilder(256),
		uint64(clash.Fill.EngineSeq), 0, clash.Fill.TradeID,
		clash.Fill.BuyOrderID, clash.Fill.SellOrderID,
		decimal.Scaled(clash.Fill.Price), decimal.Scaled(clash.Fill.Qty),
		int64(clash.Fill.EngineSeq))
	_, err := svc.ProcessFills(ctx, []ResolvedTrade{clash})
	requireCodeT(t, err, CodeTradeIDCollision)
	if len(store.posted) != 1 {
		t.Fatalf("collided fill must not post (posted=%d)", len(store.posted))
	}
	// A same-content conflict remains a clean replay — engine seq
	// differences are legal (boot recovery restamps it from the journal
	// seq), content divergence is not.
	replay := resolvedRM(9, 1, 2)
	replay.Fill.EngineSeq = 9999 // restamped seq — still the same fill
	replay.Fill.Raw = ipc.EncodeTradeFillEvent(flatbuffers.NewBuilder(256),
		uint64(replay.Fill.EngineSeq), 0, replay.Fill.TradeID,
		replay.Fill.BuyOrderID, replay.Fill.SellOrderID,
		decimal.Scaled(replay.Fill.Price), decimal.Scaled(replay.Fill.Qty),
		int64(replay.Fill.EngineSeq))
	outcomes, err := svc.ProcessFills(ctx, []ResolvedTrade{replay})
	if err != nil {
		t.Fatalf("replay must stay dedup-clean: %v", err)
	}
	if len(outcomes) != 1 || !outcomes[0].Duplicate {
		t.Fatalf("replay outcome %+v", outcomes[0])
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
	// Fill 2 is poisoned (malformed currency fails journal validation).
	// The batch aborts; the isolation pass must still commit fills 1
	// and 3.
	store := newFakeStore()
	svc := newBalanceServiceForTest(store, &fakeLocker{}, &fakeDispatcher{}, &fakeManagedPoster{}, &fakeAlerter{})
	poison := resolvedRM(2, 13, 14)
	poison.BaseCurrency = "XX"
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

func TestLockAccountsWithWaitRetriesBusy(t *testing.T) {
	locker := &fakeLocker{busyTimes: 3}
	svc := newBalanceServiceForTest(newFakeStore(), locker, &fakeDispatcher{}, &fakeManagedPoster{}, &fakeAlerter{})
	locked, err := svc.lockAccountsWithWait(context.Background(), []int64{1, 2}, "tok")
	if err != nil {
		t.Fatalf("transient ACCOUNT_BUSY must be waited out: %v", err)
	}
	if locker.lockCalls != 4 || len(locked) != 2 {
		t.Fatalf("calls=%d locked=%v", locker.lockCalls, locked)
	}
}

func TestLockAccountsWithWaitNonBusyFailsFast(t *testing.T) {
	sentinel := excerrors.New(ledger.CodeLedgerLockUnavailable, "redis down")
	locker := &fakeLocker{err: sentinel}
	svc := newBalanceServiceForTest(newFakeStore(), locker, &fakeDispatcher{}, &fakeManagedPoster{}, &fakeAlerter{})
	_, err := svc.lockAccountsWithWait(context.Background(), []int64{1}, "tok")
	if err != sentinel || locker.lockCalls != 1 {
		t.Fatalf("non-busy lock error must fail closed immediately: err=%v calls=%d", err, locker.lockCalls)
	}
}

func TestLockAccountsWithWaitRespectsCancel(t *testing.T) {
	locker := &fakeLocker{busy: true}
	svc := newBalanceServiceForTest(newFakeStore(), locker, &fakeDispatcher{}, &fakeManagedPoster{}, &fakeAlerter{})
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := svc.lockAccountsWithWait(ctx, []int64{1}, "tok")
	if err == nil || time.Since(start) > 5*time.Second {
		t.Fatalf("cancel must interrupt the wait, not run the 12s budget: err=%v", err)
	}
}

func TestLockAccountsFailFastUnchanged(t *testing.T) {
	locker := &fakeLocker{busy: true}
	svc := newBalanceServiceForTest(newFakeStore(), locker, &fakeDispatcher{}, &fakeManagedPoster{}, &fakeAlerter{})
	_, err := svc.lockAccounts(context.Background(), []int64{1}, "tok")
	var e *excerrors.Error
	if err == nil || !stderrors.As(err, &e) || e.Code != ledger.CodeAccountBusy || locker.lockCalls != 1 {
		t.Fatalf("client-facing lock path must stay fail-fast: err=%v calls=%d", err, locker.lockCalls)
	}
}
