// Grid bot engine — Task 16.3.19 lifecycle:
//
//	Create: validate → insert parent + level intent rows (cap-5 tx) →
//	        dispatch BUY legs below / SELL legs above the reference price
//	        through orders.Service.Submit (full admission pipeline).
//	OnFill: engine fill → child accounting → counter order at the
//	        adjacent level (idempotent via source_child_id) → round-trip
//	        PnL booked when the counter leg fills.
//	Stop:   status flip → engine cancel for every live child (no orphan
//	        slices — §24 #275 / §6.10 contract).
//	TP/SL:  a fill beyond take_profit_price completes the bot; beyond
//	        stop_loss_price stops it (kill switch on excursions).
package bots

import (
	"context"
	"fmt"
	"strings"
	"time"

	"exchange/internal/orders"
	"exchange/pkg/decimal"
	excerrors "exchange/pkg/errors"
)

// OrderPipeline is the admission surface the engine needs — the real
// *orders.Service satisfies it in production; tests substitute a fake.
// Every child order crosses Submit → instrument/session state → kill
// switch → appropriateness → product gate → risk limits → balance →
// durable insert → engine dispatch. Nothing bypasses it.
type OrderPipeline interface {
	Submit(ctx context.Context, acct *orders.Account, req *orders.SubmitRequest) (*orders.Ack, error)
	Cancel(ctx context.Context, acct *orders.Account, orderID int64,
		actor, requestID, ip string) (*orders.Ack, error)
	AccountByID(ctx context.Context, id int64) (*orders.Account, error)
	InstrumentBySymbol(ctx context.Context, symbol string) (*orders.Instrument, error)
}

// ReadModel is the order-store read seam (reference price for the
// BUY/SELL split, balance pre-check, order truth after cancel races).
type ReadModel interface {
	ReferencePrice(ctx context.Context, instrumentID int64) (*decimal.Decimal, error)
	AvailableBalance(ctx context.Context, accountID int64, currency string) (*decimal.Decimal, error)
	GetOrder(ctx context.Context, orderID int64) (*orders.Order, error)
}

// Engine drives grid bot lifecycle over the durable store + order pipeline.
type Engine struct {
	st   *PgStore
	ords OrderPipeline
	rm   ReadModel
	now  func() time.Time
}

// NewEngine wires the engine; now is injectable for tests.
func NewEngine(st *PgStore, ords OrderPipeline, rm ReadModel, now func() time.Time) *Engine {
	if now == nil {
		now = time.Now
	}
	return &Engine{st: st, ords: ords, rm: rm, now: now}
}

// ---- create ------------------------------------------------------------

func parseDec(field, raw string, required bool) (decimal.Decimal, *decimal.Decimal, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		if required {
			return decimal.Zero, nil, errorf(CodeGridParametersInvalid,
				"%s is required", field)
		}
		return decimal.Zero, nil, nil
	}
	d, err := decimal.NewFromString(raw)
	if err != nil || !d.IsPositive() {
		return decimal.Zero, nil, errorf(CodeGridParametersInvalid,
			"%s must be a positive decimal, got %q", field, raw)
	}
	return d, &d, nil
}

// Create validates, persists and arms a new grid bot.
func (e *Engine) Create(ctx context.Context, accountID int64,
	req CreateRequest) (*Detail, error) {

	// ---- shape validation (before any I/O that costs money) ----
	lower, _, err := parseDec("lower_price", req.LowerPrice, true)
	if err != nil {
		return nil, err
	}
	upper, _, err := parseDec("upper_price", req.UpperPrice, true)
	if err != nil {
		return nil, err
	}
	inv, _, err := parseDec("total_investment", req.TotalInvestment, true)
	if err != nil {
		return nil, err
	}
	lev := decimal.One
	if p, lp, err := parseDec("leverage", req.Leverage, false); err != nil {
		return nil, err
	} else if lp != nil {
		lev = p
	}
	var tp, sl *decimal.Decimal
	if _, p, err := parseDec("take_profit_price", req.TakeProfitPrice, false); err != nil {
		return nil, err
	} else {
		tp = p
	}
	if _, p, err := parseDec("stop_loss_price", req.StopLossPrice, false); err != nil {
		return nil, err
	} else {
		sl = p
	}
	mode := strings.ToUpper(strings.TrimSpace(req.Mode))
	if mode == "" {
		mode = ModeArithmetic
	}
	if mode != ModeArithmetic && mode != ModeGeometric {
		return nil, errorf(CodeGridParametersInvalid,
			"mode must be ARITHMETIC|GEOMETRIC, got %q", req.Mode)
	}
	if req.GridCount < MinGridCount || req.GridCount > MaxGridCount {
		return nil, errorf(CodeGridParametersInvalid,
			"grid_count %d outside [%d,%d]", req.GridCount, MinGridCount, MaxGridCount)
	}
	if !lower.LessThan(upper) {
		return nil, errorf(CodeGridParametersInvalid,
			"lower_price %s must be below upper_price %s", lower, upper)
	}
	// TP/SL sit OUTSIDE the grid — they are excursion exits, not levels.
	if tp != nil && !tp.GreaterThan(upper) {
		return nil, errorf(CodeGridParametersInvalid,
			"take_profit_price %s must be above upper_price %s", *tp, upper)
	}
	if sl != nil && !sl.LessThan(lower) {
		return nil, errorf(CodeGridParametersInvalid,
			"stop_loss_price %s must be below lower_price %s", *sl, lower)
	}

	// ---- instrument + account resolution through the real pipeline ----
	inst, err := e.ords.InstrumentBySymbol(ctx, req.Symbol)
	if err != nil {
		return nil, err
	}
	if inst == nil {
		return nil, errorf(CodeInvalidRequest, "unknown symbol %q", req.Symbol)
	}
	acct, err := e.ords.AccountByID(ctx, accountID)
	if err != nil {
		return nil, err
	}
	if acct == nil {
		return nil, errorf(CodeNotFound, "account %d not found", accountID)
	}

	// Leverage envelope: SPOT accounts run 1x only ("1x for spot-like");
	// leveraged accounts cap at the instrument's max tier.
	if acct.Type == "SPOT" && !lev.Equal(decimal.One) {
		return nil, errorf(CodeGridParametersInvalid,
			"SPOT accounts run grid bots at leverage 1 only")
	}
	if inst.MaxLeverage <= 0 && !lev.Equal(decimal.One) {
		return nil, errorf(CodeGridParametersInvalid,
			"instrument %s is not marginable (max_leverage unset) — leverage must be 1",
			inst.Symbol)
	}
	if inst.MaxLeverage > 0 && lev.GreaterThan(decimal.NewFromInt(int64(inst.MaxLeverage))) {
		return nil, errorf(CodeGridParametersInvalid,
			"leverage %s exceeds instrument max_leverage %d", lev, inst.MaxLeverage)
	}

	// ---- level generation + BUY/SELL split at reference ----
	levels, err := GridLevels(mode, lower, upper, req.GridCount, inst.TickSize)
	if err != nil {
		return nil, err
	}
	ref, err := e.rm.ReferencePrice(ctx, inst.ID)
	if err != nil {
		return nil, err
	}
	split := lower.Add(upper).Div(decimal.NewFromInt(2))
	if ref != nil && ref.IsPositive() {
		split = *ref // trade the real reference when one exists
	}

	// Per-level quote budget: total margin committed × leverage spread
	// evenly across every level; each leg converts its budget to base
	// qty at its level price, lot-rounded DOWN (never oversizes).
	budget := inv.Mul(lev).Div(decimal.NewFromInt(int64(req.GridCount)))
	var children []GridChild
	for i, px := range levels {
		qty := budget.Div(px)
		if inst.LotSize.IsPositive() {
			qty = qty.Div(inst.LotSize).Floor().Mul(inst.LotSize)
		}
		if inst.MinOrderQty.IsPositive() && qty.LessThan(inst.MinOrderQty) {
			return nil, errorf(CodeGridParametersInvalid,
				"per-level qty %s at level %d below min_order_qty %s — "+
					"raise total_investment or reduce grid_count",
				qty, i, inst.MinOrderQty)
		}
		if inst.MinNotional.IsPositive() && qty.Mul(px).LessThan(inst.MinNotional) {
			return nil, errorf(CodeGridParametersInvalid,
				"per-level notional %s at level %d below min_notional %s",
				qty.Mul(px), i, inst.MinNotional)
		}
		var side string
		switch {
		case px.LessThan(split):
			side = "BUY"
		case px.GreaterThan(split):
			side = "SELL"
		default:
			continue // level sits exactly on the reference — no leg
		}
		children = append(children, GridChild{
			LevelIndex: i, Side: side, Price: px, Qty: qty, Status: ChildPending,
		})
	}
	if len(children) == 0 {
		return nil, errorf(CodeGridParametersInvalid,
			"reference %s leaves no placeable grid levels", split)
	}

	// ---- margin pre-check (quote side) ----
	// SPOT accounts must hold the committed quote capital outright;
	// leveraged accounts defer the exact margin math to checkBalance
	// inside Submit (it owns the account-type-aware rules).
	if acct.Type == "SPOT" {
		if bal, err := e.rm.AvailableBalance(ctx, accountID, inst.QuoteCurrency); err != nil {
			return nil, err
		} else if bal == nil || bal.LessThan(inv) {
			avail := "0"
			if bal != nil {
				avail = bal.String()
			}
			return nil, errorf(CodeGridMarginInsufficient,
				"available %s %s < total_investment %s", avail, inst.QuoteCurrency, inv)
		}
	}

	bot := &GridBot{
		AccountID:       accountID,
		InstrumentID:    inst.ID,
		Symbol:          inst.Symbol,
		LowerPrice:      lower,
		UpperPrice:      upper,
		GridCount:       req.GridCount,
		Mode:            mode,
		TotalInvestment: inv,
		Leverage:        lev,
		TakeProfitPrice: tp,
		StopLossPrice:   sl,
		Status:          StatusRunning,
		PnLCurrency:     inst.QuoteCurrency,
		ReferencePrice:  &split,
	}
	bot, err = e.st.CreateBot(ctx, bot, children)
	if err != nil {
		return nil, err
	}

	// ---- dispatch initial legs through the real pipeline ----
	stored, err := e.st.Children(ctx, bot.BotID)
	if err != nil {
		return nil, err
	}
	placed := 0
	for i := range stored {
		if stored[i].Status != ChildPending {
			continue // SKIPPED intent rows never dispatch
		}
		if err := e.submitChild(ctx, acct, bot, &stored[i]); err != nil {
			return nil, err
		}
		if stored[i].Status == ChildWorking {
			placed++
		}
	}
	if placed == 0 {
		_, _ = e.st.Transition(ctx, bot.BotID,
			[]string{StatusRunning}, StatusFailed, "no leg admitted")
		bot.Status = StatusFailed
	}
	return &Detail{Bot: *bot, Children: stored}, nil
}

// submitChild dispatches one child through orders.Service and reconciles
// the child row (WORKING + order_id on accept, REJECTED + code on deny).
func (e *Engine) submitChild(ctx context.Context, acct *orders.Account,
	bot *GridBot, c *GridChild) error {

	clientID := fmt.Sprintf("grid-%d-%d", bot.BotID, c.ID)
	ack, err := e.ords.Submit(ctx, acct, &orders.SubmitRequest{
		Symbol:        bot.Symbol,
		Side:          c.Side,
		OrderType:     orders.TypeLimit,
		TimeInForce:   orders.TIFGTC,
		ClientOrderID: clientID,
		Quantity:      &c.Qty,
		Price:         &c.Price,
	})
	if err != nil {
		code := excerrors.CodeOf(err)
		_ = e.st.RejectChild(ctx, c.ID, code)
		c.Status = ChildRejected
		c.Note = code
		return nil // per-leg rejection is recorded, not fatal to the bot
	}
	if err := e.st.BindOrder(ctx, c.ID, ack.OrderID, clientID); err != nil {
		return err
	}
	c.Status = ChildWorking
	c.OrderID = &ack.OrderID
	c.ClientOrderID = clientID
	return nil
}

// ---- fill handling ------------------------------------------------------

// OnFill is invoked by the engine-event consumer's fill hook for every
// TradeFill leg. Non-grid orders return immediately; grid children get
// accounting + the adjacent-level counter order + TP/SL excursion kill.
//
// A marketable child can fill between Submit's engine dispatch and the
// BindOrder commit that records its order_id. Rather than sleeping on
// every fill, foreign orders are filtered by their client_order_id
// prefix ("grid-") on the order read model — the bounded retry then
// only runs for genuinely grid-owned fills.
func (e *Engine) OnFill(ctx context.Context, orderID int64,
	px, qty decimal.Decimal) error {

	child, err := e.st.ChildByOrderID(ctx, orderID)
	if err != nil {
		return err
	}
	if child == nil {
		o, gerr := e.rm.GetOrder(ctx, orderID)
		if gerr != nil || o == nil ||
			!strings.HasPrefix(o.ClientOrderID, "grid-") {
			return gerr // foreign fill — pass through untouched
		}
		for i := 0; i < 10; i++ {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(20 * time.Millisecond):
			}
			child, err = e.st.ChildByOrderID(ctx, orderID)
			if err != nil || child != nil {
				break
			}
		}
		if err != nil || child == nil {
			return err // bind never landed — surface for fill replay
		}
	}
	bot, err := e.st.BotByID(ctx, child.BotID)
	if err != nil || bot == nil {
		return err
	}
	fresh, full, err := e.st.ApplyChildFill(ctx, child.ID, px, qty)
	if err != nil || fresh == nil {
		return err
	}
	if !full || bot.Status != StatusRunning {
		return nil
	}
	// Round-trip credit: this child is itself a counter order — book the
	// spread between it and its source leg.
	if fresh.SourceChildID != nil && fresh.AvgFillPrice != nil {
		src, err := e.st.ChildByID(ctx, *fresh.SourceChildID)
		if err != nil {
			return err
		}
		if src != nil && src.AvgFillPrice != nil {
			pnl := roundTripPnL(fresh.Side, *fresh.AvgFillPrice,
				*src.AvgFillPrice, fresh.FilledQty)
			if err := e.st.CreditRoundTrip(ctx, bot.BotID, fresh.ID, pnl); err != nil {
				return err
			}
		}
	}
	// Excursion exits (kill switch): a fill print beyond the bot's TP or
	// SL terminates the strategy and unwinds every live child.
	if bot.TakeProfitPrice != nil && px.GreaterThanOrEqual(*bot.TakeProfitPrice) {
		return e.terminate(ctx, bot, StatusCompleted, StopReasonTakeProfit)
	}
	if bot.StopLossPrice != nil && px.LessThanOrEqual(*bot.StopLossPrice) {
		return e.terminate(ctx, bot, StatusStopped, StopReasonStopLoss)
	}
	return e.placeCounter(ctx, bot, fresh)
}

// roundTripPnL computes realized grid PnL for a completed counter leg:
// SELL counter after a BUY source earns (sell − buy)·qty; BUY counter
// after a SELL source earns (sell − buy)·qty likewise — sign follows the
// counter side.
func roundTripPnL(counterSide string, counterPx, srcPx,
	qty decimal.Decimal) decimal.Decimal {
	if counterSide == "SELL" {
		return counterPx.Sub(srcPx).Mul(qty)
	}
	return srcPx.Sub(counterPx).Mul(qty)
}

// placeCounter claims and dispatches the opposite-side order at the
// adjacent grid level. A BUY fill at level i spawns a SELL at i+1; a
// SELL fill spawns a BUY at i−1. Boundaries close the grid silently;
// an already-occupied level records a SKIPPED row (collision rule).
func (e *Engine) placeCounter(ctx context.Context, bot *GridBot,
	fresh *GridChild) error {

	target := fresh.LevelIndex + 1
	side := "SELL"
	if fresh.Side == "SELL" {
		target = fresh.LevelIndex - 1
		side = "BUY"
	}
	if target < 0 || target >= bot.GridCount {
		return nil // grid boundary — nothing to place
	}
	inst, err := e.ords.InstrumentBySymbol(ctx, bot.Symbol)
	if err != nil {
		return err
	}
	if inst == nil {
		return errorf(CodeInternal, "grid bot %d instrument vanished", bot.BotID)
	}
	levels, err := GridLevels(bot.Mode, bot.LowerPrice, bot.UpperPrice,
		bot.GridCount, inst.TickSize)
	if err != nil {
		return err
	}
	counter := &GridChild{
		BotID:         bot.BotID,
		LevelIndex:    target,
		Side:          side,
		Price:         levels[target],
		Qty:           fresh.Qty, // round-trip pairing: same base qty
		SourceChildID: &fresh.ID,
	}
	if occupied, err := e.st.LevelOccupied(ctx, bot.BotID, target, side); err != nil {
		return err
	} else if occupied {
		// Collision — record a SKIPPED claim so the fill→flip stays
		// accounted and idempotent without stacking two legs.
		counter.Status = ChildSkipped
		counter.Note = "level occupied"
		_, _, err := e.st.ClaimCounter(ctx, counter)
		return err
	}
	counter.Status = ChildPending
	id, claimed, err := e.st.ClaimCounter(ctx, counter)
	if err != nil || !claimed {
		return err // duplicate delivery — a counter already exists
	}
	counter.ID = id
	acct, err := e.ords.AccountByID(ctx, bot.AccountID)
	if err != nil || acct == nil {
		return err
	}
	return e.submitChild(ctx, acct, bot, counter)
}

// ---- stop / terminate ---------------------------------------------------

// Stop is DELETE /api/v1/bots/grid/{id}: flips RUNNING→STOPPED then
// cancels every live child through the order pipeline — idempotent on
// repeat calls (terminal bots return their state unchanged).
func (e *Engine) Stop(ctx context.Context, acct *orders.Account,
	botID int64) (*Detail, error) {

	bot, err := e.st.GetBot(ctx, acct.ID, botID)
	if err != nil {
		return nil, err
	}
	if bot == nil {
		return nil, errorf(CodeNotFound, "grid bot %d not found", botID)
	}
	if bot.Status == StatusRunning {
		if _, err := e.st.Transition(ctx, bot.BotID,
			[]string{StatusRunning}, StatusStopped, StopReasonUser); err != nil {
			return nil, err
		}
	}
	if err := e.cancelOpen(ctx, acct, bot); err != nil {
		return nil, err
	}
	return e.Detail(ctx, acct.ID, botID)
}

// terminate is the engine-side kill (TP/SL excursion).
func (e *Engine) terminate(ctx context.Context, bot *GridBot,
	to, reason string) error {

	if ok, err := e.st.Transition(ctx, bot.BotID,
		[]string{StatusRunning}, to, reason); err != nil || !ok {
		return err
	}
	acct, err := e.ords.AccountByID(ctx, bot.AccountID)
	if err != nil || acct == nil {
		return err
	}
	return e.cancelOpen(ctx, acct, bot)
}

// cancelOpen cancels every live child; per-child failures reconcile the
// row from the order read model (a racing fill wins — the child then
// shows its true terminal state rather than a fabricated CANCELLED).
func (e *Engine) cancelOpen(ctx context.Context, acct *orders.Account,
	bot *GridBot) error {

	open, err := e.st.OpenChildren(ctx, bot.BotID)
	if err != nil {
		return err
	}
	actor := fmt.Sprintf("grid-bot:%d", bot.BotID)
	for _, c := range open {
		if c.OrderID == nil {
			_ = e.st.CancelChild(ctx, c.ID)
			continue
		}
		if _, err := e.ords.Cancel(ctx, acct, *c.OrderID, actor, "", ""); err != nil {
			// Terminal-state races are truth, not failure — sync the row
			// from the read model and keep unwinding the rest.
			o, gerr := e.rm.GetOrder(ctx, *c.OrderID)
			if gerr == nil && o != nil {
				switch o.Status {
				case "FILLED":
					fq := o.FilledQty
					avg := c.Price
					if o.AvgFillPrice != nil {
						avg = *o.AvgFillPrice
					}
					_, _, _ = e.st.ApplyChildFill(ctx, c.ID, avg, fq.Sub(c.FilledQty))
					continue
				case "CANCELLED", "EXPIRED", "REJECTED":
					_ = e.st.CancelChild(ctx, c.ID)
					continue
				}
			}
			// Order may still be live (dispatch timeout) — surface the
			// error; the caller retries DELETE and re-enters here.
			return err
		}
		_ = e.st.CancelChild(ctx, c.ID)
	}
	return nil
}

// ---- reads ---------------------------------------------------------------

func (e *Engine) List(ctx context.Context, accountID int64) ([]GridBot, error) {
	return e.st.ListBots(ctx, accountID)
}

func (e *Engine) Detail(ctx context.Context, accountID, botID int64) (*Detail, error) {
	bot, err := e.st.GetBot(ctx, accountID, botID)
	if err != nil {
		return nil, err
	}
	if bot == nil {
		return nil, errorf(CodeNotFound, "grid bot %d not found", botID)
	}
	children, err := e.st.Children(ctx, botID)
	if err != nil {
		return nil, err
	}
	if children == nil {
		children = []GridChild{}
	}
	return &Detail{Bot: *bot, Children: children}, nil
}
