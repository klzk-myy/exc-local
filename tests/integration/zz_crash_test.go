// Crash-recovery e2e — the binary-level WAL leg (criteria 6/7/8/302):
// SIGKILL a live engine shard holding a real resting order, restart it
// on the same WAL dir, and prove the book recovered by cancelling the
// order through a fresh gateway (the old gateway's mmap'd rings are
// stale after engine restart — a real deployment bounces the producer
// too, so this test mirrors reality).
package integration

import (
	"context"
	"fmt"
	"net/http"
	"syscall"
	"testing"
	"time"

	"exchange-integration/itest"
)

func TestE2E_EngineCrashRecovery(t *testing.T) {
	s := getStack(t)
	ctx := context.Background()
	start := time.Now()
	const crit = "e2e:crash-wal"

	record := func(id int, ok bool, detail string) {
		recordLeg(id, crit, start, ok, detail)
	}

	pool, err := env.Pool(ctx)
	if err != nil {
		for _, id := range []int{6, 7, 8, 302} {
			recordBlocked(id, crit, "pg: "+err.Error())
		}
		t.Skip(err)
	}
	defer pool.Close()

	// 1. Rest a GTC BUY near the price-collar floor — deep enough not to
	// cross any seeded ask, inside the §5.9 reference band.
	code, ack, raw := submitOrder(t, s, s.fx.Key,
		orderBody("EUR/USD", "BUY", "1.0310", "1000", "GTC", "itest-crash-"+stk.runTag))
	if code != 202 {
		for _, id := range []int{6, 7, 8, 302} {
			record(id, false, "seed resting order failed: "+trunc(string(raw), 120))
		}
		t.Fatalf("seed order → %d %s", code, trunc(string(raw), 160))
	}
	oid := int64(ack["order_id"].(float64))

	live := itest.WaitFor(3*time.Second, 50*time.Millisecond, func() bool {
		o, e := itest.LoadOrder(ctx, pool, oid)
		return e == nil && (o.Status == "ACTIVE" || o.Status == "FILLED")
	})
	if !live {
		for _, id := range []int{6, 7, 8, 302} {
			record(id, false, "resting order never reached the book")
		}
		t.Fatalf("order %d not live before crash", oid)
	}
	time.Sleep(200 * time.Millisecond) // WAL fsync settle margin

	// 2. SIGKILL shard 0 — no graceful shutdown, WAL tail may be mid-line.
	// Death is proven by reaping the child: signal(0) keeps succeeding
	// on an unreaped zombie, which would make the test wait forever.
	eng := stk.engines[0]
	if err := eng.Signal(syscall.SIGKILL); err != nil {
		t.Fatalf("kill engine: %v", err)
	}
	reaped := make(chan error, 1)
	go func() { reaped <- eng.Cmd.Wait() }()
	select {
	case <-reaped:
	case <-time.After(5 * time.Second):
		t.Fatal("engine did not die on SIGKILL")
	}

	// 3. Restart on the SAME wal dir — WAL-only boot replays.
	re, err := itest.StartEngine(ctx, env, stk.dir, eng.Shard, 1)
	if err != nil {
		for _, id := range []int{6, 7, 8, 302} {
			recordBlocked(id, crit, "engine restart: "+err.Error())
		}
		t.Fatalf("engine restart: %v", err)
	}
	stk.engines[0] = re
	if err := re.WaitReady(ctx, 15*time.Second); err != nil {
		for _, id := range []int{6, 7, 8, 302} {
			record(id, false, "engine restart not ready: "+err.Error())
		}
		t.Fatalf("engine restart not ready: %v", err)
	}
	// Recovery-marker evidence from the engine's own log.
	replayed := itest.LogContains(re.LogPath, "replay") ||
		itest.LogContains(re.LogPath, "ready")

	// 4. Bounce the gateway so its shm producer mmaps the new images.
	stk.gw.Stop()
	port, _ := itest.FreePort()
	gw2, err := itest.StartGateway(ctx, env, stk.gw.Cmd.Path, stk.dir, port,
		stk.jwtKey, itest.B64(stk.dataKey))
	if err != nil {
		for _, id := range []int{6, 7, 8, 302} {
			recordBlocked(id, crit, "gateway restart: "+err.Error())
		}
		t.Fatalf("gateway restart: %v", err)
	}
	stk.gw = gw2
	stk.http = itest.NewClient(gw2.Addr, "")
	if err := gw2.WaitReady(ctx, 20*time.Second); err != nil {
		for _, id := range []int{6, 7, 8, 302} {
			record(id, false, "gateway bounce not ready: "+err.Error())
		}
		t.Fatalf("gateway bounce: %v", err)
	}
	_ = gw2.WaitShardsReady(ctx, 15*time.Second)

	// 5. The proof: if the book recovered, cancelling oid through the
	//    engine succeeds; if WAL replay lost it the cancel fails closed.
	resp, b, err := cli(t).Signed(ctx, s.fx.Key, http.MethodDelete,
		fmt.Sprintf("/api/v1/orders/%d", oid), nil)
	cancelled := itest.WaitFor(5*time.Second, 100*time.Millisecond, func() bool {
		o, e := itest.LoadOrder(ctx, pool, oid)
		return e == nil && o.Status == "CANCELLED"
	})
	ok := err == nil && (resp.StatusCode == 200 || resp.StatusCode == 202) && cancelled
	detail := fmt.Sprintf("kill -9 → restart → cancel oid=%d: http=%d cancelled=%v replayed=%v %s",
		oid, resp.StatusCode, cancelled, replayed, trunc(string(b), 120))
	for _, id := range []int{6, 7, 8, 302} {
		record(id, ok, detail)
	}
	if !ok {
		t.Fatal(detail)
	}
	t.Log(detail)
}
