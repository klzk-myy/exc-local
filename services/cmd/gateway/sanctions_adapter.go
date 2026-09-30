// Phase-21 Task 21.3.23 — wiring adapters for the sanctions/screening
// cluster.
//
// repairResubmitter binds compliance.Resubmitter to the SAME vendor
// client map the SubmissionDispatcher drives (Task 21.3.16): a claimed
// repair row is rebuilt into a reporting.Submission and POSTed through
// the destination's vendor client, so a retransmission is a real wire
// attempt — not a ledger-only replay.
//
// Verdict mapping: synchronous ACK → the vendor receipt ref resolves
// the repair row; synchronous NACK → non-nil error keeps the row
// retryable (bounded backoff, 2h deadline — an identical re-send of a
// still-invalid artifact must retry-then-escalate, not "succeed");
// async accept (nil verdict status) → delivered-for-processing, the
// real verdict lands on the report submission through the normal ack
// callback seam; transport error → error (retryable until deadline).
package main

import (
	"context"
	"fmt"

	"exchange/internal/compliance"
	regreport "exchange/internal/compliance/reporting"
)

// repairResubmitter retransmits one claimed repair row through the
// destination's bound vendor client.
type repairResubmitter struct {
	clients map[regreport.Destination]compliance.VendorClient
}

// Submit implements compliance.Resubmitter.
func (r repairResubmitter) Submit(ctx context.Context,
	s compliance.RepairSubmission) (string, error) {
	dest := regreport.Destination(s.DestinationType)
	client, ok := r.clients[dest]
	if !ok || client == nil {
		return "", fmt.Errorf("no vendor client for %s", s.DestinationType)
	}
	var subID, eventID int64
	if s.ReportSubmissionID != nil {
		subID = *s.ReportSubmissionID
	}
	if s.ReportEventID != nil {
		eventID = *s.ReportEventID
	}
	verdict, err := client.Submit(ctx, &regreport.Submission{
		ReportSubmissionID: subID,
		EventID:            eventID,
		Destination:        dest,
		Attempt:            s.Attempt,
		SchemaName:         s.SchemaName,
		SchemaVersion:      s.SchemaVersion,
		Payload:            s.Payload,
		PayloadXML:         s.PayloadXML,
		PayloadHash:        s.PayloadHash,
	})
	if err != nil {
		return "", err
	}
	if verdict == nil {
		return "", fmt.Errorf("vendor returned no verdict")
	}
	if verdict.Status != nil && *verdict.Status == regreport.AckReject {
		return "", fmt.Errorf("vendor NACK %s: %s",
			verdict.Code, verdict.Text)
	}
	// ACK accept or async-accept (nil status) — the wire attempt landed.
	return verdict.Ref, nil
}
