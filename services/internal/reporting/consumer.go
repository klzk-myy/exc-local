package reporting

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"exchange/internal/ipc"
	"exchange/internal/ipc/wire"
	"exchange/internal/tracing"
)

// ---------------------------------------------------------------------------
// JetStream adapter — same durable pattern as pamm.TradesFanout /
// analytics.TCAConsumer: consume the `trades` stream (durable pull
// consumer, explicit ack — at-least-once), decode wire.TradeFill, and
// drive ConfirmationService.OnFill. The sibling generator resolves the
// trade's legs, instrument, fees and counterparties internally — this
// adapter carries only the trade id + execution timestamp (the SLA
// reference). Replay is absorbed by the generator's (trade, version)
// bookkeeping.
//
// Orchestrator binding:
//
//	cons, _ := natsClient.EnsureConsumerRetry(ctx, "trades",
//	    "reporting_confirmations", excnats.WithFilterSubject("trades.>"))
//	go consumer.Consume(ctx, cons)
// ---------------------------------------------------------------------------

// ConfirmationConsumer maps trades-stream fills to OnFill calls.
type ConfirmationConsumer struct {
	Svc *ConfirmationService
}

// NewConfirmationConsumer wires the consumer.
func NewConfirmationConsumer(svc *ConfirmationService) (*ConfirmationConsumer, error) {
	if svc == nil {
		return nil, fmt.Errorf("reporting: confirmation consumer requires svc")
	}
	return &ConfirmationConsumer{Svc: svc}, nil
}

// HandleMsg decodes one trades message and generates the trade's
// confirmations. nil → caller ACKs; error → NAK (redelivery). Malformed
// frames return nil — a poison frame must never wedge the durable.
func (c *ConfirmationConsumer) HandleMsg(ctx context.Context, m jetstream.Msg) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = nil // decode panic on a poison frame — swallow, ack away
		}
	}()
	data := m.Data()
	if len(data) < 8 {
		return nil
	}
	body, _, _ := tracing.StripAeronTrace(data)
	ev := ipc.DecodeEvent(body)
	if ev == nil || ev.TypeType() != wire.EventTypeTradeFill {
		return nil
	}
	tf := ipc.EventTradeFill(ev)
	if tf == nil || tf.TradeId() > math.MaxInt64 {
		return nil
	}
	if c.Svc == nil {
		return fmt.Errorf("reporting consumer: nil service")
	}
	_, err = c.Svc.OnFill(ctx, FillNotice{
		TradeID:    int64(tf.TradeId()),
		ExecutedAt: time.Unix(0, int64(ev.Ts())).UTC(),
	})
	if err != nil {
		return fmt.Errorf("reporting consumer: onfill trade %d: %w", tf.TradeId(), err)
	}
	return nil
}

// Consume attaches to an ensured durable consumer; blocks until ctx
// cancels. ACKs on success, NAKs on error — at-least-once per the
// canonical template (nats/consumer.go).
func (c *ConfirmationConsumer) Consume(ctx context.Context, cons jetstream.Consumer) error {
	cc, err := cons.Consume(func(m jetstream.Msg) {
		if err := c.HandleMsg(ctx, m); err != nil {
			_ = m.Nak()
			return
		}
		_ = m.Ack()
	})
	if err != nil {
		return fmt.Errorf("reporting consumer: consume: %w", err)
	}
	defer cc.Stop()
	<-ctx.Done()
	return ctx.Err()
}
