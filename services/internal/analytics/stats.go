// Task 20.3.5 — volume & trading-statistics store (spec §16, Phase-20).
//
// Read/write side over the ClickHouse projections created by the Task
// 20.3.1 schema track (deploy/clickhouse/schema/):
//
//	volume_stats (symbol LowCardinality(String),
//	              bucket_start DateTime('UTC'),
//	              granularity LowCardinality(String),   -- '1h' | '1d'
//	              volume Decimal(38,8),
//	              quote_volume Decimal(38,8),
//	              trade_count UInt64,
//	              orders_submitted UInt64,
//	              orders_filled UInt64)
//	      ENGINE = SummingMergeTree ORDER BY (symbol, bucket_start,
//	      granularity) — schema 004. Every insert is a DELTA row summed
//	      on merge, so writers append, never rewrite, and every read
//	      re-sums with GROUP BY. The hourly MV writes '1h' rows from
//	      ticks; SyncOrdersCount writes '1d' rows carrying the order
//	      counters (volume columns 0 — SummingMergeTree adds them).
//	      "Per day" volume is a toStartOfDay rollup of the '1h' rows.
//	trades (ts, symbol, trade_id, maker_account_id, taker_account_id,
//	        buy_order_id, sell_order_id, price, qty, event_seq, ...)
//	      ENGINE = ReplacingMergeTree(ver) — schema 002.
//
// Tier axis: "per account tier" resolves to accounts.client_category —
// the MiFID II categorization RETAIL|PROFESSIONAL|ELIGIBLE_COUNTERPARTY
// (migration 042), not kyc_tier, because the tier reports are regulatory
// client-segmentation surfaces. The CH trades projection carries
// account ids directly (maker_account_id = buy-side account,
// taker_account_id = sell-side per the 002 naming caveat), so the join
// is account_id → accounts.client_category — wired in-process through
// the AccountTierLookup seam since CH cannot reach PG. Canonical
// orchestrator SQL:
//
//	SELECT id, client_category::text FROM accounts WHERE id = ANY($1)
//
// Fill rate: orders_submitted / orders_filled are volume_stats columns
// fed by SyncOrdersCount — the ETL consumer / orchestrator owns the
// order-event counting; this store only records deltas and reads sums.
package analytics

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"exchange/pkg/decimal"
)

// Table names owned by the Task 20.3.1 schema track — constants so a
// rename touches one place.
const (
	TableVolumeStats = "volume_stats"
	TableTrades      = "trades"
)

// Granularity labels — stored rows carry either '1h' (ticks MV buckets)
// or '1d' (SyncOrdersCount counter deltas).
const (
	GranularityHour = "1h"
	GranularityDay  = "1d"
)

// TierUnknown buckets account ids whose client_category could not be
// resolved (account purged/closed, unresolved order index → account_id 0,
// or the PG lookup missed). Trades are never silently dropped — they
// surface under this label so per-tier totals reconcile to the venue
// total.
const TierUnknown = "UNKNOWN"

// VolumeRow is one (symbol, bucket) rollup. Granularity records the
// rollup width the caller asked for — "1h" rows are raw stored buckets,
// "1d" rows are day aggregates of them.
type VolumeRow struct {
	Symbol      string
	BucketStart time.Time // UTC; hour or day boundary per Granularity
	Granularity string    // GranularityHour | GranularityDay
	Volume      decimal.Decimal
	QuoteVolume decimal.Decimal
	TradeCount  int64
}

// FillRate is the per-symbol filled/submitted order ratio over a window.
type FillRate struct {
	Symbol          string
	OrdersSubmitted int64
	OrdersFilled    int64
}

// Rate returns orders_filled / orders_submitted as a Decimal, or nil when
// nothing was submitted — a 0% rate would fabricate a signal for a symbol
// that saw no flow.
func (f FillRate) Rate() *decimal.Decimal {
	if f.OrdersSubmitted <= 0 {
		return nil
	}
	r := decimal.NewFromInt(f.OrdersFilled).
		Div(decimal.NewFromInt(f.OrdersSubmitted))
	return &r
}

// TierTradeRow is one (symbol, client_category) rollup. TradeCount counts
// side-participations — each fill contributes once to the buyer's tier
// and once to the seller's — so venue-wide the sum is ≈ 2× executions
// (a self-trade legitimately contributes two). That is the meaningful
// segmentation axis: "how much of EUR/USD flow touched RETAIL accounts",
// not "how many executions".
type TierTradeRow struct {
	Symbol     string
	Tier       string // accounts.client_category (RETAIL|PROFESSIONAL|ECP)
	TradeCount int64
	Volume     decimal.Decimal // base-currency quantity
}

// AccountTierLookup resolves account ids to accounts.client_category.
// Implemented by a PG adapter at wiring time — see the package doc for
// the canonical query.
type AccountTierLookup interface {
	ClientCategories(ctx context.Context, accountIDs []int64) (map[int64]string, error)
}

// ErrTierLookupNotWired reports a TradesPerTier call on a store built
// without an AccountTierLookup.
var ErrTierLookupNotWired = errors.New("analytics: tier lookup not wired")

// VolumeStatsStore reads the volume_stats + trades projections and
// accepts order-counter deltas through SyncOrdersCount.
type VolumeStatsStore struct {
	conn  Conn
	tiers AccountTierLookup
	// Now supplies the default-bucket clock for SyncOrdersCount;
	// injectable for tests.
	Now func() time.Time
}

// NewVolumeStatsStore builds the store. tiers may be nil —
// TradesPerTier then fails closed (ErrTierLookupNotWired) instead of
// fabricating a single-bucket report.
func NewVolumeStatsStore(conn Conn, tiers AccountTierLookup) *VolumeStatsStore {
	return &VolumeStatsStore{conn: conn, tiers: tiers}
}

func (s *VolumeStatsStore) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

// ---------------------------------------------------------------------------
// Reads
// ---------------------------------------------------------------------------

// Volume returns per-symbol bucket rows over the half-open range
// [from, to). Only '1h' rows feed the volume axes — '1d' rows carry the
// SyncOrdersCount order counters with zero volume columns, so excluding
// them is what keeps trade_count/volume correct. symbol filters to one
// instrument ("" = all); granularity must be GranularityHour (stored
// buckets) or GranularityDay (toStartOfDay rollup) — an empty value
// defaults to GranularityHour. limit caps the row count (<=0 → no
// explicit LIMIT).
func (s *VolumeStatsStore) Volume(ctx context.Context, from, to time.Time,
	symbol, granularity string, limit int) ([]VolumeRow, error) {
	if s == nil || s.conn == nil {
		return nil, ErrNoClickHouse
	}
	bucketExpr := "bucket_start"
	switch granularity {
	case "", GranularityHour:
		granularity = GranularityHour
	case GranularityDay:
		bucketExpr = "toStartOfDay(bucket_start)"
	default:
		return nil, fmt.Errorf("analytics: granularity %q is not 1h|1d", granularity)
	}
	q := `SELECT symbol, ` + bucketExpr + ` AS bucket,
	             sum(trade_count)  AS trade_count,
	             sum(volume)       AS volume,
	             sum(quote_volume) AS quote_volume
	      FROM ` + TableVolumeStats + `
	      WHERE granularity = '` + GranularityHour + `'
	        AND bucket_start >= ? AND bucket_start < ?`
	args := []any{from.UTC(), to.UTC()}
	if symbol != "" {
		q += " AND symbol = ?"
		args = append(args, symbol)
	}
	q += ` GROUP BY symbol, bucket
	       ORDER BY bucket ASC, symbol ASC`
	if limit > 0 {
		q += " LIMIT ?"
		args = append(args, limit)
	}
	rows, err := s.conn.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("analytics: volume query: %w", err)
	}
	defer rows.Close()
	var out []VolumeRow
	for rows.Next() {
		var (
			r  VolumeRow
			tc uint64
		)
		if err := rows.Scan(&r.Symbol, &r.BucketStart, &tc,
			&r.Volume, &r.QuoteVolume); err != nil {
			return nil, fmt.Errorf("analytics: volume scan: %w", err)
		}
		r.Granularity = granularity
		r.TradeCount = int64(tc)
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("analytics: volume rows: %w", err)
	}
	return out, nil
}

// FillRates returns per-symbol filled/submitted counters over [from, to)
// summed from the '1d' counter rows SyncOrdersCount wrote. from snaps
// down to the UTC day (counters only exist at day grain); to stays
// half-open.
func (s *VolumeStatsStore) FillRates(ctx context.Context, from, to time.Time) ([]FillRate, error) {
	if s == nil || s.conn == nil {
		return nil, ErrNoClickHouse
	}
	rows, err := s.conn.Query(ctx,
		`SELECT symbol,
		        sum(orders_submitted) AS orders_submitted,
		        sum(orders_filled)    AS orders_filled
		 FROM `+TableVolumeStats+`
		 WHERE granularity = '`+GranularityDay+`'
		   AND bucket_start >= toStartOfDay(?)
		   AND bucket_start <  ?
		 GROUP BY symbol
		 ORDER BY symbol ASC`,
		from.UTC(), to.UTC())
	if err != nil {
		return nil, fmt.Errorf("analytics: fill rate query: %w", err)
	}
	defer rows.Close()
	out := make([]FillRate, 0, 32)
	for rows.Next() {
		var (
			f   FillRate
			sub uint64
			fil uint64
		)
		if err := rows.Scan(&f.Symbol, &sub, &fil); err != nil {
			return nil, fmt.Errorf("analytics: fill rate scan: %w", err)
		}
		f.OrdersSubmitted = int64(sub)
		f.OrdersFilled = int64(fil)
		out = append(out, f)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("analytics: fill rate rows: %w", err)
	}
	return out, nil
}

// TradesPerTier aggregates trade side-participations by
// (symbol, client_category) over [from, to). The CH side produces
// per-(symbol, account_id) counts from BOTH account columns of each fill
// (schema 002 caveat: maker_account_id is the buy-side account,
// taker_account_id the sell-side — attribution is symmetric either way);
// the AccountTierLookup then folds accounts into categories — unresolved
// ids (including 0) land in TierUnknown rather than being dropped.
func (s *VolumeStatsStore) TradesPerTier(ctx context.Context, from, to time.Time) ([]TierTradeRow, error) {
	if s == nil || s.conn == nil {
		return nil, ErrNoClickHouse
	}
	if s.tiers == nil {
		return nil, ErrTierLookupNotWired
	}
	rows, err := s.conn.Query(ctx,
		`SELECT symbol, account_id, count() AS side_fills, sum(qty) AS volume
		 FROM (
		     SELECT symbol, maker_account_id AS account_id, qty
		     FROM `+TableTrades+` FINAL
		     WHERE ts >= ? AND ts < ?
		     UNION ALL
		     SELECT symbol, taker_account_id AS account_id, qty
		     FROM `+TableTrades+` FINAL
		     WHERE ts >= ? AND ts < ?
		 )
		 GROUP BY symbol, account_id`,
		from.UTC(), to.UTC(), from.UTC(), to.UTC())
	if err != nil {
		return nil, fmt.Errorf("analytics: per-account trades query: %w", err)
	}
	type acctKey struct {
		symbol string
		acct   int64
	}
	perAcct := map[acctKey]TierTradeRow{}
	ids := map[int64]struct{}{}
	for rows.Next() {
		var (
			symbol    string
			accountID int64
			n         uint64
			vol       decimal.Decimal
		)
		if err := rows.Scan(&symbol, &accountID, &n, &vol); err != nil {
			rows.Close()
			return nil, fmt.Errorf("analytics: per-account trades scan: %w", err)
		}
		ids[accountID] = struct{}{}
		k := acctKey{symbol, accountID}
		prev := perAcct[k]
		prev.Symbol = symbol
		prev.TradeCount += int64(n)
		prev.Volume = prev.Volume.Add(vol)
		perAcct[k] = prev
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("analytics: per-account trades rows: %w", err)
	}
	rows.Close()

	idList := make([]int64, 0, len(ids))
	for id := range ids {
		idList = append(idList, id)
	}
	sort.Slice(idList, func(i, j int) bool { return idList[i] < idList[j] })
	cats, err := s.tiers.ClientCategories(ctx, idList)
	if err != nil {
		return nil, fmt.Errorf("analytics: account tier lookup: %w", err)
	}
	type tierKey struct {
		symbol, tier string
	}
	perTier := map[tierKey]TierTradeRow{}
	for k, v := range perAcct {
		tier, ok := cats[k.acct]
		if !ok || tier == "" {
			tier = TierUnknown
		}
		tk := tierKey{k.symbol, tier}
		prev := perTier[tk]
		prev.Symbol, prev.Tier = k.symbol, tier
		prev.TradeCount += v.TradeCount
		prev.Volume = prev.Volume.Add(v.Volume)
		perTier[tk] = prev
	}
	out := make([]TierTradeRow, 0, len(perTier))
	for _, r := range perTier {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Symbol != out[j].Symbol {
			return out[i].Symbol < out[j].Symbol
		}
		return out[i].Tier < out[j].Tier
	})
	return out, nil
}

// ---------------------------------------------------------------------------
// Counter feed seam
// ---------------------------------------------------------------------------

// SyncOrdersCount records a DELTA of submitted/filled order counters for
// (symbol, bucketStart) as a '1d' volume_stats row — the SummingMergeTree
// adds it onto the day bucket. Volume columns write 0 so the volume axes
// are untouched. This is the feed seam for FillRates: the orchestrator
// (or the Task 20.3.1 ingester) calls it once per flush with the delta
// since the previous call. bucketStart zero → the store clock's current
// instant (truncated to the day). Zero deltas are a no-op.
func (s *VolumeStatsStore) SyncOrdersCount(ctx context.Context, symbol string,
	bucketStart time.Time, submittedDelta, filledDelta int64) error {
	if s == nil || s.conn == nil {
		return ErrNoClickHouse
	}
	if symbol == "" {
		return errors.New("analytics: SyncOrdersCount requires a symbol")
	}
	if submittedDelta == 0 && filledDelta == 0 {
		return nil // zero deltas only add noise
	}
	if bucketStart.IsZero() {
		bucketStart = s.now()
	}
	batch, err := s.conn.PrepareBatch(ctx, "INSERT INTO "+TableVolumeStats+
		" (symbol, bucket_start, granularity, volume, quote_volume,"+
		" trade_count, orders_submitted, orders_filled)")
	if err != nil {
		return fmt.Errorf("analytics: order counter prepare batch: %w", err)
	}
	if err := batch.Append(
		symbol,
		dayOnlyUTC(bucketStart),
		GranularityDay,
		decimal.Zero, // volume
		decimal.Zero, // quote_volume
		uint64(0),    // trade_count — counters-only row
		uint64(submittedDelta),
		uint64(filledDelta),
	); err != nil {
		_ = batch.Abort()
		return fmt.Errorf("analytics: order counter batch append: %w", err)
	}
	if err := batch.Send(); err != nil {
		return fmt.Errorf("analytics: order counter batch send: %w", err)
	}
	return nil
}
