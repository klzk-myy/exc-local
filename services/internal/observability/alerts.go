// alerts.go — operational alert-rule evaluator (Task 7.3.10, spec §2.7,
// §7.3, §19.8, §24 #306).
//
// The evaluator samples metric sources on a tick, evaluates rules
// (threshold-with-duration, counter-delta, rolling-window ratio, EWMA
// anomaly) and pages through the ops.alerts.* seam on pending→firing
// edges. Resolution notices publish on firing→clear edges so the paging
// channel self-clears.
//
// Severity ↔ incident class (spec §19.8): P0 pages on L0 faults
// immediately; P1 on sustained systemic degradation; P2 on error-rate
// spikes and monitoring gaps; P3 is informational (status page / ticket
// queue).
package observability

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// OpsAlertSubject is the monitoring alert dispatch subject — sibling of
// ops.alerts.settlement (Task 3.3.18) and ops.alerts.recovery (§27 #41).
const OpsAlertSubject = "ops.alerts.monitoring"

// Alert severities — spec §19.8 incident classes.
const (
	SeverityP0 = "P0"
	SeverityP1 = "P1"
	SeverityP2 = "P2"
	SeverityP3 = "P3"
)

// Alert is the dispatched ops alert payload (JSON on ops.alerts.monitoring).
type Alert struct {
	Rule     string            `json:"rule"`
	Severity string            `json:"severity"`
	Code     string            `json:"code"`
	Summary  string            `json:"summary"`
	Status   string            `json:"status"` // "firing" | "resolved"
	Details  map[string]string `json:"details,omitempty"`
	FiredAt  string            `json:"fired_at"`
}

// Sink receives fired/resolved alerts.
type Sink interface {
	Raise(ctx context.Context, a Alert) error
}

// Publisher is the NATS publish seam (settlement.Publisher-shaped).
type Publisher interface {
	Publish(ctx context.Context, subject string, payload []byte) error
}

// PublisherSink dispatches alerts as JSON on Subject (default
// OpsAlertSubject). A nil publisher errors — alerting is contractual,
// never best-effort (same contract as settlement.PublisherAlerter).
type PublisherSink struct {
	Pub     Publisher
	Subject string
}

// Raise publishes the alert payload.
func (s PublisherSink) Raise(ctx context.Context, a Alert) error {
	if s.Pub == nil {
		return fmt.Errorf("observability: alert sink: nil publisher")
	}
	subj := s.Subject
	if subj == "" {
		subj = OpsAlertSubject
	}
	payload, err := json.Marshal(a)
	if err != nil {
		return fmt.Errorf("observability: alert sink: marshal: %w", err)
	}
	return s.Pub.Publish(ctx, subj, payload)
}

// LogSink logs alerts — always available fallback so a paging outage
// never goes silent.
type LogSink struct{ Log *slog.Logger }

// Raise logs the alert at error level.
func (s LogSink) Raise(_ context.Context, a Alert) error {
	l := s.Log
	if l == nil {
		l = slog.Default()
	}
	l.Error("ops alert", "rule", a.Rule, "severity", a.Severity,
		"code", a.Code, "status", a.Status, "summary", a.Summary,
		"details", a.Details)
	return nil
}

// FanoutSink raises to every child; the first error is returned but all
// sinks are attempted.
type FanoutSink []Sink

// Raise fans out to all sinks.
func (f FanoutSink) Raise(ctx context.Context, a Alert) error {
	var first error
	for _, s := range f {
		if s == nil {
			continue
		}
		if err := s.Raise(ctx, a); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// ---------------------------------------------------------------------------
// rules
// ---------------------------------------------------------------------------

// Source samples a metric value at eval time (typically a gauge getter,
// counter getter, or monitor method).
type Source func() float64

// samplePoint is one tick's observation.
type samplePoint struct {
	t     time.Time
	value float64
	aux   float64 // denominator for ratio rules
}

// rule is the evaluated unit: Check consumes the trailing window of
// samples and reports firing + a detail string.
type rule struct {
	id        string
	severity  string
	code      string
	summary   string
	forDur    time.Duration // pending period before firing
	windowLen time.Duration // trailing sample retention (0 → 2×Interval)
	check     func(win []samplePoint) (firing bool, detail string)
	sample    func() (value, aux float64)

	mu          sync.Mutex
	window      []samplePoint
	firstBreach time.Time
	firing      bool
}

// Evaluator drives rules on a tick and dispatches edges to the sink.
type Evaluator struct {
	Interval time.Duration
	Now      func() time.Time
	Sink     Sink
	Log      *slog.Logger

	mu    sync.Mutex
	rules []*rule

	metrics       *Metrics // optional: exchange_alert_firing gauge
	dispatchErrs  *CounterVec
	alertFires    *CounterVec
	alertResolves *CounterVec
}

// EvaluatorOption customises the evaluator.
type EvaluatorOption func(*Evaluator)

// WithAlertMetrics wires the firing gauge + dispatch counters into m.
func WithAlertMetrics(m *Metrics) EvaluatorOption {
	return func(e *Evaluator) {
		e.metrics = m
		e.dispatchErrs = m.Reg.Counter("exchange_alert_dispatch_errors_total",
			"Alert dispatch failures (pager channel errors).")
		e.alertFires = m.Reg.Counter("exchange_alerts_fired_total",
			"Alerts dispatched, by severity/rule.")
		e.alertResolves = m.Reg.Counter("exchange_alerts_resolved_total",
			"Alert resolutions dispatched, by severity/rule.")
	}
}

// NewEvaluator builds the evaluator. Interval<=0 defaults to 10s.
func NewEvaluator(sink Sink, log *slog.Logger, opts ...EvaluatorOption) *Evaluator {
	if sink == nil {
		sink = LogSink{}
	}
	if log == nil {
		log = slog.Default()
	}
	e := &Evaluator{
		Interval: 10 * time.Second,
		Now:      time.Now,
		Sink:     sink,
		Log:      log,
	}
	for _, o := range opts {
		o(e)
	}
	return e
}

// AddThreshold registers a value>threshold rule with a pending window
// (forDur=0 → fire on first breach).
func (e *Evaluator) AddThreshold(id, severity, code, summary string,
	forDur time.Duration, src Source, above float64) {
	e.rules = append(e.rules, &rule{
		id: id, severity: severity, code: code, summary: summary,
		forDur: forDur,
		sample: func() (float64, float64) { return src(), 0 },
		check: func(win []samplePoint) (bool, string) {
			v := win[len(win)-1].value
			return v > above, fmt.Sprintf("value %.4g > threshold %.4g", v, above)
		},
	})
}

// AddDelta registers a counter-delta rule: fires while the sampled value
// increased since the previous tick (any new L0 error pages immediately;
// resolution is automatic on the next flat tick).
func (e *Evaluator) AddDelta(id, severity, code, summary string, src Source) {
	e.rules = append(e.rules, &rule{
		id: id, severity: severity, code: code, summary: summary,
		sample: func() (float64, float64) { return src(), 0 },
		check: func(win []samplePoint) (bool, string) {
			if len(win) < 2 {
				return false, ""
			}
			prev, cur := win[len(win)-2].value, win[len(win)-1].value
			d := cur - prev
			return d > 0, fmt.Sprintf("counter delta %.4g since last eval", d)
		},
	})
}

// AddRatioWindow registers a rolling-window ratio rule: fires while
// sum(num)/sum(den) over the trailing window exceeds above, given the
// denominator accumulated at least minDen samples (prevents low-traffic
// false positives).
func (e *Evaluator) AddRatioWindow(id, severity, code, summary string,
	window time.Duration, num, den Source, above, minDen float64) {
	e.rules = append(e.rules, &rule{
		id: id, severity: severity, code: code, summary: summary,
		sample: func() (float64, float64) { return num(), den() },
		check: func(win []samplePoint) (bool, string) {
			// win is pre-trimmed to the trailing window (plus one anchor
			// sample) by EvalOnce — ratios need counter deltas.
			var n, d float64
			for i := 1; i < len(win); i++ {
				n += win[i].value - win[i-1].value
				d += win[i].aux - win[i-1].aux
			}
			if d < minDen || d <= 0 {
				return false, ""
			}
			r := n / d
			return r > above, fmt.Sprintf("ratio %.4f over %d samples (%.4g/%.4g)", r, len(win), n, d)
		},
		windowLen: window,
	})
}

// AddCheck registers a fully custom rule (anomaly detector, composite).
// windowLen controls sample retention.
func (e *Evaluator) AddCheck(id, severity, code, summary string,
	forDur, windowLen time.Duration, src func() (value, aux float64),
	check func(win []samplePoint) (bool, string)) {
	e.rules = append(e.rules, &rule{
		id: id, severity: severity, code: code, summary: summary,
		forDur: forDur, windowLen: windowLen,
		sample: src, check: check,
	})
}

// Firing reports the named rule's current state (tests/health).
func (e *Evaluator) Firing(id string) bool {
	for _, r := range e.rules {
		if r.id == id {
			r.mu.Lock()
			defer r.mu.Unlock()
			return r.firing
		}
	}
	return false
}

// EvalOnce samples every source and evaluates all rules against a single
// now — exported for deterministic tests.
func (e *Evaluator) EvalOnce(now time.Time) {
	for _, r := range e.rules {
		v, aux := r.sample()
		r.mu.Lock()
		r.window = append(r.window, samplePoint{t: now, value: v, aux: aux})
		// Trim to the trailing retention window (forDur, windowLen, or
		// 2×Interval for delta checks), keeping one anchor sample before
		// the cutoff so delta/ratio rules keep a baseline.
		retain := r.windowLen
		if r.forDur > retain {
			retain = r.forDur
		}
		if retain <= 0 {
			retain = 2 * e.Interval
		}
		cutoff := now.Add(-retain)
		i := 0
		for i < len(r.window) && r.window[i].t.Before(cutoff) {
			i++
		}
		if i > 0 {
			i-- // anchor sample just before the window
		}
		if i > 0 {
			r.window = append([]samplePoint(nil), r.window[i:]...)
		}
		firing, detail := r.check(r.window)
		var alert *Alert
		switch {
		case firing && !r.firing:
			if r.forDur <= 0 {
				r.firing = true
				alert = &Alert{Rule: r.id, Severity: r.severity, Code: r.code,
					Summary: r.summary, Status: "firing",
					Details: map[string]string{"detail": detail},
					FiredAt: now.UTC().Format(time.RFC3339Nano)}
			} else {
				if r.firstBreach.IsZero() {
					r.firstBreach = now
				} else if now.Sub(r.firstBreach) >= r.forDur {
					r.firing = true
					alert = &Alert{Rule: r.id, Severity: r.severity, Code: r.code,
						Summary: r.summary, Status: "firing",
						Details: map[string]string{
							"detail":    detail,
							"sustained": now.Sub(r.firstBreach).String(),
						},
						FiredAt: now.UTC().Format(time.RFC3339Nano)}
				}
			}
		case firing && r.firing:
			// still firing
		case !firing:
			r.firstBreach = time.Time{}
			if r.firing {
				r.firing = false
				alert = &Alert{Rule: r.id, Severity: r.severity, Code: r.code,
					Summary: r.summary, Status: "resolved",
					FiredAt: now.UTC().Format(time.RFC3339Nano)}
			}
		}
		r.mu.Unlock()
		if alert != nil {
			e.dispatch(*alert)
		}
	}
}

func (e *Evaluator) dispatch(a Alert) {
	if e.metrics != nil {
		e.metrics.SetAlertFiring(a.Rule, a.Severity, a.Status == "firing")
		if a.Status == "firing" {
			e.alertFires.With("severity", a.Severity, "rule", a.Rule).Inc()
		} else {
			e.alertResolves.With("severity", a.Severity, "rule", a.Rule).Inc()
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := e.Sink.Raise(ctx, a); err != nil {
		if e.dispatchErrs != nil {
			e.dispatchErrs.With("severity", a.Severity, "rule", a.Rule).Inc()
		}
		e.Log.Error("alert dispatch failed", "rule", a.Rule,
			"severity", a.Severity, "err", err)
	}
}

// Run evaluates on Interval until ctx is cancelled.
func (e *Evaluator) Run(ctx context.Context) {
	t := time.NewTicker(e.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			e.EvalOnce(now)
		}
	}
}
