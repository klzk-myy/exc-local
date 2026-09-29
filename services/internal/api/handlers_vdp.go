// Phase-13.5 Tasks 13.5.3.8/13.5.3.9 — Vulnerability Disclosure Program
// HTTP surface (spec §19.11.2, §24 #332).
//
// Public routes (authPublic + TierPublic rate tier; no session required):
//
//	POST /api/v1/security/disclosures   researcher submission (idempotent
//	                                  on report_id; 64KB body cap;
//	                                  honeypot field `website` silently
//	                                  discards spam-shaped traffic)
//	GET  /api/v1/security/policy        the published policy markdown
//	                                  (content/security/policy.md)
//
// Admin routes (role-gated inside the service — Super Admin / Compliance
// Officer write, Read-Only Auditor reads):
//
//	POST /api/v1/admin/security/disclosures/intake        pentest/internal filing
//	GET  /api/v1/admin/security/disclosures               register list + filters
//	GET  /api/v1/admin/security/disclosures/{id}          detail
//	POST /api/v1/admin/security/disclosures/{id}/triage   severity+CVSS+ETA
//	PUT  /api/v1/admin/security/disclosures/{id}          transitions/metadata
package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"exchange/internal/gateway"
	"exchange/internal/middleware"
	"exchange/internal/security"
)

// vdpService is the handler-side seam over security.Service (testable
// without PG).
type vdpService interface {
	Submit(ctx context.Context, req security.SubmitRequest) (*security.Disclosure, bool, error)
	IngestPentest(ctx context.Context, adminID int64, req security.SubmitRequest, clientIP string) (*security.Disclosure, error)
	Triage(ctx context.Context, adminID int64, in security.TriageInput, clientIP string) (*security.Disclosure, error)
	Update(ctx context.Context, adminID int64, u security.AdminUpdate, clientIP string) (*security.Disclosure, error)
	Get(ctx context.Context, adminID, id int64) (*security.Disclosure, error)
	List(ctx context.Context, adminID int64, f security.AdminFilter,
		after *struct {
			Time time.Time
			ID   int64
		}, limit int) ([]security.Disclosure, error)
}

// vdpEnvelope renders the §8.8 list envelope for a disclosure page.
func vdpEnvelope(ds []security.Disclosure, p *ListParams) ListEnvelope {
	cursors := PageCursors(ds, func(d security.Disclosure) (time.Time, int64) {
		return d.CreatedAt, d.ID
	})
	return NewListEnvelope(ds, p, cursors, int64(len(ds)))
}

// ---------------------------------------------------------------------------
// Public surface
// ---------------------------------------------------------------------------

// vdpMaxBody is the public submission ceiling — far under the §5.3.29
// 1MB REST cap; a report is text, not an attachment dump.
const vdpMaxBody = 64 << 10

// submitDisclosureRequest is the POST /api/v1/security/disclosures body.
type submitDisclosureRequest struct {
	ReportID             string   `json:"report_id"`
	Title                string   `json:"title"`
	AffectedComponents   []string `json:"affected_components"`
	Reproduction         string   `json:"reproduction"`
	ReporterHandle       string   `json:"reporter_handle"`
	ContactEmail         string   `json:"contact_email"`
	SuggestedSeverity    string   `json:"suggested_severity"`
	AttributionRequested bool     `json:"attribution_requested"`
	// Website is the honeypot: a real browser form renders it hidden and
	// humans leave it empty; bots that fill every field self-identify.
	// Submissions with it set are accepted-and-dropped (uniform 201).
	Website string `json:"website"`
}

// VDPDisclosureSubmit serves POST /api/v1/security/disclosures —
// anonymous researcher intake. Unauthenticated by design (the reporter
// may not hold an account): TierPublic rate-limiting + the 64KB body cap
// + the honeypot are the abuse controls. Idempotent on report_id.
func VDPDisclosureSubmit(svc vdpService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, vdpMaxBody)
		var req submitDisclosureRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			WriteError(w, "INVALID_REQUEST", "malformed body or body exceeds 64KB",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		if req.Website != "" {
			// Honeypot trip: uniform-accept-and-drop — a spammer must not
			// learn which field filtered them.
			WriteJSON(w, http.StatusCreated, map[string]any{
				"status": "received",
			})
			return
		}
		d, created, err := svc.Submit(r.Context(), security.SubmitRequest{
			ReportID:             req.ReportID,
			Source:               security.SourceResearcher,
			Title:                req.Title,
			AffectedComponents:   req.AffectedComponents,
			Reproduction:         req.Reproduction,
			ReporterHandle:       req.ReporterHandle,
			ContactEmail:         req.ContactEmail,
			SuggestedSeverity:    req.SuggestedSeverity,
			AttributionRequested: req.AttributionRequested,
		})
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusCreated, map[string]any{
			"report_id": d.ReportID,
			"status":    d.Status,
			"duplicate": !created,
		})
	}
}

// VDPPolicyDoc is the served policy document — loaded once at startup by
// the wiring layer from content/security/policy.md (the canonical doc).
// Bytes are immutable post-load; nil → SERVICE_DEGRADED (a VDP with an
// unavailable policy page is worse than a 503 — never fabricate).
type VDPPolicyDoc struct {
	ContentType string // "text/markdown; charset=utf-8"
	Body        []byte
}

// VDPPolicy serves GET /api/v1/security/policy — the stable public URL
// the program's scope/safe-harbor/SLA commitments live at.
func VDPPolicy(doc *VDPPolicyDoc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if doc == nil || len(doc.Body) == 0 {
			WriteError(w, "SERVICE_DEGRADED",
				"security policy document not loaded",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		ct := doc.ContentType
		if ct == "" {
			ct = "text/markdown; charset=utf-8"
		}
		w.Header().Set("Content-Type", ct)
		w.Header().Set("Cache-Control", "public, max-age=300")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(doc.Body)
	}
}

// ---------------------------------------------------------------------------
// Admin surface — role gates live in the service (Super Admin /
// Compliance Officer write; Read-Only Auditor reads).
// ---------------------------------------------------------------------------

// AdminVDPIntake serves POST /api/v1/admin/security/disclosures/intake —
// pentest findings and internally found issues filed into the same
// register as researcher reports (spec §19.11.2 item 5).
func AdminVDPIntake(svc vdpService, trustProxy bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		adminID, ok := adminActor(w, r)
		if !ok {
			return
		}
		var req struct {
			ReportID           string   `json:"report_id"`
			Source             string   `json:"source"` // PENTEST | INTERNAL
			Title              string   `json:"title"`
			AffectedComponents []string `json:"affected_components"`
			Reproduction       string   `json:"reproduction"`
			ReporterHandle     string   `json:"reporter_handle"`
			ContactEmail       string   `json:"contact_email"`
			SuggestedSeverity  string   `json:"suggested_severity"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			WriteError(w, "INVALID_REQUEST", "malformed body",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		d, err := svc.IngestPentest(r.Context(), adminID, security.SubmitRequest{
			ReportID:           req.ReportID,
			Source:             req.Source,
			Title:              req.Title,
			AffectedComponents: req.AffectedComponents,
			Reproduction:       req.Reproduction,
			ReporterHandle:     req.ReporterHandle,
			ContactEmail:       req.ContactEmail,
			SuggestedSeverity:  req.SuggestedSeverity,
		}, middleware.ClientIP(r, trustProxy))
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusCreated, d)
	}
}

// AdminVDPList serves GET /api/v1/admin/security/disclosures.
func AdminVDPList(svc vdpService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		adminID, ok := adminActor(w, r)
		if !ok {
			return
		}
		p, err := ParseListParams(r, ListSpecFor("/api/v1/admin/security/disclosures"))
		if err != nil {
			WriteError(w, "INVALID_REQUEST", err.Error(),
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		q := r.URL.Query()
		f := security.AdminFilter{
			Status:      q.Get("status"),
			Severity:    q.Get("severity"),
			Source:      q.Get("source"),
			BulletinRef: q.Get("bulletin"),
		}
		if raw := q.Get("assignee"); raw != "" {
			id, err := strconv.ParseInt(raw, 10, 64)
			if err != nil {
				WriteError(w, "INVALID_REQUEST", "assignee must be an integer",
					gateway.RequestIDFrom(r.Context()), nil)
				return
			}
			f.AssigneeID = &id
		}
		f.Unassigned = q.Get("unassigned") == "1" || q.Get("unassigned") == "true"
		f.BreachedOnly = q.Get("breached") == "1" || q.Get("breached") == "true"

		ds, err := svc.List(r.Context(), adminID, f, keysetAfter(p), p.Limit)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, vdpEnvelope(ds, p))
	}
}

// AdminVDPGet serves GET /api/v1/admin/security/disclosures/{id}.
func AdminVDPGet(svc vdpService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		adminID, ok := adminActor(w, r)
		if !ok {
			return
		}
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil || id <= 0 {
			WriteError(w, "INVALID_REQUEST", "disclosure id must be a positive integer",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		d, err := svc.Get(r.Context(), adminID, id)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, d)
	}
}

// AdminVDPTriage serves POST /api/v1/admin/security/disclosures/{id}/triage —
// the severity+CVSS+ETA assignment (Task 13.5.3.9 contract).
func AdminVDPTriage(svc vdpService, trustProxy bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		adminID, ok := adminActor(w, r)
		if !ok {
			return
		}
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil || id <= 0 {
			WriteError(w, "INVALID_REQUEST", "disclosure id must be a positive integer",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		var req struct {
			Severity        string   `json:"severity"`
			CVSSScore       *float64 `json:"cvss_score"`
			CVSSVector      string   `json:"cvss_vector"`
			AssigneeAdminID *int64   `json:"assignee_admin_id"`
			AssignToMe      bool     `json:"assign_to_me"`
			BulletinRef     string   `json:"bulletin_ref"`
			SBOMRef         string   `json:"sbom_ref"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			WriteError(w, "INVALID_REQUEST", "malformed body",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		d, err := svc.Triage(r.Context(), adminID, security.TriageInput{
			ID:              id,
			Severity:        req.Severity,
			CVSSScore:       req.CVSSScore,
			CVSSVector:      req.CVSSVector,
			AssigneeAdminID: req.AssigneeAdminID,
			AssignToMe:      req.AssignToMe,
			BulletinRef:     req.BulletinRef,
			SBOMRef:         req.SBOMRef,
		}, middleware.ClientIP(r, trustProxy))
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, d)
	}
}

// AdminVDPUpdate serves PUT /api/v1/admin/security/disclosures/{id} —
// status transitions, assignment, bulletin/patch/SBOM linkage, dispute
// and attribution handling. Milestone timestamps are server-stamped
// only — never accepted from the client.
func AdminVDPUpdate(svc vdpService, trustProxy bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		adminID, ok := adminActor(w, r)
		if !ok {
			return
		}
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil || id <= 0 {
			WriteError(w, "INVALID_REQUEST", "disclosure id must be a positive integer",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		var req struct {
			Status                     string   `json:"status"`
			AssigneeAdminID            *int64   `json:"assignee_admin_id"`
			AssignToMe                 bool     `json:"assign_to_me"`
			BulletinRef                *string  `json:"bulletin_ref"`
			PatchRef                   *string  `json:"patch_ref"`
			SBOMRef                    *string  `json:"sbom_ref"`
			ResolutionSummary          *string  `json:"resolution_summary"`
			DisputeReason              *string  `json:"dispute_reason"`
			Acknowledge                bool     `json:"acknowledge"`
			MarkResearcherAcknowledged bool     `json:"researcher_acknowledged"`
			Severity                   string   `json:"severity"`
			CVSSScore                  *float64 `json:"cvss_score"`
			CVSSVector                 *string  `json:"cvss_vector"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			WriteError(w, "INVALID_REQUEST", "malformed body",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		d, err := svc.Update(r.Context(), adminID, security.AdminUpdate{
			ID:                         id,
			Status:                     req.Status,
			AssigneeAdminID:            req.AssigneeAdminID,
			AssignToMe:                 req.AssignToMe,
			BulletinRef:                req.BulletinRef,
			PatchRef:                   req.PatchRef,
			SBOMRef:                    req.SBOMRef,
			ResolutionSummary:          req.ResolutionSummary,
			DisputeReason:              req.DisputeReason,
			Acknowledge:                req.Acknowledge,
			MarkResearcherAcknowledged: req.MarkResearcherAcknowledged,
			Severity:                   req.Severity,
			CVSSScore:                  req.CVSSScore,
			CVSSVector:                 req.CVSSVector,
		}, middleware.ClientIP(r, trustProxy))
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, d)
	}
}
