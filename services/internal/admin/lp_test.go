// Unit tests for the Task 7.3.9 LP management module — fake lpStore /
// MetricsSource / AlertSink; PG-backed coverage lives in
// admin_integration_test.go (EXC_PG_TEST=1).
package admin

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	excerrors "exchange/pkg/errors"
)

// ---------------------------------------------------------------------------
// Fakes
// ---------------------------------------------------------------------------

type fakeLPStore struct {
	created   *LiquidityProvider
	lps       map[int64]*LiquidityProvider
	nextID    int64
	insertErr error
	getErr    error

	scSaved   []LPScorecard
	scErr     error
	alerts    []LPAlert
	inserted  bool // controls the dedup outcome
	alertErr  error
	activeLPs int
}

func newFakeLPStore() *fakeLPStore {
	return &fakeLPStore{lps: map[int64]*LiquidityProvider{}, nextID: 1, inserted: true}
}

func (f *fakeLPStore) insertLP(_ context.Context, lp LiquidityProvider, _ AdminActor) (*LiquidityProvider, error) {
	if f.insertErr != nil {
		return nil, f.insertErr
	}
	lp.LPID = f.nextID
	f.nextID++
	f.lps[lp.LPID] = &lp
	f.created = &lp
	return &lp, nil
}

func (f *fakeLPStore) getLP(_ context.Context, id int64) (*LiquidityProvider, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	lp, ok := f.lps[id]
	if !ok {
		return nil, excerrors.New("NOT_FOUND", "lp not found")
	}
	return lp, nil
}

func (f *fakeLPStore) listLPs(_ context.Context, status string) ([]LiquidityProvider, error) {
	out := []LiquidityProvider{}
	for _, lp := range f.lps {
		if status == "" || string(lp.Status) == status {
			out = append(out, *lp)
		}
	}
	return out, nil
}

func (f *fakeLPStore) updateLP(_ context.Context, upd LPUpdate, _ AdminActor) (*LiquidityProvider, error) {
	lp, ok := f.lps[upd.LPID]
	if !ok {
		return nil, excerrors.New("NOT_FOUND", "lp not found")
	}
	if upd.Status != nil {
		to := LPStatus(*upd.Status)
		if !canTransitionLP(lp.Status, to) {
			return nil, excerrors.New("INVALID_LIFECYCLE_TRANSITION", "bad transition")
		}
		lp.Status = to
	}
	return lp, nil
}

func (f *fakeLPStore) saveScorecard(_ context.Context, _ int64, sc LPScorecard) error {
	if f.scErr != nil {
		return f.scErr
	}
	f.scSaved = append(f.scSaved, sc)
	return nil
}

func (f *fakeLPStore) insertAlert(_ context.Context, a LPAlert) (*LPAlert, bool, error) {
	if f.alertErr != nil {
		return nil, false, f.alertErr
	}
	if !f.inserted {
		return nil, false, nil
	}
	a.ID = int64(len(f.alerts) + 1)
	f.alerts = append(f.alerts, a)
	return &a, true, nil
}

func (f *fakeLPStore) listAlerts(_ context.Context, _ int64, _ bool) ([]LPAlert, error) {
	return f.alerts, nil
}

func (f *fakeLPStore) countActiveLPs(_ context.Context) (int, error) {
	return f.activeLPs, nil
}

type fakeMetrics struct {
	sc  *LPScorecard
	err error
	got time.Duration
}

func (m *fakeMetrics) CollectLPMetrics(_ context.Context, _ int64, w time.Duration) (*LPScorecard, error) {
	m.got = w
	return m.sc, m.err
}

type fakeAlertSink struct {
	got []LPAlert
	err error
}

func (s *fakeAlertSink) EmitLPAlert(_ context.Context, a LPAlert) error {
	s.got = append(s.got, a)
	return s.err
}

func lpRoles(m map[int64]string) AdminRoleResolver {
	return func(_ context.Context, id int64) (string, error) {
		r, ok := m[id]
		if !ok {
			return "", errors.New("no role")
		}
		return r, nil
	}
}

func codeOf(t *testing.T, err error) string {
	t.Helper()
	var e *excerrors.Error
	if !errors.As(err, &e) {
		t.Fatalf("expected coded error, got %v", err)
	}
	return e.Code
}

// ---------------------------------------------------------------------------
// Role gate
// ---------------------------------------------------------------------------

func TestLPCreateRoleGate(t *testing.T) {
	store := newFakeLPStore()
	req := LPCreate{Name: "Acme LP", ConnectionType: "FIX"}

	// nil resolver fails closed.
	svc := newLPService(store, nil, nil, nil, DefaultLPThresholds())
	if _, err := svc.Create(context.Background(), AdminActor{UserID: 7}, req); err == nil {
		t.Fatal("nil resolver should fail closed")
	} else if codeOf(t, err) != "UNAUTHORIZED_ROLE" {
		t.Fatalf("want UNAUTHORIZED_ROLE, got %s", eCode(err))
	}

	// wrong role rejected.
	svc = newLPService(store, lpRoles(map[int64]string{7: "Support Agent"}), nil, nil, DefaultLPThresholds())
	if _, err := svc.Create(context.Background(), AdminActor{UserID: 7}, req); err == nil {
		t.Fatal("Support Agent must not create LPs")
	} else if codeOf(t, err) != "UNAUTHORIZED_ROLE" {
		t.Fatalf("want UNAUTHORIZED_ROLE, got %s", eCode(err))
	}

	// no identity at all.
	if _, err := svc.Create(context.Background(), AdminActor{}, req); err == nil {
		t.Fatal("anonymous must be rejected")
	} else if codeOf(t, err) != "UNAUTHORIZED" {
		t.Fatalf("want UNAUTHORIZED, got %s", eCode(err))
	}
}

// ---------------------------------------------------------------------------
// Create validation + lifecycle entry
// ---------------------------------------------------------------------------

func TestLPCreateValidation(t *testing.T) {
	rm := lpRoles(map[int64]string{1: "Risk Manager"})
	svc := newLPService(newFakeLPStore(), rm, nil, nil, DefaultLPThresholds())
	actor := AdminActor{UserID: 1}

	if _, err := svc.Create(context.Background(), actor, LPCreate{Name: "", ConnectionType: "FIX"}); err == nil {
		t.Fatal("empty name must fail")
	}
	if _, err := svc.Create(context.Background(), actor, LPCreate{Name: "x", ConnectionType: "GRPC"}); err == nil {
		t.Fatal("bad connection_type must fail")
	}
	if _, err := svc.Create(context.Background(), actor, LPCreate{
		Name: "x", ConnectionType: "FIX", StalenessTimeoutMS: -1}); err == nil {
		t.Fatal("negative staleness must fail")
	}
	if _, err := svc.Create(context.Background(), actor, LPCreate{
		Name: "x", ConnectionType: "FIX", Contact: json.RawMessage(`[1]`)}); err == nil {
		t.Fatal("non-object contact must fail")
	}
	if _, err := svc.Create(context.Background(), actor, LPCreate{
		Name: "x", ConnectionType: "FIX",
		Instruments: []LPInstrumentConfig{{InstrumentID: 1, SkewBps: "not-a-number"}}}); err == nil {
		t.Fatal("non-decimal skew_bps must fail")
	}
	if _, err := svc.Create(context.Background(), actor, LPCreate{
		Name: "x", ConnectionType: "FIX",
		Instruments: []LPInstrumentConfig{{InstrumentID: 1, SpreadMarkupBidBps: "20000"}}}); err == nil {
		t.Fatal("bps beyond the ±10000 bound must fail")
	}
}

func TestLPCreateLifecycleEntry(t *testing.T) {
	rm := lpRoles(map[int64]string{1: "Super Admin"})
	store := newFakeLPStore()
	svc := newLPService(store, rm, nil, nil, DefaultLPThresholds())

	lp, err := svc.Create(context.Background(), AdminActor{UserID: 1}, LPCreate{
		Name: "Acme LP", ConnectionType: "FIX",
		Instruments: []LPInstrumentConfig{
			{InstrumentID: 11, Enabled: true, SpreadMarkupBidBps: "1.5"},
		},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if lp.Status != LPStatusOnboarding {
		t.Fatalf("new LP must enter ONBOARDING, got %s", lp.Status)
	}
	if lp.CreatedBy != 1 {
		t.Fatalf("created_by must record the actor, got %d", lp.CreatedBy)
	}
}

// ---------------------------------------------------------------------------
// Lifecycle transitions (pure state machine)
// ---------------------------------------------------------------------------

func TestLPTransitions(t *testing.T) {
	ok := [][2]LPStatus{
		{LPStatusOnboarding, LPStatusActive},
		{LPStatusOnboarding, LPStatusSuspended},
		{LPStatusActive, LPStatusSuspended},
		{LPStatusSuspended, LPStatusActive},     // resume
		{LPStatusSuspended, LPStatusOnboarding}, // re-onboard
	}
	for _, tr := range ok {
		if !canTransitionLP(tr[0], tr[1]) {
			t.Errorf("expected %s→%s allowed", tr[0], tr[1])
		}
	}
	bad := [][2]LPStatus{
		{LPStatusActive, LPStatusOnboarding},
		{LPStatusOnboarding, LPStatusOnboarding},
	}
	for _, tr := range bad {
		if canTransitionLP(tr[0], tr[1]) {
			t.Errorf("expected %s→%s rejected", tr[0], tr[1])
		}
	}
}

// ---------------------------------------------------------------------------
// Scorecard: live source, persisted fallback, alerts
// ---------------------------------------------------------------------------

func healthyScorecard() *LPScorecard {
	return &LPScorecard{
		Window: "1h", QuotesReceived: 1000, Fills: 900, Rejections: 5,
		FillRatio: 0.90, RejectionRate: 0.005, AvgResponseTimeMS: 2.1,
		AvailabilityPct: 99.7, SpreadQualityBps: 0.4,
		ComputedAt: time.Now().UTC(), Source: "test",
	}
}

func TestLPScorecardLivePath(t *testing.T) {
	rm := lpRoles(map[int64]string{1: "Risk Manager"})
	store := newFakeLPStore()
	store.lps[1] = &LiquidityProvider{LPID: 1, Status: LPStatusActive}
	sink := &fakeAlertSink{}
	metrics := &fakeMetrics{sc: healthyScorecard()}
	svc := newLPService(store, rm, metrics, sink, DefaultLPThresholds())

	sc, fired, err := svc.Scorecard(context.Background(), AdminActor{UserID: 1}, 1, 0)
	if err != nil {
		t.Fatalf("scorecard: %v", err)
	}
	if sc.FillRatio != 0.90 || sc.Stale {
		t.Fatalf("live scorecard expected, got %+v", sc)
	}
	if len(fired) != 0 {
		t.Fatalf("healthy LP must not fire alerts, got %+v", fired)
	}
	if metrics.got != time.Hour {
		t.Fatalf("default window must be 1h, got %v", metrics.got)
	}
	if len(store.scSaved) != 1 {
		t.Fatalf("live scorecard must persist the snapshot")
	}
}

func TestLPScorecardThresholdAlerts(t *testing.T) {
	rm := lpRoles(map[int64]string{1: "Risk Manager"})
	store := newFakeLPStore()
	store.lps[1] = &LiquidityProvider{LPID: 1, Status: LPStatusActive}
	sink := &fakeAlertSink{}
	bad := healthyScorecard()
	bad.FillRatio = 0.70       // < 0.80 → fill_ratio alert
	bad.AvailabilityPct = 90.0 // < 95 → availability_pct alert
	metrics := &fakeMetrics{sc: bad}
	svc := newLPService(store, rm, metrics, sink, DefaultLPThresholds())

	_, fired, err := svc.Scorecard(context.Background(), AdminActor{UserID: 1}, 1, time.Hour)
	if err != nil {
		t.Fatalf("scorecard: %v", err)
	}
	if len(fired) != 2 {
		t.Fatalf("want 2 alerts (fill_ratio + availability), got %d", len(fired))
	}
	if len(sink.got) != 2 {
		t.Fatalf("sink must receive both alerts, got %d", len(sink.got))
	}

	// Second evaluation on a still-breaching LP must not duplicate the
	// OPEN rows (store dedup).
	store.inserted = false
	_, fired2, err := svc.Scorecard(context.Background(), AdminActor{UserID: 1}, 1, time.Hour)
	if err != nil {
		t.Fatalf("scorecard #2: %v", err)
	}
	if len(fired2) != 0 || len(store.alerts) != 2 {
		t.Fatalf("OPEN alerts must dedupe: fired=%d stored=%d", len(fired2), len(store.alerts))
	}
}

func TestLPScorecardPersistedFallback(t *testing.T) {
	rm := lpRoles(map[int64]string{1: "Risk Manager"})
	store := newFakeLPStore()
	snap, _ := json.Marshal(healthyScorecard())
	store.lps[1] = &LiquidityProvider{LPID: 1, Status: LPStatusActive, Scorecard: snap}

	// nil metrics → persisted snapshot marked stale.
	svc := newLPService(store, rm, nil, nil, DefaultLPThresholds())
	sc, _, err := svc.Scorecard(context.Background(), AdminActor{UserID: 1}, 1, 0)
	if err != nil {
		t.Fatalf("persisted fallback: %v", err)
	}
	if !sc.Stale || sc.FillRatio != 0.90 {
		t.Fatalf("want stale persisted snapshot, got %+v", sc)
	}

	// metrics error + persisted snapshot → stale fallback.
	metrics := &fakeMetrics{err: errors.New("clickhouse down")}
	svc = newLPService(store, rm, metrics, nil, DefaultLPThresholds())
	sc, _, err = svc.Scorecard(context.Background(), AdminActor{UserID: 1}, 1, 0)
	if err != nil {
		t.Fatalf("fallback on source error: %v", err)
	}
	if !sc.Stale {
		t.Fatal("expected stale flag on source-error fallback")
	}

	// metrics error + no snapshot → SERVICE_DEGRADED (never fabricated).
	store.lps[1].Scorecard = json.RawMessage(`{}`)
	if _, _, err := svc.Scorecard(context.Background(), AdminActor{UserID: 1}, 1, 0); err == nil {
		t.Fatal("no source + no snapshot must fail closed")
	} else if codeOf(t, err) != "SERVICE_DEGRADED" {
		t.Fatalf("want SERVICE_DEGRADED, got %s", eCode(err))
	}
}

func TestLPScorecardNoDataNoSource(t *testing.T) {
	rm := lpRoles(map[int64]string{1: "Risk Manager"})
	store := newFakeLPStore()
	store.lps[1] = &LiquidityProvider{LPID: 1, Status: LPStatusOnboarding}
	svc := newLPService(store, rm, nil, nil, DefaultLPThresholds())
	if _, _, err := svc.Scorecard(context.Background(), AdminActor{UserID: 1}, 1, 0); err == nil {
		t.Fatal("empty scorecard must not fabricate")
	} else if codeOf(t, err) != "NOT_FOUND" {
		t.Fatalf("want NOT_FOUND, got %s", eCode(err))
	}
}

// ---------------------------------------------------------------------------
// Venue coverage — all LPs down edge case
// ---------------------------------------------------------------------------

func TestLPAllDownVenueAlert(t *testing.T) {
	store := newFakeLPStore()
	store.activeLPs = 0
	sink := &fakeAlertSink{}
	svc := newLPService(store, nil, nil, sink, DefaultLPThresholds())

	ok, err := svc.EvaluateCoverage(context.Background())
	if err != nil {
		t.Fatalf("evaluate coverage: %v", err)
	}
	if ok {
		t.Fatal("zero ACTIVE LPs must report no coverage")
	}
	if len(store.alerts) != 1 || store.alerts[0].Metric != "all_lps_down" {
		t.Fatalf("want persisted all_lps_down alert, got %+v", store.alerts)
	}
	if len(sink.got) != 1 {
		t.Fatal("venue alert must dispatch through the sink")
	}

	// Coverage present → no alert.
	store.activeLPs = 2
	ok, err = svc.EvaluateCoverage(context.Background())
	if err != nil || !ok {
		t.Fatalf("coverage with 2 ACTIVE LPs: ok=%v err=%v", ok, err)
	}
}

// ---------------------------------------------------------------------------
// Update validation
// ---------------------------------------------------------------------------

func TestLPUpdateValidation(t *testing.T) {
	rm := lpRoles(map[int64]string{1: "Risk Manager"})
	store := newFakeLPStore()
	store.lps[1] = &LiquidityProvider{LPID: 1, Status: LPStatusOnboarding}
	svc := newLPService(store, rm, nil, nil, DefaultLPThresholds())
	actor := AdminActor{UserID: 1}

	if _, err := svc.Update(context.Background(), actor, LPUpdate{}); err == nil {
		t.Fatal("missing lp_id must fail")
	}
	if _, err := svc.Update(context.Background(), actor, LPUpdate{LPID: 1}); err == nil {
		t.Fatal("empty update must fail")
	}
	bad := "GONE"
	if _, err := svc.Update(context.Background(), actor, LPUpdate{LPID: 1, Status: &bad}); err == nil {
		t.Fatal("invalid status must fail")
	}
	neg := -5
	if _, err := svc.Update(context.Background(), actor, LPUpdate{LPID: 1, StalenessTimeoutMS: &neg}); err == nil {
		t.Fatal("non-positive staleness must fail")
	}
	// Guarded transition: ONBOARDING→ACTIVE ok.
	act := "ACTIVE"
	lp, err := svc.Update(context.Background(), actor, LPUpdate{LPID: 1, Status: &act})
	if err != nil || lp.Status != LPStatusActive {
		t.Fatalf("ONBOARDING→ACTIVE: lp=%+v err=%v", lp, err)
	}
	// ACTIVE→ONBOARDING must fail at the store guard.
	b := "ONBOARDING"
	if _, err := svc.Update(context.Background(), actor, LPUpdate{LPID: 1, Status: &b}); err == nil {
		t.Fatal("ACTIVE→ONBOARDING must be rejected")
	} else if codeOf(t, err) != "INVALID_LIFECYCLE_TRANSITION" {
		t.Fatalf("want INVALID_LIFECYCLE_TRANSITION, got %s", eCode(err))
	}
}

func eCode(err error) string {
	var e *excerrors.Error
	if errors.As(err, &e) {
		return e.Code
	}
	return "?" + err.Error()
}
