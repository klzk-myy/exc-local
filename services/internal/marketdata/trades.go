// Task 6.3.3 — real-time public trades stream `trades@{symbol}`
// (spec §10.1: "Trades: Real-time, no conflation").
//
// Every TradeEvent is emitted individually — no batching, no window, no
// coalescing. The frame seq is the engine's per-instrument book seq
// (wire.TradeFill.seq == book_seq), preserving trade ordering lineage
// end-to-end; a JetStream redelivery keeps its original seq rather than
// being renumbered. Events whose engine seq is absent (0 — e.g. an
// upstream that never sequenced) take a producer-local per-channel
// counter so the resume ring still sees a monotonic cursor.
//
// Payload (spec Task 6.3.3 field list — trade_id, price, quantity,
// side, timestamp, seq):
//
//	{"event":"trade","symbol":"EUR/USD","trade_id":…,"price":"1.08025",
//	 "quantity":"1500000","side":"BUY","is_buyer_maker":false,
//	 "ts_ms":…,"engine_seq":…}
//
// `side` is the aggressor (taker) side. The wire.TradeFill schema does
// not carry a taker marker — side resolves through the order-admission
// index (events.go). When the transport cannot resolve it, side is
// "UNKNOWN" and is_buyer_maker is omitted — never guessed (§2.7).
package marketdata

import (
	"context"
	"log/slog"
	"time"
)

// tradeData is the emitted trades@ payload.
type tradeData struct {
	Event        string `json:"event"` // "trade"
	Symbol       string `json:"symbol"`
	TradeID      uint64 `json:"trade_id"`
	Price        string `json:"price"`
	Quantity     string `json:"quantity"`
	Side         string `json:"side"` // "BUY" | "SELL" | "UNKNOWN"
	IsBuyerMaker *bool  `json:"is_buyer_maker,omitempty"`
	TsMs         int64  `json:"ts_ms"`      // engine event time
	EngineSeq    uint64 `json:"engine_seq"` // engine book seq
}

// TradesProducerConfig tunes TradesProducer.
type TradesProducerConfig struct {
	Logger      *slog.Logger
	Now         func() time.Time
	InputBuffer int // Push() channel depth; default 8192
}

func (c *TradesProducerConfig) defaults() {
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	if c.InputBuffer <= 0 {
		c.InputBuffer = 8192
	}
}

// TradesProducer fans TradeEvents onto trades@{symbol} with zero
// conflation. src may be nil (drive via Push — the test/embedding seam).
type TradesProducer struct {
	cfg  TradesProducerConfig
	src  TradeSource
	emit EmitFunc
	seq  *seqAllocator
	in   chan TradeEvent
}

// NewTradesProducer wires the producer; emit is Server.Publish.
func NewTradesProducer(cfg TradesProducerConfig, src TradeSource, emit EmitFunc) *TradesProducer {
	cfg.defaults()
	return &TradesProducer{
		cfg: cfg, src: src, emit: emit,
		seq: newSeqAllocator(),
		in:  make(chan TradeEvent, cfg.InputBuffer),
	}
}

// Push injects a trade event directly (tests/embedders). A saturated
// input drops the event — loudly; the seq gap is client-visible.
func (p *TradesProducer) Push(ev TradeEvent) {
	select {
	case p.in <- ev:
	default:
		p.cfg.Logger.Error("marketdata: trades input saturated — trade dropped",
			"symbol", ev.Symbol, "trade_id", ev.TradeID)
	}
}

// emitOne renders and publishes a single trade. seq = engine seq when
// present (monotonic per instrument), else the local channel counter.
func (p *TradesProducer) emitOne(ev TradeEvent) {
	if ev.Symbol == "" {
		return // unroutable — source already counted the drop
	}
	side := string(ev.TakerSide)
	var bm *bool
	switch ev.TakerSide {
	case SideBuy:
		b := false
		bm = &b // buyer was the aggressor → maker was the seller
	case SideSell:
		b := true
		bm = &b
	default:
		side = SideUnknown
	}
	seq := ev.Seq
	if seq == 0 {
		seq = p.seq.next("trades@" + ev.Symbol)
	}
	p.emit("trades@"+ev.Symbol, seq, tradeData{
		Event:        "trade",
		Symbol:       ev.Symbol,
		TradeID:      ev.TradeID,
		Price:        ev.Price.String(),
		Quantity:     ev.Quantity.String(),
		Side:         side,
		IsBuyerMaker: bm,
		TsMs:         ev.Ts.UnixMilli(),
		EngineSeq:    ev.Seq,
	})
}

// Run drives the producer until ctx is cancelled: source trades first,
// then the Push channel. Source exhaustion without cancellation keeps
// the Push path live.
func (p *TradesProducer) Run(ctx context.Context) error {
	var srcCh <-chan TradeEvent
	if p.src != nil {
		ch, err := p.src.Trades(ctx)
		if err != nil {
			return err
		}
		srcCh = ch
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case ev, ok := <-srcCh:
			if !ok {
				srcCh = nil
				continue
			}
			p.emitOne(ev)
		case ev := <-p.in:
			p.emitOne(ev)
		}
	}
}
