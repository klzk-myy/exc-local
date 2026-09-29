// Task 16.3.7 — Scaled orders.
//
// Up to 20 price levels, each tracked as an independent child limit
// order (deterministic client_order_id "algo:{parent}:{seq}"). Weights:
// EQUAL (uniform), LINEAR (linear ramp — deeper levels carry more
// size), CUSTOM (explicit weights). Prices: explicit level_prices, or
// start_price (default = current mid at plan time) stepped by spacing
// in pips or basis points. BUY ladders descend, SELL ladders ascend.
// Quantity distribution conserves total_qty exactly — final level
// takes the residual.
package algo

import (
	"context"
	"encoding/json"
	"time"

	"exchange/pkg/decimal"
)

const scaledMaxLevels = 20

func (e *Engine) validateScaled(req *SubmitRequest) (json.RawMessage, *time.Duration, error) {
	var p ScaledParams
	if len(req.Params) > 0 {
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return nil, nil, codeErr("INVALID_REQUEST", "scaled params: %v", err)
		}
	}
	if req.Side != "BUY" && req.Side != "SELL" {
		return nil, nil, codeErr("INVALID_REQUEST", "side must be BUY or SELL")
	}
	if req.Symbol == "" {
		return nil, nil, codeErr("INVALID_REQUEST", "symbol is required")
	}
	if p.Levels <= 0 {
		return nil, nil, codeErr("INVALID_REQUEST", "levels is required")
	}
	if p.Levels > scaledMaxLevels {
		return nil, nil, codeErr("INVALID_REQUEST",
			"levels %d exceeds the %d-level cap", p.Levels, scaledMaxLevels)
	}
	switch p.Distribution {
	case "EQUAL", "LINEAR":
	case "CUSTOM":
		if len(p.Weights) != p.Levels {
			return nil, nil, codeErr("INVALID_REQUEST",
				"custom distribution requires weights[] with %d entries", p.Levels)
		}
		sum := decimal.Zero
		for i, w := range p.Weights {
			d, err := decimal.NewFromString(w)
			if err != nil || d.IsNegative() {
				return nil, nil, codeErr("INVALID_REQUEST",
					"weights[%d] %q is not a non-negative decimal", i, w)
			}
			sum = sum.Add(d)
		}
		if !sum.IsPositive() {
			return nil, nil, codeErr("INVALID_REQUEST", "custom weights sum to zero")
		}
	default:
		return nil, nil, codeErr("INVALID_REQUEST",
			"distribution must be EQUAL | LINEAR | CUSTOM")
	}
	if len(p.LevelPrices) > 0 {
		if len(p.LevelPrices) != p.Levels {
			return nil, nil, codeErr("INVALID_REQUEST",
				"level_prices requires %d entries", p.Levels)
		}
		for i, s := range p.LevelPrices {
			d, err := decimal.NewFromString(s)
			if err != nil || !d.IsPositive() {
				return nil, nil, codeErr("INVALID_REQUEST",
					"level_prices[%d] %q is not a positive decimal", i, s)
			}
		}
	} else {
		switch {
		case p.SpacingPips != nil:
			d, err := decimal.NewFromString(*p.SpacingPips)
			if err != nil || !d.IsPositive() {
				return nil, nil, codeErr("INVALID_REQUEST",
					"spacing_pips %q is not a positive decimal", *p.SpacingPips)
			}
		case p.SpacingBps != nil:
			d, err := decimal.NewFromString(*p.SpacingBps)
			if err != nil || !d.IsPositive() {
				return nil, nil, codeErr("INVALID_REQUEST",
					"spacing_bps %q is not a positive decimal", *p.SpacingBps)
			}
		default:
			return nil, nil, codeErr("INVALID_REQUEST",
				"scaled orders need spacing_pips, spacing_bps, or level_prices")
		}
		if p.StartPrice != nil {
			d, err := decimal.NewFromString(*p.StartPrice)
			if err != nil || !d.IsPositive() {
				return nil, nil, codeErr("INVALID_REQUEST",
					"start_price %q is not a positive decimal", *p.StartPrice)
			}
		}
	}
	return json.RawMessage(req.Params), nil, nil // no TTL — GTC levels
}

// scaledWeights returns the raw distribution weights (EQUAL=1s,
// LINEAR=i+1 ramp, CUSTOM=explicit).
func scaledWeights(p *ScaledParams) []decimal.Decimal {
	w := make([]decimal.Decimal, p.Levels)
	switch p.Distribution {
	case "EQUAL":
		for i := range w {
			w[i] = decimal.NewFromInt(1)
		}
	case "LINEAR":
		for i := range w {
			w[i] = decimal.NewFromInt(int64(i + 1))
		}
	default: // CUSTOM
		for i, s := range p.Weights {
			w[i] = decimal.RequireFromString(s)
		}
	}
	return w
}

// scaledPrices computes the level prices: explicit level_prices win;
// else start (or mid) stepped by spacing_pips/spacing_bps — descending
// for BUY (accumulate on dips), ascending for SELL.
func (e *Engine) scaledPrices(ctx context.Context, p *Parent,
	prm *ScaledParams) ([]decimal.Decimal, error) {
	if len(prm.LevelPrices) == prm.Levels {
		out := make([]decimal.Decimal, prm.Levels)
		for i, s := range prm.LevelPrices {
			out[i] = decimal.RequireFromString(s)
		}
		return out, nil
	}
	start := decimal.Zero
	if prm.StartPrice != nil {
		start = decimal.RequireFromString(*prm.StartPrice)
	} else {
		mid, err := e.mid(ctx, p.Symbol)
		if err != nil {
			return nil, err
		}
		start = mid
	}
	var step decimal.Decimal
	switch {
	case prm.SpacingPips != nil:
		pip := e.pipSize(ctx, p.Symbol)
		if !pip.IsPositive() {
			return nil, codeErr("SERVICE_DEGRADED",
				"pip size unavailable for %s — spacing_pips cannot resolve", p.Symbol)
		}
		step = decimal.RequireFromString(*prm.SpacingPips).Mul(pip)
	default:
		bps := decimal.RequireFromString(*prm.SpacingBps)
		step = start.Mul(bps).Div(decimal.NewFromInt(10000))
	}
	out := make([]decimal.Decimal, prm.Levels)
	dir := decimal.NewFromInt(-1)
	if p.Side == "SELL" {
		dir = decimal.NewFromInt(1)
	}
	for i := range out {
		out[i] = start.Add(dir.Mul(decimal.NewFromInt(int64(i))).Mul(step))
		if !out[i].IsPositive() {
			return nil, codeErr("INVALID_REQUEST",
				"level %d steps below zero — reduce spacing or levels", i)
		}
	}
	return out, nil
}

// scaledState — durable plan cursor {"prices":[…],"qtys":[…],"next":k}.
type scaledState struct {
	Prices []string `json:"prices"`
	Qtys   []string `json:"qtys"`
	Next   int      `json:"next"`
}

func decodeScaledState(p *Parent) *scaledState {
	st := &scaledState{}
	if len(p.State) == 0 {
		return nil
	}
	if err := json.Unmarshal(p.State, st); err != nil || len(st.Qtys) == 0 {
		return nil
	}
	return st
}

func (st *scaledState) save(ctx context.Context, e *Engine, parentID int64) {
	raw, err := json.Marshal(st)
	if err != nil {
		e.logf2("scaled state marshal parent %d: %v", parentID, err)
		return
	}
	if err := e.store.SaveState(ctx, parentID, raw); err != nil {
		e.logf2("scaled state save parent %d: %v", parentID, err)
	}
}

// scaledDriver submits every level as a resting GTC limit then monitors
// children to a terminal state. Anti-gaming timing isn't part of scaled
// semantics — the ladder is visible by design (unlike TWAP/VWAP/VP).
type scaledDriver struct{}

func (scaledDriver) Run(ctx context.Context, e *Engine, parentID int64) error {
	p, err := e.store.GetParent(ctx, parentID)
	if err != nil || p == nil {
		return err
	}
	var prm ScaledParams
	if err := json.Unmarshal(p.Params, &prm); err != nil {
		e.finish(ctx, p.ID, StatusFailed, "scaled params decode: "+err.Error())
		return nil
	}
	st := decodeScaledState(p)
	if st == nil {
		prices, err := e.scaledPrices(ctx, p, &prm)
		if err != nil {
			e.finish(ctx, p.ID, StatusFailed, "scaled prices: "+err.Error())
			return nil
		}
		plan := weightsToPlan(scaledWeights(&prm), p.TotalQty)
		st = &scaledState{}
		for _, pr := range prices {
			st.Prices = append(st.Prices, pr.String())
		}
		for _, q := range plan {
			st.Qtys = append(st.Qtys, q.String())
		}
		st.save(ctx, e, p.ID)
	}
	next, err := e.nextChildSeq(ctx, p.ID)
	if err != nil {
		return err
	}
	// Submit remaining levels — each level an independent child.
	for ; st.Next < len(st.Qtys); st.Next++ {
		status, _ := e.statusOf(ctx, p.ID)
		if status != StatusRunning {
			break // paused/cancelled — monitor loop below owns the rest
		}
		price := decimal.RequireFromString(st.Prices[st.Next])
		qty := decimal.RequireFromString(st.Qtys[st.Next])
		if !qty.IsPositive() {
			continue
		}
		child := &Child{
			AlgoOrderID: p.ID, Seq: next, SliceIndex: st.Next,
			Role: RoleSlice, Symbol: p.Symbol, Side: p.Side,
			Qty: qty, Price: &price,
		}
		next++
		stored, err := e.store.InsertChild(ctx, child)
		if err != nil {
			return errInternal("scaled child intent", err)
		}
		st.save(ctx, e, p.ID)
		if _, err := e.dispatchChild(ctx, p, stored, "GTC"); err != nil {
			e.logf2("scaled parent %d level %d rejected: %v", p.ID, st.Next, err)
		}
	}
	// Monitor: refresh live children until none remain or the parent
	// goes terminal. PAUSED holds the monitor without dispatching.
	for {
		select {
		case <-ctx.Done():
			return nil
		default:
		}
		status, err := e.statusOf(ctx, p.ID)
		if err != nil {
			return err
		}
		if status != StatusRunning && status != StatusPaused {
			return nil
		}
		open, err := e.store.OpenChildren(ctx, p.ID)
		if err != nil {
			return err
		}
		live := 0
		for i := range open {
			c := open[i]
			done, err := e.refreshChild(ctx, &c)
			if err != nil {
				continue
			}
			if err := e.store.UpdateChild(ctx, &c); err != nil {
				e.logf2("scaled parent %d child %d update: %v", p.ID, c.ID, err)
			}
			if !done {
				live++
			}
		}
		_, _ = e.aggregateFilled(ctx, p.ID)
		// A PAUSED parent keeps monitoring (fills can still settle) —
		// pausing cancels live children, so live drains to zero.
		if live == 0 && status == StatusRunning {
			e.finish(ctx, p.ID, StatusCompleted, "ladder settled")
			return nil
		}
		if live == 0 && status == StatusPaused {
			return nil // driver exits; Resume re-spawns via RUNNING
		}
		select {
		case <-ctx.Done():
			return nil
		case <-e.wakeChan(p.ID):
		case <-time.After(300 * time.Millisecond):
		}
	}
}
