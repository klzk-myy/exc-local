// Tests for Task 20.3.12 — income ledger projection + read path.
//
// Unit tests use the incomeFake* seam (named distinctly from the
// sibling's pending fakeConn harness to avoid a collision when that file
// lands). Live tests skip unless EXC_CH_TEST=1 / EXC_PG_TEST=1.
package analytics

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/column"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/jackc/pgx/v5/pgxpool"

	"exchange/internal/db"
	"exchange/pkg/decimal"
)

// ---------------------------------------------------------------------------
// Fakes (driver.Conn / driver.Rows / driver.Batch)
// ---------------------------------------------------------------------------

type incomeFakeRows struct {
	cols []string
	data [][]any
	i    int
	err  error
}

func (r *incomeFakeRows) Next() bool {
	if r.i >= len(r.data) {
		return false
	}
	r.i++
	return true
}

func incomeAssign(dest, v any) error {
	switch d := dest.(type) {
	case *uint64:
		switch n := v.(type) {
		case uint64:
			*d = n
		case int64:
			*d = uint64(n)
		default:
			return fmt.Errorf("assign %T → *uint64", v)
		}
	case *int64:
		switch n := v.(type) {
		case int64:
			*d = n
		case uint64:
			*d = int64(n)
		default:
			return fmt.Errorf("assign %T → *int64", v)
		}
	case *string:
		s, ok := v.(string)
		if !ok {
			return fmt.Errorf("assign %T → *string", v)
		}
		*d = s
	case *time.Time:
		t, ok := v.(time.Time)
		if !ok {
			return fmt.Errorf("assign %T → *time.Time", v)
		}
		*d = t
	case *decimal.Decimal:
		switch x := v.(type) {
		case decimal.Decimal:
			*d = x
		case string:
			p, err := decimal.NewFromString(x)
			if err != nil {
				return err
			}
			*d = p
		default:
			return fmt.Errorf("assign %T → *decimal.Decimal", v)
		}
	default:
		return fmt.Errorf("incomeAssign: unsupported dest %T", dest)
	}
	return nil
}

func (r *incomeFakeRows) Scan(dest ...any) error {
	row := r.data[r.i-1]
	if len(dest) != len(row) {
		return fmt.Errorf("scan arity %d vs row %d", len(dest), len(row))
	}
	for i, d := range dest {
		if err := incomeAssign(d, row[i]); err != nil {
			return err
		}
	}
	return nil
}

func (r *incomeFakeRows) ScanStruct(any) error             { return errors.New("unimplemented") }
func (r *incomeFakeRows) ColumnTypes() []driver.ColumnType { return nil }
func (r *incomeFakeRows) Totals(...any) error              { return nil }
func (r *incomeFakeRows) Columns() []string                { return r.cols }
func (r *incomeFakeRows) Close() error                     { return nil }
func (r *incomeFakeRows) Err() error                       { return r.err }

type incomeFakeBatch struct {
	sql     string
	rows    [][]any
	sent    bool
	aborted bool
	sendErr error
}

func (b *incomeFakeBatch) Append(v ...any) error {
	b.rows = append(b.rows, v)
	return nil
}
func (b *incomeFakeBatch) Send() error {
	if b.sendErr != nil {
		return b.sendErr
	}
	b.sent = true
	return nil
}
func (b *incomeFakeBatch) Abort() error                  { b.aborted = true; return nil }
func (b *incomeFakeBatch) AppendStruct(any) error        { return errors.New("unimplemented") }
func (b *incomeFakeBatch) Column(int) driver.BatchColumn { return nil }
func (b *incomeFakeBatch) Flush() error                  { return nil }
func (b *incomeFakeBatch) IsSent() bool                  { return b.sent }
func (b *incomeFakeBatch) Rows() int                     { return len(b.rows) }
func (b *incomeFakeBatch) Columns() []column.Interface   { return nil }
func (b *incomeFakeBatch) Close() error                  { return nil }

// incomeFakeConn routes Query by SQL prefix so one fake serves the
// count + page pair, and captures PrepareBatch inserts.
type incomeFakeConn struct {
	batches    []*incomeFakeBatch
	prepareErr error
	queries    []string
	queryFn    func(query string, args ...any) (driver.Rows, error)
	queryErr   error
}

func (c *incomeFakeConn) Exec(context.Context, string, ...any) error { return nil }
func (c *incomeFakeConn) Query(_ context.Context, q string, args ...any) (driver.Rows, error) {
	c.queries = append(c.queries, q)
	if c.queryErr != nil {
		return nil, c.queryErr
	}
	if c.queryFn != nil {
		return c.queryFn(q, args...)
	}
	return &incomeFakeRows{}, nil
}
func (c *incomeFakeConn) PrepareBatch(_ context.Context, q string, _ ...driver.PrepareBatchOption) (driver.Batch, error) {
	if c.prepareErr != nil {
		return nil, c.prepareErr
	}
	b := &incomeFakeBatch{sql: q}
	c.batches = append(c.batches, b)
	return b, nil
}
func (c *incomeFakeConn) Ping(context.Context) error { return nil }
func (c *incomeFakeConn) Close() error               { return nil }

// ---------------------------------------------------------------------------
// Classifier table test — every §16.7 type + extended taxonomy + verbatim
// passthrough, through the real precedence order.
// ---------------------------------------------------------------------------

func TestClassifyIncomeTable(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name                         string
		entryType, jType, idem, desc string
		lines                        []string
		want                         string
	}{
		{"commission journal marker", "FEE", "FEE", "commission:77:9", "COMMISSION trade=77 tier=1", nil, IncomeCommission},
		{"commission acct code", "FEE", "FEE", "", "fee", []string{"2010_CUSTOMER_LIABILITY_USD", "4030_COMMISSION_REVENUE_USD"}, IncomeCommission},
		{"coarse FEE fallback", "FEE", "FEE", "", "fee", nil, IncomeCommission},
		{"maker rebate idem", "FEE", "FEE", "makerrebate:9:1", "MAKER_REBATE trade=9", nil, IncomeRebate},
		{"rebate acct code", "FEE", "FEE", "", "rebate", []string{"5100_LIQUIDITY_REBATE_EXPENSE_USD", "2100_CLIENT_COLLATERAL_USD"}, IncomeRebate},
		{"rollover entry type", "ROLLOVER", "EOD_ROLLOVER", "", "TOMNEXT EUR/USD LONG", nil, IncomeSwapRollover},
		{"rollover journal type only", "FEE", "EOD_ROLLOVER", "", "rollover", nil, IncomeSwapRollover},
		{"rollover acct code", "FEE", "FEE", "", "swap", []string{"4110_SWAP_MARKUP_REVENUE_USD"}, IncomeSwapRollover},
		{"nbp adjustment entry", "ADJUSTMENT", "ADJUSTMENT", "", "correction", nil, IncomeNBPAdjust},
		{"nbp restitution desc", "ADJUSTMENT", "ADJUSTMENT", "", "NBP_RESTITUTION_POSTED house P&L acct 1", nil, IncomeNBPAdjust},
		{"nbp fund-debit idem", "TRANSFER", "TRANSFER", "nbp:42", "retail NBP restitution acct 1 USD 10", nil, IncomeNBPAdjust},
		{"nbp acct code", "FEE", "FEE", "", "nbp", []string{"5200_NBP_RESTITUTION_EXPENSE_USD"}, IncomeNBPAdjust},
		{"dust convert desc", "TRANSFER", "TRANSFER", "", "DUST_CONVERT acct=1 EUR→USD mid=1.1", []string{"4400_CONVERSION_SPREAD_REVENUE_USD"}, IncomeDustConvert},
		{"dust convert idem", "TRANSFER", "TRANSFER", "dust:1:EUR:7", "sweep", nil, IncomeDustConvert},
		{"funding fee idem", "FEE", "FEE", "funding-fee:55", "funding withdrawal fee 5 USD (tier 2)", nil, IncomeFundingFee},
		{"funding fee acct", "FEE", "FEE", "", "wire fee", []string{"4200_FUNDING_FEE_REVENUE_USD"}, IncomeFundingFee},
		{"trading fee acct", "FEE", "FEE", "", "spread markup", []string{"4010_TRADING_FEE_REVENUE_USD"}, IncomeTradingFee},
		{"conversion acct", "FEE", "FEE", "", "fx conv", []string{"4400_CONVERSION_SPREAD_REVENUE_USD"}, IncomeConversionFee},
		{"swapfree 4020", "FEE", "FEE", "", "admin", []string{"4020_SWAPFREE_ADMIN_REVENUE_USD"}, IncomeSwapFreeAdminFee},
		{"swapfree 4300", "FEE", "FEE", "", "admin", []string{"4300_SWAPFREE_ADMIN_FEE_USD"}, IncomeSwapFreeAdminFee},
		{"inactivity 4500", "FEE", "FEE", "", "dormancy", []string{"4500_INACTIVITY_FEE_REVENUE_USD"}, IncomeInactivityFee},
		{"liquidation penalty 5010", "FEE", "LIQUIDATION", "", "liq pen", []string{"5010_LIQUIDATION_PENALTY_USD"}, IncomeLiquidationPen},
		// Account codes are gated to umbrella entry types: a TRADE_FILL
		// journal's 4010 fee line must NOT retag the client's fill row.
		{"fill stays verbatim despite 4010", "TRADE_FILL", "TRADE_FILL", "trade-fill:9",
			"trade 9 EUR/USD fill",
			[]string{"2010_CUSTOMER_LIABILITY_USD", "4010_TRADING_FEE_REVENUE_USD"}, "TRADE_FILL"},
		{"deposit stays verbatim despite codes", "DEPOSIT", "DEPOSIT", "",
			"wire", []string{"1010_NOSTRO_USD", "2010_CUSTOMER_LIABILITY_USD"}, "DEPOSIT"},
		{"liquidation verbatim despite 5010", "LIQUIDATION", "LIQUIDATION", "",
			"liq", []string{"5010_LIQUIDATION_PENALTY_USD"}, "LIQUIDATION"},
		{"deposit verbatim", "DEPOSIT", "DEPOSIT", "", "wire in", nil, "DEPOSIT"},
		{"trade fill verbatim", "TRADE_FILL", "TRADE_FILL", "", "fill", nil, "TRADE_FILL"},
		{"withdrawal verbatim", "WITHDRAWAL", "WITHDRAWAL", "", "wire out", nil, "WITHDRAWAL"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ClassifyIncome(tc.entryType, tc.jType, tc.idem, tc.desc, tc.lines)
			if got != tc.want {
				t.Fatalf("ClassifyIncome(%q,%q,%q,%q,%v) = %q, want %q",
					tc.entryType, tc.jType, tc.idem, tc.desc, tc.lines, got, tc.want)
			}
		})
	}
}

func TestValidIncomeType(t *testing.T) {
	for tok, want := range map[string]bool{
		"COMMISSION": true, "SWAP_ROLLOVER": true, "TRADE_FILL": true,
		"lowercase": false, "BAD TYPE": false, "';": false, "": false,
		"A": false, "TOOLONG" + strings.Repeat("X", 32): false,
	} {
		if ValidIncomeType(tok) != want {
			t.Fatalf("ValidIncomeType(%q) = %v, want %v", tok, !want, want)
		}
	}
}

// ---------------------------------------------------------------------------
// Store unit tests — batch insert, query SQL shape, fail-closed
// ---------------------------------------------------------------------------

func TestIncomeStoreInsertBatches(t *testing.T) {
	conn := &incomeFakeConn{}
	s := NewIncomeStore(conn).WithClock(func() time.Time {
		return time.Unix(1700000000, 0).UTC()
	})
	rows := []IncomeRow{{
		PostedAt: time.Unix(1700000100, 0).UTC(), AccountID: 42,
		Currency: "USD", IncomeType: IncomeCommission, EntryType: "FEE",
		Amount: decimal.RequireFromString("-2.5"), LedgerEntryID: 1001,
		JournalEntryID: 2002, ReferenceID: 77, Description: "COMMISSION trade=77",
	}}
	if err := s.Insert(context.Background(), rows); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if len(conn.batches) != 1 {
		t.Fatalf("batches = %d, want 1", len(conn.batches))
	}
	b := conn.batches[0]
	if !b.sent {
		t.Fatal("batch not sent")
	}
	wantSQL := "INSERT INTO income_ledger (posted_at, account_id, currency, " +
		"income_type, entry_type, amount, ledger_entry_id, journal_entry_id, " +
		"reference_id, description, ver)"
	if b.sql != wantSQL {
		t.Fatalf("sql = %q\nwant %q", b.sql, wantSQL)
	}
	if len(b.rows) != 1 || len(b.rows[0]) != 11 {
		t.Fatalf("rows = %+v — want one 11-column row", b.rows)
	}
	if got := b.rows[0][3]; got != IncomeCommission {
		t.Fatalf("income_type = %v", got)
	}
	if got := b.rows[0][6]; got != uint64(1001) {
		t.Fatalf("ledger_entry_id = %v", got)
	}
	if got := b.rows[0][10]; got != uint64(1700000000000000000) {
		t.Fatalf("ver = %v, want injected clock nanos", got)
	}
}

func TestIncomeStoreQueryPredicateShape(t *testing.T) {
	conn := &incomeFakeConn{}
	posted := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	conn.queryFn = func(q string, _ ...any) (driver.Rows, error) {
		if strings.HasPrefix(q, "SELECT count()") {
			return &incomeFakeRows{data: [][]any{{uint64(1)}}}, nil
		}
		return &incomeFakeRows{data: [][]any{{
			posted, uint64(42), "USD", IncomeCommission, "FEE",
			decimal.RequireFromString("-2.5"), uint64(1001), uint64(2002),
			uint64(77), "COMMISSION trade=77", "EUR/USD",
		}}}, nil
	}
	s := NewIncomeStore(conn)
	rows, total, err := s.Query(context.Background(), IncomeQuery{
		AccountID: 42, Type: IncomeCommission, Symbol: "EUR/USD",
		From: posted.Add(-time.Hour), To: posted.Add(time.Hour),
		After: &IncomeCursor{PostedAt: posted.Add(time.Hour), LedgerEntryID: 5000},
		Limit: 50,
	})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if total != 1 || len(rows) != 1 {
		t.Fatalf("total=%d rows=%d", total, len(rows))
	}
	r := rows[0]
	if r.Symbol != "EUR/USD" || r.IncomeType != IncomeCommission || r.JournalEntryID != 2002 {
		t.Fatalf("row = %+v", r)
	}
	if !r.Amount.Equal(decimal.RequireFromString("-2.5")) {
		t.Fatalf("amount = %s, want -2.5", r.Amount)
	}
	// The captured SQL must carry the derived-symbol read, all filters,
	// and the keyset continuation — inspect the page query.
	page := conn.queries[len(conn.queries)-1]
	for _, want := range []string{
		"income_ledger", "FINAL", "LEFT ANY JOIN", "trades",
		"income_type = ?", "symbol IN (?, ?)", "posted_at >= ?",
		"posted_at < ?", "(posted_at, ledger_entry_id) < (?, ?)",
		"ORDER BY posted_at DESC, ledger_entry_id DESC", "LIMIT ?",
	} {
		if !strings.Contains(page, want) {
			t.Fatalf("page SQL missing %q:\n%s", want, page)
		}
	}
}

func TestIncomeStoreFailClosed(t *testing.T) {
	var s *IncomeStore
	if _, _, err := s.Query(context.Background(), IncomeQuery{}); err == nil {
		t.Fatal("nil store: Query must fail closed")
	}
	s = NewIncomeStore(nil)
	if _, _, err := s.Query(context.Background(), IncomeQuery{}); err == nil {
		t.Fatal("nil conn: Query must fail closed")
	}
	if _, err := s.MaxLedgerEntryID(context.Background()); err == nil {
		t.Fatal("nil conn: MaxLedgerEntryID must fail closed")
	}
	if _, err := s.HasAny(context.Background()); err == nil {
		t.Fatal("nil conn: HasAny must fail closed")
	}
	if err := s.Insert(context.Background(), []IncomeRow{{AccountID: 1}}); err == nil {
		t.Fatal("nil conn: Insert must fail closed")
	}
	// A dead/unreachable conn surfaces as error → handler maps 503.
	dead := NewIncomeStore(&incomeFakeConn{queryErr: errors.New("conn refused")})
	if _, _, err := dead.Query(context.Background(), IncomeQuery{AccountID: 1}); err == nil {
		t.Fatal("query error must propagate")
	}
	if _, err := dead.HasAny(context.Background()); err == nil {
		t.Fatal("probe error must propagate")
	}
}

func TestIncomeWatermarkEmpty(t *testing.T) {
	conn := &incomeFakeConn{queryFn: func(q string, _ ...any) (driver.Rows, error) {
		return &incomeFakeRows{data: [][]any{{uint64(0)}}}, nil // CH max() on empty = 0
	}}
	got, err := NewIncomeStore(conn).MaxLedgerEntryID(context.Background())
	if err != nil || got != 0 {
		t.Fatalf("watermark = %d err %v, want 0", got, err)
	}
}

// ---------------------------------------------------------------------------
// Live tests — gated.
// ---------------------------------------------------------------------------

func chLiveConn(t *testing.T) Conn {
	t.Helper()
	if testing.Short() {
		t.Skip("short mode")
	}
	if os.Getenv("EXC_CH_TEST") != "1" {
		t.Skip("set EXC_CH_TEST=1 for live ClickHouse tests")
	}
	cfg := ConfigFromEnv()
	if cfg.User == "" {
		cfg.User = "exchange"
	}
	if cfg.Password == "" {
		cfg.Password = "exchange_dev"
	}
	conn, err := Dial(context.Background(), cfg)
	if err != nil {
		t.Skipf("clickhouse unreachable: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// TestCHIncomeRoundTrip inserts through IncomeStore and reads back with
// type/symbol/period filters — including the trades-join-derived symbol
// — then exercises Totals + the ReconcileToStatement CH side.
func TestCHIncomeRoundTrip(t *testing.T) {
	conn := chLiveConn(t)
	ctx := context.Background()
	for _, tbl := range []string{IncomeTable, TradesTable} {
		if _, err := ShowCreateTable(ctx, conn, tbl); err != nil {
			t.Skipf("%s DDL not yet deployed: %v", tbl, err)
		}
	}
	acct := int64(9_900_000_000 + time.Now().UnixNano()%1_000_000)
	posted := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)

	// One trade so the commission row resolves a symbol via reference_id.
	// The trades qty/quantity column was mid-migration when this was
	// written (real column + back-compat ALIAS under sibling dev) — pick
	// whichever non-alias name system.columns reports.
	qtyCol := "quantity"
	if cn := chCount(t, conn, `SELECT count() FROM system.columns
		WHERE database = currentDatabase() AND table = 'trades'
		  AND name = 'qty' AND default_kind = ''`); cn == 1 {
		qtyCol = "qty"
	}
	tradeID := uint64(time.Now().UnixNano() % 1e9)
	tb, err := conn.PrepareBatch(ctx, `INSERT INTO `+TradesTable+
		` (ts, symbol, trade_id, instrument_id, maker_account_id, `+
		`taker_account_id, buy_order_id, sell_order_id, price, `+qtyCol+`, `+
		`aggressor_side, event_seq, shard_id, ver)`)
	if err != nil {
		t.Fatalf("trade prepare: %v", err)
	}
	if err := tb.Append(posted, "EUR/USD", tradeID, int64(1), acct, int64(2),
		uint64(1), uint64(2), decimal.RequireFromString("1.09"),
		decimal.RequireFromString("1000"), "BUY", uint64(1), uint32(0),
		uint64(time.Now().UnixNano())); err != nil {
		t.Fatalf("trade append: %v", err)
	}
	if err := tb.Send(); err != nil {
		t.Fatalf("trade seed: %v", err)
	}

	s := NewIncomeStore(conn)
	rows := []IncomeRow{
		{PostedAt: posted, AccountID: acct, Currency: "USD",
			IncomeType: IncomeCommission, EntryType: "FEE",
			Amount:        decimal.RequireFromString("-2.5"),
			LedgerEntryID: 1, JournalEntryID: 1, ReferenceID: tradeID,
			Description: "COMMISSION trade"},
		{PostedAt: posted.Add(time.Minute), AccountID: acct, Currency: "EUR",
			IncomeType: IncomeDustConvert, EntryType: "TRANSFER",
			Amount:        decimal.RequireFromString("9.75"),
			LedgerEntryID: 2, JournalEntryID: 2,
			Description: "DUST_CONVERT acct=1 EUR→USD mid=1.09"},
		{PostedAt: posted.Add(2 * time.Minute), AccountID: acct, Currency: "USD",
			IncomeType: IncomeRebate, EntryType: "FEE",
			Amount:        decimal.RequireFromString("0.40"),
			LedgerEntryID: 3, JournalEntryID: 3,
			Description: "MAKER_REBATE trade=1"},
	}
	if err := s.Insert(ctx, rows); err != nil {
		t.Fatalf("insert: %v", err)
	}

	// async_insert visibility lag — poll until all three land.
	var got []IncomeRow
	for i := 0; i < 30; i++ {
		var err error
		got, _, err = s.Query(ctx, IncomeQuery{AccountID: acct, Limit: 10})
		if err != nil {
			t.Fatalf("query: %v", err)
		}
		if len(got) == 3 {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if len(got) != 3 {
		t.Fatalf("rows = %d, want 3", len(got))
	}
	// Newest-first ordering + signed amounts.
	if got[0].LedgerEntryID != 3 || !got[0].Amount.IsPositive() {
		t.Fatalf("row0 = %+v", got[0])
	}
	if !got[2].Amount.Equal(decimal.RequireFromString("-2.5")) {
		t.Fatalf("commission signed amount = %s, want -2.5", got[2].Amount)
	}

	// Type filter.
	only, _, err := s.Query(ctx, IncomeQuery{AccountID: acct, Type: IncomeDustConvert, Limit: 10})
	if err != nil || len(only) != 1 || only[0].IncomeType != IncomeDustConvert {
		t.Fatalf("type filter rows=%v err=%v", only, err)
	}
	// Symbol filter: narrative-derived (EUR→USD) AND trade-join (EUR/USD).
	symRows, _, err := s.Query(ctx, IncomeQuery{AccountID: acct, Symbol: "EUR/USD", Limit: 10})
	if err != nil {
		t.Fatalf("symbol query: %v", err)
	}
	if len(symRows) != 2 {
		t.Fatalf("symbol rows = %d, want 2 (narrative + trade join): %+v", len(symRows), symRows)
	}
	for _, r := range symRows {
		if r.Symbol != "EUR/USD" {
			t.Fatalf("derived symbol = %q", r.Symbol)
		}
	}
	// Time window.
	win, _, err := s.Query(ctx, IncomeQuery{AccountID: acct,
		From: posted.Add(30 * time.Second), To: posted.Add(90 * time.Second), Limit: 10})
	if err != nil || len(win) != 1 || win[0].LedgerEntryID != 2 {
		t.Fatalf("window rows=%v err=%v", win, err)
	}
	// GL linkage present on every row.
	for _, r := range got {
		if r.LedgerEntryID == 0 || r.JournalEntryID == 0 {
			t.Fatalf("row missing GL linkage: %+v", r)
		}
	}
	// Totals reconcile against the inserted set.
	tots, err := s.Totals(ctx, acct, posted.Add(-time.Minute), posted.Add(time.Hour))
	if err != nil {
		t.Fatalf("totals: %v", err)
	}
	var comm decimal.Decimal
	for _, tt := range tots {
		if tt.IncomeType == IncomeCommission && tt.Currency == "USD" {
			comm = tt.Total
		}
	}
	if !comm.Equal(decimal.RequireFromString("-2.5")) {
		t.Fatalf("commission total = %s", comm)
	}
	// Idempotent re-insert (ReplacingMergeTree collapse under FINAL).
	time.Sleep(5 * time.Millisecond)
	if err := s.Insert(ctx, rows); err != nil {
		t.Fatalf("re-insert: %v", err)
	}
	for i := 0; i < 30; i++ {
		got, _, _ = s.Query(ctx, IncomeQuery{AccountID: acct, Limit: 10})
		if len(got) == 3 {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if len(got) != 3 {
		t.Fatalf("re-inserted rows = %d, want 3 (FINAL dedup)", len(got))
	}
	// Watermark tracks max ledger_entry_id.
	if wm, err := s.MaxLedgerEntryID(ctx); err != nil || wm < 3 {
		t.Fatalf("watermark = %d err=%v", wm, err)
	}
}

// ---------------------------------------------------------------------------
// PG-gated tests — the sync read + GL-side recompute share one seeded set.
// ---------------------------------------------------------------------------

func pgTestPoolT(t *testing.T) *pgxpool.Pool {
	t.Helper()
	if os.Getenv("EXC_PG_TEST") != "1" {
		t.Skip("set EXC_PG_TEST=1 for Postgres integration tests")
	}
	dsn := os.Getenv("EXC_PG_DSN")
	if dsn == "" {
		dsn = "postgres://exchange:exchange_dev@127.0.0.1:5433/exchange?sslmode=disable"
	}
	pool, err := db.NewPool(context.Background(), dsn, 4)
	if err != nil {
		t.Skipf("postgres unreachable: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// seedIncomeFixture creates one user+account pair and one classified
// journal+wallet row per §16.7 income type. Every journal is zero-sum
// (deferred constraint) and chart codes come from migration 036/088 seed.
func seedIncomeFixture(t *testing.T, pool *pgxpool.Pool) (accountID int64, journalIDs []int64) {
	t.Helper()
	ctx := context.Background()
	tag := fmt.Sprint(time.Now().UnixNano())
	var userID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO users (email, password_hash) VALUES ($1,'x') RETURNING id`,
		"inc-test-"+tag+"@example.invalid").Scan(&userID); err != nil {
		t.Fatalf("user seed: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO accounts (user_id, account_type) VALUES ($1,'SPOT') RETURNING id`,
		userID).Scan(&accountID); err != nil {
		t.Fatalf("account seed: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO balances (account_id, currency, available) VALUES ($1,'USD',100000)`, accountID); err != nil {
		t.Fatalf("balance seed: %v", err)
	}

	type spec struct {
		jType, desc, idem, lentryType string
		dir                           string
		amount                        string
		lines                         [][3]string // (debit_acct, credit_acct, narrative)
	}
	specs := []spec{
		{"FEE", "COMMISSION trade=1 tier=1", "itest:" + tag + ":comm", "FEE", "CREDIT", "2.50",
			[][3]string{{"2010_CUSTOMER_LIABILITY_USD", "4030_COMMISSION_REVENUE_USD", "COMMISSION"}}},
		{"EOD_ROLLOVER", "TOMNEXT EUR/USD LONG interbank=1", "itest:" + tag + ":swap", "ROLLOVER", "DEBIT", "0.75",
			[][3]string{{"4100_SWAP_ROLLOVER_REVENUE_USD", "2010_CUSTOMER_LIABILITY_USD", "TOMNEXT"}}},
		{"FEE", "MAKER_REBATE trade=9", "itest:" + tag + ":reb", "FEE", "DEBIT", "0.40",
			[][3]string{{"5100_LIQUIDITY_REBATE_EXPENSE_USD", "2100_CLIENT_COLLATERAL_USD", "MAKER_REBATE"}}},
		{"ADJUSTMENT", "NBP_RESTITUTION_POSTED house P&L acct 1 USD 10", "itest:" + tag + ":nbp", "ADJUSTMENT", "DEBIT", "10.00",
			[][3]string{{"5200_NBP_RESTITUTION_EXPENSE_USD", "2010_CUSTOMER_LIABILITY_USD", "NBP_RESTITUTION"}}},
		{"TRANSFER", "DUST_CONVERT acct=1 EUR→USD mid=1.09", "itest:" + tag + ":dust", "TRANSFER", "DEBIT", "9.10",
			[][3]string{{"1200_MULTI_CURRENCY_CLEARING_USD", "2010_CUSTOMER_LIABILITY_USD", "DUST_CONVERT"}}},
		{"FEE", "funding withdrawal fee 5 USD (tier 2)", "itest:" + tag + ":fund", "FEE", "CREDIT", "5.00",
			[][3]string{{"2010_CUSTOMER_LIABILITY_USD", "4200_FUNDING_FEE_REVENUE_USD", "funding fee"}}},
	}
	var lastEntryID int64
	for i, sp := range specs {
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatalf("tx %d: %v", i, err)
		}
		var jid int64
		if err := tx.QueryRow(ctx, `
			INSERT INTO journal_entries (entry_type, reference_id, description, posted_by, idempotency_key)
			VALUES ($1::gl_entry_type_enum, $2, $3, 'income-test', $4) RETURNING id`,
			sp.jType, int64(1000000+i), sp.desc, sp.idem).Scan(&jid); err != nil {
			tx.Rollback(ctx)
			t.Fatalf("journal %d: %v", i, err)
		}
		journalIDs = append(journalIDs, jid)
		for _, ln := range sp.lines {
			if _, err := tx.Exec(ctx, `
				INSERT INTO ledger_lines (journal_entry_id, account_code, debit_amount, currency, narrative)
				VALUES ($1,$2,$3::numeric,'USD',$4)`, jid, ln[0], sp.amount, ln[2]); err != nil {
				tx.Rollback(ctx)
				t.Fatalf("debit line %d: %v", i, err)
			}
			if _, err := tx.Exec(ctx, `
				INSERT INTO ledger_lines (journal_entry_id, account_code, credit_amount, currency, narrative)
				VALUES ($1,$2,$3::numeric,'USD',$4)`, jid, ln[1], sp.amount, ln[2]); err != nil {
				tx.Rollback(ctx)
				t.Fatalf("credit line %d: %v", i, err)
			}
		}
		if err := tx.QueryRow(ctx, `
			INSERT INTO ledger_entries (entry_type, reference_id, account_id, currency,
			    direction, amount, running_balance, description, journal_entry_id, posted_by)
			VALUES ($1::ledger_entry_type_enum,$2,$3,'USD',$4::ledger_direction_enum,
			    $5::numeric, 100000, $6, $7, 'income-test') RETURNING id`,
			sp.lentryType, int64(1000000+i), accountID, sp.dir, sp.amount, sp.desc, jid).
			Scan(&lastEntryID); err != nil {
			tx.Rollback(ctx)
			t.Fatalf("ledger entry %d: %v", i, err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit %d: %v", i, err)
		}
	}
	t.Cleanup(func() {
		// Test rows are torn down so reruns stay clean (chain: wallet →
		// lines → journal → balances → account → user).
		pool.Exec(context.Background(), `DELETE FROM ledger_entries WHERE posted_by='income-test'`)
		pool.Exec(context.Background(), `DELETE FROM ledger_lines WHERE journal_entry_id = ANY($1)`, journalIDs)
		pool.Exec(context.Background(), `DELETE FROM journal_entries WHERE posted_by='income-test'`)
		pool.Exec(context.Background(), `DELETE FROM balances WHERE account_id=$1`, accountID)
		pool.Exec(context.Background(), `DELETE FROM accounts WHERE id=$1`, accountID)
		pool.Exec(context.Background(), `DELETE FROM users WHERE id=$1`, userID)
	})
	_ = lastEntryID
	return accountID, journalIDs
}

func TestPgIncomeSyncRowsClassification(t *testing.T) {
	pool := pgTestPoolT(t)
	acct, _ := seedIncomeFixture(t, pool)
	ctx := context.Background()

	// Page the whole projection — the seeded rows sit at the tail of a
	// shared dev ledger (cursor semantics get exercised too).
	mine := make(map[string]IncomeRow)
	var cursor uint64
	for pages := 0; pages < 50; pages++ {
		rows, next, err := IncomeSyncRows(ctx, pool, cursor, 500)
		if err != nil {
			t.Fatalf("IncomeSyncRows page %d: %v", pages, err)
		}
		for _, r := range rows {
			if r.AccountID == acct {
				mine[r.IncomeType] = r
			}
		}
		if len(rows) < 500 {
			break
		}
		if next == cursor {
			t.Fatal("cursor did not advance")
		}
		cursor = next
	}
	if len(mine) == 0 {
		t.Fatal("no seeded rows projected — sync read empty")
	}
	for _, want := range []string{IncomeCommission, IncomeSwapRollover, IncomeRebate,
		IncomeNBPAdjust, IncomeDustConvert, IncomeFundingFee} {
		r, ok := mine[want]
		if !ok {
			t.Fatalf("missing income type %s in projected rows %+v", want, mine)
		}
		if r.JournalEntryID == 0 || r.LedgerEntryID == 0 {
			t.Fatalf("row %s missing GL linkage: %+v", want, r)
		}
	}
	// Signed amounts: CREDIT rows negative (fee/charge), DEBIT positive.
	if !mine[IncomeCommission].Amount.Equal(decimal.RequireFromString("-2.5")) {
		t.Fatalf("commission amount = %s, want -2.5", mine[IncomeCommission].Amount)
	}
	if !mine[IncomeNBPAdjust].Amount.Equal(decimal.RequireFromString("10")) {
		t.Fatalf("nbp amount = %s, want 10", mine[IncomeNBPAdjust].Amount)
	}
}

func TestPgReconcileToStatement(t *testing.T) {
	pool := pgTestPoolT(t)
	acct, _ := seedIncomeFixture(t, pool)
	ctx := context.Background()
	from := time.Now().UTC().Add(-time.Hour)
	to := time.Now().UTC().Add(time.Hour)

	gl, err := IncomeTotalsPG(ctx, pool, acct, from, to)
	if err != nil {
		t.Fatalf("IncomeTotalsPG: %v", err)
	}
	if len(gl) != 6 {
		t.Fatalf("GL totals buckets = %d, want 6: %+v", len(gl), gl)
	}
	// The statement-side computation and the sync read are the same GL
	// rows — sum equality is proven explicitly here rather than assumed.
	sums := map[string]decimal.Decimal{}
	for _, tt := range gl {
		sums[tt.IncomeType] = tt.Total
	}
	if !sums[IncomeCommission].Equal(decimal.RequireFromString("-2.5")) ||
		!sums[IncomeFundingFee].Equal(decimal.RequireFromString("-5")) {
		t.Fatalf("GL sums = %+v", sums)
	}
}

// TestReconcileToStatementLive syncs the seeded fixture into ClickHouse
// and runs the explicit projection-vs-GL comparison — then proves the
// check actually detects drift by injecting a stray CH row.
func TestReconcileToStatementLive(t *testing.T) {
	if os.Getenv("EXC_PG_TEST") != "1" || os.Getenv("EXC_CH_TEST") != "1" {
		t.Skip("set EXC_PG_TEST=1 and EXC_CH_TEST=1 for the live reconcile test")
	}
	pool := pgTestPoolT(t)
	conn := chLiveConn(t)
	ctx := context.Background()
	if _, err := ShowCreateTable(ctx, conn, IncomeTable); err != nil {
		t.Skipf("income_ledger DDL not deployed: %v", err)
	}
	acct, _ := seedIncomeFixture(t, pool)
	from := time.Now().UTC().Add(-time.Hour)
	to := time.Now().UTC().Add(time.Hour)

	// Project ONLY this account's seeded rows into CH (the shared dev
	// ledger's other rows belong to other fixtures).
	var minID uint64 = ^uint64(0)
	if err := pool.QueryRow(ctx,
		`SELECT min(id) FROM ledger_entries WHERE account_id=$1`, acct).
		Scan(&minID); err != nil {
		t.Fatalf("min id: %v", err)
	}
	store := NewIncomeStore(conn)
	rows, _, err := IncomeSyncRows(ctx, pool, minID-1, 500)
	if err != nil {
		t.Fatalf("sync rows: %v", err)
	}
	var mine []IncomeRow
	for _, r := range rows {
		if r.AccountID == acct {
			mine = append(mine, r)
		}
	}
	if len(mine) != 6 {
		t.Fatalf("projected rows = %d, want 6", len(mine))
	}
	if err := store.Insert(ctx, mine); err != nil {
		t.Fatalf("insert: %v", err)
	}

	var rep *ReconcileReport
	for i := 0; i < 30; i++ {
		rep, err = ReconcileToStatement(ctx, pool, store, acct, from, to)
		if err != nil {
			t.Fatalf("reconcile: %v", err)
		}
		if rep.OK && len(rep.Lines) == 6 {
			break
		}
		time.Sleep(200 * time.Millisecond) // async_insert visibility
	}
	if !rep.OK {
		t.Fatalf("reconcile report not OK: %+v", rep.Lines)
	}
	for _, l := range rep.Lines {
		if !l.Match {
			t.Fatalf("line mismatch: %+v", l)
		}
	}

	// Drift injection: one CH row with no GL counterpart must flip the
	// report — this is what makes the reconciliation non-vacuous.
	if err := store.Insert(ctx, []IncomeRow{{
		PostedAt: time.Now().UTC(), AccountID: acct, Currency: "USD",
		IncomeType: IncomeCommission, EntryType: "FEE",
		Amount:        decimal.RequireFromString("-99"),
		LedgerEntryID: 900000001, JournalEntryID: 900000001,
		Description: "phantom row"}}); err != nil {
		t.Fatalf("phantom insert: %v", err)
	}
	var rep2 *ReconcileReport
	for i := 0; i < 30; i++ {
		rep2, err = ReconcileToStatement(ctx, pool, store, acct, from, to)
		if err != nil {
			t.Fatalf("reconcile2: %v", err)
		}
		if !rep2.OK {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if rep2.OK {
		t.Fatal("phantom CH row must fail reconciliation")
	}
}
