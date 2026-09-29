// Phase-14 Task 14.3.2 — auto-halt on anomaly unit tests.
//
// Coverage map (task DoD):
//   - anomaly detection trips: price (delegated), volume (delegated),
//     latency spike, error-rate spike — TestAutoHalt*Trip
//   - instrument suspended 5min — hold asserted via the INSTRUMENT
//     breaker (canonical 5min scope hold)
//   - P1 alert + user notification — spy seams on every fresh halt
//   - auto-resume if cleared — probe cycle closes the breaker → RESUMED
//     audit row; still-breaching telemetry during HALF_OPEN re-trips
//   - fail closed — store errors surface, manual trips are never
//     attributed to anomaly detection
package risk

import (
	"context"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"exchange/internal/config"
	"exchange/internal/observability"
	excredis "exchange/internal/redis"
	"exchange/pkg/decimal"
)

// ---------------------------------------------------------------------------
// Test doubles
// ---------------------------------------------------------------------------

type memAutoHaltEvents struct {
	mu   sync.Mutex
	evs  []AutoHaltEvent
	fail bool
}

func (m *memAutoHaltEvents) InsertAutoHaltEvent(_ context.Context, ev AutoHaltEvent) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fail {
		return fmt.Errorf("pg down")
	}
	m.evs = append(m.evs, ev)
	return nil
}

func (m *memAutoHaltEvents) list() []AutoHaltEvent {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]AutoHaltEvent(nil), m.evs...)
}

func (m *memAutoHaltEvents) actions() []string {
	var out []string
	for _, ev := range m.list() {
		out = append(out, ev.Action)
	}
	return out
}

type notifySpy struct {
	mu    sync.Mutex
	calls []struct {
		user  int64
		event string
	}
}

func (n *notifySpy) Notify(_ context.Context, userID int64, event string, _ map[string]any) (int, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.calls = append(n.calls, struct {
		user  int64
		event string
	}{userID, event})
	return 2, nil // two legs queued (email + ws)
}

func (n *notifySpy) count() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return len(n.calls)
}

type autoHaltRig struct {
	cb      *CircuitBreakerService
	svc     *AutoHaltService
	store   *memStore
	events  *memAutoHaltEvents
	clock   *fakeClock
	alerts  *alertSpy
	notices *notifySpy
	reg     *observability.Registry
}

func newAutoHaltRig(t *testing.T) *autoHaltRig {
	t.Helper()
	r := &autoHaltRig{
		store:   newMemStore(),
		events:  &memAutoHaltEvents{},
		clock:   newFakeClock(),
		alerts:  &alertSpy{},
		notices: &notifySpy{},
		reg:     observability.New(),
	}
	cb, err := NewCircuitBreakerService(BreakerDeps{
		Store:   r.store,
		Events:  &memEvents{},
		Metrics: NewBreakerMetrics(r.reg),
		Now:     r.clock.Now,
	})
	if err != nil {
		t.Fatalf("breaker service: %v", err)
	}
	r.cb = cb
	svc, err := NewAutoHaltService(AutoHaltDeps{
		CB:       cb,
		Events:   r.events,
		Metrics:  NewAutoHaltMetrics(r.reg),
		Alerter:  r.alerts.fn(),
		Notifier: r.notices,
		Users: func(_ context.Context, symbol string) ([]int64, error) {
			return []int64{42, 43}, nil
		},
		Now: r.clock.Now,
	})
	if err != nil {
		t.Fatalf("auto-halt service: %v", err)
	}
	r.svc = svc
	return r
}

// feedLat submits n admission-latency samples of d ms apart by 1s.
func feedLat(r *autoHaltRig, ctx context.Context, sym string, n int, d time.Duration) {
	for i := 0; i < n; i++ {
		r.svc.ObserveLatency(ctx, sym, d)
		r.clock.Add(time.Second)
	}
}

// feedAdm submits n admissions with the given systemic-error flag.
func feedAdm(r *autoHaltRig, ctx context.Context, sym string, n int, systemic bool) {
	for i := 0; i < n; i++ {
		r.svc.ObserveAdmission(ctx, sym, 5*time.Millisecond, systemic)
		r.clock.Add(time.Second)
	}
}

func instBreaker(t *testing.T, r *autoHaltRig, sym string) *Breaker {
	t.Helper()
	return breaker(t, r.cb, ScopeInstrument, config.CanonicalSymbol(sym))
}

// ---------------------------------------------------------------------------
// Detector trips
// ---------------------------------------------------------------------------

func TestAutoHaltLatencySpikeTrips(t *testing.T) {
	r := newAutoHaltRig(t)
	ctx := context.Background()

	feedLat(r, ctx, "EURUSD", AutoHaltMinSamples-1, 400*time.Millisecond)
	if b := instBreaker(t, r, "EURUSD"); b != nil && b.State == excredis.CircuitOpen {
		t.Fatal("19 samples must not trip (min samples = 20)")
	}
	r.svc.ObserveLatency(ctx, "EURUSD", 400*time.Millisecond) // 20th sample

	b := instBreaker(t, r, "EURUSD")
	if b == nil || b.State != excredis.CircuitOpen {
		t.Fatalf("expected INSTRUMENT:EUR/USD OPEN, got %+v", b)
	}
	if !b.HoldUntil.Equal(r.clock.Now().Add(5 * time.Minute)) {
		t.Fatalf("5min hold expected, got hold_until=%v", b.HoldUntil)
	}
	requireCode(t, r.cb.AdmitOrder(ctx, 7, "EURUSD"), CodeCircuitBreakerOpen)

	// P1 alert + 2 user notifications + HALTED audit row.
	if r.alerts.count() != 1 {
		t.Fatalf("expected 1 P1 alert, got %d", r.alerts.count())
	}
	if r.notices.count() != 2 {
		t.Fatalf("expected 2 user notifications, got %d", r.notices.count())
	}
	evs := r.events.list()
	if len(evs) != 1 || evs[0].Action != AutoHaltActionHalted ||
		evs[0].Detector != DetectorLatencySpike || !evs[0].AlertSent || evs[0].Notified != 4 {
		t.Fatalf("audit row wrong: %+v", evs)
	}
}

func TestAutoHaltLatencyMeanUnderThresholdNoTrip(t *testing.T) {
	r := newAutoHaltRig(t)
	ctx := context.Background()
	feedLat(r, ctx, "EURUSD", 30, 100*time.Millisecond)
	if b := instBreaker(t, r, "EURUSD"); b != nil && b.State == excredis.CircuitOpen {
		t.Fatal("mean 100ms < 250ms threshold must not trip")
	}
}

func TestAutoHaltErrorRateSpikeTrips(t *testing.T) {
	r := newAutoHaltRig(t)
	ctx := context.Background()

	feedAdm(r, ctx, "GBPUSD", 15, false) // 15 clean
	feedAdm(r, ctx, "GBPUSD", 4, true)   // 4/19 = 21% — under 25%
	if b := instBreaker(t, r, "GBPUSD"); b != nil && b.State == excredis.CircuitOpen {
		t.Fatal("21% < 25% must not trip")
	}
	r.svc.ObserveAdmission(ctx, "GBPUSD", 5*time.Millisecond, true) // 5/20 = 25% → trip
	b := instBreaker(t, r, "GBPUSD")
	if b == nil || b.State != excredis.CircuitOpen {
		t.Fatalf("expected INSTRUMENT:GBP/USD OPEN at 25%% error rate, got %+v", b)
	}
	if b.Trigger["detector"] != DetectorErrorRateSpike {
		t.Fatalf("trigger params missing detector: %v", b.Trigger)
	}
}

func TestAutoHaltClientRejectionsDoNotTrip(t *testing.T) {
	r := newAutoHaltRig(t)
	ctx := context.Background()
	// 40 admissions, all client-correctable rejects (systemic=false) —
	// a fat-finger burst cannot halt an instrument.
	feedAdm(r, ctx, "USDJPY", 40, false)
	if b := instBreaker(t, r, "USDJPY"); b != nil && b.State == excredis.CircuitOpen {
		t.Fatal("client-side rejections must not trip the error-rate detector")
	}
	if len(r.events.list()) != 0 {
		t.Fatalf("no audit rows expected, got %+v", r.events.list())
	}
}

func TestAutoHaltPriceDelegation(t *testing.T) {
	r := newAutoHaltRig(t)
	ctx := context.Background()

	r.svc.ObservePrice(ctx, "EURUSD", d("1.2500"))
	r.clock.Add(10 * time.Second)
	r.svc.ObservePrice(ctx, "EURUSD", d("1.3300")) // peak-to-trough +6.4%

	b := instBreaker(t, r, "EURUSD")
	if b == nil || b.State != excredis.CircuitOpen {
		t.Fatalf("canonical price trip expected, got %+v", b)
	}
	if r.alerts.count() != 1 || r.notices.count() != 2 {
		t.Fatalf("alert+notify expected on price halt: alerts=%d notices=%d",
			r.alerts.count(), r.notices.count())
	}
	evs := r.events.list()
	if len(evs) != 1 || evs[0].Detector != DetectorPriceSpike {
		t.Fatalf("price-spike audit row wrong: %+v", evs)
	}
}

func TestAutoHaltVolumeDelegation(t *testing.T) {
	r := newAutoHaltRig(t)
	ctx := context.Background()
	sym := "GBPUSD"

	cur := r.clock.Now().Unix() / int64(VolumeBucket.Seconds())
	m := map[int64]decimal.Decimal{}
	for i := int64(1); i <= 40; i++ {
		if i%2 == 0 {
			m[cur-i] = decimal.NewFromInt(90)
		} else {
			m[cur-i] = decimal.NewFromInt(110)
		}
	}
	r.cb.mu.Lock()
	r.cb.vols[config.CanonicalSymbol(sym)] = m
	r.cb.mu.Unlock()

	r.svc.ObserveTrade(ctx, sym, decimal.NewFromInt(130)) // cur=130, z=3 → no trip
	r.svc.ObserveTrade(ctx, sym, decimal.NewFromInt(15))  // cur=145 → z=4.5 → trip
	b := breaker(t, r.cb, ScopeVolumeSpike, config.CanonicalSymbol(sym))
	if b == nil || b.State != excredis.CircuitOpen {
		t.Fatalf("expected VOLUME_SPIKE:%s OPEN, got %+v", sym, b)
	}
	evs := r.events.list()
	if len(evs) != 1 || evs[0].Detector != DetectorVolumeSpike ||
		evs[0].Scope != ScopeVolumeSpike {
		t.Fatalf("volume-spike audit row wrong: %+v", evs)
	}
}

// ---------------------------------------------------------------------------
// Auto-resume
// ---------------------------------------------------------------------------

func TestAutoHaltResumeAfterClear(t *testing.T) {
	r := newAutoHaltRig(t)
	ctx := context.Background()

	feedLat(r, ctx, "EURUSD", AutoHaltMinSamples, 400*time.Millisecond)
	if b := instBreaker(t, r, "EURUSD"); b == nil || b.State != excredis.CircuitOpen {
		t.Fatal("latency trip expected")
	}
	// Advance past the 5min hold (latency window is 60s — stale by then,
	// so the anomaly reads cleared). 10 admitted probes close the breaker.
	r.clock.Add(5*time.Minute + time.Second)
	for i := 0; i < ProbeCount; i++ {
		if err := r.cb.AdmitOrder(ctx, 1, "EURUSD"); err != nil {
			t.Fatalf("probe %d: %v", i+1, err)
		}
	}
	b := instBreaker(t, r, "EURUSD")
	if b.State != excredis.CircuitClosed {
		t.Fatalf("10/10 probes must close the breaker, got %s", b.State)
	}
	// Reconcile emits RESUMED once the breaker reports CLOSED.
	r.svc.ObserveLatency(ctx, "EURUSD", 10*time.Millisecond)
	got := r.events.actions()
	want := []string{AutoHaltActionHalted, AutoHaltActionResumed}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("audit actions %v, want %v", got, want)
	}
	if len(r.svc.Halted()) != 0 {
		t.Fatalf("episode map must be empty after resume: %v", r.svc.Halted())
	}
}

func TestAutoHaltPersistsReTripsDuringProbe(t *testing.T) {
	r := newAutoHaltRig(t)
	ctx := context.Background()

	feedAdm(r, ctx, "EURUSD", 10, false)
	feedAdm(r, ctx, "EURUSD", 10, true) // 10/20 = 50% → trip
	if b := instBreaker(t, r, "EURUSD"); b == nil || b.State != excredis.CircuitOpen {
		t.Fatal("error-rate trip expected")
	}
	// Hold expires; first admit opens the probe window.
	r.clock.Add(5*time.Minute + time.Second)
	if err := r.cb.AdmitOrder(ctx, 1, "EURUSD"); err != nil {
		t.Fatalf("probe 1 must admit: %v", err)
	}
	if b := instBreaker(t, r, "EURUSD"); b.State != excredis.CircuitHalfOpen {
		t.Fatalf("expected HALF_OPEN, got %s", b.State)
	}
	// Fresh systemic failures during the probe window → detector still
	// breaches → immediate re-trip, no resume.
	feedAdm(r, ctx, "EURUSD", 20, true)
	b := instBreaker(t, r, "EURUSD")
	if b.State != excredis.CircuitOpen {
		t.Fatalf("still-breaching anomaly must re-trip, got %s", b.State)
	}
	if got := r.events.actions(); len(got) != 1 || got[0] != AutoHaltActionHalted {
		t.Fatalf("re-trip is same episode — audit must hold one HALTED, got %v", got)
	}
}

// ---------------------------------------------------------------------------
// Attribution / fail-closed
// ---------------------------------------------------------------------------

func TestAutoHaltManualTripNotAttributed(t *testing.T) {
	r := newAutoHaltRig(t)
	ctx := context.Background()

	if err := r.cb.ManualTrip(ctx, ScopeInstrument, "EURUSD", "ops halt", 7); err != nil {
		t.Fatal(err)
	}
	// Feeds still reconcile — a manual halt is NOT an anomaly episode:
	// no P1 page, no HALTED row, no user blast.
	r.svc.ObservePrice(ctx, "EURUSD", d("1.2500"))
	if r.alerts.count() != 0 || r.notices.count() != 0 || len(r.events.list()) != 0 {
		t.Fatalf("manual halt must not emit anomaly duties: alerts=%d notices=%d evs=%d",
			r.alerts.count(), r.notices.count(), len(r.events.list()))
	}
	// Manual reset → breaker closes; nothing attributed, no RESUMED row.
	if err := r.cb.ManualReset(ctx, ScopeInstrument, "EURUSD", 9); err != nil {
		t.Fatal(err)
	}
	r.svc.ObservePrice(ctx, "EURUSD", d("1.2500"))
	if len(r.events.list()) != 0 {
		t.Fatalf("no audit rows expected for manual episode, got %+v", r.events.list())
	}
}

func TestAutoHaltStoreErrorFailsClosed(t *testing.T) {
	r := newAutoHaltRig(t)
	ctx := context.Background()
	r.store.getErr = fmt.Errorf("redis down")
	// Reconcile can't verify state — must not emit alerts or audit rows,
	// and must return control without panic (feed errors are logged).
	r.svc.ObserveLatency(ctx, "EURUSD", 400*time.Millisecond)
	if r.alerts.count() != 0 || len(r.events.list()) != 0 {
		t.Fatal("unverifiable breaker state must not fabricate anomaly duties")
	}
}

func TestSystemicAdmissionCode(t *testing.T) {
	systemic := []string{"GATEWAY_TIMEOUT_MATCHING_ENGINE", "INTERNAL_ERROR", "SERVICE_DEGRADED"}
	client := []string{"INVALID_REQUEST", "INSUFFICIENT_BALANCE", "IDEMPOTENCY_KEY_COLLISION",
		"CIRCUIT_BREAKER_OPEN", "TRADING_HALTED", "OTR_LIMIT_EXCEEDED", "ORDER_NOT_FOUND",
		"RATE_LIMIT_TIER_EXCEEDED", ""}
	for _, c := range systemic {
		if !SystemicAdmissionCode(c) {
			t.Fatalf("code %q must be systemic", c)
		}
	}
	for _, c := range client {
		if SystemicAdmissionCode(c) {
			t.Fatalf("code %q must NOT be systemic", c)
		}
	}
}

func TestAutoHaltNilCBFailsConstruction(t *testing.T) {
	if _, err := NewAutoHaltService(AutoHaltDeps{}); err == nil {
		t.Fatal("nil circuit breaker must fail construction")
	}
}

// ---------------------------------------------------------------------------
// Integration — Redis (EXC_REDIS_TEST=1) and Postgres (EXC_PG_TEST=1)
// ---------------------------------------------------------------------------

// TestRedisAutoHaltIntegration proves the anomaly trip persists through
// the real Redis HASH (a restarting gateway still sees the halt). Run:
// EXC_REDIS_TEST=1 go test ./internal/risk/ -run TestRedisAutoHalt -v
func TestRedisAutoHaltIntegration(t *testing.T) {
	if os.Getenv("EXC_REDIS_TEST") != "1" {
		t.Skip("EXC_REDIS_TEST not set")
	}
	addr := os.Getenv("EXC_REDIS_TEST_ADDR")
	if addr == "" {
		addr = "127.0.0.1:16379"
	}
	rdb := excredis.New(addr, os.Getenv("EXC_REDIS_TEST_PASSWORD"), 14)
	ctx := context.Background()
	if err := rdb.Ping(ctx); err != nil {
		t.Fatalf("redis ping: %v", err)
	}
	if err := rdb.FlushDB(ctx).Err(); err != nil {
		t.Fatalf("redis flushdb: %v", err)
	}
	t.Cleanup(func() { _ = rdb.Close() })

	clock := newFakeClock()
	cb, err := NewCircuitBreakerService(BreakerDeps{
		Store: RedisBreakerStore{C: rdb}, Now: clock.Now})
	if err != nil {
		t.Fatal(err)
	}
	ah, err := NewAutoHaltService(AutoHaltDeps{CB: cb, Now: clock.Now})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < AutoHaltMinSamples; i++ {
		ah.ObserveLatency(ctx, "EURUSD", 400*time.Millisecond)
	}
	m, err := rdb.HGetAll(ctx, "circuit_breaker:INSTRUMENT:EUR/USD").Result()
	if err != nil || m["state"] != "OPEN" {
		t.Fatalf("redis hash after latency trip: %v %v", m, err)
	}
	requireCode(t, cb.AdmitOrder(ctx, 1, "EURUSD"), CodeCircuitBreakerOpen)
}

// TestPgAutoHaltEventStoreIntegration round-trips one audit row through
// migration 217's table. Skips cleanly when the scratch DB lacks the
// table. Run: EXC_PG_TEST=1 go test ./internal/risk/ -run TestPgAutoHalt -v
func TestPgAutoHaltEventStoreIntegration(t *testing.T) {
	if os.Getenv("EXC_PG_TEST") != "1" {
		t.Skip("EXC_PG_TEST not set")
	}
	dsn := os.Getenv("EXC_PG_DSN")
	if dsn == "" {
		dsn = "postgres://exchange:exchange_dev@localhost:5433/exchange?sslmode=disable"
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("pg pool: %v", err)
	}
	defer pool.Close()
	ctx := context.Background()
	var exists bool
	if err := pool.QueryRow(ctx,
		`SELECT to_regclass('auto_halt_events') IS NOT NULL`).Scan(&exists); err != nil {
		t.Fatalf("regclass: %v", err)
	}
	if !exists {
		t.Skip("migration 217 not applied to this database")
	}

	st := NewPgAutoHaltEventStore(pool)
	ev := AutoHaltEvent{
		Symbol: "ITEST/PAIR", Scope: ScopeInstrument,
		Detector: DetectorLatencySpike, Action: AutoHaltActionHalted,
		Observed:     map[string]string{"mean_latency": "400ms"},
		BreakerState: "OPEN", AlertSent: true, Notified: 4,
		At: time.Now().UTC(),
	}
	if err := st.InsertAutoHaltEvent(ctx, ev); err != nil {
		t.Fatalf("insert: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM auto_halt_events WHERE symbol='ITEST/PAIR'`)
	})
	evs, err := st.RecentAutoHaltEvents(ctx, 10)
	if err != nil {
		t.Fatalf("recent: %v", err)
	}
	var found *AutoHaltEvent
	for i := range evs {
		if evs[i].Symbol == "ITEST/PAIR" {
			found = &evs[i]
			break
		}
	}
	if found == nil {
		t.Fatal("inserted event not returned by RecentAutoHaltEvents")
	}
	if found.Detector != DetectorLatencySpike || found.Action != AutoHaltActionHalted ||
		found.Observed["mean_latency"] != "400ms" || !found.AlertSent || found.Notified != 4 {
		t.Fatalf("round-trip mismatch: %+v", found)
	}
}
