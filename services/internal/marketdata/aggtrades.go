// Task 6.3.12 — aggregated trades stream `aggTrades@{symbol}`
// (spec §24 #262: fills from one taker order at one price consolidate
// into a single message preserving first/last trade-ID lineage).
//
// The engine emits a taker's fills contiguously (single-threaded
// matching: one taker sweep produces back-to-back TradeFills before the
// next order), so the aggregate key is the run of consecutive fills for
// a symbol sharing (taker_order_id, price). An aggregate flushes when
// the key changes, when the FlushInterval safety timer fires (bounds
// hold time at the tail of a sweep — a taker can straddle a timer edge
// and split into two aggregates; lineage is still exact), or on
// shutdown.
//
// Fills whose aggressor is unresolvable (TakerOrderID == 0 — wire
// TradeFill carries no taker marker; see events.go) NEVER group: each
// becomes a single-fill aggregate (first==last) rather than risking a
// merge across different takers. Grouping is lineage-preserving either
// way.
//
// Payload (Task 6.3.12 contract):
//
//	{"event":"aggTrade","agg_trade_id":…,"symbol":"EUR/USD",
//	 "price":"1.08025","quantity":"4500000","first_trade_id":…,
//	 "last_trade_id":…,"trade_count":3,"is_buyer_maker":false,
//	 "ts_ms":…,"engine_seq":…}
package marketdata

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"exchange/pkg/decimal"
)

// aggTradeData is the emitted aggTrades@ payload.
type aggTradeData struct {
	Event        string `json:"event"` // "aggTrade"
	AggTradeID   uint64 `json:"agg_trade_id"`
	Symbol       string `json:"symbol"`
	Price        string `json:"price"`
	Quantity     string `json:"quantity"` // total base qty across constituents
	FirstTradeID uint64 `json:"first_trade_id"`
	LastTradeID  uint64 `json:"last_trade_id"`
	TradeCount   int    `json:"trade_count"`
	IsBuyerMaker *bool  `json:"is_buyer_maker,omitempty"`
	TsMs         int64  `json:"ts_ms"`      // first constituent's event time
	EngineSeq    uint64 `json:"engine_seq"` // last constituent's engine seq
}

// aggKey groups contiguous fills: same aggressor at the same price.
// TakerOrderID 0 (unknown) never groups — handled as a flush-on-every-
// fill singleton by keying on the trade id itself.
type aggKey struct {
	taker uint64
	price int64
}

// pendingAgg is one symbol's accumulating aggregate.
type pendingAgg struct {
	key     aggKey
	qty     decimal.Decimal
	firstID uint64
	lastID  uint64
	count   int
	bm      *bool
	ts      time.Time
	lastSeq uint64
}

// AggTradesProducerConfig tunes AggTradesProducer.
type AggTradesProducerConfig struct {
	Logger      *slog.Logger
	Now         func() time.Time
	InputBuffer int           // Push() depth; default 8192
	FlushEvery  time.Duration // pending-aggregate safety flush; default 250ms
}

func (c *AggTradesProducerConfig) defaults() {
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	if c.InputBuffer <= 0 {
		c.InputBuffer = 8192
	}
	if c.FlushEvery <= 0 {
		c.FlushEvery = 250 * time.Millisecond
	}
}

// AggTradesProducer folds same-taker/same-price fill runs onto
// aggTrades@{symbol}. src may be nil (drive via Push).
type AggTradesProducer struct {
	cfg  AggTradesProducerConfig
	src  TradeSource
	emit EmitFunc
	in   chan TradeEvent
	seq  *seqAllocator // agg_trade_id domain (per channel)

	mu      sync.Mutex
	pending map[string]*pendingAgg
	nextID  map[string]uint64 // per-symbol agg_trade_id counter
}

// NewAggTradesProducer wires the producer; emit is Server.Publish.
func NewAggTradesProducer(cfg AggTradesProducerConfig, src TradeSource, emit EmitFunc) *AggTradesProducer {
	cfg.defaults()
	return &AggTradesProducer{
		cfg: cfg, src: src, emit: emit,
		seq: newSeqAllocator(), in: make(chan TradeEvent, cfg.InputBuffer),
		pending: map[string]*pendingAgg{}, nextID: map[string]uint64{},
	}
}

// Push injects a trade event directly (tests/embedders).
func (p *AggTradesProducer) Push(ev TradeEvent) {
	select {
	case p.in <- ev:
	default:
		p.cfg.Logger.Error("marketdata: aggTrades input saturated — trade dropped",
			"symbol", ev.Symbol, "trade_id", ev.TradeID)
	}
}

// absorb folds one fill into its symbol's pending aggregate, flushing
// the prior aggregate when the grouping key changes.
func (p *AggTradesProducer) absorb(ev TradeEvent) {
	if ev.Symbol == "" {
		return
	}
	// Unresolvable aggressor → singleton key on the trade id: it can
	// never match a neighbour's key, so each such fill emits alone.
	key := aggKey{taker: ev.TakerOrderID, price: decimal.Scaled(ev.Price)}
	if ev.TakerOrderID == 0 {
		key.taker = ^uint64(0) - ev.TradeID // unique per fill
	}

	p.mu.Lock()
	pend := p.pending[ev.Symbol]
	if pend != nil && pend.key != key {
		p.flushLocked(ev.Symbol) // emits under p.mu — Publish never blocks
		pend = nil
	}
	if pend == nil {
		pend = &pendingAgg{key: key, firstID: ev.TradeID, ts: ev.Ts}
		p.pending[ev.Symbol] = pend
		switch ev.TakerSide {
		case SideBuy:
			b := false
			pend.bm = &b
		case SideSell:
			b := true
			pend.bm = &b
		}
	}
	pend.qty = pend.qty.Add(ev.Quantity)
	pend.lastID = ev.TradeID
	pend.count++
	if ev.Seq > pend.lastSeq {
		pend.lastSeq = ev.Seq
	}
	if pend.ts.IsZero() {
		pend.ts = ev.Ts
	}
	p.mu.Unlock()
}

// flushLocked emits the symbol's pending aggregate. Caller holds p.mu.
// Emit-under-lock preserves channel order — Publish fans out without
// blocking so this cannot stall the pipeline.
func (p *AggTradesProducer) flushLocked(symbol string) {
	pa := p.pending[symbol]
	if pa == nil {
		return
	}
	delete(p.pending, symbol)
	p.nextID[symbol]++
	seq := pa.lastSeq
	if seq == 0 {
		seq = p.seq.next("aggTrades@" + symbol)
	}
	p.emit("aggTrades@"+symbol, seq, aggTradeData{
		Event:        "aggTrade",
		AggTradeID:   p.nextID[symbol],
		Symbol:       symbol,
		Price:        decimal.NewFromScaled(pa.key.price).String(),
		Quantity:     pa.qty.String(),
		FirstTradeID: pa.firstID,
		LastTradeID:  pa.lastID,
		TradeCount:   pa.count,
		IsBuyerMaker: pa.bm,
		TsMs:         pa.ts.UnixMilli(),
		EngineSeq:    pa.lastSeq,
	})
}

// flushAll emits every pending aggregate (timer/shutdown path).
func (p *AggTradesProducer) flushAll() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for sym := range p.pending {
		p.flushLocked(sym)
	}
}

// Run drives the producer until ctx is cancelled.
func (p *AggTradesProducer) Run(ctx context.Context) error {
	var srcCh <-chan TradeEvent
	if p.src != nil {
		ch, err := p.src.Trades(ctx)
		if err != nil {
			return err
		}
		srcCh = ch
	}
	tick := time.NewTicker(p.cfg.FlushEvery)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			p.flushAll()
			return ctx.Err()
		case ev, ok := <-srcCh:
			if !ok {
				srcCh = nil
				continue
			}
			p.absorb(ev)
		case ev := <-p.in:
			p.absorb(ev)
		case <-tick.C:
			p.flushAll()
		}
	}
}
