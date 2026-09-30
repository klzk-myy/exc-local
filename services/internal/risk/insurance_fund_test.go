// insurance_fund_test.go — unit + gated integration coverage for the
// InsuranceFundService (Phase-19 Task 19.3.4 / 19.3.14; spec §5.15,
// §13.4–§13.6c).
//
// Ungated legs cover constructor fail-closed wiring, the per-reason GL
// journal shapes (movementJournal), input validation, the §13.6
// depletion-floor configuration, alert severity plumbing, the
// retryable-error classifier and the balance-event fan-out.
//
// Gated legs (same conventions as liquidation_store_test.go):
//
//	EXC_PG_TEST=1    go test ./internal/risk/ -run 'TestPgInsuranceFund' -v
//	EXC_PG_TEST=1 EXC_REDIS_TEST=1 \
//	    go test ./internal/risk/ -run 'TestPgRedisInsuranceFund' -v
package risk

import (
	"context"
	stderrors "errors"
	"fmt"
	"os"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"exchange/internal/ledger"
	excredis "exchange/internal/redis"
	"exchange/pkg/decimal"
)

// ---------------------------------------------------------------------------
// Construction — fail closed on missing deps
// ---------------------------------------------------------------------------

func TestInsuranceFundNewFailClosed(t *testing.T) {
	if _, err := NewInsuranceFundService(InsuranceFundDeps{
		Poster: &fakeNBPPoster{},
	}); err == nil {
		t.Fatal("nil pool must fail construction")
	}
	// pgxpool.New is lazy — it never dials, so a non-nil handle suffices
	// to prove the poster nil-guard without a live database.
	pool, err := pgxpool.New(context.Background(), liqStoreDSN())
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer pool.Close()
	if _, err := NewInsuranceFundService(InsuranceFundDeps{
		Pool: pool,
	}); err == nil {
		t.Fatal("nil journal poster must fail construction (spec §5.3)")
	}
}

// ---------------------------------------------------------------------------
// movementJournal — balanced GL per reason
// ---------------------------------------------------------------------------

func testFundSvc() *InsuranceFundService {
	return &InsuranceFundService{cfg: InsuranceFundConfig{}.normalize()}
}

func TestInsuranceFundJournalLiquidationPenalty(t *testing.T) {
	svc := testFundSvc()
	j, err := svc.movementJournal(FundMovement{
		Reason: FundReasonLiquidationPenalty, Currency: "USD",
		Amount: d("123.45"), ReferenceType: "liquidation", ReferenceID: 55,
		AccountID: 7, IdempotencyKey: "liq-penalty:55",
	}, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(j.Lines) != 2 {
		t.Fatalf("lines: %+v", j.Lines)
	}
	if j.Lines[0].AccountCode != ledger.CustomerLiability("USD") ||
		j.Lines[1].AccountCode != ledger.InsuranceFundLiability("USD") {
		t.Fatalf("penalty GL pair wrong: %+v", j.Lines)
	}
	if !j.Lines[0].Debit.Equal(d("123.45")) || !j.Lines[1].Credit.Equal(d("123.45")) {
		t.Fatalf("penalty amounts: %+v", j.Lines)
	}
	// The liquidated client's wallet is debited (allow-negative: the
	// account can be driven below zero by the penalty charge).
	if len(j.Effects) != 1 || j.Effects[0].AccountID != 7 ||
		!j.Effects[0].AvailableDelta.Equal(d("-123.45")) ||
		!j.Effects[0].AllowNegative {
		t.Fatalf("penalty effect: %+v", j.Effects)
	}
	if j.IdempotencyKey != "liq-penalty:55" {
		t.Fatalf("idempotency key not propagated: %q", j.IdempotencyKey)
	}
	if j.EntryType != ledger.EntryLiquidation {
		t.Fatalf("entry type %q, want LIQUIDATION", j.EntryType)
	}
}

func TestInsuranceFundJournalHouseFunding(t *testing.T) {
	svc := testFundSvc()
	for _, reason := range []string{
		FundReasonCapitalInjection, FundReasonContingentFacility, FundReasonManualAdjustment,
	} {
		j, err := svc.movementJournal(FundMovement{
			Reason: reason, Currency: "USD", Amount: d("1000"),
		}, true)
		if err != nil {
			t.Fatalf("%s: %v", reason, err)
		}
		if j.Lines[0].AccountCode != ledger.InsuranceFundNostro("USD") ||
			j.Lines[1].AccountCode != ledger.InsuranceFundLiability("USD") {
			t.Fatalf("%s GL pair: %+v", reason, j.Lines)
		}
		if len(j.Effects) != 0 {
			t.Fatalf("%s must be GL-only (no wallet effects)", reason)
		}
		if j.EntryType != ledger.EntryAdjustment {
			t.Fatalf("%s entry type %q, want ADJUSTMENT", reason, j.EntryType)
		}
	}
	// Retained-earnings sweep is its own GL pair.
	j, err := svc.movementJournal(FundMovement{
		Reason: FundReasonRetainedEarnings, Currency: "EUR", Amount: d("500"),
	}, true)
	if err != nil {
		t.Fatal(err)
	}
	if j.Lines[0].AccountCode != ledger.RetainedEarnings("EUR") ||
		j.Lines[1].AccountCode != ledger.InsuranceFundLiability("EUR") {
		t.Fatalf("sweep GL pair: %+v", j.Lines)
	}
}

func TestInsuranceFundJournalDebits(t *testing.T) {
	svc := testFundSvc()
	// Manual withdrawal → house nostro, GL-only.
	j, err := svc.movementJournal(FundMovement{
		Reason: FundReasonManualAdjustment, Currency: "USD", Amount: d("250"),
	}, false)
	if err != nil {
		t.Fatal(err)
	}
	if j.Lines[0].AccountCode != ledger.InsuranceFundLiability("USD") ||
		j.Lines[1].AccountCode != ledger.InsuranceFundNostro("USD") ||
		len(j.Effects) != 0 {
		t.Fatalf("manual debit: %+v", j)
	}
	// Client-facing debits credit the wallet.
	for _, reason := range []string{
		FundReasonLPRebate, FundReasonNBPRestitution, FundReasonAuctionDeficiency,
	} {
		j, err := svc.movementJournal(FundMovement{
			Reason: reason, Currency: "USD", Amount: d("75"), AccountID: 9,
		}, false)
		if err != nil {
			t.Fatalf("%s: %v", reason, err)
		}
		if j.Lines[0].AccountCode != ledger.InsuranceFundLiability("USD") ||
			j.Lines[1].AccountCode != ledger.CustomerLiability("USD") {
			t.Fatalf("%s GL pair: %+v", reason, j.Lines)
		}
		if len(j.Effects) != 1 || j.Effects[0].AccountID != 9 ||
			!j.Effects[0].AvailableDelta.Equal(d("75")) {
			t.Fatalf("%s effect: %+v", reason, j.Effects)
		}
	}
}

func TestInsuranceFundMovementValidation(t *testing.T) {
	svc := testFundSvc()
	// Non-positive amount / malformed currency reject before any I/O.
	for _, m := range []FundMovement{
		{Reason: FundReasonCapitalInjection, Currency: "USD", Amount: decimal.Zero},
		{Reason: FundReasonCapitalInjection, Currency: "USD", Amount: d("-1")},
		{Reason: FundReasonCapitalInjection, Currency: "US", Amount: d("1")},
		{Reason: FundReasonCapitalInjection, Currency: "USDX", Amount: d("1")},
	} {
		if _, err := svc.Credit(context.Background(), m); err == nil {
			t.Fatalf("movement %+v must reject", m)
		}
	}
	// Wallet effect with no account → journal validation rejects.
	if _, err := svc.Debit(context.Background(), FundMovement{
		Reason: FundReasonLPRebate, Currency: "USD", Amount: d("10"), AccountID: 0,
	}); err == nil {
		t.Fatal("wallet effect without account must reject")
	}
}

// ---------------------------------------------------------------------------
// Configuration + helpers
// ---------------------------------------------------------------------------

func TestInsuranceFundCanonicalDefaults(t *testing.T) {
	// §13.6 canonical values — regression anchors.
	if !DefaultFundLowBalance.Equal(d("100000")) {
		t.Fatalf("low watermark %s, want 100000", DefaultFundLowBalance)
	}
	if !DefaultADLNegativeFloor.Equal(d("-100000")) {
		t.Fatalf("ADL floor %s, want -100000", DefaultADLNegativeFloor)
	}
	if !DefaultADLEquityFraction.Equal(d("0.01")) {
		t.Fatalf("equity fraction %s, want 0.01", DefaultADLEquityFraction)
	}
	if !DefaultLPRebateFraction.Equal(d("0.0005")) {
		t.Fatalf("LP rebate %s, want 0.0005", DefaultLPRebateFraction)
	}
	cfg := InsuranceFundConfig{}.normalize()
	if !cfg.LowBalanceAlertUSD.Equal(DefaultFundLowBalance) ||
		!cfg.ADLNegativeFloor.Equal(DefaultADLNegativeFloor) ||
		!cfg.ADLEquityFraction.Equal(DefaultADLEquityFraction) ||
		cfg.PostedBy == "" || cfg.MaxAttempts != 3 {
		t.Fatalf("normalize: %+v", cfg)
	}
}

func TestFundMovementDirection(t *testing.T) {
	m := FundMovement{}
	if m.Direction(true) != "CREDIT" || m.Direction(false) != "DEBIT" {
		t.Fatal("direction labels wrong")
	}
}

func TestIsFundRetryable(t *testing.T) {
	if !isFundRetryable(&pgconn.PgError{Code: "40001"}) {
		t.Fatal("serialization failure must retry")
	}
	if !isFundRetryable(&pgconn.PgError{Code: "40P01"}) {
		t.Fatal("deadlock must retry")
	}
	if isFundRetryable(&pgconn.PgError{Code: "23505"}) {
		t.Fatal("unique violation must not retry")
	}
	if isFundRetryable(fmt.Errorf("plain")) {
		t.Fatal("non-pg error must not retry")
	}
	if !isFundRetryable(fmt.Errorf("wrap: %w", &pgconn.PgError{Code: "40001"})) {
		t.Fatal("wrapped pg error must still classify")
	}
}

// fundPubSpy captures FundEventPublisher.Publish calls.
type fundPubSpy struct {
	mu       sync.Mutex
	subjects []string
	err      error
}

func (p *fundPubSpy) Publish(_ context.Context, subject string, _ []byte) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.subjects = append(p.subjects, subject)
	return p.err
}

func TestDispatchBalanceEvents(t *testing.T) {
	events := []ledger.BalanceEvent{
		{AccountID: 7, Currency: "USD", EventType: "BALANCE_CHANGED"},
		{AccountID: 9, Currency: "EUR", EventType: "BALANCE_CHANGED"},
	}
	// nil publisher is a no-op — never panics.
	DispatchBalanceEvents(context.Background(), nil, events, func(string, ...any) {})

	pub := &fundPubSpy{}
	DispatchBalanceEvents(context.Background(), pub, events, func(string, ...any) {})
	if len(pub.subjects) != 2 ||
		pub.subjects[0] != ledger.BalanceChangedSubject(7) ||
		pub.subjects[1] != ledger.BalanceChangedSubject(9) {
		t.Fatalf("subjects: %v", pub.subjects)
	}
	// Publish failures are logged and swallowed — post-commit best effort.
	var logged int
	pub.err = fmt.Errorf("nats down")
	DispatchBalanceEvents(context.Background(), pub, events,
		func(string, ...any) { logged++ })
	if logged != 2 {
		t.Fatalf("log calls=%d, want 2", logged)
	}
}

// ---------------------------------------------------------------------------
// Gated PostgreSQL — the atomic write path
// ---------------------------------------------------------------------------

// fundPgPoster writes REAL journal_entries rows inside the caller tx so
// insurance_fund_transactions.journal_entry_id satisfies its FK. On an
// idempotency_key collision it resolves to the original journal —
// mirroring settlement.LedgerService.PostJournal replay semantics.
type fundPgPoster struct {
	inserts int
	replays int
}

func (p *fundPgPoster) Post(context.Context, ledger.Journal) (ledger.PostResult, error) {
	return ledger.PostResult{}, stderrors.New("fundPgPoster: tx-scoped only")
}

func (p *fundPgPoster) PostJournal(ctx context.Context, tx pgx.Tx,
	j ledger.Journal) (ledger.PostResult, error) {
	if err := j.Validate(); err != nil {
		return ledger.PostResult{}, err
	}
	var key any
	if j.IdempotencyKey != "" {
		key = j.IdempotencyKey
	}
	var id int64
	err := tx.QueryRow(ctx, `
		INSERT INTO journal_entries
		    (entry_type, reference_id, description, posted_by, idempotency_key)
		VALUES ($1::gl_entry_type_enum, NULLIF($2,0), $3, $4, $5)
		ON CONFLICT (idempotency_key) WHERE idempotency_key IS NOT NULL
		DO NOTHING RETURNING id`,
		string(j.EntryType), j.ReferenceID, j.Description, j.PostedBy, key).
		Scan(&id)
	if err == pgx.ErrNoRows {
		var eid int64
		if qerr := tx.QueryRow(ctx, `
			SELECT id FROM journal_entries WHERE idempotency_key = $1`,
			j.IdempotencyKey).Scan(&eid); qerr != nil {
			return ledger.PostResult{}, qerr
		}
		p.replays++
		return ledger.PostResult{JournalID: eid, Committed: true, Replayed: true}, nil
	}
	if err != nil {
		return ledger.PostResult{}, err
	}
	p.inserts++
	return ledger.PostResult{JournalID: id, Committed: true}, nil
}

func newFundService(t *testing.T, pool *pgxpool.Pool, poster JournalPoster,
	rdb *excredis.Client, alerts OpsAlerter) *InsuranceFundService {
	t.Helper()
	svc, err := NewInsuranceFundService(InsuranceFundDeps{
		Pool: pool, Poster: poster, Redis: rdb, Alerter: alerts,
	})
	if err != nil {
		t.Fatalf("fund service: %v", err)
	}
	return svc
}

func TestPgInsuranceFundMovements(t *testing.T) {
	pool := pgRisk19Fixture(t)
	ctx := context.Background()
	poster := &fundPgPoster{}
	spy := &opsAlertSpy{}
	svc := newFundService(t, pool, poster, nil, spy)

	// Never-touched currency → nil balance, not an error.
	if b, err := svc.Balance(ctx, "JPY"); err != nil || b != nil {
		t.Fatalf("untouched currency: %v %+v", err, b)
	}

	res, err := svc.Credit(ctx, FundMovement{
		Reason: FundReasonCapitalInjection, Currency: "USD",
		Amount: d("5000000"), ReferenceType: "manual", ReferenceID: 1,
		IdempotencyKey: "itest-seed-1",
	})
	if err != nil {
		t.Fatalf("credit: %v", err)
	}
	if !res.BalanceAfter.Equal(d("5000000")) || res.MovementID == 0 || res.JournalID == 0 {
		t.Fatalf("credit result: %+v", res)
	}
	res2, err := svc.Debit(ctx, FundMovement{
		Reason: FundReasonManualAdjustment, Currency: "USD",
		Amount: d("1500000"), ReferenceType: "manual", ReferenceID: 2,
		IdempotencyKey: "itest-withdraw-1",
	})
	if err != nil {
		t.Fatalf("debit: %v", err)
	}
	if !res2.BalanceAfter.Equal(d("3500000")) {
		t.Fatalf("debit balance=%s, want 3500000", res2.BalanceAfter)
	}

	b, err := svc.Balance(ctx, "USD")
	if err != nil || b == nil || !b.Balance.Equal(d("3500000")) {
		t.Fatalf("balance read: %v %+v", err, b)
	}
	all, err := svc.Balances(ctx)
	if err != nil || len(all) != 1 || all[0].Currency != "USD" {
		t.Fatalf("balances: %v %+v", err, all)
	}
	hist, err := svc.History(ctx, "USD", 0, 10)
	if err != nil || len(hist) != 2 {
		t.Fatalf("history: %v %d", err, len(hist))
	}
	if hist[0].Direction != "DEBIT" || hist[1].Direction != "CREDIT" ||
		!hist[0].BalanceAfter.Equal(d("3500000")) {
		t.Fatalf("history rows wrong (want newest-first): %+v", hist)
	}
	if hist[0].JournalEntryID == nil || *hist[0].JournalEntryID != res2.JournalID {
		t.Fatalf("journal linkage: %+v", hist[0])
	}
}

func TestPgInsuranceFundDepleted(t *testing.T) {
	pool := pgRisk19Fixture(t)
	ctx := context.Background()
	poster := &fundPgPoster{}
	spy := &opsAlertSpy{}
	svc := newFundService(t, pool, poster, nil, spy)
	if _, err := svc.Credit(ctx, FundMovement{
		Reason: FundReasonCapitalInjection, Currency: "USD",
		Amount: d("4000000"), IdempotencyKey: "itest-dep-seed",
	}); err != nil {
		t.Fatal(err)
	}

	// Governance row seeded by mig 230 with target_balance NULL → the
	// -$100K absolute floor applies; a positive balance is never depleted.
	dep, bal, err := svc.Depleted(ctx, "USD")
	if err != nil || dep || !bal.Equal(d("4000000")) {
		t.Fatalf("depleted=%v bal=%s err=%v, want false/4M", dep, bal, err)
	}

	// §13.6: floor = max(-$100K, 1% × target). target $10M → floor $100K —
	// the fund is depleted BEFORE literal zero.
	if _, err := pool.Exec(ctx, `
		UPDATE insurance_fund_governance SET target_balance = 10000000
		WHERE currency = 'USD'`); err != nil {
		t.Fatal(err)
	}
	dep, _, err = svc.Depleted(ctx, "USD")
	if err != nil || dep {
		t.Fatalf("4M > 100K floor must not be depleted: %v %v", dep, err)
	}
	if _, err := svc.Debit(ctx, FundMovement{
		Reason: FundReasonManualAdjustment, Currency: "USD",
		Amount: d("3950000"), IdempotencyKey: "itest-dep-draw",
	}); err != nil {
		t.Fatal(err)
	}
	dep, bal, err = svc.Depleted(ctx, "USD")
	if err != nil || !dep {
		t.Fatalf("50K < 100K floor must be depleted: %v %s %v", dep, bal, err)
	}
	// The post-commit alert evaluation fired both watermarks.
	codes := map[string]int{}
	for _, a := range spy.alerts {
		codes[a.Code]++
	}
	if codes[codeInsuranceFundLowBalance] == 0 {
		t.Fatalf("low-balance P1 alert missing: %v", spy.alerts)
	}
	if codes[CodeInsuranceFundDepleted] == 0 {
		t.Fatalf("depletion L1 alert missing: %v", spy.alerts)
	}

	// A per-currency depletion_threshold on the fund row overrides both
	// the absolute floor and the governance fraction: threshold 40000
	// CLEARS depletion even though the 1%-of-target floor says 100K.
	if _, err := pool.Exec(ctx, `
		UPDATE insurance_fund SET depletion_threshold = 40000 WHERE currency='USD'`); err != nil {
		t.Fatal(err)
	}
	dep, _, err = svc.Depleted(ctx, "USD")
	if err != nil || dep {
		t.Fatalf("50K > 40K override must clear depletion: %v %v", dep, err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE insurance_fund SET depletion_threshold = 60000 WHERE currency='USD'`); err != nil {
		t.Fatal(err)
	}
	dep, _, err = svc.Depleted(ctx, "USD")
	if err != nil || !dep {
		t.Fatalf("50K < 60K override must re-arm depletion: %v %v", dep, err)
	}
}

func TestPgInsuranceFundIdempotentReplay(t *testing.T) {
	pool := pgRisk19Fixture(t)
	ctx := context.Background()
	poster := &fundPgPoster{}
	svc := newFundService(t, pool, poster, nil, nil)

	m := FundMovement{
		Reason: FundReasonCapitalInjection, Currency: "USD",
		Amount: d("100"), ReferenceType: "liquidation", ReferenceID: 42,
		IdempotencyKey: "itest-idem-1",
	}
	res1, err := svc.Credit(ctx, m)
	if err != nil {
		t.Fatal(err)
	}
	res2, err := svc.Credit(ctx, m)
	if err != nil {
		t.Fatalf("replayed movement: %v", err)
	}
	// GL replay dedup: the same idempotency key resolves to the ORIGINAL
	// journal — no second journal_entries row can exist for the key.
	if res2.JournalID != res1.JournalID {
		t.Fatalf("replay must reuse journal %d, got %d", res1.JournalID, res2.JournalID)
	}
	var journals int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM journal_entries WHERE idempotency_key='itest-idem-1'`).
		Scan(&journals); err != nil {
		t.Fatal(err)
	}
	if journals != 1 || poster.inserts != 1 || poster.replays != 1 {
		t.Fatalf("journal dedup: rows=%d inserts=%d replays=%d",
			journals, poster.inserts, poster.replays)
	}
}

// TestPgRedisInsuranceFundWalletEffect drives a client-facing debit
// (LP_REBATE) through the real Redis account-lock path. Requires both
// gates: EXC_PG_TEST=1 EXC_REDIS_TEST=1.
func TestPgRedisInsuranceFundWalletDebit(t *testing.T) {
	if os.Getenv("EXC_REDIS_TEST") != "1" {
		t.Skip("EXC_REDIS_TEST not set")
	}
	pool := pgRisk19Fixture(t)
	ctx := context.Background()
	addr := os.Getenv("EXC_REDIS_TEST_ADDR")
	if addr == "" {
		addr = "127.0.0.1:16379"
	}
	rdb := excredis.New(addr, os.Getenv("EXC_REDIS_TEST_PASSWORD"), 14)
	if err := rdb.Ping(ctx); err != nil {
		t.Skipf("redis unreachable: %v", err)
	}
	defer func() { _ = rdb.Close() }()

	acct := liqSeedAccount(t, pool, "RETAIL")
	poster := &fundPgPoster{}
	svc := newFundService(t, pool, poster, rdb, &opsAlertSpy{})
	if _, err := svc.Credit(ctx, FundMovement{
		Reason: FundReasonCapitalInjection, Currency: "USD",
		Amount: d("1000"), IdempotencyKey: "itest-wallet-seed",
	}); err != nil {
		t.Fatal(err)
	}
	res, err := svc.Debit(ctx, FundMovement{
		Reason: FundReasonLPRebate, Currency: "USD",
		Amount: d("40"), AccountID: acct, ReferenceType: "auction",
		ReferenceID: 9, IdempotencyKey: "itest-wallet-debit",
	})
	if err != nil {
		t.Fatalf("wallet debit: %v", err)
	}
	if !res.BalanceAfter.Equal(d("960")) {
		t.Fatalf("balance=%s, want 960", res.BalanceAfter)
	}
	// The account lock was released after commit — a fresh try-lock wins.
	ok, err := rdb.TryLockAccount(ctx, fmt.Sprint(acct), "probe", excredis.AccountLockTTL)
	if err != nil || !ok {
		t.Fatalf("account lock must be released: %v %v", ok, err)
	}
	if _, err := rdb.UnlockAccount(ctx, fmt.Sprint(acct), "probe"); err != nil {
		t.Fatalf("probe unlock: %v", err)
	}
	// Lock contention rejects the movement fail-closed.
	token := "held-by-other"
	if ok, err := rdb.TryLockAccount(ctx, fmt.Sprint(acct), token,
		excredis.AccountLockTTL); err != nil || !ok {
		t.Fatalf("setup lock: %v %v", ok, err)
	}
	defer func() { _, _ = rdb.UnlockAccount(ctx, fmt.Sprint(acct), token) }()
	if _, err := svc.Debit(ctx, FundMovement{
		Reason: FundReasonLPRebate, Currency: "USD",
		Amount: d("5"), AccountID: acct, IdempotencyKey: "itest-wallet-busy",
	}); err == nil {
		t.Fatal("held account lock must reject the movement")
	}
}
