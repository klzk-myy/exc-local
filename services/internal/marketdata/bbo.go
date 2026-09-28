// Task 6.3.11 — Book Ticker / Best Bid-Offer stream `bbo@{symbol}`
// (spec §10.1 hot-path, §24 #261: "Zero-conflation BBO stream emits
// every top-of-book change").
//
// The producer consumes the RAW BookDelta stream (pre-conflation — via
// TeeDeltaSource off the same engine feed the L2 conflator rides; the
// _out ring is SPSC so a second attach is impossible). Every delta whose
// top-of-book differs from the last emitted top emits immediately —
// no window, no batch. A delta that leaves the top unchanged (a deeper
// level moved) emits nothing: top-of-book did not change.
//
// Payload (Task 6.3.11 contract):
//
//	{"event":"bbo","symbol":"EUR/USD","bid":"1.08025","bid_qty":"1500000",
//	 "ask":"1.08030","ask_qty":"2000000","engine_seq":…,"ts_ms":…}
//
// An absent book side renders as JSON null (bid/bid_qty/ask/ask_qty are
// *string) — an empty side is a real top-of-book change (best quote
// pulled), never silently zeroed.
package marketdata

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"exchange/pkg/decimal"
)

// bboData is the emitted bbo@ payload. *string fields render null when
// the side is absent.
type bboData struct {
	Event     string  `json:"event"` // "bbo"
	Symbol    string  `json:"symbol"`
	Bid       *string `json:"bid"`
	BidQty    *string `json:"bid_qty"`
	Ask       *string `json:"ask"`
	AskQty    *string `json:"ask_qty"`
	EngineSeq uint64  `json:"engine_seq"`
	TsMs      int64   `json:"ts_ms"`
}

// bboTop is the last emitted top-of-book tuple per symbol.
type bboTop struct {
	bidPx, bidQty  int64
	askPx, askQty  int64
	hasBid, hasAsk bool
	set            bool
}

// BBOProducerConfig tunes BBOProducer.
type BBOProducerConfig struct {
	Logger      *slog.Logger
	Now         func() time.Time
	InputBuffer int // Push() channel depth; default 8192
}

func (c *BBOProducerConfig) defaults() {
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

// BBOProducer fans top-of-book changes onto bbo@{symbol} with zero
// conflation. src may be nil (drive via Push).
type BBOProducer struct {
	cfg  BBOProducerConfig
	src  DeltaSource
	emit EmitFunc
	in   chan BookDelta

	mu   sync.Mutex
	tops map[string]bboTop
	seq  *seqAllocator // fallback when EngineSeq==0
}

// NewBBOProducer wires the producer; emit is Server.Publish.
func NewBBOProducer(cfg BBOProducerConfig, src DeltaSource, emit EmitFunc) *BBOProducer {
	cfg.defaults()
	return &BBOProducer{
		cfg: cfg, src: src, emit: emit,
		tops: map[string]bboTop{}, seq: newSeqAllocator(),
		in: make(chan BookDelta, cfg.InputBuffer),
	}
}

// Push injects a book delta directly (tests/embedders). Saturated input
// drops loudly — a dropped delta can hide a top-of-book change.
func (p *BBOProducer) Push(d BookDelta) {
	select {
	case p.in <- d:
	default:
		p.cfg.Logger.Error("marketdata: bbo input saturated — delta dropped",
			"symbol", d.Symbol, "engine_seq", d.EngineSeq)
	}
}

func strptr(d decimal.Decimal) *string {
	s := d.String()
	return &s
}

// emitChanged publishes the top-of-book frame when it differs from the
// last emitted tuple for the symbol. Returns true when emitted.
func (p *BBOProducer) emitChanged(d BookDelta) bool {
	var t bboTop
	if len(d.Bids) > 0 {
		t.bidPx, t.bidQty, t.hasBid = d.Bids[0].Price, d.Bids[0].Qty, true
	}
	if len(d.Asks) > 0 {
		t.askPx, t.askQty, t.hasAsk = d.Asks[0].Price, d.Asks[0].Qty, true
	}

	p.mu.Lock()
	prev, seen := p.tops[d.Symbol]
	if seen && prev == t {
		p.mu.Unlock()
		return false
	}
	p.tops[d.Symbol] = t
	p.mu.Unlock()

	seq := d.EngineSeq
	if seq == 0 {
		seq = p.seq.next("bbo@" + d.Symbol)
	}
	var bid, bidQty, ask, askQty *string
	if t.hasBid {
		bid = strptr(decimal.NewFromScaled(t.bidPx))
		bidQty = strptr(decimal.NewFromScaled(t.bidQty))
	}
	if t.hasAsk {
		ask = strptr(decimal.NewFromScaled(t.askPx))
		askQty = strptr(decimal.NewFromScaled(t.askQty))
	}
	ts := d.Ts
	if ts.IsZero() {
		ts = p.cfg.Now()
	}
	p.emit("bbo@"+d.Symbol, seq, bboData{
		Event: "bbo", Symbol: d.Symbol,
		Bid: bid, BidQty: bidQty, Ask: ask, AskQty: askQty,
		EngineSeq: d.EngineSeq, TsMs: ts.UnixMilli(),
	})
	return true
}

// Run drives the producer until ctx is cancelled.
func (p *BBOProducer) Run(ctx context.Context) error {
	var srcCh <-chan BookDelta
	if p.src != nil {
		ch, err := p.src.Deltas(ctx)
		if err != nil {
			return err
		}
		srcCh = ch
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case d, ok := <-srcCh:
			if !ok {
				srcCh = nil
				continue
			}
			p.emitChanged(d)
		case d := <-p.in:
			p.emitChanged(d)
		}
	}
}
