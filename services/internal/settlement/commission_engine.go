// commission_engine.go — Commission Engine & Dual Fee Model
// (Phase-03 Task 3.3.13 + Task 3.3.17; spec §8.5, §5.41, §24 #223/#291;
// migration 119).
//
// Two account fee models (spec §5.41: account_product_profiles.pricing_plan
// is the single source — Phase-14 Task 14.3.13 owns the table, migration
// 095):
//
//	SPREAD_MARKUP           — LP pricing is widened by the configured
//	                          markup before distribution; the fee is
//	                          implicit in the spread (Task 3.3.4 logic) and
//	                          NO separate commission is charged.
//	RAW_SPREAD_COMMISSION   — LP pricing passes through unmarked; an
//	                          explicit commission is charged per lot and/or
//	                          per million of notional on every execution.
//
// Commission economics (raw model):
//
//	charge = rate_per_lot × (qty / lot_size)
//	       + rate_per_million × (qty × price / 1e6)   [quote currency]
//
// from the commission_tiers row with the greatest min_monthly_volume met by
// the account's calendar-month USD notional (account_monthly_volume — the
// fill's own volume is recorded AFTER tier resolution, so a tier boundary
// takes effect on the next fill, never mid-fill).
//
// GL posting (distinct from spread revenue per Task 3.3.13 AC):
//
//	commission charge:  DR 2010_CUSTOMER_LIABILITY_{ccy}
//	                    CR 4030_COMMISSION_REVENUE_{ccy}
//	maker rebate:       DR 5100_LIQUIDITY_REBATE_EXPENSE_{ccy}
//	                    CR 2100_CLIENT_COLLATERAL_{ccy}   ("2100-CLIENT-FUNDS")
//
// Task 3.3.17 — negative maker fees (VIP 4+, migration 086 maker_bps < 0):
// on a maker fill the rebate = |maker_bps| × notional / 10⁴ is credited to
// the client's cash balance and debited from the liquidity-rebate expense
// account, surfaced as a signed (negative) fee in the execution report.
// A negative maker rate on a sub-VIP-4 account is configuration corruption
// and fails closed with COMMISSION_CONFIG_INVALID.
package settlement

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"

	"exchange/internal/ledger"
	"exchange/internal/position"
	excerrors "exchange/pkg/errors"
)

// Scaffold error codes — register in the Phase-05 Task 5.3.21 registry.
const (
	// CodeCommissionConfigInvalid — fee-model/tier/rate inputs are
	// incoherent (unknown model, negative rate below VIP 4, empty tier
	// ladder for a raw account). Fail-closed: never guess a fee.
	CodeCommissionConfigInvalid = "COMMISSION_CONFIG_INVALID"
	// CodeCommissionEngineInternal — store/converter failures.
	CodeCommissionEngineInternal = "COMMISSION_ENGINE_INTERNAL"
)

// FeeModel mirrors account_product_profiles.pricing_plan (§5.41).
type FeeModel string

const (
	FeeModelSpreadMarkup        FeeModel = "SPREAD_MARKUP"
	FeeModelRawSpreadCommission FeeModel = "RAW_SPREAD_COMMISSION"
)

func (m FeeModel) valid() bool {
	return m == FeeModelSpreadMarkup || m == FeeModelRawSpreadCommission
}

// FillRole distinguishes the liquidity side of an execution.
type FillRole string

const (
	FillRoleMaker FillRole = "MAKER"
	FillRoleTaker FillRole = "TAKER"
)

// VipMakerRebateMinTier is the first VIP tier eligible for negative maker
// rates (Task 3.3.17 — vip_tier_schedule seeds maker_bps < 0 from VIP 4).
const VipMakerRebateMinTier = 4

// CommissionTier is one commission_tiers row (migration 119).
type CommissionTier struct {
	TierID           int64
	TierName         string
	MinMonthlyVolume decimal.Decimal // USD-equivalent monthly notional bound
	RatePerLot       decimal.Decimal // quote-ccy per standard lot
	RatePerMillion   decimal.Decimal // quote-ccy per 1e6 quote notional
}

// TierForVolume returns the highest tier whose volume threshold is met.
// No match (shouldn't happen — tier 0 has bound 0) returns ok=false so the
// caller fails closed rather than inventing a zero commission.
func TierForVolume(tiers []CommissionTier, monthlyVolumeUSD decimal.Decimal) (CommissionTier, bool) {
	var best CommissionTier
	found := false
	for _, t := range tiers {
		if t.MinMonthlyVolume.IsNegative() {
			continue // defensive: CHECK constraint guarantees >= 0
		}
		if monthlyVolumeUSD.GreaterThanOrEqual(t.MinMonthlyVolume) &&
			(!found || t.MinMonthlyVolume.GreaterThan(best.MinMonthlyVolume)) {
			best, found = t, true
		}
	}
	return best, found
}

// CommissionTierCharge computes the raw-model commission in the
// instrument's quote currency: per-lot leg plus per-million-notional leg.
// qty is in base units; lotSize is base units per standard lot
// (instruments.lot_size).
func CommissionTierCharge(t CommissionTier, qty, lotSize, notionalQuote decimal.Decimal) decimal.Decimal {
	perLot := decimal.Zero
	if lotSize.IsPositive() {
		perLot = qty.Div(lotSize).Mul(t.RatePerLot)
	}
	perMillion := notionalQuote.Div(decimal.NewFromInt(1_000_000)).Mul(t.RatePerMillion)
	return perLot.Add(perMillion).Round(8)
}

// EffectiveSpread returns 2×|fill − mid| — the MiFID II cost-disclosure
// measure of the spread actually paid through the fill price.
func EffectiveSpread(fillPrice, midPrice decimal.Decimal) decimal.Decimal {
	return fillPrice.Sub(midPrice).Abs().Mul(decimal.NewFromInt(2)).Round(8)
}

// ---------------------------------------------------------------------------
// Engine
// ---------------------------------------------------------------------------

// CommissionFill is one execution the engine assesses.
type CommissionFill struct {
	AccountID     int64
	TradeID       int64 // trades.id → journal reference_id
	InstrumentID  int64
	Symbol        string
	Role          FillRole
	Quantity      decimal.Decimal // base units filled
	Price         decimal.Decimal // quote per 1 base
	LotSize       decimal.Decimal // base units per standard lot (0 = skip per-lot leg)
	QuoteCurrency string          // commission denomination
	MidPrice      decimal.Decimal // mark mid at fill (0 = no effective_spread)
	MakerBps      decimal.Decimal // effective VIP maker rate; <0 = rebate (VIP 4+)
	VipTier       int             // accounts.vip_tier — gates negative maker rates
	PostedBy      string          // e.g. "commission-engine"
	Month         time.Time       // calendar month the fill accrues to (day truncated internally)
}

// CommissionAssessment is the engine output: the journals to post (nil when
// nothing charges) plus the execution-report disclosure fields.
type CommissionAssessment struct {
	AccountID          int64
	FeeModel           FeeModel
	Tier               CommissionTier // zero value for markup model
	TierResolved       bool
	CommissionCurrency string
	// Commission is the explicit charge in CommissionCurrency (>= 0; always
	// 0 for SPREAD_MARKUP — that fee is implicit in the spread).
	Commission decimal.Decimal
	// MakerRebate is the cash rebate credited for a negative maker rate
	// (>= 0; reported as a negative fee on the execution report).
	MakerRebate decimal.Decimal
	// EffectiveSpread is 2×|fill−mid| — MiFID II cost disclosure.
	EffectiveSpread decimal.Decimal
	// MonthlyVolumeUSD is the account's month-to-date notional BEFORE this
	// fill (tier-resolution input), in USD equivalent.
	MonthlyVolumeUSD decimal.Decimal
	// Month is the calendar-month anchor the fill accrues to; FillVolumeUSD
	// is this fill's USD-equivalent notional. The settlement path accrues
	// them via RecordFillVolumeUSD INSIDE the fill-commit transaction —
	// atomic with the ledger (Quote alone never writes).
	Month         time.Time
	FillVolumeUSD decimal.Decimal
	Journals      []ledger.Journal
}

// CommissionStore is the persistence seam; PgCommissionStore implements it.
type CommissionStore interface {
	// LoadCommissionTiers returns the commission_tiers ladder.
	LoadCommissionTiers(ctx context.Context) ([]CommissionTier, error)
	// MonthlyVolumeUSD returns the account's calendar-month notional in USD
	// equivalent for the month containing day.
	MonthlyVolumeUSD(ctx context.Context, accountID int64, month time.Time) (decimal.Decimal, error)
	// RecordFillVolumeUSD upserts +delta into account_monthly_volume for the
	// month containing day.
	RecordFillVolumeUSD(ctx context.Context, accountID int64, month time.Time, deltaUSD decimal.Decimal) error
}

// FeeModelSource resolves the account's pricing plan. The canonical source
// is account_product_profiles.pricing_plan joined via
// accounts.product_profile_id (migration 095 — Phase-14 Task 14.3.13);
// PgProfileFeeModelSource encodes that join.
type FeeModelSource interface {
	FeeModel(ctx context.Context, accountID int64) (FeeModel, error)
}

// CommissionEngine assesses per-fill commissions and maker rebates.
// conv is required only for RAW_SPREAD_COMMISSION accounts (monthly volume
// is tracked in USD equivalent); nil is acceptable for markup-only fleets.
type CommissionEngine struct {
	models FeeModelSource
	store  CommissionStore
	conv   UsdConverter // reuse of the Task 3.3.16 USD conversion seam
	clock  func() time.Time
}

// NewCommissionEngine wires the engine.
func NewCommissionEngine(models FeeModelSource, store CommissionStore, conv UsdConverter, clock func() time.Time) (*CommissionEngine, error) {
	if models == nil || store == nil {
		return nil, fmt.Errorf("commission engine: nil model source or store")
	}
	if clock == nil {
		clock = time.Now
	}
	return &CommissionEngine{models: models, store: store, conv: conv, clock: clock}, nil
}

// monthStart normalises t to the UTC calendar-month anchor (day 1).
func monthStart(t time.Time) time.Time {
	u := t.UTC()
	return time.Date(u.Year(), u.Month(), 1, 0, 0, 0, 0, time.UTC)
}

// Assess evaluates one fill and returns the assessment plus the GL journal
// set to post (commission charge journal and/or maker-rebate journal). The
// caller posts via LedgerService.Post — each journal is independently
// balanced and carries its own wallet effect.
//
// Fail-closed (spec §2.7): unknown fee model, missing tier for a raw
// account, missing USD conversion, or a negative maker rate below VIP 4 all
// abort with a coded error — a fill is never silently charged the wrong fee.
//
// Assess = Quote + immediate RecordFillVolumeUSD on the store — correct for
// standalone callers. The settlement path calls Quote (no write) and
// accrues the volume inside the fill-commit tx (pgxBalanceTx.AccrueVolume)
// so replayed/aborted commits never double-count monthly volume.
func (e *CommissionEngine) Assess(ctx context.Context, f CommissionFill) (*CommissionAssessment, error) {
	a, err := e.Quote(ctx, f)
	if err != nil {
		return nil, err
	}
	if a.FillVolumeUSD.IsPositive() {
		if err := e.store.RecordFillVolumeUSD(ctx, f.AccountID, a.Month, a.FillVolumeUSD); err != nil {
			return nil, excerrors.Wrap(CodeCommissionEngineInternal,
				fmt.Sprintf("record monthly volume acct %d", f.AccountID), err)
		}
	}
	return a, nil
}

// Quote computes the assessment without any writes — the pure half of
// Assess for callers embedding the volume accrual in their own tx.
func (e *CommissionEngine) Quote(ctx context.Context, f CommissionFill) (*CommissionAssessment, error) {
	if f.AccountID <= 0 {
		return nil, excerrors.New(CodeCommissionConfigInvalid, "commission fill requires a positive account id")
	}
	if !f.Quantity.IsPositive() || !f.Price.IsPositive() {
		return nil, excerrors.New(CodeCommissionConfigInvalid,
			"commission fill requires positive quantity and price")
	}
	f.QuoteCurrency = strings.ToUpper(strings.TrimSpace(f.QuoteCurrency))
	if len(f.QuoteCurrency) != 3 {
		return nil, excerrors.New(CodeCommissionConfigInvalid,
			fmt.Sprintf("commission fill has bad quote currency %q", f.QuoteCurrency))
	}
	if f.Role != FillRoleMaker && f.Role != FillRoleTaker {
		return nil, excerrors.New(CodeCommissionConfigInvalid,
			fmt.Sprintf("commission fill role %q not in {MAKER,TAKER}", f.Role))
	}
	if f.MakerBps.IsNegative() && f.VipTier < VipMakerRebateMinTier {
		return nil, excerrors.New(CodeCommissionConfigInvalid, fmt.Sprintf(
			"negative maker_bps %s on VIP %d account %d — rebates start at VIP %d (Task 3.3.17)",
			f.MakerBps, f.VipTier, f.AccountID, VipMakerRebateMinTier))
	}

	model, err := e.models.FeeModel(ctx, f.AccountID)
	if err != nil {
		return nil, excerrors.Wrap(CodeCommissionEngineInternal,
			fmt.Sprintf("resolve fee model acct %d", f.AccountID), err)
	}
	if !model.valid() {
		return nil, excerrors.New(CodeCommissionConfigInvalid,
			fmt.Sprintf("account %d fee model %q not in {SPREAD_MARKUP,RAW_SPREAD_COMMISSION}", f.AccountID, model))
	}

	a := &CommissionAssessment{
		AccountID:          f.AccountID,
		FeeModel:           model,
		CommissionCurrency: f.QuoteCurrency,
		Commission:         decimal.Zero,
		MakerRebate:        decimal.Zero,
	}
	if f.MidPrice.IsPositive() {
		a.EffectiveSpread = EffectiveSpread(f.Price, f.MidPrice)
	}

	notionalQuote := f.Quantity.Mul(f.Price)

	// ── Explicit commission (raw model only) ──────────────────────────────
	if model == FeeModelRawSpreadCommission {
		if e.conv == nil {
			return nil, excerrors.New(CodeCommissionConfigInvalid,
				"RAW_SPREAD_COMMISSION requires a USD converter for monthly volume tracking")
		}
		month := f.Month
		if month.IsZero() {
			month = e.clock()
		}
		month = monthStart(month)
		a.Month = month
		volUSD, err := e.store.MonthlyVolumeUSD(ctx, f.AccountID, month)
		if err != nil {
			return nil, excerrors.Wrap(CodeCommissionEngineInternal,
				fmt.Sprintf("monthly volume acct %d", f.AccountID), err)
		}
		a.MonthlyVolumeUSD = volUSD

		tiers, err := e.store.LoadCommissionTiers(ctx)
		if err != nil {
			return nil, excerrors.Wrap(CodeCommissionEngineInternal, "load commission_tiers", err)
		}
		tier, ok := TierForVolume(tiers, volUSD)
		if !ok {
			return nil, excerrors.New(CodeCommissionConfigInvalid,
				fmt.Sprintf("no commission tier for volume %s — tier 0 bound must be 0", volUSD))
		}
		a.Tier, a.TierResolved = tier, true
		a.Commission = CommissionTierCharge(tier, f.Quantity, f.LotSize, notionalQuote)

		// Fill volume is accrued AFTER tier resolution — a boundary
		// crossing takes effect on the next fill, not mid-fill. The write
		// itself is the caller's (Assess records now; Quote callers accrue
		// a.Month/a.FillVolumeUSD inside their own commit).
		fillUSD, err := e.conv.ToUSD(ctx, f.QuoteCurrency, notionalQuote)
		if err != nil {
			return nil, excerrors.Wrap(CodeCommissionEngineInternal,
				fmt.Sprintf("usd conversion %s", f.QuoteCurrency), err)
		}
		a.FillVolumeUSD = fillUSD

		if a.Commission.IsPositive() {
			a.Journals = append(a.Journals, CommissionJournal(f, a.Commission, tier))
		}
	}

	// ── Negative maker rate → cash rebate (Task 3.3.17) ──────────────────
	if f.Role == FillRoleMaker && f.MakerBps.IsNegative() {
		a.MakerRebate = f.MakerBps.Abs().Mul(notionalQuote).
			Div(decimal.NewFromInt(10_000)).Round(8)
		if a.MakerRebate.IsPositive() {
			a.Journals = append(a.Journals, MakerRebateJournal(f, a.MakerRebate))
		}
	}

	return a, nil
}

// CommissionJournal builds the balanced charge journal for a raw-model
// commission: client liability debit against commission revenue — a
// DISTINCT revenue line from 4010_TRADING_FEE_REVENUE spread income
// (Task 3.3.13 AC).
func CommissionJournal(f CommissionFill, charge decimal.Decimal, tier CommissionTier) ledger.Journal {
	narrative := fmt.Sprintf("COMMISSION trade=%d tier=%s per_lot=%s per_million=%s",
		f.TradeID, tier.TierName, tier.RatePerLot.String(), tier.RatePerMillion.String())
	return ledger.Journal{
		EntryType:      ledger.EntryFee,
		ReferenceID:    f.TradeID,
		Description:    narrative,
		PostedBy:       f.PostedBy,
		IdempotencyKey: fmt.Sprintf("commission:%d:%d", f.TradeID, f.AccountID),
		Lines: []ledger.Line{
			ledger.DebitLine(ledger.CustomerLiability(f.QuoteCurrency), f.QuoteCurrency, charge, narrative),
			ledger.CreditLine(ledger.CommissionRevenue(f.QuoteCurrency), f.QuoteCurrency, charge, narrative),
		},
		Effects: []ledger.AccountEffect{{
			AccountID:      f.AccountID,
			Currency:       f.QuoteCurrency,
			AvailableDelta: charge.Neg(),
		}},
	}
}

// MakerRebateJournal builds the balanced rebate journal (Task 3.3.17):
// debit the 5100 liquidity-rebate expense, credit the client's cash
// liability account (2100_CLIENT_COLLATERAL — the seeded "2100-CLIENT-FUNDS"
// collateral code), and credit the wallet.
func MakerRebateJournal(f CommissionFill, rebate decimal.Decimal) ledger.Journal {
	narrative := fmt.Sprintf("MAKER_REBATE trade=%d vip_tier=%d maker_bps=%s",
		f.TradeID, f.VipTier, f.MakerBps.String())
	return ledger.Journal{
		EntryType:      ledger.EntryFee,
		ReferenceID:    f.TradeID,
		Description:    narrative,
		PostedBy:       f.PostedBy,
		IdempotencyKey: fmt.Sprintf("makerrebate:%d:%d", f.TradeID, f.AccountID),
		Lines: []ledger.Line{
			ledger.DebitLine(ledger.LiquidityRebateExpense(f.QuoteCurrency), f.QuoteCurrency, rebate, narrative),
			ledger.CreditLine(ledger.ClientCollateral(f.QuoteCurrency), f.QuoteCurrency, rebate, narrative),
		},
		Effects: []ledger.AccountEffect{{
			AccountID:      f.AccountID,
			Currency:       f.QuoteCurrency,
			AvailableDelta: rebate, // credit client cash
		}},
	}
}

// ExecutionReportView is the disclosure projection for execution reports
// and monthly statements (Task 3.3.13 step 7 / Task 3.3.17 step 4 —
// MiFID II cost transparency). SignedFee is the net client-visible fee:
// positive = charged, negative = rebate received.
func (a *CommissionAssessment) ExecutionReportView() CommissionReportView {
	return CommissionReportView{
		Commission:       a.Commission,
		MakerRebate:      a.MakerRebate,
		SignedFee:        a.Commission.Sub(a.MakerRebate),
		EffectiveSpread:  a.EffectiveSpread,
		Currency:         a.CommissionCurrency,
		FeeModel:         a.FeeModel,
		CommissionTierID: a.Tier.TierID,
		MonthlyVolumeUSD: a.MonthlyVolumeUSD,
	}
}

// CommissionReportView carries the commission + effective_spread fields the
// execution report must surface.
type CommissionReportView struct {
	Commission       decimal.Decimal `json:"commission"`
	MakerRebate      decimal.Decimal `json:"maker_rebate"`
	SignedFee        decimal.Decimal `json:"signed_fee"` // commission − rebate (negative = net rebate)
	EffectiveSpread  decimal.Decimal `json:"effective_spread"`
	Currency         string          `json:"currency"`
	FeeModel         FeeModel        `json:"fee_model"`
	CommissionTierID int64           `json:"commission_tier_id"`
	MonthlyVolumeUSD decimal.Decimal `json:"monthly_volume_usd"`
}

// ---------------------------------------------------------------------------
// Pg implementations
// ---------------------------------------------------------------------------

// PgCommissionStore implements CommissionStore over pgx. Numerics cross
// the wire as text (same convention as PgVipStore).
type PgCommissionStore struct {
	pool *pgxpool.Pool
}

// NewPgCommissionStore wires the store.
func NewPgCommissionStore(pool *pgxpool.Pool) *PgCommissionStore {
	return &PgCommissionStore{pool: pool}
}

// LoadCommissionTiers reads the full ladder.
func (s *PgCommissionStore) LoadCommissionTiers(ctx context.Context) ([]CommissionTier, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT tier_id, tier_name, min_monthly_volume::text,
		       rate_per_lot::text, rate_per_million::text
		FROM commission_tiers ORDER BY min_monthly_volume`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CommissionTier
	for rows.Next() {
		var t CommissionTier
		var mv, rl, rm string
		if err := rows.Scan(&t.TierID, &t.TierName, &mv, &rl, &rm); err != nil {
			return nil, err
		}
		var err error
		if t.MinMonthlyVolume, err = decimal.NewFromString(mv); err != nil {
			return nil, fmt.Errorf("tier %d min_monthly_volume: %w", t.TierID, err)
		}
		if t.RatePerLot, err = decimal.NewFromString(rl); err != nil {
			return nil, fmt.Errorf("tier %d rate_per_lot: %w", t.TierID, err)
		}
		if t.RatePerMillion, err = decimal.NewFromString(rm); err != nil {
			return nil, fmt.Errorf("tier %d rate_per_million: %w", t.TierID, err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// MonthlyVolumeUSD reads the account's calendar-month aggregate.
func (s *PgCommissionStore) MonthlyVolumeUSD(ctx context.Context, accountID int64, month time.Time) (decimal.Decimal, error) {
	var txt *string
	err := s.pool.QueryRow(ctx, `
		SELECT volume_usd::text FROM account_monthly_volume
		WHERE account_id = $1 AND month = $2`,
		accountID, monthStart(month)).Scan(&txt)
	if err != nil {
		if err == pgx.ErrNoRows {
			return decimal.Zero, nil
		}
		return decimal.Zero, err
	}
	if txt == nil {
		return decimal.Zero, nil
	}
	d, err := decimal.NewFromString(*txt)
	if err != nil {
		return decimal.Zero, fmt.Errorf("parse monthly volume %q: %w", *txt, err)
	}
	return d, nil
}

// recordFillVolumeSQL is the account_monthly_volume upsert — shared by
// PgCommissionStore (standalone Assess) and pgxBalanceTx.AccrueVolume
// (in-tx accrual inside the settlement commit).
const recordFillVolumeSQL = `
	INSERT INTO account_monthly_volume (account_id, month, volume_usd, fill_count)
	VALUES ($1, $2, $3::numeric, 1)
	ON CONFLICT (account_id, month) DO UPDATE SET
	    volume_usd = account_monthly_volume.volume_usd + EXCLUDED.volume_usd,
	    fill_count = account_monthly_volume.fill_count + 1,
	    updated_at = now()`

// RecordFillVolumeUSD upserts the fill's USD notional into the month's row.
func (s *PgCommissionStore) RecordFillVolumeUSD(ctx context.Context, accountID int64, month time.Time, deltaUSD decimal.Decimal) error {
	_, err := s.pool.Exec(ctx, recordFillVolumeSQL,
		accountID, monthStart(month), deltaUSD.String())
	return err
}

// PgProfileFeeModelSource resolves FeeModel through the product profile —
// the canonical §5.41 source. DEPENDENCY: activates when migration 095
// (account_product_profiles + accounts.product_profile_id, Phase-14 Task
// 14.3.13) is applied; against a pre-095 schema the query fails loudly
// (fail-closed — never guess a fee model).
type PgProfileFeeModelSource struct {
	pool *pgxpool.Pool
}

// NewPgProfileFeeModelSource wires the source.
func NewPgProfileFeeModelSource(pool *pgxpool.Pool) *PgProfileFeeModelSource {
	return &PgProfileFeeModelSource{pool: pool}
}

// FeeModel implements FeeModelSource.
func (s *PgProfileFeeModelSource) FeeModel(ctx context.Context, accountID int64) (FeeModel, error) {
	var plan string
	if err := s.pool.QueryRow(ctx, `
		SELECT COALESCE(p.pricing_plan::text, 'SPREAD_MARKUP')
		FROM accounts a
		LEFT JOIN account_product_profiles p ON p.profile_id = a.product_profile_id
		WHERE a.id = $1`, accountID).Scan(&plan); err != nil {
		return "", err
	}
	m := FeeModel(plan)
	if !m.valid() {
		return "", excerrors.New(CodeCommissionConfigInvalid,
			fmt.Sprintf("account %d pricing_plan %q invalid", accountID, plan))
	}
	return m, nil
}

// StaticFeeModelSource returns one model for every account — test fixture
// and pre-095 default.
type StaticFeeModelSource struct{ Model FeeModel }

// FeeModel implements FeeModelSource.
func (s StaticFeeModelSource) FeeModel(context.Context, int64) (FeeModel, error) {
	return s.Model, nil
}

// ConverterUsdAdapter adapts *position.Converter (mark mid-rates) to the
// UsdConverter seam for monthly-volume tracking.
type ConverterUsdAdapter struct{ Conv *position.Converter }

// ToUSD implements UsdConverter via the converter's direct/inverse/cross
// rate resolution — a missing path fails closed (PRICE_ORACLE_UNAVAILABLE).
func (a ConverterUsdAdapter) ToUSD(ctx context.Context, currency string, amount decimal.Decimal) (decimal.Decimal, error) {
	if a.Conv == nil {
		return decimal.Zero, fmt.Errorf("usd conversion %s: nil converter", currency)
	}
	if currency == position.PivotCurrency {
		return amount, nil
	}
	rate, _, err := a.Conv.Rate(ctx, currency, position.PivotCurrency)
	if err != nil {
		return decimal.Zero, err
	}
	return amount.Mul(rate), nil
}
