// Meta endpoints exposing the registries: GET /api/v1/routes (Task 5.3.7
// step 3), GET /api/v1/errors (Task 5.3.21 step 3) and
// GET /api/v1/openapi.json (registry-generated OpenAPI 3.1 skeleton —
// Task 5.3.8 replaces it with the full document).
//
// /api/v1/routes and /api/v1/errors are admin-only per the task text: they
// are registered with Auth{Required:true, Role:"Super Admin"}; enforcement
// lands with the auth middleware (Task 5.3.1 / Phase-07 Task 7.3.1) — the
// metadata is already the normative declaration CI consumes.
package gateway

import (
	"net/http"
	"strings"

	"exchange/internal/errs"
)

// routeInfo is the JSON projection of a Route for the table dump.
type routeInfo struct {
	Method      string   `json:"method"`
	Path        string   `json:"path"`
	Version     string   `json:"version"`
	Status      Status   `json:"status"`
	Owner       string   `json:"owner"`
	Auth        AuthSpec `json:"auth"`
	RateTier    string   `json:"rate_tier"`
	Weight      int      `json:"weight"`
	DualControl bool     `json:"dual_control"`
	Env         string   `json:"env,omitempty"`
	Description string   `json:"description"`
}

// RoutesHandler serves GET /api/v1/routes — the full registered table.
func (r *Router) RoutesHandler(w http.ResponseWriter, _ *http.Request) {
	routes := r.Routes()
	infos := make([]routeInfo, 0, len(routes))
	for _, rt := range routes {
		w := rt.Weight
		if w == 0 {
			w = 1 // §8.8 default request weight
		}
		infos = append(infos, routeInfo{
			Method: rt.Method, Path: rt.Path, Version: rt.Version,
			Status: rt.Status, Owner: rt.Owner, Auth: rt.Auth,
			RateTier: rt.RateTier, Weight: w,
			DualControl: rt.DualControl, Env: rt.Env, Description: rt.Description,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"routes": infos, "count": len(infos)})
}

// ErrorsHandler serves GET /api/v1/errors — the full code registry.
func (r *Router) ErrorsHandler(w http.ResponseWriter, _ *http.Request) {
	defs := r.reg.All()
	writeJSON(w, http.StatusOK, map[string]any{
		"error_codes": defs,
		"count":       len(defs),
		"spec_count":  len(errs.SpecTable()),
	})
}

// OpenAPIHandler serves a minimal OpenAPI 3.1 document generated from the
// registry: every route becomes a path+operation carrying x-owner,
// x-rate-tier, x-auth and a canonical error response. Task 5.3.8 extends
// this into the full documented spec with schemas.
func (r *Router) OpenAPIHandler(w http.ResponseWriter, req *http.Request) {
	writeJSON(w, http.StatusOK, r.OpenAPIDocument())
}

// OpenAPIDocument builds the OpenAPI 3.1 skeleton from registered routes.
func (r *Router) OpenAPIDocument() map[string]any {
	paths := map[string]any{}
	for _, rt := range r.Routes() {
		// OpenAPI path templating: {id} already matches; query variants are
		// not separate paths.
		item, _ := paths[rt.Path].(map[string]any)
		if item == nil {
			item = map[string]any{}
			paths[rt.Path] = item
		}
		method := strings.ToLower(rt.Method)
		if rt.Method == "WS" {
			method = "get" // upgrade handshake is GET
		}
		op := map[string]any{
			"summary":     rt.Description,
			"x-owner":     rt.Owner,
			"x-status":    string(rt.Status),
			"x-rate-tier": rt.RateTier,
			"x-weight":    max(rt.Weight, 1),
			"x-auth": map[string]any{
				"required": rt.Auth.Required,
				"role":     rt.Auth.Role,
				"scopes":   rt.Auth.Scopes,
				"methods":  rt.Auth.Methods,
			},
			"responses": map[string]any{
				"200": map[string]any{"description": "success"},
				"default": map[string]any{
					"description": "RFC 7807 error envelope (spec §8.7); code per spec §23",
				},
			},
		}
		if rt.DualControl {
			op["x-dual-control"] = true
		}
		item[method] = op
	}
	return map[string]any{
		"openapi": "3.1.0",
		"info": map[string]any{
			"title":       "Exchange Order Gateway API",
			"version":     "v1",
			"description": "Registry-generated skeleton (Task 5.3.7); full schema authored by Task 5.3.8.",
		},
		"paths": paths,
	}
}
