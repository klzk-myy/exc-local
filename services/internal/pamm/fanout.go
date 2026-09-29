package pamm

import (
	"context"
	"fmt"
	"math"

	"github.com/nats-io/nats.go/jetstream"

	"exchange/internal/ipc"
	"exchange/internal/ipc/wire"
	"exchange/internal/settlement"
	"exchange/pkg/decimal"
)

// ---------------------------------------------------------------------------
// Fill-event source (documented seam decision — Task 14.3.8 step 3)
// ---------------------------------------------------------------------------
//
// The cheapest deterministic fill-event seam is the NATS JetStream
// `trades` stream the settlements bridge ALREADY republishes from the
// Aeron outbound ring (bridge/route.go: EventTypeTradeFill →
// "trades.{shard}.{symbol}"). Consuming it:
//   - adds zero new surface to the engine hot path;
//   - inherits JetStream at-least-once delivery + durable replay;
//   - decodes the same wire.TradeFill the settlement consumer decodes,
//     resolved through the same PgxTradeResolver (orders ⨝ instruments ⨝
//     accounts) — the fan-out never invents its own account mapping.
//
// At-least-once is absorbed by the DB dedup keys
// (pamm_fill_allocations UNIQUE (master_trade_id, allocation_id) and
// copy_child_orders UNIQUE (master_trade_id, follow_id)) — redelivery
// resolves to a Duplicate, never a double allocation.

// MasterLeg is one side of a resolved fill attributed to a master
// account (PAMM pool account or copy strategy manager).
type MasterLeg struct {
	TradeID      int64
	InstrumentID int64
	AccountID    int64 // master account that took this leg
	Side         string
	Quantity     decimal.Decimal
	Price        decimal.Decimal
}

// LegHandler fans one master leg out. Returns handled=false when the
// account is not a master this handler owns (the fan-out tries the next
// handler). Errors are delivery-fatal: the message NAKs and redelivers.
type LegHandler interface {
	Handle(ctx context.Context, leg MasterLeg) (handled bool, err error)
}

// PoolLegHandler adapts *Engine to LegHandler — a leg is a pool fill
// when its account is a pamm_pools.pool_account_id.
type PoolLegHandler struct{ Engine *Engine }

// Handle implements LegHandler.
func (h PoolLegHandler) Handle(ctx context.Context, leg MasterLeg) (bool, error) {
	pool, err := h.Engine.store.PoolByAccountID(ctx, leg.AccountID)
	if err != nil {
		return false, err
	}
	if pool == nil {
		return false, nil // not a pool account — try the next handler
	}
	_, err = h.Engine.OnPoolFill(ctx, MasterFill{
		MasterAccountID: leg.AccountID,
		TradeID:         leg.TradeID,
		InstrumentID:    leg.InstrumentID,
		Side:            leg.Side,
		Quantity:        leg.Quantity.String(),
		Price:           leg.Price.String(),
	})
	return true, err
}

// TradesFanout consumes the `trades` JetStream stream and fans resolved
// fills out to the registered master handlers (PAMM pools, copy
// managers). One fill can hit TWO masters (a pool account buying from a
// copy manager's resting order) — both legs dispatch independently.
type TradesFanout struct {
	resolver settlement.TradeResolver
	handlers []LegHandler
}

// NewTradesFanout wires the consumer. resolver + ≥1 handler required —
// a consumer that can't resolve or dispatch must never run.
func NewTradesFanout(res settlement.TradeResolver, handlers ...LegHandler) (*TradesFanout, error) {
	if res == nil || len(handlers) == 0 {
		return nil, fmt.Errorf("pamm: trades fanout requires a resolver and ≥1 handler")
	}
	for i, h := range handlers {
		if h == nil {
			return nil, fmt.Errorf("pamm: trades fanout handler %d is nil", i)
		}
	}
	return &TradesFanout{resolver: res, handlers: handlers}, nil
}

// HandleMsg decodes one trades message, resolves it, and dispatches each
// leg to the handlers that own the master account. Return nil → caller
// ACKs; non-nil → NAK (redelivery per the at-least-once contract — the
// DB dedup keys make redelivery safe). Malformed frames return nil after
// counting — a poison frame must never wedge the durable.
func (f *TradesFanout) HandleMsg(ctx context.Context, m jetstream.Msg) (err error) {
	defer func() {
		if r := recover(); r != nil {
			// Decode panic on a poison frame → swallow (caller ACKs the
			// durable away) — never crash the consume loop.
			err = nil
		}
	}()
	data := m.Data()
	if len(data) < 8 {
		return nil // un-decodable — not a fill frame
	}
	ev := ipc.DecodeEvent(data)
	if ev == nil || ev.TypeType() != wire.EventTypeTradeFill {
		return nil // not a fill — the trades stream also carries others
	}
	tf := ipc.EventTradeFill(ev)
	if tf == nil || tf.TradeId() > math.MaxInt64 {
		return nil
	}
	fill := settlement.EngineFill{
		TradeID:     tf.TradeId(),
		BuyOrderID:  tf.BuyOrderId(),
		SellOrderID: tf.SellOrderId(),
		Price:       decimal.NewFromScaled(tf.Price()),
		Qty:         decimal.NewFromScaled(tf.Qty()),
		EngineSeq:   tf.Seq(),
	}
	rt, err := f.resolver.Resolve(ctx, fill)
	if err != nil {
		return fmt.Errorf("pamm fanout: resolve trade %d: %w", fill.TradeID, err)
	}
	legs := []MasterLeg{
		{TradeID: int64(fill.TradeID), InstrumentID: rt.InstrumentID,
			AccountID: rt.BuyerAccountID, Side: "BUY",
			Quantity: fill.Qty, Price: fill.Price},
		{TradeID: int64(fill.TradeID), InstrumentID: rt.InstrumentID,
			AccountID: rt.SellerAccountID, Side: "SELL",
			Quantity: fill.Qty, Price: fill.Price},
	}
	for _, leg := range legs {
		for _, h := range f.handlers {
			handled, herr := h.Handle(ctx, leg)
			if herr != nil {
				return fmt.Errorf("pamm fanout: leg %d acct %d: %w",
					leg.TradeID, leg.AccountID, herr)
			}
			if handled {
				break // one owner per leg per handler chain
			}
		}
	}
	return nil
}

// Consume attaches the fan-out to an already-ensured durable consumer
// (the caller creates it via excnats.Client.EnsureConsumer on the
// "trades" stream — e.g. durable "pamm_fanout", filter "trades.>").
// ACKs on success, NAKs on handler error — at-least-once; the DB dedup
// keys absorb replays. Blocks until ctx is cancelled.
func (f *TradesFanout) Consume(ctx context.Context, cons jetstream.Consumer) error {
	cc, err := cons.Consume(func(m jetstream.Msg) {
		if err := f.HandleMsg(ctx, m); err != nil {
			_ = m.Nak()
			return
		}
		_ = m.Ack()
	})
	if err != nil {
		return fmt.Errorf("pamm fanout: consume: %w", err)
	}
	defer cc.Stop()
	<-ctx.Done()
	return ctx.Err()
}
