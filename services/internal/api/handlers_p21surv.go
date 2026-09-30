// Phase-21 wave-2 admin handlers — surveillance enforcement, case
// management, RTS 6 algo/DEA controls, employee dealing and the
// audit-trail query API (Tasks 21.3.8/21.3.12/21.3.21/21.3.24/21.3.27).
//
// Route registry rows live in internal/gateway/routes_v1.go; the
// route-level adminAuth(RoleComplianceOfficer) gate runs first, and
// each mutating handler re-checks inside the service (defence in
// depth). Read endpoints stay officer/auditor-readable per §8.2.
package api

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"exchange/internal/admin"
	"exchange/internal/compliance"
	"exchange/internal/gateway"
)

// ---------------------------------------------------------------------------
// Task 21.3.8 — POST /api/v1/admin/enforcement/{signal_id}
// ---------------------------------------------------------------------------

// AdminEnforce takes the officer's action on a confirmed surveillance
// signal — WARN | THROTTLE | RESTRICT | SUSPEND | DISMISS — through
// compliance.EnforcementService (holds, kill-switch and throttle seams
// inside; never the matching engine).
func AdminEnforce(svc *compliance.EnforcementService, trustProxy bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		actor, err := adminActorFrom(r, trustProxy)
		if err != nil {
			writeSvcErr(w, r, err)
			return
		}
		sigID, err := strconv.ParseInt(r.PathValue("signal_id"), 10, 64)
		if err != nil || sigID <= 0 {
			WriteError(w, "INVALID_REQUEST", "signal_id must be an integer",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		var body struct {
			Action     string         `json:"action"`
			Note       string         `json:"note"`
			Params     map[string]any `json:"params"`
			TTLSeconds int64          `json:"ttl_seconds"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			WriteError(w, "INVALID_REQUEST", "malformed body",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		act, err := svc.Enforce(r.Context(), compliance.EnforcementRequest{
			SignalID: sigID, Action: body.Action, Params: body.Params,
			Source: "MANUAL", ActorID: actor.UserID, Note: body.Note,
			TTL: time.Duration(body.TTLSeconds) * time.Second,
		})
		if err != nil {
			writeSvcErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"action": act})
	}
}

// AdminEnforcementList — GET /api/v1/admin/enforcement?account_id=&limit=
// — the enforcement action ledger (auditor/officer-readable).
func AdminEnforcementList(svc *compliance.EnforcementService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		acct, _ := strconv.ParseInt(r.URL.Query().Get("account_id"), 10, 64)
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		out, err := svc.List(r.Context(), acct, limit)
		if err != nil {
			writeSvcErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"actions": out})
	}
}

// ---------------------------------------------------------------------------
// Task 21.3.21 — surveillance case management
// ---------------------------------------------------------------------------

// AdminSurveillanceCases — GET /api/v1/admin/surveillance/cases
// (?status=&assignee=&limit=).
func AdminSurveillanceCases(svc *compliance.CaseService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		limit, _ := strconv.Atoi(q.Get("limit"))
		cases, err := svc.List(r.Context(), q.Get("status"),
			q.Get("assignee"), limit)
		if err != nil {
			writeSvcErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"cases": cases})
	}
}

// AdminSurveillanceCaseGet — GET /api/v1/admin/surveillance/cases/{id}
// returns the investigation workspace (case + evidence + linked
// signals + order-audit refs).
func AdminSurveillanceCaseGet(svc *compliance.CaseService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil || id <= 0 {
			WriteError(w, "INVALID_REQUEST", "id must be an integer",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		ws, err := svc.Workspace(r.Context(), id)
		if err != nil {
			writeSvcErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, ws)
	}
}

// AdminSurveillanceCaseAssign — POST .../cases/{id}/assign
// body {"assignee": <user_id>} (0/omit → round-robin).
func AdminSurveillanceCaseAssign(svc *compliance.CaseService, trustProxy bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		actor, err := adminActorFrom(r, trustProxy)
		if err != nil {
			writeSvcErr(w, r, err)
			return
		}
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil || id <= 0 {
			WriteError(w, "INVALID_REQUEST", "id must be an integer",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		var body struct {
			Assignee int64 `json:"assignee"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body) // empty body → round-robin
		c, err := svc.Assign(r.Context(), actor.UserID, id, body.Assignee)
		if err != nil {
			writeSvcErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"case": c})
	}
}

// AdminSurveillanceCaseEvidence — POST .../cases/{id}/evidence
// body {"kind","body","attachment_ref","sha256"} — append-only.
func AdminSurveillanceCaseEvidence(svc *compliance.CaseService, trustProxy bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		actor, err := adminActorFrom(r, trustProxy)
		if err != nil {
			writeSvcErr(w, r, err)
			return
		}
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil || id <= 0 {
			WriteError(w, "INVALID_REQUEST", "id must be an integer",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		var body struct {
			Kind          string `json:"kind"`
			Body          string `json:"body"`
			AttachmentRef string `json:"attachment_ref"`
			SHA256        string `json:"sha256"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			WriteError(w, "INVALID_REQUEST", "malformed body",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		ev, err := svc.AttachEvidence(r.Context(), actor.UserID, id,
			body.Kind, body.Body, body.AttachmentRef, body.SHA256)
		if err != nil {
			writeSvcErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"evidence": ev})
	}
}

// AdminSurveillanceCaseDisposition — POST .../cases/{id}/disposition
// body {"disposition","reason","action"} — terminal.
func AdminSurveillanceCaseDisposition(svc *compliance.CaseService, trustProxy bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		actor, err := adminActorFrom(r, trustProxy)
		if err != nil {
			writeSvcErr(w, r, err)
			return
		}
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil || id <= 0 {
			WriteError(w, "INVALID_REQUEST", "id must be an integer",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		var body struct {
			Disposition string `json:"disposition"`
			Reason      string `json:"reason"`
			Action      string `json:"action"` // ESCALATE_ACTION verb
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			WriteError(w, "INVALID_REQUEST", "malformed body",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		c, err := svc.Disposition(r.Context(), actor.UserID, id,
			compliance.DispositionRequest{
				Disposition: body.Disposition, Reason: body.Reason,
				Action: body.Action,
			})
		if err != nil {
			writeSvcErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"case": c})
	}
}

// AdminSurveillanceSummary — GET /api/v1/admin/surveillance/summary
// ?month=YYYY-MM → opened/closed/escalated/accuracy report.
func AdminSurveillanceSummary(svc *compliance.CaseService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		month := r.URL.Query().Get("month")
		if month == "" {
			month = time.Now().UTC().Format("2006-01")
		}
		m, err := svc.MonthlySummary(r.Context(), month)
		if err != nil {
			writeSvcErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, m)
	}
}

// ---------------------------------------------------------------------------
// Task 21.3.12 — RTS 6 algo certification + DEA + retention
// ---------------------------------------------------------------------------

// AdminAlgoCertify — POST /api/v1/admin/algo-certifications
// (Compliance Officer). Registers/renews an algo certification.
func AdminAlgoCertify(svc *compliance.RTS6Service, trustProxy bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		actor, err := adminActorFrom(r, trustProxy)
		if err != nil {
			writeSvcErr(w, r, err)
			return
		}
		var body struct {
			AlgoID            string `json:"algo_id"`
			AccountID         int64  `json:"account_id"`
			TestEvidenceRef   string `json:"test_evidence_ref"`
			KillButtonTested  bool   `json:"kill_button_tested"`
			CapacityAssessRef string `json:"capacity_assessment_ref"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			WriteError(w, "INVALID_REQUEST", "malformed body",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		c, err := svc.Certify(r.Context(), compliance.CertifyRequest{
			AlgoID: body.AlgoID, AccountID: body.AccountID,
			TestEvidenceRef:   body.TestEvidenceRef,
			KillButtonTested:  body.KillButtonTested,
			CapacityAssessRef: body.CapacityAssessRef,
			ActorID:           actor.UserID,
		})
		if err != nil {
			writeSvcErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"certification": c})
	}
}

// AdminAlgoCertList — GET /api/v1/admin/algo-certifications?status=.
func AdminAlgoCertList(svc *compliance.RTS6Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		limit, _ := strconv.Atoi(q.Get("limit"))
		out, err := svc.ListCertifications(r.Context(), q.Get("status"), limit)
		if err != nil {
			writeSvcErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"certifications": out})
	}
}

// AdminAlgoCertTransition — POST /api/v1/admin/algo-certifications/{id}/transition
// body {"to": "SUSPENDED|REVOKED|CERTIFIED", "reason"}.
func AdminAlgoCertTransition(svc *compliance.RTS6Service, trustProxy bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		actor, err := adminActorFrom(r, trustProxy)
		if err != nil {
			writeSvcErr(w, r, err)
			return
		}
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil || id <= 0 {
			WriteError(w, "INVALID_REQUEST", "id must be an integer",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		var body struct {
			To     string `json:"to"`
			Reason string `json:"reason"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			WriteError(w, "INVALID_REQUEST", "malformed body",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		c, err := svc.Transition(r.Context(), actor.UserID, id,
			body.To, body.Reason)
		if err != nil {
			writeSvcErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"certification": c})
	}
}

// AdminDEAControls — GET /api/v1/admin/dea/controls?session_id= (read)
// and POST (upsert limits). Split handlers keep the verb contract clear.
func AdminDEAGet(svc *compliance.RTS6Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sessionID := r.URL.Query().Get("session_id")
		if sessionID == "" {
			WriteError(w, "INVALID_REQUEST", "session_id is required",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		c, ok, err := svc.DEALimitsFor(r.Context(), sessionID)
		if err != nil {
			writeSvcErr(w, r, err)
			return
		}
		if !ok {
			WriteError(w, "NOT_FOUND", "no DEA control for session",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"dea_control": c})
	}
}

// AdminDEASet — POST /api/v1/admin/dea/controls — upsert session
// limits (max_order_qty decimal string, max_msgs_per_sec, desk, feed).
func AdminDEASet(svc *compliance.RTS6Service, trustProxy bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		actor, err := adminActorFrom(r, trustProxy)
		if err != nil {
			writeSvcErr(w, r, err)
			return
		}
		var body struct {
			SessionID      string `json:"session_id"`
			AccountID      int64  `json:"account_id"`
			MaxOrderQty    string `json:"max_order_qty"`
			MaxMsgsPerSec  int    `json:"max_msgs_per_sec"`
			SponsoringDesk string `json:"sponsoring_desk"`
			DropCopyFeed   string `json:"drop_copy_feed"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			WriteError(w, "INVALID_REQUEST", "malformed body",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		c, err := svc.SetDEALimits(r.Context(), compliance.DEAControls{
			SessionID: body.SessionID, AccountID: body.AccountID,
			MaxOrderQty: body.MaxOrderQty, MaxMsgsPerSec: body.MaxMsgsPerSec,
			SponsoringDesk: body.SponsoringDesk, DropCopyFeed: body.DropCopyFeed,
			CreatedBy: actor.UserID,
		})
		if err != nil {
			writeSvcErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"dea_control": c})
	}
}

// AdminDEASuspend — POST /api/v1/admin/dea/controls/{session_id}/suspend.
func AdminDEASuspend(svc *compliance.RTS6Service, trustProxy bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		actor, err := adminActorFrom(r, trustProxy)
		if err != nil {
			writeSvcErr(w, r, err)
			return
		}
		var body struct {
			Reason string `json:"reason"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if err := svc.SuspendDEA(r.Context(), actor.UserID,
			r.PathValue("session_id"), body.Reason); err != nil {
			writeSvcErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"status": "SUSPENDED"})
	}
}

// AdminRTS6Assessments — GET /api/v1/admin/rts6/self-assessments.
func AdminRTS6Assessments(svc *compliance.RTS6Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		out, err := svc.ListAssessments(r.Context(), limit)
		if err != nil {
			writeSvcErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"assessments": out})
	}
}

// AdminRTS6FileAssessment — POST /api/v1/admin/rts6/self-assessments
// body {"period_year","document_ref","due_at"}.
func AdminRTS6FileAssessment(svc *compliance.RTS6Service, trustProxy bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		actor, err := adminActorFrom(r, trustProxy)
		if err != nil {
			writeSvcErr(w, r, err)
			return
		}
		var body struct {
			PeriodYear  int    `json:"period_year"`
			DocumentRef string `json:"document_ref"`
			DueAt       string `json:"due_at"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			WriteError(w, "INVALID_REQUEST", "malformed body",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		due, err := time.Parse("2006-01-02", body.DueAt)
		if err != nil {
			WriteError(w, "INVALID_REQUEST", "due_at must be YYYY-MM-DD",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		a, err := svc.FileAssessment(r.Context(), body.PeriodYear,
			body.DocumentRef, actor.UserID, due)
		if err != nil {
			writeSvcErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"assessment": a})
	}
}

// AdminOrderLifecycleExport — GET /api/v1/admin/order-records/{order_id}/export
// — the RTS 6 Art. 17 ≥5y order-lifecycle record.
func AdminOrderLifecycleExport(svc *compliance.RTS6Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		orderID, err := strconv.ParseInt(r.PathValue("order_id"), 10, 64)
		if err != nil || orderID <= 0 {
			WriteError(w, "INVALID_REQUEST", "order_id must be an integer",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		rows, err := svc.ExportOrderLifecycle(r.Context(), orderID)
		if err != nil {
			writeSvcErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{
			"order_id": orderID, "lifecycle": rows,
			"retention": "order_audit — 1825d (MiFID II RTS 6 Art. 17)",
		})
	}
}

// ---------------------------------------------------------------------------
// Task 21.3.24 — employee dealing / restricted lists / pre-clearance
// ---------------------------------------------------------------------------

// AdminRestrictedList — GET /api/v1/admin/restricted-lists?status=.
func AdminRestrictedList(svc *admin.RestrictedListService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		out, err := svc.List(r.Context(), r.URL.Query().Get("status"), limit)
		if err != nil {
			writeSvcErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"restricted_lists": out})
	}
}

// AdminRestrictedListCreate — POST /api/v1/admin/restricted-lists.
func AdminRestrictedListCreate(svc *admin.RestrictedListService,
	trustProxy bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		actor, err := adminActorFrom(r, trustProxy)
		if err != nil {
			writeSvcErr(w, r, err)
			return
		}
		var body struct {
			EventID       string   `json:"event_id"`
			EventType     string   `json:"event_type"`
			Instruments   []string `json:"instruments"`
			WindowStart   string   `json:"window_start"`
			WindowEnd     string   `json:"window_end"`
			WidenMinutes  int      `json:"widen_minutes"`
			Scope         string   `json:"scope"`
			ScopeRole     string   `json:"scope_role"`
			NamedAccounts []int64  `json:"named_accounts"`
			Reason        string   `json:"reason"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			WriteError(w, "INVALID_REQUEST", "malformed body",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		start, err := time.Parse(time.RFC3339, body.WindowStart)
		if err != nil {
			WriteError(w, "INVALID_REQUEST", "window_start must be RFC3339",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		end, err := time.Parse(time.RFC3339, body.WindowEnd)
		if err != nil {
			WriteError(w, "INVALID_REQUEST", "window_end must be RFC3339",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		var scopeRole *string
		if body.ScopeRole != "" {
			scopeRole = &body.ScopeRole
		}
		l, err := svc.Create(r.Context(), actor.UserID, admin.RestrictedList{
			EventID: body.EventID, EventType: body.EventType,
			Instruments: body.Instruments, WindowStart: start,
			WindowEnd: end, WidenMinutes: body.WidenMinutes,
			Scope: body.Scope, ScopeRole: scopeRole,
			NamedAccounts: body.NamedAccounts, Reason: body.Reason,
		})
		if err != nil {
			writeSvcErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"restricted_list": l})
	}
}

// AdminRestrictedListRetire — DELETE /api/v1/admin/restricted-lists?id=.
func AdminRestrictedListRetire(svc *admin.RestrictedListService,
	trustProxy bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		actor, err := adminActorFrom(r, trustProxy)
		if err != nil {
			writeSvcErr(w, r, err)
			return
		}
		id, err := strconv.ParseInt(r.URL.Query().Get("id"), 10, 64)
		if err != nil || id <= 0 {
			WriteError(w, "INVALID_REQUEST", "id query param is required",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		if err := svc.Retire(r.Context(), actor.UserID, id); err != nil {
			writeSvcErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"status": "RETIRED"})
	}
}

// AdminPreClearance — POST /api/v1/admin/pre-clearance. Two forms:
//
//	{account_id, instrument, side, notional_cap, reason, expires_at}
//	    → file a request (officer may file on the desk's behalf)
//	{id|request_id, approve, note}
//	    → the independent-controller decision
func AdminPreClearance(svc *compliance.EmployeeDealingService,
	trustProxy bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		actor, err := adminActorFrom(r, trustProxy)
		if err != nil {
			writeSvcErr(w, r, err)
			return
		}
		var body struct {
			ID          int64   `json:"id"`
			RequestID   string  `json:"request_id"`
			Approve     *bool   `json:"approve"`
			Note        string  `json:"note"`
			AccountID   int64   `json:"account_id"`
			Instrument  string  `json:"instrument"`
			Side        string  `json:"side"`
			NotionalCap *string `json:"notional_cap"`
			Reason      string  `json:"reason"`
			ExpiresAt   string  `json:"expires_at"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			WriteError(w, "INVALID_REQUEST", "malformed body",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		if body.ID > 0 || body.RequestID != "" {
			if body.Approve == nil {
				WriteError(w, "INVALID_REQUEST",
					"decision requires approve:true|false",
					gateway.RequestIDFrom(r.Context()), nil)
				return
			}
			id := body.ID
			if id <= 0 {
				resolved, rerr := resolvePreClearanceID(r, svc, body.RequestID)
				if rerr != nil {
					writeSvcErr(w, r, rerr)
					return
				}
				id = resolved
			}
			p, err := svc.DecideClearance(r.Context(), actor.UserID, id,
				*body.Approve, body.Note)
			if err != nil {
				writeSvcErr(w, r, err)
				return
			}
			WriteJSON(w, http.StatusOK, map[string]any{"pre_clearance": p})
			return
		}
		if body.AccountID <= 0 || body.Reason == "" {
			WriteError(w, "INVALID_REQUEST",
				"request form requires account_id and reason",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		var expires time.Time
		if body.ExpiresAt != "" {
			var perr error
			expires, perr = time.Parse(time.RFC3339, body.ExpiresAt)
			if perr != nil {
				WriteError(w, "INVALID_REQUEST", "expires_at must be RFC3339",
					gateway.RequestIDFrom(r.Context()), nil)
				return
			}
		}
		// Officer-filed on behalf of the desk: requested_by is the
		// account owner — resolve inside the service (ownership rule
		// relaxes for officer role).
		p, err := svc.RequestClearanceOfficer(r.Context(), actor.UserID,
			body.AccountID, body.Instrument, body.Side, body.Reason,
			body.NotionalCap, expires)
		if err != nil {
			writeSvcErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"pre_clearance": p})
	}
}

// resolvePreClearanceID maps the public request_id to the row id.
func resolvePreClearanceID(r *http.Request,
	svc *compliance.EmployeeDealingService, requestID string) (int64, error) {
	return svc.ResolveRequestID(r.Context(), requestID)
}

// AdminPreClearanceList — GET /api/v1/admin/pre-clearance
// (?account_id=&outcome=).
func AdminPreClearanceList(svc *compliance.EmployeeDealingService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		accountID, _ := strconv.ParseInt(q.Get("account_id"), 10, 64)
		limit, _ := strconv.Atoi(q.Get("limit"))
		out, err := svc.ListClearances(r.Context(), accountID,
			q.Get("outcome"), limit)
		if err != nil {
			writeSvcErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"pre_clearances": out})
	}
}

// AdminEmployeeDealingAudit — GET /api/v1/admin/employee-dealing/audit
// — the auditor's pre-clearance / staff-trade trail.
func AdminEmployeeDealingAudit(svc *compliance.EmployeeDealingService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		limit, _ := strconv.Atoi(q.Get("limit"))
		after, _ := strconv.ParseInt(q.Get("after_id"), 10, 64)
		rows, err := svc.Audit(r.Context(), limit, after)
		if err != nil {
			writeSvcErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"audit": rows})
	}
}

// ---------------------------------------------------------------------------
// Task 21.3.27 — surveillance tuning + audit-trail query
// ---------------------------------------------------------------------------

// AdminTuningList — GET /api/v1/admin/surveillance/tuning?signal_type=.
func AdminTuningList(svc *compliance.TuningService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		out, err := svc.List(r.Context(),
			r.URL.Query().Get("signal_type"), limit)
		if err != nil {
			writeSvcErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"tuning": out})
	}
}

// AdminTuningPropose — POST /api/v1/admin/surveillance/tuning
// body {"signal_type","params","fp_target_pct"} → DRAFT version.
func AdminTuningPropose(svc *compliance.TuningService, trustProxy bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		actor, err := adminActorFrom(r, trustProxy)
		if err != nil {
			writeSvcErr(w, r, err)
			return
		}
		var body struct {
			SignalType  string          `json:"signal_type"`
			Params      json.RawMessage `json:"params"`
			FPTargetPct float64         `json:"fp_target_pct"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			WriteError(w, "INVALID_REQUEST", "malformed body",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		t, err := svc.Propose(r.Context(), actor.UserID,
			body.SignalType, body.Params, body.FPTargetPct)
		if err != nil {
			writeSvcErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"tuning": t})
	}
}

// AdminTuningActivate — POST /api/v1/admin/surveillance/tuning/{signal}/activate
// body {"version":N} — atomic ACTIVE swap (no downtime).
func AdminTuningActivate(svc *compliance.TuningService, trustProxy bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		actor, err := adminActorFrom(r, trustProxy)
		if err != nil {
			writeSvcErr(w, r, err)
			return
		}
		var body struct {
			Version int `json:"version"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			WriteError(w, "INVALID_REQUEST", "malformed body",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		t, err := svc.Activate(r.Context(), actor.UserID,
			r.PathValue("signal"), body.Version)
		if err != nil {
			writeSvcErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"tuning": t})
	}
}

// AdminTuningBacktest — GET /api/v1/admin/surveillance/tuning/{signal}/backtest
// ?from=&to= (YYYY-MM-DD) → FP-rate calibration report.
func AdminTuningBacktest(svc *compliance.TuningService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		from, err := time.Parse("2006-01-02", q.Get("from"))
		if err != nil {
			from = time.Now().UTC().Add(-90 * 24 * time.Hour)
		}
		to, err2 := time.Parse("2006-01-02", q.Get("to"))
		if err2 != nil {
			to = time.Now().UTC()
		}
		res, err := svc.Backtest(r.Context(), r.PathValue("signal"),
			from, to)
		if err != nil {
			writeSvcErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, res)
	}
}

// AdminAuditTrail — GET /api/v1/admin/audit/trail — the joined
// admin_audit_log ⨝ audit_hash_chain search (Task 21.3.27). mask=1
// produces the auditor-shareable export (truncated IPs, scrubbed PII
// keys). Read-Only Auditor+ (requireAuditRole).
func AdminAuditTrail(svc *compliance.AuditTrailQuery,
	resolver AdminRoleResolver) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, ok := requireAuditRole(w, r, resolver); !ok {
			return
		}
		q := r.URL.Query()
		f := compliance.AuditTrailFilter{
			Action:     q.Get("action"),
			TargetType: q.Get("target_type"),
			Mask:       q.Get("mask") == "1" || q.Get("mask") == "true",
		}
		var err error
		if raw := q.Get("admin_user_id"); raw != "" {
			f.ActorID, err = strconv.ParseInt(raw, 10, 64)
			if err != nil {
				WriteError(w, "INVALID_REQUEST", "admin_user_id must be an integer",
					gateway.RequestIDFrom(r.Context()), nil)
				return
			}
		}
		if raw := q.Get("target_id"); raw != "" {
			f.TargetID, err = strconv.ParseInt(raw, 10, 64)
			if err != nil {
				WriteError(w, "INVALID_REQUEST", "target_id must be an integer",
					gateway.RequestIDFrom(r.Context()), nil)
				return
			}
		}
		if raw := q.Get("after_id"); raw != "" {
			f.AfterID, _ = strconv.ParseInt(raw, 10, 64)
		}
		var t *time.Time
		if t, err = parseTimeQuery(q.Get("from")); err != nil {
			WriteError(w, "INVALID_REQUEST", "from must be RFC3339",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		if t != nil {
			f.From = *t
		}
		if t, err = parseTimeQuery(q.Get("to")); err != nil {
			WriteError(w, "INVALID_REQUEST", "to must be RFC3339",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		if t != nil {
			f.To = *t
		}
		f.Limit, _ = strconv.Atoi(q.Get("limit"))
		rows, err := svc.Search(r.Context(), f)
		if err != nil {
			writeSvcErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{
			"rows": rows, "masked": f.Mask,
			"proof": "audit_hash_chain anchor per row (chain_seq/chain_hash)",
		})
	}
}

// AdminReportingValuesList — GET /api/v1/admin/reporting-values?scope=.
func AdminReportingValuesList(svc *compliance.ReportingValues) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		out, err := svc.List(r.Context(), r.URL.Query().Get("scope"), limit)
		if err != nil {
			writeSvcErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"reporting_values": out})
	}
}

// AdminReportingValuesSet — POST /api/v1/admin/reporting-values
// body {"scope","key","value","source"} — upsert (audited).
func AdminReportingValuesSet(svc *compliance.ReportingValues,
	trustProxy bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		actor, err := adminActorFrom(r, trustProxy)
		if err != nil {
			writeSvcErr(w, r, err)
			return
		}
		var body struct {
			Scope  string          `json:"scope"`
			Key    string          `json:"key"`
			Value  json.RawMessage `json:"value"`
			Source string          `json:"source"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			WriteError(w, "INVALID_REQUEST", "malformed body",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		v, err := svc.Set(r.Context(), actor.UserID, body.Scope,
			body.Key, body.Value, body.Source)
		if err != nil {
			writeSvcErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"reporting_value": v})
	}
}

// AdminAuditChain — GET /api/v1/admin/audit/chain — raw
// audit_hash_chain slice (table_name/action/from/to filters) for the
// WORM-integrity lane. Read-Only Auditor+.
func AdminAuditChain(svc *compliance.AuditTrailQuery,
	resolver AdminRoleResolver) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, ok := requireAuditRole(w, r, resolver); !ok {
			return
		}
		q := r.URL.Query()
		var from, to time.Time
		t, err := parseTimeQuery(q.Get("from"))
		if err != nil {
			WriteError(w, "INVALID_REQUEST", "from must be RFC3339",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		if t != nil {
			from = *t
		}
		t, err = parseTimeQuery(q.Get("to"))
		if err != nil {
			WriteError(w, "INVALID_REQUEST", "to must be RFC3339",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		if t != nil {
			to = *t
		}
		afterSeq, _ := strconv.ParseInt(q.Get("after_seq"), 10, 64)
		limit, _ := strconv.Atoi(q.Get("limit"))
		rows, err := svc.ChainSlice(r.Context(), q.Get("table_name"),
			q.Get("action"), from, to, afterSeq, limit)
		if err != nil {
			writeSvcErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"chain": rows})
	}
}
