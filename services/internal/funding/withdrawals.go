// Withdrawal service — Task 5.3.6, spec §5.6/§5.7.
//
// Lifecycle: POST /api/v1/withdrawals validates, writes the PENDING
// funding row + a 15-minute withdrawal_confirmations window (token
// sha256-only) and reserves the funds through the ledger
// (available → locked, GL 2010_CUSTOMER_LIABILITY →
// 2160_CLEARING_TRANSIT — client liability reclassified to the
// transit/suspense leg while the wire awaits dispatch).
//
// POST /api/v1/withdrawals/{id}/confirm validates the token inside the
// window, then applies the canonical review tiers: < $10K AUTO →
// CONFIRMED (straight to the Phase-11 dispatch queue); $10K–$50K
// STANDARD → CONFIRMED flagged for standard checks; > $50K →
// PENDING_REVIEW with a 4-hour review deadline + ops alert.
//
// Expiry: a lapsed pending confirmation auto-cancels the withdrawal and
// releases the hold through a reversing journal — SweepExpired runs the
// batch (wired on a ticker in cmd/gateway) and Confirm performs the same
// lazy-expire inline so a token arriving late always sees the released
// state, not a stale PENDING row.
package funding

import (
	"context"
	stderrors "errors"
	"fmt"
	"strings"
	"time"

	"exchange/internal/ledger"
	"exchange/pkg/decimal"
)

// WithdrawalService drives the create → confirm → review lifecycle.
// poster + checker are required (fail closed on nil); limits, usd and
// alerter are optional enrichments wired at composition root.
type WithdrawalService struct {
	store   Store
	poster  JournalPoster
	checker MutableChecker
	limits  WithdrawalLimiter // nil = cap enforcement not wired
	usd     UsdConverter      // nil = every withdrawal tiers PENDING_REVIEW
	alerter OpsAlerter        // nil = alerts skipped (logged)
	clock   func() time.Time
	logf    func(format string, args ...any)
}

// NewWithdrawalService wires the service; store/poster/checker are
// mandatory dependencies (fail closed).
func NewWithdrawalService(store Store, poster JournalPoster, checker MutableChecker) (*WithdrawalService, error) {
	if store == nil || poster == nil || checker == nil {
		return nil, fmt.Errorf("funding: withdrawal service requires store, poster and mutable checker")
	}
	return &WithdrawalService{store: store, poster: poster, checker: checker, clock: time.Now}, nil
}

// WithLimits wires the risk.LimitsService withdrawal-cap check.
func (s *WithdrawalService) WithLimits(l WithdrawalLimiter) *WithdrawalService {
	s.limits = l
	return s
}

// WithUSDConverter wires the USD tier-classification converter.
func (s *WithdrawalService) WithUSDConverter(c UsdConverter) *WithdrawalService {
	s.usd = c
	return s
}

// WithAlerter wires the ops alerter for >$50K review entries.
func (s *WithdrawalService) WithAlerter(a OpsAlerter) *WithdrawalService {
	s.alerter = a
	return s
}

// WithLogger wires a log sink for best-effort diagnostics.
func (s *WithdrawalService) WithLogger(f func(format string, args ...any)) *WithdrawalService {
	s.logf = f
	return s
}

// WithClock overrides the clock (tests).
func (s *WithdrawalService) WithClock(c func() time.Time) *WithdrawalService {
	s.clock = c
	return s
}

// ---------------------------------------------------------------------------
// Create
// ---------------------------------------------------------------------------

// CreateWithdrawalRequest is the POST /api/v1/withdrawals payload plus
// caller identity the handler resolved from claims.
type CreateWithdrawalRequest struct {
	AccountID        int64
	UserID           int64
	Currency         string
	Amount           string // decimal text — parsed with the §5.3 quantum rules
	ReferenceAccount string // beneficiary IBAN / account reference (required)
	BankMethod       string // optional rail hint: SWIFT|SEPA|FEDNOW|ACH|CHAPS|TARGET2|WIRE|INTERNAL
	ConfirmMethod    string // email | sms | push | 2fa_totp (default email)
	IdempotencyKey   string // Idempotency-Key header (account-scoped, §8.8)
}

// WithdrawalResult is the handler-facing outcome of create/confirm.
type WithdrawalResult struct {
	WithdrawalID    int64      `json:"withdrawal_id"`
	Status          string     `json:"status"`
	Currency        string     `json:"currency"`
	Amount          string     `json:"amount"`
	USDAmount       *string    `json:"usd_amount,omitempty"`
	ReviewTier      *string    `json:"review_tier,omitempty"`
	ReviewDeadline  *time.Time `json:"review_deadline,omitempty"`
	ConfirmToken    string     `json:"confirm_token,omitempty"` // minted once — never persisted
	ExpiresAt       *time.Time `json:"expires_at,omitempty"`
	Replayed        bool       `json:"replayed,omitempty"`
	DispatchPending bool       `json:"dispatch_pending,omitempty"` // journal committed, event dispatch failed
}

// bankMethodDomain is the bank_method_enum domain (migration 007).
var bankMethodDomain = map[string]bool{
	"SWIFT": true, "SEPA": true, "FEDNOW": true, "ACH": true,
	"CHAPS": true, "TARGET2": true, "WIRE": true, "INTERNAL": true,
}

// confirmMethodDomain is the §5.7 method domain.
var confirmMethodDomain = map[string]bool{
	"email": true, "sms": true, "push": true, "2fa_totp": true,
}

// Create validates, persists (SERIALIZABLE tx) and reserves funds through
// the ledger. Idempotent on (account_id, Idempotency-Key).
func (s *WithdrawalService) Create(ctx context.Context, req CreateWithdrawalRequest) (*WithdrawalResult, error) {
	ccy, err := normalizeCurrency(req.Currency)
	if err != nil {
		return nil, err
	}
	amount, err := parseMoney(req.Amount)
	if err != nil {
		return nil, err
	}
	refAcct := strings.TrimSpace(req.ReferenceAccount)
	if refAcct == "" || len(refAcct) > 64 {
		return nil, errCode("INVALID_REQUEST",
			"reference_account is required (≤64 chars) — the beneficiary bank account")
	}
	method := strings.ToUpper(strings.TrimSpace(req.BankMethod))
	if method != "" && !bankMethodDomain[method] {
		return nil, errf("INVALID_REQUEST", "bank_method %q is not a supported rail", method)
	}
	confirmMethod := strings.ToLower(strings.TrimSpace(req.ConfirmMethod))
	if confirmMethod == "" {
		confirmMethod = "email"
	}
	if !confirmMethodDomain[confirmMethod] {
		return nil, errf("INVALID_REQUEST", "confirm method %q unsupported", confirmMethod)
	}
	if len(req.IdempotencyKey) > 128 {
		return nil, errCode("INVALID_REQUEST", "Idempotency-Key exceeds 128 chars")
	}

	// State gate (§5.3.12): FROZEN/SUSPENDED/CLOSED fail closed.
	if err := s.checker.AssertMutable(ctx, req.AccountID); err != nil {
		return nil, err
	}

	meta, err := s.store.AccountMeta(ctx, req.AccountID)
	if err != nil {
		return nil, err
	}

	// Withdrawal caps (risk.LimitsService — wired when available).
	if s.limits != nil {
		if err := s.limits.CheckWithdrawal(ctx, req.AccountID, meta.KYCTier, amount); err != nil {
			return nil, err
		}
	}

	// Review tier — unpriced currency fails closed to PENDING_REVIEW.
	var usdAmt *decimal.Decimal
	if s.usd != nil {
		if usd, cerr := s.usd.ToUSD(ctx, ccy, amount); cerr == nil {
			usdAmt = &usd
		} else {
			s.log("funding: usd conversion failed for %s (tier→PENDING_REVIEW): %v", ccy, cerr)
		}
	}
	tier := TierForUSD(usdAmt)

	token, tokenHash, err := newConfirmToken()
	if err != nil {
		return nil, wrapCode("INTERNAL_ERROR", "confirm token", err)
	}
	expiresAt := s.clock().UTC().Add(WithdrawalConfirmWindow)
	ph := payloadHash("withdrawal", ccy, amount.String(), refAcct, method)

	var keyParam *string
	if req.IdempotencyKey != "" {
		keyParam = &req.IdempotencyKey
	}
	w := WithdrawalRow{
		AccountID:        req.AccountID,
		Currency:         ccy,
		Amount:           amount,
		BankMethod:       strPtrOrNil(method),
		ReferenceAccount: &refAcct,
		IdempotencyKey:   keyParam,
		PayloadSHA256:    &ph,
		USDAmount:        usdAmt,
		ReviewTier:       &tier,
	}

	tx, err := s.store.BeginTx(ctx)
	if err != nil {
		return nil, wrapCode("INTERNAL_ERROR", "withdrawal tx", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	ins, err := s.store.InsertWithdrawal(ctx, tx, w)
	if err != nil {
		if stderrors.Is(err, ErrIdemConflict) {
			_ = tx.Rollback(ctx)
			return s.resolveReplay(ctx, req.AccountID, req.IdempotencyKey, ph)
		}
		return nil, err
	}
	w = *ins
	if _, err := s.store.InsertConfirmation(ctx, tx, ConfirmationRow{
		WithdrawalID: w.ID,
		Method:       &confirmMethod,
		TokenHash:    &tokenHash,
		ExpiresAt:    expiresAt,
	}); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, wrapCode("INTERNAL_ERROR", "withdrawal commit", err)
	}

	// Reserve the funds — one ledger posting, never a direct balances
	// update. Idempotency key makes a retried post resolve to the same
	// journal; failure marks the row FAILED and propagates the coded error.
	res, perr := s.poster.Post(ctx, ledger.Journal{
		EntryType:      ledger.EntryWithdrawal,
		ReferenceID:    w.ID,
		Description:    fmt.Sprintf("withdrawal %d hold %s %s", w.ID, amount, ccy),
		PostedBy:       fmt.Sprintf("user:%d", req.UserID),
		IdempotencyKey: fmt.Sprintf("withdrawal-hold:%d", w.ID),
		Lines: []ledger.Line{
			ledger.DebitLine(ledger.CustomerLiability(ccy), ccy, amount,
				"client balance earmarked for withdrawal"),
			ledger.CreditLine(ledger.ClearingTransit(ccy), ccy, amount,
				"pending withdrawal payout (rail dispatch lands Phase-11)"),
		},
		Effects: []ledger.AccountEffect{{
			AccountID:      req.AccountID,
			Currency:       ccy,
			AvailableDelta: amount.Neg(),
			LockedDelta:    amount,
		}},
	})
	dispatchPending := false
	if perr != nil {
		if res.Committed {
			// Journal committed; only the post-commit event dispatch
			// failed — the hold is final, never repost.
			dispatchPending = true
			s.log("funding: withdrawal %d committed with dispatch failure: %v", w.ID, perr)
		} else {
			_ = s.store.MarkWithdrawalFailed(ctx, w.ID)
			return nil, perr
		}
	}

	if s.limits != nil {
		if _, rerr := s.limits.RecordWithdrawal(ctx, req.AccountID, amount); rerr != nil {
			s.log("funding: record withdrawal usage for %d: %v", req.AccountID, rerr)
		}
	}

	return &WithdrawalResult{
		WithdrawalID:    w.ID,
		Status:          FundingPending,
		Currency:        ccy,
		Amount:          amount.String(),
		USDAmount:       decPtr(usdAmt),
		ReviewTier:      &tier,
		ConfirmToken:    token,
		ExpiresAt:       &expiresAt,
		DispatchPending: dispatchPending,
	}, nil
}

// resolveReplay handles an (account_id, idempotency_key) conflict: same
// payload hash → the stored withdrawal is the replayed result; different
// payload → IDEMPOTENCY_KEY_MISMATCH (spec §8.8).
func (s *WithdrawalService) resolveReplay(ctx context.Context, accountID int64, key, ph string) (*WithdrawalResult, error) {
	stored, err := s.store.WithdrawalByIdemKey(ctx, accountID, key)
	if err != nil {
		return nil, err
	}
	if stored.PayloadSHA256 == nil || *stored.PayloadSHA256 != ph {
		return nil, errCode("IDEMPOTENCY_KEY_MISMATCH",
			"Idempotency-Key replayed with a different request payload")
	}
	return &WithdrawalResult{
		WithdrawalID:   stored.ID,
		Status:         stored.Status,
		Currency:       stored.Currency,
		Amount:         stored.Amount.String(),
		USDAmount:      decPtr(stored.USDAmount),
		ReviewTier:     stored.ReviewTier,
		ReviewDeadline: stored.ReviewDeadline,
		Replayed:       true,
	}, nil
}

// ---------------------------------------------------------------------------
// Confirm — validates the token inside the 15-minute window and applies
// the review tier.
// ---------------------------------------------------------------------------

// ConfirmWithdrawalRequest is the POST /api/v1/withdrawals/{id}/confirm
// payload plus caller identity.
type ConfirmWithdrawalRequest struct {
	WithdrawalID int64
	AccountID    int64 // claims account — must own the withdrawal
	UserID       int64
	Token        string // plaintext confirm token issued at create
	Method       string // optional confirmation channel override
}

// Confirm transitions PENDING → CONFIRMED or → PENDING_REVIEW per the
// canonical tier. An expired window auto-cancels + releases the hold.
func (s *WithdrawalService) Confirm(ctx context.Context, req ConfirmWithdrawalRequest) (*WithdrawalResult, error) {
	if req.WithdrawalID <= 0 {
		return nil, errCode("INVALID_REQUEST", "withdrawal id required")
	}
	token := strings.TrimSpace(req.Token)
	if token == "" || len(token) > 256 {
		return nil, errCode("INVALID_REQUEST", "confirmation token required")
	}
	method := strings.ToLower(strings.TrimSpace(req.Method))
	if method != "" && !confirmMethodDomain[method] {
		return nil, errf("INVALID_REQUEST", "method %q unsupported", method)
	}

	tx, err := s.store.BeginTx(ctx)
	if err != nil {
		return nil, wrapCode("INTERNAL_ERROR", "confirm tx", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	w, err := s.store.WithdrawalForUpdate(ctx, tx, req.WithdrawalID)
	if err != nil {
		return nil, err
	}
	if w.AccountID != req.AccountID {
		return nil, errCode("FORBIDDEN", "withdrawal belongs to a different account")
	}
	// Idempotent re-confirm: already terminal-progressed states return the
	// stored row rather than erroring.
	if w.Status == FundingConfirmed || w.Status == FundingCompleted || w.Status == FundingPendingReview {
		res := resultFromRow(w)
		res.Replayed = true
		_ = tx.Commit(ctx)
		return res, nil
	}
	if w.Status != FundingPending {
		return nil, errf("INVALID_LIFECYCLE_TRANSITION",
			"withdrawal %d is %s — not confirmable", w.ID, w.Status)
	}
	conf, err := s.store.ConfirmationForUpdate(ctx, tx, w.ID)
	if err != nil {
		return nil, err
	}
	if conf.Status != "pending" {
		return nil, errf("INVALID_LIFECYCLE_TRANSITION",
			"confirmation for withdrawal %d is %s", w.ID, conf.Status)
	}

	// Window check first (canonical 15 minutes): late tokens cancel the
	// withdrawal and release the hold — fail closed, never confirm.
	if !s.clock().Before(conf.ExpiresAt) {
		if err := s.store.SetConfirmationStatus(ctx, tx, conf.ID, "cancelled", 0, ""); err != nil {
			return nil, err
		}
		if err := s.store.SetWithdrawalStatus(ctx, tx, w.ID, FundingAutoCancelled, nil, nil); err != nil {
			return nil, err
		}
		if err := tx.Commit(ctx); err != nil {
			return nil, wrapCode("INTERNAL_ERROR", "auto-cancel commit", err)
		}
		s.releaseHold(ctx, w)
		return nil, errf(CodeWithdrawalConfirmExpired,
			"withdrawal %d confirmation window expired — request auto-cancelled", w.ID)
	}

	if conf.TokenHash == nil || *conf.TokenHash != sha256Hex([]byte(token)) {
		return nil, errCode("INVALID_REQUEST", "invalid confirmation token")
	}

	tier := ReviewTierAuto
	if w.ReviewTier != nil {
		tier = *w.ReviewTier
	}
	now := s.clock().UTC()
	var deadline *time.Time
	newStatus := FundingConfirmed
	if tier == ReviewTierPendingReview {
		newStatus = FundingPendingReview
		d := now.Add(ReviewWindow)
		deadline = &d
	}
	if err := s.store.SetConfirmationStatus(ctx, tx, conf.ID, "confirmed", req.UserID, method); err != nil {
		return nil, err
	}
	if err := s.store.SetWithdrawalStatus(ctx, tx, w.ID, newStatus, &now, deadline); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, wrapCode("INTERNAL_ERROR", "confirm commit", err)
	}

	if newStatus == FundingPendingReview && s.alerter != nil {
		if aerr := s.alerter.Raise(ctx, OpsAlert{
			Severity: "P1",
			Code:     "WITHDRAWAL_PENDING_REVIEW",
			Summary:  fmt.Sprintf("withdrawal %d entered PENDING_REVIEW (> $50K tier, 4h deadline)", w.ID),
			Details: map[string]string{
				"withdrawal_id": fmt.Sprintf("%d", w.ID),
				"account_id":    fmt.Sprintf("%d", w.AccountID),
				"currency":      w.Currency,
				"amount":        w.Amount.String(),
			},
		}); aerr != nil {
			s.log("funding: ops alert for withdrawal %d failed: %v", w.ID, aerr)
		}
	}

	return &WithdrawalResult{
		WithdrawalID:   w.ID,
		Status:         newStatus,
		Currency:       w.Currency,
		Amount:         w.Amount.String(),
		USDAmount:      decPtr(w.USDAmount),
		ReviewTier:     w.ReviewTier,
		ReviewDeadline: deadline,
	}, nil
}

// ---------------------------------------------------------------------------
// Expiry sweep — releases holds for lapsed pending confirmations.
// ---------------------------------------------------------------------------

// SweepExpired auto-cancels up to limit expired pending confirmations and
// releases their ledger holds. Returns the number processed. Per-item
// failures are logged and skipped — the next tick retries.
func (s *WithdrawalService) SweepExpired(ctx context.Context, limit int) (int, error) {
	ids, err := s.store.ExpiredPendingConfirmations(ctx, limit)
	if err != nil {
		return 0, err
	}
	done := 0
	for _, wid := range ids {
		if err := s.expireOne(ctx, wid); err != nil {
			s.log("funding: expire withdrawal %d: %v", wid, err)
			continue
		}
		done++
	}
	return done, nil
}

// expireOne transitions one expired withdrawal to AUTO_CANCELLED inside a
// SERIALIZABLE tx, then posts the hold-release journal.
func (s *WithdrawalService) expireOne(ctx context.Context, withdrawalID int64) error {
	tx, err := s.store.BeginTx(ctx)
	if err != nil {
		return wrapCode("INTERNAL_ERROR", "expire tx", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	w, err := s.store.WithdrawalForUpdate(ctx, tx, withdrawalID)
	if err != nil {
		return err
	}
	if w.Status != FundingPending {
		return nil // raced a confirm — nothing to do
	}
	conf, err := s.store.ConfirmationForUpdate(ctx, tx, w.ID)
	if err != nil {
		return err
	}
	if conf.Status != "pending" || s.clock().Before(conf.ExpiresAt) {
		return nil // not actually expired
	}
	if err := s.store.SetConfirmationStatus(ctx, tx, conf.ID, "cancelled", 0, ""); err != nil {
		return err
	}
	if err := s.store.SetWithdrawalStatus(ctx, tx, w.ID, FundingAutoCancelled, nil, nil); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return wrapCode("INTERNAL_ERROR", "expire commit", err)
	}
	s.releaseHold(ctx, w)
	return nil
}

// releaseHold posts the reversing journal (locked → available, transit
// liability back to customer liability). Idempotent via
// withdrawal-release:{id}. Failure is logged + alerted — the wallet row
// stays locked until a retry/ops replays, never silently dropped.
func (s *WithdrawalService) releaseHold(ctx context.Context, w *WithdrawalRow) {
	res, err := s.poster.Post(ctx, ledger.Journal{
		EntryType:      ledger.EntryWithdrawal,
		ReferenceID:    w.ID,
		Description:    fmt.Sprintf("withdrawal %d hold release (auto-cancel)", w.ID),
		PostedBy:       "funding:sweeper",
		IdempotencyKey: fmt.Sprintf("withdrawal-release:%d", w.ID),
		Lines: []ledger.Line{
			ledger.DebitLine(ledger.ClearingTransit(w.Currency), w.Currency, w.Amount,
				"withdrawal auto-cancelled — transit liability released"),
			ledger.CreditLine(ledger.CustomerLiability(w.Currency), w.Currency, w.Amount,
				"client balance restored"),
		},
		Effects: []ledger.AccountEffect{{
			AccountID:      w.AccountID,
			Currency:       w.Currency,
			AvailableDelta: w.Amount,
			LockedDelta:    w.Amount.Neg(),
		}},
	})
	if err != nil && !res.Committed {
		s.log("funding: hold release for withdrawal %d failed: %v", w.ID, err)
		if s.alerter != nil {
			_ = s.alerter.Raise(ctx, OpsAlert{
				Severity: "P1",
				Code:     "WITHDRAWAL_HOLD_RELEASE_FAILED",
				Summary:  fmt.Sprintf("withdrawal %d hold release failed — locked funds need ops replay", w.ID),
				Err:      err.Error(),
			})
		}
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func (s *WithdrawalService) log(format string, args ...any) {
	if s.logf != nil {
		s.logf(format, args...)
	}
}

func strPtrOrNil(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func decPtr(d *decimal.Decimal) *string {
	if d == nil {
		return nil
	}
	v := d.String()
	return &v
}

func resultFromRow(w *WithdrawalRow) *WithdrawalResult {
	return &WithdrawalResult{
		WithdrawalID:   w.ID,
		Status:         w.Status,
		Currency:       w.Currency,
		Amount:         w.Amount.String(),
		USDAmount:      decPtr(w.USDAmount),
		ReviewTier:     w.ReviewTier,
		ReviewDeadline: w.ReviewDeadline,
	}
}
