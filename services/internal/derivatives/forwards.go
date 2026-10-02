// forwards.go — Phase-22 Task 22.3.1: FX outright forwards.
//
// A forward is an agreement to buy/sell the base currency at a fixed rate
// on a future value date (spec §15.1). Pricing is covered interest parity
// over the Phase-19.5 yield curves (curve.go, spec §15.3); physical
// delivery is expressed as dated settlement_instructions legs
// (migration 254) so the Phase-03 dispatch machinery delivers the two
// currency amounts on the maturity date unchanged (spec §6.3 — "Forward
// value date specified at order time").
//
// Order admission: the orders pipeline calls ValidateForwardOrder for
// instrument_type=FORWARD submissions (POST /api/v1/orders) — the
// §5.4 orders.value_date column is persisted by Task 22.3.9; this
// package owns the date/rate validation contract.
package derivatives

import (
	"context"
	"fmt"
	"time"

	"exchange/pkg/decimal"
	excerrors "exchange/pkg/errors"
)

// ForwardService prices, validates and books outright forwards.
type ForwardService struct {
	Dates  *Dates
	Pricer *Pricer
	Store  ContractStore
	now    func() time.Time
}

// NewForwardService wires the service.
func NewForwardService(d *Dates, p *Pricer, s ContractStore) *ForwardService {
	return &ForwardService{Dates: d, Pricer: p, Store: s, now: time.Now}
}

// ForwardOrderRequest is the admission-time view of a FORWARD order
// (spec §6.3: "Forward value date specified at order time").
type ForwardOrderRequest struct {
	Pair            Pair
	Side            ContractSide
	Notional        decimal.Decimal // base currency
	ValueDate       time.Time       // required — explicit forward value date
	SettlementCycle int             // instrument's spot cycle (0/1/2)
	TradeDay        time.Time
}

// ValidateForwardOrder is the order-admission gate for FORWARD
// instruments: a positive notional, an explicit value date that is a
// mutual business day strictly after the spot value date. The gateway
// calls this before the order is admitted; rejection is coded.
func (s *ForwardService) ValidateForwardOrder(req ForwardOrderRequest) error {
	if _, err := NewPair(req.Pair.Base, req.Pair.Quote); err != nil {
		return err
	}
	if req.Side != SideBuy && req.Side != SideSell {
		return excerrors.New(CodeInvalidRequest, "forward side must be BUY|SELL")
	}
	if !req.Notional.IsPositive() {
		return excerrors.New(CodeInvalidRequest, "forward notional must be > 0")
	}
	if req.ValueDate.IsZero() {
		return excerrors.New(CodeInvalidRequest,
			"forward order requires an explicit value_date")
	}
	spot, err := s.Dates.SpotDate(req.Pair, req.TradeDay, req.SettlementCycle)
	if err != nil {
		return err
	}
	return s.Dates.CheckValueDate(req.Pair, spot, req.ValueDate)
}

// ForwardBookRequest carries the fill-derived booking inputs.
type ForwardBookRequest struct {
	ForwardOrderRequest
	TradeID        int64
	AccountID      int64
	InstrumentID   int64
	SpotRate       decimal.Decimal  // reference spot at booking (0 → oracle)
	AgreedRate     *decimal.Decimal // explicit fill rate (nil → CIP price)
	IdempotencyKey string           // e.g. "fwd:{trade_id}:{account_id}"
}

// BookForward books the contract and its physical-delivery legs in one
// SERIALIZABLE transaction. The forward rate is the agreed fill rate when
// supplied, else the CIP price off the published curves — both are stored
// (spot reference + forward rate + points) for the audit record. A fill
// replay returns the existing contract (idempotency-key dedup).
func (s *ForwardService) BookForward(ctx context.Context, req ForwardBookRequest) (*Contract, error) {
	if err := s.ValidateForwardOrder(req.ForwardOrderRequest); err != nil {
		return nil, err
	}
	if req.TradeID <= 0 || req.AccountID <= 0 || req.InstrumentID <= 0 {
		return nil, excerrors.New(CodeInvalidRequest,
			"forward booking requires trade/account/instrument ids")
	}
	spotDate, err := s.Dates.SpotDate(req.Pair, req.TradeDay, req.SettlementCycle)
	if err != nil {
		return nil, err
	}
	spot := req.SpotRate
	if !spot.IsPositive() {
		spot, err = s.Pricer.SpotRate(ctx, req.Pair)
		if err != nil {
			return nil, err
		}
	}
	var fwd decimal.Decimal
	if req.AgreedRate != nil {
		if !req.AgreedRate.IsPositive() {
			return nil, excerrors.New(CodeInvalidRequest,
				"agreed forward rate must be > 0")
		}
		if err := checkFairValue(ctx, s.Pricer, req.Pair, spot, spotDate,
			req.ValueDate, *req.AgreedRate); err != nil {
			return nil, err
		}
		fwd = *req.AgreedRate
	} else {
		q, err := s.Pricer.PriceForward(ctx, req.Pair, spot, spotDate, req.ValueDate)
		if err != nil {
			return nil, err
		}
		fwd = q.ForwardRate
	}

	c := &Contract{
		TradeID: req.TradeID, AccountID: req.AccountID,
		InstrumentID: req.InstrumentID, Kind: KindForward, Side: req.Side,
		Pair: req.Pair, Notional: req.Notional,
		SpotRate: spot, ForwardRate: fwd, SwapPoints: fwd.Sub(spot),
		SpotValueDate: spotDate, ValueDate: normDay(req.ValueDate),
		Status: StatusOpen, BookedAt: s.now().UTC(),
		IdempotencyKey: req.IdempotencyKey,
	}
	legs := s.deliveryLegs(c)

	var booked int64
	err = s.Store.InTx(ctx, func(ctx context.Context, tx ContractTx) error {
		id, created, err := tx.InsertContract(ctx, c)
		if err != nil {
			return err
		}
		if !created {
			booked = id
			return nil // fill replay — legs already persisted
		}
		for i := range legs {
			legs[i].ContractID = id
		}
		if _, err := tx.InsertLegs(ctx, legs); err != nil {
			return err
		}
		if err := ensureLegsMatch(ctx, tx, id, legs); err != nil {
			return err
		}
		booked = id
		return nil
	})
	if err != nil {
		return nil, err
	}
	c.ID = booked
	return c, nil
}

// deliveryLegs renders the physical-delivery obligation set at maturity:
// BUY base → RECEIVE base / PAY quote; SELL → PAY base / RECEIVE quote.
// Both legs carry the forward value date (spec §15.1 physical delivery).
func (s *ForwardService) deliveryLegs(c *Contract) []SettlementLeg {
	quoteAmount := c.Notional.Mul(c.ForwardRate).Round(8)
	baseLeg := SettlementLeg{
		TradeID: c.TradeID, AccountID: c.AccountID, Tag: LegTagFar,
		Currency: c.Pair.Base, Amount: c.Notional.Round(8),
		ValueDate: c.ValueDate,
	}
	quoteLeg := SettlementLeg{
		TradeID: c.TradeID, AccountID: c.AccountID, Tag: LegTagFar,
		Currency: c.Pair.Quote, Amount: quoteAmount,
		ValueDate: c.ValueDate,
	}
	if c.Side == SideBuy {
		baseLeg.Direction, quoteLeg.Direction = LegReceive, LegPay
	} else {
		baseLeg.Direction, quoteLeg.Direction = LegPay, LegReceive
	}
	return []SettlementLeg{baseLeg, quoteLeg}
}

// RollupSettlement refreshes a contract's status from its linked
// settlement legs: every leg SETTLED/RECONCILED → contract SETTLED; some
// settled → PARTIALLY_SETTLED. Ran by the maturity sweep and after
// correspondent confirmations land (Phase-03 ConfirmSettlement writes
// the leg status; this package owns the rollup). The payment dispatch
// itself is the existing settlement_instructions pipeline — no second
// executor here.
func RollupSettlement(ctx context.Context, store ContractStore, contractID int64, at time.Time) (ContractStatus, error) {
	var status ContractStatus
	err := store.InTx(ctx, func(ctx context.Context, tx ContractTx) error {
		c, err := tx.ContractForUpdate(ctx, contractID)
		if err != nil {
			return err
		}
		if c.Status == StatusSettled || c.Status == StatusCancelled || c.Status == StatusFailed {
			status = c.Status
			return nil // terminal — rollup is a no-op
		}
		legs, err := tx.ContractLegs(ctx, contractID)
		if err != nil {
			return err
		}
		status = rollupStatus(c, legs, at)
		if status != c.Status {
			var settledAt *time.Time
			if status == StatusSettled {
				t := at.UTC()
				settledAt = &t
			}
			return tx.UpdateContractStatus(ctx, contractID, status, settledAt)
		}
		return nil
	})
	return status, err
}

// rollupStatus computes the contract status from its legs.
func rollupStatus(c *Contract, legs []PersistedLeg, _ time.Time) ContractStatus {
	if len(legs) == 0 {
		return c.Status
	}
	settled, failed := 0, 0
	for _, l := range legs {
		switch l.Status {
		case "SETTLED", "RECONCILED":
			settled++
		case "FAILED":
			failed++
		}
	}
	switch {
	case failed > 0:
		return StatusFailed
	case settled == len(legs):
		return StatusSettled
	case settled > 0 && c.Kind == KindSwap:
		return StatusPartiallySettled
	}
	return c.Status
}

// DueSettlements lists OPEN/PARTIALLY_SETTLED dated contracts whose value
// or near-leg date has arrived — the sweep's input for rollup plus NDF
// settle checks.
func DueSettlements(ctx context.Context, store ContractStore, asOf time.Time, limit int) ([]Contract, error) {
	var out []Contract
	err := store.InTx(ctx, func(ctx context.Context, tx ContractTx) error {
		cs, err := tx.DueContracts(ctx, normDay(asOf), limit)
		if err != nil {
			return err
		}
		out = cs
		return nil
	})
	return out, err
}

// checkFairValue gates an explicit agreed rate against the CIP fair
// value (spec §27.1 MTF/Fair-Value row): |agreed − fair| / fair beyond
// FairValueDivergenceBps → FAIR_VALUE_DIVERGENCE (503, L1). Pricer
// failures propagate — booking an unchecked negotiated rate against a
// broken curve is fail-closed per §2.7.
func checkFairValue(ctx context.Context, p *Pricer, pair Pair,
	spot decimal.Decimal, spotDate, valueDate time.Time,
	agreed decimal.Decimal) error {
	q, err := p.PriceForward(ctx, pair, spot, spotDate, valueDate)
	if err != nil {
		return err
	}
	divergenceBps := agreed.Sub(q.ForwardRate).Abs().
		Div(q.ForwardRate).Mul(decimal.NewFromInt(10000))
	if divergenceBps.GreaterThan(decimal.NewFromInt(FairValueDivergenceBps)) {
		return excerrors.New(CodeFairValueDivergence, fmt.Sprintf(
			"agreed rate %s diverges %s bps from CIP fair value %s (band %d bps)",
			agreed.String(), divergenceBps.Round(1).String(),
			q.ForwardRate.String(), FairValueDivergenceBps))
	}
	return nil
}
