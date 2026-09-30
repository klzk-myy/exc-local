// swaps.go — Phase-22 Task 22.3.2: FX swaps (near leg + far leg).
//
// An FX swap is the simultaneous purchase and sale of identical base
// amounts on two different value dates: the near leg settles at the spot
// date at the spot rate, the far leg at a later value date at the agreed
// forward rate (spec §15.1). The price difference is the swap points:
//
//	swap_points = far_rate − near_rate   (spec Task 22.3.2 step 4)
//
// with far_rate priced by covered interest parity off the near date
// (curve.go). Equal base notionals in opposite directions mean the
// structure carries no net spot exposure — enforced at admission and at
// booking (spec §6.3 "Swap legs — near leg + far leg with respective
// value dates"; both legs settle on their own dates via
// settlement_instructions, migration 254).
package derivatives

import (
	"context"
	"fmt"
	"time"

	"exchange/pkg/decimal"
	excerrors "exchange/pkg/errors"
)

// SwapService prices, validates and books FX swaps.
type SwapService struct {
	Dates  *Dates
	Pricer *Pricer
	Store  ContractStore
	now    func() time.Time
}

// NewSwapService wires the service.
func NewSwapService(d *Dates, p *Pricer, s ContractStore) *SwapService {
	return &SwapService{Dates: d, Pricer: p, Store: s, now: time.Now}
}

// SwapQuote is a priced FX swap: near leg at the spot rate/date, far leg
// at the CIP forward. Points = far − near.
type SwapQuote struct {
	Pair          Pair
	NearRate      decimal.Decimal // spot reference
	NearValueDate time.Time
	FarRate       decimal.Decimal
	FarValueDate  time.Time
	SwapPoints    decimal.Decimal // FarRate − NearRate
	Days          int             // near → far accrual span? no — spot → far term days
	PricedAt      time.Time
}

// SwapOrderRequest is the admission-time view of a SWAP order: the far
// leg value date is required; the near leg defaults to the pair's spot
// value date (standard spot-start swap). An explicit near date supports
// forward-start swaps (e.g. Tom-Next rolls).
type SwapOrderRequest struct {
	Pair            Pair
	Side            ContractSide // near-leg direction on the base ccy (far leg inverts)
	Notional        decimal.Decimal
	NearValueDate   *time.Time       // nil → spot value date
	FarValueDate    time.Time        // required
	FarNotional     *decimal.Decimal // nil → equal to near (no net spot exposure)
	SettlementCycle int
	TradeDay        time.Time
}

// ValidateSwapOrder is the SWAP order-admission gate: positive notional,
// far leg strictly after near leg, both legs on mutual business days, and
// — when an explicit far notional is supplied — equal base notionals so
// the pair carries no net spot exposure.
func (s *SwapService) ValidateSwapOrder(req SwapOrderRequest) (near time.Time, err error) {
	if _, err := NewPair(req.Pair.Base, req.Pair.Quote); err != nil {
		return time.Time{}, err
	}
	if req.Side != SideBuy && req.Side != SideSell {
		return time.Time{}, excerrors.New(CodeInvalidRequest, "swap side must be BUY|SELL")
	}
	if !req.Notional.IsPositive() {
		return time.Time{}, excerrors.New(CodeInvalidRequest, "swap notional must be > 0")
	}
	if req.FarValueDate.IsZero() {
		return time.Time{}, excerrors.New(CodeInvalidRequest,
			"swap order requires an explicit far_leg_value_date")
	}
	if req.FarNotional != nil {
		if !req.FarNotional.IsPositive() {
			return time.Time{}, excerrors.New(CodeInvalidRequest,
				"swap far notional must be > 0")
		}
		if !req.FarNotional.Equal(req.Notional) {
			// Uneven (amortizing) swaps carry residual spot exposure —
			// the venue's swap product is matched notional only (spec
			// §6.3 swap legs; §15.1 "simultaneous spot + forward").
			return time.Time{}, excerrors.New(CodeInvalidRequest, fmt.Sprintf(
				"swap notional mismatch: near %s vs far %s — unequal base legs leave net spot exposure",
				req.Notional, *req.FarNotional))
		}
	}
	near = normDay(req.TradeDay)
	if req.NearValueDate != nil {
		near = normDay(*req.NearValueDate)
	} else {
		near, err = s.Dates.SpotDate(req.Pair, req.TradeDay, req.SettlementCycle)
		if err != nil {
			return time.Time{}, err
		}
	}
	if err := s.Dates.CheckSwapDates(req.Pair, near, req.FarValueDate); err != nil {
		return time.Time{}, err
	}
	return near, nil
}

// PriceSwap prices the far leg by CIP from the near leg and returns the
// full quote — points = far − near (spec Task 22.3.2 step 4).
func (s *SwapService) PriceSwap(ctx context.Context, req SwapOrderRequest, spot decimal.Decimal) (*SwapQuote, error) {
	near, err := s.ValidateSwapOrder(req)
	if err != nil {
		return nil, err
	}
	if !spot.IsPositive() {
		spot, err = s.Pricer.SpotRate(ctx, req.Pair)
		if err != nil {
			return nil, err
		}
	}
	// The far outright is priced over the spot→far term: d is the
	// calendar-day span between the pair's spot value date and the far
	// value date (the money-market accrual convention, spec §15.3).
	spotDate, err := s.Dates.SpotDate(req.Pair, req.TradeDay, req.SettlementCycle)
	if err != nil {
		return nil, err
	}
	q, err := s.Pricer.PriceForward(ctx, req.Pair, spot, spotDate, req.FarValueDate)
	if err != nil {
		return nil, err
	}
	return &SwapQuote{
		Pair: req.Pair, NearRate: spot, NearValueDate: near,
		FarRate: q.ForwardRate, FarValueDate: q.ValueDate,
		SwapPoints: q.SwapPoints, Days: q.Days, PricedAt: q.PricedAt,
	}, nil
}

// SwapBookRequest carries the fill-derived booking inputs.
type SwapBookRequest struct {
	SwapOrderRequest
	TradeID        int64
	AccountID      int64
	InstrumentID   int64
	SpotRate       decimal.Decimal  // near-leg rate (0 → oracle)
	AgreedFarRate  *decimal.Decimal // explicit fill rate (nil → CIP price)
	IdempotencyKey string           // e.g. "swp:{trade_id}:{account_id}"
}

// BookSwap books the contract with all four settlement legs in one
// SERIALIZABLE transaction: near PAY/RECEIVE at the near date, far
// PAY/RECEIVE at the far date. Base notional is identical on both legs —
// the net spot exposure is zero by construction and re-verified against
// the persisted leg set.
func (s *SwapService) BookSwap(ctx context.Context, req SwapBookRequest) (*Contract, *SwapQuote, error) {
	near, err := s.ValidateSwapOrder(req.SwapOrderRequest)
	if err != nil {
		return nil, nil, err
	}
	if req.TradeID <= 0 || req.AccountID <= 0 || req.InstrumentID <= 0 {
		return nil, nil, excerrors.New(CodeInvalidRequest,
			"swap booking requires trade/account/instrument ids")
	}
	spot := req.SpotRate
	if !spot.IsPositive() {
		spot, err = s.Pricer.SpotRate(ctx, req.Pair)
		if err != nil {
			return nil, nil, err
		}
	}
	var far decimal.Decimal
	if req.AgreedFarRate != nil {
		if !req.AgreedFarRate.IsPositive() {
			return nil, nil, excerrors.New(CodeInvalidRequest,
				"agreed far rate must be > 0")
		}
		far = *req.AgreedFarRate
	} else {
		spotDate, err := s.Dates.SpotDate(req.Pair, req.TradeDay, req.SettlementCycle)
		if err != nil {
			return nil, nil, err
		}
		q, err := s.Pricer.PriceForward(ctx, req.Pair, spot, spotDate, req.FarValueDate)
		if err != nil {
			return nil, nil, err
		}
		far = q.ForwardRate
	}

	c := &Contract{
		TradeID: req.TradeID, AccountID: req.AccountID,
		InstrumentID: req.InstrumentID, Kind: KindSwap, Side: req.Side,
		Pair: req.Pair, Notional: req.Notional,
		SpotRate: spot, ForwardRate: far, SwapPoints: far.Sub(spot),
		SpotValueDate:    near, // spot date under the pair's cycle
		ValueDate:        normDay(req.FarValueDate),
		NearLegValueDate: &near,
		Status:           StatusOpen, BookedAt: s.now().UTC(),
		IdempotencyKey: req.IdempotencyKey,
	}
	legs := s.legs(c)
	if err := assertZeroSpotExposure(legs, c.Pair); err != nil {
		return nil, nil, err
	}

	quote := &SwapQuote{
		Pair: req.Pair, NearRate: spot, NearValueDate: near,
		FarRate: far, FarValueDate: c.ValueDate, SwapPoints: c.SwapPoints,
		Days: int(c.ValueDate.Sub(near).Hours() / 24), PricedAt: s.now().UTC(),
	}

	var booked int64
	err = s.Store.InTx(ctx, func(ctx context.Context, tx ContractTx) error {
		id, created, err := tx.InsertContract(ctx, c)
		if err != nil {
			return err
		}
		if !created {
			booked = id
			return nil
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
		return nil, nil, err
	}
	c.ID = booked
	return c, quote, nil
}

// legs renders the four obligations: near leg exchanges base against
// quote at the near (spot) rate; the far leg inverts both directions at
// the far rate. Net base exposure across the pair of legs is zero.
func (s *SwapService) legs(c *Contract) []SettlementLeg {
	nearQuote := c.Notional.Mul(c.SpotRate).Round(8)
	farQuote := c.Notional.Mul(c.ForwardRate).Round(8)
	mk := func(tag LegTag, ccy string, amt decimal.Decimal, dir LegDirection, d time.Time) SettlementLeg {
		return SettlementLeg{
			TradeID: c.TradeID, AccountID: c.AccountID, Tag: tag,
			Currency: ccy, Amount: amt, Direction: dir, ValueDate: d,
		}
	}
	near := *c.NearLegValueDate
	if c.Side == SideBuy {
		// buy base near / sell base far.
		return []SettlementLeg{
			mk(LegTagNear, c.Pair.Base, c.Notional.Round(8), LegReceive, near),
			mk(LegTagNear, c.Pair.Quote, nearQuote, LegPay, near),
			mk(LegTagFar, c.Pair.Base, c.Notional.Round(8), LegPay, c.ValueDate),
			mk(LegTagFar, c.Pair.Quote, farQuote, LegReceive, c.ValueDate),
		}
	}
	return []SettlementLeg{
		mk(LegTagNear, c.Pair.Base, c.Notional.Round(8), LegPay, near),
		mk(LegTagNear, c.Pair.Quote, nearQuote, LegReceive, near),
		mk(LegTagFar, c.Pair.Base, c.Notional.Round(8), LegReceive, c.ValueDate),
		mk(LegTagFar, c.Pair.Quote, farQuote, LegPay, c.ValueDate),
	}
}

// assertZeroSpotExposure proves the leg set nets to zero base flow across
// near+far in each currency direction — the swap's defining invariant.
// A nonzero net is a coded rejection (structural defect or unequal legs).
func assertZeroSpotExposure(legs []SettlementLeg, pair Pair) error {
	net := decimal.Zero
	for _, l := range legs {
		if l.Currency != pair.Base {
			continue
		}
		if l.Direction == LegReceive {
			net = net.Add(l.Amount)
		} else {
			net = net.Sub(l.Amount)
		}
	}
	if !net.IsZero() {
		return excerrors.New(CodeInvalidRequest, fmt.Sprintf(
			"swap %s leaves net base exposure %s — legs must offset", pair.Symbol(), net))
	}
	return nil
}
