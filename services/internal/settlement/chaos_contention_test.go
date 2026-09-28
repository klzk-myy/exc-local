// chaos_contention_test.go — Phase-04.5 Task 4.5.3.2 scenario A
// (spec §2.7, §5.40, §24 #303): PostgreSQL SERIALIZABLE contention chaos.
//
// 1,000 concurrent balance mutations are fired at the SAME two accounts so
// the SERIALIZABLE engine itself is the contention arbiter and real
// SQLSTATE 40001/40P01 aborts are induced. The §5.3 Redis account mutexes
// are deliberately bypassed here (a nil-work locker) because a correctly
// held mutex would serialize the writers and no SSI conflict could ever
// occur — the mutex layer's own contention behavior is covered by
// TestMutexContentionAbortsBeforeTx; this test proves the layer UNDER it:
// every conflict must either cleanly retry to commit or abort atomically
// with zero phantom balance effects.
//
// Zero-loss contract verified post-hoc against the DB (not the callers'
// self-reported outcomes):
//
//	committed(trade_id) == processed_trades rows          (no silent loss)
//	journal_entries/ledger_entries counts == committed    (no phantom rows)
//	balances.total == seed + delta·committed  AND  == journal_sums.net_balance
//
// Run:
//
//	EXC_PG_TEST=1 EXC_PG_DSN='postgres://...' EXC_REDIS_TEST_ADDR=127.0.0.1:16379 \
//	  go test ./internal/settlement -run ChaosSerialization -v -count=1
package settlement

import (
	"context"
	stderrors "errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"exchange/internal/ledger"
	excredis "exchange/internal/redis"
	"exchange/pkg/decimal"
	excerrors "exchange/pkg/errors"
)

// chaosPool is testPool with an explicit MaxConns — the default pool size
// (4) would serialize the workers in the pgx pool and starve the
// SERIALIZABLE engine of the concurrency needed to induce 40001 storms.
func chaosPool(t *testing.T, maxConns int32) *pgxpool.Pool {
	t.Helper()
	if os.Getenv("EXC_PG_TEST") != "1" {
		t.Skip("set EXC_PG_TEST=1 to run PostgreSQL integration tests")
	}
	dsn := os.Getenv("EXC_PG_DSN")
	if dsn == "" {
		dsn = "postgres://exchange:exchange_dev@localhost:5433/exchange?sslmode=disable"
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	cfg.MaxConns = maxConns
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
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

// chaosCountingStore wraps the production pgxBalanceStore and tallies the
// SQLSTATE of every failed attempt — the "conflicts observed" evidence.
// It deliberately does NOT implement setBasedStore, keeping the serial
// per-fill commit path (the one that consumes the most conflicting rows).
type chaosCountingStore struct {
	inner pgxBalanceStore

	commits        atomic.Int64 // successful InTx
	serialization  atomic.Int64 // SQLSTATE 40001
	deadlocks      atomic.Int64 // SQLSTATE 40P01
	nonConflictErr atomic.Int64 // in-tx failures that are NOT 40001/40P01
}

func (s *chaosCountingStore) InTx(ctx context.Context, fn func(ctx context.Context, tx balanceTx) error) error {
	err := s.inner.InTx(ctx, fn)
	if err == nil {
		s.commits.Add(1)
		return nil
	}
	var pgErr *pgconn.PgError
	switch {
	case stderrors.As(err, &pgErr) && pgErr.Code == "40001":
		s.serialization.Add(1)
	case stderrors.As(err, &pgErr) && pgErr.Code == "40P01":
		s.deadlocks.Add(1)
	default:
		s.nonConflictErr.Add(1)
	}
	return err
}

// chaosNoopLocker admits every waiter: the §5.3 Redis mutex deliberately
// disabled so concurrent writers reach the SERIALIZABLE engine.
type chaosNoopLocker struct{}

func (chaosNoopLocker) LockAccounts(_ context.Context, ids []int64, _ string) ([]int64, error) {
	return ids, nil
}
func (chaosNoopLocker) UnlockAccounts(_ []int64, _ string) {}

// chaosCountingDispatch counts dispatched BalanceChanged events (atomic —
// hundreds of goroutines dispatch concurrently).
type chaosCountingDispatch struct{ n atomic.Int64 }

func (d *chaosCountingDispatch) Dispatch(_ context.Context, evs []ledger.BalanceEvent) error {
	d.n.Add(int64(len(evs)))
	return nil
}

// chaosSeedAccount creates a user+account with EUR+USD balances and the
// matching journal_sums genesis rows (net_balance must equal total — the
// §5.3 invariant the commit path asserts in-tx).
func chaosSeedAccount(t *testing.T, pool *pgxpool.Pool, mark int64, tag string, avail, locked string) int64 {
	t.Helper()
	ctx := context.Background()
	var uid, aid int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO users (email,status) VALUES ($1,'ACTIVE') RETURNING id`,
		fmt.Sprintf("chaos_%s_%d@x", tag, mark)).Scan(&uid); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO accounts (user_id,account_type) VALUES ($1,'MARGIN') RETURNING id`, uid).
		Scan(&aid); err != nil {
		t.Fatalf("seed account: %v", err)
	}
	total := dec(avail).Add(dec(locked))
	for _, ccy := range []string{"EUR", "USD"} {
		if _, err := pool.Exec(ctx,
			`INSERT INTO balances (account_id,currency,available,locked,version)
			 VALUES ($1,$2,$3,$4,0)`, aid, ccy, avail, locked); err != nil {
			t.Fatalf("seed balance: %v", err)
		}
		// Genesis sum: net_balance = debits − credits = total.
		if _, err := pool.Exec(ctx,
			`INSERT INTO journal_sums (account_id,currency,total_debits,total_credits)
			 VALUES ($1,$2,$3,0)`, aid, ccy, total.String()); err != nil {
			t.Fatalf("seed journal_sums: %v", err)
		}
	}
	t.Cleanup(func() {
		ctx := context.Background()
		pool.Exec(ctx, `DELETE FROM journal_sums WHERE account_id=$1`, aid)
		pool.Exec(ctx, `DELETE FROM balances WHERE account_id=$1`, aid)
		pool.Exec(ctx, `DELETE FROM accounts WHERE id=$1`, aid)
		pool.Exec(ctx, `DELETE FROM users WHERE id=$1`, uid)
	})
	return aid
}

func chaosRMFill(tradeID uint64, buyer, seller int64) ResolvedTrade {
	rt := resolvedRM(tradeID, buyer, seller)
	rt.Fill.Price = dec("1.10")
	rt.Fill.Qty = dec("100")
	rt.BuyerFee = dec("0.5")  // EUR — from base received
	rt.SellerFee = dec("2.5") // USD — from quote proceeds
	return rt
}

func TestChaosSerializationContentionZeroLoss(t *testing.T) {
	pool := chaosPool(t, 32)
	rdb := excredis.New(defaultTestRedis, os.Getenv("EXC_REDIS_TEST_PASSWORD"), 0)
	if addr := os.Getenv("EXC_REDIS_TEST_ADDR"); addr != "" {
		rdb = excredis.New(addr, os.Getenv("EXC_REDIS_TEST_PASSWORD"), 0)
	}
	ctx := context.Background()
	if err := rdb.Ping(ctx); err != nil {
		t.Skipf("redis unreachable: %v", err)
	}
	defer rdb.Close()

	mark := time.Now().UnixNano()
	buyer := chaosSeedAccount(t, pool, mark, "buyer", "1000000000000", "1000000000")
	seller := chaosSeedAccount(t, pool, mark, "seller", "1000000000000", "1000000000")

	store := &chaosCountingStore{inner: pgxBalanceStore{pool: pool}}
	disp := &chaosCountingDispatch{}
	pub := &benchSink{}
	ls, err := NewLedgerService(pool, rdb, pub)
	if err != nil {
		t.Fatalf("ledger: %v", err)
	}
	store.inner.poster = ls
	svc := newBalanceServiceForTest(store, chaosNoopLocker{}, disp, ls, &fakeAlerter{})

	// --- Warmup: 40 sequentially-committed fills; their trade ids are
	//     later replayed inside the storm to prove dedup under contention.
	const warmup = 40
	warmIDs := make([]uint64, warmup)
	base := uint64(mark)
	for i := 0; i < warmup; i++ {
		warmIDs[i] = base + uint64(i) + 1
	}
	for _, tid := range warmIDs {
		out, err := svc.ProcessFills(ctx, []ResolvedTrade{chaosRMFill(tid, buyer, seller)})
		if err != nil || len(out) != 1 || !out[0].Applied {
			t.Fatalf("warmup fill %d: out=%+v err=%v", tid, out, err)
		}
	}

	// --- Storm: 1000 concurrent single-fill mutations — 800 fresh unique
	//     trade ids + 200 replays of warmup ids. Same account pair for all
	//     → maximal contention on 4 balances rows.
	const (
		workers = 1000
		freshN  = 800
		replayN = workers - freshN
	)
	freshBase := base + 1_000_000 // disjoint from warmup ids

	type result struct {
		applied, duplicate bool
		code               string // coded error's Code, "" on success
		err                error
	}
	results := make([]result, workers)
	var wg sync.WaitGroup
	start := make(chan struct{})
	runCtx, cancel := context.WithTimeout(ctx, 4*time.Minute)
	defer cancel()
	startTs := time.Now()
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			tid := freshBase + uint64(i)
			if i >= freshN {
				tid = warmIDs[(i-freshN)%warmup] // replay a committed id
			}
			out, err := svc.ProcessFills(runCtx, []ResolvedTrade{chaosRMFill(tid, buyer, seller)})
			var r result
			if err != nil {
				r.err = err
				var e *excerrors.Error
				if stderrors.As(err, &e) {
					r.code = e.Code
				}
			} else if len(out) == 1 {
				r.applied = out[0].Applied
				r.duplicate = out[0].Duplicate
			}
			results[i] = r
		}(i)
	}
	close(start)
	wg.Wait()
	stormElapsed := time.Since(startTs)

	// --- Tally outcomes; strict error taxonomy: the ONLY legal failure
	//     mode is TRANSACTION_CONFLICT_RETRY_EXHAUSTED (a clean abort).
	//     A replayed id must NEVER report Applied.
	var appliedOut, dupOut, exhausted, otherErr int
	errCodes := map[string]int{}
	for _, r := range results {
		switch {
		case r.err != nil:
			if r.code == ledger.CodeTxnConflictExhausted {
				exhausted++
			} else {
				otherErr++
				errCodes[fmt.Sprintf("%q (%v)", r.code, r.err)]++
			}
		case r.duplicate:
			dupOut++
		case r.applied:
			appliedOut++
		default:
			otherErr++
			errCodes["empty-outcome-no-error"]++
		}
	}
	t.Logf("storm: %d workers in %s — applied=%d duplicate=%d exhausted=%d other=%d",
		workers, stormElapsed, appliedOut, dupOut, exhausted, otherErr)
	t.Logf("store: commits=%d serialization_40001=%d deadlock_40P01=%d non_conflict_abort=%d",
		store.commits.Load(), store.serialization.Load(), store.deadlocks.Load(), store.nonConflictErr.Load())
	for k, v := range errCodes {
		t.Logf("unexpected error class: %s ×%d", k, v)
	}

	if otherErr != 0 {
		t.Fatalf("%d mutations failed with a non-conflict error (only clean retry/abort allowed)", otherErr)
	}
	if appliedOut+dupOut+exhausted != workers {
		t.Fatalf("outcome accounting incoherent: %d+%d+%d != %d", appliedOut, dupOut, exhausted, workers)
	}
	if conflicts := store.serialization.Load() + store.deadlocks.Load(); conflicts == 0 {
		t.Fatal("chaos did not induce a single SQLSTATE 40001/40P01 — scenario vacuous")
	}
	if store.commits.Load() != int64(appliedOut+dupOut)+warmup {
		t.Fatalf("commit count %d != successful results %d (+warmup %d)",
			store.commits.Load(), appliedOut+dupOut, warmup)
	}

	// --- Authoritative committed-count from the DB, not the callers.
	allIDs := make([]int64, 0, warmup+freshN)
	allKeys := make([]string, 0, warmup+freshN)
	for _, id := range warmIDs {
		allIDs = append(allIDs, int64(id))
		allKeys = append(allKeys, fmt.Sprintf("trade-fill:%d", id))
	}
	for i := 0; i < freshN; i++ {
		id := freshBase + uint64(i)
		allIDs = append(allIDs, int64(id))
		allKeys = append(allKeys, fmt.Sprintf("trade-fill:%d", id))
	}
	var committedDB, journalsDB, entriesDB int64
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM processed_trades WHERE trade_id = ANY($1)`, allIDs).
		Scan(&committedDB); err != nil {
		t.Fatalf("count processed: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM journal_entries WHERE idempotency_key = ANY($1)`, allKeys).
		Scan(&journalsDB); err != nil {
		t.Fatalf("count journals: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM ledger_entries le
		  JOIN journal_entries je ON je.id = le.journal_entry_id
		 WHERE je.idempotency_key = ANY($1)`, allKeys).Scan(&entriesDB); err != nil {
		t.Fatalf("count ledger entries: %v", err)
	}

	// Dedup accounting: committed = warmup + fresh-applied. A committed
	// fill that the caller never saw as Applied would be silent
	// loss/phantom — the count must reconcile exactly.
	if committedDB != int64(warmup+appliedOut) {
		t.Fatalf("LOSS/PHANTOM: processed_trades=%d but warmup+applied=%d",
			committedDB, warmup+appliedOut)
	}
	if journalsDB != committedDB {
		t.Fatalf("phantom journal: committed=%d journal_entries=%d", committedDB, journalsDB)
	}
	if entriesDB != 4*committedDB {
		t.Fatalf("phantom ledger entries: committed=%d → want %d entries, got %d",
			committedDB, 4*committedDB, entriesDB)
	}
	if disp.n.Load() != 4*committedDB {
		t.Fatalf("dispatch loss: %d events for %d committed fills (want %d)",
			disp.n.Load(), committedDB, 4*committedDB)
	}

	// --- Zero-phantom balance proof: per-cell total ==
	//     seed + delta·committed AND == journal_sums.net_balance.
	//     fill deltas: buyer EUR avail +99.5; buyer USD locked −110;
	//     seller EUR locked −100; seller USD avail +107.5.
	type cell struct {
		acct   int64
		ccy    string
		avail  string // expected available
		locked string // expected locked
	}
	n := decimal.MustFromString(fmt.Sprint(committedDB))
	cells := []cell{
		{buyer, "EUR", dec("1000000000000").Add(dec("99.5").Mul(n)).String(), "1000000000"},
		{buyer, "USD", "1000000000000", dec("1000000000").Sub(dec("110").Mul(n)).String()},
		{seller, "EUR", "1000000000000", dec("1000000000").Sub(dec("100").Mul(n)).String()},
		{seller, "USD", dec("1000000000000").Add(dec("107.5").Mul(n)).String(), "1000000000"},
	}
	for _, c := range cells {
		var avail, locked, total, net decimal.Decimal
		if err := pool.QueryRow(ctx,
			`SELECT available, locked, total FROM balances
			 WHERE account_id=$1 AND currency=$2`, c.acct, c.ccy).
			Scan(&avail, &locked, &total); err != nil {
			t.Fatalf("read balance acct=%d %s: %v", c.acct, c.ccy, err)
		}
		if err := pool.QueryRow(ctx,
			`SELECT net_balance FROM journal_sums
			 WHERE account_id=$1 AND currency=$2`, c.acct, c.ccy).
			Scan(&net); err != nil {
			t.Fatalf("read journal_sums acct=%d %s: %v", c.acct, c.ccy, err)
		}
		wantAvail, wantLocked := dec(c.avail), dec(c.locked)
		wantTotal := wantAvail.Add(wantLocked)
		if !avail.Equal(wantAvail) || !locked.Equal(wantLocked) || !total.Equal(wantTotal) {
			t.Fatalf("PHANTOM BALANCE acct=%d %s: got avail=%s locked=%s total=%s; want %s/%s/%s (committed=%d)",
				c.acct, c.ccy, avail, locked, total, wantAvail, wantLocked, wantTotal, committedDB)
		}
		if !net.Equal(total) {
			t.Fatalf("journal_sums.net_balance=%s != balances.total=%s acct=%d %s",
				net, total, c.acct, c.ccy)
		}
		if avail.IsNegative() || locked.IsNegative() {
			t.Fatalf("negative balance component acct=%d %s: %s/%s", c.acct, c.ccy, avail, locked)
		}
	}
	var extraRows int64
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM balances WHERE account_id IN ($1,$2)`, buyer, seller).
		Scan(&extraRows); err != nil {
		t.Fatalf("count balance rows: %v", err)
	}
	if extraRows != 4 {
		t.Fatalf("phantom balance rows: want 4, got %d", extraRows)
	}
	t.Logf("zero-loss proof: committed=%d fills; all 4 cells satisfy balances.total == journal_sums.net_balance == seed+delta·%d",
		committedDB, committedDB)
}
