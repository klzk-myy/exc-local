// Task 5.3.14 — announcements & maintenance calendar (spec §8.9 item 3,
// §24 #234).
//
// Public read:
//
//	GET /api/v1/announcements[?category=&limit=]      — live announcements
//	GET /api/v1/announcements/{id}                    — one live announcement
//	GET /api/v1/maintenance/schedule                  — upcoming windows
//
// Admin CRUD (scope admin, role Support Agent — §8.2 notifications owner):
//
//	GET/POST          /api/v1/admin/announcements
//	PATCH/DELETE      /api/v1/admin/announcements/{id}
//	GET/POST          /api/v1/admin/maintenance-windows
//	PATCH/DELETE      /api/v1/admin/maintenance-windows/{id}
//
// Deletes are state transitions (RETRACTED / CANCELLED), never row
// removal — announcements are disclosure records.
package api

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"exchange/internal/auth"
	"exchange/internal/gateway"
	"exchange/internal/marketapi"
)

// AnnounceDeps bundles the two stores behind Task 5.3.14.
type AnnounceDeps struct {
	Announcements marketapi.AnnouncementStore
	Maintenance   marketapi.MaintenanceStore
	Now           func() time.Time
}

func (d *AnnounceDeps) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now()
}

var announcementCategories = map[string]bool{
	"GENERAL": true, "MAINTENANCE": true, "INCIDENT": true,
	"PRODUCT": true, "PROMOTION": true,
}

var announcementStatuses = map[string]bool{
	"DRAFT": true, "PUBLISHED": true, "EXPIRED": true, "RETRACTED": true,
}

var maintenanceScopes = map[string]bool{
	"FULL_VENUE": true, "GATEWAY": true, "MARKET_DATA": true,
	"SETTLEMENT": true, "FUNDING": true, "INSTRUMENT": true,
}

var maintenanceStatuses = map[string]bool{
	"SCHEDULED": true, "IN_PROGRESS": true, "COMPLETED": true, "CANCELLED": true,
}

func parsePathID(raw string) (int64, bool) {
	n, err := strconv.ParseInt(raw, 10, 64)
	return n, err == nil && n > 0
}

// parseTimeParam accepts RFC3339; epoch-millis is rejected — admin write
// payloads use ISO time, read payloads emit epoch ms consistently.
func parseTimeParam(s string) (time.Time, bool) {
	if s == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// actor returns the authenticated subject for created_by bookkeeping.
func actor(r *http.Request) string {
	if c := auth.ClaimsFrom(r.Context()); c != nil {
		return c.Subject
	}
	return ""
}

func storeErr(w http.ResponseWriter, r *http.Request) {
	WriteError(w, "SERVICE_DEGRADED", "announcement store unavailable",
		gateway.RequestIDFrom(r.Context()), nil)
}

// ---------------------------------------------------------------------------
// Public reads
// ---------------------------------------------------------------------------

// Announcements lists live announcements (PUBLISHED inside its window).
// ?category= filters on the enum; ?limit= defaults to 50, max 200.
func Announcements(d *AnnounceDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		cat := strings.ToUpper(q.Get("category"))
		if cat != "" && !announcementCategories[cat] {
			WriteError(w, "INVALID_REQUEST",
				"category must be GENERAL|MAINTENANCE|INCIDENT|PRODUCT|PROMOTION",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		limit, ok := intParam(q.Get("limit"), 50, 1, 200)
		if !ok {
			WriteError(w, "INVALID_REQUEST",
				"limit must be an integer in [1,200]",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		list, err := d.Announcements.ListAnnouncements(r.Context(),
			marketapi.AnnouncementFilter{Category: cat, Limit: limit})
		if err != nil {
			storeErr(w, r)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{
			"data":           list,
			"count":          len(list),
			"server_time_ms": d.now().UnixMilli(),
		})
	}
}

// AnnouncementByID returns one announcement only while it is live —
// drafts and retracted items are not visible on the public surface (404).
func AnnouncementByID(d *AnnounceDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := parsePathID(r.PathValue("id"))
		if !ok {
			WriteError(w, "INVALID_REQUEST", "id must be a positive integer",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		a, err := d.Announcements.Announcement(r.Context(), id)
		if err != nil {
			storeErr(w, r)
			return
		}
		if a == nil || !a.Live(d.now()) {
			WriteError(w, "NOT_FOUND", "announcement not found",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		WriteJSON(w, http.StatusOK, a)
	}
}

// MaintenanceSchedule returns upcoming SCHEDULED/IN_PROGRESS windows —
// the public maintenance calendar.
func MaintenanceSchedule(d *AnnounceDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		list, err := d.Maintenance.UpcomingMaintenance(r.Context(), d.now())
		if err != nil {
			storeErr(w, r)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{
			"data":           list,
			"count":          len(list),
			"server_time_ms": d.now().UnixMilli(),
		})
	}
}

// ---------------------------------------------------------------------------
// Admin: announcements
// ---------------------------------------------------------------------------

// AdminAnnouncements lists every announcement regardless of status.
func AdminAnnouncements(d *AnnounceDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		limit, ok := intParam(r.URL.Query().Get("limit"), 100, 1, 500)
		if !ok {
			WriteError(w, "INVALID_REQUEST",
				"limit must be an integer in [1,500]",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		list, err := d.Announcements.ListAnnouncements(r.Context(),
			marketapi.AnnouncementFilter{IncludeAll: true, Limit: limit})
		if err != nil {
			storeErr(w, r)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{
			"data": list, "count": len(list),
		})
	}
}

// announcementIn is the create/update wire shape.
type announcementIn struct {
	Title     *string `json:"title"`
	Body      *string `json:"body"`
	Category  *string `json:"category"`
	Status    *string `json:"status"`
	PublishAt *string `json:"publish_at"` // RFC3339
	ExpiresAt *string `json:"expires_at"` // RFC3339, "" clears
}

func (in *announcementIn) validate(requireAll bool) (string, bool) {
	if requireAll {
		if in.Title == nil || strings.TrimSpace(*in.Title) == "" {
			return "title is required", false
		}
		if in.Body == nil || strings.TrimSpace(*in.Body) == "" {
			return "body is required", false
		}
	}
	if in.Title != nil && len(*in.Title) > 200 {
		return "title exceeds 200 characters", false
	}
	if in.Category != nil {
		c := strings.ToUpper(*in.Category)
		if !announcementCategories[c] {
			return "category must be GENERAL|MAINTENANCE|INCIDENT|PRODUCT|PROMOTION", false
		}
		*in.Category = c
	}
	if in.Status != nil {
		s := strings.ToUpper(*in.Status)
		if !announcementStatuses[s] {
			return "status must be DRAFT|PUBLISHED|EXPIRED|RETRACTED", false
		}
		*in.Status = s
	}
	if in.PublishAt != nil {
		if _, ok := parseTimeParam(*in.PublishAt); !ok {
			return "publish_at must be RFC3339", false
		}
	}
	if in.ExpiresAt != nil && *in.ExpiresAt != "" {
		if _, ok := parseTimeParam(*in.ExpiresAt); !ok {
			return "expires_at must be RFC3339", false
		}
	}
	return "", true
}

// CreateAnnouncement inserts an announcement (POST admin).
func CreateAnnouncement(d *AnnounceDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var in announcementIn
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			WriteError(w, "INVALID_REQUEST", "malformed JSON body",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		if msg, ok := in.validate(true); !ok {
			WriteError(w, "INVALID_REQUEST", msg,
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		a := marketapi.Announcement{
			Title:     strings.TrimSpace(*in.Title),
			Body:      *in.Body,
			Category:  "GENERAL",
			Status:    "PUBLISHED",
			PublishAt: d.now(),
			CreatedBy: actor(r),
		}
		if in.Category != nil {
			a.Category = *in.Category
		}
		if in.Status != nil {
			a.Status = *in.Status
		}
		if in.PublishAt != nil {
			t, _ := parseTimeParam(*in.PublishAt)
			a.PublishAt = t
		}
		if in.ExpiresAt != nil && *in.ExpiresAt != "" {
			t, _ := parseTimeParam(*in.ExpiresAt)
			a.ExpiresAt = &t
		}
		if a.ExpiresAt != nil && !a.ExpiresAt.After(a.PublishAt) {
			WriteError(w, "INVALID_REQUEST",
				"expires_at must be after publish_at",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		created, err := d.Announcements.CreateAnnouncement(r.Context(), a)
		if err != nil {
			storeErr(w, r)
			return
		}
		WriteJSON(w, http.StatusCreated, created)
	}
}

// UpdateAnnouncement applies a partial update (PATCH admin).
func UpdateAnnouncement(d *AnnounceDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := parsePathID(r.PathValue("id"))
		if !ok {
			WriteError(w, "INVALID_REQUEST", "id must be a positive integer",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		var in announcementIn
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			WriteError(w, "INVALID_REQUEST", "malformed JSON body",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		if msg, ok := in.validate(false); !ok {
			WriteError(w, "INVALID_REQUEST", msg,
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		cur, err := d.Announcements.Announcement(r.Context(), id)
		if err != nil {
			storeErr(w, r)
			return
		}
		if cur == nil {
			WriteError(w, "NOT_FOUND", "announcement not found",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		if in.Title != nil {
			cur.Title = strings.TrimSpace(*in.Title)
		}
		if in.Body != nil {
			cur.Body = *in.Body
		}
		if in.Category != nil {
			cur.Category = *in.Category
		}
		if in.Status != nil {
			cur.Status = *in.Status
		}
		if in.PublishAt != nil {
			t, _ := parseTimeParam(*in.PublishAt)
			cur.PublishAt = t
		}
		if in.ExpiresAt != nil {
			if *in.ExpiresAt == "" {
				cur.ExpiresAt = nil
			} else {
				t, _ := parseTimeParam(*in.ExpiresAt)
				cur.ExpiresAt = &t
			}
		}
		updated, err := d.Announcements.UpdateAnnouncement(r.Context(), *cur)
		if err != nil {
			storeErr(w, r)
			return
		}
		if updated == nil {
			WriteError(w, "NOT_FOUND", "announcement not found",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		WriteJSON(w, http.StatusOK, updated)
	}
}

// RetractAnnouncement marks the record RETRACTED (DELETE admin).
func RetractAnnouncement(d *AnnounceDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := parsePathID(r.PathValue("id"))
		if !ok {
			WriteError(w, "INVALID_REQUEST", "id must be a positive integer",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		okDel, err := d.Announcements.RetractAnnouncement(r.Context(), id, actor(r))
		if err != nil {
			storeErr(w, r)
			return
		}
		if !okDel {
			WriteError(w, "NOT_FOUND", "announcement not found",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{
			"id": id, "status": "RETRACTED",
		})
	}
}

// ---------------------------------------------------------------------------
// Admin: maintenance windows
// ---------------------------------------------------------------------------

// AdminMaintenanceWindows lists every window (any status), newest first.
func AdminMaintenanceWindows(d *AnnounceDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		limit, ok := intParam(r.URL.Query().Get("limit"), 100, 1, 500)
		if !ok {
			WriteError(w, "INVALID_REQUEST",
				"limit must be an integer in [1,500]",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		list, err := d.Maintenance.ListMaintenance(r.Context(), limit)
		if err != nil {
			storeErr(w, r)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{
			"data": list, "count": len(list),
		})
	}
}

// maintenanceIn is the create/update wire shape.
type maintenanceIn struct {
	Title       *string  `json:"title"`
	Description *string  `json:"description"`
	Scope       *string  `json:"scope"`
	Symbols     []string `json:"symbols"`
	Status      *string  `json:"status"`
	StartsAt    *string  `json:"starts_at"` // RFC3339
	EndsAt      *string  `json:"ends_at"`   // RFC3339
}

func (in *maintenanceIn) validate(requireAll bool) (string, bool) {
	if requireAll {
		if in.Title == nil || strings.TrimSpace(*in.Title) == "" {
			return "title is required", false
		}
		if in.StartsAt == nil || in.EndsAt == nil {
			return "starts_at and ends_at are required", false
		}
	}
	if in.Title != nil && len(*in.Title) > 200 {
		return "title exceeds 200 characters", false
	}
	if in.Scope != nil {
		s := strings.ToUpper(*in.Scope)
		if !maintenanceScopes[s] {
			return "scope must be FULL_VENUE|GATEWAY|MARKET_DATA|SETTLEMENT|FUNDING|INSTRUMENT",
				false
		}
		*in.Scope = s
	}
	if in.Status != nil {
		s := strings.ToUpper(*in.Status)
		if !maintenanceStatuses[s] {
			return "status must be SCHEDULED|IN_PROGRESS|COMPLETED|CANCELLED", false
		}
		*in.Status = s
	}
	for _, p := range []*string{in.StartsAt, in.EndsAt} {
		if p != nil {
			if _, ok := parseTimeParam(*p); !ok {
				return "starts_at/ends_at must be RFC3339", false
			}
		}
	}
	return "", true
}

// CreateMaintenance schedules a window (POST admin).
func CreateMaintenance(d *AnnounceDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var in maintenanceIn
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			WriteError(w, "INVALID_REQUEST", "malformed JSON body",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		if msg, ok := in.validate(true); !ok {
			WriteError(w, "INVALID_REQUEST", msg,
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		start, _ := parseTimeParam(*in.StartsAt)
		end, _ := parseTimeParam(*in.EndsAt)
		if !end.After(start) {
			WriteError(w, "INVALID_REQUEST",
				"ends_at must be after starts_at",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		m := marketapi.MaintenanceWindow{
			Title: strings.TrimSpace(*in.Title),
			Scope: "FULL_VENUE", Status: "SCHEDULED",
			StartsAt: start, EndsAt: end, CreatedBy: actor(r),
		}
		if in.Description != nil {
			m.Description = *in.Description
		}
		if in.Scope != nil {
			m.Scope = *in.Scope
		}
		if in.Status != nil {
			m.Status = *in.Status
		}
		m.Symbols = in.Symbols
		created, err := d.Maintenance.CreateMaintenance(r.Context(), m)
		if err != nil {
			storeErr(w, r)
			return
		}
		WriteJSON(w, http.StatusCreated, created)
	}
}

// UpdateMaintenance applies a partial update (PATCH admin).
func UpdateMaintenance(d *AnnounceDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := parsePathID(r.PathValue("id"))
		if !ok {
			WriteError(w, "INVALID_REQUEST", "id must be a positive integer",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		var in maintenanceIn
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			WriteError(w, "INVALID_REQUEST", "malformed JSON body",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		if msg, ok := in.validate(false); !ok {
			WriteError(w, "INVALID_REQUEST", msg,
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		// Read-modify-write: fetch via the public list when an admin-get
		// seam is absent — List is bounded; scan for the id.
		list, err := d.Maintenance.ListMaintenance(r.Context(), 500)
		if err != nil {
			storeErr(w, r)
			return
		}
		var cur *marketapi.MaintenanceWindow
		for i := range list {
			if list[i].ID == id {
				cur = &list[i]
				break
			}
		}
		if cur == nil {
			WriteError(w, "NOT_FOUND", "maintenance window not found",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		if in.Title != nil {
			cur.Title = strings.TrimSpace(*in.Title)
		}
		if in.Description != nil {
			cur.Description = *in.Description
		}
		if in.Scope != nil {
			cur.Scope = *in.Scope
		}
		if in.Symbols != nil {
			cur.Symbols = in.Symbols
		}
		if in.Status != nil {
			cur.Status = *in.Status
		}
		if in.StartsAt != nil {
			t, _ := parseTimeParam(*in.StartsAt)
			cur.StartsAt = t
		}
		if in.EndsAt != nil {
			t, _ := parseTimeParam(*in.EndsAt)
			cur.EndsAt = t
		}
		if !cur.EndsAt.After(cur.StartsAt) {
			WriteError(w, "INVALID_REQUEST",
				"ends_at must be after starts_at",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		updated, err := d.Maintenance.UpdateMaintenance(r.Context(), *cur)
		if err != nil {
			storeErr(w, r)
			return
		}
		if updated == nil {
			WriteError(w, "NOT_FOUND", "maintenance window not found",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		WriteJSON(w, http.StatusOK, updated)
	}
}

// CancelMaintenance flips the window to CANCELLED (DELETE admin).
func CancelMaintenance(d *AnnounceDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := parsePathID(r.PathValue("id"))
		if !ok {
			WriteError(w, "INVALID_REQUEST", "id must be a positive integer",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		okDel, err := d.Maintenance.CancelMaintenance(r.Context(), id, actor(r))
		if err != nil {
			storeErr(w, r)
			return
		}
		if !okDel {
			WriteError(w, "NOT_FOUND", "maintenance window not found",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{
			"id": id, "status": "CANCELLED",
		})
	}
}
