// Task 9.3.7 — feature flag admin surface.
//
//	GET    /api/v1/admin/flags                    list (auditor+)
//	POST   /api/v1/admin/flags                    create/replace {name,...}
//	GET    /api/v1/admin/flags/{name}             read one
//	PUT    /api/v1/admin/flags/{name}             update (full body)
//	POST   /api/v1/admin/flags/{name}             toggle {enabled} (task item 4)
//	DELETE /api/v1/admin/flags/{name}             remove
//	POST   /api/v1/admin/flags/{name}/advance     step the canary ladder
//
// Auth: admin scope via requireAdmin; the route registry's RBAC wrap
// (adminAuth role) is the §8.2 gate; every mutation lands in
// admin_audit_log inside the store transaction.
package api

import (
	"encoding/json"
	"errors"
	"net/http"

	"exchange/internal/flags"
	"exchange/internal/gateway"
	"exchange/internal/middleware"
	excerrors "exchange/pkg/errors"
)

// excCode unwraps a pkg/errors coded error ("" when uncoded).
func excCode(err error) string {
	var e *excerrors.Error
	if errors.As(err, &e) {
		return e.Code
	}
	return ""
}

// flagBody is the create/update payload shape.
type flagBody struct {
	Name        string   `json:"name"`
	Enabled     *bool    `json:"enabled"`
	RolloutPct  *int     `json:"rollout_pct"`
	Stages      []int    `json:"stages"`
	StageIdx    *int     `json:"stage_idx"`
	Tiers       []string `json:"tiers"`
	Accounts    []int64  `json:"accounts"`
	Description *string  `json:"description"`
}

// FlagHandlers returns the admin handler set.
func FlagHandlers(st *flags.Store) (list, create, get, update, toggle, del, advance http.HandlerFunc) {
	audit := func(w http.ResponseWriter, r *http.Request) (int64, string, bool) {
		if requireAdmin(w, r) == nil {
			return 0, "", false
		}
		id, err := adminActorID(r)
		if err != nil {
			WriteError(w, "UNAUTHORIZED", "admin identity unresolvable",
				gateway.RequestIDFrom(r.Context()), nil)
			return 0, "", false
		}
		return id, middleware.ClientIP(r, true), true
	}
	writeErr := func(w http.ResponseWriter, r *http.Request, err error) {
		switch {
		case errors.Is(err, flags.ErrNotFound):
			WriteError(w, "NOT_FOUND", "feature flag not found",
				gateway.RequestIDFrom(r.Context()), nil)
		case excCode(err) == "INVALID_REQUEST":
			WriteError(w, "INVALID_REQUEST", err.Error(),
				gateway.RequestIDFrom(r.Context()), nil)
		default:
			WriteError(w, "SERVICE_DEGRADED", "flag store unavailable",
				gateway.RequestIDFrom(r.Context()), nil)
		}
	}

	list = func(w http.ResponseWriter, r *http.Request) {
		if requireAdmin(w, r) == nil {
			return
		}
		out, err := st.List(r.Context())
		if err != nil {
			writeErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"flags": out})
	}

	create = func(w http.ResponseWriter, r *http.Request) {
		adminID, ip, ok := audit(w, r)
		if !ok {
			return
		}
		var body flagBody
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			WriteError(w, "INVALID_REQUEST", "malformed body",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		f := flags.Flag{Name: body.Name, StageIdx: -1}
		applyFlagBody(&f, &body)
		out, err := st.Upsert(r.Context(), f, adminID, ip)
		if err != nil {
			writeErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, out)
	}

	get = func(w http.ResponseWriter, r *http.Request) {
		if requireAdmin(w, r) == nil {
			return
		}
		f, err := st.Get(r.Context(), r.PathValue("name"))
		if err != nil {
			writeErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, f)
	}

	update = func(w http.ResponseWriter, r *http.Request) {
		adminID, ip, ok := audit(w, r)
		if !ok {
			return
		}
		var body flagBody
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			WriteError(w, "INVALID_REQUEST", "malformed body",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		// PUT is a full replace shaped on the stored row.
		cur, err := st.Get(r.Context(), r.PathValue("name"))
		if err != nil {
			writeErr(w, r, err)
			return
		}
		f := *cur
		applyFlagBody(&f, &body)
		out, err := st.Upsert(r.Context(), f, adminID, ip)
		if err != nil {
			writeErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, out)
	}

	// toggle is the task's POST {name} — enable/disable only.
	toggle = func(w http.ResponseWriter, r *http.Request) {
		adminID, ip, ok := audit(w, r)
		if !ok {
			return
		}
		cur, err := st.Get(r.Context(), r.PathValue("name"))
		if err != nil {
			writeErr(w, r, err)
			return
		}
		var body struct {
			Enabled *bool `json:"enabled"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Enabled == nil {
			WriteError(w, "INVALID_REQUEST", "body requires {enabled: bool}",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		f := *cur
		f.Enabled = *body.Enabled
		out, err := st.Upsert(r.Context(), f, adminID, ip)
		if err != nil {
			writeErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, out)
	}

	del = func(w http.ResponseWriter, r *http.Request) {
		adminID, ip, ok := audit(w, r)
		if !ok {
			return
		}
		if err := st.Delete(r.Context(), r.PathValue("name"), adminID, ip); err != nil {
			writeErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"deleted": r.PathValue("name")})
	}

	advance = func(w http.ResponseWriter, r *http.Request) {
		adminID, ip, ok := audit(w, r)
		if !ok {
			return
		}
		out, done, err := st.Advance(r.Context(), r.PathValue("name"), adminID, ip)
		if err != nil {
			writeErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"flag": out, "ladder_done": done})
	}
	return list, create, get, update, toggle, del, advance
}

// applyFlagBody merges the payload over f — nil fields keep the stored
// value (PUT body semantics: omitted = unchanged for a replace-shaped
// update on the stored row).
func applyFlagBody(f *flags.Flag, body *flagBody) {
	if body.Name != "" {
		f.Name = body.Name
	}
	if body.Enabled != nil {
		f.Enabled = *body.Enabled
	}
	if body.RolloutPct != nil {
		f.RolloutPct = *body.RolloutPct
	}
	if body.Stages != nil {
		f.Stages = body.Stages
	}
	if body.StageIdx != nil {
		f.StageIdx = *body.StageIdx
	}
	if body.Tiers != nil {
		f.Tiers = body.Tiers
	}
	if body.Accounts != nil {
		f.Accounts = body.Accounts
	}
	if body.Description != nil {
		f.Description = *body.Description
	}
}
