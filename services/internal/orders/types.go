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
}

// Account is the order-flow view of an accounts row.
type Account struct {
	ID                 int64
	UserID             int64
	Type               string // SPOT | MARGIN | PORTFOLIO
	KycTier            string // T0 | T1 | T2
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
	CreatedAt     time.Time
	UpdatedAt     time.Time
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
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, true, fmt.Errorf("field %q is not valid JSON", key)
	}
	switch n := v.(type) {
	case float64:
		if n < 0 || n != float64(uint64(n)) {
			return nil, true, fmt.Errorf("field %q must be an unsigned integer", key)
		}
		u := uint64(n)
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
	if req.PostOnly, err = boolField(obj, "post_only"); err != nil {
		return nil, err
	}
	if req.ReduceOnly, err = boolField(obj, "reduce_only"); err != nil {
		return nil, err
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
	if v, _, err := strField(obj, "time_in_force"); err != nil {
		return nil, err
	} else {
		req.TimeInForce = v
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
