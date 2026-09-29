// Task 16.3.2 — VWAP (Volume-Weighted Average Price).
//
// Slices are proportional to the historical volume profile (trailing-24h
// bucketed share from PgVolumeProfile; a Phase-23 ClickHouse bucket
// source binds the same VolumeProfileSource seam when it lands). The
// documented fallback when the profile is empty/unwired is a FLAT
// distribution — equal slices, never fabricated volume.
//
// Anti-gaming (Task 16.3.12 step 4): the profile is smoothed with
// Gaussian noise σ=5% before weighting, plus the shared ±30% timing
// jitter and ±15% size perturbation — two runs of the same order
// produce different child sequences (statistically asserted in tests).
package algo

import (
	"context"
	"encoding/json"
	"math"
	"math/rand/v2"
	"time"

	"exchange/pkg/decimal"
)

const vwapDefaultIntervalSecs int64 = 60

func (e *Engine) validateVWAP(req *SubmitRequest) (json.RawMessage, *time.Duration, error) {
	var p VWAPParams
	if len(req.Params) > 0 {
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return nil, nil, codeErr("INVALID_REQUEST", "vwap params: %v", err)
		}
	}
	if req.Side != "BUY" && req.Side != "SELL" {
		return nil, nil, codeErr("INVALID_REQUEST", "side must be BUY or SELL")
	}
	if req.Symbol == "" {
		return nil, nil, codeErr("INVALID_REQUEST", "symbol is required")
	}
	if p.DurationSecs <= 0 {
		return nil, nil, codeErr("INVALID_REQUEST", "duration_secs must be positive")
	}
	if p.IntervalSecs == 0 {
		p.IntervalSecs = vwapDefaultIntervalSecs
	}
	iv := time.Duration(p.IntervalSecs) * time.Second
	if iv < twapMinInterval || iv > twapMaxInterval {
		return nil, nil, codeErr("INVALID_REQUEST",
			"interval_secs %d outside the VWAP band [1, 3600]", p.IntervalSecs)
	}
	if p.DiscretionPips < 0 || p.DiscretionPips > MaxDiscretionPips {
		return nil, nil, codeErr("INVALID_REQUEST",
			"discretion_pips %d outside [0,%d]", p.DiscretionPips, MaxDiscretionPips)
	}
	ttl := time.Duration(p.DurationSecs) * time.Second
	return json.RawMessage(req.Params), &ttl, nil
}

// weightsToPlan converts normalized weights into quantities summing to
// `total` exactly — sequential allocation, final slice takes residual.
func weightsToPlan(weights []decimal.Decimal, total decimal.Decimal) []decimal.Decimal {
	n := len(weights)
	plan := make([]decimal.Decimal, n)
	sum := decimal.Zero
	for _, w := range weights {
		if w.IsPositive() {
			sum = sum.Add(w)
		}
	}
	if !sum.IsPositive() {
		return plan
	}
	acc := decimal.Zero
	for i := 0; i < n-1; i++ {
		q := total.Mul(weights[i]).Div(sum)
		if q.IsNegative() {
			q = decimal.Zero
		}
		plan[i] = q
		acc = acc.Add(q)
	}
	plan[n-1] = total.Sub(acc)
	return plan
}

// flatProfile — the documented empty-tape fallback (one equal bucket).
func flatProfile(n int) []decimal.Decimal {
	out := make([]decimal.Decimal, n)
	for i := range out {
		out[i] = decimal.NewFromInt(1)
	}
	return out
}

// vwapDriver — RUNNING VWAP parent → interval slicer over a
// noise-smoothed volume-profile schedule.
type vwapDriver struct{}

func (vwapDriver) Run(ctx context.Context, e *Engine, parentID int64) error {
	p, err := e.store.GetParent(ctx, parentID)
	if err != nil || p == nil {
		return err
	}
	var prm VWAPParams
	if err := json.Unmarshal(p.Params, &prm); err != nil {
		e.finish(ctx, p.ID, StatusFailed, "vwap params decode: "+err.Error())
		return nil
	}
	dur := time.Duration(prm.DurationSecs) * time.Second
	iv := time.Duration(prm.IntervalSecs) * time.Second
	n := int(math.Ceil(float64(dur) / float64(iv)))
	if n < 1 {
		n = 1
	}
	pip := e.pipSize(ctx, p.Symbol)
	_, err = materializeSliceState(ctx, e, p, prm.DiscretionPips, pip,
		func(r *rand.Rand) ([]decimal.Decimal, []time.Duration, error) {
			weights, err := e.volumeProfile(ctx, p.Symbol, n)
			if err != nil {
				return nil, nil, err
			}
			// Gaussian σ=5% profile smoothing — a fresh draw per run.
			weights = gaussianPerturb(r, weights, VWAPProfileNoiseSigmaPm)
			plan := weightsToPlan(weights, p.TotalQty)
			plan = perturbSizes(r, plan, p.TotalQty, SizePerturbPermille)
			bounds := make([]time.Duration, n)
			for i := 0; i < n; i++ {
				b := jitterDuration(r, time.Duration(i)*iv, TimingJitterPermille)
				if i > 0 && b < bounds[i-1] {
					b = bounds[i-1]
				}
				if b < 0 {
					b = 0
				}
				bounds[i] = b
			}
			return plan, bounds, nil
		})
	if err != nil {
		e.finish(ctx, p.ID, StatusFailed, "vwap plan: "+err.Error())
		return nil
	}
	if p, err = e.store.GetParent(ctx, parentID); err != nil || p == nil {
		return err
	}
	return e.runSliced(ctx, p)
}

// volumeProfile resolves the schedule weights: the configured source, or
// the flat fallback when unwired/empty — documented in the file header.
func (e *Engine) volumeProfile(ctx context.Context, symbol string, buckets int) ([]decimal.Decimal, error) {
	if e.profiles != nil {
		w, err := e.profiles.VolumeProfile(ctx, symbol, buckets)
		if err != nil {
			return nil, err
		}
		sum := decimal.Zero
		for _, x := range w {
			if x.IsPositive() {
				sum = sum.Add(x)
			}
		}
		if len(w) == buckets && sum.IsPositive() {
			return w, nil
		}
	}
	return flatProfile(buckets), nil
}
