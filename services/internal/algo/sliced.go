// Shared interval-slicer driver core — TWAP (equal slices, Task 16.3.1)
// and VWAP (volume-profile-weighted slices, Task 16.3.2) differ only in
// how the schedule is built at submit time; the dispatch/pause/cancel/
// expire state machine is identical and lives here once.
package algo

import (
	"context"
	"encoding/json"
	"math/rand/v2"
	"time"

	"exchange/pkg/decimal"
)

// sliceState is the driver's durable cursor (parent.state JSON):
//
//	{"start":"RFC3339Nano","qtys":["0.5",…],"bounds_ms":[…],"next":2}
//
// The randomized plan is persisted so a restart resumes the SAME schedule
// (anti-gaming auditability — algo_order_children records actuals).
type sliceState struct {
	Start      time.Time `json:"start"`
	Qtys       []string  `json:"qtys"`
	BoundsMs   []int64   `json:"bounds_ms"`
	Next       int       `json:"next"`
	DiscrePips int64     `json:"discretion_pips"`
	PipSize    string    `json:"pip_size"`
}

func (s *sliceState) decQtys() []decimal.Decimal {
	out := make([]decimal.Decimal, len(s.Qtys))
	for i, q := range s.Qtys {
		out[i] = decimal.RequireFromString(q)
	}
	return out
}

func (s *sliceState) bounds() []time.Duration {
	out := make([]time.Duration, len(s.BoundsMs))
	for i, ms := range s.BoundsMs {
		out[i] = time.Duration(ms) * time.Millisecond
	}
	return out
}

func (s *sliceState) save(ctx context.Context, e *Engine, parentID int64) {
	raw, err := json.Marshal(s)
	if err != nil {
		e.logf2("state marshal parent %d: %v", parentID, err)
		return
	}
	if err := e.store.SaveState(ctx, parentID, raw); err != nil {
		e.logf2("state save parent %d: %v", parentID, err)
	}
}

// wakeChan returns the driver's wake channel (nil-safe).
func (e *Engine) wakeChan(parentID int64) <-chan struct{} {
	e.mu.Lock()
	defer e.mu.Unlock()
	if h, ok := e.handles[parentID]; ok {
		return h.wake
	}
	return nil
}

// waitSlice waits until `at`, honoring PAUSE (held until resume), parent
// terminal states (returns false), ctx cancellation, and expiry. While
// paused no dispatch occurs; on resume a boundary that elapsed during the
// hold dispatches promptly — the schedule continues from the cursor.
func (e *Engine) waitSlice(ctx context.Context, parentID int64,
	at time.Time) bool {
	wake := e.wakeChan(parentID)
	for {
		select {
		case <-ctx.Done():
			return false
		default:
		}
		status, err := e.statusOf(ctx, parentID)
		if err != nil {
			e.logf2("parent %d status read: %v", parentID, err)
		}
		switch status {
		case StatusPaused:
			select {
			case <-ctx.Done():
				return false
			case <-wake:
			case <-time.After(200 * time.Millisecond):
			}
			continue
		case StatusRunning:
			// fall through to the timed wait
		default:
			return false // terminal or unknown — driver exits
		}
		p, _ := e.store.GetParent(ctx, parentID)
		if p != nil && p.ExpiresAt != nil && !e.now().Before(*p.ExpiresAt) {
			return false
		}
		if !e.now().Before(at) {
			return true
		}
		d := 150 * time.Millisecond
		if rem := at.Sub(e.now()); rem < d {
			d = rem
		}
		select {
		case <-ctx.Done():
			return false
		case <-wake:
		case <-time.After(d):
		}
	}
}

// runSliced executes the interval slicer for a parent whose state holds a
// sliceState. Slices are GTC limits at mid ± discretion, cancelled at
// interval end with the unfilled remainder rolled forward
// (Task 16.3.10 step 6). Total quantity is conserved exactly: each slice
// is min(planned, remaining) and the final slice takes the rest.
func (e *Engine) runSliced(ctx context.Context, p *Parent) error {
	st := decodeSliceState(p)
	if st == nil {
		e.finish(ctx, p.ID, StatusFailed, "corrupt or missing slice state")
		return nil
	}
	plan := st.decQtys()
	bounds := st.bounds()
	if len(plan) == 0 || len(bounds) == 0 {
		e.finish(ctx, p.ID, StatusFailed, "empty schedule")
		return nil
	}
	pip := decimal.Zero
	if st.PipSize != "" {
		if d, err := decimal.NewFromString(st.PipSize); err == nil {
			pip = d
		}
	}
	r := newPRNG() // per-run; close-out jitter + discretion draws
	filled := p.FilledQty
	next, err := e.nextChildSeq(ctx, p.ID)
	if err != nil {
		return err
	}
	var open *Child
	for ; st.Next < len(plan); st.Next++ {
		// Wait for this slice's jittered boundary.
		if !e.waitSlice(ctx, p.ID, st.Start.Add(bounds[st.Next])) {
			return e.closeOut(ctx, p, open)
		}
		// Cancel the previous interval's unfilled slice — remainder rolls.
		if open != nil && isLiveChild(open) {
			if err := e.cancelChild(ctx, p, open); err != nil {
				e.logf2("parent %d close slice %d: %v", p.ID, open.ID, err)
			}
		}
		open = nil
		if agg, err := e.aggregateFilled(ctx, p.ID); err == nil {
			filled = agg
		}
		remaining := p.TotalQty.Sub(filled)
		if !remaining.IsPositive() {
			e.finish(ctx, p.ID, StatusCompleted, "fully filled")
			return nil
		}
		qty := plan[st.Next]
		if qty.GreaterThan(remaining) {
			qty = remaining
		}
		if !qty.IsPositive() {
			continue
		}
		mid, err := e.mid(ctx, p.Symbol)
		if err != nil {
			e.logf2("parent %d mid resolve slice %d: %v", p.ID, st.Next, err)
			continue // no market price — this slice waits for next boundary
		}
		price := mid.Add(discretionOffset(r, p.Side, st.DiscrePips, pip))
		child := &Child{
			AlgoOrderID: p.ID, Seq: next, SliceIndex: st.Next,
			Role: RoleSlice, Symbol: p.Symbol, Side: p.Side,
			Qty: qty, Price: &price,
		}
		next++
		stored, err := e.store.InsertChild(ctx, child)
		if err != nil {
			return errInternal("child intent", err)
		}
		st.save(ctx, e, p.ID) // durable cursor BEFORE dispatch
		disp, err := e.dispatchChild(ctx, p, stored, "GTC")
		if err != nil {
			// Real pipeline rejection — remainder conservation keeps the
			// schedule exact; the slice's portion rolls forward.
			e.logf2("parent %d slice %d rejected: %v", p.ID, st.Next, err)
			continue
		}
		open = disp
	}
	return e.closeOut(ctx, p, open)
}

func isLiveChild(c *Child) bool {
	switch c.Status {
	case ChildPending, ChildSubmitted, ChildOpen, ChildPartial:
		return true
	}
	return false
}

// closeOut settles the final open slice, recomputes fills, and lands the
// terminal state: EXPIRED when the deadline passed, COMPLETED otherwise.
func (e *Engine) closeOut(ctx context.Context, p *Parent, open *Child) error {
	if open != nil && isLiveChild(open) {
		if err := e.cancelChild(ctx, p, open); err != nil {
			e.logf2("parent %d final cancel: %v", p.ID, err)
		}
	}
	if status, err := e.statusOf(ctx, p.ID); err == nil && isTerminalStatus(status) {
		return nil // cancel/expiry raced in — respect the terminal row
	}
	if _, err := e.aggregateFilled(ctx, p.ID); err != nil {
		e.logf2("parent %d fill aggregate: %v", p.ID, err)
	}
	cur, _ := e.store.GetParent(ctx, p.ID)
	if cur != nil && cur.ExpiresAt != nil && !e.now().Before(*cur.ExpiresAt) {
		e.finish(ctx, p.ID, StatusExpired, "duration elapsed")
		return nil
	}
	// Schedule done — a PAUSED parent also completes rather than
	// stranding without a driver.
	e.finish(ctx, p.ID, StatusCompleted, "schedule complete")
	return nil
}

func decodeSliceState(p *Parent) *sliceState {
	st := &sliceState{}
	if len(p.State) == 0 {
		return nil
	}
	if err := json.Unmarshal(p.State, st); err != nil {
		return nil
	}
	if len(st.Qtys) == 0 || len(st.BoundsMs) == 0 {
		return nil
	}
	return st
}

// materializeSliceState returns the stored plan or builds a fresh
// randomized one (first driver entry) and persists it — crash recovery
// resumes the SAME schedule.
func materializeSliceState(ctx context.Context, e *Engine, p *Parent,
	discrePips int64, pipSize decimal.Decimal,
	build func(r *rand.Rand) ([]decimal.Decimal, []time.Duration, error)) (*sliceState, error) {
	if st := decodeSliceState(p); st != nil {
		return st, nil
	}
	qtys, bounds, err := build(newPRNG())
	if err != nil {
		return nil, err
	}
	st := &sliceState{Start: e.now(), Next: 0, DiscrePips: discrePips,
		PipSize: pipSize.String()}
	for _, q := range qtys {
		st.Qtys = append(st.Qtys, q.String())
	}
	for _, d := range bounds {
		st.BoundsMs = append(st.BoundsMs, d.Milliseconds())
	}
	st.save(ctx, e, p.ID)
	return st, nil
}
