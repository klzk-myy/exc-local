// Live ClickHouse test for Task 21.3.19 RTS 27 daily materialization.
// Gated on EXC_CH_TEST=1 AND EXC_PG_TEST=1 — the rollup reads
// exchange_analytics.trades (native :9000) and persists
// rts27_daily_stats in the PG scratch schema. Trades are written with a
// unique symbol on a quiet historical day so reruns never collide and
// real dev traffic cannot be confused with fixture rows.
package compliance

import (
	"context"
	"fmt"
	"math"
	"math/rand"
	"os"
	"testing"
	"time"

	"exchange/internal/analytics"
)

func chTestConn(t *testing.T) analytics.Conn {
	t.Helper()
	if os.Getenv("EXC_CH_TEST") != "1" {
		t.Skip("EXC_CH_TEST=1 not set — skipping live ClickHouse test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := analytics.Dial(ctx, analytics.ConfigFromEnv())
	if err != nil {
		t.Skipf("clickhouse unreachable — skipping: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// Three fixture fills on one instrument: VWAP, price min/max/median and
// the aggressor split are all derivable by hand — the materialization
// must reproduce them exactly. Order counters + TCA stay absent → the
// row records NULLs and inputs_complete=false (honest gaps).
func TestCHRTS27MaterializeDay(t *testing.T) {
	conn := chTestConn(t)
	ctx, pool := rtsPool(t)
	rtsSchema(t, ctx, pool)
	svc, err := NewRTS27Service(pool, conn, rtsOfficer)
	if err != nil {
		t.Fatal(err)
	}

	sym := fmt.Sprintf("TST%09d", time.Now().UnixNano()%1e9)
	inst := int64(4_000_000_000 + rand.Intn(1_000_000_000))
	if _, err := pool.Exec(ctx, `
		INSERT INTO instruments (id, symbol, instrument_type,
		                         base_currency, quote_currency)
		VALUES ($1,$2,'SPOT','EUR','USD')`, inst, sym); err != nil {
		t.Fatalf("fixture instrument: %v", err)
	}
	day := time.Date(2024, 6, 3, 0, 0, 0, 0, time.UTC) // quiet Monday
	mk := func(seq int, price, qty float64, side string) {
		ts := day.Add(time.Duration(9+seq) * time.Hour)
		err := conn.Exec(ctx, `
			INSERT INTO exchange_analytics.trades
			    (ts, symbol, trade_id, instrument_id,
			     maker_account_id, taker_account_id,
			     buy_order_id, sell_order_id, price, qty,
			     aggressor_side, event_seq, shard_id, ver)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			ts, sym, uint64(seq+1), inst, int64(1), int64(2),
			uint64(0), uint64(0), price, qty, side,
			uint64(seq+1), uint32(0), uint64(1))
		if err != nil {
			t.Fatalf("insert trade %d: %v", seq, err)
		}
	}
	mk(0, 1.20, 1000, "BUY")
	mk(1, 1.24, 2000, "BUY")
	mk(2, 1.28, 2000, "SELL")

	n, err := svc.MaterializeDay(ctx, day)
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	if n < 1 {
		t.Fatalf("materialized %d rows, want >=1", n)
	}
	var fills int64
	var volB, volQ float64
	var vwap, pmin, pmax, pmed *float64
	var aggB, aggS, unk int64
	var sub *int64
	var complete bool
	var gaps []byte
	err = pool.QueryRow(ctx, `
		SELECT fills, volume_base, volume_quote, vwap, price_min,
		       price_max, price_median, agg_buy_fills, agg_sell_fills,
		       unknown_fills, orders_submitted, inputs_complete, gaps
		  FROM rts27_daily_stats
		 WHERE instrument_id=$1 AND day='2024-06-03'`, inst).Scan(
		&fills, &volB, &volQ, &vwap, &pmin, &pmax, &pmed,
		&aggB, &aggS, &unk, &sub, &complete, &gaps)
	if err != nil {
		t.Fatalf("read daily stats: %v", err)
	}
	if fills != 3 || volB != 5000 {
		t.Fatalf("fills=%d vol=%v, want 3/5000", fills, volB)
	}
	wantVWAP := (1.20*1000 + 1.24*2000 + 1.28*2000) / 5000
	if vwap == nil || math.Abs(*vwap-wantVWAP) > 1e-6 {
		t.Fatalf("vwap=%v want %v", vwap, wantVWAP)
	}
	if pmin == nil || *pmin != 1.20 || pmax == nil || *pmax != 1.28 {
		t.Fatalf("price min/max: %v/%v", pmin, pmax)
	}
	if aggB != 2 || aggS != 1 || unk != 0 {
		t.Fatalf("aggressor split %d/%d/%d, want 2/1/0", aggB, aggS, unk)
	}
	if sub != nil {
		t.Fatal("no volume_stats rows → orders_submitted must stay NULL")
	}
	if complete {
		t.Fatalf("spread gap always records — inputs_complete must be false; gaps=%s", gaps)
	}
}
