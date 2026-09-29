// Phase-17 Task 17.3.3 — surveillance signal engine tests.
// Detector coverage is in-memory (SinkFunc collector); the PostgreSQL
// idempotency contract (dedup_key + ON CONFLICT DO NOTHING, migration
// 029) is exercised by the EXC_PG_TEST-gated integration test below.
package surveillance

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"exchange/internal/marketdata"
	"exchange/pkg/decimal"
)

// collectSink captures emissions for assertions.
type collectSink struct {
	mu   sync.Mutex
	sigs []Signal
}

func (c *collectSink) Emit(_ context.Context, s Signal) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sigs = append(c.sigs, s)
	return nil
}

func (c *collectSink) types() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, 0, len(c.sigs))
	for _, s := range c.sigs {
		out = append(out, s.Type)
	}
	return out
}

func newEngine(t *testing.T, cfg Config) (*Engine, *collectSink) {
	t.Helper()
	sink := &collectSink{}
	e, err := NewEngine(cfg, sink)
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	return e, sink
}

func ev(symbol string, kind marketdata.L3Kind, orderID, acct uint64,
	side marketdata.Side, price string, seq uint64, ts time.Time) marketdata.L3Event {
	return marketdata.L3Event{
		Symbol: symbol, OrderID: orderID, AccountHash: acct, Kind: kind,
		Side: side, Price: decimal.RequireFromString(price),
		Quantity: decimal.NewFromInt(1), Ts: ts, Seq: seq, WalSeq: seq * 10,
	}
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

func TestEngine_Spoofing(t *testing.T) {
	e, sink := newEngine(t, Config{})
	base := time.Unix(1_800_000_000, 0).UTC()
	ctx := context.Background()
	var seq uint64
	for i := 0; i < 3; i++ {
		id := uint64(100 + i)
		seq++
		e.Apply(ctx, ev("EUR/USD", marketdata.L3Add, id, 7,
			marketdata.SideSell, "1.2000", seq, base))
		seq++
		// Cancelled 100ms later — inside the 800ms bona-fide window.
		e.Apply(ctx, ev("EUR/USD", marketdata.L3Cancel, id, 7,
			marketdata.SideSell, "1.2000", seq, base.Add(100*time.Millisecond)))
	}
	if !contains(sink.types(), SignalSpoofing) {
		t.Fatalf("want SPOOFING, got %v", sink.types())
	}
	if sink.sigs[0].DedupKey == "" || sink.sigs[0].AccountHash != 7 {
		t.Fatalf("signal = %+v", sink.sigs[0])
	}
}

func TestEngine_Spoofing_RestedOrdersNotFlagged(t *testing.T) {
	e, sink := newEngine(t, Config{})
	base := time.Unix(1_800_000_000, 0).UTC()
	ctx := context.Background()
	var seq uint64
	for i := 0; i < 3; i++ {
		id := uint64(100 + i)
		seq++
		e.Apply(ctx, ev("EUR/USD", marketdata.L3Add, id, 7,
			marketdata.SideSell, "1.2000", seq, base))
		seq++
		// Rested 2s — bona fide, outside the cancel window.
		e.Apply(ctx, ev("EUR/USD", marketdata.L3Cancel, id, 7,
			marketdata.SideSell, "1.2000", seq, base.Add(2*time.Second)))
	}
	if contains(sink.types(), SignalSpoofing) {
		t.Fatalf("bona-fide cancels flagged: %v", sink.types())
	}
}

func TestEngine_Layering(t *testing.T) {
	e, sink := newEngine(t, Config{})
	base := time.Unix(1_800_000_000, 0).UTC()
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		e.Apply(ctx, ev("EUR/USD", marketdata.L3Add, uint64(200+i), 9,
			marketdata.SideBuy, []string{"1.1000", "1.1005", "1.1010"}[i],
			uint64(i+1), base.Add(time.Duration(i)*time.Second)))
	}
	if !contains(sink.types(), SignalLayering) {
		t.Fatalf("want LAYERING, got %v", sink.types())
	}
}

func TestEngine_WashTrading(t *testing.T) {
	e, sink := newEngine(t, Config{})
	base := time.Unix(1_800_000_000, 0).UTC()
	ctx := context.Background()
	// Maker BUY 300 and taker SELL 301 — same account_hash on both
	// legs, adjacent l3_seqs.
	e.Apply(ctx, ev("EUR/USD", marketdata.L3Execute, 300, 11,
		marketdata.SideBuy, "1.2000", 1, base))
	e.Apply(ctx, ev("EUR/USD", marketdata.L3Execute, 301, 11,
		marketdata.SideSell, "1.2000", 2, base))
	if !contains(sink.types(), SignalWashTrading) {
		t.Fatalf("want WASH_TRADING, got %v", sink.types())
	}
	sig := sink.sigs[0]
	if sig.OrderID != 300 || sig.CounterOrder != 301 {
		t.Fatalf("wash legs = %+v", sig)
	}
}

func TestEngine_MomentumIgnition(t *testing.T) {
	e, sink := newEngine(t, Config{
		IgnitionMoveBps: 20, IgnitionWindowMs: 2000,
	})
	base := time.Unix(1_800_000_000, 0).UTC()
	ctx := context.Background()
	// Same account, same direction: 1.2000 → 1.2100 (~83 bps).
	e.Apply(ctx, ev("EUR/USD", marketdata.L3Execute, 400, 13,
		marketdata.SideBuy, "1.2000", 1, base))
	e.Apply(ctx, ev("EUR/USD", marketdata.L3Execute, 401, 13,
		marketdata.SideBuy, "1.2100", 2, base.Add(200*time.Millisecond)))
	if !contains(sink.types(), SignalMomentumIgnition) {
		t.Fatalf("want MOMENTUM_IGNITION, got %v", sink.types())
	}
}

func TestEngine_FrontRunning(t *testing.T) {
	e, sink := newEngine(t, Config{
		FrontRunMoveBps: 10, FrontRunWindowMs: 1000,
	})
	base := time.Unix(1_800_000_000, 0).UTC()
	ctx := context.Background()
	// Account BUYs at 1.2000; the next same-account BUY prints at
	// 1.1900 (~83bps, adverse) — positioned ahead of the move.
	e.Apply(ctx, ev("EUR/USD", marketdata.L3Execute, 500, 15,
		marketdata.SideBuy, "1.2000", 1, base))
	e.Apply(ctx, ev("EUR/USD", marketdata.L3Execute, 501, 15,
		marketdata.SideBuy, "1.1900", 2, base.Add(300*time.Millisecond)))
	if !contains(sink.types(), SignalFrontRunning) {
		t.Fatalf("want FRONT_RUNNING, got %v", sink.types())
	}
}

func TestEngine_MarkingTheClose(t *testing.T) {
	e, sink := newEngine(t, Config{
		CloseUTCStart: "21:45", CloseUTCEnd: "22:05", CloseMoveBps: 10,
	})
	ctx := context.Background()
	// Close-window baseline then a print >10bps away in the window.
	open := time.Date(2026, 1, 5, 21, 50, 0, 0, time.UTC)
	e.Apply(ctx, ev("EUR/USD", marketdata.L3Execute, 600, 17,
		marketdata.SideBuy, "1.2000", 1, open))
	e.Apply(ctx, ev("EUR/USD", marketdata.L3Execute, 601, 17,
		marketdata.SideBuy, "1.2050", 2, open.Add(2*time.Minute)))
	if !contains(sink.types(), SignalMarkingTheClose) {
		t.Fatalf("want MARKING_THE_CLOSE, got %v", sink.types())
	}
	// Same move OUTSIDE the window must not flag.
	e2, sink2 := newEngine(t, Config{
		CloseUTCStart: "21:45", CloseUTCEnd: "22:05", CloseMoveBps: 10,
	})
	out := time.Date(2026, 1, 5, 12, 0, 0, 0, time.UTC)
	e2.Apply(ctx, ev("EUR/USD", marketdata.L3Execute, 700, 17,
		marketdata.SideBuy, "1.2000", 1, out))
	e2.Apply(ctx, ev("EUR/USD", marketdata.L3Execute, 701, 17,
		marketdata.SideBuy, "1.2050", 2, out.Add(2*time.Minute)))
	if contains(sink2.types(), SignalMarkingTheClose) {
		t.Fatalf("out-of-window flagged: %v", sink2.types())
	}
}

func TestEngine_InsiderDealing(t *testing.T) {
	e, sink := newEngine(t, Config{AnnounceWindows: []string{"12:25-12:35"}})
	ctx := context.Background()
	in := time.Date(2026, 1, 5, 12, 30, 0, 0, time.UTC)
	e.Apply(ctx, ev("EUR/USD", marketdata.L3Execute, 800, 19,
		marketdata.SideBuy, "1.2000", 1, in))
	if !contains(sink.types(), SignalInsiderDealing) {
		t.Fatalf("want INSIDER_DEALING, got %v", sink.types())
	}
	out := in.Add(30 * time.Minute)
	e.Apply(ctx, ev("EUR/USD", marketdata.L3Execute, 801, 19,
		marketdata.SideBuy, "1.2000", 2, out))
	if n := len(sink.types()); n != 1 {
		t.Fatalf("outside-window execute emitted extra signals: %v", sink.types())
	}
}

func TestEngine_DedupKeyDeterminism(t *testing.T) {
	// Replaying the identical l3_seq window (stream redelivery,
	// service restart) must regenerate the identical dedup_key — the
	// PG unique index makes double-emission a no-op.
	build := func() *collectSink {
		e, sink := newEngine(t, Config{})
		base := time.Unix(1_800_000_000, 0).UTC()
		ctx := context.Background()
		e.Apply(ctx, ev("EUR/USD", marketdata.L3Execute, 900, 21,
			marketdata.SideBuy, "1.2000", 41, base))
		e.Apply(ctx, ev("EUR/USD", marketdata.L3Execute, 901, 21,
			marketdata.SideSell, "1.2000", 42, base))
		return sink
	}
	k1 := build().sigs[0].DedupKey
	k2 := build().sigs[0].DedupKey
	if k1 == "" || k1 != k2 {
		t.Fatalf("dedup keys not deterministic: %q vs %q", k1, k2)
	}
	// Different seq window → different key.
	e, sink3 := newEngine(t, Config{})
	ctx := context.Background()
	base := time.Unix(1_800_000_000, 0).UTC()
	e.Apply(ctx, ev("EUR/USD", marketdata.L3Execute, 900, 21,
		marketdata.SideBuy, "1.2000", 99, base))
	e.Apply(ctx, ev("EUR/USD", marketdata.L3Execute, 901, 21,
		marketdata.SideSell, "1.2000", 100, base))
	if sink3.sigs[0].DedupKey == k1 {
		t.Fatal("distinct seq windows must not collide")
	}
}

func TestNewEngine_RejectsNilSinkAndBadWindows(t *testing.T) {
	if _, err := NewEngine(Config{}, nil); err == nil {
		t.Fatal("nil sink must fail closed")
	}
	if _, err := NewEngine(Config{CloseUTCStart: "bogus"}, SinkFunc(func(context.Context, Signal) error { return nil })); err == nil {
		t.Fatal("bad close window must fail")
	}
	if _, err := NewEngine(Config{AnnounceWindows: []string{"bogus"}}, SinkFunc(func(context.Context, Signal) error { return nil })); err == nil {
		t.Fatal("bad announce window must fail")
	}
}

// ---------------------------------------------------------------------------
// Gated integration — migration 029 + real PostgreSQL idempotency.
//   EXC_PG_TEST=1 go test ./internal/surveillance/ -run Integration -v
// ---------------------------------------------------------------------------

func testPool029(t *testing.T) *pgxpool.Pool {
	t.Helper()
	if os.Getenv("EXC_PG_TEST") != "1" {
		t.Skip("set EXC_PG_TEST=1 to run PostgreSQL integration tests")
	}
	dsn := os.Getenv("EXC_PG_DSN")
	if dsn == "" {
		dsn = "postgres://exchange:exchange_dev@localhost:5433/exchange?sslmode=disable"
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

func migrationSQL(t *testing.T, name string) string {
	t.Helper()
	_, file, _, _ := runtime.Caller(0)
	p := filepath.Join(filepath.Dir(file), "..", "db", "migrations", name)
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read migration %s: %v", name, err)
	}
	return string(b)
}

func TestSurveillanceSignalsPgIntegration(t *testing.T) {
	pool := testPool029(t)
	ctx := context.Background()

	// Apply migration 029 (idempotent enough for a scratch DB — the
	// table may already exist when the full migrate set ran).
	if _, err := pool.Exec(ctx, migrationSQL(t, "029_create_surveillance_signals.up.sql")); err != nil &&
		!strings.Contains(err.Error(), "already exists") {
		t.Fatalf("migration 029: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DELETE FROM surveillance_signals")
	})
	if _, err := pool.Exec(ctx, "DELETE FROM surveillance_signals"); err != nil {
		t.Fatalf("clean table: %v", err)
	}

	sink := NewPgSink(pool)
	now := time.Unix(1_800_000_000, 0).UTC()
	sig := Signal{
		Type: SignalWashTrading, Symbol: "EUR/USD", AccountHash: 4242,
		OrderID: 10, CounterOrder: 11,
		FirstL3Seq: 7, LastL3Seq: 8, FirstWalSeq: 70, LastWalSeq: 80,
		WindowStart: now, WindowEnd: now.Add(time.Millisecond),
		Evidence: map[string]any{"price": "1.2000", "qty": "1"},
	}
	sig.DedupKey = sig.key("wash")

	// Emit twice — ON CONFLICT DO NOTHING must keep exactly one row.
	if err := sink.Emit(ctx, sig); err != nil {
		t.Fatalf("first emit: %v", err)
	}
	if err := sink.Emit(ctx, sig); err != nil {
		t.Fatalf("second emit: %v", err)
	}
	var n int
	if err := pool.QueryRow(ctx,
		"SELECT count(*) FROM surveillance_signals WHERE dedup_key = $1",
		sig.DedupKey).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Fatalf("dedup rows = %d, want 1", n)
	}

	// CHECK constraint: unknown signal_type must be rejected.
	bad := sig
	bad.Type = "NOT_A_TYPE"
	bad.DedupKey = sig.key("bad") + "x"
	if err := sink.Emit(ctx, bad); err == nil {
		t.Fatal("invalid signal_type must violate the CHECK constraint")
	}

	// seq-window CHECK: last < first must be rejected.
	bad2 := sig
	bad2.FirstL3Seq, bad2.LastL3Seq = 9, 3
	bad2.DedupKey = sig.key("bad2") + "y"
	if err := sink.Emit(ctx, bad2); err == nil {
		t.Fatal("reversed seq window must violate the CHECK constraint")
	}
}
