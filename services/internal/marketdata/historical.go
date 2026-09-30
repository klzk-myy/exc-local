// Phase-23 Task 23.3.1 — historical trade-record reads off the
// ClickHouse `trades` projection (spec §10.3/§16.1, §24 #236):
//
//	GET /api/v1/history/trades/{symbol}?from=&to=&limit=&cursor=
//
// The `trades` table (deploy/clickhouse/schema/002_trades.sql) is the
// enriched trade-fill record: same JetStream `trades` wire source as
// the public `ticks` tape plus the order-level lineage
// (instrument_id, maker/taker account ids, buy/sell order ids). It is
// partitioned by event month —
//
//	ENGINE = ReplacingMergeTree(ver)
//	PARTITION BY toYYYYMM(ts)
//	ORDER BY (symbol, trade_id, event_seq)
//	TTL ts + INTERVAL 5 YEAR        -- MiFID II record-keeping, §19.12
//
// — so date-range queries prune whole partitions rather than scanning
// (the Task 23.3.1 "partitioned by date" contract; the klines side is
// served by analytics.OHLCVStore over the per-interval SummingMergeTree
// partitions, and raw ticks by analytics.TickStore — both existing
// Phase-20 stores; this file fills the missing trades-record read).
//
// Privacy: the row carries participant fields the public tape must
// never emit — masking lives in history_guards.go (Task 23.3.8) and
// the handler decides which tiers see them. Fail-closed per §2.7: a
// store outage surfaces SERVICE_DEGRADED at the handler, never an
// empty page that looks like quiet tape.
package marketdata

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"

	"exchange/pkg/decimal"
)

// HistoryTradesTable is the schema-002 table name. analytics.TableTrades
// carries the same constant, but this package cannot import
// internal/analytics (analytics → funding → marketdata would cycle), so
// the table name is duplicated deliberately — a schema-track rename
// touches both files (deploy/clickhouse/schema/002_trades.sql is the
// source of truth).
const HistoryTradesTable = "trades"

// tradesReadColumns is the select projection over schema 002 (ver is
// engine-internal).
const tradesReadColumns = "ts, symbol, trade_id, instrument_id, " +
	"maker_account_id, taker_account_id, buy_order_id, sell_order_id, " +
	"price, qty, aggressor_side, event_seq, shard_id"

// tradesFrom is FINAL-deduped so a replayed fill can never double
// (same contract as ticks.go's `ticks FINAL`).
const tradesFrom = HistoryTradesTable + " FINAL"

// HistoryTrade is one row of the enriched CH `trades` projection —
// the same fill the public `ticks` tape carries plus participant
// lineage. Participant fields are retained in the row so the
// pre-open/public masking guards (history_guards.go) can mask what the
// wire tier is not entitled to see.
type HistoryTrade struct {
	ShardID       uint32
	Symbol        string
	TradeID       uint64
	InstrumentID  int64 // 0 = order index had no mapping (002 comment)
	EventSeq      uint64
	Price         decimal.Decimal
	Qty           decimal.Decimal // base currency — the `qty` column
	AggressorSide string          // 'BUY'|'SELL'|'UNKNOWN'
	Participants  ParticipantFields
	Ts            time.Time // event time, UTC
}

// EventTime implements ParticipantCarrier.
func (t *HistoryTrade) EventTime() time.Time { return t.Ts }

// ParticipantView implements ParticipantCarrier.
func (t *HistoryTrade) ParticipantView() *ParticipantFields { return &t.Participants }

// TradeHistoryCursor is the (ts, trade_id) keyset position for the
// newest-first stream — identical shape to analytics.TickCursor, kept
// separate so the trades contract can drift independently (e.g. a
// future is_bust lineage column joins the keyset).
type TradeHistoryCursor struct {
	Ts      time.Time
	TradeID uint64
}

// TradeHistoryQuery is one bounded read of the trades projection.
// Rows return newest first (ts DESC, trade_id DESC); From inclusive,
// To exclusive; a zero bound is unbounded.
type TradeHistoryQuery struct {
	Symbol string
	From   time.Time           // inclusive lower bound on ts (zero = unbounded)
	To     time.Time           // exclusive upper bound on ts (zero = unbounded)
	After  *TradeHistoryCursor // keyset: continue strictly before this position
	Limit  int                 // 0 → store default (1000)
}

// CHQuerier is the narrow ClickHouse read surface the store needs —
// analytics.Conn (and the driver's own driver.Conn) satisfy it; tests
// substitute in-memory fakes. Declared locally rather than reusing
// analytics.Conn because importing that package would close an
// analytics → funding → marketdata import cycle.
type CHQuerier interface {
	Query(ctx context.Context, query string, args ...any) (driver.Rows, error)
}

// TradeHistoryStore is the read side over the shared CH connection
// seam — the ingest side is the Task 20.3.1 ETL consumer writing via
// analytics.TradeRowValues.
type TradeHistoryStore struct {
	conn CHQuerier
}

// NewTradeHistoryStore wires the store over an open CHQuerier
// (analytics.Dial's Conn satisfies the seam).
func NewTradeHistoryStore(conn CHQuerier) *TradeHistoryStore {
	return &TradeHistoryStore{conn: conn}
}

// Query reads one keyset page, newest first. A full page implies a
// possible successor, matching the §8.8 envelope contract the handler
// emits — same paging rule as analytics.TickStore.Query.
func (s *TradeHistoryStore) Query(ctx context.Context, q TradeHistoryQuery) ([]*HistoryTrade, error) {
	limit := q.Limit
	if limit <= 0 {
		limit = 1000
	}
	var b strings.Builder
	b.WriteString("SELECT " + tradesReadColumns + " FROM " + tradesFrom +
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
		return nil, fmt.Errorf("trades history query %s: %w", q.Symbol, err)
	}
	defer rows.Close()

	out := make([]*HistoryTrade, 0, limit)
	for rows.Next() {
		var t HistoryTrade
		if err := rows.Scan(&t.Ts, &t.Symbol, &t.TradeID, &t.InstrumentID,
			&t.Participants.MakerAccountID, &t.Participants.TakerAccountID,
			&t.Participants.BuyOrderID, &t.Participants.SellOrderID,
			&t.Price, &t.Qty, &t.AggressorSide, &t.EventSeq, &t.ShardID); err != nil {
			return nil, fmt.Errorf("trades history row scan: %w", err)
		}
		out = append(out, &t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("trades history rows: %w", err)
	}
	return out, nil
}
