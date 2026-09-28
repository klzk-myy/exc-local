// fee_service.go — Trading Fee Calculation & GL Posting
// (Phase-03 Task 3.3.4; spec §5.11 fee_tiers, §8.5 fee structure, §24).
//
// fee = fill_qty × fill_price × fee_bps / 10_000 — decimal-exact
// (shopspring, never float64), rounded to the §5.3 8dp quantum, and
// denominated in the instrument's quote currency (qty×price is quote
// notional).
//
// Consumption contract — the balance service (Phase-03 Task 3.3.1, a
// parallel owner) calls into this package; nothing here touches balances
// directly beyond the GL journal the balance path may embed:
//
//   - ComputeFee / FeeTier.RateBps are pure functions for pricing,
//     dry-run previews and tests.
//   - Quote resolves the account's tier (accounts.fee_tier_id →
//     fee_tiers) and applies promo rates while promo_until > now()
//     (spec §8.5: promo overrides the tier rate; promo-derived rates are
//     never below zero — a negative promo clamps to a free fill).
//   - BuildFeeJournal returns the balanced ledger.Journal — a caller
//     inside its own SERIALIZABLE trade-settlement tx posts it via
//     LedgerService.PostJournal so the fee debit is atomic with the fill.
//   - ApplyFee posts standalone through the JournalPoster seam
//     (LedgerService.Post) with idempotency key
//     fee:{trade}:{account}:{role} — replayed fills re-resolve to the
//     committed journal, never double-charge.
//
// Journal shape (EntryType FEE, debit-credit mirrored per spec §5.21):
// a positive fee debits 2010_CUSTOMER_LIABILITY_{ccy} / credits
// 4010_TRADING_FEE_REVENUE_{ccy} and deducts available balance; a
// negative rate (maker rebate, §8.5) flips the pair and credits the
// account — the rebate is paid from fee revenue.
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

// Scaffold error codes — register in Phase-05 Task 5.3.21.
const (
	// CodeFeeTierNotFound: account has no resolvable fee_tiers row —
	// fail-closed (§2.7): a fill must never silently settle fee-free.
	CodeFeeTierNotFound = "FEE_TIER_NOT_FOUND"
	// CodeFeeInvalidInput: malformed fee request (bad role, non-positive
	// qty/price, bad currency).
	CodeFeeInvalidInput = "FEE_INVALID_INPUT"
)

// LiquidityRole distinguishes maker (resting order) from taker (incoming
// order) fills — the caller resolves the role per account leg; the
// trades row records order ids only.
type LiquidityRole string

const (
	RoleMaker LiquidityRole = "MAKER"
	RoleTaker LiquidityRole = "TAKER"
)

// FeeTier is one fee_tiers row (migration 012, spec §5.11).
type FeeTier struct {
	ID            int64
	TierName      string
	MakerBps      decimal.Decimal
	TakerBps      decimal.Decimal
	PromoUntil    *time.Time       // nil = no promo
	PromoMakerBps *decimal.Decimal // nil = promo does not cover maker side
	PromoTakerBps *decimal.Decimal
}

// RateBps returns the effective bps for role at `now`, plus whether a
// promo rate applied. Per spec §8.5 the promo overrides the tier rate
// strictly while promo_until > now(); a negative promo rate clamps to
// zero ("promos ... never below zero"). Tier-level negative maker rates
// (VIP rebate tiers) pass through — a negative result is a rebate.
func (t FeeTier) RateBps(role LiquidityRole, now time.Time) (rate decimal.Decimal, promo bool, err error) {
	var base decimal.Decimal
	var promoRate *decimal.Decimal
	switch role {
	case RoleMaker:
		base, promoRate = t.MakerBps, t.PromoMakerBps
	case RoleTaker:
		base, promoRate = t.TakerBps, t.PromoTakerBps
	default:
		return decimal.Zero, false, excerrors.New(CodeFeeInvalidInput,
			fmt.Sprintf("fee: invalid liquidity role %q", role))
	}
	if t.PromoUntil != nil && now.Before(*t.PromoUntil) && promoRate != nil {
		r := *promoRate
		if r.IsNegative() {
			r = decimal.Zero
		}
		return r, true, nil
	}
	return base, false, nil
}

// bpsDenom is the /10_000 of the spec formula.
var bpsDenom = decimal.NewFromInt(10_000)

// ComputeFee is the Task 3.3.4 formula: qty × price × bps / 10_000,
// rounded to the DECIMAL(28,8) quantum. Signed — a negative rateBps
// (rebate tier) yields a negative fee.
func ComputeFee(quantity, price, rateBps decimal.Decimal) decimal.Decimal {
	return quantity.Mul(price).Mul(rateBps).Div(bpsDenom).Round(8)
}

// ---------------------------------------------------------------------------
// Quotes
// ---------------------------------------------------------------------------

// FeeParams is the ApplyFee/Quote input.
type FeeParams struct {
	TradeID   int64 // 0 allowed for pure Quote dry-runs
	AccountID int64
	Role      LiquidityRole // MAKER = resting order, TAKER = incoming
	Quantity  decimal.Decimal
	Price     decimal.Decimal
	// FeeCurrency is the denomination — the instrument's quote currency
	// for per-side billing. Must be a 3-letter uppercase ISO code.
	FeeCurrency string
	// MinFee floors a positive fee (spec §8.5 min_fee_usd, default 0);
	// the caller resolves any per-instrument config — pass decimal.Zero
	// for none. Never applied to rebates.
	MinFee decimal.Decimal
}

// FeeQuote is a fully resolved fee for one account leg of a trade.
type FeeQuote struct {
	AccountID    int64
	Role         LiquidityRole
	Currency     string
	Notional     decimal.Decimal // qty × price (fee currency)
	RateBps      decimal.Decimal // effective rate after promo resolution
	Amount       decimal.Decimal // signed: +fee, -rebate, 0 free
	PromoApplied bool
	TierID       int64
	TierName     string
}

// IsRebate reports whether the quote is a credit to the account.
func (q FeeQuote) IsRebate() bool { return q.Amount.IsNegative() }

// FeeStore resolves the account's fee tier; PgxFeeStore implements it.
type FeeStore interface {
	// FeeTierFor returns the account's tier; found=false when the account
	// is missing or has no fee_tier_id (fail-closed upstream).
	FeeTierFor(ctx context.Context, accountID int64) (tier FeeTier, found bool, err error)
}

// JournalPoster is the posting seam — *LedgerService satisfies it.
// Callers embedding the fee inside their own trade-settlement tx should
// instead use BuildFeeJournal + LedgerService.PostJournal.
type JournalPoster interface {
	Post(ctx context.Context, j ledger.Journal) (ledger.PostResult, error)
}

// FeeService resolves tiers, computes fees and posts the GL journal.
type FeeService struct {
	store    FeeStore
	poster   JournalPoster // nil → Quote/BuildFeeJournal only
	postedBy string
	clock    func() time.Time
}

// NewFeeService wires the service. store is required; poster may be nil
// (compute-only construction — ApplyFee then refuses); postedBy defaults
// to "settlement-service"; clock may be nil.
func NewFeeService(store FeeStore, poster JournalPoster, postedBy string, clock func() time.Time) (*FeeService, error) {
	if store == nil {
		return nil, fmt.Errorf("fee service: nil store")
	}
	if postedBy == "" {
		postedBy = "settlement-service"
	}
	if clock == nil {
		clock = time.Now
	}
	return &FeeService{store: store, poster: poster, postedBy: postedBy, clock: clock}, nil
}

// Quote resolves the tier and computes the fee — no writes.
func (s *FeeService) Quote(ctx context.Context, p FeeParams) (FeeQuote, error) {
	if p.AccountID <= 0 {
		return FeeQuote{}, excerrors.New(CodeFeeInvalidInput,
			"fee: account id must be positive")
	}
	if !p.Quantity.IsPositive() || !p.Price.IsPositive() {
		return FeeQuote{}, excerrors.New(CodeFeeInvalidInput,
			"fee: quantity and price must be > 0")
	}
	if !currencyRe.MatchString(p.FeeCurrency) {
		return FeeQuote{}, excerrors.New(CodeFeeInvalidInput,
			fmt.Sprintf("fee: invalid currency %q", p.FeeCurrency))
	}
	tier, found, err := s.store.FeeTierFor(ctx, p.AccountID)
	if err != nil {
		return FeeQuote{}, fmt.Errorf("fee: resolve tier for account %d: %w", p.AccountID, err)
	}
	if !found {
		return FeeQuote{}, excerrors.New(CodeFeeTierNotFound, fmt.Sprintf(
			"account %d has no fee_tier_id — cannot price the fill", p.AccountID))
	}
	rate, promo, err := tier.RateBps(p.Role, s.clock().UTC())
	if err != nil {
		return FeeQuote{}, err
	}
	q := FeeQuote{
		AccountID:    p.AccountID,
		Role:         p.Role,
		Currency:     p.FeeCurrency,
		Notional:     p.Quantity.Mul(p.Price).Round(8),
		RateBps:      rate,
		Amount:       ComputeFee(p.Quantity, p.Price, rate),
		PromoApplied: promo,
		TierID:       tier.ID,
		TierName:     tier.TierName,
	}
	if q.Amount.IsPositive() && p.MinFee.IsPositive() && q.Amount.LessThan(p.MinFee) {
		q.Amount = p.MinFee.Round(8) // §8.5 minimum fee — positive fees only
	}
	return q, nil
}

// ---------------------------------------------------------------------------
// Journal construction + posting
// ---------------------------------------------------------------------------

// BuildFeeJournal renders the balanced GL journal for a quote. Positive
// fee: debit customer liability / credit trading-fee revenue, effect
// deducts available. Negative fee (rebate): the pair flips and the
// account is credited from fee revenue. Zero fees produce no journal —
// an error here, a no-op upstream in ApplyFee.
func BuildFeeJournal(q FeeQuote, tradeID int64, postedBy string) (ledger.Journal, error) {
	if q.Amount.IsZero() {
		return ledger.Journal{}, excerrors.New(CodeFeeInvalidInput,
			"fee journal: zero fee — nothing to post")
	}
	if q.AccountID <= 0 || tradeID <= 0 {
		return ledger.Journal{}, excerrors.New(CodeFeeInvalidInput,
			"fee journal: account and trade ids must be positive")
	}
	if !currencyRe.MatchString(q.Currency) {
		return ledger.Journal{}, excerrors.New(CodeFeeInvalidInput,
			fmt.Sprintf("fee journal: invalid currency %q", q.Currency))
	}
	abs := q.Amount.Abs()
	narrative := fmt.Sprintf("TRADE_FEE trade=%d acct=%d role=%s tier=%s(%d) bps=%s promo=%t",
		tradeID, q.AccountID, q.Role, q.TierName, q.TierID, q.RateBps.String(), q.PromoApplied)

	j := ledger.Journal{
		EntryType:      ledger.EntryFee,
		ReferenceID:    tradeID,
		Description:    narrative,
		PostedBy:       postedBy,
		IdempotencyKey: FeeIdempotencyKey(tradeID, q.AccountID, q.Role),
	}
	if q.IsRebate() {
		// Maker rebate — paid from fee revenue (spec §8.5).
		j.Lines = []ledger.Line{
			ledger.DebitLine(ledger.TradingFeeRevenue(q.Currency), q.Currency, abs, narrative),
			ledger.CreditLine(ledger.CustomerLiability(q.Currency), q.Currency, abs, narrative),
		}
		j.Effects = []ledger.AccountEffect{{
			AccountID:      q.AccountID,
			Currency:       q.Currency,
			AvailableDelta: abs, // credit the account
		}}
	} else {
		j.Lines = []ledger.Line{
			ledger.DebitLine(ledger.CustomerLiability(q.Currency), q.Currency, abs, narrative),
			ledger.CreditLine(ledger.TradingFeeRevenue(q.Currency), q.Currency, abs, narrative),
		}
		j.Effects = []ledger.AccountEffect{{
			AccountID:      q.AccountID,
			Currency:       q.Currency,
			AvailableDelta: abs.Neg(), // deduct the fee
		}}
	}
	return j, nil
}

// FeeIdempotencyKey is the journal_entries.idempotency_key used for fee
// postings — replayed fills resolve to the committed original.
func FeeIdempotencyKey(tradeID, accountID int64, role LiquidityRole) string {
	return fmt.Sprintf("fee:%d:%d:%s", tradeID, accountID, role)
}

// ApplyFee resolves the tier, computes the fee and posts the journal
// through the JournalPoster seam (its own SERIALIZABLE tx — atomic in
// itself; for atomicity WITH the fill, embed BuildFeeJournal +
// PostJournal in the balance tx). A zero fee is a no-op: applied=false,
// no journal.
func (s *FeeService) ApplyFee(ctx context.Context, p FeeParams) (q FeeQuote, res ledger.PostResult, applied bool, err error) {
	q, err = s.Quote(ctx, p)
	if err != nil {
		return q, res, false, err
	}
	if q.Amount.IsZero() {
		return q, res, false, nil
	}
	if p.TradeID <= 0 {
		return q, res, false, excerrors.New(CodeFeeInvalidInput,
			"fee: ApplyFee requires a positive trade id")
	}
	if s.poster == nil {
		return q, res, false, excerrors.New(CodeFeeInvalidInput,
			"fee: no JournalPoster wired — compute-only service")
	}
	j, err := BuildFeeJournal(q, p.TradeID, s.postedBy)
	if err != nil {
		return q, res, false, err
	}
	res, err = s.poster.Post(ctx, j)
	if err != nil {
		return q, res, false, fmt.Errorf("fee: post trade %d account %d: %w", p.TradeID, p.AccountID, err)
	}
	return q, res, true, nil
}

// ---------------------------------------------------------------------------
// PgxFeeStore — PostgreSQL implementation
// ---------------------------------------------------------------------------

// PgxFeeStore resolves fee tiers over pgx.
type PgxFeeStore struct {
	Pool *pgxpool.Pool
}

// NewPgxFeeStore wires the store.
func NewPgxFeeStore(pool *pgxpool.Pool) *PgxFeeStore {
	return &PgxFeeStore{Pool: pool}
}

// FeeTierFor joins accounts.fee_tier_id → fee_tiers. NULL fee_tier_id (or
// a missing account) yields found=false — fail-closed upstream.
func (s *PgxFeeStore) FeeTierFor(ctx context.Context, accountID int64) (FeeTier, bool, error) {
	var (
		t        FeeTier
		mk, tk   string
		pmk, ptk *string
		pu       *time.Time
	)
	err := s.Pool.QueryRow(ctx, `
		SELECT ft.id, ft.tier_name, ft.maker_bps::text, ft.taker_bps::text,
		       ft.promo_until, ft.promo_maker_bps::text, ft.promo_taker_bps::text
		  FROM accounts a
		  JOIN fee_tiers ft ON ft.id = a.fee_tier_id
		 WHERE a.id = $1`, accountID).
		Scan(&t.ID, &t.TierName, &mk, &tk, &pu, &pmk, &ptk)
	if stderrors.Is(err, pgx.ErrNoRows) {
		return FeeTier{}, false, nil
	}
	if err != nil {
		return FeeTier{}, false, err
	}
	if t.MakerBps, err = decimal.NewFromString(mk); err != nil {
		return FeeTier{}, false, fmt.Errorf("tier %d maker_bps %q: %w", t.ID, mk, err)
	}
	if t.TakerBps, err = decimal.NewFromString(tk); err != nil {
		return FeeTier{}, false, fmt.Errorf("tier %d taker_bps %q: %w", t.ID, tk, err)
	}
	if pmk != nil {
		v, err := decimal.NewFromString(*pmk)
		if err != nil {
			return FeeTier{}, false, fmt.Errorf("tier %d promo_maker_bps %q: %w", t.ID, *pmk, err)
		}
		t.PromoMakerBps = &v
	}
	if ptk != nil {
		v, err := decimal.NewFromString(*ptk)
		if err != nil {
			return FeeTier{}, false, fmt.Errorf("tier %d promo_taker_bps %q: %w", t.ID, *ptk, err)
		}
		t.PromoTakerBps = &v
	}
	t.PromoUntil = pu
	return t, true, nil
}
