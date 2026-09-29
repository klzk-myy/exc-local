package algo

// Phase-16 Task 16.3.16 — Guaranteed Stop-Loss Orders (Go half).
//
// The engine sibling guarantees the exact-stop fill; the Go side owns
// the money movement per spec §24 #252 / Phase-16 Task 16.3.16:
//
//   - premium = notional × gslo_rate × distance_to_stop / current_price
//     charged at placement (debited 2010_CUSTOMER_LIABILITY, credited
//     2210_INSURANCE_FUND_LIABILITY — the premium pool the insurance
//     fund draws on),
//   - premium refund on pre-trigger cancellation,
//   - per-instrument exposure cap (gap liability aggregated over open
//     GSLO orders; configurable per instrument via
//     instruments.param_overrides->'gslo' — Risk Manager seam),
//   - insurance-fund gap absorption: when the executed fill is worse
//     than the guaranteed stop the client is made whole from the fund
//     (debit 2210, credit 2010) — never a silent loss.
//
// Config seam (migration 219 param_overrides JSONB):
//
//	{"gslo": {"rate_bps": "10", "max_exposure_quote": "5000000"}}
//
// Defaults when unset: rate_bps 10 (0.10%), max_exposure_quote 5,000,000.
// A malformed override fails closed (submission rejected).

import (
	"context"
	"encoding/json"
	"fmt"
	"math"

	"github.com/jackc/pgx/v5/pgxpool"

	"exchange/internal/ledger"
	"exchange/internal/orders"
	"exchange/pkg/decimal"
	excerrors "exchange/pkg/errors"
)

// GSLOService implements orders.GSLOHooks plus the fill-event gap
// absorber bound to orders.Consumer.WithGSLOHook.
type GSLOService struct {
	pool   *pgxpool.Pool
	txPost JournalPoster
	pub    EventPublisher
}

// NewGSLOService wires the seam; txPost may be nil only in tests that
// never reach a journal call (admission-only coverage).
func NewGSLOService(pool *pgxpool.Pool, txPost JournalPoster, pub EventPublisher) *GSLOService {
	return &GSLOService{pool: pool, txPost: txPost, pub: pub}
}

// gsloConfig is the per-instrument knob block resolved from
// instruments.param_overrides (migration 219) — RateBps is the premium
// rate in basis points of the stop-distance notional; MaxExposureQuote
// caps the aggregate worst-case gap liability per instrument.
type gsloConfig struct {
	RateBps          decimal.Decimal
	MaxExposureQuote decimal.Decimal
}

func (s *GSLOService) config(ctx context.Context, instrumentID int64) (gsloConfig, error) {
	cfg := gsloConfig{
		RateBps:          decimal.RequireFromString("10"),
		MaxExposureQuote: decimal.RequireFromString("5000000"),
	}
	var overrides []byte
	err := s.pool.QueryRow(ctx, `
		SELECT COALESCE(param_overrides->'gslo','null'::jsonb)
		FROM instruments WHERE id=$1`, instrumentID).Scan(&overrides)
	if err != nil {
		return cfg, fmt.Errorf("gslo config: %w", err)
	}
	if string(overrides) == "null" {
		return cfg, nil
	}
	var raw struct {
		RateBps     string `json:"rate_bps"`
		MaxExposure string `json:"max_exposure_quote"`
	}
	if err := json.Unmarshal(overrides, &raw); err != nil {
		return cfg, fmt.Errorf("gslo param_overrides unreadable: %w", err)
	}
	if raw.RateBps != "" {
		r, err := decimal.NewFromString(raw.RateBps)
		if err != nil || !r.IsPositive() {
			return cfg, excerrors.New("INVALID_REQUEST", fmt.Sprintf("gslo rate_bps override malformed on instrument %d", instrumentID))
		}
		cfg.RateBps = r
	}
	if raw.MaxExposure != "" {
		m, err := decimal.NewFromString(raw.MaxExposure)
		if err != nil || m.IsNegative() {
			return cfg, excerrors.New("INVALID_REQUEST", fmt.Sprintf("gslo max_exposure_quote override malformed on instrument %d", instrumentID))
		}
		cfg.MaxExposureQuote = m
	}
	return cfg, nil
}

// gapLiability is the worst-case insurance-fund payout for one GSLO
// order at the current reference: qty × |ref − stop| in quote terms.
func gapLiability(qty, ref, stop decimal.Decimal) decimal.Decimal {
	return qty.Mul(ref.Sub(stop).Abs()).Round(8)
}

// premiumOf implements the Task 16.3.16 formula:
// premium = notional × gslo_rate × distance_to_stop / current_price,
// which simplifies to qty × rate × |ref − stop| (quote currency).
func premiumOf(qty, ref, stop, rateBps decimal.Decimal) decimal.Decimal {
	rate := rateBps.Div(decimal.RequireFromString("10000"))
	return qty.Mul(rate).Mul(ref.Sub(stop).Abs()).Round(8)
}

// AdmitSubmit enforces the per-instrument exposure cap and stamps the
// computed premium into req.AlgoParams (the carry channel to
// ChargePremium — the hook contract returns only error).
func (s *GSLOService) AdmitSubmit(ctx context.Context, acct *orders.Account,
	inst *orders.Instrument, req *orders.SubmitRequest, ref *decimal.Decimal) error {
	if req.StopPrice == nil || !req.StopPrice.IsPositive() {
		return excerrors.New("INVALID_REQUEST", "gslo requires a positive stop_price")
	}
	if ref == nil || !ref.IsPositive() {
		return excerrors.New("INVALID_REQUEST", fmt.Sprintf("no reference price for gslo premium on %s (fail closed)", inst.Symbol))
	}
	qty := decimal.Zero
	if req.Quantity != nil {
		qty = *req.Quantity
	}
	if req.QuoteQuantity != nil {
		qty = req.QuoteQuantity.Div(*ref)
	}
	if !qty.IsPositive() {
		return excerrors.New("INVALID_REQUEST", "gslo requires a positive quantity")
	}
	cfg, err := s.config(ctx, inst.ID)
	if err != nil {
		return err
	}
	premium := premiumOf(qty, *ref, *req.StopPrice, cfg.RateBps)

	// Aggregate open GSLO gap liability on the instrument — per-order
	// open qty × |ref − stop| in quote terms (the worst-case fund payout
	// if the market gapped to the reference right now).
	var exposureTxt string
	err = s.pool.QueryRow(ctx, `
		SELECT COALESCE(SUM((quantity - filled_qty) * ABS($2::numeric - stop_price)),0)::text
		FROM orders
		WHERE instrument_id=$1 AND gslo AND stop_price IS NOT NULL
		  AND status IN ('PENDING','RESERVED','ACTIVE','PARTIALLY_FILLED')`,
		inst.ID, ref.String()).Scan(&exposureTxt)
	if err != nil {
		return fmt.Errorf("gslo exposure query: %w", err)
	}
	exposure := decimal.RequireFromString(exposureTxt)
	if exposure.Add(gapLiability(qty, *ref, *req.StopPrice)).Cmp(cfg.MaxExposureQuote) > 0 {
		return excerrors.New("GSLO_EXPOSURE_EXCEEDED",
			fmt.Sprintf("gslo exposure cap on %s: open %s + new %s > max %s (quote)",
				inst.Symbol, exposure,
				gapLiability(qty, *ref, *req.StopPrice), cfg.MaxExposureQuote))
	}

	// Premium affordability — the charge lands on available quote funds.
	var availTxt string
	if err := s.pool.QueryRow(ctx, `
		SELECT available::text FROM balances
		WHERE account_id=$1 AND currency=$2`,
		acct.ID, inst.QuoteCurrency).Scan(&availTxt); err != nil {
		return fmt.Errorf("gslo balance read: %w", err)
	}
	if decimal.RequireFromString(availTxt).Cmp(premium) < 0 {
		return excerrors.New("PREMIUM_INSUFFICIENT", fmt.Sprintf("gslo premium %s %s exceeds available balance", premium, inst.QuoteCurrency))
	}

	// Carry the premium to ChargePremium via algo_params (merged —
	// sibling fields such as fixing_reservation stay untouched).
	var merged map[string]any
	if len(req.AlgoParams) > 0 {
		if err := json.Unmarshal(req.AlgoParams, &merged); err != nil {
			return excerrors.New("INVALID_REQUEST", "algo_params unreadable")
		}
	} else {
		merged = map[string]any{}
	}
	merged["gslo_premium"] = premium.String()
	merged["gslo_rate_bps"] = cfg.RateBps.String()
	blob, _ := json.Marshal(merged)
	req.AlgoParams = blob
	return nil
}

// gsloPremium reads the stamped premium out of the persisted order.
func gsloPremium(o *orders.Order) decimal.Decimal {
	if len(o.AlgoParams) == 0 {
		return decimal.Zero
	}
	var p struct {
		Premium string `json:"gslo_premium"`
	}
	if err := json.Unmarshal(o.AlgoParams, &p); err != nil {
		return decimal.Zero
	}
	if p.Premium == "" {
		return decimal.Zero
	}
	d, err := decimal.NewFromString(p.Premium)
	if err != nil {
		return decimal.Zero
	}
	return d
}

// ChargePremium posts the placement premium: client liability −premium,
// insurance-fund liability +premium (the premium pool). Idempotency key
// per order.
func (s *GSLOService) ChargePremium(ctx context.Context, o *orders.Order,
	inst *orders.Instrument) error {
	premium := gsloPremium(o)
	if !premium.IsPositive() {
		return nil
	}
	if s.txPost == nil {
		return fmt.Errorf("gslo: journal poster unavailable")
	}
	jr, err := s.txPost.Post(ctx, ledger.Journal{
		EntryType:   ledger.EntryFee,
		ReferenceID: o.ID,
		Description: fmt.Sprintf("GSLO premium order %d %.8s %s", o.ID, premium, inst.QuoteCurrency),
		PostedBy:    "algo-gslo",
		IdempotencyKey: fmt.Sprintf("gslo-premium:%d",
			o.ID),
		Lines: []ledger.Line{
			ledger.DebitLine(ledger.CustomerLiability(inst.QuoteCurrency),
				inst.QuoteCurrency, premium, "gslo premium charged"),
			ledger.CreditLine(ledger.InsuranceFundLiability(inst.QuoteCurrency),
				inst.QuoteCurrency, premium, "gslo premium pool"),
		},
		Effects: []ledger.AccountEffect{{
			AccountID:      o.AccountID,
			Currency:       inst.QuoteCurrency,
			AvailableDelta: premium.Neg(),
		}},
	})
	if err != nil {
		return err
	}
	s.dispatchEvents(jr.Events)
	return nil
}

// RefundPremium reverses the charge for a pre-trigger cancellation —
// same journal shape, signs flipped.
func (s *GSLOService) RefundPremium(ctx context.Context, o *orders.Order,
	inst *orders.Instrument) error {
	premium := gsloPremium(o)
	if !premium.IsPositive() {
		return nil
	}
	if s.txPost == nil {
		return fmt.Errorf("gslo: journal poster unavailable")
	}
	jr, err := s.txPost.Post(ctx, ledger.Journal{
		EntryType:      ledger.EntryFee,
		ReferenceID:    o.ID,
		Description:    fmt.Sprintf("GSLO premium refund order %d %.8s %s", o.ID, premium, inst.QuoteCurrency),
		PostedBy:       "algo-gslo",
		IdempotencyKey: fmt.Sprintf("gslo-refund:%d", o.ID),
		Lines: []ledger.Line{
			ledger.DebitLine(ledger.InsuranceFundLiability(inst.QuoteCurrency),
				inst.QuoteCurrency, premium, "gslo premium refunded on cancel"),
			ledger.CreditLine(ledger.CustomerLiability(inst.QuoteCurrency),
				inst.QuoteCurrency, premium, "gslo premium refund"),
		},
		Effects: []ledger.AccountEffect{{
			AccountID:      o.AccountID,
			Currency:       inst.QuoteCurrency,
			AvailableDelta: premium,
		}},
	})
	if err != nil {
		return err
	}
	s.dispatchEvents(jr.Events)
	return nil
}

// OnFill is bound to orders.Consumer.WithGSLOHook: for a GSLO order
// filled WORSE than its guaranteed stop the client is made whole from
// the insurance fund. Sell stop: comp = (stop − fill) × qty when
// fill < stop. Buy stop: comp = (fill − stop) × qty when fill > stop.
// The journal is keyed on the matching trades row so replayed events
// cannot double-compensate.
func (s *GSLOService) OnFill(ctx context.Context, orderID int64,
	fillPrice, fillQty decimal.Decimal) error {
	var side, status, quote string
	var stopTxt *string
	var accountID, instrumentID int64
	var gslo bool
	err := s.pool.QueryRow(ctx, `
		SELECT o.side::text, o.status::text, o.stop_price::text, o.account_id,
		       o.instrument_id, o.gslo, i.quote_currency
		FROM orders o JOIN instruments i ON i.id=o.instrument_id
		WHERE o.id=$1`, orderID).
		Scan(&side, &status, &stopTxt, &accountID, &instrumentID, &gslo, &quote)
	if err != nil {
		return fmt.Errorf("gslo onfill order %d: %w", orderID, err)
	}
	var stop *decimal.Decimal
	if stopTxt != nil {
		d, derr := decimal.NewFromString(*stopTxt)
		if derr != nil {
			return fmt.Errorf("gslo onfill %d stop unreadable: %w", orderID, derr)
		}
		stop = &d
	}
	if !gslo || stop == nil || !stop.IsPositive() {
		return nil
	}
	var comp decimal.Decimal
	switch side {
	case "SELL":
		if fillPrice.LessThan(*stop) {
			comp = stop.Sub(fillPrice).Mul(fillQty).Round(8)
		}
	case "BUY":
		if fillPrice.GreaterThan(*stop) {
			comp = fillPrice.Sub(*stop).Mul(fillQty).Round(8)
		}
	}
	if !comp.IsPositive() {
		return nil // filled at-or-better — the guarantee cost nothing
	}

	// Idempotency anchor: the trades row the engine wrote for this fill
	// — same order + price + qty. Missing row ⇒ no anchor ⇒ fail closed
	// (a compensation journal without a provable trade is a fabrication
	// risk; the audit trail records the miss for ops).
	var tradeID int64
	err = s.pool.QueryRow(ctx, `
		SELECT id FROM trades
		WHERE (buy_order_id=$1 OR sell_order_id=$1)
		  AND price=$2::numeric AND quantity=$3::numeric
		ORDER BY id DESC LIMIT 1`,
		orderID, fillPrice.String(), fillQty.String()).Scan(&tradeID)
	if err != nil {
		return fmt.Errorf("gslo gap %d: no anchor trade for %s@%s (fail closed)",
			orderID, fillQty, fillPrice)
	}
	if s.txPost == nil {
		return fmt.Errorf("gslo: journal poster unavailable")
	}
	jr, err := s.txPost.Post(ctx, ledger.Journal{
		EntryType:      ledger.EntryAdjustment,
		ReferenceID:    orderID,
		Description:    fmt.Sprintf("GSLO gap compensation order %d %.8s %s (stop %s fill %s)", orderID, comp, quote, stop, fillPrice),
		PostedBy:       "algo-gslo",
		IdempotencyKey: fmt.Sprintf("gslo-gap:%d:%d", orderID, tradeID),
		Lines: []ledger.Line{
			ledger.DebitLine(ledger.InsuranceFundLiability(quote), quote, comp,
				"insurance fund absorbs gslo gap"),
			ledger.CreditLine(ledger.CustomerLiability(quote), quote, comp,
				"client made whole at guaranteed stop"),
		},
		Effects: []ledger.AccountEffect{{
			AccountID:      accountID,
			Currency:       quote,
			AvailableDelta: comp,
		}},
	})
	if err != nil {
		return err
	}
	s.dispatchEvents(jr.Events)
	return nil
}

func (s *GSLOService) dispatchEvents(events []ledger.BalanceEvent) {
	if s.pub == nil {
		return
	}
	for _, ev := range events {
		payload, err := json.Marshal(ev)
		if err != nil {
			continue
		}
		_ = s.pub.Publish(context.Background(),
			ledger.BalanceChangedSubject(ev.AccountID), payload)
	}
}

// Compile-time guards — the service satisfies the orders hook contract
// and the consumer fill-hook shape.
var (
	_ orders.GSLOHooks = (*GSLOService)(nil)
	_                  = func(ctx context.Context, id int64, p, q decimal.Decimal) error {
		return (*GSLOService)(nil).OnFill(ctx, id, p, q)
	}
	_ = math.MaxInt // keep math import for future cap math without churn
)
