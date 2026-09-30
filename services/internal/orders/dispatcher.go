// Dispatcher adapts the order pipeline to the accounts.OrderDispatcher
// seam used by the dead-man countdown sweeper (Task 5.3.33) and the
// close-all positions flow (Task 5.3.36). Fail-closed per the contract:
// dispatch failures return an error, never a fabricated ack.
package orders

import (
	"context"

	"exchange/internal/accounts"
	"exchange/pkg/decimal"
)

// Dispatcher implements accounts.OrderDispatcher over Service.
type Dispatcher struct {
	svc *Service
}

// NewDispatcher binds svc to the accounts seam.
func NewDispatcher(svc *Service) *Dispatcher { return &Dispatcher{svc: svc} }

// MassCancel translates the accounts scope vocabulary into the pipeline
// scope and runs the atomic mass cancel.
func (d *Dispatcher) MassCancel(ctx context.Context, scope accounts.MassCancelScope) (*accounts.MassCancelResult, error) {
	res, err := d.svc.MassCancel(ctx, MassCancelScope{
		AccountID:    scope.AccountID,
		InstrumentID: scope.InstrumentID,
		Side:         scope.Side,
		OrderType:    scope.OrderType,
		Reason:       scope.Reason,
	}, "system:"+scope.Reason, "", "")
	if err != nil {
		return nil, err
	}
	return &accounts.MassCancelResult{
		Cancelled: res.Cancelled,
	}, nil
}

// SubmitClose places the reduce-only close order. Per the CloseOrderRequest
// contract the market close is converted to a synthetic limit at
// mark±max_slippage_bps (Phase-02 Task 2.3.15 slippage protection): a
// BUY close caps at ref×(1+bps), a SELL close floors at ref×(1−bps).
// TIF is IOC — a close order must never rest on the book.
func (d *Dispatcher) SubmitClose(ctx context.Context, req accounts.CloseOrderRequest) (*accounts.OrderAck, error) {
	acct, err := d.svc.store.AccountByID(ctx, req.AccountID)
	if err != nil {
		return nil, errInternal("account lookup", err)
	}
	if acct == nil {
		return nil, codeErr("ORDER_NOT_FOUND", "account %d not found", req.AccountID)
	}
	inst, err := d.svc.store.InstrumentByID(ctx, req.InstrumentID)
	if err != nil {
		return nil, errInternal("instrument lookup", err)
	}
	if inst == nil {
		return nil, codeErr("ORDER_NOT_FOUND", "instrument %d not found", req.InstrumentID)
	}
	ref, err := d.svc.store.ReferencePrice(ctx, req.InstrumentID)
	if err != nil {
		return nil, errInternal("reference price", err)
	}
	if (ref == nil || !ref.IsPositive()) && !req.LimitPrice.IsPositive() {
		return nil, codeErr("ORDER_REJECTED_NO_LIQUIDITY",
			"no reference price for close order on %s", inst.Symbol)
	}
	side := string(req.Side)
	var cap_ decimal.Decimal
	if req.LimitPrice.IsPositive() {
		// §13.4 auction leg / force-cash cap: explicit bound supersedes
		// the synthetic slippage band.
		cap_ = req.LimitPrice
	} else {
		bps := decimal.NewFromInt(int64(req.MaxSlippageBps)).Div(decimal.NewFromInt(10000))
		if side == SideBuy {
			cap_ = ref.Mul(decimal.One.Add(bps))
		} else {
			cap_ = ref.Mul(decimal.One.Sub(bps))
		}
	}
	// Align the synthetic cap to the tick grid — a price not on tick is
	// an INVALID_REQUEST down the pipeline, and the engine would have
	// to guess the rounding direction.
	if inst.TickSize.IsPositive() {
		if side == SideBuy {
			cap_ = cap_.Div(inst.TickSize).Floor().Mul(inst.TickSize)
		} else {
			cap_ = cap_.Div(inst.TickSize).Ceil().Mul(inst.TickSize)
		}
	}
	qty := req.Quantity
	tif := TIFIOC
	if req.ExpireAt != nil {
		tif = TIFGTD // resting auction leg through the CALL/EXTEND window
	}
	ack, err := d.svc.Submit(ctx, acct, &SubmitRequest{
		Symbol:        inst.Symbol,
		Side:          side,
		OrderType:     TypeLimit,
		TimeInForce:   tif,
		GTDExpiry:     req.ExpireAt,
		Quantity:      &qty,
		Price:         &cap_,
		ReduceOnly:    true,
		ClientOrderID: req.ClientOrderID,
	})
	if err != nil {
		return &accounts.OrderAck{
			ClientOrderID: req.ClientOrderID,
			Accepted:      false,
			Detail:        err.Error(),
		}, err
	}
	return &accounts.OrderAck{
		OrderID:       ack.OrderID,
		ClientOrderID: ack.ClientOrderID,
		Accepted:      true,
	}, nil
}

// compile-time check: the accounts seam contract.
var _ accounts.OrderDispatcher = (*Dispatcher)(nil)
