// Package ohlcv is the OHLCV candlestick aggregation engine
// (Phase-06 Tasks 6.3.8/6.3.14; spec §10.3, §16.2, §24 #264).
//
// It consumes TradeEvents through the Source interface, aggregates ticks
// into candles across the canonical 13 aligned intervals, persists closed
// bars to the PostgreSQL fx_klines table (migration 173) through
// CandleStore, and emits kline@{symbol}_{timeframe} frames through the
// Emitter interface.
//
// Contracts honored here:
//   - Pre-materialized candles (spec §10.3, remediation #38): chart reads
//     NEVER scan trade history; this engine is the writer side that
//     pre-computes every bar. fx_klines is the low-latency REST read model.
//   - Canonical 13 intervals (spec §24 #264): 1s 1m 5m 15m 30m 1h 2h 4h
//     6h 8h 1D 1W 1M. 1s is memory-only — a 60-second ring buffer, never
//     persisted (Task 6.3.14); the other 12 are the §16.2 persisted set.
//   - Buckets align to UTC boundaries; a trade at an exact bucket edge
//     belongs to the NEW bucket. 1D = 00:00 UTC, 1W = Monday 00:00 UTC,
//     1M = first of month 00:00 UTC (Task 6.3.8 step 8). 8h bars align to
//     00/08/16 UTC — the major FX session boundaries (Task 6.3.14).
//   - Late/out-of-order trades update a still-open candle only while the
//     bucket remains within Config.GracePeriod of its close; anything
//     older routes to the reconciliation counter/hook — closed candles
//     are never silently rewritten (DB upsert additionally guards on
//     closed=false).
//   - Quiet intervals emit zero-volume carry-forward candles
//     (O=H=L=C=previous close, trade_count=0) per Task 6.3.8 step 7 —
//     except the memory-only 1s tier, where synthesized quiet-second
//     filler would evict real bars from the 60-bar ring; 1s finalizes
//     empty buckets silently (recorded for §27).
//   - In-progress bar updates are emitted throttled to ≤2/sec per channel
//     (Task 6.3.8 step 5); closed bars always emit immediately.
//
// Deferred seams (documented, later waves):
//   - NATS JetStream consumer adapter over the "trades" stream
//     (subjects trades.{shard}.{symbol}; the "EUR-USD" subject token maps
//     to canonical "EUR/USD"). Source is the seam.
//   - Real WebSocket fan-out behind Emitter (internal/ws).
//   - ClickHouse ohlcv_{interval} SummingMergeTree cold-tier writer
//     behind ArchiveSink (spec §16.2, Phase-20 Task 20.3.3 projection);
//     PGArchiveSink is the dev stand-in.
//   - Cross-restart durable channel sequences (spec §10.7): SeqAllocator
//     is injectable so the WS layer can plug a persistent allocator.
package ohlcv
