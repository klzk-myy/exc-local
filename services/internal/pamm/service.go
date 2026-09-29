package pamm

import (
	"context"
	"fmt"
	"time"

	"exchange/internal/ledger"
	"exchange/pkg/decimal"
)

// JournalPoster is the §5.3 posting seam — *settlement.LedgerService
// (DoubleEntryLedgerService) satisfies it; nil fails closed at construction.
type JournalPoster interface {
	Post(ctx context.Context, j ledger.Journal) (ledger.PostResult, error)
}

// MutableChecker is the FROZEN/SUSPENDED gate
// (*accounts.FreezeService satisfies it in production). nil = unchecked,
// which is why callers wiring production MUST supply it; the PAMM service
// fails closed on nil at construction.
type MutableChecker interface {
	AssertMutable(ctx context.Context, accountID int64) error
}

// Service is the PAMM pool engine: master-sub relationships, investor
// capital movements and the fill-allocation driver.
type Service struct {
	store   Store
	poster  JournalPoster
	checker MutableChecker
	now     func() time.Time
}

// NewService wires the engine. Every dependency is mandatory — a PAMM
// service without the posting contract or the mutability gate must never
// run (fail closed, spec §2.7).
func NewService(store Store, poster JournalPoster, checker MutableChecker) (*Service, error) {
	if store == nil || poster == nil || checker == nil {
		return nil, fmt.Errorf("pamm: store, poster and mutable checker are all required")
	}
	return &Service{store: store, poster: poster, checker: checker, now: time.Now}, nil
}

// ---------------------------------------------------------------------------
// Pool management (master-sub relationship)
// ---------------------------------------------------------------------------

// CreatePool provisions a pool under managerAccountID: the pool account is
// a real accounts row parented to the manager (one-level hierarchy, same
// user_id, KYC tier inherited) plus the pamm_pools record. The pool account
// is barred from fiat rails by the pamm_pool_funding_guard trigger.
func (s *Service) CreatePool(ctx context.Context, managerAccountID int64,
	name, currency string, minInvestment string) (*Pool, error) {
	if managerAccountID <= 0 {
		return nil, errorf(CodeInvalidRequest, "manager_account_id required")
	}
	if name == "" || len(name) > 128 {
		return nil, errorf(CodeInvalidRequest, "pool name required (≤128 chars)")
	}
	if len(currency) != 3 {
		return nil, errorf(CodeInvalidRequest, "pool currency must be ISO 4217")
	}
	min := decimal.Zero
	if minInvestment != "" {
		d, err := decimal.NewFromString(minInvestment)
		if err != nil || d.IsNegative() {
			return nil, errorf(CodeInvalidRequest, "min_investment %q invalid", minInvestment)
		}
		min = d
	}
	return s.store.CreatePool(ctx, managerAccountID, name, currency, min)
}

// ---------------------------------------------------------------------------
// Invest / Redeem — dedicated internal investment movements
// ---------------------------------------------------------------------------

// MovementRequest carries one PAMM_INVEST / PAMM_REDEEM instruction.
type MovementRequest struct {
	PoolID            int64
	InvestorAccountID int64
	Amount            string // decimal text, >0, ≤8dp
	// IdempotencyKey is the client replay key; identical replays resolve
	// to the stored movement, a changed amount under the same key is
	// IDEMPOTENCY_KEY_COLLISION.
	IdempotencyKey string
}

// MovementResult reports the committed movement.
type MovementResult struct {
	PoolID      int64   `json:"pool_id"`
	PoolAccount int64   `json:"pool_account_id"`
	Investor    int64   `json:"investor_account_id"`
	Amount      string  `json:"amount"`
	Currency    string  `json:"currency"`
	Invested    string  `json:"invested"` // post-movement allocation total
	JournalID   int64   `json:"journal_id"`
	Replayed    bool    `json:"replayed,omitempty"`
	TxnType     TxnType `json:"txn_type"`
}

// Invest moves amount from the investor's wallet into the pool account —
// a TRANSFER journal (GL: 2010 → 2170 pool liability) plus the
// PAMM_INVEST sub-ledger entry. Never a DEPOSIT/WITHDRAWAL; never touches
// the daily fiat withdrawal counters.
func (s *Service) Invest(ctx context.Context, req MovementRequest) (*MovementResult, error) {
	return s.move(ctx, req, TxnInvest)
}

// Redeem returns invested capital from the pool to the investor —
// PAMM_REDEEM; the allocation row's invested CHECK + FOR UPDATE read make
// over-redemption fail closed.
func (s *Service) Redeem(ctx context.Context, req MovementRequest) (*MovementResult, error) {
	return s.move(ctx, req, TxnRedeem)
}

func (s *Service) move(ctx context.Context, req MovementRequest, kind TxnType) (*MovementResult, error) {
	if req.PoolID <= 0 || req.InvestorAccountID <= 0 {
		return nil, errorf(CodeInvalidRequest, "pool_id and investor_account_id required")
	}
	amount, err := decimal.NewFromString(req.Amount)
	if err != nil || !amount.IsPositive() {
		return nil, errorf(CodeInvalidRequest, "amount %q must be a positive decimal", req.Amount)
	}
	if !amount.Round(8).Equal(amount) {
		return nil, errorf(CodeInvalidRequest,
			"amount %s exceeds the DECIMAL(28,8) quantum", amount)
	}
	if len(req.IdempotencyKey) > 120 {
		return nil, errorf(CodeInvalidRequest, "idempotency_key exceeds 120 chars")
	}
	idemKey := ""
	if req.IdempotencyKey != "" {
		idemKey = "pamm:" + string(kind) + ":" + req.IdempotencyKey
	}

	// Replay short-circuit: the keyed sub-ledger row is the movement's
	// idempotent record — resolve before touching the wallet.
	if idemKey != "" {
		prev, err := s.store.SubledgerByKey(ctx, idemKey)
		if err != nil {
			return nil, err
		}
		if prev != nil {
			if prev.TxnType != kind || !prev.Amount.Equal(amount) ||
				prev.AccountID != req.InvestorAccountID ||
				prev.PoolID == nil || *prev.PoolID != req.PoolID {
				return nil, errorf(CodeIdempotencyCollision,
					"idempotency_key replayed with a different movement payload")
			}
			res := &MovementResult{PoolID: req.PoolID, Investor: req.InvestorAccountID,
				Amount: amount.String(), Currency: prev.Currency, TxnType: kind,
				Replayed: true}
			if prev.JournalEntryID != nil {
				res.JournalID = *prev.JournalEntryID
			}
			return res, nil
		}
	}

	pool, err := s.store.PoolByID(ctx, req.PoolID)
	if err != nil {
		return nil, err
	}
	if pool == nil {
		return nil, errorf(CodeNotFound, "pamm pool %d not found", req.PoolID)
	}
	if pool.Status != PoolActive {
		return nil, errorf(CodeForbidden, "pamm pool %d is %s", req.PoolID, pool.Status)
	}
	if kind == TxnInvest && amount.LessThan(pool.MinInvestment) {
		return nil, errorf(CodeMinInvestmentNotMet,
			"investment %s below pool minimum %s", amount, pool.MinInvestment)
	}

	// Mutability gates: investor wallet must be mutable on both paths;
	// the pool account is internal but locked-for-trade states still gate
	// (a FROZEN pool rejects redeems too — fail closed until reviewed).
	// An investor-side failure surfaces as the spec-mapped
	// PAMM_INVESTOR_LOCKED code, carrying the gate's detail.
	if err := s.checker.AssertMutable(ctx, req.InvestorAccountID); err != nil {
		return nil, errorf(CodeInvestorLocked,
			"investor account %d not mutable: %v", req.InvestorAccountID, err)
	}
	if err := s.checker.AssertMutable(ctx, pool.PoolAccountID); err != nil {
		return nil, err
	}

	var journal ledger.Journal
	switch kind {
	case TxnInvest:
		// Investor liability released → pooled-investment liability assumed.
		journal = ledger.Journal{
			EntryType:      ledger.EntryTransfer,
			Description:    fmt.Sprintf("PAMM_INVEST pool %d: %s %s investor %d", pool.PoolID, amount, pool.Currency, req.InvestorAccountID),
			PostedBy:       fmt.Sprintf("account:%d", req.InvestorAccountID),
			IdempotencyKey: idemKey,
			Lines: []ledger.Line{
				ledger.DebitLine(ledger.CustomerLiability(pool.Currency), pool.Currency, amount,
					fmt.Sprintf("investor %d capital committed to pool %d", req.InvestorAccountID, pool.PoolID)),
				ledger.CreditLine(ledger.PAMMPoolLiability(pool.Currency), pool.Currency, amount,
					fmt.Sprintf("pool %d obligation to investor %d", pool.PoolID, req.InvestorAccountID)),
			},
			Effects: []ledger.AccountEffect{
				{AccountID: req.InvestorAccountID, Currency: pool.Currency, AvailableDelta: amount.Neg()},
				{AccountID: pool.PoolAccountID, Currency: pool.Currency, AvailableDelta: amount},
			},
		}
	case TxnRedeem:
		alloc, err := s.store.AllocationForUpdate(ctx, req.PoolID, req.InvestorAccountID)
		if err != nil {
			return nil, err
		}
		if alloc == nil || alloc.Status != AllocActive || alloc.Invested.LessThan(amount) {
			return nil, errorf(CodeInsufficientBalance,
				"redeem %s exceeds invested capital in pool %d", amount, req.PoolID)
		}
		journal = ledger.Journal{
			EntryType:      ledger.EntryTransfer,
			Description:    fmt.Sprintf("PAMM_REDEEM pool %d: %s %s investor %d", pool.PoolID, amount, pool.Currency, req.InvestorAccountID),
			PostedBy:       fmt.Sprintf("account:%d", req.InvestorAccountID),
			IdempotencyKey: idemKey,
			Lines: []ledger.Line{
				ledger.DebitLine(ledger.PAMMPoolLiability(pool.Currency), pool.Currency, amount,
					fmt.Sprintf("pool %d obligation to investor %d settled", pool.PoolID, req.InvestorAccountID)),
				ledger.CreditLine(ledger.CustomerLiability(pool.Currency), pool.Currency, amount,
					fmt.Sprintf("investor %d capital returned from pool %d", req.InvestorAccountID, pool.PoolID)),
			},
			Effects: []ledger.AccountEffect{
				{AccountID: pool.PoolAccountID, Currency: pool.Currency, AvailableDelta: amount.Neg()},
				{AccountID: req.InvestorAccountID, Currency: pool.Currency, AvailableDelta: amount},
			},
		}
	default:
		return nil, errorf(CodeInternalError, "movement kind %q unsupported", kind)
	}

	res, perr := s.poster.Post(ctx, journal)
	if perr != nil && !res.Committed {
		return nil, perr
	}
	// Committed (or committed-with-dispatch-failure): record the semantic
	// sub-ledger movement + bump invested capital. A dispatch failure keeps
	// funds final — the rows still land (§5.3: resync downstream).
	jid := res.JournalID
	delta := amount
	if kind == TxnRedeem {
		delta = amount.Neg()
	}
	alloc, err := s.store.BumpInvested(ctx, req.PoolID, req.InvestorAccountID, pool.Currency, delta)
	if err != nil {
		return nil, err
	}
	sub := []SubledgerEntry{
		{TxnType: kind, PoolID: &pool.PoolID, AccountID: req.InvestorAccountID,
			Currency: pool.Currency, Amount: amount,
			Direction: dirFor(kind, false), JournalEntryID: &jid,
			ReferenceID: alloc.AllocationID, IdempotencyKey: idemKey,
			Narrative: fmt.Sprintf("%s investor %s pool %d", kind, sideWord(kind, false), pool.PoolID)},
		{TxnType: kind, PoolID: &pool.PoolID, AccountID: pool.PoolAccountID,
			Currency: pool.Currency, Amount: amount,
			Direction: dirFor(kind, true), JournalEntryID: &jid,
			ReferenceID: alloc.AllocationID,
			Narrative:   fmt.Sprintf("%s pool %s", kind, sideWord(kind, true))},
	}
	if err := s.store.InsertSubledger(ctx, sub); err != nil {
		if err == errIdempotentReplay && idemKey != "" {
			// Post committed; a concurrent replay wrote the record first —
			// resolve to it (payload equality already proven by the key's
			// namespacing + journal-level dedup).
			prev, lerr := s.store.SubledgerByKey(ctx, idemKey)
			if lerr == nil && prev != nil {
				return &MovementResult{PoolID: pool.PoolID, PoolAccount: pool.PoolAccountID,
					Investor: req.InvestorAccountID, Amount: amount.String(),
					Currency: pool.Currency, Invested: alloc.Invested.String(),
					JournalID: jid, Replayed: true, TxnType: kind}, nil
			}
		}
		return nil, err
	}
	return &MovementResult{PoolID: pool.PoolID, PoolAccount: pool.PoolAccountID,
		Investor: req.InvestorAccountID, Amount: amount.String(),
		Currency: pool.Currency, Invested: alloc.Invested.String(),
		JournalID: jid, TxnType: kind}, nil
}

// dirFor gives the sub-ledger direction: DEBIT = into the investment
// scope. For invest the investor-side entry is a CREDIT (out of free
// wallet into pool) and the pool side is DEBIT; redeem mirrors.
func dirFor(kind TxnType, poolSide bool) string {
	in := kind == TxnInvest || kind == TxnFeePerf || kind == TxnFeeMgmt
	if poolSide {
		if in {
			return DirDebit
		}
		return DirCredit
	}
	if in {
		return DirCredit
	}
	return DirDebit
}

func sideWord(kind TxnType, poolSide bool) string {
	if poolSide {
		if kind == TxnInvest {
			return "receives"
		}
		return "pays"
	}
	if kind == TxnInvest {
		return "commits to"
	}
	return "redeems from"
}
