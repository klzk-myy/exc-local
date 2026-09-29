package copy

import (
	"context"
	"fmt"
	"time"

	"exchange/internal/pamm"
	"exchange/pkg/decimal"
)

// ChildSubmitter is the order-submission seam — production wraps
// orders.Service.Submit (resolved symbol + account context); tests fake
// it. Returning (0, nil) means "accepted, id pending" — the child row is
// marked SUBMITTED either way; submission errors mark REJECTED.
type ChildSubmitter interface {
	SubmitChild(ctx context.Context, investorAccountID int64,
		req ChildOrderRequest) (orderID int64, err error)
}

// ChildOrderRequest is the engine → order-pipeline payload.
type ChildOrderRequest struct {
	InstrumentID int64
	Side         string // BUY | SELL
	Quantity     decimal.Decimal
	// ClientOrderID carries deterministic replay dedup for the order
	// pipeline: "copy:{master_trade_id}:{follow_id}".
	ClientOrderID string
}

// SkipNotifier is the durable notice sink for skipped children —
// production binds the notifications service; the copy_child_orders row
// itself is ALWAYS the durable record (a notifier failure never erases
// the SKIPPED_MIN_NOTIONAL row).
type SkipNotifier interface {
	NotifyChildSkipped(ctx context.Context, investorAccountID int64,
		child ChildOrder, detail string) error
}

// Engine fans a resolved strategy-manager fill out over ACTIVE follows.
// Weight set = follow allocation_notional (the investor-stated capital);
// safety scaling applies AFTER pro-rata, BEFORE min-notional.
type Engine struct {
	store     Store
	submitter ChildSubmitter
	notifier  SkipNotifier
	now       func() time.Time
}

// EngineOption configures the engine.
type EngineOption func(*Engine)

// WithChildSubmitter binds the order pipeline; nil submitter = children
// recorded PENDING (durable intents) without dispatch — a safe degraded
// mode, still deterministic.
func WithChildSubmitter(sub ChildSubmitter) EngineOption {
	return func(e *Engine) { e.submitter = sub }
}

// WithSkipNotifier binds the skip-notice sink.
func WithSkipNotifier(n SkipNotifier) EngineOption {
	return func(e *Engine) { e.notifier = n }
}

// NewEngine wires the fan-out — store is mandatory (fail closed).
func NewEngine(store Store, opts ...EngineOption) (*Engine, error) {
	if store == nil {
		return nil, fmt.Errorf("copy: engine requires a store (fail closed)")
	}
	e := &Engine{store: store, now: time.Now}
	for _, o := range opts {
		o(e)
	}
	return e, nil
}

// EngineFill is a resolved fill on a strategy's manager account — the
// same resolved shape the NATS trades consumer hands the PAMM engine
// (decimal text on the wire; parsed once here).
type EngineFill struct {
	StrategyID    int64 // resolved upstream: manager_account → strategy
	ManagerAcctID int64
	TradeID       int64
	InstrumentID  int64
	Side          string
	Quantity      string
	Price         string
}

// FanOutResult reports one master-fill fan-out.
type FanOutResult struct {
	TradeID     int64        `json:"trade_id"`
	StrategyID  int64        `json:"strategy_id"`
	Children    []ChildOrder `json:"children"`
	Skipped     []ChildOrder `json:"skipped"`   // SKIPPED_MIN_NOTIONAL rows
	Duplicate   bool         `json:"duplicate"` // full replay — no new rows
	ProcessedAt string       `json:"processed_at"`
}

// OnManagerFill distributes the master fill pro-rata across ACTIVE
// follows (weight = allocation_notional), applies safety scaling, floors
// at instruments.min_order_qty, records skipped children with a durable
// notice, and dispatches live children through the order seam.
//
// Idempotent per (master_trade_id, follow_id): a redelivered fill yields
// Duplicate (existing rows are not re-dispatched — the submitter's own
// client-order-id dedup is the second fence).
func (e *Engine) OnManagerFill(ctx context.Context, f EngineFill) (*FanOutResult, error) {
	qty, err := decimal.NewFromString(f.Quantity)
	if err != nil || !qty.IsPositive() {
		return nil, errorf(CodeInvalidRequest, "fill quantity %q must be positive", f.Quantity)
	}
	price, err := decimal.NewFromString(f.Price)
	if err != nil || !price.IsPositive() {
		return nil, errorf(CodeInvalidRequest, "fill price %q must be positive", f.Price)
	}
	st, err := e.store.StrategyByID(ctx, f.StrategyID)
	if err != nil {
		return nil, err
	}
	if st == nil {
		return nil, errorf(CodeNotFound, "strategy %d not found", f.StrategyID)
	}
	// SUSPENDED strategies keep existing follows running per spec — a
	// suspended strategy's fills still fan out to existing followers
	// (suspension gates NEW follows only). INCUBATING strategies have no
	// followers by construction.
	follows, err := e.store.ActiveFollowsForStrategy(ctx, f.StrategyID)
	if err != nil {
		return nil, err
	}
	out := &FanOutResult{TradeID: f.TradeID, StrategyID: f.StrategyID,
		Children: []ChildOrder{}, Skipped: []ChildOrder{},
		ProcessedAt: e.now().UTC().Format(time.RFC3339Nano)}
	if len(follows) == 0 {
		return out, nil
	}
	shares := make([]pamm.Share, len(follows))
	for i, fl := range follows {
		shares[i] = pamm.Share{ID: fl.FollowID, Weight: fl.AllocationNotional}
	}
	res, err := pamm.AllocateProRata(qty, shares)
	if err != nil {
		return nil, err
	}
	minQty, err := e.store.InstrumentMinQty(ctx, f.InstrumentID)
	if err != nil {
		return nil, err
	}
	byID := make(map[int64]Follow, len(follows))
	for _, fl := range follows {
		byID[fl.FollowID] = fl
	}

	rows := make([]ChildOrder, 0, len(res))
	for _, r := range res {
		fl := byID[r.ID]
		scaled := r.Quantity
		if fl.SafetyMode == ModeHalfRisk {
			scaled = scaled.Mul(halfRiskScale).Truncate(8) // floor at quantum —
			// HALF_RISK rounds DOWN so a half-risk child never exceeds 50%
		}
		if scaled.IsZero() {
			continue
		}
		child := ChildOrder{
			FollowID: fl.FollowID, MasterTradeID: f.TradeID,
			InstrumentID: f.InstrumentID, Side: f.Side,
			Quantity: scaled, MasterPrice: price,
		}
		// Safety scaling applied BEFORE the floor: a sub-minimum scaled
		// child is skipped with notice — never silently dropped, never
		// re-scaled upward to sneak past the risk setting.
		if scaled.LessThan(minQty) {
			child.Status = ChildSkippedMinNotional
			child.Notice = fmt.Sprintf(
				"child order skipped: scaled qty %s below instrument min %s (safety_mode=%s)",
				scaled, minQty, fl.SafetyMode)
			rows = append(rows, child)
			continue
		}
		child.Status = ChildPending
		rows = append(rows, child)
	}
	if len(rows) == 0 {
		out.Duplicate = true
		return out, nil
	}

	// Persist all intents first (durable, dedup-keyed) — dispatch happens
	// after the durable record so a crash leaves replayable PENDING rows,
	// never an undispatched ghost order.
	applied, err := e.store.InsertChildOrders(ctx, rows)
	if err != nil {
		return nil, err
	}
	if len(applied) == 0 {
		out.Duplicate = true
		return out, nil
	}
	for _, c := range applied {
		if c.Status == ChildSkippedMinNotional {
			out.Skipped = append(out.Skipped, c)
			if e.notifier != nil {
				fl := byID[c.FollowID]
				// Notice failure never erases the durable row.
				_ = e.notifier.NotifyChildSkipped(ctx, fl.InvestorAccountID, c, c.Notice)
			}
			continue
		}
		out.Children = append(out.Children, c)
	}
	// Dispatch live children through the order seam. The durable PENDING
	// row precedes dispatch so a crash mid-loop leaves replayable
	// intents; ClientOrderID dedups at the order pipeline too.
	if e.submitter != nil {
		for i := range out.Children {
			c := &out.Children[i]
			fl := byID[c.FollowID]
			oid, err := e.submitter.SubmitChild(ctx, fl.InvestorAccountID, ChildOrderRequest{
				InstrumentID:  c.InstrumentID,
				Side:          c.Side,
				Quantity:      c.Quantity,
				ClientOrderID: fmt.Sprintf("copy:%d:%d", c.MasterTradeID, c.FollowID),
			})
			if err != nil {
				c.Status = ChildRejected
				c.Notice = fmt.Sprintf("child order rejected: %v", err)
				_ = e.store.UpdateChildStatus(ctx, c.ChildID, ChildRejected, nil, c.Notice)
				continue
			}
			c.Status = ChildSubmitted
			c.ChildOrderID = &oid
			if uerr := e.store.UpdateChildStatus(ctx, c.ChildID, ChildSubmitted, &oid, ""); uerr != nil {
				return nil, uerr // durable row already holds PENDING + order id missing → replay-safe
			}
		}
	}
	return out, nil
}

// ManagerLegHandler adapts *Engine to the pamm.LegHandler contract — a
// leg is handled when its account manages ≥1 strategy profile. The same
// fill can belong to BOTH a pool and a strategy; the handler chain
// resolves each leg to its owner once.
type ManagerLegHandler struct {
	Engine *Engine
}

// Handle implements pamm.LegHandler: resolves manager_account_id →
// strategy rows, then fans the fill out per strategy (each strategy
// carries its own follows). A manager with only INCUBATING strategies
// still counts as handled when no followers exist — the no-op fan-out
// is deterministic.
func (h ManagerLegHandler) Handle(ctx context.Context, leg pamm.MasterLeg) (bool, error) {
	strategies, err := h.Engine.store.StrategiesByManager(ctx, leg.AccountID)
	if err != nil {
		return false, err
	}
	if len(strategies) == 0 {
		return false, nil // not a strategy manager — try the next handler
	}
	for _, st := range strategies {
		if _, err := h.Engine.OnManagerFill(ctx, EngineFill{
			StrategyID:    st.StrategyID,
			ManagerAcctID: leg.AccountID,
			TradeID:       leg.TradeID,
			InstrumentID:  leg.InstrumentID,
			Side:          leg.Side,
			Quantity:      leg.Quantity.String(),
			Price:         leg.Price.String(),
		}); err != nil {
			return true, err // owned the leg — propagate the failure for NAK+replay
		}
	}
	return true, nil
}
