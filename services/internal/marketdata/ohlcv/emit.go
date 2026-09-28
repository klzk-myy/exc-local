package ohlcv

import (
	"context"
	"fmt"
	"time"

	"exchange/pkg/decimal"
)

// ChannelName builds the canonical WS channel for one candle series:
// kline@{symbol}_{timeframe} (Task 6.3.8 step 5, reconciled with §8.4
// item 1 by remediation #35 — the colon form is superseded).
func ChannelName(symbol string, iv Interval) string {
	return "kline@" + symbol + "_" + iv.String()
}

// KlineData is the `data` payload of a kline frame. Decimals render as
// fixed-point strings (8dp — matching fx_klines DECIMAL(20,8)/
// DECIMAL(28,8) column scale and the §5.3 string-decimal wire
// convention); times are epoch millis per the §10.5 WS convention.
type KlineData struct {
	Symbol         string `json:"symbol"`
	Timeframe      string `json:"timeframe"`
	OpenTimeMs     int64  `json:"open_time_ms"`
	CloseTimeMs    int64  `json:"close_time_ms"` // exclusive bucket end
	Open           string `json:"open"`
	High           string `json:"high"`
	Low            string `json:"low"`
	Close          string `json:"close"`
	Volume         string `json:"volume"`
	QuoteVolume    string `json:"quote_volume"`
	TakerBuyVolume string `json:"taker_buy_volume"`
	TradeCount     int64  `json:"trade_count"`
	Closed         bool   `json:"closed"`
	FirstSeq       uint64 `json:"first_seq,omitempty"`
	LastSeq        uint64 `json:"last_seq,omitempty"`
}

// KlineFrame is the server→client event envelope. Field order mirrors the
// canonical ws.eventFrame (internal/ws, spec §10.5):
// {"type":"event","channel":..,"seq":..,"data":{...},"ts_ms":..}.
type KlineFrame struct {
	Type    string    `json:"type"` // always "event"
	Channel string    `json:"channel"`
	Seq     uint64    `json:"seq"` // per-channel monotonic (Task 6.3.9)
	Data    KlineData `json:"data"`
	TsMs    int64     `json:"ts_ms"`
}

func fixed8(d decimal.Decimal) string { return d.StringFixed(8) }

// NewKlineFrame renders c into the wire envelope.
func NewKlineFrame(channel string, seq uint64, c *Candle, now time.Time) KlineFrame {
	return KlineFrame{
		Type:    "event",
		Channel: channel,
		Seq:     seq,
		Data: KlineData{
			Symbol:         c.Symbol,
			Timeframe:      c.Interval.String(),
			OpenTimeMs:     c.OpenTime.UnixMilli(),
			CloseTimeMs:    c.CloseTime.UnixMilli(),
			Open:           fixed8(c.Open),
			High:           fixed8(c.High),
			Low:            fixed8(c.Low),
			Close:          fixed8(c.Close),
			Volume:         fixed8(c.Volume),
			QuoteVolume:    fixed8(c.QuoteVolume),
			TakerBuyVolume: fixed8(c.TakerBuyVolume),
			TradeCount:     c.TradeCount,
			Closed:         c.Closed,
			FirstSeq:       c.FirstSeq,
			LastSeq:        c.LastSeq,
		},
		TsMs: now.UnixMilli(),
	}
}

// Emitter delivers kline frames to subscribers. The real WebSocket
// fan-out (internal/ws) lands in a later wave; tests use a recording
// emitter. Emit must not block unboundedly — the engine treats an error
// as a metric, not a halt.
type Emitter interface {
	Emit(ctx context.Context, frame KlineFrame) error
}

// SeqAllocator assigns the per-channel monotonic sequence number stamped
// on each frame (Task 6.3.9). The default is an in-memory counter; spec
// §10.7 durable sequences plug in here later.
type SeqAllocator func(channel string) uint64

// Source yields trade events. The production adapter is a JetStream pull
// consumer over the "trades" stream (Task 6.3.8 step 1); tests drive the
// engine with a fake. Returning a closed channel signals end-of-stream.
type Source interface {
	Events(ctx context.Context) (<-chan TradeEvent, error)
}

// LateTrade is one reconciled event: it targeted an already-finalized
// bucket and was counted rather than applied (closed candles are
// immutable — spec §2.7 fail-closed pessimism).
type LateTrade struct {
	Event    TradeEvent
	Interval Interval
	Bucket   time.Time // the finalized bucket the trade targeted
	Reason   string    // "bucket_closed"
}

func (l LateTrade) String() string {
	return fmt.Sprintf("%s %s bucket=%s trade_id=%d seq=%d",
		l.Event.Symbol, l.Interval, l.Bucket.UTC().Format(time.RFC3339),
		l.Event.TradeID, l.Event.Seq)
}
