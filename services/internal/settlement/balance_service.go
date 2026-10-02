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
	// Raw is the verbatim inbound Event frame (trace block included) —
	// retained for post-commit JetStream republication on the
	// trades/settlements streams, byte-identical to the bridge's relay.
	Raw []byte
}

// ResolvedTrade is a fill with its counterparties, instrument currencies,
// per-side fees and settlement intents resolved.
type ResolvedTrade struct {
	Fill            EngineFill
	InstrumentID    int64
	Symbol          string
	BuyerAccountID  int64
	SellerAccountID int64
	BaseCurrency    string
	QuoteCurrency   string
	// BuyerFee is charged in BASE currency (deducted from the base the
	// buyer receives); SellerFee is charged in QUOTE currency (deducted
	// from the quote the seller receives) — the standard receive-side
	// netting convention. Signed: a negative value is a rebate credited
	// to the account. Zero fees are legal.
	BuyerFee     decimal.Decimal
	SellerFee    decimal.Decimal
	BuyerIntent  SettlementIntent
	SellerIntent SettlementIntent
	// SettlementDate is the FX value date for PHYSICAL_DELIVERY fills —
	// recorded on the trades row; recon gates fee expectation on it
	// (fees post at settlement-confirm, per §5.45.2). Nil for
	// ROLLING_MARGIN fills whose fees post at execution.
	SettlementDate *time.Time
	// FeeJournals are the FEE-type journals posted inside the fill commit
	// (per-side trading fee per Task 3.3.4 plus raw-model commission /
	// maker-rebate per Task 3.3.13/3.3.17). Empty for PHYSICAL_DELIVERY —
	// those fee legs are built from trades.*_fee at settlement confirm.
	FeeJournals []ledger.Journal
	// Volumes are the commission volume accruals (Task 3.3.13) written
	// inside the same commit — monthly tier tracking is usage
	// measurement, recorded at fill time for both intents.
	Volumes []CommissionVolumeAccrual
}

// CommissionVolumeAccrual is one account's monthly-volume upsert from a
// commission Quote — accrued inside the settlement tx.
type CommissionVolumeAccrual struct {
	AccountID int64
	Month     time.Time
	DeltaUSD  decimal.Decimal
}

// TradeResolver resolves an engine fill to its balance-mutation inputs.
// The production implementation is PgxTradeResolver (orders ⨝ instruments
// ⨝ accounts, fees from the trades row when it has landed).
type TradeResolver interface {
	Resolve(ctx context.Context, f EngineFill) (ResolvedTrade, error)
}

// BatchTradeResolver is the set-based extension consumed by FillConsumer:
// ResolveBatch resolves a whole pump window in two queries. Output order
// matches input order; any unresolvable leg fails the batch (fail-closed).
type BatchTradeResolver interface {
	TradeResolver
	ResolveBatch(ctx context.Context, fills []EngineFill) ([]ResolvedTrade, error)
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
	// rawFrame persists the verbatim Event frame (migration 281) — the
	// durable source for boot-time republish repair of the
	// commit→publish crash window.
	RecordProcessed(ctx context.Context, tradeID, shardID int64, rawFrame []byte) (applied bool, err error)
	// RecordTrade writes the public tape row (trades, id = the engine
	// trade id recon keys expected fees on) inside the caller's tx — the
	// tape and the ledger commit or abort together, so the public tape
	// never advertises an unsettled fill. Called only for rows that
	// cleared processed_trades dedup.
	RecordTrade(ctx context.Context, t ResolvedTrade) error
	// PostJournal posts a validated journal inside the caller's tx (the
	// Task 3.3.6 contract — no locks/retry/dispatch of its own).
	PostJournal(ctx context.Context, j ledger.Journal) (ledger.PostResult, error)
	// AccrueVolume upserts one commission monthly-volume row inside the
	// fill commit — atomic with the ledger so aborted attempts and
	// replays never double-count (Task 3.3.13 tier-boundary semantics).
	AccrueVolume(ctx context.Context, v CommissionVolumeAccrual) error
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
	journals := make([][]ledger.Journal, 0, len(trades))
	for i := range trades {
		j, err := buildFillJournal(trades[i], s.postedBy)
		if err != nil {
			return nil, err // structural defect — never reaches a tx
		}
		// Element 0 is the fill journal (drives FillOutcome.JournalID);
		// FeeJournals append — FEE legs (trading fee / commission /
		// rebate) post in the same tx, after the fill journal credits
		// the receive-side balances they net against.
		journals = append(journals, append([]ledger.Journal{j}, trades[i].FeeJournals...))
	}
	// Set-based commit for multi-fill batches — measured ~7x the serial
	// row-by-row path on identical SERIALIZABLE semantics. (A parallel
	// partition variant was tried and removed: SSI abort churn under
	// concurrent SERIALIZABLE writers made it net-slower.)
	_, setOK := s.store.(setBasedStore)

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
	useSet := setOK && len(trades) > 1 // small batches still use the set path
	for attempt := 0; attempt < s.maxTries; attempt++ {
		if useSet {
			outcomes, err = s.commitBatchSetDispatch(ctx, trades, journals)
		} else {
			outcomes, err = s.commitBatch(ctx, trades, journals)
		}
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
	journals [][]ledger.Journal) ([]FillOutcome, error) {

	outcomes := make([]FillOutcome, 0, len(trades))
	var events []ledger.BalanceEvent
	err := s.store.InTx(ctx, func(ctx context.Context, tx balanceTx) error {
		for i := range trades {
			applied, err := tx.RecordProcessed(ctx, int64(trades[i].Fill.TradeID),
				trades[i].Fill.ShardID, trades[i].Fill.Raw)
			if err != nil {
				return fmt.Errorf("balance: dedup insert trade %d: %w", trades[i].Fill.TradeID, err)
			}
			if !applied {
				outcomes = append(outcomes, FillOutcome{TradeID: trades[i].Fill.TradeID, Duplicate: true})
				continue
			}
			if err := tx.RecordTrade(ctx, trades[i]); err != nil {
				return fmt.Errorf("balance: tape insert trade %d: %w", trades[i].Fill.TradeID, err)
			}
			var fillRes ledger.PostResult
			for _, j := range journals[i] {
				res, err := tx.PostJournal(ctx, j)
				if err != nil {
					return fmt.Errorf("balance: journal post trade %d (%s): %w",
						trades[i].Fill.TradeID, j.IdempotencyKey, err)
				}
				events = append(events, res.Events...)
				if j.EntryType == ledger.EntryTradeFill {
					fillRes = res
				}
			}
			for _, v := range trades[i].Volumes {
				if err := tx.AccrueVolume(ctx, v); err != nil {
					return fmt.Errorf("balance: volume accrual trade %d acct %d: %w",
						trades[i].Fill.TradeID, v.AccountID, err)
				}
			}
			outcomes = append(outcomes, FillOutcome{
				TradeID: trades[i].Fill.TradeID, JournalID: fillRes.JournalID, Applied: true,
			})
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
// fills (Task 3.3.1): reservations consumed, proceeds credited GROSS.
// Trading fees are NOT netted here — they post as separate FEE journals
// (fee:{trade}:{acct}:{role}) inside the same commit so recon's
// collected-fee and 4010-revenue legs see them (FeeService doc contract);
// the net wallet effect is identical.
func buildRollingJournal(rt ResolvedTrade, postedBy string) (ledger.Journal, error) {
	q := rt.Fill.Qty
	qa := quoteAmount(rt)
	buyerCredit := q
	sellerCredit := qa

	desc := fmt.Sprintf("trade %d %s/%s fill", rt.Fill.TradeID, rt.BaseCurrency, rt.QuoteCurrency)
	j := ledger.Journal{
		EntryType:      ledger.EntryTradeFill,
		ReferenceID:    int64(rt.Fill.TradeID),
		Description:    desc,
		PostedBy:       postedBy,
		IdempotencyKey: fmt.Sprintf("trade-fill:%d", rt.Fill.TradeID),
	}

	// Quote currency: buyer deliverable released → seller proceeds.
	j.Lines = append(j.Lines,
		ledger.DebitLine(ledger.CustomerLiability(rt.QuoteCurrency), rt.QuoteCurrency, qa,
			fmt.Sprintf("buyer %d quote released", rt.BuyerAccountID)),
		ledger.CreditLine(ledger.CustomerLiability(rt.QuoteCurrency), rt.QuoteCurrency, sellerCredit,
			fmt.Sprintf("seller %d quote proceeds", rt.SellerAccountID)),
	)

	// Base currency: seller deliverable released → buyer proceeds.
	j.Lines = append(j.Lines,
		ledger.DebitLine(ledger.CustomerLiability(rt.BaseCurrency), rt.BaseCurrency, q,
			fmt.Sprintf("seller %d base delivered", rt.SellerAccountID)),
		ledger.CreditLine(ledger.CustomerLiability(rt.BaseCurrency), rt.BaseCurrency, buyerCredit,
			fmt.Sprintf("buyer %d base received", rt.BuyerAccountID)),
	)

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
func affectedAccounts(journals [][]ledger.Journal) []int64 {
	seen := map[int64]struct{}{}
	for _, js := range journals {
		for _, j := range js {
			for _, e := range j.Effects {
				seen[e.AccountID] = struct{}{}
			}
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
// raw_frame (migration 281) rides the same insert — it is the durable
// republish source for the boot-time backlog repair.
func (t pgxBalanceTx) RecordProcessed(ctx context.Context, tradeID, shardID int64, rawFrame []byte) (bool, error) {
	tag, err := t.tx.Exec(ctx, `
		INSERT INTO processed_trades (trade_id, processed_at, shard_id, raw_frame)
		VALUES ($1, now(), $2, $3)
		ON CONFLICT (trade_id) DO NOTHING`, tradeID, shardID, rawFrame)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// RecordTrade lands the public tape row: id is the engine trade id
// (OVERRIDING SYSTEM VALUE — the trades.id identity column doubles as
// the engine trade-id key recon's fee expectation reads), shard/seq
// come from the fill, settlement_date is the PD value date (NULL for
// rolling margin — execution-time fees). ON CONFLICT DO NOTHING covers
// the (id, created_at) partition key on a same-instant re-delivery.
func (t pgxBalanceTx) RecordTrade(ctx context.Context, r ResolvedTrade) error {
	_, err := t.tx.Exec(ctx, `
		INSERT INTO trades (id, instrument_id, buy_order_id, sell_order_id,
		    buyer_account_id, seller_account_id, price, quantity,
		    buyer_fee, seller_fee, settlement_date, shard_id, trade_seq)
		OVERRIDING SYSTEM VALUE
		VALUES ($1,$2,$3,$4,$5,$6,$7::numeric,$8::numeric,$9::numeric,$10::numeric,$11,$12,$13)
		ON CONFLICT DO NOTHING`,
		int64(r.Fill.TradeID), r.InstrumentID, int64(r.Fill.BuyOrderID),
		int64(r.Fill.SellOrderID), r.BuyerAccountID, r.SellerAccountID,
		r.Fill.Price.String(), r.Fill.Qty.String(),
		r.BuyerFee.String(), r.SellerFee.String(), r.SettlementDate,
		int16(r.Fill.ShardID), int64(r.Fill.EngineSeq))
	return err
}

func (t pgxBalanceTx) PostJournal(ctx context.Context, j ledger.Journal) (ledger.PostResult, error) {
	return t.poster.PostJournal(ctx, t.tx, j)
}

// AccrueVolume applies the shared commission upsert inside the commit.
func (t pgxBalanceTx) AccrueVolume(ctx context.Context, v CommissionVolumeAccrual) error {
	_, err := t.tx.Exec(ctx, recordFillVolumeSQL, v.AccountID, v.Month, v.DeltaUSD.String())
	return err
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
// PgxTradeResolver — orders ⨝ instruments ⨝ accounts; fees COMPUTED via
// FeeService (Task 3.3.4) + CommissionEngine (Task 3.3.13/3.3.17).
// ---------------------------------------------------------------------------

// legFacts carries the order/instrument/account columns fee and value-date
// computation needs beyond ResolvedTrade's scalar fields.
type legFacts struct {
	symbol          string
	lotSize         decimal.Decimal
	settlementCycle int
	boSeq, soSeq    int64
	buyerVip        int
	sellerVip       int
	buyerMakerBps   decimal.Decimal // vip_tier_schedule maker rate
	sellerMakerBps  decimal.Decimal
}

// PgxTradeResolver resolves engine fills against PostgreSQL. The
// settlement_intent columns come from migration 104 (parallel task):
// order-level intent wins over the account default (spec §5.4 note).
//
// Fees are COMPUTED at resolve time — the trades row does not exist yet
// (RecordTrade writes it inside the settlement commit from these values,
// so the tape row IS the recon expected-fee record). Fees/Comm/Cal are
// nil-tolerant for test fixtures; production wires all three.
type PgxTradeResolver struct {
	Pool *pgxpool.Pool
	Fees *FeeService       // Task 3.3.4 trading fee (delivery-ccy per side)
	Comm *CommissionEngine // Task 3.3.13/17 raw commission + VIP rebate
	Cal  *HolidayCalendar  // PD value date (recon fee gate)
}

// NewPgxTradeResolver wraps a pool. feeSvc/commEng/cal may be nil in
// tests — nil fees resolve to zero; a nil calendar fails PD fills closed.
func NewPgxTradeResolver(pool *pgxpool.Pool, feeSvc *FeeService, commEng *CommissionEngine, cal *HolidayCalendar) *PgxTradeResolver {
	return &PgxTradeResolver{Pool: pool, Fees: feeSvc, Comm: commEng, Cal: cal}
}

// enrich computes the per-side trading fees, commission assessment and
// PD value date for a leg-resolved trade. Mutates rt in place.
func (r *PgxTradeResolver) enrich(ctx context.Context, rt *ResolvedTrade, l legFacts) error {
	rt.Symbol = l.symbol
	isPD := rt.BuyerIntent == IntentPhysicalDelivery
	tid := int64(rt.Fill.TradeID)

	// The wire TradeFill carries no aggressor flag: the resting order is
	// the maker — it reached the book first, i.e. the lower order_seq.
	buyerRole, sellerRole := RoleMaker, RoleTaker
	if l.boSeq > l.soSeq {
		buyerRole, sellerRole = RoleTaker, RoleMaker
	}

	if r.Fees != nil {
		bq, err := r.Fees.Quote(ctx, FeeParams{
			TradeID: tid, AccountID: rt.BuyerAccountID, Role: buyerRole,
			Quantity: rt.Fill.Qty, Price: rt.Fill.Price,
			FeeCurrency: rt.BaseCurrency, Notional: rt.Fill.Qty})
		if err != nil {
			return excerrors.Wrap(CodeTradeFillUnresolvable,
				fmt.Sprintf("buyer fee quote trade %d acct %d", tid, rt.BuyerAccountID), err)
		}
		sq, err := r.Fees.Quote(ctx, FeeParams{
			TradeID: tid, AccountID: rt.SellerAccountID, Role: sellerRole,
			Quantity: rt.Fill.Qty, Price: rt.Fill.Price,
			FeeCurrency: rt.QuoteCurrency})
		if err != nil {
			return excerrors.Wrap(CodeTradeFillUnresolvable,
				fmt.Sprintf("seller fee quote trade %d acct %d", tid, rt.SellerAccountID), err)
		}
		// A fee can never exceed the received amount it nets against.
		if bq.Amount.Abs().GreaterThan(rt.Fill.Qty) {
			return excerrors.New(CodeFeeExceedsProceeds, fmt.Sprintf(
				"trade %d: buyer fee %s exceeds base delivered %s", tid, bq.Amount, rt.Fill.Qty))
		}
		if sq.Amount.Abs().GreaterThan(quoteAmount(*rt)) {
			return excerrors.New(CodeFeeExceedsProceeds, fmt.Sprintf(
				"trade %d: seller fee %s exceeds quote proceeds %s", tid, sq.Amount, quoteAmount(*rt)))
		}
		rt.BuyerFee, rt.SellerFee = bq.Amount, sq.Amount
		if !isPD {
			// Receive-side netting posts at execution as FEE journals in
			// the fill tx; PD fee legs are rebuilt from trades.*_fee by
			// the settlement-confirm flow (§5.45.2 — fees travel with
			// the confirm journal).
			for _, q := range []FeeQuote{bq, sq} {
				if q.Amount.IsZero() {
					continue
				}
				j, err := r.Fees.Journal(q, tid)
				if err != nil {
					return excerrors.Wrap(CodeTradeFillUnresolvable,
						fmt.Sprintf("fee journal trade %d acct %d", tid, q.AccountID), err)
				}
				rt.FeeJournals = append(rt.FeeJournals, j)
			}
		}
	}

	if r.Comm != nil {
		for _, side := range []struct {
			acct int64
			role FillRole
			vip  int
			mk   decimal.Decimal
		}{
			{rt.BuyerAccountID, FillRole(buyerRole), l.buyerVip, l.buyerMakerBps},
			{rt.SellerAccountID, FillRole(sellerRole), l.sellerVip, l.sellerMakerBps},
		} {
			a, err := r.Comm.Quote(ctx, CommissionFill{
				AccountID: side.acct, TradeID: tid, InstrumentID: rt.InstrumentID,
				Symbol: l.symbol, Role: side.role, Quantity: rt.Fill.Qty,
				Price: rt.Fill.Price, LotSize: l.lotSize,
				QuoteCurrency: rt.QuoteCurrency, MakerBps: side.mk,
				VipTier: side.vip, PostedBy: "commission-engine"})
			if err != nil {
				return excerrors.Wrap(CodeTradeFillUnresolvable,
					fmt.Sprintf("commission assess trade %d acct %d", tid, side.acct), err)
			}
			if a.FillVolumeUSD.IsPositive() {
				rt.Volumes = append(rt.Volumes, CommissionVolumeAccrual{
					AccountID: side.acct, Month: a.Month, DeltaUSD: a.FillVolumeUSD})
			}
			if !isPD {
				rt.FeeJournals = append(rt.FeeJournals, a.Journals...)
			}
		}
	}

	if isPD {
		if r.Cal == nil {
			return excerrors.New(CodeTradeFillUnresolvable, fmt.Sprintf(
				"trade %d: physical delivery requires a holiday calendar for the value date", tid))
		}
		sd, err := r.Cal.SettlementDate(rt.BaseCurrency, rt.QuoteCurrency,
			time.Now().UTC(), l.settlementCycle)
		if err != nil {
			return excerrors.Wrap(CodeTradeFillUnresolvable,
				fmt.Sprintf("value date trade %d %s", tid, l.symbol), err)
		}
		rt.SettlementDate = &sd
	}
	return nil
}

// Resolve loads both order legs, the instrument, VIP maker rates and each
// side's effective settlement intent, then computes fees (see enrich).
func (r *PgxTradeResolver) Resolve(ctx context.Context, f EngineFill) (ResolvedTrade, error) {
	rt := ResolvedTrade{Fill: f}
	var l legFacts
	var lotSize, bmk, smk string
	err := r.Pool.QueryRow(ctx, `
		SELECT bo.account_id, so.account_id, bo.instrument_id,
		       i.base_currency, i.quote_currency,
		       COALESCE(bo.settlement_intent::text, ab.settlement_intent::text, 'ROLLING_MARGIN'),
		       COALESCE(so.settlement_intent::text, sa.settlement_intent::text, 'ROLLING_MARGIN'),
		       i.symbol, COALESCE(i.lot_size::text,'0'), COALESCE(i.settlement_cycle,1),
		       COALESCE(bo.order_seq,0), COALESCE(so.order_seq,0),
		       COALESCE(ab.vip_tier,0), COALESCE(sa.vip_tier,0),
		       COALESCE(vb.maker_bps::text,'0'), COALESCE(vs.maker_bps::text,'0')
		  FROM orders bo
		  JOIN orders so      ON so.id = $2
		  JOIN instruments i  ON i.id = bo.instrument_id
		  JOIN accounts ab    ON ab.id = bo.account_id
		  JOIN accounts sa    ON sa.id = so.account_id
		  LEFT JOIN vip_tier_schedule vb ON vb.vip_tier = ab.vip_tier
		  LEFT JOIN vip_tier_schedule vs ON vs.vip_tier = sa.vip_tier
		 WHERE bo.id = $1`,
		int64(f.BuyOrderID), int64(f.SellOrderID)).
		Scan(&rt.BuyerAccountID, &rt.SellerAccountID, &rt.InstrumentID,
			&rt.BaseCurrency, &rt.QuoteCurrency,
			(*string)(&rt.BuyerIntent), (*string)(&rt.SellerIntent),
			&l.symbol, &lotSize, &l.settlementCycle, &l.boSeq, &l.soSeq,
			&l.buyerVip, &l.sellerVip, &bmk, &smk)
	if err != nil {
		return ResolvedTrade{}, excerrors.Wrap(CodeTradeFillUnresolvable, fmt.Sprintf(
			"resolve fill trade %d (orders %d/%d)", f.TradeID, f.BuyOrderID, f.SellOrderID), err)
	}
	if l.lotSize, err = decimal.NewFromString(lotSize); err != nil {
		return ResolvedTrade{}, excerrors.Wrap(CodeTradeFillUnresolvable,
			fmt.Sprintf("lot size %q trade %d", lotSize, f.TradeID), err)
	}
	if l.buyerMakerBps, err = decimal.NewFromString(bmk); err != nil {
		return ResolvedTrade{}, excerrors.Wrap(CodeTradeFillUnresolvable,
			fmt.Sprintf("buyer maker bps %q trade %d", bmk, f.TradeID), err)
	}
	if l.sellerMakerBps, err = decimal.NewFromString(smk); err != nil {
		return ResolvedTrade{}, excerrors.Wrap(CodeTradeFillUnresolvable,
			fmt.Sprintf("seller maker bps %q trade %d", smk, f.TradeID), err)
	}
	if err := r.enrich(ctx, &rt, l); err != nil {
		return ResolvedTrade{}, err
	}
	return rt, nil
}

// ResolveBatch is the set-based Resolve: one order-pair join over unnest
// regardless of batch size, then per-fill fee computation (enrich). The
// FillConsumer prefers it — per-fill leg resolution is the throughput
// ceiling of the serial path. Order of fills is preserved in the output;
// any unresolvable leg fails the batch (fail-closed — a dropped fill is
// lost settlement).
func (r *PgxTradeResolver) ResolveBatch(ctx context.Context, fills []EngineFill) ([]ResolvedTrade, error) {
	out := make([]ResolvedTrade, len(fills))
	if len(fills) == 0 {
		return out, nil
	}
	boIDs := make([]int64, len(fills))
	soIDs := make([]int64, len(fills))
	for i := range fills {
		boIDs[i] = int64(fills[i].BuyOrderID)
		soIDs[i] = int64(fills[i].SellOrderID)
		out[i].Fill = fills[i]
	}

	type legKey struct{ bo, so int64 }
	type legRow struct {
		buyer, seller, instrument int64
		base, quote               string
		buyerIntent, sellerIntent string
		facts                     legFacts
	}
	legs := map[legKey]legRow{}
	rows, err := r.Pool.Query(ctx, `
		SELECT u.bo, u.so, bo.account_id, so.account_id, bo.instrument_id,
		       i.base_currency, i.quote_currency,
		       COALESCE(bo.settlement_intent::text, ab.settlement_intent::text, 'ROLLING_MARGIN'),
		       COALESCE(so.settlement_intent::text, sa.settlement_intent::text, 'ROLLING_MARGIN'),
		       i.symbol, COALESCE(i.lot_size::text,'0'), COALESCE(i.settlement_cycle,1),
		       COALESCE(bo.order_seq,0), COALESCE(so.order_seq,0),
		       COALESCE(ab.vip_tier,0), COALESCE(sa.vip_tier,0),
		       COALESCE(vb.maker_bps::text,'0'), COALESCE(vs.maker_bps::text,'0')
		  FROM unnest($1::bigint[], $2::bigint[]) AS u(bo, so)
		  JOIN orders bo      ON bo.id = u.bo
		  JOIN orders so      ON so.id = u.so
		  JOIN instruments i  ON i.id = bo.instrument_id
		  JOIN accounts ab    ON ab.id = bo.account_id
		  JOIN accounts sa    ON sa.id = so.account_id
		  LEFT JOIN vip_tier_schedule vb ON vb.vip_tier = ab.vip_tier
		  LEFT JOIN vip_tier_schedule vs ON vs.vip_tier = sa.vip_tier`,
		boIDs, soIDs)
	if err != nil {
		return nil, excerrors.Wrap(CodeTradeFillUnresolvable, "batch resolve legs", err)
	}
	for rows.Next() {
		var k legKey
		var l legRow
		var lotSize, bmk, smk string
		if err := rows.Scan(&k.bo, &k.so, &l.buyer, &l.seller, &l.instrument,
			&l.base, &l.quote, &l.buyerIntent, &l.sellerIntent,
			&l.facts.symbol, &lotSize, &l.facts.settlementCycle,
			&l.facts.boSeq, &l.facts.soSeq, &l.facts.buyerVip, &l.facts.sellerVip,
			&bmk, &smk); err != nil {
			rows.Close()
			return nil, excerrors.Wrap(CodeTradeFillUnresolvable, "batch scan legs", err)
		}
		if l.facts.lotSize, err = decimal.NewFromString(lotSize); err != nil {
			rows.Close()
			return nil, excerrors.Wrap(CodeTradeFillUnresolvable,
				fmt.Sprintf("lot size %q orders %d/%d", lotSize, k.bo, k.so), err)
		}
		if l.facts.buyerMakerBps, err = decimal.NewFromString(bmk); err != nil {
			rows.Close()
			return nil, excerrors.Wrap(CodeTradeFillUnresolvable,
				fmt.Sprintf("buyer maker bps %q orders %d/%d", bmk, k.bo, k.so), err)
		}
		if l.facts.sellerMakerBps, err = decimal.NewFromString(smk); err != nil {
			rows.Close()
			return nil, excerrors.Wrap(CodeTradeFillUnresolvable,
				fmt.Sprintf("seller maker bps %q orders %d/%d", smk, k.bo, k.so), err)
		}
		legs[k] = l
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, excerrors.Wrap(CodeTradeFillUnresolvable, "batch resolve legs", err)
	}

	for i := range fills {
		l, ok := legs[legKey{boIDs[i], soIDs[i]}]
		if !ok {
			return nil, excerrors.New(CodeTradeFillUnresolvable, fmt.Sprintf(
				"resolve fill trade %d (orders %d/%d)", fills[i].TradeID,
				fills[i].BuyOrderID, fills[i].SellOrderID))
		}
		out[i].BuyerAccountID = l.buyer
		out[i].SellerAccountID = l.seller
		out[i].InstrumentID = l.instrument
		out[i].BaseCurrency = l.base
		out[i].QuoteCurrency = l.quote
		out[i].BuyerIntent = SettlementIntent(l.buyerIntent)
		out[i].SellerIntent = SettlementIntent(l.sellerIntent)
		if err := r.enrich(ctx, &out[i], l.facts); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// RepublishBacklog implements BacklogSource for the boot-time republish
// repair: committed fills on this shard within lookback whose raw Event
// frames were persisted to processed_trades (migration 281) in the same
// transaction as the settlement commit. trade_seq is the engine
// sequence — the "s{shard}-{seq}" Nats-Msg-Id dedup key the bridge
// publishes under, so re-emission of already-published fills is a
// stream-level no-op.
func (r *PgxTradeResolver) RepublishBacklog(ctx context.Context, shardID int64,
	lookback time.Duration) ([]BacklogFill, error) {
	rows, err := r.Pool.Query(ctx, `
		SELECT pt.trade_id, i.symbol, t.trade_seq, pt.raw_frame
		  FROM processed_trades pt
		  JOIN trades t       ON t.id = pt.trade_id
		  JOIN instruments i  ON i.id = t.instrument_id
		 WHERE pt.shard_id = $1
		   AND pt.raw_frame IS NOT NULL
		   AND pt.processed_at > now() - $2::interval
		 ORDER BY pt.processed_at`, shardID, lookback.String())
	if err != nil {
		return nil, fmt.Errorf("republish backlog scan shard %d: %w", shardID, err)
	}
	defer rows.Close()
	var out []BacklogFill
	for rows.Next() {
		var f BacklogFill
		if err := rows.Scan(&f.TradeID, &f.Symbol, &f.EngineSeq, &f.Raw); err != nil {
			return nil, fmt.Errorf("republish backlog row: %w", err)
		}
		out = append(out, f)
	}
	return out, rows.Err()
}
