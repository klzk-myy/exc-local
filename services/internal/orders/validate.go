// Pure request/business validation for the order pipeline — every rule
// returns a coded *pkgerrors.Error so the §23 registry owns the HTTP
// status. No I/O lives here; callers fetch Instrument/Account snapshots
// and pass them in.
package orders

import (
	"fmt"
	"time"

	"exchange/pkg/decimal"
	excerrors "exchange/pkg/errors"
)

// codeErr builds the coded error the gateway envelope emits.
func codeErr(code, format string, args ...any) *excerrors.Error {
	return excerrors.New(code, fmt.Sprintf(format, args...))
}

// --- enum sets --------------------------------------------------------------

// gtcMaxLifetimeDays — spec §27 R8: no resting order may outlive 90
// calendar days. GTC submissions auto-convert to GTD at the cap;
// explicit GTD dates beyond it clamp down.
const gtcMaxLifetimeDays = 90

func validSide(s string) bool { return s == SideBuy || s == SideSell }
func validTIF(t string) bool {
	switch t {
	case TIFGTC, TIFIOC, TIFFOK, TIFGTD, TIFDAY:
		return true
	}
	return false
}

// wireTypes is the order-type set the FlatBuffers schema (exc.wire.
// OrderType) can express; richer §5.4 types are Phase-16+ surfaces and
// fail closed here rather than being silently truncated onto the wire.
func validWireType(t string) bool {
	switch t {
	case TypeLimit, TypeMarket, TypeStop, TypeStopLimit, TypeIceberg,
		// TypePeg is engine-owned (Phase-16 Task 16.3.11); TypeFixing
		// never reaches the wire — orders.Service queues it for the
		// fixing executor (Task 16.3.9).
		TypePeg:
		return true
	}
	return false
}

var validSTPModes = map[string]bool{
	"CANCEL_NEWEST": true, "CANCEL_OLDEST": true, "CANCEL_BOTH": true,
	"DECREMENT": true, "NONE": true,
}

// --- instrument lifecycle gate (spec §7.1 / §6.9 #3) -------------------------

// newOrderStateGate rejects order entry per instrument state. Cancels are
// never gated here (§7.1: cancels allowed in every state). DELISTED
// admits reduce_only entries during the 30-day close-only window
// (spec §7.1 remediation #35; core/src/risk/PreTradeChecker.cpp
// implements the same flag check).
func newOrderStateGate(inst *Instrument, reduceOnly bool) error {
	switch inst.Status {
	case "ACTIVE":
		return nil
	case "CANCEL_ONLY":
		return codeErr("INSTRUMENT_CANCEL_ONLY",
			"instrument %s accepts cancellations only", inst.Symbol)
	case "SUSPENDED", "DRAFT":
		// DRAFT has no book yet — same fail-closed semantics as SUSPENDED.
		return codeErr("INSTRUMENT_SUSPENDED",
			"instrument %s is not active (%s)", inst.Symbol, inst.Status)
	case "HALTED":
		return codeErr("INSTRUMENT_HALTED",
			"instrument %s is halted", inst.Symbol)
	case "DELISTED":
		if !reduceOnly {
			return codeErr("INSTRUMENT_DELISTED",
				"instrument %s is delisted — only reduce-only closing orders are accepted during the close-only window", inst.Symbol)
		}
		return nil
	case "RESTRICTED":
		return nil // order-type restriction enforced separately
	default:
		return codeErr("INSTRUMENT_SUSPENDED",
			"instrument %s in unknown state %q (fail closed)", inst.Symbol, inst.Status)
	}
}

// amendStateGate mirrors spec §6.9 #3: amends rejected in
// CANCEL_ONLY/SUSPENDED/HALTED/DELISTED with the per-state code;
// AMEND_IN_AUCTION_REJECTED is reserved for the CALL state (not yet a
// reachable instruments.status enum value — kept for completeness).
func amendStateGate(inst *Instrument) error {
	switch inst.Status {
	case "ACTIVE", "RESTRICTED":
		// RESTRICTED amends stay limit-shaped — the qty/price checks below
		// still apply; a RESTRICTED book keeps its resting limits.
		return nil
	case "CALL":
		return codeErr("AMEND_IN_AUCTION_REJECTED",
			"instrument %s is in call auction", inst.Symbol)
	case "CANCEL_ONLY":
		return codeErr("INSTRUMENT_CANCEL_ONLY",
			"instrument %s accepts cancellations only", inst.Symbol)
	case "SUSPENDED", "DRAFT":
		return codeErr("INSTRUMENT_SUSPENDED",
			"instrument %s is not active (%s)", inst.Symbol, inst.Status)
	case "HALTED":
		return codeErr("INSTRUMENT_HALTED",
			"instrument %s is halted", inst.Symbol)
	case "DELISTED":
		return codeErr("INSTRUMENT_DELISTED",
			"instrument %s is delisted", inst.Symbol)
	default:
		return codeErr("INSTRUMENT_SUSPENDED",
			"instrument %s in unknown state %q (fail closed)", inst.Symbol, inst.Status)
	}
}

// --- submit validation -------------------------------------------------------

// ValidateSubmit checks a normalized SubmitRequest against the account,
// instrument and current time. refPrice is the best available reference
// (last trade / mark) used for band and notional evaluation — nil means
// "no reference", which skips band checks rather than failing closed:
// the engine re-checks at match time.
func ValidateSubmit(req *SubmitRequest, inst *Instrument, acct *Account,
	refPrice *decimal.Decimal, now time.Time) error {

	if acct == nil {
		return codeErr("UNAUTHORIZED", "account context required")
	}
	if acct.Status == "FROZEN" {
		return codeErr("ACCOUNT_FROZEN",
			"account is compliance-frozen")
	}
	if acct.Status != "ACTIVE" {
		return codeErr("FORBIDDEN",
			"account status %s cannot trade", acct.Status)
	}
	// §23 KYC_REQUIRED — order submission requires a verified tier.
	if acct.KycTier == "T0" {
		return codeErr("KYC_REQUIRED",
			"KYC verification required before trading")
	}
	if inst == nil {
		return codeErr("INVALID_REQUEST", "symbol is required")
	}
	if err := newOrderStateGate(inst, req.ReduceOnly); err != nil {
		return err
	}
	if req.Symbol == "" || !validSide(req.Side) || req.OrderType == "" {
		return codeErr("INVALID_REQUEST",
			"symbol, side (BUY|SELL) and type are required")
	}
	// TypeFixing / MOO / MOC are deliberately not wire types —
	// orders.Service queues them for the fixing executor (Task 16.3.9)
	// or the §6.2b session call-auction (Task 16.3.25) instead of
	// dispatching at submit time. Every other §5.4 type fails closed.
	if !validWireType(req.OrderType) && req.OrderType != TypeFixing &&
		!IsAuctionType(req.OrderType) {
		return codeErr("INVALID_REQUEST",
			"unsupported order type %q (wire types: LIMIT, MARKET, STOP, STOP_LIMIT, ICEBERG, PEG; gateway-queued: FIXING, MOO, MOC)",
			req.OrderType)
	}
	if inst.Status == "RESTRICTED" && req.OrderType != TypeLimit {
		return codeErr("INSTRUMENT_RESTRICTED",
			"instrument %s is RESTRICTED: limit orders only", inst.Symbol)
	}
	if IsAuctionType(req.OrderType) {
		// §6.2b: session-bound orders are implicitly GTD to their auction
		// trigger — an explicit gtd_expiry or a continuous-matching TIF
		// would contradict the queue semantics.
		req.TimeInForce = TIFGTD
	}
	if req.TimeInForce == "" {
		req.TimeInForce = TIFGTC
	}
	if !validTIF(req.TimeInForce) {
		return codeErr("INVALID_REQUEST", "invalid time_in_force %q", req.TimeInForce)
	}
	if req.TimeInForce == TIFGTD && !IsAuctionType(req.OrderType) {
		if req.GTDExpiry == nil {
			return codeErr("INVALID_REQUEST", "GTD requires gtd_expiry")
		}
		if !req.GTDExpiry.After(now) {
			return codeErr("INVALID_REQUEST", "gtd_expiry must be in the future")
		}
	}
	// Spec §27 R8 — GTC max order lifetime is 90 calendar days:
	// auto-convert GTC to GTD at the cap so the engine's deterministic
	// expiry machinery emits the auto-expire (GTD_EXPIRED). An explicit
	// GTD beyond the cap clamps down — the ceiling applies to every
	// resting order, not just the GTC spelling.
	cap90 := now.AddDate(0, 0, gtcMaxLifetimeDays)
	switch req.TimeInForce {
	case TIFGTC:
		req.TimeInForce = TIFGTD
		if req.GTDExpiry == nil || req.GTDExpiry.After(cap90) {
			req.GTDExpiry = &cap90
		}
	case TIFGTD:
		if req.GTDExpiry != nil && req.GTDExpiry.After(cap90) {
			req.GTDExpiry = &cap90
		}
	}
	if req.STPMode != "" && !validSTPModes[req.STPMode] {
		return codeErr("INVALID_REQUEST", "invalid stp_mode %q", req.STPMode)
	}
	if len(req.ClientOrderID) > 64 {
		return codeErr("INVALID_REQUEST", "client_order_id exceeds 64 chars")
	}

	// §22.1: market orders accept exactly one of quantity/quote_quantity.
	// MOO/MOC share the market-order quantity contract — they ARE
	// market orders, bound to a session auction (§6.2b).
	if req.OrderType == TypeMarket || IsAuctionType(req.OrderType) {
		if (req.Quantity == nil) == (req.QuoteQuantity == nil) {
			return codeErr("QUOTE_QUANTITY_INVALID",
				"market orders require exactly one of quantity or quote_quantity")
		}
		if req.QuoteQuantity != nil && !req.QuoteQuantity.IsPositive() {
			return codeErr("QUOTE_QUANTITY_INVALID", "quote_quantity must be positive")
		}
	} else {
		if req.QuoteQuantity != nil {
			return codeErr("QUOTE_QUANTITY_INVALID",
				"quote_quantity is only valid on MARKET orders")
		}
		if req.Quantity == nil || !req.Quantity.IsPositive() {
			return codeErr("INVALID_REQUEST", "quantity must be positive")
		}
	}

	// Price requirements per type. §6.2b queue semantics: a price,
	// stop, iceberg display or maker-only flag cannot express a
	// session-uncross order.
	if IsAuctionType(req.OrderType) {
		if req.Price != nil || req.StopPrice != nil {
			return codeErr("INVALID_REQUEST",
				"%s orders do not accept price or stop_price (session-uncross market orders)",
				req.OrderType)
		}
		if req.PostOnly {
			return codeErr("INVALID_REQUEST",
				"post_only does not apply to %s orders", req.OrderType)
		}
	}
	switch req.OrderType {
	case TypeLimit, TypeIceberg:
		if req.Price == nil || !req.Price.IsPositive() {
			return codeErr("INVALID_REQUEST", "price required for %s orders", req.OrderType)
		}
	case TypeStop:
		// A trailing stop (offset + unit, first-class or via algo_params)
		// anchors from the reference price — no initial stop_price needed.
		trailing := req.TrailingOffset != nil || req.TrailingOffsetUnit != "" ||
			req.AlgoType == AlgoTrailingStop
		if !trailing && (req.StopPrice == nil || !req.StopPrice.IsPositive()) {
			return codeErr("INVALID_REQUEST", "stop_price required for STOP orders")
		}
	case TypeStopLimit:
		if req.StopPrice == nil || !req.StopPrice.IsPositive() ||
			req.Price == nil || !req.Price.IsPositive() {
			return codeErr("INVALID_REQUEST",
				"price and stop_price required for STOP_LIMIT orders")
		}
		// TypePeg: quantity is enforced by the generic branch above;
		// price/stop/display stay optional — the engine's peg tracker
		// owns repricing (Task 16.3.11). TypeFixing: all field rules
		// live in validateExecParams (Task 16.3.9).
	}
	if req.DisplayQty != nil {
		if req.OrderType != TypeIceberg {
			return codeErr("INVALID_REQUEST",
				"iceberg_visible_qty is only valid on ICEBERG orders")
		}
		if !req.DisplayQty.IsPositive() {
			return codeErr("INVALID_REQUEST", "iceberg_visible_qty must be positive")
		}
	}

	// Filter checks (Task 5.3.35 vocabulary): LOT_SIZE / PRICE_FILTER /
	// MIN_NOTIONAL / PRICE_BAND.
	if err := validateFilters(req, inst, refPrice); err != nil {
		return err
	}
	// Phase-16 Task 16.3.10 — exec-param surface (peg / hidden / GSLO /
	// trigger_source / fixing benchmark / algo_params + post_only·MARKET
	// and iceberg visibility rules).
	return validateExecParams(req)
}

// validateFilters applies the structured filter rules of Task 5.3.35.
func validateFilters(req *SubmitRequest, inst *Instrument, refPrice *decimal.Decimal) error {
	if req.Quantity != nil {
		q := *req.Quantity
		if q.LessThan(inst.MinOrderQty) {
			return codeErr("INVALID_REQUEST",
				"quantity %s below min_order_qty %s", q, inst.MinOrderQty)
		}
		if q.GreaterThan(inst.MaxOrderQty) {
			return codeErr("INVALID_REQUEST",
				"quantity %s exceeds max_order_qty %s", q, inst.MaxOrderQty)
		}
		if inst.LotSize.IsPositive() && !q.Mod(inst.LotSize).IsZero() {
			return codeErr("INVALID_REQUEST",
				"quantity %s is not a multiple of lot_size %s", q, inst.LotSize)
		}
	}
	if req.QuoteQuantity != nil && inst.MinNotional.IsPositive() &&
		req.QuoteQuantity.LessThan(inst.MinNotional) {
		return codeErr("MIN_NOTIONAL_VIOLATION",
			"quote_quantity %s below min_notional %s", *req.QuoteQuantity, inst.MinNotional)
	}
	if req.Price != nil {
		p := *req.Price
		if inst.MinPrice != nil && p.LessThan(*inst.MinPrice) {
			return codeErr("PRICE_OUT_OF_BAND",
				"price %s below min_price %s", p, *inst.MinPrice)
		}
		if inst.MaxPrice != nil && p.GreaterThan(*inst.MaxPrice) {
			return codeErr("PRICE_OUT_OF_BAND",
				"price %s above max_price %s", p, *inst.MaxPrice)
		}
		if inst.TickSize.IsPositive() && !p.Mod(inst.TickSize).IsZero() {
			return codeErr("INVALID_REQUEST",
				"price %s is not a multiple of tick_size %s", p, inst.TickSize)
		}
	}
	// PRICE_BAND: limit/stop prices within ±pct of the reference price.
	if refPrice != nil && refPrice.IsPositive() {
		check := func(p *decimal.Decimal) error {
			if p == nil {
				return nil
			}
			up := refPrice.Mul(decimal.One.Add(inst.PriceBandPctUp.Div(decimal.NewFromInt(100))))
			down := refPrice.Mul(decimal.One.Sub(inst.PriceBandPctDown.Div(decimal.NewFromInt(100))))
			if p.GreaterThan(up) || p.LessThan(down) {
				return codeErr("PRICE_OUT_OF_BAND",
					"price %s outside band [%s, %s] around reference %s",
					*p, down, up, *refPrice)
			}
			return nil
		}
		if err := check(req.Price); err != nil {
			return err
		}
		if err := check(req.StopPrice); err != nil {
			return err
		}
	}
	// MIN_NOTIONAL on base quantity × evaluation price.
	if req.Quantity != nil && inst.MinNotional.IsPositive() {
		eval := req.Price
		if eval == nil {
			eval = refPrice
		}
		if eval != nil && eval.IsPositive() {
			if req.Quantity.Mul(*eval).LessThan(inst.MinNotional) {
				return codeErr("MIN_NOTIONAL_VIOLATION",
					"notional %s below min_notional %s",
					req.Quantity.Mul(*eval), inst.MinNotional)
			}
		}
	}
	return nil
}

// --- amend validation --------------------------------------------------------

// StaleModify rejects a modify whose order_seq does not match the
// order's current ingress sequence (spec §5.3.22/§6.9 #1). The task text
// rejects `order_seq < current`; we reject any mismatch — a greater seq
// can only mean the client saw a mutation the read model hasn't applied,
// i.e. equally stale (fail-closed, strictly stricter, never looser).
func StaleModify(expected *uint64, current uint64) error {
	if expected == nil {
		return codeErr("INVALID_REQUEST", "order_seq is required")
	}
	if *expected != current {
		return codeErr("STALE_MODIFY",
			"order_seq %d does not match current sequence %d",
			*expected, current)
	}
	return nil
}

// ValidateModify checks an amend against the current order state.
// wire-0-means-unchanged: absent fields pass 0 to the engine.
func ValidateModify(req *ModifyRequest, o *Order, inst *Instrument,
	now time.Time) error {
	if o == nil {
		return codeErr("ORDER_NOT_FOUND", "order not found")
	}
	if err := amendStateGate(inst); err != nil {
		return err
	}
	if isTerminal(o.Status) {
		return codeErr("ORDER_NOT_FOUND",
			"order %d is in terminal state %s", o.ID, o.Status)
	}
	// §6.9 #2: IOC/FOK amends rejected outright — such orders never rest.
	if o.TimeInForce == TIFIOC || o.TimeInForce == TIFFOK {
		return codeErr("ORDER_AMEND_REJECTED",
			"IOC/FOK orders cannot be amended")
	}
	if req.OrderSeq == nil {
		return codeErr("INVALID_REQUEST", "order_seq is required")
	}
	if req.Price == nil && req.Quantity == nil && req.StopPrice == nil &&
		req.DisplayQty == nil && req.TimeInForce == "" && req.GTDExpiry == nil &&
		req.TriggerSource == "" {
		return codeErr("INVALID_REQUEST", "no mutable fields supplied")
	}
	if req.Quantity != nil {
		if !req.Quantity.IsPositive() {
			return codeErr("INVALID_REQUEST", "quantity must be positive")
		}
		// An amend must leave remaining quantity above what is filled.
		if !req.Quantity.GreaterThan(o.FilledQty) {
			return codeErr("ORDER_AMEND_REJECTED",
				"new quantity %s must exceed filled_qty %s",
				*req.Quantity, o.FilledQty)
		}
		if inst.LotSize.IsPositive() && !req.Quantity.Mod(inst.LotSize).IsZero() {
			return codeErr("INVALID_REQUEST",
				"quantity %s is not a multiple of lot_size %s",
				*req.Quantity, inst.LotSize)
		}
		if req.Quantity.GreaterThan(inst.MaxOrderQty) {
			return codeErr("INVALID_REQUEST",
				"quantity %s exceeds max_order_qty %s",
				*req.Quantity, inst.MaxOrderQty)
		}
	}
	if req.Price != nil {
		if !req.Price.IsPositive() {
			return codeErr("INVALID_REQUEST", "price must be positive")
		}
		if o.OrderType == TypeMarket {
			return codeErr("ORDER_AMEND_REJECTED",
				"MARKET orders carry no amendable price")
		}
		if inst.TickSize.IsPositive() && !req.Price.Mod(inst.TickSize).IsZero() {
			return codeErr("INVALID_REQUEST",
				"price %s is not a multiple of tick_size %s",
				*req.Price, inst.TickSize)
		}
		if inst.MinPrice != nil && req.Price.LessThan(*inst.MinPrice) {
			return codeErr("PRICE_OUT_OF_BAND",
				"price %s below min_price %s", *req.Price, *inst.MinPrice)
		}
		if inst.MaxPrice != nil && req.Price.GreaterThan(*inst.MaxPrice) {
			return codeErr("PRICE_OUT_OF_BAND",
				"price %s above max_price %s", *req.Price, *inst.MaxPrice)
		}
	}
	if req.DisplayQty != nil {
		if o.OrderType != TypeIceberg {
			return codeErr("ORDER_AMEND_REJECTED",
				"iceberg_visible_qty only applies to ICEBERG orders")
		}
		if !req.DisplayQty.IsPositive() {
			return codeErr("INVALID_REQUEST", "iceberg_visible_qty must be positive")
		}
	}
	if req.TimeInForce != "" {
		if !validTIF(req.TimeInForce) {
			return codeErr("INVALID_REQUEST", "invalid time_in_force %q", req.TimeInForce)
		}
		// §6.9 #2: amending TIF to IOC/FOK is meaningless (never rests).
		if req.TimeInForce == TIFIOC || req.TimeInForce == TIFFOK {
			return codeErr("ORDER_AMEND_REJECTED",
				"cannot amend to IOC/FOK")
		}
	}
	// Spec §27 R8 — the 90-day ceiling applies to amends too: a TIF
	// switch to GTC resolves to GTD at the cap (an earlier stored or
	// amended expiry wins), and any amended expiry past the cap clamps.
	if req.TimeInForce == TIFGTC || req.TimeInForce == TIFGTD {
		cap90 := now.AddDate(0, 0, gtcMaxLifetimeDays)
		if req.TimeInForce == TIFGTC {
			req.TimeInForce = TIFGTD
			if req.GTDExpiry == nil {
				req.GTDExpiry = o.GTDExpiry // preserve stored expiry if sooner
			}
		}
		if req.GTDExpiry == nil || req.GTDExpiry.After(cap90) {
			req.GTDExpiry = &cap90
		}
	} else if req.GTDExpiry != nil {
		if cap90 := now.AddDate(0, 0, gtcMaxLifetimeDays); req.GTDExpiry.After(cap90) {
			req.GTDExpiry = &cap90
		}
	}
	// Task 16.3.17 — a trigger-source switch on a live order is rejected.
	// The field rides the wire so the intent is rejected deterministically
	// rather than silently dropped; the enum check itself stays here so a
	// bogus value fails fast at the gateway.
	if req.TriggerSource != "" && !validTriggerSource(req.TriggerSource) {
		return codeErr("INVALID_REQUEST",
			"trigger_source must be LAST_PRICE, MARK_PRICE or INDEX_PRICE")
	}
	return nil
}

// ValidateKeepPriority enforces the §6.9 quantity-down-only rule: any
// other field, a missing/invalid qty, or a non-decrease →
// ORDER_AMEND_REJECTED (HTTP 409).
func ValidateKeepPriority(req *KeepPriorityRequest, o *Order, inst *Instrument) error {
	if o == nil {
		return codeErr("ORDER_NOT_FOUND", "order not found")
	}
	if err := amendStateGate(inst); err != nil {
		return err
	}
	if isTerminal(o.Status) {
		return codeErr("ORDER_NOT_FOUND",
			"order %d is in terminal state %s", o.ID, o.Status)
	}
	if req.Quantity == nil || !req.Quantity.IsPositive() {
		return codeErr("ORDER_AMEND_REJECTED",
			"keep-priority amend requires a positive quantity")
	}
	if !req.Quantity.LessThan(o.Quantity) {
		return codeErr("ORDER_AMEND_REJECTED",
			"keep-priority amend must decrease quantity (%s >= %s)",
			*req.Quantity, o.Quantity)
	}
	if !req.Quantity.GreaterThan(o.FilledQty) {
		return codeErr("ORDER_AMEND_REJECTED",
			"quantity %s must exceed filled_qty %s",
			*req.Quantity, o.FilledQty)
	}
	if inst.LotSize.IsPositive() && !req.Quantity.Mod(inst.LotSize).IsZero() {
		return codeErr("INVALID_REQUEST",
			"quantity %s is not a multiple of lot_size %s",
			*req.Quantity, inst.LotSize)
	}
	return nil
}

// --- mass-cancel scope validation -------------------------------------------

// NormalizeMassCancelScope validates + normalizes side/order_type
// filters ("ALL" → ""), resolving the spec §5.3.24 dimensions.
func NormalizeMassCancelScope(s *MassCancelScope, admin bool) error {
	switch s.Side {
	case "", "ALL":
		s.Side = ""
	case SideBuy, SideSell:
	default:
		return codeErr("INVALID_REQUEST", "side must be BUY, SELL or ALL")
	}
	switch s.OrderType {
	case "", "ALL":
		s.OrderType = ""
	case TypeLimit, TypeMarket, TypeStop, TypeStopLimit, TypeIceberg,
		// Queued Phase-16 types ride the same scoped-cancel contract —
		// the §6.2b remainder sweep targets MOO/MOC by type, and FIXING
		// orders cancel through the reservation-release path.
		TypePeg, TypeFixing, TypeMOO, TypeMOC:
	default:
		return codeErr("INVALID_REQUEST",
			"order_type must be one of LIMIT|MARKET|STOP|STOP_LIMIT|ICEBERG|PEG|FIXING|MOO|MOC|ALL")
	}
	if !admin && s.AccountID == 0 {
		return codeErr("UNAUTHORIZED", "account context required")
	}
	return nil
}
