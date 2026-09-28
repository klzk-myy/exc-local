// Package ledger implements the double-entry general-ledger domain of
// spec §5.21/§5.3 (Phase-03 Tasks 3.3.6 + 3.3.19): journal/line types with
// the per-currency zero-sum invariant, the production chart of accounts,
// Tom-Next swap accrual economics, and the non-trading fee schedule.
//
// The package is deliberately free of I/O: SQL execution, SERIALIZABLE
// orchestration, Redis account locks and NATS BalanceChanged dispatch live
// in internal/settlement (DoubleEntryLedgerService). Everything here is
// pure and decimal-exact — no float64 ever appears on a financial path.
package ledger

import (
	"fmt"
	"sort"
	"strings"

	"github.com/shopspring/decimal"

	excerrors "exchange/pkg/errors"
)

// Error codes emitted by the ledger domain. Canonical names come from the
// spec §23 registry; scaffold codes are marked for registration in
// Phase-05 Task 5.3.21.
const (
	// CodeLedgerImbalanceAbort — spec §23 (HTTP 500, L0): journal debits do
	// not equal credits; the whole transaction aborts.
	CodeLedgerImbalanceAbort = "LEDGER_IMBALANCE_ABORT"
	// CodeLedgerUnknownAccount — scaffold (register Phase-05 5.3.21):
	// posting resolved to a code absent from chart_of_accounts.
	CodeLedgerUnknownAccount = "LEDGER_UNKNOWN_ACCOUNT"
	// CodeLedgerInvalidJournal — scaffold: malformed journal (shape,
	// precision, empty) rejected before any write.
	CodeLedgerInvalidJournal = "LEDGER_INVALID_JOURNAL"
	// CodeInsufficientBalance — spec §23 (HTTP 400): effect would drive a
	// balance component negative without AllowNegative.
	CodeInsufficientBalance = "INSUFFICIENT_BALANCE"
	// CodeAccountBusy — spec §23 (HTTP 423): account:lock:{id} mutex held.
	CodeAccountBusy = "ACCOUNT_BUSY"
	// CodeTxnConflictExhausted — spec §23 (HTTP 503): SERIALIZABLE 40001 /
	// 40P01 retry budget exhausted (§5.40).
	CodeTxnConflictExhausted = "TRANSACTION_CONFLICT_RETRY_EXHAUSTED"
	// CodeIdempotencyMismatch — spec §23: idempotency_key replayed with a
	// different payload than the committed journal.
	CodeIdempotencyMismatch = "IDEMPOTENCY_KEY_MISMATCH"
	// CodeBalanceDispatchFailed — scaffold: journal committed but the
	// BalanceChanged event could not be dispatched (post-commit; funds are
	// final — the error signals downstream resync, not a rollback).
	CodeBalanceDispatchFailed = "BALANCE_EVENT_DISPATCH_FAILED"
	// CodeLedgerLockUnavailable — scaffold: Redis lock backend unavailable
	// while a balance mutation requires it (§5.3 locking protocol).
	CodeLedgerLockUnavailable = "LEDGER_LOCK_UNAVAILABLE"
)

// EntryType mirrors the gl_entry_type_enum of migration 036 (spec §5.21
// plus the §5.3 zero-GL-bypass additions TRANSFER/ADJUSTMENT).
type EntryType string

const (
	EntryTradeFill   EntryType = "TRADE_FILL"
	EntryDeposit     EntryType = "DEPOSIT"
	EntryWithdrawal  EntryType = "WITHDRAWAL"
	EntryFee         EntryType = "FEE"
	EntryEODRollover EntryType = "EOD_ROLLOVER"
	EntryLiquidation EntryType = "LIQUIDATION"
	EntrySettlement  EntryType = "SETTLEMENT"
	EntryTransfer    EntryType = "TRANSFER"
	EntryAdjustment  EntryType = "ADJUSTMENT"
)

// validEntryType reports whether t is in the gl_entry_type_enum domain.
func validEntryType(t EntryType) bool {
	switch t {
	case EntryTradeFill, EntryDeposit, EntryWithdrawal, EntryFee,
		EntryEODRollover, EntryLiquidation, EntrySettlement,
		EntryTransfer, EntryAdjustment:
		return true
	}
	return false
}

// LedgerEntryType maps the journal type onto the §5.3 ledger_entries enum
// (that domain spells rollover as 'ROLLOVER', not 'EOD_ROLLOVER').
func (t EntryType) LedgerEntryType() string {
	if t == EntryEODRollover {
		return "ROLLOVER"
	}
	return string(t)
}

// MoneyScale is the DECIMAL(28,8) quantum of spec §5.3 — sub-quantum
// amounts are rejected rather than silently rounded (zero precision loss).
var MoneyScale = decimal.New(1, -8) // 1e-8

// Line is one side of a journal: a pure debit XOR pure credit against a
// chart_of_accounts code. Narrative carries per-line audit text (negative
// rate signs for swap postings, VAT flags for fee lines).
type Line struct {
	AccountCode string
	Currency    string // ISO 4217, 3 letters
	Debit       decimal.Decimal
	Credit      decimal.Decimal
	Narrative   string
}

// DebitLine builds a pure-debit line.
func DebitLine(accountCode, currency string, amount decimal.Decimal, narrative string) Line {
	return Line{AccountCode: accountCode, Currency: currency, Debit: amount, Narrative: narrative}
}

// CreditLine builds a pure-credit line.
func CreditLine(accountCode, currency string, amount decimal.Decimal, narrative string) Line {
	return Line{AccountCode: accountCode, Currency: currency, Credit: amount, Narrative: narrative}
}

// Amount returns the signed flow of the line: positive for debit.
func (l Line) Amount() decimal.Decimal { return l.Debit.Sub(l.Credit) }

// AccountEffect is the wallet half of a posting (spec §5.3): the signed
// change applied to one balances row, mirrored into ledger_entries and
// journal_sums inside the same transaction. AvailableDelta/LockedDelta are
// signed; their sum is the total-balance movement recorded as a DEBIT
// (increase) or CREDIT (decrease) ledger entry.
//
// A pure available↔locked shift (net zero — order placement reserving
// margin) updates the balances row only: no ledger_entries row is emitted
// because no money moved, and journal_sums stays consistent automatically.
type AccountEffect struct {
	AccountID      int64
	Currency       string
	AvailableDelta decimal.Decimal
	LockedDelta    decimal.Decimal
	// AllowNegative permits available to go negative (liquidation, NBP,
	// financing accrual on a thin balance). Locked must never go negative.
	AllowNegative bool
}

// Net returns the total-balance movement of the effect.
func (e AccountEffect) Net() decimal.Decimal {
	return e.AvailableDelta.Add(e.LockedDelta)
}

// Journal is THE posting contract consumed by DoubleEntryLedgerService.
// Lines are the GL view (journal_entries + ledger_lines, §5.21); Effects
// are the wallet view (balances + ledger_entries + journal_sums, §5.3).
// A GL-only posting (house-to-house reclass) carries no Effects; a wallet
// mutation without balanced GL Lines is forbidden (zero GL bypass, §5.3
// invariant 4) — Validate rejects that shape.
type Journal struct {
	EntryType   EntryType
	ReferenceID int64  // source transaction id (trades.id etc.); 0 = none
	Description string // ≤255 chars — audit narrative
	PostedBy    string // service or admin identifier
	// IdempotencyKey, when non-empty, dedups replays against
	// journal_entries.idempotency_key (UNIQUE). A replayed key carrying a
	// different payload returns IDEMPOTENCY_KEY_MISMATCH.
	IdempotencyKey string
	Lines          []Line
	Effects        []AccountEffect
}

// Validate enforces the structural invariants that hold regardless of the
// chart: ≥2 lines, each line a pure positive debit XOR credit at ≤8dp,
// known entry type, and — the §5.21 zero-sum constraint —
// SUM(debits) == SUM(credits) per currency (decimal-exact). Any violation
// returns a coded error before a single byte is written (fail-closed).
func (j Journal) Validate() error {
	if !validEntryType(j.EntryType) {
		return excerrors.New(CodeLedgerInvalidJournal,
			fmt.Sprintf("journal entry_type %q is not in the GL domain", j.EntryType))
	}
	if len(j.Description) > 255 || len(j.PostedBy) > 64 || len(j.IdempotencyKey) > 128 {
		return excerrors.New(CodeLedgerInvalidJournal,
			"journal description/posted_by/idempotency_key exceed column width")
	}
	if len(j.Lines) < 2 {
		return excerrors.New(CodeLedgerInvalidJournal,
			"journal must carry at least two lines (double-entry)")
	}
	// Effects are optional (GL-only postings like house reclass entries are
	// legal); the zero-sum check below applies to Lines regardless.

	type sums struct{ d, c decimal.Decimal }
	per := map[string]*sums{}
	for i, l := range j.Lines {
		if err := l.validate(i); err != nil {
			return err
		}
		s := per[l.Currency]
		if s == nil {
			s = &sums{}
			per[l.Currency] = s
		}
		s.d = s.d.Add(l.Debit)
		s.c = s.c.Add(l.Credit)
	}
	// The zero-sum invariant — spec §5.21/§5.40, L0 fatal class.
	for ccy, s := range per {
		if !s.d.Equal(s.c) {
			return excerrors.New(CodeLedgerImbalanceAbort, fmt.Sprintf(
				"currency %s: SUM(debits)=%s != SUM(credits)=%s",
				ccy, s.d.String(), s.c.String()))
		}
	}

	for i, e := range j.Effects {
		if e.AccountID <= 0 {
			return excerrors.New(CodeLedgerInvalidJournal,
				fmt.Sprintf("effect %d: account_id must be positive", i))
		}
		if err := validCurrency(e.Currency); err != nil {
			return excerrors.Wrap(CodeLedgerInvalidJournal,
				fmt.Sprintf("effect %d currency", i), err)
		}
		for label, d := range map[string]decimal.Decimal{
			"available_delta": e.AvailableDelta, "locked_delta": e.LockedDelta,
		} {
			if !d.Round(8).Equal(d) {
				return excerrors.New(CodeLedgerInvalidJournal, fmt.Sprintf(
					"effect %d %s %s exceeds DECIMAL(28,8) quantum", i, label, d.String()))
			}
		}
	}
	return nil
}

func (l Line) validate(idx int) error {
	if l.AccountCode == "" || len(l.AccountCode) > 48 {
		return excerrors.New(CodeLedgerInvalidJournal,
			fmt.Sprintf("line %d: account_code empty or >48 chars", idx))
	}
	if err := validCurrency(l.Currency); err != nil {
		return excerrors.Wrap(CodeLedgerInvalidJournal, fmt.Sprintf("line %d currency", idx), err)
	}
	debit, credit := l.Debit.Sign(), l.Credit.Sign()
	if debit < 0 || credit < 0 {
		return excerrors.New(CodeLedgerInvalidJournal,
			fmt.Sprintf("line %d: negative debit/credit amount", idx))
	}
	if (debit > 0) == (credit > 0) {
		return excerrors.New(CodeLedgerInvalidJournal, fmt.Sprintf(
			"line %d: exactly one of debit/credit must be positive (got debit=%s credit=%s)",
			idx, l.Debit.String(), l.Credit.String()))
	}
	for label, d := range map[string]decimal.Decimal{"debit": l.Debit, "credit": l.Credit} {
		if !d.Round(8).Equal(d) {
			return excerrors.New(CodeLedgerInvalidJournal, fmt.Sprintf(
				"line %d %s %s exceeds DECIMAL(28,8) quantum", idx, label, d.String()))
		}
	}
	if len(l.Narrative) > 255 {
		return excerrors.New(CodeLedgerInvalidJournal,
			fmt.Sprintf("line %d: narrative exceeds 255 chars", idx))
	}
	return nil
}

func validCurrency(ccy string) error {
	if len(ccy) != 3 || strings.ToUpper(ccy) != ccy {
		return fmt.Errorf("currency %q must be a 3-letter uppercase ISO code", ccy)
	}
	return nil
}

// ValidateAccounts resolves every line's account code against the chart —
// Task 3.3.19 AC: "unknown account aborts fail-closed". ch must be the
// chart loaded from chart_of_accounts (never a caller-supplied subset).
func (j Journal) ValidateAccounts(ch *Chart) error {
	if ch == nil {
		return excerrors.New(CodeLedgerInvalidJournal,
			"chart required for account resolution (fail-closed)")
	}
	for i, l := range j.Lines {
		acct, ok := ch.Lookup(l.AccountCode)
		if !ok {
			return excerrors.New(CodeLedgerUnknownAccount,
				fmt.Sprintf("line %d: account_code %q is not seeded in chart_of_accounts",
					i, l.AccountCode))
		}
		if acct.Currency != l.Currency {
			return excerrors.New(CodeLedgerInvalidJournal, fmt.Sprintf(
				"line %d: account %s is denominated %s, line claims %s",
				i, l.AccountCode, acct.Currency, l.Currency))
		}
	}
	return nil
}

// AffectedAccounts returns the sorted distinct account IDs touched by
// Effects — deterministic ordering for mutex acquisition (deadlock-safe).
func (j Journal) AffectedAccounts() []int64 {
	seen := map[int64]struct{}{}
	for _, e := range j.Effects {
		seen[e.AccountID] = struct{}{}
	}
	ids := make([]int64, 0, len(seen))
	for id := range seen {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(a, b int) bool { return ids[a] < ids[b] })
	return ids
}

// PostResult reports what a committed posting produced.
type PostResult struct {
	JournalID      int64
	LedgerLineIDs  []int64
	LedgerEntryIDs []int64 // §5.3 ledger_entries ids (per effect)
	// Events are BalanceChanged payloads (one per affected account) the
	// caller MUST dispatch after commit (§5.3 invariant 4).
	Events    []BalanceEvent
	Committed bool // true once the journal is durably committed
	Replayed  bool // true when an idempotent replay resolved to a prior journal
}

// BalanceEvent is the account.balance.changed payload (Task 3.3.6 step 8).
// Amounts are decimal strings — JSON never carries a float64.
type BalanceEvent struct {
	AccountID     int64  `json:"account_id"`
	Currency      string `json:"currency"`
	JournalID     int64  `json:"journal_id"`
	LedgerEntryID int64  `json:"ledger_entry_id"`
	Available     string `json:"available"`
	Locked        string `json:"locked"`
	Total         string `json:"total"`
	EventType     string `json:"event_type"` // "BALANCE_CHANGED"
}

// BalanceChangedSubject is the §5.3 topic: account.balance.changed.{id}.
func BalanceChangedSubject(accountID int64) string {
	return fmt.Sprintf("account.balance.changed.%d", accountID)
}
