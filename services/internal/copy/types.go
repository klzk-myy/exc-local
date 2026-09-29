// Package copy implements the Phase-14 Task 14.3.14 copy-trading product
// layer on top of the Task 14.3.8 PAMM pro-rata engine
// (services/internal/pamm):
//
//   - strategy_profiles: manager-facing records gated INCUBATING → LISTED
//     (≥30 days incubating + appropriateness PASS on the strategy's
//     instrument class) → SUSPENDED (compliance misconduct action — never
//     deleted while live followers exist);
//   - discovery: GET /api/v1/copy/strategies serves COMPUTED-ONLY stats —
//     realized P&L and drawdown derived from persisted fills, follower
//     count and AUM from live follow rows. Self-reported performance is
//     never stored or served; uncomputable stats exclude the strategy
//     (fail closed);
//   - copy_follows: investor binding with allocation notional, safety_mode
//     FULL|HALF_RISK (×0.5 child-quantity scaling applied BEFORE the
//     min-notional check — sub-threshold children are recorded
//     SKIPPED_MIN_NOTIONAL with a durable notice, never silently
//     dropped), and an investor stop-loss cap;
//   - high_water_marks + profit_share_accruals: month-end/unfollow accrual
//     of manager profit share on computed P&L above the ratchet-only HWM,
//     posted as a balanced GL journal (investor P&L liability → manager
//     revenue) through the §5.3 ledger service. PAMM_FEE_PERF /
//     PAMM_FEE_MGMT sub-ledger rows record the dedicated taxonomy.
//
// Event-source decision (documented per the task's "cheapest seam"
// requirement): master fills arrive via the NATS trades feed the
// settlements bridge already republishes from Aeron — the same feed the
// settlement balance consumer trusts. The copy engine consumes resolved
// fill structs; a second direct engine subscription would duplicate the
// Aeron surface without adding determinism.
//
// AUM read source: the follower AUM is computed as the Σ of ACTIVE
// follows' allocation_notional in the strategy currency — a durable,
// deterministic in-repo figure. The Phase-20/23 analytics AUM read is a
// documented future seam; strategies whose currency conversion cannot be
// resolved fail out of discovery rather than fabricate a figure.
package copy

import (
	"fmt"
	"time"

	"exchange/pkg/decimal"
	excerrors "exchange/pkg/errors"
)

// Strategy status lattice (migration 097 copy_strategy_status_enum).
type StrategyStatus string

const (
	StatusIncubating StrategyStatus = "INCUBATING"
	StatusListed     StrategyStatus = "LISTED"
	StatusSuspended  StrategyStatus = "SUSPENDED"
)

// Safety modes (copy_safety_mode_enum).
type SafetyMode string

const (
	ModeFull     SafetyMode = "FULL"
	ModeHalfRisk SafetyMode = "HALF_RISK"
)

// halfRiskScale is the fixed-point HALF_RISK multiplier — applied to the
// child quantity BEFORE the min-notional check (spec §12.9).
var halfRiskScale = decimal.RequireFromString("0.5")

// Follow status (copy_follow_status_enum).
type FollowStatus string

const (
	FollowActive     FollowStatus = "ACTIVE"
	FollowUnfollowed FollowStatus = "UNFOLLOWED"
	FollowStopped    FollowStatus = "STOPPED" // stop-loss cap breached
)

// Child order status (copy_child_status_enum).
type ChildStatus string

const (
	ChildPending            ChildStatus = "PENDING"
	ChildSubmitted          ChildStatus = "SUBMITTED"
	ChildSkippedMinNotional ChildStatus = "SKIPPED_MIN_NOTIONAL"
	ChildCancelled          ChildStatus = "CANCELLED"
	ChildRejected           ChildStatus = "REJECTED"
)

// Accrual status (profit_share_status_enum).
type AccrualStatus string

const (
	AccrualAccrued AccrualStatus = "ACCRUED"
	AccrualPaid    AccrualStatus = "PAID"
	AccrualVoid    AccrualStatus = "VOID"
)

// ProfitShareCeiling is the hard admin cap on manager profit share —
// mirrored by the CHECK constraint on strategy_profiles.profit_share_pct.
const ProfitShareCeiling = "50"

// IncubationDays is the minimum INCUBATING track record before LISTED.
const IncubationDays = 30

// Canonical error codes — this layer emits these verbatim.
const (
	CodeInvalidRequest       = "INVALID_REQUEST"
	CodeNotFound             = "NOT_FOUND"
	CodeForbidden            = "FORBIDDEN"
	CodeInsufficientBalance  = "INSUFFICIENT_BALANCE"
	CodeServiceDegraded      = "SERVICE_DEGRADED"
	CodeIdempotencyCollision = "IDEMPOTENCY_KEY_COLLISION"
	CodeProductNotPermitted  = "PRODUCT_NOT_PERMITTED"
	CodeInternalError        = "INTERNAL_ERROR"
)

func errorf(code, format string, args ...any) *excerrors.Error {
	return excerrors.New(code, fmt.Sprintf(format, args...))
}

// Strategy mirrors one strategy_profiles row.
type Strategy struct {
	StrategyID       int64           `json:"strategy_id"`
	ManagerAccountID int64           `json:"manager_account_id"`
	DisplayName      string          `json:"display_name"`
	Description      string          `json:"description"`
	Currency         string          `json:"currency"`
	InstrumentClass  string          `json:"instrument_class"`
	Status           StrategyStatus  `json:"status"`
	ProfitSharePct   decimal.Decimal `json:"profit_share_pct"`
	IncubatingSince  time.Time       `json:"incubating_since"`
	ListedAt         *time.Time      `json:"listed_at,omitempty"`
	SuspendedAt      *time.Time      `json:"suspended_at,omitempty"`
	SuspendReason    string          `json:"suspend_reason,omitempty"`
	CreatedAt        time.Time       `json:"created_at"`
}

// Follow mirrors one copy_follows row.
type Follow struct {
	FollowID           int64            `json:"follow_id"`
	InvestorAccountID  int64            `json:"investor_account_id"`
	StrategyID         int64            `json:"strategy_id"`
	AllocationNotional decimal.Decimal  `json:"allocation_notional"`
	Currency           string           `json:"currency"`
	SafetyMode         SafetyMode       `json:"safety_mode"`
	StopLossCap        *decimal.Decimal `json:"stop_loss_cap,omitempty"`
	Status             FollowStatus     `json:"status"`
	UnfollowedAt       *time.Time       `json:"unfollowed_at,omitempty"`
	CreatedAt          time.Time        `json:"created_at"`
}

// ChildOrder mirrors one copy_child_orders row.
type ChildOrder struct {
	ChildID       int64           `json:"child_id"`
	FollowID      int64           `json:"follow_id"`
	MasterTradeID int64           `json:"master_trade_id"`
	InstrumentID  int64           `json:"instrument_id"`
	Side          string          `json:"side"`
	Quantity      decimal.Decimal `json:"quantity"`
	MasterPrice   decimal.Decimal `json:"master_price"`
	Status        ChildStatus     `json:"status"`
	ChildOrderID  *int64          `json:"child_order_id,omitempty"`
	Notice        string          `json:"notice,omitempty"`
	CreatedAt     time.Time       `json:"created_at"`
}

// HighWaterMark mirrors one high_water_marks row — the ratchet-only
// profit-share baseline.
type HighWaterMark struct {
	FollowID      int64           `json:"follow_id"`
	WatermarkPnL  decimal.Decimal `json:"watermark_pnl"`
	LastSettledAt *time.Time      `json:"last_settled_at,omitempty"`
}

// Accrual mirrors one profit_share_accruals row.
type Accrual struct {
	AccrualID       int64           `json:"accrual_id"`
	FollowID        int64           `json:"follow_id"`
	StrategyID      int64           `json:"strategy_id"`
	PeriodStart     time.Time       `json:"period_start"`
	PeriodEnd       time.Time       `json:"period_end"`
	PnL             decimal.Decimal `json:"pnl"`
	Currency        string          `json:"currency"`
	WatermarkBefore decimal.Decimal `json:"watermark_before"`
	WatermarkAfter  decimal.Decimal `json:"watermark_after"`
	AccruedAmount   decimal.Decimal `json:"accrued_amount"`
	JournalEntryID  *int64          `json:"journal_entry_id,omitempty"`
	Status          AccrualStatus   `json:"status"`
}

// StrategyStats is the computed-only discovery payload — every figure is
// derived server-side from persisted fills/follows; nothing is
// self-reported. Computed=false rows never leave this package.
type StrategyStats struct {
	StrategyID      int64           `json:"strategy_id"`
	ReturnPct       decimal.Decimal `json:"return_pct"`     // realized P&L / notional traded
	MaxDrawdown     decimal.Decimal `json:"max_drawdown"`   // peak→trough of the P&L curve
	FollowerCount   int64           `json:"follower_count"` // ACTIVE follows
	AUM             decimal.Decimal `json:"aum"`            // Σ ACTIVE follow notional
	Currency        string          `json:"currency"`       // denomination of all money fields
	IncubatingSince time.Time       `json:"incubating_since"`
}
