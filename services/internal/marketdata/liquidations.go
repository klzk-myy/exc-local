// Task 6.3.13 — public liquidation feed `liquidations@{symbol}` +
// `liquidations@all` (spec §24 #263, §13.4).
//
// THE 2-SECOND ANTI-FRONT-RUNNING GATE IS MANDATORY: every event routes
// through DelayGate anchored at the event's own timestamp — a delayed
// liquidation can never be observable before its delay elapses (release
// = min(event_ts, arrival)+2s; see delay.go). The gate is the ONLY
// publication path; there is no fast lane.
//
// Payload (Task 6.3.13 contract — anonymized, no account/order ids):
//
//	{"event":"liquidation","symbol":"EUR/USD","side":"SELL",
//	 "order_type":"LIMIT","price":"1.08000","quantity":"5000000",
//	 "is_auction":true,"ts_ms":…,"pub_ts_ms":…}
//
// Data source (transport shell — Task 6.3.13's remediation-#35 note
// defers the real feed to Phase-19): the Phase-19 liquidation scanner
// publishes on JetStream "margin-events" (stream registered in
// internal/nats.Streams). The interim wire contract consumed here is a
// JSON envelope on margin-events.{shard}.{symbol}:
//
//	{"type":"liquidation","side":"BUY|SELL","order_type":"…",
//	 "price":"1.08000","qty":"5000000","is_auction":true,"ts_ms":…}
//
// If Phase-19 lands with a different payload shape, only the decoder
// below changes — the delay gate and fanout are final.
package marketdata

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"
)

// LiquidationDelay is the spec-pinned anti-front-running hold (§24
// #263). Config-injectable for tests; production wiring never reduces
// it below this constant — enforced in NewLiquidationsProducer.
const LiquidationDelay = 2 * time.Second

// LiquidationEvent is one forced-liquidation or auction outcome —
// already anonymized (producers upstream must not attach account or
// order identity; the feed must not enable auction front-running).
type LiquidationEvent struct {
	Symbol    string
	Side      string // "BUY"|"SELL" — the forced order's side
	OrderType string // e.g. "LIMIT","MARKET"; "" renders "UNKNOWN"
	Price     string // decimal string
	Qty       string // decimal string, base currency
	IsAuction bool   // true when part of a §13.4 auction
	Ts        time.Time
}

// LiquidationSource yields liquidation events (Phase-19 scanner via
// NATS margin-events in production; tests drive Push).
type LiquidationSource interface {
	Liquidations(ctx context.Context) (<-chan LiquidationEvent, error)
}

// LiquidationSourceFunc adapts a function to LiquidationSource.
type LiquidationSourceFunc func(ctx context.Context) (<-chan LiquidationEvent, error)

// Liquidations implements LiquidationSource.
func (f LiquidationSourceFunc) Liquidations(ctx context.Context) (<-chan LiquidationEvent, error) {
	return f(ctx)
}

// liquidationData is the emitted payload — mirrors the spec field list.
type liquidationData struct {
	Event     string `json:"event"` // "liquidation"
	Symbol    string `json:"symbol"`
	Side      string `json:"side"`
	OrderType string `json:"order_type"`
	Price     string `json:"price"`
	Quantity  string `json:"quantity"`
	IsAuction bool   `json:"is_auction"`
	TsMs      int64  `json:"ts_ms"`     // event time
	PubMs     int64  `json:"pub_ts_ms"` // actual publication time (delay transparency)
}

// ---------------------------------------------------------------------------
// margin-events JetStream adapter (interim contract — Phase-19)
// ---------------------------------------------------------------------------

// marginEventJSON is the interim margin-events payload. Unknown types are
// skipped — the stream also carries margin calls the feed must not leak.
type marginEventJSON struct {
	Type      string `json:"type"`
	Side      string `json:"side"`
	OrderType string `json:"order_type"`
	Price     string `json:"price"`
	Qty       string `json:"qty"`
	IsAuction bool   `json:"is_auction"`
	TsMs      int64  `json:"ts_ms"`
}

// JetStreamLiquidationSource decodes margin-events messages into
// LiquidationEvents. Only type=="liquidation" payloads flow through;
// the symbol comes from the subject token.
func JetStreamLiquidationSource(src MsgSource, symbols SubjectSymbol,
	log *slog.Logger) LiquidationSource {
	if log == nil {
		log = slog.Default()
	}
	return LiquidationSourceFunc(func(ctx context.Context) (<-chan LiquidationEvent, error) {
		raw, err := src(ctx)
		if err != nil {
			return nil, err
		}
		out := make(chan LiquidationEvent, 256)
		go func() {
			defer close(out)
			for {
				select {
				case <-ctx.Done():
					return
				case m, ok := <-raw:
					if !ok {
						return
					}
					var e marginEventJSON
					if err := json.Unmarshal(m.Data, &e); err != nil ||
						e.Type != "liquidation" {
						continue // wrong type/malformed — not a liquidation
					}
					sym, ok := symbols.symbol(m.Subject)
					if !ok {
						log.Warn("marketdata: liquidation on unrouted subject",
							"subject", m.Subject)
						continue
					}
					lev := LiquidationEvent{
						Symbol: sym, Side: e.Side, OrderType: e.OrderType,
						Price: e.Price, Qty: e.Qty, IsAuction: e.IsAuction,
						Ts: time.UnixMilli(e.TsMs),
					}
					select {
					case out <- lev:
					case <-ctx.Done():
						return
					}
				}
			}
		}()
		return out, nil
	})
}

// ---------------------------------------------------------------------------
// Producer
// ---------------------------------------------------------------------------

// LiquidationsProducerConfig tunes LiquidationsProducer.
type LiquidationsProducerConfig struct {
	Logger      *slog.Logger
	Now         func() time.Time
	Delay       time.Duration // min LiquidationDelay (2s) — floor enforced
	InputBuffer int           // Push() depth; default 1024
}

func (c *LiquidationsProducerConfig) defaults() {
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	// §24 #263 floor — a smaller configured delay is a config bug, not a
	// feature; clamp up to the mandatory minimum.
	if c.Delay < LiquidationDelay {
		c.Delay = LiquidationDelay
	}
	if c.InputBuffer <= 0 {
		c.InputBuffer = 1024
	}
}

// LiquidationsProducer holds every liquidation event for the mandatory
// delay, then publishes to liquidations@{symbol} AND liquidations@all.
// src may be nil (drive via Push).
type LiquidationsProducer struct {
	cfg  LiquidationsProducerConfig
	src  LiquidationSource
	emit EmitFunc
	seq  *seqAllocator
	in   chan LiquidationEvent
	gate *DelayGate[LiquidationEvent]
}

// NewLiquidationsProducer wires the producer; emit is Server.Publish.
func NewLiquidationsProducer(cfg LiquidationsProducerConfig,
	src LiquidationSource, emit EmitFunc) *LiquidationsProducer {
	cfg.defaults()
	p := &LiquidationsProducer{
		cfg: cfg, src: src, emit: emit,
		seq: newSeqAllocator(),
		in:  make(chan LiquidationEvent, cfg.InputBuffer),
	}
	p.gate = NewDelayGate[LiquidationEvent](cfg.Delay, p.publish, cfg.Now)
	return p
}

// Push injects a liquidation event directly (tests/embedders).
func (p *LiquidationsProducer) Push(ev LiquidationEvent) {
	select {
	case p.in <- ev:
	default:
		p.cfg.Logger.Error("marketdata: liquidations input saturated — event dropped",
			"symbol", ev.Symbol)
	}
}

// publish emits one released event to the per-symbol channel and the
// market-wide channel. Called by the delay gate — strictly post-delay.
func (p *LiquidationsProducer) publish(ev LiquidationEvent) {
	if ev.Symbol == "" {
		return
	}
	ot := ev.OrderType
	if ot == "" {
		ot = "UNKNOWN"
	}
	data := liquidationData{
		Event: "liquidation", Symbol: ev.Symbol,
		Side: ev.Side, OrderType: ot,
		Price: ev.Price, Quantity: ev.Qty, IsAuction: ev.IsAuction,
		TsMs:  ev.Ts.UnixMilli(),
		PubMs: p.cfg.Now().UnixMilli(),
	}
	p.emit("liquidations@"+ev.Symbol,
		p.seq.next("liquidations@"+ev.Symbol), data)
	p.emit("liquidations@all", p.seq.next("liquidations@all"), data)
}

// Gate exposes the delay gate's depth counters (metrics/tests).
func (p *LiquidationsProducer) Gate() *DelayGate[LiquidationEvent] { return p.gate }

// Run drives the producer until ctx is cancelled.
func (p *LiquidationsProducer) Run(ctx context.Context) error {
	var srcCh <-chan LiquidationEvent
	if p.src != nil {
		ch, err := p.src.Liquidations(ctx)
		if err != nil {
			return fmt.Errorf("marketdata: liquidation source: %w", err)
		}
		srcCh = ch
	}
	gateDone := make(chan error, 1)
	go func() { gateDone <- p.gate.Run(ctx) }()
	for {
		select {
		case err := <-gateDone:
			return err
		case <-ctx.Done():
			<-gateDone
			return ctx.Err()
		case ev, ok := <-srcCh:
			if !ok {
				srcCh = nil
				continue
			}
			p.gate.Push(ev, ev.Ts)
		case ev := <-p.in:
			p.gate.Push(ev, ev.Ts)
		}
	}
}
