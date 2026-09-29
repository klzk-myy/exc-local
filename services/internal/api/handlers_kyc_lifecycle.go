// Phase-14 Task 14.3.4 — KYC lifecycle admin surface, and Task 14.3.7 —
// MiFID II client categorization surface.
//
//	POST /api/v1/admin/kyc/{id}/approve             — Compliance Officer
//	                                                  decision (single
//	                                                  write path to
//	                                                  accounts.kyc_tier)
//	POST /api/v1/admin/kyc/{id}/reject              — decision + reason
//	PUT  /api/v1/admin/accounts/{id}/product-profile — client_category
//	                                                  assignment (evidence
//	                                                  required on upgrade)
//	POST /api/v1/account/appropriateness            — submit assessment
//	GET  /api/v1/account/appropriateness            — own category, NBP
//	                                                  flag + assessments
//
// The role gate lives in the services (compliance.LifecycleService /
// compliance.CategorizationService resolve the actor's role through the
// Phase-07 binding store); the route registry additionally declares the
// Compliance-Officer auth spec so unauthorized calls never reach the
// handler.
package api

import (
	"encoding/json"
	"net/http"
	"strconv"

	"exchange/internal/auth"
	"exchange/internal/compliance"
	"exchange/internal/gateway"
	"exchange/internal/middleware"
)

// kycAdminActor resolves the acting admin's user id from claims.
func kycAdminActor(w http.ResponseWriter, r *http.Request, trustProxy bool) (compliance.ReviewActor, bool) {
	claims := auth.ClaimsFrom(r.Context())
	if claims == nil {
		WriteError(w, "UNAUTHORIZED", "authentication required",
			gateway.RequestIDFrom(r.Context()), nil)
		return compliance.ReviewActor{}, false
	}
	id, err := strconv.ParseInt(claims.Subject, 10, 64)
	if err != nil || id <= 0 {
		WriteError(w, "UNAUTHORIZED", "admin identity unresolvable",
			gateway.RequestIDFrom(r.Context()), nil)
		return compliance.ReviewActor{}, false
	}
	return compliance.ReviewActor{
		AdminUserID: id,
		ClientIP:    middleware.ClientIP(r, trustProxy),
	}, true
}

// kycPathID parses a positive-int64 path value; failure writes
// INVALID_REQUEST (fail closed — malformed ids never reach the store).
func kycPathID(w http.ResponseWriter, r *http.Request, name string) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue(name), 10, 64)
	if err != nil || id <= 0 {
		WriteError(w, "INVALID_REQUEST", "invalid "+name,
			gateway.RequestIDFrom(r.Context()), nil)
		return 0, false
	}
	return id, true
}

// KYCApproveHandler is POST /api/v1/admin/kyc/{id}/approve — Task
// 14.3.4. The service owns role resolution, the serializable decision
// tx (submission → APPROVED + tier assign + reverify horizon + audit)
// and the post-commit kyc_approved notification.
func KYCApproveHandler(svc *compliance.LifecycleService, trustProxy bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			WriteError(w, "SERVICE_DEGRADED", "kyc lifecycle service unavailable",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		actor, ok := kycAdminActor(w, r, trustProxy)
		if !ok {
			return
		}
		id, ok := kycPathID(w, r, "id")
		if !ok {
			return
		}
		res, err := svc.Approve(r.Context(), actor, id)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, res)
	}
}

// kycRejectBody is the POST /api/v1/admin/kyc/{id}/reject contract —
// reason is mandatory and lands in kyc_submissions.reject_reason.
type kycRejectBody struct {
	Reason string `json:"reason"`
}

// KYCRejectHandler is POST /api/v1/admin/kyc/{id}/reject.
func KYCRejectHandler(svc *compliance.LifecycleService, trustProxy bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			WriteError(w, "SERVICE_DEGRADED", "kyc lifecycle service unavailable",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		actor, ok := kycAdminActor(w, r, trustProxy)
		if !ok {
			return
		}
		id, ok := kycPathID(w, r, "id")
		if !ok {
			return
		}
		var body kycRejectBody
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			WriteError(w, "INVALID_REQUEST", "malformed body",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		res, err := svc.Reject(r.Context(), actor, id, body.Reason)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, res)
	}
}

// ---------------------------------------------------------------------------
// Task 14.3.7 — appropriateness + client categorization
// ---------------------------------------------------------------------------

// appropriatenessBody is the POST /api/v1/account/appropriateness
// contract: {instrument_class, score, answers?}. Outcome derives
// server-side from the pass mark — a client can never self-declare
// PASS.
type appropriatenessBody struct {
	InstrumentClass string          `json:"instrument_class"`
	Score           int             `json:"score"`
	Answers         json.RawMessage `json:"answers,omitempty"`
}

// AppropriatenessSubmit is POST /api/v1/account/appropriateness — the
// caller's own assessment for one gated class (FORWARD|SWAP|NDF|OPTION;
// SPOT is exempt so no row is accepted for it). expiry = 12 months.
func AppropriatenessSubmit(svc *compliance.CategorizationService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			WriteError(w, "SERVICE_DEGRADED", "categorization service unavailable",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		accountID, _, ok := claimsAccount(w, r)
		if !ok {
			return
		}
		var body appropriatenessBody
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			WriteError(w, "INVALID_REQUEST", "malformed body",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		a, err := svc.SubmitAssessment(r.Context(), compliance.AssessmentInput{
			AccountID:       accountID,
			InstrumentClass: body.InstrumentClass,
			Score:           body.Score,
			Answers:         body.Answers,
		})
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusCreated, a)
	}
}

// AppropriatenessStatus is GET /api/v1/account/appropriateness — the
// caller's client_category, nbp entitlement and assessment history
// (newest first; answers stay server-side).
func AppropriatenessStatus(svc *compliance.CategorizationService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			WriteError(w, "SERVICE_DEGRADED", "categorization service unavailable",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		accountID, _, ok := claimsAccount(w, r)
		if !ok {
			return
		}
		st, err := svc.Status(r.Context(), accountID)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, st)
	}
}

// productProfileBody is the PUT
// /api/v1/admin/accounts/{id}/product-profile contract — the registered
// route's category-assignment semantics (Task 14.3.7 item 2):
// {client_category, evidence}. Upgrades to PROFESSIONAL /
// ELIGIBLE_COUNTERPARTY require the MiFID II Annex II evidence.
type productProfileBody struct {
	ClientCategory string `json:"client_category"`
	Evidence       string `json:"evidence"`
}

// AdminClientCategory assigns accounts.client_category — Compliance
// Officer workflow, every change audit-logged inside the write tx.
func AdminClientCategory(svc *compliance.CategorizationService, trustProxy bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			WriteError(w, "SERVICE_DEGRADED", "categorization service unavailable",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		actor, ok := kycAdminActor(w, r, trustProxy)
		if !ok {
			return
		}
		id, ok := kycPathID(w, r, "id")
		if !ok {
			return
		}
		var body productProfileBody
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			WriteError(w, "INVALID_REQUEST", "malformed body",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		ch, err := svc.SetCategory(r.Context(), actor, id,
			body.ClientCategory, body.Evidence)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, ch)
	}
}
