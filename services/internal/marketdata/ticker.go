// Task 6.3.4 — rolling 24h ticker `ticker@{symbol}`, 1s cadence
// (spec §10.1: "Ticker: 1s updates (rolling 24h OHLCV)").
//
// Rolling-window machinery is shared with the all-market stats producer
// (Task 6.3.20, stats.go): trades fold into fixed-width buckets and the
// window retains the trailing `dur` of buckets. Statistics are exact to
// bucket granularity — the trailing edge expires whole buckets, so the
// effective window boundary can overshoot by < bucketW (§27 candidate:
// bucketed rather than tick-exact rolling windows; bucketW=5s bounds the
// skew and caps memory at ~8640 buckets per symbol per window).
//
// Emission: every Tick for every symbol with a live (non-empty) window;
// when a symbol's window empties it emits one final frame (last close
// carried, zero volume) then goes quiet until the next trade.
//
// Payload (Task 6.3.4 field list + §10.3 ticker contract):
//
//	{"event":"ticker24h","symbol":"EUR/USD","open":..,"high":..,"low":..,
//	 "close":..,"volume":..,"quote_volume":..,"price_change":..,
//	 "price_change_pct":..,"weighted_avg_price":..,"trade_count":..,
//	 "first_trade_id":..,"last_trade_id":..,"open_time_ms":..,
//	 "close_time_ms":..}
package marketdata

import (
	"context"
	"log/slog"
	"time"

	"exchange/pkg/decimal"
)

// ---------------------------------------------------------------------------
// Bucketed rolling window (shared by ticker + all-market stats)
// ---------------------------------------------------------------------------

// windowBucket is the fixed-width aggregation cell. Trades inside one
// bucket collapse to OHLCV + lineage — the window never stores raw
// ticks.
type windowBucket struct {
	start time.Time
	open  decimal.Decimal
	high  decimal.Decimal
	low   decimal.Decimal
	close decimal.Decimal
	vol   decimal.Decimal // base qty
	qvol  decimal.Decimal // price*qty (quote ccy)
	count int64
	first uint64 // first trade id in the bucket
	last  uint64 // last trade id
}

// fold absorbs one trade into the bucket.
func (b *windowBucket) fold(ev TradeEvent) {
	if b.count == 0 {
		b.open, b.high, b.low, b.close = ev.Price, ev.Price, ev.Price, ev.Price
		b.first = ev.TradeID
	} else {
		if ev.Price.GreaterThan(b.high) {
			b.high = ev.Price
		}
		if ev.Price.LessThan(b.low) {
			b.low = ev.Price
		}
		b.close = ev.Price
	}
	b.vol = b.vol.Add(ev.Quantity)
	b.qvol = b.qvol.Add(ev.Price.Mul(ev.Quantity))
	b.count++
	b.last = ev.TradeID
}

// rollingStats is the materialized window aggregate.
type rollingStats struct {
	Open, High, Low, Close    decimal.Decimal
	Volume, QuoteVolume       decimal.Decimal
	TradeCount                int64
	FirstTradeID, LastTradeID uint64
	Earliest, Latest          time.Time // event-time span of live buckets
}

// rollingWindow is a per-symbol sliding window of `dur`, bucketed at
// `bucketW`. Buckets append in (mostly) event-time order; a late event
// whose bucket still exists folds into it, one whose bucket expired is
// counted late and skipped — a resurrected bucket would re-order window
// statistics.
type rollingWindow struct {
	dur     time.Duration
	bucketW time.Duration

	buckets []*windowBucket         // index 0 = oldest
	byStart map[int64]*windowBucket // bucket start unixnano → bucket
	late    int64                   // events dropped as too-old
}

func newRollingWindow(dur, bucketW time.Duration) *rollingWindow {
	if bucketW <= 0 {
		bucketW = time.Second
	}
	return &rollingWindow{dur: dur, bucketW: bucketW,
		byStart: map[int64]*windowBucket{}}
}

// add folds one trade into its bucket. Returns false for a stale event
// (its bucket already expired — counted, never resurrected).
func (w *rollingWindow) add(ev TradeEvent) bool {
	ts := ev.Ts
	start := ts.Truncate(w.bucketW)
	key := start.UnixNano()

	// Fast path: the newest bucket is the overwhelmingly common target.
	if n := len(w.buckets); n > 0 {
		back := w.buckets[n-1]
		if back.start.Equal(start) {
			back.fold(ev)
			return true
		}
	}
	if b, ok := w.byStart[key]; ok {
		b.fold(ev) // late event into a still-live bucket
		return true
	}
	// Too-old events land before the front: reject rather than unshift.
	if n := len(w.buckets); n > 0 && start.Before(w.buckets[0].start) {
		w.late++
		return false
	}
	b := &windowBucket{start: start}
	b.fold(ev)
	w.buckets = append(w.buckets, b)
	w.byStart[key] = b
	return true
}

// expire evicts buckets fully older than now−dur. Returns true when the
// window transitioned to empty.
func (w *rollingWindow) expire(now time.Time) bool {
	cutoff := now.Add(-w.dur)
	for len(w.buckets) > 0 {
		b := w.buckets[0]
		// Bucket stays while its end is still inside the window.
		if b.start.Add(w.bucketW).After(cutoff) {
			break
		}
		delete(w.byStart, b.start.UnixNano())
		w.buckets = w.buckets[1:]
	}
	return len(w.buckets) == 0
}

// empty reports whether the window holds no live buckets.
func (w *rollingWindow) empty() bool { return len(w.buckets) == 0 }

// stats materializes the window aggregate. O(buckets) — bounded (~17k
// buckets at 5s/24h) and only run at the emit cadence.
func (w *rollingWindow) stats() (rollingStats, bool) {
	if len(w.buckets) == 0 {
		return rollingStats{}, false
	}
	var s rollingStats
	first := true
	for _, b := range w.buckets {
		if b.count == 0 {
			continue
		}
		if first {
			s.Open, s.High, s.Low = b.open, b.high, b.low
			s.FirstTradeID, s.Earliest = b.first, b.start
			first = false
		} else {
			if b.high.GreaterThan(s.High) {
				s.High = b.high
			}
			if b.low.LessThan(s.Low) {
				s.Low = b.low
			}
		}
		s.Close = b.close
		s.Volume = s.Volume.Add(b.vol)
		s.QuoteVolume = s.QuoteVolume.Add(b.qvol)
		s.TradeCount += b.count
		s.LastTradeID = b.last
		s.Latest = b.start
	}
	return s, !first
}

// ---------------------------------------------------------------------------
// Ticker producer
// ---------------------------------------------------------------------------

// tickerData is the emitted ticker@ payload.
type tickerData struct {
	Event            string `json:"event"` // "ticker24h"
	Symbol           string `json:"symbol"`
	Open             string `json:"open"`
	High             string `json:"high"`
	Low              string `json:"low"`
	Close            string `json:"close"`
	Volume           string `json:"volume"`       // base qty, 24h rolling
	QuoteVolume      string `json:"quote_volume"` // quote ccy, 24h rolling
	PriceChange      string `json:"price_change"`
	PriceChangePct   string `json:"price_change_pct"`
	WeightedAvgPrice string `json:"weighted_avg_price"`
	TradeCount       int64  `json:"trade_count"`
	FirstTradeID     uint64 `json:"first_trade_id"`
	LastTradeID      uint64 `json:"last_trade_id"`
	OpenTimeMs       int64  `json:"open_time_ms"`  // window start (emit−24h)
	CloseTimeMs      int64  `json:"close_time_ms"` // emit time
}

// TickerProducerConfig tunes TickerProducer.
type TickerProducerConfig struct {
	Logger      *slog.Logger
	Now         func() time.Time
	InputBuffer int           // Push() depth; default 8192
	Window      time.Duration // rolling window; default 24h
	BucketWidth time.Duration // bucket granularity; default 5s
	Tick        time.Duration // emit cadence; default 1s (spec §10.1)
}

func (c *TickerProducerConfig) defaults() {
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	if c.InputBuffer <= 0 {
		c.InputBuffer = 8192
	}
	if c.Window <= 0 {
		c.Window = 24 * time.Hour
	}
	if c.BucketWidth <= 0 {
		c.BucketWidth = 5 * time.Second
	}
	if c.Tick <= 0 {
		c.Tick = time.Second
	}
}

// TickerProducer emits the rolling-24h ticker frame per symbol on a 1s
// cadence. src may be nil (drive via Push).
type TickerProducer struct {
	cfg  TickerProducerConfig
	src  TradeSource
	emit EmitFunc
	in   chan TradeEvent
	seq  *seqAllocator

	wins map[string]*rollingWindow
	last map[string]rollingStats // tombstone close carry
}

// NewTickerProducer wires the producer; emit is Server.Publish.
func NewTickerProducer(cfg TickerProducerConfig, src TradeSource, emit EmitFunc) *TickerProducer {
	cfg.defaults()
	return &TickerProducer{
		cfg: cfg, src: src, emit: emit,
		seq: newSeqAllocator(), in: make(chan TradeEvent, cfg.InputBuffer),
		wins: map[string]*rollingWindow{}, last: map[string]rollingStats{},
	}
}

// Push injects a trade event directly (tests/embedders).
func (p *TickerProducer) Push(ev TradeEvent) {
	select {
	case p.in <- ev:
	default:
		p.cfg.Logger.Error("marketdata: ticker input saturated — trade dropped",
			"symbol", ev.Symbol, "trade_id", ev.TradeID)
	}
}

func (p *TickerProducer) absorb(ev TradeEvent) {
	if ev.Symbol == "" {
		return
	}
	w := p.wins[ev.Symbol]
	if w == nil {
		w = newRollingWindow(p.cfg.Window, p.cfg.BucketWidth)
		p.wins[ev.Symbol] = w
	}
	if w.add(ev) {
		if s, ok := w.stats(); ok {
			p.last[ev.Symbol] = s
		}
	}
}

// renderTicker builds the payload for one symbol.
func (p *TickerProducer) renderTicker(symbol string, s rollingStats, now time.Time) tickerData {
	chg := s.Close.Sub(s.Open)
	pct := decimal.Zero
	if !s.Open.IsZero() {
		pct = chg.Div(s.Open).Mul(decimal.NewFromInt(100))
	}
	wap := decimal.Zero
	if !s.Volume.IsZero() {
		wap = s.QuoteVolume.Div(s.Volume)
	}
	return tickerData{
		Event: "ticker24h", Symbol: symbol,
		Open: s.Open.String(), High: s.High.String(),
		Low: s.Low.String(), Close: s.Close.String(),
		Volume: s.Volume.String(), QuoteVolume: s.QuoteVolume.String(),
		PriceChange: chg.String(), PriceChangePct: pct.StringFixed(4),
		WeightedAvgPrice: wap.String(),
		TradeCount:       s.TradeCount,
		FirstTradeID:     s.FirstTradeID, LastTradeID: s.LastTradeID,
		OpenTimeMs: now.Add(-p.cfg.Window).UnixMilli(), CloseTimeMs: now.UnixMilli(),
	}
}

// emitAll publishes one frame per live symbol; a symbol whose window
// just emptied emits a single tombstone (last close, zero volume) and is
// then dropped until its next trade.
func (p *TickerProducer) emitAll(now time.Time) {
	for sym, w := range p.wins {
		emptied := w.expire(now)
		s, ok := w.stats()
		if ok {
			ch := "ticker@" + sym
			p.emit(ch, p.seq.next(ch), p.renderTicker(sym, s, now))
			continue
		}
		if emptied {
			// Final frame: last known close carried, zero volume.
			last := p.last[sym]
			last.Volume, last.QuoteVolume, last.TradeCount =
				decimal.Zero, decimal.Zero, 0
			ch := "ticker@" + sym
			p.emit(ch, p.seq.next(ch), p.renderTicker(sym, last, now))
			delete(p.wins, sym)
		}
	}
}

// Run drives the producer until ctx is cancelled.
func (p *TickerProducer) Run(ctx context.Context) error {
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
