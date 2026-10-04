// Phase-14 Task 14.3.2 — auto-halt on anomaly (spec §2.6/§2.7 machinery;
// §24 task acceptance: detect → suspend 5min → P1 + user notify →
// auto-resume if cleared).
//
// Auto-halt is the DETECTOR layer on top of the five-tier circuit
// breaker — it is deliberately NOT a parallel halt stack. Every
// detection lands as a trip on the canonical breaker machinery
// (Redis HASH state, circuit_breaker_events audit, probe-window
// recovery, flap doubling, CIRCUIT_BREAKER_OPEN admission gate), then
// this service performs the task-specific duties on the transition:
//
//  1. P1 ops alert through the ops-alerts seam (same FlapAlerter-shaped
//     binding the breaker's flap pager uses — ops.alerts.* in prod).
//  2. User notification (notifications.EventTradingHalt, a critical
//     event bypassing quiet hours) to every user holding an open
//     position or working order on the instrument — UserLookup seam.
//  3. auto_halt_events audit row (migration 217) with the detector
//     readings, alert/notify fan-out counts, and the later RESUMED row.
//
// Detector inventory and thresholds (explicit — never re-tuned silently):
//
//	PRICE_SPIKE       delegated to the breaker's canonical INSTRUMENT
//	                  trigger: >instrument_price_limit (def 5%) peak-to-
//	                  trough move / 60s. Auto-halt observes the result —
//	                  the 5-minute INSTRUMENT hold IS the task's
//	                  "suspended for 5min".
//	VOLUME_SPIKE      delegated to the breaker's canonical VOLUME_SPIKE
//	                  trigger: 1-min notional z-score >= 4σ vs trailing
//	                  1h baseline (10min hold — the spec §2.6 hold for
//	                  the volume scope; admission on the instrument is
//	                  suspended the same way).
//	LATENCY_SPIKE     per-symbol rolling 60s window of order-admission
//	                  latencies: mean > 250ms over >=20 samples. Feed:
//	                  orders.Service admission duration (see
//	                  ObserveAdmission). An engine ack pipeline feeding
//	                  finer-grained RTT may bind the same seam.
//	ERROR_RATE_SPIKE  per-symbol rolling 60s window of order-admission
//	                  outcomes: systemic failures (engine timeout,
//	                  INTERNAL_ERROR, SERVICE_DEGRADED — see
//	                  SystemicAdmissionCode) >= 25% over >=20 admissions.
//	                  Client-correctable rejections (validation, margin,
//	                  idempotency) count to the denominator only — a
//	                  retail fat-finger burst cannot halt an instrument.
//
// Auto-resume: the breaker's OPEN → HALF_OPEN(probe) → CLOSED machinery
// owns resume. "Only if anomaly cleared" is enforced two ways: fresh
// breaching observations during HALF_OPEN re-trip immediately (the
// sweeper also re-evaluates the detector windows each tick), and an
// empty/failed probe window re-OPENs per §2.6 — a telemetry outage can
// never complete a recovery (fail closed). Feed-source errors are
// logged, never fabricated into trips (same rule as PollIV).
package risk

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"exchange/internal/config"
	"exchange/internal/notifications"
	"exchange/internal/observability"
	excredis "exchange/internal/redis"
	"exchange/pkg/decimal"
	excerrors "exchange/pkg/errors"
)

// Detector tokens — migration 217 auto_halt_events.detector CHECK.
const (
	DetectorPriceSpike     = "PRICE_SPIKE"
	DetectorVolumeSpike    = "VOLUME_SPIKE"
	DetectorLatencySpike   = "LATENCY_SPIKE"
	DetectorErrorRateSpike = "ERROR_RATE_SPIKE"
)

// Audit actions — migration 217 auto_halt_events.action CHECK.
const (
	AutoHaltActionHalted  = "HALTED"
	AutoHaltActionResumed = "RESUMED"
)

// AutoHaltAlertCode is the ops-alert code paged on an anomaly-driven
// suspension (dispatch travels the ops.alerts.* seam like
// CIRCUIT_BREAKER_FLAPPING).
const AutoHaltAlertCode = "AUTO_HALT_ANOMALY"

// Detector thresholds for the two detectors this task owns (price/volume
// thresholds are spec §2.6 canonical and live in circuit_breaker.go).
// Exported via AutoHaltConfig for ops tuning; defaults are the initial
// operating points recorded here:
//
//	LatencySpikeMean    250ms sustained mean admission latency is ~5x the
//	                    healthy gateway admission budget — an engine/IPC
//	                    stall signature, not client jitter.
//	ErrorRateSpikePct   25% of admissions failing venue-side.
//	MinSamples          20 observations before a ratio/mean can judge —
//	                    below that the window reports insufficient data,
//	                    never a trip (same honesty rule as the volume
//	                    z-score's 30-bucket floor).
const (
	AutoHaltWindow          = 60 * time.Second
	AutoHaltMinSamples      = 20
	defaultLatencySpikeMean = 250 * time.Millisecond
	defaultErrorRatePct     = 25.0
)

// AutoHaltConfig tunes the detector thresholds. Zero fields take the
// defaults above.
type AutoHaltConfig struct {
	Window           time.Duration // rolling window for latency/error detectors
	MinSamples       int           // minimum samples before evaluation
	LatencySpikeMean time.Duration // sustained mean latency that trips
	ErrorRatePct     float64       // systemic-error share (percent) that trips
}

func (c AutoHaltConfig) normalize() AutoHaltConfig {
	if c.Window <= 0 {
		c.Window = AutoHaltWindow
	}
	if c.MinSamples <= 0 {
		c.MinSamples = AutoHaltMinSamples
	}
	if c.LatencySpikeMean <= 0 {
		c.LatencySpikeMean = defaultLatencySpikeMean
	}
	if c.ErrorRatePct <= 0 {
		c.ErrorRatePct = defaultErrorRatePct
	}
	return c
}

// AutoHaltEvent is one audit row (auto_halt_events, migration 217).
type AutoHaltEvent struct {
	Symbol       string            `json:"symbol"`
	Scope        string            `json:"scope"`
	Detector     string            `json:"detector"`
	Action       string            `json:"action"` // HALTED | RESUMED
	Observed     map[string]string `json:"observed,omitempty"`
	BreakerState string            `json:"breaker_state"`
	AlertSent    bool              `json:"alert_sent"`
	Notified     int               `json:"notified"`
	At           time.Time         `json:"at"`
}

// AutoHaltEventStore persists detector audit rows. A nil store skips
// durable audit — the halt itself is never blocked by telemetry
// (breaker events already land in circuit_breaker_events).
type AutoHaltEventStore interface {
	InsertAutoHaltEvent(ctx context.Context, ev AutoHaltEvent) error
}

// UserLookup resolves the user IDs to notify for a halted instrument:
// production binds the PG query over positions ∪ working orders →
// accounts.user_id (wired in cmd/gateway). Nil disables notifications.
type UserLookup func(ctx context.Context, symbol string) ([]int64, error)

// Notifier is the user-notification seam — *notifications.Service
// satisfies it; nil disables the user half of the alert (ops page
// still fires).
type Notifier interface {
	Notify(ctx context.Context, userID int64, event string, payload map[string]any) (int, error)
}

// AutoHaltMetrics carries the detector metric families.
type AutoHaltMetrics struct {
	events *observability.CounterVec
	halted *observability.GaugeVec
}

// NewAutoHaltMetrics registers the families on reg; nil reg → no-op.
func NewAutoHaltMetrics(reg *observability.Registry) *AutoHaltMetrics {
	if reg == nil {
		return &AutoHaltMetrics{}
	}
	return &AutoHaltMetrics{
		events: reg.Counter("auto_halt_events_total",
			"Auto-halt detector events by detector and action (Task 14.3.2)"),
		halted: reg.Gauge("auto_halt_halted",
			"Instruments currently suspended by an anomaly episode (1=halted), by scope"),
	}
}

func (m *AutoHaltMetrics) emit(detector, action string) {
	if m == nil || m.events == nil {
		return
	}
	m.events.With("detector", detector, "action", action).Inc()
}

func (m *AutoHaltMetrics) setHalted(scope, sym string, on bool) {
	if m == nil || m.halted == nil {
		return
	}
	v := 0.0
	if on {
		v = 1
	}
	m.halted.With("scope", scope, "id", sym).Set(v)
}

// ---------------------------------------------------------------------------
// Service
// ---------------------------------------------------------------------------

type latSample struct {
	t time.Time
	d time.Duration
}

type admSample struct {
	t        time.Time
	systemic bool
}

// haltEpisode tracks one attributed suspension: the detector that first
// fired and whether the P1 alert was dispatched.
type haltEpisode struct {
	detector string
	firedAt  time.Time
}

// AutoHaltService is the anomaly-detection + notification layer bound to
// a CircuitBreakerService. Construct via NewAutoHaltService; drive feeds
// (ObservePrice/ObserveTrade/ObserveAdmission/ObserveLatency); run the
// reconcile sweeper via Run.
type AutoHaltService struct {
	cb     *CircuitBreakerService
	events AutoHaltEventStore
	alert  FlapAlerter
	notify Notifier
	users  UserLookup
	met    *AutoHaltMetrics
	now    func() time.Time
	logf   func(format string, args ...any)
	cfg    AutoHaltConfig

	mu     sync.Mutex
	halted map[string]*haltEpisode // "SCOPE|sym" → attributed episode
	lat    map[string]*latWindow   // symbol → rolling admission latencies
	adm    map[string]*admWindow   // symbol → rolling admission outcomes
}

// AutoHaltDeps wires the service. CB is required — auto-halt has no halt
// path of its own. Everything else degrades honestly (nil → logged
// skip), mirroring BreakerDeps.
type AutoHaltDeps struct {
	CB       *CircuitBreakerService
	Events   AutoHaltEventStore
	Metrics  *AutoHaltMetrics
	Alerter  FlapAlerter // same ops-alerts binding as the flap pager
	Notifier Notifier
	Users    UserLookup
	Config   AutoHaltConfig
	Now      func() time.Time
	Logf     func(format string, args ...any)
}

// NewAutoHaltService builds the detector service.
func NewAutoHaltService(d AutoHaltDeps) (*AutoHaltService, error) {
	if d.CB == nil {
		return nil, fmt.Errorf("auto-halt: circuit breaker service is nil")
	}
	s := &AutoHaltService{
		cb: d.CB, events: d.Events, alert: d.Alerter,
		notify: d.Notifier, users: d.Users, met: d.Metrics,
		now: d.Now, logf: d.Logf, cfg: d.Config.normalize(),
		halted: map[string]*haltEpisode{},
		lat:    map[string]*latWindow{},
		adm:    map[string]*admWindow{},
	}
	if s.now == nil {
		s.now = func() time.Time { return time.Now().UTC() }
	}
	if s.logf == nil {
		s.logf = func(string, ...any) {}
	}
	return s, nil
}

// ---------------------------------------------------------------------------
// CircuitBreakerService extensions (auto-halt entries — same package, no
// changes to circuit_breaker.go needed)
// ---------------------------------------------------------------------------

// BreakerFor returns a copy of the live record for (scope,id) after
// advancing expired states; nil,nil when no breaker exists. Read errors
// propagate — callers fail closed (an unverifiable halt state is itself
// an anomaly, never silently "closed").
func (s *CircuitBreakerService) BreakerFor(ctx context.Context, scope, id string) (*Breaker, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.advanceLocked(ctx, s.now()); err != nil {
		s.logf("circuit-breaker: advance failed: %v", err)
	}
	b, err := s.getLocked(ctx, scope, id)
	if err != nil {
		return nil, err
	}
	if b == nil {
		return nil, nil
	}
	cp := *b
	return &cp, nil
}

// TripInstrumentAnomaly trips the INSTRUMENT breaker for symbol through
// the automated feed path — the 5-minute scope hold, flap doubling and
// probe-window recovery all apply unchanged. Used by the latency and
// error-rate anomaly detectors (price/volume trip via their own
// canonical feeds inside ObservePrice/ObserveTrade).
func (s *CircuitBreakerService) TripInstrumentAnomaly(ctx context.Context,
	symbol, reason string, trig map[string]string) error {

	sym := config.CanonicalSymbol(symbol)
	if sym == "" {
		return excerrors.New("INVALID_REQUEST", "auto-halt: symbol required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.tripByFeed(ctx, ScopeInstrument, sym, reason, trig)
}

// SystemicAdmissionCode classifies an order-admission rejection code for
// the ERROR_RATE_SPIKE numerator: only venue-side failures (engine ack
// timeout, uncoded/INTERNAL_ERROR, SERVICE_DEGRADED) are anomalies.
// Deliberate risk gates (CIRCUIT_BREAKER_OPEN, TRADING_HALTED,
// OTR_LIMIT_EXCEEDED) and client-correctable rejections are excluded —
// they are the system working as designed, and counting them would let
// a halted instrument re-trip itself on its own rejections.
func SystemicAdmissionCode(code string) bool {
	switch code {
	case "GATEWAY_TIMEOUT_MATCHING_ENGINE", "INTERNAL_ERROR", "SERVICE_DEGRADED":
		return true
	}
	return false
}

// ---------------------------------------------------------------------------
// Feeds
// ---------------------------------------------------------------------------

// ObservePrice forwards a trade/reference price to the breaker's
// canonical INSTRUMENT move detector, then reconciles the halt episode
// (alert/notify/audit on a fresh OPEN, RESUMED on CLOSED). The breaker
// keeps sole authority over the 5%/60s threshold — auto-halt never
// re-derives it.
func (a *AutoHaltService) ObservePrice(ctx context.Context, symbol string, price decimal.Decimal) {
	a.cb.ObservePrice(ctx, symbol, price)
	sym := config.CanonicalSymbol(symbol)
	if sym == "" {
		return
	}
	a.reconcile(ctx, ScopeInstrument, sym, DetectorPriceSpike,
		map[string]string{"last_price": price.String()})
}

// ObserveTrade forwards fill notional to the breaker's VOLUME_SPIKE
// z-score detector, then reconciles. The volume scope carries its
// canonical 10-minute hold — the §2.6 suspension for a volume anomaly.
func (a *AutoHaltService) ObserveTrade(ctx context.Context, symbol string, notional decimal.Decimal) {
	a.cb.ObserveTrade(ctx, symbol, notional)
	sym := config.CanonicalSymbol(symbol)
	if sym == "" {
		return
	}
	a.reconcile(ctx, ScopeVolumeSpike, sym, DetectorVolumeSpike,
		map[string]string{"notional": notional.String()})
}

// ObserveLatency feeds one per-symbol admission-latency observation into
// the LATENCY_SPIKE detector. A breach trips the INSTRUMENT breaker
// (5-minute hold). Feed source: order admission duration measured inside
// orders.Service.Submit — engine/IPC stalls surface here before fills do.
func (a *AutoHaltService) ObserveLatency(ctx context.Context, symbol string, d time.Duration) {
	sym := config.CanonicalSymbol(symbol)
	if sym == "" || d <= 0 {
		return
	}
	now := a.now()
	a.mu.Lock()
	w := a.lat[sym]
	if w == nil {
		w = &latWindow{}
		a.lat[sym] = w
	}
	w.push(latSample{t: now, d: d})
	w.prune(now.Add(-a.cfg.Window))
	mean, n := w.mean()
	a.mu.Unlock()
	var trig map[string]string
	if n >= a.cfg.MinSamples && mean > a.cfg.LatencySpikeMean {
		reason := fmt.Sprintf("latency spike: %d-sample mean %s > %s within %s",
			n, mean.Round(time.Microsecond), a.cfg.LatencySpikeMean, a.cfg.Window)
		trig = map[string]string{
			"detector":     DetectorLatencySpike,
			"mean_latency": mean.Round(time.Microsecond).String(),
			"threshold":    a.cfg.LatencySpikeMean.String(),
			"samples":      strconv.Itoa(n),
			"window":       a.cfg.Window.String(),
		}
		if err := a.cb.TripInstrumentAnomaly(ctx, sym, reason, trig); err != nil {
			a.logf("auto-halt: latency trip failed %s: %v", sym, err)
		}
	}
	// Reconcile on EVERY observation — cleared feeds land RESUMED rows,
	// fresh breaches land HALTED, and externally-tripped breakers
	// (canonical feeds, admin) still attribute correctly.
	a.reconcile(ctx, ScopeInstrument, sym, DetectorLatencySpike, trig)
}

// ObserveAdmission feeds one order-admission outcome: the latency sample
// plus the systemic-error classification (SystemicAdmissionCode applied
// at the call site). This is the production feed for both the
// LATENCY_SPIKE and ERROR_RATE_SPIKE detectors — orders.Service calls it
// once per Submit verdict.
func (a *AutoHaltService) ObserveAdmission(ctx context.Context, symbol string,
	latency time.Duration, systemic bool) {

	sym := config.CanonicalSymbol(symbol)
	if sym == "" {
		return
	}
	now := a.now()
	a.mu.Lock()
	w := a.adm[sym]
	if w == nil {
		w = &admWindow{}
		a.adm[sym] = w
	}
	w.push(admSample{t: now, systemic: systemic})
	w.prune(now.Add(-a.cfg.Window))
	total, bad := w.count()
	a.mu.Unlock()

	// Latency leg shares the admission observation (it reconciles
	// internally on every call, so cleared feeds still land RESUMED).
	if latency > 0 {
		a.ObserveLatency(ctx, sym, latency)
	}
	var trig map[string]string
	if total >= a.cfg.MinSamples {
		if pct := float64(bad) / float64(total) * 100; pct >= a.cfg.ErrorRatePct {
			reason := fmt.Sprintf("error-rate spike: %.1f%% systemic failures (%d/%d) >= %.0f%% within %s",
				pct, bad, total, a.cfg.ErrorRatePct, a.cfg.Window)
			trig = map[string]string{
				"detector":      DetectorErrorRateSpike,
				"error_pct":     strconv.FormatFloat(pct, 'f', 2, 64),
				"threshold_pct": strconv.FormatFloat(a.cfg.ErrorRatePct, 'f', 2, 64),
				"systemic":      strconv.Itoa(bad),
				"samples":       strconv.Itoa(total),
				"window":        a.cfg.Window.String(),
			}
			if err := a.cb.TripInstrumentAnomaly(ctx, sym, reason, trig); err != nil {
				a.logf("auto-halt: error-rate trip failed %s: %v", sym, err)
			}
		}
	}
	a.reconcile(ctx, ScopeInstrument, sym, DetectorErrorRateSpike, trig)
}

// ---------------------------------------------------------------------------
// Reconcile — episode attribution, alert, notify, audit
// ---------------------------------------------------------------------------

// reconcile evaluates the breaker state for (scope,sym) against the
// attributed episode map and performs the transition duties. Called on
// every feed and by the sweeper; errors are logged (a telemetry failure
// must not mask the halt itself, and the breaker already gates orders
// fail-closed independent of this layer).
func (a *AutoHaltService) reconcile(ctx context.Context, scope, sym,
	detector string, observed map[string]string) {

	key := scope + "|" + sym
	b, err := a.cb.BreakerFor(ctx, scope, sym)
	if err != nil {
		a.logf("auto-halt: breaker read %s failed (fail closed — no resume decisions made): %v", key, err)
		return
	}
	now := a.now()
	switch {
	case b == nil || b.State == excredis.CircuitClosed:
		a.mu.Lock()
		_, was := a.halted[key]
		if was {
			delete(a.halted, key)
		}
		a.mu.Unlock()
		if was {
			a.met.setHalted(scope, sym, false)
			a.audit(ctx, AutoHaltEvent{
				Symbol: sym, Scope: scope, Detector: detector,
				Action: AutoHaltActionResumed, Observed: observed,
				BreakerState: excredis.CircuitClosed, At: now,
			})
		}
	case b.State == excredis.CircuitOpen && !b.Manual:
		a.mu.Lock()
		_, was := a.halted[key]
		if !was {
			a.halted[key] = &haltEpisode{detector: detector, firedAt: now}
		}
		a.mu.Unlock()
		if was {
			return // same episode — no duplicate page/audit
		}
		a.met.setHalted(scope, sym, true)
		a.emitHalted(ctx, b, detector, observed)
	case b.State == excredis.CircuitOpen && b.Manual:
		// Operator-forced halt — not an anomaly episode; drop any
		// attribution so the post-manual resume doesn't emit a stale row.
		a.mu.Lock()
		delete(a.halted, key)
		a.mu.Unlock()
		a.met.setHalted(scope, sym, false)
	case b.State == excredis.CircuitHalfOpen:
		// Probe window: still attributed. If the anomaly detector still
		// breaches, re-trip now rather than letting 10 probes close the
		// episode over live bad telemetry (auto-resume only-if-cleared).
		a.mu.Lock()
		ep, was := a.halted[key]
		a.mu.Unlock()
		if was && a.detectorBreaching(sym, ep.detector) {
			if terr := a.cb.TripInstrumentAnomaly(ctx, sym,
				"anomaly persists through probe window — re-halted",
				map[string]string{"detector": ep.detector, "re_trip": "1"}); terr != nil {
				a.logf("auto-halt: probe-window re-trip failed %s: %v", key, terr)
			}
		}
		if !was {
			// Post-restart hydration: attribute the in-flight episode so
			// the eventual CLOSED lands a RESUMED row.
			a.mu.Lock()
			a.halted[key] = &haltEpisode{detector: detector, firedAt: now}
			a.mu.Unlock()
			a.met.setHalted(scope, sym, true)
		}
	}
}

// emitHalted performs the fresh-halt duties: P1 page, user notifications,
// audit row. None of these may veto the halt — failures are logged and
// recorded (alert_sent=false) rather than retried into the order path.
func (a *AutoHaltService) emitHalted(ctx context.Context, b *Breaker,
	detector string, observed map[string]string) {

	summary := fmt.Sprintf(
		"auto-halt: %s suspended ~%dmin — %s anomaly (%s)",
		b.ID, b.HoldMS/60000, detector, b.Reason)
	a.logf("%s", summary)

	alertSent := false
	if a.alert != nil {
		if err := a.alert(ctx, "P1", AutoHaltAlertCode, summary); err != nil {
			a.logf("auto-halt: P1 alert dispatch failed for %s: %v", b.ID, err)
		} else {
			alertSent = true
		}
	}

	notified := 0
	if a.users != nil && a.notify != nil {
		uids, err := a.users(ctx, b.ID)
		if err != nil {
			a.logf("auto-halt: user lookup failed for %s: %v", b.ID, err)
		}
		for _, uid := range uids {
			payload := map[string]any{
				"symbol":         b.ID,
				"detector":       detector,
				"reason":         b.Reason,
				"hold_minutes":   b.HoldMS / 60000,
				"suspended_from": b.EnteredAt.UTC().Format(time.RFC3339),
			}
			n, nerr := a.notify.Notify(ctx, uid, notifications.EventTradingHalt, payload)
			if nerr != nil {
				a.logf("auto-halt: notify user %d failed for %s: %v", uid, b.ID, nerr)
				continue
			}
			notified += n
		}
	}

	a.audit(ctx, AutoHaltEvent{
		Symbol: b.ID, Scope: b.Scope, Detector: detector,
		Action: AutoHaltActionHalted, Observed: observed,
		BreakerState: b.State, AlertSent: alertSent,
		Notified: notified, At: a.now(),
	})
}

// audit appends one auto_halt_events row; insert failures are logged,
// never fatal (mirrors the breaker event-store contract).
func (a *AutoHaltService) audit(ctx context.Context, ev AutoHaltEvent) {
	a.met.emit(ev.Detector, ev.Action)
	if a.events == nil {
		return
	}
	if err := a.events.InsertAutoHaltEvent(ctx, ev); err != nil {
		a.logf("auto-halt: event audit failed (%s %s %s): %v",
			ev.Symbol, ev.Detector, ev.Action, err)
	}
}

// detectorBreaching re-evaluates the attributed detector's own window —
// used during HALF_OPEN to veto auto-resume while telemetry still
// breaches. PRICE_SPIKE/VOLUME_SPIKE delegate to the breaker's feeds
// (fresh ticks re-trip on their own), so only the windows this service
// owns are checked here; a detector with an empty/expired window is
// "cleared".
func (a *AutoHaltService) detectorBreaching(sym, detector string) bool {
	now := a.now()
	a.mu.Lock()
	defer a.mu.Unlock()
	switch detector {
	case DetectorLatencySpike:
		w := a.lat[sym]
		if w == nil {
			return false
		}
		w.prune(now.Add(-a.cfg.Window))
		mean, n := w.mean()
		return n >= a.cfg.MinSamples && mean > a.cfg.LatencySpikeMean
	case DetectorErrorRateSpike:
		w := a.adm[sym]
		if w == nil {
			return false
		}
		w.prune(now.Add(-a.cfg.Window))
		total, bad := w.count()
		return total >= a.cfg.MinSamples &&
			float64(bad)/float64(total)*100 >= a.cfg.ErrorRatePct
	}
	return false
}

// Halted returns the attributed open episodes (ops surface/tests).
func (a *AutoHaltService) Halted() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]string, 0, len(a.halted))
	for k := range a.halted {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Run is the reconcile sweeper: every tick it re-evaluates attributed
// episodes — landing RESUMED rows when breakers close without fresh
// feeds, and re-tripping HALF_OPEN breakers whose detector still
// breaches. interval <= 0 defaults to 2s (probe windows are 30s).
func (a *AutoHaltService) Run(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = 2 * time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			a.sweep(ctx)
		}
	}
}

func (a *AutoHaltService) sweep(ctx context.Context) {
	a.mu.Lock()
	keys := make([]string, 0, len(a.halted))
	for k := range a.halted {
		keys = append(keys, k)
	}
	a.mu.Unlock()
	for _, k := range keys {
		scope, sym, ok := strings.Cut(k, "|")
		if !ok {
			continue
		}
		a.mu.Lock()
		ep := a.halted[k]
		a.mu.Unlock()
		detector := DetectorErrorRateSpike
		if ep != nil {
			detector = ep.detector
		}
		a.reconcile(ctx, scope, sym, detector, nil)
	}
}

// ---------------------------------------------------------------------------
// Window helpers
// ---------------------------------------------------------------------------

// sampleWindow is an amortized O(1) sliding window over time-ordered
// samples: push appends; prune advances a head offset and returns the
// evicted samples so callers can decrement running aggregates (the
// previous full-window copies and per-observation mean/count scans
// were the gateway's dominant allocation+CPU source under load).
// Compaction shifts only once the dead prefix exceeds half the buffer.
type sampleWindow[T any] struct {
	buf  []T
	head int
}

func (w *sampleWindow[T]) push(s T) { w.buf = append(w.buf, s) }
func (w *sampleWindow[T]) len() int { return len(w.buf) - w.head }

// prune drops samples older than cut (ts extracts the timestamp),
// returns the evicted prefix for aggregate adjustment, and compacts
// amortized — O(evicted) typical, O(live) only on compaction.
func (w *sampleWindow[T]) prune(cut time.Time, ts func(T) time.Time) []T {
	start := w.head
	for w.head < len(w.buf) && ts(w.buf[w.head]).Before(cut) {
		w.head++
	}
	evicted := w.buf[start:w.head]
	if w.head >= 64 && w.head*2 >= len(w.buf) {
		w.buf = append(w.buf[:0], w.buf[w.head:]...)
		w.head = 0
	}
	return evicted
}

// latWindow keeps a running duration sum alongside the sample ring so
// LATENCY_SPIKE's mean is O(1) per observation instead of scanning the
// window on every submit.
type latWindow struct {
	w   sampleWindow[latSample]
	sum time.Duration
}

func (l *latWindow) push(s latSample) { l.w.push(s); l.sum += s.d }

func (l *latWindow) prune(cut time.Time) {
	for _, e := range l.w.prune(cut, func(s latSample) time.Time { return s.t }) {
		l.sum -= e.d
	}
}

func (l *latWindow) mean() (time.Duration, int) {
	n := l.w.len()
	if n == 0 {
		return 0, 0
	}
	return l.sum / time.Duration(n), n
}

// admWindow keeps a running systemic-failure count so ERROR_RATE_SPIKE's
// ratio is O(1) per observation.
type admWindow struct {
	w        sampleWindow[admSample]
	systemic int
}

func (w *admWindow) push(s admSample) {
	w.w.push(s)
	if s.systemic {
		w.systemic++
	}
}

func (w *admWindow) prune(cut time.Time) {
	for _, e := range w.w.prune(cut, func(s admSample) time.Time { return s.t }) {
		if e.systemic {
			w.systemic--
		}
	}
}

func (w *admWindow) count() (total, systemic int) {
	return w.w.len(), w.systemic
}
