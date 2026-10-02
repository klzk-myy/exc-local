// Package orders implements the Phase-05 order pipeline: REST order
// endpoints (Task 5.3.3), modify audit + STALE_MODIFY (Task 5.3.22),
// idempotent submission (Task 5.3.24), scoped mass cancel (Task 5.3.25),
// REST batch ops (Task 5.3.32), atomic cancel-replace / keep-priority
// amend (Task 5.3.37) and quote-denominated market orders + dry-run
// preview (Task 5.3.39).
//
// The package consumes — never rewrites — the shared foundations:
// errs.Default registry codes, auth.Claims context, FlatBuffers wire
// schema (core/proto/exchange.fbs), the shm/Aeron IPC channel
// (internal/ipc), fixed-point decimal (pkg/decimal), and the durable
// PostgreSQL tables from migrations 153–155.
package orders

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"exchange/pkg/decimal"
)

// Canonical enum vocabularies (spec §5.4 / wire schema).
const (
	SideBuy  = "BUY"
	SideSell = "SELL"
)

const (
	TypeLimit     = "LIMIT"
	TypeMarket    = "MARKET"
	TypeStop      = "STOP"
	TypeStopLimit = "STOP_LIMIT"
	TypeIceberg   = "ICEBERG"
	// TypePeg is an engine-side pegged order (Phase-16 Task 16.3.11 —
	// the engine's PeggedOrderTracker re-prices it off BBO). The Go
	// gateway validates + persists peg_mode/peg_offset/peg_limit and
	// maps them onto the OrderNew wire aux fields.
	TypePeg = "PEG"
	// TypeFixing is a benchmark-fixing order (Phase-16 Task 16.3.9,
	// spec §6.4). It is NOT dispatched to the engine: orders.Service
	// queues it status=RESERVED and the Phase-15 fixing executor
	// crosses queued orders at the published fix rate.
	TypeFixing = "FIXING"
	// TypeMOO / TypeMOC are session-bound market orders (Phase-16 Task
	// 16.3.25, spec §6.2b): queued status=RESERVED during the 15-minute
	// pre-open/pre-close accumulation window and injected into the
	// call-auction uncross at the single max-volume price — never
	// continuously matched. Amends/cancels freeze T-30s before the
	// trigger; the unfilled remainder is cancelled AUCTION_CANCELLED.
	TypeMOO = "MOO"
	TypeMOC = "MOC"
)

// IsAuctionType reports whether the order type is a queued session-
// auction order (§6.2b) — RESERVED rows never reached the engine and
// therefore cancel locally rather than via the wire-confirm path.
func IsAuctionType(t string) bool { return t == TypeMOO || t == TypeMOC }

// Contingency list types (Phase-16 Task 16.3.20, spec §5.39
// migration-075 `contingency_type`).
const (
	ContingencyOPO   = "OPO"   // one pending leg activated on working fill
	ContingencyOPOCO = "OPOCO" // pending legs are an OCO pair
)

// Order-list lifecycle states (migration 075 order_list_state_enum).
const (
	ListStateExecuting = "EXECUTING" // working leg in flight
	ListStateAllDone   = "ALL_DONE"  // working filled; pending leg(s) placed
	ListStateCancelled = "CANCELLED"
	ListStateFailed    = "FAILED"  // pending validation/placement failed
	ListStateExpired   = "EXPIRED" // working leg expired unexecuted
)

// Bracket lifecycle states (migration 225 bracket_state_enum).
const (
	BracketWorking   = "WORKING"   // parent live; fills spawn children
	BracketFilled    = "FILLED"    // parent fully filled, children placed
	BracketCancelled = "CANCELLED" // parent cancelled; children swept
	BracketFailed    = "FAILED"    // parent rejected / child placement failed
)

const (
	TIFGTC = "GTC"
	TIFIOC = "IOC"
	TIFFOK = "FOK"
	TIFGTD = "GTD"
	TIFDAY = "DAY"
)

// OpenStatuses is the order_status set that rests on the book /
// in-flight and is therefore cancellable.
var OpenStatuses = []string{"PENDING", "RESERVED", "ACTIVE", "PARTIALLY_FILLED"}

// Terminal order statuses — once reached an order never transitions again.
func isTerminal(status string) bool {
	switch status {
	case "FILLED", "CANCELLED", "REJECTED", "EXPIRED":
		return true
	}
	return false
}

// ---------------------------------------------------------------------------
// Reference data snapshots
// ---------------------------------------------------------------------------

// Instrument is the per-symbol validation/filter snapshot the order
// pipeline needs (spec §5.1 + migration 170 filter columns).
type Instrument struct {
	ID               int64
	Symbol           string
	BaseCurrency     string
	QuoteCurrency    string
	InstrumentType   string // SPOT | FORWARD | SWAP | NDF | OPTION — the §24 #152 instrument-class axis
	Status           string
	TickSize         decimal.Decimal
	LotSize          decimal.Decimal
	MinOrderQty      decimal.Decimal
	MaxOrderQty      decimal.Decimal
	MinNotional      decimal.Decimal
	PriceBandPctUp   decimal.Decimal
	PriceBandPctDown decimal.Decimal
	MaxLeverage      int
	MinPrice         *decimal.Decimal
	MaxPrice         *decimal.Decimal
	MaxSpreadPips    *decimal.Decimal
	MaxOpenOrders    *int
	// SettlementMode is instruments.settlement_mode::text (GROSS | NET —
	// migration 031). PopulateSnapshot does not select it; the service
	// resolves it lazily through the instrumentLinkageReader seam
	// (service.go) only for derivative-class instruments — the Phase-22
	// Task 22.3.9 instrument-linkage check.
	SettlementMode string
}

// Account is the order-flow view of an accounts row.
type Account struct {
	ID                 int64
	UserID             int64
	Type               string // SPOT | MARGIN | PORTFOLIO
	KycTier            string // T0 | T1 | T2
	ClientCategory     string // RETAIL | PROFESSIONAL | ELIGIBLE_COUNTERPARTY (migration 042)
	NBP                bool   // §13.6c retail NBP entitlement flag (migration 042)
	Status             string // ACTIVE | SUSPENDED | FROZEN | CLOSED
	TradeGroupID       *int64
	DefaultSTPMode     string
	CancelOnDisconnect bool
}

// Order is one orders row plus the pipeline columns of migration 155.
type Order struct {
	ID            int64
	AccountID     int64
	InstrumentID  int64
	ClientOrderID string
	Side          string
	OrderType     string
	Quantity      decimal.Decimal
	QuoteQuantity *decimal.Decimal
	Price         *decimal.Decimal
	StopPrice     *decimal.Decimal
	DisplayQty    *decimal.Decimal
	TimeInForce   string
	Status        string
	FilledQty     decimal.Decimal
	AvgFillPrice  *decimal.Decimal
	ShardID       *int
	BookSeq       *int64
	OrderSeq      uint64
	PostOnly      bool
	ReduceOnly    bool
	STPMode       string
	SessionID     string
	// OcoGroupID links both legs of an OCO pair (Phase-14 Task 14.3.1,
	// migration 218) — nil = standalone order.
	OcoGroupID *int64
	// ---- Phase-16 Task 16.3.10 execution-parameter columns (migration
	// 038) + Task 16.3.17 trigger source (migration 066) ----
	PegMode         *string
	PegOffset       *decimal.Decimal
	PegLimit        *decimal.Decimal
	TriggerSource   string // LAST_PRICE | MARK_PRICE | INDEX_PRICE
	Hidden          bool
	GSLO            bool
	FixingBenchmark *string
	AlgoType        *string
	AlgoParams      json.RawMessage // NULL-able JSONB
	// DiscretionaryOffsetPips — spec §6.11 hidden price-improvement band
	// (migration 103). Nil when zero/absent so the view omits it.
	DiscretionaryOffsetPips *decimal.Decimal
	// CoDExempt exempts the order from the session-scope
	// cancel-on-disconnect sweep (spec §5.4 catalog col, §9.9,
	// migration 229; FIX tag 9510). Dead-man/admin/close-all sweeps are
	// unaffected — only Reason "cancel_on_disconnect" honours it.
	CoDExempt bool
	// GTDExpiry — effective resting deadline (migration 284): the
	// client-supplied GTD date, or the spec §27 R8 90-day ceiling stamped
	// when a GTC submission auto-converts. Nil for IOC/FOK.
	GTDExpiry *time.Time
	// ---- Phase-22 Task 22.3.9 derivative order parameters (migration
	// 039) — populated by DerivativeParams persistence, not the base
	// orderCols/scanOrder path (store.go owns those; see service.go) ----
	Strike           *decimal.Decimal
	OptionType       *string // CALL | PUT | BINARY
	ExerciseStyle    *string // EUROPEAN | AMERICAN
	ExpiryAt         *time.Time
	BarrierType      *string // UP_AND_IN | UP_AND_OUT | DOWN_AND_IN | DOWN_AND_OUT
	BarrierLevel     *decimal.Decimal
	ValueDate        *time.Time // civil date (UTC midnight)
	NearLegValueDate *time.Time
	FarLegValueDate  *time.Time
	Premium          *decimal.Decimal
	PremiumCurrency  *string // QUOTE | SETTLEMENT
	NdfFixingSource  *string
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

// View serializes an Order for REST responses — all decimals rendered as
// strings per the §5.3 wire contract.
func (o *Order) View() map[string]any {
	v := map[string]any{
		"order_id":        fmt.Sprint(o.ID),
		"account_id":      o.AccountID,
		"instrument_id":   o.InstrumentID,
		"client_order_id": o.ClientOrderID,
		"side":            o.Side,
		"type":            o.OrderType,
		"quantity":        o.Quantity.String(),
		"time_in_force":   o.TimeInForce,
		"status":          o.Status,
		"filled_qty":      o.FilledQty.String(),
		"order_seq":       o.OrderSeq,
		"post_only":       o.PostOnly,
		"reduce_only":     o.ReduceOnly,
		"created_at":      o.CreatedAt.UTC().Format(time.RFC3339Nano),
		"updated_at":      o.UpdatedAt.UTC().Format(time.RFC3339Nano),
	}
	if o.Price != nil {
		v["price"] = o.Price.String()
	}
	if o.StopPrice != nil {
		v["stop_price"] = o.StopPrice.String()
	}
	if o.QuoteQuantity != nil {
		v["quote_quantity"] = o.QuoteQuantity.String()
	}
	if o.DisplayQty != nil {
		v["iceberg_visible_qty"] = o.DisplayQty.String()
	}
	if o.AvgFillPrice != nil {
		v["avg_fill_price"] = o.AvgFillPrice.String()
	}
	if o.STPMode != "" {
		v["stp_mode"] = o.STPMode
	}
	if o.OcoGroupID != nil {
		v["oco_group_id"] = *o.OcoGroupID
	}
	if o.PegMode != nil {
		v["peg_mode"] = *o.PegMode
	}
	if o.PegOffset != nil {
		v["peg_offset"] = o.PegOffset.String()
	}
	if o.PegLimit != nil {
		v["peg_limit"] = o.PegLimit.String()
	}
	if o.TriggerSource != "" {
		v["trigger_source"] = o.TriggerSource
	}
	if o.Hidden {
		v["hidden"] = true
	}
	if o.GSLO {
		v["gslo"] = true
	}
	if o.FixingBenchmark != nil {
		v["fixing_benchmark"] = *o.FixingBenchmark
	}
	if o.AlgoType != nil {
		v["algo_type"] = *o.AlgoType
	}
	if len(o.AlgoParams) > 0 {
		v["algo_params"] = json.RawMessage(o.AlgoParams)
	}
	if o.DiscretionaryOffsetPips != nil {
		v["discretionary_offset_pips"] = o.DiscretionaryOffsetPips.String()
	}
	// Phase-22 Task 22.3.9 derivative parameters (migration 039).
	if o.Strike != nil {
		v["strike"] = o.Strike.String()
	}
	if o.OptionType != nil {
		v["option_type"] = *o.OptionType
	}
	if o.ExerciseStyle != nil {
		v["exercise_style"] = *o.ExerciseStyle
	}
	if o.ExpiryAt != nil {
		v["expiry_at"] = o.ExpiryAt.UTC().Format(time.RFC3339Nano)
	}
	if o.BarrierType != nil {
		v["barrier_type"] = *o.BarrierType
	}
	if o.BarrierLevel != nil {
		v["barrier_level"] = o.BarrierLevel.String()
	}
	if o.ValueDate != nil {
		v["value_date"] = o.ValueDate.UTC().Format("2006-01-02")
	}
	if o.NearLegValueDate != nil {
		v["near_leg_value_date"] = o.NearLegValueDate.UTC().Format("2006-01-02")
	}
	if o.FarLegValueDate != nil {
		v["far_leg_value_date"] = o.FarLegValueDate.UTC().Format("2006-01-02")
	}
	if o.Premium != nil {
		v["premium"] = o.Premium.String()
	}
	if o.PremiumCurrency != nil {
		v["premium_currency"] = *o.PremiumCurrency
	}
	if o.NdfFixingSource != nil {
		v["ndf_fixing_source"] = *o.NdfFixingSource
	}
	return v
}

// ---------------------------------------------------------------------------
// Request payloads
// ---------------------------------------------------------------------------

// SubmitRequest is the normalized POST /orders body. Decimal fields use
// presence pointers so "absent" differs from "0" — the §22.1 "exactly one
// of quantity|quote_quantity" rule depends on it.
type SubmitRequest struct {
	Symbol        string
	Side          string
	OrderType     string
	TimeInForce   string
	ClientOrderID string
	Quantity      *decimal.Decimal
	QuoteQuantity *decimal.Decimal
	Price         *decimal.Decimal
	StopPrice     *decimal.Decimal
	DisplayQty    *decimal.Decimal
	GTDExpiry     *time.Time
	PostOnly      bool
	ReduceOnly    bool
	STPMode       string
	SessionID     string // populated from claims/server side, not the body
	// CoDExempt marks the order exempt from cancel-on-disconnect
	// (spec §9.9, migration 229; FIX venue tag 9510).
	CoDExempt bool

	// ---- Phase-16 Task 16.3.10 execution-parameter surface
	// (migration 038 columns; validation in execparams.go) ----
	PegMode       string           // MID | PRIMARY | MARKET
	PegOffset     *decimal.Decimal // signed price-scale offset
	PegLimit      *decimal.Decimal // optional limit collar
	TriggerSource string           // migration 066 — LAST_PRICE | MARK_PRICE | INDEX_PRICE
	Hidden        bool
	GSLO          bool
	// Trailing stop (Task 16.3.15, Phase-3 wire-up): legal only on type
	// STOP — the wire encodes it as StopMarket + trailing_offset_unit!=0
	// and the engine derives TRAILING_STOP. TrailingOffsetUnit is
	// PIPS | PERCENTAGE | ABSOLUTE (required when the offset is present).
	// PIPS = whole pips; PERCENTAGE = percent of the anchor (wire carries
	// pct*100); ABSOLUTE = a quote-currency price distance (wire ticks).
	TrailingOffset     *decimal.Decimal
	TrailingOffsetUnit string
	// ActivationPrice arms the trailing loop only after the trigger
	// source trades through it (wire activation_price, ticks).
	ActivationPrice *decimal.Decimal
	// DiscretionaryOffsetPips (spec §6.11, migration 103): hidden
	// price-improvement band on a resting LIMIT — whole pips only, the
	// engine multiplies by pip_size_ticks.
	DiscretionaryOffsetPips *decimal.Decimal
	// FixingBenchmark carries the canonical spec §6.4 vocabulary —
	// WM_R_4PM | ECB_1415 | TOKYO_0955 (supersedes the plan's
	// WM_REFINITIV_4PM_LDN / ECB_1415_CET strings). The scheduler's
	// auction_calendar vocabulary maps onto these in algo/fixing.go.
	FixingBenchmark string
	AlgoType        string
	AlgoParams      json.RawMessage

	// ---- Phase-22 Task 22.3.9 derivative order parameters (migration
	// 039 columns; canonical spec §5.4 vocabulary; validation in
	// validateDerivativeParams below) ----
	Strike           *decimal.Decimal // OPTION strike
	OptionType       string           // CALL | PUT | BINARY
	ExerciseStyle    string           // EUROPEAN | AMERICAN
	ExpiryAt         *time.Time       // OPTION expiry (RFC3339)
	BarrierType      string           // UP_AND_IN | UP_AND_OUT | DOWN_AND_IN | DOWN_AND_OUT
	BarrierLevel     *decimal.Decimal
	ValueDate        *time.Time // FORWARD value date (civil date, UTC midnight)
	NearLegValueDate *time.Time // SWAP near leg
	FarLegValueDate  *time.Time // SWAP far leg
	Premium          *decimal.Decimal
	PremiumCurrency  string // QUOTE | SETTLEMENT
	// NdfFixingSource is the §15.7 NDF fixing-source token (migration 039
	// ndf_fixing_source) — deliberately distinct from FixingBenchmark, the
	// three-value order-level benchmark enum that migration 038 owns and
	// which applies only to FIXING order types.
	NdfFixingSource string
}

// ModifyRequest is PUT /orders/{id}: order_seq is the STALE_MODIFY fence
// (mandatory per Task 5.3.22); at least one mutable field is required.
type ModifyRequest struct {
	OrderSeq    *uint64
	Price       *decimal.Decimal
	Quantity    *decimal.Decimal
	StopPrice   *decimal.Decimal
	DisplayQty  *decimal.Decimal // iceberg_visible_qty
	TimeInForce string
	GTDExpiry   *time.Time
	// TriggerSource rides the amended wire surface so an attempted
	// switch reaches the engine and is rejected deterministically
	// (AMEND_REJECTED per Task 16.3.17) instead of silently ignored.
	TriggerSource string
}

// CancelReplaceRequest is POST /orders/{id}/cancel-replace — an atomic
// amend with an explicit mode and the same mutable-field set.
type CancelReplaceRequest struct {
	Mode string // STOP_ON_FAILURE | ALLOW_FAILURE (default STOP_ON_FAILURE)
	ModifyRequest
}

// KeepPriorityRequest is PUT /orders/{id}/amend/keep-priority — quantity
// reduction only.
type KeepPriorityRequest struct {
	OrderSeq *uint64
	Quantity *decimal.Decimal
}

// MassCancelScope mirrors accounts.MassCancelScope without importing the
// accounts package (the dispatcher adapter translates).
type MassCancelScope struct {
	AccountID    int64  // 0 = all accounts (admin path only)
	InstrumentID int64  // 0 = all instruments
	Side         string // "" / "ALL" = both; else BUY|SELL
	OrderType    string // "" / "ALL" = all; else LIMIT|STOP|...
	Reason       string // audit tag: "client"|"deadman"|"close_all"|"admin"|"cancel_on_disconnect"
	SessionID    string // non-empty = only orders submitted by that session
}

// ---------------------------------------------------------------------------
// JSON decoding helpers — decimals accepted as JSON string or number,
// never float money (spec §5.3).
// ---------------------------------------------------------------------------

// decodeBody unmarshals the request body; nil body decodes to an empty map.
func decodeBody(body []byte) (map[string]json.RawMessage, error) {
	obj := map[string]json.RawMessage{}
	if len(body) == 0 {
		return obj, nil
	}
	if err := json.Unmarshal(body, &obj); err != nil {
		return nil, fmt.Errorf("body is not a JSON object: %w", err)
	}
	return obj, nil
}

func strField(obj map[string]json.RawMessage, key string) (string, bool, error) {
	raw, ok := obj[key]
	if !ok {
		return "", false, nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", true, fmt.Errorf("field %q must be a string", key)
	}
	return s, true, nil
}

// decField parses a decimal from a JSON string or JSON number. Presence
// is reported separately so required-one-of rules work.
func decField(obj map[string]json.RawMessage, key string) (*decimal.Decimal, bool, error) {
	raw, ok := obj[key]
	if !ok || string(raw) == "null" {
		return nil, false, nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		var num json.Number
		if derr := json.Unmarshal(raw, &num); derr != nil {
			return nil, true, fmt.Errorf("field %q must be a string or number", key)
		}
		s = num.String()
	}
	d, err := decimal.NewFromString(s)
	if err != nil {
		return nil, true, fmt.Errorf("field %q is not a valid decimal: %q", key, s)
	}
	return &d, true, nil
}

func u64Field(obj map[string]json.RawMessage, key string) (*uint64, bool, error) {
	raw, ok := obj[key]
	if !ok || string(raw) == "null" {
		return nil, false, nil
	}
	// Bare integers first: order_seq is a unixnano uint64 (~1.8e18) —
	// decoding via `any` coerces to float64 and silently truncates the
	// low bits, turning every honest STALE_MODIFY fence key into a
	// mismatch (spec §6.9 — the fence is a correctness fence, not a
	// fuzz one).
	if u, err := strconv.ParseUint(string(raw), 10, 64); err == nil {
		return &u, true, nil
	}
	var v any
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		return nil, true, fmt.Errorf("field %q is not valid JSON", key)
	}
	switch n := v.(type) {
	case json.Number:
		u, err := strconv.ParseUint(n.String(), 10, 64)
		if err != nil {
			return nil, true, fmt.Errorf("field %q must be an unsigned integer", key)
		}
		return &u, true, nil
	case string:
		u, err := strconv.ParseUint(n, 10, 64)
		if err != nil {
			return nil, true, fmt.Errorf("field %q must be an unsigned integer", key)
		}
		return &u, true, nil
	default:
		return nil, true, fmt.Errorf("field %q must be an unsigned integer", key)
	}
}

func boolField(obj map[string]json.RawMessage, key string) (bool, error) {
	raw, ok := obj[key]
	if !ok {
		return false, nil
	}
	var b bool
	if err := json.Unmarshal(raw, &b); err != nil {
		return false, fmt.Errorf("field %q must be a boolean", key)
	}
	return b, nil
}

func timeField(obj map[string]json.RawMessage, key string) (*time.Time, bool, error) {
	raw, ok := obj[key]
	if !ok || string(raw) == "null" {
		return nil, false, nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, true, fmt.Errorf("field %q must be an RFC3339 string", key)
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return nil, true, fmt.Errorf("field %q is not RFC3339: %q", key, s)
	}
	return &t, true, nil
}

// dateField parses a civil date for the migration-039 DATE columns —
// strictly "YYYY-MM-DD" (no time-of-day; deterministic across TZ). The
// value lands as a UTC-midnight time.Time so DATE persistence and
// cross-field comparisons are stable.
func dateField(obj map[string]json.RawMessage, key string) (*time.Time, bool, error) {
	raw, ok := obj[key]
	if !ok || string(raw) == "null" {
		return nil, false, nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, true, fmt.Errorf("field %q must be a YYYY-MM-DD string", key)
	}
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		return nil, true, fmt.Errorf("field %q is not YYYY-MM-DD: %q", key, s)
	}
	return &t, true, nil
}

// ParseSubmit decodes + structural-checks a POST /orders body. Deep
// business validation happens in validate.go.
func ParseSubmit(body []byte) (*SubmitRequest, error) {
	obj, err := decodeBody(body)
	if err != nil {
		return nil, err
	}
	req := &SubmitRequest{}
	for _, k := range []struct {
		key string
		dst *string
	}{
		{"symbol", &req.Symbol},
		{"side", &req.Side},
		{"type", &req.OrderType},
		{"time_in_force", &req.TimeInForce},
		{"client_order_id", &req.ClientOrderID},
		{"stp_mode", &req.STPMode},
		{"peg_mode", &req.PegMode},
		{"trigger_source", &req.TriggerSource},
		{"trailing_offset_unit", &req.TrailingOffsetUnit},
		{"fixing_benchmark", &req.FixingBenchmark},
		{"algo_type", &req.AlgoType},
		// Phase-22 Task 22.3.9 derivative params (migration 039).
		{"option_type", &req.OptionType},
		{"exercise_style", &req.ExerciseStyle},
		{"barrier_type", &req.BarrierType},
		{"premium_currency", &req.PremiumCurrency},
		{"ndf_fixing_source", &req.NdfFixingSource},
	} {
		v, present, err := strField(obj, k.key)
		if err != nil {
			return nil, err
		}
		if present {
			*k.dst = strings.TrimSpace(v)
		}
	}
	for _, k := range []struct {
		key string
		dst **decimal.Decimal
	}{
		{"quantity", &req.Quantity},
		{"quote_quantity", &req.QuoteQuantity},
		{"price", &req.Price},
		{"stop_price", &req.StopPrice},
		{"iceberg_visible_qty", &req.DisplayQty},
		{"peg_offset", &req.PegOffset},
		{"peg_limit", &req.PegLimit},
		{"trailing_offset", &req.TrailingOffset},
		{"activation_price", &req.ActivationPrice},
		{"discretionary_offset_pips", &req.DiscretionaryOffsetPips},
		{"strike", &req.Strike},
		{"barrier_level", &req.BarrierLevel},
		{"premium", &req.Premium},
	} {
		v, _, err := decField(obj, k.key)
		if err != nil {
			return nil, err
		}
		*k.dst = v
	}
	if req.GTDExpiry, _, err = timeField(obj, "gtd_expiry"); err != nil {
		return nil, err
	}
	// Phase-22 Task 22.3.9: OPTION expiry is an instant; leg dates are
	// civil dates.
	if req.ExpiryAt, _, err = timeField(obj, "expiry_at"); err != nil {
		return nil, err
	}
	if req.ValueDate, _, err = dateField(obj, "value_date"); err != nil {
		return nil, err
	}
	if req.NearLegValueDate, _, err = dateField(obj, "near_leg_value_date"); err != nil {
		return nil, err
	}
	if req.FarLegValueDate, _, err = dateField(obj, "far_leg_value_date"); err != nil {
		return nil, err
	}
	if req.PostOnly, err = boolField(obj, "post_only"); err != nil {
		return nil, err
	}
	if req.ReduceOnly, err = boolField(obj, "reduce_only"); err != nil {
		return nil, err
	}
	if req.Hidden, err = boolField(obj, "hidden"); err != nil {
		return nil, err
	}
	if req.GSLO, err = boolField(obj, "gslo"); err != nil {
		return nil, err
	}
	// algo_params arrives verbatim — the byte cap and per-algo_type
	// schema checks live in execparams.go (Phase-16 Task 16.3.10).
	if raw, ok := obj["algo_params"]; ok && string(raw) != "null" {
		req.AlgoParams = append(json.RawMessage(nil), raw...)
	}
	return req, nil
}

// ParseSubmitOco decodes POST /orders/oco (Phase-14 Task 14.3.1):
// {"symbol": "...", "legs": [{<leg1>}, {<leg2>}]} — each leg is a full
// POST /orders payload (symbol optional, defaults to the pair-level one;
// the service enforces same-instrument). Exactly two legs required.
func ParseSubmitOco(body []byte) (*SubmitOcoRequest, error) {
	obj, err := decodeBody(body)
	if err != nil {
		return nil, err
	}
	req := &SubmitOcoRequest{}
	if v, present, err := strField(obj, "symbol"); err != nil {
		return nil, err
	} else if present {
		req.Symbol = strings.TrimSpace(v)
	}
	raw, ok := obj["legs"]
	if !ok {
		return nil, fmt.Errorf("oco submit requires a \"legs\" array")
	}
	var legRaws []json.RawMessage
	if err := json.Unmarshal(raw, &legRaws); err != nil {
		return nil, fmt.Errorf("field \"legs\" must be an array of two orders")
	}
	if len(legRaws) != 2 {
		return nil, fmt.Errorf("oco pair requires exactly two legs, got %d",
			len(legRaws))
	}
	for i, lr := range legRaws {
		leg, err := ParseSubmit(lr)
		if err != nil {
			return nil, fmt.Errorf("oco leg %d: %w", i, err)
		}
		req.Legs[i] = leg
	}
	return req, nil
}

// BracketChild configures one protective child of a bracket (spec
// §6.2): trigger_price is mandatory; an optional limit_price upgrades
// the child from a stop-market to a stop-limit order. client_order_id
// is optional — when empty the service derives a deterministic
// "brk{bracket}.{seq}.{sl|tp}" key so fill-triggered placement replays
// idempotently.
type BracketChild struct {
	TriggerPrice  *decimal.Decimal `json:"trigger_price"`
	LimitPrice    *decimal.Decimal `json:"limit_price,omitempty"`
	ClientOrderID string           `json:"client_order_id,omitempty"`
}

// SubmitBracketRequest is POST /orders/bracket (Phase-16 Task 16.3.14):
// Parent is a full POST /orders payload restricted to the entry-order
// vocabulary (LIMIT or MARKET); ChildSL/ChildTP carry only trigger/limit
// — they inherit the parent's instrument, opposite side, TIF-derived
// expiry (parent GTD → child GTD; otherwise GTC) and session.
type SubmitBracketRequest struct {
	Parent  *SubmitRequest
	ChildSL BracketChild
	ChildTP BracketChild
}

// ParseSubmitBracket decodes the composite bracket body:
//
//	{"parent": {<POST /orders body>},
//	 "child_sl": {"trigger_price": ..., "limit_price"?: ...},
//	 "child_tp": {"trigger_price": ..., "limit_price"?: ...}}
func ParseSubmitBracket(body []byte) (*SubmitBracketRequest, error) {
	obj, err := decodeBody(body)
	if err != nil {
		return nil, err
	}
	req := &SubmitBracketRequest{}
	raw, ok := obj["parent"]
	if !ok {
		return nil, fmt.Errorf("bracket submit requires a \"parent\" order")
	}
	parent, err := ParseSubmit(raw)
	if err != nil {
		return nil, fmt.Errorf("bracket parent: %w", err)
	}
	req.Parent = parent
	parseChild := func(key string, dst *BracketChild) error {
		raw, ok := obj[key]
		if !ok {
			return fmt.Errorf("bracket submit requires %q", key)
		}
		var cobj map[string]json.RawMessage
		if err := json.Unmarshal(raw, &cobj); err != nil {
			return fmt.Errorf("field %q must be an object", key)
		}
		if dst.TriggerPrice, _, err = decField(cobj, "trigger_price"); err != nil {
			return err
		}
		if dst.LimitPrice, _, err = decField(cobj, "limit_price"); err != nil {
			return err
		}
		if v, present, err := strField(cobj, "client_order_id"); err != nil {
			return err
		} else if present {
			dst.ClientOrderID = strings.TrimSpace(v)
		}
		return nil
	}
	if err := parseChild("child_sl", &req.ChildSL); err != nil {
		return nil, err
	}
	if err := parseChild("child_tp", &req.ChildTP); err != nil {
		return nil, err
	}
	return req, nil
}

// SubmitOrderListRequest is POST /order-lists (Phase-16 Task 16.3.20):
// ContingencyType selects OPO (one pending leg) or OPOCO (pending legs
// linked as an OCO pair). Working is the BUY entry leg; Pending holds
// the SELL leg(s) whose quantity is recomputed at activation from the
// working fill's net proceeds — any client-supplied pending quantity is
// ignored (the net-proceeds formula owns it per §24 #287).
type SubmitOrderListRequest struct {
	ContingencyType string
	Symbol          string
	ClientOrderID   string
	Working         *SubmitRequest
	Pending         []*SubmitRequest
}

// ParseSubmitOrderList decodes:
//
//	{"contingency_type": "OPO"|"OPOCO", "symbol": "...",
//	 "client_order_id": "...", "working": {<order>},
//	 "pending": [{<order>} (, <order>)]}
func ParseSubmitOrderList(body []byte) (*SubmitOrderListRequest, error) {
	obj, err := decodeBody(body)
	if err != nil {
		return nil, err
	}
	req := &SubmitOrderListRequest{}
	if v, present, err := strField(obj, "contingency_type"); err != nil {
		return nil, err
	} else if present {
		req.ContingencyType = strings.ToUpper(strings.TrimSpace(v))
	}
	if v, present, err := strField(obj, "symbol"); err != nil {
		return nil, err
	} else if present {
		req.Symbol = strings.TrimSpace(v)
	}
	if v, present, err := strField(obj, "client_order_id"); err != nil {
		return nil, err
	} else if present {
		req.ClientOrderID = strings.TrimSpace(v)
	}
	raw, ok := obj["working"]
	if !ok {
		return nil, fmt.Errorf("order-list submit requires a \"working\" order")
	}
	working, err := ParseSubmit(raw)
	if err != nil {
		return nil, fmt.Errorf("working leg: %w", err)
	}
	req.Working = working
	praw, ok := obj["pending"]
	if !ok {
		return nil, fmt.Errorf("order-list submit requires a \"pending\" leg array")
	}
	var legRaws []json.RawMessage
	if err := json.Unmarshal(praw, &legRaws); err != nil {
		return nil, fmt.Errorf("field \"pending\" must be an array of orders")
	}
	for i, lr := range legRaws {
		leg, err := ParseSubmit(lr)
		if err != nil {
			return nil, fmt.Errorf("pending leg %d: %w", i, err)
		}
		req.Pending = append(req.Pending, leg)
	}
	return req, nil
}

// ParseModify decodes PUT /orders/{id}.
func ParseModify(body []byte) (*ModifyRequest, error) {
	obj, err := decodeBody(body)
	if err != nil {
		return nil, err
	}
	req := &ModifyRequest{}
	if req.OrderSeq, _, err = u64Field(obj, "order_seq"); err != nil {
		return nil, err
	}
	for _, k := range []struct {
		key string
		dst **decimal.Decimal
	}{
		{"price", &req.Price},
		{"quantity", &req.Quantity},
		{"stop_price", &req.StopPrice},
		{"iceberg_visible_qty", &req.DisplayQty},
	} {
		v, _, err := decField(obj, k.key)
		if err != nil {
			return nil, err
		}
		*k.dst = v
	}
	for _, k := range []struct {
		key string
		dst *string
	}{
		{"time_in_force", &req.TimeInForce},
		{"trigger_source", &req.TriggerSource},
	} {
		v, _, err := strField(obj, k.key)
		if err != nil {
			return nil, err
		}
		*k.dst = strings.TrimSpace(v)
	}
	if req.GTDExpiry, _, err = timeField(obj, "gtd_expiry"); err != nil {
		return nil, err
	}
	return req, nil
}

// ParseCancelReplace decodes POST /orders/{id}/cancel-replace.
func ParseCancelReplace(body []byte) (*CancelReplaceRequest, error) {
	obj, err := decodeBody(body)
	if err != nil {
		return nil, err
	}
	m, err := ParseModify(body)
	if err != nil {
		return nil, err
	}
	req := &CancelReplaceRequest{Mode: "STOP_ON_FAILURE", ModifyRequest: *m}
	if v, present, err := strField(obj, "mode"); err != nil {
		return nil, err
	} else if present && v != "" {
		req.Mode = strings.ToUpper(v)
	}
	return req, nil
}

// ParseKeepPriority decodes PUT /orders/{id}/amend/keep-priority.
func ParseKeepPriority(body []byte) (*KeepPriorityRequest, error) {
	obj, err := decodeBody(body)
	if err != nil {
		return nil, err
	}
	req := &KeepPriorityRequest{}
	if req.OrderSeq, _, err = u64Field(obj, "order_seq"); err != nil {
		return nil, err
	}
	if req.Quantity, _, err = decField(obj, "quantity"); err != nil {
		return nil, err
	}
	return req, nil
}

// ---------------------------------------------------------------------------
// Phase-22 Task 22.3.9 — derivative order parameters (migration 039)
// ---------------------------------------------------------------------------
//
// Canonical instrument-class + derivative vocabularies (spec §5.1
// instrument_type_enum / §5.4 derivative columns). Strings are
// normalized to upper-case at parse time; validation below enforces
// presence/absence per instrument class.

const (
	InstTypeSpot    = "SPOT"
	InstTypeForward = "FORWARD"
	InstTypeSwap    = "SWAP"
	InstTypeNDF     = "NDF"
	InstTypeOption  = "OPTION"
)

// Option / barrier / premium vocabularies — spec §5.4 canonical strings.
const (
	OptionTypeCall   = "CALL"
	OptionTypePut    = "PUT"
	OptionTypeBinary = "BINARY"
)
const (
	ExerciseEuropean = "EUROPEAN"
	ExerciseAmerican = "AMERICAN"
)
const (
	BarrierUpAndIn    = "UP_AND_IN"
	BarrierUpAndOut   = "UP_AND_OUT"
	BarrierDownAndIn  = "DOWN_AND_IN"
	BarrierDownAndOut = "DOWN_AND_OUT"
)
const (
	PremiumCurrencyQuote      = "QUOTE"
	PremiumCurrencySettlement = "SETTLEMENT"
)

// Settlement modes from instruments.settlement_mode (migration 031).
const (
	SettlementModeGross = "GROSS"
	SettlementModeNet   = "NET"
)

// isDerivativeInstrumentType reports whether the instrument class
// carries derivative order parameters (spec §5.4 instrument_type_enum).
func isDerivativeInstrumentType(t string) bool {
	switch t {
	case InstTypeForward, InstTypeSwap, InstTypeNDF, InstTypeOption:
		return true
	}
	return false
}

// DerivativeParams is the migration-039 column bundle persisted alongside
// the order row. It travels request → service → store explicitly (the
// base orderCols/scanOrder surface is store.go-owned and untouched).
type DerivativeParams struct {
	Strike           *decimal.Decimal
	OptionType       string
	ExerciseStyle    string
	ExpiryAt         *time.Time
	BarrierType      string
	BarrierLevel     *decimal.Decimal
	ValueDate        *time.Time
	NearLegValueDate *time.Time
	FarLegValueDate  *time.Time
	Premium          *decimal.Decimal
	PremiumCurrency  string
	NdfFixingSource  string
}

// derivParamsFromRequest extracts the persistable bundle; nil when the
// request carries no derivative fields at all.
func derivParamsFromRequest(req *SubmitRequest) *DerivativeParams {
	if !hasDerivativeParams(req) {
		return nil
	}
	return &DerivativeParams{
		Strike:           req.Strike,
		OptionType:       req.OptionType,
		ExerciseStyle:    req.ExerciseStyle,
		ExpiryAt:         req.ExpiryAt,
		BarrierType:      req.BarrierType,
		BarrierLevel:     req.BarrierLevel,
		ValueDate:        req.ValueDate,
		NearLegValueDate: req.NearLegValueDate,
		FarLegValueDate:  req.FarLegValueDate,
		Premium:          req.Premium,
		PremiumCurrency:  req.PremiumCurrency,
		NdfFixingSource:  req.NdfFixingSource,
	}
}

// hasDerivativeParams reports whether any migration-039 field is set.
func hasDerivativeParams(req *SubmitRequest) bool {
	return req.Strike != nil ||
		req.OptionType != "" ||
		req.ExerciseStyle != "" ||
		req.ExpiryAt != nil ||
		req.BarrierType != "" ||
		req.BarrierLevel != nil ||
		req.ValueDate != nil ||
		req.NearLegValueDate != nil ||
		req.FarLegValueDate != nil ||
		req.Premium != nil ||
		req.PremiumCurrency != "" ||
		req.NdfFixingSource != ""
}

// applyTo copies the bundle onto an Order read/write view.
func (d *DerivativeParams) applyTo(o *Order) {
	if d == nil || o == nil {
		return
	}
	o.Strike = d.Strike
	o.BarrierLevel = d.BarrierLevel
	o.ValueDate = d.ValueDate
	o.NearLegValueDate = d.NearLegValueDate
	o.FarLegValueDate = d.FarLegValueDate
	o.Premium = d.Premium
	o.ExpiryAt = d.ExpiryAt
	if d.OptionType != "" {
		v := d.OptionType
		o.OptionType = &v
	}
	if d.ExerciseStyle != "" {
		v := d.ExerciseStyle
		o.ExerciseStyle = &v
	}
	if d.BarrierType != "" {
		v := d.BarrierType
		o.BarrierType = &v
	}
	if d.PremiumCurrency != "" {
		v := d.PremiumCurrency
		o.PremiumCurrency = &v
	}
	if d.NdfFixingSource != "" {
		v := d.NdfFixingSource
		o.NdfFixingSource = &v
	}
}

func validOptionType(s string) bool {
	switch s {
	case OptionTypeCall, OptionTypePut, OptionTypeBinary:
		return true
	}
	return false
}
func validExerciseStyle(s string) bool {
	return s == ExerciseEuropean || s == ExerciseAmerican
}
func validBarrierType(s string) bool {
	switch s {
	case BarrierUpAndIn, BarrierUpAndOut, BarrierDownAndIn, BarrierDownAndOut:
		return true
	}
	return false
}
func validPremiumCurrency(s string) bool {
	return s == PremiumCurrencyQuote || s == PremiumCurrencySettlement
}

// validateDerivativeParams enforces the Task 22.3.9 per-instrument-class
// contract (spec §5.4, §15.7). It runs after ValidateSubmit on every
// direct submit path (Submit, BatchSubmit pre-pass, DryRun) — validate.go
// is frozen by change scope, so the hook lives in service.go.
//
// Fail-closed posture: an instrument class the validator does not know
// rejects when derivative fields are present; a derivative-class
// instrument whose settlement_mode could not be resolved rejects rather
// than guessing.
//
// now is injected — the service never reads wall clock inside validation
// beyond the single now it already pins.
func validateDerivativeParams(req *SubmitRequest, inst *Instrument, now time.Time) error {
	// Normalize enum spellings once, before checks.
	req.OptionType = strings.ToUpper(strings.TrimSpace(req.OptionType))
	req.ExerciseStyle = strings.ToUpper(strings.TrimSpace(req.ExerciseStyle))
	req.BarrierType = strings.ToUpper(strings.TrimSpace(req.BarrierType))
	req.PremiumCurrency = strings.ToUpper(strings.TrimSpace(req.PremiumCurrency))
	req.NdfFixingSource = strings.ToUpper(strings.TrimSpace(req.NdfFixingSource))

	it := strings.ToUpper(strings.TrimSpace(inst.InstrumentType))
	hasParams := hasDerivativeParams(req)

	// Enum-shape checks run whenever a field is present, regardless of
	// instrument class — a malformed value never silently flows through.
	if req.OptionType != "" && !validOptionType(req.OptionType) {
		return codeErr("DERIVATIVE_PARAMS_INVALID",
			"option_type must be CALL, PUT or BINARY")
	}
	if req.ExerciseStyle != "" && !validExerciseStyle(req.ExerciseStyle) {
		return codeErr("DERIVATIVE_PARAMS_INVALID",
			"exercise_style must be EUROPEAN or AMERICAN")
	}
	if req.BarrierType != "" && !validBarrierType(req.BarrierType) {
		return codeErr("DERIVATIVE_PARAMS_INVALID",
			"barrier_type must be UP_AND_IN, UP_AND_OUT, DOWN_AND_IN or DOWN_AND_OUT")
	}
	if req.PremiumCurrency != "" && !validPremiumCurrency(req.PremiumCurrency) {
		return codeErr("DERIVATIVE_PARAMS_INVALID",
			"premium_currency must be QUOTE or SETTLEMENT")
	}
	if req.Strike != nil && !req.Strike.IsPositive() {
		return codeErr("DERIVATIVE_PARAMS_INVALID", "strike must be > 0")
	}
	if req.BarrierLevel != nil && !req.BarrierLevel.IsPositive() {
		return codeErr("DERIVATIVE_PARAMS_INVALID", "barrier_level must be > 0")
	}
	if req.Premium != nil && !req.Premium.IsPositive() {
		return codeErr("DERIVATIVE_PARAMS_INVALID", "premium must be > 0")
	}
	// Barrier fields are an atomic pair (mirrors the migration CHECK).
	if (req.BarrierType == "") != (req.BarrierLevel == nil) {
		return codeErr("DERIVATIVE_PARAMS_INVALID",
			"barrier_type and barrier_level must be provided together")
	}
	// Swap legs are an atomic pair, far strictly after near.
	if (req.NearLegValueDate == nil) != (req.FarLegValueDate == nil) {
		return codeErr("DERIVATIVE_PARAMS_INVALID",
			"near_leg_value_date and far_leg_value_date must be provided together")
	}
	if req.NearLegValueDate != nil &&
		!req.FarLegValueDate.After(*req.NearLegValueDate) {
		return codeErr("DERIVATIVE_PARAMS_INVALID",
			"far_leg_value_date must be strictly after near_leg_value_date")
	}
	// An OPTION expiry after its own delivery date is impossible.
	if req.ExpiryAt != nil && req.ValueDate != nil &&
		civilDateUTC(*req.ExpiryAt).After(*req.ValueDate) {
		return codeErr("DERIVATIVE_PARAMS_INVALID",
			"expiry_at date must be on or before value_date")
	}
	// Value dates cannot sit in the past (civil-date floor of now).
	today := civilDateUTC(now)
	if req.ValueDate != nil && req.ValueDate.Before(today) {
		return codeErr("DERIVATIVE_PARAMS_INVALID",
			"value_date must be today or later")
	}
	if req.NearLegValueDate != nil && req.NearLegValueDate.Before(today) {
		return codeErr("DERIVATIVE_PARAMS_INVALID",
			"near_leg_value_date must be today or later")
	}
	if req.FarLegValueDate != nil && req.FarLegValueDate.Before(today) {
		return codeErr("DERIVATIVE_PARAMS_INVALID",
			"far_leg_value_date must be today or later")
	}

	if !isDerivativeInstrumentType(it) {
		if hasParams {
			return codeErr("DERIVATIVE_PARAMS_INVALID",
				"derivative order parameters are not valid for %s instruments", it)
		}
		return nil
	}

	// Instrument linkage (Task 22.3.9): a derivative-class instrument
	// must resolve a known instruments.settlement_mode — the service
	// populates it through the linkage seam before validation.
	if inst.SettlementMode != SettlementModeGross &&
		inst.SettlementMode != SettlementModeNet {
		return codeErr("DERIVATIVE_PARAMS_INVALID",
			"instrument %s has unresolvable settlement_mode %q",
			inst.Symbol, inst.SettlementMode)
	}

	// Fields that never apply to the resolved class are rejected —
	// dropping them silently would corrupt the semantic request
	// identity (the dedup hash covers them).
	rejectOptFields := func() error {
		if req.Strike != nil || req.OptionType != "" || req.ExerciseStyle != "" ||
			req.ExpiryAt != nil || req.BarrierType != "" || req.BarrierLevel != nil ||
			req.Premium != nil || req.PremiumCurrency != "" {
			return codeErr("DERIVATIVE_PARAMS_INVALID",
				"option fields are not valid on %s instruments", it)
		}
		return nil
	}
	rejectSwapLegs := func() error {
		if req.NearLegValueDate != nil || req.FarLegValueDate != nil {
			return codeErr("DERIVATIVE_PARAMS_INVALID",
				"swap leg value dates are not valid on %s instruments", it)
		}
		return nil
	}
	rejectNdfSource := func() error {
		if req.NdfFixingSource != "" {
			return codeErr("DERIVATIVE_PARAMS_INVALID",
				"ndf_fixing_source is only valid on NDF instruments")
		}
		return nil
	}

	switch it {
	case InstTypeForward:
		if req.ValueDate == nil {
			return codeErr("DERIVATIVE_PARAMS_INVALID",
				"FORWARD orders require value_date")
		}
		if err := rejectOptFields(); err != nil {
			return err
		}
		if err := rejectSwapLegs(); err != nil {
			return err
		}
		if err := rejectNdfSource(); err != nil {
			return err
		}
	case InstTypeSwap:
		if req.NearLegValueDate == nil || req.FarLegValueDate == nil {
			return codeErr("DERIVATIVE_PARAMS_INVALID",
				"SWAP orders require near_leg_value_date and far_leg_value_date")
		}
		if req.ValueDate != nil {
			return codeErr("DERIVATIVE_PARAMS_INVALID",
				"value_date is not valid on SWAP instruments — use the leg value dates")
		}
		if err := rejectOptFields(); err != nil {
			return err
		}
		if err := rejectNdfSource(); err != nil {
			return err
		}
	case InstTypeNDF:
		// Spec §15.7/§5.4 note: the NDF fixing source is the canonical
		// anchor (central-bank/vendor/prior-day hierarchy); the
		// three-value fixing_benchmark enum cannot express it.
		if req.NdfFixingSource == "" {
			return codeErr("DERIVATIVE_PARAMS_INVALID",
				"NDF orders require ndf_fixing_source")
		}
		if req.ValueDate == nil {
			return codeErr("DERIVATIVE_PARAMS_INVALID",
				"NDF orders require value_date (cash settlement date)")
		}
		if err := rejectOptFields(); err != nil {
			return err
		}
		if err := rejectSwapLegs(); err != nil {
			return err
		}
	case InstTypeOption:
		if req.Strike == nil || req.OptionType == "" ||
			req.ExerciseStyle == "" || req.ExpiryAt == nil {
			return codeErr("DERIVATIVE_PARAMS_INVALID",
				"OPTION orders require strike, option_type, exercise_style and expiry_at")
		}
		if !req.ExpiryAt.After(now) {
			return codeErr("DERIVATIVE_PARAMS_INVALID",
				"expiry_at must be in the future")
		}
		// A purchased option carries a premium obligation (§6.3 option
		// legs); a sell-side short option books premium received — the
		// amount is still required so the premium ledger is explicit.
		if req.Premium == nil {
			return codeErr("DERIVATIVE_PARAMS_INVALID",
				"OPTION orders require premium")
		}
		// BINARY pays a fixed cash amount — premium denomination must be
		// explicit rather than defaulting to market convention.
		if req.OptionType == OptionTypeBinary && req.PremiumCurrency == "" {
			return codeErr("DERIVATIVE_PARAMS_INVALID",
				"BINARY options require explicit premium_currency")
		}
		if req.ValueDate != nil && req.ValueDate.Before(civilDateUTC(*req.ExpiryAt)) {
			return codeErr("DERIVATIVE_PARAMS_INVALID",
				"value_date must be on or after the expiry date")
		}
		if err := rejectSwapLegs(); err != nil {
			return err
		}
		if err := rejectNdfSource(); err != nil {
			return err
		}
	default:
		// Unknown derivative-class instrument types never reach here
		// (isDerivativeInstrumentType guards), but keep the tail
		// fail-closed in case the instrument enum grows.
		return codeErr("DERIVATIVE_PARAMS_INVALID",
			"unsupported derivative instrument type %q", it)
	}
	return nil
}

// civilDateUTC reduces a timestamp to its civil date (UTC) — expiry vs
// value-date comparisons are date-grain, not instant-grain.
func civilDateUTC(t time.Time) time.Time {
	u := t.UTC()
	return time.Date(u.Year(), u.Month(), u.Day(), 0, 0, 0, 0, time.UTC)
}
