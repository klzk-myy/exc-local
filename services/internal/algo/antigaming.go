// Task 16.3.12 — TWAP/VWAP/VP anti-gaming randomization.
//
// Randomness is seeded from crypto/rand per parent run and is
// deliberately NOT persisted: anti-gaming jitter must be unpredictable
// and non-replayable — a seeded PRNG in algo params would let an
// adversary (or a replayed WAL) reconstruct the schedule. Auditability
// comes from the algo_order_children rows, which record the ACTUAL
// dispatched time/qty/price of every child — behavior is auditable
// without being reproducible.
//
// All quantity/price math stays in integer permille over
// decimal.Decimal — no float64 ever touches a financial quantity
// (spec §5.3); NormFloat64 is used only inside the permille domain for
// the VWAP Gaussian profile noise.
package algo

import (
	crand "crypto/rand"
	"encoding/binary"
	"math/rand/v2"
	"time"

	"exchange/pkg/decimal"
)

// Anti-gaming envelopes (Task 16.3.12):
const (
	// TimingJitterPermille is ±30% of the interval for TWAP/VWAP slice
	// dispatch times.
	TimingJitterPermille int64 = 300
	// SizePerturbPermille is ±15% of the planned slice quantity.
	SizePerturbPermille int64 = 150
	// MaxDiscretionPips caps the price discretion band at 3 pips.
	MaxDiscretionPips int64 = 3
	// VP overrides (Task 16.3.18): ±20% size, ±15% timing.
	VPSizePerturbPermille  int64 = 200
	VPTimingJitterPermille int64 = 150
	// VWAPProfileNoiseSigmaPm is σ=5% expressed in permille for the
	// Gaussian profile smoothing.
	VWAPProfileNoiseSigmaPm int64 = 50
)

// newPRNG returns a per-run PRNG seeded from crypto/rand — a fresh,
// unrecorded seed per parent run is the anti-gaming contract.
func newPRNG() *rand.Rand {
	var seed [16]byte
	if _, err := crand.Read(seed[:]); err != nil {
		// crypto/rand failure: fall back to nanotime jitter rather than
		// silently producing a fixed sequence (still never persisted).
		return rand.New(rand.NewPCG(uint64(time.Now().UnixNano()), uint64(time.Now().UnixNano()>>1)))
	}
	return rand.New(rand.NewPCG(binary.LittleEndian.Uint64(seed[:8]),
		binary.LittleEndian.Uint64(seed[8:])))
}

// jitterFactorPm returns a multiplier in permille uniformly distributed
// in [1000−maxPm, 1000+maxPm].
func jitterFactorPm(r *rand.Rand, maxPm int64) int64 {
	if maxPm <= 0 {
		return 1000
	}
	return 1000 + r.Int64N(2*maxPm+1) - maxPm
}

// pm applies a permille multiplier: d × pm/1000.
func pm(d decimal.Decimal, permille int64) decimal.Decimal {
	return d.Mul(decimal.NewFromInt(permille)).Div(decimal.NewFromInt(1000))
}

// jitterDuration perturbs d by ±maxPm permille (Task 16.3.12 step 1:
// each TWAP slice submission time is perturbed by ±random(0,0.3×interval)).
func jitterDuration(r *rand.Rand, d time.Duration, maxPm int64) time.Duration {
	return time.Duration(int64(d) * jitterFactorPm(r, maxPm) / 1000)
}

// perturbSizes returns per-slice quantities perturbed by ±maxPm
// permille while preserving sum(schedule) == total EXACTLY — the
// remainder lands on the final slice (Task 16.3.12 step 2).
//
// Clamps keep every slice ≥ 0 and leave a positive remainder for the
// final slice: a slice may consume at most 90% of what is left, so the
// tail can never go negative even under maximal front-loaded
// perturbation (e.g. many slices all drawing +15%).
func perturbSizes(r *rand.Rand, schedule []decimal.Decimal, total decimal.Decimal,
	maxPm int64) []decimal.Decimal {
	n := len(schedule)
	out := make([]decimal.Decimal, n)
	if n == 0 {
		return out
	}
	if n == 1 {
		out[0] = total // single slice — no jitter needed (spec edge case)
		return out
	}
	remaining := total
	for i := 0; i < n-1; i++ {
		q := pm(schedule[i], jitterFactorPm(r, maxPm))
		// Never consume more than 90% of the remaining — the final
		// slice must stay positive.
		cap90 := pm(remaining, 900)
		if q.GreaterThan(cap90) {
			q = cap90
		}
		if q.IsNegative() {
			q = decimal.Zero
		}
		out[i] = q
		remaining = remaining.Sub(q)
	}
	out[n-1] = remaining
	return out
}

// gaussianPerturb multiplies each weight by (1 + N(0,σ)) clamped at 0 —
// the Task 16.3.12 step 4 VWAP profile smoothing (σ=5% → 50 permille).
// Float64 is confined to the noise draw; the multiplier lands as an
// integer permille so the Decimal math stays exact.
func gaussianPerturb(r *rand.Rand, weights []decimal.Decimal, sigmaPm int64) []decimal.Decimal {
	out := make([]decimal.Decimal, len(weights))
	for i, w := range weights {
		draw := int64(r.NormFloat64() * float64(sigmaPm))
		factor := 1000 + draw
		if factor < 0 {
			factor = 0
		}
		out[i] = pm(w, factor)
	}
	return out
}

// discretionOffset applies the price discretion band: a uniform offset
// in [0, pips] × pipSize added for BUY (aggressive) or subtracted for
// SELL — paid spread is bounded by the configured band (0–3 pips).
func discretionOffset(r *rand.Rand, side string, pips int64, pipSize decimal.Decimal) decimal.Decimal {
	if pips <= 0 || !pipSize.IsPositive() {
		return decimal.Zero
	}
	steps := r.Int64N(pips*10 + 1) // 0.1-pip granularity within the band
	off := pipSize.Mul(decimal.NewFromInt(steps).Div(decimal.NewFromInt(10)))
	if side == "SELL" {
		return off.Neg()
	}
	return off
}
