package pamm

import (
	"context"
	"fmt"
	"time"

	"exchange/pkg/decimal"
)

// MasterFill is the engine-visible shape of a pool/master fill already
// resolved from the event stream (see doc.go § event-source decision: the
// engine consumes the NATS trades feed the settlements bridge already
// publishes — no second engine subscription, no new hot-path surface).
type MasterFill struct {
	// MasterAccountID is the pool account (or strategy manager account for
	// copy trading — the copy layer resolves strategy → weight set itself).
	MasterAccountID int64
	TradeID         int64
	InstrumentID    int64
	Side            string // BUY | SELL
	Quantity        string
	Price           string
}

// FillFanOut reports one fan-out outcome.
type FillFanOut struct {
	PoolID      int64            `json:"pool_id,omitempty"`
	TradeID     int64            `json:"trade_id"`
	Allocations int              `json:"allocations"` // child rows written this call
	Duplicate   bool             `json:"duplicate"`   // replay detected — nothing written
	Skipped     []SkippedChild   `json:"skipped"`     // children below minimum notional
	Quantities  map[int64]string `json:"quantities"`  // allocation_id → qty (observability)
	ProcessedAt string           `json:"processed_at"`
}

// SkippedChild is a durable record payload for a child that could not be
// executed — never silently dropped (spec F5).
type SkippedChild struct {
	ChildID int64  `json:"child_id"` // allocation_id or follow_id
	Reason  string `json:"reason"`   // below_min_notional | trade_type_unsupported | ...
	Detail  string `json:"detail"`
}

// SkipRecorder persists/emits skipped-child notices. Production binds the
// notification service; nil = skips still return in Skipped (records are
// also stored on the durable rows when the store supports it).
type SkipRecorder interface {
	RecordSkip(ctx context.Context, masterAccountID int64, ev SkippedChild) error
}

// Engine drives the fill fan-out. The PAMM path writes fill allocations;
// the copy layer supplies its own weight set via AllocateForShares.
type Engine struct {
	store    Store
	recorder SkipRecorder
}

// EngineOption configures optional dependencies.
type EngineOption func(*Engine)

// WithSkipRecorder binds the durable skip-notice sink.
func WithSkipRecorder(r SkipRecorder) EngineOption { return func(e *Engine) { e.recorder = r } }

// NewEngine wires the fan-out. Store is mandatory — fail closed.
func NewEngine(store Store, opts ...EngineOption) (*Engine, error) {
	if store == nil {
		return nil, fmt.Errorf("pamm: engine requires a store (fail closed)")
	}
	e := &Engine{store: store}
	for _, o := range opts {
		o(e)
	}
	return e, nil
}

// OnPoolFill fans a master fill out over the pool's ACTIVE allocations.
// Idempotent per (master_trade_id, allocation_id): replays report
// Duplicate instead of double-writing.
func (e *Engine) OnPoolFill(ctx context.Context, f MasterFill) (*FillFanOut, error) {
	qty, err := parsePositive(f.Quantity, "quantity")
	if err != nil {
		return nil, err
	}
	price, err := parsePositive(f.Price, "price")
	if err != nil {
		return nil, err
	}
	pool, err := e.store.PoolByAccountID(ctx, f.MasterAccountID)
	if err != nil {
		return nil, err
	}
	if pool == nil {
		return nil, errorf(CodeNotFound,
			"master account %d is not a PAMM pool account", f.MasterAccountID)
	}
	allocs, err := e.store.ActiveAllocations(ctx, pool.PoolID)
	if err != nil {
		return nil, err
	}
	out := &FillFanOut{PoolID: pool.PoolID, TradeID: f.TradeID,
		Quantities: map[int64]string{}, ProcessedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	if len(allocs) == 0 {
		out.Duplicate = true // nothing to do — recorded as a no-op fan-out
		return out, nil
	}
	shares := make([]Share, len(allocs))
	for i, a := range allocs {
		shares[i] = Share{ID: a.AllocationID, Weight: a.Invested}
	}
	res, err := AllocateProRata(qty, shares)
	if err != nil {
		return nil, err
	}
	byID := make(map[int64]Allocation, len(allocs))
	for _, a := range allocs {
		byID[a.AllocationID] = a
	}
	rows := make([]FillAllocation, 0, len(res))
	for _, r := range res {
		if r.Quantity.IsZero() {
			continue
		}
		a := byID[r.ID]
		rows = append(rows, FillAllocation{
			PoolID: pool.PoolID, MasterTradeID: f.TradeID,
			AllocationID: a.AllocationID, InvestorAccountID: a.InvestorAccountID,
			InstrumentID: f.InstrumentID, Side: f.Side,
			Quantity: r.Quantity, Price: price,
		})
	}
	// Conservation double-check before persistence — the allocator
	// guarantees Σ == master qty; re-verify at the engine boundary so a
	// store-side surprise can never persist a diverging child set
	// (fail closed, PAMM_ALLOCATION_MISMATCH per the §27.1 matrix).
	var checkSum decimal.Decimal
	for _, r := range res {
		checkSum = checkSum.Add(r.Quantity)
	}
	if !checkSum.Equal(qty) {
		return nil, errorf(CodeAllocationMismatch,
			"pro-rata sum %s != master qty %s for trade %d", checkSum, qty, f.TradeID)
	}
	applied, err := e.store.InsertFillAllocations(ctx, rows)
	if err != nil {
		return nil, err
	}
	out.Allocations = applied
	if applied == 0 && len(rows) > 0 {
		out.Duplicate = true
	}
	for _, r := range res {
		out.Quantities[r.ID] = r.Quantity.String()
	}
	return out, nil
}

func parsePositive(raw, field string) (decimal.Decimal, error) {
	d, err := decimal.NewFromString(raw)
	if err != nil || !d.IsPositive() {
		return decimal.Zero, errorf(CodeInvalidRequest,
			"fill %s %q must be a positive decimal", field, raw)
	}
	return d, nil
}
