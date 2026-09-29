// Package strategies implements Phase-16 Task 16.3.21 — recurring FX
// conversion, target-allocation rebalancing and the approved strategy
// marketplace (spec §5.39 migration 077, §24 #296, ruling R14).
//
// Every execution routes as a firm CLOB order through orders.Service —
// principal/RFQ conversion is out of scope by ruling R14. Each run
// re-evaluates suitability (the appropriateness/product gates inside
// order admission), margin (checkBalance), spread (BBO vs instrument
// max_spread_pips) and market hours (instruments.SessionService) and
// records the truthful outcome on strategy_runs — market-closed slots
// are SKIPPED and rescheduled, never silently dropped or faked.
//
// Templates are configuration-only JSONB validated against a strict key
// allowlist; instantiation copies the config onto a new strategy row —
// executable code can never be published, copied or run.
package strategies

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"exchange/pkg/decimal"
	excerrors "exchange/pkg/errors"
)

// Canonical §23 codes emitted here (registered in internal/errs/codes.go).
const (
	CodeStrategyNotFound      = "STRATEGY_NOT_FOUND"
	CodeStrategyConfigInvalid = "STRATEGY_CONFIG_INVALID"
	CodeTemplateNotApproved   = "STRATEGY_TEMPLATE_NOT_APPROVED"
	CodeInvalidRequest        = "INVALID_REQUEST"
	CodeNotFound              = "NOT_FOUND"
	CodeInternal              = "INTERNAL_ERROR"
)

func errorf(code, format string, args ...any) *excerrors.Error {
	return excerrors.New(code, fmt.Sprintf(format, args...))
}

// Strategy kinds (strategy_kind_enum).
const (
	KindRecurringConversion = "RECURRING_CONVERSION"
	KindRebalance           = "REBALANCE"
)

// Lifecycle (strategy_status_enum).
const (
	StatusActive    = "ACTIVE"
	StatusPaused    = "PAUSED"
	StatusCancelled = "CANCELLED"
)

// Template lifecycle (strategy_template_status_enum).
const (
	TemplatePending  = "PENDING_APPROVAL"
	TemplateApproved = "APPROVED"
	TemplateRejected = "REJECTED"
	TemplateRetired  = "RETIRED"
)

// Run lifecycle (strategy_run_status_enum).
const (
	RunPending   = "PENDING"
	RunSubmitted = "SUBMITTED"
	RunCompleted = "COMPLETED"
	RunSkipped   = "SKIPPED"
	RunFailed    = "FAILED"
	RunCancelled = "CANCELLED"
)

// Schedules.
const (
	ScheduleDaily   = "DAILY"
	ScheduleWeekly  = "WEEKLY"
	ScheduleMonthly = "MONTHLY"
)

// Strategy is one strategies row.
type Strategy struct {
	StrategyID      int64                      `json:"strategy_id"`
	AccountID       int64                      `json:"account_id"`
	Kind            string                     `json:"kind"`
	Label           string                     `json:"label"`
	FromCurrency    string                     `json:"from_currency,omitempty"`
	ToCurrency      string                     `json:"to_currency,omitempty"`
	Amount          *decimal.Decimal           `json:"amount,omitempty"`
	Schedule        string                     `json:"schedule,omitempty"`
	Targets         map[string]decimal.Decimal `json:"targets,omitempty"`
	DriftBandPct    *decimal.Decimal           `json:"drift_band_pct,omitempty"`
	TemplateID      *int64                     `json:"template_id,omitempty"`
	Status          string                     `json:"status"`
	NextRunAt       *time.Time                 `json:"next_run_at,omitempty"`
	LastRunAt       *time.Time                 `json:"last_run_at,omitempty"`
	RealizedPnL     decimal.Decimal            `json:"realized_pnl"`
	TotalFees       decimal.Decimal            `json:"total_fees"`
	TotalSpreadCost decimal.Decimal            `json:"total_spread_cost"`
	TotalNotional   decimal.Decimal            `json:"total_notional"`
	HighWaterPnL    decimal.Decimal            `json:"high_water_pnl"`
	MaxDrawdown     decimal.Decimal            `json:"max_drawdown"`
	RunCount        int                        `json:"run_count"`
	CreatedAt       time.Time                  `json:"created_at"`
	UpdatedAt       time.Time                  `json:"updated_at"`
}

// Template is one strategy_templates row — configuration only.
type Template struct {
	TemplateID         int64           `json:"template_id"`
	Name               string          `json:"name"`
	Description        string          `json:"description"`
	Kind               string          `json:"kind"`
	Config             json.RawMessage `json:"config"`
	Status             string          `json:"status"`
	PublisherAccountID int64           `json:"publisher_account_id"`
	ApprovedBy         *int64          `json:"approved_by,omitempty"`
	ApprovedAt         *time.Time      `json:"approved_at,omitempty"`
	RejectedAt         *time.Time      `json:"rejected_at,omitempty"`
	RejectReason       string          `json:"reject_reason,omitempty"`
	CreatedAt          time.Time       `json:"created_at"`
	UpdatedAt          time.Time       `json:"updated_at"`
}

// RunLeg is one planned/executed leg inside a run (stored in
// strategy_runs.legs JSONB). Rebalance runs carry several legs.
type RunLeg struct {
	Currency  string `json:"currency"`
	Action    string `json:"action"` // CONVERT | SELL_EXCESS | BUY_DEFICIT
	Symbol    string `json:"symbol,omitempty"`
	Side      string `json:"side,omitempty"`
	Amount    string `json:"amount,omitempty"`     // intended spend (from-ccy)
	USDAmount string `json:"usd_amount,omitempty"` // rebalance legs
	OrderID   *int64 `json:"order_id,omitempty"`
	Status    string `json:"status"` // PLACED | SKIPPED | REJECTED | PENDING
	Note      string `json:"note,omitempty"`
}

// Run is one strategy_runs row.
type Run struct {
	RunID         int64            `json:"run_id"`
	StrategyID    int64            `json:"strategy_id"`
	AccountID     int64            `json:"account_id"`
	ScheduledFor  time.Time        `json:"scheduled_for"`
	Kind          string           `json:"kind"`
	Status        string           `json:"status"`
	SkipReason    string           `json:"skip_reason,omitempty"`
	OrderIDs      []int64          `json:"order_ids"`
	Legs          []RunLeg         `json:"legs"`
	Notional      decimal.Decimal  `json:"notional"`
	Currency      string           `json:"currency"`
	ExpectedValue *decimal.Decimal `json:"expected_value,omitempty"`
	ExecutedValue *decimal.Decimal `json:"executed_value,omitempty"`
	Fees          decimal.Decimal  `json:"fees"`
	SpreadCost    decimal.Decimal  `json:"spread_cost"`
	RealizedPnL   decimal.Decimal  `json:"realized_pnl"`
	Drawdown      decimal.Decimal  `json:"drawdown"`
	StartedAt     *time.Time       `json:"started_at,omitempty"`
	CompletedAt   *time.Time       `json:"completed_at,omitempty"`
	CreatedAt     time.Time        `json:"created_at"`
	UpdatedAt     time.Time        `json:"updated_at"`
}

// Detail is the strategy detail payload (record + recent runs).
type Detail struct {
	Strategy Strategy `json:"strategy"`
	Runs     []Run    `json:"runs"`
}

// CreateInput is the POST /api/v1/strategies body.
type CreateInput struct {
	Kind         string            `json:"kind"`
	Label        string            `json:"label,omitempty"`
	FromCurrency string            `json:"from_currency,omitempty"`
	ToCurrency   string            `json:"to_currency,omitempty"`
	Amount       string            `json:"amount,omitempty"`   // decimal string, from_ccy
	Schedule     string            `json:"schedule,omitempty"` // DAILY|WEEKLY|MONTHLY
	Targets      map[string]string `json:"targets,omitempty"`  // ccy → weight decimal string
	DriftBandPct string            `json:"drift_band_pct,omitempty"`
}

// TemplatePublishInput is the POST /api/v1/strategy-templates body.
type TemplatePublishInput struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Kind        string          `json:"kind"`
	Config      json.RawMessage `json:"config"`
}

// NextSlot advances a schedule instant one period (schedule arithmetic —
// MONTHLY is calendar-based, DAILY/WEEKLY are fixed durations).
func NextSlot(schedule string, from time.Time) time.Time {
	switch strings.ToUpper(schedule) {
	case ScheduleWeekly:
		return from.Add(7 * 24 * time.Hour)
	case ScheduleMonthly:
		return from.AddDate(0, 1, 0)
	default: // DAILY
		return from.Add(24 * time.Hour)
	}
}
