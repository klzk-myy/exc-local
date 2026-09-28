// Package-internal wiring adapters for cmd/gateway — small bridges
// between the Phase-05 subsystems (order pipeline, dead-man switch,
// manual liquidation sink) and the seams they plug into.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"

	"exchange/internal/accounts"
	"exchange/internal/api"
	"exchange/internal/config"
	"exchange/internal/nats"
	"exchange/internal/orders"
)

// shardIDs returns the full engine shard universe for the out-ring
// consumer: every statically-mapped shard plus the elastic range
// [elasticBase, elasticBase+elasticCount).
func shardIDs(m *config.ShardMap) []uint16 {
	set := map[int]struct{}{}
	for _, id := range m.Entries() {
		set[id] = struct{}{}
	}
	for i := m.ElasticBase(); i < m.ElasticBase()+m.ElasticCount(); i++ {
		set[i] = struct{}{}
	}
	out := make([]uint16, 0, len(set))
	for id := range set {
		out = append(out, uint16(id))
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// countdownAdapter exposes accounts.DeadManService as the ws surface's
// CountdownController (Task 5.3.33 — countdown_ms 0 disables).
type countdownAdapter struct {
	svc *accounts.DeadManService
}

func (a countdownAdapter) Set(ctx context.Context, accountID, countdownMs int64, renew bool) (serverTime, expiry int64, err error) {
	ack, err := a.svc.Set(ctx, accountID, countdownMs, renew)
	if err != nil {
		return 0, 0, err
	}
	return ack.ServerTime, ack.CountdownExpiry, nil
}

// liquidationSink publishes MANUAL_LIQUIDATION events to the
// margin-events JetStream stream (Task 5.3.30 → Phase-19 auction
// machinery). Subject is account-ordered: margin-events.0.ACCT-{id}.
// A publish failure propagates so the endpoint fails closed.
type liquidationSink struct {
	nc *nats.Client
}

func (s liquidationSink) EmitManualLiquidation(ctx context.Context, ev api.ManualLiquidationEvent) error {
	payload, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	_, err = s.nc.Publish(ctx, "margin-events", 0,
		"ACCT-"+strconv.FormatInt(ev.AccountID, 10), payload)
	return err
}

// orderDispatchAdapter binds accounts.OrderDispatcher (dead-man sweeper,
// close-all) to orders.Service — the single dispatch path, never a
// direct engine call.
type orderDispatchAdapter struct {
	svc   *orders.Service
	store *orders.PgStore
}

func (a orderDispatchAdapter) MassCancel(ctx context.Context, scope accounts.MassCancelScope) (*accounts.MassCancelResult, error) {
	res, err := a.svc.MassCancel(ctx, orders.MassCancelScope{
		AccountID:    scope.AccountID,
		InstrumentID: scope.InstrumentID,
		Side:         scope.Side,
		OrderType:    scope.OrderType,
		Reason:       scope.Reason,
	}, "system:"+scope.Reason, "", "")
	if err != nil {
		return nil, err
	}
	return &accounts.MassCancelResult{Cancelled: res.Cancelled}, nil
}

func (a orderDispatchAdapter) SubmitClose(ctx context.Context, req accounts.CloseOrderRequest) (*accounts.OrderAck, error) {
	inst, err := a.store.InstrumentByID(ctx, req.InstrumentID)
	if err != nil || inst == nil {
		return nil, fmt.Errorf("instrument %d not found", req.InstrumentID)
	}
	acct, err := a.store.AccountByID(ctx, req.AccountID)
	if err != nil || acct == nil {
		return nil, fmt.Errorf("account %d not found", req.AccountID)
	}
	qty := req.Quantity
	ack, err := a.svc.Submit(ctx, acct, &orders.SubmitRequest{
		Symbol:        inst.Symbol,
		Side:          string(req.Side),
		OrderType:     orders.TypeMarket,
		Quantity:      &qty,
		ReduceOnly:    req.ReduceOnly,
		ClientOrderID: req.ClientOrderID,
	})
	if err != nil {
		return nil, err
	}
	return &accounts.OrderAck{
		OrderID: ack.OrderID, ClientOrderID: ack.ClientOrderID,
		Accepted: true,
	}, nil
}
