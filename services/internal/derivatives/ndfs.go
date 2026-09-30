// ndfs.go — Phase-22 Task 22.3.3: non-deliverable forwards.
//
// An NDF is a forward on a restricted (non-deliverable) currency settled
// in cash against an official fixing instead of physical delivery
// (spec §15.1, §6.3 "Cash-settled NDFs settle at fixing date"). At the
// fixing date the contract nets to a single settlement-currency payment:
//
//	settle in quote ccy:  amount = N_base × (F − K)
//	settle in base  ccy:  amount = N_base × (F − K) / F
//
// where K is the contract rate, F the published fixing, N_base the base
// notional; a positive amount is the base-buyer's gain. (The Phase-22
// plan's "× days/base" fragment is a DCC mis-transcription — no accrual
// factor exists in a fixing-date cash settle; the rate ratio is the
// standard base-vs-quote conversion. §27 records design deviations.)
//
// Fixing sources follow the §15.7 hierarchy — central-bank fixing →
// Reuters/Bloomberg page → prior-day hold (flagged). The fixing source is
// the spec §5.4 `ndf_fixing_source` field (free-form vendor/page strings,
// validated against the canonical tier vocabulary below). A missing
// fixing at settle time fails closed with BENCHMARK_UNAVAILABLE.
package derivatives

import (
	"context"
	"fmt"
	"strings"
	"time"

	"exchange/pkg/decimal"
	excerrors "exchange/pkg/errors"
)

// Ndf fixing-source vocabulary (spec §15.7 hierarchy, §5.4
// ndf_fixing_source): a tier token optionally suffixed with the
// publisher page, e.g. "CENTRAL_BANK:BCB", "REUTERS:BRLFIX",
// "BLOOMBERG:BFIX". PRIOR_DAY_HOLD marks the last-resort tier and sets
// the row's prior_day_hold flag.
const (
	NdfSourceCentralBank = "CENTRAL_BANK"
	NdfSourceReuters     = "REUTERS"
	NdfSourceBloomberg   = "BLOOMBERG"
	NdfSourcePriorDay    = "PRIOR_DAY_HOLD"
)

// ValidateNdfSource enforces the fixing-source vocabulary — an
// unrecognized source is INVALID_REQUEST (the audit trail must be able
// to reconstruct which tier published the rate).
func ValidateNdfSource(source string) error {
	tok, _, _ := strings.Cut(strings.ToUpper(strings.TrimSpace(source)), ":")
	switch tok {
	case NdfSourceCentralBank, NdfSourceReuters, NdfSourceBloomberg, NdfSourcePriorDay:
		return nil
	}
	return excerrors.New(CodeInvalidRequest, fmt.Sprintf(
		"ndf fixing source %q not in the §15.7 hierarchy (CENTRAL_BANK|REUTERS|BLOOMBERG|PRIOR_DAY_HOLD[:page])",
		source))
}

// DefaultRestrictedCurrencies are non-deliverable currencies eligible for
// NDF instruments (spec Task 22.3.3 step 4). Instrument reference data
// may extend it per listing — this is the admission fallback.
var DefaultRestrictedCurrencies = map[string]bool{
	"CNY": true, "INR": true, "BRL": true, "KRW": true, "TWD": true,
	"IDR": true, "PHP": true, "MYR": true, "VND": true, "ARS": true,
	"COP": true, "CLP": true, "PEN": true, "EGP": true, "PKR": true,
}

// NdfService validates, books, fixes and settles NDFs.
type NdfService struct {
	Dates      *Dates
	Pricer     *Pricer
	Store      ContractStore
	Restricted map[string]bool // nil → DefaultRestrictedCurrencies
	now        func() time.Time
}

// NewNdfService wires the service.
func NewNdfService(d *Dates, p *Pricer, s ContractStore) *NdfService {
	return &NdfService{Dates: d, Pricer: p, Store: s, now: time.Now}
}

func (s *NdfService) restricted(ccy string) bool {
	set := s.Restricted
	if set == nil {
		set = DefaultRestrictedCurrencies
	}
	return set[strings.ToUpper(ccy)]
}

// NdfOrderRequest is the admission-time view of an NDF order.
type NdfOrderRequest struct {
	Pair               Pair
	Side               ContractSide
	Notional           decimal.Decimal // base currency
	ValueDate          time.Time       // requested fixing/settlement date
	SettlementCurrency string          // deliverable currency of the cash leg
	FixingSource       string          // ndf_fixing_source vocabulary
	SettlementCycle    int
	TradeDay           time.Time
}

// ValidateNdfOrder is the NDF admission gate: the pair must contain a
// restricted (non-deliverable) currency, the settlement currency must be
// the deliverable side, the fixing source must be a valid §15.7 tier,
// and the value date must be a future mutual business day strictly after
// the spot date (same forward-dating rule as outrights).
func (s *NdfService) ValidateNdfOrder(req NdfOrderRequest) (fixingDate time.Time, err error) {
	p, err := NewPair(req.Pair.Base, req.Pair.Quote)
	if err != nil {
		return time.Time{}, err
	}
	if req.Side != SideBuy && req.Side != SideSell {
		return time.Time{}, excerrors.New(CodeInvalidRequest, "ndf side must be BUY|SELL")
	}
	if !req.Notional.IsPositive() {
		return time.Time{}, excerrors.New(CodeInvalidRequest, "ndf notional must be > 0")
	}
	if req.ValueDate.IsZero() {
		return time.Time{}, excerrors.New(CodeInvalidRequest,
			"ndf order requires an explicit value_date (fixing date)")
	}
	if err := ValidateNdfSource(req.FixingSource); err != nil {
		return time.Time{}, err
	}
	var restrictedCcy, deliverable string
	switch {
	case s.restricted(p.Base) && !s.restricted(p.Quote):
		restrictedCcy, deliverable = p.Base, p.Quote
	case s.restricted(p.Quote) && !s.restricted(p.Base):
		restrictedCcy, deliverable = p.Quote, p.Base
	default:
		return time.Time{}, excerrors.New(CodeInvalidRequest, fmt.Sprintf(
			"ndf pair %s must contain exactly one restricted currency", p.Symbol()))
	}
	settle := strings.ToUpper(strings.TrimSpace(req.SettlementCurrency))
	if settle == "" {
		settle = deliverable
	}
	if settle != deliverable {
		return time.Time{}, excerrors.New(CodeInvalidRequest, fmt.Sprintf(
			"ndf %s settlement currency %q must be the deliverable side %q (restricted leg %s is non-deliverable)",
			p.Symbol(), settle, deliverable, restrictedCcy))
	}
	spot, err := s.Dates.SpotDate(p, req.TradeDay, req.SettlementCycle)
	if err != nil {
		return time.Time{}, err
	}
	if err := s.Dates.CheckValueDate(p, spot, req.ValueDate); err != nil {
		return time.Time{}, err
	}
	// Holiday-landed fixings roll to the next good business day (§7.4
	// item 2) — the stored fixing date is the rolled date.
	return s.Dates.RollNdfFixing(p, req.ValueDate)
}

// NdfBookRequest carries the fill-derived booking inputs.
type NdfBookRequest struct {
	NdfOrderRequest
	TradeID        int64
	AccountID      int64
	InstrumentID   int64
	SpotRate       decimal.Decimal
	AgreedRate     *decimal.Decimal // explicit contract rate (nil → CIP price)
	IdempotencyKey string           // e.g. "ndf:{trade_id}:{account_id}"
}

// BookNdf books the NDF contract (SERIALIZABLE). No settlement legs are
// written at booking — the cash amount only exists once the fixing lands;
// SettleNdf writes the single net leg at that point.
func (s *NdfService) BookNdf(ctx context.Context, req NdfBookRequest) (*Contract, error) {
	fixingDate, err := s.ValidateNdfOrder(req.NdfOrderRequest)
	if err != nil {
		return nil, err
	}
	if req.TradeID <= 0 || req.AccountID <= 0 || req.InstrumentID <= 0 {
		return nil, excerrors.New(CodeInvalidRequest,
			"ndf booking requires trade/account/instrument ids")
	}
	spot := req.SpotRate
	if !spot.IsPositive() {
		spot, err = s.Pricer.SpotRate(ctx, req.Pair)
		if err != nil {
			return nil, err
		}
	}
	var k decimal.Decimal
	if req.AgreedRate != nil {
		if !req.AgreedRate.IsPositive() {
			return nil, excerrors.New(CodeInvalidRequest, "agreed ndf rate must be > 0")
		}
		k = *req.AgreedRate
	} else {
		spotDate, err := s.Dates.SpotDate(req.Pair, req.TradeDay, req.SettlementCycle)
		if err != nil {
			return nil, err
		}
		q, err := s.Pricer.PriceForward(ctx, req.Pair, spot, spotDate, fixingDate)
		if err != nil {
			return nil, err
		}
		k = q.ForwardRate
	}
	settleCcy := strings.ToUpper(strings.TrimSpace(req.SettlementCurrency))
	if settleCcy == "" {
		if s.restricted(req.Pair.Base) {
			settleCcy = req.Pair.Quote
		} else {
			settleCcy = req.Pair.Base
		}
	}
	c := &Contract{
		TradeID: req.TradeID, AccountID: req.AccountID,
		InstrumentID: req.InstrumentID, Kind: KindNDF, Side: req.Side,
		Pair: req.Pair, Notional: req.Notional,
		SpotRate: spot, ForwardRate: k, SwapPoints: k.Sub(spot),
		ValueDate:          fixingDate,
		NdfFixingDate:      &fixingDate,
		NdfFixingSource:    req.FixingSource,
		SettlementCurrency: settleCcy,
		Status:             StatusOpen, BookedAt: s.now().UTC(),
		IdempotencyKey: req.IdempotencyKey,
	}
	err = s.Store.InTx(ctx, func(ctx context.Context, tx ContractTx) error {
		id, _, err := tx.InsertContract(ctx, c)
		if err != nil {
			return err
		}
		c.ID = id
		return nil
	})
	if err != nil {
		return nil, err
	}
	return c, nil
}

// ApplyFixing records the published fixing for a contract's fixing date.
// rate must be positive; source must be a recognized §15.7 tier. The
// ndf_fixings row plus the contract's fixing_rate update are atomic.
// A repeated observation for the same (contract, date) is idempotent.
func (s *NdfService) ApplyFixing(ctx context.Context, contractID int64,
	rate decimal.Decimal, source string, fixingDate time.Time) error {
	if !rate.IsPositive() {
		return excerrors.New(CodeInvalidRequest, "ndf fixing rate must be > 0")
	}
	if err := ValidateNdfSource(source); err != nil {
		return err
	}
	priorHold := strings.HasPrefix(strings.ToUpper(source), NdfSourcePriorDay)
	return s.Store.InTx(ctx, func(ctx context.Context, tx ContractTx) error {
		c, err := tx.ContractForUpdate(ctx, contractID)
		if err != nil {
			return err
		}
		if c.Kind != KindNDF {
			return excerrors.New(CodeInvalidRequest,
				fmt.Sprintf("contract %d is %s, not NDF", contractID, c.Kind))
		}
		if c.Status != StatusOpen {
			return excerrors.New(CodeDerivativeStateConflict, fmt.Sprintf(
				"ndf %d fixing applied on %s contract", contractID, c.Status))
		}
		f := &NdfFixing{
			ContractID: contractID, FixingDate: normDay(fixingDate),
			Source: source, Rate: rate, PriorDayHold: priorHold,
			ObservedAt: s.now().UTC(),
		}
		if _, err := tx.InsertNdfFixing(ctx, f); err != nil {
			return err
		}
		return tx.SetContractFixing(ctx, contractID, rate)
	})
}

// NdfSettlement is the cash-settlement result.
type NdfSettlement struct {
	ContractID         int64
	FixingRate         decimal.Decimal
	ContractRate       decimal.Decimal
	SettlementCurrency string
	// Amount is signed in the settlement currency: positive credits the
	// contract holder, negative debits.
	Amount    decimal.Decimal
	Direction LegDirection
	ValueDate time.Time
	Replayed  bool // settle replayed an already-SETTLED contract
}

// SettlementAmount is the NDF cash-settle math (spec §15.1): the holder's
// P&L on N_base contracted at rate K versus fixing F, expressed in the
// settlement currency. The base BUYER gains when F > K:
//
//	settle = quote → N·(F−K)     settle = base → N·(F−K)/F
//
// and the base SELLER's result is the mirror image (side inverts the
// sign). A positive amount credits the contract holder, negative debits.
func SettlementAmount(side ContractSide, notionalBase, contractRate, fixingRate decimal.Decimal,
	settleCcy string, p Pair) (decimal.Decimal, error) {
	if side != SideBuy && side != SideSell {
		return decimal.Zero, excerrors.New(CodeInvalidRequest,
			"ndf settlement requires a BUY|SELL side")
	}
	if !notionalBase.IsPositive() || !contractRate.IsPositive() || !fixingRate.IsPositive() {
		return decimal.Zero, excerrors.New(CodeInvalidRequest,
			"ndf settlement requires positive notional and rates")
	}
	diff := fixingRate.Sub(contractRate)
	if side == SideSell {
		diff = diff.Neg()
	}
	switch settleCcy {
	case p.Quote:
		return notionalBase.Mul(diff).Round(8), nil
	case p.Base:
		return notionalBase.Mul(diff).Div(fixingRate).Round(8), nil
	}
	return decimal.Zero, excerrors.New(CodeInvalidRequest, fmt.Sprintf(
		"ndf settlement currency %q must be base %s or quote %s", settleCcy, p.Base, p.Quote))
}

// SettleNdf crystallizes the cash leg at fixing: one settlement
// instruction in the settlement currency, dated on the fixing date.
// A missing fixing rate fails closed with BENCHMARK_UNAVAILABLE —
// settling without the reference rate would fabricate the payment
// (spec §15.6/§15.7). Replaying a settled contract returns the stored
// outcome (idempotent).
func (s *NdfService) SettleNdf(ctx context.Context, contractID int64) (*NdfSettlement, error) {
	var res *NdfSettlement
	err := s.Store.InTx(ctx, func(ctx context.Context, tx ContractTx) error {
		c, err := tx.ContractForUpdate(ctx, contractID)
		if err != nil {
			return err
		}
		if c.Kind != KindNDF {
			return excerrors.New(CodeInvalidRequest,
				fmt.Sprintf("contract %d is %s, not NDF", contractID, c.Kind))
		}
		if c.Status == StatusSettled && c.FixingRate != nil && c.SettlementAmount != nil {
			res = &NdfSettlement{
				ContractID: contractID, FixingRate: *c.FixingRate,
				ContractRate: c.ForwardRate, SettlementCurrency: c.SettlementCurrency,
				Amount: *c.SettlementAmount, ValueDate: c.ValueDate, Replayed: true,
			}
			if c.SettlementAmount.IsNegative() {
				res.Direction = LegPay
			} else {
				res.Direction = LegReceive
			}
			return nil
		}
		if c.Status != StatusOpen {
			return excerrors.New(CodeDerivativeStateConflict, fmt.Sprintf(
				"ndf %d not settleable in status %s", contractID, c.Status))
		}
		if c.FixingRate == nil {
			return excerrors.New(CodeBenchmarkUnavailable, fmt.Sprintf(
				"ndf %d: no fixing observed for %s (%s) — settlement halted",
				contractID, c.Pair.Symbol(), c.NdfFixingSource))
		}
		amount, err := SettlementAmount(c.Side, c.Notional, c.ForwardRate, *c.FixingRate,
			c.SettlementCurrency, c.Pair)
		if err != nil {
			return err
		}
		dir := LegReceive
		abs := amount
		if amount.IsNegative() {
			dir, abs = LegPay, amount.Neg()
		}
		if abs.IsPositive() {
			leg := SettlementLeg{
				ContractID: contractID, TradeID: c.TradeID, AccountID: c.AccountID,
				Tag: LegTagFixing, Currency: c.SettlementCurrency, Amount: abs,
				Direction: dir, ValueDate: c.ValueDate,
			}
			if _, err := tx.InsertLegs(ctx, []SettlementLeg{leg}); err != nil {
				return err
			}
			if err := ensureLegsMatch(ctx, tx, contractID, []SettlementLeg{leg}); err != nil {
				return err
			}
		}
		if err := tx.SetContractOutcome(ctx, contractID, *c.FixingRate, amount,
			StatusSettled, s.now().UTC()); err != nil {
			return err
		}
		res = &NdfSettlement{
			ContractID: contractID, FixingRate: *c.FixingRate,
			ContractRate: c.ForwardRate, SettlementCurrency: c.SettlementCurrency,
			Amount: amount, Direction: dir, ValueDate: c.ValueDate,
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return res, nil
}
