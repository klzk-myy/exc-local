// Phase-21 Tasks 21.3.2/21.3.3/21.3.6 — admin endpoints for the FATF
// travel-rule record surface, the SAR filing lifecycle (four-eyes on
// approve/file) and the FinCEN MSB program register + CTR/AML
// monitoring views. Role gates ride the route registry (Compliance
// Officer for mutations, Read-Only Auditor for reads); the service
// re-checks via the role resolver — fail-closed on both layers.
package api

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"exchange/internal/compliance"
	"exchange/internal/gateway"
)

// ---------------------------------------------------------------------------
// SAR lifecycle — Task 21.3.3
// ---------------------------------------------------------------------------

// AdminSARCreate serves POST /api/v1/admin/sar — manual SAR draft
// (Compliance Officer+). Machine triggers draft via the service seams
// (surveillance ingest, hold escalation, AML rules).
func AdminSARCreate(svc *compliance.SARService, trustProxy bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		actor, err := adminActorFrom(r, trustProxy)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		var body struct {
			AccountID      *int64          `json:"account_id"`
			SubjectRef     string          `json:"subject_ref"`
			Description    string          `json:"description"`
			Evidence       json.RawMessage `json:"evidence"`
			TransactionIDs []int64         `json:"transaction_ids"`
			SourceRef      string          `json:"source_ref"`
			DetectedAt     *time.Time      `json:"detected_at"`
			AmendsID       *int64          `json:"amends_id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			WriteError(w, "INVALID_REQUEST", "malformed body",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		in := compliance.SARDraftInput{
			TriggerType:    compliance.SARTriggerManual,
			AccountID:      body.AccountID,
			SubjectRef:     body.SubjectRef,
			Description:    body.Description,
			TransactionIDs: body.TransactionIDs,
			SourceRef:      body.SourceRef,
			CreatedBy:      &actor.UserID,
			AmendsID:       body.AmendsID,
		}
		if len(body.Evidence) > 0 {
			in.Evidence = body.Evidence
		}
		if body.DetectedAt != nil {
			in.DetectedAt = *body.DetectedAt
		}
		rec, created, err := svc.Draft(r.Context(), in)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		code := http.StatusCreated
		if !created {
			code = http.StatusOK // source_ref dedup replay
		}
		WriteJSON(w, code, map[string]any{"sar": rec, "created": created})
	}
}

// AdminSARList serves GET /api/v1/admin/sar?status=&limit= — the
// officer queue ordered by filing deadline.
func AdminSARList(svc *compliance.SARService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		limit := 200
		if q := r.URL.Query().Get("limit"); q != "" {
			if n, err := strconv.Atoi(q); err == nil {
				limit = n
			}
		}
		recs, err := svc.List(r.Context(), r.URL.Query().Get("status"), limit)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		if recs == nil {
			recs = []compliance.SARReport{}
		}
		WriteJSON(w, http.StatusOK, map[string]any{"reports": recs})
	}
}

// AdminSARGet serves GET /api/v1/admin/sar/{id}.
func AdminSARGet(svc *compliance.SARService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil || id <= 0 {
			WriteError(w, "INVALID_REQUEST", "bad sar id",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		rec, err := svc.Get(r.Context(), id)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"sar": rec})
	}
}

// AdminSARReview serves POST /api/v1/admin/sar/{id}/review —
// DRAFT → UNDER_REVIEW with reviewer attribution.
func AdminSARReview(svc *compliance.SARService, trustProxy bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		actor, err := adminActorFrom(r, trustProxy)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		var body struct {
			Note string `json:"note"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body) // note optional
		rec, err := svc.Review(r.Context(), sarIDFrom(r), actor.UserID, body.Note)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"sar": rec})
	}
}

// AdminSARApprove serves POST /api/v1/admin/sar/{id}/approve — the
// second-officer approval; the service rejects same-principal action
// with SAR_DUAL_CONTROL_REQUIRED.
func AdminSARApprove(svc *compliance.SARService, trustProxy bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		actor, err := adminActorFrom(r, trustProxy)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		var body struct {
			Note string `json:"note"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		rec, err := svc.Approve(r.Context(), sarIDFrom(r), actor.UserID, body.Note)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"sar": rec})
	}
}

// AdminSARFile serves POST /api/v1/admin/sar/{id}/file — records the
// FinCEN submission (filing_ref mandatory); the row is immutable after.
func AdminSARFile(svc *compliance.SARService, trustProxy bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		actor, err := adminActorFrom(r, trustProxy)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		var body struct {
			FilingRef string `json:"filing_ref"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			WriteError(w, "INVALID_REQUEST", "malformed body",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		rec, err := svc.File(r.Context(), sarIDFrom(r), actor.UserID, body.FilingRef)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"sar": rec})
	}
}

// AdminSARReject serves POST /api/v1/admin/sar/{id}/reject — the
// false-positive disposition (reason mandatory).
func AdminSARReject(svc *compliance.SARService, trustProxy bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		actor, err := adminActorFrom(r, trustProxy)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		var body struct {
			Reason string `json:"reason"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			WriteError(w, "INVALID_REQUEST", "malformed body",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		rec, err := svc.Reject(r.Context(), sarIDFrom(r), actor.UserID, body.Reason)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"sar": rec})
	}
}

func sarIDFrom(r *http.Request) int64 {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	return id
}

// ---------------------------------------------------------------------------
// Travel rule — Task 21.3.2
// ---------------------------------------------------------------------------

// AdminTravelRuleList serves GET /api/v1/admin/travel-rule?status= —
// the missing-info work queue + audit surface.
func AdminTravelRuleList(svc *compliance.TravelRuleService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		limit := 200
		if q := r.URL.Query().Get("limit"); q != "" {
			if n, err := strconv.Atoi(q); err == nil {
				limit = n
			}
		}
		recs, err := svc.List(r.Context(), r.URL.Query().Get("status"), limit)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		if recs == nil {
			recs = []compliance.TravelRuleRecord{}
		}
		WriteJSON(w, http.StatusOK, map[string]any{"records": recs})
	}
}

// AdminTravelRuleGet serves GET /api/v1/admin/travel-rule/{id}.
func AdminTravelRuleGet(svc *compliance.TravelRuleService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil || id <= 0 {
			WriteError(w, "INVALID_REQUEST", "bad record id",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		rec, err := svc.Get(r.Context(), id)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"record": rec})
	}
}

// AdminTravelRuleSupply serves POST /api/v1/admin/travel-rule/{id}/supply
// — the officer supplies absent originator/beneficiary fields; the
// record re-validates and the held transfer re-evaluates on the next
// dispatch sweep.
func AdminTravelRuleSupply(svc *compliance.TravelRuleService, trustProxy bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		actor, err := adminActorFrom(r, trustProxy)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		var body compliance.TravelRuleSupply
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			WriteError(w, "INVALID_REQUEST", "malformed body",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil || id <= 0 {
			WriteError(w, "INVALID_REQUEST", "bad record id",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		rec, err := svc.SupplyInfo(r.Context(), id, actor.UserID, body)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"record": rec})
	}
}

// ---------------------------------------------------------------------------
// FinCEN MSB / AML — Task 21.3.6
// ---------------------------------------------------------------------------

// AdminCTRList serves GET /api/v1/admin/ctr?status= — Currency
// Transaction Report triggers.
func AdminCTRList(svc *compliance.AMLService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		limit := 200
		if q := r.URL.Query().Get("limit"); q != "" {
			if n, err := strconv.Atoi(q); err == nil {
				limit = n
			}
		}
		recs, err := svc.ListCTR(r.Context(), r.URL.Query().Get("status"), limit)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		if recs == nil {
			recs = []compliance.CTRReport{}
		}
		WriteJSON(w, http.StatusOK, map[string]any{"reports": recs})
	}
}

// AdminAMLMonitoring serves GET /api/v1/admin/aml/monitoring?account_id=
// — the rule-detection feed (structuring events, scores).
func AdminAMLMonitoring(svc *compliance.AMLService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		limit := 200
		if q := r.URL.Query().Get("limit"); q != "" {
			if n, err := strconv.Atoi(q); err == nil {
				limit = n
			}
		}
		var accountID int64
		if q := r.URL.Query().Get("account_id"); q != "" {
			if n, err := strconv.ParseInt(q, 10, 64); err == nil {
				accountID = n
			}
		}
		evs, err := svc.ListMonitoring(r.Context(), accountID, limit)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		if evs == nil {
			evs = []compliance.MonitoringEvent{}
		}
		WriteJSON(w, http.StatusOK, map[string]any{"events": evs})
	}
}

// AdminAMLArtifactList serves GET /api/v1/admin/aml/artifacts?type=.
func AdminAMLArtifactList(svc *compliance.AMLService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		limit := 200
		if q := r.URL.Query().Get("limit"); q != "" {
			if n, err := strconv.Atoi(q); err == nil {
				limit = n
			}
		}
		recs, err := svc.ListArtifacts(r.Context(),
			r.URL.Query().Get("type"), limit)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		if recs == nil {
			recs = []compliance.AMLArtifact{}
		}
		WriteJSON(w, http.StatusOK, map[string]any{"artifacts": recs})
	}
}

// AdminAMLArtifactRegister serves POST /api/v1/admin/aml/artifacts —
// files a program artifact (MSB registration, policy version, training
// log, officer designation, annual review).
func AdminAMLArtifactRegister(svc *compliance.AMLService, resolver compliance.HoldRoleResolver,
	trustProxy bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		actor, err := adminActorFrom(r, trustProxy)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		var body compliance.ArtifactInput
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			WriteError(w, "INVALID_REQUEST", "malformed body",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		rec, err := svc.RegisterArtifact(r.Context(), actor.UserID, resolver, body)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusCreated, map[string]any{"artifact": rec})
	}
}

// AdminAMLProgramStatus serves GET /api/v1/admin/aml/program — the
// mandatory-artifact health check; breaches carry MSB_COMPLIANCE_BREACH.
func AdminAMLProgramStatus(svc *compliance.AMLService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		st, err := svc.ProgramStatus(r.Context())
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"program": st})
	}
}
