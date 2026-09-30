// Phase-19 Task 19.3.5 — per-account and per-instrument notional
// exposure caps (spec §13.6, §24 #97/#139).
//
// The enforcement surface is intentionally thin: risk_limits already
// carries the three cap columns (max_notional_exposure /
// max_short_exposure / max_account_notional, migration 109) with the
// §13.6 defaults ($10M symbol / $5M short / $50M account), and
// LimitsService.CheckOrder already enforces them. This file adds the
// two Phase-19 refinements:
//
//  1. Position-mode awareness (Task 19.3.15): under NETTING an
//     opposing order first consumes the open row — its exposure
//     increment is only the RESIDUAL that opens new net exposure
//     (a flip of qty Q against a short of notional S adds
//     max(0, Q×price − S), not Q×price). Under HEDGING every fill adds
//     gross (and SELL adds short) — hedge legs coexist by definition.
//
//  2. The orders.Options.ExposureGate seam (ExposureChecker): the
//     admission pipeline consults it for exposure-increasing orders
//     and marks req.ExposureChecked so LimitsService.CheckOrder does
//     not double-evaluate; CheckOrder's own block stays the
//     enforcement for callers that bypass the gate, upgraded by
//     WithPositionModes to the same mode-aware math.
//
// Fail-closed (spec §2.7): a missing position_mode reads as HEDGING —
// the conservative posture where every fill adds gross — and a store
// error rejects with RISK_LIMITS_INTERNAL. Breaches emit the canonical
// MAX_EXPOSURE_EXCEEDED (spec §23, HTTP 400/L2).
package risk

import (
	"context"
	"fmt"

	"exchange/pkg/decimal"
	excerrors "exchange/pkg/errors"
)

// PositionModeSource resolves accounts.position_mode (migration 233).
// The fill path owns the row; this seam only reads it.
type PositionModeSource interface {
	PositionMode(ctx context.Context, accountID int64) (string, error)
}

// PositionModeFunc adapts a function to PositionModeSource.
type PositionModeFunc func(ctx context.Context, accountID int64) (string, error)

// PositionMode implements PositionModeSource.
func (f PositionModeFunc) PositionMode(ctx context.Context, accountID int64) (string, error) {
	return f(ctx, accountID)
}

// exposureIncrements computes the gross/short notional the order ADDS
// given the account's live exposure on the symbol and its position
// mode. (long, short) are the account's current per-side notionals on
// this symbol (gross = long + short by construction).
func exposureIncrements(mode, side string, notional, long, short decimal.Decimal) (gross, shortIncr decimal.Decimal) {
	conservative := mode != ModeNetting // HEDGING or unreadable → full add
	if conservative {
		if side == "SELL" {
			return notional, notional
		}
		return notional, decimal.Zero
	}
	// NETTING: the fill first consumes the opposing side; only the
	// residual opens new net exposure.
	if side == "SELL" {
		residual := notional.Sub(long)
		if !residual.IsPositive() {
			return decimal.Zero, decimal.Zero // pure reduction
		}
		return residual, residual // residual is new SHORT exposure
	}
	residual := notional.Sub(short)
	if !residual.IsPositive() {
		return decimal.Zero, decimal.Zero
	}
	return residual, decimal.Zero // residual is new LONG exposure
}

// ExposureChecker is the orders.Options.ExposureGate contract (Task
// 19.3.5): the admission pipeline calls CheckExposure for every
// non-reduce-only order. A nil verdict passes; a coded error rejects.
type ExposureChecker interface {
	CheckExposure(ctx context.Context, req OrderRequest) error
}

// ExposureSource supplies the live per-symbol exposures the gate needs —
// the same projection risk.Store.SymbolExposures serves; declared here
// so the gate can bind a narrower seam (and tests fake it trivially).
type ExposureSource interface {
	SymbolExposures(ctx context.Context, accountID int64) ([]SymbolExposure, error)
}

// LimitsResolver resolves the EffectiveLimits for (account, tier,
// symbol) — *LimitsService satisfies it.
type LimitsResolver interface {
	EffectiveLimits(accountID int64, tier, symbol string) EffectiveLimits
}

// ExposureService is the production ExposureChecker: mode-aware
// exposure-cap enforcement over the risk_limits caps.
type ExposureService struct {
	limits LimitsResolver
	exps   ExposureSource
	modes  PositionModeSource // nil → conservative full-add
}

// NewExposureService wires the gate.
func NewExposureService(limits LimitsResolver, exps ExposureSource, modes PositionModeSource) *ExposureService {
	return &ExposureService{limits: limits, exps: exps, modes: modes}
}

// CheckExposure implements ExposureChecker. reduce_only orders are
// never consulted by the caller, but the check itself is also safe for
// them (a reduction can only decrease the computed increments).
func (s *ExposureService) CheckExposure(ctx context.Context, req OrderRequest) error {
	if s.limits == nil || s.exps == nil {
		return internalError("exposure gate unbound", fmt.Errorf("limits/exposure source nil"))
	}
	if req.ReduceOnly {
		return nil // closing flow can only shrink exposure
	}
	if !req.Quantity.IsPositive() {
		return excerrors.New(CodeOrderRejected, "order quantity must be positive")
	}
	if !req.Price.IsPositive() {
		return excerrors.New(CodeOrderRejected,
			"price required for exposure notional check")
	}
	lim := s.limits.EffectiveLimits(req.AccountID, req.KycTier, req.Symbol)

	mode := ModeHedging // conservative default — full add
	if s.modes != nil {
		if m, err := s.modes.PositionMode(ctx, req.AccountID); err == nil && m == ModeNetting {
			mode = ModeNetting
		}
	}

	exps, err := s.exps.SymbolExposures(ctx, req.AccountID)
	if err != nil {
		return internalError("symbol exposures", err)
	}
	var sym SymbolExposure
	var accountGross decimal.Decimal
	for _, e := range exps {
		accountGross = accountGross.Add(e.GrossNotional)
		if e.Symbol == req.Symbol {
			sym = e
		}
	}
	long := sym.GrossNotional.Sub(sym.ShortNotional)
	notional := req.Quantity.Mul(req.Price)
	grossIncr, shortIncr := exposureIncrements(mode, req.Side, notional, long, sym.ShortNotional)

	if lim.MaxShortExposure != nil && shortIncr.IsPositive() {
		if sym.ShortNotional.Add(shortIncr).GreaterThan(*lim.MaxShortExposure) {
			return excerrors.New(CodeMaxExposureExceeded,
				fmt.Sprintf("short exposure %s + %s exceeds max_short_exposure %s on %s",
					sym.ShortNotional, shortIncr, *lim.MaxShortExposure, req.Symbol))
		}
	}
	if lim.MaxNotionalExposure != nil && grossIncr.IsPositive() {
		if sym.GrossNotional.Add(grossIncr).GreaterThan(*lim.MaxNotionalExposure) {
			return excerrors.New(CodeMaxExposureExceeded,
				fmt.Sprintf("symbol exposure %s + %s exceeds max_notional_exposure %s on %s",
					sym.GrossNotional, grossIncr, *lim.MaxNotionalExposure, req.Symbol))
		}
	}
	if lim.MaxAccountNotional != nil && grossIncr.IsPositive() {
		if accountGross.Add(grossIncr).GreaterThan(*lim.MaxAccountNotional) {
			return excerrors.New(CodeMaxExposureExceeded,
				fmt.Sprintf("account exposure %s + %s exceeds max_account_notional %s",
					accountGross, grossIncr, *lim.MaxAccountNotional))
		}
	}
	return nil
}
