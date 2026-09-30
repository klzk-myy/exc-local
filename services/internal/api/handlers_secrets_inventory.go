// Phase-09 Task 9.3.29 item 4 (spec §19.14, §24 #340) — the secrets
// inventory admin surface:
//
//	GET  /api/v1/admin/security/secrets-inventory                  register + rotation states
//	PUT  /api/v1/admin/security/secrets-inventory                  upsert one row (audited)
//	POST /api/v1/admin/security/secrets-inventory/{name}/mark-rotated
//	                                                               record a completed rotation (audited)
//
// Role gate lives in the service (doc §5 "Security + SRE leads" → Super
// Admin; there is no dedicated Security role in the §8.2 canon).
//
// Emission contract: while any row is past its rotation SLA the GET
// answers 503 SECRET_ROTATION_OVERDUE — the spec-pinned emission for a
// rotation breach — with the full assessed register in details so the
// P2 responder sees the whole inventory, not just the breach count.
// The store stays metadata-only: request/response payloads carry names
// and schedules, never secret material.
package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"exchange/internal/gateway"
	"exchange/internal/middleware"
	"exchange/internal/security"
)

// inventoryService is the handler-side seam over
// security.InventoryService (testable without PG).
type inventoryService interface {
	List(ctx context.Context, adminID int64) (*security.InventoryView, error)
	UpsertEntry(ctx context.Context, adminID int64, in security.InventoryEntryInput, clientIP string) (*security.InventoryEntry, error)
	MarkRotated(ctx context.Context, adminID int64, name string, m security.RotationMark, clientIP string) (*security.InventoryEntry, error)
}

// AdminSecretsInventoryList serves GET …/secrets-inventory — the
// assessed register. Rows past their rotation SLA turn the response
// into 503 SECRET_ROTATION_OVERDUE; the register still travels in the
// error details (fail-closed visibility, spec §19.14).
func AdminSecretsInventoryList(svc inventoryService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		adminID, ok := adminActor(w, r)
		if !ok {
			return
		}
		view, err := svc.List(r.Context(), adminID)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		if len(view.Overdue) > 0 {
			WriteError(w, "SECRET_ROTATION_OVERDUE",
				"one or more secrets are past their rotation SLA — run the row's rotation_procedure, then mark-rotated",
				gateway.RequestIDFrom(r.Context()), map[string]any{
					"overdue":   view.Overdue,
					"unrotated": view.Unrotated,
					"secrets":   view.Rows,
				})
			return
		}
		WriteJSON(w, http.StatusOK, view)
	}
}

// upsertSecretRequest is the PUT body — durations arrive as seconds to
// match the §10.5 duration-unit convention.
type upsertSecretRequest struct {
	SecretName        string   `json:"secret_name"`
	SecretClass       string   `json:"secret_class"`
	Category          string   `json:"category"`
	Owner             string   `json:"owner"`
	Consumers         []string `json:"consumers"`
	TTLSeconds        int64    `json:"ttl_seconds"`
	AlertLeadSeconds  int64    `json:"alert_lead_seconds"`
	RotationProcedure string   `json:"rotation_procedure"`
	LastRotatedAt     string   `json:"last_rotated_at"` // RFC3339 backfill, optional
	DRCritical        bool     `json:"dr_critical"`
	BreakGlassPath    string   `json:"break_glass_path"`
}

// AdminSecretsInventoryUpsert serves PUT …/secrets-inventory — register
// or update one inventory row. Server-side validation clamps TTL to the
// class rotation ceiling (fail-closed — a weaker TTL is a defect).
func AdminSecretsInventoryUpsert(svc inventoryService, trustProxy bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		adminID, ok := adminActor(w, r)
		if !ok {
			return
		}
		var req upsertSecretRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			WriteError(w, "INVALID_REQUEST", "malformed body",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		var rotated *time.Time
		if raw := strings.TrimSpace(req.LastRotatedAt); raw != "" {
			t, err := time.Parse(time.RFC3339, raw)
			if err != nil {
				WriteError(w, "INVALID_REQUEST", "last_rotated_at must be RFC3339",
					gateway.RequestIDFrom(r.Context()), nil)
				return
			}
			rotated = &t
		}
		e, err := svc.UpsertEntry(r.Context(), adminID, security.InventoryEntryInput{
			SecretName:        req.SecretName,
			Class:             security.SecretClass(req.SecretClass),
			Category:          req.Category,
			Owner:             req.Owner,
			Consumers:         req.Consumers,
			TTL:               time.Duration(req.TTLSeconds) * time.Second,
			AlertLead:         time.Duration(req.AlertLeadSeconds) * time.Second,
			RotationProcedure: req.RotationProcedure,
			LastRotatedAt:     rotated,
			DRCritical:        req.DRCritical,
			BreakGlassPath:    req.BreakGlassPath,
		}, middleware.ClientIP(r, trustProxy))
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, e)
	}
}

// markRotatedRequest is the POST …/mark-rotated body — the "mark" step
// of the doc §3 rotation procedure for both scheduled runbooks and
// leak-triggered emergency rotations.
type markRotatedRequest struct {
	Emergency   bool   `json:"emergency"`    // leak-triggered — requires incident_ref
	IncidentRef string `json:"incident_ref"` // security incident record
	BreakGlass  bool   `json:"break_glass"`  // executed under a Task 7.3.12 grant
	GrantID     int64  `json:"grant_id"`     // admin_break_glass_grants.id
}

// AdminSecretsInventoryMarkRotated serves POST
// …/secrets-inventory/{name}/mark-rotated — clears the rotation SLA
// clock after a completed rotation; the evaluator clears
// SECRET_ROTATION_OVERDUE on its next pass (doc §3 step 5).
func AdminSecretsInventoryMarkRotated(svc inventoryService, trustProxy bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		adminID, ok := adminActor(w, r)
		if !ok {
			return
		}
		name := strings.TrimSpace(r.PathValue("name"))
		if name == "" {
			WriteError(w, "INVALID_REQUEST", "secret name required",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		var req markRotatedRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			WriteError(w, "INVALID_REQUEST", "malformed body",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		e, err := svc.MarkRotated(r.Context(), adminID, name, security.RotationMark{
			Emergency:   req.Emergency,
			IncidentRef: req.IncidentRef,
			BreakGlass:  req.BreakGlass,
			GrantID:     req.GrantID,
		}, middleware.ClientIP(r, trustProxy))
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, e)
	}
}
