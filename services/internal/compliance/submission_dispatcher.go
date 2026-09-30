// SubmissionDispatcher — the outbound pump tying Task 21.3.14 artifacts
// to Task 21.3.16 vendor clients (spec §14.5/§14.9):
//
//	regulatory_report_submissions (PENDING, event VALIDATED)
//	  → VendorClient.Submit → wire log + transport row (059)
//	  → verdict: ACK/NACK → Service.IngestAck (event + break)
//	  → async 202 → SUBMITTED awaiting callback
//	  → transport error → FAILED + bounded backoff (≤2h, store-and-
//	    forward) — never silently drop a regulatory report.
//
// Fail closed: a missing vendor client for a destination ledger-marks
// the attempt ENDPOINT_UNCONFIGURED + raises a P1 alert instead of
// reporting success.
package compliance

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"exchange/internal/compliance/reporting"
)

// TransportLedger is the migration-059 ledger seam the dispatcher
// consumes — *SubmissionsLedger implements it; tests fake it.
type TransportLedger interface {
	Insert(ctx context.Context, r *TransportRow) error
	LatestFor(ctx context.Context, reportSubmissionID int64) (*TransportRow, error)
	AppendResponse(ctx context.Context, transportID int64, httpStatus int,
		body []byte, endpoint string) error
	SetVerdict(ctx context.Context, id int64, ackStatus, code, detail,
		ref string, at time.Time) error
	FailAttempt(ctx context.Context, id int64, code, detail string,
		nextAttemptAt, at time.Time) error
}

// SubmissionDispatcher drives pending artifacts to vendor endpoints.
type SubmissionDispatcher struct {
	Svc     *reporting.Service
	Ledger  TransportLedger
	Clients map[reporting.Destination]VendorClient
	Now     func() time.Time
	Alert   reporting.AlertFunc
}

func (d *SubmissionDispatcher) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now().UTC()
}

func (d *SubmissionDispatcher) alert(ctx context.Context, sev, code, summary string) {
	if d.Alert != nil {
		_ = d.Alert(ctx, sev, code, summary)
	}
}

// backoff — 1m, 2m, 4m … capped at 2h (§14.9 item 3 store-and-forward).
func backoff(attempt int) time.Duration {
	d := time.Duration(1<<min(attempt, 7)) * time.Minute
	if d > 2*time.Hour {
		return 2 * time.Hour
	}
	return d
}

// regulationLabel maps (regime, destination) to the ledger's
// regulation_type vocabulary (migration 059).
func regulationLabel(regime reporting.Regime, dest reporting.Destination) string {
	if regime == reporting.RegimeMIFID2 {
		if dest == reporting.DestinationAPA {
			return "MIFID2_RTS1"
		}
		return "MIFID2_RTS22"
	}
	return string(regime)
}

// DispatchOnce sweeps up to limit pending artifacts. Returns the count
// dispatched (verdict or async-accepted); transport failures park rows
// for retry rather than erroring the whole sweep — one dead endpoint
// never starves the others.
func (d *SubmissionDispatcher) DispatchOnce(ctx context.Context, limit int) (int, error) {
	pending, err := d.Svc.Store.PendingSubmissions(ctx, limit)
	if err != nil {
		return 0, err
	}
	dispatched := 0
	for i := range pending {
		if err := d.dispatch(ctx, &pending[i]); err != nil {
			return dispatched, err // store-level defect — abort sweep
		}
		if pending[i].Status == reporting.SubSubmitted || pending[i].Status == reporting.SubAcked {
			dispatched++
		}
	}
	return dispatched, nil
}

func (d *SubmissionDispatcher) dispatch(ctx context.Context, sub *reporting.Submission) error {
	client, ok := d.Clients[sub.Destination]
	now := d.now()

	row := &TransportRow{
		ReportSubmissionID: sub.ReportSubmissionID,
		ReportEventID:      sub.EventID,
		RegulationType:     regulationLabel(sub.Regime, sub.Destination),
		DestinationType:    string(sub.Destination),
		BatchID:            batchID(sub.Destination, now),
		Payload:            sub.Payload,
		PayloadXML:         sub.PayloadXML,
		PayloadHash:        sub.PayloadHash,
		SchemaName:         sub.SchemaName,
		SchemaVersion:      sub.SchemaVersion,
		Attempt:            sub.Attempt,
		AckStatus:          TkPending,
	}

	if !ok || client == nil {
		// Unconfigured endpoint — fail closed: ledger the defect once
		// per artifact, alert, park for config fix (retry in 1h).
		if prev, err := d.Ledger.LatestFor(ctx, sub.ReportSubmissionID); err != nil {
			return err
		} else if prev != nil && prev.ErrorCode == "ENDPOINT_UNCONFIGURED" {
			return nil // already ledgered — don't spam
		}
		row.AckStatus = TkFailed
		row.ErrorCode = "ENDPOINT_UNCONFIGURED"
		row.ErrorDetail = fmt.Sprintf("no vendor client for %s", sub.Destination)
		next := now.Add(time.Hour)
		row.NextAttemptAt = &next
		row.DestinationEndpoint = string(sub.Destination)
		if err := d.Ledger.Insert(ctx, row); err != nil && !isUniqueViolation(err) {
			return err
		}
		d.alert(ctx, "P1", "ENDPOINT_UNCONFIGURED",
			fmt.Sprintf("regulatory artifact %d has no %s vendor client",
				sub.ReportSubmissionID, sub.Destination))
		return nil
	}
	row.DestinationEndpoint = client.EndpointLabel()

	if err := d.Ledger.Insert(ctx, row); err != nil {
		if isUniqueViolation(err) {
			return nil // this wire attempt already ledgered (replay)
		}
		return err
	}

	verdict, err := client.Submit(ctx, sub)
	if err != nil {
		next := now.Add(backoff(row.Attempt))
		_ = d.Ledger.FailAttempt(ctx, row.ID, "TRANSPORT_ERROR",
			err.Error(), next, now)
		d.alert(ctx, "P2", "VENDOR_TRANSPORT_ERROR",
			fmt.Sprintf("artifact %d → %s failed: %v",
				sub.ReportSubmissionID, sub.Destination, err))
		return nil
	}
	_ = d.Ledger.AppendResponse(ctx, row.ID, verdict.HTTPStatus,
		[]byte(verdict.Text), client.EndpointLabel())

	// The wire attempt landed — the artifact is in-flight regardless of
	// verdict.
	if err := d.Svc.Store.MarkSubmissionDispatched(ctx,
		sub.ReportSubmissionID, now); err != nil {
		return err
	}
	sub.Status = reporting.SubSubmitted
	_ = d.Svc.Store.SetEventStatus(ctx, sub.EventID,
		reporting.EventSubmitted, nil)

	if verdict.Status == nil {
		// Async — verdict arrives via the ack callback seam.
		return d.Ledger.SetVerdict(ctx, row.ID, TkSubmitted, "", "",
			verdict.Ref, now)
	}

	switch *verdict.Status {
	case reporting.AckAccept:
		if err := d.Ledger.SetVerdict(ctx, row.ID, TkAcked,
			verdict.Code, verdict.Text, verdict.Ref, now); err != nil {
			return err
		}
		sub.Status = reporting.SubAcked
		_, err = d.Svc.IngestAck(ctx, reporting.Ack{
			ReportSubmissionID: sub.ReportSubmissionID,
			EventID:            sub.EventID,
			AckStatus:          reporting.AckAccept,
			AckCode:            verdict.Code,
			AckText:            verdict.Text,
			ExternalRef:        verdict.Ref,
		})
		return err
	case reporting.AckReject:
		if err := d.Ledger.SetVerdict(ctx, row.ID, TkNacked,
			verdict.Code, verdict.Text, verdict.Ref, now); err != nil {
			return err
		}
		_, err = d.Svc.IngestAck(ctx, reporting.Ack{
			ReportSubmissionID: sub.ReportSubmissionID,
			EventID:            sub.EventID,
			AckStatus:          reporting.AckReject,
			AckCode:            verdict.Code,
			AckText:            verdict.Text,
			ExternalRef:        verdict.Ref,
		})
		return err
	}
	return nil
}

// batchID groups ARM submissions by trade date (T+1 batch semantics —
// §14.5). Other destinations dispatch unbatched.
func batchID(dest reporting.Destination, at time.Time) string {
	if dest == reporting.DestinationARM {
		return "ARM-" + at.UTC().Format("20060102")
	}
	return ""
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == "23505"
	}
	return false
}
