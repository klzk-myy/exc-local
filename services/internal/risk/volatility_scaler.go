// volatility_scaler.go — rolling realized-volatility initial-margin
// scaler (Phase-19 Task 19.3.28; spec §13.6e surface, §24 #412).
//
// The VolatilityScaler computes a rolling 1-hour realized volatility
// per instrument from the tick feed (TickVolatilitySource — a narrow
// seam; production binds the ClickHouse/Redis mid-price history). When
// realized vol exceeds the instrument baseline by more than 2× the
// initial-margin requirement is scaled up, capped at ×1.5:
//
//	ratio = realized_vol / baseline
//	mult  = clamp(ratio / triggerRatio, 1.0, maxMultiplier)
//	         (ratio 2.0 → ×1.0, ratio ≥ 3.0 → ×1.5 cap)
//
// MarginService.evaluate multiplies every position's required margin by
// IMMultiplier(symbol) when the seam is bound (MarginOptions.Volatility)
// — the stored positions.margin_used and the derived notional/leverage
// path are scaled identically, since the requirement being protected
// against gap risk is the same number either way.
//
// Fail-closed (§2.7): a feed or baseline failure KEEPS the last-computed
// multiplier — protection already granted is never silently removed —
// and a symbol with no successful computation reports ×1.0 (neutral);
// error streaks page ops (P1) so a dead feed cannot hide rising vol.
package risk

import (
	"context"
	"fmt"
	"math"
	"sync"
	"time"

	"exchange/pkg/decimal"
)

// Canonical §13.6e scaler constants (Task 19.3.28).
const (
	volScalerWindow    = time.Hour // rolling realized-vol lookback
	volTriggerRatio    = 2.0       // vol > 2× baseline starts scaling
	volMaxMultiplier   = 1.5       // IM scale cap
	volMinMids         = 3         // minimum samples for an honest estimate
	volErrStreakForP1  = 3         // consecutive refresh failures → page
	volDefaultBaseline = 0.004     // per-return baseline vol fallback (40bp)
)

// ---------------------------------------------------------------------------
// Seams
// ---------------------------------------------------------------------------

// TickVolatilitySource supplies recent mid prices for realized-vol
// computation — production binds ClickHouse tick history (or the Redis
// tick ring); tests substitute a fake. Prices are time-ordered
// (oldest→newest) mids over the requested window.
type TickVolatilitySource interface {
	// RecentMids returns the symbol's mid prices observed since `since`,
	// oldest→newest. Fewer than volMinMids samples yields no estimate.
	RecentMids(ctx context.Context, symbol string, since time.Time) ([]decimal.Decimal, error)
}

// VolatilityBaselineSource resolves the instrument's baseline (normal)
// realized vol — production binds a long-window (e.g. 30-day) realized
// vol computation. Returning ok=false falls back to the configured
// default for that symbol.
type VolatilityBaselineSource interface {
	BaselineVol(ctx context.Context, symbol string) (baseline float64, ok bool, err error)
}

// IMMultiplierSource is the margin-path seam: the current IM multiplier
// for a symbol — ×1.0 neutral, >1 scaled up. VolatilityScaler satisfies
// it; MarginService consults it per position when bound.
type IMMultiplierSource interface {
	IMMultiplier(symbol string) float64
}

// ---------------------------------------------------------------------------
// VolatilityScaler
// ---------------------------------------------------------------------------

// VolatilityScaler computes and serves per-symbol IM multipliers.
// Refresh recomputes multipliers for the given symbol set — drive it
// from a periodic loop (e.g. every 30–60s) or on tick batches;
// IMMultiplier reads the last computed value (lock-free for callers).
type VolatilityScaler struct {
	src      TickVolatilitySource
	baseline VolatilityBaselineSource // optional; nil ⇒ configured defaults
	alerter  OpsAlerter
	now      func() time.Time
	logf     func(format string, args ...any)

	window       time.Duration
	triggerRatio float64
	maxMult      float64

	mu              sync.RWMutex
	baselines       map[string]float64 // per-symbol configured baseline
	defaultBaseline float64
	mult            map[string]float64
	vols            map[string]float64
	errStreak       int
}

// VolatilityScalerDeps wires the scaler. Source is required — a scaler
// without a feed would fabricate neutral multipliers.
type VolatilityScalerDeps struct {
	Source   TickVolatilitySource
	Baseline VolatilityBaselineSource
	Alerter  OpsAlerter
	Now      func() time.Time
	Logf     func(format string, args ...any)

	// Baselines maps symbol → baseline realized vol; overrides
	// DefaultBaseline (and the BaselineSource when it declines).
	Baselines map[string]float64
	// DefaultBaseline is the fallback baseline vol per return sample
	// when neither Baselines nor BaselineSource resolves one;
	// <= 0 ⇒ volDefaultBaseline (40bp).
	DefaultBaseline float64
	// Window overrides the 1h lookback (tests); <=0 = default.
	Window time.Duration
	// TriggerRatio overrides the 2× gate; <=0 = default.
	TriggerRatio float64
	// MaxMultiplier overrides the ×1.5 cap; <=0 = default.
	MaxMultiplier float64
}

// NewVolatilityScaler builds the scaler.
func NewVolatilityScaler(d VolatilityScalerDeps) (*VolatilityScaler, error) {
	if d.Source == nil {
		return nil, fmt.Errorf("volatility scaler: nil tick source")
	}
	s := &VolatilityScaler{
		src: d.Source, baseline: d.Baseline, alerter: d.Alerter,
		now: d.Now, logf: d.Logf,
		window:          volScalerWindow,
		triggerRatio:    volTriggerRatio,
		maxMult:         volMaxMultiplier,
		baselines:       map[string]float64{},
		defaultBaseline: volDefaultBaseline,
		mult:            map[string]float64{},
		vols:            map[string]float64{},
	}
	for k, v := range d.Baselines {
		if v > 0 {
			s.baselines[k] = v
		}
	}
	if d.DefaultBaseline > 0 {
		s.defaultBaseline = d.DefaultBaseline
	}
	if d.Window > 0 {
		s.window = d.Window
	}
	if d.TriggerRatio > 0 {
		s.triggerRatio = d.TriggerRatio
	}
	if d.MaxMultiplier > 0 {
		s.maxMult = d.MaxMultiplier
	}
	if s.now == nil {
		s.now = func() time.Time { return time.Now().UTC() }
	}
	if s.logf == nil {
		s.logf = func(string, ...any) {}
	}
	return s, nil
}

// Refresh recomputes the multiplier for every symbol in the set. Errors
// per symbol keep the prior multiplier (protection retained) and feed a
// P1 streak — a dead feed must page, not quietly disarm the scaler.
func (s *VolatilityScaler) Refresh(ctx context.Context, symbols []string) {
	since := s.now().Add(-s.window)
	for _, sym := range symbols {
		if sym == "" {
			continue
		}
		mids, err := s.src.RecentMids(ctx, sym, since)
		if err != nil {
			s.noteErr(sym, err)
			continue
		}
		vol, ok := realizedVol(mids)
		if !ok {
			continue // insufficient samples — retain prior multiplier
		}
		base, err := s.baselineFor(ctx, sym)
		if err != nil {
			s.noteErr(sym, err)
			continue
		}
		ratio := vol / base
		mult := ratio / s.triggerRatio
		if mult < 1.0 {
			mult = 1.0
		}
		if mult > s.maxMult {
			mult = s.maxMult
		}
		s.mu.Lock()
		s.mult[sym] = mult
		s.vols[sym] = vol
		s.errStreak = 0
		s.mu.Unlock()
	}
}

// baselineFor resolves the effective baseline: configured per-symbol
// override → BaselineSource → default.
func (s *VolatilityScaler) baselineFor(ctx context.Context, sym string) (float64, error) {
	s.mu.RLock()
	if b, ok := s.baselines[sym]; ok && b > 0 {
		s.mu.RUnlock()
		return b, nil
	}
	def := s.defaultBaseline
	s.mu.RUnlock()
	if s.baseline == nil {
		return def, nil
	}
	b, ok, err := s.baseline.BaselineVol(ctx, sym)
	if err != nil {
		// Baseline outage — fall back to the configured default (never
		// a zero baseline: division by zero would fabricate an infinite
		// multiplier) and surface the error for the streak counter.
		return def, fmt.Errorf("volatility baseline %s: %w", sym, err)
	}
	if !ok || b <= 0 {
		return def, nil
	}
	return b, nil
}

// noteErr counts consecutive refresh failures and pages on the streak.
func (s *VolatilityScaler) noteErr(sym string, err error) {
	s.mu.Lock()
	s.errStreak++
	n := s.errStreak
	s.mu.Unlock()
	s.logf("volatility scaler: refresh %s: %v", sym, err)
	if n >= volErrStreakForP1 && s.alerter != nil {
		actx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if aerr := s.alerter.Raise(actx, OpsAlert{
			Severity: SeverityP1, Code: "VOLATILITY_FEED_DEGRADED",
			Summary: fmt.Sprintf("volatility scaler feed failing (%d consecutive); last multipliers retained", n),
		}); aerr != nil {
			s.logf("volatility scaler: alert dispatch failed: %v", aerr)
		}
	}
}

// IMMultiplier implements IMMultiplierSource — the margin path's read.
// Symbols with no successful computation are neutral (×1.0).
func (s *VolatilityScaler) IMMultiplier(symbol string) float64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if m, ok := s.mult[symbol]; ok && m > 1.0 {
		return m
	}
	return 1.0
}

// VolatilityReading is the admin/debug view of one symbol's state.
type VolatilityReading struct {
	Symbol     string
	Realized   float64
	Baseline   float64
	Multiplier float64
}

// Reading exposes the current computation state for a symbol
// (admin surface — ok=false when never computed).
func (s *VolatilityScaler) Reading(ctx context.Context, symbol string) (VolatilityReading, bool) {
	// Copy state under the lock then release — baselineFor takes its own
	// RLock and recursive read locking can deadlock behind a writer.
	s.mu.RLock()
	v, ok := s.vols[symbol]
	m := s.mult[symbol]
	s.mu.RUnlock()
	if !ok {
		return VolatilityReading{Symbol: symbol, Multiplier: 1.0}, false
	}
	if m < 1.0 {
		m = 1.0
	}
	b, _ := s.baselineFor(ctx, symbol)
	return VolatilityReading{Symbol: symbol, Realized: v, Baseline: b, Multiplier: m}, true
}

// SetBaseline installs a per-symbol baseline override (admin knob);
// non-positive values are rejected — a zero baseline would divide by
// zero in the ratio.
func (s *VolatilityScaler) SetBaseline(symbol string, baseline float64) error {
	if symbol == "" || baseline <= 0 {
		return fmt.Errorf("volatility scaler: bad baseline for %q (must be > 0)", symbol)
	}
	s.mu.Lock()
	s.baselines[symbol] = baseline
	s.mu.Unlock()
	return nil
}

// realizedVol computes the population standard deviation of the log
// returns of the mid series. Non-positive mids are skipped — a corrupt
// tick never reaches the estimator. ok=false means fewer than two
// usable returns.
func realizedVol(mids []decimal.Decimal) (float64, bool) {
	var rets []float64
	var prev float64
	havePrev := false
	for _, m := range mids {
		if !m.IsPositive() {
			continue
		}
		p := m.InexactFloat64()
		if havePrev {
			rets = append(rets, math.Log(p/prev))
		}
		prev = p
		havePrev = true
	}
	if len(rets) < 2 {
		return 0, false
	}
	var mean float64
	for _, r := range rets {
		mean += r
	}
	mean /= float64(len(rets))
	var sumSq float64
	for _, r := range rets {
		d := r - mean
		sumSq += d * d
	}
	return math.Sqrt(sumSq / float64(len(rets))), true
}

// compile-time seam assertions.
var _ IMMultiplierSource = (*VolatilityScaler)(nil)
