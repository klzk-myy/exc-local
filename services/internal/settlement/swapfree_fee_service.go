// swapfree_fee_service.go — Islamic (Swap-Free) Administration Fee Engine
// (Phase-03 Task 3.3.23; spec §5.45, §12.8a, §24 #407).
//
// Shariah-compliant treatment for accounts with
// accounts.swapfree_status = 'VERIFIED' (lifecycle owned by Phase-14 Task
// 14.3.15 / migration 095, brought forward by migration 116):
//
//   - Tom-Next financing accrues EXACTLY 0.0 — no interest is debited or
//     credited; the would-be accrual is carried as foregone_amount on the
//     swap_accrual_records audit row (§5.21a.2 / §12.8.2: reported, never
//     silently forgiven).
//   - In its place a flat, non-interest ADMINISTRATIVE holding fee applies
//     once a position's holding duration exceeds the schedule's grace
//     period:  fee = position_lots × daily_admin_fee_usd_per_lot, assessed
//     once per rollover day while the position remains held past grace.
//
// Schedule: swap_free_admin_fees (migration 105) — per-instrument row wins
// over the global default (instrument_id IS NULL). Absence of any row
// means no admin fee is configured for that instrument (fee is a
// configured schedule, not a default).
//
// Posting: balanced GL journal — debit 2010_CUSTOMER_LIABILITY_USD,
// credit 4020_SWAPFREE_ADMIN_REVENUE_USD — plus the wallet effect on the
// client's USD available balance (AllowNegative: the fee accrues even on
// a thin balance; the margin engine, not the ledger gate, owns the
// consequence — same policy as financing accrual in swap_accrual.go).
//
// DEVIATION (recorded for spec §27): spec §5.45.3 says "posted to GL 4300"
// while Task 3.3.23 step 4 names 4020_SWAPFREE_ADMIN_REVENUE_USD. Both
// codes are seeded (migration 088); the task text is authoritative here →
// 4020. The assessment-audit table §5.45.3 describes as
// `swap_free_admin_fees` is likewise disambiguated to
// `swap_free_admin_fee_assessments` (migration 117), since the task text
// assigns that table name to the fee schedule.
//
// Idempotency: assessments are UNIQUE(position_id, roll_date); fee
// journals carry idempotency key swapfree-fee:{rollDate}:{positionId} —
// a retried/resumed rollover run never double-charges.
package settlement

import (
	"context"
	stderrors "errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"

	"exchange/internal/ledger"
	excerrors "exchange/pkg/errors"
)

// feePostedBy is the journal_entries.posted_by identity for fee postings.
const feePostedBy = "swapfree-fee-service"

// SwapFreeFeeSchedule is one row of swap_free_admin_fees — the configured
// holding-fee schedule for an instrument (or the global default).
type SwapFreeFeeSchedule struct {
	ID                     int64
	InstrumentID           int64 // 0 = global default (instrument_id IS NULL)
	HoldingGraceDays       int
	DailyAdminFeeUSDPerLot decimal.Decimal
}

// SwapFreeFeeService assesses and collects the Task 3.3.23 administrative
// holding fee during the 17:00 ET rollover. Constructed by
// NewSwapFreeFeeService; the zero value is unusable.
type SwapFreeFeeService struct {
	pool   *pgxpool.Pool
	ledger *LedgerService
}

// NewSwapFreeFeeService wires the fee engine; pool and ledgerSvc are
// required (fail-closed).
func NewSwapFreeFeeService(pool *pgxpool.Pool, ledgerSvc *LedgerService) (*SwapFreeFeeService, error) {
	if pool == nil {
		return nil, fmt.Errorf("swapfree fee: nil pgx pool")
	}
	if ledgerSvc == nil {
		return nil, fmt.Errorf("swapfree fee: nil ledger service")
	}
	return &SwapFreeFeeService{pool: pool, ledger: ledgerSvc}, nil
}

// ScheduleFor resolves the fee schedule for an instrument: a per-instrument
// row wins over the global default; (false, nil) when no schedule exists —
// the instrument then accrues no admin fee.
func (s *SwapFreeFeeService) ScheduleFor(ctx context.Context, instrumentID int64) (SwapFreeFeeSchedule, bool, error) {
	var sch SwapFreeFeeSchedule
	var instrID *int64
	err := s.pool.QueryRow(ctx, `
		SELECT id, instrument_id, holding_grace_days, daily_admin_fee_usd_per_lot
		FROM swap_free_admin_fees
		WHERE instrument_id = $1 OR instrument_id IS NULL
		ORDER BY instrument_id NULLS LAST
		LIMIT 1`, instrumentID).
		Scan(&sch.ID, &instrID, &sch.HoldingGraceDays, &sch.DailyAdminFeeUSDPerLot)
	if stderrors.Is(err, pgx.ErrNoRows) {
		return SwapFreeFeeSchedule{}, false, nil
	}
	if err != nil {
		return SwapFreeFeeSchedule{}, false,
			fmt.Errorf("swapfree fee: schedule lookup instrument %d: %w", instrumentID, err)
	}
	if instrID != nil {
		sch.InstrumentID = *instrID
	}
	return sch, true, nil
}

// ComputeAdminFee is the pure fee rule (Task 3.3.23 step 3):
// holding strictly beyond the grace period → fee = lots × per-lot rate.
// Day 5 of a 5-day grace is still free; day 6 assesses. Zero rate or an
// at-grace holding returns (0, false).
func ComputeAdminFee(lots decimal.Decimal, holdingDays, graceDays int, ratePerLotUSD decimal.Decimal) (decimal.Decimal, bool) {
	if holdingDays <= graceDays || !ratePerLotUSD.IsPositive() || !lots.IsPositive() {
		return decimal.Zero, false
	}
	return lots.Mul(ratePerLotUSD).Round(8), true
}

// HoldingDays counts the calendar days a position has been held as of the
// roll date (both normalized to UTC days via the calendar helper).
func HoldingDays(openedAt, rollDate time.Time) int {
	return DaysBetween(openedAt, rollDate)
}

// AssessAndPost evaluates one VERIFIED swap-free position at a rollover
// run and, when past the grace period, posts the balanced fee journal and
// records the assessment. Returns the assessed fee (zero when inside
// grace, unscheduled, or already assessed — re-assessment is idempotent).
//
// Only called for swapfree_status='VERIFIED' positions by the rollover
// loop; the caller owns that gate.
func (s *SwapFreeFeeService) AssessAndPost(ctx context.Context, pos RolloverPosition, rollDate time.Time, runID int64) (decimal.Decimal, error) {
	if !pos.SwapFree {
		return decimal.Zero, excerrors.New(codeInvalidRequest, fmt.Sprintf(
			"swapfree fee: position %d account %d is not VERIFIED swap-free",
			pos.ID, pos.AccountID))
	}

	sch, ok, err := s.ScheduleFor(ctx, pos.InstrumentID)
	if err != nil {
		return decimal.Zero, err
	}
	if !ok {
		return decimal.Zero, nil // no schedule → no admin fee configured
	}

	holding := HoldingDays(pos.OpenedAt, rollDate)
	fee, chargeable := ComputeAdminFee(pos.Lots(), holding, sch.HoldingGraceDays, sch.DailyAdminFeeUSDPerLot)
	if !chargeable {
		return decimal.Zero, nil
	}

	// Balanced GL journal: debit customer liability (reduces what the
	// venue owes the client), credit swap-free admin revenue.
	narrative := fmt.Sprintf(
		"SWAPFREE_ADMIN_FEE %s pos=%d holding=%dd grace=%dd lots=%s rate=%s USD/lot",
		pos.Symbol, pos.ID, holding, sch.HoldingGraceDays,
		pos.Lots().String(), sch.DailyAdminFeeUSDPerLot.String())
	j := ledger.Journal{
		EntryType:      ledger.EntryFee,
		ReferenceID:    runID,
		Description:    narrative,
		PostedBy:       feePostedBy,
		IdempotencyKey: fmt.Sprintf("swapfree-fee:%s:%d", rollDate.Format("2006-01-02"), pos.ID),
		Lines: []ledger.Line{
			ledger.DebitLine(ledger.CustomerLiability("USD"), "USD", fee, narrative),
			ledger.CreditLine(ledger.SwapfreeAdminRevenue("USD"), "USD", fee, narrative),
		},
		Effects: []ledger.AccountEffect{{
			AccountID:      pos.AccountID,
			Currency:       "USD",
			AvailableDelta: fee.Neg(),
			AllowNegative:  true, // admin fee accrues even on a thin balance
		}},
	}
	res, err := s.ledger.Post(ctx, j)
	if err != nil {
		return decimal.Zero, excerrors.Wrap(CodeRolloverCalcError,
			fmt.Sprintf("position %d: post swapfree admin fee", pos.ID), err)
	}

	// Assessment audit row (§5.45.3 shape on the disambiguated table) —
	// COLLECTED because the journal is committed at this point. The
	// (position_id, roll_date) conflict updates to the same values: a
	// replayed run re-states, never double-charges.
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO swap_free_admin_fee_assessments
		    (account_id, position_id, holding_days, admin_fee_amount, currency,
		     roll_date, journal_entry_id, status)
		VALUES ($1,$2,$3,$4,'USD',$5,NULLIF($6,0),'COLLECTED')
		ON CONFLICT (position_id, roll_date) DO UPDATE SET
		    journal_entry_id = EXCLUDED.journal_entry_id,
		    status = 'COLLECTED'`,
		pos.AccountID, pos.ID, holding, fee.String(), rollDate, res.JournalID); err != nil {
		// The fee journal is committed — a failed audit row is an
		// operational error, not a reason to pretend the fee didn't post.
		return fee, fmt.Errorf("swapfree fee: write assessment pos %d (fee committed, journal %d): %w",
			pos.ID, res.JournalID, err)
	}
	return fee, nil
}
