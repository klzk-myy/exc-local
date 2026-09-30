// position_transfer.go — Internal Position Transfers & Sub-Account
// Allocation Engine (Phase-19 Task 19.3.12; spec §13.9, §5.38,
// §24 #190, remediation #38).
//
// LOCATION DEVIATION (documented per AGENTS.md change protocol): the
// Phase-19 plan prescribes internal/risk/position_transfer.go; this file
// lives in internal/settlement instead — the position-move machinery it
// must reuse (PositionService.applyInTx, the NETTING/HEDGING fill paths
// of Task 19.3.15, pgxPositionTx) lives here, and risk already imports
// settlement, so a risk→settlement import for the shared machinery is
// impossible without an import cycle. No behaviour change: same spec
// §13.9 contract.
//
// An internal transfer moves an open position — or a slice of it — from a
// source account to a destination account of the same legal entity (same
// user, or same master-account hierarchy) at the official PriceOracle
// mark with zero spread and zero market impact. One SERIALIZABLE
// transaction performs, in order:
//
//  1. structural + entity validation (accounts locked FOR UPDATE; same
//     user_id or same account-hierarchy root required);
//  2. audit row INSERT (position_transfers, status PENDING — rolls back
//     with the tx on any later abort);
//  3. mark-price resolution via the TransferMarkSource seam — absent or
//     non-positive mark fails closed (PRICE_ORACLE_UNAVAILABLE);
//  4. source position close at mark via the shared position machinery
//     (a REDUCE_ONLY fill against the transfer's pseudo trade id —
//     crystallizes realized P&L, never opens/flips);
//  5. destination margin check + atomic collateral rebalance: the source
//     account's locked collateral is released pro-rata (positions
//     .margin_used when the engine recorded it, else qty·mark/leverage);
//     the destination's required initial-margin delta is computed
//     mode-aware — under NETTING it is the change in |net exposure|, so a
//     transfer into an opposing position reduces before it opens (and
//     frees locked margin when the delta is negative); under HEDGING it
//     is the full qty·mark/leverage of the new leg. Insufficient
//     destination available balance aborts the ENTIRE tx with
//     INSUFFICIENT_MARGIN (HTTP 409, spec §13.9 (c));
//  6. destination position open at entry = mark;
//  7. balanced double-entry GL journal via accounting.BuildTransferJournal
//     posted through DoubleEntryLedgerService.PostJournal inside the same
//     tx (§5.3 zero-GL-bypass; collateral rebalance + crystallized P&L +
//     optional admin fee are the wallet Effects);
//  8. position_transfers → COMPLETED with gl_journal_id.
//
// Idempotency: position_transfers.idempotency_key (UNIQUE) replays to the
// committed row without re-applying; inside the tx the two position fills
// ride the position_fills dedup under a pseudo trade id
// (transferID | 1<<62) that can never collide with engine trade ids.
// Redis account:lock:{id} mutexes cover BOTH accounts before the tx
// (§5.3 locking protocol); BalanceChanged events dispatch after commit;
// SQLSTATE 40001/40P01 retry at 5/15/45ms + jitter, max 3 attempts
// (§5.40). Fail-closed throughout — validation failures reject before any
// balance mutation, and every abort rolls back the whole transfer.
package settlement

import (
	"context"
	stderrors "errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"

	"exchange/internal/accounting"
	"exchange/internal/ledger"
	excerrors "exchange/pkg/errors"
)

// Error codes emitted by this file (spec §23 registry / §27.1 Internal
// Position Transfers matrix row — registered in errs/codes.go).
const (
	// CodePositionTransferFailed — HTTP 400, L2: structural validation or
	// same-entity/hierarchy check failed, or the audit row conflicts.
	CodePositionTransferFailed = "POSITION_TRANSFER_FAILED"
	// CodeTransferInsufficient — HTTP 400, L2: source has no matching open
	// position, or the open quantity is below the requested transfer qty.
	CodeTransferInsufficient = "TRANSFER_INSUFFICIENT"
	// CodeTransferMarginInsufficient — HTTP 409, L2: destination lacks the
	// free balance to carry the required initial margin (spec §13.9 (c)).
	CodeTransferMarginInsufficient = "INSUFFICIENT_MARGIN"
)

// transferTradeIDBit namespaces the position_fills dedup key for
// transfers: transferID | 1<<62 can never collide with an engine
// trades.id (IDENTITY sequence in the low range), so a transfer replay
// dedups and a real trade never looks like a transfer fill.
const transferTradeIDBit = uint64(1) << 62

// transferMaxHops bounds the parent_account_id walk when resolving the
// account-hierarchy root — deeper chains/cycles fail closed.
const transferMaxHops = 16

// Canonical reason_code values (position_transfers.reason_code,
// VARCHAR(32) — ops vocabulary, free-form beyond these).
const (
	TransferReasonAdminAdjust = "ADMIN_ADJUST" // administrative correction
	TransferReasonGiveUp      = "GIVE_UP"      // PB give-up (§13.9 allocations)
	TransferReasonAllocation  = "ALLOCATION"   // block-trade allocation
	TransferReasonSubAccount  = "SUB_ACCOUNT"  // sub-account reallocation
)

// RegIndicatorTransfer is the §13.9 step 3 regulatory trade indicator
// persisted on every row: MiFID II RTS 1/2 post-trade transparency
// exempt; reported in EMIR/CFTC lifecycle position-continuation reports.
const RegIndicatorTransfer = "POSITION_TRANSFER"

// TransferMarkSource is the price seam for the official mark at transfer
// time (Phase-19.5 PriceOracle binds here; internal/risk's
// MarkPriceProvider cannot be imported — it would cycle). An absent or
// non-positive mark fails the transfer closed.
type TransferMarkSource interface {
	Mark(ctx context.Context, instrumentID int64) (decimal.Decimal, error)
}

// markSourceFromMarkPrice adapts this package's MarkPriceProvider seam
// (position_service.go) to TransferMarkSource.
type markSourceFromMarkPrice struct{ p MarkPriceProvider }

func (a markSourceFromMarkPrice) Mark(ctx context.Context, instrumentID int64) (decimal.Decimal, error) {
	return a.p.MarkPrice(ctx, instrumentID)
}

// TransferMarksFrom binds an existing MarkPriceProvider (oracle or
// last-trade placeholder) as the transfer mark source.
func TransferMarksFrom(p MarkPriceProvider) TransferMarkSource {
	if p == nil {
		return nil
	}
	return markSourceFromMarkPrice{p}
}

// TransferAccount is the validation view of accounts for a transfer.
type TransferAccount struct {
	ID           int64
	UserID       int64
	ParentID     int64 // 0 = master account (no parent)
	Status       string
	PositionMode string // NETTING | HEDGING (migration 233)
}

// TransferInstrument is the margin-relevant view of instruments.
type TransferInstrument struct {
	ID            int64
	Symbol        string
	QuoteCurrency string // margin/P&L denomination (spec §13.1)
	MaxLeverage   int64  // instruments.max_leverage; <=0 → leverage 1
}

// TransferRequest is one Execute call.
type TransferRequest struct {
	FromAccountID  int64
	ToAccountID    int64
	InstrumentID   int64
	Side           string // LONG | SHORT — the position leg to move
	Quantity       decimal.Decimal
	ReasonCode     string // ≤32 chars; TransferReason* consts preferred
	AuthorizedBy   int64  // authorizing user id (position_transfers.authorized_by)
	IdempotencyKey string // optional ≤128 chars — replay dedup
	// AdminFee is an optional administrative transfer fee (≥0) charged to
	// the SOURCE account's available balance in the instrument's quote
	// currency and booked to 4010_TRADING_FEE_REVENUE (§13.9 step 4).
	AdminFee decimal.Decimal
}

// PositionTransfer mirrors a position_transfers row (migration 061).
type PositionTransfer struct {
	ID             int64
	FromAccountID  int64
	ToAccountID    int64
	InstrumentID   int64
	Side           string
	Quantity       decimal.Decimal
	TransferPrice  decimal.Decimal
	ReasonCode     string
	RegIndicator   string
	GLJournalID    int64
	Status         string // PENDING | COMPLETED | REJECTED | FAILED
	AuthorizedBy   int64
	FailureReason  string
	IdempotencyKey string
	CreatedAt      time.Time
	CompletedAt    *time.Time
}

// TransferResult reports the committed transfer.
type TransferResult struct {
	Transfer       PositionTransfer
	Mark           decimal.Decimal
	RealizedPnL    decimal.Decimal // crystallized on the source (quote ccy)
	SourceReleased decimal.Decimal // collateral released on source (quote ccy)
	DestLocked     decimal.Decimal // signed margin delta applied on dest
	JournalID      int64
	SourceUpdate   *PositionUpdate
	DestUpdate     *PositionUpdate
	Replayed       bool // idempotency-key replay — nothing re-applied
}

// ---------------------------------------------------------------------------
// Store seam
// ---------------------------------------------------------------------------

// TransferStore runs fn inside a SERIALIZABLE transaction, rolling back
// on error — the PositionStore/SettlementStore discipline.
type TransferStore interface {
	InTx(ctx context.Context, fn func(ctx context.Context, tx TransferTx) error) error
}

// TransferTx is the transactional view inside TransferStore.InTx. It
// embeds PositionTx so the shared position machinery (applyInTx: NETTING
// /HEDGING, reduce-only guards, dedup, ceilings) runs unchanged on the
// same tx.
type TransferTx interface {
	PositionTx

	// LockTransferAccounts SELECTs both account rows FOR UPDATE (sorted
	// id order) and returns them keyed to fromID/toID. A missing account
	// row is an error (fail-closed).
	LockTransferAccounts(ctx context.Context, fromID, toID int64) (from, to TransferAccount, err error)
	// AccountRootID resolves the top of the account's parent_account_id
	// hierarchy (the master account) — the account itself when it has no
	// parent. Cyclic/absent chains resolve the deepest reachable row and
	// rely on entity equality to fail closed.
	AccountRootID(ctx context.Context, accountID int64) (int64, error)
	// InstrumentSpec loads the margin-relevant instrument fields.
	InstrumentSpec(ctx context.Context, instrumentID int64) (TransferInstrument, error)
	// PositionMarginUsed reads positions.margin_used — the engine-written
	// authoritative IM for the row (0 = derive notional/leverage).
	PositionMarginUsed(ctx context.Context, positionID int64) (decimal.Decimal, error)
	// EnsureBalanceForUpdate upserts-then-locks the balances row and
	// returns (available, locked).
	EnsureBalanceForUpdate(ctx context.Context, accountID int64, currency string) (avail, locked decimal.Decimal, err error)
	// InsertTransfer writes the PENDING audit row; returns 0 when
	// IdempotencyKey collides with an existing row (replay — caller
	// resolves via FindTransferByKey and does not re-execute).
	InsertTransfer(ctx context.Context, rec PositionTransfer) (int64, error)
	// FindTransferByKey loads a row by idempotency_key (nil when absent).
	FindTransferByKey(ctx context.Context, key string) (*PositionTransfer, error)
	// CompleteTransfer flips PENDING→COMPLETED with the GL journal link.
	CompleteTransfer(ctx context.Context, id, journalID int64, at time.Time) error
	// PostJournal posts a validated journal inside this tx — the
	// DoubleEntryLedgerService.PostJournal contract.
	PostJournal(ctx context.Context, j ledger.Journal) (ledger.PostResult, error)
}

// ---------------------------------------------------------------------------
// Service
// ---------------------------------------------------------------------------

// TransferService executes internal position transfers.
type TransferService struct {
	store    TransferStore
	pos      *PositionService // shared fill machinery (applyInTx)
	marks    TransferMarkSource
	locks    accountLocker
	dispatch eventDispatcher
	alerter  OpsAlerter
	postedBy string
	maxTries int
	backoff  []time.Duration
}

// NewTransferService wires the production service. pool and ls are
// required (fail-closed) — the journal/balance mutation rides the
// DoubleEntryLedgerService contract. marks may be nil: transfers then
// fail closed with PRICE_ORACLE_UNAVAILABLE at request time.
func NewTransferService(pool *pgxpool.Pool, ls *LedgerService, marks TransferMarkSource, alerter OpsAlerter) (*TransferService, error) {
	if pool == nil {
		return nil, fmt.Errorf("transfer service: nil pgx pool")
	}
	if ls == nil {
		return nil, fmt.Errorf("transfer service: nil ledger service — posting contract is mandatory")
	}
	return newTransferServiceForTest(
		pgxTransferStore{pool: pool, poster: ls},
		ledgerLockAdapter{ls}, ledgerDispatchAdapter{ls},
		marks, alerter, "position-transfer"), nil
}

func newTransferServiceForTest(store TransferStore, locks accountLocker,
	disp eventDispatcher, marks TransferMarkSource, alerter OpsAlerter, postedBy string) *TransferService {
	return &TransferService{
		store:    store,
		pos:      NewPositionService(nil, nil, 0),
		marks:    marks,
		locks:    locks,
		dispatch: disp,
		alerter:  alerter,
		postedBy: postedBy,
		maxTries: 3,
		backoff:  []time.Duration{5 * time.Millisecond, 15 * time.Millisecond, 45 * time.Millisecond},
	}
}

// Execute performs one internal position transfer atomically.
func (s *TransferService) Execute(ctx context.Context, req TransferRequest) (*TransferResult, error) {
	if err := validateTransferRequest(req); err != nil {
		return nil, err
	}

	token, err := lockToken()
	if err != nil {
		return nil, fmt.Errorf("transfer: lock token: %w", err)
	}
	ids := []int64{req.FromAccountID, req.ToAccountID}
	if ids[1] < ids[0] {
		ids[0], ids[1] = ids[1], ids[0]
	}
	locked, err := s.lockAccounts(ctx, ids, token)
	if err != nil {
		return nil, err
	}
	defer s.unlockAccounts(locked, token)

	var res *TransferResult
	var events []ledger.BalanceEvent
	var lastErr error
	for attempt := 0; attempt < s.maxTries; attempt++ {
		res, events, err = s.executeOnce(ctx, req)
		if err == nil {
			break
		}
		lastErr = err
		if !isRetryableConflict(err) {
			s.maybeAlertImbalance(ctx, err)
			return nil, err
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
			fmt.Sprintf("transfer: aborted after %d attempts", s.maxTries), lastErr)
	}

	if !res.Replayed && s.dispatch != nil && len(events) > 0 {
		if err := s.dispatch.Dispatch(ctx, events); err != nil {
			return res, err // committed — funds final; resync downstream
		}
	}
	return res, nil
}

// executeOnce is one full attempt: a single SERIALIZABLE transaction.
func (s *TransferService) executeOnce(ctx context.Context, req TransferRequest) (*TransferResult, []ledger.BalanceEvent, error) {
	var res *TransferResult
	var events []ledger.BalanceEvent
	err := s.store.InTx(ctx, func(ctx context.Context, tx TransferTx) error {
		var err error
		res, events, err = s.transferInTx(ctx, tx, req)
		return err
	})
	return res, events, err
}

func (s *TransferService) transferInTx(ctx context.Context, tx TransferTx,
	req TransferRequest) (*TransferResult, []ledger.BalanceEvent, error) {

	// 1. Account lock + entity validation.
	from, to, err := tx.LockTransferAccounts(ctx, req.FromAccountID, req.ToAccountID)
	if err != nil {
		return nil, nil, err
	}
	if from.Status != "ACTIVE" || to.Status != "ACTIVE" {
		return nil, nil, excerrors.New(CodePositionTransferFailed, fmt.Sprintf(
			"transfer requires ACTIVE accounts (from=%s to=%s)", from.Status, to.Status))
	}
	if err := s.assertSameEntity(ctx, tx, from, to); err != nil {
		return nil, nil, err
	}

	// 2. Mark price — absent fails closed.
	mark, err := s.transferMark(ctx, req.InstrumentID)
	if err != nil {
		return nil, nil, err
	}

	// 3. Instrument margin spec.
	inst, err := tx.InstrumentSpec(ctx, req.InstrumentID)
	if err != nil {
		return nil, nil, fmt.Errorf("transfer: instrument %d: %w", req.InstrumentID, err)
	}
	lev := decimal.NewFromInt(inst.MaxLeverage)
	if inst.MaxLeverage <= 0 {
		lev = decimal.NewFromInt(1)
	}

	// 4. Audit row (idempotent on the client key).
	rec := PositionTransfer{
		FromAccountID:  req.FromAccountID,
		ToAccountID:    req.ToAccountID,
		InstrumentID:   req.InstrumentID,
		Side:           req.Side,
		Quantity:       req.Quantity,
		TransferPrice:  mark,
		ReasonCode:     req.ReasonCode,
		RegIndicator:   RegIndicatorTransfer,
		Status:         "PENDING",
		AuthorizedBy:   req.AuthorizedBy,
		IdempotencyKey: req.IdempotencyKey,
	}
	transferID, err := tx.InsertTransfer(ctx, rec)
	if err != nil {
		return nil, nil, fmt.Errorf("transfer: insert audit row: %w", err)
	}
	if transferID == 0 {
		existing, err := tx.FindTransferByKey(ctx, req.IdempotencyKey)
		if err != nil || existing == nil {
			return nil, nil, excerrors.Wrap(CodePositionTransferFailed,
				"transfer: idempotency-key replay could not resolve original row", err)
		}
		return &TransferResult{Transfer: *existing, Replayed: true}, nil, nil
	}
	rec.ID = transferID
	pseudoTradeID := uint64(transferID) | transferTradeIDBit

	// 5. Source position — the leg must exist and cover the request.
	src, err := tx.GetSidePositionForUpdate(ctx, req.FromAccountID, req.InstrumentID, req.Side)
	if err != nil {
		return nil, nil, fmt.Errorf("transfer: source position load: %w", err)
	}
	if src == nil || src.Quantity.IsZero() {
		return nil, nil, excerrors.New(CodeTransferInsufficient, fmt.Sprintf(
			"account %d has no open %s position on instrument %d",
			req.FromAccountID, req.Side, req.InstrumentID))
	}
	if req.Quantity.GreaterThan(src.Quantity) {
		return nil, nil, excerrors.New(CodeTransferInsufficient, fmt.Sprintf(
			"transfer qty %s exceeds open %s qty %s acct=%d instr=%d",
			req.Quantity, req.Side, src.Quantity, req.FromAccountID, req.InstrumentID))
	}

	// Pro-rata collateral release: positions.margin_used is authoritative
	// when the engine wrote it; otherwise derive qty·mark/leverage —
	// exactly the risk-margin fallback convention (risk/margin.go).
	srcMargin, err := tx.PositionMarginUsed(ctx, src.ID)
	if err != nil {
		return nil, nil, fmt.Errorf("transfer: source margin_used: %w", err)
	}
	var srcRelease decimal.Decimal
	if srcMargin.IsPositive() {
		srcRelease = srcMargin.Mul(req.Quantity).Div(src.Quantity).Round(8)
	} else {
		srcRelease = req.Quantity.Mul(mark).Div(lev).Round(8)
	}

	// 6. Destination margin requirement — mode-aware net-exposure delta.
	var preNet decimal.Decimal
	toMode := to.PositionMode
	if toMode == posModeNetting || toMode == "" {
		pre, err := tx.GetPositionForUpdate(ctx, req.ToAccountID, req.InstrumentID)
		if err != nil {
			return nil, nil, fmt.Errorf("transfer: dest position load: %w", err)
		}
		if pre != nil {
			preNet = pre.signedQty()
		}
	}
	postNet := preNet.Add(signedQtyFor(req.Side, req.Quantity))
	var destDelta decimal.Decimal
	if toMode == posModeHedging {
		destDelta = req.Quantity.Mul(mark).Div(lev).Round(8)
	} else {
		destDelta = postNet.Abs().Sub(preNet.Abs()).Mul(mark).Div(lev).Round(8)
	}
	if destDelta.IsPositive() {
		avail, _, err := tx.EnsureBalanceForUpdate(ctx, req.ToAccountID, inst.QuoteCurrency)
		if err != nil {
			return nil, nil, fmt.Errorf("transfer: dest balance lock: %w", err)
		}
		if avail.LessThan(destDelta) {
			return nil, nil, excerrors.New(CodeTransferMarginInsufficient, fmt.Sprintf(
				"destination acct %d available %s %s < required margin %s",
				req.ToAccountID, avail, inst.QuoteCurrency, destDelta))
		}
	} else if destDelta.IsNegative() {
		// Netting absorbed the transfer — free locked margin, bounded by
		// what is actually locked (locked can never go negative).
		_, locked, err := tx.EnsureBalanceForUpdate(ctx, req.ToAccountID, inst.QuoteCurrency)
		if err != nil {
			return nil, nil, fmt.Errorf("transfer: dest balance lock: %w", err)
		}
		freed := destDelta.Neg()
		if freed.GreaterThan(locked) {
			freed = locked
		}
		destDelta = freed.Neg()
	}

	// 7. Position moves — the shared machinery (reduce-only close on the
	//    source, plain open on the destination; NETTING destinations
	//    consume an opposing position first, HEDGING opens the leg).
	srcUpd, err := s.pos.applyInTx(ctx, tx, PositionFill{
		TradeID: pseudoTradeID, AccountID: req.FromAccountID,
		InstrumentID: req.InstrumentID, Side: closeSideOf(req.Side),
		Price: mark, Quantity: req.Quantity, ReduceOnly: true,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("transfer: source close: %w", err)
	}
	dstUpd, err := s.pos.applyInTx(ctx, tx, PositionFill{
		TradeID: pseudoTradeID, AccountID: req.ToAccountID,
		InstrumentID: req.InstrumentID, Side: openSideOf(req.Side),
		Price: mark, Quantity: req.Quantity,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("transfer: destination open: %w", err)
	}

	// 8. Balanced GL journal + wallet collateral rebalance — atomic.
	j, err := accounting.BuildTransferJournal(accounting.TransferPosting{
		TransferID:     transferID,
		FromAccountID:  req.FromAccountID,
		ToAccountID:    req.ToAccountID,
		InstrumentID:   req.InstrumentID,
		Symbol:         inst.Symbol,
		Side:           req.Side,
		Currency:       inst.QuoteCurrency,
		Quantity:       req.Quantity,
		TransferPrice:  mark,
		SourceRelease:  srcRelease,
		DestLock:       destDelta,
		RealizedPnL:    srcUpd.RealizedPnLDelta,
		AdminFee:       req.AdminFee,
		PostedBy:       s.postedBy,
		IdempotencyKey: fmt.Sprintf("position-transfer:%d", transferID),
	})
	if err != nil {
		return nil, nil, err
	}
	postRes, err := tx.PostJournal(ctx, j)
	if err != nil {
		return nil, nil, err
	}

	// 9. Audit row → COMPLETED with the journal link.
	now := time.Now().UTC()
	if err := tx.CompleteTransfer(ctx, transferID, postRes.JournalID, now); err != nil {
		return nil, nil, fmt.Errorf("transfer: complete audit row %d: %w", transferID, err)
	}
	rec.Status = "COMPLETED"
	rec.GLJournalID = postRes.JournalID
	rec.CreatedAt = now
	rec.CompletedAt = &now

	return &TransferResult{
		Transfer:       rec,
		Mark:           mark,
		RealizedPnL:    srcUpd.RealizedPnLDelta,
		SourceReleased: srcRelease,
		DestLocked:     destDelta,
		JournalID:      postRes.JournalID,
		SourceUpdate:   srcUpd,
		DestUpdate:     dstUpd,
	}, postRes.Events, nil
}

// assertSameEntity enforces §13.9: identical legal entity (same owning
// user) or the same master-account hierarchy root.
func (s *TransferService) assertSameEntity(ctx context.Context, tx TransferTx,
	from, to TransferAccount) error {
	if from.UserID == to.UserID {
		return nil
	}
	rootFrom, err := tx.AccountRootID(ctx, from.ID)
	if err != nil {
		return fmt.Errorf("transfer: source hierarchy root: %w", err)
	}
	rootTo, err := tx.AccountRootID(ctx, to.ID)
	if err != nil {
		return fmt.Errorf("transfer: destination hierarchy root: %w", err)
	}
	if rootFrom != rootTo {
		return excerrors.New(CodePositionTransferFailed, fmt.Sprintf(
			"accounts %d (root %d, user %d) and %d (root %d, user %d) share no legal entity or master hierarchy",
			from.ID, rootFrom, from.UserID, to.ID, rootTo, to.UserID))
	}
	return nil
}

// transferMark resolves the official mark; the seam's failure or a
// non-positive value fails closed (spec §2.7 — a transfer can never be
// priced off a placeholder).
func (s *TransferService) transferMark(ctx context.Context, instrumentID int64) (decimal.Decimal, error) {
	if s.marks == nil {
		return decimal.Zero, excerrors.New(CodeMarkPriceUnavailable,
			"transfer: no mark-price source configured")
	}
	m, err := s.marks.Mark(ctx, instrumentID)
	if err != nil {
		return decimal.Zero, excerrors.Wrap(CodeMarkPriceUnavailable,
			fmt.Sprintf("transfer: mark for instrument %d", instrumentID), err)
	}
	if !m.IsPositive() {
		return decimal.Zero, excerrors.New(CodeMarkPriceUnavailable,
			fmt.Sprintf("transfer: non-positive mark %s for instrument %d", m, instrumentID))
	}
	return m, nil
}

func (s *TransferService) lockAccounts(ctx context.Context, ids []int64, token string) ([]int64, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	if s.locks == nil {
		return nil, excerrors.New(ledger.CodeLedgerLockUnavailable,
			"transfer requires the Redis account mutex backend (§5.3) — locker is nil")
	}
	return s.locks.LockAccounts(ctx, ids, token)
}

func (s *TransferService) unlockAccounts(ids []int64, token string) {
	if s.locks != nil {
		s.locks.UnlockAccounts(ids, token)
	}
}

// maybeAlertImbalance pages P0 on a zero-sum breach — the transfer must
// never commit an unbalanced journal (spec §5.40, L0 class).
func (s *TransferService) maybeAlertImbalance(ctx context.Context, err error) {
	var e *excerrors.Error
	if !stderrors.As(err, &e) || e.Code != ledger.CodeLedgerImbalanceAbort {
		return
	}
	if s.alerter != nil {
		_ = s.alerter.Raise(ctx, OpsAlert{
			Severity: SeverityP0, Code: ledger.CodeLedgerImbalanceAbort,
			Summary: "position transfer ledger zero-sum invariant violated — aborted",
			Err:     err.Error(),
		})
	}
}

func validateTransferRequest(req TransferRequest) error {
	if req.FromAccountID <= 0 || req.ToAccountID <= 0 || req.InstrumentID <= 0 {
		return excerrors.New(CodePositionTransferFailed,
			"transfer missing from/to account or instrument id")
	}
	if req.FromAccountID == req.ToAccountID {
		return excerrors.New(CodePositionTransferFailed,
			"transfer source and destination account are identical")
	}
	if req.Side != PositionLong && req.Side != PositionShort {
		return excerrors.New(CodePositionTransferFailed,
			fmt.Sprintf("transfer side %q must be LONG or SHORT", req.Side))
	}
	if !req.Quantity.IsPositive() {
		return excerrors.New(CodePositionTransferFailed,
			fmt.Sprintf("transfer quantity %s must be > 0", req.Quantity))
	}
	if !req.Quantity.Round(8).Equal(req.Quantity) {
		return excerrors.New(CodePositionTransferFailed,
			fmt.Sprintf("transfer quantity %s exceeds DECIMAL(28,8) quantum", req.Quantity))
	}
	if req.AuthorizedBy <= 0 {
		return excerrors.New(CodePositionTransferFailed,
			"transfer requires an authorizing user id")
	}
	if len(req.ReasonCode) == 0 || len(req.ReasonCode) > 32 {
		return excerrors.New(CodePositionTransferFailed,
			"transfer reason_code required, ≤32 chars")
	}
	if len(req.IdempotencyKey) > 128 {
		return excerrors.New(CodePositionTransferFailed,
			"transfer idempotency_key exceeds 128 chars")
	}
	if req.AdminFee.IsNegative() {
		return excerrors.New(CodePositionTransferFailed,
			fmt.Sprintf("transfer admin fee %s must be >= 0", req.AdminFee))
	}
	return nil
}

// signedQtyFor maps a side+quantity onto the signed net-position axis
// (LONG positive, SHORT negative) used for netting-mode margin deltas.
func signedQtyFor(side string, qty decimal.Decimal) decimal.Decimal {
	if side == PositionShort {
		return qty.Neg()
	}
	return qty
}

// closeSideOf returns the fill direction that closes a position of the
// given side (LONG→SELL, SHORT→BUY).
func closeSideOf(side string) FillSide {
	if side == PositionShort {
		return FillBuy
	}
	return FillSell
}

// openSideOf returns the fill direction that opens a position of the
// given side (LONG→BUY, SHORT→SELL).
func openSideOf(side string) FillSide {
	if side == PositionShort {
		return FillSell
	}
	return FillBuy
}

// ---------------------------------------------------------------------------
// pgxTransferStore — production TransferStore over pgxpool.
// ---------------------------------------------------------------------------

type pgxTransferStore struct {
	pool   *pgxpool.Pool
	poster journalPoster // DoubleEntryLedgerService.PostJournal
}

// InTx runs fn inside a SERIALIZABLE transaction, rolling back on error.
func (s pgxTransferStore) InTx(ctx context.Context, fn func(ctx context.Context, tx TransferTx) error) error {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return fmt.Errorf("transfer tx begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := fn(ctx, pgxTransferTx{pgxPositionTx{tx: tx}, tx, s.poster}); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("transfer tx commit: %w", err)
	}
	return nil
}

// pgxTransferTx satisfies TransferTx: the position mutations delegate to
// pgxPositionTx on the same tx; PostJournal delegates to the ledger
// service's tx-scoped poster.
type pgxTransferTx struct {
	pgxPositionTx
	tx     pgx.Tx
	poster journalPoster
}

func (t pgxTransferTx) LockTransferAccounts(ctx context.Context, fromID, toID int64) (TransferAccount, TransferAccount, error) {
	rows, err := t.tx.Query(ctx, `
		SELECT id, user_id, COALESCE(parent_account_id, 0),
		       status::text, position_mode::text
		  FROM accounts WHERE id = ANY($1) ORDER BY id FOR UPDATE`,
		[]int64{fromID, toID})
	if err != nil {
		return TransferAccount{}, TransferAccount{}, err
	}
	defer rows.Close()
	var from, to TransferAccount
	var seenFrom, seenTo bool
	for rows.Next() {
		var a TransferAccount
		if err := rows.Scan(&a.ID, &a.UserID, &a.ParentID, &a.Status, &a.PositionMode); err != nil {
			return TransferAccount{}, TransferAccount{}, err
		}
		switch a.ID {
		case fromID:
			from, seenFrom = a, true
		case toID:
			to, seenTo = a, true
		}
	}
	if err := rows.Err(); err != nil {
		return TransferAccount{}, TransferAccount{}, err
	}
	if !seenFrom || !seenTo {
		return TransferAccount{}, TransferAccount{}, excerrors.New(CodePositionTransferFailed,
			fmt.Sprintf("transfer account row missing (from=%d present=%t, to=%d present=%t)",
				fromID, seenFrom, toID, seenTo))
	}
	return from, to, nil
}

func (t pgxTransferTx) AccountRootID(ctx context.Context, accountID int64) (int64, error) {
	var root int64
	err := t.tx.QueryRow(ctx, `
		WITH RECURSIVE anc AS (
		    SELECT id, parent_account_id, 1 AS depth
		      FROM accounts WHERE id = $1
		    UNION ALL
		    SELECT a.id, a.parent_account_id, anc.depth + 1
		      FROM accounts a JOIN anc ON anc.parent_account_id = a.id
		     WHERE anc.depth < $2
		)
		SELECT COALESCE(
		    (SELECT id FROM anc WHERE parent_account_id IS NULL LIMIT 1),
		    (SELECT id FROM anc ORDER BY depth DESC LIMIT 1),
		    $1)`, accountID, transferMaxHops).Scan(&root)
	return root, err
}

func (t pgxTransferTx) InstrumentSpec(ctx context.Context, instrumentID int64) (TransferInstrument, error) {
	var inst TransferInstrument
	err := t.tx.QueryRow(ctx, `
		SELECT id, symbol, quote_currency, max_leverage
		  FROM instruments WHERE id = $1`, instrumentID).
		Scan(&inst.ID, &inst.Symbol, &inst.QuoteCurrency, &inst.MaxLeverage)
	if stderrors.Is(err, pgx.ErrNoRows) {
		return TransferInstrument{}, excerrors.New(CodePositionTransferFailed,
			fmt.Sprintf("instrument %d not found", instrumentID))
	}
	return inst, err
}

func (t pgxTransferTx) PositionMarginUsed(ctx context.Context, positionID int64) (decimal.Decimal, error) {
	var m string
	err := t.tx.QueryRow(ctx,
		`SELECT margin_used::text FROM positions WHERE id = $1`, positionID).Scan(&m)
	if err != nil {
		return decimal.Zero, err
	}
	return decimal.NewFromString(m)
}

func (t pgxTransferTx) EnsureBalanceForUpdate(ctx context.Context, accountID int64, currency string) (decimal.Decimal, decimal.Decimal, error) {
	if _, err := t.tx.Exec(ctx, `
		INSERT INTO balances (account_id, currency, available, locked)
		VALUES ($1,$2,0,0) ON CONFLICT (account_id, currency) DO NOTHING`,
		accountID, currency); err != nil {
		return decimal.Zero, decimal.Zero, err
	}
	var avail, locked string
	if err := t.tx.QueryRow(ctx, `
		SELECT available::text, locked::text FROM balances
		WHERE account_id = $1 AND currency = $2 FOR UPDATE`,
		accountID, currency).Scan(&avail, &locked); err != nil {
		return decimal.Zero, decimal.Zero, err
	}
	a, err := decimal.NewFromString(avail)
	if err != nil {
		return decimal.Zero, decimal.Zero, err
	}
	l, err := decimal.NewFromString(locked)
	if err != nil {
		return decimal.Zero, decimal.Zero, err
	}
	return a, l, nil
}

func (t pgxTransferTx) InsertTransfer(ctx context.Context, rec PositionTransfer) (int64, error) {
	var id int64
	err := t.tx.QueryRow(ctx, `
		INSERT INTO position_transfers
		    (from_account_id, to_account_id, instrument_id, side, quantity,
		     transfer_price, reason_code, reg_indicator, transfer_status,
		     authorized_by, idempotency_key)
		VALUES ($1,$2,$3,$4,$5::numeric,$6::numeric,$7,$8,'PENDING',$9,NULLIF($10,''))
		ON CONFLICT (idempotency_key) WHERE idempotency_key IS NOT NULL DO NOTHING
		RETURNING id`,
		rec.FromAccountID, rec.ToAccountID, rec.InstrumentID, rec.Side,
		rec.Quantity.String(), rec.TransferPrice.String(), rec.ReasonCode,
		rec.RegIndicator, rec.AuthorizedBy, rec.IdempotencyKey).Scan(&id)
	if stderrors.Is(err, pgx.ErrNoRows) {
		return 0, nil // idempotency-key replay
	}
	return id, err
}

func (t pgxTransferTx) FindTransferByKey(ctx context.Context, key string) (*PositionTransfer, error) {
	var r PositionTransfer
	var qty, px string
	var journalID *int64
	var idem, fail *string
	err := t.tx.QueryRow(ctx, `
		SELECT id, from_account_id, to_account_id, instrument_id, side::text,
		       quantity::text, transfer_price::text, reason_code, reg_indicator,
		       gl_journal_id, transfer_status::text, authorized_by,
		       failure_reason, idempotency_key, created_at, completed_at
		  FROM position_transfers WHERE idempotency_key = $1`, key).
		Scan(&r.ID, &r.FromAccountID, &r.ToAccountID, &r.InstrumentID, &r.Side,
			&qty, &px, &r.ReasonCode, &r.RegIndicator, &journalID,
			&r.Status, &r.AuthorizedBy, &fail, &idem, &r.CreatedAt, &r.CompletedAt)
	if stderrors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var err2 error
	if r.Quantity, err2 = decimal.NewFromString(qty); err2 != nil {
		return nil, fmt.Errorf("transfer %d quantity %q: %w", r.ID, qty, err2)
	}
	if r.TransferPrice, err2 = decimal.NewFromString(px); err2 != nil {
		return nil, fmt.Errorf("transfer %d transfer_price %q: %w", r.ID, px, err2)
	}
	if journalID != nil {
		r.GLJournalID = *journalID
	}
	if fail != nil {
		r.FailureReason = *fail
	}
	if idem != nil {
		r.IdempotencyKey = *idem
	}
	return &r, nil
}

func (t pgxTransferTx) CompleteTransfer(ctx context.Context, id, journalID int64, at time.Time) error {
	tag, err := t.tx.Exec(ctx, `
		UPDATE position_transfers
		   SET transfer_status = 'COMPLETED', gl_journal_id = NULLIF($2, 0), completed_at = $3
		 WHERE id = $1 AND transfer_status = 'PENDING'`, id, journalID, at)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return excerrors.New(CodePositionTransferFailed,
			fmt.Sprintf("transfer %d lost PENDING status mid-commit", id))
	}
	return nil
}

func (t pgxTransferTx) PostJournal(ctx context.Context, j ledger.Journal) (ledger.PostResult, error) {
	if t.poster == nil {
		return ledger.PostResult{}, excerrors.New(ledger.CodeLedgerInvalidJournal,
			"transfer: no journal poster configured (DoubleEntryLedgerService is mandatory)")
	}
	return t.poster.PostJournal(ctx, t.tx, j)
}
