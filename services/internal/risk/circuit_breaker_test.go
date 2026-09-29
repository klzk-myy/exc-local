// Phase-13 Tasks 13.3.1/13.3.9 — circuit-breaker unit tests.
//
// Coverage map (spec §2.6 + task checkboxes):
//   - scope table / state machine:    TestScopeTable, TestValidBreakerScope
//   - trigger thresholds (5 scopes):  TestInstrumentPriceMoveTrip{,LimitOverride},
//     TestAccountLossRapidTrip, TestVolumeSpikeZScore,
//     TestOptionsVolatilityTrip, TestMarketWideTrip
//   - probe window:                   TestProbeRecoveryCycle, TestProbeWindowExpiryReopens,
//     TestProbeCap
//   - flapping:                       TestFlapDoublesHoldAndAlerts, TestFlapCap,
//     TestFailedProbeIsNotFlap, TestManualTripSkipsFlap
//   - cooldown:                       TestTransitionCooldown
//   - fail-closed:                    TestNilStoreFailsClosed, TestStoreErrorFailsClosed,
//     TestCorruptRecordFailsClosed, TestPersistErrorStillTrips
//   - persistence:                    TestLoadHydrates (unit), Redis/PG integration tests
package risk

import (
	"context"
	"fmt"
	"os"
	"strings"
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

// fakeClock is the injectable time source — tests advance it explicitly.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{t: time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// memStore is the in-memory BreakerStore with error injection.
type memStore struct {
	mu     sync.Mutex
	recs   map[string]*excredis.CircuitBreaker // "SCOPE|id"
	getErr error
	setErr error
}

func newMemStore() *memStore { return &memStore{recs: map[string]*excredis.CircuitBreaker{}} }

func copyCB(cb *excredis.CircuitBreaker) *excredis.CircuitBreaker {
	if cb == nil {
		return nil
	}
	out := &excredis.CircuitBreaker{State: cb.State, Metadata: map[string]string{}}
	for k, v := range cb.Metadata {
		out.Metadata[k] = v
	}
	return out
}

func (m *memStore) GetCircuitBreaker(_ context.Context, scope, id string) (*excredis.CircuitBreaker, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.getErr != nil {
		return nil, m.getErr
	}
	return copyCB(m.recs[scope+"|"+id]), nil
}

func (m *memStore) SetCircuitBreaker(_ context.Context, scope, id string, cb excredis.CircuitBreaker) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.setErr != nil {
		return m.setErr
	}
	m.recs[scope+"|"+id] = copyCB(&cb)
	return nil
}

func (m *memStore) ScanBreakers(context.Context) (map[string]*excredis.CircuitBreaker, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[string]*excredis.CircuitBreaker{}
	for k, v := range m.recs {
		out[k] = copyCB(v)
	}
	return out, nil
}

// memEvents records audit rows (the BreakerEventStore seam).
type memEvents struct {
	mu   sync.Mutex
	evs  []BreakerEvent
	fail bool
}

func (m *memEvents) InsertBreakerEvent(_ context.Context, ev BreakerEvent) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fail {
		return fmt.Errorf("pg down")
	}
	m.evs = append(m.evs, ev)
	return nil
}

func (m *memEvents) list() []BreakerEvent {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]BreakerEvent(nil), m.evs...)
}

// pubSpy captures WS admin-monitor publishes.
type pubSpy struct {
	mu   sync.Mutex
	msgs []BreakerEvent
}

func (p *pubSpy) Publish(channel string, data any) {
	if channel != BreakerAdminChannel {
		return
	}
	if ev, ok := data.(BreakerEvent); ok {
		p.mu.Lock()
		p.msgs = append(p.msgs, ev)
		p.mu.Unlock()
	}
}

func (p *pubSpy) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.msgs)
}

// alertSpy captures flap pages.
type alertSpy struct {
	mu    sync.Mutex
	calls []string
}

func (a *alertSpy) fn() FlapAlerter {
	return func(_ context.Context, sev, code, summary string) error {
		a.mu.Lock()
		a.calls = append(a.calls, sev+"|"+code+"|"+summary)
		a.mu.Unlock()
		return nil
	}
}

func (a *alertSpy) count() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.calls)
}

type testRig struct {
	svc    *CircuitBreakerService
	store  *memStore
	events *memEvents
	clock  *fakeClock
	pub    *pubSpy
	alerts *alertSpy
	reg    *observability.Registry
}

func newRig(t *testing.T) *testRig {
	t.Helper()
	r := &testRig{
		store:  newMemStore(),
		events: &memEvents{},
		clock:  newFakeClock(),
		pub:    &pubSpy{},
		alerts: &alertSpy{},
		reg:    observability.New(),
	}
	svc, err := NewCircuitBreakerService(BreakerDeps{
		Store:     r.store,
		Events:    r.events,
		Metrics:   NewBreakerMetrics(r.reg),
		Publisher: r.pub,
		Alerter:   r.alerts.fn(),
		IV:        NullIVSource{},
		Now:       r.clock.Now,
	})
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	r.svc = svc
	return r
}

func breaker(t *testing.T, s *CircuitBreakerService, scope, id string) *Breaker {
	t.Helper()
	for _, b := range s.Status() {
		if b.Scope == scope && b.ID == id {
			return &b
		}
	}
	return nil
}

func d(s string) decimal.Decimal { return decimal.RequireFromString(s) }

// ---------------------------------------------------------------------------
// Scope table / state machine
// ---------------------------------------------------------------------------

func TestScopeTable(t *testing.T) {
	want := map[string]scopeSpec{
		ScopeInstrument:        {hold: 5 * time.Minute, autoRecover: true},
		ScopeAccount:           {hold: 30 * time.Minute, autoRecover: false},
		ScopeVolumeSpike:       {hold: 10 * time.Minute, autoRecover: true},
		ScopeOptionsVolatility: {hold: 15 * time.Minute, autoRecover: true},
		ScopeMarketWide:        {hold: 0, autoRecover: false},
	}
	if len(scopeSpecs) != 5 {
		t.Fatalf("spec §2.6 requires exactly five scopes, got %d", len(scopeSpecs))
	}
	for scope, spec := range want {
		got, ok := scopeSpecs[scope]
		if !ok {
			t.Fatalf("scope %s missing", scope)
		}
		if got != spec {
			t.Fatalf("scope %s: got %+v want %+v", scope, got, spec)
		}
	}
}

func TestValidBreakerScope(t *testing.T) {
	for _, s := range []string{"INSTRUMENT", "ACCOUNT", "VOLUME_SPIKE",
		"OPTIONS_VOLATILITY", "MARKET_WIDE", "instrument"} {
		if !ValidBreakerScope(s) {
			t.Fatalf("scope %q should be valid", s)
		}
	}
	for _, s := range []string{"", "GLOBAL", "SYMBOL", "MARKET"} {
		if ValidBreakerScope(s) {
			t.Fatalf("scope %q should be invalid", s)
		}
	}
}

// ---------------------------------------------------------------------------
// Trigger thresholds
// ---------------------------------------------------------------------------

func TestInstrumentPriceMoveTrip(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()

	r.svc.ObservePrice(ctx, "EURUSD", d("1.2500"))
	r.clock.Add(10 * time.Second)
	r.svc.ObservePrice(ctx, "EURUSD", d("1.3100")) // +4.8% — under the 5% limit
	if b := breaker(t, r.svc, ScopeInstrument, "EUR/USD"); b != nil && b.State == excredis.CircuitOpen {
		t.Fatal("4.8% move must not trip a 5% instrument limit")
	}

	r.clock.Add(10 * time.Second)
	r.svc.ObservePrice(ctx, "EURUSD", d("1.3300")) // peak-to-trough +6.4%
	b := breaker(t, r.svc, ScopeInstrument, "EUR/USD")
	if b == nil || b.State != excredis.CircuitOpen {
		t.Fatalf("expected INSTRUMENT:EURUSD OPEN, got %+v", b)
	}
	if !b.HoldUntil.Equal(r.clock.Now().Add(5 * time.Minute)) {
		t.Fatalf("hold_until = %v, want now+5min", b.HoldUntil)
	}
	requireCode(t, r.svc.AdmitOrder(ctx, 42, "EURUSD"), CodeCircuitBreakerOpen)
	// Other symbols unaffected.
	if err := r.svc.AdmitOrder(ctx, 42, "USDJPY"); err != nil {
		t.Fatalf("untripped symbol must admit, got %v", err)
	}
}

func TestInstrumentPriceMoveWindowExpiry(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	// Two legs >60s apart never compose a 60s move.
	r.svc.ObservePrice(ctx, "EURUSD", d("1.2500"))
	r.clock.Add(61 * time.Second)
	r.svc.ObservePrice(ctx, "EURUSD", d("1.4000")) // window holds only this point
	if b := breaker(t, r.svc, ScopeInstrument, "EUR/USD"); b != nil && b.State == excredis.CircuitOpen {
		t.Fatal("points outside the 60s window must not trip the breaker")
	}
}

func TestInstrumentLimitOverride(t *testing.T) {
	r := newRig(t)
	svc, err := NewCircuitBreakerService(BreakerDeps{
		Store: r.store, Events: r.events, Metrics: NewBreakerMetrics(r.reg),
		Now: r.clock.Now,
		PriceLimit: func(_ context.Context, symbol string) (decimal.Decimal, bool) {
			if symbol == "EUR/USD" {
				return decimal.NewFromInt(10), true // 10% instrument_price_limit
			}
			return decimal.Zero, false
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	svc.ObservePrice(ctx, "EURUSD", d("1.0000"))
	svc.ObservePrice(ctx, "EURUSD", d("1.0600")) // +6% — under the 10% override
	if b := breaker(t, svc, ScopeInstrument, "EUR/USD"); b != nil && b.State == excredis.CircuitOpen {
		t.Fatal("per-instrument 10% limit must override the 5% default")
	}
}

func TestAccountLossRapidTrip(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()

	r.svc.ObserveAccountLoss(ctx, 7, d("6"))   // 1
	r.svc.ObserveAccountLoss(ctx, 7, d("2.5")) // under 5% — does not count
	r.svc.ObserveAccountLoss(ctx, 7, d("7"))   // 2
	if b := breaker(t, r.svc, ScopeAccount, "7"); b != nil && b.State == excredis.CircuitOpen {
		t.Fatal("2 qualifying losses must not trip (need >= 3)")
	}
	r.svc.ObserveAccountLoss(ctx, 7, d("5.1")) // 3rd rapid loss > 5%
	b := breaker(t, r.svc, ScopeAccount, "7")
	if b == nil || b.State != excredis.CircuitOpen {
		t.Fatalf("expected ACCOUNT:7 OPEN, got %+v", b)
	}
	requireCode(t, r.svc.AdmitOrder(ctx, 7, "EURUSD"), CodeCircuitBreakerOpen)
	if err := r.svc.AdmitOrder(ctx, 8, "EURUSD"); err != nil {
		t.Fatalf("other accounts must admit, got %v", err)
	}

	// ACCOUNT never auto-recovers: 31min later still OPEN (no probe window).
	r.clock.Add(31 * time.Minute)
	requireCode(t, r.svc.AdmitOrder(ctx, 7, "EURUSD"), CodeCircuitBreakerOpen)
	if b = breaker(t, r.svc, ScopeAccount, "7"); b.State != excredis.CircuitOpen {
		t.Fatalf("ACCOUNT must stay OPEN (manual recovery only), got %s", b.State)
	}
}

func TestAccountLossWindowPrunes(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	r.svc.ObserveAccountLoss(ctx, 9, d("6"))
	r.clock.Add(6 * time.Minute) // first event falls out of the 5min window
	r.svc.ObserveAccountLoss(ctx, 9, d("6"))
	r.svc.ObserveAccountLoss(ctx, 9, d("6"))
	if b := breaker(t, r.svc, ScopeAccount, "9"); b != nil && b.State == excredis.CircuitOpen {
		t.Fatal("only 2 losses inside 5min — must not trip")
	}
}

func TestVolumeSpikeZScore(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	sym := "GBPUSD"

	// Seed a 40-bucket baseline alternating 90/110 → mean 100, sd 10.
	cur := r.clock.Now().Unix() / int64(VolumeBucket.Seconds())
	m := map[int64]decimal.Decimal{}
	for i := int64(1); i <= 40; i++ {
		if i%2 == 0 {
			m[cur-i] = decimal.NewFromInt(90)
		} else {
			m[cur-i] = decimal.NewFromInt(110)
		}
	}
	r.svc.mu.Lock()
	r.svc.vols[config.CanonicalSymbol(sym)] = m
	r.svc.mu.Unlock()

	r.svc.ObserveTrade(ctx, sym, decimal.NewFromInt(130)) // z=3 → no trip
	if b := breaker(t, r.svc, ScopeVolumeSpike, config.CanonicalSymbol(sym)); b != nil && b.State == excredis.CircuitOpen {
		t.Fatal("z=3 must not trip a 4σ breaker")
	}
	r.svc.ObserveTrade(ctx, sym, decimal.NewFromInt(15)) // cur=145 → z=4.5 → trip
	b := breaker(t, r.svc, ScopeVolumeSpike, config.CanonicalSymbol(sym))
	if b == nil || b.State != excredis.CircuitOpen {
		t.Fatalf("expected VOLUME_SPIKE:%s OPEN at z>=4, got %+v", sym, b)
	}
}

func TestVolumeSpikeThinBaselineNoTrip(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	// <30 baseline buckets → honest "insufficient data", never a trip.
	r.svc.ObserveTrade(ctx, "NZDUSD", decimal.NewFromInt(10))
	r.svc.ObserveTrade(ctx, "NZDUSD", decimal.NewFromInt(1_000_000))
	if b := breaker(t, r.svc, ScopeVolumeSpike, "NZD/USD"); b != nil {
		t.Fatalf("thin baseline must not trip, got %+v", b)
	}
}

func TestOptionsVolatilityTrip(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	avg := d("0.10")
	r.svc.ObserveIV(ctx, "EURUSD", d("0.30"), avg) // exactly +200% — not >200%
	if b := breaker(t, r.svc, ScopeOptionsVolatility, "EUR/USD"); b != nil && b.State == excredis.CircuitOpen {
		t.Fatal("exactly +200% must not trip (strict >)")
	}
	r.svc.ObserveIV(ctx, "EURUSD", d("0.31"), avg) // +210% → trip
	b := breaker(t, r.svc, ScopeOptionsVolatility, "EUR/USD")
	if b == nil || b.State != excredis.CircuitOpen {
		t.Fatalf("expected OPTIONS_VOLATILITY:EURUSD OPEN, got %+v", b)
	}
	if b.Trigger["iv"] != "0.31" || b.Trigger["iv_avg_30d"] != "0.1" {
		t.Fatalf("trigger params not recorded: %v", b.Trigger)
	}
}

func TestNullIVSourceFailsClosed(t *testing.T) {
	r := newRig(t)
	r.svc.PollIV(context.Background(), "EURUSD") // must log-and-return, never panic/trip
	if len(r.svc.Status()) != 0 {
		t.Fatal("NullIVSource must never produce a trip")
	}
}

func TestMarketWideTrip(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	// Two instruments >20% — under the ">2 instruments" threshold.
	r.svc.ObservePrice(ctx, "EURUSD", d("1.00"))
	r.svc.ObservePrice(ctx, "EURUSD", d("1.25"))
	r.svc.ObservePrice(ctx, "GBPUSD", d("1.00"))
	r.svc.ObservePrice(ctx, "GBPUSD", d("0.75"))
	if b := breaker(t, r.svc, ScopeMarketWide, MarketWideID); b != nil && b.State == excredis.CircuitOpen {
		t.Fatal("2 instruments must not trip MARKET_WIDE")
	}
	// Third instrument breaches → market-wide opens.
	r.svc.ObservePrice(ctx, "USDJPY", d("100"))
	r.svc.ObservePrice(ctx, "USDJPY", d("125"))
	b := breaker(t, r.svc, ScopeMarketWide, MarketWideID)
	if b == nil || b.State != excredis.CircuitOpen {
		t.Fatalf("expected MARKET_WIDE OPEN, got %+v", b)
	}
	// Blocks admission for every symbol, even a calm one.
	requireCode(t, r.svc.AdmitOrder(ctx, 1, "AUDUSD"), CodeCircuitBreakerOpen)
	// Manual resume is the only way out: sweep advances never touch it.
	r.clock.Add(2 * time.Hour)
	requireCode(t, r.svc.AdmitOrder(ctx, 1, "AUDUSD"), CodeCircuitBreakerOpen)
	if err := r.svc.ManualReset(ctx, ScopeMarketWide, "", 77); err != nil {
		t.Fatalf("manual reset: %v", err)
	}
	if err := r.svc.AdmitOrder(ctx, 1, "AUDUSD"); err != nil {
		t.Fatalf("market-wide reset must admit calm symbol, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// Admin operations + probe recovery
// ---------------------------------------------------------------------------

func TestManualTripAndReset(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()

	if err := r.svc.ManualTrip(ctx, "instrument", "eurusd", "ops halt", 7); err != nil {
		t.Fatalf("manual trip: %v", err)
	}
	b := breaker(t, r.svc, ScopeInstrument, "EUR/USD")
	if b == nil || b.State != excredis.CircuitOpen || !b.Manual || b.ActorID != 7 {
		t.Fatalf("manual trip record wrong: %+v", b)
	}
	requireCode(t, r.svc.AdmitOrder(ctx, 1, "EURUSD"), CodeCircuitBreakerOpen)

	if err := r.svc.ManualReset(ctx, "INSTRUMENT", "EUR/USD", 9); err != nil {
		t.Fatalf("manual reset: %v", err)
	}
	b = breaker(t, r.svc, ScopeInstrument, "EUR/USD")
	if b.State != excredis.CircuitClosed {
		t.Fatalf("expected CLOSED, got %s", b.State)
	}
	if err := r.svc.AdmitOrder(ctx, 1, "EURUSD"); err != nil {
		t.Fatalf("post-reset admit: %v", err)
	}
	// Second reset → NOT_FOUND (nothing open).
	requireCode(t, r.svc.ManualReset(ctx, "INSTRUMENT", "EUR/USD", 9), "NOT_FOUND")
	// Garbage scope → INVALID_REQUEST.
	requireCode(t, r.svc.ManualTrip(ctx, "GALACTIC", "X", "", 1), "INVALID_REQUEST")
}

func TestProbeRecoveryCycle(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()

	if err := r.svc.ManualTrip(ctx, ScopeInstrument, "EURUSD", "ops halt", 7); err != nil {
		t.Fatal(err)
	}
	// Advance past the 5min hold (and the 60s cooldown).
	r.clock.Add(5*time.Minute + time.Second)
	for i := 1; i <= ProbeCount; i++ {
		if err := r.svc.AdmitOrder(ctx, 1, "EURUSD"); err != nil {
			t.Fatalf("probe %d must be admitted: %v", i, err)
		}
		if i < ProbeCount {
			if b := breaker(t, r.svc, ScopeInstrument, "EUR/USD"); b.State != excredis.CircuitHalfOpen || b.Probes != i {
				t.Fatalf("probe %d: state=%s probes=%d", i, b.State, b.Probes)
			}
		}
	}
	// 10th probe completed the window → CLOSED.
	b := breaker(t, r.svc, ScopeInstrument, "EUR/USD")
	if b.State != excredis.CircuitClosed {
		t.Fatalf("10/10 probes must close the breaker, got %s", b.State)
	}
	if err := r.svc.AdmitOrder(ctx, 1, "EURUSD"); err != nil {
		t.Fatalf("closed breaker must admit, got %v", err)
	}

	// Event trail: CLOSED→OPEN, OPEN→HALF_OPEN, HALF_OPEN→CLOSED.
	var got []string
	for _, ev := range r.events.list() {
		got = append(got, ev.FromState+"→"+ev.ToState)
	}
	want := []string{"CLOSED→OPEN", "OPEN→HALF_OPEN", "HALF_OPEN→CLOSED"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("event trail %v, want %v", got, want)
	}
	if r.pub.count() != 3 {
		t.Fatalf("WS admin monitor got %d events, want 3", r.pub.count())
	}
	// Prometheus: state gauge 0 (CLOSED) + 3 transitions.
	out := r.reg.String()
	if !strings.Contains(out, `circuit_breaker_state{id="EUR/USD",scope="INSTRUMENT"} 0`) &&
		!strings.Contains(out, `circuit_breaker_state{scope="INSTRUMENT",id="EUR/USD"} 0`) {
		t.Fatalf("state gauge missing/ wrong in exposition:\n%s", out)
	}
	if !strings.Contains(out, `circuit_breaker_transitions_total{scope="INSTRUMENT",id="EUR/USD"} 3`) &&
		!strings.Contains(out, `circuit_breaker_transitions_total{id="EUR/USD",scope="INSTRUMENT"} 3`) {
		t.Fatalf("transitions counter wrong:\n%s", out)
	}
}

func TestProbeWindowExpiryReopens(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	if err := r.svc.ManualTrip(ctx, ScopeInstrument, "EURUSD", "halt", 7); err != nil {
		t.Fatal(err)
	}
	r.clock.Add(6 * time.Minute) // hold expired → first admit opens HALF_OPEN
	for i := 0; i < 3; i++ {
		if err := r.svc.AdmitOrder(ctx, 1, "EURUSD"); err != nil {
			t.Fatalf("probe %d: %v", i, err)
		}
	}
	r.clock.Add(31 * time.Second) // probe window lapses at 3/10
	requireCode(t, r.svc.AdmitOrder(ctx, 1, "EURUSD"), CodeCircuitBreakerOpen)
	b := breaker(t, r.svc, ScopeInstrument, "EUR/USD")
	if b.State != excredis.CircuitOpen {
		t.Fatalf("expired probe window must re-OPEN, got %s", b.State)
	}
	if b.Flaps != 0 {
		t.Fatalf("failed probe window is same episode — flaps must stay 0, got %d", b.Flaps)
	}
	if b.HoldMS != (5 * time.Minute).Milliseconds() {
		t.Fatalf("re-open must reuse the same hold, got %dms", b.HoldMS)
	}
}

func TestTransitionCooldown(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	// Seed an OPEN record whose hold already expired but whose trip was
	// 30s ago — the 60s trip→recovery cooldown must hold it OPEN.
	now := r.clock.Now()
	r.store.recs["INSTRUMENT|EUR/USD"] = &excredis.CircuitBreaker{
		State: excredis.CircuitOpen,
		Metadata: map[string]string{
			"entered_at_ms": fmt.Sprint(now.Add(-30 * time.Second).UnixMilli()),
			"hold_until_ms": fmt.Sprint(now.Add(-30 * time.Second).UnixMilli()),
			"last_trip_ms":  fmt.Sprint(now.Add(-30 * time.Second).UnixMilli()),
			"hold_ms":       "0",
		},
	}
	requireCode(t, r.svc.AdmitOrder(ctx, 1, "EURUSD"), CodeCircuitBreakerOpen)
	if b := breaker(t, r.svc, ScopeInstrument, "EUR/USD"); b.State != excredis.CircuitOpen {
		t.Fatal("60s cooldown must block OPEN→HALF_OPEN even past hold")
	}
	r.clock.Add(31 * time.Second) // trip is now 61s old
	if err := r.svc.AdmitOrder(ctx, 1, "EURUSD"); err != nil {
		t.Fatalf("post-cooldown admission must open the probe window: %v", err)
	}
	if b := breaker(t, r.svc, ScopeInstrument, "EUR/USD"); b.State != excredis.CircuitHalfOpen {
		t.Fatalf("expected HALF_OPEN after cooldown, got %s", b.State)
	}
}

// ---------------------------------------------------------------------------
// Flapping
// ---------------------------------------------------------------------------

func TestFlapDoublesHoldAndAlerts(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()

	// Automated trip → manual recovery → automated re-trip within 15min.
	for i := 0; i < 3; i++ {
		r.svc.ObserveAccountLoss(ctx, 7, d("6"))
	}
	if b := breaker(t, r.svc, ScopeAccount, "7"); b.HoldMS != (30 * time.Minute).Milliseconds() {
		t.Fatalf("first trip hold = %dms, want 30min", b.HoldMS)
	}
	if err := r.svc.ManualReset(ctx, ScopeAccount, "7", 9); err != nil {
		t.Fatal(err)
	}
	r.clock.Add(10 * time.Minute) // inside the 15min flap window
	for i := 0; i < 3; i++ {
		r.svc.ObserveAccountLoss(ctx, 7, d("6"))
	}
	b := breaker(t, r.svc, ScopeAccount, "7")
	if b.Flaps != 1 || b.HoldMS != (60*time.Minute).Milliseconds() {
		t.Fatalf("re-trip must double hold to 60min (flaps=1): %+v", b)
	}
	if r.alerts.count() != 1 {
		t.Fatalf("flap must page Risk Management once, got %d", r.alerts.count())
	}
	if !strings.Contains(r.alerts.calls[0], "P1|CIRCUIT_BREAKER_FLAPPING") {
		t.Fatalf("alert payload wrong: %s", r.alerts.calls[0])
	}
	if b.Trigger["flap_count"] != "1" {
		t.Fatalf("flap_count missing from trigger: %v", b.Trigger)
	}

	// A quiet >15min gap resets the ladder — next trip is base hold.
	if err := r.svc.ManualReset(ctx, ScopeAccount, "7", 9); err != nil {
		t.Fatal(err)
	}
	r.clock.Add(20 * time.Minute)
	for i := 0; i < 3; i++ {
		r.svc.ObserveAccountLoss(ctx, 7, d("6"))
	}
	if b = breaker(t, r.svc, ScopeAccount, "7"); b.Flaps != 0 ||
		b.HoldMS != (30*time.Minute).Milliseconds() {
		t.Fatalf("post-window trip must reset the ladder: %+v", b)
	}
}

func TestManualTripSkipsFlap(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		r.svc.ObserveAccountLoss(ctx, 7, d("6"))
	}
	if err := r.svc.ManualReset(ctx, ScopeAccount, "7", 9); err != nil {
		t.Fatal(err)
	}
	// Manual re-trip within the flap window: operator intent, no doubling.
	if err := r.svc.ManualTrip(ctx, ScopeAccount, "7", "ops", 9); err != nil {
		t.Fatal(err)
	}
	if b := breaker(t, r.svc, ScopeAccount, "7"); b.Flaps != 0 ||
		b.HoldMS != (30*time.Minute).Milliseconds() {
		t.Fatalf("manual trip must not double the hold: %+v", b)
	}
	if r.alerts.count() != 0 {
		t.Fatal("manual trip must not page the flap alert")
	}
}

func TestFlapCap(t *testing.T) {
	if got := effectiveHoldMS(5*time.Minute, 0); got != (5 * time.Minute).Milliseconds() {
		t.Fatalf("base hold %d", got)
	}
	if got := effectiveHoldMS(5*time.Minute, 1); got != (10 * time.Minute).Milliseconds() {
		t.Fatalf("1 flap %d", got)
	}
	if got := effectiveHoldMS(30*time.Minute, 2); got != MaxHold.Milliseconds() {
		t.Fatalf("30min × 4 must cap at 120min, got %d", got)
	}
	if got := effectiveHoldMS(5*time.Minute, 6); got != MaxHold.Milliseconds() {
		t.Fatalf("deep flap ladder must cap at 120min, got %d", got)
	}
}

// ---------------------------------------------------------------------------
// Fail-closed + persistence
// ---------------------------------------------------------------------------

func TestNilStoreFailsClosed(t *testing.T) {
	if _, err := NewCircuitBreakerService(BreakerDeps{}); err == nil {
		t.Fatal("nil store must fail construction")
	}
	// And a nil service gate rejects (defensive — orders.Service nil-checks first).
	var s *CircuitBreakerService
	requireCode(t, s.AdmitOrder(context.Background(), 1, "EURUSD"), CodeCircuitBreakerOpen)
}

func TestStoreErrorFailsClosed(t *testing.T) {
	r := newRig(t)
	r.store.getErr = fmt.Errorf("redis down")
	requireCode(t, r.svc.AdmitOrder(context.Background(), 1, "EURUSD"), CodeCircuitBreakerOpen)
}

func TestCorruptRecordFailsClosed(t *testing.T) {
	r := newRig(t)
	r.store.recs["INSTRUMENT|EUR/USD"] = &excredis.CircuitBreaker{
		State: "BOGUS", Metadata: map[string]string{}}
	requireCode(t, r.svc.AdmitOrder(context.Background(), 1, "EURUSD"), CodeCircuitBreakerOpen)
}

func TestPersistErrorStillTrips(t *testing.T) {
	r := newRig(t)
	r.store.setErr = fmt.Errorf("redis write down")
	err := r.svc.ManualTrip(context.Background(), ScopeInstrument, "EURUSD", "halt", 7)
	requireCode(t, err, CodeCircuitBreakerOpen)
	// Safety wins: in-memory OPEN still rejects admission.
	requireCode(t, r.svc.AdmitOrder(context.Background(), 1, "EURUSD"), CodeCircuitBreakerOpen)
}

func TestLoadHydrates(t *testing.T) {
	r := newRig(t)
	now := r.clock.Now()
	r.store.recs["INSTRUMENT|EUR/USD"] = &excredis.CircuitBreaker{
		State: excredis.CircuitOpen,
		Metadata: map[string]string{
			"entered_at_ms": fmt.Sprint(now.UnixMilli()),
			"hold_until_ms": fmt.Sprint(now.Add(5 * time.Minute).UnixMilli()),
			"last_trip_ms":  fmt.Sprint(now.UnixMilli()),
			"hold_ms":       "300000",
		},
	}
	r.store.recs["ACCOUNT|7"] = &excredis.CircuitBreaker{
		State: excredis.CircuitHalfOpen,
		Metadata: map[string]string{
			"entered_at_ms": fmt.Sprint(now.UnixMilli()),
			"window_end_ms": fmt.Sprint(now.Add(30 * time.Second).UnixMilli()),
			"probes":        "2",
		},
	}
	if err := r.svc.Load(context.Background()); err != nil {
		t.Fatalf("load: %v", err)
	}
	requireCode(t, r.svc.AdmitOrder(context.Background(), 1, "EURUSD"), CodeCircuitBreakerOpen)
	// HALF_OPEN record resumes its probe count.
	if err := r.svc.AdmitOrder(context.Background(), 7, "USDJPY"); err != nil {
		t.Fatalf("half-open account probe must admit: %v", err)
	}
	if b := breaker(t, r.svc, ScopeAccount, "7"); b.Probes != 3 {
		t.Fatalf("hydrated probe count = %d, want 3", b.Probes)
	}
}

func TestRedisKeyLayout(t *testing.T) {
	r := newRig(t)
	if err := r.svc.ManualTrip(context.Background(), ScopeInstrument, "EURUSD", "x", 1); err != nil {
		t.Fatal(err)
	}
	// The store seam keys by scope/id — verify the hash the Redis helper
	// would write carries every field the hydrator reads back.
	cb := r.store.recs["INSTRUMENT|EUR/USD"]
	if cb == nil || cb.State != excredis.CircuitOpen {
		t.Fatalf("stored record: %+v", cb)
	}
	for _, f := range []string{"entered_at_ms", "hold_until_ms", "last_trip_ms", "hold_ms", "probes"} {
		if cb.Metadata[f] == "" {
			t.Fatalf("hash missing field %q: %v", f, cb.Metadata)
		}
	}
	// Round-trip through the hydrator.
	b := breakerFromHash("INSTRUMENT", "EUR/USD", cb)
	if b == nil || b.State != excredis.CircuitOpen {
		t.Fatalf("round-trip hydrate: %+v", b)
	}
}

// ---------------------------------------------------------------------------
// Integration — Redis (EXC_REDIS_TEST=1) and Postgres (EXC_PG_TEST=1)
// ---------------------------------------------------------------------------

// TestRedisBreakerIntegration exercises the real RedisBreakerStore over a
// scratch Redis DB: trip → hash on circuit_breaker:INSTRUMENT:EURUSD →
// fresh service Load() gates admission. Run:
// EXC_REDIS_TEST=1 go test ./internal/risk/ -run TestRedisBreaker -v
func TestRedisBreakerIntegration(t *testing.T) {
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
	svc, err := NewCircuitBreakerService(BreakerDeps{
		Store: RedisBreakerStore{C: rdb}, Now: clock.Now})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.ManualTrip(ctx, ScopeInstrument, "EURUSD", "itest", 7); err != nil {
		t.Fatalf("trip: %v", err)
	}
	m, err := rdb.HGetAll(ctx, "circuit_breaker:INSTRUMENT:EUR/USD").Result()
	if err != nil || m["state"] != "OPEN" {
		t.Fatalf("redis hash: %v %v", m, err)
	}
	// Fresh service hydrating from Redis gates admission — restart safety.
	svc2, err := NewCircuitBreakerService(BreakerDeps{
		Store: RedisBreakerStore{C: rdb}, Now: clock.Now})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc2.Load(ctx); err != nil {
		t.Fatalf("load: %v", err)
	}
	requireCode(t, svc2.AdmitOrder(ctx, 1, "EURUSD"), CodeCircuitBreakerOpen)
}

// TestPgBreakerEventStoreIntegration round-trips one audit row through
// migration 206's table. Skips cleanly when the scratch DB lacks the
// table (migration not applied). Run:
// EXC_PG_TEST=1 go test ./internal/risk/ -run TestPgBreaker -v
func TestPgBreakerEventStoreIntegration(t *testing.T) {
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
		`SELECT to_regclass('circuit_breaker_events') IS NOT NULL`).Scan(&exists); err != nil {
		t.Fatalf("regclass: %v", err)
	}
	if !exists {
		t.Skip("migration 206 not applied to this database")
	}

	st := NewPgBreakerEventStore(pool)
	ev := BreakerEvent{
		Scope: ScopeInstrument, ID: "ITEST_PAIR", FromState: "CLOSED",
		ToState: "OPEN", Reason: "integration test",
		Trigger: map[string]string{"move_pct": "6.5", "limit_pct": "5"},
		HoldMS:  300000, At: time.Now().UTC(),
	}
	if err := st.InsertBreakerEvent(ctx, ev); err != nil {
		t.Fatalf("insert: %v", err)
	}
	// Clean up the row afterwards — the scratch DB is shared.
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM circuit_breaker_events WHERE target_id='ITEST_PAIR'`)
	})
	evs, err := st.RecentEvents(ctx, 10)
	if err != nil {
		t.Fatalf("recent: %v", err)
	}
	var found *BreakerEvent
	for i := range evs {
		if evs[i].ID == "ITEST_PAIR" {
			found = &evs[i]
			break
		}
	}
	if found == nil {
		t.Fatal("inserted event not returned by RecentEvents")
	}
	if found.FromState != "CLOSED" || found.ToState != "OPEN" ||
		found.Trigger["move_pct"] != "6.5" || found.HoldMS != 300000 {
		t.Fatalf("round-trip mismatch: %+v", found)
	}
}
