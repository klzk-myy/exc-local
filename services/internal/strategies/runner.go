// Run pipeline — the sweep that fires due recurring conversions and
// drift-triggered rebalances, plus the marketplace template service.
//
// Every run walks the same gate chain: account status → market hours
// (instruments.SessionService state) → BBO spread ceiling → order
// admission (which itself runs kill-switch → appropriateness → product
// gate → risk → balance). A failed gate produces a truthful SKIPPED run
// row with the reason code — never a silent drop, never a fabricated
// success. Executions are firm CLOB MARKET IOC orders through
// orders.Service (R14 — no RFQ path exists).
package strategies

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"exchange/internal/admin"
	"exchange/internal/orders"
	"exchange/pkg/decimal"
	excerrors "exchange/pkg/errors"
)

// stalePendingAge is how long a claimed-but-never-dispatched run may sit
// PENDING before the reconcile pass marks it FAILED — crash-recovery
// honesty (the run was claimed, the process died, the row must not lie
// forever).
const stalePendingAge = 10 * time.Minute

// Sweep executes one scheduler tick: due recurring slots, drift checks
// for rebalances, then reconciliation of in-flight runs. Per-strategy
// errors are logged via the returned count + stored on the run row —
// a single poisoned strategy never halts the sweep.
func (s *Service) Sweep(ctx context.Context) (int, error) {
	now := s.now()
	processed := 0

	due, err := s.st.DueRecurring(ctx, now)
	if err != nil {
		return processed, err
	}
	for i := range due {
		st := &due[i]
		if err := s.runRecurring(ctx, st, now); err != nil {
			processed++
			continue // error already recorded on the run row
		}
		processed++
	}

	reb, err := s.st.ActiveRebalances(ctx)
	if err != nil {
		return processed, err
	}
	for i := range reb {
		st := &reb[i]
		if err := s.evaluateRebalance(ctx, st, now); err != nil {
			continue
		}
	}

	if err := s.reconcile(ctx, now); err != nil {
		return processed, err
	}
	return processed, nil
}

// ---------------------------------------------------------------------------
// Recurring conversion runs
// ---------------------------------------------------------------------------

func (s *Service) runRecurring(ctx context.Context, st *Strategy, now time.Time) error {
	slot := *st.NextRunAt
	runID, claimed, err := s.st.ClaimRun(ctx, st, slot)
	if err != nil || !claimed {
		return err
	}

	skip := func(reason string, legs []RunLeg) {
		_ = s.st.FinishRun(ctx, runID, RunSkipped, reason, legs, nil)
	}

	// Gate 1 — account live.
	acct, err := s.ords.AccountByID(ctx, st.AccountID)
	if err != nil {
		return s.failRun(runID, err)
	}
	if acct == nil || acct.Status != "ACTIVE" {
		skip("ACCOUNT_INACTIVE", nil)
		return nil
	}
	// Gate 2 — market hours via the canonical session service.
	if s.sess == nil || !s.sess.Calendar().IsOpen(now) {
		state := "UNAVAILABLE"
		if s.sess != nil {
			state = string(s.sess.Calendar().State(now))
		}
		skip("MARKET_CLOSED", []RunLeg{{
			Currency: st.FromCurrency, Action: "CONVERT",
			Status: "SKIPPED", Note: "venue session " + state}})
		return nil
	}
	// Gate 3 — instrument still tradable + spread check.
	inst, side, err := s.resolvePair(ctx, st.FromCurrency, st.ToCurrency)
	if err != nil {
		skip("INSTRUMENT_UNAVAILABLE", nil)
		return nil
	}
	mid, spreadOK, spreadNote := s.spreadGate(ctx, inst)
	if !spreadOK {
		skip("SPREAD_TOO_WIDE", []RunLeg{{
			Currency: st.FromCurrency, Action: "CONVERT",
			Symbol: inst.Symbol, Status: "SKIPPED", Note: spreadNote}})
		return nil
	}

	// Expected counter-value at mid (the honest cost baseline).
	var expected decimal.Decimal
	if side == "SELL" {
		expected = st.Amount.Mul(mid)
	} else {
		expected = st.Amount.Div(mid)
	}
	legs := []RunLeg{{
		Currency: st.FromCurrency, Action: "CONVERT",
		Symbol: inst.Symbol, Side: side, Amount: st.Amount.String(),
		Status: "PENDING"}}

	// Firm CLOB market order — admission/risk/balance gates all run
	// inside Submit; a coded rejection becomes the truthful skip reason.
	req := &orders.SubmitRequest{
		Symbol:      inst.Symbol,
		Side:        side,
		OrderType:   orders.TypeMarket,
		TimeInForce: orders.TIFIOC,
	}
	if side == "SELL" {
		q := *st.Amount
		req.Quantity = &q
	} else {
		q := *st.Amount
		req.QuoteQuantity = &q // spend `amount` of from-ccy (the pair's quote)
	}
	ack, err := s.ords.Submit(ctx, acct, req)
	if err != nil {
		code := excerrors.CodeOf(err)
		var e *excerrors.Error
		if ok := errorAs(err, &e); ok && e != nil {
			skip(code, legs)
			return nil
		}
		return s.failRun(runID, err)
	}
	legs[0].Status = "PLACED"
	legs[0].OrderID = &ack.OrderID
	if err := s.st.MarkRunSubmitted(ctx, runID, []int64{ack.OrderID}, legs,
		*st.Amount, st.ToCurrency, &expected); err != nil {
		return err
	}
	return nil
}

func (s *Service) failRun(runID int64, err error) error {
	return s.st.FinishRun(context.Background(), runID, RunFailed,
		"INTERNAL", nil, nil)
}

// spreadGate returns the BBO mid + whether the spread is inside the
// instrument's ceiling. An empty book is a hard gate (MARKET IOC would
// just expire unfilled — skipping is the honest outcome).
func (s *Service) spreadGate(ctx context.Context,
	inst *orders.Instrument) (mid decimal.Decimal, ok bool, note string) {

	if s.book == nil {
		return decimal.Zero, false, "book snapshot unavailable (fail closed)"
	}
	snap, err := s.book.Snapshot(ctx, inst.Symbol, 1)
	if err != nil {
		return decimal.Zero, false, "book snapshot error: " + err.Error()
	}
	if snap == nil || len(snap.Bids) == 0 || len(snap.Asks) == 0 {
		return decimal.Zero, false, "no BBO — book empty"
	}
	bid, _ := decimal.NewFromString(snap.Bids[0].Price)
	ask, _ := decimal.NewFromString(snap.Asks[0].Price)
	mid = bid.Add(ask).Div(decimal.NewFromInt(2))
	if !mid.IsPositive() {
		return mid, false, "degenerate BBO"
	}
	if inst.MaxSpreadPips != nil && inst.MaxSpreadPips.IsPositive() &&
		inst.TickSize.IsPositive() {
		// 1 pip = 10 ticks (FX pipette convention); allowed spread in
		// bps = pips × pipSize / mid × 10⁴.
		pip := inst.TickSize.Mul(decimal.NewFromInt(10))
		allowedBps := inst.MaxSpreadPips.Mul(pip).
			Div(mid).Mul(decimal.NewFromInt(10000))
		actualBps := ask.Sub(bid).Div(mid).Mul(decimal.NewFromInt(10000))
		if actualBps.GreaterThan(allowedBps) {
			return mid, false, fmt.Sprintf(
				"spread %s bps exceeds ceiling %s bps",
				actualBps.Round(2), allowedBps.Round(2))
		}
	}
	return mid, true, ""
}

func errorAs(err error, target **excerrors.Error) bool {
	var e *excerrors.Error
	if errors.As(err, &e) {
		*target = e
		return true
	}
	return false
}

// ---------------------------------------------------------------------------
// Rebalance runs
// ---------------------------------------------------------------------------

// evaluateRebalance computes current allocation vs targets; a breach of
// the drift band opens one rebalance run.
func (s *Service) evaluateRebalance(ctx context.Context, st *Strategy, now time.Time) error {
	if s.balances == nil || s.usd == nil {
		return errorf(CodeInternal, "rebalance seams unwired")
	}
	open, err := s.st.HasOpenRun(ctx, st.StrategyID)
	if err != nil || open {
		return err
	}
	drift, vals, totalUSD, err := s.drift(ctx, st)
	if err != nil || !drift {
		return err
	}
	runID, claimed, err := s.st.ClaimRebalanceRun(ctx, st, now)
	if err != nil || !claimed {
		return err
	}
	return s.executeRebalance(ctx, st, runID, vals, totalUSD)
}

// drift computes per-currency USD values + whether any |act−target|
// exceeds the band.
func (s *Service) drift(ctx context.Context, st *Strategy) (bool,
	map[string]decimal.Decimal, decimal.Decimal, error) {

	rows, err := s.balances.BalancesFor(ctx, st.AccountID)
	if err != nil {
		return false, nil, decimal.Zero, err
	}
	vals := map[string]decimal.Decimal{}
	total := decimal.Zero
	for _, b := range rows {
		v, err := s.usd.ToUSD(ctx, b.Currency, b.Total)
		if err != nil {
			return false, nil, decimal.Zero, err
		}
		vals[b.Currency] = v
		total = total.Add(v)
	}
	if !total.IsPositive() {
		return false, vals, total, nil
	}
	band := st.DriftBandPct.Div(decimal.NewFromInt(100))
	for ccy, target := range st.Targets {
		act := vals[ccy].Div(total)
		if act.Sub(target).Abs().GreaterThan(band) {
			return true, vals, total, nil
		}
	}
	return false, vals, total, nil
}

// executeRebalance emits one MARKET IOC leg per breached currency: an
// overweight currency sells its excess USD worth, an underweight buys
// its deficit. USD is the valuation pivot — no FX leg for it.
func (s *Service) executeRebalance(ctx context.Context, st *Strategy,
	runID int64, vals map[string]decimal.Decimal, totalUSD decimal.Decimal) error {

	acct, err := s.ords.AccountByID(ctx, st.AccountID)
	if err != nil {
		return s.failRun(runID, err)
	}
	if acct == nil || acct.Status != "ACTIVE" {
		return s.st.FinishRun(ctx, runID, RunSkipped, "ACCOUNT_INACTIVE", nil, nil)
	}
	if s.sess == nil || !s.sess.Calendar().IsOpen(s.now()) {
		return s.st.FinishRun(ctx, runID, RunSkipped, "MARKET_CLOSED", nil, nil)
	}

	band := st.DriftBandPct.Div(decimal.NewFromInt(100))
	var legs []RunLeg
	var orderIDs []int64
	notional := decimal.Zero
	for ccy, target := range st.Targets {
		act := vals[ccy].Div(totalUSD)
		diff := target.Sub(act) // >0 underweight → buy
		if diff.Abs().LessThanOrEqual(band) {
			continue
		}
		diffUSD := diff.Mul(totalUSD).Abs() // USD amount to move
		leg := RunLeg{Currency: ccy, USDAmount: diffUSD.String(), Status: "PENDING"}
		if ccy == "USD" {
			leg.Status = "SKIPPED"
			leg.Note = "pivot currency — settles via counterparties"
			legs = append(legs, leg)
			continue
		}
		var inst *orders.Instrument
		var side string
		if diff.LessThan(decimal.Zero) {
			// Overweight: sell excess ccy into USD.
			leg.Action = "SELL_EXCESS"
			inst, side, err = s.resolvePair(ctx, ccy, "USD")
		} else {
			// Underweight: spend USD to acquire ccy.
			leg.Action = "BUY_DEFICIT"
			inst, side, err = s.resolvePair(ctx, "USD", ccy)
		}
		if err != nil || inst == nil {
			leg.Status = "SKIPPED"
			leg.Note = "no USD pair for " + ccy
			legs = append(legs, leg)
			continue
		}
		leg.Symbol = inst.Symbol
		leg.Side = side
		req := &orders.SubmitRequest{
			Symbol:      inst.Symbol,
			Side:        side,
			OrderType:   orders.TypeMarket,
			TimeInForce: orders.TIFIOC,
		}
		switch {
		case inst.QuoteCurrency == "USD" && side == "BUY":
			// BUY {ccy}/USD spending USD quote directly.
			q := diffUSD
			req.QuoteQuantity = &q
		case inst.BaseCurrency == "USD" && side == "SELL":
			// SELL {USD}/{ccy}: base qty IS the USD spend.
			q := diffUSD
			req.Quantity = &q
		case inst.QuoteCurrency == "USD" && side == "SELL":
			// SELL {ccy}/USD: qty = USD excess valued at reference.
			ref, rerr := s.rm.ReferencePrice(ctx, inst.ID)
			if rerr != nil || ref == nil || !ref.IsPositive() {
				leg.Status = "SKIPPED"
				leg.Note = "no reference price"
				legs = append(legs, leg)
				continue
			}
			q := diffUSD.Div(*ref)
			req.Quantity = &q
		default:
			// BUY on {USD}/{ccy} (quote=ccy): USD spend needs conversion
			// — use the reference price for the quote amount.
			ref, rerr := s.rm.ReferencePrice(ctx, inst.ID)
			if rerr != nil || ref == nil || !ref.IsPositive() {
				leg.Status = "SKIPPED"
				leg.Note = "no reference price"
				legs = append(legs, leg)
				continue
			}
			q := diffUSD.Mul(*ref)
			req.QuoteQuantity = &q
		}
		ack, serr := s.ords.Submit(ctx, acct, req)
		if serr != nil {
			leg.Status = "REJECTED"
			leg.Note = excerrors.CodeOf(serr)
			legs = append(legs, leg)
			continue
		}
		leg.Status = "PLACED"
		leg.OrderID = &ack.OrderID
		orderIDs = append(orderIDs, ack.OrderID)
		notional = notional.Add(diffUSD)
		legs = append(legs, leg)
	}
	if len(orderIDs) == 0 {
		return s.st.FinishRun(ctx, runID, RunSkipped, "NO_EXECUTABLE_LEGS", legs, nil)
	}
	return s.st.MarkRunSubmitted(ctx, runID, orderIDs, legs, notional, "USD", nil)
}

// ---------------------------------------------------------------------------
// Reconcile — settle SUBMITTED runs into COMPLETED with real costs
// ---------------------------------------------------------------------------

func (s *Service) reconcile(ctx context.Context, now time.Time) error {
	open, err := s.st.OpenRuns(ctx)
	if err != nil {
		return err
	}
	for i := range open {
		r := &open[i]
		if r.Status == RunPending {
			if r.CreatedAt.Add(stalePendingAge).Before(now) {
				_ = s.st.FinishRun(ctx, r.RunID, RunFailed,
					"RUN_ORPHANED", r.Legs, r.OrderIDs)
			}
			continue
		}
		if err := s.reconcileRun(ctx, r); err != nil {
			continue
		}
	}
	return nil
}

func (s *Service) reconcileRun(ctx context.Context, r *Run) error {
	st, err := s.st.GetByID(ctx, r.StrategyID)
	if err != nil || st == nil {
		return err
	}
	allTerminal := true
	executed := decimal.Zero
	fees := decimal.Zero
	for i := range r.Legs {
		leg := &r.Legs[i]
		if leg.OrderID == nil {
			continue
		}
		o, err := s.rm.GetOrder(ctx, *leg.OrderID)
		if err != nil {
			return err
		}
		if o == nil {
			// An order id we submitted must exist — treat a miss as
			// non-terminal rather than completing with fabricated zeros.
			allTerminal = false
			continue
		}
		switch o.Status {
		case "FILLED", "PARTIALLY_FILLED":
			// part of executed value — computed below
		case "CANCELLED", "EXPIRED", "REJECTED":
			// terminal with whatever filled
		default:
			allTerminal = false
			continue
		}
		fee, ferr := s.st.FeeForOrder(ctx, o.ID, o.Side)
		if ferr == nil {
			fees = fees.Add(fee)
		}
		leg.Status = o.Status
		if o.AvgFillPrice != nil && o.FilledQty.IsPositive() {
			// Value in the run's reporting ccy: SELL yields quote
			// (qty×px); BUY yields base qty (quote_ccy-denominated
			// orders bought base directly).
			if o.Side == "SELL" {
				executed = executed.Add(o.FilledQty.Mul(*o.AvgFillPrice))
			} else {
				executed = executed.Add(o.FilledQty)
			}
		}
	}
	if !allTerminal {
		return nil
	}
	r.Status = RunCompleted
	r.ExecutedValue = &executed
	r.Fees = fees
	// Expected is in the run's reporting currency; pro-rate for partial
	// fills so a half-filled conversion reports half the expected value.
	if r.ExpectedValue != nil {
		r.SpreadCost = r.ExpectedValue.Sub(executed)
		r.RealizedPnL = executed.Sub(*r.ExpectedValue).Sub(fees)
	}
	return s.st.CompleteRun(ctx, r, st)
}

// ---------------------------------------------------------------------------
// Marketplace — publish / approve / reject / instantiate
// ---------------------------------------------------------------------------

// PublishTemplate validates the config (strict allowlist — config-only,
// no executable payloads) and registers a PENDING_APPROVAL template.
func (s *Service) PublishTemplate(ctx context.Context, accountID int64,
	in TemplatePublishInput) (*Template, error) {

	kind := strings.ToUpper(strings.TrimSpace(in.Kind))
	cfg, err := parseTemplateConfig(kind, in.Config)
	if err != nil {
		return nil, err
	}
	// The config must validate as a real strategy — a template that
	// can't instantiate is unapprovable.
	if _, err := s.validateConfig(ctx, cfg); err != nil {
		return nil, err
	}
	t := &Template{
		Name:               strings.TrimSpace(in.Name),
		Description:        strings.TrimSpace(in.Description),
		Kind:               kind,
		Config:             in.Config,
		Status:             TemplatePending,
		PublisherAccountID: accountID,
	}
	if t.Name == "" {
		return nil, errorf(CodeStrategyConfigInvalid, "name is required")
	}
	if err := s.st.PublishTemplate(ctx, t); err != nil {
		return nil, err
	}
	return t, nil
}

// parseTemplateConfig decodes template config into CreateInput with a
// strict field allowlist — unknown keys (which would be the executable-
// payload smuggle vector) are rejected outright.
func parseTemplateConfig(kind string, raw json.RawMessage) (*CreateInput, error) {
	var in CreateInput
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		return nil, errorf(CodeStrategyConfigInvalid,
			"template config invalid: %v", err)
	}
	in.Kind = kind
	return &in, nil
}

// ListTemplates serves the marketplace (APPROVED only for clients;
// status="" returns everything for admin review).
func (s *Service) ListTemplates(ctx context.Context, status string) ([]Template, error) {
	out, err := s.st.Templates(ctx, status)
	if out == nil {
		out = []Template{}
	}
	return out, err
}

// DecideTemplate applies the admin approve/reject decision with an
// audit-log row in the same transaction.
func (s *Service) DecideTemplate(ctx context.Context, templateID int64,
	approve bool, adminUserID int64, ip, reason string) (*Template, error) {

	action := "strategy_template.approve"
	if !approve {
		action = "strategy_template.reject"
	}
	t, err := s.st.DecideTemplate(ctx, templateID, approve, reason,
		admin.AuditEntry{
			AdminUserID: adminUserID,
			Action:      action,
			TargetType:  "strategy_template",
			TargetID:    &templateID,
			IPAddress:   ip,
		})
	if err != nil {
		return nil, err
	}
	if t == nil {
		return nil, errorf(CodeNotFound, "template %d not found", templateID)
	}
	return t, nil
}

// Instantiate copies an APPROVED template's configuration onto a new
// account-owned strategy — configuration only, never executable code,
// and copy-by-value so later template changes cannot mutate the live
// strategy.
func (s *Service) Instantiate(ctx context.Context, accountID,
	templateID int64) (*Strategy, error) {

	t, err := s.st.Template(ctx, templateID)
	if err != nil {
		return nil, err
	}
	if t == nil {
		return nil, errorf(CodeNotFound, "template %d not found", templateID)
	}
	if t.Status != TemplateApproved {
		return nil, errorf(CodeTemplateNotApproved,
			"template %d is %s — only APPROVED templates instantiate",
			templateID, t.Status)
	}
	in, err := parseTemplateConfig(t.Kind, t.Config)
	if err != nil {
		return nil, err
	}
	st, err := s.validateConfig(ctx, in)
	if err != nil {
		return nil, err
	}
	st.AccountID = accountID
	st.TemplateID = &templateID
	if st.Label == "" {
		st.Label = t.Name
	}
	if err := s.st.Create(ctx, st); err != nil {
		return nil, err
	}
	return st, nil
}
