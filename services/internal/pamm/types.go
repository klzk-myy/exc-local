package pamm

import (
	"fmt"
	"time"

	"exchange/pkg/decimal"
	excerrors "exchange/pkg/errors"
)

// Internal transaction taxonomy (spec §5.3 invariant 5 / F6). These are
// the dedicated internal investment sub-ledger types — they are never
// emitted as DEPOSIT/WITHDRAWAL ledger or funding types.
type TxnType string

const (
	TxnInvest  TxnType = "PAMM_INVEST"   // investor → pool capital allocation
	TxnRedeem  TxnType = "PAMM_REDEEM"   // pool → investor capital return
	TxnFeePerf TxnType = "PAMM_FEE_PERF" // performance-fee / profit-share
	TxnFeeMgmt TxnType = "PAMM_FEE_MGMT" // management fee accrual
)

// Direction of a sub-ledger entry — DEBIT = value into the account's
// investment scope, CREDIT = value out (mirrors ledger_direction_enum).
const (
	DirDebit  = "DEBIT"
	DirCredit = "CREDIT"
)

// Pool status lattice.
const (
	PoolActive    = "ACTIVE"
	PoolSuspended = "SUSPENDED"
	PoolClosed    = "CLOSED"
)

// Allocation status.
const (
	AllocActive = "ACTIVE"
	AllocClosed = "CLOSED"
)

// Error codes emitted by this package — all registered codes. The
// §27.1 PAMM matrix codes (PAMM_MIN_INVESTMENT_NOT_MET,
// PAMM_INVESTOR_LOCKED, PAMM_ALLOCATION_MISMATCH) are registered in
// internal/errs with this task's owner citation and emitted at their
// natural sites.
const (
	CodeInvalidRequest       = "INVALID_REQUEST"
	CodeForbidden            = "FORBIDDEN"
	CodeNotFound             = "NOT_FOUND"
	CodeInsufficientBalance  = "INSUFFICIENT_BALANCE"
	CodeInternalError        = "INTERNAL_ERROR"
	CodeIdempotencyCollision = "IDEMPOTENCY_KEY_COLLISION"
	// PAMM-surface codes per the spec §27.1 feature matrix.
	CodeMinInvestmentNotMet = "PAMM_MIN_INVESTMENT_NOT_MET" // 400
	CodeInvestorLocked      = "PAMM_INVESTOR_LOCKED"        // 403
	CodeAllocationMismatch  = "PAMM_ALLOCATION_MISMATCH"    // 500
)

func errorf(code, format string, args ...any) *excerrors.Error {
	return excerrors.New(code, fmt.Sprintf(format, args...))
}

// Pool is one pamm_pools row.
type Pool struct {
	PoolID           int64
	ManagerAccountID int64
	PoolAccountID    int64
	Name             string
	Currency         string
	MinInvestment    decimal.Decimal
	Status           string
	CreatedAt        time.Time
}

// Allocation is one pamm_allocations row — an investor's invested capital.
type Allocation struct {
	AllocationID      int64
	PoolID            int64
	InvestorAccountID int64
	Currency          string
	Invested          decimal.Decimal
	Status            string
	CreatedAt         time.Time
}

// SubledgerEntry is one pamm_subledger_entries row — the semantic record
// of an internal investment movement.
type SubledgerEntry struct {
	TxnType        TxnType
	PoolID         *int64
	CopyFollowID   *int64
	AccountID      int64
	Currency       string
	Direction      string
	Amount         decimal.Decimal
	JournalEntryID *int64
	ReferenceID    int64
	Narrative      string
	// IdempotencyKey is the namespaced dedup key ('pamm:{kind}:{client}').
	// Empty means no client-keyed dedup requested.
	IdempotencyKey string
}

// StatementRow is the investor-facing read model over
// pamm_subledger_entries — the semantic movement plus its journal
// coordinates (id/posted_at are read-model only; writes use
// SubledgerEntry).
type StatementRow struct {
	EntryID        int64           `json:"entry_id"`
	TxnType        TxnType         `json:"txn_type"`
	PoolID         *int64          `json:"pool_id,omitempty"`
	CopyFollowID   *int64          `json:"copy_follow_id,omitempty"`
	AccountID      int64           `json:"account_id"`
	Currency       string          `json:"currency"`
	Direction      string          `json:"direction"`
	Amount         decimal.Decimal `json:"amount"`
	JournalEntryID *int64          `json:"journal_entry_id,omitempty"`
	ReferenceID    int64           `json:"reference_id,omitempty"`
	Narrative      string          `json:"narrative,omitempty"`
	PostedAt       time.Time       `json:"posted_at"`
}

// PoolSummary is the pool-detail read model: the pool row plus its
// live allocation totals and (when the caller has one) the caller's own
// allocation.
type PoolSummary struct {
	Pool          Pool            `json:"pool"`
	InvestorCount int             `json:"investor_count"`
	TotalInvested decimal.Decimal `json:"total_invested"`
	MyAllocation  *Allocation     `json:"my_allocation,omitempty"`
}

// FillAllocation is one pamm_fill_allocations row — the pro-rata share of
// a pool master fill attributed to one investor.
type FillAllocation struct {
	PoolID            int64
	MasterTradeID     int64
	AllocationID      int64
	InvestorAccountID int64
	InstrumentID      int64
	Side              string
	Quantity          decimal.Decimal
	Price             decimal.Decimal
}
