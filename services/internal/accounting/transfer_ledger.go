// transfer_ledger.go — balanced double-entry GL journal construction for
// internal position transfers (Phase-19 Task 19.3.12; spec §13.9,
// §5.38 `position_transfers`, §5.3 zero-GL-bypass invariant).
//
// Package accounting is the ledger-journal construction home (the Phase-19
// plan named internal/accounting/transfer_ledger.go — landed as the package
// root). It is deliberately I/O-free like internal/ledger: it produces
// validated ledger.Journal values; execution (SERIALIZABLE tx, wallet
// effects, Redis locks, NATS dispatch) is the caller's
// DoubleEntryLedgerService — internal/settlement for position transfers.
//
// The journal mirrors the transfer's wallet movements (quote/settlement
// currency):
//
//	Source collateral release R_s:   DR 2100_CLIENT_COLLATERAL
//	                                 CR 2010_CUSTOMER_LIABILITY
//	Dest margin lock D > 0:          DR 2010_CUSTOMER_LIABILITY
//	                                 CR 2100_CLIENT_COLLATERAL
//	Dest margin freed D < 0:         DR 2100_CLIENT_COLLATERAL
//	                                 CR 2010_CUSTOMER_LIABILITY
//	Crystallized gain R > 0:         DR 3020_RETAINED_EARNINGS (house)
//	                                 CR 2010_CUSTOMER_LIABILITY
//	Crystallized loss R < 0:         DR 2010_CUSTOMER_LIABILITY
//	                                 CR 3020_RETAINED_EARNINGS (house)
//	Admin fee F > 0 (source pays):   DR 2010_CUSTOMER_LIABILITY
//	                                 CR 4010_TRADING_FEE_REVENUE
//
// Client liabilities and client collateral are both LIABILITY accounts in
// the client-money range (chart.go segregation map) — a liability↔liability
// reclass inside 2000–2199 never leaves the client side of the
// safeguarding boundary.
//
// Zero-sum holds per currency by construction; every line pair is
// self-balancing. Journal.Validate() re-verifies before the caller posts.
package accounting

import (
	"fmt"

	"exchange/internal/ledger"
	"exchange/pkg/decimal"
	excerrors "exchange/pkg/errors"
)

// CodeTransferJournalInvalid — scaffold code (register in the Phase-05
// Task 5.3.21 §23 registry): the posting input failed structural
// validation before any write (fail-closed, L2-class defect).
const CodeTransferJournalInvalid = "TRANSFER_JOURNAL_INVALID"

// TransferPosting carries the economics of one committed internal
// position transfer — everything needed to build its balanced journal.
// All monetary values are denominated in Currency (the instrument's quote
// currency; positions P&L is quote-denominated per spec §13.1) and must be
// at or below the DECIMAL(28,8) quantum — the ledger rejects sub-quantum
// amounts.
type TransferPosting struct {
	TransferID    int64 // position_transfers.id — journal reference
	FromAccountID int64
	ToAccountID   int64
	InstrumentID  int64
	Symbol        string // e.g. EUR/USD — narrative only
	Side          string // LONG | SHORT — narrative only
	Currency      string // ISO 4217 quote currency of the margin/P&L legs

	// Quantity is the transferred base-currency quantity; TransferPrice is
	// the official mark the transfer priced at (both > 0).
	Quantity      decimal.Decimal
	TransferPrice decimal.Decimal

	// SourceRelease ≥ 0: collateral released on the source account
	// (balances.locked → available per §13.9 (a)).
	SourceRelease decimal.Decimal
	// DestLock: signed initial-margin delta on the destination account —
	// positive locks available→locked per §13.9 (b); negative frees
	// locked→available when a NETTING-mode destination absorbed the
	// transferred quantity into an opposing position.
	DestLock decimal.Decimal
	// RealizedPnL: signed P&L crystallized on the source at the transfer
	// mark (positive = client gain funded by house equity).
	RealizedPnL decimal.Decimal
	// AdminFee ≥ 0: optional administrative transfer fee charged to the
	// source account's available balance.
	AdminFee decimal.Decimal

	// PostedBy is the service/admin identifier stamped on the journal.
	PostedBy string
	// IdempotencyKey dedups journal replays (journal_entries
	// .idempotency_key UNIQUE); callers should use
	// "position-transfer:{TransferID}".
	IdempotencyKey string
}

// BuildTransferJournal returns the validated TRANSFER journal for p.
// The wallet Effects encode the §13.9 collateral rebalance plus the
// crystallized P&L and fee on the source; the Lines are the balanced GL
// view. Structural violations return CodeTransferJournalInvalid (or the
// ledger's own coded validation errors) before anything is written.
func BuildTransferJournal(p TransferPosting) (ledger.Journal, error) {
	if err := p.validate(); err != nil {
		return ledger.Journal{}, err
	}

	ccy := p.Currency
	narr := func(what string) string {
		return fmt.Sprintf("POSITION_TRANSFER id=%d %s %s qty=%s px=%s %s",
			p.TransferID, p.Symbol, p.Side, p.Quantity, p.TransferPrice, what)
	}

	j := ledger.Journal{
		EntryType:   ledger.EntryTransfer,
		ReferenceID: p.TransferID,
		Description: fmt.Sprintf("position transfer %d: %s %s %s %d→%d @ %s",
			p.TransferID, p.Side, p.Symbol, p.Quantity, p.FromAccountID, p.ToAccountID, p.TransferPrice),
		PostedBy:       p.PostedBy,
		IdempotencyKey: p.IdempotencyKey,
	}

	// Collateral rebalance legs (§13.9 (a)+(b)).
	if p.SourceRelease.IsPositive() {
		j.Lines = append(j.Lines,
			ledger.DebitLine(ledger.ClientCollateral(ccy), ccy, p.SourceRelease,
				narr(fmt.Sprintf("release source margin acct=%d", p.FromAccountID))),
			ledger.CreditLine(ledger.CustomerLiability(ccy), ccy, p.SourceRelease,
				narr(fmt.Sprintf("restore source cash acct=%d", p.FromAccountID))),
		)
	}
	switch {
	case p.DestLock.IsPositive():
		j.Lines = append(j.Lines,
			ledger.DebitLine(ledger.CustomerLiability(ccy), ccy, p.DestLock,
				narr(fmt.Sprintf("dest margin call acct=%d", p.ToAccountID))),
			ledger.CreditLine(ledger.ClientCollateral(ccy), ccy, p.DestLock,
				narr(fmt.Sprintf("lock dest collateral acct=%d", p.ToAccountID))),
		)
	case p.DestLock.IsNegative():
		freed := p.DestLock.Neg()
		j.Lines = append(j.Lines,
			ledger.DebitLine(ledger.ClientCollateral(ccy), ccy, freed,
				narr(fmt.Sprintf("free dest collateral acct=%d", p.ToAccountID))),
			ledger.CreditLine(ledger.CustomerLiability(ccy), ccy, freed,
				narr(fmt.Sprintf("restore dest cash acct=%d", p.ToAccountID))),
		)
	}

	// Crystallized P&L leg — house equity is the counterparty account for
	// book-side client gains/losses (same convention as liquidation
	// penalty/NBP postings on the house side).
	switch {
	case p.RealizedPnL.IsPositive():
		j.Lines = append(j.Lines,
			ledger.DebitLine(ledger.RetainedEarnings(ccy), ccy, p.RealizedPnL,
				narr(fmt.Sprintf("house funds realized gain acct=%d", p.FromAccountID))),
			ledger.CreditLine(ledger.CustomerLiability(ccy), ccy, p.RealizedPnL,
				narr(fmt.Sprintf("crystallize gain acct=%d", p.FromAccountID))),
		)
	case p.RealizedPnL.IsNegative():
		loss := p.RealizedPnL.Neg()
		j.Lines = append(j.Lines,
			ledger.DebitLine(ledger.CustomerLiability(ccy), ccy, loss,
				narr(fmt.Sprintf("crystallize loss acct=%d", p.FromAccountID))),
			ledger.CreditLine(ledger.RetainedEarnings(ccy), ccy, loss,
				narr(fmt.Sprintf("house absorbs realized loss acct=%d", p.FromAccountID))),
		)
	}

	// Administrative transfer fee on the source (§13.9 step 4).
	if p.AdminFee.IsPositive() {
		j.Lines = append(j.Lines,
			ledger.DebitLine(ledger.CustomerLiability(ccy), ccy, p.AdminFee,
				narr(fmt.Sprintf("admin transfer fee acct=%d", p.FromAccountID))),
			ledger.CreditLine(ledger.TradingFeeRevenue(ccy), ccy, p.AdminFee,
				narr("admin transfer fee revenue")),
		)
	}

	// Wallet effects — the balances.locked rebalance is atomic with the GL
	// posting by construction (one journal, one tx — §13.9 invariant (d)).
	j.Effects = append(j.Effects,
		ledger.AccountEffect{
			AccountID:      p.FromAccountID,
			Currency:       ccy,
			AvailableDelta: p.SourceRelease.Add(p.RealizedPnL).Sub(p.AdminFee),
			LockedDelta:    p.SourceRelease.Neg(),
		},
		ledger.AccountEffect{
			AccountID:      p.ToAccountID,
			Currency:       ccy,
			AvailableDelta: p.DestLock.Neg(),
			LockedDelta:    p.DestLock,
		},
	)

	if err := j.Validate(); err != nil {
		return ledger.Journal{}, err
	}
	return j, nil
}

func (p TransferPosting) validate() error {
	if p.TransferID <= 0 || p.FromAccountID <= 0 || p.ToAccountID <= 0 || p.InstrumentID <= 0 {
		return excerrors.New(CodeTransferJournalInvalid,
			"transfer posting missing transfer/account/instrument id")
	}
	if p.FromAccountID == p.ToAccountID {
		return excerrors.New(CodeTransferJournalInvalid,
			"transfer posting source and destination account are identical")
	}
	if len(p.Currency) != 3 {
		return excerrors.New(CodeTransferJournalInvalid,
			fmt.Sprintf("transfer posting currency %q must be 3-letter ISO", p.Currency))
	}
	if !p.Quantity.IsPositive() || !p.TransferPrice.IsPositive() {
		return excerrors.New(CodeTransferJournalInvalid,
			"transfer posting quantity and transfer price must be > 0")
	}
	if p.SourceRelease.IsNegative() {
		return excerrors.New(CodeTransferJournalInvalid,
			fmt.Sprintf("source release %s must be >= 0", p.SourceRelease))
	}
	if p.AdminFee.IsNegative() {
		return excerrors.New(CodeTransferJournalInvalid,
			fmt.Sprintf("admin fee %s must be >= 0", p.AdminFee))
	}
	if len(p.PostedBy) > 64 || len(p.IdempotencyKey) > 128 {
		return excerrors.New(CodeTransferJournalInvalid,
			"posted_by/idempotency_key exceed journal column width")
	}
	return nil
}
