package ohlcv

import (
	"context"
	"log/slog"
)

// ArchiveSink is the hot→cold tier seam for closed candles. Spec §16.2
// projects the 12 persisted intervals into ClickHouse
// ohlcv_{interval} SummingMergeTree tables (5-year TTL per §19.12); the
// production sink is a ClickHouse batch writer owned by the analytics
// consumer (Phase-20 Task 20.3.3). fx_klines in PostgreSQL stays the hot
// read model for the REST surface regardless.
//
// Deferred to the ClickHouse wave: real CH schema/client, batching, and
// backfill. Only closed bars archive — in-progress rows are a hot-tier
// concern.
type ArchiveSink interface {
	Archive(ctx context.Context, c Candle) error
}

// PGArchiveSink is the dev stand-in for the §16.2 ClickHouse tier: it
// re-uses the fx_klines upsert so a deployment without ClickHouse still
// exercises the archive path end-to-end. The closed-row immutability
// guard on the upsert makes repeat archives idempotent.
type PGArchiveSink struct {
	store *PGStore
}

// NewPGArchiveSink wires the dev sink.
func NewPGArchiveSink(store *PGStore) *PGArchiveSink {
	return &PGArchiveSink{store: store}
}

// Archive persists the closed bar; open candles are ignored.
func (s *PGArchiveSink) Archive(ctx context.Context, c Candle) error {
	if !c.Closed {
		return nil
	}
	return s.store.Save(ctx, c)
}

// LogArchiveSink reports each closed bar through slog — useful while the
// ClickHouse writer is absent and for bring-up debugging.
type LogArchiveSink struct {
	Log *slog.Logger
}

// Archive logs the bar; a nil logger uses slog.Default().
func (s LogArchiveSink) Archive(_ context.Context, c Candle) error {
	l := s.Log
	if l == nil {
		l = slog.Default()
	}
	l.Info("kline archive", "symbol", c.Symbol, "timeframe", c.Interval.String(),
		"open_time", c.OpenTime, "close", c.Close.String(), "trades", c.TradeCount,
		"carry_forward", c.CarryForward)
	return nil
}
