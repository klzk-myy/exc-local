package ohlcv

import (
	"time"

	"exchange/pkg/decimal"
)

// Side is the taker (aggressor) side of a trade.
type Side string

const (
	SideBuy  Side = "BUY"
	SideSell Side = "SELL"
)

// TradeEvent is one public tape entry consumed by the engine. It is the
// transport-neutral projection of the core's wire.TradeFill (Aeron /
// FlatBuffers) as republished on the JetStream "trades" stream by the
// Bridge service (Phase-03 Task 3.3.10).
//
// Symbol is the canonical DB/wire form ("EUR/USD"); the NATS adapter maps
// the "EUR-USD" subject token. Seq is the engine's per-symbol trade
// sequence (trades.trade_seq): 0 means unsequenced and skips the
// regression check. Ts is the engine event time; a zero Ts falls back to
// the engine clock.
type TradeEvent struct {
	TradeID      uint64
	InstrumentID int64  // 0 → CandleStore resolves id from Symbol
	Symbol       string // canonical "EUR/USD"
	Price        decimal.Decimal
	Quantity     decimal.Decimal // base currency
	TakerSide    Side            // "" when unknown: taker_buy_volume not accumulated
	Seq          uint64          // engine per-symbol sequence; 0 = unsequenced
	Ts           time.Time       // event time (converted to UTC)
}

// Candle is one OHLCV bar for a (symbol, interval, bucket). Decimal fields
// are exact shopspring decimals — no float64 anywhere on the money path
// (spec §5.3 invariant 1).
type Candle struct {
	InstrumentID int64
	Symbol       string
	Interval     Interval
	OpenTime     time.Time // bucket start (UTC, inclusive)
	CloseTime    time.Time // bucket end (UTC, exclusive) = Interval.Next(OpenTime)

	Open           decimal.Decimal // first trade price in the bucket
	High           decimal.Decimal // max trade price
	Low            decimal.Decimal // min trade price
	Close          decimal.Decimal // last trade price
	Volume         decimal.Decimal // cumulative base quantity
	QuoteVolume    decimal.Decimal // cumulative price*qty (quote ccy)
	TakerBuyVolume decimal.Decimal // base qty where TakerSide == BUY
	TradeCount     int64
	FirstSeq       uint64 // first trade seq aggregated (0 when empty)
	LastSeq        uint64 // last trade seq aggregated

	Closed       bool // true once the bucket finalized
	CarryForward bool // synthesized zero-volume gap candle (Task 6.3.8 step 7)
}

// newCandle opens a bar for the bucket starting at start.
func newCandle(ev TradeEvent, iv Interval, start time.Time) *Candle {
	return &Candle{
		InstrumentID: ev.InstrumentID,
		Symbol:       ev.Symbol,
		Interval:     iv,
		OpenTime:     start,
		CloseTime:    iv.Next(start),
	}
}

// gapCandle synthesizes the zero-volume carry-forward bar for an
// untraded bucket: O=H=L=C=previous close (Task 6.3.8 step 7).
func gapCandle(symbol string, instrumentID int64, iv Interval,
	start time.Time, prevClose decimal.Decimal) Candle {
	return Candle{
		InstrumentID: instrumentID,
		Symbol:       symbol,
		Interval:     iv,
		OpenTime:     start,
		CloseTime:    iv.Next(start),
		Open:         prevClose,
		High:         prevClose,
		Low:          prevClose,
		Close:        prevClose,
		Closed:       true,
		CarryForward: true,
	}
}

// addTrade folds one tick into the bar.
func (c *Candle) addTrade(ev TradeEvent) {
	if c.TradeCount == 0 {
		c.Open, c.High, c.Low = ev.Price, ev.Price, ev.Price
		c.FirstSeq = ev.Seq
	} else {
		if ev.Price.GreaterThan(c.High) {
			c.High = ev.Price
		}
		if ev.Price.LessThan(c.Low) {
			c.Low = ev.Price
		}
	}
	c.Close = ev.Price
	c.Volume = c.Volume.Add(ev.Quantity)
	c.QuoteVolume = c.QuoteVolume.Add(ev.Price.Mul(ev.Quantity))
	if ev.TakerSide == SideBuy {
		c.TakerBuyVolume = c.TakerBuyVolume.Add(ev.Quantity)
	}
	c.TradeCount++
	c.LastSeq = ev.Seq
}
