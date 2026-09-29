// Task 16.3.18 — Volume Participation (VP).
//
// Each 5-second window measures OBSERVED trade volume on the symbol
// (VolumeSource seam — wired to the trades tape via the fill-bus/NATS
// tracker in cmd/gateway, with the PgVolumeSource tape read as the
// durable fallback) and submits a child sized at participation_rate of
// that volume. Children are IOC limits at mid ± discretion, capped by
// the optional price_limit — IOC is deliberate: a resting VP slice
// would detach participation from the measured window.
//
// participation_rate ∈ [0.01, 0.50] (1%–50%). Anti-gaming (16.3.12):
// window boundaries jittered ±15%, child size perturbed ±20% — the
// tighter VP envelope per the task text. Actual dispatches audit through
// algo_order_children like every strategy.
package algo

import (
	"context"
	"encoding/json"
	"time"

	"exchange/pkg/decimal"
)

const (
	vpWindow        = 5 * time.Second
	vpMinRate       = 0.01
	vpMaxRate       = 0.50
	vpDefaultMaxDur = int64(3600) // 1h default when max_duration_secs omitted
)

func (e *Engine) validateVP(req *SubmitRequest) (json.RawMessage, *time.Duration, error) {
	var p VPParams
	if len(req.Params) > 0 {
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return nil, nil, codeErr("INVALID_REQUEST", "vp params: %v", err)
		}
	}
	if e.volume == nil {
		return nil, nil, codeErr("SERVICE_DEGRADED",
			"volume participation unavailable — trade-volume source unwired")
	}
	if req.Side != "BUY" && req.Side != "SELL" {
		return nil, nil, codeErr("INVALID_REQUEST", "side must be BUY or SELL")
	}
	if req.Symbol == "" {
		return nil, nil, codeErr("INVALID_REQUEST", "symbol is required")
	}
	if p.ParticipationRate < vpMinRate || p.ParticipationRate > vpMaxRate {
		return nil, nil, codeErr("INVALID_REQUEST",
			"participation_rate %g outside [0.01, 0.50]", p.ParticipationRate)
	}
	if p.PriceLimit != nil {
		if _, err := decimal.NewFromString(*p.PriceLimit); err != nil {
			return nil, nil, codeErr("INVALID_REQUEST",
				"price_limit %q is not a valid decimal", *p.PriceLimit)
		}
	}
	if p.MaxDurationSecs <= 0 {
		p.MaxDurationSecs = vpDefaultMaxDur
	}
	if p.DiscretionPips < 0 || p.DiscretionPips > MaxDiscretionPips {
		return nil, nil, codeErr("INVALID_REQUEST",
			"discretion_pips %d outside [0,%d]", p.DiscretionPips, MaxDiscretionPips)
	}
	// Re-emit normalized params (defaults applied).
	norm, err := json.Marshal(p)
	if err != nil {
		return nil, nil, errInternal("vp params", err)
	}
	ttl := time.Duration(p.MaxDurationSecs) * time.Second
	return json.RawMessage(norm), &ttl, nil
}

// vpDriver runs one VP parent: 5s windows → participation-sized IOC
// children until max duration or terminal transition.
type vpDriver struct{}

func (vpDriver) Run(ctx context.Context, e *Engine, parentID int64) error {
	p, err := e.store.GetParent(ctx, parentID)
	if err != nil || p == nil {
		return err
	}
	var prm VPParams
	if err := json.Unmarshal(p.Params, &prm); err != nil {
		e.finish(ctx, p.ID, StatusFailed, "vp params decode: "+err.Error())
		return nil
	}
	rate := decimal.NewFromFloat(prm.ParticipationRate)
	var limit *decimal.Decimal
	if prm.PriceLimit != nil {
		if d, err := decimal.NewFromString(*prm.PriceLimit); err == nil {
			limit = &d
		}
	}
	pip := e.pipSize(ctx, p.Symbol)
	r := newPRNG()
	next, err := e.nextChildSeq(ctx, p.ID)
	if err != nil {
		return err
	}
	mark := e.now() // window watermark — restart-safe (fresh window)
	for {
		// Jittered next-window boundary (±15% — VP envelope).
		at := mark.Add(jitterDuration(r, vpWindow, VPTimingJitterPermille))
		if !e.waitSlice(ctx, p.ID, at) {
			break // terminal/paused-forever/expired — closeOut lands state
		}
		observed, err := e.volume.VolumeSince(ctx, p.Symbol, mark)
		mark = e.now() // advance the watermark even on read error
		if err != nil {
			e.logf2("vp parent %d volume read: %v", p.ID, err)
			continue
		}
		filled, _ := e.aggregateFilled(ctx, p.ID)
		remaining := p.TotalQty.Sub(filled)
		if !remaining.IsPositive() {
			e.finish(ctx, p.ID, StatusCompleted, "fully filled")
			return nil
		}
		if !observed.IsPositive() {
			continue // silent window — no participation
		}
		// qty = observed × rate, perturbed ±20%, capped at remaining.
		qty := pm(observed.Mul(rate), jitterFactorPm(r, VPSizePerturbPermille))
		if qty.GreaterThan(remaining) {
			qty = remaining
		}
		if !qty.IsPositive() {
			continue
		}
		mid, err := e.mid(ctx, p.Symbol)
		if err != nil {
			e.logf2("vp parent %d mid: %v", p.ID, err)
			continue
		}
		price := mid.Add(discretionOffset(r, p.Side, prm.DiscretionPips, pip))
		if limit != nil {
			// price_limit caps aggression: BUY ≤ limit, SELL ≥ limit.
			if p.Side == "BUY" && price.GreaterThan(*limit) {
				price = *limit
			} else if p.Side == "SELL" && price.LessThan(*limit) {
				price = *limit
			}
		}
		child := &Child{
			AlgoOrderID: p.ID, Seq: next, SliceIndex: next,
			Role: RoleSlice, Symbol: p.Symbol, Side: p.Side,
			Qty: qty, Price: &price,
		}
		next++
		stored, err := e.store.InsertChild(ctx, child)
		if err != nil {
			return errInternal("vp child intent", err)
		}
		if _, err := e.dispatchChild(ctx, p, stored, "IOC"); err != nil {
			e.logf2("vp parent %d slice rejected: %v", p.ID, err)
		}
	}
	// Terminal landing — same close-out as the slicer (no open IOC left).
	return e.closeOut(ctx, p, nil)
}
