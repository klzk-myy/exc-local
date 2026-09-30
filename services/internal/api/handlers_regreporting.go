// Phase-21 Tasks 21.3.4/.5/.9/.14/.16 — regulatory reporting admin
// surface: the Compliance Officer repair queue (rejected NACKs +
// validation quarantines + transport failures), corrected resubmission,
// break resolution, party-identifier maintenance, event export, the
// async vendor ACK/NACK ingest seam and on-demand reconciliation.
//
// Role gates ride the route registry (Compliance Officer for mutations,
// Read-Only Auditor for reads); handler-side identity resolves via
// adminActorFrom — fail-closed on both layers.
package api

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"exchange/internal/compliance"
	"exchange/internal/compliance/reporting"
	"exchange/internal/gateway"
	"exchange/internal/middleware"
)

// RegReportingDeps wires the handler surface — the canonical service +
// transport ledger; the regime adapters ride along for task-shaped
// entry points (deadline assertions, regime-specific repair paths).
type RegReportingDeps struct {
	Svc        *reporting.Service
	Ledger     *compliance.SubmissionsLedger
	MiFID      *compliance.MiFIDReporter
	EMIR       *compliance.EMIRReporter
	DoddFrank  *compliance.DoddFrankReporter
	TrustProxy bool
	// ForceRegime pins the event export to one regime (the legacy
	// /admin/emir-report mount); empty = ?regime= required.
	ForceRegime reporting.Regime
}

// ---------------------------------------------------------------------------
// Repair queue + breaks
// ---------------------------------------------------------------------------

// AdminRegQueue serves GET /api/v1/admin/regreporting/queue — the
// merged officer queue: open reconciliation/repair breaks plus
// NACKED/FAILED/REPAIRING transport rows.
func AdminRegQueue(d RegReportingDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		limit := queryLimit(r, 200)
		var regime reporting.Regime
		if q := r.URL.Query().Get("regime"); q != "" {
			regime = reporting.Regime(q)
		}
		breaks, err := d.Svc.Store.OpenBreaks(r.Context(), regime, limit)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		var transports []compliance.RepairQueueItem
		if d.Ledger != nil {
			transports, err = d.Ledger.RepairQueue(r.Context(), limit)
			if err != nil {
				writeServiceErr(w, r, err)
				return
			}
		}
		if breaks == nil {
			breaks = []reporting.Break{}
		}
		if transports == nil {
			transports = []compliance.RepairQueueItem{}
		}
		WriteJSON(w, http.StatusOK, map[string]any{
			"breaks": breaks, "transport": transports,
		})
	}
}

// AdminRegBreakResolve serves POST /api/v1/admin/regreporting/breaks/{id}/resolve
// — disposition {resolution: RESOLVED|WONT_FIX, notes}.
func AdminRegBreakResolve(d RegReportingDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		actor, err := adminActorFrom(r, d.TrustProxy)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		breakID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil || breakID <= 0 {
			WriteError(w, "INVALID_REQUEST", "bad break id",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		var body struct {
			Resolution string `json:"resolution"`
			Notes      string `json:"notes"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			WriteError(w, "INVALID_REQUEST", "malformed body",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		status := reporting.BreakStatus(body.Resolution)
		if status != reporting.BreakResolved && status != reporting.BreakWontFix {
			WriteError(w, "INVALID_REQUEST", "resolution must be RESOLVED or WONT_FIX",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		applied, err := d.Svc.Store.ResolveBreakTx(r.Context(), breakID, status,
			actor.UserID, body.Notes, time.Now().UTC(), middleware.ClientIP(r, d.TrustProxy))
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{
			"break_id": breakID, "resolution": string(status), "applied": applied,
		})
	}
}

// ---------------------------------------------------------------------------
// Events + submissions
// ---------------------------------------------------------------------------

// AdminRegEvents serves GET /api/v1/admin/regreporting/events — the
// canonical event export (?regime=&from=&to=&limit=).
func AdminRegEvents(d RegReportingDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		regime := reporting.Regime(r.URL.Query().Get("regime"))
		if d.ForceRegime != "" {
			regime = d.ForceRegime
		}
		if regime == "" {
			WriteError(w, "INVALID_REQUEST", "regime is required",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		var from, to *time.Time
		if q := r.URL.Query().Get("from"); q != "" {
			if t, err := time.Parse(time.RFC3339, q); err == nil {
				from = &t
			}
		}
		if q := r.URL.Query().Get("to"); q != "" {
			if t, err := time.Parse(time.RFC3339, q); err == nil {
				to = &t
			}
		}
		evs, err := d.Svc.Store.ExportEvents(r.Context(), regime, from, to,
			queryLimit(r, 500))
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		if evs == nil {
			evs = []reporting.Event{}
		}
		WriteJSON(w, http.StatusOK, map[string]any{"events": evs})
	}
}

// AdminRegEventDetail serves GET /api/v1/admin/regreporting/events/{id}
// — the event plus its immutable artifact + ack history.
func AdminRegEventDetail(d RegReportingDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil || id <= 0 {
			WriteError(w, "INVALID_REQUEST", "bad event id",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		e, err := d.Svc.Store.EventByID(r.Context(), id)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		if e == nil {
			WriteError(w, "NOT_FOUND", "regulatory event not found",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		subs, err := d.Svc.Store.SubmissionsForEvent(r.Context(), id)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		acks, err := d.Svc.Store.AcksForEvent(r.Context(), id)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		if subs == nil {
			subs = []reporting.Submission{}
		}
		if acks == nil {
			acks = []reporting.Ack{}
		}
		WriteJSON(w, http.StatusOK, map[string]any{
			"event": e, "submissions": subs, "acks": acks,
		})
	}
}

// AdminRegSubmissions serves GET /api/v1/admin/regreporting/submissions
// — the transport ledger feed (?status=PENDING|SUBMITTED|ACK|NACK|…).
func AdminRegSubmissions(d RegReportingDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if d.Ledger == nil {
			WriteError(w, "SERVICE_DEGRADED", "transport ledger unavailable",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		rows, err := d.Ledger.ListTransport(r.Context(),
			r.URL.Query().Get("status"), queryLimit(r, 200))
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		if rows == nil {
			rows = []compliance.TransportRow{}
		}
		WriteJSON(w, http.StatusOK, map[string]any{"submissions": rows})
	}
}

// AdminRegResubmit serves POST /api/v1/admin/regreporting/submissions/{id}/resubmit
// — Compliance Officer correction → corrected artifact attempt
// {corrections: {field: value, …}}.
func AdminRegResubmit(d RegReportingDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		actor, err := adminActorFrom(r, d.TrustProxy)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil || id <= 0 {
			WriteError(w, "INVALID_REQUEST", "bad submission id",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		var body struct {
			Corrections map[string]any `json:"corrections"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body.Corrections) == 0 {
			WriteError(w, "INVALID_REQUEST", "corrections object required",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		fresh, err := d.Svc.Resubmit(r.Context(), id, body.Corrections, actor.UserID)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusCreated, map[string]any{"submission": fresh})
	}
}

// AdminRegAckIngest serves POST /api/v1/admin/regreporting/acks — the
// asynchronous vendor verdict seam (async NACK per Task 21.3.16):
// {report_submission_id|external_ref, status: ACK|NACK|RECON, code?,
//
//	text?}.
func AdminRegAckIngest(d RegReportingDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		actor, err := adminActorFrom(r, d.TrustProxy)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		var body struct {
			ReportSubmissionID int64           `json:"report_submission_id"`
			ExternalRef        string          `json:"external_ref"`
			Status             string          `json:"status"`
			Code               string          `json:"code"`
			Text               string          `json:"text"`
			Payload            json.RawMessage `json:"payload"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			WriteError(w, "INVALID_REQUEST", "malformed body",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		subID := body.ReportSubmissionID
		if subID == 0 && body.ExternalRef != "" && d.Ledger != nil {
			// Correlate by the vendor's receipt id.
			row, err := d.Ledger.FindTransportByRef(r.Context(), body.ExternalRef)
			if err != nil {
				writeServiceErr(w, r, err)
				return
			}
			if row == nil {
				WriteError(w, "NOT_FOUND", "no submission for external_ref",
					gateway.RequestIDFrom(r.Context()), nil)
				return
			}
			subID = row.ReportSubmissionID
		}
		if subID == 0 {
			WriteError(w, "INVALID_REQUEST",
				"report_submission_id or external_ref required",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		sub, err := d.Svc.Store.SubmissionByID(r.Context(), subID)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		if sub == nil {
			WriteError(w, "NOT_FOUND", "artifact not found",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		var ackStatus reporting.AckStatus
		switch reporting.AckStatus(body.Status) {
		case reporting.AckAccept, reporting.AckReject, reporting.AckRecon:
			ackStatus = reporting.AckStatus(body.Status)
		default:
			WriteError(w, "INVALID_REQUEST", "status must be ACK|NACK|RECON",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		br, err := d.Svc.IngestAck(r.Context(), reporting.Ack{
			ReportSubmissionID: subID,
			EventID:            sub.EventID,
			AckStatus:          ackStatus,
			AckCode:            body.Code,
			AckText:            body.Text,
			ExternalRef:        body.ExternalRef,
			Payload:            body.Payload,
		})
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		// The officer posted the verdict — attribute the repair-queue
		// entry (admin audit already stamped by the ack tx).
		_ = actor
		WriteJSON(w, http.StatusOK, map[string]any{
			"ingested": true, "break": br,
		})
	}
}

// ---------------------------------------------------------------------------
// Party identifiers + reconciliation
// ---------------------------------------------------------------------------

// AdminRegPartyUpsert serves POST /api/v1/admin/regreporting/party-identifiers
// — registers/repairs the account's LEI/national-id/decision-maker
// (LEI checksum-validated at write; missing identifiers quarantine
// reports, never fabricated).
func AdminRegPartyUpsert(d RegReportingDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		actor, err := adminActorFrom(r, d.TrustProxy)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		var body struct {
			AccountID         int64  `json:"account_id"`
			LEI               string `json:"lei"`
			NationalIDType    string `json:"national_id_type"`
			NationalID        string `json:"national_id"`
			DecisionMakerID   string `json:"decision_maker_id"`
			DecisionMakerType string `json:"decision_maker_type"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.AccountID <= 0 {
			WriteError(w, "INVALID_REQUEST", "account_id required",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		if body.LEI != "" {
			if err := reporting.ValidateLEI(body.LEI); err != nil {
				WriteError(w, "INVALID_REQUEST", "lei fails ISO 17442: "+err.Error(),
					gateway.RequestIDFrom(r.Context()), nil)
				return
			}
		}
		err = d.Svc.Store.UpsertParty(r.Context(), reporting.PartyIdentifiers{
			AccountID:         body.AccountID,
			LEI:               body.LEI,
			NationalIDType:    body.NationalIDType,
			NationalID:        body.NationalID,
			DecisionMakerID:   body.DecisionMakerID,
			DecisionMakerType: body.DecisionMakerType,
		}, actor.UserID, middleware.ClientIP(r, d.TrustProxy))
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"account_id": body.AccountID})
	}
}

// AdminRegReconcile serves POST /api/v1/admin/regreporting/reconcile
// {regime: EMIR_REFIT|CFTC_P45} — an on-demand repo-vs-internal pass.
func AdminRegReconcile(d RegReportingDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Regime string `json:"regime"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		rep, err := d.Svc.Reconcile(r.Context(), reporting.Regime(body.Regime))
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"report": rep})
	}
}

// queryLimit reads ?limit= with a sane default/cap.
func queryLimit(r *http.Request, def int) int {
	if q := r.URL.Query().Get("limit"); q != "" {
		if n, err := strconv.Atoi(q); err == nil && n > 0 {
			if n > 1000 {
				return 1000
			}
			return n
		}
	}
	return def
}
