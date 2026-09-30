package accounts

import (
	"context"
	"time"

	"exchange/pkg/decimal"
)

// dispatch.go — the order-flow seam shared by the dead-man switch
// (Task 5.3.33) and close-all positions (Task 5.3.36).
//
// Both flows terminate in the matching engine's mass-cancel / order-entry
// path, which is owned by the order pipeline (Tasks 5.3.24/5.3.25
// scoped mass cancel; Phase-02 core ORDER_CANCEL/WAL). This package
// defines the contract; the gateway wires the concrete implementation
// (Aeron mass-cancel + order submission) when the order route registry
// mounts these handlers.

// OrderSide mirrors order_side_enum.
type OrderSide string

const (
	SideBuy  OrderSide = "BUY"
	SideSell OrderSide = "SELL"
)

// MassCancelScope narrows a mass cancel (Task 5.3.24 item 5 filter
// dimensions: account_id, instrument_id, side, order_type).
type MassCancelScope struct {
	AccountID    int64
	InstrumentID int64  // 0 = all instruments
	Side         string // "" = both; else BUY|SELL
	OrderType    string // "" = all
	Reason       string // audit trail tag, e.g. "deadman" | "close_all"
}

// MassCancelResult reports the engine's cancel outcome.
type MassCancelResult struct {
	Cancelled int    `json:"cancelled"`
	Detail    string `json:"detail,omitempty"`
}

// CloseOrderRequest is one reduce-only market close dispatched per open
// position by close-all (Task 5.3.36). Slippage protection maps to the
// engine's max_slippage_bps mechanism (Phase-02 Task 2.3.15): the market
// close is converted to a synthetic limit at mark±bps.
//
// Auction legs (spec §13.4) set LimitPrice explicitly — it becomes the
// cap instead of the synthetic mark±bps band — and ExpireAt to rest the
// leg as a reduce-only GTD through the CALL/EXTEND/FILL window instead
// of an immediate IOC.
type CloseOrderRequest struct {
	AccountID      int64
	InstrumentID   int64
	Side           OrderSide       // opposite of the open position
	Quantity       decimal.Decimal // full position quantity
	ReduceOnly     bool            // always true on this path
	MaxSlippageBps int             // slippage guard; <=0 uses venue default
	// LimitPrice is an explicit cap (BUY) / floor (SELL); when positive
	// it supersedes the synthetic mark±slippage band (auction legs).
	LimitPrice decimal.Decimal
	// ExpireAt rests the close as GTD until the timestamp instead of an
	// IOC — the §13.4 CALL/EXTEND windows need the leg on the book.
	ExpireAt      *time.Time
	ClientOrderID string // idempotency key for the dispatch
}

// OrderAck is the order pipeline's acceptance receipt for one close.
type OrderAck struct {
	OrderID       int64  `json:"order_id"`
	ClientOrderID string `json:"client_order_id"`
	Accepted      bool   `json:"accepted"`
	Detail        string `json:"detail,omitempty"`
}

// OrderDispatcher is the seam into the order pipeline. Implementations
// must be fail-closed: a dispatch failure returns an error, never a
// fabricated ack.
type OrderDispatcher interface {
	// MassCancel cancels every resting order matching scope. Called by
	// the dead-man sweeper on countdown expiry and by close-all before
	// the close orders are submitted.
	MassCancel(ctx context.Context, scope MassCancelScope) (*MassCancelResult, error)
	// SubmitClose places one reduce-only market close order.
	SubmitClose(ctx context.Context, req CloseOrderRequest) (*OrderAck, error)
}
