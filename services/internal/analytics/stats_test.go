// Unit tests for the Task 20.3.5 volume/stats store — shares the
// pnlFake* conn seam from pnl_test.go, plus the EXC_CH_TEST=1 gated
// live round-trip.
package analytics

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"exchange/pkg/decimal"
)

var statsFixedNow = time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)

type fakeAccountTiers struct {
	cats map[int64]string
	err  error
	got  []int64
}

func (f *fakeAccountTiers) ClientCategories(_ context.Context, ids []int64) (map[int64]string, error) {
	f.got = ids
	return f.cats, f.err
}

func TestVolumeQueryFiltersAndMaps(t *testing.T) {
	conn := &pnlFakeConn{queryRows: [][]any{
		// (symbol, bucket, trade_count, volume, quote_volume)
		{"EUR/USD",
			time.Date(2026, 9, 20, 14, 0, 0, 0, time.UTC),
			uint64(12),
			decimal.RequireFromString("1500.5"),
			decimal.RequireFromString("1650.2")},
	}}
	s := NewVolumeStatsStore(conn, nil)
	from := time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC)
	to := from.Add(24 * time.Hour)
	rows, err := s.Volume(context.Background(), from, to, "EUR/USD", "1h", 100)
	if err != nil {
		t.Fatalf("volume: %v", err)
	}
	q := conn.queries[0]
	for _, frag := range []string{
		"FROM " + TableVolumeStats,
		"granularity = '1h'",
		"bucket_start >= ? AND bucket_start < ?",
		"AND symbol = ?", "sum(trade_count)", "sum(volume)",
		"LIMIT ?",
	} {
		if !strings.Contains(q, frag) {
			t.Fatalf("query missing %q: %s", frag, q)
		}
	}
	// args: from, to, symbol, limit
	args := conn.queryArgs[0]
	if len(args) != 4 || args[2] != "EUR/USD" || args[3] != 100 {
		t.Fatalf("args = %v", args)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d", len(rows))
	}
	r := rows[0]
	if r.Symbol != "EUR/USD" || r.Granularity != "1h" || r.TradeCount != 12 {
		t.Fatalf("row = %+v", r)
	}
	if !r.Volume.Equal(decimal.RequireFromString("1500.5")) ||
		!r.QuoteVolume.Equal(decimal.RequireFromString("1650.2")) {
		t.Fatalf("volumes = %s / %s", r.Volume, r.QuoteVolume)
	}
}

func TestVolumeDayRollupUsesToStartOfDay(t *testing.T) {
	conn := &pnlFakeConn{}
	s := NewVolumeStatsStore(conn, nil)
	if _, err := s.Volume(context.Background(),
		statsFixedNow, statsFixedNow.Add(24*time.Hour), "", "1d", 0); err != nil {
		t.Fatalf("volume: %v", err)
	}
	if !strings.Contains(conn.queries[0], "toStartOfDay(bucket_start)") {
		t.Fatalf("1d rollup missing toStartOfDay: %s", conn.queries[0])
	}
	// Day rollup still reads only '1h' rows — '1d' rows carry the
	// order-counter deltas with zero volume.
	if !strings.Contains(conn.queries[0], "granularity = '1h'") {
		t.Fatalf("1d rollup must source '1h' rows: %s", conn.queries[0])
	}
	if len(conn.queryArgs[0]) != 2 {
		t.Fatalf("unfiltered day query args = %v", conn.queryArgs[0])
	}
	// Empty granularity defaults to the stored hour buckets — no
	// toStartOfDay wrap.
	if _, err := s.Volume(context.Background(),
		statsFixedNow, statsFixedNow.Add(24*time.Hour), "", "", 0); err != nil {
		t.Fatalf("volume: %v", err)
	}
	if strings.Contains(conn.queries[1], "toStartOfDay") ||
		strings.Contains(conn.queries[1], "symbol = ?") ||
		strings.Contains(conn.queries[1], "LIMIT") {
		t.Fatalf("unfiltered hour query wrong: %s", conn.queries[1])
	}
	// Bad granularity fails closed.
	if _, err := s.Volume(context.Background(),
		statsFixedNow, statsFixedNow.Add(time.Hour), "", "5m", 0); err == nil {
		t.Fatal("granularity=5m must fail")
	}
	if _, err := NewVolumeStatsStore(nil, nil).Volume(context.Background(),
		statsFixedNow, statsFixedNow.Add(time.Hour), "", "", 0); err == nil {
		t.Fatal("nil conn must fail closed")
	}
}

func TestFillRatesReadCounterRows(t *testing.T) {
	conn := &pnlFakeConn{queryRows: [][]any{
		// (symbol, orders_submitted, orders_filled)
		{"EUR/USD", uint64(100), uint64(97)},
		{"USD/JPY", uint64(10), uint64(0)},
	}}
	s := NewVolumeStatsStore(conn, nil)
	from := time.Date(2026, 9, 21, 9, 30, 0, 0, time.UTC)
	to := from.Add(2 * time.Hour)
	rates, err := s.FillRates(context.Background(), from, to)
	if err != nil {
		t.Fatalf("fill rates: %v", err)
	}
	q := conn.queries[0]
	for _, frag := range []string{
		"granularity = '1d'", "sum(orders_submitted)", "sum(orders_filled)",
		"toStartOfDay(?)", "GROUP BY symbol",
	} {
		if !strings.Contains(q, frag) {
			t.Fatalf("query missing %q: %s", frag, q)
		}
	}
	if len(rates) != 2 {
		t.Fatalf("rates = %d: %+v", len(rates), rates)
	}
	if rates[0].Symbol != "EUR/USD" || rates[0].OrdersSubmitted != 100 ||
		rates[0].OrdersFilled != 97 {
		t.Fatalf("rate0 = %+v", rates[0])
	}
	if got := rates[0].Rate(); got == nil || got.String() != "0.97" {
		t.Fatalf("rate = %v, want 0.97", got)
	}
	if rates[1].Symbol != "USD/JPY" || rates[1].OrdersFilled != 0 {
		t.Fatalf("rate1 = %+v", rates[1])
	}
	if rates[1].Rate() == nil || rates[1].Rate().String() != "0" {
		t.Fatalf("10 submitted / 0 filled = rate 0, got %v", rates[1].Rate())
	}
	// Zero submissions → nil rate (no fabricated 0%).
	if got := (FillRate{Symbol: "X", OrdersSubmitted: 0}).Rate(); got != nil {
		t.Fatalf("zero submissions must give nil rate, got %v", got)
	}
	if _, err := NewVolumeStatsStore(nil, nil).FillRates(context.Background(),
		from, to); !errors.Is(err, ErrNoClickHouse) {
		t.Fatalf("nil conn err = %v", err)
	}
	conn2 := &pnlFakeConn{queryErr: errSentinel{}}
	if _, err := NewVolumeStatsStore(conn2, nil).FillRates(context.Background(),
		from, to); err == nil {
		t.Fatal("query error must propagate")
	}
}

func TestSyncOrdersCountWritesDayDelta(t *testing.T) {
	conn := &pnlFakeConn{}
	s := NewVolumeStatsStore(conn, nil)
	s.Now = func() time.Time { return statsFixedNow }
	bucket := time.Date(2026, 9, 21, 10, 45, 0, 0, time.UTC) // mid-day
	if err := s.SyncOrdersCount(context.Background(), "EUR/USD", bucket, 60, 58); err != nil {
		t.Fatalf("sync: %v", err)
	}
	pb := conn.lastBatch()
	if pb == nil || !pb.batch.sent {
		t.Fatal("counter batch not sent")
	}
	for _, col := range []string{"symbol", "bucket_start", "granularity",
		"orders_submitted", "orders_filled"} {
		if !strings.Contains(pb.query, col) {
			t.Fatalf("insert missing %q: %s", col, pb.query)
		}
	}
	row := pb.batch.rows[0]
	// (symbol, bucket_start, granularity, volume, quote_volume,
	//  trade_count, orders_submitted, orders_filled)
	if row[0] != "EUR/USD" || row[2] != GranularityDay {
		t.Fatalf("row head = %v", row[:3])
	}
	if d := row[1].(time.Time); !d.Equal(dayOnlyUTC(bucket)) {
		t.Fatalf("bucket not day-truncated: %v", d)
	}
	if row[6] != uint64(60) || row[7] != uint64(58) {
		t.Fatalf("counter deltas = %v %v", row[6], row[7])
	}

	// Validation + no-op paths.
	if err := s.SyncOrdersCount(context.Background(), "", bucket, 1, 1); err == nil {
		t.Fatal("empty symbol must fail")
	}
	batchesBefore := len(conn.batches)
	if err := s.SyncOrdersCount(context.Background(), "EUR/USD", bucket, 0, 0); err != nil {
		t.Fatalf("zero-delta sync: %v", err)
	}
	if len(conn.batches) != batchesBefore {
		t.Fatal("zero-delta sync must not write")
	}
	// Zero bucketStart falls back to the injected clock.
	if err := s.SyncOrdersCount(context.Background(), "GBP/USD", time.Time{}, 1, 1); err != nil {
		t.Fatalf("default bucket sync: %v", err)
	}
	last := conn.lastBatch().batch.rows[0]
	if d := last[1].(time.Time); !d.Equal(dayOnlyUTC(statsFixedNow)) {
		t.Fatalf("default bucket = %v, want %v", d, dayOnlyUTC(statsFixedNow))
	}
	// nil conn fails closed.
	if err := NewVolumeStatsStore(nil, nil).SyncOrdersCount(context.Background(),
		"EUR/USD", bucket, 1, 1); !errors.Is(err, ErrNoClickHouse) {
		t.Fatalf("nil conn err = %v", err)
	}
}

func TestTradesPerTierFoldsAccountsIntoCategories(t *testing.T) {
	conn := &pnlFakeConn{queryRows: [][]any{
		// (symbol, account_id, side_fills, volume)
		{"EUR/USD", int64(11), uint64(5), decimal.RequireFromString("100")},
		{"EUR/USD", int64(12), uint64(3), decimal.RequireFromString("60")},
		{"EUR/USD", int64(21), uint64(2), decimal.RequireFromString("40")},
		{"USD/JPY", int64(99), uint64(1), decimal.RequireFromString("10")},
		{"USD/JPY", int64(0), uint64(1), decimal.RequireFromString("5")}, // unresolved
	}}
	tiers := &fakeAccountTiers{cats: map[int64]string{
		11: "RETAIL", 12: "RETAIL", 21: "PROFESSIONAL"}}
	s := NewVolumeStatsStore(conn, tiers)
	rows, err := s.TradesPerTier(context.Background(),
		statsFixedNow, statsFixedNow.Add(24*time.Hour))
	if err != nil {
		t.Fatalf("tier trades: %v", err)
	}
	q := conn.queries[0]
	for _, frag := range []string{
		TableTrades + " FINAL", "maker_account_id", "taker_account_id",
		"UNION ALL", "GROUP BY symbol, account_id",
	} {
		if !strings.Contains(q, frag) {
			t.Fatalf("query missing %q: %s", frag, q)
		}
	}
	if len(conn.queryArgs[0]) != 4 {
		t.Fatalf("window args = %v", conn.queryArgs[0])
	}
	if len(rows) != 3 {
		t.Fatalf("rows = %d: %+v", len(rows), rows)
	}
	// Sorted (symbol, tier): EUR/USD PROFESSIONAL, EUR/USD RETAIL,
	// USD/JPY UNKNOWN.
	if rows[0].Symbol != "EUR/USD" || rows[0].Tier != "PROFESSIONAL" || rows[0].TradeCount != 2 {
		t.Fatalf("row0 = %+v", rows[0])
	}
	if rows[1].Tier != "RETAIL" || rows[1].TradeCount != 8 ||
		!rows[1].Volume.Equal(decimal.RequireFromString("160")) {
		t.Fatalf("row1 = %+v", rows[1])
	}
	// USD/JPY folds accounts 99 + 0 into UNKNOWN (unmapped + unresolved).
	if rows[2].Symbol != "USD/JPY" || rows[2].Tier != TierUnknown ||
		rows[2].TradeCount != 2 || !rows[2].Volume.Equal(decimal.RequireFromString("15")) {
		t.Fatalf("row2 = %+v", rows[2])
	}
	// All five distinct account ids are looked up (incl. 0).
	if len(tiers.got) != 5 {
		t.Fatalf("lookup ids = %v", tiers.got)
	}
}

func TestTradesPerTierRequiresLookup(t *testing.T) {
	s := NewVolumeStatsStore(&pnlFakeConn{}, nil)
	_, err := s.TradesPerTier(context.Background(), statsFixedNow,
		statsFixedNow.Add(time.Hour))
	if !errors.Is(err, ErrTierLookupNotWired) {
		t.Fatalf("err = %v, want ErrTierLookupNotWired", err)
	}
	if _, err := NewVolumeStatsStore(nil, &fakeAccountTiers{}).TradesPerTier(
		context.Background(), statsFixedNow, statsFixedNow.Add(time.Hour)); err == nil {
		t.Fatal("nil conn must fail closed")
	}
}

func TestTradesPerTierLookupErrorPropagates(t *testing.T) {
	conn := &pnlFakeConn{queryRows: [][]any{
		{"EUR/USD", int64(11), uint64(5), decimal.RequireFromString("1")},
	}}
	s := NewVolumeStatsStore(conn, &fakeAccountTiers{err: errors.New("pg down")})
	if _, err := s.TradesPerTier(context.Background(), statsFixedNow,
		statsFixedNow.Add(time.Hour)); err == nil ||
		!strings.Contains(err.Error(), "pg down") {
		t.Fatalf("err = %v", err)
	}
}

// ---------------------------------------------------------------------------
// Gated live test (EXC_CH_TEST=1). volume_stats/trades are fed by the
// hourly MV over ticks — the test inserts a tick through TickStore and
// reads the rollup back; the counter round-trip exercises SyncOrdersCount
// directly (that IS the write seam — counters do not derive from ticks).
// ---------------------------------------------------------------------------

func TestCHVolumeStatsRoundTrip(t *testing.T) {
	conn := chTestConn(t)
	ctx := context.Background()
	ddls := map[string]string{}
	for _, tbl := range []string{TableVolumeStats, TableTrades} {
		ddl, err := ShowCreateTable(ctx, conn, tbl)
		if err != nil {
			t.Skipf("%s DDL not yet deployed (sibling schema pending): %v", tbl, err)
		}
		ddls[tbl] = ddl
	}
	// The deployed dev tables may predate the landed schema (004 wrote
	// bucket_start/granularity/orders_* later) — skip rather than fail
	// until that DDL is applied.
	for _, col := range []string{"`bucket_start`", "`granularity`",
		"`orders_submitted`", "`orders_filled`", "`volume`"} {
		if !strings.Contains(ddls[TableVolumeStats], col) {
			t.Skipf("deployed volume_stats predates landed schema (missing %s):\n%s",
				col, ddls[TableVolumeStats])
		}
	}
	tradesDDL := ddls[TableTrades]
	if !strings.Contains(tradesDDL, "`qty`") {
		t.Skipf("deployed trades predates landed schema (missing `qty`):\n%s", tradesDDL)
	}
	s := NewVolumeStatsStore(conn, nil)
	s.Now = func() time.Time { return statsFixedNow }

	// Counter seam round-trip: '1d' delta rows.
	sym := "TEST/PAIRSYN"
	if err := s.SyncOrdersCount(ctx, sym, time.Now().UTC(), 10, 7); err != nil {
		t.Fatalf("sync: %v", err)
	}
	today := dayOnlyUTC(time.Now())
	var rates []FillRate
	var err error
	for i := 0; i < 20; i++ {
		rates, err = s.FillRates(ctx, today, today.Add(24*time.Hour))
		if err != nil {
			t.Fatalf("fill rates: %v", err)
		}
		for _, r := range rates {
			if r.Symbol == sym && r.OrdersSubmitted >= 10 && r.OrdersFilled >= 7 {
				goto synced
			}
		}
		time.Sleep(150 * time.Millisecond)
	}
	t.Fatalf("counter row never visible: %+v", rates)
synced:

	// Tick → volume_stats via the hourly materialized view.
	ts := time.Now().UTC().Truncate(time.Millisecond)
	tick := Tick{ShardID: 0, Symbol: "TEST/PAIRVOL",
		TradeID: uint64(1<<63 - 2), EventSeq: 1,
		Price: decimal.MustFromString("9.99"), Qty: decimal.MustFromString("3"),
		Side: "BUY", Ts: ts}
	if err := NewTickStore(conn).Insert(ctx, []Tick{tick}); err != nil {
		t.Fatalf("tick insert: %v", err)
	}
	hour := ts.Truncate(time.Hour)
	var rows []VolumeRow
	for i := 0; i < 30; i++ {
		rows, err = s.Volume(ctx, hour, hour.Add(time.Hour), "TEST/PAIRVOL", "1h", 10)
		if err != nil {
			t.Fatalf("volume: %v", err)
		}
		if len(rows) == 1 && rows[0].TradeCount >= 1 {
			break
		}
		time.Sleep(200 * time.Millisecond) // MV/async_insert visibility lag
	}
	if len(rows) != 1 {
		t.Fatalf("volume rows = %d: %+v", len(rows), rows)
	}
	if rows[0].TradeCount < 1 ||
		!rows[0].Volume.GreaterThanOrEqual(decimal.RequireFromString("3")) {
		t.Fatalf("volume row = %+v", rows[0])
	}
}
