// EMIR REFIT trade reporting adapter (Task 21.3.5, spec §14.1a; §24
// #160). Derivative legs for forwards, swaps and NDFs report to a Trade
// Repository over ISO 20022 auth.030.001.05 — lifecycle events
// (new/modify/cancel/error plus the full continuation vocabulary),
// dual-sided reporting, and the Phase-19.5 forward-points oracle seam
// for valuation.
//
// The position/trade schema is the Phase-21 reality (full derivative-leg
// vocabulary lands Phase-22): a derivative trade reports NEWT keyed by
// its UTI, and lifecycle continuations (VALU/MARU/TERM/CORR/ERRO) chain
// off it — RecordLifecycle carries position_id once positions exist.
package compliance

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"exchange/internal/compliance/reporting"
	"exchange/internal/oracle/rates"
	excerrors "exchange/pkg/errors"
)

// ForwardPointsOracle is the Phase-19.5 swap-points seam —
// rates.Store.SwapPointFor satisfies it. Nil = valuation continuations
// carry mark fields without forward-point context (fail-open on the
// optional enrichment, fail-closed on the required fields).
type ForwardPointsOracle func(ctx context.Context, pair string) (rates.SwapPoint, error)

// EMIRReporter is the Task 21.3.5 façade over the canonical service.
type EMIRReporter struct {
	Svc    *reporting.Service
	Points ForwardPointsOracle
}

// NewEMIRReporter wires the adapter.
func NewEMIRReporter(svc *reporting.Service, points ForwardPointsOracle) (*EMIRReporter, error) {
	if svc == nil {
		return nil, fmt.Errorf("emir: nil reporting service")
	}
	return &EMIRReporter{Svc: svc, Points: points}, nil
}

// ReportDerivative records the NEWT event for a derivative execution
// (FORWARD/SWAP/NDF/OPTION). Spot trades are rejected — MiFID covers
// them. Dual-sided rides the venue's reporting flag (Config.DualSided).
func (r *EMIRReporter) ReportDerivative(ctx context.Context, tc *reporting.TradeContext) (*reporting.Event, error) {
	switch tc.InstrumentType {
	case "FORWARD", "SWAP", "NDF", "OPTION":
	default:
		return nil, excerrors.New("INVALID_REQUEST",
			fmt.Sprintf("emir: instrument_type %s is not a derivative", tc.InstrumentType))
	}
	events, err := r.Svc.RecordExecution(ctx, tc)
	if err != nil {
		return nil, excerrors.Wrap("REGULATORY_REPORT_RESUBMISSION",
			"emir record", err)
	}
	for _, e := range events {
		if e.Regime == reporting.RegimeEMIRREFIT {
			return e, nil
		}
	}
	return nil, excerrors.New("INTERNAL_ERROR",
		"emir event missing after record")
}

// EnrichNEWT is the reporting.Config.DerivativeEnrich hook — it supplies
// the EMIR REFIT NEWT economics the trade row doesn't carry:
//   - valuation: the outright-forward mark built from the Phase-19.5
//     forward-points oracle (spot price + signed T/N points, source and
//     as-of stamped for auditability).
//   - margin: the venue's collateralisation block — bilateral FX trades
//     on this venue carry no trade-level collateral at inception, so the
//     block states UNCOLLATERALISED with zero IM/VM (a factual margin
//     status, not a fabricated amount).
//
// An oracle error propagates — stale or missing forward points fail
// closed (the trades consumer NAKs for redelivery) rather than shipping
// an unpriced report. Pair-less instruments skip the points lookup and
// value at the execution mark.
func (r *EMIRReporter) EnrichNEWT(ctx context.Context, tc *reporting.TradeContext) (
	string, json.RawMessage, json.RawMessage, error) {

	pair := tc.InstrumentCode // venue code == the fwd_points key ("EUR/USD")
	val := map[string]any{
		"mark_price": tc.Price, "currency": tc.QuoteCurrency,
		"as_of": tc.ExecutedAt.UTC().Format(time.RFC3339Nano),
		"basis": "EXECUTION_MARK",
	}
	if r.Points != nil && pair != "" {
		sp, err := r.Points(ctx, pair)
		if err != nil {
			return "", nil, nil, excerrors.Wrap("PRICE_ORACLE_UNAVAILABLE",
				"emir: forward-points oracle", err)
		}
		val["basis"] = "FWD_POINTS_ORACLE"
		val["fwd_points_long"] = sp.Long.String()
		val["fwd_points_short"] = sp.Short.String()
		val["fwd_points_source"] = sp.Source
		val["fwd_points_as_of"] = sp.AsOf.UTC().Format(time.RFC3339Nano)
	}
	valJSON, err := json.Marshal(val)
	if err != nil {
		return "", nil, nil, err
	}
	mrgJSON, err := json.Marshal(map[string]any{
		"collateralisation": "UNCOLLATERALISED",
		"initial_margin":    "0", "variation_margin": "0",
		"currency": tc.QuoteCurrency,
		"as_of":    tc.ExecutedAt.UTC().Format(time.RFC3339Nano),
	})
	if err != nil {
		return "", nil, nil, err
	}
	return "", valJSON, mrgJSON, nil
}

// Lifecycle continuation helpers — each appends seq+1 to the UTI chain.

// Modify posts a MODI continuation (economic/terms change).
func (r *EMIRReporter) Modify(ctx context.Context, uti string, in reporting.LifecycleInput) (*reporting.Event, error) {
	in.UTI, in.Regime = uti, reporting.RegimeEMIRREFIT
	in.Action, in.EventType = reporting.ActionModify, reporting.EventTypeModify
	return r.Svc.RecordLifecycle(ctx, in)
}

// Cancel posts a TERM continuation (cancellation/early termination).
func (r *EMIRReporter) Cancel(ctx context.Context, uti string, at time.Time) (*reporting.Event, error) {
	return r.Svc.RecordLifecycle(ctx, reporting.LifecycleInput{
		UTI: uti, Regime: reporting.RegimeEMIRREFIT,
		Action: reporting.ActionTerminate, EventType: reporting.EventTypeTermination,
		EventTS: at,
	})
}

// ReportError posts an ERRO continuation — the error/omission workflow.
func (r *EMIRReporter) ReportError(ctx context.Context, uti string, at time.Time) (*reporting.Event, error) {
	return r.Svc.RecordLifecycle(ctx, reporting.LifecycleInput{
		UTI: uti, Regime: reporting.RegimeEMIRREFIT,
		Action: reporting.ActionError, EventType: reporting.EventTypeError,
		EventTS: at,
	})
}

// Valuation posts a VALU mark-to-market continuation, enriched with the
// venue's forward-points oracle quote when available (Task 21.3.5 —
// "use the Phase-19.5 forward-points oracle seam").
func (r *EMIRReporter) Valuation(ctx context.Context, uti, pair string,
	markPrice, notional, ccy string, positionID int64, at time.Time) (*reporting.Event, error) {

	val := map[string]any{
		"mark_price": markPrice, "notional": notional, "currency": ccy,
		"as_of": at.UTC().Format(time.RFC3339Nano), "basis": "MTM",
	}
	if r.Points != nil && pair != "" {
		if sp, err := r.Points(ctx, pair); err == nil {
			val["fwd_points_long"] = sp.Long.String()
			val["fwd_points_short"] = sp.Short.String()
			val["fwd_points_source"] = sp.Source
			val["fwd_points_as_of"] = sp.AsOf.UTC().Format(time.RFC3339Nano)
		}
		// stale/missing points: valuation still carries mark fields —
		// the required-fields validator decides sufficiency.
	}
	raw, err := json.Marshal(val)
	if err != nil {
		return nil, err
	}
	return r.Svc.RecordLifecycle(ctx, reporting.LifecycleInput{
		UTI: uti, Regime: reporting.RegimeEMIRREFIT,
		Action: reporting.ActionValuation, EventType: reporting.EventTypeValuation,
		EventTS: at, PositionID: positionID, Valuation: raw, Notional: notional,
	})
}

// Margin posts a MARU continuation (IM/VM/collateral move).
func (r *EMIRReporter) Margin(ctx context.Context, uti string,
	im, vm, ccy string, positionID int64, at time.Time) (*reporting.Event, error) {
	raw, err := json.Marshal(map[string]any{
		"initial_margin": im, "variation_margin": vm,
		"currency": ccy, "as_of": at.UTC().Format(time.RFC3339Nano),
	})
	if err != nil {
		return nil, err
	}
	return r.Svc.RecordLifecycle(ctx, reporting.LifecycleInput{
		UTI: uti, Regime: reporting.RegimeEMIRREFIT,
		Action: reporting.ActionMargin, EventType: reporting.EventTypeMargin,
		EventTS: at, PositionID: positionID, Margin: raw,
	})
}

// Correct posts a CORR continuation after a repository NACK — the
// supersedes chain pins the prior report (immutable history).
func (r *EMIRReporter) Correct(ctx context.Context, uti string, in reporting.LifecycleInput) (*reporting.Event, error) {
	in.UTI, in.Regime = uti, reporting.RegimeEMIRREFIT
	in.Action, in.EventType = reporting.ActionCorrect, reporting.EventTypeCorrection
	return r.Svc.RecordLifecycle(ctx, in)
}

// Reconcile runs the daily repo-vs-internal reconciliation (EMIR side).
func (r *EMIRReporter) Reconcile(ctx context.Context) (*reporting.ReconcileReport, error) {
	return r.Svc.Reconcile(ctx, reporting.RegimeEMIRREFIT)
}
