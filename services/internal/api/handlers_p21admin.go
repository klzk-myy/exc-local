// handlers_p21admin.go — Phase-21 wave-2 admin surfaces:
//
//	GET  /api/v1/admin/comms-recordings                  — register list (Task 21.3.20)
//	GET  /api/v1/admin/comms-recordings/{id}             — register row
//	POST /api/v1/admin/comms-recordings/{id}/retrieve    — dual-control content retrieval (DC)
//	POST /api/v1/admin/comms-recordings/verify-day       — day-chain recompute check
//	GET  /api/v1/admin/tax-reporting/runs                — CRS/FATCA run list (Task 21.3.22)
//	POST /api/v1/admin/tax-reporting/runs                — generate a run
//	GET  /api/v1/admin/tax-reporting/runs/{id}           — run detail
//	GET  /api/v1/admin/tax-reporting/runs/{id}/xml       — artifact download
//	POST /api/v1/admin/tax-reporting/runs/{id}/review    — DRAFT→UNDER_REVIEW
//	POST /api/v1/admin/tax-reporting/runs/{id}/approve   — UNDER_REVIEW→APPROVED (DC)
//	POST /api/v1/admin/tax-reporting/runs/{id}/reject    — REJECTED + reason
//	POST /api/v1/admin/tax-reporting/runs/{id}/submit    — APPROVED→SUBMITTED
//	GET  /api/v1/admin/promotions                        — list (Task 21.3.26)
//	POST /api/v1/admin/promotions                        — create draft
//	PUT  /api/v1/admin/promotions                        — revise (new version, slug in body)
//	POST /api/v1/admin/promotions/{id}/submit            — DRAFT→PENDING_REVIEW
//	POST /api/v1/admin/promotions/{id}/approve           — approval (DC)
//	POST /api/v1/admin/promotions/{id}/reject            — reject + reason
//	POST /api/v1/admin/promotions/{id}/withdraw          — withdraw APPROVED
//	GET  /api/v1/admin/data-residency/policies           — policy map (Task 21.3.18)
//	GET  /api/v1/admin/data-residency/access-log         — cross-border audit
//	POST /api/v1/admin/accounts/{id}/jurisdiction        — pin residency tag
//
// Role gates ride the route registry (Compliance Officer / Read-Only
// Auditor); identity resolves via adminActorFrom / IdentityFrom.
package api

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"exchange/internal/admin"
	"exchange/internal/compliance"
	"exchange/internal/gateway"
)

func p21AdminActor(r *http.Request) (int64, error) {
	actor, err := adminActorFrom(r, false)
	if err != nil {
		return 0, err
	}
	return actor.UserID, nil
}

// ---------------------------------------------------------------------------
// Task 21.3.20 — communications recordings
// ---------------------------------------------------------------------------

// AdminCommsList serves GET /api/v1/admin/comms-recordings.
func AdminCommsList(svc *compliance.CommsRecordingService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var acct *int64
		if raw := r.URL.Query().Get("account_id"); raw != "" {
			v, err := strconv.ParseInt(raw, 10, 64)
			if err != nil || v <= 0 {
				WriteError(w, "INVALID_REQUEST", "account_id must be a positive integer",
					gateway.RequestIDFrom(r.Context()), nil)
				return
			}
			acct = &v
		}
		limit := parseLimitQuery(r.URL.Query().Get("limit"), 100, 500)
		list, err := svc.List(r.Context(), acct, limit)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"recordings": list})
	}
}

// AdminCommsGet serves GET /api/v1/admin/comms-recordings/{id}.
func AdminCommsGet(svc *compliance.CommsRecordingService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := lpPathID(r)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		rec, err := svc.Get(r.Context(), id)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"recording": rec})
	}
}

// AdminCommsRetrieve serves POST /api/v1/admin/comms-recordings/{id}/retrieve —
// dual-control content retrieval. Body: {"approver_id":N,
// "justification":"...","case_ref":"..."}.
func AdminCommsRetrieve(svc *compliance.CommsRecordingService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		uid, err := p21AdminActor(r)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		id, err := lpPathID(r)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		var in struct {
			ApproverID    int64  `json:"approver_id"`
			Justification string `json:"justification"`
			CaseRef       string `json:"case_ref"`
		}
		if !decodeJSONBody(w, r, &in) {
			return
		}
		if in.ApproverID <= 0 {
			WriteError(w, "DUAL_CONTROL_REQUIRED",
				"approver_id of a distinct second officer is required",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		out, err := svc.Retrieve(r.Context(), id, uid, in.ApproverID,
			in.Justification, in.CaseRef)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, out)
	}
}

// AdminCommsVerifyDay serves POST /api/v1/admin/comms-recordings/verify-day —
// recomputes one UTC day's register chain. Body: {"day":"YYYY-MM-DD"}.
func AdminCommsVerifyDay(svc *compliance.CommsRecordingService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Day string `json:"day"`
		}
		if !decodeJSONBody(w, r, &in) {
			return
		}
		day, err := time.Parse("2006-01-02", strings.TrimSpace(in.Day))
		if err != nil {
			WriteError(w, "INVALID_REQUEST", "day must be YYYY-MM-DD",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		ok, err := svc.VerifyDay(r.Context(), day)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{
			"day": in.Day, "chain_ok": ok,
		})
	}
}

// ---------------------------------------------------------------------------
// Task 21.3.22 — CRS/FATCA runs
// ---------------------------------------------------------------------------

// AdminTaxRunList serves GET /api/v1/admin/tax-reporting/runs.
func AdminTaxRunList(svc *compliance.TaxReportService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		limit := parseLimitQuery(r.URL.Query().Get("limit"), 100, 500)
		list, err := svc.List(r.Context(),
			r.URL.Query().Get("regime"), limit)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"runs": list})
	}
}

// AdminTaxRunGenerate serves POST /api/v1/admin/tax-reporting/runs.
func AdminTaxRunGenerate(svc *compliance.TaxReportService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		uid, err := p21AdminActor(r)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		var in struct {
			Regime       string `json:"regime"`
			ReportYear   int    `json:"report_year"`
			Jurisdiction string `json:"jurisdiction"`
		}
		if !decodeJSONBody(w, r, &in) {
			return
		}
		run, err := svc.Generate(r.Context(), in.Regime, in.ReportYear,
			in.Jurisdiction, uid)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusCreated, map[string]any{"run": run})
	}
}

// AdminTaxRunGet serves GET /api/v1/admin/tax-reporting/runs/{id}.
func AdminTaxRunGet(svc *compliance.TaxReportService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := lpPathID(r)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		run, err := svc.Get(r.Context(), id)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"run": run})
	}
}

// AdminTaxRunXML serves GET .../runs/{id}/xml — the artifact bytes.
func AdminTaxRunXML(svc *compliance.TaxReportService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := lpPathID(r)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		body, err := svc.Artifact(r.Context(), id)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	}
}

// AdminTaxRunReview serves POST .../runs/{id}/review.
func AdminTaxRunReview(svc *compliance.TaxReportService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p21TaxTransition(w, r, func(id int64, uid int64) (*compliance.TaxReportRun, error) {
			return svc.Review(r.Context(), id, uid)
		})
	}
}

// AdminTaxRunApprove serves POST .../runs/{id}/approve (dual control).
func AdminTaxRunApprove(svc *compliance.TaxReportService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p21TaxTransition(w, r, func(id int64, uid int64) (*compliance.TaxReportRun, error) {
			return svc.Approve(r.Context(), id, uid)
		})
	}
}

// AdminTaxRunReject serves POST .../runs/{id}/reject — {"reason":...}.
func AdminTaxRunReject(svc *compliance.TaxReportService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Reason string `json:"reason"`
		}
		if !decodeJSONBody(w, r, &in) {
			return
		}
		p21TaxTransition(w, r, func(id int64, uid int64) (*compliance.TaxReportRun, error) {
			return svc.Reject(r.Context(), id, uid, in.Reason)
		})
	}
}

// AdminTaxRunSubmit serves POST .../runs/{id}/submit —
// {"submission_ref":"..."} authority receipt.
func AdminTaxRunSubmit(svc *compliance.TaxReportService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			SubmissionRef string `json:"submission_ref"`
		}
		if !decodeJSONBody(w, r, &in) {
			return
		}
		p21TaxTransition(w, r, func(id int64, uid int64) (*compliance.TaxReportRun, error) {
			return svc.Submit(r.Context(), id, uid, in.SubmissionRef)
		})
	}
}

// p21TaxTransition resolves actor+path id then runs the transition.
func p21TaxTransition(w http.ResponseWriter, r *http.Request,
	fn func(runID, uid int64) (*compliance.TaxReportRun, error)) {
	uid, err := p21AdminActor(r)
	if err != nil {
		writeServiceErr(w, r, err)
		return
	}
	id, err := lpPathID(r)
	if err != nil {
		writeServiceErr(w, r, err)
		return
	}
	run, err := fn(id, uid)
	if err != nil {
		writeServiceErr(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"run": run})
}

// ---------------------------------------------------------------------------
// Task 21.3.26 — financial promotions admin
// ---------------------------------------------------------------------------

// AdminPromotionsList serves GET /api/v1/admin/promotions.
func AdminPromotionsList(svc *compliance.PromotionService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		limit := parseLimitQuery(r.URL.Query().Get("limit"), 100, 500)
		list, err := svc.List(r.Context(),
			r.URL.Query().Get("status"), limit)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"promotions": list})
	}
}

// AdminPromotionsCreate serves POST /api/v1/admin/promotions.
func AdminPromotionsCreate(svc *compliance.PromotionService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		uid, err := p21AdminActor(r)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		var in compliance.PromotionInput
		if !decodeJSONBody(w, r, &in) {
			return
		}
		p, err := svc.Create(r.Context(), uid, in)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusCreated, map[string]any{"promotion": p})
	}
}

// AdminPromotionsRevise serves PUT /api/v1/admin/promotions — slug in
// body, lands a new version (same seam as create by design).
func AdminPromotionsRevise(svc *compliance.PromotionService) http.HandlerFunc {
	return AdminPromotionsCreate(svc)
}

// AdminPromotionTransition wires the {id}-keyed verbs.
func p21PromotionTransition(w http.ResponseWriter, r *http.Request,
	fn func(promotionID, uid int64) (*compliance.Promotion, error)) {
	uid, err := p21AdminActor(r)
	if err != nil {
		writeServiceErr(w, r, err)
		return
	}
	id, err := lpPathID(r)
	if err != nil {
		writeServiceErr(w, r, err)
		return
	}
	p, err := fn(id, uid)
	if err != nil {
		writeServiceErr(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"promotion": p})
}

// AdminPromotionSubmit serves POST .../promotions/{id}/submit.
func AdminPromotionSubmit(svc *compliance.PromotionService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p21PromotionTransition(w, r, func(id, uid int64) (*compliance.Promotion, error) {
			return svc.SubmitForReview(r.Context(), id, uid)
		})
	}
}

// AdminPromotionApprove serves POST .../promotions/{id}/approve —
// dual-control flagged route; claims promotions additionally carry a
// distinct second_approver_id. Body:
// {"checklist":{"risk_warning":true,...},"second_approver_id":N,
//
//	"approved_until":"RFC3339"}.
func AdminPromotionApprove(svc *compliance.PromotionService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		uid, err := p21AdminActor(r)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		id, err := lpPathID(r)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		var in struct {
			Checklist        compliance.PromoChecklist `json:"checklist"`
			SecondApproverID *int64                    `json:"second_approver_id"`
			ApprovedUntil    *time.Time                `json:"approved_until"`
		}
		if !decodeJSONBody(w, r, &in) {
			return
		}
		p, err := svc.Approve(r.Context(), id, uid, in.Checklist,
			in.SecondApproverID, in.ApprovedUntil)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"promotion": p})
	}
}

// AdminPromotionReject serves POST .../promotions/{id}/reject.
func AdminPromotionReject(svc *compliance.PromotionService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Reason string `json:"reason"`
		}
		if !decodeJSONBody(w, r, &in) {
			return
		}
		p21PromotionTransition(w, r, func(id, uid int64) (*compliance.Promotion, error) {
			return svc.Reject(r.Context(), id, uid, in.Reason)
		})
	}
}

// AdminPromotionWithdraw serves POST .../promotions/{id}/withdraw.
func AdminPromotionWithdraw(svc *compliance.PromotionService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Reason string `json:"reason"`
		}
		if !decodeOptionalJSONBody(w, r, &in) {
			return
		}
		p21PromotionTransition(w, r, func(id, uid int64) (*compliance.Promotion, error) {
			return svc.Withdraw(r.Context(), id, uid, in.Reason)
		})
	}
}

// ---------------------------------------------------------------------------
// Task 21.3.18 — data residency admin
// ---------------------------------------------------------------------------

// AdminResidencyPolicies serves GET /api/v1/admin/data-residency/policies.
func AdminResidencyPolicies(svc *compliance.ResidencyService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		list, err := svc.Policies(r.Context())
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"policies": list})
	}
}

// AdminResidencyAccessLog serves GET /api/v1/admin/data-residency/access-log.
func AdminResidencyAccessLog(svc *compliance.ResidencyService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		limit := parseLimitQuery(r.URL.Query().Get("limit"), 100, 500)
		list, err := svc.AccessLog(r.Context(), limit)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"access_log": list})
	}
}

// AdminAccountJurisdictionPin serves POST /api/v1/admin/accounts/{id}/jurisdiction —
// pins the residency tag. The cross-border rule applies to the pin
// itself (a foreign-region admin needs justification).
func AdminAccountJurisdictionPin(svc *compliance.ResidencyService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		uid, err := p21AdminActor(r)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		acct, err := lpPathID(r)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		var in struct {
			JurisdictionCode string `json:"jurisdiction_code"`
			Justification    string `json:"justification"`
		}
		if !decodeJSONBody(w, r, &in) {
			return
		}
		// Cross-border gate on the access itself — regions from the
		// caller's active bindings.
		var regions []string
		if id := admin.IdentityFrom(r.Context()); id != nil {
			for _, b := range id.Bindings {
				if b.Scope != nil {
					regions = append(regions, b.Scope.Regions...)
				}
			}
		}
		if _, err := svc.AuthorizeAccess(r.Context(), uid, regions, acct,
			"PIN", in.Justification); err != nil {
			writeServiceErr(w, r, err)
			return
		}
		pol, err := svc.PinAccount(r.Context(), acct, in.JurisdictionCode,
			strconv.FormatInt(uid, 10))
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"policy": pol})
	}
}
