// Router — the Task 5.3.7 central route registry mounted on net/http.
//
// A Router wraps a *http.ServeMux (Go 1.22+ method patterns). Every call to
// Register validates the Route metadata, mounts the handler (or the 501
// stub for Status=Stub), and records the entry for the route-table dump,
// the OpenAPI generator and the Phase-8 completeness cross-check.
//
// Registration rules:
//   - duplicate (method, path) → error (ServeMux would panic otherwise);
//   - Status=Stub mounts the 501 NOT_IMPLEMENTED envelope regardless of
//     the supplied handler;
//   - routes declaring a BodySchema get centralized structural validation
//     (spec §8.4 item 3): INVALID_REQUEST 400 on malformed bodies, before
//     the handler runs.
package gateway

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"sync"

	"exchange/internal/errs"
)

// Router is a route registry + ServeMux. Not safe to Register concurrently
// with serving on the mux (registration is a startup-phase activity);
// Routes()/table dumps are safe at any time.
type Router struct {
	mux  *http.ServeMux
	reg  *errs.Registry
	wrap func(Route, http.Handler) http.Handler // cross-route hook (Task 7.3.1 RBAC)

	mu     sync.RWMutex
	routes map[string]Route // key() → route
	order  []string         // registration order for stable dumps
}

// NewRouter binds a registry to mux. reg==nil uses errs.Default.
func NewRouter(mux *http.ServeMux, reg *errs.Registry) *Router {
	if mux == nil {
		mux = http.NewServeMux()
	}
	if reg == nil {
		reg = errs.Default
	}
	return &Router{mux: mux, reg: reg, routes: make(map[string]Route)}
}

// Mux exposes the underlying ServeMux for direct mounts that bypass the
// registry (e.g. /health). Bypassed handlers are invisible to the route
// dump and the completeness check — use only for non-API plumbing.
func (r *Router) Mux() *http.ServeMux { return r.mux }

// Registry exposes the error registry the router emits through.
func (r *Router) Registry() *errs.Registry { return r.reg }

// SetWrapper installs a cross-route wrapper consulted at Register time —
// the Phase-07 RBAC middleware hook (Task 7.3.1, spec §8.2/§8.2a). The
// wrapper receives the route's own metadata and the final mounted
// handler (post stub/body-schema wrapping) and returns the outermost
// handler: authorization runs BEFORE body parsing, and stubs are wrapped
// too so an unauthorized caller gets 401/403 rather than the 501 shape.
// Startup-phase setter like Register; pass nil to clear.
func (r *Router) SetWrapper(fn func(Route, http.Handler) http.Handler) {
	r.wrap = fn
}

// Register validates rt, mounts handler on the mux and records the route.
// A nil handler mounts the 501 stub. Registering a Stub route with a real
// handler is an error — a stub must say stub until its owner lands.
func (r *Router) Register(rt Route, handler http.Handler) error {
	if err := rt.validate(); err != nil {
		return err
	}
	if rt.Status == StatusStub && handler != nil {
		return fmt.Errorf("route %s: stub routes must not mount a live handler", rt.key())
	}
	key := rt.key()

	r.mu.Lock()
	if _, dup := r.routes[key]; dup {
		r.mu.Unlock()
		return fmt.Errorf("route %s: duplicate registration", key)
	}
	if rt.Status == StatusLive && handler == nil {
		r.mu.Unlock()
		return fmt.Errorf("route %s: live route requires a handler", key)
	}
	r.routes[key] = rt
	r.order = append(r.order, key)
	r.mu.Unlock()

	h := handler
	if rt.Status == StatusStub {
		h = r.stubHandler(rt)
	}
	if rt.Schema != nil {
		h = r.validateBody(rt, h)
	}
	if r.wrap != nil {
		h = r.wrap(rt, h)
	}
	r.mux.Handle(rt.mountPattern(), h)
	return nil
}

// RegisterFunc is Register for HandlerFunc-shaped handlers.
func (r *Router) RegisterFunc(rt Route, hf func(http.ResponseWriter, *http.Request)) error {
	if hf == nil {
		return r.Register(rt, nil)
	}
	return r.Register(rt, http.HandlerFunc(hf))
}

// RegisterErrFunc registers a live route whose handler returns an error —
// the error-mapping seam of Task 5.3.41 (internal error classes → §23
// codes + HTTP status; the RFC 7807 envelope is written by WriteError).
func (r *Router) RegisterErrFunc(rt Route, hf func(http.ResponseWriter, *http.Request) error) error {
	if hf == nil {
		return r.Register(rt, nil)
	}
	return r.Register(rt, r.ErrHandler(hf))
}

// Routes returns every registered route, sorted by (path, method) — the
// payload for GET /api/v1/routes and the Phase-8 completeness check.
func (r *Router) Routes() []Route {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Route, 0, len(r.routes))
	for _, rt := range r.routes {
		out = append(out, rt)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Path != out[j].Path {
			return out[i].Path < out[j].Path
		}
		return out[i].Method < out[j].Method
	})
	return out
}

// RouteFor looks up one registered route by method+path.
func (r *Router) RouteFor(method, path string) (Route, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	rt, ok := r.routes[method+" "+path]
	return rt, ok
}

// stubHandler is the 501 placeholder for routes registered ahead of their
// owning phase (registration-completeness invariant).
func (r *Router) stubHandler(rt Route) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.WriteError(w, req, errs.CodeNotImplemented,
			"endpoint registered but not yet implemented",
			map[string]any{"owner": rt.Owner, "route": rt.Method + " " + rt.Path})
	})
}

// validateBody wraps h with centralized BodySchema enforcement
// (Task 5.3.7 step 5). Only body-carrying methods are validated; a schema
// on a bodyless method is a no-op but keeps the metadata honest.
func (r *Router) validateBody(rt Route, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch req.Method {
		case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		default:
			h.ServeHTTP(w, req)
			return
		}
		body, err := io.ReadAll(io.LimitReader(req.Body, maxBodyBytes+1))
		if err != nil || len(body) > maxBodyBytes {
			r.WriteError(w, req, "INVALID_REQUEST", "request body unreadable or exceeds 1MB", nil)
			return
		}
		if verr := rt.Schema.ValidateRequest(body); verr != nil {
			r.WriteError(w, req, "INVALID_REQUEST", verr.Error(), nil)
			return
		}
		// Restore the drained body so the handler can re-decode it.
		req.Body = io.NopCloser(bytes.NewReader(body))
		h.ServeHTTP(w, req)
	})
}

// writeJSON is the shared JSON response writer for meta endpoints.
func writeJSON(w http.ResponseWriter, status int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"type":"error","error":"INTERNAL_ERROR","message":"marshal failure","status":500}`))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}
