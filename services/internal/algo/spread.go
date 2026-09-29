// Task 16.3.6 — Spread orders.
//
// Exactly two legs, opposite sides, with the spread-price invariant
// spread_price = leg1_price − leg2_price (positive spread = leg1 trades
// richer). Legs execute as IOC/marketable limits at the current mid —
// the per-shard matching core has NO cross-instrument atomicity, so the
// spec's "atomically" wording is realized as: submit leg A IOC → submit
// leg B IOC → on any leg failing or partially filling, compensate by
// flattening the filled residual (opposite-side IOC unwind leg) — the
// documented both-or-neither approximation (rollback-on-partial), with
// the atomicity deviation recorded in the Phase-16 plan.
//
// Market check: for a spread BUY (leg1 BUY/leg2 SELL) the market spread
// mid(leg1)−mid(leg2) must be ≤ spread_price (paying no more than the
// cap); for a spread SELL it must be ≥ spread_price. Mismatch rejects
// with SPREAD_ORDER_REJECTED before any parent row is written.
package algo

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"exchange/pkg/decimal"
)

// spreadSettleWindow is the synchronous post-dispatch observation window
// for IOC leg results (the pipeline's out-ring consumer must reflect
// fills before rollback decides).
const spreadSettleWindow = 2 * time.Second

func (e *Engine) validateSpread(req *SubmitRequest) (json.RawMessage, *time.Duration, error) {
	var p SpreadParams
	if len(req.Params) > 0 {
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return nil, nil, codeErr("INVALID_REQUEST", "spread params: %v", err)
		}
	}
	if len(p.Legs) != 2 {
		return nil, nil, codeErr("INVALID_REQUEST",
			"spread orders require exactly two legs, got %d", len(p.Legs))
	}
	for i, leg := range p.Legs {
		if leg.Symbol == "" {
			return nil, nil, codeErr("INVALID_REQUEST", "legs[%d].symbol is required", i)
		}
		if leg.Side != "BUY" && leg.Side != "SELL" {
			return nil, nil, codeErr("INVALID_REQUEST",
				"legs[%d].side must be BUY or SELL", i)
		}
		q, err := decimal.NewFromString(leg.Quantity)
		if err != nil || !q.IsPositive() {
			return nil, nil, codeErr("INVALID_REQUEST",
				"legs[%d].quantity %q is not a positive decimal", i, leg.Quantity)
		}
	}
	if p.Legs[0].Side == p.Legs[1].Side {
		return nil, nil, codeErr("INVALID_REQUEST",
			"spread legs must be opposite sides (both %s given)", p.Legs[0].Side)
	}
	if _, err := decimal.NewFromString(p.SpreadPrice); err != nil {
		return nil, nil, codeErr("INVALID_REQUEST",
			"spread_price %q is not a valid decimal", p.SpreadPrice)
	}
	return json.RawMessage(req.Params), nil, nil
}

// precheckSpread resolves current mids for both legs and applies the
// market-spread invariant — runs BEFORE the parent row exists, so a
// mismatch is a clean SPREAD_ORDER_REJECTED with no durable residue.
func (e *Engine) precheckSpread(ctx context.Context, req *SubmitRequest) error {
	var p SpreadParams
	if err := json.Unmarshal(req.Params, &p); err != nil {
		return codeErr("INVALID_REQUEST", "spread params: %v", err)
	}
	mid1, err := e.mid(ctx, p.Legs[0].Symbol)
	if err != nil {
		return codeErr("SPREAD_ORDER_REJECTED",
			"no market for leg %s: %v", p.Legs[0].Symbol, err)
	}
	mid2, err := e.mid(ctx, p.Legs[1].Symbol)
	if err != nil {
		return codeErr("SPREAD_ORDER_REJECTED",
			"no market for leg %s: %v", p.Legs[1].Symbol, err)
	}
	market := mid1.Sub(mid2)
	want, _ := decimal.NewFromString(p.SpreadPrice)
	// spread BUY = leg1 BUY + leg2 SELL: acceptable when market ≤ cap.
	if p.Legs[0].Side == "BUY" {
		if market.GreaterThan(want) {
			return codeErr("SPREAD_ORDER_REJECTED",
				"market spread %s exceeds spread_price %s", market, want)
		}
	} else {
		if market.LessThan(want) {
			return codeErr("SPREAD_ORDER_REJECTED",
				"market spread %s below spread_price %s", market, want)
		}
	}
	return nil
}

// executeSpread runs the synchronous leg-dispatch + rollback path on a
// freshly inserted NEW parent: RUNNING → COMPLETED / FAILED inside the
// request (SPREAD has no driver goroutine).
func (e *Engine) executeSpread(ctx context.Context, p *Parent) (*Parent, error) {
	if ok, err := e.store.CASStatus(ctx, p.ID,
		[]string{StatusNew}, StatusRunning, ""); err != nil || !ok {
		// Raced cancel — return the stored row.
		cur, _ := e.store.GetParent(ctx, p.ID)
		if cur != nil {
			return cur, nil
		}
		return nil, err
	}
	p.Status = StatusRunning
	var prm SpreadParams
	if err := json.Unmarshal(p.Params, &prm); err != nil {
		e.finish(ctx, p.ID, StatusFailed, "spread params decode")
		return e.store.GetParent(ctx, p.ID)
	}
	legs := make([]*Child, 2)
	for i, leg := range prm.Legs {
		qty := decimal.RequireFromString(leg.Quantity)
		mid, err := e.mid(ctx, leg.Symbol)
		if err != nil {
			e.finish(ctx, p.ID, StatusFailed,
				fmt.Sprintf("leg %d mid: %v", i, err))
			return e.store.GetParent(ctx, p.ID)
		}
		c := &Child{
			AlgoOrderID: p.ID, Seq: i, SliceIndex: i, Role: RoleLeg,
			Symbol: leg.Symbol, Side: leg.Side, Qty: qty, Price: &mid,
		}
		stored, err := e.store.InsertChild(ctx, c)
		if err != nil {
			e.finish(ctx, p.ID, StatusFailed, "leg intent: "+err.Error())
			return nil, errInternal("spread child intent", err)
		}
		legs[i] = stored
	}
	// Dispatch leg A, then leg B — IOC: each leg either fills in the
	// window or leaves a residual to unwind.
	dispErr := error(nil)
	for i, c := range legs {
		disp, err := e.dispatchChild(ctx, p, c, "IOC")
		legs[i] = disp
		if err != nil {
			dispErr = err
			break // leg A never went out → leg B must not dispatch
		}
	}
	// Observe fill results within the settle window.
	e.awaitLegs(ctx, p, legs)
	filledA := legs[0].FilledQty
	var filledB decimal.Decimal
	if legs[1] != nil && legs[1].Status != ChildPending {
		filledB = legs[1].FilledQty
	}
	_, _ = e.aggregateFilled(ctx, p.ID)
	bothDone := legs[0].Status == ChildFilled && legs[1] != nil &&
		legs[1].Status == ChildFilled
	if bothDone {
		e.finish(ctx, p.ID, StatusCompleted, "both legs filled")
		return e.store.GetParent(ctx, p.ID)
	}
	// Rollback-on-partial: flatten whatever filled — both-or-neither.
	for i, c := range legs {
		if c == nil || !c.FilledQty.IsPositive() {
			continue
		}
		if err := e.unwindLeg(ctx, p, c, 2+i); err != nil {
			e.logf2("spread parent %d unwind leg %d: %v", p.ID, i, err)
		}
	}
	// Cancel any leg left resting (IOC shouldn't rest — defensive).
	_ = e.cancelOpenChildren(ctx, p.ID, "spread rollback")
	detail := "spread not executable atomically"
	if dispErr != nil {
		detail = "leg dispatch: " + dispErr.Error()
	} else if filledA.IsPositive() || filledB.IsPositive() {
		detail = fmt.Sprintf("partial spread filled (%s/%s) — unwound",
			filledA, filledB)
	}
	e.finish(ctx, p.ID, StatusFailed, detail)
	return e.store.GetParent(ctx, p.ID)
}

// awaitLegs polls child statuses until both are terminal or the settle
// window lapses.
func (e *Engine) awaitLegs(ctx context.Context, p *Parent, legs []*Child) {
	deadline := e.now().Add(spreadSettleWindow)
	for e.now().Before(deadline) {
		allTerminal := true
		for i, c := range legs {
			if c == nil || c.OrderID == nil {
				continue
			}
			done, err := e.refreshChild(ctx, c)
			if err == nil {
				_ = e.store.UpdateChild(ctx, c)
				if !done {
					allTerminal = false
				}
			}
			legs[i] = c
		}
		if allTerminal {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// unwindLeg submits an opposite-side IOC child (role UNWIND) for the
// leg's filled quantity — the flattening leg of rollback-on-partial.
// Its own filled qty offsets the parent's aggregate (aggregateFilled
// subtracts UNWIND fills).
func (e *Engine) unwindLeg(ctx context.Context, p *Parent, leg *Child, seq int) error {
	opp := "SELL"
	if leg.Side == "SELL" {
		opp = "BUY"
	}
	mid, err := e.mid(ctx, leg.Symbol)
	if err != nil {
		return fmt.Errorf("unwind mid: %w", err)
	}
	c := &Child{
		AlgoOrderID: p.ID, Seq: seq, SliceIndex: leg.SliceIndex,
		Role: RoleUnwind, Symbol: leg.Symbol, Side: opp,
		Qty: leg.FilledQty, Price: &mid,
	}
	stored, err := e.store.InsertChild(ctx, c)
	if err != nil {
		return err
	}
	disp, err := e.dispatchChild(ctx, p, stored, "IOC")
	if err != nil {
		return err
	}
	// Brief settle observe for the unwind leg.
	deadline := e.now().Add(time.Second)
	for e.now().Before(deadline) {
		done, _ := e.refreshChild(ctx, disp)
		_ = e.store.UpdateChild(ctx, disp)
		if done {
			break
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(100 * time.Millisecond):
		}
	}
	return nil
}
