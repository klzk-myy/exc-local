// Task 7.3.1/7.3.11 — RBACMiddleware: role + scope + env enforcement on
// admin routes, driven by the route's own registry metadata (spec §8.2:
// "the enforceable matrix is role × admin route, generated from the Task
// 5.3.7 route registry metadata"; §8.2a scopes; §19.16.4 env axis).
//
// Mounted through the gateway.Router.Wrap hook — every registered route
// whose Auth.Role is non-empty passes through Wrap before the handler
// (and before body validation). Stubs are wrapped too: an unauthorized
// caller gets 401/403, never the 501 shape leak.
//
// Request flow:
//  1. Identity: auth.Claims in context (upstream middleware) or a Bearer
//     JWT verified here; a sid claim is re-validated against the live
//     session store so expiry/revocation (Task 7.3.12 session kill)
//     takes effect immediately.
//  2. Bindings: the principal's ACTIVE VENUE_ADMIN bindings load from
//     admin_role_bindings — exactly one role system per session
//     (§8.2a.2); none → FORBIDDEN.
//  3. Role: some binding must satisfy the route's required role
//     (Permits — required role or Super Admin) else UNAUTHORIZED_ROLE.
//  4. Scope: the env axis is intersected against the request's target
//     environment (X-Admin-Env header, defaulting to this deployment's
//     env); desk/region/currency query params are probed the same way.
//     Out-of-scope → FORBIDDEN. Handler-level entity checks use
//     AssertScope on the attached Identity.
//  5. Read-Only Auditor is hard-limited to read methods — a registry
//     typo mounting a mutation under that role cannot arm it.
package admin

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"exchange/internal/auth"
	"exchange/internal/errs"
	"exchange/internal/gateway"

	excerrors "exchange/pkg/errors"
)

// EnvHeader carries the admin session's target environment (§19.16:
// "the admin session targets one environment at a time").
const EnvHeader = "X-Admin-Env"

// Identity is the resolved admin principal attached to the request
// context after Wrap succeeds.
type Identity struct {
	UserID     int64     // users.id of the admin
	Role       string    // strongest satisfying binding role
	Bindings   []Binding // all role-satisfying active bindings
	Env        string    // request env after intersection
	BreakGlass bool      // a satisfying binding is BREAK_GLASS
	Incident   string    // its incident tag, when set
}

// ScopeAllows checks the union of satisfying bindings against entity
// dims — a handler calls this with the target row's desk/region/
// currency and maps false → FORBIDDEN.
func (id *Identity) ScopeAllows(d Dims) bool {
	for _, b := range id.Bindings {
		if b.Scope.Allows(d) {
			return true
		}
	}
	return false
}

type ctxKeyIdentity struct{}

// WithIdentity attaches the resolved identity (tests + middleware).
func WithIdentity(ctx context.Context, id *Identity) context.Context {
	return context.WithValue(ctx, ctxKeyIdentity{}, id)
}

// IdentityFrom returns the RBAC-verified admin identity, nil when the
// request did not pass Wrap (a wiring bug — treat as fail-closed).
func IdentityFrom(ctx context.Context) *Identity {
	id, _ := ctx.Value(ctxKeyIdentity{}).(*Identity)
	return id
}

// AssertScope enforces the entity-level scope check for handlers reading
// a specific row (desk/region/currency of the target). Returns a coded
// FORBIDDEN error for out-of-scope reads (§8.2a.1).
func AssertScope(ctx context.Context, d Dims) error {
	id := IdentityFrom(ctx)
	if id == nil {
		return excerrors.New("UNAUTHORIZED", "admin identity required")
	}
	if !id.ScopeAllows(d) {
		return excerrors.New("FORBIDDEN", "target outside the binding's data scope")
	}
	return nil
}

// BindingSource is the binding-read seam — *Store in production, an
// in-memory fake in unit tests. Deliberately narrow (reads only): the
// middleware never writes bindings.
type BindingSource interface {
	ActiveBindings(ctx context.Context, userID int64) ([]Binding, error)
}

// Middleware is the RBAC+scope enforcement engine.
type Middleware struct {
	Store    BindingSource        // binding reads (required)
	Issuer   *auth.Issuer         // optional: Bearer fallback when no claims
	Sessions *auth.SessionManager // optional: sid re-validation
	Env      string               // this deployment's env (normalized)
}

// NewMiddleware wires the middleware. env is the deployment label
// (config.Environment) — normalized into the §19.16 dev/staging/
// production vocabulary.
func NewMiddleware(store BindingSource, issuer *auth.Issuer, sessions *auth.SessionManager, env string) *Middleware {
	return &Middleware{Store: store, Issuer: issuer, Sessions: sessions,
		Env: NormalizeEnv(env)}
}

// Wrap enforces the route's declared role + scope. Routes with no
// Auth.Role pass through untouched (public/client surfaces are not this
// middleware's concern).
func (m *Middleware) Wrap(rt gateway.Route, next http.Handler) http.Handler {
	if rt.Auth.Role == "" {
		return next
	}
	required := rt.Auth.Role
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, err := m.authenticate(r)
		if err != nil {
			writeProblem(w, err)
			return
		}
		uid, err := strconv.ParseInt(strings.TrimSpace(id.Subject), 10, 64)
		if err != nil || uid <= 0 {
			writeProblem(w, excerrors.New("UNAUTHORIZED",
				"admin identity unresolvable"))
			return
		}

		// Env axis (§8.2a.3 / §19.16.4): X-Admin-Env overrides the
		// deployment default; a binding valid in dev grants nothing in
		// staging/production. Route.Env="nonprod" rows are additionally
		// barred from production outright.
		reqEnv := NormalizeEnv(r.Header.Get(EnvHeader))
		if reqEnv == "" {
			reqEnv = m.Env
		}
		if rt.Env == "nonprod" && reqEnv == "production" {
			writeProblem(w, excerrors.New("FORBIDDEN",
				"route is non-production only"))
			return
		}

		bindings, err := m.Store.ActiveBindings(r.Context(), uid)
		if err != nil {
			writeProblem(w, excerrors.Wrap("INTERNAL_ERROR",
				"binding lookup", err))
			return
		}
		if len(bindings) == 0 {
			writeProblem(w, excerrors.New("FORBIDDEN",
				"no active admin role binding"))
			return
		}

		// Role check against the registry metadata.
		var satisfying []Binding
		for _, b := range bindings {
			if Permits(required, b.Role) {
				satisfying = append(satisfying, b)
			}
		}
		if len(satisfying) == 0 {
			writeProblem(w, excerrors.New("UNAUTHORIZED_ROLE",
				"route requires "+required))
			return
		}

		// Auditor bindings never satisfy a mutation — matrix typo armor.
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			allAuditor := true
			for _, b := range satisfying {
				if b.Role != RoleReadOnlyAuditor {
					allAuditor = false
				}
			}
			if allAuditor {
				writeProblem(w, excerrors.New("FORBIDDEN",
					"Read-Only Auditor bindings cannot mutate"))
				return
			}
		}

		envOK := satisfying[:0]
		for _, b := range satisfying {
			// "env-scoped" routes demand an explicit env axis — a global
			// binding does not carry deploy authority (route.go Env doc).
			if rt.Env == "env-scoped" && (b.Scope == nil || len(b.Scope.Env) == 0) {
				continue
			}
			if b.Scope.AllowsEnv(reqEnv) {
				envOK = append(envOK, b)
			}
		}
		satisfying = envOK
		if len(satisfying) == 0 {
			writeProblem(w, excerrors.New("FORBIDDEN",
				"no binding valid in environment "+reqEnv))
			return
		}

		// Cheap dims probe: query params desk/region/currency are scope
		// dimensions when present (entity-level checks stay with the
		// handler via AssertScope).
		d := Dims{
			Desk:     r.URL.Query().Get("desk"),
			Region:   r.URL.Query().Get("region"),
			Currency: r.URL.Query().Get("currency"),
		}
		dimsOK := satisfying[:0]
		for _, b := range satisfying {
			if b.Scope.Allows(d) {
				dimsOK = append(dimsOK, b)
			}
		}
		satisfying = dimsOK
		if len(satisfying) == 0 {
			writeProblem(w, excerrors.New("FORBIDDEN",
				"request dimensions outside the binding's data scope"))
			return
		}

		idn := &Identity{UserID: uid, Bindings: satisfying, Env: reqEnv}
		best := -1
		for _, b := range satisfying {
			if r := roleRank(b.Role); r > best {
				best, idn.Role = r, b.Role
			}
			if b.Kind == KindBreakGlass {
				idn.BreakGlass = true
				if b.Scope != nil && b.Scope.Incident != "" {
					idn.Incident = b.Scope.Incident
				}
			}
		}
		next.ServeHTTP(w, r.WithContext(WithIdentity(r.Context(), idn)))
	})
}

// WrapRoute is the gateway.Router.Wrap-compatible form.
func (m *Middleware) WrapRoute(rt gateway.Route, h http.Handler) http.Handler {
	return m.Wrap(rt, h)
}

// authenticate resolves claims: upstream-middleware claims win, else a
// Bearer token is verified through the issuer. With sessions wired, a
// sid claim is re-validated (binding expiry → session kill lands on the
// very next request).
func (m *Middleware) authenticate(r *http.Request) (*auth.Claims, error) {
	if c := auth.ClaimsFrom(r.Context()); c != nil {
		return c, nil
	}
	if m.Issuer == nil {
		return nil, excerrors.New("UNAUTHORIZED", "authentication required")
	}
	hdr := r.Header.Get("Authorization")
	if hdr == "" || !strings.HasPrefix(hdr, "Bearer ") {
		return nil, excerrors.New("UNAUTHORIZED", "missing bearer token")
	}
	claims, err := m.Issuer.Parse(strings.TrimSpace(hdr[len("Bearer "):]))
	if err != nil {
		// The auth-cluster error codes are not §23-registered; map every
		// parse failure to the canonical UNAUTHORIZED (fail closed).
		return nil, excerrors.New("UNAUTHORIZED", "invalid bearer token")
	}
	if m.Sessions != nil && claims.SessionID != "" {
		sess, err := m.Sessions.Validate(r.Context(), claims.SessionID)
		if err != nil {
			return nil, excerrors.New("UNAUTHORIZED", "session not valid")
		}
		if sess.UserID != "" && sess.UserID != claims.Subject {
			return nil, excerrors.New("UNAUTHORIZED",
				"session/user binding mismatch")
		}
		claims.AccountID = sess.AccountID
		if len(sess.AMR) > 0 {
			claims.AMR = sess.AMR
		}
	}
	return &claims, nil
}

// writeProblem emits the RFC 7807 envelope with the §23-registered
// status for the code (errs.Default is the emission gate).
func writeProblem(w http.ResponseWriter, err error) {
	var e *excerrors.Error
	status, code, msg := http.StatusInternalServerError, "INTERNAL_ERROR", "internal error"
	if errors.As(err, &e) {
		code, msg = e.Code, e.Message
		if s := errs.Default.HTTPStatus(code); s >= 400 {
			status = s
		}
	}
	excerrors.NewProblem(status, code, code, msg).WriteTo(w)
}
