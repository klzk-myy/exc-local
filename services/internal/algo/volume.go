// Task 16.3.18 support — observed trade-volume tracking for VP.
//
// TradeVolumeTracker is the in-process VolumeSource implementation fed
// by the `trades` JetStream stream (Aeron→NATS bridge republishes each
// engine TradeFill on "trades.{shard}.{symbol-token}"). It keeps a
// bounded per-symbol event ring (default 1h horizon — a VP parent never
// reads further back) and answers VolumeSince over it.
//
// Durable fallback: when the NATS consumer is unwired, PgVolumeSource
// (store.go) reads the trades tape directly — VP works in degraded
// freshness rather than being disabled.
//
// Note: the tracker is intentionally in-memory — observed volume is a
// rolling statistic, not durable state; a restart replays durable tape
// via the PG fallback / JetStream redelivery rather than snapshotting
// the ring.
package algo

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"exchange/internal/config"
	"exchange/internal/ipc"
	"exchange/internal/ipc/wire"
	"exchange/internal/tracing"
	"exchange/pkg/decimal"
)

// volEvent is one observed trade on the ring.
type volEvent struct {
	at  time.Time
	qty decimal.Decimal
}

// TradeVolumeTracker implements VolumeSource over a rolling ring fed by
// the trades stream.
type TradeVolumeTracker struct {
	mu      sync.Mutex
	events  map[string][]volEvent // canonical symbol → chronological events
	horizon time.Duration
	now     func() time.Time
}

// NewTradeVolumeTracker builds a tracker retaining `horizon` of history
// (≤0 → 1h default — comfortably beyond the VP window cadence).
func NewTradeVolumeTracker(horizon time.Duration) *TradeVolumeTracker {
	if horizon <= 0 {
		horizon = time.Hour
	}
	return &TradeVolumeTracker{
		events: map[string][]volEvent{}, horizon: horizon, now: time.Now,
	}
}

// Record adds one observed trade — public so tests and the NATS glue
// share the seam.
func (t *TradeVolumeTracker) Record(symbol string, qty decimal.Decimal, at time.Time) {
	sym := config.CanonicalSymbol(symbol)
	if sym == "" || !qty.IsPositive() {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.events[sym] = append(t.events[sym], volEvent{at: at, qty: qty})
	t.evictLocked(sym)
}

func (t *TradeVolumeTracker) evictLocked(sym string) {
	cutoff := t.now().Add(-t.horizon)
	evs := t.events[sym]
	i := 0
	for i < len(evs) && evs[i].at.Before(cutoff) {
		i++
	}
	if i > 0 {
		t.events[sym] = append([]volEvent(nil), evs[i:]...)
	}
}

// VolumeSince sums observed trade quantity on `symbol` at-or-after
// `since` — the VolumeSource seam implementation.
func (t *TradeVolumeTracker) VolumeSince(_ context.Context, symbol string,
	since time.Time) (decimal.Decimal, error) {
	sym := config.CanonicalSymbol(symbol)
	t.mu.Lock()
	defer t.mu.Unlock()
	t.evictLocked(sym)
	sum := decimal.Zero
	for _, ev := range t.events[sym] {
		if !ev.at.Before(since) {
			sum = sum.Add(ev.qty)
		}
	}
	return sum, nil
}

// Consume is the JetStream message handler for the `trades` stream —
// feed it via nats.Subscribe (cmd/gateway wires the consumer). Subject
// tail is the symbol token ("EUR-USD" → canonical "EUR/USD"); payload is
// the raw wire.Event — only TradeFill frames count volume.
func (t *TradeVolumeTracker) Consume(msg jetstream.Msg) {
	defer func() { _ = recover() }() // malformed frame must not kill the subscription
	subj := msg.Subject()
	toks := strings.Split(subj, ".")
	if len(toks) < 3 {
		_ = msg.Ack()
		return
	}
	symbol := toks[len(toks)-1]
	body, _, _ := tracing.StripAeronTrace(msg.Data())
	ev := ipc.DecodeEvent(body)
	if ev == nil || ev.TypeType() != wire.EventTypeTradeFill {
		_ = msg.Ack()
		return
	}
	tf := ipc.EventTradeFill(ev)
	if tf == nil {
		_ = msg.Ack()
		return
	}
	qty := decimal.NewFromScaled(tf.Qty())
	t.Record(symbol, qty, t.now())
	_ = msg.Ack()
}
