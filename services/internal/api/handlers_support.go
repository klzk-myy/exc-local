// Phase-07 Task 7.3.7 — support tickets / complaints / support-view
// HTTP surface, plus Task 7.3.3's admin audit query lives in
// handlers_audit.go.
//
// Client routes (authUser):
//
//	POST /api/v1/support/tickets            open a ticket
//	GET  /api/v1/support/tickets            list own tickets (cursor-paged)
//	GET  /api/v1/support/tickets/{id}       own ticket detail + public notes
//
// Admin routes (role-gated by the service via the RoleResolver seam —
// Phase-07 Task 7.3.1 owns the real role store; nil fails closed):
//
//	GET  /api/v1/admin/support/tickets             queue list + filters
//	GET  /api/v1/admin/support/tickets/{id}        detail incl. internal notes
//	PUT  /api/v1/admin/support/tickets             assign/transition/ADR update
//	POST /api/v1/admin/support/tickets/{id}/notes  internal note
//	GET  /api/v1/admin/support/complaints/register MiFID complaint register
//	GET  /api/v1/admin/support/accounts/{id}       read-only support-view
package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"exchange/internal/admin"
	"exchange/internal/auth"
	"exchange/internal/gateway"
	"exchange/internal/middleware"
	"exchange/internal/support"
)

// ---------------------------------------------------------------------------
// Service seams (testable without PG)
// ---------------------------------------------------------------------------

// ticketService is the handler-side seam over support.Service.
type ticketService interface {
	Create(ctx context.Context, req support.CreateRequest) (*support.Ticket, error)
	ListMine(ctx context.Context, accountID int64,
		after *struct {
			Time time.Time
			ID   int64
		}, limit int) ([]support.Ticket, error)
	GetMine(ctx context.Context, accountID, ticketID int64) (*support.Ticket, []support.Note, error)
	List(ctx context.Context, adminID int64, f support.AdminFilter,
		after *struct {
			Time time.Time
			ID   int64
		}, limit int) ([]support.Ticket, error)
	ComplaintRegister(ctx context.Context, adminID int64,
		after *struct {
			Time time.Time
			ID   int64
		}, limit int) ([]support.Ticket, error)
	Get(ctx context.Context, adminID, ticketID int64) (*support.Ticket, []support.Note, error)
	Update(ctx context.Context, adminID int64, u support.AdminUpdate, clientIP string) (*support.Ticket, error)
	AddNote(ctx context.Context, adminID, ticketID int64, body string, internal bool, clientIP string) (*support.Note, error)
}

// keysetAfter adapts the §8.8 cursor to the service's page position.
func keysetAfter(p *ListParams) *struct {
	Time time.Time
	ID   int64
} {
	if p == nil || p.Decoded == nil {
		return nil
	}
	return &struct {
		Time time.Time
		ID   int64
	}{p.Decoded.CreatedAt, p.Decoded.ID}
}

// ticketEnvelope renders the §8.8 list envelope for a ticket page.
func ticketEnvelope(ts []support.Ticket, p *ListParams) ListEnvelope {
	cursors := PageCursors(ts, func(t support.Ticket) (time.Time, int64) {
		return t.CreatedAt, t.ID
	})
	return NewListEnvelope(ts, p, cursors, int64(len(ts)))
}

// adminActor resolves the caller's admin user id (claims.Subject is the
// users.id — same convention as ManualLiquidationHandler).
func adminActor(w http.ResponseWriter, r *http.Request) (int64, bool) {
	claims := auth.ClaimsFrom(r.Context())
	if claims == nil {
		WriteError(w, "UNAUTHORIZED", "authentication required",
			gateway.RequestIDFrom(r.Context()), nil)
		return 0, false
	}
	id, err := strconv.ParseInt(claims.Subject, 10, 64)
	if err != nil || id <= 0 {
		WriteError(w, "UNAUTHORIZED", "admin identity unresolvable",
			gateway.RequestIDFrom(r.Context()), nil)
		return 0, false
	}
	return id, true
}

// ---------------------------------------------------------------------------
// Client handlers
// ---------------------------------------------------------------------------

// createTicketRequest is the POST /api/v1/support/tickets body.
type createTicketRequest struct {
	Category      string `json:"category"`
	Subject       string `json:"subject"`
	Body          string `json:"body"`
	Priority      string `json:"priority"`
	OriginChannel string `json:"origin_channel"`
}

// SupportTicketCreate serves POST /api/v1/support/tickets.
func SupportTicketCreate(svc ticketService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		accountID, _, ok := claimsAccount(w, r)
		if !ok {
			return
		}
		var req createTicketRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			WriteError(w, "INVALID_REQUEST", "malformed body",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		t, err := svc.Create(r.Context(), support.CreateRequest{
			AccountID:     accountID,
			Category:      req.Category,
			Subject:       req.Subject,
			Body:          req.Body,
			Priority:      req.Priority,
			OriginChannel: req.OriginChannel,
		})
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusCreated, t)
	}
}

// SupportTicketList serves GET /api/v1/support/tickets — own tickets.
func SupportTicketList(svc ticketService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		accountID, _, ok := claimsAccount(w, r)
		if !ok {
			return
		}
		p, err := ParseListParams(r, ListSpecFor("/api/v1/support/tickets"))
		if err != nil {
			WriteError(w, "INVALID_REQUEST", err.Error(),
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		ts, err := svc.ListMine(r.Context(), accountID, keysetAfter(p), p.Limit)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, ticketEnvelope(ts, p))
	}
}

// SupportTicketGet serves GET /api/v1/support/tickets/{id} — own ticket
// plus non-internal notes.
func SupportTicketGet(svc ticketService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		accountID, _, ok := claimsAccount(w, r)
		if !ok {
			return
		}
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil || id <= 0 {
			WriteError(w, "INVALID_REQUEST", "ticket id must be a positive integer",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		t, notes, err := svc.GetMine(r.Context(), accountID, id)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{
			"ticket": t, "notes": notes,
		})
	}
}

// ---------------------------------------------------------------------------
// Admin handlers
// ---------------------------------------------------------------------------

// AdminSupportTicketList serves GET /api/v1/admin/support/tickets.
func AdminSupportTicketList(svc ticketService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		adminID, ok := adminActor(w, r)
		if !ok {
			return
		}
		p, err := ParseListParams(r, ListSpecFor("/api/v1/admin/support/tickets"))
		if err != nil {
			WriteError(w, "INVALID_REQUEST", err.Error(),
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		q := r.URL.Query()
		f := support.AdminFilter{
			Status:   q.Get("status"),
			Category: q.Get("category"),
			Type:     q.Get("type"),
			Queue:    q.Get("queue"),
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
		if raw := q.Get("account_id"); raw != "" {
			id, err := strconv.ParseInt(raw, 10, 64)
			if err != nil {
				WriteError(w, "INVALID_REQUEST", "account_id must be an integer",
					gateway.RequestIDFrom(r.Context()), nil)
				return
			}
			f.AccountID = &id
		}
		f.Unassigned = q.Get("unassigned") == "1" || q.Get("unassigned") == "true"
		f.BreachedOnly = q.Get("breached") == "1" || q.Get("breached") == "true"

		ts, err := svc.List(r.Context(), adminID, f, keysetAfter(p), p.Limit)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, ticketEnvelope(ts, p))
	}
}

// AdminSupportTicketGet serves GET /api/v1/admin/support/tickets/{id}
// including internal notes.
func AdminSupportTicketGet(svc ticketService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		adminID, ok := adminActor(w, r)
		if !ok {
			return
		}
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil || id <= 0 {
			WriteError(w, "INVALID_REQUEST", "ticket id must be a positive integer",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		t, notes, err := svc.Get(r.Context(), adminID, id)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{
			"ticket": t, "notes": notes,
		})
	}
}

// adminTicketUpdateRequest is the PUT /api/v1/admin/support/tickets
// body — the route registry pins PUT without a path id, so the ticket id
// travels in the body.
type adminTicketUpdateRequest struct {
	TicketID        int64  `json:"ticket_id"`
	Status          string `json:"status"`
	AssigneeAdminID *int64 `json:"assignee_admin_id"`
	AssignToMe      bool   `json:"assign_to_me"`
	Priority        string `json:"priority"`
	Note            string `json:"note"`
	ADRRequested    *bool  `json:"adr_requested"`
	ADRScheme       string `json:"adr_scheme"`
	ADRReference    string `json:"adr_reference"`
	ADRAck          bool   `json:"adr_acknowledge"`
}

// AdminSupportTicketUpdate serves PUT /api/v1/admin/support/tickets.
func AdminSupportTicketUpdate(svc ticketService, trustProxy bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		adminID, ok := adminActor(w, r)
		if !ok {
			return
		}
		var req adminTicketUpdateRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			WriteError(w, "INVALID_REQUEST", "malformed body",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		t, err := svc.Update(r.Context(), adminID, support.AdminUpdate{
			TicketID:        req.TicketID,
			Status:          req.Status,
			AssigneeAdminID: req.AssigneeAdminID,
			AssignToMe:      req.AssignToMe,
			Priority:        req.Priority,
			Note:            req.Note,
			ADRRequested:    req.ADRRequested,
			ADRScheme:       req.ADRScheme,
			ADRReference:    req.ADRReference,
			ADRAck:          req.ADRAck,
		}, middleware.ClientIP(r, trustProxy))
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, t)
	}
}

// adminNoteRequest is the POST .../notes body.
type adminNoteRequest struct {
	Body     string `json:"body"`
	Internal *bool  `json:"internal"` // default true — notes are internal unless marked public
}

// AdminSupportTicketNote serves POST /api/v1/admin/support/tickets/{id}/notes.
func AdminSupportTicketNote(svc ticketService, trustProxy bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		adminID, ok := adminActor(w, r)
		if !ok {
			return
		}
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil || id <= 0 {
			WriteError(w, "INVALID_REQUEST", "ticket id must be a positive integer",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		var req adminNoteRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			WriteError(w, "INVALID_REQUEST", "malformed body",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		internal := true
		if req.Internal != nil {
			internal = *req.Internal
		}
		n, err := svc.AddNote(r.Context(), adminID, id, req.Body, internal,
			middleware.ClientIP(r, trustProxy))
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusCreated, n)
	}
}

// AdminComplaintRegister serves
// GET /api/v1/admin/support/complaints/register — the MiFID
// complaint-handling record (COMPLAINT + DISPUTE rows).
func AdminComplaintRegister(svc ticketService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		adminID, ok := adminActor(w, r)
		if !ok {
			return
		}
		p, err := ParseListParams(r, ListSpecFor("/api/v1/admin/support/complaints/register"))
		if err != nil {
			WriteError(w, "INVALID_REQUEST", err.Error(),
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		ts, err := svc.ComplaintRegister(r.Context(), adminID, keysetAfter(p), p.Limit)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, ticketEnvelope(ts, p))
	}
}

// supportViewer is the support-view service seam.
type supportViewer interface {
	View(ctx context.Context, adminUserID int64, accountID int64, clientIP string) (*admin.SupportView, error)
}

// AdminSupportView serves GET /api/v1/admin/support/accounts/{id} —
// the read-only dossier. Role gate: Support Agent or Super Admin
// (checked via the resolver seam; the service also audit-logs the view).
func AdminSupportView(svc supportViewer, resolver AdminRoleResolver, trustProxy bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		adminID, ok := adminActor(w, r)
		if !ok {
			return
		}
		if resolver == nil {
			WriteError(w, "UNAUTHORIZED_ROLE",
				"role resolver not configured (Phase-07 RBAC stub boundary)",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		role, err := resolver(r.Context(), adminID)
		if err != nil {
			WriteError(w, "INTERNAL_ERROR", "role lookup failed",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		if role != "Support Agent" && role != "Super Admin" {
			WriteError(w, "UNAUTHORIZED_ROLE",
				"support view requires Support Agent or Super Admin",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil || id <= 0 {
			WriteError(w, "INVALID_REQUEST", "account id must be a positive integer",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		v, err := svc.View(r.Context(), adminID, id, middleware.ClientIP(r, trustProxy))
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, v)
	}
}
