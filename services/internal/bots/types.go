// Package bots implements the Phase-16 Task 16.3.19 grid trading bot
// engine (spec §5.39 migration 071, §24 #275, ruling R13).
//
// A grid bot is a server-managed strategy: a parent grid_bots row owns
// grid_bot_orders children — real LIMIT orders dispatched through the
// orders.Service admission/risk/balance/dispatch pipeline (never a side
// channel). On a child fill the engine places the opposite order at the
// adjacent grid level and books the round-trip as realized grid PnL.
//
// Concurrency contract: at most MaxConcurrentBots RUNNING bots per
// account, enforced inside the create transaction under an account row
// lock. Fill→counter flips are idempotent under at-least-once delivery:
// grid_bot_orders.source_child_id carries a partial unique index, so a
// re-delivered fill can never spawn a second counter order.
package bots

import (
	"fmt"
	"time"

	"exchange/pkg/decimal"
	excerrors "exchange/pkg/errors"
)

// Canonical §23 codes emitted by this package (registered in
// internal/errs/codes.go — see spec §23 rows).
const (
	CodeMaxGridBotsExceeded    = "MAX_GRID_BOTS_EXCEEDED"
	CodeGridParametersInvalid  = "GRID_PARAMETERS_INVALID"
	CodeGridMarginInsufficient = "GRID_MARGIN_INSUFFICIENT"
	CodeNotFound               = "NOT_FOUND"
	CodeInvalidRequest         = "INVALID_REQUEST"
	CodeInternal               = "INTERNAL_ERROR"
)

func errorf(code, format string, args ...any) *excerrors.Error {
	return excerrors.New(code, fmt.Sprintf(format, args...))
}

// MaxConcurrentBots is the R13 cap on concurrently RUNNING grid bots
// per account (spec §10.2 grid-bot wizard note + §24 #275).
const MaxConcurrentBots = 5

// Grid bounds per Task 16.3.19: grid_count ∈ [5, 200].
const (
	MinGridCount = 5
	MaxGridCount = 200
)

// Grid modes (grid_mode_enum).
const (
	ModeArithmetic = "ARITHMETIC"
	ModeGeometric  = "GEOMETRIC"
)

// Bot lifecycle (grid_bot_status_enum).
const (
	StatusRunning   = "RUNNING"
	StatusCompleted = "COMPLETED" // take-profit excursion — clean exit
	StatusStopped   = "STOPPED"   // user stop / stop-loss excursion
	StatusFailed    = "FAILED"    // unrecoverable placement failure
)

// Child lifecycle (grid_child_status_enum).
const (
	ChildPending   = "PENDING"
	ChildWorking   = "WORKING"
	ChildFilled    = "FILLED"
	ChildCancelled = "CANCELLED"
	ChildRejected  = "REJECTED"
	ChildSkipped   = "SKIPPED"
)

// Stop reasons recorded on grid_bots.stop_reason.
const (
	StopReasonUser       = "USER_STOP"
	StopReasonTakeProfit = "TAKE_PROFIT"
	StopReasonStopLoss   = "STOP_LOSS"
	StopReasonFailed     = "FAILED"
)

// GridBot is one grid_bots row (parent record).
type GridBot struct {
	BotID           int64            `json:"bot_id"`
	AccountID       int64            `json:"account_id"`
	InstrumentID    int64            `json:"instrument_id"`
	Symbol          string           `json:"symbol"`
	LowerPrice      decimal.Decimal  `json:"lower_price"`
	UpperPrice      decimal.Decimal  `json:"upper_price"`
	GridCount       int              `json:"grid_count"`
	Mode            string           `json:"mode"`
	TotalInvestment decimal.Decimal  `json:"total_investment"`
	Leverage        decimal.Decimal  `json:"leverage"`
	TakeProfitPrice *decimal.Decimal `json:"take_profit_price,omitempty"`
	StopLossPrice   *decimal.Decimal `json:"stop_loss_price,omitempty"`
	Status          string           `json:"status"`
	RealizedPnL     decimal.Decimal  `json:"realized_pnl"`
	PnLCurrency     string           `json:"pnl_currency"`
	FillsCount      int              `json:"fills_count"`
	ReferencePrice  *decimal.Decimal `json:"reference_price,omitempty"`
	StoppedAt       *time.Time       `json:"stopped_at,omitempty"`
	StopReason      string           `json:"stop_reason,omitempty"`
	CreatedAt       time.Time        `json:"created_at"`
	UpdatedAt       time.Time        `json:"updated_at"`
}

// GridChild is one grid_bot_orders row — one placed (or intentionally
// skipped) child order at a grid level.
type GridChild struct {
	ID            int64            `json:"id"`
	BotID         int64            `json:"bot_id"`
	LevelIndex    int              `json:"level_index"`
	Side          string           `json:"side"`
	Price         decimal.Decimal  `json:"price"`
	Qty           decimal.Decimal  `json:"qty"`
	OrderID       *int64           `json:"order_id,omitempty"`
	ClientOrderID string           `json:"client_order_id,omitempty"`
	Status        string           `json:"status"`
	FilledQty     decimal.Decimal  `json:"filled_qty"`
	AvgFillPrice  *decimal.Decimal `json:"avg_fill_price,omitempty"`
	SourceChildID *int64           `json:"source_child_id,omitempty"`
	RealizedPnL   decimal.Decimal  `json:"realized_pnl"`
	Note          string           `json:"note,omitempty"`
	FilledAt      *time.Time       `json:"filled_at,omitempty"`
	CreatedAt     time.Time        `json:"created_at"`
	UpdatedAt     time.Time        `json:"updated_at"`
}

// CreateRequest is the POST /api/v1/bots/grid body (Task 16.3.19 shape).
type CreateRequest struct {
	Symbol          string `json:"symbol"`
	UpperPrice      string `json:"upper_price"`
	LowerPrice      string `json:"lower_price"`
	GridCount       int    `json:"grid_count"`
	Mode            string `json:"mode"`
	TotalInvestment string `json:"total_investment"`
	Leverage        string `json:"leverage,omitempty"`
	TakeProfitPrice string `json:"take_profit_price,omitempty"`
	StopLossPrice   string `json:"stop_loss_price,omitempty"`
}

// Detail is the GET /api/v1/bots/grid/{id} payload — the parent record
// plus full child accounting.
type Detail struct {
	Bot      GridBot     `json:"bot"`
	Children []GridChild `json:"children"`
}
