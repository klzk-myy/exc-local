package analytics

// Phase-20 Task 20.3.1 + 20.3.11 — ClickHouse ETL write path.
//
// Two feeds converge here:
//   - JetStream `trades`/`analytics` streams (Bridge-published flatbuffers
//     wire events) -> ticks/trades tables via the Consumer in consumer.go.
//   - PostgreSQL `ledger_entries` ⋈ `journal_entries` (migrations 102/036)
//     -> income_ledger via the batch poller below. A batch poll is used
//     (not CDC): the read-replica-friendly cursor is restart-safe in the
//     spool's meta keyspace and at-least-once replay is absorbed by
//     ReplacingMergeTree(ver) — documented §16.7 read projection.
//
// Durability contract (§2.7 fail-closed): InsertWithSpool returns nil only
// when the batch is durable — either committed to ClickHouse or synced to
// the local spool. If both fail the error propagates and the caller must
// not ack the source messages (NATS redelivery is the recovery path).

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"
)

// tableColumns is the single source of truth for insert column order —
// the drain path rebuilds INSERT statements purely from this registry, so
// table names can never arrive as untrusted SQL (no injection surface).
// `ver` is always the last column on dedup tables and is stamped by the
// Ingester, not by callers.
var tableColumns = map[string][]string{
	"ticks": {
		"ts", "symbol", "price", "quantity", "side",
		"trade_id", "event_seq", "shard_id", "ver",
	},
	"trades": {
		"ts", "symbol", "trade_id", "instrument_id",
		"maker_account_id", "taker_account_id",
		"buy_order_id", "sell_order_id",
		"price", "qty", "aggressor_side", "event_seq", "shard_id", "ver",
	},
	"income_ledger": {
		"posted_at", "account_id", "currency", "income_type", "entry_type",
		"amount", "ledger_entry_id", "journal_entry_id", "reference_id",
		"description", "ver",
	},
	// SummingMergeTree delta table — no ver; callers pass FULL rows (all
	// 8 columns — see VolumeStatCounters). '1h' rows arrive from the
	// volume_stats_hourly_mv; '1d' counters-only rows from the consumer's
	// order/fill counting and VolumeStatsStore.SyncOrdersCount.
	"volume_stats": {
		"symbol", "bucket_start", "granularity", "volume", "quote_volume",
		"trade_count", "orders_submitted", "orders_filled",
	},
	// Forward-declared sibling tables — registered so InsertWithSpool /
	// the drain path can carry them if a later writer funnels through
	// the Ingester; Task 20.3.4/20.3.9 stores currently write directly.
	"account_pnl": {
		"account_id", "instrument_id", "symbol", "day",
		"realized", "unrealized", "fees", "ts", "ver",
	},
	"tca_results": {
		"account_id", "instrument_id", "symbol", "fill_id", "order_id",
		"exec_price", "arrival_price", "session_vwap", "ecb_fix",
		"slip_arrival_bps", "slip_vwap_bps", "slip_fix_bps",
		"price_improvement_delta", "period", "period_start", "ts", "ver",
	},
}

// TableColumns exposes the registry (tests + cmd wiring).
func TableColumns(table string) ([]string, bool) {
	c, ok := tableColumns[table]
	return c, ok
}

// Options tunes the ingest path. Zero values get the §16.6 defaults.
type Options struct {
	// InsertTimeout bounds one PrepareBatch+Send; >5s is the §16.6
	// divert-to-spool trigger. Default 5s.
	InsertTimeout time.Duration
	// DrainBatch is the recovery drain batch size (Task 20.3.11: 10,000).
	DrainBatch int
	// RecoverPoll is the idle interval between Ping/drain attempts.
	RecoverPoll time.Duration
	// DrainBackoffMin/Max bound the exponential pacing between drained
	// batches (100ms doubling to 5s avoids a post-outage stampede).
	DrainBackoffMin time.Duration
	DrainBackoffMax time.Duration
	// IncomePollInterval is the GL projection cadence (default 60s).
	IncomePollInterval time.Duration
	// IncomePageSize bounds one ledger_entries page (default 5000).
	IncomePageSize int
	// SpoolDir / SpoolMaxBytes configure the local disk spool.
	// EXC_CH_SPOOL_DIR env overrides; default DefaultSpoolDir.
	SpoolDir      string
	SpoolMaxBytes uint64
}

// OptionsFromEnv resolves EXC_CH_SPOOL_DIR over the defaults.
func OptionsFromEnv() Options {
	o := DefaultOptions()
	if d := strings.TrimSpace(os.Getenv("EXC_CH_SPOOL_DIR")); d != "" {
		o.SpoolDir = d
	}
	return o
}

// DefaultOptions returns the §16.6/Task-20.3.11 canonical values.
func DefaultOptions() Options {
	return Options{
		InsertTimeout:      5 * time.Second,
		DrainBatch:         10_000,
		RecoverPoll:        5 * time.Second,
		DrainBackoffMin:    100 * time.Millisecond,
		DrainBackoffMax:    5 * time.Second,
		IncomePollInterval: 60 * time.Second,
		IncomePageSize:     5_000,
		SpoolDir:           DefaultSpoolDir,
		SpoolMaxBytes:      DefaultSpoolMaxBytes,
	}
}

// Ingester owns the ClickHouse write path: columnar batch insert with
// automatic diversion to the disk spool, plus the paced recovery drain.
type Ingester struct {
	conn  Conn
	spool *Spool
	m     *IngestMetrics
	opts  Options
	log   *slog.Logger

	down atomic.Bool // CH currently unreachable (for Recover transitions)
}

// NewIngester wires conn + spool. spool may be nil ONLY for tests that
// never take the divert path; production construction uses OpenSpool.
func NewIngester(conn Conn, spool *Spool, opts Options, m *IngestMetrics, log *slog.Logger) *Ingester {
	if m == nil {
		m = NewIngestMetrics()
	}
	if log == nil {
		log = slog.Default()
	}
	if opts.InsertTimeout <= 0 {
		opts.InsertTimeout = 5 * time.Second
	}
	if opts.DrainBatch <= 0 {
		opts.DrainBatch = 10_000
	}
	if opts.RecoverPoll <= 0 {
		opts.RecoverPoll = 5 * time.Second
	}
	if opts.DrainBackoffMin <= 0 {
		opts.DrainBackoffMin = 100 * time.Millisecond
	}
	if opts.DrainBackoffMax <= 0 {
		opts.DrainBackoffMax = 5 * time.Second
	}
	return &Ingester{conn: conn, spool: spool, m: m, opts: opts, log: log}
}

// Metrics exposes the panel for the daemon's /metrics handler.
func (g *Ingester) Metrics() *IngestMetrics { return g.m }

// Spool exposes the disk buffer (cursor storage, stats).
func (g *Ingester) Spool() *Spool { return g.spool }

// nextVer stamps the ReplacingMergeTree version: ingest wall-clock nanos.
// Monotonic-enough per process; replayed batches carry strictly newer
// vers than the originals they replace.
func nextVer() uint64 { return uint64(time.Now().UnixNano()) }

// insertBatch performs the columnar PrepareBatch insert. rows carry the
// full column set including ver.
func (g *Ingester) insertBatch(ctx context.Context, table string, rows [][]any) error {
	cols, ok := tableColumns[table]
	if !ok {
		return fmt.Errorf("analytics: unknown table %q", table)
	}
	b, err := g.conn.PrepareBatch(ctx,
		fmt.Sprintf("INSERT INTO %s (%s)", table, strings.Join(cols, ", ")))
	if err != nil {
		return fmt.Errorf("analytics: prepare %s: %w", table, err)
	}
	for _, r := range rows {
		if len(r) != len(cols) {
			_ = b.Abort()
			return fmt.Errorf("analytics: %s row has %d cols, want %d", table, len(r), len(cols))
		}
		if err := b.Append(r...); err != nil {
			_ = b.Abort()
			return fmt.Errorf("analytics: append %s: %w", table, err)
		}
	}
	if err := b.Send(); err != nil {
		return fmt.Errorf("analytics: send %s: %w", table, err)
	}
	return nil
}

// InsertWithSpool is the only ingest entrypoint. Rows are passed WITHOUT
// the trailing `ver` column — the ingester stamps it so the same version
// lands in CH or in the spool (deterministic replay). Returns nil when
// the batch is durable somewhere; a non-nil error means it is durable
// NOWHERE and the caller must retry/Nak rather than ack.
func (g *Ingester) InsertWithSpool(ctx context.Context, table string, rows [][]any) error {
	if len(rows) == 0 {
		return nil
	}
	cols, ok := tableColumns[table]
	if !ok {
		return fmt.Errorf("analytics: unknown table %q", table)
	}
	ver := nextVer()
	full := make([][]any, len(rows))
	for i, r := range rows {
		if len(r) == len(cols) { // caller already stamped ver — keep it
			full[i] = r
			continue
		}
		if len(r) != len(cols)-1 {
			return fmt.Errorf("analytics: %s row has %d cols, want %d (excl. ver)", table, len(r), len(cols)-1)
		}
		full[i] = append(append([]any(nil), r...), ver)
	}

	iCtx, cancel := context.WithTimeout(ctx, g.opts.InsertTimeout)
	start := time.Now()
	err := g.insertBatch(iCtx, table, full)
	cancel()
	if err == nil {
		g.m.observeInsertNanos(time.Since(start))
		g.m.incBatchInserted()
		g.m.incRowsInserted(len(full))
		return nil
	}
	g.m.incInsertErrors()
	g.log.Warn("clickhouse insert failed; diverting to spool",
		"table", table, "rows", len(full), "err", err)

	if g.spool == nil {
		return fmt.Errorf("analytics: insert %s failed and no spool configured: %w", table, err)
	}
	evicted, serr := g.spool.Append(table, full)
	if serr != nil {
		// Nothing durable — the caller MUST NOT ack the source.
		return fmt.Errorf("analytics: insert %s failed (%v) and spool append failed: %w", table, err, serr)
	}
	if evicted > 0 {
		g.m.incSpoolDropped(evicted)
		g.log.Error("spool over capacity bound; dropped oldest entries",
			"evicted", evicted, "table", table) // L1 alert surface (§16.6 F15)
	}
	g.m.incSpooled(len(full))
	g.refreshSpoolGauges()
	return nil
}

// refreshSpoolGauges republishes spool depth/bytes (cheap; dev-scale scan
// avoided — pebble metrics + the entry counter maintained by the Spool).
func (g *Ingester) refreshSpoolGauges() {
	if g.spool == nil {
		return
	}
	g.m.SetSpoolGauges(g.spool.Entries(), g.spool.SizeBytes())
}

// DrainOnce pulls up to DrainBatch spool entries (oldest first), groups
// them per table, inserts each group, and deletes only the entries whose
// insert acked. Returns (entriesDrained, error). An insert error stops
// the round — unacked entries stay for the next attempt (at-least-once;
// downstream ReplacingMergeTree collapses any partial re-insert).
func (g *Ingester) DrainOnce(ctx context.Context) (int, error) {
	if g.spool == nil {
		return 0, nil
	}
	entries, err := g.spool.Peek(g.opts.DrainBatch)
	if err != nil {
		return 0, err
	}
	if len(entries) == 0 {
		return 0, nil
	}
	// Group by table preserving FIFO order inside each group.
	byTable := make(map[string][]spoolEntry)
	order := make([]string, 0, 4)
	for _, e := range entries {
		if _, seen := byTable[e.batch.Table]; !seen {
			order = append(order, e.batch.Table)
		}
		byTable[e.batch.Table] = append(byTable[e.batch.Table], e)
	}

	drained := 0
	for _, table := range order {
		group := byTable[table]
		var rows [][]any
		for _, e := range group {
			rows = append(rows, e.batch.Rows...)
		}
		iCtx, cancel := context.WithTimeout(ctx, g.opts.InsertTimeout)
		err := g.insertBatch(iCtx, table, rows)
		cancel()
		if err != nil {
			return drained, fmt.Errorf("analytics: drain insert %s: %w", table, err)
		}
		keys := make([][]byte, 0, len(group))
		for _, e := range group {
			keys = append(keys, e.key)
		}
		if err := g.spool.Delete(keys); err != nil {
			return drained, fmt.Errorf("analytics: drain delete: %w", err)
		}
		drained += len(group)
		g.m.incDrained(len(group), len(rows))
	}
	g.refreshSpoolGauges()
	return drained, nil
}

// Recover is the long-running drain loop: when CH pings healthy it drains
// in DrainBatch-sized rounds with exponential pacing between rounds; when
// it doesn't, it sleeps RecoverPoll and retries. Run it in a goroutine;
// it exits on ctx cancellation.
func (g *Ingester) Recover(ctx context.Context) {
	backoff := g.opts.DrainBackoffMin
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		if err := g.conn.Ping(ctx); err != nil {
			if !g.down.Swap(true) {
				g.log.Warn("clickhouse unreachable; spooling until recovery", "err", err)
			}
			g.sleep(ctx, g.opts.RecoverPoll)
			continue
		}
		if g.down.Swap(false) {
			g.m.incRecoveries()
			g.log.Info("clickhouse recovered; draining spool")
		}
		n, err := g.DrainOnce(ctx)
		if err != nil {
			g.log.Warn("spool drain interrupted", "err", err, "drained", n)
			g.sleep(ctx, g.opts.RecoverPoll)
			continue
		}
		if n == 0 {
			backoff = g.opts.DrainBackoffMin
			g.sleep(ctx, g.opts.RecoverPoll)
			continue
		}
		// Paced drain: 100ms doubling to 5s between rounds so a recovered
		// cluster isn't stampeded by the backlog (Task 20.3.11).
		g.sleep(ctx, backoff)
		backoff *= 2
		if backoff > g.opts.DrainBackoffMax {
			backoff = g.opts.DrainBackoffMax
		}
	}
}

func (g *Ingester) sleep(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

// ---- PostgreSQL GL -> income_ledger batch projection (§16.7) ----

// PGQuerier is the narrow pgx surface the projection needs; *pgxpool.Pool
// satisfies it.
type PGQuerier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// mapIncomeType projects the wallet-ledger entry type onto the §16.7
// income taxonomy. Unmapped types pass through verbatim — the CH column
// is LowCardinality(String), and future enum additions (REBATE,
// DUST_CONVERT, FUNDING_FEE) land queryable without a code change.
func mapIncomeType(entryType string) string {
	switch entryType {
	case "FEE":
		return "COMMISSION"
	case "ROLLOVER":
		return "SWAP_ROLLOVER"
	case "ADJUSTMENT":
		return "NBP_ADJUSTMENT"
	default:
		return entryType
	}
}

const incomeSelectSQL = `
SELECT id, account_id, currency, direction, amount,
       entry_type, reference_id, journal_entry_id, posted_at, description
FROM ledger_entries
WHERE id > $1
ORDER BY id ASC
LIMIT $2`

// PollIncomeOnce pages ledger_entries newer than cursor into
// income_ledger and returns the next cursor. The caller persists the
// cursor (the daemon stores it in the spool meta keyspace so a restart
// resumes, not replays — ReplacingMergeTree absorbs replays anyway).
// P&L projection lag (§24 #68) is measured against the newest posted_at.
func (g *Ingester) PollIncomeOnce(ctx context.Context, q PGQuerier, cursor uint64, limit int) (uint64, int, error) {
	if limit <= 0 {
		limit = 5_000
	}
	rows, err := q.Query(ctx, incomeSelectSQL, cursor, limit)
	if err != nil {
		return cursor, 0, fmt.Errorf("income poll: %w", err)
	}
	defer rows.Close()

	var out [][]any
	var maxID uint64 = cursor
	var maxPosted time.Time
	for rows.Next() {
		var (
			id, acct, ref, journal uint64
			currency, dir, etype   string
			amount                 decimal.Decimal
			posted                 time.Time
			descr                  *string
			journalID, refID       *int64
		)
		// journal_entry_id / reference_id are nullable.
		var id64, acct64 int64
		if err := rows.Scan(&id64, &acct64, &currency, &dir, &amount,
			&etype, &refID, &journalID, &posted, &descr); err != nil {
			return cursor, 0, fmt.Errorf("income scan: %w", err)
		}
		id, acct = uint64(id64), uint64(acct64)
		if journalID != nil {
			journal = uint64(*journalID)
		}
		if refID != nil {
			ref = uint64(*refID)
		}
		// Signed amount from the wallet's perspective (§16.7): DEBIT =
		// value into the wallet (+), CREDIT = value out (−).
		if dir == "CREDIT" {
			amount = amount.Neg()
		}
		d := ""
		if descr != nil {
			d = *descr
		}
		out = append(out, []any{
			posted, acct, currency, mapIncomeType(etype), etype,
			amount, id, journal, ref, d,
		})
		if id > maxID {
			maxID = id
		}
		if posted.After(maxPosted) {
			maxPosted = posted
		}
	}
	if err := rows.Err(); err != nil {
		return cursor, 0, fmt.Errorf("income rows: %w", err)
	}
	if len(out) == 0 {
		return cursor, 0, nil
	}
	if err := g.InsertWithSpool(ctx, "income_ledger", out); err != nil {
		return cursor, 0, err
	}
	g.m.incIncomeRows(len(out))
	if !maxPosted.IsZero() {
		g.m.SetIncomeLag(time.Since(maxPosted))
	}
	return maxID, len(out), nil
}

// PollIncome is the periodic projection loop (default cadence 60s). It
// resumes from the spool-persisted cursor and persists the new cursor
// only after the CH insert (or spool divert) is durable.
func (g *Ingester) PollIncome(ctx context.Context, q PGQuerier) {
	interval := g.opts.IncomePollInterval
	if interval <= 0 {
		interval = time.Minute
	}
	poll := func() {
		cursor := uint64(0)
		if g.spool != nil {
			if c, err := g.spool.Cursor(); err == nil {
				cursor = c
			}
		}
		for {
			next, n, err := g.PollIncomeOnce(ctx, q, cursor, g.opts.IncomePageSize)
			if err != nil {
				g.log.Warn("income projection poll failed", "err", err)
				return
			}
			if n == 0 {
				return
			}
			cursor = next
			if g.spool != nil {
				if err := g.spool.SetCursor(cursor); err != nil {
					g.log.Warn("income cursor persist failed", "err", err)
				}
			}
			if n < g.opts.IncomePageSize {
				return // caught up
			}
		}
	}
	poll() // first pass immediately at boot
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			poll()
		}
	}
}

// TickRowValues builds the ticks row (excluding ver) from a decoded
// TradeFill. price/qty arrive as int64 scaled 1e8 (wire convention) and
// are converted exactly via decimal exponent shift — never float division.
func TickRowValues(ts time.Time, symbol string, priceE8, qtyE8 int64, side string,
	tradeID, eventSeq uint64, shardID uint32) []any {
	return []any{
		ts, symbol,
		decimal.New(priceE8, -8), decimal.New(qtyE8, -8),
		side, tradeID, eventSeq, shardID,
	}
}

// TradeRowValues builds the trades row (excluding ver). instrumentID /
// makerAcct / takerAcct come from the consumer's order index — 0 when
// unresolved (fail-closed: never guess an account). NAMING CAVEAT: the
// wire exposes buy/sell order ids, not maker/taker flags, so
// maker_account_id carries the buy-side account and taker_account_id the
// sell-side (schema 002 comment; the per-tier rollup treats them
// symmetrically so attribution stays exact).
func TradeRowValues(ts time.Time, symbol string, tradeID uint64, instrumentID uint32,
	makerAcct, takerAcct int64, buyOrderID, sellOrderID uint64,
	priceE8, qtyE8 int64, aggressor string, eventSeq uint64, shardID uint32) []any {
	return []any{
		ts, symbol, tradeID, int64(instrumentID), makerAcct, takerAcct,
		buyOrderID, sellOrderID,
		decimal.New(priceE8, -8), decimal.New(qtyE8, -8),
		aggressor, eventSeq, shardID,
	}
}

// VolumeStatCounters builds a '1d'-granularity counters-only delta row
// for volume_stats — same shape as VolumeStatsStore.SyncOrdersCount
// writes (volume/quote_volume/trade_count all zero so the '1h' volume
// axes are untouched; FillRates reads these). NOTE: volume_stats has no
// `ver` column — rows must carry all 8 columns or InsertWithSpool would
// mistake the row for a missing-ver form.
func VolumeStatCounters(symbol string, dayBucket time.Time, submitted, filled uint64) []any {
	return []any{
		symbol, dayOnlyUTC(dayBucket), GranularityDay,
		decimal.Zero, decimal.Zero, uint64(0), submitted, filled,
	}
}
