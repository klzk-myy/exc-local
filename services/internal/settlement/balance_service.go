// balance_service.go — Post-Trade Balance Service
// (Phase-03 Task 3.3.1; spec §5.3/§5.21/§5.40, §24 #301 + the balance side
// of Task 3.3.22 physical-delivery partitioning, spec §5.45/§17.4a,
// §24 #406).
//
// BalanceService consumes engine TradeFill events (Aeron `orders_out`
// stream / shm `{base}_{shard}_out` — see balance_consumer.go) and turns
// each fill into ONE SERIALIZABLE transaction containing:
//
//	INSERT processed_trades (trade_id dedup — ON CONFLICT = replay, skip)
//	  → PostJournal of the fill's balanced four-legged mutation
//	    (journal_entries + ledger_lines + balances + ledger_entries +
//	     journal_sums — the landed Task 3.3.6 contract)
//
// FOUR-LEGGED MUTATION (ROLLING_MARGIN fill on pair BASE/QUOTE, qty q at
// price p, quoteAmt = q*p rounded to the DECIMAL(28,8) quantum):
//
//	buyer  locked   QUOTE −q·p        (order reservation consumed)
//	buyer  available BASE  +q−buyerFee (buyer fee charged in BASE)
//	seller locked   BASE  −q          (order reservation consumed)
//	seller available QUOTE +q·p−sellerFee (seller fee charged in QUOTE)
//
// GL view (per-currency zero-sum, spec §5.21): the released deliverable is
// a debit to 2010_CUSTOMER_LIABILITY_{ccy}; the counterparty proceeds are
// a credit to the same account; each side's fee is a credit to
// 4010_TRADING_FEE_REVENUE_{ccy}. Debits == credits per currency by
// construction.
//
// PHYSICAL DELIVERY (Task 3.3.22 balance side — spec §5.45.2): for a
// PHYSICAL_DELIVERY trade the deliverable legs do NOT net internally.
// The buyer's quote currency and the seller's base currency are moved
// available → locked on the wallet AND reclassified
// 2010_CUSTOMER_LIABILITY_{ccy} → 2011_PENDING_SETTLEMENT_DELIVERY_{ccy}
// in the GL, where they stay until SWIFT/CLS settlement confirms (the
// unwind journal is owned by the Phase-24 settlement-confirm flow, not
// this task). Proceeds are NOT credited to available at execution — the
// bought currency is delivered to the beneficiary's bank off-exchange.
// A fill whose two legs disagree on intent is a matching anomaly: the
// batch aborts SETTLEMENT_INTENT_MISMATCH (fail-closed, L0-class defect —
// the matcher must never pair PD with RM).
//
// INVARIANTS (Task 3.3.18): every journal rides the ledger's zero-sum
// enforcement — app-side Validate + in-tx re-aggregation + the deferred
// gl_journal_zero_sum_chk trigger; an imbalance aborts the whole tx and
// raises a P0 ops alert. SQLSTATE 40001/40P01 retry at 5/15/45ms +
// decorrelated jitter, max 3 attempts, then
// TRANSACTION_CONFLICT_RETRY_EXHAUSTED (HTTP 503). Account mutexes are
// Redis account:lock:{id} SETNX EX 10 taken in sorted order before the tx
// (§5.3 locking protocol).
//
// DEPENDENCY: the production resolver reads orders.settlement_intent /
// accounts.settlement_intent (migration 104, parallel task) — until that
// migration applies, PgxTradeResolver fails closed on the lookup.
package settlement

import (
	"context"
	stderrors "errors"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"exchange/internal/ledger"
	"exchange/pkg/decimal"
	excerrors "exchange/pkg/errors"
)

// Error codes emitted here; canonical registration (code + HTTP status)
// lands in Phase-05 Task 5.3.21 (spec §23).
const (
	// CodeTradeFillUnresolvable — HTTP 500, L1: fill's orders/instrument
	// could not be resolved (or settled intent lookup failed).
	CodeTradeFillUnresolvable = "TRADE_FILL_UNRESOLVABLE"
	// CodeSettlementIntentMismatch — HTTP 500, L0-class data anomaly: a
	// fill's legs carry different settlement intents (PD vs RM), which the
	// four-legged mutation cannot settle coherently.
	CodeSettlementIntentMismatch = "SETTLEMENT_INTENT_MISMATCH"
	// CodeFeeExceedsProceeds — HTTP 500, L0: a leg fee exceeds the
	// deliverable it is deducted from (negative credit would be theft).
	CodeFeeExceedsProceeds = "FEE_EXCEEDS_PROCEEDS"
)

// SettlementIntent mirrors the settlement_intent enum (migration 104;
// spec §5.45.2).
type SettlementIntent string

const (
	// IntentRollingMargin is the leveraged rolling-spot default: fills net
	// into wallet balances and accrue Tom-Next financing.
	IntentRollingMargin SettlementIntent = "ROLLING_MARGIN"
	// IntentPhysicalDelivery: deliverable currencies segregate into
	// 2011_PENDING_SETTLEMENT_DELIVERY_{ccy} until rail settlement
	// confirms; positions are exempt from rollover (Task 3.3.22).
	IntentPhysicalDelivery SettlementIntent = "PHYSICAL_DELIVERY"
)

// EngineFill is one decoded wire.TradeFill (order IDs + scaled price/qty;
// accounts, instrument and fees are resolved upstream by TradeResolver).
// ShardID is the consuming channel's shard — the wire message does not
// carry it; the consumer stamps it from channel config.
type EngineFill struct {
	TradeID     uint64
	BuyOrderID  uint64
	SellOrderID uint64
	Price       decimal.Decimal
	Qty         decimal.Decimal
	EngineSeq   uint64
	ShardID     int64
}

// ResolvedTrade is a fill with its counterparties, instrument currencies,
// per-side fees and settlement intents resolved.
type ResolvedTrade struct {
	Fill            EngineFill
	InstrumentID    int64
	BuyerAccountID  int64
	SellerAccountID int64
	BaseCurrency    string
	QuoteCurrency   string
	// BuyerFee is charged in BASE currency (deducted from the base the
	// buyer receives); SellerFee is charged in QUOTE currency (deducted
	// from the quote the seller receives) — the standard receive-side
	// netting convention. Zero fees are legal.
	BuyerFee     decimal.Decimal
	SellerFee    decimal.Decimal
	BuyerIntent  SettlementIntent
	SellerIntent SettlementIntent
}

// TradeResolver resolves an engine fill to its balance-mutation inputs.
// The production implementation is PgxTradeResolver (orders ⨝ instruments
// ⨝ accounts, fees from the trades row when it has landed).
type TradeResolver interface {
	Resolve(ctx context.Context, f EngineFill) (ResolvedTrade, error)
}

// FillOutcome is the per-fill result of a committed batch.
type FillOutcome struct {
	TradeID   uint64
	JournalID int64
	Applied   bool // false when Duplicate
	Duplicate bool // trade_id already present in processed_trades
}

// ---------------------------------------------------------------------------
// Persistence seams (same pattern as PositionStore — production pgx
// implementation below, fakes in tests).
// ---------------------------------------------------------------------------

// balanceTx is the transactional surface used inside a batch commit.
type balanceTx interface {
	// RecordProcessed inserts the processed_trades dedup row; applied is
	// false when trade_id already exists (idempotent replay → skip).
	RecordProcessed(ctx context.Context, tradeID, shardID int64) (applied bool, err error)
	// PostJournal posts a validated journal inside the caller's tx (the
	// Task 3.3.6 contract — no locks/retry/dispatch of its own).
	PostJournal(ctx context.Context, j ledger.Journal) (ledger.PostResult, error)
}

// balanceStore runs fn inside a SERIALIZABLE transaction, rolling back on
// error. Production: pgxBalanceStore.
type balanceStore interface {
	InTx(ctx context.Context, fn func(ctx context.Context, tx balanceTx) error) error
}

// accountLocker is the §5.3 Redis mutex surface (implemented by the
// LedgerService lock adapter — sorted, all-or-nothing SETNX).
type accountLocker interface {
	LockAccounts(ctx context.Context, ids []int64, token string) ([]int64, error)
	UnlockAccounts(ids []int64, token string)
}

// eventDispatcher publishes committed BalanceChanged events.
type eventDispatcher interface {
	Dispatch(ctx context.Context, events []ledger.BalanceEvent) error
}

// journalPoster adapts the concrete tx-scoped ledger contract.
type journalPoster interface {
	PostJournal(ctx context.Context, tx pgx.Tx, j ledger.Journal) (ledger.PostResult, error)
}

// managedPoster is the full Post path (locks + §5.40 retry + dispatch) —
// used by the compensation workflow where no batch tx is in flight.
type managedPoster interface {
	Post(ctx context.Context, j ledger.Journal) (ledger.PostResult, error)
}

// ---------------------------------------------------------------------------
// BalanceService
// ---------------------------------------------------------------------------

// BalanceService applies engine trade fills to client balances.
type BalanceService struct {
	store    balanceStore
	locks    accountLocker
	dispatch eventDispatcher
	poster   managedPoster // full Post path for out-of-batch journals
	alerter  OpsAlerter    // may be nil → alerts skipped (wired in production)
	postedBy string
	maxTries int
	backoff  []time.Duration
}

// NewBalanceService wires the service. store is required (fail-closed).
// ls supplies the §5.3 machinery: account locks, PostJournal and
// BalanceChanged dispatch — the BalanceService rides the landed Task
// 3.3.6 contract rather than re-implementing it.
func NewBalanceService(pool *pgxpool.Pool, ls *LedgerService, alerter OpsAlerter) (*BalanceService, error) {
	if pool == nil {
		return nil, fmt.Errorf("balance service: nil pgx pool")
	}
	if ls == nil {
		return nil, fmt.Errorf("balance service: nil ledger service — posting contract is mandatory")
	}
	return &BalanceService{
		store:    pgxBalanceStore{pool: pool, poster: ls},
		locks:    ledgerLockAdapter{ls},
		dispatch: ledgerDispatchAdapter{ls},
		poster:   ls,
		alerter:  alerter,
		postedBy: "balance-service",
		maxTries: 3,
		backoff:  []time.Duration{5 * time.Millisecond, 15 * time.Millisecond, 45 * time.Millisecond},
	}, nil
}

// newBalanceServiceForTest injects seams directly (unit tests).
func newBalanceServiceForTest(store balanceStore, locks accountLocker, disp eventDispatcher,
	poster managedPoster, alerter OpsAlerter) *BalanceService {
	return &BalanceService{
		store: store, locks: locks, dispatch: disp, poster: poster, alerter: alerter,
		postedBy: "balance-service-test",
		maxTries: 3,
		backoff:  []time.Duration{5 * time.Millisecond, 15 * time.Millisecond, 45 * time.Millisecond},
	}
}

// ProcessFills settles a batch of resolved trades atomically per fill
// inside ONE SERIALIZABLE transaction per attempt. On a non-retryable
// batch failure with >1 fill, the batch is isolated fill-by-fill so one
// poisoned trade cannot strand its neighbours; per-fill failures are
// collected into the returned error (fail-closed — nothing is silently
// dropped).
func (s *BalanceService) ProcessFills(ctx context.Context, trades []ResolvedTrade) ([]FillOutcome, error) {
	if len(trades) == 0 {
		return nil, nil
	}
	outcomes, err := s.commitWithRetry(ctx, trades)
	if err == nil {
		return outcomes, nil
	}
	if isRetryableConflict(err) || len(trades) == 1 {
		return nil, err
	}
	var coded *excerrors.Error
	if stderrors.As(err, &coded) && coded.Code == ledger.CodeBalanceDispatchFailed {
		// Committed but not fully dispatched — funds are final; the caller
		// must resync downstream, never repost the batch.
		return outcomes, err
	}
	// Isolate the poisoned fill: retry each trade as its own batch.
	var failed []string
	var ok []FillOutcome
	for _, rt := range trades {
		o, ferr := s.commitWithRetry(ctx, []ResolvedTrade{rt})
		if ferr != nil {
			failed = append(failed, fmt.Sprintf("trade %d: %v", rt.Fill.TradeID, ferr))
			s.alertOps(ctx, SeverityP0, "FILL_SETTLEMENT_FAILED",
				fmt.Sprintf("trade %d settlement aborted", rt.Fill.TradeID), ferr, rt)
			continue
		}
		ok = append(ok, o...)
	}
	if len(failed) > 0 {
		return ok, fmt.Errorf("balance: %d/%d fills failed: %v",
			len(failed), len(trades), failed)
	}
	return ok, nil
}

// commitWithRetry runs commitBatch under the §5.40 conflict-retry
// schedule: SQLSTATE 40001/40P01 → backoff 5/15/45ms + jitter, max 3
// attempts, exhaustion → TRANSACTION_CONFLICT_RETRY_EXHAUSTED (HTTP 503).
func (s *BalanceService) commitWithRetry(ctx context.Context, trades []ResolvedTrade) ([]FillOutcome, error) {
	journals := make([]ledger.Journal, 0, len(trades))
	for i := range trades {
		j, err := buildFillJournal(trades[i], s.postedBy)
		if err != nil {
			return nil, err // structural defect — never reaches a tx
		}
		journals = append(journals, j)
	}
	accounts := affectedAccounts(journals)

	token, err := lockToken()
	if err != nil {
		return nil, fmt.Errorf("balance: lock token: %w", err)
	}
	locked, err := s.lockAccounts(ctx, accounts, token)
	if err != nil {
		return nil, err
	}
	defer s.unlockAccounts(locked, token)

	var outcomes []FillOutcome
	var lastErr error
	for attempt := 0; attempt < s.maxTries; attempt++ {
		outcomes, err = s.commitBatch(ctx, trades, journals)
		if err == nil {
			break
		}
		lastErr = err
		if !isRetryableConflict(err) {
			s.maybeAlertImbalance(ctx, err)
			return outcomes, err // may hold committed outcomes (dispatch-fail case)
		}
		if attempt+1 < s.maxTries {
			sleep := s.backoff[min(attempt, len(s.backoff)-1)]
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(sleep + jitter(sleep)):
			}
		}
	}
	if err != nil {
		return nil, excerrors.Wrap(ledger.CodeTxnConflictExhausted,
			fmt.Sprintf("balance: batch aborted after %d attempts", s.maxTries), lastErr)
	}
	return outcomes, nil // BalanceChanged events already dispatched post-commit
}

// commitBatch is one attempt: lock already held; all fills posted in a
// single SERIALIZABLE tx; BalanceChanged events dispatched after commit.
func (s *BalanceService) commitBatch(ctx context.Context, trades []ResolvedTrade,
	journals []ledger.Journal) ([]FillOutcome, error) {

	outcomes := make([]FillOutcome, 0, len(trades))
	var events []ledger.BalanceEvent
	err := s.store.InTx(ctx, func(ctx context.Context, tx balanceTx) error {
		for i := range trades {
			applied, err := tx.RecordProcessed(ctx, int64(trades[i].Fill.TradeID), trades[i].Fill.ShardID)
			if err != nil {
				return fmt.Errorf("balance: dedup insert trade %d: %w", trades[i].Fill.TradeID, err)
			}
			if !applied {
				outcomes = append(outcomes, FillOutcome{TradeID: trades[i].Fill.TradeID, Duplicate: true})
				continue
			}
			res, err := tx.PostJournal(ctx, journals[i])
			if err != nil {
				return err
			}
			outcomes = append(outcomes, FillOutcome{
				TradeID: trades[i].Fill.TradeID, JournalID: res.JournalID, Applied: true,
			})
			events = append(events, res.Events...)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if s.dispatch != nil && len(events) > 0 {
		if err := s.dispatch.Dispatch(ctx, events); err != nil {
			return outcomes, err // committed — funds final; resync downstream
		}
	}
	return outcomes, nil
}

func (s *BalanceService) lockAccounts(ctx context.Context, ids []int64, token string) ([]int64, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	if s.locks == nil {
		return nil, excerrors.New(ledger.CodeLedgerLockUnavailable,
			"balance mutation requires the Redis account mutex backend (§5.3) — locker is nil")
	}
	return s.locks.LockAccounts(ctx, ids, token)
}

func (s *BalanceService) unlockAccounts(ids []int64, token string) {
	if s.locks != nil {
		s.locks.UnlockAccounts(ids, token)
	}
}

// maybeAlertImbalance raises the P0 page when a batch aborts on the
// zero-sum invariant (spec §5.40 — LEDGER_IMBALANCE_ABORT is L0 fatal).
func (s *BalanceService) maybeAlertImbalance(ctx context.Context, err error) {
	var e *excerrors.Error
	if !stderrors.As(err, &e) || e.Code != ledger.CodeLedgerImbalanceAbort {
		return
	}
	s.alertOps(ctx, SeverityP0, ledger.CodeLedgerImbalanceAbort,
		"ledger zero-sum invariant violated — batch aborted", err, ResolvedTrade{})
}

// ---------------------------------------------------------------------------
// Journal construction — the pure half (no I/O; unit-tested directly).
// ---------------------------------------------------------------------------

// buildFillJournal turns a resolved trade into its balanced posting.
// Zero-sum holds per currency by construction; Validate re-verifies.
func buildFillJournal(rt ResolvedTrade, postedBy string) (ledger.Journal, error) {
	if err := validateResolved(rt); err != nil {
		return ledger.Journal{}, err
	}
	if rt.BuyerIntent != rt.SellerIntent {
		return ledger.Journal{}, excerrors.New(CodeSettlementIntentMismatch, fmt.Sprintf(
			"trade %d: buyer intent %s != seller intent %s — matching anomaly",
			rt.Fill.TradeID, rt.BuyerIntent, rt.SellerIntent))
	}
	if rt.BuyerIntent == IntentPhysicalDelivery {
		return buildDeliveryJournal(rt, postedBy)
	}
	return buildRollingJournal(rt, postedBy)
}

func validateResolved(rt ResolvedTrade) error {
	f := rt.Fill
	if f.TradeID == 0 || f.BuyOrderID == 0 || f.SellOrderID == 0 ||
		rt.BuyerAccountID <= 0 || rt.SellerAccountID <= 0 || rt.InstrumentID <= 0 {
		return excerrors.New(CodeInvalidFill, "fill missing trade/order/account/instrument id")
	}
	if !f.Price.IsPositive() || !f.Qty.IsPositive() {
		return excerrors.New(CodeInvalidFill, "fill price and quantity must be > 0")
	}
	if len(rt.BaseCurrency) != 3 || len(rt.QuoteCurrency) != 3 {
		return excerrors.New(CodeInvalidFill, "resolved currencies must be 3-letter ISO")
	}
	if rt.BuyerFee.IsNegative() || rt.SellerFee.IsNegative() {
		return excerrors.New(CodeInvalidFill, "negative leg fee")
	}
	switch rt.BuyerIntent {
	case IntentRollingMargin, IntentPhysicalDelivery:
	default:
		return excerrors.New(CodeInvalidFill, fmt.Sprintf("invalid buyer intent %q", rt.BuyerIntent))
	}
	switch rt.SellerIntent {
	case IntentRollingMargin, IntentPhysicalDelivery:
	default:
		return excerrors.New(CodeInvalidFill, fmt.Sprintf("invalid seller intent %q", rt.SellerIntent))
	}
	return nil
}

// quoteAmount is the quote-currency value of the fill at the DECIMAL(28,8)
// quantum. qty·price can carry up to 16 decimal places; the posted amount
// is rounded half-up to the ledger quantum (both legs use the identical
// value, so zero-sum is preserved exactly).
func quoteAmount(rt ResolvedTrade) decimal.Decimal {
	return rt.Fill.Qty.Mul(rt.Fill.Price).Round(8)
}

// buildRollingJournal is the four-legged mutation for ROLLING_MARGIN
// fills (Task 3.3.1): reservations consumed, proceeds credited net of
// receive-side fees, fee revenue booked to 4010.
func buildRollingJournal(rt ResolvedTrade, postedBy string) (ledger.Journal, error) {
	q := rt.Fill.Qty
	qa := quoteAmount(rt)
	buyerCredit := q.Sub(rt.BuyerFee)
	sellerCredit := qa.Sub(rt.SellerFee)
	if buyerCredit.IsNegative() {
		return ledger.Journal{}, excerrors.New(CodeFeeExceedsProceeds, fmt.Sprintf(
			"trade %d: buyer fee %s exceeds base delivered %s",
			rt.Fill.TradeID, rt.BuyerFee, q))
	}
	if sellerCredit.IsNegative() {
		return ledger.Journal{}, excerrors.New(CodeFeeExceedsProceeds, fmt.Sprintf(
			"trade %d: seller fee %s exceeds quote proceeds %s",
			rt.Fill.TradeID, rt.SellerFee, qa))
	}

	desc := fmt.Sprintf("trade %d %s/%s fill", rt.Fill.TradeID, rt.BaseCurrency, rt.QuoteCurrency)
	j := ledger.Journal{
		EntryType:      ledger.EntryTradeFill,
		ReferenceID:    int64(rt.Fill.TradeID),
		Description:    desc,
		PostedBy:       postedBy,
		IdempotencyKey: fmt.Sprintf("trade-fill:%d", rt.Fill.TradeID),
	}

	// Quote currency: buyer deliverable released → seller proceeds + fee.
	j.Lines = append(j.Lines,
		ledger.DebitLine(ledger.CustomerLiability(rt.QuoteCurrency), rt.QuoteCurrency, qa,
			fmt.Sprintf("buyer %d quote released", rt.BuyerAccountID)),
		ledger.CreditLine(ledger.CustomerLiability(rt.QuoteCurrency), rt.QuoteCurrency, sellerCredit,
			fmt.Sprintf("seller %d quote proceeds net of fee", rt.SellerAccountID)),
	)
	if rt.SellerFee.IsPositive() {
		j.Lines = append(j.Lines,
			ledger.CreditLine(ledger.TradingFeeRevenue(rt.QuoteCurrency), rt.QuoteCurrency, rt.SellerFee,
				fmt.Sprintf("seller %d trading fee", rt.SellerAccountID)))
	}

	// Base currency: seller deliverable released → buyer proceeds + fee.
	j.Lines = append(j.Lines,
		ledger.DebitLine(ledger.CustomerLiability(rt.BaseCurrency), rt.BaseCurrency, q,
			fmt.Sprintf("seller %d base delivered", rt.SellerAccountID)),
		ledger.CreditLine(ledger.CustomerLiability(rt.BaseCurrency), rt.BaseCurrency, buyerCredit,
			fmt.Sprintf("buyer %d base received net of fee", rt.BuyerAccountID)),
	)
	if rt.BuyerFee.IsPositive() {
		j.Lines = append(j.Lines,
			ledger.CreditLine(ledger.TradingFeeRevenue(rt.BaseCurrency), rt.BaseCurrency, rt.BuyerFee,
				fmt.Sprintf("buyer %d trading fee", rt.BuyerAccountID)))
	}

	j.Effects = []ledger.AccountEffect{
		{AccountID: rt.BuyerAccountID, Currency: rt.QuoteCurrency,
			LockedDelta: qa.Neg()}, // reservation consumed
		{AccountID: rt.BuyerAccountID, Currency: rt.BaseCurrency,
			AvailableDelta: buyerCredit},
		{AccountID: rt.SellerAccountID, Currency: rt.BaseCurrency,
			LockedDelta: q.Neg()}, // reservation consumed
		{AccountID: rt.SellerAccountID, Currency: rt.QuoteCurrency,
			AvailableDelta: sellerCredit},
	}
	return j, j.Validate()
}

// buildDeliveryJournal is the PHYSICAL_DELIVERY execution posting
// (Task 3.3.22, spec §5.45.2): each side's deliverable moves
// available → locked on the wallet and is reclassified
// 2010 → 2011_PENDING_SETTLEMENT_DELIVERY in the GL until SWIFT/CLS
// settlement confirms. Receive legs are not credited at execution — the
// bought currency is delivered to the beneficiary off-exchange at the
// value date; fee recognition travels with the settlement-confirm journal.
func buildDeliveryJournal(rt ResolvedTrade, postedBy string) (ledger.Journal, error) {
	q := rt.Fill.Qty
	qa := quoteAmount(rt)
	desc := fmt.Sprintf("trade %d %s/%s physical-delivery lock", rt.Fill.TradeID, rt.BaseCurrency, rt.QuoteCurrency)
	j := ledger.Journal{
		EntryType:      ledger.EntryTradeFill,
		ReferenceID:    int64(rt.Fill.TradeID),
		Description:    desc,
		PostedBy:       postedBy,
		IdempotencyKey: fmt.Sprintf("trade-fill:%d", rt.Fill.TradeID),
		Lines: []ledger.Line{
			// Buyer's quote deliverable reclassified to pending delivery.
			ledger.DebitLine(ledger.CustomerLiability(rt.QuoteCurrency), rt.QuoteCurrency, qa,
				fmt.Sprintf("buyer %d quote segregated for delivery", rt.BuyerAccountID)),
			ledger.CreditLine(ledger.PendingSettlementDelivery(rt.QuoteCurrency), rt.QuoteCurrency, qa,
				fmt.Sprintf("pending settlement delivery (%s) trade %d", rt.QuoteCurrency, rt.Fill.TradeID)),
			// Seller's base deliverable reclassified to pending delivery.
			ledger.DebitLine(ledger.CustomerLiability(rt.BaseCurrency), rt.BaseCurrency, q,
				fmt.Sprintf("seller %d base segregated for delivery", rt.SellerAccountID)),
			ledger.CreditLine(ledger.PendingSettlementDelivery(rt.BaseCurrency), rt.BaseCurrency, q,
				fmt.Sprintf("pending settlement delivery (%s) trade %d", rt.BaseCurrency, rt.Fill.TradeID)),
		},
		Effects: []ledger.AccountEffect{
			{AccountID: rt.BuyerAccountID, Currency: rt.QuoteCurrency,
				AvailableDelta: qa.Neg(), LockedDelta: qa},
			{AccountID: rt.SellerAccountID, Currency: rt.BaseCurrency,
				AvailableDelta: q.Neg(), LockedDelta: q},
		},
	}
	return j, j.Validate()
}

// affectedAccounts is the sorted distinct account set across journals —
// deterministic lock order (deadlock-safe), mirroring
// Journal.AffectedAccounts over a whole batch.
func affectedAccounts(journals []ledger.Journal) []int64 {
	seen := map[int64]struct{}{}
	for _, j := range journals {
		for _, e := range j.Effects {
			seen[e.AccountID] = struct{}{}
		}
	}
	ids := make([]int64, 0, len(seen))
	for id := range seen {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(a, b int) bool { return ids[a] < ids[b] })
	return ids
}

// ---------------------------------------------------------------------------
// Production adapters
// ---------------------------------------------------------------------------

// pgxBalanceStore runs batches inside pool SERIALIZABLE transactions.
type pgxBalanceStore struct {
	pool   *pgxpool.Pool
	poster journalPoster
}

func (s pgxBalanceStore) InTx(ctx context.Context, fn func(ctx context.Context, tx balanceTx) error) error {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return fmt.Errorf("balance tx begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := fn(ctx, pgxBalanceTx{tx: tx, poster: s.poster}); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("balance tx commit: %w", err)
	}
	return nil
}

type pgxBalanceTx struct {
	tx     pgx.Tx
	poster journalPoster
}

// RecordProcessed is the trade_id dedup (migration 022): ON CONFLICT the
// row already exists → the fill was applied by an earlier commit; skip.
func (t pgxBalanceTx) RecordProcessed(ctx context.Context, tradeID, shardID int64) (bool, error) {
	tag, err := t.tx.Exec(ctx, `
		INSERT INTO processed_trades (trade_id, processed_at, shard_id)
		VALUES ($1, now(), $2)
		ON CONFLICT (trade_id) DO NOTHING`, tradeID, shardID)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

func (t pgxBalanceTx) PostJournal(ctx context.Context, j ledger.Journal) (ledger.PostResult, error) {
	return t.poster.PostJournal(ctx, t.tx, j)
}

// ledgerLockAdapter / ledgerDispatchAdapter expose the LedgerService's
// §5.3 machinery to BalanceService without widening its public API.
type ledgerLockAdapter struct{ s *LedgerService }

func (a ledgerLockAdapter) LockAccounts(ctx context.Context, ids []int64, token string) ([]int64, error) {
	return a.s.lockAccounts(ctx, ids, token)
}
func (a ledgerLockAdapter) UnlockAccounts(ids []int64, token string) {
	a.s.unlockAccounts(ids, token)
}

type ledgerDispatchAdapter struct{ s *LedgerService }

func (a ledgerDispatchAdapter) Dispatch(ctx context.Context, events []ledger.BalanceEvent) error {
	return a.s.dispatch(ctx, events)
}

// ---------------------------------------------------------------------------
// PgxTradeResolver — orders ⨝ instruments ⨝ accounts, fees from trades.
// ---------------------------------------------------------------------------

// PgxTradeResolver resolves engine fills against PostgreSQL. The
// settlement_intent columns come from migration 104 (parallel task):
// order-level intent wins over the account default (spec §5.4 note).
type PgxTradeResolver struct {
	Pool *pgxpool.Pool
}

// NewPgxTradeResolver wraps a pool.
func NewPgxTradeResolver(pool *pgxpool.Pool) *PgxTradeResolver {
	return &PgxTradeResolver{Pool: pool}
}

// Resolve loads both order legs, the instrument currencies and each
// side's effective settlement intent, then attaches fees from the trades
// row when it has already landed (buyer_fee/seller_fee, migration 006);
// a missing trades row means zero fees at fill time.
func (r *PgxTradeResolver) Resolve(ctx context.Context, f EngineFill) (ResolvedTrade, error) {
	rt := ResolvedTrade{Fill: f}
	err := r.Pool.QueryRow(ctx, `
		SELECT bo.account_id, so.account_id, bo.instrument_id,
		       i.base_currency, i.quote_currency,
		       COALESCE(bo.settlement_intent::text, ab.settlement_intent::text, 'ROLLING_MARGIN'),
		       COALESCE(so.settlement_intent::text, sa.settlement_intent::text, 'ROLLING_MARGIN')
		  FROM orders bo
		  JOIN orders so      ON so.id = $2
		  JOIN instruments i  ON i.id = bo.instrument_id
		  JOIN accounts ab    ON ab.id = bo.account_id
		  JOIN accounts sa    ON sa.id = so.account_id
		 WHERE bo.id = $1`,
		int64(f.BuyOrderID), int64(f.SellOrderID)).
		Scan(&rt.BuyerAccountID, &rt.SellerAccountID, &rt.InstrumentID,
			&rt.BaseCurrency, &rt.QuoteCurrency,
			(*string)(&rt.BuyerIntent), (*string)(&rt.SellerIntent))
	if err != nil {
		return ResolvedTrade{}, excerrors.Wrap(CodeTradeFillUnresolvable, fmt.Sprintf(
			"resolve fill trade %d (orders %d/%d)", f.TradeID, f.BuyOrderID, f.SellOrderID), err)
	}
	var bf, sf *decimal.Decimal
	var shard *int16
	err = r.Pool.QueryRow(ctx, `
		SELECT buyer_fee, seller_fee, shard_id FROM trades WHERE id = $1`,
		int64(f.TradeID)).Scan(&bf, &sf, &shard)
	switch {
	case err == nil:
		if bf != nil {
			rt.BuyerFee = *bf
		}
		if sf != nil {
			rt.SellerFee = *sf
		}
		if shard != nil {
			rt.Fill.ShardID = int64(*shard)
		}
	case stderrors.Is(err, pgx.ErrNoRows):
		// trades row not yet persisted — zero fees at fill time.
	default:
		return ResolvedTrade{}, excerrors.Wrap(CodeTradeFillUnresolvable,
			fmt.Sprintf("resolve fees for trade %d", f.TradeID), err)
	}
	return rt, nil
}
