// Phase-23 Task 23.3.10 — taker-volume & positioning ratios
// (spec §10.8 item 2, §24 #359).
//
//   - `GET /api/v1/market/taker-volume?symbol=&interval=` — taker
//     buy/sell notional ratio bucketed by interval, computed from the
//     ClickHouse `trades` projection (schema 002 — the fills feed, so
//     the sums reconcile to fills for the window by construction).
//   - `GET /api/v1/market/positioning?symbol=` — long/short account
//     ratio + top-position concentration bands from the §5.13 positions
//     cohort (PositionCohort in sentiment.go — the delayed read seam is
//     SentimentProducer.LatestCohort).
//   - The same TakerFlow window aggregate feeds the sentiment@ frame's
//     taker_flow section (Task 23.3.6).
//
// Guards shared with Task 23.3.6: SentimentPublicationDelay (5m) and
// SentimentMinCohortAccounts (100) gate every surface; the cohort basis
// for taker data is distinct maker+taker accounts over the window
// (union of trades.maker_account_id / taker_account_id — per the schema
// 002 naming caveat those columns are buy-side/sell-side accounts, so
// the union is the true distinct-participant count). No per-account
// field ever leaves this file.
package marketdata

import (
	"context"
	"errors"
	"fmt"
	"time"

	"exchange/pkg/decimal"
)

// TakerFlow is the anonymized taker buy/sell aggregate over one window.
// Notional = Σ price×qty (quote ccy) split by the wire aggressor flag;
// Volume is the same split in base units. Accounts is the distinct
// participant count — the cohort basis for the 100-account floor.
type TakerFlow struct {
	BuyNotional     decimal.Decimal
	SellNotional    decimal.Decimal
	UnknownNotional decimal.Decimal // aggressor_side 'UNKNOWN' — disclosed, not dropped
	BuyVolume       decimal.Decimal
	SellVolume      decimal.Decimal
	Trades          int64
	Accounts        int64
}

// BuySellRatio returns buy_notional / sell_notional — nil when the sell
// side is empty (an infinite/zero-division ratio is uninformative and a
// guessed number would mislead).
func (f TakerFlow) BuySellRatio() *decimal.Decimal {
	if !f.SellNotional.IsPositive() {
		return nil
	}
	r := f.BuyNotional.Div(f.SellNotional)
	return &r
}

// Total reconciles the bucket to the window's fills: buy+sell+unknown
// notional is the full aggressor split (callers assert it equals the
// fill notional for the same range).
func (f TakerFlow) Total() decimal.Decimal {
	return f.BuyNotional.Add(f.SellNotional).Add(f.UnknownNotional)
}

// TakerFlowBucket is one interval cell of the taker-volume series.
type TakerFlowBucket struct {
	BucketStart time.Time
	Flow        TakerFlow
}

// TakerFlowSource is the single-window read seam (Task 23.3.6 WS frame).
type TakerFlowSource interface {
	TakerFlow(ctx context.Context, symbol string, from, to time.Time) (TakerFlow, error)
}

// TakerFlowSeriesSource is the bucketed read seam (Task 23.3.10 REST).
type TakerFlowSeriesSource interface {
	Buckets(ctx context.Context, symbol string, from, to time.Time,
		bucketSec int) ([]TakerFlowBucket, error)
}

// ErrNoTakerFlowStore reports a store built on a nil connection — the
// handler maps it to SERVICE_DEGRADED rather than serving an empty page.
var ErrNoTakerFlowStore = errors.New("marketdata: taker-flow store not wired")

// TakerFlowStore reads the ClickHouse `trades` projection through the
// shared CHQuerier seam (historical.go — analytics cannot be imported
// here; the schema-002 table/column contract is deploy/clickhouse/
// schema/002_trades.sql). Queries are bounded by the caller's ctx —
// handlers apply the HistoryQueryGuard 10s timeout.
type TakerFlowStore struct {
	conn CHQuerier
}

// NewTakerFlowStore wires the store over an open CHQuerier.
func NewTakerFlowStore(conn CHQuerier) *TakerFlowStore {
	return &TakerFlowStore{conn: conn}
}

// takerSumCols is the aggressor-split select projection shared by the
// window and bucketed queries.
const takerSumCols = `
	sumIf(toDecimal128(price * qty, 8), aggressor_side = 'BUY')   AS buy_notional,
	sumIf(toDecimal128(price * qty, 8), aggressor_side = 'SELL')  AS sell_notional,
	sumIf(toDecimal128(price * qty, 8), aggressor_side NOT IN ('BUY','SELL')) AS unknown_notional,
	sumIf(qty, aggressor_side = 'BUY')  AS buy_volume,
	sumIf(qty, aggressor_side = 'SELL') AS sell_volume,
	count() AS trades`

// TakerFlow implements TakerFlowSource — one window aggregate plus the
// distinct-participant cohort count for the same window.
func (s *TakerFlowStore) TakerFlow(ctx context.Context, symbol string,
	from, to time.Time) (TakerFlow, error) {
	if s == nil || s.conn == nil {
		return TakerFlow{}, ErrNoTakerFlowStore
	}
	var f TakerFlow
	rows, err := s.conn.Query(ctx,
		`SELECT `+takerSumCols+`
		 FROM `+tradesFrom+`
		 WHERE symbol = ? AND ts >= ? AND ts < ?`,
		symbol, from.UTC(), to.UTC())
	if err != nil {
		return f, fmt.Errorf("marketdata: taker flow %s: %w", symbol, err)
	}
	var trades uint64
	if rows.Next() {
		if err := rows.Scan(&f.BuyNotional, &f.SellNotional,
			&f.UnknownNotional, &f.BuyVolume, &f.SellVolume, &trades); err != nil {
			rows.Close()
			return f, fmt.Errorf("marketdata: taker flow scan: %w", err)
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return f, fmt.Errorf("marketdata: taker flow rows: %w", err)
	}
	rows.Close()
	f.Trades = int64(trades)

	accts, err := s.cohort(ctx, symbol, from, to)
	if err != nil {
		return f, err
	}
	f.Accounts = accts
	return f, nil
}

// Buckets implements TakerFlowSeriesSource — per-bucket flow sums and a
// per-bucket distinct-account cohort, merged in-process. bucketSec is a
// ClickHouse INTERVAL width; the caller bounds [from,to) so the delay
// horizon is enforced by construction.
func (s *TakerFlowStore) Buckets(ctx context.Context, symbol string,
	from, to time.Time, bucketSec int) ([]TakerFlowBucket, error) {
	if s == nil || s.conn == nil {
		return nil, ErrNoTakerFlowStore
	}
	if bucketSec <= 0 {
		return nil, errors.New("marketdata: bucket width must be positive")
	}
	flows, err := s.bucketFlows(ctx, symbol, from, to, bucketSec)
	if err != nil {
		return nil, err
	}
	cohorts, err := s.bucketCohorts(ctx, symbol, from, to, bucketSec)
	if err != nil {
		return nil, err
	}
	for i := range flows {
		flows[i].Flow.Accounts = cohorts[flows[i].BucketStart]
	}
	return flows, nil
}

func (s *TakerFlowStore) bucketFlows(ctx context.Context, symbol string,
	from, to time.Time, bucketSec int) ([]TakerFlowBucket, error) {
	rows, err := s.conn.Query(ctx,
		`SELECT toStartOfInterval(ts, INTERVAL ? SECOND) AS bucket, `+
			takerSumCols+`
		 FROM `+tradesFrom+`
		 WHERE symbol = ? AND ts >= ? AND ts < ?
		 GROUP BY bucket ORDER BY bucket ASC`,
		bucketSec, symbol, from.UTC(), to.UTC())
	if err != nil {
		return nil, fmt.Errorf("marketdata: taker buckets %s: %w", symbol, err)
	}
	defer rows.Close()
	var out []TakerFlowBucket
	for rows.Next() {
		var b TakerFlowBucket
		var trades uint64
		if err := rows.Scan(&b.BucketStart, &b.Flow.BuyNotional,
			&b.Flow.SellNotional, &b.Flow.UnknownNotional,
			&b.Flow.BuyVolume, &b.Flow.SellVolume, &trades); err != nil {
			return nil, fmt.Errorf("marketdata: taker bucket scan: %w", err)
		}
		b.BucketStart = b.BucketStart.UTC()
		b.Flow.Trades = int64(trades)
		out = append(out, b)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("marketdata: taker bucket rows: %w", err)
	}
	return out, nil
}

// bucketCohorts returns the distinct maker∪taker account count per
// bucket — the per-cell privacy cohort.
func (s *TakerFlowStore) bucketCohorts(ctx context.Context, symbol string,
	from, to time.Time, bucketSec int) (map[time.Time]int64, error) {
	rows, err := s.conn.Query(ctx, `
		SELECT bucket, uniqExact(account_id) AS accts FROM (
			SELECT toStartOfInterval(ts, INTERVAL ? SECOND) AS bucket,
			       maker_account_id AS account_id
			FROM `+tradesFrom+`
			WHERE symbol = ? AND ts >= ? AND ts < ?
			UNION ALL
			SELECT toStartOfInterval(ts, INTERVAL ? SECOND) AS bucket,
			       taker_account_id AS account_id
			FROM `+tradesFrom+`
			WHERE symbol = ? AND ts >= ? AND ts < ?
		) GROUP BY bucket`, bucketSec, symbol, from.UTC(), to.UTC(),
		bucketSec, symbol, from.UTC(), to.UTC())
	if err != nil {
		return nil, fmt.Errorf("marketdata: taker cohort %s: %w", symbol, err)
	}
	defer rows.Close()
	out := map[time.Time]int64{}
	for rows.Next() {
		var (
			b time.Time
			n uint64
		)
		if err := rows.Scan(&b, &n); err != nil {
			return nil, fmt.Errorf("marketdata: taker cohort scan: %w", err)
		}
		out[b.UTC()] = int64(n)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("marketdata: taker cohort rows: %w", err)
	}
	return out, nil
}

// cohort is the whole-window distinct-participant count used by
// TakerFlow (single-window variant of bucketCohorts).
func (s *TakerFlowStore) cohort(ctx context.Context, symbol string,
	from, to time.Time) (int64, error) {
	rows, err := s.conn.Query(ctx, `
		SELECT uniqExact(account_id) FROM (
			SELECT maker_account_id AS account_id
			FROM `+tradesFrom+`
			WHERE symbol = ? AND ts >= ? AND ts < ?
			UNION ALL
			SELECT taker_account_id AS account_id
			FROM `+tradesFrom+`
			WHERE symbol = ? AND ts >= ? AND ts < ?
		)`, symbol, from.UTC(), to.UTC(), symbol, from.UTC(), to.UTC())
	if err != nil {
		return 0, fmt.Errorf("marketdata: taker window cohort %s: %w", symbol, err)
	}
	defer rows.Close()
	var n uint64
	if rows.Next() {
		if err := rows.Scan(&n); err != nil {
			return 0, fmt.Errorf("marketdata: taker window cohort scan: %w", err)
		}
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("marketdata: taker window cohort rows: %w", err)
	}
	return int64(n), nil
}
