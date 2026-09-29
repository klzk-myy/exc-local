// Phase-14 Task 14.3.14 — copy-trading engine adapters.
//
// copyChildSubmitter binds the copy engine's ChildSubmitter seam to the
// real order pipeline (orders.Service.Submit): a copy child becomes a
// MARKET order on the investor's account, client_order_id carrying the
// deterministic "copy:{master_trade_id}:{follow_id}" dedup key — a
// redelivered fill replays to the stored ack instead of double-filling.
//
// copySkipNotifier binds SkipNotifier to the notifications service —
// SKIPPED_MIN_NOTIONAL children are durable rows first; this notice is
// the investor-visible explanation, best-effort (a notify failure never
// erases the durable row per the engine contract).
package main

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	excopy "exchange/internal/copy"
	"exchange/internal/notifications"
	"exchange/internal/orders"
)

// copyChildSubmitter implements copy.ChildSubmitter over orders.Service.
type copyChildSubmitter struct {
	svc   *orders.Service
	store *orders.PgStore
}

// SubmitChild resolves the investor account + instrument and submits a
// MARKET child order. The order pipeline's own admission gates (kill
// switch, breakers, OTR, product gates) still apply — a rejection lands
// back on the durable row as REJECTED via the engine.
func (a *copyChildSubmitter) SubmitChild(ctx context.Context,
	investorAccountID int64, req excopy.ChildOrderRequest) (int64, error) {
	acct, err := a.store.AccountByID(ctx, investorAccountID)
	if err != nil {
		return 0, fmt.Errorf("copy child: account lookup: %w", err)
	}
	if acct == nil {
		return 0, fmt.Errorf("copy child: account %d not found", investorAccountID)
	}
	inst, err := a.store.InstrumentByID(ctx, req.InstrumentID)
	if err != nil {
		return 0, fmt.Errorf("copy child: instrument lookup: %w", err)
	}
	if inst == nil {
		return 0, fmt.Errorf("copy child: instrument %d not found", req.InstrumentID)
	}
	qty := req.Quantity
	ack, err := a.svc.Submit(ctx, acct, &orders.SubmitRequest{
		Symbol:        inst.Symbol,
		Side:          req.Side,
		OrderType:     orders.TypeMarket,
		TimeInForce:   orders.TIFIOC,
		Quantity:      &qty,
		ClientOrderID: req.ClientOrderID,
	})
	if err != nil {
		return 0, err
	}
	return ack.OrderID, nil
}

// copySkipNotifier implements copy.SkipNotifier over the notifications
// service: account → user resolution then Notify.
type copySkipNotifier struct {
	pool *pgxpool.Pool
	svc  *notifications.Service
}

func (n *copySkipNotifier) NotifyChildSkipped(ctx context.Context,
	investorAccountID int64, child excopy.ChildOrder, detail string) error {
	var userID int64
	if err := n.pool.QueryRow(ctx,
		`SELECT user_id FROM accounts WHERE id = $1`,
		investorAccountID).Scan(&userID); err != nil {
		return fmt.Errorf("copy skip notice: account→user: %w", err)
	}
	_, err := n.svc.Notify(ctx, userID, notifications.EventCopyChildSkipped,
		map[string]any{
			"follow_id":       child.FollowID,
			"master_trade_id": child.MasterTradeID,
			"instrument_id":   child.InstrumentID,
			"side":            child.Side,
			"quantity":        child.Quantity.String(),
			"notice":          detail,
		})
	return err
}
