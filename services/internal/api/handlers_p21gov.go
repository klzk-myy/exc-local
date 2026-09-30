// Phase-21 governance/assurance admin surface — Tasks 21.3.13
// (Basel III reporting), 21.3.17 (FX Global Code 55-principle
// self-assessment + Statement of Commitment), 21.3.25 (regulatory
// change watch register + impact assessment) and 21.3.28 (order
// execution policy publication, consent & annual review).
//
// Role gates ride the route registry AND re-check inside the services
// (Compliance Officer mutations; the regulatory-change read surface is
// scoped for the Read-Only Auditor). The public execution-policy read
// stays unauthenticated; the account consent route resolves account
// identity from the caller's own claims — never a body-supplied
// account_id.
package api

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"exchange/internal/compliance"
	"exchange/internal/gateway"
	"exchange/internal/middleware"

	excerrors "exchange/pkg/errors"
)

// ---------------------------------------------------------------------------
// Basel III — Task 21.3.13
// ---------------------------------------------------------------------------

// AdminBaselReport serves GET /api/v1/admin/basel-report?period= —
// newest stored report version for the period (?period=YYYY-MM-DD or
// YYYY-MM; empty → latest). ?versions=1 returns the version history
// (the audit/export surface); a breach flag or inputs_complete=false
// rides the payload's `code` — never a silent green.
func AdminBaselReport(svc *compliance.BaselService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			WriteError(w, "SERVICE_DEGRADED", "basel service unavailable",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		if r.URL.Query().Get("versions") == "1" ||
			r.URL.Query().Get("versions") == "true" {
			limit := 200
			if q := r.URL.Query().Get("limit"); q != "" {
				if n, err := strconv.Atoi(q); err == nil {
					limit = n
				}
			}
			reps, err := svc.List(r.Context(), limit)
			if err != nil {
				writeServiceErr(w, r, err)
				return
			}
			if reps == nil {
				reps = []compliance.BaselReport{}
			}
			WriteJSON(w, http.StatusOK, map[string]any{"reports": reps})
			return
		}
		var period time.Time
		if q := strings.TrimSpace(r.URL.Query().Get("period")); q != "" {
			var err error
			switch len(q) {
			case 10: // YYYY-MM-DD
				period, err = time.Parse("2006-01-02", q)
			case 7: // YYYY-MM → first of month
				period, err = time.Parse("2006-01", q)
			default:
				err = strconv.ErrSyntax
			}
			if err != nil {
				WriteError(w, "INVALID_REQUEST",
					"period must be YYYY-MM-DD or YYYY-MM",
					gateway.RequestIDFrom(r.Context()), nil)
				return
			}
		}
		rep, err := svc.Get(r.Context(), period)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"report": rep})
	}
}

// ---------------------------------------------------------------------------
// FX Global Code — Task 21.3.17
// ---------------------------------------------------------------------------

// AdminFXGCStart serves POST /api/v1/admin/fx-global-code/assessments —
// opens an annual review run ({period, code_version?}); the 55-row
// matrix seeds and the automated probes (P9/P10/P17/P50) evaluate
// immediately. Idempotent on (framework, period).
func AdminFXGCStart(svc *compliance.FXGCService, trustProxy bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		actor, err := adminActorFrom(r, trustProxy)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		var body struct {
			Period      string `json:"period"`
			CodeVersion string `json:"code_version"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			WriteError(w, "INVALID_REQUEST", "malformed body",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		run, created, err := svc.StartRun(r.Context(), body.Period,
			body.CodeVersion, actor.UserID)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		code := http.StatusCreated
		if !created {
			code = http.StatusOK
		}
		WriteJSON(w, code, map[string]any{"run": run, "created": created})
	}
}

// AdminFXGCList serves GET /api/v1/admin/fx-global-code/assessments.
func AdminFXGCList(svc *compliance.FXGCService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		limit := 50
		if q := r.URL.Query().Get("limit"); q != "" {
			if n, err := strconv.Atoi(q); err == nil {
				limit = n
			}
		}
		runs, err := svc.ListRuns(r.Context(), limit)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		if runs == nil {
			runs = []compliance.AssessmentRun{}
		}
		WriteJSON(w, http.StatusOK, map[string]any{"runs": runs})
	}
}

// AdminFXGCGet serves GET /api/v1/admin/fx-global-code/assessments/{id}
// — the run plus the full 55-principle matrix and, once generated, the
// Statement of Commitment metadata.
func AdminFXGCGet(svc *compliance.FXGCService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		runID, ok := fxgcRunID(w, r)
		if !ok {
			return
		}
		run, err := svc.Export(r.Context(), runID)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"assessment": run})
	}
}

// AdminFXGCVerdict serves POST
// /api/v1/admin/fx-global-code/assessments/{id}/verdicts — records the
// officer's verdict on one principle ({principle_id, adherence_status,
// evidence_summary?, remediation_ref?}); PARTIAL/NON_ADHERENT require
// the remediation ticket.
func AdminFXGCVerdict(svc *compliance.FXGCService, trustProxy bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		actor, err := adminActorFrom(r, trustProxy)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		var body struct {
			PrincipleID     int    `json:"principle_id"`
			AdherenceStatus string `json:"adherence_status"`
			EvidenceSummary string `json:"evidence_summary"`
			RemediationRef  string `json:"remediation_ref"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			WriteError(w, "INVALID_REQUEST", "malformed body",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		runID, ok := fxgcRunID(w, r)
		if !ok {
			return
		}
		row, err := svc.Assess(r.Context(), runID, body.PrincipleID,
			actor.UserID, body.AdherenceStatus, body.EvidenceSummary,
			body.RemediationRef)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"principle": row})
	}
}

// AdminFXGCComplete serves POST
// /api/v1/admin/fx-global-code/assessments/{id}/complete — freezes the
// verdicts, computes the score and generates the Statement of
// Commitment. Fails closed while any verdict is PENDING.
func AdminFXGCComplete(svc *compliance.FXGCService, trustProxy bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		actor, err := adminActorFrom(r, trustProxy)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		runID, ok := fxgcRunID(w, r)
		if !ok {
			return
		}
		run, err := svc.Complete(r.Context(), runID, actor.UserID)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"run": run})
	}
}

// AdminFXGCSign serves POST
// /api/v1/admin/fx-global-code/assessments/{id}/sign — the executive
// sign-off on the generated statement.
func AdminFXGCSign(svc *compliance.FXGCService, trustProxy bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		actor, err := adminActorFrom(r, trustProxy)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		runID, ok := fxgcRunID(w, r)
		if !ok {
			return
		}
		run, err := svc.Sign(r.Context(), runID, actor.UserID)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"run": run})
	}
}

// AdminFXGCPublish serves POST
// /api/v1/admin/fx-global-code/assessments/{id}/publish — flags the
// signed statement for the public register.
func AdminFXGCPublish(svc *compliance.FXGCService, trustProxy bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		actor, err := adminActorFrom(r, trustProxy)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		runID, ok := fxgcRunID(w, r)
		if !ok {
			return
		}
		stmt, err := svc.PublishStatement(r.Context(), runID, actor.UserID)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"statement": stmt})
	}
}

func fxgcRunID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		WriteError(w, "INVALID_REQUEST", "bad assessment id",
			gateway.RequestIDFrom(r.Context()), nil)
		return 0, false
	}
	return id, true
}

// ---------------------------------------------------------------------------
// Regulatory change — Task 21.3.25
// ---------------------------------------------------------------------------

// AdminRegChangeList serves GET /api/v1/admin/regulatory-changes —
// officer roles get the filtered register; a Read-Only Auditor caller
// is confined to the untriaged + near-deadline union (service-scoped —
// the auditor can never probe the full register).
func AdminRegChangeList(svc *compliance.RegChangeService,
	resolver compliance.HoldRoleResolver, trustProxy bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		actor, err := adminActorFrom(r, trustProxy)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		limit := 200
		if q := r.URL.Query().Get("limit"); q != "" {
			if n, err := strconv.Atoi(q); err == nil {
				limit = n
			}
		}
		if r.URL.Query().Get("scope") == "dashboard" {
			dash, err := svc.Dashboard(r.Context(), actor.UserID, limit)
			if err != nil {
				writeServiceErr(w, r, err)
				return
			}
			WriteJSON(w, http.StatusOK, map[string]any{"dashboard": dash})
			return
		}
		recs, err := svc.ListForRole(r.Context(), actor.UserID,
			r.URL.Query().Get("status"), limit)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		if recs == nil {
			recs = []compliance.RegChange{}
		}
		WriteJSON(w, http.StatusOK, map[string]any{"changes": recs})
	}
}

// AdminRegChangeCreate serves POST /api/v1/admin/regulatory-changes —
// registers a watch row (triage_due_at = published_at + 10 business
// days computed service-side; emergency publications accepted).
func AdminRegChangeCreate(svc *compliance.RegChangeService,
	trustProxy bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		actor, err := adminActorFrom(r, trustProxy)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		var body compliance.RegChangeInput
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			WriteError(w, "INVALID_REQUEST", "malformed body",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		chg, overlaps, created, err := svc.Register(r.Context(), body, actor.UserID)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		out := map[string]any{"change": chg, "created": created}
		if len(overlaps) > 0 {
			out["overlapping_change_ids"] = overlaps
		}
		code := http.StatusOK
		if created {
			code = http.StatusCreated
		}
		WriteJSON(w, code, out)
	}
}

// AdminRegChangeImpactGet serves GET
// /api/v1/admin/regulatory-changes/{id}/impact — the change plus its
// impact map and linked regulator correspondence.
func AdminRegChangeImpactGet(svc *compliance.RegChangeService,
	resolver compliance.HoldRoleResolver, trustProxy bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		actor, err := adminActorFrom(r, trustProxy)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		chgID, ok := regChangeID(w, r)
		if !ok {
			return
		}
		chg, err := svc.GetForRole(r.Context(), actor.UserID, chgID)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		impacts, err := svc.ListImpacts(r.Context(), chg.ChangeID)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		corr, err := svc.ListCorrespondence(r.Context(), chg.ChangeID)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		if impacts == nil {
			impacts = []compliance.RegChangeImpact{}
		}
		if corr == nil {
			corr = []compliance.RegCorrespondence{}
		}
		WriteJSON(w, http.StatusOK, map[string]any{
			"change": chg, "impacts": impacts, "correspondence": corr})
	}
}

// AdminRegChangeImpactPut serves PUT
// /api/v1/admin/regulatory-changes/{id}/impact — records/updates one
// impact item ({kind, ref, owner?, effort_estimate?, due_at?}).
func AdminRegChangeImpactPut(svc *compliance.RegChangeService,
	trustProxy bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		actor, err := adminActorFrom(r, trustProxy)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		var body struct {
			Kind           string     `json:"kind"`
			Ref            string     `json:"ref"`
			Owner          *int64     `json:"owner"`
			EffortEstimate string     `json:"effort_estimate"`
			DueAt          *time.Time `json:"due_at"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			WriteError(w, "INVALID_REQUEST", "malformed body",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		chgID, ok := regChangeID(w, r)
		if !ok {
			return
		}
		imp, created, err := svc.RecordImpact(r.Context(), chgID,
			actor.UserID, body.Kind, body.Ref, body.Owner,
			body.EffortEstimate, body.DueAt)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		code := http.StatusOK
		if created {
			code = http.StatusCreated
		}
		WriteJSON(w, code, map[string]any{"impact": imp, "created": created})
	}
}

// AdminRegChangeTransition serves POST
// /api/v1/admin/regulatory-changes/{id}/transition — the lifecycle
// moves {action: triage|scope|implement|close, owner?, notes?,
// matrix_update_ref?}. IMPLEMENTED is gated on a completed assessment
// (+ matrix_update_ref when a §24 criterion is altered).
func AdminRegChangeTransition(svc *compliance.RegChangeService,
	trustProxy bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		actor, err := adminActorFrom(r, trustProxy)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		var body struct {
			Action          string `json:"action"`
			Owner           *int64 `json:"owner"`
			Notes           string `json:"notes"`
			MatrixUpdateRef string `json:"matrix_update_ref"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			WriteError(w, "INVALID_REQUEST", "malformed body",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		id, ok := regChangeID(w, r)
		if !ok {
			return
		}
		var chg *compliance.RegChange
		switch body.Action {
		case "triage":
			chg, err = svc.Triage(r.Context(), id, actor.UserID, body.Owner, body.Notes)
		case "scope":
			chg, err = svc.SetStatus(r.Context(), id, actor.UserID, compliance.RegStatusScoped)
		case "implement":
			chg, err = svc.MarkImplemented(r.Context(), id, actor.UserID, body.MatrixUpdateRef)
		case "close":
			chg, err = svc.Close(r.Context(), id, actor.UserID, body.Notes)
		case "assign_owner":
			if body.Owner == nil {
				err = excerrors.New("INVALID_REQUEST",
					"owner is required for assign_owner")
			} else {
				chg, err = svc.AssignOwner(r.Context(), id, actor.UserID, *body.Owner)
			}
		default:
			err = excerrors.New("INVALID_REQUEST",
				"action must be triage|scope|implement|close|assign_owner")
		}
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"change": chg})
	}
}

// AdminRegChangeImpactDone serves POST
// /api/v1/admin/regulatory-changes/impacts/{id}/done — flips one OPEN
// impact item to DONE.
func AdminRegChangeImpactDone(svc *compliance.RegChangeService,
	trustProxy bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		actor, err := adminActorFrom(r, trustProxy)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		impID, ok := regChangeID(w, r)
		if !ok {
			return
		}
		imp, err := svc.CompleteImpact(r.Context(), impID, actor.UserID)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"impact": imp})
	}
}

// AdminRegChangeCorrespondence serves POST
// /api/v1/admin/regulatory-changes/{id}/correspondence — attaches a
// regulator information hold/request ({kind: INFO_HOLD|INFO_REQUEST,
// summary, received_at, due_at?}).
func AdminRegChangeCorrespondence(svc *compliance.RegChangeService,
	trustProxy bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		actor, err := adminActorFrom(r, trustProxy)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		var body struct {
			Kind       string     `json:"kind"`
			Summary    string     `json:"summary"`
			ReceivedAt time.Time  `json:"received_at"`
			DueAt      *time.Time `json:"due_at"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			WriteError(w, "INVALID_REQUEST", "malformed body",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		chgID, ok := regChangeID(w, r)
		if !ok {
			return
		}
		rec, err := svc.AttachCorrespondence(r.Context(), chgID,
			actor.UserID, body.Kind, body.Summary, body.ReceivedAt, body.DueAt)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusCreated, map[string]any{"correspondence": rec})
	}
}

func regChangeID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		WriteError(w, "INVALID_REQUEST", "bad id",
			gateway.RequestIDFrom(r.Context()), nil)
		return 0, false
	}
	return id, true
}

// ---------------------------------------------------------------------------
// Execution policy — Task 21.3.28
// ---------------------------------------------------------------------------

// PublicExecutionPolicy serves GET /api/v1/execution-policy — the
// ACTIVE version, unauthenticated per spec §14.13. 404 while no policy
// is published.
func PublicExecutionPolicy(svc *compliance.ExecutionPolicyService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			WriteError(w, "SERVICE_DEGRADED", "execution policy unavailable",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		p, err := svc.Active(r.Context())
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		if p == nil {
			WriteError(w, "NOT_FOUND", "no execution policy is currently published",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"policy": p})
	}
}

// AccountConsent serves PUT /api/v1/account/consent — records the
// caller's versioned consent. {consent_type:"EXECUTION_POLICY",
// version?, refused?:false}. The account identity always resolves from
// claims — a foreign account_id in the body is FORBIDDEN; a refusal
// writes nothing (close-only is the natural consequence).
func AccountConsent(svc *compliance.ExecutionPolicyService,
	trustProxy bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			WriteError(w, "SERVICE_DEGRADED", "consent service unavailable",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		accountID, claims, ok := claimsAccount(w, r)
		if !ok {
			return
		}
		var body struct {
			ConsentType string          `json:"consent_type"`
			Version     string          `json:"version"`
			AccountID   *int64          `json:"account_id"`
			Refused     bool            `json:"refused"`
			Metadata    json.RawMessage `json:"metadata"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			WriteError(w, "INVALID_REQUEST", "malformed body",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		if body.AccountID != nil && *body.AccountID != accountID {
			WriteError(w, "FORBIDDEN", "cannot consent for another account",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		if body.ConsentType != "" && body.ConsentType != compliance.ConsentExecPolicy {
			WriteError(w, "INVALID_REQUEST",
				"unsupported consent_type "+body.ConsentType,
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		if body.Refused {
			// A refusal records no consent — the admission gate keeps
			// the account close-only. The status read reports it.
			st, has, err := svc.ConsentStatus(r.Context(), accountID)
			if err != nil {
				writeServiceErr(w, r, err)
				return
			}
			WriteJSON(w, http.StatusOK, map[string]any{
				"consented": has, "refused": true, "consent": st})
			return
		}
		var userID int64
		if claims != nil {
			userID, _ = strconv.ParseInt(claims.Subject, 10, 64)
		}
		rec, created, err := svc.Consent(r.Context(), accountID, userID,
			body.Version, middleware.ClientIP(r, trustProxy), body.Metadata)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		code := http.StatusCreated
		if !created {
			code = http.StatusOK // idempotent consent replay
		}
		WriteJSON(w, code, map[string]any{"consent": rec, "created": created})
	}
}

// AdminPolicyList serves GET /api/v1/admin/execution-policies — the
// version history (Read-Only Auditor visibility).
func AdminPolicyList(svc *compliance.ExecutionPolicyService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		limit := 50
		if q := r.URL.Query().Get("limit"); q != "" {
			if n, err := strconv.Atoi(q); err == nil {
				limit = n
			}
		}
		policies, err := svc.List(r.Context(), limit)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		if policies == nil {
			policies = []compliance.ExecutionPolicy{}
		}
		WriteJSON(w, http.StatusOK, map[string]any{"policies": policies})
	}
}

// AdminPolicyDraft serves POST /api/v1/admin/execution-policies —
// files a new DRAFT version {version, body_ref}.
func AdminPolicyDraft(svc *compliance.ExecutionPolicyService,
	trustProxy bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		actor, err := adminActorFrom(r, trustProxy)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		var body struct {
			Version string `json:"version"`
			BodyRef string `json:"body_ref"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			WriteError(w, "INVALID_REQUEST", "malformed body",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		p, err := svc.CreateDraft(r.Context(), body.Version, body.BodyRef,
			actor.UserID)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusCreated, map[string]any{"policy": p})
	}
}

// AdminPolicyActivate serves POST
// /api/v1/admin/execution-policies/{id}/activate — the CCO approval
// that publishes a DRAFT ({effective_from?, review_due_at,
// material_change?, reg_change_id?}). Refused while the incumbent's
// annual review is overdue.
func AdminPolicyActivate(svc *compliance.ExecutionPolicyService,
	trustProxy bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		actor, err := adminActorFrom(r, trustProxy)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		var body struct {
			EffectiveFrom  time.Time `json:"effective_from"`
			ReviewDueAt    time.Time `json:"review_due_at"`
			MaterialChange bool      `json:"material_change"`
			RegChangeID    *int64    `json:"reg_change_id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			WriteError(w, "INVALID_REQUEST", "malformed body",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		policyID, ok := regChangeID(w, r)
		if !ok {
			return
		}
		p, err := svc.Activate(r.Context(), policyID, actor.UserID,
			body.EffectiveFrom, body.ReviewDueAt, body.MaterialChange,
			body.RegChangeID)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"policy": p})
	}
}

// AdminPolicyReview serves POST
// /api/v1/admin/execution-policies/{id}/review — the annual CCO review
// {notes?, review_due_at}; snapshots the assembled evidence pack and
// rolls the deadline forward.
func AdminPolicyReview(svc *compliance.ExecutionPolicyService,
	trustProxy bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		actor, err := adminActorFrom(r, trustProxy)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		var body struct {
			Notes       string    `json:"notes"`
			ReviewDueAt time.Time `json:"review_due_at"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			WriteError(w, "INVALID_REQUEST", "malformed body",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		policyID, ok := regChangeID(w, r)
		if !ok {
			return
		}
		p, err := svc.Review(r.Context(), policyID, actor.UserID,
			body.Notes, body.ReviewDueAt)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"policy": p})
	}
}
