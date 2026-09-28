package ledger

import (
	"fmt"
	"time"

	"github.com/shopspring/decimal"

	excerrors "exchange/pkg/errors"
)

// Scaffold code for fee-schedule domain errors — register in Phase-05
// Task 5.3.21.
const CodeFeeScheduleInvalid = "FEE_SCHEDULE_INVALID"

// FeeKind mirrors non_trading_fee_kind_enum (migration 088).
type FeeKind string

const (
	// FeeKindInactivity is charged after the account's first dormancy
	// threshold crosses; FeeKindDormancy covers deeper tiers.
	FeeKindInactivity FeeKind = "INACTIVITY"
	FeeKindDormancy   FeeKind = "DORMANCY"
	// FeeKindConversionSpread is the disclosed default conversion spread
	// per instrument (consumed by dust-convert Task 3.3.20 and MiFID II
	// cost disclosure).
	FeeKindConversionSpread FeeKind = "CONVERSION_SPREAD"
	// FeeKindFinancingSpread is the disclosed financing-spread line.
	FeeKindFinancingSpread FeeKind = "FINANCING_SPREAD"
)

// FeeUnit mirrors non_trading_fee_unit_enum.
type FeeUnit string

const (
	FeeUnitFlat     FeeUnit = "FLAT"          // amount is a flat fee in Currency
	FeeUnitBps      FeeUnit = "BPS"           // amount is bps of notional
	FeeUnitPctAnnum FeeUnit = "PCT_PER_ANNUM" // amount is %/year, accrued daily
)

// FeeStatus mirrors non_trading_fee_status_enum — dual-controlled
// lifecycle: PENDING_APPROVAL → ACTIVE → RETIRED.
type FeeStatus string

const (
	FeePendingApproval FeeStatus = "PENDING_APPROVAL"
	FeeActive          FeeStatus = "ACTIVE"
	FeeRetired         FeeStatus = "RETIRED"
)

// FeeLine is one non_trading_fee_schedule row (§5.21a item 3).
type FeeLine struct {
	ID             int64
	Kind           FeeKind
	InstrumentID   int64 // 0 = applies to all instruments
	DaysDormantMin int   // dormancy tier lower bound (days)
	DaysDormantMax int   // 0 = unbounded
	Amount         decimal.Decimal
	Unit           FeeUnit
	Currency       string
	// VatApplicable is the VAT-ability flag consumed by Phase-20 invoicing
	// (Task 20.3.6) — carried onto the journal narrative so invoice
	// generation needs no new logic (Task 3.3.19 AC).
	VatApplicable bool
	Status        FeeStatus
	EffectiveFrom time.Time
	ProposedBy    string
	ApprovedBy    string
}

// Approve enforces dual control on fee schedule rows (same rule as
// swap markup policies).
func (f *FeeLine) Approve(approver string) error {
	if f.Status != FeePendingApproval {
		return excerrors.New(CodeFeeScheduleInvalid,
			fmt.Sprintf("fee line %d is %s, not PENDING_APPROVAL", f.ID, f.Status))
	}
	if approver == "" || approver == f.ProposedBy {
		return excerrors.New(CodeFeeScheduleInvalid,
			"fee line approver must be a different principal than the proposer")
	}
	f.ApprovedBy = approver
	f.Status = FeeActive
	return nil
}

// live reports whether the line is chargeable at t.
func (f FeeLine) live(now time.Time) bool {
	return f.Status == FeeActive && !now.Before(f.EffectiveFrom)
}

// Charge computes the fee amount in f.Currency for the given context.
// notional is only meaningful for BPS / PCT_PER_ANNUM units; days is the
// accrual horizon for PCT_PER_ANNUM (uses the fee currency's day-count
// convention — same table as swap accruals).
func (f FeeLine) Charge(notional decimal.Decimal, days int) (decimal.Decimal, error) {
	switch f.Unit {
	case FeeUnitFlat:
		return f.Amount.Round(8), nil
	case FeeUnitBps:
		return notional.Mul(f.Amount).Div(decimal.NewFromInt(10_000)).Round(8), nil
	case FeeUnitPctAnnum:
		if days < 1 {
			return decimal.Zero, excerrors.New(CodeFeeScheduleInvalid,
				"PCT_PER_ANNUM fee requires days >= 1")
		}
		dc, err := DayCountFor(f.Currency)
		if err != nil {
			return decimal.Zero, err
		}
		return notional.Mul(f.Amount).Div(decimal.NewFromInt(100)).
			Mul(decimal.NewFromInt(int64(days))).Div(dc.Basis()).Round(8), nil
	default:
		return decimal.Zero, excerrors.New(CodeFeeScheduleInvalid,
			fmt.Sprintf("fee line %d has unknown unit %q", f.ID, f.Unit))
	}
}

// RevenueAccount is the GL revenue account the fee posts to.
func (f FeeLine) RevenueAccount() string {
	switch f.Kind {
	case FeeKindConversionSpread:
		return ConversionSpreadRevenue(f.Currency)
	case FeeKindFinancingSpread:
		return FundingFeeRevenue(f.Currency)
	default:
		return InactivityFeeRevenue(f.Currency)
	}
}

// FeeJournal builds the balanced journal for one assessed fee: client
// liability debit against the kind's revenue account. The VAT flag is
// embedded in the journal narrative so the Phase-20 invoicing path
// (Task 20.3.6) reads it from the ledger without new logic.
func (f FeeLine) FeeJournal(accountID int64, charge decimal.Decimal, referenceID int64, postedBy string) (Journal, error) {
	if charge.Sign() <= 0 {
		return Journal{}, excerrors.New(CodeFeeScheduleInvalid,
			"fee charge must be positive")
	}
	vat := "no"
	if f.VatApplicable {
		vat = "yes"
	}
	narrative := fmt.Sprintf("NONTRADING_FEE kind=%s fee_id=%d vat=%s",
		f.Kind, f.ID, vat)
	return Journal{
		EntryType:   EntryFee,
		ReferenceID: referenceID,
		Description: narrative,
		PostedBy:    postedBy,
		Lines: []Line{
			DebitLine(CustomerLiability(f.Currency), f.Currency, charge, narrative),
			CreditLine(f.RevenueAccount(), f.Currency, charge, narrative),
		},
		Effects: []AccountEffect{{
			AccountID:      accountID,
			Currency:       f.Currency,
			AvailableDelta: charge.Neg(),
		}},
	}, nil
}

// SelectDormancyFee picks the ACTIVE inactivity/dormancy tier matching
// daysDormant for the currency: the tier with the greatest
// DaysDormantMin ≤ daysDormant whose max is unbounded or ≥ daysDormant.
// Tiers are expected non-overlapping; an overlap fails closed by picking
// the largest Min bound (most dormant tier — the honest match, never the
// cheapest).
func SelectDormancyFee(lines []FeeLine, kind FeeKind, daysDormant int, ccy string, now time.Time) (FeeLine, bool) {
	var best FeeLine
	found := false
	for _, l := range lines {
		if l.Kind != kind || l.Currency != ccy || !l.live(now) {
			continue
		}
		if l.DaysDormantMin > daysDormant {
			continue
		}
		if l.DaysDormantMax != 0 && l.DaysDormantMax < daysDormant {
			continue
		}
		if !found || l.DaysDormantMin > best.DaysDormantMin {
			best, found = l, true
		}
	}
	return best, found
}

// ConversionSpreadBps returns the disclosed conversion spread for an
// instrument: the per-instrument ACTIVE line wins over the global
// (instrument_id=0) default. Absent lines return false — disclosure
// consumers must show "no disclosed spread", never an implied zero.
func ConversionSpreadBps(lines []FeeLine, instrumentID int64, now time.Time) (decimal.Decimal, bool) {
	return pickDisclosure(lines, FeeKindConversionSpread, instrumentID, now)
}

// FinancingSpreadBps returns the ACTIVE financing-spread disclosure line.
func FinancingSpreadBps(lines []FeeLine, instrumentID int64, now time.Time) (decimal.Decimal, bool) {
	return pickDisclosure(lines, FeeKindFinancingSpread, instrumentID, now)
}

func pickDisclosure(lines []FeeLine, kind FeeKind, instrumentID int64, now time.Time) (decimal.Decimal, bool) {
	var global *FeeLine
	for i := range lines {
		l := &lines[i]
		if l.Kind != kind || !l.live(now) {
			continue
		}
		if l.InstrumentID == instrumentID {
			return l.Amount, true
		}
		if l.InstrumentID == 0 {
			global = l
		}
	}
	if global != nil {
		return global.Amount, true
	}
	return decimal.Zero, false
}
