// fillbridge.go — the FILL_BRIDGE consumer (IMP-PLAN Phase-3 Task 4,
// Task 18.3.14, spec §9.8): external-venue fills projected onto the
// parent order's read-model row.
//
// The router publishes FillBridgeEvent JSON to the "settlements"
// JetStream stream under subject token "sorfill". A gateway-side durable
// consumer (filter "settlements.*.sorfill") drives this handler — the
// sor_fill_dedup row already won upstream in Router.onVenueEvent, so
// at-least-once replay here is absorbed by ApplyFill's own idempotent
// projection (engine fills use the same contract).
package sor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/nats-io/nats.go/jetstream"

	"exchange/pkg/decimal"
)

// FillBridgeSubjectToken is the symbol-token half of the publish subject —
// "settlements.{shard}.sorfill" — chosen so the durable consumer's filter
// subject never collides with per-instrument fill traffic.
const FillBridgeSubjectToken = "sorfill"

// ParentFillStore is the slice of orders.Store the projection needs —
// *orders.PgStore satisfies it.
type ParentFillStore interface {
	ApplyFill(ctx context.Context, orderID int64, px, qty decimal.Decimal) error
}

// FillBridgeConsumer maps settlements-stream FILL_BRIDGE events to
// parent-order ApplyFill calls.
type FillBridgeConsumer struct {
	Store ParentFillStore
}

func NewFillBridgeConsumer(store ParentFillStore) *FillBridgeConsumer {
	return &FillBridgeConsumer{Store: store}
}

// HandleMsg decodes one message; nil error → caller ACKs, error → NAK
// (redelivery — a transient store failure must not drop a venue fill).
// Malformed frames and non-FILL_BRIDGE payloads are ACKed away: a poison
// frame must never wedge the durable.
func (c *FillBridgeConsumer) HandleMsg(ctx context.Context, m jetstream.Msg) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = nil // decode panic on a poison frame — swallow, ack away
		}
	}()
	var ev FillBridgeEvent
	if jerr := json.Unmarshal(m.Data(), &ev); jerr != nil ||
		ev.EventType != "FILL_BRIDGE" || ev.ParentOrderID <= 0 {
		return nil
	}
	if c.Store == nil {
		return errors.New("sor: fill-bridge consumer has no store")
	}
	px, err := decimal.NewFromString(ev.Price)
	if err != nil {
		return nil // unparseable venue price — poison frame, ack away
	}
	qty, err := decimal.NewFromString(ev.Qty)
	if err != nil || !qty.IsPositive() {
		return nil
	}
	if err := c.Store.ApplyFill(ctx, ev.ParentOrderID, px, qty); err != nil {
		return fmt.Errorf("sor: fill-bridge apply parent %d: %w",
			ev.ParentOrderID, err)
	}
	return nil
}

// Consume attaches to an ensured durable consumer; blocks until ctx
// cancels. ACK/NAK contract mirrors reporting.ConfirmationConsumer.
func (c *FillBridgeConsumer) Consume(ctx context.Context, cons jetstream.Consumer) error {
	cc, err := cons.Consume(func(m jetstream.Msg) {
		if err := c.HandleMsg(ctx, m); err != nil {
			_ = m.Nak()
			return
		}
		_ = m.Ack()
	})
	if err != nil {
		return fmt.Errorf("sor: fill-bridge consume: %w", err)
	}
	defer cc.Stop()
	<-ctx.Done()
	return ctx.Err()
}
