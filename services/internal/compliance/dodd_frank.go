// Dodd-Frank swap reporting adapter (Task 21.3.9, spec §14.1a; §24
// #161). CFTC Part 43 real-time public dissemination + Part 45
// regulatory swap-data reporting over the canonical event store —
// UTI/USI generation, prior-ID links, SDR submission seam, position
// limits and large-trader reports.
package compliance

import (
	"context"
	"fmt"

	"github.com/shopspring/decimal"

	"exchange/internal/compliance/reporting"
	excerrors "exchange/pkg/errors"
)

// DoddFrankReporter is the Task 21.3.9 façade over the canonical
// reporting service.
type DoddFrankReporter struct {
	Svc *reporting.Service
}

// NewDoddFrankReporter wires the adapter; nil service fails closed.
func NewDoddFrankReporter(svc *reporting.Service) (*DoddFrankReporter, error) {
	if svc == nil {
		return nil, fmt.Errorf("dodd_frank: nil reporting service")
	}
	return &DoddFrankReporter{Svc: svc}, nil
}

// ReportSwap records the CFTC Parts 43/45 events for a US-nexus
// derivative execution. Returns the two events (P43 real-time + P45
// lifecycle); a trade without US nexus returns nil events (EMIR covers
// it — the US-nexus gate lives in Service.RegimesFor).
func (r *DoddFrankReporter) ReportSwap(ctx context.Context, tc *reporting.TradeContext) (p43, p45 *reporting.Event, err error) {
	events, err := r.Svc.RecordExecution(ctx, tc)
	if err != nil {
		return nil, nil, excerrors.Wrap("REGULATORY_REPORT_RESUBMISSION",
			"cftc record", err)
	}
	for _, e := range events {
		switch e.Regime {
		case reporting.RegimeCFTCP43:
			p43 = e
		case reporting.RegimeCFTCP45:
			p45 = e
		}
	}
	return p43, p45, nil
}

// ReportLifecycle appends a continuation to both CFTC regimes —
// creation (NEWT is the consumer path) / continuation lifecycle events
// per Parts 43/45 with prior-ID links already on the chain.
func (r *DoddFrankReporter) ReportLifecycle(ctx context.Context, uti string,
	in reporting.LifecycleInput) (p43, p45 *reporting.Event, err error) {
	in.UTI = uti
	in.Regime = reporting.RegimeCFTCP45
	p45, err = r.Svc.RecordLifecycle(ctx, in)
	if err != nil {
		return nil, nil, err
	}
	// Part 43 disseminates only TRADE + termination events publicly —
	// internal lifecycle updates are P45-only (CFTC §43.4 masking rules
	// keep valuation/margin non-public).
	switch in.Action {
	case reporting.ActionTerminate, reporting.ActionError:
		in.Regime = reporting.RegimeCFTCP43
		p43, err = r.Svc.RecordLifecycle(ctx, in)
	}
	return p43, p45, err
}

// CheckLimits evaluates the account's open quantity against the
// in-force CFTC position limit / large-trader threshold and posts an
// LTR (large-trader) event when crossed — spec §14.11 enforcement.
func (r *DoddFrankReporter) CheckLimits(ctx context.Context, accountID, instrumentID int64,
	instrumentType, pair string) (*reporting.Event, error) {

	lim, err := r.Svc.Store.ActiveLimitFor(ctx, instrumentID, instrumentType, pair, r.Svc.Now())
	if err != nil {
		return nil, err
	}
	if lim == nil {
		return nil, nil // no in-force limit — nothing to enforce
	}
	qtyText, err := r.Svc.Store.OpenQuantityFor(ctx, accountID, instrumentID)
	if err != nil {
		return nil, err
	}
	qty, err := decimal.NewFromString(qtyText)
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "open qty parse", err)
	}
	thr, err := decimal.NewFromString(lim.LargeTraderThreshold)
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "large-trader threshold", err)
	}
	if qty.Abs().LessThanOrEqual(thr.Abs()) {
		return nil, nil
	}
	// Large-trader threshold crossed — report on the position UTI
	// (scope POSITION distinct from TRADE: position-level reporting).
	uti := reporting.UTIFor(r.Svc.Cfg.VenueLEI, "POS", instrumentID)
	return r.Svc.RecordLifecycle(ctx, reporting.LifecycleInput{
		UTI: uti, Regime: reporting.RegimeCFTCP45,
		Action: reporting.ActionLargeTrader, EventType: reporting.EventTypeReport,
		EventTS: r.Svc.Now(), InstrumentID: instrumentID,
		AccountID: accountID,
		Quantity:  qtyText,
	})
}

// Reconcile runs the daily repo-vs-internal reconciliation (P45 side —
// Part 45 holds the regulatory open-interest record).
func (r *DoddFrankReporter) Reconcile(ctx context.Context) (*reporting.ReconcileReport, error) {
	return r.Svc.Reconcile(ctx, reporting.RegimeCFTCP45)
}
