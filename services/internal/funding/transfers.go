// Internal transfer service — Task 5.3.23 (create) + Task 5.3.45
// (history). Spec §8.8 idempotency, §24 #144/#363.
//
// A transfer moves available balance between two accounts owned by the
// same user (same user_id — master↔sub qualifies because sub-accounts
// inherit the master's user_id at creation; accounts/handlers.go).
// Authorization is strict: the caller's claims account must resolve to a
// user_id that owns BOTH endpoints — cross-user moves reject FORBIDDEN
// (Phase-05 AC #44).
//
// The wallet mutation is a single settlement.LedgerService.Post journal:
// TRANSFER entry type, GL legs debit/credit 2010_CUSTOMER_LIABILITY_{CCY}
// (the client-liability control account nets zero — the reattribution
// lives in the per-wallet Effects + ledger_entries rows, §5.3 invariant
// 4). Insufficient available fails closed via INSUFFICIENT_BALANCE inside
// the posting's SERIALIZABLE tx.
//
// Idempotency: the transfers row carries UNIQUE (account_id,
// idempotency_key) with payload_sha256 — identical retries replay the
// stored row; a changed payload under the same key is
// IDEMPOTENCY_KEY_MISMATCH (422). The journal itself is keyed
// transfer:{id} so a retried Post after a mid-flow crash replays the
// original GL entry instead of double-moving funds.
package funding

import (
	"context"
	stderrors "errors"
	"fmt"
	"time"

	"exchange/internal/ledger"
)

// TransferService wires internal transfers to the ledger + store.
type TransferService struct {
	store   Store
	poster  JournalPoster
	checker MutableChecker
	clock   func() time.Time
	logf    func(format string, args ...any)
}

// NewTransferService wires the service (fail closed on nil deps).
func NewTransferService(store Store, poster JournalPoster, checker MutableChecker) (*TransferService, error) {
	if store == nil || poster == nil || checker == nil {
		return nil, fmt.Errorf("funding: transfer service requires store, poster and mutable checker")
	}
	return &TransferService{store: store, poster: poster, checker: checker, clock: time.Now}, nil
}

// WithLogger wires a diagnostics sink.
func (s *TransferService) WithLogger(f func(format string, args ...any)) *TransferService {
	s.logf = f
	return s
}

// CreateTransferRequest is the POST /api/v1/transfers payload plus the
// caller identity resolved from claims.
type CreateTransferRequest struct {
	CallerAccountID int64 // claims.AccountID
	FromAccountID   int64
	ToAccountID     int64
	Currency        string
	Amount          string // decimal text
	IdempotencyKey  string // Idempotency-Key header (account-scoped)
}

// TransferResult is the handler-facing outcome.
type TransferResult struct {
	Transfer        TransferRow `json:"transfer"`
	Replayed        bool        `json:"replayed,omitempty"`
	DispatchPending bool        `json:"dispatch_pending,omitempty"`
}

// Create validates ownership + mutability, then moves the funds through
// one ledger posting. Idempotent on (from_account_id, Idempotency-Key).
func (s *TransferService) Create(ctx context.Context, req CreateTransferRequest) (*TransferResult, error) {
	if req.FromAccountID <= 0 || req.ToAccountID <= 0 {
		return nil, errCode("INVALID_REQUEST", "from_account_id and to_account_id are required")
	}
	if req.FromAccountID == req.ToAccountID {
		return nil, errCode("INVALID_REQUEST",
			"from_account_id and to_account_id must differ")
	}
	ccy, err := normalizeCurrency(req.Currency)
	if err != nil {
		return nil, err
	}
	amount, err := parseMoney(req.Amount)
	if err != nil {
		return nil, err
	}
	if len(req.IdempotencyKey) > 128 {
		return nil, errCode("INVALID_REQUEST", "Idempotency-Key exceeds 128 chars")
	}

	caller, err := s.store.AccountMeta(ctx, req.CallerAccountID)
	if err != nil {
		return nil, err
	}
	from, err := s.store.AccountMeta(ctx, req.FromAccountID)
	if err != nil {
		return nil, err
	}
	to, err := s.store.AccountMeta(ctx, req.ToAccountID)
	if err != nil {
		return nil, err
	}

	// Ownership: same user_id on all three legs. This covers account↔
	// account and master↔sub (subs inherit the master's user_id) — a
	// cross-user move is FORBIDDEN (AC #44), never silently allowed.
	if from.UserID != caller.UserID || to.UserID != caller.UserID {
		return nil, errCode("FORBIDDEN",
			"transfers require both accounts to belong to the caller")
	}

	// State gates: FROZEN/SUSPENDED/CLOSED fail closed on both endpoints.
	if err := s.checker.AssertMutable(ctx, req.FromAccountID); err != nil {
		return nil, err
	}
	if err := s.checker.AssertMutable(ctx, req.ToAccountID); err != nil {
		return nil, err
	}

	ph := payloadHash("transfer", fmt.Sprintf("%d", req.FromAccountID),
		fmt.Sprintf("%d", req.ToAccountID), ccy, amount.String())
	actorID := caller.UserID
	t := TransferRow{
		AccountID:     req.FromAccountID, // idempotency namespace
		FromAccountID: req.FromAccountID,
		ToAccountID:   req.ToAccountID,
		Currency:      ccy,
		Amount:        amount,
		Actor:         "SELF",
		ActorID:       &actorID,
		PayloadSHA256: ph,
	}
	if req.IdempotencyKey != "" {
		key := req.IdempotencyKey
		t.IdempotencyKey = &key
	}

	ins, err := s.store.InsertTransfer(ctx, t)
	if err != nil {
		if stderrors.Is(err, ErrIdemConflict) {
			return s.resolveReplay(ctx, req.FromAccountID, req.IdempotencyKey, ph)
		}
		return nil, err
	}
	t = *ins

	// The ledger posting — TRANSFER entry type, both wallet effects in the
	// same SERIALIZABLE tx. INSUFFICIENT_BALANCE propagates fail-closed.
	res, perr := s.poster.Post(ctx, ledger.Journal{
		EntryType:      ledger.EntryTransfer,
		ReferenceID:    t.ID,
		Description:    fmt.Sprintf("internal transfer %d: %s %s %d→%d", t.ID, amount, ccy, t.FromAccountID, t.ToAccountID),
		PostedBy:       fmt.Sprintf("user:%d", caller.UserID),
		IdempotencyKey: fmt.Sprintf("transfer:%d", t.ID),
		Lines: []ledger.Line{
			ledger.DebitLine(ledger.CustomerLiability(ccy), ccy, amount,
				fmt.Sprintf("client %d liability released", t.FromAccountID)),
			ledger.CreditLine(ledger.CustomerLiability(ccy), ccy, amount,
				fmt.Sprintf("client %d liability assumed", t.ToAccountID)),
		},
		Effects: []ledger.AccountEffect{
			{AccountID: t.FromAccountID, Currency: ccy, AvailableDelta: amount.Neg()},
			{AccountID: t.ToAccountID, Currency: ccy, AvailableDelta: amount},
		},
	})
	dispatchPending := false
	if perr != nil {
		if res.Committed {
			// Money moved; only the BalanceChanged dispatch failed.
			dispatchPending = true
			s.log("funding: transfer %d committed with dispatch failure: %v", t.ID, perr)
		} else {
			reason := perr.Error()
			if uerr := s.store.SetTransferResult(ctx, t.ID, TransferFailed, nil, &reason); uerr != nil {
				s.log("funding: mark transfer %d failed: %v", t.ID, uerr)
			}
			return nil, perr
		}
	}
	if err := s.store.SetTransferResult(ctx, t.ID, TransferCompleted, &res.JournalID, nil); err != nil {
		return nil, err
	}
	t.Status = TransferCompleted
	t.JournalEntryID = &res.JournalID

	return &TransferResult{Transfer: t, DispatchPending: dispatchPending}, nil
}

// resolveReplay handles an (account_id, idempotency_key) conflict on the
// transfers journal: same payload → stored row replays; different → 422.
func (s *TransferService) resolveReplay(ctx context.Context, accountID int64, key, ph string) (*TransferResult, error) {
	stored, err := s.store.TransferByIdemKey(ctx, accountID, key)
	if err != nil {
		return nil, err
	}
	if stored.PayloadSHA256 != ph {
		return nil, errCode("IDEMPOTENCY_KEY_MISMATCH",
			"Idempotency-Key replayed with a different request payload")
	}
	return &TransferResult{Transfer: *stored, Replayed: true}, nil
}

// History implements GET /api/v1/transfers (Task 5.3.45) — delegates to
// the shared HistoryService read path.
func (s *TransferService) History(ctx context.Context, callerAccountID int64,
	f TransferFilter) ([]TransferRow, string, int64, error) {
	return NewHistoryService(s.store).Transfers(ctx, callerAccountID, f)
}

func (s *TransferService) log(format string, args ...any) {
	if s.logf != nil {
		s.logf(format, args...)
	}
}
