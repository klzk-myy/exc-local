// Task 20.3.3 — OHLCV analytics projection (spec §16.2).
//
// Single-owner rule: the Phase-06 Task 6.3.8 engine computes candles;
// this package only persists + projects them — it NEVER recomputes bars
// from ticks. Closed candles arrive through
// marketdata/ohlcv.CHArchiveSink (archive_ch.go) and are appended into
// the twelve per-interval SummingMergeTree tables owned by the schema
// track (deploy/clickhouse/schema/, database exchange_analytics).
// Deployed DDL (verified via SHOW CREATE TABLE against dev):
//
//	ohlcv_{interval} (
//	    bucket DateTime('UTC'), symbol LowCardinality(String),
//	    open  SimpleAggregateFunction(anyLast, Decimal(38,8)),
//	    high  SimpleAggregateFunction(max,     Decimal(38,8)),
//	    low   SimpleAggregateFunction(min,     Decimal(38,8)),
//	    close SimpleAggregateFunction(anyLast, Decimal(38,8)),
//	    volume Decimal(38,8), quote_volume Decimal(38,8),
//	    trade_count UInt64
//	) ENGINE = SummingMergeTree
//	  PARTITION BY toYYYYMM(bucket)
//	  ORDER BY (symbol, bucket)
//	  TTL bucket + toIntervalYear(5)   -- §16.2/§19.12 aggregate retention
//
// Interval label → table-name mapping is NOT the raw label: the DDL
// lowercases day/week ("1D"→ohlcv_1d, "1W"→ohlcv_1w) and spells the
// month "1mo" ("1M"→ohlcv_1mo) — ohlcvTables below owns that mapping;
// a rename is a one-line fix there.
//
// The `ohlcv` name in the same database is a UNION ALL VIEW over the
// twelve tables (SELECT-only — inserts must target the per-interval
// tables, which is what this store does). Materialized views /
// real-time aggregation over raw ticks (e.g. volume_stats_hourly_mv)
// are owned by the schema track; this file is the store + read
// projection only.
package analytics

import (
	"context"
	"fmt"
	"strings"
	"time"

	"exchange/pkg/decimal"
)

// OHLCVAggregateRetentionYears is the §16.2/§19.12 retention contract for
// the aggregated candle tables (vs the 90-day raw-tick TTL in ticks.go).
const OHLCVAggregateRetentionYears = 5

// persistedIntervalLabels is the §16.2 twelve-timeframe persisted set —
// every canonical marketdata/ohlcv interval except the memory-only 1s.
// Duplicated deliberately: archive_ch.go (marketdata/ohlcv) imports this
// package, so analytics cannot import marketdata/ohlcv back — that would
// be a compile cycle. The lists are both derived from the same §16.2
// contract and TestOHLCVTableForAllIntervals pins the count.
var persistedIntervalLabels = map[string]bool{
	"1m": true, "5m": true, "15m": true, "30m": true,
	"1h": true, "2h": true, "4h": true, "6h": true, "8h": true,
	"1D": true, "1W": true, "1M": true,
}

// PersistedInterval reports whether label is one of the 12 persisted
// candle timeframes (spec §16.2). "1s" is memory-only upstream and any
// other label is invalid — both return false.
func PersistedInterval(label string) bool { return persistedIntervalLabels[label] }

// PersistedIntervalLabels returns the canonical 12 labels in width order —
// for error payloads and docs.
func PersistedIntervalLabels() []string {
	return []string{"1m", "5m", "15m", "30m", "1h", "2h", "4h",
		"6h", "8h", "1D", "1W", "1M"}
}

// ohlcvTables maps each canonical interval label to its deployed CH
// table. The schema track's spelling differs from the label for the
// calendar buckets: 1D→ohlcv_1d, 1W→ohlcv_1w, 1M→ohlcv_1mo.
var ohlcvTables = map[string]string{
	"1m": "ohlcv_1m", "5m": "ohlcv_5m", "15m": "ohlcv_15m",
	"30m": "ohlcv_30m", "1h": "ohlcv_1h", "2h": "ohlcv_2h",
	"4h": "ohlcv_4h", "6h": "ohlcv_6h", "8h": "ohlcv_8h",
	"1D": "ohlcv_1d", "1W": "ohlcv_1w", "1M": "ohlcv_1mo",
}

// OHLCVTableFor resolves the table for a persisted interval label.
// Returns ("", false) for 1s / unknown labels — callers map that to a
// request error rather than writing to a fabricated table.
func OHLCVTableFor(interval string) (string, bool) {
	t, ok := ohlcvTables[interval]
	return t, ok
}

// ohlcvColumns is the insert/select tuple for the deployed per-interval
// column set (bucket is the schema's name for open_time).
const ohlcvColumns = "bucket, symbol, open, high, low, close, " +
	"volume, quote_volume, trade_count"

// CandleRow is one closed OHLCV bar for the CH projection — the columnar
// image of marketdata/ohlcv.Candle minus engine-only fields (FirstSeq /
// LastSeq / Closed / CarryForward / InstrumentID are hot-tier or PG-side
// concerns; the archive only ever sees Closed=true rows, and the schema
// does not carry instrument_id). TradeCount mirrors the engine's int64;
// the trade_count column is UInt64 — Insert/Query convert at the
// boundary (counts are never negative).
type CandleRow struct {
	Symbol      string // canonical "EUR/USD"
	Interval    string // "1m" … "1M" — must be in persistedIntervalLabels
	OpenTime    time.Time
	Open        decimal.Decimal
	High        decimal.Decimal
	Low         decimal.Decimal
	Close       decimal.Decimal
	Volume      decimal.Decimal
	QuoteVolume decimal.Decimal
	TradeCount  int64
}

// OHLCVQuery is one bounded read of the candle projection. Rows return
// oldest→newest (bucket ASC) over [From, To); After is the keyset
// continuation (bucket strictly greater).
type OHLCVQuery struct {
	Symbol   string
	Interval string
	From     time.Time  // inclusive; zero = unbounded
	To       time.Time  // exclusive; zero = unbounded
	After    *time.Time // keyset: bucket > After
	Limit    int        // 0 → store default (500, matching the kline route spec)
}

// OHLCVStore persists and reads the candle projection over the shared
// Conn seam.
type OHLCVStore struct{ conn Conn }

// NewOHLCVStore wires the store.
func NewOHLCVStore(conn Conn) *OHLCVStore { return &OHLCVStore{conn: conn} }

// Insert appends closed bars, grouped per interval so each batch hits
// its own SummingMergeTree table. A row whose Interval is not a
// persisted label fails the whole call — silently dropping a bar would
// leave the projection short (fail-closed, spec §2.7).
func (s *OHLCVStore) Insert(ctx context.Context, rows []CandleRow) error {
	if len(rows) == 0 {
		return nil
	}
	byTable := make(map[string][]CandleRow, len(ohlcvTables))
	for i := range rows {
		r := rows[i]
		tbl, ok := OHLCVTableFor(r.Interval)
		if !ok {
			return fmt.Errorf("ohlcv insert: interval %q is not a persisted timeframe", r.Interval)
		}
		byTable[tbl] = append(byTable[tbl], r)
	}
	for tbl, group := range byTable {
		batch, err := s.conn.PrepareBatch(ctx,
			"INSERT INTO "+tbl+" ("+ohlcvColumns+")")
		if err != nil {
			return fmt.Errorf("ohlcv batch prepare %s: %w", tbl, err)
		}
		for i := range group {
			r := group[i]
			err := batch.Append(
				r.OpenTime.UTC(), r.Symbol,
				r.Open, r.High, r.Low, r.Close,
				r.Volume, r.QuoteVolume, uint64(r.TradeCount))
			if err != nil {
				_ = batch.Abort()
				return fmt.Errorf("ohlcv batch append %s %s %s: %w",
					r.Symbol, r.Interval, r.OpenTime.UTC().Format(time.RFC3339), err)
			}
		}
		if err := batch.Send(); err != nil {
			return fmt.Errorf("ohlcv batch send %s (%d rows): %w", tbl, len(group), err)
		}
	}
	return nil
}

// Query reads one page of closed bars for (symbol, interval) ordered by
// bucket ASC — the same read shape marketapi.Klines serves from the PG
// fx_klines hot tier, so clients can page identically against either
// surface. FINAL collapses unmerged SummingMergeTree partials so a
// re-archived bar never double-counts volume.
func (s *OHLCVStore) Query(ctx context.Context, q OHLCVQuery) ([]CandleRow, error) {
	tbl, ok := OHLCVTableFor(q.Interval)
	if !ok {
		return nil, fmt.Errorf("ohlcv query: interval %q is not a persisted timeframe", q.Interval)
	}
	limit := q.Limit
	if limit <= 0 {
		limit = 500
	}
	var b strings.Builder
	b.WriteString("SELECT " + ohlcvColumns + " FROM " + tbl + " FINAL" +
		" WHERE symbol = ?")
	args := []any{q.Symbol}
	if !q.From.IsZero() {
		b.WriteString(" AND bucket >= ?")
		args = append(args, q.From.UTC())
	}
	if !q.To.IsZero() {
		b.WriteString(" AND bucket < ?")
		args = append(args, q.To.UTC())
	}
	if q.After != nil {
		b.WriteString(" AND bucket > ?")
		args = append(args, q.After.UTC())
	}
	b.WriteString(" ORDER BY bucket ASC LIMIT ?")
	args = append(args, limit)

	rows, err := s.conn.Query(ctx, b.String(), args...)
	if err != nil {
		return nil, fmt.Errorf("ohlcv query %s %s: %w", q.Symbol, q.Interval, err)
	}
	defer rows.Close()

	out := make([]CandleRow, 0, limit)
	for rows.Next() {
		var r CandleRow
		var tradeCount uint64   // trade_count is UInt64 in the schema
		r.Interval = q.Interval // per-interval tables carry no label column
		if err := rows.Scan(&r.OpenTime, &r.Symbol, &r.Open, &r.High,
			&r.Low, &r.Close, &r.Volume, &r.QuoteVolume, &tradeCount); err != nil {
			return nil, fmt.Errorf("ohlcv row scan: %w", err)
		}
		r.TradeCount = int64(tradeCount)
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ohlcv rows: %w", err)
	}
	return out, nil
}
