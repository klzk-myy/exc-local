package algo

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"exchange/pkg/decimal"
	excerrors "exchange/pkg/errors"
)

// codeErr builds the coded error the gateway envelope emits — every code
// used here is pre-registered in errs (Task 5.3.21 emission gate).
func codeErr(code, format string, args ...any) *excerrors.Error {
	return excerrors.New(code, fmt.Sprintf(format, args...))
}

// ---------------------------------------------------------------------------
// Algo types & parent state machine (Task 16.3.8)
// ---------------------------------------------------------------------------

const (
	TypeTWAP   = "TWAP"
	TypeVWAP   = "VWAP"
	TypeVP     = "VP"
	TypeScaled = "SCALE"
	TypeSpread = "SPREAD"
)

// Parent lifecycle states — see migration 224 CHECK constraint.
const (
	StatusNew       = "NEW"
	StatusPending   = "PENDING"
	StatusRunning   = "RUNNING"
	StatusPaused    = "PAUSED"
	StatusCompleted = "COMPLETED"
	StatusCancelled = "CANCELLED"
	StatusExpired   = "EXPIRED"
	StatusFailed    = "FAILED"
)

// Terminal parent states — once reached a driver never restarts.
func isTerminalStatus(s string) bool {
	switch s {
	case StatusCompleted, StatusCancelled, StatusExpired, StatusFailed:
		return true
	}
	return false
}

// Child row statuses (algo_order_children.status).
const (
	ChildPending   = "PENDING"   // durable intent, not yet dispatched
	ChildSubmitted = "SUBMITTED" // dispatched, ack not yet observed
	ChildOpen      = "OPEN"      // resting/working on the book
	ChildFilled    = "FILLED"
	ChildPartial   = "PARTIAL"
	ChildCancelled = "CANCELLED"
	ChildRejected  = "REJECTED"
)

const (
	RoleSlice  = "SLICE"
	RoleLeg    = "LEG"
	RoleUnwind = "UNWIND"
)

// ---------------------------------------------------------------------------
// Parent / child records (migration 224)
// ---------------------------------------------------------------------------

// Parent is one algo_orders row — the durable strategy intent.
type Parent struct {
	ID            int64
	AccountID     int64
	Type          string
	Symbol        string
	Side          string
	Status        string
	TotalQty      decimal.Decimal
	FilledQty     decimal.Decimal
	Params        json.RawMessage
	State         json.RawMessage
	StartAt       *time.Time
	ExpiresAt     *time.Time
	ClientOrderID string
	ErrorDetail   string
	CreatedAt     time.Time
	UpdatedAt     time.Time
	CompletedAt   *time.Time
}

// View serializes a parent for REST responses.
func (p *Parent) View() map[string]any {
	v := map[string]any{
		"algo_order_id": p.ID,
		"account_id":    p.AccountID,
		"algo_type":     p.Type,
		"symbol":        p.Symbol,
		"side":          p.Side,
		"status":        p.Status,
		"total_qty":     p.TotalQty.String(),
		"filled_qty":    p.FilledQty.String(),
		"params":        json.RawMessage(p.Params),
		"created_at":    p.CreatedAt.UTC().Format(time.RFC3339Nano),
		"updated_at":    p.UpdatedAt.UTC().Format(time.RFC3339Nano),
	}
	if p.ClientOrderID != "" {
		v["client_order_id"] = p.ClientOrderID
	}
	if p.StartAt != nil {
		v["start_at"] = p.StartAt.UTC().Format(time.RFC3339Nano)
	}
	if p.ExpiresAt != nil {
		v["expires_at"] = p.ExpiresAt.UTC().Format(time.RFC3339Nano)
	}
	if p.CompletedAt != nil {
		v["completed_at"] = p.CompletedAt.UTC().Format(time.RFC3339Nano)
	}
	if p.ErrorDetail != "" {
		v["error_detail"] = p.ErrorDetail
	}
	return v
}

// Child is one algo_order_children row — the durable per-slice audit.
type Child struct {
	ID            int64
	AlgoOrderID   int64
	Seq           int
	SliceIndex    int
	Role          string
	Symbol        string
	Side          string
	ClientOrderID string
	OrderID       *int64
	Qty           decimal.Decimal
	Price         *decimal.Decimal
	Status        string
	FilledQty     decimal.Decimal
	Detail        string
	DispatchedAt  *time.Time
	ResolvedAt    *time.Time
}

// View serializes a child row.
func (c *Child) View() map[string]any {
	v := map[string]any{
		"algo_order_id":   c.AlgoOrderID,
		"seq":             c.Seq,
		"slice_index":     c.SliceIndex,
		"role":            c.Role,
		"symbol":          c.Symbol,
		"side":            c.Side,
		"client_order_id": c.ClientOrderID,
		"qty":             c.Qty.String(),
		"status":          c.Status,
		"filled_qty":      c.FilledQty.String(),
	}
	if c.OrderID != nil {
		v["order_id"] = *c.OrderID
	}
	if c.Price != nil {
		v["price"] = c.Price.String()
	}
	if c.Detail != "" {
		v["detail"] = c.Detail
	}
	if c.DispatchedAt != nil {
		v["dispatched_at"] = c.DispatchedAt.UTC().Format(time.RFC3339Nano)
	}
	return v
}

// childCID derives the idempotent child client_order_id
// (parent_algo_id + dispatch seq) — a retry of the same durable child
// row replays the stored ack at the order pipeline (§8.7).
func childCID(parentID int64, seq int) string {
	return fmt.Sprintf("algo:%d:%d", parentID, seq)
}

// ---------------------------------------------------------------------------
// Submit request + per-strategy params
// ---------------------------------------------------------------------------

// SubmitRequest is the normalized POST /orders/algo body (and the
// payload the typed /orders/{twap,vwap,scaled,spread} endpoints build).
type SubmitRequest struct {
	AlgoType      string
	Symbol        string
	Side          string
	TotalQty      decimal.Decimal
	Params        json.RawMessage // strategy params (validated per type)
	StartAt       *time.Time      // delayed dispatch
	ClientOrderID string
	SessionID     string // server-side session attribution
}

// TWAPParams — POST /orders/twap and algo_type=TWAP.
//
//	interval_secs: 1..3600 (Task 16.3.10 step 3 validation band)
//	duration_secs: >0
type TWAPParams struct {
	IntervalSecs   int64 `json:"interval_secs"`
	DurationSecs   int64 `json:"duration_secs"`
	DiscretionPips int64 `json:"discretion_pips,omitempty"` // 0..3
}

// VWAPParams — POST /orders/vwap and algo_type=VWAP.
type VWAPParams struct {
	DurationSecs   int64 `json:"duration_secs"`
	IntervalSecs   int64 `json:"interval_secs,omitempty"` // default 60
	DiscretionPips int64 `json:"discretion_pips,omitempty"`
}

// VPParams — Task 16.3.18 volume participation.
type VPParams struct {
	ParticipationRate float64 `json:"participation_rate"` // 0.01..0.50
	MaxDurationSecs   int64   `json:"max_duration_secs"`
	PriceLimit        *string `json:"price_limit,omitempty"`
	DiscretionPips    int64   `json:"discretion_pips,omitempty"`
}

// ScaledParams — POST /orders/scaled (Task 16.3.7).
type ScaledParams struct {
	Levels       int      `json:"levels"`       // 1..20
	Distribution string   `json:"distribution"` // EQUAL | LINEAR | CUSTOM
	Weights      []string `json:"weights"`      // CUSTOM: decimal strings, len==levels
	StartPrice   *string  `json:"start_price"`  // default: current mid
	SpacingPips  *string  `json:"spacing_pips"` // spacing in pips (OR spacing_bps)
	SpacingBps   *string  `json:"spacing_bps"`  // spacing in basis points
	LevelPrices  []string `json:"level_prices"` // CUSTOM explicit levels (len==levels)
}

// SpreadParams — POST /orders/spread (Task 16.3.6).
// Exactly two legs, opposite sides; spread_price = leg1_price − leg2_price.
type SpreadParams struct {
	Legs        []SpreadLeg `json:"legs"`
	SpreadPrice string      `json:"spread_price"`
}

// SpreadLeg is one leg of a spread order.
type SpreadLeg struct {
	Symbol   string `json:"symbol"`
	Side     string `json:"side"`
	Quantity string `json:"quantity"`
}

// ---------------------------------------------------------------------------
// Decoding helpers — same conventions as internal/orders/types.go
// ---------------------------------------------------------------------------

func decodeObject(body []byte) (map[string]json.RawMessage, error) {
	obj := map[string]json.RawMessage{}
	if len(body) == 0 {
		return obj, nil
	}
	if err := json.Unmarshal(body, &obj); err != nil {
		return nil, fmt.Errorf("body is not a JSON object: %w", err)
	}
	return obj, nil
}

func strOf(obj map[string]json.RawMessage, key string) (string, error) {
	raw, ok := obj[key]
	if !ok {
		return "", nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", fmt.Errorf("field %q must be a string", key)
	}
	return strings.TrimSpace(s), nil
}

func decOf(obj map[string]json.RawMessage, key string) (*decimal.Decimal, error) {
	raw, ok := obj[key]
	if !ok || string(raw) == "null" {
		return nil, nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		var num json.Number
		if derr := json.Unmarshal(raw, &num); derr != nil {
			return nil, fmt.Errorf("field %q must be a string or number", key)
		}
		s = num.String()
	}
	d, err := decimal.NewFromString(s)
	if err != nil {
		return nil, fmt.Errorf("field %q is not a valid decimal: %q", key, s)
	}
	return &d, nil
}

func timeOf(obj map[string]json.RawMessage, key string) (*time.Time, error) {
	raw, ok := obj[key]
	if !ok || string(raw) == "null" {
		return nil, nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, fmt.Errorf("field %q must be an RFC3339 string", key)
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return nil, fmt.Errorf("field %q is not RFC3339: %q", key, s)
	}
	return &t, nil
}

// ParseAlgoSubmit decodes POST /api/v1/orders/algo:
//
//	{"algo_type":"TWAP","symbol":"EUR/USD","side":"BUY",
//	 "total_qty":"1000000","params":{...},"start_at":"...",
//	 "client_order_id":"..."}
//
// The typed endpoints (/orders/twap etc.) fold their whole body into
// Params via ParseTypedSubmit instead.
func ParseAlgoSubmit(body []byte) (*SubmitRequest, error) {
	obj, err := decodeObject(body)
	if err != nil {
		return nil, err
	}
	req := &SubmitRequest{}
	if req.AlgoType, err = strOf(obj, "algo_type"); err != nil {
		return nil, err
	}
	if req.AlgoType == "" {
		if req.AlgoType, err = strOf(obj, "type"); err != nil {
			return nil, err
		}
	}
	req.AlgoType = strings.ToUpper(req.AlgoType)
	if req.Symbol, err = strOf(obj, "symbol"); err != nil {
		return nil, err
	}
	if req.Side, err = strOf(obj, "side"); err != nil {
		return nil, err
	}
	req.Side = strings.ToUpper(req.Side)
	if req.ClientOrderID, err = strOf(obj, "client_order_id"); err != nil {
		return nil, err
	}
	var q *decimal.Decimal
	if q, err = decOf(obj, "total_qty"); err != nil {
		return nil, err
	}
	if q == nil {
		if q, err = decOf(obj, "quantity"); err != nil {
			return nil, err
		}
	}
	if q != nil {
		req.TotalQty = *q
	}
	if req.StartAt, err = timeOf(obj, "start_at"); err != nil {
		return nil, err
	}
	if raw, ok := obj["params"]; ok {
		req.Params = raw
	}
	return req, nil
}

// ParseTypedSubmit decodes the typed endpoints: the flat body carries the
// strategy params inline alongside symbol/side/total_qty — e.g.
// POST /orders/twap {"symbol","side","total_qty","duration_secs","interval_secs"}.
// The whole object is re-encoded as Params for the strategy validator.
func ParseTypedSubmit(algoType string, body []byte) (*SubmitRequest, error) {
	obj, err := decodeObject(body)
	if err != nil {
		return nil, err
	}
	req := &SubmitRequest{AlgoType: algoType}
	if req.Symbol, err = strOf(obj, "symbol"); err != nil {
		return nil, err
	}
	if req.Side, err = strOf(obj, "side"); err != nil {
		return nil, err
	}
	req.Side = strings.ToUpper(req.Side)
	if req.ClientOrderID, err = strOf(obj, "client_order_id"); err != nil {
		return nil, err
	}
	var q *decimal.Decimal
	if q, err = decOf(obj, "total_qty"); err != nil {
		return nil, err
	}
	if q == nil {
		if q, err = decOf(obj, "quantity"); err != nil {
			return nil, err
		}
	}
	if q != nil {
		req.TotalQty = *q
	}
	if req.StartAt, err = timeOf(obj, "start_at"); err != nil {
		return nil, err
	}
	req.Params = body // full body is the params document for typed endpoints
	return req, nil
}
