// auto_exchange.go — Multi-Asset Collateral Auto-Exchange Deficit
// Settlement (Phase-03 Task 3.3.14; spec §13.6/§13.6b, §24 #284 in the
// Phase-03 AC table, journal/line contract §5.21).
//
// When a single currency runs a deficit (negative balance) while the
// account holds excess collateral in other currencies, the engine clears
// the deficit internally — no external banking rail is touched:
//
//  1. Consume an AutoExchangeTrigger produced by the Phase-19 Task 19.3.8
//     collateral concentration-limit monitor (the trigger carries the
//     account, deficit currency/magnitude and the post-haircut collateral
//     snapshot the monitor already computed — this engine does NOT
//     re-derive haircuts).
//  2. Pick the collateral currency with the highest post-haircut excess
//     equity, valued in the deficit currency at the Index Price.
//  3. Convert the deficit plus a 0.1% buffer at the live Index Price.
//  4. Post one balanced double-entry journal through the LedgerService:
//     per-currency legs across 2100_CLIENT_COLLATERAL_{ccy} (client side)
//     and 1200_MULTI_CURRENCY_CLEARING_{ccy} (house clearing side), with
//     wallet effects moving available balances (source −, deficit +).
//
// Fail-closed (spec §2.7): a non-positive index rate, an empty collateral
// snapshot, or collateral insufficient to cover deficit + buffer all abort
// with coded errors — a partial or guessed conversion never posts.
package settlement

import (
	"context"
	"fmt"
	"strings"

	"github.com/shopspring/decimal"

	"exchange/internal/ledger"
	"exchange/internal/position"
	excerrors "exchange/pkg/errors"
)

// Scaffold error codes — register in the Phase-05 Task 5.3.21 registry.
const (
	// CodeAutoExchangeInvalid — malformed trigger (non-positive deficit,
	// duplicate/self collateral leg, empty collateral set).
	CodeAutoExchangeInvalid = "AUTO_EXCHANGE_INVALID"
	// CodeAutoExchangeInsufficient — no collateral currency can cover the
	// deficit at index price + buffer within its free balance.
	CodeAutoExchangeInsufficient = "AUTO_EXCHANGE_INSUFFICIENT_COLLATERAL"
	// CodeIndexPriceUnavailable — the index-pricing seam failed or returned
	// a non-positive rate (fail-closed, §2.7).
	CodeIndexPriceUnavailable = "INDEX_PRICE_UNAVAILABLE"
)

// AutoExchangeBufferBps is the canonical 0.1% conversion buffer over the
// deficit (Task 3.3.14 step 3: "Index Price + 0.1% buffer").
var AutoExchangeBufferBps = decimal.NewFromInt(10) // 10 bps = 0.1%

// IndexPricer resolves the live index rate between two currencies:
// rate = units of quote per 1 unit of base. The Phase-19.5 mark/index
// oracle owns the source; ConverterIndexPricer adapts the Task 3.3.9
// position.Converter (direct, inverse, USD-cross resolution).
type IndexPricer interface {
	IndexRate(ctx context.Context, base, quote string) (decimal.Decimal, error)
}

// ConverterIndexPricer adapts *position.Converter to IndexPricer — the
// mark mid-rate stands in for the index price until the Phase-19.5 oracle
// feed lands; the seam is identical (positive mark mid per unit).
type ConverterIndexPricer struct{ Conv *position.Converter }

// IndexRate implements IndexPricer via the converter's direct/inverse/
// USD-cross resolution (fail-closed: unresolved path → error).
func (p ConverterIndexPricer) IndexRate(ctx context.Context, base, quote string) (decimal.Decimal, error) {
	if p.Conv == nil {
		return decimal.Zero, excerrors.New(CodeIndexPriceUnavailable, "nil index pricer")
	}
	r, _, err := p.Conv.Rate(ctx, base, quote)
	return r, err
}

// CollateralSlice is one currency's collateral snapshot supplied by the
// Phase-19 concentration monitor: the free balance and the §13.6b haircut
// already assigned to that currency for this account.
type CollateralSlice struct {
	Currency   string
	Free       decimal.Decimal // available (unencumbered) balance
	HaircutBps decimal.Decimal // §13.6b haircut, basis points
}

// PostHaircutValue returns Free × (1 − haircut/10⁴) — the equity the
// slice contributes after haircut, in its own currency.
func (c CollateralSlice) PostHaircutValue() decimal.Decimal {
	return c.Free.Mul(decimal.NewFromInt(10_000).Sub(c.HaircutBps)).
		Div(decimal.NewFromInt(10_000))
}

// AutoExchangeTrigger is the input seam owned by the Phase-19 Task 19.3.8
// collateral concentration monitor — one trigger per deficit detected.
type AutoExchangeTrigger struct {
	AccountID       int64
	DeficitCurrency string          // currency in deficit
	DeficitAmount   decimal.Decimal // positive magnitude to clear
	// Collateral is the monitor's current collateral snapshot: candidate
	// funding currencies with free balance and assigned haircut. The
	// deficit currency itself is ignored if present.
	Collateral  []CollateralSlice
	Reason      string // e.g. "concentration-limit" — audit narrative
	ReferenceID int64  // monitor event id → journal reference + idempotency
	PostedBy    string // e.g. "collateral-monitor"
}

// AutoExchangeResult reports one settled conversion.
type AutoExchangeResult struct {
	SourceCurrency  string
	SourceAmount    decimal.Decimal // debited from source balance
	DeficitCurrency string
	DeficitCredited decimal.Decimal // credited to deficit balance (= deficit + 0.1% buffer, rounded)
	IndexRate       decimal.Decimal // deficit-ccy per 1 source unit
	BufferBps       decimal.Decimal
	JournalID       int64
	Replayed        bool // idempotency replay — no new posting
	Post            ledger.PostResult
}

// AutoExchangeEngine settles single-currency deficits via internal
// collateral conversion.
type AutoExchangeEngine struct {
	poster JournalPoster
	pricer IndexPricer
}

// JournalPoster is declared in fee_service.go — *LedgerService satisfies
// it; the seam keeps this engine testable without Redis/NATS plumbing.

// NewAutoExchangeEngine wires the engine; both deps are required.
func NewAutoExchangeEngine(poster JournalPoster, pricer IndexPricer) (*AutoExchangeEngine, error) {
	if poster == nil || pricer == nil {
		return nil, fmt.Errorf("auto-exchange engine: nil poster or pricer")
	}
	return &AutoExchangeEngine{poster: poster, pricer: pricer}, nil
}

// Settle consumes one trigger: picks the collateral currency, sizes the
// conversion at index + 0.1% buffer, and posts the balanced GL journal.
// Idempotent per trigger: IdempotencyKey = autoex:{account}:{reference}.
func (e *AutoExchangeEngine) Settle(ctx context.Context, tr AutoExchangeTrigger) (*AutoExchangeResult, error) {
	tr.DeficitCurrency = strings.ToUpper(strings.TrimSpace(tr.DeficitCurrency))
	if tr.AccountID <= 0 {
		return nil, excerrors.New(CodeAutoExchangeInvalid, "trigger requires a positive account id")
	}
	if len(tr.DeficitCurrency) != 3 {
		return nil, excerrors.New(CodeAutoExchangeInvalid,
			fmt.Sprintf("trigger deficit currency %q invalid", tr.DeficitCurrency))
	}
	if !tr.DeficitAmount.IsPositive() {
		return nil, excerrors.New(CodeAutoExchangeInvalid,
			"trigger deficit amount must be a positive magnitude")
	}
	if len(tr.Collateral) == 0 {
		return nil, excerrors.New(CodeAutoExchangeInvalid,
			"trigger carries no collateral snapshot")
	}

	// Target credit = deficit × (1 + buffer) in the deficit currency.
	target := tr.DeficitAmount.
		Mul(decimal.NewFromInt(10_000).Add(AutoExchangeBufferBps)).
		Div(decimal.NewFromInt(10_000)).
		RoundCeil(8)

	// ── Candidate selection: rank every collateral currency by post-haircut
	//    excess equity expressed in the DEFICIT currency at the index rate;
	//    the first candidate that can cover the source debit wins.
	type candidate struct {
		ccy        string
		free       decimal.Decimal // raw free balance (spend limit)
		rate       decimal.Decimal // deficit-ccy per 1 unit of ccy
		excessEq   decimal.Decimal // post-haircut free, deficit-ccy terms
		sourceNeed decimal.Decimal // source units to fund target
	}
	var cands []candidate
	var lastErr error
	for _, c := range tr.Collateral {
		ccy := strings.ToUpper(c.Currency)
		if ccy == tr.DeficitCurrency || !c.Free.IsPositive() {
			continue
		}
		if c.HaircutBps.IsNegative() || c.HaircutBps.GreaterThan(decimal.NewFromInt(10_000)) {
			return nil, excerrors.New(CodeAutoExchangeInvalid, fmt.Sprintf(
				"collateral %s haircut %s bps outside [0,10000]", ccy, c.HaircutBps))
		}
		rate, err := e.pricer.IndexRate(ctx, ccy, tr.DeficitCurrency)
		if err != nil {
			// A currency we cannot price can never be chosen — record and
			// continue; all-fail lands in the insufficient error below.
			lastErr = err
			continue
		}
		if !rate.IsPositive() {
			return nil, excerrors.New(CodeIndexPriceUnavailable, fmt.Sprintf(
				"non-positive index rate %s→%s: %s", ccy, tr.DeficitCurrency, rate))
		}
		cands = append(cands, candidate{
			ccy:        ccy,
			free:       c.Free,
			rate:       rate,
			excessEq:   c.PostHaircutValue().Mul(rate),
			sourceNeed: target.Div(rate).RoundCeil(8),
		})
	}
	if len(cands) == 0 {
		if lastErr != nil {
			return nil, excerrors.Wrap(CodeIndexPriceUnavailable,
				"no priceable collateral currency", lastErr)
		}
		return nil, excerrors.New(CodeAutoExchangeInsufficient,
			"trigger snapshot holds no usable collateral currency")
	}
	// Highest post-haircut excess first (Task 3.3.14 step 2).
	for i := 0; i < len(cands); i++ {
		for j := i + 1; j < len(cands); j++ {
			if cands[j].excessEq.GreaterThan(cands[i].excessEq) {
				cands[i], cands[j] = cands[j], cands[i]
			}
		}
	}
	var pick *candidate
	for i := range cands {
		if cands[i].sourceNeed.LessThanOrEqual(cands[i].free) {
			pick = &cands[i]
			break
		}
	}
	if pick == nil {
		return nil, excerrors.New(CodeAutoExchangeInsufficient, fmt.Sprintf(
			"no collateral currency covers deficit %s %s + %s bps buffer at index",
			tr.DeficitAmount, tr.DeficitCurrency, AutoExchangeBufferBps))
	}

	narrative := fmt.Sprintf("AUTO_EXCHANGE acct=%d deficit=%s %s src=%s rate=%s buf_bps=%s reason=%s",
		tr.AccountID, tr.DeficitAmount, tr.DeficitCurrency, pick.ccy,
		pick.rate.String(), AutoExchangeBufferBps, tr.Reason)

	j := ledger.Journal{
		EntryType:      ledger.EntryTransfer,
		ReferenceID:    tr.ReferenceID,
		Description:    narrative,
		PostedBy:       tr.PostedBy,
		IdempotencyKey: fmt.Sprintf("autoex:%d:%d", tr.AccountID, tr.ReferenceID),
		Lines: []ledger.Line{
			// Source leg: collateral liability down, clearing credit.
			ledger.DebitLine(ledger.ClientCollateral(pick.ccy), pick.ccy, pick.sourceNeed, narrative+" leg=source"),
			ledger.CreditLine(ledger.MultiCcyClearing(pick.ccy), pick.ccy, pick.sourceNeed, narrative+" leg=source"),
			// Deficit leg: clearing debit, collateral liability restored.
			ledger.DebitLine(ledger.MultiCcyClearing(tr.DeficitCurrency), tr.DeficitCurrency, target, narrative+" leg=deficit"),
			ledger.CreditLine(ledger.ClientCollateral(tr.DeficitCurrency), tr.DeficitCurrency, target, narrative+" leg=deficit"),
		},
		Effects: []ledger.AccountEffect{
			{AccountID: tr.AccountID, Currency: pick.ccy, AvailableDelta: pick.sourceNeed.Neg()},
			{AccountID: tr.AccountID, Currency: tr.DeficitCurrency, AvailableDelta: target, AllowNegative: true},
		},
	}
	if err := j.Validate(); err != nil {
		return nil, err
	}
	res, err := e.poster.Post(ctx, j)
	if err != nil {
		return nil, err
	}
	return &AutoExchangeResult{
		SourceCurrency:  pick.ccy,
		SourceAmount:    pick.sourceNeed,
		DeficitCurrency: tr.DeficitCurrency,
		DeficitCredited: target,
		IndexRate:       pick.rate,
		BufferBps:       AutoExchangeBufferBps,
		JournalID:       res.JournalID,
		Replayed:        res.Replayed,
		Post:            res,
	}, nil
}
