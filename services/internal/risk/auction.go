// auction.go — the §13.4 liquidation auction ladder.
//
// Positions whose liquidated notional exceeds 1% of instrument open
// interest never hit the book as a raw market close — they open an
// auction that solicits LP flow at a floor derived from the position's
// liquidation_price:
//
//	CALL       5s      resting reduce-only close at floor (LPs cross it)
//	FILL       —       residual ≤50% keeps working at floor until the
//	                   absolute deadline (created + 5s + 60s)
//	EXTEND     ≤60s    residual >50% at a window edge re-floors the order
//	                   0.5% more aggressive per 5s increment
//	FORCE_CASH —       budget exhausted → IOC at mark×0.95 (sells) /
//	                   mark×1.05 (buys); deficiency lands on the fund
//
// Redis contract (spec §4):
//
//	auction:{instrument_id}:{position_id}  HASH    live state
//	liquidation_auction:{id}:phase         STRING  phase TTL 120s
//	auction:{id}:events                    LIST    AUCTION_STARTED /
//	                                             AUCTION_EXTEND /
//	                                             AUCTION_FILLED /
//	                                             AUCTION_FAILED
//
// Fill reconciliation flows through LiquidationService.RecordFill →
// AuctionEngine.OnFill — the engine never trusts the resting order's
// presence as proof of progress; the persisted row is authority.
package risk

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"exchange/internal/accounts"
	excredis "exchange/internal/redis"
	"exchange/pkg/decimal"
	excerrors "exchange/pkg/errors"
)

// §13.4 auction timing & floor contract.
const (
	// AuctionCallDuration is the CALL solicitation window.
	AuctionCallDuration = 5 * time.Second
	// AuctionExtendIncrement is the per-EXTEND step.
	AuctionExtendIncrement = 5 * time.Second
	// AuctionExtendBudget is the total EXTEND allowance after CALL.
	AuctionExtendBudget = 60 * time.Second
	// AuctionExtendThreshold — a residual above 50% of the original
	// quantity at a window edge extends the call; at-or-below it the
	// auction keeps working in FILL.
	AuctionExtendThreshold = "0.5"
	// AuctionPhaseTTL bounds the liquidation_auction:{id}:phase key.
	AuctionPhaseTTL = 120 * time.Second
)

// AuctionFloorDecayPerStep is the §24 #34 0.5% floor decay applied per
// 5s EXTEND increment (sells: floor×0.995ⁿ; buys: cap×1.005ⁿ).
var AuctionFloorDecayPerStep = decimal.NewFromFloat(0.005)

// Force-cash price bands (§13.4 failover): liquidated longs force-sell
// at mark×0.95; shorts force-buy at mark×1.05.
var (
	AuctionForceCashLongFactor  = decimal.NewFromFloat(0.95)
	AuctionForceCashShortFactor = decimal.NewFromFloat(1.05)
)

// Auction phases — liquidation_auctions.phase (auction_phase_enum).
const (
	AuctionPhaseCall      = "CALL"
	AuctionPhaseFill      = "FILL"
	AuctionPhaseExtend    = "EXTEND"
	AuctionPhaseForceCash = "FORCE_CASH"
	// AuctionPhaseParked is the terminal state for an auction whose
	// close legs can never be dispatched (account TRADING_HALTED /
	// missing). Parking stops the per-tick retry storm; the row stays
	// queryable history and a single P1 alert carries the detail.
	AuctionPhaseParked = "PARKED"
)

// Auction event vocabulary (spec §13.4 event log).
const (
	auctionEventStarted = "AUCTION_STARTED"
	auctionEventExtend  = "AUCTION_EXTEND"
	auctionEventFilled  = "AUCTION_FILLED"
	auctionEventFailed  = "AUCTION_FAILED"
)

// AuctionRow is one liquidation_auctions row (migration 015).
type AuctionRow struct {
	ID           int64
	InstrumentID int64
	PositionID   int64
	Phase        string // CALL | FILL | EXTEND | FORCE_CASH
	FloorPrice   decimal.Decimal
	UnfilledQty  decimal.Decimal
	FilledQty    decimal.Decimal
	AvgFillPrice decimal.Decimal
	PhaseStartAt time.Time
	PhaseEndAt   time.Time
	CreatedAt    time.Time
}

// OriginalQty is filled + unfilled — the EXTEND-threshold denominator.
func (a AuctionRow) OriginalQty() decimal.Decimal {
	return a.FilledQty.Add(a.UnfilledQty)
}

// AuctionStore is the engine's PG seam — a superset-free slice of
// LiquidationStore so the auction can be constructed against the same
// persistence object.
type AuctionStore interface {
	InsertAuction(ctx context.Context, a AuctionRow) (int64, error)
	AuctionByID(ctx context.Context, id int64) (*AuctionRow, error)
	UpdateAuctionPhase(ctx context.Context, id int64, phase string,
		floor decimal.Decimal, start, end time.Time) error
	RecordAuctionFill(ctx context.Context, id int64, filledQty decimal.Decimal,
		avgPrice decimal.Decimal, unfilled decimal.Decimal) error
	ActiveAuctions(ctx context.Context) ([]AuctionRow, error)
	// PositionByID resolves the liquidated position for repriece /
	// force-cash submission. nil, nil when the row is gone (already
	// settled — the auction is finished regardless of row state).
	PositionByID(ctx context.Context, positionID int64) (*LiqPosition, error)
}

// AuctionEngine drives the §13.4 ladder for one liquidation cluster.
// Open is invoked synchronously on the liquidation path; AdvanceOnce is
// invoked every scanner tick (2s cadence) and on fill notifications.
type AuctionEngine struct {
	rdb      *excredis.Client
	store    AuctionStore
	dispatch accounts.OrderDispatcher
	marks    MarkPriceProvider
	alerter  OpsAlerter
	now      func() time.Time
	logf     func(format string, args ...any)
}

// AuctionDeps wires the engine. Store, Dispatch and Marks are
// mandatory — an auction without a floor or a dispatch path is a
// stranded position (fail closed).
type AuctionDeps struct {
	Redis    *excredis.Client
	Store    AuctionStore
	Dispatch accounts.OrderDispatcher
	Marks    MarkPriceProvider
	Alerter  OpsAlerter
	Now      func() time.Time
	Logf     func(format string, args ...any)
}

// NewAuctionEngine builds the engine.
func NewAuctionEngine(d AuctionDeps) (*AuctionEngine, error) {
	if d.Redis == nil {
		return nil, fmt.Errorf("auction: nil redis")
	}
	if d.Store == nil {
		return nil, fmt.Errorf("auction: nil store")
	}
	if d.Dispatch == nil {
		return nil, fmt.Errorf("auction: nil order dispatcher")
	}
	if d.Marks == nil {
		return nil, fmt.Errorf("auction: nil mark price provider")
	}
	e := &AuctionEngine{
		rdb: d.Redis, store: d.Store, dispatch: d.Dispatch,
		marks: d.Marks, alerter: d.Alerter, now: d.Now, logf: d.Logf,
	}
	if e.now == nil {
		e.now = func() time.Time { return time.Now().UTC() }
	}
	if e.logf == nil {
		e.logf = func(string, ...any) {}
	}
	return e, nil
}

// FloorFor derives the §13.4 auction floor: liquidation_price×0.98 for
// a liquidated LONG (the close is a SELL — never fill below floor) and
// ×1.02 for a SHORT (never fill above the cap). A missing
// liquidation_price falls back to mark — fail-closed would strand the
// position; the seam records the substitution in the state hash.
func FloorFor(p LiqPosition) decimal.Decimal {
	basis := p.LiquidationPrice
	if !basis.IsPositive() {
		basis = p.MarkPrice
	}
	if !basis.IsPositive() {
		return decimal.Zero
	}
	if p.Side == "LONG" {
		return basis.Mul(decimal.NewFromFloat(0.98)).Round(8)
	}
	return basis.Mul(decimal.NewFromFloat(1.02)).Round(8)
}

// decayFloor moves the floor 0.5% more aggressive per EXTEND step:
// sells lower the floor, buys raise the cap (§24 #34).
func decayFloor(floor decimal.Decimal, side string, steps int) decimal.Decimal {
	f := floor
	for i := 0; i < steps; i++ {
		if side == "LONG" {
			f = f.Mul(decimal.One.Sub(AuctionFloorDecayPerStep))
		} else {
			f = f.Mul(decimal.One.Add(AuctionFloorDecayPerStep))
		}
	}
	return f.Round(8)
}

// forceCashCap is the §13.4 failover price: mark×0.95 sells /
// mark×1.05 buys.
func forceCashCap(mark decimal.Decimal, side string) decimal.Decimal {
	if side == "LONG" {
		return mark.Mul(AuctionForceCashLongFactor).Round(8)
	}
	return mark.Mul(AuctionForceCashShortFactor).Round(8)
}

// absoluteDeadline is created + CALL + the full EXTEND budget — the
// last instant before FORCE_CASH must fire.
func (e *AuctionEngine) absoluteDeadline(a AuctionRow) time.Time {
	return a.CreatedAt.Add(AuctionCallDuration).Add(AuctionExtendBudget)
}

// Open starts a CALL: persist the row, publish the state hash, submit
// the resting reduce-only close at floor for the 5s window.
func (e *AuctionEngine) Open(ctx context.Context, p LiqPosition,
	reason string, marginCallEvID *int64) error {

	floor := FloorFor(p)
	if !floor.IsPositive() {
		return excerrors.New(CodeLiquidationFailed,
			fmt.Sprintf("auction pos %d: no positive floor (liq=%s mark=%s)",
				p.ID, p.LiquidationPrice, p.MarkPrice))
	}
	now := e.now()
	row := AuctionRow{
		InstrumentID: p.InstrumentID,
		PositionID:   p.ID,
		Phase:        AuctionPhaseCall,
		FloorPrice:   floor,
		UnfilledQty:  p.Quantity.Abs(),
		PhaseStartAt: now,
		PhaseEndAt:   now.Add(AuctionCallDuration),
		CreatedAt:    now,
	}
	id, err := e.store.InsertAuction(ctx, row)
	if err != nil {
		return excerrors.Wrap("INTERNAL_ERROR", "auction insert", err)
	}
	row.ID = id

	if err := e.submitLeg(ctx, p, row, floor, row.PhaseEndAt); err != nil {
		if isParkable(err) {
			e.appendEvent(ctx, id, auctionEventStarted, map[string]string{
				"position_id": fmt.Sprint(p.ID), "reason": reason,
				"floor": floor.String(), "qty": p.Quantity.Abs().String(),
			})
			e.park(ctx, row, p, err.Error())
			return nil
		}
		return err
	}
	e.publishState(ctx, row, p, 0)
	e.appendEvent(ctx, id, auctionEventStarted, map[string]string{
		"position_id": fmt.Sprint(p.ID), "reason": reason,
		"floor": floor.String(), "qty": p.Quantity.Abs().String(),
	})
	return nil
}

// submitLeg dispatches one resting reduce-only GTD close at price,
// expiring at the phase window edge. The client order id carries the
// auction id so fill reports route back through OnFill.
func (e *AuctionEngine) submitLeg(ctx context.Context, p LiqPosition,
	row AuctionRow, price decimal.Decimal, expireAt time.Time) error {

	ack, err := e.dispatch.SubmitClose(ctx, accounts.CloseOrderRequest{
		AccountID:     p.AccountID,
		InstrumentID:  p.InstrumentID,
		Side:          p.CloseSide(),
		Quantity:      row.UnfilledQty,
		ReduceOnly:    true,
		LimitPrice:    price,
		ExpireAt:      &expireAt,
		ClientOrderID: fmt.Sprintf("auc-%d-%d", row.ID, e.now().UnixMilli()),
	})
	if err != nil {
		return excerrors.Wrap(CodeLiquidationFailed,
			fmt.Sprintf("auction %d leg dispatch", row.ID), err)
	}
	if ack == nil || !ack.Accepted {
		return excerrors.New(CodeLiquidationFailed,
			fmt.Sprintf("auction %d leg rejected: %s", row.ID, ackDetail(ack)))
	}
	e.setStateFields(ctx, row, map[string]string{
		"order_id":        fmt.Sprint(ack.OrderID),
		"client_order_id": ack.ClientOrderID,
	})
	return nil
}

// OnFill records one execution against the auction. Called by
// LiquidationService.RecordFill when a fill report carries an auction
// id. Idempotent via the store's monotonic filled_qty update.
func (e *AuctionEngine) OnFill(ctx context.Context, auctionID int64,
	filledQty, avgPrice, unfilled decimal.Decimal) error {

	if err := e.store.RecordAuctionFill(ctx, auctionID, filledQty, avgPrice, unfilled); err != nil {
		return excerrors.Wrap("INTERNAL_ERROR", "auction fill record", err)
	}
	if !unfilled.IsPositive() {
		return e.complete(ctx, auctionID)
	}
	return nil
}

// complete finishes a filled auction: log AUCTION_FILLED and retire the
// live keys. The liquidation_events row was written by RecordFill.
func (e *AuctionEngine) complete(ctx context.Context, auctionID int64) error {
	row, err := e.store.AuctionByID(ctx, auctionID)
	if err != nil {
		return err
	}
	if row == nil {
		return nil
	}
	e.appendEvent(ctx, auctionID, auctionEventFilled, map[string]string{
		"filled_qty": row.FilledQty.String(), "avg_price": row.AvgFillPrice.String(),
	})
	e.retireState(ctx, *row)
	return nil
}

// AdvanceOnce is the phase advancer — invoked on every scanner tick.
// It walks active auctions and fires due phase transitions; fills that
// emptied the residual terminate early via complete().
func (e *AuctionEngine) AdvanceOnce(ctx context.Context) (int, error) {
	rows, err := e.store.ActiveAuctions(ctx)
	if err != nil {
		return 0, excerrors.Wrap("INTERNAL_ERROR", "auction scan", err)
	}
	n := 0
	for _, row := range rows {
		if err := e.advance(ctx, row); err != nil {
			s := err
			e.logf("auction %d advance: %v", row.ID, s)
			continue
		}
		n++
	}
	return n, nil
}

// advance executes the state machine for one auction row.
func (e *AuctionEngine) advance(ctx context.Context, a AuctionRow) error {
	if !a.UnfilledQty.IsPositive() {
		return e.complete(ctx, a.ID)
	}
	now := e.now()
	if now.Before(a.PhaseEndAt) {
		return nil // window still open
	}
	p, err := e.store.PositionByID(ctx, a.PositionID)
	if err != nil {
		return excerrors.Wrap("INTERNAL_ERROR", "auction position read", err)
	}
	if p == nil {
		// Position already settled outside the auction — retire clean.
		return e.complete(ctx, a.ID)
	}
	pastDeadline := !now.Before(e.absoluteDeadline(a))
	switch a.Phase {
	case AuctionPhaseCall, AuctionPhaseExtend, AuctionPhaseFill:
		residualRatio := a.UnfilledQty.Div(a.OriginalQty())
		if pastDeadline {
			return e.parkOnTerminal(ctx, a, *p, e.forceCash(ctx, a, *p))
		}
		if a.Phase != AuctionPhaseFill &&
			residualRatio.GreaterThan(decimal.RequireFromString(AuctionExtendThreshold)) {
			return e.parkOnTerminal(ctx, a, *p, e.extend(ctx, a, *p))
		}
		// ≤50% residual: FILL — keep the order working at the current
		// floor until the absolute deadline.
		return e.parkOnTerminal(ctx, a, *p, e.toFill(ctx, a, *p))
	case AuctionPhaseForceCash:
		// Residual still open after force-cash dispatch — retry once per
		// tick; the deficiency-vs-fund accounting lands in RecordFill.
		return e.parkOnTerminal(ctx, a, *p, e.forceCash(ctx, a, *p))
	case AuctionPhaseParked:
		// Terminal — ActiveAuctions excludes parked rows; this is the
		// defensive path for a scan racing the phase update.
		return nil
	default:
		return fmt.Errorf("auction %d: unknown phase %q", a.ID, a.Phase)
	}
}

// parkOnTerminal converts an undispatchable rejection into a PARKED
// transition — one event + one alert, then the row leaves the scan.
// Any other error passes through for the normal retry ladder.
func (e *AuctionEngine) parkOnTerminal(ctx context.Context, a AuctionRow,
	p LiqPosition, err error) error {

	if err == nil {
		return nil
	}
	if !isParkable(err) {
		return err
	}
	e.park(ctx, a, p, err.Error())
	return nil
}

// park transitions the auction to the terminal PARKED phase after an
// undispatchable leg rejection: one AUCTION_FAILED event, one P1
// alert, live keys retired. The residual stays recorded for ops —
// when the halt lifts an operator closes the position manually; the
// ladder does not resurrect itself.
func (e *AuctionEngine) park(ctx context.Context, a AuctionRow,
	p LiqPosition, detail string) {

	now := e.now()
	if err := e.store.UpdateAuctionPhase(ctx, a.ID, AuctionPhaseParked,
		a.FloorPrice, now, now); err != nil {
		// A failed PARKED write is transient — leave the row active so
		// the next tick retries the transition.
		e.logf("auction %d park transition: %v", a.ID, err)
		return
	}
	e.appendEvent(ctx, a.ID, auctionEventFailed, map[string]string{
		"detail": detail, "terminal": AuctionPhaseParked,
	})
	e.raiseAlert(ctx, SeverityP1, CodeLiquidationFailed, fmt.Sprintf(
		"auction %d parked: close undispatchable (%s)", a.ID, detail),
		map[string]string{
			"auction_id":  fmt.Sprint(a.ID),
			"position_id": fmt.Sprint(p.ID),
			"account_id":  fmt.Sprint(p.AccountID),
			"detail":      detail,
		})
	e.retireState(ctx, a)
}

// isParkable reports whether a leg-dispatch failure is terminal for
// the auction: a TRADING_HALTED account cannot accept reduce-only
// legs (admission rejects them), and a missing account/instrument can
// never be dispatched to. The check walks the whole wrap chain — leg
// errors surface inside a CodeLiquidationFailed wrap. Liquidity and
// transient I/O failures stay on the retry ladder.
func isParkable(err error) bool {
	for err != nil {
		var e *excerrors.Error
		if errors.As(err, &e) {
			switch e.Code {
			case "TRADING_HALTED", "ORDER_NOT_FOUND", "ACCOUNT_NOT_FOUND":
				return true
			}
			err = e.Unwrap()
			continue
		}
		err = errors.Unwrap(err)
	}
	return false
}

// extend decays the floor 0.5% and resubmits the residual for the next
// 5s increment (§13.4 EXTEND / §24 #34).
func (e *AuctionEngine) extend(ctx context.Context, a AuctionRow, p LiqPosition) error {
	// EXTEND count derives from elapsed budget so a replayed row resumes
	// deterministically: steps = elapsed_extend / 5s + 1.
	elapsed := e.now().Sub(a.CreatedAt.Add(AuctionCallDuration))
	steps := int(elapsed/AuctionExtendIncrement) + 1
	floor := decayFloor(FloorFor(p), p.Side, steps)
	end := e.now().Add(AuctionExtendIncrement)
	if dl := e.absoluteDeadline(a); end.After(dl) {
		end = dl
	}
	if err := e.store.UpdateAuctionPhase(ctx, a.ID, AuctionPhaseExtend, floor,
		e.now(), end); err != nil {
		return excerrors.Wrap("INTERNAL_ERROR", "auction extend", err)
	}
	if err := e.resubmit(ctx, a, p, floor, end); err != nil {
		return err
	}
	e.appendEvent(ctx, a.ID, auctionEventExtend, map[string]string{
		"floor": floor.String(), "step": fmt.Sprint(steps),
	})
	return nil
}

// toFill parks the residual at the current floor until the deadline.
func (e *AuctionEngine) toFill(ctx context.Context, a AuctionRow, p LiqPosition) error {
	end := e.absoluteDeadline(a)
	if err := e.store.UpdateAuctionPhase(ctx, a.ID, AuctionPhaseFill,
		a.FloorPrice, e.now(), end); err != nil {
		return excerrors.Wrap("INTERNAL_ERROR", "auction fill phase", err)
	}
	return e.resubmit(ctx, a, p, a.FloorPrice, end)
}

// resubmit replaces the resting leg: cancel whatever rests for the
// account on the instrument (the account is in LIQUIDATING — only our
// legs can rest), then submit at the new price with a GTD expiry.
func (e *AuctionEngine) resubmit(ctx context.Context, a AuctionRow,
	p LiqPosition, price decimal.Decimal, expireAt time.Time) error {

	if _, err := e.dispatch.MassCancel(ctx, accounts.MassCancelScope{
		AccountID: p.AccountID, InstrumentID: p.InstrumentID,
		Reason: "auction-reprice",
	}); err != nil {
		return excerrors.Wrap(CodeLiquidationFailed, "auction reprice cancel", err)
	}
	row := a
	row.UnfilledQty = a.UnfilledQty
	return e.submitLeg(ctx, p, row, price, expireAt)
}

// forceCash fires the §13.4 failover: IOC at mark×0.95/×1.05. The
// deficiency between the forced price and liquidation_price lands on
// the insurance fund through LiquidationService.RecordFill.
func (e *AuctionEngine) forceCash(ctx context.Context, a AuctionRow, p LiqPosition) error {
	mark, err := e.marks.GetMarkPrice(p.Symbol)
	if err != nil || !mark.IsPositive() {
		// No mark — do NOT force-close blind. Alert and hold the row for
		// the next tick (the position stays liquidatable; the scanner
		// keeps the account job alive via the dedup key).
		e.raiseAlert(ctx, SeverityP1, CodeLiquidationFailed, fmt.Sprintf(
			"auction %d force-cash blocked: no mark for %s", a.ID, p.Symbol),
			map[string]string{"auction_id": fmt.Sprint(a.ID), "symbol": p.Symbol})
		return fmt.Errorf("auction %d: mark unavailable for force cash", a.ID)
	}
	cap_ := forceCashCap(mark, p.Side)
	now := e.now()
	if err := e.store.UpdateAuctionPhase(ctx, a.ID, AuctionPhaseForceCash,
		cap_, now, now.Add(AuctionPhaseTTL)); err != nil {
		return excerrors.Wrap("INTERNAL_ERROR", "auction force-cash phase", err)
	}
	if _, err := e.dispatch.MassCancel(ctx, accounts.MassCancelScope{
		AccountID: p.AccountID, InstrumentID: p.InstrumentID,
		Reason: "auction-force-cash",
	}); err != nil {
		return excerrors.Wrap(CodeLiquidationFailed, "force-cash cancel", err)
	}
	ack, err := e.dispatch.SubmitClose(ctx, accounts.CloseOrderRequest{
		AccountID:     p.AccountID,
		InstrumentID:  p.InstrumentID,
		Side:          p.CloseSide(),
		Quantity:      a.UnfilledQty,
		ReduceOnly:    true,
		LimitPrice:    cap_,
		ClientOrderID: fmt.Sprintf("auc-fc-%d-%d", a.ID, now.UnixMilli()),
	})
	if err != nil || ack == nil || !ack.Accepted {
		e.appendEvent(ctx, a.ID, auctionEventFailed, map[string]string{
			"detail": ackDetail(ack),
		})
		if err == nil {
			err = fmt.Errorf("force-cash rejected: %s", ackDetail(ack))
		}
		return excerrors.Wrap(CodeLiquidationFailed, "force-cash dispatch", err)
	}
	e.setStateFields(ctx, a, map[string]string{"force_cash_at": now.UTC().Format(time.RFC3339Nano)})
	return nil
}

// ---------------------------------------------------------------------------
// Redis state & event log (spec §4 keys, §13.4 event vocabulary)
// ---------------------------------------------------------------------------

func (e *AuctionEngine) publishState(ctx context.Context, a AuctionRow,
	p LiqPosition, extends int) {

	h := map[string]string{
		"auction_id":    fmt.Sprint(a.ID),
		"phase":         a.Phase,
		"floor_price":   a.FloorPrice.String(),
		"unfilled_qty":  a.UnfilledQty.String(),
		"filled_qty":    a.FilledQty.String(),
		"position_side": p.Side,
		"account_id":    fmt.Sprint(p.AccountID),
		"phase_end_at":  fmt.Sprint(a.PhaseEndAt.UnixMilli()),
		"extends":       fmt.Sprint(extends),
	}
	key := AuctionStateKey(a.InstrumentID, a.PositionID)
	if err := e.rdb.HSet(ctx, key, h).Err(); err != nil {
		e.logf("auction %d state publish: %v", a.ID, err)
	}
	e.setPhaseKey(ctx, a)
}

func (e *AuctionEngine) setPhaseKey(ctx context.Context, a AuctionRow) {
	key := fmt.Sprintf("liquidation_auction:%d:phase", a.ID)
	if err := e.rdb.Set(ctx, key, a.Phase, AuctionPhaseTTL).Err(); err != nil {
		e.logf("auction %d phase key: %v", a.ID, err)
	}
}

func (e *AuctionEngine) setStateFields(ctx context.Context, a AuctionRow, fields map[string]string) {
	key := AuctionStateKey(a.InstrumentID, a.PositionID)
	args := make([]any, 0, len(fields)*2)
	for k, v := range fields {
		args = append(args, k, v)
	}
	if err := e.rdb.HSet(ctx, key, args...).Err(); err != nil {
		e.logf("auction %d state fields: %v", a.ID, err)
	}
	e.setPhaseKey(ctx, a)
}

func (e *AuctionEngine) appendEvent(ctx context.Context, auctionID int64,
	kind string, fields map[string]string) {

	ev := map[string]string{"kind": kind, "ts_ms": fmt.Sprint(e.now().UnixMilli())}
	for k, v := range fields {
		ev[k] = v
	}
	payload, err := json.Marshal(ev)
	if err != nil {
		e.logf("auction %d event marshal: %v", auctionID, err)
		return
	}
	if err := e.rdb.LPush(ctx, fmt.Sprintf("auction:%d:events", auctionID), payload).Err(); err != nil {
		e.logf("auction %d event push: %v", auctionID, err)
	}
}

// retireState drops the live auction keys once the auction terminates.
func (e *AuctionEngine) retireState(ctx context.Context, a AuctionRow) {
	if err := e.rdb.Del(ctx,
		AuctionStateKey(a.InstrumentID, a.PositionID),
		fmt.Sprintf("liquidation_auction:%d:phase", a.ID)).Err(); err != nil {
		e.logf("auction %d retire: %v", a.ID, err)
	}
}

func (e *AuctionEngine) raiseAlert(ctx context.Context, severity, code, summary string, details map[string]string) {
	if e.alerter == nil {
		e.logf("auction: alert %s undeliverable (no alerter): %s", code, summary)
		return
	}
	actx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := e.alerter.Raise(actx, OpsAlert{
		Severity: severity, Code: code, Summary: summary, Details: details,
	}); err != nil {
		e.logf("auction: alert %s dispatch failed: %v", code, err)
	}
}
