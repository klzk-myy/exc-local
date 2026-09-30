// Task 20.3.2 — tick history store (spec §16.1).
//
// Every trade tick lands in the ClickHouse `ticks` table owned by the
// schema track (deploy/clickhouse/schema/, database exchange_analytics).
// Deployed DDL (verified via SHOW CREATE TABLE against dev):
//
//	ticks (
//	    ts DateTime64(3,'UTC'), symbol LowCardinality(String),
//	    price Decimal(38,8), quantity Decimal(38,8),
//	    side LowCardinality(String), trade_id UInt64,
//	    event_seq UInt64, shard_id UInt32, ver UInt64
//	) ENGINE = ReplacingMergeTree(ver)
//	  PARTITION BY toYYYYMM(ts)
//	  ORDER BY (symbol, trade_id, event_seq)
//	  TTL ts + toIntervalDay(90)      -- spec §16.1 raw-tick retention
//
// The 5-year retention contract applies to aggregated ohlcv/volume_stats
// and the `trades` table, not raw ticks (Phase-20 plan remediation #35
// follow-on). Compression is the MergeTree default codec: no column
// carries an explicit CODEC(...) clause ⇒ LZ4.
//
// Drift hatch: if the schema track renames the table or a column, the
// const block below is the entire fix surface.
package analytics

import (
	"context"
	"fmt"
	"strings"
	"time"

	"exchange/pkg/decimal"
)

// TickTable is the raw-tick table name in exchange_analytics.
const TickTable = "ticks"

// TickRetentionDays is the spec §16.1 raw-tick TTL contract encoded by
// the deployed DDL as `TTL ts + toIntervalDay(90)`. Gated tests assert it
// via ShowCreateTable; it is exported so the ops runbook and the test
// share one constant.
const TickRetentionDays = 90

// tickColumns is the insert column tuple; tickReadColumns is the select
// projection (ver is engine-internal). `ver` is the ReplacingMergeTree
// version — we supply ingest time in millis so a later re-ingest wins
// the dedup merge.
const tickColumns = "ts, symbol, price, quantity, side, " +
	"trade_id, event_seq, shard_id, ver"

const tickReadColumns = "ts, symbol, price, quantity, side, " +
	"trade_id, event_seq, shard_id"

// ticksFrom is the read source: FINAL collapses any not-yet-merged
// ReplacingMergeTree duplicate (replay/re-ingest) so reads never double
// a tick. Swap to plain TickTable only if the schema track switches
// engines.
const ticksFrom = TickTable + " FINAL"

// Tick is one public tape row stored in ClickHouse — the analytics
// projection of a core trade fill republished on the JetStream "trades"
// stream (Phase-03 Bridge). Price/Qty stay decimal.Decimal end to end
// (spec §5.3 invariant 1).
type Tick struct {
	ShardID  uint32 // core shard that produced the fill
	Symbol   string // canonical "EUR/USD"
	TradeID  uint64
	EventSeq uint64 // engine per-symbol trade sequence
	Price    decimal.Decimal
	Qty      decimal.Decimal // base currency — the `quantity` column
	Side     string          // aggressor side: "BUY" | "SELL" ("" = unknown)
	Ts       time.Time       // event time, UTC
}

// TickCursor is the (ts, trade_id) keyset position for the DESC stream —
// the api layer renders it as the opaque §8.8 cursor token.
type TickCursor struct {
	Ts      time.Time
	TradeID uint64
}

// TickQuery is one bounded read of the tick archive. Rows return newest
// first ordered by (ts DESC, trade_id DESC); From is inclusive, To
// exclusive; a zero bound is unbounded.
type TickQuery struct {
	Symbol string
	From   time.Time   // inclusive lower bound on ts (zero = unbounded)
	To     time.Time   // exclusive upper bound on ts (zero = unbounded)
	After  *TickCursor // keyset: continue strictly before this position
	Limit  int         // 0 → store default (100)
}

// TickStore is the columnar ingest + read path over the shared Conn seam.
type TickStore struct {
	conn Conn
	// Now supplies the `ver` version clock; injectable for tests.
	Now func() time.Time
}

// NewTickStore wires the store over an open Conn (Dial).
func NewTickStore(conn Conn) *TickStore {
	return &TickStore{conn: conn}
}

func (s *TickStore) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

// Insert writes one batch of ticks via PrepareBatch — the columnar
// binary path the §24 #65 50k-inserts/sec contract requires (async
// insert mode is enabled on the dial, ch.go). Empty input is a no-op.
// Callers (the JetStream ETL consumer) batch to their own cadence; this
// method sends exactly the slice it is given.
func (s *TickStore) Insert(ctx context.Context, ticks []Tick) error {
	if len(ticks) == 0 {
		return nil
	}
	batch, err := s.conn.PrepareBatch(ctx,
		"INSERT INTO "+TickTable+" ("+tickColumns+")")
	if err != nil {
		return fmt.Errorf("tick batch prepare: %w", err)
	}
	ver := uint64(s.now().UnixMilli())
	for i := range ticks {
		t := ticks[i]
		err := batch.Append(
			t.Ts.UTC(), t.Symbol, t.Price, t.Qty, t.Side,
			t.TradeID, t.EventSeq, t.ShardID, ver)
		if err != nil {
			_ = batch.Abort()
			return fmt.Errorf("tick batch append trade_id=%d: %w", t.TradeID, err)
		}
	}
	if err := batch.Send(); err != nil {
		return fmt.Errorf("tick batch send (%d rows): %w", len(ticks), err)
	}
	return nil
}

// Query reads one keyset page. The (ts, trade_id) tuple cursor is unique
// per row so pages never drift under concurrent inserts — OFFSET paging
// would. LIMIT+1 probing is unnecessary: a full page implies a possible
// successor, matching the §8.8 envelope contract the handler emits.
func (s *TickStore) Query(ctx context.Context, q TickQuery) ([]Tick, error) {
	limit := q.Limit
	if limit <= 0 {
		limit = 100
	}
	var b strings.Builder
	b.WriteString("SELECT " + tickReadColumns + " FROM " + ticksFrom +
		" WHERE symbol = ?")
	args := []any{q.Symbol}
	if !q.From.IsZero() {
		b.WriteString(" AND ts >= ?")
		args = append(args, q.From.UTC())
	}
	if !q.To.IsZero() {
		b.WriteString(" AND ts < ?")
		args = append(args, q.To.UTC())
	}
	if q.After != nil {
		b.WriteString(" AND (ts, trade_id) < (?, ?)")
		args = append(args, q.After.Ts.UTC(), q.After.TradeID)
	}
	b.WriteString(" ORDER BY ts DESC, trade_id DESC LIMIT ?")
	args = append(args, limit)

	rows, err := s.conn.Query(ctx, b.String(), args...)
	if err != nil {
		return nil, fmt.Errorf("tick query %s: %w", q.Symbol, err)
	}
	defer rows.Close()

	out := make([]Tick, 0, limit)
	for rows.Next() {
		var t Tick
		if err := rows.Scan(&t.Ts, &t.Symbol, &t.Price, &t.Qty, &t.Side,
			&t.TradeID, &t.EventSeq, &t.ShardID); err != nil {
			return nil, fmt.Errorf("tick row scan: %w", err)
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("tick rows: %w", err)
	}
	return out, nil
}

// ShowCreateTable returns the CREATE statement for table (single-column
// "statement" row). It is the schema-verification seam the gated tests
// use to assert the sibling DDL honours §16.1/§16.2 — a missing table
// surfaces as the driver's exception error, which callers translate to
// t.Skip.
func ShowCreateTable(ctx context.Context, conn Conn, table string) (string, error) {
	rows, err := conn.Query(ctx, "SHOW CREATE TABLE "+table)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var stmt string
	for rows.Next() {
		if err := rows.Scan(&stmt); err != nil {
			return "", err
		}
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	if stmt == "" {
		return "", fmt.Errorf("SHOW CREATE TABLE %s returned no rows", table)
	}
	return stmt, nil
}
