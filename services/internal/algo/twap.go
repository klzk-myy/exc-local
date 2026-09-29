// Task 16.3.1 — TWAP (Time-Weighted Average Price).
//
// Equal quantity slices dispatched once per interval at the current
// midpoint (§16.3.10 step 2 price basis), interval band 1s–1h (step 3),
// unfilled slice cancelled at interval end with the remainder rolled
// forward (step 6). Anti-gaming (Task 16.3.12): dispatch boundaries
// jittered ±30% of interval, slice quantities perturbed ±15% with the
// residual pinned to the final slice — sum(plan) == total_qty exactly.
package algo

import (
	"context"
	"encoding/json"
	"math"
	"math/rand/v2"
	"time"

	"exchange/pkg/decimal"
)

// TWAP validation band — Task 16.3.10 step 3.
const (
	twapMinInterval = time.Second
	twapMaxInterval = time.Hour
)

func (e *Engine) validateTWAP(req *SubmitRequest) (json.RawMessage, *time.Duration, error) {
	var p TWAPParams
	if len(req.Params) > 0 {
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return nil, nil, codeErr("INVALID_REQUEST", "twap params: %v", err)
		}
	}
	if req.Side != "BUY" && req.Side != "SELL" {
		return nil, nil, codeErr("INVALID_REQUEST", "side must be BUY or SELL")
	}
	if req.Symbol == "" {
		return nil, nil, codeErr("INVALID_REQUEST", "symbol is required")
	}
	if p.IntervalSecs <= 0 {
		return nil, nil, codeErr("INVALID_REQUEST", "interval_secs is required")
	}
	iv := time.Duration(p.IntervalSecs) * time.Second
	if iv < twapMinInterval || iv > twapMaxInterval {
		return nil, nil, codeErr("INVALID_REQUEST",
			"interval_secs %d outside the TWAP band [1, 3600]", p.IntervalSecs)
	}
	if p.DurationSecs <= 0 {
		return nil, nil, codeErr("INVALID_REQUEST", "duration_secs must be positive")
	}
	if p.DiscretionPips < 0 || p.DiscretionPips > MaxDiscretionPips {
		return nil, nil, codeErr("INVALID_REQUEST",
			"discretion_pips %d outside [0,%d]", p.DiscretionPips, MaxDiscretionPips)
	}
	ttl := time.Duration(p.DurationSecs) * time.Second
	return json.RawMessage(req.Params), &ttl, nil
}

// twapPlan builds the randomized schedule: N equal intervals over the
// duration, boundaries jittered ±30% (monotone-clamped), quantities
// perturbed ±15% with exact-sum conservation (final slice = residual).
func twapPlan(total decimal.Decimal, dur, iv time.Duration, r *rand.Rand,
) ([]decimal.Decimal, []time.Duration) {
	n := int64(math.Ceil(float64(dur) / float64(iv)))
	if n < 1 {
		n = 1
	}
	// Equal base split; the residual lands on the final slice so the
	// plan sums to `total` exactly even when total/n isn't terminating.
	base := total.Div(decimal.NewFromInt(n))
	plan := make([]decimal.Decimal, n)
	for i := int64(0); i < n-1; i++ {
		plan[i] = base
	}
	plan[n-1] = total.Sub(base.Mul(decimal.NewFromInt(n - 1)))
	plan = perturbSizes(r, plan, total, SizePerturbPermille)

	bounds := make([]time.Duration, n)
	for i := int64(0); i < n; i++ {
		b := jitterDuration(r, time.Duration(i)*iv, TimingJitterPermille)
		if i > 0 && b < bounds[i-1] {
			b = bounds[i-1] // monotone: a jittered bound never precedes its parent
		}
		if b < 0 {
			b = 0
		}
		bounds[i] = b
	}
	return plan, bounds
}

// twapDriver — one goroutine per RUNNING TWAP parent.
type twapDriver struct{}

func (twapDriver) Run(ctx context.Context, e *Engine, parentID int64) error {
	p, err := e.store.GetParent(ctx, parentID)
	if err != nil || p == nil {
		return err
	}
	var prm TWAPParams
	if err := json.Unmarshal(p.Params, &prm); err != nil {
		e.finish(ctx, p.ID, StatusFailed, "twap params decode: "+err.Error())
		return nil
	}
	pip := e.pipSize(ctx, p.Symbol)
	_, err = materializeSliceState(ctx, e, p, prm.DiscretionPips, pip,
		func(r *rand.Rand) ([]decimal.Decimal, []time.Duration, error) {
			qtys, bounds := twapPlan(p.TotalQty,
				time.Duration(prm.DurationSecs)*time.Second,
				time.Duration(prm.IntervalSecs)*time.Second, r)
			return qtys, bounds, nil
		})
	if err != nil {
		e.finish(ctx, p.ID, StatusFailed, "twap plan: "+err.Error())
		return nil
	}
	// Re-read: materializeSliceState persisted the plan — runSliced
	// decodes the durable copy.
	if p, err = e.store.GetParent(ctx, parentID); err != nil || p == nil {
		return err
	}
	return e.runSliced(ctx, p)
}
