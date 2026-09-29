// Deposit lifecycle — Phase-11 Task 11.3.3 (spec §5.6, §8.8).
//
// Flow:
//  1. Detection — a bank-statement poll or partner webhook calls
//     IngestDetected (POST /api/v1/admin/funding/deposits); a client
//     deposit intent (POST /api/v1/deposits) may pre-declare the wire.
//     Every detection persists a DEPOSIT funding row at PENDING,
//     records its ingestion source as the first independent
//     confirmation, and locks the funds (+locked, Nostro →
//     CustomerLiability — received but not spendable).
//  2. Dual-source verification — a SECOND confirmation from a distinct
//     source (POST /api/v1/admin/funding/deposits/{id}/confirm)
//     completes the task's two-independent-confirmations requirement.
//  3. Anti-fraud tiers (canonical — same thresholds as withdrawals):
//     < $10K     → AUTO: credited on confirm, no further checks.
//     $10K–$50K  → STANDARD: velocity + sanctions screen +
//     source-of-funds consistency before credit.
//     > $50K     → PENDING_REVIEW, 4-hour ops SLA, ops alert.
//     A missing sanctions screener or unpriceable amount fails closed
//     into PENDING_REVIEW.
//  4. Credit — the journal flips locked → available (the liability was
//     booked at detection); status → COMPLETED.
//  5. Admin review — POST /api/v1/admin/funding/deposits/{id}/review
//     (Finance Ops, four-eyes) credits APPROVE / reverses REJECT.
//
// Account-scoped idempotency (spec §8.8): (account_id, idempotency_key)
// on funding_transactions dedups intents and ingest notifications —
// same payload replays the stored acknowledgment, a payload change on
// the same key answers IDEMPOTENCY_KEY_MISMATCH. deposit_confirmations
// carries the same contract per (deposit, source).
package funding

import (
	"context"
	stderrors "errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"exchange/internal/ledger"
	"exchange/pkg/decimal"
)

// DepositConfirmationSources — the independent-source domain for the
// dual-source verification requirement (unbounded values are rejected;
// two confirmations sharing a source never satisfy dual-source).
var depositSourceDomain = map[string]bool{
	"STATEMENT": true, // bank statement poll (MT940/camt.053 line)
	"WEBHOOK":   true, // banking partner webhook notification
	"CAMT054":   true, // bank credit notification message
	"MT103":     true, // SWIFT customer credit transfer
	"MANUAL":    true, // ops-recorded independent confirmation
}

// DepositVelocityWindow is the trailing window for the STANDARD-tier
// velocity check.
const DepositVelocityWindow = 24 * time.Hour

// DepositVelocityMaxUSD — aggregate last-24h deposit volume above the
// canonical review threshold escalates a STANDARD-tier deposit to
// PENDING_REVIEW (velocity check).
var depositVelocityMaxUSD = decimal.NewFromInt(50_000)

// DepositVelocityMaxCount is the trailing-window count limit —
// more than this many deposits inside DepositVelocityWindow is itself
// a velocity flag.
const DepositVelocityMaxCount = 3

// SanctionsScreener is the Phase-21 sanctions seam (production binding
// lands with the compliance screening service). A nil screener fails
// the STANDARD tier closed to PENDING_REVIEW — "standard checks" cannot
// silently pass without their sanctions leg.
type SanctionsScreener interface {
	ScreenDeposit(ctx context.Context, accountID int64,
		senderName, senderAccount string) (hit bool, err error)
}

// DepositStore is the persistence seam (PgStore satisfies it).
type DepositStore interface {
	BeginTx(ctx context.Context) (pgx.Tx, error)
	InsertDepositPending(ctx context.Context, tx pgx.Tx, d DepositRow) (*DepositRow, error)
	DepositByIdemKey(ctx context.Context, accountID int64, key string) (*FundingTxRow, error)
	DepositByReference(ctx context.Context, accountID int64, reference string) (*FundingTxRow, error)
	DepositByBankRef(ctx context.Context, accountID int64, reference string) (*FundingTxRow, error)
	StampDepositBankRef(ctx context.Context, tx pgx.Tx, id int64,
		bankRef, reviewTier string) error
	FundingTxForUpdate(ctx context.Context, tx pgx.Tx, id int64) (*FundingTxRow, error)
	FundingTxPayloadSHA(ctx context.Context, tx pgx.Tx, id int64) (*string, error)
	InsertDepositConfirmation(ctx context.Context, tx pgx.Tx, c DepositConfirmationRow) (*DepositConfirmationRow, error)
	DepositConfirmations(ctx context.Context, tx pgx.Tx, fundingTxID int64) ([]DepositConfirmationRow, error)
	DepositVelocity(ctx context.Context, accountID int64, since time.Time) (int64, decimal.Decimal, error)
	SetDepositReview(ctx context.Context, tx pgx.Tx, id int64, status string,
		deadline *time.Time, reviewedBy *int64, at time.Time) error
	SetDepositCompleted(ctx context.Context, tx pgx.Tx, id int64, at time.Time) error
	SetFundingTxStatus(ctx context.Context, tx pgx.Tx, id int64, status string, completedAt *time.Time) error
	InsertFundingOpsAlert(ctx context.Context, tx pgx.Tx, a FundingOpsAlertRow) (int64, error)
	ExpiredReviewDeadlines(ctx context.Context, limit int) ([]int64, error)
	BumpReviewDeadline(ctx context.Context, tx pgx.Tx, id int64, deadline time.Time) error
}

// DepositService drives the detect → dual-source confirm → tier →
// credit lifecycle.
type DepositService struct {
	store     DepositStore
	poster    JournalPoster // nil → holds/credits fail closed
	checker   MutableChecker
	usd       UsdConverter      // nil → every deposit tiers PENDING_REVIEW
	sanctions SanctionsScreener // nil → STANDARD tier escalates to review
	alerter   OpsAlerter
	notifier  Notifier // optional Phase-12 client-notification seam
	clock     func() time.Time
	logf      func(format string, args ...any)
}

// NewDepositService wires the service; store + poster + checker are
// mandatory (fail closed).
func NewDepositService(store DepositStore, poster JournalPoster,
	checker MutableChecker) (*DepositService, error) {
	if store == nil || poster == nil || checker == nil {
		return nil, fmt.Errorf("funding: deposit service requires store, poster and mutable checker")
	}
	return &DepositService{store: store, poster: poster, checker: checker, clock: time.Now}, nil
}

// WithUSDConverter wires the tier-classification converter.
func (s *DepositService) WithUSDConverter(c UsdConverter) *DepositService {
	s.usd = c
	return s
}

// WithSanctions wires the sanctions screen.
func (s *DepositService) WithSanctions(sc SanctionsScreener) *DepositService {
	s.sanctions = sc
	return s
}

// WithAlerter wires the ops alerter.
func (s *DepositService) WithAlerter(a OpsAlerter) *DepositService {
	s.alerter = a
	return s
}

// WithNotifier wires the Phase-12 client-notification seam — emits
// deposit_confirmed after the credit journal commits (best-effort).
func (s *DepositService) WithNotifier(n Notifier) *DepositService {
	s.notifier = n
	return s
}

// WithClock overrides the clock (tests).
func (s *DepositService) WithClock(c func() time.Time) *DepositService {
	s.clock = c
	return s
}

// WithLogger wires a log sink.
func (s *DepositService) WithLogger(f func(format string, args ...any)) *DepositService {
	s.logf = f
	return s
}

// log emits to the wired sink when present — nil sink means drop.
func (s *DepositService) log(format string, args ...any) {
	if s.logf != nil {
		s.logf(format, args...)
	}
}

// notify fires the optional client notification — post-commit,
// best-effort; the panic guard keeps a misbehaving emitter from
// crashing the funding path.
func (s *DepositService) notify(ctx context.Context, accountID int64, event string, payload map[string]any) {
	if s.notifier == nil {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			s.log("funding: notifier panic on %s: %v", event, r)
		}
	}()
	s.notifier.Notify(ctx, accountID, event, payload)
}

// DepositResult is the handler-facing deposit outcome.
type DepositResult struct {
	DepositID      int64      `json:"deposit_id"`
	Status         string     `json:"status"`
	Currency       string     `json:"currency"`
	Amount         string     `json:"amount"`
	USDAmount      *string    `json:"usd_amount,omitempty"`
	ReviewTier     *string    `json:"review_tier,omitempty"`
	ReviewDeadline *time.Time `json:"review_deadline,omitempty"`
	Confirmations  int        `json:"confirmations"`
	Flags          []string   `json:"flags,omitempty"`
	Replayed       bool       `json:"replayed,omitempty"`
}

// ---------------------------------------------------------------------------
// Client intent — POST /api/v1/deposits
// ---------------------------------------------------------------------------

// CreateDepositIntentRequest is the client deposit-intent payload.
// Idempotency-Key is REQUIRED (account-scoped namespace, spec §8.8).
type CreateDepositIntentRequest struct {
	AccountID      int64
	UserID         int64
	Currency       string
	Amount         string
	Reference      string // client-supplied wire reference (optional)
	BankMethod     string // expected rail (optional)
	IdempotencyKey string // REQUIRED — Idempotency-Key header
}

// CreateIntent persists a PENDING DEPOSIT intent — no ledger posting
// (no funds have arrived). Replays on (account_id, Idempotency-Key).
func (s *DepositService) CreateIntent(ctx context.Context,
	req CreateDepositIntentRequest) (*DepositResult, error) {
	ccy, err := normalizeCurrency(req.Currency)
	if err != nil {
		return nil, err
	}
	amount, err := parseMoney(req.Amount)
	if err != nil {
		return nil, err
	}
	key := strings.TrimSpace(req.IdempotencyKey)
	if key == "" {
		return nil, errCode("INVALID_REQUEST",
			"Idempotency-Key header required for deposit intents")
	}
	if len(key) > 128 {
		return nil, errCode("INVALID_REQUEST", "Idempotency-Key exceeds 128 chars")
	}
	method := strings.ToUpper(strings.TrimSpace(req.BankMethod))
	if method != "" && !bankMethodDomain[method] {
		return nil, errf("INVALID_REQUEST", "bank_method %q is not a supported rail", method)
	}
	ref := strings.TrimSpace(req.Reference)
	if len(ref) > 128 {
		return nil, errCode("INVALID_REQUEST", "reference exceeds 128 chars")
	}
	if err := s.checker.AssertMutable(ctx, req.AccountID); err != nil {
		return nil, err
	}
	ph := payloadHash("deposit-intent", ccy, amount.String(), ref, method)

	tx, err := s.store.BeginTx(ctx)
	if err != nil {
		return nil, wrapCode("INTERNAL_ERROR", "deposit intent tx", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	d, err := s.store.InsertDepositPending(ctx, tx, DepositRow{
		AccountID:      req.AccountID,
		Currency:       ccy,
		Amount:         amount,
		Status:         FundingPending,
		BankMethod:     strPtrOrNil(method),
		Reference:      strPtrOrNil(ref),
		IdempotencyKey: &key,
		PayloadSHA256:  &ph,
	})
	if err != nil {
		if stderrors.Is(err, ErrIdemConflict) {
			_ = tx.Rollback(ctx)
			return s.replayDeposit(ctx, req.AccountID, key, ph)
		}
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, wrapCode("INTERNAL_ERROR", "deposit intent commit", err)
	}
	return &DepositResult{
		DepositID: d.ID, Status: FundingPending, Currency: ccy,
		Amount: amount.String(),
	}, nil
}

// replayDeposit resolves the §8.8 replay-or-mismatch contract.
func (s *DepositService) replayDeposit(ctx context.Context, accountID int64,
	key, ph string) (*DepositResult, error) {
	stored, err := s.store.DepositByIdemKey(ctx, accountID, key)
	if err != nil {
		return nil, err
	}
	tx, err := s.store.BeginTx(ctx)
	if err != nil {
		return nil, wrapCode("INTERNAL_ERROR", "replay tx", err)
	}
	storedPH, err := s.store.FundingTxPayloadSHA(ctx, tx, stored.ID)
	_ = tx.Rollback(ctx)
	if err != nil {
		return nil, err
	}
	if storedPH == nil || *storedPH != ph {
		return nil, errCode("IDEMPOTENCY_KEY_MISMATCH",
			"Idempotency-Key replayed with a different request payload")
	}
	return &DepositResult{
		DepositID: stored.ID, Status: stored.Status, Currency: stored.Currency,
		Amount: stored.Amount.String(), USDAmount: decPtr(stored.USDAmount),
		ReviewTier: stored.ReviewTier, ReviewDeadline: stored.ReviewDeadline,
		Replayed: true,
	}, nil
}

// ---------------------------------------------------------------------------
// Detection ingest — POST /api/v1/admin/funding/deposits
// (bank statement poll / partner webhook).
// ---------------------------------------------------------------------------

// IngestDepositRequest is one detected inbound deposit.
type IngestDepositRequest struct {
	AccountID         int64
	Currency          string
	Amount            string
	Reference         string // bank transaction id / wire reference (required)
	BankMethod        string
	OriginatorName    string // sender name reported by the source
	OriginatorAccount string
	Source            string // depositSourceDomain (required)
	IntentReference   string // optional: link a client-declared intent
	IdempotencyKey    string // optional; defaults to dep:{reference}
	ReceivedBy        int64  // ops user / poller identity
}

// IngestDetected persists the detection: PENDING DEPOSIT row + first
// source confirmation + the funds hold (Nostro → CustomerLiability,
// wallet +locked). A matching client intent is adopted instead of
// creating a second row.
func (s *DepositService) IngestDetected(ctx context.Context,
	req IngestDepositRequest) (*DepositResult, error) {
	ccy, err := normalizeCurrency(req.Currency)
	if err != nil {
		return nil, err
	}
	amount, err := parseMoney(req.Amount)
	if err != nil {
		return nil, err
	}
	ref := strings.TrimSpace(req.Reference)
	if ref == "" || len(ref) > 128 {
		return nil, errCode("INVALID_REQUEST", "reference (bank transaction id) required, ≤128 chars")
	}
	source := strings.ToUpper(strings.TrimSpace(req.Source))
	if !depositSourceDomain[source] {
		return nil, errf("INVALID_REQUEST", "source %q not in the confirmation-source domain", req.Source)
	}
	method := strings.ToUpper(strings.TrimSpace(req.BankMethod))
	if method != "" && !bankMethodDomain[method] {
		return nil, errf("INVALID_REQUEST", "bank_method %q is not a supported rail", method)
	}
	key := strings.TrimSpace(req.IdempotencyKey)
	if key == "" {
		key = "dep:" + ref // derived bank-tx dedup
	}
	if len(key) > 128 {
		return nil, errCode("INVALID_REQUEST", "idempotency key exceeds 128 chars")
	}
	var usdAmt *decimal.Decimal
	if s.usd != nil {
		if usd, cerr := s.usd.ToUSD(ctx, ccy, amount); cerr == nil {
			usdAmt = &usd
		} else {
			s.log("funding: usd conversion failed for %s deposit (tier→PENDING_REVIEW): %v", ccy, cerr)
		}
	}
	tier := TierForUSD(usdAmt)
	ph := payloadHash("deposit", ccy, amount.String(), ref, method)

	// Adopt a matching client intent (same account + declared reference).
	var adopted *FundingTxRow
	if ir := strings.TrimSpace(req.IntentReference); ir != "" {
		if f, ferr := s.store.DepositByReference(ctx, req.AccountID, ir); ferr == nil && f != nil {
			adopted = f
		}
	}
	// Bank-transaction dedup: a deposit already stamped with this bank
	// reference (a prior ingest, or an intent adopted then re-stamped
	// with the bank tx id) must never create a second funding row —
	// an adopted intent keeps the client's idempotency key, so the
	// derived bank key alone cannot dedup a re-polled statement.
	var prior *FundingTxRow
	if adopted == nil {
		if f, ferr := s.store.DepositByBankRef(ctx, req.AccountID, ref); ferr == nil && f != nil {
			prior = f
		}
	}

	tx, err := s.store.BeginTx(ctx)
	if err != nil {
		return nil, wrapCode("INTERNAL_ERROR", "deposit ingest tx", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var depositID int64
	switch {
	case adopted != nil:
		depositID = adopted.ID
		// Re-stamp the intent's declared reference with the bank's
		// transaction id so every later notification dedups on `ref`.
		if serr := s.store.StampDepositBankRef(ctx, tx, adopted.ID, ref, string(tier)); serr != nil {
			return nil, serr
		}
	case prior != nil:
		depositID = prior.ID
	default:
		d, derr := s.store.InsertDepositPending(ctx, tx, DepositRow{
			AccountID:        req.AccountID,
			Currency:         ccy,
			Amount:           amount,
			Status:           FundingPending,
			BankMethod:       strPtrOrNil(method),
			Reference:        &ref,
			ReferenceAccount: strPtrOrNil(strings.TrimSpace(req.OriginatorAccount)),
			IdempotencyKey:   &key,
			PayloadSHA256:    &ph,
			USDAmount:        usdAmt,
			ReviewTier:       &tier,
		})
		if derr != nil {
			if stderrors.Is(derr, ErrIdemConflict) {
				_ = tx.Rollback(ctx)
				return s.replayDeposit(ctx, req.AccountID, key, ph)
			}
			return nil, derr
		}
		depositID = d.ID
	}
	conf := DepositConfirmationRow{
		FundingTransactionID: depositID,
		Source:               source,
		SenderName:           strPtrOrNil(strings.TrimSpace(req.OriginatorName)),
		SenderAccount:        strPtrOrNil(strings.TrimSpace(req.OriginatorAccount)),
		PayloadSHA256:        &ph,
		ReceivedBy:           int64PtrOrNil(req.ReceivedBy),
	}
	replayed := prior != nil
	if _, cerr := s.store.InsertDepositConfirmation(ctx, tx, conf); cerr != nil {
		if stderrors.Is(cerr, ErrIdemConflict) {
			// Same source reported twice — resolve replay/mismatch below
			// inside this tx for atomicity.
			if rerr := s.resolveSourceConflict(ctx, tx, depositID, source, ph); rerr != nil {
				return nil, rerr
			}
			replayed = true
		} else {
			return nil, cerr
		}
	}
	confs, cerr := s.store.DepositConfirmations(ctx, tx, depositID)
	if cerr != nil {
		return nil, cerr
	}

	// A distinct-source notification on an existing PENDING deposit
	// completes dual-source here — the ingest poller/webhook carries
	// the same weight as the explicit confirm endpoint.
	var row *FundingTxRow
	switch {
	case prior != nil:
		row = prior
	case adopted != nil:
		row = adopted
	default:
		row = &FundingTxRow{ID: depositID, AccountID: req.AccountID,
			Currency: ccy, Amount: amount, Status: FundingPending,
			USDAmount: usdAmt, ReviewTier: &tier}
	}
	newStatus := row.Status
	var flags []string
	var deadline *time.Time
	if row.Status == FundingPending && len(distinctSources(confs)) >= 2 {
		ns, fl, dl, terr := s.applyDualSource(ctx, tx, row, confs)
		if terr != nil {
			return nil, terr
		}
		newStatus, flags, deadline = ns, fl, dl
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, wrapCode("INTERNAL_ERROR", "deposit ingest commit", err)
	}

	// Post-detection hold (idempotent — replayed posts dedupe on
	// deposit-hold:{id}). The deposit stays PENDING until the
	// dual-source gate completes.
	if herr := s.postHold(ctx, depositID, req.AccountID, ccy, amount); herr != nil {
		return nil, herr
	}
	switch newStatus {
	case FundingCompleted:
		if cerr := s.credit(ctx, row); cerr != nil {
			return nil, cerr
		}
	case FundingPendingReview:
		s.persistAlert(ctx, row, flags, deadline)
	}
	return &DepositResult{
		DepositID: depositID, Status: newStatus, Currency: ccy,
		Amount: amount.String(), USDAmount: decPtr(usdAmt),
		ReviewTier: &tier, ReviewDeadline: deadline,
		Confirmations: len(confs), Flags: flags, Replayed: replayed,
	}, nil
}

// applyDualSource resolves the anti-fraud tier inside tx once ≥2
// distinct source confirmations exist — the shared tail of Confirm
// and the ingest path's second-source notification. The caller
// commits, then credits (COMPLETED) or alerts (PENDING_REVIEW).
func (s *DepositService) applyDualSource(ctx context.Context, tx pgx.Tx,
	row *FundingTxRow, rows []DepositConfirmationRow) (string, []string, *time.Time, error) {
	flags := s.sourceOfFundsFlags(rows)
	tier := ReviewTierPendingReview
	if row.ReviewTier != nil {
		tier = *row.ReviewTier
	}
	now := s.clock().UTC()
	newStatus := FundingCompleted
	var deadline *time.Time
	switch {
	case len(flags) > 0:
		newStatus = FundingPendingReview // source-of-funds divergence
	case tier == ReviewTierAuto:
		newStatus = FundingCompleted
	case tier == ReviewTierStandard:
		if ferr := s.standardChecks(ctx, row); ferr != nil {
			if exc, ok := ferr.(*depositFlag); ok {
				flags = append(flags, exc.flag)
				newStatus = FundingPendingReview
			} else {
				return "", nil, nil, ferr
			}
		}
	default:
		newStatus = FundingPendingReview
	}
	if newStatus == FundingPendingReview {
		d := now.Add(ReviewWindow)
		deadline = &d
		if err := s.store.SetDepositReview(ctx, tx, row.ID, newStatus, deadline, nil, now); err != nil {
			return "", nil, nil, err
		}
	} else {
		if err := s.store.SetDepositCompleted(ctx, tx, row.ID, now); err != nil {
			return "", nil, nil, err
		}
	}
	return newStatus, flags, deadline, nil
}

// resolveSourceConflict decides replay-vs-mismatch for a repeated
// (deposit, source) confirmation inside the ingest tx.
func (s *DepositService) resolveSourceConflict(ctx context.Context, tx pgx.Tx,
	depositID int64, source, ph string) error {
	rows, err := s.store.DepositConfirmations(ctx, tx, depositID)
	if err != nil {
		return err
	}
	for _, r := range rows {
		if r.Source == source {
			if r.PayloadSHA256 == nil || *r.PayloadSHA256 != ph {
				return errCode("IDEMPOTENCY_KEY_MISMATCH",
					"confirmation source replayed with a different payload")
			}
			return nil // same payload — idempotent replay
		}
	}
	return wrapCode("INTERNAL_ERROR", "source conflict without stored row", nil)
}

// postHold books the received-but-unspendable hold: Nostro →
// CustomerLiability, wallet +locked. Idempotent on deposit-hold:{id}.
func (s *DepositService) postHold(ctx context.Context, depositID, accountID int64,
	ccy string, amount decimal.Decimal) error {
	res, err := s.poster.Post(ctx, ledger.Journal{
		EntryType:      ledger.EntryDeposit,
		ReferenceID:    depositID,
		Description:    fmt.Sprintf("deposit %d hold %s %s (dual-source pending)", depositID, amount, ccy),
		PostedBy:       "funding:deposit",
		IdempotencyKey: fmt.Sprintf("deposit-hold:%d", depositID),
		Lines: []ledger.Line{
			ledger.DebitLine(ledger.Nostro(ccy), ccy, amount,
				"bank deposit received"),
			ledger.CreditLine(ledger.CustomerLiability(ccy), ccy, amount,
				"client deposit held pending verification"),
		},
		Effects: []ledger.AccountEffect{{
			AccountID: accountID, Currency: ccy, LockedDelta: amount,
		}},
	})
	if err != nil && !res.Committed {
		s.raiseOps(ctx, nil, accountID, ccy, amount, "P1", "DEPOSIT_JOURNAL_FAILED",
			fmt.Sprintf("deposit %d hold journal failed — ledger needs ops replay", depositID))
		return wrapCode("INTERNAL_ERROR", "deposit hold journal", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Second-source confirmation — POST …/deposits/{id}/confirm
// ---------------------------------------------------------------------------

// DepositConfirmRequest records one additional independent source.
type DepositConfirmRequest struct {
	DepositID     int64
	Source        string // distinct from previously recorded sources
	SenderName    string
	SenderAccount string
	PayloadSHA    string // optional caller-supplied dedup hash
	ReceivedBy    int64
}

// Confirm records a source confirmation; reaching two distinct sources
// resolves the anti-fraud tier (credit or PENDING_REVIEW).
func (s *DepositService) Confirm(ctx context.Context,
	req DepositConfirmRequest) (*DepositResult, error) {
	if req.DepositID <= 0 {
		return nil, errCode("INVALID_REQUEST", "deposit id required")
	}
	source := strings.ToUpper(strings.TrimSpace(req.Source))
	if !depositSourceDomain[source] {
		return nil, errf("INVALID_REQUEST", "source %q not in the confirmation-source domain", req.Source)
	}
	ph := strings.TrimSpace(req.PayloadSHA)
	if ph == "" {
		ph = payloadHash("deposit-confirm", fmt.Sprint(req.DepositID), source,
			strings.TrimSpace(req.SenderName), strings.TrimSpace(req.SenderAccount))
	}

	tx, err := s.store.BeginTx(ctx)
	if err != nil {
		return nil, wrapCode("INTERNAL_ERROR", "deposit confirm tx", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	row, err := s.store.FundingTxForUpdate(ctx, tx, req.DepositID)
	if err != nil {
		return nil, err
	}
	if row.Type != "DEPOSIT" {
		return nil, errCode("INVALID_REQUEST", "funding transaction is not a deposit")
	}
	if row.Status != FundingPending {
		return nil, errf("INVALID_LIFECYCLE_TRANSITION",
			"deposit %d is %s — no further confirmations accepted", row.ID, row.Status)
	}
	conf := DepositConfirmationRow{
		FundingTransactionID: row.ID,
		Source:               source,
		SenderName:           strPtrOrNil(strings.TrimSpace(req.SenderName)),
		SenderAccount:        strPtrOrNil(strings.TrimSpace(req.SenderAccount)),
		PayloadSHA256:        &ph,
		ReceivedBy:           int64PtrOrNil(req.ReceivedBy),
	}
	if _, cerr := s.store.InsertDepositConfirmation(ctx, tx, conf); cerr != nil {
		if stderrors.Is(cerr, ErrIdemConflict) {
			if rerr := s.resolveSourceConflict(ctx, tx, row.ID, source, ph); rerr != nil {
				return nil, rerr
			}
			rows, rerr := s.store.DepositConfirmations(ctx, tx, row.ID)
			if rerr != nil {
				return nil, rerr
			}
			if cerr := tx.Commit(ctx); cerr != nil {
				return nil, wrapCode("INTERNAL_ERROR", "replay commit", cerr)
			}
			return &DepositResult{
				DepositID: row.ID, Status: row.Status, Currency: row.Currency,
				Amount: row.Amount.String(), USDAmount: decPtr(row.USDAmount),
				ReviewTier: row.ReviewTier, ReviewDeadline: row.ReviewDeadline,
				Confirmations: len(rows), Replayed: true,
			}, nil
		}
		return nil, cerr
	}
	rows, err := s.store.DepositConfirmations(ctx, tx, row.ID)
	if err != nil {
		return nil, err
	}
	if len(distinctSources(rows)) < 2 {
		if err := tx.Commit(ctx); err != nil {
			return nil, wrapCode("INTERNAL_ERROR", "confirm commit", err)
		}
		return &DepositResult{
			DepositID: row.ID, Status: FundingPending, Currency: row.Currency,
			Amount: row.Amount.String(), USDAmount: decPtr(row.USDAmount),
			ReviewTier: row.ReviewTier, ReviewDeadline: row.ReviewDeadline,
			Confirmations: len(rows),
		}, nil
	}

	// Dual-source complete — resolve the anti-fraud tier.
	newStatus, flags, deadline, terr := s.applyDualSource(ctx, tx, row, rows)
	if terr != nil {
		return nil, terr
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, wrapCode("INTERNAL_ERROR", "tier commit", err)
	}
	if newStatus == FundingCompleted {
		if cerr := s.credit(ctx, row); cerr != nil {
			return nil, cerr
		}
	} else {
		s.persistAlert(ctx, row, flags, deadline)
	}
	return &DepositResult{
		DepositID: row.ID, Status: newStatus, Currency: row.Currency,
		Amount: row.Amount.String(), USDAmount: decPtr(row.USDAmount),
		ReviewTier: row.ReviewTier, ReviewDeadline: deadline,
		Confirmations: len(rows), Flags: flags,
	}, nil
}

// depositFlag carries an anti-fraud flag up to the tier decision.
type depositFlag struct{ flag string }

func (e *depositFlag) Error() string { return e.flag }

// sourceOfFundsFlags compares the independent sources' originator
// data — divergent sender names across sources flag the deposit for
// review (source-of-funds consistency, Task 11.3.3 anti-fraud).
func (s *DepositService) sourceOfFundsFlags(rows []DepositConfirmationRow) []string {
	var flags []string
	var names []string
	for _, r := range rows {
		if r.SenderName != nil && strings.TrimSpace(*r.SenderName) != "" {
			names = append(names, strings.ToUpper(strings.TrimSpace(*r.SenderName)))
		}
	}
	if len(names) >= 2 && names[0] != names[len(names)-1] {
		flags = append(flags, "SOF_SENDER_DIVERGENCE")
	}
	return flags
}

// standardChecks runs the STANDARD-tier legs (velocity + sanctions).
func (s *DepositService) standardChecks(ctx context.Context, row *FundingTxRow) error {
	n, sum, err := s.store.DepositVelocity(ctx, row.AccountID,
		s.clock().UTC().Add(-DepositVelocityWindow))
	if err != nil {
		return err
	}
	if n > DepositVelocityMaxCount {
		return &depositFlag{"VELOCITY_COUNT"}
	}
	if s.usd != nil {
		usdSum, cerr := s.usd.ToUSD(ctx, row.Currency, sum)
		if cerr != nil {
			return wrapCode("INTERNAL_ERROR", "velocity usd", cerr)
		}
		if usdSum.GreaterThan(depositVelocityMaxUSD) {
			return &depositFlag{"VELOCITY_VOLUME"}
		}
	} else {
		return &depositFlag{"VELOCITY_UNAVAILABLE"}
	}
	if s.sanctions == nil {
		return &depositFlag{"SANCTIONS_UNAVAILABLE"} // fail closed
	}
	hit, err := s.sanctions.ScreenDeposit(ctx, row.AccountID,
		strVal(row.ReferenceAccount), "")
	if err != nil {
		return wrapCode("INTERNAL_ERROR", "sanctions screen", err)
	}
	if hit {
		return &depositFlag{"SANCTIONS_HIT"}
	}
	return nil
}

// credit posts the locked→available flip — the liability was booked at
// detection, so the credit journal is a zero-net GL reclass (deposit
// cleared review) whose wallet effect frees the funds. Idempotent on
// deposit-credit:{id}.
func (s *DepositService) credit(ctx context.Context, row *FundingTxRow) error {
	res, err := s.poster.Post(ctx, ledger.Journal{
		EntryType:      ledger.EntryDeposit,
		ReferenceID:    row.ID,
		Description:    fmt.Sprintf("deposit %d credited %s %s", row.ID, row.Amount, row.Currency),
		PostedBy:       "funding:deposit",
		IdempotencyKey: fmt.Sprintf("deposit-credit:%d", row.ID),
		Lines: []ledger.Line{
			ledger.DebitLine(ledger.CustomerLiability(row.Currency), row.Currency, row.Amount,
				"held deposit released to spendable"),
			ledger.CreditLine(ledger.CustomerLiability(row.Currency), row.Currency, row.Amount,
				"client deposit credited after dual-source verification"),
		},
		Effects: []ledger.AccountEffect{{
			AccountID:      row.AccountID,
			Currency:       row.Currency,
			AvailableDelta: row.Amount,
			LockedDelta:    row.Amount.Neg(),
		}},
	})
	if err != nil && !res.Committed {
		s.raiseOps(ctx, &row.ID, row.AccountID, row.Currency, row.Amount, "P1",
			"DEPOSIT_JOURNAL_FAILED",
			fmt.Sprintf("deposit %d credit journal failed — ledger needs ops replay", row.ID))
		return wrapCode("INTERNAL_ERROR", "deposit credit journal", err)
	}
	// Phase-12 Task 12.3.5: deposit_confirmed user notification —
	// post-commit, best-effort (notify swallows its own errors).
	s.notify(ctx, row.AccountID, "deposit_confirmed", map[string]any{
		"deposit_id": row.ID,
		"currency":   row.Currency,
		"amount":     row.Amount.String(),
	})
	return nil
}

// ---------------------------------------------------------------------------
// Admin review — POST …/deposits/{id}/review {action: APPROVE|REJECT}
// ---------------------------------------------------------------------------

// AdminReview resolves a PENDING_REVIEW deposit. APPROVE credits;
// REJECT marks FAILED and reverses the hold (funds leave via the
// return-wire path owned by the rails cluster).
func (s *DepositService) AdminReview(ctx context.Context, adminID, approverID,
	depositID int64, approve bool, note string) (*DepositResult, error) {
	if approverID <= 0 {
		return nil, errCode("DUAL_CONTROL_REQUIRED",
			"approver_id required — deposit review is four-eyes")
	}
	if approverID == adminID {
		return nil, errCode("DUAL_CONTROL_VIOLATION",
			"approver must differ from the reviewing admin")
	}
	tx, err := s.store.BeginTx(ctx)
	if err != nil {
		return nil, wrapCode("INTERNAL_ERROR", "deposit review tx", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	row, err := s.store.FundingTxForUpdate(ctx, tx, depositID)
	if err != nil {
		return nil, err
	}
	if row.Type != "DEPOSIT" {
		return nil, errCode("INVALID_REQUEST", "funding transaction is not a deposit")
	}
	if row.Status != FundingPendingReview {
		return nil, errf("INVALID_LIFECYCLE_TRANSITION",
			"deposit %d is %s — only PENDING_REVIEW is reviewable", row.ID, row.Status)
	}
	now := s.clock().UTC()
	if approve {
		if err := s.store.SetDepositCompleted(ctx, tx, row.ID, now); err != nil {
			return nil, err
		}
	} else {
		if err := s.store.SetDepositReview(ctx, tx, row.ID, FundingFailed,
			nil, &adminID, now); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, wrapCode("INTERNAL_ERROR", "deposit review commit", err)
	}
	if approve {
		if cerr := s.credit(ctx, row); cerr != nil {
			return nil, cerr
		}
	} else {
		s.reverseHold(ctx, row, note)
	}
	status := FundingFailed
	if approve {
		status = FundingCompleted
	}
	return &DepositResult{
		DepositID: row.ID, Status: status, Currency: row.Currency,
		Amount: row.Amount.String(), USDAmount: decPtr(row.USDAmount),
		ReviewTier: row.ReviewTier,
	}, nil
}

// reverseHold posts the compensating journal for a rejected deposit:
// DR CustomerLiability / CR Nostro, wallet locked −amount — the inbound
// credit is unwound back to the bank account (return wire).
func (s *DepositService) reverseHold(ctx context.Context, row *FundingTxRow, note string) {
	res, err := s.poster.Post(ctx, ledger.Journal{
		EntryType:      ledger.EntryAdjustment,
		ReferenceID:    row.ID,
		Description:    fmt.Sprintf("deposit %d rejected — hold reversed", row.ID),
		PostedBy:       "funding:deposit-review",
		IdempotencyKey: fmt.Sprintf("deposit-reverse:%d", row.ID),
		Lines: []ledger.Line{
			ledger.DebitLine(ledger.CustomerLiability(row.Currency), row.Currency, row.Amount,
				"rejected deposit — client liability unwound"),
			ledger.CreditLine(ledger.Nostro(row.Currency), row.Currency, row.Amount,
				"returned to originator (bank reversal)"),
		},
		Effects: []ledger.AccountEffect{{
			AccountID: row.AccountID, Currency: row.Currency,
			LockedDelta: row.Amount.Neg(),
		}},
	})
	if err != nil && !res.Committed {
		s.raiseOps(ctx, &row.ID, row.AccountID, row.Currency, row.Amount, "P1",
			"DEPOSIT_JOURNAL_FAILED",
			fmt.Sprintf("deposit %d rejection journal failed — ledger needs ops replay", row.ID))
	}
}

// persistAlert writes the durable ops-alert row + raises the NATS page
// for a PENDING_REVIEW deposit (4h SLA).
func (s *DepositService) persistAlert(ctx context.Context, row *FundingTxRow,
	flags []string, deadline *time.Time) {
	detail := fmt.Sprintf(`{"flags":%q,"review_deadline":%q}`,
		strings.Join(flags, ","), deadline.Format(time.RFC3339))
	tx, err := s.store.BeginTx(ctx)
	if err == nil {
		if _, aerr := s.store.InsertFundingOpsAlert(ctx, tx, FundingOpsAlertRow{
			Code:                 "DEPOSIT_PENDING_REVIEW",
			Severity:             "P1",
			FundingTransactionID: &row.ID,
			AccountID:            &row.AccountID,
			Currency:             &row.Currency,
			Amount:               &row.Amount,
			Summary: fmt.Sprintf("deposit %d entered PENDING_REVIEW (tier %s, flags %v)",
				row.ID, strVal(row.ReviewTier), flags),
			Detail: []byte(detail),
		}); aerr != nil {
			s.log("funding: ops alert row for deposit %d failed: %v", row.ID, aerr)
		}
		if cerr := tx.Commit(ctx); cerr != nil {
			s.log("funding: ops alert commit for deposit %d failed: %v", row.ID, cerr)
		}
	}
	if s.alerter != nil {
		_ = s.alerter.Raise(ctx, OpsAlert{
			Severity: "P1",
			Code:     "DEPOSIT_PENDING_REVIEW",
			Summary: fmt.Sprintf("deposit %d entered PENDING_REVIEW (> $50K tier or flags, 4h deadline)",
				row.ID),
			Details: map[string]string{
				"deposit_id": fmt.Sprintf("%d", row.ID),
				"account_id": fmt.Sprintf("%d", row.AccountID),
				"currency":   row.Currency,
				"amount":     row.Amount.String(),
				"flags":      strings.Join(flags, ","),
			},
		})
	}
}

// SweepReviewSLA escalates PENDING_REVIEW funding rows (deposits and
// withdrawals) whose canonical 4h ops deadline lapsed: inside one tx
// per row a durable REVIEW_SLA_BREACH alert is persisted and the
// deadline rolls forward one window, so every 4h of continued breach
// alerts exactly once — a breached row is never silently cleared.
func (s *DepositService) SweepReviewSLA(ctx context.Context, limit int) (int, error) {
	ids, err := s.store.ExpiredReviewDeadlines(ctx, limit)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, id := range ids {
		tx, terr := s.store.BeginTx(ctx)
		if terr != nil {
			return n, wrapCode("INTERNAL_ERROR", "review SLA tx", terr)
		}
		row, rerr := s.store.FundingTxForUpdate(ctx, tx, id)
		if rerr != nil {
			_ = tx.Rollback(ctx)
			return n, rerr
		}
		now := s.clock().UTC()
		if row == nil || row.Status != FundingPendingReview ||
			row.ReviewDeadline == nil || !row.ReviewDeadline.Before(now) {
			_ = tx.Rollback(ctx)
			continue
		}
		next := now.Add(ReviewWindow)
		if berr := s.store.BumpReviewDeadline(ctx, tx, id, next); berr != nil {
			_ = tx.Rollback(ctx)
			return n, berr
		}
		if _, aerr := s.store.InsertFundingOpsAlert(ctx, tx, FundingOpsAlertRow{
			Code:                 "REVIEW_SLA_BREACH",
			Severity:             "P1",
			FundingTransactionID: &id,
			AccountID:            &row.AccountID,
			Currency:             &row.Currency,
			Amount:               &row.Amount,
			Summary: fmt.Sprintf("%s %d review SLA breached — deadline rolled to %s",
				row.Type, id, next.Format(time.RFC3339)),
		}); aerr != nil {
			_ = tx.Rollback(ctx)
			return n, aerr
		}
		if cerr := tx.Commit(ctx); cerr != nil {
			return n, wrapCode("INTERNAL_ERROR", "review SLA commit", cerr)
		}
		if s.alerter != nil {
			_ = s.alerter.Raise(ctx, OpsAlert{
				Severity: "P1", Code: "REVIEW_SLA_BREACH",
				Summary: fmt.Sprintf("funding tx %d PENDING_REVIEW past the 4h ops SLA", id),
				Details: map[string]string{
					"funding_transaction_id": fmt.Sprintf("%d", id),
					"account_id":             fmt.Sprintf("%d", row.AccountID),
					"type":                   row.Type,
				},
			})
		}
		n++
	}
	return n, nil
}

// raiseOps emits a best-effort durable alert outside any tx (journal
// failure path — the alert must not be inside the failed work).
func (s *DepositService) raiseOps(ctx context.Context, depositID *int64,
	accountID int64, ccy string, amount decimal.Decimal, sev, code, summary string) {
	tx, err := s.store.BeginTx(ctx)
	if err != nil {
		return
	}
	_, _ = s.store.InsertFundingOpsAlert(ctx, tx, FundingOpsAlertRow{
		Code: code, Severity: sev, FundingTransactionID: depositID,
		AccountID: &accountID, Currency: &ccy, Amount: &amount,
		Summary: summary,
	})
	_ = tx.Commit(ctx)
	if s.alerter != nil {
		_ = s.alerter.Raise(ctx, OpsAlert{Severity: sev, Code: code, Summary: summary})
	}
}

func distinctSources(rows []DepositConfirmationRow) map[string]bool {
	m := map[string]bool{}
	for _, r := range rows {
		m[r.Source] = true
	}
	return m
}

func strVal(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func int64PtrOrNil(v int64) *int64 {
	if v == 0 {
		return nil
	}
	return &v
}
