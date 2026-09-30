package ohlcv

import (
	"context"

	"exchange/internal/analytics"
)

// CHArchiveSink is the Phase-20 Task 20.3.3 production ArchiveSink: it
// maps each closed Candle onto the ClickHouse analytics projection
// (analytics.OHLCVStore → ohlcv_{interval} SummingMergeTree tables,
// 5-year TTL per spec §16.2/§19.12). fx_klines in PostgreSQL stays the
// hot read model for the REST surface; this sink is the cold-tier
// mirror — only Closed bars cross the seam, same contract as
// PGArchiveSink.
//
// Wiring (orchestrator): swap NewPGArchiveSink → NewCHArchiveSink at the
// engine construction site in services/cmd/gateway/main.go (or wherever
// the candle engine's ArchiveSink is bound):
//
//	chConn, _ := analytics.Dial(ctx, analytics.ConfigFromEnv())
//	sink := ohlcv.NewCHArchiveSink(analytics.NewOHLCVStore(chConn))
//
// PGArchiveSink remains the right choice for deployments without
// ClickHouse; LogArchiveSink stays the bring-up/debug stand-in.
type CHArchiveSink struct {
	sink CHSink
}

// CHSink is the narrow insert seam — *analytics.OHLCVStore satisfies it;
// tests substitute a fake.
type CHSink interface {
	Insert(ctx context.Context, rows []analytics.CandleRow) error
}

// NewCHArchiveSink wires the sink.
func NewCHArchiveSink(sink CHSink) *CHArchiveSink {
	return &CHArchiveSink{sink: sink}
}

// Archive projects one closed bar into ClickHouse; open bars are a
// hot-tier concern and are ignored (nil error, no call through).
// CarryForward gap candles DO archive — a zero-volume bar is a real
// closed bucket, not a fabrication.
func (s *CHArchiveSink) Archive(ctx context.Context, c Candle) error {
	if !c.Closed {
		return nil
	}
	return s.sink.Insert(ctx, []analytics.CandleRow{candleToRow(c)})
}

// candleToRow maps the engine Candle onto the analytics projection.
// Interval resolves through the canonical label ("1m" … "1M") — the
// store's ohlcvTables map routes it to the right table; a non-persisted
// interval (1s) fails closed at the store boundary. InstrumentID is a
// PG-side key; the ClickHouse schema keys on symbol.
func candleToRow(c Candle) analytics.CandleRow {
	return analytics.CandleRow{
		Symbol:      c.Symbol,
		Interval:    c.Interval.String(),
		OpenTime:    c.OpenTime.UTC(),
		Open:        c.Open,
		High:        c.High,
		Low:         c.Low,
		Close:       c.Close,
		Volume:      c.Volume,
		QuoteVolume: c.QuoteVolume,
		TradeCount:  c.TradeCount,
	}
}

var _ ArchiveSink = (*CHArchiveSink)(nil)
