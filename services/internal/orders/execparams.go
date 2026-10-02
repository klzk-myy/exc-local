package orders

// Phase-16 Task 16.3.10 — execution-parameter persistence + validation
// (migration 038 columns, spec §6.2/§6.5). This file owns all Phase-16
// submit-path validation so validate.go changes stay a one-line hook;
// sibling Phase-16 algo engines re-use ValidateAlgoParams for their own
// submit surfaces.

import (
	"encoding/json"
	"strings"
	"time"

	"exchange/pkg/decimal"
)

// ---------------------------------------------------------------------------
// Canonical vocabularies
// ---------------------------------------------------------------------------

// Fixing benchmarks — the canonical spec §6.4 order-level vocabulary
// (remediation #35: supersedes the plan strings WM_REFINITIV_4PM_LDN /
// ECB_1415_CET; the auction_calendar scheduler values WM_LONDON_4PM /
// ECB_REF_1415 / TOKYO_0955 map onto these inside internal/algo).
const (
	FixingBenchmarkWMR4PM    = "WM_R_4PM"
	FixingBenchmarkECB1415   = "ECB_1415"
	FixingBenchmarkTokyo0955 = "TOKYO_0955"
)

func validFixingBenchmark(v string) bool {
	switch v {
	case FixingBenchmarkWMR4PM, FixingBenchmarkECB1415, FixingBenchmarkTokyo0955:
		return true
	}
	return false
}

// Trigger sources — migration 066, Phase-16 Task 16.3.17 (spec §6.2
// dual-price trigger). MARK_PRICE / INDEX_PRICE ride the Phase-19.5
// oracle; the engine suspends triggers on stale feeds
// (CONDITIONAL_TRIGGER_ORACLE_STALE) — admission only validates the enum.
const (
	TriggerSourceLast  = "LAST_PRICE"
	TriggerSourceMark  = "MARK_PRICE"
	TriggerSourceIndex = "INDEX_PRICE"
)

func validTriggerSource(v string) bool {
	switch v {
	case TriggerSourceLast, TriggerSourceMark, TriggerSourceIndex:
		return true
	}
	return false
}

// Trailing-stop distance units — Task 16.3.15 (spec §5.4). The wire
// ordinals are the engine's kTrailUnit* values verbatim (Order.hpp).
const (
	TrailUnitPips       = "PIPS"       // whole pips
	TrailUnitPercentage = "PERCENTAGE" // pct of the trail anchor (wire: pct*100)
	TrailUnitAbsolute   = "ABSOLUTE"   // quote-currency distance (wire: ticks)
)

func validTrailUnit(v string) bool {
	switch v {
	case TrailUnitPips, TrailUnitPercentage, TrailUnitAbsolute:
		return true
	}
	return false
}

// Peg modes — spec §5.4 peg_mode values (Phase-16 Task 16.3.11).
const (
	PegModeMid     = "MID"
	PegModePrimary = "PRIMARY"
	PegModeMarket  = "MARKET"
)

func validPegMode(v string) bool {
	switch v {
	case PegModeMid, PegModePrimary, PegModeMarket:
		return true
	}
	return false
}

// algoParamsMaxBytes is the gateway-side algo_params payload cap — a
// parameter blob is strategy configuration, never a data channel.
const algoParamsMaxBytes = 8 * 1024

// Algo type names that carry schema-validated params. VP/GRID ride the
// same surface (plan-level strategy names, no new order_type values).
const (
	AlgoTWAP         = "TWAP"
	AlgoVWAP         = "VWAP"
	AlgoVP           = "VP"
	AlgoTrailingStop = "TRAILING_STOP"
	AlgoScaled       = "SCALED"
	AlgoGrid         = "GRID"
	AlgoFixing       = "FIXING"
)

// ---------------------------------------------------------------------------
// Submit-path validation — called once from ValidateSubmit
// ---------------------------------------------------------------------------

// validateExecParams enforces the Phase-16 combination rules over the
// normalized request. Everything here is additive: requests without any
// Phase-16 field sail through untouched.
func validateExecParams(req *SubmitRequest) error {
	// TRAILING_STOP fold (Task 16.3.15 wire-up, IMP-PLAN Phase-3 Task 2):
	// lift the algo_params parameter set into the first-class fields
	// BEFORE the field validations below so a folded trigger_source /
	// trailing_offset run through the same enum and positivity gates.
	if err := foldTrailingParams(req); err != nil {
		return err
	}
	// §6.5 — a post-only order that would execute immediately is
	// rejected; a MARKET order is marketable by definition.
	if req.PostOnly && req.OrderType == TypeMarket {
		return codeErr("POST_ONLY_VIOLATION",
			"post_only cannot be combined with a MARKET order")
	}
	// Iceberg: the visible slice must be strictly below the total
	// quantity — an iceberg displaying everything is a limit order.
	if req.DisplayQty != nil && req.Quantity != nil &&
		!req.DisplayQty.LessThan(*req.Quantity) {
		return codeErr("INVALID_REQUEST",
			"iceberg_visible_qty must be strictly less than quantity")
	}
	// Trigger source enum (migration 066). MARK/INDEX admission is a
	// pure syntax gate — freshness is enforced at trigger time by the
	// engine's oracle seam (CONDITIONAL_TRIGGER_ORACLE_STALE); the
	// gateway never fabricates an oracle state.
	if req.TriggerSource != "" && !validTriggerSource(req.TriggerSource) {
		return codeErr("INVALID_REQUEST",
			"trigger_source must be LAST_PRICE, MARK_PRICE or INDEX_PRICE")
	}
	// Peg surface (Task 16.3.11 — engine-owned repricing; Go validates
	// + persists the triplet and maps it onto the OrderNew wire).
	if req.PegMode != "" && !validPegMode(req.PegMode) {
		return codeErr("INVALID_REQUEST",
			"peg_mode must be MID, PRIMARY or MARKET")
	}
	if (req.PegOffset != nil || req.PegLimit != nil) &&
		req.PegMode == "" && req.OrderType != TypePeg {
		return codeErr("INVALID_REQUEST",
			"peg_offset/peg_limit require peg_mode (or type PEG)")
	}
	if req.OrderType == TypePeg && req.PegMode == "" {
		return codeErr("INVALID_REQUEST",
			"PEG orders require peg_mode (MID, PRIMARY or MARKET)")
	}
	// Hidden liquidity (Task 16.3.13 — engine-owned display rules):
	// a hidden flag only makes sense on a resting limit order.
	if req.Hidden && req.OrderType != TypeLimit {
		return codeErr("INVALID_REQUEST",
			"hidden is only valid on LIMIT orders")
	}
	// GSLO (Task 16.3.16): a guaranteed stop is a flag on a stop-loss
	// order — without a stop price there is nothing to guarantee.
	if req.GSLO {
		if req.OrderType != TypeStop && req.OrderType != TypeStopLimit {
			return codeErr("INVALID_REQUEST",
				"gslo requires a STOP or STOP_LIMIT order")
		}
		if req.StopPrice == nil || !req.StopPrice.IsPositive() {
			return codeErr("INVALID_REQUEST",
				"gslo requires a positive stop_price")
		}
	}
	// FIXING (Task 16.3.9): benchmark enum + quantity only — price,
	// stop, iceberg and post-only semantics do not apply to an order
	// that executes at the published fix.
	if req.OrderType == TypeFixing {
		if !validFixingBenchmark(req.FixingBenchmark) {
			return codeErr("INVALID_REQUEST",
				"fixing_benchmark must be WM_R_4PM, ECB_1415 or TOKYO_0955")
		}
		if req.Price != nil || req.StopPrice != nil || req.DisplayQty != nil {
			return codeErr("INVALID_REQUEST",
				"FIXING orders carry no price/stop/display fields")
		}
		if req.PostOnly {
			return codeErr("POST_ONLY_VIOLATION",
				"post_only is meaningless on a FIXING order")
		}
		if req.Hidden || req.GSLO {
			return codeErr("INVALID_REQUEST",
				"hidden/gslo flags do not apply to FIXING orders")
		}
		if req.Quantity == nil || !req.Quantity.IsPositive() {
			return codeErr("INVALID_REQUEST",
				"FIXING orders require a positive quantity")
		}
		if req.QuoteQuantity != nil {
			return codeErr("INVALID_REQUEST",
				"FIXING orders are quantity-denominated only")
		}
	} else if req.FixingBenchmark != "" {
		return codeErr("INVALID_REQUEST",
			"fixing_benchmark requires order type FIXING")
	}
	// algo_params byte cap + per-algo_type schema. Params without a
	// discriminator are unenforceable — reject rather than guess.
	if req.AlgoType == "" && len(req.AlgoParams) > 0 {
		return codeErr("INVALID_REQUEST",
			"algo_params requires algo_type")
	}
	if err := ValidateAlgoParams(req.AlgoType, req.AlgoParams); err != nil {
		return err
	}
	return nil
}

// ---------------------------------------------------------------------------
// Trailing stop + discretionary offset (Task 16.3.15 / spec §6.11)
// ---------------------------------------------------------------------------

// foldTrailingParams lifts the algo_params TRAILING_STOP parameter set
// into the first-class request fields and enforces the trailing/
// discretionary legality matrix. The wire encodes a trailing stop as
// StopMarket + trailing_offset_unit != 0 — the engine derives
// OrderType::TRAILING_STOP (EnginePump).
func foldTrailingParams(req *SubmitRequest) error {
	if strings.EqualFold(req.AlgoType, AlgoTrailingStop) {
		req.AlgoType = AlgoTrailingStop
		var obj map[string]json.RawMessage
		if len(req.AlgoParams) > 0 {
			if err := json.Unmarshal(req.AlgoParams, &obj); err != nil {
				return codeErr("INVALID_REQUEST",
					"TRAILING_STOP algo_params must be a JSON object")
			}
		}
		dec := func(keys ...string) (*decimal.Decimal, error) {
			for _, k := range keys {
				raw, ok := obj[k]
				if !ok {
					continue
				}
				var d decimal.Decimal
				if err := json.Unmarshal(raw, &d); err != nil {
					var s string
					if serr := json.Unmarshal(raw, &s); serr == nil {
						if dd, derr := decimal.NewFromString(s); derr == nil {
							return &dd, nil
						}
					}
					return nil, codeErr("INVALID_REQUEST",
						"algo_params.%s must be a number", k)
				}
				return &d, nil
			}
			return nil, nil
		}
		str := func(keys ...string) (string, error) {
			for _, k := range keys {
				raw, ok := obj[k]
				if !ok {
					continue
				}
				var s string
				if err := json.Unmarshal(raw, &s); err != nil {
					return "", codeErr("INVALID_REQUEST",
						"algo_params.%s must be a string", k)
				}
				return strings.ToUpper(strings.TrimSpace(s)), nil
			}
			return "", nil
		}
		var err error
		var d *decimal.Decimal
		if d, err = dec("trailing_offset", "offset"); err != nil {
			return err
		}
		if d != nil && req.TrailingOffset == nil {
			req.TrailingOffset = d
		}
		var s string
		if s, err = str("trailing_unit", "trailing_offset_unit", "offset_unit"); err != nil {
			return err
		}
		if s != "" && req.TrailingOffsetUnit == "" {
			req.TrailingOffsetUnit = s
		}
		if d, err = dec("activation_price"); err != nil {
			return err
		}
		if d != nil && req.ActivationPrice == nil {
			req.ActivationPrice = d
		}
		if s, err = str("trigger_source"); err != nil {
			return err
		}
		if s != "" && req.TriggerSource == "" {
			req.TriggerSource = s
		}
		// A trailing stop is a stop order — type STOP is required (the
		// wire needs StopMarket for the trail-unit overlay to decode;
		// callers reaching ValidateSubmit already carry a non-empty type).
		if req.OrderType != TypeStop {
			return codeErr("INVALID_REQUEST",
				"TRAILING_STOP params require order type STOP")
		}
	}
	if req.TrailingOffset == nil && req.TrailingOffsetUnit == "" {
		if req.ActivationPrice != nil {
			return codeErr("INVALID_REQUEST",
				"activation_price requires trailing_offset")
		}
	} else {
		if req.OrderType != TypeStop {
			return codeErr("INVALID_REQUEST",
				"trailing_offset/trailing_offset_unit require order type STOP")
		}
		if req.TrailingOffset == nil || req.TrailingOffsetUnit == "" {
			return codeErr("INVALID_REQUEST",
				"trailing_offset and trailing_offset_unit must be supplied together")
		}
		if !validTrailUnit(req.TrailingOffsetUnit) {
			return codeErr("INVALID_REQUEST",
				"trailing_offset_unit must be PIPS, PERCENTAGE or ABSOLUTE")
		}
		if !req.TrailingOffset.IsPositive() {
			return codeErr("INVALID_REQUEST",
				"trailing_offset must be positive")
		}
		// Wire fidelity: PIPS carries whole pips and PERCENTAGE carries
		// pct*100 — a finer-grained distance would truncate silently on
		// the wire, so the gateway refuses it instead (§2.7 fail-closed).
		switch req.TrailingOffsetUnit {
		case TrailUnitPips:
			if !req.TrailingOffset.IsInteger() {
				return codeErr("INVALID_REQUEST",
					"trailing_offset in PIPS must be a whole number")
			}
		case TrailUnitPercentage:
			if !req.TrailingOffset.Shift(2).IsInteger() {
				return codeErr("INVALID_REQUEST",
					"trailing_offset in PERCENTAGE supports 2 decimal places (basis points)")
			}
		}
		if req.ActivationPrice != nil && !req.ActivationPrice.IsPositive() {
			return codeErr("INVALID_REQUEST",
				"activation_price must be positive")
		}
		// Self-describing row (spec §5.4 algo_params persistence
		// contract): a first-class-fields submit synthesizes the
		// algo_params document so the read model carries the trail config.
		if req.AlgoType == "" {
			req.AlgoType = AlgoTrailingStop
			doc := map[string]any{
				"trailing_offset": req.TrailingOffset.String(),
				"trailing_unit":   req.TrailingOffsetUnit,
			}
			if req.ActivationPrice != nil {
				doc["activation_price"] = req.ActivationPrice.String()
			}
			if req.TriggerSource != "" {
				doc["trigger_source"] = req.TriggerSource
			}
			enc, err := json.Marshal(doc)
			if err != nil {
				return errInternal("algo_params encode", err)
			}
			req.AlgoParams = enc
		}
	}
	// §6.11 discretionary offset — a hidden price-improvement band on a
	// resting LIMIT, whole pips only (the engine multiplies the pip size).
	if req.DiscretionaryOffsetPips != nil {
		if req.OrderType != TypeLimit {
			return codeErr("INVALID_REQUEST",
				"discretionary_offset_pips is only valid on LIMIT orders")
		}
		if !req.DiscretionaryOffsetPips.IsPositive() ||
			!req.DiscretionaryOffsetPips.IsInteger() {
			return codeErr("INVALID_REQUEST",
				"discretionary_offset_pips must be a positive whole number of pips")
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// algo_params schema validation (shared with the sibling algo engines)
// ---------------------------------------------------------------------------

// ValidateAlgoParams enforces the size cap and the per-algo_type schema.
// params may be nil (no params — always valid). Unknown algo types are
// allowed through with only the size cap applied — sibling tasks own
// their strategy contracts; the gateway must not hard-fail a strategy
// it doesn't know while siblings land incrementally.
func ValidateAlgoParams(algoType string, params json.RawMessage) error {
	if len(params) > algoParamsMaxBytes {
		return codeErr("INVALID_REQUEST",
			"algo_params exceeds %d bytes", algoParamsMaxBytes)
	}
	if len(params) == 0 {
		return nil
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(params, &obj); err != nil {
		return codeErr("INVALID_REQUEST",
			"algo_params must be a JSON object")
	}
	num := func(key string) (*decimal.Decimal, bool, error) {
		raw, ok := obj[key]
		if !ok {
			return nil, false, nil
		}
		var d decimal.Decimal
		if err := json.Unmarshal(raw, &d); err != nil {
			return nil, true, codeErr("INVALID_REQUEST",
				"algo_params.%s must be a number", key)
		}
		return &d, true, nil
	}
	str := func(key string) (string, bool, error) {
		raw, ok := obj[key]
		if !ok {
			return "", false, nil
		}
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return "", true, codeErr("INVALID_REQUEST",
				"algo_params.%s must be a string", key)
		}
		return s, true, nil
	}
	switch strings.ToUpper(algoType) {
	case AlgoTWAP:
		// Task 16.3.2: child-slice interval must sit between 1s and 1h.
		iv, present, err := num("interval_seconds")
		if err != nil {
			return err
		}
		if !present {
			return codeErr("INVALID_REQUEST",
				"TWAP algo_params.interval_seconds is required")
		}
		if iv.LessThan(decimal.NewFromInt(1)) ||
			iv.GreaterThan(decimal.NewFromInt(int64(time.Hour/time.Second))) {
			return codeErr("INVALID_REQUEST",
				"TWAP interval_seconds must be between 1s and 3600s (1 hour)")
		}
	case AlgoTrailingStop:
		// Task 16.3.15: a trailing offset of zero trails at the touch —
		// it is a market order in disguise; reject it. `trailing_offset`
		// is the first-class field's spelling inside algo_params; `offset`
		// is the original short alias — both fold into
		// SubmitRequest.TrailingOffset.
		off, present, err := num("trailing_offset")
		if err != nil {
			return err
		}
		if !present {
			off, present, err = num("offset")
			if err != nil {
				return err
			}
		}
		if !present || !off.IsPositive() {
			return codeErr("INVALID_REQUEST",
				"TRAILING_STOP algo_params.trailing_offset must be positive")
		}
	case AlgoScaled:
		// Scaled ladder cap: 20 levels (Task 16.3.17-family sizing rule).
		raw, ok := obj["levels"]
		if !ok {
			return codeErr("INVALID_REQUEST",
				"SCALED algo_params.levels is required")
		}
		var levels []map[string]json.RawMessage
		if err := json.Unmarshal(raw, &levels); err != nil {
			return codeErr("INVALID_REQUEST",
				"SCALED algo_params.levels must be an array")
		}
		if len(levels) == 0 || len(levels) > 20 {
			return codeErr("INVALID_REQUEST",
				"SCALED ladders support between 1 and 20 levels")
		}
		for i, lvl := range levels {
			if _, ok := lvl["price"]; !ok {
				return codeErr("INVALID_REQUEST",
					"SCALED level %d missing price", i)
			}
			if _, ok := lvl["quantity"]; !ok {
				return codeErr("INVALID_REQUEST",
					"SCALED level %d missing quantity", i)
			}
		}
	case AlgoFixing:
		// Benchmark may arrive inside algo_params on the strategy
		// surface — validate it against the same canonical enum.
		bm, present, err := str("benchmark")
		if err != nil {
			return err
		}
		if present && !validFixingBenchmark(bm) {
			return codeErr("INVALID_REQUEST",
				"algo_params.benchmark must be WM_R_4PM, ECB_1415 or TOKYO_0955")
		}
	}
	return nil
}
