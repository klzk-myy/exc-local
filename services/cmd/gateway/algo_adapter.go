// Phase-16 — algo framework adapters.
//
// algoChildExecutor binds algo.ChildExecutor to the real order pipeline
// (orders.Service.Submit/Cancel + orders.PgStore.GetOrder) — the same
// seam shape as copyChildSubmitter: algo children are LIMIT orders on
// the parent's account with the derived "algo:{parent}:{seq}"
// client_order_id, so every pipeline admission gate (kill switch,
// breakers, risk limits, balance sufficiency, §8.7 dedup) applies and a
// crash-retry replays the stored ack rather than double-submitting.
// Status reads reuse the pipeline's durable read model — the engine's
// out-ring consumer already folds TradeFill into orders.filled_qty.
package main

import (
	"context"
	"fmt"

	"exchange/internal/algo"
	"exchange/internal/orders"
)

// algoChildExecutor implements algo.ChildExecutor over orders.Service.
type algoChildExecutor struct {
	svc   *orders.Service
	store *orders.PgStore
}

func (a *algoChildExecutor) account(ctx context.Context, accountID int64) (*orders.Account, error) {
	acct, err := a.store.AccountByID(ctx, accountID)
	if err != nil {
		return nil, fmt.Errorf("algo child: account lookup: %w", err)
	}
	if acct == nil {
		return nil, fmt.Errorf("algo child: account %d not found", accountID)
	}
	return acct, nil
}

// SubmitChild dispatches a LIMIT child through the full order pipeline.
// TIF maps verbatim — GTC for resting slices, IOC for VP/spread legs.
func (a *algoChildExecutor) SubmitChild(ctx context.Context, accountID int64,
	req algo.ChildRequest) (int64, error) {
	acct, err := a.account(ctx, accountID)
	if err != nil {
		return 0, err
	}
	tif := req.TimeInForce
	if tif == "" {
		tif = orders.TIFGTC
	}
	qty := req.Quantity
	price := req.Price
	ack, err := a.svc.Submit(ctx, acct, &orders.SubmitRequest{
		Symbol:        req.Symbol,
		Side:          req.Side,
		OrderType:     orders.TypeLimit,
		TimeInForce:   tif,
		Quantity:      &qty,
		Price:         &price,
		ClientOrderID: req.ClientOrderID,
		PostOnly:      req.PostOnly,
	})
	if err != nil {
		return 0, err
	}
	return ack.OrderID, nil
}

// CancelChild cancels a live child through the engine-confirm cancel
// path — same semantics as the REST cancel endpoint.
func (a *algoChildExecutor) CancelChild(ctx context.Context, accountID,
	orderID int64) error {
	acct, err := a.account(ctx, accountID)
	if err != nil {
		return err
	}
	_, err = a.svc.Cancel(ctx, acct, orderID, "algo:"+fmt.Sprint(accountID), "", "")
	return err
}

// ChildStatus reads the pipeline's durable order state — the out-ring
// consumer's TradeFill folding is the fill feed (no second event tap).
func (a *algoChildExecutor) ChildStatus(ctx context.Context,
	orderID int64) (*algo.ChildStatus, error) {
	o, err := a.store.GetOrder(ctx, orderID)
	if err != nil {
		return nil, err
	}
	if o == nil {
		return nil, fmt.Errorf("algo child: order %d not found", orderID)
	}
	return &algo.ChildStatus{
		Status:       o.Status,
		FilledQty:    o.FilledQty,
		AvgFillPrice: o.AvgFillPrice,
	}, nil
}
