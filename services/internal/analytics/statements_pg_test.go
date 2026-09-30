// statements_pg_test.go — Phase-20 Task 20.3.6/20.3.7 PostgreSQL
// integration coverage. Gated: skipped unless EXC_PG_TEST=1; targets
// EXC_PG_DSN (default: the dev database). Run:
//
//	EXC_PG_TEST=1 go test ./internal/analytics -run 'TestPg' -v
package analytics

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"
)

// testDocCipher stands in for the EXC_DOCS_SECRET-derived cipher —
// generated client PDFs are AES-128 encrypted (Task 20.3.8) and
// generation is fail-closed without one.
var testDocCipher = NewDocCipher([]byte("test-doc-secret"))

func pgTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	if os.Getenv("EXC_PG_TEST") != "1" {
		t.Skip("set EXC_PG_TEST=1 to run PostgreSQL integration tests")
	}
	dsn := os.Getenv("EXC_PG_DSN")
	if dsn == "" {
		dsn = "postgres://exchange:exchange_dev@127.0.0.1:5433/exchange?sslmode=disable"
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Skipf("postgres unreachable: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Skipf("postgres unreachable: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// pgSeedAccount inserts a fresh user+account; unique per call via the
// monotonic suffix so parallel tests never share keys.
func pgSeedAccount(t *testing.T, pool *pgxpool.Pool, category string) int64 {
	t.Helper()
	ctx := context.Background()
	tag := fmt.Sprintf("pg-test-%d@x", time.Now().UnixNano())
	var uid int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO users (email) VALUES ($1) RETURNING id`, tag).Scan(&uid); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	var aid int64
	if err := pool.QueryRow(ctx, `
		INSERT INTO accounts (user_id, account_type, client_category)
		VALUES ($1, 'MARGIN', $2) RETURNING id`, uid, category).Scan(&aid); err != nil {
		t.Fatalf("seed account: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM client_statements WHERE account_id = $1`, aid)
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM trade_confirmations WHERE account_id = $1`, aid)
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM fee_invoices WHERE account_id = $1`, aid)
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM ledger_entries WHERE account_id = $1`, aid)
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM funding_transactions WHERE account_id = $1`, aid)
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM balances WHERE account_id = $1`, aid)
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM accounts WHERE id = $1`, aid)
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM users WHERE id = $1`, uid)
	})
	return aid
}

// pgSeedJournal posts a balanced two-line GL journal and returns its id
// (the deferred zero-sum trigger refuses anything unbalanced).
func pgSeedJournal(t *testing.T, pool *pgxpool.Pool, postedAt time.Time,
	debitAcct, creditAcct, ccy, amt string) int64 {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("journal tx: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var jid int64
	if err := tx.QueryRow(ctx, `
		INSERT INTO journal_entries (entry_type, description, posted_by, posted_at)
		VALUES ('DEPOSIT', 'pg test seed', 'test', $1) RETURNING id`,
		postedAt).Scan(&jid); err != nil {
		t.Fatalf("journal head: %v", err)
	}
	for _, l := range []struct {
		acct   string
		debit  string
		credit string
	}{{debitAcct, amt, "0"}, {creditAcct, "0", amt}} {
		if _, err := tx.Exec(ctx, `
			INSERT INTO ledger_lines
			    (journal_entry_id, account_code, currency, debit_amount, credit_amount)
			VALUES ($1, $2, $3, $4::numeric, $5::numeric)`,
			jid, l.acct, ccy, l.debit, l.credit); err != nil {
			t.Fatalf("ledger line %s: %v", l.acct, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("journal commit (zero-sum trigger): %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM ledger_lines WHERE journal_entry_id = $1`, jid)
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM journal_entries WHERE id = $1`, jid)
	})
	return jid
}

func pgSeedLedgerEntry(t *testing.T, pool *pgxpool.Pool, accountID int64,
	entryType, direction, ccy, amount, running string, postedAt time.Time, jid int64) {
	t.Helper()
	var j any
	if jid > 0 {
		j = jid
	}
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO ledger_entries
		    (entry_type, account_id, currency, direction, amount,
		     running_balance, journal_entry_id, posted_at, posted_by)
		VALUES ($1, $2, $3, $4, $5::numeric, $6::numeric, $7, $8, 'test')`,
		entryType, accountID, ccy, direction, amount, running, j, postedAt); err != nil {
		t.Fatalf("ledger entry: %v", err)
	}
}

// TestPgTrialBalanceEmptyDay — a day with no GL activity yields a zero
// trial balance, not an error (weekend/holiday policy).
func TestPgTrialBalanceEmptyDay(t *testing.T) {
	pool := pgTestPool(t)
	svc, err := NewTrialBalanceService(pool)
	if err != nil {
		t.Fatalf("service: %v", err)
	}
	tb, err := svc.Compute(context.Background(), time.Date(1990, 1, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("empty-day compute: %v", err)
	}
	if len(tb.Currencies) != 0 {
		t.Fatalf("expected zero currencies, got %d", len(tb.Currencies))
	}
}

// TestPgVerifyGLConservation — a balanced journal passes; the DB's own
// deferred trigger already refuses unbalanced journals so the test also
// asserts the balanced path round-trips.
func TestPgVerifyGLConservation(t *testing.T) {
	pool := pgTestPool(t)
	day := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	jid := pgSeedJournal(t, pool, day,
		"1110_CLIENT_MONEY_SEGREGATED_USD", "2010_CUSTOMER_LIABILITY_USD",
		"USD", "1000")
	svc, err := NewStatementService(pool, NewMemFileStore())
	if err != nil {
		t.Fatalf("service: %v", err)
	}
	svc.SetDocCipher(testDocCipher)
	if err := svc.VerifyGLConservation(context.Background(), []int64{jid}); err != nil {
		t.Fatalf("balanced journal failed conservation: %v", err)
	}
	if err := svc.VerifyGLConservation(context.Background(), nil); err != nil {
		t.Fatalf("empty journal set: %v", err)
	}
}

// TestPgStatementGenerateFetch — end-to-end: seed opening balance,
// in-period fee, pending funding; generate → assert invariant, files
// stored, list + fetch round-trip.
func TestPgStatementGenerateFetch(t *testing.T) {
	pool := pgTestPool(t)
	files := NewMemFileStore()
	acct := pgSeedAccount(t, pool, "RETAIL")
	day := time.Date(2026, 9, 29, 0, 0, 0, 0, time.UTC)

	// opening deposit before the period (GL-backed).
	jid1 := pgSeedJournal(t, pool, day.Add(-24*time.Hour),
		"1110_CLIENT_MONEY_SEGREGATED_USD", "2010_CUSTOMER_LIABILITY_USD",
		"USD", "1000")
	pgSeedLedgerEntry(t, pool, acct, "DEPOSIT", "DEBIT", "USD",
		"1000", "1000", day.Add(-24*time.Hour), jid1)

	// in-period fee (wallet CREDIT out; GL DR liability / CR revenue).
	jid2 := pgSeedJournal(t, pool, day.Add(6*time.Hour),
		"2010_CUSTOMER_LIABILITY_USD", "4010_TRADING_FEE_REVENUE_USD",
		"USD", "2.5")
	pgSeedLedgerEntry(t, pool, acct, "FEE", "CREDIT", "USD",
		"2.5", "997.5", day.Add(6*time.Hour), jid2)

	// pending deposit inside the period — must appear FLAGGED, excluded
	// from totals.
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO funding_transactions (account_id, currency, type, amount, status, created_at)
		VALUES ($1, 'USD', 'DEPOSIT', 500, 'PENDING', $2)`,
		acct, day.Add(9*time.Hour)); err != nil {
		t.Fatalf("pending funding: %v", err)
	}
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO balances (account_id, currency, available, locked)
		VALUES ($1, 'USD', 997.5, 0)`, acct); err != nil {
		t.Fatalf("balance row: %v", err)
	}

	svc, err := NewStatementService(pool, files)
	if err != nil {
		t.Fatalf("service: %v", err)
	}
	svc.SetDocCipher(testDocCipher)
	row, st, err := svc.GenerateForAccount(context.Background(), acct,
		StmtPeriodDaily, day, day.AddDate(0, 0, 1))
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if row.StatementID == 0 {
		t.Fatal("no statement_id")
	}
	if len(st.Currencies) != 1 || st.Currencies[0].Currency != "USD" {
		t.Fatalf("expected USD section, got %+v", st.Currencies)
	}
	cs := st.Currencies[0]
	if !cs.Opening.Equal(dec1000) || !cs.Closing.Equal(dec9975) {
		t.Fatalf("balances: opening %s closing %s", cs.Opening, cs.Closing)
	}
	if !cs.Opening.Add(cs.NetMove).Equal(cs.Closing) {
		t.Fatal("opening+net != closing")
	}
	if len(st.Pending) != 1 || st.Pending[0].Status != "PENDING" {
		t.Fatalf("pending movement not flagged: %+v", st.Pending)
	}
	// both renderings stored under the file_ref stem.
	for _, ext := range []string{".pdf", ".csv"} {
		if _, ok := files.Objects[row.FileRef+ext]; !ok {
			t.Fatalf("missing stored object %s", row.FileRef+ext)
		}
	}
	// list + fetch.
	rows, total, err := svc.List(context.Background(), acct, StmtPeriodDaily, 10, nil, 0)
	if err != nil || len(rows) != 1 || total != 1 {
		t.Fatalf("list: %v rows=%v total=%v", err, rows, total)
	}
	f, err := svc.Fetch(context.Background(), acct, rows[0].StatementID, "pdf")
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if !strings.HasPrefix(string(f.Body), "%PDF") {
		t.Fatal("fetched body is not a pdf")
	}
	if _, err := svc.Fetch(context.Background(), acct+999, rows[0].StatementID, "pdf"); err == nil {
		t.Fatal("foreign-account fetch must fail")
	}
}

// TestPgConfirmationGenerateAndAdjust — v1 per-leg generation, then the
// busted-trade path: standing rows → ADJUSTED + version-2 reissue.
func TestPgConfirmationGenerateAndAdjust(t *testing.T) {
	pool := pgTestPool(t)
	files := NewMemFileStore()
	buyer := pgSeedAccount(t, pool, "RETAIL")
	seller := pgSeedAccount(t, pool, "RETAIL")
	day := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)

	var instID int64
	if err := pool.QueryRow(context.Background(), `
		INSERT INTO instruments (symbol, base_currency, quote_currency,
		    instrument_type, tick_size, lot_size, min_order_qty, max_order_qty,
		    settlement_cycle, max_leverage, contract_size, decimal_places, pip_size)
		VALUES ('TESTUSD', 'TST', 'USD', 'SPOT', 0.0001, 1000, 1, 1000000000,
		        2, 30, 1000, 5, 0.0001)
		RETURNING id`).Scan(&instID); err != nil {
		t.Fatalf("instrument: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM instruments WHERE id = $1`, instID)
	})
	var tradeID int64
	if err := pool.QueryRow(context.Background(), `
		INSERT INTO trades (instrument_id, buy_order_id, sell_order_id,
		    buyer_account_id, seller_account_id, price, quantity,
		    buyer_fee, seller_fee, settlement_date, created_at)
		VALUES ($1, 1, 2, $2, $3, 1.05, 10000, 2.5, 2.5, '2026-10-01', $4)
		RETURNING id`, instID, buyer, seller, day).Scan(&tradeID); err != nil {
		t.Fatalf("trade: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM trades WHERE id = $1`, tradeID)
	})

	cs, err := NewConfirmationService(pool, files)
	if err != nil {
		t.Fatalf("service: %v", err)
	}
	cs.SetDocCipher(testDocCipher)
	v1, err := cs.Generate(context.Background(), tradeID)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if len(v1) != 2 {
		t.Fatalf("expected 2 confirmations, got %d", len(v1))
	}
	for _, c := range v1 {
		if c.Status != "GENERATED" || c.Version != 1 {
			t.Fatalf("bad v1 row: %+v", c)
		}
		if _, ok := files.Objects[c.FileRef+".pdf"]; !ok {
			t.Fatalf("pdf not stored for %s", c.FileRef)
		}
	}
	// bust path: prior rows flip ADJUSTED, new v2 issued.
	v2, err := cs.MarkAdjusted(context.Background(), tradeID)
	if err != nil {
		t.Fatalf("adjust: %v", err)
	}
	if len(v2) != 2 || v2[0].Version != 2 {
		t.Fatalf("bad v2 rows: %+v", v2)
	}
	var adjCount int
	if err := pool.QueryRow(context.Background(), `
		SELECT count(*) FROM trade_confirmations
		WHERE trade_id = $1 AND status = 'ADJUSTED'`, tradeID).Scan(&adjCount); err != nil {
		t.Fatalf("count adjusted: %v", err)
	}
	if adjCount != 2 {
		t.Fatalf("expected 2 ADJUSTED rows, got %d", adjCount)
	}
	got, err := cs.Get(context.Background(), buyer, tradeID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Version != 2 || got.Status != "GENERATED" {
		t.Fatalf("latest not v2: %+v", got)
	}
}

// TestPgInvoiceSuspendedMM — a PROFESSIONAL account with a suspended MM
// program still gets an invoice; the rebate line is zero.
func TestPgInvoiceSuspendedMM(t *testing.T) {
	pool := pgTestPool(t)
	files := NewMemFileStore()
	acct := pgSeedAccount(t, pool, "PROFESSIONAL")
	month := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)

	jid := pgSeedJournal(t, pool, month.Add(48*time.Hour),
		"2010_CUSTOMER_LIABILITY_USD", "4010_TRADING_FEE_REVENUE_USD",
		"USD", "120.5")
	pgSeedLedgerEntry(t, pool, acct, "FEE", "CREDIT", "USD",
		"120.5", "0", month.Add(48*time.Hour), jid)

	// suspended MM program — no accruals → zero rebate line.
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO mm_programs (account_id, min_quote_size, max_spread_bps,
		    presence_pct, mmp_max_fills, mmp_window_ms, rebate_bps, status)
		VALUES ($1, 100000, 20, 85, 10, 5000, 0.5, 'SUSPENDED')`, acct); err != nil {
		t.Fatalf("mm program: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM mm_programs WHERE account_id = $1`, acct)
	})

	svc, err := NewInvoiceService(pool, files, nil)
	if err != nil {
		t.Fatalf("service: %v", err)
	}
	svc.SetDocCipher(testDocCipher)
	rep, err := svc.GenerateMonthly(context.Background(), month)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if rep.Issued < 1 {
		t.Fatalf("no invoices issued: %+v", rep)
	}
	invs, err := svc.List(context.Background(), acct, &month, 10)
	if err != nil || len(invs) != 1 {
		t.Fatalf("list: %v %+v", err, invs)
	}
	inv := invs[0]
	if !inv.TradingFees.Equal(dec1205) || !inv.MMRebates.IsZero() {
		t.Fatalf("invoice lines: %+v", inv)
	}
	if inv.Status != "ISSUED" {
		t.Fatalf("status %q", inv.Status)
	}
	if _, ok := files.Objects[inv.FileRef+".pdf"]; !ok {
		t.Fatal("invoice pdf not stored")
	}
	body, ct, err := svc.FetchFile(context.Background(), inv.InvoiceID, "csv")
	if err != nil || !strings.HasPrefix(ct, "text/csv") || len(body) == 0 {
		t.Fatalf("fetch csv: %v %q", err, ct)
	}
}

// TestPgERPReplayProtection — RunNightly for the same business day is
// rejected once SENT (run_id uniqueness + status gate).
func TestPgERPReplayProtection(t *testing.T) {
	pool := pgTestPool(t)
	dir := t.TempDir()
	svc, err := NewERPBatchService(pool, &SFTPDropAdapter{Dir: dir}, nil)
	if err != nil {
		t.Fatalf("service: %v", err)
	}
	day := time.Date(2026, 9, 29, 0, 0, 0, 0, time.UTC)
	d1, err := svc.RunNightly(context.Background(), day)
	if err != nil {
		t.Fatalf("first run: %v", err)
	}
	if d1.Status != "SENT" {
		t.Fatalf("first run status %q", d1.Status)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM erp_delivery_log WHERE run_id = $1`, RunIDFor(day))
	})
	if _, err := svc.RunNightly(context.Background(), day); err == nil {
		t.Fatal("same-run resend must be rejected")
	}
}

// TestPgDailyTBJob — the orchestrator seam: compute + persist + reconcile.
func TestPgDailyTBJob(t *testing.T) {
	pool := pgTestPool(t)
	svc, err := NewTrialBalanceService(pool)
	if err != nil {
		t.Fatalf("service: %v", err)
	}
	job := NewDailyTrialBalanceJob(svc)
	day := time.Date(2026, 9, 29, 0, 0, 0, 0, time.UTC)
	tb, checks, err := job.RunOnce(context.Background(), day)
	if err != nil {
		t.Fatalf("job: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM trial_balances WHERE business_date = $1`, day)
	})
	// Real dev GL may hold postings — the invariant is structural:
	// every currency section must satisfy debits==credits.
	for _, ct := range tb.Currencies {
		if !ct.ZeroSumOK {
			t.Fatalf("unbalanced section %s", ct.Currency)
		}
	}
	for _, c := range checks {
		_ = c // variance table surfaced; value depends on dev seed data
	}
	// persisted rows exist iff compute produced lines
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM trial_balances WHERE business_date = $1`, day).Scan(&n); err != nil {
		t.Fatalf("persisted count: %v", err)
	}
	var lineCount int
	for _, ct := range tb.Currencies {
		lineCount += len(ct.Lines)
	}
	if n != lineCount {
		t.Fatalf("persisted %d rows vs %d computed lines", n, lineCount)
	}
}

var (
	dec1000 = mustDec("1000")
	dec9975 = mustDec("997.5")
	dec1205 = mustDec("120.5")
)

func mustDec(s string) decimal.Decimal {
	d, err := decimal.NewFromString(s)
	if err != nil {
		panic(err)
	}
	return d
}
