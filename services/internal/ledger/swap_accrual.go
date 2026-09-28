package ledger

import (
	"fmt"

	"github.com/shopspring/decimal"

	excerrors "exchange/pkg/errors"
)

// Scaffold error code for the swap-accrual domain — register in Phase-05
// Task 5.3.21.
const CodeSwapDayCountUnknown = "SWAP_DAYCOUNT_UNKNOWN"

// DayCount is the per-currency accrual convention of §5.21a item 2 —
// ACT/360 vs ACT/365 following the Task 22.3.1 convention (no separate
// accrual convention is defined here).
type DayCount string

const (
	DayCountACT360 DayCount = "ACT/360"
	DayCountACT365 DayCount = "ACT/365"
)

// Basis returns the year divisor of the convention.
func (d DayCount) Basis() decimal.Decimal {
	if d == DayCountACT365 {
		return decimal.NewFromInt(365)
	}
	return decimal.NewFromInt(360)
}

// CurrencyDayCounts is the canonical money-market convention table:
// ACT/360 for USD/EUR/JPY/CHF/CAD/MXN corridors, ACT/365 for GBP/AUD/NZD
// (sterling & commonwealth convention). The currency_day_counts table
// (migration 088) is the authoritative store; this map is the seed + the
// offline fallback used by tests and pre-DB validation.
var CurrencyDayCounts = map[string]DayCount{
	"USD": DayCountACT360, "EUR": DayCountACT360, "JPY": DayCountACT360,
	"CHF": DayCountACT360, "CAD": DayCountACT360, "MXN": DayCountACT360,
	"GBP": DayCountACT365, "AUD": DayCountACT365, "NZD": DayCountACT365,
}

// DayCountFor resolves a currency's convention. Unknown currency fails
// closed — an unclassified accrual basis must never silently default.
func DayCountFor(ccy string) (DayCount, error) {
	dc, ok := CurrencyDayCounts[ccy]
	if !ok {
		return "", excerrors.New(CodeSwapDayCountUnknown,
			fmt.Sprintf("no day-count convention seeded for currency %q", ccy))
	}
	return dc, nil
}

// SwapSide is the position direction a swap rate applies to.
type SwapSide string

const (
	SwapLong  SwapSide = "LONG"  // uses long_swap_points
	SwapShort SwapSide = "SHORT" // uses short_swap_points
)

// SwapAccrualInput is the per-position Tom-Next accrual request. The swap
// rate engine (Task 3.3.11) owns swap-point math and supplies
// InterbankAmount already day-multiplied (×1 normal, ×3 Wednesday roll,
// ×4/×5 holiday weekends); this package owns the admin markup leg, the
// sign/narrative rules and the swap-free carve-out of §5.21a item 2.
type SwapAccrualInput struct {
	AccountID       int64
	PositionID      int64
	InstrumentID    int64
	Symbol          string // e.g. "EUR/USD"
	Side            SwapSide
	InterbankAmount decimal.Decimal // signed, AccrualCurrency, days-inclusive
	Notional        decimal.Decimal // position notional in AccrualCurrency
	MarkupBps       decimal.Decimal // admin markup over interbank, per side
	AccrualCurrency string          // currency the charge settles in
	Days            int             // rollover day count for the markup leg
	SwapFree        bool            // accounts.swapfree_status == 'VERIFIED'
	ReferenceID     int64           // rollover run / position reference
	PostedBy        string          // e.g. "rollover-service"
}

// SwapAccrualRecord is the persisted accrual audit row
// (swap_accrual_records, migration 088) — interbank vs markup split for
// the §17.15 swap-rate history feed and the swap-free foregone report.
type SwapAccrualRecord struct {
	AccountID       int64
	PositionID      int64
	InstrumentID    int64
	Symbol          string
	Side            SwapSide
	Currency        string
	Days            int
	DayCount        DayCount
	InterbankAmount decimal.Decimal // signed client delta, interbank leg
	MarkupAmount    decimal.Decimal // house markup charge (>= 0)
	ClientDelta     decimal.Decimal // signed total applied to the wallet
	MarkupBps       decimal.Decimal
	SwapFree        bool
	ForegoneAmount  decimal.Decimal // signed amount that would have accrued
	Narrative       string
	JournalEntryID  int64 // back-filled by the poster once committed (0 if none)
}

// InsertSQL persists the record; journal_entry_id links back to the GL
// journal when one was posted (NULL-able for swap-free accruals).
const SwapAccrualInsertSQL = `
INSERT INTO swap_accrual_records
    (account_id, position_id, instrument_id, symbol, side, currency,
     days, day_count, interbank_amount, markup_amount, client_delta,
     markup_bps, swap_free, foregone_amount, narrative, journal_entry_id)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,NULLIF($16,0))
RETURNING id`

// SwapAccrual is the computed accrual: the balanced journal (nil when
// nothing posts) plus the audit record. Swap-free accounts carry
// Journal=nil and ForegoneAmount=the would-be delta — reported, never
// silently forgiven (§5.21a.2).
type SwapAccrual struct {
	Record  SwapAccrualRecord
	Journal *Journal
}

// ComputeSwapAccrual applies the admin markup over the engine-supplied
// interbank accrual and builds the balanced GL journal.
//
// Economics (spec §5.21a.2):
//   - markup leg = Notional × MarkupBps/10⁴ × Days/Basis, rounded to the
//     8dp quantum; it always accrues TO the house — symmetric whether the
//     interbank leg is a client credit or debit;
//   - negative policy rates post symmetrically: a negative InterbankAmount
//     reverses the interbank leg direction and keeps the explicit '-' sign
//     in the line/journal narrative;
//   - client_delta = interbank − markup.
func ComputeSwapAccrual(in SwapAccrualInput) (SwapAccrual, error) {
	if in.AccountID <= 0 || in.InstrumentID <= 0 || in.PositionID <= 0 {
		return SwapAccrual{}, excerrors.New(CodeLedgerInvalidJournal,
			"swap accrual requires positive account/position/instrument ids")
	}
	if in.Symbol == "" {
		return SwapAccrual{}, excerrors.New(CodeLedgerInvalidJournal,
			"swap accrual requires a symbol")
	}
	if in.Side != SwapLong && in.Side != SwapShort {
		return SwapAccrual{}, excerrors.New(CodeLedgerInvalidJournal,
			fmt.Sprintf("swap accrual side %q not in {LONG,SHORT}", in.Side))
	}
	if in.Days < 1 {
		return SwapAccrual{}, excerrors.New(CodeLedgerInvalidJournal,
			fmt.Sprintf("swap accrual days=%d must be >= 1", in.Days))
	}
	if in.MarkupBps.Sign() < 0 || in.Notional.Sign() < 0 {
		return SwapAccrual{}, excerrors.New(CodeLedgerInvalidJournal,
			"swap accrual markup_bps and notional must be >= 0")
	}
	if err := validCurrency(in.AccrualCurrency); err != nil {
		return SwapAccrual{}, excerrors.Wrap(CodeLedgerInvalidJournal,
			"swap accrual currency", err)
	}
	dc, err := DayCountFor(in.AccrualCurrency)
	if err != nil {
		return SwapAccrual{}, err
	}
	if !in.InterbankAmount.Round(8).Equal(in.InterbankAmount) {
		return SwapAccrual{}, excerrors.New(CodeLedgerInvalidJournal,
			"swap accrual interbank amount exceeds DECIMAL(28,8) quantum")
	}

	markup := in.Notional.Mul(in.MarkupBps).
		Div(decimal.NewFromInt(10_000)).
		Mul(decimal.NewFromInt(int64(in.Days))).
		Div(dc.Basis()).
		Round(8)
	clientDelta := in.InterbankAmount.Sub(markup)

	narrative := fmt.Sprintf(
		"TOMNEXT %s %s interbank=%s markup_bps=%s days=%d basis=%s",
		in.Symbol, in.Side, in.InterbankAmount.String(), in.MarkupBps.String(),
		in.Days, dc)

	rec := SwapAccrualRecord{
		AccountID: in.AccountID, PositionID: in.PositionID,
		InstrumentID: in.InstrumentID, Symbol: in.Symbol, Side: in.Side,
		Currency: in.AccrualCurrency, Days: in.Days, DayCount: dc,
		InterbankAmount: in.InterbankAmount, MarkupAmount: markup,
		ClientDelta: clientDelta, MarkupBps: in.MarkupBps,
		SwapFree:  in.SwapFree,
		Narrative: narrative,
	}

	// Swap-free (Islamic) verified accounts accrue exactly zero; the
	// foregone amount is carried on the record for reporting — never
	// silently forgiven (§5.21a.2, §12.8).
	if in.SwapFree {
		rec.ForegoneAmount = clientDelta
		rec.ClientDelta = decimal.Zero
		rec.Narrative = narrative + " swapfree=1 foregone=" + clientDelta.String()
		return SwapAccrual{Record: rec}, nil
	}

	lines, ok := swapJournalLines(in.AccrualCurrency, in.InterbankAmount, markup, narrative)
	if !ok {
		// Interbank and markup both zero — nothing to post; the zero-delta
		// record is still written for the accrual audit trail.
		rec.ClientDelta = decimal.Zero
		return SwapAccrual{Record: rec}, nil
	}

	j := &Journal{
		EntryType:   EntryEODRollover,
		ReferenceID: in.ReferenceID,
		Description: narrative,
		PostedBy:    in.PostedBy,
		Lines:       lines,
		Effects: []AccountEffect{{
			AccountID:      in.AccountID,
			Currency:       in.AccrualCurrency,
			AvailableDelta: clientDelta,
			// Financing accrues even when it pushes a thin balance
			// negative — the margin engine, not the ledger gate, owns
			// the consequence (§13).
			AllowNegative: true,
		}},
	}
	return SwapAccrual{Record: rec, Journal: j}, nil
}

// swapJournalLines emits the interbank and markup legs as SEPARATE line
// pairs (§5.21a: "swap journal carries interbank points + markup as
// separate lines"). Each leg is self-balancing in the accrual currency, so
// the composite journal satisfies the per-currency zero-sum invariant.
// ok=false when both legs are zero.
func swapJournalLines(ccy string, interbank, markup decimal.Decimal, narrative string) (lines []Line, ok bool) {
	if interbank.IsPositive() {
		// House pays client: revenue contra-debit, client liability credit.
		lines = append(lines,
			DebitLine(SwapRolloverRevenue(ccy), ccy, interbank, narrative+" leg=interbank"),
			CreditLine(CustomerLiability(ccy), ccy, interbank, narrative+" leg=interbank"))
	} else if interbank.IsNegative() {
		// Negative policy rates post symmetrically: client liability debit,
		// revenue credit — magnitude on the lines, sign in the narrative.
		amt := interbank.Neg()
		lines = append(lines,
			DebitLine(CustomerLiability(ccy), ccy, amt, narrative+" leg=interbank"),
			CreditLine(SwapRolloverRevenue(ccy), ccy, amt, narrative+" leg=interbank"))
	}
	if markup.IsPositive() {
		lines = append(lines,
			DebitLine(CustomerLiability(ccy), ccy, markup, narrative+" leg=markup"),
			CreditLine(SwapMarkupRevenue(ccy), ccy, markup, narrative+" leg=markup"))
	}
	return lines, len(lines) > 0
}

// ---------------------------------------------------------------------------
// Dual-controlled markup policies (swap_markup_policies, migration 088).
// ---------------------------------------------------------------------------

// MarkupPolicyStatus mirrors the swap_markup_status_enum.
type MarkupPolicyStatus string

const (
	MarkupPending MarkupPolicyStatus = "PENDING_APPROVAL"
	MarkupActive  MarkupPolicyStatus = "ACTIVE"
	MarkupRetired MarkupPolicyStatus = "RETIRED"
)

// CodeMarkupDualControl is a scaffold code for Phase-05 Task 5.3.21.
const CodeMarkupDualControl = "DUAL_CONTROL_VIOLATION"

// SwapMarkupPolicy is one row of the dual-controlled markup schedule.
type SwapMarkupPolicy struct {
	ID             int64
	InstrumentID   int64 // 0 = global default
	LongMarkupBps  decimal.Decimal
	ShortMarkupBps decimal.Decimal
	Status         MarkupPolicyStatus
	ProposedBy     string
	ApprovedBy     string
}

// Approve enforces dual control: the approver must differ from the
// proposer and the policy must be PENDING_APPROVAL. Violations fail
// closed — a self-approved markup must never activate.
func (p *SwapMarkupPolicy) Approve(approver string) error {
	if p.Status != MarkupPending {
		return excerrors.New(CodeMarkupDualControl,
			fmt.Sprintf("markup policy %d is %s, not PENDING_APPROVAL", p.ID, p.Status))
	}
	if approver == "" || approver == p.ProposedBy {
		return excerrors.New(CodeMarkupDualControl,
			"markup approver must be a different principal than the proposer")
	}
	p.ApprovedBy = approver
	p.Status = MarkupActive
	return nil
}

// MarkupFor picks the ACTIVE markup for an instrument+side; a per-instrument
// policy wins over the global (instrument_id=0) default.
func MarkupFor(policies []SwapMarkupPolicy, instrumentID int64, side SwapSide) (decimal.Decimal, bool) {
	var global *SwapMarkupPolicy
	for i := range policies {
		p := &policies[i]
		if p.Status != MarkupActive {
			continue
		}
		if p.InstrumentID == instrumentID {
			if side == SwapShort {
				return p.ShortMarkupBps, true
			}
			return p.LongMarkupBps, true
		}
		if p.InstrumentID == 0 {
			global = p
		}
	}
	if global != nil {
		if side == SwapShort {
			return global.ShortMarkupBps, true
		}
		return global.LongMarkupBps, true
	}
	return decimal.Zero, false
}
