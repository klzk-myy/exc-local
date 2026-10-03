// JetStream adapters (Task 21.3.14): reportable events arrive on the
// `trades` and `settlements` canonical streams — independent durable pull
// consumers on LimitsPolicy streams (explicit ack → at-least-once; the
// durable cursor survives restarts, per services/internal/nats).
//
//	trades.>      → TradeFill frames → ResolveTrade → RecordExecution
//	settlements.> → TradeFill copies → RecordSettlement (confirmation/
//	                settlement data appended as lifecycle continuations
//	                for derivative UTIs)
//
// Replay safety: RecordExecution is idempotent (existing NEWT for
// (uti, regime) returns the stored row); RecordSettlement skips when the
// confirmation it carries already landed.
package reporting

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"exchange/internal/ipc"
	"exchange/internal/ipc/wire"
	"exchange/internal/tracing"
)

// Durable names — stable identities; changing one orphans the cursor.
const (
	DurableTrades      = "regreport_trades"
	DurableSettlements = "regreport_settlements"
)

// ExecutionConsumer maps trades-stream fills to RecordExecution calls.
type ExecutionConsumer struct {
	Svc *Service
}

// NewExecutionConsumer wires the consumer; nil service fails closed.
func NewExecutionConsumer(svc *Service) (*ExecutionConsumer, error) {
	if svc == nil {
		return nil, fmt.Errorf("reporting: execution consumer requires service")
	}
	return &ExecutionConsumer{Svc: svc}, nil
}

// HandleMsg decodes one frame; nil → caller ACKs, error → NAK
// (redelivery). Poison frames return nil — never wedge the durable.
func (c *ExecutionConsumer) HandleMsg(ctx context.Context, m jetstream.Msg) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = nil
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
	tc, err := c.Svc.Store.ResolveTrade(ctx, int64(tf.TradeId()))
	if err != nil {
		return fmt.Errorf("reporting consumer: resolve trade %d: %w", tf.TradeId(), err)
	}
	if tc == nil {
		// Trade not yet visible in PG (replica lag) — NAK for redelivery.
		return fmt.Errorf("reporting consumer: trade %d not resolved", tf.TradeId())
	}
	if _, err := c.Svc.RecordExecution(ctx, tc); err != nil {
		return fmt.Errorf("reporting consumer: record trade %d: %w", tf.TradeId(), err)
	}
	return nil
}

// Consume attaches to an ensured durable and blocks until ctx cancels.
func (c *ExecutionConsumer) Consume(ctx context.Context, cons jetstream.Consumer) error {
	return consume(ctx, cons, c.HandleMsg)
}

// ---------------------------------------------------------------------------
// Settlement-side continuation
// ---------------------------------------------------------------------------

// SettlementConsumer maps settlements-stream fills to confirmation
// continuations: for derivative UTIs a settlement lands the
// confirmation/settlement-date data EMIR REFIT requires on the open
// report (spec §14.1a). Non-derivative trades skip — settlement data on
// spot lives in the trade itself.
type SettlementConsumer struct {
	Svc *Service
}

// NewSettlementConsumer wires the consumer.
func NewSettlementConsumer(svc *Service) (*SettlementConsumer, error) {
	if svc == nil {
		return nil, fmt.Errorf("reporting: settlement consumer requires service")
	}
	return &SettlementConsumer{Svc: svc}, nil
}

// HandleMsg — same frame contract as the trades consumer.
func (c *SettlementConsumer) HandleMsg(ctx context.Context, m jetstream.Msg) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = nil
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
	tc, err := c.Svc.Store.ResolveTrade(ctx, int64(tf.TradeId()))
	if err != nil {
		return fmt.Errorf("reporting consumer: resolve trade %d: %w", tf.TradeId(), err)
	}
	if tc == nil {
		return fmt.Errorf("reporting consumer: trade %d not resolved", tf.TradeId())
	}
	return c.Svc.RecordSettlement(ctx, tc)
}

// Consume attaches to an ensured durable and blocks until ctx cancels.
func (c *SettlementConsumer) Consume(ctx context.Context, cons jetstream.Consumer) error {
	return consume(ctx, cons, c.HandleMsg)
}

// RecordSettlement appends the confirmation continuation for derivative
// regimes (EMIR + CFTC P45 where applicable). Idempotent: a confirmation
// carrying the same settlement_date is already recorded → no-op.
func (s *Service) RecordSettlement(ctx context.Context, tc *TradeContext) error {
	if !isDerivative(tc.InstrumentType) {
		return nil
	}
	conf := map[string]any{
		"status": "CONFIRMED",
		"at":     s.Cfg.now().Format(time.RFC3339Nano),
	}
	if tc.SettlementDate != nil {
		conf["settlement_date"] = tc.SettlementDate.UTC().Format("2006-01-02")
	}
	confJSON, _ := json.Marshal(conf)

	uti := UTIFor(s.Cfg.VenueLEI, "TRADE", tc.TradeID)
	for _, regime := range s.RegimesFor(tc) {
		if regime == RegimeMIFID2 || regime == RegimeCFTCP43 {
			continue // RTS 22 + Part 43 don't carry settlement confirmations
		}
		latest, err := s.Store.LatestEvent(ctx, uti, regime)
		if err != nil {
			return err
		}
		if latest == nil {
			// Execution hasn't landed yet — the trades consumer NAKs
			// on its own; settlement continuation is best-effort after.
			continue
		}
		if hasConfirmation(latest.Confirmation, tc.SettlementDate) {
			continue // already recorded — replay-safe
		}
		if _, err := s.RecordLifecycle(ctx, LifecycleInput{
			UTI:          uti,
			Regime:       regime,
			Action:       ActionModify,
			EventType:    EventTypeModify,
			EventTS:      s.Cfg.now(),
			Confirmation: confJSON,
		}); err != nil {
			return fmt.Errorf("reporting: settlement continuation %s: %w", uti, err)
		}
	}
	return nil
}

// hasConfirmation reports whether an existing confirmation JSONB already
// carries the same settlement_date (replay dedup).
func hasConfirmation(raw json.RawMessage, sd *time.Time) bool {
	if len(raw) == 0 {
		return false
	}
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		return false
	}
	if m["status"] != "CONFIRMED" {
		return false
	}
	if sd == nil {
		return true
	}
	return m["settlement_date"] == sd.UTC().Format("2006-01-02")
}

// consume is the shared loop — ACK on success, NAK on error.
func consume(ctx context.Context, cons jetstream.Consumer,
	handle func(context.Context, jetstream.Msg) error) error {
	cc, err := cons.Consume(func(m jetstream.Msg) {
		if err := handle(ctx, m); err != nil {
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
