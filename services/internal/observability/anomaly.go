// anomaly.go — rolling-baseline error-rate anomaly detection
// (Task 7.3.10 item 4, spec §2.7/§24 #306).
//
// Threshold alerts catch known-bad levels; anomaly alarms catch the
// unknown-bad: a per-tick error rate that is statistically unusual for
// THIS service, even when below the absolute threshold. The detector is
// an EWMA baseline over windowed error rates with a variance band —
// alerting when the observed rate exceeds max(floor, baseline + k·σ)
// after enough samples exist to make the baseline meaningful.
package observability

import (
	"fmt"
	"math"
	"time"
)

// AnomalyConfig tunes the detector.
type AnomalyConfig struct {
	// Alpha is the EWMA weight for the newest sample (0,1]. Higher adapts
	// faster but alarms more on regime changes. Default 0.2.
	Alpha float64
	// Floor is the minimum error rate that can ever alarm — guards
	// against zero-variance baselines alarming on a single error.
	// Default 0.001 (0.1%).
	Floor float64
	// SigmaMultiplier k: alarm when rate > mean + k·σ. Default 4.
	SigmaMultiplier float64
	// MinSamples is the minimum number of denominator events in the
	// window before the detector arms (default 100) — low-traffic windows
	// never alarm.
	MinSamples float64
	// Severity for the fired alert (default P2).
	Severity string
}

func (c AnomalyConfig) withDefaults() AnomalyConfig {
	if c.Alpha <= 0 || c.Alpha > 1 {
		c.Alpha = 0.2
	}
	if c.Floor <= 0 {
		c.Floor = 0.001
	}
	if c.SigmaMultiplier <= 0 {
		c.SigmaMultiplier = 4
	}
	if c.MinSamples <= 0 {
		c.MinSamples = 100
	}
	if c.Severity == "" {
		c.Severity = SeverityP2
	}
	return c
}

// RateAnomaly maintains the rolling baseline for one (service, metric)
// pair. Observe is called once per eval tick with the window's error and
// event counts.
type RateAnomaly struct {
	cfg AnomalyConfig

	mean    float64 // EWMA of error rate
	m2      float64 // EWMA of squared deviation (variance estimate)
	samples int     // windows observed
	armed   bool    // baseline initialized
}

// NewRateAnomaly builds the detector with defaults applied.
func NewRateAnomaly(cfg AnomalyConfig) *RateAnomaly {
	return &RateAnomaly{cfg: cfg.withDefaults()}
}

// Observe records one window's error rate and reports whether it is
// anomalous. errors/total are window counts (not cumulative) — callers
// using cumulative counters must pass deltas.
//
// The baseline updates only on non-anomalous samples so a sustained
// incident doesn't teach the detector that elevated is normal; when the
// rate returns to baseline the alarm auto-resolves.
func (a *RateAnomaly) Observe(errors, total float64) (anomalous bool, detail string) {
	if total < a.cfg.MinSamples || total <= 0 {
		return false, "" // insufficient traffic — never alarm
	}
	rate := errors / total
	a.samples++
	if !a.armed {
		a.armed = true
		a.mean = rate
		return false, ""
	}
	limit := math.Max(a.cfg.Floor, a.mean+a.cfg.SigmaMultiplier*math.Sqrt(a.m2))
	if rate > limit {
		return true, fmt.Sprintf(
			"error rate %.4f exceeds baseline %.4f + %.1fσ (limit %.4f, %d windows)",
			rate, a.mean, a.cfg.SigmaMultiplier, limit, a.samples)
	}
	// fold the healthy sample into the baseline
	delta := rate - a.mean
	a.mean += a.cfg.Alpha * delta
	a.m2 = (1 - a.cfg.Alpha) * (a.m2 + a.cfg.Alpha*delta*delta)
	return false, ""
}

// Rate returns the current baseline mean — exposed for dashboards.
func (a *RateAnomaly) Rate() float64 { return a.mean }

// AddErrorRateAnomaly wires a per-tick anomaly rule onto the evaluator:
// each EvalOnce samples the cumulative (errors, total) sources, computes
// the window deltas since the previous tick, feeds the detector, and
// fires the rule while the window is anomalous.
func (e *Evaluator) AddErrorRateAnomaly(id, code, summary string,
	cfg AnomalyConfig, errors, total Source) {
	det := NewRateAnomaly(cfg)
	var prevErr, prevTot float64
	var started bool
	e.AddCheck(id, cfg.withDefaults().Severity, code, summary,
		0, 2*e.Interval,
		func() (float64, float64) { return errors(), total() },
		func(win []samplePoint) (bool, string) {
			cur := win[len(win)-1]
			if !started {
				started = true
				prevErr, prevTot = cur.value, cur.aux
				return false, ""
			}
			dErr := cur.value - prevErr
			dTot := cur.aux - prevTot
			prevErr, prevTot = cur.value, cur.aux
			if dErr < 0 {
				dErr = 0 // counter reset (restart) — clamp
			}
			return det.Observe(dErr, dTot)
		})
}

// DefaultWindow returns the anomaly evaluation window used by the admin
// service wiring (documented for dashboards).
func DefaultWindow() time.Duration { return time.Minute }
