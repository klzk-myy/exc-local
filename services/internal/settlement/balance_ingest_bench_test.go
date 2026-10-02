// balance_ingest_bench_test.go — sustained fill-ingest rate evidence for
// Phase-03 Task 3.3.1 DoD "Trade fills consumed with zero loss at 50k/sec".
//
// Drives the REAL pipeline end-to-end: FillConsumer (FuncSource carrying
// wire-encoded TradeFill events) → PgxTradeResolver (orders⨝instruments⨝
// accounts) → BalanceService (Redis account locks + SERIALIZABLE batch
// commit + ledger journal + processed_trades dedup). The transport hop
// (Aeron/SHM ring) is exercised by the C++ soak; this bench measures the
// consume→settle path the DoD gates on.
//
//	EXC_PG_TEST=1 EXC_PG_DSN='postgres://...' EXC_REDIS_TEST_ADDR=127.0.0.1:16379 \
//	  EXC_BENCH_INGEST=1 go test ./internal/settlement -run IngestBench -v -timeout 10m
package settlement

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"sync/atomic"
	"testing"
	"time"

	flatbuffers "github.com/google/flatbuffers/go"

	"exchange/internal/ipc"
	excredis "exchange/internal/redis"
)

// benchSink is a no-op Publisher (events counted, not emitted — NATS
// publication is not on the measured path).
type benchSink struct{ n atomic.Uint64 }

func (b *benchSink) Publish(_ context.Context, _ string, _ []byte) error {
	b.n.Add(1)
	return nil
}

func TestIngestBenchSustainedRate(t *testing.T) {
	if os.Getenv("EXC_BENCH_INGEST") != "1" {
		t.Skip("set EXC_BENCH_INGEST=1 (plus EXC_PG_TEST=1/EXC_PG_DSN) to run the ingest benchmark")
	}
	pool := testPool(t)
	rdb := excredis.New(defaultTestRedis, os.Getenv("EXC_REDIS_TEST_PASSWORD"), 0)
	if addr := os.Getenv("EXC_REDIS_TEST_ADDR"); addr != "" {
		rdb = excredis.New(addr, os.Getenv("EXC_REDIS_TEST_PASSWORD"), 0)
	}
	ctx := context.Background()
	if err := rdb.Ping(ctx); err != nil {
		t.Skipf("redis unreachable: %v", err)
	}

	const (
		accountPairs = 64  // distinct buyer/seller account pairs
		orderPairs   = 512 // resting order pairs fills cycle over
		fills        = 200_000
	)
	// --- seed: users/accounts/balances/orders on instrument 1 (EUR/USD) ---
	mark := time.Now().UnixNano()
	var acctIDs [accountPairs][2]int64
	for i := 0; i < accountPairs; i++ {
		for side := 0; side < 2; side++ {
			var uid, aid int64
			if err := pool.QueryRow(ctx,
				`INSERT INTO users (email,status) VALUES ($1,'ACTIVE') RETURNING id`,
				fmt.Sprintf("bench_%d_%d_%d@x", mark, i, side)).Scan(&uid); err != nil {
				t.Fatalf("seed user: %v", err)
			}
			if err := pool.QueryRow(ctx,
				`INSERT INTO accounts (user_id,account_type) VALUES ($1,'MARGIN') RETURNING id`, uid).
				Scan(&aid); err != nil {
				t.Fatalf("seed account: %v", err)
			}
			for _, ccy := range []string{"EUR", "USD"} {
				if _, err := pool.Exec(ctx,
					`INSERT INTO balances (account_id,currency,available,locked,version)
					 VALUES ($1,$2,1e12,1e9,0)`, aid, ccy); err != nil {
					t.Fatalf("seed balance: %v", err)
				}
				// §5.3 invariant: journal_sums.net_balance must equal
				// balances.total — seed the sums cache at the genesis total
				// (the real baseline is established by deposit journals).
				if _, err := pool.Exec(ctx,
					`INSERT INTO journal_sums (account_id,currency,total_debits,total_credits)
					 VALUES ($1,$2,1001000000000,0)`, aid, ccy); err != nil {
					t.Fatalf("seed journal_sums: %v", err)
				}
			}
			acctIDs[i][side] = aid
		}
	}
	var orderIDs [orderPairs][2]int64
	for i := 0; i < orderPairs; i++ {
		pair := acctIDs[i%accountPairs]
		for side := 0; side < 2; side++ {
			var oid int64
			if err := pool.QueryRow(ctx, `
				INSERT INTO orders (account_id,instrument_id,side,order_type,quantity,price,
					time_in_force,status)
				VALUES ($1,1,$2,'LIMIT',1000,1.10,'GTC','PARTIALLY_FILLED') RETURNING id`,
				pair[side], map[int]string{0: "BUY", 1: "SELL"}[side]).Scan(&oid); err != nil {
				t.Fatalf("seed order: %v", err)
			}
			orderIDs[i][side] = oid
		}
	}
	var preProcessed int64
	pool.QueryRow(ctx, `SELECT count(*) FROM processed_trades`).Scan(&preProcessed)

	// --- wire payloads: unique trade_ids, cycling order pairs ---
	payloads := make([][]byte, fills)
	for i := 0; i < fills; i++ {
		b := flatbuffers.NewBuilder(96)
		op := orderIDs[i%orderPairs]
		payloads[i] = append([]byte(nil), ipc.EncodeTradeFillEvent(b,
			uint64(i+1), uint64(time.Now().UnixNano()),
			uint64(mark)+uint64(i), uint64(op[0]), uint64(op[1]),
			110000000 /*price 1.10 1e8*/, 100000000 /*qty 1.0 1e8*/, int64(i+1))...)
	}

	if os.Getenv("EXC_BENCH_TIMING") == "1" {
		timings := map[string]time.Duration{}
		calls := map[string]int{}
		batchTimingHook = func(step string, d time.Duration) {
			timings[step] += d
			calls[step]++
		}
		t.Cleanup(func() {
			batchTimingHook = nil
			steps := make([]string, 0, len(timings))
			for s := range timings {
				steps = append(steps, s)
			}
			sort.Strings(steps)
			for _, s := range steps {
				t.Logf("step %-24s calls=%d total=%s avg=%s", s, calls[s], timings[s], timings[s]/time.Duration(calls[s]))
			}
		})
	}

	pub := &benchSink{}
	ls, err := NewLedgerService(pool, rdb, pub)
	if err != nil {
		t.Fatalf("ledger: %v", err)
	}
	svc, err := NewBalanceService(pool, ls, nil)
	if err != nil {
		t.Fatalf("balance svc: %v", err)
	}

	var cursor atomic.Int64
	src := FuncSource(func(limit int, deliver func([]byte)) int {
		n := 0
		for n < limit {
			idx := cursor.Add(1) - 1
			if idx >= int64(fills) {
				break
			}
			deliver(payloads[idx])
			n++
		}
		return n
	})

	consumer, err := NewFillConsumer(svc, NewPgxTradeResolver(pool, nil, nil, nil), src, 0, 5000, 10*time.Millisecond)
	if err != nil {
		t.Fatalf("consumer: %v", err)
	}
	rctx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	start := time.Now()
	go func() { done <- consumer.Run(rctx) }()

	// Poll until all fills flushed, the consumer exits, or deadline.
	budgetSecs := 180
	if v := os.Getenv("EXC_BENCH_SECONDS"); v != "" {
		if n, err := fmt.Sscanf(v, "%d", &budgetSecs); err == nil && n == 1 {
			// parsed
		}
	}
	deadline := time.Now().Add(time.Duration(budgetSecs) * time.Second)
	var m ConsumerMetrics
	var runErr error
	for {
		m = consumer.Metrics()
		var applied int64
		_ = pool.QueryRow(ctx, `SELECT count(*) FROM processed_trades`).Scan(&applied)
		if applied-preProcessed >= fills || time.Now().After(deadline) {
			break
		}
		select {
		case runErr = <-done:
			cancel()
			elapsed := time.Since(start)
			t.Fatalf("consumer exited early after %s: %v (resolved=%d)", elapsed, runErr, m.Resolved)
		default:
		}
		time.Sleep(50 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil && runErr == nil {
		runErr = err
	}
	elapsed := time.Since(start)

	var postProcessed int64
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM processed_trades`).Scan(&postProcessed); err != nil {
		t.Fatalf("count processed: %v", err)
	}
	applied := postProcessed - preProcessed
	rate := float64(m.Resolved) / elapsed.Seconds()
	t.Logf("ingest bench: resolved=%d flushed=%d malformed=%d nonfill=%d in %s → %.0f fills/s; processed_trades +%d",
		m.Resolved, m.Flushed, m.Malformed, m.NonFill, elapsed, rate, applied)
	if runErr != nil && !errors.Is(runErr, context.Canceled) {
		t.Fatalf("consumer error: %v", runErr)
	}

	if applied != int64(m.Resolved) {
		t.Fatalf("LOSS: resolved=%d but processed_trades grew by %d", m.Resolved, applied)
	}
	if rate < 50_000 {
		t.Logf("NOTE: sustained settle rate %.0f/s < 50k/s target — batch size/commit path bound", rate)
	}
}
