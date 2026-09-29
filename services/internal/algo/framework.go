// Task 16.3.8 — the algo order framework.
//
// Engine owns the parent lifecycle: NEW → PENDING (delayed dispatch) →
// RUNNING ⇄ PAUSED → COMPLETED/CANCELLED/EXPIRED/FAILED. Parent state is
// durable in algo_orders (migration 224); each RUNNING parent gets a
// driver goroutine that re-reads authoritative state from the store on
// every iteration, so API-driven pause/cancel and crash recovery both
// converge on the row, not on in-memory copies.
//
// Children are dispatched ONLY through the ChildExecutor seam — wired to
// the real orders.Service pipeline in cmd/gateway (admission gates, risk
// checks, balance sufficiency, idempotent dedup all still apply).
// Fill observation reuses the pipeline's read model: the engine's
// out-ring consumer already folds TradeFill into orders.filled_qty /
// status, so ChildStatus reads order status rather than opening a
// second event tap (documented seam choice — Task 16.3.8 allows either
// the fill consumer or order status; order status is the durable view).
package algo

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"exchange/pkg/decimal"
	excerrors "exchange/pkg/errors"
)

// ---------------------------------------------------------------------------
// Order-pipeline seam
// ---------------------------------------------------------------------------

// ChildRequest is the driver → order-pipeline dispatch payload. Children
// are always LIMIT orders — MARKET algo children would bypass the
// price-band checks the strategies rely on for discretion.
type ChildRequest struct {
	Symbol        string
	Side          string
	TimeInForce   string // "GTC" (resting slice) | "IOC" (VP/close-out)
	Quantity      decimal.Decimal
	Price         decimal.Decimal // limit price — always set
	ClientOrderID string          // derived idempotency key
	PostOnly      bool            // spread legs never set; mid slices may
}

// ChildStatus mirrors the order-pipeline read model for one child.
type ChildStatus struct {
	Status       string // orders.status: PENDING/ACTIVE/PARTIALLY_FILLED/FILLED/CANCELLED/REJECTED/EXPIRED
	FilledQty    decimal.Decimal
	AvgFillPrice *decimal.Decimal
}

// ChildExecutor is the seam into the Phase-05 order pipeline.
// Production binds orders.Service + orders.PgStore via the cmd/gateway
// adapter; tests fake it. Every call inherits the pipeline's admission,
// risk, balance and idempotency machinery — the framework NEVER writes
// orders directly.
type ChildExecutor interface {
	// SubmitChild dispatches a child order. Returns the order id;
	// a coded error (INSUFFICIENT_BALANCE, TRADING_HALTED, …) is a real
	// pipeline rejection, never retried silently by the driver.
	SubmitChild(ctx context.Context, accountID int64, req ChildRequest) (orderID int64, err error)
	// CancelChild cancels a live child order through the pipeline's
	// engine-confirmation path.
	CancelChild(ctx context.Context, accountID, orderID int64) error
	// ChildStatus reads the pipeline's durable order state.
	ChildStatus(ctx context.Context, orderID int64) (*ChildStatus, error)
}

// ---------------------------------------------------------------------------
// Engine
// ---------------------------------------------------------------------------

// Driver runs one parent to a terminal state. It must tolerate
// cancellation (ctx.Done — shutdown) and re-read parent state each
// iteration so PAUSED/CANCELLED transitions land promptly.
type Driver interface {
	Run(ctx context.Context, e *Engine, parentID int64) error
}

// Options wires the engine's seams.
type Options struct {
	Store      Store
	Exec       ChildExecutor       // nil → dispatch fails closed (SERVICE_DEGRADED)
	Quote      QuoteSource         // nil → mid resolution fails closed
	Ref        RefSource           // nil → no reference fallback
	Pips       PipSizeSource       // instruments.pip_size for discretion bands
	Profiles   VolumeProfileSource // nil → VWAP uses the flat fallback
	Volume     VolumeSource        // VP observed-volume seam; nil → VP submits rejected
	Drivers    map[string]Driver   // algo_type → driver; nil → builtin set
	Race       *RaceGuard          // Task 16.3.22 — nil → race reconcile skipped (child rows still cancel; audit flagging degraded)
	SweepEvery time.Duration       // delayed-dispatch + adopt sweep (default 500ms)
	Now        func() time.Time
	Logf       func(string, ...any)
}

// Engine is the algo framework service.
type Engine struct {
	store    Store
	exec     ChildExecutor
	quote    QuoteSource
	ref      RefSource
	pips     PipSizeSource
	profiles VolumeProfileSource
	volume   VolumeSource
	race     *RaceGuard
	drivers  map[string]Driver
	sweep    time.Duration
	now      func() time.Time
	logf     func(string, ...any)

	mu      sync.Mutex
	handles map[int64]*driverHandle
	started bool
}

type driverHandle struct {
	wake   chan struct{}
	cancel context.CancelFunc
}

func NewEngine(o Options) (*Engine, error) {
	if o.Store == nil {
		return nil, fmt.Errorf("algo: store is nil")
	}
	e := &Engine{
		store: o.Store, exec: o.Exec, quote: o.Quote, ref: o.Ref,
		pips: o.Pips, profiles: o.Profiles, volume: o.Volume,
		race:  o.Race,
		sweep: o.SweepEvery, now: o.Now, logf: o.Logf,
		drivers: map[string]Driver{}, handles: map[int64]*driverHandle{},
	}
	if e.sweep <= 0 {
		e.sweep = 500 * time.Millisecond
	}
	if e.now == nil {
		e.now = time.Now
	}
	if e.logf == nil {
		e.logf = func(string, ...any) {}
	}
	for t, d := range o.Drivers {
		e.drivers[t] = d
	}
	// Builtin strategies — SPREAD executes synchronously inside Submit
	// (IOC legs + compensate-on-fail) and has no driver; SCALE gets the
	// ladder driver; TWAP/VWAP share the interval slicer; VP the
	// participation driver.
	if _, ok := e.drivers[TypeTWAP]; !ok {
		e.drivers[TypeTWAP] = &twapDriver{}
	}
	if _, ok := e.drivers[TypeVWAP]; !ok {
		e.drivers[TypeVWAP] = &vwapDriver{}
	}
	if _, ok := e.drivers[TypeVP]; !ok {
		e.drivers[TypeVP] = &vpDriver{}
	}
	if _, ok := e.drivers[TypeScaled]; !ok {
		e.drivers[TypeScaled] = &scaledDriver{}
	}
	return e, nil
}

func (e *Engine) logf2(f string, a ...any) { e.logf("algo: "+f, a...) }

// ---------------------------------------------------------------------------
// Submit — POST /api/v1/orders/algo (+ typed endpoints fold into it)
// ---------------------------------------------------------------------------

// Submit validates the request, persists the NEW parent (idempotent on
// client_order_id) and activates it: future start_at → PENDING under the
// delayed-dispatch sweeper; otherwise RUNNING with a spawned driver.
// SPREAD parents short-circuit — leg validation + execution are
// synchronous (see spread.go).
func (e *Engine) Submit(ctx context.Context, accountID int64, req *SubmitRequest) (*Parent, error) {
	p, err := e.validateSubmit(ctx, accountID, req)
	if err != nil {
		return nil, err
	}
	if p.Type == TypeSpread {
		// Market-spread invariant BEFORE any row — a rejected spread
		// leaves no durable residue (same convention as order rejects).
		if err := e.precheckSpread(ctx, req); err != nil {
			return nil, err
		}
	}
	stored, dup, err := e.store.InsertParent(ctx, p)
	if err != nil {
		return nil, errInternal("algo parent insert", err)
	}
	if dup {
		return stored, nil // §8.7 idempotent replay — stored parent, no resubmit
	}
	if p.Type == TypeSpread {
		// Synchronous path: the parent's terminal state is produced
		// inside the request (NEW→RUNNING→COMPLETED/FAILED).
		return e.executeSpread(ctx, stored)
	}
	e.activate(stored)
	return stored, nil
}

func errInternal(what string, err error) *excerrors.Error {
	return codeErr("INTERNAL_ERROR", "algo %s: %v", what, err)
}

// validateSubmit runs the shared + per-type validation and produces the
// parent row to insert (params normalized to canonical JSON).
func (e *Engine) validateSubmit(ctx context.Context, accountID int64,
	req *SubmitRequest) (*Parent, error) {
	if accountID <= 0 {
		return nil, codeErr("UNAUTHORIZED", "account context required")
	}
	if req == nil {
		return nil, codeErr("INVALID_REQUEST", "empty request")
	}
	req.AlgoType = strings.ToUpper(strings.TrimSpace(req.AlgoType))
	req.Side = strings.ToUpper(strings.TrimSpace(req.Side))
	req.Symbol = strings.ToUpper(strings.TrimSpace(req.Symbol))
	p := &Parent{
		AccountID: accountID, Type: req.AlgoType, Symbol: req.Symbol,
		Side: req.Side, TotalQty: req.TotalQty, StartAt: req.StartAt,
		ClientOrderID: req.ClientOrderID,
	}
	// Per-type validators return normalized params + an optional TTL
	// (mapped to expires_at — the terminal EXPIRED deadline).
	var ttl *time.Duration
	switch req.AlgoType {
	case TypeTWAP:
		prm, t, err := e.validateTWAP(req)
		if err != nil {
			return nil, err
		}
		p.Params, ttl = prm, t
	case TypeVWAP:
		prm, t, err := e.validateVWAP(req)
		if err != nil {
			return nil, err
		}
		p.Params, ttl = prm, t
	case TypeVP:
		prm, t, err := e.validateVP(req)
		if err != nil {
			return nil, err
		}
		p.Params, ttl = prm, t
	case TypeScaled:
		prm, t, err := e.validateScaled(req)
		if err != nil {
			return nil, err
		}
		p.Params, ttl = prm, t
	case TypeSpread:
		prm, t, err := e.validateSpread(req)
		if err != nil {
			return nil, err
		}
		p.Params, ttl = prm, t
	default:
		return nil, codeErr("INVALID_REQUEST",
			"unknown algo_type %q (want TWAP|VWAP|VP|SCALE|SPREAD)", req.AlgoType)
	}
	if ttl != nil && *ttl > 0 {
		start := e.now()
		if p.StartAt != nil && p.StartAt.After(start) {
			start = *p.StartAt
		}
		exp := start.Add(*ttl)
		p.ExpiresAt = &exp
	}
	if req.AlgoType == TypeSpread {
		// total_qty has no single-instrument meaning for a spread —
		// carry leg1's quantity so the CHECK (total_qty > 0) holds and
		// progress reads sensibly.
		var sp SpreadParams
		if err := json.Unmarshal(p.Params, &sp); err == nil && len(sp.Legs) == 2 {
			if q, qerr := decimal.NewFromString(sp.Legs[0].Quantity); qerr == nil {
				p.TotalQty = q
			}
			p.Symbol = strings.ToUpper(sp.Legs[0].Symbol)
			p.Side = sp.Legs[0].Side
		}
	}
	if !p.TotalQty.IsPositive() {
		return nil, codeErr("INVALID_REQUEST", "total_qty must be positive")
	}
	if p.StartAt != nil && p.StartAt.After(e.now().Add(maxScheduleDelay)) {
		return nil, codeErr("INVALID_REQUEST",
			"start_at beyond the maximum scheduling horizon (%s)", maxScheduleDelay)
	}
	_ = ctx
	return p, nil
}

// maxScheduleDelay bounds delayed dispatch so a typo can't park an algo
// for a year — 24h covers all documented use (session rolls, fixings).
const maxScheduleDelay = 24 * time.Hour

// activate transitions the parent out of NEW: delayed start → PENDING
// (the sweeper promotes at start_at); else RUNNING + driver spawn.
// A PAUSED insert-time state is impossible; the CAS guards a cancel
// that raced activation — a cancelled parent never spawns a driver.
func (e *Engine) activate(p *Parent) {
	if p.StartAt != nil && p.StartAt.After(e.now()) {
		if ok, err := e.store.CASStatus(context.Background(), p.ID,
			[]string{StatusNew}, StatusPending, ""); err != nil {
			e.logf2("activate parent %d: %v", p.ID, err)
			return
		} else if !ok {
			return // raced to terminal
		}
		e.nudge() // sweeper wakes early to schedule precisely
		return
	}
	if ok, err := e.store.CASStatus(context.Background(), p.ID,
		[]string{StatusNew, StatusPending}, StatusRunning, ""); err != nil {
		e.logf2("activate parent %d: %v", p.ID, err)
		return
	} else if !ok {
		return // raced to terminal — never spawn
	}
	e.spawn(p)
}

// spawn starts the driver's goroutine and registers its handle.
func (e *Engine) spawn(p *Parent) {
	d, ok := e.drivers[p.Type]
	if !ok {
		e.logf2("no driver for algo_type %s (parent %d)", p.Type, p.ID)
		_, _ = e.store.CASStatus(context.Background(), p.ID,
			[]string{StatusRunning}, StatusFailed,
			"no driver registered for algo_type "+p.Type)
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	h := &driverHandle{wake: make(chan struct{}, 1), cancel: cancel}
	e.mu.Lock()
	e.handles[p.ID] = h
	e.mu.Unlock()
	go func() {
		defer func() {
			if r := recover(); r != nil {
				e.logf2("driver panic parent %d: %v", p.ID, r)
			}
			e.mu.Lock()
			delete(e.handles, p.ID)
			e.mu.Unlock()
			cancel()
		}()
		if err := d.Run(ctx, e, p.ID); err != nil {
			e.logf2("driver parent %d exited: %v", p.ID, err)
		}
	}()
}

// wake nudges a running driver's loop (pause/resume/cancel dispatch).
func (e *Engine) wake(parentID int64) {
	e.mu.Lock()
	h, ok := e.handles[parentID]
	e.mu.Unlock()
	if ok {
		select {
		case h.wake <- struct{}{}:
		default:
		}
	}
}

// nudge wakes the delayed-dispatch sweeper early.
func (e *Engine) nudge() {
	// The sweeper polls on e.sweep; a nudge channel would shave latency
	// for far-future start_at — the ≤500ms poll already bounds it.
}

// ---------------------------------------------------------------------------
// Pause / Resume / Cancel
// ---------------------------------------------------------------------------

// ownedParent resolves the parent and enforces account ownership —
// same convention as orders.Service.Cancel (foreign ids are NOT_FOUND).
func (e *Engine) ownedParent(ctx context.Context, accountID, id int64) (*Parent, error) {
	p, err := e.store.GetParent(ctx, id)
	if err != nil {
		return nil, errInternal("parent read", err)
	}
	if p == nil || p.AccountID != accountID {
		return nil, codeErr("ORDER_NOT_FOUND", "algo order %d not found", id)
	}
	return p, nil
}

// Pause halts child dispatch (RUNNING → PAUSED) and cancels the parent's
// open child slices — a paused algo must not leave live market exposure.
// The state row is untouched; resume continues from the same cursor.
func (e *Engine) Pause(ctx context.Context, accountID, id int64) (*Parent, error) {
	p, err := e.ownedParent(ctx, accountID, id)
	if err != nil {
		return nil, err
	}
	ok, err := e.store.CASStatus(ctx, id, []string{StatusRunning}, StatusPaused, "")
	if err != nil {
		return nil, errInternal("pause", err)
	}
	if !ok {
		return nil, codeErr("INVALID_REQUEST",
			"algo order %d in state %s cannot be paused", id, p.Status)
	}
	// Quiesce live children — pause must leave nothing resting.
	if cerr := e.cancelOpenChildren(ctx, id, "parent paused"); cerr != nil {
		e.logf2("pause parent %d: child cancel: %v", id, cerr)
	}
	e.wake(id)
	p.Status = StatusPaused
	return p, nil
}

// Resume restarts dispatch on a PAUSED parent.
func (e *Engine) Resume(ctx context.Context, accountID, id int64) (*Parent, error) {
	p, err := e.ownedParent(ctx, accountID, id)
	if err != nil {
		return nil, err
	}
	ok, err := e.store.CASStatus(ctx, id, []string{StatusPaused}, StatusRunning, "")
	if err != nil {
		return nil, errInternal("resume", err)
	}
	if !ok {
		return nil, codeErr("INVALID_REQUEST",
			"algo order %d in state %s cannot be resumed", id, p.Status)
	}
	e.wake(id)
	p.Status = StatusRunning
	return p, nil
}

// Cancel terminates the parent and every live child slice through the
// order pipeline's cancel path — zero orphan slices (spec §6.10).
func (e *Engine) Cancel(ctx context.Context, accountID, id int64,
	reason string) (*Parent, error) {
	p, err := e.ownedParent(ctx, accountID, id)
	if err != nil {
		return nil, err
	}
	if isTerminalStatus(p.Status) {
		return p, nil // idempotent: already terminal
	}
	if err := e.cancelOpenChildren(ctx, id, "parent cancelled"); err != nil {
		return nil, errInternal("cancel children", err)
	}
	ok, err := e.store.CASStatus(ctx, id,
		[]string{StatusNew, StatusPending, StatusRunning, StatusPaused},
		StatusCancelled, reason)
	if err != nil {
		return nil, errInternal("cancel", err)
	}
	if !ok {
		// Raced to terminal between read and CAS — report current state.
		cur, rerr := e.store.GetParent(ctx, id)
		if rerr == nil && cur != nil {
			return cur, nil
		}
		return nil, codeErr("INVALID_REQUEST",
			"algo order %d could not transition to CANCELLED", id)
	}
	// Task 16.3.22 — deterministic race reconcile AFTER the terminal CAS:
	// child fills sequenced before the cancel are kept (fill-wins), late
	// fill audits are flagged OCO_SIBLING_CANCEL_RACE, never undone.
	if e.race != nil {
		if _, rerr := e.race.ReconcileParent(ctx, id); rerr != nil {
			e.logf2("raceguard reconcile parent %d: %v", id, rerr)
		}
	}
	e.wake(id)
	if h := e.handle(id); h != nil {
		h.cancel() // driver exits promptly; handle cleanup on defer
	}
	p.Status = StatusCancelled
	return p, nil
}

func (e *Engine) handle(id int64) *driverHandle {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.handles[id]
}

// List returns the account's parents (status filter optional, newest
// first) — the GET /algo-orders read model.
func (e *Engine) List(ctx context.Context, accountID int64,
	status string, limit int) ([]Parent, error) {
	parents, err := e.store.ListParents(ctx, accountID, status, limit)
	if err != nil {
		return nil, errInternal("parent list", err)
	}
	return parents, nil
}

// CancelAll terminates every non-terminal parent of the account —
// DELETE /api/v1/algo-orders (Task 16.3.23). symbol != "" scopes the
// sweep to one instrument. Every parent rides the same Cancel path as
// a single cancel: children terminate through the owning engine path
// and the Task 16.3.22 race reconcile settles the child's final row —
// zero orphan slices (§24 #365).
func (e *Engine) CancelAll(ctx context.Context, accountID int64,
	symbol string) (int, error) {
	parents, err := e.store.ListParents(ctx, accountID, "", 1000)
	if err != nil {
		return 0, errInternal("parent list", err)
	}
	n := 0
	for i := range parents {
		if isTerminalStatus(parents[i].Status) {
			continue
		}
		if symbol != "" && parents[i].Symbol != symbol {
			continue
		}
		if _, err := e.Cancel(ctx, accountID, parents[i].ID, "cancel-all"); err != nil {
			e.logf2("cancel-all parent %d: %v", parents[i].ID, err)
			continue
		}
		n++
	}
	return n, nil
}

// Get returns the parent plus its child rows — the detail view.
func (e *Engine) Get(ctx context.Context, accountID, id int64) (*Parent, []Child, error) {
	p, err := e.ownedParent(ctx, accountID, id)
	if err != nil {
		return nil, nil, err
	}
	children, err := e.store.Children(ctx, id)
	if err != nil {
		return nil, nil, errInternal("children read", err)
	}
	return p, children, nil
}

// ---------------------------------------------------------------------------
// Run — delayed-dispatch sweeper + crash adoption
// ---------------------------------------------------------------------------

// Run adopts every non-terminal parent (restart recovery — the durable
// rows outlive the process) then sweeps PENDING parents whose start_at
// has arrived into RUNNING. Blocks until ctx is cancelled.
func (e *Engine) Run(ctx context.Context) {
	e.mu.Lock()
	if e.started {
		e.mu.Unlock()
		return
	}
	e.started = true
	e.mu.Unlock()

	// Adopt: RUNNING/PAUSED parents respawn drivers; PENDING parents
	// with an elapsed start_at promote immediately.
	actives, err := e.store.ActiveParents(ctx)
	if err != nil {
		e.logf2("adopt active parents: %v", err)
	} else {
		for i := range actives {
			p := actives[i]
			switch p.Status {
			case StatusPending:
				if p.StartAt == nil || !p.StartAt.After(e.now()) {
					e.activate(&p)
				}
			default:
				e.spawn(&p) // RUNNING or PAUSED — driver waits while paused
			}
		}
		e.logf2("adopted %d active parents", len(actives))
	}

	tick := time.NewTicker(e.sweep)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		due, err := e.store.PendingDue(ctx, e.now(), 64)
		if err != nil {
			e.logf2("pending sweep: %v", err)
			continue
		}
		for i := range due {
			p := due[i]
			if ok, err := e.store.CASStatus(ctx, p.ID,
				[]string{StatusPending}, StatusRunning, ""); err != nil {
				e.logf2("promote parent %d: %v", p.ID, err)
			} else if ok {
				e.spawn(&p)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Driver-facing helpers — every driver shares these primitives
// ---------------------------------------------------------------------------

// pipSize resolves the discretion-band unit for a symbol; a missing
// seam or unknown symbol yields a zero size (discretion collapses to
// exactly mid — never fabricated).
func (e *Engine) pipSize(ctx context.Context, symbol string) decimal.Decimal {
	if e.pips == nil {
		return decimal.Zero
	}
	p, err := e.pips.PipSize(ctx, symbol)
	if err != nil || !p.IsPositive() {
		return decimal.Zero
	}
	return p
}

// statusOf re-reads the parent's authoritative row.
func (e *Engine) statusOf(ctx context.Context, id int64) (string, error) {
	p, err := e.store.GetParent(ctx, id)
	if err != nil {
		return "", err
	}
	if p == nil {
		return "", fmt.Errorf("algo parent %d vanished", id)
	}
	return p.Status, nil
}

// mid resolves the mid price for symbol: (best bid + best ask)/2 from
// the persisted book; when a book side is absent the last-trade
// reference stands in (documented fallback — the alternative, skipping
// the slice, stalls the schedule; the reference is still a real market
// price). Both missing → error, driver retries next tick.
func (e *Engine) mid(ctx context.Context, symbol string) (decimal.Decimal, error) {
	if e.quote != nil {
		bid, ask, ok, err := e.quote.BestBidAsk(ctx, symbol)
		if err != nil {
			return decimal.Zero, fmt.Errorf("quote: %w", err)
		}
		if ok {
			return bid.Add(ask).Div(decimal.NewFromInt(2)), nil
		}
	}
	if e.ref != nil {
		ref, err := e.ref.ReferencePrice(ctx, symbol)
		if err != nil {
			return decimal.Zero, fmt.Errorf("reference: %w", err)
		}
		if ref != nil && ref.IsPositive() {
			return *ref, nil
		}
	}
	return decimal.Zero, fmt.Errorf("no book top or reference price for %s", symbol)
}

// dispatchChild sends one durable child row through the order pipeline.
// The row is PENDING-persisted BEFORE SubmitChild so a crash between
// insert and dispatch leaves a replayable intent carrying the derived
// client_order_id. Returns the updated child (SUBMITTED + order_id, or
// REJECTED + detail on a coded pipeline rejection).
func (e *Engine) dispatchChild(ctx context.Context, p *Parent, c *Child,
	tif string) (*Child, error) {
	if e.exec == nil {
		return nil, codeErr("SERVICE_DEGRADED",
			"order pipeline executor unwired — child dispatch refused (fail closed)")
	}
	now := e.now().UTC()
	c.DispatchedAt = &now
	oid, err := e.exec.SubmitChild(ctx, p.AccountID, ChildRequest{
		Symbol:        c.Symbol,
		Side:          c.Side,
		TimeInForce:   tif,
		Quantity:      c.Qty,
		Price:         derefOr(c.Price, decimal.Zero),
		ClientOrderID: c.ClientOrderID,
	})
	if err != nil {
		c.Status = ChildRejected
		c.Detail = err.Error()
		c.ResolvedAt = &now
		if uerr := e.store.UpdateChild(ctx, c); uerr != nil {
			e.logf2("child %d reject-mark failed: %v", c.ID, uerr)
		}
		return c, err
	}
	c.OrderID = &oid
	c.Status = ChildSubmitted
	if uerr := e.store.UpdateChild(ctx, c); uerr != nil {
		return c, uerr
	}
	return c, nil
}

func derefOr(d *decimal.Decimal, def decimal.Decimal) decimal.Decimal {
	if d == nil {
		return def
	}
	return *d
}

// refreshChild folds the pipeline's order state into the child row and
// returns whether the child is terminal.
func (e *Engine) refreshChild(ctx context.Context, c *Child) (bool, error) {
	if c.OrderID == nil || e.exec == nil {
		return c.Status == ChildRejected, nil
	}
	st, err := e.exec.ChildStatus(ctx, *c.OrderID)
	if err != nil {
		return false, err
	}
	c.FilledQty = st.FilledQty
	now := e.now().UTC()
	switch st.Status {
	case "FILLED":
		c.Status = ChildFilled
		c.ResolvedAt = &now
		return true, nil
	case "PARTIALLY_FILLED":
		c.Status = ChildPartial
		return false, nil
	case "CANCELLED", "EXPIRED":
		c.Status = ChildCancelled
		c.ResolvedAt = &now
		return true, nil
	case "REJECTED":
		c.Status = ChildRejected
		c.ResolvedAt = &now
		return true, nil
	default: // PENDING / RESERVED / ACTIVE
		c.Status = ChildOpen
		return false, nil
	}
}

// cancelChild cancels one live child through the pipeline and settles
// the row's terminal state.
func (e *Engine) cancelChild(ctx context.Context, p *Parent, c *Child) error {
	if c.OrderID == nil {
		c.Status = ChildCancelled
		now := e.now().UTC()
		c.ResolvedAt = &now
		return e.store.UpdateChild(ctx, c)
	}
	if err := e.exec.CancelChild(ctx, p.AccountID, *c.OrderID); err != nil {
		return err
	}
	_, err := e.refreshChild(ctx, c)
	if c.Status != ChildFilled {
		c.Status = ChildCancelled
		now := e.now().UTC()
		c.ResolvedAt = &now
	}
	return err
}

// cancelOpenChildren terminates every non-terminal child — used by
// parent cancel and pause. Returns the first error (best-effort per
// child; others still cancelled).
func (e *Engine) cancelOpenChildren(ctx context.Context, parentID int64,
	reason string) error {
	p := &Parent{ID: parentID}
	// AccountID needed for the cancel path — resolve once.
	full, err := e.store.GetParent(ctx, parentID)
	if err != nil {
		return err
	}
	if full != nil {
		p = full
	}
	open, err := e.store.OpenChildren(ctx, parentID)
	if err != nil {
		return err
	}
	var first error
	for i := range open {
		c := open[i]
		if err := e.cancelChild(ctx, p, &c); err != nil {
			if first == nil {
				first = err
			}
			c.Detail = reason + ": " + err.Error()
			c.Status = ChildCancelled // mark cancelled locally; engine state reconciles via refresh
			now := e.now().UTC()
			c.ResolvedAt = &now
		} else if c.Detail == "" {
			c.Detail = reason
		}
		_ = e.store.UpdateChild(ctx, &c)
	}
	return first
}

// nextChildSeq reads the highest used child seq for the parent —
// state.carry cursor for crash recovery (children rows are the durable
// record; seq allocation resumes after the max).
func (e *Engine) nextChildSeq(ctx context.Context, parentID int64) (int, error) {
	children, err := e.store.Children(ctx, parentID)
	if err != nil {
		return 0, err
	}
	max := 0
	for _, c := range children {
		if c.Seq >= max {
			max = c.Seq + 1
		}
	}
	return max, nil
}

// aggregateFilled recomputes the parent's filled_qty from children rows
// and persists it — the parent-level progress figure.
func (e *Engine) aggregateFilled(ctx context.Context, parentID int64) (decimal.Decimal, error) {
	children, err := e.store.Children(ctx, parentID)
	if err != nil {
		return decimal.Zero, err
	}
	total := decimal.Zero
	for _, c := range children {
		if c.Role == RoleUnwind {
			total = total.Sub(c.FilledQty) // unwind legs offset parent fills
			continue
		}
		total = total.Add(c.FilledQty)
	}
	if err := e.store.SetFilled(ctx, parentID, total); err != nil {
		return total, err
	}
	return total, nil
}

// finish transitions the parent to a terminal state with a reason —
// no-op when the row already raced terminal (CAS guard).
func (e *Engine) finish(ctx context.Context, id int64, to, detail string) {
	ok, err := e.store.CASStatus(ctx, id,
		[]string{StatusNew, StatusPending, StatusRunning, StatusPaused},
		to, detail)
	if err != nil {
		e.logf2("finish parent %d → %s: %v", id, to, err)
	} else if !ok {
		e.logf2("finish parent %d → %s: already terminal", id, to)
	}
}

// stateMap decodes parent.State into a mutable map for a driver.
func stateMap(raw json.RawMessage) map[string]any {
	m := map[string]any{}
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &m)
	}
	return m
}
