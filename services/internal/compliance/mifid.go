// MiFID II RTS 22 transaction reporting adapter (Task 21.3.4, spec
// §14.5; §24 acceptance #159).
//
// The heavy lifting lives in compliance/reporting (Task 21.3.14): this
// adapter owns the RTS 22 task surface — venue instrument code (NOT
// ISIN), buyer/seller/decision-maker decomposition, the T+1 23:59 CET
// deadline, the ARM submission seam, and rejection/repair resubmission.
package compliance

import (
	"context"
	"fmt"
	"time"

	"exchange/internal/compliance/reporting"
	excerrors "exchange/pkg/errors"
)

// MiFIDReporter is the Task 21.3.4 façade over the canonical reporting
// service. Constructed with the shared *reporting.Service — the same
// instance the consumer/dispatcher use, so every path writes through
// one store.
type MiFIDReporter struct {
	Svc *reporting.Service
}

// NewMiFIDReporter wires the adapter; nil service fails closed.
func NewMiFIDReporter(svc *reporting.Service) (*MiFIDReporter, error) {
	if svc == nil {
		return nil, fmt.Errorf("mifid: nil reporting service")
	}
	return &MiFIDReporter{Svc: svc}, nil
}

// ReportExecution records the RTS 22 event for one executed trade —
// the trades consumer's entry point and the direct-call seam for
// backfill. Returns the MIFID2 event (QUARANTINED rows included — the
// caller inspects Status + ValidationErrors).
func (r *MiFIDReporter) ReportExecution(ctx context.Context, tc *reporting.TradeContext) (*reporting.Event, error) {
	events, err := r.Svc.RecordExecution(ctx, tc)
	if err != nil {
		return nil, excerrors.Wrap("REGULATORY_REPORT_RESUBMISSION",
			"mifid rts22 record", err)
	}
	for _, e := range events {
		if e.Regime == reporting.RegimeMIFID2 {
			return e, nil
		}
	}
	return nil, excerrors.New("INTERNAL_ERROR",
		"mifid rts22 event missing after record")
}

// RTS22Deadline is the T+1 23:59 CET transmission deadline for a trade
// executed at ts — dissemination_due_at on the event and the SLA the
// repair queue enforces (spec §14.5, §14.9).
func (r *MiFIDReporter) RTS22Deadline(executedAt time.Time) time.Time {
	return reporting.RTS22Deadline(executedAt)
}

// RepairQueue lists open repair items for the regime — the Compliance
// Officer queue (rejected NACKs, validation quarantines, breaks).
func (r *MiFIDReporter) RepairQueue(ctx context.Context, limit int) ([]reporting.Break, error) {
	return r.Svc.Store.OpenBreaks(ctx, reporting.RegimeMIFID2, limit)
}

// Resubmit applies Compliance Officer corrections and re-dispatches the
// corrected artifact (immutable attempt history per spec §14.1a).
func (r *MiFIDReporter) Resubmit(ctx context.Context, reportSubmissionID int64,
	corrections map[string]any, adminUserID int64) (*reporting.Submission, error) {
	return r.Svc.Resubmit(ctx, reportSubmissionID, corrections, adminUserID)
}

// IngestAsyncAck lands a vendor's asynchronous NACK/feedback message for
// a previously SUBMITTED artifact — the "asynchronous NACK" leg of Task
// 21.3.16. matByRef resolves the artifact by the repository's external
// ref when the vendor doesn't echo our submission id.
func (r *MiFIDReporter) IngestAsyncAck(ctx context.Context, a reporting.Ack) (*reporting.Break, error) {
	return r.Svc.IngestAck(ctx, a)
}
