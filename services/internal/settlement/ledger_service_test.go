package settlement

import (
	"context"
	stderrors "errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"

	"exchange/internal/ledger"
	excredis "exchange/internal/redis"
	excerrors "exchange/pkg/errors"
)

// ---------------------------------------------------------------------------
// Pure unit tests — no infrastructure required.
// ---------------------------------------------------------------------------

// A balance-mutating journal with no Redis backend must fail closed at the
// lock stage (§5.3 mandates the account mutex) — before touching Postgres.
func TestPostRequiresLockBackend(t *testing.T) {
	pool, err := pgxpool.New(context.Background(),
		"postgres://exchange:exchange_dev@127.0.0.1:1/exchange?sslmode=disable")
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer pool.Close()
	svc, err := NewLedgerService(pool, nil, nil)
	if err != nil {
		t.Fatalf("service: %v", err)
	}
	j := ledger.Journal{
		EntryType: ledger.EntryDeposit, Description: "d", PostedBy: "t",
		Lines: []ledger.Line{
			ledger.DebitLine(ledger.Nostro("USD"), "USD", decimal.NewFromInt(1), ""),
			ledger.CreditLine(ledger.CustomerLiability("USD"), "USD", decimal.NewFromInt(1), ""),
		},
		Effects: []ledger.AccountEffect{{
			AccountID: 1, Currency: "USD", AvailableDelta: decimal.NewFromInt(1),
		}},
	}
	_, err = svc.Post(context.Background(), j)
	requireCode(t, err, ledger.CodeLedgerLockUnavailable)
}

// journalHash is deterministic and payload-sensitive.
func TestJournalHash(t *testing.T) {
	j := depositTestJournal(7, "100")
	if journalHash(j) != journalHash(j) {
		t.Fatal("hash not deterministic")
	}
	j2 := depositTestJournal(7, "101")
	if journalHash(j) == journalHash(j2) {
		t.Fatal("hash insensitive to payload change")
	}
}

func depositTestJournal(accountID int64, amount string) ledger.Journal {
	amt := decimal.RequireFromString(amount)
	return ledger.Journal{
		EntryType:   ledger.EntryDeposit,
		ReferenceID: 1,
		Description: "test deposit",
		PostedBy:    "test",
		Lines: []ledger.Line{
			ledger.DebitLine(ledger.Nostro("USD"), "USD", amt, "in"),
			ledger.CreditLine(ledger.CustomerLiability("USD"), "USD", amt, "client"),
		},
		Effects: []ledger.AccountEffect{{
			AccountID: accountID, Currency: "USD", AvailableDelta: amt,
		}},
	}
}

func requireCode(t *testing.T, err error, code string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected %s, got nil", code)
	}
	var e *excerrors.Error
	if !stderrors.As(err, &e) || e.Code != code {
		t.Fatalf("expected code %s, got %v", code, err)
	}
}

// ---------------------------------------------------------------------------
// Integration test — dev Postgres (EXC_TEST_DSN) + dev Redis.
//
// The whole GL schema is applied into a per-run throwaway schema reached
// via the connection search_path, so the real database is never polluted.
// ledger_entries / journal_sums are the spec §5.3 tables owned by
// migration 102 (not yet applied): the fixture mirrors their contract,
// plus ledger_entries.journal_entry_id and a GENERATED journal_sums.
// net_balance — both flagged as requirements for the 102 author.
// ---------------------------------------------------------------------------

const defaultTestDSN = "postgres://exchange:exchange_dev@127.0.0.1:5433/exchange?sslmode=disable"
const defaultTestRedis = "127.0.0.1:16379"

type fakePub struct {
	subjects []string
	payloads [][]byte
	err      error
}

func (f *fakePub) Publish(_ context.Context, subject string, payload []byte) error {
	if f.err != nil {
		return f.err
	}
	f.subjects = append(f.subjects, subject)
	f.payloads = append(f.payloads, payload)
	return nil
}

func itestSchema(t *testing.T) string {
	return fmt.Sprintf("ledger_itest_%d", time.Now().UnixNano())
}

// migExec applies a migration file on a simple-protocol connection (pgx
// prepared statements cannot carry BEGIN..COMMIT multi-statement scripts).
func migExec(t *testing.T, ctx context.Context, dsn, schema, file string) {
	t.Helper()
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	cfg.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol
	cfg.RuntimeParams["search_path"] = schema + ",public"
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Skipf("postgres unreachable: %v", err)
	}
	defer conn.Close(ctx)
	sql, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("read %s: %v", file, err)
	}
	if _, err := conn.Exec(ctx, string(sql)); err != nil {
		t.Fatalf("apply %s: %v", file, err)
	}
}

const ledgerEntriesFixture = `
CREATE TYPE ledger_direction_enum  AS ENUM ('DEBIT','CREDIT');
CREATE TYPE ledger_entry_type_enum AS ENUM
    ('DEPOSIT','WITHDRAWAL','TRADE_FILL','FEE','TRANSFER','SETTLEMENT','ROLLOVER','LIQUIDATION','ADJUSTMENT');
CREATE TABLE ledger_entries (
    id               BIGSERIAL PRIMARY KEY,
    entry_type       ledger_entry_type_enum NOT NULL,
    reference_id     BIGINT,
    account_id       BIGINT NOT NULL,
    currency         VARCHAR(3) NOT NULL,
    direction        ledger_direction_enum NOT NULL,
    amount           DECIMAL(28,8) NOT NULL CHECK (amount > 0),
    running_balance  DECIMAL(28,8) NOT NULL,
    description      VARCHAR(255),
    journal_entry_id BIGINT,
    posted_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    posted_by        VARCHAR(64)
);
CREATE TABLE journal_sums (
    account_id    BIGINT       NOT NULL,
    currency      VARCHAR(3)   NOT NULL,
    total_debits  DECIMAL(28,8) NOT NULL DEFAULT 0,
    total_credits DECIMAL(28,8) NOT NULL DEFAULT 0,
    net_balance   DECIMAL(28,8) GENERATED ALWAYS AS (total_debits - total_credits) STORED,
    entry_count   BIGINT        NOT NULL DEFAULT 0,
    last_entry_id BIGINT,
    PRIMARY KEY (account_id, currency)
);`

func itestLedger(t *testing.T) (*LedgerService, *pgxpool.Pool, *fakePub, string) {
	t.Helper()
	if os.Getenv("EXC_PG_TEST") != "1" {
		t.Skip("set EXC_PG_TEST=1 to run Postgres/Redis integration tests")
	}
	dsn := os.Getenv("EXC_TEST_DSN")
	if dsn == "" {
		dsn = defaultTestDSN
	}
	schema := itestSchema(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Bootstrap: scratch conn creates the schema, applies 036+088 verbatim,
	// and creates the §5.3 fixture tables.
	boot, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Skipf("postgres unreachable (%v)", err)
	}
	if err := boot.Ping(ctx); err != nil {
		boot.Close(ctx)
		t.Skipf("postgres unreachable (%v)", err)
	}
	if _, err := boot.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		boot.Close(ctx)
		t.Fatalf("create schema: %v", err)
	}
	t.Cleanup(func() {
		c2, cancel2 := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel2()
		conn, err := pgx.Connect(c2, dsn)
		if err == nil {
			_, _ = conn.Exec(c2, "DROP SCHEMA IF EXISTS "+schema+" CASCADE")
			conn.Close(c2)
		}
	})
	boot.Close(ctx)

	migDir := "../db/migrations"
	migExec(t, ctx, dsn, schema, migDir+"/036_create_general_ledger.up.sql")
	migExec(t, ctx, dsn, schema, migDir+"/088_gl_chart_of_accounts.up.sql")

	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema + ",public"
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)

	// §5.3 fixture tables in the test schema (shape owned by migration 102).
	// Separate Exec calls — prepared mode cannot carry multi-statement SQL.
	for _, ddl := range strings.Split(ledgerEntriesFixture, ";") {
		ddl = strings.TrimSpace(ddl)
		if ddl == "" {
			continue
		}
		if _, err := pool.Exec(ctx, ddl); err != nil {
			t.Fatalf("fixture: %v", err)
		}
	}

	rdb := excredis.New(defaultTestRedis, os.Getenv("EXC_REDIS_TEST_PASSWORD"), 13)
	if addr := os.Getenv("EXC_REDIS_TEST_ADDR"); addr != "" {
		rdb = excredis.New(addr, os.Getenv("EXC_REDIS_TEST_PASSWORD"), 13)
	}
	if err := rdb.Ping(ctx); err != nil {
		t.Skipf("redis unreachable (%v)", err)
	}
	t.Cleanup(func() { _ = rdb.Close() })

	pub := &fakePub{}
	svc, err := NewLedgerService(pool, rdb, pub)
	if err != nil {
		t.Fatalf("service: %v", err)
	}
	return svc, pool, pub, schema
}

func TestLedgerIntegration(t *testing.T) {
	svc, pool, pub, _ := itestLedger(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	acctA, acctB := int64(999990001), int64(999990002)
	t.Cleanup(func() {
		c2, cancel2 := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel2()
		_, _ = pool.Exec(c2, "DELETE FROM public.balances WHERE account_id IN ($1,$2)", acctA, acctB)
	})

	// ── deposit posts journal + wallet ledger + journal_sums ─────────────
	res, err := svc.Post(ctx, depositTestJournal(acctA, "100"))
	if err != nil {
		t.Fatalf("deposit post: %v", err)
	}
	if !res.Committed || res.Replayed || res.JournalID == 0 || len(res.LedgerLineIDs) != 2 || len(res.LedgerEntryIDs) != 1 {
		t.Fatalf("result %+v", res)
	}
	var avail, total decimal.Decimal
	if err := pool.QueryRow(ctx,
		"SELECT available, total FROM public.balances WHERE account_id=$1 AND currency='USD'",
		acctA).Scan(&avail, &total); err != nil {
		t.Fatalf("read balance: %v", err)
	}
	if !avail.Equal(decimal.NewFromInt(100)) || !total.Equal(decimal.NewFromInt(100)) {
		t.Fatalf("balance avail=%s total=%s", avail, total)
	}
	var net decimal.Decimal
	var cnt int64
	if err := pool.QueryRow(ctx,
		"SELECT net_balance, entry_count FROM journal_sums WHERE account_id=$1 AND currency='USD'",
		acctA).Scan(&net, &cnt); err != nil {
		t.Fatalf("read journal_sums: %v", err)
	}
	if !net.Equal(total) || cnt != 1 {
		t.Fatalf("journal_sums net=%s count=%d vs total=%s", net, cnt, total)
	}
	var dir string
	var runBal decimal.Decimal
	if err := pool.QueryRow(ctx,
		"SELECT direction, running_balance FROM ledger_entries WHERE account_id=$1",
		acctA).Scan(&dir, &runBal); err != nil {
		t.Fatalf("read ledger_entries: %v", err)
	}
	if dir != "DEBIT" || !runBal.Equal(decimal.NewFromInt(100)) {
		t.Fatalf("ledger_entries dir=%s running=%s", dir, runBal)
	}
	if len(pub.subjects) != 1 || pub.subjects[0] != "account.balance.changed.999990001" {
		t.Fatalf("events %v", pub.subjects)
	}

	// ── internal transfer A→B: zero GL bypass path, two wallet entries ───
	j := ledger.Journal{
		EntryType: ledger.EntryTransfer, ReferenceID: 5,
		Description: "internal transfer", PostedBy: "test",
		Lines: []ledger.Line{
			ledger.DebitLine(ledger.CustomerLiability("USD"), "USD", decimal.NewFromInt(25), "debit A"),
			ledger.CreditLine(ledger.CustomerLiability("USD"), "USD", decimal.NewFromInt(25), "credit B"),
		},
		Effects: []ledger.AccountEffect{
			{AccountID: acctA, Currency: "USD", AvailableDelta: decimal.NewFromInt(-25)},
			{AccountID: acctB, Currency: "USD", AvailableDelta: decimal.NewFromInt(25)},
		},
	}
	res, err = svc.Post(ctx, j)
	if err != nil {
		t.Fatalf("transfer post: %v", err)
	}
	if len(res.LedgerEntryIDs) != 2 || len(pub.subjects) != 3 {
		t.Fatalf("transfer res=%+v subjects=%v", res, pub.subjects)
	}
	if err := pool.QueryRow(ctx,
		"SELECT total FROM public.balances WHERE account_id=$1 AND currency='USD'",
		acctA).Scan(&total); err != nil {
		t.Fatalf("read A: %v", err)
	}
	if !total.Equal(decimal.NewFromInt(75)) {
		t.Fatalf("A total=%s want 75", total)
	}
	// journal_sums tracks both accounts.
	if err := pool.QueryRow(ctx,
		"SELECT net_balance FROM journal_sums WHERE account_id=$1 AND currency='USD'",
		acctB).Scan(&net); err != nil {
		t.Fatalf("journal_sums B: %v", err)
	}
	if !net.Equal(decimal.NewFromInt(25)) {
		t.Fatalf("B net=%s want 25", net)
	}

	// ── insufficient funds: atomic reject, nothing persisted ─────────────
	j = depositTestJournal(acctB, "10")
	j.EntryType = ledger.EntryWithdrawal
	j.Effects[0].AvailableDelta = decimal.NewFromInt(-60) // B holds 25
	j.Lines = []ledger.Line{
		ledger.DebitLine(ledger.CustomerLiability("USD"), "USD", decimal.NewFromInt(60), ""),
		ledger.CreditLine(ledger.Nostro("USD"), "USD", decimal.NewFromInt(60), ""),
	}
	_, err = svc.Post(ctx, j)
	requireCode(t, err, ledger.CodeInsufficientBalance)
	if err := pool.QueryRow(ctx,
		"SELECT total FROM public.balances WHERE account_id=$1 AND currency='USD'",
		acctB).Scan(&total); err != nil {
		t.Fatalf("read B: %v", err)
	}
	if !total.Equal(decimal.NewFromInt(25)) {
		t.Fatalf("B total=%s — rollback failed", total)
	}

	// ── unknown account aborts fail-closed ───────────────────────────────
	j = depositTestJournal(acctA, "1")
	j.Lines[0].AccountCode = "9999_NONEXISTENT_USD"
	_, err = svc.Post(ctx, j)
	requireCode(t, err, ledger.CodeLedgerUnknownAccount)

	// ── imbalanced journal aborts at validation (never reaches the DB) ───
	j = depositTestJournal(acctA, "1")
	j.Lines[1].Credit = decimal.RequireFromString("0.5")
	_, err = svc.Post(ctx, j)
	requireCode(t, err, ledger.CodeLedgerImbalanceAbort)

	// ── the DB trigger independently enforces zero-sum at commit ─────────
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	var jid int64
	if err := tx.QueryRow(ctx,
		`INSERT INTO journal_entries (entry_type, reference_id, description, posted_by)
		 VALUES ('DEPOSIT', 1, 'bad', 'test') RETURNING id`).Scan(&jid); err != nil {
		t.Fatalf("insert journal: %v", err)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO ledger_lines (journal_entry_id, account_code, debit_amount, credit_amount, currency)
		 VALUES ($1,'1010_NOSTRO_USD',10,0,'USD'),($1,'2010_CUSTOMER_LIABILITY_USD',0,9,'USD')`,
		jid); err != nil {
		t.Fatalf("insert lines: %v", err)
	}
	err = tx.Commit(ctx)
	if err == nil || !strings.Contains(err.Error(), "LEDGER_IMBALANCE_ABORT") {
		t.Fatalf("zero-sum trigger did not fire: %v", err)
	}

	// ── idempotent replay: same key + payload resolves, no double-apply ──
	j = depositTestJournal(acctA, "5")
	j.IdempotencyKey = "test-idem-1"
	res1, err := svc.Post(ctx, j)
	if err != nil {
		t.Fatalf("post idem: %v", err)
	}
	res2, err := svc.Post(ctx, j)
	if err != nil {
		t.Fatalf("replay idem: %v", err)
	}
	if !res2.Replayed || res2.JournalID != res1.JournalID {
		t.Fatalf("replay res %+v vs %+v", res2, res1)
	}
	if err := pool.QueryRow(ctx,
		"SELECT total FROM public.balances WHERE account_id=$1 AND currency='USD'",
		acctA).Scan(&total); err != nil {
		t.Fatalf("read A: %v", err)
	}
	if !total.Equal(decimal.NewFromInt(80)) { // 75 + 5 once
		t.Fatalf("A total=%s — replay double-applied", total)
	}
	// same key, different payload → mismatch
	j.Description = "tampered"
	_, err = svc.Post(ctx, j)
	requireCode(t, err, ledger.CodeIdempotencyMismatch)

	// ── swap accrual journal end-to-end (Task 3.3.19 seam) ───────────────
	accr, err := ledger.ComputeSwapAccrual(ledger.SwapAccrualInput{
		AccountID: acctA, PositionID: 1, InstrumentID: 1, Symbol: "EUR/USD",
		Side: ledger.SwapLong, InterbankAmount: decimal.RequireFromString("-4"),
		Notional: decimal.NewFromInt(1000000), MarkupBps: decimal.NewFromInt(10),
		AccrualCurrency: "USD", Days: 1, ReferenceID: 77, PostedBy: "rollover-test",
	})
	if err != nil {
		t.Fatalf("accrual: %v", err)
	}
	res, err = svc.Post(ctx, *accr.Journal)
	if err != nil {
		t.Fatalf("accrual post: %v", err)
	}
	if len(res.LedgerLineIDs) != 4 {
		t.Fatalf("accrual lines %d — interbank+markup must post as separate legs",
			len(res.LedgerLineIDs))
	}
	var recID int64
	if err := pool.QueryRow(ctx, ledger.SwapAccrualInsertSQL,
		accr.Record.AccountID, accr.Record.PositionID, accr.Record.InstrumentID,
		accr.Record.Symbol, string(accr.Record.Side), accr.Record.Currency,
		accr.Record.Days, string(accr.Record.DayCount),
		accr.Record.InterbankAmount.String(), accr.Record.MarkupAmount.String(),
		accr.Record.ClientDelta.String(), accr.Record.MarkupBps.String(),
		accr.Record.SwapFree, accr.Record.ForegoneAmount.String(),
		accr.Record.Narrative, res.JournalID).Scan(&recID); err != nil {
		t.Fatalf("accrual record: %v", err)
	}
	if recID == 0 {
		t.Fatal("accrual record id 0")
	}

	// ── PostJournal inside a caller-owned tx (spec's postJournal seam) ────
	tx, err = pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		t.Fatalf("begin serializable: %v", err)
	}
	j = depositTestJournal(acctB, "10")
	res, err = svc.PostJournal(ctx, tx, j)
	if err != nil {
		t.Fatalf("postJournal: %v", err)
	}
	if res.Committed {
		t.Fatal("tx-scoped post must not claim commit before the caller commits")
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("caller commit: %v", err)
	}
	if err := pool.QueryRow(ctx,
		"SELECT total FROM public.balances WHERE account_id=$1 AND currency='USD'",
		acctB).Scan(&total); err != nil {
		t.Fatalf("read B: %v", err)
	}
	if !total.Equal(decimal.NewFromInt(35)) { // 25 + 10
		t.Fatalf("B total=%s want 35", total)
	}
}
