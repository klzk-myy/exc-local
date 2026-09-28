// Task 6.3.20 — all-market statistics + delayed anonymous block-trade
// tape (spec §10.8 companion, §24 #291).
//
// Two producers share one TradeEvent stream:
//
//   - StatsProducer publishes `stats@all` and `miniTicker@all` on a 1s
//     cadence. stats@all carries per-symbol rolling windows for the
//     1h/4h/1d/1w set (canonical trade data, shared rollingWindow from
//     ticker.go); miniTicker@all is the thin 24h view. No per-account
//     attribution exists anywhere in the payload — aggregates only.
//   - BlockTapeProducer publishes `blockTrades@{symbol}` for executions
//     at or above the venue block threshold (default $1,000,000 USD
//     notional per the §28.1 Block Tape matrix row), held for the
//     configured regulatory delay (MiFID II deferred publication —
//     default 15m, "up to 15-min" per the same row). Payloads are
//     ANONYMOUS: no side/direction, no account or order ids, no resting
//     hidden-liquidity leakage — only post-execution terms + the
//     publication delay marker. Corrections/busts publish immediately
//     with a link once the original entry is public — holding back a
//     bust would leave a stale public tape (§27 candidate: corrections
//     bypass the deferral that applies to first publication).
//
// stats@all payload:
//
//	{"event":"marketStats","symbols":[{"symbol":"EUR/USD","last_price":..,
//	  "windows":{"1h":{…},"4h":{…},"1d":{…},"1w":{…}}}],"ts_ms":…}
//
// miniTicker@all payload:
//
//	{"event":"miniTicker","symbols":[{"symbol":..,"open":..,"high":..,
//	  "low":..,"close":..,"volume":..,"quote_volume":..}],"ts_ms":…}
//
// blockTrades@ payload:
//
//	{"event":"blockTrade","block_trade_id":…,"symbol":"EUR/USD",
//	 "price":..,"quantity":..,"notional_usd":..,"exec_ts_ms":…,
//	 "pub_ts_ms":…,"delay_ms":…,"venue_flags":["DEFERRED_PUBLICATION"]}
package marketdata

import (
	"context"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"exchange/pkg/decimal"
)

// ---------------------------------------------------------------------------
// All-market statistics
// ---------------------------------------------------------------------------

// statWindows are the canonical rolling windows for stats@all (Task
// 6.3.20 item 1). "1d" is 24h.
var statWindows = []struct {
	Label string
	Dur   time.Duration
}{
	{"1h", time.Hour},
	{"4h", 4 * time.Hour},
	{"1d", 24 * time.Hour},
	{"1w", 7 * 24 * time.Hour},
}

// windowStatsData is one rolling-window aggregate inside the stats frame.
type windowStatsData struct {
	Open         string `json:"open"`
	High         string `json:"high"`
	Low          string `json:"low"`
	Close        string `json:"close"`
	Volume       string `json:"volume"`
	QuoteVolume  string `json:"quote_volume"`
	TradeCount   int64  `json:"trade_count"`
	FirstTradeID uint64 `json:"first_trade_id"`
	LastTradeID  uint64 `json:"last_trade_id"`
	WindowMs     int64  `json:"window_ms"`
}

// symbolStats is one symbol's entry in the stats@all frame.
type symbolStats struct {
	Symbol    string                     `json:"symbol"`
	LastPrice string                     `json:"last_price"`
	Windows   map[string]windowStatsData `json:"windows"`
}

// statsData is the stats@all payload.
type statsData struct {
	Event   string        `json:"event"` // "marketStats"
	Symbols []symbolStats `json:"symbols"`
	TsMs    int64         `json:"ts_ms"`
}

// miniTickerEntry is one symbol in the miniTicker@all frame (24h view).
type miniTickerEntry struct {
	Symbol      string `json:"symbol"`
	Open        string `json:"open"`
	High        string `json:"high"`
	Low         string `json:"low"`
	Close       string `json:"close"`
	Volume      string `json:"volume"`
	QuoteVolume string `json:"quote_volume"`
	TradeCount  int64  `json:"trade_count"`
}

// miniTickerData is the miniTicker@all payload.
type miniTickerData struct {
	Event   string            `json:"event"` // "miniTicker"
	Symbols []miniTickerEntry `json:"symbols"`
	TsMs    int64             `json:"ts_ms"`
}

// StatsProducerConfig tunes StatsProducer.
type StatsProducerConfig struct {
	Logger      *slog.Logger
	Now         func() time.Time
	InputBuffer int           // Push() depth; default 8192
	BucketWidth time.Duration // default 5s (see ticker.go granularity note)
	Tick        time.Duration // emit cadence; default 1s
}

func (c *StatsProducerConfig) defaults() {
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	if c.InputBuffer <= 0 {
		c.InputBuffer = 8192
	}
	if c.BucketWidth <= 0 {
		c.BucketWidth = 5 * time.Second
	}
	if c.Tick <= 0 {
		c.Tick = time.Second
	}
}

// StatsProducer maintains the four rolling windows per symbol and emits
// the all-market frames each tick. src may be nil (drive via Push).
type StatsProducer struct {
	cfg  StatsProducerConfig
	src  TradeSource
	emit EmitFunc
	seq  *seqAllocator
	in   chan TradeEvent

	wins map[string][]*rollingWindow // symbol → len(statWindows) windows
}

// NewStatsProducer wires the producer; emit is Server.Publish.
func NewStatsProducer(cfg StatsProducerConfig, src TradeSource, emit EmitFunc) *StatsProducer {
	cfg.defaults()
	return &StatsProducer{
		cfg: cfg, src: src, emit: emit,
		seq: newSeqAllocator(), in: make(chan TradeEvent, cfg.InputBuffer),
		wins: map[string][]*rollingWindow{},
	}
}

// Push injects a trade event directly (tests/embedders).
func (p *StatsProducer) Push(ev TradeEvent) {
	select {
	case p.in <- ev:
	default:
		p.cfg.Logger.Error("marketdata: stats input saturated — trade dropped",
			"symbol", ev.Symbol, "trade_id", ev.TradeID)
	}
}

func (p *StatsProducer) absorb(ev TradeEvent) {
	if ev.Symbol == "" {
		return
	}
	ws := p.wins[ev.Symbol]
	if ws == nil {
		ws = make([]*rollingWindow, len(statWindows))
		for i, w := range statWindows {
			ws[i] = newRollingWindow(w.Dur, p.cfg.BucketWidth)
		}
		p.wins[ev.Symbol] = ws
	}
	for _, w := range ws {
		w.add(ev)
	}
}

// emitAll builds and publishes both all-market frames. Symbols whose
// 1d window emptied are dropped (no live stats to report).
func (p *StatsProducer) emitAll(now time.Time) {
	if len(p.wins) == 0 {
		return
	}
	syms := make([]string, 0, len(p.wins))
	for s := range p.wins {
		syms = append(syms, s)
	}
	sort.Strings(syms) // deterministic payload order

	stats := statsData{Event: "marketStats", TsMs: now.UnixMilli()}
	mini := miniTickerData{Event: "miniTicker", TsMs: now.UnixMilli()}
	for _, sym := range syms {
		ws := p.wins[sym]
		for _, w := range ws {
			w.expire(now)
		}
		// The 1d window gates liveness: a symbol with no trades in 24h
		// drops out of both frames entirely.
		s1d, live := ws[2].stats()
		if !live {
			delete(p.wins, sym)
			continue
		}
		entry := symbolStats{Symbol: sym, LastPrice: s1d.Close.String(),
			Windows: map[string]windowStatsData{}}
		for i, w := range ws {
			if s, ok := w.stats(); ok {
				entry.Windows[statWindows[i].Label] = windowStatsData{
					Open: s.Open.String(), High: s.High.String(),
					Low: s.Low.String(), Close: s.Close.String(),
					Volume: s.Volume.String(), QuoteVolume: s.QuoteVolume.String(),
					TradeCount: s.TradeCount, FirstTradeID: s.FirstTradeID,
					LastTradeID: s.LastTradeID,
					WindowMs:    statWindows[i].Dur.Milliseconds(),
				}
			}
		}
		stats.Symbols = append(stats.Symbols, entry)
		mini.Symbols = append(mini.Symbols, miniTickerEntry{
			Symbol: sym, Open: s1d.Open.String(), High: s1d.High.String(),
			Low: s1d.Low.String(), Close: s1d.Close.String(),
			Volume: s1d.Volume.String(), QuoteVolume: s1d.QuoteVolume.String(),
			TradeCount: s1d.TradeCount,
		})
	}
	if len(stats.Symbols) > 0 {
		p.emit("stats@all", p.seq.next("stats@all"), stats)
		p.emit("miniTicker@all", p.seq.next("miniTicker@all"), mini)
	}
}

// Run drives the producer until ctx is cancelled.
func (p *StatsProducer) Run(ctx context.Context) error {
	var srcCh <-chan TradeEvent
	if p.src != nil {
		ch, err := p.src.Trades(ctx)
		if err != nil {
			return err
		}
		srcCh = ch
	}
	tick := time.NewTicker(p.cfg.Tick)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
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
			p.emitAll(p.cfg.Now())
		}
	}
}

// ---------------------------------------------------------------------------
// Block-trade tape
// ---------------------------------------------------------------------------

// BlockThresholdUSD is the default block-trade notional floor — the
// §28.1 Block Tape matrix pins $1,000,000 USD.
var BlockThresholdUSD = decimal.NewFromInt(1_000_000)

// DefaultBlockDelay is the MiFID II deferred-publication default — the
// §28.1 row allows "up to 15-min delay"; the venue runs the maximum.
const DefaultBlockDelay = 15 * time.Minute

// USDNotional resolves a trade's USD notional (qty×price for USD-quoted
// pairs, qty for USD-base pairs). Returns false when the pair has no USD
// leg — the caller decides whether a conversion oracle is available;
// never estimates through a crossed rate (§2.7: no fabricated pricing).
type USDNotional func(ev TradeEvent) (decimal.Decimal, bool)

// DirectUSDNotional is the built-in resolver: "X/USD" → qty×price,
// "USD/X" → qty. Anything else returns false — a real cross-rate
// converter (Phase-19.5 oracle) can replace it at wiring time.
func DirectUSDNotional(ev TradeEvent) (decimal.Decimal, bool) {
	base, quote, ok := strings.Cut(ev.Symbol, "/")
	if !ok || base == "" || quote == "" {
		return decimal.Zero, false
	}
	switch quote {
	case "USD":
		return ev.Quantity.Mul(ev.Price), true
	}
	if base == "USD" {
		return ev.Quantity, true
	}
	return decimal.Zero, false
}

// blockTradeData is the emitted blockTrades@ payload — anonymous: no
// side, no ids beyond the tape's own block_trade_id.
type blockTradeData struct {
	Event        string   `json:"event"` // "blockTrade"
	BlockTradeID uint64   `json:"block_trade_id"`
	Symbol       string   `json:"symbol"`
	Price        string   `json:"price"`
	Quantity     string   `json:"quantity"`
	NotionalUSD  string   `json:"notional_usd"`
	ExecTsMs     int64    `json:"exec_ts_ms"`
	PubTsMs      int64    `json:"pub_ts_ms"`
	DelayMs      int64    `json:"delay_ms"`
	VenueFlags   []string `json:"venue_flags"`
}

// blockCorrectionData announces a bust/adjustment of a published entry —
// emitted on the original entry's channel, linked by block_trade_id and
// the engine trade_id it corrects.
type blockCorrectionData struct {
	Event             string `json:"event"` // "blockTradeCorrection"
	Kind              string `json:"kind"`  // "CORRECTION" | "BUST"
	BlockTradeID      uint64 `json:"block_trade_id"`
	OriginalTradeID   uint64 `json:"original_trade_id"`
	CorrectedPrice    string `json:"corrected_price,omitempty"`
	CorrectedQuantity string `json:"corrected_quantity,omitempty"`
	TsMs              int64  `json:"ts_ms"`
}

// BlockTapeSink persists published entries for the Phase-23 ClickHouse
// tape (block_trades_tape) — optional; nil disables persistence.
type BlockTapeSink interface {
	Record(ctx context.Context, pub blockTradeData) error
}

// blockItem is the delay-gate payload.
type blockItem struct {
	ev       TradeEvent
	notional decimal.Decimal
	id       uint64
	tradeID  uint64 // dedup/correction key
}

// blockState tracks a trade's tape lifecycle so a correction can decide
// between cancel-unpublished (never observed → stays private) and
// publish-correction (already public → linked fix).
type blockState uint8

const (
	blockQueued blockState = iota + 1
	blockPublished
	blockCancelled
)

// BlockTapeProducerConfig tunes BlockTapeProducer.
type BlockTapeProducerConfig struct {
	Logger       *slog.Logger
	Now          func() time.Time
	InputBuffer  int             // Push() depth; default 1024
	ThresholdUSD decimal.Decimal // default $1,000,000
	Delay        time.Duration   // default 15m (MiFID II deferred publication)
	Notional     USDNotional     // default DirectUSDNotional
	Sink         BlockTapeSink   // optional ClickHouse/history seam
}

func (c *BlockTapeProducerConfig) defaults() {
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	if c.InputBuffer <= 0 {
		c.InputBuffer = 1024
	}
	if c.ThresholdUSD.IsZero() {
		c.ThresholdUSD = BlockThresholdUSD
	}
	if c.Delay <= 0 {
		c.Delay = DefaultBlockDelay
	}
	if c.Notional == nil {
		c.Notional = DirectUSDNotional
	}
}

// BlockTapeProducer publishes qualifying executions after the delay
// gate. src may be nil (drive via Push).
type BlockTapeProducer struct {
	cfg  BlockTapeProducerConfig
	src  TradeSource
	emit EmitFunc
	seq  *seqAllocator
	in   chan TradeEvent
	gate *DelayGate[blockItem]

	mu        sync.Mutex
	next      uint64
	state     map[uint64]blockState // trade_id → tape lifecycle
	published map[uint64]blockRef   // trade_id → public print (corrections)
}

// blockRef is what a correction needs to link back to a public print.
type blockRef struct {
	symbol  string
	blockID uint64
}

// NewBlockTapeProducer wires the producer; emit is Server.Publish.
func NewBlockTapeProducer(cfg BlockTapeProducerConfig, src TradeSource, emit EmitFunc) *BlockTapeProducer {
	cfg.defaults()
	p := &BlockTapeProducer{
		cfg: cfg, src: src, emit: emit,
		seq: newSeqAllocator(), in: make(chan TradeEvent, cfg.InputBuffer),
		state: map[uint64]blockState{}, published: map[uint64]blockRef{},
	}
	p.gate = NewDelayGate[blockItem](cfg.Delay, p.publish, cfg.Now)
	return p
}

// Push injects a trade event directly (tests/embedders).
func (p *BlockTapeProducer) Push(ev TradeEvent) {
	select {
	case p.in <- ev:
	default:
		p.cfg.Logger.Error("marketdata: block tape input saturated — trade dropped",
			"symbol", ev.Symbol, "trade_id", ev.TradeID)
	}
}

// absorb enqueues trades above the block threshold.
func (p *BlockTapeProducer) absorb(ev TradeEvent) {
	if ev.Symbol == "" {
		return
	}
	notional, ok := p.cfg.Notional(ev)
	if !ok || notional.LessThan(p.cfg.ThresholdUSD) {
		return
	}
	p.mu.Lock()
	if p.state[ev.TradeID] == blockCancelled {
		p.mu.Unlock()
		return // pre-cancelled by a correction that raced the absorb
	}
	p.next++
	id := p.next
	p.state[ev.TradeID] = blockQueued
	p.mu.Unlock()
	p.gate.Push(blockItem{ev: ev, notional: notional, id: id,
		tradeID: ev.TradeID}, ev.Ts)
}

// publish releases one block print — the delay gate's emit side.
func (p *BlockTapeProducer) publish(it blockItem) {
	p.mu.Lock()
	st := p.state[it.tradeID]
	if st == blockCancelled {
		p.mu.Unlock()
		return // corrected before publication — never observable
	}
	p.state[it.tradeID] = blockPublished
	p.published[it.tradeID] = blockRef{symbol: it.ev.Symbol, blockID: it.id}
	p.mu.Unlock()

	data := blockTradeData{
		Event: "blockTrade", BlockTradeID: it.id, Symbol: it.ev.Symbol,
		Price: it.ev.Price.String(), Quantity: it.ev.Quantity.String(),
		NotionalUSD: it.notional.String(),
		ExecTsMs:    it.ev.Ts.UnixMilli(),
		PubTsMs:     p.cfg.Now().UnixMilli(),
		DelayMs:     p.cfg.Delay.Milliseconds(),
		VenueFlags:  []string{"DEFERRED_PUBLICATION"},
	}
	p.emit("blockTrades@"+it.ev.Symbol,
		p.seq.next("blockTrades@"+it.ev.Symbol), data)
	if p.cfg.Sink != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		if err := p.cfg.Sink.Record(ctx, data); err != nil {
			p.cfg.Logger.Error("marketdata: block tape sink failed",
				"block_trade_id", it.id, "err", err)
		}
		cancel()
	}
}

// PushCorrection records a correction/bust for a previously seen trade.
// If the block is still queued it is cancelled silently (it was never
// public); if already published a linked correction event is emitted
// immediately — the public tape must not retain a busted print.
func (p *BlockTapeProducer) PushCorrection(tradeID uint64, kind,
	correctedPrice, correctedQty string, ts time.Time) {
	p.mu.Lock()
	var ref blockRef
	switch p.state[tradeID] {
	case blockQueued:
		p.state[tradeID] = blockCancelled
		p.mu.Unlock()
		return // never published → nothing to correct publicly
	case blockPublished:
		ref = p.published[tradeID]
		delete(p.state, tradeID)
		delete(p.published, tradeID)
		p.mu.Unlock()
	default:
		// Unknown trade: the correction may have beaten the trade's
		// async absorb (Push() channels are independent). Record a
		// pre-cancel so a late-arriving qualifying trade never prints;
		// bounded against unbounded ID growth.
		if len(p.state) < 1_000_000 {
			p.state[tradeID] = blockCancelled
		} else {
			p.cfg.Logger.Error("marketdata: block state map saturated — correction not recorded",
				"trade_id", tradeID)
		}
		p.mu.Unlock()
		return
	}
	ch := "blockTrades@" + ref.symbol
	p.emit(ch, p.seq.next(ch), blockCorrectionData{
		Event: "blockTradeCorrection", Kind: kind,
		BlockTradeID:      ref.blockID,
		OriginalTradeID:   tradeID,
		CorrectedPrice:    correctedPrice,
		CorrectedQuantity: correctedQty,
		TsMs:              p.cfg.Now().UnixMilli(),
	})
}

// Run drives the producer until ctx is cancelled.
func (p *BlockTapeProducer) Run(ctx context.Context) error {
	var srcCh <-chan TradeEvent
	if p.src != nil {
		ch, err := p.src.Trades(ctx)
		if err != nil {
			return err
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
			p.absorb(ev)
		case ev := <-p.in:
			p.absorb(ev)
		}
	}
}
